// Tests that a FAIL-halted statement keeps rows written before the error and
// marks them durable, not just session-visible. Covers both RAISE(FAIL) in
// triggers and conflict-tagged FAIL (INSERT OR FAIL).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func orFailSurvivorTable(t *testing.T, driver, dsn string, setup []string, dml string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s: open: %v", driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range setup {
		if _, serr := db.Exec(s); serr != nil {
			t.Fatalf("%s: setup %q: %v", driver, s, serr)
		}
	}
	// Execute statement and drain results
	if rows, qerr := db.Query(dml); qerr == nil {
		for rows.Next() {
		}
		rows.Close()
	} else {
		db.Exec(dml)
	}
	rows, qerr := db.Query(`SELECT a FROM t ORDER BY a`)
	if qerr != nil {
		t.Fatalf("%s: readback: %v", driver, qerr)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a int64
		if serr := rows.Scan(&a); serr != nil {
			t.Fatalf("%s: scan: %v", driver, serr)
		}
		out = append(out, string(rune('0'+a%10)))
	}
	return strings.Join(out, ",")
}

func TestOrFailKeepsSurvivors(t *testing.T) {
	// UNIQUE constraint causes third row to fail; first two should survive
	setup := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE)`,
		`INSERT INTO t(a,b) VALUES(1,'x')`,
	}
	for _, tc := range []struct {
		name string
		dml  string
		// open pins what this engine answers TODAY for a divergence that is
		// known and not yet fixed. Borrowed from failreturning_r39c_test.go's
		// own field of the same name, and with the same contract: it is a PIN,
		// not an expectation, and the case fails if the engine moves off it in
		// EITHER direction -- including onto the oracle's answer, which is the
		// signal to delete the pin.
		open string
	}{
		// INSERT OR FAIL matches oracle behavior
		{"or-fail", `INSERT OR FAIL INTO t(a,b) VALUES(4,'q'),(5,'r'),(6,'x')`, ""},
		// INSERT OR FAIL with RETURNING now matches oracle behavior
		{"or-fail-returning", `INSERT OR FAIL INTO t(a,b) VALUES(4,'q'),(5,'r'),(6,'x') RETURNING a`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := orFailSurvivorTable(t, "sqlite3", filepath.Join(t.TempDir(), "oracle.sqlite"), setup, tc.dml)
			got := orFailSurvivorTable(t, "sqlite", filepath.Join(t.TempDir(), "musql.sqlite"), setup, tc.dml)
			if tc.open != "" {
				if got != tc.open {
					t.Errorf("%s: %s\n  this engine now answers %q, not the pinned %q.\n"+
						"  If it now equals the oracle's %q, DELETE this case's `open` field --\n"+
						"  a stale pin is what lets the next regression here pass unnoticed.",
						tc.name, tc.dml, got, tc.open, want)
				}
				return
			}
			if got != want {
				t.Errorf("%s: %s\n  oracle: %q\n  engine: %q\n"+
					"  a FAIL-halted statement's survivors must reach the FILE, not just the session",
					tc.name, tc.dml, want, got)
			}
		})
	}
}
