package compat

import "testing"

// Attached database reads inside explicit transactions.

func TestAttachedReadInsideTransaction(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'m1'),(2,'m2')`,
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO aux.t VALUES(1,'A'),(3,'C')`,
		`CREATE TABLE aux.u(p)`,
		`INSERT INTO aux.u VALUES(9)`,
	}
	cases := [][]string{
		{`BEGIN`, `SELECT count(*) FROM aux.t`, `COMMIT`},
		{`BEGIN`, `SELECT b FROM aux.t ORDER BY a`, `COMMIT`},
		{`BEGIN`, `SELECT m.b, x.b FROM main.t m JOIN aux.t x ON m.a=x.a`, `COMMIT`},
		{`BEGIN`, `SELECT (SELECT count(*) FROM aux.t), (SELECT count(*) FROM main.t)`, `COMMIT`},
		{`BEGIN`, `INSERT INTO main.t VALUES(5,'m5')`, `SELECT count(*) FROM main.t`,
			`SELECT count(*) FROM aux.t`, `COMMIT`, `SELECT count(*) FROM main.t`},
		{`BEGIN`, `SELECT count(*) FROM aux.u`, `ROLLBACK`, `SELECT count(*) FROM aux.u`},
		{`BEGIN`, `SELECT * FROM main.t UNION ALL SELECT * FROM aux.t ORDER BY 1,2`, `COMMIT`},
		{`BEGIN`, `SELECT a FROM main.t WHERE a IN (SELECT a FROM aux.t) ORDER BY a`, `COMMIT`},
		{`SAVEPOINT s`, `SELECT count(*) FROM aux.t`, `RELEASE s`},
		{`BEGIN`, `SELECT count(*) FROM aux.t`, `INSERT INTO main.t VALUES(7,'m7')`,
			`SELECT count(*) FROM aux.t`, `ROLLBACK`, `SELECT count(*) FROM main.t`},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i))+tc[1], func(t *testing.T) {
			differ(t, "attachtxn/"+tc[1], append(append([]string{}, base...), tc...))
		})
	}
}
