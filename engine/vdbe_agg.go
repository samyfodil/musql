// Runtime side of the aggregate / GROUP BY opcodes (OpAggReset/OpAggStep/
// OpAggResult/OpGroupSame/OpRecCopy, vdbe_op.go). No aggregate math lives here;
// the opcodes drive shared machinery:
//
//   - aggItem.step / aggItem.finalize (sql_agg.go): per-row accumulation and
//     final results, DISTINCT and overflow/empty-input rules included;
//   - an itemPlan's rewritten tree (sql_group.go), compiled against registers
//     holding the finalized accumulators, group key and anchor row
//     (compileAggItemProgram), as C resolves AggInfoFuncReg/AggInfoColumnReg
//     with no code (expr.c:5342, 4996). An item that does not lower fails the
//     statement at compile time (aggItemLowerable);
//   - cloneAggTemplates: a fresh accumulator set per group;
//   - keysEqual: grouping equality (compareValues, NULL==NULL). The key is
//     computed by the compiled scan body and read back from the sorter record
//     (select.c:8657's OP_Column).
//
// So plain, GROUP BY and window aggregates step the same accumulators. The
// compiler is compileScanAggregate / compileScanGroupBy (vdbe_agg_codegen.go).
package engine

import "fmt"

// aggPlan is an OpAggReset/OpAggStep P4 payload: what the aggregate opcodes share
// across one query's groups. The offset-adjusted table scopes, the total column
// count (the [cols.., rowids..] layout every row source packs, one rowid per
// scope), the cursor backing each scope (OpAggStep's cursor-source mode), and the
// rewritten item plans for the select list / HAVING / ORDER BY with their
// accumulator templates. Built from planNoGroupAggregate / planGroupByStmt.
//
// It carries no GROUP BY key expressions: the scan body evaluates keys
// (compileScanGroupBy / compileScanGroupByHash) and the drain reads them from the
// sorter record (select.c:8657).
type aggPlan struct {
	scopes     []tableScope
	nCols      int         // total column count across every joined table -- the [cols..] block width; len(scopes) rowid slots immediately follow it in the [cols.., rowids..] layout (see aggRowSplit)
	cursors    []int       // parallel to scopes: the cursor number backing each scope, for the ROWID half of gatherCursorRow
	// gathers is one entry per PHYSICAL row source, which is NOT the same as
	// one per scope: a materialized parenthesized join GROUP is several leaf
	// SCOPES (one per member, for name resolution) sharing ONE cursor whose
	// row is the whole group's columns concatenated. Copying that row once per
	// member, each at the member's own offset, would write past the group's
	// extent and misplace every value after the first -- so the row copy is
	// driven by this list and the rowids by cursors/scopes above.
	gathers []aggGather
	outPlans   []*itemPlan // one per select-list output column
	havingPlan *itemPlan   // nil when there is no HAVING clause
	orderPlans []orderPlan // GROUP BY ORDER BY terms; empty for whole-table or unordered

	// magnet is the query's min()/max() census and its S flag (magnetPlan,
	// sql_group.go). OpAggReset clones its sites per group into
	// aggAccumulators.magnets; they exist only to decide which row of the
	// group bare columns anchor to (see aggAccumulators.magnetWalk).
	magnet *magnetPlan

	// anchorCheck arms the run-time anchor certificate (anchor_certificate.go)
	// where the compiler could not prove the row order; nil otherwise.
	anchorCheck *anchorCheck
}

// aggRowSplit splits a [cols.., rowids.., ...] record (OpAggStep's P3==1
// mode) into its column-values row and its per-table rowid slice,
// generalizing the single-table [col0..colN-1, rowid] layout to one rowid
// slot per joined table. Anything after that block -- the sorted GROUP BY
// drain's trailing scan-order sequence number and group-key columns
// (compileScanGroupBy, vdbe_agg_codegen.go) -- is read by the program's own
// OpRecordColumns and is none of this function's business.
func aggRowSplit(plan *aggPlan, rec []Value) (row []Value, rowids []Value) {
	row = rec[:plan.nCols]
	clearJSONSubtype(row)
	return row, rec[plan.nCols : plan.nCols+len(plan.scopes)]
}

// clearJSONSubtype drops the function subtype (Value.Subtype) from every value
// in vals, applied to rows entering a GROUP BY's aggregate machinery. C passes a
// grouped query's rows through a sorter record, which loses subtypes, so every
// row-sourced value in a grouped query loses it (bare column, key, aggregate
// argument); a whole-table aggregate keeps it. json_each's value is the only row
// source here with a subtype:
//
//	json_quote(value) FROM json_each('[[7]]')                  [7]     kept
//	json_quote(value) FROM json_each('[[7]]') GROUP BY key     "[7]"   lost
//	json_quote(min(value)) FROM json_each(...) GROUP BY key    "[7]"   lost
//	json_group_array(value) FROM json_each(...) GROUP BY key   ["[7]"] lost
//	json_quote(max(value)) FROM json_each('[[7],[8]]')         [8]     kept
//
// A subtype produced by the item's own program after grouping survives
// ("json_quote(json_array(a)) ... GROUP BY a" is [1]).
func clearJSONSubtype(vals []Value) {
	for i := range vals {
		vals[i].Subtype = 0
	}
}

// aggGather names one physical cursor whose row lands at offset in the
// [cols..] block and is n columns wide. See aggPlan.gathers.
type aggGather struct {
	cursor int
	offset int
	n      int
}

// gatherCursorRow builds OpAggStep's P3==0 row (a whole-table aggregate, with
// no sorter record) and per-table rowids by reading each cursor directly,
// placing each table's columns at its scope's offset. A LEFT-joined cursor in
// the NullRow state contributes NULL columns and a NULL rowid.
func (m *vdbe) gatherCursorRow(plan *aggPlan) (row []Value, rowids []Value, err error) {
	// Reused across rows rather than allocated per row: over a 100k-row scan
	// these two were 22.5% of ALL bytes allocated, and the GC time to rescan
	// that garbage was a third of the query's CPU. Safe because the pair lives
	// exactly as long as the aggStep call below it -- aggStep cloneValues()
	// anything it keeps (the group's anchor row) -- and because
	// gatherCursorRow is reachable only from OpAggStep
	// and hashAggStep, never re-entrantly from expression evaluation.
	row = growValues(&m.aggRowBuf, plan.nCols)
	rowids = growValues(&m.aggRowidBuf, len(plan.scopes))
	// The ROW, once per physical cursor.
	for _, g := range plan.gathers {
		cur := m.cursors[g.cursor]
		vals, verr := cur.fullRow()
		if verr != nil {
			return nil, nil, verr
		}
		if n := len(vals); n > g.n {
			vals = vals[:g.n] // a cursor may carry more than this source exposes
		}
		copy(row[g.offset:g.offset+len(vals)], vals)
	}
	// The ROWIDS, once per SCOPE, since that is what evalCtx.rowids is indexed
	// by. A scope with no rowid pseudo-column of its own -- a derived table, a
	// view, and every member of a materialized join GROUP -- has none to
	// publish, so it is NULL rather than the backing cursor's.
	for i, curIdx := range plan.cursors {
		if plan.scopes[i].noRowid {
			rowids[i] = Value{Typ: Null}
			continue
		}
		cur := m.cursors[curIdx]
		if cur.rowidNull {
			rowids[i] = Value{Typ: Null}
		} else {
			rowids[i] = Value{Typ: Int, I: int64(cur.rowid)}
		}
	}
	return row, rowids, nil
}

// growValues returns buf re-sliced to n, reallocating only when it is too
// small, and zeroes it so a reused buffer never leaks a previous row's values
// into a column this row does not write (gatherCursorRow copies only the
// columns each cursor actually provides).
func growValues(buf *[]Value, n int) []Value {
	if cap(*buf) < n {
		*buf = make([]Value, n)
	}
	out := (*buf)[:n]
	for i := range out {
		out[i] = Value{}
	}
	return out
}

// The three item sets an aggResultInfo can select, matching aggAccumulators'
// three parallel accumulator slices.
const (
	aggSetOut    = iota // a select-list output column (plan.outPlans[idx])
	aggSetHaving        // the HAVING expression (plan.havingPlan; idx ignored)
	aggSetOrder         // a GROUP BY ORDER BY term (plan.orderPlans[idx].item)
)

// aggResultInfo is an OpAggResult P4 payload: which single item (select-list
// column, HAVING, or ORDER BY term) to finalize+evaluate for the current
// group.
type aggResultInfo struct {
	plan *aggPlan
	set  int
	idx  int
}

// aggAccumulators is the VM's live per-group accumulator state (vdbe.aggAccs):
// one instantiated *aggItem set per select-list item, plus the HAVING set and
// one set per ORDER BY expression term -- cloned fresh from the plan's
// templates by OpAggReset and stepped over the group's rows by OpAggStep.
type aggAccumulators struct {
	out    [][]*aggItem
	having []*aggItem
	order  [][]*aggItem

	// magnets are this group's own instances of the query's min()/max() census
	// sites (aggPlan.magnet). Deliberately SEPARATE accumulators from whatever
	// clones those same calls' output items already get, so running the census
	// can never perturb an aggregate's value.
	magnets []*aggItem

	// anchorVals/anchorRowids snapshot the LAST row on which magnetWalk left
	// the magnet clear -- the group's anchor for bare (non-grouped,
	// non-aggregated) columns and for a correlated subquery in the same item.
	// Copied, because the row/rowid slices the step opcodes hand in are reused
	// across rows (see aggRowCtx).
	anchorVals   []Value
	anchorRowids []Value
	haveAnchor   bool

	// magnetHit is M, SQLite's regHit. It is a REGISTER, so it survives from
	// one scanned row to the next: a row on which every census site is jumped
	// over inherits the last verdict written, which is exactly how a run of
	// DISTINCT duplicates after a first winner keeps the anchor pinned to that
	// winner. Resetting it per row is a wrong answer -- caught by
	// TestR26AnchorFuzz over y = 0,NULL,0,NULL,0 and "max(DISTINCT y)", where
	// rows 3-5 are all duplicates and must NOT re-anchor.
	//
	// seenRow is S, SQLite's regAcc/iUseFlag: false on the first row of the
	// group, true afterwards. magnetS is aggPlan.magnet.allocS, copied here so
	// magnetWalk needs nothing but the row's ctx. See magnetWalk.
	magnetHit bool
	seenRow   bool
	magnetS   bool

	// check, checkVals, checkSeen and anchorRisk are the anchor certificate's
	// per-group state (anchor_certificate.go): the slots to compare, the first
	// row's values, and whether two rows have disagreed.
	check      *anchorCheck
	checkVals  []Value
	checkSeen  bool
	anchorRisk bool
}

// magnetWalk runs the query's min()/max() census over one row and reports
// whether the row becomes the group's anchor, reproducing updateAccumulator's
// register dance around the conditional copy of the row's columns
// (select.c, c:155809-155950).
//
// M is the magnet (regHit), starting clear; each census site may set it:
//
//	site has a FILTER and S is allocated -> M := S    (OP_Copy regAcc,regHit)
//	site is skipped (FILTER rejects the row, or its DISTINCT already saw this
//	  argument)                          -> M untouched: the PREVIOUS site's
//	                                        verdict stands
//	otherwise                            -> M := 0, step, M := 1 iff the
//	                                        accumulator did not change
//	                                        (OP_CollSeq then OP_AggStep --
//	                                        aggItem.magnetStep, sql_agg.go)
//
// An empty census leaves M := S: with no min/max only the group's first row
// (S == false) anchors. After the walk, M == false makes this row the anchor.
// C never resets M per group; resetting here is equivalent, since on a group's
// first row S is false and every site copies S or clears M (a DISTINCT site
// cannot skip on a first row: its dedup table is reopened per group).
//
// Over t0(a,b) = (3,5),(1,7),(2,9):
//
//	SELECT count(*), a, b FROM t0 HAVING min(a)>0 ORDER BY max(b)  -> 3|1|7
//	SELECT max(b), min(a) FILTER (WHERE b<9), a, b FROM t0         -> 9|1|2|9
//	   ... the same with "GROUP BY 1=1" appended                   -> 9|1|1|7
//	SELECT max(DISTINCT b), a, b FROM td3(1,5),(2,9),(3,9)         -> 9|3|9
func (a *aggAccumulators) magnetWalk(ctx *evalCtx, regs []Value) error {
	a.certifyAnchor(ctx)
	if len(a.magnets) == 0 {
		a.magnetHit = a.seenRow
	}
	for _, site := range a.magnets {
		if site.filter != nil && a.magnetS {
			a.magnetHit = a.seenRow
		}
		touched, siteHit, err := site.magnetStep(ctx, regs)
		if err != nil {
			return err
		}
		if touched {
			a.magnetHit = siteHit
		}
	}
	a.seenRow = true
	if a.magnetHit {
		return nil
	}
	a.anchorVals = cloneValues(ctx.vals)
	a.anchorRowids = cloneValues(ctx.rowids)
	a.haveAnchor = true
	return nil
}

// anchorRow returns the row a groupBareColExpr placeholder (or a correlated
// subquery in the item) reads for the current group: the row magnetWalk last
// accepted. A whole-table aggregate whose scan stepped no rows still emits one
// row, and C reads every column in it as NULL ("SELECT max(b), c FROM t1" over an
// empty t1 is (NULL, NULL); "count(*), c" is (0, NULL)), so the anchor is then an
// all-NULL row of plan's width. A GROUP BY group is never empty.
func (a *aggAccumulators) anchorRow(plan *aggPlan) (vals, rowids []Value) {
	if !a.haveAnchor {
		return make([]Value, plan.nCols), make([]Value, len(plan.scopes))
	}
	return a.anchorVals, a.anchorRowids
}

// newAggAccs clones a fresh accumulator set from plan's item templates
// (cloneAggTemplates) for a new group.
func newAggAccs(plan *aggPlan) *aggAccumulators {
	a := &aggAccumulators{out: make([][]*aggItem, len(plan.outPlans)), check: plan.anchorCheck}
	for i, it := range plan.outPlans {
		a.out[i] = cloneAggTemplates(it.aggTemplates)
	}
	if plan.havingPlan != nil {
		a.having = cloneAggTemplates(plan.havingPlan.aggTemplates)
	}
	if plan.magnet != nil {
		a.magnets = cloneAggTemplates(plan.magnet.sites)
		a.magnetS = plan.magnet.allocS
	}
	a.order = make([][]*aggItem, len(plan.orderPlans))
	for i, op := range plan.orderPlans {
		if op.item != nil {
			a.order[i] = cloneAggTemplates(op.item.aggTemplates)
		}
	}
	return a
}

// aggRowCtx builds the evalCtx an aggregate opcode uses for a row: the query's
// scopes, the row's concatenated values, one rowid per table (NULL for a LEFT
// JOIN's NULL-extended table), and the outer/pager/params plumbing. It is reused
// across rows (m.aggCtx), allocated on first use (allocating per row was 38.7% of
// bytes over 100k rows; inlining it bloated every machine). Safe: nothing retains
// it (aggStep clones the row images it keeps), and only the OpAggStep /
// hashAggStep / key opcode bodies call it, never re-entrantly.
func (m *vdbe) aggRowCtx(plan *aggPlan, row []Value, rowids []Value) *evalCtx {
	if m.aggCtx == nil {
		m.aggCtx = new(evalCtx)
	}
	*m.aggCtx = evalCtx{tables: plan.scopes, vals: row, rowids: rowids, outer: m.outer, pager: m.pager, params: m.params}
	return m.aggCtx
}

// aggStep advances every accumulator in the select list, HAVING and ORDER BY
// sets over one row (aggItem.step). m.regs rides along so an accumulator reads
// the argument register the scan body computed (aggItem.rowValue/rowRegs;
// planAggArgRegs); the sorted drain fills those registers from the sorter record
// (aggDrainRow). A missing register is an internal error, not a fallback.
//
// seg is the step segment this opcode owns (aggItem.stepSeg): only its
// accumulators advance. C codes one OP_AggStep per aggregate after that
// aggregate's arguments (select.c:6824, 6906, 6942), so a later aggregate's
// argument is never evaluated before an earlier one's step; a plan with nothing
// to separate has one segment.
//
// magnetWalk runs with segment 0, once per row, as the census is per row in C.
// Running it per segment would change no answer (it is idempotent within a row:
// nothing improves on a value just accepted), but that is a property of the
// magnetStep bodies, not of this function.
func (m *vdbe) aggStep(ctx *evalCtx, seg int) error {
	// Anchor bookkeeping for bare columns (see aggAccumulators.magnetWalk).
	// C SQLite runs this BEFORE it loads the accumulator columns and after
	// every aggregate has stepped; the order relative to the item accumulators
	// below cannot matter, since the census sites are separate accumulators
	// that share no state with them.
	if seg == 0 {
		if err := m.aggAccs.magnetWalk(ctx, m.regs); err != nil {
			return err
		}
	}
	for _, accs := range m.aggAccs.out {
		if err := stepAccs(accs, ctx, m.regs, seg); err != nil {
			return err
		}
	}
	if err := stepAccs(m.aggAccs.having, ctx, m.regs, seg); err != nil {
		return err
	}
	for _, accs := range m.aggAccs.order {
		if err := stepAccs(accs, ctx, m.regs, seg); err != nil {
			return err
		}
	}
	return nil
}

// stepAccs steps every accumulator in one item's set that belongs to step
// segment seg over ctx's row. The rest belong to another of this row's step
// opcodes and are skipped here -- see aggStep.
func stepAccs(accs []*aggItem, ctx *evalCtx, regs []Value, seg int) error {
	for _, acc := range accs {
		if acc.stepSeg != seg {
			continue
		}
		if err := acc.step(ctx, regs); err != nil {
			return err
		}
	}
	return nil
}

// aggResult finalizes the selected item's accumulators and evaluates its
// rewritten expression tree for the current group -- select.c's own pair,
// finalizeAggFunctions (:6733, one OP_AggFinal per aggregate into
// AggInfoFuncReg) followed by ordinary expression coding over the registers it
// wrote (:8746-8748 for the sorted GROUP BY drain, :8905-8911 for a whole-table
// aggregate).
//
// The tree is RUN, never walked: the item's program reads the three
// placeholders out of a register block this function seeds (aggItemProgram,
// vdbe_agg_item_codegen.go), and an item that did not lower never got this
// far -- compileAggItemPrograms refused the statement at COMPILE time.
func (m *vdbe) aggResult(info *aggResultInfo, groupKey []Value) (Value, error) {
	plan := info.plan
	if m.aggAccs != nil && m.aggAccs.anchorRisk {
		return Value{}, fmt.Errorf("%w: a bare column in an aggregate query whose row order is not provably C SQLite's, and whose value differs between rows of one group, so which row is the anchor decides the answer", errVDBEUnsupported)
	}
	var it *itemPlan
	var accs []*aggItem
	switch info.set {
	case aggSetHaving:
		it, accs = plan.havingPlan, m.aggAccs.having
	case aggSetOrder:
		it, accs = plan.orderPlans[info.idx].item, m.aggAccs.order[info.idx]
	default: // aggSetOut
		it, accs = plan.outPlans[info.idx], m.aggAccs.out[info.idx]
	}
	aggVals := make([]Value, len(accs))
	for i, acc := range accs {
		v, err := acc.finalize()
		if err != nil {
			return Value{}, err
		}
		aggVals[i] = v
	}
	// A GROUP BY key is row-sourced, so it loses the JSON subtype too
	// (clearJSONSubtype); the hash-aggregate path builds its key record
	// separately from the row (OpHashAggStep's P1), so it is cleared here.
	if groupKey != nil {
		clearJSONSubtype(groupKey)
	}
	bareVals, bareRowids := m.aggAccs.anchorRow(plan)
	// The compiled item: run seeds the three register blocks and the
	// program reads them, C's answer for the same nodes (AggInfoFuncReg,
	// expr.c:5342; AggInfoColumnReg, expr.c:4996; the key via OP_Column,
	// select.c:8657). A subquery in the item reads the same blocks across
	// the frame (OpOuterAggReg, rewriteAggItemBodies).
	//
	// Two former declines, recorded because the reasoning matters:
	//
	//   - aggnested.test's "SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t1
	//     GROUP BY c3 HAVING (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT
	//     sum(DISTINCT c1))))" does not hoist sum(DISTINCT c1) into t0's
	//     WHERE: SF_Aggregate comes from the result set alone
	//     (resolve.c:1978-1985, before WHERE at :2005), and only pEList/ORDER
	//     BY/HAVING are analyzed (select.c:8432-8442), so that call would be
	//     "misuse of aggregate" (expr.c:5333-5344). It answers only because
	//     countOfViewOptimization (select.c:7137) deletes each arm's select
	//     list first (count_of_view.go).
	//   - "SELECT k, avg(v) AS av FROM t GROUP BY k HAVING EXISTS (SELECT 1
	//     WHERE av>3)" was blocked by outerAliasAggCall rejecting an
	//     unqualified argument in resolveAlias's copy of the aliased call
	//     (resolve.c:69-101); it answers (1,4.0).
	//
	// An item that does not lower fails at compile time, so this check only
	// guards per-execution widths.
	if p := it.prog; p.runnable(m, aggVals, groupKey, bareVals, bareRowids) {
		return p.run(m, aggVals, groupKey, bareVals, bareRowids)
	}
	return Value{}, fmt.Errorf("%w: aggregate result item's register block does not match its program", errVDBEUnsupported)
}

// aggPlanReadsOnlyLoweredCols reports whether every value this plan consumes
// arrives through a lowered register, so a caller with no cursor (the columnar
// GROUP BY driver, segment_group.go) can hand over an empty row. Excluded: a
// min()/max() anchor and usesGroupBare (a bare column read from the gathered
// row), and hoisted aggregates (evaluated from the row). HAVING and ORDER BY
// programs are excluded outright, since they may read the anchor row.
func aggPlanReadsOnlyLoweredCols(plan *aggPlan) bool {
	return aggPlanReadsOnlyLoweredColsExceptOrder(plan) && len(plan.orderPlans) == 0
}

// havingReadsNoRow reports whether a rewritten HAVING is built only of
// aggregate and group-key placeholders, literals, parameters and operators over
// them. Anything else -- a subquery, which can read the outer row, or a node
// this list does not know -- answers false.
func havingReadsNoRow(e Expr) bool {
	all := func(es ...Expr) bool {
		for _, x := range es {
			if x != nil && !havingReadsNoRow(x) {
				return false
			}
		}
		return true
	}
	switch x := e.(type) {
	case groupAggExpr, groupKeyExpr, LiteralExpr, ParamExpr:
		return true
	case UnaryExpr:
		return all(x.X)
	case BinaryExpr:
		return all(x.L, x.R)
	case IsNullExpr:
		return all(x.X)
	case BetweenExpr:
		return all(x.X, x.Lo, x.Hi)
	case CastExpr:
		return all(x.X)
	case CollateExpr:
		return all(x.X)
	case InExpr:
		return x.Sub == nil && all(x.X) && all(x.List...)
	}
	return false
}

// aggPlanReadsOnlyLoweredColsExceptOrder is the same test without the ORDER BY
// exclusion, for the columnar GROUP BY recognizer, which accepts only an
// OpHashAggSort drain, emitted only when ORDER BY is the group keys ascending
// (orderIsGroupKeyAscending), so no order expression is evaluated. Without it
// "... GROUP BY k ORDER BY k" declined while the same query without ORDER BY was
// served.
func aggPlanReadsOnlyLoweredColsExceptOrder(plan *aggPlan) bool {
	if plan == nil {
		return false
	}
	for _, item := range plan.outPlans {
		if item == nil || item.usesGroupBare || len(item.hoisted) != 0 {
			return false
		}
	}
	// A HAVING is the same test plus havingReadsNoRow: its aggregates are
	// stepped from lowered registers like the outputs' (aggPlanStepOrder lists
	// them), and over aggregates, group keys and constants it reads no row.
	if hp := plan.havingPlan; hp != nil && (hp.usesGroupBare || len(hp.hoisted) != 0 || !havingReadsNoRow(hp.rewritten)) {
		return false
	}
	// A min()/max() census does not disqualify the plan once the checks
	// above pass: a site exists so a bare column can read the anchor row,
	// and usesGroupBare and hoisted are false, so nothing reads it. The
	// extreme itself comes from the argument register the columnar driver
	// fills (segGroupPlan.argRegs). Excluding it cost "SELECT k, min(v),
	// max(v) FROM t GROUP BY k ORDER BY k" the columnar path (129ms vs C's
	// 86ms). Each site must still be this statement's min/max with no
	// FILTER, since a filtered site steps a different subset of rows.
	if plan.magnet != nil {
		for _, st := range plan.magnet.sites {
			if st == nil || st.filter != nil || (st.kind != aggMin && st.kind != aggMax) {
				return false
			}
		}
	}
	return true
}
