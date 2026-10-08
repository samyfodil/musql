package engine

import (
	"strings"
	"sync/atomic"
)

// The JITted VDBE's recogniser: turn an ordinary filter-and-count loop into one
// opcode a vector kernel answers.
//
// It is a peephole over the compiled program, not a branch in the compiler:
// the compiler decides what a query means; this only decides how the program
// runs, and leaves anything it does not recognise untouched (RULE #1).
//
// The shape it matches -- what "SELECT count(*) FROM t WHERE a OP ? AND b OP
// ?" compiles to:
//
//	Init / AggReset / OpenRead
//	Rewind      -> end
//	  Column c  -> rX          |
//	  Integer k -> rY          | once per predicate
//	  <cmp>     rY, rZ, rX     |
//	  IfNot     rZ -> next     |
//	  AggStep
//	next: Next  -> loop
//	Close / AggResult / ResultRow / Halt
//
// Every instruction, register and jump target is checked; anything else is
// returned untouched, since a loose match fails as a fast wrong answer.

// segFilterPlan is what OpSegFilterCount carries: the predicates and the
// cursor whose table they read.
//
// Each predicate carries a literal or a parameter index, resolved at run time,
// so a prepared "WHERE col > ?" qualifies. Not the register the bound lands in:
// OpSegFilterCount is a guard before the loop, and the instruction filling that
// register is inside it, so at guard time the register is empty. m.params is
// filled before the program starts.
type segFilterPlan struct {
	preds []segPlanPred
	// isSum says the statement is sum(<sumCol>) rather than count(*). It is a
	// separate flag rather than a -1 sentinel so the ZERO VALUE of this struct
	// means count(*) and not "sum of column 0" -- a plan built without setting
	// it would otherwise answer a different query, silently and confidently.
	//
	// The column is read out of the PROGRAM -- the lowered
	// "Column <col> -> r; SCopy r -> argreg" pair before the AggStep -- not out
	// of the aggregate's AST expression, which would mean re-resolving a name
	// this recogniser has no scope for.
	isSum  bool
	sumCol int

	// semis are correlated EXISTS answered as semi-joins; a count only.
	semis []segPlanSemi
}

// segPlanPred is a predicate whose bound is a literal, or the 1-based index of
// the parameter supplying it.
type segPlanPred struct {
	Col int
	Op  segPredOp
	// Lit is the bound when ParamIdx == 0, TYPED. It was an int64 field, which
	// is why a REAL bound could not be represented at all and every non-integer
	// filter silently kept the VDBE's loop -- measured at 7.9x SLOWER than C
	// SQLite on a REAL column where the same query over an INTEGER column was
	// 0.01x. The plan could not say "REAL", so nothing downstream ever got the
	// chance to.
	Lit      Value
	ParamIdx int // 1-based; 0 means the bound is the literal above
	// Aff is the comparison's affinity (P5), which the loop applies to the
	// bound before comparing and so must segPlanPreds.
	Aff affinity
}

// segPeephole rewrites prog in place when it is a filter-count over a table
// this build can answer with a vector kernel. Reports whether it did.
//
// It refuses outright when the JIT is off, because the interpreter's own path
// through segFilterCount is not faster than the loop being replaced -- the
// rewrite would be churn with no payoff and a second thing to be wrong.
func segPeephole(prog *Program) bool {
	if !jitEnabled || prog == nil {
		return false
	}
	in := prog.Insns
	// The fixed prologue and epilogue. Anything else -- a subquery, a LIMIT, a
	// second cursor, an ORDER BY -- changes these and is declined here.
	if len(in) < 9 ||
		in[0].Op != OpInit || in[1].Op != OpAggReset || in[2].Op != OpOpenRead {
		return false
	}
	// A prologue may sit between the open and the Rewind:
	//
	//	OpAutoIndexOrder   whenever the table carries ANY index
	//	OpVariable/OpInteger + OpSeekIndexHint/OpSeekRowidHint
	//	                   whenever the WHERE pins a rowid or an indexed
	//	                   column to a constant
	//
	// Skipping either is safe: neither changes which rows satisfy the WHERE.
	// OpAutoIndexOrder only permutes scan order, and a count ignores order; a
	// seek hint only restricts candidates while its conjunct stays in the
	// WHERE -- and this opcode does not use the cursor, it evaluates the
	// predicate over every row from the segments.
	//
	// The guard goes before the whole prologue, since positioning and
	// materializing the cursor is the cost it avoids. The list is a
	// whitelist: an unknown opcode here might matter.
	rewindAt := 3
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	end := segMainEnd(in)
	if end < 4 || in[end-1].Op != OpHalt || in[end-2].Op != OpResultRow ||
		in[end-3].Op != OpAggResult || in[end-4].Op != OpClose {
		return false
	}
	// The output item must BE the aggregate, not an expression around it -- see
	// segAggOutputsAreBare for the wrong answers this omission produced.
	if !segAggOutputsAreBare(segAggPlanOf(in[end-3])) {
		return false
	}
	closeAt := end - 4
	if in[rewindAt].P2 != closeAt { // Rewind's empty-table target must be the Close
		return false
	}
	nextAt := closeAt - 1
	if in[nextAt].Op != OpNext || in[nextAt].P2 != rewindAt+1 || in[nextAt].P1 != in[rewindAt].P1 {
		return false
	}
	aggAt := nextAt - 1
	if in[aggAt].Op != OpAggStep || in[aggAt].P3 != 0 {
		return false
	}
	plan, ok := in[aggAt].P4.(*aggPlan)
	if !ok {
		return false
	}
	// count(*), or sum(<column>). For sum the two instructions before the
	// AggStep load the column and copy it into the aggregate's argument
	// register; requiring exactly that pair is what identifies the column
	// without resolving the expression.
	isSum, sumCol := false, 0
	predEnd := aggAt
	switch {
	case segPlanIsPlainCountStar(plan):
	case segPlanIsPlainSum(plan):
		if aggAt < 2 {
			return false
		}
		col, scopy := in[aggAt-2], in[aggAt-1]
		if col.Op != OpColumn || col.P1 != in[rewindAt].P1 ||
			scopy.Op != OpSCopy || scopy.P1 != col.P3 {
			return false
		}
		isSum, sumCol = true, col.P2
		predEnd = aggAt - 2
	default:
		return false
	}

	// The predicate block: groups of four, Column/bound/cmp/IfNot. EMPTY is
	// allowed -- `SELECT count(*) FROM t` has no predicates at all, and a
	// segment already knows its row count, so that query is addition rather
	// than a scan.
	//
	// The groups come in two spellings. A WHERE of plain conjuncts jumps each
	// failed test straight to the Next (IfNot P3=1). An AND inside one
	// expression -- what BETWEEN compiles to -- jumps every failed test to a
	// shared FALSE label (IfNot P3=0) and then settles the three-valued result
	// in a fixed tail; segAndTreeTail checks that tail.
	// Plain predicate groups, and between them any correlated EXISTS the
	// inliner turned into a subroutine call that a semi-join can answer
	// (segment_semijoin.go).
	var preds []segPlanPred
	var semis []segPlanSemi
	pc := rewindAt + 1
	for {
		more, npc, ok := segParsePredBlock(in, pc, predEnd, in[rewindAt].P1, nextAt)
		if !ok {
			return false
		}
		preds, pc = append(preds, more...), npc
		sj, npc, ok := segParseSemiGroup(in, pc, predEnd, in[rewindAt].P1, nextAt)
		if !ok {
			break
		}
		semis, pc = append(semis, sj), npc
	}
	if len(semis) > 0 && isSum {
		return false // the semi-join answers a count only
	}
	// Zero, one or two. One was excluded only because the first scalar kernel
	// could not beat Go's loop at a single compare, which is no longer true on
	// any architecture measured. Zero is `SELECT count(*) FROM t`, where C
	// SQLite counts b-tree entries without decoding rows (its OP_Count) and this
	// engine walked and decoded every one -- 139ms against C's 12us at 100k
	// rows. A segment stores its row count, so the answer is a sum over
	// segments.
	if pc != predEnd || len(preds) > 2 {
		return false
	}

	// Rewrite: a GUARD before the loop, and the loop kept exactly as it was.
	//
	// The loop stays because the guard can decline at run time -- a column that
	// is not a fixed-width block, a NULL, a value in the exception list -- and
	// something has to answer the query when it does. Deleting the loop would
	// make that decline an error instead of a fallback, which is the whole
	// reason this is a guard rather than a replacement.
	//
	// The guard jumps to a DUPLICATED tail (close, emit, halt) rather than into
	// the original one, because the original runs AggResult, which would
	// overwrite the count with the accumulators the loop never filled.
	dst := in[end-3].P1
	out := make([]Instruction, 0, len(in)+4)
	out = append(out, in[0], in[1], in[2])
	guardTarget := len(in) + 1 // the duplicated tail, after every shifted insn
	out = append(out, Instruction{Op: OpSegFilterCount, P1: dst, P2: in[rewindAt].P1, P3: guardTarget,
		P4: &segFilterPlan{preds: preds, isSum: isSum, sumCol: sumCol, semis: semis}})
	// Everything from the third instruction on -- which is OpAutoIndexOrder when
	// there is one, then the Rewind -- one address later than it was.
	for _, ins := range in[3:] {
		switch ins.Op {
		case OpRewind, OpNext, OpIfNot, OpIsNull, OpGoto, OpGosub:
			ins.P2++
		}
		out = append(out, ins)
	}
	out = append(out, in[closeAt], in[end-2], in[end-1])
	if len(out) != len(in)+4 || out[guardTarget].Op != OpClose {
		return false // the arithmetic and the program disagree; change nothing
	}
	prog.Insns = out
	return true
}

// segPeepholeBoundOp reports whether an instruction can supply a predicate's
// bound. OpInteger and OpVariable were the whole list, which is why a REAL, TEXT
// or BLOB filter never reached the columnar path at all.
func segPeepholeBoundOp(op OpCode) bool {
	switch op {
	case OpInteger, OpVariable, OpReal, OpString8, OpBlob:
		return true
	}
	return false
}

// segPeepholeCompareOK reports whether a comparison is one the block path
// implements (as segCompareLowerable does):
//
//   - p5StoreP2 set: the peephole rewrites a compare-and-store, not a
//     compare-and-jump.
//   - p5JumpIfNull and p5NullEq clear: those change what a NULL means; the
//     block path applies the ordinary rule (a NULL operand makes no comparison
//     true).
//   - an affinity that is a no-op on the block's values: affNone, affText or
//     affNumeric, given segPredColsFilterable already refused columns with
//     exceptions. The bound is converted by segPlanPreds as the loop does.
//   - BINARY collation: P4 defaults to BINARY and the block path compares via
//     compareValues, so NOCASE or RTRIM must decline.
func segPeepholeCompareOK(cmp Instruction) bool {
	if cmp.P5&p5StoreP2 == 0 || cmp.P5&(p5JumpIfNull|p5NullEq) != 0 {
		return false
	}
	switch affinity(cmp.P5 & p5AffMask) {
	case affNone, affText, affNumeric:
	default:
		return false
	}
	if coll, ok := cmp.P4.(string); ok && coll != "" && !strings.EqualFold(coll, "BINARY") {
		return false
	}
	return true
}

// segOpOfCompare maps a comparison opcode to the predicate operator, reporting

// segParsePredBlock reads the predicate block that starts at pc over cursor:
// groups of Column/bound/cmp/IfNot whose failed test skips the row (to nextAt),
// possibly followed by an AND tree's tail. It stops at the first instruction
// that does not start such a group and returns the predicates and that pc;
// ok is false when a group starts but does not match exactly.
func segParsePredBlock(in []Instruction, pc, limit, cursor, nextAt int) (preds []segPlanPred, end int, ok bool) {
	preds = make([]segPlanPred, 0, 2)
	var cmpRegs []int
	falseAt := -1
	for pc+3 < limit && in[pc].Op == OpColumn && segLooksLikePredGroup(in, pc, limit) {
		col, bound, cmp, ifnot := in[pc], in[pc+1], in[pc+2], in[pc+3]
		// A negative numeric literal is spelled "Integer 100; Negative": the
		// bound then sits in the Negative's register, and the group is five
		// instructions long.
		boundReg, size, negate := bound.P2, 4, false
		if (bound.Op == OpInteger || bound.Op == OpReal) && cmp.Op == OpNegative &&
			cmp.P1 == bound.P2 && pc+4 < limit {
			boundReg, size, negate = cmp.P2, 5, true
			cmp, ifnot = in[pc+3], in[pc+4]
		}
		direct := ifnot.P3 == 1 && ifnot.P2 == nextAt
		tree := ifnot.P3 == 0 && (falseAt < 0 || ifnot.P2 == falseAt)
		if tree {
			falseAt = ifnot.P2
		}
		// OpInteger for a literal, OpVariable for a bound parameter. Both do
		// the same thing here -- put the comparison value in a register -- and
		// the instructions after them are identical, which is why one matcher
		// covers both. Nothing else is accepted: an expression bound would put
		// arbitrary opcodes in this slot.
		if col.Op != OpColumn || col.P1 != cursor ||
			!segPeepholeBoundOp(bound.Op) || bound.P2 != col.P3+1 || (negate && boundReg != bound.P2+1) ||
			ifnot.Op != OpIfNot || !(direct || tree) || (direct && falseAt >= 0) {
			return nil, 0, false
		}
		op, ok := segOpOfCompare(cmp.Op)
		if !ok || cmp.P3 != col.P3 || cmp.P1 != boundReg || cmp.P2 != boundReg+1 ||
			ifnot.P1 != cmp.P2 {
			return nil, 0, false
		}
		// The comparison must be one the block path actually implements.
		if !segPeepholeCompareOK(cmp) {
			return nil, 0, false
		}
		pred := segPlanPred{Col: col.P2, Op: op, Aff: affinity(cmp.P5 & p5AffMask)}
		switch bound.Op {
		case OpVariable:
			pred.ParamIdx = bound.P1 // 1-based, per OpVariable's contract
		case OpReal:
			f, ok := bound.P4.(float64)
			if !ok {
				return nil, 0, false
			}
			pred.Lit = Value{Typ: Float, F: f}
		case OpString8:
			t, ok := bound.P4.(string)
			if !ok {
				return nil, 0, false
			}
			pred.Lit = Value{Typ: Text, S: []byte(t)}
		case OpBlob:
			b, ok := bound.P4.([]byte)
			if !ok {
				return nil, 0, false
			}
			pred.Lit = Value{Typ: Blob, S: append([]byte(nil), b...)}
		default:
			pred.Lit = Value{Typ: Int, I: int64(bound.P1)}
		}
		if negate {
			switch pred.Lit.Typ {
			case Int:
				pred.Lit.I = -pred.Lit.I
			case Float:
				pred.Lit.F = -pred.Lit.F
			default:
				return nil, 0, false
			}
		}
		preds = append(preds, pred)
		cmpRegs = append(cmpRegs, cmp.P2)
		pc += size
	}
	if falseAt >= 0 {
		end, ok := segAndTreeTail(in, pc, cmpRegs, falseAt, nextAt)
		if !ok {
			return nil, 0, false
		}
		pc = end
	}
	return preds, pc, true
}

// segMainEnd is where prog's main body ends: len(in), or, when inlined
// subroutines follow the body (existsInline appends them), just past the Halt
// that precedes the first of them. Everything after it must be reachable only
// through a Gosub, which this checks rather than assumes.
func segMainEnd(in []Instruction) int {
	first := len(in)
	for _, ins := range in {
		if ins.Op == OpGosub && ins.P2 < first {
			first = ins.P2
		}
	}
	if first == len(in) {
		return len(in)
	}
	if first < 1 || in[first-1].Op != OpHalt {
		return -1
	}
	for i := 0; i < first; i++ {
		if in[i].Op != OpGosub && in[i].P2 >= first && segJumpsP2(in[i].Op) {
			return -1 // the body jumps into the subroutines: not a plain tail
		}
	}
	return first
}

// segJumpsP2 reports whether op's P2 is a jump target, for segMainEnd.
func segJumpsP2(op OpCode) bool {
	switch op {
	case OpRewind, OpNext, OpIfNot, OpIf, OpIsNull, OpNotNull, OpGoto, OpInit:
		return true
	}
	return false
}

// segLooksLikePredGroup reports whether the Column at pc starts a predicate
// group rather than, say, a GROUP BY key load: its IfNot sits three or (after a
// Negative) four instructions on.
func segLooksLikePredGroup(in []Instruction, pc, limit int) bool {
	if in[pc+3].Op == OpIfNot {
		return true
	}
	return pc+4 < limit && in[pc+2].Op == OpNegative && in[pc+4].Op == OpIfNot
}

// false for anything that is not one of the six.
func segOpOfCompare(op OpCode) (segPredOp, bool) {
	switch op {
	case OpGt:
		return segGT, true
	case OpGe:
		return segGE, true
	case OpLt:
		return segLT, true
	case OpLe:
		return segLE, true
	case OpEq:
		return segEQ, true
	case OpNe:
		return segNE, true
	}
	return 0, false
}

// segPlanSoleAgg returns the one aggregate a whole-table plan computes, or nil
// when the plan is more than that: more than one scope, HAVING, ORDER BY, an
// anchor, DISTINCT, a separator. The magnet (min/max census) is left to
// callers (segPlanNoMagnet).
func segPlanSoleAgg(plan *aggPlan) *aggItem {
	if plan == nil || len(plan.scopes) != 1 || len(plan.outPlans) != 1 ||
		plan.havingPlan != nil || len(plan.orderPlans) != 0 ||
		plan.anchorCheck != nil {
		return nil
	}
	item := plan.outPlans[0]
	if item == nil || len(item.aggTemplates) != 1 || item.usesGroupBare || len(item.hoisted) != 0 {
		return nil
	}
	a := item.aggTemplates[0]
	if a == nil || a.distinct || a.sepExpr != nil {
		return nil
	}
	return a
}

// segPlanNoMagnet reports that no min()/max() census names a site. A site means
// a bare column may anchor to some particular row of the group, and none of
// these opcodes read rows -- so the fixed kernels refuse outright. The program
// recogniser is allowed one exception, for the census a min()/max() raises
// about ITSELF; see segProgPeephole.
func segPlanNoMagnet(plan *aggPlan) bool {
	return plan == nil || plan.magnet == nil || len(plan.magnet.sites) == 0
}

// segPlanIsPlainCountStar reports whether this aggregate is exactly
// "count(*)", whole-table, with nothing else attached: GROUP BY, HAVING, a
// second output, DISTINCT, a magnet or anchor, or a hoisted aggregate would
// each change the answer.
func segPlanIsPlainCountStar(plan *aggPlan) bool {
	a := segPlanSoleAgg(plan)
	return a != nil && segPlanNoMagnet(plan) && a.kind == aggCountStar && a.expr == nil
}

// segPlanPreds resolves each predicate's bound: a literal directly, a parameter
// from m.params. Reports false when a bound is of a class no kernel models,
// which sends the whole statement back to the loop the peephole left in place.
func (m *vdbe) segPlanPreds(plan *segFilterPlan) ([]segPred, bool) {
	preds := make([]segPred, 0, len(plan.preds))
	for _, p := range plan.preds {
		v := p.Lit
		if p.ParamIdx != 0 {
			if p.ParamIdx < 1 || p.ParamIdx > len(m.params) {
				return nil, false
			}
			v = m.params[p.ParamIdx-1]
		}
		// Everything but NULL. NULL declines: its three-valued logic is not
		// "orders below every value". A bound of the wrong class needs no
		// check: Int64Column answers only for PhysInt64 blocks and
		// Float64Column only for PhysFloat64, so an INTEGER column with a REAL
		// bound (or the reverse) takes the general path, which compares exactly
		// via compareIntFloat (sqlite3IntFloatCompare) -- the case where
		// coercing either side would round at |i| >= 2^53.
		if v.Typ == Null {
			return nil, false
		}
		// THE COMPARISON'S AFFINITY APPLIES TO THE BOUND, exactly as the loop this
		// replaces applies it (the affinity block of the comparison opcode,
		// vdbe.go, porting OP_Lt's at vdbe.c:2349-2380): "x = '1'" over an INTEGER
		// column compares 1, not '1'. Only the COLUMN side is a no-op here -- a
		// filterable column carries no exception, so every value already has the
		// affinity's type (segPeepholeCompareOK) -- and treating the bound the
		// same way was a wrong answer: count(*) WHERE x = '1' reported 0 rows
		// where C reports 8, and WHERE x < '1' reported every row.
		if p.Aff != affNone && !affinityIsIdentity(v.Typ, p.Aff) {
			v = applyAffinityToValue(v, p.Aff)
		}
		preds = append(preds, segPred{Col: p.Col, Op: p.Op, Val: v})
	}
	return preds, true
}

// ProgramUsesSegFilterForTest reports whether stmt compiles to a program the
// peephole rewrote -- that is, whether it will be answered by the JITted filter
// rather than by the VDBE's loop.
//
// Exported for the differential harness, which is a separate module and cannot
// see OpSegFilterCount or compileSelectScan. It exists so a benchmark can
// ASSERT it is timing the fast path: an arm labelled "columnar" that quietly
// ran the loop is the failure mode that makes a performance claim meaningless,
// and this repo has already published one such number.
func ProgramUsesSegFilterForTest(rp *ReadOnlyPager, stmt *SelectStmt) (bool, error) {
	prog, err := compileSelectScan(rp, stmt, nil)
	if err != nil {
		return false, err
	}
	for _, in := range prog.Insns {
		if in.Op == OpSegFilterCount {
			return true, nil
		}
	}
	return false, nil
}

// Counters for whether OpSegFilterCount actually SERVED a statement or declined
// to the loop the peephole left behind.
//
// They exist because "the opcode is in the program" and "the fast path answered
// the query" are different claims, and a benchmark that checks the first while
// reporting the second is measuring the VDBE and calling it a JIT. That is not
// hypothetical: this repo published a columnar number that was the b-tree's,
// and then a second one where both arms of a comparison ran the same path.
//
// Incremented once per statement, not per row.
var segFilterServed, segFilterDeclined atomic.Int64

// SegFilterCountersForTest returns how many statements the JITted filter has
// answered and how many it has handed back to the loop, since the last reset.
func SegFilterCountersForTest() (served, declined int64) {
	return segFilterServed.Load(), segFilterDeclined.Load()
}

// ResetSegFilterCountersForTest zeroes them.
func ResetSegFilterCountersForTest() {
	segFilterServed.Store(0)
	segFilterDeclined.Store(0)
}

// segIsScanPrologue reports whether op may appear between the OpenRead and the
// Rewind of a shape this recogniser answers, without changing which rows
// satisfy the WHERE. See segPeephole for the argument for each one.
func segIsScanPrologue(op OpCode) bool {
	switch op {
	case OpAutoIndexOrder, OpSeekIndexHint, OpSeekRowidHint, OpVariable, OpInteger:
		return true
	}
	return false
}

// segPlanIsPlainSum is segPlanIsPlainCountStar for sum(<expr>): one aggregate,
// no DISTINCT, no separator, no GROUP BY, no min()/max() anchor.
func segPlanIsPlainSum(plan *aggPlan) bool {
	a := segPlanSoleAgg(plan)
	return a != nil && segPlanNoMagnet(plan) && a.kind == aggSum && a.expr != nil
}

// segAggOutputsAreBare reports whether every output item is a bare aggregate
// (the groupAggExpr placeholder rewriteGroupExpr substitutes) with nothing
// around it. OpAggResult evaluates every output item into the result
// registers, so for "SELECT count(*) + 1" a recogniser answering only the
// aggregate would publish 5 where the answer is 6.
func segAggOutputsAreBare(plan *aggPlan) bool {
	if plan == nil || len(plan.outPlans) == 0 {
		return false
	}
	for _, it := range plan.outPlans {
		if it == nil || len(it.aggTemplates) != 1 {
			return false
		}
		g, ok := it.rewritten.(groupAggExpr)
		if !ok || g.accIdx != 0 {
			return false
		}
	}
	return true
}

// segAggPlanOf pulls the plan out of an OpAggResult's payload.
func segAggPlanOf(in Instruction) *aggPlan {
	info, ok := in.P4.(*aggResultInfo)
	if !ok {
		return nil
	}
	return info.plan
}

// segAndTreeTail checks the code an AND inside one expression ends with, and
// returns the address after it. For tests whose results sit in regs:
//
//	IsNull  reg_i -> N        once per test, in order
//	Integer 1 -> R
//	Goto    J
//	N: Null -> R
//	Goto    J
//	F: Integer 0 -> R         every failed test jumped here
//	J: IfNot R -> next (P3=1)
//
// With NULL-free columns (segPredColsFilterable) and non-NULL bounds
// (segPlanPreds) no test can produce NULL, so the IsNull arm is never taken and
// R is exactly the AND of the tests: what the kernel computes.
func segAndTreeTail(in []Instruction, pc int, regs []int, falseAt, nextAt int) (int, bool) {
	n := len(regs)
	if pc+n+6 > len(in) {
		return 0, false
	}
	nullAt := pc + n + 2
	for i, r := range regs {
		if in[pc+i].Op != OpIsNull || in[pc+i].P1 != r || in[pc+i].P2 != nullAt {
			return 0, false
		}
	}
	t := pc + n
	one, j1, null, j2, zero, test := in[t], in[t+1], in[t+2], in[t+3], in[t+4], in[t+5]
	r, join := one.P2, t+5
	if one.Op != OpInteger || one.P1 != 1 ||
		j1.Op != OpGoto || j1.P2 != join ||
		null.Op != OpNull || null.P2 != r ||
		j2.Op != OpGoto || j2.P2 != join ||
		zero.Op != OpInteger || zero.P1 != 0 || zero.P2 != r || t+4 != falseAt ||
		test.Op != OpIfNot || test.P1 != r || test.P2 != nextAt || test.P3 != 1 {
		return 0, false
	}
	return t + 6, true
}
