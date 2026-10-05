// Tests routed trigger body behavior with ATTACH operations and counters.
package compat

import "testing"

// Changes() accounting in routed trigger bodies.
func TestEngineAttachRoutedBodyChangesAccounting(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v)",
		"INSERT INTO logged VALUES(1,1),(2,2),(3,3)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TABLE main.seen(c)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"UPDATE logged SET v = v + 1; " +
		"INSERT INTO seen VALUES(changes()); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT c FROM main.seen")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT changes()")
	p.agreeQuery("SELECT total_changes()")
}

// The LOCAL twin of the above: the two spellings must agree with each other as
// well as with the oracle.
func TestEngineLocalBodyChangesAccounting(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("CREATE TABLE main.logged(id INTEGER PRIMARY KEY, v)")
	p.agreeExec("INSERT INTO main.logged VALUES(1,1),(2,2),(3,3)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TABLE main.seen(c)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"UPDATE logged SET v = v + 1; " +
		"INSERT INTO seen VALUES(changes()); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT c FROM main.seen")
	p.agreeQuery("SELECT id, v FROM main.logged ORDER BY id")
	p.agreeQuery("SELECT changes()")
	p.agreeQuery("SELECT total_changes()")
}

// read-your-writes WITHIN the firing statement: a later body statement reads
// back what an earlier one routed.
func TestEngineAttachRoutedBodyLaterStepReadsIt(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TABLE main.seen(n)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"INSERT INTO logged(v) VALUES(new.a); " +
		"INSERT INTO seen SELECT count(*) FROM logged; END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT n FROM main.seen ORDER BY rowid")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

// OLD and NEW through a routed body, with the firing statement an UPDATE (which
// carries both images).
func TestEngineAttachRoutedBodyOldAndNewImages(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, o, n)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a, b)")
	p.agreeExec("INSERT INTO main.t1 VALUES(1,'x'),(2,'y')")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER UPDATE ON main.t1 BEGIN INSERT INTO logged(o,n) VALUES(old.a || '/' || old.b, new.a || '/' || new.b); END")
	p.agreeExec("UPDATE main.t1 SET a = a + 100")
	p.agreeQuery("SELECT id, o, n FROM aux.logged ORDER BY id")
}

// The body's SOURCE names bind in the CONNECTION, so a TEMP table of this
// session and a SECOND attachment both resolve -- the far half of
// wireOriginatingReaders' ordering, which hazard 4 only exercises for main.
func TestEngineAttachRoutedBodySourcesTempAndSecondAttachment(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	goOther, cgoOther := buildAuxPair(t, "other", "CREATE TABLE far(x)", "INSERT INTO far VALUES(41)")
	p := newAttachPair(t, []string{goAux, goOther}, []string{cgoAux, cgoOther})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("ATTACH '{1}' AS other")
	p.agreeExec("CREATE TEMP TABLE tt(x)")
	p.agreeExec("INSERT INTO tt VALUES(99)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trgTemp AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) SELECT x FROM tt; END")
	p.agreeExec("CREATE TABLE main.t2(a)")
	p.agreeExec("CREATE TEMP TRIGGER trgFar AFTER INSERT ON main.t2 BEGIN INSERT INTO logged(v) SELECT x FROM far; END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeExec("INSERT INTO main.t2 VALUES(1)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
}

// ON CONFLICT IGNORE skips its row and lets the firing statement carry on; an
// unqualified conflict aborts everything.
func TestEngineAttachRoutedBodyConflictClauses(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v UNIQUE)",
		"INSERT INTO logged VALUES(1,5)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT OR IGNORE INTO logged(v) VALUES(new.a); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(5)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT a FROM main.t1 ORDER BY a")

	p.agreeExec("CREATE TABLE main.t2(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg2 AFTER INSERT ON main.t2 BEGIN INSERT INTO logged(v) VALUES(new.a); END")
	p.agreeExec("INSERT INTO main.t2 VALUES(5)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t2")
}

// A REPLACE conflict on the routed target, whose victim delete has to fire the
// mirrored TEMP trigger's DELETE arm on the delegated session.
func TestEngineAttachRoutedBodyReplaceVictimFiresMirroredDelete(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(k INTEGER PRIMARY KEY, v)",
		"CREATE TABLE gone(k, v)",
		"INSERT INTO logged VALUES(1,'old')",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER onDel AFTER DELETE ON aux.logged BEGIN INSERT INTO gone VALUES(old.k, old.v); END")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT OR REPLACE INTO logged(k,v) VALUES(1,'new'); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT k, v FROM aux.logged ORDER BY k")
	p.agreeQuery("SELECT k, v FROM aux.gone ORDER BY k")
}

// The ATTACHMENT's own column rules -- its DEFAULTs, its NOT NULL, its STORED
// generated column, its INTEGER PRIMARY KEY -- decide a routed INSERT, and its
// declared collation decides a routed DELETE's comparison.
func TestEngineAttachRoutedBodyUsesAttachedColumnRules(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v INTEGER NOT NULL DEFAULT 42, d TEXT DEFAULT 'dd', g AS (v*2) STORED)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN INSERT INTO logged(v) VALUES(new.a); END")
	p.agreeExec("CREATE TABLE main.t2(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg2 AFTER INSERT ON main.t2 BEGIN INSERT INTO logged(d) VALUES('z'); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(7)")
	p.agreeExec("INSERT INTO main.t2 VALUES(1)")
	p.agreeQuery("SELECT id, v, d, g FROM aux.logged ORDER BY id")
	p.agreeExec("INSERT INTO main.t1 VALUES(NULL)") // NOT NULL, declared inside aux
	p.agreeQuery("SELECT id, v, d, g FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}

func TestEngineAttachRoutedBodyUsesAttachedCollationAndAffinity(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(k TEXT COLLATE NOCASE, v INTEGER)",
		"INSERT INTO logged VALUES('ABC', 1), ('def', 2)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN DELETE FROM logged WHERE k = new.a; END")
	p.agreeExec("CREATE TABLE main.t2(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg2 AFTER INSERT ON main.t2 BEGIN INSERT INTO logged(k,v) VALUES(new.a, new.a); END")
	p.agreeExec("INSERT INTO main.t1 VALUES('abc')")
	p.agreeQuery("SELECT k, v, typeof(k), typeof(v) FROM aux.logged ORDER BY rowid")
	p.agreeExec("INSERT INTO main.t2 VALUES('77')")
	p.agreeQuery("SELECT k, v, typeof(k), typeof(v) FROM aux.logged ORDER BY rowid")
}

// An INSTEAD OF TEMP trigger on a MAIN view whose body routes: the OTHER fire
// plan the patch taught trig.isTemp (engine/vdbe_view_write.go), which nothing
// else in this directory reaches.
func TestEngineAttachRoutedBodyFromInsteadOfTriggerOnMainView(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE logged(id INTEGER PRIMARY KEY, v)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.base(a)")
	p.agreeExec("CREATE VIEW main.vw AS SELECT a FROM base")
	p.agreeExec("CREATE TEMP TRIGGER io INSTEAD OF INSERT ON main.vw BEGIN INSERT INTO logged(v) VALUES(new.a); END")
	p.agreeExec("INSERT INTO main.vw VALUES(5)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.base")
}

// A WHEN guard reading MAIN while the body writes the attachment, and a routed
// DELETE whose WHERE reads MAIN.
func TestEngineAttachRoutedBodyGuardAndWhereReadMain(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE logged(id INTEGER PRIMARY KEY, v)",
		"INSERT INTO logged VALUES(1,10),(2,20),(3,30)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.gate(ok)")
	p.agreeExec("INSERT INTO main.gate VALUES(0)")
	p.agreeExec("CREATE TABLE main.keep(v)")
	p.agreeExec("INSERT INTO main.keep VALUES(20)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 WHEN (SELECT ok FROM gate) = 1 BEGIN DELETE FROM logged WHERE v NOT IN (SELECT v FROM keep); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)") // guard false: nothing happens
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeExec("UPDATE main.gate SET ok = 1")
	p.agreeExec("INSERT INTO main.t1 VALUES(2)")
	p.agreeQuery("SELECT id, v FROM aux.logged ORDER BY id")
	p.agreeQuery("SELECT changes()")
}

// KNOWN DIVERGENCE, pinned as a decline rather than left to be re-found.
//
// An IMMEDIATE foreign key is counted for the WHOLE VDBE STATEMENT in real
// SQLite and checked exactly once, when that statement halts: Vdbe.nFkConstraint
// is "Number of imm. FK constraints this VM" (vdbeInt.h:471), bumped by
// OP_FkCounter (vdbe.c:7640), zeroed only by sqlite3VdbeRewind (vdbeaux.c:2620)
// and read by sqlite3VdbeCheckFkImmediate (vdbeaux.c:3291-3294) from
// sqlite3VdbeHalt (vdbeaux.c:3392). A trigger body runs as a FRAME inside that
// same VM, so a violation one body statement creates and the next repairs never
// fires -- verified: the oracle accepts the statement below and ends with
// aux.par holding 1,2 and aux.kid holding (11,2).
//
// runRoutedTriggerBody gives each routed body statement its own
// fkBeginStatement/fkFinishStatement pair on the delegated session, so the check
// lands halfway through and the engine reports "FOREIGN KEY constraint failed".
// It is the ERROR direction, never a silent wrong answer -- an earlier check can
// only add violations, never miss one -- and the whole firing statement unwinds
// atomically, which the queries below assert. The LOCAL spelling of the same
// trigger is correct (TestEngineLocalBodyFKTransientAcrossTwoBodyStatements),
// which is what makes this the routing's own defect rather than the FK model's.
func TestEngineAttachRoutedBodyFKStatementScopeIsKnownDivergent(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux",
		"CREATE TABLE par(id INTEGER PRIMARY KEY)",
		"CREATE TABLE kid(id INTEGER PRIMARY KEY, p REFERENCES par(id))",
		"INSERT INTO par VALUES(1)",
	)
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("PRAGMA foreign_keys=ON")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"INSERT INTO kid VALUES(11,2); " +
		"INSERT INTO par VALUES(2); END")
	p.declineExec("INSERT INTO main.t1 VALUES(1)")
	// The decline is TOTAL on both sides of the routed write.
	p.agreeQuery("SELECT id FROM aux.par ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM aux.kid")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}

// The LOCAL twin of the divergence above, which the engine gets right -- kept
// so a future change that "fixes" the routed side by breaking the local one
// cannot pass.
func TestEngineLocalBodyFKTransientAcrossTwoBodyStatements(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("PRAGMA foreign_keys=ON")
	p.agreeExec("CREATE TABLE main.par(id INTEGER PRIMARY KEY)")
	p.agreeExec("CREATE TABLE main.kid(id INTEGER PRIMARY KEY, p REFERENCES par(id))")
	p.agreeExec("INSERT INTO main.par VALUES(1)")
	p.agreeExec("CREATE TABLE main.t1(a)")
	p.agreeExec("CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1 BEGIN " +
		"INSERT INTO kid VALUES(11,2); " +
		"INSERT INTO par VALUES(2); END")
	p.agreeExec("INSERT INTO main.t1 VALUES(1)")
	p.agreeQuery("SELECT id FROM main.par ORDER BY id")
	p.agreeQuery("SELECT id, p FROM main.kid ORDER BY id")
	p.agreeQuery("SELECT count(*) FROM main.t1")
}
