package engine

import (
	"fmt"
	"testing"
)

// TestSegSortedSeekAcrossSegments: a sorted seek over a table of more than
// one segment answers as the scan does, each row read from its own segment.
func TestSegSortedSeekAcrossSegments(t *testing.T) {
	stmts := []string{
		`CREATE TABLE c (id INTEGER PRIMARY KEY, kr REAL)`,
		`CREATE TABLE o (id INTEGER PRIMARY KEY, cid INTEGER, v INTEGER)`,
		fmt.Sprintf(`INSERT INTO o SELECT value, (value*7919)%%45, value%%1000 FROM generate_series(1, %d)`, segmentRows+4000),
	}
	for i := range 40 {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO c VALUES (%d, %d.0)`, i, i))
	}
	p := newSegPair(t, stmts...)
	const q = `SELECT c.id, count(o.id), sum(o.v) FROM c LEFT JOIN o ON o.cid = c.kr GROUP BY c.id`
	const scan = `SELECT c.id, count(o.id), sum(o.v) FROM c LEFT JOIN o ON CAST(o.cid AS INTEGER) = c.kr GROUP BY c.id`
	segSortedSeeksServed.Store(0)
	got := p.mustFast(q)
	if segSortedSeeksServed.Load() == 0 {
		t.Fatal("the segments served no seek")
	}
	if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(p.mustFast(scan))); g != w {
		t.Fatalf("seek %.300s\n scan %.300s", g, w)
	}
}
