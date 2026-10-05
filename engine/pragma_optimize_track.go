// This file is PRAGMA optimize's session tracking: a syntactic detector for C's
// TF_MaybeReanalyze (sqliteInt.h:2497) -- schema shape and WHERE-term shape,
// never a stat1 value -- plus the cached stat1 baseline the growth check
// (pragma.c condition 5b) compares against. execOptimize is the only reader.
//
//   - whereLoopAddBtreeIndex (where.c:3220-3312): a WHERE term matching an
//     index column at the current key position (opMask at 3261,
//     "WO_EQ|WO_IN|WO_GT|WO_GE|WO_LT|WO_LE|WO_ISNULL|WO_IS") sets bldFlags1
//     to SQLITE_BLDF1_UNIQUE for a UNIQUE index's last key column, else
//     SQLITE_BLDF1_INDEXED (3308-3311, whereInt.h:437-438).
//   - whereLoopAddBtree (where.c:4108-4142) does that for every index of the
//     table, once per FROM table per statement, whatever plan wins.
//   - where.c:4293-4302 resets bldFlags1 per index and ORs TF_MaybeReanalyze
//     only on exact equality with SQLITE_BLDF1_INDEXED. A single-column UNIQUE
//     index always takes the UNIQUE branch, so unique indexes are excluded
//     wholesale (maybeReanalyzeIndexCandidates); the rarer multi-column prefix
//     case only costs a missed accept, never a wrong answer.
package engine

import (
	"slices"
	"strconv"
	"strings"
)

// maybeReanalyzeIndexCandidates returns t's indexes that could satisfy
// where.c:4296's exact "bldFlags1==SQLITE_BLDF1_INDEXED" test: non-unique
// (IsUniqueIndex, where.c:3308, excludes automatic and explicit UNIQUE),
// plain-column (not exprOrPartial) indexes with a key. It reads db.indexes
// directly, as computeStat1Rows does, so it needs no compiler or pager.
func (db *DB) maybeReanalyzeIndexCandidates(t *tableMeta) []*indexMeta {
	var out []*indexMeta
	for _, idx := range db.indexes {
		if idx.isTemp != t.isTemp || !equalFoldName(idx.table, t.name) {
			continue
		}
		if idx.unique || idx.exprOrPartial || idx.isTablePK {
			continue
		}
		if len(idx.colIdx) == 0 {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// columnMatchesLeading reports whether e is a bare reference to fromTable's
// own column named colName (case-insensitive), under the single-table
// qualifier rule: unqualified, or qualified by that FROM item's alias (if it
// has one) or its own table name (if it does not). No scope-chain walk is
// needed -- markMaybeReanalyze's caller guarantees exactly one table is in
// play (see its own precondition checks) -- so this is a plain name
// comparison, not a resolveInScopes call.
func columnMatchesLeading(e Expr, f FromItem, colName string) bool {
	ce, ok := e.(ColumnExpr)
	if !ok || ce.UsingRepr || ce.Schema != "" || !equalFoldName(ce.Name, colName) {
		return false
	}
	if ce.Qualifier == "" {
		return true
	}
	want := f.Alias
	if want == "" {
		want = f.Table
	}
	return equalFoldName(ce.Qualifier, want)
}

// columnSideMatchesCandidate reports whether e is a bare reference to ANY of
// cands' own leading columns.
func columnSideMatchesCandidate(e Expr, f FromItem, cands []*indexMeta) bool {
	for _, idx := range cands {
		if columnMatchesLeading(e, f, idx.cols[0]) {
			return true
		}
	}
	return false
}

// columnSideMatchesNullableCandidate is columnSideMatchesCandidate narrowed for
// "IS NULL": indexColumnNotNull (where.c:613-626) and its guard (where.c:3291-
// 3294, "if( (eOp==WO_ISNULL || (pTerm->wtFlags&TERM_VNULL)!=0) &&
// indexColumnNotNull(...) ) continue") skip a NOT NULL column before
// bldFlags1 is set. t is the table every candidate belongs to.
//
// It does not apply to "col = NULL": that is WO_EQ with no TERM_VNULL
// (TERM_VNULL marks only the synthesized "x>NULL" of "x IS NOT NULL",
// whereexpr.c:1322-1346), so it matches like "col = 5" -- and C does
// reanalyze after "WHERE a = NULL" on a NOT NULL column.
func columnSideMatchesNullableCandidate(t *tableMeta, e Expr, f FromItem, cands []*indexMeta) bool {
	for _, idx := range cands {
		if !columnMatchesLeading(e, f, idx.cols[0]) {
			continue
		}
		j := idx.colIdx[0]
		if j >= 0 && j < len(t.cols) && t.cols[j].NotNull {
			continue // indexColumnNotNull(pProbe, saved_nEq) true -> "continue" in where.c, no match
		}
		return true
	}
	return false
}

// maybeReanalyzeConjunctMatches reports whether cj, one top-level AND conjunct
// of a single-table WHERE, is a term whereScanInit (opMask, where.c:3261) would
// hand whereLoopAddBtreeIndex for some candidate: =, ==, <, <=, >, >=, IS, IS
// NULL/ISNULL or IN (<list>) on the leading column against a column-free
// literal or parameter (isSeekKeyCandidate). That is narrower than C's
// "prereqRight & maskSelf" test only for a same-table column comparison
// ("WHERE a=b"), and reuses an audited helper. t is threaded for the IS NULL
// guard.
func maybeReanalyzeConjunctMatches(cj Expr, f FromItem, cands []*indexMeta, t *tableMeta) bool {
	switch x := cj.(type) {
	case BinaryExpr:
		switch x.Op {
		case "=", "==", "<", "<=", ">", ">=", "IS":
			// No NOT NULL guard here: a literal-NULL "=" (or "IS") RHS/LHS
			// compiles to WO_EQ/WO_IS, not WO_ISNULL, and indexColumnNotNull
			// is consulted ONLY for WO_ISNULL/TERM_VNULL (where.c:3291-3294)
			// -- see columnSideMatchesNullableCandidate's own doc comment
			// for the live-oracle transcript proving "col = NULL" against a
			// NOT NULL column still sets TF_MaybeReanalyze.
			if columnSideMatchesCandidate(x.L, f, cands) && isSeekKeyCandidate(x.R) {
				return true
			}
			if columnSideMatchesCandidate(x.R, f, cands) && isSeekKeyCandidate(x.L) {
				return true
			}
		}
	case IsNullExpr:
		// "col IS NULL" / "col ISNULL" -- WO_ISNULL. "IS NOT NULL"/"NOTNULL"
		// (x.Not true) is NOT in where.c:3261's opMask and is excluded. A
		// NOT NULL column can never take the WO_ISNULL branch either
		// (indexColumnNotNull, where.c:613-626 and its call at 3291-3294) --
		// columnSideMatchesNullableCandidate excludes exactly that.
		if !x.Not && columnSideMatchesNullableCandidate(t, x.X, f, cands) {
			return true
		}
	case InExpr:
		// "col IN (<value-list>)" -- WO_IN. The subquery form (x.Sub) and
		// "NOT IN" are excluded -- WO_IN never covers NOT IN, and a subquery
		// RHS is a different shape this narrow detector does not need for
		// the statement this pass closes.
		if !x.Not && x.Sub == nil && columnSideMatchesCandidate(x.X, f, cands) {
			return true
		}
	}
	return false
}

// markMaybeReanalyze is PRAGMA optimize's tracking hook, called once per
// top-level SELECT from QueryArgs after parsing -- before the plan cache's fast
// path, so a statement repeated verbatim is tracked on its first run only.
// Every omission here (a repeat, a qualified FROM item, a table-valued
// function, a view, a temp-shadowed name) can only cause a false negative,
// which execOptimize's fallback declines on rather than assuming clean.
// sawJoinOrSubquery is reserved for the separate, permanently conservative
// case (where.c:6605-6644).
//
// Only a single plain-table FROM (no derived table, join group or CTE) is
// recognized. UPDATE/DELETE WHEREs are not tracked (C scores them the same);
// that only costs a missed accept.
func (db *DB) markMaybeReanalyze(stmt *SelectStmt) {
	if db == nil || stmt == nil {
		return
	}
	if len(stmt.CTEs) != 0 || len(stmt.From) != 1 || stmt.From[0].Subquery != nil || stmt.From[0].GroupLen != 0 {
		// 2+ FROM sources, a derived table, a parenthesized join group, or
		// any WITH clause on the statement -- see *DB.sawJoinOrSubquery's
		// doc comment (writer.go) for why this is the ONLY thing that sets
		// it, and why it is permanent for the rest of the session.
		db.sawJoinOrSubquery = true
		return
	}
	if db.sawJoinOrSubquery {
		return // already permanently conservative; nothing left to learn
	}
	f := stmt.From[0]
	if f.TableFunc || f.Schema != "" || stmt.Where == nil {
		// A table-valued function has no sqlite_stat1 row ever, and a
		// schema-qualified reference is outside this narrow detector's
		// scope -- neither is "wider than one table", so nothing is marked
		// AT ALL (see this function's own doc comment for why that is safe
		// and deliberately distinct from the sawJoinOrSubquery branch above).
		return
	}
	if db.findTableMetaIn(scopeTemp, f.Table) != nil {
		// C SQLite resolves an unqualified name against TEMP first
		// (query.go's resolveTableIn doc comment) -- a same-named TEMP table
		// shadows this reference entirely, so it says nothing about any MAIN
		// table's stat1.
		return
	}
	t := db.findTableMetaIn(scopeMain, f.Table)
	if t == nil {
		return // a view, a virtual table, or a name execSelect will itself reject
	}
	cands := db.maybeReanalyzeIndexCandidates(t)
	if len(cands) == 0 {
		return
	}
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		if maybeReanalyzeConjunctMatches(cj, f, cands, t) {
			db.markMaybeReanalyzeName(t.name)
			return
		}
	}
}

// markMaybeReanalyzeName records table name (MAIN schema) as
// maybe-reanalyzed for the rest of this session. Folded through
// r33sFoldIdent so "T1"/"t1" collide on lookup exactly as sqlite_-prefix
// checks elsewhere in this package already do.
func (db *DB) markMaybeReanalyzeName(name string) {
	if db.maybeReanalyze == nil {
		db.maybeReanalyze = map[string]bool{}
	}
	db.maybeReanalyze[r33sFoldIdent(name)] = true
}

// maybeReanalyzed reports whether name (MAIN schema) was recorded by
// markMaybeReanalyzeName at any point this session.
func (db *DB) maybeReanalyzed(name string) bool {
	return db.maybeReanalyze[r33sFoldIdent(name)]
}

// refreshStat1Baseline re-reads sqlite_stat1's current content into
// db.stat1Baseline for every table, as sqlite3AnalysisLoad reloads the whole
// database's stats (analyze.c:1942). Called from storeStat1Scoped, through
// which every ANALYZE form and PRAGMA optimize's re-ANALYZE write. Only table
// row counts matter for condition 5b; see stat1DiskRowCounts.
func (db *DB) refreshStat1Baseline() {
	db.stat1Baseline = db.stat1DiskRowCounts()
}

// stat1DiskRowCounts computes, per table in sqlite_stat1, the row count
// analysisLoader (analyze.c:1602-1647) loads into Table.nRowLogEst: the leading
// number of the last non-partial-index or idx-NULL row. analysisLoader
// overwrites per qualifying row with no ORDER BY, so the last wins, and rowid
// order reproduces its scan. refreshStat1Baseline and stat1DiskRowCount
// (execOptimize's uncached fallback) share it so they agree.
func (db *DB) stat1DiskRowCounts() map[string]int64 {
	out := map[string]int64{}
	stat1 := db.findTableMetaIn(scopeMain, "sqlite_stat1")
	if stat1 == nil {
		return out
	}
	rowids := make([]uint64, 0, stat1.rows.len())
	for rid := range stat1.rows.all() {
		rowids = append(rowids, rid)
	}
	slices.Sort(rowids)
	for _, rid := range rowids {
		row := stat1.rows.row(rid)
		if len(row) != 3 || row[0].Typ != Text || row[2].Typ != Text {
			continue // hand-written garbage this cache cannot use
		}
		if row[1].Typ != Text {
			// idx IS NULL: the table-level row, analysisLoader's else branch
			// (analyze.c:1641-1648) -- always updates nRowLogEst.
		} else {
			// A named index's row updates nRowLogEst only when it is NOT a
			// partial index (analyze.c:1633's "if( pIndex->pPartIdxWhere==0
			// )"). Resolve the CURRENT schema's own idea of that index --
			// exactly like analysisLoader's sqlite3FindIndex lookup -- rather
			// than trusting a stale name from before some intervening
			// DROP/CREATE; an index this session no longer recognizes cannot
			// update anything (C SQLite's own sqlite3FindIndex returning
			// NULL takes the same "argv==0" early-return, analyze.c:1603).
			ix := db.findIndexMetaIn(scopeMain, string(row[1].S))
			if ix == nil {
				if !equalFoldName(string(row[0].S), string(row[1].S)) {
					continue
				}
				// tbl==idx names a WITHOUT ROWID table's own PRIMARY KEY row
				// (analyze.c:1611's sqlite3PrimaryKeyIndex special case,
				// mirrored by computeStat1Rows' isTablePK naming) -- always
				// non-partial.
			} else if ix.exprOrPartial && ix.where != nil {
				continue
			}
		}
		n := parseLeadingStatCount(string(row[2].S))
		if n < 0 {
			continue
		}
		out[r33sFoldIdent(string(row[0].S))] = n
	}
	return out
}

// stat1DiskRowCount is stat1DiskRowCounts narrowed to one table -- see
// execOptimize's own doc comment for why this exists and when it is used
// (the *DB.stat1Baseline session cache has nothing for this table).
func (db *DB) stat1DiskRowCount(name string) (int64, bool) {
	n, ok := db.stat1DiskRowCounts()[r33sFoldIdent(name)]
	return n, ok
}

// parseLeadingStatCount reads the leading space-separated integer out of a
// sqlite_stat1 "stat" column's text (e.g. "1001 1" -> 1001) -- the row-count
// field every stat1 row carries first, whether it is an index's own row or
// the idx-NULL table-level row (computeStat1Rows, analyze_write.go). Returns
// -1 for anything that does not parse as a non-negative integer (hand-
// written garbage), which refreshStat1Baseline treats as unusable.
func parseLeadingStatCount(stat string) int64 {
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		return -1
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// stat1BaselineRowCount returns table name's row count as of this
// connection's own cached view of sqlite_stat1 (see *DB.stat1Baseline's doc
// comment, writer.go), and whether one is cached at all -- false exactly
// when no ANALYZE covering this table has run yet during this session,
// which execOptimize treats as "cannot compute the growth check" rather
// than guessing.
func (db *DB) stat1BaselineRowCount(name string) (int64, bool) {
	n, ok := db.stat1Baseline[r33sFoldIdent(name)]
	return n, ok
}

// tableMissingStat1Index reports pragma.c's condition 4b/5a for t
// (pragma.c:2485-2511): does some index of t lack a sqlite_stat1 row? This is
// current-content shape: C recomputes Index.hasStat1 at every schema load
// (analyze.c:1942), so a hand write cannot leave it stale. It reuses
// computeStat1Rows for which rows ANALYZE would write and under which names.
// An empty table contributes no rows, so this reports false for it, and
// execOptimize handles empty tables itself (pragma.c:2606-2610).
func (db *DB) tableMissingStat1Index(t *tableMeta) bool {
	stat1 := db.findTableMetaIn(scopeMain, "sqlite_stat1")
	have := map[string]bool{}
	if stat1 != nil {
		for _, row := range stat1.rows.all() {
			if len(row) != 3 || row[0].Typ != Text {
				continue
			}
			if !equalFoldName(string(row[0].S), t.name) {
				continue
			}
			who := "\x00"
			if row[1].Typ == Text {
				who = r33sFoldIdent(string(row[1].S))
			}
			have[who] = true
		}
	}
	for _, r := range db.computeStat1Rows() {
		if !equalFoldName(r.tbl, t.name) {
			continue
		}
		who := "\x00"
		if r.idx.Typ == Text {
			who = r33sFoldIdent(string(r.idx.S))
		}
		if !have[who] {
			return true
		}
	}
	return false
}

// stat1GrowthOutsideWindow reproduces vdbe.c:6296-6323's OP_IfSizeBetween
// EXACTLY: "jump [skip the ANALYZE] if X is between P3 and P4, inclusive",
// where X is 10*log2(currentRows) (or -1 if currentRows is zero -- the
// opcode's own "-Infinity encoding" for an empty table) and P3/P4 are
// baselineRows' own LogEst +-33 (pragma.c:2599's "const LogEst iRange = 33"
// -- 10x in LogEst units), P3 clamped to -1 rather than going negative for a
// small baseline (pragma.c:2600's "szThreshold>=iRange ? szThreshold-iRange
// : -1"). Returns true -- reanalyze -- exactly when X falls OUTSIDE
// [P3,P4].
func stat1GrowthOutsideWindow(baselineRows, currentRows int64) bool {
	const iRange logEst = 33
	threshold := logEstFromInt(uint64(baselineRows))
	lo := logEst(-1)
	if threshold >= iRange {
		lo = threshold - iRange
	}
	hi := threshold + iRange
	sz := logEst(-1)
	if currentRows > 0 {
		sz = logEstFromInt(uint64(currentRows))
	}
	return sz < lo || sz > hi
}
