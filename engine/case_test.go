// This file tests CASE expression support (sql_ast.go's CaseExpr, parsed in
// sql_parser.go, evaluated by evalCase): both the searched
// form (CASE WHEN <cond> THEN <r> ... [ELSE <r>] END) and the simple form
// (CASE <base> WHEN <v> THEN <r> ... [ELSE <r>] END), including NULL
// propagation and the missing-ELSE-yields-NULL rule. Every case is checked
// against the reference (cgo-backed) database/sql driver, not just inferred
// from documentation.
package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// buildCaseTestDB creates a table exercising CASE's interesting corners:
// NULL bases/WHEN-values/results, an ELSE clause, a missing ELSE clause, and
// values that participate in affinity-coerced simple-CASE comparisons.
func buildCaseTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	exec(`CREATE TABLE t1 (a INTEGER, b INTEGER, c TEXT)`)

	type row struct {
		a, b any
		c    any
	}
	rows := []row{
		{1, 10, "x"},
		{2, 20, "y"},
		{3, nil, "z"},  // NULL b: exercises NULL WHEN-condition / NULL simple-CASE base
		{nil, 5, "w"},  // NULL a
		{4, 4, nil},    // NULL result column, a == b
		{5, 100, "5"},  // c is the TEXT '5', for simple-CASE affinity coercion vs INTEGER a
		{-1, -1, "-1"}, // negative values
	}
	st, err := db.Prepare(`INSERT INTO t1 (a,b,c) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := st.Exec(r.a, r.b, r.c); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	return path, db
}

func TestCaseExprMatchesReference(t *testing.T) {
	path, db := buildCaseTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Searched CASE: ordinary WHEN/THEN/ELSE.
		`SELECT a, CASE WHEN a > b THEN 'a-bigger' WHEN a < b THEN 'b-bigger' ELSE 'tie' END FROM t1`,
		// Searched CASE, no ELSE: falls to NULL when no WHEN matches.
		`SELECT a, CASE WHEN a > b THEN 1 END FROM t1`,
		// Searched CASE where the condition itself can be NULL (a or b NULL):
		// a NULL condition must never be treated as "matched".
		`SELECT a, b, CASE WHEN a > 0 THEN 'pos' WHEN a <= 0 THEN 'nonpos' ELSE 'unknown' END FROM t1`,
		// Searched CASE with multiple WHEN arms, first-match-wins order.
		`SELECT a, CASE WHEN a >= 4 THEN 'big' WHEN a >= 1 THEN 'small' WHEN a IS NULL THEN 'null' ELSE 'neg' END FROM t1`,
		// A THEN/ELSE result that is itself NULL.
		`SELECT a, CASE WHEN a = 4 THEN c ELSE 'other' END FROM t1`,

		// Simple CASE: base compared against each WHEN value with "=" semantics.
		`SELECT a, CASE a WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'other' END FROM t1`,
		// Simple CASE, no ELSE.
		`SELECT a, CASE a WHEN 1 THEN 'one' WHEN 2 THEN 'two' END FROM t1`,
		// Simple CASE with a NULL base: must never match any WHEN, regardless
		// of whether a WHEN value is itself NULL or not.
		`SELECT a, CASE a WHEN 1 THEN 'one' ELSE 'fallback' END FROM t1`,
		// Simple CASE, base is a computed expression.
		`SELECT a, b, CASE a+1 WHEN b THEN 'adjacent' ELSE 'not-adjacent' END FROM t1`,
		// Simple CASE exercising comparison-affinity coercion (INTEGER column
		// vs a TEXT literal that is a well-formed number).
		`SELECT a, CASE a WHEN '5' THEN 'matched-text-five' ELSE 'no' END FROM t1`,
		// Simple CASE whose base is a TEXT column, one WHEN value NULL.
		`SELECT c, CASE c WHEN 'x' THEN 1 WHEN NULL THEN 2 ELSE 0 END FROM t1`,

		// CASE nested inside arithmetic/comparison (as an ordinary expression).
		`SELECT a, (CASE WHEN a > 0 THEN a ELSE -a END) * 2 FROM t1`,
		// CASE in WHERE.
		`SELECT a FROM t1 WHERE CASE WHEN a > 0 THEN 1 ELSE 0 END = 1`,
		// CASE in ORDER BY.
		`SELECT a FROM t1 ORDER BY CASE WHEN a IS NULL THEN 1 ELSE 0 END, a`,
		// Nested CASE (a THEN/ELSE branch is itself a CASE).
		`SELECT a, CASE WHEN a IS NULL THEN 'null' ELSE CASE WHEN a < 0 THEN 'neg' ELSE 'nonneg' END END FROM t1`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}
