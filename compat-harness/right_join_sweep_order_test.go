package compat

// A RIGHT or FULL JOIN's unmatched rows come out of a sweep that plans the
// right table on its own (sqlite3WhereRightJoinLoop), so with a covering
// index the sweep walks the index, not the rowids. A bare column of an
// aggregate takes the first row, so the order is visible in the answer.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var rightJoinSweepSchema = []string{
	"CREATE TABLE ra(id INTEGER PRIMARY KEY, k INT, v INT)",
	"INSERT INTO ra VALUES(1,1,10),(2,2,20),(3,2,30),(4,NULL,40),(5,5,50)",
	"CREATE TABLE rb(id INTEGER PRIMARY KEY, k INT, v INT)",
	"INSERT INTO rb VALUES(1,1,100),(2,2,200),(3,3,300),(4,NULL,400),(5,3,NULL)",
	"CREATE INDEX rb_k ON rb(k)",
	"CREATE INDEX ra_k ON ra(k)",
}

func TestRightJoinSweepOrder(t *testing.T) {
	for _, q := range []string{
		"SELECT a.id, b.id FROM ra a RIGHT JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, b.k FROM ra a FULL JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, b.v FROM ra a RIGHT JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, count(*) FROM ra a RIGHT JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, count(*) FROM ra a FULL JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, total(b.v) FROM ra a RIGHT JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id, max(b.v) FROM ra a FULL JOIN rb b ON a.k=b.k",
		"SELECT a.id, b.id FROM ra a RIGHT JOIN rb b ON a.k=b.k OR b.k IS NULL ORDER BY 1, 2",
		"SELECT a.id, b.id FROM ra a RIGHT JOIN rb b NOT INDEXED ON a.k=b.k",
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, q, append(append([]string{}, rightJoinSweepSchema...), q))
		})
	}
}
