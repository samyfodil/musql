// IN and OR WHERE clauses drive index scans in index order.
// Test cases observe row order to confirm the plan is correct.
package compat

import "testing"

// r36aSchema has multiple index shapes and varied value types to cover different plans and orderings.
var r36aSchema = []string{
	`CREATE TABLE t(a INTEGER, b TEXT, c INTEGER, d TEXT COLLATE NOCASE)`,
	`CREATE INDEX ta ON t(a)`,
	`CREATE INDEX tbc ON t(b,c)`,
	`CREATE INDEX tdd ON t(d DESC)`,
	`INSERT INTO t VALUES(7,'gg',70,'GG'),(2,'bb',20,'bb'),(9,'ii',90,'II'),
	                     (4,'dd',40,'dd'),(1,'aa',10,'AA'),(6,'ff',60,'ff'),
	                     (3,'cc',30,'CC'),(8,'hh',80,'hh'),(5,'ee',50,'ee'),
	                     (NULL,'jj',NULL,'JJ'),(5,'ee',55,'ee'),(1.0,'aa',11,'aa')`,
	`CREATE TABLE u(x, y, z)`,
	`CREATE UNIQUE INDEX ux ON u(x)`,
	`INSERT INTO u VALUES(30,'c',3),(10,'a',1),(20,'b',2),(40,'d',4)`,
	`CREATE TABLE cov(p INTEGER, q INTEGER, r INTEGER)`,
	`CREATE INDEX covpqr ON cov(p,q,r)`,
	`INSERT INTO cov VALUES(3,30,300),(1,10,100),(2,20,200),(1,11,110),(2,21,210)`,
	`CREATE TABLE n(k INTEGER, v TEXT)`,
	`INSERT INTO n VALUES(3,'c3'),(1,'c1'),(2,'c2')`,
}

func r36a(t *testing.T, name, q string) {
	t.Helper()
	differ(t, name, append(append([]string{}, r36aSchema...), q))
}

// TestR36aInDrivesIndex: an IN over an indexed column, read back through every
// readout the plan is observable in.
func TestR36aInDrivesIndex(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"star", `SELECT * FROM t WHERE a IN (2,5,8)`},
		{"bare", `SELECT a,b FROM t WHERE a IN (2,5,8)`},
		{"covering", `SELECT a FROM t WHERE a IN (2,5,8)`},
		{"typeof", `SELECT a,typeof(a) FROM t WHERE a IN (1,5)`},
		{"quote", `SELECT quote(a),quote(b) FROM t WHERE a IN (1,5)`},
		{"rowid", `SELECT rowid,a FROM t WHERE a IN (2,5,8)`},
		{"group_concat", `SELECT group_concat(b,'-') FROM t WHERE a IN (2,5,8)`},
		{"group_concat of a", `SELECT group_concat(a) FROM t WHERE a IN (1,5,9)`},
		{"minmax", `SELECT min(a),max(a) FROM t WHERE a IN (2,5,8)`},
		{"bare magnet", `SELECT a,b,max(a) FROM t WHERE a IN (2,5,8)`},
		{"count", `SELECT count(*) FROM t WHERE a IN (2,5,8)`},
		{"limit", `SELECT a,b FROM t WHERE a IN (2,5,8) LIMIT 2`},
		{"limit offset", `SELECT a,b FROM t WHERE a IN (1,2,5,8) LIMIT 2 OFFSET 1`},
		{"distinct", `SELECT DISTINCT a FROM t WHERE a IN (1,5)`},
		{"group by", `SELECT a,count(*) FROM t WHERE a IN (1,5) GROUP BY a`},
		{"group by having", `SELECT a,count(*) FROM t WHERE a IN (1,5) GROUP BY a HAVING count(*)>=1`},
		{"order by tie", `SELECT a,b FROM t WHERE a IN (1,5) ORDER BY length(b) DESC`},
		{"order by desc", `SELECT a,b FROM t WHERE a IN (2,5,8) ORDER BY a DESC`},
		{"order by limit", `SELECT a,b FROM t WHERE a IN (2,5,8) ORDER BY a LIMIT 2`},

		// The ephemeral set is SORTED, so a list written out of order, with
		// duplicates, or of one element must all answer the same way.
		{"unsorted list", `SELECT a,b FROM t WHERE a IN (8,2,5)`},
		{"duplicate values", `SELECT a,b FROM t WHERE a IN (5,5,2)`},
		{"one value", `SELECT a,b FROM t WHERE a IN (5)`},
		{"with null", `SELECT a,b FROM t WHERE a IN (2,NULL,8)`},
		{"all miss", `SELECT a,b FROM t WHERE a IN (100,200)`},

		// Storage class and affinity: the values get the comparison's affinity
		// before they are sorted into the ephemeral index.
		{"text spellings", `SELECT a,b FROM t WHERE a IN ('2','5','8')`},
		{"real spellings", `SELECT a,b FROM t WHERE a IN (2.0,5.0)`},
		{"mixed classes", `SELECT a,b FROM t WHERE a IN (2,'5',8.0)`},
		{"int vs real key", `SELECT a,typeof(a),c FROM t WHERE a IN (1)`},
		{"expression values", `SELECT a,b FROM t WHERE a IN (1+1,10/2)`},

		// Other index shapes.
		{"desc index", `SELECT d,b FROM t WHERE d IN ('bb','ee','hh')`},
		{"nocase collation", `SELECT d,b FROM t WHERE d IN ('BB','EE','HH')`},
		{"two column leading", `SELECT b,c FROM t WHERE b IN ('bb','ee','hh')`},
		{"two column inner", `SELECT b,c FROM t WHERE b='ee' AND c IN (50,55)`},
		{"two column both in", `SELECT b,c FROM t WHERE b IN ('ee','bb') AND c IN (20,50,55)`},
		{"unique index", `SELECT * FROM u WHERE x IN (30,10)`},
		{"covering three col", `SELECT p,q FROM cov WHERE p IN (1,2)`},
		{"covering inner", `SELECT p,q,r FROM cov WHERE p IN (1,2) AND q IN (10,21)`},
		{"unindexed table", `SELECT k,v FROM n WHERE k IN (1,3)`},

		// GROUP BY over a DESCENDING index with IN clause.
		{"group by desc index", `SELECT d,count(*) FROM t WHERE d IN ('bb','ee','hh') GROUP BY d`},
		{"group by desc index having", `SELECT d,count(*) FROM t WHERE d IN ('bb','ee','hh') GROUP BY d HAVING count(*)>=1`},
		{"group by desc index limit", `SELECT d,count(*) FROM t WHERE d IN ('bb','ee','hh') GROUP BY d LIMIT 2`},
		{"group by desc index bare", `SELECT d,b FROM t WHERE d IN ('bb','ee','hh') GROUP BY d`},
		{"group by desc index concat", `SELECT d,group_concat(b) FROM t WHERE d IN ('bb','ee','hh') GROUP BY d`},
		{"group by eq pinned then in", `SELECT b,c,count(*) FROM t WHERE b='ee' AND c IN (50,55) GROUP BY c`},
		{"rowid in", `SELECT a,b FROM t WHERE rowid IN (5,2,9)`},
		{"rowid in with order", `SELECT a,b FROM t WHERE rowid IN (5,2,9) ORDER BY a LIMIT 2`},

		// Combined with other terms.
		{"plus another term", `SELECT a,b FROM t WHERE a IN (2,5,8) AND b<>'zz'`},
		{"plus an equality", `SELECT a,b,c FROM t WHERE a IN (5) AND b='ee'`},
		{"plus a range", `SELECT a,b FROM t WHERE a IN (2,5,8) AND c>25`},
		{"plus is not null", `SELECT a,b FROM t WHERE a IN (2,5,8) AND c IS NOT NULL`},
		{"two ins", `SELECT a,b,c FROM t WHERE a IN (2,5,8) AND c IN (20,50)`},
		{"nested and", `SELECT a,b FROM t WHERE (a IN (2,5)) AND (c>0)`},

		// NOT IN serves as a filter with no constraints on plan choice.
		{"not in", `SELECT a,b FROM t WHERE a NOT IN (5,2)`},
		{"not in covering", `SELECT a FROM t WHERE a NOT IN (5,2)`},
		{"not in plus eq", `SELECT a,b,c FROM t WHERE a NOT IN (5,2) AND b='gg'`},
	} {
		t.Run(c.name, func(t *testing.T) { r36a(t, c.name, c.q) })
	}
}

// TestR36aOrConvertsToIn tests OR clauses that convert to equivalent IN expressions.
func TestR36aOrConvertsToIn(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"star", `SELECT * FROM t WHERE a = 2 OR a = 8`},
		{"bare", `SELECT a,b FROM t WHERE a = 2 OR a = 8`},
		{"covering", `SELECT a FROM t WHERE a = 2 OR a = 8`},
		{"descending values", `SELECT a,b FROM t WHERE a = 8 OR a = 2`},
		{"three way", `SELECT a,b FROM t WHERE a = 8 OR a = 2 OR a = 5`},
		{"four way", `SELECT a,b FROM t WHERE a = 8 OR a = 2 OR a = 5 OR a = 1`},
		{"group_concat", `SELECT group_concat(b,'-') FROM t WHERE a = 2 OR a = 8`},
		{"minmax", `SELECT min(a),max(a) FROM t WHERE a = 2 OR a = 8`},
		{"bare magnet", `SELECT a,b,max(a) FROM t WHERE a = 2 OR a = 8`},
		{"limit", `SELECT a,b FROM t WHERE a = 2 OR a = 8 OR a = 5 LIMIT 2`},
		{"distinct", `SELECT DISTINCT a FROM t WHERE a = 1 OR a = 5`},
		{"group by", `SELECT a,count(*) FROM t WHERE a = 1 OR a = 5 GROUP BY a`},
		{"order by tie", `SELECT a,b FROM t WHERE a = 1 OR a = 5 ORDER BY length(b) DESC`},
		{"parenthesized", `SELECT a,b FROM t WHERE (a = 2 OR a = 8)`},
		{"plus another term", `SELECT a,b FROM t WHERE (a = 2 OR a = 8) AND b<>'zz'`},
		{"nested or", `SELECT a,b FROM t WHERE a = 2 OR (a = 8 OR a = 5)`},
		{"int and real", `SELECT a,typeof(a),c FROM t WHERE a = 1 OR a = 1.0`},
		{"text spellings", `SELECT a,b FROM t WHERE a = '2' OR a = '8'`},
		{"desc index", `SELECT d,b FROM t WHERE d = 'bb' OR d = 'hh'`},
		{"nocase collation", `SELECT d,b FROM t WHERE d = 'BB' OR d = 'HH'`},
		{"two column leading", `SELECT b,c FROM t WHERE b = 'bb' OR b = 'hh'`},
		{"two column inner", `SELECT b,c FROM t WHERE b='ee' AND (c = 50 OR c = 55)`},
		{"unique index", `SELECT * FROM u WHERE x = 30 OR x = 10`},
		{"covering", `SELECT p,q FROM cov WHERE p = 1 OR p = 2`},
		{"unindexed table", `SELECT k,v FROM n WHERE k = 1 OR k = 3`},
		{"group by desc index", `SELECT d,count(*) FROM t WHERE d = 'bb' OR d = 'hh' GROUP BY d`},
		{"group by desc index limit", `SELECT d,count(*) FROM t WHERE d = 'bb' OR d = 'hh' GROUP BY d LIMIT 2`},
		{"rowid or", `SELECT a,b FROM t WHERE rowid = 5 OR rowid = 2`},
		{"no match", `SELECT a,b FROM t WHERE a = 100 OR a = 200`},
		{"null side", `SELECT a,b FROM t WHERE a = 2 OR a = NULL`},
	} {
		t.Run(c.name, func(t *testing.T) { r36a(t, c.name, c.q) })
	}
}

// TestR36aInMultiTable tests IN and OR rewrites on joins.
func TestR36aInMultiTable(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"two table in", `SELECT t.a,u.x FROM t,u WHERE t.a IN (2,5) AND u.x IN (10,30)`},
		{"two table or", `SELECT t.a,u.x FROM t,u WHERE (t.a = 2 OR t.a = 5) AND u.x = 10`},
		{"join concat", `SELECT group_concat(t.b||'/'||u.y) FROM t,u WHERE t.a IN (2,5) AND u.x IN (10,30)`},
		{"join on in", `SELECT t.a,cov.q FROM t JOIN cov ON cov.p = t.a WHERE t.a IN (1,2)`},
		{"left join in", `SELECT t.a,u.x FROM t LEFT JOIN u ON u.z = t.a WHERE t.a IN (1,2,3)`},
		{"join limit", `SELECT t.a,u.x FROM t,u WHERE t.a IN (2,5) AND u.x IN (10,30) LIMIT 3`},
		{"join magnet", `SELECT t.a,u.y,max(t.c) FROM t,u WHERE t.a IN (2,5) AND u.x IN (10,30)`},
		{"three table", `SELECT group_concat(t.b) FROM t,u,n WHERE t.a IN (2,5) AND u.x = 10 AND n.k IN (1,2)`},
	} {
		t.Run(c.name, func(t *testing.T) { r36a(t, c.name, c.q) })
	}
}

// TestR36aOrStaysDeclined tests OR shapes that do not convert, using natural index selection.
func TestR36aOrStaysDeclined(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"two columns unindexed", `SELECT k,v FROM n WHERE k = 1 OR v = 'c3'`},
		{"or with range unindexed", `SELECT k,v FROM n WHERE k < 2 OR k > 2`},
		{"or of like", `SELECT k,v FROM n WHERE v LIKE 'c1' OR v LIKE 'c3'`},
	} {
		t.Run(c.name, func(t *testing.T) { r36a(t, c.name, c.q) })
	}
}
