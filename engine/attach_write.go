// Writes into an ATTACHed database: CREATE/DROP of every object kind and
// INSERT/UPDATE/DELETE whose target names an attached database.
//
// A write session models one logical database materialized into one file
// (writer.go). Rather than teach it two files, a statement whose target names
// an attachment is delegated, minus the qualifier, to that database's own write
// session, an ordinary *DB. That session is opened lazily on first write; once
// it exists it is the truth for that database, and attachedDB.pager is
// refreshed from it after every write so cross-database reads see the writes.
//
// COMMIT (and Close) commits every open session, main last. C SQLite makes a
// multi-file transaction atomic with a master journal; this does not write one.
//
// ponytail: a crash between two files' commits leaves one committed and the
// other not. Each file stays consistent on its own, so this is a cross-file
// atomicity ceiling only for transactions writing more than one database. The
// upgrade path is a master journal: write the member list, fsync, commit each
// member, delete it, and consult it on recovery.
package engine

import (
	"fmt"
	"strings"
)

// attachedWriteSession returns the write session for the attached database
// named q, opening it on first use. Returns nil if q does not name an
// attachment. Equivalent to attachedWriteSessionFor(q, false) -- see that
// function's doc comment for what the forWrite parameter it lacks would add.
func (db *DB) attachedWriteSession(q string) (*DB, error) {
	return db.attachedWriteSessionFor(q, false)
}

// attachedWriteSessionFor is attachedWriteSession's parameterized form.
// forWrite marks a call about to run a real write through the session (a
// routed statement, "VACUUM <attached>", a TEMP trigger body via
// opTriggerBodyRouted) as opposed to one that only needs the session to exist
// (execAttach's lock seeding, execLockingMode's "locking_mode=normal" sweep).
//
// forWrite adds one check: a same-file sibling alias holding a persistent
// SHARED lock from exclusive-locking-mode seeding (holdLockingModeLock) but
// never written. C's autocommit commit (vdbeCommit, vdbeaux.c:2948-2973)
// requests EXCLUSIVE on every Btree the statement wrote
// (sqlite3PagerExclusiveLock), and unixInodeInfo (os_unix.c:1228-1237) has one
// lock per inode, so that request conflicts with the sibling's SHARED lock:
// "database is locked" (main.c:1667), and the row never lands. The check must
// be gated on forWrite, or a second ATTACH of the same file under exclusive
// mode would refuse itself.
func (db *DB) attachedWriteSessionFor(q string, forWrite bool) (*DB, error) {
	ad := db.attachedNamed(q)
	if ad == nil {
		return nil, nil
	}
	// Writing a file attached under more than one name, when another alias has
	// already written, is declined. C allows the aliasing and writing through any
	// single alias, but once one handle holds RESERVED or higher a second handle's
	// write conflicts at the OS lock level (one unixInodeInfo per inode, even for
	// separate Btrees): attach.test 9.2's second INSERT fails "database is locked".
	//
	// other.wroteData is the gate, not other.wdb != nil: a purely-read routed
	// statement ("PRAGMA t2.integrity_check") opens wdb without taking a write
	// lock. A handle re-entering its own lock never conflicts. wroteData is
	// sticky for the attachment's life, which can only over-decline.
	// Inside an explicit transaction a write takes RESERVED and escalates to
	// EXCLUSIVE at COMMIT; in autocommit the statement's commit is the
	// escalation. That decides the rules in the loop below.
	inTxn := db.inTransaction() || len(db.savepoints) > 0
	for _, other := range db.attached {
		if other == ad || !samePath(other.path, ad.path) {
			continue
		}
		// RESERVED against RESERVED: a second handle's write while a sibling
		// already holds the write lock in THIS transaction. Both lock
		// implementations refuse it -- os_unix.c's RESERVED byte, and memdb.c's
		// "if( p->nWrLock>0 ) rc = SQLITE_BUSY" (memdb.c:393-399) -- and SQLite
		// reports it as "database is locked". Checked ahead of the wroteData
		// decline below, which is the conservative answer for a sibling whose
		// write lock this engine cannot see being released; within one
		// transaction it CAN see it, and it is still held.
		if forWrite && inTxn && other.reservedInTxn {
			return nil, fmt.Errorf("%w: writing into ATTACHed database %s, whose file is also attached as %s, which already holds this transaction's write lock (C SQLite: a second handle's RESERVED request on one file conflicts with the first's)", ErrBusy, q, other.name)
		}
		if other.wroteData {
			return nil, fmt.Errorf("%w: writing into ATTACHed database %s, whose file is also attached as %s, which this session has already written into (C SQLite applies lock semantics across two handles on one file once either has written; reading through the alias, and a write through the ALREADY-written alias itself, are unaffected)", errVDBEUnsupported, q, other.name)
		}
		// forWrite only: other never wrote but may hold a persistent SHARED
		// lock from exclusive-locking-mode seeding (lockingHeld). This write's
		// EXCLUSIVE request at its own commit conflicts with it on the same
		// inode (see the doc comment). It is ErrBusy, not a decline: an exact
		// SQLITE_BUSY.
		// Only in autocommit: inside a transaction the write takes RESERVED,
		// which SHARED does not block, and the conflict moves to COMMIT
		// (attachedCommitLockCheck). Two aliases under locking_mode=exclusive:
		//
		//	INSERT INTO aux1.t VALUES(1)          -> database is locked
		//	BEGIN; INSERT INTO aux1.t VALUES(1)   -> OK
		//	COMMIT                                -> database is locked
		if forWrite && !inTxn && other.wdb != nil && other.wdb.lockingHeld {
			return nil, fmt.Errorf("%w: writing into ATTACHed database %s, whose file is also attached as %s, which holds a persistent SHARED lock under PRAGMA locking_mode=exclusive -- a genuine write's own EXCLUSIVE lock request conflicts with it on the same file, even though %s never itself wrote anything (C SQLite: vdbeCommit escalates the WRITING statement's own Btree to EXCLUSIVE at its autocommit commit boundary, and one inode has one lock regardless of which Pager object asks)", ErrBusy, q, other.name, other.name)
		}
	}
	// Admitted: inside a transaction this write now holds RESERVED on the
	// file until the transaction ends (see attachedDB.reservedInTxn).
	if forWrite && inTxn {
		ad.reservedInTxn = true
	}
	if ad.wdb != nil {
		return ad.wdb, nil
	}
	w, sess, err := openAttachedWrite(ad.path)
	if err != nil {
		return nil, fmt.Errorf("engine: cannot write into ATTACHed database %s: %w", q, err)
	}
	// Opened lazily, this session may start with savepoints already held
	// on the originating one. Replay the stack onto it at its untouched
	// start so a later ROLLBACK TO discards its writes, as in C. Replayed
	// onto the row store: savepoints are in-memory until commit.
	for _, sp := range db.savepoints {
		if serr := w.openSavepoint(sp.name); serr != nil {
			sess.Discard()
			return nil, serr
		}
	}
	// The delegated statement still carries no qualifier (routeAttachedStatement
	// strips it), but a statement that names the attachment in a SUBQUERY's FROM
	// -- "INSERT INTO aux.t SELECT * FROM aux.s" -- keeps it, so the session must
	// answer to that name. This mirrors what driver's own routing does.
	w.SetLocalSchema(ad.name)
	w.SetForeignKeys(db.fkEnforce)
	// PRAGMA legacy_alter_table is per CONNECTION too, so "ALTER TABLE
	// aux.p1 RENAME TO ppp" -- delegated here with its qualifier stripped --
	// gets the LEGACY cascade renameTableTo reads LegacyAlterTable() for,
	// exactly like an unqualified rename in main would. Verified directly
	// against 3.53.3 (alterlegacy.test 8.x): with legacy_alter_table=1 and
	// foreign_keys=on, ATTACHed aux.c1's "REFERENCES p1(a)" is rewritten when
	// aux.p1 is renamed -- both flags have to reach this session or the
	// rewrite silently reverts to the non-legacy shape.
	w.SetLegacyAlterTable(db.LegacyAlterTable())
	// PRAGMA trusted_schema is per CONNECTION, so a view in the ATTACHed
	// database is checked under the same flag main's is (trusted_schema.go).
	w.SetTrustedSchema(db.TrustedSchema())
	// recursive_triggers is per connection: C reads db->flags when coding
	// OP_Program (trigger.c:1412), whichever database the write lands in.
	// Without this a mirrored TEMP trigger stopped chaining after one level.
	w.SetRecursiveTriggers(db.RecursiveTriggers())
	w.textEncoding = db.encoding()
	// Only the opener knows the database is memory-backed (':memory:',
	// vfs=memdb and cache=shared attachments are temp files underneath,
	// memdb_registry.go). C answers "memory" for such an attachment's
	// journal_mode and ignores every mode a set asks for, so a routed
	// PRAGMA must see it.
	w.SetInMemory(ad.isMem || ad.isMemdb || ad.isSharedMem)
	// An unqualified "PRAGMA journal_mode = <mode>" sets every database
	// attached at that moment; a qualified one moves only its database; a
	// later ATTACH starts at "delete". This session is opened lazily, so
	// the mode comes off the binding (attachedDB.journalMode).
	w.SetJournalMode(ad.journalMode)
	ad.wdb = w
	ad.wsess = sess
	return w, nil
}

// openAttachedWrite opens a write session on one ATTACHed database's file and
// returns the row store plus the segment session that owns it. Like
// openAttachedRead, it accepts only our format via OpenOrCreate. It does not
// sniff the format first: a new attachment is a zero-byte file
// (ensureDatabaseFile), which OpenOrCreate reads as an empty database.
func openAttachedWrite(path string) (*DB, *Session, error) {
	sess, err := OpenOrCreate(path)
	if err != nil {
		return nil, nil, err
	}
	return sess.DB, sess, nil
}

// closeWrite COMMITS this attachment's write session and clears it, through
// whichever door its format uses -- Session.Commit appends a delta batch, a
// SQLite-format DB.Close writes pages. A no-op when no session is open.
//
// Every caller used to inline "w := ad.wdb; ad.wdb = nil; w.Close()", which is
// why the format branch is here instead of at five call sites.
func (ad *attachedDB) closeWrite() error {
	if ad.wdb == nil {
		return nil
	}
	sess := ad.wsess
	ad.wdb, ad.wsess = nil, nil
	return sess.Close()
}

// discardWrite throws this attachment's write session away WITHOUT committing.
// A no-op when none is open.
func (ad *attachedDB) discardWrite() {
	if ad.wdb == nil {
		return
	}
	sess := ad.wsess
	ad.wdb, ad.wsess = nil, nil
	sess.Discard()
}

// routeAttachedStatement reports whether trimmed targets an ATTACHed database
// and, if so, returns that database's write session and the statement with its
// target qualifier removed.
//
// Only the target is rewritten. Other qualifiers stay: the delegated session
// answers to the attachment's name and carries the originating databases as
// read snapshots in search order (wireOriginatingReaders).
//
// A CREATE TRIGGER's ON clause is the exception: C requires a non-TEMP
// trigger's table to be in the trigger's own database, so a qualifier equal to
// q is redundant and is spliced out (stripAttachedCreateTriggerOnQualifier); a
// different one is left for CreateTrigger to reject ("trigger t cannot
// reference objects in database aux").
func (db *DB) routeAttachedStatement(trimmed string, toks []token) (w *DB, q, rewritten string, ok bool, originating []*triggerMeta, schemaCascade *attachedSchemaCascade, err error) {
	q, rewritten, ok = attachedTargetQualifier(trimmed, toks)
	if !ok {
		// No qualifier written: an unqualified target still resolves ACROSS
		// databases -- see unqualifiedAttachedTarget. The statement is routed
		// unchanged, since there is no qualifier to splice out.
		q, ok = db.unqualifiedAttachedTarget(trimmed, toks)
		if !ok {
			return nil, "", "", false, nil, nil, nil
		}
		rewritten = trimmed
	}
	if db.attachedNamed(q) == nil {
		return nil, "", "", false, nil, nil, nil
	}
	// Confirmed a genuine route to q, not merely a qualifier that happens to
	// resolve locally (a "main."/"temp." trigger repeating its own qualifier
	// on the ON-clause too, which is ordinary and not this statement's shape).
	if isCreateTriggerStmt(toks) {
		rewritten = stripAttachedCreateTriggerOnQualifier(rewritten, q)
	}
	// A TEMP trigger this session created on q's copy of the target
	// (triggerMeta.tableAttachName) needs firing (INSERT/UPDATE/DELETE/
	// REPLACE) or a cross-session cascade (DROP TABLE/VIEW,
	// build.c:3387-3411; ALTER TABLE RENAME). The delegated session has its
	// own unrelated db.triggers, so plain delegation would drop the effect.
	//
	// DROP TABLE/VIEW and RENAME TO cascades are applied to db after
	// delegation (schemaCascade). Other schema changes stay declined.
	// Firing has two safe cases:
	//
	//   - attachedTriggerMirrorSafe: the body resolves inside q
	//     (trigger1.test 10.10); q's session fires it with its own machinery
	//     via SetForeignTempTriggers.
	//   - attachedTriggerOriginatingSafe: the body resolves to this session's
	//     catalog (trigger1.test 10.3-10.8); this session fires it after the
	//     delegated write, from the row it stored
	//     (attachedOriginatingFireInsert).
	//
	// Anything else (BEFORE/INSTEAD OF, an unenumerable body, a body table
	// in another attachment) stays declined.
	if name, nameOK := attachedRoutedTargetName(rewritten); nameOK && db.tableHasAnyAttachedTrigger(q, name) {
		routable := true
		var originatingCandidates []*triggerMeta
		switch {
		case attachedStatementIsDML(toks):
			for _, tr := range db.attachedTriggersFor(q, name) {
				switch {
				case db.attachedTriggerMirrorSafe(tr, q):
					// Handled below via SetForeignTempTriggers, unchanged.
				case db.attachedTriggerOriginatingSafe(tr):
					originatingCandidates = append(originatingCandidates, tr)
				default:
					routable = false
				}
			}
		case isAttachedDropTargetStmt(toks):
			// DROP TABLE/VIEW cascades into every matching trigger
			// unconditionally -- build.c:3387-3413's sqlite3CodeDropTable
			// loop (sqlite3TriggerList/sqlite3DropTriggerPtr,
			// trigger.c:50-94/709-741) matches by Schema-POINTER identity
			// (agnostic of main/temp/attached) and deletes the trigger's own
			// schema row with no rewrite and no failure mode -- see
			// removeAttachedTargetTriggers, applied post-delegation.
			schemaCascade = &attachedSchemaCascade{kind: attachedSchemaCascadeDrop, triggers: db.attachedTriggersFor(q, name)}
		default:
			if newName, renameOK := isAttachedRenameTableStmt(toks); renameOK {
				trs := db.attachedTriggersFor(q, name)
				if db.attachedTriggerRenameSafe(trs, name) {
					schemaCascade = &attachedSchemaCascade{kind: attachedSchemaCascadeRename, triggers: trs, oldName: name, newName: newName}
				} else {
					routable = false
				}
			} else {
				routable = false // CREATE, DROP TRIGGER/INDEX, RENAME COLUMN, ADD/DROP COLUMN, ... -- no cascade built
			}
		}
		if routable && len(originatingCandidates) > 0 {
			// A candidate for an event this statement does not raise never fires.
			// Matching ones need the row to be capturable: for INSERT only a plain
			// single-row INSERT (attachedOriginatingInsertCapturable); UPDATE and
			// DELETE are covered by w's change capture (OLD and NEW).
			event, eventOK := attachedStatementEventKind(toks)
			var toFire []*triggerMeta
			for _, tr := range originatingCandidates {
				if eventOK && tr.event == event {
					toFire = append(toFire, tr)
				}
			}
			if len(toFire) > 0 {
				// An INSERT is fired from the row w stored (it has no OLD
				// image to keep); an UPDATE or a DELETE from the OLD/NEW pairs
				// w's own change capture recorded, in the order it applied
				// them (attachedOriginatingFireRows).
				if event == triggerInsert && !attachedOriginatingInsertCapturable(rewritten) {
					routable = false
				} else {
					originating = toFire
				}
			}
		}
		if !routable {
			return nil, q, "", true, nil, nil, fmt.Errorf("%w: this statement targets %s.%s, which a TEMP trigger created in this session is bound to -- firing it, or cascading a DROP/ALTER through it, is not implemented by this write path", errVDBEUnsupported, q, name)
		}
	}
	// A transaction spans every member database in C:
	//
	//	BEGIN; INSERT INTO main.t4 ...; INSERT INTO aux.t4 ...; ROLLBACK
	//	  both tables empty afterwards
	//
	// which enterTxnAttachedWrite reproduces, since an attached session only
	// touches its file at Close. Savepoints are mirrored onto every open
	// attached session (txn.go), and replayed onto one opened later.
	// A refused write must leave this alias's lock state as it found it:
	// btreeBeginTrans takes SHARED, asks for RESERVED, and on failure drops
	// the SHARED again unless already in a transaction (unlockBtreeIfUnused,
	// btree.c:3729-3731). So "BEGIN; INSERT INTO a1.t ...; INSERT INTO a2.t
	// ..." (locked) followed by COMMIT commits.
	wasTouched := false
	if ad := db.attachedNamed(q); ad != nil {
		wasTouched = ad.touchedInTxn
	}
	if db.inTransaction() {
		if terr := db.enterTxnAttachedWrite(q); terr != nil {
			return nil, q, "", true, nil, nil, terr
		}
	}
	// forWrite is per statement: a routed PRAGMA may be a pure read
	// (attachedRoutedStatementWrites). C's conflict is the EXCLUSIVE
	// escalation at the writer's commit (vdbeaux.c:2948-2973); two SHARED
	// holders on one inode coexist (os_unix.c:2030 only conflicts on an
	// EXCLUSIVE request). So with a1 attached but unwritten, "ATTACH ... AS
	// a2; PRAGMA a2.integrity_check" answers "ok".
	w, werr := db.attachedWriteSessionFor(q, attachedRoutedStatementWrites(toks, rewritten))
	if werr != nil {
		if ad := db.attachedNamed(q); ad != nil {
			ad.touchedInTxn = wasTouched
		}
		return nil, q, "", true, nil, nil, werr
	}
	// See trigger.go's attachedTriggerMirrorSafe/SetForeignTempTriggers: w
	// fires these using its OWN local trigger machinery, unmodified, as if
	// they were genuinely its own. Recomputed fresh on EVERY route, never
	// cached on w -- unlike attachedWriteSession's other Set* calls above,
	// which only run once at open -- because a CREATE/DROP TRIGGER between
	// two routed writes must be seen by the very next one (trigger1.test
	// 10.9's DROP+CREATE of insert_log is exactly a schema change of this
	// kind, between two writes into the SAME already-open w).
	w.SetForeignTempTriggers(db.attachedMirrorSafeTriggers(q))
	return w, q, rewritten, true, originating, schemaCascade, nil
}

// attachedStatementEventKind maps toks' leading verb -- already known to be
// one of attachedStatementIsDML's four, or the caller never reaches here --
// to the triggerEventKind an AFTER trigger fires on for it. REPLACE is
// INSERT-shaped for trigger purposes exactly like a plain "INSERT OR
// REPLACE": both fire triggerInsert, and trigger.go has no separate
// "REPLACE event" anywhere.
func attachedStatementEventKind(toks []token) (triggerEventKind, bool) {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return 0, false
	}
	switch toks[0].upper() {
	case "INSERT", "REPLACE":
		return triggerInsert, true
	case "UPDATE":
		return triggerUpdate, true
	case "DELETE":
		return triggerDelete, true
	}
	return 0, false
}

// attachedOriginatingInsertCapturable reports whether routed inserts exactly
// one row whose stored values (after affinity, defaults and rowid assignment)
// attachedOriginatingFireInsert can read back from the session's table.
// Refused: a SELECT source, more than one VALUES row, an explicit conflict
// action (which changes whether or how the row lands), and an UPSERT.
func attachedOriginatingInsertCapturable(routed string) bool {
	stmt, err := parseInsertStmt(routed)
	if err != nil {
		return false
	}
	return stmt.selectStmt == nil && len(stmt.rows) == 1 && !stmt.explicitOr && stmt.upsert == nil
}

// attachedOriginatingFireInsert fires trs (TEMP triggers on an attached table,
// classified attachedTriggerOriginatingSafe) against this session's catalog,
// using the row w's single-row INSERT stored at rowid li. Reading the stored
// row means defaults, an auto-assigned IPK and affinity appear in NEW.* as for
// a local trigger.
//
// A WITHOUT ROWID table has no real rowid, so it is refused here. That is the
// one failure possible after the delegated write; the caller wraps the
// sequence in paired beginStatementSnapshot calls on db and w so it unwinds
// both.
func (db *DB) attachedOriginatingFireInsert(trs []*triggerMeta, w *DB, name string, li int64) error {
	tbl := w.findTableMeta(name)
	if tbl == nil || tbl.withoutRowid {
		return fmt.Errorf("engine: internal: cannot locate the row a delegated ATTACHed INSERT into %s just wrote to fire a TEMP trigger against it", name)
	}
	stored, ok := tbl.rows.get(uint64(li))
	if !ok {
		return fmt.Errorf("engine: internal: cannot locate the row a delegated ATTACHed INSERT into %s just wrote to fire a TEMP trigger against it", name)
	}
	// Normalized, not raw: the stored row has NULL for an INTEGER PRIMARY
	// KEY (OP_Column substitutes the rowid, vdbe.c:3345-3350), integers for
	// REAL columns, no values for columns added later or generated ones.
	// normalizeRow undoes all four, as every OLD/NEW image builder does. It
	// also copies, so a body writing this table cannot move NEW.* under the
	// firing.
	full := normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, uint64(li), stored)
	// A compiled fire driven from outside a running program.
	// firePlanForRow takes the OLD/NEW images directly and runTriggerSub
	// builds each body's sub-VM, so the caller only supplies the write
	// context. The plan is compiled against w's table (what NEW.* binds to)
	// while the triggers belong to this session.
	plan, perr := db.compileTriggerFirePlanFor(tbl, triggerInsert, newTriggerPrgMemo(), conflictAbort, false, trs)
	if perr != nil {
		return perr
	}
	if plan == nil {
		return nil
	}
	// This session has NO active write of its own: execRoutedToAttached
	// delegated the statement to w, so w is the one executing a program. The
	// fire therefore needs a write context established here, and it needs the
	// same statement-journal discipline runWrite gives every other write --
	// a trigger body that fails partway must not leave its own rows applied.
	// TestAttachedOriginatingTriggerFailureUndoesBothSessions is exactly that
	// case, and it is what caught the first version of this, which borrowed
	// db.activeWC and found it nil.
	wc := &writeCtx{db: db, changeMark: db.changeMark()}
	savedWC := db.activeWC
	db.activeWC = wc
	defer func() { db.activeWC = savedWC }()
	if err := (&vdbe{wctx: wc}).firePlanForRow(plan, full, li, nil, 0); err != nil {
		wc.rollback()
		return err
	}
	wc.aincEnd() // see runWrite
	return nil
}

// attachedRowCapture turns w's change capture on for one delegated statement.
// The returned function reports the rows it recorded and puts the flag (and,
// when this turned it on, the log) back as it found them -- a session the
// replication layer is already capturing on keeps every entry.
func attachedRowCapture(w *DB) func() []RowChange {
	// FULL capture for the span: attachedOriginatingFireRows fires triggers
	// against the OLD/NEW images recorded here, and the light form records no OLD.
	saved, savedFull, mark := w.captureChanges, w.captureFull, len(w.changeLog)
	w.captureChanges, w.captureFull = true, true
	return func() []RowChange {
		rows := append([]RowChange(nil), w.changeLog[mark:]...)
		if !saved {
			w.changeLog = w.changeLog[:mark]
			w.captureChanges = false
		}
		w.captureFull = savedFull
		return rows
	}
}

// attachedOriginatingFireRows is attachedOriginatingFireInsert for UPDATE or
// DELETE: it fires trs for every row w's write touched, in order, with the
// OLD/NEW images its change capture recorded (rowhook.go). C fires each row's
// AFTER trigger inside the statement loop, this fires them after, which is
// indistinguishable here: every table the bodies reach is this session's own,
// so they cannot observe the delegated statement nor be observed by it.
func (db *DB) attachedOriginatingFireRows(trs []*triggerMeta, w *DB, name string, event triggerEventKind, changes []RowChange) error {
	tbl := w.findTableMeta(name)
	if tbl == nil {
		return fmt.Errorf("engine: internal: cannot locate %s, the table a delegated ATTACHed write this session must fire a TEMP trigger against touched", name)
	}
	plan, perr := db.compileTriggerFirePlanFor(tbl, event, newTriggerPrgMemo(), conflictAbort, false, trs)
	if perr != nil {
		return perr
	}
	if plan == nil {
		return nil
	}
	// See attachedOriginatingFireInsert: this session runs no program of its
	// own here, so the fire needs a write context and the statement-journal
	// discipline that comes with it.
	wc := &writeCtx{db: db, changeMark: db.changeMark()}
	savedWC := db.activeWC
	db.activeWC = wc
	defer func() { db.activeWC = savedWC }()
	for _, ch := range changes {
		if !equalFoldName(ch.Table, name) {
			continue
		}
		var old, new []Value
		var oldRowid, newRowid int64
		switch event {
		case triggerUpdate:
			if ch.Kind != RowUpdate {
				continue
			}
			old, new = ch.Old, ch.New
			oldRowid, newRowid = ch.Rowid, ch.Rowid
		case triggerDelete:
			if ch.Kind != RowDelete {
				continue
			}
			old, oldRowid = ch.Old, ch.Rowid
		default:
			continue
		}
		if err := (&vdbe{wctx: wc}).firePlanForRow(plan, new, newRowid, old, oldRowid); err != nil {
			wc.rollback()
			return err
		}
	}
	wc.aincEnd() // see runWrite
	return nil
}

// attachedStatementIsDML reports whether toks is INSERT/REPLACE/UPDATE/DELETE,
// the only kinds trigger mirroring applies to. DROP TABLE/VIEW and RENAME TO
// are cascades, classified separately; other DDL stays declined.
func attachedStatementIsDML(toks []token) bool {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return false
	}
	switch toks[0].upper() {
	case "INSERT", "REPLACE", "UPDATE", "DELETE":
		return true
	}
	return false
}

// isAttachedDropTargetStmt reports whether toks is a "DROP TABLE" or "DROP
// VIEW" statement -- the two DDL shapes whose real-SQLite drop path
// (sqlite3CodeDropTable, build.c:3387-3413, for a table; the identical
// trigger sweep this engine's own view.go's removeView mirrors, for a view)
// cascades into the dropped object's own triggers. "DROP INDEX"/"DROP
// TRIGGER" never do, and fall through to routeAttachedStatement's default
// decline.
func isAttachedDropTargetStmt(toks []token) bool {
	if len(toks) < 2 || toks[0].kind != tkIdent || toks[0].upper() != "DROP" || toks[1].kind != tkIdent {
		return false
	}
	switch toks[1].upper() {
	case "TABLE", "VIEW":
		return true
	}
	return false
}

// isAttachedRenameTableStmt reports whether toks is "ALTER TABLE
// [schema.]name RENAME TO newName" (not a column rename, ADD or DROP), and
// returns newName. RENAME COLUMN's cascade (sqlite3AlterRenameColumn,
// alter.c:645/676) is not handled and falls through to the decline.
func isAttachedRenameTableStmt(toks []token) (newName string, ok bool) {
	if len(toks) < 2 || toks[0].kind != tkIdent || toks[0].upper() != "ALTER" ||
		toks[1].kind != tkIdent || toks[1].upper() != "TABLE" {
		return "", false
	}
	i := 2
	if i >= len(toks) || !isNameToken(toks[i]) {
		return "", false
	}
	i++
	// An optional "schema." qualifier on the target -- present in the RAW
	// toks this is called with (routeAttachedStatement passes the ORIGINAL,
	// not-yet-qualifier-stripped stream, same as attachedStatementIsDML).
	if i+1 < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." && isNameToken(toks[i+1]) {
		i += 2
	}
	if i >= len(toks) || toks[i].kind != tkIdent || toks[i].upper() != "RENAME" {
		return "", false
	}
	i++
	if i >= len(toks) || toks[i].kind != tkIdent || toks[i].upper() != "TO" {
		return "", false // "RENAME COLUMN ..."/"RENAME <col> TO ..." -- not this shape
	}
	i++
	if i >= len(toks) || !isNameToken(toks[i]) {
		return "", false
	}
	switch toks[i].kind {
	case tkIdent:
		return toks[i].text, true
	case tkString:
		return toks[i].str, true
	}
	return "", false
}

// attachedSchemaCascadeKind distinguishes the two schema cascades
// routeAttachedStatement can hand to execRoutedToAttached -- see
// attachedSchemaCascade.
type attachedSchemaCascadeKind int

const (
	attachedSchemaCascadeDrop attachedSchemaCascadeKind = iota
	attachedSchemaCascadeRename
)

// attachedSchemaCascade is the local side effect a routed DROP TABLE/VIEW or
// RENAME TO applies after its delegated half succeeds (execRoutedToAttached).
// triggers is db.attachedTriggersFor(q, name), computed once before
// delegation; no trigger can be created or dropped in between.
type attachedSchemaCascade struct {
	kind     attachedSchemaCascadeKind
	triggers []*triggerMeta
	// oldName/newName are cascadeRename-only: the RENAME's own target names,
	// already known safe (attachedTriggerRenameSafe) at routing time.
	oldName, newName string
}

// removeAttachedTargetTriggers drops trs from db.triggers, the local half of a
// routed DROP TABLE/VIEW. removeTableAndIndexes and removeView skip triggers
// with a tableAttachName, so this takes an explicit set. trs is always TEMP
// (only a TEMP trigger can target an attachment), matching C's cascade, which
// deletes from the trigger's own schema (build.c:3387-3413,
// trigger.c:709-741).
func (db *DB) removeAttachedTargetTriggers(trs []*triggerMeta) {
	if len(trs) == 0 {
		return
	}
	kept := db.triggers[:0]
	for _, tr := range db.triggers {
		drop := false
		for _, d := range trs {
			if tr == d {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, tr)
		}
	}
	db.triggers = kept
	db.bumpSchema(true)
}

// renameAttachedTargetTriggerOnClause rewrites only the ON clause's table-name
// token of each trigger in trs (via locateTriggerOnTableToken, which skips a
// "schema." qualifier, so "ON aux.t1" becomes "ON aux.t2"). C rewrites this
// position unconditionally, matched by schema identity (alter.c:1854-1857), so
// like renameTriggerOnClauseLegacy it cannot fail.
func renameAttachedTargetTriggerOnClause(trs []*triggerMeta, newName string) {
	for _, tr := range trs {
		onTok, terr := locateTriggerOnTableToken(tr.sql)
		if terr != nil {
			continue // every stored trigger re-lexes; unreachable in practice
		}
		tr.sql = applyEdits(tr.sql, []textEdit{{onTok.Start, onTok.End, quoteIdent(newName)}})
		tr.table = newName
	}
}

// attachedTriggerRenameSafe reports whether a routed "ALTER TABLE oldName
// RENAME TO newName" on an attached table may cascade into trs. Checked before
// delegation, so "no" declines the whole statement.
//
// Always safe under legacy_alter_table, which skips the WHEN/body walk
// (alter.c:1859). Otherwise C rewrites body mentions by resolved identity
// (alter.c:1859-1873); this cascade is textual, so it requires oldName to
// appear exactly once in the stored SQL (the ON clause). A second mention could
// be a shadowing table, another attachment's table or a column, which text
// cannot tell apart.
func (db *DB) attachedTriggerRenameSafe(trs []*triggerMeta, oldName string) bool {
	if db.LegacyAlterTable() {
		return true
	}
	for _, tr := range trs {
		if countIdentMentions(tr.sql, oldName) != 1 {
			return false
		}
	}
	return true
}

// applyAttachedTriggerRenameCascade performs the rename cascade
// attachedTriggerRenameSafe cleared, after the delegated rename. It cannot
// fail: the single-mention requirement excludes what applyTriggerRename could
// reject.
func (db *DB) applyAttachedTriggerRenameCascade(trs []*triggerMeta, oldName, newName string) error {
	if len(trs) == 0 {
		return nil
	}
	if db.LegacyAlterTable() {
		renameAttachedTargetTriggerOnClause(trs, newName)
		return nil
	}
	return applyTriggerRename(trs, oldName, identRepl{name: newName, force: true, isTable: true}, true, "ALTER TABLE RENAME TO", "", nil)
}

// attachedRoutedStatementWrites reports whether the statement delegated to an
// attachment's session mutated its file, as opposed to only being answered "as
// that database" (attachedDB.wroteData). Every routed non-PRAGMA statement
// writes. A routed PRAGMA (attachedPragmaScope's pragmaScopeRouted) is split:
//
//   - user_version/schema_version/application_id/max_page_count and
//     journal_mode/auto_vacuum/secure_delete/locking_mode: only the setter
//     writes.
//   - wal_checkpoint: always a write.
//   - wal_autocheckpoint: never (connection state).
//   - integrity_check/quick_check/foreign_key_check: never.
//
// pragma.test 3.9a-3.9c depend on this: "PRAGMA t2.integrity_check=..." must
// not mark t2 written, or a later ATTACH of the same file reports "already
// written into".
func attachedRoutedStatementWrites(toks []token, routed string) bool {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return false
	}
	if toks[0].upper() != "PRAGMA" {
		return true
	}
	stmt, err := ParsePragma(routed)
	if err != nil {
		// Unreachable in practice: routeAttachedStatement only routes a PRAGMA
		// here after attachedTargetTokenIndex's own "PRAGMA" case (pragma.go)
		// has already confirmed this exact text lexes as an ATTACHED-scoped
		// pragma, and w.ExecArgs (just above, in the caller) has already run
		// it successfully. Assume a write rather than silently under-mark.
		return true
	}
	switch stmt.Name {
	case "wal_checkpoint":
		return true
	case "user_version", "schema_version", "application_id", "max_page_count",
		"journal_mode", "auto_vacuum", "secure_delete", "locking_mode":
		return stmt.HasValue
	default: // wal_autocheckpoint, foreign_key_check, integrity_check, quick_check
		return false
	}
}

// attachedRoutedTargetName returns the bare object name a routed statement
// targets, re-lexing the already-spliced text and using
// attachedTargetTokenIndex's per-verb positions (skipping a leading WITH).
func attachedRoutedTargetName(routed string) (string, bool) {
	toks, err := lex(routed)
	if err != nil {
		return "", false
	}
	rest := withSkippedTokens(routed, toks)
	i, ok := attachedTargetTokenIndex(rest)
	if !ok || i >= len(rest) {
		return "", false
	}
	switch rest[i].kind {
	case tkIdent:
		return rest[i].text, true
	case tkString:
		return rest[i].str, true
	}
	return "", false
}

// enterTxnAttachedWrite prepares attachment q for a write inside the current
// transaction, so ROLLBACK can undo it.
//
// An attached session only touches its file at Close, so discarding it undoes
// everything it holds. But a session opened before the transaction holds
// autocommit writes, which C already committed (trigger1.test#7). So the first
// in-transaction write closes any existing session, flushing those, and
// attachedWriteSession reopens a fresh one that holds only transaction work.
// The attachment is recorded so rollback touches only sessions written since
// BEGIN.
func (db *DB) enterTxnAttachedWrite(q string) error {
	ad := db.attachedNamed(q)
	if ad == nil {
		return nil
	}
	for _, seen := range db.txnAttachedWrites {
		if seen == ad {
			return nil // already reopened for this transaction
		}
	}
	if err := ad.closeWrite(); err != nil {
		return fmt.Errorf("engine: flushing ATTACHed database %s's autocommit writes before a transaction writes into it: %w", ad.name, err)
	}
	db.txnAttachedWrites = append(db.txnAttachedWrites, ad)
	ad.touchedInTxn = true
	return nil
}

// rollbackTxnAttachedWrites discards every attached session this transaction
// opened (enterTxnAttachedWrite) and reopens each binding's read pager from
// the file, since it held a snapshot of the discarded session. Called from
// clearTxnState; a no-op after COMMIT, which already closed the sessions.
func (db *DB) rollbackTxnAttachedWrites() {
	for _, ad := range db.txnAttachedWrites {
		if ad.wdb == nil {
			continue // committed already (commitAttachedWrites)
		}
		ad.discardWrite()
		rp, err := openAttachedRead(ad.path)
		if err != nil {
			continue // leave the stale pager rather than a nil one; reads still error
		}
		rp.SetLocalSchema(ad.name)
		if ad.pager != nil {
			ad.pager.Close()
		}
		ad.pager = rp
	}
	if len(db.txnAttachedWrites) > 0 {
		db.refreshAttachedReaders()
	}
	db.txnAttachedWrites = nil
}

// unqualifiedAttachedTarget resolves a write whose target has no qualifier and
// reports the attachment that owns it. sqlite3FindTable searches TEMP, MAIN,
// then attachments in ATTACH order, so
//
//	ATTACH 'x.db' AS aux; CREATE TABLE aux.t2(a,b); INSERT INTO t2 VALUES(3,4)
//
// writes aux.t2, as the read side (resolveFrom, cross_db.go) already resolves.
//
// TEMP or MAIN always wins; nothing is routed unless both lack the name. Only
// targets that must already exist are resolved: an unqualified CREATE creates
// in main (or temp) even when an attachment has the name.
func (db *DB) unqualifiedAttachedTarget(trimmed string, toks []token) (string, bool) {
	rest := withSkippedTokens(trimmed, toks)
	kinds, ok := unqualifiedTargetKinds(rest)
	if !ok {
		return "", false
	}
	i, ok := attachedTargetTokenIndex(rest)
	if !ok || i >= len(rest) || rest[i].kind != tkIdent {
		return "", false
	}
	// A following "." means the name IS qualified -- the caller's own path.
	if i+1 < len(rest) && rest[i+1].kind == tkPunct && rest[i+1].text == "." {
		return "", false
	}
	if !rest[i].quoted && nonIdentifierKeywords[rest[i].upper()] {
		return "", false
	}
	return db.resolveUnqualifiedAcrossAttached(rest[i].text, kinds)
}

// resolveUnqualifiedAcrossAttached resolves a bare name of one of kinds: the
// local catalog first, then attachments in ATTACH order (unqualifiedAttachedTarget's
// precedence, shared with attachedTriggerMirrorSafe). Returns the winning
// attachment's name, or ok=false if none has it or the local catalog does.
func (db *DB) resolveUnqualifiedAcrossAttached(name string, kinds []string) (string, bool) {
	if db.hasLocalObject(name, kinds) {
		return "", false // TEMP and MAIN both outrank every attachment
	}
	for _, a := range db.attached {
		if a.wdb != nil {
			// Its own write session is the current state; the read pager still
			// shows the file as it was before those writes.
			if a.wdb.hasLocalObject(name, kinds) {
				return a.name, true
			}
			continue
		}
		if a.pager == nil {
			continue
		}
		rows, err := a.pager.Schema()
		if err != nil {
			continue
		}
		for _, r := range rows {
			if !equalFoldName(r.Name, name) {
				continue
			}
			for _, k := range kinds {
				if equalFoldName(r.Type, k) {
					return a.name, true
				}
			}
		}
	}
	return "", false
}

// unqualifiedTargetKinds is the sqlite_schema "type" an unqualified target must
// have for the statement to mean it. Keyed per statement because the name space
// is per type: "DROP TABLE t" must not follow an INDEX called t into an
// attachment.
func unqualifiedTargetKinds(toks []token) ([]string, bool) {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return nil, false
	}
	switch toks[0].upper() {
	case "INSERT", "REPLACE", "UPDATE", "DELETE":
		// A view is included because C SQLite resolves the name to it and
		// then reports "cannot modify <v> because it is a view". Routing so the
		// delegated session raises that is the same answer; declining is a gap.
		return []string{"table", "view"}, true
	case "ALTER":
		return []string{"table"}, true
	case "DROP":
		if len(toks) > 1 && toks[1].kind == tkIdent {
			switch toks[1].upper() {
			case "TABLE":
				return []string{"table"}, true
			case "VIEW":
				return []string{"view"}, true
			case "INDEX":
				return []string{"index"}, true
			case "TRIGGER":
				return []string{"trigger"}, true
			}
		}
	}
	return nil, false
}

// hasLocalObject reports whether this session's OWN catalogs (temp first, then
// main -- scopeAny) hold an object of one of kinds under name.
func (db *DB) hasLocalObject(name string, kinds []string) bool {
	for _, k := range kinds {
		switch k {
		case "table":
			if db.findTableMetaIn(scopeAny, name) != nil {
				return true
			}
			// A virtual table is tracked in db.vtabs, not db.tables, but its
			// schema row is type='table'. Without this, unqualified writes into
			// an attached virtual table never routed.
			if db.findVtabMetaIn(scopeAny, name) != nil {
				return true
			}
		case "view":
			if db.findViewMetaIn(scopeAny, name) != nil {
				return true
			}
		case "index":
			if db.findIndexMetaIn(scopeAny, name) != nil {
				return true
			}
		case "trigger":
			if db.findTriggerMetaIn(scopeAny, name) != nil {
				return true
			}
		}
	}
	return false
}

// attachedHasTable reports whether ad's CURRENT schema -- its own write
// session if one is already open, else its read pager -- holds a base table
// named name. Used by CreateTrigger's ON-clause resolution
// (trigger.go's resolveCreateTriggerAttachedTarget), mirroring
// unqualifiedAttachedTarget's identical dual wdb/pager check just above.
func (db *DB) attachedHasTable(ad *attachedDB, name string) bool {
	if ad.wdb != nil {
		return ad.wdb.findTableMetaIn(scopeAny, name) != nil
	}
	if ad.pager == nil {
		return false
	}
	rows, err := ad.pager.Schema()
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.Type == "table" && equalFoldName(r.Name, name) {
			return true
		}
	}
	return false
}

// firstAttachedTable searches db.attached, in attach order, for the first
// database with a base table named name -- the TEMP-trigger bare-unqualified
// ON-clause search (build.c:373-385's sqlite3FindTable, TEMP/MAIN/ATTACHED
// order, with TEMP and MAIN already ruled out by the caller since neither has
// the name). Returns nil if none does.
func (db *DB) firstAttachedTable(name string) *attachedDB {
	for _, ad := range db.attached {
		if db.attachedHasTable(ad, name) {
			return ad
		}
	}
	return nil
}

// attachedTargetQualifier finds a write statement's target database qualifier
// and returns the statement with "<qualifier>." spliced out. It works on the
// token stream (targets sit in a fixed position after the leading keywords, and
// tokens carry byte offsets). ok=false means no qualified target. A leading
// WITH is skipped first (withSkippedTokens); the returned tokens share offsets
// with trimmed.
func attachedTargetQualifier(trimmed string, toks []token) (qualifier, rewritten string, ok bool) {
	rest := withSkippedTokens(trimmed, toks)
	i, ok := attachedTargetTokenIndex(rest)
	if !ok || i+2 >= len(rest) {
		return "", "", false
	}
	name, dot := rest[i], rest[i+1]
	if dot.kind != tkPunct || dot.text != "." {
		return "", "", false
	}
	// The qualifier may be a string literal: ATTACH's name is an
	// expression, so 'aux', "aux" and [aux] name one database, and every
	// statement kind accepts 'aux'.<name>. This matters for a database
	// whose name is a reserved keyword (alter.test's 'ON').
	var qname string
	switch name.kind {
	case tkIdent:
		qname = name.text
	case tkString:
		qname = name.str
	default:
		return "", "", false
	}
	// An unquoted reserved keyword cannot be a qualifier, even when such
	// a database exists (alter.test):
	//
	//	ATTACH 'test3.db' AS 'ON';
	//	CREATE TABLE ON.t1(a,b,c)    -> near "ON": syntax error
	//	CREATE TABLE 'ON'.t1(a,b,c)  -> accepted
	//
	// This bypasses the parser, so apply its nonIdentifierKeywords gate.
	if name.kind == tkIdent && !name.quoted && nonIdentifierKeywords[name.upper()] {
		return "", "", false
	}
	// Same rule for the target name, and it matters: splicing out the
	// qualifier hands the delegated session a different statement, which
	// must not turn something C refuses into something it accepts:
	//
	//	CREATE TABLE aux.ON(a,b,c)    -> near "ON": syntax error
	//	CREATE TABLE aux."ON"(a)      -> accepted
	//	CREATE TABLE aux.KEY(a)       -> accepted (a keyword that IS an identifier)
	if target := rest[i+2]; target.kind == tkIdent && !target.quoted && nonIdentifierKeywords[target.upper()] {
		return "", "", false
	}
	// Splice out "<name>." by byte offset: the target token that follows starts
	// the surviving text.
	start, end := name.Start, rest[i+2].Start
	if start < 0 || end <= start || end > len(trimmed) {
		return "", "", false
	}
	return qname, trimmed[:start] + trimmed[end:], true
}

// withSkippedTokens returns toks with a leading "WITH [RECURSIVE] <cte-list>"
// skipped, so callers that locate a target by fixed per-verb position
// (attachedTargetTokenIndex, unqualifiedTargetKinds) see the verb. Only parsing
// the CTE list finds its end (verbAfterLeadingWith, sql_parser.go). The slice
// shares toks' tokens, so byte offsets stay valid. A missing or unparseable
// WITH returns toks unchanged; the real parser then reports the error.
func withSkippedTokens(trimmed string, toks []token) []token {
	if len(toks) == 0 || toks[0].kind != tkIdent || toks[0].upper() != "WITH" {
		return toks
	}
	p := newParser(trimmed, toks)
	if _, err := p.parseWithClause(); err != nil || p.pos <= 0 || p.pos >= len(toks) {
		return toks
	}
	return toks[p.pos:]
}

// attachedTargetTokenIndex returns the index of the token that would begin a
// write statement's target name, or ok=false for a statement kind that has no
// single write target (SELECT, the transaction verbs, ATTACH/DETACH). Callers
// that may see a leading WITH clause skip it first (withSkippedTokens) --
// this switch itself never recognizes "WITH", by design: a fixed per-verb
// position table has no meaningful entry for a clause of arbitrary length.
func attachedTargetTokenIndex(toks []token) (int, bool) {
	if len(toks) == 0 || toks[0].kind != tkIdent {
		return 0, false
	}
	skipIdent := func(i int, words ...string) int {
		if i < len(toks) && toks[i].kind == tkIdent {
			for _, w := range words {
				if toks[i].upper() == w {
					return i + 1
				}
			}
		}
		return i
	}
	switch toks[0].upper() {
	case "INSERT", "REPLACE":
		i := 1
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "OR" {
			i += 2 // OR <action>
		}
		return skipIdent(i, "INTO"), true
	case "UPDATE":
		i := 1
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "OR" {
			i += 2
		}
		return i, true
	case "DELETE":
		return skipIdent(1, "FROM"), true
	case "CREATE":
		i := 1
		// A TEMP object's name must be UNQUALIFIED -- C SQLite:
		// "CREATE TEMP TABLE two.abc(x,y)" is "temporary table name must be
		// unqualified" (temptable.test). Routing would strip the qualifier and
		// create it for real, so a TEMP create is never routed and falls through
		// to the parser that already reports that error.
		if i < len(toks) && toks[i].kind == tkIdent &&
			(toks[i].upper() == "TEMP" || toks[i].upper() == "TEMPORARY") {
			return 0, false
		}
		i = skipIdent(i, "UNIQUE")
		i = skipIdent(i, "VIRTUAL")
		i = skipIdent(i, "TABLE", "INDEX", "VIEW", "TRIGGER")
		i = skipIdent(i, "IF")
		i = skipIdent(i, "NOT")
		i = skipIdent(i, "EXISTS")
		return i, true
	case "DROP":
		i := skipIdent(1, "TABLE", "INDEX", "VIEW", "TRIGGER")
		i = skipIdent(i, "IF")
		i = skipIdent(i, "EXISTS")
		return i, true
	case "ALTER":
		i := skipIdent(1, "TABLE")
		return i, true
	case "PRAGMA":
		// "PRAGMA <db>.<name>": only the per-database pragmas route
		// (attachedPragmaScope, pragma.go); the rest fall through to
		// execPragma. Routing a per-connection pragma would apply it to the
		// attachment alone, and not routing a per-database one would apply it
		// to main; both are wrong answers.
		if len(toks) > 3 && toks[2].kind == tkPunct && toks[2].text == "." &&
			toks[3].kind == tkIdent &&
			attachedPragmaScope(r33sFoldIdent(toks[3].text)) == pragmaScopeRouted {
			return 1, true
		}
		return 0, false
	}
	return 0, false
}

// qualifierTokenName returns the database name a token spells when used as a
// schema qualifier -- an identifier (subject to the same reserved-keyword
// exclusion attachedTargetQualifier's own doc comment verifies) or a string
// literal, since ATTACH's own name is an EXPRESSION and both spellings are
// accepted wherever a database name is used. ok=false for a token that cannot
// spell a qualifier at all, or an unquoted reserved keyword.
func qualifierTokenName(tok token) (qname string, ok bool) {
	switch tok.kind {
	case tkIdent:
		if !tok.quoted && nonIdentifierKeywords[tok.upper()] {
			return "", false
		}
		return tok.text, true
	case tkString:
		return tok.str, true
	}
	return "", false
}

// createTriggerOnQualifierIndex walks CREATE TRIGGER's fixed header
// ([BEFORE|AFTER|INSTEAD OF] {DELETE|INSERT|UPDATE [OF cols]} ON) from token
// index from and returns the index where the ON clause's [schema.]table
// begins; ok=false for any other shape (left to parseCreateTriggerStmt).
func createTriggerOnQualifierIndex(toks []token, from int) (int, bool) {
	i := from
	if i < len(toks) && toks[i].kind == tkIdent {
		switch toks[i].upper() {
		case "BEFORE", "AFTER":
			i++
		case "INSTEAD":
			i++
			if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "OF" {
				i++
			}
		}
	}
	if i >= len(toks) || toks[i].kind != tkIdent {
		return 0, false
	}
	switch toks[i].upper() {
	case "DELETE", "INSERT":
		i++
	case "UPDATE":
		i++
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "OF" {
			i++
			for {
				if i >= len(toks) || toks[i].kind != tkIdent {
					return 0, false
				}
				i++
				if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "," {
					i++
					continue
				}
				break
			}
		}
	default:
		return 0, false
	}
	if i >= len(toks) || toks[i].kind != tkIdent || toks[i].upper() != "ON" {
		return 0, false
	}
	i++
	if i >= len(toks) || toks[i].kind != tkIdent {
		return 0, false
	}
	return i, true
}

// attachedCreateTriggerOnSelfQualifier reports the byte range of a CREATE
// TRIGGER's ON-clause qualifier when it equals q, the database the statement
// routed to. toks2/nameIdx are the re-lexed statement and its name's index.
// The delegated session treats q as its local schema, but CREATE TRIGGER's
// parser (scopeOfQualifier) only knows main/temp, so "ON q.table" must be
// reduced to "ON table". A different qualifier is left alone, and declines as
// in C.
func attachedCreateTriggerOnSelfQualifier(toks2 []token, nameIdx int, q string) (start, end int, ok bool) {
	i, ok := createTriggerOnQualifierIndex(toks2, nameIdx+1)
	if !ok || i+2 >= len(toks2) {
		return 0, 0, false
	}
	nameTok, dot := toks2[i], toks2[i+1]
	if dot.kind != tkPunct || dot.text != "." {
		return 0, 0, false
	}
	qname, qok := qualifierTokenName(nameTok)
	if !qok || !equalFoldName(qname, q) {
		return 0, 0, false
	}
	// Mirrors attachedTargetQualifier's own guard: the table name that follows
	// must not be a bare reserved keyword the parser would otherwise refuse.
	if target := toks2[i+2]; target.kind == tkIdent && !target.quoted && nonIdentifierKeywords[target.upper()] {
		return 0, 0, false
	}
	return nameTok.Start, toks2[i+2].Start, true
}

// stripAttachedCreateTriggerOnQualifier removes a CREATE TRIGGER's ON-clause
// qualifier from rewritten when it repeats q (see
// attachedCreateTriggerOnSelfQualifier). rewritten is re-lexed because the
// earlier splice shifted offsets. Returns rewritten unchanged when there is
// nothing to strip or the header does not parse (left for the parser).
func stripAttachedCreateTriggerOnQualifier(rewritten, q string) string {
	toks2, lexErr := lex(rewritten)
	if lexErr != nil {
		return rewritten
	}
	nameIdx, ok := attachedTargetTokenIndex(toks2)
	if !ok {
		return rewritten
	}
	start, end, ok := attachedCreateTriggerOnSelfQualifier(toks2, nameIdx, q)
	if !ok {
		return rewritten
	}
	return rewritten[:start] + rewritten[end:]
}

// wireOriginatingReaders lets the statement delegated to w read the databases
// it came from, so "INSERT INTO aux.x1 SELECT a,b FROM m1" (m1 in main) works.
//
// The order is the originating connection's: temp, main, then attachments in
// ATTACH order, so with m1 in both main and aux, "INSERT INTO aux.dst SELECT a
// FROM m1" copies main's rows; the target attachment does not come first.
// originReadersBefore expresses that to itemOwner.
//
// The readers belong to this one statement, hence the unwire function: after
// it, the attachment's snapshot is published as a plain read pager that must
// not reach main, or a view stored in the attachment would resolve its body
// there, which C refuses.
//
// reads says whether the statement can name a source at all; without one,
// snapshotting the originating database (O(dbsize), SnapshotPager) is wasted.
// Trigger-body statements (vdbe_trigger_routed.go) pass true.
func (db *DB) wireOriginatingReaders(w *DB, reads bool) (unwire func(), err error) {
	noop := func() {}
	if !reads {
		return noop, nil
	}
	before := 0
	for i, ad := range db.attached {
		if ad.wdb == w {
			before = i + 1 // +1 for the ORIGINATING database itself, first below
		}
	}
	if before == 0 {
		return noop, nil // not one of this session's attachments after all
	}
	origin, err := db.SnapshotPager()
	if err != nil {
		return noop, err
	}
	origin.SetLocalSchema(localSchemaOr(db.localSchema))
	// A FRESH slice, for the reason refreshAttachedReaders spells out: every
	// pager this statement materializes shares this backing array, so refilling
	// one in place would mutate a snapshot an outstanding read still holds.
	readers := make([]attachedReader, 0, len(db.attached)+1)
	// The TEMP database comes along: it belongs to the CONNECTION, so a
	// statement this session delegates ("INSERT INTO aux.dst SELECT a FROM
	// tmpsrc") reads the same temp objects the originating session sees. It is
	// first, which is its place in the search order (temp_store.go).
	if tr, terr := db.tempReader(false); terr != nil {
		return noop, terr
	} else if tr != nil {
		tr.pager.attachedReaders = append([]attachedReader{{name: "main", pager: origin}}, tr.pager.attachedReaders...)
		readers = append(readers, *tr)
		before++
	}
	readers = append(readers, attachedReader{name: r33sFoldIdent(localSchemaOr(db.localSchema)), pager: origin})
	for _, ad := range db.attached {
		if ad.wdb == w || ad.pager == nil {
			continue
		}
		readers = append(readers, attachedReader{name: r33sFoldIdent(ad.name), pager: ad.pager})
	}
	w.attachedReaders = readers
	w.originReadersBefore = before
	return func() {
		w.attachedReaders = nil
		w.originReadersBefore = 0
		origin.Close()
	}, nil
}

// tokensContainSelect reports whether a SELECT keyword appears anywhere in the
// statement -- the cheap, conservative test for "this statement reads
// something". Every source a routed write can name (an INSERT's source SELECT,
// a CREATE ... AS SELECT, a subquery in a SET/WHERE/VALUES) is inside one.
func tokensContainSelect(toks []token) bool {
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && t.upper() == "SELECT" {
			return true
		}
	}
	return false
}

// refreshAttachedWriteReaders re-points every attachment's read pager at its
// write session's current snapshot, so a later cross-database READ sees what a
// cross-database WRITE just did. Called after any delegated write, and after a
// fresh ATTACH (execAttach), which needs the identical re-pointing for the
// alias it just opened -- see below.
func (db *DB) refreshAttachedWriteReaders() error {
	inTxn := db.inTransaction() || len(db.savepoints) > 0
	for _, ad := range db.attached {
		// Inside a transaction a same-file sibling must not see another
		// handle's uncommitted writes (each ATTACH has its own pager cache):
		//
		//	BEGIN; INSERT INTO aux1.t VALUES(2)
		//	SELECT count(*) FROM aux2.t   -> 1
		//	SELECT count(*) FROM aux1.t   -> 2
		//
		// So the alias holding the write reads its own session and the rest
		// keep the last committed view; attachedCommitRefresh updates the group
		// after COMMIT. Outside a transaction the group rule below applies.
		if inTxn {
			if ad.reservedInTxn {
				if ad.wdb != nil {
					if err := db.repointAttachedPager(ad, ad.wdb); err != nil {
						return err
					}
				}
				continue
			}
			held := false
			for _, sib := range db.attached {
				if sib != ad && sib.reservedInTxn && samePath(sib.path, ad.path) {
					held = true
					break
				}
			}
			if held {
				continue
			}
		}
		// writer is the same-path attachment (ad or a sibling) whose session
		// should back ad's read pager: the one with wroteData (at most one per
		// path), else any open wdb in the group; nil if none ever opened one.
		// Every member, including one without its own wdb, refreshes from the
		// same session, so a read through any alias sees a sibling's writes,
		// as C's per-statement autocommit flush makes visible to a second
		// alias's independent pager. Re-deriving writer for each ad keeps this
		// order-independent (TestEngineAttachSameFileTwiceReaderStaysFresh,
		// TestEngineAttachSameFileTwiceReadsAndWrites).
		var writer *attachedDB
		for _, sib := range db.attached {
			if sib.wdb == nil || !samePath(sib.path, ad.path) {
				continue
			}
			if writer == nil {
				writer = sib
			}
			if sib.wroteData {
				writer = sib
				break // wroteData is unique per group; nothing outranks it
			}
		}
		if writer == nil {
			continue
		}
		if err := db.repointAttachedPager(ad, writer.wdb); err != nil {
			return err
		}
	}
	db.refreshAttachedReaders()
	return nil
}

// repointAttachedPager points ad's read pager at a snapshot of session w.
func (db *DB) repointAttachedPager(ad *attachedDB, w *DB) error {
	p, err := w.SnapshotPager()
	if err != nil {
		return err
	}
	p.SetLocalSchema(ad.name)
	if ad.pager != nil {
		ad.pager.Close()
	}
	ad.pager = p
	return nil
}

// attachedCommitLockCheck is the COMMIT half of the same-file alias rules: the
// RESERVED-to-EXCLUSIVE escalation vdbeCommit performs for every database the
// transaction wrote. It is refused while another handle on the same file holds
// SHARED (os_unix.c; memdb.c:407-409 "if( p->nRdLock>1 ) rc = SQLITE_BUSY"),
// which a sibling does once read or written in this transaction, or always
// under locking_mode=exclusive:
//
//	BEGIN; INSERT INTO aux1.t VALUES(2); SELECT count(*) FROM aux2.t
//	COMMIT   -> database is locked
//
// Called before anything is consumed, so a refusal leaves the transaction open,
// as C restores autoCommit on SQLITE_BUSY (vdbe.c:4042-4047).
func (db *DB) attachedCommitLockCheck() error {
	for _, ad := range db.attached {
		if !ad.reservedInTxn {
			continue
		}
		for _, sib := range db.attached {
			if sib == ad || !samePath(sib.path, ad.path) {
				continue
			}
			if sib.touchedInTxn || (sib.wdb != nil && sib.wdb.lockingHeld) {
				return fmt.Errorf("%w: cannot commit ATTACHed database %s: its file is also attached as %s, which holds a SHARED lock on it (C SQLite: COMMIT escalates %s's write lock to EXCLUSIVE, which a second handle's reader blocks)", ErrBusy, ad.name, sib.name, ad.name)
			}
		}
	}
	return nil
}

// reservedAttached is the set of attachments holding this transaction's write
// lock, taken before the commit clears it.
func (db *DB) reservedAttached() map[*attachedDB]bool {
	var out map[*attachedDB]bool
	for _, ad := range db.attached {
		if ad.reservedInTxn {
			if out == nil {
				out = map[*attachedDB]bool{}
			}
			out[ad] = true
		}
	}
	return out
}

// attachedCommitRefresh re-reads, from the committed file, every member of a
// same-file group that had a write in the transaction just committed: a
// sibling kept its pre-transaction view while the write was uncommitted
// (refreshAttachedWriteReaders), and now the file holds the result.
func (db *DB) attachedCommitRefresh(reserved map[*attachedDB]bool) {
	refreshed := false
	for _, ad := range db.attached {
		inGroup := false
		for r := range reserved {
			if samePath(r.path, ad.path) {
				inGroup = true
				break
			}
		}
		if !inGroup {
			continue
		}
		rp, err := openAttachedRead(ad.path)
		if err != nil {
			continue // leave the old pager rather than a nil one, as rollbackTxnAttachedWrites does
		}
		rp.SetLocalSchema(ad.name)
		if ad.pager != nil {
			ad.pager.Close()
		}
		ad.pager = rp
		refreshed = true
	}
	if refreshed {
		db.refreshAttachedReaders()
	}
}

// commitAttachedWrites commits every attached write session, called by COMMIT
// and Close before main's own commit, so main moves last (see the file doc for
// the atomicity ceiling).
//
// ponytail: a session whose Close fails is dropped (ad.wdb is cleared first)
// and cannot be committed again, while commitTxn leaves the transaction open
// and a retried COMMIT would report success having lost those writes. Lock
// conflicts are refused earlier (attachedCommitLockCheck); this only remains
// for a real I/O failure. Fix by making Close retryable or keeping the session
// on failure.
func (db *DB) commitAttachedWrites() error {
	var firstErr error
	for _, ad := range db.attached {
		if err := ad.closeWrite(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("engine: committing ATTACHed database %s: %w", ad.name, err)
		}
	}
	return firstErr
}

// discardAttachedWrites drops every attached write session without
// committing, for DB.Discard: nothing reached their files. SQL ROLLBACK does
// not come here (rollbackTxnAttachedWrites), since it must keep sessions holding
// autocommit writes C already committed.
func (db *DB) discardAttachedWrites() {
	for _, ad := range db.attached {
		ad.discardWrite()
	}
}

// execRoutedToAttached is ExecArgs' delegation step: if sqlText's target names
// an ATTACHed database, run it on that database's own write session with the
// qualifier stripped and report handled=true.
func (db *DB) execRoutedToAttached(sqlText string, args []Value) (rowsAffected, lastInsertID int64, handled bool, err error) {
	if len(db.attached) == 0 {
		return 0, 0, false, nil // the overwhelmingly common case: no attachments
	}
	return db.execRoutedToAttachedSlow(sqlText, args)
}

// execRoutedToAttachedSlow is execRoutedToAttached once there is an attachment.
// Split out because its deferred snapshots take &err, which moves err to the
// heap at function ENTRY -- one allocation per statement, paid by every write
// on a connection with nothing attached.
func (db *DB) execRoutedToAttachedSlow(sqlText string, args []Value) (rowsAffected, lastInsertID int64, handled bool, err error) {
	trimmed := strings.TrimSpace(sqlText)
	toks, lexErr := lex(trimmed)
	if lexErr != nil {
		return 0, 0, false, nil // let the ordinary path report the lex error
	}
	w, q, routed, ok, originating, schemaCascade, rerr := db.routeAttachedStatement(trimmed, toks)
	if !ok {
		return 0, 0, false, nil
	}
	if rerr != nil {
		return 0, 0, true, rerr
	}
	unwire, uerr := db.wireOriginatingReaders(w, tokensContainSelect(toks))
	if uerr != nil {
		return 0, 0, true, uerr
	}
	var capture func() []RowChange
	event, _ := attachedStatementEventKind(toks)
	if len(originating) > 0 && event != triggerInsert {
		capture = attachedRowCapture(w)
	}
	if len(originating) > 0 {
		// The delegated write and this session's trigger body are one
		// statement and must succeed or fail together. Neither touches its
		// file before Close/COMMIT, so paired statement snapshots on db and w,
		// taken before w writes, unwind both on any later failure (see
		// attachedOriginatingFireInsert).
		defer w.beginStatementSnapshot(&err)()
		defer db.beginStatementSnapshot(&err)()
	}
	ra, li, aerr := w.ExecArgs(routed, args)
	unwire()
	var touched []RowChange
	if capture != nil {
		touched = capture()
	}
	if aerr != nil {
		// A failed delegated statement may still have moved the attachment:
		// OR FAIL keeps the rows it applied (OE_Fail is not rolled back,
		// vdbeaux.c:3448), so "INSERT OR FAIL INTO aux.logged SELECT ..."
		// leaves rows in aux that ad.pager must show. Refreshed on every error
		// rather than classifying which errors leave w unchanged.
		if ferr := db.refreshAttachedWriteReaders(); ferr != nil {
			return 0, 0, true, aerr // the statement's own error outranks it
		}
		return 0, 0, true, aerr
	}
	// See attachedDB.wroteData's own doc comment: w now genuinely exists
	// (routeAttachedStatement's own lazy-open above), but that is not the
	// same question as whether it just MUTATED anything -- a routed
	// read-only pragma (integrity_check and friends) opens w for exactly the
	// same reason a routed write does, purely to answer "as that database".
	if ad := db.attachedNamed(q); ad != nil && attachedRoutedStatementWrites(toks, routed) {
		ad.wroteData = true
	}
	if len(originating) > 0 {
		name, nameOK := attachedRoutedTargetName(routed)
		if !nameOK {
			return 0, 0, true, fmt.Errorf("engine: internal: cannot re-resolve the target of a delegated ATTACHed INSERT this session must also fire a TEMP trigger against")
		}
		if event == triggerInsert {
			if ferr := db.attachedOriginatingFireInsert(originating, w, name, li); ferr != nil {
				return 0, 0, true, ferr
			}
		} else if ferr := db.attachedOriginatingFireRows(originating, w, name, event, touched); ferr != nil {
			return 0, 0, true, ferr
		}
	}
	// The LOCAL half of a routed DROP TABLE/VIEW or ALTER TABLE RENAME TO
	// cascade, applied now that the delegated half (inside w) has actually
	// succeeded -- see attachedSchemaCascade's own doc comment for why this
	// runs post-delegation rather than before.
	if schemaCascade != nil {
		switch schemaCascade.kind {
		case attachedSchemaCascadeDrop:
			db.removeAttachedTargetTriggers(schemaCascade.triggers)
		case attachedSchemaCascadeRename:
			if cerr := db.applyAttachedTriggerRenameCascade(schemaCascade.triggers, schemaCascade.oldName, schemaCascade.newName); cerr != nil {
				return 0, 0, true, cerr
			}
		}
	}
	if ferr := db.refreshAttachedWriteReaders(); ferr != nil {
		return 0, 0, true, ferr
	}
	// last_insert_rowid() is CONNECTION-wide in C SQLite, so an insert into an
	// attached database sets it here too.
	if li != 0 {
		db.lastInsertRowid = li
	}
	return ra, li, true, nil
}

// pragmaObjectScoped is the pragmas whose ARGUMENT names a schema object (a
// table or an index) rather than a value -- the ones queryPragmaStmt resolves
// across the attached databases via pragmaObjectOwner below. It is
// pragmaVtabTakesArg's set (vtab_pragma.go, the pragmas with a "pragma_*()"
// table-valued wrapper) plus index_xinfo, which has no wrapper but shares
// index_info's implementation and its argument rule.
var pragmaObjectScoped = map[string]bool{
	"table_info":       true,
	"table_xinfo":      true,
	"index_list":       true,
	"index_info":       true,
	"index_xinfo":      true,
	"foreign_key_list": true,
}

// pragmaObjectOwner returns the ATTACHED reader whose schema holds an object
// named name, when this pager's own schema does not -- what an object-scoped
// PRAGMA (and its pragma_* table-valued wrapper) has to consult, because real
// SQLite resolves such a name across every attached database with main first.
// nil when the object is here, is nowhere, or there are no attachments, all of
// which leave the caller on its own pager.
func (p *ReadOnlyPager) pragmaObjectOwner(name string) *ReadOnlyPager {
	if name == "" || len(p.attachedReaders) == 0 {
		return nil
	}
	if p.schemaHasObjectNamed(name) {
		return nil // main wins
	}
	for _, ar := range p.attachedReaders {
		if ar.pager != nil && ar.pager.schemaHasObjectNamed(name) {
			return ar.pager
		}
	}
	return nil
}

// schemaHasObjectNamed reports whether this database's catalog holds any object
// of that name -- table, index, view or trigger.
func (p *ReadOnlyPager) schemaHasObjectNamed(name string) bool {
	rows, err := p.Schema()
	if err != nil {
		return false
	}
	for i := range rows {
		if equalFoldName(rows[i].Name, name) {
			return true
		}
	}
	return false
}

// A three-part "schema.table.column" reference into an attached
// database through a nested join resolves by database identity: dbIdx
// on each tableScope discriminates every name match (resolveInScopes,
// qualifiedScopeAmbiguous, sameNamedScopeAlsoExposes,
// starQualifierAmbiguousAcrossDB; dbIdxForSchema, cross_db.go), as C
// does (resolve.c:125-156, 309-331; select.c:6165-6172, 6304).


// StripCreateTriggerOnSelfQualifier is stripAttachedCreateTriggerOnQualifier
// for the driver's routing: the driver strips the trigger NAME's qualifier
// (StripDDLTargetSchema) but not the ON clause's, which CREATE TRIGGER's
// parser (scopeOfQualifier) cannot resolve. schema is the attach name the
// statement was routed to; any other qualifier is left alone.
func StripCreateTriggerOnSelfQualifier(sqlText, schema string) string {
	if schema == "" {
		return sqlText
	}
	return stripAttachedCreateTriggerOnQualifier(sqlText, schema)
}


// CreateTriggerCrossSchemaError checks, after the self-qualifier is stripped,
// that no ON-clause qualifier names a different database for a non-TEMP
// trigger. C's answer (sqlite3FixSrcList via sqlite3FixInit,
// trigger.c:176-180) is "trigger %s cannot reference objects in database %s"
// (attach.c:498-501):
//
//	CREATE TRIGGER aux.g  ... ON aux.t   ok
//	CREATE TRIGGER aux.g  ... ON t       ok    (pinned to aux)
//	CREATE TRIGGER aux.g  ... ON main.t  trigger g cannot reference objects
//	                                     in database main
//	CREATE TEMP TRIGGER g ... ON aux.t   ok    (TEMP: no pin)
//
// Accepting the third would store a schema row C re-reads as a trigger on aux.t
// (trigger.c:155-160). isTemp lifts the rule.
func CreateTriggerCrossSchemaError(sqlText, schema string, isTemp bool) error {
	if schema == "" || isTemp {
		return nil
	}
	toks, err := lex(sqlText)
	if err != nil {
		return nil
	}
	nameIdx, ok := attachedTargetTokenIndex(toks)
	if !ok {
		return nil
	}
	i, ok := createTriggerOnQualifierIndex(toks, nameIdx+1)
	if !ok || i+2 >= len(toks) {
		return nil
	}
	if dot := toks[i+1]; dot.kind != tkPunct || dot.text != "." {
		return nil
	}
	qname, qok := qualifierTokenName(toks[i])
	if !qok || equalFoldName(qname, schema) {
		return nil
	}
	name, nok := qualifierTokenName(toks[nameIdx])
	if !nok {
		return nil
	}
	return fmt.Errorf("engine: trigger %s cannot reference objects in database %s", name, qname)
}

// StatementIsTempTrigger reports whether sqlText is a "CREATE TEMP TRIGGER" /
// "CREATE TEMPORARY TRIGGER". It is the one bit CreateTriggerCrossSchemaError
// needs from the statement that routing has not already extracted: a TEMP
// trigger's ON clause is NOT pinned to the trigger's own database, because
// sqlite3FixSrcList skips the whole per-item check when the fixer is TEMP
// (attach.c's "bTemp = (iDb==1)" branch).
func StatementIsTempTrigger(sqlText string) bool {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) < 3 {
		return false
	}
	if toks[0].kind != tkIdent || toks[0].upper() != "CREATE" {
		return false
	}
	if toks[1].kind != tkIdent {
		return false
	}
	switch toks[1].upper() {
	case "TEMP", "TEMPORARY":
	default:
		return false
	}
	return toks[2].kind == tkIdent && toks[2].upper() == "TRIGGER"
}
