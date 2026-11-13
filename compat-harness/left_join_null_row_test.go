package compat

import (
	"fmt"
	"testing"
)

// TPC-DS q78's shape (issue #169): a LEFT JOIN whose unmatched rows are most
// rows, read whole between matched ones, and GROUP BY keys that are integers
// or NULL, drained in key order either way.
func TestLeftJoinNullRowsAndIntegerGroups(t *testing.T) {
	pre := []string{
		"CREATE TABLE s(id INTEGER PRIMARY KEY, t INT, item INT, cust INT, q INT, p REAL)",
		"CREATE TABLE r(t INT, item INT, amt REAL, note TEXT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 600) " +
			"INSERT INTO s SELECT i, i % 97, CASE WHEN i % 17 = 0 THEN NULL ELSE i % 23 END, CASE WHEN i % 13 = 0 THEN NULL ELSE (i * 7) % 41 - 20 END, i % 9, (i % 11) * 0.25 FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 120) " +
			"INSERT INTO r SELECT (i * 5) % 97, i % 23, i * 0.5, 'n' || i FROM g",
	}
	for i, q := range []string{
		"SELECT s.id, r.* FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item ORDER BY s.id",
		"SELECT DISTINCT r.* FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item ORDER BY 1, 2, 3",
		"SELECT s.item, r.note, max(s.q) FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item GROUP BY s.item ORDER BY 1",
		"SELECT s.id, r.note, row_number() OVER (PARTITION BY r.note ORDER BY s.id) FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item ORDER BY s.id",
		"SELECT item, cust, sum(q), sum(p), count(*) FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item WHERE r.t IS NULL GROUP BY item, cust",
		"SELECT item, cust, sum(q) FROM s GROUP BY item, cust ORDER BY item DESC, cust",
		"SELECT cust, item, t % 3, sum(q) FROM s GROUP BY cust, item, t % 3 ORDER BY cust, item DESC",
		"SELECT count(*), sum(x) FROM (SELECT item, cust, sum(q) x FROM s GROUP BY item, cust)",
		"SELECT item, cust, sum(q) FROM s GROUP BY item, cust ORDER BY item, cust",
		"SELECT item, cust, sum(q) FROM s GROUP BY item, cust ORDER BY item DESC",
		"SELECT cust, item, count(*) FROM s LEFT JOIN r ON r.t = s.t AND r.item = s.item WHERE r.t IS NULL GROUP BY cust, item ORDER BY cust DESC",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
		differ(t, fmt.Sprintf("q-%d-vacuumed", i), append(append([]string(nil), pre...), "VACUUM", q))
	}
}
