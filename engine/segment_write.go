package engine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// Writing this format: a commit is an append. The rows a transaction changed
// are already recorded (RowChange, rowhook.go), so committing translates them,
// appends one batch to the delta and fsyncs: no image, journal or page diff.
//
// The write path (VDBE, triggers, FKs, constraints, upserts) works against
// rowStore (row_store.go), a map or segments with the session's changes on top
// (row_store_seg.go); this file owns where rows come from and what a commit
// does. A catalog change rewrites the whole file (Session.rewriteFile,
// segment_ddl.go).
//
// A segment file carries an index's SQL, not its data (segment_open.go): UNIQUE
// is enforced against the session's rows (row_store_uniqindex.go), and an index
// seek uses a posting map over the rows (row_store_eqindex.go) or a scan.

// Session is a write session on a segment file.
//
// It is a distinct type rather than DB itself because DB is the engine state and
// this is the file: Commit appends a delta batch and never touches the segments,
// and a catalog change rewrites the file.
type Session struct {
	// *DB is the engine state this session commits: its statements, pragmas,
	// attached readers and connection state. Embedded, so the session IS the
	// connection handle; Session's own Exec, Commit and Close shadow DB's.
	*DB
	src     *ReadOnlyPager // the segment read side: schema, segments, delta
	segPath string
	// deltaPath is segDeltaPath(deltaFor), kept so the per-statement staleness
	// check (pairStamp) does not build the string every time.
	deltaPath, deltaFor string

	baseCtr, basePages uint32 // the segment file's identity, for pairing
	endCtr, endPages   uint32 // the state the pair is at, which each commit moves

	// loadedCtr/loadedPages are the file state this session's rows reflect, unlike
	// endCtr, which is where the file is (refreshPairIdentity re-reads it before
	// every append, so another writer's append raises it). Comparing the file
	// against endCtr made a session permanently stale, missing every later commit
	// (seen as replicated nodes that never converged). These move only with the
	// rows: open, rebuild, file rewrite, or an append by a session that was
	// current.
	loadedCtr, loadedPages uint32

	tableIndex map[string]int
	ipkOf      map[string]int

	// lockFile is the descriptor this session's HELD locks live on, on the
	// SESSION rather than its *DB because a refresh replaces the *DB and the
	// locks belong to the connection. ONE descriptor for both holders below:
	// OFD locks conflict across descriptors even within a process, so a second
	// one would block against the first.
	//
	// modeHeld is "PRAGMA locking_mode=exclusive" (HoldLockingMode), and
	// lockExclusive its strength: SHARED after a read, EXCLUSIVE after a write
	// or in WAL mode. txnLock is a BEGIN IMMEDIATE/EXCLUSIVE's write transaction
	// (BeginWriteTxn), txnExclusive an EXCLUSIVE one's hold on the state byte.
	// applyLocks turns the four into the lock file's two bytes.
	lockFile                *os.File
	modeHeld, lockExclusive bool
	txnLock, txnExclusive   bool

	// appendAt is where this session's last batch ended, carried so a commit does
	// not re-read the whole delta to find its own tail. See SegDeltaAppendState.
	appendAt SegDeltaAppendState
	recBuf   []SegDeltaRecord // the delta records of a commit, reused (segDeltaRecordsInto)

	// catalogAtLastWrite is the catalog fingerprint as of the last time the FILE
	// was written. Commit compares against it rather than trusting Exec to have
	// noticed, because a caller may drive DB() directly -- a database/sql driver
	// does, since its whole execution path is written against *DB -- and then no
	// Exec of ours ever sees the statement. Detecting at the commit makes it
	// impossible to miss: whoever ran the DDL, the catalog either moved or it did
	// not.
	catalogAtLastWrite string

	// seenStat is the (size, mtime) of the segment file and its delta as of the
	// last time this session looked. The freshness check compares THESE first,
	// because the authoritative check -- reading the pair's committed state --
	// replays the delta, and doing that per statement is O(delta) per statement
	// and quadratic over a run of them. That cost is not hypothetical: it made a
	// two-node CRDT convergence test time out.
	seenStat [2]fileStamp

	// NoSyncOnCommit drops the fsync from every commit, which is the durability
	// contract C SQLite's WAL runs at by DEFAULT (synchronous=NORMAL): the batch
	// is still atomic, it is just not yet on the platter. Off by default -- a
	// segment commit is FULL by default, which is stricter than C's default and the
	// reason a like-for-like comparison has to say which it measured.
	NoSyncOnCommit bool

	// poisoned marks a session whose in-memory state can no longer be trusted:
	// its COMMIT failed with the rows already applied. The next freshness check
	// rebuilds it from the file (RefreshIfStale), which is the point where the
	// lock the commit could not get is free again.
	//
	// A flag rather than an immediate rebuild because the rebuild READS the file,
	// which takes the very lock whose unavailability poisoned the session -- so
	// rebuilding here would fail busy and leave the bad state exactly where it
	// was. And a flag rather than discarding the session, because the TEMP
	// database is the CONNECTION's and a discard would take it along
	// (adoptTempFrom).
	poisoned bool

	closed bool
}

// Poison marks this session's in-memory state untrustworthy, so the next
// statement rebuilds it from the file.
//
// Its caller is a COMMIT that failed after the statement had already applied its
// rows: the caller is told the statement failed, so those rows must not be
// committed by the NEXT statement -- this project's "halted statement still
// publishes" class. See the driver's commitHeldSession, which found it as a lost
// row under N4.
func (n *Session) Poison() { n.poisoned = true }

// OpenWrite opens a segment file for writing.
//
// Rows load LAZILY, per table, the first time a statement touches one -- so a
// point UPDATE against one table of many pays for that table alone, exactly as
// the SQLite-format path does.
func OpenWrite(segPath string) (*Session, error) {
	return OpenWriteWait(segPath, BusyTimeout)
}

// OpenWriteWait is OpenWrite waiting at most wait for another connection's lock
// on the file -- the opening connection's busy_timeout.
func OpenWriteWait(segPath string, wait time.Duration) (*Session, error) {
	// ONE READ, UNDER ONE SHARED LOCK. The rows, the file's base identity and the
	// STATE those rows are at must all come from the same look at the pair, and
	// getting that wrong was a lost-row bug, not a tidiness point: this opened the
	// pair (rows as of state S1), then called SegmentFileState separately (state S2
	// -- possibly many commits later), and took S2 as "the state my rows are at".
	// A session that had loaded an EMPTY table therefore believed it was current,
	// so the stale-image check passed, and it allocated rowids from 1 -- over rows
	// another connection had already committed at those rowids. Two writers'
	// rows collapsed onto one rowid and one row was gone
	// (TestSegmentConcurrentWritersLoseNothing).
	var src *ReadOnlyPager
	var baseCtr, basePages, endCtr, endPages uint32
	var tableIndex map[string]int
	var ipkOf map[string]int
	var rootEdits []ConvertedRootEdit
	if lerr := withSegmentReadLockWait(segPath, wait, func() error {
		f, st, rerr := readSegmentPairUnlocked(segPath)
		if rerr != nil {
			return rerr
		}
		tableIndex, ipkOf = SegmentFileTableIndex(f)
		rootEdits = append([]ConvertedRootEdit(nil), f.Catalog().RootEdits...)
		baseCtr, basePages = f.srcChangeCounter, f.srcPageCount
		endCtr, endPages = st.endCtr, st.endPages    // the replay's own result, not a second read
		p, perr := buildSegmentPager(segPath, f, st) // takes ownership of f
		if perr != nil {
			return perr
		}
		src = p
		return nil
	}); lerr != nil {
		return nil, lerr
	}

	schema, scerr := src.Schema()
	if scerr != nil {
		src.Close()
		return nil, scerr
	}
	db := &DB{
		path:              segPath,
		textEncoding:      UTF8,
		pageSize:          4096,
		usable:            4096,
		walAutoCheckpoint: walDefaultAutoCheckpoint,
		segments:          src,
	}
	db.schemaCookie = src.meta.schemaCookie // see ConvertedCatalog.SchemaVersion
	db.userVersion, db.applicationID = src.meta.userVersion, src.meta.applicationID
	db.segWAL = src.meta.wal         // see ConvertedCatalog.JournalWAL
	db.textEncoding = src.encoding() // see ConvertedCatalog.Encoding
	if ps := src.meta.pageSize; ps != 0 {
		db.pageSize, db.usable = ps, ps
	}
	db.autoVacuum = src.segAutoVacuum
	db.captureGuard = src.meta.captureGuard // see ConvertedCatalog.CaptureGuard
	if lerr := db.loadSchemaObjects(src, schema, "OpenWrite", false); lerr != nil {
		src.Close()
		return nil, lerr
	}
	// Opening the schema loads its statistics (sqlite3InitOne ->
	// sqlite3AnalysisLoad): what sqlite_stat1 holds NOW is what plans use until
	// the next load, whatever is written into it meanwhile.
	db.loadAnalysisFresh()
	db.enableDeltaRowCapture()
	n := &Session{
		DB: db, src: src, segPath: segPath,
		baseCtr: baseCtr, basePages: basePages,
		endCtr: endCtr, endPages: endPages,
		loadedCtr: endCtr, loadedPages: endPages, // the state just read

		tableIndex: tableIndex, ipkOf: ipkOf,
	}
	if len(rootEdits) > 0 {
		if rerr := db.replayRootEdits(rootEdits); rerr != nil {
			src.Close()
			return nil, rerr
		}
	}
	n.catalogAtLastWrite = n.catalogFingerprint()
	return n, nil
}

// PragmaHasNoMeaningHere refuses a pragma naming a SQLite-format mechanism this
// engine does not have (pages, rollback journal, write-ahead log). Declining
// rather than accepting as a no-op is invariant 2: an ignored pragma
// desynchronizes the next statement. Each entry says what the pragma meant, so
// anything later given a meaning on this format is removed deliberately.
func PragmaHasNoMeaningHere(name string) error {
	why, bad := pragmasWithNoMeaningHere[strings.ToLower(name)]
	if !bad {
		return nil
	}
	return fmt.Errorf("%w: PRAGMA %s has no meaning on this format (%s); it names a "+
		"SQLite-format mechanism this engine no longer uses", errVDBEUnsupported, name, why)
}

var pragmasWithNoMeaningHere = map[string]string{
	// page_count and freelist_count ANSWER (segment_read.go's segFilePages); this
	// one cannot, because its whole observable is a write FAILING. Our pages are not
	// C's pages, so the INSERT that fills C's ceiling leaves ours far from it:
	// accepting without enforcing is the accept-and-silently-ignore trap, and
	// enforcing against our own count makes every "fill until full" sequence
	// disagree with C.
	"max_page_count": "a page ceiling is observable only by making a write FAIL, and our pages are not C SQLite's pages -- the same INSERT that fills C's ceiling leaves ours far from it",
}

// Exec runs one statement.
//
// A statement that changes the SCHEMA is noted rather than refused: the catalog
// lives in the segment file's DIRECTORY, not in the delta, so the next Commit
// rewrites the file instead of appending a batch. See segment_ddl.go for why DDL
// is allowed to be the expensive path.
func (n *Session) Exec(sql string) error {
	if n.closed {
		return fmt.Errorf("engine: write session is closed")
	}
	if perr := pragmaDeclineFor(sql); perr != nil {
		return perr
	}
	return n.DB.Exec(sql)
}

// pragmaDeclineFor is PragmaHasNoMeaningHere applied to a statement, for the
// session entry points a caller reaches directly. Parsing only when the text
// could be a pragma at all keeps it off the hot path.
func pragmaDeclineFor(sql string) error {
	if len(sql) < 6 || !strings.EqualFold(strings.TrimLeft(sql, " \t\r\n")[:min(6, len(strings.TrimLeft(sql, " \t\r\n")))], "PRAGMA") {
		return nil
	}
	p, perr := ParsePragma(sql)
	if perr != nil || p == nil {
		return nil
	}
	return PragmaHasNoMeaningHere(p.Name)
}

// catalogFingerprint is every schema object's name and text, so a statement that
// moved the catalog can be caught rather than half-applied.
func (n *Session) catalogFingerprint() string {
	// The application-owned header words (user_version etc.) live in the
	// catalog on this format (ConvertedCatalog.UserVersion), so changing one
	// changes the catalog and the commit must rewrite the file; a delta
	// batch carries no catalog. The schema cookie is one of them: "PRAGMA
	// schema_version=777" alone was forgotten at reopen.
	out := "C" + strconv.FormatUint(uint64(n.schemaCookie), 10) +
		"U" + strconv.FormatUint(uint64(n.userVersion), 10) +
		"A" + strconv.FormatUint(uint64(n.applicationID), 10) +
		"E" + strconv.FormatUint(uint64(n.encoding()), 10) +
		"P" + strconv.FormatUint(uint64(n.pageSize), 10) +
		"V" + strconv.FormatUint(uint64(n.autoVacuum), 10) +
		"W" + strconv.FormatBool(n.segWAL) +
		"G" + n.captureGuard + "\x00"
	for _, t := range n.tables {
		out += "T" + t.name + "\x00" + t.sql + "\x00" + t.aliasOf + "\x00"
	}
	// A direct catalog edit changes what the file must say without changing the
	// live schema (applyCatalogEditsForWrite), so the overlay is part of it.
	if n.writableSchemaEditsActive() {
		for _, r := range n.wsCatalogWithOverlay() {
			out += "X" + fmt.Sprint(r.vals) + "\x00"
		}
	}
	for _, i := range n.indexes {
		out += "I" + i.name + "\x00"
	}
	// Views and triggers too. Leaving them out let CREATE VIEW through, which is
	// exactly the shape this exists to stop: accepted, visible for the session,
	// gone at the next open.
	for _, v := range n.views {
		out += "V" + v.name + "\x00"
	}
	for _, g := range n.triggers {
		out += "G" + g.name + "\x00"
	}
	// ...and VIRTUAL TABLES, for exactly the reason views and triggers are here. A
	// module with SHADOW TABLES (fts5, rtree) changed the table list above and so
	// survived by accident; one with NO storage of its own did not. "CREATE VIRTUAL
	// TABLE vrow USING fts5vocab(t, row)" worked in its own session and was GONE at
	// the next open -- the fingerprint never moved, so the commit appended a delta
	// batch and the catalog holding that definition was never written.
	for _, vt := range n.vtabs {
		out += "X" + vt.name + "\x00" + vt.sql + "\x00"
	}
	return out
}

// ExecArgsNoRet runs one statement and discards its counts, which is what a
// benchmark's inner loop wants.
func (n *Session) ExecArgsNoRet(sql string) error { return n.Exec(sql) }

// ExecArgs runs one statement with bound arguments.
func (n *Session) ExecArgs(sql string, args []Value) (int64, int64, error) {
	if n.closed {
		return 0, 0, fmt.Errorf("engine: write session is closed")
	}
	return n.DB.ExecArgs(sql, args)
}

// Commit appends everything executed since the last Commit as ONE delta batch
// and fsyncs it. That batch's trailer is the commit point.
//
// It reports whether anything was written: a session that changed no row appends
// nothing, which is what makes a read-only statement cost no I/O.
func (n *Session) Commit() (bool, error) {
	if n.closed {
		return false, fmt.Errorf("engine: write session is closed")
	}
	var wrote bool
	// ALREADY HELD? Then commit under it rather than taking a second one.
	// withSegmentWriteLock's lock is an OFD lock, which conflicts across
	// DESCRIPTORS even inside one process -- that is exactly what makes it work
	// between two connections here -- so a session holding the exclusive lock for
	// "PRAGMA locking_mode=exclusive" would block against ITSELF.
	//
	// COMPACTION RUNS UNDER THE SAME LOCK. It renames a new file over the segments
	// and deletes the delta; unlocked, another writer could refresh its pairing
	// first and then create a fresh delta naming the replaced file, or append a
	// batch the deletion then threw away.
	commit := func() error {
		var cerr error
		wrote, cerr = n.commitLocked()
		if cerr == nil && wrote {
			n.compactIfWorthIt()
		}
		return cerr
	}
	var err error
	if n.modeHeld {
		// A WRITE under the held lock escalates it to EXCLUSIVE and KEEPS it, which
		// is what C's pager does in exclusive locking mode (pager.c:5228/5405 keep
		// what a transaction took): after this, another connection can neither
		// write nor read.
		if err = n.HoldLockingMode(true); err == nil {
			err = commit()
		}
	} else if n.txnLock {
		// A BEGIN IMMEDIATE's commit: the reserved byte is ours already, so only
		// the state byte is taken -- on OUR descriptor, which a second one would
		// block against -- and handed back after, unless EXCLUSIVE keeps it.
		wasExcl := n.txnExclusive
		n.txnExclusive = true
		if err = n.applyLocks(); err == nil {
			err = commit()
		} else {
			err = segmentLockBusy(err, "write")
		}
		n.txnExclusive = wasExcl
		n.applyLocks()
	} else if n.DB != nil && n.segLockFile != nil {
		err = commit()
	} else {
		err = withSegmentWriteLockWait(n.segPath, n.busyTimeout(), commit)
	}
	if !n.InTransaction() {
		n.EndWriteTxn() // the transaction a BEGIN IMMEDIATE opened is over
	}
	return wrote, err
}

// commitSync is how this session's commits reach the disk: not at all under
// NoSyncOnCommit, F_FULLFSYNC under "PRAGMA fullfsync=ON", a plain fsync
// otherwise -- C's own choice (sqlite3PagerSetFlags, pager.c:3686-3690).
func (n *Session) commitSync() syncMode {
	switch {
	case n.NoSyncOnCommit:
		return syncNone
	case n.pragmaState.FullFsync():
		return syncFull
	}
	return syncNormal
}

// commitLocked is Commit with the segment-file write lock already held.
func (n *Session) commitLocked() (bool, error) {
	// The temp database is committed to its own file (commitTempFile),
	// since it belongs to the connection and a rebuilt session must recover
	// it from the file; it is committed even when main produced no delta.
	//
	// Main first, then temp: main's commit can still fail on another
	// connection (stale image, busy lock), and C takes every written
	// database's lock before any commits (vdbeCommit, vdbeaux.c:2948-2975),
	// so a failed COMMIT leaves nothing behind. Temp first left a failed
	// COMMIT half-durable ("table already exists" on retry). The temp
	// changes are taken out first so main's commit sees only its own.
	tempChanges := n.takeTempChanges()
	wrote, err := n.commitMainLocked()
	if err != nil {
		return false, err
	}
	// Still under the segment lock, so two writers' commits are reported in the
	// order they landed (CommitOrderHookForTest).
	if wrote && CommitOrderHookForTest != nil {
		CommitOrderHookForTest(n.lastExecSQL, n.lastExecArgs)
	}
	if terr := n.commitTempFile(tempChanges); terr != nil {
		return wrote, terr
	}
	// ...and this is the state a later failed commit returns temp to.
	n.noteTempCommitted()
	return wrote, nil
}

// commitMainLocked is commitLocked's main-database half.
func (n *Session) commitMainLocked() (bool, error) {
	// A VACUUM ASKED FOR THE REWRITE (execVacuum's segment branch): it is the same
	// whole-file re-lay a catalog change takes, for the same reason -- fold the
	// delta in, write one segment per table, drop the log.
	if n.segmentVacuumPending {
		n.segmentVacuumPending = false
		if err := n.rewriteFile(); err != nil {
			return false, err
		}
		n.clearWrittenSinceCommit()
		n.noteCommittedColumnsStale()
		return true, nil
	}
	if n.catalogFingerprint() != n.catalogAtLastWrite {
		// A catalog change cannot be a delta record, and it also invalidates every
		// record already in the log: a record names its table by its INDEX in the
		// directory, which a DROP moves. rewriteFile folds the log in and writes
		// the new directory in one step. See segment_ddl.go.
		if err := n.rewriteFile(); err != nil {
			return false, err
		}
		n.clearWrittenSinceCommit()
		n.noteCommittedColumnsStale()
		return true, nil
	}
	// Same reason rewriteFile does it: the delta header names the FILE's identity,
	// and another writer may have moved it since this session last looked.
	//
	// ONLY WHEN SOMETHING TOUCHED THE PAIR, though, because this is not cheap: it
	// re-reads the base, re-reads the state, and MAPS the segment file to re-derive
	// the table directory -- per commit, for a value that only a rewrite by someone
	// else can change. The stat pair is the same two syscalls RefreshIfStale uses
	// for the same question, and our own appends update seenStat, so a single
	// writer's run of commits skips it entirely. Measured on the 500-commit
	// segment-vs-C write bench: 955us per commit with it unconditional.
	if n.pairStamp() != n.seenStat {
		if rerr := n.refreshPairIdentity(); rerr != nil {
			return false, rerr
		}
	}
	changes := n.TakeRowChanges()
	// A ROW WRITE THE CHANGE LOG DOES NOT DESCRIBE cannot become a delta record,
	// and appending the log and stopping LOSES it. Fall back to the whole-file
	// rewrite, which writes every table's live rows and so cannot miss one.
	if n.mutatedOutsideChangeLog(changes) {
		if err := n.rewriteFile(); err != nil {
			return false, err
		}
		n.clearWrittenSinceCommit()
		n.noteCommittedColumnsStale()
		return true, nil
	}
	if len(changes) == 0 {
		return false, nil
	}
	// CONSUMING, when nothing else reads the log -- see segDeltaRecordsInto.
	recs, err := segDeltaRecordsInto(n.recBuf, changes, n.effectiveTableIndex(), n.ipkOf, n.storeOf(false))
	// Kept for the next commit, and emptied on the way out so it pins no row.
	defer func() { clear(recs[:cap(recs)]); n.recBuf = recs[:0] }()
	if err != nil {
		return false, err
	}
	if len(recs) == 0 {
		// Every change was to a table this file does not hold -- a TEMP table,
		// say. Nothing to append, and nothing lost.
		return false, nil
	}
	// The change counter moves one per commit (Open and RefreshIfStale
	// compare against it); the page count is carried unchanged, as identity
	// only.
	// Stale image: these records were computed from rows loaded at
	// loadedCtr, and the file has moved past that. Appending would be a lost
	// update, so it is refused with ErrBusy and the driver's busyRetry
	// re-runs the statement. The delta is absolute puts and kills, so a
	// record built from a stale image is wrong about what it replaces
	// (replicated nodes lost committed rows to an "INSERT OR REPLACE" on a
	// stale image).
	if n.loadedCtr != n.endCtr || n.loadedPages != n.endPages {
		return false, fmt.Errorf("%w: another connection committed while this transaction was open", ErrBusy)
	}
	// If this session's rows were current before this batch they still are
	// after it (the batch is its own work); if not, appending does not make
	// them current, since other writers' batches are still unread.
	// "PRAGMA max_size" (DB.SetMaxSize): if appending would exceed the
	// limit, rewrite instead, one file of the live rows, dropping dead
	// records (this transaction's deletes included, so deleting makes room),
	// judged whole by rewriteFile: it fits or nothing is written.
	if n.maxSize > 0 && n.deltaAppendExceeds(recs) {
		if err := n.rewriteFile(); err != nil {
			return false, err
		}
		n.clearWrittenSinceCommit()
		n.noteCommittedColumnsStale()
		return true, nil
	}
	wasCurrent := n.loadedCtr == n.endCtr && n.loadedPages == n.endPages
	n.endCtr++
	if aerr := appendSegmentDeltaAt(n.segPath, n.baseCtr, n.basePages, recs, n.endCtr, n.endPages, &n.appendAt, n.commitSync()); aerr != nil {
		n.endCtr-- // the append failed, so the state did not move
		return false, aerr
	}
	if wasCurrent {
		n.loadedCtr, n.loadedPages = n.endCtr, n.endPages
	}
	n.seenStat = n.pairStamp()
	n.clearWrittenSinceCommit()
	n.noteCommittedColumnsStale()
	return true, nil
}

// deltaAppendExceeds reports whether appending recs could leave main's segment
// file and delta over maxSize. It over-counts -- a torn tail the append would
// overwrite, the wider length prefix -- and that is safe: the cost of a false
// "yes" is the exact check rewriteFile makes.
func (n *Session) deltaAppendExceeds(recs []SegDeltaRecord) bool {
	size := segDeltaBatchBytes(recs)
	if st, err := os.Stat(n.segPath); err == nil {
		size += st.Size()
	}
	if st, err := os.Stat(segDeltaPath(n.segPath)); err == nil {
		size += st.Size()
	} else {
		size += segDeltaHdrSize // the append creates it
	}
	return size > n.maxSize
}

// mutatedOutsideChangeLog reports whether this session wrote some table's rows
// without the change log describing it, which a delta batch cannot express.
// Virtual tables do this: fts3/fts4/fts5 and rtree rewrite their shadow tables
// wholesale after each mutation (fts5SyncShadows) through putRow/dropRow,
// noting no RowChange. Committing only the log lost their data ("CREATE VIRTUAL
// TABLE v USING fts5(c); INSERT INTO v VALUES('apple')" reopened empty).
//
// ponytail: the answer is the whole-file rewrite. Turning shadow rows into
// records needs the pre-mutation rowid set the wholesale rewrite discards, so
// the vtab layer would have to record its own writes; do that if vtab-heavy
// write loads matter.
func (db *DB) mutatedOutsideChangeLog(changes []RowChange) bool {
	described := make(map[string]bool, len(changes))
	for _, c := range changes {
		if !c.Temp {
			described[strings.ToLower(c.Table)] = true
		}
	}
	for _, t := range db.tables {
		if t.isTemp || !t.rowsWrittenSinceCommit || t.rows == nil {
			continue
		}
		if !described[strings.ToLower(t.name)] {
			return true
		}
	}
	return false
}

// clearWrittenSinceCommit resets the per-commit half of the written flag, for
// every table this commit has just made durable.
func (db *DB) clearWrittenSinceCommit() {
	for _, t := range db.tables {
		if !t.isTemp { // the TEMP file's commit clears its own (temp_file.go)
			t.rowsWrittenSinceCommit = false
		}
	}
}

// Close commits and releases the session.
func (n *Session) Close() error {
	if n.closed {
		return nil
	}
	// Attached databases this session wrote into are committed first, so
	// main's file moves last (attach_write.go). Session.Close does not go
	// through DB.Close, so without this an engine-direct session's attached
	// writes were never committed.
	//
	// An explicit transaction still open is not committed: closing the
	// connection rolls it back (sqlite3LeaveMutexAndCloseZombie,
	// main.c:1398; sqlite3RollbackAll; TestSavepointClosePersistence). Only
	// autocommit work, which an engine-direct session batches until here, is
	// committed.
	var err error
	if n.InTransaction() {
		_, _, err = n.DB.ExecArgs("ROLLBACK", nil)
	}
	if aerr := n.commitAttachedWrites(); aerr != nil && err == nil {
		err = aerr
	}
	if _, cerr := n.Commit(); cerr != nil && err == nil {
		err = cerr
	}
	n.closeAttached()
	n.EndWriteTxn()
	n.ReleaseLockingMode()
	n.releaseSegmentLockingModeLock() // the engine-direct pragma's lock (holdLockingModeLock)
	n.closed = true
	n.dropTempFileIfUnclaimed() // C's temp database is OPEN_DELETEONCLOSE
	n.DB.closed = true
	if cerr := n.src.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// HoldLockingMode takes the lock "PRAGMA locking_mode=exclusive" holds between
// statements, or strengthens it to EXCLUSIVE; it never weakens one. The driver
// calls it at every access while the connection is in exclusive mode, because
// that is when C takes it ("the change does not actually take effect until the
// next time the database file is accessed"): SHARED on a read, so another
// connection may read but not write; EXCLUSIVE on a write, and at once in WAL
// mode, where C gives up the shared-memory wal-index and holds the file.
//
// It is the lock every commit and every read-state check takes
// (withSegmentWriteLock/withSegmentReadLock), on the sidecar lock file, held on
// a descriptor of this session's own.
func (n *Session) HoldLockingMode(exclusive bool) error {
	if n.modeHeld && (n.lockExclusive || !exclusive) {
		return nil
	}
	wasHeld, wasExcl := n.modeHeld, n.lockExclusive
	n.modeHeld, n.lockExclusive = true, exclusive || n.lockExclusive
	if err := n.applyLocks(); err != nil {
		n.modeHeld, n.lockExclusive = wasHeld, wasExcl
		n.applyLocks() // only gives back what the failed step took
		kind := "read"
		if exclusive {
			kind = "write"
		}
		return segmentLockBusy(err, kind)
	}
	return nil
}

// ReleaseLockingMode drops what HoldLockingMode holds -- the "=normal" half,
// which C performs at the next access rather than at the pragma.
func (n *Session) ReleaseLockingMode() {
	if !n.modeHeld {
		return
	}
	n.modeHeld, n.lockExclusive = false, false
	n.applyLocks()
}

// BeginWriteTxn takes a write transaction's lock at its BEGIN, as C's
// sqlite3BeginTransaction does for IMMEDIATE and EXCLUSIVE (build.c:5258-5271,
// OP_Transaction with a write flag on every database): the RESERVED byte, so
// no other connection can commit until this transaction ends and its own
// COMMIT can never find the file moved under it -- and for exclusive, the
// state byte too, which keeps readers out as C's EXCLUSIVE lock does. The
// caller refreshes AFTER this, so the transaction starts from the newest
// state. EndWriteTxn gives it back.
func (n *Session) BeginWriteTxn(exclusive bool) error {
	n.txnLock, n.txnExclusive = true, exclusive
	if err := n.applyLocks(); err != nil {
		n.txnLock, n.txnExclusive = false, false
		n.applyLocks()
		return segmentLockBusy(err, "write")
	}
	return nil
}

// EndWriteTxn releases BeginWriteTxn's lock. Idempotent; every end of a
// transaction reaches it (Commit, Discard, Close, and the driver's ROLLBACK).
func (n *Session) EndWriteTxn() {
	if !n.txnLock {
		return
	}
	n.txnLock, n.txnExclusive = false, false
	n.applyLocks()
}

// applyLocks sets the lock file's two bytes to what the holders want, taking
// the stronger lock first so a failure leaves nothing half-taken beyond what
// the caller's rollback of its flags gives back. A lock on a range this
// descriptor already holds is converted in place (fcntl's F_SETLK semantics).
func (n *Session) applyLocks() error {
	stateExcl := (n.modeHeld && n.lockExclusive) || n.txnExclusive
	stateShared := n.modeHeld && !stateExcl
	reserved := (n.modeHeld && n.lockExclusive) || n.txnLock
	if !stateExcl && !stateShared && !reserved {
		if n.lockFile != nil {
			releaseLock(n.lockFile, segStateByte, 2)
			n.lockFile.Close()
			n.lockFile = nil
		}
		return nil
	}
	if n.lockFile == nil {
		f, err := openSegmentLockFile(n.segPath)
		if err != nil {
			return err
		}
		n.lockFile = f
	}
	wait := n.busyTimeout()
	if reserved {
		if err := acquireLock(n.lockFile, segReservedByte, 1, true, wait); err != nil {
			return err
		}
	} else {
		releaseLock(n.lockFile, segReservedByte, 1)
	}
	switch {
	case stateExcl:
		return acquireLock(n.lockFile, segStateByte, 1, true, wait)
	case stateShared:
		return acquireLock(n.lockFile, segStateByte, 1, false, wait)
	}
	return releaseLock(n.lockFile, segStateByte, 1)
}

// AccessesMainFile reports whether C would open main's file for sqlText -- a read
// transaction on main, mainReadTxnOf's measured table -- which is when a pending
// "PRAGMA locking_mode=exclusive" takes its lock. An unclassified statement
// counts as an access: the lock it takes can only turn another connection's
// statement into a BUSY, never answer anything wrong.
func (n *Session) AccessesMainFile(sqlText string) bool {
	return n.mainReadTxnOf(sqlText) != mainReadTxnNo
}

// effectiveTableIndex is tableIndex with every ALIAS (tableMeta.aliasOf) routed
// to its owner's directory entry: the two share one row store, so a change made
// through either name is a change to the owner's rows.
func (n *Session) effectiveTableIndex() map[string]int {
	var out map[string]int
	for _, t := range n.tables {
		if t.aliasOf == "" || t.isTemp {
			continue
		}
		ti, ok := n.tableIndex[t.aliasOf]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]int, len(n.tableIndex))
			for k, v := range n.tableIndex {
				out[k] = v
			}
		}
		out[t.name] = ti
	}
	if out == nil {
		return n.tableIndex
	}
	return out
}

// LockingModeHeld reports whether HoldLockingMode's lock is held.
func (n *Session) LockingModeHeld() bool { return n.modeHeld }

// Discard abandons the session without committing, which loses every change
// made through it -- the delta is append-only and nothing was appended.
func (n *Session) Discard() error {
	if n.closed {
		return nil
	}
	// The CONNECTION is being thrown away, so every attached write session goes
	// with it uncommitted -- what DB.Discard does, and the counterpart to Close's
	// commitAttachedWrites above.
	n.discardAttachedWrites()
	n.closeAttached()
	n.EndWriteTxn()
	n.ReleaseLockingMode()
	n.releaseSegmentLockingModeLock() // the engine-direct pragma's lock (holdLockingModeLock)
	n.closed = true
	n.dropTempFileIfUnclaimed() // C's temp database is OPEN_DELETEONCLOSE
	n.DB.closed = true
	return n.src.Close()
}

// CompactIfLargerThan folds the delta back into columnar segments once it
// exceeds nBytes. It is a policy separate from the commit, which stays "append
// and fsync"; a caller that never calls it keeps a growing log and correct
// answers. Nothing calls it.
//
// The columnar fast paths merge the log (segment_delta_merge.go) at O(delta):
// "count(*) WHERE v < ?" over 100,000 rows takes 2.78ms with the table in the
// log and 88us folded (C: 2.12ms). Wiring it into autocommit failed: a
// compaction rewrites the file and the triggering session reloads, so a
// 100,000-row autocommit load at a 256 KiB threshold compacted every ~5,000 rows
// and ran for minutes (262ms without). It needs a session to survive its own
// compaction without reloading every table.
func (n *Session) CompactIfLargerThan(nBytes int64) (bool, error) {
	st, err := os.Stat(segDeltaPath(n.segPath))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if st.Size() < nBytes {
		return false, nil
	}
	did, cerr := CompactSegmentFile(n.segPath)
	if did {
		// The blocks moved, so the columnar read handle has to be rebuilt before
		// the next fast path reads one (DB.segColumns).
		n.noteCommittedColumnsStale()
	}
	return did, cerr
}

// ReadPager is a read handle on this session's current state, committed rows
// plus uncommitted changes. The session already holds the merged rows, so the
// pager points at them (a map lookup per table) rather than materializing an
// image. Every table is loaded first, a one-time cost for a held connection.
func (n *Session) ReadPager() (*ReadOnlyPager, error) {
	if n.closed {
		return nil, fmt.Errorf("engine: write session is closed")
	}
	return n.segmentReadPager()
}

// segmentReadPager is ReadPager without the session wrapper, so the WRITE path can
// take the same snapshot.
//
// It has to be reachable from *DB, because every sub-read a write statement
// performs goes through DB.SnapshotPager -- an "INSERT ... SELECT", a trigger
// body, ALTER TABLE's own scans, RETURNING's captured rows, a pragma that reads.
// SnapshotPager used to materialize a SQLite IMAGE, which a segment-backed
// session does not have, so all of them failed with "SnapshotPager: engine: page
// store: invalid argument". "INSERT INTO t5 SELECT * FROM t4" was the visible one:
// it errored, t5 stayed empty, and then "2 IN t5" answered 0 where C answers 1 --
// 60 divergences in the corpus's own in1.test from one missing seam.
func (db *DB) segmentReadPager() (*ReadOnlyPager, error) { return db.segmentPager(false) }

// segmentSnapshotPager is segmentReadPager frozen: its rows are a copy taken
// now, so a statement writing while reading through it sees the table as it was.
// C evaluates every row's WHERE before writing any when the WHERE has a subquery
// (delete.c:497-498; update.c:723-737, reason (5)). Reading live rows,
//
//	INSERT INTO t2(x) VALUES(1),(2),(3),(4),(5);
//	DELETE FROM t2 WHERE EXISTS(SELECT 1 FROM t2 AS v WHERE v.x=t2.x-1);
//
// left 1,3,5 where C leaves 1 (also delete.test, update.test, delete4.test).
//
// ponytail: every loaded table is copied up front. Copy-on-write would copy
// only written tables but needs the frozen pager's lifetime to be explicit;
// writeSubqueryPager's call site (vdbe_write.go) is where to make it lazier.
func (db *DB) segmentSnapshotPager() (*ReadOnlyPager, error) { return db.segmentPager(true) }

// segmentPager builds the read pager over this session's rows, live or frozen.
func (db *DB) segmentPager(freeze bool) (*ReadOnlyPager, error) {
	p := newReadOnlyPager()
	// The schema this session can name, and the handle its committed column
	// blocks come from. Both are inputs to the source below AND to its cache key.
	rows := db.segmentSchemaRows()
	cols := db.committedColumns()
	// ONE fingerprint per statement, shared by the two things that want one: the
	// read source's cache key below, and the plan cache's validity check later
	// (ReadOnlyPager.schemaFingerprint, which memoises on this field).
	p.schemaFP = schemaRowsFingerprint(rows)
	src, serr := db.cachedSegSource(rows, cols, freeze, p.schemaFP)
	if serr != nil {
		return nil, serr
	}
	p.segs = src
	p.schemaRows = rows
	p.schemaLoaded = true
	// THE ATTACHED DATABASES AND TEMP, exactly as DB.SnapshotPager wires them
	// (writer.go): a statement this session runs may name a table another database
	// owns, and this is the pager that has to resolve it. Without them an
	// ATTACHed database was invisible to every read a segment-backed session did --
	// "SELECT ... FROM aux.b" answered "no such table: aux.b" for a database the
	// same session had just attached, which is how the mined corpus (engine-direct)
	// sees ATTACH at all.
	p.attachedReaders = db.attachedReaders
	p.originReadersBefore = db.originReadersBefore
	// The temp reader is built from the session's live temp objects, catalog
	// included, rather than from the temp FILE, which nothing writes until the
	// commit: "SELECT name FROM temp.sqlite_master" answered zero rows for a temp
	// table the same session had just created, and a read cannot wait for one.
	if tr, terr := db.tempReader(freeze); terr != nil {
		return nil, terr
	} else if tr != nil {
		// TEMP goes FIRST, and reaches main and the attachments through readers of
		// its own -- see SnapshotPager's own note for the rule and its citation.
		tr.pager.attachedReaders = append([]attachedReader{{name: "main", pager: p}}, p.attachedReaders...)
		p.attachedReaders = append([]attachedReader{*tr}, p.attachedReaders...)
		p.originReadersBefore++
	}
	// EVERY piece of connection state a read answers from, through the SAME list
	// SnapshotPager uses (stampPager, writer.go). This pager grew its own partial
	// copy and the gaps were wrong answers: changes() read 0 after a statement that
	// changed two rows, and "PRAGMA automatic_index=OFF" never reached the plan, so
	// a transient index set the inner loop's visit order and an order-sensitive
	// aggregate over a join came out in the wrong one.
	db.stampPager(p)
	// The schema cookie is the one thing stampPager cannot carry: the image-backed
	// pager reads it out of the image's own header, and a segment file keeps it in
	// its catalog instead (ConvertedCatalog.SchemaVersion). The session bumps its
	// copy on every DDL, and a read in the same session must see that rather than
	// the last rewrite's value, or "PRAGMA schema_version" answers stale.
	p.meta.schemaCookie = db.schemaCookie
	p.meta.userVersion, p.meta.applicationID = db.userVersion, db.applicationID
	p.meta.wal = db.segWAL
	p.meta.captureGuard = db.captureGuard
	p.meta.encoding = db.encoding()
	p.meta.pageSize = db.pageSize
	p.segAutoVacuum = db.autoVacuum
	return p, nil
}

// segmentSchemaRows renders this session's schema as sqlite_master rows: one per
// table, index, virtual table, view and trigger, in that order, with each
// table's SYNTHETIC root page and everything else's 0.
//
// One renderer, because there are two readers of it now -- the read pager
// (segmentPager, which also wires each table's live rows) and the direct
// catalog write's own view of the catalog it is about to edit
// (segmentNaturalCatalog, segment_schema_write.go). A row present in one and
// not the other would make an UPDATE of sqlite_schema edit a row no reader can
// see, or miss one it can.
func (db *DB) segmentSchemaRows() []SchemaRow {
	rows := make([]SchemaRow, 0, len(db.tables)+len(db.indexes)+len(db.views)+len(db.triggers)+len(db.vtabs))
	for _, t := range db.tables {
		// TEMP tables INCLUDED, unlike the file rewrite which must exclude them:
		// a temp table is session-private and never persisted, but it is very much
		// readable -- leaving it out made "SELECT ... FROM temp.t" report "no such
		// table" for a table the same session had just created.
		rows = append(rows, SchemaRow{
			Type: "table", Name: t.name, TblName: t.name, RootPage: t.rootPage,
			SQL: t.sql, Temp: t.isTemp, Rowid: int64(t.schemaSeq), AliasOf: t.aliasOf,
		})
	}
	for _, idx := range db.indexes {
		// AN AUTOMATIC INDEX IS LISTED TOO, with no SQL -- which is exactly what C
		// writes for one (a sqlite_schema row whose sql is NULL). Skipping them made
		// "SELECT type, name, tbl_name, sql FROM sqlite_schema" omit
		// sqlite_autoindex_t1_1 for a table with a UNIQUE column, where C reports it.
		//
		// A WITHOUT ROWID table's PRIMARY KEY index is the exception: C names it but
		// writes no row for it at all (see segment_open.go's own note, and
		// pragmaIndexInfo's withoutRowidAutoPKOwner).
		if idx.sql == "" && idx.isTablePK {
			continue
		}
		rows = append(rows, SchemaRow{
			Type: "index", Name: idx.name, TblName: idx.table, RootPage: 0,
			SQL: idx.sql, Temp: idx.isTemp, Rowid: int64(idx.schemaSeq),
		})
	}
	// VIRTUAL TABLES, as "table" rows with rootpage 0 -- what C writes for one.
	// Missing them made a vtab invisible to every read in its own session: an rtree
	// created, filled and reopened answered "no such table: t" while its three
	// shadow tables sat right there in the file.
	for _, vt := range db.vtabs {
		rows = append(rows, SchemaRow{Type: "table", Name: vt.name, TblName: vt.name,
			RootPage: 0, SQL: vt.sql, Temp: vt.isTemp, Rowid: int64(vt.schemaSeq)})
	}
	for _, v := range db.views {
		rows = append(rows, SchemaRow{Type: "view", Name: v.name, TblName: v.name,
			SQL: v.sql, Temp: v.isTemp, Rowid: int64(v.schemaSeq)})
	}
	for _, g := range db.triggers {
		rows = append(rows, SchemaRow{Type: "trigger", Name: g.name, TblName: g.table,
			SQL: g.sql, Temp: g.isTemp, Rowid: int64(g.schemaSeq)})
	}
	// ...and then into CREATION order, which is C's: sqlite_schema is read in
	// rowid order and every CREATE appends. Grouped by kind, "SELECT
	// group_concat(name) FROM sqlite_schema" answered "a,b,c,sqlite_autoindex_a_1,bi"
	// where C answers "a,sqlite_autoindex_a_1,b,bi,c".
	//
	// MAIN's rows before TEMP's: each catalog numbers its own rows from 1
	// (nextSchemaSeq), so the two sequences say nothing about each other, and a
	// TEMP object sorted in among main's made a first-match lookup of
	// "main.trial" find the TEMP "trial".
	seqs := make([]uint64, 0, len(rows))
	seqOf := func(temp bool, seq uint64) uint64 {
		if temp {
			return seq | 1<<62
		}
		return seq
	}
	for _, t := range db.tables {
		seqs = append(seqs, seqOf(t.isTemp, t.schemaSeq))
	}
	for _, idx := range db.indexes {
		if idx.sql == "" && idx.isTablePK {
			continue
		}
		seqs = append(seqs, seqOf(idx.isTemp, idx.schemaSeq))
	}
	for _, vt := range db.vtabs {
		seqs = append(seqs, seqOf(vt.isTemp, vt.schemaSeq))
	}
	for _, v := range db.views {
		seqs = append(seqs, seqOf(v.isTemp, v.schemaSeq))
	}
	for _, g := range db.triggers {
		seqs = append(seqs, seqOf(g.isTemp, g.schemaSeq))
	}
	ranks := catalogRanks(seqs)
	keys := make([]catalogOrderKey, len(ranks))
	for i, r := range ranks {
		keys[i] = catalogOrderKey{rank: r}
	}
	return sortCatalogRows(rows, keys)
}

// Insert, Update and Delete run one statement and return the row count
// (insert_write.go, write_update_delete.go).
//
// Thin on purpose: they exist so a caller that holds a session does not have to
// know which format it has, which is what the engine's own tests were relying on
// when their fixtures moved to this format.
func (n *Session) Insert(sql string) (int, error) { return n.countingExec(sql) }

// Update runs one UPDATE and returns the number of rows it changed.
func (n *Session) Update(sql string) (int, error) { return n.countingExec(sql) }

// Delete runs one DELETE and returns the number of rows it removed.
func (n *Session) Delete(sql string) (int, error) { return n.countingExec(sql) }

// countingExec is the three above: run it, commit if this is autocommit, report
// how many rows it touched.
func (n *Session) countingExec(sql string) (int, error) {
	if n.closed {
		return 0, fmt.Errorf("engine: write session is closed")
	}
	nRows, _, err := n.DB.ExecArgs(sql, nil)
	if err != nil {
		// An OR FAIL halt KEEPS the rows it wrote before the offending one, and in
		// autocommit C commits them and counts them: "UPDATE t SET b=b+10" over a
		// UNIQUE ON CONFLICT FAIL column reports changes()=2 with rows 1 and 2
		// updated. The driver's finishAutocommit already does this; returning 0
		// here and skipping the commit left those rows reported as unchanged and
		// sitting uncommitted in the session. Any other error undid the statement.
		if !ErrorKeepsAutocommitChanges(err) {
			return 0, err
		}
		if _, cerr := n.CommitIfAutocommit(); cerr != nil {
			return int(nRows), cerr
		}
		return int(nRows), err
	}
	if _, cerr := n.CommitIfAutocommit(); cerr != nil {
		return int(nRows), cerr
	}
	return int(nRows), nil
}

// The remaining *DB pass-throughs a caller holding a session needs, delegated
// rather than reached through DB(): the statement forms with arguments, the
// connection-level flags whose getters a test reads back, and the two RETURNING
// entry points.
//
// They are one-liners because the session's job is the STORAGE contract (commit
// by appending a batch, refuse a stale image); what a statement MEANS is the same
// on either format and belongs to *DB.
func (n *Session) ExecReturningArgs(sql string, args []Value) ([]string, [][]Value, error) {
	return n.DB.ExecReturningArgs(sql, args)
}

// InsertArgs, UpdateArgs and DeleteArgs are the bound-parameter forms.
func (n *Session) InsertArgs(sql string, args []Value) (int, error) {
	return n.DB.InsertArgs(sql, args)
}

// UpdateArgs runs one parameterised UPDATE.
func (n *Session) UpdateArgs(sql string, args []Value) (int, error) {
	return n.DB.UpdateArgs(sql, args)
}

// DeleteArgs runs one parameterised DELETE.
func (n *Session) DeleteArgs(sql string, args []Value) (int, error) {
	return n.DB.DeleteArgs(sql, args)
}

// DeferForeignKeys / SetDeferForeignKeys and TrustedSchema are connection state a
// reader answers from, so a caller that sets them on a session must be able to
// read them back.
func (n *Session) DeferForeignKeys() bool     { return n.DB.DeferForeignKeys() }
func (n *Session) SetDeferForeignKeys(v bool) { n.DB.SetDeferForeignKeys(v) }
func (n *Session) TrustedSchema() bool        { return n.DB.TrustedSchema() }
func (n *Session) MarkHeldTransaction()       { n.DB.MarkHeldTransaction() }
func (n *Session) JournalMode() string        { return n.DB.JournalMode() }

// TempPath and SetTempPath are the connection's TEMP database file, delegated so
// a caller that holds a session does not have to reach through DB() for it.
// TempDatabase and SetTempDatabase hand the connection's TEMP database out of
// and into this session (temp_store.go).
func (n *Session) TempDatabase() *TempDatabase     { return n.DB.TempDatabase() }
func (n *Session) SetTempDatabase(t *TempDatabase) { n.DB.SetTempDatabase(t) }

// SnapshotPager is this session's read pager, named as DB.SnapshotPager so a
// caller need not know the format. It is the read path's top-level entry, so it
// deposits the provisional wal-index note QueryArgs resolves
// (resolveWalIndexRead): a read that opens the WAL ("SELECT * FROM
// sqlite_master" under journal_mode=wal) must count, or a later exclusive-mode
// write opens it in heap memory and "PRAGMA locking_mode=normal" answers
// exclusive where C answers normal (wal2.test wal2-7, walnoshm.test).
func (n *Session) SnapshotPager() (*ReadOnlyPager, error) {
	if n.stmtDepth == 0 {
		n.noteWalIndexStatement(mainReadTxnUnknown)
	}
	// ...and corruptSchema's latch refuses the read, as DB.SnapshotPager's own
	// check does: a RESET (or a load) that failed the schema leaves every
	// statement answering "malformed database schema", reads included.
	if cerr := n.schemaCorruptErr(); cerr != nil {
		return nil, cerr
	}
	return n.ReadPager()
}

// ExecReturning runs a write that RETURNS ROWS -- an INSERT/UPDATE/DELETE with a
// RETURNING clause -- and commits it unless a transaction is open.
//
// It is separate from Query because a RETURNING statement is a WRITE: handing it
// to ReadPager's read-only pager fails, which is exactly what happened when the
// driver's query path was pointed at a segment-backed session without this
// ("INSERT ... RETURNING" reached a pager that cannot write).
func (n *Session) ExecReturning(sql string, args []Value) ([]string, [][]Value, error) {
	if n.closed {
		return nil, nil, fmt.Errorf("engine: write session is closed")
	}
	cols, rows, err := n.DB.ExecReturningArgs(sql, args)
	if err != nil {
		return nil, nil, err
	}
	if _, cerr := n.CommitIfAutocommit(); cerr != nil {
		return nil, nil, cerr
	}
	return cols, rows, nil
}

// Query runs a read against this session's current state.
func (n *Session) Query(sql string, args []Value) ([]string, [][]Value, error) {
	rp, err := n.ReadPager()
	if err != nil {
		return nil, nil, err
	}
	defer rp.Close()
	return rp.QueryArgs(sql, args)
}

// ---- the surface a database/sql driver needs from a held session ----
//
// A driver holding ONE session per connection does not have to bridge
// connection state across sessions, which is what the SQLite-format path spends
// a good deal of Conn on: an autocommit statement there opens a throwaway *DB,
// so changes(), total_changes() and last_insert_rowid() have to be seeded into
// it and read back out again. Here there is one session, so the engine's own
// state IS the connection's.

// InTransaction reports whether an explicit BEGIN or SAVEPOINT is open.
//
// It is what tells a driver whether a statement it just ran should be COMMITTED
// (autocommit) or left in memory for a later COMMIT. The engine answers it
// because the engine is what processed the BEGIN.
func (n *Session) InTransaction() bool { return n.HasActiveTransaction() }

// CommitIfAutocommit commits unless an explicit transaction is open -- the
// autocommit rule in one call, so a driver cannot get the order wrong.
func (n *Session) CommitIfAutocommit() (bool, error) {
	if n.InTransaction() {
		return false, nil
	}
	return n.Commit() // Commit compacts; see compactIfWorthIt
}

// compactIfWorthIt folds the log into columnar segments once it exceeds a
// fraction of the file, so the columnar fast paths read blocks instead of
// merging the log at O(delta) per query. On a 20,000-row "count(*) WHERE v < ?":
//
//	whole table in the log      571.6us
//	after the log is folded in   13.2us
//	...and once the session re-reads the file  4.9us
//
// A quarter of the file, with a floor: at threshold == file size the log never
// catches up after one early compaction; a quarter bounds write amplification at
// 4x. The floor stops a small database compacting every commit.
//
// Failure is not propagated: a compaction that cannot run leaves a correct
// database with a longer log. compactionOffForTest holds the delta in place for
// tests.
var compactionOffForTest bool

func (n *Session) compactIfWorthIt() {
	if compactionOffForTest {
		return
	}
	const floor = 1 << 20
	st, err := os.Stat(n.segPath)
	if err != nil {
		return
	}
	threshold := st.Size() / 4
	if threshold < floor {
		threshold = floor
	}
	did, cerr := n.CompactIfLargerThan(threshold)
	if !did || cerr != nil {
		return
	}
	// The file this session extends was rewritten, so re-read its identity
	// before appending. Otherwise every later delta header names the old
	// file ("segment delta was built for counter 2 pages 1; this file is
	// counter 24971"), replaySegDelta refuses it, and a reopen loses every
	// row committed after the compaction (TestCompactionKeepsTheFastPathFed).
	if rerr := n.refreshPairIdentity(); rerr != nil {
		return
	}
	n.loadedCtr, n.loadedPages = n.endCtr, n.endPages
	// ...and this session must not treat its OWN compaction as another
	// connection's commit: RefreshIfStale compares the file's stat pair, which a
	// compaction moves, and reopening would throw away a view that is still
	// correct -- compaction preserves content, only the bytes moved.
	n.seenStat = n.pairStamp()
	n.rebase(openSegmentsUnlocked) // the commit's write lock is still held
}

// ConnState is sqlite3_changes / sqlite3_total_changes /
// sqlite3_last_insert_rowid, and which of the last two a virtual-table write has
// put out of reach.
func (n *Session) ConnState() (changes, totalChanges, lastInsertRowid int64, totalChangesOpaque, lastRowidOpaque bool) {
	return n.DB.ConnState()
}

// SetConnState restores what ConnState reported -- for a driver that has to seed
// a session, and for tests.
func (n *Session) SetConnState(changes, totalChanges, lastInsertRowid int64, totalChangesOpaque, lastRowidOpaque bool) {
	n.DB.SetConnState(changes, totalChanges, lastInsertRowid, totalChangesOpaque, lastRowidOpaque)
}

// LastInsertRowid is sqlite3_last_insert_rowid() for this session.
func (n *Session) LastInsertRowid() int64 { return n.DB.LastInsertRowid() }

// Path is the segment file this session writes.
func (n *Session) Path() string { return n.segPath }

// OpenOrCreate opens a write session on dbPath, creating an empty
// database there when the file does not exist -- which is what a database/sql
// driver's Open has to do (sql.Open on a path that is not there yet is an empty
// database, not an error).
//
// A file that IS there but is not one of this engine's databases is an ERROR,
// never an implicit conversion; converting is an explicit step.
func OpenOrCreate(dbPath string) (*Session, error) {
	return OpenOrCreateWait(dbPath, BusyTimeout)
}

// OpenOrCreateWait is OpenOrCreate waiting at most wait on another connection's
// lock -- see OpenWriteWait.
func OpenOrCreateWait(dbPath string, wait time.Duration) (*Session, error) {
	fi, err := os.Stat(dbPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return Create(dbPath)
	}
	if fi.Size() == 0 {
		// A ZERO-LENGTH file is an empty database, exactly as it is for real
		// SQLite -- and it is what a ":memory:" DSN leaves behind
		// (driver/memdb.go's os.CreateTemp) and what a pre-touched path is. Read
		// as a segment file it is "not a segment file", which is a confusing
		// error for the most ordinary case there is.
		return Create(dbPath)
	}
	return OpenWriteWait(dbPath, wait)
}

// ---- FRESHNESS: a held session has to notice another connection's commits ----
//
// A held session keeps the catalog and loaded rows, which makes staleness
// possible: two connections on one file each holding a session did not see each
// other (replication's oplog writer and reader disagreed). So every statement
// checks first, and a session whose file moved is rebuilt: dropped and reopened,
// re-reading the catalog and reloading rows lazily. Rebuilding rather than
// reconciling is deliberate (the file may have gained or dropped tables, or been
// compacted; see engine/session.go). A session inside a transaction is never
// refreshed (isolation).

// fileStamp is a file's size and modification time, or the zero value when it
// does not exist.
type fileStamp struct {
	size  int64
	mtime int64
	there bool
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano(), there: true}
}

// pairStamp is the segment file's stamp and its delta's.
func (n *Session) pairStamp() [2]fileStamp {
	if n.deltaFor != n.segPath || n.deltaPath == "" {
		n.deltaPath, n.deltaFor = segDeltaPath(n.segPath), n.segPath
	}
	return [2]fileStamp{stampOf(n.segPath), stampOf(n.deltaPath)}
}

// RefreshIfStale rebuilds this session when the file it is reading has moved
// under it. It reports whether a rebuild happened.
//
// TWO CHECKS, cheapest first. A stat of the segment file and its delta answers
// "has anything touched these at all", which is two syscalls and is the answer
// almost every time. Only when that says yes does it read the pair's committed
// state, which replays the delta.
func (n *Session) RefreshIfStale() (bool, error) {
	if n.closed || n.DB == nil {
		return false, nil
	}
	if n.poisoned {
		// Ahead of BOTH guards below, deliberately: a poisoned session's pending
		// changes are exactly what must NOT survive, and a transaction it may still
		// think is open ended when its commit ran.
		n.poisoned = false
		return true, n.reopen(true)
	}
	if n.InTransaction() {
		return false, nil // isolation: a transaction keeps the state it began with
	}
	if (n.modeHeld && n.lockExclusive) || n.txnExclusive {
		// Nobody else can have committed while this session holds EXCLUSIVE, and
		// SegmentFileState's own SHARED lock would block against it (OFD locks
		// conflict across descriptors, even this process's own).
		return false, nil
	}
	if n.HasUncommittedChanges() {
		// Uncommitted work of our own outranks freshness: dropping it would lose a
		// write, and the caller has not asked to commit yet.
		return false, nil
	}
	now := n.pairStamp()
	if now == n.seenStat {
		return false, nil // nothing has touched either file
	}
	var ctr, pages uint32
	err := withSegmentReadLockWait(n.segPath, n.busyTimeout(), func() error {
		var serr error
		ctr, pages, serr = segmentFileStateUnlocked(n.segPath)
		return serr
	})
	if err != nil {
		return false, err
	}
	if ctr == n.loadedCtr && pages == n.loadedPages {
		// Touched but not MOVED -- our own append, or a stat granularity artefact.
		// Record the stamp so the cheap check answers next time.
		n.seenStat = now
		return false, nil
	}
	return true, n.reopen(false)
}

// adoptConnSettingsFrom carries a connection's PRAGMA settings from old (the DB
// a refresh replaces) onto db, since C keeps them on the connection, not the
// file; otherwise another connection's commit reset e.g. foreign_keys. The
// driver re-stamps its own copies before each write (openWriteOrCreatePath);
// this list is the same, for engine API callers. Left out: deferred-FK counters
// (transaction state; no refresh mid-transaction), the schema-corruption verdict
// (recomputed), the TEMP database (adoptTempFrom), and main's journal mode
// (its WAL half lives in the file).
func (db *DB) adoptConnSettingsFrom(old *DB) {
	db.fkEnforce, db.deferFKs = old.fkEnforce, old.deferFKs
	db.caseSensitiveLike = old.caseSensitiveLike
	db.writableSchema = old.writableSchema
	db.recursiveTriggers = old.recursiveTriggers
	db.untrustedSchema = old.untrustedSchema
	db.rtreeConns = old.rtreeConns
	db.queryOnly = old.queryOnly
	db.writeLock = old.writeLock
	db.maxSize = old.maxSize
	db.pragmaState = old.pragmaState
	db.noAutoIndex = old.noAutoIndex
	db.fullColumnNames, db.shortColumnNamesOff = old.fullColumnNames, old.shortColumnNamesOff
	db.lockingDefault, db.lockingMain = old.lockingDefault, old.lockingMain
	db.fts3AutomergeCache = old.fts3AutomergeCache
	db.walAutoCheckpoint = old.walAutoCheckpoint
	db.autoVacuumPendingPlus1 = old.autoVacuumPendingPlus1
	db.secureDelete = old.secureDelete
	db.tempJournalMode, db.tempStore = old.tempJournalMode, old.tempStore
	db.inMemory = old.inMemory
}

// reopen rebuilds the session's read side, catalog and row stores from the file
// as it stands now, keeping this Session's identity so a caller's handle stays
// valid.
//
// afterFailedCommit rebuilds the TEMP database from its file too, rather than
// carrying the connection's temp objects over as they stand: a commit that
// failed ended the transaction in memory, temp's half included, and the file --
// which only a successful commit writes (commitLocked) -- is the state from
// before it.
// ReopenedHookForTest runs between a reopen's read of the file and its stamp,
// where another connection's commit is the race the stamp order closes.
var ReopenedHookForTest func()

func (n *Session) reopen(afterFailedCommit bool) error {
	// THE STAMP BEFORE THE STATE. The cheap freshness check trusts seenStat to
	// describe a file no newer than what this session loaded; stamped AFTER the
	// open, a commit landing in between was in the stamp and not in the rows,
	// and the session served the old state from then on -- every later check
	// saw "nothing touched" (RefreshIfStale's own stamp is taken first too).
	stamp := n.pairStamp()
	fresh, err := OpenWriteWait(n.segPath, n.busyTimeout())
	if err != nil {
		return err
	}
	if ReopenedHookForTest != nil {
		ReopenedHookForTest()
	}
	// Connection state is the CALLER's, not the file's, so it survives the rebuild
	// -- changes(), total_changes() and last_insert_rowid() are per connection and
	// another connection's commit must not reset them.
	ch, tot, last, totOpaque, lastOpaque := n.DB.ConnState()
	fresh.DB.SetConnState(ch, tot, last, totOpaque, lastOpaque)
	// ...and so is the TEMP database, which belongs to the connection and not to
	// the file another connection just moved. See adoptTempFrom.
	if afterFailedCommit {
		fresh.adoptTempCommittedFrom(n.DB)
	} else {
		fresh.adoptTempFrom(n.DB)
	}
	// ...and every PRAGMA setting the connection made. See adoptConnSettingsFrom.
	fresh.adoptConnSettingsFrom(n.DB)
	// ...and the statistics this connection loaded, unless the schema moved:
	// C reloads its schema -- and sqlite_stat1 with it -- only when another
	// connection changed the schema cookie, so an ANALYZE elsewhere that merely
	// rewrote the rows changes no plan here.
	if fresh.schemaCookie == n.schemaCookie && n.stat1Loaded {
		fresh.stat1 = n.stat1
	}

	old := n.src
	n.DB, n.src = fresh.DB, fresh.src
	n.baseCtr, n.basePages = fresh.baseCtr, fresh.basePages
	n.endCtr, n.endPages = fresh.endCtr, fresh.endPages
	n.loadedCtr, n.loadedPages = fresh.loadedCtr, fresh.loadedPages
	n.tableIndex, n.ipkOf = fresh.tableIndex, fresh.ipkOf
	n.appendAt = SegDeltaAppendState{}
	n.catalogAtLastWrite = fresh.catalogAtLastWrite
	n.seenStat = stamp
	if old != nil {
		old.Close()
	}
	return nil
}

// committedColumns is the COMMITTED-state read handle the columnar fast paths get
// their blocks from, rebuilt on the first call after a commit marked it stale.
//
// nil is a complete answer, not a failure: the fast paths then find no blocks and
// decline, and the scan answers from the live row store. So a file that cannot be
// re-opened costs speed and never correctness.
func (db *DB) committedColumns() *ReadOnlyPager {
	if db.segColumns != nil && !db.segColumnsStale {
		return db.segColumns
	}
	p, err := openSegmentsUnlocked(db.path)
	if err != nil {
		// Leave whatever is cached alone rather than dropping it: a stale handle
		// is never READ (the rowsWrittenSinceCommit gate is what admits a table),
		// and re-trying the open on the next query is cheaper than declining
		// forever.
		return nil
	}
	if db.segColumns != nil {
		db.segColumns.Close()
	}
	db.segColumns, db.segColumnsStale = p, false
	return p
}

// noteCommittedColumnsStale marks the columnar handle for a rebuild. Called by
// every commit, of either kind, because either kind moves what is committed.
func (db *DB) noteCommittedColumnsStale() {
	db.segColumnsStale = true
	// ...and the catalog-corruption verdict, whose key this is the second half of.
	db.schemaCorruptGen++
}

// cachedSchemaSegment renders the catalog once per schema rather than per
// statement (it was 67% of a held connection's per-statement pager cost). Keyed
// on the catalog itself, never a generation counter: a savepoint rollback once
// restored a schema without moving the guarding counters and "SELECT *" got the
// wrong table's columns (savepoint.test). Segments are immutable, so sharing is
// safe. The writable-schema overlay does not come through here.
func (db *DB) cachedSchemaSegment(rows []SchemaRow) (*segment, error) {
	key := schemaRowsFingerprint(rows)
	if db.schemaSeg != nil && db.schemaSegKey == key {
		return db.schemaSeg, nil
	}
	seg, err := buildSchemaSegment(rows)
	if err != nil {
		return nil, err
	}
	db.schemaSeg, db.schemaSegKey = seg, key
	return seg, nil
}

// buildSegSource is the read source a statement sees: the live row store behind
// every table, the committed column blocks beside it, and the catalog rendered
// as sqlite_master. Split out of segmentPager so it can be CACHED -- see
// cachedSegSource.
func (db *DB) buildSegSource(rows []SchemaRow, cols *ReadOnlyPager, freeze bool) (*segSource, error) {
	src := &segSource{
		byRoot:  map[uint32][]*segment{},
		tableOf: map[uint32]int{},
		live:    map[uint32]*rowStore{},
		session: db,
		frozen:  freeze,
		// The rows every table this session has not loaded still has; see
		// segSource.frozen.
		frozenSegs: db.segments,
	}
	// The live row store behind each table.
	for _, t := range db.tables {
		// TEMP tables INCLUDED, unlike the file rewrite which must exclude them:
		// a temp table is session-private and never persisted, but it is very much
		// readable -- leaving it out made "SELECT ... FROM temp.t" report "no such
		// table" for a table the same session had just created.
		live := t.rows
		if freeze {
			live = t.rows.clone(t)
		}
		if !t.loaded {
			if src.unloaded == nil {
				src.unloaded = map[uint32]bool{}
			}
			src.unloaded[t.rootPage] = true
		} else {
			src.live[t.rootPage] = live
		}
		// ...and the committed column blocks beside it, for a table this
		// session has not written since its last commit, so the columnar fast
		// paths (count, sum, group, order) have blocks to read for a held
		// connection's reads ("count(*) WHERE v < ?" went from 11.67ms to tens of
		// microseconds; C: 2.16ms). Gated on rowsWrittenSinceCommit, cleared by
		// every commit (clearWrittenSinceCommit) and set by every row write:
		// false means file plus delta are the table's rows; true leaves the
		// table out and the scan reads the live store. live still wins for row
		// reads (scanSegments), so this only adds a capability.
		if !t.rowsWrittenSinceCommit && cols != nil && cols.segs != nil {
			if csegs, have := cols.segs.byRoot[t.rootPage]; have {
				src.byRoot[t.rootPage] = csegs
				if ti, known := cols.segs.tableOf[t.rootPage]; known {
					src.tableOf[t.rootPage] = ti
				}
			}
		}
	}
	// The committed log, so the fast paths' delta merge has something to correct
	// against (segment_delta_merge.go). Only meaningful alongside byRoot above.
	if cols != nil && cols.segs != nil && len(src.tableOf) > 0 {
		src.delta = cols.segs.delta
	}
	// sqlite_master as a table, mirroring the rows above -- see segment_open.go --
	// but MAIN's rows only.
	//
	// A TEMP object is not in main's sqlite_schema in C SQLite; it is in the
	// temp database's own (sqlite_temp_schema, answered by the temp reader --
	// Session.TempPager). Putting every row in this one segment made "SELECT type,
	// name FROM sqlite_schema" list a connection's TEMP view and TEMP trigger as if
	// they were main's, which is a wrong answer about what the database contains.
	//
	// p.schemaRows above keeps them, with Temp set: that list is what the RESOLVER
	// walks, and it has to see every object this session can name.
	mainRows := make([]SchemaRow, 0, len(rows))
	for _, r := range rows {
		if !r.Temp {
			mainRows = append(mainRows, r)
		}
	}
	src.byRoot[schemaRootPage] = []*segment{}
	// AND THE OVERLAY, when a direct sqlite_schema write is outstanding: the rows
	// a SCAN of sqlite_master reads are the EDITED ones, exactly as C's are once
	// the statement has written them into the b-tree. p.schemaRows above keeps the
	// PRE-edit list on purpose, so table resolution keeps running on the schema
	// this session still holds -- the same split stampPager's
	// writableSchemaCleanRows makes (writer.go). Without it
	// "SELECT name FROM sqlite_master WHERE sql LIKE '%collate%'" answered zero
	// rows right after the UPDATE that put that text there (parser1.test#0).
	//
	// From the overlay's RAW VALUES rather than through SchemaRow, which cannot
	// carry a NULL in type/name/tbl_name -- see buildSchemaSegmentFromValues.
	if db.writableSchemaEditsActive() {
		crows := db.wsCatalogWithOverlay()
		rowids := make([]uint64, 0, len(crows))
		stored := make([][]Value, 0, len(crows))
		// Each row under its own rowid, and a row the session INSERTed (rowid 0)
		// under max+1 -- OP_NewRowid on the schema b-tree, as C gives it.
		var top int64
		for _, r := range crows {
			rid := r.rowid
			if rid <= top {
				rid = top + 1
			}
			top = rid
			rowids = append(rowids, uint64(rid))
			stored = append(stored, r.vals)
		}
		if len(stored) > 0 {
			seg, serr := buildSchemaSegmentFromValues(rowids, stored)
			if serr != nil {
				return nil, serr
			}
			src.byRoot[schemaRootPage] = []*segment{seg}
		}
	} else if len(mainRows) > 0 {
		seg, serr := db.cachedSchemaSegment(mainRows)
		if serr != nil {
			return nil, serr
		}
		src.byRoot[schemaRootPage] = []*segment{seg}
	}
	return src, nil
}

// cachedSegSource builds the segSource once per distinct input rather than per
// statement (its rootPage-keyed maps cost 851 bytes and 8 allocations each). A
// segSource is immutable once built (byRoot, tableOf and live are written only in
// constructors), so statements can share one; liveRows loading a table on first
// touch drops the source from this cache.
//
// The key is the input set: each table's root page, live store identity and
// written-since-commit flag, the committed-block handle, whether a sqlite_schema
// edit is outstanding, and the catalog fingerprint. A counter-keyed cache once
// answered savepoint.test's "SELECT *" with the wrong column count.
//
// Not for freeze: a frozen source clones row stores on purpose.
func (db *DB) cachedSegSource(rows []SchemaRow, cols *ReadOnlyPager, freeze bool, schemaFP uint64) (*segSource, error) {
	if freeze {
		return db.buildSegSource(rows, cols, freeze)
	}
	const prime64 = 1099511628211
	h := schemaFP
	mix := func(v uint64) { h ^= v; h *= prime64 }
	for _, t := range db.tables {
		mix(uint64(t.rootPage))
		mix(uint64(uintptr(unsafe.Pointer(t.rows))))
		if t.rowsWrittenSinceCommit {
			mix(1)
		}
	}
	mix(uint64(uintptr(unsafe.Pointer(cols))))
	if cols != nil {
		mix(uint64(uintptr(unsafe.Pointer(cols.segs))))
	}
	if db.writableSchemaEditsActive() {
		// The overlay's CONTENT is not hashed -- an outstanding direct
		// sqlite_schema edit simply refuses the cache, which is the shape this
		// takes maybe once in a corpus.
		return db.buildSegSource(rows, cols, freeze)
	}
	if db.segSrcCache != nil && db.segSrcKey == h {
		return db.segSrcCache, nil
	}
	src, err := db.buildSegSource(rows, cols, freeze)
	if err != nil {
		return nil, err
	}
	db.segSrcCache, db.segSrcKey = src, h
	return src, nil
}
