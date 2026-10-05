// This file tests edge cases in schema reloading after writable_schema edits.
package compat

import "testing"

func TestSchemaReloadEdges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []wsrpStep
	}{
		{
			name: "view body rewritten",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,10),(2,20)`},
				{sql: `CREATE VIEW v AS SELECT a FROM t`},
				{sql: `SELECT * FROM v ORDER BY 1`, query: true},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE VIEW v AS SELECT b FROM t' WHERE name='v'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `SELECT * FROM v ORDER BY 1`, query: true},
				{sql: `SELECT type,name,sql FROM sqlite_schema`, query: true},
			},
		},
		{
			name: "trigger body rewritten",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a)`},
				{sql: `CREATE TABLE log(x)`},
				{sql: `CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(1); END`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(99); END' WHERE name='tr'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `INSERT INTO t VALUES(1)`},
				{sql: `SELECT x FROM log`, query: true},
				{sql: `SELECT type,name,sql FROM sqlite_schema`, query: true},
			},
		},
		{
			name: "STRICT added in the tail",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(id ANY PRIMARY KEY, x TEXT)`},
				{sql: `INSERT INTO t VALUES(1,'a')`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `INSERT INTO t(x) VALUES('b')`, wantErrSubstr: "NOT NULL constraint failed: t.id"},
				{sql: `SELECT id,x FROM t`, query: true},
			},
		},
		{
			name: "UNIQUE constraint removed, orphaning its autoindex row",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a UNIQUE, b)`},
				{sql: `INSERT INTO t VALUES(1,1)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a, b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`, wantGoDecline: "PRAGMA writable_schema=RESET while this session holds a direct sqlite_master write"},
			},
		},
		{
			name: "WITHOUT ROWID added by an edit",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a PRIMARY KEY, b)`},
				{sql: `INSERT INTO t VALUES(1,10)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`, wantGoDecline: "PRAGMA writable_schema=RESET while this session holds a direct sqlite_master write"},
			},
		},
		{
			name: "sql text names a different table than the name column",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,2)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE zzz(a,b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `SELECT * FROM t`, wantErrSubstr: "malformed database schema (t)"},
			},
		},
		{
			name: "sql text is a different object kind than the type column",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,2)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE VIEW t AS SELECT 1' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `SELECT * FROM t`, wantErrSubstr: "malformed database schema (t)"},
			},
		},
		{
			name: "temp table alive across the reload",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a)`},
				{sql: `CREATE TEMP TABLE tmp(x)`},
				{sql: `INSERT INTO t VALUES(1)`},
				{sql: `INSERT INTO tmp VALUES(9)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a,b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `SELECT * FROM tmp`, query: true},
				{sql: `SELECT * FROM t`, query: true},
				{sql: `INSERT INTO tmp VALUES(10)`},
				{sql: `SELECT x FROM tmp ORDER BY 1`, query: true},
			},
		},
		{
			name: "reload inside a transaction stays declined",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,2)`},
				{sql: `BEGIN`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`, wantGoDecline: "PRAGMA writable_schema=RESET while this session holds a direct sqlite_master write"},
			},
		},
		{
			name: "reload outside a transaction is durable",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,2)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `INSERT INTO t VALUES(3,4,5)`},
				{sql: `SELECT a,b,c FROM t ORDER BY a`, query: true},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { wsrpRun(t, 4096, tc.steps) })
	}
}
