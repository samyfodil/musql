package engine

import "fmt"

// A FROM-clause subquery C codes as a co-routine is run where its loop
// begins: wherecode.c:1550 emits OP_InitCoroutine at the top of that level's
// loop, so a co-routine the planner put in an INNER loop restarts for every
// row of the loops outside it, and a body that is not deterministic is
// evaluated again each time. "WITH t AS (SELECT random() AS r FROM (SELECT 1)
// GROUP BY 1) SELECT count(DISTINCT r) FROM x, t" is 3 over three rows of x.
//
// This engine materializes a subquery once per execution. That is the same
// answer as an OUTERMOST co-routine (run once, its rows paired with every row
// inside) and as a materialization (OP_Once), so only an inner co-routine
// needs anything: its cursor runs the body again at every rewind
// (derivedSource.rerun), with the loops outside it exactly the ones C has
// there, which is why the whole plan is C's (markCoroutineReruns). Only a body
// that is not deterministic can tell the difference; every other subquery is
// left alone.

// cSubqueryBody is a FROM item's subquery as sqlite3Select sees it: an inline
// subquery, a CTE reference (cte non-nil) or a view (view true). ok is false for
// a plain table or a table-valued function.
func (p *ReadOnlyPager) cSubqueryBody(it FromItem) (body *SelectStmt, cte *cteBinding, view bool, ok bool) {
	if it.Subquery != nil {
		return it.Subquery, nil, false, true
	}
	if p == nil || it.TableFunc || it.Table == "" || it.GroupLen != 0 || len(it.NestedGroupSpan) != 0 {
		return nil, nil, false, false
	}
	if it.Schema == "" {
		if b, found := p.lookupCTE(it.Table); found && b.core != nil {
			return b.core, b, false, true
		}
	}
	pcv, found, err := p.resolveViewByNameIn(fromItemScope(it), it.Table)
	if err != nil || !found || pcv == nil {
		return nil, nil, false, false
	}
	return pcv.selectStmt, nil, true, true
}

// cSelectIsAggregate is SF_Aggregate: an aggregate in the select list or the
// ORDER BY, or a GROUP BY (resolve.c sets NC_HasAgg for the whole select).
func cSelectIsAggregate(s *SelectStmt) bool {
	if selectIsAggregateQuery(s) {
		return true
	}
	for _, ot := range s.OrderBy {
		if containsAggregate(ot.Expr) {
			return true
		}
	}
	return false
}

// cHasWindow is p->pWin: a window function in the select list or ORDER BY.
func cHasWindow(s *SelectStmt) bool {
	return len(s.Windows) != 0 || selectHasWindow(s) || orderByHasWindow(s)
}

// cFlattens ports sqlite3Select's FROM-loop guards (select.c:7790-7880) and
// flattenSubquery's restrictions (select.c:4341-4440) for items[i] of parent:
// whether C's query flattener absorbs that subquery into parent. certain is
// false for a shape whose answer depends on something this does not model; the
// caller must not guess then.
func (p *ReadOnlyPager) cFlattens(parent *SelectStmt, items []FromItem, i int) (flat, certain bool) {
	sub, cte, _, ok := p.cSubqueryBody(items[i])
	if !ok || sub == nil {
		return false, true
	}
	return p.cFlattensBody(parent, items, i, sub, cte)
}

// cFlattensBody is cFlattens for items[i] whose subquery is sub (cte when it
// is a CTE reference): a parenthesized join's synthetic "SELECT * FROM ..."
// has no FromItem of its own to read it off.
func (p *ReadOnlyPager) cFlattensBody(parent *SelectStmt, items []FromItem, i int, sub *SelectStmt, cte *cteBinding) (flat, certain bool) {
	it := items[i]
	if cte != nil {
		if cte.m10d == cteM10dYes { // (28), select.c:7795
			return false, true
		}
		if cte.recursive != nil || cte.selfRef != nil { // (22)
			return false, true
		}
	}
	isAgg := cSelectIsAggregate(parent)
	arms := []*SelectStmt{sub}
	for _, a := range sub.Compound {
		if a.Stmt == nil {
			return false, false
		}
		arms = append(arms, a.Stmt)
	}
	// pSub is the RIGHTMOST arm in C: SF_Aggregate, SF_Distinct and pSrc are
	// read off it, the ORDER BY and LIMIT off the compound as a whole.
	last := arms[len(arms)-1]
	if cSelectIsAggregate(last) { // select.c:7806
		return false, true
	}
	subOrderBy := len(sub.OrderBy) != 0
	subLimit := sub.Limit != nil || sub.LimitParam != nil
	// tag-select-0230: an ORDER BY that accomplishes nothing is dropped.
	if subOrderBy && (len(parent.OrderBy) != 0 || len(items) > 1) && !subLimit &&
		!r41OrderAgg(parent) && !parent.hasUpdateTarget() {
		subOrderBy = false
	}
	if subOrderBy && i == 0 && r41ComplexResult(parent) &&
		(len(items) == 1 || items[1].CrossKeyword || items[1].Join == JoinLeft || items[1].Join == JoinRight || items[1].Join == JoinFull) {
		return false, true
	}
	// flattenSubquery.
	if cHasWindow(parent) || cHasWindow(last) { // (25)
		return false, true
	}
	parentLimit := parent.Limit != nil || parent.LimitParam != nil
	if subLimit && parentLimit { // (13)
		return false, true
	}
	if sub.Offset != nil || sub.OffsetParam != nil { // (14)
		return false, true
	}
	// (15) needs to know whether parent is an arm of a compound; it only
	// matters with a LIMIT, which (8) refuses in a join anyway.
	if subLimit && len(items) == 1 && !isAgg {
		return false, false
	}
	if len(last.From) == 0 { // (7)
		return false, true
	}
	if last.Distinct { // (4)
		return false, true
	}
	if subLimit && (len(items) > 1 || isAgg) { // (8), (9)
		return false, true
	}
	if len(parent.OrderBy) != 0 && subOrderBy { // (11)
		return false, true
	}
	if isAgg && subOrderBy { // (16)
		return false, true
	}
	if subLimit && parent.Where != nil { // (19)
		return false, true
	}
	if subLimit && parent.Distinct { // (21)
		return false, true
	}
	outer := it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull || cItemIsLTORJ(items, i)
	if outer {
		if len(last.From) > 1 || parent.Distinct || it.Join == JoinRight || it.Join == JoinFull { // (3a), (3d), (26)
			return false, true
		}
	}
	if i > 0 && cFromHasRightJoin(last.From) { // (27a)
		return false, true
	}
	if len(arms) > 1 {
		if len(sub.OrderBy) != 0 { // (20)
			return false, true
		}
		if isAgg || parent.Distinct || outer { // (17d1), (17d2), (17f)
			return false, true
		}
		for k, a := range arms {
			if a.Distinct || cSelectIsAggregate(a) { // (17b)
				return false, true
			}
			if k > 0 && sub.Compound[k-1].Op != "UNION ALL" { // (17a)
				return false, true
			}
			if len(a.From) == 0 || cHasWindow(a) { // (17c), (17e)
				return false, true
			}
			if i > 0 && cFromHasRightJoin(a.From) { // (17g), (27b)
				return false, true
			}
		}
		if len(parent.OrderBy) != 0 { // (18)
			switch cOrderByAllResultCols(parent) {
			case triNo:
				return false, true
			case triUnknown:
				return false, false
			}
		}
		// (23) and (17h): the affinity comparison is compoundHasDifferentAffinities.
		if p.r41CompoundNotFlattened(parent, arms) {
			return false, true
		}
		return false, false
	}
	return true, true
}

// hasUpdateTarget reports the UPDATE ... FROM shape, whose subqueries carry
// SF_OrderByReqd (select.c:7840's condition (3)).
func (s *SelectStmt) hasUpdateTarget() bool {
	for _, it := range s.From {
		if it.UpdateTarget {
			return true
		}
	}
	return false
}

// cItemIsLTORJ is JT_LTORJ on items[i]: some later item is a RIGHT or FULL
// join, which sqlite3SrcListShiftJoinType marks on every item to its left.
func cItemIsLTORJ(items []FromItem, i int) bool {
	for _, it := range items[i+1:] {
		if it.Join == JoinRight || it.Join == JoinFull {
			return true
		}
	}
	return false
}

func cFromHasRightJoin(items []FromItem) bool {
	for _, it := range items {
		if it.Join == JoinRight || it.Join == JoinFull {
			return true
		}
	}
	return false
}

// cSelectRowEst is p->nSelectRow once sqlite3Select has coded body, which is
// what the enclosing planner gets as the subquery item's nRowLogEst
// (select.c:8068, :8127). prog is the body's compiled program, for
// pWInfo->nRowOut; nil when there is none. seed is pParse->nQueryLoop where
// the body is coded, seedOK whether it is known. ok is false where this does
// not follow C.
func cSelectRowEst(body *SelectStmt, prog *Program, compile func(*SelectStmt) *Program, seed logEst, seedOK bool) (logEst, bool) {
	if body == nil || body.ValuesArms != 0 || body.LimitParam != nil {
		// A multi-row VALUES (multiSelectValues) and a LIMIT that is not an
		// integer literal are not modelled.
		return 0, false
	}
	if len(body.Compound) == 0 {
		return cArmRowEst(body, prog, body.Limit, compile, seed, seedOK)
	}
	// multiSelect's UNION ALL arm (select.c:3005-3049): the left-most select
	// takes the LIMIT (computeLimitRegisters runs there first; every later
	// arm finds iLimit set and returns at once), and each arm's estimate is
	// added onto the running total, which an integer LIMIT then caps. Every
	// other operator, or an ORDER BY, goes through multiSelectByMerge, whose
	// estimates are not modelled.
	if len(body.OrderBy) != 0 || prog == nil || prog.Compound == nil || len(prog.Compound.arms) != len(body.Compound)+1 {
		return 0, false
	}
	first := *body
	first.Compound, first.OrderBy, first.Limit, first.Offset = nil, nil, nil, nil
	total, ok := cArmRowEst(&first, prog.Compound.arms[0], body.Limit, compile, seed, seedOK)
	if !ok {
		return 0, false
	}
	for k, a := range body.Compound {
		if a.Op != "UNION ALL" || a.Stmt == nil {
			return 0, false
		}
		n, ok := cArmRowEst(a.Stmt, prog.Compound.arms[k+1], nil, compile, seed, seedOK)
		if !ok {
			return 0, false
		}
		total = logEstAdd(total, n)
		if body.Limit != nil && *body.Limit > 0 {
			if l := logEstFromInt(uint64(*body.Limit)); total > l {
				total = l
			}
		}
	}
	return total, true
}

// cArmRowEst is one simple select's nSelectRow: 320 (tag-select-0650), lowered
// by an integer LIMIT (computeLimitRegisters, select.c:2543), then the WHERE
// loop's row estimate for a plain select (select.c:8295), 66 at most for a
// GROUP BY (select.c:8386) and 0 for an aggregate without one (select.c:8403).
// A window function moves the select into a subquery of a new outer select
// (sqlite3WindowRewrite) whose one loop scans it, so the estimate passes
// through unchanged.
//
// compile compiles a statement where body's FROM clause resolves, for the
// window case; nil when there is none.
func cArmRowEst(body *SelectStmt, prog *Program, limit *int64, compile func(*SelectStmt) *Program, seed logEst, seedOK bool) (logEst, bool) {
	if body.LimitParam != nil || len(body.Compound) != 0 || body.ValuesArms != 0 {
		return 0, false
	}
	n := logEst(320)
	if limit != nil && *limit > 0 {
		if l := logEstFromInt(uint64(*limit)); n > l {
			n = l
		}
	}
	if body.Distinct && len(body.OrderBy) != 0 {
		// The DISTINCT-to-GROUP-BY rewrite (select.c ~8180) is not modelled.
		return 0, false
	}
	if cHasWindow(body) && (cSelectIsAggregate(body) || body.Distinct || len(body.OrderBy) != 0) {
		return 0, false
	}
	if cSelectIsAggregate(body) {
		if len(body.GroupBy) == 0 {
			return 0, true // select.c:8403
		}
		if n > 66 { // select.c:8386
			n = 66
		}
		return n, true
	}
	if len(body.From) == 0 {
		// wherePathSolver over no loop leaves nRowOut at the seed path's
		// MIN(nQueryLoop, 48) (where.c:5921, :6250), less 30 for DISTINCT
		// (where.c:7118); seed is pParse->nQueryLoop where the subquery is
		// coded.
		if !seedOK {
			return 0, false
		}
		r := min(seed, 48)
		if body.Distinct {
			r -= 30
		}
		if r < n {
			n = r
		}
		return n, true
	}
	if cHasWindow(body) {
		// The rewrite's inner select keeps the FROM and WHERE and sorts by
		// the window's PARTITION BY and ORDER BY; with no WHERE term every
		// loop of one table outputs its whole row count, so the order cannot
		// change the estimate, which is a plain scan's.
		if body.Where != nil || len(body.From) != 1 || body.From[0].Subquery != nil || compile == nil {
			return 0, false
		}
		prog = compile(&SelectStmt{Columns: []SelectColumn{{Expr: LiteralExpr{Val: Value{Typ: Int, I: 1}}, Text: "1"}}, From: body.From})
	}
	if prog == nil || !prog.planNRowOK {
		return 0, false
	}
	if prog.planNRow < n { // select.c:8295
		n = prog.planNRow
	}
	return n, true
}

// cCoroutineVerdict ports fromClauseTermCanBeCoroutine (select.c:7266) for
// items[i], a subquery C does not flatten, every earlier subquery also being
// one C does not flatten (the caller's check). certain is false where the rule
// reads something this does not model.
func (p *ReadOnlyPager) cCoroutineVerdict(parent *SelectStmt, items []FromItem, i int, pushFree []bool) (coroutine, certain bool) {
	_, cte, view, _ := p.cSubqueryBody(items[i])
	if cte != nil {
		if cte.m10d == cteM10dYes { // (2a)
			return false, true
		}
		if cte.nUse >= 2 && cte.m10d != cteM10dNo { // (2b)
			return false, true
		}
		switch p.cteRefPlacementIn(parent, items, i, cte) {
		case cteRefCoroutine:
			return true, true
		case cteRefMaterialized:
			return false, true
		}
		return false, false
	}
	for _, it := range items {
		if it.Join == JoinRight || it.Join == JoinFull { // (3)
			return false, true
		}
		if it.UpdateTarget { // (1c-iii)
			return false, true
		}
	}
	if view {
		// (5): a later reference to the same view is a self-join, unless a
		// push-down changed one of them (SF_PushDown), which needs a WHERE or
		// ON term to push and is not modelled.
		for _, it := range items[i+1:] {
			if cSameView(items[i], it) {
				return false, pushFree != nil && pushFree[i]
			}
		}
	}
	if i == 0 { // (1a), (1b)
		return true, true
	}
	for k := i; ; k-- {
		it := items[k]
		if it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull || it.CrossKeyword { // (1c-ii)
			return false, true
		}
		if k == 0 {
			break
		}
		if _, _, _, sq := p.cSubqueryBody(items[k-1]); sq { // (1c-i)
			return false, true
		}
	}
	return true, true
}

// markCoroutineReruns finds the FROM items C runs as an inner-loop co-routine
// over a body that is not deterministic, and makes each re-run its body at
// every rewind under C's own loop order. It answers an error -- a decline --
// for such a body whose coding or loop order it cannot follow, rather than the
// once-per-statement answer this engine would otherwise give.
func markCoroutineReruns(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) error {
	if p == nil || c == nil || stmt == nil || len(srcs) < 2 {
		return nil
	}
	volatile := false
	for i := range srcs {
		if srcs[i].derived == nil && (srcs[i].cteItem == nil || srcs[i].cteItem.queue != nil || srcs[i].cteItem.recSelf != nil) {
			continue
		}
		if len(stmt.From) != len(srcs) {
			volatile = true // checked below, once the FROM list is known to line up
			break
		}
		if body, _, _, ok := p.cSubqueryBody(stmt.From[i]); ok && selectCallsNondeterministicFunc(body) {
			volatile = true
		}
	}
	if !volatile {
		return nil
	}
	decline := func(why string) error {
		return fmt.Errorf("%w: a FROM-clause subquery that is not deterministic, %s", errVDBEUnsupported, why)
	}
	items := stmt.From
	if len(items) != len(srcs) {
		return decline("in a FROM clause whose items do not line up with the compiled sources")
	}
	for _, it := range items {
		if it.GroupLen != 0 || len(it.NestedGroupSpan) != 0 {
			return decline("beside a parenthesized join")
		}
	}
	pushFree := cPushDownFree(stmt, srcs)
	// Every subquery left in the FROM clause must be one C does not flatten
	// either, or C's FROM clause is not this one.
	type subq struct {
		body *SelectStmt
		cte  *cteBinding
		co   bool
	}
	subs := map[int]subq{}
	rerun := map[int]bool{}
	flatKept := map[int]bool{}
	for i := range srcs {
		body, cte, _, ok := p.cSubqueryBody(items[i])
		if !ok {
			if srcs[i].derived != nil || srcs[i].cteItem != nil {
				return decline("beside a subquery source this cannot place")
			}
			continue
		}
		if srcs[i].derived == nil && srcs[i].cteItem == nil {
			return decline("beside a FROM item this cannot place")
		}
		flat, certain := p.cFlattens(stmt, items, i)
		if !certain {
			return decline("beside a subquery whose flattening is not followed")
		}
		if flat {
			// substExpr copies each result expression into every reference
			// (select.c:3797); r41Flatten does that only where it can, and
			// left this one in place. That is still the same answer when
			// nothing that is not deterministic moves: see
			// cFlattenKeepsAnswer.
			if !p.cFlattenKeepsAnswer(body) {
				return decline("in a join C flattens it into")
			}
			if srcs[i].derived == nil && srcs[i].cteItem == nil {
				return decline("beside a FROM item this cannot place")
			}
			flatKept[i] = true
			continue
		}
		co, certain := p.cCoroutineVerdict(stmt, items, i, pushFree)
		if !certain {
			return decline("whose co-routine placement is not followed")
		}
		subs[i] = subq{body: body, cte: cte, co: co}
		// An item 0 that a CROSS JOIN (or nothing) follows is the outer loop
		// whatever it costs: run once, as here.
		forcedOuter := i == 0 && (len(items) == 1 || items[1].CrossKeyword)
		if co && !forcedOuter && selectCallsNondeterministicFunc(body) {
			rerun[i] = true
		}
	}
	if len(rerun) == 0 {
		return nil
	}
	if len(flatKept) != 0 {
		// C's FROM clause holds that body's tables where this one holds the
		// subquery, so its loop order is not this FROM clause's.
		return decline("beside a subquery C flattens")
	}
	der := map[int]wherePlanDerivedItem{}
	for i, sq := range subs {
		var prog *Program
		if srcs[i].derived != nil {
			prog = srcs[i].derived
		} else if sq.cte != nil {
			prog = p.cteBodyProgram(c, items[i].Table, sq.cte)
		}
		compile := func(s *SelectStmt) *Program {
			if sq.cte != nil {
				defer p.enterCTEBodyScope(items[i].Table, sq.cte)()
			}
			sp, err := compileSubProgram(p, s, c.outer)
			if err != nil {
				return nil
			}
			return sp
		}
		est, eok := cSelectRowEst(sq.body, prog, compile, c.nQueryLoop, c.outer == nil || c.nQueryLoopKnown)
		if sq.cte != nil && !sq.co {
			// A reference that reuses an earlier materialization keeps the
			// table's own default estimate, 200 (select.c:5752; the reuse at
			// select.c:8086 sets only pSub's). The materialization is earlier
			// when a reference before this one in the FROM clause made it;
			// one made anywhere else is not followed (nUse counts more
			// references than this FROM clause holds).
			reuse, inList := false, 0
			for k := range items {
				if osq, ok := subs[k]; ok && osq.cte == sq.cte {
					inList++
					if k < i && !osq.co {
						reuse = true
					}
				} else if k > i {
					if _, octe, _, ok := p.cSubqueryBody(items[k]); ok && octe == sq.cte {
						inList++
					}
				}
			}
			switch {
			case reuse:
				est, eok = whereDefaultRowLogEst, true
			case inList != sq.cte.nUse:
				eok = false
			}
		}
		if !eok {
			return decline("whose row estimate, or a neighbour's, is not followed")
		}
		subOB, sok := cSubqueryOrderBy(stmt, items, i, sq.body, sq.cte)
		if !sok {
			return decline("beside a subquery whose ORDER BY is not followed")
		}
		der[i] = wherePlanDerivedItem{nRow: est, correlated: prog != nil && prog.Correlated, subOB: subOB, coroutine: sq.co}
	}
	var auto []bool
	order, keys, planNRow, ok := wherePlanMultiTableOrderWith(p, c, srcs, stmt, der, &auto)
	if !ok || len(auto) != len(order) {
		return decline("in a join whose loop order is not followed")
	}
	needed := false
	for level, iTab := range order {
		if rerun[iTab] && level > 0 && !auto[level] {
			srcs[iTab].coroutineRerun = true
			needed = true
		}
	}
	if !needed {
		// Outermost, or filled once into an automatic index: one run, which is
		// what the materialization already does.
		return nil
	}
	// The loops outside each re-run co-routine must be C's, so the whole plan
	// is: the same hand-off markWherePlanEligibility makes.
	c.planNRow, c.planNRowOK = planNRow, c.outer == nil || c.nQueryLoopKnown
	noAutoIdx := !p.AutomaticIndex()
	for level, iTab := range order {
		k := keys[level]
		if k == nil {
			k = &autoIndexKey{}
		}
		k.level = level
		srcs[iTab].idxOrderKey = k
	}
	for i := range srcs {
		srcs[i].wherePlanOK = true
		srcs[i].noAutoIndex = noAutoIdx
		if srcs[i].on != nil && !srcs[i].left && !srcs[i].rightOuter {
			srcs[i].onToWhere = true
		}
	}
	return nil
}

// cteBodyProgram compiles a CTE's body, in the scope it was defined in, for
// its row estimate only: a reference's rows are produced at run time
// (runCTEOnce), so nothing else compiles it here. nil when it does not
// compile.
func (p *ReadOnlyPager) cteBodyProgram(c *compiler, name string, b *cteBinding) *Program {
	for _, seen := range p.cteExpansion {
		if seen.b == b {
			return nil
		}
	}
	defer p.enterCTEBodyScope(name, b)()
	prog, err := compileSubProgram(p, b.core, c.outer)
	if err != nil {
		return nil
	}
	return prog
}

// cOrderByAllResultCols is flattenSubquery's restriction (18) test: every
// ORDER BY term of parent has iOrderByCol set, which resolveOrderGroupBy does
// for an ordinal, a result alias, and a term sqlite3ExprCompare finds equal to
// a result expression (resolve.c:1815-1860).
func cOrderByAllResultCols(parent *SelectStmt) tri {
	for _, ot := range parent.OrderBy {
		e := ot.Expr
		if _, ok := orderByOrdinal(whereSkipCollate(e)); ok {
			continue
		}
		matched, maybe := false, false
		if ce, ok := e.(ColumnExpr); ok && ce.Qualifier == "" {
			for _, c := range parent.Columns {
				if !c.Star && c.HasAlias && equalFoldName(c.Alias, ce.Name) {
					matched = true
				}
			}
		}
		for _, c := range parent.Columns {
			if c.Star {
				maybe = true
				continue
			}
			if exprEqual(e, c.Expr) {
				matched = true
			} else if exprEqualIgnoringQualifier(e, c.Expr) {
				maybe = true
			}
		}
		if matched {
			continue
		}
		if maybe {
			return triUnknown
		}
		return triNo
	}
	return triYes
}

// cSubqueryOrderBy is the ORDER BY a subquery's loop carries into the
// enclosing plan (where.c:4180): the body's own, unless tag-select-0230
// deleted it (select.c:7811-7848: not an aggregate, not a MATERIALIZED CTE, no
// LIMIT, and the parent orders or joins). Each term's iOrderByCol is the result
// column resolveOrderGroupBy matched it to. ok is false for a term whose match
// is not followed.
func cSubqueryOrderBy(parent *SelectStmt, items []FromItem, i int, body *SelectStmt, cte *cteBinding) ([]whereSubOB, bool) {
	if body == nil || len(body.OrderBy) == 0 {
		return nil, true
	}
	if len(body.Compound) != 0 {
		return nil, false
	}
	m10d := cte != nil && cte.m10d == cteM10dYes
	if !m10d && !cSelectIsAggregate(body) && (len(parent.OrderBy) != 0 || len(items) > 1) &&
		body.Limit == nil && body.LimitParam == nil && !r41OrderAgg(parent) && !parent.hasUpdateTarget() {
		return nil, true
	}
	out := make([]whereSubOB, len(body.OrderBy))
	for k, ot := range body.OrderBy {
		col := -1
		e := ot.Expr
		if _, isColl := e.(CollateExpr); isColl {
			return nil, false
		}
		if n, ok := orderByOrdinal(e); ok {
			if n < 1 || int(n) > len(body.Columns) {
				return nil, false
			}
			col = int(n) - 1
		} else {
			for j, c := range body.Columns {
				if c.Star {
					return nil, false
				}
				if ce, ok := e.(ColumnExpr); ok && ce.Qualifier == "" && c.HasAlias && equalFoldName(c.Alias, ce.Name) {
					col = j
					break
				}
				if exprEqual(e, c.Expr) {
					col = j
					break
				}
				if exprEqualIgnoringQualifier(e, c.Expr) {
					return nil, false
				}
			}
		}
		out[k] = whereSubOB{col: col, desc: ot.Desc, bigNull: wherePlanSortBigNull(ot)}
	}
	return out, true
}

// cSameView is isSelfJoinView's name test (select.c:7086-7090) for two FROM
// items naming views: the same name in the same schema.
func cSameView(a, b FromItem) bool {
	return a.Subquery == nil && b.Subquery == nil && !a.TableFunc && !b.TableFunc &&
		a.Table != "" && equalFoldName(a.Table, b.Table) && equalFoldName(a.Schema, b.Schema)
}

// cPushDownFree reports, per FROM item, that no WHERE term can be pushed into
// it (pushDownWhereTerms, which marks the body SF_PushDown): every conjunct
// names some other item, so none is a single-table constraint on this one
// (sqlite3ExprIsSingleTableConstraint, which a constant term also is). An ON
// clause, or a conjunct holding a subquery, leaves every item unknown (false).
func cPushDownFree(stmt *SelectStmt, srcs []joinSource) []bool {
	out := make([]bool, len(srcs))
	if len(stmt.From) != len(srcs) {
		return out
	}
	for _, it := range stmt.From {
		if it.On != nil || len(it.Using) != 0 || it.Natural {
			return out
		}
	}
	scopes := tableScopesOf(srcs)
	conj := splitTopLevelAnd(stmt.Where)
	for _, cj := range conj {
		if containsSubquery(cj) {
			return out
		}
	}
	for k := range out {
		free := true
		for _, cj := range conj {
			refs := map[int]bool{}
			collectTableRefs(cj, scopes, refs)
			other := false
			for r := range refs {
				if r != k {
					other = true
				}
			}
			if !other {
				free = false
				break
			}
		}
		out[k] = free
	}
	return out
}

// shareSelfJoinViews ports isSelfJoinView's reuse (select.c:8092-8100): a view
// named again in the same FROM clause is not materialized a second time; the
// later reference opens a copy of the earlier one's ephemeral table
// (OP_OpenDup). Here the later reference reads the earlier one's run-once
// slot. Only where no push-down could have made the two bodies differ, and
// where C materializes the earlier one rather than running it as a co-routine.
func shareSelfJoinViews(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) {
	if p == nil || stmt == nil || len(stmt.From) != len(srcs) || len(srcs) < 2 {
		return
	}
	pushFree := cPushDownFree(stmt, srcs)
	for _, it := range stmt.From {
		if it.GroupLen != 0 || len(it.NestedGroupSpan) != 0 || it.Join == JoinRight || it.Join == JoinFull {
			return
		}
	}
	for j := range srcs {
		if srcs[j].derived == nil || srcs[j].derived.Correlated || !pushFree[j] {
			continue
		}
		if _, cte, view, ok := p.cSubqueryBody(stmt.From[j]); !ok || !view || cte != nil {
			continue
		}
		for k := 0; k < j; k++ {
			if srcs[k].derived == nil || srcs[k].derived.Correlated || srcs[k].dbIdx != srcs[j].dbIdx ||
				!pushFree[k] || !cSameView(stmt.From[k], stmt.From[j]) {
				continue
			}
			srcs[j].derivedSlot = srcs[k].derivedSlot
			srcs[j].selfJoinShared, srcs[k].selfJoinShared = true, true
			break
		}
	}
}

// cFromEntry is one item of a FROM clause as sqlite3Select's flattening loop
// leaves it.
type cFromEntry struct {
	it FromItem // the join fields; the name, for a table or view
	// sub, when non-nil, is the item's subquery: inline, a CTE reference
	// (cte), a view, or a parenthesized join's "SELECT * FROM ..." (group).
	sub   *SelectStmt
	cte   *cteBinding
	group bool
	// orig is the item's index in the FROM clause as written; -1 for an item
	// a view's or CTE's body brought up, whose names resolve in that body's
	// own scope, not this one.
	orig int
	// span is a group's member indexes in the FROM clause as written.
	span []int
}

// cEntries is items as sqlite3Select's FROM list: a parenthesized join group
// is one subquery item (the parser's SF_NestedFrom select). ok is false for a
// nested group, which this does not model.
func (p *ReadOnlyPager) cEntries(items []FromItem) ([]cFromEntry, bool) {
	var out []cFromEntry
	for i := 0; i < len(items); i++ {
		it := items[i]
		if len(it.NestedGroupSpan) != 0 || it.NestedNonLeading || it.NestedGroupID != 0 {
			return nil, false
		}
		if it.GroupLen > 0 {
			gl := it.GroupLen
			if i+gl > len(items) {
				return nil, false
			}
			span := make([]FromItem, gl)
			copy(span, items[i:i+gl])
			span[0].Join, span[0].On, span[0].Using, span[0].Natural, span[0].GroupLen = JoinCross, nil, nil, false, 0
			span[0].CrossKeyword = false
			idx := make([]int, gl)
			for k := range idx {
				idx[k] = i + k
				if k > 0 && (items[i+k].GroupLen > 0 || len(items[i+k].NestedGroupSpan) != 0) {
					return nil, false // nested
				}
			}
			out = append(out, cFromEntry{
				it:  FromItem{Join: it.Join, On: it.On, Using: it.Using, Natural: it.Natural, CrossKeyword: it.CrossKeyword, Alias: it.GroupAlias},
				sub: &SelectStmt{Columns: []SelectColumn{{Star: true}}, From: span}, group: true, orig: -1, span: idx,
			})
			i += gl - 1
			continue
		}
		e := cFromEntry{it: it, orig: i}
		if body, cte, _, ok := p.cSubqueryBody(it); ok {
			e.sub, e.cte = body, cte
		}
		out = append(out, e)
	}
	return out, true
}

func cEntryItems(es []cFromEntry) []FromItem {
	out := make([]FromItem, len(es))
	for k, e := range es {
		out[k] = e.it
	}
	return out
}

// cPostFlatten runs sqlite3Select's flattening loop (select.c:7770-7890) over
// entries: each subquery C absorbs is replaced by its body's FROM items, the
// first taking the subquery's place in the join, and the walk restarts. ok is
// false where a verdict is not certain, or a body brings up a subquery whose
// name resolves outside this scope.
func (p *ReadOnlyPager) cPostFlatten(parent *SelectStmt, entries []cFromEntry) ([]cFromEntry, bool) {
	cur := append([]cFromEntry(nil), entries...)
	state := *parent
	for guard := 0; guard < 64; guard++ {
		merged := false
		for i := range cur {
			if cur[i].sub == nil {
				continue
			}
			if cur[i].orig < 0 && !cur[i].group {
				return nil, false
			}
			items := cEntryItems(cur)
			flat, certain := p.cFlattensBody(&state, items, i, cur[i].sub, cur[i].cte)
			if !certain {
				return nil, false
			}
			if !flat {
				continue
			}
			body := cur[i].sub
			var spliced []cFromEntry
			if cur[i].group {
				for k, it := range body.From {
					e := cFromEntry{it: it, orig: cur[i].span[k]}
					if k == 0 {
						e.it.Join, e.it.On, e.it.CrossKeyword = cur[i].it.Join, cur[i].it.On, cur[i].it.CrossKeyword
						e.it.Using, e.it.Natural = cur[i].it.Using, cur[i].it.Natural
					}
					if sb, cte, _, ok := p.cSubqueryBody(it); ok {
						e.sub, e.cte = sb, cte
					}
					spliced = append(spliced, e)
				}
			} else {
				for k, it := range body.From {
					e := cFromEntry{it: it, orig: -1}
					if k == 0 {
						e.it.Join, e.it.CrossKeyword = cur[i].it.Join, cur[i].it.CrossKeyword
					}
					if it.Subquery != nil || it.GroupLen != 0 {
						e.sub = it.Subquery
						if e.sub == nil {
							return nil, false
						}
					} else if !it.TableFunc && it.Schema == "" {
						// A name in a view or CTE body: whether it is a table,
						// a view or a CTE is that body's scope's question.
						if _, cte, _, ok := p.cSubqueryBody(it); ok || cte != nil {
							return nil, false
						}
					}
					spliced = append(spliced, e)
				}
				if body.Where != nil && state.Where == nil {
					state.Where = body.Where
				}
			}
			next := append([]cFromEntry(nil), cur[:i]...)
			next = append(next, spliced...)
			cur = append(next, cur[i+1:]...)
			state.From = cEntryItems(cur)
			merged = true
			break
		}
		if !merged {
			return cur, true
		}
	}
	return nil, false
}

// cteRefPlacementIn is cteRefPlacementOf over parent, the SELECT whose FROM
// clause items is: C decides on the FROM list its flattener leaves, so that
// list is built first (cPostFlatten), a parenthesized join being a subquery
// like any other.
func (p *ReadOnlyPager) cteRefPlacementIn(parent *SelectStmt, items []FromItem, i int, b *cteBinding) cteRefPlacement {
	if parent == nil {
		return cteRefUnknown
	}
	entries, ok := p.cEntries(items)
	if !ok {
		return cteRefUnknown
	}
	final, ok := p.cPostFlatten(parent, entries)
	if !ok {
		return cteRefUnknown
	}
	for k, e := range final {
		if e.group {
			for _, m := range e.span {
				if m == i {
					// Coded inside the group's own select, over the group's
					// members, which is where its placement is decided.
					sub := e.sub
					return p.cteRefPlacementIn(sub, sub.From, i-e.span[0], b)
				}
			}
			continue
		}
		if e.orig == i {
			if e.cte != b {
				return cteRefUnknown
			}
			return cRefPlacementInList(final, k, b)
		}
	}
	// The reference itself was flattened away: not a placement at all.
	return cteRefUnknown
}

// cRefPlacementInList is fromClauseTermCanBeCoroutine (select.c:7266) for
// list[t], a reference to b, on the FROM list sqlite3Select codes.
func cRefPlacementInList(list []cFromEntry, t int, b *cteBinding) cteRefPlacement {
	for _, e := range list {
		// (3): a RIGHT JOIN anywhere marks the first item JT_LTORJ.
		if e.it.Join == JoinRight || e.it.Join == JoinFull {
			return cteRefMaterialized
		}
		// (1c-iii): UPDATE ... FROM.
		if e.it.UpdateTarget {
			return cteRefMaterialized
		}
	}
	// (5): a later reference to the same CTE (isSelfJoinView). Inside a view
	// every copy of the body has its own selId, so none matches.
	if !b.inView {
		for _, e := range list[t+1:] {
			if e.cte == b && e.orig >= 0 {
				return cteRefMaterialized
			}
		}
	}
	if t == 0 { // (1a), (1b)
		return cteRefCoroutine
	}
	for k := t; ; k-- {
		it := list[k].it
		if it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull || it.CrossKeyword { // (1c-ii)
			return cteRefMaterialized
		}
		if k == 0 {
			break
		}
		if list[k-1].sub != nil { // (1c-i)
			return cteRefMaterialized
		}
	}
	return cteRefCoroutine
}

// cFlattenKeepsAnswer reports that flattening body into its parent, which
// this engine did not do, cannot change an answer: no result column, WHERE,
// GROUP BY or ORDER BY of it calls random() or randomblob() (substExpr would
// copy such a call into every reference, and its WHERE would run per joined
// row), and every subquery its FROM clause brings up that is not
// deterministic is materialized once wherever it lands -- a CTE that is AS
// MATERIALIZED, or that the body itself names twice without AS NOT
// MATERIALIZED (select.c:7276's (2a)/(2b); nUse can only grow from there).
func (p *ReadOnlyPager) cFlattenKeepsAnswer(body *SelectStmt) bool {
	if body == nil || len(body.Compound) != 0 {
		return false
	}
	for _, c := range body.Columns {
		if !c.Star && exprCallsNondeterministicFunc(c.Expr) {
			return false
		}
	}
	if exprCallsNondeterministicFunc(body.Where) || exprCallsNondeterministicFunc(body.Having) {
		return false
	}
	for _, g := range body.GroupBy {
		if exprCallsNondeterministicFunc(g) {
			return false
		}
	}
	for _, ot := range body.OrderBy {
		if exprCallsNondeterministicFunc(ot.Expr) {
			return false
		}
	}
	defs := map[string]CTEDef{}
	for _, d := range body.CTEs {
		defs[r33sFoldIdent(d.Name)] = d
	}
	uses := map[string]int{}
	for _, it := range body.From {
		if it.Subquery == nil && it.Schema == "" && !it.TableFunc {
			uses[r33sFoldIdent(it.Table)]++
		}
	}
	for _, it := range body.From {
		if exprCallsNondeterministicFunc(it.On) {
			return false
		}
		if it.Subquery != nil {
			if selectCallsNondeterministicFunc(it.Subquery) {
				return false
			}
			continue
		}
		if it.TableFunc || it.Schema != "" {
			continue
		}
		name := r33sFoldIdent(it.Table)
		if d, ok := defs[name]; ok {
			if !selectCallsNondeterministicFunc(d.Select) {
				continue
			}
			if d.Materialized == cteM10dYes || (d.Materialized != cteM10dNo && uses[name] >= 2) {
				continue
			}
			return false
		}
		if sb, _, _, ok := p.cSubqueryBody(it); ok && selectCallsNondeterministicFunc(sb) {
			return false
		}
	}
	return true
}
