package engine

import (
	"fmt"
	"testing"
)

// TestSegFilterNullablePredColumns: a count or an INTEGER sum filtered on
// columns that hold NULLs, of every storage type, answers as the loop does
// from the segment blocks, a NULL row satisfying no comparison: every
// operator, BETWEEN, two nullable columns ANDed, a bound on a NULL-free column
// beside one on a nullable column, and with rows in the delta. The rowid alias
// and a VIRTUAL generated column, whose NULL cells are not NULLs, still answer.
func TestSegFilterNullablePredColumns(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, q INTEGER, r REAL, s TEXT, k INTEGER NOT NULL, v INTEGER NOT NULL)`}
	for i := 1; i <= 5000; i++ {
		q, r, s := fmt.Sprint(i%40), fmt.Sprint(float64(i%90)*0.5), fmt.Sprintf("'w%03d'", i%120)
		if i%7 == 0 {
			q = "NULL"
		}
		if i%11 == 0 {
			r = "NULL"
		}
		if i%13 == 0 {
			s = "NULL"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %s, %s, %d, %d)`, i, q, r, s, i%9, i%1000-500))
	}
	p := newSegPair(t, stmts...)
	var queries []string
	for _, op := range []string{"<", "<=", ">", ">=", "<>"} {
		queries = append(queries,
			`SELECT count(*) FROM t WHERE q `+op+` 20`,
			`SELECT count(*) FROM t WHERE r `+op+` 12.5`,
			`SELECT count(*) FROM t WHERE s `+op+` 'w060'`,
			`SELECT sum(v) FROM t WHERE q `+op+` 20`,
		)
	}
	queries = append(queries,
		`SELECT count(*) FROM t WHERE q BETWEEN 1 AND 20`,
		`SELECT count(*) FROM t WHERE q >= 1 AND r > 5.0`,
		`SELECT count(*) FROM t WHERE q <= 20 AND s < 'w050'`,
		`SELECT count(*) FROM t WHERE r < 20.0 AND s >= 'w030'`,
		`SELECT count(*) FROM t WHERE k = 3 AND q >= 0`,
		`SELECT count(*) FROM t WHERE r BETWEEN 0 AND 44.5`,
		`SELECT sum(v) FROM t WHERE q BETWEEN 1 AND 20`,
		`SELECT sum(v) FROM t WHERE q > 5 AND s > 'w010'`,
		`SELECT sum(k) FROM t WHERE r > 3.5`,
	)
	for _, q := range queries {
		want := typedRows(p.mustPlain(q))
		served := segFilterServed.Load()
		got := typedRows(p.mustFast(q))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s:\n fast  %v\n plain %v", q, got, want)
		}
		if segFilterServed.Load() == served {
			t.Errorf("%s: not served from the segments", q)
		}
	}
	// The INTEGER PRIMARY KEY and a VIRTUAL generated column hold NULL cells
	// that are not NULLs: the rowid, and a value computed from the others.
	g := newSegPair(t, `CREATE TABLE g(id INTEGER PRIMARY KEY, a INTEGER, b INTEGER GENERATED ALWAYS AS (a * 2) VIRTUAL, q INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 3000) INSERT INTO g(id, a, q) SELECT i, i % 50, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 40 END FROM c`)
	for _, q := range []string{
		`SELECT count(*) FROM g WHERE id > 100`,
		`SELECT sum(a) FROM g WHERE id > 0`,
		`SELECT sum(a) FROM g WHERE id <= 2500 AND q > 3`,
		`SELECT count(*) FROM g WHERE b > 10`,
		`SELECT sum(a) FROM g WHERE b >= 20 AND q < 30`,
	} {
		if got, want := typedRows(g.mustFast(q)), typedRows(g.mustPlain(q)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s:\n fast  %v\n plain %v", q, got, want)
		}
	}
	p.delta(`INSERT INTO t VALUES(9001, NULL, 2.5, NULL, 1, 7)`, `UPDATE t SET q = NULL, s = NULL WHERE id % 17 = 0`,
		`UPDATE t SET r = NULL WHERE id % 19 = 0`)
	for _, q := range queries {
		if got, want := typedRows(p.mustFast(q)), typedRows(p.mustPlain(q)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("delta %s:\n fast  %v\n plain %v", q, got, want)
		}
	}
}
