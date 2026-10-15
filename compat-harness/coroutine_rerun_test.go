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
		"CREATE VIEW v2 AS SELECT r FROM v",
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
		"SELECT count(DISTINCT r) FROM x, (SELECT r FROM v) AS t",
		"SELECT count(DISTINCT r) FROM x, v2",
		"WITH a AS (SELECT random() AS r FROM (SELECT 1) GROUP BY 1), b AS (SELECT r FROM a) SELECT count(DISTINCT r) FROM x, b",
		"WITH a AS (SELECT random() AS r FROM (SELECT 1) GROUP BY 1), b AS (SELECT r FROM a) SELECT count(DISTINCT r) FROM b, x",
		"WITH a AS (SELECT random() AS r FROM y), b AS (SELECT r FROM a) SELECT count(DISTINCT r) FROM x, b",
		"WITH a AS (SELECT random() AS r FROM y), b AS (SELECT r + 0 AS r FROM a) SELECT count(DISTINCT r) FROM x, b",
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

// TestCoroutineRerunJoinShapes are joins around a co-routine body that once
// declined: an outer join whose WHERE C may first reduce (tag-select-0220), a
// table-valued function beside the subquery (jsonEachBestIndex's cost), a parent
// holding a subquery of its own, an order-sensitive aggregate reading the
// arrival order, and a DISTINCT aggregate's GROUP BY over the join. xd holds
// its rows out of key order, so an arrival or group order shows.
func TestCoroutineRerunJoinShapes(t *testing.T) {
	pre := append(append([]string(nil), coroutineRerunPre...),
		"CREATE TABLE xd(a)", "INSERT INTO xd VALUES(3),(1),(2)",
		"CREATE TABLE w(k INTEGER PRIMARY KEY, v) WITHOUT ROWID", "INSERT INTO w VALUES(1,1),(2,2)",
		"CREATE TABLE ix(a, c)", "CREATE INDEX ixa ON ix(a)", "INSERT INTO ix VALUES(3,1),(1,2),(2,3)",
		"CREATE TABLE ip(a INTEGER PRIMARY KEY, c)", "INSERT INTO ip VALUES(3,1),(1,2),(2,3)",
		"CREATE TABLE n(s TEXT COLLATE NOCASE)", "INSERT INTO n VALUES('A'),('b')",
	)
	for i, q := range []string{
		// An outer join and a parent WHERE.
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE t.b IS NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE t.b IS NOT NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE t.b > 1",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE x.a > 1",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE r IS NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE r IS NOT NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE coalesce(t.b, 0) = 0",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y WHERE b > 1) t ON t.b = x.a WHERE t.b IS NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y WHERE b > 1) t ON t.b = x.a WHERE t.b = 2",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, k FROM w) t ON t.k = x.a WHERE t.k IS NULL",
		"SELECT count(r), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, k FROM w) t ON t.k = x.a WHERE t.k = 1",
		"SELECT a, r IS NULL FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE t.b IS NULL OR x.a = 1 ORDER BY a",
		"SELECT a, b FROM xd LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = xd.a WHERE b IS NULL",
		"SELECT count(*), count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t WHERE t.b IN (1,2)",
		"SELECT count(DISTINCT r) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE EXISTS (SELECT 1 FROM y)",
		// A table-valued function beside the subquery.
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM json_each('[1,2]') j, x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM json_each('[1,2,3,4,5]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t, json_each('[1,2,3]') j",
		`SELECT count(DISTINCT r) FROM x, json_tree('{"a":[1,2]}') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t`,
		`SELECT count(DISTINCT r) FROM x, json_each('{"a":[1,2,3]}', '$.a') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t`,
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE j.value = x.a",
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE j.value > 1",
		"SELECT count(DISTINCT r) FROM ix, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE ix.a = j.value",
		"SELECT count(DISTINCT r) FROM x, y, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t WHERE j.value = 1 OR j.value = 2",
		"WITH t AS (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, t",
		"WITH t AS (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) SELECT count(DISTINCT r) FROM json_each('[10,20,30,40]') j, x, t",
		// A parent holding a subquery of its own.
		"WITH t AS (SELECT random() AS r FROM y) SELECT count(DISTINCT t1.r) FROM x, t t1 WHERE (SELECT count(*) FROM t) > 0",
		"WITH t AS (SELECT random() AS r FROM y) SELECT count(DISTINCT t1.r) FROM x, t t1 WHERE EXISTS (SELECT 1 FROM t)",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE EXISTS (SELECT 1 FROM y)",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE a IN (SELECT b FROM y)",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE (SELECT max(b) FROM y) = 2",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE EXISTS (SELECT 1 FROM y WHERE b = x.a)",
		"SELECT count(DISTINCT r), (SELECT count(*) FROM y) FROM x, (SELECT random() AS r FROM y) t",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE a > (SELECT min(b) FROM y)",
		"SELECT count(*) FROM x, (SELECT random() AS r FROM y) t WHERE r = r AND EXISTS (SELECT 1 FROM y)",
		"SELECT count(*) FROM n, (SELECT hex(randomblob(1)) AS h FROM y) WHERE h = s AND (SELECT 1) = 1",
		// An order-sensitive aggregate reading the arrival order.
		"SELECT count(DISTINCT r), group_concat(a) FROM x, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(a) FROM xd, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(a) FROM (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t, xd",
		"SELECT group_concat(a), count(DISTINCT r) FROM xd CROSS JOIN (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(a, '|') FROM xd, y, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(b) FROM xd, (SELECT random() AS r, b FROM y GROUP BY b) AS t",
		"SELECT count(DISTINCT r), json_group_array(a) FROM xd, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(a ORDER BY a) FROM xd, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT a, count(DISTINCT r), group_concat(b) FROM xd, y, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t GROUP BY a",
		"WITH t AS (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) SELECT count(DISTINCT r), group_concat(a) FROM xd, t",
		// A DISTINCT aggregate's GROUP BY over the join.
		"WITH t AS (SELECT random() AS r) SELECT count(DISTINCT r) FROM x, y, t GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM xd, y, t GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM y, xd, t GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT b, count(DISTINCT r) FROM xd, y, t GROUP BY b",
		"WITH t AS (SELECT random() AS r) SELECT a, b, count(DISTINCT r) FROM xd, y, t GROUP BY a, b",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM xd, y, t WHERE a = b GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM xd, y, t WHERE b > 1 GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM ix, y, t GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM ip, y, t GROUP BY a",
		"WITH t AS (SELECT random() AS r) SELECT c, count(DISTINCT r) FROM ix, y, t GROUP BY c",
		"WITH t AS (SELECT random() AS r) SELECT a, count(DISTINCT r) FROM xd, y, t GROUP BY a ORDER BY a DESC",
		"WITH t AS (SELECT random() AS r) SELECT b, count(DISTINCT r) FROM xd JOIN y ON a = b, t GROUP BY b",
		"WITH t AS (SELECT random() AS r FROM y) SELECT a, count(DISTINCT r) FROM xd, t GROUP BY a",
		"SELECT a, count(DISTINCT b) FROM xd, y GROUP BY a",
		"SELECT b, count(DISTINCT a) FROM xd, y GROUP BY b",
		"SELECT a, count(DISTINCT b) FROM ix, y GROUP BY a",
		"SELECT xd.a, count(DISTINCT b) FROM xd LEFT JOIN y ON y.b = xd.a GROUP BY xd.a",
		"SELECT xd.a, count(DISTINCT b) FROM xd, y WHERE xd.rowid = 2 GROUP BY xd.a",
	} {
		differ(t, fmt.Sprintf("join-%d", i), append(append([]string(nil), pre...), q))
	}
}

// TestCoroutineRerunFlattenShapes are the volatile bodies C flattens into a
// parent that once declined: substExpr's copy compared under its own implicit
// collating sequence (BINARY, or the NULL row's TK_IF_NULL_ROW beside a LEFT
// JOIN) and as an IN operand, a table-valued function beside the flattened
// body or reading a column of another item, a subquery of the parent naming
// the derived table (substSelect), and a DISTINCT aggregate beside another
// aggregate over plain tables, which hands the planner no distinct flags.
// n declares NOCASE, so the copy's BINARY decides where a bare expression
// would take n's sequence.
func TestCoroutineRerunFlattenShapes(t *testing.T) {
	pre := append(append([]string(nil), coroutineRerunPre...),
		"CREATE TABLE n(s TEXT COLLATE NOCASE)", "INSERT INTO n VALUES('A'),('b')",
		"CREATE TABLE xd(a)", "INSERT INTO xd VALUES(3),(1),(2)",
		"CREATE TABLE ix(a, c)", "CREATE INDEX ixa ON ix(a)", "INSERT INTO ix VALUES(3,1),(1,2),(2,3)",
	)
	for i, q := range []string{
		// The NULL row's copy, compared.
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE r = r",
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE r IS NULL OR r <> 5",
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE r IN (1,2) OR r IS NULL",
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, b FROM y) t ON t.b = x.a WHERE (r, 1) = (r, 1)",
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, CAST(b AS TEXT) AS k, b FROM y) t ON t.b = x.a WHERE k = 1",
		"SELECT count(*) FROM x LEFT JOIN (SELECT random() AS r, CAST(b AS TEXT) AS k, b FROM y) t ON t.b = x.a WHERE CAST(k AS INT) = 1",
		"SELECT count(*) FROM n LEFT JOIN (SELECT random() AS r, 'a' || b AS k, b FROM y) t ON t.b = 1 WHERE k = s",
		"SELECT a, h IS NULL FROM x LEFT JOIN (SELECT 'A' || random() AS h, b FROM y) t ON t.b = x.a WHERE h BETWEEN 'a' AND 'z' OR h IS NULL ORDER BY a",
		// The copy as an operand of IN, BETWEEN, CASE and under CAST.
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE r NOT IN (SELECT b FROM y)",
		"SELECT count(*) FROM n, (SELECT hex(randomblob(1)) || 'a' AS h, 'B' AS k FROM y) WHERE k IN (SELECT s FROM n)",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE k IN (SELECT s COLLATE BINARY FROM n)",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE k BETWEEN s AND 'c'",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE k BETWEEN s AND 'c' COLLATE BINARY",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE CASE k WHEN s THEN 1 END",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE CAST(k AS TEXT) = s",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) WHERE s = k",
		// A table-valued function beside the flattened body.
		"SELECT count(DISTINCT r) FROM x, json_each('[1,2]') j, (SELECT random() AS r FROM y) AS t",
		"SELECT count(DISTINCT r), count(*) FROM json_each('[1,2]') j, (SELECT random() AS r, b FROM y) AS t WHERE j.value = t.b",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r, b FROM y) AS t, json_each('[1,2,3]') j WHERE j.value > t.b",
		"SELECT count(*) FROM (SELECT random() AS r, b FROM y) AS t, json_each(json_array(t.b, 5)) j",
		"SELECT b, value FROM (SELECT random() AS r, b FROM y) AS t, json_each(json_array(t.b, 5)) j ORDER BY 1, 2",
		"SELECT key, value FROM (SELECT random() AS r, b FROM y) AS t, json_each(json_array(t.b, 5)) j WHERE key = 0 ORDER BY 1, 2",
		`SELECT count(DISTINCT r) FROM json_tree('{"a":[1,2]}') j, (SELECT random() AS r FROM y) AS t`,
		// A table-valued function reading another item's column.
		"SELECT count(DISTINCT r) FROM x, json_each(x.a) j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x, json_each(json_array(x.a, x.a)) j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM x, y, json_each(json_array(x.a, y.b)) j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r), group_concat(a) FROM x, json_each(x.a) j, (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t",
		"SELECT count(DISTINCT r) FROM (SELECT random() AS r FROM (SELECT 1) GROUP BY 1) AS t, x, json_each(x.a) j",
		"SELECT count(DISTINCT r) FROM x, json_each(x.a) j, (SELECT random() AS r, b FROM y GROUP BY b) AS t WHERE t.b = j.value",
		// A subquery of the parent naming the derived table.
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r FROM y) t WHERE (SELECT count(*) FROM y WHERE b < t.r) >= 0",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r, b FROM y) t WHERE (SELECT count(*) FROM y WHERE y.b = t.b) = 1",
		"SELECT count(DISTINCT r) FROM x, (SELECT random() AS r, b FROM y) t WHERE EXISTS (SELECT 1 FROM y AS q WHERE q.b = b)",
		"SELECT a, (SELECT b FROM y WHERE b = t.k) FROM x, (SELECT random() AS r, b AS k FROM y) t ORDER BY 1, 2",
		"SELECT count(*) FROM x, (SELECT random() AS r, b FROM y) t WHERE (SELECT count(*) FROM y WHERE b < t.r) >= 0 AND a IN (SELECT t.b)",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) t WHERE EXISTS (SELECT 1 FROM n AS m WHERE t.k = m.s)",
		"SELECT count(*) FROM n, (SELECT random() AS r, 'B' AS k FROM y) t WHERE EXISTS (SELECT 1 FROM n AS m WHERE m.s = t.k)",
		// A DISTINCT aggregate beside another aggregate, over plain tables.
		"SELECT a, count(DISTINCT b), group_concat(b) FROM x, y GROUP BY a",
		"SELECT a, count(DISTINCT b), group_concat(b) FROM xd, y GROUP BY a",
		"SELECT b, count(DISTINCT a), group_concat(a) FROM xd, y GROUP BY b",
		"SELECT a, count(DISTINCT b), group_concat(b) FROM ix, y GROUP BY a",
		"SELECT c, count(DISTINCT b), group_concat(b, '|') FROM ix, y WHERE b < 3 GROUP BY c ORDER BY c DESC",
		"SELECT a, sum(DISTINCT b), group_concat(b) FROM xd JOIN y ON a >= b GROUP BY a",
		"SELECT a, count(DISTINCT b), count(DISTINCT b) FROM xd, y GROUP BY a",
	} {
		differ(t, fmt.Sprintf("flatten-%d", i), append(append([]string(nil), pre...), q))
	}
}
