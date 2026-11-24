package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestJoinCorrelatedTermTestedAtItsLevel: a WHERE term whose subquery reads a
// row of this FROM clause is tested at the level that binds the items it
// names, inside the subquery as well as outside, as where.c places it by
// prereqAll (exprSelectUsage), after that level's other terms. Testing it only
// in the innermost loop ran it once per joined row rather than once per row of
// its own item, and missed the error C raises for an outer row no inner row
// joins. The answers are C SQLite's.
func TestJoinCorrelatedTermTestedAtItsLevel(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "v.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE a(x, p)`,
		`CREATE TABLE b(y, q)`,
		`INSERT INTO a VALUES(1, 10), (2, 20), (3, 30), (4, 40)`,
		`INSERT INTO b VALUES(1, 5), (2, 6), (2, 7), (5, 8)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct{ q, want string }{
		{`SELECT * FROM a CROSS JOIN b WHERE b.y = a.x + 10 AND (SELECT abs(-9223372036854775807-1) WHERE a.x = 1)`, "integer overflow"},
		{`SELECT a.x, b.q FROM a CROSS JOIN b WHERE b.y = a.x AND a.p > (SELECT avg(p) FROM a j WHERE j.x <= a.x) - 6 ORDER BY 1, 2`,
			"[[1 5] [2 6] [2 7]]"},
		{`SELECT a.x, b.q FROM a CROSS JOIN b WHERE b.y = a.x AND (SELECT count(*) FROM b k WHERE k.y = a.x AND k.q >= b.q) = 1 ORDER BY 1, 2`,
			"[[1 5] [2 7]]"},
		{`SELECT a.x, b.q FROM a LEFT JOIN b ON b.y = a.x WHERE (SELECT b.q IS NULL OR b.q > 6) ORDER BY 1, 2`,
			"[[2 7] [3 NULL] [4 NULL]]"},
		// A row value is a node the item walk does not enter: the term waits
		// for the innermost level, where b is positioned (rowvalue.test).
		{`SELECT a.x, b.q FROM a LEFT JOIN b ON b.y = NULL WHERE (b.y, b.q) == (SELECT 1, 5 + a.x - a.x)`, "[]"},
		{`SELECT a.x, b.q FROM a CROSS JOIN b WHERE (b.y, b.q) == (SELECT a.x, 5 + a.x - 1) ORDER BY 1`, "[[1 5] [2 6]]"},
	} {
		_, rows, err := p.QueryArgs(c.q, nil)
		var got string
		if err != nil {
			got = err.Error()
		} else {
			var out []string
			for _, row := range rows {
				var vs []string
				for _, v := range row {
					if v.Typ == Null {
						vs = append(vs, "NULL")
					} else {
						vs = append(vs, fmt.Sprint(v.I))
					}
				}
				out = append(out, "["+strings.Join(vs, " ")+"]")
			}
			got = "[" + strings.Join(out, " ") + "]"
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s:\n%s, want %s", c.q, got, c.want)
		}
	}
}
