// Tests transaction control and ATTACH/DETACH verbs engine-direct, since the
// driver intercepts these before they reach the engine. Covers BEGIN/COMMIT/END,
// ROLLBACK, savepoints, ATTACH/DETACH, and remaining OpDdl operations like
// CREATE INDEX, ANALYZE, and virtual tables.
package compat

import "testing"

// TestControlVerbTxnEngineDirect tests transaction control: BEGIN/COMMIT/END,
// ROLLBACK, SAVEPOINT, RELEASE, and ROLLBACK TO.
func TestControlVerbTxnEngineDirect(t *testing.T) {
	t.Run("begin-commit", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`BEGIN`)
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`COMMIT`)
		// A second COMMIT must fail on both: it is what catches a COMMIT that
		// silently did nothing and left the transaction open.
		p.agreeExec(`COMMIT`)
		p.agreeQuery(`SELECT count(*) AS n FROM t`)
		p.agreeExec(`INSERT INTO t VALUES(2)`)
		p.agreeQuery(`SELECT a FROM t ORDER BY a`)
	})
	t.Run("begin-rollback", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`BEGIN`)
		p.agreeExec(`INSERT INTO t VALUES(2)`)
		p.agreeExec(`ROLLBACK`)
		// The row count is the assertion: a BEGIN that did nothing would have
		// autocommitted the insert, and a ROLLBACK that did nothing would have
		// kept it.
		p.agreeQuery(`SELECT a FROM t ORDER BY a`)
		p.agreeExec(`ROLLBACK`) // nothing open now: both must reject
	})
	t.Run("end-is-commit", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`BEGIN TRANSACTION`)
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`END TRANSACTION`)
		p.agreeExec(`END`) // nothing open: both must reject
		p.agreeQuery(`SELECT a FROM t ORDER BY a`)
	})
	t.Run("begin-modes-and-nesting", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		for _, mode := range []string{`BEGIN DEFERRED`, `BEGIN IMMEDIATE`, `BEGIN EXCLUSIVE`} {
			p.agreeExec(mode)
			p.agreeExec(`BEGIN`) // already inside one: both must reject
			p.agreeExec(`INSERT INTO t VALUES(1)`)
			p.agreeExec(`COMMIT`)
		}
		p.agreeQuery(`SELECT count(*) AS n FROM t`)
	})
	// Nested savepoints with ROLLBACK TO the outer one verify that savepoint
	// names are correctly preserved during compilation.
	t.Run("savepoint-names", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`SAVEPOINT a`) // opens a transaction of its own
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`SAVEPOINT b`)
		p.agreeExec(`INSERT INTO t VALUES(2)`)
		p.agreeExec(`ROLLBACK TO a`)
		p.agreeQuery(`SELECT count(*) AS n FROM t`) // 0
		p.agreeExec(`RELEASE a`)                    // commits, and pops both
		p.agreeExec(`RELEASE a`)                    // gone now: both must reject
		p.agreeExec(`ROLLBACK TO b`)                // likewise
		p.agreeQuery(`SELECT count(*) AS n FROM t`)
	})
	t.Run("savepoint-inside-a-transaction", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`BEGIN`)
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`SAVEPOINT sp`)
		p.agreeExec(`INSERT INTO t VALUES(2)`)
		p.agreeExec(`ROLLBACK TO SAVEPOINT sp`)
		p.agreeExec(`RELEASE SAVEPOINT sp`)
		p.agreeExec(`COMMIT`)
		p.agreeQuery(`SELECT a FROM t ORDER BY a`) // just row 1
	})
	t.Run("savepoint-release-commits-the-outermost", func(t *testing.T) {
		p := newAttachPair(t, nil, nil)
		p.agreeExec(`CREATE TABLE t(a)`)
		p.agreeExec(`SAVEPOINT one`)
		p.agreeExec(`INSERT INTO t VALUES(1)`)
		p.agreeExec(`RELEASE one`)
		p.agreeExec(`RELEASE one`) // both reject: the stack is empty
		p.agreeQuery(`SELECT a FROM t ORDER BY a`)
	})
}

// TestControlVerbAttachEngineDirect tests ATTACH and DETACH operations.
func TestControlVerbAttachEngineDirect(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "cvaux", `CREATE TABLE at(x)`, `INSERT INTO at VALUES(7),(8)`)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec(`CREATE TABLE t(a)`)
	// Before the ATTACH the alias resolves to nothing, on both.
	p.agreeQuery(`SELECT x FROM aux.at ORDER BY x`)
	p.agreeExec(`ATTACH DATABASE '{0}' AS aux`)
	// ...and after it, to the file's own rows. A no-op ATTACH shows up here.
	p.agreeQuery(`SELECT x FROM aux.at ORDER BY x`)
	p.agreeExec(`ATTACH DATABASE '{0}' AS aux`) // duplicate alias: both reject
	p.agreeExec(`DETACH DATABASE aux`)
	// ...and after the DETACH the alias is gone again. A no-op DETACH shows up
	// here, and in the second DETACH below.
	p.agreeQuery(`SELECT x FROM aux.at ORDER BY x`)
	p.agreeExec(`DETACH DATABASE aux`)
	p.agreeExec(`DETACH DATABASE main`) // never detachable: both reject
	p.agreeQuery(`SELECT count(*) AS n FROM t`)
}

// TestControlVerbUtilityEngineDirect tests remaining DDL operations: CREATE INDEX,
// ANALYZE, REINDEX, PRAGMA, TRIGGER, and virtual tables.
func TestControlVerbUtilityEngineDirect(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec(`CREATE TABLE t(a,b)`)
	p.agreeExec(`CREATE INDEX ix ON t(a)`)
	p.agreeExec(`INSERT INTO t VALUES(1,'x'),(2,'y')`)
	p.agreeExec(`PRAGMA user_version = 11`)
	p.agreeQuery(`PRAGMA user_version`)
	p.agreeExec(`ANALYZE`)
	p.agreeQuery(`SELECT count(*) AS n FROM sqlite_master WHERE name='sqlite_stat1'`)
	p.agreeExec(`REINDEX ix`)
	p.agreeExec(`REINDEX nosuchthing`) // both reject
	p.agreeExec(`CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET b='z' WHERE a=new.a; END`)
	p.agreeExec(`INSERT INTO t VALUES(3,'w')`)
	p.agreeQuery(`SELECT a,b FROM t ORDER BY a`)
	p.agreeExec(`DROP TRIGGER tr`)
	p.agreeExec(`DROP TRIGGER tr`) // both reject
	p.agreeExec(`INSERT INTO t VALUES(4,'w')`)
	p.agreeQuery(`SELECT a,b FROM t ORDER BY a`)
	p.agreeExec(`VACUUM`)
	p.agreeQuery(`SELECT a,b FROM t ORDER BY a`)
	p.agreeExec(`CREATE VIRTUAL TABLE vt USING fts4(body)`)
	p.agreeExec(`INSERT INTO vt(body) VALUES('alpha beta')`)
	p.agreeQuery(`SELECT body FROM vt WHERE vt MATCH 'beta'`)
	p.agreeExec(`CREATE VIRTUAL TABLE bad USING nosuchmodule(x)`) // both reject
	p.agreeQuery(`SELECT count(*) AS n FROM vt`)
}
