package compat

import (
	"fmt"
	"testing"
)

// Joins whose probed table is ordered by integer keys holding NULLs -- the
// foreign keys of TPC-DS's fact tables (issue #169). The rows a probe visits,
// and so a group_concat with no ORDER BY, must come in C's order: a NULL key
// sorts before every integer and matches nothing.
func TestNullIntegerKeyOrder(t *testing.T) {
	pre := []string{
		"CREATE TABLE f(id INTEGER PRIMARY KEY, a INT, b INT, v INT)",
		"CREATE TABLE d(sk INT, name TEXT)",
		"CREATE TABLE e(sk INT, k2 INT, tag TEXT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 400) " +
			"INSERT INTO f SELECT i, CASE WHEN i % 7 = 0 THEN NULL ELSE i % 13 END, CASE WHEN i % 5 = 0 THEN NULL ELSE i % 4 END, i FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM g WHERE i < 12) INSERT INTO d SELECT i, 'd' || i FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM g WHERE i < 40) INSERT INTO e SELECT i % 13, CASE WHEN i % 6 = 0 THEN NULL ELSE i % 4 END, 't' || i FROM g",
	}
	for i, q := range []string{
		"SELECT name, group_concat(v) FROM d, f WHERE f.a = d.sk GROUP BY name",
		"SELECT d.name, group_concat(f.v) FROM d JOIN f ON f.a = d.sk AND f.v > 100 GROUP BY d.sk",
		"SELECT tag, group_concat(v) FROM e, f WHERE f.a = e.sk AND f.b = e.k2 GROUP BY tag ORDER BY tag",
		"SELECT group_concat(v) FROM e JOIN f ON f.a = e.sk AND f.b = e.k2",
		"SELECT e.tag, (SELECT group_concat(v) FROM f WHERE f.a = e.sk AND f.b = e.k2) FROM e",
		"SELECT d.name, count(*), sum(v) FROM d LEFT JOIN f ON f.a = d.sk GROUP BY d.name",
		"SELECT group_concat(f.v) FROM f, f g WHERE g.a = f.b AND g.v < 20",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
	}
}
