package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// multiColSeekSchema is a composite key whose leading column holds one value
// for most of the table (the replication clock's shape), an index over an
// INTEGER and a NOCASE column, and a key over columns of no affinity holding
// every class, which the segments cannot index.
func multiColSeekSchema() []string {
	stmts := []string{
		`CREATE TABLE c(tbl TEXT, pk BLOB, col TEXT, v, PRIMARY KEY(tbl, pk, col))`,
		`CREATE TABLE e(a INTEGER, b TEXT COLLATE NOCASE, r REAL, v)`,
		`CREATE UNIQUE INDEX e_ab ON e(a, b)`,
		`CREATE TABLE x(a, b, c, UNIQUE(a, b))`,
	}
	for i := 0; i < 3000; i++ {
		tbl := "t"
		if i%10 == 0 {
			tbl = "u"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES('%s', x'%04x', 'c%d', %d)`, tbl, i/5, i%5, i))
	}
	for i := 0; i < 2000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO e VALUES(%d, 'Name%d', %d.5, %d)`, i/4, i%4, i%7, i))
	}
	vals := []string{"1", "'1'", "2.5", "NULL", "x'31'", "'abc'", "'ABC'"}
	for i := 0; i < len(vals)*len(vals); i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO x VALUES(%s, %s, %d)`, vals[i%len(vals)], vals[i/len(vals)], i))
	}
	return stmts
}

// multiColSeekQuery is one query, its literal spelling and the same with its
// keys bound.
type multiColSeekQuery struct {
	lit, bound string
	args       []Value
}

func multiColSeekQueries() []multiColSeekQuery {
	txt := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	blob := func(b ...byte) Value { return Value{Typ: Blob, S: b} }
	i64 := func(n int64) Value { return Value{Typ: Int, I: n} }
	f64 := func(f float64) Value { return Value{Typ: Float, F: f} }
	null := Value{Typ: Null}
	q := func(lit, bound string, args ...Value) multiColSeekQuery {
		return multiColSeekQuery{lit: lit, bound: bound, args: args}
	}
	return []multiColSeekQuery{
		q(`SELECT v FROM c WHERE tbl='t' AND pk=x'0001' AND col='c1'`, `SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, txt("t"), blob(0, 1), txt("c1")),
		q(`SELECT v, col FROM c WHERE tbl='t' AND pk=x'0001'`, `SELECT v, col FROM c WHERE tbl=? AND pk=?`, txt("t"), blob(0, 1)),
		q(`SELECT v FROM c WHERE pk=x'0002' AND tbl='u'`, `SELECT v FROM c WHERE pk=? AND tbl=?`, blob(0, 2), txt("u")),
		q(`SELECT count(*) FROM c WHERE col='c3' AND tbl='t'`, `SELECT count(*) FROM c WHERE col=? AND tbl=?`, txt("c3"), txt("t")),
		q(`SELECT v FROM c WHERE tbl='t' AND pk=x'0003' AND col>'c1'`, `SELECT v FROM c WHERE tbl=? AND pk=? AND col>?`, txt("t"), blob(0, 3), txt("c1")),
		q(`SELECT v FROM c WHERE tbl='t' AND pk='0001' AND col='c1'`, `SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, txt("t"), txt("\x00\x01"), txt("c1")),
		q(`SELECT v FROM c WHERE tbl=1 AND pk=x'0001'`, `SELECT v FROM c WHERE tbl=? AND pk=?`, i64(1), blob(0, 1)),
		q(`SELECT v FROM c WHERE tbl='t' AND pk=x'0001' AND col=NULL`, `SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, txt("t"), blob(0, 1), null),
		q(`SELECT group_concat(v) FROM c WHERE tbl='t' AND pk=x'0004'`, `SELECT group_concat(v) FROM c WHERE tbl=? AND pk=?`, txt("t"), blob(0, 4)),
		q(`SELECT v FROM e WHERE a=5 AND b='NAME1'`, `SELECT v FROM e WHERE a=? AND b=?`, i64(5), txt("NAME1")),
		q(`SELECT v FROM e WHERE a='5' AND b='name2'`, `SELECT v FROM e WHERE a=? AND b=?`, txt("5"), txt("name2")),
		q(`SELECT v FROM e WHERE a=5.0 AND b='Name3'`, `SELECT v FROM e WHERE a=? AND b=?`, f64(5.0), txt("Name3")),
		q(`SELECT v FROM e WHERE a=6 AND r=1.5`, `SELECT v FROM e WHERE a=? AND r=?`, i64(6), i64(1)),
		q(`SELECT v FROM e WHERE b='name0' AND r=3.5`, `SELECT v FROM e WHERE b=? AND r=?`, txt("name0"), f64(3.5)),
		q(`SELECT v FROM e WHERE a=7 AND b>'name1'`, `SELECT v FROM e WHERE a=? AND b>?`, i64(7), txt("name1")),
		q(`SELECT c FROM x WHERE a=1 AND b='abc'`, `SELECT c FROM x WHERE a=? AND b=?`, i64(1), txt("abc")),
		q(`SELECT c FROM x WHERE a='1' AND b=1`, `SELECT c FROM x WHERE a=? AND b=?`, txt("1"), i64(1)),
		q(`SELECT c FROM x WHERE a=x'31' AND b=2.5`, `SELECT c FROM x WHERE a=? AND b=?`, blob('1'), f64(2.5)),
		q(`SELECT c FROM x WHERE a='ABC' AND b IS NULL`, `SELECT c FROM x WHERE a=? AND b IS ?`, txt("ABC"), null),
	}
}

// TestMultiColumnSeekMatchesTheScan: an equality on several columns is answered
// from whichever column's posting list the segments say is smallest. Each query,
// literal and bound, is compared with the same SQL compiled with no index seek:
// over segments, over rows the log holds -- through a session, which loads the
// table, and through a read-only pager, which reads the log beside the segments
// -- and inside a transaction whose own rows are not committed.
func TestMultiColumnSeekMatchesTheScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.musq")
	buildDB(t, path, multiColSeekSchema()...)
	execDB(t, path, `VACUUM`)

	render := func(rows [][]Value) string {
		var b strings.Builder
		for _, r := range rows {
			for _, c := range r {
				fmt.Fprintf(&b, "%d:%d:%v:%q|", c.Typ, c.I, c.F, string(c.S))
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	check := func(stage string, query func(string, []Value) ([]string, [][]Value, error)) {
		t.Helper()
		segMultiKeySeeksServed.Store(0)
		for _, q := range multiColSeekQueries() {
			for _, form := range []struct {
				sql  string
				args []Value
			}{{q.lit, nil}, {q.bound, q.args}} {
				_, got, err := query(form.sql, form.args)
				if err != nil {
					t.Fatalf("%s %s: %v", stage, form.sql, err)
				}
				// Spelled differently, so no cached plan of the seek answers it.
				served := segIndexSeeksServed.Load()
				indexSeeksOffForTest = true
				_, want, err := query(form.sql+" /* no seek */", form.args)
				indexSeeksOffForTest = false
				if err != nil {
					t.Fatalf("%s %s with no seek: %v", stage, form.sql, err)
				}
				if segIndexSeeksServed.Load() != served {
					t.Fatalf("%s %s: the reference seeked too", stage, form.sql)
				}
				if render(got) != render(want) {
					t.Errorf("%s %s %v:\n seek %.300q\n scan %.300q", stage, form.sql, form.args, render(got), render(want))
				}
			}
		}
		if segMultiKeySeeksServed.Load() == 0 {
			t.Errorf("%s: no seek over several keys was served, so every query compared a scan with a scan", stage)
		}
	}

	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	check("segments", n.Query)
	n.Discard()

	// The log's rows are tested against each probe rather than unioned whole, so
	// rows move both into and out of probed keys here.
	execDB(t, path,
		`INSERT INTO c VALUES('t', x'0001', 'c9', 9001)`,
		`UPDATE c SET v = -v WHERE tbl = 't' AND pk = x'0004'`,
		`DELETE FROM c WHERE pk = x'0002' AND col = 'c0'`,
		`UPDATE c SET pk = x'0001', col = 'c7' WHERE pk = x'0100' AND col = 'c3'`,
		`UPDATE c SET pk = x'0300' WHERE pk = x'0003' AND col = 'c2'`,
		`INSERT INTO e VALUES(5, 'NAME9', 1.5, 9002)`,
		`UPDATE e SET b = 'name1x' WHERE a = 7 AND b = 'Name1'`,
		`UPDATE e SET a = 6, b = 'Moved', r = 1.5 WHERE a = 9 AND b = 'Name2'`,
		`INSERT INTO x VALUES(1, 'abd', 9003)`)
	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	check("log", n.Query)
	n.Discard()
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check("log, read-only", p.QueryArgs)
	p.Close()

	n, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for _, s := range []string{
		`BEGIN`,
		`INSERT INTO c VALUES('t', x'0001', 'c8', 9101)`,
		`INSERT INTO c VALUES('u', x'0002', 'c7', 9102)`,
		`UPDATE c SET col = 'c6' WHERE tbl = 't' AND pk = x'0003' AND col = 'c4'`,
		`DELETE FROM c WHERE tbl = 't' AND pk = x'0004' AND col = 'c1'`,
		`INSERT INTO e VALUES(6, 'NEW', 1.5, 9103)`,
		`DELETE FROM e WHERE a = 5 AND b = 'Name2'`,
		`INSERT INTO x VALUES('1', 'abd', 9104)`,
	} {
		if err := n.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	check("open transaction", n.Query)

	// Enough written rows, and enough seeks testing them one by one, that the
	// store builds its own index mid-transaction (prefersEqIndex) and then has
	// to keep it through more writes.
	for i := 0; i < 2500; i++ {
		if err := n.Exec(fmt.Sprintf(`INSERT INTO c VALUES('w', x'%04x', 'c%d', %d)`, i/5, i%5, 20000+i)); err != nil {
			t.Fatal(err)
		}
	}
	check("a large transaction", n.Query)
	check("a large transaction, again", n.Query)
	if c := n.DB.findTableMeta("c"); c == nil || c.rows == nil || len(c.rows.eqIdx) == 0 {
		t.Fatal("the store never built its own index, so the writes below test nothing new")
	}
	for _, s := range []string{
		`UPDATE c SET tbl = 't', pk = x'0001', col = 'c6' WHERE tbl = 'w' AND pk = x'0010' AND col = 'c0'`,
		`UPDATE c SET pk = x'0700' WHERE tbl = 'w' AND pk = x'0010'`,
		`UPDATE c SET tbl = 't', col = 'c8' WHERE tbl = 'w' AND pk = x'0011' AND col = 'c0'`,
		`DELETE FROM c WHERE tbl = 't' AND pk = x'0001' AND col = 'c2'`,
		`UPDATE c SET pk = x'0002' WHERE tbl = 'u' AND pk = x'0002' AND col = 'c7'`,
	} {
		if err := n.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	check("a large transaction, after more writes", n.Query)
}

// TestMultiColumnSeekPlan pins EXPLAIN QUERY PLAN for the composite-key shapes
// to C SQLite's text, verified against the 3.53.3 oracle
// (compat-harness/multicol_seek_test.go compares them live).
func TestMultiColumnSeekPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	buildDB(t, path, multiColSeekSchema()...)
	for _, tc := range []struct{ q, want string }{
		{`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, `SEARCH c USING INDEX sqlite_autoindex_c_1 (tbl=? AND pk=? AND col=?)`},
		{`SELECT v FROM c WHERE tbl=? AND pk=?`, `SEARCH c USING INDEX sqlite_autoindex_c_1 (tbl=? AND pk=?)`},
		{`SELECT col FROM c WHERE tbl=? AND pk=?`, `SEARCH c USING COVERING INDEX sqlite_autoindex_c_1 (tbl=? AND pk=?)`},
		{`SELECT v FROM c WHERE tbl='t' AND pk=x'01' AND col>'c1'`, `SEARCH c USING INDEX sqlite_autoindex_c_1 (tbl=? AND pk=? AND col>?)`},
		{`SELECT v FROM e WHERE a=? AND b=?`, `SEARCH e USING INDEX e_ab (a=? AND b=?)`},
		{`SELECT v FROM e WHERE a=? AND b>? AND b<?`, `SEARCH e USING INDEX e_ab (a=? AND b>? AND b<?)`},
	} {
		got, err := queryDB(t, path, "EXPLAIN QUERY PLAN "+tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if !strings.Contains(got, fmt.Sprintf("%q", tc.want)) {
			t.Errorf("%s:\n got  %s\n want %s", tc.q, got, tc.want)
		}
	}
}
