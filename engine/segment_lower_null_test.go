package engine

import (
	"fmt"
	"testing"
)

// TestSegLowerNullShapes pins the OpNull rule: an OpNull no lowered path
// reaches (the NULL arm of OR and IN) is admitted, and one a path does reach
// (a NULL literal the comparison reads) refuses the body. Every answer is
// checked against the plain VDBE whether or not it was served.
func TestSegLowerNullShapes(t *testing.T) {
	p := newSegPair(t, segProgStmts()...)
	cases := []struct {
		sql    string
		served bool
	}{
		{`SELECT count(*) FROM t WHERE k IN (1,3,5,7)`, true},
		{`SELECT count(*) FROM t WHERE k NOT IN (1,3)`, true},
		{`SELECT count(*) FROM t WHERE v > 10 OR k = 0`, true},
		{`SELECT count(*) FROM t WHERE k IN (1, NULL)`, false},
		{`SELECT count(*) FROM t WHERE k NOT IN (1, NULL)`, false},
		{`SELECT count(*) FROM t WHERE k = NULL`, false},
		{`SELECT count(*) FROM t WHERE k > NULL OR v > 0`, false},
		{`SELECT count(*) FROM t WHERE v < NULL`, false},
	}
	for _, c := range cases {
		want := fmt.Sprint(typedRows(p.mustPlain(c.sql)))
		ResetSegFilterCountersForTest()
		got := fmt.Sprint(typedRows(p.mustFast(c.sql)))
		served, _ := SegFilterCountersForTest()
		if got != want {
			t.Errorf("%s: fast %s plain %s (served=%d)", c.sql, got, want, served)
		}
		if JITEnabled() && (served > 0) != c.served {
			t.Errorf("%s: served=%d", c.sql, served)
		}
	}
}
