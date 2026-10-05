// Tests INSERT ... SELECT statements inside UPDATE and DELETE trigger bodies.
package compat

import "testing"

// TestTriggerInsertSelectBody covers the accepted half.
func TestTriggerInsertSelectBody(t *testing.T) {
	differ(t, "insert-select in an UPDATE trigger", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TABLE log(v)`,
		`INSERT INTO src VALUES(1),(2)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log SELECT x FROM src; END`,
		`UPDATE t SET b='two' WHERE a=1`,
		`SELECT v FROM log ORDER BY v`,
		`UPDATE t SET b='three' WHERE a=99`,
		`SELECT v FROM log ORDER BY v`,
	})
	differ(t, "insert-select in a DELETE trigger", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TABLE log(v)`,
		`INSERT INTO src VALUES(5)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two')`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log SELECT x FROM src; END`,
		`DELETE FROM t WHERE a=1`,
		`SELECT v FROM log ORDER BY v`,
		`DELETE FROM t WHERE a=99`,
		`SELECT v FROM log ORDER BY v`,
	})
	// OLD/NEW must resolve inside the source SELECT's own expressions.
	differ(t, "insert-select referencing OLD and NEW", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TABLE log(oldb, newb, x)`,
		`INSERT INTO src VALUES(7)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log SELECT OLD.b, NEW.b, x FROM src; END`,
		`UPDATE t SET b='two' WHERE a=1`,
		`SELECT oldb,newb,x FROM log`,
	})
	// A WHERE-filtered source, and one whose FROM is the trigger's own table.
	differ(t, "insert-select with a filter", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TABLE log(v)`,
		`INSERT INTO src VALUES(1),(2),(3)`,
		`INSERT INTO t VALUES(1,'one')`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log SELECT x FROM src WHERE x>1; END`,
		`UPDATE t SET b='two'`,
		`SELECT v FROM log ORDER BY v`,
	})
}

// TestTriggerInsertSelectBodyStillValidates is the load-bearing half: a body
// reference C SQLite rejects at CREATE time must be rejected there here too,
// rather than surviving to a firing that may never happen.
func TestTriggerInsertSelectBodyStillValidates(t *testing.T) {
	differ(t, "unknown table in the source", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(v)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log SELECT x FROM nosuchtable; END`,
		`UPDATE t SET b='x'`,
	})
	differ(t, "unknown column in the source", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TABLE log(v)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log SELECT nosuchcol FROM src; END`,
		`UPDATE t SET b='x'`,
	})
	differ(t, "unknown target table", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE src(x)`,
		`CREATE TRIGGER tu AFTER DELETE ON t BEGIN INSERT INTO nosuchlog SELECT x FROM src; END`,
		`DELETE FROM t`,
	})
}
