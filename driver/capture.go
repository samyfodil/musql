package driver

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"sync"

	"github.com/samyfodil/musql/engine"
)

// Change capture: the driver-side seam the replication layer (musql/replication)
// uses to observe row changes and write its own bookkeeping rows atomically
// with the user's transaction -- what SQLite itself provides through
// sqlite3_preupdate_hook plus the commit/rollback hooks.
//
// A consumer registers a connection hook (RegisterConnectionHook), inspects
// the DSN, and -- for the connections it cares about -- installs CaptureHooks
// on the *Conn. From then on every engine session that Conn opens records row
// changes (engine's rowhook.go), and at each commit point the Conn:
//
//  1. drains the session's recorded changes and calls PreCommit with them,
//     with the session still open and still routed to by Conn.Exec -- so any
//     SQL PreCommit runs lands in the SAME transaction the changes came from;
//  2. commits the session;
//  3. calls PostCommit.
//
// Rollback (an explicit ROLLBACK, an abandoned transaction, a failed
// statement's discard) calls Rollback instead, and the recorded changes go
// away with the discarded session.

// CaptureHooks is the set of callbacks a capture consumer installs on a Conn
// (see SetCaptureHooks). Any field may be nil.
type CaptureHooks struct {
	// PreCommit runs inside the transaction about to commit, with every row
	// change it made (never empty -- it is skipped when nothing changed). It
	// may run SQL on conn; those writes commit atomically with the user's.
	// An error aborts the commit and rolls the transaction back.
	PreCommit func(ctx context.Context, conn *Conn, changes []engine.RowChange) error

	// PostCommit runs after the transaction is durable.
	PostCommit func()

	// Rollback runs when a transaction (or a failed autocommit statement) is
	// discarded instead of committed.
	Rollback func()

	// HandOff decides a commit whose PreCommit returned ErrHandOff: the
	// transaction has been discarded, as a refused one is, and HandOff's error
	// is the commit's result -- nil reports it committed. A consumer whose
	// changes must go through someone else's log first (a consensus round)
	// takes them in PreCommit and lands them itself here.
	HandOff func() error
}

// ErrHandOff, returned by PreCommit, discards the transaction and makes the
// commit's result CaptureHooks.HandOff's.
var ErrHandOff = errors.New("driver: commit handed off")

// SetCaptureHooks installs h on this connection, enabling row-change capture
// for every engine session it opens from now on. Passing nil disables it.
// Call from a connection hook (RegisterConnectionHook), before the connection
// is used.
func (c *Conn) SetCaptureHooks(h *CaptureHooks) { c.hooks = h }

// SetCaptureGuard makes this connection's main database refuse writes from any
// connection that does not capture them, with reason as the error. It commits
// at once, into the file (engine's DB.SetCaptureGuard), so it holds for every
// program that opens it -- including one that never links the capture
// consumer, which is the one whose writes would go unseen. An empty reason
// lifts it. Outside a transaction only.
func (c *Conn) SetCaptureGuard(reason string) error {
	if c.tx != nil {
		return errors.New("driver: SetCaptureGuard inside a transaction")
	}
	db, err := c.openWriteOrCreate()
	if err != nil {
		return err
	}
	db.SetCaptureGuard(reason)
	return c.finishAutocommit(db, nil)
}

// AllowUncapturedWrites lets this connection write a guarded database without
// capture hooks: a consumer's own bookkeeping handle, whose writes are not the
// application's. A connection with capture hooks needs no call.
func (c *Conn) AllowUncapturedWrites() { c.uncapturedWriter = true }

// captureLock is the write lock for this connection's session of path: empty,
// or the guard's reason it may not write db. The exemption is for the
// connection's own main file only: capture covers nothing else, so a guarded
// database ATTACHed to a capturing connection is locked like any other.
func (c *Conn) captureLock(db *engine.DB, path string) string {
	if (c.hooks != nil || c.uncapturedWriter) && path == c.path {
		return ""
	}
	return db.CaptureGuard()
}

// SetRowidRange makes this connection's auto-assigned rowids come from [lo, hi)
// in the main database (engine's DB.SetRowidRange); an explicit rowid is
// untouched. replication gives each site a range, so two nodes inserting while apart
// never assign the same rowid.
func (c *Conn) SetRowidRange(lo, hi uint64) { c.rowidLo, c.rowidHi = lo, hi }

// SetRowidFloor gives that range a floor per main table (engine's
// DB.SetRowidFloor): fn(name) is the largest rowid the range ever handed out
// for the table, so an id whose row was deleted is not handed out again.
func (c *Conn) SetRowidFloor(fn func(table string) uint64) { c.rowidFloor = fn }

// ConnectionHookFn is called with every newly opened connection and the DSN it
// was opened with. Returning an error fails the connection.
type ConnectionHookFn func(conn *Conn, dsn string) error

var connHooks struct {
	mu sync.RWMutex
	fn []ConnectionHookFn
}

// RegisterConnectionHook registers fn to run on every connection opened
// AFTERWARDS (connections already open are unaffected). Hooks run in
// registration order.
func RegisterConnectionHook(fn ConnectionHookFn) {
	connHooks.mu.Lock()
	connHooks.fn = append(connHooks.fn, fn)
	connHooks.mu.Unlock()
}

// runConnectionHooks applies every registered hook to a fresh connection.
func runConnectionHooks(c *Conn, dsn string) error {
	connHooks.mu.RLock()
	fns := connHooks.fn
	connHooks.mu.RUnlock()
	for _, fn := range fns {
		if err := fn(c, dsn); err != nil {
			return err
		}
	}
	return nil
}

// GoValue converts one engine column value into the plain Go value a
// database/sql caller would Scan it as (nil/int64/float64/string/[]byte) --
// what a capture consumer wants for the values in a RowChange.
func GoValue(v engine.Value) any { return engineValueToDriver(v) }

// capturing reports whether this connection has capture hooks installed.
func (c *Conn) capturing() bool { return c.hooks != nil }

// enableCapture turns on row-change recording for a freshly opened session.
func (c *Conn) enableCapture(db *engine.DB) {
	if c.hooks != nil {
		db.EnableRowChangeCapture()
	}
}

// commitSession runs the pre-commit hook (if any), commits db, then runs the
// post-commit hook. It is the single commit point every write path on this
// Conn goes through -- autocommit statement, Tx.Commit, and a SQL-text
// COMMIT alike.
func (c *Conn) commitSession(db *engine.DB) error {
	// The backstop for sqlite3VdbeCheckFk at COMMIT. Every path that commits a
	// session this connection was HOLDING runs it first, so that a violation
	// can leave the transaction OPEN as C SQLite's failed COMMIT does; this
	// catches a path that forgets. For an autocommit session it is always zero
	// -- fkFinishStatement checked and cleared the counters at the statement's
	// own end -- so it costs one comparison there.
	if err := db.CheckDeferredForeignKeys(); err != nil {
		return err
	}
	// THE HELD SEGMENT SESSION commits by appending a delta batch, not by Close --
	// see driver/session.go. Its pre-commit hook must PEEK the row changes rather
	// than take them: the delta is the other consumer of that same log, and taking
	// them here left it with nothing to append, which is data loss and not a
	// missed notification.
	// ANY held session -- main's or an ATTACHed database's (sessionFor) -- commits
	// by appending a batch, not by Close.
	if nw := c.sessionOwning(db); nw != nil {
		return c.commitHeldSession(nw, db)
	}
	// Every database this driver hands out belongs to a session (sessionFor), so
	// one that no session owns is a stale handle -- its session was replaced, and
	// what it holds is a transaction nothing can commit.
	return errors.New("sqlite: commit of a database no session owns")
}

// handOff is a refused commit's result: err itself, or for ErrHandOff the
// HandOff hook's, the transaction being discarded already.
func (c *Conn) handOff(err error) error {
	if !errors.Is(err, ErrHandOff) {
		return err
	}
	if c.hooks.HandOff == nil {
		return errors.New("driver: PreCommit handed a commit off with no HandOff hook")
	}
	return c.hooks.HandOff()
}

// runPreCommit runs the PreCommit hook with Conn.Exec routed into db, the
// session being committed. The hook's SQL is bookkeeping, so changes(),
// total_changes() and last_insert_rowid() come out of it as the user's
// statements left them -- as C keeps a trigger program's writes out of them:
// OP_Program saves them (vdbe.c:7569-7571) and sqlite3VdbeFrameRestore puts them
// back (vdbeaux.c:2821-2823). Without this, LastInsertId after an INSERT on a
// replicated database was the rowid of replication's own op-log row.
func (c *Conn) runPreCommit(db *engine.DB, changes []engine.RowChange) error {
	ch, tot, last, totOpaque, lastOpaque := db.ConnState()
	cCh, cTot, cLast, cTotOpaque, cLastOpaque, cIns := c.changes, c.totalChanges, c.lastInsertRowid, c.totalChangesOpaque, c.lastRowidOpaque, c.rowsInserted
	prev := c.tx
	c.tx = db
	err := c.hooks.PreCommit(context.Background(), c, changes)
	c.tx = prev
	db.SetConnState(ch, tot, last, totOpaque, lastOpaque)
	c.changes, c.totalChanges, c.lastInsertRowid, c.totalChangesOpaque, c.lastRowidOpaque, c.rowsInserted = cCh, cTot, cLast, cTotOpaque, cLastOpaque, cIns
	return err
}

// commitHeldSession is commitSession for the connection's own segment session: the
// hook sees the changes, then the session appends them as one batch.
func (c *Conn) commitHeldSession(nw *engine.Session, db *engine.DB) error {
	// ONLY a session capture was enabled on -- main's (openWriteOrCreatePath). An
	// ATTACHed database's held session logs its rows too, for its own delta, and
	// those reached the hook looking exactly like main's.
	if c.hooks != nil && c.hooks.PreCommit != nil && db.RowChangeCaptureEnabled() {
		if changes := db.PeekRowChanges(); len(changes) > 0 {
			err := c.runPreCommit(db, changes)
			if err != nil {
				if derr := c.discardSession(db); derr != nil {
					if errors.Is(err, ErrHandOff) {
						return errors.Join(derr, c.handOff(err))
					}
					return derr
				}
				// The statements SUCCEEDED, so their rows are in the row stores and
				// the engine undid nothing; dropping the change log only stopped the
				// append. Left there, the connection read a row that was never
				// committed, and the next catalog rewrite -- which writes from the
				// row stores -- would have made it durable. Rebuilt from the file at
				// the next statement, as a failed commit is below.
				nw.Poison()
				return c.handOff(err)
			}
		}
	}
	// END THE ENGINE TRANSACTION FIRST when one is open. A segment-backed session commits
	// by appending a batch, and it refuses to do so while a transaction is open --
	// correctly, because that is what defers an autocommit statement's batch. So a
	// COMMIT has to close the transaction before the append, or the batch is never
	// written and the whole transaction is silently dropped.
	if nw.InTransaction() {
		if cerr := db.Exec(`COMMIT`); cerr != nil {
			return cerr
		}
	}
	if _, err := nw.Commit(); err != nil {
		// THE STATEMENT MUST LEAVE NOTHING BEHIND when its commit fails, and the
		// only way to guarantee that without the lock it just failed to get is to
		// THROW THE SESSION AWAY.
		//
		// A held session keeps the rows it applied, so an autocommit statement whose
		// commit timed out on the write lock is reported as failed to its caller and
		// then COMMITTED by the next statement on that connection -- this project's
		// "halted statement still publishes" class, reading here as a LOST row
		// rather than a phantom one: N4's ledger replay kept a row the concurrent
		// run had deleted, because the DELETE that deleted it answered busy and was
		// therefore never recorded, while its kill record rode out on the next
		// commit ("append recs=[1 2] kill0=true" on a statement that only inserted
		// 2). Left in, it also makes busyRetry's retry wrong: the retry re-runs the
		// statement over rows attempt 1 already applied, so an INSERT comes back a
		// UNIQUE violation where C SQLite simply succeeds.
		//
		// Marked rather than rebuilt HERE, because rebuilding READS the file and so
		// takes the very lock whose unavailability caused this -- the first fix
		// attempted did that and the discard itself failed busy, leaving the rows
		// exactly where they were. engine.Session.Poison defers it to the next
		// statement's freshness check, by which time the lock is free; until then
		// nothing can observe the state, because every statement path -- read and
		// write alike -- goes through ensureSession first.
		//
		// This is the single commit point for the held session, so every caller --
		// autocommit, Tx.Commit, a SQL-text COMMIT, a RETURNING statement -- is
		// covered here. Each one clears c.tx before calling, so none keeps the
		// *engine.DB this drops; the connection state (changes(), total_changes(),
		// last_insert_rowid()) was read back by noteConnState and is re-seeded into
		// the next session by Conn.session.
		nw.Poison()
		c.notifyRollback()
		return err
	}
	db.EndHeldTransaction() // the session outlives the transaction; see engine.EndHeldTransaction
	if c.hooks != nil && c.hooks.PostCommit != nil {
		c.hooks.PostCommit()
	}
	return nil
}

// discardSession rolls a session back and notifies the capture consumer.
func (c *Conn) discardSession(db *engine.DB) error {
	if nw := c.sessionOwning(db); nw != nil {
		// The HELD segment session is not closed by a discard -- it outlives the
		// statement. Two things make the discard real:
		//
		//   - the engine's own ROLLBACK, when a transaction is open. That is what
		//     undoes changes already applied to the row stores; dropping the change
		//     LOG alone leaves the rows in place, and a rolled-back INSERT then
		//     stayed visible and was committed by the next statement
		//     (TestCaptureExplicitTxAtomic caught exactly that: u3 survived).
		//   - dropping the pending change log, so nothing is appended.
		//
		// Outside a transaction there is nothing to roll back: the engine has
		// already undone the failed statement itself (statement-level atomicity),
		// which TestSegmentAutocommitFailureLeavesNothingBehind pins.
		if nw.InTransaction() {
			if rerr := db.Exec(`ROLLBACK`); rerr != nil {
				return rerr
			}
		}
		db.TakeRowChanges()
		db.EndHeldTransaction()
		nw.EndWriteTxn() // a BEGIN IMMEDIATE's lock ends with its transaction
		c.notifyRollback()
		return nil
	}
	err := db.Discard()
	c.notifyRollback()
	return err
}

func (c *Conn) notifyRollback() {
	if c.hooks != nil && c.hooks.Rollback != nil {
		c.hooks.Rollback()
	}
}

// Exec runs one statement on this connection with positional arguments,
// for a capture hook to write its own rows with (see CaptureHooks.PreCommit).
// It is a plain Go method, not part of any database/sql driver interface: it
// routes through exactly the same path a db.Exec would, so during PreCommit
// it lands in the transaction being committed.
func (c *Conn) Exec(ctx context.Context, sqlText string, args ...any) error {
	info, err := engine.ParseParamInfo(sqlText)
	if err != nil {
		return err
	}
	eargs := make([]engine.Value, info.NumParams)
	for i, a := range args {
		if i >= len(eargs) {
			break
		}
		v, cerr := driverValueToEngine(a)
		if cerr != nil {
			return cerr
		}
		eargs[i] = v
	}
	_, err = c.execArgs(sqlText, eargs)
	return err
}

// Query is Exec for a statement that returns rows: it runs on the same path a
// db.Query would -- so during PreCommit it reads the transaction being
// committed -- and returns every row as plain Go values (see GoValue).
func (c *Conn) Query(ctx context.Context, sqlText string, args ...any) ([][]any, error) {
	info, err := engine.ParseParamInfo(sqlText)
	if err != nil {
		return nil, err
	}
	eargs := make([]engine.Value, info.NumParams)
	for i, a := range args {
		if i >= len(eargs) {
			break
		}
		v, cerr := driverValueToEngine(a)
		if cerr != nil {
			return nil, cerr
		}
		eargs[i] = v
	}
	rows, err := c.queryArgs(sqlText, eargs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for {
		dest := make([]driver.Value, len(rows.Columns()))
		if err := rows.Next(dest); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		row := make([]any, len(dest))
		for i, v := range dest {
			row[i] = v
		}
		out = append(out, row)
	}
}
