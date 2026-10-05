// This file tests EXISTS/NOT EXISTS subquery expressions (sql_ast.go's
// ExistsExpr, parsed in sql_parser.go's parsePrimary EXISTS case, evaluated
// by evalExists via the reentrant execSelect in query.go):
// non-correlated and correlated forms, EXISTS over an empty result, EXISTS
// ignoring a multi-column/"SELECT *" subquery body, and EXISTS used both in
// WHERE (combined with AND/OR/NOT, the select1.test idiom) and as an
// ordinary select-list scalar (always 0 or 1, never NULL). Every case is
// checked against the reference (cgo-backed) database/sql driver.
package engine_test

import (
	"testing"

	"github.com/samyfodil/musql/engine"
)

func TestExistsMatchesReference(t *testing.T) {
	path, db := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Correlated EXISTS / NOT EXISTS in WHERE: for each t1 row, does a
		// matching t2 row (same a) exist.
		`SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a)`,
		`SELECT a FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a)`,

		// The exact select1.test idiom: correlated EXISTS via a self-join
		// alias ("t1 AS x ... x.b < t1.b").
		`SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t1 AS x WHERE x.b < t1.b)`,
		`SELECT a FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t1 AS x WHERE x.b < t1.b)`,

		// EXISTS combined with an ordinary predicate via AND/OR, as
		// select1.test does throughout.
		`SELECT a FROM t1 WHERE b > 20 AND EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a)`,
		`SELECT a FROM t1 WHERE b > 20 OR EXISTS (SELECT 1 FROM t1 AS x WHERE x.b < t1.b)`,

		// Non-correlated EXISTS: same truth value for every outer row.
		`SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.a > 100)`,
		`SELECT a FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t2 WHERE t2.a > 100)`,

		// EXISTS over an always-empty table (t3): EXISTS is always false,
		// NOT EXISTS is always true -- every outer row (including ones with
		// NULL columns) is affected identically since EXISTS never looks at
		// row contents.
		`SELECT a FROM t1 WHERE EXISTS (SELECT 1 FROM t3)`,
		`SELECT a FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t3)`,
		`SELECT a FROM t1 WHERE EXISTS (SELECT x FROM t3 WHERE x > 0)`,

		// EXISTS ignores the subquery body's column count/values entirely --
		// a multi-column or "SELECT *" body is fine, unlike a scalar
		// subquery.
		`SELECT a FROM t1 WHERE EXISTS (SELECT * FROM t2 WHERE t2.b > 10)`,
		`SELECT a FROM t1 WHERE EXISTS (SELECT a, b FROM t2 WHERE t2.a = t1.a)`,

		// EXISTS as an ordinary select-list scalar expression (0/1, never
		// NULL), both non-correlated and correlated.
		`SELECT a, EXISTS (SELECT 1 FROM t2 WHERE t2.a > 100) FROM t1`,
		`SELECT a, EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a) FROM t1`,
		`SELECT a, NOT EXISTS (SELECT 1 FROM t2 WHERE t2.a = t1.a) FROM t1`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}
