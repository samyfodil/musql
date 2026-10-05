// Tests INSERT ... SELECT ... ON CONFLICT ... DO UPDATE with UPDATE triggers.
package compat

import (
	"testing"
)

func TestInsertSelectUpsertFiresUpdateTriggers(t *testing.T) {
	for _, c := range []struct {
		name   string
		script []string
	}{
		// AFTER UPDATE and INSERT triggers.
		{"after-only", []string{
			`CREATE TABLE t(a UNIQUE,b,c)`,
			`CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'one',10),(2,'two',20)`,
			`INSERT INTO s VALUES(1,'s1'),(3,'s3')`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('U'||new.a||':'||new.c); END`,
			`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I'||new.a); END`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`,
		}},

		// BEFORE and AFTER UPDATE triggers.
		{"before-and-after", []string{
			`CREATE TABLE t(a UNIQUE,b,c)`,
			`CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'one',10)`,
			`INSERT INTO s VALUES(1,'s1'),(3,'s3')`,
			`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES('B'||old.c); END`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('A'||new.c); END`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=excluded.c`,
		}},

		// A DO UPDATE ... WHERE that is FALSE for one conflicting row: that row
		// must fire nothing at all, which is the outcome a "did the trigger
		// run?" check with a single row cannot distinguish.
		{"do-update-where", []string{
			`CREATE TABLE t(a UNIQUE,b,c)`,
			`CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x)`,
			`INSERT INTO t VALUES(1,'one',10),(2,'two',99)`,
			`INSERT INTO s VALUES(1,'s1'),(2,'s2')`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('U'||new.a); END`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=c+1 WHERE c<50`,
		}},

		// The orconf override, and the case this file exists for. upsert.c:325
		// passes a LITERAL OE_Abort, so trigger.c:1137 replaces the body's own
		// "INSERT OR IGNORE" with ABORT -- the statement must FAIL on the
		// duplicate and leave t untouched, where the same body under a
		// written-out UPDATE would absorb it.
		{"body-orconf-override", []string{
			`CREATE TABLE t(a UNIQUE,b,c)`,
			`CREATE TABLE s(a,b)`,
			`CREATE TABLE log(x UNIQUE)`,
			`INSERT INTO t VALUES(1,'one',10)`,
			`INSERT INTO s VALUES(1,'s1')`,
			`INSERT INTO log VALUES(1)`,
			`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT OR IGNORE INTO log VALUES(1); END`,
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=c+1`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, c.script...),
				`SELECT a,b,c FROM t ORDER BY a`, `SELECT x FROM log ORDER BY rowid`))
		})
	}
}
