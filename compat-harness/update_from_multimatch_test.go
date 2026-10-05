// This file tests that UPDATE ... FROM with multiple matches per target row
// is properly declined (result depends on query plan, not deterministic).
// Single-match joins must still work exactly.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// ufmRun applies the multi-match UPDATE under one physical configuration and
// reports what the target holds.
func ufmRun(t *testing.T, driver, dsn string, extra []string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
		`CREATE TABLE src(k INT, v INT)`,
		`INSERT INTO tgt VALUES(1,0),(2,0)`,
		`INSERT INTO src VALUES(1,30),(1,10),(1,20),(2,50),(2,40)`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s setup %q: %v", driver, s, eerr)
		}
	}
	for _, s := range extra {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s extra %q: %v", driver, s, eerr)
		}
	}
	if _, eerr := db.Exec(`UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id`); eerr != nil {
		return "ERR"
	}
	var a, b int
	db.QueryRow(`SELECT val FROM tgt WHERE id=1`).Scan(&a)
	db.QueryRow(`SELECT val FROM tgt WHERE id=2`).Scan(&b)
	return fmt.Sprintf("(1,%d)(2,%d)", a, b)
}

// TestUpdateFromMultiMatchStaysDeclined is the two-sided pin.
func TestUpdateFromMultiMatchStaysDeclined(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []string
	}{
		{"plain", nil},
		{"index-src-k", []string{`CREATE INDEX sk ON src(k)`}},
		{"covering-index-analyzed", []string{`CREATE UNIQUE INDEX skv ON src(k,v)`, `ANALYZE`}},
	} {
		dir := t.TempDir()
		if got := ufmRun(t, "sqlite3", filepath.Join(dir, "cgo.db"), c.extra); got == "ERR" {
			t.Errorf("[%s] the oracle now REJECTS a multi-match UPDATE ... FROM -- this pin is obsolete", c.name)
		}
		if got := ufmRun(t, "sqlite", filepath.Join(dir, "musql.db"), c.extra); got != "ERR" {
			t.Errorf("[%s] musql now ANSWERS a multi-match UPDATE ... FROM (%s). "+
				"That is only correct if SQLite's choice has become plan-independent -- "+
				"re-measure it across ANALYZE and index configurations before keeping this.",
				c.name, got)
		}
	}
}

// TestUpdateFromSingleMatchStillExact verifies single-match joins still work exactly.
func TestUpdateFromSingleMatchStillExact(t *testing.T) {
	for _, c := range []struct {
		name  string
		stmts []string
	}{
		{"one-to-one", []string{
			`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
			`CREATE TABLE src(k INT, v INT)`,
			`INSERT INTO tgt VALUES(1,0),(2,0),(3,0)`,
			`INSERT INTO src VALUES(1,10),(2,20),(3,30)`,
			`UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id`,
			`SELECT id, val FROM tgt ORDER BY id`,
		}},
		{"partial-match", []string{
			`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
			`CREATE TABLE src(k INT, v INT)`,
			`INSERT INTO tgt VALUES(1,0),(2,0),(3,0)`,
			`INSERT INTO src VALUES(1,10),(3,30)`,
			`UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id`,
			`SELECT id, val FROM tgt ORDER BY id`,
		}},
		{"no-match", []string{
			`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
			`CREATE TABLE src(k INT, v INT)`,
			`INSERT INTO tgt VALUES(1,7)`,
			`INSERT INTO src VALUES(9,99)`,
			`UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id`,
			`SELECT id, val FROM tgt ORDER BY id`,
		}},
		{"with-index", []string{
			`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
			`CREATE TABLE src(k INT PRIMARY KEY, v INT)`,
			`INSERT INTO tgt VALUES(1,0),(2,0)`,
			`INSERT INTO src VALUES(1,10),(2,20)`,
			`UPDATE tgt SET val = src.v FROM src WHERE src.k = tgt.id`,
			`SELECT id, val FROM tgt ORDER BY id`,
		}},
		{"expression-set", []string{
			`CREATE TABLE tgt(id INTEGER PRIMARY KEY, val INT)`,
			`CREATE TABLE src(k INT, v INT)`,
			`INSERT INTO tgt VALUES(1,5),(2,6)`,
			`INSERT INTO src VALUES(1,10),(2,20)`,
			`UPDATE tgt SET val = tgt.val + src.v FROM src WHERE src.k = tgt.id`,
			`SELECT id, val FROM tgt ORDER BY id`,
		}},
	} {
		cfaDiffer(t, c.name, c.stmts)
	}
}
