package compat

import "testing"

// TestFlattenSharedName: a CTE or view read twice under one name ("FROM t,
// t") is flattened as C flattens it (#109), and a reference qualified by that
// name stays as ambiguous as C finds it.
func TestFlattenSharedName(t *testing.T) {
	base := []string{
		`CREATE TABLE x(a INTEGER, b TEXT)`,
		`INSERT INTO x VALUES (1, 'p'), (2, 'q'), (3, 'r'), (NULL, 's')`,
		`CREATE VIEW v AS SELECT a, b FROM x WHERE a IS NOT NULL`,
	}
	for _, q := range []string{
		`WITH t AS (SELECT a FROM x) SELECT count(*) FROM t, t`,
		`WITH t AS (SELECT a FROM x) SELECT count(*), sum(1) FROM t, t, t`,
		`WITH t AS (SELECT a, b FROM x WHERE a > 1) SELECT * FROM t, t`,
		`WITH t AS (SELECT a FROM x) SELECT t.a FROM t, t`,
		`WITH t AS (SELECT a FROM x) SELECT a FROM t, t`,
		`WITH t AS (SELECT a FROM x) SELECT count(*) FROM t, t WHERE t.a = 1`,
		`WITH t AS (SELECT a FROM x) SELECT count(*) FROM t, t u WHERE u.a = 2`,
		`WITH t AS (SELECT a FROM x) SELECT u.a, count(*) FROM t, t, t u GROUP BY u.a ORDER BY 1`,
		`WITH t AS (SELECT a FROM x) SELECT count(*) FROM t, t WHERE EXISTS (SELECT 1 FROM x WHERE x.a = 3)`,
		`WITH t AS (SELECT a FROM x) SELECT (SELECT count(*) FROM t, t)`,
		`SELECT count(*) FROM v, v`,
		`SELECT v.a FROM v, v`,
		`SELECT * FROM v, v ORDER BY 1, 3`,
		`SELECT count(*) FROM (SELECT a FROM x) s, (SELECT b FROM x) s`,
		`SELECT s.a FROM (SELECT a FROM x) s, (SELECT a FROM x) s`,
		`SELECT count(*) FROM x t, (SELECT a FROM x) t`,
	} {
		differ(t, "flatten-shared-name", append(append([]string{}, base...), q))
	}
}
