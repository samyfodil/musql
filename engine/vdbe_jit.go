package engine

import (
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// The JITted VDBE: internal/jit compiles every instruction it has a native
// form for; run() enters that code at any pc and interprets instructions
// it cannot execute (unsupported opcodes, types, overflows, etc.).

// vmJIT is one program's native code.
type vmJIT struct {
	code   *jit.Code
	insns  *Instruction // the program it was compiled from: run() checks identity
	n      int
	nReg   int
	entry  []bool        // the pcs the VDBE enters native code at
	marked []Instruction // the program with OpJIT at each entry: what run() dispatches on
	native int           // how many of its instructions have native code (the JIT score's static share)

	// curs is the cursor numbers the native code reads or advances (VColumn,
	// VNext), and nCurs the table size it indexes (the largest + 1). gate[pc]
	// is the first of them an entry at pc reaches, or -1: the VDBE skips an
	// entry whose cursor native code could not use, which would only pay for
	// the call and exit at that instruction.
	curs  []int
	nCurs int
	gate  []int16
	// fill[pc] lists, for an entry at pc, the columns of each cursor that the
	// native code on the straight path from pc reads (VColumn): what the
	// engine copies out of a cursor it cannot hand over as a row list. Nil
	// for a pc with none.
	fill [][]curCols
}

// curCols is one cursor's columns an entry reads.
type curCols struct {
	cursor int
	cols   []int
}

// vmJITEnabled controls the whole-program JIT (off with WithoutJIT).
var vmJITEnabled = jitEnabled

// noVMJIT caches when a Program has no native code.
var noVMJIT = &vmJIT{}

// jitCode is prog's native code, compiled on its first run and cached on the
// Program -- which the plan cache keeps, so a prepared statement compiles once.
// nil when it has none.
//
// The cache is checked against prog.Insns rather than trusted: a Program
// copied by value (limit_offset_halt.go) and given new instructions carries
// the old pointer, and compiles its own.
func (prog *Program) jitCode() *vmJIT {
	if len(prog.Insns) == 0 {
		return nil
	}
	if jx := (*vmJIT)(atomic.LoadPointer(&prog.vmJIT)); jx != nil && (jx == noVMJIT || jx.insns == &prog.Insns[0] && jx.n == len(prog.Insns)) {
		if jx == noVMJIT {
			return nil
		}
		return jx
	}
	jx := compileVMJIT(prog)
	if jx == nil {
		atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(noVMJIT))
		return nil
	}
	atomic.StorePointer(&prog.vmJIT, unsafe.Pointer(jx))
	return jx
}

var valueLayout = jit.ValueLayout{
	Size:    int(unsafe.Sizeof(Value{})),
	OffTyp:  int(unsafe.Offsetof(Value{}.Typ)),
	OffI:    int(unsafe.Offsetof(Value{}.I)),
	OffF:    int(unsafe.Offsetof(Value{}.F)),
	OffS:    int(unsafe.Offsetof(Value{}.S)),
	TypNull: byte(Null),
	TypInt:  byte(Int),
}

// compileVMJIT compiles prog, or returns nil: the JIT off, no emitter for this
// platform, or nothing in the program it could run natively. nil means the
// VDBE runs every instruction, which is always correct.
func compileVMJIT(prog *Program) *vmJIT {
	if !vmJITEnabled || len(prog.Insns) == 0 {
		return nil
	}
	vp := make([]jit.VInsn, len(prog.Insns))
	entry := make([]bool, len(prog.Insns))
	native := 0
	scannable := scannableCursors(prog.Insns, prog.NCursors)
	for pc := range prog.Insns {
		vp[pc] = lowerVM(&prog.Insns[pc], prog.NReg)
		// A column of any cursor is read natively (a cursor native code cannot
		// walk is read from a copy of its current row); only a cursor that
		// materializes its rows is walked natively.
		if c := vp[pc]; !vmJITCursorOps && (c.Op == jit.VColumn || c.Op == jit.VNext || c.Op == jit.VRewind) {
			vp[pc] = jit.VInsn{Op: jit.VExit}
		} else if (c.Op == jit.VNext || c.Op == jit.VRewind) && (c.A >= len(scannable) || !scannable[c.A]) {
			vp[pc] = jit.VInsn{Op: jit.VExit}
		}
		if vp[pc].Op != jit.VExit {
			entry[pc] = true
			native++
		}
	}
	if native == 0 {
		return nil
	}
	// The VDBE enters only where the code would then do at least
	// vmJITMinWork before handing control back. Every block stays reachable
	// from native code either way.
	//
	// A program left with no entry at all gets no code: its run loop then pays
	// nothing for the JIT, not even the check.
	entries := 0
	for pc := range entry {
		if entry[pc] && !worthEntering(vp, pc) {
			entry[pc] = false
		}
		if entry[pc] {
			entries++
		}
	}
	if entries == 0 {
		return nil
	}
	b, err := jit.EmitVM(vp, prog.NReg, valueLayout)
	if err != nil {
		return nil
	}
	code, err := jit.Map(b)
	if err != nil {
		return nil
	}
	marked := append([]Instruction(nil), prog.Insns...)
	for pc, e := range entry {
		if e {
			marked[pc].Op = OpJIT
		}
	}
	jx := &vmJIT{code: code, insns: &prog.Insns[0], n: len(prog.Insns), nReg: prog.NReg, entry: entry, marked: marked, native: native}
	jx.gate = make([]int16, len(vp))
	jx.fill = make([][]curCols, len(vp))
	seen := map[int]bool{}
	for pc := range vp {
		jx.gate[pc] = -1
		if !entry[pc] {
			continue
		}
		jx.gate[pc] = int16(firstCursor(vp, pc))
		jx.fill[pc] = fillColumns(vp, pc)
	}
	for _, in := range vp {
		if (in.Op == jit.VColumn || in.Op == jit.VNext || in.Op == jit.VRewind) && !seen[in.A] {
			seen[in.A] = true
			jx.curs = append(jx.curs, in.A)
			jx.nCurs = max(jx.nCurs, in.A+1)
		}
	}
	return jx
}

// scannableCursors marks the cursor numbers native code may read: every
// instruction that opens the cursor opens a materializing kind -- a table read
// that is not a streamed scan (OpOpenRead, P5 bit 0) or a derived source. A
// write-path cursor (OpOpenWrite), whose current row can differ from the row
// list it was built from, never is; neither is a cursor nothing opens.
func scannableCursors(insns []Instruction, nCursors int) []bool {
	ok := make([]bool, nCursors)
	opened := make([]bool, nCursors)
	for i := range insns {
		in := &insns[i]
		var scan bool
		switch in.Op {
		case OpOpenRead:
			scan = in.P5&1 == 0
		case OpOpenDerived:
			scan = true
		case OpOpenWrite:
			scan = false
		default:
			continue
		}
		if in.P1 < 0 || in.P1 >= nCursors {
			continue
		}
		if !opened[in.P1] {
			opened[in.P1], ok[in.P1] = true, scan
		} else if !scan {
			ok[in.P1] = false
		}
	}
	return ok
}

// fillColumns is the columns of each cursor the VColumn instructions on the
// straight path from pc (the path nativeRun weighs, a little longer) read.
func fillColumns(vp []jit.VInsn, pc int) []curCols {
	var out []curCols
	for steps := 0; steps < 48 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		in := vp[pc]
		switch in.Op {
		case jit.VGoto:
			pc = in.T
			continue
		case jit.VColumn:
			i := slices.IndexFunc(out, func(cc curCols) bool { return cc.cursor == in.A })
			if i < 0 {
				out = append(out, curCols{cursor: in.A})
				i = len(out) - 1
			}
			if !slices.Contains(out[i].cols, in.B) {
				out[i].cols = append(out[i].cols, in.B)
			}
		}
		pc++
	}
	return out
}

// firstCursor is the first cursor number a cursor instruction on the native
// run from pc (the same path nativeRun weighs) names, or -1.
func firstCursor(vp []jit.VInsn, pc int) int {
	for steps := 0; steps < 16 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		switch vp[pc].Op {
		case jit.VGoto:
			pc = vp[pc].T
			continue
		case jit.VColumn, jit.VNext, jit.VRewind:
			return vp[pc].A
		}
		pc++
	}
	return -1
}

var (
	vmArith = map[OpCode]jit.VOp{OpAdd: jit.VAdd, OpSubtract: jit.VSub, OpMultiply: jit.VMul,
		OpBitAnd: jit.VAnd, OpBitOr: jit.VOr, OpShiftLeft: jit.VShl, OpShiftRight: jit.VShr}
	vmCond = map[OpCode]jit.Cond{OpEq: jit.CondE, OpNe: jit.CondNE, OpLt: jit.CondL,
		OpLe: jit.CondLE, OpGt: jit.CondG, OpGe: jit.CondGE}
)

// vmJITCursorOps is whether Column and Next have native forms: on, except in
// the benchmark that measures what they add.
var vmJITCursorOps = true

// vmJITMinWork is the least native work worth an entry from the VDBE, in
// nativeRun's units. An entry and its exit cost ~4.7ns natively (measured on
// a two-SCopy block), about what the VDBE spends interpreting two copies --
// so a run of copies and constants never pays, while one integer opcode, which
// the VDBE spends several ns on, does once anything rides along with it. The
// platform's figure is jit.VMMinWork: an entry on js/wasm crosses into the JS
// host and back, which costs more than a native call.
var vmJITMinWork = jit.VMMinWork

// vmJITCursorEntryWork is what an entry whose run reads or advances a cursor
// costs beyond a plain one, in nativeRun's units: the engine fills the native
// view of each cursor before the call and brings the cursor up to date after.
const vmJITCursorEntryWork = 5

// worthEntering reports whether an entry at pc does enough native work to pay
// for itself: vmJITMinWork, plus vmJITCursorEntryWork when the run touches a
// cursor.
func worthEntering(vp []jit.VInsn, pc int) bool {
	w, cursor := nativeRun(vp, pc, vmJITMinWork+vmJITCursorEntryWork)
	if cursor {
		return w >= vmJITMinWork+vmJITCursorEntryWork
	}
	return w >= vmJITMinWork
}

// nativeRun weighs the native instructions from pc along the fall-through and
// unconditional-jump path, up to limit: a copy or a constant is 1, a jump
// nothing, anything that computes or tests 2. It reports whether the run
// includes a cursor instruction.
func nativeRun(vp []jit.VInsn, pc, limit int) (w int, cursor bool) {
	for steps := 0; w < limit && steps < 64 && pc >= 0 && pc < len(vp) && vp[pc].Op != jit.VExit; steps++ {
		switch vp[pc].Op {
		case jit.VGoto:
			pc = vp[pc].T
			continue
		case jit.VCopy, jit.VInt:
			w++
		case jit.VColumn, jit.VNext, jit.VRewind:
			w += 2
			cursor = true
		default:
			w += 2
		}
		pc++
	}
	return w, cursor
}

// lowerVM is in's native form, or VExit. Each case is the INTEGER arm of the
// VDBE's own case for that opcode (vdbe.go), which the VDBE also tests first;
// every other operand type exits to it.
func lowerVM(in *Instruction, nReg int) jit.VInsn {
	exit := jit.VInsn{Op: jit.VExit}
	ok := func(regs ...int) bool {
		for _, r := range regs {
			if r < 0 || r >= nReg {
				return false
			}
		}
		return true
	}
	switch in.Op {
	case OpInit, OpGoto:
		return jit.VInsn{Op: jit.VGoto, T: in.P2}
	case OpInteger:
		if ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: int64(in.P1)}
		}
	case OpInt64:
		if v, isInt := in.P4.(int64); isInt && ok(in.P2) {
			return jit.VInsn{Op: jit.VInt, A: in.P2, Imm: v}
		}
	case OpSCopy, OpCopy:
		// copyValue differs from a plain copy only for a string or blob, which
		// the native copy refuses.
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VCopy, A: in.P1, B: in.P2}
		}
	case OpAdd, OpSubtract, OpMultiply, OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
		if ok(in.P1, in.P2, in.P3) {
			return jit.VInsn{Op: vmArith[in.Op], A: in.P1, B: in.P2, C: in.P3}
		}
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		// Two INTEGERs answer before NULL handling, affinity or collation
		// (compareOp's first arm, vdbe.c:2288), so P4 and P5 matter only for
		// the store-the-result form, which stays the VDBE's.
		if in.P5&p5StoreP2 == 0 && ok(in.P1, in.P3) {
			return jit.VInsn{Op: jit.VCmpJump, A: in.P1, B: in.P3, T: in.P2, Cond: vmCond[in.Op]}
		}
		// ...and in the store form, the same two integers give 0 or 1 in r[P2]
		// (deliverCompare).
		if in.P5&p5StoreP2 != 0 && ok(in.P1, in.P3, in.P2) {
			return jit.VInsn{Op: jit.VCmpSet, A: in.P1, B: in.P3, C: in.P2, Cond: vmCond[in.Op]}
		}
	case OpIf, OpIfNot:
		if ok(in.P1) {
			want := int64(0)
			if in.Op == OpIf {
				want = 1
			}
			return jit.VInsn{Op: jit.VIf, A: in.P1, T: in.P2, C: in.P3, Imm: want}
		}
	case OpIsNull, OpNotNull:
		if ok(in.P1) {
			want := int64(0)
			if in.Op == OpIsNull {
				want = 1
			}
			return jit.VInsn{Op: jit.VNullJump, A: in.P1, T: in.P2, Imm: want}
		}
	case OpNull:
		if ok(in.P2) {
			return jit.VInsn{Op: jit.VNull, C: in.P2}
		}
	case OpNegative:
		if ok(in.P1, in.P2) {
			return jit.VInsn{Op: jit.VNeg, A: in.P1, C: in.P2}
		}
	case OpColumn:
		// Native code reads a cursor's column from its row list; whether the
		// cursor is one it may (scannableCursors) is checked by the caller, and
		// whether it is right now, at every entry (jitCursorOK).
		if ok(in.P3) && in.P1 >= 0 && in.P1 < 512 && in.P2 >= 0 && in.P2 < 64 {
			return jit.VInsn{Op: jit.VColumn, A: in.P1, B: in.P2, C: in.P3}
		}
	case OpRewind:
		// Only a cursor the engine has already materialized and may re-walk
		// (jitCursorOK): its rewind is a reset of the position, which the
		// native code does; the first rewind and a re-seek take the VDBE's.
		if in.P1 >= 0 && in.P1 < 512 && in.P4 == nil {
			return jit.VInsn{Op: jit.VRewind, A: in.P1, T: in.P2}
		}
	case OpNext:
		if in.P1 >= 0 && in.P1 < 512 {
			return jit.VInsn{Op: jit.VNext, A: in.P1, T: in.P2}
		}
	case OpGosub:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VGosub, A: in.P1, T: in.P2}
		}
	case OpReturn:
		if ok(in.P1) {
			return jit.VInsn{Op: jit.VReturn, A: in.P1}
		}
	}
	return exit
}

// vmJITCursorSyncs counts the entries after which native code had advanced a
// cursor and the engine brought the cursor up to date -- what a test reads to
// know the cursor instructions really ran natively.
var vmJITCursorSyncs atomic.Int64

// jitCursorOK reports whether native code may read and advance cur right now:
// a materialized row list whose current row is rows[pos] -- no streamed or
// segment source, no write-path cursor (whose row can be refreshed apart from
// the list), no NULL row, no lazily normalized rows, no masked row.
func jitCursorOK(cur *vdbeCursor) bool {
	return cur != nil && cur.materialized && cur.sub == nil && cur.segSrc == nil && cur.segCur == nil &&
		cur.rowStore == nil && !cur.lazyNorm && cur.rowRaw == nil && !cur.rowidNull &&
		cur.streamErrPending == nil && len(cur.rows) >= len(cur.rowids)
}

// jitSnap is the one-row list a cursor native code can read but not walk is
// handed over as: the row's columns copied out through readColumn, in a list
// of one row. The header array is part of the struct so its address is fixed.
type jitSnap struct {
	row []Value
	hdr [1][]Value
}

// jitReadColumn is cur.readColumn for a cursor whose state is not known to
// hold a row: false, writing nothing, where the row has no column i.
func jitReadColumn(cur *vdbeCursor, i int, dst *Value) bool {
	switch {
	case cur.segCur != nil:
		*dst = cur.segColumn(i)
		return true
	case cur.rowRaw != nil:
		if !cur.colMask.has(i) {
			if cur.decodeFullRow() != nil {
				return false
			}
		}
	}
	if i < 0 || i >= len(cur.rowVals) {
		return false
	}
	*dst = cur.rowVals[i]
	return true
}

// enter runs native code from pc and returns the pc the VDBE runs next.
func (jx *vmJIT) enter(m *vdbe, pc int) int {
	regs := m.regs
	if len(regs) < jx.nReg || jx.nReg == 0 {
		return pc
	}
	args := jit.ProgArgs{Regs: (*int64)(unsafe.Pointer(&regs[0])), PC: int64(pc)}
	if jx.nCurs > 0 {
		if len(m.jcur) < jx.nCurs {
			m.jcur = make([]jit.VCursor, jx.nCurs)
			m.jsnap = make([]jitSnap, jx.nCurs)
		}
		for _, c := range jx.curs {
			jc := &m.jcur[c]
			*jc = jit.VCursor{Last: -1}
			if c >= len(m.cursors) || m.cursors[c] == nil {
				continue
			}
			cur := m.cursors[c]
			if jitCursorOK(cur) {
				jc.Rows = unsafe.Pointer(unsafe.SliceData(cur.rows))
				jc.N, jc.Pos, jc.Ok, jc.Adv, jc.Mask = int64(len(cur.rowids)), int64(cur.pos), 1, 1, ^uint64(0)
				if !cur.seekReseek {
					jc.Rewind = 1 // a re-seeking cursor rebuilds its rows at every rewind
				}
				continue
			}
			// Not a cursor native code can walk: hand over a copy of its current
			// row, with the columns this entry reads, and let it read only
			// those. Nothing native moves it (Adv and Rewind are zero), so the
			// copy stays the cursor's row for as long as the code runs.
			for _, cc := range jx.fill[pc] {
				if cc.cursor != c {
					continue
				}
				sn := &m.jsnap[c]
				n := 0
				for _, col := range cc.cols {
					n = max(n, col+1)
				}
				if cap(sn.row) < n {
					sn.row = make([]Value, n)
				}
				sn.row = sn.row[:n]
				var mask uint64
				for _, col := range cc.cols {
					if jitReadColumn(cur, col, &sn.row[col]) {
						mask |= 1 << uint(col)
					}
				}
				if mask != 0 {
					sn.hdr[0] = sn.row
					jc.Rows, jc.N, jc.Pos, jc.Ok, jc.Mask = unsafe.Pointer(&sn.hdr[0]), 1, 0, 1, mask
				}
			}
		}
		args.Cur = &m.jcur[0]
	}
	jx.code.Call2(&args)
	if jx.nCurs > 0 {
		for _, c := range jx.curs {
			jc := &m.jcur[c]
			if sn := &m.jsnap[c]; len(sn.row) != 0 {
				clear(sn.row) // the copy holds no row past the call
				sn.hdr[0] = nil
			}
			if jc.Dirty != 0 {
				// The native advance moved only the position; the cursor's
				// current row, rowid and the rest follow it here, as advance()
				// would have left them. Past the end advance leaves the last row
				// current, and so does this (Last).
				vmJITCursorSyncs.Add(1)
				cur := m.cursors[c]
				cur.pos = int(jc.Pos)
				if jc.Last >= 0 {
					cur.rowid = cur.rowids[jc.Last]
					cur.rowVals = cur.rows[jc.Last]
					cur.rowRaw = nil
				}
			}
		}
	}
	return int(args.PC)
}
