// Tests PRAGMA integrity_check argument resolution with virtual tables.
// table/...) is tracked in instead of db.tables. C SQLite's own argument
// resolution (pragma.c:1695-1727's PragTyp_INTEGRITY_CHECK case) calls
// sqlite3LocateTable, which is sqlite3FindTable underneath (build.c:337): ONE
// hash per schema holding every Table*, ordinary table, view or virtual table
// alike, with no type discrimination at all -- so a vtab name is exactly as
// good an argument as an ordinary table's. The READ path
// (checkIntegrityCheckTarget, reached only via QueryArgs) already agreed,
// because it scans raw sqlite_schema rows instead of a Go map, and a vtab's
// row there has type='table' like any other (verified directly: a CREATE
// VIRTUAL TABLE's own sqlite_schema row, in both engines). Only the EXEC
// path's map-based check was missing the vtab case, so "PRAGMA
// integrity_check(v)" over a virtual table v answered "no such table: v" from
// Exec while the identical statement, run as a SELECT-shaped query, already
// answered "ok".
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// pragmaIntegrityCheckVtabCase opens one engine.DB and one cgo *sql.DB, runs
// setup on both via their own Exec, then requires the two engines to AGREE
// (both accept, or both reject) on q run through ExecArgs/Exec directly --
// the corpus's own dispatch for a bare PRAGMA, not the package's Query-first
// differ() helper. Mirrors fts5_extcontent_delete_drift_test.go's
// engineSession.agreeExec, inlined here because that type lives behind
// "//go:build sqlite_fts5" and this file must also run in the default build
// (its fts4/rtree cases need no such tag).
func pragmaIntegrityCheckVtabCase(t *testing.T, setup []string, q string) {
	t.Helper()
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()

	for _, s := range setup {
		if _, _, gerr := godb.ExecArgs(s, nil); gerr != nil {
			t.Fatalf("engine setup %q: %v", s, gerr)
		}
		if _, cerr := cgodb.Exec(s); cerr != nil {
			t.Fatalf("cgo setup %q: %v", s, cerr)
		}
	}

	goErr, panicked, panicVal := tclSafeExecArgs(godb, q)
	if panicked {
		t.Fatalf("PANIC executing %q: %v", q, panicVal)
	}
	_, cgoErr := cgodb.Exec(q)
	switch {
	case goErr == nil && cgoErr != nil:
		t.Errorf("%q: engine ACCEPTED, C SQLite rejected: %v", q, cgoErr)
	case goErr != nil && cgoErr == nil:
		t.Errorf("%q: engine rejected (%v), C SQLite ACCEPTED", q, goErr)
	}
}

// TestPragmaIntegrityCheckExecPathVtabFts4 pins the fts4 case, which needs no
// build tag on either side: the oracle's default cgo build always has fts3/4.
func TestPragmaIntegrityCheckExecPathVtabFts4(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`INSERT INTO ft(x) VALUES('hello world')`,
	}
	for _, q := range []string{
		"PRAGMA integrity_check(ft)",
		"PRAGMA quick_check(ft)",
		"PRAGMA integrity_check('ft')",
	} {
		pragmaIntegrityCheckVtabCase(t, setup, q)
	}
}

// TestPragmaIntegrityCheckExecPathVtabRtree pins the rtree case too, the same
// reason: no build tag needed.
func TestPragmaIntegrityCheckExecPathVtabRtree(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE rt USING rtree(id, x0, x1)`,
		`INSERT INTO rt VALUES(1, 0.0, 1.0)`,
	}
	pragmaIntegrityCheckVtabCase(t, setup, "PRAGMA integrity_check(rt)")
}
