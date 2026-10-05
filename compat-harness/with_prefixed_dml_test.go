// Tests for WITH clauses in DELETE and UPDATE statements.
package compat

import "testing"

func TestWithPrefixedDelete(t *testing.T) {
	differ(t, "with-prefixed delete", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1),(2),(3),(4),(5)`,
		`WITH dset AS (SELECT 2 UNION ALL SELECT 4) DELETE FROM t1 WHERE x IN dset`,
		`SELECT x FROM t1 ORDER BY x`,
		`WITH dset AS (SELECT 3) DELETE FROM t1 WHERE x IN (SELECT * FROM dset)`,
		`SELECT x FROM t1 ORDER BY x`,
		`WITH d(v) AS (VALUES(5)) DELETE FROM t1 WHERE EXISTS(SELECT 1 FROM d WHERE d.v=t1.x)`,
		`SELECT x FROM t1 ORDER BY x`,
		`WITH d(v) AS (VALUES(1)) DELETE FROM t1 WHERE x = (SELECT v FROM d)`,
		`SELECT x FROM t1 ORDER BY x`,
	})
	differ(t, "with-prefixed delete, recursive and unused", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1),(2),(3),(4),(5),(6)`,
		`WITH RECURSIVE c(n) AS (SELECT 2 UNION ALL SELECT n+2 FROM c WHERE n<6)
		   DELETE FROM t1 WHERE x IN (SELECT n FROM c)`,
		`SELECT x FROM t1 ORDER BY x`,
		`WITH unused AS (SELECT 99) DELETE FROM t1 WHERE x=1`,
		`SELECT x FROM t1 ORDER BY x`,
	})
	differ(t, "with-prefixed delete scoping", []string{
		`CREATE TABLE t1(x)`,
		`CREATE TABLE s(v)`,
		`INSERT INTO t1 VALUES(1),(2),(3)`,
		`INSERT INTO s VALUES(1)`,
		`WITH s(v) AS (SELECT 3) DELETE FROM t1 WHERE x IN (SELECT v FROM s)`,
		`SELECT x FROM t1 ORDER BY x`,
		// ...the real s is back, unshadowed, for the very next statement.
		`DELETE FROM t1 WHERE x IN (SELECT v FROM s)`,
		`SELECT x FROM t1 ORDER BY x`,
		// ...and a CTE named for a table that does NOT exist leaves no trace.
		`SELECT v FROM s ORDER BY v`,
	})
}

func TestWithPrefixedUpdate(t *testing.T) {
	differ(t, "with-prefixed update", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1),(2),(3),(4),(5)`,
		`WITH up(v) AS (SELECT 3) UPDATE t1 SET x=x*10 WHERE x IN (SELECT v FROM up)`,
		`SELECT x FROM t1 ORDER BY x`,
		// The CTE is reachable from a SET expression too, not only the WHERE.
		`WITH bump(v) AS (SELECT 100) UPDATE t1 SET x = x + (SELECT v FROM bump) WHERE x=1`,
		`SELECT x FROM t1 ORDER BY x`,
		// ...and from both at once.
		`WITH k(v) AS (VALUES(2))
		   UPDATE t1 SET x = x + (SELECT v FROM k) WHERE x IN (SELECT v FROM k)`,
		`SELECT x FROM t1 ORDER BY x`,
		`WITH unused AS (SELECT 99) UPDATE t1 SET x=x WHERE 0`,
		`SELECT x FROM t1 ORDER BY x`,
	})
	// WITH ... INSERT, which always worked, must be unaffected.
	differ(t, "with-prefixed insert still works", []string{
		`CREATE TABLE t1(x)`,
		`CREATE TABLE t2(a,b)`,
		`WITH nx(a,b) AS (VALUES(1,8),(2,11)) INSERT INTO t2(a,b) SELECT a,b FROM nx`,
		`SELECT a,b FROM t2 ORDER BY a`,
		`WITH ins AS (SELECT 7 AS z) INSERT INTO t1 SELECT z FROM ins`,
		`SELECT x FROM t1 ORDER BY x`,
	})
	// A CTE naming a table that does not exist is still an error when it is
	// actually REFERENCED -- accepting the clause must not swallow that.
	differ(t, "with-prefixed dml still reports real errors", []string{
		`CREATE TABLE t1(x)`,
		`INSERT INTO t1 VALUES(1)`,
		`WITH c AS (SELECT * FROM nosuchtable) DELETE FROM t1 WHERE x IN (SELECT * FROM c)`,
		`DELETE FROM t1 WHERE x IN (SELECT * FROM nosuchcte)`,
		`WITH c AS (SELECT 1) DELETE FROM t1 WHERE x IN (SELECT * FROM othercte)`,
		`SELECT x FROM t1 ORDER BY x`,
	})
}
