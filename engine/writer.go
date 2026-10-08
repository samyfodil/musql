// This file is the write side's state: DB, one connection's view of a
// database on our format, and the snapshot it hands the read path.
//
// ---- the write model ----
//
// Statements change ROW STORES (row_store.go): each table's committed rows are
// read from the mapped segment file and its delta on demand
// (row_store_seg.go), and what this connection writes sits in an overlay on
// top. Nothing reaches the file until the Session that owns this DB commits
// (segment_write.go): an ordinary commit appends one batch to the delta, whose
// trailer is the commit marker; a catalog change rewrites the file
// (segment_ddl.go).
//
// Concurrency is the segment lock (lock.go, segment_delta.go): one exclusive
// byte-range lock serialises a delta append against a rewrite, and a writer
// whose view is older than the file's is refused with ErrBusy rather than
// allowed to write a stale image.
//
// Savepoints and ROLLBACK are row-store snapshots and undo logs (txn.go,
// row_store_undo.go).
package engine

import (
	"os"
	"strconv"
	"strings"
)

// DB is one connection's engine state over a database on our format: its
// catalog, its tables' row stores, its pragma settings and connection state.
// A Session embeds it and owns its commit (segment_write.go).
type DB struct {
	path string

	pageSize uint32
	// usable is the USABLE page size: pageSize, since this format reserves no
	// bytes per page. fts3 sizes its leaves from it, as C does from the pager's.
	usable uint32

	// pageSizeRequest is the LAST value any "PRAGMA page_size = N" asked for,
	// honorable or not -- C SQLite's db->nextPagesize, which pragma.c assigns
	// unconditionally (sqlite3Atoi of the text, so a non-numeric one is 0).
	// The pragma changes nothing else here (execPragmaSafeNoop): a page size
	// never alters a statement's rows or error text. What it DOES do is size the
	// next VACUUM INTO's copy -- see notePageSizeRequest, which owns the rule,
	// and execVacuumInto, which applies it.
	pageSizeRequest int

	tables   []*tableMeta   // every table registered this session (via CreateTable, or recovered by OpenWrite), in creation/schema order
	indexes  []*indexMeta   // every index registered this session (via CreateIndex, or recovered by OpenWrite), in creation/schema order; see index_write.go
	views    []*viewMeta    // every view registered this session (via CreateView, or recovered by OpenWrite), in creation/schema order; see view.go
	vtabs    []*vtabMeta    // every virtual table registered this session (via CreateVirtualTable, or recovered by OpenWrite), in creation/schema order; see vtab.go
	triggers []*triggerMeta // every trigger registered this session (via CreateTrigger, or recovered by OpenWrite), in creation order; see trigger.go

	//	// foreignTempTriggers holds triggerMeta pointers owned by a different
	//	// session -- the originating connection that delegated a write here
	//	// (attachedWriteSession) -- for TEMP triggers bound to this database's
	//	// tables (triggerMeta.tableAttachName). Trigger lookups (matchingTriggers,
	//	// tableHasTriggers) consult them alongside db.triggers, so the local
	//	// machinery fires them as its own. Each entry meets
	//	// attachedTriggerMirrorSafe; installed per routed write by
	//	// SetForeignTempTriggers, never cached across statements. Never written
	//	// to this database's schema: they belong to the other connection.
	foreignTempTriggers []*triggerMeta

	// schemaCookie is the FILE's schema cookie (header offset 40), what
	// "PRAGMA schema_version" reads. Only a change to the PERSISTENT schema
	// moves it -- see bumpSchema.
	schemaCookie uint32
	// schemaGen is the cache-invalidation generation for anything derived from
	// the schema (the write-plan cache, the foreign-key cache). It counts EVERY
	// schema change, temp objects included: a TEMP table is invisible to the
	// file's cookie but perfectly capable of making a compiled plan wrong.
	// Monotonic and never persisted, so it is always safe to bump.
	schemaGen uint32

	// schemaSeq is the row count the last schema load saw. It no longer hands
	// out ranks: each object's schemaSeq is its sqlite_schema rowid, numbered
	// max+1 by nextSchemaSeq -- see schemaSeqReserved.
	schemaSeq uint64
	// schemaSeqReserved is nextSchemaSeq's high-water mark for the statement
	// running now, main's then temp's.
	schemaSeqReserved [2]uint64

	// segWAL is "PRAGMA journal_mode=wal" on OUR format: a value the catalog
	// records (ConvertedCatalog.JournalWAL), with the delta as the log.
	segWAL bool

	// captureGuard is ConvertedCatalog.CaptureGuard; see SetCaptureGuard.
	captureGuard string

	// txGen counts wholesale restores of this session's logical state
	// (txn.go's restoreSnapshot -- ROLLBACK, and the mid-statement unwind an
	// "ON CONFLICT ROLLBACK" violation triggers). A restore swaps db.tables &
	// co. for FRESH CLONES while putting the snapshot's schemaCookie NUMBER
	// back, so the cookie alone cannot tell that every *tableMeta pointer just
	// changed identity. Anything caching a compiled program that baked such a
	// pointer in must key on this too -- see cachedWriteProgram.
	txGen uint32

	// writePlans memoizes compiled write Programs by statement text;
	// writePlanCookie/writePlanTxGen are the schemaCookie and txGen they were
	// compiled against. See cachedWriteProgram (vdbe_write.go) for why all
	// three guards are needed.
	writePlans      map[string]*Program
	writePlanCookie uint32
	writePlanTxGen  uint32
	// writePlanStat1 is the statistics snapshot they were planned under: an
	// ANALYZE over an existing sqlite_stat1 moves neither guard above.
	writePlanStat1 *planStat1

	// stat1 is sqlite_stat1 as this connection last LOADED it, and stat1Loaded
	// whether it has -- the Table/Index statistics C keeps in its schema. See
	// planStat1 (where_plan_stat1.go) for when a load happens.
	stat1       *planStat1
	stat1Loaded bool

	// userVersion mirrors the header's user-version field (offset 60): a
	// plain application-defined integer with no meaning to this engine
	// itself, read/written wholesale by PRAGMA user_version (see
	// pragma.go's execPragma/queryPragma) and persisted across Close/OpenWrite
	// exactly like schemaCookie already is.
	userVersion uint32

	// applicationID mirrors the header's application-id field (offset 68):
	// the same shape as userVersion above -- an application-defined 32-bit
	// integer this engine gives no meaning to, read/written wholesale by
	// PRAGMA application_id and persisted across Close/OpenWrite.
	//
	// It is a catalog field (ConvertedCatalog.ApplicationID), carried through
	// both conversions: a constant 0 once erased a C database's id on the way
	// through.
	applicationID uint32

	// lastInsertRowid mirrors sqlite3_last_insert_rowid(): the rowid of the
	// most recently successfully inserted row on this *DB (updated by
	// InsertArgs; untouched by UPDATE/DELETE/CREATE, exactly like real
	// SQLite's connection-level counter). Zero until the first successful
	// INSERT. See ExecArgs, which is what a database/sql driver's
	// driver.Result.LastInsertId reads this through, and conn_state.go, which
	// is what SQL's own last_insert_rowid() reads it through.
	lastInsertRowid int64

	// nChange / nTotalChange are sqlite3_changes() / sqlite3_total_changes():
	// the rows the most recently COMPLETED INSERT/UPDATE/DELETE modified, and
	// the running total since this connection opened. Both are connection
	// state, NOT database state -- C SQLite never resets either on ROLLBACK
	// -- so, like caseSensitiveLike, they are deliberately absent from
	// txSnapshot (txn.go). Moved only through setChanges; see conn_state.go for
	// every counting rule and the oracle evidence behind it.
	nChange      int64
	nTotalChange int64

	// nRowsInserted is insert.c's regRowCount for the most recently completed
	// INSERT -- what "PRAGMA count_changes" makes the statement answer as its
	// "rows inserted" column -- and it is NOT sqlite3_changes(). insert.c bumps
	// regRowCount only after sqlite3CompleteInsertion (insert.c:1600), and an
	// upsert whose DO UPDATE branch fired never gets there: OE_Update falls
	// through to OE_Ignore's "sqlite3VdbeGoto(v, ignoreDest)" (insert.c:2362),
	// which jumps to endOfLoop, PAST that AddImm. So
	// "INSERT ... ON CONFLICT DO UPDATE" that updates an existing row answers
	// "rows inserted" 0 while changes() is 1 -- verified against the oracle.
	// Published beside nChange at statement end; read through RowsInserted.
	nRowsInserted int64

	// totalChangesOpaque / lastRowidOpaque mark this session's total_changes()
	// and last_insert_rowid() unanswerable because a VIRTUAL TABLE write moved
	// C SQLite's own counters by amounts that are its module's internal
	// shadow-table SQL -- see markConnStateOpaque. They are separate because
	// ANY vtab row INSERT ends in an OP_VUpdate that OVERWRITES
	// last_insert_rowid() with the rowid it stored, which is exactly
	// reproducible while that module's row counts are not: lastRowidOpaque is
	// the one flag a later statement can clear (markVtabInsertRowid).
	totalChangesOpaque bool
	lastRowidOpaque    bool

	// internalSchemaInit suppresses the reserved-"sqlite_"-name check
	// (schema_write.go's checkReservedObjectName) while the ENGINE ITSELF
	// creates one of SQLite's own internal schema tables through the ordinary
	// write path -- today only ANALYZE's sqlite_stat1 (analyze_write.go's
	// storeStat1). It mirrors C SQLite's db->init.busy, which bypasses the
	// very same check for the very same reason. Never set while executing
	// user SQL.
	internalSchemaInit bool

	// aincCollect is where compileWrite gathers the statement's AUTOINCREMENT
	// tables as its INSERTs, trigger bodies included, are compiled
	// (noteAutoincrement) -- nil outside a compile.
	aincCollect *[]*tableMeta

	//	// tempDatabaseOpened is C's "db->aDb[1].pBt != 0": whether this
	//	// connection has opened its temp database. C opens it lazily from
	//	// sqlite3CodeVerifySchemaAtToplevel for iDb==1 (build.c:5366) and from a
	//	// "temp."-qualified pragma (pragma.c:457), and it stays open even after
	//	// every temp object is dropped. It decides whether "PRAGMA
	//	// temp_store=<v>" is a no-op or closes the temp database
	//	// (invalidateTempStorage); r35aStatementOpensTempDatabase keeps it
	//	// current.
	//	//
	//	// Never persisted: C SQLite's temp database dies with the connection.
	tempDatabaseOpened bool

	// tempDatabaseHeldObject is the narrower fact tempDatabaseOpened is NOT:
	// whether anything has ever been WRITTEN into the temp database, i.e.
	// whether a temp object has existed this session. A merely-opened temp
	// database still has zero pages; the first temp object gives it one (a
	// VIEW, schema only) or two (a TABLE), and it never shrinks back --
	// see r35aEmptyTempPragmaResult (temp_schema.go), which is what needs it.
	// Cleared only where C SQLite discards the pager outright, by a
	// successful temp_store change (r35aTempStoreSetter, pragma.go).
	tempDatabaseHeldObject bool

	// tempStore is C SQLite's db->temp_store, the value "PRAGMA temp_store"
	// last set (0 = compile-time default, 1 = file, 2 = memory), parsed by
	// r35aTempStoreValue exactly as pragma.c's getTempStore() does. It matters
	// only because changeTempStorage returns EARLY when the requested value
	// equals it -- a re-set to the value already in force is a no-op even in
	// the states where a real change would fail or would discard the temp
	// database.
	tempStore uint8

	// caseSensitiveLike is the per-connection "PRAGMA case_sensitive_like"
	// flag (see pragma.go's execPragma). When false (the default), LIKE folds
	// ASCII letters case-insensitively (C SQLite's default); when true, LIKE
	// compares case-sensitively. It is connection-level, NOT transactional --
	// C SQLite never resets it on ROLLBACK -- so it is deliberately absent
	// from txSnapshot (txn.go). SnapshotPager copies it into the ReadOnlyPager
	// it materializes so LIKE evaluation (sql_eval.go/vdbe.go) can consult it.
	caseSensitiveLike bool

	// untrustedSchema is the per-connection "PRAGMA trusted_schema" flag,
	// stored INVERTED so the zero value is SQLite's ON default (see
	// trusted_schema.go, which owns the rule and the oracle evidence). Like
	// caseSensitiveLike it is connection-level and NOT transactional -- a
	// setter issued inside BEGIN takes effect at once and survives a ROLLBACK
	// -- so it is deliberately absent from txSnapshot (txn.go). SnapshotPager
	// copies it into the ReadOnlyPager it materializes, which is where the
	// two DDL-origin sites (a view body, a trigger body) consult it.
	untrustedSchema bool

	// fkEnforce is the per-connection "PRAGMA foreign_keys" flag (see fk.go):
	// false (SQLite's own default) means no foreign key is enforced at all.
	// Like caseSensitiveLike it is connection-level and NOT transactional --
	// C SQLite never resets it on ROLLBACK, and a setter issued INSIDE a
	// transaction is silently ignored -- so it is deliberately absent from
	// txSnapshot (txn.go). SnapshotPager copies it into the ReadOnlyPager it
	// materializes so "PRAGMA foreign_keys" can report it back.
	fkEnforce bool

	//	// fts3AutomergeCache mirrors C fts3's per-connection p->nAutoincrmerge
	//	// (fts3Int.h:271, resolved by sqlite3Fts3PendingTermsFlush,
	//	// fts3_write.c:3357-3370): once a table's automerge=N is resolved or set,
	//	// it is sticky for the connection, surviving a wipe that clears the
	//	// %_stat row. See fts3ReadAutoincrmerge. Lazily allocated.
	//	//
	//	// Connection-level, not transactional, so absent from txSnapshot: a
	//	// ROLLBACK must not un-resolve a value C would still enforce. The driver
	//	// installs a per-connection map (SetFts3AutomergeCache).
	fts3AutomergeCache map[string]int64

	// writableSchema is the per-connection "PRAGMA writable_schema" flag: it
	// lets ordinary INSERT/UPDATE/DELETE run against sqlite_master
	// (schema_write_direct.go). Like caseSensitiveLike and fkEnforce it is
	// connection-level and NOT transactional -- verified directly against
	// mattn/go-sqlite3 3.53.3 that it still reads back 1 after a COMMIT and
	// after a ROLLBACK -- so it is deliberately absent from txSnapshot
	// (txn.go). SnapshotPager copies it into the ReadOnlyPager it materializes
	// so "PRAGMA writable_schema" can report it back.
	writableSchema bool

	// wsEdits / wsInserted / wsNatural are the direct-sqlite_master write
	// overlay -- what "PRAGMA writable_schema=ON" actually unlocks. See
	// schema_write_direct.go, which owns all three: wsEdits maps a catalog
	// row's stable key to the columns a statement assigned (or its removal),
	// wsInserted counts the rows an INSERT appended, and wsNatural is the
	// PRE-overlay row list the last materialize built (the "loaded schema"
	// this session keeps running on while page 1 says something else).
	wsEdits    map[wsCatalogKey]*wsCatalogEdit
	wsInserted int
	wsNatural  []catalogRow

	// wsAlterUndo is the state from before an ALTER TABLE ... RENAME TO
	// reloaded the schema ahead of running (writableSchemaDDLDecline). opDdl
	// restores it when the ALTER then fails, and clears it either way.
	wsAlterUndo *txSnapshot

	// wsSchemaCorruptObj is the ONE state a schema RELOAD can leave this
	// connection in that is not "some object changed": the name of the
	// catalog object whose row named a rootpage past the end of the file, at
	// which point C SQLite's whole schema load FAILS and every later
	// statement on that connection answers "malformed database schema (<obj>)
	// - invalid rootpage" instead of running. Empty when the schema is
	// loaded, which is every session that has never run
	// "PRAGMA writable_schema=RESET" over such an edit. Set only through
	// latchSchemaCorrupt (schema_reload_rootpage.go), which owns the C
	// citations and the measured evidence; read by ExecArgs and SnapshotPager;
	// cleared by "PRAGMA writable_schema=ON".
	wsSchemaCorruptObj string

	// wsSchemaCorruptDetail is corruptSchema's optional " - <detail>" suffix
	// ("invalid rootpage", "orphan index"), empty for the arms that pass none.
	wsSchemaCorruptDetail string

	// queryOnly is the per-connection "PRAGMA query_only" flag: while it is set,
	// any statement that would open a WRITE transaction fails with
	// "attempt to write a readonly database". Connection-level and NOT
	// transactional (the setter works inside a transaction and survives the
	// COMMIT, verified), so it is absent from txSnapshot. See
	// queryOnlyRefusesWrite.
	queryOnly bool
	// writeLock, when set, refuses writes as queryOnly does, with itself as the
	// error; see SetWriteLock.
	writeLock string
	// maxSize is the connection's "PRAGMA max_size": 0, or the most bytes main's
	// segment file and delta may hold after a commit; see SetMaxSize.
	maxSize int64

	// recursiveTriggers is the per-connection "PRAGMA recursive_triggers" flag
	// (trigger.go): false, SQLite's own default, means a trigger already on the
	// firing stack does not fire again. Connection-level and NOT transactional
	// -- verified that a setter issued inside a transaction takes effect at once
	// and is still set after the COMMIT -- so it is absent from txSnapshot, like
	// caseSensitiveLike and fkEnforce.
	recursiveTriggers bool

	//	// noAutoIndex is "PRAGMA automatic_index", inverted so the zero value is
	//	// SQLite's ON default. It is the switch whereLoopAddBtree consults
	//	// ("(pParse->db->flags & SQLITE_AutoIndex)!=0"), and it is observable:
	//	// the automatic index's key decides inner-loop row order. Connection-
	//	// level, not transactional (a db->flags bit), so absent from txSnapshot.
	//	// SnapshotPager copies it for markWherePlanEligibility.
	noAutoIndex bool

	//	// maybeReanalyze records, per main-schema table (r33sFoldIdent key),
	//	// whether a single-table SELECT this session ran had a WHERE matching a
	//	// secondary index's leading column as whereLoopAddBtreeIndex
	//	// (where.c:3220-3312) would score it -- the syntactic half of C's
	//	// TF_MaybeReanalyze (sqliteInt.h:2497). Set by markMaybeReanalyze, read
	//	// by execOptimize. Connection-level and not transactional, like C's
	//	// Table.tabFlags, which where.c only ever ORs.
	maybeReanalyze map[string]bool

	//	// sawJoinOrSubquery marks that some SELECT this session ran had a FROM
	//	// wider than one plain table (2+ sources, a join group, a derived table,
	//	// any WITH). That class is left untracked: C decides it at
	//	// whereCheckIfBloomFilterIsUseful (where.c:6605-6644, gated on nLevel>=2),
	//	// which needs the solved loop nest. Once set it stays set, and
	//	// execOptimize falls back to its stat1MatchesFreshAnalyze decline for
	//	// every table. Connection-level and not transactional.
	sawJoinOrSubquery bool

	//	// stat1Baseline is this connection's cached row count per main-schema
	//	// table as of the last ANALYZE run in this session -- C's
	//	// Table.nRowLogEst, refreshed only by analysisLoader
	//	// (analyze.c:1602-1647) from OP_LoadAnalysis (vdbe.c:7194) or a schema
	//	// reload (prepare.c:383). A direct UPDATE of sqlite_stat1 refreshes
	//	// neither, so it leaves this stale just as it leaves C's copy stale --
	//	// and PRAGMA optimize's growth check (OP_IfSizeBetween,
	//	// vdbe.c:6296-6323) must compare against this, not the table's current
	//	// content, or it would reanalyze where C leaves a hand-seeded row alone.
	//	//
	//	// Refreshed for every table after every stat1 write, as
	//	// sqlite3AnalysisLoad rereads the whole schema (analyze.c:1942). See
	//	// refreshStat1Baseline. Connection-level and not transactional.
	stat1Baseline map[string]int64

	// fkCache is this session's resolved foreign keys for one schema
	// generation, and fkStmt the current write statement's violation
	// bookkeeping. Both nil until enforcement is on; see fk.go.
	fkCache *fkSchema
	fkStmt  *fkStmtState

	//	// pendingLoadErr records an ensureTableLoaded failure hit where there is
	//	// no error return to use (fkParentHasKey/fkChildRowsMatching,
	//	// captureSnapshot). It is checked and cleared at fkFinishStatement/
	//	// fkCommitCheck and at ExecArgs' per-statement checkpoint, so it never
	//	// outlives a statement.
	pendingLoadErr error

	// fkDeferred is the outstanding DEFERRABLE INITIALLY DEFERRED violation
	// count -- fkey.c's db->nDeferredCons. Unlike fkStmt.counter it is
	// TRANSACTION state, not statement state: it survives every statement in
	// an explicit transaction and is reported at COMMIT (commitTxn), which is
	// why it lives here and rides in txSnapshot so ROLLBACK TO restores it.
	// See fk.go.
	fkDeferred int

	// deferFKs is "PRAGMA defer_foreign_keys" (fkey.c's SQLITE_DeferFKs), and
	// fkDeferredImm the SECOND deferred counter it fills (nDeferredImmCons).
	// While the flag is on EVERY foreign key's accounting -- immediate and
	// DEFERRABLE INITIALLY DEFERRED alike -- lands in fkDeferredImm, and the
	// check moves to the outermost COMMIT. Both are TRANSACTION state: this
	// engine accepts the setter only inside an explicit transaction (pragma.go
	// says why), and clearTxnState zeroes all three on every way out of one.
	// fkDeferredImm rides in txSnapshot beside fkDeferred, because real
	// SQLite's Savepoint struct saves and restores both. See fk.go.
	deferFKs      bool
	fkDeferredImm int

	// fkSuppressChild marks the row mutations fkBeforeDropTable makes as
	// child-side-exempt; see fkRowMutated.
	fkSuppressChild bool

	// textEncoding is this database's TEXT encoding (header byte 56): UTF8, or
	// UTF16LE/UTF16BE once "PRAGMA encoding" has switched a still-empty database
	// (pragma.go). It is a property of the FILE, stored in its catalog. Text is held
	// INTERNALLY as UTF-8 whatever it says; the encoding is applied at the three
	// seams utf16.go's doc comment lists.
	textEncoding TextEncoding

	// fts3Txn is the level-0 segment each fts3/fts4 table is accumulating into
	// for the current transaction (fts3_txn.go). nil in autocommit, where every
	// statement flushes its own segment exactly as C fts3 does.
	fts3Txn map[string]*fts3TxnSegment

	// fts3TxnTaint is set for the rest of the transaction the first time a
	// statement runs that fts3_txn.go's accumulating-segment model does NOT
	// account for -- see fts3TxnMaybeTaint. It exists only to gate optimize()
	// (fts3_optimize.go): that function's answer is C fts3's SEGMENT COUNT,
	// which this engine's eager per-statement rewrite matches byte for byte in
	// every case fts3_txn.go models, but not across the DDL/other-table/trigger
	// gap that file's own comment documents as unmodelled. Reset to false
	// everywhere fts3Txn itself resets to nil (fts3TxnSeal, fts3TxnDiscard) --
	// a COMMIT or SAVEPOINT brings both engines back in sync, so tracking
	// restarts clean from there.
	fts3TxnTaint bool

	// fts3AutomergeTouched is every fts3/fts4 table written inside the open
	// transaction, whatever its automerge setting was at the time: C fts3's
	// nLeafAdd counts leaves from writes made BEFORE "automerge=N" is set as
	// well (it is reset only at xBegin, fts3.c:3606), so a table can become a
	// merge candidate at COMMIT through a write the guard let through with
	// automerge still off. fts3AutomergeCommitCheck (fts3_automerge.go) is the
	// one reader. Cleared by clearTxnState, i.e. on every way a transaction
	// ends; deliberately NOT by ROLLBACK TO, because C's nLeafAdd is not
	// either.
	fts3AutomergeTouched map[string]bool

	// fts5TxnPending is the set of fts5 tables with a 'secure-delete' %_config
	// version bump deferred to the next transaction flush point (fts5_txn.go).
	// nil in autocommit, where the bump lands immediately.
	fts5TxnPending map[string]bool

	// journalModeName is this session's ROLLBACK journal mode: "" (the default,
	// delete), "truncate", "persist", "memory" or "off". Unlike WAL (segWAL) it
	// is NOT a property of the file -- C SQLite stores it nowhere on disk, so a
	// second connection to a database this one put in truncate mode still reads
	// back "delete" (verified). On this format every mode commits the same way
	// (a delta batch); "off" changes what an in-transaction undo does
	// (journalOffUndoDisabled), and the name is what the getter reports.
	journalModeName string

	// tempJournalMode is the TEMP database's own journal mode -- separate
	// connection state, not a second view of journalModeName. See
	// tempJournalModeResult (pragma.go) for the rules and their oracle
	// evidence; "" is the default (delete) and tempJournalModeUnmodelled marks
	// the one state this engine declines to answer for.
	tempJournalMode string

	// walIndexState is WHERE this connection's wal-index lives, which is the
	// one thing that decides whether "PRAGMA locking_mode=normal" can leave
	// exclusive locking mode on a WAL database. Maintained by
	// noteWalIndexStatement from ExecArgs and SnapshotPager below; see
	// walIndexKind (pragma.go) for the states and the pager.c rule behind them.
	walIndexState walIndexKind

	// lastVerbText/lastVerb/lastVerbOK memoize the single most recent
	// LeadingStatementVerb answer -- see DB.leadingVerb (pragma.go).
	lastVerbText string
	lastVerb     string
	lastVerbOK   bool

	// stmtDepth is how many top-level ExecArgs statements are on this session's
	// stack. It exists so the two walIndexState hooks fire ONCE per statement:
	// a write statement materializes its own read snapshots as it runs (a
	// trigger body, a RETURNING capture, a PRAGMA foreign_key_check), and those
	// inner SnapshotPager calls must not be mistaken for the read path's own
	// entry, whose statement text this session never sees.
	stmtDepth int

	// walAutoCheckpoint is the connection's "PRAGMA wal_autocheckpoint" (default
	// walDefaultAutoCheckpoint), held so the getter answers what was set. Its
	// unit is C's page frames, which this format's log does not count, so it
	// triggers nothing: the log is folded by compaction (compactIfWorthIt) on a
	// byte threshold instead.
	walAutoCheckpoint int

	// autoVacuum is this database's "PRAGMA auto_vacuum" mode: 0 (none), 1
	// (full) or 2 (incremental), a catalog field (ConvertedCatalog.AutoVacuumPlus1)
	// that an export stamps into C's header. execAutoVacuum changes it only where
	// C SQLite's setter changes pBt's -- on a database with no schema yet, or
	// between full and incremental on one that is already auto-vacuum.
	autoVacuum int

	// autoVacuumPendingPlus1 is the DEFERRED "PRAGMA auto_vacuum = MODE"
	// request: the mode C SQLite remembered but did not apply, because the
	// flag cannot change on a database that already has a page 1
	// (execAutoVacuum's case 3). The next VACUUM is where it CONVERTS the file,
	// which execVacuum does. Stored plus one -- exactly like
	// walAutoCheckpointPlus1 -- because 0 has to mean "no request at all" and
	// still leave "= 0", a request to turn auto-vacuum OFF, expressible. It is
	// per CONNECTION, so a driver whose autocommit model throws the session away
	// after one statement carries it across (SetAutoVacuumPending/
	// AutoVacuumPending, exactly like SetForeignKeys and SetWalAutoCheckpoint).
	autoVacuumPendingPlus1 int

	// secureDelete is this connection's "PRAGMA secure_delete" setting: 0 (off),
	// 1 (on) or 2 ("fast"). This format keeps no freed page content to erase,
	// so it changes nothing here; it exists to report the value back, and is
	// carried across sessions by the driver like the flags above.
	secureDelete int

	// lockingDefault / lockingMain are the two pieces of state "PRAGMA
	// locking_mode" reads and writes, kept apart because C SQLite keeps them
	// apart: lockingDefault is the CONNECTION-wide default (what the bare getter
	// reports, and what a database ATTACHed later inherits) and lockingMain is
	// THIS database's own mode (what every other form reports). See
	// execLockingMode for the full rule set and the oracle evidence.
	lockingDefault bool
	lockingMain    bool

	// lockingHeld records that this session holds the segment lock
	// locking_mode=exclusive promises to keep ACROSS statements (segLockFile).
	// See holdLockingModeLock.
	lockingHeld bool

	// activeWC is the write statement currently executing on this session
	// (runWrite sets and clears it), or nil outside one. It exists so code
	// reached from a statement WITHOUT the VM's writeCtx threaded through it
	// -- today only fk.go's action application, which mutates child tables
	// from the end-of-statement foreign key pass and from DROP TABLE -- can
	// push its undo onto the same statement journal.
	activeWC *writeCtx

	// writeRecChunk / writeRecChunkSize carry a write program's record arena
	// (vdbe.recAlloc) from one execution to the next, so a prepared single-row
	// INSERT run in a loop carves its stored rows from shared chunks instead of
	// allocating one per row. See runWrite.
	writeRecChunk     []Value
	writeRecChunkSize int

	// captureChanges/changeLog implement row-change capture for the replication
	// layer (see rowhook.go): off unless EnableRowChangeCapture was called,
	// drained by TakeRowChanges, truncated by the undo paths.
	captureChanges bool
	// captureFull says a consumer reads the OLD images and column names, not just
	// the segment delta. See noteRowChange for what it costs when nobody does.
	captureFull bool
	// commitsEachStatement: the caller commits after every autocommit
	// statement (SetCommitsEachStatement).
	commitsEachStatement bool
	changeLog   []RowChange

	// segments is the read side of a SEGMENT session (segment_write.go): the
	// pager whose ScanTable serves segments with the delta merged. Non-nil only
	// for OpenWrite, and it is what ensureTableLoaded reads a table's rows
	// through instead of opening db.path as a SQLite file -- there is no SQLite
	// file to open.
	segments *ReadOnlyPager

	// segLockFile is the held exclusive lock "PRAGMA locking_mode=exclusive"
	// promises on this format -- the sidecar lock file a commit would otherwise
	// take and release per commit. nil unless the mode is exclusive. See
	// holdLockingModeLock.
	segLockFile *os.File

	// rowidLo, rowidHi are this connection's rowid allocation range; see
	// SetRowidRange. Zero means the default, max+1.
	rowidLo, rowidHi uint64
	// rowidFloor is the range's per-table floor; see SetRowidFloor.
	rowidFloor func(table string) uint64

	//	// segColumns is a committed-state read handle that supplies column
	//	// blocks to the columnar fast paths while this session is held open. It
	//	// is not db.segments, which is read before a rewrite writes the file and
	//	// so is one rewrite behind. Rebuilt lazily on the first read after a
	//	// commit marks it stale, so the commit path never pays the re-open.
	segColumns      *ReadOnlyPager
	segColumnsStale bool

	//	// schemaCorrupt caches schemaLoadCorruptRefuses' verdict, which runs on
	//	// every non-PRAGMA statement and otherwise re-scans every catalog --
	//	// nearly half the cost of a prepared point lookup. C likewise reads the
	//	// schema once and re-reads only when the cookie moves (sqlite3ReadSchema).
	//	// Shared by pointer with every pager this session stamps (stampPager).
	schemaCorrupt *schemaCorruptVerdict

	// schemaCorruptGen is bumped by every commit, and is the second half of that
	// cache's key. See schemaCorruptVerdict. The compiled-plan cache keys on it
	// too (sharedPlanCache).
	schemaCorruptGen uint64

	// plans is the session's compiled-plan cache, shared by pointer with every
	// pager it stamps so a PREPARED statement is compiled once rather than once
	// per execution. See sharedPlanCache.
	plans *sharedPlanCache

	// schemaSeg is the catalog RENDERED as the five-column sqlite_master table,
	// kept across statements because a held connection rebuilds its read pager for
	// every one and re-rendering it was 67% of what that cost -- 983,945 objects
	// over 40,000 point lookups, for a table that had not changed.
	//
	// Keyed on the catalog's own fingerprint, the same key the plan cache learned
	// to use after a savepoint rollback restored a schema without moving either of
	// its other two. A segment is immutable once built, so sharing one between
	// pagers is safe by construction.
	schemaSeg    *segment
	schemaSegKey uint64

	// segSrcCache is the whole READ SOURCE -- live row stores, committed column
	// blocks, rendered catalog -- kept across statements for the same reason and
	// under the same discipline. See cachedSegSource for why sharing one is safe
	// and what the key covers.
	segSrcCache *segSource
	segSrcKey   uint64

	// segmentVacuumPending is a VACUUM waiting for its commit, on the segment
	// format. VACUUM there means what compaction means -- fold the delta in and
	// re-lay every table -- which is exactly the whole-file rewrite a commit
	// already knows how to do (Session.rewriteFile), so the statement records the
	// request and the commit performs it. See execVacuum's segment branch.
	segmentVacuumPending bool

	// deferredErr is the first error a function with no error result recorded
	// (deferErr), read at the end of the statement (takeDeferredErr).
	deferredErr error

	// temp is the TEMP database's header (temp_store.go), opened on the first
	// TEMP object exactly as sqlite3OpenTempDatabase opens C SQLite's
	// (build.c:2073); nil until then. Its objects are in the schema lists with
	// isTemp set and their rows in in-memory row stores.
	temp *tempDB
	// tempAtCommit is the TEMP database as the last successful commit left it,
	// which a session rebuilt after a FAILED commit returns to
	// (adoptTempCommittedFrom).
	tempAtCommit *TempDatabase
	// tempHandle is the connection's TEMP database this session was given or
	// published into, and tempSeen the version of it this session's temp state
	// reflects (SetTempDatabase).
	tempHandle *TempDatabase
	tempSeen   uint64
	// tempPair is the TEMP database's file (temp_file.go), nil until a commit
	// first writes one or when it is kept in memory.
	tempPair *tempPair

	// lastExecSQL / lastExecArgs are the statement CommitOrderHookForTest
	// reports for the commit that carries it; recorded only while the hook is
	// set.
	lastExecSQL  string
	lastExecArgs []Value

	//	// fts5ConfigGen counts writes to any fts5 table's %_config. It keys the
	//	// plan cache (sharedPlanCache) because a compiled program bakes in which
	//	// function "rank" calls, and that lives in a table, moving no schema
	//	// cookie. C fts5 keeps the same counter: Fts5Config.iCookie,
	//	// "Incremented when %_config is modified" (ext/fts5/fts5Int.h:254),
	//	// decremented by 'rank' (fts5_main.c:1830). fts5ConfigPut is the single
	//	// writer, so it is the one place that bumps.
	fts5ConfigGen uint64

	// fullColumnNames / shortColumnNamesOff are the connection's legacy
	// "PRAGMA full_column_names" / "short_column_names", stored so the zero
	// value is SQLite's default. They change only result column names.
	// Connection-level and not transactional, so absent from txSnapshot;
	// SnapshotPager copies them for expandSelectList.
	fullColumnNames     bool
	shortColumnNamesOff bool

	// localSchema is the database-qualifier name this write session answers to
	// (the write-path counterpart of ReadOnlyPager.localSchema) -- "" means the
	// default "main". Set (SetLocalSchema) when the driver Conn routes a
	// write referencing an ATTACHed database to that database's own file, so a
	// qualifier written in the SQL ("aux.t") matches the file it was routed to.
	// Copied into the pager SnapshotPager materializes. See
	// checkWriteSchemaQualifier (schema_qualifier.go).
	localSchema string

	// inMemory is the write-path counterpart of ReadOnlyPager.inMemory: this
	// database has no durable backing of its own (a ":memory:"/"mode=memory"
	// DSN). Set via SetInMemory, copied into the pager SnapshotPager
	// materializes, and read only by PRAGMA journal_mode (pragma.go).
	inMemory bool

	// tuning holds the per-database "PRAGMA synchronous / cache_size /
	// journal_size_limit / mmap_size" values this connection has set, keyed
	// schema.name. They configure C SQLite's dealings with the operating
	// system and nothing this engine does, so the stored value is their whole
	// observable behavior -- see pragma_tuning.go. SnapshotPager copies it onto
	// the reader so a getter answers what the writer set.
	pragmaState *PragmaConnState

	//	// attachedReaders are the other attached databases a cross-database DML
	//	// statement's source SELECT or subquery may read ("INSERT INTO main.t
	//	// SELECT * FROM aux.s"). Set by SetAttachedReaders and copied onto every
	//	// pager SnapshotPager makes, so foreign tables route to their owner as in
	//	// a cross-database SELECT. See cross_db.go.
	attachedReaders []attachedReader

	// originReadersBefore is the write-session counterpart of the pager field of
	// the same name: nonzero only while a statement is DELEGATED to this session
	// because it targets the attached database this session owns
	// (attach_write.go). Read by SnapshotPager, which is the one place it
	// reaches a read.
	originReadersBefore int

	// attached is this session's own ATTACH DATABASE registry, in attach
	// order: the bindings an "ATTACH '<path>' AS <name>" statement executed
	// THROUGH this engine created (see attach.go's execAttach/execDetach).
	// It is what keeps attachedReaders above populated for such a session --
	// as distinct from the driver Conn, which owns its own registry and
	// pushes the readers in via SetAttachedReaders without ever letting an
	// ATTACH statement reach the engine at all. The two never coexist on one
	// *DB: whichever layer handles ATTACH owns the readers.
	attached []*attachedDB

	// txnAttachedWrites is the subset of attached whose write session was
	// (re)opened INSIDE the current transaction, so a ROLLBACK has to throw it
	// away -- see attach_write.go's enterTxnAttachedWrite. Empty in autocommit,
	// and emptied again by clearTxnState on every way out of a transaction.
	txnAttachedWrites []*attachedDB

	// txActive/txSnapshot implement explicit SQL-text transaction control
	// (BEGIN/COMMIT/END/ROLLBACK -- see txn.go): txActive is true between a
	// successful BEGIN and the matching COMMIT/ROLLBACK; txSnapshot, non-nil
	// exactly when txActive is true, is the deep copy of every piece of
	// mutable logical state (tables/indexes/views plus the scalar counters
	// above) captured at BEGIN, which ROLLBACK swaps back in wholesale. See
	// txn.go's package doc comment for why a snapshot-and-restore.
	txActive   bool
	txSnapshot *txSnapshot

	// txMayHaveDirtiedMain is the transaction-scoped "the pager might already
	// have modified a page of main" bit that "PRAGMA journal_mode=<mode>"
	// consults while a transaction is open. It is a one-way over-approximation
	// on purpose -- see noteTransactionMayHaveDirtiedMain (txn.go), which is
	// the only thing that sets it, and execJournalMode, the only thing that
	// reads it.
	txMayHaveDirtiedMain bool

	// txDirtiedMain is that bit's DEFINITE counterpart: the transaction has
	// CERTAINLY modified a page of main (C SQLite's pager is at
	// PAGER_WRITER_CACHEMOD or beyond), so a journal_mode setter is certainly
	// in its silently-ignored regime. Set only by noteTransactionDirtiedMain
	// (txn.go) for the statement kinds whose page write was verified against
	// the oracle; false still means "unknown", never "clean" -- clean is what
	// txMayHaveDirtiedMain==false proves. Reset alongside it.
	txDirtiedMain bool

	//	// txSchemaChanged mirrors C's DBFLAG_SchemaChange (sqliteInt.h:1893):
	//	// set once a schema-changing DDL succeeds in the open transaction
	//	// (noteSchemaChange, from opDdl), reset at BEGIN and by clearTxnState.
	//	//
	//	// Consulted only by rollbackTxn/rollbackToSavepoint, mirroring
	//	// sqlite3RollbackAll's gate (main.c:1509) and OP_Savepoint's
	//	// SAVEPOINT_ROLLBACK (vdbe.c:3939), which reset the schema
	//	// (sqlite3ResetAllSchemasOfConnection, build.c:650) only after a schema
	//	// change. C's flag is also cleared by COMMIT (build.c:675), but since it
	//	// is only consulted inside a transaction, "since BEGIN" is equivalent.
	txSchemaChanged bool

	// rtreeConns is this session's connection's r-tree registry (rtree_conn.go).
	rtreeConns *RtreeConnections

	// heldTransaction marks a session a DRIVER is holding open as one logical
	// transaction (driver's BeginTx keeps a single engine.DB from BEGIN to
	// COMMIT and never issues a SQL "BEGIN", so txActive above stays false --
	// see driver/conn.go's Conn doc comment). Nothing in the engine's own
	// transaction machinery needs to know, because "keep the current in-memory
	// state" already IS the commit. It exists for the one write path whose
	// CORRECTNESS depends on where the transaction boundaries are: fts3/fts4
	// flushes its pending terms into a new index segment once per statement,
	// which matches C fts3's flush-at-COMMIT exactly in autocommit and not
	// at all inside a transaction, so vtab_fts3.go declines there. Any future
	// caller with the same dependency should consult it too.
	heldTransaction bool

	// deferredFKCommitter marks a held session whose holder runs the COMMIT
	// deferred-foreign-key check itself -- see MarkDeferredFKCommitter.
	deferredFKCommitter bool

	// savepoints is the SAVEPOINT stack (txn.go), outermost first: one entry
	// per SAVEPOINT that has been neither RELEASEd nor discarded, each
	// carrying its own snapshot of the state at the moment it was opened. It
	// is empty whenever txActive is false -- a savepoint cannot outlive its
	// transaction (verified: after "BEGIN; SAVEPOINT a; ROLLBACK", a
	// following "RELEASE a" reports "no such savepoint: a").
	savepoints []savepoint

	closed bool
}

// notePageSizeRequest records "PRAGMA page_size = <text>"'s one deferred
// effect (DB.pageSizeRequest): the size the next VACUUM INTO writes its copy
// at.
//
// It follows C's two steps: pragma.c assigns "db->nextPagesize =
// sqlite3Atoi(zRight)" unconditionally (non-numeric becomes 0), and vacuum.c
// calls sqlite3BtreeSetPageSize first with the source's size and then with
// nextPagesize, which ignores anything not a power of two in [512,
// SQLITE_MAX_PAGE_SIZE]. So an unusable request leaves the copy at the
// source's size, and a request is not consumed -- two VACUUM INTOs in a row
// both use it.
func (db *DB) notePageSizeRequest(text string) {
	// sqlite3Atoi's own shape: leading digits, everything else 0. strconv
	// rejecting the whole string gives the same 0.
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		n = 0
	}
	db.pageSizeRequest = n
}

// PageSizeRequest reports this session's deferred "PRAGMA page_size" request,
// and SetPageSizeRequest carries one in. They exist for the same reason
// AutoVacuumPending/SetAutoVacuumPending do: a driver whose autocommit model
// opens a fresh session per statement has to own the connection-level value
// itself, or the pragma is gone before the VACUUM INTO that consumes it runs.
func (db *DB) PageSizeRequest() int { return db.pageSizeRequest }

// PageSize is this database's own page size -- what "PRAGMA page_size" answers.
//
// Exported for the driver, which has to fill it into PragmaTuningDB: a couple of
// tuning pragmas convert a negative cache_size from KB to PAGES and need it. On
// the segment format the value comes out of the catalog (ConvertedCatalog.PageSize).
func (db *DB) PageSize() uint32         { return db.pageSize }
func (db *DB) SetPageSizeRequest(v int) { db.pageSizeRequest = v }

// vacuumIntoPageSize is the page size a VACUUM INTO copy comes out at: the
// deferred request when this engine can honor it, and otherwise the source's
// own size -- see notePageSizeRequest for why "otherwise" covers every
// unusable value rather than erroring on it.
func (db *DB) vacuumIntoPageSize() uint32 {
	n := db.pageSizeRequest
	if n < 512 || n > 65536 || !isPowerOfTwo(uint32(n)) {
		return db.pageSize
	}
	return uint32(n)
}

// Discard is the ROLLBACK counterpart to Close: it releases this DB without
// writing anything, so every mutation since it was opened is dropped and the
// file stays as it was. A driver transaction's Rollback calls it. Safe to
// call more than once, and mutually exclusive with Close (first wins).
func (db *DB) Discard() error {
	// A driver's ROLLBACK: over a schema change it resets the schema, which
	// disconnects every r-tree (sqlite3RollbackAll, main.c:1525).
	if db.txSchemaChanged {
		db.rtreeConns.reset()
	}
	// An ATTACHed database this session wrote into is discarded too: its own
	// session never touched its file (a session writes only at Close), so
	// dropping it is the whole rollback. See attach_write.go.
	db.discardAttachedWrites()
	if db.closed {
		return nil
	}
	db.closed = true
	db.closeAttached()
	return nil
}

// stampPager carries every piece of this session's connection state onto a read
// pager it hands out.
//
// ONE list, because there are two such pagers now -- SnapshotPager's image-backed
// one and segmentReadPager's (segment_write.go) -- and a flag set on one and not
// the other is a WRONG ANSWER rather than a missing feature: the getter reports a
// value the connection never set, or a planner flag stops reaching the plan. Both
// happened while this was inline in SnapshotPager.
func (db *DB) stampPager(p *ReadOnlyPager) {
	// The shared catalog-corruption verdict (DB.schemaCorrupt): by pointer, so a
	// statement's pager fills it in and the next statement's pager finds it.
	if db.schemaCorrupt == nil {
		db.schemaCorrupt = &schemaCorruptVerdict{}
	}
	p.schemaCorrupt = db.schemaCorrupt
	p.schemaCorruptGen = db.schemaCorruptGen
	if db.plans == nil {
		db.plans = &sharedPlanCache{}
	}
	p.sharedPlans = db.plans
	// ...and the statistics the connection last loaded, which is what C's
	// planner prices with -- not whatever sqlite_stat1 holds now.
	p.setPlanStats(db.planStats())
	p.caseSensitiveLike = db.caseSensitiveLike
	// ...and PRAGMA temp_store, which only the connection knows (see
	// DB.tempStore) and which its own getter answers from.
	p.tempStore = db.tempStore
	// ...and PRAGMA trusted_schema, which a view or trigger body reached from
	// this snapshot consults before it uses anything non-innocuous (see
	// DB.untrustedSchema and trusted_schema.go).
	p.untrustedSchema = db.untrustedSchema
	// ...and the connection's r-tree registry (rtree_conn.go).
	p.rtreeConns = db.rtreeConns
	// ...and PRAGMA defer_foreign_keys, whose getter has nowhere else to read
	// it from (see ReadOnlyPager.deferFKs).
	p.deferFKs = db.deferFKs
	// ...and PRAGMA automatic_index, which decides whether the ported planner
	// may build the transient index whose key sets the inner loop's visit order
	// (see DB.noAutoIndex and markWherePlanEligibility).
	p.noAutoIndex = db.noAutoIndex
	// ...and the session ITSELF, which is the only way a read reaches back to
	// the writer it was taken from. Its single caller is fts3's optimize()
	// (fts3_optimize.go); see ReadOnlyPager.writeSession for why that does not
	// weaken the snapshot's immutability.
	p.writeSession = db
	// ...and the PRAGMA foreign_keys flag, so its getter reports what the
	// connection actually set (see ReadOnlyPager.foreignKeys), and PRAGMA
	// secure_delete for the same reason (ReadOnlyPager.secureDelete).
	p.foreignKeys = db.fkEnforce
	p.secureDelete = db.secureDelete
	// ...and PRAGMA writable_schema, so its getter reports the flag this
	// session is really running under (ReadOnlyPager.writableSchema).
	p.writableSchema = db.writableSchema
	// ...and the tuning pragmas' per-database values, so their getters report
	// what this connection set rather than the defaults (pragma_tuning.go).
	p.pragmaState = db.pragmaState
	// While a direct sqlite_master write is outstanding, the image's page 1 no
	// longer describes the schema this session is running on -- exactly the
	// state C SQLite's writing connection is in, since it never reloads.
	// Seed the snapshot's memoized Schema() with the PRE-edit rows so table
	// resolution, table_info and integrity_check keep using the loaded schema,
	// while "SELECT ... FROM sqlite_master" (which SCANS page 1) shows the
	// edit. See schema_write_direct.go.
	if clean := db.writableSchemaCleanRows(); clean != nil {
		p.schemaRows, p.schemaLoaded = clean, true
	}
	// ...and, separately, the overlay-APPLIED list schemaCatalogRows
	// (temp_catalog.go) needs so a FILTERED "SELECT ... FROM sqlite_master/
	// sqlite_temp_master" -- which does not scan page 1 directly -- shows the
	// same edit the clean-seeded Schema() above deliberately does NOT. See
	// ReadOnlyPager.schemaRowsEdited (pager.go) and
	// writableSchemaEditedRows' own doc comment (schema_write_direct.go).
	p.schemaRowsEdited = db.writableSchemaEditedRows()
	// ...and the two PRAGMA locking_mode values, so the getter over a held
	// transaction reports the mode this session actually entered (pragma.go's
	// lockingModeResult).
	p.lockingDefault = db.lockingDefault
	p.lockingMain = db.lockingMain
	// ...and the connection's change counters / last_insert_rowid, so a SELECT
	// run over this snapshot answers changes()/total_changes()/
	// last_insert_rowid() with what this session has actually done. This is
	// also the seam that makes a trigger body's own SELECT and an
	// "INSERT ... SELECT changes()" source read the LIVE value: both
	// materialize their pager mid-statement, and setChanges has not run for
	// the statement in flight yet -- which is exactly when C SQLite still
	// reports the previous statement's count.
	p.changes, p.totalChanges, p.lastInsertRowid = db.nChange, db.nTotalChange, db.lastInsertRowid
	p.totalChangesOpaque, p.lastRowidOpaque = db.totalChangesOpaque, db.lastRowidOpaque
	// Likewise carry the legacy full_column_names / short_column_names flags
	// so output-column NAMING over this snapshot honors them (see
	// DB.fullColumnNames / shortColumnNamesOff and ReadOnlyPager.colNameMode).
	p.fullColumnNames = db.fullColumnNames
	p.shortColumnNamesOff = db.shortColumnNamesOff
	// ...and the fts5 %_config generation, which the plan cache keys on. See
	// DB.fts5ConfigGen.
	p.fts5ConfigGen = db.fts5ConfigGen
	// Carry the write session's database-qualifier name onto the snapshot, so a
	// schema-qualified read run against a held (attached-routed) transaction
	// resolves its qualifier the same way an autocommit read would.
	p.localSchema = db.localSchema
	// ...and likewise whether this database is memory-backed, so a read-side
	// PRAGMA journal_mode over a held transaction answers the same "memory"
	// an autocommit read would (pragma.go's journalModeResult) -- together with
	// the rollback journal mode this session is in, for the same reason (see
	// DB.journalModeName: nothing in the file records it).
	p.inMemory = db.inMemory
	// ...and the two facts "PRAGMA database_list" reports about this connection
	// (pragma_database_list.go): which file this database is, and whether the
	// TEMP one has been opened.
	p.mainPath = db.path
	p.tempOpen = db.TempDatabaseOpened()
	p.journalModeName = db.journalModeName
	p.tempJournalMode = db.tempJournalMode // the TEMP database's own, see tempJournalModeResult
	// Carry any cross-database attached readers onto the snapshot so a source
	// SELECT / subquery this write session runs (via pager.execSelect) can
	// resolve a foreign table against the database that owns it -- the write-path
	// counterpart of an autocommit cross-database read (see DB.attachedReaders).
}

// SnapshotPager is a read pager over this DB's CURRENT rows, uncommitted
// changes included, without touching the file: read-your-writes for an open
// transaction. Its row stores are frozen copies (segmentSnapshotPager), so a
// write that runs while a read holds the snapshot cannot change what it sees.
func (db *DB) SnapshotPager() (*ReadOnlyPager, error) {
	// A snapshot taken at the TOP LEVEL is the READ path's own entry: some
	// caller is about to run a query through it, and this session never sees
	// that query's text. Whether it opens a read transaction on main --
	// "SELECT * FROM t" does, "SELECT 1" does not -- therefore cannot be
	// decided here, so the wal-index state becomes UNKNOWN and PRAGMA
	// locking_mode=normal declines rather than guess. See walIndexKind. (Inner
	// snapshots, taken while a statement of this session's own is running, are
	// that statement's business and are covered by ExecArgs' own note.)
	if db.stmtDepth == 0 {
		db.noteWalIndexStatement(mainReadTxnUnknown)
	}
	// A failed schema load refuses every READ too -- this is the query side of
	// the same latch ExecArgs applies (schemaCorruptRefusesStatement,
	// schema_reload_rootpage.go). Every caller here is about to run a
	// statement against the snapshot, so refusing at the source is what makes
	// the refusal complete; a pragma answered from connection state alone
	// never reaches this function.
	if cerr := db.schemaCorruptErr(); cerr != nil {
		return nil, cerr
	}
	// Its own row stores plus the segments behind them -- COPIED, because the word
	// in the name is load-bearing. See segmentSnapshotPager for what reading the
	// live rows instead got wrong.
	return db.segmentSnapshotPager()
}

// CommitOrderHookForTest, when set, is called with the statement each commit
// carries -- the last one this session ran, which for an autocommit statement is
// the whole commit -- in COMMIT order: the call happens while the commit still
// holds the segment lock, so two concurrent writers cannot be recorded in the
// wrong order. The
// file's bytes depend on the order its writes arrived in, so a test that
// replays a concurrent run needs that order, not the order the calls returned
// in (n4_concurrent_test.go).
var CommitOrderHookForTest func(sqlText string, args []Value)

// deferErr records the first error a function with no error result hit; the
// statement reads it at its end (takeDeferredErr).
func (db *DB) deferErr(err error) {
	if db.deferredErr == nil {
		db.deferredErr = err
	}
}

// takeDeferredErr returns and clears the recorded error.
func (db *DB) takeDeferredErr() error {
	err := db.deferredErr
	db.deferredErr = nil
	return err
}
