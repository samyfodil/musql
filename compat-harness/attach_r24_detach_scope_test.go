package compat

// DETACH "main" and "temp" are always refused.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestR24OracleAlwaysRefusesDetachOfMainAndTemp(t *testing.T) {
	for _, withTemp := range []bool{false, true} {
		withTemp := withTemp
		name := "no-temp-database"
		if withTemp {
			name = "temp-database-open"
		}
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
				t.Fatal(err)
			}
			if withTemp {
				if _, err := db.Exec(`CREATE TEMP TABLE tt(y)`); err != nil {
					t.Fatal(err)
				}
			}
			// With a real attachment to verify rejection is not just empty state.
			if _, err := db.Exec(`ATTACH '` + filepath.Join(t.TempDir(), "a.db") + `' AS aux`); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []string{
				`DETACH main`, `DETACH DATABASE 'MAIN'`, `DETACH temp`, `DETACH Temp`,
			} {
				_, err := db.Exec(stmt)
				if err == nil {
					t.Errorf("%q: oracle ACCEPTED it; tclOracleRefusesDetach would book a false mutual reject", stmt)
					continue
				}
				if !strings.Contains(err.Error(), "cannot detach database") &&
					!strings.Contains(err.Error(), "no such database") {
					t.Errorf("%q: oracle rejected it with an unexpected message %q", stmt, err)
				}
				if !tclOracleRefusesDetach(stmt) {
					t.Errorf("%q: tclOracleRefusesDetach said false, but the oracle rejects it", stmt)
				}
			}
			// Predicate must not claim ordinary DETACHes.
			for _, stmt := range []string{`DETACH aux`, `DETACH nosuchdb`, `DETACH DATABASE mainly`} {
				if tclOracleRefusesDetach(stmt) {
					t.Errorf("%q: tclOracleRefusesDetach must not claim this", stmt)
				}
			}
		})
	}
}
