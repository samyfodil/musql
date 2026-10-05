package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestAttachOnSegmentFormat tests ATTACH with cross-database joins and TEMP tables.
func TestAttachOnSegmentFormat(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "m.musq")
	other := filepath.Join(dir, "o.musq")
	db, err := sql.Open(DriverName, main)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO a VALUES(1,'x')`,
		`ATTACH DATABASE '` + other + `' AS o`,
		`CREATE TABLE o.b(id INTEGER PRIMARY KEY, w TEXT)`,
		`INSERT INTO o.b VALUES(2,'y')`,
		`CREATE TEMP TABLE tt(id INTEGER PRIMARY KEY, z TEXT)`,
		`INSERT INTO tt VALUES(3,'z')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, f := range []string{main, other} {
		b := make([]byte, 16)
		fh, _ := os.Open(f)
		fh.Read(b)
		fh.Close()
		if got := string(b[:4]); got != "MQSF" {
			t.Fatalf("%s starts %q, want a segment file (MQSF)", filepath.Base(f), string(b[:13]))
		}
	}
	var got string
	if err := db.QueryRow(`SELECT a.v || '/' || b.w FROM a, o.b`).Scan(&got); err != nil {
		t.Fatalf("cross-database join: %v", err)
	}
	if got != "x/y" {
		t.Fatalf("got %q", got)
	}
	var three string
	if err := db.QueryRow(`SELECT a.v || '/' || b.w || '/' || tt.z FROM a, o.b, tt`).Scan(&three); err != nil {
		t.Fatalf("main+attached+temp join: %v", err)
	}
	if three != "x/y/z" {
		t.Fatalf("main+attached+temp join = %q", three)
	}
	if err := db.QueryRow(`SELECT z FROM temp.tt`).Scan(&three); err != nil {
		t.Fatalf("qualified temp read with an attachment open: %v", err)
	}
}
