// This file turns the inner side of a nested-loop join from a full scan into an
// index/rowid seek keyed off the join column -- the correlated counterpart of
// vdbe_scan.go's detectRowidSeekKey/detectIndexSeekKey. The key is computed
// from tables bound strictly before this level and re-evaluated per outer row
// (emitJoinSeekHint emits OpSeekRowidHint/OpSeekIndexHint with the re-seek flag
// before the level's OpRewind; see vdbeCursor.seekReseek).
//
// # Why it is result- and order-neutral
//
// The seek replaces only the inner row source, never the condition test:
//
//   - Superset: SeekRowidSegments returns the row rowid==key, and
//     SeekIndexRowidsSegments the rows whose leading indexed column equals the
//     probe under the index's collation. The probe takes the comparison's
//     affinity and must match its collation, and seekAffinitySafe rejects any
//     pairing where that affinity could change the stored values' comparison
//     (which could miss matches).
//   - Re-evaluation: emitJoinLevel still emits the ON test and every pushed-down
//     WHERE conjunct, so the full condition is re-tested on each fetched row.
//   - Order: a rowid seek yields what a full scan would. An index seek matches
//     C's inner-loop order: codeOneLoopStart (wherecode.c) emits OP_SeekGE over
//     the equality prefix and walks forward until OP_IdxGE, so rows arrive in
//     index key order (key columns, then rowid). SeekIndexRowidsSegments sorts
//     by rowid, which is that order only for a one-column index.
//
// # LEFT JOIN
//
// A LEFT source is seeked off its ON conjuncts only. The NULL-extension is
// decided by matchReg, set only after the ON test passes, so a pruned superset
// cannot change it. codeOneLoopStart does the same for a LEFT JOIN's inner
// loop, and disableTerm consumes a term as a loop constraint on an iLeftJoin
// level only when it carries EP_OuterON.
//
// # Declined (full inner scan)
//
// RIGHT/FULL joins (the unmatched-row pass needs the full inner set), derived
// tables, WITHOUT ROWID inner tables, a cross-database source whose dbIdx
// names no attached reader, keys referencing this or a later level, subquery/
// aggregate/OR/IN keys, collation-mismatched or affinity-unsafe indexes, and a
// DESC-leading index (secondaryIndexSeekCandidates).
package engine

import (
	"slices"
	"strings"
)

// annotateJoinSeeks records a correlated inner-side seek plan on every inner
// join source (depth >= 1) whose key can be safely seeked, so emitJoinLevel
// seeks instead of scanning. It mutates only srcs[i].joinSeekKeyExpr /
// joinIdxSeek, so skipping it gives identical results. Called after
// joinPushdownPlan by each scan codegen. A single-table FROM is left to
// detectRowidSeekKey/detectIndexSeekKey.
//
// joinSeekDisabled, when true, makes annotateJoinSeeks a no-op. It is set only
// by the before/after benchmark (vdbe_join_seek_bench_test.go).
var joinSeekDisabled bool

func annotateJoinSeeks(c *compiler, srcs []joinSource, plan joinPlan) {
	if joinSeekDisabled || c == nil || c.pager == nil {
		return
	}
	if len(srcs) == 1 {
		annotateOuterKeySortedSeek(c, srcs, plan)
		return
	}
	order := plan.execOrder
	for level := 1; level < len(order); level++ {
		origIdx := order[level]
		s := srcs[origIdx]
		if s.tbl == nil || s.vtabItem != nil {
			continue
		}
		var planKey *autoIndexKey
		if level < len(plan.autoIdxKeys) {
			planKey = plan.autoIdxKeys[level]
		}
		// The cursors bound STRICTLY BEFORE this level are the only valid key
		// sources (they are positioned when the seek key is evaluated, right
		// before this level's OpRewind).
		outer := make(map[int]bool, level)
		for k := 0; k < level; k++ {
			outer[srcs[order[k]].scope.cursor] = true
		}
		// Candidate equalities come from this source's ON condition and its
		// pushed-down WHERE bucket, both still re-evaluated by emitJoinLevel.
		//
		// For a LEFT source only ON conjuncts qualify: a WHERE conjunct is not
		// part of the match-vs-NULL-extend decision, so pruning with one could
		// NULL-extend a row SQLite would join. planJoinPushdown already leaves
		// a LEFT level's bucket empty; this enforces the invariant the seek
		// depends on. A RIGHT/FULL operand's unmatched-row
		// sweep is decided the same way, by ON alone.
		var eqs []Expr
		eqs = append(eqs, splitTopLevelAnd(s.on)...)
		if !s.left && !s.rightOuter && level < len(plan.buckets) {
			eqs = append(eqs, plan.buckets[level]...)
		}
		eqs = plainUsingRefs(srcs, orCommonConjuncts(eqs))
		// RIGHT/FULL operands (whose sweep needs the full row list back) and
		// WITHOUT ROWID tables take only the sorted seek.
		// A derived table or CTE has no index or rowid to seek; its rows,
		// built once, take the sorted seek.
		table := s.derived == nil && s.cteItem == nil
		if table && !s.rightOuter && !s.tbl.withoutRowid && annotateIndexJoinSeek(c, srcs, origIdx, planKey, outer, eqs) {
			continue
		}
		if planKey != nil && planKey.multiOr != nil {
			continue // ordered per outer row, inside the loop
		}
		srcs[origIdx].joinSortedKey, srcs[origIdx].joinSortedMore, srcs[origIdx].joinSortedSeek = findJoinSortedSeek(c, s, outer, eqs)
	}
}

// annotateOuterKeySortedSeek gives the one subquery of a nested compile a
// sorted seek on an equality whose key names only the enclosing query, fixed
// for each run of it: the rows the key matches keep the order they had (a
// run of sortRowsBy keeps it), found by binary search where a correlated
// subquery would scan them all on every run.
func annotateOuterKeySortedSeek(c *compiler, srcs []joinSource, plan joinPlan) {
	s := srcs[0]
	if c.outer == nil || s.tbl == nil || s.derived == nil && s.cteItem == nil || s.vtabItem != nil ||
		s.left || s.rightOuter || len(plan.buckets) == 0 {
		return
	}
	if len(plan.autoIdxKeys) > 0 && plan.autoIdxKeys[0] != nil && plan.autoIdxKeys[0].multiOr != nil {
		return
	}
	eqs := plainUsingRefs(srcs, orCommonConjuncts(plan.buckets[0]))
	outerOnly := func(e Expr) bool {
		return exprRefsOnly(e, func(x ColumnExpr) bool {
			_, _, _, found, _, hard := resolveInScopes(c.scopes, x, c.pager)
			return hard == nil && !found
		})
	}
	srcs[0].joinSortedKey, srcs[0].joinSortedMore, srcs[0].joinSortedSeek = findJoinSortedSeekBy(c, s, outerOnly, eqs)
}

// annotateIndexJoinSeek installs a rowid or index seek on srcs[origIdx] when
// one applies, reporting whether it did.
func annotateIndexJoinSeek(c *compiler, srcs []joinSource, origIdx int, planKey *autoIndexKey, outer map[int]bool, eqs []Expr) bool {
	s := srcs[origIdx]
	// A level the planner gave an index-order key has a decided visit order.
	// OpAutoIndexOrder declines a re-seeking cursor (it re-materializes a
	// different row set per rewind), so the seek is kept only where its order
	// provably agrees (joinSeekReproducesKey).
	if planKey == nil || planKey.multiOr != nil {
		// A ROWID point lookup delivers its (at most one) row per outer row in
		// the table's own order, which is what an sPk loop produces -- and an
		// sPk loop carries no key at all. A non-nil key therefore means the
		// planner chose to WALK an index here, so the rowid seek would deliver
		// a different order and is not taken -- unless the key is a
		// WHERE_MULTI_OR's, whose order of one row is that row.
		if key, ok := findJoinRowidSeek(c, s, outer, eqs); ok {
			srcs[origIdx].joinSeekKeyExpr = key // strictly cheaper than an index seek
			return true
		}
	}
	// An index root page only means something in its own file. A
	// cross-database item's cursor opens on the attached reader (OpOpenRead's
	// P3 = dbIdx), so its index must be looked up there too; using the
	// compiling pager's root reads an unrelated b-tree in the attached file --
	// a wrong answer (TestR30CrossDatabaseSeekReadsTheRightFile). A dbIdx
	// naming no attached reader declines.
	sp := c.pager
	if s.dbIdx != 0 {
		if s.dbIdx-1 >= len(c.pager.attachedReaders) {
			return false
		}
		sp = c.pager.attachedReaders[s.dbIdx-1].pager
	}
	var want *autoIndexKey
	if planKey != nil {
		// A real index has a root, or, on a segment table (whose indexes are
		// SQL only), a name. A key with neither is a transient automatic index
		// or a REVERSED rowid scan.
		if planKey.root == 0 && (planKey.index == "" || !sp.ScannedFromSegments(s.tbl.root)) {
			return false
		}
		want = planKey
	}
	p, ok := findJoinIndexSeek(c, sp, s, outer, eqs, want)
	if !ok {
		return false
	}
	if planKey != nil {
		ok, sorted := joinSeekReproducesKey(sp, s.tbl, p.root, p.index, planKey)
		if !ok {
			return false
		}
		if sorted {
			p.order = planKey
		}
	}
	srcs[origIdx].joinIdxSeek = p
	return true
}

// joinSeekReproducesKey reports whether this level's correlated index seek will
// visit rows in exactly the order key names -- the order the ported planner
// decided. OpAutoIndexOrder declines a re-seeking cursor, so the seek's own
// order must agree with the planner's key; where it cannot be shown to, the
// caller drops the seek and the planner's key wins. It re-derives the index's
// key from its schema text (no automatic index, no rival index with the same
// leading column) and compares it with key column for column.
func joinSeekReproducesKey(sp *ReadOnlyPager, tbl *resolvedTable, root uint32, name string, key *autoIndexKey) (ok, sorted bool) {
	if sp == nil || tbl == nil || key == nil || key.root != root {
		return false, false
	}
	// A segment table's index (root 0) is found by name, and only there.
	if root == 0 && (name == "" || !equalFoldName(name, key.index) || !sp.ScannedFromSegments(tbl.root)) {
		return false, false
	}
	rows, err := sp.Schema()
	if err != nil {
		return false, false
	}
	var chosen *SchemaRow
	for i := range rows {
		r := &rows[i]
		if r.Type == "index" && (r.SQL != "" || root == 0) && (root != 0 && r.RootPage == root || root == 0 && r.RootPage == 0 && equalFoldName(r.Name, name) && equalFoldName(r.TblName, tbl.name)) {
			chosen = r
			break
		}
	}
	if chosen == nil {
		// An automatic index (sqlite_schema.sql NULL) is not reproduced.
		return false, false
	}
	if chosen.SQL == "" {
		// A segment table's PRIMARY KEY or UNIQUE index (autoIndexSeekCandidate):
		// the planner's key is its columns, then the rowid, and each probe's
		// rows are sorted into it.
		n := len(key.cols) - 1
		return n >= 1 && len(key.colls) == n+1 && len(key.desc) == n+1 &&
			key.cols[n] == -1 && equalFoldName(key.colls[n], "BINARY") && !key.desc[n], true
	}
	lead, ok := leadingIndexColumn(chosen.SQL)
	if !ok {
		return false, false
	}
	for i := range rows {
		r := &rows[i]
		if r.Type != "index" || r == chosen || r.SQL == "" ||
			r.Temp != chosen.Temp || !equalFoldName(r.TblName, chosen.TblName) {
			continue
		}
		if other, lok := leadingIndexColumn(r.SQL); !lok || equalFoldName(other, lead) {
			return false, false // a rival index leads with the same column
		}
	}
	stmt, perr := parseCreateIndexStmt(chosen.SQL)
	if perr != nil || stmt.exprOrPartial || len(stmt.cols) == 0 {
		return false, false
	}
	n := len(stmt.cols)
	if len(key.cols) != n+1 || len(key.colls) != n+1 || len(key.desc) != n+1 {
		return false, false
	}
	// A segment seek (root 0) answers from the column's equality index, which
	// hands each key's rows over in rowid order: the index's own order only
	// for one column. A longer key's rows are sorted into it (sorted).
	sorted = root == 0 && n != 1
	for j, name := range stmt.cols {
		ci := -1
		for k := range tbl.cols {
			if equalFoldName(tbl.cols[k].Name, name) {
				ci = k
				break
			}
		}
		if ci < 0 || key.cols[j] != ci {
			return false, false
		}
		coll := effectiveCollation(tbl.cols[ci].Collation)
		if j < len(stmt.collate) && stmt.collate[j] != "" {
			coll = strings.ToUpper(stmt.collate[j])
		}
		if !equalFoldName(key.colls[j], coll) {
			return false, false
		}
		if key.desc[j] != (j < len(stmt.desc) && stmt.desc[j]) {
			return false, false
		}
	}
	// The trailing rowid every index record over a rowid table carries.
	return key.cols[n] == -1 && equalFoldName(key.colls[n], "BINARY") && !key.desc[n], sorted
}

// orCommonConjuncts adds to eqs, for each OR among them, the conjuncts every
// one of its disjuncts carries: a row the OR passes passes each of those, so
// one can key a seek as a top-level conjunct can. "(a.k=b.k AND ...) OR
// (a.k=b.k AND ...)" seeks on a.k=b.k.
func orCommonConjuncts(eqs []Expr) []Expr {
	out := eqs
	for _, cj := range eqs {
		if be, ok := cj.(BinaryExpr); !ok || be.Op != "OR" {
			continue
		}
		ds := splitTopLevelOr(cj)
		common := splitTopLevelAnd(ds[0])
		for _, d := range ds[1:] {
			conj := splitTopLevelAnd(d)
			var kept []Expr
			for _, c := range common {
				if slices.ContainsFunc(conj, func(e Expr) bool { return exprEqual(c, e) }) {
					kept = append(kept, c)
				}
			}
			common = kept
		}
		out = append(out[:len(out):len(out)], common...)
	}
	return out
}

// splitTopLevelOr is splitTopLevelAnd for OR.
func splitTopLevelOr(e Expr) []Expr {
	if b, ok := e.(BinaryExpr); ok && b.Op == "OR" {
		return append(splitTopLevelOr(b.L), splitTopLevelOr(b.R)...)
	}
	return []Expr{e}
}

// plainUsingRefs rewrites a USING/NATURAL condition's representative column
// (ColumnExpr.UsingRepr) as the plain qualified column it is when no RIGHT or
// FULL operand before the condition's own item can coalesce it, so it can key
// a seek. A pinned one is left as it is. The loop still tests the condition as
// written.
func plainUsingRefs(srcs []joinSource, eqs []Expr) []Expr {
	plain := func(e Expr) (Expr, bool) {
		ce, ok := e.(ColumnExpr)
		if !ok || !ce.UsingRepr || ce.UsingPinned || ce.Qualifier == "" || ce.UsingReprOwnItem > len(srcs) {
			return e, false
		}
		for k := 0; k < ce.UsingReprOwnItem; k++ {
			if srcs[k].rightOuter {
				return e, false
			}
		}
		ce.UsingRepr, ce.UsingReprOwnItem = false, 0
		return ce, true
	}
	var out []Expr
	for i, cj := range eqs {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") {
			continue
		}
		l, lok := plain(be.L)
		r, rok := plain(be.R)
		if !lok && !rok {
			continue
		}
		if out == nil {
			out = slices.Clone(eqs)
		}
		be.L, be.R = l, r
		out[i] = be
	}
	if out == nil {
		return eqs
	}
	return out
}

// findJoinSortedSeek looks for an equality pinning a bare column of s to a key
// over the outer cursors, for OpSortedSeek: the rows whose column equals the
// key under the comparison's collation, once the key takes the comparison's
// affinity, must include every row the equality passes, which
// seekAffinitySafe guarantees.
//
// Every further equality on another column joins the key (sortedSeekHint.more):
// a run of rows equal on one column of few values, a year say, is most of the
// table for every probe.
func findJoinSortedSeek(c *compiler, s joinSource, outer map[int]bool, eqs []Expr) (Expr, []Expr, *sortedSeekHint) {
	return findJoinSortedSeekBy(c, s, func(e Expr) bool { return keyRefsOnlyCursors(c, e, outer) }, eqs)
}

// findJoinSortedSeekBy is findJoinSortedSeek with keyOK deciding which keys
// are bound before the seek.
func findJoinSortedSeekBy(c *compiler, s joinSource, keyOK func(Expr) bool, eqs []Expr) (Expr, []Expr, *sortedSeekHint) {
	var key Expr
	var more []Expr
	var h *sortedSeekHint
	for _, cj := range eqs {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") {
			continue
		}
		for _, side := range [2][2]Expr{{be.L, be.R}, {be.R, be.L}} {
			col, ok := scopeColumnIndex(c, s, side[0])
			if !ok || col < 0 || col >= len(s.tbl.cols) || !keyOK(side[1]) {
				continue
			}
			aff := comparisonAffinity(c.affCtx(), side[0], side[1])
			if !seekAffinitySafe(aff, s.tbl.cols[col].Aff) {
				continue
			}
			coll := effectiveCollation(resolveCompareCollation(c.affCtx(), be.L, be.R))
			switch {
			case h == nil:
				key, h = side[1], &sortedSeekHint{col: col, coll: coll, aff: aff, built: s.rightOuter}
			case col != h.col && !slices.ContainsFunc(h.more, func(m sortedSeekCol) bool { return m.col == col }):
				more = append(more, side[1])
				h.more = append(h.more, sortedSeekCol{col: col, coll: coll, aff: aff})
			}
			break
		}
	}
	return key, more, h
}

// findJoinRowidSeek looks for an equality pinning s's rowid (or INTEGER PRIMARY
// KEY alias) to a key over the outer cursors -- an inner rowid point lookup. It
// needs no affinity/collation check of its own: OpSeekRowidHint gives the key
// numeric affinity, an integer K matches exactly the one inner row rowid==K,
// and any other key matches none.
func findJoinRowidSeek(c *compiler, s joinSource, outer map[int]bool, eqs []Expr) (Expr, bool) {
	for _, cj := range eqs {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") {
			continue
		}
		if isScopeRowidRef(c, s, be.L) && keyRefsOnlyCursors(c, be.R, outer) {
			return be.R, true
		}
		if isScopeRowidRef(c, s, be.R) && keyRefsOnlyCursors(c, be.L, outer) {
			return be.L, true
		}
	}
	return nil, false
}

// findJoinIndexSeek looks for an equality pinning a bare indexed column of s to
// a key over the outer cursors. secondaryIndexSeekCandidates supplies indexes
// already safe to seek (non-DESC leading column, index collation equal to the
// column's); joinIndexPlanFor adds the comparison's collation and affinity
// checks a correlated key needs.
//
// sp is the pager s's cursor opens on (an attached reader for a
// cross-database source), since a root page is file-local.
//
// It takes the first candidate whose leading column the equality pins. All
// select the same rows, but SQLite walks the cheapest (whereLoopAddBtreeIndex:
// szIdxRow, and whether the index covers the query's columns), which decides
// visit order. Without colUsed this cannot answer covering, so when two
// indexes lead with the same column the order is not reproduced
// (joinSeekReproducesKey).
func findJoinIndexSeek(c *compiler, sp *ReadOnlyPager, s joinSource, outer map[int]bool, eqs []Expr, want *autoIndexKey) (*indexSeekPlan, bool) {
	if s.notIndexed {
		return nil, false // NOT INDEXED takes every secondary index away (where.c:4070)
	}
	cands, err := sp.secondaryIndexSeekCandidates(s.scope.tableName, s.tbl.root, s.tbl.cols)
	if err != nil {
		return nil, false
	}
	if want != nil && want.root == 0 {
		if cd, ok := sp.autoIndexSeekCandidate(s.scope.tableName, s.tbl.root, s.tbl.cols, want.index); ok {
			cands = append(cands, cd)
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	if want != nil {
		// The ported planner already decided WHICH index this level walks, so
		// the "first candidate wins" rule below is replaced by its verdict: by
		// root, or by name for a segment table's (root 0) index.
		kept := cands[:0]
		for _, cd := range cands {
			if want.root != 0 && cd.root == want.root || want.root == 0 && equalFoldName(cd.name, want.index) {
				kept = append(kept, cd)
			}
		}
		cands = kept
		if len(cands) == 0 {
			return nil, false
		}
	}
	for _, cj := range eqs {
		be, ok := cj.(BinaryExpr)
		if !ok || (be.Op != "=" && be.Op != "==") {
			continue
		}
		if colIdx, ok := scopeColumnIndex(c, s, be.L); ok && keyRefsOnlyCursors(c, be.R, outer) {
			if p, ok := joinIndexPlanFor(c, s, cands, colIdx, be, be.L, be.R); ok {
				return p, true
			}
		}
		if colIdx, ok := scopeColumnIndex(c, s, be.R); ok && keyRefsOnlyCursors(c, be.L, outer) {
			if p, ok := joinIndexPlanFor(c, s, cands, colIdx, be, be.R, be.L); ok {
				return p, true
			}
		}
	}
	return nil, false
}

// joinIndexPlanFor builds the seek plan when a candidate index leads with
// column colIdx, checking the two conditions a correlated key needs:
//
//   - the actual comparison's collation (left-then-right from be.L/be.R) must
//     equal the index's leading collation, so ordering and equality agree;
//   - seekAffinitySafe(comparisonAffinity, columnAffinity): applying the
//     comparison's affinity to the stored values must not change outcomes.
//
// A single-table literal key satisfies both automatically, which is why
// detectIndexSeekKey omits them.
func joinIndexPlanFor(c *compiler, s joinSource, cands []indexSeekCandidate, colIdx int, be BinaryExpr, colExpr, keyExpr Expr) (*indexSeekPlan, bool) {
	for _, cd := range cands {
		if cd.leadingCol != colIdx {
			continue
		}
		coll := effectiveCollation(resolveCompareCollation(c.affCtx(), be.L, be.R))
		if !equalFoldName(coll, cd.coll) {
			return nil, false
		}
		aff := comparisonAffinity(c.affCtx(), colExpr, keyExpr)
		if !seekAffinitySafe(aff, s.tbl.cols[colIdx].Aff) {
			return nil, false
		}
		return &indexSeekPlan{root: cd.root, keyExpr: keyExpr, aff: aff, coll: cd.coll, leadingCol: cd.leadingCol, index: cd.name}, true
	}
	return nil, false
}

// seekAffinitySafe reports whether coercing the inner column's stored values
// with comparison affinity aff is a no-op given the column's affinity colAff,
// so probing raw stored values with an aff-coerced key reproduces the
// comparison:
//
//   - aff == affNone: nothing is coerced -- safe.
//   - aff numeric: safe only for a numeric colAff; a TEXT/NONE column can hold
//     "5", which the comparison folds to 5 but is stored as text.
//   - aff == affText: safe only for colAff == affText; a numeric column's
//     values would be stringified by the comparison but stored raw.
func seekAffinitySafe(aff, colAff affinity) bool {
	switch {
	case aff == affNone:
		return true
	case isNumericAffinity(aff):
		return isNumericAffinity(colAff)
	case aff == affText:
		return colAff == affText
	default:
		return false
	}
}

// keyRefsOnlyCursors reports whether e is a safe correlated seek key: every
// column it references resolves (via the compiler's own resolveInScopes, so the
// rules match the rest of the compile) to a cursor in the allowed set (the
// tables bound strictly before this join level), and e is built only from a
// conservative whitelist of column-free-safe expression nodes. Any subquery,
// aggregate, OR/IN/MATCH/row expression, USING-representative, pinned or
// schema-qualified column, unresolved/ambiguous column, or column resolving to
// a not-yet-bound cursor makes it decline -- the safe direction (full scan).
func keyRefsOnlyCursors(c *compiler, e Expr, allowed map[int]bool) bool {
	return exprRefsOnly(e, func(x ColumnExpr) bool {
		cursor, _, _, found, fb, hard := resolveInScopes(c.scopes, x, c.pager)
		return hard == nil && found && !fb.has && allowed[cursor]
	})
}

// exprRefsOnly is keyRefsOnlyCursors' whitelist walk with the column test
// supplied: col decides each ColumnExpr that is neither a USING representative
// nor schema-qualified.
func exprRefsOnly(e Expr, col func(ColumnExpr) bool) bool {
	switch x := e.(type) {
	case nil:
		return false
	case LiteralExpr, ParamExpr:
		return true
	case ColumnExpr:
		// A pinned name counts FROM items, which the narrowed scope a seek key
		// compiles under does not.
		if x.UsingRepr || x.UsingPinned || x.Schema != "" {
			return false
		}
		return col(x)
	case UnaryExpr:
		return exprRefsOnly(x.X, col)
	case BinaryExpr:
		return exprRefsOnly(x.L, col) && exprRefsOnly(x.R, col)
	case CastExpr:
		return exprRefsOnly(x.X, col)
	case CollateExpr:
		return exprRefsOnly(x.X, col)
	case IsNullExpr:
		return exprRefsOnly(x.X, col)
	case BetweenExpr:
		return exprRefsOnly(x.X, col) && exprRefsOnly(x.Lo, col) && exprRefsOnly(x.Hi, col)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if !exprRefsOnly(a, col) {
				return false
			}
		}
		return true
	case CaseExpr:
		if x.Base != nil && !exprRefsOnly(x.Base, col) {
			return false
		}
		for _, w := range x.Whens {
			if !exprRefsOnly(w.When, col) || !exprRefsOnly(w.Then, col) {
				return false
			}
		}
		if x.Else != nil {
			return exprRefsOnly(x.Else, col)
		}
		return true
	default:
		// Anything not explicitly whitelisted (subquery, EXISTS, IN, MATCH, row
		// expression, ...) is conservatively declined.
		return false
	}
}

// emitJoinSeekHint, called by emitJoinLevel right before an INNER level's
// OpRewind (every outer cursor positioned), evaluates that source's correlated
// seek key and emits the OpSeekRowidHint/OpSeekIndexHint that reconfigures the
// cursor to re-seek on its next rewind (P3==1). A no-op unless annotateJoinSeeks
// recorded a plan. The key is compiled under a scope NARROWED to the tables
// bound strictly before this level, so a key column can only ever resolve to an
// already-positioned outer cursor (annotateJoinSeeks already verified this;
// narrowing makes it structural, mirroring emitJoinLevel's own ON narrowing).
func (c *compiler) emitJoinSeekHint(srcs []joinSource, order []int, level int, s joinSource) error {
	if s.joinSeekKeyExpr == nil && s.joinIdxSeek == nil && s.joinSortedSeek == nil {
		return nil
	}
	saved := c.scopes
	var visible []compileScope
	for i := 0; i < level; i++ {
		// sourceScopes, not .scope directly: a preceding materialized
		// parenthesized join GROUP contributes every one of its own leaf
		// members here, not just its connector's -- see emitJoinLevel's
		// identical ON-narrowing comment.
		visible = append(visible, sourceScopes(srcs[order[i]])...)
	}
	c.scopes = visible
	defer func() { c.scopes = saved }()

	cur := s.scope.cursor
	if s.joinSeekKeyExpr != nil {
		keyReg, err := c.compileExpr(s.joinSeekKeyExpr)
		if err != nil {
			return err
		}
		c.emit(Instruction{Op: OpSeekRowidHint, P1: cur, P2: keyReg, P3: 1})
		return nil
	}
	if s.joinSortedSeek != nil {
		start := len(c.insns)
		keyReg, err := c.compileExpr(s.joinSortedKey)
		if err != nil {
			return err
		}
		h := *s.joinSortedSeek
		h.more = slices.Clone(h.more)
		for i, k := range s.joinSortedMore {
			reg, err := c.compileExpr(k)
			if err != nil {
				return err
			}
			h.more[i].reg = reg
		}
		h.keyLen = len(c.insns) - start
		c.emit(Instruction{Op: OpSortedSeek, P1: cur, P2: keyReg, P3: 1, P4: &h})
		return nil
	}
	keyReg, err := c.compileExpr(s.joinIdxSeek.keyExpr)
	if err != nil {
		return err
	}
	c.emit(Instruction{Op: OpSeekIndexHint, P1: cur, P2: keyReg, P3: 1,
		P4: &indexSeekHint{root: s.joinIdxSeek.root, aff: s.joinIdxSeek.aff, coll: s.joinIdxSeek.coll, leadingCol: s.joinIdxSeek.leadingCol, index: s.joinIdxSeek.index, order: s.joinIdxSeek.order}})
	return nil
}
