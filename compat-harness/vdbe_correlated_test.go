package compat

// Tests the VDBE's correlated subquery support, verifying that subqueries
// referencing enclosing columns produce identical results through the VDBE
// and C SQLite.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBECorrelatedDB creates test tables for correlated subquery testing.
func buildVDBECorrelatedDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_correlated.sqlite"
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

	exec(`CREATE TABLE t1 (k INTEGER PRIMARY KEY, v INTEGER, g INTEGER, s TEXT)`)
	exec(`INSERT INTO t1 VALUES (1, 10, 100, 'a')`)
	exec(`INSERT INTO t1 VALUES (2, 20, 100, 'b')`)
	exec(`INSERT INTO t1 VALUES (3, 30, 200, 'c')`)
	exec(`INSERT INTO t1 VALUES (4, NULL, 200, 'd')`) // NULL v -- reaches NOT IN's NULL case; k=4 has no t2 rows
	exec(`INSERT INTO t1 VALUES (5, 150, 300, 'e')`)  // matches a t2.b so a correlated ">" / "IN" hits

	exec(`CREATE TABLE t2 (k INTEGER, b INTEGER, g INTEGER)`)
	exec(`INSERT INTO t2 VALUES (1, 100, 100)`)
	exec(`INSERT INTO t2 VALUES (1, 150, 100)`) // k=1 has two b -> correlated max/count > 1
	exec(`INSERT INTO t2 VALUES (2, 200, 100)`)
	exec(`INSERT INTO t2 VALUES (3, NULL, 200)`) // NULL b -> correlated max(b) is NULL, NOT IN poisoned
	exec(`INSERT INTO t2 VALUES (5, 150, 300)`)
	// k=4 absent -> a correlated subquery keyed on it produces no rows (NULL scalar)

	exec(`CREATE TABLE t3 (k INTEGER, g INTEGER)`)
	exec(`INSERT INTO t3 VALUES (1, 100)`)
	exec(`INSERT INTO t3 VALUES (2, 100)`)
	exec(`INSERT INTO t3 VALUES (3, 200)`)

	exec(`CREATE TABLE empty_outer (k INTEGER, v INTEGER)`)
	exec(`CREATE TABLE empty_inner (k INTEGER, b INTEGER)`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeCorrelatedCorpus defines test cases for correlated subqueries.
var vdbeCorrelatedCorpus = []subCase{
	// Correlated scalar subquery in SELECT list.
	{"SELECT k, (SELECT max(b) FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},
	{"SELECT k, (SELECT count(*) FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},
	{"SELECT k, (SELECT sum(b) FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},
	{"SELECT k, (SELECT avg(b) FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},
	{"SELECT k + (SELECT count(*) FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},
	{"SELECT k, (SELECT b FROM t2 WHERE t2.k = t1.k ORDER BY b DESC LIMIT 1) FROM t1 ORDER BY k", true},
	{"SELECT (SELECT max(b) FROM t2 WHERE t2.k = e.k) FROM empty_outer e", false},

	// Correlated scalar subquery in WHERE.
	{"SELECT k FROM t1 WHERE v > (SELECT avg(b) FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v = (SELECT max(b) FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v >= (SELECT min(b) FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},

	// Correlated EXISTS / NOT EXISTS.
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k AND t2.b > 120) ORDER BY k", true},
	{"SELECT k, EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) FROM t1 ORDER BY k", true},

	// Correlated IN / NOT IN with NULLs.
	{"SELECT k FROM t1 WHERE v IN (SELECT b FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v NOT IN (SELECT b FROM t2 WHERE t2.k = t1.k) ORDER BY k", true},
	{"SELECT k FROM t1 WHERE v IN (SELECT b FROM t2 WHERE t2.g = t1.g) ORDER BY k", true},

	// Multi-column correlation.
	{"SELECT k, (SELECT count(*) FROM t2 WHERE t2.g = t1.g) FROM t1 ORDER BY k", true},
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k AND t2.g = t1.g) ORDER BY k", true},

	// Aliased outer table reference.
	{"SELECT x.k, (SELECT max(b) FROM t2 WHERE t2.k = x.k) FROM t1 AS x ORDER BY x.k", true},
	{"SELECT x.k FROM t1 AS x WHERE EXISTS (SELECT 1 FROM t2 AS y WHERE y.k = x.k) ORDER BY x.k", true},

	// Correlation to rowid pseudo-column.
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.rowid) ORDER BY k", true},

	// --- empty inner side ---
	{"SELECT k, (SELECT max(b) FROM empty_inner WHERE empty_inner.k = t1.k) FROM t1 ORDER BY k", true}, // always NULL
	{"SELECT k FROM t1 WHERE NOT EXISTS (SELECT 1 FROM empty_inner WHERE empty_inner.k = t1.k) ORDER BY k", true},

	// --- composing with the outer query's own machinery (ORDER BY / LIMIT) ---
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k) ORDER BY k DESC LIMIT 2", true},
	{"SELECT s FROM t1 WHERE v = (SELECT max(b) FROM t2 WHERE t2.k = t1.k) ORDER BY s DESC", true},

	// --- nested / grandparent correlation (general outer chain) ---
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k AND EXISTS (SELECT 1 FROM t3 WHERE t3.k = t2.k AND t3.g = t1.g)) ORDER BY k", true},
	{"SELECT k, (SELECT count(*) FROM t2 WHERE t2.k = t1.k AND EXISTS (SELECT 1 FROM t3 WHERE t3.g = t1.g)) FROM t1 ORDER BY k", true},

	// --- an uncorrelated subquery nested INSIDE a correlated one (mixed) ---
	{"SELECT k FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.k AND t2.b > (SELECT avg(b) FROM t2)) ORDER BY k", true},
}

// TestVDBECorrelatedResultParity is the hard gate: every correlated corpus
// statement must produce identical results through the VDBE and real C
// SQLite.
func TestVDBECorrelatedResultParity(t *testing.T) {
	path := buildVDBECorrelatedDB(t)

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
	total := 0
	for _, tc := range vdbeCorrelatedCorpus {
		total++
		sqlText := tc.sql

		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)

		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error (a compilable correlated subquery must not decline)\n  vdbe=%v\n  cgo=%v", sqlText, vErr, cErr)
			continue
		}

		vRows := engineRowsToStrings(vVals)

		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] VDBE DIVERGES from C SQLite: %s\n  vdbe: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
				sqlText, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("VDBE correlated-subquery result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE correlated parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBECorrelatedDualModeIntegration exercises the VDBEMode=dual integration
// path (tryVDBEScan) through the ordinary QueryArgs entry point for the whole
// correlated corpus, confirming every statement the direct QueryVDBE gate above
// proves correct also runs clean on the driver path -- where a compile decline
// would surface as an error, not as a fallback.
func TestVDBECorrelatedDualModeIntegration(t *testing.T) {
	path := buildVDBECorrelatedDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeCorrelatedCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] VDBEMode=dual: %v", tc.sql, err)
		}
	}
}
