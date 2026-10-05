package compat

// Deterministic three-table loop-order test without automatic indexing.
import (
	"encoding/json"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestR27ThreeTableLoopOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(273))
	const nCase = 180
	const batch = 20

	type gen struct {
		setup []string
		query string
	}
	cases := make([]gen, nCase)
	for id := range cases {
		sfx := strconv.Itoa(id)
		names := [3]string{"ta" + sfx, "tb" + sfx, "tc" + sfx}
		tags := [3]string{"a", "b", "c"}
		var setup []string
		for k, n := range names {
			nRow := 2 + rng.Intn(3)
			perm := rng.Perm(nRow)
			rows := make([]string, nRow)
			for r := 0; r < nRow; r++ {
				rows[r] = "('" + tags[k] + strconv.Itoa(perm[r]) + "'," + strconv.Itoa(rng.Intn(4)) + ")"
			}
			setup = append(setup,
				"CREATE TABLE "+n+"(v,f)",
				"INSERT INTO "+n+" VALUES"+strings.Join(rows, ","))
		}

		// Connectors between item 0-1 and 1-2. A LEFT join's ON names only
		// its own table, so it can never become an index-driving equality.
		var leftJoined [3]bool
		conn := func(k int, cur string) string {
			switch rng.Intn(4) {
			case 0:
				return ", " + cur
			case 1:
				return " CROSS JOIN " + cur
			case 2:
				return " JOIN " + cur + " ON " + cur + ".f>=0"
			default:
				leftJoined[k] = true
				return " LEFT JOIN " + cur + " ON " + cur + ".f>" + strconv.Itoa(rng.Intn(3))
			}
		}
		from := names[0] + conn(1, names[1]) + conn(2, names[2])

		// A WHERE of zero to three single-table inequality restrictions --
		// which is precisely what SQLite hoists and this engine used to sink.
		//
		// Never one on a LEFT-joined table: a WHERE term that cannot be true
		// of a NULL row triggers SQLite's OUTER JOIN STRENGTH REDUCTION
		// (select.c tag-select-0220, sqlite3ExprImpliesNonNullRow), which
		// rewrites the LEFT JOIN into an inner one BEFORE the planner runs and
		// so removes the reorder barrier entirely. That rewrite is not ported
		// -- see the note in engine/where_plan_gate.go -- and it is a separate
		// piece of work from the loop order this gate is about.
		var preds []string
		for k, n := range names {
			if leftJoined[k] && os.Getenv("R27_LEFT_PREDS") == "" {
				continue
			}
			switch rng.Intn(3) {
			case 0:
				preds = append(preds, n+".f>"+strconv.Itoa(rng.Intn(3)))
			case 1:
				preds = append(preds, n+".v<>'zz'")
			}
		}
		where := ""
		if len(preds) > 0 {
			where = " WHERE " + strings.Join(preds, " AND ")
		}

		sel := "group_concat(coalesce(" + names[0] + ".v,'-')||coalesce(" +
			names[1] + ".v,'-')||coalesce(" + names[2] + ".v,'-'))"
		if rng.Intn(3) == 0 {
			cases[id] = gen{setup, "SELECT " + names[0] + ".v, " + names[2] +
				".v FROM " + from + where + " LIMIT 4"}
			continue
		}
		cases[id] = gen{setup, "SELECT " + sel + " FROM " + from + where}
	}

	diverged := 0
	for start := 0; start < nCase; start += batch {
		end := start + batch
		if end > nCase {
			end = nCase
		}
		var stmts []string
		qAt := make([]int, 0, batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			cb, _ := json.Marshal(cgo[qAt[k]])
			mb, _ := json.Marshal(mush[qAt[k]])
			if string(cb) != string(mb) {
				diverged++
				if diverged <= 5 {
					t.Errorf("three-table loop order DIVERGES\n  %s\n  %s\n  cgo:    %s\n  musql: %s",
						strings.Join(c.setup, "; "), c.query, cb, mb)
				}
			}
		}
	}
	if diverged > 0 {
		t.Errorf("three-table loop order: %d/%d shapes diverge", diverged, nCase)
	}
}
