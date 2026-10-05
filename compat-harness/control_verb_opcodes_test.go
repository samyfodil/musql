// This file tests control, DDL, and utility verbs (BEGIN, CREATE TABLE, etc.)
// to ensure their behavior and side effects match C SQLite.
package compat

import "testing"

func TestControlVerbTransactionOpcodes(t *testing.T) {
	differ(t, "BEGIN/COMMIT leaves the rows committed", []string{
		`CREATE TABLE t(a)`,
		`BEGIN`,
		`INSERT INTO t VALUES(1)`,
		`INSERT INTO t VALUES(2)`,
		`COMMIT`,
		`SELECT count(*) AS n FROM t`,
		// ...and the session is back in autocommit: a bare INSERT sticks.
		`INSERT INTO t VALUES(3)`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "BEGIN/ROLLBACK discards, and END is COMMIT", []string{
		`CREATE TABLE t(a)`,
		`BEGIN`,
		`INSERT INTO t VALUES(1)`,
		`ROLLBACK`,
		`SELECT count(*) AS n FROM t`,
		`BEGIN TRANSACTION`,
		`INSERT INTO t VALUES(2)`,
		`END`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "every BEGIN mode", []string{
		`CREATE TABLE t(a)`,
		`BEGIN DEFERRED`,
		`INSERT INTO t VALUES(1)`,
		`COMMIT`,
		`BEGIN IMMEDIATE`,
		`INSERT INTO t VALUES(2)`,
		`COMMIT`,
		`BEGIN EXCLUSIVE`,
		`INSERT INTO t VALUES(3)`,
		`COMMIT`,
		`SELECT count(*) AS n FROM t`,
	})
	// The error paths, and the state each leaves: a rejected verb must not
	// change whether a transaction is open.
	differ(t, "transaction verbs that error leave the state alone", []string{
		`CREATE TABLE t(a)`,
		`COMMIT`,   // nothing open
		`ROLLBACK`, // nothing open
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) AS n FROM t`, // still autocommit: 1 row
		`BEGIN`,
		`BEGIN`, // already open -> error, but the first is still open
		`INSERT INTO t VALUES(2)`,
		`ROLLBACK`,
		`SELECT count(*) AS n FROM t`, // the second insert is gone
	})
}

func TestControlVerbSavepointOpcodes(t *testing.T) {
	differ(t, "SAVEPOINT/RELEASE/ROLLBACK TO", []string{
		`CREATE TABLE t(a)`,
		`SAVEPOINT outer`, // opens a transaction of its own
		`INSERT INTO t VALUES(1)`,
		`SAVEPOINT inner`,
		`INSERT INTO t VALUES(2)`,
		`ROLLBACK TO inner`,
		`SELECT count(*) AS n FROM t`, // 1
		`RELEASE inner`,
		`INSERT INTO t VALUES(3)`,
		`RELEASE outer`, // commits
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "SAVEPOINT inside an explicit transaction", []string{
		`CREATE TABLE t(a)`,
		`BEGIN`,
		`INSERT INTO t VALUES(1)`,
		`SAVEPOINT sp`,
		`INSERT INTO t VALUES(2)`,
		`ROLLBACK TO SAVEPOINT sp`,
		`RELEASE SAVEPOINT sp`,
		`COMMIT`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "savepoint verbs naming nothing", []string{
		`CREATE TABLE t(a)`,
		`RELEASE nosuch`,
		`ROLLBACK TO nosuch`,
		`INSERT INTO t VALUES(1)`,
		`SELECT count(*) AS n FROM t`,
	})
}

func TestControlVerbPragmaOpcode(t *testing.T) {
	differ(t, "PRAGMA sets state a later statement reads", []string{
		`PRAGMA user_version = 7`,
		`PRAGMA user_version`,
		`CREATE TABLE t(a)`,
		`PRAGMA main.user_version = 9`,
		`PRAGMA user_version`,
		`PRAGMA table_info(t)`,
	})
	// A PRAGMA that changes how a LATER statement behaves is the case a no-op
	// "accept" would hide -- a PRAGMA accepted as a no-op desyncs later statements.
	differ(t, "PRAGMA that changes a later statement's outcome", []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(pid REFERENCES p(id))`,
		`PRAGMA foreign_keys = ON`,
		`INSERT INTO c VALUES(1)`, // rejected: no parent row
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO c VALUES(1)`, // accepted
		`SELECT count(*) AS n FROM c`,
	})
}

func TestControlVerbSchemaOpcodes(t *testing.T) {
	differ(t, "CREATE TRIGGER then DROP TRIGGER", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT x FROM log ORDER BY x`,
		`SELECT count(*) AS n FROM sqlite_master WHERE type='trigger'`,
		`DROP TRIGGER tr`,
		`INSERT INTO t VALUES(2)`,
		`SELECT x FROM log ORDER BY x`, // still just the one row
		`SELECT count(*) AS n FROM sqlite_master WHERE type='trigger'`,
		`DROP TRIGGER tr`, // gone -> error
		`DROP TRIGGER IF EXISTS tr`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "CREATE VIRTUAL TABLE", []string{
		`CREATE VIRTUAL TABLE vt USING fts4(body)`,
		`INSERT INTO vt(body) VALUES('one two three')`,
		`INSERT INTO vt(body) VALUES('four five six')`,
		`SELECT body FROM vt WHERE vt MATCH 'five'`,
		`SELECT count(*) AS n FROM vt`,
		`CREATE VIRTUAL TABLE bad USING nosuchmodule(x)`, // error on both
		`SELECT count(*) AS n FROM vt`,
	})
	differ(t, "ANALYZE and REINDEX", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE INDEX ix ON t(a)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`ANALYZE`,
		`SELECT count(*) AS n FROM sqlite_master WHERE name='sqlite_stat1'`,
		`REINDEX`,
		`REINDEX ix`,
		`REINDEX t`,
		`SELECT a,b FROM t ORDER BY a`,
		`REINDEX nosuchthing`, // error on both
		`SELECT a FROM t WHERE a=2`,
	})
	differ(t, "VACUUM", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`DELETE FROM t WHERE a=2`,
		`VACUUM`,
		`SELECT a FROM t ORDER BY a`,
		`BEGIN`,
		`VACUUM`, // "cannot VACUUM from within a transaction" on both
		`ROLLBACK`,
		`SELECT a FROM t ORDER BY a`,
	})
}

func TestControlVerbAttachOpcodes(t *testing.T) {
	// ATTACH/DETACH of an in-memory database: no filesystem path to keep the
	// two engines' files apart, so the identical statement text is safe to run
	// on both. The FILE-backed forms already have their own differential gates
	// (attach_diff_test.go, crossdb_diff_test.go); what is gated here is that
	// routing the verb through OpDdl left the alias, its writes and the
	// detach-time teardown exactly as they were.
	differ(t, "ATTACH/DETACH an in-memory database", []string{
		`ATTACH DATABASE ':memory:' AS aux`,
		`CREATE TABLE aux.t(a)`,
		`INSERT INTO aux.t VALUES(1),(2)`,
		`SELECT a FROM aux.t ORDER BY a`,
		`DETACH DATABASE aux`,
		`SELECT a FROM aux.t ORDER BY a`, // gone -> error
		`DETACH DATABASE aux`,            // gone -> error
		`DETACH DATABASE main`,           // cannot detach main -> error
		`SELECT 1 AS n`,                  // the session still works
	})
}

// TestControlVerbWithPrefixedDMLResolves is the WITH half of the batch: the
// write compiler dispatched on toks[0] where the rest of the write path uses
// writeDispatchKeyword, so every WITH-prefixed INSERT/UPDATE/DELETE missed its
// own compiler entirely.
//
// The EMPTY-table cases are the ones that matter. The route it missed onto
// resolved a WHERE subquery's names per ROW, so over a table with no rows it
// never looked -- and answered "ok" where C SQLite, which resolves at
// PREPARE time, raises "no such table". Both of these DIVERGED before this
// change, which is why they are pinned here rather than assumed.
func TestControlVerbWithPrefixedDMLResolves(t *testing.T) {
	differ(t, "WITH-prefixed DML over an empty table still resolves its names", []string{
		`CREATE TABLE t(a)`,
		`WITH c AS (SELECT * FROM nosuch) DELETE FROM t WHERE a IN (SELECT * FROM c)`,
		`WITH c AS (SELECT * FROM nosuch) UPDATE t SET a=1 WHERE a IN (SELECT * FROM c)`,
		`WITH c AS (SELECT 1) DELETE FROM t WHERE a IN (SELECT * FROM othercte)`,
		`SELECT count(*) AS n FROM t`,
	})
	differ(t, "WITH-prefixed DML still runs", []string{
		`CREATE TABLE t(a)`,
		`WITH c(v) AS (VALUES(1),(2),(3)) INSERT INTO t SELECT v FROM c`,
		`WITH c(v) AS (VALUES(2)) UPDATE t SET a=a*10 WHERE a IN (SELECT v FROM c)`,
		`WITH c(v) AS (VALUES(3)) DELETE FROM t WHERE a IN (SELECT v FROM c)`,
		`SELECT a FROM t ORDER BY a`,
		// The REPLACE spelling behind a WITH is out of this batch's scope (it
		// is a conflict-clause INSERT); it must still behave.
		`CREATE TABLE u(a UNIQUE)`,
		`WITH c(v) AS (VALUES(1)) REPLACE INTO u SELECT v FROM c`,
		`WITH c(v) AS (VALUES(1)) REPLACE INTO u SELECT v FROM c`,
		`SELECT a FROM u ORDER BY a`,
	})
}
