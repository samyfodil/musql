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
