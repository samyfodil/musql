package engine

import (
	"sync"

	"github.com/samyfodil/musql/internal/jit"
)

// The general recogniser compiles whatever the loop body contains. This lowers
// the body to the program JIT's IR and runs machine code compiled for it.
// Tried after the vector recogniser, so fixed-shape kernels are preserved.

// segProgPlan is what OpSegProgram carries: one lowered program per aggregate
// the statement computes, in output-column order.
type segProgPlan struct {
	// lows is the programs over INTEGER columns and parameters, nil when the
	// loop lowers only where some are REAL.
	lows []*segLowered
	src  *segProgSource
	// variants are the programs for each combination of REAL columns and
	// parameters a run has met: segKinds.key() -> []*segLowered, nil when
	// the loop does not lower for it.
	variants sync.Map
}

// segProgSource is the loop a plan lowers, kept for the lowerings a run over
// REAL columns asks for.
type segProgSource struct {
	body                []Instruction
	base, cursor, aggAt int
	aggs                []*aggItem
	cols                []int // the table columns the body loads
	params              []int // the parameters it reads
	// series is a loop over generate_series (segment_series.go), whose one
	// INTEGER column comes in blocks, so a body reading nothing still has rows
	// to count.
	series bool
}

// forKinds is the plan's programs for kinds, lowered on first use.
func (p *segProgPlan) forKinds(kinds *segKinds) []*segLowered {
	if kinds == nil {
		return p.lows
	}
	key := kinds.key()
	if v, ok := p.variants.Load(key); ok {
		return v.([]*segLowered)
	}
	lows, _ := p.src.lower(kinds)
	v, _ := p.variants.LoadOrStore(key, lows)
	return v.([]*segLowered)
}

// segProgPlanFor is the plan for a loop whose body feeds the AggStep at
// aggAt, or nil when its aggregates are not ones a program computes or the
// body lowers for no kinds of column.
func segProgPlanFor(body []Instruction, base, cursor, aggAt int, plan *aggPlan, nAgg int, series bool) *segProgPlan {
	aggs := segPlanAggs(plan, nAgg)
	if aggs == nil {
		return nil
	}
	// A FILTER is per aggregate, and the lowered body is shared: its jump
	// past the filtered aggregate's argument reads, compiled, as a row filter
	// for every aggregate in the step. "SELECT sum(abs(y)) FILTER (WHERE 0),
	// count(*) FROM t" counted no rows (C: 1). The loop answers instead.
	for _, a := range aggs {
		if a.filter != nil {
			return nil
		}
	}
	// The MAGNET is admissible only when every site is one of THESE
	// aggregates' own min()/max() census. A site means some column anchors to a
	// particular row, and this program has nothing to anchor: the tail above is
	// AggResult straight into ResultRow, so the statement's whole answer IS the
	// aggregates. "SELECT k, max(v) FROM t" -- where k really does come from the
	// max row -- fails segPlanAggs' bare-column check before reaching here.
	if !segPlanNoMagnet(plan) {
		sites := plan.magnet.sites
		minMax := 0
		for _, a := range aggs {
			if a.kind == aggMin || a.kind == aggMax {
				minMax++
			}
		}
		if len(sites) != minMax {
			return nil
		}
		for _, st := range sites {
			if st == nil || st.filter != nil || (st.kind != aggMin && st.kind != aggMax) {
				return nil
			}
		}
	}
	src := &segProgSource{body: body, base: base, cursor: cursor, aggAt: aggAt, aggs: aggs, series: series}
	seen := map[int]bool{}
	for _, in := range body {
		switch {
		case in.Op == OpColumn && in.P1 == cursor && !seen[in.P2]:
			seen[in.P2] = true
			src.cols = append(src.cols, in.P2)
		case in.Op == OpVariable:
			src.params = append(src.params, in.P1)
		}
	}
	lows, ok := src.lower(nil)
	if !ok && series {
		return nil
	}
	if !ok {
		// Lowering may need REAL columns: a REAL literal compared with one.
		// Which columns are REAL is for the segments to say when it runs.
		all := &segKinds{realCol: map[int]bool{}}
		for _, c := range src.cols {
			all.realCol[c] = true
		}
		if _, okReal := src.lower(all); !okReal {
			return nil
		}
		lows = nil
	}
	return &segProgPlan{lows: lows, src: src}
}

// lower builds one program per aggregate for kinds.
func (src *segProgSource) lower(kinds *segKinds) ([]*segLowered, bool) {
	// ONE lowered body, shared. The aggregates differ only in the accumulate at
	// the end, so the predicate and the column reads are compiled once and each
	// aggregate gets a copy with its own terminal op. N aggregates are N passes
	// over the same int64 blocks, which is the trade segRunProgramAll explains.
	base, okLow := segLowerBody(src.body, src.base, src.cursor, src.aggAt, kinds)
	if !okLow || (len(base.cols) == 0 && len(base.blocks) == 0 && !src.series) {
		return nil, false
	}
	// Each aggregate's argument is read by the accumulate appended below,
	// outside the body: a service block that computes it must write it back.
	for _, a := range src.aggs {
		if a.expr != nil && a.rowRegs[aggExprArg] != 0 {
			base.readNatively(a.rowRegs[aggExprArg] - 1)
			if base.textVar != nil {
				base.textVar.readNatively(a.rowRegs[aggExprArg] - 1)
			}
		}
	}
	lows := make([]*segLowered, 0, len(src.aggs))
	for _, a := range src.aggs {
		argReg := 0
		switch a.kind {
		case aggCountStar:
			if a.expr != nil {
				return nil, false
			}
		case aggSum, aggTotal, aggAvg, aggCount, aggMin, aggMax:
			if a.expr == nil || a.rowRegs[aggExprArg] == 0 {
				return nil, false
			}
			argReg = a.rowRegs[aggExprArg] - 1
		default:
			return nil, false
		}
		low := base.clone()
		low.agg = a.kind
		rowsReg := low.nRegs
		low.nRegs++
		low.sumReg, low.rowsReg = argReg, rowsReg
		// A REAL argument, which may be NULL (a NaN), accumulates in FAcc.
		isReal := a.kind != aggCountStar && low.real[argReg]
		switch {
		case a.distinct && a.kind != aggMin && a.kind != aggMax:
			// The distinct values are found in Go from the arguments the
			// program hands back (segDistinctAnswer), as integers. A DISTINCT
			// min or max is the plain one.
			if isReal {
				return nil, false
			}
			low.nRegs--
			low.distinct = true
			low.add(jit.ProgInsn{Op: jit.POpEmitVal, A: argReg})
		case a.kind == aggCountStar || (a.kind == aggCount && !isReal):
			// count(x) is the row count: segCleanInt64Column refuses a column
			// with a NULL bitmap or an exception, so every value the program
			// sees is non-NULL and countStep (func.c:1970) steps on every one.
			low.nRegs--
			low.add(jit.ProgInsn{Op: jit.POpAccCount})
		case a.kind == aggCount:
			low.nRegs--
			low.add(jit.ProgInsn{Op: jit.POpFAccCount, A: argReg})
		case a.kind == aggMin && isReal:
			low.add(jit.ProgInsn{Op: jit.POpFAccMin, A: argReg, B: rowsReg})
		case a.kind == aggMax && isReal:
			low.add(jit.ProgInsn{Op: jit.POpFAccMax, A: argReg, B: rowsReg})
		case isReal:
			low.add(jit.ProgInsn{Op: jit.POpFAccSum, A: argReg, B: rowsReg})
		case a.kind == aggMin:
			low.add(jit.ProgInsn{Op: jit.POpAccMin, A: argReg, B: rowsReg})
		case a.kind == aggMax:
			low.add(jit.ProgInsn{Op: jit.POpAccMax, A: argReg, B: rowsReg})
		default:
			low.add(jit.ProgInsn{Op: jit.POpAccSum, A: argReg, B: rowsReg})
		}
		low.fAcc = isReal && a.kind != aggCount
		if tv := low.textVar; tv != nil {
			// The same accumulate, on the same registers, in the text variant;
			// its own row counter sits past its own registers.
			acc := low.insns[len(low.insns)-1]
			if a.kind != aggCountStar && a.kind != aggCount && !low.distinct {
				acc.B = tv.nRegs
				tv.nRegs++
				tv.sumReg, tv.rowsReg = low.sumReg, acc.B
			}
			tv.agg, tv.fAcc, tv.distinct = low.agg, low.fAcc, low.distinct
			tv.add(acc)
		}
		lows = append(lows, low)
	}
	return lows, true
}

// segPlanAggs returns the ONE aggregate each of the plan's n output columns
// computes, in order, or nil when any column is more than that.
//
// The per-item checks are segPlanSoleAgg's, applied to every item instead of
// insisting there be one: no bare group column and no hoist (both read the
// group's ANCHOR ROW, which these opcodes never touch), exactly one template,
// no separator. DISTINCT is allowed: segProgPeephole hands its values back to
// Go. The plan-wide checks -- one scope, no HAVING, no ORDER BY, no anchor --
// still have to hold once.
func segPlanAggs(plan *aggPlan, n int) []*aggItem {
	if plan == nil || len(plan.scopes) != 1 || len(plan.outPlans) != n ||
		plan.havingPlan != nil || len(plan.orderPlans) != 0 || plan.anchorCheck != nil {
		return nil
	}
	out := make([]*aggItem, 0, n)
	for _, item := range plan.outPlans {
		if item == nil || len(item.aggTemplates) != 1 || item.usesGroupBare || len(item.hoisted) != 0 {
			return nil
		}
		a := item.aggTemplates[0]
		if a == nil || a.sepExpr != nil {
			return nil
		}
		out = append(out, a)
	}
	return out
}

// segProgPeephole rewrites prog when its scan loop lowers to the program JIT.
func segProgPeephole(prog *Program) bool {
	if !jitEnabled || !jit.Available || prog == nil {
		return false
	}
	in := prog.Insns
	if len(in) < 9 || in[0].Op != OpInit || in[1].Op != OpAggReset {
		return false
	}
	series := segSeriesOpen(in[2])
	if in[2].Op != OpOpenRead && !series {
		return false
	}
	rewindAt := 3
	for rewindAt < len(in) && segIsScanPrologue(in[rewindAt].Op) {
		rewindAt++
	}
	if rewindAt >= len(in) || in[rewindAt].Op != OpRewind {
		return false
	}
	end := len(in)
	if in[end-1].Op != OpHalt || in[end-2].Op != OpResultRow {
		return false
	}
	// N consecutive AggResult, one per output column. ResultRow must read exactly that range.
	nAgg := 0
	for i := end - 3; i >= 0 && in[i].Op == OpAggResult; i-- {
		nAgg++
	}
	if nAgg == 0 || in[end-2].P2 != nAgg {
		return false
	}
	// Every output item must be its aggregate, not wrapped in an expression.
	if !segAggOutputsAreBare(segAggPlanOf(in[end-2-nAgg])) {
		return false
	}
	// Halt, ResultRow, then nAgg AggResults, then Close.
	closeAt := end - 3 - nAgg
	if closeAt < 0 || in[closeAt].Op != OpClose {
		return false
	}
	dstBase := in[closeAt+1].P1
	if in[end-2].P1 != dstBase {
		return false
	}
	for i := 0; i < nAgg; i++ {
		if in[closeAt+1+i].P1 != dstBase+i {
			return false
		}
	}
	if in[rewindAt].P2 != closeAt {
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
	cursor := in[rewindAt].P1
	if series && in[2].P1 != cursor {
		return false
	}
	pplan := segProgPlanFor(in[rewindAt+1:aggAt], rewindAt+1, cursor, aggAt, plan, nAgg, series)
	if pplan == nil {
		return false
	}
	// DISPLACE the Rewind; do not insert the guard.
	//
	// The other recognisers splice their guard in at a fixed index and shift
	// every jump target by one, which is safe only because their matchers accept
	// a fixed opcode list -- they know exactly which P2 fields are addresses.
	// This matcher accepts ANY body that lowers, so an opcode carrying a jump the
	// shift list had not been told about silently survives pointing one short.
	// That is not hypothetical: teaching the lowering about OpNotNull admitted
	// "k IN (1,2)", whose IN chain is built from them, and the unshifted targets
	// corrupted the FALLBACK loop -- so every pager without segments, which is
	// every ordinary connection, answered 0 for "count(*) WHERE k IN (1,2)"
	// where C SQLite answers 8. A silently empty filter is the worst answer this
	// engine can give, and it passed two corpus sweeps.
	//
	// So nothing moves. The Rewind -- the one instruction the matcher has proved
	// is a single slot nothing else jumps to -- becomes a Goto to a block
	// appended past the Halt, which runs the guard, then the displaced Rewind,
	// then a Goto back to the body. Every existing address stays exactly what it
	// was, including the ones this recogniser never learned to recognise.
	guardAt := len(in)
	out := make([]Instruction, len(in), len(in)+6)
	copy(out, in)
	out[rewindAt] = Instruction{Op: OpGoto, P2: guardAt}
	out = append(out,
		// guardAt: serve -> the emit tail; decline -> fall through.
		Instruction{Op: OpSegProgram, P1: dstBase, P2: cursor, P3: guardAt + 3,
			P4: pplan},
		in[rewindAt], // its P2 (the empty-table exit) still holds
		Instruction{Op: OpGoto, P2: rewindAt + 1}, // where the Rewind used to fall through
		// guardAt+3: the served tail. AggResult is deliberately NOT repeated --
		// it would overwrite dst with accumulators the loop never filled.
		in[closeAt],
		Instruction{Op: OpResultRow, P1: dstBase, P2: nAgg},
		Instruction{Op: OpHalt},
	)
	prog.Insns = out
	return true
}
