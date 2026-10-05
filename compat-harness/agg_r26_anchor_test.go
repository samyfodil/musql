package compat

// Tests which row of a group supplies a bare column (neither grouped nor
// aggregated) in aggregate queries. SQLite uses the last scanned row to
// contribute to aggregate accumulator registers.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var r26AnchorSchema = []string{
	"CREATE TABLE t0(a,b)",
	"INSERT INTO t0 VALUES(3,5),(1,7),(2,9)",
	"CREATE TABLE td3(a,b)",
	"INSERT INTO td3 VALUES(1,5),(2,9),(3,9)",
	"CREATE TABLE g1(k,v,c)",
	"INSERT INTO g1 VALUES(1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k')",
	"CREATE TABLE n1(k,v,c)",
	"INSERT INTO n1 VALUES(1,NULL,'p'),(1,NULL,'q'),(1,5,'r'),(1,NULL,'s')",
	"CREATE TABLE dn(k,v,c)",
	"INSERT INTO dn VALUES(1,NULL,'p'),(1,NULL,'q'),(1,3,'r')",
}

// TestR26AnchorRules pins the four rules that make up the model, each with the
// control that must NOT move beside it.
func TestR26AnchorRules(t *testing.T) {
	for _, q := range []string{
		// (1) CENSUS ORDER is SELECT LIST -> ORDER BY -> HAVING (sqlite3Select
		// c:157426-157436), not select-list -> HAVING -> ORDER BY. min(a) is
		// therefore the LAST site, and on row (2,9) it does not beat 1, so the
		// magnet stays SET and row (1,7) remains the anchor.
		"SELECT count(*), a, b FROM t0 HAVING min(a)>0 ORDER BY max(b)",
		// ...and with the two clauses swapped, so max(b) is last: its own row wins.
		"SELECT count(*), a, b FROM t0 HAVING max(b)>0 ORDER BY min(a)",
		// Two sites in the SELECT LIST alone: the later one decides.
		"SELECT min(v), max(v), k, c FROM g1 GROUP BY k ORDER BY k",
		"SELECT max(v), min(v), k, c FROM g1 GROUP BY k ORDER BY k",

		// (2) A PARTIAL FILTER. On the last row the filtered site is jumped
		// over entirely, so the previous site's cleared magnet stands -- unless
		// S was allocated, which for a whole-table aggregate happens only when
		// EVERY min/max is filtered, and for a GROUP BY always.
		"SELECT max(b), min(a) FILTER (WHERE b<9), a, b FROM t0",
		"SELECT max(b), min(a) FILTER (WHERE b<9), a, b FROM t0 GROUP BY 1=1",
		"SELECT min(a) FILTER (WHERE b<9), max(b), a, b FROM t0",
		// A FILTER that rejects EVERY row: the group's first row, exactly like a
		// query with no min/max at all.
		"SELECT max(v) FILTER (WHERE c='zzz'), k, c FROM g1 GROUP BY k ORDER BY k",
		// Only filtered min/max sites, so S IS allocated even without GROUP BY.
		"SELECT max(b) FILTER (WHERE a<3), a, b FROM t0",

		// (3) A DISTINCT duplicate is jumped over too -- it does not clear the
		// magnet, so the previous verdict (or the previous row's) stands.
		"SELECT max(DISTINCT b), a, b FROM td3",
		"SELECT max(b), a, b FROM td3",
		"SELECT count(DISTINCT b), max(b), a, b FROM td3",
		// The NULL corner: codeDistinct dedups the raw argument register, where
		// NULL==NULL, so a repeated NULL IS a duplicate there even though NULL
		// never reaches an ordinary DISTINCT accumulator's value set.
		"SELECT max(DISTINCT v), k, c FROM dn GROUP BY k",
		"SELECT min(c), max(DISTINCT v), k FROM dn GROUP BY k",

		// (4) The three cases the model subsumes.
		"SELECT count(*), k, c FROM g1 GROUP BY k ORDER BY k",       // no min/max -> first row
		"SELECT max(v), k, c FROM n1 GROUP BY k",                    // a non-NULL winner
		"SELECT max(NULL), k, c FROM n1 GROUP BY k",                 // never a winner -> LAST row
		"SELECT max(v), min(NULL), k, c FROM n1 GROUP BY k",         // ...and as the LAST site
		"SELECT min(NULL), max(v), k, c FROM n1 GROUP BY k",         // ...and as the FIRST
		"SELECT max(v) FROM n1 GROUP BY k HAVING v IS NULL",         // HAVING reads the same anchor
		"SELECT k, c FROM g1 GROUP BY k ORDER BY max(v)",            // ORDER BY site moves it
		"SELECT k, c, max(v) OVER () FROM g1 GROUP BY k ORDER BY k", // observable through a window

		// A correlated select-list subquery reads the SAME anchor a bare column
		// would, so it must move with it identically.
		"SELECT k, max(v), (SELECT c) FROM g1 GROUP BY k ORDER BY k",
		"SELECT k, count(*), (SELECT c) FROM g1 GROUP BY k ORDER BY k",

		// An empty group / zero-row scan still emits its one row, all NULL.
		"SELECT max(b), a, b FROM t0 WHERE 0",
		"SELECT count(*), a, b FROM t0 WHERE 0",
	} {
		if !differ(t, "r26anchor", append(append([]string(nil), r26AnchorSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestR26AnchorFuzz is the gate that actually matters. The anchor is
// VALUE-dependent -- which row wins depends on the data as much as on the
// query -- and this project has already learned once that hand-picked probes
// all agree while a randomized sweep does not (the "a min/max moves the anchor
// to the winning row" rule itself was found that way). So the sub-shapes are
// generated rather than enumerated: the census is built from a random pick of
// clauses, sites, FILTERs and DISTINCTs, over a random table whose columns hold
// heavy duplicates and NULLs precisely so ties, all-NULL groups and DISTINCT
// dedup all fire often.
//
// Every generated statement ends in an explicit ORDER BY over the GROUP BY key
// (or has no GROUP BY at all, hence one row), so the comparison is exact.
func TestR26AnchorFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("randomized anchor sweep")
	}
	rng := rand.New(rand.NewSource(r26EnvInt("R26_SEED", 0x26a9c4)))
	for iter := 0; iter < int(r26EnvInt("R26_ITERS", 400)); iter++ {
		stmts := r26FuzzSchema(rng)
		q := r26FuzzQuery(rng)
		if !differ(t, "r26fuzz", append(stmts, q)) {
			t.Fatalf("diverged on: %s\nschema: %v", q, stmts)
		}
	}
}

// TestR26AnchorPlanOrderDeclines is the other half of the anchor rule: WHICH
// row is "the group's first" depends on the chosen plan's SCAN ORDER, and this
// engine always scans a base table in ascending rowid order where SQLite may
// walk an index. engine/vdbe_agg_codegen.go's anchorPlanOrderProvable declines
// the whole class rather than guess.
//
// The gate is SELF-VERIFYING in both directions: for each declined statement it
// first asks the ORACLE for the answer and asserts that answer is NOT what
// musql's rowid-order anchor would have produced -- so the day the decline is
// lifted, this test says whether that was an upgrade or a wrong answer, rather
// than merely that something changed. The controls below it are the no-index
// shapes, which must keep ANSWERING and agreeing.
func TestR26AnchorPlanOrderDeclines(t *testing.T) {
	indexed := []string{
		"CREATE TABLE p1(a INTEGER,b INTEGER)",
		"INSERT INTO p1 VALUES(1,100),(2,202),(2,201),(2,200)",
		"CREATE INDEX px ON p1(a,b)",
	}
	// SERVED, and byte-identical to the oracle: the anchor's ACCESS-PATH half
	// now asks the ported planner whether the scan order is DECIDED
	// (wherePlanIndexOrderDecided, engine/where_plan_gate.go) instead of the
	// conservative proxy "does any scanned table carry an index". Both of these
	// return the covering index's 200, which is what the oracle returns -- the
	// rowid-order 202 this gate was built to refuse is gone, not hidden.
	for _, q := range []string{
		"SELECT a, sum(b), b FROM p1 GROUP BY a ORDER BY a",
		"SELECT a, count(*), b FROM p1 GROUP BY a ORDER BY a",
		// The correlated-subquery spelling of the same anchor read, moved up
		// from the declined list below in round 36. It reads the anchor exactly
		// as the bare column above does; what kept it declined was that ANY
		// subquery made SrcItem.colUsed incomputable (wherePlanColUsed), and
		// colUsed is what wherePlanIndexOrderDecided needs. r36dSubColUsed
		// (engine/where_plan_subcolused_r36d.go) computes it through the
		// subquery now, so this shape answers the covering index's 200 -- the
		// same 200 the oracle answers, not the rowid-order 202.
		"SELECT a, count(*), (SELECT b) FROM p1 GROUP BY a ORDER BY a",
	} {
		stmts := append(append([]string(nil), indexed...), q)
		if res := run(t, "musql", stmts); res[len(res)-1]["kind"] == "error" {
			t.Errorf("the ported planner decides this scan order; it must be served: %s\n  got: %v", q, res[len(res)-1])
			continue
		}
		if !differ(t, "r26anchor", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}

	// The declined list is EMPTY. Its one entry -- the correlated select-list
	// subquery -- moved into the served list above in round 36. aggResult still
	// hands the anchor to HAVING and ORDER BY items too (anchorIsRead,
	// vdbe_agg_codegen.go); the HAVING form stays pinned by
	// compat-harness/vdbe_agg_hoist_grouped_test.go. Keep the loop, with its
	// self-verifying rowidOrder sentinel -- that sentinel is what tells an
	// upgrade apart from a wrong answer -- and re-add a case the next time a
	// plan-order decline in this family is worth pinning.
	for _, c := range []struct{ q, rowidOrder string }{} {
		stmts := append(append([]string(nil), indexed...), c.q)
		res := run(t, "musql", stmts)
		if res[len(res)-1]["kind"] != "error" {
			t.Errorf("expected a plan-order decline for %q, got %v", c.q, res[len(res)-1])
			continue
		}
		oracle, _ := json.Marshal(run(t, "cgo", stmts)[len(stmts)-1])
		if strings.Contains(string(oracle), c.rowidOrder) {
			t.Errorf("%q: the oracle now agrees with rowid order (%s) -- this shape no longer needs the decline: %s",
				c.q, c.rowidOrder, oracle)
		}
	}

	// A whole-table aggregate reads the anchor too, and SQLite's own min/max
	// optimization -- an index seek, taken when the query has no GROUP BY, no
	// HAVING and exactly one aggregate -- lands on a DIFFERENT tied row than a
	// rowid-order scan does. Over (1,5),(2,5),(3,1) with an index on b the
	// oracle's "SELECT max(b), a, b" is a=2, the LAST row holding the maximum
	// (where the descending seek stops); a full scan pins the FIRST. Without
	// the seek the two coincide, which is why "count(*), a, b" over p1 above is
	// NOT one of these -- the oracle agrees with rowid order there.
	tied := []string{
		"CREATE TABLE w1(a INTEGER,b INTEGER)",
		"INSERT INTO w1 VALUES(1,5),(2,5),(3,1)",
		"CREATE INDEX w1b ON w1(b)",
	}
	// SERVED now, and it picks the LAST tied row exactly as the oracle does --
	// this was the sharpest case in the gate, because rowid order and the seek
	// disagree on WHICH tied row wins, so serving it wrongly would have been
	// visible here as a=1. It reports a=2. The oracle assertion below stays: if
	// C SQLite ever stops taking the last tied row, this fixture is no
	// longer testing what its comment claims.
	tq := "SELECT max(b), a, b FROM w1"
	tstmts := append(append([]string(nil), tied...), tq)
	if res := run(t, "musql", tstmts); res[len(res)-1]["kind"] == "error" {
		t.Errorf("the ported planner decides this scan order; it must be served: %q\n  got: %v", tq, res[len(res)-1])
	} else if !differ(t, "r26anchor", tstmts) {
		t.Errorf("diverged on: %s", tq)
	}
	if oracle, _ := json.Marshal(run(t, "cgo", tstmts)[len(tstmts)-1]); !strings.Contains(string(oracle), `"I:5","I:2","I:5"`) {
		t.Errorf("%q: the oracle no longer picks the last tied row: %s", tq, oracle)
	}

	// Controls: the same statements over a table with NO index must still be
	// answered, and must agree. 900 randomized samples of this shape found 0
	// divergences without an index (agg_r26_planorder_test.go), which is what
	// makes "no index" the safe side of the line.
	plain := []string{
		"CREATE TABLE p2(a INTEGER,b INTEGER)",
		"INSERT INTO p2 VALUES(1,100),(2,202),(2,201),(2,200)",
	}
	for _, q := range []string{
		"SELECT a, sum(b), b FROM p2 GROUP BY a ORDER BY a",
		"SELECT a, count(*), b FROM p2 GROUP BY a ORDER BY a",
		"SELECT count(*), a, b FROM p2",
		"SELECT max(b), a, b FROM p2",
		"SELECT a, max(b), b FROM p2 GROUP BY a ORDER BY a",
		// A correlated subquery reads the same anchor a bare column does, in
		// any of the three item sets -- measured at 20/96, 11/70 and 18/83
		// wrong over an INDEXED table, and 0/201 over this one.
		"SELECT a, count(*), (SELECT b) FROM p2 GROUP BY a ORDER BY a",
		"SELECT a, count(*) FROM p2 GROUP BY a HAVING (SELECT b) IS NOT NULL ORDER BY a",
		"SELECT a, count(*) FROM p2 GROUP BY a ORDER BY (SELECT b), a",
	} {
		if !differ(t, "r26planorder-control", append(append([]string(nil), plain...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// r26EnvInt lets a local sweep widen the fuzz (R26_SEED / R26_ITERS) without
// editing it; the defaults are what an ordinary run uses.
func r26EnvInt(name string, def int64) int64 {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.ParseInt(s, 0, 64); err == nil {
			return n
		}
	}
	return def
}

// r26FuzzSchema builds a small table whose values collide heavily: a 3-value
// domain plus NULL over 4-6 rows makes ties, repeated arguments and all-NULL
// groups the common case rather than the corner.
func r26FuzzSchema(rng *rand.Rand) []string {
	nRow := 4 + rng.Intn(3)
	vals := make([]string, nRow)
	for i := range vals {
		vals[i] = fmt.Sprintf("(%s,%s,%s,%s)",
			r26Cell(rng, 2), r26Cell(rng, 3), r26Cell(rng, 3), r26Cell(rng, 4))
	}
	return []string{
		"CREATE TABLE f(k,x,y,z)",
		"INSERT INTO f VALUES" + strings.Join(vals, ","),
	}
}

// r26Cell is one column value: NULL a quarter of the time, otherwise a small
// integer from a domain of n.
func r26Cell(rng *rand.Rand, n int) string {
	if rng.Intn(4) == 0 {
		return "NULL"
	}
	return fmt.Sprintf("%d", rng.Intn(n))
}

// r26FuzzQuery generates one statement: a random census spread over the select
// list, ORDER BY and HAVING, with bare columns to read the anchor through.
func r26FuzzQuery(rng *rand.Rand) string {
	site := func() string {
		s := []string{"min", "max"}[rng.Intn(2)] + "("
		if rng.Intn(4) == 0 {
			s += "DISTINCT "
		}
		s += []string{"x", "y", "z", "NULL"}[rng.Intn(4)] + ")"
		if rng.Intn(3) == 0 {
			s += fmt.Sprintf(" FILTER (WHERE %s)", r26Pred(rng))
		}
		return s
	}
	var sel []string
	for i, n := 0, rng.Intn(3); i < n; i++ {
		sel = append(sel, site())
	}
	sel = append(sel, "x", "y", "z")
	if rng.Intn(3) == 0 {
		sel = append(sel, "count(*)")
	}
	grouped := rng.Intn(2) == 0
	q := "SELECT "
	if grouped {
		q += "k, "
	}
	q += strings.Join(sel, ", ") + " FROM f"
	if grouped {
		q += " GROUP BY k"
	}
	if rng.Intn(3) == 0 {
		q += " HAVING " + site() + " IS NOT NULL"
	}
	if grouped {
		q += " ORDER BY "
		if rng.Intn(3) == 0 {
			q += site() + ", "
		}
		q += "k"
	} else if rng.Intn(3) == 0 {
		q += " ORDER BY " + site()
	}
	return q
}

// r26Pred is a FILTER condition that rejects a varying, data-dependent subset
// of the rows -- including, deliberately, ALL of them and NONE of them.
func r26Pred(rng *rand.Rand) string {
	switch rng.Intn(5) {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return fmt.Sprintf("x<%d", rng.Intn(3))
	case 3:
		return fmt.Sprintf("y>=%d", rng.Intn(3))
	default:
		return "z IS NOT NULL"
	}
}
