// Aggregate item with USING join: declined shapes must match C's.
// Every statement here was verified directly against mattn/go-sqlite3 3.53.3.
package compat

import "testing"

// TestAggItemUsingJoinCommonColumn is the reported shape. C SQLite answers
// it [1 1 2]; before the fix this engine answered nothing at all.
func TestAggItemUsingJoinCommonColumn(t *testing.T) {
	setup := []string{
		`CREATE TABLE b(n)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
		`CREATE TABLE j1(g, m)`,
		`INSERT INTO j1 VALUES(1,10),(2,20)`,
		`CREATE TABLE j2(g, q)`,
		`INSERT INTO j2 VALUES(1,100),(2,200)`,
	}
	with := func(stmts ...string) []string {
		return append(append([]string{}, setup...), stmts...)
	}
	differ(t, "USING join, subquery correlates to the common column", with(
		`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1 JOIN j2 USING(g) GROUP BY g`))
	differ(t, "USING join, common column reached through a QUALIFIED name", with(
		`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > j2.q) FROM j1 JOIN j2 USING(g) GROUP BY g`))
	differ(t, "NATURAL join, same shape: the common column is paired the same way", with(
		`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1 NATURAL JOIN j2 GROUP BY g`))
	differ(t, "LEFT JOIN ... USING: still the left-most table (resolve.c:447-449)", with(
		`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1 LEFT JOIN j2 USING(g) GROUP BY g`))
	differ(t, "USING join with the common column in HAVING's subquery", with(
		`SELECT g, count(*) FROM j1 JOIN j2 USING(g) GROUP BY g HAVING (SELECT count(*) FROM b WHERE n > g) > 1`))
	differ(t, "USING(g) plus an ordinary correlated reference to a non-common column", with(
		`SELECT g, count(*), (SELECT count(*) FROM b WHERE n > m) FROM j1 JOIN j2 USING(g) GROUP BY g`))
}

// TestAggItemPlainJoinAmbiguityStillErrors is the other side, and it is the
// one that keeps the fix honest: with no USING clause pairing them, two
// same-named columns are genuinely AMBIGUOUS, C lets cnt go above 1
// (resolve.c:438-443) and reports "ambiguous column name" (resolve.c:784).
// Answering here would be a wrong answer, not a wider capability.
func TestAggItemPlainJoinAmbiguityStillErrors(t *testing.T) {
	setup := []string{
		`CREATE TABLE b(n)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
		`CREATE TABLE j1(g, m)`,
		`INSERT INTO j1 VALUES(1,10),(2,20)`,
		`CREATE TABLE j2(g, q)`,
		`INSERT INTO j2 VALUES(1,100),(2,200)`,
	}
	with := func(stmts ...string) []string {
		return append(append([]string{}, setup...), stmts...)
	}
	differ(t, "plain join, both tables declare g: ambiguous, and both engines must refuse", with(
		`SELECT j1.g, count(*), (SELECT count(*) FROM b WHERE n > g) FROM j1, j2 GROUP BY j1.g`))
	differ(t, "CONTROL: the same plain join with an unambiguous reference answers", with(
		`SELECT j1.g, count(*), (SELECT count(*) FROM b WHERE n > j1.m) FROM j1, j2 GROUP BY j1.g`))
}
