package compat

import (
	"fmt"
	"testing"
)

// keepSetup is an enclosing table whose join key repeats under every storage
// class -- 2, '2' and 2.0 among them -- with NULLs, and a table a correlated
// subquery reads by it.
func keepSetup() []string {
	stmts := []string{
		"CREATE TABLE a(id INTEGER PRIMARY KEY, k, v INT, w TEXT)",
		"CREATE TABLE b(k, x REAL, y INT)",
	}
	keys := []string{"1", "2", "'2'", "2.0", "NULL", "'x'", "X'01'", "-0.0", "0"}
	for i := range 120 {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO a(k, v, w) VALUES(%s, %d, '%c')", keys[i%len(keys)], i%7, 'a'+rune(i%3)))
	}
	for i := range 90 {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO b VALUES(%s, %d.5, %d)", keys[(i*4)%len(keys)], i, i%5))
	}
	return stmts
}

// TestCorrelatedSubqueryKeptRows: a correlated scalar subquery whose rows are
// kept by the enclosing values it reads answers as C, which runs it for
// every row -- including where it reads the enclosing row in ways that keep
// nothing: an aggregate's argument, a function that is not constant, a
// nested subquery and an enclosing aggregate's value.
func TestCorrelatedSubqueryKeptRows(t *testing.T) {
	queries := []string{
		"SELECT id, (SELECT avg(x) FROM b WHERE b.k = a.k) FROM a",
		"SELECT id, (SELECT count(*) FROM b WHERE b.k IS a.k) FROM a",
		"SELECT id FROM a WHERE v > (SELECT avg(y) FROM b WHERE b.k = a.k) ORDER BY id",
		"SELECT id, (SELECT min(x) FROM b WHERE b.k = a.k AND b.y > a.v - 3) FROM a",
		"SELECT id, (SELECT CASE WHEN a.v > 3 THEN max(x) ELSE a.w END FROM b WHERE b.k = a.k) FROM a",
		"SELECT id, (SELECT sum(x * a.v) FROM b WHERE b.k = a.k) FROM a",
		"SELECT id, (SELECT group_concat(a.w || y) FROM b WHERE b.k = a.k) FROM a",
		"SELECT id, (SELECT x FROM b WHERE b.k = a.k ORDER BY y DESC, x LIMIT 1 OFFSET 1) FROM a",
		"SELECT id, (SELECT (SELECT count(*) FROM b b2 WHERE b2.y = b.y AND b2.k = a.k) FROM b WHERE b.k = a.k LIMIT 1) FROM a",
		"SELECT count(DISTINCT (SELECT random() + y FROM b WHERE b.k = a.k LIMIT 1)) > 20 FROM a",
		"SELECT count(DISTINCT (SELECT random() + sum(x) OVER () FROM b WHERE b.k = a.k LIMIT 1)) > 20 FROM a",
		"SELECT id, (SELECT value FROM json_each((SELECT json_array(a.v, a.w) WHERE a.k IS NOT NULL)) WHERE a.k = 1 LIMIT 1 OFFSET 1) FROM a",
		"SELECT v, sum(id), (SELECT count(*) FROM b WHERE b.y < sum(a.id) % 5) FROM a GROUP BY v",
		"SELECT a.id, (SELECT max(y) FROM b WHERE b.k = a2.k) FROM a LEFT JOIN a a2 ON a2.id = a.id + 100",
		"WITH t AS (SELECT k, sum(x) s FROM b GROUP BY k) SELECT id FROM a, t t1 WHERE t1.k = a.k AND t1.s > (SELECT avg(s) FROM t t2 WHERE t2.k IS NOT a.k) ORDER BY id",
	}
	for _, q := range queries {
		differ(t, q, append(keepSetup(), q))
	}
}
