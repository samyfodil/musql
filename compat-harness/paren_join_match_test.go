package compat

import (
	"fmt"
	"testing"
)

// TestParenJoinLeadingMatch: a parenthesized join that leads its FROM clause,
// unaliased, is spliced in flat by C (parse.y's seltablist rule, the pSrc==0
// arm), so an fts4 MATCH in an outer join's ON inside it is an ordinary fts
// constraint. Statements whose row order C leaves to its planner carry an
// ORDER BY.
func TestParenJoinLeadingMatch(t *testing.T) {
	pre := []string{
		"CREATE VIRTUAL TABLE t10 USING fts4(c)",
		"INSERT INTO t10 VALUES('one'),('two'),('one two')",
		"CREATE TABLE q(term)", "INSERT INTO q VALUES('one'),('zzz'),('two')",
		"CREATE TABLE r(k)", "INSERT INTO r VALUES(1),(2)",
	}
	for i, s := range []string{
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term)",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term) ORDER BY 1, 2",
		"SELECT q.term, t10.rowid FROM (q LEFT JOIN t10 ON t10 MATCH q.term) ORDER BY 1, 2",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term) JOIN r ON r.k = 1",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term) WHERE t10.c IS NULL",
		"SELECT * FROM (q JOIN t10 ON t10 MATCH q.term)",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH 'one')",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term), r ORDER BY 1, 2, 3",
		"SELECT count(*) FROM (q LEFT JOIN t10 ON t10 MATCH q.term)",
		"SELECT * FROM ((q LEFT JOIN t10 ON t10 MATCH q.term) JOIN r) ORDER BY 1, 2, 3",
		"SELECT * FROM (r LEFT JOIN q ON q.rowid = r.k) LEFT JOIN t10 ON t10 MATCH q.term",
		"SELECT * FROM (r LEFT JOIN q ON q.rowid = r.k)",
		"SELECT * FROM (r AS a LEFT JOIN r AS b ON a.k = b.k + 1)",
		"SELECT * FROM (r AS a LEFT JOIN r AS b ON a.k = b.k + 1) JOIN q ON q.rowid = a.k",
		"SELECT a.k, b.k FROM (r AS a LEFT JOIN r AS b ON a.k = b.k + 1) LEFT JOIN r AS c ON c.k = b.k",
		"SELECT rowid FROM (r LEFT JOIN q ON 0)",
		"SELECT * FROM (r NATURAL LEFT JOIN q)",
		"SELECT * FROM (r LEFT JOIN q ON q.rowid = r.k) AS g",
		"SELECT * FROM (r LEFT JOIN q ON q.rowid = r.k), (r AS r2 LEFT JOIN q AS q2 ON q2.rowid = r2.k) ORDER BY 1, 2, 3, 4",
	} {
		differ(t, fmt.Sprintf("leading-%d", i), append(append([]string(nil), pre...), s))
	}
	// A group that does not lead, or is aliased, is an SF_NestedFrom subquery
	// in C: flattened into an inner join, its MATCH an ordinary fts
	// constraint again; the right operand of an outer join, a subquery of
	// its own.
	for i, s := range []string{
		"SELECT * FROM r JOIN (q LEFT JOIN t10 ON t10 MATCH q.term) ON 1",
		"SELECT * FROM (q LEFT JOIN t10 ON t10 MATCH q.term) AS g",
		"SELECT g.term, g.c FROM (q LEFT JOIN t10 ON t10 MATCH q.term) AS g ORDER BY 1, 2",
		"SELECT * FROM r, (q LEFT JOIN t10 ON t10 MATCH q.term) ORDER BY 1, 2, 3",
		"SELECT * FROM r JOIN (q LEFT JOIN t10 ON t10 MATCH q.term) ON t10.c IS NULL",
		"SELECT * FROM r JOIN (q JOIN t10 ON t10 MATCH q.term) AS g ON g.term = 'one' ORDER BY 1, 2, 3",
		"SELECT * FROM r LEFT JOIN (q LEFT JOIN t10 ON t10 MATCH q.term) ON r.k = 1",
		"SELECT * FROM r LEFT JOIN (q JOIN t10 ON t10 MATCH q.term) ON r.k = 1",
		"SELECT * FROM r LEFT JOIN (q JOIN t10 ON t10 MATCH q.term) AS g ON r.k = 1",
		"SELECT r.k, g.term, t10.rowid FROM r LEFT JOIN (q JOIN t10 ON t10 MATCH q.term) AS g ON r.k = 1 ORDER BY 1, 2, 3",
		"SELECT k, (SELECT count(*) FROM q AS z WHERE z.term = g.term) FROM r LEFT JOIN (q JOIN t10 ON t10 MATCH q.term) AS g ON r.k = g.rowid ORDER BY 1, 2",
		"SELECT count(*) FROM r LEFT JOIN (q LEFT JOIN t10 ON t10 MATCH q.term) ON r.k = 1 WHERE c IS NULL",
	} {
		differ(t, fmt.Sprintf("nested-%d", i), append(append([]string(nil), pre...), s))
	}
}
