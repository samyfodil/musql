// Tests RETURNING subqueries, which can be re-evaluated per row
// if they reference the target table, or evaluated once if they reference others.
package compat

import "testing"

// TestReturningSubqueryOverTheTargetTable tests subqueries over the modified table.
func TestReturningSubqueryOverTheTargetTable(t *testing.T) {
	differ(t, "returning subquery over target, single INSERT", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t`,
		`PRAGMA integrity_check`,
	})
	// Each row sees previously written rows in multi-row insert.
	differ(t, "returning subquery over target, multi-row INSERT", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r') RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Counting includes pre-existing rows.
	differ(t, "returning subquery over a non-empty target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(9,'seed'),(8,'seed')`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	// UPDATE with RETURNING subquery evaluated per row.
	differ(t, "returning subquery over target, UPDATE", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "returning subquery over target, UPDATE with a WHERE", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0),(4,0)`,
		`UPDATE t SET b=10 WHERE a>1 RETURNING a,(SELECT count(*) FROM t WHERE b=10)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// INSERT ... SELECT with RETURNING.
	differ(t, "returning subquery over target, INSERT ... SELECT", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO t SELECT x,'y' FROM s RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Aggregates and expressions wrapping subqueries.
	differ(t, "returning subquery inside an expression", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20) RETURNING a, (SELECT sum(b) FROM t)*2, (SELECT max(a) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// EXISTS and IN over the target.
	differ(t, "returning EXISTS and IN over target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20) RETURNING a, EXISTS(SELECT 1 FROM t WHERE b>15), a IN (SELECT a FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// WITH INTEGER PRIMARY KEY.
	differ(t, "returning subquery over target with an IPK", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(NULL,'p'),(NULL,'q') RETURNING k,(SELECT count(*) FROM t)`,
		`SELECT k,b FROM t ORDER BY k`,
	})
	differ(t, "returning subquery while the rowid moves", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET k=k+10 RETURNING k,(SELECT count(*) FROM t WHERE k>5)`,
		`SELECT k,b FROM t ORDER BY k`,
		`PRAGMA integrity_check`,
	})
	// WITH WITHOUT ROWID tables.
	differ(t, "returning subquery over a WITHOUT ROWID target", []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2) RETURNING k,(SELECT count(*) FROM t)`,
		`SELECT k,b FROM t ORDER BY k`,
		`PRAGMA integrity_check`,
	})
	// RETURNING * with a subquery in the same list.
	differ(t, "returning star beside a subquery", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y') RETURNING *,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestReturningSubqueryOverAnotherTable tests subqueries over tables other than the target.
func TestReturningSubqueryOverAnotherTable(t *testing.T) {
	differ(t, "returning subquery over another table", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "returning subquery over another table, UPDATE", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`INSERT INTO s VALUES(5),(6),(7)`,
		`UPDATE t SET b=1 RETURNING a,(SELECT sum(x) FROM s)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Empty subquery table yields NULL.
	differ(t, "returning empty subquery is NULL", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,quote((SELECT x FROM s))`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestReturningSubqueryDeclinedShapes tests shapes not lowered to compiled code.
func TestReturningSubqueryDeclinedShapes(t *testing.T) {
	// DELETE with RETURNING evaluated before the row is removed.
	differ(t, "returning subquery on a DELETE", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Subquery correlated to the affected row.
	differ(t, "returning subquery correlated to the affected row", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE x=a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Triggered table with RETURNING reading the target.
	differ(t, "returning subquery against a triggered table", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT x FROM log ORDER BY rowid`,
	})
}
