// This file tests bare-column aggregate queries, where a non-aggregate column appears
// in the SELECT list of a GROUP BY query.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

func buildGroupByBareDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("gbbare_%d.sqlite", pageSize))
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
	exec(`CREATE TABLE t (g INTEGER, v INTEGER, label TEXT)`)
	rows := []struct {
		g     int
		v     int
		label string
	}{
		{1, 10, "a10"}, {1, 30, "a30"}, {1, 20, "a20"},
		{2, 5, "b5"}, {2, 15, "b15"},
		{3, 42, "c42"},
		{4, -7, "d-7"}, {4, 3, "d3"}, {4, 100, "d100"},
	}
	for _, r := range rows {
		exec(fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, '%s')`, r.g, r.v, r.label))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// groupByBareCorpus: honored min/max bare-column queries with a deterministic
// per-group winner. Each carries ORDER BY the group key (or is a single-group
// whole-table aggregate) so row order is pinned.
var groupByBareCorpus = []string{
	`SELECT g, label, max(v) FROM t GROUP BY g ORDER BY g`,
	`SELECT g, label, min(v) FROM t GROUP BY g ORDER BY g`,
	`SELECT g, v, max(v) FROM t GROUP BY g ORDER BY g`,
	`SELECT g, v, min(v) FROM t GROUP BY g ORDER BY g`,
	`SELECT label, max(v) FROM t`,
	`SELECT label, min(v) FROM t`,
	`SELECT v, max(v) FROM t`,
	`SELECT label, v, max(v) FROM t`,
	// bare column alongside max() plus an extra plain aggregate
	`SELECT g, label, max(v), count(*) FROM t GROUP BY g ORDER BY g`,
	// max() over a WHERE-filtered set still anchors correctly
	`SELECT g, label, max(v) FROM t WHERE v >= 0 GROUP BY g ORDER BY g`,
}

// TestGroupByBareColumnMatchesCSQLite gates result PARITY with real C
// SQLite for the bare-column extension, on both QueryVDBE and the integrated
// p.Query path. The VDBE now implements the extension directly (see
// aggAccumulators.anchorRow, engine/vdbe_agg.go): a bare select-list column
// takes its value from the row that produced the query's honored min()/max()
// when there is one, from the group's FIRST row when the query has no
// min/max at all, and from the group's LAST row when a min/max is present
// but never found a non-NULL winner. Every corpus statement below has a
// deterministic per-group winner, so SQLite's own pick is reproducible and
// must match exactly.
func TestGroupByBareColumnMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildGroupByBareDB(t, pageSize)

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
			for _, sqlText := range groupByBareCorpus {
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
				if cErr != nil {
					wrong++
					t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
					continue
				}
				for _, run := range []struct {
					name string
					fn   func() ([]string, [][]engine.Value, error)
				}{
					{"QueryVDBE", func() ([]string, [][]engine.Value, error) { return p.QueryArgs(sqlText, nil) }},
					{"Query", func() ([]string, [][]engine.Value, error) { return p.Query(sqlText) }},
				} {
					vCols, vVals, vErr := run.fn()
					if vErr != nil {
						wrong++
						t.Errorf("[%s] %s: unexpected error: %v", sqlText, run.name, vErr)
						continue
					}
					if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, false); !ok {
						wrong++
						t.Errorf("[%s] %s DIVERGES from C SQLite: %s\n  got:  cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
							sqlText, run.name, reason, vCols, engineRowsToStrings(vVals), cCols, cRows)
					}
				}
			}
			t.Logf("GROUP BY bare-column parity (pagesize=%d): %d statements, wrong=%d", pageSize, len(groupByBareCorpus), wrong)
			if wrong != 0 {
				t.Fatalf("GROUP BY bare-column parity FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
