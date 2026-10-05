package compat

// Measurement of plan-order effects on group anchor selection.
// Run with R26_MEASURE=1 environment variable set.

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

func TestR26PlanOrderMeasure(t *testing.T) {
	if os.Getenv("R26_MEASURE") == "" {
		t.Skip("set R26_MEASURE=1 to run the plan-order measurement")
	}
	rng := rand.New(rand.NewSource(r26EnvInt("R26_SEED", 99)))
	type tally struct{ n, diverged int }
	byShape := map[string]*tally{}
	examples := map[string]string{}

	for iter := 0; iter < int(r26EnvInt("R26_ITERS", 400)); iter++ {
		if os.Getenv("R26_JOIN") != "" {
			shape, stmts := r26JoinCase(rng)
			cgo, _ := json.Marshal(run(t, "cgo", stmts))
			mush, _ := json.Marshal(run(t, "musql", stmts))
			tl := byShape[shape]
			if tl == nil {
				tl = &tally{}
				byShape[shape] = tl
			}
			tl.n++
			if string(cgo) != string(mush) {
				tl.diverged++
				if examples[shape] == "" {
					examples[shape] = fmt.Sprintf("%v\n    cgo: %s\n    musql: %s", stmts, cgo, mush)
				}
			}
			continue
		}
		shape, idx := r26IndexShape(rng)
		rows := r26PlanRows(rng)
		q, where := r26PlanQuery(rng)
		stmts := []string{"CREATE TABLE p(a,b,c,d)", "INSERT INTO p VALUES" + rows}
		if idx != "" {
			stmts = append(stmts, idx)
		}
		stmts = append(stmts, q)
		shape += " " + where

		cgo, _ := json.Marshal(run(t, "cgo", stmts))
		mush, _ := json.Marshal(run(t, "musql", stmts))
		tl := byShape[shape]
		if tl == nil {
			tl = &tally{}
			byShape[shape] = tl
		}
		tl.n++
		if string(cgo) != string(mush) {
			tl.diverged++
			if examples[shape] == "" {
				examples[shape] = fmt.Sprintf("%v\n    cgo: %s\n    musql: %s", stmts, cgo, mush)
			}
		}
	}

	shapes := make([]string, 0, len(byShape))
	for s := range byShape {
		shapes = append(shapes, s)
	}
	sort.Strings(shapes)
	for _, s := range shapes {
		tl := byShape[s]
		t.Logf("%-28s %3d/%3d diverged", s, tl.diverged, tl.n)
		if examples[s] != "" {
			t.Logf("    first: %s", examples[s])
		}
	}
}

// r26IndexShape picks one index shape (or none) for table p(a,b,c).
func r26IndexShape(rng *rand.Rand) (name, ddl string) {
	switch rng.Intn(7) {
	case 0:
		return "no-index", ""
	case 1:
		// An index on a column the query never mentions: SQLite has no reason
		// to scan it, so this bucket says whether "any index at all" is too
		// wide a condition.
		return "idx(d-unused)", "CREATE INDEX pd ON p(d)"
	case 2:
		return "idx(a)", "CREATE INDEX pa ON p(a)"
	case 3:
		return "idx(a,b)", "CREATE INDEX pab ON p(a,b)"
	case 4:
		return "idx(a DESC)", "CREATE INDEX pad ON p(a DESC)"
	case 5:
		return "idx(b)", "CREATE INDEX pb ON p(b)"
	default:
		return "unique(a,b,c)", "CREATE UNIQUE INDEX pu ON p(a,b,c)"
	}
}

// r26PlanRows is 5-8 rows over a tiny domain, so groups have several rows and
// the anchor's identity is visible in b/c.
func r26PlanRows(rng *rand.Rand) string {
	n := 5 + rng.Intn(4)
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("(%d,%d,%d,%d)", rng.Intn(3), rng.Intn(100), rng.Intn(100), rng.Intn(100))
	}
	return strings.Join(out, ",")
}

// r26PlanQuery is a grouped (or whole-table) aggregate reading a bare column,
// always with a total ORDER BY so the comparison is exact.
func r26PlanQuery(rng *rand.Rand) (sql, where string) {
	agg := []string{"count(*)", "sum(b)", "max(b)", "min(b)", "max(c)"}[rng.Intn(5)]
	if rng.Intn(5) == 0 {
		return fmt.Sprintf("SELECT %s, a, b, c FROM p", agg), "no-where"
	}
	q := fmt.Sprintf("SELECT a, %s, b, c FROM p", agg)
	switch rng.Intn(3) {
	case 0:
		q += " WHERE a>0"
		where = "where-a"
	case 1:
		q += " WHERE b<50"
		where = "where-b"
	default:
		where = "no-where"
	}
	q += " GROUP BY a"
	if rng.Intn(3) == 0 {
		q += " ORDER BY a DESC"
	} else {
		q += " ORDER BY a"
	}
	return q, where
}

// r26JoinCase is the JOIN half of the same question: SQLite nests join loops by
// cost where emitJoinLoops used to nest in FROM order, so a multi-table
// aggregate's "first row of the group" is a plan choice too. Neither table
// carries an index, which isolates loop order from index order.
//
// This measured 227/600 when it was written, and it is now 0/600: the ported
// planner (engine/where_plan.go) decides these shapes, and where it decides it
// is right. The shapes it does NOT decide are still wrong and are now DECLINED
// instead -- see anchorLoopOrderProvable (engine/vdbe_agg_codegen.go) and
// compat-harness/anchor_r28_holes_test.go, which measures that remainder.
func r26JoinCase(rng *rand.Rand) (name string, stmts []string) {
	rows := func(n int, cols int) string {
		out := make([]string, n)
		for i := range out {
			vals := make([]string, cols)
			for j := range vals {
				vals[j] = fmt.Sprintf("%d", rng.Intn(3+10*j))
			}
			out[i] = "(" + strings.Join(vals, ",") + ")"
		}
		return strings.Join(out, ",")
	}
	stmts = []string{
		"CREATE TABLE j1(a,b)",
		"INSERT INTO j1 VALUES" + rows(4+rng.Intn(3), 2),
		"CREATE TABLE j2(a,c)",
		"INSERT INTO j2 VALUES" + rows(4+rng.Intn(3), 2),
	}
	agg := []string{"count(*)", "sum(j1.b)", "max(j1.b)", "min(j2.c)"}[rng.Intn(4)]
	switch rng.Intn(4) {
	case 0:
		name = "join comma"
		stmts = append(stmts, fmt.Sprintf(
			"SELECT j1.a, %s, j1.b, j2.c FROM j1, j2 WHERE j1.a=j2.a GROUP BY j1.a ORDER BY j1.a", agg))
	case 1:
		name = "join left"
		stmts = append(stmts, fmt.Sprintf(
			"SELECT j1.a, %s, j1.b, j2.c FROM j1 LEFT JOIN j2 ON j1.a=j2.a GROUP BY j1.a ORDER BY j1.a", agg))
	case 2:
		name = "join reversed"
		stmts = append(stmts, fmt.Sprintf(
			"SELECT j2.a, %s, j1.b, j2.c FROM j2, j1 WHERE j1.a=j2.a GROUP BY j2.a ORDER BY j2.a", agg))
	default:
		name = "join whole-table"
		stmts = append(stmts, fmt.Sprintf(
			"SELECT %s, j1.b, j2.c FROM j1, j2 WHERE j1.a=j2.a", agg))
	}
	return name, stmts
}
