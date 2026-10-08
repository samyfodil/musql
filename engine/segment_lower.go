package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/samyfodil/musql/internal/jit"
)

// Lowering a compiled VDBE loop body into the JIT's IR. Only values that
// are non-NULL int64 are accepted. Three-valued-logic tests on comparisons
// (which cannot produce NULL for integers) are lowered as never-taken.

// segLowered is a loop body compiled to the program JIT's IR, plus what the
// caller must put in the register file before each run.
type segLowered struct {
	insns   []jit.ProgInsn
	cols    []int  // table column index for each column slot the program reads
	nullCol []bool // parallel to cols: fill this slot from the NULL INDICATOR, not the value
	consts  []int  // register slot for each constant, parallel to constVals
	cvKind  []bool // true when the constant is a bound parameter rather than a literal
	cvIndex []int  // parameter index (1-based) or the literal value
	nRegs   int
	// agg is the aggregate the loop feeds. Aggregates are answered from the
	// running sum and matched-row count.
	agg     aggKind
	sumReg  int
	rowsReg int // counts matched rows; sum()/avg() answer NULL at 0
}

// segLowerRegOperands returns the operands of in that name a REGISTER, -1 for
// the slots that do not.
func segLowerRegOperands(in Instruction) [3]int {
	switch in.Op {
	case OpColumn: // P1 cursor, P2 column, P3 destination
		return [3]int{in.P3, -1, -1}
	case OpInteger, OpVariable: // P1 value / parameter index, P2 destination
		return [3]int{in.P2, -1, -1}
	case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe: // P1, P2 operands; P3 destination in store form
		return [3]int{in.P1, in.P2, in.P3}
	case OpIfNot, OpIf, OpIsNull, OpNotNull: // P1 register, P2 jump address
		return [3]int{in.P1, -1, -1}
	case OpNot, OpSCopy, OpNegative: // P1 source, P2 destination
		return [3]int{in.P1, in.P2, -1}
	case OpAdd, OpSubtract, OpMultiply, OpRemainder, OpBitAnd, OpBitOr: // r[P3] = r[P2] op r[P1]
		return [3]int{in.P1, in.P2, in.P3}
	case OpNull: // P1 destination
		return [3]int{in.P1, -1, -1}
	}
	return [3]int{-1, -1, -1} // OpGoto and anything the switch will refuse
}

// segNullTestedOnly reports, per register, whether every instruction that
// reads it is OpIsNull or OpNotNull (so it can hold the NULL INDICATOR
// instead of the value). Conservative: unfamiliar instructions disqualify a register.
func segNullTestedOnly(body []Instruction) map[int]bool {
	loads := map[int]int{}  // register -> how many OpColumns write it
	writes := map[int]int{} // register -> how many instructions write it at all
	reads := map[int]int{}  // register -> reads that are NOT a null test
	for _, in := range body {
		for _, r := range segLowerRegOperands(in) {
			if r >= 0 {
				reads[r]++
			}
		}
		switch in.Op {
		case OpColumn:
			loads[in.P3]++
			writes[in.P3]++
			reads[in.P3]-- // its own destination is not a read
		case OpIsNull, OpNotNull:
			reads[in.P1]-- // a null test does not read the VALUE
		case OpInteger, OpVariable, OpNot, OpSCopy, OpNegative, OpNull:
			writes[in.P2]++
			reads[in.P2]--
		case OpAdd, OpSubtract, OpMultiply, OpRemainder, OpBitAnd, OpBitOr:
			writes[in.P3]++
			reads[in.P3]--
		case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
			if store, _ := segCompareLowerable(in.P5); store {
				writes[in.P3]++
				reads[in.P3]--
			}
		}
	}
	out := map[int]bool{}
	for r, n := range loads {
		if n == 1 && writes[r] == 1 && reads[r] <= 0 {
			out[r] = true
		}
	}
	return out
}

// clone copies a lowered body so several aggregates can share it, each
// appending its own terminal accumulate. The slices are copied rather than
// aliased: appending to two clones of one backing array would have the second
// aggregate's accumulate overwrite the first's.
func (l *segLowered) clone() *segLowered {
	out := *l
	out.insns = append([]jit.ProgInsn(nil), l.insns...)
	out.cols = append([]int(nil), l.cols...)
	out.nullCol = append([]bool(nil), l.nullCol...)
	out.consts = append([]int(nil), l.consts...)
	out.cvKind = append([]bool(nil), l.cvKind...)
	out.cvIndex = append([]int(nil), l.cvIndex...)
	return &out
}

// segLowerBody compiles the instructions of a scan loop body, or reports false
// when it contains an opcode this does not model.
func segLowerBody(body []Instruction, base int, cursor int, aggAt int) (*segLowered, bool) {
	// AN OpNull ON A PATH THAT RUNS REFUSES THE WHOLE BODY, and modelling NULL
	// any more cleverly than that is how this went wrong before.
	//
	// "col = NULL" and "col = 0" compile to STRUCTURALLY IDENTICAL programs --
	// Column, then Null or Integer into the same register, then Eq -- so a NULL
	// literal is a value the comparison reads, not an unreachable epilogue. This
	// lowering once wrote it as the constant zero, which silently turned every
	// "col op NULL" into "col op 0": slt's index/random/scale1000_slt_good_5
	// answered COUNT(*) = 1000 where C SQLite answers 0. It also wrote that zero
	// to the wrong register (OpNull's destination is P2, not P1).
	//
	// So NULL is never given a value here. What IS admitted is an OpNull no
	// lowered path can reach: the NULL arm of the three-valued epilogue OR and
	// IN compile into, which only an OpIsNull or a not-taken OpNotNull leads to
	// -- and no value in a compiled program is NULL, so neither branches there.
	// Each OpNull lowers to a marker, and after the jumps are resolved a walk of
	// the lowered control flow from the entry refuses the body if any marker
	// is reached. "col = NULL" reaches its OpNull on the straight-line path and
	// is refused exactly as before.
	out := &segLowered{}
	colSlot := map[int]int{}         // table column -> program column slot
	irOf := make([]int, len(body)+1) // body index -> IR index
	maxReg := 0
	// One register the lowering owns, above anything the program names, for a
	// comparison whose result the VDBE never stored.
	//
	// It must be derived from the operands that ARE registers, which is why
	// this asks per opcode instead of scanning P1/P2/P3 blindly. Doing that
	// read OpInteger's P1 -- the literal VALUE -- as a register number, so
	// "WHERE v > 500000" sized the register file at 500,003 slots and
	// segSelectRows allocated 4MB of it per segment per query. The same
	// arithmetic on a literal near the int32 ceiling asks for 16GB, which is
	// the never-panic invariant one allocation away from failing.
	scratchReg := 0
	for _, in := range body {
		for _, r := range segLowerRegOperands(in) {
			if r >= scratchReg {
				scratchReg = r + 1
			}
		}
	}
	// WHICH REGISTERS ONLY EVER GET NULL-TESTED.
	//
	// A column loaded as its NULL INDICATOR is a 0/1, not the column's value,
	// so a register holding one may be read by OpIsNull and OpNotNull and by
	// nothing else. This proves that first, over the whole body, because the
	// OpColumn that loads the register comes BEFORE the tests that constrain it
	// and the decision has to be made at the load.
	nullOnly := segNullTestedOnly(body)
	var nullMarks []int // IR indices of OpNull markers
	note := func(r int) {
		if r+1 > maxReg {
			maxReg = r + 1
		}
	}

	for i, in := range body {
		irOf[i] = len(out.insns)
		switch in.Op {
		case OpColumn:
			if in.P1 != cursor {
				return nil, false
			}
			asNull := nullOnly[in.P3]
			// The value slot and the indicator slot for one column are
			// DIFFERENT slots, so the key carries which is wanted. "WHERE v > 0
			// AND v IS NULL" would otherwise reuse one for both.
			key := in.P2
			if asNull {
				key = ^in.P2
			}
			slot, ok := colSlot[key]
			if !ok {
				if len(out.cols) >= jit.MaxProgCols {
					return nil, false
				}
				slot = len(out.cols)
				colSlot[key] = slot
				out.cols = append(out.cols, in.P2)
				out.nullCol = append(out.nullCol, asNull)
			}
			note(in.P3)
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpLoadCol, A: in.P3, B: slot})
		case OpInteger:
			note(in.P2)
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpSetConst, A: in.P2, B: in.P1})
		case OpVariable:
			// The value is not known until the statement runs, so the caller
			// writes it into this register and the program just reads it. That
			// is also what makes one compiled program reusable across bindings.
			note(in.P2)
			out.consts = append(out.consts, in.P2)
			out.cvKind = append(out.cvKind, true)
			out.cvIndex = append(out.cvIndex, in.P1)
		case OpGt, OpGe, OpLt, OpLe, OpEq, OpNe:
			cond, ok := jitCondOfOpcode(in.Op)
			if !ok {
				return nil, false
			}
			store, okP5 := segCompareLowerable(in.P5)
			if !okP5 {
				return nil, false
			}
			if store {
				// "r[P3] <op> r[P1] -> r[P2]".
				note(in.P2)
				out.insns = append(out.insns, jit.ProgInsn{
					Op: jit.POpCmp, A: in.P2, B: in.P3, C: in.P1, Cond: cond})
				break
			}
			// The JUMP form: no p5StoreP2, so the comparison branches to P2 on
			// a true result rather than storing one. An IN list is a chain of
			// these, which is why treating every comparison as a store would
			// not merely miss the shape -- it would answer it wrongly.
			scratch := scratchReg
			out.insns = append(out.insns,
				jit.ProgInsn{Op: jit.POpCmp, A: scratch, B: in.P3, C: in.P1, Cond: cond},
				jit.ProgInsn{Op: jit.POpJumpIfNotZero, A: in.P2 - base, B: scratch})
		case OpIfNot, OpIf:
			op := jit.POpJumpIfZero
			if in.Op == OpIf {
				op = jit.POpJumpIfNotZero
			}
			out.insns = append(out.insns, jit.ProgInsn{
				Op: op, A: in.P2 - base, B: in.P1})
		case OpGoto:
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpJump, A: in.P2 - base})
		case OpIsNull:
			if nullOnly[in.P1] {
				// A REAL test, over the indicator block: jump when it is 1.
				out.insns = append(out.insns, jit.ProgInsn{
					Op: jit.POpJumpIfNotZero, A: in.P2 - base, B: in.P1})
				break
			}
			// Never taken: see the file comment. Emitted as nothing at all, so
			// the IR index still maps for any jump that targets it.
		case OpNotNull:
			if nullOnly[in.P1] {
				// The mirror: jump when the indicator is 0.
				out.insns = append(out.insns, jit.ProgInsn{
					Op: jit.POpJumpIfZero, A: in.P2 - base, B: in.P1})
				break
			}
			// ALWAYS taken, the mirror of OpIsNull: no value in a compiled
			// program can be NULL. An IN list compiles to a chain of these, so
			// lowering it as an unconditional jump is what admits
			// "k IN (1,3,5)" -- the equality tests it guards are the real work
			// and they are already lowerable.
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpJump, A: in.P2 - base})
		case OpNot:
			note(in.P2)
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpNot, A: in.P2, B: in.P1})
		case OpAdd, OpSubtract, OpMultiply, OpRemainder:
			// The VDBE's arithmetic is "r[P3] = r[P2] <op> r[P1]" -- the RIGHT
			// operand is P1 (vdbe_op.go's own note), which is why the operands
			// look reversed here. A remainder by zero is NULL in SQL; POpRem
			// flags it like an overflow and the VDBE answers instead.
			var pop jit.POp
			switch in.Op {
			case OpAdd:
				pop = jit.POpAdd
			case OpSubtract:
				pop = jit.POpSub
			case OpRemainder:
				pop = jit.POpRem
			default:
				pop = jit.POpMul
			}
			note(in.P3)
			out.insns = append(out.insns, jit.ProgInsn{Op: pop, A: in.P3, B: in.P2, C: in.P1})
		case OpNegative:
			// -v as 0 - v. OpNegative's own doc note says a subtraction is NOT
			// equivalent because it does not preserve IEEE negative zero -- and
			// that is a FLOAT fact. Every value in a compiled program is an
			// int64, where -0 is 0, so the rewrite is exact here and nowhere
			// else. POpSub already flags the one integer case that overflows,
			// 0 - MinInt64, which declines the whole program to the VDBE.
			note(scratchReg)
			note(in.P2)
			out.insns = append(out.insns,
				jit.ProgInsn{Op: jit.POpSetConst, A: scratchReg, B: 0},
				jit.ProgInsn{Op: jit.POpSub, A: in.P2, B: scratchReg, C: in.P1})
		case OpBitAnd, OpBitOr:
			// "r[P3] = r[P2] OP r[P1]", the same reversed-operand convention
			// the arithmetic opcodes use. POpAnd/POpOr emit a full 64-bit
			// AND/OR, so although the IR introduced them for the 0/1 results of
			// POpCmp they ARE the bitwise operators -- no new machine code.
			//
			// bitwiseBinaryValue casts a non-integer operand to integer first;
			// here every operand already is one, so no affinity step is lost.
			// Neither operator can overflow.
			pop := jit.POpAnd
			if in.Op == OpBitOr {
				pop = jit.POpOr
			}
			note(in.P3)
			out.insns = append(out.insns, jit.ProgInsn{Op: pop, A: in.P3, B: in.P2, C: in.P1})
		case OpNull:
			// A marker, proven unreachable below or the body is refused. It
			// writes the lowering's scratch register, never a register the
			// program reads.
			note(scratchReg)
			nullMarks = append(nullMarks, len(out.insns))
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpSetConst, A: scratchReg, B: 0})
		case OpSCopy:
			note(in.P2)
			out.insns = append(out.insns, jit.ProgInsn{Op: jit.POpLoadReg, A: in.P2, B: in.P1})
		default:
			return nil, false
		}
	}
	irOf[len(body)] = len(out.insns)

	// Resolve jump targets from PROGRAM addresses to IR indices. A jump past
	// the body -- to the AggStep or the Next -- means this row is finished.
	for i := range out.insns {
		switch out.insns[i].Op {
		case jit.POpJump, jit.POpJumpIfZero, jit.POpJumpIfNotZero:
			t := out.insns[i].A
			if t < 0 || t >= len(body) {
				out.insns[i].A = jit.ProgNextRow
				continue
			}
			out.insns[i].A = irOf[t]
		}
	}
	if len(nullMarks) > 0 {
		reach := segLoweredReachable(out.insns)
		for _, at := range nullMarks {
			if reach[at] {
				return nil, false
			}
		}
	}
	if scratchReg+1 > maxReg {
		maxReg = scratchReg + 1
	}
	out.nRegs = maxReg + 1
	return out, true
}

// segLoweredReachable marks every IR instruction some path from the entry can
// execute. It follows jumps and fall-through, and it is path-sensitive about
// registers set by POpSetConst: a branch on a register whose value is known on
// that path takes only the edge the value selects. That is what proves the
// NULL arm of an IN list dead -- it hangs off a flag the list sets to 0 and
// sets to 1 only on a NotNull branch no compiled value takes. Every other write
// makes a register unknown, so the walk never assumes more than the code says.
// A body too branchy to finish within the step budget reports every
// instruction reachable, which refuses it.
func segLoweredReachable(insns []jit.ProgInsn) []bool {
	seen := make([]bool, len(insns))
	type state struct {
		at    int
		known map[int]int64
	}
	key := func(st state) string {
		b := make([]byte, 0, 16+len(st.known)*12)
		b = append(b, fmt.Sprint(st.at, ":")...)
		regs := make([]int, 0, len(st.known))
		for r := range st.known {
			regs = append(regs, r)
		}
		slices.Sort(regs)
		for _, r := range regs {
			b = append(b, fmt.Sprint(r, "=", st.known[r], ";")...)
		}
		return string(b)
	}
	visited := map[string]bool{}
	work := []state{{at: 0, known: map[int]int64{}}}
	for steps := 0; len(work) > 0; steps++ {
		if steps > 1<<14 {
			for i := range seen {
				seen[i] = true
			}
			return seen
		}
		st := work[len(work)-1]
		work = work[:len(work)-1]
		if st.at < 0 || st.at >= len(insns) {
			continue
		}
		k := key(st)
		if visited[k] {
			continue
		}
		visited[k] = true
		seen[st.at] = true
		in := insns[st.at]
		next := func(at int, known map[int]int64) { work = append(work, state{at, known}) }
		switch in.Op {
		case jit.POpJump:
			if in.A != jit.ProgNextRow {
				next(in.A, st.known)
			}
			continue
		case jit.POpJumpIfZero, jit.POpJumpIfNotZero:
			v, ok := st.known[in.B]
			taken := ok && (v == 0) == (in.Op == jit.POpJumpIfZero)
			if (!ok || taken) && in.A != jit.ProgNextRow {
				next(in.A, st.known)
			}
			if !ok || !taken {
				next(st.at+1, st.known)
			}
			continue
		case jit.POpSkipIfZero:
			next(st.at+1, st.known)
			continue
		}
		known := st.known
		switch in.Op {
		case jit.POpSetConst:
			known = maps.Clone(known)
			known[in.A] = int64(in.B)
		case jit.POpLoadCol, jit.POpLoadReg, jit.POpCmp, jit.POpAnd, jit.POpOr,
			jit.POpNot, jit.POpAdd, jit.POpSub, jit.POpMul, jit.POpRem:
			if _, ok := known[in.A]; ok {
				known = maps.Clone(known)
				delete(known, in.A)
			}
		}
		next(st.at+1, known)
	}
	return seen
}

// jitCondOfOpcode maps a VDBE comparison opcode to the JIT's condition.
func jitCondOfOpcode(op OpCode) (jit.Cond, bool) {
	switch op {
	case OpGt:
		return jit.CondG, true
	case OpGe:
		return jit.CondGE, true
	case OpLt:
		return jit.CondL, true
	case OpLe:
		return jit.CondLE, true
	case OpEq:
		return jit.CondE, true
	case OpNe:
		return jit.CondNE, true
	}
	return 0, false
}

// segCompareLowerable reports whether a comparison's P5 flags describe one this
// can compile, and whether it STORES its result or jumps.
//
// The affinity bits are accepted only where they cannot change the answer
// between two integers:
//
//   - affNone and affNumeric and affInteger are no-ops on integers.
//   - affText would STRINGIFY both operands, which is a different ordering
//     entirely ('10' < '9').
//   - affReal would compare them as floats, which differs from an integer
//     comparison above 2^53 -- the kind of gap that is invisible in a test with
//     small numbers and wrong in production.
//
// p5JumpIfNull and p5NullEq are refused outright. Nothing in a compiled program
// is NULL so they should never fire, but they encode IS / IS NOT semantics and
// accepting a flag on the grounds that it cannot matter is how it comes to
// matter.
func segCompareLowerable(p5 uint16) (store bool, ok bool) {
	if p5&(p5JumpIfNull|p5NullEq) != 0 {
		return false, false
	}
	switch affinity(p5 & p5AffMask) {
	case affNone, affNumeric, affInteger:
	default:
		return false, false
	}
	return p5&p5StoreP2 != 0, true
}
