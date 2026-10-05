// This file gates collation-aware row deduplication in compound SELECTs.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildCompoundCollateDB builds test tables with various collations.
func buildCompoundCollateDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("compoundcollate_%d.sqlite", pageSize))
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

	exec(`CREATE TABLE nc1 (a TEXT COLLATE NOCASE)`)
	exec(`INSERT INTO nc1 (a) VALUES ('abc')`)
	exec(`INSERT INTO nc1 (a) VALUES ('XYZ')`)
	exec(`INSERT INTO nc1 (a) VALUES (NULL)`)

	exec(`CREATE TABLE nc2 (a TEXT)`)
	exec(`INSERT INTO nc2 (a) VALUES ('ABC')`) // NOCASE-dup of nc1's 'abc'
	exec(`INSERT INTO nc2 (a) VALUES ('xyz')`) // NOCASE-dup of nc1's 'XYZ'
	exec(`INSERT INTO nc2 (a) VALUES ('def')`) // only in nc2
	exec(`INSERT INTO nc2 (a) VALUES (NULL)`)  // NULL-equal dup of nc1's NULL

	exec(`CREATE TABLE rt1 (a TEXT COLLATE RTRIM)`)
	exec(`INSERT INTO rt1 (a) VALUES ('ab')`)
	exec(`INSERT INTO rt1 (a) VALUES ('cd  ')`)

	exec(`CREATE TABLE rt2 (a TEXT)`)
	exec(`INSERT INTO rt2 (a) VALUES ('ab  ')`) // RTRIM-dup of rt1's 'ab'
	exec(`INSERT INTO rt2 (a) VALUES ('cd')`)   // RTRIM-dup of rt1's 'cd  '
	exec(`INSERT INTO rt2 (a) VALUES ('ef')`)   // only in rt2

	exec(`CREATE TABLE pl1 (a TEXT)`)
	exec(`INSERT INTO pl1 (a) VALUES ('abc')`)
	exec(`INSERT INTO pl1 (a) VALUES ('ghi')`)

	exec(`CREATE TABLE pl2 (a TEXT COLLATE NOCASE)`)
	exec(`INSERT INTO pl2 (a) VALUES ('ABC')`) // would NOCASE-dup pl1's 'abc' IF pl2's collation mattered -- it must not (pl2 is not leftmost)
	exec(`INSERT INTO pl2 (a) VALUES ('jkl')`)

	exec(`CREATE TABLE arm1 (a TEXT)`)
	exec(`INSERT INTO arm1 (a) VALUES ('abc')`)
	exec(`CREATE TABLE arm2 (a TEXT)`)
	exec(`INSERT INTO arm2 (a) VALUES ('ABC')`)
	exec(`CREATE TABLE arm3 (a TEXT)`)
	exec(`INSERT INTO arm3 (a) VALUES ('aBc')`)

	exec(`CREATE TABLE wide1 (a TEXT COLLATE NOCASE, b INTEGER)`)
	exec(`INSERT INTO wide1 (a,b) VALUES ('abc', 1)`)
	exec(`INSERT INTO wide1 (a,b) VALUES ('abc', 2)`)
	exec(`CREATE TABLE wide2 (a TEXT, b INTEGER)`)
	exec(`INSERT INTO wide2 (a,b) VALUES ('ABC', 1)`) // dups wide1's ('abc',1) under (NOCASE,plain)
	exec(`INSERT INTO wide2 (a,b) VALUES ('ABC', 3)`) // distinct: b differs

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// compoundCollateUnorderedCorpus are queries whose result SET must match --
// row order irrelevant, but each surviving row's exact byte content still
// must match (a collation-driven dedup can collapse two byte-different rows
// into one, and which one's bytes survive is part of the observable
// contract -- see combineCompound's unionMerge doc comment).
var compoundCollateUnorderedCorpus = []string{
	// --- UNION dedup via a DECLARED collation on the LEFTMOST arm. ---
	"SELECT a FROM nc1 UNION SELECT a FROM nc2",
	"SELECT a FROM rt1 UNION SELECT a FROM rt2",

	// --- INTERSECT/EXCEPT via a DECLARED collation on the LEFTMOST arm. ---
	"SELECT a FROM nc1 INTERSECT SELECT a FROM nc2",
	"SELECT a FROM nc1 EXCEPT SELECT a FROM nc2",
	"SELECT a FROM nc2 EXCEPT SELECT a FROM nc1",
	"SELECT a FROM rt1 INTERSECT SELECT a FROM rt2",
	"SELECT a FROM rt1 EXCEPT SELECT a FROM rt2",

	// --- Regression lock: a declared non-BINARY collation on a NON-leftmost
	// arm must NOT affect dedup at all (verified directly against real
	// SQLite -- see buildCompoundCollateDB's own doc comment for pl1/pl2). ---
	"SELECT a FROM pl1 UNION SELECT a FROM pl2",
	"SELECT a FROM pl1 INTERSECT SELECT a FROM pl2",
	"SELECT a FROM pl1 EXCEPT SELECT a FROM pl2",

	// --- Per-column independence: only column a carries NOCASE; column b
	// (plain INTEGER) still distinguishes otherwise-NOCASE-equal rows. ---
	"SELECT a, b FROM wide1 UNION SELECT a, b FROM wide2",

	// --- WHERE narrows each arm before combining; still collation-aware. ---
	"SELECT a FROM nc1 WHERE a IS NOT NULL UNION SELECT a FROM nc2 WHERE a IS NOT NULL",

	// --- UNION dedup via an EXPLICIT collation on the LEFTMOST arm's own
	// select-list item (as opposed to a column's declared collation). ---
	"SELECT a COLLATE NOCASE FROM pl1 UNION SELECT a FROM pl2",
	"SELECT a COLLATE NOCASE FROM pl1 INTERSECT SELECT a FROM pl2",
	"SELECT a COLLATE NOCASE FROM pl1 EXCEPT SELECT a FROM pl2",
	// 3-arm UNION chain with an explicit COLLATE on the leftmost arm.
	"SELECT a COLLATE NOCASE FROM arm1 UNION SELECT a FROM arm2 UNION SELECT a FROM arm3",
}

// compoundCollateOrderedCorpus are queries whose result ROW ORDER must
// match exactly: a compound-level ORDER BY inherits its collation from
// ONLY the leftmost arm's own select-list expression (declared or
// explicit) -- the SAME rule combineCompound's own dedup uses -- never a
// later arm's, even when a later arm has one and the leftmost doesn't.
var compoundCollateOrderedCorpus = []string{
	// ORDER BY over a leftmost DECLARED-NOCASE column: case-insensitive sort.
	"SELECT a FROM nc1 UNION ALL SELECT a FROM nc2 ORDER BY a",
	"SELECT a FROM nc1 UNION ALL SELECT a FROM nc2 ORDER BY a DESC",
	"SELECT a FROM nc1 UNION ALL SELECT a FROM nc2 ORDER BY 1",

	// ORDER BY does NOT inherit a non-leftmost arm's declared collation:
	// pl2's NOCASE (2nd arm) must NOT affect this sort, which stays BINARY.
	"SELECT a FROM pl1 UNION ALL SELECT a FROM pl2 ORDER BY a",

	// Deduped (UNION, not ALL) + ORDER BY together, over a declared-RTRIM
	// leftmost column.
	"SELECT a FROM rt1 UNION SELECT a FROM rt2 ORDER BY a",

	// LIMIT/OFFSET applied to the collation-aware deduped-and-ordered result.
	"SELECT a FROM nc1 UNION SELECT a FROM nc2 ORDER BY a LIMIT 2",
	"SELECT a FROM nc1 UNION SELECT a FROM nc2 ORDER BY a LIMIT 2 OFFSET 1",

	// ORDER BY resolving (ordinally) to a leftmost EXPLICIT-COLLATE item.
	"SELECT a COLLATE NOCASE FROM pl1 UNION ALL SELECT a FROM pl2 ORDER BY 1",

	// --- compoundOrderByCollateNameQuirk: convertCompoundSelectToSubquery's
	// column-NAME-only rewrite (select.c:5522-5579), mined from SQLite's own
	// selectE.test (ticket 6709574d2a8d8b9be3a9cb1afbf4ff2de48ea4e7). A
	// non-"UNION ALL" connective (EXCEPT here) plus an ORDER BY term with its
	// OWN explicit COLLATE renames the leftmost arm's unaliased "a COLLATE
	// NOCASE" select item down to bare "a" -- see compoundHasNonUnionAllOp
	// (engine/sql_compound.go) and its caller in compileCompound
	// (engine/vdbe_compound_codegen.go). ---
	"SELECT a COLLATE NOCASE FROM pl1 EXCEPT SELECT a FROM pl2 ORDER BY 1 COLLATE BINARY",
	// Same select list, ORDER BY with no explicit COLLATE of its own: the
	// rewrite's trigger condition is unmet, so the full name survives.
	"SELECT a COLLATE NOCASE FROM pl1 EXCEPT SELECT a FROM pl2 ORDER BY 1",
	// Pure UNION ALL: convertCompoundSelectToSubquery's own p->pPrior walk
	// (select.c:5540-5546) never fires for an all-"UNION ALL" chain, so the
	// name survives even with an ORDER BY COLLATE term present.
	"SELECT a COLLATE NOCASE FROM pl1 UNION ALL SELECT a FROM pl2 ORDER BY 1 COLLATE BINARY",
}

// TestCompoundCollateTableParity is the table-driven hard gate: every
// corpus statement must produce identical results (including exact
// surviving-row byte content for a collation-driven dedup) through the
// integrated engine path and C SQLite, at both page sizes 512 and 4096.
func TestCompoundCollateTableParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildCompoundCollateDB(t, pageSize)

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
			runCase := func(sqlText string, orderSensitive bool) {
				total++
				eCols, eVals, eErr := p.Query(sqlText)
				cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

				if eErr != nil || cErr != nil {
					wrong++
					t.Errorf("[%s] unexpected error\n  engine=%v\n  cgo=%v", sqlText, eErr, cErr)
					return
				}

				eRows := engineRowsToStrings(eVals)

				if ok, reason := queryResultsMatch(eCols, eRows, cCols, cRows, orderSensitive); !ok {
					wrong++
					t.Errorf("[%s] engine DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
						sqlText, reason, eCols, eRows, cCols, cRows)
				}

				// A declared-collation compound compiles fully through the
				// VDBE (compileCompound handles it directly), so QueryVDBE
				// must succeed and match too -- not just a bonus check.
				vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
				if vErr != nil {
					wrong++
					t.Errorf("[%s] QueryVDBE: expected a collation-aware compound to compile, got: %v", sqlText, vErr)
					return
				}
				vRows := engineRowsToStrings(vVals)
				if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, orderSensitive); !ok {
					wrong++
					t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
						sqlText, reason, vCols, vRows, cCols, cRows)
				}
			}

			for _, sqlText := range compoundCollateUnorderedCorpus {
				runCase(sqlText, false)
			}
			for _, sqlText := range compoundCollateOrderedCorpus {
				runCase(sqlText, true)
			}

			t.Logf("compound SELECT collation-aware dedup parity gate (pagesize=%d): %d statements, wrong=%d", pageSize, total, wrong)
			if wrong != 0 {
				t.Fatalf("compound collation parity gate FAILED: wrong=%d (must be 0)", wrong)
			}
		})
	}
}
