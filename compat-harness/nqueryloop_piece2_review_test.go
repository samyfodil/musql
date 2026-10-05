// Tests nQueryLoop piece 2 for multi-table nested consumption.
// (wherePlanMultiTableSources), a three-hop outer chain, a derived-table
// grandparent barrier interacting with a nested WHERE subquery, EXISTS/NOT
// EXISTS correlated forms, an UPDATE...WHERE row-scope nested subquery, and a
// shape aimed at reaching the automatic-index decline branch to confirm it
// stays a clean decline (never a guess) even when nQueryLoop is trusted and
// nonzero.
package compat

import (
	"fmt"
	"testing"
)

// TestReviewNestedMultiTableWhereSubquery hits wherePlanMultiTableSources'
// new consumption directly: the nested subquery itself has a TWO-table FROM
// (a join), previously always declined outright via "c.outer != nil", now
// eligible whenever nQueryLoopKnown is true. No ORDER BY inside the
// subquery, so its own join order/nesting is exactly the thing under test.
func TestReviewNestedMultiTableWhereSubquery(t *testing.T) {
	differ(t, "review_nested_multitable_where", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE TABLE t3(p INTEGER PRIMARY KEY, q INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`CREATE INDEX i3q ON t3(q)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`INSERT INTO t3 VALUES(1,50),(2,10),(3,20),(4,30),(5,50)`,
		`SELECT a FROM t1 WHERE a IN (
			SELECT mid.a FROM t1 AS mid, t2, t3
			WHERE t2.y = t3.q AND t2.x = mid.a + t3.p
		)`,
	})
}

// TestReviewThreeHopOuterChain stresses the outer-chain propagation across
// THREE nested levels of plain single-table SELECTs (t3.outer == mid2,
// mid2.outer == mid1, mid1.outer == top, top.outer == nil), verifying
// inheritedNQueryLoop composes correctly across more than one hop, not just
// the two the shipped tests cover.
func TestReviewThreeHopOuterChain(t *testing.T) {
	differ(t, "review_three_hop_outer_chain", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7),(6,11),(7,3)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT a FROM t1 AS top WHERE top.a = (
			SELECT mid1.a FROM t1 AS mid1 WHERE mid1.b = (
				SELECT mid2.x FROM t2 AS mid2 WHERE mid2.y = top.b*10 ORDER BY mid2.x LIMIT 1
			) / 10
		)`,
	})
}

// TestReviewDerivedTableGrandparentBarrier: a derived table (subquery in
// FROM) whose OWN body has a correlated subquery in its WHERE clause
// referencing the derived table's OUTER column -- exercising
// derivedOuterBarrier's grandparent-nQueryLoop threading directly, at a point
// where the containing query is itself nested one level deep (so the
// grandparent's own nQueryLoop is nonzero and known).
func TestReviewDerivedTableGrandparentBarrier(t *testing.T) {
	differ(t, "review_derived_table_grandparent_barrier", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT a FROM t1 WHERE a IN (
			SELECT mid.a FROM t1 AS mid, (
				SELECT x, y FROM t2 WHERE t2.y = mid.b*10
			) AS dv
			WHERE dv.x > 0
		)`,
	})
}

// TestReviewExistsCorrelatedNested exercises the EXISTS/NOT EXISTS spelling
// of a correlated subquery (a different AST shape than "= (SELECT ...)"),
// nested two levels deep.
func TestReviewExistsCorrelatedNested(t *testing.T) {
	differ(t, "review_exists_correlated_nested", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`SELECT a FROM t1 WHERE EXISTS (
			SELECT 1 FROM t1 AS mid
			WHERE mid.b = t1.b AND EXISTS (
				SELECT 1 FROM t2 WHERE t2.y = mid.b*10
			)
		)`,
	})
}

// TestReviewUpdateWhereNestedSubquery: an UPDATE's own WHERE clause,
// compiled through the write-path row-scope compiler (rowOuterCompile,
// vdbe_run.go), holding a nested single-table correlated subquery two levels
// deep -- confirming a write-statement's row scope stays conservatively
// declining (its own compiler carries no outer chain at all) rather than
// picking up a stray trusted nQueryLoop.
func TestReviewUpdateWhereNestedSubquery(t *testing.T) {
	differ(t, "review_update_where_nested_subquery", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,5),(2,2),(3,9),(4,5),(5,7)`,
		`INSERT INTO t2 VALUES(30,50),(10,10),(70,50),(20,20),(50,10),(60,30),(40,50),(80,10),(90,20)`,
		`UPDATE t1 SET b = b + 1000 WHERE a IN (
			SELECT mid.a FROM t1 AS mid
			WHERE mid.b = (SELECT x FROM t2 WHERE y = mid.b*10 ORDER BY x LIMIT 1) - 20
		)`,
		`SELECT a, b FROM t1 ORDER BY a`,
	})
}

// TestReviewAutomaticIndexReachableStaysDeclined targets the exact branch
// wherePlanSingleIndexKey's w.autoIdx guard newly documents as "no longer
// unreachable" once a nested compile's seed clears the "pFrom->nRow < 3"
// gate in solve() -- a nested correlated subquery whose own table has NO
// index on the correlated column at all, run enough times (a large outer
// driving table) that an automatic index would pay for itself under real
// SQLite's cost model. This must decline the ported planner for the
// subquery's own order (falling back to the pre-existing engine behavior)
// rather than ever fabricate an automatic-index plan that was never ported,
// and the FINAL rows must still agree with the live oracle regardless of
// which plan either engine takes.
func TestReviewAutomaticIndexReachableStaysDeclined(t *testing.T) {
	stmts := []string{
		`CREATE TABLE big(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE noidx(x INTEGER, y INTEGER)`,
	}
	for i := 1; i <= 40; i++ {
		stmts = append(stmts, sqlInsertBig(i))
	}
	stmts = append(stmts,
		`INSERT INTO noidx VALUES(1,10),(2,20),(3,30),(4,40),(5,50),(6,60),(7,70),(8,80)`,
		`SELECT a FROM big WHERE a IN (
			SELECT mid.a FROM big AS mid
			WHERE mid.b = (SELECT y FROM noidx WHERE x = mid.a % 8 + 1)
		)`,
	)
	differ(t, "review_autoindex_reachable_stays_declined", stmts)
}

// TestReviewNestedDistinctOrderBySortCostThreshold targets
// whereSortingCost's own nonlinear "wantDistinct" branch (nRow>10 halves the
// row estimate before costing the sort) on a NESTED subquery's second solve()
// pass, whose nRowEst is derived from the first pass's best.nRow -- itself
// seeded from the propagated nQueryLoop. A table sized to sit close to that
// threshold on the correlated (indexed) column, combined with DISTINCT +
// ORDER BY inside the nested subquery, is the shape most likely to flip a
// sort-vs-no-sort choice if the seed were even slightly wrong.
func TestReviewNestedDistinctOrderBySortCostThreshold(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,1),(2,2),(3,3)`,
	}
	for i := 1; i <= 24; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO t2 VALUES(%d,%d,%d)", i, i%3, i))
	}
	stmts = append(stmts, `SELECT a, (
		SELECT group_concat(z) FROM (
			SELECT DISTINCT z FROM t2 WHERE y = a ORDER BY z DESC
		)
	) FROM t1`)
	differ(t, "review_nested_distinct_orderby_threshold", stmts)
}

// TestReviewNestedLimitSortCostThreshold targets whereSortingCost's
// "useLimit" branch (s.iLimit < nRow) on the same nested second-solve path,
// with a LIMIT chosen close to the candidate table's own row count so the
// comparison sits near its boundary.
func TestReviewNestedLimitSortCostThreshold(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`CREATE TABLE t2(x INTEGER PRIMARY KEY, y INTEGER, z INTEGER)`,
		`CREATE INDEX i2y ON t2(y)`,
		`INSERT INTO t1 VALUES(1,1),(2,2),(3,3)`,
	}
	for i := 1; i <= 12; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO t2 VALUES(%d,%d,%d)", i, i%3, i))
	}
	stmts = append(stmts, `SELECT a, (
		SELECT group_concat(z) FROM (
			SELECT z FROM t2 WHERE y = a ORDER BY z DESC LIMIT 4
		)
	) FROM t1`)
	differ(t, "review_nested_limit_threshold", stmts)
}

func sqlInsertBig(i int) string {
	return fmt.Sprintf("INSERT INTO big VALUES(%d,%d)", i, i%7)
}
