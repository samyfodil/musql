package driver_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestWritableSchemaWritesThroughTheDriver gates PRAGMA writable_schema edits to
// sqlite_schema through database/sql, including connection survival and correct
// reset behavior.
func TestWritableSchemaWritesThroughTheDriver(t *testing.T) {
	for _, tc := range []struct {
		name, stmt string
		wantSQL    string // the sql column of t1's row after RESET, "" if the row is gone
	}{
		{"a sql-text edit", `UPDATE sqlite_schema SET sql='CREATE TABLE t1(id INTEGER PRIMARY KEY)' WHERE name='t1'`,
			"CREATE TABLE t1(id INTEGER PRIMARY KEY)"},
		{"a deleted row", `DELETE FROM sqlite_schema WHERE name='t1'`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "w.musq"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, s := range []string{
				`CREATE TABLE t1(id INTEGER PRIMARY KEY, x TEXT)`,
				`INSERT INTO t1 VALUES(1,'one')`,
				`PRAGMA writable_schema=ON`,
				tc.stmt,
				`PRAGMA writable_schema=RESET`,
			} {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Fatalf("%s: %v", s, eerr)
				}
			}
			var got string
			qerr := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='t1'`).Scan(&got)
			switch {
			case tc.wantSQL == "":
				if qerr == nil {
					t.Errorf("t1's catalog row survived the DELETE: %q", got)
				}
			case qerr != nil:
				t.Fatalf("reading t1's catalog row back: %v", qerr)
			case got != tc.wantSQL:
				t.Errorf("sql = %q, want %q", got, tc.wantSQL)
			}
			// The connection is still usable either way.
			var n int
			if serr := db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&n); serr != nil {
				t.Fatalf("the connection is unusable afterwards: %v", serr)
			}
		})
	}
}

// TestWritableSchemaRootpageIsDeclined pins what stays declined, and why. The
// UPDATE itself is accepted, as C accepts it (the connection keeps its loaded
// schema); what a rootpage MEANS is decided by the next load -- the RESET -- where
// a number that names no object's storage declines: C would read an interior page
// or fail past its end of file, depending on a page layout this format does not
// have (segmentRootpageEditPlan). The decline carries the phrase that books it OUT
// OF SCOPE in the differential harness rather than as a gap.
func TestWritableSchemaRootpageIsDeclined(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "w.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, x TEXT)`,
		`PRAGMA writable_schema=ON`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	// t1 is the first object, so its own rootpage is 2: a no-op, served.
	if _, oerr := db.Exec(`UPDATE sqlite_schema SET rootpage=2 WHERE name='t1'`); oerr != nil {
		t.Fatalf("assigning t1 its own rootpage was declined: %v", oerr)
	}
	if _, uerr := db.Exec(`UPDATE sqlite_schema SET rootpage=7 WHERE name='t1'`); uerr != nil {
		t.Fatalf("the rootpage UPDATE itself was refused; C accepts it: %v", uerr)
	}
	_, eerr := db.Exec(`PRAGMA writable_schema=RESET`)
	if eerr == nil {
		t.Fatal("a RESET over a rootpage naming no object's storage was served; C's answer depends on its page layout")
	}
	if !strings.Contains(eerr.Error(), "not reproducible against C SQLite") {
		t.Errorf("the decline is missing the out-of-scope phrasing: %v", eerr)
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM t1`).Scan(&n); qerr != nil {
		t.Fatalf("the connection is unusable after the decline: %v", qerr)
	}
}
