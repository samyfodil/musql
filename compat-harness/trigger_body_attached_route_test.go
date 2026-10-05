// TEMP trigger body targeting ATTACHed table; unqualified name resolution order.
package compat

import "testing"

// UPDATE body in TEMP trigger targets unqualified name in ATTACHed database.
func TestEngineAttachTempTriggerBodyUpdateRoutes(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE counters(name TEXT PRIMARY KEY, n INTEGER)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("INSERT INTO aux.counters VALUES('hits', 0)")

	p.agreeExec("CREATE TABLE main.t1(a)")
	// Unqualified name resolves only in aux via TEMP/MAIN/ATTACHED search.
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN UPDATE counters SET n = n + 1 WHERE name = 'hits'; END")

	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT n FROM aux.counters WHERE name = 'hits'")
}

// TestEngineAttachTempTriggerBodyDeleteRoutes is the same shape for
// routedTriggerBodyTarget's bs.delete branch.
func TestEngineAttachTempTriggerBodyDeleteRoutes(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE stale(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("INSERT INTO aux.stale VALUES(1,'x')")
	p.agreeExec("INSERT INTO aux.stale VALUES(2,'y')")

	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN DELETE FROM stale WHERE id = new.a; END")

	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT * FROM aux.stale ORDER BY id")
}

// TestEngineAttachTempTriggerBodyLastInsertRowid checks that an INSERT a
// trigger body routes into an attachment updates THIS session's
// last_insert_rowid() too -- C SQLite's counter is CONNECTION-wide, not
// per-database (verified directly; also already the rule
// execRoutedToAttached applies to a top-level routed write, see its own doc
// comment in attach_write.go).
func TestEngineAttachTempTriggerBodyLastInsertRowid(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")

	p.agreeExec("INSERT INTO main.t1 VALUES(42)")
	p.agreeQuery("SELECT last_insert_rowid()")
	p.agreeQuery("SELECT * FROM aux.logged")
}

// TestEngineAttachTempTriggerBodyRoutedWriteRollsBack checks that a delegated
// trigger-body write made inside an explicit transaction is undone by
// ROLLBACK, the same guarantee enterTxnAttachedWrite already gives a
// top-level routed write (attach_write.go's own doc comment: "a transaction
// spans every member database in C SQLite").
func TestEngineAttachTempTriggerBodyRoutedWriteRollsBack(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT * FROM aux.logged")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT * FROM aux.logged")
	p.agreeQuery("SELECT * FROM main.t1")

	// A later, un-rolled-back write still routes correctly -- the rollback
	// must not have left the attachment's delegated session wedged.
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT * FROM aux.logged")
}

// TestEngineAttachTempTriggerBodyMainTriggerStaysPinned is the excluded class
// routedTriggerBodyTarget must NOT widen: a non-TEMP (MAIN) trigger's body
// reference is schema-pinned to main at CREATE TRIGGER time
// (sqlite3FinishTrigger, trigger.c:346, via sqlite3FixInit/fixSelectCb --
// attach.c:492's "pFix->bTemp==0" guard, which for iDb==0 (main) is true) and
// can NEVER resolve into an attachment, unlike a TEMP trigger's. So a MAIN
// trigger whose body names a table that exists ONLY in an attachment must
// still fail exactly as before this fix -- both at CREATE TRIGGER time
// (validateTriggerExprsOnce runs checkTriggerBodyTables eagerly) if the
// attachment already exists, or at fire time if it does not yet.
func TestEngineAttachTempTriggerBodyMainTriggerStaysPinned(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	// A MAIN (non-TEMP) trigger, unlike the TEMP ones above: both engines
	// must agree this still fails to resolve "logged" against main alone.
	p.agreeExec("CREATE TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")
}
