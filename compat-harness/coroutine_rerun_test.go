package compat

import (
	"fmt"
	"testing"
)

// A FROM-clause subquery C codes as a co-routine restarts wherever its loop
// begins (OP_InitCoroutine, wherecode.c:1550), so one its planner puts in an
// inner loop runs again for every outer row; one it flattens is copied into
// every reference (substExpr, select.c:3797); one it materializes runs once. A
// body calling random() makes each visible. Only counts and comparisons are
// printed, so every answer is the same on every run.

// coroutineRerunPre is the schema the sweeps below share: three tables of 3, 2
// and 4 rows.
var coroutineRerunPre = []string{
	"CREATE TABLE x(a)", "INSERT INTO x VALUES(1),(2),(3)",
	"CREATE TABLE y(b)", "INSERT INTO y VALUES(1),(2)",
	"CREATE TABLE z(c)", "INSERT INTO z VALUES(5),(6),(7),(8)",
}

// TestCoroutineRerunSweep crosses subquery bodies -- a co-routine C puts
// inside or outside, one it flattens, one it materializes -- with the FROM
// clauses around them, each body as a CTE, a NOT MATERIALIZED CTE and a
// derived table.
func TestCoroutineRerunSweep(t *testing.T) {
	bodies := []string{
		"SELECT random() AS r FROM (SELECT 1) GROUP BY 1",
		"SELECT random() AS r",
		"SELECT random() AS r FROM y",
		"SELECT random() AS r FROM y GROUP BY b",
		"SELECT random() AS r FROM y LIMIT 1",
		"SELECT random() AS r FROM y LIMIT 2",
		"SELECT DISTINCT random() AS r FROM y",
		"SELECT max(random()) AS r FROM y",
		"SELECT random() AS r UNION ALL SELECT random()",
		"SELECT random() AS r FROM y UNION ALL SELECT random() FROM y",
		"VALUES(random()),(random())",
		"SELECT random() AS r FROM y ORDER BY 1",
		"SELECT random() AS r FROM y WHERE b=1",
		"SELECT random()+b AS r FROM y GROUP BY b HAVING b>0",
		"SELECT random() AS r, row_number() OVER () FROM y",
	}
	outer := []string{
		"SELECT count(DISTINCT r) FROM x, %s",
		"SELECT count(DISTINCT r) FROM %s, x",
		"SELECT count(DISTINCT r) FROM x, y, %s",
		"SELECT count(DISTINCT r) FROM x, y, z, %s",
		"SELECT count(DISTINCT r) FROM x, %s, y",
		"SELECT count(DISTINCT r) FROM x LEFT JOIN %s",
		"SELECT count(DISTINCT r) FROM x LEFT JOIN %s ON 1",
		"SELECT count(DISTINCT r) FROM x CROSS JOIN %s",
		"SELECT count(DISTINCT r) FROM %s CROSS JOIN x",
		"SELECT count(DISTINCT r) FROM x JOIN %s ON r<>a",
		"SELECT count(DISTINCT r) FROM x, %s WHERE r = a",
		"SELECT count(DISTINCT r) FROM x, %s WHERE a > 1",
		"SELECT count(*) FROM (SELECT r FROM x, %s ORDER BY a)",
		"SELECT (SELECT count(DISTINCT r) FROM y, %s WHERE a>0) FROM x",
	}
	var cases [][]string
	for _, b := range bodies {
		for _, o := range outer {
			for _, q := range []string{
				"WITH t AS (" + b + ") " + fmt.Sprintf(o, "t"),
				"WITH t AS NOT MATERIALIZED (" + b + ") " + fmt.Sprintf(o, "t"),
				fmt.Sprintf(o, "("+b+") AS t"),
			} {
				cases = append(cases, append(append([]string(nil), coroutineRerunPre...), q))
			}
		}
	}
	sweepAll(t, cases)
}

// TestCoroutineRerunShapes are the shapes around the sweep: references a
// flattened body's copies keep apart, a collating sequence the copy brings to
// a comparison, an outer join's NULL row, a view joined to itself, a
// subquery's own ORDER BY satisfying the statement's, and a correlated body.
func TestCoroutineRerunShapes(t *testing.T) {
	pre := append(append([]string(nil), coroutineRerunPre...),
		"CREATE TABLE n(s TEXT COLLATE NOCASE)", "INSERT INTO n VALUES('A'),('b')",
		"CREATE TABLE w(k INTEGER PRIMARY KEY, v) WITHOUT ROWID", "INSERT INTO w VALUES(1,1),(2,2)",
		"CREATE TABLE ix(a, c)", "CREATE INDEX ixa ON ix(a)", "INSERT INTO ix VALUES(1,1),(2,2),(3,3)",
		"CREATE VIEW v AS SELECT random() AS r FROM (SELECT 1) GROUP BY 1",
		"CREATE VIEW vw AS WITH t AS NOT MATERIALIZED (SELECT random() AS r) SELECT a.r = b.r AS e, a.r AS ar, b.r AS br FROM t a, t b",
	)
	for i, q := range []string{
		"SELECT count(*) FROM x, (SELECT random() AS r FROM y) WHERE r = r",
		"SELECT count(*) FROM (SELECT random() AS r FROM y) WHERE r = r",
		"SELECT count(*) FROM (SELECT random() AS r FROM y) WHERE r <> r",
		"SELECT r = r FROM (SELECT random() AS r FROM y)",
		"SELECT count(DISTINCT r), count(DISTINCT r+0) FROM x, (SELECT random() AS r FROM y)",
		"SELECT count(*) FROM n, (SELECT hex(randomblob(1)) AS h FROM y) WHERE h = s",
		"SELECT count(*) FROM n, (SELECT hex(randomblob(1)) AS h FROM y) WHERE s = h",
		"SELECT count(*) FROM n, (SELECT 'a' || substr(random(),1,0) AS h FROM y) WHERE h = s",
		"SELECT count(*) FROM n, (SELECT 'a' || substr(random(),1,0) AS h FROM y) WHERE s = h",
		"SELECT count(*) FROM n, (SELECT 'a' || substr(random(),1,0) AS h FROM y) WHERE h = 'A'",
		"SELECT count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y WHERE b > 1) t",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, k FROM w) t ON t.k = x.a",
		"SELECT (SELECT count(DISTINCT r) FROM y, (SELECT random()+x.a AS r FROM (SELECT 1) GROUP BY 1) AS t) FROM x",
		"SELECT (SELECT count(DISTINCT r) FROM y, (SELECT random()+x.a AS r FROM (SELECT 1) GROUP BY 1) AS t) FROM x ORDER BY a",
		"SELECT count(DISTINCT r) FROM ix, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE ix.a = 2",
		"SELECT count(DISTINCT r) FROM ix, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE ix.a > 1",
		"SELECT count(DISTINCT r) FROM ix, (SELECT random() AS r, 1 AS one FROM (SELECT 1) GROUP BY 1) AS t WHERE ix.a = t.one",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t, y WHERE x.a = y.b",
		"SELECT count(DISTINCT r1), count(DISTINCT r2) FROM x, (SELECT random() AS r1 FROM (SELECT 1) GROUP BY 1) AS t1, (SELECT random() AS r2 FROM (SELECT 1) GROUP BY 1) AS t2",
		"SELECT a, count(DISTINCT r) FROM x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t GROUP BY a ORDER BY a",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1 ORDER BY 1) AS t",
		"SELECT count(DISTINCT r) FROM (SELECT a FROM x ORDER BY a LIMIT 2), (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y ORDER BY b LIMIT 1) AS t",
		"SELECT count(DISTINCT r) FROM x, (SELECT abs(random()) % 1000000 AS r FROM y GROUP BY b) AS t",
		"SELECT count(DISTINCT r) FROM x, (SELECT randomblob(8) AS r FROM y GROUP BY b) AS t",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y WHERE b = 1 GROUP BY b) AS t",
		"SELECT count(DISTINCT r) FROM x CROSS JOIN (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x NATURAL JOIN (SELECT random() AS r, 1 AS a FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x JOIN (SELECT random() AS r, 1 AS a FROM (SELECT 1) GROUP BY 1) AS t USING (a)",
		"SELECT count(DISTINCT r) FROM x, (SELECT r FROM (SELECT random() AS r FROM (SELECT 1) GROUP BY 1)) AS t",
		"SELECT count(DISTINCT r) FROM x, v",
		"SELECT count(DISTINCT r) FROM v, x",
		"SELECT count(DISTINCT v1.r), count(DISTINCT v2.r) FROM x, v v1, v v2",
		"SELECT count(*) FROM v v1, v v2 WHERE v1.r = v2.r",
		"SELECT count(*) FROM x, v v1, v v2 WHERE v1.r = v2.r",
		"SELECT v1.r = v2.r FROM v v1, v v2",
		"SELECT count(DISTINCT r) FROM x LEFT JOIN v",
		"SELECT e FROM x, vw",
		"SELECT count(DISTINCT ar), count(DISTINCT br) FROM x, vw",
		"SELECT count(*) FROM vw, x WHERE e",
	} {
		differ(t, fmt.Sprintf("shape-%d", i), append(append([]string(nil), pre...), q))
	}
	for i, q := range []string{
		"SELECT count(DISTINCT r) FROM x, t",
		"SELECT count(DISTINCT t1.r), count(DISTINCT t2.r) FROM x, t t1, t t2",
	} {
		for _, hint := range []string{"", "MATERIALIZED ", "NOT MATERIALIZED "} {
			for _, body := range []string{"SELECT random() AS r FROM y", "SELECT random() AS r FROM (SELECT 1) GROUP BY 1"} {
				differ(t, fmt.Sprintf("cte-%d", i), append(append([]string(nil), pre...), "WITH t AS "+hint+"("+body+") "+q))
			}
		}
	}
}

// TestCoroutineRerunDeclines holds the shapes this engine still declines
// rather than answers, each for a C behaviour not followed here (see the
// comment on each). A decline is allowed; any answer must be C's.
func TestCoroutineRerunDeclines(t *testing.T) {
	pre := append([]string(nil), coroutineRerunPre...)
	for i, q := range []string{
		// The outer join's WHERE decides OUTER JOIN STRENGTH REDUCTION
		// (tag-select-0220) before C flattens; not followed.
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE t.b IS NULL",
		// A table-valued function's xBestIndex cost is not in the ported planner.
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		// The flattener does not reach a parent holding a subquery
		// (r37cMayReferenceRowid cannot rule a rowid reference out).
		"WITH t AS (SELECT random() AS r FROM y) SELECT count(DISTINCT t1.r) FROM x, t t1 WHERE (SELECT count(*) FROM t) > 0",
		// group_concat's arrival order over a derived table is not proven.
		"SELECT count(DISTINCT r), group_concat(a) FROM x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		// A DISTINCT aggregate's GROUP BY over a join: its group emission order.
		"WITH t AS (SELECT random() AS r) SELECT count(DISTINCT r) FROM x, y, t GROUP BY a",
	} {
		differAllowingDeclines(t, fmt.Sprintf("decline-%d", i), append(append([]string(nil), pre...), q))
	}
}
