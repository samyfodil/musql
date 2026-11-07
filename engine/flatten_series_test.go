package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlattenedSeriesMatchesATable runs each shape over a subquery or CTE of
// generate_series(1,20), which flattens into the series (r41SeriesBase), and
// over the same subquery of a table holding the series' values: the answers,
// and which shapes are errors, must agree. A hidden column, a rowid and a
// column of a sibling FROM item stay out of reach through the derived table.
func TestFlattenedSeriesMatchesATable(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "series.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE s(value INTEGER)",
		"INSERT INTO s SELECT value FROM generate_series(1,20)",
		"CREATE TABLE o(x INTEGER)",
		"INSERT INTO o VALUES(3),(7)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(s, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	shapes := []string{
		"SELECT count(*) FROM (SELECT value FROM SRC)",
		"SELECT sum(value) FROM (SELECT value FROM SRC) WHERE value > 15",
		"SELECT * FROM (SELECT * FROM SRC) WHERE value < 4",
		"SELECT d.value FROM (SELECT value FROM SRC) d WHERE d.value BETWEEN 5 AND 7",
		"WITH t AS (SELECT value FROM SRC) SELECT count(*) FROM t",
		"WITH t AS (SELECT value FROM SRC) SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM t t1 WHERE t1.value = t.value*2)",
		"SELECT value FROM (SELECT value FROM SRC WHERE value % 3 = 0) ORDER BY value DESC",
		"SELECT value FROM (SELECT value FROM SRC ORDER BY value DESC LIMIT 3)",
		"SELECT v FROM (SELECT value*2 AS v FROM SRC) WHERE v > 36",
		"SELECT o.x, d.value FROM o, (SELECT value FROM SRC) d WHERE d.value = o.x",
		"SELECT count(*) FROM o LEFT JOIN (SELECT value FROM SRC) d ON d.value = o.x",
		"SELECT start FROM (SELECT value FROM SRC)",
		"SELECT d.stop FROM (SELECT value FROM SRC) d",
		"SELECT rowid FROM (SELECT value FROM SRC)",
	}
	for _, shape := range shapes {
		series := p.flattenProbe(strings.ReplaceAll(shape, "SRC", "generate_series(1,20)"))
		table := p.flattenProbe(strings.ReplaceAll(shape, "SRC", "s"))
		if series != table {
			t.Errorf("%s\n  series: %s\n  table:  %s", shape, series, table)
		}
	}
	// A call whose argument reads a sibling's column is not flattened into a
	// lateral join: through a derived table it cannot see o at all.
	if _, _, err := p.Query("SELECT count(*) FROM o, (SELECT value FROM generate_series(1, o.x))"); err == nil {
		t.Error("a derived table's generate_series read the FROM item beside it")
	}
}

// flattenProbe is a query's rows, or "error" for any error.
func (p *ReadOnlyPager) flattenProbe(q string) string {
	_, rows, err := p.Query(q)
	if err != nil {
		return "error"
	}
	return fmt.Sprint(rows)
}
