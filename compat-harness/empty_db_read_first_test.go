package compat

// Tests the first statement on a connection reading from an empty database,
// in all forms: absent file, zero-length file, and memory DSNs. All must
// complete without deadlock or timeout.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// readFirstBudget bounds how long ONE first-statement read may take. It exists
// to name the failure mode: a self-deadlock against the reader's own lock shows
// up as exactly engine.BusyTimeout (5s) of waiting, so anything near that is
// this bug and not a slow machine.
const readFirstBudget = 3 * time.Second

// firstStatementRead opens a brand-new connection to dsn under driver and runs
// stmt as its very first statement, returning the scanned value. A run that
// outlives readFirstBudget fails the test immediately rather than being reported
// as a value mismatch, so the log says "deadlock" instead of "wrong answer".
func firstStatementRead(t *testing.T, driver, dsn, stmt string) (any, error) {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s: sql.Open(%q): %v", driver, dsn, err)
	}
	defer db.Close()

	type outcome struct {
		v   any
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var v any
		done <- outcome{v, db.QueryRow(stmt).Scan(&v)}
	}()
	select {
	case o := <-done:
		return o.v, o.err
	case <-time.After(readFirstBudget):
		t.Fatalf("%s: %q as the FIRST statement on %q blocked longer than %s -- a lock self-deadlock, not a slow test",
			driver, stmt, dsn, readFirstBudget)
		return nil, nil
	}
}

func TestEmptyDatabaseReadFirstMatchesCSQLite(t *testing.T) {
	// Every statement gets its own fresh pair of files/DSNs: the whole point is
	// that nothing has written to the database yet.
	for _, stmt := range []string{
		"SELECT 1",
		"SELECT 'x' || 'y'",
		"PRAGMA user_version",
		"SELECT count(*) FROM sqlite_master",
	} {
		for _, shape := range []string{"absent", "zero-length", ":memory:", "mode=memory", "mode=memory&cache=shared"} {
			t.Run(fmt.Sprintf("%s/%s", shape, stmt), func(t *testing.T) {
				dsn := func(tag string) string {
					switch shape {
					case "absent":
						return filepath.Join(t.TempDir(), tag+".db")
					case "zero-length":
						p := filepath.Join(t.TempDir(), tag+".db")
						if err := os.WriteFile(p, nil, 0o644); err != nil {
							t.Fatalf("creating the 0-byte database: %v", err)
						}
						return p
					case ":memory:":
						return ":memory:"
					case "mode=memory":
						return "file:" + tag + "?mode=memory"
					default:
						return "file:" + tag + "?mode=memory&cache=shared"
					}
				}
				wantV, wantErr := firstStatementRead(t, "sqlite3", dsn("cgo"), stmt)
				gotV, gotErr := firstStatementRead(t, "sqlite", dsn("go"), stmt)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("%q on a %s database: cgo err=%v, engine err=%v", stmt, shape, wantErr, gotErr)
				}
				if wantErr != nil {
					return // both rejected it; nothing to compare
				}
				if fmt.Sprint(gotV) != fmt.Sprint(wantV) {
					t.Errorf("%q on a %s database: engine=%v cgo=%v", stmt, shape, gotV, wantV)
				}
			})
		}
	}
}
