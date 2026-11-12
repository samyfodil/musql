package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSeriesInSubqueryReadingEnclosingRow: generate_series consumes an IN
// whose subquery has its own FROM and reads an enclosing query's row, running
// once for each of the values the subquery gives for that row, so a text value
// the series reads as a number matches as in C SQLite (#159). The answers are
// C 3.53's.
func TestSeriesInSubqueryReadingEnclosingRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE t(x)`,
		`INSERT INTO t VALUES('5'),(3),('x'),(NULL)`,
		`CREATE TABLE u(k, y)`,
		`INSERT INTO u VALUES('5','5'),('5','7'),(3,'2.0'),(3,'3'),(3,NULL),('x','1e1'),('x','1E1')`)
	each := func(inner string) string {
		return `SELECT group_concat(x || ':' || coalesce((` + inner + `), '-'), ' ') FROM t`
	}
	for _, tc := range []struct{ q, want string }{
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x)`), "5:5,7 3:2,3 x:10,10"},
		{each(`SELECT group_concat(value) FROM generate_series(1,12) WHERE value IN (SELECT y COLLATE nocase FROM u WHERE u.k = t.x)`), "5:5,7 3:2,3 x:10"},
		{each(`SELECT group_concat(value) FROM generate_series WHERE stop = 8 AND start IN (SELECT y FROM u WHERE u.k = t.x)`), "5:5,6,7,8,7,8 3:2,3,4,5,6,7,8,3,4,5,6,7,8 x:-"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT max(y) FROM u WHERE u.k = t.x)`), "5:7 3:3 x:10"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x UNION SELECT '9')`), "5:5,7,9 3:2,3,9 x:10,10,9"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x ORDER BY y DESC LIMIT 1)`), "5:7 3:3 x:10"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) s WHERE s.value IN (SELECT y FROM u WHERE u.k = t.x) AND s.value > 2`), "5:5,7 3:3 x:10,10"},
		{each(`SELECT (SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x))`), "5:5,7 3:2,3 x:10,10"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x) AND value IN (SELECT y FROM u WHERE u.k = '5')`), "5:- 3:- x:-"},
		{each(`SELECT group_concat(value) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x) OR value = 1`), "5:1 3:1 x:1"},
		// Reading this statement's own row as well: tested as written.
		{each(`SELECT group_concat(value) FROM generate_series(1,10) s WHERE value IN (SELECT y FROM u WHERE u.k = t.x AND u.y <> s.value + 0)`), "5:- 3:- x:-"},
		// The series below the outermost loop.
		{each(`SELECT sum(value) FROM (SELECT 1 AS one UNION ALL SELECT 2) z CROSS JOIN generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x)`), "5:24 3:10 x:40"},
		{each(`SELECT sum(value) FROM u AS w CROSS JOIN generate_series(1,10) WHERE w.k = 3 AND value IN (SELECT y FROM u WHERE u.k = t.x)`), "5:36 3:15 x:60"},
	} {
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		_, rows, err := n.Query(tc.q, nil)
		n.Discard()
		if err != nil {
			t.Errorf("%s: %v", tc.q, err)
			continue
		}
		if got := string(rows[0][0].S); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.q, got, tc.want)
		}
	}
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	_, rows, err := n.Query(`SELECT x, (SELECT count(*) FROM generate_series(1,10) WHERE value IN (SELECT y FROM u WHERE u.k = t.x)) FROM t ORDER BY 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(typedRows(rows)); got != `[0:null|1:0| 1:3|1:2| 3:"5"|1:2| 3:"x"|1:2|]` {
		t.Errorf("counts %s", got)
	}
}
