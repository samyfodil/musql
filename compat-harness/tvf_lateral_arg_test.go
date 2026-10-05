package compat

import "testing"

// TestTableValuedFunctionLateralArguments compares with C SQLite a
// table-valued function whose argument reads a row: a table earlier in the
// same FROM, or an enclosing query's. C rewrites each argument into a
// "hidden-column = +arg" term (whereexpr.c:1926-1935), coded per outer row
// before OP_VFilter; the engine computes it into a register at the source's own
// join level (vtab_correlated.go).
func TestTableValuedFunctionLateralArguments(t *testing.T) {
	seed := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT, p TEXT, n INT)`,
		`INSERT INTO t VALUES(1,'[1,2,3]','$[1]',2),(2,'[]','$',0),(3,NULL,NULL,NULL),(4,'{"x":[4,5],"y":6}','$.x',1),(5,'[[7,8],[9]]','$[0]',3)`,
		`CREATE TABLE u(p,q,r)`,
		`CREATE TABLE s(k TEXT)`,
		`INSERT INTO s VALUES('t'),('u'),('nosuch')`,
	}
	for _, q := range []string{
		`SELECT t.id, j.key, j.value FROM t, json_each(t.a) j ORDER BY t.id, j.key`,
		`SELECT t.id, j.value FROM t, json_each(json_array(t.id, t.n)) j ORDER BY 1, 2`,
		`SELECT id, key, value FROM t, json_each(a) ORDER BY 1, 2`,
		`SELECT t.id, j.value FROM t JOIN json_each(t.a) j ON j.value>1 ORDER BY 1, 2`,
		`SELECT t.id, j.value FROM t LEFT JOIN json_each(t.a) j ORDER BY 1, 2`,
		`SELECT t.id, j.value FROM t LEFT JOIN json_each(t.a) j ON j.value>1 WHERE t.id<4 ORDER BY 1, 2`,
		`SELECT t.id, j.fullkey, j.value FROM t, json_each(t.a, t.p) j ORDER BY 1, 2`,
		`SELECT t.id, j.fullkey, j.type FROM t, json_tree(t.a) j ORDER BY 1, 2`,
		`SELECT t.id, j.key, k.value FROM t, json_each(t.a) j, json_each(j.value) k ORDER BY 1, 2, 3`,
		`SELECT t.id, sum(j.value), count(*) FROM t, json_each(t.a) j GROUP BY t.id ORDER BY 1`,
		`SELECT t.id, j.value, row_number() OVER (PARTITION BY t.id ORDER BY j.key DESC) FROM t, json_each(t.a) j ORDER BY 1, 2`,
		`SELECT DISTINCT j.type FROM t, json_each(t.a) j ORDER BY 1`,
		`SELECT id, (SELECT count(*) FROM json_each(o.a)) FROM t AS o ORDER BY id`,
		`SELECT id, (SELECT group_concat(value, '|') FROM json_each(o.a) WHERE value>o.n) FROM t AS o ORDER BY id`,
		`SELECT id FROM t AS o WHERE EXISTS (SELECT 1 FROM json_each(o.a) WHERE value=2) ORDER BY id`,
		`SELECT id FROM t AS o WHERE 3 IN (SELECT value FROM json_each(o.a)) ORDER BY id`,
		`SELECT k, (SELECT count(*) FROM pragma_table_info(s.k)) FROM s ORDER BY k`,
		`SELECT k, (SELECT group_concat(name) FROM pragma_table_info(s.k) WHERE cid>0) FROM s ORDER BY k`,
		`SELECT s.k, i.name FROM s, pragma_table_info(s.k) i ORDER BY 1, 2`,
		`SELECT id, (SELECT max(k.value) FROM json_each(o.a) j, json_each(j.value) k) FROM t o ORDER BY id`,
		`SELECT (SELECT count(*) FROM json_each(t.a), t) FROM t`,
		`WITH c(x) AS (VALUES('[10,20]'),('[30]')) SELECT c.x, j.value FROM c, json_each(c.x) j ORDER BY 1, 2`,
		`SELECT t.id, j.value FROM json_each(t.a) j, t ORDER BY 1, 2`,
	} {
		differ(t, "lateral tvf: "+q, append(append([]string{}, seed...), q))
	}
	differ(t, "lateral tvf: through a view", append(append([]string{}, seed...),
		`CREATE VIEW v AS SELECT t.id, j.value AS v FROM t, json_each(t.a) j`,
		`SELECT id, v FROM v ORDER BY 1, 2`,
		`SELECT id, count(*) FROM v GROUP BY id ORDER BY id`))
	differ(t, "lateral tvf: DELETE and UPDATE subqueries", append(append([]string{}, seed...),
		`DELETE FROM t AS o WHERE (SELECT count(*) FROM json_each(o.a))=0`,
		`SELECT id FROM t ORDER BY id`,
		`UPDATE t AS o SET n=(SELECT sum(value) FROM s, json_each(o.a))`,
		`SELECT id, n FROM t ORDER BY id`,
		`UPDATE t AS o SET n=(SELECT count(*) FROM t, json_each(o.a))`,
		`SELECT id, n FROM t ORDER BY id`))
	differ(t, "lateral tvf: INSERT SELECT and a trigger body", append(append([]string{}, seed...),
		`CREATE TABLE log(id, v)`,
		`INSERT INTO log SELECT t.id, j.value FROM t, json_each(t.a) j`,
		`SELECT * FROM log ORDER BY 1, 2`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log SELECT new.id, j.value FROM s, json_each(new.a) j WHERE s.k='t'; INSERT INTO log SELECT x.id, k.value FROM t x, json_each(x.a) k WHERE x.id=new.id; END`,
		`INSERT INTO t VALUES(9,'[90,91]',NULL,NULL)`,
		`SELECT * FROM log WHERE id=9 ORDER BY 1, 2`))
	differ(t, "lateral tvf: bound parameter and collation beside the row", append(append([]string{}, seed...),
		`SELECT t.id, j.value FROM t, json_each(t.a, '$') j WHERE j.value IS NOT NULL ORDER BY 1, 2`,
		`SELECT t.id, j.key FROM t, json_each(CASE WHEN t.id%2=1 THEN t.a ELSE '[0]' END) j ORDER BY 1, 2`))
}
