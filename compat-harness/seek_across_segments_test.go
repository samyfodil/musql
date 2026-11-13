package compat

import (
	"fmt"
	"testing"
)

// Joins probing a table of two segments (issue #169): one keyed in rowid
// order, whose runs sit in one segment but at the boundary, one scattered
// over both. The probes' runs come in C's order (ties of an ORDER BY on the
// outer table, a group_concat), a LEFT JOIN's unmatched rows read their
// columns as NULL, and NULL, REAL and TEXT probes match as C matches them.
func TestSeekAcrossSegments(t *testing.T) {
	pre := []string{
		"CREATE TABLE r(id INTEGER PRIMARY KEY, k INT, k2 INT, v INT, w INT)",
		"CREATE TABLE q(id INTEGER PRIMARY KEY, k INT, v INT)",
		"CREATE TABLE s(id INTEGER PRIMARY KEY, k INT, k2 INT, kf REAL, kt TEXT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 70000) " +
			"INSERT INTO r SELECT i, i / 3, i % 5, (i * 37) % 1000, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 100 END FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 70000) " +
			"INSERT INTO q SELECT i, (i * 7919) % 20011, (i * 13) % 997 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 900) " +
			"INSERT INTO s SELECT i, CASE WHEN i % 31 = 0 THEN NULL ELSE (i * 97) % 23500 END, i % 5, (i * 97) % 23500, CAST((i * 97) % 23500 AS TEXT) FROM g",
		"INSERT INTO s VALUES (1001, 21845, 0, 21845.5, '21845'), (1002, 21845, 1, 21845.0, 'x'), (1003, 21846, 2, NULL, NULL)",
	}
	for i, sql := range []string{
		"SELECT s.id, group_concat(r.v) FROM s JOIN r ON r.k = s.k GROUP BY s.id",
		"SELECT s.id, r.v, r.k2 FROM s LEFT JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT s.id, r.v FROM s JOIN r ON r.k = s.k ORDER BY s.id",
		"SELECT s.id, q.v FROM s LEFT JOIN q ON q.k = s.k ORDER BY s.id",
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
