// Tests scalar subquery expressions: correlated and non-correlated, with various
// WHERE and aggregate patterns.
package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func buildSubqueryTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subquery.sqlite")
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

	exec(`CREATE TABLE t1 (a INTEGER, b INTEGER, c INTEGER)`)
	exec(`CREATE TABLE t2 (a INTEGER, b INTEGER)`)
	exec(`CREATE TABLE t3 (x INTEGER)`) // always empty
	exec(`CREATE TABLE t4 (a INTEGER)`) // single-column, for "IN <table-name>"

	t1rows := [][3]any{
		{1, 10, 100},
		{2, 20, 200},
		{3, nil, 300}, // NULL b
		{4, 40, nil},  // NULL c
		{5, 50, 500},
	}
	st, err := db.Prepare(`INSERT INTO t1 (a,b,c) VALUES (?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range t1rows {
		if _, err := st.Exec(r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	t2rows := [][2]any{
		{1, 5},
		{2, 15},
		{3, 25},
		{4, nil},
	}
	st, err = db.Prepare(`INSERT INTO t2 (a,b) VALUES (?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range t2rows {
		if _, err := st.Exec(r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	st, err = db.Prepare(`INSERT INTO t4 (a) VALUES (?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{2, 4, nil} {
		if _, err := st.Exec(v); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	return path, db
}

func TestSubqueryExprMatchesReference(t *testing.T) {
	path, db := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Non-correlated scalar subquery (aggregate body), alongside a plain
		// outer column.
		`SELECT (SELECT count(*) FROM t1), a FROM t2`,
		// Non-correlated scalar subquery, non-aggregate body (single row via
		// LIMIT 1); every outer row sees the same value.
		`SELECT a, (SELECT a FROM t1 ORDER BY a LIMIT 1) FROM t2`,

		// Correlated scalar subquery: aggregate body referencing the outer
		// row's column.
		`SELECT a, (SELECT count(*) FROM t2 WHERE t2.b < t1.a) FROM t1`,
		// Same shape, but the subquery's own table is a self-join via alias
		// (the "t1 AS x ... x.col ... t1.col" pattern used throughout
		// select1.test): the outer table's rows are compared against the
		// subquery's own (differently-scoped) instance of the same table.
		`SELECT a, (SELECT count(*) FROM t1 AS x WHERE x.b < t1.b) FROM t1`,
		`SELECT a, (SELECT count(*) FROM t1 AS x WHERE x.c > t1.c AND x.a < t1.a) FROM t1`,

		// Scalar subquery in WHERE: aggregate over the whole (outer) table,
		// non-correlated, filtering outer rows.
		`SELECT a, c FROM t1 WHERE c > (SELECT avg(c) FROM t1)`,
		// Correlated subquery in WHERE via EXISTS-shaped count comparison.
		`SELECT a FROM t1 WHERE (SELECT count(*) FROM t2 WHERE t2.a = t1.a) > 0`,

		// CASE combined with a correlated/non-correlated scalar subquery --
		// the exact select1.test idiom this feature was built to unlock.
		`SELECT a, b, CASE WHEN c > (SELECT avg(c) FROM t1) THEN a*2 ELSE b*10 END FROM t1`,

		// Empty subquery -> NULL: t3 is always empty, both as a bare column
		// pick and as an aggregate-free correlated-looking (but not actually
		// correlated, t3 has no matching column) subquery.
		`SELECT a, (SELECT x FROM t3) FROM t1`,
		`SELECT a, (SELECT x FROM t3 WHERE x > 0) FROM t1`,

		// Subquery as the sole select-list item, ORDER BY the outer column.
		`SELECT (SELECT count(*) FROM t1 AS x WHERE x.b < t1.b) FROM t1 ORDER BY a`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestSubqueryExprKnownUnsupported documents a construct this package's
// scalar-subquery support deliberately does not attempt: anything not
// reducible to "exactly one result column, take row 0" is rejected with an
// error rather than guessed at. (EXISTS and IN (SELECT ...) subqueries used
// to be out of scope too, but are now supported -- see exists_test.go and
// in_subquery_test.go.)
func TestSubqueryExprKnownUnsupported(t *testing.T) {
	path, _ := buildSubqueryTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []struct {
		sql    string
		reason string
	}{
		{`SELECT (SELECT a, b FROM t1) FROM t2`,
			"a scalar subquery returning more than one result column is an error, not a guess"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			mustError(t, p, c.sql, c.reason)
		})
	}
}
