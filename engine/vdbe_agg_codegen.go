// Compiles the two aggregate shapes of a SELECT: a whole-table aggregate
// (compileScanAggregate) and GROUP BY (compileScanGroupBy), dispatched from
// compileSelectScan. Planning is planNoGroupAggregate / planGroupByStmt; the
// nested-loop skeleton is emitJoinLoops'; accumulation is the aggregate opcodes
// (vdbe_agg.go). Anything not handled faithfully is errVDBEUnsupported.
//
// Whole-table aggregate:
//
//	OpenRead(each); AggReset; [nested Rewind->end/Next->loop per table]
//	  WHERE?, AggStep(joined cursors); [/nested]; Close(each);
//	AggResult(out) x N; ResultRow; Halt
//
// GROUP BY via sorter (C's shape):
//
//	scan:  OpenRead(each); SorterOpen s1(key=groupExprs);
//	       [nested Rewind->scanEnd/Next->loop per table]
//	         WHERE?, compute groupExprs, read every table's columns + rowid,
//	         MakeRecord[keys..,cols..,rowids..,seq,groupVals..], SorterInsert s1;
//	       [/nested]; Close(each)
//	drain: SorterSort s1->done; loop2: SorterData->rowRec, RecordColumn x nGroup
//	       + MakeRecord->rowKeyRec,
//	       read scan-order sequence number; on a NEW group (GroupSame vs
//	       curKeyRec fails) EMIT the previous group; (re)AggReset + remember
//	       key/min-seq; AggStep(row); SorterNext->loop2; EMIT the final group;
//	       done: ...
//	EMIT: AggResult(out) x N -> outBase; HAVING? AggResult(having), skip if
//	      false/NULL; then either ResultRow (LIMIT/OFFSET-gated) for the
//	      unordered case, or MakeRecord[orderKeys..,minSeq,out..] +
//	      SorterInsert s2 for the ORDER BY case.
package engine

import (
	"fmt"
)

// compileScanAggregate compiles a whole-table (no GROUP BY) aggregate, possibly
// over a join. c already has every source's cursor and scope installed; this
// supplies emitJoinLoops' innermost body: OpAggStep over the row every source's
// cursor currently holds (gatherCursorRow).
func compileScanAggregate(c *compiler, stmt *SelectStmt, srcs []joinSource, scopes []tableScope) (*Program, error) {
	// The select list and HAVING run in the finalize phase, after sqlite3WhereEnd
	// restored nQueryLoop (where.c:7484), unlike a plain scan's select list. Since
	// WHERE and select-list positions are not tracked here, distrust nQueryLoop for
	// everything compiled beneath c (see compiler.nQueryLoopKnown). The saved values
	// keep the FROM planning's trust for withFromPlanTrust below.
	savedNQueryLoop, savedNQueryLoopKnown := c.nQueryLoop, c.nQueryLoopKnown
	c.nQueryLoopKnown = false
	// A whole-table aggregate yields exactly one row, so ORDER BY cannot reorder
	// anything, but its terms are still validated at prepare time
	// (validateNoGroupOrderByTerm). "*" cannot combine with an aggregate.
	//
	// HAVING filters the single implicit group: "SELECT count(*) FROM empty HAVING
	// count(*)=0" still answers one row, which is why this is not "GROUP BY
	// <constant>" (an empty table has zero groups).
	for _, sc := range stmt.Columns {
		if sc.Star {
			return nil, fmt.Errorf("%w: \"*\" with an aggregate", errVDBEUnsupported)
		}
	}

	// outer=nil: this is a plan-time (compile-time) call with no runtime
	// evalCtx available -- see planNoGroupAggregate's doc comment. This means
	// an aggregate argument that mixes a local column with a PURELY outer-
	// correlated one (planNoGroupAggregate's hasLocal relaxation) still
	// compiles fine as far as planNoGroupAggregate itself is concerned (that
	// check needs scopes alone, not outer) -- see bindAggOuterRefs just below
	// for which of those shapes it can reach and which it still declines.
	outCols, items, allAggs, magnet, usesGroupBare, perr := planNoGroupAggregate(stmt, scopes, nil, c.pager.colNameMode(), c.outer)
	if perr != nil {
		// An aggregate this query does not own belongs to an ENCLOSING one, and
		// is HOISTED there when that query can be found (vdbe_agg_hoist.go) --
		// the enclosing compile then restarts as an aggregate query and this one
		// is compiled again, this time with the call gone. Any error will do to
		// unwind: the retry keys off the recorded spec, not off this error.
		if ae, ok := asAggAssociationError(perr); ok && hoistAggregatesOutward(c.outer, stmt, ae) {
			return nil, fmt.Errorf("%w: aggregate belongs to an enclosing query", errVDBEUnsupported)
		}
		// A real planning error (correlated aggregate argument, unsupported/
		// wrong-arity aggregate, ...): surfaced as a compile decline, which
		// (there is no fallback) reaches the caller as an error.
		return nil, declineOrSemantic(perr)
	}
	// ...and the mirror image: aggregates SOME INNER query handed to this one
	// (see this compile's own hoisted list, installed by compileSelectScanRow's
	// retry). Each becomes an ordinary accumulator of this query, stepped over
	// this query's own rows, whose finalized value is substituted into the
	// subquery body at OpAggResult time.
	if len(c.hoisted) > 0 {
		hoistAggs, herr := planHoistedAggs(items, c.hoisted)
		if herr != nil {
			return nil, herr
		}
		allAggs = append(allAggs, hoistAggs...)
		// A hoisted min()/max() is one of THIS query's aggregates, so it is a
		// census site for the anchor row every bare column and correlated
		// subquery in the same item reads from -- and minMaxCensus could not
		// have seen it. See honoredWithHoisted for the wrong answer this
		// prevents.
		magnet, herr = honoredWithHoisted(stmt, c.hoisted, magnet)
		if herr != nil {
			return nil, herr
		}
	}
	// Resolve outer references in the aggregate arguments and items. A correlated
	// sub-Program reached via execWithParent rebuilds its outer chain from the live
	// parent frames (Program.OuterFrames / buildOuterEvalCtx). What still declines
	// is what has no faithful runtime answer; see bindAggOuterRefs.
	if !bindAggOuterRefs(c, allAggs, itemExprsOf(items)...) {
		return nil, fmt.Errorf("%w: aggregate argument correlated to an enclosing query this compiler cannot reach", errVDBEUnsupported)
	}

	// A select-list subquery is served: OpAggResult anchors a correlated one to the
	// group's anchor row, the row a bare column in the same item reads. A subquery
	// inside an aggregate's argument is evaluated per scanned row, as in C.

	// ORDER BY validation (see this function's doc comment): every term must
	// resolve exactly like query.go's own ORDER BY rules (resolveOrderKeys/
	// planGroupByStmt) -- an ordinal into outCols, a select-list alias, or a
	// genuine source column -- but nothing is ever emitted for it (the single
	// result row can't be reordered). An invalid term is a real, user-visible
	// C-SQLite rejection, not a capability gap, so it is wrapped
	// errVDBESemantic to PROPAGATE as the statement's actual error.
	for oi, ot := range stmt.OrderBy {
		if err := validateNoGroupOrderByTerm(c, ot, outCols, oi+1); err != nil {
			return nil, err
		}
	}

	// Same anchor-row precondition the GROUP BY compiler applies: a whole-table
	// aggregate has exactly one group, but a bare column in it still reads the
	// anchor, and SQLite's own min/max optimization is an index seek that lands
	// on a different TIED row than a full scan does. exprHasAnchorSubquery (not
	// the wider exprContainsSubquery) so a subquery that is strictly an
	// AGGREGATE's own argument -- e.g. "max((SELECT ...))" -- does not force
	// this guard: see its doc comment for why that shape never reads the
	// anchor at all.
	selSub := false
	for _, oc := range outCols {
		if anchorSubqueryWalk(oc.expr, c.subqueryReadsOuter) {
			selSub = true
			break
		}
	}
	// A whole-table aggregate has no GROUP BY key, so anchorIdentityFixed pins
	// nothing. withFromPlanTrust asks the FROM planning's question, before this
	// function's nQueryLoop reset. Unprovable order arms the run-time anchor
	// certificate (anchor_certificate.go) instead of declining.
	needAnchorCert := false
	subRead := selSub || anchorSubqueryWalk(stmt.Having, c.subqueryReadsOuter)
	for _, ot := range stmt.OrderBy {
		subRead = subRead || anchorSubqueryWalk(ot.Expr, c.subqueryReadsOuter)
	}
	if (usesGroupBare || subRead) &&
		!withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
			return anchorPlanOrderProvable(c.pager, c, srcs, scopes, stmt)
		}) {
		if c.outer != nil || subRead {
			return nil, errAnchorPlanOrder(!anchorNoIndexInPlay(c.pager, srcs, scopes, stmt))
		}
		needAnchorCert = true
	}
	// An ORDER-SENSITIVE AGGREGATE needs a row order proven too, for its own
	// reason -- see armOrderSensitive, which takes the SAME proof the anchor
	// does.
	orderProvable := len(allAggs) == 0 || !anyOrderSensitive(allAggs) ||
		withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
			return aggPlanOrderProvable(c.pager, c, srcs, scopes, stmt)
		})
	armOrderSensitive(orderProvable, allAggs)

	plan := &aggPlan{scopes: scopes, nCols: totalCols(srcs), cursors: cursorsOf(srcs), gathers: gathersOf(srcs), outPlans: items, magnet: magnet}

	// HAVING is planned exactly as compileScanGroupBy plans it (planGroupByStmt,
	// sql_group.go): the same planGroupItem call and, for a whole-table
	// aggregate, no GROUP BY key expressions to resolve against.
	// aggPlan.havingPlan / aggSetHaving / OpAggResult already work group-key-
	// less (aggResult passes groupKey == nil for a whole-table aggregate), so
	// nothing below the compiler needed a change.
	if stmt.Having != nil {
		// selectIsAggregateQuery(stmt) here is the SAME aggregateOwnedHere
		// planGroupByStmt computes: stmt is the identical AST on both this
		// compile's first attempt AND any hoistOwnerFor retry (only c.hoisted
		// differs between them), so this stays correctly false across a retry
		// too -- see checkItemSubqueries' own doc for what it gates. c.outer is
		// this compile's own enclosing compiler chain, threaded the same way
		// planGroupByStmt's own outerCompiler parameter is.
		hp, herr := planGroupItem(stmt.Having, nil, nil, nil, scopes, true, stmt.Columns, stmt.From, c.pager, selectIsAggregateQuery(stmt), c.outer, true)
		if herr != nil {
			return nil, declineOrSemantic(herr)
		}
		plan.havingPlan = hp
		// ...and through the same aggregate-ASSOCIATION check the select list
		// gets inside planNoGroupAggregate. HAVING is planned after that call
		// returns, so its own aggregates used to skip the check entirely and
		// bindAggOuterRefs would then happily bind one whose every column
		// belongs to the enclosing query -- a re-associated aggregate answered
		// per outer row. Verified against C SQLite over t1(a1)=(1,2,3),
		// t2(b1)=(4,5): "SELECT (SELECT sum(b1) FROM t2 HAVING sum(a1)>0) FROM
		// t1" is ONE row (9) there and was THREE here.
		if err := checkAggregateAssociation(hp.aggTemplates, scopes, nil, c.outer); err != nil {
			return nil, declineOrSemantic(err)
		}
		// The HAVING clause's own aggregates and its rewritten expression go
		// through the same reference-binding gate the select list's do -- both,
		// exactly as groupPlanRefs feeds them for a GROUP BY query, so a
		// correlated reference in HAVING is SERVED where bindAggOuterRefs can
		// reach it and declined only where it cannot.
		if !bindAggOuterRefs(c, hp.aggTemplates, hp.rewritten) {
			return nil, fmt.Errorf("%w: HAVING aggregate argument correlated to an enclosing query", errVDBEUnsupported)
		}
		// A HAVING accumulator is stepped over the same rows the select list's
		// are, so it needs the same row-order proof (this is the shape "SELECT
		// count(*) FROM t1,t2 WHERE ... HAVING group_concat(t2.c)='...'", whose
		// select list is order-free). Planned after the arming above, so it is
		// armed here.
		armOrderSensitive(!anyOrderSensitive(hp.aggTemplates) ||
			withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
				return aggPlanOrderProvable(c.pager, c, srcs, scopes, stmt)
			}), hp.aggTemplates)
	}
	if needAnchorCert {
		chk, ok := anchorCheckFor(append(append([]*itemPlan(nil), items...), plan.havingPlan))
		if !ok {
			return nil, errAnchorPlanOrder(!anchorNoIndexInPlay(c.pager, srcs, scopes, stmt))
		}
		plan.anchorCheck = chk
	}
	// Every item's rewritten tree, compiled against the registers holding this
	// group's finalized accumulators and anchor row -- select.c:8905/:8909's own
	// pair, finalizeAggFunctions and then ordinary expression coding. That is
	// past sqlite3WhereEnd, so under the PRE-loop nQueryLoop.
	c.nQueryLoop, c.nQueryLoopKnown = savedNQueryLoop, savedNQueryLoopKnown
	itemErr := compileAggItemPrograms(c.pager, c, plan)
	c.nQueryLoopKnown = false
	if itemErr != nil {
		return nil, itemErr
	}

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpAggReset, P4: plan})

	resultBase := c.allocN(len(outCols))

	// Constant-key seeks, as compileScanPlain does: without them an aggregate
	// full-scanned even when the WHERE pinned a rowid or indexed column (count(*)
	// with "sec = ?" took 23.7 ms vs 5 us for the row-returning form at 30k rows).
	// Safe because it only restricts candidates (the conjunct stays in the WHERE),
	// and the order is unchanged: a rowid key pins one row, and an equality on an
	// index's leading column visits rows in ascending rowid, their full-scan order.
	// Order matters here for group_concat and the min()/max() anchor.
	if idx, key, ok := detectRowidSeekKey(c, srcs, stmt.Where); ok {
		srcs[idx].seekKeyExpr = key
	} else if idx, iplan, ok := detectIndexSeekKey(c, srcs, stmt.Where); ok {
		srcs[idx].idxSeek = iplan
	}

	// Predicate pushdown: see compileScanPlain (vdbe_scan.go). buckets prune at
	// their join level; this body tests only the deferred residue. (The local
	// `plan` here is the aggregate plan, so the pushdown plan is jplan.)
	jplan := joinPushdownPlan(srcs, scopes, stmt.Where)
	// Index-driven inner-side seeks (annotateJoinSeeks, vdbe_join_seek.go): the
	// seek preserves ascending-rowid (full-scan) inner order, so an
	// order-sensitive aggregate (e.g. group_concat) sees the same row sequence.
	annotateJoinSeeks(c, srcs, jplan)
	deferredWhere := andConjuncts(jplan.deferred)

	// Aggregate argument lowering (planAggArgRegs): the registers are reserved
	// here, ONCE, and computed into from the body below -- which a LEFT/FULL
	// JOIN emits more than once.
	loweredArgs, argsErr := planAggArgRegs(c, plan, false, srcs, nil)
	if argsErr != nil {
		return nil, argsErr
	}

	body := func() error {
		whereJump := -1
		if deferredWhere != nil {
			// deferredWhere is andConjuncts of plan.deferred -- itself a set of
			// genuine top-level WHERE AND-conjuncts (splitTopLevelAnd, join.go)
			// recombined into one AND tree -- see emitJoinLevel's identical guard
			// (vdbe_join_codegen.go) and compiler.inWhereConjunct's doc comment.
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}
		if err := emitAggSteps(c, loweredArgs, func(seg int) {
			c.emit(Instruction{Op: OpAggStep, P2: seg, P3: 0, P4: plan})
		}); err != nil {
			return err
		}
		if whereJump >= 0 {
			c.patch(whereJump, c.here())
		}
		return nil
	}
	// The scan body runs with every cursor on the current row, so a correlated
	// subquery in the WHERE or grouping keys may read them (rowLive), as in
	// compileScanPlain (e_fkey.test). Restored afterwards: the result phase is not
	// row-live.
	savedRowLive := c.rowLive
	c.rowLive = true
	err := aggLoopNQueryLoop(c, savedNQueryLoopKnown, func() error { return emitJoinLoops(c, srcs, jplan, body) })
	c.rowLive = savedRowLive
	if err != nil {
		return nil, err
	}

	// Finalize + evaluate every output item into the result registers. This
	// always runs (even when LIMIT 0 / OFFSET>=1 will suppress the row), so any
	// evaluation error still surfaces.
	for i := range outCols {
		c.emit(Instruction{Op: OpAggResult, P1: resultBase + i, P3: -1, P4: &aggResultInfo{plan: plan, set: aggSetOut, idx: i}})
	}
	skipRow := (stmt.Offset != nil && *stmt.Offset >= 1) || (stmt.Limit != nil && *stmt.Limit == 0)
	// HAVING gates the one row, evaluated AFTER the outputs so an evaluation
	// error in either surfaces in the same order it always did. False or NULL
	// suppresses the row entirely ("SELECT count(*) FROM t1 HAVING NULL" and
	// "... HAVING 'abc'" both return zero rows -- verified directly), which is
	// the one way a whole-table aggregate can fail to return its single row.
	havingJump := -1
	if plan.havingPlan != nil {
		havingReg := c.alloc()
		c.emit(Instruction{Op: OpAggResult, P1: havingReg, P3: -1, P4: &aggResultInfo{plan: plan, set: aggSetHaving, idx: 0}})
		havingJump = c.emit(Instruction{Op: OpIfNot, P1: havingReg, P3: 1})
	}
	if !skipRow {
		c.emit(Instruction{Op: OpResultRow, P1: resultBase, P2: len(outCols)})
	}
	if havingJump >= 0 {
		c.patch(havingJump, c.here())
	}
	c.emit(Instruction{Op: OpHalt})

	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NCursors:   c.nCursor,
		NSubCache:  c.nSub,
		NResultCol: len(outCols),
		ColNames:   outColNames(outCols),
		Correlated: c.correlated,
	}, nil
}

// aggPlanTemplates returns every accumulator template plan's opcodes step over
// one scanned row, in the ORDER the runtime steps them (aggStep, vdbe_agg.go):
// the min()/max() census sites first -- magnetWalk runs before any item set --
// then the select list's, HAVING's and the GROUP BY ORDER BY terms'. Each
// template is returned once even when two item plans share one, so a caller
// that emits code per template does not emit it twice.
func aggPlanTemplates(plan *aggPlan) []*aggItem {
	out, _ := aggPlanStepOrder(plan)
	return out
}

// aggPlanStepOrder is aggPlanTemplates plus nMagnet, the number of leading
// templates that are min()/max() census sites. The census is indivisible:
// magnetWalk steps every site and then reads their verdict to choose the
// anchor, so no segment boundary may fall inside it. In C census sites are
// ordinary aggregates interleaved with the rest (select.c:6938); here they run
// first, and planAggArgRegs keeps them in one segment.
func aggPlanStepOrder(plan *aggPlan) (tmpls []*aggItem, nMagnet int) {
	seen := map[*aggItem]bool{}
	add := func(items []*aggItem) {
		for _, t := range items {
			if t != nil && !seen[t] {
				seen[t] = true
				tmpls = append(tmpls, t)
			}
		}
	}
	if plan.magnet != nil {
		add(plan.magnet.sites)
	}
	nMagnet = len(tmpls)
	for _, it := range plan.outPlans {
		if it != nil {
			add(it.aggTemplates)
		}
	}
	if plan.havingPlan != nil {
		add(plan.havingPlan.aggTemplates)
	}
	for _, op := range plan.orderPlans {
		if op.item != nil {
			add(op.item.aggTemplates)
		}
	}
	return tmpls, nMagnet
}

// planAggArgRegs reserves the registers for each accumulator's per-row
// expressions (FILTER, argument, group_concat separator / json_group_object
// value) and records them on the template (aggItem.rowRegs); emitAggArgRegs
// fills them before the step opcode, so aggItem.step reads values instead of
// walking expressions. This is updateAccumulator's shape (select.c:6808): the
// argument list coded into a temp range (select.c:6902) and handed to
// OP_AggStep (6942), with the FILTER a jump over both (select.c:6854).
//
// Two row sources:
//
//   - drain == nil: accumulators step from live cursors in the scan body
//     (OpAggStep, OpHashAggStep); compileExpr codes column reads as OpColumn.
//   - drain != nil: the sorted GROUP BY drain steps after the scan closed, off
//     the sorter record. aggDrainRow loads the referenced columns into a register
//     block under a regScope; the rest codes normally. That is C's split:
//     updateAccumulator inside the drain loop with useSortingIdx set
//     (select.c:8704, 8607), reading leaves from the pseudo-table over the record
//     (expr.c:4995).
//
// Rules:
//
//   - A FILTER is a jump, so an accumulator whose FILTER cannot be lowered
//     lowers nothing; a filtered-out row's argument must not be evaluated (e.g.
//     "sum(abs(y)) FILTER (WHERE 0)" must not raise overflow).
//   - A step body that can raise (aggStepBodyMayRaise) ends a step segment: C
//     codes aggregate i's arguments and step before aggregate i+1's arguments
//     (select.c:6824), so a later argument's error must not precede an earlier
//     body's. Later accumulators get the next segment (aggItem.stepSeg) and a
//     fresh step opcode (emitAggSteps). Segments coalesce wherever the boundary
//     is unobservable, so ordinary plans emit one step opcode per row. The
//     census block (aggPlanStepOrder) is never cut.
//   - grouped: the JSON subtype is dropped at the leaf (stripRowSubtype), as in
//     C, so nothing is refused for it.
//   - drain: an expression whose leaves the record cannot supply is refused
//     (aggDrainRow.lowerable); today only MATCH and fts aux calls.
//
// A refusal is the statement's error (errVDBEUnsupported); aggItem.rowValue
// reads only the register.
//
// Registers are allocated once here and filled by emitAggArgRegs, because
// emitJoinLevel emits the body more than once for LEFT/FULL/RIGHT joins; a
// register per emission made matched rows read a register only the
// NULL-extension path wrote (distinctagg.test 6.1). C reserves its range once
// per AggInfo too (select.c:6652).
func planAggArgRegs(c *compiler, plan *aggPlan, grouped bool, srcs []joinSource, drain *aggDrainRow) (aggArgPlan, error) {
	// The strip is the hash path's only: the sorted drain's record already lost the
	// subtype (OpSorterData's P3), and a row block that cannot carry one has
	// nothing to lose (aggRowCanCarrySubtype). It applies at the leaves, as in C.
	stripLeaves := grouped && drain == nil && aggRowCanCarrySubtype(srcs)
	// The drain is the only row source that refuses anything, and after a
	// refusal there is nowhere else to go: RULE #1 makes an expression the
	// compiler cannot lower the STATEMENT's error, not a route to something
	// that walks it.
	refuse := func(e Expr) error {
		if drain == nil || drain.lowerable(c, e) {
			return nil
		}
		return fmt.Errorf("%w: an aggregate's per-row expression over a sorted GROUP BY drain reads something the sorter record cannot serve (%T)", errVDBEUnsupported, e)
	}
	var lowered []*aggItem
	tmpls, nMagnet := aggPlanStepOrder(plan)
	seg := 0
	raised := false // a point that can raise is already coded in segment seg
	for i, it := range tmpls {
		it.rowRegs = [3]int{}
		if raised && i >= nMagnet {
			// C's boundary, taken: everything from here on is coded for the
			// NEXT step opcode, so nothing it evaluates can precede the raising
			// point above. i >= nMagnet because the census block is one unit
			// with no interior boundary to take -- see aggPlanStepOrder.
			seg++
			raised = false
		}
		it.stepSeg = seg
		stamped := false
		for _, which := range [3]int{aggExprFilter, aggExprArg, aggExprSep} {
			e := it.rowExpr(which)
			if e == nil {
				continue // count(*) has no argument; most kinds no separator
			}
			if err := refuse(e); err != nil {
				return aggArgPlan{}, err
			}
			it.rowRegs[which] = c.alloc() + 1
			stamped = true
		}
		// The call's own ORDER BY keys (aggItem.orderBy). A key that IS the
		// argument shares its register: C codes it once, as the key, with no
		// separate payload (bOBPayload==0, expr.c:7522-7526, select.c:6883-6893).
		if len(it.orderBy) == 1 && it.sepExpr == nil && it.expr != nil && !aggOBKeyIsArg(it) &&
			obKeyArgCursorZeroRisk(c, it.orderBy[0].Expr, it.expr) {
			return aggArgPlan{}, fmt.Errorf("%w: aggregate whose one ORDER BY term and one argument name the same column position of DIFFERENT tables, which 3.53.3 treats as the same expression when the ORDER BY's table is cursor 0 (see obKeyArgCursorZeroRisk)", errVDBEUnsupported)
		}
		it.obRegs = nil
		for k, t := range it.orderBy {
			if err := refuse(t.Expr); err != nil {
				return aggArgPlan{}, err
			}
			if it.obRegs == nil {
				it.obRegs = make([]int, len(it.orderBy))
			}
			if k == 0 && len(it.orderBy) == 1 && aggOBKeyIsArg(it) {
				it.obRegs[k] = it.rowRegs[aggExprArg]
				continue
			}
			it.obRegs[k] = c.alloc() + 1
			stamped = true
		}
		// min()/max() carry no orderBy -- C never codes theirs -- but C does
		// RESOLVE it (resolve.c:1326-1330): "max(x ORDER BY nosuch)" is "no
		// such column: nosuch" in 3.53.3.
		if it.orderBy == nil && (it.kind == aggMin || it.kind == aggMax) {
			for _, ob := range it.srcCall.orderByExprs() {
				if err := validateOrderByExprResolves(c, ob); err != nil {
					return aggArgPlan{}, err
				}
			}
		}
		if stamped {
			lowered = append(lowered, it)
		}
		if aggStepBodyMayRaise(it.kind) {
			raised = true
		}
	}
	return aggArgPlan{lowered: lowered, nSeg: seg + 1, stripLeaves: stripLeaves}, nil
}

// obKeyArgCursorZeroRisk reports whether 3.53.3 could treat an aggregate's one
// ORDER BY term as its argument where exprEqual says they differ, taking the
// key-only branch (expr.c:7522). sqlite3ExprCompare with iTab 0 matches two
// column references on position alone when the ORDER BY's is on cursor 0
// (expr.c:6640), an oracle bug that depends on C's cursor numbering:
//
//	SELECT group_concat(u.x ORDER BY t.x) FROM t, u    -> '1,1,2,2'
//	SELECT group_concat(u.x ORDER BY t.x) FROM u, t    -> '10,20,10,20'
//
// So decline where it can arise: two column references at the same position in
// different tables when the ORDER BY's table can be cursor 0 (rowid and an IPK
// are both -1; a table nested under a FROM is never cursor 0), or two compound
// expressions of the same shape that read a column. COLLATE never compares
// equal (expr.c:6585).
func obKeyArgCursorZeroRisk(c *compiler, ob, arg Expr) bool {
	a, aCol := ob.(ColumnExpr)
	b, bCol := arg.(ColumnExpr)
	if aCol && bCol {
		al, as, ap, aok := obResolveColumn(c, a)
		bl, bs, bp, bok := obResolveColumn(c, b)
		if !aok || !bok {
			return true
		}
		if al == bl && as == bs || ap != bp {
			return false
		}
		for o := al.outer; o != nil; o = o.outer {
			if len(o.scopes) > 0 {
				return false // the ORDER BY's table is nested under a FROM
			}
		}
		return true
	}
	if aCol || bCol {
		return false
	}
	if !exprHasColumnRef(ob) || !exprHasColumnRef(arg) {
		return false
	}
	switch x := ob.(type) {
	case FuncExpr:
		y, ok := arg.(FuncExpr)
		return ok && equalFoldName(x.Name, y.Name) && len(x.Args) == len(y.Args)
	case BinaryExpr:
		y, ok := arg.(BinaryExpr)
		return ok && x.Op == y.Op
	}
	return fmt.Sprintf("%T", ob) == fmt.Sprintf("%T", arg)
}

// obResolveColumn is obScopeColumn walked outward from c, innermost level
// first, the way a name is looked up: the level that answered, its scope and
// the column's position.
func obResolveColumn(c *compiler, x ColumnExpr) (level *compiler, scope, pos int, ok bool) {
	for lv := c; lv != nil; lv = lv.outer {
		if s, p, found := obScopeColumn(lv.scopes, x); found {
			return lv, s, p, true
		}
	}
	return nil, 0, 0, false
}

// obScopeColumn resolves a column reference against ONE query level's scopes,
// for obKeyArgCursorZeroRisk alone: the scope index and C's iColumn (-1 for the
// rowid and its INTEGER PRIMARY KEY alias). ok is false for anything it cannot
// pin to exactly one scope of this level.
func obScopeColumn(scopes []compileScope, x ColumnExpr) (scope, pos int, ok bool) {
	lname := r33sFoldIdent(x.Name)
	scope = -1
	for i := range scopes {
		s := &scopes[i].tableScope
		if x.Qualifier != "" && !equalFoldName(s.name, x.Qualifier) {
			continue
		}
		idx, has := s.colIndex[lname]
		switch {
		case has:
			pos = idx
			if idx >= 0 && idx < len(s.cols) && s.cols[idx].IsRowidAlias {
				pos = -1
			}
		case !s.noRowid && (lname == "rowid" || lname == "oid" || lname == "_rowid_"):
			pos = -1
		default:
			continue
		}
		if scope >= 0 {
			return 0, 0, false // ambiguous at this level
		}
		scope = i
	}
	return scope, pos, scope >= 0
}

// aggArgPlan is planAggArgRegs' output: the templates emitAggArgRegs fills, and
// nSeg, the number of step opcodes per row (at least 1). Each template's
// segment is stamped on it (aggItem.stepSeg) so cloneAggTemplates carries it.
type aggArgPlan struct {
	lowered []*aggItem
	nSeg    int

	// stripLeaves is the HASH-grouped plan's subtype loss, carried from
	// planAggArgRegs to emitAggArgRegs because it is a property of the PLAN
	// (which row sources it reads) and a fact the EMISSION has to act on. See
	// compiler.stripRowSubtype.
	stripLeaves bool
}

// emitDrainStepsWithoutRow is the sorted drain's path when no row block could
// be built (newAggDrainRow returned nil). A plan with nothing to lower emits its
// one step opcode as usual; a plan with per-row expressions has nowhere to
// evaluate them and declines. Stamps are cleared by hand since planAggArgRegs
// did not run, and a hoist retry compiles the same plan twice.
func emitDrainStepsWithoutRow(c *compiler, plan *aggPlan, emitDrainStep func(seg int)) error {
	for _, it := range aggPlanTemplates(plan) {
		it.rowRegs = [3]int{}
		it.stepSeg = 0
		for _, which := range [3]int{aggExprFilter, aggExprArg, aggExprSep} {
			if it.rowExpr(which) != nil {
				return fmt.Errorf("%w: a sorted GROUP BY drain over a row source whose scope and table disagree about their column count, with a per-row aggregate expression to evaluate", errVDBEUnsupported)
			}
		}
		if len(it.orderBy) > 0 || len(it.srcCall.OrderBy) > 0 {
			return fmt.Errorf("%w: a sorted GROUP BY drain over a row source whose scope and table disagree about their column count, with an aggregate ORDER BY to evaluate", errVDBEUnsupported)
		}
	}
	emitDrainStep(0)
	return nil
}

// aggRowCanCarrySubtype reports whether any value in this query's row block can
// carry a JSON subtype, turning the grouped strip from a mode into a type
// question. Subtypes arise from JSON function results, fts5_insttoken and
// json_each/json_tree's value column, so a row value can carry one only from a
// virtual table, a derived table, or a generated column. Stored columns and
// rowids cannot (C's sorter record cannot either, vdbeaux.c:4123).
func aggRowCanCarrySubtype(srcs []joinSource) bool {
	for i := range srcs {
		if srcs[i].derived != nil || srcs[i].vtabItem != nil {
			return true
		}
		if srcs[i].tbl == nil {
			return true // unknown shape: assume it can
		}
		for _, col := range srcs[i].tbl.cols {
			if col.IsGenerated() {
				return true
			}
		}
	}
	return false
}

// aggStepBodyMayRaise reports whether this kind's step body can return an
// error: group_concat/string_agg (coerceUTF16BlobValue) and the two JSON
// aggregates (jsonGroupValueText). It decides a segment cut, not a refusal, so
// over-including costs one step opcode; omitting one would hoist a later
// argument ahead of a failing body. group_concat is listed even though its
// coercion cannot fail today, so this does not depend on another file.
func aggStepBodyMayRaise(k aggKind) bool {
	switch k {
	case aggGroupConcat, aggJSONGroupArray, aggJSONGroupObject:
		return true
	}
	return false
}

// emitAggArgRegs computes segment seg's lowered per-row expressions into their
// registers just before that segment's step opcode. An expression that does not
// compile is the statement's error. Per accumulator: the FILTER, a jump over
// the arguments when false or NULL (select.c:6855), then the arguments
// (select.c:6902). aggItem.step reads the same filter register to skip
// accumulation.
func emitAggArgRegs(c *compiler, ap aggArgPlan, seg int) error {
	// The grouped subtype loss, applied to the LEAVES for the length of this
	// block exactly as C's sorter applies it to the record the drain reads
	// (select.c:8601-8607 / expr.c:4995-4997). Restored on the way out so the
	// step opcode, the GROUP BY key and everything else the scan body codes
	// still see a live subtype -- C codes those BEFORE the sorter.
	defer func(prev bool) { c.stripRowSubtype = prev }(c.stripRowSubtype)
	c.stripRowSubtype = ap.stripLeaves
	for _, it := range ap.lowered {
		if it.stepSeg != seg {
			continue // another of this row's step opcodes owns it
		}
		filterJump := -1
		if r := it.rowRegs[aggExprFilter]; r > 0 {
			fr, err := c.compileExpr(it.filter)
			if err != nil {
				return fmt.Errorf("aggregate FILTER: %w", err)
			}
			c.emit(Instruction{Op: OpSCopy, P1: fr, P2: r - 1})
			filterJump = c.emit(Instruction{Op: OpIfNot, P1: r - 1, P3: 1})
		}
		// An ORDER BY's keys are coded BEFORE the arguments, in updateAccumulator
		// (select.c:6883 ahead of :6891): an error in a key is raised first.
		for k, t := range it.orderBy {
			if k < len(it.obRegs) && it.obRegs[k] > 0 && it.obRegs[k] != it.rowRegs[aggExprArg] {
				vr, err := c.compileExpr(t.Expr)
				if err != nil {
					return fmt.Errorf("aggregate ORDER BY term: %w", err)
				}
				c.emit(Instruction{Op: OpSCopy, P1: vr, P2: it.obRegs[k] - 1})
			}
		}
		for _, which := range [2]int{aggExprArg, aggExprSep} {
			r := it.rowRegs[which]
			if r == 0 {
				continue
			}
			vr, err := c.compileExpr(it.rowExpr(which))
			if err != nil {
				return fmt.Errorf("aggregate per-row operand: %w", err)
			}
			c.emit(Instruction{Op: OpSCopy, P1: vr, P2: r - 1})
		}
		if filterJump >= 0 {
			c.patch(filterJump, c.here())
		}
	}
	return nil
}

// emitAggSteps emits, per step segment, the segment's per-row expressions then
// its step opcode, as updateAccumulator interleaves them (select.c:6824). The
// sorted drain runs this loop itself, hoisting its record loads and checking
// for dead-cursor reads (aggDrainInsnForbidden).
func emitAggSteps(c *compiler, ap aggArgPlan, step func(seg int)) error {
	for seg := 0; seg < ap.nSeg; seg++ {
		if err := emitAggArgRegs(c, ap, seg); err != nil {
			return err
		}
		step(seg)
	}
	return nil
}

// aggDrainRow is the sorted GROUP BY drain's row source: the sorter record,
// presented as a register-backed row scope so accumulator expressions are
// lowered there. The drain steps after the scan's cursors are closed, so
// nothing can be read from them.
//
// A port of C (select.c:8601-8607): open a pseudo-table over the sorter record
// and set useSortingIdx, after which updateAccumulator, called inside the
// drain loop (select.c:8704), codes arguments as usual and only the
// TK_AGG_COLUMN leaf reads the record (expr.c:4995):
//
//	}else if( pAggInfo->useSortingIdx ){
//	  Table *pTab = pCol->pTab;
//	  sqlite3VdbeAddOp3(v, OP_Column, pAggInfo->sortingIdxPTab,
//	                        pCol->iSorterColumn, target);
//
// Differences, because records here are native []Value: no pseudo-cursor
// (OpRecordColumn reads the record register directly), and no
// OP_RealAffinity (nothing was serialized). The block is [cols.., rowids..],
// matching the record's leading blocks (aggRowSplit); only slots the
// expressions read are loaded.
type aggDrainRow struct {
	rec    int          // record register OpSorterData fills: [cols.., rowids.., seq, groupVals..]
	base   int          // first of the nCols+len(srcs) registers holding [cols.., rowids..]
	nCols  int          // width of the record's (and the block's) leading columns block
	srcs   []joinSource // the scan's sources, for slotFor's cursor -> slot mapping
	scopes []regScope   // one per source, over base -- pushed while the drain's expressions compile
	need   []bool       // nCols+len(srcs) wide: which slots a lowered expression reads
	prev   *aggDrainRow // the drain this one displaced on compiler.drain, restored by pop
}

// newAggDrainRow reserves the drain's row block and builds one register-backed
// scope per source over it. It returns nil -- lower nothing --
// when a source's scope and its table disagree about how many columns
// it has, since resolveRowReg indexes regs by the scope's own colIndex and a
// short block would be an out-of-range panic (invariant 3). srcs and scopes are
// parallel here: compileScanGroupBy is never reached with a parenthesized join
// GROUP present (groupPresent, vdbe_scan.go), which is the only thing that
// makes tableScopesOf's flattened list longer than srcs.
func newAggDrainRow(c *compiler, rec int, srcs []joinSource, scopes []tableScope, nCols int) *aggDrainRow {
	// One regScope per LEAF scope, which is one per source EXCEPT for a
	// materialized parenthesized join GROUP: that is several member scopes
	// sharing the group's single flattened row, each starting at its own
	// colBase WITHIN it (compileScope.colBase). So a member's registers begin
	// at the GROUP's offset plus that base, and the rowid block is as wide as
	// the leaf-scope count rather than the source count.
	leaves := make([]compileScope, 0, len(scopes))
	owner := make([]int, 0, len(scopes)) // leaf -> its source index
	for i := range srcs {
		for _, sc := range sourceScopes(srcs[i]) {
			leaves = append(leaves, sc)
			owner = append(owner, i)
		}
	}
	if len(leaves) != len(scopes) {
		return nil
	}
	d := &aggDrainRow{rec: rec, nCols: nCols, srcs: srcs}
	d.base = c.allocN(nCols + len(leaves))
	d.need = make([]bool, nCols+len(leaves))
	for k := range leaves {
		n := len(leaves[k].cols)
		off := srcs[owner[k]].scope.offset + leaves[k].colBase
		if len(scopes[k].cols) != n || off+n > nCols {
			return nil
		}
		d.scopes = append(d.scopes, regScope{
			scope:    &scopes[k],
			regs:     rowRegsFor(d.base+off, n),
			rowidReg: d.base + nCols + k,
			// These ARE c.scopes' own FROM items, re-published over the
			// drain's register block -- see regScope.mirrorsCursorScope
			// (vdbe_codegen.go) for the wrong answer a second schema copy
			// caused in compiler.affCtx.
			mirrorsCursorScope: true,
		})
	}
	return d
}

// push publishes the drain on the compiler as well as pushing its scopes,
// because a RIGHT/FULL JOIN coalesced column cannot be served through a
// regScope at all: resolveRowReg answers with ONE register, and both join arms
// carry the name, so it either reports the ambiguity or silently picks an arm.
// compileColumn consults c.drain for exactly that shape (compileCoalesce).
func (d *aggDrainRow) push(c *compiler) {
	for _, rs := range d.scopes {
		c.pushRegScope(rs)
	}
	d.prev, c.drain = c.drain, d
}

func (d *aggDrainRow) pop(c *compiler) {
	c.drain = d.prev
	for range d.scopes {
		c.popRegScope()
	}
}

// lowerable reports whether e may be compiled against the drain's row block. A
// whitelist, so a new expression form cannot silently read a dead cursor. Every
// ColumnExpr must name a slot the drain serves (slotFor), and nothing may read a
// whole row off a closed cursor (MATCH and fts aux are excluded;
// aggDrainOpForbidden is the backstop).
//
// Sub-programs are admitted, as in C: analyzeAggregate walks into sub-selects
// (expr.c:7570), so a column inside an argument's subquery naming the aggregate
// query's own cursor becomes an AggInfo column read from the sorter record
// (expr.c:7454, 4996). Here compileColumn emits OpOuterAggReg into this block;
// a sub-program still reading this frame's cursors is refused
// (aggDrainInsnForbidden). RaiseExpr is absent: it cannot occur in a SELECT
// aggregate argument.
func (d *aggDrainRow) lowerable(c *compiler, e Expr) bool {
	switch x := e.(type) {
	case nil:
		return true
	case ColumnExpr:
		if d.slotFor(c, x) >= 0 || d.coalesceSlots(c, x) != nil {
			return true
		}
		if c.outerRegRowOffers(x) {
			// Not in this record, and not a cursor read either: an ENCLOSING
			// compile's REGISTER-backed row answers it, so compileColumn emits
			// an OpOuterAggReg -- a read of a live parent frame's register.
			// That is the same class as the three placeholder nodes above, and
			// it is the BARE spelling of exactly what they are: an aggregate
			// item program publishes its group's anchor row as register scopes
			// (compileAggItemProgram), so a body compiled beneath it resolves a
			// bare name there rather than being handed a placeholder.
			return true
		}
		if c.outerCursorOffers(x) {
			return true
		}
		return d.keywordLiteralOnly(c, x)
	case LiteralExpr, ParamExpr, SubqueryExpr, ExistsExpr:
		return true
	case groupAggExpr, groupKeyExpr, groupBareColExpr:
		// An enclosing aggregate's value substituted by aggItemSubst compiles to a
		// register (aggResultReg), never a cursor read, and that register is live for
		// as long as the drain is.
		return true
	case InExpr:
	case FuncExpr:
		// A window function is not an aggregate argument, and an fts aux call
		// (bm25/highlight/snippet, matchinfo/offsets) reads a scopes row.
		if x.Over != nil {
			return false
		}
	case UnaryExpr, BinaryExpr, IsNullExpr, BetweenExpr, LikeExpr, GlobExpr,
		CollateExpr, CastExpr, CaseExpr, RowExpr:
	default:
		return false
	}
	ok := true
	walkExprOperands(e, func(sub Expr) {
		if ok && sub != nil && !d.lowerable(c, sub) {
			ok = false
		}
	})
	return ok
}

// keywordLiteralOnly reports whether x is a bare TRUE/FALSE that binds to no
// column, so compileColumn emits a constant and no slot is needed. The
// conditions mirror compileColumn's FallbackLiteral arm: unqualified, unquoted,
// and no column of that name found.
func (d *aggDrainRow) keywordLiteralOnly(c *compiler, x ColumnExpr) bool {
	if x.FallbackLiteral == nil || x.Qualifier != "" || x.Schema != "" {
		return false
	}
	_, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager)
	return hard == nil && !found
}

// slotFor returns the row-block slot x is read from, or -1 if the drain cannot
// serve it. resolveInScopes (compileColumn's resolver) is the authority, and
// resolveRowReg must agree, since that is the arm compileColumn takes once the
// regScopes are pushed. They differ for flat FROM lists (innermost-first vs
// first match) and coalesced RIGHT/FULL columns; both were wrong answers (e.g.
// "t1 LEFT JOIN t2 USING(a) ... sum(a)" summed t2's NULL-extended a). Anything
// they do not agree on is refused.
func (d *aggDrainRow) slotFor(c *compiler, x ColumnExpr) int {
	cursor, colIdx, isRowid, found, fb, hard := resolveInScopes(c.scopes, x, c.pager)
	if hard != nil || !found || fb.has {
		return -1
	}
	var slot int
	if isRowid {
		src := d.srcForCursor(cursor)
		if src < 0 {
			return -1
		}
		slot = d.nCols + src // the record's rowid block, one entry per source
	} else if slot = d.slotForCursorCol(cursor, colIdx); slot < 0 {
		return -1
	}
	return slot
}

// srcForCursor returns the index of the one source behind cursor, or -1 when
// there is none (an outer/correlated scope, or a cursor this record has no
// block for) or more than one (no unique slot).
func (d *aggDrainRow) srcForCursor(cursor int) int {
	src := -1
	for i := range d.srcs {
		if d.srcs[i].scope.cursor != cursor {
			continue
		}
		if src >= 0 {
			return -1
		}
		src = i
	}
	return src
}

// slotForCursorCol maps one (cursor, column) pair to its record slot, or -1.
func (d *aggDrainRow) slotForCursorCol(cursor, colIdx int) int {
	src := d.srcForCursor(cursor)
	if src < 0 || colIdx < 0 || colIdx >= len(d.srcs[src].tbl.cols) {
		return -1
	}
	return d.srcs[src].scope.offset + colIdx
}

// coalesceSlots returns the record slots a RIGHT/FULL coalesced column reads,
// primary first then each fallback owner in resolveCoalesceChain's order, or
// nil if the record cannot serve every arm. The drain's emitColumnReadCoalesce;
// separate from slotFor because there is no single register.
// compileCoalesce reads these same slots.
func (d *aggDrainRow) coalesceSlots(c *compiler, x ColumnExpr) []int {
	cursor, colIdx, isRowid, found, fb, hard := resolveInScopes(c.scopes, x, c.pager)
	if hard != nil || !found || !fb.has || isRowid {
		return nil
	}
	slots := make([]int, 0, len(fb.owners)+1)
	for _, o := range append([]coalesceOwner{{cursor: cursor, colIdx: colIdx}}, fb.owners...) {
		slot := d.slotForCursorCol(o.cursor, o.colIdx)
		if slot < 0 {
			return nil
		}
		slots = append(slots, slot)
	}
	return slots
}

// compileCoalesce emits a coalescing column reference over the drain's row
// block: the same OpNotNull chain emitColumnReadCoalesce builds over cursors,
// with each OpColumn replaced by a copy out of the register the slot loaded
// into. Registers, not the record, because emitLoads has already copied every
// needed slot out and the arms may be read more than once.
func (d *aggDrainRow) compileCoalesce(c *compiler, x ColumnExpr) (int, bool) {
	slots := d.coalesceSlots(c, x)
	if slots == nil {
		return 0, false
	}
	r := c.alloc()
	c.emit(Instruction{Op: OpSCopy, P1: d.base + slots[0], P2: r})
	skips := make([]int, 0, len(slots)-1)
	for _, slot := range slots[1:] {
		skips = append(skips, c.emit(Instruction{Op: OpNotNull, P1: r}))
		c.emit(Instruction{Op: OpSCopy, P1: d.base + slot, P2: r})
	}
	end := c.here()
	for _, j := range skips {
		c.patch(j, end)
	}
	return r, true
}

// compileDrainColumn serves any column reference the drain's block can answer,
// resolving through resolveInScopes like the cursor path, and returns the
// register its slot was loaded into. It replaces slotFor's agreement check,
// which refused every USING/NATURAL join (resolveRowReg's innermost-wins answers
// b.k where the join rule gives a.k). Anything the block cannot serve (an outer
// reference, a cursor with no block, two sources on one cursor) falls through
// to compileColumn's other arms.
func (d *aggDrainRow) compileDrainColumn(c *compiler, x ColumnExpr) (int, bool) {
	if reg, ok := d.compileCoalesce(c, x); ok {
		return reg, true
	}
	slot := d.slotFor(c, x)
	if slot < 0 {
		return 0, false
	}
	return d.base + slot, true
}

// markNeeded records the slots the lowered expressions read, using lowerable's
// resolver. A node with a sub-program marks the whole block (needAll) rather
// than re-deriving a binding from its *SelectStmt; the sub-compile already bound
// it to a register here via OpOuterAggReg.
func (d *aggDrainRow) markNeeded(c *compiler, e Expr) {
	if e == nil {
		return
	}
	switch x := e.(type) {
	case ColumnExpr:
		if slot := d.slotFor(c, x); slot >= 0 {
			d.need[slot] = true
			return
		}
		// A coalescing reference reads EVERY arm, so every arm's slot has to
		// be loaded; marking only the primary would leave the fallbacks
		// reading whatever the register block last held.
		for _, slot := range d.coalesceSlots(c, x) {
			d.need[slot] = true
		}
		return
	case SubqueryExpr, ExistsExpr:
		d.needAll()
		return
	case InExpr:
		if x.Sub != nil {
			d.needAll()
		}
	}
	walkExprOperands(e, func(sub Expr) { d.markNeeded(c, sub) })
}

// needAll marks the whole row block as loaded -- see markNeeded.
func (d *aggDrainRow) needAll() {
	for i := range d.need {
		d.need[i] = true
	}
}

// outerSlotReg is compileDrainColumn for a program compiled beneath the drain
// (an argument's subquery), returning the register to read across the frame
// with OpOuterAggReg. It refuses an unmarked slot: emitLoads already ran, so it
// would be unfilled. markNeeded marks every slot when a sub-program is present,
// so this is a guarantee, not a filter.
func (d *aggDrainRow) outerSlotReg(c *compiler, x ColumnExpr) (int, bool) {
	slot := d.slotFor(c, x)
	if slot < 0 || !d.need[slot] {
		return 0, false
	}
	return d.base + slot, true
}

// outerCoalesceRegs is outerSlotReg for a RIGHT/FULL JOIN's USING/NATURAL
// column: the row-block registers holding each arm, primary first, in
// coalesceSlots' order -- the same slots compileCoalesce reads, so the two
// cannot pick different arms. The CHAIN itself is emitted by the caller, into
// its own frame, because that is the frame that can emit at all.
func (d *aggDrainRow) outerCoalesceRegs(c *compiler, x ColumnExpr) ([]int, bool) {
	slots := d.coalesceSlots(c, x)
	if slots == nil {
		return nil, false
	}
	regs := make([]int, len(slots))
	for i, slot := range slots {
		if !d.need[slot] { // see outerSlotReg
			return nil, false
		}
		regs[i] = d.base + slot
	}
	return regs, true
}

// emitLoads copies the needed record slots into the row block, one
// OpRecordColumn each (expr.c:4996 without the pseudo-cursor). Emitted once
// ahead of the block rather than at each leaf: a record read cannot raise or
// have side effects, so hoisting it past a FILTER jump is unobservable. The
// argument computation stays behind the jump (emitAggArgRegs).
func (d *aggDrainRow) emitLoads(c *compiler, lowered []*aggItem) {
	for _, it := range lowered {
		for _, which := range [3]int{aggExprFilter, aggExprArg, aggExprSep} {
			if it.rowRegs[which] > 0 {
				d.markNeeded(c, it.rowExpr(which))
			}
		}
		// The call's own ORDER BY keys read the record too. Missing them left
		// every key an unloaded register, so every row tied and "group_concat(a
		// ORDER BY d) ... GROUP BY b" came out in arrival order
		// (aggorderby.test 2.4).
		for _, t := range it.orderBy {
			d.markNeeded(c, t.Expr)
		}
	}
	for i, want := range d.need {
		if want {
			c.emit(Instruction{Op: OpRecordColumn, P1: d.rec, P2: i, P3: d.base + i})
		}
	}
}

// aggDrainOpForbidden is the drain's correctness backstop: an opcode that reads
// a cursor row, or runs a sub-program reaching this frame's cursors, must not
// appear in drain code since every cursor is exhausted. Unlike
// readsWholeCursorRows, an omission here is a wrong answer, so it lists
// everything explicitly. Reaching it means lowerable admitted an unmodelled
// route; the caller rolls back and declines.
//
// No case reproduces it today (slotFor and the regScope subquery refusals close
// every route), but it is the one structural guard: an exhausted cursor fails
// silently with whatever row it was left on. TestAggDrainReadsNoCursor asserts
// the invariant.
func aggDrainOpForbidden(op OpCode) bool {
	switch op {
	case OpColumn, OpRowid: // a live cursor's row
		return true
	case OpOuterColumn, OpOuterRowid: // an enclosing frame's cursors
		return true
	case OpAggStep, OpHashAggStep: // gatherCursorRow
		return true
	case OpMatch, OpFts3Aux, OpFts5Aux: // gatherScopesRow
		return true
	case OpOpenDerived: // execWithParent -> gatherFrameRow
		// This function is handed an OPCODE, so it has no derivedSource to
		// inspect and refuses the whole kind. aggDrainInsnForbidden, which does
		// have the payload, looks into the body instead -- and only ever for a
		// derived table nested inside a sub-program, since an aggregate operand
		// cannot name one directly.
		return true
	}
	return false
}

// aggDrainInsnForbidden is aggDrainOpForbidden over an instruction, plus, for
// the four opcodes that run a sub-Program, a walk of that program for a read of
// the drain's frame. A sub-Program runs on a fresh vdbe whose parent is the
// drain's, so OpOuterColumn with P5==1 names the drain, 2 one level deeper. A
// compound is checked at its wrapper's level (it has no machine of its own). A
// level beyond the drain names a live outer scan and is fine.
func aggDrainInsnForbidden(in Instruction, level int) bool {
	if (in.Op == OpOuterColumn || in.Op == OpOuterRowid) && int(in.P5) >= level {
		// The block's own read of an ENCLOSING frame (level 1 is its parent):
		// the drain's own cursors are read by OpColumn/OpRowid, and an
		// enclosing query stays on its row while this one runs
		// (outerCursorOffers).
		return false
	}
	if aggDrainOpForbidden(in.Op) {
		return true
	}
	switch in.Op {
	case OpSubquery, OpExists:
		prog, _ := in.P4.(*Program)
		return aggDrainProgReadsFrame(prog, level)
	case OpInSub:
		plan, _ := in.P4.(*inSubPlan)
		return plan == nil || aggDrainProgReadsFrame(plan.prog, level)
	case OpRowSub:
		plan, _ := in.P4.(*rowSubPlan)
		return plan == nil || aggDrainProgReadsFrame(plan.prog, level)
	}
	return false
}

// aggDrainProgReadsFrame reports whether prog, or anything nested beneath it,
// reads the frame `level` parent hops out -- the drain's, whose cursors closed
// with the scan. A nil program is REFUSED rather than trusted: an unknown shape
// is exactly what this backstop exists for.
func aggDrainProgReadsFrame(prog *Program, level int) bool {
	if prog == nil {
		return true
	}
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			if aggDrainProgReadsFrame(arm, level) {
				return true
			}
		}
		return false // a compound wrapper carries no instructions of its own
	}
	for i := range prog.Insns {
		in := prog.Insns[i]
		switch in.Op {
		case OpOuterColumn, OpOuterRowid:
			if int(in.P5) == level {
				return true
			}
		case OpSubquery, OpExists:
			sub, _ := in.P4.(*Program)
			if aggDrainProgReadsFrame(sub, level+1) {
				return true
			}
		case OpInSub:
			plan, _ := in.P4.(*inSubPlan)
			if plan == nil || aggDrainProgReadsFrame(plan.prog, level+1) {
				return true
			}
		case OpRowSub:
			plan, _ := in.P4.(*rowSubPlan)
			if plan == nil || aggDrainProgReadsFrame(plan.prog, level+1) {
				return true
			}
		case OpOpenDerived:
			// A derived table's body is materialised through execWithParent
			// too, so its program is checked one level further in -- the same
			// arithmetic as a subquery's. Every OTHER derivedSource kind is
			// REFUSED: a CTE runs resolveCTERows' runtime compile and a virtual
			// table / TVF runs materializeVtab, neither of which this walk can
			// inspect, and a schema/sequence catalog source carries no program
			// either. Refusing an uninspectable body is what this backstop is
			// for.
			ds, _ := in.P4.(*derivedSource)
			if ds == nil || ds.prog == nil || aggDrainProgReadsFrame(ds.prog, level+1) {
				return true
			}
		}
	}
	return false
}

// bindAggOuterRefs walks every ColumnExpr an aggregate query evaluates
// INTERPRETIVELY -- each aggregate's own argument expressions (expr/sepExpr,
// stepped per row by aggRowCtx) and each already-rewritten output/HAVING/ORDER
// BY item (evaluated once per group by aggResult) -- and reports whether every
// one of them can be resolved at run time.
//
// A reference into c's OWN scopes always can. One that resolves only in an
// ENCLOSING compile is a genuine correlated reference, and IS servable: the
// sub-Program carries its enclosing frames (Program.OuterFrames, attached by
// compileSubProgram) and execWithParent rebuilds the evalCtx chain from the
// live parent frames, so the ordinary outward walk over it finds the reference. Binding one
// here also marks every compiler up to the owning one CORRELATED -- nothing
// else would, because no OpOuterColumn is ever emitted for it, and without the
// flag the sub-Program would be run once and CACHED (runSubOnce) instead of
// re-run against each outer row.
//
// Everything else is declined: a reference that resolves nowhere, a hard
// resolution error (ambiguity), a RIGHT/FULL JOIN coalesced outer column
// (OpOuterColumn's own decline, mirrored -- gatherFrameRow reads the raw
// cursor, not the coalesced chain), and an outer scope whose cursors are not
// row-live where this sub-Program runs.
func bindAggOuterRefs(c *compiler, allAggs []*aggItem, items ...Expr) bool {
	// regRow TRUE: an aggregate query's references are COMPILED now, on both
	// halves. Each item's rewritten tree is a program (compileAggItemProgram),
	// and each aggregate's per-row expressions are compiled into registers by
	// planAggArgRegs/emitAggArgRegs -- including in the sorted DRAIN, whose
	// whitelist admits a reference an enclosing register row answers
	// (aggDrainRow.lowerable). So an enclosing compile's register-backed row is
	// a place the compile that follows really does find the value, and refusing
	// to see it here declines a statement the compiler goes on to build: the
	// aggnested.test shape "SELECT max(value1), (SELECT sum(value2=value1) FROM
	// t2) FROM t1 GROUP BY id1" is one, whose bare value1 lives in the item
	// program's own anchor-row register block.
	//
	// It was FALSE while aggResult walked the item's tree against
	// m.outer -- an evalCtx chain rebuilt from enclosing frames' CURSORS
	// (gatherFrameRow), which cannot see a register. Answering "bound" for a
	// register row then promised a value that walk could not find, and it does
	// not fail quietly: it keeps going, so an enclosing cursor scope offering
	// the same NAME would answer instead. That arm is deleted.
	ok, _ := bindAggOuterRefsReason(c, allAggs, true, items...)
	return ok
}

// bindAggOuterRefsReason is bindAggOuterRefs plus whether a failure was a real
// resolution error (an ambiguous name) rather than a reference no reachable
// scope offers. compileScanWindow declines the latter but lets an ambiguity run,
// so it reports C's "ambiguous column name" rather than a decline.
//
// regRow selects the delivery mechanism: aggregate callers deliver a bound
// reference through the evalCtx chain rebuilt from enclosing frames' cursors, so
// for them "bound" must mean a cursor scope offers it; compileScanWindow's
// references are compiled and can also reach register-backed rows. Mixing them
// would let a compile succeed on a value the outward walk then finds elsewhere,
// under the same name.
func bindAggOuterRefsReason(c *compiler, allAggs []*aggItem, regRow bool, items ...Expr) (ok, hardErr bool) {
	allLocal := true
	var walk func(Expr)
	walk = func(e Expr) {
		if !allLocal {
			return
		}
		switch x := e.(type) {
		case nil, LiteralExpr, ParamExpr:
		case ColumnExpr:
			// A PSEUDO-ROW reference -- NEW./OLD. in a trigger body -- names no
			// FROM item and needs no outer binding: compileColumn answers it
			// from the compiler's own trigger context, as an OpParam read of
			// the firing row. Without this arm a FROM-less aggregate over one
			// ("INSERT INTO tlog VALUES((SELECT sum(new.a)))") found nothing to
			// resolve and declined "column reference in a FROM-less aggregate",
			// where the oracle answers.
			//
			// It is not "local" either, which is why it returns rather than
			// falling through: C never counts it, because analyzeAggregate
			// associates by matching TK_COLUMN cursor numbers and a NEW./OLD.
			// reference is TK_TRIGGER (expr.c).
			if c.trig != nil && x.Qualifier != "" {
				switch r33sFoldIdent(x.Qualifier) {
				case "new", "old":
					return
				}
			}
			// An upsert's "excluded" is the same kind of reference, answered by
			// the same opcode: compileColumn resolves it off the enclosing
			// DO UPDATE's context as an OpParam read of the proposed row
			// (emitExcludedParam), so it names no FROM item and needs no outer
			// binding. Without this arm "DO UPDATE SET v=(SELECT sum(
			// excluded.v))" declined "column reference in a FROM-less
			// aggregate" where the oracle answers. C does not count it either,
			// for the identical reason the NEW./OLD. arm above does not:
			// lookupName rewrites it to TK_REGISTER (resolve.c:572-576), and
			// analyzeAggregate associates by TK_COLUMN cursor number.
			// x.Schema == "" is resolve.c:521's own "cnt==0 && zDb==0": a
			// three-part "main.excluded.v" never reaches the upsert arm, so it
			// must not be carved out here either -- emitExcludedParam refuses
			// it, and this predicate has to agree or the binder would report a
			// name nothing can compile.
			if ec := c.enclosingExcl(); ec != nil && !ec.shadowed && x.Schema == "" && x.Qualifier != "" && equalFoldName(x.Qualifier, "excluded") {
				return
			}
			_, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager)
			if hard != nil {
				allLocal = false
				hardErr = true
				return
			}
			if !found && !c.bindOuterAggRef(x, regRow) {
				// A bare TRUE/FALSE is lexed as an IDENTIFIER carrying a literal
				// fallback (ColumnExpr.FallbackLiteral, sql_ast.go), because
				// SQLite resolves it as a column FIRST and only converts it to a
				// boolean when no column of that name exists
				// (sqlite3ExprIdToTrueFalse). So the fallback is consulted here
				// too, and only AFTER the local and outer lookups have both
				// failed -- an enclosing column actually named "true" still wins.
				// A plain row read applies the same fallback (columnRowValue,
				// row_scope.go),
				// so the argument evaluates to the literal exactly as it does in
				// every non-aggregate position.
				//
				// Without this, EVERY aggregate over one declined: "SELECT
				// count(TRUE) FROM t GROUP BY g", "sum(TRUE)", "max(FALSE)",
				// "count(DISTINCT TRUE)" (with or without GROUP BY) and
				// "group_concat(v, TRUE)" -- while the same literal in the select
				// list, in GROUP BY, or in a FILTER already worked.
				if x.Qualifier == "" && x.FallbackLiteral != nil {
					return
				}
				// A RETURNING clause's affected row, the third pseudo-row: a
				// subquery in the list reads it as an OpParam of the same kind
				// (emitReturningParam), reached only after every enclosing FROM
				// missed -- resolve.c:528-532 tries it after the FROM-item walk.
				// returning1.test 20.3's "(SELECT min(t2.a)+t1.a*100 FROM t1 AS
				// t2)" names t1.a there.
				if c.enclosingRetRowAnswers(x) {
					return
				}
				allLocal = false
			}
		case FuncExpr:
			for _, a := range x.walkArgs() {
				walk(a)
			}
		case UnaryExpr:
			walk(x.X)
		case BinaryExpr:
			walk(x.L)
			walk(x.R)
		case IsNullExpr:
			walk(x.X)
		case InExpr:
			walk(x.X)
			for _, a := range x.List {
				walk(a)
			}
			if x.Sub != nil {
				markSubqueryCorrelated(c)
			}
		case BetweenExpr:
			walk(x.X)
			walk(x.Lo)
			walk(x.Hi)
		case LikeExpr:
			walk(x.X)
			walk(x.Pattern)
			walk(x.Escape)
		case GlobExpr:
			walk(x.X)
			walk(x.Pattern)
		case CollateExpr:
			walk(x.X)
		case CastExpr:
			walk(x.X)
		case CaseExpr:
			if x.Base != nil {
				walk(x.Base)
			}
			for _, w := range x.Whens {
				walk(w.When)
				walk(w.Then)
			}
			if x.Else != nil {
				walk(x.Else)
			}
		// SubqueryExpr/ExistsExpr: an independent scope, deliberately not
		// descended into (matches aggArgHasLocalColumnRef's identical
		// reasoning, sql_agg.go) -- but its BODY may still reach a query
		// enclosing c at run time, which is what markSubqueryCorrelated is for.
		case SubqueryExpr:
			markSubqueryCorrelated(c)
		case ExistsExpr:
			markSubqueryCorrelated(c)
		default:
		}
	}
	for _, agg := range allAggs {
		if agg.expr != nil {
			walk(agg.expr)
		}
		if agg.sepExpr != nil {
			walk(agg.sepExpr)
		}
		// Walk the FILTER too: it is evaluated per row against the aggregate's evalCtx,
		// so an outer reference in it must be bound and the compile marked correlated.
		// Otherwise "SELECT (SELECT count(*) FILTER (WHERE x = o.a) FROM inr) FROM o" ran
		// once with no parent ("no such table: o"). C resolves the FILTER in the same
		// NameContext walk (resolve.c:1352, setting isCorrelated at 1953) and analyses
		// it for aggregates (select.c:6544; walker.c:31).
		if agg.filter != nil {
			walk(agg.filter)
		}
		if !allLocal {
			return false, hardErr
		}
	}
	for _, it := range items {
		walk(it)
		if !allLocal {
			return false, hardErr
		}
	}
	return allLocal, hardErr
}

// markSubqueryCorrelated marks c and every enclosing compile correlated because
// an expression of c's aggregate query holds a subquery that may bind an
// enclosing column at run time. bindAggOuterRefs does not descend into
// subqueries, so nothing else sets the flag; uncorrelated, the sub-Program runs
// once with no parent and the inner reference fails (aggnested.test: "SELECT
// (SELECT sum(x+(SELECT y)) FROM bb) FROM aa"). Conservative: it costs only the
// run-once caching. Only taken when an enclosing row exists.
func markSubqueryCorrelated(c *compiler) {
	if c == nil || (c.outer == nil && c.rowOuter == nil) {
		return
	}
	if c.rowOuter == nil && !enclosingOffersRow(c.outer) {
		return
	}
	for p := c; p != nil; p = p.outer {
		p.correlated = true
	}
}

// enclosingOffersRow reports whether any compile enclosing oc (inclusive) can
// supply a row a subquery could bind against: a FROM scope or a register row.
// trigOnlyOuter gives a trigger body an enclosing compiler holding only the
// NEW/OLD pseudo-row, read through a register (resolve.c:522-544), so there is no
// enclosing row to reach. Marking correlated there would also make
// compileLiveSubProgram refuse the program.
func enclosingOffersRow(oc *compiler) bool {
	for ; oc != nil; oc = oc.outer {
		if len(oc.scopes) != 0 || len(oc.regScopes) != 0 || oc.drain != nil || oc.bufOut != nil || oc.rowOuter != nil {
			return true
		}
	}
	return false
}

// outerRegRowOffers reports whether x -- a reference THIS compile's own scopes
// do not answer -- is answered by an ENCLOSING compile's REGISTER-backed row.
// It walks the chain the way compileColumn walks it: each compile's register
// row before its cursor scopes, stopping at the first that answers, so it
// predicts which of the two compileColumn will emit rather than guessing.
//
// A false answer for a name an enclosing CURSOR scope offers is the point: that
// read is an OpOuterColumn, which is a different class entirely.
func (c *compiler) outerRegRowOffers(x ColumnExpr) bool {
	if _, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager); hard != nil || found {
		return false
	}
	for oc := c.outer; oc != nil; oc = oc.outer {
		if oc.rowLive && len(oc.regScopes) != 0 {
			switch _, found, amb := oc.resolveRowReg(x); {
			case amb != nil:
				return false
			case found:
				return true
			}
		}
		if _, _, _, found, _, hard := resolveInScopes(oc.scopes, x, oc.pager); hard != nil || found {
			return false
		}
	}
	return false
}

// outerCursorOffers reports whether x, which this query's own scopes do not
// resolve, is a correlated read of an ENCLOSING query's cursor. That query
// sits on its row for as long as this subquery runs, drain included -- the
// cursors a drain has lost are its own -- and C reads it there: a TK_COLUMN
// of an outer cursor becomes no AggInfo column (expr.c:7454-7469), so the
// aggregate argument codes it as the plain correlated read (indexexpr2.test
// 9.0's "max(c+abs(b))" over t1.b).
func (c *compiler) outerCursorOffers(x ColumnExpr) bool {
	if _, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager); hard != nil || found {
		return false
	}
	for oc := c.outer; oc != nil; oc = oc.outer {
		if oc.rowLive && len(oc.regScopes) != 0 {
			if _, found, amb := oc.resolveRowReg(x); found || amb != nil {
				return false
			}
		}
		switch _, _, _, found, _, hard := resolveInScopes(oc.scopes, x, oc.pager); {
		case hard != nil:
			return false
		case found:
			return true
		}
	}
	return false
}

// bindOuterAggRef resolves x against the ENCLOSING compile chain exactly as
// compileColumn does, and on a match marks every compiler from c up to (not
// including) the owning one as correlated. See bindAggOuterRefs, its only
// caller, for why the flag has to be set here by hand.
func (c *compiler) bindOuterAggRef(x ColumnExpr, regRow bool) bool {
	for oc := c.outer; oc != nil; oc = oc.outer {
		// An enclosing compile's register row is asked before its cursor scopes, the
		// order compileColumn uses, so this answer predicts the compile. Only when
		// regRow (references are compiled); see bindAggOuterRefsReason. A window
		// projection has no cursor chain, so a window query nested in its subquery
		// reaches the enclosing row only here (window1.test 34.2;
		// compat-harness/window_r24_nested_spec_test.go). windowBufCols is not mirrored:
		// this walk never enters a SubqueryExpr, the only way a buffered reference is
		// written.
		if regRow && oc.rowLive && len(oc.regScopes) != 0 {
			switch _, found, amb := oc.resolveRowReg(x); {
			case amb != nil:
				return false
			case found:
				for p := c; p != oc; p = p.outer {
					p.correlated = true
				}
				return true
			}
		}
		_, _, _, found, fb, hard := resolveInScopes(oc.scopes, x, oc.pager)
		if hard != nil {
			return false
		}
		if !found {
			continue
		}
		if fb.has || !oc.rowLive {
			return false
		}
		for p := c; p != oc; p = p.outer {
			p.correlated = true
		}
		return true
	}
	// Last resort: the live enclosing row (compiler.rowOuter), for a subquery
	// compiled standalone with no enclosing compiler. It is the same evalCtx the
	// program runs with (tryVDBEScan), and for an enclosing aggregate query it is
	// the anchor row, so the answer is predictive. Only reached for an aggregate the
	// association check made this query's own (checkAggregateAssociation), so other
	// references in it are per-row constants (aggnested-3.10).
	if row := c.outerRowCtx(); row != nil {
		if _, ok, err := resolveRowCtxValue(row, x); err == nil && ok {
			return true
		}
	}
	return false
}

// validateNoGroupOrderByTerm validates one ORDER BY term of a whole-table
// aggregate with resolveOrderKeys' rules: an integer literal is a 1-based
// ordinal (out of range is an error), a bare name matching an output alias is
// that column, anything else must resolve against the query's scopes. Nothing
// is emitted: there is one row to sort.
func validateNoGroupOrderByTerm(c *compiler, ot OrderTerm, outCols []outputColumn, term int) error {
	if n, ok := orderByOrdinal(ot.Expr); ok {
		if n < 1 || int(n) > len(outCols) {
			return semanticf("%s ORDER BY term out of range - should be between 1 and %d",
				sqliteOrdinalWord(term), len(outCols))
		}
		return nil
	}
	if colRef, ok := ot.Expr.(ColumnExpr); ok && colRef.Qualifier == "" {
		if idx := findOutputColByName(outCols, colRef.Name); idx >= 0 {
			return nil
		}
	}
	return validateOrderByExprResolves(c, ot.Expr)
}

// validateOrderByExprResolves checks that every column reference in e binds
// against c's scopes (resolveInScopes). A failure is errVDBESemantic; a shape
// outside this walk (a subquery) declines. Functions, CASE, IN, BETWEEN,
// LIKE and GLOB are walked since their value is irrelevant with one output row;
// an unknown or wrong-arity function declines, and an aggregate call is checked
// with planAggregateCall.
func validateOrderByExprResolves(c *compiler, e Expr) error {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return nil
	case ColumnExpr:
		_, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager)
		if hard != nil {
			return semanticf("%v", hard)
		}
		if found {
			return nil
		}
		if x.Qualifier != "" {
			return fmt.Errorf("%w: no such table: %s", errVDBEUnsupported, x.Qualifier)
		}
		if x.FallbackLiteral != nil {
			return nil
		}
		return semanticf("engine: no such column: %s", x.Name)
	case UnaryExpr:
		return validateOrderByExprResolves(c, x.X)
	case BinaryExpr:
		if err := validateOrderByExprResolves(c, x.L); err != nil {
			return err
		}
		return validateOrderByExprResolves(c, x.R)
	case IsNullExpr:
		return validateOrderByExprResolves(c, x.X)
	case CollateExpr:
		return validateOrderByExprResolves(c, x.X)
	case CastExpr:
		return validateOrderByExprResolves(c, x.X)
	case FuncExpr:
		return validateOrderByFuncResolves(c, x)
	case CaseExpr:
		if x.Base != nil {
			if err := validateOrderByExprResolves(c, x.Base); err != nil {
				return err
			}
		}
		for _, w := range x.Whens {
			if err := validateOrderByExprResolves(c, w.When); err != nil {
				return err
			}
			if err := validateOrderByExprResolves(c, w.Then); err != nil {
				return err
			}
		}
		if x.Else != nil {
			return validateOrderByExprResolves(c, x.Else)
		}
		return nil
	case InExpr:
		if x.Sub != nil {
			// A subquery probe: outside this narrow walk, same as a bare
			// SubqueryExpr (default case below).
			return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
		}
		if err := validateOrderByExprResolves(c, x.X); err != nil {
			return err
		}
		for _, a := range x.List {
			if err := validateOrderByExprResolves(c, a); err != nil {
				return err
			}
		}
		return nil
	case BetweenExpr:
		if err := validateOrderByExprResolves(c, x.X); err != nil {
			return err
		}
		if err := validateOrderByExprResolves(c, x.Lo); err != nil {
			return err
		}
		return validateOrderByExprResolves(c, x.Hi)
	case LikeExpr:
		if err := validateOrderByExprResolves(c, x.X); err != nil {
			return err
		}
		if err := validateOrderByExprResolves(c, x.Pattern); err != nil {
			return err
		}
		if x.Escape == nil {
			return nil
		}
		return validateOrderByExprResolves(c, x.Escape)
	case GlobExpr:
		if err := validateOrderByExprResolves(c, x.X); err != nil {
			return err
		}
		return validateOrderByExprResolves(c, x.Pattern)
	default:
		return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
	}
}

// validateOrderByFuncResolves is validateOrderByExprResolves' FuncExpr case: an
// aggregate is checked with planAggregateCall, a scalar must be known with a
// valid arity (else decline), and arguments and any FILTER clause are walked for
// column references.
func validateOrderByFuncResolves(c *compiler, x FuncExpr) error {
	if isAggregateCall(x) {
		if _, err := planAggregateCall(x); err != nil {
			return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
		}
		if x.Filter != nil {
			if err := validateOrderByExprResolves(c, x.Filter); err != nil {
				return err
			}
		}
		for _, ob := range x.orderByExprs() {
			if err := validateOrderByExprResolves(c, ob); err != nil {
				return err
			}
		}
	} else {
		name := r33sFoldIdent(x.Name)
		// A connection-state function is admitted by PREDICATE, not by a
		// supportedFuncs entry -- the same carve-out checkExprSupported makes
		// (isConnStateFunc arm) and rewriteGroupExpr now makes.
		// It takes no arguments, so funcArity has nothing to say about it.
		// "SELECT count(*) FROM t ORDER BY changes()" is accepted by real
		// SQLite (verified against 3.53.3, alongside the GROUP BY spellings
		// recorded at rewriteGroupExpr's own arm) and was declined here.
		if isConnStateFunc(name) {
			if len(x.Args) != 0 {
				return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
			}
			return nil
		}
		if !supportedFuncs[name] {
			return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
		}
		if min, max, ok := funcArity(name); ok {
			n := len(x.Args)
			if n < min || (max >= 0 && n > max) {
				return fmt.Errorf("%w: ORDER BY term with a whole-table aggregate (expression form)", errVDBEUnsupported)
			}
		}
	}
	for _, a := range x.Args {
		if err := validateOrderByExprResolves(c, a); err != nil {
			return err
		}
	}
	return nil
}

// compileScanGroupBy compiles a GROUP BY SELECT, possibly over a join. c already
// has every source's cursor and scope; this supplies emitJoinLoops' innermost
// body (WHERE, GROUP BY keys, every source's columns and rowid into one
// [cols.., rowids..] record; see aggPlan), then the sorted drain, group
// boundary detection, emitGroup and the optional ORDER BY sorter-2 drain.
//
// A trailing ORDER BY and SELECT DISTINCT both need a first-seen tiebreak. That
// is a scan-order sequence number incremented per row reaching sorter1, stored
// as a trailing [.., seq] column; emitJoinLoops visits rows in join.go's order,
// so it is faithful for any join. DISTINCT + GROUP BY collects all groups and
// resolves them in groupBatchFinal (vdbe_group_distinct.go).
func compileScanGroupBy(p *ReadOnlyPager, c *compiler, stmt *SelectStmt, srcs []joinSource, scopes []tableScope) (*Program, error) {
	// Same as compileScanAggregate: the select list and HAVING run in the drain
	// after sqlite3WhereEnd restored nQueryLoop, so distrust it for children; the
	// saved values serve withFromPlanTrust below.
	savedNQueryLoop, savedNQueryLoopKnown := c.nQueryLoop, c.nQueryLoopKnown
	c.nQueryLoopKnown = false
	useDistinctBatch := stmt.Distinct

	gp, perr := p.planGroupByStmt(stmt, scopes, nil, nil, c.outer)
	if perr != nil {
		// See compileScanAggregate's identical arm: an aggregate this query does
		// not own is hoisted to the enclosing query that does.
		if ae, ok := asAggAssociationError(perr); ok && hoistAggregatesOutward(c.outer, stmt, ae) {
			return nil, fmt.Errorf("%w: aggregate belongs to an enclosing query", errVDBEUnsupported)
		}
		return nil, declineOrSemantic(perr)
	}
	// Every column reference still standing in a rewritten item must be
	// resolvable at run time -- locally, or through the enclosing frames a
	// correlated sub-Program carries. See bindAggOuterRefs, and itemExprsOf
	// for the wrong answer this check exists to prevent.
	aggs, items := groupPlanRefs(gp)
	if !bindAggOuterRefs(c, aggs, items...) {
		return nil, fmt.Errorf("%w: column reference in a GROUP BY query that resolves to no reachable scope", errVDBEUnsupported)
	}
	// A select-list subquery is served (see compileScanAggregate): a correlated one
	// reads its group's anchor row, which aggResult threads in (each group's max(b)
	// row with max(), its first row with count(*)). gp.selectHasSubquery only keeps
	// this shape off the hash path.

	// The anchor row is reproducible only when the scan order is
	// (anchorPlanOrderProvable), unless the GROUP BY key makes the choice of row
	// unobservable (anchorIdentityFixed). An order-sensitive aggregate needs a
	// proven order too, since it reads no anchor (armOrderSensitive); it is asked
	// first so a query needing no proof skips the planner.
	//
	// When order is unprovable, the run-time anchor certificate
	// (anchor_certificate.go) replaces the decline for a top-level compile with no
	// ORDER BY, no subquery reading the anchor, and a single grouping term emitted
	// ascending under every plan. ORDER BY stays declined: sorter2's tiebreak does
	// not reproduce C's emission-order ties (select.c:740). gp.anchorObservable
	// first: a subquery reading only GROUP BY key columns reads nothing the anchor
	// decides (stmtReadsAnchorRow; cursorhint.test 6.0).
	var anchorChk *anchorCheck
	if gp.anchorObservable && anchorIsRead(gp.usesGroupBare, gp.selectHasSubquery, stmt) &&
		!anchorIdentityFixed(srcs, scopes, gp, stmt) &&
		!withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
			return anchorPlanOrderProvable(p, c, srcs, scopes, stmt)
		}) {
		chk, ok := anchorCheckFor(groupPlanItems(gp))
		if !ok || c.outer != nil || anchorSubqueryRead(gp.selectHasSubquery, stmt) || len(gp.orderPlans) > 0 ||
			!groupEmissionAscendingEverywhere(p, srcs, len(gp.groupExprs)) {
			return nil, errAnchorPlanOrder(!anchorNoIndexInPlay(p, srcs, scopes, stmt))
		}
		anchorChk = chk
	}
	armOrderSensitive(!anyOrderSensitive(aggs) ||
		withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
			return aggPlanOrderProvable(p, c, srcs, scopes, stmt)
		}), aggs)

	nGroup := len(gp.groupExprs)
	nCols := totalCols(srcs)
	nOut := len(gp.outCols)
	nOrder := len(gp.orderPlans)
	hasOrder := nOrder > 0

	// When the plan already delivers rows grouped (sqlite3WhereIsOrdered ==
	// nExpr), C opens no sorting index and emits groups in scan order. sorter1
	// always sorted ascending by key, wrong whenever the scan order is not
	// ascending; groupsArriveOutOfKeyOrder detects that and drops the sort. Only
	// observable without a trailing ORDER BY (sorter2 re-sorts); DISTINCT is
	// included since groupBatchFinal honours plan.scanOrder. If the planner
	// declined, there is no verdict; see groupEmissionOrderProvable.
	if !hasOrder && !withFromPlanTrust(c, savedNQueryLoop, savedNQueryLoopKnown, func() bool {
		return groupEmissionOrderProvable(p, c, srcs, scopes, gp, stmt)
	}) {
		return nil, fmt.Errorf("%w: a GROUP BY with no ORDER BY, over a table carrying an index that could deliver the grouping, in a statement the ported planner declined -- so WHICH ORDER THE GROUPS come out in (select.c's groupBySort==0 arm) is not reproducible", errVDBEUnsupported)
	}
	scanOrderGroups := !hasOrder && groupsArriveOutOfKeyOrder(srcs, scopes, gp)
	plan := &aggPlan{
		scopes:     scopes,
		nCols:      nCols,
		cursors:    cursorsOf(srcs),
		gathers:    gathersOf(srcs),
		outPlans:   gp.outPlans,
		havingPlan: gp.havingPlan,
		orderPlans: gp.orderPlans,

		magnet:      gp.magnet,
		anchorCheck: anchorChk,
	}
	// See compileScanAggregate's identical call: this is select.c:8746/:8747's
	// pair for the sorted GROUP BY drain -- finalizeAggFunctions, then HAVING
	// and the select list CODED against the registers it wrote. Done here so
	// the hash-aggregate path below inherits the same programs. Past
	// sqlite3WhereEnd too, so under the PRE-loop nQueryLoop.
	c.nQueryLoop, c.nQueryLoopKnown = savedNQueryLoop, savedNQueryLoopKnown
	itemErr := compileAggItemPrograms(c.pager, c, plan)
	c.nQueryLoopKnown = false
	if itemErr != nil {
		return nil, itemErr
	}

	// Hash-aggregate fast path (vdbe_hashagg.go): O(n) instead of sort+drain, only
	// when byte-identical (see compileScanGroupByHash). Excluded: DISTINCT,
	// ORDER BY other than ascending group key, non-BINARY key collations,
	// scan-ordered groups (hashAggSort drains ascending), select-list subqueries
	// and bare columns reading the anchor through state only the sorter path has,
	// and RIGHT/FULL coalesced columns in aggregate arguments, which only the drain
	// can serve. A reference to a GROUP BY key column is allowed: each bucket holds
	// a full aggAccumulators and magnetWalk runs per bucket in scan order.
	if !useDistinctBatch && (!hasOrder || orderIsGroupKeyAscending(stmt, gp)) &&
		!gp.usesGroupBare && !gp.selectHasSubquery &&
		!joinHasCoalescedColumns(plan.scopes) &&
		!anyNonBinaryCollation(gp.groupColls) && !scanOrderGroups {
		return compileScanGroupByHash(c, stmt, srcs, gp, plan, savedNQueryLoopKnown)
	}

	sorter1 := c.allocSorter()
	sorter2 := -1
	if hasOrder && !useDistinctBatch {
		sorter2 = c.allocSorter()
	}

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// nSortKey is select.c's groupBySort. nGroup: sorter1's key is the GROUP BY
	// tuple (groups adjacent, emitted in collated key order). 0: no key, so the
	// sorter's insertion-order tiebreak keeps scan order (scanOrderGroups proved
	// the grouping is already there). The payload is identical either way.
	// gp.groupColls is the grouping equality, on the comparator that groups and
	// orders; the stable tiebreak keeps each group's scan-first row.
	nSortKey := nGroup
	if scanOrderGroups {
		nSortKey = 0
	}
	// The grouping sort is ASCENDING unless the ORDER BY pushed its own DESC
	// bits onto the GROUP BY keys -- see groupPlan.groupDesc for
	// sqlite3CopySortOrder, the side effect that does it. gp.groupDesc is nil
	// for every statement where that did not happen, including all of the
	// scanOrderGroups and hash-aggregate ones (both require no ORDER BY at
	// all, and the copy requires one).
	desc1 := make([]bool, nSortKey)
	for i := range desc1 {
		if i < len(gp.groupDesc) {
			desc1[i] = gp.groupDesc[i]
		}
	}
	nulls1 := make([]NullsOrder, nSortKey)
	var coll1 []string
	if nSortKey > 0 {
		coll1 = collationNames(gp.groupColls)
	}
	c.emit(Instruction{Op: OpSorterOpen, P1: sorter1, P4: &sorterKeyInfo{nKey: nSortKey, desc: desc1, nulls: nulls1, coll: coll1}})
	if hasOrder && !useDistinctBatch {
		// sorter2 sorts finished groups by the ORDER BY keys, with the group's emission
		// ordinal as a final ascending tiebreak (C pushes groups into the ORDER BY sorter
		// in emission order). Unused with useDistinctBatch.
		desc2 := make([]bool, nOrder+1)
		nulls2 := make([]NullsOrder, nOrder+1)
		// coll2's trailing entry is the sequence-number tiebreak, always BINARY.
		coll2 := make([]string, nOrder+1)
		for i, op := range gp.orderPlans {
			desc2[i] = op.desc
			nulls2[i] = op.nulls
			coll2[i] = op.coll
		}
		c.emit(Instruction{Op: OpSorterOpen, P1: sorter2, P4: &sorterKeyInfo{nKey: nOrder + 1, desc: desc2, nulls: nulls2, coll: coll2}})
	}

	// seqCounterReg is the scan-order sequence counter (see this function's
	// doc comment): 0 before the scan starts, incremented by seqOneReg once
	// per row that passes WHERE and reaches sorter1, regardless of how many
	// tables are joined -- a faithful first-seen-scan-order tiebreak because
	// emitJoinLoops visits joined-row combinations in exactly the tree-
	// walker's own physical order (vdbe_join_codegen.go). Only ever READ
	// (via seqDrainReg/curEmitSeqReg below) when hasOrder or useDistinctBatch.
	seqCounterReg := c.alloc()
	seqOneReg := c.alloc()
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: seqCounterReg})
	c.emit(Instruction{Op: OpInteger, P1: 1, P2: seqOneReg})

	// Scan-phase record registers, kept contiguous so OpMakeRecord packs them
	// as one range [groupKeys.., cols.., rowids.., seq]: the sorter's key is
	// the leading nGroup group-key columns, its payload the rest ([cols..,
	// rowids.., seq], one rowid slot per joined table plus the trailing scan-
	// order sequence number). On the groupBySort==0 arm (nSortKey 0) the record
	// STARTS at colBase instead, so the payload is again the whole of it and the
	// drain's own offsets are untouched; the group keys are then neither stored
	// nor computed, exactly as select.c leaves them out of the un-sorted arm.
	keyBase := c.allocN(nSortKey)
	colBase := c.allocN(nCols)
	rowidBase := c.allocN(len(srcs))
	seqSlot := c.alloc()
	// groupValBase stores the group key again as trailing payload so the drain reads
	// it from the record instead of recomputing it, as C does (select.c:8581,
	// 8657). Needed because this engine's sorter returns only the payload from
	// data(), not the key columns. Written on both nSortKey arms so the payload
	// layout ([cols.., rowids.., seq, groupVals..]) is the same.
	groupValBase := c.allocN(nGroup)
	recReg := c.allocRec()
	recBase, recWidth := keyBase, nSortKey+nCols+len(srcs)+1+nGroup
	if nSortKey == 0 {
		recBase = colBase
		recWidth = nCols + len(srcs) + 1 + nGroup
	}
	// Where groupValBase's columns land in the PAYLOAD the drain reads
	// (aggRowSplit's [cols.., rowids..] block, then the sequence number).
	groupValPayloadOff := nCols + len(srcs) + 1

	// Predicate pushdown: see compileScanPlain (vdbe_scan.go). buckets prune at
	// their join level; this body tests only the deferred residue. (`plan` is
	// the aggregate plan here, so the pushdown plan is jplan.)
	jplan := joinPushdownPlan(srcs, scopes, stmt.Where)
	// Index-driven inner-side seeks (annotateJoinSeeks, vdbe_join_seek.go): order-
	// preserving, so GROUP BY grouping and order-sensitive aggregates are
	// unaffected.
	annotateJoinSeeks(c, srcs, jplan)
	deferredWhere := andConjuncts(jplan.deferred)

	body := func() error {
		whereJump := -1
		if deferredWhere != nil {
			// See compileScanAggregate's identical guard just above and
			// compiler.inWhereConjunct's doc comment: deferredWhere is real
			// top-level WHERE AND-conjuncts, so tag-20220128a's WHERE-position
			// rule applies here too.
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}
		// Each GROUP BY term is compiled ONCE and copied to both destinations:
		// the sorter's key columns (only on the groupBySort==1 arm, where they
		// are the sort key) and the payload block the drain reads back. One
		// evaluation, one value -- so the order the rows are grouped in and the
		// key they are grouped BY can never come from two different evaluators.
		for i, ge := range gp.groupExprs {
			r, gerr := c.compileExpr(ge)
			if gerr != nil {
				return gerr
			}
			if nSortKey > 0 {
				c.emit(Instruction{Op: OpSCopy, P1: r, P2: keyBase + i})
			}
			c.emit(Instruction{Op: OpSCopy, P1: r, P2: groupValBase + i})
		}
		for _, s := range srcs {
			for j := 0; j < len(s.tbl.cols); j++ {
				c.emit(Instruction{Op: OpColumn, P1: s.scope.cursor, P2: j, P3: colBase + s.scope.offset + j})
			}
		}
		for i, s := range srcs {
			c.emit(Instruction{Op: OpRowid, P1: s.scope.cursor, P2: rowidBase + i})
		}
		// This row's current scan-order sequence number, then advance the
		// counter for the next row that reaches here.
		c.emit(Instruction{Op: OpSCopy, P1: seqCounterReg, P2: seqSlot})
		c.emit(Instruction{Op: OpAdd, P1: seqOneReg, P2: seqCounterReg, P3: seqCounterReg})
		c.emit(Instruction{Op: OpMakeRecord, P1: recBase, P2: recWidth, P3: recReg})
		c.emit(Instruction{Op: OpSorterInsert, P1: sorter1, P2: recReg})

		if whereJump >= 0 {
			c.patch(whereJump, c.here())
		}
		return nil
	}
	// The scan body runs with every cursor on the current row, so a correlated
	// subquery in the WHERE or grouping keys may read them (rowLive), as in
	// compileScanPlain (e_fkey.test). Restored afterwards: the result phase is not
	// row-live.
	savedRowLive := c.rowLive
	c.rowLive = true
	err := aggLoopNQueryLoop(c, savedNQueryLoopKnown, func() error { return emitJoinLoops(c, srcs, jplan, body) })
	c.rowLive = savedRowLive
	if err != nil {
		return nil, err
	}

	// Drain-phase registers.
	rowRec := c.allocRec()    // sorter1 payload: [cols.., rowids.., seq, groupVals..]
	rowKeyRec := c.allocRec() // this row's group key, read back out of rowRec
	drainKeyBase := c.allocN(nGroup)
	curKeyRec := c.allocRec() // the current group's key
	seqDrainReg := c.alloc()  // this row's scan-order sequence number; only ever read when hasOrder or useDistinctBatch
	// curEmitSeqReg is the group's emission ordinal (1, 2, ...), the ORDER BY
	// tiebreak: C's ORDER BY sorter is stable over emission order. A minimum
	// scan-sequence tiebreak got "GROUP BY g ORDER BY 'x'" over (2),(1) wrong.
	// sorter1 still carries the scan sequence, which keeps each group's anchor the
	// scan-first row.
	curEmitSeqReg := c.alloc()
	emitSeqOneReg := c.alloc()
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: curEmitSeqReg})
	c.emit(Instruction{Op: OpInteger, P1: 1, P2: emitSeqOneReg})
	haveGroupReg := c.alloc() // 0 until the first group starts
	havingReg := -1
	if gp.havingPlan != nil {
		havingReg = c.alloc()
	}

	// Output/sort registers. For the ORDER BY case (non-DISTINCT) the sorter-2
	// record must be contiguous [orderKeys.., minSeq, out..], so outBase
	// lives inside that block. For useDistinctBatch, outBase is always its own
	// standalone block (the batch entry is assembled separately -- see
	// batchBase below -- since it also needs the group's OWN key columns,
	// which live in a record register, copied out via OpRecordColumn, not a
	// plain register range).
	var outBase, sort2Base, minSeqSlot, rec2Reg, finalRec, finalBase int
	var orderKeyBase, batchBase, batchWidth, batchRecReg int
	switch {
	case useDistinctBatch:
		outBase = c.allocN(nOut)
		if hasOrder {
			orderKeyBase = c.allocN(nOrder)
		}
		batchWidth = 1 + nGroup + nOut
		if hasOrder {
			batchWidth += nOrder
		}
		batchBase = c.allocN(batchWidth)
		batchRecReg = c.allocRec()
	case hasOrder:
		sort2Base = c.allocN(nOrder + 1 + nOut)
		minSeqSlot = sort2Base + nOrder
		outBase = sort2Base + nOrder + 1
		rec2Reg = c.allocRec()
		finalRec = c.allocRec()
		finalBase = c.allocN(nOut)
	default:
		outBase = c.allocN(nOut)
	}

	// LIMIT/OFFSET counters. For the unordered, non-DISTINCT case they gate the
	// group loop's ResultRow directly (groups already drain in group-key order
	// == keyTupleLess order). For the ORDER BY case they gate
	// the sorter-2 drain instead (LIMIT/OFFSET apply after ordering); set up
	// there. useDistinctBatch needs neither: groupBatchFinal applies LIMIT/
	// OFFSET itself, directly from stmt, in its single Go-side pass.
	var offsetReg, limitReg, oneReg int
	haveOffset, haveLimit := false, false
	if !hasOrder && !useDistinctBatch {
		offsetReg, limitReg, oneReg, haveOffset, haveLimit = c.emitLimitOffsetCounters(stmt)
	}

	// emitGroup emits one group's finalize+output, using curKeyRec as the group
	// key and curEmitSeqReg as its emission ordinal (the ORDER BY tiebreak).
	// It is emitted twice (at a boundary, and for the final group),
	// self-contained each time.
	emitGroup := func() {
		for i := 0; i < nOut; i++ {
			c.emit(Instruction{Op: OpAggResult, P1: outBase + i, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetOut, idx: i}})
		}
		var skipJumps []int
		if gp.havingPlan != nil {
			c.emit(Instruction{Op: OpAggResult, P1: havingReg, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetHaving, idx: 0}})
			// HAVING false or NULL -> skip this group's output.
			skipJumps = append(skipJumps, c.emit(Instruction{Op: OpIfNot, P1: havingReg, P3: 1}))
		}
		switch {
		case useDistinctBatch:
			// Collect this group's entry -- [minSeq, groupKey.., out..,
			// orderKeys..] -- into the batch groupBatchFinal resolves once the
			// whole drain is done (see this function's doc comment and
			// vdbe_group_distinct.go). Order-key values, if any, are computed
			// fresh here (mirroring the hasOrder branch below) into their own
			// register block rather than sort2Base/rec2Reg, which this branch
			// never allocates.
			if hasOrder {
				for i, op := range gp.orderPlans {
					if op.ordinal > 0 {
						c.emit(Instruction{Op: OpSCopy, P1: outBase + (op.ordinal - 1), P2: orderKeyBase + i})
					} else {
						c.emit(Instruction{Op: OpAggResult, P1: orderKeyBase + i, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetOrder, idx: i}})
					}
				}
			}
			c.emit(Instruction{Op: OpSCopy, P1: curEmitSeqReg, P2: batchBase})
			for i := 0; i < nGroup; i++ {
				c.emit(Instruction{Op: OpRecordColumn, P1: curKeyRec, P2: i, P3: batchBase + 1 + i})
			}
			for i := 0; i < nOut; i++ {
				c.emit(Instruction{Op: OpSCopy, P1: outBase + i, P2: batchBase + 1 + nGroup + i})
			}
			if hasOrder {
				for i := 0; i < nOrder; i++ {
					c.emit(Instruction{Op: OpSCopy, P1: orderKeyBase + i, P2: batchBase + 1 + nGroup + nOut + i})
				}
			}
			c.emit(Instruction{Op: OpMakeRecord, P1: batchBase, P2: batchWidth, P3: batchRecReg})
			c.emit(Instruction{Op: OpGroupBatchAppend, P1: batchRecReg})
		case hasOrder:
			for i, op := range gp.orderPlans {
				if op.ordinal > 0 {
					c.emit(Instruction{Op: OpSCopy, P1: outBase + (op.ordinal - 1), P2: sort2Base + i})
				} else {
					c.emit(Instruction{Op: OpAggResult, P1: sort2Base + i, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetOrder, idx: i}})
				}
			}
			c.emit(Instruction{Op: OpSCopy, P1: curEmitSeqReg, P2: minSeqSlot})
			c.emit(Instruction{Op: OpMakeRecord, P1: sort2Base, P2: nOrder + 1 + nOut, P3: rec2Reg})
			c.emit(Instruction{Op: OpSorterInsert, P1: sorter2, P2: rec2Reg})
		default:
			skipJumps = append(skipJumps, c.emitLimitOffsetGate(offsetReg, limitReg, oneReg, haveOffset, haveLimit, outBase, nOut)...)
		}
		skip := c.here()
		for _, j := range skipJumps {
			c.patch(j, skip)
		}
	}

	sortJump := c.emit(Instruction{Op: OpSorterSort, P1: sorter1})
	c.emit(Instruction{Op: OpInteger, P1: 0, P2: haveGroupReg})
	drainLoop := c.here()
	// P3 == nCols: the [cols..] block drops its JSON subtype on the way out, as a
	// grouped row does in C (serialized sorter record). Done once per row at the
	// leaves, so drain-lowered expressions may generate or consume subtypes freely.
	// The hash path does the same per register (OpClearSubtype).
	c.emit(Instruction{Op: OpSorterData, P1: sorter1, P2: rowRec, P3: nCols})
	// This row's group key, READ from the record the scan body already put it
	// in -- select.c:8657's "OP_Column sortPTab, j, iBMem+j", one column read
	// per GROUP BY term, then packed into the record OpGroupSame/OpRecCopy
	// compare and keep. There is no re-evaluation here and no aggPlan payload:
	// the opcode that used to walk gp.groupExprs per drained row is gone.
	for i := 0; i < nGroup; i++ {
		c.emit(Instruction{Op: OpRecordColumn, P1: rowRec, P2: groupValPayloadOff + i, P3: drainKeyBase + i})
	}
	c.emit(Instruction{Op: OpMakeRecord, P1: drainKeyBase, P2: nGroup, P3: rowKeyRec})
	c.emit(Instruction{Op: OpRecordColumn, P1: rowRec, P2: nCols + len(srcs), P3: seqDrainReg})
	jFirst := c.emit(Instruction{Op: OpIfNot, P1: haveGroupReg}) // first row -> start group, no emit
	jSame := c.emit(Instruction{Op: OpGroupSame, P1: rowKeyRec, P3: curKeyRec, P4: collationNames(gp.groupColls)})
	// Boundary: emit the group that just ended.
	emitGroup()
	// Start a new group (also the target of jFirst).
	startGroup := c.here()
	c.patch(jFirst, startGroup)
	c.emit(Instruction{Op: OpAggReset, P4: plan})
	c.emit(Instruction{Op: OpRecCopy, P1: rowKeyRec, P2: curKeyRec})
	c.emit(Instruction{Op: OpAdd, P1: emitSeqOneReg, P2: curEmitSeqReg, P3: curEmitSeqReg})
	c.emit(Instruction{Op: OpInteger, P1: 1, P2: haveGroupReg})
	sameGroup := c.here()
	c.patch(jSame, sameGroup)
	// Lower this row's per-row expressions off the sorter record, so
	// aggItem.step reads VALUES here too. This is select.c:8704 --
	// updateAccumulator called from INSIDE the drain loop, with
	// pAggInfo->useSortingIdx set (select.c:8607) so each column leaf comes
	// from the record rather than from a cursor that closed with the scan. See
	// aggDrainRow, which owns the whole rule; grouped is true because a sorted
	// GROUP BY's rows are exactly the ones clearJSONSubtype describes.
	emitDrainStep := func(seg int) {
		c.emit(Instruction{Op: OpAggStep, P1: rowRec, P2: seg, P3: 1, P4: plan})
	}
	if drain := newAggDrainRow(c, rowRec, srcs, scopes, nCols); drain != nil {
		mark := len(c.insns)
		drain.push(c)
		ap, apErr := planAggArgRegs(c, plan, true, srcs, drain)
		if apErr != nil {
			drain.pop(c)
			return nil, apErr
		}
		drain.emitLoads(c, ap.lowered)
		// The step opcodes are emitted INSIDE the checked region -- that is
		// what interleaving means -- and OpAggStep is on aggDrainOpForbidden's
		// list for the routes that are not this one, so the ones emitted here
		// are recorded and skipped by the scan below. They are record-mode
		// (P3==1) reads of the sorter payload, which is the drain's whole point.
		steps := map[int]bool{}
		stepErr := emitAggSteps(c, ap, func(seg int) {
			steps[len(c.insns)] = true
			emitDrainStep(seg)
		})
		drain.pop(c)
		if stepErr != nil {
			return nil, stepErr
		}
		for i := mark; i < len(c.insns); i++ {
			if steps[i] {
				continue
			}
			// level 1: a sub-Program launched from this block runs on a machine
			// whose PARENT is this one (see aggDrainInsnForbidden).
			if aggDrainInsnForbidden(c.insns[i], 1) {
				// The block reads a cursor the scan closed. There is no
				// rollback to a second evaluator any more, so this is the
				// statement's error: emitting it anyway would read whatever
				// row the exhausted cursor was left on, which invariant 2
				// ranks below an honest failure.
				return nil, fmt.Errorf("%w: a sorted GROUP BY drain's aggregate operand reads a cursor the scan closed (%v)", errVDBEUnsupported, c.insns[i].Op)
			}
		}
	} else if err := emitDrainStepsWithoutRow(c, plan, emitDrainStep); err != nil {
		return nil, err
	}
	c.emit(Instruction{Op: OpSorterNext, P1: sorter1, P2: drainLoop})

	// After the drain: emit the final group (unless there were no rows at all).
	jNoLast := c.emit(Instruction{Op: OpIfNot, P1: haveGroupReg})
	emitGroup()
	afterGroups := c.here()
	c.patch(jNoLast, afterGroups)
	c.patch(sortJump, afterGroups) // empty sorter1 -> zero groups -> here

	switch {
	case useDistinctBatch:
		var orderDesc []bool
		var orderNulls []NullsOrder
		var orderColl []string
		if hasOrder {
			orderDesc = make([]bool, nOrder)
			orderNulls = make([]NullsOrder, nOrder)
			orderColl = make([]string, nOrder)
			for i, op := range gp.orderPlans {
				orderDesc[i] = op.desc
				orderNulls[i] = op.nulls
				orderColl[i] = op.coll
			}
		}
		c.emit(Instruction{Op: OpGroupBatchFinal, P4: &groupBatchPlan{
			nGroup:     nGroup,
			nOut:       nOut,
			nOrder:     nOrder,
			hasOrder:   hasOrder,
			orderDesc:  orderDesc,
			orderNulls: orderNulls,
			orderColl:  orderColl,
			outColl:    gp.outColls,
			scanOrder:  scanOrderGroups,
			limit:      stmt.Limit,
			offset:     stmt.Offset,
		}})
	case hasOrder:
		c.emitOrderedDrain(stmt, sorter2, finalRec, finalBase, nOut)
	}
	c.emit(Instruction{Op: OpHalt})

	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NCursors:   c.nCursor,
		NRecRegs:   c.nRec,
		NSorters:   c.nSorter,
		NSubCache:  c.nSub,
		NResultCol: nOut,
		ColNames:   outColNames(gp.outCols),
		Correlated: c.correlated,
	}, nil
}

// compileScanGroupByHash compiles the GROUP BY hash fast path (vdbe_hashagg.go),
// taken only when byte-identical to the sorter path: no DISTINCT, no trailing
// ORDER BY (it needs sorter2's tiebreak), BINARY key collations, and no
// select-list subquery. Then hashAggKeyBytes groups exactly as GROUP BY's
// equality does, rows step into buckets in scan order (so order-sensitive
// aggregates match), and hashAggSort drains in sorter1's key order.
//
// The body is WHERE, the GROUP BY keys packed into one record for the bucket
// lookup, then OpHashAggStep, which reads the row from the positioned cursors
// like OpAggStep's P3==0 mode, so no payload record is built.
func compileScanGroupByHash(c *compiler, stmt *SelectStmt, srcs []joinSource, gp *groupByPlan, plan *aggPlan, preNQLKnown bool) (*Program, error) {
	nGroup := len(gp.groupExprs)
	nOut := len(gp.outCols)

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	// Scan-phase registers: the group-key columns, packed into their own
	// small record (keyRec) purely so OpHashAggStep can key its bucket
	// lookup/creation from one register rather than a P1..P1+nGroup-1 range.
	keyBase := c.allocN(nGroup)
	keyRec := c.allocRec()

	// Predicate pushdown / index-driven seeks: identical to compileScanGroupBy's
	// own setup (see there for the doc comment on both).
	jplan := joinPushdownPlan(srcs, plan.scopes, stmt.Where)
	annotateJoinSeeks(c, srcs, jplan)
	deferredWhere := andConjuncts(jplan.deferred)

	// See compileScanAggregate's identical pair: reserved once here, computed
	// into from a body a LEFT/FULL JOIN emits more than once.
	loweredArgs, argsErr := planAggArgRegs(c, plan, true, srcs, nil)
	if argsErr != nil {
		return nil, argsErr
	}

	body := func() error {
		whereJump := -1
		if deferredWhere != nil {
			// See compileScanGroupBy's identical guard and
			// compiler.inWhereConjunct's doc comment: deferredWhere is real
			// top-level WHERE AND-conjuncts, so tag-20220128a's WHERE-position
			// rule applies here too.
			c.inWhereConjunct = true
			wReg, werr := c.compileExpr(deferredWhere)
			if werr != nil {
				return werr
			}
			whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
		}
		for i, ge := range gp.groupExprs {
			r, gerr := c.compileExpr(ge)
			if gerr != nil {
				return gerr
			}
			c.emit(Instruction{Op: OpSCopy, P1: r, P2: keyBase + i})
		}
		c.emit(Instruction{Op: OpMakeRecord, P1: keyBase, P2: nGroup, P3: keyRec})
		if err := emitAggSteps(c, loweredArgs, func(seg int) {
			c.emit(Instruction{Op: OpHashAggStep, P1: keyRec, P2: seg, P4: plan})
		}); err != nil {
			return err
		}

		if whereJump >= 0 {
			c.patch(whereJump, c.here())
		}
		return nil
	}
	// The scan body runs with every cursor on the current row, so a correlated
	// subquery in the WHERE or grouping keys may read them (rowLive), as in
	// compileScanPlain (e_fkey.test). Restored afterwards: the result phase is not
	// row-live.
	savedRowLive := c.rowLive
	c.rowLive = true
	err := aggLoopNQueryLoop(c, preNQLKnown, func() error { return emitJoinLoops(c, srcs, jplan, body) })
	c.rowLive = savedRowLive
	if err != nil {
		return nil, err
	}

	// Drain phase: one bucket at a time, in group-key-ascending order.
	curKeyRec := c.allocRec()
	outBase := c.allocN(nOut)
	havingReg := -1
	if gp.havingPlan != nil {
		havingReg = c.alloc()
	}
	offsetReg, limitReg, oneReg, haveOffset, haveLimit := c.emitLimitOffsetCounters(stmt)

	sortJump := c.emit(Instruction{Op: OpHashAggSort})
	drainLoop := c.here()
	c.emit(Instruction{Op: OpHashAggData, P2: curKeyRec})
	for i := 0; i < nOut; i++ {
		c.emit(Instruction{Op: OpAggResult, P1: outBase + i, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetOut, idx: i}})
	}
	var skipJumps []int
	if gp.havingPlan != nil {
		c.emit(Instruction{Op: OpAggResult, P1: havingReg, P3: curKeyRec, P4: &aggResultInfo{plan: plan, set: aggSetHaving, idx: 0}})
		// HAVING false or NULL -> skip this group's output.
		skipJumps = append(skipJumps, c.emit(Instruction{Op: OpIfNot, P1: havingReg, P3: 1}))
	}
	skipJumps = append(skipJumps, c.emitLimitOffsetGate(offsetReg, limitReg, oneReg, haveOffset, haveLimit, outBase, nOut)...)
	skip := c.here()
	for _, j := range skipJumps {
		c.patch(j, skip)
	}
	c.emit(Instruction{Op: OpHashAggNext, P2: drainLoop})
	afterDrain := c.here()
	c.patch(sortJump, afterDrain)

	c.emit(Instruction{Op: OpHalt})

	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NCursors:   c.nCursor,
		NRecRegs:   c.nRec,
		NSubCache:  c.nSub,
		NResultCol: nOut,
		ColNames:   outColNames(gp.outCols),
		Correlated: c.correlated,
	}, nil
}

// emitLimitOffsetCounters loads the LIMIT/OFFSET running counters (a positive
// OFFSET, a non-negative LIMIT, and a shared constant 1) into fresh registers,
// exactly like compileScanPlain/compileScanSorted. It returns the register
// numbers and which counters are live.
func (c *compiler) emitLimitOffsetCounters(stmt *SelectStmt) (offsetReg, limitReg, oneReg int, haveOffset, haveLimit bool) {
	if stmt.Offset != nil && *stmt.Offset > 0 {
		haveOffset = true
		offsetReg = c.alloc()
		c.emit(literalIntInstr(*stmt.Offset, offsetReg))
	}
	if stmt.Limit != nil && *stmt.Limit >= 0 {
		haveLimit = true
		limitReg = c.alloc()
		c.emit(literalIntInstr(*stmt.Limit, limitReg))
	}
	if haveOffset || haveLimit {
		oneReg = c.alloc()
		c.emit(literalIntInstr(1, oneReg))
	}
	return
}

// emitLimitOffsetGate emits the per-output-row LIMIT/OFFSET gate for the
// unordered GROUP BY case: skip while OFFSET is positive (decrementing it),
// then output while LIMIT is positive (decrementing it), else skip. It emits
// the OpResultRow itself and returns the jump addresses that must be patched to
// the "skip this row" point (the caller patches them once it knows that
// address). Unlike compileScanPlain, it never breaks the drain early on LIMIT
// exhaustion -- every group's output is still finalized, just not emitted.
func (c *compiler) emitLimitOffsetGate(offsetReg, limitReg, oneReg int, haveOffset, haveLimit bool, outBase, nOut int) []int {
	var skipJumps []int
	if haveOffset {
		offHas := c.emit(Instruction{Op: OpIf, P1: offsetReg}) // offset!=0 -> skip this row
		offPast := c.emit(Instruction{Op: OpGoto})             // offset==0 -> fall to LIMIT
		c.patch(offHas, c.here())
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: offsetReg, P3: offsetReg})
		skipJumps = append(skipJumps, c.emit(Instruction{Op: OpGoto})) // -> skip
		c.patch(offPast, c.here())
	}
	if haveLimit {
		limHas := c.emit(Instruction{Op: OpIf, P1: limitReg})          // limit!=0 -> output
		skipJumps = append(skipJumps, c.emit(Instruction{Op: OpGoto})) // limit==0 -> skip
		c.patch(limHas, c.here())
	}
	c.emit(Instruction{Op: OpResultRow, P1: outBase, P2: nOut})
	if haveLimit {
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: limitReg, P3: limitReg})
	}
	return skipJumps
}

// emitOrderedDrain emits the sorter-2 drain for a GROUP BY + ORDER BY query:
// sort the group outputs by their ORDER BY keys (with the group's min-rowid as
// a final tiebreak column, reproducing a stable sort over first-seen order),
// then emit each as a result row, LIMIT/OFFSET-gated exactly
// like compileScanSorted's drain (here an early stop on LIMIT is fine -- this
// is the final ordering).
func (c *compiler) emitOrderedDrain(stmt *SelectStmt, sorter2, finalRec, finalBase, nOut int) {
	offsetReg, limitReg, oneReg, haveOffset, haveLimit := c.emitLimitOffsetCounters(stmt)

	sortJump := c.emit(Instruction{Op: OpSorterSort, P1: sorter2})
	drainLoop := c.here()

	offGotoNext := -1
	if haveOffset {
		offHas := c.emit(Instruction{Op: OpIf, P1: offsetReg})
		offPast := c.emit(Instruction{Op: OpGoto})
		c.patch(offHas, c.here())
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: offsetReg, P3: offsetReg})
		offGotoNext = c.emit(Instruction{Op: OpGoto})
		c.patch(offPast, c.here())
	}
	limExhausted := -1
	if haveLimit {
		limHas := c.emit(Instruction{Op: OpIf, P1: limitReg})
		limExhausted = c.emit(Instruction{Op: OpGoto})
		c.patch(limHas, c.here())
	}

	c.emit(Instruction{Op: OpSorterData, P1: sorter2, P2: finalRec})
	for i := 0; i < nOut; i++ {
		c.emit(Instruction{Op: OpRecordColumn, P1: finalRec, P2: i, P3: finalBase + i})
	}
	c.emit(Instruction{Op: OpResultRow, P1: finalBase, P2: nOut})
	if haveLimit {
		c.emit(Instruction{Op: OpSubtract, P1: oneReg, P2: limitReg, P3: limitReg})
	}
	sorterNext := c.emit(Instruction{Op: OpSorterNext, P1: sorter2, P2: drainLoop})
	if offGotoNext >= 0 {
		c.patch(offGotoNext, sorterNext)
	}
	drainEnd := c.here()
	c.patch(sortJump, drainEnd)
	if limExhausted >= 0 {
		c.patch(limExhausted, drainEnd)
	}
}

// outColNames extracts the result column names from an expanded select list.
func outColNames(outCols []outputColumn) []string {
	names := make([]string, len(outCols))
	for i, oc := range outCols {
		names[i] = oc.name
	}
	return names
}

// compileNoFromAggregate compiles a FROM-less SELECT with an aggregate
// ("SELECT count(*)", "SELECT sum(5)+1"). outer is the enclosing compile for a
// subquery body, nil at top level. It behaves as a whole-table aggregate over
// one implicit row with no columns: a false WHERE leaves every aggregate with
// zero rows ("SELECT count(*) WHERE 1=0" is 0), and there is always exactly one
// output row. Items are planned with planNoGroupAggregate and nil scopes, so a
// bare column is rejected. ORDER BY declines.
func compileNoFromAggregate(pager *ReadOnlyPager, stmt *SelectStmt, outer *compiler) (*Program, error) {
	if len(stmt.OrderBy) != 0 {
		return nil, fmt.Errorf("%w: ORDER BY with a FROM-less aggregate", errVDBEUnsupported)
	}
	if stmt.Having != nil {
		return nil, fmt.Errorf("%w: HAVING without FROM", errVDBEUnsupported)
	}
	for _, sc := range stmt.Columns {
		if sc.Star {
			return nil, fmt.Errorf("%w: \"*\" with a FROM-less aggregate", errVDBEUnsupported)
		}
	}

	outCols, items, allAggs, _, usesGroupBare, perr := planNoGroupAggregate(stmt, nil, nil, pager.colNameMode(), outer)
	if perr != nil {
		// A FROM-less body has no FROM at all, so sqlite3ReferencesSrcList can
		// never answer 1 here: every aggregate in one either names nothing (its
		// -1, which planNoGroupAggregate accepts) or belongs to an ENCLOSING
		// query. The latter is hoisted there when that query can be found --
		// "SELECT (SELECT y FROM (SELECT sum(x) AS y)) FROM t1" is one row of
		// sum(x) over t1 in C SQLite. See vdbe_agg_hoist.go.
		if ae, ok := asAggAssociationError(perr); ok && hoistAggregatesOutward(outer, stmt, ae) {
			return nil, fmt.Errorf("%w: aggregate belongs to an enclosing query", errVDBEUnsupported)
		}
		// A real planning error (bare column reference -- there is no FROM
		// clause for one to name a column of -- wrong-arity aggregate, ...):
		// a genuine rejection, never silently guessed at.
		return nil, declineOrSemantic(perr)
	}
	if usesGroupBare {
		// See compileScanAggregate's identical decline. In practice this
		// never fires here (the bare-column anchor extension still needs a
		// real column to resolve against, which nil scopes can never
		// provide), but declined defensively rather than assumed impossible.
		return nil, fmt.Errorf("%w: bare column resolved via the aggregate anchor row", errVDBEUnsupported)
	}
	// A subquery in the select list is not declined here either (see
	// compileScanAggregate): with no FROM clause there is nothing local for
	// one to correlate to, so it is evaluated exactly like any other
	// row-independent expression at OpAggResult time -- "SELECT count(*),
	// (SELECT 5)" is (1, 5), verified directly against C SQLite.

	// scopes/cursors both nil, nCols 0: the single synthetic row contributes
	// no columns of its own -- see this function's doc comment.
	plan := &aggPlan{outPlans: items}
	// trig from the enclosing chain: a FROM-less aggregate inside a trigger
	// body may name NEW./OLD. ("INSERT INTO tlog2 VALUES((SELECT sum(new.a)))"),
	// and with no trigger context compileColumn has nothing to resolve it
	// against. Same inheritance compileScanAttempt does.
	c := &compiler{pager: pager, outer: outer, trig: enclosingTriggerCtx(outer)}
	if err := compileAggItemPrograms(pager, c, plan); err != nil {
		return nil, err
	}

	// A FROM-less aggregate's select list IS its whole body (no separate WHERE
	// loop at all), evaluated at OpAggResult time -- there is no loop for a
	// nested subquery here to see the contribution of, so nQueryLoop starts
	// distrusted unconditionally rather than inherited from outer. See
	// compiler.nQueryLoopKnown.
	c.nQueryLoopKnown = false
	// With no FROM, a remaining column reference must come from an enclosing
	// compile: rejected at top level, bound to the enclosing cursor in a subquery
	// ("SELECT (SELECT count(*)+a1) FROM t1" is 2,3,4 over a1=1,2,3; count(*) stays
	// in the subquery by C's association rule).
	if !bindAggOuterRefs(c, allAggs, itemExprsOf(items)...) {
		return nil, fmt.Errorf("%w: column reference in a FROM-less aggregate", errVDBEUnsupported)
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpAggReset, P4: plan})

	resultBase := c.allocN(len(outCols))

	// WHERE gates whether the one synthetic row is stepped into the
	// accumulators at all (a false/NULL WHERE steps zero rows -- e.g.
	// "SELECT count(*) WHERE 1=0" is 0, not zero output rows) -- it never
	// skips the AggResult finalize/eval below, exactly like
	// compileScanAggregate: the query still always yields exactly one
	// output row.
	whereJump := -1
	if stmt.Where != nil {
		// A FROM-less WHERE still gets the vector-equality split (where.c:6941, 6990);
		// see compiler.inWhereConjunct.
		c.inWhereConjunct = true
		wReg, werr := c.compileExpr(stmt.Where)
		if werr != nil {
			return nil, werr
		}
		whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	}
	// The one synthetic row's per-row expressions are computed into registers
	// here, exactly as compileScanAggregate's scan body does it (planAggArgRegs
	// / emitAggArgRegs), and INSIDE the WHERE gate for the same reason: a row
	// the WHERE rejected is never stepped, so its arguments are never
	// evaluated. Only one body is emitted here -- there is no join, so none of
	// the LEFT/RIGHT re-emission planAggArgRegs' doc comment is about -- but
	// the split is kept so both shapes go through one pair of functions.
	loweredArgs, argsErr := planAggArgRegs(c, plan, false, nil, nil)
	if argsErr != nil {
		return nil, argsErr
	}
	if err := emitAggSteps(c, loweredArgs, func(seg int) {
		c.emit(Instruction{Op: OpAggStep, P2: seg, P3: 0, P4: plan})
	}); err != nil {
		return nil, err
	}
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}

	for i := range outCols {
		c.emit(Instruction{Op: OpAggResult, P1: resultBase + i, P3: -1, P4: &aggResultInfo{plan: plan, set: aggSetOut, idx: i}})
	}
	skipRow := (stmt.Offset != nil && *stmt.Offset >= 1) || (stmt.Limit != nil && *stmt.Limit == 0)
	if !skipRow {
		c.emit(Instruction{Op: OpResultRow, P1: resultBase, P2: len(outCols)})
	}
	c.emit(Instruction{Op: OpHalt})

	return &Program{
		Insns:      c.insns,
		NReg:       c.nReg,
		NSubCache:  c.nSub,
		NResultCol: len(outCols),
		ColNames:   outColNames(outCols),
		Correlated: c.correlated,
	}, nil
}

// itemExprsOf flattens the planned items' rewritten expressions for
// bindAggOuterRefs. What remains as ColumnExpr is what planGroupByStmt passed
// through as correlated or bad, so walking them is required: otherwise "HAVING
// count(*)<z" with no z anywhere answered zero rows instead of "no such column"
// (select5.test).
func itemExprsOf(items []*itemPlan) []Expr {
	out := make([]Expr, 0, len(items))
	for _, it := range items {
		if it != nil {
			out = append(out, it.rewritten)
		}
	}
	return out
}

// groupPlanItems is every itemPlan a GROUP BY query planned, in one slice: the
// select list, then HAVING, then the non-ordinal ORDER BY terms. All three read
// the same anchor row, so anything that reasons about the anchor has to see all
// three (anchorIdentityFixed) -- as does bindAggOuterRefs, through groupPlanRefs.
func groupPlanItems(gp *groupByPlan) []*itemPlan {
	items := make([]*itemPlan, 0, len(gp.outPlans)+1+len(gp.orderPlans))
	for _, it := range gp.outPlans {
		if it != nil {
			items = append(items, it)
		}
	}
	if gp.havingPlan != nil {
		items = append(items, gp.havingPlan)
	}
	for _, op := range gp.orderPlans {
		if op.item != nil {
			items = append(items, op.item)
		}
	}
	return items
}

// groupsArriveOutOfKeyOrder reports whether the chosen access path (the
// outermost level's idxOrderKey) already delivers the GROUP BY grouping in an
// order other than ascending key, select.c's groupBySort==0 arm. For a GROUP BY,
// wherePathSatisfiesOrderBy matches terms in any order and ignores direction,
// so a DESC index or a permuted one groups and emits non-ascending (with
// "CREATE INDEX i ON t(a DESC)", "GROUP BY a" emits 3,2,1 in C).
//
// A join reaches the same arm: the outer loop alone can satisfy the GROUP BY
// (where.c:5216). The verdict is the one wherePlanOrder stamped on every level,
// and the ascending test reads the outermost key.
func groupsArriveOutOfKeyOrder(srcs []joinSource, scopes []tableScope, gp *groupByPlan) bool {
	if len(srcs) == 0 {
		return false
	}
	outer := 0
	if len(srcs) > 1 {
		jts := joinedTablesFor(srcs)
		if hasRightOuter(jts) {
			return false // computeExecOrder answers FROM order and never asks the port
		}
		order, _, ok := sqliteExecOrder(jts, scopes, nil)
		if !ok {
			return false
		}
		outer = order[0]
	}
	src := &srcs[outer]
	if src.tbl == nil {
		return false
	}
	key := src.idxOrderKey
	n := len(gp.groupExprs)
	// A nil key is the rowid scan, ascending. key.groupsSorted is
	// wherePathSatisfiesOrderBy's own answer for the winning loop
	// (where_plan_index.go), including the pre-loop pinned terms and the nEq skip;
	// an earlier re-derivation here got many eq-pinned shapes wrong.
	if key == nil || n == 0 || !key.groupsSorted {
		return false
	}
	// Each GROUP BY term must resolve to a column of the outer table;
	// resolveColumnIndex gives an absolute index (rebased for a join), and a rowid
	// or IPK maps to xnRowid.
	gcol := make([]int, n)
	for i, ge := range gp.groupExprs {
		ce, ok := ge.(ColumnExpr)
		if !ok {
			return false
		}
		idx, ok := resolveColumnIndex(scopes, ce)
		if !ok {
			return false
		}
		switch {
		case idx <= rowidIdentityBase:
			if len(srcs) > 1 && rowidIdentityBase-idx != outer {
				return true
			}
			idx = xnRowid
		case idx < src.scope.offset || idx >= src.scope.offset+len(src.tbl.cols):
			return true
		default:
			idx -= src.scope.offset
			if idx == src.tbl.ipkIndex {
				idx = xnRowid
			}
		}
		gcol[i] = idx
	}
	// Only which order the satisfied plan delivers is computed here, and that is an
	// optimization: once groupsSorted holds, dropping the sort is correct either way.
	// The walk delivers ascending-key order only when key columns from nEq on carry
	// the remaining GROUP BY terms left to right, all ascending.
	nEq := key.nEq
	if nEq > len(key.cols) {
		nEq = len(key.cols)
	}
	pinned := make(map[int]bool, nEq)
	for j := 0; j < nEq; j++ {
		pinned[keyColumnOf(src.tbl, key.cols[j])] = true
	}
	inKeyOrder := true
	next := 0 // the next unpinned GROUP BY term the ascending order expects
	for _, c := range gcol {
		if pinned[c] {
			continue
		}
		j := nEq + next
		next++
		if j >= len(key.cols) || j >= len(key.desc) || key.desc[j] ||
			keyColumnOf(src.tbl, key.cols[j]) != c {
			inKeyOrder = false
			break
		}
	}
	// The plan groups. It is only worth dropping the sort when the order it
	// delivers differs from the ascending-key one sorter1 (and the hash path's
	// keyTupleLess drain) would have produced anyway.
	return !inKeyOrder
}

// groupEmissionOrderProvable reports whether which group comes out first is
// reproducible, separate from the anchor row and the order rows reach step().
// groupsArriveOutOfKeyOrder answers from the planner's verdict; when the planner
// declined there is none, and the sorter1 fallback is right only if C sorted
// too, i.e. no access path could deliver the grouping
// (groupingCouldArriveOrdered). Example: a DISTINCT aggregate argument adds
// WHERE_WANT_DISTINCT (select.c:8483) which the port does not model, and with
// an index on t(a DESC) C emits groups descending.
func groupEmissionOrderProvable(p *ReadOnlyPager, c *compiler, srcs []joinSource,
	scopes []tableScope, gp *groupByPlan, stmt *SelectStmt) bool {
	if len(gp.groupExprs) == 0 {
		return true
	}
	// Scoped to the family the port declines for a DISTINCT aggregate argument,
	// for aggPlanOrderProvable's reason: everywhere else the port decides the
	// scan and groupsArriveOutOfKeyOrder reads its verdict. A statement the port
	// declines for one of its OTHER reasons -- a correlated compile context,
	// sqlite_stat1 present, a generated column -- has the same hazard and is
	// still served ascending; no wrong answer has been MEASURED for one, so it is
	// reported rather than declined blind.
	if !wherePlanHasDistinctAgg(stmt) {
		return true
	}
	if anchorPlanOrderProvable(p, c, srcs, scopes, stmt) {
		return true
	}
	// A single ascending-everywhere grouping is emitted in key order by every
	// plan (groupEmissionAscendingEverywhere), which is the order the sorted
	// drain produces.
	if c != nil && c.outer == nil && groupEmissionAscendingEverywhere(p, srcs, len(gp.groupExprs)) {
		return true
	}
	return !groupingCouldArriveOrdered(p, srcs, scopes, gp)
}

// groupingCouldArriveOrdered: could any access path deliver the grouping without
// a sort (select.c's groupBySort==0)? A superset, used only to decline: it
// ignores pinned columns, direction and key column order (a GROUP BY match is
// "not as strict", where.c:5138), and asks only whether some index holds every
// GROUP BY column.
func groupingCouldArriveOrdered(p *ReadOnlyPager, srcs []joinSource, scopes []tableScope, gp *groupByPlan) bool {
	if p == nil || len(srcs) != 1 || srcs[0].tbl == nil {
		return true // a join's nesting decides this, and is not decided here
	}
	tbl := srcs[0].tbl
	cols := make([]int, 0, len(gp.groupExprs))
	for _, ge := range gp.groupExprs {
		ce, ok := ge.(ColumnExpr)
		if !ok {
			return true
		}
		idx, ok := resolveColumnIndex(scopes, ce)
		if !ok {
			return true
		}
		if idx <= rowidIdentityBase || idx == tbl.ipkIndex {
			// A rowid term is delivered grouped by EVERY access path -- the
			// rowid full scan trivially, an index because its key carries the
			// rowid as a trailing column.
			return true
		}
		cols = append(cols, idx)
	}
	// Asked of the source's OWN catalog, for the reason anchorNoIndexInPlay
	// gives: an index on a table read through "aux.t" is not in the COMPILING
	// pager's schema, and missing it here would answer false and serve.
	owner := p.forDB(srcs[0].dbIdx)
	if owner == nil {
		return true
	}
	// tbl.name, not the FROM item's scope name: wherePlanIndexList matches it
	// against SchemaRow.TblName, which an ALIAS would never equal.
	idxs, _, ok := wherePlanIndexList(owner, tbl, tbl.name, "", false, nil) // groupingCouldArriveOrdered: not yet proven safe here, see where_plan_exprindex_skip.go
	if !ok {
		return true
	}
	for _, ix := range idxs {
		if ix.ipk {
			continue // the fake sPk: its one key column is the rowid, handled above
		}
		covered := true
		for _, gc := range cols {
			found := false
			for _, ac := range ix.aiColumn {
				if ac == gc {
					found = true
					break
				}
			}
			if !found {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

// keyColumnOf is build.c's "if( iColumn==pTable->iPKey ) iColumn = XN_ROWID"
// applied to one index-key column number, so an INTEGER PRIMARY KEY column and
// an explicit rowid reference compare as the same thing.
func keyColumnOf(tbl *resolvedTable, col int) int {
	if tbl != nil && col == tbl.ipkIndex {
		return xnRowid
	}
	return col
}

// groupPlanRefs collects everything bindAggOuterRefs must walk for a GROUP BY
// query: every item's rewritten expression (select list, HAVING and ORDER BY
// alike) and every aggregate accumulator template any of them planned.
func groupPlanRefs(gp *groupByPlan) ([]*aggItem, []Expr) {
	items := groupPlanItems(gp)
	var aggs []*aggItem
	for _, it := range items {
		aggs = append(aggs, it.aggTemplates...)
	}
	return aggs, itemExprsOf(items)
}

// anchorPlanOrderProvable reports whether the row order fed to the aggregates is
// provably C's, the precondition for a reproducible anchor row (the group's
// first row without min()/max()). This engine visits a base table in ascending
// rowid (index seeks are sorted back, SeekIndexRowidsSegments) while C may walk
// an index or nest joins differently. Checked: the access path per table
// (anchorNoIndexInPlay) and the join nesting (anchorLoopOrderProvable).
func anchorPlanOrderProvable(p *ReadOnlyPager, c *compiler, srcs []joinSource, scopes []tableScope, stmt *SelectStmt) bool {
	if wherePlanIndexOrderDecided(p, c, srcs, stmt) {
		return true
	}
	// anchorNoIndexInPlay is a proxy; an index that yields no WhereLoop leaves the
	// plan identical, which wherePlanIndexesProvablyInert proves. A WHERE_MULTI_OR
	// loop comes from an OR term, not an index, and returns rows as concatenated
	// sub-scans; anchorMultiOrInPlay covers it.
	if anchorMultiOrInPlay(p, srcs, scopes, stmt) {
		return false
	}
	// Both proxies speak for REAL indexes only; an automatic index needs none.
	if anchorAutoIndexPossible(p, c, srcs, scopes, stmt) {
		return false
	}
	return (anchorNoIndexInPlay(p, srcs, scopes, stmt) || wherePlanIndexesProvablyInert(p, c, srcs, stmt, nil)) &&
		anchorLoopOrderProvable(srcs, scopes, stmt.Where)
}

// anchorAutoIndexPossible reports whether C could walk a table through an
// automatic index, which the no-index proxies miss. Requires automatic_index, a
// non-outermost loop (seed nRow 0 skips it, where.c:5950), and a term
// termCanDriveIndex accepts (where.c:901). Later tests are not modelled;
// "possible" is the safe side.
func anchorAutoIndexPossible(p *ReadOnlyPager, c *compiler, srcs []joinSource, scopes []tableScope, stmt *SelectStmt) bool {
	// A subquery compiled for one live outer row (rowOuter) runs once per row
	// of that loop just as one compiled under an outer compiler does.
	if p == nil || !p.AutomaticIndex() || len(srcs) == 1 && (c == nil || c.outer == nil && c.rowOuter == nil) {
		return false
	}
	conjuncts := splitTopLevelAnd(stmt.Where)
	rightJoin := false
	for i := range srcs {
		conjuncts = append(conjuncts, splitTopLevelAnd(srcs[i].on)...)
		rightJoin = rightJoin || srcs[i].rightOuter
	}
	drives := func(col, other Expr) bool {
		ce, ok := whereSkipCollate(col).(ColumnExpr)
		if !ok {
			return false
		}
		refs := map[int]bool{}
		collectTableRefs(ce, scopes, refs)
		if len(refs) != 1 {
			return false
		}
		for i := range refs {
			if i >= len(srcs) || srcs[i].tbl == nil || isRowidName(r33sFoldIdent(ce.Name)) {
				return false
			}
			// A RIGHT or FULL join is not reordered, so its first item is the
			// outermost loop, at seed 0; its right operand never takes one
			// (termCanDriveIndex's JT_RIGHT assertion, where.c:911).
			if rightJoin && (i == 0 || srcs[i].rightOuter) {
				return false
			}
			if ci, hit := scopes[i].colIndex[r33sFoldIdent(ce.Name)]; !hit || srcs[i].tbl.cols[ci].IsRowidAlias {
				return false
			}
			orefs := map[int]bool{}
			collectTableRefs(other, scopes, orefs)
			return !orefs[i]
		}
		return false
	}
	for _, cj := range conjuncts {
		be, ok := cj.(BinaryExpr)
		if !ok || !(be.Op == "=" || be.Op == "==" || equalFoldName(be.Op, "IS")) {
			continue
		}
		if drives(be.L, be.R) || drives(be.R, be.L) {
			return true
		}
	}
	return false
}

// anchorMultiOrInPlay reports whether an OR term in WHERE or ON can produce a
// WHERE_MULTI_OR loop (exprAnalyzeOrTerm case 3, whereexpr.c:788; where.c:4838),
// which emits rows as concatenated per-disjunct sub-scans (OP_RowSetTest,
// wherecode.c:2404). That order is ported (where_plan_multior_r37a.go) only on
// the path wherePlanIndexOrderDecided answers, checked first; reaching here
// means the planner did not decide, so a possible loop is enough to refuse.
func anchorMultiOrInPlay(p *ReadOnlyPager, srcs []joinSource, scopes []tableScope, stmt *SelectStmt) bool {
	conjuncts := splitTopLevelAnd(stmt.Where)
	for i := range srcs {
		conjuncts = append(conjuncts, splitTopLevelAnd(srcs[i].on)...)
	}
	var ors []Expr
	for _, cj := range conjuncts {
		if b, ok := cj.(BinaryExpr); ok && equalFoldName(b.Op, "OR") {
			ors = append(ors, cj)
		}
	}
	if len(ors) == 0 {
		return false // exprAnalyzeOrTerm never runs, so no case-3 term exists
	}
	allInert := true
	for _, or := range ors {
		if !anchorOrTermProvablyInert(or) {
			allInert = false
			break
		}
	}
	if allInert {
		return false
	}
	if anchorSingleTableOrsCannotYieldLTerm(p, srcs, scopes, ors) {
		return false
	}
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{
			tbl: srcs[i].tbl, on: srcs[i].on,
			left: srcs[i].left, rightOuter: srcs[i].rightOuter,
		}
	}
	terms, ok := wherePlanTermsFrom(jts, scopes, stmt.Where)
	if !ok {
		return true
	}
	return wherePlanTermsHaveOr(terms)
}

// anchorOrTermProvablyInert reports whether OR term or's case-3 indexable mask
// (exprAnalyzeOrTerm, whereexpr.c:707-793) is provably zero, so whereLoopAddOr
// (where.c:4838) builds no WHERE_MULTI_OR loop from it:
//
//   - the OR contains a COLLATE anywhere (whereexpr.c:1319): exprAnalyzeOrTerm
//     never runs;
//   - a direct disjunct is [NOT] EXISTS: allowedOp (whereexpr.c:99) rejects
//     TK_EXISTS, so that disjunct contributes an empty mask (whereexpr.c:733) and
//     "indexable &= b" (whereexpr.c:775) zeroes the whole term.
//
// Any other shape is left unknown, so this only narrows anchorMultiOrInPlay's
// default.
func anchorOrTermProvablyInert(or Expr) bool {
	if wherePlanOrTermIsInert(or) {
		return true
	}
	var disjuncts []Expr
	var split func(Expr)
	split = func(x Expr) {
		if b, ok := x.(BinaryExpr); ok && equalFoldName(b.Op, "OR") {
			split(b.L)
			split(b.R)
			return
		}
		disjuncts = append(disjuncts, x)
	}
	split(or)
	for _, d := range disjuncts {
		if _, ok := d.(ExistsExpr); ok {
			return true
		}
	}
	return false
}

// anchorSingleTableOrsCannotYieldLTerm is a third inertness proof for
// anchorMultiOrInPlay: on a single unindexed rowid table, whereLoopAddOr probes
// each disjunct against the fake rowid key only (where.c:4033; automatic
// indexes are excluded, where.c:4066). A disjunct that references no rowid/IPK
// column contributes no lterm (where.c:2851, 4134), which empties the OR set
// (where.c:4882), so no WHERE_MULTI_OR loop is built and rowid scan order is
// C's. Single source only: resolveColumnIndex's offset equals tbl.ipkIndex only
// then.
func anchorSingleTableOrsCannotYieldLTerm(p *ReadOnlyPager, srcs []joinSource, scopes []tableScope, ors []Expr) bool {
	if len(srcs) != 1 {
		return false
	}
	s := srcs[0]
	if s.tbl == nil || s.tbl.withoutRowid || s.derived != nil || s.vtabItem != nil || s.cteItem != nil {
		return false
	}
	// srcs[0].derived == nil is already guaranteed above, so
	// anchorNoIndexInPlay's derived-source branch (the only one that reads
	// scopes/stmt) can never trigger here -- nil is safe.
	if !anchorNoIndexInPlay(p, srcs, nil, nil) {
		return false
	}
	for _, or := range ors {
		if exprReferencesRowidColumn(or, scopes, s.tbl) {
			return false
		}
	}
	return true
}

// exprReferencesRowidColumn conservatively reports whether e's tree could
// possibly evaluate a ROWID/INTEGER PRIMARY KEY alias column of tbl -- used
// only by anchorSingleTableOrsCannotYieldLTerm above to PROVE an OR clause
// inert, so like exprMightReference (where_plan_exprindex_skip.go) the two
// failure directions are not symmetric: an unmodelled node shape answers
// true ("might reference"), never false. Modelled on that function's own
// traversal.
func exprReferencesRowidColumn(e Expr, scopes []tableScope, tbl *resolvedTable) bool {
	if e == nil {
		return false
	}
	switch x := e.(type) {
	case ColumnExpr:
		idx, ok := resolveColumnIndex(scopes, x)
		if !ok {
			return true
		}
		return idx <= rowidIdentityBase || idx == tbl.ipkIndex
	case LiteralExpr, ParamExpr:
		return false
	case UnaryExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl)
	case BinaryExpr:
		return exprReferencesRowidColumn(x.L, scopes, tbl) || exprReferencesRowidColumn(x.R, scopes, tbl)
	case IsNullExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl)
	case InExpr:
		// x.Sub's CONTENTS never matter here, whatever they correlate to:
		// exprMightBeIndexed (whereexpr.c:1198-1220) identifies a term's
		// leftColumn from pLeft (x.X, this InExpr's own tested operand)
		// alone, never from pExpr->x.pSelect. A subquery is not itself a
		// TK_COLUMN node, so it can never BE the rowid match either --
		// only x.X (or a list/vector element) can.
		if exprReferencesRowidColumn(x.X, scopes, tbl) {
			return true
		}
		for _, it := range x.List {
			if exprReferencesRowidColumn(it, scopes, tbl) {
				return true
			}
		}
		return false
	case SubqueryExpr, ExistsExpr:
		// Neither is a TK_COLUMN, and as a whole term both fall outside allowedOp
		// (whereexpr.c:99), so neither can contribute an lterm.
		return false
	case BetweenExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl) ||
			exprReferencesRowidColumn(x.Lo, scopes, tbl) ||
			exprReferencesRowidColumn(x.Hi, scopes, tbl)
	case LikeExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl) ||
			exprReferencesRowidColumn(x.Pattern, scopes, tbl) ||
			exprReferencesRowidColumn(x.Escape, scopes, tbl)
	case GlobExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl) || exprReferencesRowidColumn(x.Pattern, scopes, tbl)
	case CollateExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprReferencesRowidColumn(a, scopes, tbl) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprReferencesRowidColumn(x.X, scopes, tbl)
	case CaseExpr:
		if x.Base != nil && exprReferencesRowidColumn(x.Base, scopes, tbl) {
			return true
		}
		for _, w := range x.Whens {
			if exprReferencesRowidColumn(w.When, scopes, tbl) || exprReferencesRowidColumn(w.Then, scopes, tbl) {
				return true
			}
		}
		return x.Else != nil && exprReferencesRowidColumn(x.Else, scopes, tbl)
	case RowExpr:
		for _, el := range x.Elems {
			if exprReferencesRowidColumn(el, scopes, tbl) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// anchorNoIndexInPlay is the access-path half: no scanned table carries an
// index, so C's only option is the rowid scan, which is this engine's order.
// Measured (agg_r26_planorder_test.go), divergence appears only where C has an
// index it chose to use. A superset of the divergent cases; serving them would
// need walking the index C chose, but a secondary-index read here is sorted
// back into rowid order (SeekIndexRowidsSegments). Indexes that can produce no
// loop (neither covering, nor useful for ORDER BY, nor partial, nor INDEXED BY)
// are handled by wherePlanIndexesProvablyInert.
func anchorNoIndexInPlay(p *ReadOnlyPager, srcs []joinSource, scopes []tableScope, stmt *SelectStmt) bool {
	byDB := map[int][]SchemaRow{}
	opaque := false
	for i, s := range srcs {
		// A derived table, vtab or CTE hides its FROM from this walk, and C flattens a
		// simple subquery before planning, so an index can be reached through one
		// ("SELECT group_concat(x) FROM (SELECT x FROM t)" walks a covering index in
		// C). Such an opaque source is provable only when no reachable catalog holds an
		// index at all.
		//
		// A derived table resolveDerivedSource proved simple (derivedOrderProvable: one
		// plain base table) has exactly the order wherePlanSingleTableIndexOrder decided
		// for it, so it is excluded from opaque, provided no outer clause could fold a
		// predicate into it when flattened (select.c:4572-4704):
		// derivedSourceOuterSafe, or the narrower derivedExprOnlyOuterSafe.
		if s.derived != nil && s.derivedOrderProvable &&
			(!s.derivedOrderNeedsOuterSafety || derivedSourceOuterSafe(i, srcs, scopes, stmt) ||
				derivedExprOnlyOuterSafe(p, i, srcs)) {
			continue
		}
		// An fts3/4/5 vtab is excluded from opaque too: vtab items are never flattened
		// (select.c:7781, pSub==0), so no b-tree index can affect them, and both fts
		// modules always deliver rowid/docid order (fts3.c:1612; fts5_main.c:614,
		// FTS5_BI_ORDER_ROWID). Other modules stay opaque. Loop nesting is still
		// checked separately by anchorLoopOrderProvable: a vtab whose MATCH correlates
		// to another table is forced inner in C (where.c:4390; fts3.c:1636;
		// fts5_main.c:657), which the join executor does not reproduce.
		if isFtsVtabSource(p, s) {
			continue
		}
		// A non-materialized CTE flattens on the same terms as a derived table
		// (withExpand, select.c:5722; only restriction 28, select.c:7797), so one whose
		// body cteOrderProvable accepts is excluded from opaque too. Otherwise an
		// unrelated index elsewhere made with3.test's "WITH t1(a) AS (VALUES(1)) ...
		// GROUP BY 1" decline.
		if s.cteItem != nil && s.cteItem.orderProvable {
			continue
		}
		// A RECURSIVE CTE is excluded on a DIFFERENT proof from the one just
		// above -- cteOrderProvable answers false for every one of them, by
		// design -- see recursiveCTEIndexUnreachable (anchor_recursive_cte.go)
		// for the C that makes an index unreachable from inside one
		// (select.c:4354's flattenSubquery restriction (22), select.c:5146's
		// pushDownWhereTerms restriction (2)) and for the 42-answer oracle
		// battery that measured it.
		if s.cteItem != nil && recursiveCTEIndexUnreachable(s.cteItem.binding) {
			continue
		}
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil {
			opaque = true
			continue
		}
		// A schema-catalog / sqlite_sequence source is this engine's own b-tree,
		// which carries no index either way.
		if s.tbl == nil || s.tbl.name == "" {
			continue
		}
		// An attached table's indexes are not in p.Schema(); forDB returns the owning
		// reader, so each source is checked in its own catalog. A multi-table
		// cross-database FROM is still declined by markWherePlanEligibility.
		rows, ok := byDB[s.dbIdx]
		if !ok {
			owner := p.forDB(s.dbIdx)
			if owner == nil {
				return false // no schema to consult: cannot prove anything
			}
			var err error
			if rows, err = owner.Schema(); err != nil {
				return false
			}
			byDB[s.dbIdx] = rows
		}
		for i := range rows {
			if rows[i].Type == "index" && equalFoldName(rows[i].TblName, s.tbl.name) {
				return false
			}
		}
	}
	if opaque {
		return noIndexAnywhere(p)
	}
	return true
}

// isFtsVtabSource reports whether s is a persisted fts3/fts4/fts5 table, never a
// table-valued function or another module (see anchorNoIndexInPlay). Checked in
// the source's own catalog (p.forDB).
func isFtsVtabSource(p *ReadOnlyPager, s joinSource) bool {
	if s.vtabItem == nil || s.vtabItem.TableFunc {
		return false
	}
	owner := p.forDB(s.dbIdx)
	if owner == nil {
		return false
	}
	name := s.vtabItem.Table
	if _, ok := owner.fts3TableInfo(name); ok {
		return true
	}
	return owner.isFts5Table(name)
}

// derivedSourceOuterSafe reports whether nothing outside srcs[i]'s own body
// could fold a predicate into it when C flattens it (select.c:4572-4704), which
// could change the access path the isolated verdict picked. Over-inclusive by
// design: any reference to index i in WHERE, HAVING, GROUP BY, ORDER BY or any
// join condition (including its own) refuses.
func derivedSourceOuterSafe(i int, srcs []joinSource, scopes []tableScope, stmt *SelectStmt) bool {
	if stmt == nil {
		return false
	}
	refs := map[int]bool{}
	collectTableRefs(stmt.Where, scopes, refs)
	collectTableRefs(stmt.Having, scopes, refs)
	for _, g := range stmt.GroupBy {
		collectTableRefs(g, scopes, refs)
	}
	for _, ot := range stmt.OrderBy {
		collectTableRefs(ot.Expr, scopes, refs)
	}
	for _, s := range srcs {
		collectTableRefs(s.on, scopes, refs)
	}
	return !refs[i]
}

// derivedExprOnlyOuterSafe is the alternative proof for an outer reference to
// derived source i. A flattened predicate can only drive or re-cost an index
// through exprMightBeIndexed (whereexpr.c:1066): a bare TK_COLUMN, or a match
// against an expression index key. If every column source i exposes is a
// computed expression (columnInfo.NoAffinity), substitution never yields a bare
// column, and noExprIndexAnywhere rules out the second arm, so the isolated
// verdict survives (join.test join-14.5).
func derivedExprOnlyOuterSafe(p *ReadOnlyPager, i int, srcs []joinSource) bool {
	if i < 0 || i >= len(srcs) || srcs[i].tbl == nil || len(srcs[i].tbl.cols) == 0 {
		return false
	}
	for _, c := range srcs[i].tbl.cols {
		if !c.NoAffinity {
			return false // a plain passthrough column: the bare-TK_COLUMN arm could still fire on it
		}
	}
	return noExprIndexAnywhere(p)
}

// noExprIndexAnywhere reports whether no reachable index (main, temp,
// attachments) has an expression key column: the precondition of
// exprMightBeIndexed's second arm, without reproducing its match. Autoindexes
// (empty SQL) are plain-column and skipped.
func noExprIndexAnywhere(p *ReadOnlyPager) bool {
	if p == nil {
		return false
	}
	for dbIdx := 0; dbIdx <= len(p.attachedReaders); dbIdx++ {
		owner := p.forDB(dbIdx)
		if owner == nil {
			return false
		}
		rows, err := owner.Schema()
		if err != nil {
			return false
		}
		for i := range rows {
			if rows[i].Type != "index" || rows[i].SQL == "" {
				continue
			}
			pci, perr := parseCreateIndexStmt(rows[i].SQL)
			if perr != nil {
				return false // cannot prove: conservatively assume it might be expression-keyed
			}
			for _, e := range pci.exprs {
				if e != nil {
					return false
				}
			}
		}
	}
	return true
}

// noIndexAnywhere reports whether NO catalog this pager can reach -- its own
// (main plus temp) and every attached database's -- holds an index at all. It is
// what anchorNoIndexInPlay falls back to for a row source whose own FROM clause
// it cannot see; see there.
func noIndexAnywhere(p *ReadOnlyPager) bool {
	if p == nil {
		return false
	}
	for dbIdx := 0; dbIdx <= len(p.attachedReaders); dbIdx++ {
		owner := p.forDB(dbIdx)
		if owner == nil {
			return false
		}
		rows, err := owner.Schema()
		if err != nil {
			return false
		}
		for i := range rows {
			if rows[i].Type == "index" {
				return false
			}
		}
	}
	return true
}

// anchorLoopOrderProvable is the join-nesting half: the anchor is the first row
// of the nesting, and C nests by cost (wherePathSolver), possibly with an
// automatic index. It asks computeExecOrder's own two questions in its order:
// a RIGHT/FULL join pins literal FROM order (hasRightOuter), else the ported
// planner's verdict (sqliteExecOrder). Where the port decides it is right
// (anchor_r28_holes_test.go's control); the heuristic fallback is not C's
// order.
func anchorLoopOrderProvable(srcs []joinSource, scopes []tableScope, where Expr) bool {
	if len(srcs) < 2 {
		// One source: this engine full-scans it in ascending rowid order, which
		// is what SQLite's only remaining access path does too once
		// anchorNoIndexInPlay has established there is no index to walk.
		return true
	}
	// joinedTablesFor (vdbe_join_codegen.go) is the same conversion
	// joinPushdownPlan hands planJoinPushdown, so this cannot drift from the
	// fields the ported planner actually reads -- it used to be duplicated here.
	// hasRightOuter is still asked FIRST: wherePlanDecidedOrder answers false for
	// it (the port never sees such a FROM clause), but computeExecOrder answers
	// literal FROM order outright, which is provable without the port.
	jts := joinedTablesFor(srcs)
	if hasRightOuter(jts) {
		return true
	}
	// Two items with the second LEFT JOINed: whereLoopAddAll forces item 1 after
	// item 0 (where.c:4968, 4990), so the nesting is fixed regardless of cost. Same
	// as computeExecOrder's fallback for this shape. Does not generalize to three
	// items.
	if len(jts) == 2 && !jts[0].left && jts[1].left {
		return true
	}
	// Two items where one is an fts vtab whose MATCH correlates to the other:
	// computeExecOrder uses this same ftsMatchForcedOrder for the physical nesting,
	// so the answer cannot drift (see where_plan_fts_forced_order.go).
	if _, ok := ftsMatchForcedOrder(jts, scopes, where); ok {
		return true
	}
	return wherePlanDecidedOrder(jts, scopes, where)
}

// anchorIdentityFixed reports whether which row of a group supplies the anchor
// is unobservable, so no order proof is needed. The anchor is read by bare
// columns or, through a subquery, any column of any item (anchorIsRead). If
// the GROUP BY key pins an item to one row per group, every column of it is
// constant within the group: bare columns need their own items pinned, a
// subquery needs all of them. This is where.c's isDistinctRedundant; see
// anchorPinnedTables. An order-sensitive aggregate over an unpinned side is a
// separate question (armOrderSensitive).
func anchorIdentityFixed(srcs []joinSource, scopes []tableScope, gp *groupByPlan, stmt *SelectStmt) bool {
	if len(gp.groupExprs) == 0 || len(srcs) > 64 || len(scopes) != len(srcs) {
		// No GROUP BY key pins anything: a whole-table aggregate's single group
		// is every row the scan produced, so which one is the anchor always
		// shows.
		return false
	}
	pinned := anchorPinnedTables(srcs, scopes, gp.groupExprs)
	if anchorSubqueryRead(gp.selectHasSubquery, stmt) {
		all := uint64(1)<<uint(len(srcs)) - 1
		return pinned&all == all
	}
	need := uint64(0)
	for _, it := range groupPlanItems(gp) {
		if it.bareWide {
			return false
		}
		need |= it.bareTables
	}
	return pinned&need == need
}

// anchorPinnedTables reports, per FROM item, whether the GROUP BY key pins it to
// one row per group. Ported from isDistinctRedundant (where.c), first test only:
// a rowid/IPK in the key, which is unique and non-null without index metadata.
// The UNIQUE-index test is not reproduced (only costs declines). An outer join's
// NULL rowid means the whole item is NULL-extended, still uniform. Only a plain
// column reference counts (not COLLATE or CAST).
func anchorPinnedTables(srcs []joinSource, scopes []tableScope, groupExprs []Expr) uint64 {
	ctx := &evalCtx{tables: scopes}
	var pinned uint64
	pin := func(i int) {
		if i < 0 || i >= len(srcs) {
			return
		}
		// A DERIVED table, CTE, vtab or schema-catalog item is not a base table:
		// its rows are whatever the inner query/module/catalog produced, so no
		// column of it identifies one (a subquery may repeat an INTEGER PRIMARY
		// KEY value freely), and a WITHOUT ROWID table has no rowid to pin with.
		s := &srcs[i]
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil ||
			s.catalogScope != scopeAny ||
			s.tbl == nil || s.tbl.name == "" || s.tbl.withoutRowid {
			return
		}
		pinned |= uint64(1) << uint(i)
	}
	for _, ge := range groupExprs {
		ce, ok := ge.(ColumnExpr)
		if !ok {
			continue
		}
		_, idx, col, rowidTab, err := resolveColumn(ctx, ce.Qualifier, ce.Name)
		if err != nil {
			continue
		}
		switch {
		case rowidTab >= 0:
			pin(rowidTab)
		case col != nil && col.IsRowidAlias:
			pin(scopeOfValueIndex(scopes, idx))
		}
	}
	return pinned
}

// aggPlanOrderProvable is armOrderSensitive's precondition: the loop-order half
// always, plus the access-path half for statements the planner did not decide
// (wherePlanHasDistinctAgg, LIKE/GLOB/likelihood terms, subquery terms,
// row-value ranges). Where the planner decides the scan, this engine walks the
// index it chose, so non-DISTINCT order-sensitive aggregates are right and
// arming them would only decline. A DISTINCT aggregate argument gives C's
// planner WHERE_WANT_DISTINCT (select.c:8483, 8861), which the port does not
// model.
func aggPlanOrderProvable(p *ReadOnlyPager, c *compiler, srcs []joinSource,
	scopes []tableScope, stmt *SelectStmt) bool {
	if !anchorLoopOrderProvable(srcs, scopes, stmt.Where) {
		return false
	}
	if !wherePlanHasDistinctAgg(stmt) && (!aggSourcesArePlainTables(srcs) || wherePlanIndexOrderDecided(p, c, srcs, stmt)) {
		return true
	}
	return anchorPlanOrderProvable(p, c, srcs, scopes, stmt)
}

// aggSourcesArePlainTables reports a FROM clause of base tables only -- the
// ported planner's domain, where its declining leaves an index C may walk.
// A derived table, CTE or virtual table arrives in an order of its own making
// (a derived table's ORDER BY, say), which the planner is never asked about.
func aggSourcesArePlainTables(srcs []joinSource) bool {
	for i := range srcs {
		s := &srcs[i]
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil || s.tbl == nil {
			return false
		}
	}
	return len(srcs) > 0
}

// armOrderSensitive marks every order-sensitive accumulator
// (aggItem.orderSensitive) when this query's row order is not provably C's, so
// it declines at finalize only if its accumulation actually depended on order
// (aggItem.orderRiskErr). Separate from the anchor guard: it is step order, not
// a row read, so anchorIdentityFixed cannot excuse it. Arming instead of
// declining keeps order-free results (an integer sum over a join) answering.
func armOrderSensitive(orderProvable bool, aggs []*aggItem) {
	if orderProvable {
		return
	}
	for _, a := range aggs {
		if a != nil && a.orderSensitive() {
			a.orderStrict = true
		}
	}
}

// anyOrderSensitive is armOrderSensitive's question on its own, asked BEFORE the
// proof is computed so a query that needs no proof does not pay for one.
func anyOrderSensitive(aggs []*aggItem) bool {
	for _, a := range aggs {
		if a != nil && a.orderSensitive() {
			return true
		}
	}
	return false
}

// aggLoopNQueryLoop runs fn (the aggregate's scan loop, WHERE and arguments)
// under the nQueryLoop C uses for that loop body: the pre-loop value plus this
// FROM clause's nRowOut (where.c:7198). updateAccumulator runs inside the loop,
// so an argument subquery plans with the bumped value; the output phase uses
// the pre-loop one. c.nQueryLoop holds the pre-loop value on entry.
func aggLoopNQueryLoop(c *compiler, preKnown bool, fn func() error) error {
	pre := c.nQueryLoop
	c.nQueryLoop, c.nQueryLoopKnown = pre+c.planNRow, preKnown && c.planNRowOK
	err := fn()
	c.nQueryLoop, c.nQueryLoopKnown = pre, false
	return err
}

// withFromPlanTrust runs fn (anchorPlanOrderProvable or aggPlanOrderProvable)
// with the nQueryLoop trust c had when its FROM clause was planned, before
// compileScanAggregate/GroupBy reset it for children. Without it, a nested
// aggregate re-asking the planner got a different answer than the planner had
// already given (in.test's view over an indexed GROUP BY). The values are
// restored right after fn.
func withFromPlanTrust(c *compiler, savedNQL logEst, savedNQLKnown bool, fn func() bool) bool {
	c.nQueryLoop, c.nQueryLoopKnown = savedNQL, savedNQLKnown
	ok := fn()
	c.nQueryLoopKnown = false
	return ok
}

// errAnchorPlanOrder is the decline anchorPlanOrderProvable produces, shared by
// the GROUP BY and whole-table compilers. It names WHICH half failed, because
// the two are different pieces of work: the first waits on
// whereLoopAddBtreeIndex plus an index-ordered scan cursor, the second on
// whatever markWherePlanEligibility currently refuses. Both keep the same
// leading phrase so one bucket still counts them together. The order-sensitive
// AGGREGATE's own decline is a different message and a different backlog --
// aggItem.orderRiskErr, raised at finalize rather than here.
func errAnchorPlanOrder(indexInPlay bool) error {
	if indexInPlay {
		return fmt.Errorf("%w: a bare column (or correlated select-list subquery) in an aggregate query over an INDEXED table, whose anchor row C SQLite may take from an index scan instead of a rowid-order one", errVDBEUnsupported)
	}
	return fmt.Errorf("%w: a bare column (or correlated select-list subquery) in an aggregate query over a multi-table FROM clause whose JOIN LOOP ORDER the ported planner declined, so which row of a group is its anchor is not reproducible", errVDBEUnsupported)
}

// anchorIsRead reports whether anything reads the group's anchor row: a bare
// column (usesGroupBare) or a subquery in the select list, HAVING or ORDER BY
// (aggResult hands it the anchor). A GROUP BY key column reference reads the
// anchor too but is not counted: its value is equal across the group and only
// its representation (1 vs 1.0) can vary, which already depended on scan order.
// Counting it declined plain "GROUP BY k ... min() ... ORDER BY k" shapes.
func anchorIsRead(usesGroupBare, selectSub bool, stmt *SelectStmt) bool {
	return usesGroupBare || anchorSubqueryRead(selectSub, stmt)
}

// anchorSubqueryRead is anchorIsRead's SUBQUERY half on its own: a subquery in
// any of the three item sets, which unlike a bare column can name any column of
// any FROM item and so cannot be attributed to one (anchorIdentityFixed).
func anchorSubqueryRead(selectSub bool, stmt *SelectStmt) bool {
	if selectSub || exprHasAnchorSubquery(stmt.Having) {
		return true
	}
	for _, ot := range stmt.OrderBy {
		if exprHasAnchorSubquery(ot.Expr) {
			return true
		}
	}
	return false
}

// exprHasAnchorSubquery is exprContainsSubquery minus subqueries inside an
// aggregate's own arguments, which are evaluated per scanned row, never against
// the anchor (e.g. "max((SELECT count(a) OVER (ORDER BY 1)))"). A window call is
// not an aggregate call and is walked normally.
func exprHasAnchorSubquery(e Expr) bool { return anchorSubqueryWalk(e, nil) }

// subqueryReadsOuter reports whether sub can read anything of the query it
// sits in -- the anchor row included. A subquery that compiles ON ITS OWN, with
// no outer scope, resolved every name inside itself, and it resolves them the
// same way with the outer scope present: the innermost scope wins
// (lookupName walks its NameContexts inner to outer, resolve.c). So it reads
// no row of the outer query, and which row is the anchor cannot change it.
// Anything that does not compile alone counts as reading the outer query.
func (c *compiler) subqueryReadsOuter(sub *SelectStmt) bool {
	prog, err := compileSubProgram(c.pager, sub, nil)
	return err != nil || prog.Correlated
}

// anchorSubqueryWalk is exprHasAnchorSubquery with a say in which subqueries
// count: reads, when given, decides for each one; nil counts every one.
func anchorSubqueryWalk(e Expr, reads func(*SelectStmt) bool) bool {
	counts := func(sub *SelectStmt) bool { return reads == nil || reads(sub) }
	// Shadowed so the walk below recurses with reads kept.
	exprHasAnchorSubquery := func(e Expr) bool { return anchorSubqueryWalk(e, reads) }
	switch x := e.(type) {
	case SubqueryExpr:
		return counts(x.Stmt)
	case ExistsExpr:
		return counts(x.Stmt)
	case UnaryExpr:
		return exprHasAnchorSubquery(x.X)
	case BinaryExpr:
		return exprHasAnchorSubquery(x.L) || exprHasAnchorSubquery(x.R)
	case IsNullExpr:
		return exprHasAnchorSubquery(x.X)
	case InExpr:
		if x.Sub != nil && counts(x.Sub) {
			return true
		}
		if exprHasAnchorSubquery(x.X) {
			return true
		}
		for _, it := range x.List {
			if exprHasAnchorSubquery(it) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprHasAnchorSubquery(x.X) || exprHasAnchorSubquery(x.Lo) || exprHasAnchorSubquery(x.Hi)
	case LikeExpr:
		return exprHasAnchorSubquery(x.X) || exprHasAnchorSubquery(x.Pattern)
	case GlobExpr:
		return exprHasAnchorSubquery(x.X) || exprHasAnchorSubquery(x.Pattern)
	case MatchExpr:
		return exprHasAnchorSubquery(x.X) || exprHasAnchorSubquery(x.Pattern)
	case CollateExpr:
		return exprHasAnchorSubquery(x.X)
	case FuncExpr:
		if isAggregateCall(x) {
			// The whole argument list -- including any subquery in it -- is
			// evaluated per scanned row by aggItem.step, never against the
			// anchor. See the doc comment above.
			return false
		}
		for _, a := range x.walkArgs() {
			if exprHasAnchorSubquery(a) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprHasAnchorSubquery(x.X)
	case RowExpr:
		for _, el := range x.Elems {
			if exprHasAnchorSubquery(el) {
				return true
			}
		}
		return false
	case CaseExpr:
		if x.Base != nil && exprHasAnchorSubquery(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasAnchorSubquery(w.When) || exprHasAnchorSubquery(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprHasAnchorSubquery(x.Else)
	default:
		return false
	}
}

// orderIsGroupKeyAscending reports whether ORDER BY asks for exactly what
// compileScanGroupByHash emits (group keys ascending, NULLs first, BINARY), so
// the sort can be skipped. Without it "GROUP BY k ORDER BY k" sorted every row.
// Narrow on purpose: the group keys positionally and completely, all ascending
// with no NULLS clause, matched by expression equality (not ordinals or
// aliases). Collation is excluded by the caller; LIMIT/OFFSET are applied by
// the hash path.
func orderIsGroupKeyAscending(stmt *SelectStmt, gp *groupByPlan) bool {
	if stmt == nil || gp == nil || len(gp.groupExprs) == 0 {
		return false
	}
	if len(stmt.OrderBy) != len(gp.groupExprs) {
		return false
	}
	for i, ot := range stmt.OrderBy {
		if ot.Desc || ot.Nulls != NullsDefault || ot.Expr == nil {
			return false
		}
		if !exprEqual(ot.Expr, gp.groupExprs[i]) {
			return false
		}
	}
	return true
}
