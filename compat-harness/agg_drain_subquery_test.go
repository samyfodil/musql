package compat

import "testing"

// The sorted GROUP BY drain fixture; trailing ORDER BY is load-bearing.
const aggDrainFx = `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT)`

var aggDrainRows = []string{
	`INSERT INTO t VALUES(1,0,5,'a'),(2,0,7,'b'),(3,1,2,'c'),(4,1,9,'d'),(5,2,NULL,'e')`,
	`CREATE TABLE u(a INTEGER, b INTEGER)`,
	`INSERT INTO u VALUES(0,100),(1,200),(2,300)`,
}

func aggDrainScript() []string {
	return append([]string{aggDrainFx}, aggDrainRows...)
}

// TestAggDrainSubqueryAnswers pins answers for subqueries inside aggregate
// expressions on the sorted GROUP BY drain, where cursors are closed.
func TestAggDrainSubqueryAnswers(t *testing.T) {
	for _, tc := range []struct{ name, q string }{
		{"bare correlated scalar", `SELECT k, sum((SELECT v)) FROM t GROUP BY k ORDER BY k`},
		{"bare correlated text", `SELECT k, group_concat((SELECT s)) FROM t GROUP BY k ORDER BY k`},
		{"qualified correlated", `SELECT k, sum((SELECT t.v)) FROM t GROUP BY k ORDER BY k`},
		{"correlated rowid", `SELECT k, sum((SELECT rowid)) FROM t GROUP BY k ORDER BY k`},
		{"qualified correlated rowid", `SELECT k, sum((SELECT t.rowid)) FROM t GROUP BY k ORDER BY k`},
		{"correlated over another table", `SELECT k, sum((SELECT b FROM u WHERE u.a = t.k)) FROM t GROUP BY k ORDER BY k`},
		{"correlated with its own aggregate", `SELECT k, sum((SELECT max(b) FROM u WHERE u.a = t.k)) FROM t GROUP BY k ORDER BY k`},
		{"correlated inside arithmetic", `SELECT k, sum(v + (SELECT max(b) FROM u WHERE u.a = t.k)) FROM t GROUP BY k ORDER BY k`},
		{"correlated inside CASE", `SELECT k, sum(CASE WHEN (SELECT v) > 4 THEN 1 ELSE 0 END) FROM t GROUP BY k ORDER BY k`},
		{"uncorrelated scalar", `SELECT k, sum((SELECT 1)) FROM t GROUP BY k ORDER BY k`},
		{"uncorrelated over a table", `SELECT k, sum((SELECT max(b) FROM u)) FROM t GROUP BY k ORDER BY k`},
		{"correlated EXISTS", `SELECT k, sum(EXISTS(SELECT 1 FROM u WHERE u.b = t.v)) FROM t GROUP BY k ORDER BY k`},
		{"correlated NOT EXISTS", `SELECT k, sum(NOT EXISTS(SELECT 1 FROM u WHERE u.a = t.k)) FROM t GROUP BY k ORDER BY k`},
		{"IN over a subquery", `SELECT k, sum(CASE WHEN v IN (SELECT b FROM u) THEN 1 ELSE 0 END) FROM t GROUP BY k ORDER BY k`},
		{"IN over a correlated subquery", `SELECT k, sum(CASE WHEN k IN (SELECT a FROM u WHERE u.b > t.v) THEN 1 ELSE 0 END) FROM t GROUP BY k ORDER BY k`},
		{"compound body", `SELECT k, sum((SELECT v UNION SELECT v)) FROM t GROUP BY k ORDER BY k`},
		{"compound correlated arms", `SELECT k, sum((SELECT b FROM u WHERE u.a=t.k UNION SELECT 0)) FROM t GROUP BY k ORDER BY k`},
		{"subquery in the FILTER", `SELECT k, sum(v) FILTER (WHERE (SELECT v) > 4) FROM t GROUP BY k ORDER BY k`},
		{"subquery in filter and argument", `SELECT k, sum((SELECT v)) FILTER (WHERE (SELECT v) > 4) FROM t GROUP BY k ORDER BY k`},
		{"subquery as the separator", `SELECT k, group_concat(s, (SELECT '-')) FROM t GROUP BY k ORDER BY k`},
		{"correlated separator", `SELECT k, group_concat(s, (SELECT CAST(k AS TEXT))) FROM t GROUP BY k ORDER BY k`},
		{"DISTINCT over a subquery argument", `SELECT k, count(DISTINCT (SELECT v)) FROM t GROUP BY k ORDER BY k`},
		{"HAVING over a subquery argument", `SELECT k, count(*) FROM t GROUP BY k HAVING sum((SELECT v)) > 3 ORDER BY k`},
		{"no matching row", `SELECT k, sum((SELECT b FROM u WHERE u.a = t.v)) FROM t GROUP BY k ORDER BY k`},
		{"correlated on a NULL column", `SELECT k, group_concat((SELECT v)) FROM t GROUP BY k ORDER BY k`},
		{"two frames deep", `SELECT k, sum((SELECT (SELECT t.v))) FROM t GROUP BY k ORDER BY k`},
		{"two frames deep with a FROM", `SELECT k, sum((SELECT (SELECT b FROM u WHERE u.a=t.k))) FROM t GROUP BY k ORDER BY k`},
		{"three frames deep", `SELECT k, sum((SELECT (SELECT (SELECT count(*) FROM u WHERE u.b > t.v)))) FROM t GROUP BY k ORDER BY k`},
		{"derived table in the body", `SELECT k, sum((SELECT count(*) FROM (SELECT 1) w)) FROM t GROUP BY k ORDER BY k`},
		{"correlated derived table in the body", `SELECT k, sum((SELECT max(x) FROM (SELECT v AS x FROM t t2 WHERE t2.k = t.k))) FROM t GROUP BY k ORDER BY k`},
		{"compound derived table in the body", `SELECT k, max(v + (SELECT count(*) FROM (SELECT b FROM u UNION ALL SELECT b FROM u))) FROM t GROUP BY k ORDER BY k`},
		{"window function in the body", `SELECT k, sum((SELECT sum(b) OVER () FROM u WHERE u.a = t.k)) FROM t GROUP BY k ORDER BY k`},
		{"body with its own ORDER BY and LIMIT", `SELECT k, group_concat((SELECT b FROM u ORDER BY abs(u.a - t.k) LIMIT 1)) FROM t GROUP BY k ORDER BY k`},
		{"hash-path control", `SELECT k, sum((SELECT v)) FROM t GROUP BY k`},
		{"whole-table control", `SELECT sum((SELECT v)) FROM t`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, aggDrainScript(), tc.q)
		})
	}
}

// TestAggDrainSubqueryJoinAnswers pins subqueries on the drain over JOINs,
// testing USING/NATURAL join resolution and LEFT/RIGHT/FULL coalesced columns.
func TestAggDrainSubqueryJoinAnswers(t *testing.T) {
	script := []string{
		`CREATE TABLE j1(k INTEGER, x INTEGER)`,
		`CREATE TABLE j2(k INTEGER, y INTEGER)`,
		`CREATE TABLE j3(k INTEGER, z INTEGER)`,
		`INSERT INTO j1 VALUES(1,10),(2,20),(NULL,90)`,
		`INSERT INTO j2 VALUES(1,111),(3,333),(NULL,900)`,
		`INSERT INTO j3 VALUES(1,7),(4,8)`,
	}
	for _, tc := range []struct{ name, q string }{
		{"inner using", `SELECT x, sum((SELECT k)) FROM j1 JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"left using", `SELECT x, sum((SELECT k)) FROM j1 LEFT JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"right using", `SELECT x, sum((SELECT k)) FROM j1 RIGHT JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"full using", `SELECT x, sum((SELECT k)) FROM j1 FULL JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"natural inner", `SELECT x, sum((SELECT k)) FROM j1 NATURAL JOIN j2 GROUP BY x ORDER BY x`},
		{"natural full", `SELECT x, sum((SELECT k)) FROM j1 NATURAL FULL JOIN j2 GROUP BY x ORDER BY x`},
		{"full using group_concat", `SELECT x, group_concat((SELECT k)) FROM j1 FULL JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"three coalesced arms", `SELECT x, group_concat((SELECT k)) FROM j1 FULL JOIN j2 USING(k) FULL JOIN j3 USING(k) GROUP BY x ORDER BY x`},
		{"typeof through the chain", `SELECT x, group_concat((SELECT typeof(k))) FROM j1 FULL JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"null through the chain", `SELECT x, sum((SELECT k IS NULL)) FROM j1 FULL JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"qualified names one arm", `SELECT j1.x, sum((SELECT j2.y)) FROM j1 LEFT JOIN j2 ON j1.k=j2.k GROUP BY j1.x ORDER BY j1.x`},
		{"null-extended side", `SELECT j1.x, group_concat((SELECT j2.y)) FROM j1 LEFT JOIN j2 ON j1.k=j2.k GROUP BY j1.x ORDER BY j1.x`},
		{"rowid of one arm", `SELECT x, sum((SELECT j1.rowid)) FROM j1 LEFT JOIN j2 USING(k) GROUP BY x ORDER BY x`},
		{"self join aliased", `SELECT p.x, sum((SELECT q.y)) FROM j1 p LEFT JOIN j2 q USING(k) GROUP BY p.x ORDER BY p.x`},
		{"coalesced inside arithmetic", `SELECT x, sum((SELECT k) * 2 + 1) FROM j1 FULL JOIN j2 USING(k) GROUP BY x ORDER BY x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, script, tc.q)
		})
	}
}

// TestAggDrainSubtypeThroughTheRecord pins JSON subtype loss through
// serialized records; json_quote detects the difference between subtyped and plain text.
func TestAggDrainSubtypeThroughTheRecord(t *testing.T) {
	script := []string{
		`CREATE TABLE g(k INTEGER, j TEXT, gen TEXT GENERATED ALWAYS AS (json(j)) VIRTUAL)`,
		`INSERT INTO g(k,j) VALUES(1,'[7]'),(1,'[8]'),(2,'{"a":1}')`,
	}
	for _, tc := range []struct{ name, q string }{
		{"generated column direct",
			`SELECT k, group_concat(json_quote(gen)) FROM g GROUP BY k ORDER BY k`},
		{"generated column through a subquery",
			`SELECT k, group_concat((SELECT json_quote(gen))) FROM g GROUP BY k ORDER BY k`},
		{"generated column, subquery quotes outside",
			`SELECT k, group_concat(json_quote((SELECT gen))) FROM g GROUP BY k ORDER BY k`},
		{"generated column through min/max, which return their argument",
			`SELECT k, json_quote(max(gen)) FROM g GROUP BY k ORDER BY k`},
		{"generated column hash control",
			`SELECT k, group_concat((SELECT json_quote(gen))) FROM g GROUP BY k`},
		{"generated column whole-table control",
			`SELECT group_concat(json_quote(gen)) FROM g`},
		{"json_each value direct",
			`SELECT key, group_concat(json_quote(value)) FROM json_each('[[1],[2],[3]]') GROUP BY key ORDER BY key`},
		{"json_each value through a subquery",
			`SELECT key, group_concat((SELECT json_quote(value))) FROM json_each('[[1],[2],[3]]') GROUP BY key ORDER BY key`},
		{"json_each joined to a table",
			`SELECT g.k, group_concat((SELECT json_quote(je.value))) FROM g, json_each('[[1]]') je GROUP BY g.k ORDER BY g.k`},
		{"subtype() over the generated column",
			`SELECT k, group_concat(subtype(gen)) FROM g GROUP BY k ORDER BY k`},
		{"subtype() through a subquery",
			`SELECT k, group_concat((SELECT subtype(gen))) FROM g GROUP BY k ORDER BY k`},
		{"json() rebuilt above the record leaf",
			`SELECT k, group_concat(json_quote(json(j))) FROM g GROUP BY k ORDER BY k`},
		{"json() rebuilt inside a subquery",
			`SELECT k, group_concat((SELECT json_quote(json(j)))) FROM g GROUP BY k ORDER BY k`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, script, tc.q)
		})
	}
}

// TestAggDrainOperandMetadata pins declared collation and comparison affinity
// of aggregate operands on the drain; pinned for a live wrong answer.
func TestAggDrainOperandMetadata(t *testing.T) {
	script := []string{
		`CREATE TABLE c1(k INTEGER, a TEXT COLLATE NOCASE, n INTEGER)`,
		`INSERT INTO c1 VALUES(1,'abc',1),(1,'DEF',2),(2,'ghi',3)`,
		`CREATE TABLE s(b TEXT, m TEXT)`,
		`INSERT INTO s VALUES('ABC','1'),('def','2'),('zzz','3')`,
	}
	for _, tc := range []struct{ name, q string }{
		{"bare name declared collation", `SELECT k, sum(a = 'ABC') FROM c1 GROUP BY k ORDER BY k`},
		{"bare name collation in CASE", `SELECT k, sum(CASE WHEN a = 'ABC' THEN 1 ELSE 0 END) FROM c1 GROUP BY k ORDER BY k`},
		{"bare name comparison affinity", `SELECT k, sum(n = '1') FROM c1 GROUP BY k ORDER BY k`},
		{"qualified collation", `SELECT k, sum(c1.a = 'ABC') FROM c1 GROUP BY k ORDER BY k`},
		{"qualified affinity", `SELECT k, sum(c1.n = '1') FROM c1 GROUP BY k ORDER BY k`},
		{"hash control collation", `SELECT k, sum(a = 'ABC') FROM c1 GROUP BY k`},
		{"hash control affinity", `SELECT k, sum(n = '1') FROM c1 GROUP BY k`},
		{"collation through a subquery", `SELECT k, sum((SELECT a = 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"affinity through a subquery", `SELECT k, sum((SELECT n = '1')) FROM c1 GROUP BY k ORDER BY k`},
		{"collation into a correlated body", `SELECT k, sum((SELECT count(*) FROM s WHERE s.b = c1.a)) FROM c1 GROUP BY k ORDER BY k`},
		{"affinity into a correlated body", `SELECT k, sum((SELECT count(*) FROM s WHERE s.m = c1.n)) FROM c1 GROUP BY k ORDER BY k`},
		{"IN over a subquery keeps the collation", `SELECT k, sum(a IN (SELECT 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"IN over a table keeps the collation", `SELECT k, sum(a IN (SELECT b FROM s)) FROM c1 GROUP BY k ORDER BY k`},
		{"compound EXCEPT dedups NOCASE", `SELECT k, group_concat((SELECT a EXCEPT SELECT 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"compound INTERSECT dedups NOCASE", `SELECT k, group_concat((SELECT a INTERSECT SELECT 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"compound UNION dedups NOCASE", `SELECT k, group_concat((SELECT a UNION SELECT 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"compound under EXISTS", `SELECT k, sum(EXISTS(SELECT a EXCEPT SELECT 'ABC')) FROM c1 GROUP BY k ORDER BY k`},
		{"compound hash control", `SELECT k, group_concat((SELECT a EXCEPT SELECT 'ABC')) FROM c1 GROUP BY k`},
		{"typeof through the record", `SELECT k, group_concat((SELECT typeof(a))) FROM c1 GROUP BY k ORDER BY k`},
		{"typeof of an integer column", `SELECT k, group_concat((SELECT typeof(n))) FROM c1 GROUP BY k ORDER BY k`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, script, tc.q)
		})
	}
}

// TestAggOperandShapesAgainstOracle pins aggregate operand shapes against the
// oracle across three plan paths: whole-table, hash GROUP BY, and sorted drain.
func TestAggOperandShapesAgainstOracle(t *testing.T) {
	script := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT)`,
		`INSERT INTO t VALUES(1,0,5,'s0'),(2,0,-3,'s1'),(3,1,7,'s2'),(4,1,7,'s3'),(5,2,0,'s4')`,
		`INSERT INTO t VALUES(7,0,11,'s6'),(8,1,-1,'s7')`,
		`INSERT INTO t VALUES(9,0,NULL,NULL)`,
		`INSERT INTO t VALUES(10,1,2.5,'x')`,
	}
	for _, call := range []string{
		"count(v)", "sum(v)", "total(v)", "avg(v)", "min(v)", "max(v)", "max(s)",
		"sum(DISTINCT v)", "count(DISTINCT v)",
		"min(v * 2)", "max(abs(v + 1))", "sum(CASE WHEN v > 0 THEN v ELSE 0 END)",
		"min(s || 'z')", "sum(DISTINCT v * 2)",
		"sum(v) FILTER (WHERE 1)", "sum(v) FILTER (WHERE 0)",
		"sum(v) FILTER (WHERE v > 0)", "sum(v) FILTER (WHERE v)",
		"count(*) FILTER (WHERE k = 1)", "max(s) FILTER (WHERE v > 0)",
		"count(DISTINCT v) FILTER (WHERE v > 0)",
		"group_concat(s)", "group_concat(s, '-')", "group_concat(s, s)",
		"group_concat(DISTINCT s)", "group_concat(s, '-') FILTER (WHERE v > 0)",
		"json_group_array(v)", "json_group_object(s, v)",
		"sum(v), group_concat(s)", "group_concat(s), sum(v)",
		"sum(rowid)", "min(rowid)", "max(t.rowid)", "count(DISTINCT rowid)",
	} {
		t.Run(call, func(t *testing.T) {
			flLockstep(t, call, script,
				"SELECT "+call+" FROM t",
				"SELECT k, "+call+" FROM t GROUP BY k",
				"SELECT k, "+call+" FROM t GROUP BY k ORDER BY k")
		})
	}
}

// TestAggDrainVtabInSubqueryAnswers pins virtual table MATCH inside an
// aggregate's subquery; answers correctly via the hash path where cursors are live.
func TestAggDrainVtabInSubqueryAnswers(t *testing.T) {
	script := []string{
		`CREATE VIRTUAL TABLE f USING fts4(body)`,
		`INSERT INTO f(body) VALUES('alpha beta'),('beta gamma'),('gamma delta')`,
		`CREATE TABLE m(k INTEGER, q TEXT)`,
		`INSERT INTO m VALUES(1,'beta'),(1,'gamma'),(2,'delta')`,
	}
	for _, tc := range []struct{ name, q string }{
		{"sum of a MATCH count", `SELECT k, sum((SELECT count(*) FROM f WHERE f.body MATCH m.q)) FROM m GROUP BY k ORDER BY k`},
		{"group_concat through MATCH", `SELECT k, group_concat((SELECT group_concat(body) FROM f WHERE f MATCH m.q)) FROM m GROUP BY k ORDER BY k`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flLockstep(t, tc.name, script, tc.q)
		})
	}
}
