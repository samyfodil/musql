// This file gates qualified outer-query aggregates referenced from inside
// nested subqueries. Unqualified references and min()/max() are not covered.
package compat

import "testing"

func TestOuterAggInSubqueryWhere_Qualified(t *testing.T) {
	differ(t, "outer agg in subquery WHERE, qualified reference", []string{
		`CREATE TABLE t1(id1, value1)`,
		`CREATE TABLE t2(value2)`,
		`INSERT INTO t1 VALUES(1,5),(1,9),(2,3)`,
		`INSERT INTO t2 VALUES(9),(3),(100)`,
		`SELECT sum(value1), (SELECT count(*) FROM t2 WHERE value2=sum(t1.value1)) FROM t1 GROUP BY id1`,
	})
}

// Same shape, but the outer aggregate does NOT also appear in the outer
// select-list -- confirming this is not "reuse an existing outer aggregate
// slot" but a genuine independent hoist.
func TestOuterAggInSubqueryWhere_NoOuterListMatch(t *testing.T) {
	differ(t, "outer agg in subquery WHERE, no matching outer select-list aggregate", []string{
		`CREATE TABLE t1(id1, value1)`,
		`CREATE TABLE t2(value2)`,
		`INSERT INTO t1 VALUES(1,5),(1,9),(2,3)`,
		`INSERT INTO t2 VALUES(9),(3),(100)`,
		`SELECT id1, (SELECT count(*) FROM t2 WHERE value2=sum(t1.value1)) FROM t1 GROUP BY id1`,
	})
}

// The g1/g2 case from materializedOuterRefInAggArg's own doc comment, WHERE
// form: the subquery's own select-list is a bare column (not an aggregate),
// so C SQLite genuinely rejects this with "misuse of aggregate function
// sum()" -- MUST STAY DECLINED. This is the adversarial control: relaxing
// the WHERE guard unconditionally would silently answer this wrong instead
// of erroring.
func TestOuterAggInSubqueryWhere_NonAggSubqueryStaysDeclined(t *testing.T) {
	differ(t, "g1/g2 WHERE, non-aggregate subquery -- must stay declined", []string{
		`CREATE TABLE g1(k, v)`,
		`CREATE TABLE g2(w)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30),(2,NULL),(3,40)`,
		`INSERT INTO g2 VALUES(1),(2)`,
		`SELECT k, (SELECT w FROM g2 WHERE sum(g1.v) > w) FROM g1 GROUP BY k`,
	})
}

// The same g1/g2 fixture, but the subquery's own select-list is now an
// aggregate (count(*)) -- flips C SQLite from rejecting to accepting,
// with no other change to the WHERE clause. This is the exact flip that
// pins the rule to "is the subquery itself an aggregate query", not
// anything about the WHERE clause's own text.
func TestOuterAggInSubqueryWhere_AggSubqueryNowServed(t *testing.T) {
	differ(t, "g1/g2 WHERE, aggregate subquery -- now servable", []string{
		`CREATE TABLE g1(k, v)`,
		`CREATE TABLE g2(w)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30),(2,NULL),(3,40)`,
		`INSERT INTO g2 VALUES(1),(2)`,
		`SELECT k, (SELECT count(*) FROM g2 WHERE sum(g1.v) > w) FROM g1 GROUP BY k`,
	})
}

// JOIN ON variant of the non-aggregate control: must also stay declined.
func TestOuterAggInSubqueryOn_NonAggSubqueryStaysDeclined(t *testing.T) {
	differ(t, "g1/g2/g3 JOIN ON, non-aggregate subquery -- must stay declined", []string{
		`CREATE TABLE g1(k, v)`,
		`CREATE TABLE g2(w)`,
		`CREATE TABLE g3(z)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30)`,
		`INSERT INTO g2 VALUES(1),(2)`,
		`INSERT INTO g3 VALUES(1),(2)`,
		`SELECT k, (SELECT w FROM g2 JOIN g3 ON sum(g1.v) > w AND g3.z=g2.w) FROM g1 GROUP BY k`,
	})
}

// JOIN ON variant of the aggregate case: now servable, identical flip to the
// WHERE case.
func TestOuterAggInSubqueryOn_AggSubqueryNowServed(t *testing.T) {
	differ(t, "g1/g2/g3 JOIN ON, aggregate subquery -- now servable", []string{
		`CREATE TABLE g1(k, v)`,
		`CREATE TABLE g2(w)`,
		`CREATE TABLE g3(z)`,
		`INSERT INTO g1 VALUES(1,10),(1,20),(2,30)`,
		`INSERT INTO g2 VALUES(1),(2)`,
		`INSERT INTO g3 VALUES(1),(2)`,
		`SELECT k, (SELECT count(*) FROM g2 JOIN g3 ON sum(g1.v) > w AND g3.z=g2.w) FROM g1 GROUP BY k`,
	})
}
