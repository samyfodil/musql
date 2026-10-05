// Tests nested window functions in PARTITION BY / ORDER BY clauses. These are
// declined because nested windows cannot be modeled in the operand lowering.
package compat

import (
	"fmt"
	"math/rand"
	"testing"
)

func windowR24NestedFixture() []string {
	return []string{
		`CREATE TABLE t1(a, b, c, d)`,
		`CREATE TABLE t2(a, b, c, d)`,
		`INSERT INTO t1 VALUES(3,1,1,1),(1,2,2,2),(2,3,3,3),(2,4,1,9)`,
		`INSERT INTO t2 VALUES(7,1,1,1),(8,2,2,2),(5,3,9,4)`,
	}
}

func TestWindowR24NestedSpecWindow(t *testing.T) {
	q := func(sql ...string) []string {
		return append(windowR24NestedFixture(), sql...)
	}

	// The three mined statements' own shapes, over their own fixtures.
	differ(t, "windowfault.test 10", []string{
		`CREATE TABLE t1(a, b, c, d)`,
		`CREATE TABLE t2(a, b, c, d)`,
		`SELECT row_number() OVER win FROM t1 WINDOW win AS ( ORDER BY ( SELECT percent_rank() OVER win2 FROM t2 WINDOW win2 AS (ORDER BY a) ) )`,
		`INSERT INTO t1 VALUES(3,1,1,1),(1,2,2,2),(2,3,3,3)`,
		`INSERT INTO t2 VALUES(7,1,1,1),(8,2,2,2)`,
		`SELECT row_number() OVER win FROM t1 WINDOW win AS ( ORDER BY ( SELECT percent_rank() OVER win2 FROM t2 WINDOW win2 AS (ORDER BY a) ) )`,
		`SELECT a, row_number() OVER win FROM t1 WINDOW win AS ( ORDER BY ( SELECT percent_rank() OVER win2 FROM t2 WINDOW win2 AS (ORDER BY a) ) )`,
	})
	differ(t, "window1.test 34.2 (doubly nested)", []string{
		`CREATE TABLE t1(a, b, c)`,
		`SELECT avg(a) OVER ( ORDER BY (SELECT sum(b) OVER () FROM t1 ORDER BY ( SELECT total(d) OVER (ORDER BY c) FROM (SELECT 1 AS d) ORDER BY 1 ) ) ) FROM t1`,
		`INSERT INTO t1 VALUES(1,1,1),(2,2,2),(3,9,0)`,
		`SELECT avg(a) OVER ( ORDER BY (SELECT sum(b) OVER () FROM t1 ORDER BY ( SELECT total(d) OVER (ORDER BY c) FROM (SELECT 1 AS d) ORDER BY 1 ) ) ) FROM t1`,
	})
	// window1.test 42's own mined statement, over the fixture the CORPUS puts
	// under it (t1(b, x) -- not this file's t1(a,b,c), where its "x" is simply
	// "no such column" on both engines). This is the one the whole nested-window
	// decline was really covering, and the one every deterministic and
	// randomized case above misses: t1 is EMPTY and C SQLite still answers
	// ONE row, because the sum() inside the spec's subquery names t1.x and so
	// re-associates with the WINDOW query, making it an aggregate query. See
	// TestWindowR24SpecSubqueryAggReassociates below.
	differ(t, "window1.test 42's fuzz statement, this file's fixture", []string{
		`CREATE TABLE t1(a, b, c)`,
		`INSERT INTO t1 VALUES(1, 1, 1)`,
		`INSERT INTO t1 VALUES(2, 2, 2)`,
		`SELECT sum(b) over( ORDER BY ( SELECT max(b) OVER( ORDER BY sum( (SELECT x AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1`,
	})

	// A CORRELATED spec subquery whose inner window depends on the outer row:
	// the key really does vary per row, so a plan that evaluated it once would
	// answer a single partition / a constant sort key.
	differ(t, "correlated nested window in the spec", q(
		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a > t1.a)) FROM t1`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT max(t2.a) OVER (ORDER BY t2.a) FROM t2 WHERE t2.b >= t1.b LIMIT 1)) FROM t1`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT row_number() OVER () FROM t2 WHERE t2.b = t1.b)) FROM t1`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT sum(t1.a) OVER () )) FROM t1`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT lag(t2.a) OVER (ORDER BY t2.a) FROM t2 ORDER BY t2.a DESC LIMIT 1)) FROM t1`,
		`SELECT a, b, sum(a) OVER (PARTITION BY (SELECT dense_rank() OVER (ORDER BY t2.c) FROM t2 WHERE t2.a > t1.b LIMIT 1) ORDER BY b) FROM t1`,
	))

	// Uncorrelated, EXISTS/IN spellings, and a nested window reached through an
	// operator/CASE rather than sitting at the top of the key.
	differ(t, "uncorrelated and non-scalar spellings", q(
		`SELECT a, count(*) OVER (ORDER BY (SELECT sum(a) OVER () FROM t2 LIMIT 1)) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY EXISTS(SELECT row_number() OVER () FROM t2 WHERE t2.a=t1.a)) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY t1.b IN (SELECT rank() OVER (ORDER BY t2.a) FROM t2)) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY 1 + (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY CASE WHEN a>1 THEN (SELECT ntile(2) OVER () FROM t2 LIMIT 1) ELSE 0 END) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a) DESC) FROM t1`,
	))

	// The nested window inside a NAMED window's definition, and chained named
	// windows -- the resolution path resolveWindowSpec takes.
	differ(t, "named windows", q(
		`SELECT a, sum(a) OVER w FROM t1 WINDOW w AS (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a))`,
		`SELECT a, sum(a) OVER (w ROWS 1 PRECEDING) FROM t1 WINDOW w AS (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a))`,
		`SELECT a, sum(a) OVER w2 FROM t1 WINDOW w AS (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)), w2 AS (w ROWS 1 PRECEDING)`,
	))

	// A nested window in the spec BESIDE a second window with its own spec, so
	// the level chain (computeWindowLevels) is exercised at the same time.
	differ(t, "nested spec plus a second level", q(
		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)), count(*) OVER (ORDER BY b DESC) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY b DESC), sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)) FROM t1`,
	))

	// A frame on top of a nested-window key.
	differ(t, "framed", q(
		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a) ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t1`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a) RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM t1`,
	))

	// The inner window reads the SAME table the outer one is scanning, is two
	// levels deep, or feeds DISTINCT / LIMIT / a top-level ORDER BY -- the
	// places where "the inner window runs over its own batch" could have leaked.
	differ(t, "self-referential, two-deep, and result-shaping", q(
		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) OVER () FROM t1 AS u WHERE u.a > t1.a)) FROM t1`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT max(u.a) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>u.a)) FROM t1 AS u LIMIT 1)) FROM t1`,
		`SELECT DISTINCT b%2, count(*) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)) FROM t1`,
		`SELECT a, count(*) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)) FROM t1 LIMIT 2`,
		`SELECT a, count(*) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)) FROM t1 ORDER BY 2, 1`,
		`SELECT a FROM t1 ORDER BY count(*) OVER (ORDER BY (SELECT count(*) OVER () FROM t2 WHERE t2.a>t1.a)), a`,
	))
}

// TestWindowR24SpecSubqueryAggReassociates pins the rule that replaced
// the nested-window decline, and its evidence.
//
// window1.test 42.4 over its own corpus fixture -- t1(b, x), EMPTY -- answers
// ONE ROW of NULL in C SQLite where a plain window query over an empty table
// answers none. The sum() is written in a FROM-LESS subquery inside the window's
// own ORDER BY, its argument names t1.x, and resolve.c's outward walk
// (sqlite3ReferencesSrcList == 1 for t1) therefore re-associates it with the
// WINDOW query -- which makes that query an AGGREGATE query, and an aggregate
// query always emits its one row. Nothing about the nested WINDOW mattered.
//
// The engine used to decline it rather than model the re-association; it now
// answers the three FROM-less spellings exactly as C SQLite does, over the
// empty fixture and a filled one, and they are compared with the oracle. The
// spelling with its own FROM (t2) may still decline, never differ. The
// neighbouring shape that must NOT be swept up -- an aggregate in a spec
// subquery naming a column of that SUBQUERY's own FROM stays put -- stays
// served.
func TestWindowR24SpecSubqueryAggReassociates(t *testing.T) {
	fixture := []string{
		`CREATE TABLE t1(b, x)`,
		`CREATE TABLE t2(c, d)`,
	}
	for _, fill := range [][]string{nil, {`INSERT INTO t1 VALUES(1,10),(2,20),(3,5)`, `INSERT INTO t2 VALUES(7,8)`}} {
		for _, q := range []string{
			`SELECT sum(b) over( ORDER BY ( SELECT max(b) OVER( ORDER BY sum( (SELECT x AS c UNION SELECT 1234 ORDER BY c) ) ) AS e ORDER BY e ) ) FROM t1`,
			// The same mechanism spelled plainly, without any nested window.
			`SELECT sum(b) OVER (ORDER BY (SELECT sum(x))) FROM t1`,
			`SELECT sum(b) OVER (PARTITION BY (SELECT count(x))) FROM t1`,
		} {
			differ(t, "spec subquery aggregate re-associates", append(append(append([]string{}, fixture...), fill...), q))
		}
		differAllowingDeclines(t, "spec subquery aggregate over its own FROM re-associates",
			append(append(append([]string{}, fixture...), fill...), `SELECT sum(b) OVER (ORDER BY (SELECT max(t1.x) FROM t2)) FROM t1`))
	}
	// ...and the neighbour that must stay served: the aggregate names only the
	// subquery's own column, so it never reaches this query.
	differ(t, "spec subquery aggregate that stays put", []string{
		`CREATE TABLE t1(b, x)`,
		`CREATE TABLE t2(c, d)`,
		`INSERT INTO t1 VALUES(5,50),(6,60)`,
		`INSERT INTO t2 VALUES(1,2),(3,4)`,
		`SELECT b, sum(b) OVER (ORDER BY (SELECT sum(c) FROM t2)) FROM t1`,
		`SELECT b, sum(b) OVER (PARTITION BY (SELECT count(d) FROM t2)) FROM t1`,
		`SELECT b, sum(b) OVER (ORDER BY (SELECT sum(t2.c) FROM t2 WHERE t2.c < t1.b)) FROM t1`,
	})
}

// TestWindowR24NestedSpecFuzz builds nested-window spec keys at random and
// requires byte-exact agreement on every one. Deterministic probes agreeing is
// what shipped this shape's last wrong answer; a generator that reaches shapes
// nobody thought to write is the check that would have caught it.
func TestWindowR24NestedSpecFuzz(t *testing.T) {
	outerFn := []string{"sum(a)", "count(*)", "row_number()", "rank()", "dense_rank()",
		"max(b)", "min(b)", "avg(a)", "lag(a)", "lead(a)", "first_value(a)", "last_value(a)",
		"ntile(2)", "cume_dist()", "percent_rank()", "total(d)", "group_concat(c)"}
	innerFn := []string{"count(*)", "sum(t2.a)", "row_number()", "rank()", "max(t2.b)",
		"percent_rank()", "dense_rank()", "min(t2.c)", "lag(t2.a)", "ntile(2)", "avg(t2.d)"}
	innerSpec := []string{"()", "(ORDER BY t2.a)", "(PARTITION BY t2.c)",
		"(ORDER BY t2.b DESC)", "(PARTITION BY t2.c ORDER BY t2.a)",
		"(ORDER BY t2.a ROWS 1 PRECEDING)"}
	innerWhere := []string{"", " WHERE t2.a > t1.a", " WHERE t2.b >= t1.b", " WHERE t2.c <> t1.c",
		" WHERE t2.a IS NOT NULL"}
	keyWrap := []string{"%s", "1 + %s", "-(%s)", "CASE WHEN a>1 THEN (%s) ELSE 0 END",
		"coalesce(%s, 0)", "(%s) IS NULL"}
	outerClause := []string{"ORDER BY %s", "PARTITION BY %s", "ORDER BY %s DESC",
		"PARTITION BY %s ORDER BY b", "ORDER BY b, %s"}
	tail := []string{"", " ORDER BY 1", " ORDER BY 1, 2"}
	frame := []string{"", " ROWS BETWEEN 1 PRECEDING AND CURRENT ROW",
		" RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW", " GROUPS 1 PRECEDING"}

	rng := rand.New(rand.NewSource(20260806))
	pick := func(s []string) string { return s[rng.Intn(len(s))] }

	for i := 0; i < 220; i++ {
		inner := fmt.Sprintf("(SELECT %s OVER %s FROM t2%s LIMIT 1)",
			pick(innerFn), pick(innerSpec), pick(innerWhere))
		key := fmt.Sprintf(pick(keyWrap), inner)
		spec := fmt.Sprintf(pick(outerClause), key)
		q := fmt.Sprintf("SELECT a, b, %s OVER (%s%s) FROM t1%s",
			pick(outerFn), spec, pick(frame), pick(tail))
		if !differ(t, fmt.Sprintf("nested-spec fuzz #%d", i), append(windowR24NestedFixture(), q)) {
			// One divergence is the whole finding; keep the log readable.
			return
		}
	}

	// Second pass, different generator: the inner window reads t1 itself (so
	// both engines' "inner batch" is over the same rows the outer window is
	// buffering), the key can nest two deep, and the outer statement shapes its
	// result with DISTINCT / LIMIT / ORDER BY.
	src := []string{"t2", "t1 AS u", "(SELECT a, b, c, d FROM t2) AS s"}
	col := map[string]string{"t2": "t2", "t1 AS u": "u", "(SELECT a, b, c, d FROM t2) AS s": "s"}
	shape := []string{"", " LIMIT 2", " ORDER BY 1", " ORDER BY 2, 1"}
	for i := 0; i < 150; i++ {
		s := pick(src)
		p := col[s]
		inner := fmt.Sprintf("(SELECT %s OVER %s FROM %s WHERE %s.a <> t1.a LIMIT 1)",
			pick([]string{"count(*)", "row_number()", "rank()", "sum(" + p + ".a)", "max(" + p + ".b)", "ntile(2)", "lag(" + p + ".a)"}),
			pick([]string{"()", "(ORDER BY " + p + ".a)", "(PARTITION BY " + p + ".c)", "(ORDER BY " + p + ".b DESC)"}),
			s, p)
		if rng.Intn(3) == 0 {
			inner = fmt.Sprintf("(SELECT count(*) OVER (ORDER BY %s) FROM t2 LIMIT 1)", inner)
		}
		distinct := ""
		if rng.Intn(4) == 0 {
			distinct = "DISTINCT "
		}
		q := fmt.Sprintf("SELECT %sa, %s OVER (%s) FROM t1%s",
			distinct, pick(outerFn), fmt.Sprintf(pick(outerClause), inner), pick(shape))
		if !differ(t, fmt.Sprintf("nested-spec fuzz B #%d", i), append(windowR24NestedFixture(), q)) {
			return
		}
	}
}
