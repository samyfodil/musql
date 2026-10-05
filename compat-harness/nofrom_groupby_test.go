package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// A FROM-less GROUP BY changes no answer -- with one synthetic row there is
// exactly one group -- so the clause is pure validation. Every case here was
// verified directly against mattn/go-sqlite3 3.53.3; see
// engine/query.go's validateNoFromGroupBy for the derived rules.
var noFromGroupByStmts = []string{
	`SELECT 986 AS x GROUP BY X ORDER BY X`,
	`SELECT 1 GROUP BY 1`,
	`SELECT 1 GROUP BY 2`,
	`SELECT 1 GROUP BY 0`,
	`SELECT 1 GROUP BY -1`,
	`SELECT 1 AS a GROUP BY a HAVING a>0`,
	`SELECT 1 AS a GROUP BY a HAVING a>5`,
	`SELECT count(*) GROUP BY 1`,
	`SELECT count(*) AS c GROUP BY c`,
	`SELECT 1 GROUP BY NULL`,
	`SELECT 1 GROUP BY random()`,
	`SELECT 1 GROUP BY nosuchcol`,
	`SELECT 1 AS a, 2 AS b GROUP BY a, b`,
	`SELECT 1 AS a, 2 AS b GROUP BY 3`,
	`SELECT 1 GROUP BY 1 HAVING count(*)>0`,
	`SELECT 1 GROUP BY 1 HAVING count(*)>1`,
	`SELECT sum(3) GROUP BY 1`,
	`SELECT max(4) AS m GROUP BY m`,
	`SELECT 1 WHERE 0 GROUP BY 1`,
	`SELECT 1 GROUP BY 1 ORDER BY 1 LIMIT 0`,
	`SELECT 'a' GROUP BY 1 COLLATE nocase`,
	`SELECT 1 GROUP BY (SELECT 2)`,
	`SELECT 1 GROUP BY 1, 1`,
	`SELECT 'x' AS a GROUP BY a, 1`,
	`SELECT 2+3 GROUP BY 1`,
	`SELECT 1 GROUP BY 1+1`,
	`SELECT abs(-7) AS a GROUP BY a`,
}

// noFromGroupByDeclined are the shapes this engine deliberately refuses: a
// FROM-less GROUP BY combined with HAVING. C SQLite runs the HAVING against
// the single synthetic group ("HAVING a>5" -> no rows, "HAVING count(*)>1" ->
// no rows because that group holds exactly ONE row), which means keeping the
// statement aggregate-shaped rather than dropping the clause -- this engine
// accepts HAVING only on an aggregate query. Pinned here so the boundary is a
// decision with a test behind it, and so the day it is served this gate says so.
var noFromGroupByDeclined = map[string]bool{
	`SELECT 1 AS a GROUP BY a HAVING a>0`:   true,
	`SELECT 1 AS a GROUP BY a HAVING a>5`:   true,
	`SELECT 1 GROUP BY 1 HAVING count(*)>0`: true,
	`SELECT 1 GROUP BY 1 HAVING count(*)>1`: true,
}

func TestNoFromGroupByParity(t *testing.T) {
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	for _, q := range noFromGroupByStmts {
		t.Run(q, func(t *testing.T) {
			goCols, goRows, gerr, panicked, panicVal := tclSafeGoQuery(godb, q)
			if panicked {
				t.Fatalf("engine PANICKED: %v", panicVal)
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, q)
			if noFromGroupByDeclined[q] {
				if gerr == nil {
					t.Fatalf("newly SERVED -- move it out of noFromGroupByDeclined and assert the answer")
				}
				if cerr != nil {
					t.Fatalf("premise gone: cgo now rejects it too: %v", cerr)
				}
				return
			}
			if (gerr != nil) != (cerr != nil) {
				t.Fatalf("accept/reject disagrees\n  go:  %v\n  cgo: %v", gerr, cerr)
			}
			if gerr != nil {
				// Both rejected. The messages need not match word for word,
				// but the DIAGNOSIS must: an out-of-range ordinal, an
				// aggregate in the clause and an unknown column are three
				// different reasons and confusing them would hide a real gap.
				g, c := strings.ToLower(gerr.Error()), strings.ToLower(cerr.Error())
				for _, kw := range []string{"out of range", "aggregate", "no such column"} {
					if strings.Contains(c, kw) != strings.Contains(g, kw) {
						t.Fatalf("both rejected but for different reasons (%q)\n  go:  %v\n  cgo: %v", kw, gerr, cerr)
					}
				}
				return
			}
			if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
				t.Fatalf("%s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", reason, goCols, goRows, cgoCols, cgoRows)
			}
		})
	}
}
