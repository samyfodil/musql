package engine

import (
	"fmt"
	"testing"
)

// TestSubqueryInlineMatchesSubProgram: a correlated scalar subquery relocated
// into its parent (subqueryInlinePeephole) answers as the per-row sub-program
// does, which the pair's plain side runs with the peepholes off. The shapes:
// a seek with and without a row, several rows of which the first is the
// answer, a row value, subqueries in WHERE, ORDER BY and the select list, one
// inside another and inside an EXISTS, an uncorrelated subquery inside one
// beside the parent's own, a bound parameter, NULL and non-integer keys, and
// rows the log holds.
func TestSubqueryInlineMatchesSubProgram(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, bid, k INTEGER, s TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT, w REAL, g INTEGER)`,
		`CREATE INDEX b_g ON b(g)`,
		`CREATE TABLE e (id INTEGER PRIMARY KEY, x INTEGER)`,
	}
	for i := 0; i < 400; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, 'l%d', %d.5, %d)`, i*3, i%7, i%11, i%40))
	}
	keys := []string{"%d", "%d", "%d", "NULL", "'%d'", "%d.0", "%d.5", "-%d"}
	for i := 1; i <= 600; i++ {
		k := keys[i%len(keys)]
		if k != "NULL" {
			k = fmt.Sprintf(k, i)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %d, 'l%d')`, i, k, i%45, i%13))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT t.id, (SELECT b.label FROM b WHERE b.id = t.bid) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT b.w FROM b WHERE b.rowid = t.bid), (SELECT b.label || t.s FROM b WHERE b.id = t.id) FROM t ORDER BY t.id`,
		`SELECT count(*) FROM t WHERE (SELECT b.w FROM b WHERE b.id = t.bid) > 3`,
		`SELECT t.id FROM t WHERE (SELECT b.label FROM b WHERE b.id = t.bid) IS NULL ORDER BY t.id`,
		`SELECT t.id, (SELECT b.id FROM b WHERE b.g = t.k) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT b.id FROM b WHERE b.label = t.s) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT e.x FROM e WHERE e.id = t.bid) FROM t ORDER BY t.id`, // empty inner table
		`SELECT t.id FROM t WHERE (t.k, t.s) = (SELECT b.g, b.label FROM b WHERE b.id = t.bid) ORDER BY t.id`,
		`SELECT t.id FROM t ORDER BY (SELECT b.w FROM b WHERE b.id = t.bid), t.id`,
		`SELECT t.id, (SELECT b.label FROM b WHERE b.id = (SELECT i.bid FROM t AS i WHERE i.id = t.id + 1)) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT b.label FROM b WHERE b.id = t.bid AND b.g = (SELECT max(e.x) FROM e)) FROM t ORDER BY t.id`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.w > (SELECT b2.w FROM b AS b2 WHERE b2.id = t.id))`,
		`SELECT t.id, (SELECT b.label FROM b WHERE b.id = t.bid AND b.id < ?) FROM t ORDER BY t.id`,
		`SELECT t.k, (SELECT b.label FROM b WHERE b.id = t.k * 3 AND b.w > 2) FROM t GROUP BY t.k ORDER BY t.k`,
		`SELECT t.id FROM t WHERE (SELECT b.g, b.label FROM b WHERE b.id = t.bid) IN (SELECT b2.g, b2.label FROM b AS b2 WHERE b2.w > 5) ORDER BY t.id`,
		`SELECT t.id, (SELECT b.g, b.label FROM b WHERE b.id = t.bid) NOT IN (SELECT b2.g, b2.label FROM b AS b2 WHERE b2.w > 5) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT b.label FROM b WHERE b.id = t.bid AND b.g IN (SELECT e.x FROM e)) FROM t ORDER BY t.id`,
		`SELECT t.id, (SELECT max(w) FROM b), (SELECT b.label FROM b WHERE b.id = t.bid AND b.g IN (SELECT e.x FROM e)) FROM t ORDER BY t.id`,
		`SELECT t.id, t.k IN (SELECT g FROM b WHERE w > 9), (SELECT b.label FROM b WHERE b.id = t.bid AND b.g IN (SELECT e.x FROM e)) FROM t ORDER BY t.id`,
	}
	run := func(stage string) {
		for _, q := range queries {
			_, want, err := p.plain(q, Value{Typ: Int, I: 600})
			if err != nil {
				t.Fatalf("%s plain %s: %v", stage, q, err)
			}
			_, got, err := p.fast(q, Value{Typ: Int, I: 600})
			if err != nil {
				t.Fatalf("%s fast %s: %v", stage, q, err)
			}
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("%s %s:\n inlined     %.300s\n sub-program %.300s", stage, q, g, w)
			}
		}
	}
	run("segments")
	inlined := 0
	for _, q := range queries {
		_, ops, err := p.fast("EXPLAIN "+q, Value{Typ: Int, I: 600})
		if err != nil {
			t.Fatal(err)
		}
		left := false
		for _, r := range ops {
			if string(r[1].S) == "Subquery" && r[6].I&int64(p5Correlated) != 0 {
				left = true
			}
		}
		if !left {
			inlined++
		} else {
			t.Logf("kept a sub-program: %s", q)
		}
	}
	if inlined < len(queries)/2 {
		t.Errorf("only %d of %d queries had every correlated subquery inlined", inlined, len(queries))
	}
	p.delta(`INSERT INTO b VALUES (1, 'new', 1.5, 7)`, `DELETE FROM b WHERE id = 3`, `UPDATE t SET bid = 1 WHERE id = 2`, `INSERT INTO e VALUES (5, 5)`)
	run("with log")
}
