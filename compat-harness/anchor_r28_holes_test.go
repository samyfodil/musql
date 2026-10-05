package compat

// This file tests bare-column aggregate anchors when the planner's order differs from a fallback heuristic.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r28Answered reports whether musql actually answered the last statement (as
// opposed to declining it), and the JSON of every engine's results.
func r28Answered(t *testing.T, stmts []string) (answered bool, cgo, mush []byte) {
	t.Helper()
	c := run(t, "cgo", stmts)
	m := run(t, "musql", stmts)
	cgo, _ = json.Marshal(c)
	mush, _ = json.Marshal(m)
	return m[len(m)-1]["kind"] != "error", cgo, mush
}

// TestR28AnchorHoles is the deterministic half: one shape per reason the ported
// planner refuses a FROM clause, all over UNINDEXED tables so the INDEX half of
// the guard has nothing to say and only the LOOP ORDER half can decline. Each
// must either agree with the oracle or decline -- serving a different answer is
// the failure this file exists to catch.
func TestR28AnchorHoles(t *testing.T) {
	// j1..j4 are deliberately unindexed and small, with a 2-value join key so
	// every group holds several rows and the anchor shows in b/c/d/e.
	base := []string{
		"CREATE TABLE j1(a,b)",
		"INSERT INTO j1 VALUES(1,10),(1,11),(2,12),(2,13)",
		"CREATE TABLE j2(a,c)",
		"INSERT INTO j2 VALUES(1,20),(1,21),(2,22),(2,23)",
		"CREATE TABLE j3(a,d)",
		"INSERT INTO j3 VALUES(1,30),(1,31),(2,32),(2,33)",
		"CREATE TABLE j4(a,e)",
		"INSERT INTO j4 VALUES(1,40),(1,41),(2,42),(2,43)",
	}
	cases := []struct{ name, sql string }{
		// A DERIVED table: anchorNoIndexInPlay skips it ("no index can be reached
		// through one"), but sqlite3 FLATTENS a simple subquery into the outer
		// FROM clause before the planner runs, so the outer aggregate's rows
		// arrive in the FLATTENED plan's order, not the subquery's.
		{"derived-flatten", "SELECT x.a, count(*), x.b, y.c FROM (SELECT * FROM j1) AS x, j2 AS y WHERE x.a=y.a GROUP BY x.a ORDER BY x.a"},
		{"derived-single", "SELECT x.a, count(*), x.b FROM (SELECT * FROM j1) AS x GROUP BY x.a ORDER BY x.a"},
		{"view-join", "SELECT v.a, count(*), v.b, j2.c FROM vj1 AS v, j2 WHERE v.a=j2.a GROUP BY v.a ORDER BY v.a"},
		// FOUR items: past markWherePlanEligibility's 3-item cap.
		{"four-table", "SELECT j1.a, count(*), j1.b, j2.c, j3.d, j4.e FROM j1,j2,j3,j4 WHERE j1.a=j2.a AND j2.a=j3.a AND j3.a=j4.a GROUP BY j1.a ORDER BY j1.a"},
		{"four-table-whole", "SELECT count(*), j1.b, j2.c, j3.d, j4.e FROM j1,j2,j3,j4 WHERE j1.a=j2.a AND j2.a=j3.a AND j3.a=j4.a"},
		// A ROWID in the WHERE: wherePlanRowidConstrained, because the unported
		// whereLoopAddBtreeIndex is what prices the fake sPk loops.
		{"rowid-where", "SELECT j1.a, count(*), j1.b, j2.c FROM j1,j2 WHERE j1.a=j2.a AND j1.rowid>0 GROUP BY j1.a ORDER BY j1.a"},
		{"rowid-where-eq", "SELECT j1.a, count(*), j1.b, j2.c FROM j1,j2 WHERE j1.a=j2.a AND j2.rowid<>9 GROUP BY j1.a ORDER BY j1.a"},
		// A ROWID sort term: wherePlanSortTermRisky.
		{"rowid-order", "SELECT j1.a, count(*), j1.b, j2.c FROM j1,j2 WHERE j1.a=j2.a GROUP BY j1.a ORDER BY j1.a, j1.rowid"},
		// g1 declares a generated column, but this query never REFERENCES it --
		// colUsedBitsFor (engine/where_plan_gate.go) floods colUsed only on an
		// actual reference (sqlite3ExprColUsed, resolve.c:176-198), so this
		// shape is decided by the ported planner now, not declined.
		{"generated", "SELECT g1.a, count(*), g1.b, j2.c FROM g1,j2 WHERE g1.a=j2.a GROUP BY g1.a ORDER BY g1.a"},
		// ANALYZE replaces build.c's default row estimate per table.
		{"analyzed", "SELECT j1.a, count(*), j1.b, j2.c FROM j1,j2 WHERE j1.a=j2.a GROUP BY j1.a ORDER BY j1.a"},
		// A RIGHT JOIN pins the whole FROM clause to literal FROM order on both
		// sides, so this one must still be ANSWERED -- see anchorLoopOrderProvable.
		{"right-join", "SELECT j1.a, count(*), j1.b, j2.c FROM j1 RIGHT JOIN j2 ON j1.a=j2.a GROUP BY j1.a ORDER BY j1.a"},
		// A NESTED compile: nQueryLoop is non-zero inside an outer loop.
		{"nested", "SELECT (SELECT j1.b || '/' || count(*) FROM j1,j2 WHERE j1.a=j2.a AND j1.a=o.a) FROM (SELECT 1 AS a UNION ALL SELECT 2) AS o ORDER BY o.a"},
		// An OUTER join whose ON cannot move (onClausesStayInScope) -- but this
		// is also the EXACTLY-TWO-ITEMS, second-is-LEFT shape anchorLoopOrderProvable
		// now decides structurally (where.c:4938/4968-4990) without asking the
		// ported planner at all, so it too must be ANSWERED regardless of what
		// the ON clause contains.
		{"outer-on", "SELECT j1.a, count(*), j1.b, j2.c FROM j1 LEFT JOIN j2 ON j1.a=j2.a AND j2.c>0 GROUP BY j1.a ORDER BY j1.a"},
		// A parenthesized join GROUP, which computeExecOrder collapses.
		{"paren-group", "SELECT j1.a, count(*), j1.b, j2.c, j3.d FROM j1, (j2 JOIN j3 ON j2.a=j3.a) WHERE j1.a=j2.a GROUP BY j1.a ORDER BY j1.a"},
	}
	for _, c := range cases {
		stmts := append([]string(nil), base...)
		switch c.name {
		case "view-join":
			stmts = append(stmts, "CREATE VIEW vj1 AS SELECT * FROM j1")
		case "generated":
			stmts = append(stmts,
				"CREATE TABLE g1(a, b, s AS (a+b))",
				"INSERT INTO g1(a,b) VALUES(1,10),(1,11),(2,12),(2,13)")
		case "analyzed":
			stmts = append(stmts, "ANALYZE")
		}
		stmts = append(stmts, c.sql)
		answered, cgo, mush := r28Answered(t, stmts)
		switch {
		case string(cgo) == string(mush):
			// agreed -- either both answered, or both errored
		case !answered:
			t.Logf("DECLINED  %-16s %s", c.name, c.sql)
			if c.name == "right-join" || c.name == "outer-on" {
				t.Errorf("%s: a 2-item FROM clause with an OUTER join is pinned to FROM order and must still be answered", c.name)
			}
		default:
			t.Errorf("[r28holes] %s SERVED A DIFFERENT ANSWER\n  sql:    %s\n  cgo:    %s\n  musql: %s",
				c.name, c.sql, cgo, mush)
		}
	}
}

// TestR28AnchorHoleFuzz is the gate that actually matters for the loop-order
// half, because a hand-written probe matrix agreeing means nothing here: the
// anchor is VALUE-dependent, and every one of the deterministic cases above
// AGREED at the commit that introduced this file while the randomized sweep
// below found 150 wrong answers in 700 shapes.
//
// Every generated statement is over UNINDEXED tables (so anchorNoIndexInPlay
// answers) and carries a DISQUALIFIER that makes markWherePlanEligibility refuse
// the ported planner, so computeExecOrder's own heuristic decides the nesting.
// The assertion is AGREE-OR-DECLINE. The control-3 bucket -- three plain items,
// which the port DOES decide -- must additionally never be declined, so the
// guard cannot pass by swallowing the subset the port already gets right.
//
// Measured at the commit before the loop-order half existed, with
// R28_AGG=stable (order-insensitive aggregates only, so a group_concat's own
// order-sensitivity cannot be mistaken for the anchor's):
//
//	four FROM items      43/105   a rowid in GROUP BY/ORDER BY  34/105
//	a generated column   31/ 93   a rowid term in the WHERE     17/ 97
//	a DERIVED table      21/ 96   sqlite_stat1 present (ANALYZE) 4/113
//	three plain items     0/ 91  <- the control
func TestR28AnchorHoleFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("randomized anchor loop-order sweep")
	}
	rng := rand.New(rand.NewSource(r26EnvInt("R28_SEED", 0x28a1)))
	type tally struct{ n, diverged, declined int }
	byShape := map[string]*tally{}
	for iter := 0; iter < int(r26EnvInt("R28_ITERS", 300)); iter++ {
		shape, stmts := r28HoleCase(rng)
		answered, cgo, mush := r28Answered(t, stmts)
		tl := byShape[shape]
		if tl == nil {
			tl = &tally{}
			byShape[shape] = tl
		}
		tl.n++
		switch {
		case string(cgo) == string(mush):
		case !answered:
			tl.declined++
			if strings.HasPrefix(shape, "control-3") {
				t.Errorf("[r28fuzz] the ported planner decides this shape; it must not decline\n  sql: %v", stmts)
			}
		default:
			tl.diverged++
			t.Errorf("[r28fuzz] %s SERVED A DIFFERENT ANSWER\n  sql:    %v\n  cgo:    %s\n  musql: %s",
				shape, stmts, cgo, mush)
		}
	}
	shapes := make([]string, 0, len(byShape))
	for s := range byShape {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	for _, s := range shapes {
		t.Logf("%-24s %3d shapes, %3d declined, %3d wrong", s, byShape[s].n, byShape[s].declined, byShape[s].diverged)
	}
}

// r28HoleCase builds one unindexed multi-table aggregate reading bare columns,
// plus one disqualifier that keeps the ported planner out of the decision.
func r28HoleCase(rng *rand.Rand) (string, []string) {
	cell := func(n int) string {
		if rng.Intn(6) == 0 {
			return "NULL"
		}
		return fmt.Sprintf("%d", rng.Intn(n))
	}
	rows := func(cols int) string {
		out := make([]string, 3+rng.Intn(4))
		for i := range out {
			vals := make([]string, cols)
			for j := range vals {
				vals[j] = cell(3 + 9*j)
			}
			out[i] = "(" + strings.Join(vals, ",") + ")"
		}
		return strings.Join(out, ",")
	}
	stmts := []string{
		"CREATE TABLE j1(a,b)", "INSERT INTO j1 VALUES" + rows(2),
		"CREATE TABLE j2(a,c)", "INSERT INTO j2 VALUES" + rows(2),
		"CREATE TABLE j3(a,d)", "INSERT INTO j3 VALUES" + rows(2),
	}
	// group_concat is deliberately SEPARABLE: it is order-sensitive in its own
	// right, so a divergence with it in the select list does not prove the
	// bare-column ANCHOR moved. R28_AGG=stable drops it; R28_AGG=concat keeps
	// only it, which measures the order-sensitive-AGGREGATE hazard instead --
	// a SIBLING of this guard (armOrderSensitive, engine/vdbe_agg_codegen.go),
	// since a query whose only order-sensitive thing is a group_concat reads no
	// anchor at all.
	aggs := []string{"count(*)", "sum(j1.b)", "max(j1.b)", "min(j2.c)", "group_concat(j2.c)"}
	switch os.Getenv("R28_AGG") {
	case "stable":
		aggs = aggs[:4]
	case "concat":
		aggs = aggs[4:]
	}
	key := "j1.a"
	agg := aggs[rng.Intn(len(aggs))]
	bare, from, where, extra, sort2 := "j1.b, j2.c", "j1, j2", "j1.a=j2.a", "", ""
	var shape string
	switch rng.Intn(7) {
	case 0: // FOUR items: past markWherePlanEligibility's 3-item cap.
		shape = "four-item"
		stmts = append(stmts, "CREATE TABLE j4(a,e)", "INSERT INTO j4 VALUES"+rows(2))
		from, where = "j1, j2, j3, j4", "j1.a=j2.a AND j2.a=j3.a AND j3.a=j4.a"
		bare = "j1.b, j2.c, j3.d, j4.e"
	case 1: // a ROWID in the WHERE: wherePlanRowidConstrained.
		shape = "rowid-where"
		extra = []string{" AND j1.rowid>0", " AND j2.rowid<>99", " AND j1.rowid=j2.rowid"}[rng.Intn(3)]
	case 2: // a ROWID sort term: wherePlanSortTermRisky.
		shape = "rowid-sort"
		sort2 = ", j1.rowid"
	case 3: // g1 declares a generated column, s, that key/bare/agg below
		// never reference -- no longer a disqualifier (colUsedBitsFor
		// floods colUsed only when a reference actually names the
		// generated column, resolve.c:176-198), so this shape now
		// measures the ORDINARY path over a table that merely has one.
		shape = "generated"
		stmts = append(stmts, "CREATE TABLE g1(a,b,s AS (a+b))", "INSERT INTO g1(a,b) VALUES"+rows(2))
		key, from, where, bare = "g1.a", "g1, j2", "g1.a=j2.a", "g1.b, j2.c"
		agg = strings.ReplaceAll(agg, "j1.", "g1.")
	case 4: // ANALYZE data: per-table row estimates replace the default 200.
		shape = "analyzed"
		stmts = append(stmts, "ANALYZE")
	case 5: // a DERIVED table, which sqlite3 FLATTENS into the outer FROM.
		shape = "derived"
		from = "(SELECT * FROM j1) AS j1, j2"
	default: // THREE plain items, eligible: the control that must stay served.
		shape = "control-3"
		from, where = "j1, j2, j3", "j1.a=j2.a AND j2.a=j3.a"
		bare = "j1.b, j2.c, j3.d"
	}
	// R28_NOBARE=1 drops the bare columns, which takes the anchor out of the
	// query entirely: anchorIsRead is then false and this guard declines
	// nothing. Combined with R28_AGG=concat it measures the SIBLING guard
	// instead -- an order-sensitive AGGREGATE over an unprovable loop order:
	//
	//	before armOrderSensitive  48 wrong, 38 declined / 300
	//	after                      0 wrong, 149 declined / 300
	//
	// The 38 declines that were already there are the rowid-SORT shapes, whose
	// "ORDER BY j1.rowid" is itself a bare column, so the anchor covered them by
	// accident; control-3, the shapes the port DOES decide, is 0 wrong and 0
	// declined in both columns.
	if os.Getenv("R28_NOBARE") != "" {
		bare = agg
	}
	if sort2 == "" && rng.Intn(3) == 0 {
		// whole-table: exactly one group, so no ORDER BY is needed for exactness
		stmts = append(stmts, fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s%s", agg, bare, from, where, extra))
		return shape + " whole", stmts
	}
	stmts = append(stmts, fmt.Sprintf("SELECT %s, %s, %s FROM %s WHERE %s%s GROUP BY %s ORDER BY %s%s",
		key, agg, bare, from, where, extra, key, key, sort2))
	return shape + " grouped", stmts
}
