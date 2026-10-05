// This file tests BETWEEN in WHERE clauses to ensure index-driven queries
// return results in the correct order.
package compat

import "testing"

// r34xRangeSchema defines test tables with various index types.
var r34xRangeSchema = []string{
	`CREATE TABLE t(a INTEGER, b TEXT, c INTEGER, d TEXT COLLATE NOCASE)`,
	`CREATE INDEX ta ON t(a)`,
	`CREATE INDEX tbc ON t(b,c)`,
	`CREATE INDEX tdd ON t(d DESC)`,
	`INSERT INTO t VALUES(7,'gg',70,'GG'),(2,'bb',20,'bb'),(9,'ii',90,'II'),
	                     (4,'dd',40,'dd'),(1,'aa',10,'AA'),(6,'ff',60,'ff'),
	                     (3,'cc',30,'CC'),(8,'hh',80,'hh'),(5,'ee',50,'ee'),
	                     (NULL,'jj',NULL,'JJ'),(5,'ee',55,'ee')`,
	`CREATE TABLE u(x INTEGER, y TEXT, lo INTEGER, hi INTEGER)`,
	`CREATE INDEX ux ON u(x)`,
	`INSERT INTO u VALUES(5,'e',4,6),(1,'a',0,2),(3,'c',2,4),(2,'b',1,3),(4,'d',3,5)`,
	`CREATE TABLE n(k INTEGER, v TEXT)`,
	`INSERT INTO n VALUES(3,'c3'),(1,'c1'),(2,'c2')`,
}

func r34xRange(t *testing.T, name, q string) {
	t.Helper()
	differ(t, name, append(append([]string{}, r34xRangeSchema...), q))
}

// TestR34XBetweenDrivesIndex tests BETWEEN conditions with various readout methods.
func TestR34XBetweenDrivesIndex(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"bare columns", `SELECT a,b,c FROM t WHERE a BETWEEN 2 AND 8`},
		{"star", `SELECT * FROM t WHERE a BETWEEN 2 AND 8`},
		{"covering", `SELECT a FROM t WHERE a BETWEEN 2 AND 8`},
		{"typeof readout", `SELECT typeof(a),quote(b) FROM t WHERE a BETWEEN 2 AND 8`},
		{"cast readout", `SELECT CAST(a AS TEXT)||'/'||b FROM t WHERE a BETWEEN 2 AND 8`},
		{"group_concat", `SELECT group_concat(b,'-') FROM t WHERE a BETWEEN 2 AND 8`},
		{"group_concat of a", `SELECT group_concat(a) FROM t WHERE a BETWEEN 2 AND 8`},
		{"limit", `SELECT a,b FROM t WHERE a BETWEEN 2 AND 8 LIMIT 3`},
		{"limit offset", `SELECT a,b FROM t WHERE a BETWEEN 2 AND 8 LIMIT 3 OFFSET 2`},
		{"distinct", `SELECT DISTINCT a FROM t WHERE a BETWEEN 2 AND 8`},
		{"two column index", `SELECT b,c FROM t WHERE b BETWEEN 'bb' AND 'hh'`},
		{"two column index inner", `SELECT b,c FROM t WHERE b='ee' AND c BETWEEN 20 AND 60`},
		{"desc index", `SELECT d FROM t WHERE d BETWEEN 'bb' AND 'hh'`},
		{"nocase collation", `SELECT d,b FROM t WHERE d BETWEEN 'BB' AND 'HH'`},
		{"explicit collate", `SELECT b FROM t WHERE b BETWEEN 'bb' COLLATE NOCASE AND 'hh'`},
		{"not between", `SELECT a,b FROM t WHERE a NOT BETWEEN 2 AND 8`},
		{"reversed bounds", `SELECT a,b FROM t WHERE a BETWEEN 8 AND 2`},
		{"equal bounds", `SELECT a,b FROM t WHERE a BETWEEN 5 AND 5`},
		{"null bound", `SELECT a,b FROM t WHERE a BETWEEN NULL AND 8`},
		{"text bounds on int", `SELECT a,b FROM t WHERE a BETWEEN '2' AND '8'`},
		{"real bounds", `SELECT a,b FROM t WHERE a BETWEEN 2.5 AND 7.5`},
		{"rowid bounds", `SELECT a FROM t WHERE rowid BETWEEN 3 AND 6`},
		{"expression bound", `SELECT a,b FROM t WHERE a BETWEEN 1+1 AND 4*2`},
		{"plus another term", `SELECT a,b FROM t WHERE a BETWEEN 2 AND 8 AND b<>'zz'`},
		{"plus an equality", `SELECT a,b,c FROM t WHERE a BETWEEN 2 AND 8 AND b='ee'`},
		{"plus is not null", `SELECT a,b FROM t WHERE a BETWEEN 2 AND 8 AND c IS NOT NULL`},
		{"two betweens", `SELECT a,b,c FROM t WHERE a BETWEEN 2 AND 8 AND c BETWEEN 20 AND 60`},
		{"nested and", `SELECT a,b FROM t WHERE (a BETWEEN 2 AND 8) AND (c>0)`},
		{"unindexed column", `SELECT k,v FROM n WHERE k BETWEEN 1 AND 3`},
		{"aggregate magnet", `SELECT count(*),min(a),max(a),sum(c) FROM t WHERE a BETWEEN 2 AND 8`},
		{"group by", `SELECT b,count(*) FROM t WHERE a BETWEEN 2 AND 8 GROUP BY b`},
		{"bare column in group", `SELECT b,c,count(*) FROM t WHERE a BETWEEN 2 AND 8 GROUP BY b`},
		{"having", `SELECT b,count(*) FROM t WHERE a BETWEEN 2 AND 8 GROUP BY b HAVING count(*)>0`},
	} {
		r34xRange(t, "r34x between "+c.name, c.q)
	}
}

// TestR34XBetweenJoinsAndSubqueries puts the rewrite where the loop ORDER is
// what it decides, not just one table's access path.
func TestR34XBetweenJoinsAndSubqueries(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"comma join", `SELECT t.a,u.y FROM t,u WHERE t.a=u.x AND t.a BETWEEN 2 AND 4`},
		{"inner join", `SELECT t.a,u.y FROM t JOIN u ON t.a=u.x WHERE t.a BETWEEN 2 AND 4`},
		{"between in the on clause", `SELECT t.a,u.y FROM t JOIN u ON t.a=u.x AND t.a BETWEEN 2 AND 4`},
		{"left join on", `SELECT t.a,u.y FROM t LEFT JOIN u ON t.a=u.x AND t.a BETWEEN 2 AND 4`},
		{"left join where", `SELECT t.a,u.y FROM t LEFT JOIN u ON t.a=u.x WHERE t.a BETWEEN 2 AND 4`},
		{"column bounds", `SELECT u.x,t.a FROM u,t WHERE t.a BETWEEN u.lo AND u.hi`},
		{"column bounds joined", `SELECT u.x,t.a FROM u,t WHERE t.a=u.x AND t.a BETWEEN u.lo AND u.hi`},
		{"three tables", `SELECT t.a,u.y,n.v FROM t,u,n WHERE t.a=u.x AND u.x=n.k AND t.a BETWEEN 1 AND 3`},
		{"other side between", `SELECT t.a,u.y FROM t,u WHERE t.a=u.x AND u.x BETWEEN 2 AND 4`},
		{"derived table", `SELECT * FROM (SELECT a,b FROM t WHERE a BETWEEN 2 AND 8)`},
		{"compound", `SELECT a FROM t WHERE a BETWEEN 2 AND 4 UNION ALL SELECT x FROM u WHERE x BETWEEN 1 AND 2`},
		{"cte", `WITH s AS (SELECT a,b FROM t WHERE a BETWEEN 2 AND 8) SELECT * FROM s`},
	} {
		r34xRange(t, "r34x between "+c.name, c.q)
	}
}

// TestR34XSubqueryScanOrderClosed pins what TestR34XSubqueryScanOrderOpen
// used to track as open: this engine's rowid-order scan vs the oracle's index
// walk, for a table read inside a subquery's own FROM. Each case is paired with
// the identical query spelled ">= AND <=" instead, which diverged the same way
// -- the evidence that the gap was the subquery's scan order and not the
// BETWEEN rewrite. The scalar-subquery pair closed first; the EXISTS pair now
// agrees too (at 030f87c9, answering x = 2,3,4 in ux order where rowid order
// is 3,2,4), so per this file's r32oKnownOpen convention ("no longer diverges
// -- delete this case") the open test is gone and all four are asserted here.
func TestR34XSubqueryScanOrderClosed(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"scalar subquery, BETWEEN", `SELECT (SELECT group_concat(a) FROM t WHERE a BETWEEN 2 AND 4)`},
		{"scalar subquery, >= AND <=", `SELECT (SELECT group_concat(a) FROM t WHERE a>=2 AND a<=4)`},
		{"exists, BETWEEN", `SELECT x FROM u WHERE EXISTS(SELECT 1 FROM t WHERE t.a=u.x AND t.a BETWEEN 2 AND 4)`},
		{"exists, >= AND <=", `SELECT x FROM u WHERE EXISTS(SELECT 1 FROM t WHERE t.a=u.x AND t.a>=2 AND t.a<=4)`},
	} {
		if !differ(t, "r34x "+c.name, append(append([]string{}, r34xRangeSchema...), c.q)) {
			t.Errorf("diverged on: %s", c.q)
		}
	}
}

// TestR34XBetweenWrites is the DML half: the same rewrite decides which rows an
// UPDATE/DELETE visits and in which order, which a RETURNING clause exposes.
func TestR34XBetweenWrites(t *testing.T) {
	for _, c := range []struct {
		name  string
		stmts []string
	}{
		{"update", []string{
			`UPDATE t SET c=c+1 WHERE a BETWEEN 2 AND 8`,
			`SELECT a,c FROM t`,
		}},
		{"update returning", []string{
			`UPDATE t SET c=c+1 WHERE a BETWEEN 2 AND 8 RETURNING a,c`,
		}},
		{"delete", []string{
			`DELETE FROM t WHERE a BETWEEN 2 AND 8`,
			`SELECT a,b FROM t`,
		}},
		{"delete returning", []string{
			`DELETE FROM t WHERE a BETWEEN 2 AND 8 RETURNING a,b`,
		}},
		{"insert select", []string{
			`CREATE TABLE dst(p,q)`,
			`INSERT INTO dst SELECT a,b FROM t WHERE a BETWEEN 2 AND 8`,
			`SELECT rowid,p,q FROM dst`,
		}},
	} {
		differ(t, "r34x between "+c.name, append(append([]string{}, r34xRangeSchema...), c.stmts...))
	}
}
