// This file tests correlated subqueries inside UPDATE/DELETE statements,
// covering per-row answers, frame reads, compound bodies, NULL and collation
// semantics, and edge cases like ATTACH and WITHOUT ROWID targets.
package compat

import "testing"

// TestWriteCorrelatedUpdateShapes: correlated subqueries in an UPDATE's SET.
func TestWriteCorrelatedUpdateShapes(t *testing.T) {
	// Row 3 has no match; subquery returns NULL.
	differ(t, "update set correlated over another table", []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t2(x,y)`,
		`INSERT INTO t1 VALUES(1,10),(2,20),(3,30)`,
		`INSERT INTO t2 VALUES(1,100),(2,200)`,
		`UPDATE t1 SET b=b+(SELECT y FROM t2 WHERE x=a)`,
		`SELECT a,b FROM t1 ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// Count must be per-row.
	differ(t, "update set correlated count is per row", []string{
		`CREATE TABLE p1(a,b)`,
		`CREATE TABLE p2(x)`,
		`INSERT INTO p1 VALUES(1,-1),(2,-1),(3,-1)`,
		`INSERT INTO p2 VALUES(1),(2)`,
		`UPDATE p1 SET b=(SELECT count(*) FROM p2 WHERE x < p1.a)`,
		`SELECT a,b FROM p1 ORDER BY a`,
	})
	// FROM-less body uses only the outer frame.
	differ(t, "update set correlated from-less body", []string{
		`CREATE TABLE c1(a,b)`,
		`INSERT INTO c1 VALUES('ABC','x'),('def','y'),(NULL,'z')`,
		`UPDATE c1 SET b=(SELECT CASE WHEN c1.a='ABC' THEN 'HIT' ELSE b END)`,
		`SELECT a,b FROM c1 ORDER BY a`,
	})
	// Outer rowid pseudo-column.
	differ(t, "update set correlated on the outer rowid", []string{
		`CREATE TABLE r1(a,b)`,
		`CREATE TABLE r2(k,v)`,
		`INSERT INTO r1 VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO r2 VALUES(1,'p'),(2,'q')`,
		`UPDATE r1 SET b=(SELECT v FROM r2 WHERE r2.k = r1.rowid)`,
		`SELECT a,b FROM r1 ORDER BY a`,
	})
	// Reference two frames out via nested EXISTS.
	differ(t, "update set correlated two frames out", []string{
		`CREATE TABLE l1(a,b)`,
		`CREATE TABLE l2(x,y)`,
		`CREATE TABLE l3(p,q)`,
		`INSERT INTO l1 VALUES(1,0),(2,0)`,
		`INSERT INTO l2 VALUES(1,10),(2,20)`,
		`INSERT INTO l3 VALUES(10,'A'),(20,'B')`,
		`UPDATE l1 SET b=(SELECT count(*) FROM l2 WHERE EXISTS(SELECT 1 FROM l3 WHERE l3.p=l2.y AND l1.a=l2.x))`,
		`SELECT a,b FROM l1 ORDER BY a`,
	})
	// NULL semantics: "=" vs "IS".
	differ(t, "update set correlated NULL comparison", []string{
		`CREATE TABLE nl(a,b)`,
		`CREATE TABLE ns(k,v)`,
		`INSERT INTO nl VALUES(1,0),(2,0),(NULL,0)`,
		`INSERT INTO ns VALUES(1,'p'),(NULL,'n')`,
		`UPDATE nl SET b=(SELECT v FROM ns WHERE ns.k=nl.a)`,
		`SELECT a,b FROM nl ORDER BY a IS NULL, a`,
		`UPDATE nl SET b=(SELECT v FROM ns WHERE ns.k IS nl.a)`,
		`SELECT a,b FROM nl ORDER BY a IS NULL, a`,
	})
	// Target column collation crosses frame.
	differ(t, "update set correlated carries the outer collation", []string{
		`CREATE TABLE nc(a TEXT COLLATE NOCASE, b)`,
		`CREATE TABLE nk(t TEXT)`,
		`INSERT INTO nc VALUES('Foo',0),('BAR',0)`,
		`INSERT INTO nk VALUES('foo')`,
		`UPDATE nc SET b=(SELECT count(*) FROM nk WHERE t = nc.a)`,
		`SELECT a,b FROM nc ORDER BY a`,
	})
	// WITHOUT ROWID target.
	differ(t, "update set correlated on a WITHOUT ROWID target", []string{
		`CREATE TABLE wr2(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`CREATE TABLE wsrc(a TEXT, n)`,
		`INSERT INTO wr2 VALUES('p',0),('q',0)`,
		`INSERT INTO wsrc VALUES('p',7),('q',9)`,
		`UPDATE wr2 SET b=(SELECT n FROM wsrc WHERE wsrc.a = wr2.a)`,
		`SELECT a,b FROM wr2 ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// CTE scope.
	differ(t, "update set correlated through a CTE", []string{
		`CREATE TABLE w1(a,b)`,
		`CREATE TABLE w2(k,v)`,
		`INSERT INTO w1 VALUES(1,0),(2,0)`,
		`INSERT INTO w2 VALUES(1,'q'),(2,'r')`,
		`WITH cc AS (SELECT k,v FROM w2) UPDATE w1 SET b=(SELECT v FROM cc WHERE cc.k=w1.a)`,
		`SELECT a,b FROM w1 ORDER BY a`,
	})
	// Through a view.
	differ(t, "update set correlated through a view", []string{
		`CREATE TABLE v1(a,b)`,
		`CREATE TABLE u(p,q,r)`,
		`INSERT INTO v1 VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO u VALUES(1,'x',9),(2,'y',8),(3,'z',7),(1,'w',6)`,
		`CREATE VIEW uv AS SELECT p,q,r FROM u`,
		`UPDATE v1 SET b=(SELECT count(*) FROM uv WHERE p<=v1.a)`,
		`SELECT a,b FROM v1 ORDER BY a`,
	})
	// Generated column on target.
	differ(t, "update set correlated on a generated column", []string{
		`CREATE TABLE g(a, b, c AS (a*2))`,
		`CREATE TABLE gs(k,v)`,
		`INSERT INTO g(a,b) VALUES(1,0),(2,0)`,
		`INSERT INTO gs VALUES(1,'p'),(2,'q')`,
		`UPDATE g SET b=(SELECT v FROM gs WHERE gs.k=g.c/2)`,
		`SELECT a,b,c FROM g ORDER BY a`,
	})
	// Correlated value violates CHECK.
	differ(t, "update set correlated violates a CHECK", []string{
		`CREATE TABLE ck(a, b CHECK(b <> 'bad'))`,
		`CREATE TABLE cs(k,v)`,
		`INSERT INTO ck VALUES(1,'ok'),(2,'ok')`,
		`INSERT INTO cs VALUES(1,'fine'),(2,'bad')`,
		`UPDATE ck SET b=(SELECT v FROM cs WHERE cs.k=ck.a)`,
		`SELECT a,b FROM ck ORDER BY a`,
	})
	// UPDATE OR REPLACE with correlated identity.
	differ(t, "update or replace set correlated identity", []string{
		`CREATE TABLE cr(a UNIQUE, b)`,
		`CREATE TABLE crs(k,v)`,
		`INSERT INTO cr VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO crs VALUES(1,2),(2,3),(3,4)`,
		`UPDATE OR REPLACE cr SET a=(SELECT v FROM crs WHERE crs.k=cr.a)`,
		`SELECT a,b FROM cr ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// Trigger writes source table alongside correlated read.
	differ(t, "update set correlated beside a trigger writing the source", []string{
		`CREATE TABLE trg(a,b)`,
		`CREATE TABLE src(k,v)`,
		`INSERT INTO trg VALUES(1,0),(2,0)`,
		`INSERT INTO src VALUES(1,'m'),(2,'n')`,
		`CREATE TRIGGER trb BEFORE UPDATE ON trg BEGIN INSERT INTO src VALUES(99,'zz'); END`,
		`UPDATE trg SET b=(SELECT count(*) FROM src WHERE src.k<=trg.a)`,
		`SELECT a,b FROM trg ORDER BY a`,
		`SELECT k,v FROM src ORDER BY k,v`,
	})
	// Row-value SET from correlated subquery.
	differ(t, "update set row-value from a correlated subquery", []string{
		`CREATE TABLE t1(a,b,c)`,
		`CREATE TABLE t2(x,y,z)`,
		`INSERT INTO t1 VALUES(1,0,0),(2,0,0),(3,0,0)`,
		`INSERT INTO t2 VALUES(1,'p','P'),(2,'q','Q')`,
		`UPDATE t1 SET (b,c) = (SELECT y,z FROM t2 WHERE x=t1.a)`,
		`SELECT a,b,c FROM t1 ORDER BY a`,
	})
	// COLLATE and NOCASE across frame.
	differ(t, "update where correlated comparison collation", []string{
		`CREATE TABLE cc(a,b COLLATE NOCASE,c)`,
		`INSERT INTO cc VALUES('ABC','AbC','xy'),('ABC ','zz','pq')`,
		`UPDATE cc SET b='HIT' WHERE (SELECT 'ABC ' COLLATE RTRIM = cc.a)`,
		`SELECT a,b,c FROM cc ORDER BY a`,
		`UPDATE cc SET c='H2' WHERE (SELECT cc.a='ABC ' COLLATE RTRIM)`,
		`SELECT a,b,c FROM cc ORDER BY a`,
		`UPDATE cc SET c='H3' WHERE (SELECT cc.b='hit')`,
		`SELECT a,b,c FROM cc ORDER BY a`,
	})
	// The outer ROWID compared against a TEXT column, and an unqualified
	// correlated name that only the outer scope can resolve.
	differ(t, "update where correlated outer rowid and bare name", []string{
		`CREATE TABLE o6(i,t,n,r)`,
		`CREATE TABLE inn(i,t)`,
		`INSERT INTO o6 VALUES('1.0','a',1,1.0),('2','b',2,2.0)`,
		`INSERT INTO inn VALUES(1,'1.0'),(2,'2')`,
		`UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o6.rowid)`,
		`SELECT i,t,n,r FROM o6 ORDER BY i`,
		`UPDATE o6 SET t='H2' WHERE (SELECT o6.i IN (SELECT t FROM inn))`,
		`SELECT i,t,n,r FROM o6 ORDER BY i`,
		`DELETE FROM o6 WHERE (SELECT o6.i='1.0')`,
		`SELECT i,t,n,r FROM o6 ORDER BY i`,
	})
	// A correlated reference the INNER query could have resolved locally if the
	// scopes were searched in the wrong order.
	differ(t, "update where correlated bare name resolves outward", []string{
		`CREATE TABLE p1(a,b)`,
		`CREATE TABLE p2(z)`,
		`INSERT INTO p1 VALUES(1,0),(2,0)`,
		`INSERT INTO p2 VALUES(1)`,
		`UPDATE p1 SET b='HIT' WHERE EXISTS (SELECT 1 FROM p2 WHERE a=z)`,
		`SELECT a,b FROM p1 ORDER BY a`,
	})
	// A correlated ORDER BY / GROUP BY key inside the sub-select.
	differ(t, "update set correlated ORDER BY and GROUP BY keys", []string{
		`CREATE TABLE ord(a,k)`,
		`CREATE TABLE two(x)`,
		`INSERT INTO ord VALUES(1,0),(2,0)`,
		`INSERT INTO two VALUES(7),(8)`,
		`UPDATE ord SET k=(SELECT x FROM two GROUP BY ord.a)`,
		`SELECT a,k FROM ord ORDER BY a`,
		`UPDATE ord SET k=(SELECT x FROM two ORDER BY ord.a LIMIT 1)`,
		`SELECT a,k FROM ord ORDER BY a`,
	})
	// An ATTACHed source. Page numbers are file-local, so a correlated read
	// whose sub-program opens a foreign pager is its own risk.
	differ(t, "update set correlated over an attached database", []string{
		`CREATE TABLE dst(id,v)`,
		`INSERT INTO dst VALUES(1,10),(2,20)`,
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.ref(id,bump)`,
		`INSERT INTO aux.ref VALUES(1,5),(2,6)`,
		`UPDATE dst SET v = v + COALESCE((SELECT bump FROM aux.ref WHERE ref.id = dst.id),0)`,
		`SELECT id,v FROM dst ORDER BY id`,
		`DETACH aux`,
	})
}

// TestWriteCorrelatedWhereShapes tests correlated subqueries in UPDATE and
// DELETE WHERE clauses.
func TestWriteCorrelatedWhereShapes(t *testing.T) {
	differ(t, "delete where EXISTS correlated through an alias", []string{
		`CREATE TABLE t11(a,b)`,
		`INSERT INTO t11 VALUES(1,5),(2,4),(3,9)`,
		`DELETE FROM t11 AS xyz WHERE EXISTS(SELECT 1 FROM t11 WHERE t11.a>xyz.a AND t11.b<=xyz.b)`,
		`SELECT a,b FROM t11 ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "delete where correlated IN over another table", []string{
		`CREATE TABLE n1(k,v)`,
		`CREATE TABLE n2(k,w)`,
		`INSERT INTO n1 VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO n2 VALUES(1,10),(2,20)`,
		`DELETE FROM n1 WHERE k IN (SELECT k FROM n2 WHERE n2.w > n1.k)`,
		`SELECT k,v FROM n1 ORDER BY k`,
	})
	// A COMPOUND correlated body: execWithParent's prog.Compound arm hands the
	// parent frame to each arm directly rather than wrapping it in a level of
	// its own, so P5 must still be 1 inside every arm.
	differ(t, "update where correlated compound body", []string{
		`CREATE TABLE c1(a,b)`,
		`INSERT INTO c1 VALUES('ABC','x'),('def','y')`,
		`UPDATE c1 SET b='HIT' WHERE EXISTS (SELECT c1.a INTERSECT SELECT 'ABC')`,
		`SELECT a,b FROM c1 ORDER BY a`,
	})
	differ(t, "update where correlated from-less body", []string{
		`CREATE TABLE c1(a,b)`,
		`INSERT INTO c1 VALUES('ABC','x'),('def','y')`,
		`UPDATE c1 SET b='H3' WHERE (SELECT c1.a='ABC')`,
		`SELECT a,b FROM c1 ORDER BY a`,
		`DELETE FROM c1 WHERE (SELECT c1.a='def')`,
		`SELECT a,b FROM c1 ORDER BY a`,
	})
	// A correlated WHERE subquery that reads the TARGET table. The C evaluates
	// it in pass one, over the untouched table -- so the answer must NOT depend
	// on how many rows the loop has already removed.
	differ(t, "delete where correlated reads the target itself", []string{
		`CREATE TABLE fl(a,b)`,
		`INSERT INTO fl VALUES(1,1),(2,2),(3,3),(4,4)`,
		`DELETE FROM fl WHERE (SELECT count(*) FROM fl f3 WHERE f3.a < fl.a) >= 2`,
		`SELECT a,b FROM fl ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "update where correlated reads the target itself", []string{
		`CREATE TABLE fl(a,b)`,
		`INSERT INTO fl VALUES(1,1),(2,2),(3,3),(4,4)`,
		`UPDATE fl SET b=0 WHERE (SELECT count(*) FROM fl f3 WHERE f3.a < fl.a) >= 2`,
		`SELECT a,b FROM fl ORDER BY a`,
	})
	// A VIEW write: the same loop over a different cursor -- the MATERIALIZED
	// view, which IS iDataCur for a view write (delete.c:432, update.c:632).
	differ(t, "view update/delete with a correlated subquery", []string{
		`CREATE TABLE b(x,c)`,
		`CREATE TABLE s(k,v)`,
		`CREATE TABLE log(n)`,
		`INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')`,
		`INSERT INTO s VALUES(1,'P'),(3,'R')`,
		`CREATE VIEW v AS SELECT x,c FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE x=old.x; INSERT INTO log VALUES(old.x); END`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c WHERE x=old.x; END`,
		`UPDATE v SET c=(SELECT v FROM s WHERE s.k=v.x)`,
		`SELECT x,c FROM b ORDER BY x`,
		`DELETE FROM v WHERE EXISTS(SELECT 1 FROM s WHERE s.k=v.x)`,
		`SELECT x,c FROM b ORDER BY x`,
		`SELECT n FROM log ORDER BY n`,
		`PRAGMA integrity_check`,
	})
	differ(t, "view update with a correlated aggregate and a correlated WHERE", []string{
		`CREATE TABLE b2(x,c)`,
		`CREATE TABLE s2(k,v)`,
		`INSERT INTO b2 VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO s2 VALUES(1,'p'),(1,'q'),(2,'r')`,
		`CREATE VIEW v2 AS SELECT x,c FROM b2`,
		`CREATE TRIGGER v2u INSTEAD OF UPDATE ON v2 BEGIN UPDATE b2 SET c=new.c WHERE x=old.x; END`,
		`UPDATE v2 SET c=(SELECT count(*) FROM s2 WHERE s2.k=v2.x)`,
		`SELECT x,c FROM b2 ORDER BY x`,
		`UPDATE v2 SET c=x WHERE (SELECT count(*) FROM s2 WHERE s2.k=v2.x) > 1`,
		`SELECT x,c FROM b2 ORDER BY x`,
	})
	// A correlated WHERE beside DELETE triggers, which is the arm that emits
	// the row-gone guard between the WHERE and everything after it.
	differ(t, "delete where correlated on a triggered table", []string{
		`CREATE TABLE dt(a,b)`,
		`CREATE TABLE ds(k)`,
		`CREATE TABLE dlog(x)`,
		`INSERT INTO dt VALUES(1,0),(2,0),(3,0)`,
		`INSERT INTO ds VALUES(1),(3)`,
		`CREATE TRIGGER dtd AFTER DELETE ON dt BEGIN INSERT INTO dlog VALUES(old.a); END`,
		`DELETE FROM dt WHERE EXISTS(SELECT 1 FROM ds WHERE ds.k=dt.a)`,
		`SELECT a,b FROM dt ORDER BY a`,
		`SELECT x FROM dlog ORDER BY x`,
	})
}

// TestWriteCorrelatedAggregateShapes covers the half whose correlated reference
// is NOT read by an OpOuterColumn: an aggregate's argument or FILTER, and a
// window operand, are resolved by the aggregate/window opcodes AT RUN TIME,
// through the evalCtx chain buildOuterEvalCtx reconstitutes from the enclosing
// frames' CURSORS (engine/vdbe.go). Marking
// the write scan row-live is what first makes that chain reach a WRITE frame,
// so these are the cases where the frame being live has to be true of the
// CURSORS and not merely of the emitted opcodes.
func TestWriteCorrelatedAggregateShapes(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a INTEGER, b, s TEXT)`,
		`CREATE TABLE u(p INTEGER, q TEXT, r)`,
		`INSERT INTO t VALUES(1,0,'one'),(2,0,'two'),(3,0,'three'),(NULL,0,NULL)`,
		`INSERT INTO u VALUES(1,'x',9),(2,'y',8),(3,'z',7),(1,'w',6)`,
	}
	with := func(name string, tail ...string) {
		differ(t, name, append(append([]string(nil), setup...), tail...))
	}
	with("update set correlated FILTER",
		`UPDATE t AS o SET b=(SELECT count(*) FILTER (WHERE p<o.a) FROM u)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated FILTER over a sum",
		`UPDATE t AS o SET b=(SELECT sum(r) FILTER (WHERE p=o.a) FROM u)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated FILTER over a text column",
		`UPDATE t AS o SET b=(SELECT avg(r) FILTER (WHERE q>o.s) FROM u)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated GROUP BY/HAVING",
		`UPDATE t AS o SET b=(SELECT count(*) FROM u GROUP BY q HAVING count(*)>0 AND max(p)=o.a LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated window PARTITION BY",
		`UPDATE t AS o SET b=(SELECT count(*) OVER (PARTITION BY o.a ORDER BY p) FROM u LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated named window",
		`UPDATE t AS o SET b=(SELECT sum(p) OVER w FROM u WINDOW w AS (ORDER BY o.a) LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated frame-bounded window",
		`UPDATE t AS o SET b=(SELECT total(p) OVER (ORDER BY p ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) FROM u WHERE p<=o.a LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("delete where correlated FILTER",
		`DELETE FROM t AS o WHERE (SELECT count(*) FILTER (WHERE p<o.a) FROM u)>=2`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated DISTINCT aggregate",
		`UPDATE t AS o SET b=(SELECT count(DISTINCT p) FILTER (WHERE p<=o.a) FROM u)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated aggregate beside a scalar subquery",
		`UPDATE t AS o SET b=(SELECT (SELECT count(*) FROM u u2 WHERE u2.p=o.a) + count(*) FROM u WHERE p<o.a)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated inside a derived table",
		`UPDATE t AS o SET b=(SELECT count(*) FROM (SELECT p FROM u WHERE p<=o.a UNION SELECT 99))`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
	with("update set correlated inside a WITH body",
		`UPDATE t AS o SET b=(WITH z AS (SELECT p FROM u WHERE p<=o.a) SELECT count(*) FROM z)`,
		`SELECT a,b FROM t ORDER BY a IS NULL, a`)
}
