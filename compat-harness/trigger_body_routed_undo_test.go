// Tests ATTACH write operations and undo behavior in trigger bodies.
package compat

import "testing"

// Statement abort after routed writes with UNIQUE violation.
func TestEngineAttachRoutedBodyAbortUndoesRoutedRows(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a UNIQUE)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(-1)") // a survivor, so the session really commits
	p.agreeExec("INSERT INTO main.t1 VALUES(1),(2),(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM aux.logged")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT a FROM main.t1 ORDER BY a")
}

// The same undo reached through a BEFORE trigger, whose routed body has already
// run by the time the firing statement fails its own NOT NULL check.
func TestEngineAttachRoutedBodyBeforeTriggerUndoneWithTheStatement(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a NOT NULL)")
	p.agreeExec("CREATE TEMP TRIGGER trg BEFORE INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(7); END")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(-1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(NULL)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}

// ...and through a RAISE(ABORT) raised by a LATER step of the same trigger.
func TestEngineAttachRoutedBodyUndoneByRaiseAbort(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"INSERT INTO logged(v) VALUES(new.a); " +
		"SELECT RAISE(ABORT,'nope'); END")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(-1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}

// ON CONFLICT FAIL inside a routed body: the surviving rows are real, and the
// connection has to be able to see them.
func TestEngineAttachRoutedBodyConflictFailRowsStayVisible(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v UNIQUE)",
		"CREATE TABLE src(v)",
		"INSERT INTO src VALUES(1),(2),(3)",
		"INSERT INTO logged VALUES(9,3)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT OR FAIL INTO logged(v) SELECT v FROM src ORDER BY v; END")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(-1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

// The same ON CONFLICT FAIL shape reached by a TOP-LEVEL routed write, which
// execRoutedToAttached used to return from before its own
// refreshAttachedWriteReaders. Kept next to the routed-body spelling because
// the two are one defect with two doors, and only fixing the door the trigger
// body opened would leave the other one measuring nothing.
func TestEngineAttachTopLevelRouteConflictFailRowsStayVisible(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v UNIQUE)",
		"CREATE TABLE src(v)",
		"INSERT INTO src VALUES(1),(2),(3)",
		"INSERT INTO logged VALUES(9,3)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(-1)")
	p.agreeExec("INSERT OR FAIL INTO aux.logged(v) SELECT v FROM src ORDER BY v")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

// Two DIFFERENT attachments written by one trigger, the second statement
// failing: the first attachment's rows go back with it, and its reader says so.
func TestEngineAttachRoutedBodyTwoAttachmentsUndoTogether(t *testing.T) {
	goA, cgoA := buildAuxPair(t, "a1", "CREATE TABLE la(id INTEGER PRIMARY KEY, v)")
	goB, cgoB := buildAuxPair(t, "a2", "CREATE TABLE lb(id INTEGER PRIMARY KEY, v UNIQUE)", "INSERT INTO lb VALUES(1,9)")
	p := newAttachPair(t, []string{goA, goB}, []string{cgoA, cgoB})
	p.agreeExec("ATTACH '{0}' AS a1")
	p.agreeExec("ATTACH '{1}' AS a2")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"INSERT INTO la(v) VALUES(new.a); " +
		"INSERT INTO lb(v) VALUES(9); END")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO a1.la(v) VALUES(-1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(5)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM a1.la ORDER BY id")
	p.agreeQuery("SELECT id, v FROM a2.lb ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}

// SAVEPOINT / ROLLBACK TO over a routed body's write. txn.go's
// rollbackToSavepoint already re-publishes the reader after undoing an
// attachment's sub-transaction; this is the routed-body shape reaching it.
func TestEngineAttachRoutedBodyRollbackToSavepoint(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeExec("ROLLBACK TO s1")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT a FROM main.t1 ORDER BY a")
}

// PRAGMA recursive_triggers is per CONNECTION and C SQLite reads it off
// db->flags when it codes each OP_Program (trigger.c:1412), with no per-schema
// copy. The delegated session was born with the default, so a TEMP trigger
// mirrored onto it stopped chaining after one level. Both doors to it are
// pinned: the routed trigger body here, and the top-level route below.
func TestEngineAttachRoutedBodyHonoursRecursiveTriggers(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA recursive_triggers=ON")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER onLogged AFTER INSERT ON aux.logged WHEN new.v < 4 BEGIN INSERT INTO logged(v) VALUES(new.v + 1); END")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

func TestEngineAttachTopLevelRouteHonoursRecursiveTriggers(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA recursive_triggers=ON")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TEMP TRIGGER onLogged AFTER INSERT ON aux.logged WHEN new.v < 4 BEGIN INSERT INTO logged(v) VALUES(new.v + 1); END")
	p.agreeExec("INSERT INTO aux.logged(v) VALUES(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}
