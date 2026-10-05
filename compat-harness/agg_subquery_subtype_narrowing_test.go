package compat

import "testing"

// TestAggSubqueryArgSubtypeNarrowingMatchesCSQLite tests subqueries in grouped
// aggregate arguments, particularly subtype handling.
func TestAggSubqueryArgSubtypeNarrowingMatchesCSQLite(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(k,v)`,
		`INSERT INTO t VALUES(1,10),(1,20),(2,30)`,
		`CREATE TABLE s(z)`,
		`INSERT INTO s VALUES(5),(6)`,
	}

	// Subqueries in aggregate arguments without subtype reads.
	flLockstep(t, "agg arg with a plain scalar subquery", setup,
		`SELECT k, max(v + (SELECT count(*) FROM s)) FROM t GROUP BY k ORDER BY k`)
	flLockstep(t, "agg arg with EXISTS", setup,
		`SELECT k, sum(v + EXISTS(SELECT 1 FROM s WHERE z>5)) FROM t GROUP BY k ORDER BY k`)
	flLockstep(t, "agg arg with IN (subquery)", setup,
		`SELECT k, count(v IN (SELECT z FROM s)) FROM t GROUP BY k ORDER BY k`)
	flLockstep(t, "agg arg with a correlated subquery", setup,
		`SELECT k, max((SELECT count(*) FROM s WHERE z > t.k)) FROM t GROUP BY k ORDER BY k`)
	flLockstep(t, "agg arg with a compound-arm body", setup,
		`SELECT k, max(v + (SELECT count(*) FROM (SELECT z FROM s UNION ALL SELECT z FROM s))) FROM t GROUP BY k ORDER BY k`)

	// Subtype reads inside subquery bodies.
	flLockstep(t, "subtype read inside a scalar subquery body", []string{
		`CREATE TABLE j(a)`, `INSERT INTO j VALUES(1),(2)`,
	}, `SELECT a, max((SELECT subtype(json_array(1)))) FROM j GROUP BY a ORDER BY a`)

	// Grouped JSON subtype case.
	flLockstep(t, "the original grouped subtype case", []string{},
		`SELECT key, max(subtype(value)) FROM json_each('[[7],[8]]') GROUP BY key`)
}
