package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// eagerAggSchema is shared in spirit with compat-harness's
// TestEagerAggregateAsC: facts fa, fb, fc over a dim d with duplicate keys,
// NULL keys and values, a NOCASE column whose order differs from BINARY's, a
// TEXT-keyed dim, an untyped and a NOCASE key, indexed copies of the facts,
// and a dim too large for the facts to fan out.
var eagerAggSchema = []string{
	`CREATE TABLE fa(k INTEGER, x INTEGER, s TEXT COLLATE NOCASE, r REAL)`,
	`INSERT INTO fa SELECT CASE WHEN value % 97 = 0 THEN NULL ELSE value % 53 END, CASE WHEN value % 7 = 0 THEN NULL ELSE value % 11 END, CASE WHEN value % 26 < 13 THEN char(97 + value % 26) ELSE char(65 + value % 26) END, (value % 100) / 8.0 FROM generate_series(1, 2000)`,
	`CREATE TABLE fb(k INT, y REAL, t TEXT)`,
	`INSERT INTO fb SELECT value % 47, (value * 7 % 100) / 3.0, nullif(char(97 + value % 5), 'c') FROM generate_series(1, 2000)`,
	`CREATE TABLE fc(k INTEGER, z INTEGER)`,
	`INSERT INTO fc SELECT value % 31, CASE WHEN value % 5 = 0 THEN NULL ELSE value % 13 END FROM generate_series(1, 1500)`,
	`CREATE TABLE d(k INTEGER, g TEXT, h INT)`,
	`INSERT INTO d SELECT value % 60, 'g' || (value % 7), value % 5 FROM generate_series(1, 66)`,
	`CREATE TABLE dt(k TEXT, g TEXT)`,
	`INSERT INTO dt SELECT value % 60, 'g' || (value % 4) FROM generate_series(1, 60)`,
	`CREATE TABLE fu(k, v)`,
	`INSERT INTO fu SELECT CASE WHEN value % 9 = 0 THEN (value % 53) * 1.0 ELSE value % 53 END, value FROM generate_series(1, 2000)`,
	`CREATE TABLE fu2(k, v)`,
	`INSERT INTO fu2 SELECT * FROM fu`,
	`CREATE TABLE fn(k TEXT COLLATE NOCASE, v)`,
	`INSERT INTO fn SELECT CASE WHEN value % 2 THEN 'k' ELSE 'K' END || (value % 50), value FROM generate_series(1, 2000)`,
	`CREATE TABLE dn(k TEXT, g TEXT)`,
	`INSERT INTO dn SELECT 'k' || value, 'g' || (value % 3) FROM generate_series(0, 49)`,
	`CREATE TABLE fia(k INTEGER, x INTEGER)`,
	`INSERT INTO fia SELECT k, x FROM fa`,
	`CREATE INDEX fia_k ON fia(k)`,
	`CREATE TABLE fib(k INTEGER, y REAL)`,
	`INSERT INTO fib SELECT k, y FROM fb`,
	`CREATE INDEX fib_k ON fib(k)`,
	`CREATE TABLE d2(k INTEGER, g TEXT)`,
	`INSERT INTO d2 SELECT value % 60, 'g' || (value % 3) FROM generate_series(1, 2000)`,
}

var eagerAggServed = []string{
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) AS n, count(a.x), count(b.t), max(b.y), min(b.y), max(a.s), min(a.s), max(a.r), min(d.h), count(d.h) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.h, count(*), max(a.x) FROM fa a, d, fb b WHERE a.k = d.k AND b.k = d.k AND a.x > 5 AND b.y < 30 GROUP BY d.h HAVING count(*) > 10 ORDER BY 2 DESC, 1`,
	`SELECT d.g, d.h, count(*) c FROM d JOIN fa a ON a.k = d.k JOIN fb b ON b.k = a.k GROUP BY d.g, d.h ORDER BY c DESC, d.g, d.h LIMIT 5 OFFSET 2`,
	`SELECT dt.g, count(*), max(b.y) FROM fa a JOIN dt USING (k) JOIN fb b USING (k) GROUP BY dt.g`,
	`SELECT d.g, count(*), count(c.z), max(c.z), min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) JOIN fc c USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE d.h > 2 GROUP BY d.g`,
	`SELECT upper(d.g) || '!', count(*) + 1, max(b.y) * 2 FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(b.y) > 1 AND d.g <> 'g3'`,
	`SELECT d.g, count(*) FROM fia a JOIN d USING (k) JOIN fib b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.k IS NOT NULL AND b.t IS NOT NULL GROUP BY d.g ORDER BY count(*), d.g`,
	`SELECT d.g, max(a.x), count(*) FROM d JOIN fa a ON a.k = d.k AND a.x BETWEEN 2 AND 8 JOIN fb b ON b.k = d.k AND b.t IN ('a', 'b') GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.r > 100 GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = d.h GROUP BY d.g`,
}

var eagerAggRefused = []string{
	`SELECT d.h, d.g, count(*) FROM fa JOIN d ON fa.k = d.k JOIN fb ON fb.k = fa.k GROUP BY d.g`,
	`SELECT d.g, count(*), avg(b.y) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, sum(a.x) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, total(b.y), group_concat(b.t) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(DISTINCT a.x) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FILTER (WHERE a.x > 3) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, max(a.x + 1) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, max(a.s COLLATE BINARY) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM d LEFT JOIN fa a USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.x < d.h GROUP BY d.g`,
	`SELECT dt.g, count(*) FROM dt JOIN fu ON fu.k = dt.k JOIN fu2 ON fu2.k = dt.k GROUP BY dt.g`,
	`SELECT dn.g, count(*) FROM fn x JOIN dn USING (k) JOIN fn y USING (k) GROUP BY dn.g`,
	`SELECT d.g, count(*) OVER () FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT DISTINCT d.h, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fia a JOIN d USING (k) JOIN fib b USING (k) WHERE d.h = 1 GROUP BY d.g`,
	`SELECT d2.g, count(*) FROM fa a JOIN d2 USING (k) JOIN fb b USING (k) GROUP BY d2.g`,
	`SELECT a.x, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY a.x`,
	`SELECT count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k)`,
	`SELECT k, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY k`,
	`SELECT d.g, count(a.nope) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE abs(a.x) > 2 GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING (SELECT 1)`,
	`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = a.x GROUP BY d.g`,
}

// TestEagerAggregate: #400. A fanning-out join under count/min/max is joined
// per key (eager_agg.go) and answers as the join itself does; the shapes it
// must refuse keep the join. The reference for a served one is the same query with fb read
// through a subquery, which the rewrite does not take and the flattener then
// turns back into the plain join.
func TestEagerAggregate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.musq")
	buildDB(t, path, eagerAggSchema...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, q := range append(append([]string{}, eagerAggServed...), eagerAggRefused...) {
		stmt, err := parseSelectStatement(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		isServed := false
		for _, s := range eagerAggServed {
			isServed = isServed || s == q
		}
		if got := p.eagerAggregate(stmt) != nil; got != isServed {
			t.Errorf("%s: rewritten %v, want %v", q, got, isServed)
		}
		if !isServed {
			continue
		}
		ref := strings.NewReplacer(" fb b ", " (SELECT * FROM fb) b ", " fb ON", " (SELECT * FROM fb) fb ON", " fib b ", " (SELECT * FROM fib) b ").Replace(q)
		if ref == q {
			t.Fatalf("%s: no reference form", q)
		}
		gotCols, got, gerr := p.Query(q)
		wantCols, want, werr := p.Query(ref)
		if fmt.Sprint(gotCols, got, gerr) != fmt.Sprint(wantCols, want, werr) {
			t.Errorf("%s:\n got %v %v %v\nwant %v %v %v", q, gotCols, got, gerr, wantCols, want, werr)
		}
	}
}
