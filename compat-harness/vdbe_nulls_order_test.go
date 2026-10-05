// This file gates the ORDER BY NULLS FIRST/NULLS LAST modifier.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildNullsOrderDB builds a test database with NULLs and mixed values.
func buildNullsOrderDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("nullsorder_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	exec := func(sqlText string) {
		t.Helper()
		if err := db.Exec(sqlText); err != nil {
			t.Fatalf("Exec(%s): %v", sqlText, err)
		}
	}

	exec(`CREATE TABLE nullord (id INTEGER PRIMARY KEY, x INTEGER, y TEXT)`)
	rows := []struct {
		id int
		x  any
		y  any
	}{
		{1, nil, "b"},
		{2, 5, "A"},
		{3, nil, "a"},
		{4, 3, "B"},
		{5, 8, nil},
		{6, 3, "c"},
		{7, 1, nil},
		{8, 5, "z"},
	}
	for _, r := range rows {
		xLit := "NULL"
		if r.x != nil {
			xLit = fmt.Sprintf("%v", r.x)
		}
		yLit := "NULL"
		if r.y != nil {
			yLit = fmt.Sprintf("'%v'", r.y)
		}
		exec(fmt.Sprintf(`INSERT INTO nullord VALUES (%d, %s, %s)`, r.id, xLit, yLit))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// nullsOrderCorpus are ORDER BY queries whose result ROW ORDER must match
// exactly through the VDBE (both QueryVDBE directly and the integrated
// p.Query path), covering:
//
//   - the DEFAULT (no NULLS clause) for both ASC and DESC -- verifying this
//     feature did not change the pre-existing default (NULL is the smallest
//     value: first for ASC, last for DESC);
//   - each explicit clause that AGREES with the default (ASC NULLS FIRST,
//     DESC NULLS LAST) -- a same-answer sanity check that parsing/plumbing
//     the explicit form doesn't accidentally change anything;
//   - each explicit clause that OVERRIDES the default (ASC NULLS LAST, DESC
//     NULLS FIRST -- the tricky non-default one, exercised by nulls1.test);
//   - a multi-term ORDER BY mixing independent directions/NULLS per column;
//   - NULLS combined with an explicit COLLATE on the same term;
//   - GROUP BY's own ORDER BY plan (sql_group.go's orderPlan.nulls) and a
//     compound SELECT's ORDER BY (sql_compound.go/vdbe_compound_codegen.go),
//     so every one of this feature's sort sites gets covered, not just the
//     plain single-table SELECT path.
//
// "id" is appended as a final ASC tiebreak wherever x/y alone wouldn't fully
// disambiguate (x has two rows of 3 and two of 5 and two NULLs; y has two
// NULLs), so the correct order is never ambiguous.
var nullsOrderCorpus = []string{
	// ---- default (no NULLS clause): must be UNCHANGED by this feature ----
	"SELECT id, x FROM nullord ORDER BY x ASC, id",
	"SELECT id, x FROM nullord ORDER BY x DESC, id",

	// ---- explicit clause AGREEING with the default ----
	"SELECT id, x FROM nullord ORDER BY x ASC NULLS FIRST, id",
	"SELECT id, x FROM nullord ORDER BY x DESC NULLS LAST, id",

	// ---- explicit clause OVERRIDING the default ----
	"SELECT id, x FROM nullord ORDER BY x ASC NULLS LAST, id",
	"SELECT id, x FROM nullord ORDER BY x DESC NULLS FIRST, id",

	// ---- bare NULLS clause with no ASC/DESC (defaults to ASC direction) ----
	"SELECT id, x FROM nullord ORDER BY x NULLS FIRST, id",
	"SELECT id, x FROM nullord ORDER BY x NULLS LAST, id",

	// ---- multi-term: independent direction/NULLS choice per column ----
	"SELECT id, x, y FROM nullord ORDER BY x ASC NULLS LAST, y DESC NULLS FIRST, id",
	"SELECT id, x, y FROM nullord ORDER BY y DESC NULLS FIRST, x ASC NULLS LAST, id",

	// ---- GROUP BY's own ORDER BY plan (sql_group.go/vdbe_agg_codegen.go) ----
	"SELECT x, COUNT(*) AS c FROM nullord GROUP BY x ORDER BY x DESC NULLS FIRST",
	"SELECT x, COUNT(*) AS c FROM nullord GROUP BY x ORDER BY x ASC NULLS LAST",

	// ---- SELECT DISTINCT on top of GROUP BY: the VDBE's separate
	// OpGroupBatchFinal/groupBatchFinal batch-sort path
	// (vdbe_agg_codegen.go's useDistinctBatch / vdbe_group_distinct.go), a
	// third, distinct-from-the-plain-hasOrder-sorter2-path ORDER BY sort
	// site this feature must also cover.
	"SELECT DISTINCT x FROM nullord GROUP BY x ORDER BY x DESC NULLS FIRST",
	"SELECT DISTINCT x FROM nullord GROUP BY x ORDER BY x ASC NULLS LAST",

	// ---- compound SELECT's ORDER BY (sql_compound.go/vdbe_compound_codegen.go) ----
	"SELECT x FROM nullord WHERE id <= 4 UNION SELECT x FROM nullord WHERE id > 4 ORDER BY x DESC NULLS FIRST",
	"SELECT x FROM nullord WHERE id <= 4 UNION SELECT x FROM nullord WHERE id > 4 ORDER BY x ASC NULLS LAST",

	// ---- NULLS combined with an explicit COLLATE on the same ORDER BY term
	// (compileExpr's CollateExpr case is a pure pass-through, and
	// compileScanSorted's coll[] resolution -- vdbe_sort_codegen.go --
	// already recovers the explicit collation from the ORIGINAL term
	// independent of the NULLS clause, so this combination is a hard parity
	// requirement, not a decline) ----
	"SELECT id, y FROM nullord ORDER BY y COLLATE NOCASE ASC NULLS LAST, id",
	"SELECT id, y FROM nullord ORDER BY y COLLATE NOCASE DESC NULLS FIRST, id",
	"SELECT id, y FROM nullord ORDER BY y COLLATE NOCASE NULLS FIRST, id",
}

// TestNullsOrderParity is the table-driven hard gate, at both page sizes 512
// and 4096: every nullsOrderCorpus statement must produce identical ROW ORDER
// through the VDBE (both QueryVDBE directly and the integrated p.Query path),
// compared against C SQLite.
func TestNullsOrderParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildNullsOrderDB(t, pageSize)

			cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			wrong := 0
			total := 0
			runCase := func(sqlText string) {
				total++
				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if eErr != nil || cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					return
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
				if vErr != nil {
					wrong++
					t.Errorf("[%s] QueryVDBE: expected this NULLS-ordering shape to compile, got: %v", sqlText, vErr)
					return
				}
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, true); !ok {
					wrong++
					t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
						sqlText, reason, vCols, vRows, cCols, cRows)
				}
			}

			for _, sqlText := range nullsOrderCorpus {
				runCase(sqlText)
			}

			t.Logf("NULLS ORDER parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("NULLS ORDER parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
