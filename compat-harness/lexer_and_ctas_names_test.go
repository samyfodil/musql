// Lexer and CREATE TABLE AS SELECT rules: unterminated block comments are valid,
// and duplicate CTAS column names are renamed, not rejected.
package compat

import "testing"

func TestUnterminatedBlockComment(t *testing.T) {
	// Cases alias output columns to avoid comment-normalization artifacts.
	for _, q := range []string{
		`SELECT 1 AS x /* unterminated`,
		`SELECT 1 AS x /* unterminated with * and / inside`,
		`SELECT 1 AS x /**`,
		`SELECT 1 AS x /*/`,
		`SELECT 1 AS x -- line comment`,
		`SELECT 1 AS x /* closed */`,
		`/* leading */ SELECT 1 AS x`,
		// The boundary between unterminated comment and syntax error.
		`SELECT 1 AS x /*`,
	} {
		differ(t, q, []string{q})
	}
	differ(t, "unterminated comment over a table", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT a FROM t ORDER BY a /* trailing`,
		`SELECT count(*) AS c FROM t /* another`,
	})
}

func TestCTASDuplicateColumnNames(t *testing.T) {
	differ(t, "ctas duplicate names", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,2)`,
		`CREATE TABLE c1 AS SELECT a, a FROM t1`,
		`CREATE TABLE c2 AS SELECT 1 AS n, 2 AS n`,
		`CREATE TABLE c3 AS SELECT a AS n, b AS n FROM t1`,
		`CREATE TABLE c4 AS SELECT a, b AS a FROM t1`,
		`CREATE TABLE c5 AS SELECT t1.a, x.a FROM t1, t1 AS x`,
		`CREATE TABLE c6 AS SELECT a AS N, b AS n FROM t1`,
		`CREATE TABLE c7 AS SELECT 1 AS n, 2 AS n, 3 AS n`,
		`SELECT name, sql FROM sqlite_master WHERE name LIKE 'c%' ORDER BY name`,
	})
	// The renamed columns are real columns: they hold the right values and can
	// be selected back by their new names.
	differ(t, "ctas renamed columns hold values", []string{
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,2),(3,4)`,
		`CREATE TABLE c AS SELECT a, b AS a FROM t1`,
		`SELECT * FROM c ORDER BY 1`,
		`SELECT "a:1" FROM c ORDER BY 1`,
		`SELECT sql FROM sqlite_master WHERE name='c'`,
		`PRAGMA table_info(c)`,
	})
	// An explicit column LIST still rejects a duplicate, on both engines.
	differ(t, "explicit list still rejects", []string{
		`CREATE TABLE d(n, n)`,
	})
}

// CTAS with more than five duplicate column names: C SQLite's disambiguation uses
// a randomized counter past the 5th repeat. Only row count is checked, since exact
// column names become non-deterministic in both engines.
func TestCTASSixPlusDuplicateColumnNames(t *testing.T) {
	differ(t, "ctas six-plus duplicate names", []string{
		`CREATE TABLE t0 AS SELECT DISTINCT 0xda, 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0'`,
		`SELECT count(*) FROM t0`,
	})
	// A second, larger shape: a name repeated far past the randomness
	// boundary (66 times, matching distinct.test's own count), interleaved
	// with two OTHER distinct expressions so the repeated name is never
	// contiguous -- confirms every repeat lands, not just the ones next to
	// each other.
	differ(t, "ctas many-plus duplicate names interleaved", []string{
		`CREATE TABLE t9 AS SELECT DISTINCT 1, 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 'lit0', 2, 'lit0', 'lit0', 'lit0', 'lit0', 'lit0'`,
		`SELECT count(*) FROM t9`,
	})
}
