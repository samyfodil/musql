package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestInnerScanList: a full scan below the outermost loop that nothing seeks
// walks a list over its table's segments on each rewind, instead of building
// every row at the table's full width (#385), and reads its rows from the
// segment each one is in, natively too. The table spans several segments and
// negative rowids; the answers are C SQLite's, before and after writes the
// segments no longer answer alone.
func TestInnerScanList(t *testing.T) {
	lowEntryThreshold(t)
	p := newSegPair(t,
		`CREATE TABLE o(k INT, n TEXT)`,
		`CREATE TABLE d(a INTEGER PRIMARY KEY, b TEXT, c INT, e INT, r REAL)`,
		`INSERT INTO o SELECT value, 'x' || value FROM generate_series(1, 6)`,
		`INSERT INTO d SELECT value - 45000, 'd' || value, value % 7, value % 11, value % 50 / 10.0 FROM generate_series(1, 90000)`)
	type tc struct {
		q, want string
		list    bool
	}
	ordered := `SELECT o.k, d.a FROM o CROSS JOIN d WHERE o.k <= 2 AND d.a % 30000 = 7`
	text := `SELECT count(*), sum(d.a) FROM o CROSS JOIN d WHERE o.k <> 2 AND d.b BETWEEN 'd5' AND 'd6'`
	cases := []tc{
		{text, "55560 277550030", true},
		{`SELECT count(*), sum(d.a) FROM o CROSS JOIN d WHERE o.n <> 'x2' AND d.b BETWEEN 'd5' AND 2002`, "0 NULL", true},
		{`SELECT count(*), sum(d.c) FROM o CROSS JOIN d WHERE o.k < 4 AND d.c > 5 AND d.e < 3`, "10521 63126", true},
		{ordered, "1 7|1 30007|2 7|2 30007", true},
		{`SELECT o.k, count(d.a), sum(d.a) FROM o LEFT JOIN d ON d.c + o.k = 9 AND d.a < 50 GROUP BY o.k`,
			"1 0 NULL|2 0 NULL|3 6435 -144626625|4 6435 -144633060|5 6436 -144639446|6 6436 -144645882", false},
		{`SELECT count(*), sum(d.a) FROM o CROSS JOIN d WHERE o.k > 3 AND d.r > 4.5 AND d.r < 4.7`, "5400 113400", true},
		{`SELECT o.k, d.a, d.b FROM o CROSS JOIN d WHERE o.k = 3 + 0 AND d.e = 10 AND d.c = 6 AND d.a BETWEEN -44000 AND -43000`,
			"3 -44000 d1000|3 -43923 d1077|3 -43846 d1154|3 -43769 d1231|3 -43692 d1308|3 -43615 d1385|3 -43538 d1462|3 -43461 d1539|" +
				"3 -43384 d1616|3 -43307 d1693|3 -43230 d1770|3 -43153 d1847|3 -43076 d1924", false},
	}
	run := func(cases []tc) {
		t.Helper()
		for _, jitOn := range []bool{true, false} {
			old := vmJITEnabled
			vmJITEnabled = jitOn
			for _, c := range cases {
				segScanLists.Store(0)
				if got := renderPlain(p.mustFast(c.q)); got != c.want {
					t.Errorf("jit=%v %s:\n%s\nwant %s", jitOn, c.q, got, c.want)
				}
				if c.list && segScanLists.Load() == 0 {
					t.Errorf("jit=%v %s: the inner scan built its rows", jitOn, c.q)
				}
			}
			vmJITEnabled = old
		}
	}
	run(cases)
	p.delta(`UPDATE d SET b = 'd5x' WHERE a = 7`, `DELETE FROM d WHERE a = 30007`)
	run([]tc{{text, "55565 277550065", false}, {ordered, "1 7|2 7", false}})
}

// renderPlain is rows as C's shell lists them: columns by a space, rows by "|".
func renderPlain(rows [][]Value) string {
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('|')
		}
		for j, v := range r {
			if j > 0 {
				b.WriteByte(' ')
			}
			switch v.Typ {
			case Int:
				fmt.Fprintf(&b, "%d", v.I)
			case Float:
				fmt.Fprintf(&b, "%v", v.F)
			case Text, Blob:
				b.Write(v.S)
			default:
				b.WriteString("NULL")
			}
		}
	}
	return b.String()
}
