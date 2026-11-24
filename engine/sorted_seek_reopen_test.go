package engine

import (
	"fmt"
	"testing"
)

// TestSortedSeekKeptAcrossReopen: a correlated EXISTS opens its tables once
// per outer row, and its sorted seek's rows were sorted again each time, from
// every segment of the table (TPC-DS q10 without ANALYZE, past 60 s against
// C's 37). A cursor the machine reopens on the same table takes over the rows
// its close left while they are still the table's. The answers are C SQLite's.
func TestSortedSeekKeptAcrossReopen(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE c(ck BIGINT, g BIGINT)`,
		`CREATE TABLE s(sck BIGINT, sdt BIGINT)`,
		`CREATE TABLE d(dsk BIGINT, yr BIGINT)`,
		`INSERT INTO c SELECT value, value % 7 FROM generate_series(1, 400)`,
		`INSERT INTO s SELECT (value * 7919) % 500, 1 + (value * 104729) % 73049 FROM generate_series(1, 3000)`,
		`INSERT INTO d SELECT value, 1900 + value / 365 FROM generate_series(1, 73049)`)
	for _, c := range []struct{ q, want string }{
		{`SELECT count(*), sum(ck) FROM c WHERE EXISTS (SELECT 1 FROM s, d WHERE s.sck = c.ck AND s.sdt = d.dsk AND d.yr + 0 = 2002)`,
			"[[11 2180]]"},
		{`SELECT g, count(*) FROM c WHERE NOT EXISTS (SELECT 1 FROM s, d WHERE s.sck = c.ck AND s.sdt = d.dsk AND d.yr + 0 BETWEEN 1990 AND 2010) GROUP BY g ORDER BY g`,
			"[[0 23] [1 22] [2 19] [3 22] [4 22] [5 22] [6 18]]"},
	} {
		segSortedBuilds.Store(0)
		_, rows, err := p.fast(c.q)
		if err != nil {
			t.Fatal(err)
		}
		var got [][]int64
		for _, r := range rows {
			var vs []int64
			for _, v := range r {
				vs = append(vs, v.I)
			}
			got = append(got, vs)
		}
		if s := fmt.Sprint(got); s != c.want {
			t.Errorf("%s:\n%s, want %s", c.q, s, c.want)
		}
		if n := segSortedBuilds.Load(); n > 1 {
			t.Errorf("%s: sorted %d times", c.q, n)
		}
	}
}
