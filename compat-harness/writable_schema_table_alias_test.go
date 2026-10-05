package compat

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestWritableSchemaTableAliasIsServed tests table aliasing via rootpage writes.
func TestWritableSchemaTableAliasIsServed(t *testing.T) {
	for _, withReset := range []bool{true, false} {
		out := map[string]string{}
		for _, eng := range engineOrder {
			dsn := filepath.Join(t.TempDir(), "a.db")
			s1 := []string{
				`CREATE TABLE t1(a INT, b INTEGER, c TEXT)`,
				`CREATE TABLE t2(a INT, b INTEGER, c TEXT)`,
				`INSERT INTO t1 VALUES(1,1,'one'),(2,2,'two')`,
				`INSERT INTO t2 VALUES(7,7,'seven')`,
				`PRAGMA writable_schema=on`,
				`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='t1') WHERE name='t2'`,
				`SELECT * FROM t2`, // still the loaded schema: t2's own rows
			}
			if withReset {
				s1 = append(s1, `PRAGMA writable_schema=RESET`, `SELECT * FROM t2`,
					`INSERT INTO t2 VALUES(3,3,'three')`, `SELECT * FROM t1`)
			}
			a := runWithDSN(t, eng, dsn, s1)
			b := runWithDSN(t, eng, dsn, []string{
				`SELECT name, rootpage FROM sqlite_schema`,
				`SELECT * FROM t2`, `SELECT * FROM t1`,
				`INSERT INTO t1 VALUES(4,4,'four')`, `SELECT * FROM t2 ORDER BY a`,
				`UPDATE t2 SET c='TWO' WHERE a=2`, `SELECT * FROM t1 ORDER BY a`,
				`PRAGMA integrity_check('t1')`, `PRAGMA quick_check('t2')`,
			})
			out[eng] = fmt.Sprint(a) + "\n  REOPEN " + fmt.Sprint(b)
		}
		if out["cgo"] != out["musql"] {
			t.Errorf("reset=%v\ncgo:    %s\nmusql: %s", withReset, out["cgo"], out["musql"])
		}
	}
}

// TestWritableSchemaStrictAliasQuickCheck is strict2.test's own use of the idiom,
// with the column types agreeing: a value written through the untyped-looking
// alias lands in the STRICT table's rows, and quick_check reports it the way
// pragma.c:1984-2002 does ("non-INTEGER value in t1.b").
func TestWritableSchemaStrictAliasQuickCheck(t *testing.T) {
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `CREATE TABLE t1(a INT, b INTEGER, c TEXT, d REAL, e BLOB) STRICT`},
		{sql: `CREATE TABLE t2(a INT, b INTEGER, c TEXT, d REAL, e BLOB)`},
		{sql: `INSERT INTO t1 VALUES(1,1,'one',1.0,x'b1'),(2,2,'two',2.25,x'b2b2')`},
		{sql: `PRAGMA writable_schema=on`},
		{sql: `UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='t1') WHERE name='t2'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `SELECT * FROM t2`, query: true},
		{sql: `UPDATE t2 SET b=2.5 WHERE a=2`},
		{sql: `SELECT * FROM t1`, query: true},
		{sql: `PRAGMA quick_check('t1')`, query: true},
		{sql: `UPDATE t2 SET b='two' WHERE a=2`},
		{sql: `PRAGMA quick_check('t1')`, query: true},
		{sql: `UPDATE t2 SET b=NULL WHERE a=2`},
		{sql: `PRAGMA quick_check('t1')`, query: true},
	})
}
