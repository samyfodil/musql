// Gates LIMIT transfer in subquery flattening: a subquery LIMIT is applied after
// the enclosing ORDER BY when flattened.
package compat

import "testing"

func TestR35cFlattenSubqueryLimit(t *testing.T) {
	edb, cdb := r35cPair(t)
	r35cExec(t, edb, cdb,
		"CREATE TABLE t(a)",
		"INSERT INTO t VALUES(1),(2),(3),(4),(5)",
		"CREATE TABLE u(b)",
		"INSERT INTO u VALUES(10),(20)",
		"CREATE TABLE tc(a TEXT COLLATE nocase)",
		"INSERT INTO tc VALUES('a'),('B'),('c'),('D')",
	)
	for _, q := range []string{
		// LIMIT transfer: applies LIMIT after outer ORDER BY.
		"SELECT * FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC",
		"SELECT a FROM (SELECT a FROM t LIMIT 2) ORDER BY a DESC",
		"SELECT a*10 FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC",
		"SELECT a FROM (SELECT a FROM t LIMIT 2) AS s ORDER BY a DESC",
		"SELECT a FROM (SELECT a FROM t LIMIT 2) ORDER BY a",
		"SELECT a FROM (SELECT a FROM t LIMIT 0) ORDER BY a DESC",
		"SELECT a FROM (SELECT a FROM t LIMIT 99) ORDER BY a DESC",
		"SELECT a FROM (SELECT a FROM t WHERE a<>3 LIMIT 2) ORDER BY a DESC",
		"SELECT a FROM (SELECT a FROM tc LIMIT 2) ORDER BY a DESC",
		"SELECT a FROM (SELECT a FROM tc LIMIT 2) ORDER BY a COLLATE binary DESC",
		"SELECT a, a FROM (SELECT a FROM t LIMIT 2) ORDER BY 2 DESC, 1",
		// Nested subqueries: rewrite reaches inner levels.
		"SELECT * FROM (SELECT a FROM (SELECT a FROM t LIMIT 3) ORDER BY a DESC)",

		// Cases where LIMIT transfer does NOT happen.
		"SELECT * FROM (SELECT a FROM t ORDER BY a LIMIT 2) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT a FROM t LIMIT 2 OFFSET 1) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC LIMIT 9",
		"SELECT * FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC LIMIT 1 OFFSET 1",
		"SELECT * FROM (SELECT a FROM t LIMIT 2) WHERE a>0 ORDER BY 1 DESC",
		"SELECT DISTINCT a FROM (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC",
		"SELECT max(a) FROM (SELECT a FROM t LIMIT 2)",
		"SELECT a, count(*) FROM (SELECT a FROM t LIMIT 2) GROUP BY a ORDER BY 1 DESC",
		"SELECT * FROM (SELECT a FROM t LIMIT 2), u ORDER BY 1 DESC",
		"SELECT * FROM u, (SELECT a FROM t LIMIT 2) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT DISTINCT a FROM t LIMIT 2) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT 1 AS a LIMIT 1) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT a FROM t LIMIT 2) UNION ALL SELECT b FROM u ORDER BY 1 DESC",
		// No outer ORDER BY: unchanged behavior.
		"SELECT * FROM (SELECT a FROM t LIMIT 2)",
		"SELECT a*2 FROM (SELECT a FROM t LIMIT 2)",
		// LIMIT-free subquery: nothing to transfer.
		"SELECT * FROM (SELECT a FROM t) ORDER BY 1 DESC",
		"SELECT * FROM (SELECT a FROM t ORDER BY a DESC) ORDER BY 1",
	} {
		flCompareQuery(t, "r35cflat", edb, cdb, q)
	}
}
