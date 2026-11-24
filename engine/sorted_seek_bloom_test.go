package engine

import (
	"fmt"
	"testing"
)

// TestSortedSeekBloom: a sorted seek most of whose probes find no row answers
// them from a filter over the table's keys, as C's OP_Filter does (TPC-DS q49
// misses 30 of every 31). A probe the filter cannot rule out is still
// searched: one key a REAL, one TEXT, one NULL, NULLs in the table's key,
// and a REAL that is no integer, which equals no row. The answers are C
// SQLite's.
func TestSortedSeekBloom(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE s(dt INTEGER, tk INTEGER, it INTEGER, v INTEGER)`,
		`CREATE TABLE r(tk INTEGER, it INTEGER, amt INTEGER)`,
		`CREATE TABLE d(dsk INTEGER, yr INTEGER)`,
		`INSERT INTO s SELECT 1 + value % 100, value, CASE WHEN value % 97 = 0 THEN NULL ELSE value % 37 END, value FROM generate_series(1, 20000)`,
		`INSERT INTO r SELECT (value * 7) % 25000, CASE WHEN value % 2 = 0 THEN ((value * 7) % 25000) % 37 WHEN value % 11 = 0 THEN NULL ELSE ((value * 7) % 25000 + 1) % 37 END, value FROM generate_series(1, 3000)`,
		`INSERT INTO d SELECT value, 2000 + value / 20 FROM generate_series(1, 100)`)
	for _, c := range []struct {
		q, want string
		bloom   bool
	}{
		{`SELECT count(*), sum(v) FROM r CROSS JOIN d CROSS JOIN s WHERE s.dt = d.dsk AND s.tk = r.tk AND s.it = r.it AND d.yr = 2001`, "[283 2824402]", true},
		{`SELECT count(*), sum(v) FROM r CROSS JOIN d CROSS JOIN s WHERE s.dt = d.dsk AND s.tk = r.tk * 1.0 AND s.it = r.it AND d.yr = 2002`, "[283 2829554]", true},
		{`SELECT count(*), sum(v) FROM r CROSS JOIN d CROSS JOIN s WHERE s.dt = d.dsk AND s.tk = CAST(r.tk AS TEXT) AND s.it = r.it AND d.yr = 2003`, "[283 2842028]", true},
		{`SELECT count(*), sum(v) FROM r CROSS JOIN d CROSS JOIN s WHERE s.dt = d.dsk AND s.tk = r.tk + 0.5 AND s.it = r.it`, "[0 NULL]", false},
		{`SELECT count(*), sum(amt) FROM r WHERE EXISTS (SELECT 1 FROM d CROSS JOIN s WHERE s.dt = d.dsk AND s.tk = r.tk AND s.it = r.it AND d.yr = 2004)`, "[284 405740]", false},
	} {
		segBloomsBuilt.Store(0)
		_, rows, err := p.fast(c.q)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, v := range rows[0] {
			if v.Typ == Null {
				got = append(got, "NULL")
			} else {
				got = append(got, fmt.Sprint(v.I))
			}
		}
		if s := fmt.Sprint(got); s != c.want {
			t.Errorf("%s:\n%s, want %s", c.q, s, c.want)
		}
		if c.bloom && segBloomsBuilt.Load() == 0 {
			t.Errorf("%s: no filter answered its probes", c.q)
		}
	}
}
