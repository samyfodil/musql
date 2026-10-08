package engine

import (
	"slices"

	"github.com/samyfodil/musql/internal/jit"
)

// SERVICE BLOCKS: what a compiled loop body does with an instruction it has no
// native form for.
//
// The program JIT's IR is int64-only, and the lowering used to refuse a whole
// loop over one instruction outside it -- a length(), a LIKE, a comparison
// under a collation -- so one such call kept the entire loop in the VDBE. Now a
// run of such instructions becomes a SERVICE BLOCK: the kernel stops at it
// (jit.POpService), Go runs the block's instructions on that row against a
// window of Values with the VDBE's own opcode code, and the kernel resumes on
// the same row. Nothing is reimplemented: a block's Function is opFunction, its
// LIKE opLike, its comparison compareOp, its column read segColumnValue -- the
// same functions the run loop calls.
//
// REGISTERS have one of two homes. A register only blocks touch lives in the
// window as a Value, NULL and TEXT included. A register native code reads or
// writes lives in the kernel's int64 file: a block loads it from there as an
// integer, and writes it back after, where a value that is not an INTEGER --
// a NULL, a REAL, a TEXT -- cannot be represented, and the block declines the
// statement exactly as an overflow does: the VDBE answers it instead. The
// window starts as a copy of the interpreter's registers, so a constant the
// program loaded before the loop is already in it.

// segSvcBlock is one service block: body instructions it runs, in order.
type segSvcBlock struct {
	insns     []Instruction
	load      []int // kernel registers read into the window first
	writeBack []int // window registers written back to the kernel after
	// flag is the kernel register the block's closing jump decision lands
	// in (1 taken, 0 not), -1 when it does not end in a jump.
	flag int
}

// segInsnRegs is the registers in reads and writes, and whether it may end in
// a jump. ok is false for an opcode a service block cannot run.
func segInsnRegs(in Instruction) (reads, writes []int, jump, ok bool) {
	switch in.Op {
	case OpColumn:
		return nil, []int{in.P3}, false, true
	case OpInteger, OpInt64, OpReal, OpString8, OpNull, OpVariable:
		return nil, []int{in.P2}, false, true
	case OpSCopy, OpCopy, OpNot:
		return []int{in.P1}, []int{in.P2}, false, true
	case OpFunction:
		r := make([]int, in.P2)
		for i := range r {
			r[i] = in.P1 + i
		}
		return r, []int{in.P3}, false, true
	case OpLike:
		r := []int{in.P1, in.P3}
		if esc, isInt := in.P4.(int); isInt && esc >= 0 {
			r = append(r, esc)
		}
		return r, []int{in.P2}, false, true
	case OpGlob:
		return []int{in.P1, in.P3}, []int{in.P2}, false, true
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		if in.P5&p5StoreP2 != 0 {
			return []int{in.P1, in.P3}, []int{in.P2}, false, true
		}
		return []int{in.P1, in.P3}, nil, true, true
	case OpIf, OpIfNot, OpIsNull, OpNotNull:
		return []int{in.P1}, nil, true, true
	case OpConcat, OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder:
		return []int{in.P1, in.P2}, []int{in.P3}, false, true
	}
	return nil, nil, false, false
}

// segNativeLowerable mirrors segLowerBody's switch: whether in has a native
// form at all. A Column or a copy may still go to a block when only blocks
// read what it writes (segClassifyServices).
func segNativeLowerable(in Instruction, cursor int) bool {
	switch in.Op {
	case OpColumn:
		return in.P1 == cursor
	case OpInteger, OpVariable, OpIfNot, OpIf, OpGoto, OpIsNull, OpNotNull, OpNot,
		OpAdd, OpSubtract, OpMultiply, OpRemainder, OpNegative, OpBitAnd, OpBitOr, OpNull, OpSCopy:
		return true
	case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
		_, ok := segCompareLowerable(in.P5)
		return ok
	case OpFunction:
		return segLowerableFunc(in)
	}
	return false
}

// segServiceable reports whether a block may run in. A function must be a
// deterministic built-in named by its string: a kernel that declines part way
// hands the statement to the VDBE, which runs every call again, and a call
// with an effect or a varying answer must not be made twice.
func segServiceable(in Instruction, cursor int) bool {
	if _, _, _, ok := segInsnRegs(in); !ok {
		return false
	}
	switch in.Op {
	case OpColumn:
		return in.P1 == cursor
	case OpFunction:
		name, isName := in.P4.(string)
		return isName && name != "rtreecheck" && !nonConstantFuncs[r33sFoldIdent(name)]
	}
	return true
}

// segClassifyServices decides which body instructions run in service blocks:
// every one with no native form, plus any Column, copy, NULL or test whose
// value only blocks would use or that reads what a block wrote -- so a text
// column feeding length() is read by the block that calls it, and the IfNot
// after a LIKE sees its NULL. nil when an instruction can be neither.
func segClassifyServices(body []Instruction, cursor int) []bool {
	svc := make([]bool, len(body))
	some := false
	for i, in := range body {
		if !segNativeLowerable(in, cursor) {
			if !segServiceable(in, cursor) {
				return nil
			}
			svc[i] = true
			some = true
		}
	}
	if !some {
		return svc
	}
	for changed := true; changed; {
		changed = false
		readers, writers := map[int][]int{}, map[int][]int{}
		for i, in := range body {
			r, w, _, _ := segInsnRegs(in)
			if _, _, _, ok := segInsnRegs(in); !ok {
				r = nil
				for _, x := range segLowerRegOperands(in) {
					if x >= 0 {
						r = append(r, x)
					}
				}
			}
			for _, x := range r {
				readers[x] = append(readers[x], i)
			}
			for _, x := range w {
				writers[x] = append(writers[x], i)
			}
		}
		allSvc := func(is []int) bool {
			for _, i := range is {
				if !svc[i] {
					return false
				}
			}
			return len(is) > 0
		}
		anySvc := func(is []int) bool {
			for _, i := range is {
				if svc[i] {
					return true
				}
			}
			return false
		}
		for i, in := range body {
			if svc[i] || !segServiceable(in, cursor) {
				continue
			}
			pull := false
			switch in.Op {
			case OpColumn:
				pull = allSvc(readers[in.P3])
			case OpNull, OpInteger:
				pull = allSvc(readers[in.P2])
			case OpSCopy, OpNot:
				pull = allSvc(readers[in.P2]) || anySvc(writers[in.P1])
			case OpIf, OpIfNot, OpIsNull, OpNotNull:
				pull = anySvc(writers[in.P1])
			}
			if pull {
				svc[i] = true
				changed = true
			}
		}
	}
	return svc
}

// segBuildBlocks groups consecutive service instructions into blocks. A jump
// target starts a new block (a branch must land on the kernel's
// POpService), and a jump ends one. startOf[i] is the block starting at body
// index i, -1 elsewhere.
func segBuildBlocks(body []Instruction, svc []bool, base int, flagFrom int) (blocks []segSvcBlock, startOf []int) {
	target := make([]bool, len(body)+1)
	for _, in := range body {
		switch in.Op {
		case OpGoto, OpIf, OpIfNot, OpIsNull, OpNotNull, OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			if in.Op >= OpEq && in.Op <= OpGe && in.P5&p5StoreP2 != 0 {
				continue
			}
			if t := in.P2 - base; t >= 0 && t < len(body) {
				target[t] = true
			}
		}
	}
	startOf = make([]int, len(body))
	for i := range startOf {
		startOf[i] = -1
	}
	for i := 0; i < len(body); {
		if !svc[i] {
			i++
			continue
		}
		b := segSvcBlock{flag: -1}
		start := i
		computed := false // the block already made a call or a test
		for i < len(body) && svc[i] && (i == start || !target[i]) && !(computed && body[i].Op == OpColumn) {
			switch body[i].Op {
			case OpFunction, OpLike, OpGlob, OpConcat, OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
				computed = true
			}
			b.insns = append(b.insns, body[i])
			_, _, jump, _ := segInsnRegs(body[i])
			i++
			if jump {
				b.flag = flagFrom + len(blocks)
				break
			}
		}
		startOf[start] = len(blocks)
		blocks = append(blocks, b)
	}
	return blocks, startOf
}

// segBlockHomes fills each block's load and writeBack lists: what it reads
// that native code may have written, and what it writes that native code
// reads or also writes.
func segBlockHomes(body []Instruction, svc []bool, blocks []segSvcBlock) {
	nativeRead, nativeWrite := map[int]bool{}, map[int]bool{}
	for i, in := range body {
		if svc[i] {
			continue
		}
		for _, x := range segLowerRegOperands(in) {
			if x >= 0 {
				nativeRead[x] = true // a destination counts too: it is written
			}
		}
		if _, w, _, ok := segInsnRegs(in); ok {
			for _, x := range w {
				nativeWrite[x] = true
			}
		}
		if in.Op == OpColumn {
			nativeWrite[in.P3] = true
		}
	}
	for bi := range blocks {
		b := &blocks[bi]
		seenL, seenW := map[int]bool{}, map[int]bool{}
		for _, in := range b.insns {
			r, w, _, _ := segInsnRegs(in)
			for _, x := range r {
				if nativeWrite[x] && !seenL[x] {
					seenL[x] = true
					b.load = append(b.load, x)
				}
			}
			for _, x := range w {
				if (nativeRead[x] || nativeWrite[x]) && !seenW[x] {
					seenW[x] = true
					b.writeBack = append(b.writeBack, x)
				}
			}
		}
	}
}

// segSvcRunner runs a kernel's service blocks over one statement: one window
// for the whole run, as the interpreter has one register file.
type segSvcRunner struct {
	vm    vdbe // a shallow copy of the running machine, regs = the window
	plans []segColPlan
}

func newSegSvcRunner(m *vdbe, tbl *resolvedTable) *segSvcRunner {
	r := &segSvcRunner{vm: *m, plans: segColPlansFor(tbl)}
	r.vm.regs = append([]Value(nil), m.regs...)
	r.vm.fnArgBuf = nil
	r.vm.likePlan = nil
	return r
}

// run performs block b on row `row` of s, reading and writing the kernel's
// registers. false declines the statement: a value the kernel cannot hold, or
// an error, which the VDBE will raise when it runs the statement itself.
func (r *segSvcRunner) run(b *segSvcBlock, s *segment, row int, regs []int64) bool {
	w := r.vm.regs
	for _, x := range b.load {
		w[x] = Value{Typ: Int, I: regs[x]}
	}
	m := &r.vm
	for k := range b.insns {
		op := &b.insns[k]
		taken := false
		switch op.Op {
		case OpColumn:
			width := min(s.Width(row), len(r.plans))
			w[op.P3] = segColumnValue(r.plans, s, row, width, s.Rowid(row), op.P2)
		case OpInteger:
			w[op.P2] = Value{Typ: Int, I: int64(op.P1)}
		case OpInt64:
			w[op.P2] = Value{Typ: Int, I: op.P4.(int64)}
		case OpReal:
			w[op.P2] = Value{Typ: Float, F: op.P4.(float64)}
		case OpString8:
			w[op.P2] = Value{Typ: Text, S: []byte(op.P4.(string))}
		case OpNull:
			w[op.P2] = Value{Typ: Null}
		case OpVariable:
			w[op.P2] = paramAt(m.params, op.P1)
		case OpCopy:
			w[op.P2] = copyValue(w[op.P1])
		case OpSCopy:
			w[op.P2] = w[op.P1]
		case OpNot:
			if v := w[op.P1]; v.Typ == Null {
				w[op.P2] = Value{Typ: Null}
			} else {
				w[op.P2] = boolValue(!isTruthy(v))
			}
		case OpFunction:
			if m.opFunction(op) != nil {
				return false
			}
		case OpLike:
			if m.opLike(op) != nil {
				return false
			}
		case OpGlob:
			m.opGlob(op)
		case OpConcat:
			w[op.P3] = concatValuesEnc(w[op.P2], w[op.P1], m.encoding())
		case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder:
			if v, ok := intArith(op.Op, w[op.P2], w[op.P1]); ok {
				w[op.P3] = v
				break
			}
			v, err := evalArith(arithOpName(op.Op), w[op.P2], w[op.P1])
			if err != nil {
				return false
			}
			w[op.P3] = v
		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			taken, _ = m.compareOp(op)
		case OpIf:
			taken = m.jumpIf(op.P1, op.P3, true)
		case OpIfNot:
			taken = m.jumpIf(op.P1, op.P3, false)
		case OpIsNull:
			taken = w[op.P1].Typ == Null
		case OpNotNull:
			taken = w[op.P1].Typ != Null
		default:
			return false
		}
		if b.flag >= 0 && k == len(b.insns)-1 {
			regs[b.flag] = 0
			if taken {
				regs[b.flag] = 1
			}
		}
	}
	for _, x := range b.writeBack {
		v := w[x]
		if v.Typ != Int {
			return false
		}
		regs[x] = v.I
	}
	return true
}

// segCallKernel runs kern to completion, performing each service it stops
// for. false declines the statement.
func segCallKernel(kern *jit.Code, args *jit.ProgArgs, svc *segSvcRunner, blocks []segSvcBlock, s *segment, regs []int64) bool {
	for {
		kern.Call2(args)
		if args.PC == 0 {
			return true
		}
		k := int(args.PC - 1)
		if svc == nil || k < 0 || k >= len(blocks) {
			return false
		}
		if !svc.run(&blocks[k], s, int(args.N+args.Row), regs) {
			return false
		}
	}
}

// readNatively records that native code outside the lowered body -- the
// accumulate an aggregate appends -- reads r, so any block that writes r must
// write it back. Without it max(length(s)) took the max of a register the
// block never wrote, and count(f(x)) would count a NULL f(x) as a row.
func (l *segLowered) readNatively(r int) {
	for bi := range l.blocks {
		b := &l.blocks[bi]
		writes := false
		for _, in := range b.insns {
			_, w, _, _ := segInsnRegs(in)
			for _, x := range w {
				writes = writes || x == r
			}
		}
		if writes && !slices.Contains(b.writeBack, r) {
			b.writeBack = append(b.writeBack, r)
		}
	}
}

// segBlockIsLength reports whether block bi is exactly "length(col)" into one
// register native code reads: Column, any SCopys of it, and the one-argument
// built-in length, nothing loaded and nothing else written back -- and that no
// other block reads what it leaves in the window, since POpTextLen skips it.
func segBlockIsLength(body []Instruction, svc []bool, blocks []segSvcBlock, bi, cursor int) (col, dst int, ok bool) {
	b := &blocks[bi]
	if len(b.load) != 0 || b.flag >= 0 || len(b.writeBack) > 1 || len(b.insns) < 2 {
		return 0, 0, false
	}
	// Column, SCopys of it, length(), then any SCopys of its result -- an
	// aggregate's argument is copied into the aggregate's own register.
	colOf := map[int]int{}
	called := false
	for _, in := range b.insns {
		switch {
		case !called && in.Op == OpColumn && in.P1 == cursor:
			colOf[in.P3] = in.P2
		case !called && in.Op == OpSCopy:
			c, have := colOf[in.P1]
			if !have {
				return 0, 0, false
			}
			colOf[in.P2] = c
		case !called && in.Op == OpFunction:
			name, _ := in.P4.(string)
			c, have := colOf[in.P1]
			if !have || in.P2 != 1 || r33sFoldIdent(name) != "length" {
				return 0, 0, false
			}
			col, dst, called = c, in.P3, true
		case called && in.Op == OpSCopy && in.P1 == dst:
			colOf[dst] = col // the earlier result is a window register now
			dst = in.P2
		default:
			return 0, 0, false
		}
	}
	// The result is written back, or read by no one at all (a min()/max()
	// census copy the kernel never consults): POpTextLen writes it either way.
	if !called || (len(b.writeBack) == 1 && b.writeBack[0] != dst) {
		return 0, 0, false
	}
	for obi, ob := range blocks {
		if obi == bi {
			continue
		}
		for _, in := range ob.insns {
			r, _, _, _ := segInsnRegs(in)
			for _, x := range r {
				if _, written := colOf[x]; written {
					return 0, 0, false
				}
			}
		}
	}
	return col, dst, true
}

// segVecColumn is column c of s when it is clean TEXT: every row a TEXT
// value, no NULL (so no row predates the column) and no exception -- what a
// text operation in a kernel may read as cells.
func segVecColumn(s *segment, c int) (cells []uint64, heap []byte, ok bool) {
	if c < 0 || c >= len(s.cols) || s.cols[c].phys != PhysText {
		return nil, nil, false
	}
	cells, heap, ok = s.BytesColumn(c)
	return cells, heap, ok && len(cells) >= s.nRows
}
