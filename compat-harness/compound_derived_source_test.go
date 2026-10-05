package compat

// Tests derived tables inside compound SELECT arms.

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// compoundDerivedSchema is shaped so the two arms overlap (so UNION really
// dedups and INTERSECT/EXCEPT really select), and so several rows TIE on the
// ordering column -- a tie is what makes an inner ORDER BY's row order
// genuinely undefined, and it must still not change the answer.
var compoundDerivedSchema = []string{
	"CREATE TABLE t1(a INTEGER, b TEXT)",
	"CREATE TABLE t2(c INTEGER, d TEXT)",
	"INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z'),(2,'y'),(5,'tie'),(5,'tie2')",
	"INSERT INTO t2 VALUES(2,'y'),(4,'w'),(5,'tie'),(6,'v')",
	"CREATE VIEW v1 AS SELECT a, b FROM t1 ORDER BY a DESC",
	"CREATE VIEW v2 AS SELECT c AS a, d AS b FROM t2 ORDER BY c",
}

func TestCompoundDerivedArm(t *testing.T) {
	inners := []string{
		"SELECT a, b FROM t1 ORDER BY a",
		"SELECT a, b FROM t1 ORDER BY a DESC",
		"SELECT a, b FROM t1 ORDER BY b, a",
		"SELECT a, b FROM t1 WHERE a > 1 ORDER BY a",
		"SELECT a, b FROM t1 GROUP BY a, b ORDER BY a",
		"SELECT a, b FROM t1", // the no-ORDER-BY control, already supported
	}
	ops := []string{"UNION", "UNION ALL", "INTERSECT", "EXCEPT"}
	tails := []string{"", " ORDER BY 1", " ORDER BY 1 DESC, 2", " ORDER BY 2, 1"}

	for _, inner := range inners {
		for _, op := range ops {
			for _, tail := range tails {
				for _, q := range []string{
					// The derived table in the FIRST arm, and in the SECOND.
					fmt.Sprintf("SELECT * FROM (%s) %s SELECT c, d FROM t2%s", inner, op, tail),
					fmt.Sprintf("SELECT c, d FROM t2 %s SELECT * FROM (%s)%s", op, inner, tail),
					// Both arms derived.
					fmt.Sprintf("SELECT * FROM (%s) %s SELECT * FROM (SELECT c, d FROM t2 ORDER BY c DESC)%s", inner, op, tail),
					// Aliased, and with an outer WHERE over the derived source.
					fmt.Sprintf("SELECT z.a, z.b FROM (%s) AS z WHERE z.a <> 3 %s SELECT c, d FROM t2%s", inner, op, tail),
					// A CTE reference is the same row source by another name.
					fmt.Sprintf("WITH cte AS (%s) SELECT * FROM cte %s SELECT c, d FROM t2%s", inner, op, tail),
				} {
					if !differ(t, "cmpderived", append(append([]string(nil), compoundDerivedSchema...), q)) {
						t.Errorf("diverged on: %s", q)
					}
				}
			}
		}
	}
}

// TestCompoundDerivedView covers the VIEW spelling, whose inner ORDER BY lives
// in the view's DDL rather than in the statement's own text -- the case that
// most clearly is not something a reader of the query could call order-bearing.
func TestCompoundDerivedView(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM v1 UNION SELECT * FROM v2",
		"SELECT * FROM v1 UNION ALL SELECT * FROM v2 ORDER BY 1, 2",
		"SELECT * FROM v1 INTERSECT SELECT * FROM v2",
		"SELECT * FROM v1 EXCEPT SELECT * FROM v2 ORDER BY 1",
		"SELECT * FROM v1 UNION SELECT a, b FROM t1 UNION ALL SELECT * FROM v2 ORDER BY 1 DESC",
	} {
		if !differ(t, "cmpview", append(append([]string(nil), compoundDerivedSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestCompoundDerivedLimit covers the LIMIT/OFFSET half, which used to be
// declined on the theory that an inner LIMIT decides WHICH rows survive and is
// ambiguous under a tie in the inner ORDER BY. The ambiguity is real, but it is
// not something the COMPOUND introduces: the identical derived table answers
// outside one already ("SELECT * FROM (SELECT a,b FROM t1 ORDER BY a LIMIT 3)"
// has always compiled), so declining it only when a UNION happens to sit next
// to it protected nothing that was not already exposed. These rows tie on the
// ordering column on purpose -- compoundDerivedSchema's (5,'tie')/(5,'tie2')
// pair -- so a genuine tie-order disagreement fails here.
func TestCompoundDerivedLimit(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM (SELECT a, b FROM t1 ORDER BY a LIMIT 3) UNION SELECT c, d FROM t2",
		"SELECT * FROM (SELECT a, b FROM t1 ORDER BY a LIMIT 3 OFFSET 1) UNION ALL SELECT c, d FROM t2",
		"SELECT c, d FROM t2 EXCEPT SELECT * FROM (SELECT a, b FROM t1 LIMIT 2)",
		"WITH cte AS (SELECT a, b FROM t1 ORDER BY a LIMIT 2) SELECT * FROM cte UNION SELECT c, d FROM t2",
		// The bare (non-compound) spelling of the same derived table, which
		// has always been answered -- this is the control that makes the
		// argument above checkable rather than asserted.
		"SELECT * FROM (SELECT a, b FROM t1 ORDER BY a LIMIT 3)",
	} {
		if !differ(t, "cmpderivedlimit", append(append([]string(nil), compoundDerivedSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
