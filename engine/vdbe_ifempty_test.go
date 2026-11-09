package engine

import (
	"fmt"
	"testing"
)

// TestIfEmptyInnerJoinTable: a third inner-join table is tested for rows
// before the loops open (OpIfEmpty, no P4), from one row rather than a rewind
// of the whole table. The answer must follow the table as segments, the log
// and a session's own writes leave it.
func TestIfEmptyInnerJoinTable(t *testing.T) {
	stmts := []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, bid INTEGER, cid INTEGER)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE c (id INTEGER PRIMARY KEY, w INTEGER)`,
	}
	for i := 0; i < 50; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, %d)`, i, i))
	}
	for i := 0; i < 300; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO a VALUES (%d, %d, %d)`, i, i%50, i%40))
	}
	p := newSegPair(t, stmts...)
	const q = `SELECT count(*) FROM a, b, c WHERE b.id = a.bid AND c.id = a.cid`
	const rowsQ = `SELECT a.id, b.v, c.w FROM a, b, c WHERE b.id = a.bid AND c.id = a.cid`
	for _, q := range []string{q, rowsQ} {
		_, plan, err := p.fast(`EXPLAIN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range plan {
			found = found || string(r[1].S) == "IfEmpty"
		}
		if !found {
			t.Fatalf("no OpIfEmpty in %s, so this test tests nothing", q)
		}
	}
	count := func(stage string, want int64) {
		t.Helper()
		rows := p.mustFast(q)
		if len(rows) != 1 || rows[0][0].I != want {
			t.Errorf("%s: %v, want %d", stage, rows, want)
		}
		if rows := p.mustFast(rowsQ); int64(len(rows)) != want {
			t.Errorf("%s: %d rows, want %d", stage, len(rows), want)
		}
	}
	count("empty", 0)
	p.delta(`INSERT INTO c SELECT value, value FROM generate_series(0, 9)`)
	count("log rows", 80)
	n, err := OpenWrite(p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	if err := n.Exec(`DELETE FROM c`); err != nil {
		t.Fatal(err)
	}
	if _, rows, err := n.Query(q, nil); err != nil || len(rows) != 1 || rows[0][0].I != 0 {
		t.Errorf("session emptied it: %v %v, want 0", rows, err)
	}
	if _, rows, err := n.Query(rowsQ, nil); err != nil || len(rows) != 0 {
		t.Errorf("session emptied it: %d rows %v, want 0", len(rows), err)
	}
	if err := n.Exec(`INSERT INTO c VALUES (3, 3)`); err != nil {
		t.Fatal(err)
	}
	if _, rows, err := n.Query(q, nil); err != nil || len(rows) != 1 || rows[0][0].I != 8 {
		t.Errorf("session refilled it: %v %v, want 8", rows, err)
	}
	if _, rows, err := n.Query(rowsQ, nil); err != nil || len(rows) != 8 {
		t.Errorf("session refilled it: %d rows %v, want 8", len(rows), err)
	}
}
