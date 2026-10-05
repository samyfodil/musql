// Tests inner WITH clauses that shadow outer CTEs of the same name.
package compat

import "testing"

func TestCTEInnerWithShadowsOuter(t *testing.T) {
	differ(t, "inner WITH shadows an outer CTE of the same name", []string{
		// with1.test's own two, which differ only by the inner LIMIT.
		`WITH RECURSIVE t21(a,b) AS ( WITH t21(x) AS (VALUES(1)) SELECT x, x FROM t21 ORDER BY 1 ) SELECT * FROM t21 AS tA, t21 AS tB`,
		`WITH RECURSIVE t21(a,b) AS ( WITH t21(x) AS (VALUES(1)) SELECT x, x FROM t21 ORDER BY 1 LIMIT 5 ) SELECT * FROM t21 AS tA, t21 AS tB`,
		// Without RECURSIVE, and without the self-join, so the shadowing is the
		// only thing under test.
		`WITH q(a,b) AS ( WITH q(x) AS (VALUES(7)) SELECT x, x*2 FROM q ) SELECT * FROM q`,
		// The inner definition must be the one READ: give the two different
		// rows so a resolution to the outer one would show a different value.
		`WITH s(v) AS ( VALUES(100) ), r(v) AS ( WITH s(v) AS (VALUES(200)) SELECT v FROM s ) SELECT * FROM r`,
		`WITH s(v) AS ( VALUES(100) ), r(v) AS ( SELECT v FROM s ) SELECT * FROM r`,
		// Shadowing two levels deep.
		`WITH a(v) AS ( VALUES(1) ) SELECT (WITH a(v) AS (VALUES(2)) SELECT (WITH a(v) AS (VALUES(3)) SELECT v FROM a)) AS z`,
	})
	// A GENUINE cycle must still be rejected -- the guard is narrower now, not
	// gone.
	differ(t, "a genuine CTE cycle is still rejected", []string{
		`CREATE TABLE base(x)`,
		`INSERT INTO base VALUES(1)`,
		`WITH c(x) AS ( SELECT x FROM c ) SELECT * FROM c`,
		`WITH c1(x) AS ( SELECT x FROM c2 ), c2(x) AS ( SELECT x FROM c1 ) SELECT * FROM c1`,
		// ...and one where the self-reference hides one level down, inside the
		// shadowing CTE's own body rather than being shadowed by it.
		`WITH c(x) AS ( WITH d(y) AS (SELECT x FROM c) SELECT y FROM d ) SELECT * FROM c`,
		`SELECT * FROM base`,
	})
	// An ordinary CTE referenced TWICE in one query is not a cycle either --
	// two sequential expansions of the same binding, not a nested one.
	differ(t, "a CTE referenced twice is not a cycle", []string{
		`WITH c(x) AS ( VALUES(1),(2) ) SELECT c1.x, c2.x FROM c AS c1, c AS c2 ORDER BY 1, 2`,
		`WITH c(x) AS ( VALUES(3) ) SELECT (SELECT x FROM c) + (SELECT x FROM c) AS z`,
	})
}
