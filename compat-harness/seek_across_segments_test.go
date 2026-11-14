package compat

import (
	"fmt"
	"testing"
)

// Joins probing a table of two segments (issue #169): one keyed in rowid
// order, whose runs sit in one segment but at the boundary, one scattered
// over both, its runs ordered by a column holding NULLs, and one whose runs
// hold rows tied on the automatic index's key. Probed often enough that
// native code takes the seeks over (sortedSeekServed). The probes' runs come in C's order (ties of an ORDER BY on the
// outer table, a group_concat), a LEFT JOIN's unmatched rows read their
// columns as NULL, and NULL, REAL and TEXT probes match as C matches them.
func TestSeekAcrossSegments(t *testing.T) {
	pre := []string{
		"CREATE TABLE r(id INTEGER PRIMARY KEY, k INT, k2 INT, v INT, w INT, t TEXT, tc TEXT COLLATE NOCASE)",
		"CREATE TABLE q(id INTEGER PRIMARY KEY, k INT, v INT, w INT, x)",
		"CREATE TABLE s(id INTEGER PRIMARY KEY, k INT, k2 INT, kf REAL, kt TEXT)",
		"CREATE TABLE p(id INTEGER PRIMARY KEY, k INT, x INT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 70000) INSERT INTO p SELECT i, i % 4000, i % 3 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 70000) " +
			"INSERT INTO r SELECT i, i / 3, i % 5, (i * 37) % 1000, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 100 END, " +
			"char(65 + (i * 2) % 3), char(97 + (i * 5) % 2 - 32 * (i % 3 = 0)) FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 70000) " +
			"INSERT INTO q SELECT i, (i * 7919) % 20011, (i * 13) % 997, CASE WHEN i % 11 = 0 THEN NULL ELSE (i * 7) % 31 END, " +
			"CASE WHEN i % 13 = 0 THEN 'x' || i ELSE i END FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 3000) " +
			"INSERT INTO s SELECT i, CASE WHEN i % 31 = 0 THEN NULL ELSE (i * 97) % 23500 END, i % 5, (i * 97) % 23500, CAST((i * 97) % 23500 AS TEXT) FROM g",
		"INSERT INTO s VALUES (3001, 21845, 0, 21845.5, '21845'), (3002, 21845, 1, 21845.0, 'x'), (3003, 21846, 2, NULL, NULL)",
	}
	for i, sql := range []string{
		"SELECT s.id, group_concat(r.v) FROM s JOIN r ON r.k = s.k GROUP BY s.id",
		"SELECT s.id, r.v, r.k2 FROM s LEFT JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT s.id, r.v FROM s JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT s.id, q.v FROM s LEFT JOIN q ON q.k = s.k ORDER BY s.id",
		"SELECT s.id, q.w, q.id FROM s LEFT JOIN q ON q.k = s.k ORDER BY s.id",
		"SELECT s.id, q.w FROM s JOIN q ON q.k = s.k WHERE q.w IS NULL OR q.w > 20 ORDER BY s.id",
		"SELECT s.id, q.id, q.v FROM s JOIN q ON q.k = s.k WHERE q.w < 15 ORDER BY s.id",
		"SELECT s.id, q.id, q.x, q.v FROM s JOIN q ON q.k = s.k ORDER BY s.id",
		"SELECT s.id, r.v, r.id FROM s JOIN r ON r.k = s.k AND r.t = 'B' ORDER BY s.id",
		"SELECT s.id, r.t, r.id FROM s JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT s.id, p.x, p.id FROM s JOIN p ON p.k = s.k % 4000 ORDER BY s.id",
		"SELECT s.id, r.v, r.tc FROM s LEFT JOIN r ON r.k = s.k AND r.tc = 'a' ORDER BY s.id",
		"SELECT s.id, r.w, r.v FROM s LEFT JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT count(*), sum(s.id) FROM s LEFT JOIN r ON r.k = s.k AND r.k2 = s.k2 WHERE r.k IS NULL",
		"SELECT s.id, group_concat(coalesce(r.w, 'n')), count(r.id) FROM s LEFT JOIN r ON r.k = s.k AND r.k2 = s.k2 GROUP BY s.id",
		"SELECT s.id, r.id, r.w FROM s LEFT JOIN r ON r.k = s.k AND r.k2 = s.k2 WHERE s.id % 9 = 0 ORDER BY s.id, r.id",
		"SELECT s.id, group_concat(q.v) FROM s JOIN q ON q.k = s.k GROUP BY s.id",
		"SELECT count(*), sum(s.id) FROM s LEFT JOIN q ON q.k = s.k WHERE q.id IS NULL",
		"SELECT s.id, group_concat(r.v) FROM s JOIN r ON r.k = s.kf GROUP BY s.id",
		"SELECT s.id, group_concat(r.v) FROM s JOIN r ON r.k = s.kt GROUP BY s.id",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), sql))
		differ(t, fmt.Sprintf("q-%d-vacuumed", i), append(append([]string(nil), pre...), "VACUUM", sql))
	}
}
