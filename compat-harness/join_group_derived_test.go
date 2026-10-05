package compat

import "testing"

// TestJoinGroupDerivedCorpusStatements tests NATURAL/USING joins
// combined with unaliased derived tables.
func TestJoinGroupDerivedCorpusStatements(t *testing.T) {
	// joinH.test 16.0/16.3.2: expects "1 {}" (t2.c0=1 coalesced, t0_b.c0
	// NULL for the unmatched row).
	differ(t, "joinH-16.3.2", []string{
		"CREATE TABLE t0_a (c0 INT)",
		"CREATE TABLE t0_b (c0 INT)",
		"CREATE TABLE t2 (c0 INT)",
		"INSERT INTO t2 VALUES (1)",
		"SELECT * FROM (t0_a RIGHT JOIN (SELECT * FROM t2 LEFT JOIN t0_b) USING (c0))",
	})

	// joinH.test 16.4.0/16.4.2: expects "blue red".
	differ(t, "joinH-16.4.2", []string{
		"CREATE TABLE x0(a TEXT)",
		"CREATE TABLE x1(a TEXT)",
		"CREATE TABLE x2(a TEXT)",
		"INSERT INTO x1 VALUES('blue')",
		"INSERT INTO x2 VALUES('red')",
		"SELECT * FROM x0 RIGHT JOIN (SELECT * FROM x1, x2) USING (a)",
	})
}
