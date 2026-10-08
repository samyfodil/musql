package engine

import (
	"fmt"
	"testing"
)

// TestSegmentLikeMatchesTheLoop: a LIKE in a filtered count is matched in the
// segments' TEXT heap; it must answer as OpLike does in the loop, served for
// the shapes it claims and declined for the rest.
func TestSegmentLikeMatchesTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t (id INTEGER PRIMARY KEY, s TEXT, n TEXT, mixed, k INTEGER)`}
	words := []string{"row-1-7-payload", "Row-12-payload", "ROW-7-PAYLOAD", "abc", "ABC", "a_c", "x10", "X1y", "é-7", "", "%lit", "50%"}
	for i := 1; i <= 2000; i++ {
		w := words[i%len(words)] + fmt.Sprint(i%13)
		n := fmt.Sprintf("'%s'", w)
		if i%17 == 0 {
			n = "NULL"
		}
		mixed := fmt.Sprintf("'%s'", w)
		if i%5 == 0 {
			mixed = fmt.Sprint(i)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, '%s', %s, %s, %d)`, i, w, n, mixed, i%4))
	}
	p := newSegPair(t, stmts...)
	tv := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	cases := []struct {
		sql    string
		args   []Value
		served bool
	}{
		{`SELECT count(*) FROM t WHERE s LIKE '%-7-payload%'`, nil, true},
		{`SELECT count(*) FROM t WHERE s LIKE 'row%'`, nil, true},
		{`SELECT count(*) FROM t WHERE s LIKE '%PAYLOAD1'`, nil, true},
		{`SELECT count(*) FROM t WHERE s NOT LIKE '%a%'`, nil, true},
		{`SELECT count(*) FROM t WHERE s LIKE 'abc3'`, nil, true},
		{`SELECT count(*) FROM t WHERE s LIKE ? AND k = 2`, []Value{tv("%1%2%")}, true},
		{`SELECT count(*) FROM t WHERE s LIKE '%'`, nil, true},
		{`SELECT count(*) FROM t WHERE s LIKE 'a_c%'`, nil, false},                  // '_'
		{`SELECT count(*) FROM t WHERE s LIKE '50!%%' ESCAPE '!'`, nil, true},       // ESCAPE: the compiled kernel's service block
		{`SELECT count(*) FROM t WHERE n LIKE '%7%'`, nil, false},                   // NULLs in the column
		{`SELECT count(*) FROM t WHERE mixed LIKE '%1%'`, nil, false},               // numbers in the column
		{`SELECT count(*) FROM t WHERE s LIKE ?`, []Value{{Typ: Int, I: 7}}, false}, // a non-text pattern
		{`SELECT count(*) FROM t WHERE s LIKE 'é%'`, nil, false},                    // a non-ASCII pattern
	}
	for _, c := range cases {
		_, want, err := p.plain(c.sql, c.args...)
		if err != nil {
			t.Fatalf("%s plain: %v", c.sql, err)
		}
		ResetSegFilterCountersForTest()
		_, got, err := p.fast(c.sql, c.args...)
		if err != nil {
			t.Fatalf("%s fast: %v", c.sql, err)
		}
		n, _ := SegFilterCountersForTest()
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s %v: heap %s, loop %s", c.sql, c.args, g, w)
		}
		if JITEnabled() && (n > 0) != c.served {
			t.Errorf("%s %v: served=%d, want %v", c.sql, c.args, n, c.served)
		}
	}
}
