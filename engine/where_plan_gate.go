package engine

import (
	"slices"
	"strings"
)

// markWherePlanEligibility decides, once per resolved FROM clause, whether the
// ported SQLite planner (wherePlanOrder) chooses the loop order and each level's
// access path. It is the only place joinSource.wherePlanOK is set; paths that
// skip it keep this package's own order. It also runs the planner and stashes
// the verdict on each source as an autoIndexKey, because the planner needs the
// statement's ORDER BY / GROUP BY / DISTINCT choice (wherePlanSortCtlFor), which
// computeExecOrder cannot see.
//
// Each decline names a part of where.c not reproduced:
//
//   - a nested compile (nQueryLoop is not 0, so the seed path differs);
//   - anything but a plain local rowid base table (subqueries, CTEs, views,
//     vtabs and WITHOUT ROWID take other arms, and subqueries may be flattened);
//   - a RIGHT/FULL join (pinned to FROM order anyway);
//   - a table of 63+ columns (colUsed folds them onto one bit);
//   - a compound SELECT (its ORDER BY belongs to the compound);
//   - whatever wherePlanTermsFrom, wherePlanHasRangeRewrite or
//     wherePlanSortCtlFor refuse.
//
// sqlite_stat1 is loaded and priced as C does (where_plan_stat1.go); stat4 is not
// compiled into the oracle. More than three FROM items, tables declaring
// generated columns (colUsedBitsFor floods only for a reference to one), indexes
// on joined tables, and rowid references are all handled.
func markWherePlanEligibility(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) {
	if p == nil || c == nil || stmt == nil {
		return
	}
	// sqlite3ProcessJoin folds every ON clause into the WHERE during name resolution
	// (select.c:655-658), and name lookup walks the whole FROM list (resolve.c:346),
	// so an INNER join's ON may name a later item (distinct2.test: "... JOIN t102 AS
	// t4 ON (t2.i0 IN t102) ... JOIN t102 AS t2 ..."). So onToWhere is set for plain
	// INNER joins here, before and regardless of the solver gate below;
	// planJoinPushdown buckets the conjunct at the depth binding its tables. Outer
	// joins keep their ON at their own level (NULL-extension semantics).
	//
	// Not when a join group or RIGHT/FULL join is in the FROM: buildJoinPlan and
	// planJoinPushdown then stop moving ON clauses, and emitJoinLevel would skip
	// testing an ON it believes moved, dropping a join condition.
	groupOrRightOuter := groupPresent(srcs)
	if !groupOrRightOuter {
		for _, s := range srcs {
			if s.rightOuter {
				groupOrRightOuter = true
				break
			}
		}
	}
	if !groupOrRightOuter {
		// An ON referencing the UPDATE ... FROM target (joinSource.updateTarget) must
		// never resolve: C detaches the target before resolving (update.c:222-230). So it
		// keeps the narrow position-scoped compileOn path, which cannot see the target
		// (TestOnClauseUpdateFromTargetRefStaysDeclined).
		scopes := tableScopesOf(srcs)
		for i := range srcs {
			if srcs[i].on == nil || srcs[i].left || srcs[i].rightOuter {
				continue
			}
			refs := map[int]bool{}
			collectTableRefs(srcs[i].on, scopes, refs)
			refsTarget := false
			for r := range refs {
				if r >= 0 && r < len(srcs) && srcs[r].updateTarget {
					refsTarget = true
					break
				}
			}
			if !refsTarget {
				srcs[i].onToWhere = true
			}
		}
	}
	if ej, applies, ok := wherePlanExistsToJoin(p, c, srcs, stmt); applies {
		if ok {
			markWherePlanExistsEligibility(p, c, srcs, stmt, ej)
		}
		return
	}
	if len(srcs) == 1 {
		markWherePlanIndexEligibility(p, c, srcs, stmt)
		return
	}
	if !wherePlanMultiTableSources(p, c, srcs, stmt) {
		return
	}

	scopes := tableScopesOf(srcs)
	wherePlanBindOuter(c, scopes)
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}

	// colUsed is stashed on every source BEFORE the plan is attempted, not only
	// on the path that reaches wherePlanOK: it is what findJoinIndexSeek
	// (vdbe_join_seek.go) needs to price rival indexes leading with the same
	// column -- a question that arises precisely on the FROM clauses this
	// function goes on to decline.
	colUsed, colUsedOK := wherePlanColUsed(p, c, jts, scopes, srcs, stmt)
	for i := range srcs {
		srcs[i].colUsedOK = colUsedOK
		if colUsedOK {
			srcs[i].colUsed = colUsed[i]
		}
	}

	order, keys, planNRow, ok := wherePlanMultiTableOrder(p, c, srcs, stmt)
	if !ok {
		return
	}
	for level, iTab := range order {
		if keys[level] != nil && keys[level].multiOr != nil && srcs[iTab].updateTarget {
			// UPDATE ... FROM's own target is scanned through a WRITE cursor,
			// whose rows OpMultiOrSort does not own. Declined rather than
			// reordered by a path that has never seen one.
			return
		}
	}
	// See markWherePlanIndexEligibility's identical stash: compileScanPlain/
	// compileScanSorted read this to bump c.nQueryLoop around their own body.
	c.planNRow, c.planNRowOK = planNRow, c.outer == nil || c.nQueryLoopKnown
	// OUTER JOIN STRENGTH REDUCTION (select.c tag-select-0220). The PLAN was
	// already computed under it -- wherePlanMultiTableOrder applies it to its own
	// local view, since which items are JT_OUTER is an input to whereLoopAddAll's
	// barrier accumulator -- and it is applied to the SOURCES only now, on the
	// path that actually accepts. Doing it earlier would rewrite the FROM clause
	// of statements the port then declined, which is not this gate's to do:
	// applyRiskyCollationOrPlan (collation_or_plan.go) reads srcs[1].left and
	// serves a shape it would otherwise decline.
	wherePlanReduceOuterJoins(jts, scopes, srcs, stmt.Where)

	// PRAGMA automatic_index (whereLoopAddBtree's SQLITE_AutoIndex guard), read here
	// where the pager is and carried on every item. With it off the solver is still
	// exact, with one fewer access method.
	noAutoIdx := !p.AutomaticIndex()
	for level, iTab := range order {
		k := keys[level]
		if k == nil {
			// "This level is the plain ascending-rowid table scan" -- the order
			// this engine already produces, so there is nothing to permute. It
			// still has to be a non-nil verdict, because that is the channel the
			// LEVEL travels on.
			k = &autoIndexKey{}
		}
		k.level = level
		srcs[iTab].idxOrderKey = k
	}
	for i := range srcs {
		srcs[i].wherePlanOK = true
		srcs[i].noAutoIndex = noAutoIdx
		// sqlite3ProcessJoin's move: an INNER join's ON clause becomes an
		// ordinary WHERE term, tested wherever every table it names is bound, so
		// the planner may bind this item before a table its own ON references.
		// An OUTER join's ON is deliberately NOT moved here -- see
		// joinSource.onToWhere and this file's outer-join note.
		if srcs[i].on != nil && !srcs[i].left && !srcs[i].rightOuter {
			srcs[i].onToWhere = true
		}
	}
}

// wherePlanMultiTableSources is the per-SOURCE half of the gate above: the
// statement-shape declines that do not depend on the WHERE clause. Split out so
// markWherePlanEligibility can stash colUsed after them and before the plan.
func wherePlanMultiTableSources(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) bool {
	if len(srcs) < 2 || len(srcs) > 64 {
		// 64 is BMS: sqlite3WhereBegin refuses a longer FROM clause outright.
		// See wherePlanOrder.
		return false
	}
	// A trigger body's or a register-backed row's names plan as constants
	// (wherePlanBoundOuter); a live enclosing ROW (rowOuter) is a compile done
	// fresh for one row, whose nQueryLoop nothing here knows.
	if c.rowOuter != nil {
		return false
	}
	if len(stmt.Compound) > 0 || len(stmt.From) != len(srcs) {
		return false
	}
	for i := range stmt.From {
		// A parenthesized join GROUP is an atomic relation to computeExecOrder
		// (join.go), which answers literal FROM order for one and never consults
		// this port at all -- so claiming to have decided the order would be a
		// claim about an order nothing here produced. It also breaks the
		// positional stmt.From[i] <-> srcs[i] alignment the INDEXED BY / NOT
		// INDEXED lookup below depends on.
		if stmt.From[i].GroupLen > 0 {
			return false
		}
	}
	for i := range srcs {
		s := &srcs[i]
		// Every virtual-table source is excluded from this solver. For an fts MATCH
		// correlated to another item, C forces the vtab after that item through costs
		// (where.c:4390; fts3.c:1636's 1e50; fts5_main.c:657's SQLITE_CONSTRAINT). With
		// exactly two items that is the whole question, handled outside this solver by
		// ftsMatchForcedOrder (used by computeExecOrder and anchorLoopOrderProvable).
		// With three or more, the other items' order is a cost tie this does not model.
		//
		// WITHOUT ROWID tables are allowed: their probe chain is just the PRIMARY KEY
		// index (where.c:4034, wherePlanWithoutRowidIndexList, which declines a second
		// index), whose order is the PK walk this engine already does.
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil ||
			s.catalogScope != scopeAny ||
			s.rightOuter || s.tbl == nil || (s.dbIdx != 0 && !p.dbIdxIsTemp(s.dbIdx)) {
			return false
		}
		// colUsed folds every column at or past bit 63 onto that one bit
		// (resolve.c:196-198's "if(n>=BMS) n=BMS-1"), which this port does not
		// reproduce -- see wherePlanColUsed. A table merely DECLARING a
		// generated column is no longer excluded here: colUsedBitsFor
		// (wherePlanColUsed) reproduces sqlite3ExprColUsed's actual gate,
		// which floods colUsed only when a reference NAMES that specific
		// generated column (resolve.c:176-198), not for every table that
		// happens to have one.
		if len(s.tbl.cols) >= 63 {
			return false
		}
	}
	return true
}

// wherePlanMultiTableOrder runs the ported planner over a resolved multi-table
// FROM clause and answers the nesting order plus the b-tree each level is walked
// through. It never mutates srcs, so wherePlanIndexOrderDecided can ask the
// SAME question the codegen answers without side effects.
// nRow is wherePlanResult.nRow for the whole multi-table plan, meaningless
// when ok is false. See wherePlanOrder's identical return.
func wherePlanMultiTableOrder(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) ([]int, []*autoIndexKey, logEst, bool) {
	if p == nil || c == nil || stmt == nil {
		return nil, nil, 0, false
	}
	if !wherePlanMultiTableSources(p, c, srcs, stmt) {
		return nil, nil, 0, false
	}
	// The IN shapes sqlite3FindInIndex resolves to something other than an
	// ephemeral sorted set -- see wherePlanHasRangeRewrite.
	conjuncts := splitTopLevelAnd(stmt.Where)
	for i := range srcs {
		conjuncts = append(conjuncts, splitTopLevelAnd(srcs[i].on)...)
	}
	for _, cj := range conjuncts {
		if wherePlanHasRangeRewrite(cj) {
			return nil, nil, 0, false
		}
	}

	scopes := tableScopesOf(srcs)
	wherePlanBindOuter(c, scopes)
	// An ON naming a later FROM item is rejected by C (selectCheckOnClausesExpr,
	// select.c:7409-7455) only when that ON is an outer join's, or an inner join's
	// while the FROM has a RIGHT/FULL join anywhere (select.c:7403). Otherwise it is
	// valid ("SELECT * FROM t0 JOIN t1 ON (t2.x NOTNULL) LEFT JOIN t2 ON 0"), and
	// onToWhere makes it safe by evaluating it once the referenced table is bound.
	//
	// A reference to the UPDATE ... FROM target is declined unconditionally, as C
	// detaches the target before resolving (update.c:222-230). hasRightJoin is always
	// false here today (rightOuter sources are excluded earlier) but is computed to
	// match C's rule.
	hasRightJoin := false
	for i := range srcs {
		if srcs[i].rightOuter {
			hasRightJoin = true
			break
		}
	}
	for i := range srcs {
		refs := map[int]bool{}
		collectTableRefs(srcs[i].on, scopes, refs)
		for r := range refs {
			if r <= i {
				continue
			}
			if srcs[r].updateTarget || srcs[i].left || srcs[i].rightOuter || hasRightJoin {
				return nil, nil, 0, false
			}
		}
	}
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}
	// select.c runs its OUTER JOIN STRENGTH REDUCTION before the planner, so the
	// plan is computed over the REDUCED join types. Only the local view is
	// reduced here; markWherePlanEligibility applies it to the sources once the
	// plan has succeeded.
	reduced := wherePlanOuterJoinsReduced(jts, scopes, srcs, stmt.Where)
	for i := range srcs {
		left := srcs[i].left && !reduced[i]
		jts[i] = joinedTable{
			tbl: srcs[i].tbl, on: srcs[i].on,
			left: left, rightOuter: srcs[i].rightOuter,
			onToWhere: srcs[i].on != nil && !left && !srcs[i].rightOuter,
		}
	}
	// colUsed. An INEXACT one is not automatically a decline: colUsed decides
	// whether a REAL index is covering (whereLoopAddBtree's "m = pSrc->colUsed &
	// pProbe->colNotIdxed"), and it decides the extra key columns
	// constructAutomaticIndex appends -- but with no real index anywhere the
	// first use is unreachable, and the second costs only the automatic index's
	// KEY, which wherePlanAutoIndexKey then declines to reproduce, leaving that
	// level's pre-port scan order. That is exactly what this gate did before it
	// planned anything.
	colUsed, colUsedOK := wherePlanColUsed(p, c, jts, scopes, srcs, stmt)
	pWhere, pJts, ok := wherePlanPropagateConstants(jts, scopes, stmt.Where)
	if !ok {
		return nil, nil, 0, false
	}
	terms, ok := wherePlanTermsFrom(pJts, scopes, pWhere)
	if !ok {
		return nil, nil, 0, false
	}
	sortCtl, ok := wherePlanSortCtlFor(jts, scopes, stmt)
	if !ok {
		return nil, nil, 0, false
	}

	items := make([]whereItem, len(srcs))
	for i := range srcs {
		// stmt, not nil: a pure expression index is source-generic, and a partial index
		// is chained with its condition, whereUsablePartialIndex checking it against the
		// terms (ON clauses included), as in C (where_plan_partial.go).
		idxs, szTabRow, iok := wherePlanPartialIndexList(p, srcs[i].tbl, srcs[i].scope.tableName,
			stmt.From[i].IndexedBy, stmt.From[i].NotIndexed, stmt)
		if !iok || len(idxs) == 0 {
			return nil, nil, 0, false
		}
		nRowLogEst, szTabRow := wherePlanItemStats(p.forDB(srcs[i].dbIdx), srcs[i].tbl, idxs, szTabRow)
		if !colUsedOK && len(idxs) > 1 {
			// idxs[0] is the fake sPk, so len > 1 means this item has a REAL
			// index whose covering test colUsed answers. Then an inexact one is
			// a decline after all. A WITHOUT ROWID table never reaches this:
			// its chain has no sPk and is exactly one index long (see
			// wherePlanWithoutRowidIndexList), and that one index is
			// unconditionally covering (build.c:2435), so
			// whereLoopAddBtree's "m = pSrc->colUsed & pProbe->colNotIdxed"
			// (where.c:4194) never reads colUsed for it either.
			return nil, nil, 0, false
		}
		items[i] = whereItem{
			outer: jts[i].left || jts[i].rightOuter, cross: srcs[i].cross, fromExists: scopes[i].fromExists,
			cols: srcs[i].tbl.cols, tabName: r33sFoldIdent(srcs[i].tbl.name),
			colUsedOK: colUsedOK, noRowid: srcs[i].tbl.withoutRowid,
			ipkIndex: srcs[i].tbl.ipkIndex, szTabRow: szTabRow, idxs: idxs,
			nRowLogEst: nRowLogEst, nRowLogEstSet: true,
			indexedBy: stmt.From[i].IndexedBy != "", notIndexed: stmt.From[i].NotIndexed,
		}
		if colUsedOK {
			items[i].colUsed = colUsed[i]
		}
	}

	in := wherePlanInput{
		items: items, terms: terms, sort: sortCtl,
		noAutoIndex: !p.AutomaticIndex(), nQueryLoop: c.nQueryLoop,
		reverseOrder: p.ReverseUnorderedSelects(),
	}
	var order []int
	var keys []*autoIndexKey
	var planNRow logEst
	if c.outer != nil && !c.nQueryLoopKnown {
		// C's nQueryLoop here is not known -- see wherePlanSeedInvariant, which
		// the single-table path shares: the join is taken only if every seed
		// orders and walks it the same way.
		order, keys, ok = wherePlanSeedInvariantOrder(in)
	} else {
		order, keys, planNRow, ok = wherePlanOrder(in)
	}
	if !ok || !onClausesStayInScope(jts, scopes, order) {
		return nil, nil, 0, false
	}
	// PRAGMA reverse_unordered_selects: whereReverseScanOrder (where.c:6727) flips
	// every item's direction when there is no pOrderBy (where.c:7126), after the
	// solver, so only directions change. sortCtl is read after plan() settles
	// pOrderBy. Its materialized-CTE exception (where.c:6731) cannot arise:
	// wherePlanMultiTableSources declines CTEs.
	if p.ReverseUnorderedSelects() && sortCtl.nOrderBy() == 0 {
		for i := range keys {
			if keys[i] != nil && keys[i].multiOr != nil {
				// The case-5 arm reads no bRev of its own; its sub-scans were
				// reversed where they were planned (wherePlanJoinMultiOrKey).
				continue
			}
			keys[i] = whereReversedScanKey(keys[i])
		}
	}
	return order, keys, planNRow, true
}

// whereReversedScanKey returns the key of one level's rows walked backwards
// (whereReverseScanOrder). Flipping each column's direction is safe here because
// a reversal from an ORDER BY (path.rev) and from the pragma are mutually
// exclusive, so the key was built unreversed. nil (the rowid scan) becomes the
// descending-rowid key. desc is grown to len(cols), since an automatic index's
// key leaves desc nil (all ascending); flipping only existing entries left it
// forward ("join auto index" in pragma_reverse_r33q_test.go).
func whereReversedScanKey(k *autoIndexKey) *autoIndexKey {
	if k == nil {
		return &autoIndexKey{cols: []int{xnRowid}, colls: []string{"BINARY"}, desc: []bool{true}}
	}
	for len(k.desc) < len(k.cols) {
		k.desc = append(k.desc, false)
	}
	for i := range k.desc {
		k.desc[i] = !k.desc[i]
	}
	return k
}

// markWherePlanIndexEligibility is the one-table arm: whether the port of
// whereLoopAddBtree / whereLoopAddBtreeIndex decides which index the single scan
// walks, and so the row order (OpAutoIndexOrder permutes the materialized
// cursor; the row set never changes). Declined:
//
//   - a nested compile (the seed path's nRow is not 0);
//   - anything but a plain local rowid base table, or 63+ columns;
//   - a window function, a DISTINCT aggregate argument, a parameter
//     LIMIT/OFFSET, the DISTINCT-to-GROUP-BY rewrite, or an ORDER BY / GROUP BY
//     term not resolvable as resolve.c does (otherwise ORDER BY, GROUP BY and
//     DISTINCT are served);
//   - an IN over an empty list (see wherePlanHasRangeRewrite);
//   - a WHERE conjunct wherePlanTermsFrom cannot use;
//   - an index not rebuildable exactly (wherePlanIndexList).
func markWherePlanIndexEligibility(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) {
	key, nRow, ok := wherePlanSingleTableIndexOrder(p, c, srcs, stmt)
	if !ok {
		return
	}
	// Stashed even when key == nil ("this level is the plain ascending-rowid
	// scan, which is the order this engine already produces" -- see this
	// function's doc comment): the port still fully DECIDED the plan and its
	// row estimate in that case, and compileScanPlain/compileScanSorted need
	// that estimate to bump nQueryLoop around their own body regardless of
	// whether an index-order key was also installed.
	// ...except where the seed was unknown (wherePlanSeedInvariant): the ORDER
	// is decided, the estimate still carries C's unknown nQueryLoop.
	c.planNRow, c.planNRowOK = nRow, c.outer == nil || c.nQueryLoopKnown
	if key == nil {
		return
	}
	srcs[0].idxOrderKey = key
}

// markWherePlanExistsEligibility is markWherePlanEligibility's verdict for a
// statement existsToJoin rewrites (where_plan_exists.go): planned as the
// rewritten join, recorded on the original sources alone. One source takes the
// single-table channel (idxOrderKey, nil for the rowid scan); more take the
// join channel, levels renumbered with the EXISTS items dropped.
func markWherePlanExistsEligibility(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt, ej *wherePlanExistsJoin) {
	scopes := tableScopesOf(srcs)
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}
	if len(srcs) > 1 {
		// Stashed before the plan, as the join path does: findJoinIndexSeek
		// needs it on a FROM clause the plan then declines.
		colUsed, colUsedOK := wherePlanColUsed(p, c, jts, scopes, srcs, stmt)
		for i := range srcs {
			srcs[i].colUsedOK = colUsedOK
			if colUsedOK {
				srcs[i].colUsed = colUsed[i]
			}
		}
	}
	keys, levels, nRow, ok := wherePlanExistsOrder(p, c, stmt, ej)
	if !ok {
		return
	}
	c.planNRow, c.planNRowOK = nRow, c.outer == nil || c.nQueryLoopKnown
	if len(srcs) == 1 {
		srcs[0].idxOrderKey = keys[0]
		return
	}
	wherePlanReduceOuterJoins(jts, scopes, srcs, stmt.Where)
	noAutoIdx := !p.AutomaticIndex()
	for i := range srcs {
		k := keys[i]
		if k == nil {
			k = &autoIndexKey{}
		}
		k.level = levels[i]
		srcs[i].idxOrderKey = k
		srcs[i].wherePlanOK = true
		srcs[i].noAutoIndex = noAutoIdx
		if srcs[i].on != nil && !srcs[i].left && !srcs[i].rightOuter {
			srcs[i].onToWhere = true
		}
	}
}

// wherePlanIndexOrderDecided reports whether the port decided this statement's
// row order (often the rowid scan, needing no key). Used by the aggregate anchor
// guard (anchorPlanOrderProvable) instead of the "any index at all" proxy; it
// makes the same call the codegen makes. For a multi-table FROM, true covers
// both access paths and loop nesting. A descending key under GROUP BY is fine now
// that groupsArriveOutOfKeyOrder handles group emission order.
func wherePlanIndexOrderDecided(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) bool {
	if p == nil || c == nil || stmt == nil {
		return false
	}
	if ej, applies, ok := wherePlanExistsToJoin(p, c, srcs, stmt); applies {
		if !ok {
			return false
		}
		_, _, _, ok = wherePlanExistsOrder(p, c, stmt, ej)
		return ok
	}
	if len(srcs) == 1 {
		_, _, ok := wherePlanSingleTableIndexOrder(p, c, srcs, stmt)
		return ok
	}
	_, _, _, ok := wherePlanMultiTableOrder(p, c, srcs, stmt)
	return ok
}

// wherePlanSingleTableIndexOrder is markWherePlanIndexEligibility's body,
// factored out so wherePlanIndexOrderDecided above asks EXACTLY the question the
// codegen answers. It never mutates srcs. ok == true means the port decided the
// scan; a nil key with ok == true means it decided on the rowid full scan, which
// is the order this engine already produces. nRow is wherePlanResult.nRow for
// the winning plan -- see that field's own doc comment -- meaningless when ok
// is false.
func wherePlanSingleTableIndexOrder(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) (key *autoIndexKey, nRow logEst, ok bool) {
	if p == nil || c == nil || stmt == nil || len(srcs) != 1 {
		return nil, 0, false
	}
	if c.rowOuter != nil { // see wherePlanMultiTableSources' identical guard
		return nil, 0, false
	}
	if len(stmt.From) != 1 {
		return nil, 0, false
	}
	s := &srcs[0]
	if s.derived != nil || s.vtabItem != nil || s.cteItem != nil ||
		s.catalogScope != scopeAny ||
		s.left || s.rightOuter || s.tbl == nil || s.tbl.withoutRowid ||
		(s.dbIdx != 0 && !p.dbIdxIsTemp(s.dbIdx)) {
		return nil, 0, false
	}
	// See wherePlanMultiTableSources' matching check: colUsed's 64-bit fold is
	// still a real limit at 63+ columns, but merely DECLARING a generated
	// column no longer is -- colUsedBitsFor floods only on an actual
	// reference (resolve.c:176-198), which wherePlanColUsed below already
	// applies correctly.
	if len(s.tbl.cols) >= 63 {
		return nil, 0, false
	}
	if len(stmt.Compound) > 0 {
		return nil, 0, false
	}
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		if wherePlanHasRangeRewrite(cj) {
			return nil, 0, false
		}
	}

	scopes := tableScopesOf(srcs)
	wherePlanBindOuter(c, scopes)
	jts := []joinedTable{{tbl: s.tbl}}
	where := stmt.Where
	if !c.onePassPlan {
		// select.c tag-select-0330; an UPDATE or DELETE never reaches it.
		pw, _, pok := wherePlanPropagateConstants(jts, scopes, where)
		if !pok {
			return nil, 0, false
		}
		where = pw
	}
	terms, ok := wherePlanTermsFrom(jts, scopes, where)
	if !ok {
		return nil, 0, false
	}
	colUsed, colUsedOK := wherePlanColUsed(p, c, jts, scopes, srcs, stmt)
	if !colUsedOK {
		return nil, 0, false
	}
	// whereLoopAddBtree chains a table's real indexes onto its fake sPk only
	// when "pSrc->fg.notIndexed==0", so NOT INDEXED leaves the rowid scan as the
	// only candidate -- which is still a CHOICE once there is an ORDER BY, since
	// wherePathSatisfiesOrderBy can decide to walk it backwards.
	idxs, szTabRow, ok := wherePlanPartialIndexList(p, s.tbl, s.scope.tableName,
		stmt.From[0].IndexedBy, stmt.From[0].NotIndexed, stmt)
	if !ok || len(idxs) == 0 {
		return nil, 0, false
	}
	nRowLogEst, szTabRow := wherePlanItemStats(p, s.tbl, idxs, szTabRow)
	sortCtl, ok := wherePlanSortCtlFor(jts, scopes, stmt)
	if !ok {
		return nil, 0, false
	}
	// PRAGMA reverse_unordered_selects: sqlite3WhereBegin's last act
	//
	//	if( pWInfo->pOrderBy==0 && (db->flags & SQLITE_ReverseOrder)!=0 ){
	//	  whereReverseScanOrder(pWInfo);
	//	}
	//
	// (where.c:7126) flips every item's direction after the solver.
	// wherePlanSingleIndexKey applies it once pOrderBy is settled.
	reverseOrder := p.ReverseUnorderedSelects()
	// Built once, shared by the "sPk only" fast path below (which still needs
	// an authoritative row estimate even though it skips the ORDER-deciding
	// call to wherePlanSingleIndexKey) and the general case.
	in := wherePlanInput{
		items: []whereItem{{
			cols: s.tbl.cols, tabName: r33sFoldIdent(s.tbl.name),
			colUsed: colUsed[0], colUsedOK: true,
			ipkIndex: s.tbl.ipkIndex, szTabRow: szTabRow, idxs: idxs,
			nRowLogEst: nRowLogEst, nRowLogEstSet: true,
			indexedBy: stmt.From[0].IndexedBy != "", notIndexed: stmt.From[0].NotIndexed,
			onePass: c.onePassPlan,
		}},
		terms: terms, sort: sortCtl, noAutoIndex: !p.AutomaticIndex(),
		winner: c.planWinner,
	}
	if c.outer != nil && !c.nQueryLoopKnown {
		// A NESTED compile whose enclosing context could not prove its
		// nQueryLoop (compiler.nQueryLoopKnown) -- an aggregate's HAVING or
		// select list, or anything under a scan the port did not plan. C's
		// value there reaches the solver ONLY as the seed path's row count,
		// MIN(nQueryLoop, 48) (where.c:5921), so the order is decided if it is
		// the same for every seed that can be: see wherePlanSeedInvariant. The
		// row estimate is not, and the callers leave planNRowOK clear.
		return wherePlanSeedInvariant(in, func(in wherePlanInput) (*autoIndexKey, logEst, bool) {
			return wherePlanSingleTableSolve(in, stmt, idxs, terms, sortCtl, reverseOrder)
		})
	}
	if c.outer != nil {
		// A true top level has c.outer == nil and in.nQueryLoop stays its zero
		// value, reproducing pre-existing behavior exactly.
		in.nQueryLoop = c.nQueryLoop
	}
	return wherePlanSingleTableSolve(in, stmt, idxs, terms, sortCtl, reverseOrder)
}

// wherePlanSeedInvariant runs solve for every seed-path row count C could plan
// with (MIN(nQueryLoop, 48), down to where costs saturate below -600) and returns
// the key only if every run agrees, including winner flags and the absence of a
// multi-OR winner. nRow is 0. ponytail: 649 solves over one cached loop set, about
// 0.3ms per scan, once per prepared statement; bound the range by the loops' own
// costs if that shows up.
func wherePlanSeedInvariant(in wherePlanInput, solve func(wherePlanInput) (*autoIndexKey, logEst, bool)) (*autoIndexKey, logEst, bool) {
	var first *autoIndexKey
	var firstWin wherePlanWinner
	winner := in.winner
	in.loops = &whereLoopCache{}
	for seed := logEst(48); seed >= -600; seed-- {
		var w wherePlanWinner
		in.nQueryLoop, in.winner = seed, &w
		k, _, ok := solve(in)
		if !ok || k != nil && k.multiOr != nil {
			return nil, 0, false
		}
		if seed == 48 {
			first, firstWin = k, w
			continue
		}
		if w != firstWin || !sameAutoIndexKey(k, first) {
			return nil, 0, false
		}
	}
	if winner != nil {
		*winner = firstWin
	}
	return first, 0, true
}

// wherePlanSeedInvariantOrder is wherePlanSeedInvariant for a join: every
// seed must give the same nesting order and the same key at every level.
func wherePlanSeedInvariantOrder(in wherePlanInput) ([]int, []*autoIndexKey, bool) {
	var order []int
	var keys []*autoIndexKey
	in.loops = &whereLoopCache{}
	for seed := logEst(48); seed >= -600; seed-- {
		in.nQueryLoop = seed
		o, k, _, ok := wherePlanOrder(in)
		if !ok {
			return nil, nil, false
		}
		if seed == 48 {
			order, keys = o, k
			continue
		}
		if !slices.Equal(o, order) || len(k) != len(keys) {
			return nil, nil, false
		}
		for i := range k {
			if !sameAutoIndexKey(k[i], keys[i]) {
				return nil, nil, false
			}
		}
	}
	return order, keys, true
}

// sameAutoIndexKey reports two keys that order rows identically and answer the
// GROUP BY codegen's two questions identically.
func sameAutoIndexKey(a, b *autoIndexKey) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(a.cols, b.cols) && slices.Equal(a.colls, b.colls) && slices.Equal(a.desc, b.desc) &&
		a.multiOr == nil && b.multiOr == nil && a.level == b.level && a.root == b.root &&
		a.nEq == b.nEq && a.groupsSorted == b.groupsSorted
}

// wherePlanSingleTableSolve is wherePlanSingleTableIndexOrder's solver half, for
// an input whose nQueryLoop is settled.
func wherePlanSingleTableSolve(in wherePlanInput, stmt *SelectStmt, idxs []whereIndexInfo, terms []whereIdxTerm, sortCtl *whereSortCtl, reverseOrder bool) (*autoIndexKey, logEst, bool) {
	if stmt.From[0].IndexedBy == "" && len(idxs) < 2 && !wherePlanTermsHaveOr(terms) &&
		!(reverseOrder && (sortCtl.nOrderBy() == 0 || sortCtl.distinctCandidate)) {
		// Only the rowid scan to choose: its order is this engine's own. Except an OR
		// term: whereLoopAddOr can build a WHERE_MULTI_OR loop over sub-scans even with no
		// index, returning their concatenation ("WHERE rowid>2 OR rowid<2" returns the
		// rowid>2 run first).
		//
		// A reverse rowid scan requested by ORDER BY is not followed: an ORDER BY on the
		// rowid is total, so the direction is unobservable, and a key would force
		// materializing every "ORDER BY rowid DESC" scan. Under reverse_unordered_selects
		// with no settled pOrderBy the direction is observable (with LIMIT it picks
		// rows), so that case goes to the solver, as does a distinctCandidate (unknown
		// until plan() runs isDistinctRedundant). The solver is run here anyway to read
		// nRowOut, as C computes it even for one candidate.
		fb := &wherePlanIdxBuild{
			items: in.items, terms: in.terms,
			sort: in.sort, noAutoIndex: in.noAutoIndex, nQueryLoop: in.nQueryLoop,
			cache: in.loops,
		}
		res, fok := fb.plan()
		if !fok {
			return nil, 0, false
		}
		if in.winner != nil && len(res.loops) == 1 {
			w := &fb.loops[res.loops[0]]
			in.winner.auto, in.winner.oneRow = w.autoIdx, w.oneRow
		}
		if len(res.loops) == 1 && fb.loops[res.loops[0]].autoIdx {
			// Not the rowid scan after all: see wherePlanSingleIndexKey's
			// automatic-index arm, which this one mirrors.
			k := wherePlanAutoIndexKey(in, []int{0}, 0)
			if k == nil {
				return nil, 0, false
			}
			if reverseOrder && in.sort.nOrderBy() == 0 {
				k = whereReversedScanKey(k)
			}
			return k, res.nRow, true
		}
		return nil, res.nRow, true
	}
	return wherePlanSingleIndexKey(in, reverseOrder)
}

// wherePlanSortCtlFor reproduces select.c's choice of which list it hands
// sqlite3WhereBegin as pOrderBy, and with which wctrlFlags. ok == false declines.
//
//   - tag-select-0700 (non-aggregate): pOrderBy is the ORDER BY, plus
//     WHERE_WANT_DISTINCT for DISTINCT and WHERE_USE_LIMIT for an integer LIMIT.
//     With DISTINCT and no ORDER BY, sqlite3WhereBegin uses the result set
//     (WHERE_DISTINCTBY) unless isDistinctRedundant, so that is carried as
//     distinctCandidate.
//   - GROUP BY: pOrderBy is the GROUP BY with WHERE_GROUPBY, plus
//     WHERE_SORTBYGROUP when ORDER BY matches it term for term (which also copies
//     ORDER BY's DESC onto it).
//   - tag-select-0822 (whole-table aggregate): the lone min()/max() argument
//     (minMaxQuery), else nothing.
//
// Declined: window functions; a DISTINCT aggregate argument (WHERE_AGG_DISTINCT
// and pDistinct); a parameter LIMIT/OFFSET; the DISTINCT-to-GROUP-BY rewrite
// (select.c:8180); an ORDER BY / GROUP BY term not resolvable as resolve.c does.
func wherePlanSortCtlFor(jts []joinedTable, scopes []tableScope, stmt *SelectStmt) (*whereSortCtl, bool) {
	if stmt.LimitParam != nil || stmt.OffsetParam != nil {
		return nil, false
	}
	if selectHasWindow(stmt) || orderByHasWindow(stmt) {
		return nil, false
	}
	outExprs, outAlias, ok := wherePlanSelectListExprs(stmt, jts, scopes)
	if !ok {
		return nil, false
	}
	ctl := &whereSortCtl{nEList: len(outExprs)}

	// select.c's own "isAgg = (p->selFlags & SF_Aggregate)!=0", which the
	// resolver sets for an aggregate call ANYWHERE the NameContext reaches --
	// the select list, HAVING and ORDER BY alike -- not just the select list.
	// "SELECT a FROM t ORDER BY count(*)" is an aggregate query with no GROUP BY
	// and takes tag-select-0822's arm, not tag-select-0700's.
	isAgg := len(stmt.GroupBy) > 0 || containsAggregate(stmt.Having)
	for _, sc := range stmt.Columns {
		if !sc.Star && containsAggregate(sc.Expr) {
			isAgg = true
		}
	}
	for _, ot := range stmt.OrderBy {
		if containsAggregate(ot.Expr) {
			isAgg = true
		}
	}
	if isAgg && wherePlanHasDistinctAgg(stmt) {
		// A DISTINCT aggregate argument adds WHERE_WANT_DISTINCT|WHERE_AGG_DISTINCT and a
		// pDistinct (select.c:8492, 8864). With one FROM item and a real GROUP BY it does
		// not affect path selection: pOrderBy is the GROUP BY (select.c:8531), so the
		// pOrderBy==0 substitution (where.c:7048) cannot fire; the solver reads only
		// pOrderBy (where.c:5971); and WHERE_AGG_DISTINCT's effect needs two or more
		// levels (where.c:7172). So that case proceeds (distinctagg.test stmt17). The
		// whole-table arm and joins stay declined.
		if len(stmt.GroupBy) == 0 || len(jts) != 1 {
			return nil, false
		}
	}

	term := func(e Expr, desc, bigNull bool) (whereSortTerm, bool) {
		return wherePlanSortTermOf(jts, scopes, e, outExprs, outAlias, desc, bigNull)
	}

	switch {
	case isAgg && len(stmt.GroupBy) > 0:
		ctl.groupBy = true
		// wantDistinct is select.c:8492's distFlag; its remaining effect is
		// whereSortingCost's -10 LogEst (where.c:5579), which changes which loop wins, so
		// it is set on the exact precondition (wherePlanSingleDistinctAgg).
		ctl.wantDistinct = wherePlanSingleDistinctAgg(stmt)
		// sqlite3CopySortOrder copies the ORDER BY's DESC bits onto the GROUP BY
		// terms whenever the two lists are the same LENGTH -- a side effect that
		// happens even when the comparison afterwards fails. BIGNULL is masked
		// off, so a GROUP BY term never carries it.
		sameLen := len(stmt.OrderBy) == len(stmt.GroupBy)
		for i, ge := range stmt.GroupBy {
			desc := sameLen && stmt.OrderBy[i].Desc
			t, ok := term(ge, desc, false)
			if !ok {
				return nil, false
			}
			ctl.ob = append(ctl.ob, t)
		}
		// orderByGrp: the lists must also compare equal term for term, sortFlags
		// included -- so an ORDER BY term carrying NULLS FIRST/LAST against the
		// direction (KEYINFO_ORDER_BIGNULL, which CopySortOrder does not copy)
		// defeats it.
		if sameLen && len(stmt.OrderBy) > 0 {
			ctl.sortByGroup = true
			for i := range stmt.OrderBy {
				// Both sides are compared AFTER resolve.c has replaced an
				// ordinal or an alias by the select-list expression it names,
				// which is what makes "GROUP BY a ORDER BY 1" the identical
				// pair sqlite3ExprListCompare sees. exprEqual (sql_group.go) is
				// this package's own sqlite3ExprCompare: structurally identical
				// trees, identifiers case-insensitive, literals exactly equal.
				g, gok := wherePlanResolveSortExpr(jts, scopes, stmt.GroupBy[i], outExprs, outAlias)
				o, ook := wherePlanResolveSortExpr(jts, scopes, stmt.OrderBy[i].Expr, outExprs, outAlias)
				if !gok || !ook || wherePlanSortBigNull(stmt.OrderBy[i]) || !exprEqual(g, o) {
					ctl.sortByGroup = false
					break
				}
			}
		}

	case isAgg:
		// A whole-table aggregate. minMaxQuery fires only for a LONE aggregate
		// call that is min(X) or max(X) with one argument and no HAVING; every
		// other whole-table aggregate hands the planner no ORDER BY at all.
		mm, name, arg := wherePlanMinMaxQuery(stmt)
		if mm {
			// pMinMaxOrderBy is a DUPLICATE of an already-resolved argument
			// list, never an ORDER BY the parser handed to resolve.c -- so it
			// gets NO ordinal or alias substitution ("SELECT min(1) FROM t" does
			// not order by result column 1).
			t, ok := wherePlanSortTermFromResolved(jts, scopes, arg, false, false)
			if !ok {
				return nil, false
			}
			// WHERE_ORDERBY_MAX sorts DESC; WHERE_ORDERBY_MIN sorts ASC and
			// carries KEYINFO_ORDER_BIGNULL when the argument can be NULL.
			isMax := equalFoldName(name, "max")
			t.desc = isMax
			t.bigNull = !isMax && wherePlanExprCanBeNull(jts, scopes, arg)
			ctl.ob = append(ctl.ob, t)
			ctl.minMax = true
		}

	default:
		if stmt.Distinct && len(stmt.OrderBy) == len(outExprs) && len(stmt.OrderBy) > 0 {
			// The DISTINCT-to-GROUP-BY rewrite (select.c ~8180) needs the lists to match in
			// length and term by term; this declines on length alone, so "SELECT DISTINCT a
			// FROM t ORDER BY b" (not rewritten by C) is also declined. Narrowing it widens
			// what the planner decides and needs its own tests.
			return nil, false
		}
		ctl.wantDistinct = stmt.Distinct
		if stmt.Limit != nil && *stmt.Limit > 0 {
			// computeLimitRegisters (select.c): an integer LIMIT n sets
			// SF_FixedLimit and drops nSelectRow -- which sqlite3WhereBegin then
			// stores as pWInfo->iLimit -- to sqlite3LogEst(n). n == 0 short-
			// circuits the whole query instead, and a negative one does nothing.
			ctl.useLimit = true
			ctl.iLimit = logEstFromInt(uint64(*stmt.Limit))
		}
		for _, ot := range stmt.OrderBy {
			t, ok := term(ot.Expr, ot.Desc, wherePlanSortBigNull(ot))
			if !ok {
				return nil, false
			}
			ctl.ob = append(ctl.ob, t)
		}
		if ctl.wantDistinct && len(ctl.ob) == 0 {
			// "Try to ORDER BY the result set to make distinct processing
			// easier" -- pOrderBy becomes pResultSet, whose terms carry no sort
			// flags at all.
			for _, e := range outExprs {
				t, ok := term(e, false, false)
				if !ok {
					return nil, false
				}
				ctl.ob = append(ctl.ob, t)
			}
			ctl.distinctCandidate = true
		}
	}
	return ctl, true
}

// wherePlanSortBigNull is ExprList_item.fg.sortFlags & KEYINFO_ORDER_BIGNULL:
// sqlite3ExprListSetSortOrder (expr.c) sets it exactly when an EXPLICIT NULLS
// clause disagrees with the term's own ASC/DESC direction.
func wherePlanSortBigNull(ot OrderTerm) bool {
	return ot.Nulls != NullsDefault && ot.NullsFirst() == ot.Desc
}

// wherePlanSortTermOf reduces one ORDER BY / GROUP BY / result-set expression to
// a whereSortTerm, resolving it the way resolve.c does first: an ORDINAL and an
// explicit result-column ALIAS are both replaced by the select-list expression
// itself (sqlite3ResolveOrderGroupBy -> resolveAlias). ok == false for a name
// this cannot resolve at all, which would otherwise be silently mistaken for a
// non-column term and give the planner a different answer than SQLite's.
func wherePlanSortTermOf(jts []joinedTable, scopes []tableScope, e Expr, outExprs []Expr,
	outAlias []string, desc, bigNull bool) (whereSortTerm, bool) {
	t := whereSortTerm{tab: -1, col: xnNoColumn, coll: "BINARY", desc: desc, bigNull: bigNull}
	if e == nil {
		return t, false
	}
	resolved, ok := wherePlanResolveSortExpr(jts, scopes, e, outExprs, outAlias)
	if !ok {
		return t, false
	}
	return wherePlanSortTermFromResolved(jts, scopes, resolved, desc, bigNull)
}

// wherePlanSortTermFromResolved is wherePlanSortTermOf's tail, on an expression
// resolve.c has already finished with.
func wherePlanSortTermFromResolved(jts []joinedTable, scopes []tableScope, resolved Expr,
	desc, bigNull bool) (whereSortTerm, bool) {
	t := whereSortTerm{tab: -1, col: xnNoColumn, coll: "BINARY", desc: desc, bigNull: bigNull}
	if resolved == nil {
		return t, false
	}
	refs := map[int]bool{}
	collectTableRefs(resolved, scopes, refs)
	for i := range refs {
		if i >= 63 {
			return t, false
		}
		t.uses |= whereMask(1) << uint(i)
	}
	t.isConst = t.uses == 0 && wherePlanExprIsConstant(resolved)

	if t.uses == 0 && !t.isConst {
		// A term that names no table and that this cannot PROVE constant --
		// a function call, chiefly. sqlite3ExprIsConstant may well accept it
		// (EP_ConstFunc), in which case the last block of
		// wherePathSatisfiesOrderBy marks it satisfied and this would not:
		// a LOWER isOrdered, a different winning loop, a different row order.
		// Decline rather than answer with the smaller number.
		return t, false
	}

	bare := whereSkipCollate(resolved)
	if ce, ok := bare.(ColumnExpr); ok {
		tab, col, found := wherePlanColumnRef(jts, scopes, ce)
		if !found {
			return t, false
		}
		t.tab, t.col = tab, col
	}
	// sqlite3ExprNNCollSeq: an explicit COLLATE on the term (the ORIGINAL, not
	// the stripped one), else the column's declared collation, else BINARY.
	if n, ok := exprCollation(resolved); ok {
		t.coll = effectiveCollation(n)
	} else if n, ok := wherePlanDeclaredCollation(jts, scopes, bare); ok {
		t.coll = effectiveCollation(n)
	}
	return t, true
}

// wherePlanResolveSortExpr is sqlite3ResolveOrderGroupBy (resolve.c): an ORDER
// BY / GROUP BY term that is an ORDINAL, or an explicit result-column ALIAS, is
// REPLACED by the select-list expression itself (resolveAlias) before the
// planner -- or orderByGrp's own sqlite3ExprListCompare -- ever sees it. ok ==
// false for a name this cannot resolve at all, which would otherwise be
// silently mistaken for a non-column term.
func wherePlanResolveSortExpr(jts []joinedTable, scopes []tableScope, e Expr,
	outExprs []Expr, outAlias []string) (Expr, bool) {
	if e == nil {
		return nil, false
	}
	if n, ok := orderByOrdinal(whereSkipCollate(e)); ok {
		if n < 1 || int(n) > len(outExprs) {
			return nil, false
		}
		return outExprs[n-1], true
	}
	if ce, ok := whereSkipCollate(e).(ColumnExpr); ok && ce.Qualifier == "" {
		// resolveAsName is tried FIRST and matches only an EXPLICIT result-column
		// alias; a bare name matching neither an alias nor a column of this table
		// is an outer reference or an error, and either way not this table's
		// ordering.
		if j := wherePlanAliasIndex(ce.Name, outAlias); j >= 0 {
			return outExprs[j], true
		}
		if _, _, found := wherePlanColumnRef(jts, scopes, ce); !found {
			return nil, false
		}
	}
	return e, true
}

// wherePlanSelectListExprs expands "*"/"t.*" into one expression per column, as
// sqlite3SelectExpand leaves p->pEList: its length is what whereSortingCost
// prices, its entries are what ordinals resolve to, its aliases what
// resolveAsName matches. Each expanded reference is qualified by its FROM item.
func wherePlanSelectListExprs(stmt *SelectStmt, jts []joinedTable, scopes []tableScope) ([]Expr, []string, bool) {
	var exprs []Expr
	var alias []string
	for _, sc := range stmt.Columns {
		if !sc.Star {
			exprs = append(exprs, sc.Expr)
			if sc.HasAlias {
				alias = append(alias, sc.Alias)
			} else {
				alias = append(alias, "")
			}
			continue
		}
		for j := range jts {
			if jts[j].tbl == nil || j >= len(scopes) {
				return nil, nil, false
			}
			if sc.StarQualifier != "" && !equalFoldName(scopes[j].name, sc.StarQualifier) || scopes[j].fromExists {
				continue // "*" was expanded before existsToJoin added that item
			}
			for i := range jts[j].tbl.cols {
				if jts[j].tbl.cols[i].Hidden {
					continue
				}
				// "*" expands to a plain reference per column, the INTEGER
				// PRIMARY KEY included -- expandSelectList's own set.
				exprs = append(exprs, ColumnExpr{
					Qualifier: scopes[j].name, Name: jts[j].tbl.cols[i].Name,
				})
				alias = append(alias, "")
			}
		}
	}
	if len(exprs) == 0 {
		return nil, nil, false
	}
	return exprs, alias, true
}

// wherePlanAliasIndex is resolveAsName (resolve.c): the position of the result
// column whose EXPLICIT alias is zName, or -1. Only an explicit "AS x" counts --
// ENAME_NAME is set by sqlite3ExprListSetName, which the grammar calls only for
// a written alias.
func wherePlanAliasIndex(name string, outAlias []string) int {
	if name == "" {
		return -1
	}
	for i, a := range outAlias {
		if a != "" && equalFoldName(a, name) {
			return i
		}
	}
	return -1
}

// wherePlanHasDistinctAgg reports whether any aggregate call in the select list
// or HAVING is a DISTINCT one -- select.c's pDistinct, which adds
// WHERE_WANT_DISTINCT|WHERE_AGG_DISTINCT to wctrlFlags and a second result set
// this port does not model.
func wherePlanHasDistinctAgg(stmt *SelectStmt) bool {
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if found || e == nil {
			return
		}
		if f, ok := e.(FuncExpr); ok && f.Distinct {
			found = true
			return
		}
		wherePlanWalkChildren(e, walk)
	}
	for _, sc := range stmt.Columns {
		if !sc.Star {
			walk(sc.Expr)
		}
	}
	walk(stmt.Having)
	return found
}

// wherePlanSingleDistinctAgg reports C's exact precondition for the GROUP BY
// arm's distFlag (select.c:8483-8492): one aggregate call site after
// deduplication, and it is DISTINCT. It feeds wantDistinct, so it must match C
// exactly (wherePlanHasDistinctAgg is the deliberately wider decline test). The
// walk covers select list, HAVING and ORDER BY and dedups identical calls as
// analyzeAggregate does (expr.c:7484).
func wherePlanSingleDistinctAgg(stmt *SelectStmt) bool {
	var calls []FuncExpr
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil {
			return
		}
		if f, ok := e.(FuncExpr); ok && isAggregateCall(f) {
			calls = append(calls, f)
		}
		wherePlanWalkChildren(e, walk)
	}
	for _, sc := range stmt.Columns {
		if !sc.Star {
			walk(sc.Expr)
		}
	}
	walk(stmt.Having)
	for _, ot := range stmt.OrderBy {
		walk(ot.Expr)
	}
	calls = dedupMinMaxCalls(calls)
	return len(calls) == 1 && calls[0].Distinct
}

// wherePlanMinMaxQuery ports minMaxQuery (select.c): a whole-table aggregate
// whose ONE aggregate call is min(X) or max(X) with a single argument, no
// FILTER and no window, hands sqlite3WhereBegin "ORDER BY X" (descending for
// max) as its pOrderBy. Anything else hands it nothing.
//
// The C's precondition is "p->pGroupBy==0 && p->pHaving==0 && pAggInfo->nFunc==1",
// i.e. exactly one aggregate CALL anywhere in the statement.
func wherePlanMinMaxQuery(stmt *SelectStmt) (bool, string, Expr) {
	if len(stmt.GroupBy) > 0 || stmt.Having != nil {
		return false, "", nil
	}
	var calls []FuncExpr
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil {
			return
		}
		if f, ok := e.(FuncExpr); ok && isAggregateCall(f) {
			calls = append(calls, f)
		}
		wherePlanWalkChildren(e, walk)
	}
	for _, sc := range stmt.Columns {
		if !sc.Star {
			walk(sc.Expr)
		}
	}
	// pAggInfo->nFunc counts every aggregate call the resolver reached, ORDER BY
	// included: "SELECT min(a) FROM t ORDER BY count(*)" has nFunc 2 and is not a
	// min/max query.
	for _, ot := range stmt.OrderBy {
		walk(ot.Expr)
	}
	// ...and it counts DISTINCT sites: analyzeAggregate looks each
	// TK_AGG_FUNCTION up in pAggInfo->aFunc[] with sqlite3ExprCompare
	// (expr.c:7484-7488) and reuses the entry it finds, so
	// "SELECT max(x), typeof(max(x))" is nFunc 1 and IS a min/max query.
	// Without this, that shape lost the seek and answered from the rowid scan --
	// cgo 1.0/real against this engine's 1/integer over a byte-distinct tie.
	calls = dedupMinMaxCalls(calls)
	if len(calls) != 1 {
		return false, "", nil
	}
	f := calls[0]
	if len(f.Args) != 1 || f.Star || f.Over != nil || f.Filter != nil || len(f.OrderBy) > 0 {
		return false, "", nil
	}
	if !equalFoldName(f.Name, "min") && !equalFoldName(f.Name, "max") {
		return false, "", nil
	}
	return true, f.Name, f.Args[0]
}

// wherePlanExprCanBeNull is sqlite3ExprCanBeNull (expr.c) over the shapes a
// min()/max() argument can take here: a NOT NULL column and a non-NULL literal
// cannot, everything else conservatively can.
func wherePlanExprCanBeNull(jts []joinedTable, scopes []tableScope, e Expr) bool {
	switch x := e.(type) {
	case ColumnExpr:
		tab, col, ok := wherePlanColumnRef(jts, scopes, e)
		if !ok || tab != 0 {
			return true
		}
		if col < 0 {
			return false // the rowid is never NULL
		}
		return col >= len(jts[0].tbl.cols) || !jts[0].tbl.cols[col].NotNull
	case LiteralExpr:
		return x.Val.Typ == Null
	}
	return true
}

// wherePlanExprIsConstant ports sqlite3ExprIsConstant(0, p) (exprNodeIsConstant,
// eCode 1) for ORDER BY terms that reference no table. Bound parameters are
// constant; columns, aggregates, subqueries and RAISE are not; every function
// answers false (C needs EP_ConstFunc), which the caller turns into a decline.
func wherePlanExprIsConstant(e Expr) bool {
	ok := true
	var walk func(Expr)
	walk = func(x Expr) {
		if !ok || x == nil {
			return
		}
		switch x.(type) {
		case LiteralExpr, ParamExpr:
			return
		case ColumnExpr, SubqueryExpr, ExistsExpr, FuncExpr:
			ok = false
			return
		}
		wherePlanWalkChildren(x, walk)
	}
	walk(e)
	return ok
}

// wherePlanWalkChildren visits every direct sub-expression of e. It is
// deliberately TOTAL over the node kinds this package builds; a node it does not
// recognise simply has no children walked, which every caller above treats as
// "cannot classify" through its own default.
func wherePlanWalkChildren(e Expr, visit func(Expr)) {
	switch x := e.(type) {
	case UnaryExpr:
		visit(x.X)
	case BinaryExpr:
		visit(x.L)
		visit(x.R)
	case IsNullExpr:
		visit(x.X)
	case CollateExpr:
		visit(x.X)
	case CastExpr:
		visit(x.X)
	case BetweenExpr:
		visit(x.X)
		visit(x.Lo)
		visit(x.Hi)
	case LikeExpr:
		visit(x.X)
		visit(x.Pattern)
		visit(x.Escape)
	case GlobExpr:
		visit(x.X)
		visit(x.Pattern)
	case InExpr:
		visit(x.X)
		for _, a := range x.List {
			visit(a)
		}
	case CaseExpr:
		visit(x.Base)
		for _, w := range x.Whens {
			visit(w.When)
			visit(w.Then)
		}
		visit(x.Else)
	case FuncExpr:
		for _, a := range x.Args {
			visit(a)
		}
		visit(x.Filter)
		for _, ob := range x.orderByExprs() {
			visit(ob)
		}
	case RowExpr:
		for _, a := range x.Elems {
			visit(a)
		}
	}
}

// wherePlanHasRangeRewrite reports whether an expression anywhere (nested ANDs
// included, as exprAnalyze walks them) contains an IN this port does not
// reproduce: only the empty list remains, which C's parser folds to a constant so
// there is no WhereTerm at all. BETWEEN, the value-list IN, the OR converted to
// an IN, and the subquery IN are all ported (wherePlanInShape,
// wherePlanInRhsRefuse).
func wherePlanHasRangeRewrite(e Expr) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if found || x == nil {
			return
		}
		switch v := x.(type) {
		case InExpr:
			if len(v.List) == 0 && v.Sub == nil {
				found = true
				return
			}
			walk(v.X)
			for _, a := range v.List {
				walk(a)
			}
		case BetweenExpr:
			walk(v.X)
			walk(v.Lo)
			walk(v.Hi)
		case BinaryExpr:
			walk(v.L)
			walk(v.R)
		case UnaryExpr:
			walk(v.X)
		case IsNullExpr:
			walk(v.X)
		case CollateExpr:
			walk(v.X)
		case CastExpr:
			walk(v.X)
		case CaseExpr:
			walk(v.Base)
			for _, w := range v.Whens {
				walk(w.When)
				walk(w.Then)
			}
			walk(v.Else)
		case FuncExpr:
			for _, a := range v.walkArgs() {
				walk(a)
			}
		}
	}
	walk(e)
	return found
}

// wherePlanReduceOuterJoins ports outer join strength reduction (select.c
// tag-select-0220): if WHERE requires a column of a LEFT-joined item to be
// non-NULL, no NULL-extended row survives, so the join becomes inner before
// planning, removing its reorder barrier. The whole rewrite is done, since
// reordering without stopping NULL-extension would be wrong here:
//
//   - jointype &= ~(JT_LEFT|JT_OUTER): srcs[i].left = false, so emitJoinLevel
//     takes its inner branch and the item stops being a barrier;
//   - unsetJoinExpr re-tags its ON terms EP_InnerON: outerON becomes false for
//     them in wherePlanTermsFrom;
//   - its ON is already in WHERE in C: srcs[i].onToWhere = true.
//
// Cascading and ascending, as in C. RIGHT/FULL arms are unreachable (declined).
// Known divergence: C skips this for a compound arm after the first (!p->pPrior),
// which this cannot tell apart; it only affects loop order, never the row set.
func wherePlanReduceOuterJoins(jts []joinedTable, scopes []tableScope, srcs []joinSource, where Expr) {
	for i, r := range wherePlanOuterJoinsReduced(jts, scopes, srcs, where) {
		if !r {
			continue
		}
		srcs[i].left = false
		if srcs[i].on != nil {
			srcs[i].onToWhere = true
		}
	}
}

// wherePlanOuterJoinsReduced is that rewrite's DECISION, with no side effect:
// reduced[i] reports whether item i's LEFT join becomes an INNER one. It is
// separate from the application above because the ported planner has to plan
// UNDER the rewrite (whereItem.outer is an input to whereLoopAddAll's barrier
// accumulator) while the sources must only be rewritten once the plan succeeds
// -- srcs[i].left is read by other compilers too.
func wherePlanOuterJoinsReduced(jts []joinedTable, scopes []tableScope, srcs []joinSource, where Expr) []bool {
	reduced := make([]bool, len(srcs))
	// The C tests p->pWhere, which by this point holds every ON clause ANDed onto
	// the end in FROM order and tagged. impliesNotNullRow PRUNES an EP_OuterON
	// node outright, so an OUTER join's own ON contributes nothing, while an
	// INNER join's (EP_InnerON, and isRJ is 0 here) counts in full. So the terms
	// to test are the WHERE's own conjuncts plus the ON conjuncts of every item
	// INNER at that point in the loop. Their order is irrelevant: the test is an
	// OR over independent conjuncts. A desugared row value is one conjunct, as
	// in C (whereSplitTopAnd).
	terms := whereSplitTopAnd(where)
	for i := range srcs {
		if srcs[i].on != nil && !srcs[i].left && !srcs[i].rightOuter {
			terms = append(terms, whereSplitTopAnd(srcs[i].on)...)
		}
	}
	w := wherePlanNonNullWalk{jts: jts, scopes: scopes}
	for i := range srcs {
		if !srcs[i].left || srcs[i].rightOuter {
			continue
		}
		w.iTab = i
		implied := false
		for _, t := range terms {
			if wherePlanImpliesNonNullRow(t, w) {
				implied = true
				break
			}
		}
		if !implied {
			continue
		}
		reduced[i] = true
		if srcs[i].on != nil {
			terms = append(terms, whereSplitTopAnd(srcs[i].on)...)
		}
	}
	return reduced
}

// wherePlanImpliesNonNullRow ports sqlite3ExprImpliesNonNullRow (expr.c) for one
// top-level WHERE conjunct: e can be true only if some column of FROM item w.iTab
// is non-NULL. At the top, "A AND B" qualifies if either side does; inside the
// walk AND needs both (it may be under NOT or OR). "X IS NOT NULL" at the top
// walks X. A false negative only misses a reduction; a false positive would drop
// NULL-extended rows, so unknown nodes prune.
func wherePlanImpliesNonNullRow(e Expr, w wherePlanNonNullWalk) bool {
	e = wherePlanSkipCollateAndLikely(e)
	if e == nil {
		return false
	}
	if x, ok := e.(IsNullExpr); ok && x.Not { // TK_NOTNULL
		return w.walk(x.X)
	}
	for {
		b, ok := e.(BinaryExpr)
		if !ok || !equalFoldName(b.Op, "AND") || b.row != nil {
			break
		}
		if wherePlanImpliesNonNullRow(b.L, w) {
			return true
		}
		e = b.R
	}
	return w.walk(e)
}

// wherePlanSkipCollateAndLikely ports sqlite3ExprSkipCollateAndLikely (expr.c):
// EP_Skip is TK_COLLATE and EP_Unlikely is a likelihood()/likely()/unlikely()
// call, whose first argument replaces it.
func wherePlanSkipCollateAndLikely(e Expr) Expr {
	for {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
		case FuncExpr:
			switch r33sFoldIdent(x.Name) {
			case "likelihood", "likely", "unlikely":
				if len(x.Args) == 0 {
					return e
				}
				e = x.Args[0]
			default:
				return e
			}
		default:
			return e
		}
	}
}

// wherePlanNonNullWalk is impliesNotNullRow's Walker (expr.c), driven by walk
// below in place of sqlite3WalkExpr. The EP_OuterON prune is reproduced by the
// CALLER, which simply never passes an OUTER join's ON conjuncts in; the
// EP_InnerON prune only fires for isRJ, which the gate makes unreachable.
type wherePlanNonNullWalk struct {
	jts    []joinedTable
	scopes []tableScope
	iTab   int
}

// walk reports whether the walk sets eCode -- i.e. whether e forces some column
// of w.iTab to be non-NULL. "Prune" in the C becomes a plain false here (a
// pruned subtree can never set eCode); "Continue" becomes a walk of every child.
func (w wherePlanNonNullWalk) walk(e Expr) bool {
	// bothImplyNotNullRow: eCode is set only if BOTH arms separately imply it,
	// because "NOT (x AND y)" and "x OR y" can each be true on one arm alone.
	both := func(a, b Expr) bool { return w.walk(a) && w.walk(b) }
	switch x := e.(type) {
	case ColumnExpr:
		tab, _, ok := wherePlanColumnRef(w.jts, w.scopes, x)
		return ok && tab == w.iTab

	case IsNullExpr:
		return false // TK_ISNULL / TK_NOTNULL
	case FuncExpr, CaseExpr, RowExpr, LikeExpr, GlobExpr, MatchExpr:
		// TK_FUNCTION (LIKE/GLOB/MATCH are function calls in the C), TK_CASE and
		// TK_VECTOR. TK_TRUTH ("X IS TRUE") reaches here too: this package
		// desugars it into a CaseExpr, optionally under a NOT whose own arm
		// continues straight into that CaseExpr, so either spelling prunes.
		return false
	case SubqueryExpr, ExistsExpr:
		// sqlite3WalkSelect returns immediately when xSelectCallback is 0, and
		// these nodes have no other children, so the C never descends either.
		return false

	case BinaryExpr:
		if x.row != nil {
			// A desugared row value implies nothing, whatever its tree does:
			// every operand of it is a TK_VECTOR, which prunes (expr.c:6917),
			// and a vector IN's right-hand side is a SELECT, "(a,b) IN (VALUES
			// ...)", whose left the TK_IN arm never walks (expr.c:6957).
			return false
		}
		switch strings.ToUpper(x.Op) {
		case "IS", "IS NOT":
			return false // TK_IS / TK_ISNOT
		case "AND", "OR":
			return both(x.L, x.R)
		}
		// Every remaining operator -- the comparisons TK_EQ..TK_GE and the
		// arithmetic/concat/bitwise ones -- takes the C's default WRC_Continue.
		// (The virtual-table exception inside the comparison arm is unreachable:
		// the gate declines a virtual table.)
		return w.walk(x.L) || w.walk(x.R)

	case InExpr:
		// "x NOT IN ()" can be true of a NULL x, so only a NON-EMPTY value LIST
		// lets the walk descend; a subquery RHS (ExprUseXList false) never does.
		if x.Sub == nil && len(x.List) > 0 {
			return w.walk(x.X)
		}
		return false
	case BetweenExpr:
		// Either x implies it, or both bounds do -- the same for BETWEEN and NOT
		// BETWEEN, which the C reaches through a TK_NOT wrapper.
		return w.walk(x.X) || both(x.Lo, x.Hi)

	case UnaryExpr:
		return w.walk(x.X) // TK_NOT / TK_UMINUS / TK_UPLUS / TK_BITNOT: Continue
	case CastExpr:
		return w.walk(x.X)
	case CollateExpr:
		return w.walk(x.X)
	}
	// nil, a literal, a parameter (no children) -- and anything unrecognised,
	// which prunes because a false positive here is a wrong answer.
	return false
}

// wherePlanInertOpts lets a caller that knows more than the AST shows adjust the
// inertness test; used by the window peer-order guard (window_peer_order.go):
//
//   - sortExprs replaces the ORDER BY / GROUP BY / select-list walk standing in
//     for pWInfo->pOrderBy: sqlite3WindowRewrite hands the scan the innermost
//     window's PARTITION BY ++ ORDER BY (window.c:1002). An empty non-nil list
//     means no ordering question ("OVER ()").
//   - exempt declares one index harmless for the caller's question
//     (windowPeerConstantIndexFn).
//
// nil is the plain behavior.
type wherePlanInertOpts struct {
	sortExprs []Expr
	exempt    func(srcIdx int, idx *whereIndexInfo) bool
}

func (o *wherePlanInertOpts) sortOverride() ([]Expr, bool) {
	if o == nil || o.sortExprs == nil {
		return nil, false
	}
	return o.sortExprs, true
}

func (o *wherePlanInertOpts) isExempt(i int, idx *whereIndexInfo) bool {
	return o != nil && o.exempt != nil && o.exempt(i, idx)
}

// wherePlanIndexesInert reports whether every index on these tables yields no
// WhereLoop in whereLoopAddBtree, so SQLite's plan is the same as with no
// indexes. An index can produce a loop three ways, each closed here:
//
//   - whereLoopAddBtreeIndex: it only matches terms on the index's leading
//     column, so no index's leading column may appear anywhere in WHERE or ON
//     (wider than C, which only declines more). Skip-scan needs stat1, which
//     the caller declines.
//   - the full scan via index: needs indexMightHelpWithOrderBy (no key column
//     or rowid in any ordering term, over a superset of pOrderBy) or a
//     covering index narrower than the table (colUsed & colNotIdxed == 0 and
//     szIdxRow < szTabRow).
//   - the automatic index: columnIsGoodIndexCandidate rejects any column that
//     leads another index, which the first rule already excludes.
//
// NOT INDEXED makes a source inert outright (where.c:4054, 4070).
// wherePlanIndexList declines indexes it cannot rebuild exactly. pagerOf gives
// the pager owning srcs[i]'s indexes (never nil). Callers check that every
// source is a plain local rowid table under 63 columns with no generated column
// and that no catalog holds sqlite_stat1/stat4.
func wherePlanIndexesInert(pagerOf func(int) *ReadOnlyPager, c *compiler, jts []joinedTable, scopes []tableScope,
	srcs []joinSource, stmt *SelectStmt, colUsed []uint64, colUsedOK bool,
	opts *wherePlanInertOpts) bool {
	if !colUsedOK {
		// colUsed answers the covering test, and colUsedOK is also what
		// guarantees every node of the WHERE/ON walk below is one
		// wherePlanWalkColumns actually recognises -- an unrecognised node
		// reports an empty ColumnExpr, which would silently hide a constrained
		// leading column.
		return false
	}
	// The index walk below reads srcs[i]'s qualifier off stmt.From[i], which
	// holds only while resolveJoinSources maps FROM items to sources one for
	// one. Its one exception is a parenthesized join GROUP, which collapses a
	// whole span into a single source -- and that source carries a derived
	// program, which the caller's prerequisite walk already refuses. Assert the
	// correspondence here rather than rest a per-source read on that at a
	// distance: a misattributed NOT INDEXED would call a genuinely indexed
	// table inert, which is a WRONG ANSWER, not a gap.
	if len(srcs) != len(stmt.From) {
		return false
	}
	for i := range stmt.From {
		// INDEXED BY makes pProbe that one index and drops sPk entirely, so the
		// named index IS the scan -- never inert. NOT INDEXED is the opposite
		// and is handled per source in the index walk below.
		if stmt.From[i].IndexedBy != "" {
			return false
		}
	}

	// Every (table, column) pair the WHERE clause and the ON clauses mention.
	used := make([]uint64, len(srcs))
	resolved := true
	terms := splitTopLevelAnd(stmt.Where)
	for i := range srcs {
		terms = append(terms, splitTopLevelAnd(srcs[i].on)...)
	}
	for _, t := range terms {
		wherePlanWalkColumns(t, func(ce ColumnExpr) bool {
			tab, col, ok := wherePlanColumnRef(jts, scopes, ce)
			if !ok {
				// A QUALIFIED correlated reference into an enclosing query is not
				// "unresolved" the way an ambiguous or genuinely unknown name is
				// -- see wherePlanOuterRef's own doc comment. It constrains none
				// of THIS statement's own sources (the term is a constant as far
				// as this query's own WHERE-solving is concerned), so it drives
				// no index and is simply skipped rather than declined.
				if wherePlanOuterRef(c, ce) {
					return true
				}
				resolved = false
				return false
			}
			if col >= 0 && col < 63 {
				used[tab] |= uint64(1) << uint(col)
			}
			return true
		})
	}
	if !resolved {
		return false
	}

	// indexMightHelpWithOrderBy's input is pWInfo->pOrderBy: the ORDER BY, GROUP BY
	// or (for DISTINCT) the result set. Use the union of all three, a superset that
	// can only refuse more. The select list is included even with an ORDER BY
	// because an ordinal names a select-list expression.
	//
	// Two more sources: a lone DISTINCT aggregate argument, which select.c:8492 makes
	// pDistinct and sqlite3WhereBegin substitutes for a missing pOrderBy
	// (where.c:7048), so "SELECT group_concat(DISTINCT x) FROM t1" walks an index on
	// x; and a window's PARTITION BY ++ ORDER BY, which sqlite3WindowRewrite makes the
	// inner pOrderBy (window.c:1002), reached because wherePlanWalkColumns descends
	// into a self-contained OVER(...).
	sortCols := make([]uint64, len(srcs))
	sortRowid := make([]bool, len(srcs))
	overrideExprs, haveOverride := opts.sortOverride()
	if haveOverride || len(stmt.OrderBy) > 0 || len(stmt.GroupBy) > 0 || stmt.Distinct ||
		wherePlanHasDistinctAgg(stmt) || selectHasWindow(stmt) || orderByHasWindow(stmt) {
		markSort := func(ce ColumnExpr) bool {
			tab, col, ok := wherePlanColumnRef(jts, scopes, ce)
			if !ok {
				// See the identical carve-out on the WHERE/ON walk above: a
				// qualified correlated reference names no local source, so it is
				// no more a sort-order candidate here than an index-driving term
				// there.
				if wherePlanOuterRef(c, ce) {
					return true
				}
				resolved = false
				return false
			}
			if col < 0 {
				// "if( pExpr->iColumn<0 ) return 1": a rowid term is satisfied
				// by ANY index of a rowid table, every one of which ends in
				// XN_ROWID.
				sortRowid[tab] = true
			} else if col < 63 {
				sortCols[tab] |= uint64(1) << uint(col)
			}
			return true
		}
		if haveOverride {
			// The caller supplied the REAL ordering list; the three walks below
			// would only widen it back into the superset it is replacing.
			for _, e := range overrideExprs {
				wherePlanWalkColumns(e, markSort)
			}
		} else {
			for _, ot := range stmt.OrderBy {
				wherePlanWalkColumns(ot.Expr, markSort)
			}
			for _, e := range stmt.GroupBy {
				wherePlanWalkColumns(e, markSort)
			}
			for _, sc := range stmt.Columns {
				if sc.Star {
					// "*" names every column, so no index of a table it covers
					// can be shown inert this way.
					for i := range srcs {
						if sc.StarQualifier == "" || equalFoldName(scopes[i].name, sc.StarQualifier) {
							sortCols[i] = ^uint64(0)
						}
					}
					continue
				}
				wherePlanWalkColumns(sc.Expr, markSort)
			}
		}
		if !resolved {
			return false
		}
	}

	// exprIndexStmt is stmt only for a single-table statement: an
	// expression/partial index proven irrelevant to stmt.Where/Having/
	// GroupBy/OrderBy/Columns (where_plan_exprindex_skip.go) says nothing
	// about a JOIN's own ON clause, which this function does not inspect for
	// it -- so with more than one source, stay exactly as conservative as
	// before.
	var exprIndexStmt *SelectStmt
	if len(srcs) == 1 {
		exprIndexStmt = stmt
	}
	for i := range srcs {
		// NOT INDEXED is passed through, so this walks the chain whereLoopAddBtree walks:
		// just the fake sPk (where.c:4054), and the automatic-index block is skipped too
		// (where.c:4070), so the source is inert.
		idxs, szTabRow, ok := wherePlanIndexList(pagerOf(i), srcs[i].tbl, srcs[i].scope.tableName, "", stmt.From[i].NotIndexed, exprIndexStmt)
		if !ok {
			return false
		}
		for j := range idxs {
			idx := &idxs[j]
			if idx.ipk {
				continue // the fake sPk, which is the rowid scan the port already builds
			}
			// A caller that can show THIS index harmless for the question it is
			// asking -- not that it builds no loop, but that the loop it builds
			// delivers the rows in an order the caller does not care about --
			// says so here. See windowPeerConstantIndexFn (window_peer_order.go),
			// the only such caller: an index whose every key column is constant
			// inside a window peer group leaves rowid as the only surviving
			// order, which is the order this engine already produces.
			if opts.isExempt(i, idx) {
				continue
			}
			lead := idx.aiColumn[0]
			if lead < 0 || lead >= 63 {
				return false
			}
			if used[i]&(uint64(1)<<uint(lead)) != 0 {
				return false
			}
			if colUsed[i]&idx.colNotIdxed == 0 && idx.szIdxRow < szTabRow {
				return false
			}
			if sortRowid[i] {
				return false
			}
			for k := 0; k < idx.nKeyCol && k < len(idx.aiColumn); k++ {
				if c := idx.aiColumn[k]; c >= 0 && c < 63 && sortCols[i]&(uint64(1)<<uint(c)) != 0 {
					return false
				}
			}
		}
	}
	return true
}

// wherePlanIndexesProvablyInert is wherePlanIndexesInert with its prerequisites
// checked here: every index on every scanned table yields no WhereLoop, so SQLite
// plans as if none existed. For the aggregate anchor guard's access-path half,
// alongside anchorNoIndexInPlay, which rests on the same premise.
func wherePlanIndexesProvablyInert(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt,
	opts *wherePlanInertOpts) bool {
	if p == nil || stmt == nil || len(srcs) == 0 {
		return false
	}
	for i := range srcs {
		s := &srcs[i]
		// A DERIVED table, vtab or CTE hides its own FROM clause: which base
		// tables it reads -- and therefore which indexes are in play -- is not
		// visible from here. Everything else is whereLoopAddBtree's plain rowid
		// base-table arm, which is the only one whose loops this reasons about.
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil ||
			s.catalogScope != scopeAny ||
			s.tbl == nil || s.tbl.name == "" || s.tbl.withoutRowid {
			return false
		}
		// colUsed folds every column at or past bit 63 onto that one bit,
		// which this port does not reproduce -- see wherePlanColUsed. A
		// generated column no longer disqualifies the table outright: a
		// REFERENCE to one still floods every bit (colUsedBitsFor,
		// resolve.c:176-198), and wherePlanColUsed below applies that
		// correctly, so the covering test it feeds stays exact.
		if len(s.tbl.cols) >= 63 {
			return false
		}
	}
	// Index.hasStat1 gates both the skip-scan block and columnIsGoodIndexCandidate's
	// second rule, so any stat table anywhere a source could read from is a decline.
	pagers := make([]*ReadOnlyPager, len(srcs))
	for i := range srcs {
		owner := p.forDB(srcs[i].dbIdx)
		if owner == nil {
			return false
		}
		pagers[i] = owner
		rows, err := owner.Schema()
		if err != nil {
			return false
		}
		for j := range rows {
			if rows[j].Type == "table" && strings.HasPrefix(r33sFoldIdent(rows[j].Name), "sqlite_stat") {
				return false
			}
		}
	}

	scopes := tableScopesOf(srcs)
	wherePlanBindOuter(c, scopes)
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}
	colUsed, colUsedOK := wherePlanColUsed(p, c, jts, scopes, srcs, stmt)
	return wherePlanIndexesInert(func(i int) *ReadOnlyPager { return pagers[i] },
		c, jts, scopes, srcs, stmt, colUsed, colUsedOK, opts)
}

// wherePlanRowidConstrained used to live here: it declined any WHERE or ON
// clause mentioning a ROWID, "the shape whose sPk loops whereLoopAddBtreeIndex
// prices and this port does not build". whereLoopAddBtreeIndex IS built now --
// it prices the fake sPk exactly as it does any other index -- so the decline
// went with it.


// colUsedBitsFor is sqlite3ExprColUsed (resolve.c:176-198): the colUsed bits for
// one reference to column col. An ordinary column gives its own bit; a generated
// column floods every bit of the table (resolve.c:171-174), keyed on the column
// referenced, not on the table having one. Callers guarantee fewer than 63
// columns, so no ALLBITS fallback is needed.
func colUsedBitsFor(cols []columnInfo, col int) uint64 {
	if col >= 0 && col < len(cols) && cols[col].IsGenerated() {
		return uint64(1)<<uint(len(cols)) - 1
	}
	return uint64(1) << uint(col)
}

// wherePlanBoundOuter resolves a name the planned FROM does not supply against
// the statement's context (tableScope.planOuter): an enclosing row, a trigger's
// NEW/OLD, an upsert's "excluded" or conflicting row. To where.c each is a
// constant for the scan, so it plans as one, carrying the column's affinity and
// declared collation into its comparison. ok false: not resolvable there.
func wherePlanBoundOuter(scopes []tableScope, ce ColumnExpr) (aff affinity, hasAff bool, coll string, hasColl bool, ok bool) {
	if len(scopes) == 0 || scopes[0].planOuter == nil {
		return
	}
	// Only a name the FROM clause does not supply at all: one it supplies but
	// the planner declined for its own reasons (ambiguous, a coalesced USING
	// column, a hidden one) is not an outer name, whatever an enclosing scope
	// also calls a column.
	own := scopes
	if ce.Qualifier == "" {
		// See wherePlanColumnRef: existsToJoin's items answer no unqualified name.
		own = nil
		for i := range scopes {
			if !scopes[i].fromExists {
				own = append(own, scopes[i])
			}
		}
	}
	if _, _, _, _, ferr := resolveColumnEx(&evalCtx{tables: own}, ce); ferr == nil ||
		strings.Contains(ferr.Error(), "ambiguous") {
		return
	}
	found, _, col, rowidIdx, err := resolveColumnEx(scopes[0].planOuter, ce)
	if err != nil || found == nil {
		return
	}
	if col == nil {
		if rowidIdx < 0 {
			return
		}
		return affInteger, true, "", false, true // a rowid: INTEGER, and no collation of its own
	}
	return col.Aff, !col.NoAffinity, col.Collation, !col.NoCollation, true
}

// wherePlanBindOuter sets the enclosing context wherePlanBoundOuter resolves
// against onto the planner's own copy of the statement's scopes.
func wherePlanBindOuter(c *compiler, scopes []tableScope) {
	if len(scopes) > 0 && c != nil {
		scopes[0].planOuter = c.enclosingAffCtx()
		if c.pager != nil {
			scopes[0].planCaseSensitiveLike = c.pager.caseSensitiveLike
			scopes[0].planUTF16LE = c.pager.encoding() == UTF16LE
			scopes[0].planPager = c.pager
		}
	}
}

// wherePlanOuterRef reports whether ce is a qualified reference resolving
// against an enclosing compile's scopes (c.outer, as compileColumn walks), i.e.
// a correlated reference. lookupName credits it to the outer item's colUsed
// (resolve.c:827), so it uses no local column. Unqualified names (possibly a
// select-list alias) and outer rowid references answer false.
func wherePlanOuterRef(c *compiler, ce ColumnExpr) bool {
	if c == nil || ce.Qualifier == "" {
		return false
	}
	lname := r33sFoldIdent(ce.Name)
	for oc := c.outer; oc != nil; oc = oc.outer {
		for i := range oc.scopes {
			if !equalFoldName(oc.scopes[i].name, ce.Qualifier) {
				continue
			}
			if _, hit := oc.scopes[i].colIndex[lname]; hit {
				return true
			}
		}
	}
	return false
}

// wherePlanColUsed computes SrcItem.colUsed for every source: bit N set when
// column N of that item is referenced anywhere in the statement. A rowid
// reference, the IPK included, sets no bit, which the automatic-index key is
// sensitive to ("... FROM a,b WHERE a.k=b.k" keys b's index (k, rowid)).
// Desugared USING/NATURAL conditions contribute bits like any reference, as
// sqlite3CreateColumnExpr does (resolve.c:864-889). A correlated name inside a
// subquery credits the enclosing item (r36dSubColUsed). ok is false for anything
// not reproducible: a compound arm, a window function, a schema-qualified
// column, an ambiguous name, or an unknown node.
func wherePlanColUsed(p *ReadOnlyPager, c *compiler, jts []joinedTable, scopes []tableScope, srcs []joinSource, stmt *SelectStmt) ([]uint64, bool) {
	if len(stmt.Compound) > 0 {
		return nil, false
	}
	out := make([]uint64, len(srcs))
	ok := true
	mark := func(ce ColumnExpr) bool {
		if ce.Schema != "" {
			ok = false
			return false
		}
		tab, col, found := wherePlanColumnRef(jts, scopes, ce)
		if !found {
			// Either a name this FROM clause does not supply (a select-list
			// alias or an outer reference, which SQLite resolves in another
			// NameContext and which therefore sets no bit here) or an ambiguous
			// one. wherePlanOuterRef distinguishes a QUALIFIED outer reference
			// from the genuinely unclassifiable rest (see its own doc comment);
			// everything else stays declined.
			if wherePlanOuterRef(c, ce) {
				return true
			}
			if _, _, _, _, bound := wherePlanBoundOuter(scopes, ce); bound {
				return true
			}
			// A bare TRUE/FALSE (or a double-quoted name under the legacy misfeature) naming
			// no column sets no colUsed bit: resolve.c:718-747 rewrites it to a literal
			// before colUsed accumulates. Only when c.outer == nil, since an unqualified
			// name could still resolve to a live outer column named true, which
			// wherePlanOuterRef does not recognize.
			if c.outer == nil && ce.Qualifier == "" && ce.FallbackLiteral != nil {
				return true
			}
			ok = false
			return false
		}
		if col >= 0 {
			out[tab] |= colUsedBitsFor(jts[tab].tbl.cols, col)
		}
		return true
	}
	// "*" and "t.*" expand to one name per column, each resolved through lookupName
	// (expandStar builds plain names, select.c:6216), so every column but the IPK
	// gets its bit, or the flood for a generated one. Columns hidden by USING still
	// get bits through the ON walk below.
	markStar := func(qual string) {
		for i := range srcs {
			if qual != "" && !equalFoldName(scopes[i].name, qual) || scopes[i].fromExists {
				continue // "*" was expanded before existsToJoin added that item
			}
			for c, ci := range srcs[i].tbl.cols {
				if ci.IsRowidAlias || ci.Hidden {
					continue
				}
				out[i] |= colUsedBitsFor(srcs[i].tbl.cols, c)
			}
		}
	}
	var sub func(*SelectStmt) bool
	crossDB := false
	for i := range srcs {
		if srcs[i].dbIdx != 0 {
			// r36dShadowOf resolves a nested FROM name against THIS pager's
			// schema; with a source read out of an ATTACHed file, an unqualified
			// name inside the subquery need not mean the same table. Page numbers
			// are file-local and so are names -- keep the old decline.
			crossDB = true
		}
	}
	if !crossDB {
		cteDefs := r36dWithCTEDefs(nil, stmt.CTEs)
		sub = func(inner *SelectStmt) bool {
			if !r36dSubColUsed(p, inner, nil, cteDefs, 0, mark) {
				ok = false
				return false
			}
			return true
		}
	}
	walk := func(e Expr) {
		if e != nil {
			wherePlanWalkColumnsSub(e, mark, sub)
		}
	}
	for _, sc := range stmt.Columns {
		if sc.Star {
			markStar(sc.StarQualifier)
			continue
		}
		walk(sc.Expr)
	}
	walk(stmt.Where)
	walk(stmt.Having)
	for _, e := range stmt.GroupBy {
		walk(e)
	}
	for _, ot := range stmt.OrderBy {
		walk(ot.Expr)
	}
	for i := range srcs {
		walk(srcs[i].on)
		out[i] |= scopes[i].existsColUsed
	}
	if !ok {
		return nil, false
	}
	return out, true
}

// wherePlanWalkColumns calls visit for every plain column reference in e,
// stopping early once visit answers false. It is TOTAL by construction: any
// node it does not recognise -- a subquery, EXISTS, IN-over-subquery, MATCH, a
// row value, a window or post-rewrite internal node -- reports itself through
// visit(ColumnExpr{}) with an empty name, which no scope can resolve, so the
// caller's own "did not resolve" decline covers it. That is deliberate: a leaf
// silently treated as column-free is exactly how a colUsed bit goes missing.
func wherePlanWalkColumns(e Expr, visit func(ColumnExpr) bool) bool {
	return wherePlanWalkColumnsSub(e, visit, nil)
}

// wherePlanWalkColumnsSub is wherePlanWalkColumns with a hook for nodes carrying
// a nested SELECT. With sub nil they report as unresolvable and the caller
// declines; wherePlanColUsed passes a hook to credit a subquery's outward names
// to enclosing items (where_plan_subcolused_r36d.go). wherePlanTermsFrom's
// colMask walk must not: a term with a subquery is refused by
// whereTermAnalyzable first.
func wherePlanWalkColumnsSub(e Expr, visit func(ColumnExpr) bool, sub func(*SelectStmt) bool) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return true
	case groupKeyExpr, groupBareColExpr, groupAggExpr:
		// An ENCLOSING aggregate's per-group value, substituted into a nested
		// statement (sql_group.go): C's TK_AGG_COLUMN/TK_AGG_FUNCTION against an
		// outer AggInfo, which credits no column of this FROM clause.
		return true
	case whereFixedCol:
		return visit(x.col) // still a column to sqlite3ExprCoveredByIndex
	case ColumnExpr:
		return visit(x)
	case UnaryExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub)
	case BinaryExpr:
		return wherePlanWalkColumnsSub(x.L, visit, sub) && wherePlanWalkColumnsSub(x.R, visit, sub)
	case IsNullExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub)
	case CollateExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub)
	case CastExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub)
	case BetweenExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub) &&
			wherePlanWalkColumnsSub(x.Lo, visit, sub) && wherePlanWalkColumnsSub(x.Hi, visit, sub)
	case LikeExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub) &&
			wherePlanWalkColumnsSub(x.Pattern, visit, sub) && wherePlanWalkColumnsSub(x.Escape, visit, sub)
	case GlobExpr:
		return wherePlanWalkColumnsSub(x.X, visit, sub) && wherePlanWalkColumnsSub(x.Pattern, visit, sub)
	case SubqueryExpr:
		if sub == nil {
			return visit(ColumnExpr{})
		}
		return sub(x.Stmt)
	case ExistsExpr:
		if sub == nil {
			return visit(ColumnExpr{})
		}
		return sub(x.Stmt)
	case InExpr:
		if x.Sub != nil {
			if sub == nil {
				return visit(ColumnExpr{})
			}
			return wherePlanWalkColumnsSub(x.X, visit, sub) && sub(x.Sub)
		}
		if !wherePlanWalkColumnsSub(x.X, visit, sub) {
			return false
		}
		for _, a := range x.List {
			if !wherePlanWalkColumnsSub(a, visit, sub) {
				return false
			}
		}
		return true
	case CaseExpr:
		if x.Base != nil && !wherePlanWalkColumnsSub(x.Base, visit, sub) {
			return false
		}
		for _, w := range x.Whens {
			if !wherePlanWalkColumnsSub(w.When, visit, sub) || !wherePlanWalkColumnsSub(w.Then, visit, sub) {
				return false
			}
		}
		return x.Else == nil || wherePlanWalkColumnsSub(x.Else, visit, sub)
	case FuncExpr:
		if x.Over != nil {
			// A window call's arguments, PARTITION BY, ORDER BY and FILTER set colUsed bits
			// like any expression (resolve.c:1324, 1332-1341). Only a self-contained
			// "OVER (...)" is walked; a named window inherits from the WINDOW clause, not
			// visible here, so it stays opaque.
			if x.Over.Ref != "" {
				return visit(ColumnExpr{})
			}
			// A FILTER on a non-aggregate window function is invalid in C ("FILTER may not be
			// used with non-aggregate lead()"), but only the aggregate+window path here checks
			// it. Walking it would let such a statement be answered instead of rejected, so
			// stay opaque when a FILTER is present.
			if x.Filter != nil {
				return visit(ColumnExpr{})
			}
			for _, a := range x.Args {
				if !wherePlanWalkColumnsSub(a, visit, sub) {
					return false
				}
			}
			for _, e := range x.Over.PartitionBy {
				if !wherePlanWalkColumnsSub(e, visit, sub) {
					return false
				}
			}
			for _, ot := range x.Over.OrderBy {
				if !wherePlanWalkColumnsSub(ot.Expr, visit, sub) {
					return false
				}
			}
			if fr := x.Over.Frame; fr != nil {
				if fr.Start.Offset != nil && !wherePlanWalkColumnsSub(fr.Start.Offset, visit, sub) {
					return false
				}
				if fr.End.Offset != nil && !wherePlanWalkColumnsSub(fr.End.Offset, visit, sub) {
					return false
				}
			}
			return true
		}
		// A FILTER clause goes through the SAME NameContext walk every argument
		// does -- resolve.c:1352, "if( ExprHasProperty(pExpr, EP_WinFunc) ){
		// sqlite3WalkExpr(pWalker, pExpr->y.pWin->pFilter); }" on the
		// TK_AGG_FUNCTION arm -- so lookupName sets a colUsed bit for every
		// column in it, exactly like an argument's.
		if x.Filter != nil && !wherePlanWalkColumnsSub(x.Filter, visit, sub) {
			return false
		}
		for _, ob := range x.orderByExprs() {
			if !wherePlanWalkColumnsSub(ob, visit, sub) {
				return false
			}
		}
		for _, a := range x.Args {
			if !wherePlanWalkColumnsSub(a, visit, sub) {
				return false
			}
		}
		// A bare "count(*)" names no column at all, which is exactly why
		// select.c's tag-select-0410 has a "pItem->colUsed==0" case.
		return true
	default:
		return visit(ColumnExpr{})
	}
}

// sqliteExecOrder reads back the verdict markWherePlanEligibility stashed on each
// FROM item, or returns ok == false to leave the order to computeExecOrder. It
// reads rather than plans because planning needs the statement (which list
// becomes pOrderBy), and computeExecOrder only has []joinedTable.
//
// keys[level], when non-nil, is the b-tree the winning plan walks at that level
// (an automatic or real index), whose key decides that level's visit order. The
// plain rowid scan is a nil key, so emitJoinLoops leaves the cursor alone and can
// stream the outermost level. See OpAutoIndexOrder.
func sqliteExecOrder(jts []joinedTable, scopes []tableScope, where Expr) ([]int, []*autoIndexKey, bool) {
	if len(jts) < 1 || len(scopes) < len(jts) {
		return nil, nil, false
	}
	if len(jts) == 1 {
		// One FROM item: there is no loop ORDER to decide, only which index
		// SQLite scans it through -- which decides the order its rows arrive
		// in just as surely. markWherePlanIndexEligibility already priced it.
		if jts[0].idxOrderKey == nil {
			return nil, nil, false
		}
		return []int{0}, []*autoIndexKey{jts[0].idxOrderKey}, true
	}
	order := make([]int, len(jts))
	keys := make([]*autoIndexKey, len(jts))
	seen := make([]bool, len(jts))
	for i := range jts {
		k := jts[i].idxOrderKey
		if !jts[i].wherePlanOK || jts[i].tbl == nil || k == nil {
			return nil, nil, false
		}
		if k.level < 0 || k.level >= len(jts) || seen[k.level] {
			return nil, nil, false
		}
		seen[k.level] = true
		order[k.level] = i
		if len(k.cols) > 0 || k.multiOr != nil {
			keys[k.level] = k
		}
	}
	return order, keys, true
}

// wherePlanDecidedOrder reports whether the ported planner decided this FROM
// clause's row order: wherePlanOrder for several tables, the index-order winner
// for one. False means the fallback order, which matches C only by accident.
// Used by the aggregate anchor guard; it makes the same call as the codegen.
// Build jts with joinedTablesFor so the question is about the compiled FROM.
func wherePlanDecidedOrder(jts []joinedTable, scopes []tableScope, where Expr) bool {
	if hasRightOuter(jts) || hasGroups(jts) {
		// computeExecOrder answers FROM order outright for these and never
		// consults the port at all.
		return false
	}
	_, _, ok := sqliteExecOrder(jts, scopes, where)
	return ok
}

// onClausesStayInScope rejects an order this codegen cannot emit, which now
// concerns only an outer join's ON, still pinned to its own FROM item. An INNER
// item's ON is moved to the WHERE (onToWhere) and imposes no order constraint.
//
// An outer ON is not moved on purpose: C can move it only because the term is
// tagged EP_OuterON with the NULL-extended item's cursor, which keeps it from
// being tested early, makes it decide match versus NULL-extension, and feeds
// constraintCompatibleWithOuterJoin. Here the match flag lives at the item's
// nesting depth, so a moved outer ON would filter NULL-extended rows away,
// turning a LEFT JOIN into an inner one.
func onClausesStayInScope(jts []joinedTable, scopes []tableScope, order []int) bool {
	pos := make([]int, len(jts))
	for p, orig := range order {
		pos[orig] = p
	}
	for i, jt := range jts {
		if jt.on == nil || jt.onToWhere {
			continue
		}
		refs := map[int]bool{}
		collectTableRefs(jt.on, scopes, refs)
		for r := range refs {
			if r < len(pos) && pos[r] > pos[i] {
				return false
			}
		}
	}
	return true
}



