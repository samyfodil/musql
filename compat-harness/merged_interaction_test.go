package compat

// Tests shapes involving derived tables in compound queries and NATURAL/USING joins.

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var mergedInteractionSchema = []string{
	"CREATE TABLE t1(a INTEGER, b TEXT)",
	"CREATE TABLE t2(a INTEGER, c TEXT)",
	"CREATE TABLE t3(a INTEGER, b TEXT)",
	"INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z'),(2,'y')",
	"INSERT INTO t2 VALUES(2,'p'),(3,'q'),(4,'r')",
	"INSERT INTO t3 VALUES(2,'y'),(5,'m')",
	"CREATE VIEW v2 AS SELECT a, c FROM t2 ORDER BY a DESC",
}

func TestMergedDerivedJoinInCompoundArm(t *testing.T) {
	arms := []string{
		// Unaliased derived tables with NATURAL/USING joins.
		"SELECT * FROM t1 NATURAL JOIN (SELECT a, c FROM t2)",
		"SELECT * FROM t1 JOIN (SELECT a, c FROM t2) USING(a)",
		"SELECT * FROM (SELECT a, c FROM t2) NATURAL JOIN t1",
		// With inner ORDER BY in the derived table.
		"SELECT * FROM t1 NATURAL JOIN (SELECT a, c FROM t2 ORDER BY a DESC)",
		"SELECT * FROM t1 JOIN (SELECT a, c FROM t2 ORDER BY c) USING(a)",
		// Aliased, view, and CTE variants.
		"SELECT * FROM t1 NATURAL JOIN (SELECT a, c FROM t2) AS d",
		"SELECT * FROM t1 NATURAL JOIN v2",
		"SELECT * FROM t1 LEFT JOIN (SELECT a, c FROM t2 ORDER BY a) USING(a)",
		// Two unaliased derived sides.
		"SELECT * FROM (SELECT a, b FROM t1) NATURAL JOIN (SELECT a, c FROM t2)",
	}
	others := []string{
		"SELECT a, b FROM t3",
		"SELECT * FROM t3 NATURAL JOIN (SELECT a, b FROM t1)",
	}
	ops := []string{"UNION", "UNION ALL", "INTERSECT", "EXCEPT"}
	tails := []string{"", " ORDER BY 1", " ORDER BY 1 DESC, 2"}

	for _, arm := range arms {
		for _, other := range others {
			for _, op := range ops {
				for _, tail := range tails {
					for _, q := range []string{
						fmt.Sprintf("%s %s %s%s", arm, op, other, tail),
						fmt.Sprintf("%s %s %s%s", other, op, arm, tail),
					} {
						if !differ(t, "merged", append(append([]string(nil), mergedInteractionSchema...), q)) {
							t.Errorf("diverged on: %s", q)
						}
					}
				}
			}
		}
	}
}

// TestMergedDerivedJoinInCompoundLimitDeclined verifies LIMIT still declines in this context.
func TestMergedDerivedJoinInCompoundLimitDeclined(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t1 NATURAL JOIN (SELECT a, c FROM t2 ORDER BY a LIMIT 2) UNION SELECT a, b FROM t3",
		"SELECT a, b FROM t3 UNION ALL SELECT * FROM t1 JOIN (SELECT a, c FROM t2 LIMIT 1) USING(a)",
	} {
		res := run(t, "musql", append(append([]string(nil), mergedInteractionSchema...), q))
		last := res[len(res)-1]
		if last["kind"] != "error" {
			t.Errorf("expected a clean decline, got %v for: %s", last, q)
		}
	}
}
