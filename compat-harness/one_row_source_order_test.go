package compat

import "testing"

// TestOneRowSourceKeepsArrivalOrder: a subquery or view that returns at most
// one row (an aggregate with no GROUP BY) cannot change the order the other
// source's rows reach an order-sensitive aggregate, wherever the loops nest it.
// TPC-H q22 sums REAL balances over such a join. The REALs here are chosen so
// that their sum depends on the order they are added in.
func TestOneRowSourceKeepsArrivalOrder(t *testing.T) {
	base := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, g TEXT, r REAL, k INT)`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<240)
		 INSERT INTO t SELECT x, char(65 + x % 5), x * 0.1 + CASE x % 7 WHEN 0 THEN 1e16 WHEN 3 THEN -1e16 ELSE 0 END, (x * 37) % 101 FROM n`,
		`CREATE TABLE s(v REAL, h INT)`,
		`INSERT INTO s VALUES(0.5, 1), (2.25, 2), (-1.75, 1), (10.0, 3)`,
		`CREATE VIEW one AS SELECT avg(v) AS av FROM s`,
	}
	served := []string{
		`SELECT g, count(*), sum(r) FROM (SELECT g, r, av FROM one JOIN t) a WHERE r > av GROUP BY g ORDER BY g`,
		`SELECT g, count(*), sum(r) FROM (SELECT g, r, av FROM t JOIN one) a WHERE r > av GROUP BY g ORDER BY g`,
		`SELECT g, sum(r), total(r), avg(r) FROM (SELECT avg(v) AS av FROM s) o JOIN t WHERE r > o.av GROUP BY g ORDER BY g`,
		`SELECT g, sum(r) FROM one, t WHERE r > av GROUP BY g`,
		`SELECT g, sum(r) FROM t, (SELECT max(v) AS m FROM s) WHERE r > m GROUP BY g`,
		`SELECT g, sum(r) FROM one LEFT JOIN t ON r > av GROUP BY g`,
		`SELECT sum(r), total(r) FROM one JOIN t WHERE r > av`,
		`SELECT g, group_concat(id) FROM one JOIN t WHERE r > av GROUP BY g`,
	}
	for _, q := range served {
		differ(t, "one-row-source", append(append([]string{}, base...), q))
	}
	// An index C may walk on the other table, or a second ordinary source:
	// answered as C does, or declined, never answered in another order.
	guarded := [][]string{
		{`CREATE INDEX t_k ON t(k)`, `SELECT g, sum(r) FROM one JOIN t WHERE k > 50 AND r > av GROUP BY g`},
		{`CREATE INDEX t_g ON t(g)`, `SELECT g, sum(r) FROM one JOIN t WHERE r > av GROUP BY g`},
		{`CREATE INDEX t_gk ON t(g, k)`, `SELECT g, sum(r), group_concat(id) FROM t JOIN one WHERE g > 'A' GROUP BY g`},
		{`CREATE TABLE t2(id INTEGER PRIMARY KEY, w REAL)`, `INSERT INTO t2 SELECT id, r * 2 FROM t WHERE id % 3 = 0`,
			`SELECT g, sum(w) FROM one JOIN t JOIN t2 ON t2.id = t.id WHERE r > av GROUP BY g`},
		{`SELECT g, sum(r) FROM (SELECT avg(v) AS av FROM s HAVING av > 100) JOIN t GROUP BY g`},
		{`SELECT g, sum(r) FROM (SELECT h, avg(v) AS av FROM s GROUP BY h) JOIN t WHERE r > av GROUP BY g`},
	}
	for _, extra := range guarded {
		differAllowingDeclines(t, "one-row-source-guarded", append(append([]string{}, base...), extra...))
	}
}
