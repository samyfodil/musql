package engine

import "fmt"

// An "INDEXED BY <name>" hint narrows the probe chain to that one index.
// When the index is partial and the WHERE clause doesn't imply its condition,
// the table has no loop and raises "no query solution".

// errNoQuerySolution is the "no query solution" error.
func errNoQuerySolution() error { return semanticf("engine: no query solution") }

func declineIndexedByPartial() error {
	return fmt.Errorf("%w: INDEXED BY a partial index whose usability is not decided here", errVDBEUnsupported)
}

// indexedByNoSolution checks if INDEXED BY on a partial index will have a solution.
// partial[i] is the condition of the partial index item i's hint names.
func indexedByNoSolution(c *compiler, srcs []joinSource, partial []Expr, where Expr, fromBody bool) error {
	if c == nil || c.rowOuter != nil {
		return declineIndexedByPartial()
	}
	for i := range srcs {
		s := &srcs[i]
		// A RIGHT JOIN sets JT_LTORJ, which whereUsablePartialIndex answers
		// with 0 before reading a term (where.c:3710); a derived, virtual or
		// CTE source has no b-tree loop for this to reason about.
		if s.tbl == nil || s.derived != nil || s.vtabItem != nil || s.cteItem != nil || s.rightOuter {
			return declineIndexedByPartial()
		}
	}
	// The WhereClause sqlite3WhereBegin splits: the WHERE after select.c's
	// outer-join strength reduction and constant propagation (neither of which
	// an UPDATE or DELETE runs -- c.onePassPlan), with every INNER join's ON
	// folded in and every OUTER join's ON tagged with its item. The same
	// construction the planner gate uses (where_plan_gate.go).
	scopes := tableScopesOf(srcs)
	wherePlanBindOuter(c, scopes)
	jts := make([]joinedTable, len(srcs))
	for i := range srcs {
		jts[i] = joinedTable{tbl: srcs[i].tbl}
	}
	reduced := wherePlanOuterJoinsReduced(jts, scopes, srcs, where)
	items := make([]whereItem, len(srcs))
	for i := range srcs {
		left := srcs[i].left && !reduced[i]
		jts[i] = joinedTable{tbl: srcs[i].tbl, on: srcs[i].on, left: left, onToWhere: srcs[i].on != nil && !left}
		items[i].outer = left
	}
	if !c.onePassPlan {
		pw, pj, ok := wherePlanPropagateConstants(jts, scopes, where)
		if !ok {
			return declineIndexedByPartial()
		}
		where, jts = pw, pj
	}
	terms, ok := wherePlanTermsFrom(jts, scopes, where)
	if !ok {
		return declineIndexedByPartial()
	}
	b := &wherePlanIdxBuild{items: items, terms: terms}
	for i, w := range partial {
		if w == nil {
			continue
		}
		pw := wherePlanPartialWhere(srcs[i].tbl, srcs[i].scope.tableName, w)
		if pw == nil {
			return declineIndexedByPartial()
		}
		switch b.whereUsablePartialIndex(i, pw) {
		case triUnknown:
			return declineIndexedByPartial()
		case triNo:
			if fromBody {
				return declineIndexedByPartial()
			}
			return errNoQuerySolution()
		}
	}
	return nil
}

// selectIndexedByNoSolution is indexedByNoSolution for one SELECT core, run by
// compileScanAttempt once its FROM clause has resolved.
func selectIndexedByNoSolution(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) error {
	var partial []Expr
	for i := range stmt.From {
		it := stmt.From[i]
		if it.IndexedBy == "" {
			continue
		}
		if len(srcs) != len(stmt.From) {
			return declineIndexedByPartial()
		}
		rp := p.forDB(srcs[i].dbIdx)
		if rp == nil {
			return declineIndexedByPartial()
		}
		row, err := rp.hintedIndexRow(it.IndexedBy, it.Table, fromItemScope(it))
		if err != nil || row.SQL == "" {
			// Validated already (resolveJoinSources), and an automatic index
			// is never partial.
			continue
		}
		ix, perr := parseCreateIndexStmt(row.SQL)
		if perr != nil {
			return declineIndexedByPartial()
		}
		if ix.where == nil {
			continue
		}
		if partial == nil {
			partial = make([]Expr, len(stmt.From))
		}
		partial[i] = ix.where
	}
	if partial == nil || isSimpleCountShape(stmt) {
		return nil
	}
	return indexedByNoSolution(c, srcs, partial, stmt.Where, p.fromBodyDepth > 0)
}

// isSimpleCountShape is isSimpleCount (select.c:5441): "SELECT count(*) FROM
// <one ordinary table>" with no WHERE, GROUP BY or HAVING is coded as OP_Count
// (select.c:8770-8830) and never reaches sqlite3WhereBegin, so no hint can
// leave it without a query solution. count(x) is not SQLITE_FUNC_COUNT, and
// DISTINCT, FILTER and OVER each disqualify it (EP_Distinct|EP_WinFunc).
func isSimpleCountShape(stmt *SelectStmt) bool {
	if stmt.Where != nil || len(stmt.GroupBy) != 0 || stmt.Having != nil || len(stmt.Columns) != 1 || len(stmt.From) != 1 {
		return false
	}
	f, ok := stmt.Columns[0].Expr.(FuncExpr)
	return ok && equalFoldName(f.Name, "count") && (f.Star || len(f.Args) == 0) &&
		!f.Distinct && f.Filter == nil && f.Over == nil && len(f.OrderBy) == 0
}

// writeIndexedByNoSolution is indexedByNoSolution for an UPDATE's or DELETE's
// own scan of tbl (update.c and delete.c both code it with sqlite3WhereBegin),
// its WHERE planned as the one-pass loop updateOnePassOrder plans. The hinted
// index is read from this session's live catalog, as checkWriteIndexHint
// reads it. undecided is a statement C may or may not code with
// sqlite3WhereBegin at all, which a partial hint then declines.
func (db *DB) writeIndexedByNoSolution(tbl *tableMeta, schema, alias, indexedBy string, where Expr, trig *trigCompileCtx, undecided bool) error {
	if indexedBy == "" || tbl == nil {
		return nil
	}
	ix := db.findTableIndexMeta(tbl, indexedBy)
	if ix == nil || ix.where == nil {
		return nil
	}
	if undecided {
		return declineIndexedByPartial()
	}
	p, err := db.segmentReadPager()
	if err != nil || p == nil {
		return declineIndexedByPartial()
	}
	c := &compiler{pager: p, onePassPlan: true, trig: trig}
	srcs, err := resolveJoinSources(p, c, []FromItem{{Table: tbl.name, Schema: schema, Alias: alias}})
	if err != nil || len(srcs) != 1 || srcs[0].tbl == nil || !equalFoldName(srcs[0].tbl.name, tbl.name) {
		return declineIndexedByPartial()
	}
	return indexedByNoSolution(c, srcs, []Expr{ix.where}, where, false)
}

// enterFromBody marks a view, CTE or FROM-subquery body's compile for
// selectIndexedByNoSolution; the returned func ends it.
func (p *ReadOnlyPager) enterFromBody() func() {
	p.fromBodyDepth++
	return func() { p.fromBodyDepth-- }
}
