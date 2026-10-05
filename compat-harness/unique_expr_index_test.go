// This file gates UNIQUE EXPRESSION and UNIQUE PARTIAL indexes. Expression keys
// evaluate to NULL never conflict; partial indexes constrain only matching rows.
package compat

import "testing"

// TestUniqueExpressionIndex gates UNIQUE expression indexes.
func TestUniqueExpressionIndex(t *testing.T) {
	differ(t, "unique expression index", []string{
		`CREATE TABLE t5(a,b)`,
		`CREATE UNIQUE INDEX t5x ON t5(a+b)`,
		`INSERT INTO t5 VALUES(1,2)`,
		`INSERT INTO t5 VALUES(2,1)`,
		`INSERT INTO t5 VALUES(3,9)`,
		`INSERT INTO t5 VALUES(NULL,1)`,
		`INSERT INTO t5 VALUES(NULL,2)`,
		`SELECT a,b FROM t5 ORDER BY rowid`,
		`UPDATE t5 SET b=9 WHERE a=3`,
		`SELECT a,b FROM t5 ORDER BY rowid`,
		`UPDATE t5 SET a=1,b=2 WHERE a=3`,
		`SELECT a,b FROM t5 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "unique index on a function", []string{
		`CREATE TABLE t7(a TEXT)`,
		`CREATE UNIQUE INDEX t7x ON t7(lower(a))`,
		`INSERT INTO t7 VALUES('Ab')`,
		`INSERT INTO t7 VALUES('aB')`,
		`INSERT INTO t7 VALUES('cd')`,
		`INSERT INTO t7 VALUES(NULL)`,
		`INSERT INTO t7 VALUES(NULL)`,
		`SELECT a FROM t7 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	// Multi-key, mixing plain columns with expressions -- indexexpr1.test's
	// own shapes.
	differ(t, "multi-key expression index", []string{
		`CREATE TABLE t3(a,b,c)`,
		`CREATE UNIQUE INDEX t3abc ON t3(CAST(a AS text), b, substr(c,1,3))`,
		`INSERT INTO t3 VALUES(1,2,'abcdef')`,
		`INSERT INTO t3 VALUES('1',2,'abcxyz')`,
		`INSERT INTO t3 VALUES(1,2,'zzz')`,
		`SELECT a,b,c FROM t3 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "expression index with a collation", []string{
		`CREATE TABLE t8(a,b)`,
		`CREATE UNIQUE INDEX t8bx ON t8(substr(b,2,4) COLLATE nocase)`,
		`INSERT INTO t8 VALUES(1,'xABCDy')`,
		`INSERT INTO t8 VALUES(2,'xabcdz')`,
		`INSERT INTO t8 VALUES(3,'xqrstu')`,
		`SELECT a,b FROM t8 ORDER BY rowid`,
	})
	// A constant key: every row collides with every other.
	differ(t, "constant expression index", []string{
		`CREATE TABLE t0(c0)`,
		`CREATE UNIQUE INDEX i1 ON t0(0)`,
		`INSERT INTO t0 VALUES(1)`,
		`INSERT INTO t0 VALUES(2)`,
		`SELECT c0 FROM t0`,
	})
}

// TestUniquePartialIndex covers the WHERE form, whose whole point is that it
// constrains only the rows it matches.
func TestUniquePartialIndex(t *testing.T) {
	differ(t, "unique partial index", []string{
		`CREATE TABLE t6(a,b)`,
		`CREATE UNIQUE INDEX t6x ON t6(a) WHERE b>0`,
		`INSERT INTO t6 VALUES(1,1)`,
		`INSERT INTO t6 VALUES(1,-1)`,
		`INSERT INTO t6 VALUES(1,-2)`,
		`INSERT INTO t6 VALUES(1,5)`,
		`INSERT INTO t6 VALUES(2,5)`,
		`SELECT a,b FROM t6 ORDER BY rowid`,
		// moving a row INTO the index's scope must start conflicting
		`UPDATE t6 SET b=7 WHERE a=1 AND b=-1`,
		`SELECT a,b FROM t6 ORDER BY rowid`,
		// ...and moving one OUT must free the slot
		`UPDATE t6 SET b=-9 WHERE a=1 AND b=1`,
		`INSERT INTO t6 VALUES(1,3)`,
		`SELECT a,b FROM t6 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "partial index whose WHERE is never true", []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE UNIQUE INDEX t1b ON t1(b) WHERE a>NULL`,
		`INSERT INTO t1 VALUES(1,1)`,
		`INSERT INTO t1 VALUES(2,1)`,
		`SELECT a,b FROM t1 ORDER BY rowid`,
	})
	differ(t, "partial index over an equality", []string{
		`CREATE TABLE t9(x)`,
		`CREATE UNIQUE INDEX t9x ON t9(x) WHERE x=1`,
		`INSERT INTO t9 VALUES(1)`,
		`INSERT INTO t9 VALUES(2)`,
		`INSERT INTO t9 VALUES(2)`,
		`INSERT INTO t9 VALUES(1)`,
		`SELECT x FROM t9 ORDER BY rowid`,
	})
}

// TestUniqueExprIndexConflictClause covers the conflict clauses against such an
// index. They were declined at first, because once-per-statement enforcement is
// exactly ABORT's behavior and nothing else's; findRowConflicts now EVALUATES
// an expression key (and honours the partial WHERE), so OR IGNORE skips the row
// and OR REPLACE deletes the conflicting one, both resolved before the store.
func TestUniqueExprIndexConflictClause(t *testing.T) {
	differ(t, "conflict clauses on an expression index", []string{
		`CREATE TABLE t5(a,b)`,
		`CREATE UNIQUE INDEX t5x ON t5(a+b)`,
		`INSERT INTO t5 VALUES(1,2)`,
		`INSERT INTO t5 VALUES(3,9)`,
		`INSERT INTO t5 VALUES(2,1)`,
		`INSERT OR IGNORE INTO t5 VALUES(2,1)`,
		`SELECT a,b FROM t5 ORDER BY rowid`,
		`INSERT OR REPLACE INTO t5 VALUES(2,1)`,
		`SELECT a,b FROM t5 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "conflict clauses on a partial index", []string{
		`CREATE TABLE t6(a,b)`,
		`CREATE UNIQUE INDEX t6x ON t6(a) WHERE b>0`,
		`INSERT INTO t6 VALUES(1,1)`,
		`INSERT OR IGNORE INTO t6 VALUES(1,-1)`,
		`INSERT OR IGNORE INTO t6 VALUES(1,5)`,
		`SELECT a,b FROM t6 ORDER BY rowid`,
		`INSERT OR REPLACE INTO t6 VALUES(1,7)`,
		`SELECT a,b FROM t6 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "OR FAIL and OR ROLLBACK", []string{
		`CREATE TABLE t7(a,b)`,
		`CREATE UNIQUE INDEX t7x ON t7(a+b)`,
		`INSERT INTO t7 VALUES(1,2)`,
		`INSERT OR FAIL INTO t7 VALUES(2,1)`,
		`SELECT a,b FROM t7 ORDER BY rowid`,
		`BEGIN`,
		`INSERT OR ROLLBACK INTO t7 VALUES(3,0)`,
		`SELECT a,b FROM t7 ORDER BY rowid`,
	})
	// ...and an UPDATE carrying a clause resolves the same way.
	differ(t, "UPDATE with a conflict clause", []string{
		`CREATE TABLE t8(a,b)`,
		`CREATE UNIQUE INDEX t8x ON t8(a+b)`,
		`INSERT INTO t8 VALUES(1,2)`,
		`INSERT INTO t8 VALUES(5,5)`,
		`UPDATE OR IGNORE t8 SET a=2,b=1 WHERE a=5`,
		`SELECT a,b FROM t8 ORDER BY rowid`,
		`UPDATE OR REPLACE t8 SET a=2,b=1 WHERE a=5`,
		`SELECT a,b FROM t8 ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	// The plain form still ABORTs with the index-named error.
	differ(t, "no clause still aborts", []string{
		`CREATE TABLE t9(a,b)`,
		`CREATE UNIQUE INDEX t9x ON t9(a+b)`,
		`INSERT INTO t9 VALUES(1,2)`,
		`INSERT INTO t9 VALUES(2,1)`,
		`SELECT a,b FROM t9 ORDER BY rowid`,
	})
}
