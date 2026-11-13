package compat

import (
	"fmt"
	"testing"
)

// An automatic index whose key holds numbers of both kinds and NULLs (issue
// #169): the rows a probe visits, and so a group_concat with no ORDER BY or
// the ties of an ORDER BY, come in C's order, a NULL first and an INTEGER
// beside the REAL it equals.
func TestAutoIndexNumericKeyOrder(t *testing.T) {
	pre := []string{
		"CREATE TABLE f(id INTEGER PRIMARY KEY, k INT, p REAL, n NUMERIC)",
		"CREATE TABLE d(id INTEGER PRIMARY KEY, k INT, tag TEXT)",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 3000) " +
			"INSERT INTO f SELECT i, i % 37, CASE WHEN i % 11 = 0 THEN NULL ELSE ((i * 7) % 13) * 0.5 END, " +
			"CASE WHEN i % 5 = 0 THEN (i % 9) ELSE (i % 9) + 0.5 END FROM g",
		"WITH RECURSIVE g(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM g WHERE i < 60) INSERT INTO d SELECT i, i % 41, 't' || (i % 4) FROM g",
	}
	for i, q := range []string{
		"SELECT d.id, f.id, f.p FROM d JOIN f ON f.k = d.k ORDER BY d.id",
		"SELECT d.id, group_concat(f.id || ':' || f.p) FROM d JOIN f ON f.k = d.k GROUP BY d.id",
		"SELECT d.id, f.id, f.n FROM d JOIN f ON f.k = d.k ORDER BY d.id",
		"SELECT d.tag, group_concat(f.n), sum(f.p) FROM d JOIN f ON f.k = d.k GROUP BY d.tag",
		"SELECT d.id, f.id FROM d LEFT JOIN f ON f.k = d.k AND f.p > 2 ORDER BY d.id",
	} {
		differ(t, fmt.Sprintf("q-%d", i), append(append([]string(nil), pre...), q))
		differ(t, fmt.Sprintf("q-%d-vacuumed", i), append(append([]string(nil), pre...), "VACUUM", q))
	}
}
