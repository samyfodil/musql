package engine

import (
	"fmt"
	"testing"
)

// TestSemiJoinCountMatchesTheLoop: count(*) under a correlated EXISTS over a
// rowid equality, answered as a semi-join over column blocks, must equal the
// loop's answer -- and must be SERVED for the shapes it claims and decline the
// rest, so a decline cannot pass as a match.
func TestSemiJoinCountMatchesTheLoop(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, bid INTEGER, k INTEGER, mixed, n INTEGER)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, w REAL, s TEXT)`,
		`CREATE TABLE sparse (id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE e (id INTEGER PRIMARY KEY)`,
	}
	for i := 0; i < 3000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, %d.5, 's%d')`, i*2-500, i%9, i%17))
	}
	for i := 0; i < 200; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO sparse VALUES (%d, %d)`, int64(i)*1_000_000_007-50, i))
	}
	for i := 1; i <= 5000; i++ {
		mixed := fmt.Sprintf("%d", i%4000)
		if i%7 == 0 {
			mixed = fmt.Sprintf("'%d'", i%4000)
		}
		n := fmt.Sprintf("%d", i%3000)
		if i%11 == 0 {
			n = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, %d, %s, %s)`, i, (i*37)%7000-600, i%5, mixed, n))
	}
	p := newSegPair(t, stmts...)
	iv := func(i int64) Value { return Value{Typ: Int, I: i} }
	cases := []struct {
		sql    string
		args   []Value
		served bool
	}{
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid)`, nil, true},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)`, []Value{iv(1000)}, true},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE t.bid = b.id AND b.w > 4.5)`, nil, true},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.s > 's5')`, nil, true},
		{`SELECT count(*) FROM t WHERE k <> 2 AND EXISTS (SELECT 1 FROM b WHERE b.id = t.bid) AND bid > 100`, nil, true},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid) AND id > 100`, nil, false}, // the rowid alias in the outer filter
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid) AND EXISTS (SELECT 1 FROM b WHERE b.id = t.id AND b.w < 3)`, nil, true},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.id)`, nil, true},                               // outer key is the rowid
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM e WHERE e.id = t.bid)`, nil, false},                             // empty inner: no segments to read
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM sparse WHERE sparse.id = t.bid * 1000000007 - 50)`, nil, false}, // an expression key
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.mixed)`, nil, false},                           // a TEXT value in the key
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.n)`, nil, false},                               // NULLs in the key
		{`SELECT count(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.id = t.bid)`, nil, false},
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)`, []Value{{Typ: Float, F: 10.5}}, false}, // a REAL bound on the rowid
		{`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)`, []Value{{Typ: Null}}, false},
	}
	// A sparse inner table, served through the map rather than the bitmap.
	sparseStmts := []string{`CREATE TABLE o (id INTEGER PRIMARY KEY, sid INTEGER)`, `CREATE TABLE sp (id INTEGER PRIMARY KEY, v INTEGER)`}
	for i := 0; i < 300; i++ {
		sparseStmts = append(sparseStmts, fmt.Sprintf(`INSERT INTO sp VALUES (%d, %d)`, int64(i)*1_000_000_007-50, i%3))
		sparseStmts = append(sparseStmts, fmt.Sprintf(`INSERT INTO o VALUES (%d, %d)`, i+1, int64(i%400)*1_000_000_007-50))
	}
	sp := newSegPair(t, sparseStmts...)

	check := func(stage string, p *segPair, sql string, args []Value, served bool) {
		_, want, err := p.plain(sql, args...)
		if err != nil {
			t.Fatalf("%s %s plain: %v", stage, sql, err)
		}
		ResetSegFilterCountersForTest()
		_, got, err := p.fast(sql, args...)
		if err != nil {
			t.Fatalf("%s %s fast: %v", stage, sql, err)
		}
		n, _ := SegFilterCountersForTest()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s %s %v: semi-join %s, loop %s", stage, sql, args, g, w)
		}
		if JITEnabled() && (n > 0) != served {
			t.Errorf("%s %s %v: served=%d, want %v", stage, sql, args, n, served)
		}
	}
	for _, c := range cases {
		check("segments", p, c.sql, c.args, c.served)
	}
	check("sparse", sp, `SELECT count(*) FROM o WHERE EXISTS (SELECT 1 FROM sp WHERE sp.id = o.sid AND sp.v = 1)`, nil, true)
	// Log rows on either side decline, and the loop answers.
	p.delta(`INSERT INTO b VALUES (99999, 1.5, 'x')`, `UPDATE t SET bid = 99999 WHERE id = 3`)
	for _, c := range cases[:3] {
		check("with log", p, c.sql, c.args, false)
	}
}
