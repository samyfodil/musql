package compat

import (
	"fmt"
	"testing"
)

// Subqueries in INSERT/UPDATE/DELETE statements: row order and index usage.
func TestWriteSubqueriesPlanAsC(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b, c, d)", "CREATE INDEX tc ON t(c)",
		"WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<30) INSERT INTO t SELECT n, 31 - n, 31 - n, n % 3 FROM s",
		"CREATE TABLE u(x UNIQUE, y)", "INSERT INTO u VALUES(0, 0), (1, 0), (2, 0)",
		"CREATE TABLE e(k)", "CREATE TABLE lg(v)",
	}
	modes := map[string][]string{
		"plain":    nil,
		"reverse":  {"PRAGMA reverse_unordered_selects = ON"},
		"analyzed": {"ANALYZE"},
		"small t":  {"ANALYZE", "DELETE FROM sqlite_stat1", "INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')", "ANALYZE sqlite_master"},
		"small t reversed": {"PRAGMA reverse_unordered_selects = ON", "ANALYZE", "DELETE FROM sqlite_stat1",
			"INSERT INTO sqlite_stat1 VALUES('t', NULL, '7')", "ANALYZE sqlite_master"},
	}
	shapes := [][]string{
		{"UPDATE u SET y = (SELECT group_concat(a) FROM t WHERE c > 23)", "SELECT * FROM u ORDER BY x"},
		{"UPDATE u SET y = (SELECT group_concat(t.a) FROM t WHERE t.d = u.x AND t.c > 12)", "SELECT * FROM u ORDER BY x"},
		{"INSERT INTO lg VALUES((SELECT group_concat(a) FROM t WHERE c > 24))", "SELECT * FROM lg"},
		{"INSERT INTO lg SELECT (SELECT group_concat(t2.a) FROM t t2 WHERE t2.d = t.d AND t2.c > 20) FROM t WHERE a < 4", "SELECT * FROM lg ORDER BY rowid"},
		{"UPDATE u SET x = x RETURNING x, (SELECT group_concat(a) FROM t WHERE c > 26)"},
		{"INSERT INTO u VALUES(9, 9) RETURNING (SELECT group_concat(a) FROM t WHERE c > 27)"},
		{"DELETE FROM u WHERE x = 2 RETURNING (SELECT group_concat(a) FROM t WHERE c > 25)"},
		{"INSERT INTO u VALUES(1, 5), (2, 6) ON CONFLICT(x) DO UPDATE SET y = (SELECT group_concat(t2.a) FROM t t2 WHERE t2.d = excluded.x AND t2.c > 10)",
			"SELECT * FROM u ORDER BY x"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN INSERT INTO lg VALUES((SELECT group_concat(t2.a) FROM t t2 WHERE t2.c > new.k)); END",
			"INSERT INTO e VALUES(22)", "SELECT * FROM lg"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN INSERT INTO lg SELECT (SELECT group_concat(t2.a) FROM t t2 WHERE t2.c > new.k); END",
			"INSERT INTO e VALUES(23)", "SELECT * FROM lg"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN UPDATE t SET d = (SELECT group_concat(t2.a) FROM t t2 WHERE t2.c > new.k) WHERE a = 1; END",
			"INSERT INTO e VALUES(25)", "SELECT d FROM t WHERE a = 1"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN UPDATE t SET b = (SELECT group_concat(t2.a) FROM t t2 WHERE t2.d = t.d AND t2.c = new.k) WHERE c > 3; END",
			"INSERT INTO e VALUES(4)", "SELECT group_concat(b, ';') FROM (SELECT b FROM t ORDER BY a)"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN UPDATE t SET b = (SELECT count(*) FROM t t2 WHERE t2.d = t.d AND t2.b > 20) WHERE c > new.k; END",
			"INSERT INTO e VALUES(4)", "SELECT group_concat(b) FROM (SELECT b FROM t ORDER BY a)"},
		{"CREATE TRIGGER tr AFTER INSERT ON e BEGIN DELETE FROM t WHERE b < (SELECT max(t2.b) FROM t t2 WHERE t2.d = t.d) - 20; END",
			"INSERT INTO e VALUES(2)", "SELECT group_concat(a) FROM (SELECT a FROM t ORDER BY a)"},
		{"CREATE TRIGGER tr AFTER UPDATE ON u BEGIN INSERT INTO lg VALUES((SELECT group_concat(a) FROM t WHERE c > new.y)); END",
			"UPDATE u SET y = 20 + x", "SELECT * FROM lg ORDER BY rowid"},
	}
	for mode, pre := range modes {
		for i, sh := range shapes {
			differ(t, fmt.Sprintf("%s/%d", mode, i), append(append(append([]string{}, base...), pre...), sh...))
		}
	}
}
