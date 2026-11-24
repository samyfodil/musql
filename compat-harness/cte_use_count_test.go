package compat

import "testing"

// TestCTEUseCountedAsC: a CTE's references are counted as C counts them
// (CteUse.nUse), which decides whether its body runs once for the statement
// or again each time the subquery naming it runs. C codes an IN's subquery
// from a copy (sqlite3CodeRhsOfIN), and the copy counts its CTE references a
// second time; an EXISTS or a scalar subquery has no copy. A body calling
// random() shows which: one run gives every outer row the same value, so the
// rows an IN or EXISTS keeps hold at most one distinct t.b.
func TestCTEUseCountedAsC(t *testing.T) {
	schema := []string{
		`CREATE TABLE t(a INT, b INT)`, `CREATE TABLE u(c INT, d INT)`, `CREATE TABLE w(e INT)`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<700) INSERT INTO t SELECT v, v%7 FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<700) INSERT INTO u SELECT v, v%5 FROM g`,
		`WITH RECURSIVE g(v) AS (SELECT 1 UNION ALL SELECT v+1 FROM g WHERE v<20) INSERT INTO w SELECT v FROM g`,
	}
	c := `WITH c(x) AS (SELECT abs(random())%7 FROM w LIMIT 1) `
	cb := `WITH b(y) AS (SELECT abs(random())%7 FROM w LIMIT 1), c(x) AS (SELECT y FROM b) `
	one := `SELECT count(DISTINCT t.b) <= 1 FROM t, u WHERE t.a = u.c AND `
	inner := `WITH c(x) AS (SELECT abs(random())%7 FROM w LIMIT 1) `
	qs := []string{
		c + one + `t.b IN (SELECT x FROM c WHERE u.d >= 0)`,
		c + one + `t.b IN (SELECT x FROM c)`,
		c + one + `EXISTS (SELECT 1 FROM c WHERE x = t.b AND u.d >= 0)`,
		c + one + `t.b = (SELECT x FROM c WHERE u.d >= 0)`,
		c + `SELECT count(DISTINCT t.b) <= 1 FROM t WHERE t.b IN (SELECT x FROM c WHERE t.a >= 0)`,
		c + one + `t.b IN (SELECT x FROM c WHERE u.d >= 0) AND u.d IN (SELECT x FROM c WHERE t.a >= 0)`,
		`WITH c(x) AS NOT MATERIALIZED (SELECT abs(random())%7 FROM w LIMIT 1) ` + one + `t.b IN (SELECT x FROM c WHERE u.d >= 0)`,
		cb + one + `t.b IN (SELECT x FROM c WHERE u.d >= 0)`,
		cb + one + `EXISTS (SELECT 1 FROM c WHERE x = t.b AND u.d >= 0)`,
		one + `t.b IN (` + inner + `SELECT x FROM c WHERE u.d >= 0)`,
		one + `EXISTS (` + inner + `SELECT 1 FROM c WHERE x = t.b AND u.d >= 0)`,
		c + one + `t.b IN (SELECT e FROM w WHERE e IN (SELECT x FROM c WHERE u.d >= 0))`,
		c + one + `EXISTS (SELECT 1 FROM w WHERE e = t.b AND e IN (SELECT x FROM c WHERE u.d >= 0))`,
		c + `SELECT count(DISTINCT t.b) <= 1 FROM t WHERE t.b IN (SELECT x FROM c WHERE t.a >= 0) OR t.b IN (SELECT x + 100 FROM c)`,
		`WITH c(x) AS (SELECT e FROM w WHERE e%2=0) SELECT count(*) FROM t, u WHERE t.a = u.c AND t.b IN (SELECT x FROM c)`,
		`WITH c(x) AS (SELECT e FROM w) SELECT count(*) FROM t, u WHERE t.a = u.c AND t.b IN (SELECT x FROM c WHERE x > u.d)`,
		`WITH c(x) AS (SELECT e FROM w) SELECT count(*) FROM t, u, c WHERE t.a = u.c AND u.d = c.x AND t.b IN (SELECT x FROM c)`,
	}
	for _, q := range qs {
		differ(t, "cte-use-count", append(append([]string{}, schema...), q))
	}
}
