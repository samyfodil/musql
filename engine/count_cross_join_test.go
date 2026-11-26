package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCountCrossJoinIsTheProduct: #394. count(*) over a cross join whose WHERE
// terms each read one table (named, or the one that declares the column) is the product of the tables' counts; each query
// answers as count(1), which the rewrite leaves to the nested loop. The
// shapes it must refuse keep the loop.
func TestCountCrossJoinIsTheProduct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	buildDB(t, path,
		`CREATE TABLE a(x INTEGER, s TEXT COLLATE NOCASE)`,
		`INSERT INTO a SELECT value, char(65 + value % 26) FROM generate_series(1, 300)`,
		`INSERT INTO a VALUES (NULL, NULL), (7.5, 'z')`,
		`CREATE TABLE b(y, k INTEGER PRIMARY KEY)`,
		`INSERT INTO b SELECT value % 40, value FROM generate_series(1, 200)`,
		`CREATE TABLE e(z)`,
		`CREATE TABLE w(p TEXT PRIMARY KEY, q) WITHOUT ROWID`,
		`INSERT INTO w SELECT 'k' || value, value FROM generate_series(1, 50)`,
		`CREATE VIEW v AS SELECT x FROM a`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	served := []string{
		`SELECT count(*) FROM a, b`,
		`SELECT count(*) FROM a, b WHERE a.x > 150`,
		`SELECT count(*) FROM a, b WHERE a.x > 150 AND b.y < 20`,
		`SELECT count(*) AS n FROM a CROSS JOIN b JOIN w WHERE b.y BETWEEN 3 AND 9 AND w.q % 7 = 1 AND a.s = 'c'`,
		`SELECT count(*) FROM a x, a y WHERE (x.x IS NULL OR x.x = 7.5) AND y.s COLLATE BINARY = 'C'`,
		`SELECT count(*) FROM a, e WHERE a.x > 0`,
		`SELECT count(*) FROM e, a`,
		`SELECT count(*) FROM a, b WHERE 1 = 0`,
		`SELECT count(*) FROM a, b WHERE b.rowid IN (1, 5, 9, NULL) AND NOT a.x < 100 AND CAST(a.x AS TEXT) > '2'`,
		`SELECT count(*) FROM generate_series(1, 1000) g, generate_series(1, 1000) h WHERE g.value > 500 AND h.value < 500`,
		`SELECT count(*) FROM generate_series(5, 1) g, b`,
		`SELECT count(*) FROM a, b WHERE a.x > ? AND b.y < ?`,
		`SELECT count(*) FROM a, b, generate_series(1, 9) g WHERE x > 150 AND y < 20 AND k % 3 = 0 AND value > 4`,
	}
	refused := []string{
		`SELECT count(*) FROM a x, a y WHERE s = 'c'`,
		`SELECT count(*) FROM a, b WHERE rowid > 3`,
		`SELECT count(*) FROM generate_series(1, 9) g, generate_series(1, 9) h WHERE value > 3`,
		`SELECT count(*) FROM a, b WHERE x = y`,
		`SELECT count(*) FROM a, b WHERE a.x = b.y`,
		`SELECT count(*) FROM a x, a y WHERE x.x IS NULL OR x.x = 7.5 AND y.s = 'C'`,
		`SELECT count(*) FROM a, b WHERE abs(a.x) > 150`,
		`SELECT count(*) FROM a, b WHERE a.s LIKE 'c%'`,
		`SELECT count(*) FROM a, b WHERE a.s || 'x' = 'cx'`,
		`SELECT count(*) FROM a, b WHERE a.x IN (SELECT y FROM b)`,
		`SELECT count(*) FROM a LEFT JOIN b`,
		`SELECT count(*) FROM a JOIN b ON a.x = b.y`,
		`SELECT count(*) FROM a, v`,
		`SELECT count(*) FROM a, (SELECT y FROM b) s`,
		`SELECT count(*) FROM a, a`,
		`SELECT count(*), 1 FROM a, b`,
		`SELECT count(*) FROM a, b LIMIT 0`,
		`SELECT count(*) FROM a, b GROUP BY a.x`,
		`SELECT count(*) FROM a, b WHERE a.nope > 1`,
		`SELECT count(*) FROM a, b WHERE b.y > 1 AND a.nope > 1`,
		`SELECT count(*) FROM a, generate_series(1, a.x) g`,
		`SELECT count(*) FROM a, json_each('[1,2]') j`,
	}
	for _, q := range append(append([]string{}, served...), refused...) {
		stmt, err := parseSelectStatement(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		isServed := false
		for _, s := range served {
			isServed = isServed || s == q
		}
		if got := p.countCrossJoin(stmt) != nil; got != isServed {
			t.Errorf("%s: rewritten %v, want %v", q, got, isServed)
		}
		args := []Value{}
		if strings.Contains(q, "?") {
			args = []Value{{Typ: Int, I: 100}, {Typ: Int, I: 30}}
		}
		_, got, gerr := p.QueryArgs(q, args)
		_, want, werr := p.QueryArgs(strings.Replace(q, "count(*)", "count(1)", 1), args)
		if fmt.Sprint(got, gerr) != fmt.Sprint(want, werr) {
			t.Errorf("%s: %v %v, want %v %v", q, got, gerr, want, werr)
		}
	}
}
