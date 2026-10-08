// This file lowers one shape: a TEMP trigger's INSERT/UPDATE/DELETE body
// statement whose unqualified target is not in this session's own catalogs
// but is in an ATTACHed database.
//
//	ATTACH 'x.db' AS aux                    -- aux holds logged(id INTEGER PRIMARY KEY, v)
//	CREATE TABLE main.t1(a)
//	CREATE TEMP TRIGGER trg AFTER INSERT ON main.t1
//	  BEGIN INSERT INTO logged(v) VALUES(new.a); END
//	INSERT INTO main.t1 VALUES(42)          -- the row lands in aux.logged
//
// # Why C allows it
//
// A TEMP trigger is pinned to no schema: sqlite3FixInit sets "pFix->bTemp =
// (iDb==1);" (attach.c:547), fixSelectCb's pinning is under "if(
// pFix->bTemp==0 ...)" (attach.c:495), and attach.c:565-566 says TEMP objects
// "are allowed to refer to anything". So the body's target is re-resolved on
// every firing by the ordinary temp, main, attachments search (sqlite3FindTable,
// build.c:374-384), and the write lands in the resolved table's database
// (insert.c:966, delete.c:346, update.c:363). A non-temp trigger's body is
// pinned (trigger.c:346) and can never reach an attachment;
// trigCompileCtx.isTemp is that split.
//
// # Why the write is a real statement on the attachment's session
//
// The write session materializes one file, so a write into an attachment is
// delegated to that database's own *DB. Swapping writeCtx.db under an
// already-compiled body would silently break three things: foreign keys
// (fkRowMutated needs db.fkStmt, which only runWrite sets up), FK undo closures
// (fkJournal writes to db.activeWC), and last_insert_rowid() (connection-wide
// in C). So the body runs as a statement on the attached session with its own
// writeCtx and FK statement pairing, connection counters carried both ways,
// and the firing row's OLD/NEW bound -- what execRoutedToAttached does for a
// top-level routed write.
//
// # Why it compiles at fire time
//
// It compiles against the attached session's catalog, and that session opens
// lazily on first write and closes at COMMIT. Compiling with the firing
// statement would open it (with a BUSY failure mode) for a trigger that may
// never fire, and bake its *tableMeta pointers into a cached program that
// outlives them. Compiling at fire time is still only opcodes (RULE #1), the
// same re-lowering compileLiveSubProgram does (cf. sqlite3Reprepare). It is
// memoized on the attachment's schema/transaction generation under
// cachedWriteProgram's guards.
//
// # What the delegated session is told
//
// Re-derived on every firing, since either can change between writes into the
// same open session (trigger1.test 10.9):
//
//   - the originating connection's read scope (wireOriginatingReaders), because
//     a TEMP trigger's body binds its source names through the connection's
//     search, not inside the attachment -- see fireRoutedTriggerBody;
//   - this session's TEMP triggers on the attachment's tables
//     (SetForeignTempTriggers), which the delegated session would otherwise not
//     fire -- see routedTriggerBodyMirrorable.
//
// Either changes only through DDL, which bumps db.schemaGen or w.schemaGen --
// the memoization's own guards.
package engine

import (
	"fmt"
	"slices"
)

// routedTriggerBody is OpTriggerBodyRouted's P4: everything the fire-time
// compile and run need, and no *DB pointer at all -- which session the body
// runs on is re-resolved per firing, exactly as the C re-resolves the name
// per statement (build.c:374-384).
type routedTriggerBody struct {
	bs    triggerBodyStmt
	table string // the body statement's own target name

	// schema is the attachment a QUALIFIED target names ("INSERT INTO aux.t"
	// in a TEMP trigger's body), "" for an unqualified one, which is found by
	// its name alone.
	schema string

	// trig is the firing context the body was compiled under -- OLD/NEW
	// widths and column list, and the enclosing statement's imposed ON
	// CONFLICT policy (trigger.c:1137's pParse->eOrconf). memo is deliberately
	// NOT carried: the attached session's compile must build its own
	// TriggerPrg list, since a compiledTrigger from THIS session's list holds
	// sub-programs bound to THIS session's tables.
	trig trigCompileCtx

	// prog/progFor/progGen/progTx memoize the fire-time compile under
	// cachedWriteProgram's own three guards (vdbe_write.go): the session it
	// was compiled against, that session's DDL generation, and its
	// rollback/restore generation. A program whose subqueries read a frozen
	// snapshot (WritePager != nil) is never memoized, for the identical
	// reason -- reusing it would answer a later firing from an earlier
	// firing's view of the database.
	prog    *Program
	progFor *DB
	progGen uint32
	progTx  uint32
}

// routedTriggerBodyTarget reports the body statement's target name when it is
// the shape this file lowers, and "" otherwise. The refusals are C's own:
//
//   - a non-TEMP trigger's body is schema-pinned (trigCompileCtx.isTemp);
//   - a name this session's own catalogs hold wins, since TEMP and MAIN precede
//     every attachment in sqlite3FindTable;
//   - a name no attachment holds is missing, reported by the ordinary compiler.
//
// A target qualified with an attachment's name routes straight there: only a
// TEMP trigger may carry one (triggerStepAllocate, trigger.c:478-485), and
// sqlite3LocateTableItem then looks in that database alone (build.c:487).
func (db *DB) routedTriggerBodyTarget(bs triggerBodyStmt, trig *trigCompileCtx) (name, attached string) {
	if trig == nil || !trig.isTemp || len(db.attached) == 0 {
		return "", ""
	}
	var schema string
	switch {
	case bs.insert != nil:
		name, schema = bs.insert.table, bs.insert.schema
	case bs.update != nil:
		name, schema = bs.update.table, bs.update.schema
	case bs.delete != nil:
		name, schema = bs.delete.table, bs.delete.schema
	default:
		return "", ""
	}
	if name == "" {
		return "", ""
	}
	if schema != "" {
		if equalFoldName(schema, localSchemaOr(db.localSchema)) || isTempSchemaQualifier(schema) || db.attachedNamed(schema) == nil {
			return "", ""
		}
		return name, schema
	}
	if db.findTableMeta(name) != nil || db.findViewMeta(name) != nil || db.findVtabMeta(name) != nil {
		return "", ""
	}
	if db.firstAttachedTable(name) == nil {
		return "", ""
	}
	return name, ""
}

// compileRoutedTriggerBody emits the one-instruction sub-program that stands
// in for a body statement resolving into an attachment. CountsChanges is set
// for the same reason every other DML body step sets it -- trigger.c emits an
// OP_ResetCount after each of its three DML arms (:1157/:1166/:1174) -- so
// runOneTrigger publishes the routed statement's own row count as changes().
func (db *DB) compileRoutedTriggerBody(bs triggerBodyStmt, trig *trigCompileCtx, name, attached string) (*Program, error) {
	rb := &routedTriggerBody{bs: bs, table: name, schema: attached, trig: *trig}
	rb.trig.memo = nil // see the field's own doc comment
	c := &compiler{}
	c.emit(Instruction{Op: OpTriggerBodyRouted, P4: rb})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, CountsChanges: true}, nil
}

// programFor returns the body statement compiled against w's catalog, reusing
// the memoized one when it is still valid for w.
func (rb *routedTriggerBody) programFor(w *DB) (*Program, error) {
	if rb.prog != nil && rb.progFor == w && rb.progGen == w.schemaGen && rb.progTx == w.txGen {
		return rb.prog, nil
	}
	trig := rb.trig
	trig.memo = newTriggerPrgMemo()
	prog, err := w.compileTriggerBodyStmt(rb.bs, &trig)
	if err != nil {
		return nil, err
	}
	rb.prog, rb.progFor, rb.progGen, rb.progTx = nil, nil, 0, 0
	if prog.WritePager == nil {
		rb.prog, rb.progFor, rb.progGen, rb.progTx = prog, w, w.schemaGen, w.txGen
	}
	return prog, nil
}

// opTriggerBodyRouted runs one routed body statement for the firing row this
// machine carries.
func (m *vdbe) opTriggerBodyRouted(op *Instruction) error {
	rb := op.P4.(*routedTriggerBody)
	db := m.wctx.db
	// Re-resolved per firing, never trusted from the compile: build.c:374-384
	// runs on every statement, and trigger1.test 10.9-10.10 is precisely a
	// session in which the answer CHANGES mid-run ("the reference within
	// trig3's program is re-resolved at statement compile time, not trigger
	// installation time", that file's own comment).
	if rb.schema == "" && (db.findTableMeta(rb.table) != nil || db.findViewMeta(rb.table) != nil || db.findVtabMeta(rb.table) != nil) {
		// Unreachable: creating the name locally is DDL, which bumps
		// db.schemaGen and so throws away the cached write program this
		// instruction belongs to (cachedWriteProgram). Stated as an error
		// rather than assumed, because the wrong answer it would otherwise
		// hide -- the row landing in the attachment while TEMP/MAIN outranks
		// it -- is exactly what invariant 2 forbids.
		return fmt.Errorf("engine: internal: trigger body target %s now resolves locally but its program was compiled to route into an ATTACHed database", rb.table)
	}
	var ad *attachedDB
	if rb.schema != "" {
		if ad = db.attachedNamed(rb.schema); ad == nil {
			return fmt.Errorf("engine: no such table: %s.%s", rb.schema, rb.table)
		}
	} else if ad = db.firstAttachedTable(rb.table); ad == nil {
		return fmt.Errorf("engine: no such table: %s", rb.table)
	}
	// Classified BEFORE anything is opened or written, so a shape this cannot
	// reproduce is an honest decline rather than a write with a trigger left
	// unfired -- see routedTriggerBodyMirrorable.
	if uerr := db.routedTriggerBodyMirrorable(ad.name, rb.table, routedBodyEvents(rb.bs)); uerr != nil {
		return uerr
	}
	// Mirrors routeAttachedStatement's identical ordering (attach_write.go): a
	// delegated write made INSIDE a transaction reopens the attachment's
	// session first, so a later ROLLBACK undoes it the same way it undoes this
	// session's own writes.
	// A refused write leaves the alias's lock state as it found it -- see the
	// identical save/restore in routeAttachedStatement (attach_write.go).
	wasTouched := ad.touchedInTxn
	if db.inTransaction() {
		if err := db.enterTxnAttachedWrite(ad.name); err != nil {
			return err
		}
	}
	// forWrite=true: a body statement reaching here always writes.
	w, werr := db.attachedWriteSessionFor(ad.name, true)
	if werr != nil {
		ad.touchedInTxn = wasTouched
		return werr
	}
	ad.wroteData = true // see attachedDB.wroteData's own doc comment
	// The TEMP triggers THIS session bound to the attachment's own tables, so
	// w's ordinary LOCAL trigger machinery fires them for the write below as if
	// they were genuinely its own. This is routeAttachedStatement's identical
	// line (attach_write.go), for the identical reason, and it is recomputed on
	// EVERY firing rather than cached on w for the reason stated there:
	// trigger1.test 10.9 DROPs and re-CREATEs insert_log between two writes
	// into the SAME already-open w, and the very next write must see it.
	//
	// Without it a routed body's write stored its row with the trigger silently
	// unfired -- tableHasTriggers/matchingTriggers consult w.foreignTempTriggers
	// and nothing else could have told w these triggers exist.
	w.SetForeignTempTriggers(db.attachedMirrorSafeTriggers(ad.name))
	n, rerr := db.fireRoutedTriggerBody(rb, w, m)
	// Read-your-writes: a later body step, or the firing statement, reading
	// the attachment through this session must see what was just written
	// (as execRoutedToAttached does). On every outcome, not just success: an
	// ON CONFLICT FAIL halt keeps the rows it already applied (only OE_Abort
	// rolls the statement back, vdbeaux.c:3448), so skipping this left the
	// connection reading a snapshot from before them.
	ferr := db.refreshAttachedWriteReaders()
	if rerr != nil {
		return rerr
	}
	if ferr != nil {
		return ferr
	}
	// The routed rows count as this body statement's own changes(), published
	// by runOneTrigger's per-step setChanges when this sub-program returns
	// (Program.CountsChanges, set by compileRoutedTriggerBody).
	m.wctx.rowsAffected += n
	return nil
}

// routedTriggerBodyMirrorable reports an errVDBEUnsupported decline when a
// TEMP trigger this session bound to attachName's copy of table cannot be
// fired by mirroring it onto attachName's delegated session -- the only way
// the routed write can fire it. Writing with the trigger unfired is not an
// option. The shape is
//
//	CREATE TEMP TRIGGER onLogged AFTER INSERT ON aux.logged
//	  BEGIN INSERT INTO audit VALUES(new.v); END;   -- audit is in MAIN
//
// where "audit" resolves to this session's main, which w cannot reach
// (attachedTriggerMirrorSafe). routeAttachedStatement handles the top-level
// case by firing on the originating session afterwards
// (attachedOriginatingFireInsert), but that is INSERT-only and keyed by the
// assigned rowid, so here it stays a decline.
//
// routedBodyEvents is every trigger event bs can raise on its own target: its
// own verb, plus DELETE for an INSERT or an UPDATE, either of which can
// resolve a conflict by REPLACE and delete a row on the way
// (sqlite3GenerateConstraintChecks' OE_Replace arms, insert.c:2313/:2596).
// A trigger for any other event cannot fire, so it need not be mirrorable.
func routedBodyEvents(bs triggerBodyStmt) []triggerEventKind {
	switch {
	case bs.insert != nil:
		return []triggerEventKind{triggerInsert, triggerDelete}
	case bs.update != nil:
		return []triggerEventKind{triggerUpdate, triggerDelete}
	case bs.delete != nil:
		return []triggerEventKind{triggerDelete}
	}
	return []triggerEventKind{triggerInsert, triggerUpdate, triggerDelete}
}

func (db *DB) routedTriggerBodyMirrorable(attachName, table string, events []triggerEventKind) error {
	for _, tr := range db.attachedTriggersFor(attachName, table) {
		if !slices.Contains(events, tr.event) {
			continue // this body cannot raise that event (routedBodyEvents)
		}
		if !db.attachedTriggerMirrorSafe(tr, attachName) {
			return fmt.Errorf("%w: TEMP trigger %s on %s.%s cannot be mirrored onto the session a routed trigger body writes through", errVDBEUnsupported, tr.name, attachName, table)
		}
	}
	return nil
}

// fireRoutedTriggerBody compiles the body statement against w and runs it, both
// inside the originating connection's read scope.
//
// wireOriginatingReaders installs that scope, needed by both halves. The
// compile binds every source name the body mentions, and a TEMP trigger
// resolves them through the connection's temp/main/attachments search
// (attach.c:495, 547; sqlite3FindTable, build.c:374-384), not w's catalog --
// otherwise "src" in main is "no such table", or worse binds a same-named
// table in the attachment. The run needs it too: liveLower re-lowers
// sub-programs per firing against w.SnapshotPager(), which carries
// w.attachedReaders.
//
// reads=true unconditionally: execRoutedToAttached's tokensContainSelect gate
// works on text, and a body statement is an AST. A hand-written AST walk for
// the same fact would be a second enumeration to keep complete, so the
// snapshot cost is paid instead.
//
// The unwire must happen before the caller's refreshAttachedWriteReaders,
// which publishes w's snapshot as the attachment's read pager: a snapshot
// still reaching into main would let a view stored in the attachment resolve
// its body there. Hence a function, not a defer.
func (db *DB) fireRoutedTriggerBody(rb *routedTriggerBody, w *DB, firing *vdbe) (int, error) {
	unwire, uerr := db.wireOriginatingReaders(w, true)
	if uerr != nil {
		return 0, uerr
	}
	defer unwire()
	prog, cerr := rb.programFor(w)
	if cerr != nil {
		return 0, cerr
	}
	return db.runRoutedTriggerBody(w, prog, firing)
}

// runRoutedTriggerBody runs prog as a statement on the attached session w,
// with the firing machine's OLD/NEW row bound for the body's OpParam reads.
//
// It is runWrite's body with three differences:
//
//   - changes()/total_changes() are not published here; each body step
//     publishes through the firing session at C's OP_ResetCount
//     (trigger.c:1157), so publishing on w would double-count.
//   - connection counters are carried onto w and back. C has one connection,
//     so last_insert_rowid() inside the body is the firing connection's, and an
//     INSERT the body routed sets it.
//   - w's statement journal is appended to the firing statement's rather than
//     dropped on success. In C the trigger runs inside the firing VM's
//     statement transaction (sqlite3VdbeHalt's eStatementOp), so a later
//     failure must undo the routed rows too. wc.rollback is reused whole for
//     that.
func (db *DB) runRoutedTriggerBody(w *DB, prog *Program, firing *vdbe) (rowsAffected int, err error) {
	if ferr := w.fkCheckTargets(prog); ferr != nil {
		return 0, ferr
	}
	savedConn := func() func() {
		ch, tot, li, totOpaque, liOpaque := db.ConnState()
		w.SetConnState(ch, tot, li, totOpaque, liOpaque)
		return func() { db.SetConnState(w.ConnState()) }
	}()
	defer savedConn()
	wc := &writeCtx{db: w, changeMark: w.changeMark()}
	m := &vdbe{
		regs:         make([]Value, prog.NReg),
		cursors:      make([]*vdbeCursor, prog.NCursors),
		recRegs:      make([][]Value, prog.NRecRegs),
		sorters:      make([]*vdbeSorter, prog.NSorters),
		distinctSets: make([]*vdbeDistinctSet, prog.NDistinct),
		subCache:     make([]subCacheEntry, prog.NSubCache),
		pager:        prog.WritePager,
		wctx:         wc,
		trigNew:      firing.trigNew,
		trigNewRowid: firing.trigNewRowid,
		trigOld:      firing.trigOld,
		trigOldRowid: firing.trigOldRowid,
	}
	if prog.NeedsStmtImage {
		img, ierr := w.SnapshotPager()
		if ierr != nil {
			return 0, ierr
		}
		m.stmtImage = img
	}
	// Foreign keys are enforced per STATEMENT and this is a statement: fkStmt
	// is nil until fkBeginStatement sets it, and fkRowMutated returns
	// immediately on a nil one (fk.go), so without this line every foreign
	// key inside the attachment silently stops being enforced -- an ON DELETE
	// CASCADE that fires for the identical DELETE run locally, and for a
	// top-level "DELETE FROM aux.par", did not fire from a routed body: the
	// parent row gone and the orphan kept, on disk. C SQLite has one
	// connection and one foreign_keys setting, and the cascade is coded into
	// the DELETE's own program (sqlite3FkActions, fkey.c) with no knowledge of what
	// caused the delete. Paired with the fkFinishStatement below exactly as
	// runWrite pairs them.
	w.fkBeginStatement()
	savedWC := w.activeWC
	w.activeWC = wc
	defer func() { w.activeWC = savedWC }()
	if aerr := wc.aincBegin(prog); aerr != nil {
		return 0, aerr
	}
	_, rerr := m.run(prog.Insns)
	if rerr != nil {
		return 0, wc.resolveHalt(rerr)
	}
	wc.aincEnd() // see runWrite
	if ferr := w.fkFinishStatement(); ferr != nil {
		wc.unwindFKViolation()
		return 0, ferr
	}
	// The undo must also re-publish the attachment's read pager:
	// refreshAttachedWriteReaders pointed ad.pager at a snapshot taken when
	// this write succeeded, and this session's reads of the attachment
	// answer only from ad.pager. wc.rollback fixes w, but the snapshot would
	// keep serving the discarded rows (rollbackToSavepoint states the same
	// rule). e.g. "INSERT INTO main.t1 VALUES(1),(2),(1)" aborting on a
	// UNIQUE conflict must leave aux.logged as it was.
	//
	// The refresh is inside the closure because there is no post-unwind
	// hook: closures run in reverse, so the earliest routed body's runs last
	// and publishes the fully unwound session.
	firing.wctx.undoFn(func() {
		wc.rollback()
		// A SnapshotPager failure here cannot be reported -- a journal closure
		// undoes, it does not fail -- and leaves ad.pager exactly as stale as
		// it was before this line existed, which is the behaviour being fixed
		// rather than a new one.
		db.refreshAttachedWriteReaders()
	})
	return wc.rowsAffected, nil
}
