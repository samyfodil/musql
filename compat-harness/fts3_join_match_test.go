// Tests MATCH operator on fts table inside multi-table joins. The MATCH
// constraint must be evaluated at the correct join level where the fts table
// cursor is positioned.
package compat

import "testing"

func TestFts3MatchInJoin(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fts4 table second in the FROM list", []string{
			`CREATE TABLE t1(a, b)`,
			`INSERT INTO t1 VALUES('a','hello')`,
			`INSERT INTO t1 VALUES('b','world')`,
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft VALUES('b')`,
			`INSERT INTO ft VALUES('y')`,
			// MATCH tested only once cursor is positioned
			`SELECT t1.a, ft.a FROM t1, ft WHERE ft MATCH 'b' ORDER BY t1.a, ft.a`,
			`SELECT t1.a, ft.a FROM ft, t1 WHERE ft MATCH 'b' ORDER BY t1.a, ft.a`,
			`SELECT t1.a, ft.a FROM t1, ft WHERE ft MATCH 'b' AND t1.a=ft.a`,
			`SELECT t1.a, ft.a FROM t1 JOIN ft ON t1.a=ft.a WHERE ft MATCH 'b'`,
			`SELECT count(*) FROM t1, ft WHERE ft MATCH 'nosuchterm'`,
		}},
		{"fts3 table second, column-form MATCH", []string{
			`CREATE TABLE t1(k)`,
			`INSERT INTO t1 VALUES(1)`,
			`INSERT INTO t1 VALUES(2)`,
			`CREATE VIRTUAL TABLE ft USING fts3(a, b)`,
			`INSERT INTO ft VALUES('one two','three')`,
			`INSERT INTO ft VALUES('four','one')`,
			// Column-form MATCH as control case
			`SELECT k, ft.a FROM t1, ft WHERE ft.a MATCH 'one' ORDER BY k, ft.a`,
			`SELECT k, ft.b FROM t1, ft WHERE ft.b MATCH 'one' ORDER BY k, ft.b`,
		}},
		{"three tables, fts4 last", []string{
			`CREATE TABLE t1(a)`,
			`CREATE TABLE t2(b)`,
			`INSERT INTO t1 VALUES('x')`,
			`INSERT INTO t2 VALUES('y')`,
			`CREATE VIRTUAL TABLE ft USING fts4(c)`,
			`INSERT INTO ft VALUES('alpha beta')`,
			`INSERT INTO ft VALUES('gamma')`,
			`SELECT a, b, c FROM t1, t2, ft WHERE ft MATCH 'beta'`,
			`SELECT a, b, c FROM t1, t2, ft WHERE ft MATCH 'alpha beta'`,
			`SELECT a, b, c FROM t1, t2, ft WHERE ft MATCH 'gamma OR beta' ORDER BY c`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}
