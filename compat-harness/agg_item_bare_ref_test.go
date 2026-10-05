// This file tests bare column references in subqueries within aggregate
// queries.
package compat

import "testing"

// aggBareRefSchema is shared by the anchor-row cases: t is the aggregate
// query's own table, u is what the bodies select from. The rows are the ones
// engine/vdbe_agg.go's own comment records having probed against C SQLite
// for the anchor-row census, so the min/max/count/FILTER cases below move the
// anchor to four different rows of the same group.
var aggBareRefSchema = []string{
	`CREATE TABLE t(a,b,c)`,
	`INSERT INTO t VALUES(1,2,'x'),(1,9,'y'),(1,4,'z'),(2,5,'p'),(2,7,'q')`,
	`CREATE TABLE u(k,v)`,
	`INSERT INTO u VALUES('x',10),('y',20),('z',30),('p',40),('q',50)`,
}

// TestAggItemBareRefAnchorRow pins that a BARE reference inside the body reads
// the same group ANCHOR ROW a bare column written at the item's own top level
// reads -- the row the min/max census moves, not "the first row".
func TestAggItemBareRefAnchorRow(t *testing.T) {
	flLockstep(t, "agg-bare-anchor", aggBareRefSchema,
		`SELECT a, max(b), (SELECT v FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, min(b), (SELECT v FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, count(*), (SELECT v FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, max(NULL), (SELECT v FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, max(b) FILTER (WHERE c='zzz'), (SELECT v FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		// The same bare name in EXISTS and IN bodies, and one level deeper.
		`SELECT a, max(b), EXISTS(SELECT 1 FROM u WHERE k = c) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, max(b), c IN (SELECT k FROM u WHERE v > 15) FROM t GROUP BY a ORDER BY a`,
		`SELECT a, max(b), (SELECT (SELECT v FROM u WHERE k = c)) FROM t GROUP BY a ORDER BY a`,
		// HAVING and ORDER BY carry itemPlans of their own.
		`SELECT a, max(b) FROM t GROUP BY a HAVING (SELECT v FROM u WHERE k = c) > 15 ORDER BY a`,
		`SELECT a, max(b) FROM t GROUP BY a ORDER BY (SELECT v FROM u WHERE k = c)`,
		// A bare reference to the anchor row's ROWID.
		`SELECT a, max(b), (SELECT count(*) FROM u WHERE u.rowid <= rowid) FROM t GROUP BY a ORDER BY a`,
	)
}

// TestAggItemBareRefInnerWins pins lookupName's ordering: a bare name the
// BODY's own FROM can answer binds THERE, never to the aggregate query, even
// though the aggregate query also has a column of that name. s1.c is negative
// and s2.c positive, so binding outward would answer 0 rows instead of 2.
func TestAggItemBareRefInnerWins(t *testing.T) {
	flLockstep(t, "agg-bare-inner-wins", []string{
		`CREATE TABLE s1(a, c)`,
		`INSERT INTO s1 VALUES(1,-5),(1,-6),(2,-7)`,
		`CREATE TABLE s2(c, w)`,
		`INSERT INTO s2 VALUES(1,100),(2,200)`,
	},
		`SELECT a, count(*), (SELECT sum(w) FROM s2 WHERE c > 0) FROM s1 GROUP BY a ORDER BY a`,
		`SELECT a, count(*), (SELECT sum(w) FROM s2 WHERE s1.c < 0 AND c > 0) FROM s1 GROUP BY a ORDER BY a`,
		// Two levels of body, the middle one supplying the name.
		`SELECT a, count(*), (SELECT (SELECT sum(w) FROM s2 x WHERE x.c = c) FROM s2 WHERE w = 100) FROM s1 GROUP BY a ORDER BY a`,
	)
}

// TestAggItemBareRefStopsAtOwner is the regression this whole mechanism has to
// keep passing, in the spelling that now COMPILES rather than declining: the
// aggregate query owns the name, so it must never reach the query enclosing it.
// aa.x is 2 and o.x is 5 over uu = 1..4, so the two readings are 2 and 0.
func TestAggItemBareRefStopsAtOwner(t *testing.T) {
	flLockstep(t, "agg-bare-stops-at-owner", []string{
		`CREATE TABLE o(x)`,
		`INSERT INTO o VALUES(5)`,
		`CREATE TABLE aa(x, y)`,
		`INSERT INTO aa VALUES(2, 1)`,
		`CREATE TABLE uu(k)`,
		`INSERT INTO uu VALUES(1),(2),(3),(4)`,
	},
		`SELECT (SELECT (SELECT count(*) FROM uu WHERE uu.k > x) FROM aa GROUP BY y) FROM o`,
		`SELECT (SELECT (SELECT count(*) FROM uu WHERE uu.k > aa.x) FROM aa GROUP BY y) FROM o`,
	)
}

// TestAggItemBareRefAmbiguous is the other half of that regression: a bare name
// TWO of the aggregate query's FROM items offer is "ambiguous column name" in
// C SQLite (resolve.c:784, reached because :703's "if( cnt ) break;" stops
// the outward walk the moment a level matched at all) -- NOT a resolution
// against the enclosing query, which also has the column and would answer.
func TestAggItemBareRefAmbiguous(t *testing.T) {
	flLockstep(t, "agg-bare-ambiguous", []string{
		`CREATE TABLE o2(cc)`,
		`INSERT INTO o2 VALUES(9)`,
		`CREATE TABLE j1(g, cc)`,
		`INSERT INTO j1 VALUES(1, 2)`,
		`CREATE TABLE j2(cc, z)`,
		`INSERT INTO j2 VALUES(3, 1)`,
		`CREATE TABLE uu2(k)`,
		`INSERT INTO uu2 VALUES(1),(2),(3),(4)`,
	},
		`SELECT (SELECT (SELECT count(*) FROM uu2 WHERE uu2.k > cc) FROM j1, j2 GROUP BY g) FROM o2`,
		`SELECT (SELECT (SELECT count(*) FROM uu2 WHERE uu2.k > j1.cc) FROM j1, j2 GROUP BY g) FROM o2`,
	)
	// The same ambiguity with the aggregate query at the TOP level, where there
	// is no enclosing query for a wrong resolution to reach: both engines owe
	// the rejection on its own terms rather than by falling off the end of a
	// scope chain.
	flLockstep(t, "agg-bare-ambiguous-top", []string{
		`CREATE TABLE k1(g, cc)`,
		`INSERT INTO k1 VALUES(1, 2)`,
		`CREATE TABLE k2(cc, z)`,
		`INSERT INTO k2 VALUES(3, 1)`,
		`CREATE TABLE k3(n)`,
		`INSERT INTO k3 VALUES(1),(2),(3)`,
	},
		`SELECT g, count(*), (SELECT count(*) FROM k3 WHERE n > cc) FROM k1, k2 GROUP BY g`,
	)
}

// TestAggItemBareRefCollationAndAffinity pins that the compiled read carries
// the column's DECLARED collation and affinity, which is what a comparison in
// the body ranks by. Both fixtures are ones this repo has already had a live
// wrong answer on through the analogous seam (compiler.affCtx's own record):
// a NOCASE column compared against a literal, and an INTEGER column compared
// against a TEXT column's value.
func TestAggItemBareRefCollationAndAffinity(t *testing.T) {
	flLockstep(t, "agg-bare-collation", []string{
		`CREATE TABLE c1(g, a TEXT COLLATE NOCASE)`,
		`INSERT INTO c1 VALUES(1,'abc')`,
	},
		`SELECT g, (SELECT 'ABC' = a) FROM c1 GROUP BY g`,
		`SELECT g, (SELECT a = 'ABC') FROM c1 GROUP BY g`,
		`SELECT g, (SELECT count(*) FROM c1 x WHERE x.a = a) FROM c1 GROUP BY g`,
	)
	flLockstep(t, "agg-bare-affinity", []string{
		`CREATE TABLE uu3(i INTEGER, g)`,
		`INSERT INTO uu3 VALUES(1, 7)`,
		`CREATE TABLE tt(t TEXT)`,
		`INSERT INTO tt VALUES('1.0')`,
	},
		`SELECT g, (SELECT count(*) FROM tt WHERE tt.t = i) FROM uu3 GROUP BY g`,
		`SELECT g, (SELECT count(*) FROM tt WHERE tt.t = uu3.i) FROM uu3 GROUP BY g`,
	)
}

// TestAggItemBareRefCompoundBody exercises a COMPOUND subquery body, which is
// the one shape that reaches outerSchemaCtx (vdbe_compound_codegen.go) -- the
// arm-output/collation walk that has to model the item compiler's register
// scopes now that they exist, or an arm loses every enclosing query's opinion.
func TestAggItemBareRefCompoundBody(t *testing.T) {
	flLockstep(t, "agg-bare-compound", []string{
		`CREATE TABLE cm(g, a TEXT COLLATE NOCASE)`,
		`INSERT INTO cm VALUES(1,'abc'),(1,'zz')`,
		`CREATE TABLE cn(b)`,
		`INSERT INTO cn VALUES('ABC')`,
	},
		`SELECT g, min(a), (SELECT b FROM cn WHERE a = b UNION SELECT 'q') FROM cm GROUP BY g`,
		`SELECT g, min(a), (SELECT b FROM cn WHERE a = b EXCEPT SELECT 'ABC') FROM cm GROUP BY g`,
		`SELECT g, min(a), (SELECT b FROM cn WHERE cm.a = b UNION SELECT 'q') FROM cm GROUP BY g`,
	)
}

// TestAggItemBareRefSubtype is the fixture the "narrowing that shipped a wrong
// answer" lesson demands: a value whose meaning depends on whether it still
// carries a JSON SUBTYPE, read out of the group's anchor row. A plain TEXT
// column carries none, so a fixture built from one could never create the
// condition -- json_each's own "value" column can.
func TestAggItemBareRefSubtype(t *testing.T) {
	flExprParity(t,
		`SELECT key, min(json_quote(value)) FROM json_each('[[7]]') GROUP BY key`,
		`SELECT key, count(*), (SELECT json_quote(value)) FROM json_each('[[7],[8]]') GROUP BY key`,
		`SELECT key, count(*), (SELECT json_type(value)) FROM json_each('[[7],[8]]') GROUP BY key`,
	)
	flLockstep(t, "agg-bare-subtype-col", []string{
		`CREATE TABLE jj(g, d)`,
		`INSERT INTO jj VALUES(1,'[1,2]'),(1,'[3]')`,
	},
		`SELECT g, max(d), (SELECT json_quote(d)) FROM jj GROUP BY g`,
		`SELECT g, max(d), (SELECT json_quote(json(d))) FROM jj GROUP BY g`,
	)
}

// TestAggItemBareRefJoinedAnchor covers the scope shapes
// aggAnchorRegScopesUsable decides between: a plain multi-table FROM (the
// register scopes ARE published) and a USING/NATURAL join (they are not,
// because resolveRowReg has no counterpart to resolveColumnEx's hidden-
// duplicate skip -- resolve.c:441-446 keeps the left-most table and leaves
// cnt at 1, where a by-name count would see two hits).
func TestAggItemBareRefJoinedAnchor(t *testing.T) {
	flLockstep(t, "agg-bare-join", []string{
		`CREATE TABLE p1(g, m)`,
		`INSERT INTO p1 VALUES(1,10),(1,11),(2,12)`,
		`CREATE TABLE p2(g2, n)`,
		`INSERT INTO p2 VALUES(1,100),(2,200)`,
		`CREATE TABLE p3(q)`,
		`INSERT INTO p3 VALUES(10),(100),(110)`,
	},
		`SELECT g, max(m), (SELECT count(*) FROM p3 WHERE q > n) FROM p1, p2 WHERE g=g2 GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT count(*) FROM p3 WHERE q > m) FROM p1, p2 WHERE g=g2 GROUP BY g ORDER BY g`,
	)
	flLockstep(t, "agg-bare-using-join", []string{
		`CREATE TABLE w1(g, m)`,
		`INSERT INTO w1 VALUES(1,10),(1,11),(2,12)`,
		`CREATE TABLE w2(g, n)`,
		`INSERT INTO w2 VALUES(1,100),(2,200)`,
		`CREATE TABLE w3(q)`,
		`INSERT INTO w3 VALUES(10),(100),(110)`,
	},
		`SELECT g, max(m), (SELECT count(*) FROM w3 WHERE q > n) FROM w1 JOIN w2 USING(g) GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT count(*) FROM w3 WHERE q > g) FROM w1 JOIN w2 USING(g) GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT count(*) FROM w3 WHERE q > n) FROM w1 LEFT JOIN w2 USING(g) GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT count(*) FROM w3 WHERE q > n) FROM w1 RIGHT JOIN w2 USING(g) GROUP BY g ORDER BY g`,
	)
}

// TestAggItemBareRefHardPositions pins the three body positions that were the
// last to be settled: a window function's projection, the body's own AGGREGATE
// argument, and a compound arm with no scope of its own. Each once stood under
// a REFUSED REGION (refusedRegion, engine/vdbe_agg_item_subst.go) where a
// substituted placeholder would have been read against a context that has no
// group at all -- which answers "no such column" where the oracle answers a
// value. What these cases pin is that the value each position produces now is
// the value the oracle produces.
func TestAggItemBareRefHardPositions(t *testing.T) {
	flLockstep(t, "agg-bare-hard-positions", []string{
		`CREATE TABLE r1(g, m)`,
		`INSERT INTO r1 VALUES(1,10),(1,11),(2,12)`,
		`CREATE TABLE r2(q)`,
		`INSERT INTO r2 VALUES(1),(2),(3)`,
	},
		// A window function in the body's own select list.
		`SELECT g, max(m), (SELECT sum(q + m) OVER () FROM r2 LIMIT 1) FROM r1 GROUP BY g ORDER BY g`,
		// A bare reference standing inside the body's own AGGREGATE argument.
		`SELECT g, max(m), (SELECT sum(q * m) FROM r2) FROM r1 GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT group_concat(q, m) FROM r2) FROM r1 GROUP BY g ORDER BY g`,
		`SELECT g, max(m), (SELECT count(*) FILTER (WHERE q > m) FROM r2) FROM r1 GROUP BY g ORDER BY g`,
		// A compound arm with no scope of its own.
		`SELECT g, max(m), (SELECT q FROM r2 WHERE q=1 UNION ALL SELECT m) FROM r1 GROUP BY g ORDER BY g`,
	)
}
