// Tests window query result order with multiple window specifications.
package compat

import "testing"

func TestWindowR24MultiSpecEmitOrder(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(3,'c'),(1,'a'),(2,'b')`,
	}
	q := func(sql ...string) []string { return append(append([]string{}, setup...), sql...) }

	differ(t, "single spec", q(
		`SELECT a, sum(a) OVER (ORDER BY a) FROM t`,
		`SELECT a, count(*) OVER (ORDER BY b DESC) FROM t`,
		`SELECT a, sum(a) OVER (PARTITION BY a%2) FROM t`,
	))

	differ(t, "two specs, first written wins", q(
		`SELECT a, sum(a) OVER (ORDER BY a), count(*) OVER (ORDER BY b DESC) FROM t`,
		`SELECT a, count(*) OVER (ORDER BY b DESC), sum(a) OVER (ORDER BY a) FROM t`,
		`SELECT a, sum(a) OVER (ORDER BY a DESC), count(*) OVER (ORDER BY a) FROM t`,
	))

	differ(t, "empty first spec falls through to the inner one", q(
		`SELECT a, count(*) OVER (), sum(a) OVER (ORDER BY a) FROM t`,
		`SELECT a, sum(a) OVER (ORDER BY a), count(*) OVER () FROM t`,
		`SELECT a, count(*) OVER (), sum(a) OVER (ORDER BY b DESC) FROM t`,
	))

	differ(t, "partition over an inner order", q(
		`SELECT a, sum(a) OVER (PARTITION BY a%2), count(*) OVER (ORDER BY b DESC) FROM t`,
		`SELECT a, sum(a) OVER (PARTITION BY a%2), count(*) OVER (ORDER BY b) FROM t`,
		`SELECT a, count(*) OVER (ORDER BY b DESC), sum(a) OVER (PARTITION BY a%2) FROM t`,
	))

	differ(t, "three specs and a repeat", q(
		`SELECT a, sum(a) OVER (ORDER BY b), count(*) OVER (ORDER BY a DESC), max(a) OVER (PARTITION BY a) FROM t`,
		`SELECT a, sum(a) OVER (ORDER BY a), count(*) OVER (ORDER BY b DESC), min(a) OVER (ORDER BY a) FROM t`,
		`SELECT a, count(*) OVER (ORDER BY a DESC), sum(a) OVER (ORDER BY b), max(b) OVER (ORDER BY a DESC) FROM t`,
	))

	differ(t, "named windows", q(
		`SELECT a, sum(a) OVER w1, count(*) OVER w2 FROM t WINDOW w1 AS (ORDER BY a), w2 AS (ORDER BY b DESC)`,
		`SELECT a, sum(a) OVER w2, count(*) OVER w1 FROM t WINDOW w1 AS (ORDER BY a), w2 AS (ORDER BY b DESC)`,
	))

	differ(t, "limit and distinct over a multi-spec order", q(
		`SELECT a, sum(a) OVER (ORDER BY a), count(*) OVER (ORDER BY b DESC) FROM t LIMIT 2`,
		`SELECT a, count(*) OVER (ORDER BY b DESC), sum(a) OVER (ORDER BY a) FROM t LIMIT 2`,
		`SELECT a, count(*) OVER (), sum(a) OVER (ORDER BY a) FROM t LIMIT 1 OFFSET 1`,
		`SELECT DISTINCT a%2, count(*) OVER (ORDER BY a DESC), sum(a) OVER (ORDER BY a) FROM t`,
	))

	differ(t, "same keys different frames", q(
		`SELECT a, sum(a) OVER (ORDER BY a ROWS 1 PRECEDING), count(*) OVER (ORDER BY a) FROM t`,
		`SELECT a, sum(a) OVER (ORDER BY b DESC ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), count(*) OVER (ORDER BY a) FROM t`,
	))

	differ(t, "window in ORDER BY", q(
		`SELECT a, sum(a) OVER (ORDER BY b) FROM t ORDER BY count(*) OVER (ORDER BY a), a`,
	))

	// The nesting decides the outer level's VALUES, not only its row sequence:
	// with k tied on every row, only the inner window's order can break the tie,
	// and a frame/lag/row_number that walks the partition reads it.
	differ(t, "outer level values see the inner level's order", []string{
		`CREATE TABLE u(a,k,z)`,
		`INSERT INTO u VALUES(1,9,'c'),(2,9,'a'),(3,9,'b')`,
		`SELECT a, sum(a) OVER (ORDER BY k ROWS 1 PRECEDING) FROM u`,
		`SELECT a, sum(a) OVER (ORDER BY k ROWS 1 PRECEDING), count(*) OVER (ORDER BY z) FROM u`,
		`SELECT a, count(*) OVER (ORDER BY z), sum(a) OVER (ORDER BY k ROWS 1 PRECEDING) FROM u`,
		`SELECT a, lag(a) OVER (ORDER BY k), count(*) OVER (ORDER BY z) FROM u`,
		`SELECT a, row_number() OVER (ORDER BY k), count(*) OVER (ORDER BY z) FROM u`,
		`SELECT a, first_value(a) OVER (ORDER BY k), count(*) OVER (ORDER BY z DESC) FROM u`,
		`SELECT a, ntile(2) OVER (ORDER BY k), count(*) OVER (ORDER BY z) FROM u`,
		// ... and with a top-level ORDER BY whose key ties, so the emit order is
		// what breaks it.
		`SELECT a, sum(a) OVER (ORDER BY k ROWS 1 PRECEDING), count(*) OVER (ORDER BY z) FROM u ORDER BY k`,
	})

	// Collation on the level's own key: NOCASE partitions/orders must sort with
	// it, not BINARY.
	differ(t, "collated keys across levels", []string{
		`CREATE TABLE c(x TEXT COLLATE NOCASE, y)`,
		`INSERT INTO c VALUES('b',1),('A',2),('B',3),('a',4)`,
		`SELECT x, y, count(*) OVER (PARTITION BY x), sum(y) OVER (ORDER BY y DESC) FROM c`,
		`SELECT x, y, sum(y) OVER (ORDER BY y DESC), count(*) OVER (PARTITION BY x) FROM c`,
	})
}
