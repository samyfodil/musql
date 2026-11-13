package engine

import (
	"fmt"
	"testing"
)

// TestSegSortedSeekMultiKey: a sorted seek keyed on two columns is served from
// the segments on its first column where that column's runs are short, and
// answers as the seek defeated by a CAST does; where the first column holds
// few values its runs are most of the table, and the built rows serve it.
func TestSegSortedSeekMultiKey(t *testing.T) {
	stmts := []string{
		`CREATE TABLE c (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, y INTEGER)`,
		`CREATE TABLE o (id INTEGER PRIMARY KEY, cid INTEGER, k2 INTEGER, yr INTEGER, v TEXT)`,
	}
	for i := 0; i < 150; i++ {
		b := fmt.Sprint(i % 4)
		if i%10 == 0 {
			b = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES (%d, %d, %s, %d)`, i, i%60, b, 2000+i%2))
	}
	for i := 0; i < 2000; i++ {
		k2 := fmt.Sprint(i % 5)
		if i%13 == 0 {
			k2 = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO o VALUES (%d, %d, %s, %d, 'v%d')`, 5000-i, (i*7)%300, k2, 2000+i%2, i))
	}
	p := newSegPair(t, stmts...)
	for _, tc := range []struct {
		seek, scan string
		served     bool
	}{
		{`SELECT c.id, group_concat(o.id) FROM c JOIN o ON o.cid = c.a AND o.k2 = c.b GROUP BY c.id`,
			`SELECT c.id, group_concat(o.id) FROM c JOIN o ON CAST(o.cid AS INTEGER) = c.a AND CAST(o.k2 AS INTEGER) = c.b GROUP BY c.id`, true},
		{`SELECT c.id, count(o.id) FROM c LEFT JOIN o ON o.cid = c.a AND o.k2 = c.b GROUP BY c.id`,
			`SELECT c.id, count(o.id) FROM c LEFT JOIN o ON CAST(o.cid AS INTEGER) = c.a AND CAST(o.k2 AS INTEGER) = c.b GROUP BY c.id`, true},
		{`SELECT c.id, count(o.id) FROM c JOIN o ON o.yr = c.y AND o.k2 = c.b GROUP BY c.id`,
			`SELECT c.id, count(o.id) FROM c JOIN o ON CAST(o.yr AS INTEGER) = c.y AND CAST(o.k2 AS INTEGER) = c.b GROUP BY c.id`, false},
	} {
		segSortedSeeksServed.Store(0)
		got := p.mustFast(tc.seek)
		if n := segSortedSeeksServed.Load(); (n > 0) != tc.served {
			t.Errorf("%s: %d seeks served by the segments, want served=%v", tc.seek, n, tc.served)
		}
		want := p.mustFast(tc.scan)
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
			t.Errorf("%s:\n seek %.300s\n scan %.300s", tc.seek, g, w)
		}
	}
}
