package engine

import (
	"fmt"
	"strings"
)

// OPENING A SEGMENT FILE AS A DATABASE -- the step that makes the segment file a
// database rather than an export of one.
//
// TestSegmentFileCannotYetBeOpenedAsADatabase used to pin the gap and was written
// to FAIL when someone closed it. This is that.
//
// ---- why it is this small ----
//
// The read path reaches its storage through exactly two doors, and both were
// already pluggable:
//
//   - Schema() caches into ReadOnlyPager.schemaRows and returns that when
//     schemaLoaded is set, so a segment open FILLS IT IN instead of scanning
//     sqlite_master;
//   - ScanTable() routes to segSource.scanSegments for any root a segment source
//     covers (segment_read.go).
//
// So no query compiler, VDBE or expression code changes. The corpus already
// proves rows read from segments answer identically; what is new is where the
// SCHEMA comes from and that no source database has to exist.
//
// ---- indexes, and why scanning is not a concession ----
//
// A segment file carries an index's SQL and not its data. For READS that costs
// nothing in correctness: an index is an accelerator, and a scan returns the same
// rows. It matters that the planner must not try to USE one, and it already
// cannot -- where_plan_index.go and index_seek_read.go both skip an index
// whose RootPage is 0, which is what the catalog's indexes are given here. They
// are still REPORTED, so sqlite_master is faithful and a convert-back rebuilds
// them.
//
// Scanning instead of seeking is also the direction the measurements point: on
// this format with the JIT, a filtered scan runs 14-67x faster than C SQLite's
// equivalent (docs/format-design.md section 9). Index DATA in the format is a
// later, measurement-driven addition, not a debt this open is carrying.
//
// ---- the one piece of scaffolding ----
//
// The pager starts empty (newReadOnlyPager) and takes the database's values --
// page size, text encoding and the rest -- from the catalog. Every row comes
// from the segments.

// segFileCloser lets ReadOnlyPager.Close release the segment file's mapping, and
// the delta's beside it (segDeltaState.mapping).
type segFileCloser struct {
	f  *SegmentFile
	st *segDeltaState
}

func (c segFileCloser) Close() error {
	c.st.release()
	return c.f.Close()
}

// segRootBase is where synthetic root pages start. A segment file has no
// b-tree roots, but ScanTable and the segment source are both keyed on one, so
// each table gets a stable number here. It begins well past page 1 so nothing
// mistakes one for the schema root.
const segRootBase = 1000

// segTempRootBase is where the TEMP database's synthetic roots start. The two
// databases' tables share this session's lists and its read source, so a TEMP
// table keyed in main's range was shadowed by -- or shadowed -- the main table
// that later took its number: a trigger's INSERT into main's log wrote nowhere
// either table showed.
const segTempRootBase = 1 << 30

// Open opens a segment file as a read-only database, folding in
// whatever delta sits beside it. No SQLite-format file is involved, and none has
// to exist.
//
// The returned pager owns the mapping; Close releases it.
func Open(path string) (*ReadOnlyPager, error) {
	// The segment file AND its delta are read under one SHARED lock, so a reader
	// cannot land between a rewrite's rename and its delta removal. See
	// withSegmentReadLock.
	var f *SegmentFile
	var st *segDeltaState
	if lerr := withSegmentReadLock(path, func() error {
		var err error
		f, st, err = readSegmentPairUnlocked(path)
		return err
	}); lerr != nil {
		return nil, lerr
	}
	// A catalog whose rootpages name OTHER objects' storage is resolved by a
	// session's load (replayRootEdits), which this read-only open does not run:
	// read here, an alias's own entry is empty, so it would answer "no rows"
	// where C reads its owner's. Declined rather than guessed -- which covers an
	// ATTACH of such a database too.
	if len(f.Catalog().RootEdits) > 0 {
		f.Close()
		st.release()
		return nil, errSegmentRootpageNotReproducible
	}

	return buildSegmentPager(path, f, st)
}

// sqlValueForSchemaRow is an object's CREATE text as sqlite_schema stores it:
// NULL, not the empty string, for an object that has none.
//
// An automatic index is the only such object, and the distinction is visible: C
// answers NULL for "SELECT sql FROM sqlite_schema WHERE name='sqlite_autoindex_t1_1'",
// so an empty TEXT here is a wrong answer rather than a cosmetic one.
func sqlValueForSchemaRow(sql string) Value {
	if sql == "" {
		return Value{Typ: Null}
	}
	return Value{Typ: Text, S: []byte(sql)}
}

// buildSchemaSegment renders a catalog as the five-column sqlite_master table,
// so a segment open can be queried for its own schema.
// schemaRowidsUsable reports whether rows carry their own sqlite_schema rowids:
// every one set and strictly ascending, which is the order the catalog is
// rendered in. A file written before the field existed has none, and its rows
// keep their positions.
func schemaRowidsUsable(rows []SchemaRow) bool {
	var prev int64
	for _, r := range rows {
		if r.Rowid <= prev {
			return false
		}
		prev = r.Rowid
	}
	return true
}

func buildSchemaSegment(rows []SchemaRow) (*segment, error) {
	rowids := make([]uint64, 0, len(rows))
	stored := make([][]Value, 0, len(rows))
	// ONE PASS for every rootpage, not one pass PER ROW. catalogRootpageFor walks
	// rows 0..i to find i's number, and calling it inside this loop made rendering
	// the catalog O(rows^2) -- each step re-deciding, by LEXING the stored CREATE
	// text, whether every earlier object has storage.
	//
	// That is per STATEMENT, because a held session rebuilds its pager for each
	// one, and it was the single largest allocator in a prepared point lookup:
	// isCreateVirtualTableSQL was 33% of all lexing, and lexing was 23% of every
	// byte the query allocated.
	roots := catalogRootpages(rows)
	own := schemaRowidsUsable(rows)
	for i, r := range rows {
		if own {
			rowids = append(rowids, uint64(r.Rowid))
		} else {
			rowids = append(rowids, uint64(i+1))
		}
		stored = append(stored, []Value{
			{Typ: Text, S: []byte(r.Type)},
			{Typ: Text, S: []byte(r.Name)},
			{Typ: Text, S: []byte(r.TblName)},
			// ROOTPAGE: a small dense number per object with storage, 0 for the ones
			// without -- see catalogRootpageFor.
			//
			// It used to be 0 for everything, and READING the column was declined on
			// top of that. Both were wrong in the same way: a database that cannot
			// answer is worse than one that answers its own number, and C's own
			// value here is not comparable between two storage formats anyway (the
			// differential harness blanks this column on both sides for exactly that
			// reason). What this must NOT carry is the SYNTHETIC routing key
			// (segRootBase + i): that leaked "rootpage 1000" where C reports 2.
			//
			// The resolver is unaffected: it reads ReadOnlyPager.schemaRows, which
			// keeps the routing key. This is the rendered TABLE.
			{Typ: Int, I: int64(roots[i])},
			sqlValueForSchemaRow(r.SQL),
		})
	}
	return buildSchemaSegmentFromValues(rowids, stored)
}

// buildSchemaSegmentFromValues is buildSchemaSegment from the catalog's RAW
// five-column values, for the one caller that has them: a session holding a
// direct sqlite_schema write, whose rows come from the overlay
// (wsCatalogWithOverlay, schema_write_direct.go).
//
// It exists because SchemaRow cannot carry them. Its Type/Name/TblName are Go
// strings, so a NULL and an empty string are the same value in it -- and
// "UPDATE sqlite_master SET tbl_name=NULL" then read back as '' where C reads
// back NULL (corruptM.test#0, whose whole point is what the catalog can be made
// to hold).
func buildSchemaSegmentFromValues(rowids []uint64, stored [][]Value) (*segment, error) {
	cols := []columnInfo{{Name: "type"}, {Name: "name"}, {Name: "tbl_name"},
		{Name: "rootpage"}, {Name: "sql"}}
	raw, err := buildSegment(cols, rowids, stored)
	if err != nil {
		return nil, fmt.Errorf("engine: open: building sqlite_master: %w", err)
	}
	return openSegment(raw)
}

// openSegmentsUnlocked is Open WITHOUT taking the shared lock, for a caller
// that already holds the segment file's write lock.
//
// It exists because locks here are OFD byte-range locks, owned by the open file
// DESCRIPTION: a writer holding EXCLUSIVE that then opens a second descriptor and
// asks for SHARED conflicts with ITSELF and waits out the whole busy timeout. A
// rewrite does exactly that -- it holds the write lock and then reads the pair it
// is about to replace -- and every CREATE TABLE answered "database is locked".
// Two ATTACH aliases of one file are the same self-conflict (attach_write.go).
func openSegmentsUnlocked(path string) (*ReadOnlyPager, error) {
	f, st, err := readSegmentPairUnlocked(path)
	if err != nil {
		return nil, err
	}
	return buildSegmentPager(path, f, st)
}

// readSegmentPairUnlocked reads the segment file and replays its delta, taking no
// lock of its own.
func readSegmentPairUnlocked(path string) (*SegmentFile, *segDeltaState, error) {
	f, err := OpenSegmentFile(path)
	if err != nil {
		return nil, nil, err
	}
	// MAPPED, not read: the replayed rows alias the mapping, which the state owns
	// and the pager releases (segDeltaState.mapping). A read put the whole log on
	// the heap of every connection that opened the file while it was long -- a
	// 2.83 GB commit, for the window before its compaction folded it.
	m, data, derr := mapSegDelta(segDeltaPath(path))
	if derr != nil {
		f.Close()
		return nil, nil, derr
	}
	st, rerr := replaySegDelta(data, f.srcChangeCounter, f.srcPageCount)
	if rerr != nil {
		f.Close()
		if m != nil {
			m.Close()
		}
		return nil, nil, rerr
	}
	st.mapping = m
	return f, st, nil
}

// Tables is the table names a segment file holds, without opening it as a
// database -- for a caller that wants to know what is in a file.
func Tables(path string) ([]string, error) {
	f, err := OpenSegmentFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]string, 0, len(f.Tables()))
	for _, t := range f.Tables() {
		out = append(out, t.Name)
	}
	return out, nil
}

// schemaSQLOf is the CREATE text for one object in a segment file, used by the
// tests that compare a segment open's catalog against the source database's.
func schemaSQLOf(rows []SchemaRow, name string) string {
	for _, r := range rows {
		if strings.EqualFold(r.Name, name) {
			return r.SQL
		}
	}
	return ""
}

// buildSegmentPager wires a read pager over a segment file and its replayed delta.
func buildSegmentPager(path string, f *SegmentFile, st *segDeltaState) (*ReadOnlyPager, error) {
	p := newReadOnlyPager()

	src := &segSource{file: f, byRoot: map[uint32][]*segment{}, tableOf: map[uint32]int{}}
	if !st.empty() {
		src.delta = st
	}
	var rows []SchemaRow
	// keys is each row's place in the catalog order the file recorded (see
	// catalogOrderKey), parallel to rows.
	var keys []catalogOrderKey
	for ti, t := range f.Tables() {
		keys = append(keys, catalogOrderKey{t.Rank, 0})
		segs, serr := t.openSegments()
		if serr != nil {
			f.Close()
			st.release()
			return nil, serr
		}
		root := f.RootOf(ti)
		src.byRoot[root] = segs
		src.tableOf[root] = ti
		rows = append(rows, SchemaRow{
			Type: "table", Name: t.Name, TblName: t.Name, RootPage: root, SQL: t.SQL, Rowid: t.Rowid,
		})
	}
	// AUTOMATIC INDEXES, re-derived from each table's own CREATE text the way
	// OpenWrite re-derives them (buildAutoIndexes, index_write.go).
	//
	// They are not in the catalog and should not be: an automatic index has no SQL
	// of its own -- its sqlite_schema row carries NULL -- so the converter skips it
	// and the table's own UNIQUE/PRIMARY KEY constraint is what rebuilds it. But the
	// ROW still has to exist, because it is what names the index: without it
	// "PRAGMA index_xinfo('sqlite_autoindex_wu_1')" answered NOTHING for a table
	// whose PRIMARY KEY is right there in its DDL, and anything else that walks the
	// catalog looking for a table's indexes saw none.
	for _, t := range f.Tables() {
		if strings.TrimSpace(t.SQL) == "" {
			continue
		}
		def, derr := parsePragmaTableDef(t.SQL)
		if derr != nil {
			continue // not parseable as a CREATE TABLE: nothing to derive
		}
		// An automatic index's ROWID follows its table's: sqlite3StartTable takes
		// the table's row first (build.c:1377) and each constraint's
		// sqlite3CreateIndex the next one (build.c:4460) -- a WITHOUT ROWID
		// table's PRIMARY KEY, which writes no row, taking none.
		autoRowid := t.Rowid
		for i, spec := range def.autoIdx {
			// A WITHOUT ROWID table's PRIMARY KEY constraint gets NO row, which is
			// C SQLite's own shape: it names that index but writes no
			// sqlite_schema row for it, and pragmaIndexInfo's own
			// withoutRowidAutoPKOwner path depends on the absence to know to append
			// the table's non-key columns. Synthesizing one made index_xinfo report
			// the key columns alone.
			//
			// The NUMBER still counts it: C numbers automatic indexes in constraint
			// order, PRIMARY KEY included, so the UNIQUE constraint after a skipped
			// PK is _2 and not _1.
			if def.withoutRowid && spec.kind == "pk" {
				continue
			}
			keys = append(keys, catalogOrderKey{t.Rank, i + 1})
			var rid int64
			if t.Rowid > 0 {
				autoRowid++
				rid = autoRowid
			}
			rows = append(rows, SchemaRow{
				Type: "index", Name: fmt.Sprintf("sqlite_autoindex_%s_%d", t.Name, i+1),
				TblName: t.Name, RootPage: 0, SQL: "", Rowid: rid,
			})
		}
	}
	// The catalog, with RootPage 0 on every index so the planner scans rather than
	// seeking into data this file does not carry. A view or trigger has no root
	// page in SQLite either.
	for _, o := range f.Catalog().Objects {
		// A VIRTUAL TABLE's row is "table" with rootpage 0, which is what C writes
		// for one too -- so table_list can type it "virtual", table_xinfo can answer
		// for it, and its shadow tables type as "shadow" instead of "table".
		keys = append(keys, catalogOrderKey{o.Rank, 0})
		rows = append(rows, SchemaRow{
			Type: o.Type, Name: o.Name, TblName: o.TblName, RootPage: 0, SQL: o.SQL, Rowid: o.Rowid,
		})
	}
	rows = sortCatalogRows(rows, keys)
	// sqlite_master AS A TABLE. Schema() is satisfied from schemaRows below, but a
	// SQL query naming sqlite_master goes through the ordinary table path, so the
	// catalog needs a segment at the schema root or "SELECT ... FROM sqlite_master"
	// returns nothing at all. Its content mirrors schemaRows exactly, so the two
	// cannot disagree.
	// An EMPTY schema registers an empty segment LIST, not an empty segment:
	// buildSegment refuses zero rows, and what a reader needs here is a root it
	// recognises that yields nothing -- otherwise "SELECT count(*) FROM
	// sqlite_master" on a fresh database falls through to a b-tree page 1 that
	// does not exist.
	src.byRoot[schemaRootPage] = []*segment{}
	if len(rows) > 0 {
		masterSeg, merr := buildSchemaSegment(rows)
		if merr != nil {
			f.Close()
			st.release()
			return nil, merr
		}
		src.byRoot[schemaRootPage] = []*segment{masterSeg}
	}

	p.segs = src
	p.schemaRows = rows
	p.schemaLoaded = true
	// ...and the schema cookie, which a segment file keeps in its catalog because it
	// has no SQLite header to keep it in. See ConvertedCatalog.SchemaVersion.
	p.meta.schemaCookie = f.Catalog().SchemaVersion
	// ...and the two application-owned words, which live there for the same
	// reason (ConvertedCatalog.UserVersion).
	p.meta.userVersion, p.meta.applicationID = f.Catalog().UserVersion, f.Catalog().ApplicationID
	// ...and "wal" (ConvertedCatalog.JournalWAL), where journalMode() looks.
	p.meta.wal = f.Catalog().JournalWAL != 0
	p.meta.captureGuard = f.Catalog().CaptureGuard
	// ...and the text encoding, which every value decoded from this file is in.
	//
	// Stamped even when it is UTF-8, because the ZERO value has its own meaning:
	// a file with no schema yet has declared no encoding, and that is what lets
	// ATTACH treat it the way C treats an empty database -- no encoding to
	// disagree with (see attach.go's agreement check).
	p.meta.encoding = TextEncoding(f.Catalog().Encoding)
	p.segEncodingDeclared = f.Catalog().Encoding != 0
	p.mainPath = path // "PRAGMA page_count" measures the file; see segFilePages
	if ps := f.Catalog().PageSize; ps != 0 {
		p.meta.pageSize = ps // "PRAGMA page_size"; see ConvertedCatalog.PageSize
	}
	if av := f.Catalog().AutoVacuumPlus1; av != 0 {
		p.segAutoVacuum = int(av - 1) // "PRAGMA auto_vacuum"; see ConvertedCatalog.AutoVacuumPlus1
	}
	// Close releases the mapping through the pager's own closer, so a caller
	// closes a segment pager exactly as it closes any other.
	p.closer = segFileCloser{f, st}
	return p, nil
}

// catalogRootpageFor is the ROOTPAGE a rendered sqlite_master row reports: a
// dense number from 2 upwards, in catalog order, for each object that HAS
// storage, and 0 for each one that does not.
//
// Why a number at all, on a format with no pages: because C's own rule is "0 for
// a view, a trigger or a virtual table, a page number otherwise", and an
// application that groups or joins on this column needs the distinction. 2
// upwards in creation order is also what C allocates for a database built by a
// linear sequence of CREATEs, so the number usually IS C's -- and where it is not
// (after DROP churn, which lets C reuse a freed page), the value is
// implementation-defined storage detail that no two formats could agree on. The
// differential harness blanks the column on both sides and says so in those terms
// (queryResultsMatch, pureengine_test.go).
func catalogRootpageFor(rows []SchemaRow, i int) uint32 {
	if i < 0 || i >= len(rows) {
		return 0
	}
	return catalogRootpages(rows)[i]
}

// catalogRootpages is every row's rootpage in ONE pass. The per-row entry point
// above is kept for callers that want a single number and is now defined in terms
// of this, so the two cannot disagree about which objects have storage.
func catalogRootpages(rows []SchemaRow) []uint32 {
	out := make([]uint32, len(rows))
	next := uint32(2)
	for k := range rows {
		if !catalogRowHasStorage(rows[k]) {
			continue
		}
		out[k] = next
		next++
	}
	// An ALIAS keeps its own slot -- C's orphaned b-tree still occupies its page,
	// so nothing after it renumbers -- and shows its owner's number, which is
	// what its catalog row holds.
	for k := range rows {
		if rows[k].AliasOf == "" {
			continue
		}
		for j := range rows {
			if rows[j].Type == "table" && rows[j].Temp == rows[k].Temp && equalFoldName(rows[j].Name, rows[k].AliasOf) {
				out[k] = out[j]
				break
			}
		}
	}
	return out
}

// catalogRowHasStorage reports whether a catalog row describes an object with a
// b-tree of its own in C: a table that is not VIRTUAL, or an index. A view, a
// trigger and a virtual table have none, and C writes 0 for each.
func catalogRowHasStorage(r SchemaRow) bool {
	switch r.Type {
	case "table":
		return !isCreateVirtualTableSQL(r.SQL)
	case "index":
		return true
	}
	return false
}
