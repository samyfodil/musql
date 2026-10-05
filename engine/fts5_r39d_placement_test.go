package engine

import "testing"

// TestR39DFts5WalkMatchExprIsExhaustive verifies that fts5WalkMatchExpr
// recognizes every expression node kind, since unrecognized nodes would cause
// errors in every fts5 MATCH in the statement.
func TestR39DFts5WalkMatchExprIsExhaustive(t *testing.T) {
	stmt := mustParseSelect(t, `
		SELECT a, -b, a+1, a IS NULL, a IN (1,2), a BETWEEN 1 AND 2,
		       a LIKE 'x' ESCAPE '!', a GLOB 'y', a COLLATE NOCASE,
		       CAST(a AS INTEGER), CASE a WHEN 1 THEN 2 ELSE 3 END,
		       abs(a), count(*) OVER (PARTITION BY b ORDER BY a
		                              ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING),
		       (SELECT 1), EXISTS(SELECT 1), ?, NULL, t MATCH 'q'
		  FROM t
		 WHERE (a,b) IN ((1,2)) AND t MATCH 'r'
		 GROUP BY a HAVING sum(b) FILTER (WHERE a>0) > 0
		 ORDER BY a`)

	var matches, cols int
	count := func(e Expr) {
		if !fts5WalkMatchExpr(e, func(MatchExpr) { matches++ }, func(ColumnExpr) { cols++ }) {
			t.Errorf("fts5WalkMatchExpr does not recognise every node of %#v", e)
		}
	}
	for _, sc := range stmt.Columns {
		count(sc.Expr)
	}
	count(stmt.Where)
	for _, g := range stmt.GroupBy {
		count(g)
	}
	count(stmt.Having)
	for _, o := range stmt.OrderBy {
		count(o.Expr)
	}
	if matches != 2 {
		t.Errorf("MatchExpr nodes seen = %d, want 2 (the select list's and the WHERE's)", matches)
	}
	if cols == 0 {
		t.Error("no ColumnExpr reported: fts5ExprScopes would see an expression as reading nothing")
	}
}

// TestR39DFts5DepCycle verifies that cycles in fts5 query dependencies are
// detected.
func TestR39DFts5DepCycle(t *testing.T) {
	good := map[int]bool{0: true, 1: true, 2: true}
	cyclic := map[int]map[int]bool{0: {1: true}, 1: {0: true}}
	if !fts5DepCycle(0, cyclic, good) || !fts5DepCycle(1, cyclic, good) {
		t.Error("a 2-cycle must be reported for both of its scopes")
	}
	chain := map[int]map[int]bool{0: {1: true}, 1: {2: true}}
	for idx := range good {
		if fts5DepCycle(idx, chain, good) {
			t.Errorf("scope %d is on no cycle of an acyclic chain", idx)
		}
	}
	// An edge into an unbound scope does not form a cycle.
	if fts5DepCycle(0, cyclic, map[int]bool{0: true}) {
		t.Error("an edge into an unbound scope must not close a cycle")
	}
}
