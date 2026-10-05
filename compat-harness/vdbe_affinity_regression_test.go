package compat

// This file tests VDBE comparison affinity with columns and literals of different types.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEAffinityDB creates test schemas with different column affinities.
func buildVDBEAffinityDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_affinity.sqlite"
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

	exec(`CREATE TABLE t1(x, y)`)
	exec(`INSERT INTO t1 VALUES(1,99)`)
	exec(`CREATE TABLE t2(a, b TEXT)`)
	exec(`INSERT INTO t2 VALUES(2,99)`)

	exec(`CREATE TABLE n1(y)`)
	exec(`INSERT INTO n1 VALUES(99),(100),(5)`)
	exec(`CREATE TABLE x2(b TEXT)`)
	exec(`INSERT INTO x2 VALUES('99'),('100'),('5')`)

	exec(`CREATE TABLE t0(c0 REAL UNIQUE)`)
	exec(`INSERT INTO t0(c0) VALUES (3175546974276630385)`)

	exec(`CREATE TABLE tx(b TEXT)`)
	exec(`INSERT INTO tx VALUES('5')`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeAffinityCase is a test case with row-order significance flag.
type vdbeAffinityCase struct {
	sql     string
	ordered bool
}

var vdbeAffinityCorpus = []vdbeAffinityCase{
	{"SELECT x, a, y=b FROM t1, t2 ORDER BY +x, +a", true},
	{"SELECT x, a, y=b FROM t1, t2 WHERE y=b", false},
	{"SELECT x, a, y=b FROM t1, t2 WHERE b=y", false},
	{"SELECT x, a, y=b FROM t1, t2 WHERE +y=+b", false},
	{"SELECT x, a, b=y FROM t1, t2 WHERE b=y", false},

	// The same NONE-vs-TEXT column comparison across the other operators,
	// over multiple value pairs, comma-joined -- every result cell must match
	// C SQLite's own storage-class comparison (no affinity coercion). ---
	{"SELECT n1.y, x2.b, n1.y=x2.b, n1.y<x2.b, n1.y>x2.b, n1.y<>x2.b FROM n1, x2 ORDER BY +n1.y, +x2.b", true},
	{"SELECT n1.y FROM n1, x2 WHERE n1.y=x2.b", false},
	{"SELECT count(*) FROM n1, x2 WHERE n1.y=x2.b", false},

	// --- affinity2.test: integer LITERAL vs REAL COLUMN. Generic numeric
	// affinity keeps the literal an integer, so the big literal compares
	// STRICTLY LESS THAN the rounded-up float64 in c0 (TRUE / one row). ---
	{"SELECT 3175546974276630385 < c0 FROM t0", false},
	{"SELECT 1 FROM t0 WHERE 3175546974276630385 < c0", false},
	{"SELECT c0 > 3175546974276630385 FROM t0", false},
	{"SELECT 3175546974276630385 = c0 FROM t0", false},
	{"SELECT 3175546974276630385 <= c0, 3175546974276630385 >= c0 FROM t0", false},
	{"SELECT b=5, b=5.0, 5=b, b<'6', b>'4' FROM tx", false},
	{"SELECT b FROM tx WHERE b=5", false},
}

// TestVDBEComparisonAffinityRegression verifies that VDBE comparison affinity matches C SQLite.
func TestVDBEComparisonAffinityRegression(t *testing.T) {
	path := buildVDBEAffinityDB(t)

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
	for _, tc := range vdbeAffinityCorpus {
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
	t.Logf("VDBE comparison-affinity regression gate: %d statements, wrong=%d", len(vdbeAffinityCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("VDBE comparison-affinity regression gate FAILED: wrong=%d (must be 0)", wrong)
	}
}
