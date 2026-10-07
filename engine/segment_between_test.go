package engine

import (
	"fmt"
	"testing"
)

// TestColumnarBetweenMatchesTheLoop checks the AND-tree spelling (BETWEEN, and
// AND inside one expression) against the plain VDBE, and that the kernel
// served it.
func TestColumnarBetweenMatchesTheLoop(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, n INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 5000)
		 INSERT INTO t SELECT i, i % 7, (i * 7919) % 10001 - 5000, CASE WHEN i % 50 = 0 THEN NULL ELSE i END FROM c`)
	iv := func(n int64) Value { return Value{Typ: Int, I: n} }
	cases := []struct {
		sql    string
		args   []Value
		served bool
	}{
		{`SELECT count(*) FROM t WHERE v BETWEEN -100 AND 2500`, nil, true},
		{`SELECT count(*) FROM t WHERE v BETWEEN ? AND ?`, []Value{iv(-3000), iv(10)}, true},
		{`SELECT count(*) FROM t WHERE v BETWEEN ? AND ?`, []Value{iv(10), iv(-3000)}, true}, // empty range
		{`SELECT count(*) FROM t WHERE v BETWEEN ? AND ?`, []Value{{Typ: Null}, iv(10)}, false},
		{`SELECT sum(v) FROM t WHERE v BETWEEN ? AND ?`, []Value{iv(-1000), iv(1000)}, true},
		{`SELECT count(*) FROM t WHERE (v > ? AND k <> ?)`, []Value{iv(0), iv(3)}, true},
		{`SELECT count(*) FROM t WHERE n BETWEEN 10 AND 4000`, nil, false}, // NULL-bearing column
		{`SELECT count(*) FROM t WHERE v > -2500`, nil, true}, // Integer; Negative
		{`SELECT count(*) FROM t WHERE v < -2500.5`, nil, true},
		{`SELECT count(*) FROM t WHERE v NOT BETWEEN -100 AND 100`, nil, true}, // the program JIT, since OR and IN
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
		served, _ := SegFilterCountersForTest()
		if fmt.Sprint(typedRows(got)) != fmt.Sprint(typedRows(want)) {
			t.Errorf("%s %v: fast %v plain %v", c.sql, c.args, typedRows(got), typedRows(want))
		}
		if programJITEnabled() && (served > 0) != c.served {
			t.Errorf("%s %v: served=%d", c.sql, c.args, served)
		}
	}
}
