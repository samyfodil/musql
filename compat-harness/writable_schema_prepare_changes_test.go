// This file tests that failed sqlite_master writes don't update change counters.
// Failed PREPARE-time checks should not affect changes()/total_changes().
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

func TestWritableSchemaPrepareFailureKeepsChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string // run on both, must succeed on both
		stmt  string   // run on both, must FAIL on both
	}{
		// sqlite3IsReadOnly, with the flag off -- all three verbs.
		{"read-only refusal, UPDATE", nil, `UPDATE sqlite_master SET sql='x'`},
		{"read-only refusal, DELETE", nil, `DELETE FROM sqlite_master`},
		{"read-only refusal, INSERT", nil,
			`INSERT INTO sqlite_master VALUES('table','z','z',7,'CREATE TABLE z(a)')`},
		// ...and with a WHERE/SET/IDLIST that is itself invalid, to prove the
		// refusal is asked FIRST (it is the error both engines report).
		{"read-only refusal beats a bad SET target", nil, `UPDATE sqlite_master SET nosuchcol='x'`},
		{"read-only refusal beats a bad arity", nil, `INSERT INTO sqlite_master VALUES(1,2,3,4)`},
		// The other two prepare-time checks, with the flag ON.
		{"VALUES arity", []string{`PRAGMA writable_schema=ON`},
			`INSERT INTO sqlite_master VALUES(1,2,3,4)`},
		{"SET target that is not a catalog column", []string{`PRAGMA writable_schema=ON`},
			`UPDATE sqlite_master SET nosuchcol='x'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			godb, err := engine.Create(filepath.Join(dir, "pure.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
			if err != nil {
				t.Fatal(err)
			}
			cgodb.SetMaxOpenConns(1) // per-connection flag; per-connection counters
			defer cgodb.Close()

			both := func(sql string) (goErr, cgoErr error) {
				_, _, goErr = wsGoRun(godb, sql)
				_, _, cgoErr = wsCGORun(cgodb, sql)
				return
			}
			for _, s := range append([]string{
				`CREATE TABLE t(a)`,
				`INSERT INTO t VALUES(1),(2),(3)`,
			}, tc.setup...) {
				if ge, ce := both(s); ge != nil || ce != nil {
					t.Fatalf("setup %q: pure=%v real=%v", s, ge, ce)
				}
			}
			// Both must REFUSE. A pass here that came from one engine quietly
			// succeeding would make the counter comparison meaningless.
			ge, ce := both(tc.stmt)
			if ge == nil {
				t.Fatalf("%q: this engine accepted it", tc.stmt)
			}
			if ce == nil {
				t.Fatalf("%q: C SQLite accepted it where this engine refused (%v)", tc.stmt, ge)
			}
			goCols, goRows, goErr := wsGoRun(godb, `SELECT changes(), total_changes()`)
			if goErr != nil {
				t.Fatalf("pure changes(): %v", goErr)
			}
			cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, `SELECT changes(), total_changes()`)
			if cgoErr != nil {
				t.Fatalf("real changes(): %v", cgoErr)
			}
			if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
				t.Fatalf("after the refused %q, changes()/total_changes() diverge:\n"+
					"  pure: %v\n  real: %v\n"+
					"C SQLite raises this while GENERATING code, so the statement never reaches\n"+
					"RUN state and both counters keep the previous statement's values.",
					tc.stmt, goRows, cgoRows)
			}
		})
	}
}
