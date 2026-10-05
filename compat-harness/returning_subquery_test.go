// Tests subqueries inside RETURNING expressions.
// Each row sees a fresh snapshot of the database state after its own write.
package compat

import "testing"

// TestReturningSubquery covers subqueries in RETURNING expressions.
func TestReturningSubquery(t *testing.T) {
	differ(t, "mined: correlated unqualified subquery over generated column table", []string{
		`CREATE TABLE t1(xyz)`,
		`CREATE TABLE t2(a AS (1+1), b)`,
		`UPDATE t2 SET b='123' WHERE b='abc' RETURNING (SELECT b FROM t1)`,
		`INSERT INTO t2(b) VALUES('abc')`,
		`UPDATE t2 SET b='123' WHERE b='abc' RETURNING (SELECT b FROM t1)`,
		`INSERT INTO t2(b) VALUES('abc')`,
		`INSERT INTO t1(xyz) VALUES(1)`,
		`UPDATE t2 SET b='123' WHERE b='abc' RETURNING b`,
		`INSERT INTO t2(b) VALUES('abc')`,
		`UPDATE t2 SET b='123' WHERE b='abc' RETURNING (SELECT b FROM t1)`,
	})
	// Subquery over unrelated table.
	differ(t, "subquery over an unrelated table", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TABLE t5(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO t4 VALUES(1,'abc'),(2,'xyz')`,
		`INSERT INTO t5 VALUES(1,'abc'),(2,'xyz')`,
		`UPDATE t5 SET b='123' WHERE b='abc' RETURNING (SELECT b FROM t4 WHERE a=1)`,
	})
	// Self-referential subquery over the same table: each row sees incremental state.
	differ(t, "self-referential subquery insert", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)`,
		`INSERT INTO t1 VALUES(1, 10)`,
		`INSERT INTO t1(a,b) VALUES(2,20),(3,30) RETURNING a, (SELECT count(*) FROM t1)`,
	})
	differ(t, "self-referential subquery update", []string{
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b INTEGER)`,
		`INSERT INTO t2 VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t2 SET b = b + 1 RETURNING a, b, (SELECT sum(b) FROM t2)`,
	})
	differ(t, "self-referential subquery delete", []string{
		`CREATE TABLE t3(a INTEGER PRIMARY KEY, b INTEGER)`,
		`INSERT INTO t3 VALUES(1,10),(2,20),(3,30)`,
		`DELETE FROM t3 RETURNING a, (SELECT count(*) FROM t3)`,
	})
	// Correlated subquery and EXISTS in RETURNING.
	differ(t, "correlated subquery and exists over a different table", []string{
		`CREATE TABLE t6(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TABLE t7(k INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t7 VALUES(1,'one'),(2,'two'),(3,'three')`,
		`INSERT INTO t6(a,b) VALUES(1,'x'),(2,'y') RETURNING a, (SELECT v FROM t7 WHERE k=a)`,
		`INSERT INTO t6(a,b) VALUES(10,'z') RETURNING a, EXISTS(SELECT 1 FROM t7 WHERE k=a)`,
	})
	// RETURNING subquery combined with WHERE subquery: separate snapshots.
	differ(t, "returning subquery combined with where subquery", []string{
		`CREATE TABLE t8(a INTEGER PRIMARY KEY, b INTEGER)`,
		`INSERT INTO t8 VALUES(1,1),(2,2),(3,3)`,
		`DELETE FROM t8 WHERE a IN (SELECT a FROM t8 WHERE b < 3) RETURNING a, (SELECT count(*) FROM t8)`,
	})
	// Bad table reference in RETURNING: error at prepare time.
	res := run(t, "musql", []string{
		`CREATE TABLE t9(a INTEGER PRIMARY KEY, b TEXT)`,
		`UPDATE t9 SET b='x' WHERE a=999 RETURNING a, (SELECT x FROM nosuchtable)`,
	})
	if res[1]["kind"] != "error" {
		t.Errorf("expected a RETURNING subquery over a nonexistent table to error even at zero matched rows, got %v", res[1])
	}
	res = run(t, "musql", []string{
		`CREATE TABLE t10(a INTEGER PRIMARY KEY, b TEXT)`,
		`DELETE FROM t10 WHERE a=999 RETURNING a, (SELECT x FROM nosuchtable)`,
	})
	if res[1]["kind"] != "error" {
		t.Errorf("expected a RETURNING subquery over a nonexistent table to error even at zero matched DELETE rows, got %v", res[1])
	}
}

// TestReturningSubqueryColumnGapZeroRows checks that bad column references
// in RETURNING subqueries error at prepare time.
func TestReturningSubqueryColumnGapZeroRows(t *testing.T) {
	differ(t, "zero-row UPDATE RETURNING subquery with a bad column errors", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)`,
		`UPDATE t1 SET b='x' WHERE a=999 RETURNING a, (SELECT nosuchcol FROM t1)`,
	})
	differ(t, "zero-row DELETE RETURNING subquery with a bad column errors", []string{
		`CREATE TABLE t3(a INTEGER PRIMARY KEY, b TEXT)`,
		`DELETE FROM t3 WHERE a=999 RETURNING a, (SELECT nosuchcol FROM t3)`,
	})
	// WHERE subquery with bad column: error at prepare time.
	differ(t, "zero-row WHERE subquery with a bad column errors", []string{
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b TEXT)`,
		`UPDATE t2 SET b='x' WHERE a=999 AND EXISTS(SELECT nosuchcol FROM t2)`,
		`SELECT count(*) FROM t2`,
	})
	differ(t, "zero-row SET subquery with a bad column errors", []string{
		`CREATE TABLE t4(a INTEGER PRIMARY KEY, b TEXT)`,
		`UPDATE t4 SET b=(SELECT nosuchcol FROM t4) WHERE a=999`,
		`SELECT count(*) FROM t4`,
	})
}
