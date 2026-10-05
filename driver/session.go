package driver

import "github.com/samyfodil/musql/engine"

// The connection on the segment format.
//
// A segment-backed session survives the connection, allowing the plan cache
// to persist across statements. The connection maintains one held session
// for autocommit and explicit transactions, with read-your-writes and
// connection state (last_insert_rowid, changes(), total_changes) integrated
// into the engine.
//
// Pragmas that reference SQLite file features (pages, journals, logs) decline,
// since the segment format has no such structures.

// session is this connection's held write session, opened on first use.
//
// Opened lazily rather than in newConn because sql.Open does no I/O and must not
// fail for a path that is only ever used by a connection that is never used.
func (c *Conn) session() (*engine.Session, error) {
	if c.ndb != nil {
		return c.ndb, nil
	}
	nw, err := engine.OpenOrCreateWait(c.path, c.pragmaState.BusyTimeout())
	if err != nil {
		return nil, err
	}
	// Connection state the caller may already have set (a hook, or a previous
	// session on this Conn) carries into the session that now owns it.
	nw.SetConnState(c.changes, c.totalChanges, c.lastInsertRowid,
		c.totalChangesOpaque, c.lastRowidOpaque)
	// ...and the reverse for corruptSchema's latch: a load can SET it (a catalog
	// whose rootpages collide, engine's replayRootEdits), and the Conn mirror is
	// what every later statement is refused from and stamped back with.
	if obj, detail := nw.SchemaCorrupt(); obj != "" {
		c.schemaCorruptObj, c.schemaCorruptDetail = obj, detail
	}
	c.ndb = nw
	return nw, nil
}

// sessionDB is the held session's write handle -- what every storage-acquiring
// path in Conn now returns instead of opening a throwaway.
//
// It is deliberately NOT stored in c.tx. Throughout Conn, "c.tx != nil" means AN
// EXPLICIT TRANSACTION IS OPEN, and dozens of branches read it that way: putting
// the permanent session there made the driver believe a transaction was always
// open, and a plain BEGIN answered "cannot start a transaction within a
// transaction".
func (c *Conn) sessionDB() (*engine.DB, error) {
	nw, err := c.session()
	if err != nil {
		return nil, err
	}
	// REVALIDATED HERE, not only at the top of a statement. busyRetry re-runs its
	// whole function on ErrBusy and that function acquires the session through
	// THIS call, so a retry that skipped the check ran against the state the
	// failed attempt left -- a poisoned session still holding the rows whose
	// commit failed, which made the retry of an INSERT report "UNIQUE constraint
	// failed" for a row only the failed attempt had put there. Every acquisition
	// point revalidates; RefreshIfStale is two stat calls when nothing moved.
	if rerr := c.refreshOnce(nw); rerr != nil {
		return nil, rerr
	}
	return nw.DB, nil
}

// refreshOnce is RefreshIfStale at most once per statement. See
// Conn.refreshedThisStmt for why once is the whole of the guarantee.
func (c *Conn) refreshOnce(nw *engine.Session) error {
	if c.refreshedThisStmt {
		return nil
	}
	// PRAGMA locking_mode=exclusive takes its lock at the first file access,
	// per engine.Session.Commit. Hold a SHARED lock before refresh so no other
	// writer moves the state being read. Statements that don't access the main
	// file (pragma itself, getter, BEGIN) are excluded (see mainReadTxnOf).
	access := c.lockingMain && nw.AccessesMainFile(c.stmtText)
	if access {
		if err := nw.HoldLockingMode(false); err != nil {
			return err
		}
	} else if !c.lockingMain && nw.LockingModeHeld() {
		nw.ReleaseLockingMode()
	}
	if _, err := nw.RefreshIfStale(); err != nil {
		return err
	}
	if access && engine.SegmentFileJournalWAL(c.path) {
		if err := nw.HoldLockingMode(true); err != nil {
			return err
		}
	}
	c.refreshedThisStmt = true
	return nil
}

// ensureSession opens the held session if it is not open yet, and REVALIDATES it
// if another connection has committed since this one last looked.
//
// The revalidation is not optional. A held session keeps its loaded rows, which is
// the whole reason it is fast -- and the first time it can therefore be STALE. Two
// connections on one file each holding a session saw none of each other's commits:
// the replication layer writes its oplog through one *sql.DB and reads it through
// another, and the reader found an empty log while the writer's own connection saw
// the row. See engine's Session.RefreshIfStale.
func (c *Conn) ensureSession() error {
	nw, err := c.session()
	if err != nil {
		return err
	}
	return c.refreshOnce(nw)
}

// commitIfAutocommit is the autocommit rule, applied after a statement: no
// explicit BEGIN open means the statement is its own transaction and its batch is
// appended now.
//
// The ENGINE decides, because the engine processed the BEGIN.
func (c *Conn) commitIfAutocommit() error {
	if c.ndb == nil {
		return nil
	}
	_, err := c.ndb.CommitIfAutocommit()
	return err
}

// readPagerStamped is the session's read pager with this connection's carried
// state put onto it -- which is not optional: "SELECT changes()" and the other
// three connection-state functions are answered on the READ side, so a pager
// that never heard about them reports zeroes. That was
// TestAbortedReturningPublishesChanges reading changes() = 0 where it must be 1.
//
// stampConnState carries more than the counters (trusted_schema,
// ignore_check_constraints, automatic_index, the column-name pragmas, the tuning
// values); every one of them is consulted on the read side and every one is a
// WRONG ANSWER rather than a gap if it is missing. See its own doc comment.
func (c *Conn) readPagerStamped(nw *engine.Session) (*engine.ReadOnlyPager, error) {
	rp, err := nw.ReadPager()
	if err != nil {
		return nil, err
	}
	// stampReadPager, not a hand-picked subset of it. It carries FOURTEEN pieces
	// of connection state that only a READ answers -- foreign_keys, deferFKs,
	// max_page_count, case_sensitive_like, writable_schema, secure_delete, the two
	// journal modes, temp_store, the main path, whether temp is open, the WAL
	// autocheckpoint, and stampConnState's own counters -- and every one of them is
	// a WRONG ANSWER from a pragma getter rather than a gap if it is missing.
	//
	// Reimplementing it here is how that goes wrong: the first version of this
	// function called stampConnState alone, and "PRAGMA temp_store" reported 0 for
	// every value the connection had set.
	c.stampReadPager(rp, c.path)
	return rp, nil
}

// closeSessions commits and releases every session this connection holds:
// main's, and one per ATTACHed database.
//
// A session holds a memory mapping of its segment file and a descriptor on its
// lock file, so a connection that never released them leaks both -- and its last
// statements' batch stays uncommitted, since a session commits at Close.
func (c *Conn) closeSessions() error {
	var first error
	if c.ndb != nil {
		nw := c.ndb
		c.ndb = nil
		if cerr := nw.Close(); cerr != nil {
			first = cerr
		}
	}
	if cerr := c.closeAttachedSessions(); cerr != nil && first == nil {
		first = cerr
	}
	return first
}

// sessionFor is the held write session for one database FILE -- main's, or an
// ATTACHed one's.
//
// Every path gets the same model, which is what made ATTACH work on this format
// at all: the attached database used to be opened as a throwaway per statement
// through openWriteOrCreateFile, which wrote the SQLITE format, so "ATTACH
// 'o.musq' AS o; CREATE TABLE o.b(...)" produced a SQLite file beside a segment
// main, and the cross-database read then failed with "bad magic" -- against main,
// because that read opened main as SQLite too.
//
// Held rather than per statement for the same reason main's is (driver/session.go's
// own doc comment): a session that survives the statement keeps its catalog and
// its loaded rows. It also gives read-your-writes ACROSS databases for free,
// since the reader handed to a cross-database statement is this same session's.
func (c *Conn) sessionFor(path string) (*engine.Session, error) {
	if path == c.path {
		nw, err := c.session()
		if err != nil {
			return nil, err
		}
		if _, rerr := nw.RefreshIfStale(); rerr != nil {
			return nil, rerr
		}
		// WHERE THE TEMP DATABASE IS, which only the CONNECTION knows: it outlives
		// every session and a session opened before the first temp object has never
		// heard of it. openWriteOrCreatePath tells the session on the write path;
		// the read path did not, so a cross-database read asked the session for its
		// temp pager and got nothing -- "no such table: u" for a temp table this
		// connection had created (TestIndexedByCatalog's step 7). SetTempPath is a
		// no-op when the path is unchanged.
		nw.DB.SetTempDatabase(c.temp)
		return nw, nil
	}
	nw := c.ndbs[path]
	if nw == nil {
		var err error
		if nw, err = engine.OpenOrCreate(path); err != nil {
			return nil, err
		}
		if c.ndbs == nil {
			c.ndbs = map[string]*engine.Session{}
		}
		c.ndbs[path] = nw
	}
	// Same revalidation main's gets, and for the same reason: another connection
	// may have committed to this file since this session last looked.
	if _, rerr := nw.RefreshIfStale(); rerr != nil {
		return nil, rerr
	}
	return nw, nil
}

// sessionOwning is the session a *engine.DB belongs to, or nil if it belongs to
// none -- which is how the commit and discard paths tell a held session (commit
// by appending a batch) from anything else.
func (c *Conn) sessionOwning(db *engine.DB) *engine.Session {
	if c.ndb != nil && db == c.ndb.DB {
		return c.ndb
	}
	for _, nw := range c.ndbs {
		if nw != nil && db == nw.DB {
			return nw
		}
	}
	return nil
}

// closeAttachedSessions commits and releases every ATTACHed database's session.
func (c *Conn) closeAttachedSessions() error {
	var first error
	for path, nw := range c.ndbs {
		if cerr := nw.Close(); cerr != nil && first == nil {
			first = cerr
		}
		delete(c.ndbs, path)
	}
	return first
}

// pragmaDeclines refuses a pragma that names a mechanism this format does not
// have. The LIST lives in the engine (engine.PragmaHasNoMeaningHere), because
// the format is what makes a pragma meaningless and the engine is what knows the
// format -- the list used to live here, which left every ENGINE-DIRECT caller
// (the corpus runs that way) answering for pragmas the driver refuses. "PRAGMA
// journal_mode = DELETE" reached the rollback-journal code on a session that has
// no file to journal and came back "bad file descriptor".
func (c *Conn) pragmaDeclines(sqlText string) error {
	p, perr := engine.ParsePragma(sqlText)
	if perr != nil || p == nil {
		return nil // not a pragma at all
	}
	return engine.PragmaHasNoMeaningHere(p.Name)
}
