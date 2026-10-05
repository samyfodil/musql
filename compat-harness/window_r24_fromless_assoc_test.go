// Aggregates in FROM-less window subqueries; association with enclosing query.
package compat

import "testing"

func TestWindowR24FromlessAggregateAssociation(t *testing.T) {
	differ(t, "aggregate hoists out of a from-less window subquery", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`SELECT (SELECT max(a) + row_number() OVER ()) FROM t1`,
		`SELECT (SELECT min(a) + row_number() OVER ()) FROM t1`,
		`SELECT (SELECT sum(a) + row_number() OVER ()) FROM t1`,
		`SELECT (SELECT total(a) + ntile(1) OVER ()) FROM t1`,
		`SELECT a, (SELECT max(a) + row_number() OVER ()) FROM t1`,
		`SELECT a, (SELECT sum(a) + row_number() OVER ()) FROM t1`,
		`SELECT (SELECT max(a) + count(*) OVER ()) FROM t1`,
		`SELECT (SELECT count(a) OVER ()) FROM t1`,
		`SELECT (SELECT a + row_number() OVER ()) FROM t1`,
	})

	differ(t, "aggregates split across two enclosing levels", []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(x, y, z)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO t2 VALUES(1000,2000,3000),(7,8,9)`,
		`SELECT (SELECT (SELECT sum(a) + sum(x) + row_number() OVER ()) FROM t2) FROM t1`,
		`SELECT (SELECT (SELECT sum(a) + sum(x)) FROM t2) FROM t1`,
		`SELECT (SELECT (SELECT sum(a) + row_number() OVER ()) FROM t2) FROM t1`,
	})

	differ(t, "window1.test 44's from-less MIN beside a window", []string{
		`CREATE TABLE t0(c0)`,
		`SELECT (SELECT MIN(c0) + NTILE(1) OVER()) FROM t0`,
		`INSERT INTO t0 VALUES(2), (1), (0)`,
		`SELECT (SELECT MIN(c0) + NTILE(1) OVER()) FROM t0`,
		`SELECT (SELECT count(c0) + row_number() OVER ()) FROM t0`,
		`SELECT c0, (SELECT count(c0) + row_number() OVER ()) FROM t0`,
	})

	differ(t, "window1.test 44's row-value IN, hoisted", []string{
		`CREATE TABLE t0(c0)`,
		`SELECT (0, 0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`INSERT INTO t0 VALUES(2), (1), (0)`,
		`SELECT (0, 0) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT (0, 1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
		`SELECT c0, (0,1) IN(SELECT MIN(c0), NTILE(1) OVER()) FROM t0`,
	})

	differ(t, "aggregate that names nothing stays in the subquery", []string{
		`CREATE TABLE t1(x, y, z)`,
		`INSERT INTO t1 VALUES(1000, 2000, 3000),(7, 8, 9)`,
		`SELECT (SELECT sum(1) + count(*) OVER ()) FROM t1`,
		`SELECT x, (SELECT sum(1) + count(*) OVER ()) FROM t1`,
		`SELECT x, (SELECT max(2) OVER (PARTITION BY sum(3))) FROM t1`,
		`SELECT x, (SELECT group_concat('q') + row_number() OVER ()) FROM t1`,
	})

	differ(t, "a window call's own argument is a correlated column", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`SELECT (SELECT sum(a) + count(a) OVER ()) FROM t1`,
		`SELECT (SELECT count(a) OVER ( ORDER BY sum(a) ) + total(a) OVER()) FROM t1`,
		`SELECT (SELECT count(a) OVER (PARTITION BY sum(a))) FROM t1`,
		`SELECT count(*), (SELECT a + row_number() OVER ()) FROM t1`,
	})

	differ(t, "window4.test 12.3: unqualified mover inside a compound arm", []string{
		`CREATE TABLE t2(a INTEGER)`,
		`INSERT INTO t2 VALUES(1), (2), (3)`,
		`SELECT (SELECT avg(a) UNION SELECT min(a) OVER ()) FROM t2 GROUP BY a ORDER BY 1`,
	})

	differ(t, "colname.test 9.330: non-degenerate windowed-arm anchor", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(17),(2),(99),(-3),(7)`,
		`SELECT (SELECT avg(a) UNION SELECT min(a) OVER()) FROM t1`,
	})

	differ(t, "top-level and FROM-ful spellings", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,50),(1,10),(2,1),(2,20),(3,30)`,
		`SELECT count(*) AS c, row_number() OVER () AS r`,
		`SELECT max(1) AS m, ntile(1) OVER () AS n`,
		`SELECT min(7) AS m, row_number() OVER () AS r`,
		`SELECT sum(3) AS s, ntile(1) OVER () AS n`,
		`SELECT group_concat('a') AS g, row_number() OVER () AS r`,
		`SELECT total(2) AS t, rank() OVER (ORDER BY 1) AS rk`,
		`SELECT sum(b) AS s, count(*) OVER () AS c FROM t1`,
		`SELECT a, sum(b) OVER () FROM t1 GROUP BY a`,
		`SELECT sum(b) OVER (ORDER BY sum(a)) FROM t1`,
	})

	differ(t, "unaliased derived-table FROM stays local, not hoisted", []string{
		`CREATE TABLE t2(a INTEGER, x INTEGER)`,
		`INSERT INTO t2 VALUES(1,100),(2,200),(3,300)`,
		`SELECT a, (SELECT sum(x) FROM (SELECT 5 AS x)) AS v FROM t2 GROUP BY a ORDER BY a`,
	})
}

// TestWindowR24FromlessAggregateAssociationAliasedArm pins shapes with
// aliased arms in UNION queries; some still decline for safety reasons.
func TestWindowR24FromlessAggregateAssociationAliasedArm(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
	}
	for _, q := range []string{
		`SELECT sum(a) OVER( ORDER BY ( SELECT max(a) OVER( ORDER BY sum( (SELECT a AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1`,
	} {
		differ(t, "window1.test 42.4 over a real column", append(append([]string{}, setup...), q))
	}
}
