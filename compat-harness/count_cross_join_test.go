package compat

import "testing"

// TestCountCrossJoinAsC: #394's count(*) over a cross join whose WHERE terms
// each read one table, which musql answers as the product of the tables'
// counts, answers as C's nested loop does: NULLs, a REAL among INTEGERs, a
// NOCASE column, a WITHOUT ROWID table, an empty table, unqualified names and
// a constant term; and the names and errors C gives for a missing or
// ambiguous column, an unknown collation and a table named twice. The oracle
// has no generate_series; the engine's test covers it.
func TestCountCrossJoinAsC(t *testing.T) {
	differ(t, "count-cross-join", []string{
		`CREATE TABLE a(x INTEGER, s TEXT COLLATE NOCASE)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 300) INSERT INTO a SELECT v, char(65 + v % 26) FROM g`,
		`INSERT INTO a VALUES (NULL, NULL), (7.5, 'z')`,
		`CREATE TABLE b(y, k INTEGER PRIMARY KEY)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 200) INSERT INTO b SELECT v % 40, v FROM g`,
		`CREATE TABLE e(z)`,
		`CREATE TABLE w(p TEXT PRIMARY KEY, q) WITHOUT ROWID`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM g WHERE v < 50) INSERT INTO w SELECT 'k' || v, v FROM g`,
		`SELECT count(*) FROM a, b`,
		`SELECT count(*) FROM a, b WHERE a.x > 150`,
		`SELECT count(*) FROM a, b WHERE a.x > 150 AND b.y < 20`,
		`SELECT count(*) FROM a, b WHERE x > 150 AND y < 20 AND k % 3 = 0`,
		`SELECT count(*) AS n FROM a CROSS JOIN b JOIN w WHERE b.y BETWEEN 3 AND 9 AND w.q % 7 = 1 AND a.s = 'c'`,
		`SELECT count(*) FROM a x, a y WHERE (x.x IS NULL OR x.x = 7.5) AND y.s COLLATE BINARY = 'C'`,
		`SELECT count(*) FROM a x, a y WHERE x.x IS NULL OR x.x = 7.5 AND y.s = 'C'`,
		`SELECT count(*) FROM a, e WHERE a.x > 0`,
		`SELECT count(*) FROM e, a`,
		`SELECT count(*) FROM a, b WHERE 1 = 0`,
		`SELECT count(*) FROM a, b WHERE b.rowid IN (1, 5, 9, NULL) AND NOT a.x < 100 AND CAST(a.x AS TEXT) > '2'`,
		`SELECT count(*) FROM a, b WHERE a.nope > 1`,
		`SELECT count(*) FROM a, b WHERE b.y > 1 AND a.nope > 1`,
		`SELECT count(*) FROM a x, a y WHERE s = 'c'`,
		`SELECT count(*) FROM a, b WHERE rowid > 3`,
		`SELECT count(*) FROM a, a`,
		`SELECT count(*) FROM a, b WHERE a.s COLLATE nosuch = 'c'`,
	})
}
