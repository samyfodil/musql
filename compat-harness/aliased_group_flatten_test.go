// Gates that aliased and outward-ON/USING join groups must not be flattened
// into the FROM list, as their conditions must apply across all group members.
package compat

import "testing"

func TestAliasedAllCrossGroupFlattens(t *testing.T) {
	differ(t, "aliased-all-cross-group", []string{
		`CREATE TABLE t0(c0, c1)`,
		`CREATE TABLE t2(w)`,
		`CREATE TABLE t4(y)`,
		`CREATE TABLE t5(z)`,
		`INSERT INTO t0 VALUES(1,2)`,
		`INSERT INTO t0 VALUES(5,1)`,
		`INSERT INTO t2 VALUES(3)`,
		`INSERT INTO t4 VALUES(5)`,
		`INSERT INTO t5 VALUES(7)`,
		`SELECT 1234 FROM t4 RIGHT JOIN t5 CROSS JOIN (t2 CROSS JOIN t0) AS a1 ON (a1.c0 < a1.c1)`,
	})
}

func TestUnaliasedGroupOutwardOnFlattens(t *testing.T) {
	// Outward ON clause must apply across all group members.
	differ(t, "unaliased-group-outward-on", []string{
		`CREATE TABLE v1(x)`,
		`CREATE TABLE v2(y)`,
		`CREATE TABLE t0(c0, c1)`,
		`INSERT INTO v1 VALUES(1)`,
		`INSERT INTO v2 VALUES(2)`,
		`INSERT INTO t0 VALUES(1,2)`,
		`INSERT INTO t0 VALUES(5,1)`,
		`SELECT * FROM v1 INNER JOIN (v2 CROSS JOIN t0) ON (t0.c0 < t0.c1)`,
	})
}
