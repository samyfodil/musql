// Tests that TEMP triggers and views can resolve unqualified references
// to attached tables during ALTER TABLE re-validation.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

func TestAlterAttachedTableReachableFromTempTriggerBody(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
	}{
		{
			name: "TEMP trigger body reaching an attached table: ALTER succeeds",
			steps: []string{
				`ATTACH '' AS aux`,
				`CREATE TABLE t1(a)`,
				`CREATE TABLE aux.log(v)`,
				// tr1's body writes "log" unqualified -- resolves through
				// TEMP (nothing), MAIN (nothing), then aux (log exists there).
				`CREATE TEMP TRIGGER tr1 AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES(new.a); END`,
				`ALTER TABLE t1 RENAME a TO b`,
			},
		},
		// Control: the IDENTICAL unqualified body reference on a NON-temp
		// (main) trigger must still be reported dangling and block the
		// ALTER -- proving the relaxation is scoped to TEMP objects, not a
		// blanket "check every attachment" change.
		{
			name: "control: MAIN trigger body reaching only an attached table still fails",
			steps: []string{
				`ATTACH '' AS aux`,
				`CREATE TABLE t1(a)`,
				`CREATE TABLE aux.log(v)`,
				`CREATE TRIGGER tr1x AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES(new.a); END`,
				`ALTER TABLE t1 RENAME a TO b`,
			},
		},
		// Control: a TEMP VIEW gets the identical unrestricted search
		// (sqlite3CreateView applies the SAME FixInit/bTemp rule, build.c:
		// 3032), so this must ALSO succeed -- proving the fix is not
		// trigger-only.
		{
			name: "TEMP view body reaching an attached table: ALTER succeeds",
			steps: []string{
				`ATTACH '' AS aux`,
				`CREATE TABLE t1(a)`,
				`CREATE TABLE aux.log(v)`,
				`CREATE TEMP VIEW tv1 AS SELECT v FROM log`,
				`ALTER TABLE t1 RENAME a TO b`,
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
