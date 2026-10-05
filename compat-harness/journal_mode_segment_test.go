package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
)

// TestJournalModesOnOurFormat verifies that WAL and OFF journal modes work
// correctly on the native format.
func TestJournalModesOnOurFormat(t *testing.T) {
	for i, st := range [][]string{
		{"PRAGMA journal_mode=wal", "PRAGMA journal_mode", "CREATE TABLE t(a)", "INSERT INTO t VALUES(1)", "SELECT * FROM t", "PRAGMA journal_mode"},
		{"PRAGMA journal_mode=wal", "PRAGMA wal_checkpoint", "PRAGMA wal_checkpoint(TRUNCATE)"},
		{"CREATE TABLE t(a)", "PRAGMA journal_mode=wal", "PRAGMA wal_checkpoint", "INSERT INTO t VALUES(1)", "PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA wal_checkpoint", "SELECT * FROM t"},
		{"PRAGMA journal_mode=wal", "CREATE TABLE t(a)", "PRAGMA journal_mode=delete", "PRAGMA journal_mode", "PRAGMA wal_checkpoint"},
		{"PRAGMA wal_checkpoint", "PRAGMA wal_checkpoint(FULL)"},
		{"PRAGMA journal_mode=wal", "BEGIN", "PRAGMA journal_mode=delete", "COMMIT", "PRAGMA journal_mode"},
		{"CREATE TABLE t(a UNIQUE)", "PRAGMA journal_mode=off", "BEGIN", "INSERT INTO t VALUES(1)", "SAVEPOINT s", "INSERT INTO t VALUES(2)", "ROLLBACK TO s", "COMMIT", "SELECT * FROM t"},
		{"CREATE TABLE t(a UNIQUE)", "INSERT INTO t VALUES(5)", "PRAGMA journal_mode=off", "BEGIN", "INSERT INTO t VALUES(1),(2),(5),(3)", "SELECT * FROM t", "ROLLBACK", "SELECT * FROM t"},
		{"CREATE TABLE t(a)", "PRAGMA journal_mode=off", "INSERT INTO t VALUES(1)", "BEGIN", "INSERT INTO t VALUES(2)", "ROLLBACK", "SELECT * FROM t", "PRAGMA journal_mode"},
		{"PRAGMA journal_mode=memory", "PRAGMA journal_mode=persist", "PRAGMA journal_mode=truncate", "PRAGMA journal_mode"},
		{"CREATE TABLE t(a UNIQUE)", "PRAGMA journal_mode=off", "BEGIN", "INSERT INTO t VALUES(1)", "UPDATE t SET a=a+1", "INSERT INTO t VALUES(2)", "SELECT * FROM t"},
	} {
		differ(t, fmt.Sprint(i), st)
	}
}

// TestWALModeIsTheFiles: in C "wal" is a property of the file -- the next
// connection reads it back -- so it has to survive a reopen here, an export
// has to hand C a WAL database, and an import of one has to stay WAL.
func TestWALModeIsTheFiles(t *testing.T) {
	mode := func(drv, path string) string {
		db, err := sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var m string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&m); err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
		return m
	}
	path := filepath.Join(t.TempDir(), "w.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"PRAGMA journal_mode=wal", "CREATE TABLE t(a)", "INSERT INTO t VALUES(1)"} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	db.Close()
	if got := mode("sqlite", path); got != "wal" {
		t.Errorf("after a reopen: journal_mode = %s, want wal", got)
	}
	out := filepath.Join(t.TempDir(), "exported.db")
	if err := sqliteconv.Export(path, out, 0); err != nil {
		t.Fatal(err)
	}
	if got := mode("sqlite3", out); got != "wal" {
		t.Errorf("C reading the export: journal_mode = %s, want wal", got)
	}

	src := filepath.Join(t.TempDir(), "c.db")
	cdb, err := sql.Open("sqlite3", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"PRAGMA journal_mode=wal", "CREATE TABLE t(a)"} {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	cdb.Close()
	imported := filepath.Join(t.TempDir(), "imported.db")
	if err := sqliteconv.Import(src, imported, sqliteconv.ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := mode("sqlite", imported); got != "wal" {
		t.Errorf("imported from a C WAL database: journal_mode = %s, want wal", got)
	}
	// ...and leaving it is recorded the same way.
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=delete"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := mode("sqlite", path); got != "delete" {
		t.Errorf("after leaving wal and reopening: journal_mode = %s, want delete", got)
	}
}
