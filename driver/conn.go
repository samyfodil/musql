package driver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samyfodil/musql/engine"
)

// Conn is one database/sql connection onto a single musql database file
// (path). database/sql guarantees a single goroutine uses a given Conn at a
// time, so no locking is needed inside Conn itself.
//
// ---- the execution + transaction model ----
//
// The connection holds ONE engine session for its whole life (session.go):
// autocommit is Exec then CommitIfAutocommit on it; an explicit transaction
// (tx != nil, from BeginTx or a SQL BEGIN) is the same Exec with the commit
// deferred to Commit, or Discard on Rollback, which leaves the file exactly as
// it was before BEGIN; and a SELECT reads the session's own rows (ReadPager),
// which is what gives a transaction read-your-writes. An ATTACHed database's
// file gets a held session of its own (ndbs, sessionFor).
type Conn struct {
	path string
	tx   *engine.DB // non-nil while a transaction (BeginTx) is open on this Conn

	// ndb is this connection's HELD segment session -- one for the connection's
	// whole life, which is what this format allows and the SQLite format
	// did not. See driver/session.go for the model and what it declines.
	ndb *engine.Session

	// refreshedThisStmt records that the held session's freshness check has
	// already run for the statement in flight. A statement acquires the session
	// twice -- once at entry, once again just before the read pager is built --
	// and RefreshIfStale is two stat syscalls each time, ~12 allocations and a
	// microsecond per statement for the pair.
	//
	// Asking twice inside one statement buys NO guarantee the first ask did not
	// already give: another process can commit one nanosecond after either check,
	// so the answer is only ever "as of a boundary". Once per boundary is the
	// semantics; the second is work with nothing behind it. Cleared at the top of
	// execArgs and queryArgs, which ARE the boundaries.
	refreshedThisStmt bool

	// txSavepoint is the name of the SAVEPOINT that implicitly STARTED the
	// held transaction, or "" when a BEGIN/BeginTx did. A SAVEPOINT issued in
	// autocommit starts a transaction in C SQLite, and the RELEASE of that
	// same outermost savepoint COMMITS it -- verified against 3.53.3, where a
	// COMMIT issued straight after such a RELEASE reports "cannot commit - no
	// transaction is active". Matching the name is what tells that release
	// from an inner one, which merely releases.
	txSavepoint string

	// lastInsertRowid mirrors sqlite3_last_insert_rowid() at the CONNECTION
	// level, exactly like C SQLite (and mattn/go-sqlite3, whose
	// Result.LastInsertId reads that same connection-level counter regardless
	// of statement kind). The engine keeps its own per session, and a
	// connection has more than one -- main's, each ATTACHed file's -- so this
	// is the connection's value: each session is SEEDED with it before a
	// statement and noteConnState reads it back after, so the engine's own
	// "only a genuine row store moves it" rule carries across sessions
	// unchanged (UPDATE/DELETE/CREATE/DROP leave it unchanged). It is also
	// what SQL's own last_insert_rowid() reads on this connection; see
	// engine/conn_state.go.
	lastInsertRowid int64

	// changes / totalChanges are the rest of the same connection-level state:
	// sqlite3_changes() and sqlite3_total_changes(), carried across sessions
	// for the same reason lastInsertRowid is, so "SELECT changes()" reports the
	// previous statement's count whichever database it wrote. totalChangesOpaque / lastRowidOpaque carry which
	// of the last two a virtual-table write has put out of reach; see
	// engine/conn_state.go.
	changes            int64
	totalChanges       int64
	totalChangesOpaque bool
	lastRowidOpaque    bool

	// rowsInserted is NOT connection state, unlike the four above: it is the
	// statement that just ran, read back out of its session (noteConnState) so
	// countChangesRow can answer "PRAGMA count_changes"'s "rows inserted" with
	// insert.c's regRowCount rather than with sqlite3_changes(), which counts an
	// upsert's DO UPDATE branch and regRowCount does not. Nothing seeds it back
	// into a session.
	rowsInserted int64

	// attached is this connection's ATTACHed database set (ATTACH/DETACH,
	// attach.go), in attach order. Empty in the overwhelmingly common single-
	// database case, in which every statement runs against path exactly as
	// before (route's fast path). When non-empty, each statement is routed to
	// the single database it references; see attach.go's package comment.
	attached []*attachedDB

	// ndbs is the held write session for each ATTACHed database file, keyed by
	// path; main's own is c.ndb. See sessionFor.
	ndbs map[string]*engine.Session

	// lockingDefault and lockingMain are "PRAGMA locking_mode"'s two pieces of
	// state, exactly as execLockingMode (engine/pragma.go) names them: the
	// connection-wide default that only the UNQUALIFIED setter writes, and
	// main's own mode. The lock they describe lives on the held session
	// (engine's Session.HoldLockingMode, taken in refreshOnce).
	lockingDefault, lockingMain bool
	// rowidLo, rowidHi are the rowid allocation range; see SetRowidRange.
	rowidLo, rowidHi uint64
	rowidFloor       func(table string) uint64
	// uncapturedWriter is AllowUncapturedWrites'.
	uncapturedWriter bool
	// stmtText is the statement being run, for refreshOnce's locking-mode rule:
	// only a statement that opens main's file takes the lock.
	stmtText string

	// lockingEnteredInWAL records whether the file was ALREADY in WAL mode
	// when this connection entered exclusive locking mode, which is what
	// decides whether it can leave again. C SQLite's wal-index is a heap
	// allocation when the Wal is opened while the pager is ALREADY exclusive,
	// and sqlite3PagerLockingMode then REFUSES to go back to normal
	// (execLockingMode's walIndexHeap arm, engine/pragma.go) -- measured:
	// "journal_mode=wal; locking_mode=exclusive; locking_mode=normal" answers
	// normal, while "locking_mode=exclusive; journal_mode=wal;
	// locking_mode=normal" answers EXCLUSIVE and leaves main exclusive with
	// the connection default already normal.
	lockingEnteredInWAL bool

	// schemaCorruptObj/schemaCorruptDetail are corruptSchema's latch as
	// CONNECTION state (engine's DB.SchemaCorrupt): a "PRAGMA
	// writable_schema=RESET" over a catalog row the schema load refuses fails
	// that load, and in C SQLite every later statement on the connection
	// answers the same "malformed database schema (%s)" until "PRAGMA
	// writable_schema=ON" lifts it. It is connection state, so it rides
	// here rather than on a session.
	schemaCorruptObj, schemaCorruptDetail string

	// plainStmts caches, per statement TEXT, whether it is ordinary DML --
	// that is, none of ATTACH/DETACH, a transaction control statement, a
	// "PRAGMA page_size" assignment, or a RETURNING statement. Each of those
	// four questions is answered by its own parse, so without this a bulk
	// INSERT re-parsed its statement four times per row just to be told "no"
	// each time. Keyed by text because that is what the answers depend on;
	// bounded because a connection issues finitely many DISTINCT statements
	// (database/sql prepares and reuses them). See plainDML.
	plainStmts map[string]bool

	// timeDeclType and loc are the DSN opt-in for mattn/go-sqlite3's one type
	// coercion: "?_time_decltype=1" makes a result column whose DECLARED type is
	// "timestamp"/"datetime"/"date" arrive as a Go time.Time rather than as the
	// value SQLite stored, and "?_loc=<name>" (or "auto") places it. Both
	// default off/nil, which is this driver's own behaviour and
	// modernc.org/sqlite's. See decltype_time.go for the whole rule and for why
	// it is not the default.
	timeDeclType bool
	loc          *time.Location

	// reportDeclTypes: see ReportDeclTypes.
	reportDeclTypes bool

	// hooks is the change-capture seam (capture.go), nil unless a connection
	// hook installed one. Non-nil turns on the engine's row-change recording
	// for every session this Conn opens.
	hooks *CaptureHooks

	// foreignKeys is this connection's "PRAGMA foreign_keys". Connection
	// settings like it live on the Conn, not on a session: a session is rebuilt
	// (another connection's commit, an attached database's own session), and
	// every session and read pager this Conn opens (openWriteOrCreatePath,
	// readPagerStamped) is told about them, so the setting spans statements as C's
	// connection flag does. foreignKeysPragma reads and writes it.
	foreignKeys bool

	// deferFKs is this connection's "PRAGMA defer_foreign_keys". It must
	// outlive the pragma statement and be in force in a transaction a later
	// statement opens ("PRAGMA defer_foreign_keys=ON; BEGIN; DELETE ...",
	// fkey6.test 1.8). openWriteOrCreatePath puts it on every session,
	// stampConnState on every read pager, and noteConnState reads it back so
	// the engine's autocommit lifetime rules decide transitions
	// (deferFKAutocommitGuard/Apply). C clears it when a statement's Vdbe
	// would set bIsReader in autocommit: "...; SELECT 1" keeps it, "...;
	// SELECT count(*) FROM t" does not.
	deferFKs bool

	// fts3AutomergeCache carries an fts3/fts4 table's resolved automerge=N
	// across this connection's engine sessions. C's p->nAutoincrmerge cache
	// (fts3Int.h:271) survives a table wipe (fts3DeleteAll clears %_stat, not
	// the field), so a table emptied after configuring automerge keeps
	// enforcing it (fts3ReadAutoincrmerge, engine/fts3_automerge.go).
	//
	// It is a map shared by reference (allocated lazily, passed to
	// engine.SetFts3AutomergeCache), so sessions write into it directly. Keyed
	// by database file path (main or attached), then table name, since a
	// session resolves names only within its own database.
	fts3AutomergeCache map[string]map[string]int64

	// caseSensitiveLike is this connection's "PRAGMA case_sensitive_like"
	// setting (see foreignKeys): with it ON, LIKE stops folding ASCII case.
	// Both forms are answered from caseSensitiveLikePragma. The getter is a
	// no-op in C (no rows), so it does not read this field.
	caseSensitiveLike bool

	// recursiveTriggers is this connection's "PRAGMA recursive_triggers"
	// setting (see foreignKeys). Unlike foreignKeys its setter takes effect
	// inside a transaction (engine's SetRecursiveTriggers).
	recursiveTriggers bool

	// untrustedSchema is this connection's "PRAGMA trusted_schema" setting,
	// stored inverted so the zero value is SQLite's ON default (see
	// foreignKeys); its setter takes effect inside a transaction. See engine's
	// trusted_schema.go.
	untrustedSchema bool

	// rtreeConns is this connection's r-tree registry (engine/rtree_conn.go):
	// which r-trees it created, which decides what a damaged one answers.
	// Every session and read pager this Conn opens shares it.
	rtreeConns *engine.RtreeConnections

	// queryOnly is this connection's "PRAGMA query_only" setting (see
	// foreignKeys); its setter takes effect inside a transaction.
	queryOnly bool

	// maxSize is this connection's "PRAGMA max_size" (maxSizePragma), stamped
	// on every session of its main database for queryOnly's reason.
	maxSize int64

	// legacyAlterTable is this connection's "PRAGMA legacy_alter_table"
	// setting, here for exactly queryOnly's reason and with the same
	// transaction rule: verified against mattn/go-sqlite3 3.53.3, it is still 1
	// after a BEGIN, after a ROLLBACK and after an intervening CREATE TABLE.
	// This is what made the pragma unservable through the driver before the
	// flag existed at all -- the session that ran the setter died with the
	// statement, so the ALTER that the flag changes never saw it. See engine's
	// PragmaConnState.LegacyAlterTable for what it changes.
	legacyAlterTable bool

	// fullColumnNames / shortColumnNamesOff are "PRAGMA full_column_names" /
	// "PRAGMA short_column_names" (SQLITE_FullColNames / SQLITE_ShortColNames),
	// stored so the zero value is SQLite's default (ShortColNames on), hence the
	// inverted second one. Only generateColumnNames (select.c) reads them, naming
	// a later statement's columns, which has no decline channel
	// (tail_r31_colnames_test.go). A setter inside a transaction takes effect and
	// survives ROLLBACK (PragTyp_FLAG masks only SQLITE_ForeignKeys while
	// autoCommit is 0). A qualifier is ignored: db->flags is per connection.
	fullColumnNames     bool
	shortColumnNamesOff bool

	// ignoreCheckConstraints is this connection's "PRAGMA
	// ignore_check_constraints", here for exactly legacyAlterTable's reason and
	// with the same transaction rule (settable inside a BEGIN; a ROLLBACK does
	// not restore it -- verified against mattn/go-sqlite3 3.53.3). It is pushed
	// into every write session AND onto every read pager, because the flag has
	// two enforcement sites and one of them -- integrity_check's CHECK
	// verification -- is answered on the read side. See engine's
	// SetIgnoreCheckConstraints.
	ignoreCheckConstraints bool

	// noAutomaticIndex is "PRAGMA automatic_index", inverted so the zero value is
	// SQLite's ON. Its effect is on later joins (loop order and inner row order,
	// where_plan_gate.go), so it is pushed into every write session and onto every
	// read pager, where SELECTs are compiled. A setter inside a transaction takes
	// effect and survives ROLLBACK (a db->flags bit).
	noAutomaticIndex bool

	// tuning holds "PRAGMA synchronous / cache_size / journal_size_limit /
	// mmap_size" per database (keyed schema.name), here for the same reason
	// queryOnly is. These configure C SQLite's dealings with the operating
	// system and nothing this engine does, so the value you set is their whole
	// observable behavior -- see engine's PragmaTuningResult for the measured
	// parse, clamp and result-shape rules, which differ per pragma.
	pragmaState *engine.PragmaConnState

	// textEncoding is the encoding "PRAGMA encoding" asked for while this
	// connection's database was still empty, applied by the session that
	// creates the file. A session opening an existing file ignores it; the
	// file decides (engine's OpenWrite).
	textEncoding engine.TextEncoding

	// walAutoCheckpoint is this connection's "PRAGMA wal_autocheckpoint"
	// threshold, biased by one so the zero value means "never set" (0 itself is
	// meaningful: it disables automatic checkpointing). Pushed into every
	// session and read snapshot.
	walAutoCheckpointPlus1 int

	// autoVacuumPendingPlus1 is the mode a "PRAGMA auto_vacuum = MODE" asked for
	// C SQLite REMEMBERED without applying (engine's execAutoVacuum, which
	// owns the rule). It lives here because that memory is per CONNECTION --
	// verified: a second connection's VACUUM on the same file does not convert
	// it. execArgs pushes it into every session and reads it back.
	autoVacuumPendingPlus1 int

	// secureDelete is this connection's "PRAGMA secure_delete" setting: 0 (off),
	// 1 (on) or 2 ("fast"). Connection state, not file state -- verified: a
	// second connection on the same file reads back the default 0. See
	// secureDeletePragma, and engine's SecureDeleteResult for the rule.
	secureDelete int

	// journalMode is this connection's ROLLBACK journal mode: "" (the default,
	// delete), "truncate", "persist" or "memory". Connection state for the same
	// verified reason secureDelete is -- C SQLite records it nowhere in the
	// file, so a second connection to a database this one put in truncate mode
	// reads back "delete", while "=wal" does persist. Pushed into every session
	// and read snapshot. See engine's DB.journalModeName / DB.SetJournalMode.
	journalMode string

	// tempStore is this connection's "PRAGMA temp_store" value (0 default,
	// 1 file, 2 memory). Carried here for journalMode's reason: C SQLite
	// keeps db->temp_store per CONNECTION and nowhere in any file, and a
	// connection here has a session per database file and rebuilds one when
	// another connection commits -- so the connection, not a session, is where
	// the value lives. See engine.TempStore.
	tempStore uint8

	// tempJournalMode is the TEMP database's own journal mode, which real
	// SQLite keeps on the connection separately from main's (engine's
	// tempJournalModeResult).
	tempJournalMode string

	// writableSchema is this connection's "PRAGMA writable_schema" flag (see
	// foreignKeys), pushed into every session and read snapshot. It is neither
	// reset by COMMIT/ROLLBACK nor shared with other connections.
	writableSchema bool

	// tempOpened is "has this connection opened its TEMP database" -- real
	// SQLite's db->aDb[1].pBt != 0, which "PRAGMA database_list" reports by
	// listing the temp row at all. Separate from temp because a statement
	// can OPEN the temp database without putting anything in it ("SELECT *
	// FROM sqlite_temp_master" is an opener), and once open it stays open.
	tempOpened bool

	// temp is this connection's TEMP database (engine/temp_store.go), in
	// memory and on the engine's own format, handed out by the first statement
	// that makes a temp object and dropped by Close; C's temp database belongs
	// to the connection.
	temp *engine.TempDatabase

	// memPrivate marks path as the private temp file backing a ":memory:"
	// database for this connection alone (memdb.go): removed by Close.
	// memShared, when non-empty, is the shared in-memory database name this
	// connection holds a reference on (released by Close).
	memPrivate bool
	memShared  string
}

// stampReadPager carries every piece of CONNECTION state a read-only pager
// answers from onto p: the pragma flags whose getters read them, and the
// counters stampConnState carries. Every path that opens a read pager needs all
// of it -- a path that stamped only some of it answered "PRAGMA journal_mode"
// (and foreign_keys, case_sensitive_like, secure_delete, writable_schema) out
// of a freshly-opened file's defaults instead of out of this connection.
func (c *Conn) stampReadPager(pager *engine.ReadOnlyPager, path string) {
	pager.SetInMemory(c.memBacked(path))            // see memdb.go's memBacked
	pager.SetForeignKeys(c.foreignKeys)             // see Conn.foreignKeys
	pager.SetDeferForeignKeys(c.deferFKs)           // see Conn.deferFKs -- the getter reads it
	pager.SetCaseSensitiveLike(c.caseSensitiveLike) // see Conn.caseSensitiveLike
	pager.SetWritableSchema(c.writableSchema)       // see Conn.writableSchema
	pager.SetSecureDelete(c.secureDelete)           // see Conn.secureDelete
	pager.SetJournalMode(c.journalMode)             // see Conn.journalMode
	pager.SetTempJournalMode(c.tempJournalMode)     // see Conn.tempJournalMode
	pager.SetTempStore(c.tempStore)                 // see Conn.tempStore
	// "PRAGMA database_list" reports this connection's databases, so the read
	// side needs to know which file it is on and whether temp is open
	// (engine/pragma_database_list.go).
	pager.SetMainPath(path)
	pager.SetTempOpen(c.tempOpened || c.temp != nil)
	c.stampConnState(pager)
	if c.walAutoCheckpointPlus1 > 0 {
		pager.SetWalAutoCheckpoint(c.walAutoCheckpointPlus1 - 1) // see Conn.walAutoCheckpointPlus1
	}
}

var (
	_ driver.Conn               = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ConnBeginTx        = (*Conn)(nil)
	_ driver.NamedValueChecker  = (*Conn)(nil)
)

// CheckNamedValue converts bound arguments that need no conversion without
// database/sql's reflection (driverArgsConnLocked calls it for every argument;
// the default goes through reflect.ValueOf, 7.5% of a prepared batch INSERT).
// It answers only as the default would: driver.IsValue types pass through, and
// unnamed signed integers and float32 pass through unconverted for
// driverValueToEngine to widen (widening here would allocate). Anything else
// (unsigned, named types, Valuers, pointers) returns ErrSkip.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	switch nv.Value.(type) {
	case nil, int64, float64, bool, string, []byte, time.Time,
		int, int32, int16, int8, float32:
		return nil
	}
	return driver.ErrSkip
}

// Prepare implements driver.Conn (the legacy path; database/sql prefers
// PrepareContext, implemented below, when available).
func (c *Conn) prepareImpl(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext. It parses just
// enough of query (via engine.ParseParamInfo) to recover its bound-parameter
// shape for Stmt.NumInput and named-parameter binding; a genuinely malformed
// SELECT/INSERT/UPDATE/DELETE fails here, at prepare time, exactly as a real
// driver's sqlite3_prepare would. CREATE/DROP and anything else this
// engine's write path doesn't implement are NOT rejected here (ParseParamInfo
// only reports "no parameters" for those, deferring any "unsupported
// statement" error to Exec/Query time) -- see ParseParamInfo's own doc
// comment.
func (c *Conn) prepareContextImpl(ctx context.Context, query string) (driver.Stmt, error) {
	// A ";"-separated SCRIPT has to be recognised BEFORE ParseParamInfo, not
	// after it fails: that function parses one statement and reports "no
	// parameters" for a leading CREATE without ever looking at the trailing
	// text, so "CREATE TABLE u(x); INSERT INTO u VALUES(1)" prepared fine and
	// then died at Exec -- creating nothing and inserting nothing, where real
	// SQLite's exec runs both. mayBeScript keeps that off the hot path: it is a
	// byte scan, and a ";" that turns out to be inside a string or a comment
	// simply splits to one statement and falls through here unchanged.
	if mayBeScript(query) {
		if stmts, serr := engine.SplitStatements(query); serr == nil {
			if len(stmts) > 1 {
				return &Stmt{conn: c, sqlText: query, script: stmts}, nil
			}
			// Exactly one statement, but the text around it held separators --
			// a leading ";", a doubled ";;", a trailing one. C SQLite treats
			// each as an empty statement and runs the rest; parsing the ORIGINAL
			// text here would fail on the stray ";". Use what the splitter
			// recovered.
			if len(stmts) == 1 {
				query = stmts[0]
			}
		}
	}
	info, err := engine.ParseParamInfo(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{conn: c, sqlText: query, info: info, isQuery: isSelectStmt(query)}, nil
}

// mayBeScript reports whether query MIGHT hold more than one statement: a ";"
// with anything other than whitespace and further ";"s after it. Deliberately
// crude -- it cannot tell a separator from a ";" inside a string literal, and
// does not try, because its only job is to decide whether the real (lexing)
// splitter is worth running. A false positive costs one lex; a false negative
// is impossible, since every separator is a literal ";" byte.
func mayBeScript(query string) bool {
	for i := 0; i < len(query); i++ {
		if query[i] != ';' {
			continue
		}
		for j := i + 1; j < len(query); j++ {
			switch query[j] {
			case ' ', '\t', '\r', '\n', ';':
			default:
				return true
			}
		}
		return false
	}
	return false
}

// Close implements driver.Conn. It closes the connection's held sessions, and
// if a transaction is still open on this Conn (the caller never called
// Commit/Rollback, e.g. because it abandoned the *sql.Tx), discards it rather
// than committing it.
func (c *Conn) Close() error {
	// THE HELD SESSIONS -- main's and every ATTACHed database's -- are released
	// here, and nothing did that before: closeSessions' predecessor existed
	// and had no caller, so every connection left its segment mapping and its lock
	// descriptor behind, and its last statements' batch was never committed.
	//
	// Registered FIRST so, being LIFO, it runs LAST: dropTempObjects and
	// cleanupAttached below may still need a session. A held transaction needs no
	// special handling -- c.tx's own discardSession below is live code, not
	// deferred, so it completes before any defer here fires.
	defer c.closeSessions()
	defer c.dropTempObjects() // TEMP objects live only as long as their connection
	defer c.cleanupAttached() // remove any private temp files backing ':memory:' attaches
	if c.memPrivate {
		defer func() {
			os.Remove(c.path)
			os.Remove(c.path + "-journal")
		}()
	}
	if c.memShared != "" {
		defer releaseSharedMem(c.memShared)
	}
	if c.tx != nil {
		db := c.tx
		c.tx, c.txSavepoint = nil, ""
		return c.discardSession(db)
	}
	return nil
}

// Begin implements driver.Conn (the legacy path; database/sql prefers
// BeginTx, implemented below, when available).
func (c *Conn) beginImpl() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx. opts.Isolation/ReadOnly are
// accepted but not enforced (see the package doc comment's "not yet
// supported" list): every transaction is an ordinary read-write session
// backed by one held engine.DB (opened here and kept open until Commit/
// Rollback -- see Conn's own doc comment for the full model).
func (c *Conn) beginTxImpl(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.tx != nil {
		return nil, errors.New("driver: a transaction is already open on this connection")
	}
	db, err := c.openWriteOrCreate()
	if err != nil {
		return nil, err
	}
	// AN ACTUAL "BEGIN" reaches the engine now, which it did not before.
	//
	// The session is permanent and commits per statement unless it knows a
	// transaction is open --
	// so without this, every statement between BEGIN and ROLLBACK committed
	// immediately and the rollback undid nothing (TestCaptureExplicitTxAtomic: a
	// rolled-back INSERT survived).
	//
	// It also gives ROLLBACK something real to do: the engine's snapshot is what
	// reverts rows already applied to the row stores.
	if berr := db.Exec(`BEGIN`); berr != nil {
		return nil, berr
	}
	db.MarkHeldTransaction()
	db.MarkDeferredFKCommitter() // commitHeldSession runs the COMMIT check
	c.tx = db
	return &Tx{conn: c}, nil
}

// openWriteOrCreate opens c.path for writing: engine.Create for a file that
// doesn't exist yet or is empty (0 bytes -- e.g. os.Create'd but never
// written to), engine.OpenWrite for an existing, non-empty database file.
// This is shared by autocommit Exec (see execArgs below) and BeginTx.
func (c *Conn) openWriteOrCreate() (*engine.DB, error) {
	return c.openWriteOrCreatePath(c.path)
}

// openWriteOrCreatePath is openWriteOrCreate against an arbitrary file path (the
// main file, or an ATTACHed database's file when a statement is routed to it --
// see attach.go).
func (c *Conn) openWriteOrCreatePath(path string) (*engine.DB, error) {
	db, err := c.openSessionOrCreateFile(path)
	if err != nil {
		return nil, err
	}
	db.SetInMemory(c.memBacked(path))                              // see memdb.go's memBacked
	db.SetForeignKeys(c.foreignKeys)                               // connection-level; see Conn.foreignKeys
	db.SetDeferForeignKeys(c.deferFKs)                             // likewise; see Conn.deferFKs
	db.SetCaseSensitiveLike(c.caseSensitiveLike)                   // likewise; see Conn.caseSensitiveLike
	db.SetWritableSchema(c.writableSchema)                         // likewise; see Conn.writableSchema
	db.SetRecursiveTriggers(c.recursiveTriggers)                   // likewise; see Conn.recursiveTriggers
	db.SetTrustedSchema(!c.untrustedSchema)                        // likewise; see Conn.untrustedSchema
	db.SetRtreeConnections(c.rtreeConns)                           // likewise; see Conn.rtreeConns
	db.SetQueryOnly(c.queryOnly)                                   // likewise; see Conn.queryOnly
	db.SetWriteLock(c.captureLock(db, path))                       // see Conn.captureLock
	if path == c.path {
		db.SetMaxSize(c.maxSize) // see Conn.maxSizePragma; main only
	}
	db.SetLegacyAlterTable(c.legacyAlterTable)                     // likewise; see Conn.legacyAlterTable
	db.SetIgnoreCheckConstraints(c.ignoreCheckConstraints)
	db.SetAutomaticIndex(!c.noAutomaticIndex)      // likewise; see Conn.noAutomaticIndex
	db.SetFullColumnNames(c.fullColumnNames)       // likewise; see Conn.fullColumnNames
	db.SetShortColumnNames(!c.shortColumnNamesOff) // likewise; see Conn.fullColumnNames
	// "PRAGMA analysis_limit" decides how much of each index an ANALYZE in
	// THIS session walks, and the setter never reaches a session at all (it
	// is answered from Conn state by tuningPragma) -- so without this an
	// ANALYZE would always run unlimited. See engine's pragmaAnalysisLimit.
	db.SetAnalysisLimit(c.pragmaState.AnalysisLimitValue())
	// ...and the tuning values, one of which the WRITE compiler reads
	// (reverse_unordered_selects). See engine's DB.SetPragmaTuningValues.
	db.SetPragmaTuningValues(c.pragmaState)
	// The TEMP database is per CONNECTION, not per statement: this session
	// writes its temp objects into the file the previous statement's session
	// created, and reads them back from it. See engine/temp_store.go.
	// "PRAGMA locking_mode" is per CONNECTION and its LOCK is held across
	// statements on this Session's descriptor, so the mode has to ride along
	// for the session's own getter and its ATTACH rule to see it
	// (Session.HoldLockingMode holds the lock). See Conn.lockingMain.
	if path == c.path {
		db.SetRowidRange(c.rowidLo, c.rowidHi) // see Conn.SetRowidRange
		db.SetRowidFloor(c.rowidFloor)
		db.SetLockingModes(c.lockingDefault, c.lockingMain)
		db.SetSchemaCorrupt(c.schemaCorruptObj, c.schemaCorruptDetail) // see Conn.schemaCorruptObj
	}
	db.SetTempDatabaseOpened(c.tempOpened) // see Conn.tempOpened
	db.SetTempDatabase(c.temp)
	if c.fts3AutomergeCache == nil {
		c.fts3AutomergeCache = map[string]map[string]int64{}
	}
	if c.fts3AutomergeCache[path] == nil {
		c.fts3AutomergeCache[path] = map[string]int64{}
	}
	db.SetFts3AutomergeCache(c.fts3AutomergeCache[path]) // likewise; see Conn.fts3AutomergeCache
	if c.walAutoCheckpointPlus1 > 0 {
		db.SetWalAutoCheckpoint(c.walAutoCheckpointPlus1 - 1) // see Conn.walAutoCheckpointPlus1
	}
	if c.textEncoding != 0 {
		db.SetTextEncoding(c.textEncoding) // no-op once the file exists
	}
	db.SetAutoVacuumPending(c.autoVacuumPendingPlus1) // see Conn.autoVacuumPendingPlus1
	db.SetSecureDelete(c.secureDelete)                // see Conn.secureDelete
	db.SetJournalMode(c.journalMode)                  // see Conn.journalMode
	db.SetTempJournalMode(c.tempJournalMode)          // see Conn.tempJournalMode
	db.SetTempStore(c.tempStore)                      // see Conn.tempStore
	// Seed the session with this CONNECTION's changes()/total_changes()/
	// last_insert_rowid(), which a session other than the one that ran the last
	// statement would otherwise not know; noteConnState reads them back
	// afterwards. See
	// engine/conn_state.go.
	db.SetConnState(c.changes, c.totalChanges, c.lastInsertRowid, c.totalChangesOpaque, c.lastRowidOpaque)
	// MAIN ONLY. An ATTACHed database's session reached the capture hook with
	// its DDL (qualifier stripped) and its rows looking exactly like main's, so a
	// consumer replicating main by name would have written them into main's
	// same-named table on every peer.
	if path == c.path {
		c.enableCapture(db) // no-op unless this Conn has capture hooks (capture.go)
	}
	return db, nil
}

// noteConnState brings this Conn's changes()/total_changes()/
// last_insert_rowid() forward from the session that just ran a statement, so
// the NEXT statement -- which may run on another database's session, or read
// through a pager -- sees what C SQLite's connection-level counters would
// report. The
// counterpart of the SetConnState seeding in openWriteOrCreatePath.
func (c *Conn) noteConnState(db *engine.DB) {
	c.changes, c.totalChanges, c.lastInsertRowid, c.totalChangesOpaque, c.lastRowidOpaque = db.ConnState()
	// ...and the statement's own regRowCount, which is not connection state at
	// all -- see Conn.rowsInserted and countChangesRow.
	c.rowsInserted = db.RowsInserted()
	// ...and corruptSchema's latch as the statement left it: a RESET may have
	// SET it, and "PRAGMA writable_schema=ON" clears it. See
	// Conn.schemaCorruptObj.
	c.schemaCorruptObj, c.schemaCorruptDetail = db.SchemaCorrupt()
	// ...and the TEMP database this statement may have created, which every
	// later statement on this connection has to find (Conn.temp).
	if t := db.TempDatabase(); t != nil {
		c.temp = t
	}
	// ...and whether this statement OPENED the temp database (see
	// Conn.tempOpened): sticky, exactly as C SQLite's aDb[1] is.
	if db.TempDatabaseOpened() {
		c.tempOpened = true
	}
	// ...and "PRAGMA defer_foreign_keys" as the ENGINE left it: this statement
	// may have set it, and a statement that reached a reader/writer Vdbe with
	// autocommit on may have CLEARED it (deferFKAutocommitApply). Reading it
	// back rather than deciding here is what keeps that rule in one place.
	// See Conn.deferFKs.
	c.deferFKs = db.DeferForeignKeys()
}

// stampConnState is noteConnState's read-side mirror: it puts this Conn's
// carried state onto a read-only pager, so a SELECT sees the connection's:
//
//   - change counters, for the connection-state functions;
//   - trusted_schema, since every such pager may resolve a view body
//     (Conn.untrustedSchema);
//   - ignore_check_constraints, since integrity_check is answered here;
//   - automatic_index, read where a SELECT's join order is compiled
//     (markWherePlanEligibility; Conn.noAutomaticIndex);
//   - full_column_names / short_column_names, read where result names are
//     built (expandSelectList; Conn.fullColumnNames).
//
// A pager missing any of these answers wrongly rather than declining.
func (c *Conn) stampConnState(p *engine.ReadOnlyPager) {
	p.SetConnState(c.changes, c.totalChanges, c.lastInsertRowid, c.totalChangesOpaque, c.lastRowidOpaque)
	p.SetTrustedSchema(!c.untrustedSchema)
	p.SetRtreeConnections(c.rtreeConns)
	// "PRAGMA defer_foreign_keys" is readable, and it is read off the
	// CONNECTION rather than off the held session: it used to be
	// c.tx.DeferForeignKeys(), which is 0 whenever there is no held transaction
	// -- true of the flag before the autocommit setter was served, and wrong
	// afterwards ("PRAGMA defer_foreign_keys=ON" then its own getter answers 1
	// in C SQLite, because a bare pragma's Vdbe never set bIsReader). Inside
	// a held transaction the two agree by construction: noteConnState reads the
	// session's value back into Conn.deferFKs after every statement.
	p.SetDeferForeignKeys(c.deferFKs)
	p.SetIgnoreCheckConstraints(c.ignoreCheckConstraints)
	p.SetAutomaticIndex(!c.noAutomaticIndex)
	p.SetFullColumnNames(c.fullColumnNames)
	p.SetShortColumnNames(!c.shortColumnNamesOff)
	// The per-database tuning values ride along because one of them is now
	// READABLE from a SELECT: "SELECT * FROM pragma_cache_size" (engine's
	// pragmaVtabSchemaOnlyHidden). The setter is answered from Conn state
	// (tuningPragma) and never reaches a session, so a pager that never heard
	// about it reports the -2000 default however many times the connection set
	// the value -- a wrong answer, not a gap. See Conn.pragmaState.
	p.SetPragmaTuningValues(c.pragmaState)
}

// openSessionOrCreateFile is the *DB of the session this connection holds for
// path (sessionFor), which opening creates when the file does not exist yet.
func (c *Conn) openSessionOrCreateFile(path string) (*engine.DB, error) {
	nw, err := c.sessionFor(path)
	if err != nil {
		return nil, err
	}
	return nw.DB, nil
}

// ensureFileExistsPath creates an empty database file at path when none exists
// or it is zero bytes, as C's open does, so a FROM-less "SELECT 1" as the first
// statement on a new *sql.DB works (driver_conformance_test.go). path may be an
// attached database's file. It writes (openWriteOrCreatePath + Close is a
// commit), so callers must run it outside any SHARED lock on the same path (see
// queryArgs).
func (c *Conn) ensureFileExistsPath(path string) error {
	fi, err := os.Stat(path)
	if err == nil && fi.Size() > 0 {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Opening the session CREATES the file (OpenOrCreate), and the session is
	// HELD -- so this must not close it: closing it committed nothing and left
	// the connection's own map holding a closed session.
	_, err = c.sessionFor(path)
	return err
}

// execArgs runs one INSERT/UPDATE/DELETE/CREATE/DROP statement with already-
// engine-typed bound arguments, routing through the held transaction *DB
// (c.tx != nil, no commit -- stays in memory) or the held session in autocommit
// (c.tx == nil: ExecArgs then commit immediately; a failed statement discards
// rather than committing a partial write) -- see Conn's doc comment for the
// full model.
//
// A literal SQL "BEGIN"/"COMMIT"/"END"/"ROLLBACK" statement is intercepted
// BEFORE any of that -- see execTxnStmt's own doc comment for why: the
// autocommit path's open-ExecArgs-Close-immediately cycle would silently
// discard a "BEGIN"'s effect the instant it ran.
func (c *Conn) execArgs(sqlText string, args []engine.Value) (driver.Result, error) {
	c.refreshedThisStmt = false // a new statement is a new freshness boundary
	c.stmtText = sqlText
	if _, handled, perr := c.busyTimeoutPragmaEarly(sqlText); handled {
		return &execResult{lastInsertID: c.lastInsertRowid}, perr
	}
	if _, handled, perr := c.maxSizePragma(sqlText); handled {
		return &execResult{lastInsertID: c.lastInsertRowid}, perr
	}
	// The held segment session, so every branch below finds c.tx non-nil and takes
	// the path it already had. See driver/session.go.
	if serr := c.ensureSession(); serr != nil {
		return nil, serr
	}
	// See deferFKPragmaGuard: a pragma this driver answers itself is the one
	// statement kind neither the session nor the pager half of the
	// defer_foreign_keys lifetime can see. Both calls are a single bool read
	// unless the flag is on with no transaction open.
	if derr := c.deferFKPragmaGuard(sqlText); derr != nil {
		return nil, derr
	}
	defer c.deferFKPragmaApply(sqlText)
	if serr := c.schemaCorruptRefuses(sqlText); serr != nil {
		return nil, serr
	}
	// A statement can OPEN the temp database without writing anything into it,
	// and database/sql routes a row-less caller here whatever the SQL is -- so
	// this path notices it too. See queryArgs' identical note and
	// Conn.tempOpened.
	if !c.tempOpened && engine.StatementOpensTempDatabase(sqlText) {
		c.tempOpened = true
	}
	// "EXPLAIN <stmt>" describes a statement's program and runs nothing, so it
	// belongs on the query path whichever database/sql method the caller used
	// -- C SQLite's own sqlite3_exec("EXPLAIN ...") steps the description
	// and discards its rows, which is what this does. Routed here rather than
	// left to the engine's write path, which has no EXPLAIN at all.
	//
	// An EXPLAIN cannot change the file, the attached set or the transaction
	// state, so the warm read pager stays valid across one.
	if engine.IsExplainStatement(sqlText) {
		rows, err := c.queryArgs(sqlText, args)
		if err != nil {
			return nil, err
		}
		rows.Close()
		return &execResult{lastInsertID: c.lastInsertRowid}, nil
	}
	// Four of the checks below each LEX the statement to answer a yes/no
	// question about it, and every one of them answered "no" for ordinary DML
	// -- so a bulk INSERT paid four full parses per row before the engine even
	// saw the statement. plainDML caches that verdict per statement text (see
	// Conn.plainStmts) and skips them outright once it is known.
	if !c.plainDML(sqlText) {
		// ATTACH/DETACH DATABASE manage the connection's attached-database set
		// (attach.go); they never reach the engine's own Exec.
		if res, handled, err := c.handleAttachDetach(sqlText); handled {
			return res, err
		}
		if kind, ok := engine.TxnStmtKind(sqlText); ok {
			return c.execTxnStmt(kind)
		}
		// SAVEPOINT/RELEASE/ROLLBACK TO open or join the connection's
		// transaction (c.tx), exactly like BEGIN's -- the autocommit path below
		// would commit the statement's own transaction at once. See
		// execSavepointStmt.
		if kind, name, ok := engine.SavepointStmt(sqlText); ok {
			return c.execSavepointStmt(kind, name, sqlText)
		}
		// Every check from here through tuningPragma is PRAGMA-only: each one's
		// own first action is engine.ParsePragma(sqlText), which fails outright
		// unless sqlText's first keyword is literally PRAGMA (pragma.go's
		// ParsePragma, "expected PRAGMA"). isPragmaStmt answers that same
		// question directly, without lexing the statement once per check, so
		// gating the whole chain on it is behavior-preserving by construction:
		// an ordinary SELECT/INSERT/UPDATE/DELETE now skips ~17 guaranteed-to-fail
		// full parses (and the allocated error each one returns) instead of
		// paying for every single one on every non-pragma statement.
		if isPragmaStmt(sqlText) {
			// A PRAGMA that names a SQLite-format mechanism this engine no longer
			// uses is DECLINED, ahead of every handler below. See pragmaDeclines:
			// accepting one as a no-op is what desynchronises the statement after
			// it, and this list was written and then never consulted.
			if derr := c.pragmaDeclines(sqlText); derr != nil {
				return nil, derr
			}
			// A RESERVED WORD as the value is a SYNTAX error whatever the
			// pragma is (parse.y's "nmnum ::= plus_num | nm | ON | DELETE |
			// DEFAULT"), and the connection-level handlers below never reach
			// the engine's own copy of that check -- so "PRAGMA
			// foreign_keys=NULL", "=SELECT" and "=TABLE" were taken here by
			// every pragma this driver answers itself. See
			// engine.PragmaValueSpellingError.
			if serr := engine.PragmaValueSpellingError(sqlText); serr != nil {
				return nil, serr
			}
			// A row-returning pragma whose row an Exec discards, exactly like
			// journal_mode's -- but the SETTER must still be APPLIED here, since
			// that is the path a plain db.Exec takes.
			if _, handled, err := c.secureDeletePragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			// Answered by the Conn for the opposite reason secure_delete is: the
			// mode and its lock are the CONNECTION's, held across statements and
			// across every session it opens. See lockingModePragma.
			if _, handled, err := c.lockingModePragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			// "PRAGMA wal_autocheckpoint = N" is remembered on the Conn whichever
			// database/sql method ran it, as foreign_keys is; otherwise the
			// threshold reverted to the default (1000) for later statements and the
			// getter. Unlike foreign_keys it is not handled here: the PRAGMA still
			// reaches the engine below.
			c.rememberWalAutoCheckpoint(sqlText)
			if handled, err := c.foreignKeysPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			// Answered here, as Conn state -- see
			// deferForeignKeysAutocommitPragma.
			if handled, err := c.deferForeignKeysAutocommitPragma(sqlText); handled {
				return nil, err
			}
			if handled, err := c.caseSensitiveLikePragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			// Conn state too, for the same reason: the flag is the connection's,
			// not any one session's.
			if _, handled, err := c.writableSchemaPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.recursiveTriggersPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.trustedSchemaPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.queryOnlyPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.legacyAlterTablePragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.ignoreCheckConstraintsPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.automaticIndexPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			if handled, err := c.columnNamePragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
			// The tuning pragmas are Conn state as well -- see tuningPragma.
			if _, handled, err := c.tuningPragma(sqlText); handled {
				return &execResult{lastInsertID: c.lastInsertRowid}, err
			}
		}

		// A RETURNING statement has no Result that matches mattn: it steps such
		// a statement once and reads sqlite3_changes() before counting it, so
		// RowsAffected is the previous statement's count. db.Exec no longer
		// arrives here (Stmt.ExecContext runs it through queryArgs and discards
		// the rows, reporting the real count); this is the backstop for a direct
		// execArgs caller. plainDML already ruled out RETURNING when it applies,
		// so this only runs for a statement that might have one.
		if engine.StatementHasReturning(sqlText) {
			return nil, fmt.Errorf("driver: a RETURNING statement must be run with Query, not Exec (its RowsAffected is unspecified): %s", sqlText)
		}
	}

	// Route to the single database this statement references (attach.go). With
	// no attachments this is the main file unchanged; a cross-database or
	// in-transaction attached access is declined here. execSQL differs from
	// sqlText only for a DDL statement routed to an attached database (its
	// redundant target qualifier is stripped, see route).
	targetPath, targetSchema, execSQL, rerr := c.route(sqlText)
	if rerr != nil {
		// A genuine cross-database DML (write to one database, read from others:
		// INSERT ... SELECT, UPDATE ... (SELECT), DELETE ... IN (SELECT)) is not
		// routable to a single file -- run it against a single write target with
		// the other databases wired on as read snapshots (crossDBExec), so long
		// as no explicit transaction is open (a cross-database write inside a
		// transaction stays declined: no cross-file atomic commit; see route).
		if errors.Is(rerr, errCrossDB) && c.tx == nil {
			return c.crossDBExec(sqlText, args)
		}
		return nil, rerr
	}
	sqlText = execSQL

	if c.tx != nil {
		// route guarantees targetPath == c.path here (an attached target inside
		// a transaction was declined above), so the held main session applies.
		// The attachments are wired on as read snapshots for this one statement
		// (withAttachedReaders): a statement route calls main-only can still
		// READ an attachment -- an object-scoped pragma's own lookup, a view or
		// trigger body's qualified reference -- and used to see zero rows.
		release, aerr := c.withAttachedReaders(c.tx)
		if aerr != nil {
			return nil, aerr
		}
		n, _, err := c.tx.ExecArgs(sqlText, args)
		release()

		// Read the counters back unconditionally, as the autocommit path does: a
		// halted statement still publishes them. Reading only on success left
		// the Conn's copy stale until COMMIT replaced the session:
		//
		//	BEGIN; INSERT OR FAIL INTO t VALUES(...4 rows, the 3rd violating...);
		//	SELECT changes(), total_changes()   -- (2,3) both engines
		//	COMMIT;
		//	SELECT changes(), total_changes()   -- (2,3)
		c.noteConnState(c.tx)
		if err != nil {
			// A RAISE(ROLLBACK) trigger error unwinds the WHOLE transaction.
			// Since this driver models the transaction as the held c.tx engine
			// session (the engine's own txActive is not set here), discard that
			// session so the transaction is fully rolled back -- a subsequent
			// COMMIT then correctly reports "no transaction is active", matching
			// C SQLite. Every other error leaves the held session intact
			// (the failing statement's own changes already reverted in-memory).
			if engine.ErrorRollsBackTransaction(err) {
				db := c.tx
				c.tx, c.txSavepoint = nil, ""
				c.discardSession(db)
			}
			return nil, err
		}
		c.autoVacuumPendingPlus1 = c.tx.AutoVacuumPending() // see Conn.autoVacuumPendingPlus1
		c.journalMode = c.tx.JournalMode()                  // see Conn.journalMode
		c.tempStore = c.tx.TempStore()                      // see Conn.tempStore
		c.tempJournalMode = c.tx.TempJournalMode()          // see Conn.tempJournalMode
		// A "writable_schema=RESET" that writableSchemaReload actually applies
		// (engine/schema_write_direct.go) flips c.tx's own flag without going
		// through writableSchemaPragma's setter -- see engine.WritableSchema's
		// doc comment -- so the Conn-level mirror is read back here too.
		c.writableSchema = c.tx.WritableSchema()
		c.noteConnState(c.tx)
		return &execResult{rowsAffected: n, lastInsertID: c.lastInsertRowid}, nil
	}

	var n int64
	// Autocommit statements retry on SQLITE_BUSY: another connection committed
	// since this session loaded (the engine's stale-image refusal, ErrBusy) or
	// held the write lock. Each attempt revalidates the held session first
	// (sessionDB), so the retry runs against the other writer's committed
	// state, exactly like real
	// SQLite's busy handler re-running the statement.
	err := busyRetry(c.pragmaState.BusyTimeout(), func() error {
		db, err := c.openWriteOrCreatePath(targetPath)
		if err != nil {
			return err
		}
		db.SetLocalSchema(targetSchema)
		// Same unconditional wiring the read path does, for the same reason --
		// see withAttachedReaders. Only when MAIN is the primary: a statement
		// route sent to an attachment already runs against its owning file.
		if targetPath == c.path {
			release, aerr := c.withAttachedReaders(db)
			if aerr != nil {
				c.discardSession(db)
				return aerr
			}
			defer release()
		}
		n, _, err = db.ExecArgs(sqlText, args)
		// A "PRAGMA auto_vacuum = MODE" the session accepted leaves its
		// DEFERRED request there, and the session is about to be thrown away --
		// see Conn.autoVacuumPendingPlus1 -- and, for the same reason, the
		// ROLLBACK JOURNAL MODE a "PRAGMA journal_mode = <mode>" left on it,
		// which C SQLite keeps per CONNECTION and nowhere in the file
		// (Conn.journalMode). Reading both back unconditionally is a no-op for
		// every other statement: openWriteOrCreatePath seeded the session from
		// these same fields.
		if err == nil {
			c.autoVacuumPendingPlus1 = db.AutoVacuumPending()
			c.journalMode = db.JournalMode()
			c.tempJournalMode = db.TempJournalMode()
			c.tempStore = db.TempStore() // see Conn.tempStore
			// A RESET the engine performed turned the flag OFF without the setter
			// below -- the explicit-transaction branch's read-back, for the held
			// session. Missing, "PRAGMA writable_schema" answered 1 after a RESET
			// where C answers 0.
			c.writableSchema = db.WritableSchema()
		}
		// ...and likewise this session's changes()/total_changes()/
		// last_insert_rowid(), read back UNCONDITIONALLY (a failed statement
		// still publishes them -- an aborted INSERT sets changes() to 0 and
		// keeps the rowid of the row it undid; see engine/conn_state.go).
		c.noteConnState(db)
		return c.finishAutocommit(db, err)
	})
	if err != nil {
		return nil, err
	}
	// c.lastInsertRowid was already brought forward by noteConnState above: the
	// session STARTED from this Conn's value (openWriteOrCreatePath seeds it)
	// and only a genuine row store moves it, so a statement that
	// inserted nothing -- an UPSERT's DO UPDATE, an INSERT OR IGNORE that
	// skipped every row, an "INSERT ... SELECT" matching none, or any
	// UPDATE/DELETE -- hands the same value straight back, exactly like real
	// SQLite's sqlite3_last_insert_rowid(). ExecArgs' own last-insert-id return
	// is that same value, so it is no longer read here.
	return &execResult{rowsAffected: n, lastInsertID: c.lastInsertRowid}, nil
}

// zeroRowSetterPragmas lists the pragmas whose assignment form returns an empty
// result while the getter returns one row (data_version, journal_mode and
// locking_mode echo the value instead), so it must be per name. It cannot key
// on PragmaStmt.HasValue: that is also true for "(value)", which for
// table_info(t), index_list(t), foreign_key_list(t) and foreign_key_check(t) is
// an argument whose rows are the whole answer.
var zeroRowSetterPragmas = map[string]bool{
	"page_size":      true,
	"user_version":   true,
	"schema_version": true,
	"application_id": true, // the third header scalar, same shape as the two above
	"foreign_keys":   true,
	"encoding":       true,
	// auto_vacuum joined the list when page_count became answerable: its
	// assignment returns no rows, while its getter returns one. The engine still
	// runs the statement, which is what REJECTS "= 1"/"= 2" (C SQLite would
	// add a pointer-map page this engine does not write -- see
	// engine/pragma.go's autoVacuumResult).
	"auto_vacuum": true,
	// temp_store is connection state rather than a header value, and its
	// ASSIGNMENT is a write-path statement here (r35aTempStoreSetter can DISCARD
	// the temp database, which a read snapshot cannot do) while its getter is
	// answered from the snapshot. C SQLite's PragTyp_TEMP_STORE setter answers
	// ZERO rows, which is this list's shape exactly -- without the entry, a
	// Query of "PRAGMA temp_store=MEMORY" reached the read side and was declined
	// while the same text through Exec succeeded, so whether an application's
	// pragma worked depended on which database/sql method it happened to call.
	"temp_store": true,
	// defer_foreign_keys is a connection flag rather than a header value, but
	// its assignment has the same shape: C SQLite answers zero rows for it
	// and one for the getter, and the engine's read side declines the
	// assignment outright (queryPragmaStmt) so it has to be run as a write.
	"defer_foreign_keys": true,
	// writable_schema's setter is Conn state -- except RESET with a direct catalog
	// edit outstanding, which is a schema RELOAD only the write path can perform.
	// Through Query it reached the read side and was declined ("on the read side")
	// where C runs it (writable_schema_reset_corrupt_row_test.go); Exec already
	// handled every form.
	"writable_schema": true,
}

// isZeroRowSetterPragma reports whether sqlText is an assignment to one of them.
func isZeroRowSetterPragma(sqlText string) bool {
	stmt, err := engine.ParsePragma(sqlText)
	return err == nil && stmt != nil && stmt.HasValue && zeroRowSetterPragmas[stmt.Name]
}

// encodingPragma handles "PRAGMA encoding = <name>" at the CONNECTION level.
// The setter only ever acts on a database with no schema yet (see engine's
// execPragma for the verified rules), and in this driver the session that
// eventually CREATES the file is a later one, so the request is remembered here
// and pushed into every session this Conn opens. The engine still runs the
// statement -- that is what rejects an unrecognized name on an empty database
// and what makes the setter a no-op once a schema exists.
func (c *Conn) encodingPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "encoding" || !stmt.HasValue {
		return false, nil
	}
	if _, eerr := c.execArgs(sqlText, nil); eerr != nil {
		return true, eerr
	}
	if enc, ok := engine.TextEncodingByName(stmt.ValueText); ok && !c.databaseExists() {
		c.textEncoding = enc
	}
	return true, nil
}

// databaseExists reports whether this connection's file is already a database
// with content -- in which case "PRAGMA encoding" is a no-op and must not be
// remembered.
func (c *Conn) databaseExists() bool {
	fi, err := os.Stat(c.path)
	return err == nil && fi.Size() > 0
}

// isJournalModeAssignment reports whether sqlText is "PRAGMA
// [schema.]journal_mode = <mode>" (as opposed to the value-less getter).
func isJournalModeAssignment(sqlText string) bool {
	stmt, err := engine.ParsePragma(sqlText)
	return err == nil && stmt != nil && stmt.Name == "journal_mode" && stmt.HasValue
}


// journalModeGetterFor is the value-less form of a journal_mode assignment,
// preserving its schema qualifier -- what queryArgs reads the resulting mode
// back with once the assignment has been applied through the write path.
func journalModeGetterFor(sqlText string) string {
	if stmt, err := engine.ParsePragma(sqlText); err == nil && stmt != nil && stmt.Schema != "" {
		return "PRAGMA " + stmt.Schema + ".journal_mode"
	}
	return "PRAGMA journal_mode"
}

// rememberWalAutoCheckpoint records a "PRAGMA wal_autocheckpoint = N"
// assignment on the Conn, so the getter -- served from a later read snapshot --
// answers what this connection asked for rather than the built-in default. A
// value that is not a number, or is negative, reads back as 0 in C SQLite.
func (c *Conn) rememberWalAutoCheckpoint(sqlText string) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "wal_autocheckpoint" || !stmt.HasValue {
		return
	}
	n, cerr := strconv.Atoi(strings.TrimSpace(stmt.ValueText))
	if cerr != nil || n < 0 {
		n = 0
	}
	c.walAutoCheckpointPlus1 = n + 1
}

// isWalCheckpointPragma reports whether sqlText is "PRAGMA
// [schema.]wal_checkpoint[(MODE)]" and, if so, returns its parsed form --
// walCheckpointQueryArgs needs the *PragmaStmt itself, not just a yes/no, to
// actually run the checkpoint.
func isWalCheckpointPragma(sqlText string) (*engine.PragmaStmt, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "wal_checkpoint" {
		return nil, false
	}
	return stmt, true
}

// isWalAutoCheckpointAssignment reports whether sqlText is "PRAGMA
// wal_autocheckpoint = N": its ANSWER depends on an action only a write
// session can take (the threshold lives on that session), same as
// wal_checkpoint below -- but unlike wal_checkpoint, its getter
// (queryPragmaStmt's own "wal_autocheckpoint" case) answers purely from what
// this driver remembered (rememberWalAutoCheckpoint), never by re-deriving
// from file state, so there is no race for it to get wrong.
func isWalAutoCheckpointAssignment(sqlText string) bool {
	stmt, err := engine.ParsePragma(sqlText)
	return err == nil && stmt != nil && stmt.Name == "wal_autocheckpoint" && stmt.HasValue
}

// deferFKPragmaGuard / deferFKPragmaApply apply "PRAGMA defer_foreign_keys"'s
// autocommit lifetime to a statement this driver answers itself, which never
// reaches the engine's halves (deferFKAutocommitGuard/Apply,
// deferFKPagerReadGuard/Apply). Classification is engine.DeferFKPragmaClears;
// an unknown verdict declines rather than letting the flag drift, since a stale
// ON would defer a later transaction's FK check C performs immediately.
func (c *Conn) deferFKPragmaGuard(sqlText string) error {
	if !c.deferFKs || c.tx != nil {
		return nil
	}
	if _, isPragma := engine.ParsePragma(sqlText); isPragma != nil {
		return nil // not a pragma: the session or pager halves own it
	}
	if _, known := engine.DeferFKPragmaClears(sqlText); known {
		return nil
	}
	return fmt.Errorf("driver: PRAGMA defer_foreign_keys is ON with no transaction open, and this pragma is outside the table this engine has measured for whether C SQLite would then clear it -- declined rather than guessed: %s", sqlText)
}

func (c *Conn) deferFKPragmaApply(sqlText string) {
	if !c.deferFKs || c.tx != nil {
		return
	}
	if clears, known := engine.DeferFKPragmaClears(sqlText); known && clears {
		c.deferFKs = false
	}
}

// deferForeignKeysAutocommitPragma no longer intercepts anything: Conn.deferFKs
// carries the flag, sessions and pagers are stamped with it, and the engine's
// execPragma serves the statement (defer_foreign_keys_pragma_test.go). It always
// reports "not handled".
func (c *Conn) deferForeignKeysAutocommitPragma(string) (handled bool, err error) {
	return false, nil
}

// foreignKeysPragma handles "PRAGMA [schema.]foreign_keys [= <boolean>]" at
// the CONNECTION level. Two rules, both verified directly against C SQLite
// (mattn/go-sqlite3 3.53.3) and shared with the engine's own execPragma:
//
//   - the assignment returns NO rows and the getter returns exactly one;
//   - an assignment issued INSIDE a transaction is silently IGNORED.
//
// A value outside the canonical boolean spellings is left to the engine, which
// declines it (see engine/pragma.go) rather than guess sqlite3GetBoolean's own
// idiosyncratic parse.
func (c *Conn) foreignKeysPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "foreign_keys" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from foreignKeysPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	if c.tx == nil {
		c.foreignKeys = b
	}
	return true, nil
}

// caseSensitiveLikePragma handles "PRAGMA case_sensitive_like [= <boolean>]" at
// the CONNECTION level, for the reason foreignKeysPragma exists. Both forms
// return NO rows -- the getter is a no-op in C SQLite (it reports nothing
// and changes nothing), which is why there is no ...PragmaRows counterpart.
// A value spelling the engine would not accept is left to the engine to
// decline in its own wording, exactly like foreignKeysPragma does.
func (c *Conn) caseSensitiveLikePragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "case_sensitive_like" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: no rows, and the setting is left alone
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.caseSensitiveLike = b
	return true, nil
}

// queryOnlyPragma handles "PRAGMA [schema.]query_only [= <boolean>]" at the
// CONNECTION level, for the reason foreignKeysPragma exists. Like
// recursive_triggers and unlike foreign_keys, a setter issued inside a
// transaction is NOT ignored (verified against mattn/go-sqlite3 3.53.3), so
// there is no c.tx guard.
func (c *Conn) queryOnlyPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "query_only" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from queryOnlyPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.queryOnly = b
	return true, nil
}

// queryOnlyPragmaRows is the getter's single row, for the Query path.
func (c *Conn) queryOnlyPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if c.queryOnly {
		v.I = 1
	}
	return &Rows{cols: []string{"query_only"}, rows: [][]engine.Value{{v}}}, true
}

// recursiveTriggersPragma handles "PRAGMA [schema.]recursive_triggers
// [= <boolean>]" at the CONNECTION level, for the reason foreignKeysPragma
// exists. It differs from that one in a single rule, verified directly against
// mattn/go-sqlite3 3.53.3: a setter issued INSIDE a transaction is NOT ignored
// -- it takes effect at once and is still set after the COMMIT -- so there is
// no c.tx guard here.
func (c *Conn) recursiveTriggersPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "recursive_triggers" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from recursiveTriggersPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.recursiveTriggers = b
	return true, nil
}

// legacyAlterTablePragma handles "PRAGMA [schema.]legacy_alter_table
// [= <boolean>]" at the CONNECTION level, for the reason queryOnlyPragma exists
// -- and this one NEEDS it more than the others: the flag's whole behavior is a
// LATER statement's (the ALTER TABLE RENAME TO after it), so a value that dies
// with the session that set it is not a partial implementation, it is no
// implementation. A qualifier is ignored, and a setter inside a transaction is
// not (both verified against mattn/go-sqlite3 3.53.3).
func (c *Conn) legacyAlterTablePragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "legacy_alter_table" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from legacyAlterTablePragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.legacyAlterTable = b
	return true, nil
}

// legacyAlterTablePragmaRows is the getter's single row, for the Query path.
func (c *Conn) legacyAlterTablePragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if c.legacyAlterTable {
		v.I = 1
	}
	return &Rows{cols: []string{"legacy_alter_table"}, rows: [][]engine.Value{{v}}}, true
}

// columnNamePragma handles the setter of "PRAGMA [schema.]full_column_names"
// and "short_column_names" at the connection level, since their only effect is
// later statements' result names (see Conn.fullColumnNames). The getter is left
// to the engine's read side, which answers from the same fields
// expandSelectList reads (stamped by stampConnState). Inside a transaction the
// held session is updated too (PragTyp_FLAG takes effect at once;
// SnapshotPager copies DB.fullColumnNames), and the Conn field is written
// either way (ROLLBACK does not restore a db->flags bit).
func (c *Conn) columnNamePragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil {
		return false, nil
	}
	if stmt.Name != "full_column_names" && stmt.Name != "short_column_names" {
		return false, nil
	}
	if !stmt.HasValue {
		return false, nil // getter: answered by the engine's read side
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	if stmt.Name == "full_column_names" {
		c.fullColumnNames = b
		if c.tx != nil {
			c.tx.SetFullColumnNames(b)
		}
	} else {
		c.shortColumnNamesOff = !b
		if c.tx != nil {
			c.tx.SetShortColumnNames(b)
		}
	}
	// Nothing to invalidate here: stampReadPager stamps every read pager as it
	// is handed out, and engine's SetFullColumnNames drops the compiled-plan
	// cache when the flag really moves -- which it must, since a Program carries
	// its own ColNames (see that function).
	return true, nil
}

// ignoreCheckConstraintsPragma handles "PRAGMA
// [schema.]ignore_check_constraints [= <boolean>]" at the connection level:
// its effect belongs to later statements (CHECK enforcement, integrity_check).
// A qualifier is ignored ("PRAGMA temp.ignore_check_constraints=OFF" clears a
// bare "= ON"), and a setter inside a transaction survives ROLLBACK.
func (c *Conn) ignoreCheckConstraintsPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "ignore_check_constraints" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from ignoreCheckConstraintsPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.ignoreCheckConstraints = b
	return true, nil
}

// ignoreCheckConstraintsPragmaRows is the getter's single row, for the Query
// path.
func (c *Conn) ignoreCheckConstraintsPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if c.ignoreCheckConstraints {
		v.I = 1
	}
	return &Rows{cols: []string{"ignore_check_constraints"}, rows: [][]engine.Value{{v}}}, true
}

// automaticIndexPragma handles "PRAGMA [schema.]automatic_index [= <boolean>]"
// at the CONNECTION level, for legacyAlterTablePragma's reason -- see
// Conn.noAutomaticIndex. A qualifier is ignored (it is a db->flags bit; see
// engine's attachedPragmaScope), and a setter inside a transaction is not.
func (c *Conn) automaticIndexPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "automatic_index" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from automaticIndexPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.noAutomaticIndex = !b
	return true, nil
}

// automaticIndexPragmaRows is the getter's single row, for the Query path.
func (c *Conn) automaticIndexPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if !c.noAutomaticIndex {
		v.I = 1
	}
	return &Rows{cols: []string{"automatic_index"}, rows: [][]engine.Value{{v}}}, true
}

// trustedSchemaPragma handles "PRAGMA [schema.]trusted_schema [= <boolean>]"
// at the CONNECTION level, for the reason recursiveTriggersPragma exists -- and
// with its transaction rule too: a setter issued inside BEGIN takes effect at
// once and still reads back after a ROLLBACK (verified against mattn/go-sqlite3
// 3.53.3). A qualifier is ignored: "PRAGMA aux.trusted_schema=0" makes the bare
// getter and "PRAGMA main.trusted_schema" both answer 0.
func (c *Conn) trustedSchemaPragma(sqlText string) (handled bool, err error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "trusted_schema" {
		return false, nil
	}
	if !stmt.HasValue {
		return true, nil // getter: rows come from trustedSchemaPragmaRows
	}
	b := engine.PragmaBooleanValue(stmt.ValueText)
	c.untrustedSchema = !b
	return true, nil
}

// trustedSchemaPragmaRows is the getter's single row, for the Query path.
func (c *Conn) trustedSchemaPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if !c.untrustedSchema {
		v.I = 1
	}
	return &Rows{cols: []string{"trusted_schema"}, rows: [][]engine.Value{{v}}}, true
}

// recursiveTriggersPragmaRows is the getter's single row, for the Query path.
func (c *Conn) recursiveTriggersPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if c.recursiveTriggers {
		v.I = 1
	}
	return &Rows{cols: []string{"recursive_triggers"}, rows: [][]engine.Value{{v}}}, true
}

// foreignKeysPragmaRows is the getter's single row, for the Query path.
func (c *Conn) foreignKeysPragmaRows(sqlText string) (driver.Rows, bool) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.HasValue {
		return nil, false
	}
	v := engine.Value{Typ: engine.Int}
	if c.foreignKeys {
		v.I = 1
	}
	return &Rows{cols: []string{"foreign_keys"}, rows: [][]engine.Value{{v}}}, true
}

// writableSchemaPragma answers "PRAGMA [schema.]writable_schema [= <value>]"
// at the connection level, like foreignKeysPragma: the setter returns no rows
// and the getter one, and the flag lives on the Conn (Conn.writableSchema).
// The value spellings and the RESET rule
// belong to the engine -- engine.PragmaWritableSchemaValue -- so both sides
// answer from one parse; a spelling it does not recognize is left to the
// engine to decline in its own wording.
func (c *Conn) writableSchemaPragma(sqlText string) (driver.Rows, bool, error) {
	stmt, perr := engine.ParsePragma(sqlText)
	if perr != nil || stmt == nil || stmt.Name != "writable_schema" {
		return nil, false, nil
	}
	if !c.pragmaSchemaResolves(stmt.Schema) {
		return nil, true, fmt.Errorf("driver: unknown database %s", stmt.Schema)
	}
	if !stmt.HasValue {
		v := engine.Value{Typ: engine.Int}
		if c.writableSchema {
			v.I = 1
		}
		return &Rows{cols: []string{"writable_schema"}, rows: [][]engine.Value{{v}}}, true, nil
	}
	on, isReset := engine.PragmaWritableSchemaValue(stmt.ValueText)
	if isReset && c.writableSchemaEditsOutstanding() {
		// RESET is OFF plus a schema reload; recording OFF here would drop the
		// reload (writableSchemaResetDecline). Ask the session, not c.tx: the
		// connection holds one session for its life, so an autocommit catalog
		// edit is still outstanding on it, and recording OFF would leave it
		// outstanding forever.
		return nil, false, nil // let the engine reload it, or decline in its own wording
	}
	c.writableSchema = on
	if on {
		// Turning it ON lifts corruptSchema's latch: the reload C retries at the
		// next prepare takes the silent arm (prepare.c:45-46), as the engine's own
		// setter does (clearSchemaCorrupt). The latch lives on the Conn AND on the
		// held session, so both are cleared.
		c.schemaCorruptObj, c.schemaCorruptDetail = "", ""
		if c.ndb != nil && c.ndb.DB != nil {
			c.ndb.SetSchemaCorrupt("", "")
		}
	}
	if c.tx != nil {
		// A HELD session took this flag at open (openWriteOrCreatePath), so a
		// setter run inside the transaction has to reach it too -- otherwise
		// the very next "INSERT INTO sqlite_schema" in the same transaction
		// is refused by a session that never heard about it.
		c.tx.SetWritableSchema(on)
	}
	return &Rows{}, true, nil // setter: empty result set, no columns
}

// writableSchemaEditsOutstanding reports whether this connection holds a direct
// sqlite_schema write that has not been reloaded yet -- on the held session, or
// on an explicit transaction's.
func (c *Conn) writableSchemaEditsOutstanding() bool {
	if c.tx != nil && c.tx.WritableSchemaEditsActive() {
		return true
	}
	return c.ndb != nil && c.ndb.DB != nil && c.ndb.WritableSchemaEditsActive()
}

// secureDeletePragma answers "PRAGMA [schema.]secure_delete [= <value>]" from
// this connection's value (it is not stored in the file; see
// engine.SecureDeleteResult for the rules). The setter reports the new value,
// so both forms answer one row. The qualifier is resolved against this
// connection's ATTACHed set, which the engine cannot see: "PRAGMA
// db2.secure_delete" before the ATTACH is "unknown database db2".
func (c *Conn) secureDeletePragma(sqlText string) (driver.Rows, bool, error) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "secure_delete" {
		return nil, false, nil
	}
	if !c.pragmaSchemaResolves(stmt.Schema) {
		return nil, true, fmt.Errorf("driver: unknown database %s", stmt.Schema)
	}
	if c.pragmaState == nil {
		c.pragmaState = &engine.PragmaConnState{}
	}
	cols, rows, err := engine.SecureDeleteStateResult(stmt, c.pragmaState)
	if err != nil {
		return nil, true, err
	}
	// Conn.secureDelete stays MAIN's value: it is what this Conn pushes into
	// every session and pager it opens (SetSecureDelete).
	c.secureDelete = engine.SecureDeleteMain(c.pragmaState)
	return &Rows{cols: cols, rows: rows}, true, nil
}


// schemaCorruptRefuses applies corruptSchema's latch (Conn.schemaCorruptObj)
// on BOTH of this driver's paths, because unlike the engine's own the latch
// here has no session to live on -- see the engine's
// schemaCorruptRefusesStatement for the rule, which exempts PRAGMA so that
// "PRAGMA writable_schema=ON" can lift it.
func (c *Conn) schemaCorruptRefuses(sqlText string) error {
	return engine.SchemaCorruptRefuses(c.schemaCorruptObj, c.schemaCorruptDetail, sqlText)
}

// lockingModeSet records a change of this connection's locking mode (and, for
// the unqualified form, the connection default alongside main's mode, as
// execLockingMode splits them). It takes no lock: refreshOnce, run at the
// top of every read and write, does, because C takes it at the next file access
// (the pragma alone excludes nobody; TestLockingModeExclusiveOracleSpec).
// Leaving takes effect in the getter at once and the lock survives until the
// next access ends.
//
// Entering the mode with a database attached is declined, as the engine
// declines it: C's unqualified setter locks every attached file too.
func (c *Conn) lockingModeSet(exclusive, bare bool) (refused bool, err error) {
	if exclusive && !c.lockingMain && len(c.attached) > 0 {
		return false, fmt.Errorf("driver: unsupported PRAGMA locking_mode=exclusive while databases are ATTACHed (C SQLite's unqualified setter locks every attached file as well, which this connection would have to hold a descriptor on each of)")
	}
	walMode := engine.SegmentFileJournalWAL(c.path)
	if exclusive && !c.lockingMain {
		c.lockingEnteredInWAL = walMode
	}
	if bare {
		c.lockingDefault = exclusive
	}
	// LEAVING is refused while the wal-index is heap-backed -- the Wal was
	// opened while this connection was already exclusive. The DEFAULT above
	// still moves, because pragma.c writes db->dfltLockMode before it asks the
	// pager and unconditionally; only main's own mode stays. See
	// Conn.lockingEnteredInWAL.
	if !exclusive && c.lockingMain && walMode && !c.lockingEnteredInWAL {
		return true, nil
	}
	c.lockingMain = exclusive
	return false, nil
}

// lockingModePragma answers "PRAGMA [schema.]locking_mode [= <mode>]" on the
// connection, which holds both the connection default and main's mode
// (Conn.lockingMain).
//
// Exclusive mode is real: the lock is taken at the first file access after the
// pragma (refreshOnce) and held across statements on engine.Session.f
// (Session.HoldLockingMode), and every other lock this connection would take is
// suppressed meanwhile (Session.BeginWrite, the engine's
// withSegmentReadLock). That is C's model: one unixFile handle per connection
// (os_unix.c:261) that does not re-lock; this driver has four descriptors on its
// main file, and OFD locks would make them block each other, so one holds and
// the rest are suppressed.
//
// TestLockingModeExclusiveOracleSpec reads the behavior off 3.53.3: the pragma
// alone excludes nobody; after a read another connection can read but not
// write; after a write it can do neither; in WAL the other connection loses
// reads once this one has read (hence a write lock over the shared range in
// WAL, a read lock otherwise). Leaving flips the getter at once and the lock
// lasts until the next access (TestLockingModeExclusiveLeavesTheModeLikeC).
//
// No lock is involved for the getter, an unrecognized value (getLockingMode
// treats "=xyz" as a query), or "temp." (always EXCLUSIVE, unchangeable). A
// memory-backed database is declined (lockingModeResult), as is entering the
// mode with a database attached (lockingModeSet).
func (c *Conn) lockingModePragma(sqlText string) (driver.Rows, bool, error) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "locking_mode" {
		return nil, false, nil
	}
	if !c.pragmaSchemaResolves(stmt.Schema) {
		return nil, true, fmt.Errorf("driver: unknown database %s", stmt.Schema)
	}
	// A real mode CHANGE is recorded HERE, on the connection, and takes no
	// lock yet: C SQLite's own "the change does not actually take effect
	// until the next time the database file is accessed" is exactly that, and
	// this driver's next access -- read or write -- runs refreshOnce
	// before it touches the file.
	if want, ok := engine.LockingModeRequested(stmt); ok && !engine.IsTempSchemaQualifier(stmt.Schema) &&
		!c.memBacked(c.path) {
		refused, derr := c.lockingModeSet(want, stmt.Schema == "")
		if derr != nil {
			return nil, true, derr
		}
		if refused {
			// pragma.c answers the mode the pager is STILL in, which is what
			// the pager refused to leave -- see lockingModeSet.
			return &Rows{cols: []string{"locking_mode"}, rows: [][]engine.Value{{{Typ: engine.Text, S: []byte("exclusive")}}}}, true, nil
		}
	}
	cols, rows, lerr := engine.LockingModeResult(c.memBacked(c.path), c.lockingDefault, c.lockingMain, stmt)
	if lerr != nil {
		return nil, true, lerr
	}
	return &Rows{cols: cols, rows: rows}, true, nil
}

// tuningPragma answers "PRAGMA [schema.]synchronous / cache_size /
// journal_size_limit / mmap_size" -- and default_cache_size, which the oracle
// omits entirely -- from Conn state, both forms, for the reason
// writableSchemaPragma exists. The rules all live in the engine
// (PragmaTuningResult); this only owns the map, because the values are the
// connection's, not any one session's.
//
// An UNRESOLVED qualifier is deliberately not handled here: C SQLite fails
// it with "unknown database X", so answering a row would be a wrong answer
// databasePageSize is this connection's database's own page size, or the default
// when no session is open yet to ask. See engine.PageSize.
func (c *Conn) databasePageSize() uint32 {
	if c.ndb != nil {
		if db := c.ndb.DB; db != nil {
			if ps := db.PageSize(); ps != 0 {
				return ps
			}
		}
	}
	return defaultPageSize
}

// rather than a gap. Left unhandled, it takes the normal path and errors.
// busyTimeoutPragmaEarly answers "PRAGMA busy_timeout [= N]" from connection
// state BEFORE the statement acquires the session: in C it touches no file
// (mainReadTxnOf's measured table lists it NO), and the one pragma that sets how
// long to wait for a lock must not itself wait -- behind another connection's
// exclusive lock it did, for the full default, and then failed.
func (c *Conn) busyTimeoutPragmaEarly(sqlText string) (driver.Rows, bool, error) {
	// Plain DML first: it is cached per statement text, so the common case
	// costs a map probe rather than a scan of the text (it was a tenth of a
	// batch INSERT's per-row time when it lowered a copy of every statement).
	if c.plainDML(sqlText) || !containsFold(sqlText, "BUSY_TIMEOUT") {
		return nil, false, nil
	}
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "busy_timeout" || !c.pragmaSchemaResolves(stmt.Schema) {
		return nil, false, nil
	}
	if c.pragmaState == nil {
		c.pragmaState = &engine.PragmaConnState{}
	}
	cols, rows, handled, perr := engine.PragmaTuningResult(stmt, engine.PragmaTuningDB{InTransaction: c.tx != nil}, c.pragmaState)
	if !handled || perr != nil {
		return nil, handled, perr
	}
	// Both forms answer one row in C (pragmaBusyTimeout); Exec discards it.
	return &Rows{cols: cols, rows: rows}, true, nil
}

// maxSizePragma answers "PRAGMA max_size [= N]": the most bytes this
// connection's commits may leave main's file holding (engine's DB.SetMaxSize),
// 0 for none. musql's own pragma -- C has no counterpart, and its nearest,
// max_page_count, counts pages this format does not have. Connection state, set
// and read without touching the file, and not persisted: the caller keeps it
// (or passes it on every open, "?_pragma=max_size(N)"). Main only, so a schema
// other than main is refused rather than quietly applied to main.
func (c *Conn) maxSizePragma(sqlText string) (driver.Rows, bool, error) {
	if c.plainDML(sqlText) || !containsFold(sqlText, "MAX_SIZE") {
		return nil, false, nil
	}
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || stmt.Name != "max_size" {
		return nil, false, nil
	}
	if stmt.Schema != "" && !strings.EqualFold(stmt.Schema, "main") {
		return nil, true, fmt.Errorf("driver: PRAGMA %s.max_size: the limit is main's only", stmt.Schema)
	}
	if stmt.HasValue {
		n, perr := strconv.ParseInt(stmt.ValueText, 10, 64)
		if perr != nil || n < 0 || stmt.ValueIsString {
			return nil, true, fmt.Errorf("driver: PRAGMA max_size = %s: want a byte count >= 0", stmt.ValueText)
		}
		c.maxSize = n
	}
	return &Rows{cols: []string{"max_size"}, rows: [][]engine.Value{{{Typ: engine.Int, I: c.maxSize}}}}, true, nil
}

func (c *Conn) tuningPragma(sqlText string) (driver.Rows, bool, error) {
	stmt, err := engine.ParsePragma(sqlText)
	if err != nil || stmt == nil || !c.pragmaSchemaResolves(stmt.Schema) {
		return nil, false, nil
	}
	if c.pragmaState == nil {
		c.pragmaState = &engine.PragmaConnState{}
	}
	attached, memBacked := c.pragmaSchemaKind(stmt.Schema)
	tdb := engine.PragmaTuningDB{
		InTransaction: c.tx != nil,
		Attached:      attached,
		MemoryBacked:  memBacked,
		// THE DATABASE'S OWN PAGE SIZE, which this format records in its catalog
		// (engine's ConvertedCatalog.PageSize) -- so the KB-to-pages conversion a
		// couple of tuning pragmas need has the number it needs. It used to be
		// hard-zero here, on the reasoning that the format has no page size, and
		// that made "PRAGMA cache_spill" decline for the ordinary case of a
		// negative cache_size (C answers a page-cache geometry computed from
		// |cache_size| * 1024 / page_size).
		PageSize: c.databasePageSize(),
	}
	if stmt.Name == "lock_proxy_file" {
		// Only lock_proxy_file reads LockHeld, so its cost is charged here (see
		// engine.PragmaTuningDB.LockHeld). This side supplies an open
		// transaction (Conn.tx) and WAL, a property of the file's catalog; held
		// exclusive locking mode is the engine side's third term.
		tdb.LockHeld = c.tx != nil || engine.SegmentFileJournalWAL(c.path)
	}
	cols, rows, handled, perr := engine.PragmaTuningResult(stmt, tdb, c.pragmaState)
	if !handled || perr != nil {
		return nil, handled, perr
	}
	// The held session compiles this connection's writes, so it has to hear a
	// setter too -- see engine's DB.SetPragmaTuningValues.
	if c.ndb != nil {
		c.ndb.SetPragmaTuningValues(c.pragmaState)
	}
	return &Rows{cols: cols, rows: rows}, true, nil
}

// countChangesRow is "PRAGMA count_changes", the half that cannot live in the
// engine: with SQLITE_CountRows, sqlite3Insert/Update/DeleteFrom append a result
// row of their counter named after the verb (sqlite3CodeChangeCount(v,
// regRowCount, "rows inserted"|"rows updated"|"rows deleted"),
// src/delete.c:51). ExecArgs returns no rows, so the flag rides in
// PragmaConnState and the row is produced here, on the path with a result set.
//
// C's conditions: each arm requires !pParse->nested && !pParse->pTriggerTab &&
// !pParse->bReturning (a top-level, non-RETURNING statement; queryArgs routes
// RETURNING elsewhere), and UPDATE also pUpsert==0.
//
// The count is the statement's change count for UPDATE and DELETE. INSERT
// counts only after sqlite3CompleteInsertion (insert.c:1600); an upsert's DO
// UPDATE jumps past it (insert.c:2362), so "INSERT ... ON CONFLICT DO UPDATE"
// updating a row answers "rows inserted" 0 while RowsAffected is 1. The engine
// tracks that counter (DB.RowsInserted, engine/writer.go), and the INSERT arm
// reads it.
func (c *Conn) countChangesRow(sqlText string, res driver.Result) *Rows {
	if !c.pragmaState.CountChanges() || res == nil {
		return nil
	}
	verb, ok := engine.LeadingStatementVerb(sqlText)
	if !ok {
		return nil
	}
	var col string
	switch strings.ToUpper(verb) {
	case "INSERT", "REPLACE":
		return &Rows{cols: []string{"rows inserted"},
			rows: [][]engine.Value{{{Typ: engine.Int, I: c.rowsInserted}}}}
	case "UPDATE":
		col = "rows updated"
	case "DELETE":
		col = "rows deleted"
	default:
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	return &Rows{cols: []string{col}, rows: [][]engine.Value{{{Typ: engine.Int, I: n}}}}
}

// pragmaSchemaKind reports the two things a tuning pragma's answer depends on
// besides its own name: whether the qualifier names an ATTACHed database
// (whose defaults differ from main's) and whether that database has no real
// file behind it (which is what makes mmap_size answer nothing).
func (c *Conn) pragmaSchemaKind(schema string) (attached, memoryBacked bool) {
	switch s := r33sFoldIdent(strings.TrimSpace(schema)); s {
	case "", "main":
		return false, c.memBacked(c.path)
	case "temp":
		return false, true
	}
	for _, a := range c.attached {
		if r33sIdentEq(a.name, schema) {
			return true, a.isMem
		}
	}
	return false, false
}

// pragmaSchemaResolves reports whether a pragma's schema qualifier names a
// database this connection has: "" and "main" and "temp" always, an ATTACHed
// name when it is attached.
func (c *Conn) pragmaSchemaResolves(schema string) bool {
	if schema == "" || r33sIdentEq(schema, "main") || r33sIdentEq(schema, "temp") {
		return true
	}
	for _, a := range c.attached {
		if r33sIdentEq(a.name, schema) {
			return true
		}
	}
	return false
}

// queryArgs runs one SELECT with already-engine-typed bound arguments,
// against either the held transaction's CURRENT in-memory snapshot
// (c.tx != nil -- read-your-writes, see SnapshotPager) or the last committed
// on-disk state (c.tx == nil: a fresh engine.Open, closed again once the
// rows are collected) -- see Conn's doc comment for the full model.
func (c *Conn) queryArgs(sqlText string, args []engine.Value) (driver.Rows, error) {
	c.refreshedThisStmt = false // a new statement is a new freshness boundary
	c.stmtText = sqlText
	if rows, handled, perr := c.busyTimeoutPragmaEarly(sqlText); handled {
		if perr != nil {
			return nil, perr
		}
		return rows, nil
	}
	if rows, handled, perr := c.maxSizePragma(sqlText); handled {
		if perr != nil {
			return nil, perr
		}
		return rows, nil
	}
	// See execArgs: one held session for reads too.
	if serr := c.ensureSession(); serr != nil {
		return nil, serr
	}
	if serr := c.schemaCorruptRefuses(sqlText); serr != nil {
		return nil, serr
	}
	// See deferFKPragmaGuard, and execArgs' identical pair.
	if derr := c.deferFKPragmaGuard(sqlText); derr != nil {
		return nil, derr
	}
	defer c.deferFKPragmaApply(sqlText)
	// A pure READ runs on a snapshot with no write session behind it, so this is
	// where the connection notices that the statement OPENED its temp database
	// -- "SELECT * FROM sqlite_temp_master" does, with nothing in it. The write
	// half rides on the session (noteConnState). See Conn.tempOpened.
	if !c.tempOpened && engine.StatementOpensTempDatabase(sqlText) {
		c.tempOpened = true
	}
	// Every check from here through lockingModePragma is PRAGMA-only: each
	// one's own first action is engine.ParsePragma(sqlText) (directly, or via
	// one of the is*Pragma helpers above), which fails outright unless
	// sqlText's first keyword is literally PRAGMA (pragma.go's ParsePragma,
	// "expected PRAGMA"). isPragmaStmt answers that same question directly,
	// without lexing the statement once per check, so gating the whole chain
	// on it is behavior-preserving by construction: an ordinary SELECT/JOIN
	// now skips ~18 guaranteed-to-fail full parses (and the allocated error
	// each one returns) instead of paying for every single one on every
	// non-pragma query.
	if isPragmaStmt(sqlText) {
		// The same decline the Exec path applies, for the same reason: a getter
		// answering a value for a mechanism the format does not have is a wrong
		// answer, not a courtesy. See pragmaDeclines.
		if derr := c.pragmaDeclines(sqlText); derr != nil {
			return nil, derr
		}
		// The same reserved-word check the Exec path runs (see its own
		// comment): a QUERY of "PRAGMA automatic_index=NULL" reaches the
		// connection-level handlers below just as an Exec does, and without
		// this it answered a row where C SQLite reports
		// near "NULL": syntax error.
		if serr := engine.PragmaValueSpellingError(sqlText); serr != nil {
			return nil, serr
		}
		// A PRAGMA whose ASSIGNMENT form returns NO ROWS has to be routed through
		// the write path and answered with an empty result set -- see
		// zeroRowSetterPragmas for the list and the evidence. Doing it here is what
		// makes the shape right without the read-side getter having to suppress
		// itself, which would ALSO suppress applying the setter (a dropped write is
		// strictly worse than a cosmetic extra row).
		if isZeroRowSetterPragma(sqlText) {
			if _, err := c.execArgs(sqlText, args); err != nil {
				return nil, err
			}
			return &Rows{}, nil
		}
		// "PRAGMA optimize" writes (its ANALYZE) and answers ZERO rows of ONE
		// column named after itself: its pragma table entry is FLAG Result1 with
		// no COLS (tool/mkpragmatab.tcl:402-403), and setPragmaResultColumnNames
		// names a column-less Result1 pragma's one column with the pragma's own
		// name. The engine declines the 0x01 debug mask, the only form that
		// reports rows.
		if stmt, err := engine.ParsePragma(sqlText); err == nil && stmt != nil && stmt.Name == "optimize" {
			if _, err := c.execArgs(sqlText, args); err != nil {
				return nil, err
			}
			return &Rows{cols: []string{"optimize"}}, nil
		}
		// A journal_mode ASSIGNMENT is a WRITE: it changes the database file's
		// catalog, so it cannot be served from the read
		// snapshot this function otherwise routes a PRAGMA to. Run it through the
		// write path first and then fall through, so the getter below reports the
		// mode the file now carries -- which is what makes Query and Exec agree on
		// the same statement.
		if isJournalModeAssignment(sqlText) {
			// ...unless proxy locking is on and the target is WAL, in which
			// case C SQLite DOWNGRADES the request back to the current mode
			// silently and the file never moves. Running the write here would
			// really change the mode and make every later getter wrong. The
			// engine's own execJournalMode carries the same guard for the
			// engine-direct path; this connection needs its own because
			// Conn.pragmaState and the write session's DB.pragmaState are
			// different objects. Citation chain in engine's journalModeResult:
			// os_unix.c:5936-5945 -> pager.c:7595-7599 -> vdbe.c:8087-8094.
			if !c.walBlockedByLockProxy(sqlText) {
				if _, err := c.execArgs(sqlText, args); err != nil {
					return nil, err
				}
			}
			sqlText = journalModeGetterFor(sqlText)
		}
		// "PRAGMA wal_checkpoint" is a WRITE for the same reason: it folds the log
		// into the database file -- but unlike every pragma above/below, its own
		// success/busy split matters to the caller and cannot be recovered by a
		// separate read-side requery afterwards. Run it directly and return
		// whatever row it produces (including a busy=1 one, which C SQLite
		// reports as a SUCCESSFUL result, never an error -- see
		// walCheckpointQueryArgs' own doc comment).
		if stmt, ok := isWalCheckpointPragma(sqlText); ok {
			return c.walCheckpointQueryArgs(stmt)
		}
		// "PRAGMA wal_autocheckpoint = N" is routed through the write path too,
		// because the threshold lives on that session, then falls through so the
		// getter below reports it -- safe here because that getter answers from
		// driver-remembered state, not racing file state (see
		// isWalAutoCheckpointAssignment).
		if isWalAutoCheckpointAssignment(sqlText) {
			if _, err := c.execArgs(sqlText, args); err != nil {
				return nil, err
			}
			c.rememberWalAutoCheckpoint(sqlText)
		}
		// "PRAGMA foreign_keys" is answered by the Conn, not by a session: it is
		// connection state this driver owns (see Conn.foreignKeys). Handled here
		// too, not only in execArgs, because callers -- including this repo's own
		// differential worker -- probe with Query before falling back to Exec, and
		// the SETTER must take effect either way.
		if handled, err := c.encodingPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return &Rows{}, nil
		}
		if handled, err := c.foreignKeysPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.foreignKeysPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		// "PRAGMA case_sensitive_like" answers NO rows in either form, so both go
		// through one branch -- see caseSensitiveLikePragma.
		if handled, err := c.caseSensitiveLikePragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return &Rows{}, nil
		}
		// "PRAGMA writable_schema" is Conn state as well; both forms are answered
		// from here (the setter with an empty result set, the getter with one row).
		if rows, handled, err := c.writableSchemaPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
		if handled, err := c.recursiveTriggersPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.recursiveTriggersPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		if handled, err := c.trustedSchemaPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.trustedSchemaPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		if handled, err := c.queryOnlyPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.queryOnlyPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		if handled, err := c.legacyAlterTablePragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.legacyAlterTablePragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		if handled, err := c.ignoreCheckConstraintsPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.ignoreCheckConstraintsPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		if handled, err := c.automaticIndexPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			if rows, isGetter := c.automaticIndexPragmaRows(sqlText); isGetter {
				return rows, nil
			}
			return &Rows{}, nil
		}
		// Only the SETTER is handled here, and it returns an EMPTY result set --
		// pragma.c's PragTyp_FLAG codes returnSingleInt only on its zRight==0 (getter)
		// branch. The getter falls through to the read side; see columnNamePragma.
		if handled, err := c.columnNamePragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return &Rows{}, nil
		}
		// The tuning pragmas answer both forms from here, and the SETTER's shape is
		// per-pragma (journal_size_limit and mmap_size echo the new value, the rest
		// return nothing) -- see tuningPragma.
		if rows, handled, err := c.tuningPragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
		// "PRAGMA secure_delete" is Conn state too, and unlike foreign_keys its
		// SETTER reports the new value back rather than returning nothing -- so both
		// forms answer the same single row from here.
		if rows, handled, err := c.secureDeletePragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
		// "PRAGMA locking_mode" is answered by the Conn too, and for a reason that
		// only exists on this side of the engine boundary -- see lockingModePragma.
		if rows, handled, err := c.lockingModePragma(sqlText); handled {
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
	}
	// A non-SELECT statement is Query-able too: C SQLite (and every driver
	// over it) simply steps it and returns an empty result set, which callers
	// -- and code that probes with Query before falling back to Exec -- rely
	// on. Run it through the write path and report zero rows. RETURNING is
	// excluded: it produces a real result set, handled below.
	if !isSelectStmt(sqlText) && !isPragmaStmt(sqlText) && !engine.StatementHasReturning(sqlText) {
		res, err := c.execArgs(sqlText, args)
		if err != nil {
			return nil, err
		}
		// ...unless "PRAGMA count_changes" is on, in which case a DML really
		// does answer one row. See countChangesRow.
		if rows := c.countChangesRow(sqlText, res); rows != nil {
			return rows, nil
		}
		// ...or names a column over the empty result, as an ALTER TABLE whose
		// program runs a nested SELECT does in C (engine.AlterResultColumns).
		if cols := engine.AlterResultColumns(sqlText); cols != nil {
			return &Rows{cols: cols}, nil
		}
		return &Rows{}, nil
	}
	// Route to the single database this statement references (attach.go).
	targetPath, targetSchema, execSQL, rerr := c.route(sqlText)
	if rerr != nil {
		// A genuine CROSS-database SELECT (FROM items spanning two or more
		// databases) is not routable to a single file -- run it against a
		// multi-database read snapshot instead (crossDBQuery), so long as no
		// explicit transaction is open (a cross-database read mid-transaction
		// stays declined; see route). A cross-database write (a RETURNING DML)
		// is NOT taken here -- it falls through to the decline below.
		if errors.Is(rerr, errCrossDB) && c.tx == nil && isSelectStmt(sqlText) {
			return c.crossDBQuery(sqlText, args)
		}
		return nil, rerr
	}
	sqlText = execSQL
	// An INSERT/UPDATE/DELETE ... RETURNING statement is a WRITE that produces
	// a result set. database/sql routes it here (Query) whenever the caller
	// uses db.Query/QueryRow on it; run it through the write path's
	// ExecReturningArgs (engine/returning_write.go) instead of the read-only
	// SELECT path below -- against the held transaction (read-your-writes, no
	// commit) or a fresh autocommit session (open, run, Close == commit). A
	// plain db.Exec of the same statement still works (execArgs -> ExecArgs
	// discards the RETURNING rows and reports the affected-row count), matching
	// C SQLite, where a RETURNING statement is both Exec-able and Query-able.
	if engine.StatementHasReturning(sqlText) {
		return c.queryReturningArgs(sqlText, args, targetPath, targetSchema)
	}
	// A SELECT that calls fts3/fts4's optimize() is the other statement kind
	// that READS and WRITES: the function merges the named table's segments
	// (engine/fts3_optimize.go). It is served from a write session's OWN
	// snapshot, exactly like a RETURNING DML above -- the held transaction
	// below already is one (c.tx), so only the autocommit case needs routing;
	// the warm read pager it would otherwise use has no session behind it and
	// the engine declines there. StatementCallsFts3Optimize is a lexical test,
	// so it can name a statement that turns out not to need a write session --
	// which costs nothing but the session.
	if c.tx == nil && engine.StatementCallsFts3Optimize(sqlText) {
		return c.queryWriteSessionArgs(sqlText, args, targetPath, targetSchema)
	}
	// "EXPLAIN <write statement>" is the third read served from a write
	// session: it COMPILES the write (and runs nothing, writes nothing), and
	// the write compiler lives on the session, not on a read snapshot. Same
	// routing as the two above -- a held transaction already is such a
	// session. See engine.ExplainWrapsWrite.
	if c.tx == nil && engine.ExplainWrapsWrite(sqlText) {
		return c.queryWriteSessionArgs(sqlText, args, targetPath, targetSchema)
	}
	// EVERY read goes through the held session, in a transaction or not. There is
	// no second read path any more: the warm read pager below it existed to keep a
	// SQLite-format pager's schema and page cache hot across autocommit
	// statements, and a held session keeps both by construction.
	//
	// The session's OWN rows, not SnapshotPager's whole-database materialization:
	// read-your-writes is a map lookup per table. See engine's Session.ReadPager.
	{
		sdb, serr := c.sessionDB()
		if serr != nil {
			return nil, serr
		}
		pager, err := c.readPagerStamped(c.ndb)
		if err != nil {
			return nil, err
		}
		_ = sdb
		release, aerr := c.withAttachedReaders(pager)
		if aerr != nil {
			pager.Close()
			return nil, aerr
		}
		cols, rows, err := pager.QueryArgs(sqlText, args)
		c.deferFKs = pager.DeferForeignKeys() // see Conn.deferFKs: the read may have cleared it
		release()
		pager.Close() // no-op: SnapshotPager's pager owns no file descriptor
		if err != nil {
			return nil, err
		}
		return c.rowsWithDeclTypes(pager, sqlText, cols, rows), nil
	}

	// THE SQLITE READ PATH IS GONE from here. Everything above returns, because a
	// read is answered by the held segment session -- so the warm read pager, its
	// freshness check, the SHARED lock it took, the attached-reader wiring it
	// duplicated and the cross-database routing beneath it were all unreachable.
	//
	// They existed to keep a SQLite pager's schema and page cache hot across
	// autocommit statements, each of which threw its session away. A held session
	// keeps both by construction. See driver/session.go.
}

// queryReturningArgs runs one INSERT/UPDATE/DELETE ... RETURNING statement and
// returns its RETURNING result set, routing through the held transaction *DB
// (c.tx != nil, no commit -- read-your-writes) or the held session in
// autocommit (c.tx == nil: ExecReturningArgs then commit; a failed statement
// discards rather than committing a partial write) -- the exact
// mirror of execArgs's own model, but returning rows instead of a count.
func (c *Conn) queryReturningArgs(sqlText string, args []engine.Value, targetPath, targetSchema string) (driver.Rows, error) {
	if c.tx != nil {
		release, aerr := c.withAttachedReaders(c.tx) // see execArgs' identical wiring
		if aerr != nil {
			return nil, aerr
		}
		cols, rows, err := c.tx.ExecReturningArgs(sqlText, args)
		release()
		// Unconditional, the same rule the autocommit branch below carries
		// the evidence for. Not observable here today -- a held session
		// answers changes() from itself -- so this is consistency, not a fix.
		c.noteConnState(c.tx)
		// AUTOCOMMIT: a RETURNING statement is a write, so it commits here for the
		// same reason execArgs does. Missing it left an autocommit
		// "INSERT ... RETURNING" visible to this connection and gone at the next
		// open -- and left changes() reporting a count for a batch never appended.
		if err == nil {
			if cerr := c.commitIfAutocommit(); cerr != nil {
				err = cerr
			}
		}
		if err != nil {
			return nil, err
		}
		return &Rows{cols: cols, rows: rows}, nil
	}
	db, err := c.openWriteOrCreatePath(targetPath)
	if err != nil {
		return nil, err
	}
	db.SetLocalSchema(targetSchema)
	if targetPath == c.path {
		release, aerr := c.withAttachedReaders(db)
		if aerr != nil {
			c.discardSession(db)
			return nil, aerr
		}
		defer release()
	}
	cols, rows, err := db.ExecReturningArgs(sqlText, args)
	// UNCONDITIONALLY, exactly as execArgs reads it back for a statement with
	// no RETURNING clause: a FAILED write still publishes changes(). An
	// aborted INSERT sets it to 0 (conn_state.go), and returning early here
	// left the PREVIOUS statement's value in place -- over a "u(a UNIQUE,
	// b NOT NULL)" holding two rows, "INSERT INTO u VALUES(1,'y') ON
	// CONFLICT(a) DO UPDATE SET b=NULL RETURNING a,b" aborts and the oracle's
	// next changes() is 0, where this answered 1. The same statement WITHOUT
	// the RETURNING clause already agreed, which is what pinned the gap to
	// this path rather than to the engine.
	c.noteConnState(db)
	if err != nil {
		return nil, c.finishAutocommit(db, err)
	}
	if err := c.commitSession(db); err != nil {
		return nil, err
	}
	return &Rows{cols: cols, rows: rows}, nil
}

// queryWriteSessionArgs runs a SELECT that also WRITES -- today only one that
// calls fts3's optimize() -- against a fresh autocommit WRITE session's own
// snapshot, then commits it. It is queryReturningArgs' sibling and deliberately
// its shape: open, run, commit, and discard the session on any error so a
// half-applied merge never reaches the file.
//
// The rows come from SnapshotPager+QueryArgs rather than from a write-path
// executor because the statement really is a SELECT -- the write is a side
// effect of one function inside it, and the snapshot is the channel back to the
// session (engine.ReadOnlyPager.writeSession).
func (c *Conn) queryWriteSessionArgs(sqlText string, args []engine.Value, targetPath, targetSchema string) (driver.Rows, error) {
	db, err := c.openWriteOrCreatePath(targetPath)
	if err != nil {
		return nil, err
	}
	db.SetLocalSchema(targetSchema)
	if targetPath == c.path {
		// Wired BEFORE the snapshot, so SnapshotPager carries the attachments
		// onto the pager the query actually runs on -- see execArgs' identical
		// wiring and SnapshotPager's attachedReaders copy.
		release, aerr := c.withAttachedReaders(db)
		if aerr != nil {
			c.discardSession(db)
			return nil, aerr
		}
		defer release()
	}
	pager, perr := db.SnapshotPager()
	if perr != nil {
		c.discardSession(db)
		return nil, perr
	}
	cols, rows, qerr := pager.QueryArgs(sqlText, args)
	c.deferFKs = pager.DeferForeignKeys() // see Conn.deferFKs: the read may have cleared it
	var timeCols []bool
	var declTypes []string
	if qerr == nil {
		timeCols = c.timeDeclTypeCols(pager, sqlText, cols)
		declTypes = c.reportedDeclTypes(pager, sqlText, cols)
	}
	pager.Close() // no-op: SnapshotPager's pager owns no file descriptor
	if qerr != nil {
		c.discardSession(db)
		return nil, qerr
	}
	c.noteConnState(db)
	if err := c.commitSession(db); err != nil {
		return nil, err
	}
	r := &Rows{cols: cols, rows: rows, declTypes: declTypes}
	if timeCols != nil {
		r.timeCols, r.loc = timeCols, c.loc
	}
	return r, nil
}

// walCheckpointQueryArgs runs "PRAGMA [db.]wal_checkpoint[(MODE)]" on a write
// session and returns that call's row, rather than requerying afterwards like
// the other write-then-report pragmas: a checkpoint's answer is about the
// attempt, and the read side declines over a non-empty log where a TRUNCATE
// that just folded it answers 0|0|0 (segmentWALCheckpoint,
// engine/pragma_wal.go). Calling engine.WalCheckpointPragma directly also keeps
// it out of noteTransactionMayHaveDirtiedMain (engine/txn.go), which would make
// a later "PRAGMA journal_mode=truncate" in the transaction decline.
func (c *Conn) walCheckpointQueryArgs(stmt *engine.PragmaStmt) (driver.Rows, error) {

	if c.tx != nil {
		// A checkpoint inside an explicit transaction is always declined --
		// db.WalCheckpointPragma reaches walCheckpointPragma's own
		// inTransaction check (pragma_wal.go) -- and that error surfaces here
		// unchanged, exactly as it did through the old execArgs-then-fallthrough
		// shape.
		cols, rows, err := c.tx.WalCheckpointPragma(stmt)
		if err != nil {
			return nil, err
		}
		c.noteConnState(c.tx)
		return &Rows{cols: cols, rows: rows}, nil
	}

	// route() sends every PRAGMA to c.path regardless of any schema qualifier
	// (see route's own "everything else (PRAGMA, ...) runs against main"
	// branch), and wal_checkpoint's own engine implementation never consults
	// stmt.Schema either (pragma_wal.go) -- so there is nothing here to route.
	// This mirrors execArgs' own autocommit branch for this identical
	// statement, substituting WalCheckpointPragma's captured row for
	// ExecArgs' discarded one.
	var cols []string
	var rows [][]engine.Value
	err := busyRetry(c.pragmaState.BusyTimeout(), func() error {
		db, oerr := c.openWriteOrCreatePath(c.path)
		if oerr != nil {
			return oerr
		}
		release, aerr := c.withAttachedReaders(db)
		if aerr != nil {
			c.discardSession(db)
			return aerr
		}
		var cerr error
		cols, rows, cerr = db.WalCheckpointPragma(stmt)
		release()
		if cerr == nil {
			c.autoVacuumPendingPlus1 = db.AutoVacuumPending()
			c.journalMode = db.JournalMode()
			c.tempJournalMode = db.TempJournalMode()
			c.tempStore = db.TempStore() // see Conn.tempStore
		}
		c.noteConnState(db)
		return c.finishAutocommit(db, cerr)
	})
	if err != nil {
		return nil, err
	}
	return &Rows{cols: cols, rows: rows}, nil
}

// execTxnStmt implements SQL "BEGIN"/"COMMIT"/"END"/"ROLLBACK" (intercepted in
// execArgs) with the same c.tx machinery BeginTx/Tx.Commit/Tx.Rollback use, so
// the SQL and Go spellings are one operation and mixing them on a Conn is safe
// (there is at most one held transaction, c.tx). The error strings are C's
// (engine.ErrMsgTxnNested/ErrMsgNoActiveCommit/ErrMsgNoActiveRollback),
// deliberately not the driver.Tx-level errors below, which guard misuse of
// *sql.Tx itself.
func (c *Conn) execTxnStmt(kind engine.TxnKind) (driver.Result, error) {
	switch kind {
	case engine.TxnBegin:
		if c.tx != nil {
			return nil, errors.New(engine.ErrMsgTxnNested)
		}
		// IMMEDIATE and EXCLUSIVE lock at the BEGIN, BEFORE openWriteOrCreate
		// refreshes the session, so the transaction starts from a state no other
		// connection can move until it ends (engine's Session.BeginWriteTxn). In
		// WAL mode C's EXCLUSIVE is IMMEDIATE: readers are never locked out.
		if write, excl := engine.BeginTxnLock(c.stmtText); write {
			nw, err := c.session()
			if err != nil {
				return nil, err
			}
			if err := nw.BeginWriteTxn(excl && !engine.SegmentFileJournalWAL(c.path)); err != nil {
				return nil, err
			}
		}
		db, err := c.openWriteOrCreate()
		if err != nil {
			if c.ndb != nil {
				c.ndb.EndWriteTxn()
			}
			return nil, err
		}
		// THE ENGINE HAS TO SEE THE BEGIN, exactly as beginTxImpl makes it -- and
		// this path is where that was missed, with the worst possible symptom: a
		// SQL-text "BEGIN; INSERT; ROLLBACK" left the row in place, because a held
		// segment session commits per statement unless it knows a transaction is
		// open and the engine had nothing to roll back. C SQLite answered
		// count(*)=1 where this answered 2.
		//
		// beginTxImpl (the database/sql Begin path) was fixed and this one was not,
		// which is exactly the split the harness catches and a driver test does not:
		// Tx.Rollback goes through one, "ROLLBACK" as a statement goes through the
		// other, and an application can reach either.
		if berr := db.Exec(`BEGIN`); berr != nil {
			if c.ndb != nil {
				c.ndb.EndWriteTxn()
			}
			return nil, berr
		}
		db.MarkHeldTransaction()     // see BeginTx's identical call
		db.MarkDeferredFKCommitter() // ...and its identical promise
		c.tx = db
		return &execResult{lastInsertID: c.lastInsertRowid}, nil
	case engine.TxnCommit:
		if c.tx == nil {
			return nil, errors.New(engine.ErrMsgNoActiveCommit)
		}
		db := c.tx
		if err := db.CheckDeferredForeignKeys(); err != nil {
			return nil, err // the transaction stays OPEN, as C SQLite's does
		}
		c.tx, c.txSavepoint = nil, ""
		// The outermost COMMIT clears "PRAGMA defer_foreign_keys" -- see Tx's
		// commitImpl, which does the same for the Go-level path. Only here, after
		// CheckDeferredForeignKeys: a COMMIT that FAILS that check leaves the
		// transaction open with the flag and its counters intact, measured.
		c.deferFKs = false
		// A COMMIT ends whatever started the transaction, savepoint or BEGIN;
		// the savepoint stack goes with it. When a forwarded SAVEPOINT started
		// an ENGINE-level transaction, end that first: Close DISCARDS a session
		// whose engine transaction is still active (the abandoned-BEGIN rule),
		// so committing without this dropped every row written under the
		// savepoint -- "SAVEPOINT c; INSERT; COMMIT" stored nothing.
		if err := c.commitEngineTxn(db); err != nil {
			return nil, err
		}
		if err := c.commitSession(db); err != nil {
			return nil, err
		}
		return &execResult{lastInsertID: c.lastInsertRowid}, nil
	case engine.TxnRollback:
		if c.tx == nil {
			return nil, errors.New(engine.ErrMsgNoActiveRollback)
		}
		db := c.tx
		c.tx, c.txSavepoint = nil, ""
		c.deferFKs = false // as at COMMIT; sqlite3RollbackAll, main.c:1530-1532
		// Likewise a plain ROLLBACK: "SAVEPOINT d; INSERT; ROLLBACK" discards
		// the row in C SQLite, savepoint stack and all.
		if err := c.discardSession(db); err != nil {
			return nil, err
		}
		return &execResult{lastInsertID: c.lastInsertRowid}, nil
	default:
		return nil, errors.New("driver: unreachable: unrecognized TxnKind")
	}
}

// commitEngineTxn ends an ENGINE-level transaction on db, if it has one --
// see engine.HasActiveTransaction for why that is not the same question as
// "is this driver in a transaction". A no-op for every session this driver
// merely holds open.
func (c *Conn) commitEngineTxn(db *engine.DB) error {
	if db == nil || !db.HasActiveTransaction() {
		return nil
	}
	return db.Exec("COMMIT")
}

// execSavepointStmt runs one savepoint statement on the held session, opening
// one if there is none, as a SAVEPOINT in autocommit starts a transaction in C:
//
//	SAVEPOINT a; INSERT; RELEASE a;   -> the row is COMMITTED, and a COMMIT
//	                                     right after reports "cannot commit -
//	                                     no transaction is active"
//	SAVEPOINT a; SAVEPOINT b; RELEASE a
//	                                  -> releases b too, and commits
//	SAVEPOINT c; INSERT; ROLLBACK TO c
//	                                  -> the transaction stays OPEN; a later
//	                                     COMMIT succeeds
//	RELEASE x / ROLLBACK TO x with nothing open   -> error
//
// Only a RELEASE naming the savepoint that started the transaction
// (Conn.txSavepoint) ends it; ROLLBACK TO never does, nor does a savepoint inside
// an explicit BEGIN (txSavepoint is "").
func (c *Conn) execSavepointStmt(kind engine.SavepointKind, name, sqlText string) (driver.Result, error) {
	if c.tx == nil {
		if kind != engine.SavepointOpen {
			// C SQLite: "no such savepoint: <name>". Nothing is open, so
			// there is nothing to release or roll back to.
			return nil, fmt.Errorf("engine: no such savepoint: %s", name)
		}
		db, err := c.openWriteOrCreate()
		if err != nil {
			return nil, err
		}
		db.MarkHeldTransaction() // see execTxnStmt's identical call
		c.tx = db
		c.txSavepoint = name
	}
	if err := c.tx.Exec(sqlText); err != nil {
		return nil, err
	}
	// Did that statement end the transaction the savepoint started? Ask the
	// ENGINE rather than compare names: RELEASE frees the INNERMOST savepoint
	// of the given name, so with "SAVEPOINT sp0; SAVEPOINT sp0; ... RELEASE
	// sp0" the outer one is still open and the transaction still live, while a
	// name comparison would have committed it and made the following COMMIT
	// fail. The engine keeps the real stack, and its transaction goes away
	// exactly when the outermost savepoint does -- found by the fuzz, which is
	// the only reason duplicate names were tried at all.
	//
	// Ending it means COMMITTING: a release unwinds nothing, so every row
	// written since is kept.
	if c.txSavepoint != "" && !c.tx.HasActiveTransaction() {
		db := c.tx
		// A RELEASE that ends the transaction IS a commit, so it runs the
		// COMMIT-time deferred foreign-key check like the other two -- and
		// leaves the transaction open when that fails.
		if err := db.CheckDeferredForeignKeys(); err != nil {
			return nil, err
		}
		c.tx, c.txSavepoint = nil, ""
		if err := c.commitSession(db); err != nil {
			return nil, err
		}
	}
	return &execResult{lastInsertID: c.lastInsertRowid}, nil
}

// Tx implements driver.Tx over one Conn's held transaction *DB (see Conn's
// doc comment).
type Tx struct {
	conn *Conn
}

var _ driver.Tx = (*Tx)(nil)

// commitImpl commits the held transaction: the session's Commit appends the
// transaction's batch to the delta under the write lock, the batch trailer
// being the commit marker (engine/segment_write.go), so this is a genuine
// durable commit, not a simulated one.
func (t *Tx) commitImpl() error {
	db := t.conn.tx
	if db == nil {
		return errors.New("driver: Commit: no transaction is open on this connection")
	}
	// The deferred foreign-key check C SQLite runs at COMMIT
	// (sqlite3VdbeCheckFkDeferred), before anything is torn down: a violation leaves
	// the transaction OPEN there, so it must leave conn.tx in place here.
	if err := db.CheckDeferredForeignKeys(); err != nil {
		return err
	}
	t.conn.tx = nil
	// The OUTERMOST commit clears "PRAGMA defer_foreign_keys" (and the deferred
	// counters with it -- sqlite3VdbeHalt's own clear, vdbeaux.c:3432-3434). The
	// engine does that on the session, which is about to be thrown away, so the
	// carried copy is cleared here too. A SAVEPOINT / RELEASE / ROLLBACK TO does
	// NOT clear it and does not come through this method. See Conn.deferFKs.
	t.conn.deferFKs = false
	return t.conn.commitSession(db)
}

// rollbackImpl rolls the held transaction back (discardSession): every
// statement run in it is undone in the session and nothing is appended, so the
// on-disk file is untouched.
func (t *Tx) rollbackImpl() error {
	db := t.conn.tx
	if db == nil {
		return errors.New("driver: Rollback: no transaction is open on this connection")
	}
	t.conn.tx = nil
	t.conn.deferFKs = false // as at COMMIT; sqlite3RollbackAll, main.c:1530-1532
	return t.conn.discardSession(db)
}

// plainDML reports whether sqlText is ordinary DML: not ATTACH/DETACH, not
// transaction control, not a PRAGMA, not RETURNING, so execArgs can skip those
// four parses. Cached per statement text (Conn.plainStmts). Anything not cheaply
// classifiable is "not plain", which just runs the checks; only the verdict is
// cached, since the handlers execute when they match.
func (c *Conn) plainDML(sqlText string) bool {
	if plain, ok := c.plainStmts[sqlText]; ok {
		return plain
	}
	plain := isPlainDMLText(sqlText)
	if c.plainStmts == nil {
		c.plainStmts = make(map[string]bool)
	}
	c.plainStmts[sqlText] = plain
	return plain
}

// isPlainDMLText decides plainDML's question from the statement's FIRST WORD
// plus a substring test, without lexing: every handler execArgs guards keys off
// a leading keyword (ATTACH/DETACH, BEGIN/COMMIT/END/ROLLBACK, PRAGMA), and a
// RETURNING clause cannot be present unless the word appears in the text.
//
// It errs toward NOT plain in every ambiguous case -- a "RETURNING" inside a
// string literal, an unrecognized leading word -- which simply runs the
// original parse-based checks. Only a confident "this is none of them" skips
// them, so a false negative costs speed and a false positive is impossible.
func isPlainDMLText(sqlText string) bool {
	s := strings.TrimLeft(sqlText, " \t\r\n(")
	end := 0
	for end < len(s) && !isASCIISpace(s[end]) {
		end++
	}
	switch strings.ToUpper(s[:end]) {
	case "SELECT", "INSERT", "REPLACE", "UPDATE", "DELETE", "WITH":
	default:
		return false // CREATE/DROP/ALTER/PRAGMA/ATTACH/BEGIN/... and anything odd
	}
	return !containsFold(sqlText, "RETURNING")
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// containsFold is a case-insensitive strings.Contains for an ASCII needle.
func containsFold(hay, needle string) bool {
	if len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			c := hay[i+j]
			if c >= 'a' && c <= 'z' {
				c -= 32
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// finishAutocommit commits or discards one autocommit session given the
// statement's error (nil on success) and reports what the caller should return.
// An error that keeps autocommit changes (engine.ErrorKeepsAutocommitChanges:
// RAISE(FAIL), OR FAIL) commits the applied state while still reporting the
// error; every other error has already reverted it, so the session is
// discarded.
func (c *Conn) finishAutocommit(db *engine.DB, execErr error) error {
	if execErr != nil {
		if engine.ErrorKeepsAutocommitChanges(execErr) {
			if cerr := c.commitSession(db); cerr != nil {
				c.discardSession(db)
			}
			return execErr
		}
		c.discardSession(db) // don't commit a failed/partial autocommit statement
		return execErr
	}
	return c.commitSession(db)
}

// dropTempObjects ends this connection's TEMP database, so a temp object lives
// no longer than the connection that created it: its file goes with it, as C
// SQLite's temp database is deleted when aDb[1] closes.
func (c *Conn) dropTempObjects() {
	c.temp.Close() // removes its file
	c.temp = nil
}

// BusyRetryAttemptHookForTest, when non-nil, is called once per retry busyRetry
// performs (fn reported engine.ErrBusy and is about to be re-run in a fresh
// session). busyRetry hides contention from the caller, so a harness counting
// only propagated errors can see busy=0 after hundreds of retries; this hook
// lets the concurrent-writer harness count them. Test-only; no effect when nil.
var BusyRetryAttemptHookForTest func()

// busyRetry runs fn, retrying while it reports engine.ErrBusy, until
// engine.BusyTimeout elapses -- the driver-side equivalent of SQLite's
// busy handler (PRAGMA busy_timeout). fn must be safe to run again: every
// caller opens its own fresh engine session inside it.
func busyRetry(wait time.Duration, fn func() error) error {
	deadline := time.Now().Add(wait)
	delay := time.Millisecond
	for {
		err := fn()
		if !errors.Is(err, engine.ErrBusy) || time.Now().After(deadline) {
			return err
		}
		if BusyRetryAttemptHookForTest != nil {
			BusyRetryAttemptHookForTest()
		}
		time.Sleep(delay)
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
}

// walBlockedByLockProxy reports whether sqlText asks for journal_mode=WAL on a
// connection where "PRAGMA lock_proxy_file" has turned proxy locking on.
//
// Proxy locking swaps the file's IO methods for ones with no xShmMap
// (os_unix.c:5936-5945), so sqlite3PagerWalSupported goes false
// (pager.c:7595-7599) and OP_JournalMode replaces the requested mode with the
// current one, silently and without an error (vdbe.c:8087-8094). False on every
// platform but darwin by construction: lock_proxy_file is gated there.
func (c *Conn) walBlockedByLockProxy(sqlText string) bool {
	if c.pragmaState == nil || !c.pragmaState.LockProxyOn {
		return false
	}
	stmt, err := engine.ParsePragma(strings.TrimSpace(sqlText))
	if err != nil || stmt == nil || !stmt.HasValue {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(stmt.ValueText), "wal")
}

// Prepare normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in prepareImpl.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	v, err := c.prepareImpl(query)
	return v, parityErr(err)
}

// PrepareContext normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in prepareContextImpl.
func (c *Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	v, err := c.prepareContextImpl(ctx, query)
	return v, parityErr(err)
}

// Begin normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in beginImpl.
func (c *Conn) Begin() (driver.Tx, error) {
	v, err := c.beginImpl()
	return v, parityErr(err)
}

// BeginTx normalizes its error for parity with C SQLite -- see parityErr
// (errtext.go). The work is in beginTxImpl.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	v, err := c.beginTxImpl(ctx, opts)
	return v, parityErr(err)
}

// Commit normalizes its error for parity with C SQLite -- see parityErr.
func (t *Tx) Commit() error { return parityErr(t.commitImpl()) }

// Rollback normalizes its error for parity with C SQLite -- see parityErr.
func (t *Tx) Rollback() error { return parityErr(t.rollbackImpl()) }
