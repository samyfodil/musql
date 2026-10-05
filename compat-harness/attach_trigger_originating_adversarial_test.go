// Adversarial coverage for attached triggers originating in the main session:
// shapes not exercised by the engine's own tests, run against the oracle.
package compat

import "testing"

// TestEngineAttachOriginatingTriggerMixedWithMirrorTrigger covers TWO
// triggers on the SAME attached table and event -- one mirror-safe (body
// resolves back into the attachment) and one originating-safe (body
// resolves back into this session) -- firing off the SAME single INSERT.
// This is the shape the shipped tests never combine: each of the bucket's
// own tests has exactly one relevant trigger per table.
func TestEngineAttachOriginatingTriggerMixedWithMirrorTrigger(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t4(a, b, c)",
		"CREATE TABLE aux_log(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main_log(x)")

	p.agreeExec("CREATE TEMP TRIGGER trigMirror AFTER INSERT ON aux.t4 BEGIN INSERT INTO aux_log(x) VALUES(new.a); END")
	p.agreeExec("CREATE TEMP TRIGGER trigOrig AFTER INSERT ON aux.t4 BEGIN INSERT INTO main_log(x) VALUES(new.a); END")

	p.agreeExec("INSERT INTO aux.t4 VALUES(7,8,9)")
	p.agreeQuery("SELECT x FROM aux.aux_log ORDER BY x")
	p.agreeQuery("SELECT x FROM main_log ORDER BY x")

	p.agreeExec("INSERT INTO aux.t4 VALUES(17,18,19)")
	p.agreeQuery("SELECT x FROM aux.aux_log ORDER BY x")
	p.agreeQuery("SELECT x FROM main_log ORDER BY x")
}

// TestEngineAttachOriginatingTriggerRowidCollision is a targeted stress of
// the row-lookup mechanism (attachedOriginatingFireInsert reads the just-
// inserted row back out of w's tableMeta by last_insert_rowid()): a mirror
// trigger on the SAME table cascades an insert into a DIFFERENT attached
// table, whose own auto-rowid is engineered to coincide with a PRE-EXISTING,
// stale rowid in the very table the originating trigger reads back from. If
// last_insert_rowid() bookkeeping (or its restore-on-trigger-exit) is even
// slightly off, this reads the stale row's values instead of the real one.
func TestEngineAttachOriginatingTriggerRowidCollision(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t4(a INTEGER PRIMARY KEY, b, c)",
		"CREATE TABLE log_aux(x)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(v)")

	p.agreeExec("CREATE TEMP TRIGGER trigM AFTER INSERT ON aux.t4 BEGIN INSERT INTO log_aux(x) VALUES(new.a); END")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received(v) VALUES(new.a); END")

	for i := 0; i < 100; i++ {
		p.agreeExec("INSERT INTO aux.log_aux(x) VALUES(0)")
	}
	// A stale row at rowid 101 in t4 -- the rowid log_aux's cascaded insert
	// will land on next.
	p.agreeExec("INSERT INTO aux.t4(a,b,c) VALUES(101,'stale','stale')")
	p.agreeQuery("SELECT v FROM received ORDER BY v")

	p.agreeExec("INSERT INTO aux.t4(a,b,c) VALUES(7,8,9)")
	p.agreeQuery("SELECT v FROM received ORDER BY v")
	p.agreeQuery("SELECT count(*) FROM aux.log_aux")
}

// TestEngineAttachOriginatingTriggerDefaultAndAffinity checks that NEW.* the
// originating trigger sees reflects the FINAL stored row -- a DEFAULT column
// fill-in and a TEXT-affinity coercion of a numeric literal -- not the raw
// statement text (attachedOriginatingFireInsert's own doc comment claims
// this; it is untested by the shipped battery, which only ever inserts
// fully-specified integer literals).
func TestEngineAttachOriginatingTriggerDefaultAndAffinity(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE t4(a INTEGER PRIMARY KEY, b TEXT, c DEFAULT 42)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a, b, c)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a, new.b, new.c); END")

	// b is TEXT affinity: the integer literal 99 must be stored (and read
	// back by NEW.b) as the coerced text '99'. c is omitted: DEFAULT 42 must
	// show up in NEW.c.
	p.agreeExec("INSERT INTO aux.t4(a,b) VALUES(1,99)")
	p.agreeQuery("SELECT a, b, typeof(b), c FROM received")
}

// TestEngineAttachOriginatingTriggerAutoRowid checks the row-lookup path
// when the INSERT relies on an AUTO-assigned rowid (no explicit INTEGER
// PRIMARY KEY value given at all) rather than an explicit one -- every
// shipped test gives an explicit rowid/IPK value.
func TestEngineAttachOriginatingTriggerAutoRowid(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(rid, a)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.rowid, new.a); END")

	p.agreeExec("INSERT INTO aux.t4(a,b,c) VALUES(5,6,7)")
	p.agreeExec("INSERT INTO aux.t4(a,b,c) VALUES(15,16,17)")
	p.agreeQuery("SELECT rid, a FROM received ORDER BY rid")
	p.agreeQuery("SELECT last_insert_rowid()")
}

// TestEngineAttachOriginatingTriggerWhenClause checks a WHEN clause that
// filters which rows actually fire -- the shipped battery's trig3 has no
// WHEN clause at all.
func TestEngineAttachOriginatingTriggerWhenClause(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 WHEN new.a > 10 BEGIN INSERT INTO received VALUES(new.a); END")

	p.agreeExec("INSERT INTO aux.t4 VALUES(5,1,1)")  // WHEN false: must not fire
	p.agreeExec("INSERT INTO aux.t4 VALUES(15,1,1)") // WHEN true: must fire
	p.agreeQuery("SELECT a FROM received ORDER BY a")
}

// TestEngineAttachOriginatingTriggerExplicitTxnPartialRollback checks that a
// FIRST statement's originating-trigger effect, already committed to an open
// explicit transaction, survives a LATER statement's own trigger failure
// (ROLLBACK is never issued -- the failing statement alone must undo, not
// the whole transaction). This is a narrower slice of atomicity than
// TestAttachedOriginatingTriggerFailureUndoesBothSessions, which never wraps
// an explicit BEGIN around the sequence.
func TestEngineAttachOriginatingTriggerExplicitTxnPartialRollback(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a UNIQUE)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a); END")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.t4 VALUES(1,2,3)") // trigO fires, received gets a=1
	p.agreeExec("INSERT INTO aux.t4 VALUES(1,9,9)") // trigO's own body collides on received.a UNIQUE: must fail
	p.agreeQuery("SELECT a FROM received ORDER BY a")
	p.agreeQuery("SELECT a,b,c FROM aux.t4 ORDER BY a")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a FROM received ORDER BY a")
	p.agreeQuery("SELECT a,b,c FROM aux.t4 ORDER BY a")
}

// TestEngineAttachOriginatingTriggerWithoutRowidTarget checks a WITHOUT
// ROWID target table -- attachedOriginatingFireInsert's own doc comment
// says this cannot be captured (tableMeta.withoutRowid's storage key is not
// a real rowid) and refuses. The shipped battery never uses a WITHOUT ROWID
// table, so its refusal is unverified against the oracle: both sides must
// still agree the STATEMENT itself is accepted by C SQLite (with the
// trigger firing) while confirming musql's decline leaves no partial
// write, rather than silently skipping the trigger.
func TestEngineAttachOriginatingTriggerWithoutRowidTarget(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a TEXT PRIMARY KEY, b) WITHOUT ROWID")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a); END")

	// C SQLite accepts and fires; musql is allowed to decline (never
	// silently skip) -- so this is a declineExec-shaped probe, not agreeExec.
	// What matters is: no partial write on the musql side, whichever way it
	// goes.
	goSQL := "INSERT INTO aux.t4 VALUES('k','v')"
	_, _, goErr := p.godb.ExecArgs(goSQL, nil)
	pager, perr := p.godb.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}
	defer pager.Close()
	_, rows, qerr := pager.QueryArgs("SELECT count(*) FROM aux.t4", nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	rowExists := rows[0][0].I == 1
	_, rows2, qerr2 := pager.QueryArgs("SELECT count(*) FROM received", nil)
	if qerr2 != nil {
		t.Fatal(qerr2)
	}
	triggerFired := rows2[0][0].I == 1
	if rowExists != triggerFired {
		t.Fatalf("WITHOUT ROWID originating shape left an inconsistent state: goErr=%v row-inserted=%v trigger-fired=%v -- a delegated row must never be committed with its trigger silently unfired (or vice versa)", goErr, rowExists, triggerFired)
	}
}

// TestEngineAttachOriginatingTriggerFailureUndoesBothSessionsAgainstOracle
// is the differential (real-oracle-comparing) counterpart to attach_write_
// test.go's own TestAttachedOriginatingTriggerFailureUndoesBothSessions,
// which only ever asserts musql's own before/after row counts -- it never
// confirms against C SQLite that a trigger body's own constraint failure
// really does roll back the delegated base-table INSERT too, rather than
// leaving it committed with only the trigger's effect undone. This closes
// that gap: agreeExec + agreeQuery below fail loudly if musql's assumed
// "one real-SQLite statement, one atomic unit" premise for this shape is
// actually wrong.
func TestEngineAttachOriginatingTriggerFailureUndoesBothSessionsAgainstOracle(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a, b, c)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a UNIQUE)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a); END")

	p.agreeExec("INSERT INTO aux.t4 VALUES(1,2,3)") // trigO fires fine, received={1}
	// trigO's OWN body now collides on received.a UNIQUE: C SQLite fails
	// the whole statement, undoing aux.t4's own just-inserted row too.
	p.agreeExec("INSERT INTO aux.t4 VALUES(1,9,9)")
	p.agreeQuery("SELECT a,b,c FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT a FROM received ORDER BY a")

	// A later, unrelated insert must still work normally on both sides.
	p.agreeExec("INSERT INTO aux.t4 VALUES(2,5,6)")
	p.agreeQuery("SELECT a,b,c FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT a FROM received ORDER BY a")
}

// TestEngineAttachOriginatingTriggerUpdateAndDeleteEvents runs an
// originating-safe TEMP trigger bound to the UPDATE and DELETE events of an
// ATTACHed table, statement for statement against the oracle.
//
// It used to assert a DECLINE, and the decline was real: routeAttachedStatement
// built no OLD image and no multi-row capture, so only a single-row INSERT
// could fire such a trigger. Both events fire from the attachment's own change
// capture now (attach_write.go's attachedOriginatingFireRows), which is what
// this checks -- including a MULTI-row UPDATE, where the per-row fire order is
// the order the attachment applied them, and a DELETE, whose OLD image no
// read-back could have recovered.
func TestEngineAttachOriginatingTriggerUpdateAndDeleteEvents(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a INTEGER PRIMARY KEY, b)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("INSERT INTO aux.t4 VALUES(1,'x')")
	p.agreeExec("INSERT INTO aux.t4 VALUES(2,'p')")
	p.agreeExec("CREATE TABLE received(kind, oldb, newb)")
	p.agreeExec("CREATE TEMP TRIGGER trigU AFTER UPDATE ON aux.t4 BEGIN INSERT INTO received VALUES('u', old.b, new.b); END")
	p.agreeExec("CREATE TEMP TRIGGER trigD AFTER DELETE ON aux.t4 BEGIN INSERT INTO received VALUES('d', old.b, NULL); END")

	p.agreeExec("UPDATE aux.t4 SET b='y' WHERE a=1")
	p.agreeQuery("SELECT a,b FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT kind,oldb,newb FROM received ORDER BY rowid")

	// A MULTI-row UPDATE: one fire per row, and the OLD image is each row's own.
	p.agreeExec("UPDATE aux.t4 SET b=b||'!'")
	p.agreeQuery("SELECT a,b FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT kind,oldb,newb FROM received ORDER BY rowid")

	p.agreeExec("DELETE FROM aux.t4 WHERE a=2")
	p.agreeQuery("SELECT a,b FROM aux.t4 ORDER BY a")
	p.agreeQuery("SELECT kind,oldb,newb FROM received ORDER BY rowid")
}

// TestEngineAttachOriginatingTriggerDefaultValues checks the "INSERT INTO t
// DEFAULT VALUES" shape: parseInsertStmt represents it as rows=[one empty
// tuple], so attachedOriginatingInsertCapturable's len(stmt.rows)==1 check
// accepts it -- untested by the shipped battery, which always writes an
// explicit VALUES list.
func TestEngineAttachOriginatingTriggerDefaultValues(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a INTEGER PRIMARY KEY, b DEFAULT 'd')")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a, b)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a, new.b); END")

	p.agreeExec("INSERT INTO aux.t4 DEFAULT VALUES")
	p.agreeQuery("SELECT a, b FROM received")
}

// TestEngineAttachOriginatingTriggerGeneratedColumn checks NEW.* against a
// GENERATED ALWAYS AS column -- a computed, not literally-inserted, value --
// which AGENTS.md flags as a past silent-ignore bug class in
// this codebase. The shipped battery never uses a generated column.
func TestEngineAttachOriginatingTriggerGeneratedColumn(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a INTEGER PRIMARY KEY, b, c GENERATED ALWAYS AS (a*2) STORED)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a, c)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a, new.c); END")

	p.agreeExec("INSERT INTO aux.t4(a,b) VALUES(5,'x')")
	p.agreeQuery("SELECT a, c FROM received")
}

// TestEngineAttachOriginatingTriggerReplaceEventDeclines checks that
// "INSERT OR REPLACE", which C SQLite fires as a plain INSERT event
// (not a distinct REPLACE event) when it does not collide, is still
// refused by attachedOriginatingInsertCapturable's explicitOr gate even in
// the NO-COLLISION case where firing would have been representable. This
// pins that the refusal doesn't accidentally depend on whether a collision
// actually occurs.
func TestEngineAttachOriginatingTriggerReplaceEventDeclines(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t4(a INTEGER PRIMARY KEY, b)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE received(a)")
	p.agreeExec("CREATE TEMP TRIGGER trigO AFTER INSERT ON aux.t4 BEGIN INSERT INTO received VALUES(new.a); END")

	// No collision at all -- C SQLite fires trigO normally. musql is
	// allowed to decline (explicitOr is a blanket refusal), never to
	// silently skip firing while accepting the row.
	goSQL := "INSERT OR REPLACE INTO aux.t4 VALUES(1,2)"
	_, _, goErr := p.godb.ExecArgs(goSQL, nil)
	if goErr == nil {
		// If musql accepted it, the trigger MUST have fired -- verify
		// against the oracle for full agreement.
		if _, err := p.cgo.Exec(p.stmt("INSERT OR REPLACE INTO aux.t4 VALUES(1,2)", p.cgoPaths)); err != nil {
			t.Fatalf("cgo setup: %v", err)
		}
		p.agreeQuery("SELECT a FROM received")
	}
}
