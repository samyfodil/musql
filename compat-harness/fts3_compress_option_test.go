// FTS4 "compress=" and "uncompress=" module options: functions are resolved
// lazily at row access time, not at CREATE time. Unregistered functions cause
// DML to fail; 'optimize' is unaffected since it doesn't touch content.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestFts3CompressCreateDiff pins the CREATE-time behavior: parsed, paired,
// and (when "content=" is also given) silently discarded, all matching the
// oracle byte for byte via sqlite_master + PRAGMA table_info.
func TestFts3CompressCreateDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"mined: paired compress/uncompress, no rows ever touched", []string{
			`CREATE VIRTUAL TABLE tt USING fts4(compress=zip, uncompress=unzip)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			`PRAGMA table_info(tt)`,
		}},
		{"fts3 table: compress= is a column, not an option", []string{
			`CREATE VIRTUAL TABLE t3 USING fts3(compress=zip)`,
			`PRAGMA table_info(t3)`,
		}},
		{"quoted function names", []string{
			`CREATE VIRTUAL TABLE tq USING fts4(compress="zip", uncompress='unzip')`,
			`SELECT sql FROM sqlite_master WHERE name='tq'`,
		}},
		{"unpaired compress= alone", []string{
			`CREATE VIRTUAL TABLE tu USING fts4(compress=zip)`,
			`SELECT count(*) FROM sqlite_master`,
		}},
		{"unpaired uncompress= alone", []string{
			`CREATE VIRTUAL TABLE tu2 USING fts4(uncompress=unzip)`,
			`SELECT count(*) FROM sqlite_master`,
		}},
		{"content= discards compress/uncompress, option first", []string{
			`CREATE TABLE src(a,b)`,
			`CREATE VIRTUAL TABLE tc USING fts4(compress=zip, uncompress=unzip, content=src)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
		{"content= discards compress/uncompress, option last", []string{
			`CREATE TABLE src2(a,b)`,
			`CREATE VIRTUAL TABLE tc2 USING fts4(content=src2, compress=zip, uncompress=unzip)`,
			`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		}},
		{"empty compress= alone still requires uncompress=", []string{
			`CREATE VIRTUAL TABLE te1 USING fts4(compress='')`,
			`SELECT count(*) FROM sqlite_master`,
		}},
		{"empty uncompress= alone still requires compress=", []string{
			`CREATE VIRTUAL TABLE te2 USING fts4(uncompress='')`,
			`SELECT count(*) FROM sqlite_master`,
		}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestFts3CompressDML pins that every DML shape reaching content fails
// when the compress/uncompress functions are unregistered. Queries fail on
// fetch, not at prepare time, even on empty tables.
func TestFts3CompressDML(t *testing.T) {
	create := `CREATE VIRTUAL TABLE tt USING fts4(compress=zip, uncompress=unzip)`
	cases := []struct {
		name  string
		stmts []string
	}{
		{"INSERT", []string{create, `INSERT INTO tt VALUES('hello world')`}},
		{"INSERT with docid", []string{create, `INSERT INTO tt(docid,content) VALUES(1,'x')`}},
		{"DELETE, no WHERE, empty table", []string{create, `DELETE FROM tt`}},
		{"DELETE, WHERE docid, empty table", []string{create, `DELETE FROM tt WHERE docid=1`}},
		{"UPDATE, empty table", []string{create, `UPDATE tt SET content='x' WHERE docid=1`}},
		{"SELECT *, empty table", []string{create, `SELECT * FROM tt`}},
		{"SELECT count(*), empty table", []string{create, `SELECT count(*) FROM tt`}},
		{"'rebuild' command", []string{create, `INSERT INTO tt(tt) VALUES('rebuild')`}},
		{"'integrity-check' command", []string{create, `INSERT INTO tt(tt) VALUES('integrity-check')`}},
		{"'optimize' command is unaffected", []string{create, `INSERT INTO tt(tt) VALUES('optimize')`}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestFts3CompressBothEmptyStillDeclines gates empty compress/uncompress values.
// An empty function name is paired (both keys present) but still unregistered,
// causing DML to fail, not silently pass through unchanged.
func TestFts3CompressBothEmptyStillDeclines(t *testing.T) {
	create := `CREATE VIRTUAL TABLE tt3 USING fts4(compress='', uncompress='')`
	cases := []struct {
		name  string
		stmts []string
	}{
		{"CREATE with both empty succeeds on both engines", []string{create}},
		{"INSERT declines, matching the oracle's own function-not-found failure", []string{create, `INSERT INTO tt3 VALUES('hello')`}},
		{"SELECT declines even on an empty table, matching the oracle", []string{create, `SELECT * FROM tt3`}},
	}
	for _, c := range cases {
		differ(t, c.name, c.stmts)
	}
}

// TestFts3CompressDeclineIsClean checks this engine's OWN error directly (not
// through differ(), which discards error text): every DML that would touch
// %_content on a table whose compress=/uncompress= names no function fails
// with the error C SQLite's prepare raises, "no such function", and does
// not panic.
func TestFts3CompressDeclineIsClean(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE tt USING fts4(compress=zip, uncompress=unzip)`,
	}
	cases := []struct {
		name string
		stmt string
	}{
		{"INSERT", `INSERT INTO tt VALUES('hello world')`},
		{"DELETE", `DELETE FROM tt`},
		{"UPDATE", `UPDATE tt SET content='x' WHERE docid=1`},
		{"rebuild", `INSERT INTO tt(tt) VALUES('rebuild')`},
		{"integrity-check", `INSERT INTO tt(tt) VALUES('integrity-check')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "compress.db")
			db, err := sql.Open("sqlite", importedForMusql(t, dsn))
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			defer db.Close()
			for _, s := range setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			_, err = db.Exec(c.stmt)
			if err == nil {
				t.Fatalf("%s: engine accepted DML on a table whose compress=/uncompress= functions do not exist", c.stmt)
			}
			if !strings.Contains(err.Error(), "no such function") {
				t.Fatalf("%s: failed, but with %q rather than C SQLite's \"no such function\"", c.stmt, err.Error())
			}
		})
	}

	t.Run("SELECT", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "compress2.db")
		db, err := sql.Open("sqlite", importedForMusql(t, dsn))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(setup[0]); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if _, err := db.Query(`SELECT * FROM tt`); err == nil {
			t.Fatalf("SELECT * FROM tt: engine read a table whose uncompress= function does not exist")
		}
	})
}

// TestFts3CompressOptimizeUnaffected verifies 'optimize' works on compress= tables.
func TestFts3CompressOptimizeUnaffected(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "compress3.db")
	db, err := sql.Open("sqlite", importedForMusql(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE VIRTUAL TABLE tt USING fts4(compress=zip, uncompress=unzip)`,
		`INSERT INTO tt(tt) VALUES('optimize')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
}
