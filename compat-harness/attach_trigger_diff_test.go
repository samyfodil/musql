// Tests CREATE TRIGGER with attached database names in trigger and ON clauses.
package compat

import "testing"

// TestEngineAttachCreateTriggerSelfQualified tests trigger and ON-clause both in same database.
func TestEngineAttachCreateTriggerSelfQualified(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t2(a, b)",
		"CREATE TABLE log2(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	// Same tables in main to test trigger scoping.
	p.agreeExec("CREATE TABLE t2(a, b)")
	p.agreeExec("CREATE TABLE log2(x)")

	p.agreeExec("CREATE TRIGGER aux.trig AFTER INSERT ON aux.t2 BEGIN INSERT INTO log2 VALUES(new.a); END")
	p.agreeQuery("SELECT type, name FROM aux.sqlite_master ORDER BY name")

	p.agreeExec("INSERT INTO aux.t2 VALUES(1,2)")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x") // trigger fired in aux
	p.agreeQuery("SELECT x FROM log2 ORDER BY x")     // main's log2 empty

	p.agreeExec("INSERT INTO t2 VALUES(9,9)") // main's t2: no trigger
	p.agreeQuery("SELECT x FROM log2 ORDER BY x")
	p.agreeExec("INSERT INTO aux.t2 VALUES(3,4)")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x")

	// A trigger whose ON-clause names a DIFFERENT attached database than its
	// own name is still refused on both sides -- C SQLite: "trigger t
	// cannot reference objects in database <other>". This is not a routing
	// gap: attach_write.go only ever strips an ON-clause qualifier that
	// matches the database the statement is already being routed to.
	goAux2, cgoAux2 := buildAuxPair(t, "aux2", "CREATE TABLE t3(a)")
	p2 := newAttachPair(t, []string{goAux, goAux2}, []string{cgoAux, cgoAux2})
	p2.agreeExec("ATTACH '{0}' AS aux")
	p2.agreeExec("ATTACH '{1}' AS aux2")
	p2.agreeExec("CREATE TRIGGER aux.trig2 AFTER INSERT ON aux2.t3 BEGIN SELECT 1; END")
}

// TestEngineAttachCreateTriggerSelfQualifiedRollback checks that a
// self-qualified CREATE TRIGGER created inside a transaction is undone by
// ROLLBACK exactly like any other routed cross-database write
// (rollbackTxnAttachedWrites, attach_write.go): it must not persist, and must
// not fire afterward.
func TestEngineAttachCreateTriggerSelfQualifiedRollback(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t2(a)",
		"CREATE TABLE log2(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("CREATE TRIGGER aux.trig AFTER INSERT ON aux.t2 BEGIN INSERT INTO log2 VALUES(new.a); END")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x") // empty again: the INSERT and the CREATE TRIGGER both undone
	p.agreeQuery("SELECT type, name FROM aux.sqlite_master ORDER BY name")

	// The trigger must not exist to fire post-rollback.
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x")
}

// TestEngineAttachCreateTriggerSelfQualifiedReopen checks that a self-qualified
// trigger, once its CREATE has been committed and the session that created it
// closed, survives a completely fresh session re-ATTACHing the same file --
// engine/writer_open.go's OpenWrite reload path. attachedWriteSession calls
// OpenWrite before SetLocalSchema, so a reload of a stored trigger whose
// ON-clause still carried a qualifier would fail exactly the way CREATE did;
// stripAttachedCreateTriggerOnQualifier avoids that at CREATE time instead, by
// never storing the redundant self-qualifier in the first place.
func TestEngineAttachCreateTriggerSelfQualifiedReopen(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t2(a)",
		"CREATE TABLE log2(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TRIGGER aux.trig AFTER INSERT ON aux.t2 BEGIN INSERT INTO log2 VALUES(new.a); END")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeQuery("SELECT x FROM aux.log2 ORDER BY x")
	// The raw engine.DB defers every write to Close (see writer.go's package
	// doc comment: nothing this session did -- including an attached session
	// it opened -- reaches disk before then), unlike a bare cgo statement,
	// which C SQLite already commits durably as it runs. Close explicitly
	// rather than let t.Cleanup's Discard run: Close is documented safe to
	// call early, with Discard becoming the no-op once it has (writer.go).
	if err := p.godb.Close(); err != nil {
		t.Fatalf("closing the first session: %v", err)
	}
	if err := p.cgo.Close(); err != nil {
		t.Fatalf("closing the first cgo connection: %v", err)
	}

	// A brand new session, over the SAME (now on-disk) files.
	p2 := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p2.agreeExec("ATTACH '{0}' AS aux")
	p2.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p2.agreeQuery("SELECT x FROM aux.log2 ORDER BY x") // the reloaded trigger must still fire
}
