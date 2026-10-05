// Gates sqlite_compileoption_get(): out-of-range indices are NULL everywhere,
// in-range indices must decline because the build's own options cannot be reported.
package compat

import "testing"

// Verifies out-of-range arguments return NULL on both engines.
func TestCompileOptionGet(t *testing.T) {
	// Plainly out of range on either side of the list.
	differ(t, "sqlite_compileoption_get out of range", []string{
		`SELECT sqlite_compileoption_get(-1) AS a`,
		`SELECT sqlite_compileoption_get(-2) AS a`,
		`SELECT typeof(sqlite_compileoption_get(-1)) AS a`,
		`SELECT sqlite_compileoption_get(-1) IS NULL AS a`,
		`SELECT sqlite_compileoption_get(242) AS a`,
		`SELECT sqlite_compileoption_get(9999) AS a`,
		`SELECT typeof(sqlite_compileoption_get(9999)) AS a`,
	})
	// Out of range after 32-bit truncation.
	differ(t, "sqlite_compileoption_get 32-bit truncation", []string{
		`SELECT sqlite_compileoption_get(4294967295) AS a`,
		`SELECT sqlite_compileoption_get(-2147483649) AS a`,
		`SELECT sqlite_compileoption_get(9223372036854775807) AS a`,
	})
	// REAL and TEXT arguments that land out of range.
	differ(t, "sqlite_compileoption_get non-integer arguments", []string{
		`SELECT sqlite_compileoption_get(1e3) AS a`,
		`SELECT sqlite_compileoption_get(-1.9) AS a`,
		`SELECT sqlite_compileoption_get(1.0e18) AS a`,
		`SELECT sqlite_compileoption_get(1.0e19) AS a`,
		`SELECT sqlite_compileoption_get('-1') AS a`,
		`SELECT sqlite_compileoption_get('  -1  ') AS a`,
		`SELECT sqlite_compileoption_get('-1abc') AS a`,
		`SELECT sqlite_compileoption_get('99999999999999999999') AS a`,
	})
	// Arity is a prepare-time rejection on both sides.
	differ(t, "sqlite_compileoption_get arity", []string{
		`SELECT sqlite_compileoption_get() AS a`,
		`SELECT sqlite_compileoption_get(0, 0) AS a`,
		`SELECT 1 AS alive`,
	})
	// Registered function must appear in pragma_function_list.
	differ(t, "sqlite_compileoption_get in pragma_function_list", []string{
		`SELECT name, builtin, type, enc, narg, flags FROM pragma_function_list` +
			` WHERE name = 'sqlite_compileoption_get'`,
	})
}

// In-range indices must decline (build-dependent content).
func TestCompileOptionGetInRangeStillDeclines(t *testing.T) {
	for _, q := range []string{
		// In-range indices.
		`SELECT sqlite_compileoption_get(0)`,
		`SELECT sqlite_compileoption_get(1)`,
		`SELECT sqlite_compileoption_get(41)`,
		// NULL argument is index 0.
		`SELECT sqlite_compileoption_get(NULL)`,
		// Conversion traps.
		`SELECT sqlite_compileoption_get(4294967296)`,
		`SELECT sqlite_compileoption_get(-9223372036854775808)`,
		`SELECT sqlite_compileoption_get('1e3')`,
		`SELECT sqlite_compileoption_get('abc')`,
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", q, res[0])
		}
	}
}

// Verifies compile-option diagnostics are prohibited in index and generated column expressions.
func TestCompileOptionDiagsProhibitedInIndexAndGeneratedColumn(t *testing.T) {
	differ(t, "compile-option diagnostics in an index expression", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i1 ON t(sqlite_compileoption_used('THREADSAFE'), a)`,
		`CREATE INDEX i2 ON t(sqlite_compileoption_get(0), a)`,
		`CREATE INDEX i3 ON t(a) WHERE sqlite_compileoption_used('THREADSAFE')`,
		`CREATE INDEX i4 ON t(a) WHERE sqlite_compileoption_get(0) IS NULL`,
		`CREATE INDEX i5 ON t(abs(a) + length(sqlite_compileoption_used('X')))`,
		// Control: sqlite_version() and changes() already rejected.
		`CREATE INDEX i6 ON t(sqlite_version(), a)`,
		`CREATE INDEX i7 ON t(changes(), a)`,
		`SELECT name FROM sqlite_master WHERE type='index' ORDER BY name`,
	})
	differ(t, "compile-option diagnostics in a generated column", []string{
		`CREATE TABLE u(a, b AS (sqlite_compileoption_used('THREADSAFE')))`,
		`CREATE TABLE u2(a, b AS (sqlite_compileoption_get(0)))`,
		`CREATE TABLE u3(a, b AS (a + length(sqlite_compileoption_used('X'))))`,
		`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
	})
	// Legal in CHECK constraints and view bodies.
	differ(t, "compile-option diagnostics stay legal in CHECK and a view", []string{
		`CREATE TABLE v(a, CHECK(sqlite_compileoption_used('THREADSAFE')))`,
		`INSERT INTO v VALUES(1)`,
		`SELECT a FROM v`,
		`CREATE VIEW w AS SELECT sqlite_compileoption_used('THREADSAFE') AS x`,
		`SELECT x FROM w`,
	})
}
