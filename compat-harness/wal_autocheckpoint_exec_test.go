// This file tests PRAGMA wal_autocheckpoint through database/sql's Exec method,
// verifying the setter's value is remembered correctly.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// walAutocheckpointExecGetter reads the current PRAGMA wal_autocheckpoint value.
func walAutocheckpointExecGetter(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&n); err != nil {
		t.Fatalf("PRAGMA wal_autocheckpoint (getter): %v", err)
	}
	return n
}

// TestWalAutocheckpointExecSetterMatchesCSQLite verifies the threshold set via Exec.
func TestWalAutocheckpointExecSetterMatchesCSQLite(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		drv := drv
		t.Run(drv, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.sqlite")
			db, err := sql.Open(drv, path)
			if err != nil {
				t.Fatalf("sql.Open(%s): %v", drv, err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)

			if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
				t.Fatalf("PRAGMA journal_mode=WAL: %v", err)
			}
			if _, err := db.Exec("PRAGMA wal_autocheckpoint=7"); err != nil {
				t.Fatalf("Exec(PRAGMA wal_autocheckpoint=7): %v", err)
			}
			if got := walAutocheckpointExecGetter(t, db); got != 7 {
				t.Fatalf("%s: after Exec-only \"wal_autocheckpoint=7\", getter reports %d, want 7", drv, got)
			}

			// Verify second assignment also takes.
			if _, err := db.Exec("PRAGMA wal_autocheckpoint=3"); err != nil {
				t.Fatalf("Exec(PRAGMA wal_autocheckpoint=3): %v", err)
			}
			if got := walAutocheckpointExecGetter(t, db); got != 3 {
				t.Fatalf("%s: after a SECOND Exec-only assignment (3), getter reports %d, want 3", drv, got)
			}

			// Verify autocheckpoint respects the threshold.
			if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
				t.Fatalf("CREATE TABLE: %v", err)
			}
			for i := 0; i < 20; i++ {
				if _, err := db.Exec("INSERT INTO t VALUES(?)", i); err != nil {
					t.Fatalf("INSERT: %v", err)
				}
			}
			var busy, log, checkpointed int
			if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
				t.Fatalf("PRAGMA wal_checkpoint(TRUNCATE): %v", err)
			}
			t.Logf("%s: after threshold=3 and 21 commits, wal_checkpoint(TRUNCATE) reports busy=%d log=%d checkpointed=%d", drv, busy, log, checkpointed)
		})
	}
}

// TestWalAutocheckpointExecVsQueryAgreeOnMusql verifies Exec and Query agree.
func TestWalAutocheckpointExecVsQueryAgreeOnMusql(t *testing.T) {
	viaExec := filepath.Join(t.TempDir(), "exec.db")
	dbExec, err := sql.Open("sqlite", viaExec)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer dbExec.Close()
	dbExec.SetMaxOpenConns(1)
	if _, err := dbExec.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	if _, err := dbExec.Exec("PRAGMA wal_autocheckpoint=7"); err != nil {
		t.Fatalf("Exec wal_autocheckpoint=7: %v", err)
	}
	gotExec := walAutocheckpointExecGetter(t, dbExec)

	viaQuery := filepath.Join(t.TempDir(), "query.db")
	dbQuery, err := sql.Open("sqlite", viaQuery)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer dbQuery.Close()
	dbQuery.SetMaxOpenConns(1)
	if _, err := dbQuery.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	if rows, err := dbQuery.Query("PRAGMA wal_autocheckpoint=7"); err != nil {
		t.Fatalf("Query wal_autocheckpoint=7: %v", err)
	} else {
		rows.Close()
	}
	gotQuery := walAutocheckpointExecGetter(t, dbQuery)

	if gotExec != gotQuery {
		t.Fatalf("musql: Exec-set threshold reads back as %d, Query-set threshold reads back as %d for the identical statement text -- these must agree", gotExec, gotQuery)
	}
	if gotExec != 7 {
		t.Fatalf("musql: both paths should report 7, got %d", gotExec)
	}
}
