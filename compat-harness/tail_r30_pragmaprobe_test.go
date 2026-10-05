package compat

// This file measures whether the oracle rejects certain PRAGMA statements.
// It checks if PRAGMA errors occur at prepare time. Skipped unless
// TAIL_R30_PRAGMA_PROBE is set.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestTailR30PragmaOracleRejects(t *testing.T) {
	if os.Getenv("TAIL_R30_PRAGMA_PROBE") == "" {
		t.Skip("set TAIL_R30_PRAGMA_PROBE=1 to run this measurement")
	}
	cases := []struct {
		setup []string
		stmt  string
	}{
		{nil, "pragma encoding=bogus"},
		{[]string{"CREATE TABLE t(a)", "BEGIN"}, "pragma synchronous = OFF"},
		{[]string{"CREATE TABLE t(a)", "BEGIN"}, "PRAGMA temp_store = 1"},
		{[]string{"CREATE TABLE t(a)", "CREATE TEMP TABLE tt(x)", "BEGIN"}, "PRAGMA temp_store = 1"},
		{nil, "PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'"},
		{[]string{"CREATE TABLE t(a)", "BEGIN", "INSERT INTO t VALUES(1)"}, "PRAGMA journal_mode = WAL"},
		{[]string{"CREATE TABLE t(a)", "BEGIN", "INSERT INTO t VALUES(1)"}, "PRAGMA journal_mode = memory"},
		{nil, "PRAGMA compile_options *"},
		{nil, "PRAGMA temp.page_count"},
		{nil, "PRAGMA writable_schema = RESET"},
		{[]string{"CREATE TABLE t(a)"}, "PRAGMA optimize"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		cdb.SetMaxOpenConns(1)
		for _, s := range c.setup {
			if _, err := cdb.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		prepErr := ""
		if st, err := cdb.Prepare(c.stmt); err != nil {
			prepErr = err.Error()
		} else {
			st.Close()
		}
		runErr := ""
		if _, err := cdb.Exec(c.stmt); err != nil {
			runErr = err.Error()
		}
		t.Logf("%-58q setup=%v\n     prepare: %q\n     exec   : %q", c.stmt, c.setup, prepErr, runErr)
		cdb.Close()
	}
}
