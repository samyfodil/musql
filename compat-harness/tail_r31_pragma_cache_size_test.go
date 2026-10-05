// This file tests pragma_cache_size as a table-valued function.
package compat

import (
	"encoding/json"
	"testing"
)

// differAllowingDeclines compares results but allows selected musql declines.
func differAllowingDeclines(t *testing.T, name string, stmts []string) {
	t.Helper()
	oracle := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	if len(oracle) != len(got) {
		t.Fatalf("[%s] statement count %d vs %d", name, len(oracle), len(got))
	}
	for i := range oracle {
		ob, _ := json.Marshal(oracle[i])
		gb, _ := json.Marshal(got[i])
		if string(ob) == string(gb) {
			continue
		}
		if got[i]["kind"] == "error" && oracle[i]["kind"] != "error" {
			continue // a declared gap: declined, never answered wrongly
		}
		t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:     %s\n  cgo:     %s\n  musql:  %s",
			name, stmts[i], ob, gb)
	}
}

func TestTailR31PragmaCacheSizeTVF(t *testing.T) {
	// The shape itself: one column named after the pragma, one row.
	differ(t, "pragma_cache_size shape", []string{
		`CREATE TABLE t1(a)`,
		`SELECT * FROM pragma_cache_size`,
		`SELECT cache_size FROM pragma_cache_size`,
		`SELECT count(*) FROM pragma_cache_size`,
		`SELECT typeof(cache_size) FROM pragma_cache_size`,
	})
	// where.test 29.1 verbatim (trimmed to the shape that matters): the vtab's
	// own ROWID is referenced, which is the only thing that statement reads.
	differ(t, "pragma_cache_size rowid, where.test 29.1", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INT)`,
		`SELECT DISTINCT 'xyz' FROM pragma_cache_size WHERE rowid OR abs(0) ORDER BY 1, 1, 1, 1`,
		`SELECT rowid, cache_size FROM pragma_cache_size`,
	})
	// The value TRACKS the setter -- the case a wrapper is most likely to get
	// wrong, because the connection's value has to reach the read snapshot the
	// TVF is answered from.
	differ(t, "pragma_cache_size follows its setter", []string{
		`CREATE TABLE t1(a)`,
		`SELECT * FROM pragma_cache_size`,
		`PRAGMA cache_size=100`,
		`SELECT * FROM pragma_cache_size`,
		`PRAGMA cache_size=-4000`,
		`SELECT * FROM pragma_cache_size`,
		`PRAGMA cache_size=0`,
		`SELECT * FROM pragma_cache_size`,
	})
	// The identical statement text either side of a setter, for the compiled-plan
	// cache: a Program memoized on SQL text alone must not replay the old value.
	differ(t, "pragma_cache_size across a setter, identical text", []string{
		`CREATE TABLE t1(a)`,
		`SELECT cache_size FROM pragma_cache_size`,
		`PRAGMA cache_size=7`,
		`SELECT cache_size FROM pragma_cache_size`,
		`PRAGMA cache_size=9`,
		`SELECT cache_size FROM pragma_cache_size`,
	})
	// The hidden SCHEMA column: nameable, and -- since cache_size has no "arg"
	// column at all -- a call's FIRST argument. The echoed value is NULL even
	// when one was supplied (pragmaVtabFilter writes azArg[1] where
	// pragmaVtabColumn reads azArg[0]).
	//
	// A schema CONSTRAINT is a declared DECLINE here, not a served shape, so
	// these run through differAllowingDeclines -- which still fails on any
	// musql answer that DIFFERS from the oracle's, and only tolerates an error
	// where the oracle answered rows. See engine's pragmaVtabSchemaOnlyHidden.
	differAllowingDeclines(t, "pragma_cache_size schema column", []string{
		`CREATE TABLE t1(a)`,
		`SELECT cache_size, schema FROM pragma_cache_size`,
		`SELECT * FROM pragma_cache_size('main')`,
		`SELECT cache_size, schema FROM pragma_cache_size('main')`,
		`SELECT * FROM pragma_cache_size('temp')`,
		`SELECT * FROM pragma_cache_size WHERE schema='temp'`,
		`SELECT * FROM pragma_cache_size WHERE schema='main'`,
	})
	// temp keeps its OWN value: pragmaTuningDefault has temp at 0 where main is
	// -2000, and a qualified setter moves one database. The unqualified reads
	// must still be exact -- only the schema-argument spellings may decline.
	differAllowingDeclines(t, "pragma_cache_size temp is a separate database", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA temp.cache_size=55`,
		`SELECT * FROM pragma_cache_size('temp')`,
		`SELECT * FROM pragma_cache_size('main')`,
		`SELECT * FROM pragma_cache_size`,
	})
	// Arity: cache_size has exactly ONE hidden column, so a second call argument
	// is "too many arguments on pragma_cache_size() - max 1".
	differ(t, "pragma_cache_size arity", []string{
		`CREATE TABLE t1(a)`,
		`SELECT * FROM pragma_cache_size('main','extra')`,
	})
	// A join against a real table, so the TVF is not the only FROM item -- the
	// shape the corpus statement above actually compiles to.
	differ(t, "pragma_cache_size joined", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1),(2)`,
		`PRAGMA cache_size=321`,
		`SELECT t1.a, p.cache_size FROM t1, pragma_cache_size AS p ORDER BY t1.a`,
	})
}
