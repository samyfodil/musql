// This file gates aggregates inside WINDOW PARTITION BY / ORDER BY clauses.
// The per-item collector must walk a call's window spec, not just its arguments.
package compat

import "testing"

// TestAggItemWindowSpecHoistCorpus is the corpus statement that motivated this fix.
func TestAggItemWindowSpecHoistCorpus(t *testing.T) {
	flLockstep(t, "window1-55.1", []string{`CREATE TABLE a(b)`},
		`SELECT
      (SELECT b FROM a
        GROUP BY b
        HAVING (SELECT COUNT()OVER() + lead(b)OVER(ORDER BY SUM(DISTINCT b) + b))
      )
    FROM a
  UNION
   SELECT 99
    ORDER BY 1`)
}

// TestAggItemWindowSpecHoist is the family around it. Every statement here
// answers in 3.53.3; each was "no such column" (surfacing as "aggregate result
// expression") before the spec walk existed, EXCEPT where noted -- the two
// order/HAVING spellings and the whole-table owner are here because the same
// collector serves them.
func TestAggItemWindowSpecHoist(t *testing.T) {
	setup := []string{
		`CREATE TABLE o(k, v)`,
		`INSERT INTO o VALUES(1,10),(1,20),(2,30)`,
		`CREATE TABLE t(k, v, s)`,
		`INSERT INTO t VALUES(1,10,'a'),(1,20,'b'),(2,30,'c')`,
		`CREATE TABLE b(n)`,
		`INSERT INTO b VALUES(1),(2),(3)`,
	}
	flLockstep(t, "window-spec aggregate hoisted to the enclosing group", setup,
		// The aggregate names THIS query's own column, so it is this query's:
		// qualified and unqualified spellings, ORDER BY and PARTITION BY keys.
		`SELECT k, (SELECT lead(n) OVER (ORDER BY sum(t.v)) FROM b LIMIT 1) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT lead(1) OVER (ORDER BY sum(v))) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT lead(1) OVER (PARTITION BY sum(v))) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT lead(1) OVER (ORDER BY sum(v) FILTER (WHERE v>10))) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT lead(1) OVER (ORDER BY SUM(DISTINCT v))) FROM t GROUP BY k ORDER BY k`,
		// ...through a FURTHER-OUT level (recordSpec's escalation), and one
		// nested a second subquery deep.
		`SELECT o.k, (SELECT lead(1) OVER (ORDER BY sum(o.v)) FROM b LIMIT 1) FROM o GROUP BY o.k ORDER BY 1`,
		`SELECT o.k, (SELECT (SELECT lead(1) OVER (ORDER BY sum(o.v))) FROM b LIMIT 1) FROM o GROUP BY o.k ORDER BY 1`,
		// The item is HAVING, and the item is an ORDER BY term.
		`SELECT k FROM t GROUP BY k HAVING (SELECT lead(1) OVER (ORDER BY sum(v))) IS NULL ORDER BY k`,
		`SELECT k FROM t GROUP BY k ORDER BY (SELECT lead(1) OVER (ORDER BY sum(v))), k`,
		// A WHOLE-TABLE aggregate owner, no GROUP BY.
		`SELECT sum(v), (SELECT lead(1) OVER (ORDER BY sum(v))) FROM t`,
		// CONTROLS, all of which already answered: the spec's aggregate names no
		// column of this query at all; the spec has no aggregate; the window
		// call is itself the aggregate.
		`SELECT k, (SELECT lead(1) OVER (ORDER BY count(*))) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT lead(t.v) OVER (ORDER BY 1)) FROM t GROUP BY k ORDER BY k`,
		`SELECT k, (SELECT sum(t.v) OVER ()) FROM t GROUP BY k ORDER BY k`,
	)
}

// TestAggItemWindowSpecHoistUnplaceableStays is the ALL-OR-NOTHING half, and
// it is a REGRESSION gate rather than a new capability: rowvalue.test 30.1
// answers here and in 3.53.3 with the spec's aggregate left exactly where it is
// written, and a first cut of the spec walk turned it into a decline. Its
// argument is a SUBQUERY, which callOwnedByScope cannot see into
// (aggArgHasLocalColumnRef stops at a SubqueryExpr), so recordSpec can neither
// place the call nor escalate it -- and a spec walk that ends up blocked must
// put the collector back exactly as it found it.
func TestAggItemWindowSpecHoistUnplaceableStays(t *testing.T) {
	flLockstep(t, "rowvalue-30.1", []string{
		`CREATE TABLE t1(x, y, z)`,
		`CREATE TABLE t2(a, b)`,
		`INSERT INTO t1 VALUES(1000, 2000, 3000)`,
		`INSERT INTO t2 VALUES(NULL, NULL)`,
		`UPDATE t2 SET (a,b)=(
    SELECT max( t1.x ) OVER( PARTITION BY sum( (SELECT t1.y) ) ), 2
  )
  FROM t1`,
	},
		`SELECT * FROM t2`,
		`SELECT max(t1.x) OVER (PARTITION BY sum((SELECT t1.y))) FROM t1`,
		`SELECT max(x) OVER (PARTITION BY sum((SELECT y))) FROM t1`,
		`SELECT max(t1.x) OVER (PARTITION BY sum((SELECT t1.y))) FROM t1, t2`,
	)
}
