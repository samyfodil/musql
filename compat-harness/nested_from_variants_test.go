package compat

// Shapes the parenthesized-join binder (engine/nested_from_bind.go) took from
// a decline to an answer, and their neighbours: NULL-padded rows, a FULL
// JOIN's coalesced column, names repeated across members and across the
// group boundary, ":N" naming through an alias, and an fts4 MATCH beside a
// group. Every one is compared against C, cells and names.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var nestedFromVariantSchema = []string{
	`CREATE TABLE ta(a,b)`, `CREATE TABLE td(b,g)`, `CREATE TABLE te(a,f)`, `CREATE TABLE tc(z)`,
	`INSERT INTO ta VALUES(1,2),(3,4),(5,NULL),(NULL,6)`,
	`INSERT INTO td VALUES(2,'x'),(4,'y'),(7,'w'),(NULL,'n')`,
	`INSERT INTO te VALUES(1,200),(3,300),(9,900)`,
	`INSERT INTO tc VALUES('p'),('q')`,
	`CREATE TABLE dj1(a INTEGER, b TEXT)`,
	`INSERT INTO dj1 VALUES(1,'one'),(2,'two'),(3,'three'),(NULL,'null')`,
	`CREATE TABLE dj2(a INTEGER, c TEXT)`,
	`INSERT INTO dj2 VALUES(1,'x'),(2,'y'),(5,'z')`,
	`CREATE TABLE dm1(a INTEGER, b TEXT, k INTEGER)`,
	`INSERT INTO dm1 VALUES(1,'one',7),(2,'two',8),(4,'four',9)`,
	`CREATE TABLE ss1(a, b)`, `INSERT INTO ss1 VALUES(1,'s1'),(2,'s2'),(NULL,'sn')`,
	`CREATE TABLE ssd(p, q)`, `INSERT INTO ssd VALUES(1,'d1'),(3,'d3')`,
	`CREATE TABLE q1(a)`, `CREATE TABLE q2(b)`, `CREATE TABLE q3(c)`, `CREATE TABLE q4(d)`,
	`INSERT INTO q1 VALUES(1)`, `INSERT INTO q2 VALUES(2),(3)`, `INSERT INTO q3 VALUES(4)`, `INSERT INTO q4 VALUES(5)`,
	`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
	`INSERT INTO t10 VALUES('apple'),('banana'),('cherry')`,
	`CREATE TABLE x(term TEXT, tag INTEGER)`,
	`INSERT INTO x VALUES('banana',1),('apple',2),('kiwi',3)`,
}

func TestNestedFromServedVariants(t *testing.T) {
	for _, q := range []string{
		// Front-move through an alias, with NULL-padded rows.
		`SELECT * FROM (ta LEFT JOIN td USING(b)) AS g ORDER BY 1, 2, 3`,
		`SELECT * FROM (ta RIGHT JOIN td USING(b)) AS g ORDER BY 1, 2, 3`,
		`SELECT * FROM (ta FULL JOIN td USING(b)) AS g ORDER BY 1, 2, 3`,
		`SELECT g.b, typeof(g.b), b FROM (ta FULL JOIN td USING(b)) AS g ORDER BY 1, 2`,
		`SELECT * FROM tc, (ta FULL JOIN td USING(b)) ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM tc, (ta NATURAL FULL JOIN td) ORDER BY 1, 2, 3, 4`,
		`SELECT b, ta.b, td.b FROM tc, (ta FULL JOIN td USING(b)) ORDER BY 1, 2, 3`,
		// Duplicate names across members and the group boundary.
		`SELECT * FROM (ta JOIN te ON ta.a=te.a) ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM ta, (te LEFT JOIN td ON te.a=td.b) ORDER BY 1, 2, 3, 4, 5, 6`,
		`SELECT ta.*, td.* FROM tc, (ta LEFT JOIN td USING(b)) ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM (ta NATURAL LEFT JOIN td NATURAL JOIN te) AS qq FULL JOIN tc ON true ORDER BY 1, 2, 3, 4, 5`,
		`SELECT * FROM (ta NATURAL LEFT JOIN td NATURAL LEFT JOIN te) AS qq FULL JOIN tc ON qq.a=1 ORDER BY 1, 2, 3, 4, 5`,
		// Derived tables beside and inside a group.
		`SELECT * FROM dm1 JOIN (dj1 LEFT JOIN (SELECT a, c FROM dj2) USING(a)) USING(a) ORDER BY 1, 2`,
		`SELECT * FROM dm1 LEFT JOIN (dj1 LEFT JOIN (SELECT a, c FROM dj2) USING(a)) USING(a) ORDER BY 1, 2`,
		`SELECT * FROM ((SELECT a, c FROM dj2) RIGHT JOIN dj1 USING(a)) ORDER BY 1, 2, 3`,
		`SELECT * FROM ((SELECT a, c FROM dj2) FULL JOIN dj1 USING(a)) ORDER BY 1, 2, 3`,
		`SELECT * FROM (dm1 RIGHT JOIN (dj1 RIGHT JOIN (SELECT a, c FROM dj2) USING(a)) USING(a)) ORDER BY 1, 2, 3, 4`,
		`SELECT * FROM (dm1 FULL JOIN (dj1 FULL JOIN (SELECT a, c FROM dj2) USING(a)) USING(a)) ORDER BY 1, 2, 3, 4`,
		// A table joined NATURALly to a group holding itself.
		`SELECT * FROM ss1 NATURAL JOIN (ss1 LEFT JOIN ssd ON ss1.a=ssd.p) ORDER BY 1, 2`,
		`SELECT * FROM ss1 NATURAL LEFT JOIN (ss1 RIGHT JOIN ssd ON ss1.a=ssd.p) ORDER BY 1, 2`,
		// ":N" naming in a flat group.
		`SELECT q3.rowid, q4.rowid FROM q1 JOIN ((q2 JOIN q3) JOIN q4) ORDER BY 1, 2`,
		`SELECT q2.rowid, q4.rowid FROM q1 LEFT JOIN ((q2 JOIN q3) JOIN q4) ORDER BY 1, 2`,
		// An fts4 MATCH beside a group, LEFT and inner.
		`SELECT tc.z, x.tag, t10.value FROM tc, (ta LEFT JOIN td USING(b)), x LEFT JOIN t10 ON t10 MATCH x.term ORDER BY 1, 2, 3`,
		`SELECT x.tag, t10.value FROM (ta LEFT JOIN td USING(b)), x JOIN t10 ON t10 MATCH x.term WHERE ta.a=1 ORDER BY 1, 2`,
	} {
		t.Run(q, func(t *testing.T) {
			differ(t, q, append(append([]string{}, nestedFromVariantSchema...), q))
		})
	}
}
