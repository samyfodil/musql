// This file implements ATTACH/DETACH DATABASE and the routing that turns a
// single-file engine (engine/) into a multi-database connection.
//
// The engine is strictly one database per open file. A connection's attached
// databases therefore live HERE, as a list of (name -> file) bindings on the
// Conn, and every statement is ROUTED -- whole -- to the single database it
// references: the Conn opens an engine session against THAT database's file
// (with its attach name set as the engine's local schema, so a written
// qualifier like "aux.t" resolves), runs the statement unchanged, and tears it
// down, exactly as the autocommit path already does for the main file. This
// reuses all of the engine's verified single-file query/write machinery.
//
// What is byte-exact: ATTACH/DETACH themselves; schema-qualified and unqualified
// resolution across main + attached databases (SQLite's search order: main,
// then attached in attach order, then temp); and any SELECT/DML/DDL that reads
// or writes tables belonging to ONE database (main OR a single attached one).
//
// What is DECLINED cleanly (an error, never a wrong answer): a genuine
// CROSS-database statement -- one whose referenced tables span two or more
// databases (e.g. "SELECT ... FROM main.a JOIN aux.b", or a DML that copies
// between databases) -- because the engine has no cross-file join/write
// machinery; and ANY attached-database access while an explicit transaction is
// open, because this driver models a transaction as a single held engine
// session on the main file (see Conn's doc comment) and cannot span the atomic
// commit across multiple files. Both are surfaced as errors so they can never
// be a silent wrong result.

package driver

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// attachedDB is one ATTACH binding on a Conn.
type attachedDB struct {
	name  string // the attach schema name (case preserved; matched case-insensitively)
	path  string // backing file the engine opens for this database
	isMem bool   // true for ATTACH ':memory:' -- path is a private temp file, deleted on DETACH/Close
}

// handleAttachDetach intercepts an "ATTACH ... AS name" / "DETACH name"
// statement and mutates the Conn's attached set. handled is false for any other
// statement (the caller proceeds with normal dispatch). ATTACH/DETACH are
// permitted while a transaction is open (matching modern SQLite, which no
// longer restricts them) -- they touch only the connection-level registry, not
// the held transaction session.
func (c *Conn) handleAttachDetach(sqlText string) (res driver.Result, handled bool, err error) {
	if path, name, ok, perr := engine.ParseAttachStmt(sqlText); ok {
		if perr != nil {
			return nil, true, perr
		}
		// engine.ParseAttachStmt resolves a SHARED "file:/name?vfs=memdb" URI
		// to a sentinel string carrying the memdb NAME, not a real filesystem
		// path (see engine.IsMemdbAttachName's own doc comment) -- it must be
		// checked and handled BEFORE path ever reaches c.attach, which treats
		// its argument as a literal path to open/create. The engine-direct
		// ATTACH path (engine/attach.go's execAttach) resolves it through a
		// process-wide registry; that registry, and the read-model reasoning
		// behind what it can and cannot make sound, is specific to
		// execAttach's single ReadOnlyPager-per-binding model (see
		// memdb_registry.go's package comment) and does not carry over
		// automatically to this driver's own per-statement routing (attach()
		// below, and route()/openWriteOrCreatePath elsewhere in this
		// package) -- so this connection declines cleanly rather than
		// silently opening a file literally named after the memdb path.
		if memName, isMemdb := engine.IsMemdbAttachName(path); isMemdb {
			return nil, true, fmt.Errorf("driver: ATTACH URI vfs=memdb (%q): shared in-RAM database sharing is implemented for engine-direct ATTACH (engine/attach.go) only, not yet through this driver's own cross-database routing", memName)
		}
		// Same reasoning, same decline, for the OTHER shared in-RAM ATTACH
		// form: engine.ParseAttachStmt resolves "file:X?mode=memory&cache=
		// shared" to its own sentinel (engine.IsSharedMemAttachName's doc
		// comment), and it too is wired only through execAttach's process-wide
		// registry, not through this driver's per-statement file routing.
		if sharedName, isSharedMem := engine.IsSharedMemAttachName(path); isSharedMem {
			return nil, true, fmt.Errorf("driver: ATTACH URI mode=memory&cache=shared (%q): shared in-RAM database sharing is implemented for engine-direct ATTACH (engine/attach.go) only, not yet through this driver's own cross-database routing", sharedName)
		}
		if err := c.attach(name, path); err != nil {
			return nil, true, err
		}
		return &execResult{lastInsertID: c.lastInsertRowid}, true, nil
	}
	if name, ok, perr := engine.ParseDetachStmt(sqlText); ok {
		if perr != nil {
			return nil, true, perr
		}
		if err := c.detach(name); err != nil {
			return nil, true, err
		}
		return &execResult{lastInsertID: c.lastInsertRowid}, true, nil
	}
	return nil, false, nil
}

func (c *Conn) attach(name, path string) error {
	if r33sIdentEq(name, "main") || r33sIdentEq(name, "temp") {
		return fmt.Errorf("driver: database %s is already in use", name)
	}
	for _, a := range c.attached {
		if r33sIdentEq(a.name, name) {
			return fmt.Errorf("driver: database %s is already in use", name)
		}
	}
	ad := &attachedDB{name: name}
	if r33sIdentEq(path, ":memory:") || path == "" {
		// An in-memory attached database is private to this connection and
		// non-durable. A private temp file gives byte-identical OBSERVABLE
		// behavior (this connection sees its own writes; no other connection
		// ever opens this path) while reusing the ordinary file machinery; it
		// is removed on DETACH / Conn.Close.
		f, err := os.CreateTemp("", "musql-attach-mem-*.musq")
		if err != nil {
			return fmt.Errorf("driver: ATTACH ':memory:': %w", err)
		}
		f.Close()
		ad.path = f.Name()
		ad.isMem = true
	} else {
		ad.path = path
		// C OPENS the file at ATTACH time (SQLITE_OPEN_CREATE), so a path it
		// cannot create is an error THERE, not on first use: attach.c:255-269
		// closes the half-open b-tree and reports "unable to open database:
		// %s", with the VFS's own errno text appended. This driver only stored
		// the path, so "ATTACH '/nonexistent/dir/x.db' AS z" reported SUCCESS
		// where 3.53.3 says "unable to open database:
		// /nonexistent/dir/x.db: no such file or directory" -- accepting where
		// C rejects, which is worse than any message difference.
		if err := attachPathOpenable(path); err != nil {
			return err
		}
	}
	if err := c.checkAttachEncoding(ad); err != nil {
		return err
	}
	c.attached = append(c.attached, ad)
	return nil
}

// checkAttachEncoding enforces SQLite's same-text-encoding rule for ATTACH
// (attach.c:207-211). A brand-new or zero-length file has no format yet and
// adopts the attaching connection's encoding instead of being rejected.
func (c *Conn) checkAttachEncoding(ad *attachedDB) error {
	auxEnc, auxInit, err := engine.FileTextEncoding(ad.path)
	if err != nil || !auxInit {
		// Unreadable here means "let the ordinary open path report it" -- this
		// check must never be the thing that turns a real error into a
		// different one. An uninitialized file adopts main's encoding.
		return nil
	}
	mainEnc, _, err := engine.FileTextEncoding(c.path)
	if err != nil {
		return nil
	}
	if auxEnc != mainEnc {
		return errors.New("attached databases must use the same text encoding as main database")
	}
	return nil
}

// attachPathOpenable tests whether a path can be opened for ATTACH (attachment.c).
// It tries read-write first, then read-only if that fails, creating the file
// when absent. URIs are left alone.
func attachPathOpenable(path string) error {
	if strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?") {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		// A read-only FILE still attaches in C (it retries the open read-only),
		// but a DIRECTORY does not -- "unable to open database: /tmp: is a
		// directory" -- and a bare os.Open succeeds on one, which is why the
		// retry has to insist on a regular file rather than just on opening.
		if f2, err2 := os.Open(path); err2 == nil {
			st, serr := f2.Stat()
			f2.Close()
			if serr == nil && st.Mode().IsRegular() {
				return nil
			}
		}
		msg := err.Error()
		var pe *os.PathError
		if errors.As(err, &pe) {
			msg = pe.Err.Error() // just the errno text, as C's VFS reports it
		}
		return fmt.Errorf("driver: unable to open database: %s: %s", path, msg)
	}
	f.Close()
	return nil
}

func (c *Conn) detach(name string) error {
	if r33sIdentEq(name, "temp") && !c.tempOpened {
		// C's detach loop SKIPS a database whose b-tree is not open --
		// "if( pDb->pBt==0 ) continue" (attach.c:310-311) -- and the temp
		// database has none until something opens it, so the name is simply not
		// found: "no such database: temp". Once anything HAS opened it, i==1
		// and the next test (i<2) gives "cannot detach database temp".
		//
		// Conn.tempOpened is that same bit, and its boundary is the same one:
		// measured against 3.53.3, a temp TABLE or VIEW opens it and so does a
		// plain read of temp.sqlite_master, while "PRAGMA temp_store=2" -- only
		// a preference -- does not.
		return fmt.Errorf("driver: no such database: %s", name)
	}
	if r33sIdentEq(name, "main") || r33sIdentEq(name, "temp") {
		return fmt.Errorf("driver: cannot detach database %s", name)
	}
	for i, a := range c.attached {
		if r33sIdentEq(a.name, name) {
			c.attached = append(c.attached[:i], c.attached[i+1:]...)
			if a.isMem {
				// An in-memory database ends here: its session first (which
				// holds its mapping and its lock), then every file of it.
				if nw := c.ndbs[a.path]; nw != nil {
					nw.Close()
					delete(c.ndbs, a.path)
				}
				engine.RemoveDatabaseFiles(a.path)
			}
			return nil
		}
	}
	return fmt.Errorf("driver: no such database: %s", name)
}

// cleanupAttached forgets the attachments, noting the private temp files
// backing ':memory:' ones for Close to remove once their sessions have closed.
func (c *Conn) cleanupAttached() {
	for _, a := range c.attached {
		if a.isMem {
			c.memGone = append(c.memGone, a.path) // removed once its session closes
		}
	}
	c.attached = nil
}

// errCrossDB is the sentinel decline for a statement whose referenced tables
// span more than one database, or which touches an attached database while a
// transaction is open. It is returned as an ordinary error so the statement
// fails cleanly rather than producing a wrong result.
var errCrossDB = errors.New("driver: cross-database statement is not supported (each statement must reference a single database)")

// route decides which database file a statement runs against, and the schema
// name the engine should answer to for that file. When no databases are
// attached it is the fast, zero-overhead path: the main file, unchanged. With
// attachments it resolves the statement's referenced tables to their owning
// databases (SQLite search order for unqualified names) and returns that single
// database -- or errCrossDB if the statement spans several, or touches an
// attached database inside an open transaction.
func (c *Conn) route(sqlText string) (path, schema, execSQL string, err error) {
	if len(c.attached) == 0 {
		return c.path, "", sqlText, nil
	}

	// Determine the set of databases the statement references.
	refs, understood, perr := engine.ReferencedTables(sqlText)
	if perr != nil {
		// A genuine parse error: run it against main so the engine reports the
		// same failure it would without any attachment.
		return c.path, "", sqlText, nil
	}

	type target struct{ path, schema string }
	var targets []target
	seen := map[string]bool{}
	addTarget := func(p, s string) {
		key := r33sFoldIdent(s)
		if key == "" {
			key = "main"
		}
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, target{p, s})
	}

	execSQL = sqlText
	if !understood {
		// Not a SELECT/INSERT/UPDATE/DELETE. A CREATE/DROP/ALTER names a single
		// target database (StatementTargetSchema); everything else (PRAGMA, ...)
		// runs against main.
		if sch, isDDL := engine.StatementTargetSchema(sqlText); isDDL {
			p, s, ok := c.dbForSchema(sch)
			if !ok {
				return c.path, "", sqlText, nil // unknown qualifier: let main's engine error
			}
			addTarget(p, s)
			// When a DDL statement is routed to an ATTACHed database, the owning
			// file is already chosen; strip the now-redundant "<name>." target
			// qualifier so the engine's single-file DDL parser (which accepts
			// only an unqualified / main / temp target) runs it unchanged.
			if s != "" {
				if stripped, ok := engine.StripDDLTargetSchema(sqlText); ok {
					execSQL = stripped
				}
				// ...and CREATE TRIGGER's ON clause may repeat the same
				// qualifier, which the delegated session cannot resolve.
				// See engine.StripCreateTriggerOnSelfQualifier.
				execSQL = engine.StripCreateTriggerOnSelfQualifier(execSQL, s)
				// A qualifier still standing after that names a DIFFERENT
				// database, which a non-TEMP trigger may not reference.
				if terr := engine.CreateTriggerCrossSchemaError(execSQL, s, engine.StatementIsTempTrigger(sqlText)); terr != nil {
					return "", "", "", terr
				}
			}
		} else {
			addTarget(c.path, "")
		}
	} else {
		for _, r := range refs {
			p, s := c.ownerOf(r)
			addTarget(p, s)
		}
	}

	switch len(targets) {
	case 0:
		path, schema = c.path, "" // no resolvable base table (e.g. a CTE-only or FROM-less query): main
	case 1:
		path, schema = targets[0].path, targets[0].schema
	default:
		// Several databases in one statement. Outside a transaction the
		// caller runs it against a purpose-built multi-database snapshot
		// (crossDBQuery / crossDBExec). INSIDE one, a READ already has such
		// a surface -- the transaction's own snapshot with every attachment
		// wired onto it (withAttachedReaders) -- so route it to main and let
		// the engine resolve each FROM item to its owner. A cross-database
		// WRITE still needs the cross-file atomic commit this driver has no
		// super-journal for, and keeps declining.
		if c.tx != nil && isSelectStmt(sqlText) {
			path, schema = c.path, ""
			break
		}
		return "", "", "", errCrossDB
	}

	// A transaction is a single held engine session on the main file; it cannot
	// span an attached database's separate file/commit. A WRITE to one
	// mid-transaction is declined rather than risking a non-atomic result --
	// there is no super-journal here.
	//
	// A READ is not that. queryArgs' held-transaction branch already wires
	// every attachment onto the transaction's own snapshot as a read pager
	// (withAttachedReaders) and the engine routes each FROM item to the file
	// that owns it, which is exactly how an UNQUALIFIED read of the same
	// table already resolves inside a transaction. Declining it meant
	// "BEGIN; SELECT count(*) FROM aux.t" -- a statement C SQLite answers
	// -- was an error, and so was every qualified read in between.
	if c.tx != nil && path != c.path {
		if !isSelectStmt(sqlText) {
			return "", "", "", errCrossDB
		}
		path, schema = c.path, ""
	}
	return path, schema, execSQL, nil
}

// openCrossDBReaders opens a read-only ReadOnlyPager over the main file
// and every ATTACHed database, wiring attached ones onto the primary via
// SetAttachedReaders so the engine can route each FROM item to the correct file.
// closeAll must be called by the caller (deferred).
func (c *Conn) openCrossDBReaders() (primary *engine.ReadOnlyPager, closeAll func(), err error) {
	if err := c.ensureFileExistsPath(c.path); err != nil {
		return nil, nil, err
	}
	// The held session's own reader, not a fresh open: main is a segment
	// database, and the session's pager answers this connection's uncommitted rows.
	nw, nerr := c.sessionFor(c.path)
	if nerr != nil {
		return nil, nil, nerr
	}
	primary, err = c.readPagerStamped(nw)
	if err != nil {
		return nil, nil, err
	}
	closeAttached, aerr := c.wireAttachedReaders(primary)
	if aerr != nil {
		primary.Close()
		return nil, nil, aerr
	}
	return primary, func() { closeAttached(); primary.Close() }, nil
}

// attachedReaderTarget is anything the ATTACHed databases can be wired onto:
// a read pager or a write session.
type attachedReaderTarget interface {
	SetAttachedReaders(names []string, pagers []*engine.ReadOnlyPager)
	// SetAttachedDatabases is SetAttachedReaders plus each database's file and
	// memory-ness, which "PRAGMA database_list" reports
	// (engine/pragma_database_list.go).
	SetAttachedDatabases(dbs []engine.AttachedDatabase)
	// OwnsTempDatabase reports whether the target already reaches this
	// connection's TEMP database itself -- a write session does, and its own
	// reader carries the current statement's uncommitted temp pages, which a
	// reader opened over the temp FILE would silently replace with whatever
	// was last committed to it.
	OwnsTempDatabase() bool
}

// wireAttachedReaders opens a read-only ReadOnlyPager over every ATTACHed
// database and wires them onto primary, routing each FROM item to the correct
// file. closeAll must be called by the caller (deferred).
func (c *Conn) wireAttachedReaders(primary attachedReaderTarget) (closeAll func(), err error) {
	var opened []*engine.ReadOnlyPager
	cleanup := func() {
		for _, p := range opened {
			p.Close()
		}
	}
	var dbs []engine.AttachedDatabase
	// The TEMP database goes FIRST: it is a database of this connection's own
	// (engine/temp_store.go), with its own file and its own catalog, and an
	// unqualified name resolves against it before main. A connection that never
	// made a temp object has no temp file and adds nothing here.
	if c.temp != nil && !primary.OwnsTempDatabase() {
		// From the SESSION, not from the temp file: the file lags everything this
		// session has written to temp and not yet flushed, and the format it is in
		// is the engine's own business. See engine.Session.TempPager.
		tnw, terr := c.sessionFor(c.path)
		if terr != nil {
			cleanup()
			return nil, terr
		}
		rp, oerr := tnw.TempPager()
		if oerr != nil {
			cleanup()
			return nil, oerr
		}
		if rp != nil {
			rp.SetLocalSchema("temp")
			c.stampConnState(rp)
			opened = append(opened, rp)
			// The TEMP database reports NO file, like C SQLite's
			// (sqlite3BtreeGetFilename over a zero-named pager).
			dbs = append(dbs, engine.AttachedDatabase{Name: "temp", Pager: rp, InMemory: true})
		}
	}
	for _, a := range c.attached {
		if err := c.ensureFileExistsPath(a.path); err != nil {
			cleanup()
			return nil, err
		}
		anw, aerr := c.sessionFor(a.path)
		if aerr != nil {
			cleanup()
			return nil, aerr
		}
		rp, oerr := anw.ReadPager()
		if oerr != nil {
			cleanup()
			return nil, oerr
		}
		rp.SetLocalSchema(a.name)
		rp.SetInMemory(a.isMem) // see memdb.go's memBacked
		c.stampConnState(rp)    // carries PRAGMA trusted_schema too; see Conn.untrustedSchema
		opened = append(opened, rp)
		dbs = append(dbs, engine.AttachedDatabase{
			Name: a.name, Pager: rp,
			// A memory-backed attachment has a private temp file underneath and
			// reports no file at all, exactly as ATTACH ':memory:' does.
			Path: a.path, InMemory: c.memBacked(a.path),
		})
	}
	primary.SetAttachedDatabases(dbs)
	return cleanup, nil
}

// withAttachedReaders wires every ATTACHed database onto target as a read
// snapshot for the duration of ONE statement. It is a no-op on a connection with
// no attachments.
//
// Every statement that runs with MAIN as its primary goes through this,
// on the read path AND the write path, because a view or trigger body can
// reference attached databases even if the statement's own SQL text does not.
// Wiring unconditionally adds reach only; route(above) has already ensured every
// table the statement names belongs to main, so the attachments are consulted
// only for lookups inside views/triggers.
func (c *Conn) withAttachedReaders(target attachedReaderTarget) (release func(), err error) {
	// Nothing to wire is a genuine no-op, and it has to be: SetAttachedReaders
	// REPLACES the target's reader list, so wiring an empty one onto a write
	// session's snapshot would take away the TEMP database the session put
	// there itself (SnapshotPager) -- which is why OwnsTempDatabase is part of
	// the test and not just "c.temp == nil".
	if len(c.attached) == 0 && (c.temp == nil || target.OwnsTempDatabase()) {
		return func() {}, nil
	}
	closeAll, err := c.wireAttachedReaders(target)
	if err != nil {
		return nil, err
	}
	return func() {
		target.SetAttachedReaders(nil, nil)
		closeAll()
	}, nil
}

// crossDBQuery runs a genuine cross-database read -- a SELECT whose FROM items
// span two or more databases (e.g. "SELECT * FROM main.a JOIN aux.b ON ...") --
// by opening a reader per database (openCrossDBReaders) and running the
// statement once against the primary, whose resolveFrom routes each table to
// its owning file. It is attempted by queryArgs ONLY when route declined the
// statement as cross-database AND no explicit transaction is open (a
// cross-database read mid-transaction stays declined: this driver models a
// transaction as one held session on the main file). A cross-database shape the
// engine cannot serve (a view whose body itself crosses databases, ...) surfaces
// as an ordinary error from QueryArgs -- never a wrong result.
func (c *Conn) crossDBQuery(sqlText string, args []engine.Value) (driver.Rows, error) {
	primary, closeAll, err := c.openCrossDBReaders()
	if err != nil {
		return nil, err
	}
	defer closeAll()
	cols, rows, err := primary.QueryArgs(sqlText, args)
	if err != nil {
		return nil, err
	}
	return &Rows{cols: cols, rows: rows}, nil
}

// crossDBExec runs a genuine cross-database DML statement -- one whose write
// TARGET is a single database but whose source SELECT / subquery reads span
// others: "INSERT INTO t SELECT ... FROM other.s", "UPDATE t SET x=(SELECT ..
// FROM other.s)", "DELETE FROM t WHERE .. IN (SELECT FROM other.s)". A single
// INSERT/UPDATE/DELETE writes only its own target table, so exactly ONE database
// is ever written; every other referenced database is a read. It opens the
// target for writing, wires each OTHER referenced database onto it as a read
// snapshot (SetAttachedReaders, so the write path's own execSelect routes each
// foreign table to the reader that owns it -- see engine/cross_db.go), runs the
// statement, and commits (autocommit) -- the exact mirror of execArgs's
// autocommit path, extended with the read snapshots.
//
// It is attempted by execArgs ONLY when route declined the statement as
// cross-database AND no explicit transaction is open. A cross-database write
// INSIDE an explicit transaction is DECLINED (route returns errCrossDB and
// execArgs surfaces it): this driver models a transaction as one held session
// on the main file and cannot guarantee an atomic commit across two files, so
// -- rather than fake atomicity -- it refuses cleanly.
func (c *Conn) crossDBExec(sqlText string, args []engine.Value) (driver.Result, error) {
	refs, understood, perr := engine.ReferencedTables(sqlText)
	if perr != nil || !understood || len(refs) == 0 {
		return nil, errCrossDB // not a routable DML: keep declining
	}
	// refs[0] is the DML target (ReferencedTables appends it first); it names the
	// single database this statement writes.
	targetPath, targetSchema := c.ownerOf(refs[0])

	db, err := c.openWriteOrCreatePath(targetPath)
	if err != nil {
		return nil, err
	}
	db.SetLocalSchema(targetSchema)

	// Open a read snapshot for every OTHER referenced database.
	var opened []*engine.ReadOnlyPager
	var dbs []engine.AttachedDatabase
	closeReaders := func() {
		for _, p := range opened {
			p.Close()
		}
	}
	seen := map[string]bool{targetPath: true}
	for _, r := range refs {
		p, s := c.ownerOf(r)
		if seen[p] {
			continue
		}
		seen[p] = true
		if err := c.ensureFileExistsPath(p); err != nil {
			closeReaders()
			db.Discard()
			return nil, err
		}
		// The held session's reader for that file, like every other read on this
		// format: engine.Open cannot read a segment database at all.
		pnw, pnerr := c.sessionFor(p)
		if pnerr != nil {
			closeReaders()
			db.Discard()
			return nil, pnerr
		}
		rp, oerr := pnw.ReadPager()
		if oerr != nil {
			closeReaders()
			db.Discard()
			return nil, oerr
		}
		// The main file's ownerOf schema is "" -- but as a READER (when the write
		// target is an attached database) it must answer to the qualifier "main"
		// so a source reference like "main.dst" resolves to it. Register and set
		// its local schema as "main" in that case.
		name := s
		if name == "" {
			name = "main"
		}
		rp.SetLocalSchema(name)
		rp.SetInMemory(c.memBacked(p)) // see memdb.go's memBacked
		c.stampConnState(rp)           // carries PRAGMA trusted_schema too; see Conn.untrustedSchema
		opened = append(opened, rp)
		dbs = append(dbs, engine.AttachedDatabase{
			Name: name, Pager: rp, Path: p, InMemory: c.memBacked(p),
		})
	}
	db.SetAttachedDatabases(dbs)
	defer closeReaders()

	n, _, err := db.ExecArgs(sqlText, args)
	if err != nil {
		// Mirror execArgs's autocommit error handling: a RAISE(FAIL) trigger
		// error preserves the changes already applied (commit while reporting the
		// error); every other error discards the partial write.
		if engine.ErrorKeepsAutocommitChanges(err) {
			if cerr := c.commitSession(db); cerr != nil {
				c.discardSession(db)
			}
			return nil, err
		}
		c.discardSession(db)
		return nil, err
	}
	c.noteConnState(db) // see Conn.lastInsertRowid / engine/conn_state.go
	if err := c.commitSession(db); err != nil {
		return nil, err
	}
	return &execResult{rowsAffected: n, lastInsertID: c.lastInsertRowid}, nil
}

// ownerOf resolves one table reference to its owning database's (path, schema).
// Qualified references name their database directly; unqualified references
// resolve by SQLite's search order: main first, then each attached database
// in attach order.
func (c *Conn) ownerOf(r engine.TableRef) (path, schema string) {
	if r.Schema != "" {
		if r33sIdentEq(r.Schema, "main") || r33sIdentEq(r.Schema, "temp") {
			return c.path, ""
		}
		if p, s, ok := c.dbForSchema(r.Schema); ok {
			return p, s
		}
		return c.path, "" // unknown database: main's engine reports it
	}
	if c.dbHasTable(c.path, r.Table) {
		return c.path, ""
	}
	for _, a := range c.attached {
		if c.dbHasTable(a.path, r.Table) {
			return a.path, a.name
		}
	}
	return c.path, ""
}

// dbForSchema maps a database qualifier to (path, schema). "main"/"temp" map to
// the main file; an attached name maps to its file. ok is false for an unknown
// name.
func (c *Conn) dbForSchema(name string) (path, schema string, ok bool) {
	if r33sIdentEq(name, "main") || r33sIdentEq(name, "temp") {
		return c.path, "", true
	}
	for _, a := range c.attached {
		if r33sIdentEq(a.name, name) {
			return a.path, a.name, true
		}
	}
	return "", "", false
}

// dbHasTable reports whether the database file at path contains an ordinary
// table named tbl (case-insensitive). A missing or empty file has no tables.
func (c *Conn) dbHasTable(path, tbl string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return false
	}
	p, err := engine.Open(path)
	if err != nil {
		return false
	}
	defer p.Close()
	names, err := p.TableNames()
	if err != nil {
		return false
	}
	for _, n := range names {
		if r33sIdentEq(n, tbl) {
			return true
		}
	}
	return false
}

// r33sIdentEq and r33sFoldIdent fold identifiers the way SQLite does:
// byte-wise through sqlite3UpperToLower (global.c:24), which only maps
// 'A'-'Z' to 'a'-'z'. strings.EqualFold applies Unicode folding, which
// would incorrectly collapse other character pairs SQLite keeps distinct.
func r33sIdentEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func r33sFoldIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for ; i < len(b); i++ {
				if b[i] >= 'A' && b[i] <= 'Z' {
					b[i] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
