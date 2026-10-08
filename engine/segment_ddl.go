package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// CREATING a segment database, and changing its SCHEMA.
//
// The engine does not open SQLite files; they are converted first.
//
// Two capabilities were missing, and neither is the one I first claimed. I said
// the conformance gates forced the SQLite path because they build their databases
// through musql. That was WRONG: the gates compare RESULTS between musql and C
// SQLite -- C writes its own .db, musql writes its own file, nothing is shared --
// so the oracle never needs musql to open a SQLite file. What was actually
// missing:
//
//  1. a way to create an EMPTY segment database, since every segment file came from
//     exporting a .db;
//  2. DDL, because a delta carries ROWS and the catalog lives in the segment
//     file's directory.
//
// ---- the split, and why DDL is allowed to be expensive ----
//
// A row change APPENDS: translate, write one batch, fsync (segment_write.go).
// That is the hot path and it is 61us-2.4ms depending on the durability asked
// for.
//
// A catalog change REWRITES THE WHOLE FILE. DDL is rare -- a schema moves once
// per deploy, not once per statement -- so paying O(database) for it buys a much
// simpler and more checkable design than threading catalog records through the
// delta.
//
// It also has to COMPACT first, and that is correctness rather than tidiness: a
// delta record names its table by its INDEX in the directory, so a DROP TABLE
// that removes an entry silently repoints every record after it. Folding the log
// in before the directory moves means there are no records left to repoint.

// CreateFile creates an empty segment database at path, replacing anything
// already there.
func CreateFile(path string) error {
	// A delta beside the old file describes rows that are about to stop existing.
	if err := os.Remove(segDeltaPath(path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Counter 1, pages 1: a state, not a page count. A segment file has no pages;
	// the pair (counter, pages) exists only so a delta can name the file it
	// extends and so AttachSegments can compare a converted database against it.
	return WriteSegmentFileWithCatalog(path, nil, ConvertedCatalog{}, 1, 1)
}

// Create creates an empty segment database and opens a write session on
// it -- CreateFile followed by OpenWrite.
func Create(path string) (*Session, error) {
	if err := CreateFile(path); err != nil {
		return nil, err
	}
	return OpenWrite(path)
}

// rewriteFile writes the whole database this session holds back out as a new
// segment file: every table's rows from its row store, and the catalog from the
// session's own objects.
//
// It is how a catalog change is persisted, and it is also what makes a segment
// session able to CREATE a table at all -- there is nowhere else for the
// directory entry to come from.
func (n *Session) rewriteFile() error {
	db := n.DB
	// NOTHING ANOTHER WRITER COMMITTED MAY BE LOST, and nothing of THIS session's
	// transaction may land before the rename does.
	//
	// This function writes the whole file from THIS session's row stores, so a
	// batch another connection appended and this session never loaded would be
	// destroyed by it. That is not theoretical: two replicated nodes share a file
	// between an application connection and a sync connection, and a CREATE TABLE
	// IF NOT EXISTS on one of them deleted the other's committed rows. The
	// stale-image check below refuses the rewrite outright when anyone has
	// committed since this session loaded, so what it writes is the file as it
	// stands plus this transaction -- every table is loaded from the session's own
	// source (segmentFileContentsOf), which nothing has moved.
	//
	// It used to append the transaction's pending rows to the delta first and
	// reload every file table through a reopened source. That made the rewrite TWO
	// commit points: a rewrite that then failed -- or was refused by "PRAGMA
	// max_size", which can only judge the file once it is built -- left the rows
	// durable and the DDL undone. The reload had been how other writers' rows were
	// folded in; the stale-image check made it a no-op, so it is gone, and the
	// rename below is the only commit point.
	//
	// The pairing identity is the FILE's, not this session's. Each session was
	// incrementing its own endCtr, so two writers on one file produced different
	// counters for it and one of them then wrote a delta header naming a base the
	// file no longer had ("delta was built for counter 11 pages 1; this file is
	// counter 7"). Re-read it before appending anything.
	if err := n.refreshPairIdentity(); err != nil {
		return err
	}
	// A REWRITE WRITES THE WHOLE DATABASE from this session's view of it, so a
	// commit by anyone else since this session loaded makes that view the wrong
	// thing to write -- the same stale-image refusal the delta path applies, for a
	// bigger blast radius: without it, an ALTER racing another writer's INSERT
	// would drop that row.
	if n.loadedCtr != n.endCtr || n.loadedPages != n.endPages {
		return fmt.Errorf("%w: another connection committed while this transaction was open", ErrBusy)
	}
	// Write beside the target and rename, so an interruption leaves the previous
	// file intact rather than a half-written directory.
	dir := filepath.Dir(n.segPath)
	w, werr := newSegFileWriter(dir)
	if werr != nil {
		return werr
	}
	defer w.discard()
	tables, cat, berr := db.segmentFileContents(w)
	if berr != nil {
		return berr
	}
	tmp, terr := os.CreateTemp(dir, filepath.Base(n.segPath)+".ddl-*")
	if terr != nil {
		return terr
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	n.endCtr++
	if werr := w.finish(tmpPath, tables, cat, n.endCtr, n.endPages); werr != nil {
		n.endCtr--
		return werr
	}
	// "PRAGMA max_size" (DB.SetMaxSize): the new file replaces segments and delta
	// both, so its size alone is what the database will hold. Refused here,
	// nothing of the transaction is durable -- the temporary file goes with the
	// deferred remove.
	if n.maxSize > 0 {
		if st, serr := os.Stat(tmpPath); serr != nil || st.Size() > n.maxSize {
			n.endCtr--
			if serr != nil {
				return serr
			}
			return ErrDiskFull
		}
	}
	if serr := syncSegFile(tmpPath); serr != nil {
		n.endCtr--
		return serr
	}
	if rerr := os.Rename(tmpPath, n.segPath); rerr != nil {
		n.endCtr--
		return rerr
	}
	syncDir(n.segPath)
	// A direct-path load's segments are in the file now.
	for _, t := range db.tables {
		t.bulk = nil
	}
	// The delta is gone: its rows are in the segments now, and its records named
	// table indexes the new directory may not use.
	if rerr := os.Remove(segDeltaPath(n.segPath)); rerr != nil && !os.IsNotExist(rerr) {
		return rerr
	}
	syncDir(n.segPath)

	// The file's identity moved, so the session's pairing and its append point
	// both start again from it.
	n.baseCtr, n.basePages = n.endCtr, n.endPages
	n.loadedCtr, n.loadedPages = n.endCtr, n.endPages // every row was folded in above
	n.appendAt.release()
	f, ferr := OpenSegmentFile(n.segPath)
	if ferr != nil {
		return ferr
	}
	n.tableIndex, n.ipkOf = SegmentFileTableIndex(f)
	f.Close()
	// Every row is now IN the segments, so the changes that produced them must not
	// also be appended as a delta batch.
	n.TakeRowChanges()
	// AND THE CATALOG IS NOW WRITTEN, which is what stops the next commit rewriting
	// the file again. Losing this line made every commit take the rewrite path: the
	// fingerprint never matched, so an ordinary INSERT rewrote the whole file, and
	// one of those rewrites reloaded a table before its rows had been folded in and
	// wrote it EMPTY.
	n.catalogAtLastWrite = n.catalogFingerprint()
	n.seenStat = n.pairStamp()
	for _, t := range db.tables {
		t.inFile = !t.isTemp // every main table is the file's now
	}
	n.rebase(openSegmentsUnlocked) // the write lock is held: see openSegmentsUnlocked
	return nil
}

// rebase points the session at the file it has just written -- a rewrite or a
// compaction -- and lets every main table read its rows from there again.
//
// Until then a table this session wrote stayed an in-memory store of ALL its
// rows: fromFile skips a mutated table's reload, rightly while the file lags
// the session, and nothing ever re-read it afterwards. So a connection that
// loaded a table held every row of it for its whole life, and a write scan over
// it could never reach the segments (segWriteFilterPeephole's filter had no
// segments to read): "UPDATE t SET k = k + 1 WHERE sec = ?" through the driver
// scanned all 100k rows at 33ms where the same statement on a reopened session
// took 0.5ms. Once the file IS the session's view -- every row the session
// holds has just been written to it -- reading back from it loses nothing.
//
// Skipped inside a transaction (a savepoint's snapshot shares these stores),
// while a table aliases another's storage (the alias shares its owner's store
// object, which unloading both would split), and while there is a virtual
// table: fts3/fts4/fts5 and rtree write their shadow tables through putRow,
// which assumes a loaded store (fts3StoreSegment on an unloaded one was a nil
// dereference in TestVirtualTableRowsSurviveAReopen).
//
// ponytail: a database with a virtual table keeps the old in-memory behavior;
// keep its shadow tables loaded and re-base the rest if that ever matters.
func (n *Session) rebase(open func(string) (*ReadOnlyPager, error)) {
	db := n.DB
	if db.inTransaction() || len(db.vtabs) > 0 {
		return
	}
	for _, t := range db.tables {
		if t.aliasOf != "" {
			return
		}
	}
	fresh, err := open(n.segPath)
	if err != nil {
		return // the old view is still correct, only bigger
	}
	// Whatever survives the swap must not alias the mapping being closed: the
	// TEMP tables, which are never reloaded (as in rewriteFile).
	for _, t := range db.tables {
		if t.isTemp && t.rows != nil {
			detachRowsFromMapping(t)
		}
	}
	old := n.src
	n.src, db.segments = fresh, fresh
	if old != nil {
		old.Close()
	}
	for _, t := range db.tables {
		if !t.isTemp {
			t.loaded, t.rows, t.rowsMutatedThisSession = false, nil, false
		}
	}
	n.noteCommittedColumnsStale()
}

// segmentFileContents is this session's whole database as a segment file's
// contents: one ConvertedTable per non-TEMP table with its rows, plus the catalog
// of everything else the file records.
//
// Two callers, and they are the same operation seen twice: the file REWRITE a
// catalog change or a compaction performs (Session.rewriteFile above), and
// "VACUUM INTO", which writes exactly this to another path instead of over this
// one (execVacuumInto, vacuum_write.go).
func (db *DB) segmentFileContents(w *segFileWriter) ([]ConvertedTable, ConvertedCatalog, error) {
	return db.segmentFileContentsOf(false, w)
}

// segmentFileContentsOf is one database's file contents: main's (temp false) or
// the TEMP database's own (temp_file.go), which is written the same way. Every
// table's segments go to w as they are built; the tables returned carry only
// their metadata, for w.finish.
func (db *DB) segmentFileContentsOf(temp bool, w *segFileWriter) ([]ConvertedTable, ConvertedCatalog, error) {
	// Every table's rows must be in hand, so a table nothing touched this session
	// is loaded now rather than written out empty.
	for _, t := range db.tables {
		if t.isTemp != temp || temp {
			continue // a TEMP table is always loaded
		}
		if err := db.ensureTableLoaded(t); err != nil {
			return nil, ConvertedCatalog{}, fmt.Errorf("engine: DDL: loading %s: %w", t.name, err)
		}
	}

	var tables []ConvertedTable
	// seqs is every entry's schemaSeq, tables then objects -- the order the
	// file lists them in -- from which catalogRanks derives each one's Rank.
	var seqs []uint64
	aliasRoots := 0
	for _, t := range db.tables {
		if t.isTemp != temp {
			continue
		}
		seqs = append(seqs, t.schemaSeq)
		ct := ConvertedTable{Name: t.name, SQL: t.sql, IPK: t.ipkIndex, Rowid: int64(t.schemaSeq), Root: t.rootPage}
		for _, c := range t.cols {
			ct.Cols = append(ct.Cols, c.Name)
		}
		colInfos := make([]columnInfo, len(t.cols))
		copy(colInfos, t.cols)
		if t.aliasOf != "" {
			// An ALIAS reads its owner's rows, which the owner's own entry writes;
			// its entry stays, empty, like C's orphaned b-tree, and the catalog
			// records whose storage it reads (ConvertedCatalog.RootEdits, below).
			//
			// Its root is a FRESH one, not the owner's it is bound to in memory:
			// two entries recording one root made the reopened load take the
			// OWNER for the alias ("SELECT rootpage" answered t1's own slot for
			// both rows), and the alias's empty entry claimed the owner's
			// segments. replayRootEdits rebinds it by name either way.
			ct.Root = db.nextSegmentRoot(temp) + uint32(aliasRoots)
			aliasRoots++
			tables = append(tables, ct)
			continue
		}

		ti := len(tables)
		// A direct-path load (insert_bulk_direct.go): its rows are already
		// segments, and the row store holds none of them.
		if t.bulk != nil {
			segs, berr := t.bulkSegments()
			for _, raw := range segs {
				if berr != nil {
					break
				}
				berr = w.addSegment(ti, raw)
			}
			if berr != nil {
				return nil, ConvertedCatalog{}, fmt.Errorf("engine: DDL: %w", berr)
			}
			tables = append(tables, ct)
			continue
		}
		// A table whose rows are still exactly its file's segments -- nothing
		// written, deleted or spilled this session, nothing waiting for it in
		// the delta, the same columns -- is copied segment by segment as the
		// bytes it already is: a rebuild would cut and encode the same rows
		// and rebuild the same column indexes. A VACUUM right after a bulk
		// load paid a whole second rebuild for nothing.
		if segs, ok := pristineSegments(t); ok {
			var aerr error
			for _, sg := range segs {
				if aerr = w.addSegment(ti, sg.buf); aerr != nil {
					break
				}
			}
			if aerr != nil {
				return nil, ConvertedCatalog{}, fmt.Errorf("engine: DDL: %w", aerr)
			}
			tables = append(tables, ct)
			continue
		}
		// ONE SEGMENT IN HAND at a time (segCutter): the rows are read in rowid
		// order and cut as they fill, so a table this session spilled
		// (row_store_spill.go) is read back a segment at a time, not whole.
		cut := &segCutter{cols: colInfos, what: t.name, emit: func(raw []byte) error { return w.addSegment(ti, raw) }}
		var cerr error
		t.rows.eachSortedUntil(func(rid uint64, vals []Value) bool {
			cerr = cut.add(rid, vals)
			return cerr == nil
		})
		if cerr == nil {
			cerr = cut.flush()
		}
		if cerr != nil {
			return nil, ConvertedCatalog{}, fmt.Errorf("engine: DDL: %w", cerr)
		}
		tables = append(tables, ct)
	}

	var cat ConvertedCatalog
	// The schema cookie belongs to the FILE, and on this format the catalog is the
	// only place it can live. Every DDL has already bumped the session's own
	// (bumpSchema, schema_write.go), so writing it here is what makes "PRAGMA
	// schema_version" answer anything but 0 for a segment database.
	cat.SchemaVersion = db.schemaCookie
	// ...and the two words the APPLICATION owns, for the same reason: C SQLite
	// keeps them in the file header and this format has no header. Without them
	// "PRAGMA user_version=11" was forgotten at the next open.
	cat.UserVersion, cat.ApplicationID = db.userVersion, db.applicationID
	if temp {
		h := db.tempHdr()
		cat.SchemaVersion, cat.UserVersion, cat.ApplicationID = h.cookie, h.userVersion, h.applicationID
	}
	// "PRAGMA encoding", and only once there is a SCHEMA to be encoded -- which is
	// C's own model: header byte 56 stays 0 on a database with no schema, so a
	// freshly created one has declared no encoding and an ATTACH of it INHERITS the
	// connection's (attach.go's agreement check reads the zero exactly that way).
	if db.hasSchemaObjects() {
		cat.Encoding = uint32(db.encoding())
	}
	cat.PageSize = db.pageSize                      // see ConvertedCatalog.PageSize
	cat.AutoVacuumPlus1 = uint32(db.autoVacuum) + 1 // see ConvertedCatalog.AutoVacuumPlus1
	if db.segWAL {
		cat.JournalWAL = 1 // see ConvertedCatalog.JournalWAL
	}
	if !temp {
		cat.CaptureGuard = db.captureGuard // see ConvertedCatalog.CaptureGuard
	}
	for _, idx := range db.indexes {
		if idx.sql == "" || idx.isTemp != temp {
			continue // an automatic index: the table's own constraint rebuilds it
		}
		seqs = append(seqs, idx.schemaSeq)
		cat.Objects = append(cat.Objects, ConvertedObject{
			Type: "index", Name: idx.name, TblName: idx.table, SQL: idx.sql, Rowid: int64(idx.schemaSeq),
		})
	}
	for _, v := range db.views {
		if v.isTemp != temp {
			continue
		}
		seqs = append(seqs, v.schemaSeq)
		cat.Objects = append(cat.Objects, ConvertedObject{
			Type: "view", Name: v.name, TblName: v.name, SQL: v.sql, Rowid: int64(v.schemaSeq),
		})
	}
	for _, g := range db.triggers {
		if g.isTemp != temp {
			continue
		}
		seqs = append(seqs, g.schemaSeq)
		cat.Objects = append(cat.Objects, ConvertedObject{
			Type: "trigger", Name: g.name, TblName: g.table, SQL: g.sql, Rowid: int64(g.schemaSeq),
		})
	}
	// VIRTUAL TABLES, which are "table" rows with rootpage 0 in SQLite and were
	// simply not written here at all: a segment-backed session could CREATE VIRTUAL TABLE,
	// use it, and lose the definition at the next rewrite -- the shadow tables
	// survived as ordinary tables with rows and nothing was over them. "PRAGMA
	// table_list" then reported no row for the vtab and typed every shadow "table"
	// instead of "shadow", and table_xinfo answered nothing.
	for _, vt := range db.vtabs {
		if vt.isTemp != temp {
			continue
		}
		seqs = append(seqs, vt.schemaSeq)
		cat.Objects = append(cat.Objects, ConvertedObject{
			Type: "table", Name: vt.name, TblName: vt.name, SQL: vt.sql, Rowid: int64(vt.schemaSeq),
		})
	}
	ranks := catalogRanks(seqs)
	for i := range tables {
		tables[i].Rank = ranks[i]
	}
	for i := range cat.Objects {
		cat.Objects[i].Rank = ranks[len(tables)+i]
	}
	// Every live ALIAS, by the rendered number of the table it reads.
	if temp {
		return tables, cat, nil // no alias roots, no direct catalog edits: main's alone
	}
	if rows := db.segmentSchemaRows(); len(rows) > 0 {
		roots := catalogRootpages(rows)
		for i, r := range rows {
			if r.AliasOf != "" && !r.Temp {
				cat.RootEdits = append(cat.RootEdits, ConvertedRootEdit{Name: r.Name, Root: int64(roots[i])})
			}
		}
	}
	if db.writableSchemaEditsActive() {
		return db.applyCatalogEditsForWrite(tables, cat)
	}
	return tables, cat, nil
}

// applyCatalogEditsForWrite writes the session's direct sqlite_schema edits
// (db.wsEdits) into the catalog the rewrite is about to persist.
//
// C writes a "PRAGMA writable_schema=ON" edit straight into the sqlite_schema
// b-tree: it is durable at COMMIT, and the next connection's schema load reads
// it, while the connection that made it keeps its loaded schema until a RESET or
// a reopen. This format keeps the edit as an overlay until a RESET reloads from
// it -- and a commit without one DROPPED it: the UPDATE reported one row changed
// and a reopen found the old text (TestWritableSchemaWritePersists). The live
// session is left exactly as it was, which is C's same-connection behaviour.
//
// Rows keep their stored shape: a table whose text gains a column reads its
// existing rows as SHORT, i.e. the new column's DEFAULT, as C reads a record
// written before the edit.
//
// ponytail: only edits the catalog can SAY are written -- sql text, a name, an
// index/view/trigger's tbl_name, a deleted row, an inserted storage-less row. The
// rest (a type change, a table's tbl_name apart from its name, a NULL text column,
// an inserted row naming storage) is what C persists as a CORRUPT catalog, whose
// next load fails "malformed database schema"; those stay unwritten, as every edit
// was before this. The upgrade is a raw-rows catalog field the loader runs C's
// sqlite3InitCallback rules over.
func (db *DB) applyCatalogEditsForWrite(tables []ConvertedTable, cat ConvertedCatalog) ([]ConvertedTable, ConvertedCatalog, error) {
	text := func(v *Value) (string, bool) {
		if v == nil || v.Typ != Text {
			return "", false
		}
		return string(v.S), true
	}
	for key, e := range db.wsEdits {
		if key.temp || key.ins != 0 {
			continue // TEMP is never persisted; inserted rows are appended below
		}
		ti, oi := -1, -1
		for i := range tables {
			if tables[i].Name == key.name {
				ti = i
			}
		}
		for i := range cat.Objects {
			if cat.Objects[i].Name == key.name {
				oi = i
			}
		}
		if ti < 0 && oi < 0 {
			continue // an automatic index's row: its table's DDL rebuilds it
		}
		if e.deleted {
			// The row is gone, so the next load has no such object -- C leaves its
			// b-tree orphaned, unreachable, which is the same database to a reader.
			if ti >= 0 {
				tables = append(tables[:ti], tables[ti+1:]...)
			} else {
				cat.Objects = append(cat.Objects[:oi], cat.Objects[oi+1:]...)
			}
			continue
		}
		curType := "table"
		if oi >= 0 {
			curType = cat.Objects[oi].Type
		}
		if e.set[0] != nil {
			if t, ok := text(e.set[0]); !ok || t != curType {
				continue // unrepresentable: see the ponytail note above
			}
		}
		name, sqlText := key.name, ""
		if e.set[1] != nil {
			n, ok := text(e.set[1])
			if !ok {
				continue
			}
			name = n
		}
		tbl, haveTbl := "", false
		if e.set[2] != nil {
			tn, ok := text(e.set[2])
			if !ok || (ti >= 0 && tn != name) {
				continue
			}
			tbl, haveTbl = tn, true
		}
		haveSQL := e.set[4] != nil
		if haveSQL {
			q, ok := text(e.set[4])
			if !ok {
				continue
			}
			sqlText = q
		}
		// set[3], rootpage: an assignment of anything but the row's own number is
		// recorded as the number itself (ConvertedCatalog.RootEdits), which the
		// next load resolves as a RESET would -- C's catalog holds the number and
		// its next load reads whatever it names.
		if e.set[3] != nil && db.segmentRootpageEditPlan(key, e.set[3]) != rootEditNoop {
			root := int64(-1)
			if e.set[3].Typ == Int {
				root = e.set[3].I
			}
			cat.RootEdits = append(cat.RootEdits, ConvertedRootEdit{Name: name, Root: root})
		}
		if ti >= 0 {
			tables[ti].Name = name
			if haveSQL {
				tables[ti].SQL = sqlText
			}
		} else {
			cat.Objects[oi].Name = name
			if haveTbl {
				cat.Objects[oi].TblName = tbl
			}
			if haveSQL {
				cat.Objects[oi].SQL = sqlText
			}
		}
	}
	for _, r := range db.writableSchemaInsertedRows() {
		if len(r.vals) < 5 || r.vals[3].Typ != Int || r.vals[3].I != 0 {
			continue // names storage, or is not a row the catalog can say
		}
		typ, ok1 := text(&r.vals[0])
		name, ok2 := text(&r.vals[1])
		tbl, ok3 := text(&r.vals[2])
		q, ok4 := text(&r.vals[4])
		if !ok1 || !ok2 || !ok3 || !ok4 {
			continue
		}
		cat.Objects = append(cat.Objects, ConvertedObject{Type: typ, Name: name, TblName: tbl, SQL: q})
	}
	return tables, cat, nil
}

// refreshPairIdentity re-reads the segment file's own identity and the state its
// delta brings the pair to, WITHOUT touching this session's rows.
//
// Separate from RefreshIfStale because the two answer different questions: that
// one asks "should I throw my loaded rows away", and must refuse while this
// session has uncommitted work. This one only corrects the bookkeeping a delta
// header is written from, which is safe at any time and REQUIRED before an append
// once more than one writer exists.
func (n *Session) refreshPairIdentity() error {
	baseCtr, basePages, berr := SegmentFileBase(n.segPath)
	if berr != nil {
		return berr
	}
	endCtr, endPages, serr := segmentFileStateUnlocked(n.segPath)
	if serr != nil {
		return serr
	}
	if baseCtr != n.baseCtr || basePages != n.basePages {
		// A different generation of the file: nothing this session carried about
		// where to append is still true.
		n.appendAt.release()
	}
	n.baseCtr, n.basePages = baseCtr, basePages
	n.endCtr, n.endPages = endCtr, endPages
	f, ferr := OpenSegmentFile(n.segPath)
	if ferr != nil {
		return ferr
	}
	n.tableIndex, n.ipkOf = SegmentFileTableIndex(f)
	f.Close()
	return nil
}

// detachRowsFromMapping copies every byte slice a table's rows hold, so they no
// longer point into a segment file's mapping.
//
// One table at a time and only where it is needed: a row that came from a
// segment aliases the mapping (segment.Value hands back a sub-slice, which is
// what makes a scan allocation-free), and anything that outlives the mapping has
// to own its bytes.
// ownValues returns vals with every byte slice copied, so nothing in it points
// into a segment file's mapping any more.
func ownValues(vals []Value) []Value {
	out := make([]Value, len(vals))
	copy(out, vals)
	for i := range out {
		if out[i].S != nil {
			out[i].S = append([]byte(nil), out[i].S...)
		}
	}
	return out
}

func detachRowsFromMapping(t *tableMeta) {
	// A store READING the mapping (row_store_seg.go) cannot be detached by
	// rewriting its rows -- they would land in its overlay with the base still
	// on the mapping about to be closed. It becomes an owned in-memory store,
	// which is what every table was before that store existed.
	// ponytail: a full load, paid only by a table whose catalog entry a rewrite
	// changes; rebasing it onto the new file would avoid it.
	if t.rows.seg != nil {
		m := map[uint64][]Value{}
		for rowid, vals := range t.rows.all() {
			m[rowid] = ownValues(vals)
		}
		t.rows = newRowStore(m, nil)
		return
	}
	type owned struct {
		rowid uint64
		vals  []Value
	}
	var todo []owned
	for rowid, vals := range t.rows.all() {
		todo = append(todo, owned{rowid, ownValues(vals)})
	}
	for _, o := range todo {
		t.rows.rewrite(o.rowid, o.vals)
	}
}

// newTableStore gives a table created this session its key and an empty row
// store.
func (db *DB) newTableStore(tbl *tableMeta) error {
	// A SEGMENT table has no b-tree root to allocate: its identity is its place
	// in the segment file's directory, assigned when the file is written
	// (segment_ddl.go's rewriteFile). A synthetic number is handed out here so
	// the rest of the session -- cursors, the schema it reports, the row store
	// it reads through -- has the stable key it expects.
	tbl.rootPage = db.nextSegmentRoot(tbl.isTemp)
	tbl.rows = newRowStore(map[uint64][]Value{}, nil)
	tbl.loaded = true
	return nil
}

// nextSegmentRoot hands out the next synthetic root in the database a new table
// belongs to: above every root its own database's tables hold, and in the TEMP database's own range for a temp table
// (segTempRootBase), so the two can never collide.
func (db *DB) nextSegmentRoot(isTemp bool) uint32 {
	next := uint32(segRootBase)
	if isTemp {
		next = segTempRootBase
	}
	for _, t := range db.tables {
		if t.isTemp == isTemp && t.rootPage >= next {
			next = t.rootPage + 1
		}
	}
	// ...and above every root main's open file keys, which includes an alias's
	// own empty entry (segmentFileContentsOf) that no table holds in memory.
	if !isTemp && db.segments != nil && db.segments.segs != nil {
		for r := range db.segments.segs.byRoot {
			if r >= next {
				next = r + 1
			}
		}
	}
	return next
}

// checkNewIndex validates a new index against its table's rows, as
// sqlite3RefillIndex (build.c:3450) does while it fills one: a UNIQUE index
// whose rows already collide, or an expression that errors on a row, fails the
// CREATE INDEX.
func (db *DB) checkNewIndex(idx *indexMeta, tbl *tableMeta) error {
	// A SEGMENT file carries an index's SQL and not its data (segment_open.go),
	// so there is no b-tree to fill. The UNIQUE check still has to run -- it is
	// the CONSTRAINT, not the index, and skipping it would accept duplicate
	// rows a later import would reject.
	if idx.unique && !idx.isTablePK {
		if err := validateUniqueIndex(tbl, idx, db.encoding()); err != nil {
			return err
		}
	} else if idx.exprOrPartial {
		// ...and so do its expressions, which sqlite3RefillIndex computes
		// for every row (see checkIndexExprsForRow).
		whereProg, keyProgs := compileIndexExprs(tbl, idx)
		for rowid, vals := range tbl.rows.all() {
			if err := indexExprsEvalErr(tbl, idx, whereProg, keyProgs, rowid, vals); err != nil {
				return err
			}
		}
	}
	return nil
}

// pristineSegments returns t's file segments when its row store has not moved
// off them: no row written or deleted this session, none spilled, no delta
// record for the table, and every segment as wide as the table.
func pristineSegments(t *tableMeta) ([]*segment, bool) {
	st := t.rows
	if st == nil || st.seg == nil || len(st.m) != 0 || len(st.gone) != 0 || st.baseGone || st.spill != nil {
		return nil, false
	}
	rp := st.seg.rp
	if rp == nil || rp.segs == nil {
		return nil, false
	}
	segs, ok := rp.segs.byRoot[st.seg.root]
	if !ok {
		return nil, false
	}
	if rows, dead, clean := rp.segs.overlayFor(st.seg.root); !clean || len(rows) != 0 || len(dead) != 0 {
		return nil, false
	}
	for _, sg := range segs {
		if len(sg.cols) != len(t.cols) {
			return nil, false
		}
	}
	return segs, true
}
