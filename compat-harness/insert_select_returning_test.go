// This file tests INSERT ... SELECT ... RETURNING statements, verifying
// correct rowid assignment and column ordering.
package compat

import "testing"

// TestInsertSelectReturning covers INSERT SELECT RETURNING cases.
func TestInsertSelectReturning(t *testing.T) {
	differ(t, "insert select returning", []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, a, b, d DEFAULT 'dflt')`,
		`INSERT INTO dst(a,b) SELECT a,b FROM src ORDER BY a RETURNING id, a, b, d`,
		`SELECT id,a,b,d FROM dst ORDER BY id`,
	})
	differ(t, "returning the assigned rowid", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(10),(20),(30)`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, a)`,
		`INSERT INTO dst(a) SELECT a FROM src ORDER BY a RETURNING id`,
		`INSERT INTO dst(a) SELECT a FROM src ORDER BY a DESC RETURNING id, a`,
		`SELECT id,a FROM dst ORDER BY id`,
	})
	differ(t, "returning expressions", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(2)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO dst SELECT a*10 FROM src ORDER BY a RETURNING rowid, a, a+1, 'lit'`,
		`SELECT rowid,a FROM dst ORDER BY rowid`,
	})
	differ(t, "returning over an empty source", []string{
		`CREATE TABLE src(a)`,
		`CREATE TABLE dst(a)`,
		`INSERT INTO dst SELECT a FROM src RETURNING a`,
		`SELECT count(*) FROM dst`,
	})
	// A WHERE-filtered and a compound source.
	differ(t, "returning over a filtered source", []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'k'),(2,'k'),(3,'j')`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO dst(a,b) SELECT a,b FROM src WHERE b='k' ORDER BY a RETURNING id, a`,
		`SELECT id,a,b FROM dst ORDER BY id`,
	})
	// RETURNING * over a SELECT source.
	differ(t, "returning star", []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(7,'q')`,
		`CREATE TABLE dst(a,b)`,
		`INSERT INTO dst SELECT a,b FROM src RETURNING *`,
		`SELECT a,b FROM dst`,
	})
	// A UNIQUE violation must still abort the whole statement, RETURNING or
	// not, and leave nothing behind.
	differ(t, "returning with a constraint failure", []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(1)`,
		`CREATE TABLE dst(a UNIQUE)`,
		`INSERT INTO dst SELECT a FROM src RETURNING a`,
		`SELECT count(*) FROM dst`,
	})
}

// TestInsertSelectReturningTrigger covers the SELECT-sourced form of
// "RETURNING against a triggered table" (insertFromSelect's own plain+trigger
// dispatch, insert_write.go) -- the VALUES-sourced form is
// returning_trigger_subquery_test.go's TestReturningTrigger.
func TestInsertSelectReturningTrigger(t *testing.T) {
	differ(t, "insert select returning trigger", []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, a, b)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tr1 AFTER INSERT ON dst BEGIN INSERT INTO log VALUES(NEW.id); END`,
		`INSERT INTO dst(a,b) SELECT a,b FROM src ORDER BY a RETURNING id, a, b`,
		`SELECT * FROM log`,
	})
}
