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

import "strings"

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
	if joinSeekDisabled || c == nil || c.pager == nil || len(srcs) < 2 {
		return
	}
	order := plan.execOrder
	for level := 1; level < len(order); level++ {
		origIdx := order[level]
		s := srcs[origIdx]
		// INNER and LEFT; decline RIGHT/FULL, derived tables, and WITHOUT ROWID
		// tables (see this file's package doc comment).
		if s.rightOuter || s.derived != nil || s.tbl == nil || s.tbl.withoutRowid {
			continue
		}
		// A level the planner gave an index-order key has a decided visit
		// order. OpAutoIndexOrder declines a re-seeking cursor (it
		// re-materializes a different row set per rewind), so the seek is kept
		// only where its order provably agrees (joinSeekReproducesKey).
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
		// buckets empty from the first LEFT join on; this enforces the
		// invariant the seek depends on.
		var eqs []Expr
		eqs = append(eqs, splitTopLevelAnd(s.on)...)
		if !s.left && level < len(plan.buckets) {
			eqs = append(eqs, plan.buckets[level]...)
		}
		if planKey == nil {
			// A ROWID point lookup delivers its (at most one) row per outer row
			// in the table's own order, which is what an sPk loop produces --
			// and an sPk loop carries no key at all. A non-nil key therefore
			// means the planner chose to WALK an index here, so the rowid seek
			// would deliver a different order and is not taken.
			if key, ok := findJoinRowidSeek(c, s, outer, eqs); ok {
				srcs[origIdx].joinSeekKeyExpr = key // strictly cheaper than an index seek
				continue
			}
		}
		// An index root page only means something in its own file. A
		// cross-database item's cursor opens on the attached reader
		// (OpOpenRead's P3 = dbIdx), so its index must be looked up there too;
		// using the compiling pager's root reads an unrelated b-tree in the
		// attached file -- a wrong answer
		// (TestR30CrossDatabaseSeekReadsTheRightFile). A dbIdx naming no
		// attached reader declines.
		sp := c.pager
		if s.dbIdx != 0 {
			if s.dbIdx-1 >= len(c.pager.attachedReaders) {
				continue
			}
			sp = c.pager.attachedReaders[s.dbIdx-1].pager
		}
		var wantRoot uint32
		if planKey != nil {
			wantRoot = planKey.root
			if wantRoot == 0 {
				continue // a transient automatic index, or a REVERSED rowid scan
			}
		}
		p, ok := findJoinIndexSeek(c, sp, s, outer, eqs, wantRoot)
		if !ok {
			continue
		}
		if planKey != nil && !joinSeekReproducesKey(sp, s.tbl, p.root, planKey) {
			continue
		}
		srcs[origIdx].joinIdxSeek = p
	}
}

// joinSeekReproducesKey reports whether this level's correlated index seek will
// visit rows in exactly the order key names -- the order the ported planner
// decided. OpAutoIndexOrder declines a re-seeking cursor, so the seek's own
// order must agree with the planner's key; where it cannot be shown to, the
// caller drops the seek and the planner's key wins. It re-derives the index's
// key from its schema text (no automatic index, no rival index with the same
// leading column) and compares it with key column for column.
func joinSeekReproducesKey(sp *ReadOnlyPager, tbl *resolvedTable, root uint32, key *autoIndexKey) bool {
	if sp == nil || tbl == nil || key == nil || root == 0 || key.root != root {
		return false
	}
	rows, err := sp.Schema()
	if err != nil {
		return false
	}
	var chosen *SchemaRow
	for i := range rows {
		if rows[i].Type == "index" && rows[i].RootPage == root && rows[i].SQL != "" {
			chosen = &rows[i]
			break
		}
	}
	if chosen == nil {
		// An automatic index (sqlite_schema.sql NULL) is not reproduced.
		return false
	}
	lead, ok := leadingIndexColumn(chosen.SQL)
	if !ok {
		return false
	}
	for i := range rows {
		r := &rows[i]
		if r.Type != "index" || r.RootPage == chosen.RootPage || r.SQL == "" ||
			r.Temp != chosen.Temp || !equalFoldName(r.TblName, chosen.TblName) {
			continue
		}
		if other, lok := leadingIndexColumn(r.SQL); !lok || equalFoldName(other, lead) {
			return false // a rival index leads with the same column
		}
	}
	stmt, perr := parseCreateIndexStmt(chosen.SQL)
	if perr != nil || stmt.exprOrPartial || len(stmt.cols) == 0 {
		return false
	}
	n := len(stmt.cols)
	if len(key.cols) != n+1 || len(key.colls) != n+1 || len(key.desc) != n+1 {
		return false
	}
	for j, name := range stmt.cols {
		ci := -1
		for k := range tbl.cols {
			if equalFoldName(tbl.cols[k].Name, name) {
				ci = k
				break
			}
		}
		if ci < 0 || key.cols[j] != ci {
			return false
		}
		coll := effectiveCollation(tbl.cols[ci].Collation)
		if j < len(stmt.collate) && stmt.collate[j] != "" {
			coll = strings.ToUpper(stmt.collate[j])
		}
		if !equalFoldName(key.colls[j], coll) {
			return false
		}
		if key.desc[j] != (j < len(stmt.desc) && stmt.desc[j]) {
			return false
		}
	}
	// The trailing rowid every index record over a rowid table carries.
	return key.cols[n] == -1 && equalFoldName(key.colls[n], "BINARY") && !key.desc[n]
}

// findJoinRowidSeek looks for an equality pinning s's rowid (or INTEGER PRIMARY
// KEY alias) to a key over the outer cursors -- an inner rowid point lookup. The
// runtime OpSeekRowidHint gate (key must be a genuine Integer, else full scan)
// makes this superset-safe with no affinity/collation check of its own: an
// integer key K matches exactly the one inner row rowid==K, and any non-integer
// key this iteration falls back to a full inner scan.
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
func findJoinIndexSeek(c *compiler, sp *ReadOnlyPager, s joinSource, outer map[int]bool, eqs []Expr, wantRoot uint32) (*indexSeekPlan, bool) {
	if s.notIndexed {
		return nil, false // NOT INDEXED takes every secondary index away (where.c:4070)
	}
	cands, err := sp.secondaryIndexSeekCandidates(s.scope.tableName, s.tbl.root, s.tbl.cols)
	if err != nil || len(cands) == 0 {
		return nil, false
	}
	if wantRoot != 0 {
		// The ported planner already decided WHICH index this level walks, so
		// the "first candidate wins" rule below is replaced by its verdict.
		kept := cands[:0]
		for _, cd := range cands {
			if cd.root == wantRoot {
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
		return &indexSeekPlan{root: cd.root, keyExpr: keyExpr, aff: aff, coll: cd.coll, leadingCol: cd.leadingCol}, true
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
// aggregate, OR/IN/MATCH/row expression, USING-representative or
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
		if x.UsingRepr || x.Schema != "" {
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
	if s.joinSeekKeyExpr == nil && s.joinIdxSeek == nil {
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
	keyReg, err := c.compileExpr(s.joinIdxSeek.keyExpr)
	if err != nil {
		return err
	}
	c.emit(Instruction{Op: OpSeekIndexHint, P1: cur, P2: keyReg, P3: 1,
		P4: &indexSeekHint{root: s.joinIdxSeek.root, aff: s.joinIdxSeek.aff, coll: s.joinIdxSeek.coll, leadingCol: s.joinIdxSeek.leadingCol}})
	return nil
}
