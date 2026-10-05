// This file tests PRAGMA handling at prepare time. Some PRAGMAs C SQLite rejects
// at prepare time without side effects; flag PRAGMAs modify connection state during
// preparation, requiring per-name handling.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// tailR31PrepareOnlyRejects holds PRAGMA statements that C SQLite rejects at prepare time.
var tailR31PrepareOnlyRejects = []struct {
	name  string
	setup []string
	stmt  string
}{
	{"encoding=bogus", nil, `pragma encoding=bogus`},
	{"synchronous inside a transaction", []string{`CREATE TABLE t1(a)`, `BEGIN`}, `pragma synchronous = OFF`},
	{"temp_store_directory to a bad path", nil, `PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'`},
	{"compile_options with a trailing star", nil, `PRAGMA compile_options *`},
}

// TestTailR31PragmaPrepareOnlyRejects verifies these PRAGMAs are rejected at prepare time
// without side effects.
func TestTailR31PragmaPrepareOnlyRejects(t *testing.T) {
	for _, c := range tailR31PrepareOnlyRejects {
		t.Run(c.name, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1) // one connection, so state is observable
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			before := tailR31PragmaFingerprint(t, db)
			st, perr := db.Prepare(c.stmt)
			if perr == nil {
				st.Close()
				t.Fatalf("%q PREPARED on the oracle; it cannot be classified by prepare alone", c.stmt)
			}
			if after := tailR31PragmaFingerprint(t, db); after != before {
				t.Errorf("preparing %q moved the oracle's state:\n  before %v\n  after  %v", c.stmt, before, after)
			}
		})
	}
}

// TestTailR31PragmaPrepareIsNotFreeForFlagPragmas shows flag PRAGMAs have side effects
// at prepare time.
func TestTailR31PragmaPrepareIsNotFreeForFlagPragmas(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var before, after int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	st, perr := db.Prepare(`PRAGMA foreign_keys=ON`)
	if perr != nil {
		t.Fatalf("prepare: %v", perr)
	}
	st.Close() // never stepped
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Skipf("preparing PRAGMA foreign_keys=ON left it at %d; the flag arm did not run at prepare time in this build, so a blanket prepare-probe would be safe after all", before)
	}
	t.Logf("preparing (never stepping) PRAGMA foreign_keys=ON moved foreign_keys %d -> %d: a prepare-probe MUST be a per-name set", before, after)
}

// tailR31PragmaFingerprint captures connection state: encoding, synchronous level, temp directory, and journal mode.
func tailR31PragmaFingerprint(t *testing.T, db *sql.DB) [4]string {
	t.Helper()
	var out [4]string
	for i, q := range []string{`PRAGMA encoding`, `PRAGMA synchronous`, `PRAGMA temp_store_directory`, `PRAGMA journal_mode`} {
		rows, err := db.Query(q)
		if err != nil {
			out[i] = "err:" + err.Error()
			continue
		}
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				out[i] += "scan-err"
				continue
			}
			out[i] += "|" + v.String
		}
		rows.Close()
	}
	return out
}
