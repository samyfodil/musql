package compat

// This file tests JOIN ON conditions with TRUE/FALSE keywords and subqueries.
//  3. A VIEW referenced in a FROM/JOIN clause (e.g. "t1 JOIN v ON ...",
//     "FROM v" alone). Root cause: resolveJoinSources' base-table branch
//     only ever tried resolveTable (which requires sqlite_schema
//     Type=="table"), so any view reference hard-errored "no such table:
//     <view>" -- even though checkDerivedJoinSupported already anticipated
//     this (fromItemIsDerived, view.go, already treated a view reference as
//     "derived", back when that meant declining NATURAL/USING combined with
//     one -- a view NATURAL/USING-joined now compiles, see
//     derived_natural_join_diff_test.go). Fixed by
//     resolveViewSource (vdbe_join_codegen.go), which compiles the view's
//     own stored SELECT into a derived-table row source exactly like an
//     ordinary parenthesized derived subquery, with the three view-specific
//     differences (unaliased-but-still-qualifiable name, explicit
//     "(col,...)" rename, duplicate-output-name decline) documented on that
//     function.
//
// Follows vdbe_join_test.go's exact parity shape: one shared on-disk
// database (so C SQLite and the engine read the IDENTICAL bytes),
// checked via (a) (*engine.ReadOnlyPager).QueryVDBE directly and (b) real C
// SQLite (github.com/mattn/go-sqlite3), byte-exact (queryResultsMatch,
// tolerating only the documented INTEGER/REAL storage-class optimization
// and, for a query with no fully-ordering ORDER BY, row order).

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildJoinGapFixDB builds a small shared join-key schema (t1/t2/t3, mirroring
// buildVDBEJoinDB's own NULL/duplicate/unmatched-key coverage in miniature)
// plus two views: v1 (no explicit column list, still qualifiable by its own
// name per this file's package doc comment point 3) and v2 (an EXPLICIT
// "(x, y)" rename list, exercising resolveViewSource's column-rename path).
func buildJoinGapFixDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "join_gap_fix.sqlite")
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

	exec(`CREATE TABLE t1 (id INTEGER PRIMARY KEY, a INTEGER, name TEXT)`)
	exec(`INSERT INTO t1 VALUES (1, 10, 'alice')`)
	exec(`INSERT INTO t1 VALUES (2, 20, 'bob')`)
	exec(`INSERT INTO t1 VALUES (3, NULL, 'carol')`) // NULL join key
	exec(`INSERT INTO t1 VALUES (4, 20, 'dave')`)    // duplicate a=20
	exec(`INSERT INTO t1 VALUES (5, 30, 'eve')`)     // unmatched in t2

	exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY, a INTEGER, val TEXT)`)
	exec(`INSERT INTO t2 VALUES (1, 10, 'x')`)
	exec(`INSERT INTO t2 VALUES (2, 20, 'y')`)
	exec(`INSERT INTO t2 VALUES (3, NULL, 'n')`) // NULL join key
	exec(`INSERT INTO t2 VALUES (4, 40, 'w')`)   // unmatched in t1

	exec(`CREATE TABLE t3 (id INTEGER PRIMARY KEY, a INTEGER, tag TEXT)`)
	exec(`INSERT INTO t3 VALUES (1, 10, 'red')`)
	exec(`INSERT INTO t3 VALUES (2, 20, 'blue')`)
	exec(`INSERT INTO t3 VALUES (3, 30, 'green')`)

	// v1: no explicit column list -- verifies an unaliased view reference is
	// still qualifiable by its OWN name ("v1.a", "v1.val" below).
	exec(`CREATE VIEW v1 AS SELECT a, val FROM t2 WHERE a IS NOT NULL`)
	// v2: explicit "(x, y)" rename list -- verifies resolveViewSource's
	// column-rename branch.
	exec(`CREATE VIEW v2 (x, y) AS SELECT a, tag FROM t3`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestVDBEJoinGapFixParity(t *testing.T) {
	path := buildJoinGapFixDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
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

	cases := []joinCase{
		// --- (1) bare TRUE/FALSE in ON ---
		{"SELECT t1.name, t2.val FROM t1 INNER JOIN t2 ON true ORDER BY t1.id, t2.id", true},
		{"SELECT t1.name, t2.val FROM t1 INNER JOIN t2 ON false ORDER BY t1.id, t2.id", true},
		{"SELECT t1.id, t1.name, t2.val FROM t1 LEFT JOIN t2 ON false ORDER BY t1.id", true},
		{"SELECT t1.name, t2.val FROM t1 RIGHT JOIN t2 ON true ORDER BY t2.id, t1.id", true},
		{"SELECT t1.name, t2.val FROM t1 FULL JOIN t2 ON t1.a = t2.a AND TRUE ORDER BY t1.id, t2.id", true},
		{"SELECT DISTINCT t1.name FROM t1 JOIN t2 ON true WHERE t2.val = 'x'", false},

		// --- (2) subquery inside ON, including correlated ---
		{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a AND t2.a IN (SELECT a FROM t3) ORDER BY t1.id, t2.id", true},
		{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a AND t1.a NOT IN (SELECT a FROM t3 WHERE tag = 'green') ORDER BY t1.id, t2.id", true},
		{"SELECT t1.name, t2.val FROM t1 LEFT JOIN t2 ON t1.a = t2.a AND EXISTS (SELECT 1 FROM t3 WHERE t3.a = t1.a) ORDER BY t1.id, t2.id", true},
		{"SELECT t1.name, t2.val FROM t1 JOIN t2 ON t1.a = t2.a AND t2.a = (SELECT MIN(a) FROM t3 WHERE t3.a >= t1.a) ORDER BY t1.id, t2.id", true},
		{"SELECT t1.name FROM t1 LEFT JOIN t2 ON t1.a = t2.a AND NOT EXISTS (SELECT 1 FROM t3 WHERE t3.a = t1.a) ORDER BY t1.id", true},

		// --- (3) views as a JOIN source ---
		{"SELECT * FROM v1 ORDER BY a", true},
		{"SELECT t1.name, v1.val FROM t1 JOIN v1 ON t1.a = v1.a ORDER BY t1.id", true},
		{"SELECT t1.name, v1.a, v1.val FROM t1 LEFT JOIN v1 ON t1.a = v1.a ORDER BY t1.id", true},
		{"SELECT v1.val, t3.tag FROM v1 RIGHT JOIN t3 ON v1.a = t3.a ORDER BY t3.id", true},
		{"SELECT t1.name, v2.y FROM t1 LEFT JOIN v2 ON t1.a = v2.x ORDER BY t1.id", true},
		{"SELECT t1.name, v2.x, v2.y FROM t1 JOIN v2 ON t1.a = v2.x ORDER BY t1.id", true},

		// --- combinations of more than one fix at once ---
		{"SELECT t1.name, v1.val FROM t1 JOIN v1 ON true AND t1.a = v1.a ORDER BY t1.id", true},
		{"SELECT t1.name, v2.y FROM t1 LEFT JOIN v2 ON t1.a = v2.x AND v2.x IN (SELECT a FROM t2) ORDER BY t1.id", true},
	}

	wrong := 0
	for _, tc := range cases {
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("JOIN-gap-fix parity gate: %d statements, wrong=%d", len(cases), wrong)
	if wrong != 0 {
		t.Fatalf("JOIN-gap-fix parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}
