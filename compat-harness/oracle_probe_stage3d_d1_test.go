// TestOracleProbeStage3dD1 builds a multi-page table, bulk-deletes enough
// leading rows to empty at least one leaf, and checks on musql and C SQLite:
//
//  1. identical query results;
//  2. each engine's integrity_check reports "ok"; and
//  3. C SQLite, given the export of musql's database, passes integrity_check
//     and reads the same rows.

package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

const (
	probeD1Rows     = 500
	probeD1PageSize = 512
)

// probeD1Build creates a database at path, inserts probeD1Rows rows in one
// transaction, then in a separate session deletes the first half, enough to
// empty at least one leaf on either engine.
func probeD1Build(t *testing.T, driverName, path string) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("%s: open: %v", driverName, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA page_size=%d`, probeD1PageSize)); err != nil {
		t.Fatalf("%s: page_size: %v", driverName, err)
	}
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("%s: create: %v", driverName, err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("%s: begin: %v", driverName, err)
	}
	for i := 1; i <= probeD1Rows; i++ {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?, ?)`, i, "x0123456789"); err != nil {
			t.Fatalf("%s: insert %d: %v", driverName, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s: commit: %v", driverName, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("%s: close session 1: %v", driverName, err)
	}

	db2, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("%s: reopen for delete: %v", driverName, err)
	}
	if _, err := db2.Exec(fmt.Sprintf(`DELETE FROM t WHERE id <= %d`, probeD1Rows/2)); err != nil {
		t.Fatalf("%s: delete: %v", driverName, err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("%s: close session 2: %v", driverName, err)
	}
}

// probeD1SelectAll returns every row as "id|v", ordered by id.
func probeD1SelectAll(t *testing.T, driverName, path string) []string {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("%s: open for select: %v", driverName, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, v FROM t ORDER BY id`)
	if err != nil {
		t.Fatalf("%s: select: %v", driverName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatalf("%s: scan: %v", driverName, err)
		}
		out = append(out, fmt.Sprintf("%d|%s", id, v))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows.Err: %v", driverName, err)
	}
	return out
}

func TestOracleProbeStage3dD1(t *testing.T) {
	dir := t.TempDir()
	musqlPath := filepath.Join(dir, "musql.db")
	cgoPath := filepath.Join(dir, "cgo.db")

	probeD1Build(t, "sqlite", musqlPath)
	probeD1Build(t, "sqlite3", cgoPath)

	// (1) Both files pass their own engine's integrity_check.
	for _, c := range []struct{ driver, path string }{{"sqlite", musqlPath}, {"sqlite3", cgoPath}} {
		db, err := sql.Open(c.driver, c.path)
		if err != nil {
			t.Fatalf("%s: reopen for check: %v", c.driver, err)
		}
		var check string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
			t.Fatalf("%s: integrity_check: %v", c.driver, err)
		}
		db.Close()
		if check != "ok" {
			t.Fatalf("%s: integrity_check=%q, want ok", c.driver, check)
		}
	}

	// (2) Identical query results.
	mrows := probeD1SelectAll(t, "sqlite", musqlPath)
	crows := probeD1SelectAll(t, "sqlite3", cgoPath)
	if len(mrows) != len(crows) {
		t.Fatalf("row count differs: musql=%d cgo=%d", len(mrows), len(crows))
	}
	for i := range mrows {
		if mrows[i] != crows[i] {
			t.Fatalf("row %d differs: musql=%q cgo=%q", i, mrows[i], crows[i])
		}
	}
	if len(mrows) != probeD1Rows-probeD1Rows/2 {
		t.Fatalf("row count=%d, want %d (the deleted half must be gone on both engines)", len(mrows), probeD1Rows-probeD1Rows/2)
	}

	// (3) C SQLite reads the export.
	edb, err := sql.Open("sqlite3", exportedForOracle(t, musqlPath))
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer edb.Close()
	var check string
	if err := edb.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("C over the export: integrity_check=%q (err %v), want ok", check, err)
	}
	erows := probeD1SelectAll(t, "sqlite3", exportedForOracle(t, musqlPath))
	if fmt.Sprint(erows) != fmt.Sprint(crows) {
		t.Fatalf("C over the export reads %d rows, C over its own file %d", len(erows), len(crows))
	}
}
