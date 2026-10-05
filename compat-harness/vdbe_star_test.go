package compat

// Tests qualified "table.*"/"alias.*" select-list items, verifying VDBE
// results match C SQLite.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildVDBEStarDB creates test tables with various structures for qualified-star
// testing.
func buildVDBEStarDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/vdbe_star.sqlite"
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
	exec(`INSERT INTO t1 VALUES (3, 20, 'carol')`)

	exec(`CREATE TABLE t2 (id INTEGER PRIMARY KEY, b INTEGER, val TEXT)`)
	exec(`INSERT INTO t2 VALUES (1, 10, 'x')`)
	exec(`INSERT INTO t2 VALUES (2, 20, 'y')`)
	exec(`INSERT INTO t2 VALUES (3, 20, 'z')`)

	exec(`CREATE TABLE plain_t (p INTEGER, q TEXT)`)
	exec(`INSERT INTO plain_t VALUES (5, 'p5')`)
	exec(`INSERT INTO plain_t VALUES (6, 'p6')`)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// vdbeStarCorpus defines test cases for qualified-star queries with various
// join types and clauses.
var vdbeStarCorpus = []joinCase{
	// --- single table, bare qualified star ---
	{"SELECT t1.* FROM t1", false},
	{"SELECT x.* FROM t1 AS x", false},
	{"SELECT pp.* FROM plain_t pp", false},

	// --- qualified star across a comma join ---
	{"SELECT t1.* FROM t1, t2", false},
	{"SELECT t1.*, t2.val FROM t1, t2", false},
	{"SELECT t1.a, t2.* FROM t1, t2", false},
	{"SELECT t1.*, t2.* FROM t1, t2", false},

	// --- qualified star over INNER/LEFT JOIN ---
	{"SELECT t1.* FROM t1 INNER JOIN t2 ON t1.a = t2.b", false},
	{"SELECT t1.*, t2.* FROM t1 INNER JOIN t2 ON t1.a = t2.b", false},
	{"SELECT t1.*, t2.val FROM t1 LEFT JOIN t2 ON t1.a = t2.b AND t1.id = 99", false},

	// --- self-join, two aliases each ".*" ---
	{"SELECT x.*, y.* FROM t1 x, t1 y WHERE x.id < y.id", false},
	{"SELECT x.name, y.* FROM t1 x, t1 y WHERE x.id < y.id", false},

	// --- WHERE / ORDER BY / LIMIT ---
	{"SELECT t1.* FROM t1, t2 WHERE t1.a = t2.b", false},
	{"SELECT t1.* FROM t1, t2 WHERE t1.a = t2.b ORDER BY t1.id, t2.id", true},
	{"SELECT t1.* FROM t1, t2 ORDER BY t1.id, t2.id LIMIT 2", true},
	{"SELECT t1.* FROM t1, t2 ORDER BY t1.id DESC LIMIT 3 OFFSET 1", true},

	// DISTINCT collapses repeated rows.
	{"SELECT DISTINCT t1.* FROM t1, t2", false},

	// GROUP BY with expanded columns.
	{"SELECT t1.* FROM t1, t2 WHERE t1.a = t2.b GROUP BY t1.id, t1.a, t1.name", false},
}

// TestVDBEStarResultParity verifies that qualified-star queries produce
// identical results through the VDBE and C SQLite.
func TestVDBEStarResultParity(t *testing.T) {
	path := buildVDBEStarDB(t)

	// Use the exported file for oracle comparison.
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
	for _, tc := range vdbeStarCorpus {
		total++
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
	t.Logf("VDBE qualified-star result-parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("VDBE qualified-star parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestVDBEStarIntegratedQueryPath runs qualified-star queries through the
// integrated QueryArgs path.
func TestVDBEStarIntegratedQueryPath(t *testing.T) {
	path := buildVDBEStarDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range vdbeStarCorpus {
		if _, _, err := p.QueryArgs(tc.sql, nil); err != nil {
			t.Errorf("[%s] QueryArgs: %v", tc.sql, err)
		}
	}
}

// TestVDBEStarNoSuchTable verifies that unresolvable star qualifiers error
// consistently across both QueryVDBE and integrated paths.
func TestVDBEStarNoSuchTable(t *testing.T) {
	path := buildVDBEStarDB(t)

	// Use the exported file for oracle comparison.
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

	const sqlText = "SELECT nosuch.* FROM t1"

	if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
		t.Fatalf("[%s] expected C SQLite to reject an unresolvable star qualifier, got nil", sqlText)
	}
	if _, _, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
		t.Errorf("[%s] expected QueryVDBE to reject an unresolvable star qualifier, got nil", sqlText)
	}
	if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
		t.Errorf("[%s] expected the integrated QueryArgs path to reject an unresolvable star qualifier, got nil", sqlText)
	}
}

// TestVDBEStarAggregateStillRejected verifies that star mixed with aggregate
// calls is properly rejected.
func TestVDBEStarAggregateStillRejected(t *testing.T) {
	path := buildVDBEStarDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const sqlText = "SELECT t1.*, count(*) FROM t1, t2"
	if _, _, vErr := p.QueryArgs(sqlText, nil); vErr == nil {
		t.Errorf("[%s] expected QueryVDBE to reject star+aggregate, got nil", sqlText)
	}
}
