package compat

import (
	"fmt"
	"os"
	"testing"
)

// THE NAMES A FLATTENED SUBQUERY'S PARENT BINDS.
//
// SQLite resolves every name BEFORE flattenSubquery (select.c:4290) merges a
// subquery into its parent; this engine flattens the AST and resolves after
// (engine/flatten_projection.go). Every case here is one where the rewrite
// could change what a name binds to, what a result column is called, or which
// error is raised -- each compared against the oracle.
func TestDerivedFlattenNamesMatchC(t *testing.T) {
	base := []string{
		"CREATE TABLE f(x, y, z)", "CREATE INDEX fx ON f(x)", "CREATE INDEX fz ON f(z)",
		"WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<24) INSERT INTO f SELECT (i*7)%12, 'r'||i, i%4 FROM s",
		"CREATE TABLE g(a INTEGER PRIMARY KEY, b)", "INSERT INTO g VALUES(0,'g0'),(1,'g1'),(2,'g2'),(3,'g3')",
		"CREATE TABLE c(a TEXT COLLATE NOCASE, b INTEGER, n)", "CREATE INDEX ca ON c(a)", "CREATE INDEX cb ON c(b)",
		"INSERT INTO c VALUES('x',3,1),('X',1,2),('y',2,3),('Y','2',4),('b',10,5),('A',7,6)",
		"CREATE TABLE gt(a, b AS (a*2))", "CREATE INDEX gta ON gt(a)", "INSERT INTO gt(a) VALUES(5),(1),(3)",
		"CREATE TABLE ip(id INTEGER PRIMARY KEY, v)", "INSERT INTO ip VALUES(5,'e'),(1,'a'),(3,'c')",
		"CREATE VIEW v AS SELECT y, x AS q FROM f WHERE z <> 3",
		"CREATE TABLE t1(a TEXT)", "CREATE TABLE t2(b INTEGER)",
		"INSERT INTO t1 VALUES('1'),('2')", "INSERT INTO t2 VALUES(1),(2)",
	}
	for i, q := range []string{
		// Renamed columns, case, qualifiers.
		"SELECT q FROM (SELECT x AS q, y FROM f) WHERE q > 7",
		"SELECT Q, D.Y FROM (SELECT x AS q, y FROM f) AS d WHERE d.q > 7",
		"SELECT Y FROM (SELECT y, x FROM f) WHERE X > 7",
		"SELECT d.q FROM (SELECT x AS q FROM f) AS d WHERE d.nosuch > 7",
		"SELECT f.y FROM (SELECT y, x FROM f) WHERE x > 7",
		"SELECT sqlite_flat_1.y FROM (SELECT y, x FROM f) WHERE x > 7",
		// Stars.
		"SELECT * FROM (SELECT y, x AS q FROM f) WHERE q > 7",
		"SELECT d.*, g.b FROM (SELECT y, x FROM f) AS d, g WHERE a = x",
		"SELECT * FROM (SELECT * FROM f) WHERE x > 7",
		"SELECT * FROM (SELECT f.* FROM f) AS d WHERE d.x > 7",
		// A base column the body does not project, and names that are not columns.
		"SELECT z FROM (SELECT y, x FROM f) WHERE x > 7",
		"SELECT y FROM (SELECT y, x FROM f) WHERE z > 1",
		"SELECT y AS w FROM (SELECT y, x FROM f) WHERE w > 'r1' AND x > 3",
		"SELECT y AS z FROM (SELECT y, x FROM f) WHERE z > 'r1' AND x > 3",
		"SELECT y FROM (SELECT y, x FROM f) WHERE y > \"r1\" AND x > 3",
		"SELECT y FROM (SELECT y, x FROM f) WHERE y > \"z\" AND x > 3",
		// ORDER BY and GROUP BY resolution.
		"SELECT x AS y FROM (SELECT y, x FROM f) WHERE x > 3 ORDER BY y",
		"SELECT y, x FROM (SELECT y, x FROM f) WHERE x > 3 ORDER BY 2",
		"SELECT y AS x, x AS y FROM (SELECT y, x FROM f) WHERE x > 3 ORDER BY x, y",
		"SELECT x, count(*), group_concat(y) FROM (SELECT x, y FROM f) GROUP BY x",
		"SELECT x, count(*) FROM (SELECT x FROM f) GROUP BY x HAVING x > 3",
		"SELECT y FROM (SELECT y, x FROM f) WHERE x > 3 ORDER BY y COLLATE nocase DESC",
		"SELECT DISTINCT z FROM (SELECT z, x FROM f) WHERE x > 3",
		"SELECT y FROM (SELECT y, x FROM f) WHERE x > 3 LIMIT 2 OFFSET 1",
		// Collation and affinity through the substitution.
		"SELECT n FROM (SELECT a, n FROM c) WHERE a = 'X'",
		"SELECT n FROM (SELECT a AS k, n FROM c) WHERE k > 'x'",
		"SELECT n FROM (SELECT b, n FROM c) WHERE b = '2'",
		"SELECT n FROM (SELECT b, n FROM c) WHERE b > '2'",
		"SELECT group_concat(n) FROM (SELECT a, n FROM c) WHERE a < 'z'",
		"SELECT n, a FROM (SELECT a, n FROM c) WHERE a >= 'a' ORDER BY a",
		// Generated and rowid-alias columns.
		"SELECT b FROM (SELECT a, b FROM gt) WHERE a > 1",
		"SELECT v, id FROM (SELECT id, v FROM ip) WHERE id > 2",
		"SELECT * FROM (SELECT id, v FROM ip) WHERE id > 0",
		"SELECT rowid FROM (SELECT y FROM f)",
		// Correlation, ambiguity, subqueries.
		"SELECT a, (SELECT group_concat(y) FROM (SELECT y, x FROM f) WHERE x > a) FROM g",
		"SELECT a, (SELECT group_concat(y) FROM (SELECT y, x FROM f) WHERE x > g.a) FROM g",
		"SELECT y FROM (SELECT y, x FROM f) AS d, f WHERE d.x > 7",
		"SELECT d.y FROM (SELECT y FROM f) AS d, f WHERE f.x > 7",
		"SELECT y FROM (SELECT y, x FROM f) WHERE x IN (SELECT a FROM g)",
		"SELECT (SELECT 1), y FROM (SELECT y, x FROM f) WHERE x > 7",
		"SELECT y, b FROM (SELECT y, z FROM f), g WHERE a = z",
		"SELECT y, b FROM (SELECT y, z, x FROM f) JOIN g ON a = z AND x > 3",
		// Nested bodies, views, CTEs.
		"SELECT y FROM (SELECT * FROM (SELECT y, x FROM f WHERE x > 1) WHERE x < 9) WHERE x <> 5",
		"SELECT y, q FROM v WHERE q > 5",
		"SELECT * FROM v WHERE q BETWEEN 2 AND 8",
		"WITH w(p, r) AS (SELECT x, y FROM f) SELECT r FROM w WHERE p > 7",
		"WITH w AS (SELECT x, y FROM f) SELECT w.y, w2.y FROM w, w AS w2 WHERE w.x = w2.x AND w.x > 9",
		// Body ORDER BY: moved onto the parent, or dropped.
		"SELECT y FROM (SELECT y, x, z FROM f ORDER BY z) WHERE x > 4",
		"SELECT y FROM (SELECT y, x AS k, z FROM f ORDER BY k DESC) WHERE z > 1",
		"SELECT y FROM (SELECT y, x, z FROM f ORDER BY 3, 2) WHERE x > 4",
		"SELECT y FROM (SELECT y, x, z FROM f ORDER BY z) WHERE x > 4 ORDER BY x",
		"SELECT upper(y) FROM (SELECT y, x, z FROM f ORDER BY z) WHERE x > 4",
		"SELECT count(*), max(y) FROM (SELECT y, x, z FROM f ORDER BY z) WHERE x > 4",
		"SELECT y, b FROM (SELECT y, x, z FROM f ORDER BY z) AS d JOIN g ON a = d.z WHERE x > 4",
		// UNION ALL bodies.
		"SELECT q FROM (SELECT x AS q FROM f UNION ALL SELECT a FROM g) WHERE q > 2",
		"SELECT y FROM (SELECT y, x FROM f WHERE z = 1 UNION ALL SELECT y, x FROM f WHERE z = 2) WHERE x > 5",
		"SELECT y FROM (SELECT y, x FROM f UNION ALL SELECT b, a FROM g) WHERE x > 2 LIMIT 3",
		"SELECT * FROM (SELECT y, x FROM f UNION ALL SELECT y, z FROM f) WHERE x > 8",
		"SELECT count(*), group_concat(y) FROM (SELECT y, x FROM f UNION ALL SELECT y, z FROM f) WHERE x > 2",
		"SELECT DISTINCT y FROM (SELECT y, x FROM f UNION ALL SELECT y, z FROM f) WHERE x > 8",
		// Arms that disagree on a column's affinity are not flattened (17h).
		"SELECT a, typeof(a) FROM (SELECT a FROM t1 UNION ALL SELECT b FROM t2) WHERE a = 1",
		"SELECT a, typeof(a) FROM (SELECT a FROM t1 UNION ALL SELECT b FROM t2) WHERE a = '1'",
		"SELECT a, typeof(a) FROM (SELECT a FROM t1 UNION ALL SELECT b FROM t2) WHERE a > 1",
		"SELECT count(*) FROM (SELECT a FROM t1 UNION ALL SELECT b FROM t2) WHERE a = 1",
		// ...nor ones whose columns' collations differ: substExpr would wrap the
		// later arm's copy in an implicit COLLATE (collate5.test).
		"SELECT * FROM (SELECT a, n FROM c UNION ALL SELECT y, x FROM f) WHERE a = 'X'",
		"SELECT * FROM (SELECT y, x FROM f UNION ALL SELECT a, n FROM c) WHERE y = 'X'",
		"SELECT count(*) FROM (SELECT a, n FROM c UNION ALL SELECT y, x FROM f) WHERE a = 'X'",
		// DISTINCT bodies: filtered before the duplicates go.
		"SELECT y FROM (SELECT DISTINCT y, x FROM f) WHERE x > 7",
		"SELECT a, typeof(a) FROM (SELECT DISTINCT a FROM c) WHERE a GLOB 'x*'",
		"SELECT b, typeof(b) FROM (SELECT DISTINCT b FROM c) WHERE typeof(b) = 'text'",
		"SELECT y FROM (SELECT DISTINCT y, x FROM f) WHERE x > 7 AND abs(-1) = 1",
		"SELECT a FROM (SELECT DISTINCT a FROM c) WHERE a = 'x'",
		"SELECT a, n FROM (SELECT DISTINCT a, n FROM c) WHERE a > 'a' AND n > 1",
		"SELECT y, z FROM (SELECT DISTINCT y, x, z FROM f) WHERE x > 3 AND z = 1",
		"SELECT y FROM (SELECT DISTINCT y, x FROM f) WHERE x > 3 AND y LIKE 'r1%'",
		"SELECT y, b FROM (SELECT DISTINCT y, x, z FROM f) AS d JOIN g ON a = d.z WHERE x > 6",
		"SELECT y, b FROM g LEFT JOIN (SELECT DISTINCT y, x, z FROM f) AS d ON a = d.z AND x > 6",
		"SELECT y, b FROM g LEFT JOIN (SELECT y, x, z FROM f UNION ALL SELECT y, z, x FROM f) AS d ON a = d.z AND x > 6",
		"SELECT DISTINCT y, b FROM g LEFT JOIN (SELECT y, x, z FROM f) AS d ON a = d.z AND x > 6",
		"SELECT y, b FROM g LEFT JOIN (SELECT DISTINCT y, x, z FROM f) AS d ON a = d.z AND x > 6 WHERE b <> 'g2'",
		"SELECT a, (SELECT group_concat(y) FROM (SELECT DISTINCT y, x FROM f) WHERE x > g.a + 5) FROM g",
		"SELECT count(*), group_concat(y) FROM (SELECT y, x FROM f ORDER BY z) WHERE x BETWEEN 2 AND 6",
		"SELECT y FROM (SELECT y, x FROM f ORDER BY z) WHERE x IN (3, 5, 9) ORDER BY y",
	} {
		if only := os.Getenv("NAMES_ONLY"); only != "" && only != fmt.Sprint(i) {
			continue
		}
		differ(t, fmt.Sprintf("names %d", i), append(append([]string{}, base...), q))
		differ(t, fmt.Sprintf("names %d analyzed", i), append(append(append([]string{}, base...), "ANALYZE"), q))
	}
}

// A SEGMENT's equality posting list is keyed on a text value's raw bytes, so
// it answers BINARY equality only. Under a NOCASE or RTRIM index it would hand
// back a subset of the matching rows -- "WHERE a = 'X'" over 'x' and 'X' lost
// the 'x' row once a later CREATE TABLE rewrote the catalog and compacted the
// table (engine/vdbe_cursor.go, the segment seek arm). Flattening exposed it:
// the same filter through a derived table used to scan.
func TestSegmentSeekHonorsIndexCollation(t *testing.T) {
	for i, q := range []string{
		"SELECT n FROM c WHERE a = 'X'",
		"SELECT n FROM c WHERE a = 'x' ORDER BY n",
		"SELECT n FROM (SELECT a, n FROM c) WHERE a = 'X'",
		"SELECT v || '|' FROM r WHERE v = 'p'",
		"SELECT v || '|' FROM r WHERE v = 'p  '",
		"SELECT n FROM c WHERE b = 2",
	} {
		differ(t, fmt.Sprintf("segment seek collation %d", i), []string{
			"CREATE TABLE c(a TEXT COLLATE NOCASE, b INTEGER, n)", "CREATE INDEX ca ON c(a)", "CREATE INDEX cb ON c(b)",
			"INSERT INTO c VALUES('x',3,1),('X',1,2),('y',2,3),('Y','2',4)",
			"CREATE TABLE r(v TEXT COLLATE RTRIM)", "CREATE INDEX rv ON r(v)", "INSERT INTO r VALUES('p'),('p '),('q')",
			"CREATE TABLE later(z)",
			q,
		})
	}
}
