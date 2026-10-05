package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"
)

// buildAggTestDB creates a set of tables exercising aggregate functions:
// nums (normal mix), empty_nums (zero rows), allnull_nums (all NULLs),
// mixed_sum (INTEGER and REAL), overflow_sum, and overflow_mixed.
func buildAggTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agg.sqlite")
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

	exec(`CREATE TABLE nums (x INTEGER, y REAL, z TEXT)`)
	type numRow struct{ x, y, z any }
	for _, r := range []numRow{
		{1, 1.5, "a"},
		{2, 2.5, "b"},
		{nil, nil, nil},
		{3, 3.5, "10"},
		{-1, -0.5, "Z"},
	} {
		exec(`INSERT INTO nums (x,y,z) VALUES (?,?,?)`, r.x, r.y, r.z)
	}

	exec(`CREATE TABLE empty_nums (x INTEGER, y REAL, z TEXT)`)

	exec(`CREATE TABLE allnull_nums (x INTEGER, y REAL, z TEXT)`)
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO allnull_nums (x,y,z) VALUES (NULL,NULL,NULL)`)
	}

	exec(`CREATE TABLE mixed_sum (x)`)
	for _, v := range []any{1, 2.5, 3, 4.5} {
		exec(`INSERT INTO mixed_sum (x) VALUES (?)`, v)
	}

	exec(`CREATE TABLE overflow_sum (x INTEGER)`)
	exec(`INSERT INTO overflow_sum (x) VALUES (9223372036854775807), (1)`)

	exec(`CREATE TABLE overflow_mixed (x)`)
	exec(`INSERT INTO overflow_mixed (x) VALUES (9223372036854775807), (1), (2.5)`)

	return path, db
}

// TestAggregateFunctions tests aggregate functions across various edge cases:
// empty tables, all-NULL columns, WHERE filters, and mixed int+real values.
func TestAggregateFunctions(t *testing.T) {
	path, db := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Basic aggregates over nums table.
		`SELECT count(*) FROM nums`,
		`SELECT count(x) FROM nums`,
		`SELECT count(y) FROM nums`,
		`SELECT count(z) FROM nums`,
		`SELECT sum(x) FROM nums`,
		`SELECT sum(y) FROM nums`,
		`SELECT total(x) FROM nums`,
		`SELECT total(y) FROM nums`,
		`SELECT avg(x) FROM nums`,
		`SELECT avg(y) FROM nums`,
		`SELECT min(x) FROM nums`,
		`SELECT max(x) FROM nums`,
		`SELECT min(y) FROM nums`,
		`SELECT max(y) FROM nums`,
		`SELECT min(z) FROM nums`,
		`SELECT max(z) FROM nums`,
		`SELECT group_concat(z) FROM nums`,
		`SELECT group_concat(x) FROM nums`,
		`SELECT group_concat(z, '-') FROM nums`,
		`SELECT group_concat(z, ':') FROM nums`,

		// Case-insensitive function names.
		`SELECT COUNT(*) FROM nums`,
		`SELECT Sum(x) FROM nums`,
		`SELECT AVG(x) FROM nums`,

		// Aliased aggregates and constant expressions.
		`SELECT count(*) AS n, sum(x) AS total_x FROM nums`,
		`SELECT count(*), 1 + 1, 'const' FROM nums`,
		`SELECT count(x), sum(x), avg(x), min(x), max(x) FROM nums`,

		// WHERE filters.
		`SELECT count(*) FROM nums WHERE x > 0`,
		`SELECT sum(x) FROM nums WHERE x > 0`,
		`SELECT avg(y) FROM nums WHERE y IS NOT NULL`,

		// WHERE matches nothing.
		`SELECT count(*), count(x), sum(x), total(x), avg(x), min(x), max(x), group_concat(z) FROM nums WHERE x > 1000000`,

		// Empty table.
		`SELECT count(*) FROM empty_nums`,
		`SELECT count(x) FROM empty_nums`,
		`SELECT sum(x) FROM empty_nums`,
		`SELECT total(x) FROM empty_nums`,
		`SELECT avg(x) FROM empty_nums`,
		`SELECT min(x) FROM empty_nums`,
		`SELECT max(x) FROM empty_nums`,
		`SELECT group_concat(x) FROM empty_nums`,

		// All-NULL column.
		`SELECT count(*) FROM allnull_nums`,
		`SELECT count(x) FROM allnull_nums`,
		`SELECT sum(x) FROM allnull_nums`,
		`SELECT total(x) FROM allnull_nums`,
		`SELECT avg(x) FROM allnull_nums`,
		`SELECT min(x) FROM allnull_nums`,
		`SELECT max(x) FROM allnull_nums`,
		`SELECT group_concat(x) FROM allnull_nums`,

		// Mixed INTEGER + REAL: sum() must be REAL.
		`SELECT sum(x) FROM mixed_sum`,
		`SELECT avg(x) FROM mixed_sum`,
		`SELECT total(x) FROM mixed_sum`,

		// Pure-INTEGER overflow: avg() and total() never error.
		`SELECT avg(x) FROM overflow_sum`,
		`SELECT total(x) FROM overflow_sum`,

		// Overflowing INTEGER pair with REAL: sum() does not error.
		`SELECT sum(x) FROM overflow_mixed`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatch(t, p, db, q)
		})
	}
}

// TestAggregateSumResultTypeMatchesReference checks that sum()/total()/avg()
// return values in the correct storage class (INTEGER vs. REAL vs. NULL).
func TestAggregateSumResultTypeMatchesReference(t *testing.T) {
	path, _ := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []struct {
		query   string
		wantTyp engine.ValueType
	}{
		{`SELECT sum(x) FROM nums`, engine.Int},           // every contributing value an exact integer
		{`SELECT sum(y) FROM nums`, engine.Float},         // REAL column
		{`SELECT sum(x) FROM mixed_sum`, engine.Float},    // one REAL contribution taints the whole sum
		{`SELECT total(x) FROM nums`, engine.Float},       // total() is always REAL
		{`SELECT avg(x) FROM nums`, engine.Float},         // avg() is always REAL
		{`SELECT sum(x) FROM empty_nums`, engine.Null},    // 0 contributing rows
		{`SELECT total(x) FROM empty_nums`, engine.Float}, // total() never NULL: 0.0
		{`SELECT avg(x) FROM empty_nums`, engine.Null},    // avg() NULL when count is 0
		{`SELECT sum(x) FROM allnull_nums`, engine.Null},  // N>0 rows but 0 non-NULL contributions
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			_, rows, err := p.Query(c.query)
			if err != nil {
				t.Fatalf("engine Query(%q): %v", c.query, err)
			}
			if len(rows) != 1 || len(rows[0]) != 1 {
				t.Fatalf("%q: got %d rows, want exactly 1 row of 1 column", c.query, len(rows))
			}
			if got := rows[0][0].Typ; got != c.wantTyp {
				t.Errorf("%q: storage class = %v, want %v (value=%+v)", c.query, got, c.wantTyp, rows[0][0])
			}
		})
	}
}

// TestAggregateLimitOffset checks that LIMIT/OFFSET apply to aggregate results.
func TestAggregateLimitOffset(t *testing.T) {
	path, db := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT count(*) FROM nums LIMIT 1`,
		`SELECT count(*) FROM nums LIMIT 100`,
	} {
		t.Run(q, func(t *testing.T) { mustMatch(t, p, db, q) })
	}

	for _, q := range []string{
		`SELECT count(*) FROM nums LIMIT 0`,
		`SELECT count(*) FROM nums LIMIT 1 OFFSET 1`,
		`SELECT count(*) FROM nums LIMIT 5 OFFSET 3`,
	} {
		t.Run(q, func(t *testing.T) {
			_, rows, err := p.Query(q)
			if err != nil {
				t.Fatalf("engine Query(%q): %v", q, err)
			}
			if len(rows) != 0 {
				t.Errorf("%q: got %d rows, want 0", q, len(rows))
			}
			wantCols, wantRows := refRowsFor(t, db, q)
			_ = wantCols
			if len(wantRows) != 0 {
				t.Fatalf("%q: reference returned %d rows, want 0 -- test's LIMIT/OFFSET assumption is wrong", q, len(wantRows))
			}
		})
	}
}

// TestAggregateSumIntegerOverflowMatchesReference checks that sum() over
// pure-INTEGER input overflowing int64 produces an error.
func TestAggregateSumIntegerOverflowMatchesReference(t *testing.T) {
	path, db := buildAggTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	const q = `SELECT sum(x) FROM overflow_sum`

	if _, _, err := p.Query(q); err == nil {
		t.Errorf("engine %q: expected an integer-overflow error, got none", q)
	}

	if !refQueryErrors(t, db, q) {
		t.Errorf("reference %q: expected an integer-overflow error, got none -- "+
			"this package's understanding of sum()'s overflow behavior (verified "+
			"separately against real CGo SQLite) may be stale", q)
	}
}

// refQueryErrors reports whether query q fails.
func refQueryErrors(t *testing.T, db *sql.DB, q string) bool {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err() != nil
}
