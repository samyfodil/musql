package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestColumnarAggregatesMergeTheDelta: the compiled-program aggregates
// (segRunOne, through segDeltaSplit) and the filtered sum
// (segFilterSumTable, through segDeltaSumCorrection) answer over the
// segments with the delta merged, rather than declining to the row loop once
// a table has been written. Each delta state below -- rows added, replaced,
// removed, a NULL and a REAL written over an integer column -- must give the
// loop's answer.
func TestColumnarAggregatesMergeTheDelta(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w INTEGER)`}
	for i := 1; i <= 4000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %d, %d)`, i, rng.Intn(10), rng.Intn(2_000_001)-1_000_000, rng.Intn(100)))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT count(*), sum(v) FROM t`,
		`SELECT count(*) FROM t`,
		`SELECT sum(v) FROM t`,
		`SELECT min(v), max(v) FROM t`,
		`SELECT avg(w), total(v) FROM t`,
		`SELECT sum(v) FROM t WHERE v > 0`,
		`SELECT sum(w) FROM t WHERE k = 3`,
		`SELECT count(*) FROM t WHERE v > 0 AND k <> 2`,
		`SELECT sum(v * 2 + w) FROM t WHERE k < 5`,
		`SELECT max(id), min(id), count(id) FROM t`,
		`SELECT sum(v) FROM t WHERE v > ?`,
	}
	check := func(label string) {
		t.Helper()
		for _, q := range queries {
			var args []Value
			if q == `SELECT sum(v) FROM t WHERE v > ?` {
				args = []Value{{Typ: Int, I: 250000}}
			}
			want := fmt.Sprint(typedRows(p.mustPlain(q, args...)))
			got := fmt.Sprint(typedRows(p.mustFast(q, args...)))
			if got != want {
				t.Errorf("%s %s:\n fast  %s\n plain %s", label, q, got, want)
			}
		}
	}
	check("segments")
	p.delta(`UPDATE t SET v = v + 1 WHERE id = 7`)
	check("one update")
	p.delta(`INSERT INTO t VALUES (9001, 3, 999, 1), (9002, 4, -5, 2)`, `DELETE FROM t WHERE id IN (1, 2, 4000)`)
	check("added and removed")
	p.delta(`UPDATE t SET v = -v WHERE id % 13 = 0`, `DELETE FROM t WHERE id % 29 = 0`)
	check("many")
	p.delta(`UPDATE t SET w = NULL WHERE id = 10`)
	check("a NULL")
	p.delta(`UPDATE t SET v = 2.5 WHERE id = 11`)
	check("a REAL")
}
