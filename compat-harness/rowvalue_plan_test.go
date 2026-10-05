package compat

import (
	"fmt"
	"testing"
)

// TestRowValuePlanMatchesC tests that row-value comparisons generate plans
// matching C SQLite's optimizer.
func TestRowValuePlanMatchesC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)",
		"CREATE INDEX tbd ON t(b, d)", "CREATE INDEX tcd ON t(c, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
	}
	wheres := []string{
		"(c, d) > (30, 2)", "(c, d) >= (10, 3)", "(b, d) < (2, 4)", "(b, d) = (2, 4)",
		"(c, d) = (30, 4) OR a = 5", "(b, d) > (3, 1) AND c > 5", "(d, c) <= (2, 20)",
		"(b, d) IS (1, 1)", "(a, b) > (3, 1)", "(b, c) BETWEEN (1, 5) AND (3, 30)",
		"(b, d) IN ((1, 1), (2, 2))",
		// More of the same arms: a range bounded both sides, a vector range
		// alongside a scalar equality on its column, a vector equality mixed
		// with scalar terms, NOT, a three-wide vector, a one-wide one.
		"(c, d) > (5, 1) AND (c, d) < (30, 2)", "b = 2 AND (d, c) > (3, 10)", "(b, d) = (2, 4) AND c > 3",
		"(b, d) <> (2, 4) AND c > 30", "NOT ((c, d) > (30, 2))", "(b, d, c) > (2, 3, 10)", "(c) > (30)",
		"(b, d) IN ((1, 1), (2, 2), (3, 3)) AND c > 10", "(b, d) NOT IN ((1, 1), (2, 2)) AND c > 30",
		"(b, d) IN ((1, 1))", "(b, c) NOT BETWEEN (1, 5) AND (3, 30) AND c > 20",
		"(c, d) < (10, 2) OR (b, d) = (1, 1)", "(b, d) IS NOT (1, 1) AND c < 10", "(d, b) == (3, 3)",
		"(c + 0, d) > (30, 2)", "(30, 2) < (c, d)", "(b, d) >= (4, 5) AND b < 5",
	}
	outs := []string{
		"SELECT group_concat(a) FROM t WHERE %s",
		"SELECT a FROM t WHERE %s LIMIT 4",
		"SELECT a FROM t WHERE %s ORDER BY b LIMIT 4",
	}
	for _, analyze := range []bool{false, true} {
		seed := append([]string{}, base...)
		if analyze {
			seed = append(seed, "ANALYZE")
		}
		for i, w := range wheres {
			for j, out := range outs {
				differ(t, fmt.Sprintf("rowvalue analyze=%v %d/%d %s", analyze, i, j, w),
					append(append([]string{}, seed...), fmt.Sprintf(out, w)))
			}
		}
	}
}

// TestRowValuePlanTypedMatchesC: the same arms over declared affinities and
// collations, where a vector's comparison is its FIRST elements' (affinity and
// collation both, expr.c:74, 274), a COLLATE on a later element still picks
// the side that answers (sqlite3BinaryCompareCollSeq, expr.c:424), a COLLATE'd
// element indexes nothing (exprMightBeIndexed, whereexpr.c:1080, 1211), and a
// VALUES row whose field compares differently from the last row's keeps that
// field's IN loop out of the plan.
func TestRowValuePlanTypedMatchesC(t *testing.T) {
	base := []string{
		"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER, k TEXT)",
		"CREATE INDEX wsn ON w(s, n)", "CREATE INDEX wns ON w(n, s)", "CREATE INDEX wk ON w(k)", "CREATE INDEX wkn ON w(k COLLATE nocase, n)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<40) INSERT INTO w SELECT i, " +
			"CASE WHEN i % 2 THEN char(97 + i % 5) ELSE char(65 + i % 5) END, i % 7, CAST(i % 9 AS TEXT) || CASE WHEN i % 3 THEN 'x' ELSE 'X' END FROM r",
	}
	wheres := []string{
		"(s, n) > ('b', 3)", "(n, s) >= (3, 'B')", "(s COLLATE binary, n) > ('b', 3)", "(s, n COLLATE binary) > ('b', 3)",
		"(s, n) = ('b', 3)", "(s, n) IN (('b', 3), ('C', 4))", "(n, s) IN (('3', 'b'), (4, 'c'))",
		"(k, n) > ('5x', 2)", "(n, k) = ('3', '3x')", "(k, n) < ('3X' COLLATE nocase, 2)",
		"(s, n) IN (('b', 3), ('c' COLLATE binary, 4))",
		"(n, s) IN ((3, 'b'), (CAST('4' AS TEXT), 'c'))", "(n, s) IN ((CAST('4' AS TEXT), 'c'), (3, 'b'))",
		"(n, s) BETWEEN (2, 'a') AND (5, 'z')", "(n, s) > (2, 'a' COLLATE binary)", "(a + 0, s) > (30, 'b')",
		"(k COLLATE nocase, n) >= ('4X', 3)", "(n, k) IN ((1, '1x'), (2, '2X'), (4, '4x'))",
	}
	outs := []string{
		"SELECT group_concat(a) FROM w WHERE %s",
		"SELECT a FROM w WHERE %s LIMIT 3",
		"SELECT a FROM w WHERE %s ORDER BY n LIMIT 4",
	}
	for _, analyze := range []bool{false, true} {
		seed := append([]string{}, base...)
		if analyze {
			seed = append(seed, "ANALYZE")
		}
		for i, w := range wheres {
			for j, out := range outs {
				differ(t, fmt.Sprintf("typed analyze=%v %d/%d %s", analyze, i, j, w),
					append(append([]string{}, seed...), fmt.Sprintf(out, w)))
			}
		}
	}
}

// TestRowValuePlanBookkeepingMatchesC: the row-value details that move a cost
// by a unit or a dependency by a table -- so each shows only where two plans
// were close. A fixed column inside a vector is a constant to its term's usage
// (propagateConstants walks into a TK_VECTOR), but a vector equality defines
// no constant; a COLLATE on a later element of the vector picks which side's
// first element answers the collation; a WO_AND disjunct's slices all count
// towards its nOut (pWC->nBase); and a sliced original is TERM_VIRTUAL, out of
// every OR sub-scan's pAndExpr.
func TestRowValuePlanBookkeepingMatchesC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)",
		"CREATE INDEX tbd ON t(b, d)", "CREATE INDEX tcd ON t(c, d)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
		"CREATE TABLE u(x, y, z)", "CREATE INDEX uxy ON u(x, y)", "CREATE INDEX uz ON u(z)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO u SELECT n % 6, n % 4, n FROM s",
		"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER)", "CREATE INDEX wsn ON w(s, n)", "CREATE INDEX wn ON w(n)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<40) INSERT INTO w SELECT i, " +
			"CASE WHEN i % 2 THEN char(97 + i % 5) ELSE char(65 + i % 5) END, i % 7 FROM r",
	}
	qs := []string{
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE u.x = 2 AND (t.c, t.d) > (u.x + 35, 1)",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE u.x = 2 AND (t.c, t.d) > (u.x + 35, 1)",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE u.x = 2 AND (t.b, t.d) = (u.x, 3)",
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE (t.b, t.d) = (2, 3) AND u.x = t.b",
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE (t.b, t.d) = (2, 3) AND u.y = t.b",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (u.x, u.y) = (2, 2) AND t.b = u.x",
		"SELECT group_concat(a) FROM w WHERE ('b', n COLLATE binary) < (s, 5)",
		"SELECT group_concat(a) FROM w WHERE ('b', n) < (s, 5)",
		"SELECT group_concat(a) FROM w WHERE ('B', n COLLATE binary) >= (s, 2) AND n > 3",
		"SELECT group_concat(a) FROM t WHERE (b, a) = (1, 6) OR c = 5",
		"SELECT group_concat(a) FROM t WHERE (d, a) = (6, 6) OR c < 4",
		"SELECT group_concat(a) FROM t WHERE (b, a) = (1, 6) OR (d, a) = (2, 2)",
		"SELECT group_concat(a) FROM t WHERE (b, a, c) = (1, 6, 35) OR d = 3",
		"SELECT group_concat(a) FROM t WHERE (c < 3 OR d = 6) AND (a, b) = (5, 0)",
		"SELECT group_concat(a) FROM t WHERE (c < 2 OR c > 39) AND (a, d) = (40, 5)",
		"SELECT group_concat(a) FROM t WHERE (c < 5 OR d = 1) AND (b, a) = (1, 36)",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (t.b, t.a) = (u.x, 12) AND u.z < 20",
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE (t.b, t.a) = (u.x, u.z) AND u.y = 1",
	}
	for _, analyze := range []bool{false, true} {
		seed := append([]string{}, base...)
		if analyze {
			seed = append(seed, "ANALYZE")
		}
		for i, q := range qs {
			differ(t, fmt.Sprintf("bookkeeping analyze=%v %d %s", analyze, i, q), append(append([]string{}, seed...), q))
		}
	}
}

// TestRowValueInMixedCollationMatchesC: C's IN loop on one field of "(s, n) IN
// (VALUES ...)" seeks with every row's value under the LAST row's collation
// (sqlite3CodeRhsOfIN's KeyInfo, expr.c:3752). This used to decline, the
// desugared rows each comparing under their own; the parser now gives every
// row that one collation (rowInKeyCollations), so the WHERE and the loop agree
// and the plan is compared.
func TestRowValueInMixedCollationMatchesC(t *testing.T) {
	differ(t, "row-in mixed collation", []string{
		"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER)", "CREATE INDEX wsn ON w(s, n)",
		"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<40) INSERT INTO w SELECT i, " +
			"CASE WHEN i % 2 THEN char(97 + i % 5) ELSE char(65 + i % 5) END, i % 7 FROM r",
		"SELECT group_concat(a) FROM w WHERE (s, n) IN (('b' COLLATE binary, 3), ('c', 4))",
	})
}

// TestRowValuePlanShapesMatchC: the row-value arms in the positions that
// exercise their bookkeeping -- a vector IN's fields sharing one nIn and never
// both matching an ORDER BY term (where.c:3340, 5339), slices of a
// column-to-column equality with their commuted copies, a vector range whose
// first element is on the right, an OR of row values (WHERE_MULTI_OR, whose
// AND disjuncts slice), constant propagation that must not read a vector
// equality, a LEFT JOIN a row value must not reduce, and the statements that
// plan through the same planner: a nested scan, an UPDATE, a DELETE.
func TestRowValuePlanShapesMatchC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)", "CREATE INDEX td ON t(d)",
		"CREATE INDEX tbd ON t(b, d)", "CREATE INDEX tcd ON t(c, d)", "CREATE INDEX tdb ON t(d DESC, b)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<40) INSERT INTO t SELECT n, n % 5, 41 - n, n % 7 FROM s",
		"CREATE TABLE u(x, y, z)", "CREATE INDEX uxy ON u(x, y)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO u SELECT n % 6, n % 4, n FROM s",
	}
	single := []string{
		"(b, d) IN ((1, 1), (2, 2), (4, 4)) ORDER BY b",
		"(b, d) IN ((1, 1), (2, 2), (4, 4)) ORDER BY b, d",
		"(b, d) IN ((1, 1), (2, 2), (4, 4)) ORDER BY d",
		"(d, b) IN ((1, 1), (2, 2), (4, 4)) ORDER BY d DESC",
		"(b, d) IN ((1, 1), (2, 2)) AND c IN (1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)",
		"(b, d) IN ((1, 1), (2, 2)) AND c IN (1, 2, 3)",
		"(b, b) IN ((1, 1), (2, 2))",
		"(b, a) IN ((1, 1), (2, 12), (3, 3))",
		"(b, d) IN ((1, 1), (2, 2)) AND b = 2",
		"(b COLLATE nocase, d) IN ((1, 1), (2, 2))",
		"(b, d) IN ((1, CAST('1' AS TEXT)), (2, 2))",
		"(b, d) IN ((1, 1), (d, 2))",
		"b = 2 AND (b, d) > (2, 3)", "b = 2 AND (d, c) = (3, 25)", "(b, d) = (2, 3) AND c = b + 30",
		"(b, d) = (2, 3) ORDER BY c", "(b, d) IS (2, 3) ORDER BY d", "(d, b) = (3, 2) ORDER BY a DESC",
		"(c, d) > (30, 2) ORDER BY c DESC", "(d, b) > (5, 3) ORDER BY d", "(d, b) < (2, 1) ORDER BY b",
		"(c, d) >= (10, 3) AND (c, d) <= (20, 5) ORDER BY d",
		"likely((c, d) > (30, 2))", "unlikely((b, d) = (2, 3)) AND c > 10", "likelihood((b, d) IN ((1, 1)), 0.9) AND c > 5",
		"(c, d) > (30, 2) OR (b, d) = (1, 1)", "(c, d) > (30, 2) OR b = 3", "(b, d) IN ((1, 1), (2, 2)) OR c = 5",
		"((c, d) > (35, 0) OR (d, c) < (1, 10)) AND b < 4",
		"(b, c) BETWEEN (1, 5) AND (3, 30) ORDER BY c", "(c, d) BETWEEN (10, 0) AND (20, 9) ORDER BY b",
		"(c, d COLLATE nocase) > (30, 2)", "(c COLLATE nocase, d) > (30, 2)", "(c, d) > (30 COLLATE nocase, 2)",
		"(b, d) = (2, 3) OR (b, d) = (4, 4)", "NOT ((b, d) = (2, 3)) AND c < 10", "(b, d) <> (2, 3) OR c = 5",
		"(c + 0, d) > (30, 2) AND b = 1", "(a, b) < (5, 0) ORDER BY b",
		// A one-row list is still "IN (VALUES ...)", priced at 46 per loop.
		"(b, d) IN ((1, 1)) AND c > 36", "(b, d) IN ((1, 1)) AND c BETWEEN 30 AND 33", "(d, b) IN ((3, 3)) ORDER BY c LIMIT 2",
		"(c, d) BETWEEN (10, 0) AND (20, 9) AND b = 2", "(b, d) BETWEEN (1, 0) AND (1, 9) AND c > 30",
	}
	outs := []string{
		"SELECT group_concat(a) FROM (SELECT a FROM t WHERE %s)",
		"SELECT a FROM t WHERE %s LIMIT 3",
	}
	joins := []string{
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (u.x, u.y) = (t.b, t.d) AND t.c > 30",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (t.b, t.d) = (u.x, u.y) AND u.z < 5",
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE (t.b, t.d) > (u.x, u.y) AND u.z = 7",
		"SELECT group_concat(t.a || ':' || u.z) FROM u, t WHERE (u.x, u.y) < (t.b, t.d) AND u.z = 7",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (t.b, u.x) = (1, t.d) AND u.z > 25",
		"SELECT group_concat(t.a || ':' || u.z) FROM t, u WHERE (u.x, u.y) IN ((t.b, 1), (t.d, 2)) AND t.c > 35",
		"SELECT group_concat(t.a || ':' || coalesce(u.z, '-')) FROM t LEFT JOIN u ON u.x = t.b WHERE (u.y, u.z) > (1, 20) AND t.c > 30",
		"SELECT group_concat(t.a || ':' || coalesce(u.z, '-')) FROM t LEFT JOIN u ON u.x = t.b WHERE (u.y, u.z) = (1, 21)",
		"SELECT group_concat(t.a || ':' || coalesce(u.z, '-')) FROM t LEFT JOIN u ON u.x = t.b WHERE (u.y, u.z) IN ((1, 21), (2, 26))",
		"SELECT group_concat(t.a || ':' || coalesce(u.z, '-')) FROM t LEFT JOIN u ON (u.x, u.y) = (t.b, t.d) WHERE t.c > 30",
		"SELECT group_concat(x.a || ':' || (SELECT a FROM t WHERE (c, d) > (x.a, 2) LIMIT 1)) FROM t x WHERE x.a < 8",
		"SELECT group_concat(x.a || ':' || (SELECT a FROM t WHERE (b, d) = (x.b, x.d) AND a <> x.a LIMIT 1)) FROM t x WHERE x.a < 12",
		"SELECT b, group_concat(a) FROM t WHERE (b, d) IN ((1, 1), (2, 2), (3, 3)) GROUP BY b",
		"SELECT d, group_concat(a) FROM t WHERE (c, d) > (20, 3) GROUP BY d",
		"SELECT DISTINCT b FROM t WHERE (c, d) > (20, 3)",
	}
	writes := []string{
		"UPDATE t SET a = a + 100 WHERE (c, d) > (30, 2) RETURNING a",
		"UPDATE t SET a = a + 100 WHERE (b, d) IN ((1, 1), (2, 2)) RETURNING a",
		"DELETE FROM t WHERE (b, d) = (2, 2) RETURNING a",
		"DELETE FROM t WHERE (b, c) BETWEEN (1, 5) AND (3, 30) RETURNING a",
	}
	for _, analyze := range []bool{false, true} {
		seed := append([]string{}, base...)
		if analyze {
			seed = append(seed, "ANALYZE")
		}
		for i, w := range single {
			for j, out := range outs {
				differ(t, fmt.Sprintf("shape analyze=%v %d/%d %s", analyze, i, j, w),
					append(append([]string{}, seed...), fmt.Sprintf(out, w)))
			}
		}
		for i, q := range append(append([]string{}, joins...), writes...) {
			differ(t, fmt.Sprintf("stmt analyze=%v %d %s", analyze, i, q), append(append([]string{}, seed...), q))
		}
	}
}
