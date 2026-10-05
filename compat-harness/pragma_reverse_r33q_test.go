// Package compat gates reverse_unordered_selects pragma tests.
package compat

import (
	"encoding/json"
	"testing"
)

// r33qSchema creates test tables with varied indexes and structure.
var r33qSchema = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c INT)`,
	`INSERT INTO t1 VALUES(50,'e',5),(10,'a',1),(40,'d',4),(20,'b',2),(30,'c',3)`,
	`CREATE TABLE t2(k TEXT PRIMARY KEY, v INT) WITHOUT ROWID`,
	`INSERT INTO t2 VALUES('pp',1),('mm',2),('zz',3),('aa',4)`,
	`CREATE TABLE t3(x INT, y TEXT, z INT)`,
	`INSERT INTO t3 VALUES(3,'c',30),(1,'a',10),(2,'b',20),(4,'d',40)`,
	`CREATE INDEX t3desc ON t3(x DESC)`,
	`CREATE INDEX t3xy ON t3(y, z)`,
	`CREATE TABLE t4(p, q)`,
	`INSERT INTO t4 VALUES('one',1),('two',2),('three',3)`,
	`CREATE TABLE t5(m INT, n TEXT)`,
	`INSERT INTO t5 VALUES(1,'i'),(2,'ii'),(3,'iii'),(4,'iv')`,
	`CREATE INDEX t5part ON t5(m) WHERE m>1`,
	`CREATE TABLE t7(k)`,
	`INSERT INTO t7 VALUES(1)`,
	`CREATE TABLE t8(k, o)`,
	`INSERT INTO t8 VALUES(1,'z'),(1,'m'),(1,'a')`,
	`CREATE VIEW v1 AS SELECT a, b FROM t1`,
	`CREATE TEMP TABLE tt(s INT)`,
	`INSERT INTO tt VALUES(7),(8),(9)`,
}

// r33qReadouts are test cases run with and without reverse_unordered_selects=1.
var r33qReadouts = []struct{ name, query string }{
	{"gc rowid table", `SELECT group_concat(b,'') FROM t1`},
	{"gc no index", `SELECT group_concat(p,'') FROM t4`},
	{"gc without rowid", `SELECT group_concat(k,'') FROM t2`},
	{"gc temp table", `SELECT group_concat(s,'') FROM tt`},
	{"gc view", `SELECT group_concat(b,'') FROM v1`},
	{"gc desc index", `SELECT group_concat(y,'') FROM t3 WHERE x>0`},
	{"gc two col index", `SELECT group_concat(y,'') FROM t3 WHERE y>'a'`},
	{"gc partial index", `SELECT group_concat(n,'') FROM t5 WHERE m>1`},
	{"gc indexed by", `SELECT group_concat(y,'') FROM t3 INDEXED BY t3xy WHERE y>'a'`},
	{"gc not indexed", `SELECT group_concat(y,'') FROM t3 NOT INDEXED WHERE x>0`},
	{"gc with order by", `SELECT group_concat(b,'') FROM (SELECT b FROM t1 ORDER BY a)`},
	{"limit", `SELECT a FROM t1 LIMIT 2`},
	{"limit offset", `SELECT a FROM t1 LIMIT 2 OFFSET 2`},
	{"limit without rowid", `SELECT k FROM t2 LIMIT 2`},
	{"limit no index", `SELECT p FROM t4 LIMIT 1`},
	{"limit temp", `SELECT s FROM tt LIMIT 1`},
	{"limit view", `SELECT a FROM v1 LIMIT 2`},
	{"limit where", `SELECT a FROM t1 WHERE a>15 LIMIT 2`},
	{"limit like", `SELECT a FROM t1 WHERE b LIKE '%' LIMIT 2`},
	{"limit in", `SELECT a FROM t1 WHERE a IN (10,30,50) LIMIT 2`},
	{"limit between", `SELECT a FROM t1 WHERE a BETWEEN 10 AND 40 LIMIT 2`},
	{"limit is null", `SELECT a FROM t1 WHERE c IS NOT NULL LIMIT 2`},
	{"derived limit", `SELECT a FROM (SELECT a FROM t1) LIMIT 2`},
	{"derived gc", `SELECT group_concat(b,'') FROM (SELECT b FROM t1)`},
	{"cte limit", `WITH w AS (SELECT a FROM t1) SELECT a FROM w LIMIT 2`},
	{"cte materialized", `WITH w AS MATERIALIZED (SELECT a FROM t1) SELECT a FROM w LIMIT 2`},
	{"union all", `SELECT a FROM t1 UNION ALL SELECT x FROM t3`},
	{"union", `SELECT a FROM t1 UNION SELECT x FROM t3`},
	{"except", `SELECT a FROM t1 EXCEPT SELECT x FROM t3`},
	{"intersect", `SELECT a FROM t1 INTERSECT SELECT a FROM t1`},
	{"scalar subquery", `SELECT (SELECT b FROM t1 LIMIT 1)`},
	{"correlated subquery", `SELECT p, (SELECT b FROM t1 LIMIT 1) FROM t4 LIMIT 1`},
	{"exists", `SELECT p FROM t4 WHERE EXISTS(SELECT 1 FROM t1 WHERE a>0) LIMIT 1`},
	{"in subquery", `SELECT a FROM t1 WHERE a IN (SELECT x*10 FROM t3) LIMIT 2`},
	{"join gc", `SELECT group_concat(t1.b||t3.y,'') FROM t1, t3 WHERE t1.c=t3.x`},
	{"join limit", `SELECT t1.a, t3.x FROM t1, t3 WHERE t1.c=t3.x LIMIT 2`},
	{"join three tables", `SELECT group_concat(t1.b||t3.y||t5.n,'') FROM t1, t3, t5 WHERE t1.c=t3.x AND t3.x=t5.m`},
	{"join cross", `SELECT group_concat(t4.p||t3.y,'') FROM t4, t3`},
	{"join left", `SELECT group_concat(coalesce(t3.y,'-'),'') FROM t1 LEFT JOIN t3 ON t1.c=t3.x`},
	{"join using", `SELECT group_concat(z,'') FROM t3 JOIN (SELECT 1 AS x UNION ALL SELECT 2) USING (x)`},
	{"join no index", `SELECT group_concat(t4.p||t5.n,'') FROM t4, t5 WHERE t5.m<3`},
	{"join order by", `SELECT group_concat(t3.y,'') FROM t1, t3 WHERE t1.c=t3.x ORDER BY t3.y`},
	{"join auto index", `SELECT group_concat(t8.o,'') FROM t7, t8 WHERE t7.k=t8.k`},
	{"join auto index limit", `SELECT t8.o FROM t7, t8 WHERE t7.k=t8.k LIMIT 2`},
	{"order by", `SELECT a FROM t1 ORDER BY a LIMIT 2`},
	{"order by desc", `SELECT a FROM t1 ORDER BY a DESC LIMIT 2`},
	{"order by b", `SELECT b FROM t1 ORDER BY b LIMIT 2`},
	{"group by", `SELECT b, count(*) FROM t1 GROUP BY b LIMIT 2`},
	{"group by gc", `SELECT group_concat(b,'') FROM t1 GROUP BY c>3`},
	{"distinct", `SELECT DISTINCT b FROM t1 LIMIT 2`},
	{"distinct redundant ipk", `SELECT DISTINCT a FROM t1 LIMIT 2`},
	{"distinct redundant gc", `SELECT group_concat(a,'') FROM (SELECT DISTINCT a FROM t1)`},
	{"min", `SELECT min(a) FROM t1`},
	{"max", `SELECT max(a) FROM t1`},
	{"count", `SELECT count(*) FROM t1`},
	{"sum", `SELECT sum(a) FROM t1`},
}

// r33qDeclined tracks shapes this engine refuses with the pragma on and off.
var r33qDeclined = map[string]bool{
	"join using": true,
}

// r33qOpen tracks known-open divergences where the pragma is on.
var r33qOpen = map[string]bool{
	"gc without rowid":    true,
	"limit without rowid": true,
	"cte materialized":    true,
}

// TestR33QReverseUnorderedReadouts measures readouts with and without the pragma.
func TestR33QReverseUnorderedReadouts(t *testing.T) {
	for _, on := range []bool{true, false} {
		setup := append([]string{}, r33qSchema...)
		label := "off"
		if on {
			setup = append(setup, `PRAGMA reverse_unordered_selects=1`)
			label = "on"
		}
		for _, r := range r33qReadouts {
			open := r33qDeclined[r.name] || (on && r33qOpen[r.name])
			t.Run(label+"/"+r.name, func(t *testing.T) {
				r33qLastAgrees(t, r.name, open, append(append([]string{}, setup...), r.query))
			})
		}
	}
}

// TestR33QReverseUnorderedWritePath tests INSERT...SELECT and CREATE TABLE AS
// SELECT, where the scan direction is visible in stored rowids.
func TestR33QReverseUnorderedWritePath(t *testing.T) {
	differ(t, "r33q insert select under the flag",
		append(append([]string{}, r33qSchema...),
			`PRAGMA reverse_unordered_selects=1`,
			`CREATE TABLE dst(id INTEGER PRIMARY KEY, v TEXT)`,
			`INSERT INTO dst(v) SELECT b FROM t1`,
			`SELECT id, v FROM dst ORDER BY id`,
		))
	differ(t, "r33q create table as select under the flag",
		append(append([]string{}, r33qSchema...),
			`PRAGMA reverse_unordered_selects=1`,
			`CREATE TABLE dst2 AS SELECT b FROM t1`,
			`SELECT rowid, b FROM dst2 ORDER BY rowid`,
		))
	differ(t, "r33q update and delete under the flag", append(append([]string{}, r33qSchema...),
		`PRAGMA reverse_unordered_selects=1`,
		`UPDATE t1 SET c=c+100 WHERE a>20`,
		`DELETE FROM t1 WHERE a=10`,
		`SELECT a, c FROM t1 ORDER BY a`,
	))
}

// TestR33QReverseUnorderedPragmaShape verifies the pragma getter and setter
// shapes, and that the flag survives intervening statements.
func TestR33QReverseUnorderedPragmaShape(t *testing.T) {
	differ(t, "r33q reverse_unordered_selects shape", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA reverse_unordered_selects`, // a fresh connection: 0
		`PRAGMA reverse_unordered_selects=1`,
		`PRAGMA reverse_unordered_selects`,
		`PRAGMA reverse_unordered_selects=0`,
		`PRAGMA reverse_unordered_selects`,
		`PRAGMA reverse_unordered_selects=yes`,
		`PRAGMA reverse_unordered_selects`,
		`PRAGMA reverse_unordered_selects=false`,
		`PRAGMA reverse_unordered_selects`,
		`PRAGMA reverse_unordered_selects=on`,
		`PRAGMA main.reverse_unordered_selects`,
		`PRAGMA temp.reverse_unordered_selects=off`,
		`PRAGMA reverse_unordered_selects`,
	})
	differ(t, "r33q reverse_unordered_selects survives", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`PRAGMA reverse_unordered_selects=1`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
		`PRAGMA reverse_unordered_selects`,
		`CREATE INDEX i1 ON t1(b)`,
		`PRAGMA reverse_unordered_selects`,
		`BEGIN`,
		`INSERT INTO t1 VALUES(4,'w')`,
		`PRAGMA reverse_unordered_selects`,
		`COMMIT`,
		`PRAGMA reverse_unordered_selects`,
		`SELECT group_concat(b,'') FROM t1`,
	})
}

// TestR33QReverseUnorderedPlanCache verifies the plan cache doesn't reuse plans
// across pragma changes.
func TestR33QReverseUnorderedPlanCache(t *testing.T) {
	differ(t, "r33q plan cache across the setter", append(append([]string{}, r33qSchema...),
		`SELECT group_concat(b,'') FROM t1`,
		`PRAGMA reverse_unordered_selects=1`,
		`SELECT group_concat(b,'') FROM t1`,
		`PRAGMA reverse_unordered_selects=0`,
		`SELECT group_concat(b,'') FROM t1`,
		`PRAGMA reverse_unordered_selects=1`,
		`SELECT group_concat(b,'') FROM t1`,
	))
	differ(t, "r33q plan cache limit across the setter", append(append([]string{}, r33qSchema...),
		`SELECT a FROM t1 LIMIT 2`,
		`PRAGMA reverse_unordered_selects=1`,
		`SELECT a FROM t1 LIMIT 2`,
		`PRAGMA reverse_unordered_selects=off`,
		`SELECT a FROM t1 LIMIT 2`,
	))
}

// r33qLastAgrees compares the last statement's result against the oracle's.
func r33qLastAgrees(t *testing.T, name string, open bool, stmts []string) {
	t.Helper()
	oracle := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	ob, _ := json.Marshal(oracle[len(oracle)-1])
	gb, _ := json.Marshal(got[len(got)-1])
	agrees := string(ob) == string(gb)
	switch {
	case open && agrees:
		t.Errorf("[r33q %s] no longer diverges -- remove it from r33qOpen\n  sql:  %s\n  both: %s",
			name, stmts[len(stmts)-1], ob)
	case open:
		t.Logf("KNOWN OPEN r33q/%s\n  sql:    %s\n  cgo:    %s\n  musql: %s",
			name, stmts[len(stmts)-1], ob, gb)
	case !agrees:
		t.Errorf("[r33q %s] musql DIVERGES from C SQLite\n  sql:     %s\n  cgo:     %s\n  musql:  %s",
			name, stmts[len(stmts)-1], ob, gb)
	}
}
