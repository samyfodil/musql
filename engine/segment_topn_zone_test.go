package engine

import (
	"fmt"
	"testing"
)

// TestColumnarTopNCutoffMatchesTheLoop exercises the top-N cutoff (row and
// zone) over heavy ties, both directions, and a delta that removes rows. Every
// ORDER BY is total: which of several tied rows a LIMIT keeps is unspecified,
// and the columnar path offers the delta's rows after the segments' where the
// loop interleaves them by rowid.
func TestColumnarTopNCutoffMatchesTheLoop(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, w INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 150000)
		 INSERT INTO t SELECT i, (i * 7919) % 97, i % 5 FROM c`)
	qs := []string{
		`SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20`,
		`SELECT id, v FROM t ORDER BY v, id LIMIT 20`,
		`SELECT id, v FROM t ORDER BY v DESC, w, id LIMIT 7`,
		`SELECT id, v FROM t ORDER BY v DESC, id LIMIT 5`,
		`SELECT id, v FROM t ORDER BY v, id DESC LIMIT 300`,
	}
	check := func(label string) {
		for _, q := range qs {
			want := fmt.Sprint(typedRows(p.mustPlain(q)))
			if got := fmt.Sprint(typedRows(p.mustFast(q))); got != want {
				t.Errorf("%s %s:\n fast  %v\n plain %v", label, q, got, want)
			}
		}
	}
	check("clean")
	p.delta(`DELETE FROM t WHERE v = 96 AND id % 3 = 0`, `UPDATE t SET v = 96 WHERE id IN (5, 77777, 149999)`,
		`DELETE FROM t WHERE v = 0 AND id < 50000`)
	check("delta")
}
