package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
)

// TestSchemaRowidMatchesCSQLite: a sqlite_schema row's rowid is C's --
// OP_NewRowid on the schema b-tree, max(rowid)+1, so a DROP of the newest
// object gives its number back and a DROP of an older one leaves a gap -- and a
// VACUUM renumbers the catalog in the order vacuum.c rebuilds it. Our format
// used to decline every reference to the column (alter.test#3's
// "SELECT max(oid) FROM sqlite_master").
func TestSchemaRowidMatchesCSQLite(t *testing.T) {
	q := "SELECT rowid, type, name FROM sqlite_master ORDER BY rowid"
	for i, st := range [][]string{
		{"CREATE TABLE a(x)", "CREATE TABLE b(x UNIQUE, y UNIQUE)", "CREATE INDEX bi ON b(y)", "CREATE VIEW v AS SELECT 1", "CREATE TRIGGER tr AFTER INSERT ON a BEGIN SELECT 1; END", q},
		{"CREATE TABLE a(x)", "CREATE TABLE b(x)", "CREATE TABLE c(x)", "DROP TABLE b", "CREATE TABLE d(x)", q, "SELECT max(oid) FROM sqlite_master"},
		{"CREATE TABLE a(x)", "CREATE TABLE b(x)", "DROP TABLE b", "CREATE TABLE d(x)", q},
		{"CREATE TABLE a(x INTEGER PRIMARY KEY AUTOINCREMENT, y UNIQUE)", "CREATE TABLE b(x INTEGER PRIMARY KEY AUTOINCREMENT)", q},
		{"CREATE TABLE w(a PRIMARY KEY, b UNIQUE) WITHOUT ROWID", "CREATE TABLE z(q)", q},
		{"CREATE VIRTUAL TABLE f USING fts4(x)", "CREATE TABLE after(x)", q},
		{"CREATE TABLE a(x)", "CREATE TABLE b(x UNIQUE)", "ALTER TABLE a RENAME TO a2", "ALTER TABLE b ADD COLUMN c", q},
		{"CREATE TABLE a(x)", "CREATE TABLE b(x UNIQUE)", "CREATE VIEW v AS SELECT 1", "CREATE INDEX ai ON a(x)", "CREATE TABLE c(x)", "DROP TABLE a", "VACUUM", q},
		{"CREATE TABLE a(x INTEGER PRIMARY KEY AUTOINCREMENT)", "CREATE VIEW v AS SELECT 1", "CREATE TABLE b(x UNIQUE)", "CREATE INDEX bi ON b(x)", "CREATE TRIGGER tr AFTER INSERT ON a BEGIN SELECT 1; END", "VACUUM", q},
		{"CREATE VIRTUAL TABLE f USING fts4(x)", "CREATE TABLE t(x)", "CREATE VIEW v AS SELECT 1", "VACUUM", q},
		{"CREATE TABLE a(x)", "PRAGMA writable_schema=ON", "INSERT INTO sqlite_master VALUES('table','zz','zz',0,'CREATE TABLE zz(q)')", "PRAGMA writable_schema=OFF", q},
		{"CREATE TABLE a(x)", "CREATE TABLE a(y)", "CREATE TABLE b(x)", q},
		{"CREATE TABLE a(x)", "CREATE TABLE c AS SELECT * FROM a", "CREATE TEMP TABLE tt(x)", "CREATE TABLE d(x)", q, "SELECT rowid, name FROM sqlite_temp_master"},
		{"CREATE TABLE a(x)", "CREATE TABLE b(x)", "DROP TABLE a", "SELECT rowid, name FROM sqlite_master WHERE rowid=2", "PRAGMA writable_schema=ON", "UPDATE sqlite_master SET sql='CREATE TABLE b(x, y)' WHERE rowid=2", "PRAGMA writable_schema=OFF", "SELECT sql FROM sqlite_master"},
		{"CREATE TABLE a(x INTEGER PRIMARY KEY AUTOINCREMENT)", "DROP TABLE a", "CREATE TABLE b(x)", "VACUUM", q},
	} {
		differ(t, fmt.Sprint(i), st)
	}
}

// TestSchemaRowidSurvivesTheFile: the rowids are the file's, not the session's
// -- reopening keeps a DROP's gap, a CREATE after the reopen counts past the
// highest one, and a C database imported with a gap keeps it.
func TestSchemaRowidSurvivesTheFile(t *testing.T) {
	rowids := func(db *sql.DB) string {
		rows, err := db.Query("SELECT rowid, name FROM sqlite_master ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var r int64
			var n string
			if err := rows.Scan(&r, &n); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d:%s", r, n))
		}
		return strings.Join(out, ",")
	}
	build := []string{"CREATE TABLE a(x)", "CREATE TABLE b(x UNIQUE)", "CREATE TABLE c(x)", "DROP TABLE b"}
	after := "CREATE TABLE d(x)"
	for _, drv := range []string{"sqlite3", "sqlite"} {
		path := filepath.Join(t.TempDir(), "r.db")
		db, err := sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range build {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s %s: %v", drv, s, err)
			}
		}
		db.Close()
		db, err = sql.Open(drv, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(after); err != nil {
			t.Fatal(err)
		}
		got := rowids(db)
		db.Close()
		if want := "1:a,4:c,5:d"; got != want {
			t.Errorf("%s: after a reopen, sqlite_master rowids = %s, want %s", drv, got, want)
		}
	}

	src := filepath.Join(t.TempDir(), "c.db")
	cdb, err := sql.Open("sqlite3", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range build {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	cdb.Close()
	dst := filepath.Join(t.TempDir(), "imported.db")
	if err := sqliteconv.Import(src, dst, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}
	mdb, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	if got, want := rowids(mdb), "1:a,4:c"; got != want {
		t.Errorf("imported: sqlite_master rowids = %s, want %s", got, want)
	}
}
