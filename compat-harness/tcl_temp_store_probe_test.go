// Tests that PRAGMA temp_store can be set only when the temp database
// is not open and not in a transaction.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestTCLR32OOracleRefusesTempStoreInTxn(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string // run first, on the SAME connection as the probe
		stmt    string
		refuses bool
	}{
		{
			name:    "pragma.test#21: temp db open, in transaction",
			setup:   []string{`CREATE TEMP TABLE tt(z)`, `BEGIN`},
			stmt:    `PRAGMA temp_store = 1`,
			refuses: true,
		},
		{
			name:    "temp db never opened, in transaction -- accepted",
			setup:   []string{`BEGIN`},
			stmt:    `PRAGMA temp_store = 1`,
			refuses: false,
		},
		{
			name:    "temp db open, NOT in transaction -- accepted",
			setup:   []string{`CREATE TEMP TABLE tt(z)`},
			stmt:    `PRAGMA temp_store = 1`,
			refuses: false,
		},
		{
			name:    "getter is never restricted",
			setup:   []string{`CREATE TEMP TABLE tt(z)`, `BEGIN`},
			stmt:    `PRAGMA temp_store`,
			refuses: false,
		},
		{
			name:    "value already in force -- accepted even open+in txn",
			setup:   []string{`CREATE TEMP TABLE tt(z)`, `BEGIN`},
			stmt:    `PRAGMA temp_store = 0`,
			refuses: false,
		},
		{
			name:    "not a temp_store pragma at all",
			setup:   []string{`CREATE TEMP TABLE tt(z)`, `BEGIN`},
			stmt:    `PRAGMA synchronous = OFF`,
			refuses: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite3", filepath.Join(dir, "x.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, s := range tc.setup {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Fatalf("setup %q: %v", s, eerr)
				}
			}
			if got := tclR32OOracleRefusesTempStoreInTxn(db, tc.stmt); got != tc.refuses {
				t.Errorf("tclR32OOracleRefusesTempStoreInTxn(%q) = %v, want %v", tc.stmt, got, tc.refuses)
			}
			db.Exec("ROLLBACK") // leave a clean connection for db.Close()
		})
	}
}
