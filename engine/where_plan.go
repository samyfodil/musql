// The join loop-order planner, ported from src/where.c: whereLoopAddAll's
// prerequisite masks, whereLoopAddBtree's cost model, whereLoopOutputAdjust,
// whereLoopFindLesser and wherePathSolver's dynamic program with its
// lexicographic tiebreak. Costs are LogEst (where_cost.go).
//
// Why a port and not a heuristic: loop order is observable (group_concat, a
// bare column in an aggregate, LIMIT without a total ORDER BY), so "an equally
// sensible order" is a wrong answer.
//
// This file owns exprAnalyze's half -- turning a resolved WHERE/ON into the
// WhereTerm list -- plus the entry point and the automatic index key. The cost
// model lives in where_plan_index.go and is shared by the one-table and
// multi-table arms: one WhereLoop type and one solver, as in C.
//
// Scope: wherePlanOrder answers only for the subset it reproduces exactly and
// reports ok == false otherwise (the caller keeps its existing order). The
// subset is on markWherePlanEligibility (where_plan_gate.go): plain local rowid
// base tables, top level, no subquery, LIKE/GLOB-family term or likelihood()
// hint in the WHERE.
package engine

import "strings"

// whereMask is SQLite's Bitmask: bit i is FROM item i. SQLite's is 64 bits
// wide with the top bit shared by every table past 63; wherePlanOrder declines
// long FROM clauses outright rather than reproduce that folding.
type whereMask uint64

// whereItem is one FROM item as whereLoopAddAll sees it.
type whereItem struct {
	// outer is JT_OUTER: the item is a LEFT, RIGHT or FULL join target. Such
	// an item may never be lifted above anything to its left.
	outer bool
	// fromExists is SrcItem.fg.fromExists: the item existsToJoin moved out of
	// a WHERE EXISTS (where_plan_exists.go). Its loops report nOut 0, it never
	// skip-scans, and it is never placed left of a table its terms name.
	fromExists bool
	// cross is JT_CROSS: the CROSS keyword was actually written. SQLite makes
	// it a reorder barrier purely as a planner hint -- "no real-world query
	// that cares about performance uses the CROSS JOIN syntax" -- and it is a
	// barrier even when an ON clause is present, which is why this is a
	// separate flag rather than a join KIND (see FromItem.CrossKeyword).
	cross bool
	// cols is the item's column list, for the affinity half of
	// termCanDriveIndex. nil declines the port.
	cols []columnInfo
	// tabName is SrcItem.pSTab's identity, folded: computeMxChoice compares
	// two items' Table POINTERS to spot a self-join, and every item the gate
	// admits is a plain base table in the main schema, so equal folded names is
	// that same test. Not the ALIAS -- two aliases of one table are a self-join.
	tabName string
	// colUsed is SrcItem.colUsed: bit N set if column N of this item is
	// referenced anywhere in the statement. It is what makes an index a
	// COVERING one (whereLoopAddBtree's "m = pSrc->colUsed & pProbe->colNotIdxed"),
	// and it also decides the key columns constructAutomaticIndex appends after
	// the constrained ones -- see wherePlanAutoIndexKey.
	// colUsedOK is false when markWherePlanEligibility could not compute it
	// exactly, which declines the port outright: with a real index in the FROM
	// clause colUsed decides WHICH loops exist, not merely one key's shape.
	colUsed   uint64
	colUsedOK bool

	// The Index metadata whereLoopAddBtree prices: build.c's estimateTableWidth,
	// the INTEGER PRIMARY KEY column (for whereScanInit's "iColumn==iPKey"
	// fold), and the probe chain itself -- the fake sPk first, then every real
	// index, exactly as wherePlanIndexList (where_plan_index.go) assembles it.
	ipkIndex int
	szTabRow logEst
	idxs     []whereIndexInfo
	// nRowLogEst is pTab->nRowLogEst as the loaded statistics leave it
	// (planStat1.apply), when nRowLogEstSet; otherwise build.c's default 200.
	nRowLogEst    logEst
	nRowLogEstSet bool
	// indexedBy is pSrc->fg.isIndexedBy: with an INDEXED BY hint the probe chain
	// holds only the named index and no sPk, and the "full scan via index" arm
	// is taken unconditionally. notIndexed is pSrc->fg.notIndexed, which stops
	// the chain at sPk; both switch off the automatic-index block.
	indexedBy  bool
	notIndexed bool
	// onePass is WHERE_ONEPASS_DESIRED: an UPDATE's or DELETE's own scan, for
	// which where.c:4240 withholds the covering-index full scan
	// ("(pWInfo->wctrlFlags & WHERE_ONEPASS_DESIRED)==0"). See
	// DB.updateOnePassOrder.
	onePass bool
	// noRowid is !HasRowid(pTab): a WITHOUT ROWID table, whose probe chain
	// carries no fake sPk at all and whose every index loop is therefore the
	// "full scan via index" arm -- which where.c:4233's own "|| !HasRowid(pTab)"
	// builds unconditionally, without asking whether the index is covering or
	// narrower than the table row.
	noRowid bool
}

// whereDefaultRowLogEst is build.c's pTable->nRowLogEst for a table with no
// sqlite_stat1 row: 200 == sqlite3LogEst(1048576). Every table looks the same
// size to the planner until ANALYZE runs, which is exactly why the loop order
// is a pure function of the schema and the SQL rather than of the data.
const whereDefaultRowLogEst logEst = 200

// rowLogEst is the item's pTab->nRowLogEst.
func (it *whereItem) rowLogEst() logEst {
	if it.nRowLogEstSet {
		return it.nRowLogEst
	}
	return whereDefaultRowLogEst
}

// wherePlanInput is everything wherePlanOrder needs, assembled by the caller
// from a resolved FROM clause. terms must be in SQLite's own order: the WHERE
// clause's conjuncts left to right, then each item's ON conjuncts in FROM
// order (sqlite3ProcessJoin ANDs every ON onto the end of the WHERE clause
// before the planner ever sees it), with the commuted copies appended after ALL
// of them in REVERSE base order -- see wherePlanTermsFrom.
type wherePlanInput struct {
	items []whereItem
	terms []whereIdxTerm
	// sort is pWInfo->pOrderBy plus the wctrlFlags bits that decide what
	// wherePathSatisfiesOrderBy makes of it; nil means the statement asks no
	// ordering question at all.
	sort *whereSortCtl
	// noAutoIndex is "(pParse->db->flags & SQLITE_AutoIndex)==0" -- PRAGMA
	// automatic_index off -- which skips whereLoopAddBtree's whole
	// automatic-index block.
	noAutoIndex bool
	// nQueryLoop is pParse->nQueryLoop as of this statement's own compile --
	// see compiler.nQueryLoop. Zero (the Go default) reproduces pre-existing
	// behavior exactly: a true top-level statement always has nQueryLoop 0
	// (prepare.c:788), which is the only value every caller of wherePlanOrder/
	// wherePlanSingleIndexKey passed before this field existed. Only a
	// TRUSTED nested compile (wherePlanSingleTableIndexOrder/
	// wherePlanMultiTableOrder, gated on compiler.nQueryLoopKnown) sets it
	// nonzero. Consumed by (*wherePlanIdxBuild).solve's seed path, mirroring
	// wherePathSolver's "aFrom[0].nRow = MIN(pParse->nQueryLoop, 48)"
	// (where.c:5921).
	nQueryLoop logEst
	// winner, when set, is told what the single-table plan's winning loop is
	// (compiler.planWinner).
	winner *wherePlanWinner
	// loops, when set, carries whereLoopAddAll's loops from one solve of this
	// input to the next: wherePlanSeedInvariant solves it once per seed, and
	// nothing addAll builds reads the seed.
	loops *whereLoopCache
	// orSubclause is WHERE_OR_SUBCLAUSE: this is the sqlite3WhereBegin the
	// case-5 arm runs for ONE sub-scan of a WHERE_MULTI_OR loop
	// (wherecode.c:2431), which never takes whereShortCut (where.c:6364) and
	// never builds an automatic index (where.c:4067, which noAutoIndex carries).
	orSubclause bool
	// reverseOrder is db->flags & SQLITE_ReverseOrder -- PRAGMA
	// reverse_unordered_selects. The SOLVER never reads it (where.c:7126 applies
	// it after wherePathSolver): it is here for the WHERE_MULTI_OR sub-scans a
	// multi-table plan re-plans, each of which is its own sqlite3WhereBegin with
	// no ORDER BY, and so each of which is reversed under the pragma whatever the
	// outer statement's ORDER BY says.
	reverseOrder bool
}

type whereLoopCache struct {
	loops          []whereIdxLoop
	limitHit       bool
	partialUnknown bool
	populated      bool
}

// wherePlanWinner is what a caller planning an UPDATE's loop, or a subquery
// inside one, needs to know about the loop that won beyond its order: whether
// it is an AUTOMATIC index (WHERE_AUTO_INDEX), and whether it returns at most
// one row (WHERE_ONEROW, which makes an UPDATE ONEPASS_SINGLE rather than
// ONEPASS_MULTI, update.c:733/where.c's eOnePass choice).
type wherePlanWinner struct {
	auto, oneRow bool
}

// wherePlanOrder returns the FROM-item order SQLite binds its nested loops in,
// innermost last, plus each level's access path, or ok == false outside the
// ported subset.
//
// keys[i], when non-nil, is the key of the b-tree walked at level i -- an
// automatic index (wherePlanAutoIndexKey) or a real one
// (wherePlanIndexOrderKey) -- whose order that level's rows arrive in. nil is
// the ascending rowid scan. See autoIndexKey and OpAutoIndexOrder.
//
// nRow is pWInfo->nRowOut (wherePlanResult.nRow), used by
// wherePlanMultiTableOrder's caller to bump nQueryLoop; meaningless when ok is
// false.
func wherePlanOrder(in wherePlanInput) (order []int, keys []*autoIndexKey, nRow logEst, ok bool) {
	n := len(in.items)
	if n < 2 || n > 64 {
		// n < 2 has nothing to order. 64 is BMS (sqliteInt.h:1411), the width of
		// SQLite's own Bitmask: sqlite3WhereBegin refuses a longer FROM clause
		// outright ("at most 64 tables in a join", where.c:6880), so there is no
		// plan above it to agree with.
		return nil, nil, 0, false
	}
	for i := range in.items {
		if in.items[i].cols == nil || len(in.items[i].idxs) == 0 {
			return nil, nil, 0, false
		}
	}
	b := &wherePlanIdxBuild{
		items: in.items, terms: in.terms,
		sort: in.sort, noAutoIndex: in.noAutoIndex, nQueryLoop: in.nQueryLoop,
		cache: in.loops,
	}
	path, pok := b.plan()
	if !pok {
		return nil, nil, 0, false
	}
	order = make([]int, n)
	keys = make([]*autoIndexKey, n)
	for level, li := range path.loops {
		order[level] = b.loops[li].iTab
	}
	for level, li := range path.loops {
		l := &b.loops[li]
		switch {
		case l.multiOr:
			// A WHERE_MULTI_OR loop won this level. An OR term whose loop did NOT
			// win is only a filter, and the plan around it stands as solved --
			// which is why an orInfo anywhere in the clause is no longer a
			// decline by itself. This one's rows arrive in a CONCATENATION of
			// sub-scans, re-derived per outer row (wherePlanJoinMultiOrKey), and
			// pParse->nQueryLoop has already been bumped by this plan's nRowOut
			// when the case-5 arm plans them (where.c:7198).
			if len(l.lTerm) == 0 {
				return nil, nil, 0, false
			}
			k, ok := wherePlanJoinMultiOrKey(in, order, level, l.lTerm[0], in.nQueryLoop+path.nRow)
			if !ok {
				return nil, nil, 0, false
			}
			keys[level] = k
		case l.autoIdx:
			keys[level] = wherePlanAutoIndexKey(in, order, level)
		default:
			keys[level] = wherePlanIndexOrderKey(l.idx, path.rev[level])
		}
	}
	// autoIndexKey.groupsSorted for a join: select.c:8545's
	// "sqlite3WhereIsOrdered(pWInfo)==pGroupBy->nExpr" is about the whole
	// path, so every level carries the same answer. A satisfied GROUP BY
	// leaves revMask 0 (only WHERE_SORTBYGROUP, which needs an ORDER BY,
	// re-asks strictly), so a level walked through an index on t(a DESC)
	// emits groups descending and no sorter reorders them.
	//
	// A nil level becomes the empty key, which readers take as the rowid
	// scan; only the flag has to survive. whereReversedScanKey, the one
	// reader that tells nil from empty, runs only without ORDER BY.
	if in.sort != nil && in.sort.groupBy && in.sort.nOrderBy() > 0 &&
		int(path.isOrdered) == in.sort.nOrderBy() {
		for level := range keys {
			if keys[level] == nil {
				keys[level] = &autoIndexKey{}
			}
			keys[level].groupsSorted = true
		}
	}
	return order, keys, path.nRow, true
}

// wherePlanIndexOrderKey turns the winning loop's index into the key whose
// order its rows arrive in: the index's own key columns under their collating
// sequences and sort directions, then the trailing rowid. nil means the plain
// ascending-rowid table scan, which is what this engine already produces.
//
// rev is the loop's bit of pWInfo->revMask. Walking the b-tree backwards visits
// the key in the exactly-opposite order, which is the same permutation as
// flipping every column's direction -- the trailing rowid included, so there are
// no ties for the stable sort to preserve in the wrong order.
func wherePlanIndexOrderKey(idx *whereIndexInfo, rev bool) *autoIndexKey {
	if idx == nil {
		return nil
	}
	if idx.ipk {
		if !rev {
			return nil
		}
		// A REVERSE rowid scan. The key is the rowid alone, descending, which
		// autoIndexOrderCursor's stable sort turns into the exact reverse of the
		// forward walk (the rowid is unique, so it has no ties to preserve).
		return &autoIndexKey{cols: []int{xnRowid}, colls: []string{"BINARY"}, desc: []bool{true}}
	}
	k := &autoIndexKey{
		cols:  append([]int(nil), idx.aiColumn...),
		colls: append([]string(nil), idx.azColl...),
		desc:  append([]bool(nil), idx.aSortOrder...),
		root:  idx.root,
	}
	if rev {
		for i := range k.desc {
			k.desc[i] = !k.desc[i]
		}
	}
	return k
}

// termCanDriveIndex ports termCanDriveIndex (where.c). notReady is the C's own
// third argument: whereLoopAddBtree passes 0 when it PRICES an automatic-index
// loop (it guards maskSelf separately), constructAutomaticIndex passes the real
// mask when it BUILDS the key.
func termCanDriveIndex(t *whereIdxTerm, it whereItem, iTab int, notReady whereMask) bool {
	if t.cursor != iTab {
		return false
	}
	if t.op&(woEq|woIs) == 0 {
		return false
	}
	if it.outer && !constraintCompatibleWithOuterJoin(t, iTab) {
		return false
	}
	if t.prereqRight&notReady != 0 {
		return false
	}
	if t.column < 0 {
		return false // a rowid reference indexes nothing
	}
	if !indexAffinityOk(t.aff, it.cols[t.column].Aff) {
		return false
	}
	return columnIsGoodIndexCandidate(it, t.column)
}

// columnIsGoodIndexCandidate ports columnIsGoodIndexCandidate (where.c): a
// column that already LEADS one of the table's real indexes is not worth an
// automatic index, because that real index already serves the same constraint.
// Nor is one that is part of an index sqlite_stat1 says repeats each value of
// its prefix more than four times (aiRowLogEst[j+1] > 20): too unselective to
// be worth building one for (where.c:884).
//
// The fake sPk is skipped: it stands for the rowid and is not on pTab->pIndex.
// Before real indexes were priced here this whole routine was vacuously true,
// which is why it did not exist.
func columnIsGoodIndexCandidate(it whereItem, iCol int) bool {
	for i := range it.idxs {
		idx := &it.idxs[i]
		if idx.ipk {
			continue
		}
		for j := 0; j < idx.nKeyCol && j < len(idx.aiColumn); j++ {
			if idx.aiColumn[j] != iCol {
				continue
			}
			if j == 0 {
				return false
			}
			if idx.hasStat1 && j+1 < len(idx.aiRowLogEst) && idx.aiRowLogEst[j+1] > 20 {
				return false
			}
			break
		}
	}
	return true
}

// constraintCompatibleWithOuterJoin ports constraintCompatibleWithOuterJoin
// (where.c): only the outer join's OWN ON terms may constrain the item it
// NULL-extends. https://sqlite.org/forum/forumpost/51e6959f61
func constraintCompatibleWithOuterJoin(t *whereIdxTerm, iTab int) bool {
	return t.outerON && t.onItem == iTab
}

// indexAffinityOk ports sqlite3IndexAffinityOk (expr.c). cmp is the term's
// comparison affinity, idx the indexed column's declared affinity. affNone is
// this package's spelling of both SQLITE_AFF_NONE and SQLITE_AFF_BLOB, which
// are the two values the C tests with "aff < SQLITE_AFF_TEXT".
func indexAffinityOk(cmp, idx affinity) bool {
	switch cmp {
	case affNone:
		return true
	case affText:
		return idx == affText
	default:
		return isNumericAffinity(idx)
	}
}

// autoIndexKey is the key of one transient automatic index, from
// constructAutomaticIndex (where.c): the constrained columns in WHERE-term
// order (deduped), then the covering columns in table order, then the rowid.
// cols[i] indexes the table's columns, or -1 for the rowid; colls[i] is its
// collation.
//
// SQLite's seek yields a probe's matching rows in (extra cols..., rowid) order,
// while this engine full-scans and filters. Sorting the inner cursor's rows by
// the whole key once gives every matching subset the same relative order, so
// only the order changes, never the row set. See OpAutoIndexOrder.
//
// desc is parallel to cols, nil for an automatic index (always ascending), and
// set by wherePlanIndexOrderKey for a real index with DESC columns.
type autoIndexKey struct {
	cols  []int
	colls []string
	desc  []bool

	// multiOr, when set, replaces the whole cols/colls/desc key: the winning loop
	// is a WHERE_MULTI_OR union of one sub-scan per disjunct, whose order is a
	// CONCATENATION of sub-scans rather than any single b-tree key. cols is empty
	// in that case. See multiOrOrder (vdbe_op.go) and where_plan_multior_r37a.go.
	multiOr *multiOrOrder

	// level is the NESTING POSITION the ported planner gave the FROM item this
	// key belongs to, innermost last. It is meaningful only on the per-item
	// verdict markWherePlanEligibility stashes in joinSource.idxOrderKey for a
	// MULTI-table FROM clause, and it is how the loop ORDER reaches
	// sqliteExecOrder at all: the planner has to run where the STATEMENT is in
	// hand (it needs select.c's ORDER BY/GROUP BY/DISTINCT choice --
	// wherePlanSortCtlFor), and joinedTable, which is all sqliteExecOrder gets,
	// carries no statement. A verdict with no cols is "this level is the plain
	// ascending-rowid table scan", which needs no permutation at all.
	level int
	// root is the b-tree root page of the REAL index this key came from, or 0
	// for a transient automatic index and for the reversed-rowid key. It exists
	// so annotateJoinSeeks (vdbe_join_seek.go) can tell whether the correlated
	// seek it is about to install walks the very index the planner chose.
	root uint32

	// nEq and groupsSorted are the real-index path's answers to select.c's GROUP
	// BY codegen, which cannot re-derive them.
	//
	// nEq is the winning loop's u.btree.nEq: leading key columns pinned by
	// ==/IS/IS NULL. wherePathSatisfiesOrderBy skips those ("if(
	// j<pLoop->u.btree.nEq && j>=pLoop->nSkip )"); they are constant, so they say
	// nothing about group order.
	//
	// groupsSorted is "sqlite3WhereIsOrdered(pWInfo)==pGroupBy->nExpr": every GROUP
	// BY term satisfied, so no sorter and groups arrive in scan order.
	//
	// Both are zero for an automatic index and the reversed-rowid key. Read by
	// groupsArriveOutOfKeyOrder.
	nEq          int
	groupsSorted bool
}

// wherePlanAutoIndexKey ports constructAutomaticIndex's key construction
// (where.c) for the FROM item at nesting level `level`, or nil when no key can
// be built (the level's scan is left as it was).
//
// The term loop covers every term including the commuted copies (C walks
// pWC->a[0..nTerm), not the nBase prefix whereLoopOutputAdjust stops at), which
// is why in.terms carries them after the originals.
//
// notReady is where.c's: a loop's bit is cleared only after its level is coded,
// so at this level the mask still holds its own table, making a term whose RHS
// names this table or one bound inside it unusable. whereLoopAddBtree prices
// with notReady == 0, so the key built here can be wider than the pricing term;
// pLoop->u.btree.nEq is overwritten with the full count.
func wherePlanAutoIndexKey(in wherePlanInput, order []int, level int) *autoIndexKey {
	iTab := order[level]
	it := in.items[iTab]
	if !it.colUsedOK {
		return nil
	}
	var ready whereMask
	for i := 0; i < level; i++ {
		ready |= whereMask(1) << uint(order[i])
	}
	notReady := ^ready

	var idxCols uint64
	k := &autoIndexKey{}
	for i := range in.terms {
		t := &in.terms[i]
		if !termCanDriveIndex(t, it, iTab, notReady) {
			continue
		}
		// cMask folds every column at or past bit 63 onto bit 63; the caller's
		// gate declines a table that wide, so leftColumn is always its own bit.
		cMask := uint64(1) << uint(t.column)
		if idxCols&cMask != 0 {
			continue
		}
		idxCols |= cMask
		k.cols = append(k.cols, t.column)
		k.colls = append(k.colls, t.coll)
	}
	if len(k.cols) == 0 {
		// The winning loop was priced as an automatic index, so at least one
		// term drove it under notReady == 0 -- but that term's right-hand side
		// may name a table bound no earlier than this one, which the C's own
		// assert(nKeyCol>0) tolerates only because whereLoopAddBtree's prereq
		// makes it unreachable. Answer "no key" rather than assume.
		return nil
	}

	// extraCols. IsView's ALLBITS arm and the WITHOUT ROWID PRIMARY KEY
	// force-add are both unreachable here (the gate declines a view, a derived
	// table and a WITHOUT ROWID table), and so is "colUsed & MASKBIT(BMS-1)",
	// which only fires for a reference to column 63 or beyond.
	extra := it.colUsed & ^idxCols
	for c := 0; c < len(it.cols); c++ {
		if extra&(uint64(1)<<uint(c)) != 0 {
			k.cols = append(k.cols, c)
			k.colls = append(k.colls, "BINARY")
		}
	}
	k.cols = append(k.cols, -1) // XN_ROWID, azColl sqlite3StrBINARY
	k.colls = append(k.colls, "BINARY")
	return k
}

// whereCompareCollation ports sqlite3ExprCompareCollSeq (expr.c) over the
// shapes wherePlanOrder admits, resolving sqlite3BinaryCompareCollSeq in the
// operands' original order: explicit COLLATE left, then right, then the left
// column's collation, then the right's, else BINARY. One value serves a term
// and its commuted copy: exprCommute sets EP_Commuted only when the swap would
// change the answer, and sqlite3ExprCompareCollSeq then reads back to front.
func whereCompareCollation(jts []joinedTable, scopes []tableScope, l, r Expr) string {
	if n, ok := exprCollation(l); ok {
		return effectiveCollation(n)
	}
	if n, ok := exprCollation(r); ok {
		return effectiveCollation(n)
	}
	if n, ok := wherePlanDeclaredCollation(jts, scopes, l); ok {
		return effectiveCollation(n)
	}
	if n, ok := wherePlanDeclaredCollation(jts, scopes, r); ok {
		return effectiveCollation(n)
	}
	return "BINARY"
}

// wherePlanDeclaredCollation is sqlite3ExprCollSeq's no-EP_Collate half over
// those same shapes: it walks through TK_CAST and TK_UPLUS to a plain column
// and answers that column's DECLARED collating sequence. A rowid reference
// answers false -- the C breaks out with pColl still 0 for iColumn < 0.
func wherePlanDeclaredCollation(jts []joinedTable, scopes []tableScope, e Expr) (string, bool) {
	switch x := e.(type) {
	case CastExpr:
		return wherePlanDeclaredCollation(jts, scopes, x.X)
	case UnaryExpr:
		if x.Op == "+" {
			return wherePlanDeclaredCollation(jts, scopes, x.X)
		}
	case ColumnExpr:
		tab, col, ok := wherePlanColumnRef(jts, scopes, e)
		if !ok {
			_, _, coll, has, bound := wherePlanBoundOuter(scopes, x)
			return coll, bound && has
		}
		if col < 0 {
			return "", false
		}
		return jts[tab].tbl.cols[col].Collation, true
	case groupKeyExpr, groupBareColExpr:
		return declaredColumnCollation(nil, e)
	case whereFixedCol:
		return wherePlanDeclaredCollation(jts, scopes, x.col)
	}
	return "", false
}

// wherePlanTermsFrom builds the WhereClause exprAnalyze (whereexpr.c) leaves
// for a resolved FROM clause. sqlite3ProcessJoin ANDs every ON clause onto the
// WHERE first, so an ON constraint is an ordinary term carrying the join's
// identity; USING and NATURAL are already desugared into on by
// desugarJoinItem, the same rewrite.
//
// Term order is load-bearing (which candidate whereScanNext hands
// whereLoopAddBtreeIndex, automatic-index candidate order, and the automatic
// key itself), so it is exact: sqlite3WhereExprAnalyze walks base terms
// backwards ("for(i=pWC->nTerm-1; i>=0; i--)") and appends commuted copies,
// giving the base terms in clause order then their copies in reverse.
//
// ok == false for what this does not reproduce: a subquery, an OR neither
// wherePlanOrToIn nor wherePlanOrTermInfo can analyse, or a LIKE/GLOB/MATCH
// term (whereLoopOutputAdjust's third heuristic). A likelihood()/likely()/
// unlikely() at a conjunct's root is stripped and kept as truthProb, as
// whereClauseInsert does.
func wherePlanTermsFrom(jts []joinedTable, scopes []tableScope, where Expr) ([]whereIdxTerm, bool) {
	return wherePlanTermsFromClause(jts, scopes, where, true, true)
}

// wherePlanTermsFromClause is wherePlanTermsFrom for a WhereClause whose op is
// TK_AND (andClause) or TK_OR -- one disjunct of an OR analysed on its own,
// which exprAnalyzeOrTerm does in the OR's own clause. The LIKE optimization
// runs only in an AND clause ("pWC->op==TK_AND", whereexpr.c:1377). top is the
// statement's own WHERE, the one clause existsToJoin walks.
func wherePlanTermsFromClause(jts []joinedTable, scopes []tableScope, where Expr, andClause, top bool) ([]whereIdxTerm, bool) {
	var conj []whereConj
	for _, cj := range whereSplitAnd(where) {
		conj = append(conj, whereConj{e: cj, onItem: -1})
	}
	return wherePlanTermsFromConj(jts, scopes, conj, andClause, top)
}

// whereConj is one conjunct handed to the analyser together with the join
// markings sqlite3SetJoinExpr left on it (select.c): EP_OuterON and w.iJoin,
// here outerON and onItem (-1 for none). A WHERE conjunct carries none; a
// conjunct lifted out of an ON clause keeps its join's, which is what lets a
// sub-WHERE assembled from such conjuncts -- a WHERE_MULTI_OR sub-scan's
// "<disjunct> AND <pAndExpr>" (wherecode.c:2374-2431) -- be analysed exactly as
// the terms it copies were.
type whereConj struct {
	e       Expr
	outerON bool
	onItem  int
}

// wherePlanTermsFromConj is wherePlanTermsFromClause over conjuncts already
// split and marked, followed by every jts[i].on (sqlite3ProcessJoin's move).
func wherePlanTermsFromConj(jts []joinedTable, scopes []tableScope, conj []whereConj, andClause, top bool) ([]whereIdxTerm, bool) {
	type raw struct {
		e         Expr
		outerON   bool
		onItem    int
		truthProb logEst
	}
	var raws []raw
	// whereClauseInsert (whereexpr.c:80): the term is the conjunct with any
	// COLLATE and likelihood()/likely()/unlikely() at its root skipped, and
	// such a hint becomes the term's truthProb.
	add := func(cj Expr, outerON bool, onItem int) {
		e, tp, ok := whereSkipCollateAndLikely(cj)
		if !ok {
			e, tp = cj, 1 // an invalid probability: refused below as it stands
		}
		raws = append(raws, raw{e: e, outerON: outerON, onItem: onItem, truthProb: tp})
	}
	for _, c := range conj {
		add(c.e, c.outerON, c.onItem)
	}
	for i, jt := range jts {
		if jt.on == nil {
			continue
		}
		for _, cj := range whereSplitAnd(jt.on) {
			add(cj, jt.left || jt.rightOuter, i)
		}
	}

	subFailed := false
	maskOf := func(e Expr) whereMask {
		set := map[int]bool{}
		collectTableRefs(e, scopes, set)
		var m whereMask
		for i := range set {
			if i >= 63 {
				return ^whereMask(0)
			}
			m |= whereMask(1) << uint(i)
		}
		// exprSelectUsage (whereexpr.c): a subquery's outward-resolving names
		// are prerequisites too, which collectTableRefs does not follow.
		if containsSubquery(e) {
			sm, ok := wherePlanSubqueryUsage(jts, scopes, e)
			if !ok {
				subFailed = true
			}
			m |= sm
		}
		return m
	}

	// commuteToggles is exprCommute's EP_Commuted test (whereexpr.c:117): a
	// vector operand, or sqlite3BinaryCompareCollSeq answering differently
	// for the two operand orders.
	cv := wherePlanTermConv(jts, scopes)
	commuteToggles := func(x BinaryExpr) bool {
		_, lRow := x.L.(RowExpr)
		_, rRow := x.R.(RowExpr)
		return lRow || rRow || !strings.EqualFold(whereCompareCollation(jts, scopes, x.L, x.R),
			whereCompareCollation(jts, scopes, x.R, x.L))
	}
	// analyze is exprAnalyze over ONE expression: the WhereTerm it leaves
	// behind, plus the VIRTUAL terms it inserts alongside (a commuted copy, a
	// TERM_VNULL child), in creation order. self is the index the term itself
	// will occupy in the finished slice, which is what markTermAsChild stores
	// as those copies' iParent.
	analyze := func(e Expr, outerON bool, onItem int, self int) (whereIdxTerm, []whereIdxTerm) {
		var virt []whereIdxTerm
		ri := self
		t := whereIdxTerm{
			cursor: -1, column: xnRowid, rCursor: -1, rColumn: xnRowid,
			parent: -1, outerON: outerON, onItem: onItem,
			colMask: make([]uint64, len(jts)), colsKnown: true, truthProb: 1,
		}
		// A desugared row value's usage is its operand elements' -- read off
		// the comparison as written, whose elements constant propagation
		// rewrote exactly as C's walk does (where_plan_constprop.go).
		src := []Expr{e}
		if o := whereRowOrigin(e); o != nil {
			src = o.elems()
		}
		for _, s := range src {
			t.prereqAll |= maskOf(s)
		}
		if s, isLike := whereLikeOf(e); isLike && s.infix {
			t.likeInfix = true
			t.likeSz = estLikePatternLength(s.pattern, !s.glob)
		}
		// colMask is what sqlite3ExprCoveredByIndex walks: every column of every
		// FROM item this term references, per item.
		for _, s := range src {
			wherePlanWalkColumns(s, func(ce ColumnExpr) bool {
				tab, col, ok := wherePlanColumnRef(jts, scopes, ce)
				switch {
				case !ok:
					// A name bound OUTSIDE this FROM clause is another cursor's
					// or a register's, which sqlite3ExprCoveredByIndex never asks
					// this index about; anything else is not known.
					if _, _, _, _, bound := wherePlanBoundOuter(scopes, ce); !bound {
						t.colsKnown = false
					}
				case col >= 0 && col < 63 && tab < len(t.colMask):
					t.colMask[tab] |= uint64(1) << uint(col)
				case col >= 63:
					t.colsKnown = false
				}
				return t.colsKnown
			})
		}
		// extraRight: "ON clause terms may not be used with an index on the left
		// table of a LEFT JOIN. Ticket #3015." An EP_OuterON term's prereqAll
		// gains the NULL-extended item's bit, and any COMMUTED copy of it gains
		// every bit BELOW that item in its prereqRight.
		t.pcx = cv.conv(e)
		var extraRight whereMask
		if outerON && onItem >= 0 {
			x := whereMask(1) << uint(onItem)
			t.prereqAll |= x
			extraRight = x - 1
		}
		switch x := e.(type) {
		case IsNullExpr:
			if x.Not {
				// TK_NOTNULL is not allowedOp, so the term itself carries no
				// eOperator -- but exprAnalyze adds a VIRTUAL child "col > NULL"
				// (TERM_VNULL) for it, which IS an index range bound: NULLs sort
				// first in every index, so skipping past them is exactly what
				// "col IS NOT NULL" asks for. Without this the oracle's own
				// "SEARCH t USING COVERING INDEX i (a>?)" plan has no
				// counterpart here and the loop order comes out different
				// (measured on 2 of the 3 remaining shapes of the 800-case
				// placement battery).
				tab, col, ok := wherePlanColumnRef(jts, scopes, whereSkipCollate(x.X))
				if !ok || col < 0 || outerON {
					break // "pLeft->iColumn>=0 && !ExprHasProperty(EP_OuterON)"
				}
				v := t
				v.virt = true
				v.vnull = true
				v.pcx = &pcx{op: pcxGt, l: cv.conv(x.X), r: &pcx{op: pcxNull}} // "pLeft > NULL"
				v.parent = ri
				v.cursor, v.column = tab, col
				v.op = woGT
				v.prereqRight = 0
				// The comparison is "<column> > NULL": one side a column and the
				// other affinity-less, so comparisonAffinity is the column's own,
				// and sqlite3ExprCompareCollSeq is its declared collation.
				v.aff = jts[tab].tbl.cols[col].Aff
				v.coll = effectiveCollation(jts[tab].tbl.cols[col].Collation)
				virt = append(virt, v)
				break
			}
			tab, col, ok := wherePlanColumnRef(jts, scopes, whereSkipCollate(x.X))
			if !ok {
				break
			}
			if !outerON && col >= 0 && tab < len(jts) && jts[tab].tbl != nil &&
				col < len(jts[tab].tbl.cols) && jts[tab].tbl.cols[col].NotNull {
				// tag-20230504-1: "X IS NULL" on a NOT NULL column is rewritten
				// to the constant false, which clears prereqAll as well as
				// eOperator -- so the term stops contributing even its
				// whereLoopOutputAdjust decrement.
				t.prereqAll = 0
				t.pcx = &pcx{op: pcxTrueFalse, opaque: true}
				break
			}
			t.cursor, t.column, t.op = tab, col, woIsNull
		case BinaryExpr:
			// op/lhs/rhs are the comparison exprAnalyze sees, and lFirst/
			// rFirst the operands exprMightBeIndexed tests. For a desugared
			// row value that is the vector comparison as written, whose
			// vectors exprMightBeIndexed steps into -- to the FIRST element,
			// and for an inequality only (whereexpr.c:1080) -- without
			// skipping a COLLATE on it: "(c COLLATE nocase, d) > (1, 2)" is
			// indexable on no column, unlike its scalar spelling.
			op, lhs, rhs := x.Op, []Expr{x.L}, []Expr{x.R}
			lFirst, rFirst := whereSkipCollate(x.L), whereSkipCollate(x.R)
			o := x.row
			if o != nil {
				op, lhs, rhs = o.op, o.l, o.r
				if o.op == "IN" {
					// "(a,b) IN (VALUES ...)": prereqRight is exprSelectUsage
					// of the VALUES, whose rows are the list. The term itself
					// indexes nothing (exprMightBeIndexed never matches a
					// TK_VECTOR for TK_IN); its per-field children are built
					// by the caller.
					rhs = nil
					for _, row := range o.list {
						rhs = append(rhs, row...)
					}
				}
			}
			for _, r := range rhs {
				t.prereqRight |= maskOf(r) // 0 for a BETWEEN, whose pRight is 0
			}
			if !whereAllowedOp(op) || o != nil && !o.isRange() {
				// A vector "=" or "IS" is allowedOp too, but neither side is a
				// column to exprMightBeIndexed, so it gets no eOperator and no
				// commuted copy; tag-20220128a slices it (the caller).
				break
			}
			if o != nil {
				lFirst, rFirst = o.l[0], o.r[0]
			}
			var prereqLeft whereMask
			for _, l := range lhs {
				prereqLeft |= maskOf(l)
			}
			// opMask: an operator whose two sides share a table contributes only
			// WO_EQUIV, i.e. neither WO_EQ nor WO_IS -- "a.x = a.y" neither
			// reduces nOut's cap nor drives an index. (It is still not INERT:
			// WO_EQUIV alone is enough for whereScanNext's transitive closure.)
			opMask := woEquiv
			if prereqLeft&t.prereqRight == 0 {
				opMask = ^uint16(0) // WO_ALL
			}
			// exprAnalyze tests exprMightBeIndexed against
			// sqlite3ExprSkipCollate(pExpr->pLeft/pRight), so "a.x = b.y COLLATE
			// NOCASE" DOES give b.y a commuted term and hence an automatic index
			// on b -- the COLLATE only decides that index's collating sequence
			// (whereCompareCollation), never whether it exists. Missing this
			// skip left every explicit-COLLATE equi-join unable to build one
			// (measured: 6 of 240 shapes and one named rule in
			// compat-harness/joinorder_r28_autoindex_test.go).
			lTab, lCol, lOK := wherePlanColumnRef(jts, scopes, lFirst)
			rTab, rCol, rOK := wherePlanColumnRef(jts, scopes, rFirst)
			if _, fixed := rFirst.(whereFixedCol); fixed && o == nil {
				// "&& !ExprHasProperty(pRight, EP_FixedCol)" (whereexpr.c:1223)
				// -- tested on pRight itself, which for a row value is the
				// TK_VECTOR, never fixed, so its first element still commutes.
				rOK = false
			}
			// A vector's affinity and collation are its first element's
			// (sqlite3ExprAffinity and sqlite3ExprCollSeq, expr.c:74, 274),
			// and neither is ever a small integer (sqlite3ExprIsInteger).
			aff := whereComparisonAffinity(jts, scopes, lhs[0], rhs[0])
			coll := whereCompareCollation(jts, scopes, lhs[0], rhs[0])
			if o != nil {
				coll = whereRowCollation(jts, scopes, o.l, o.r)
			}
			isSmall := func(e []Expr) bool { return o == nil && whereIsSmallInt(e[0]) }
			switch {
			case lOK:
				t.cursor, t.column = lTab, lCol
				t.op = whereIdxOperatorMask(op, false) & opMask
				t.smallInt = isSmall(rhs)
				t.aff, t.coll = aff, coll
				if rOK {
					t.rCursor, t.rColumn = rTab, rCol
					// termIsEquivalence decides whether the pair takes part in
					// whereScanNext's transitive closure. The C ORs WO_EQUIV
					// onto the original AFTER masking, so a same-table equality
					// -- whose opMask left it operator-less -- still carries it.
					var eExtra uint16
					if termIsEquivalence(jts, scopes, x, outerON) {
						t.op |= woEquiv
						eExtra = woEquiv
					}
					// exprAnalyze appends a commuted copy so the OTHER table
					// can drive an index off the same equality, and
					// markTermAsChild makes the ORIGINAL its parent -- which is
					// what stops whereLoopOutputAdjust decrementing nOut for an
					// equality whose commuted copy already drove the loop.
					v := t
					v.virt = true
					v.parent = ri
					v.pcx = t.pcx.commute(commuteToggles(x))
					v.cursor, v.column = rTab, rCol
					v.rCursor, v.rColumn = lTab, lCol
					v.prereqRight = prereqLeft | extraRight
					v.smallInt = isSmall(lhs)
					v.op = (whereIdxOperatorMask(op, true) + eExtra) & opMask
					virt = append(virt, v)
				}
			case rOK:
				// exprCommute rewrites the term in place; there is no copy.
				t.pcx = t.pcx.commute(commuteToggles(x))
				t.cursor, t.column = rTab, rCol
				t.rCursor, t.rColumn = lTab, lCol
				t.prereqRight = prereqLeft | extraRight
				t.op = whereIdxOperatorMask(op, true) & opMask
				t.smallInt = isSmall(lhs)
				t.aff, t.coll = aff, coll
			}
		case InExpr:
			// exprAnalyze's TK_IN arm (whereexpr.c:1197). "x IN (v1,...,vK)"
			// has NO pRight at all -- prereqRight is the LIST's usage and
			// prereqAll is prereqLeft|prereqRight -- so there is no commuted
			// copy to make, and eOperator is operatorMask(TK_IN) == WO_IN
			// masked by the same WO_ALL/WO_EQUIV opMask every comparison gets.
			//
			// "x NOT IN (...)" is not this shape. parse.y:1240 wraps the TK_IN
			// in a TK_NOT, which is not allowedOp, so the term keeps eOperator
			// 0 and constrains nothing -- which is what falling out of
			// wherePlanInShape below leaves behind.
			inTab, inCol, n, ok := wherePlanInShape(jts, scopes, x)
			if !ok {
				break
			}
			var prereqRight whereMask
			for _, a := range x.List {
				prereqRight |= maskOf(a)
			}
			if x.Sub != nil {
				prereqRight = maskOf(SubqueryExpr{Stmt: x.Sub}) // exprSelectUsage
				t.inRefuse = wherePlanInRhsRefuse(jts, scopes, x)
			}
			t.prereqRight = prereqRight | extraRight
			if maskOf(x.X)&t.prereqRight != 0 {
				break // opMask == WO_EQUIV, and WO_IN & WO_EQUIV == 0
			}
			t.cursor, t.column = inTab, inCol
			t.op = woIn
			t.inCount = n
			// comparisonAffinity(TK_IN) (expr.c) is sqlite3ExprAffinity of the
			// LEFT operand alone -- pRight is 0 and there is no x.pSelect --
			// with SQLITE_AFF_BLOB standing in when that answers 0.
			// sqlite3ExprCompareCollSeq is sqlite3BinaryCompareCollSeq(pLeft,0),
			// i.e. an explicit COLLATE on the left, else its declared
			// collation, else BINARY. Both are what indexInAffinityOk
			// (where.c:319) hands whereScanNext for a non-vector IN.
			t.aff = whereStaticAffinity(jts, scopes, x.X)
			if x.Sub != nil {
				// comparisonAffinity folds the subquery's first result column
				// in: "aff = sqlite3CompareAffinity(pSelect->pEList->a[0].pExpr,
				// aff)" (expr.c).
				t.aff = whereComparisonAffinity(jts, scopes, x.X, SubqueryExpr{Stmt: x.Sub})
			}
			t.coll = whereInCollation(jts, scopes, x.X)
		}
		return t, virt
	}

	terms := make([]whereIdxTerm, len(raws))
	virt := make([]whereIdxTerm, 0, len(raws))
	orInfos := make([]*whereOrInfo, len(raws))
	// The BACKWARDS analysis walk. Each base term lands at its own clause
	// position; each virtual term is appended in the order it is created,
	// which is the reverse of that.
	for ri := len(raws) - 1; ri >= 0; ri-- {
		r := raws[ri]
		if top && whereTermNeedsUnportedRewrite(r.e) {
			return nil, false
		}
		if !whereTermAnalyzable(r.e) {
			// whereTermAnalyzable refuses every OR outright; the two shapes
			// exprAnalyzeOrTerm turns into something this port CAN price are
			// admitted here instead -- case 1's virtual IN (built below) and
			// case 3's WHERE_MULTI_OR union of sub-scans.
			if _, isOrIn := wherePlanOrToIn(jts, scopes, r.e); !isOrIn {
				// An OR inside an ON clause is offered too, with that clause's join
				// markings: sqlite3SetJoinExpr tags every node of the ON, disjuncts
				// included. wherePlanOrTermInfo analyses the OR's own clause, holding
				// only the disjuncts sqlite3WhereSplit produced (whereexpr.c:720), as C
				// does -- re-entering this routine over jts would hand the OR back to
				// itself forever.
				info, isMultiOr, orInert := wherePlanOrTermInfo(jts, scopes, r.e, r.outerON, r.onItem)
				switch {
				case isMultiOr:
					orInfos[ri] = info
				case orInert, wherePlanOrTermIsInert(r.e):
					// exprAnalyzeOrTerm never runs for this one, so it is an
					// ordinary opaque conjunct and needs no orInfo at all.
				default:
					return nil, false
				}
			}
		}
		t, copies := analyze(r.e, r.outerON, r.onItem, ri)
		t.truthProb = r.truthProb // its children follow it: see the tail
		if orInfos[ri] != nil {
			// exprAnalyzeOrTerm's case-3 tail (whereexpr.c:788): "pTerm->eOperator
			// = WO_OR; pTerm->leftCursor = -1". analyze left cursor at -1 already,
			// since TK_OR is not allowedOp.
			t.op = woOr
			t.orInfo = orInfos[ri]
		}
		virt = append(virt, copies...)
		// exprAnalyzeOrTerm's case 1 (whereexpr.c:830), "chngToIN": an OR whose
		// every subterm is "<same column> = <expr>" becomes one virtual
		// "column IN (...)" term, a child of the OR (markTermAsChild), so once
		// the IN drives the loop the OR stops contributing its output decrement.
		//
		// No WHERE_MULTI_OR loop is offered for a converted OR, and none is
		// needed: whereLoopAddOr prices the union as LogEstAdd over n sub-scans
		// plus one, and LogEstAdd of n equal values is the value plus
		// sqlite3LogEst(n) -- the IN loop's own "rRun += nIn". All subterms hit
		// the same column, so the MULTI_OR candidate is always one unit dearer
		// and whereLoopFindLesser discards it. A non-converting OR gets orInfo
		// from wherePlanOrTermInfo, priced by addOr.
		if orIn, isOrIn := wherePlanOrToIn(jts, scopes, r.e); isOrIn {
			self := len(raws) + len(virt)
			c, ccopies := analyze(orIn, r.outerON, r.onItem, self)
			c.virt = true
			c.parent = ri
			virt = append(virt, c)
			virt = append(virt, ccopies...)
		}
		// exprAnalyze's BETWEEN arm (whereexpr.c:1290): "x BETWEEN y AND z"
		// inserts virtual children "x>=y" and "x<=z", each analyzed (and
		// commutable) and made a child of the BETWEEN by markTermAsChild, with
		// the join markings copied (transferJoinMarkings). The BETWEEN keeps no
		// eOperator (TK_BETWEEN is not allowedOp), so only the children drive an
		// index, and being children stops whereIdxOutputAdjust decrementing for
		// the parent once one does.
		//
		// "x NOT BETWEEN y AND z" is not it: parse.y:1486 wraps TK_BETWEEN in
		// TK_NOT, and the arm tests pExpr->op itself, so it constrains nothing.
		// Only in an AND clause (whereexpr.c:1290): a BETWEEN inside an OR's
		// clause gets no children. A row-value BETWEEN's children are vector
		// ranges (whereexpr.c:1302).
		var betweenSubs []Expr
		if bt, isBetween := r.e.(BetweenExpr); isBetween && !bt.Not && andClause {
			betweenSubs = []Expr{
				BinaryExpr{Op: ">=", L: bt.X, R: bt.Lo},
				BinaryExpr{Op: "<=", L: bt.X, R: bt.Hi},
			}
		} else if o := whereRowOrigin(r.e); o != nil && o.op == "BETWEEN" && andClause {
			betweenSubs = []Expr{whereRowCmpExpr(">=", o.l, o.lo), whereRowCmpExpr("<=", o.l, o.hi)}
		}
		if betweenSubs != nil {
			for _, sub := range betweenSubs {
				self := len(raws) + len(virt)
				c, ccopies := analyze(sub, r.outerON, r.onItem, self)
				c.virt = true
				c.parent = ri
				virt = append(virt, c)
				virt = append(virt, ccopies...)
			}
		}
		// The LIKE optimization (whereexpr.c:1376-1453): two virtual range
		// terms, lower then upper, adjacent -- whereLoopAddBtreeIndex takes the
		// upper as "&pTerm[1]" -- and the LIKE term's children only when the
		// range alone decides it (isComplete).
		if s, isLike := whereLikeOf(r.e); isLike && andClause && len(scopes) > 0 {
			tab, col, isCol := wherePlanColumnRef(jts, scopes, s.x)
			textCol := isCol && col >= 0 && jts[tab].tbl.cols[col].Aff == affText
			if lo, hi, complete, ok := whereLikeRange(s, scopes[0].planCaseSensitiveLike, scopes[0].planUTF16LE, textCol); ok {
				for _, sub := range []Expr{lo, hi} {
					self := len(raws) + len(virt)
					c, _ := analyze(sub, r.outerON, r.onItem, self)
					c.virt, c.likeOpt = true, true
					if complete {
						c.parent = ri
					}
					virt = append(virt, c)
				}
			}
		}
		// The row-value arms that follow the LIKE optimization in exprAnalyze,
		// both "pWC->op==TK_AND" only. See where_plan_rowvalue.go.
		if o := whereRowOrigin(r.e); o != nil && andClause {
			switch {
			case o.isEq():
				// tag-20220128a (whereexpr.c:1455-1489): one "Li op Ri" slice
				// per field, APPENDED and analysed in turn (so a column-to-column
				// slice's commuted copy follows it). A slice is TERM_SLICE but
				// not TERM_VIRTUAL -- it extends pWC->nBase, and
				// whereLoopOutputAdjust counts it like any written conjunct --
				// and is no child: it keeps its own iParent -1 and truthProb.
				// Then the original is disabled, "pTerm->wtFlags |=
				// TERM_CODED|TERM_VIRTUAL; pTerm->eOperator = WO_ROWVAL".
				for i := range o.l {
					self := len(raws) + len(virt)
					c, ccopies := analyze(BinaryExpr{Op: o.op, L: o.l[i], R: o.r[i]}, r.outerON, r.onItem, self)
					virt = append(virt, c)
					virt = append(virt, ccopies...)
				}
				t.virt = true
				t.op = woRowval
			case o.op == "IN":
				virt = append(virt, whereRowInFields(jts, scopes, o, t, ri, maskOf)...)
			}
		}
		terms[ri] = t
	}
	// pAndExpr (wherecode.c:2374). Each WHERE_MULTI_OR sub-scan is planned over
	// "<disjunct> AND w", where w is every OTHER top-level term that carries an
	// eOperator -- so a term factored out of the disjunction can still drive the
	// sub-scan's index. Virtual terms are skipped (TERM_VIRTUAL), and so are
	// terms holding a subquery (tag-20220303a), which whereTermAnalyzable has
	// already refused outright. Collected here, once every term's eOperator is
	// known, rather than inside the backwards walk above.
	for ri := range orInfos {
		if orInfos[ri] == nil {
			continue
		}
		for j := range terms {
			// A sliced row-value equality is TERM_VIRTUAL by now (its slices,
			// TERM_SLICE, are skipped too, and are not base terms here).
			if j == ri || terms[j].virt {
				continue
			}
			// "(eOperator & WO_ALL)==0" skips a term. Every OR exprAnalyzeOrTerm
			// ran on carries WO_OR, whatever its cases concluded -- the tail sets
			// "pTerm->eOperator = WO_OR" unconditionally (whereexpr.c:789) -- so
			// an OR case 1 turned into an IN, or one no table is indexable for,
			// is in pAndExpr too, although this port gives it no operator.
			if terms[j].op == 0 && !wherePlanOrAnalysed(raws[j].e) {
				continue
			}
			orInfos[ri].others = append(orInfos[ri].others, raws[j].e)
			orInfos[ri].otherIdx = append(orInfos[ri].otherIdx, j)
		}
	}
	if subFailed {
		return nil, false
	}
	all := append(terms, virt...)
	// markTermAsChild (whereexpr.c:517): every derived child -- a commuted copy,
	// a BETWEEN bound, an OR's IN, a TERM_VNULL bound -- carries its parent's
	// truthProb. A child always follows its parent, so one pass settles chains.
	for i := range all {
		if all[i].virt && all[i].parent >= 0 && all[i].parent < i {
			all[i].truthProb = all[all[i].parent].truthProb
		}
	}
	return all, true
}

// termIsEquivalence ports termIsEquivalence (whereexpr.c): whether a
// column-to-column equality may be followed TRANSITIVELY by whereScanNext, so
// that a search for constraints on X also returns the constraints on Y.
// Condition (1) is the SQLITE_Transitive optimization flag, which is on by
// default and cannot be cleared through any interface this engine exposes.
// Condition (4) is a RIGHT JOIN test, and the gate declines those.
func termIsEquivalence(jts []joinedTable, scopes []tableScope, be BinaryExpr, outerON bool) bool {
	op := strings.ToUpper(be.Op)
	if op != "=" && op != "==" && op != "IS" { // (2)
		return false
	}
	if outerON { // (3), EP_OuterON
		return false
	}
	if _, ok := exprCollation(be.L); ok { // (3), EP_Collate
		return false
	}
	if _, ok := exprCollation(be.R); ok {
		return false
	}
	aff1 := whereStaticAffinity(jts, scopes, be.L)
	aff2 := whereStaticAffinity(jts, scopes, be.R)
	if aff1 != aff2 && (!isNumericAffinity(aff1) || !isNumericAffinity(aff2)) { // (5)
		return false
	}
	// (6) sqlite3ExprCollSeqMatch: the two sides' collating sequences must be
	// the same. With no explicit COLLATE on either side (checked above) that is
	// the two columns' declared collations.
	l, lok := wherePlanDeclaredCollation(jts, scopes, be.L)
	rc, rok := wherePlanDeclaredCollation(jts, scopes, be.R)
	if !lok {
		l = "BINARY"
	}
	if !rok {
		rc = "BINARY"
	}
	return equalFoldName(effectiveCollation(l), effectiveCollation(rc))
}

// whereTermAnalyzable rejects the term shapes exprAnalyze handles in ways this
// port does not reproduce -- see wherePlanTermsFrom's doc comment. An
// unrecognised node answers false rather than being treated as an opaque leaf:
// a leaf that in fact contained an OR would silently take the planner outside
// the subset it reproduces exactly.
func whereTermAnalyzable(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return true
	case UnaryExpr:
		return whereTermAnalyzable(x.X)
	case BinaryExpr:
		if x.row != nil {
			// A desugared row value: C analyses the vector comparison it was
			// written as (where_plan_rowvalue.go), whose OR -- if the tree
			// has one -- is no TK_OR at all.
			for _, el := range x.row.elems() {
				if !whereTermAnalyzable(el) {
					return false
				}
			}
			return true
		}
		if equalFoldName(x.Op, "OR") {
			return false // whereLoopAddOr builds WHERE_MULTI_OR loops
		}
		return whereTermAnalyzable(x.L) && whereTermAnalyzable(x.R)
	case IsNullExpr:
		return whereTermAnalyzable(x.X)
	case InExpr:
		if x.Sub != nil {
			// The subquery is an operand planned as its own statement; a
			// top-level non-negated one is refused by
			// whereTermNeedsUnportedRewrite.
			return whereTermAnalyzable(x.X)
		}
		if len(x.List) == 0 {
			return false
		}
		if !whereTermAnalyzable(x.X) {
			return false
		}
		for _, a := range x.List {
			if !whereTermAnalyzable(a) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return whereTermAnalyzable(x.X) && whereTermAnalyzable(x.Lo) && whereTermAnalyzable(x.Hi)
	case CollateExpr:
		return whereTermAnalyzable(x.X)
	case CastExpr:
		return whereTermAnalyzable(x.X)
	case CaseExpr:
		if x.Base != nil && !whereTermAnalyzable(x.Base) {
			return false
		}
		for _, w := range x.Whens {
			if !whereTermAnalyzable(w.When) || !whereTermAnalyzable(w.Then) {
				return false
			}
		}
		return x.Else == nil || whereTermAnalyzable(x.Else)
	case LikeExpr, GlobExpr:
		return whereLikeAnalyzable(x)
	case FuncExpr:
		switch r33sFoldIdent(x.Name) {
		case "like", "glob":
			if _, isLike := whereLikeOf(x); isLike {
				return whereLikeAnalyzable(x)
			}
		case "regexp", "match":
			return false
		}
		for _, a := range x.walkArgs() {
			if !whereTermAnalyzable(a) {
				return false
			}
		}
		return true
	case SubqueryExpr, ExistsExpr:
		// An operand of the term, planned as its own statement; what the term
		// depends on comes from wherePlanSubqueryUsage. A top-level EXISTS is
		// refused by whereTermNeedsUnportedRewrite instead.
		return true
	case groupBareColExpr, groupAggExpr:
		// An ENCLOSING aggregate's per-group value (sql_group.go): a WHERE is
		// never rewritten with its own query's placeholders, since it runs
		// before that query groups. To this scan it is a constant, as C's
		// TK_AGG_COLUMN/TK_AGG_FUNCTION reference to an outer AggInfo is.
		return true
	case whereFixedCol:
		return true
	case groupKeyExpr:
		// The same, unless its affinity is none: a typeless COLUMN key has
		// SQLITE_AFF_BLOB (sqlite3ExprAffinity's TK_AGG_COLUMN arm) where a
		// computed key has none, and the placeholder records neither apart.
		return x.aff != affNone
	default:
		// MatchExpr, RowExpr, RaiseExpr and anything added later.
		return false
	}
}

// whereTermNeedsUnportedRewrite reports a top-level conjunct existsToJoin
// (select.c:7326) would turn into a FROM item -- which the gate plans as that
// rewritten join (where_plan_exists.go), so meeting one HERE means planning the
// statement as though C had not. Every other EXISTS -- negated, under an OR, or
// with a subquery existsToJoin leaves alone -- is an ordinary opaque term in C
// too.
func whereTermNeedsUnportedRewrite(e Expr) bool {
	x, ok := e.(ExistsExpr)
	return ok && !x.Not && existsToJoinCandidate(x.Stmt)
}

// wherePlanInRhsRefuse reports an "x IN (SELECT ...)" whose IN loop visits a
// different row set than the WHERE describes. codeINTerm walks the RHS in the
// loop's direction (wherecode.c:720), so the order is always x's index order.
// But when the RHS column carries a COLLATE x does not, the ephemeral table
// dedups under the RHS's collation (expr.c:3752) while the seek uses x's, so C
// returns a different set than the WHERE alone. This engine realizes a plan as
// a walk plus the WHERE and has only the latter, so plan() refuses a winning
// loop using such a term. Only TEXT can merge that way.
func wherePlanInRhsRefuse(jts []joinedTable, scopes []tableScope, x InExpr) bool {
	if _, lhsExplicit := exprCollation(x.X); lhsExplicit {
		return false
	}
	if _, col, ok := wherePlanColumnRef(jts, scopes, whereSkipCollate(x.X)); ok && col < 0 {
		return false
	}
	lhsColl := whereInCollation(jts, scopes, x.X)
	// x.pSelect is the RIGHTMOST arm of a compound; checked on every arm.
	arms := []*SelectStmt{x.Sub}
	for _, c := range x.Sub.Compound {
		arms = append(arms, c.Stmt)
	}
	for _, arm := range arms {
		if len(arm.Columns) > 0 && !arm.Columns[0].Star {
			if n, ok := exprCollation(arm.Columns[0].Expr); ok && !equalFoldName(effectiveCollation(n), lhsColl) {
				return true
			}
		}
	}
	return false
}

// wherePlanSubqueryUsage is exprSelectUsage (whereexpr.c) over every subquery
// in e: the FROM items of this clause that names inside them resolve to --
// found by the same exact walk wherePlanColUsed uses (r36dSubColUsed), which
// resolves names at compile time and executes nothing. A name that resolves
// further out still is a constant here. ok false when a name inside cannot be
// resolved exactly.
func wherePlanSubqueryUsage(jts []joinedTable, scopes []tableScope, e Expr) (whereMask, bool) {
	if len(scopes) == 0 || scopes[0].planPager == nil {
		return 0, false
	}
	p := scopes[0].planPager
	var m whereMask
	ok := true
	mark := func(ce ColumnExpr) bool {
		if tab, _, found := wherePlanColumnRef(jts, scopes, ce); found && tab < 63 {
			m |= whereMask(1) << uint(tab)
		}
		return true
	}
	sub := func(inner *SelectStmt) bool {
		if !r36dSubColUsed(p, inner, nil, nil, 0, mark) {
			ok = false
			return false
		}
		return true
	}
	wherePlanWalkColumnsSub(e, func(ColumnExpr) bool { return true }, sub)
	return m, ok
}

// whereLikeAnalyzable is whereTermAnalyzable for a LIKE/GLOB, in either
// spelling. Its operands must be, and its pattern must not be a bound
// parameter: C plans that with the value bound at the time and re-prepares
// when it changes (see where_plan_like.go).
func whereLikeAnalyzable(e Expr) bool {
	s, ok := whereLikeOf(e)
	if !ok {
		// "x NOT LIKE y": an opaque term with no heuristic of its own.
		switch x := e.(type) {
		case LikeExpr:
			return whereTermAnalyzable(x.X) && whereTermAnalyzable(x.Pattern) && whereTermAnalyzable(x.Escape)
		case GlobExpr:
			return whereTermAnalyzable(x.X) && whereTermAnalyzable(x.Pattern)
		}
		return false
	}
	if _, isParam := whereSkipCollate(s.pattern).(ParamExpr); isParam {
		return false
	}
	return whereTermAnalyzable(s.x) && whereTermAnalyzable(s.pattern) && whereTermAnalyzable(s.escape)
}

// wherePlanInShape admits the IN this port can price: a plain column of one
// FROM item on the left and a non-empty value list on the right. It returns the
// item, the column (xnRowid for a rowid/IPK) and K == len(list), which is
// whereLoopAddBtreeIndex's nIn.
//
// Refused, each being a different algorithm:
//
//   - NOT IN: parse.y wraps TK_IN in TK_NOT, so the C term is inert too.
//   - a row-value LHS: one WhereTerm per field sharing a pExpr, with iField
//     bookkeeping.
//   - an empty list: the parser rewrites "x IN ()" to a constant.
//
// "x IN (SELECT ...)" answers K == -1: priced at a flat nIn of 46, "the SELECT
// returns 25 rows" (where.c:3337).
func wherePlanInShape(jts []joinedTable, scopes []tableScope, x InExpr) (int, int, int, bool) {
	if x.Not || x.Sub == nil && len(x.List) == 0 {
		return 0, 0, 0, false
	}
	if _, isRow := whereSkipCollate(x.X).(RowExpr); isRow {
		return 0, 0, 0, false
	}
	tab, col, ok := wherePlanColumnRef(jts, scopes, whereSkipCollate(x.X))
	if !ok {
		return 0, 0, 0, false
	}
	if x.Sub != nil {
		return tab, col, -1, true
	}
	return tab, col, len(x.List), true
}

// whereInCollation is sqlite3BinaryCompareCollSeq(pLeft, 0) -- what
// sqlite3ExprCompareCollSeq answers for a TK_IN, whose pRight is 0: an explicit
// COLLATE on the left operand, else that column's declared collating sequence,
// else BINARY.
func whereInCollation(jts []joinedTable, scopes []tableScope, l Expr) string {
	if n, ok := exprCollation(l); ok {
		return effectiveCollation(n)
	}
	if n, ok := wherePlanDeclaredCollation(jts, scopes, l); ok {
		return effectiveCollation(n)
	}
	return "BINARY"
}

// wherePlanOrToIn ports exprAnalyzeOrTerm's case-1 test and rewrite: the
// "column IN (rhs, ...)" the C inserts as a virtual child of an OR, or
// ok == false.
//
// C's chngToIN wants every subterm to be WO_EQ on one table and column, with a
// column RHS matching the left's affinity (ticket #2249). This reproduces a
// strict subset -- literal constant RHSs only -- because the rest needs
// machinery the port lacks:
//
//   - a column RHS brings a second table into chngToIN (the "2-bit case"),
//     whose second pass works on commuted copies inside the OR's sub-clause;
//   - a COLLATE on a subterm's left makes the IN's collation depend on which
//     subterm C kept pLeft from;
//   - a two-way OR whose operands compare equal reaches whereCombineDisjuncts
//     (whereexpr.c:600), which inserts another virtual term.
//
// A refused OR takes the statement out of the ported subset.
func wherePlanOrToIn(jts []joinedTable, scopes []tableScope, e Expr) (Expr, bool) {
	be, isBin := e.(BinaryExpr)
	if !isBin || !whereIsPlainOr(be) {
		return nil, false
	}
	// sqlite3WhereSplit(pOrWc, pExpr, TK_OR) flattens the whole OR tree.
	var subs []Expr
	var split func(Expr)
	split = func(x Expr) {
		if b, ok := x.(BinaryExpr); ok && whereIsPlainOr(b) {
			split(b.L)
			split(b.R)
			return
		}
		subs = append(subs, x)
	}
	split(be)
	if len(subs) < 2 {
		return nil, false
	}
	tab, col := -1, 0
	var lhs Expr
	var list []Expr
	for _, s := range subs {
		sb, ok := s.(BinaryExpr)
		if !ok || !whereIsEqOp(sb.Op) || equalFoldName(sb.Op, "IS") {
			return nil, false // eOperator must be WO_EQ, and TK_IS is WO_IS
		}
		if _, hasColl := exprCollation(sb.L); hasColl {
			return nil, false
		}
		if _, isLit := sb.R.(LiteralExpr); !isLit {
			return nil, false
		}
		t, c, ok := wherePlanColumnRef(jts, scopes, sb.L)
		if !ok {
			return nil, false
		}
		if tab < 0 {
			tab, col, lhs = t, c, sb.L
		} else if t != tab || c != col {
			return nil, false
		}
		list = append(list, sb.R)
	}
	if len(subs) == 2 && whereLiteralsMayCompareEqual(list[0], list[1]) {
		// whereCombineDisjuncts territory -- see the doc comment.
		return nil, false
	}
	return InExpr{X: lhs, List: list}, true
}

// whereLiteralsMayCompareEqual is a CONSERVATIVE sqlite3ExprCompare over two
// literals: it answers true whenever the C might call them equal. The C
// compares the two nodes' op and then their TOKEN TEXT with sqlite3StrICmp --
// so 'x' and 'X' ARE equal there while 1 and 1.0 are not (different ops) -- and
// this has the parsed VALUE rather than the token, so "1" and "01" answer true
// here where the C would say no. Erring that way only declines an OR that could
// have been converted; erring the other way would build the IN term for a pair
// the C sent to whereCombineDisjuncts first.
func whereLiteralsMayCompareEqual(a, b Expr) bool {
	la, aok := a.(LiteralExpr)
	lb, bok := b.(LiteralExpr)
	if !aok || !bok {
		return true
	}
	if la.Val.Typ != lb.Val.Typ {
		return false
	}
	if la.Val.Typ == Text {
		return equalFoldName(string(la.Val.S), string(lb.Val.S))
	}
	return valuesEqualExact(la.Val, lb.Val)
}

// whereAllowedOp ports allowedOp (whereexpr.c): the operators that can carry
// an eOperator at all.
func whereAllowedOp(op string) bool {
	switch strings.ToUpper(op) {
	case "=", "==", "<", "<=", ">", ">=", "IS":
		return true
	}
	return false
}

func whereIsEqOp(op string) bool {
	switch strings.ToUpper(op) {
	case "=", "==", "IS":
		return true
	}
	return false
}

// whereIsSmallInt ports sqlite3ExprIsInteger(pRight,&k,0) && -1<=k<=1.
func whereIsSmallInt(e Expr) bool {
	switch x := e.(type) {
	case LiteralExpr:
		if x.Val.Typ == Int {
			return x.Val.I >= -1 && x.Val.I <= 1
		}
	case UnaryExpr:
		if x.Op == "-" || x.Op == "+" {
			return whereIsSmallInt(x.X)
		}
	}
	return false
}

// wherePlanColumnRef resolves a plain column reference to (FROM index, column
// index), with column index -1 for a rowid pseudo-column -- exprMightBeIndexed
// (whereexpr.c) in the no-expression-index case. Anything else is not a column
// reference for the planner's purposes.
func wherePlanColumnRef(jts []joinedTable, scopes []tableScope, e Expr) (int, int, bool) {
	if f, fixed := e.(whereFixedCol); fixed {
		e = f.col // still TK_COLUMN to exprMightBeIndexed; see whereFixedCol
	}
	ce, ok := e.(ColumnExpr)
	if !ok {
		return 0, 0, false
	}
	lname := r33sFoldIdent(ce.Name)
	found, tab, col := 0, -1, -1
	if ce.UsingPinned {
		// A pinned side of a desugared USING/NATURAL condition
		// (ColumnExpr.UsingPinned): its scope name cannot identify its FROM item
		// (an unaliased self-join), so it is addressed by item index with an
		// empty Qualifier. Read as an ordinary name, both sides of "t JOIN t
		// USING(a)" would land on item 0, and every colMask/colUsed bit from the
		// right side would describe the wrong table. sqlite3ProcessJoin has the
		// item index in hand on both sides (select.c:594,638).
		if ce.UsingPinnedItem < 0 || ce.UsingPinnedItem >= len(scopes) {
			return 0, 0, false
		}
		ci, hit := scopes[ce.UsingPinnedItem].colIndex[lname]
		if !hit {
			// A USING/NATURAL common column is present in both items by
			// construction, so this is unreachable; declining rather than
			// falling through keeps a pin from ever being re-read as a name.
			return 0, 0, false
		}
		found, tab, col = 1, ce.UsingPinnedItem, ci
	}
	for i := range scopes {
		if ce.UsingPinned {
			break
		}
		if ce.Qualifier != "" && !equalFoldName(scopes[i].name, ce.Qualifier) {
			continue
		}
		if ce.Qualifier == "" && scopes[i].fromExists {
			// Every name existsToJoin's item answers was rebound to its own
			// qualifier (where_plan_exists.go); an unqualified one was resolved
			// before the rewrite, when that item was not in this FROM clause.
			continue
		}
		if ce.Qualifier == "" {
			// lookupName (resolve.c) SKIPS a column named in the item's own
			// USING clause -- "if( (pItem->fg.jointype & JT_USING)!=0 &&
			// nameInUsingClause(pItem->u3.pUsing, zCol) ) continue" -- so a
			// USING/NATURAL common column is NOT ambiguous: it resolves to the
			// leftmost (representative) table. tableScope.coalesced
			// is this package's own spelling of that set, and resolveColumn
			// applies the identical rule. A QUALIFIED reference is unaffected:
			// it always reads that table's own real column.
			if _, hidden := scopes[i].coalesced[lname]; hidden {
				continue
			}
		}
		if ci, hit := scopes[i].colIndex[lname]; hit {
			found++
			tab, col = i, ci
			if ce.UsingRepr && ce.Qualifier != "" {
				// A desugared USING/NATURAL condition's REPRESENTATIVE side,
				// qualified rather than pinned. desugarJoinItem (join.go) pins it
				// exactly when firstScopeNamed(qualifier) != repr, so on this
				// branch the FIRST scope bearing the qualifier IS the
				// representative -- which is also the scope resolveColumnEx
				// picks, its qualified branch returning on the first
				// name match. Counting on would make an unaliased self-join's own
				// condition ("t JOIN t USING(a)": both scopes named t) look
				// AMBIGUOUS and decline the plan, where sqlite3ProcessJoin has the
				// item index in hand and never asks (select.c:594).
				break
			}
		}
	}
	if found == 1 {
		if tab >= len(jts) || jts[tab].tbl == nil || col >= len(jts[tab].tbl.cols) {
			return 0, 0, false
		}
		if jts[tab].tbl.cols[col].IsRowidAlias {
			return tab, -1, true
		}
		return tab, col, true
	}
	// A name matching no declared column is the rowid pseudo-column, which
	// SQLite resolves to TK_COLUMN with iColumn == -1. A name matching two
	// tables is ambiguous, and anything else is an outer reference or a
	// select-list alias -- neither is this FROM clause's plain column.
	if found == 0 && isRowidName(lname) {
		if ce.Qualifier != "" {
			for i := range scopes {
				if equalFoldName(scopes[i].name, ce.Qualifier) {
					return i, -1, true
				}
			}
			return 0, 0, false
		}
		// Unqualified: lookupName keeps one rowid candidate per FROM item
		// (resolve.c:471) and takes it only when no real column matched,
		// assigning cnt = cntTab (resolve.c:623) -- so two candidates are
		// "ambiguous column name". The fallback is therefore single-item only.
		// cntTab counts VisibleRowid items, and every caller here admits only
		// plain rowid tables, so it is the FROM item count.
		only, n := 0, 0
		for i := range scopes {
			if !scopes[i].fromExists {
				only, n = i, n+1
			}
		}
		if n == 1 {
			return only, -1, true
		}
	}
	return 0, 0, false
}

// whereSkipCollate ports sqlite3ExprSkipCollate (expr.c): EP_Skip is set on
// TK_COLLATE and nothing else, so this unwraps explicit COLLATE operators only
// -- not the likelihood() family sqlite3ExprSkipCollateAndLikely also strips.
func whereSkipCollate(e Expr) Expr {
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			return e
		}
		e = c.X
	}
}

func isRowidName(lname string) bool {
	return lname == "rowid" || lname == "oid" || lname == "_rowid_"
}

// whereComparisonAffinity ports comparisonAffinity + sqlite3CompareAffinity
// (expr.c) over the shapes wherePlanOrder admits, only to decide whether an
// index on a column could serve the comparison.
//
// The both-columns arm needs care: C splits on "aff > SQLITE_AFF_NONE",
// separating no affinity (a literal, parameter, function) from SQLITE_AFF_BLOB
// (a BLOB-declared or typeless column). This package spells both affNone, so
// whereStaticAffinityOf's second result carries the distinction. Two columns,
// neither numeric, give SQLITE_AFF_BLOB -- no constraint -- not TEXT;
// otherwise sqlite3IndexAffinityOk refuses an automatic index C builds.
func whereComparisonAffinity(jts []joinedTable, scopes []tableScope, l, r Expr) affinity {
	// aff2 is comparisonAffinity's own starting value (the LEFT operand); aff1
	// is sqlite3CompareAffinity's argument (the RIGHT one).
	aff2, has2 := whereStaticAffinityOf(jts, scopes, l)
	aff1, has1 := whereStaticAffinityOf(jts, scopes, r)
	if has1 && has2 {
		// "Both sides of the comparison are columns. If one has numeric
		// affinity, use that. Otherwise use no affinity."
		if isNumericAffinity(aff1) || isNumericAffinity(aff2) {
			return affNumeric
		}
		return affNone // SQLITE_AFF_BLOB, which is "< SQLITE_AFF_TEXT"
	}
	// "One side is a column, the other is not. Use the column's affinity."
	if !has1 {
		if !has2 {
			return affNone // SQLITE_AFF_NONE
		}
		return aff2
	}
	return aff1
}

// whereStaticAffinityOf ports sqlite3ExprAffinity for the shapes a WHERE term in
// this subset can hold. The second result is "the C would answer something
// greater than SQLITE_AFF_NONE" -- true for a column reference and a CAST, false
// for a literal, a parameter and anything unrecognised, whose Expr.affExpr is 0.
func whereStaticAffinityOf(jts []joinedTable, scopes []tableScope, e Expr) (affinity, bool) {
	switch x := e.(type) {
	case CollateExpr:
		return whereStaticAffinityOf(jts, scopes, x.X)
	case CastExpr:
		// sqlite3AffinityType never answers 0, so a CAST always has one.
		return typeAffinity(x.Type), true
	case SubqueryExpr:
		// "if( op==TK_SELECT ) return sqlite3ExprAffinity(pSelect->pEList->a[0]
		// .pExpr)" (expr.c:54): its first result column's, as that column of
		// a derived table carries it. Of a compound, pSelect is the RIGHTMOST
		// arm -- the parser links each arm's pPrior to the one before it.
		if len(scopes) == 0 || scopes[0].planPager == nil {
			return affNone, false
		}
		stmt := x.Stmt
		if n := len(stmt.Compound); n > 0 {
			arm := *stmt.Compound[n-1].Stmt
			arm.CTEs = stmt.CTEs // WITH governs the whole compound
			stmt = &arm
		}
		cols := scopes[0].planPager.derivedColumnInfos(stmt, []string{""})
		if len(cols) == 0 {
			return affNone, false
		}
		return cols[0].Aff, !cols[0].NoAffinity
	case whereFixedCol:
		return whereStaticAffinityOf(jts, scopes, x.col)
	case groupKeyExpr:
		return x.aff, x.aff != affNone // whereTermAnalyzable refused the rest
	case groupBareColExpr:
		return x.aff, !x.noAff
	case ColumnExpr:
		tab, col, ok := wherePlanColumnRef(jts, scopes, e)
		if !ok {
			aff, has, _, _, bound := wherePlanBoundOuter(scopes, x)
			return aff, bound && has
		}
		if col < 0 {
			return affInteger, true // the rowid
		}
		return jts[tab].tbl.cols[col].Aff, true
	}
	return affNone, false
}

// whereStaticAffinity is whereStaticAffinityOf's affinity alone, for the
// callers that only need "which affinity", not "does it have one".
func whereStaticAffinity(jts []joinedTable, scopes []tableScope, e Expr) affinity {
	a, _ := whereStaticAffinityOf(jts, scopes, e)
	return a
}

// whereSplitAnd is sqlite3WhereSplit (whereexpr.c) for TK_AND: the conjuncts
// of e, splitting through any COLLATE or likelihood()/likely()/unlikely() at a
// node's root -- so "likely(a AND b)" is the two terms a and b, whose hint is
// dropped with the wrapper, exactly as there.
func whereSplitAnd(e Expr) []Expr {
	if e == nil {
		return nil
	}
	inner, _, _ := whereSkipCollateAndLikely(e)
	// A desugared row value is one TK_EQ/TK_BETWEEN/TK_IN term to C, whatever
	// AND it became here.
	if b, ok := inner.(BinaryExpr); ok && equalFoldName(b.Op, "AND") && b.row == nil {
		return append(whereSplitAnd(b.L), whereSplitAnd(b.R)...)
	}
	return []Expr{e}
}

// whereSkipCollateAndLikely is sqlite3ExprSkipCollateAndLikely (expr.c:218)
// with the truthProb whereClauseInsert derives from the outermost hint:
// "sqlite3LogEst(p->iTable) - 270", where iTable is the probability scaled by
// 2^27 -- exprProbability for likelihood()'s second argument (resolve.c:936),
// 8388608 for unlikely() and 125829120 for likely() (resolve.c:1181) -- and 1,
// "no hint", otherwise. ok is false for a likelihood() whose probability is not
// a floating-point literal in [0, 1], which C refuses at resolve time.
//
// Only a hint at the very ROOT sets truthProb -- whereClauseInsert tests
// EP_Unlikely on the conjunct itself, so a COLLATE over it hides it.
func whereSkipCollateAndLikely(e Expr) (inner Expr, truthProb logEst, ok bool) {
	truthProb, ok = 1, true
	first := true
	for ; ; first = false {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
			continue
		case FuncExpr:
			var iTable int64 = -1
			switch r33sFoldIdent(x.Name) {
			case "likelihood":
				if len(x.Args) == 2 {
					if lit, isLit := x.Args[1].(LiteralExpr); isLit && lit.Val.Typ == Float && lit.Val.F >= 0 && lit.Val.F <= 1 {
						iTable = int64(lit.Val.F * 134217728.0)
					} else {
						return e, 1, false
					}
				}
			case "unlikely":
				if len(x.Args) == 1 {
					iTable = 8388608
				}
			case "likely":
				if len(x.Args) == 1 {
					iTable = 125829120
				}
			}
			if iTable < 0 || x.Star || x.Distinct || x.Over != nil || x.Filter != nil {
				return e, truthProb, ok
			}
			if first {
				truthProb = logEstFromInt(uint64(iTable)) - 270
			}
			e = x.Args[0]
			continue
		}
		return e, truthProb, ok
	}
}