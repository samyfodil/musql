package engine_test

import (
	"github.com/samyfodil/musql/engine"

	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildGroupTestDB creates test tables: sales with grouped rows and nulls, and empty_sales.
func buildGroupTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "group.sqlite")
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

	exec(`CREATE TABLE sales (id INTEGER PRIMARY KEY, category TEXT, region TEXT, amount REAL, qty INTEGER)`)
	type row struct {
		category any
		region   any
		amount   any
		qty      any
	}
	rows := []row{
		{"fruit", "east", 10.0, 5},
		{"fruit", "east", 20.5, 3},
		{"fruit", "west", 7.25, 0},
		{nil, "east", 100.0, 1},
		{"veg", "west", -5.0, 2},
		{nil, "west", 50.0, -1},
		{"veg", "east", 15.0, 4},
		{"fruit", "west", 2.75, 10},
		{nil, "east", 1.0, 0},
		{"veg", "west", 8.0, 6},
	}
	for _, r := range rows {
		exec(`INSERT INTO sales (category, region, amount, qty) VALUES (?,?,?,?)`, r.category, r.region, r.amount, r.qty)
	}

	exec(`CREATE TABLE empty_sales (id INTEGER PRIMARY KEY, category TEXT, region TEXT, amount REAL, qty INTEGER)`)

	return path, db
}

// rowLess orders two output rows for comparison when the query has no ORDER BY.
func rowLess(a, b []engine.Value) bool {
	for i := range a {
		if c := engine.CompareValuesForTest(a[i], b[i]); c != 0 {
			return c < 0
		}
	}
	return false
}

func sortedRows(rows [][]engine.Value) [][]engine.Value {
	out := make([][]engine.Value, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool { return rowLess(out[i], out[j]) })
	return out
}

// mustMatchGroup runs sqlText through both engines and compares results, with order-insensitive comparison unless ORDER BY is present.
func mustMatchGroup(t *testing.T, p *engine.ReadOnlyPager, db *sql.DB, sqlText string) {
	t.Helper()
	gotCols, gotRows, err := p.Query(sqlText)
	if err != nil {
		t.Fatalf("engine Query(%q): %v", sqlText, err)
	}
	wantCols, wantRows := refRowsFor(t, db, sqlText)

	if len(gotCols) != len(wantCols) {
		t.Fatalf("%q: got %d columns %v, want %d columns %v", sqlText, len(gotCols), gotCols, len(wantCols), wantCols)
	}
	for i := range gotCols {
		if gotCols[i] != wantCols[i] {
			t.Errorf("%q: column %d name = %q, want %q", sqlText, i, gotCols[i], wantCols[i])
		}
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("%q: got %d rows, want %d\n  engine: %v\n  ref:    %v", sqlText, len(gotRows), len(wantRows), gotRows, wantRows)
	}

	orderSensitive := strings.Contains(strings.ToLower(sqlText), "order by")
	g, w := gotRows, wantRows
	if !orderSensitive {
		g, w = sortedRows(gotRows), sortedRows(wantRows)
	}

	for r := range g {
		if len(g[r]) != len(w[r]) {
			t.Fatalf("%q: row %d: got %d values, want %d", sqlText, r, len(g[r]), len(w[r]))
		}
		for c := range g[r] {
			if !valuesEqualAllowingRealStorageOptimization(g[r][c], w[r][c]) {
				t.Errorf("%q: row %d col %d (%s): got %+v, want %+v",
					sqlText, r, c, gotCols[c], g[r][c], w[r][c])
			}
		}
	}
}

// TestGroupByMatchesReference compares GROUP BY/HAVING execution against the reference engine.
func TestGroupByMatchesReference(t *testing.T) {
	path, db := buildGroupTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	queries := []string{
		// Group by one column with NULLs.
		`SELECT category, count(*) FROM sales GROUP BY category`,
		`SELECT category, count(*) FROM sales GROUP BY category ORDER BY category`,

		// Bare columns in GROUP BY queries.
		`SELECT category, region, count(*) FROM sales GROUP BY category`,
		`SELECT * FROM sales GROUP BY category`,
		`SELECT *, count(*) FROM sales GROUP BY category`,
		`SELECT region FROM sales GROUP BY category`,
		`SELECT category, region, max(amount) FROM sales GROUP BY category`,
		`SELECT category, region, min(amount) FROM sales GROUP BY category`,
		`SELECT category, region, min(amount), max(amount) FROM sales GROUP BY category`,
		`SELECT category, region, max(amount), min(amount) FROM sales GROUP BY category`,
		`SELECT * FROM sales GROUP BY category HAVING count(*) > 1`,

		// Every supported aggregate, per group.
		`SELECT category, count(*), count(amount), sum(amount), total(amount), avg(amount), min(amount), max(amount) FROM sales GROUP BY category`,
		`SELECT category, group_concat(region) FROM sales GROUP BY category`,
		`SELECT category, group_concat(region, '|') FROM sales GROUP BY category`,

		// Multiple grouping columns.
		`SELECT category, region, count(*) FROM sales GROUP BY category, region`,
		`SELECT category, region, sum(amount) FROM sales GROUP BY category, region ORDER BY category, region`,

		// HAVING narrows groups: count/sum comparisons, and a predicate on the
		// group key itself (functionally a grouped bare column, allowed).
		`SELECT category, count(*) FROM sales GROUP BY category HAVING count(*) > 1`,
		`SELECT category, sum(amount) FROM sales GROUP BY category HAVING sum(amount) > 20`,
		`SELECT category, count(*) FROM sales GROUP BY category HAVING category IS NOT NULL`,
		`SELECT category, count(*) FROM sales GROUP BY category HAVING category IS NULL`,

		// WHERE applies before grouping.
		`SELECT category, count(*) FROM sales WHERE qty > 0 GROUP BY category`,
		`SELECT category, sum(amount) FROM sales WHERE region = 'west' GROUP BY category`,

		// GROUP BY + ORDER BY + LIMIT/OFFSET.
		`SELECT category, count(*) AS n FROM sales GROUP BY category ORDER BY n DESC, category`,
		`SELECT category, count(*) AS n FROM sales GROUP BY category ORDER BY n DESC LIMIT 1`,
		`SELECT category, count(*) AS n FROM sales GROUP BY category ORDER BY n DESC, category LIMIT 2 OFFSET 1`,

		// Bare grouped column with no aggregate in the select-list at all.
		`SELECT category FROM sales GROUP BY category`,
		`SELECT region FROM sales GROUP BY region`,

		// GROUP BY ordinal referencing an output column by position.
		`SELECT category, count(*) FROM sales GROUP BY 1`,

		// Aggregates with arithmetic operators.
		`SELECT category, count(*) + 1 FROM sales GROUP BY category`,
		`SELECT category, sum(amount) - count(*) FROM sales GROUP BY category`,
		`SELECT category, count(*) FROM sales GROUP BY category HAVING count(*) + 1 > 2`,

		// Empty table: zero groups, zero rows.
		`SELECT category, count(*) FROM empty_sales GROUP BY category`,
		`SELECT category, count(*) FROM empty_sales GROUP BY category HAVING count(*) > 0`,

		// WHERE that matches nothing, still with GROUP BY: zero rows (not one,
		// unlike the GROUP-BY-less aggregate path).
		`SELECT category, count(*) FROM sales WHERE qty > 100000 GROUP BY category`,
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			mustMatchGroup(t, p, db, q)
		})
	}
}

// TestGroupByEmptyYieldsZeroRows checks that GROUP BY over empty input returns zero rows, unlike a bare aggregate.
func TestGroupByEmptyYieldsZeroRows(t *testing.T) {
	path, err := func() (string, error) {
		p := filepath.Join(t.TempDir(), "empty.sqlite")
		db, err := sql.Open("sqlite", "file:"+p)
		if err != nil {
			return "", err
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE t (g TEXT, x INTEGER)`); err != nil {
			return "", err
		}
		return p, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	_, rows, err := p.Query(`SELECT g, count(*), sum(x) FROM t GROUP BY g`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("GROUP BY over empty table: got %d rows, want 0: %v", len(rows), rows)
	}

	// Contrast: the same aggregates WITHOUT GROUP BY yield exactly one row.
	_, rows2, err := p.Query(`SELECT count(*), sum(x) FROM t`)
	if err != nil {
		t.Fatalf("Query (no GROUP BY): %v", err)
	}
	if len(rows2) != 1 {
		t.Errorf("bare aggregate over empty table: got %d rows, want 1: %v", len(rows2), rows2)
	}
}

// TestGroupByKnownUnsupported documents GROUP BY/HAVING grammar this package rejects, ensuring errors not wrong answers.
func TestGroupByKnownUnsupported(t *testing.T) {
	path, _ := buildGroupTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	cases := []struct {
		sql    string
		reason string
	}{
		{`SELECT category, sum(count(amount)) FROM sales GROUP BY category`,
			"an aggregate nested inside another aggregate's argument is unsupported"},
		{`SELECT category, count(*) FROM sales GROUP BY 100`,
			"GROUP BY ordinal out of range (only 2 output columns)"},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			mustError(t, p, c.sql, c.reason)
		})
	}
}

// TestGroupBareColumnInHaving checks that bare columns in HAVING and ORDER BY answer correctly.
func TestGroupBareColumnInHaving(t *testing.T) {
	path, _ := buildGroupTestDB(t)
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	for _, sql := range []string{
		`SELECT category, count(*) FROM sales GROUP BY category HAVING region = 'east'`,
		`SELECT category FROM sales GROUP BY category ORDER BY region`,
	} {
		if _, _, err := p.Query(sql); err != nil {
			t.Errorf("%s: %v (a bare column in HAVING/ORDER BY must answer, not decline)", sql, err)
		}
	}
}
