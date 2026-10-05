package compat

import "testing"

// TestResultAliasIntoNestedFromSubquery pins lookupName's walk outward for a
// result alias named inside a subquery that has a FROM of its own: the
// subquery's FROM wins a name it offers, and otherwise the alias of the
// enclosing SELECT does (resolve.c:703-704, NC_UEList), including an alias
// whose expression is itself a subquery naming only its own columns
// (existsexpr.test 10.2).
func TestResultAliasIntoNestedFromSubquery(t *testing.T) {
	differ(t, "result alias into a nested FROM", []string{
		"CREATE TABLE t1(a)",
		"CREATE TABLE x1(x)",
		"SELECT EXISTS( SELECT 1 FROM t1 ) aaa FROM x1 WHERE aaa AND aaa",
		"SELECT EXISTS( SELECT 1 FROM t1 ) aaa WHERE ( SELECT 1 FROM x1 WHERE aaa AND aaa )",
		"INSERT INTO t1 VALUES(5)",
		"INSERT INTO x1 VALUES(7)",
		"SELECT EXISTS( SELECT 1 FROM t1 ) aaa WHERE ( SELECT 1 FROM x1 WHERE aaa AND aaa )",
		"SELECT 2 AS x WHERE (SELECT 1 FROM x1 WHERE x=7)",
		"SELECT 2 AS y WHERE (SELECT 1 FROM x1 WHERE y=2)",
		"SELECT 2 AS y WHERE (SELECT y FROM x1)",
		"SELECT 2 AS y WHERE (SELECT 1 FROM x1 JOIN t1 ON y=2)",
		"SELECT (SELECT count(*) FROM t1) AS n WHERE (SELECT n FROM x1)",
		"SELECT (SELECT max(a) FROM t1) AS n WHERE (SELECT n=5 FROM x1)",
		"SELECT 2 AS rowid WHERE (SELECT 1 FROM x1 WHERE rowid=2)",
		"WITH t1(a) AS (SELECT 9) SELECT (SELECT max(a) FROM t1) AS n WHERE (SELECT n FROM x1)",
		"SELECT (SELECT max(a) FROM t1) AS n FROM x1 WHERE (SELECT n+x FROM t1) = 12",
		"SELECT 3 AS a WHERE (SELECT a FROM t1) = 5",
		"SELECT (SELECT max(t1.a)+1 FROM t1) AS n WHERE (SELECT count(*) FROM x1 WHERE n=6)",
		"SELECT (SELECT max(x) FROM t1) AS n FROM x1 WHERE (SELECT n FROM t1)",
	})
}
