package compat

import "testing"

// TestParseR31EmptyInOrdinal verifies empty IN() folds in ORDER BY and GROUP BY
// are treated as constants, not ordinals.
func TestParseR31EmptyInOrdinal(t *testing.T) {
	differ(t, "r31-emptyin-orderby-constant", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(2,'x'),(1,'y')`,
		`SELECT a FROM t ORDER BY (1 NOT IN ())`,
		`SELECT a FROM t ORDER BY (nosuchcol NOT IN ())`,
		`SELECT a FROM t ORDER BY ((a,b) NOT IN ())`,
		`SELECT a FROM t ORDER BY (1 IN ())`,
	})
	differ(t, "r31-emptyin-groupby-constant", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(2,'x'),(1,'y')`,
		`SELECT count(*) FROM t GROUP BY (1 NOT IN ())`,
		`SELECT count(*) FROM t GROUP BY (nosuchcol IN ())`,
		`SELECT count(*) FROM t GROUP BY ((a,b) NOT IN ())`,
	})
	differ(t, "r31-emptyin-compound-orderby", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(2),(1)`,
		`SELECT a FROM t UNION ALL SELECT a FROM t ORDER BY (1 NOT IN ())`,
	})
	// Every ordinal site in this engine funnels through one predicate
	// (orderByOrdinal, query.go), but they are reached from different
	// compilers: a plain sort, a window's own ORDER BY, and a window
	// PARTITION/ORDER pair each ask separately.
	differ(t, "r31-emptyin-window-orderby", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(2,'x'),(1,'y')`,
		`SELECT a, row_number() OVER (ORDER BY (1 NOT IN ())) FROM t`,
		`SELECT a, count(*) OVER (PARTITION BY (nosuchcol IN ()) ORDER BY a) FROM t ORDER BY a`,
	})
}
