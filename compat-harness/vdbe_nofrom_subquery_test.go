package compat

// This file tests FROM-less SELECT statements with subqueries in WHERE and select list.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBENoFromSubqueryDB builds test tables for FROM-less subquery tests.
func buildVDBENoFromSubqueryDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_nofrom_subquery.sqlite"
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

	exec(`CREATE TABLE nt (v INTEGER)`)
	exec(`INSERT INTO nt VALUES (10)`)
	exec(`INSERT INTO nt VALUES (20)`)
	exec(`INSERT INTO nt VALUES (20)`) // duplicate -- membership must not double-count
	exec(`INSERT INTO nt VALUES (NULL)`)

	exec(`CREATE TABLE empty_nt (v INTEGER)`) // genuinely empty

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeNoFromSubqueryCorpus is the gate corpus: every statement is a
// TOP-LEVEL, FROM-less SELECT (no FROM of its own) whose WHERE or select-list
// references a real table only through a subquery.
var vdbeNoFromSubqueryCorpus = []string{
	// --- "X [NOT] IN <table-name>" (desugared to "X IN (SELECT * FROM
	// <table-name>)" by the parser) -- the form this fix specifically
	// targets. ---
	"SELECT 10 IN nt",
	"SELECT 99 IN nt",
	"SELECT 10 NOT IN nt",
	"SELECT 99 NOT IN nt",
	"SELECT NULL IN nt",     // NULL probe, non-empty set, no definite match -> NULL
	"SELECT 10 IN empty_nt", // empty set -> IN is always false, even non-NULL probe
	"SELECT 10 NOT IN empty_nt",
	"SELECT 1 WHERE 10 IN nt",
	"SELECT 1 WHERE 99 IN nt", // WHERE false -> zero rows
	"SELECT 1 WHERE 10 IN nt LIMIT 0",

	// --- the ordinary parenthesized subquery forms, over the same tables,
	// verifying the pager-threading fix doesn't regress or half-compile
	// these (they use the identical compileSubProgram machinery). ---
	"SELECT 10 IN (SELECT v FROM nt)",
	"SELECT 10 IN (SELECT v FROM nt WHERE v > 15)",
	"SELECT (SELECT v FROM nt WHERE v = 10)",
	"SELECT (SELECT v FROM nt WHERE v = 999)", // no rows -> NULL
	"SELECT EXISTS (SELECT 1 FROM nt WHERE v = 10)",
	"SELECT NOT EXISTS (SELECT 1 FROM nt WHERE v = 999)",
	"SELECT EXISTS (SELECT 1 FROM empty_nt)",
}

// runVDBENoFromSubqueryCorpus runs vdbeNoFromSubqueryCorpus's statements
// through the integrated path (QueryArgs), comparing each against real C
// SQLite, and returns the number of divergences found.
func runVDBENoFromSubqueryCorpus(t *testing.T, p *engine.ReadOnlyPager, cdb *sql.DB, mode string) int {
	t.Helper()

	wrong := 0
	for _, sqlText := range vdbeNoFromSubqueryCorpus {
		aCols, aVals, aErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if aErr != nil || cErr != nil {
			wrong++
			t.Errorf("[mode=%s][%s] unexpected error: engine=%v cgo=%v", mode, sqlText, aErr, cErr)
			continue
		}
		aRows := engineRowsToStrings(aVals)
		if ok, reason := queryResultsMatch(aCols, aRows, cCols, cRows, false); !ok {
			wrong++
			t.Errorf("[mode=%s][%s] DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
				mode, sqlText, reason, aCols, aRows, cCols, cRows)
		}
	}
	return wrong
}

// TestVDBENoFromSubqueryResultParity is the hard gate: every corpus statement
// must produce identical results through the compiled VDBE path and real C
// SQLite.
func TestVDBENoFromSubqueryResultParity(t *testing.T) {
	path := buildVDBENoFromSubqueryDB(t)

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

	wrong := 0
	for _, mode := range engineModes {
		wrong += runVDBENoFromSubqueryCorpus(t, p, cdb, mode)
	}
	t.Logf("VDBE FROM-less-subquery result-parity gate: %d statements, wrong=%d", len(vdbeNoFromSubqueryCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("VDBE FROM-less-subquery parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBENoFromSubqueryCompiles confirms the specific compile-time fix
// directly: a FROM-less top-level "X IN <table-name>" compiles rather than
// hard-erroring (there is no fallback executor -- a statement the compiler
// cannot handle fails outright).
func TestVDBENoFromSubqueryCompiles(t *testing.T) {
	path := buildVDBENoFromSubqueryDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		"SELECT 10 IN nt",
		"SELECT 20 NOT IN nt",
		"SELECT 1 WHERE 10 IN nt",
	} {
		if _, _, err := p.QueryArgs(sqlText, nil); err != nil {
			t.Errorf("[%s] did not compile+run: %v", sqlText, err)
		}
	}
}
