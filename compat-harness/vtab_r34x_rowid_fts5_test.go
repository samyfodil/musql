//go:build sqlite_fts5

// FTS5 tests for rowid handling in virtual table operations.
// See vtab_r34x_rowid_test.go for rtree equivalents.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r34xFts5Lockstep runs stmts against both engines statement by statement,
// requiring the same accept/reject at each, then compares the table.
func r34xFts5Lockstep(t *testing.T, name string, stmts []string, verify ...string) {
	t.Helper()
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	for i, s := range stmts {
		eerr := fts5Exec(t, "sqlite", dsn["sqlite"], []string{s})
		cerr := fts5Exec(t, "sqlite3", dsn["sqlite3"], []string{s})
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] stmt #%d accept/reject disagrees: %s\n  musql: %v\n  cgo:    %v", name, i, s, eerr, cerr)
			return
		}
	}
	for _, q := range verify {
		got, gerr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		want, werr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		if (gerr == nil) != (werr == nil) {
			t.Errorf("[%s] %s accept/reject disagrees\n  musql: %v\n  cgo:    %v", name, q, gerr, werr)
			continue
		}
		if got != want {
			t.Errorf("[%s] %s DIVERGES\n  cgo:    %s\n  musql: %s", name, q, want, got)
		}
	}
}

// TestR34XFts5NamedRowid puts every storage class through the rowid slot of an
// ordinary (content-carrying) fts5 table, in all three spellings.
func TestR34XFts5NamedRowid(t *testing.T) {
	for _, v := range r34xRowidValues {
		for _, spelling := range []string{"rowid", "oid", "_rowid_"} {
			r34xFts5Lockstep(t, "r34x fts5 "+spelling+" "+v.name, []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t(` + spelling + `, a) VALUES(` + v.val + `, 'alpha')`,
			}, `SELECT rowid, a FROM t ORDER BY rowid`)
		}
	}
	// Test auto-assignment after coercion.
	r34xFts5Lockstep(t, "r34x fts5 coerced then auto", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(rowid, a) VALUES('7', 'alpha')`,
		`INSERT INTO t(a) VALUES('bravo')`,
	}, `SELECT rowid, a FROM t ORDER BY rowid`)
	// A multi-row VALUES list whose second row is the bad one.
	r34xFts5Lockstep(t, "r34x fts5 multi row second bad", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(rowid, a) VALUES(1, 'alpha'),('x', 'bravo')`,
	}, `SELECT rowid, a FROM t ORDER BY rowid`)
	// Test with SELECT source.
	r34xFts5Lockstep(t, "r34x fts5 select source bad rowid", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`CREATE TABLE s(r, v)`,
		`INSERT INTO s VALUES('x', 'alpha')`,
		`INSERT INTO t(rowid, a) SELECT r, v FROM s`,
	}, `SELECT rowid, a FROM t ORDER BY rowid`)
}

// TestR34XFts5CommandChannelRowid tests FTS5 commands with rowid constraints.
func TestR34XFts5CommandChannelRowid(t *testing.T) {
	seed := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(rowid, a) VALUES(3, 'zulu')`,
	}
	for _, v := range r34xRowidValues {
		r34xFts5Lockstep(t, "r34x fts5 delete command "+v.name,
			append(append([]string{}, seed...),
				`INSERT INTO t(t, rowid, a) VALUES('delete', `+v.val+`, 'zulu')`),
			`SELECT rowid, a FROM t ORDER BY rowid`)
	}
	// Test command without rowid argument.
	r34xFts5Lockstep(t, "r34x fts5 optimize command",
		append(append([]string{}, seed...), `INSERT INTO t(t) VALUES('optimize')`),
		`SELECT rowid, a FROM t ORDER BY rowid`)
}
