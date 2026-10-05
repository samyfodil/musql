package compat

import "testing"

// TestParseR31EmptyIn tests empty IN value lists against C SQLite.
func TestParseR31EmptyIn(t *testing.T) {
	differ(t, "r31-emptyin-constant", []string{
		`SELECT 1 IN (), 1 NOT IN (), typeof(1 IN ()), typeof(1 NOT IN ())`,
		`SELECT NULL IN (), NULL NOT IN ()`,
		`SELECT (1 IN ()) IS NULL, (NULL IN ()) IS NULL`,
	})
	differ(t, "r31-emptyin-unresolved-col", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`SELECT * FROM t WHERE a=1 OR (nosuchcol IN ())`,
		`SELECT nosuchcol IN () FROM t`,
		`SELECT nosuchcol NOT IN () FROM t`,
		`SELECT nosuchtable.nosuchcol IN () FROM t`,
		`SELECT a FROM t WHERE nosuchcol NOT IN ()`,
	})
	differ(t, "r31-emptyin-unresolved-subquery", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`SELECT (SELECT x FROM nosuchtable) IN () FROM t`,
		`SELECT (SELECT abs(nosuchcol)) IN () FROM t`,
		`SELECT EXISTS(SELECT * FROM nosuchtable) IN () FROM t`,
		`SELECT (nosuchcol IN (SELECT 1)) IN () FROM t`,
	})
	differ(t, "r31-emptyin-hasfunc", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		`SELECT abs(nosuchcol) IN () FROM t`,
		`SELECT abs(a) IN (), abs(a) NOT IN () FROM t ORDER BY a`,
		`SELECT (a+abs(b)) IN () FROM t ORDER BY a`,
		`SELECT CAST(abs(a) AS INT) IN () FROM t ORDER BY a`,
		`SELECT (CASE WHEN abs(a) THEN 1 ELSE 2 END) IN () FROM t ORDER BY a`,
		`SELECT (a BETWEEN abs(b) AND 9) IN () FROM t ORDER BY a`,
		`SELECT (a IN (abs(b))) IN () FROM t ORDER BY a`,
		`SELECT (a, abs(b)) IN () FROM t ORDER BY a`,
		`SELECT (abs(a) COLLATE nocase) IN () FROM t ORDER BY a`,
		`SELECT (a LIKE 'x') IN () FROM t ORDER BY a`,
		`SELECT (a GLOB 'x') IN () FROM t ORDER BY a`,
		`SELECT (a -> '$') IN () FROM t ORDER BY a`,
		`SELECT (RAISE_dummy) IN () FROM t ORDER BY a`,
	})
	// COLLATE is the one propagation edge that LOOKS like it should carry
	// EP_HasFunc and does not: sqlite3ExprAddCollateToken (expr.c) sets
	// pNew->pLeft directly instead of calling sqlite3ExprAttachSubtrees, and
	// ORs in only EP_Collate|EP_Skip. So the function under it is folded away
	// with the rest of the operand -- name and aggregate-ness alike.
	differ(t, "r31-emptyin-collate-blocks-propagation", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT abs(nosuchcol) COLLATE nocase IN () FROM t`,
		`SELECT count(*) COLLATE binary IN () FROM t`,
		`SELECT count(*) IN () FROM t`,
		`SELECT nosuchcol COLLATE nocase NOT IN () FROM t`,
	})
	// A vector's grammar action (parse.y:1333) DOES propagate, elementwise,
	// so an aggregate inside one keeps the query aggregate. rowvalue.test
	// 36.0-36.2 is exactly this, from forum post 2026-05-09T07:55:45Z.
	differ(t, "r31-emptyin-vector-aggregate", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`SELECT (1,2,3,max(x)) IN () FROM t1`,
		`SELECT (1,2,max(x),3) IN () FROM t1`,
		`SELECT (max(x),1,23) IN () FROM t1`,
		`SELECT (x, count(*)) IN () FROM t1`,
		`SELECT (x, 1) IN () FROM t1`,
		`SELECT (x, abs(x)) IN () FROM t1`,
		`SELECT (nosuchcol, max(x)) IN () FROM t1`,
	})
	// exprCodeTargetAndOr (expr.c:4874) calls sqlite3ExprSimplifiedAndOr
	// (expr.c:2373) first, and an EP_IsFalse left operand of TK_AND collapses
	// the node to that operand, so a SURVIVING operand is never coded -- its
	// runtime errors never fire.
	differ(t, "r31-emptyin-surviving-operand-never-coded", []string{
		`SELECT json('x') IN ()`,
		`SELECT json('x') NOT IN ()`,
		`SELECT unicode(char(55296)) IN ()`,
	})

	// --- the aggregate case the exception exists for -------------------
	differ(t, "r31-emptyin-aggregate", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		// count(*) IN () becomes "0 AND count(*)": still an aggregate query,
		// so ONE row -- whereas a folded-away operand would leave three.
		`SELECT count(*) IN () FROM t`,
		`SELECT count(*) NOT IN () FROM t`,
		`SELECT 1 IN () FROM t`,
		`SELECT sum(a) IN (), max(a) NOT IN () FROM t`,
	})

	// --- non-scalar / odd operands ------------------------------------
	differ(t, "r31-emptyin-vector", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2)`,
		`SELECT (a,b) IN () FROM t`,
		`SELECT (nosuchcol, alsonosuch) IN () FROM t`,
		`SELECT (nosuchcol, alsonosuch) NOT IN () FROM t`,
		`SELECT ((SELECT a, b FROM t)) IN () FROM t`,
	})
	differ(t, "r31-emptyin-cast-operand", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		// CAST is in parse.y's %fallback ID list, so "cast(x)" with no AS is a
		// CALL to a (nonexistent) function named cast, which sets EP_HasFunc --
		// while "CAST(x AS ...)" is TK_CAST, which does not.
		`SELECT CAST(nosuchcol AS INTEGER) IN () FROM t`,
		`SELECT cast(nosuchcol AS text) NOT IN () FROM t`,
	})
	differ(t, "r31-emptyin-cast-called-as-function", []string{
		`SELECT cast(1) IN ()`,
	})

	// --- where the operand is used, not just selected ------------------
	differ(t, "r31-emptyin-where", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2)`,
		`SELECT a FROM t WHERE a IN () ORDER BY a`,
		`SELECT a FROM t WHERE a NOT IN () ORDER BY a`,
		`SELECT count(*) FROM t GROUP BY (nosuchcol IN ())`,
		`SELECT a FROM t ORDER BY (nosuchcol IN ()), a`,
		`SELECT a FROM t HAVING nosuchcol IN () ORDER BY a`,
	})
	differ(t, "r31-emptyin-dml", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`DELETE FROM t WHERE nosuchcol IN ()`,
		`SELECT a FROM t ORDER BY a`,
		`UPDATE t SET a=a+10 WHERE nosuchcol NOT IN ()`,
		`SELECT a FROM t ORDER BY a`,
		`DELETE FROM t WHERE nosuchcol NOT IN ()`,
		`SELECT count(*) FROM t`,
	})

	// --- schema objects: the two shapes that currently decline ---------
	differ(t, "r31-emptyin-check", []string{
		`CREATE TABLE t(a, CHECK(b IN ()))`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) FROM t`,
		`SELECT sql FROM sqlite_master WHERE name='t'`,
	})
	differ(t, "r31-emptyin-check-not", []string{
		`CREATE TABLE t(a, CHECK(b NOT IN ()))`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) FROM t`,
	})
	differ(t, "r31-emptyin-partial-index", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		`CREATE INDEX i ON t(a) WHERE b IN ()`,
		`SELECT a FROM t WHERE b IN () ORDER BY a`,
		`SELECT sql FROM sqlite_master WHERE name='i'`,
	})
	differ(t, "r31-emptyin-partial-index-unresolved", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		`CREATE INDEX i ON t(a) WHERE nosuchcol NOT IN ()`,
		`SELECT a FROM t ORDER BY a`,
		// A folded predicate must leave the index MATERIALIZED consistently:
		// "NOT IN ()" is 1, so every row is in it, and integrity_check walks
		// the whole b-tree comparing it against the table.
		`INSERT INTO t VALUES(5,6)`,
		`DELETE FROM t WHERE a=1`,
		`SELECT a FROM t WHERE a>0 ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "r31-emptyin-partial-index-empty", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2),(3,4)`,
		// ... and "IN ()" is 0, so the index holds NO rows.
		`CREATE INDEX i ON t(a) WHERE nosuchcol IN ()`,
		`INSERT INTO t VALUES(5,6)`,
		`SELECT a FROM t WHERE a>0 ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	// A generated column and a DEFAULT both re-parse the same folded text on
	// every reopen; the round trip has to agree with the first parse.
	differ(t, "r31-emptyin-generated-roundtrip", []string{
		`CREATE TABLE t(a, g AS (nosuchcol NOT IN ()) STORED, h AS (nosuchcol IN ()) VIRTUAL)`,
		`INSERT INTO t(a) VALUES(1),(2)`,
		`SELECT a, g, h, typeof(g), typeof(h) FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "r31-emptyin-view", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2)`,
		`CREATE VIEW v AS SELECT a, nosuchcol IN () AS c FROM t`,
		`SELECT * FROM v ORDER BY a`,
	})
	differ(t, "r31-emptyin-generated", []string{
		`CREATE TABLE t(a, g AS (nosuchcol IN ()))`,
		`INSERT INTO t(a) VALUES(1)`,
		`SELECT a, g FROM t`,
	})

	// --- ALTER's rename cascade over a folded operand ------------------
	differ(t, "r31-emptyin-rename-column", []string{
		`CREATE TABLE t1(a, b, c)`,
		`CREATE INDEX i1 ON t1(a) WHERE b IN ()`,
		`ALTER TABLE t1 RENAME b TO bbb`,
		`SELECT count(*) FROM t1`,
	})
}
