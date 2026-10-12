package compat

import (
	"fmt"
	"testing"
)

// TestUnionAllStreamedDerived compares with C SQLite the shapes whose
// outermost FROM source is a UNION ALL chain, which musql reads as consecutive
// co-routines instead of building the rows: aggregates over it, a WHERE on it,
// mixed types and affinities, empty and failing arms, parameters, and the
// compounds that must keep materializing (ORDER BY, LIMIT, UNION, a join).
func TestUnionAllStreamedDerived(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT COLLATE NOCASE, r REAL)`,
		`INSERT INTO t VALUES (1,1,10,'a',1.5),(2,8,20,'B',2.5),(3,5,30,'c',NULL),(4,9,40,'A',4.5),(5,0,50,NULL,5.5),(6,8,60,'b',6.5)`,
		`CREATE TABLE u(x, y TEXT, z NUMERIC)`,
		`INSERT INTO u VALUES (1,'1','2.0'),('x','y',3),(NULL,NULL,NULL),(2.5,'a','b')`,
		`CREATE TABLE e(a)`,
	}
	queries := []string{
		`SELECT count(*) FROM (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7)`,
		`SELECT count(*), sum(k), min(k), max(k), avg(k), total(k) FROM (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7)`,
		`SELECT count(*) FROM (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7 UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM e UNION ALL SELECT k FROM t WHERE k > 100 UNION ALL SELECT k FROM e)`,
		`SELECT count(*), sum(k) FROM (SELECT k FROM e UNION ALL SELECT k FROM t WHERE k > 7)`,
		`SELECT sum(k) FROM (SELECT k FROM t WHERE k > 100 UNION ALL SELECT k FROM e)`,
		`SELECT count(*) FROM (SELECT a AS k FROM e UNION ALL SELECT a FROM e)`,
		`SELECT k, count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) GROUP BY k ORDER BY k`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) WHERE k > 4`,
		`SELECT sum(k) FROM (SELECT k FROM t UNION ALL SELECT v FROM t) WHERE k > 4`,
		`SELECT count(*), count(s), group_concat(s) FROM (SELECT s FROM t UNION ALL SELECT s FROM t)`,
		`SELECT count(*), count(r), sum(r) FROM (SELECT r FROM t UNION ALL SELECT k FROM t)`,
		`SELECT typeof(x), count(*) FROM (SELECT x FROM u UNION ALL SELECT y FROM u UNION ALL SELECT z FROM u) GROUP BY 1 ORDER BY 1`,
		`SELECT typeof(z), z, count(*) FROM (SELECT z FROM u UNION ALL SELECT x FROM u) GROUP BY 1, 2 ORDER BY 1, 2`,
		`SELECT min(s), max(s), count(DISTINCT s) FROM (SELECT s FROM t UNION ALL SELECT s FROM t)`,
		`SELECT sum(k + 1), count(*) FROM (SELECT k, v FROM t UNION ALL SELECT v, k FROM t) WHERE v >= 8`,
		`SELECT count(*) FROM (SELECT k FROM t ORDER BY k LIMIT 2 UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT * FROM (SELECT k FROM t LIMIT 2) UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t LIMIT 5)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t LIMIT 5 OFFSET 3)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t ORDER BY 1 DESC LIMIT 4)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t EXCEPT SELECT k FROM t WHERE k = 8)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t UNION SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) a JOIN (SELECT k FROM t UNION ALL SELECT k FROM t) b ON a.k = b.k`,
		`SELECT count(*) FROM t WHERE k IN (SELECT k FROM t WHERE k < 3 UNION ALL SELECT k FROM t WHERE k > 7)`,
		`SELECT (SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT id FROM t WHERE id = t2.id)) FROM t t2 ORDER BY id`,
		`SELECT count(*) FROM (SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3)`,
		`SELECT sum(c) FROM (SELECT count(*) AS c FROM t UNION ALL SELECT count(*) FROM t WHERE k > 7)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k / 0 FROM t)`,
		`SELECT count(*), sum(k) FROM (SELECT k FROM t UNION ALL SELECT json_extract('{bad', '$') FROM t)`,
		`SELECT sum(k) FROM (SELECT k FROM t UNION ALL SELECT 9223372036854775807 FROM t UNION ALL SELECT 1 FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k, v FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM missing_table)`,
		`SELECT count(*) FROM (SELECT json_object('a', k) AS j FROM t UNION ALL SELECT json_object('b', v) FROM t) WHERE j LIKE '%a%'`,
		`SELECT count(*), sum(json_extract(j, '$.a')) FROM (SELECT json_object('a', k) AS j FROM t UNION ALL SELECT json_object('a', v) FROM t)`,
		`SELECT COUNT(*) AS n FROM (SELECT 1 UNION ALL SELECT 2) AS x`,
		`SELECT   count( * )   FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(*) "my col" FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT * FROM t UNION ALL SELECT * FROM t)`,
		`SELECT count(*) FROM (SELECT * FROM t UNION ALL SELECT id, k FROM t)`,
		`SELECT count(*) FROM (SELECT k, v FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT DISTINCT k FROM t UNION ALL SELECT k FROM t GROUP BY k)`,
		`SELECT count(*) FROM (SELECT count(*) FROM t UNION ALL SELECT k FROM t WHERE k > 100 UNION ALL SELECT max(k) FROM t HAVING max(k) > 100)`,
		`SELECT count(*) FROM (SELECT k FROM t WHERE k > 100 UNION ALL SELECT k FROM t WHERE k > 100)`,
		`SELECT count(*) FROM (SELECT value FROM json_each('[1,2,3]') UNION ALL SELECT 1 UNION ALL SELECT k FROM t LIMIT 1)`,
		`SELECT count(*) FROM (SELECT value FROM json_each('[1,2,3]') UNION ALL SELECT 1)`,
		`WITH c AS (SELECT k FROM t) SELECT count(*) FROM (SELECT k FROM c UNION ALL SELECT k FROM c)`,
		`SELECT count(*) FROM (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT 2) UNION ALL SELECT 5)`,
		`SELECT count(*), 1 FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(k) FROM (SELECT k FROM t UNION ALL SELECT NULL)`,
		`SELECT count(*) + 1 FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT typeof(count(*)) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(DISTINCT k) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FILTER (WHERE k > 4) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) GROUP BY 1`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) HAVING count(*) > 100`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) LIMIT 0`,
		`SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t) ORDER BY 1`,
		`SELECT (SELECT count(*) FROM (SELECT t2.k UNION ALL SELECT t2.v)) FROM t t2 ORDER BY id`,
		`SELECT id, (SELECT count(*) FROM (SELECT k FROM t WHERE k < t2.k UNION ALL SELECT k FROM t WHERE k > t2.k)) FROM t t2 ORDER BY id`,
	}
	multi := [][]string{
		{`CREATE TABLE t0(c0)`, `CREATE TABLE t1(c1, c3)`, `INSERT INTO t0 VALUES (1)`, `INSERT INTO t1 VALUES (2, 3), (4, 3)`, `SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM t1 GROUP BY c3 HAVING (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT sum(DISTINCT c1)))) BETWEEN 1 AND 1`},
		{`CREATE VIEW vv AS SELECT count(*) AS c FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`, `SELECT c FROM vv`, `SELECT * FROM vv`},
		{`CREATE VIEW vw AS SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT v FROM t)`, `SELECT * FROM vw`},
		{`CREATE TABLE ctas AS SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`, `SELECT sql FROM sqlite_schema WHERE name = 'ctas'`, `SELECT * FROM ctas`},
		{`INSERT INTO e SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`, `SELECT a, typeof(a) FROM e`},
		{`BEGIN`, `INSERT INTO t VALUES (7, 3, 3, 'z', 0.5)`, `SELECT count(*), sum(k) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`, `ROLLBACK`, `SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t)`},
		{`CREATE TRIGGER tr AFTER INSERT ON e BEGIN INSERT INTO t(k) SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT k FROM t); END`, `INSERT INTO e VALUES (1)`, `SELECT id, k FROM t ORDER BY id`},
	}
	for i, m := range multi {
		stmts := append(append([]string{}, setup...), m...)
		t.Run(fmt.Sprintf("multi-%d", i), func(t *testing.T) { differ(t, "union-all-stream", stmts) })
	}
	for _, q := range queries {
		stmts := append(append([]string{}, setup...), q)
		t.Run(q, func(t *testing.T) { differ(t, "union-all-stream", stmts) })
	}
}
