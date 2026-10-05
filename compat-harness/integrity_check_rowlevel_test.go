// Differential tests of PRAGMA integrity_check's row-level violations,
// validating which findings appear, in what order, and with what truncation.
package compat

import "testing"

func TestIntegrityCheckRowLevelBattery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []wsrpStep
	}{
		{
			// NOT NULL violations do not report the INTEGER PRIMARY KEY column.
			name: "NOT NULL over existing NULLs, rowid alias exempt",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(id INTEGER PRIMARY KEY, a, b)`},
				{sql: `INSERT INTO t VALUES(1,NULL,1),(2,2,NULL),(3,3,3),(4,NULL,NULL)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(id INTEGER PRIMARY KEY NOT NULL, a NOT NULL, b NOT NULL)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
				{sql: `PRAGMA quick_check`, query: true},
			},
		},
		{
			// UNIQUE index violations exempt rows with NULL in a nullable key column.
			name: "UNIQUE index over duplicates and NULLs",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `CREATE INDEX i ON t(a)`},
				{sql: `INSERT INTO t VALUES(1,1),(2,2),(2,3),(2,4),(NULL,5),(NULL,6)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a)' WHERE name='i'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
				{sql: `PRAGMA integrity_check(2)`, query: true},
				{sql: `PRAGMA quick_check`, query: true},
			},
		},
		{
			// Multi-column UNIQUE index: only nullable columns with NULL are exempt.
			name: "two-column UNIQUE index",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b,c)`},
				{sql: `CREATE INDEX i ON t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,1,'x'),(1,1,'y'),(1,2,'z'),(2,NULL,'w'),(2,NULL,'v')`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a,b)' WHERE name='i'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// UNIQUE indexes enforce uniqueness using their collation sequence.
			name: "NOCASE UNIQUE index",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a TEXT COLLATE nocase, b)`},
				{sql: `CREATE INDEX i ON t(a)`},
				{sql: `INSERT INTO t VALUES('abc',1),('ABC',2),('xyz',3)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a)' WHERE name='i'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// DESC key columns report violations consistently.
			name: "DESC UNIQUE index",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `CREATE INDEX i ON t(a DESC)`},
				{sql: `INSERT INTO t VALUES(1,1),(2,2),(2,3),(3,4)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a DESC)' WHERE name='i'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// Violations on the same row report column findings before index findings.
			name: "NOT NULL and non-unique interleaved per row",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `CREATE INDEX i ON t(a)`},
				{sql: `INSERT INTO t VALUES(1,1),(1,2),(NULL,3),(NULL,4)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a)' WHERE name='i'`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a NOT NULL,b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
				{sql: `PRAGMA integrity_check(1)`, query: true},
				{sql: `PRAGMA integrity_check(2)`, query: true},
				{sql: `PRAGMA integrity_check(3)`, query: true},
				{sql: `PRAGMA integrity_check(4)`, query: true},
				{sql: `PRAGMA integrity_check(5)`, query: true},
			},
		},
		{
			// Automatic indexes introduced by schema edits are declined.
			name: "automatic UNIQUE index INTRODUCED by the edit stays declined",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(1,1),(1,2),(2,3)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a UNIQUE,b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`, wantGoDecline: "PRAGMA writable_schema=RESET while this session holds a direct sqlite_master write"},
			},
		},
		{
			// Multiple indexes on one table report violations in index order.
			name: "two UNIQUE indexes on one table",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `CREATE INDEX i1 ON t(a)`},
				{sql: `CREATE INDEX i2 ON t(b)`},
				{sql: `INSERT INTO t VALUES(1,1),(1,1)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i1 ON t(a)' WHERE name='i1'`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i2 ON t(b)' WHERE name='i2'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// Partial indexes do not appear in integrity_check; plain indexes still report.
			name: "partial index present alongside a plain one",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `CREATE INDEX i ON t(a)`},
				{sql: `CREATE INDEX ip ON t(b) WHERE b>0`},
				{sql: `INSERT INTO t VALUES(1,1),(1,2)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE UNIQUE INDEX i ON t(a)' WHERE name='i'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// Integrity checks work on tables with generated columns.
			name: "generated columns",
			steps: []wsrpStep{
				{sql: `CREATE TABLE t(a, v AS (a*2), s AS (a+1) STORED)`},
				{sql: `INSERT INTO t(a) VALUES(1),(NULL),(3)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a NOT NULL, v AS (a*2), s AS (a+1) STORED)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `SELECT a,v,s FROM t ORDER BY rowid`, query: true},
				{sql: `PRAGMA integrity_check`, query: true},
			},
		},
		{
			// PRAGMA integrity_check can be scoped to a single table.
			name: "scoped to one table",
			steps: []wsrpStep{
				{sql: `CREATE TABLE clean(x)`},
				{sql: `INSERT INTO clean VALUES(1)`},
				{sql: `CREATE TABLE t(a,b)`},
				{sql: `INSERT INTO t VALUES(NULL,1)`},
				{sql: `PRAGMA writable_schema=ON`},
				{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t(a NOT NULL,b)' WHERE name='t'`},
				{sql: `PRAGMA writable_schema=RESET`},
				{sql: `PRAGMA integrity_check(t)`, query: true},
				{sql: `PRAGMA integrity_check(clean)`, query: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) { wsrpRun(t, 4096, tc.steps) })
	}
}
