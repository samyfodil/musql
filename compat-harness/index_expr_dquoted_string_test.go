// This file tests that double-quoted strings in index expressions and partial
// index WHERE clauses are handled as string literals when they don't resolve to columns.
package compat

import "testing"

func TestIndexExpressionDoubleQuotedString(t *testing.T) {
	differ(t, "double-quoted string in an index expression", []string{
		`CREATE TABLE q1(x,y,z)`,
		`INSERT INTO q1 VALUES(1,2,'zz'),(3,4,'ww')`,
		`CREATE INDEX i2 ON q1(x, y, z||"abc")`,
		`SELECT name, sql FROM sqlite_master WHERE type='index' ORDER BY name`,
		`PRAGMA integrity_check`,
		// The index must survive a later write, and the table's contents must
		// be unaffected by it.
		`INSERT INTO q1 VALUES(5,6,'qq')`,
		`SELECT x,y,z FROM q1 ORDER BY x`,
		`PRAGMA integrity_check`,
		`SELECT z||"abc" FROM q1 ORDER BY x`,
	})
	// A PARTIAL index's WHERE degrades identically -- both when the degraded
	// literal matches nothing and when it matches a row.
	differ(t, "double-quoted string in a partial index WHERE", []string{
		`CREATE TABLE q1(x,y,z)`,
		`INSERT INTO q1 VALUES(1,2,'zz'),(3,4,'ww')`,
		`CREATE INDEX i5 ON q1(x) WHERE z = "nosuch"`,
		`CREATE INDEX i7 ON q1(x) WHERE z = "zz"`,
		`SELECT name, sql FROM sqlite_master WHERE type='index' ORDER BY name`,
		`PRAGMA integrity_check`,
		`SELECT x FROM q1 ORDER BY x`,
		`INSERT INTO q1 VALUES(5,6,'qq')`,
		`SELECT x,y,z FROM q1 ORDER BY x`,
		`PRAGMA integrity_check`,
	})
	// A double-quoted name that DOES resolve to a column still resolves to it
	// -- the fallback must never shadow a real column.
	differ(t, "double-quoted name that resolves is still a column", []string{
		`CREATE TABLE q1(x,y,z)`,
		`INSERT INTO q1 VALUES(1,2,'zz'),(3,4,'ww')`,
		`CREATE INDEX i8 ON q1(x, "z"||'!')`,
		`SELECT sql FROM sqlite_master WHERE name='i8'`,
		`SELECT "z"||'!' FROM q1 ORDER BY x`,
		`PRAGMA integrity_check`,
	})
	// An UNQUOTED unknown name is still "no such column" on both engines: the
	// fallback is about the SPELLING, not about tolerating typos.
	differ(t, "unquoted unknown name still errors", []string{
		`CREATE TABLE q1(x,y,z)`,
		`CREATE INDEX i9 ON q1(x, z||abc)`,
		`SELECT count(*) AS n FROM sqlite_master WHERE type='index'`,
	})
}
