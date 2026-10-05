package engine

import (
	"errors"
	"maps"
	"slices"
	"strings"
)

// The TEMP database, on this engine's own format. C SQLite keeps it as
// db->aDb[1], a second database private to the connection
// (sqlite3OpenTempDatabase, build.c:2073), and so does this engine -- but it
// is NOT a SQLite-format file. RULE #3: the engine does not execute against
// that format, and a TEMP database stored in it was the one place the engine
// still did, at run time, for every connection that made a temp table.
//
// What it is instead is C's "temp_store = MEMORY" (pragma.c's
// invalidateTempStorage / sqlite3TempInMemory): the temp objects live in the
// session's own schema with isTemp set, exactly as before, and their rows live
// in the same in-memory row stores a session holds main's live rows in
// -- so a temp table's INSERT, UPDATE, savepoint and ROLLBACK go through the
// row store's own undo log (row_store_undo.go) and need nothing of their own.
// Reads see the temp database through a read pager built over those
// stores (tempReader), with sqlite_temp_master rendered from the objects the
// way sqlite_master is (buildSchemaSegment).
//
// The temp database belongs to the CONNECTION. A driver hands it from one
// session to the next as a TempDatabase (TempDatabase / SetTempDatabase); a
// session rebuilt after another connection's commit carries it over
// (adoptTempFrom), and one rebuilt after its OWN failed commit takes the state
// of the last successful commit (tempAtCommit), because a COMMIT that fails
// must leave neither database changed.

// tempDB is the TEMP database's header: the values a "temp."-qualified PRAGMA
// reads and sets. Its objects are in the session's schema, its rows in their
// row stores.
type tempDB struct {
	// cookie is the TEMP database's own schema cookie, bumped by temp DDL the
	// way main's is by main DDL.
	cookie uint32
	// userVersion and applicationID are the other two header scalars a
	// "temp."-qualified PRAGMA can set, stored in the temp database's header.
	userVersion   uint32
	applicationID uint32
	// rowids is sqlite_temp_master's own rowid for each object, keyed by
	// tempCatalogKey. C assigns them with OP_NewRowid on the temp catalog --
	// one past the largest rowid present -- so they start again at 1 whatever
	// main holds, and a DROP that removes the largest lets the next CREATE
	// reuse it. tempCatalogRowids keeps exactly that.
	rowids map[string]int64
}

// errTempNotLoaded is a TEMP table asked to load its rows: a TEMP table is
// created loaded, in memory, so reaching the loader means its store was lost.
var errTempNotLoaded = errors.New("engine: internal: a TEMP table's rows are never loaded from a file")

// tempDatabase returns the TEMP database, opening it on first use --
// sqlite3OpenTempDatabase's own laziness (build.c:2073: "the temp database is
// opened the first time it is needed"). Not to be confused with DB.tempStore,
// which is "PRAGMA temp_store"'s value (writer.go).
func (db *DB) tempDatabase() (*tempDB, error) {
	if db.temp == nil {
		db.temp = &tempDB{}
	}
	db.tempDatabaseOpened = true
	return db.temp, nil
}

// tempHdr is the TEMP database's header without OPENING the database: a read
// of a temp object the session already holds must not count as the first use.
func (db *DB) tempHdr() *tempDB {
	if db.temp == nil {
		db.temp = &tempDB{}
	}
	return db.temp
}

// TempDatabaseOpened / SetTempDatabaseOpened track whether this connection
// opened its TEMP database (C SQLite's db->aDb[1].pBt != 0), used by
// "PRAGMA database_list". Once opened, it stays open even after every temp
// object is dropped.
func (db *DB) TempDatabaseOpened() bool { return db != nil && db.tempDatabaseOpened }

// SetTempDatabaseOpened seeds that bit from the connection.
func (db *DB) SetTempDatabaseOpened(v bool) {
	if db != nil && v {
		db.tempDatabaseOpened = true
	}
}

// rawCatalogRows reads rp's catalog as VALUES, in the order Schema() reports
// its rows, so a row can be inspected with its own NULLs intact.
func rawCatalogRows(rp *ReadOnlyPager) ([][]Value, error) {
	seq, errFn := rp.ScanTable(schemaRootPage)
	var out [][]Value
	for _, vals := range seq {
		out = append(out, append([]Value(nil), vals...))
	}
	return out, errFn()
}

// TempDatabase is a connection's TEMP database, as a driver carries it between
// the sessions its statements run on: the objects (with their row stores) and
// the header. It is ONE object per connection, not a copy per statement: a
// session publishes into it (TempDatabase), and version says which publication
// a session last saw, so installing it (SetTempDatabase) never replaces newer
// temp state a session holds with older -- which is exactly what happened when
// a statement's own commit hook ran a query on the same connection before the
// statement had published (replication's catalog check after a CREATE TEMP
// VIEW lost the view).
type TempDatabase struct {
	version  uint64
	pair     *tempPair
	tables   []*tableMeta
	views    []*viewMeta
	vtabs    []*vtabMeta
	triggers []*triggerMeta
	indexes  []*indexMeta
	hdr      *tempDB
	opened   bool
	held     bool
}

// TempDatabase is the connection's TEMP database as this session last
// PUBLISHED it -- at its last successful commit -- or nil before any. Never the
// state of a transaction still in flight: a statement whose commit fails (a
// BUSY the driver retries in a fresh session) must not hand its temp objects to
// the retry, exactly as C's temp database never sees a rolled-back change.
func (db *DB) TempDatabase() *TempDatabase {
	if db == nil {
		return nil
	}
	// The caller is the connection, which owns the temp file from here on.
	if db.tempPair != nil {
		db.tempPair.claimed = true
	}
	return db.tempHandle
}

// publishTemp makes this session's temp state the connection's: the handle's
// contents become the live objects, and its version moves so every other
// session holding an older one re-installs it.
func (db *DB) publishTemp() {
	if db.temp == nil && !db.tempDatabaseOpened && !db.holdsAnyTempObject() && db.tempHandle == nil {
		return
	}
	t := db.tempHandle
	if t == nil {
		t = &TempDatabase{}
		db.tempHandle = t
	}
	db.tempContents(t)
	t.version++
	db.tempSeen = t.version
}

// tempContents fills t from this session's temp objects and header.
func (db *DB) tempContents(t *TempDatabase) {
	*t = TempDatabase{version: t.version, pair: db.tempPair, hdr: db.temp, opened: db.tempDatabaseOpened, held: db.tempDatabaseHeldObject}
	for _, x := range db.tables {
		if x.isTemp {
			t.tables = append(t.tables, x)
		}
	}
	for _, x := range db.views {
		if x.isTemp {
			t.views = append(t.views, x)
		}
	}
	for _, x := range db.vtabs {
		if x.isTemp {
			t.vtabs = append(t.vtabs, x)
		}
	}
	for _, x := range db.triggers {
		if x.isTemp {
			t.triggers = append(t.triggers, x)
		}
	}
	for _, x := range db.indexes {
		if x.isTemp {
			t.indexes = append(t.indexes, x)
		}
	}
}

// SetTempDatabase makes t this session's TEMP database, replacing whatever temp
// objects it held -- unless this session already holds t at the version t is
// at, in which case its own state is the newer one and is kept. nil leaves the
// session as it is.
func (db *DB) SetTempDatabase(t *TempDatabase) {
	if db == nil || t == nil || (db.tempHandle == t && db.tempSeen == t.version) {
		return
	}
	db.installTemp(t)
	db.tempHandle, db.tempSeen = t, t.version
}

// installTemp replaces this session's temp objects and header with t's.
func (db *DB) installTemp(t *TempDatabase) {
	db.dropTempFromLists()
	db.tables = append(db.tables, t.tables...)
	db.views = append(db.views, t.views...)
	db.vtabs = append(db.vtabs, t.vtabs...)
	db.triggers = append(db.triggers, t.triggers...)
	db.indexes = append(db.indexes, t.indexes...)
	if t.hdr != nil {
		db.temp = t.hdr
	}
	db.tempPair = t.pair
	db.tempDatabaseOpened = db.tempDatabaseOpened || t.opened
	db.tempDatabaseHeldObject = db.tempDatabaseHeldObject || t.held
}

// dropTempFromLists removes every TEMP object from the session's schema lists,
// leaving main's in their own order.
func (db *DB) dropTempFromLists() {
	db.tables = slices.DeleteFunc(db.tables, func(x *tableMeta) bool { return x.isTemp })
	db.views = slices.DeleteFunc(db.views, func(x *viewMeta) bool { return x.isTemp })
	db.vtabs = slices.DeleteFunc(db.vtabs, func(x *vtabMeta) bool { return x.isTemp })
	db.triggers = slices.DeleteFunc(db.triggers, func(x *triggerMeta) bool { return x.isTemp })
	db.indexes = slices.DeleteFunc(db.indexes, func(x *indexMeta) bool { return x.isTemp })
}

// cloneTemp is t with every table's rows SNAPSHOTTED (rowStore.clone), for the
// state a failed commit returns to: the live stores go on changing after it
// is taken.
func (t *TempDatabase) cloneTemp() *TempDatabase {
	if t == nil {
		return nil
	}
	cp := *t
	cp.tables = make([]*tableMeta, len(t.tables))
	for i, tbl := range t.tables {
		c := *tbl
		if tbl.rows != nil {
			c.rows = tbl.rows.clone(tbl)
		}
		cp.tables[i] = &c
	}
	if t.hdr != nil {
		h := *t.hdr
		h.rowids = maps.Clone(t.hdr.rowids)
		cp.hdr = &h
	}
	return &cp
}

// noteTempCommitted records the TEMP database as the last successful commit
// left it -- what adoptTempCommittedFrom restores after a commit that failed.
func (db *DB) noteTempCommitted() {
	var cur TempDatabase
	db.tempContents(&cur)
	db.tempAtCommit = cur.cloneTemp()
	db.publishTemp()
}

// adoptTempFrom moves the TEMP half of one session onto this one: a
// session REBUILT because another connection committed comes back with main's
// half re-read and temp's exactly as it was. C SQLite resets one schema at a
// time for the same reason (sqlite3ResetOneSchema, build.c:5196) and never
// re-derives aDb[1] from aDb[0]. Without it a connection holding a TEMP table
// answered "no such table" for it the moment ANY other connection committed.
func (db *DB) adoptTempFrom(old *DB) {
	if old == nil || db == old {
		return
	}
	// Old's state as it stands, published or not, and the same handle.
	var cur TempDatabase
	old.tempContents(&cur)
	db.installTemp(&cur)
	db.tempHandle, db.tempSeen = old.tempHandle, old.tempSeen
	db.tempAtCommit = old.tempAtCommit
	db.tempStore = old.tempStore
	db.tempJournalMode = old.tempJournalMode
	old.dropTempFromLists()
	old.temp = nil
}

// adoptTempCommittedFrom is adoptTempFrom for a session rebuilt after its OWN
// failed commit: the connection's temp database comes back as the last
// successful commit left it, not with the failed transaction's changes in it.
func (db *DB) adoptTempCommittedFrom(old *DB) {
	if old == nil || db == old {
		return
	}
	if c := old.tempAtCommit.cloneTemp(); c != nil {
		db.installTemp(c)
	}
	db.tempHandle, db.tempSeen = old.tempHandle, 0 // a failed commit's state was published; this replaces it
	db.tempAtCommit = old.tempAtCommit
	db.tempDatabaseOpened = db.tempDatabaseOpened || old.tempDatabaseOpened
	db.tempStore, db.tempJournalMode = old.tempStore, old.tempJournalMode
	old.dropTempFromLists()
	old.temp = nil
}

// OwnsTempDatabase reports whether this session has a TEMP database of its own.
func (db *DB) OwnsTempDatabase() bool {
	return db != nil && (db.temp != nil || db.holdsAnyTempObject())
}

// setTempHeaderScalar writes one of the TEMP database's header values, opening
// that database if this is the first thing to touch it -- which is what C
// SQLite does too (its "PRAGMA temp.user_version=7" opens aDb[1]).
func (db *DB) setTempHeaderScalar(name string, v uint32) error {
	t, err := db.tempDatabase()
	if err != nil {
		return err
	}
	switch name {
	case "user_version":
		t.userVersion = v
	case "application_id":
		t.applicationID = v
	case "schema_version":
		t.cookie = v
	}
	return nil
}

// emptyTempDatabase discards the TEMP database's header -- invalidateTempStorage
// (pragma.c:189-206) closes aDb[1] -- reached from DropTempObjects and a
// "PRAGMA temp_store" change. The objects themselves are dropped by the caller.
func (db *DB) emptyTempDatabase() error {
	db.temp = nil
	if db.tempPair != nil {
		db.tempPair.remove()
		db.tempPair = nil
	}
	return nil
}

// tempCatalogKey names one temp object in sqlite_temp_master's rowid map.
func tempCatalogKey(r SchemaRow) string { return r.Type + "\x00" + strings.ToLower(r.Name) }

// tempCatalogRowids gives every temp row its sqlite_temp_master rowid: the one it
// was created with, and for a new object one past the largest still present --
// OP_NewRowid's rule on the temp catalog (vdbe.c's OP_NewRowid, from
// sqlite3NestedParse's INSERT INTO sqlite_temp_master in build.c).
func (db *DB) tempCatalogRowids(rows []SchemaRow) {
	t := db.tempHdr()
	present := map[string]bool{}
	for _, r := range rows {
		present[tempCatalogKey(r)] = true
	}
	for k := range t.rowids {
		if !present[k] {
			delete(t.rowids, k)
		}
	}
	if t.rowids == nil {
		t.rowids = map[string]int64{}
	}
	var top int64
	for _, id := range t.rowids {
		top = max(top, id)
	}
	for i := range rows {
		k := tempCatalogKey(rows[i])
		id, ok := t.rowids[k]
		if !ok {
			top++
			id = top
			t.rowids[k] = id
		}
		rows[i].Rowid = id
	}
}

// tempReader is the TEMP database as a read pager named "temp": the temp
// tables' live row stores and sqlite_temp_master rendered from the temp
// objects. nil when the session has no temp database. freeze snapshots the row
// stores, as segmentPager's does.
func (db *DB) tempReader(freeze bool) (*attachedReader, error) {
	if !db.OwnsTempDatabase() {
		return nil, nil
	}
	var rows []SchemaRow
	for _, r := range db.segmentSchemaRows() {
		if r.Temp {
			// The text as C stores it in the temp catalog: TEMP/TEMPORARY taken
			// back out.
			r.SQL = withoutTempKeyword(r.SQL)
			rows = append(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b SchemaRow) int { return int(a.Rowid - b.Rowid) })
	db.tempCatalogRowids(rows)
	p := newReadOnlyPager()
	src := &segSource{
		byRoot:  map[uint32][]*segment{},
		tableOf: map[uint32]int{},
		live:    map[uint32]*rowStore{},
		session: db,
		frozen:  freeze,
	}
	for _, t := range db.tables {
		if !t.isTemp || t.rows == nil {
			continue
		}
		live := t.rows
		if freeze {
			live = t.rows.clone(t)
		}
		src.live[t.rootPage] = live
	}
	src.byRoot[schemaRootPage] = []*segment{}
	if inserted := db.writableSchemaInsertedRowsIn(true); len(inserted) > 0 {
		// A row written straight into sqlite_temp_schema is listed after the
		// objects', under max+1 as OP_NewRowid gives it, from its RAW values: a
		// row of NULLs reads back NULL, which SchemaRow cannot carry (see
		// buildSchemaSegmentFromValues, and segmentPager's identical overlay for
		// main).
		rowids := make([]uint64, 0, len(rows)+len(inserted))
		stored := make([][]Value, 0, len(rows)+len(inserted))
		var top int64
		for _, r := range rows {
			rowids = append(rowids, uint64(r.Rowid))
			top = max(top, r.Rowid)
			stored = append(stored, []Value{
				{Typ: Text, S: []byte(r.Type)}, {Typ: Text, S: []byte(r.Name)},
				{Typ: Text, S: []byte(r.TblName)}, {Typ: Int, I: int64(r.RootPage)},
				sqlValueForSchemaRow(r.SQL),
			})
		}
		for _, r := range inserted {
			top++
			rowids = append(rowids, uint64(top))
			stored = append(stored, r.vals)
		}
		seg, serr := buildSchemaSegmentFromValues(rowids, stored)
		if serr != nil {
			return nil, serr
		}
		src.byRoot[schemaRootPage] = []*segment{seg}
	} else if len(rows) > 0 {
		seg, serr := buildSchemaSegment(rows)
		if serr != nil {
			return nil, serr
		}
		src.byRoot[schemaRootPage] = []*segment{seg}
	}
	p.segs = src
	p.schemaRows = rows
	p.schemaLoaded = true
	// The TEMP database is read under the SAME connection state as main: a
	// pragma that changes how a scan answers is per CONNECTION in C SQLite.
	db.stampPager(p)
	// ...but it is NOT main: stampPager carries the session's own schema name
	// and readers, and a temp pager left answering to "" took itself for main --
	// fts4aux(main, t1) then looked for t1 in the temp database (amatch1.test).
	p.localSchema = "temp"
	p.writeSession = db
	p.attachedReaders = nil
	p.originReadersBefore = 0
	t := db.tempHdr()
	p.meta.schemaCookie, p.meta.userVersion, p.meta.applicationID = t.cookie, t.userVersion, t.applicationID
	p.meta.encoding = db.encoding()
	return &attachedReader{name: "temp", pager: p}, nil
}

// TempPager is this session's TEMP database as a read pager, or nil when the
// session has none -- so the driver can wire TEMP into a cross-database read
// without knowing how the engine keeps it.
func (n *Session) TempPager() (*ReadOnlyPager, error) {
	ar, err := n.tempReader(false)
	if err != nil || ar == nil {
		return nil, err
	}
	return ar.pager, nil
}
