package compat

import "testing"

// TestAggDistinctHashAgrees pins DISTINCT aggregates against the oracle over
// many distinct values, values that compare equal across storage classes, and
// the collations whose equality is not byte equality (NOCASE stops at the
// first NUL), in each text encoding.
func TestAggDistinctHashAgrees(t *testing.T) {
	series := "WITH RECURSIVE s(value) AS (VALUES(1) UNION ALL SELECT value+1 FROM s WHERE value<20000) "
	schema := []string{
		"CREATE TABLE m(k, v)",
		"INSERT INTO m VALUES(1,2),(1,2.0),(1,'2'),(1,x'32'),(1,NULL),(1,0.0),(1,-0.0),(1,0)," +
			"(2,9007199254740993),(2,9007199254740992.0),(2,9007199254740992),(2,-9223372036854775808),(2,-9223372036854775808.0),(2,1e300),(2,1e300)",
		"CREATE TABLE c(n TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM, b TEXT)",
		"INSERT INTO c VALUES('a'||char(0)||'b','x','é'),('A'||char(0)||'c','x ','É'),('ab','x  ','é'),('AB','y','e'),('a'||char(0)||'bc',' x','ü'),('abc','Y ',NULL)",
		"CREATE TABLE u(x, y TEXT COLLATE NOCASE)",
		"INSERT INTO u VALUES('é','A'),('é','a'),('ä','B'),('Ä','b'),('日本','x'),('日本','X'),('Ω',NULL)",
	}
	queries := []string{
		series + "SELECT sum(DISTINCT round(value/1.5)), count(DISTINCT value%1000), avg(DISTINCT value%997), total(DISTINCT -value) FROM s",
		series + "SELECT max(DISTINCT value%5000), min(DISTINCT value%5000), json_array_length(json_group_array(DISTINCT value%3000)) FROM s",
		"SELECT k, sum(DISTINCT v), count(DISTINCT v), group_concat(DISTINCT v) FROM m WHERE typeof(v) <> 'blob' GROUP BY k ORDER BY k",
		"SELECT count(DISTINCT v), count(DISTINCT typeof(v)) FROM m",
		"SELECT max(DISTINCT v), k FROM m",
		"SELECT min(DISTINCT v), k FROM m WHERE k = 2",
		"SELECT json_group_array(DISTINCT v) FROM m WHERE typeof(v) <> 'blob'",
		"SELECT count(DISTINCT n), hex(group_concat(DISTINCT n)), count(DISTINCT r), group_concat(DISTINCT r), count(DISTINCT b), group_concat(DISTINCT b) FROM c",
		"SELECT count(DISTINCT x), group_concat(DISTINCT x), count(DISTINCT y), group_concat(DISTINCT y), max(DISTINCT y) FROM u",
	}
	for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
		for _, q := range queries {
			stmts := append(append([]string{"PRAGMA encoding='" + enc + "'"}, schema...), q)
			aggOrderCheck(t, aggOrderCase{enc + ": " + q, stmts, true})
		}
	}
}

// TestAggDistinctColumnarAgrees pins the DISTINCT aggregates the program JIT
// serves -- over a VACUUMed table, so its rows are segments -- against the
// oracle, beside a delta, and where the loop answers instead: a NULL column,
// and a sum whose magnitude may overflow.
func TestAggDistinctColumnarAgrees(t *testing.T) {
	setup := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, n INTEGER, big INTEGER)",
		"INSERT INTO t SELECT x, x % 10, (x * 7919) % 501 - 250, CASE WHEN x % 41 = 0 THEN NULL ELSE x % 50 END, 9223372036854775807 - x % 3 " +
			"FROM (WITH RECURSIVE s(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM s WHERE x < 5000) SELECT x FROM s)",
		"VACUUM",
	}
	delta := []string{"INSERT INTO t VALUES(9001, 1, 9999, 1, 1)", "UPDATE t SET v = v + 1000 WHERE id % 97 = 0", "DELETE FROM t WHERE id % 89 = 0"}
	queries := []string{
		"SELECT count(DISTINCT v), sum(DISTINCT v), total(DISTINCT v), avg(DISTINCT v) FROM t",
		"SELECT min(DISTINCT v), max(DISTINCT v), count(DISTINCT k) FROM t",
		"SELECT count(DISTINCT v % 7), sum(DISTINCT k * 3) FROM t WHERE v > 0",
		"SELECT count(DISTINCT id), sum(DISTINCT id) FROM t WHERE k < 3",
		"SELECT sum(DISTINCT v), count(DISTINCT v) FROM t WHERE v > 100000",
		"SELECT count(DISTINCT n), sum(DISTINCT n) FROM t",
		"SELECT sum(DISTINCT big) FROM t",
		"SELECT avg(DISTINCT big), total(DISTINCT big) FROM t",
	}
	for _, withDelta := range []bool{false, true} {
		for _, q := range queries {
			stmts := append([]string(nil), setup...)
			if withDelta {
				stmts = append(stmts, delta...)
			}
			aggOrderCheck(t, aggOrderCase{q, append(stmts, q), true})
		}
	}
}
