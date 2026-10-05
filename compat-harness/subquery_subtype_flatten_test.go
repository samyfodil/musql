package compat

import "testing"

// TestSubqueryFlattenKeepsSubtypeMatchesCSQLite compares with C SQLite
// when a JSON subtype crosses a FROM-clause subquery, view or CTE: only when C
// flattens it and the column is a bare column reference, which re-reads the
// virtual table's value and its subtype. A co-routine, a materialization and a
// soft COLLATE (anything but a bare column) all clear it.
func TestSubqueryFlattenKeepsSubtypeMatchesCSQLite(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES (1)`,
		`CREATE VIEW vj AS SELECT value AS v FROM json_each('[[7]]')`,
		`CREATE VIEW vj2(w) AS SELECT value FROM json_tree('{"a":[1,2]}') WHERE type = 'array'`,
	}
	for _, q := range []string{
		// Flattened, bare: kept.
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]'))`,
		`WITH c AS (SELECT value AS v FROM json_each('[[7]]')) SELECT json_quote(v) FROM c`,
		`WITH c AS NOT MATERIALIZED (SELECT value AS v FROM json_each('[[7]]')) SELECT json_quote(v) FROM c`,
		`WITH c AS (SELECT value AS v FROM json_each('[[7]]')) SELECT json_quote(a.v), json_quote(b.v) FROM c a, c b`,
		`SELECT json_quote(v) FROM vj`,
		`SELECT json_quote(w) FROM vj2`,
		`SELECT json_quote(v) FROM t1, (SELECT value AS v FROM json_each('[[7]]'))`,
		`SELECT json_quote(v) FROM t1 LEFT JOIN (SELECT value AS v FROM json_each('[[7]]')) ON 1`,
		`SELECT json_array(v) FROM (SELECT value AS v FROM json_each('[{"a":1}]'))`,
		`SELECT json_quote(v) FROM (SELECT j.value AS v FROM json_each('[[7]]') AS j, t1)`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')) WHERE v IS NOT NULL`,
		`SELECT json_quote(w) FROM (SELECT v AS w FROM (SELECT value AS v FROM json_each('[[7]]')))`,
		`SELECT json_quote(v) FROM (SELECT * FROM (SELECT value AS v FROM json_each('[[7]]')))`,
		`SELECT json_quote(value) FROM (SELECT * FROM json_each('[[7]]'))`,
		`SELECT json_quote(value), key FROM (SELECT key, * FROM json_each('[[7],[8]]'))`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')) ORDER BY 1`,
		`SELECT json_quote(v), count(*) FROM (SELECT value AS v FROM json_each('[[7]]')) GROUP BY 1`,
		`SELECT DISTINCT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]'))`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')) UNION ALL SELECT 1`,
		`SELECT (SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')))`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')), json_each('[1]') AS k`,
		`SELECT json_quote(v), json_quote(k) FROM (SELECT value AS v, key AS k FROM json_each('{"a":[7]}'))`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')) RIGHT JOIN t1 ON 1`,
		`SELECT DISTINCT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]')) RIGHT JOIN t1 ON 1`,
		// Not flattened, or not bare: cleared.
		`SELECT json_quote(v) FROM (SELECT +value AS v FROM json_each('[[7]]'))`,
		`SELECT json_quote(v) FROM (SELECT value COLLATE nocase AS v FROM json_each('[[7]]'))`,
		`WITH c AS MATERIALIZED (SELECT value AS v FROM json_each('[[7]]')) SELECT json_quote(v) FROM c`,
		`SELECT json_quote(v) FROM (SELECT value AS v FROM json_each('[[7]]') ORDER BY 1)`,
		`SELECT json_quote(v) FROM (SELECT DISTINCT value AS v FROM json_each('[[7]]'))`,
		`SELECT json_quote(v) FROM (SELECT value AS v, count(*) FROM json_each('[[7]]'))`,
		`SELECT json_quote(v) FROM (SELECT json('[7]') AS v FROM t1)`,
		`SELECT json_quote(v) FROM (SELECT j.value AS v FROM json_each('[[7]]') AS j, t1) RIGHT JOIN t1 ON 1`,
		`SELECT json_quote(v) FROM t1 RIGHT JOIN (SELECT value AS v FROM json_each('[[7]]')) ON 1`,
		`SELECT json_quote(v), row_number() OVER () FROM (SELECT value AS v FROM json_each('[[7]]'))`,
	} {
		t.Run(q, func(t *testing.T) { differ(t, q, append(append([]string{}, setup...), q)) })
	}
}
