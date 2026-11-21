package engine

import (
	"fmt"
	"testing"
)

// TestSegJoinGatherFewGroups: a joined GROUP BY hands each group's first row
// to the program as the loop does. With few groups their columns are read a
// cell at a time; with many, whole once enough groups have been gathered, the
// groups before that read a cell at a time.
func TestSegJoinGatherFewGroups(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE o(id INTEGER PRIMARY KEY, pri TEXT)`,
		`CREATE TABLE l(id INTEGER PRIMARY KEY, ok INTEGER, sn TEXT, rd TEXT, q INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 50) INSERT INTO o SELECT i, (i % 5 + 1) || '-PRI' FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 6000) INSERT INTO l SELECT i, i % 50 + 1,
			CASE WHEN i % 11 = 0 THEN NULL ELSE 'm' || (i % 4) END,
			printf('%d-%02d-%02d', 1990 + i % 9, i % 12 + 1, i % 28 + 1), i % 997 FROM c`,
	)
	db, err := OpenWrite(p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, c := range []struct {
		q    string
		many bool
	}{
		{`SELECT l.sn, count(*), sum(l.q) FROM o, l WHERE o.id = l.ok GROUP BY l.sn`, false},
		{`SELECT l.q, l.sn, count(*) FROM o, l WHERE o.id = l.ok GROUP BY l.q`, true},
	} {
		want := fmt.Sprint(p.plain(c.q))
		before := segJoinGatherWhole.Load()
		if got := fmt.Sprint(db.Query(c.q, nil)); got != want {
			t.Errorf("%s:\n  loop   %.300s\n  kernel %.300s", c.q, want, got)
		}
		if whole := segJoinGatherWhole.Load() - before; jitEnabled && (whole > 0) != c.many {
			t.Errorf("%s: %d columns read whole", c.q, whole)
		}
	}
}
