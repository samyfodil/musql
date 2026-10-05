package compat

import "testing"

// Bare column references in aggregate subquery bodies.
func TestUnqualifiedAggBodyRefMatchesCSQLite(t *testing.T) {
	setup := []string{
		`CREATE TABLE w(v,q)`, `INSERT INTO w VALUES(1,9)`,
		`CREATE TABLE t(g)`, `INSERT INTO t VALUES(1)`,
		`CREATE TABLE u(k)`, `INSERT INTO u VALUES(2),(3)`,
	}
	flLockstep(t, "bare v in an agg item's subquery body", setup,
		`SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > v) FROM t GROUP BY g) FROM w`)
	flLockstep(t, "qualified w.v twin", setup,
		`SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > w.v) FROM t GROUP BY g) FROM w`)
	flLockstep(t, "EXISTS spelling", setup,
		`SELECT (SELECT EXISTS(SELECT 1 FROM u WHERE u.k > v) FROM t GROUP BY g) FROM w`)

	// The WRITE: a wrong value reaching the file is invariant 2's worst case.
	flLockstep(t, "bare v persisted by an UPDATE", append(append([]string{}, setup...),
		`UPDATE w SET q = (SELECT (SELECT count(*) FROM u WHERE u.k > v) FROM t GROUP BY g)`),
		`SELECT v,q FROM w`)

	// USING-join variant.
	flLockstep(t, "bare x through a USING join", []string{
		`CREATE TABLE o(x)`, `INSERT INTO o VALUES(100)`,
		`CREATE TABLE a(x,y)`, `INSERT INTO a VALUES(1,1)`,
		`CREATE TABLE b(x)`, `INSERT INTO b VALUES(1)`,
		`CREATE TABLE u(k)`, `INSERT INTO u VALUES(2),(3)`,
	}, `SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > x) FROM a JOIN b USING(x) GROUP BY y) FROM o`)

	// Ambiguity: C SQLite raises, so an ANSWER here is a wrong answer.
	flLockstep(t, "ambiguous bare c", []string{
		`CREATE TABLE o2(c)`, `INSERT INTO o2 VALUES(100)`,
		`CREATE TABLE t1(c,g)`, `INSERT INTO t1 VALUES(5,1)`,
		`CREATE TABLE t2(c)`, `INSERT INTO t2 VALUES(7)`,
		`CREATE TABLE u(k)`, `INSERT INTO u VALUES(2),(3)`,
	}, `SELECT (SELECT (SELECT count(*) FROM u WHERE u.k > c) FROM t1,t2 GROUP BY g) FROM o2`)
}
