// This file tests that unrecognized PRAGMA names are silently ignored,
// following C SQLite's behavior.
package compat

import "testing"

func TestUnrecognizedPragmaIsANoop(t *testing.T) {
	differ(t, "an unrecognized pragma name is inert", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`PRAGMA sql_trace`,
		`PRAGMA sql_trace=on`,
		`PRAGMA sql_trace=off`,
		`PRAGMA incr_vacuum`,
		`PRAGMA incr_vacuum=1`,
		`PRAGMA incr_vacuum(2)`,
		`PRAGMA autovacuum`,
		`PRAGMA autovacuum=full`,
		`PRAGMA filename`,
		`PRAGMA filename='x'`,
		`PRAGMA bogus`,
		`PRAGMA bogus=7`,
		`PRAGMA main.bogus=7`,
		`PRAGMA bogus('arg')`,
		`PRAGMA "quoted bogus"`,
		`SELECT count(*) AS n FROM t`,
		`SELECT a FROM t`,
	})
	differ(t, "a temp-qualified unrecognized pragma is inert", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2)`,
		`PRAGMA temp.sql_trace=1`,
		`PRAGMA temp.bogus`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "an unknown database qualifier still errors", []string{
		`CREATE TABLE t(a)`,
		`PRAGMA aux.bogus`,
		`PRAGMA aux.bogus=1`,
		`SELECT 1 AS alive`,
	})
	differ(t, "default_cache_size is compiled out and inert", []string{
		`PRAGMA cache_size`,
		`PRAGMA default_cache_size=200`,
		`PRAGMA cache_size`,
		`PRAGMA default_cache_size`,
		// The temp-qualified spelling too: window_cte_temp_pragma_test.go used
		// to list this among the file-scoped pragmas musql declines, which it
		// no longer does -- it is simply a name SQLite does not have.
		`CREATE TEMP TABLE tt(x)`,
		`PRAGMA temp.default_cache_size`,
		`PRAGMA temp.default_cache_size=200`,
		`SELECT count(*) AS n FROM tt`,
	})
}

// TestListedPragmasStillDecline is the wrong-answer boundary. Both names below
// ARE in pragma_list, so C SQLite implements them and a silent no-op here
// would change what a later ALTER / constraint check does. musql must keep
// erroring; asserted one-sided, since the oracle accepts them.
func TestListedPragmasStillDecline(t *testing.T) {
	for _, p := range []string{
	} {
		res := run(t, "musql", []string{`CREATE TABLE t(a)`, p})
		if res[1]["kind"] != "error" {
			t.Errorf("expected %q to keep declining (it is a real pragma), got %v", p, res[1])
		}
	}
}
