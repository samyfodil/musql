// This file tests UPDATE statements with SET subqueries that read the target
// table. Uncorrelated subqueries use a pre-update snapshot; correlated subqueries
// read the live, partially-updated table.
package compat

import "testing"

func TestUpdateSetSubqueryUncorrelatedOverTargetTable(t *testing.T) {
	differ(t, "uncorrelated SET subquery over the target table", []string{
		`CREATE TABLE u(a,b)`,
		`INSERT INTO u VALUES(1,10),(2,20),(3,30)`,
		// A whole-table aggregate: every row must get the PRE-update value, so
		// a live re-read would show as 30,50,80 rather than 30,30,30.
		`UPDATE u SET b=(SELECT sum(b) FROM u)`,
		`SELECT a,b FROM u ORDER BY a`,
	})
	differ(t, "uncorrelated SET subquery with a filter", []string{
		`CREATE TABLE v(a,b)`,
		`INSERT INTO v VALUES(1,10),(2,20),(3,30)`,
		`UPDATE v SET b=(SELECT count(*) FROM v WHERE b>15)`,
		`SELECT a,b FROM v ORDER BY a`,
	})
	differ(t, "uncorrelated SET subquery, partial update", []string{
		`CREATE TABLE w(a,b)`,
		`INSERT INTO w VALUES(1,1),(2,2),(3,3)`,
		`UPDATE w SET b = (SELECT max(b) FROM w) WHERE a<=2`,
		`SELECT a,b FROM w ORDER BY a`,
		// A single-row read of another row of the same table.
		`UPDATE w SET b = (SELECT b FROM w WHERE a=1) WHERE a=3`,
		`SELECT a,b FROM w ORDER BY a`,
	})
	// update.test 5's own statement: two SET terms, one of them reading the
	// target through an aggregate, with a WHERE that leaves later rows alone.
	// A live re-read would make row 2's y come out 1 instead of 0.
	differ(t, "update.test's two-term SET over the target", []string{
		`CREATE TABLE t1(x,y)`,
		`INSERT INTO t1 VALUES(1,0),(2,0),(3,0),(4,0)`,
		`UPDATE t1 SET x=x+100, y=x<=(SELECT min(x) FROM t1) WHERE x<3`,
		`SELECT x,y FROM t1 ORDER BY rowid`,
	})
	// A qualifier the subquery's own FROM DOES introduce is not a reach-out --
	// the inner "z" shadows the outer one, so "z.a" means the inner row and
	// "a<z.a" is never true.
	differ(t, "a qualifier the subquery introduces itself", []string{
		`CREATE TABLE z(a,b)`,
		`INSERT INTO z VALUES(1,1),(2,2),(3,3)`,
		`UPDATE z SET b = b + (SELECT sum(b) FROM z WHERE a<z.a)`,
		`SELECT a,b FROM z ORDER BY a`,
	})
	// A SET subquery over ANOTHER table was never declined and must stay that
	// way, correlated or not.
	differ(t, "SET subquery over another table is unaffected", []string{
		`CREATE TABLE t(x,y)`,
		`CREATE TABLE o(k,v)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`INSERT INTO o VALUES(1,'one'),(2,'two')`,
		`UPDATE t SET y=(SELECT v FROM o WHERE o.k=t.x)`,
		`SELECT x,y FROM t ORDER BY x`,
	})
}

// TestUpdateSetSubqueryCorrelatedMatchesCSQLite tests correlated subqueries
// against the live, partially-updated table.
func TestUpdateSetSubqueryCorrelatedMatchesCSQLite(t *testing.T) {
	for _, tc := range []struct{ name, stmt string }{
		// The inner FROM aliases t to t2, so "t.x" can only mean the outer row.
		// cgo answers 2,4,5.
		{"aliased self-join", `UPDATE t SET x = x + (SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`},
		// The same reach written through EXISTS and through IN.
		{"exists", `UPDATE t SET x = x + (SELECT count(*) FROM t t2 WHERE EXISTS(SELECT 1 FROM t t3 WHERE t3.x=t.x))`},
		{"in", `UPDATE t SET x = x + (SELECT count(*) FROM t t2 WHERE t2.x IN (SELECT t.x))`},
		// ...and one level deeper, inside a derived table.
		{"nested derived", `UPDATE t SET x = x + (SELECT count(*) FROM (SELECT x FROM t t2 WHERE t2.x<=t.x))`},
	} {
		differ(t, "correlated SET over the target: "+tc.name, []string{
			`CREATE TABLE t(x)`,
			`INSERT INTO t VALUES(1),(2),(3)`,
			tc.stmt,
			`SELECT x FROM t ORDER BY rowid`,
		})
	}
}
