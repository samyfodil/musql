// A trigger body statement whose target resolves only in an ATTACHed database
// routes to that database's write session, but must preserve per-statement state
// that lives on the connection (foreign keys, undo closures, rowid counters).
package compat

import "testing"

// TestEngineAttachRoutedTriggerBodyEnforcesForeignKeys checks that foreign key
// cascades fire correctly for routed trigger bodies.
func TestEngineAttachRoutedTriggerBodyEnforcesForeignKeys(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE par(id INTEGER PRIMARY KEY)",
		"CREATE TABLE kid(id INTEGER PRIMARY KEY, p REFERENCES par(id) ON DELETE CASCADE)",
		"INSERT INTO par VALUES(1),(2)",
		"INSERT INTO kid VALUES(10,1),(11,1),(20,2)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA foreign_keys=ON")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	// par/kid exist ONLY in aux, so this body's target is routed.
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN DELETE FROM par WHERE id = new.a; END")

	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	// The cascade is the whole point: par 1 gone AND both of its kids gone.
	p.agreeQuery("SELECT id FROM aux.par ORDER BY id")
	p.agreeQuery("SELECT id, p FROM aux.kid ORDER BY id")

	// A second firing, to prove the session did not wedge after the first.
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT id FROM aux.par ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM aux.kid")
}

// TestEngineAttachRoutedTriggerBodyUndoesCascadeOnViolation checks that FK
// undo closures are properly tracked when a routed trigger body's DELETE fails.
func TestEngineAttachRoutedTriggerBodyUndoesCascadeOnViolation(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE par(id INTEGER PRIMARY KEY)",
		"CREATE TABLE kid(id INTEGER PRIMARY KEY, p REFERENCES par(id) ON DELETE CASCADE)",
		"CREATE TABLE hold(id INTEGER PRIMARY KEY, p REFERENCES par(id))",
		"INSERT INTO par VALUES(1)",
		"INSERT INTO kid VALUES(10,1),(11,1)",
		"INSERT INTO hold VALUES(20,1)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA foreign_keys=ON")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN DELETE FROM par WHERE id = new.a; END")

	p.agreeExec("BEGIN")
	// Both engines must REJECT: hold(20) still references par(1) after the
	// cascade emptied kid, so the routed DELETE fails its immediate check.
	// The transaction stays open across it.
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	// ...and this one must SUCCEED, which is what gives the attachment's
	// session a real commit to make (see 2 above).
	p.agreeExec("INSERT INTO aux.hold VALUES(21,1)")
	p.agreeExec("COMMIT")

	// Nothing the failed statement touched may have moved -- on either side of
	// the routed write -- and this is read back after the commit, from the
	// attachment's own file.
	p.agreeQuery("SELECT id FROM aux.par ORDER BY id")
	p.agreeQuery("SELECT id, p FROM aux.kid ORDER BY id")
	p.agreeQuery("SELECT id, p FROM aux.hold ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")

	// A later firing that SHOULD succeed still does -- the unwind must not
	// have wedged either session. Clearing hold makes the same DELETE legal,
	// and its cascade must now really happen.
	p.agreeExec("DELETE FROM aux.hold")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT id FROM aux.par ORDER BY id")
	p.agreeQuery("SELECT id, p FROM aux.kid ORDER BY id")
}

// TestEngineAttachRoutedTriggerBodyReadsConnectionRowid checks that
// last_insert_rowid() in a routed trigger body reads the connection's counter.
func TestEngineAttachRoutedTriggerBodyReadsConnectionRowid(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, seen)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.anchor(id INTEGER PRIMARY KEY, v)")
	p.agreeExec("CREATE TABLE main.locallog(id INTEGER PRIMARY KEY, seen)")
	p.agreeExec("CREATE TABLE main.t1(a INTEGER PRIMARY KEY)")
	// logged is aux-only (routed); locallog is main-only (not routed). The
	// two bodies are otherwise identical, so any difference between the two
	// answers is the routing.
	p.agreeExec("CREATE TEMP TRIGGER trgRouted AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(seen) VALUES(last_insert_rowid()); END")
	p.agreeExec("CREATE TEMP TRIGGER trgLocal AFTER INSERT ON main.t1 BEGIN INSERT INTO locallog(seen) VALUES(last_insert_rowid()); END")

	// Set the connection's counter to something no rowid in this script can
	// coincide with, so a session-local 0 (or a re-derived small rowid) is
	// distinguishable from the real answer.
	p.agreeExec("INSERT INTO main.anchor VALUES(77,'x')")
	p.agreeExec("INSERT INTO main.t1 VALUES(5)")

	p.agreeQuery("SELECT id, seen FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT id, seen FROM main.locallog ORDER BY id")
	// ...and the counter the connection is left holding afterwards, which a
	// routed INSERT must set exactly as a top-level "INSERT INTO aux.logged"
	// does (execRoutedToAttached's own rule).
	p.agreeQuery("SELECT last_insert_rowid()")
}

// TestEngineAttachRoutedTriggerBodyResolvesSourcesInTheConnection checks that
// a routed trigger body's source tables resolve in the connection's catalog.
func TestEngineAttachRoutedTriggerBodyResolvesSourcesInTheConnection(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v)",
		// Deliberately NOT holding "onlymain", and holding a "shadow" whose
		// row differs from main's.
		"CREATE TABLE shadow(x)",
		"INSERT INTO shadow VALUES(111)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.onlymain(x)")
	p.agreeExec("INSERT INTO main.onlymain VALUES(5)")
	p.agreeExec("CREATE TABLE main.shadow(x)")
	p.agreeExec("INSERT INTO main.shadow VALUES(9)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TABLE main.t2(a)")

	// logged is aux-only, so both bodies route; their sources do not.
	p.agreeExec("CREATE TEMP TRIGGER trgMissing AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) SELECT x FROM onlymain; END")
	p.agreeExec("CREATE TEMP TRIGGER trgShadow AFTER INSERT ON main.t2 BEGIN INSERT INTO logged(v) SELECT x FROM shadow; END")

	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("INSERT INTO main.t2 VALUES(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	// Neither source may have been touched, on either side.
	p.agreeQuery("SELECT x FROM main.shadow ORDER BY x")
	p.agreeQuery("SELECT x FROM aux.shadow ORDER BY x")
}

// TestEngineAttachRoutedTriggerBodyFiresTempTriggerOnTheTarget checks that
// TEMP triggers on the routed target still fire for the delegated write.
func TestEngineAttachRoutedTriggerBodyFiresTempTriggerOnTheTarget(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v)",
		"CREATE TABLE echo(id INTEGER PRIMARY KEY, v)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER onLogged AFTER INSERT ON aux.logged BEGIN INSERT INTO echo(v) VALUES(new.v * 10); END")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")

	p.agreeExec("INSERT INTO main.t1 VALUES(7)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT id, v FROM aux.echo ORDER BY id")

	// A second firing, after DROPping the mirrored trigger: the routed write
	// must stop firing it immediately. This is trigger1.test 10.9's own shape
	// -- a trigger change between two writes into the SAME already-open
	// delegated session -- and is why the mirror list is recomputed on every
	// firing rather than cached on it.
	p.agreeExec("DROP TRIGGER onLogged")
	p.agreeExec("INSERT INTO main.t1 VALUES(8)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT id, v FROM aux.echo ORDER BY id")
}

// TestEngineAttachRoutedTriggerBodyDeclinesUnmirrorableTempTrigger checks
// that unmirrorable TEMP triggers on routed targets cause the write to decline.
func TestEngineAttachRoutedTriggerBodyDeclinesUnmirrorableTempTrigger(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.audit(v)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER onLogged AFTER INSERT ON aux.logged BEGIN INSERT INTO audit VALUES(new.v); END")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")

	p.declineExec("INSERT INTO main.t1 VALUES(7)")
	p.agreeQuery("SELECT count(*) FROM aux.logged")
	p.agreeQuery("SELECT count(*) FROM main.t1")
	p.agreeQuery("SELECT count(*) FROM main.audit")

	// Once the trigger is gone the same statement must work again -- the
	// decline is about that trigger, not a wedged session.
	p.agreeExec("DROP TRIGGER onLogged")
	p.agreeExec("INSERT INTO main.t1 VALUES(7)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

// TestEngineAttachRoutedTriggerBodyKeepsStoredBodiesPinned checks that
// stored triggers in attachments keep their own database scope, not the connection's.
func TestEngineAttachRoutedTriggerBodyKeepsStoredBodiesPinned(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE dst(a)",
		"CREATE TABLE landing(a)",
		// Body names "mo", which exists ONLY in the originating main.
		"CREATE TRIGGER tro AFTER INSERT ON landing BEGIN INSERT INTO dst SELECT a FROM mo; END",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("CREATE TABLE main.mo(a)")
	p.agreeExec("INSERT INTO main.mo VALUES(8)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO landing VALUES(new.a); END")

	// Both must reject: aux's own stored trigger cannot see main's mo.
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT count(*) FROM aux.dst")
	p.agreeQuery("SELECT count(*) FROM aux.landing")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}
