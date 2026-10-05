package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"testing"
)

// buildDistinctTestDB creates test tables with duplicates, NULLs, and edge cases
// for testing DISTINCT in rows and aggregates.
func buildDistinctTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "distinct.sqlite")
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

	exec(`CREATE TABLE dup_rows (a INTEGER, b TEXT, c REAL)`)
	type dupRow struct{ a, b, c any }
	for _, r := range []dupRow{
		{1, "x", 1.5},
		{1, "x", 1.5}, // exact duplicate
		{2, "y", nil},
		{2, "y", nil}, // duplicate with a NULL column
		{nil, nil, nil},
		{nil, nil, nil}, // all-NULL duplicate: must collapse to one row
		{3, nil, 2.5},
		{nil, "z", 2.5}, // partially NULL, distinct from the row above
		{1, "x", 1.5},   // another duplicate of the first row
		{4, "w", 3.5},
	} {
		exec(`INSERT INTO dup_rows (a,b,c) VALUES (?,?,?)`, r.a, r.b, r.c)
	}

	exec(`CREATE TABLE empty_dup_rows (a INTEGER, b TEXT, c REAL)`)

	exec(`CREATE TABLE dup_nums (x INTEGER, y REAL, z TEXT)`)
	type numRow struct{ x, y, z any }
	for _, r := range []numRow{
		{1, 1.5, "a"},
		{1, 1.5, "a"}, // duplicate
		{2, 2.5, "b"},
		{nil, nil, nil},
		{nil, nil, nil}, // duplicate NULL row
		{3, 1.5, "a"},   // distinct x, duplicate y and z
		{-1, -0.5, "Z"},
		{2, 2.5, "b"}, // duplicate of row 3
	} {
		exec(`INSERT INTO dup_nums (x,y,z) VALUES (?,?,?)`, r.x, r.y, r.z)
	}

	exec(`CREATE TABLE empty_dup_nums (x INTEGER, y REAL, z TEXT)`)

	exec(`CREATE TABLE allnull_dup_nums (x INTEGER, y REAL, z TEXT)`)
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO allnull_dup_nums (x,y,z) VALUES (NULL,NULL,NULL)`)
	}

	exec(`CREATE TABLE cat_vals (cat TEXT, val INTEGER)`)
	type catRow struct {
		cat any
		val any
	}
	for _, r := range []catRow{
		{"a", 1},
		{"a", 1}, // duplicate within category a
		{"a", 2},
		{"a", nil},
		{"a", nil}, // duplicate NULL within category a
		{"b", 5},
		{"b", 5}, // duplicate within category b
		{"b", nil},
	} {
		exec(`INSERT INTO cat_vals (cat,val) VALUES (?,?)`, r.cat, r.val)
	}

	exec(`CREATE TABLE ab (a INTEGER, b INTEGER)`)
	for _, r := range [][2]any{{1, 10}, {1, 20}, {2, 10}, {2, 20}, {1, 10}} {
		exec(`INSERT INTO ab (a,b) VALUES (?,?)`, r[0], r[1])
	}

	return path, db
}

// TestSelectDistinct tests SELECT DISTINCT with various row patterns,
// ORDER BY, LIMIT, and NULL handling.
func TestSelectDistinct(t *testing.T) {
	path, db := buildDistinctTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Multi-column and single-column DISTINCT over duplicates, an
		// all-NULL duplicate row, and partially-NULL rows.
		`SELECT DISTINCT a, b, c FROM dup_rows`,
		`SELECT DISTINCT a FROM dup_rows`,
		`SELECT DISTINCT b FROM dup_rows`,
		`SELECT DISTINCT c FROM dup_rows`,
		`SELECT DISTINCT a, b FROM dup_rows`,
		`SELECT DISTINCT * FROM dup_rows`,

		// ALL is the no-op default -- identical to no modifier at all.
		`SELECT ALL a, b, c FROM dup_rows`,

		// DISTINCT + ORDER BY: dedupe first, then sort the deduped set.
		`SELECT DISTINCT a, b, c FROM dup_rows ORDER BY a, b, c`,
		`SELECT DISTINCT a, b, c FROM dup_rows ORDER BY a DESC, b ASC`,
		`SELECT DISTINCT a FROM dup_rows ORDER BY a ASC`,

		// DISTINCT + LIMIT/OFFSET: limit applies to the deduped set.
		`SELECT DISTINCT a, b, c FROM dup_rows LIMIT 3`,
		`SELECT DISTINCT a, b, c FROM dup_rows LIMIT 2 OFFSET 1`,
		`SELECT DISTINCT a, b, c FROM dup_rows ORDER BY a, b, c LIMIT 2`,

		// WHERE narrows the input before DISTINCT collapses it.
		`SELECT DISTINCT a FROM dup_rows WHERE a IS NOT NULL`,
		`SELECT DISTINCT b FROM dup_rows WHERE a > 1`,

		// WHERE matches nothing: DISTINCT over zero surviving rows.
		`SELECT DISTINCT a FROM dup_rows WHERE a > 1000000`,

		// Empty table (zero rows, not just zero surviving WHERE matches).
		`SELECT DISTINCT a, b, c FROM empty_dup_rows`,
		`SELECT DISTINCT a FROM empty_dup_rows`,

		// Aliased and computed-expression DISTINCT columns.
		`SELECT DISTINCT a AS x, b AS y FROM dup_rows`,
		`SELECT DISTINCT a + 1 FROM dup_rows`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestSelectDistinctGroupBy tests DISTINCT combined with GROUP BY.
func TestSelectDistinctGroupBy(t *testing.T) {
	path, db := buildDistinctTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Grouping by (a,b) but selecting only DISTINCT a: rows for (1,10)
		// and (1,20) must collapse into a single a=1 output row.
		`SELECT DISTINCT a FROM ab GROUP BY a, b`,
		// Selecting DISTINCT a AND count(*) (which differs per group) keeps
		// the groups apart, since the full projected row isn't a duplicate.
		`SELECT DISTINCT a, count(*) FROM ab GROUP BY a, b`,
		// DISTINCT at the top level plus DISTINCT inside the aggregate,
		// together.
		`SELECT DISTINCT a, avg(DISTINCT b) FROM ab GROUP BY a`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestAggregateDistinct tests DISTINCT in count/sum/avg/min/max/total/group_concat
// aggregates over duplicates, NULLs, and edge cases.
func TestAggregateDistinct(t *testing.T) {
	path, db := buildDistinctTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Basic DISTINCT aggregates over duplicates (dup_nums: x INTEGER,
		// y REAL, z TEXT, with a duplicated NULL row and duplicated
		// non-NULL values).
		`SELECT count(DISTINCT x) FROM dup_nums`,
		`SELECT count(DISTINCT y) FROM dup_nums`,
		`SELECT count(DISTINCT z) FROM dup_nums`,
		`SELECT sum(DISTINCT x) FROM dup_nums`,
		`SELECT sum(DISTINCT y) FROM dup_nums`,
		`SELECT total(DISTINCT x) FROM dup_nums`,
		`SELECT avg(DISTINCT x) FROM dup_nums`,
		`SELECT avg(DISTINCT y) FROM dup_nums`,
		`SELECT min(DISTINCT x) FROM dup_nums`,
		`SELECT max(DISTINCT x) FROM dup_nums`,
		`SELECT min(DISTINCT z) FROM dup_nums`,
		`SELECT max(DISTINCT z) FROM dup_nums`,
		`SELECT group_concat(DISTINCT z) FROM dup_nums`,
		`SELECT group_concat(DISTINCT x) FROM dup_nums`,

		// Case-insensitive DISTINCT keyword.
		`SELECT COUNT(DISTINCT x) FROM dup_nums`,
		`SELECT Sum(Distinct x) FROM dup_nums`,

		// Multiple DISTINCT aggregates (each with its own independent dedup
		// set) plus a plain (non-DISTINCT) aggregate, side by side.
		`SELECT count(DISTINCT x), count(x), count(*) FROM dup_nums`,
		`SELECT count(DISTINCT x), sum(DISTINCT x), avg(DISTINCT x), min(DISTINCT x), max(DISTINCT x) FROM dup_nums`,

		// WHERE narrows the aggregated set before DISTINCT dedup.
		`SELECT count(DISTINCT x) FROM dup_nums WHERE x IS NOT NULL`,
		`SELECT sum(DISTINCT x) FROM dup_nums WHERE x > 0`,

		// WHERE matches nothing: DISTINCT aggregate over zero surviving rows.
		`SELECT count(DISTINCT x), sum(DISTINCT x), avg(DISTINCT x), min(DISTINCT x), max(DISTINCT x), total(DISTINCT x), group_concat(DISTINCT z) FROM dup_nums WHERE x > 1000000`,

		// Empty table (zero rows, not just zero surviving WHERE matches).
		`SELECT count(DISTINCT x) FROM empty_dup_nums`,
		`SELECT sum(DISTINCT x) FROM empty_dup_nums`,
		`SELECT avg(DISTINCT x) FROM empty_dup_nums`,
		`SELECT min(DISTINCT x) FROM empty_dup_nums`,
		`SELECT max(DISTINCT x) FROM empty_dup_nums`,
		`SELECT total(DISTINCT x) FROM empty_dup_nums`,
		`SELECT group_concat(DISTINCT x) FROM empty_dup_nums`,

		// All-NULL column (N>0 rows, but every value NULL): 0 distinct
		// non-NULL contributions.
		`SELECT count(DISTINCT x) FROM allnull_dup_nums`,
		`SELECT sum(DISTINCT x) FROM allnull_dup_nums`,
		`SELECT avg(DISTINCT x) FROM allnull_dup_nums`,
		`SELECT min(DISTINCT x) FROM allnull_dup_nums`,
		`SELECT total(DISTINCT x) FROM allnull_dup_nums`,
		`SELECT group_concat(DISTINCT x) FROM allnull_dup_nums`,

		// DISTINCT inside an aggregate combined with GROUP BY (sql_group.go's
		// per-group accumulators, each with their own independent dedup set
		// scoped to that group).
		`SELECT cat, count(DISTINCT val) FROM cat_vals GROUP BY cat`,
		`SELECT cat, sum(DISTINCT val) FROM cat_vals GROUP BY cat`,
		`SELECT cat, avg(DISTINCT val) FROM cat_vals GROUP BY cat`,
		`SELECT cat, group_concat(DISTINCT val) FROM cat_vals GROUP BY cat`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestAggregateDistinctNumericStorageClassCollapse verifies storage class
// handling in DISTINCT aggregates.
func TestAggregateDistinctNumericStorageClassCollapse(t *testing.T) {
	path, db := buildQueryTestDBForDistinctStorageClass(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		`SELECT count(DISTINCT x) FROM storage_mix`,
		`SELECT sum(DISTINCT x) FROM storage_mix`,
		`SELECT group_concat(DISTINCT x) FROM storage_mix`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// buildQueryTestDBForDistinctStorageClass creates a test table for storage class tests.
func buildQueryTestDBForDistinctStorageClass(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "distinct_storage.sqlite")
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
	exec(`CREATE TABLE storage_mix (x)`)
	for _, v := range []any{1, 1.0, 2, "2", "5abc", 2.0} {
		exec(`INSERT INTO storage_mix (x) VALUES (?)`, v)
	}
	return path, db
}

// TestAggregateDistinctKnownUnsupported tests unsupported DISTINCT aggregate forms.
func TestAggregateDistinctKnownUnsupported(t *testing.T) {
	path, db := buildDistinctTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []struct {
		sql    string
		reason string
	}{
		{`SELECT group_concat(DISTINCT z, '-') FROM dup_nums`,
			"a custom separator combined with DISTINCT is invalid SQL"},
		{`SELECT count(DISTINCT *) FROM dup_nums`,
			"count(DISTINCT *) is invalid SQL"},
		{`SELECT DISTINCT ALL a FROM dup_rows`,
			"DISTINCT and ALL are mutually exclusive"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			mustError(t, p, c.sql, c.reason)
			if !refQueryErrors(t, db, c.sql) {
				t.Errorf("reference %q: expected an error, got none -- "+
					"this package's understanding of the construct's rejection "+
					"(verified separately against real CGo SQLite) may be stale", c.sql)
			}
		})
	}
}
