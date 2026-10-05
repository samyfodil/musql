// This file tests that dangling trigger/view references are suppressed during
// ALTER TABLE when PRAGMA writable_schema=ON.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

func TestAlterWritableSchemaSuppressesRevalidation(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
	}{
		{
			name: "dangling trigger body: RENAME COLUMN succeeds under writable_schema=ON",
			steps: []string{
				`CREATE TABLE t1(a,b,c,e)`,
				// r3's body references t3, which never exists -- this is the
				// EXACT altercol.test 23.10 shape.
				`CREATE TRIGGER r3 AFTER INSERT ON t1 BEGIN INSERT INTO t3(x,y) VALUES(new.a, new.b); END`,
				`PRAGMA writable_schema=ON`,
				`ALTER TABLE t1 RENAME COLUMN e TO eeee`,
				`PRAGMA writable_schema=OFF`,
				// The table's own CREATE TABLE text really was rewritten --
				// this is not a silent no-op, only the re-validation check
				// that was skipped.
				`SELECT sql FROM sqlite_master WHERE name='t1'`,
			},
		},
		{
			name: "dangling VIEW: RENAME TO succeeds under writable_schema=ON",
			steps: []string{
				`CREATE TABLE t1(a,b)`,
				`CREATE VIEW v1 AS SELECT * FROM ff`, // ff never exists
				`PRAGMA writable_schema=ON`,
				`ALTER TABLE t1 RENAME TO t9`,
				`PRAGMA writable_schema=OFF`,
				`SELECT sql FROM sqlite_master WHERE name='t9'`,
			},
		},
		{
			// r33rTriggersResolveAfter is the OTHER half (the "after rename"/
			// "after drop column" pass): an INSTEAD OF trigger's OLD/NEW bind
			// to the VIEW's freshly recomputed columns (sqlite3ViewGetColumnNames,
			// alter.c:1358), so renaming a column the view selects by name
			// (no alias, no explicit column list) ordinarily fails the WHOLE
			// ALTER even though the trigger's body never mentions t1 at all.
			name: "post-cascade half: INSTEAD OF trigger's stale column succeeds under writable_schema=ON",
			steps: []string{
				`CREATE TABLE t1(a,b)`,
				`CREATE VIEW v1 AS SELECT a,b FROM t1`,
				`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a,new.b); END`,
				`PRAGMA writable_schema=ON`,
				`ALTER TABLE t1 RENAME a TO aaa`,
				`PRAGMA writable_schema=OFF`,
				`SELECT sql FROM sqlite_master WHERE name='t1'`,
			},
		},
		// Control: the identical INSTEAD OF shape WITHOUT writable_schema=ON
		// must still correctly fail the post-cascade check.
		{
			name: "control: same INSTEAD OF trigger WITHOUT writable_schema still fails",
			steps: []string{
				`CREATE TABLE t1(a,b)`,
				`CREATE VIEW v1 AS SELECT a,b FROM t1`,
				`CREATE TRIGGER tr INSTEAD OF INSERT ON v1 BEGIN INSERT INTO t1 VALUES(new.a,new.b); END`,
				`ALTER TABLE t1 RENAME a TO aaa`,
			},
		},
		// Control: the identical dangling-trigger shape WITHOUT
		// writable_schema=ON must still correctly block the ALTER, on both
		// engines, with the exact same error text -- proving the suppression
		// is scoped to the flag and does not just always accept.
		{
			name: "control: same dangling trigger WITHOUT writable_schema still fails",
			steps: []string{
				`CREATE TABLE t1(a,b,c,e)`,
				`CREATE TRIGGER r3 AFTER INSERT ON t1 BEGIN INSERT INTO t3(x,y) VALUES(new.a, new.b); END`,
				`ALTER TABLE t1 RENAME COLUMN e TO eeee`,
			},
		},
	}

	for _, tc := range cases {
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
			cgodb.SetMaxOpenConns(1)
			defer cgodb.Close()

			for i, step := range tc.steps {
				goCols, goRows, goErr := wsGoRun(godb, step)
				cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, step)
				if (goErr == nil) != (cgoErr == nil) {
					t.Fatalf("step %d %q accept/reject disagrees\n  engine=%v\n  cgo=%v", i, step, goErr, cgoErr)
				}
				if goErr != nil {
					if got := strings.TrimPrefix(goErr.Error(), "engine: "); got != cgoErr.Error() {
						t.Fatalf("step %d %q error text mismatch\n  engine: %q\n  cgo:    %q", i, step, got, cgoErr.Error())
					}
					continue
				}
				if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
					t.Fatalf("step %d %q result mismatch\n  engine: %v %v\n  cgo:    %v %v", i, step, goCols, goRows, cgoCols, cgoRows)
				}
			}
		})
	}
}
