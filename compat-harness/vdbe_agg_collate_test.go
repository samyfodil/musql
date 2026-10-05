// Tests that aggregates use the correct collation for their arguments.
// min()/max() pick their extremum using the argument's collation, and
// DISTINCT dedup is also collation-aware.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggCollateDB seeds case-variant text ('abc'/'ABC') plus a
// trailing-space value, in one plain-BINARY column (y) and one declared
// NOCASE column (z), so a query can select the collation either way: an
// explicit COLLATE on y, or a bare reference to z.
func buildAggCollateDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("aggcollate_%d.sqlite", pageSize))
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
	exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, y TEXT, z TEXT COLLATE NOCASE, g INTEGER)`)
	for i, r := range []struct {
		y, z string
		g    int
	}{
		{"abc", "abc", 1},
		{"ABC", "ABC", 1},
		{"BCD", "BCD", 1},
		{"def ", "def ", 2},
		{"DEF", "DEF", 2},
	} {
		exec(fmt.Sprintf(`INSERT INTO t(id,y,z,g) VALUES (%d,'%s','%s',%d)`, i+1, r.y, r.z, r.g))
	}
	// A NULL contributes to no aggregate on either side.
	exec(`INSERT INTO t(id,y,z,g) VALUES (6,NULL,NULL,2)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggCollateUnorderedCorpus: each statement's result must match C SQLite
// exactly. Every one of these returned a BINARY answer before argCollation
// existed, except the "no collation in play" baselines, which are here to
// prove the fix did not perturb them.
var aggCollateUnorderedCorpus = []string{
	// Baselines: plain BINARY argument, unchanged by the fix.
	"SELECT min(y), max(y) FROM t",
	"SELECT count(DISTINCT y) FROM t",

	// Explicit COLLATE on the argument selects the sequence.
	"SELECT min(y COLLATE nocase), max(y COLLATE nocase) FROM t",
	"SELECT min(y COLLATE rtrim), max(y COLLATE rtrim) FROM t",
	"SELECT min(y COLLATE binary), max(y COLLATE binary) FROM t",
	"SELECT count(DISTINCT y COLLATE nocase) FROM t",
	"SELECT count(DISTINCT y COLLATE rtrim) FROM t",
	"SELECT group_concat(DISTINCT y COLLATE nocase) FROM t",

	// A bare column's DECLARED collation does the same job.
	"SELECT min(z), max(z) FROM t",
	"SELECT count(DISTINCT z) FROM t",
	// ...and an explicit COLLATE overrides that declared one.
	"SELECT min(z COLLATE binary), max(z COLLATE binary) FROM t",
	"SELECT count(DISTINCT z COLLATE binary) FROM t",

	// The collation rides through CAST/unary-"+"/"||" exactly as
	// topExprCollation/exprCollation define, not just on a bare column.
	"SELECT min(CAST(y AS TEXT) COLLATE nocase) FROM t",
	"SELECT min(y||'' COLLATE nocase) FROM t",

	// Per-group accumulators each resolve it independently.
	"SELECT g, min(z), count(DISTINCT z) FROM t GROUP BY g",
	"SELECT g, min(y COLLATE nocase) FROM t GROUP BY g",

	// Collation never applies to non-TEXT values: an integer argument's
	// min/max and DISTINCT are byte-identical either way.
	"SELECT min(id COLLATE nocase), count(DISTINCT id COLLATE nocase) FROM t",
}

// TestAggCollateParity is the hard gate: the integrated engine path (and the
// VDBE directly, when it compiles the statement) must agree with real C
// SQLite on every aggCollateUnorderedCorpus statement, at both page sizes.
func TestAggCollateParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildAggCollateDB(t, pageSize)

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

			for _, sqlText := range aggCollateUnorderedCorpus {
				t.Run(sqlText, func(t *testing.T) {
					runCollateColumnCase(t, p, cdb, sqlText, false)
				})
			}
		})
	}
}
