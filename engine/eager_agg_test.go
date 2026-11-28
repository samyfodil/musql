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
// and a dim larger than the facts' key count, which they still fan out against.
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
	`SELECT d.g, count(*) AS n, count(a.x), count(b.t), max(b.t), min(b.t), max(a.x), min(a.x), max(b.y), min(b.y), max(a.r), min(d.h), count(d.h) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.h, count(*), max(a.x) FROM fa a, d, fb b WHERE a.k = d.k AND b.k = d.k AND a.x > 5 AND b.y < 30 GROUP BY d.h HAVING count(*) > 10 ORDER BY 2 DESC, 1`,
	`SELECT d.g, d.h, count(*) c FROM d JOIN fa a ON a.k = d.k JOIN fb b ON b.k = a.k GROUP BY d.g, d.h ORDER BY c DESC, d.g, d.h LIMIT 5 OFFSET 2`,
	`SELECT dt.g, count(*), max(b.t) FROM fa a JOIN dt USING (k) JOIN fb b USING (k) GROUP BY dt.g`,
	`SELECT d.g, count(*), count(c.z), max(c.z), min(b.t) FROM fa a JOIN d USING (k) JOIN fb b USING (k) JOIN fc c USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE d.h > 2 GROUP BY d.g`,
	`SELECT upper(d.g) || '!', count(*) + 1, max(a.x) * 2 FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(a.x) > 1 AND d.g <> 'g3'`,
	`SELECT d.g, count(*) FROM fia a JOIN d USING (k) JOIN fib b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.k IS NOT NULL AND b.t IS NOT NULL GROUP BY d.g ORDER BY count(*), d.g`,
	`SELECT d.g, max(a.x), count(*) FROM d JOIN fa a ON a.k = d.k AND a.x BETWEEN 2 AND 8 JOIN fb b ON b.k = d.k AND b.t IN ('a', 'b') GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE a.r > 100 GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = d.h GROUP BY d.g`,
	`SELECT d.g AS gg, count(*) AS n, max(b.t) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING n > 1000 AND gg <> 'g1' ORDER BY m, gg`,
	`SELECT d2.g, count(*) FROM fa a JOIN d2 USING (k) JOIN fb b USING (k) GROUP BY d2.g`,
}

var eagerAggRefused = []string{
	`SELECT d.g, max(a.s), min(a.s), max(a.r) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g, a.s`,
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
	`SELECT a.x, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY a.x`,
	`SELECT count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k)`,
	`SELECT k, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY k`,
	`SELECT d.g, count(a.nope) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) WHERE abs(a.x) > 2 GROUP BY d.g`,
	`SELECT d.g, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING (SELECT 1)`,
	`SELECT d.g, count(*) FROM fa a JOIN d ON a.k = d.k JOIN fb b ON b.k = a.x GROUP BY d.g`,
	`SELECT d.g, count(*) AS n FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING nope > 1`,
}

// TestEagerAggregateStat1: ANALYZE's rows per key decide the fan-out where an
// index on the key has them. Keys too sparse to count in a bitmap over their
// span leave the facts' fan-out unknown, and the join as written, until
// ANALYZE has measured it.
func TestEagerAggregateStat1(t *testing.T) {
	schema := []string{
		`CREATE TABLE ua(k INTEGER, x INTEGER)`,
		`INSERT INTO ua SELECT value % 40 * 1000000, value % 9 FROM generate_series(1, 2000)`,
		`CREATE INDEX ua_k ON ua(k)`,
		`CREATE TABLE ub(k INTEGER, y INTEGER)`,
		`INSERT INTO ub SELECT value % 40 * 1000000, value % 5 FROM generate_series(1, 2000)`,
		`CREATE INDEX ub_k ON ub(k)`,
		`CREATE TABLE d(k INTEGER, g TEXT)`,
		`INSERT INTO d SELECT value % 40 * 1000000, 'g' || (value % 3) FROM generate_series(1, 60)`,
	}
	q := `SELECT d.g, count(*), max(b.y) FROM ua a JOIN d USING (k) JOIN ub b USING (k) GROUP BY d.g`
	for _, analyzed := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "s.musq")
		if analyzed {
			buildDB(t, path, append(schema, `ANALYZE`)...)
		} else {
			buildDB(t, path, schema...)
		}
		p, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := parseSelectStatement(q)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.eagerAggregate(stmt) != nil; got != analyzed {
			t.Errorf("analyzed=%v: rewritten %v", analyzed, got)
		}
		p.Close()
	}
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
		// The same query as a FROM subquery and as a scalar subquery's body,
		// which compileSubProgram rewrites.
		// which compileSubProgram rewrites. The scalar one is named after its
		// text, which differs.
		for _, f := range []string{"%s", "SELECT * FROM (%s) ORDER BY 1", "SELECT (SELECT count(*) || '/' || max(n) FROM (SELECT count(*) AS n FROM (%s))) AS v"} {
			gotCols, got, gerr := p.Query(fmt.Sprintf(f, q))
			wantCols, want, werr := p.Query(fmt.Sprintf(f, ref))
			if fmt.Sprint(gotCols, got, gerr) != fmt.Sprint(wantCols, want, werr) {
				t.Errorf("%s:\n got %v %v %v\nwant %v %v %v", fmt.Sprintf(f, q), gotCols, got, gerr, wantCols, want, werr)
			}
		}
	}
}

// TestEagerAggregateCollation: min/max and a group's key take one of the
// group's equal values, and which one depends on the order rows arrive in. The
// per-key join's is not C's, so a column whose equal values may differ in bytes
// (texts under NOCASE or RTRIM, 5 and 5.0, the integer and the REAL -2^63,
// 0.0 and -0.0) is not rewritten, and answers as written. An INTEGER column
// is, and so is a REAL one without -0.0.
func TestEagerAggregateCollation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	buildDB(t, path,
		`CREATE TABLE d(k INTEGER, g TEXT, n)`,
		`INSERT INTO d SELECT value, 'g' || (value % 3), CASE WHEN value % 2 THEN 1 ELSE 1.0 END FROM generate_series(0, 19)`,
		`CREATE TABLE fa(k INTEGER, x INTEGER, s TEXT COLLATE NOCASE, u TEXT COLLATE RTRIM, v, i INTEGER, w, z REAL)`,
		`INSERT INTO fa SELECT value % 20, value, CASE WHEN value % 20 < 7 THEN 'a' ELSE 'B' END, CASE WHEN value % 3 = 0 THEN 'b ' ELSE 'b' END, CASE WHEN value % 3 THEN 5 ELSE 5.0 END, CASE WHEN value % 3 THEN -9223372036854775808 ELSE -9223372036854775808.0 END, CASE WHEN value % 3 THEN 0.0 ELSE -0.0 END, (value % 7) / 2.0 - 1 FROM generate_series(0, 399)`,
		`CREATE TABLE fm(k INTEGER, s TEXT COLLATE NOCASE)`,
		`INSERT INTO fm SELECT value % 20, char(CASE value % 3 WHEN 0 THEN 97 WHEN 1 THEN 65 ELSE 98 END) FROM generate_series(0, 399)`,
		`CREATE TABLE fb(k INTEGER, y)`,
		`INSERT INTO fb SELECT value % 20, value FROM generate_series(0, 399)`,
	)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, q := range []string{
		`SELECT d.k, min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k HAVING min(a.s) < 'B'`,
		`SELECT d.k, min(a.s) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k HAVING m < 'B'`,
		`SELECT d.k, min(a.s) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k ORDER BY min(a.s), d.k`,
		`SELECT d.k, max(a.s) AS m FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k ORDER BY m DESC, d.k`,
		`SELECT d.g, max(a.s) FROM fm a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(a.s) = 'B'`,
		`SELECT d.g, max(a.s), min(a.s) FROM fm a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING min(a.s) = 'A'`,
		`SELECT d.g, max(a.u) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g HAVING max(a.u) = 'b'`,
		`SELECT d.g, max(a.v), typeof(max(a.v)) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.g, min(a.i), typeof(min(a.i)) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.n, count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.n`,
		`SELECT d.k, min(a.x), max(a.x) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k`,
		`SELECT d.g, max(a.w), min(a.w) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.g`,
		`SELECT d.k, min(a.z), max(a.z), count(*) FROM fa a JOIN d USING (k) JOIN fb b USING (k) GROUP BY d.k`,
	} {
		stmt, err := parseSelectStatement(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		tieFree := strings.Contains(q, "a.x") || strings.Contains(q, "a.z")
		if rewritten := p.eagerAggregate(stmt) != nil; rewritten != tieFree {
			t.Errorf("%s: rewritten %v", q, rewritten)
		}
		_, got, err := p.Query(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
		}
		if !tieFree {
			continue
		}
		ref := strings.Replace(q, " fb b ", " (SELECT * FROM fb) b ", 1)
		if _, want, err := p.Query(ref); err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s:\n got %v\nwant %v %v", q, got, want, err)
		}
	}
}

// TestEagerAggregateFanout: the rewrite fires on the facts' own rows per key,
// not on their size against the dim's. Facts with many keys, or a key per
// row, joined to a small dim make a small join, which the per-key subqueries,
// joined to each other without an index, would make far slower. Facts with
// few keys still fan out, against a dim of any size.
func TestEagerAggregateFanout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.musq")
	buildDB(t, path,
		`CREATE TABLE d(k INTEGER, g TEXT)`,
		`INSERT INTO d SELECT value * 7, 'g' || (value % 10) FROM generate_series(0, 99)`,
		`CREATE TABLE ma(k INTEGER, x INTEGER)`,
		`INSERT INTO ma SELECT value % 5000, value FROM generate_series(0, 19999)`,
		`CREATE TABLE mb(k INTEGER, y INTEGER)`,
		`INSERT INTO mb SELECT value % 5000, value FROM generate_series(0, 19999)`,
		`CREATE TABLE ua(k INTEGER, x INTEGER)`,
		`INSERT INTO ua SELECT value, value FROM generate_series(0, 19999)`,
		`CREATE TABLE ub(k INTEGER, y INTEGER)`,
		`INSERT INTO ub SELECT value, value FROM generate_series(0, 19999)`,
		`CREATE TABLE fa(k INTEGER, x INTEGER)`,
		`INSERT INTO fa SELECT value % 100 * 7, value FROM generate_series(0, 3999)`,
		`CREATE TABLE fb(k INTEGER, y INTEGER)`,
		`INSERT INTO fb SELECT value % 100 * 7, value FROM generate_series(0, 3999)`,
		`CREATE TABLE big(k INTEGER, g TEXT)`,
		`INSERT INTO big SELECT value % 700, 'g' || (value % 10) FROM generate_series(0, 3999)`,
	)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct {
		q    string
		want bool
	}{
		{`SELECT d.g, count(*), max(b.y) FROM d JOIN ma a USING (k) JOIN mb b USING (k) GROUP BY d.g`, false},
		{`SELECT d.g, count(*), max(b.y) FROM ma a JOIN d USING (k) JOIN mb b USING (k) GROUP BY d.g`, false},
		{`SELECT d.g, count(*), max(b.y) FROM ua a JOIN d USING (k) JOIN ub b USING (k) GROUP BY d.g`, false},
		{`SELECT d.g, count(*), max(b.y) FROM d JOIN fa a USING (k) JOIN fb b USING (k) GROUP BY d.g`, true},
		{`SELECT big.g, count(*), max(b.y) FROM fa a JOIN big USING (k) JOIN fb b USING (k) GROUP BY big.g`, true},
	} {
		stmt, err := parseSelectStatement(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if got := p.eagerAggregate(stmt) != nil; got != c.want {
			t.Errorf("%s: rewritten %v, want %v", c.q, got, c.want)
		}
	}
}
