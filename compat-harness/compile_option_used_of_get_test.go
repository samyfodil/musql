// This file gates the one composed compile-option call whose answer is the
// same on every build:
//
//	SELECT sqlite_compileoption_used(sqlite_compileoption_get(0))   -- 1
//
// Every build has at least one option (THREADSAFE), so get(0) is never NULL and
// used() finds it. Only index 0 is provable: get(1) may be NULL elsewhere, so it
// stays declined. Both diagnostics stay prohibited in index expressions,
// partial-index WHERE clauses and generated columns, but allowed in CHECK
// constraints and view bodies.
package compat

import "testing"

// TestCompileOptionUsedOfGetIsOne checks every folded spelling of index 0
// against the oracle, plus the out-of-range case, which answers NULL unfolded.
func TestCompileOptionUsedOfGetIsOne(t *testing.T) {
	// Unaliased, so the result-column name is compared too.
	differ(t, "ctime-2.3 sqlite_compileoption_used(sqlite_compileoption_get(0))", []string{
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0))`,
	})
	// Every literal that converts to C int 0: an INTEGER, a truncated REAL, an
	// integer TEXT prefix, and NULL.
	differ(t, "sqlite_compileoption_used(get(0)) literal spellings", []string{
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0)) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0.0)) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0.4)) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get('0')) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get('0abc')) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(NULL)) AS a`,
		`SELECT typeof(sqlite_compileoption_used(sqlite_compileoption_get(0))) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0)) + 1 AS a`,
		`SELECT SQLITE_COMPILEOPTION_USED(SQLITE_COMPILEOPTION_GET(0)) AS a`,
	})
	// The MinInt64 token folds to a literal and truncates to 0.
	differ(t, "sqlite_compileoption_used(get(MinInt64)) truncates to index 0", []string{
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(-9223372036854775808)) AS a`,
	})
	// Out of range: get() answers NULL, so used(NULL) is NULL.
	differ(t, "sqlite_compileoption_used(get(out of range)) is NULL", []string{
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(-1)) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(9999)) AS a`,
		`SELECT typeof(sqlite_compileoption_used(sqlite_compileoption_get(-1))) AS a`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(9223372036854775807)) AS a`,
	})
	// The same call in a WHERE clause, a view body, and a CHECK constraint (the
	// last one on the write path).
	differ(t, "sqlite_compileoption_used(get(0)) in WHERE, a view and a CHECK", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2)`,
		`SELECT a FROM t WHERE sqlite_compileoption_used(sqlite_compileoption_get(0)) ORDER BY a`,
		`CREATE VIEW w AS SELECT sqlite_compileoption_used(sqlite_compileoption_get(0)) AS x`,
		`SELECT x FROM w`,
		`CREATE TABLE c(a, CHECK(sqlite_compileoption_used(sqlite_compileoption_get(0))))`,
		`INSERT INTO c VALUES(1)`,
		`SELECT a FROM c`,
	})
}

// TestCompileOptionUsedOfGetBoundaryStillDeclines lists statements whose
// answer differs between correct builds, so musql must decline them even though
// the oracle answers.
func TestCompileOptionUsedOfGetBoundaryStillDeclines(t *testing.T) {
	for _, q := range []string{
		// Not provable: 1 here, NULL on a build with only THREADSAFE.
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(1))`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(41))`,
		// Signed constant tokens stay a unary node and are not folded.
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(-0))`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(+0))`,
		// Only a single constant token folds, not an expression that evaluates to 0.
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(abs(0)))`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(0+0))`,
		`SELECT sqlite_compileoption_used(sqlite_compileoption_get(CAST(0 AS INTEGER)))`,
		`SELECT sqlite_compileoption_used((SELECT sqlite_compileoption_get(0)))`,
		// The inner call alone is the build's identity, booked out of scope.
		`SELECT sqlite_compileoption_get(0)`,
	} {
		res := run(t, "musql", []string{q})
		if res[0]["kind"] != "error" {
			t.Errorf("expected a clean decline for %q, got %v", q, res[0])
		}
	}
}

// TestCompileOptionUsedOfGetStillProhibitedInDDL checks the fold opens no hole:
// the composed call is refused in DDL positions like the bare one. The last two
// cases (CHECK, view) are allowed, so rejecting everything cannot pass.
func TestCompileOptionUsedOfGetStillProhibitedInDDL(t *testing.T) {
	differ(t, "composed compile-option call in index/generated-column DDL", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX i1 ON t(sqlite_compileoption_used(sqlite_compileoption_get(0)), a)`,
		`CREATE INDEX i2 ON t(a) WHERE sqlite_compileoption_used(sqlite_compileoption_get(0))`,
		`CREATE TABLE u(a, b AS (sqlite_compileoption_used(sqlite_compileoption_get(0))))`,
		`SELECT name FROM sqlite_master ORDER BY name`,
	})
	differ(t, "composed compile-option call stays legal in CHECK and a view", []string{
		`CREATE TABLE v(a, CHECK(sqlite_compileoption_used(sqlite_compileoption_get(0))))`,
		`INSERT INTO v VALUES(1)`,
		`SELECT a FROM v`,
		`CREATE VIEW w AS SELECT sqlite_compileoption_used(sqlite_compileoption_get(0)) AS x`,
		`SELECT x FROM w`,
	})
}

// TestCompileOptionsBuildIdentityOnlyFiresOnProjectedValues checks that
// tclCompileOptionsBuildIdentity books a statement out of scope only when its
// answer depends on the build, not merely when it mentions
// pragma_compile_options. count(*) does depend on it.
func TestCompileOptionsBuildIdentityOnlyFiresOnProjectedValues(t *testing.T) {
	for _, c := range []struct {
		stmt string
		want bool
		why  string
	}{
		{`SELECT compile_options AS x FROM pragma_compile_options WHERE x LIKE 'max_mmap_size=%'`, true,
			"the corpus shape: the option TEXT is projected, and no two builds need agree on it"},
		{`SELECT * FROM pragma_compile_options`, true,
			"a select-list star expands to the option text"},
		{`SELECT count(*) FROM pragma_compile_options`, true,
			"counts the build's OWN option rows, so the answer differs by build exactly like the text does"},
		// Not listed: "count(*) ... WHERE 0" answers 0 everywhere, but the rule is
		// syntactic and books it out of scope too.
		{`SELECT 1 FROM pragma_compile_options LIMIT 1`, false,
			"projects a literal"},
	} {
		if got := tclCompileOptionsBuildIdentity(c.stmt); got != c.want {
			t.Errorf("tclCompileOptionsBuildIdentity(%q) = %v, want %v -- %s", c.stmt, got, c.want, c.why)
		}
	}
}
