// Tests "SELECT *" expansion over unaliased derived tables,
// ensuring columns bind correctly to their source.
package compat

import "testing"

func TestStarOverUnaliasedDerivedTable(t *testing.T) {
	// Identically-shaped tables with colliding column names.
	differ(t, "star over unaliased derived, colliding names", []string{
		`CREATE TABLE t23(a, b, c)`,
		`CREATE TABLE t24(a, b, c)`,
		`INSERT INTO t23 VALUES(1, 2, 3)`,
		`SELECT * FROM t23 LEFT JOIN t24`,
		`SELECT * FROM t23 LEFT JOIN (SELECT * FROM t24)`,
		`SELECT * FROM t23, (SELECT * FROM t24)`,
		`SELECT * FROM t23 JOIN (SELECT * FROM t24)`,
		`SELECT * FROM (SELECT * FROM t24) LEFT JOIN t23`,
		`SELECT * FROM (SELECT * FROM t23) LEFT JOIN (SELECT * FROM t24)`,
	})
	// With distinguishable values to detect misbound columns.
	differ(t, "star over unaliased derived, distinguishable values", []string{
		`CREATE TABLE l(a, b)`,
		`CREATE TABLE r(a, b)`,
		`INSERT INTO l VALUES(1, 2), (3, 4)`,
		`INSERT INTO r VALUES(10, 20), (30, 40)`,
		`SELECT * FROM l, (SELECT * FROM r) ORDER BY 1, 3`,
		`SELECT * FROM l LEFT JOIN (SELECT * FROM r) ORDER BY 1, 3`,
		`SELECT * FROM (SELECT * FROM r) LEFT JOIN l ORDER BY 1, 3`,
		`SELECT * FROM (SELECT a+100 AS a, b FROM r) , l ORDER BY 1, 3`,
		// A three-way FROM: the pinned item is neither first nor last.
		`SELECT * FROM l, (SELECT * FROM r), l AS l2 ORDER BY 1, 3, 5`,
	})
	// Aliased derived tables must still work identically.
	differ(t, "star over aliased derived still works", []string{
		`CREATE TABLE l(a, b)`,
		`CREATE TABLE r(a, b)`,
		`INSERT INTO l VALUES(1, 2)`,
		`INSERT INTO r VALUES(10, 20)`,
		`SELECT * FROM l, (SELECT * FROM r) AS d`,
		`SELECT d.* FROM l, (SELECT * FROM r) AS d`,
		`SELECT l.* FROM l, (SELECT * FROM r) AS d`,
	})
	// USING/NATURAL joins over unaliased derived tables.
	differ(t, "star over unaliased derived with USING", []string{
		`CREATE TABLE l(a, b)`,
		`CREATE TABLE r(a, c)`,
		`INSERT INTO l VALUES(1, 2), (3, 4)`,
		`INSERT INTO r VALUES(1, 30), (9, 90)`,
		`SELECT * FROM l JOIN (SELECT * FROM r) USING(a) ORDER BY 1`,
		`SELECT * FROM l NATURAL JOIN (SELECT * FROM r) ORDER BY 1`,
		`SELECT * FROM l LEFT JOIN (SELECT * FROM r) USING(a) ORDER BY 1`,
	})
	// Columns unique to the derived table remain reachable unqualified.
	differ(t, "unqualified reference unaffected", []string{
		`CREATE TABLE l(a, b)`,
		`CREATE TABLE r(a, z)`,
		`INSERT INTO l VALUES(1, 2)`,
		`INSERT INTO r VALUES(10, 20)`,
		`SELECT z FROM l, (SELECT * FROM r)`,
		`SELECT *, z FROM l, (SELECT * FROM r)`,
		// Ambiguous references still error as expected.
		`SELECT a FROM l, (SELECT * FROM r)`,
	})
}
