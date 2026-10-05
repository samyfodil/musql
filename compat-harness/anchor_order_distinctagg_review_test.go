package compat

// Independent adversarial review coverage for the
// "distinct-aggregate-groupby-order-toplevel" bucket (wherePlanSortCtlFor's
// narrowed DISTINCT-aggregate decline, engine/where_plan_gate.go). Every case
// here is deliberately NOT one of the shipped battery's own cases
// (anchor_order_distinctagg_test.go) -- see this file's own review notes for
// what each one is trying to break.
import "testing"

func TestAnchorOrderDistinctAggGroupByReview(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// A bare, non-aggregated, non-grouped ANCHOR column alongside the
		// DISTINCT aggregate -- exactly the shape whose VALUE is a tie-break
		// question (aggAccumulators.magnetWalk) and therefore the shape most
		// exposed if this port's plan choice for a DISTINCT-agg GROUP BY ever
		// disagreed with C SQLite's for the SAME data. Two rows per group
		// with DIFFERENT anchor-column values, so a wrong tie-break is visible.
		{
			"anchor-column-tie-break",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,'first',10)`,
				`INSERT INTO t1 VALUES(1,'second',20)`,
				`INSERT INTO t1 VALUES(2,'third',30)`,
				`INSERT INTO t1 VALUES(2,'fourth',10)`,
				`SELECT a, b, count(DISTINCT c) FROM t1 GROUP BY a`,
			},
		},
		// group_concat(DISTINCT x) -- unlike count(DISTINCT x), its RESULT is
		// order-sensitive (the surviving spelling AND the concatenation
		// order), so this is the aggregate kind most exposed by any row-order
		// mismatch this port's DISTINCT-agg plan choice could introduce.
		{
			"group-concat-distinct-groupby",
			[]string{
				`CREATE TABLE t1(a, b)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,'z')`,
				`INSERT INTO t1 VALUES(1,'a')`,
				`INSERT INTO t1 VALUES(1,'z')`,
				`INSERT INTO t1 VALUES(2,'m')`,
				`INSERT INTO t1 VALUES(2,'b')`,
				`SELECT a, group_concat(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// min(DISTINCT x)/max(DISTINCT x) under NOCASE, where the dedup
		// SURVIVOR's byte spelling (not just the count) is the observable
		// value, and ties resolve by arrival order.
		{
			"min-max-distinct-nocase-tie",
			[]string{
				`CREATE TABLE t1(a, b COLLATE NOCASE)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,'ABC')`,
				`INSERT INTO t1 VALUES(1,'abc')`,
				`INSERT INTO t1 VALUES(1,'xyz')`,
				`INSERT INTO t1 VALUES(2,'Q')`,
				`SELECT a, min(DISTINCT b), max(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// Two GROUP BY columns against a matching composite DESC index, plus
		// a DISTINCT aggregate -- exercises sqlite3CopySortOrder's DESC-bit
		// copy and the multi-column group-order proof together with the new
		// wantDistinct wiring.
		{
			"composite-groupby-desc-index",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1ab ON t1(a DESC, b DESC)`,
				`INSERT INTO t1 VALUES(1,1,'x')`,
				`INSERT INTO t1 VALUES(1,1,'y')`,
				`INSERT INTO t1 VALUES(1,2,'x')`,
				`INSERT INTO t1 VALUES(2,1,'z')`,
				`INSERT INTO t1 VALUES(NULL,1,'w')`,
				`SELECT a, b, count(DISTINCT c) FROM t1 GROUP BY a, b ORDER BY a DESC, b DESC`,
			},
		},
		// HAVING referencing the DISTINCT aggregate directly (a second
		// evaluation site distinct from the select list).
		{
			"having-distinct-agg",
			[]string{
				`CREATE TABLE t1(a, b)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,1)`,
				`INSERT INTO t1 VALUES(1,1)`,
				`INSERT INTO t1 VALUES(1,2)`,
				`INSERT INTO t1 VALUES(2,5)`,
				`SELECT a, count(DISTINCT b) FROM t1 GROUP BY a HAVING count(DISTINCT b) > 1`,
			},
		},
		// GROUP BY on an EXPRESSION (not a bare column) with a DISTINCT
		// aggregate -- the group key itself is never index-ordered, so this
		// must still be correct however it is planned (index or fallback).
		{
			"groupby-expression-distinct-agg",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,10,'x')`,
				`INSERT INTO t1 VALUES(2,9,'x')`,
				`INSERT INTO t1 VALUES(3,8,'y')`,
				`INSERT INTO t1 VALUES(4,7,'y')`,
				`SELECT a+b, count(DISTINCT c) FROM t1 GROUP BY a+b`,
			},
		},
		// A NULL group key mixed with an index that sorts NULLs first
		// (default ASC) -- BIGNULL/NULL-ordering interacting with the
		// DISTINCT-agg unblock.
		{
			"null-group-key-distinct-agg",
			[]string{
				`CREATE TABLE t1(a, b)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(NULL,1)`,
				`INSERT INTO t1 VALUES(NULL,1)`,
				`INSERT INTO t1 VALUES(NULL,2)`,
				`INSERT INTO t1 VALUES(1,3)`,
				`SELECT a, count(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// ANALYZE present (sqlite_stat1 exists): markWherePlanIndexEligibility
		// declines OUTRIGHT the instant any sqlite_stat* table exists
		// (where_plan_gate.go, wherePlanSingleTableIndexOrder), independent of
		// this fix -- confirms the whole stats-driven cost-estimate family
		// this review investigated cannot reach musql's executed path at all,
		// so it must still answer correctly (via whatever fallback this
		// engine uses) rather than silently miscompute.
		{
			"analyze-present-distinct-agg",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1,1,10)`,
				`INSERT INTO t1 VALUES(1,1,10)`,
				`INSERT INTO t1 VALUES(1,2,20)`,
				`INSERT INTO t1 VALUES(2,5,30)`,
				`ANALYZE`,
				`SELECT a, count(DISTINCT c) FROM t1 GROUP BY a`,
			},
		},
		// sum(DISTINCT)/avg(DISTINCT) float-summation order sensitivity
		// (Kahan-Babuska-Neumaier accumulation order) combined with a
		// GROUP BY over a real index.
		{
			"sum-avg-distinct-float-order",
			[]string{
				`CREATE TABLE t1(a, b)`,
				`CREATE INDEX t1a ON t1(a)`,
				`INSERT INTO t1 VALUES(1, 9007199254740992)`,
				`INSERT INTO t1 VALUES(1, 1)`,
				`INSERT INTO t1 VALUES(1, 1)`,
				`INSERT INTO t1 VALUES(2, 100)`,
				`SELECT a, sum(DISTINCT b), avg(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// A UNIQUE index on a DIFFERENT column than the GROUP BY column, so
		// the only usable order-delivering index for the group key is NONE --
		// the fallback (no index helps GROUP BY order) path, still with a
		// single DISTINCT aggregate.
		{
			"unrelated-unique-index-no-groupby-index",
			[]string{
				`CREATE TABLE t1(a, b, c)`,
				`CREATE UNIQUE INDEX t1c ON t1(c)`,
				`INSERT INTO t1 VALUES(2,1,1)`,
				`INSERT INTO t1 VALUES(1,1,2)`,
				`INSERT INTO t1 VALUES(2,2,3)`,
				`INSERT INTO t1 VALUES(1,2,4)`,
				`SELECT a, count(DISTINCT b) FROM t1 GROUP BY a`,
			},
		},
		// A large-ish table (WHERE-selective index competing against a
		// GROUP-BY-ordered one, no ANALYZE) to stress the DP solver's actual
		// cost arithmetic post-fix at a scale where LogEst rounding matters,
		// not just the tiny hand fixtures above.
		{
			"larger-table-where-and-groupby-indexes",
			[]string{
				`CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT, d INTEGER)`,
				`CREATE INDEX t1a ON t1(a)`,
				`CREATE INDEX t1b ON t1(b)`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmts := c.stmts
			if c.name == "larger-table-where-and-groupby-indexes" {
				for i := 0; i < 500; i++ {
					stmts = append(stmts, sprintfInsertT1(i))
				}
				stmts = append(stmts, `SELECT b, c, count(DISTINCT d) FROM t1 WHERE a >= 480 GROUP BY b`)
			}
			differ(t, c.name, stmts)
		})
	}
}

func sprintfInsertT1(i int) string {
	return "INSERT INTO t1 VALUES(" +
		itoaFast(i) + "," + itoaFast(i%13) + ",'v" + itoaFast(i) + "'," + itoaFast(i%7) + ")"
}

func itoaFast(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
