package compat

import "testing"

// TestINSubqueryAffinityAndCollation tests affinity and collation handling in
// IN subqueries with compound SELECT bodies and various view/CTE shapes.
func TestINSubqueryAffinityAndCollation(t *testing.T) {
	setup := []string{
		"CREATE TABLE x2(b TEXT)", "CREATE TABLE x1(a TEXT)", "INSERT INTO x1 VALUES('123')",
		"CREATE VIEW vc AS SELECT b FROM x2 UNION SELECT 123",
		"CREATE TABLE tc(x TEXT COLLATE NOCASE, y TEXT)", "INSERT INTO tc VALUES('ABC','abc')",
		"CREATE VIEW vtc AS SELECT x FROM tc",
		"CREATE TABLE ti(n INTEGER)", "INSERT INTO ti VALUES(1)", "CREATE VIEW vti AS SELECT n FROM ti",
		"CREATE TABLE tt(s TEXT)", "INSERT INTO tt VALUES('1.0')",
	}
	queries := []string{
		"WITH c(x) AS (SELECT b FROM x2 UNION SELECT 123) SELECT count(*) FROM x1 WHERE a IN c",
		"WITH c(x) AS (SELECT 123 UNION SELECT b FROM x2) SELECT count(*) FROM x1 WHERE a IN c",
		"WITH c(x) AS (SELECT b FROM x2 UNION SELECT 123) SELECT count(*) FROM x1 WHERE a IN (SELECT x FROM c)",
		"WITH c(x) AS (SELECT b FROM x2 UNION SELECT 123) SELECT count(*) FROM x1, c WHERE a = x",
		"WITH c(x) AS (SELECT 123 UNION SELECT b FROM x2) SELECT count(*) FROM x1, c WHERE a = x",
		"WITH c AS (SELECT b FROM x2 UNION SELECT 123) SELECT count(*) FROM x1 WHERE a IN c",
		"WITH c(x) AS (SELECT b FROM x2 UNION ALL SELECT 123) SELECT count(*) FROM x1 WHERE a IN c",
		"WITH c(x) AS (SELECT b FROM x2 UNION SELECT '123') SELECT count(*) FROM x1 WHERE a IN c",
		"WITH c(x) AS (SELECT b||'' FROM x2 UNION SELECT 123) SELECT count(*) FROM x1 WHERE a IN c",
		"WITH s AS (VALUES(123), (456)) SELECT count(*) FROM x1 WHERE a IN s",
		"SELECT count(*) FROM x1 WHERE a IN (VALUES(123),(456))",
		"SELECT count(*) FROM x1 WHERE a IN (SELECT b FROM x2 UNION SELECT 123)",
		"SELECT count(*) FROM x1 WHERE a IN (SELECT 123 UNION SELECT b FROM x2)",
		"SELECT count(*) FROM x1 WHERE a IN vc",
		"SELECT count(*) FROM x1 WHERE a IN (SELECT * FROM vc)",
		"SELECT count(*) FROM x1 WHERE a IN (SELECT * FROM (SELECT b FROM x2 UNION SELECT 123))",
		"SELECT 'abc' IN (SELECT x FROM tc UNION ALL SELECT 'zzz')",
		"SELECT 'abc' IN (SELECT 'zzz' UNION ALL SELECT x FROM tc)",
		"WITH c AS (SELECT x FROM tc) SELECT 'abc' IN c",
		"SELECT 'abc' IN vtc",
		"SELECT 'abc' IN (SELECT x FROM (SELECT x FROM tc))",
		"SELECT 'abc' IN (SELECT x FROM vtc)",
		"SELECT count(*) FROM tt WHERE s IN vti",
		"SELECT count(*) FROM tt WHERE s IN (SELECT n FROM ti UNION SELECT 7)",
		"SELECT count(*) FROM tt WHERE s IN (SELECT 7 UNION SELECT n FROM ti)",
		"WITH c AS (SELECT n FROM ti) SELECT count(*) FROM tt WHERE s IN c",
		"SELECT count(*) FROM tt WHERE s IN (SELECT n FROM (SELECT n FROM ti))",
	}
	differ(t, "IN subquery affinity and collation", append(setup, queries...))
}
