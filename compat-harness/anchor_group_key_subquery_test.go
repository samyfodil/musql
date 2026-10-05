package compat

import "testing"

// Aggregate anchor guard with subqueries in select list, HAVING, or ORDER BY.
func TestAnchorGuardGroupKeyOnlySubquery(t *testing.T) {
	setup := []string{
		"CREATE TABLE t6(a TEXT UNIQUE, b TEXT)",
		"INSERT INTO t6(a,b) VALUES('uvw','xyz'),('abc','def')",
	}
	differ(t, "HAVING subquery reads only the GROUP BY key", append(append([]string{}, setup...),
		"WITH v1(a) AS (SELECT a COLLATE NOCASE FROM t6)"+
			" SELECT v1.a, count(*) FROM t6 LEFT JOIN v1 ON true"+
			" GROUP BY 1"+
			" HAVING (SELECT true FROM t6 AS aa LEFT JOIN t6 AS bb ON length(v1.a)>5)",
	))
	differ(t, "select-list subquery reads only the GROUP BY key", append(append([]string{}, setup...),
		"SELECT a, count(*), (SELECT count(*) FROM t6 AS z WHERE z.a=t6.a) FROM t6 GROUP BY a ORDER BY 1",
	))
	// Real column named "true" shadows the keyword.
	differ(t, "a real column named true still shadows the keyword", []string{
		"CREATE TABLE tb(\"true\" INT, g INT)",
		"INSERT INTO tb VALUES(7,1),(8,1),(9,2)",
		"SELECT g, count(*) FROM tb GROUP BY g HAVING (SELECT true) ORDER BY 1",
		"SELECT g, count(*), true FROM tb GROUP BY g ORDER BY 1",
	})
}
