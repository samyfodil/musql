// This file implements the FROM-clause form of UPDATE: "UPDATE t SET c =
// <expr> FROM <sources> [WHERE ...]", compiled by compileUpdateStmt through
// compileUpdateFromSource (vdbe_update_from.go).
//
// The target is joined with the FROM sources and filtered by WHERE, as if it
// were a comma-joined entry of a SELECT's FROM. A target row is updated once
// per join row; one with no match is unchanged. Pass one is a synthetic SELECT
// (buildUpdateFromSelect) yielding the target's key plus every SET value per
// join row; pass two applies them through the ordinary UPDATE machinery.
//
// Multi-match: when one target row matches several join rows, C uses one of
// them, and which is undefined (lang_update.html#upfrom). It follows the plan:
// updateFromSelect writes join rows into an ephemeral table keyed on the
// target (select.c:1355, SRT_Upfrom's OP_Insert), so the last join row
// visited wins, and the visit order is whatever the cost-based planner chose
// (it flips with ANALYZE and indexes). So such a target row is declined --
// unless every candidate computed the identical SET tuple, where the choice
// cannot matter (setValueTuplesEqual).
//
// A bare aggregate is a valid SET expression ("UPDATE t1 SET b=sum(a) FROM
// t0", upfrom1.test 3.1): updateFromSelect (update.c:265) puts the SET
// expressions straight into the synthetic SELECT, so it becomes a
// GROUP-BY-less aggregate over the whole join and updates exactly one target
// row, chosen by the bare-column anchor rule (magnetPlan, which declines when
// that choice is plan-dependent). Over t0(1,2,3) and three target rows, one
// row gets 18 (6*3). A zero-row aggregate yields a NULL key -- no target
// matched -- and is a no-op.
//
// A WITHOUT ROWID target projects its PRIMARY KEY tuple instead of a rowid,
// mapped back by identifyTargetRow (upfrom3.test, including SETs of PK
// columns; uniqueness is still enforced).
//
// A trigger body may use this form (triggerupfrom.test 1.0); the firing row is
// the synthetic SELECT's outer scope.
//
// An OR-clause or a column's declared ON CONFLICT uses the same conflict
// handling as a single-table UPDATE, because C shares it:
// sqlite3GenerateConstraintChecks (update.c:1031) is outside every
// nChangeFrom test, and update.c:984 passes onError to sqlite3CodeRowTrigger
// unconditionally.
//
// The view form ("UPDATE <view> ... FROM ...", INSTEAD OF UPDATE) fires the
// trigger once per join row and lives in vdbe_view_update_from.go.
package engine

import (
	"fmt"
)

// buildUpdateFromSelect builds pass one of "UPDATE t SET ... FROM ... [WHERE
// ...]": "SELECT <target key>, <SET expressions> FROM <sources>, <target>
// [WHERE ...]", returning it and how many leading columns are the key.
//
// This is updateFromSelect (update.c:187-273): the target's rowid (TK_ROW,
// update.c:250) or PRIMARY KEY columns (update.c:233-240), then every pChanges
// expression (update.c:258-262), over sqlite3SrcListDup(pTabList) with the
// statement's WHERE (update.c:222-223). compileViewUpdateFromSource builds the
// same shape by hand and must never build a different join.
func buildUpdateFromSelect(stmt *updateStmt, tbl *tableMeta) (*SelectStmt, int, error) {
	// An "AS alias" on the target renames it for this synthetic SELECT too --
	// the user's own WHERE/SET expressions were written against the alias
	// (see parseWriteTargetSuffix, write_update_delete.go), so the FROM item
	// carries it and this internal rowid reference is qualified by whichever
	// name is actually in scope.
	targetName := writeScopeName(stmt.alias, tbl)
	// The leading output column(s) identify the target ROW: its rowid for an
	// ordinary table, or -- a WITHOUT ROWID table has no rowid to project
	// (tableScope.noRowid, so "t.rowid" is "no such column" there exactly as in
	// C SQLite) -- its PRIMARY KEY tuple, which identifyTargetRow maps back
	// to the row store's own opaque key.
	keyCols := []Expr{ColumnExpr{Qualifier: targetName, Name: "rowid"}}
	if tbl.withoutRowid {
		keyCols = keyCols[:0]
		for _, ci := range tbl.pkIndex.colIdx {
			keyCols = append(keyCols, ColumnExpr{Qualifier: targetName, Name: tbl.cols[ci].Name})
		}
	}
	cols := make([]SelectColumn, 0, len(stmt.sets)+len(keyCols))
	for _, e := range keyCols {
		cols = append(cols, SelectColumn{Expr: e})
	}
	for _, a := range stmt.sets {
		cols = append(cols, SelectColumn{Expr: a.expr})
	}
	// The target is comma-joined after the FROM clause's join tree, not
	// before: C joins it with the FROM's result, so an outer join inside FROM
	// never null-extends the target and FROM's ON clauses cannot see it
	// (upfrom4.test 120/210):
	//
	//	UPDATE t5 SET b=y, c=v FROM m2 RIGHT JOIN m1 ON (x=u) WHERE x=a
	//	  -- m1's unmatched row still updates its t5 row, with v NULL
	//	UPDATE t5 SET b=y, c=v FROM m1 LEFT JOIN m2 ON (u=t5.a) WHERE x=a
	//	  -- "no such column: t5.a"
	from := make([]FromItem, 0, len(stmt.from)+1)
	from = append(from, stmt.from...)
	//
	// Schema is the statement's own qualifier, so "UPDATE main.t ... FROM m"
	// joins main's t even when a temp t exists. updateFromSelect dups the
	// SrcList (update.c:222) and clears only the target's resolved table and
	// cursor (update.c:228, 230), so its name and database survive
	// (expr.c:1913 copies zDatabase). Without it, pass one resolved the temp
	// t and pass two wrote that rowid into main's t.
	from = append(from, FromItem{Table: tbl.name, Schema: stmt.schema, Alias: stmt.alias, UpdateTarget: true})
	// A leading "WITH ..." belongs to the join, not just to the statement's own
	// subqueries: "WITH data(k,v) AS (VALUES(1,'ten')) UPDATE t1 SET b=v FROM
	// data WHERE a=k" names the CTE as a FROM SOURCE (upfrom2.test 1.x.1), so
	// the CTE definitions ride on the synthetic SELECT, which is where
	// execSelect pushes them (query.go).
	joinCTEs, cerr := writeJoinCTEs(stmt.ctes, tbl.name, stmt.table, stmt.sets, stmt.where)
	if cerr != nil {
		return nil, 0, cerr
	}
	return &SelectStmt{CTEs: joinCTEs, Columns: cols, From: from, Where: stmt.where}, len(keyCols), nil
}

// writeJoinCTEs returns the leading WITH definitions to put in scope for an
// "UPDATE ... FROM" join -- all except a CTE shadowing the target's name.
//
// C resolves the target to the real table even with a same-named CTE in
// scope (upfrom1.test 4.2), so letting it in would bind the target FROM item
// to the CTE. A subquery in SET/WHERE binds the other way (the CTE shadows,
// 4.1), and one FROM scope cannot serve both, so that combination declines.
func writeJoinCTEs(ctes []CTEDef, targetName, displayName string, sets []assignment, where Expr) ([]CTEDef, error) {
	if len(ctes) == 0 {
		return nil, nil
	}
	out := make([]CTEDef, 0, len(ctes))
	shadowed := false
	for _, c := range ctes {
		if equalFoldName(c.Name, targetName) {
			shadowed = true
			continue
		}
		out = append(out, c)
	}
	if shadowed {
		hasSub := containsSubquery(where)
		for _, a := range sets {
			hasSub = hasSub || containsSubquery(a.expr)
		}
		if hasSub {
			return nil, fmt.Errorf("engine: UPDATE %s: a WITH clause whose CTE shadows the target table, combined with a subquery in SET/WHERE, is not supported by this write path (the target resolves to the table but the subquery resolves to the CTE)", displayName)
		}
	}
	return out, nil
}

// setValueTuplesEqual reports whether two matches for the same target row
// would apply the identical SET tuple. It is storage-class-and-payload
// equality (valuesEqualExact), not SQL "=": TEXT '3' and INTEGER 3 differ,
// since affinity runs on whichever is kept. When all candidates are equal the
// plan-dependent choice (see the file comment) cannot change the result, so
// the multi-match decline is skipped.
func setValueTuplesEqual(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqualExact(a[i], b[i]) {
			return false
		}
	}
	return true
}

// identifyTargetRow turns the join SELECT's leading key column(s) back into the
// row store's key. For a rowid table it is the projected rowid. For WITHOUT
// ROWID it is the PRIMARY KEY tuple, found by scanning for the row holding
// exactly those values: they came out of this table, and the PK is unique
// under its collations, so binary equality (keysEqual) finds one row. A key
// matching no row is an error, never a skipped or different row.
func identifyTargetRow(tbl *tableMeta, key []Value) (uint64, error) {
	if !tbl.withoutRowid {
		if key[0].Typ != Int {
			// The rowid column of an ordinary rowid table is always an integer;
			// a non-integer here would mean a shape this path doesn't model, so
			// decline rather than guess.
			return 0, fmt.Errorf("unsupported UPDATE ... FROM shape (target rowid did not resolve to an integer)")
		}
		return uint64(key[0].I), nil
	}
	cand := make([]Value, len(key))
	for rowid, vals := range tbl.rows.all() {
		for i, ci := range tbl.pkIndex.colIdx {
			cand[i] = indexColumnValue(tbl, rowid, vals, ci)
		}
		if keysEqual(cand, key) {
			return rowid, nil
		}
	}
	return 0, fmt.Errorf("unsupported UPDATE ... FROM shape (the join produced a target PRIMARY KEY that matches no row of this WITHOUT ROWID table)")
}
