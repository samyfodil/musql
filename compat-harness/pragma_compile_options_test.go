// Package compat gates pragma_compile_options table tests.
// The table rows depend on how the oracle was compiled (host machine and C
// compiler), so it must be declined to avoid wrong answers.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestPragmaCompileOptionsMaxMmapSizeMatches verifies the oracle returns
// MAX_MMAP_SIZE=0x7fff0000 for the mined query.
func TestPragmaCompileOptionsMaxMmapSizeMatches(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT compile_options AS x FROM pragma_compile_options WHERE x LIKE 'max_mmap_size=%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if len(got) != 1 || got[0] != "MAX_MMAP_SIZE=0x7fff0000" {
		t.Fatalf("oracle's own answer moved -- re-derive this file's reasoning: got %v, want exactly [MAX_MMAP_SIZE=0x7fff0000]", got)
	}
}

// TestPragmaCompileOptionsStaysDeclined checks that pragma_compile_options
// queries decline at prepare time, never silently return wrong answers.
func TestPragmaCompileOptionsStaysDeclined(t *testing.T) {
	for _, q := range []string{
		`SELECT compile_options AS x FROM pragma_compile_options WHERE x LIKE 'max_mmap_size=%'`,
		`SELECT * FROM pragma_compile_options`,
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", q, res[0])
		}
	}
}

// TestPragmaCompileOptionsIsBookedOutOfScope verifies that pragma_compile_options
// statements are classified as out-of-scope, not merely unsupported.
func TestPragmaCompileOptionsIsBookedOutOfScope(t *testing.T) {
	for _, q := range []string{
		`SELECT compile_options AS x FROM pragma_compile_options WHERE x LIKE 'max_mmap_size=%'`,
		`SELECT * FROM pragma_compile_options`,
		`select count(*) from PRAGMA_COMPILE_OPTIONS`,
	} {
		if !tclCompileOptionsBuildIdentity(q) {
			t.Errorf("tclCompileOptionsBuildIdentity(%q) = false, want true", q)
		}
	}
	for _, q := range []string{
		`SELECT * FROM pragma_compile_options_extra`,
		`SELECT * FROM xpragma_compile_options`,
		`SELECT sqlite_compileoption_used('THREADSAFE')`,
		`SELECT sqlite_compileoption_get(0)`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0))`,
	} {
		if tclCompileOptionsBuildIdentity(q) {
			t.Errorf("tclCompileOptionsBuildIdentity(%q) = true, want false", q)
		}
	}

	path := filepath.Join(t.TempDir(), "oos.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ExecArgs("CREATE TABLE t(a)", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const getStmt = `SELECT sqlite_compileoption_get(0)`
	_, _, qerr := p.Query(getStmt)
	if qerr == nil {
		t.Fatalf("%s: expected a decline, got success -- see compile_options.go", getStmt)
	}
	if !strings.Contains(qerr.Error(), "no two independently-built implementations can agree on it") {
		t.Errorf("%s: decline no longer carries the out-of-scope marker tclIsOutOfScope keys on:\n  %v", getStmt, qerr)
	}
	if !tclIsOutOfScope(getStmt, qerr) {
		t.Errorf("tclIsOutOfScope(%q, %v) = false, want true", getStmt, qerr)
	}
	const fts5Stmt = `SELECT sqlite_compileoption_used('ENABLE_FTS5')`
	_, _, fts5Err := p.Query(fts5Stmt)
	if fts5Err == nil {
		t.Fatalf("%s: expected a decline -- see compile_options.go", fts5Stmt)
	}
	if tclIsOutOfScope(fts5Stmt, fts5Err) {
		t.Errorf("tclIsOutOfScope(%q, %v) = true, want false", fts5Stmt, fts5Err)
	}
}
