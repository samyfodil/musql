// This file implements fts5's external content tables -- "content=<table>",
// optionally with "content_rowid=<column>" (FTS5_CONTENT_EXTERNAL;
// fts5ConfigParseSpecial picks the mode, fts5ConfigMakeExprlist composes the
// "T.<content_rowid>, T.<col1>, ..." every read runs). Contentless tables are
// fts5_contentless.go.
//
// # What changes
//
//   - No %_content shadow table (sqlite3Fts5StorageOpen, sqlite3Fts5DropAll,
//     sqlite3Fts5StorageRename handle it only for NORMAL/UNINDEXED).
//   - The columns are still the declared ones: unlike fts4, fts5 never consults
//     the content table at CREATE, so "content=nosuchtable" creates.
//   - A read without MATCH scans the content table (FTS5_STMT_SCAN_ASC); the
//     index is not consulted.
//   - A MATCH takes rowids from the index and columns from the content table
//     (FTS5_STMT_LOOKUP); a missing content row is FTS5_CORRUPT, "fts5: missing
//     row %lld from content table %s".
//   - A write never touches the content table. DELETE reads the current content
//     row to know which postings to remove; the 'delete' command supplies the
//     values instead. 'delete-all' is allowed.
//
// # Why this engine verifies
//
// C fts5 edits its index incrementally, so drift between index and content
// table is carried along. This engine re-encodes the whole index from the
// documents after every write (fts5SyncShadows), so it must know the document
// set exactly. An external-content table stores it nowhere, so it is
// reconstructed -- %_docsize's rowids paired with content rows -- and checked by
// re-encoding and comparing with the shadow bytes in the file (fts5ExtVerify).
// The encoding is deterministic, so a byte match proves the reconstruction;
// anything else is declined via fts5Store.extErr, never approximated. The cost
// is charged lazily, at the first sync of a write or first MATCH of a read.
//
// A file C left in several segments therefore scans fine but declines a MATCH,
// since this engine's single-segment encoding differs. 'rebuild' converts it.
package engine

import (
	"bytes"
	"fmt"
	"maps"
	"sort"
	"strings"
)

// fts5ExtRows is one external content table's rows, projected into the fts5
// table's own column shape: fts5ConfigMakeExprlist's "T.<content_rowid>,
// T.<col1>, ..." reduced to the values it selects.
type fts5ExtRows struct {
	rowids []int64           // ascending, distinct
	byID   map[int64][]Value // rowid -> len(colNames) values, in fts5 column order
	// locales is the locale of every value the content table holds as an
	// fts5_locale() value, for a locale=1 table (fts5_locale.go). Unlike an
	// internal-content table's, these are not read out of an l<i> column --
	// there is none -- they are carried INSIDE the content table's own value,
	// which fts5TextFromStmt (fts5_main.c:2264) unwraps on every read under
	// "bLocale && eContent==FTS5_CONTENT_EXTERNAL". Empty for every other kind.
	locales map[int64][]Value
}

// fts5ExtShadows is the part of an external-content fts5 table's shadow set
// fts5ExtVerify compares against, read out of a snapshot in one pass.
type fts5ExtShadows struct {
	data    map[int64][]byte // %_data id -> block bytes
	idx     []fts5IdxRow     // %_idx rows, as stored
	docsize map[int64][]byte // %_docsize id -> sz blob
	// origins is %_docsize's THIRD column, which only a contentless_delete=1
	// table has (fts5_contentless.go). Empty for every other kind.
	origins map[int64]int64
	pgsz    int
	cookie  uint32
}

// fts5ExtRowsOf reads st's content table and projects it. The errors are the
// ones C fts5 fails a read with: a content table that is not there at all,
// and a declared fts5 column the content table has no column of that name for
// (its "SELECT T.'a', ..." simply does not resolve).
func (p *ReadOnlyPager) fts5ExtRowsOf(st *fts5Store) (*fts5ExtRows, error) {
	rt, err := p.resolveTable(st.extContent)
	if err != nil {
		return nil, err
	}
	var records [][]Value
	var btreeRowids []int64
	rowids, recs, rerr := p.RowsOfRoot(rt.root)
	if rerr != nil {
		return nil, rerr
	}
	for i, rid := range rowids {
		// A segment row of a WITHOUT ROWID table is already in declared
		// order, and its position is no rowid -- fts5ExtRowsIn's rule.
		if rt.withoutRowid {
			records = append(records, recs[i])
			continue
		}
		records = append(records, normalizeRow("", rt.cols, rt.ipkIndex, rid, recs[i]))
		btreeRowids = append(btreeRowids, int64(rid))
	}
	return fts5ExtProject(st, fts5ExtTable{cols: rt.cols, withoutRowid: rt.withoutRowid}, btreeRowids, records)
}

// fts5ExtRowsIn is fts5ExtRowsOf read out of the WRITE session instead of a
// snapshot -- what 'rebuild' needs, since it must see the content table as the
// statement has left it rather than as OpenWrite found it.
//
// The content table named here is an ORDINARY table whose row store may not be
// set up yet (table_load.go), so it is loaded before its rows are read.
func (db *DB) fts5ExtRowsIn(st *fts5Store) (*fts5ExtRows, error) {
	tbl := db.findTableMeta(st.extContent)
	if tbl == nil {
		return nil, fmt.Errorf("engine: no such table: main.%s", st.extContent)
	}
	if err := db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	rowids := make([]uint64, 0, tbl.rows.len())
	for rid := range tbl.rows.all() {
		rowids = append(rowids, rid)
	}
	sort.Slice(rowids, func(a, b int) bool { return rowidLess(rowids[a], rowids[b]) })
	records := make([][]Value, 0, len(rowids))
	btreeRowids := make([]int64, 0, len(rowids))
	for _, rid := range rowids {
		// A WITHOUT ROWID table's map key is an INTERNAL identifier, never a
		// rowid (schema_write.go), so it is not offered as one.
		if tbl.withoutRowid {
			records = append(records, tbl.rows.row(rid))
			continue
		}
		records = append(records, normalizeRow("", tbl.cols, tbl.ipkIndex, rid, tbl.rows.row(rid)))
		btreeRowids = append(btreeRowids, int64(rid))
	}
	return fts5ExtProject(st, fts5ExtTable{cols: tbl.cols, withoutRowid: tbl.withoutRowid}, btreeRowids, records)
}

// fts5ExtTable is the little a projection needs to know about the content
// table, so that the read snapshot's resolvedTable and the write session's
// tableMeta can both supply it.
type fts5ExtTable struct {
	cols         []columnInfo
	withoutRowid bool
}

// fts5ExtProject turns the content table's rows (already in its own declared
// column order) into the fts5 table's shape. btreeRowids is parallel to
// records for an ordinary rowid table and nil for a WITHOUT ROWID one, which
// has no rowid for content_rowid's default spelling to resolve to.
func fts5ExtProject(st *fts5Store, rt fts5ExtTable, btreeRowids []int64, records [][]Value) (*fts5ExtRows, error) {
	pick := make([]int, len(st.colNames))
	for i, want := range st.colNames {
		pick[i] = -1
		for j, have := range rt.cols {
			if strings.EqualFold(have.Name, want) {
				pick[i] = j
				break
			}
		}
		if pick[i] < 0 {
			return nil, fmt.Errorf("engine: fts5 table %s: its content table %s has no column %q", st.name, st.extContent, want)
		}
	}
	// content_rowid= usually names a declared column, but its DEFAULT is
	// "rowid", which on an ordinary rowid table resolves to the table's own
	// rowid rather than to any column. normalizeRow has already substituted the
	// rowid into an INTEGER PRIMARY KEY column, so a declared match is enough
	// for that case too.
	ridPick := -1
	for j, have := range rt.cols {
		if strings.EqualFold(have.Name, st.extRowid) {
			ridPick = j
			break
		}
	}
	useBtreeRowid := false
	if ridPick < 0 {
		// content_rowid's default is the bare name "rowid", which on an
		// ordinary rowid table is the table's own key rather than a declared
		// column. A WITHOUT ROWID table has no such key, and C fts5 fails
		// every read of one with "no such column: T.rowid".
		if !isRowidAlias(st.extRowid) || rt.withoutRowid || len(btreeRowids) != len(records) {
			return nil, fmt.Errorf("engine: fts5 table %s: its content table %s has no column %q for content_rowid", st.name, st.extContent, st.extRowid)
		}
		useBtreeRowid = true
	}
	out := &fts5ExtRows{byID: make(map[int64][]Value, len(records))}
	for ri, rec := range records {
		var rv Value
		if useBtreeRowid {
			rv = Value{Typ: Int, I: btreeRowids[ri]}
		} else if ridPick < len(rec) {
			rv = rec[ridPick]
		}
		if rv.Typ != Int {
			// fts5 reads this column with sqlite3_column_int64, which COERCES.
			// Reproducing the coercion is not the risk -- two content rows
			// coercing to the same rowid is, since the index keys on it -- so
			// the whole table is declined rather than the row guessed at.
			return nil, fmt.Errorf("engine: fts5 table %s: content table %s has a non-integer %s (%s), which C fts5 silently coerces to an integer rowid", st.name, st.extContent, st.extRowid, valueTypeName(rv))
		}
		if _, dup := out.byID[rv.I]; dup {
			return nil, fmt.Errorf("engine: fts5 table %s: content table %s has two rows with %s = %d", st.name, st.extContent, st.extRowid, rv.I)
		}
		vals := make([]Value, len(pick))
		for i, j := range pick {
			if j < len(rec) {
				vals[i] = rec[j]
			}
		}
		// An external-content table has no l<i> column, so a locale=1 one
		// carries its locales INSIDE the content table's own values -- and
		// fts5TextFromStmt (fts5_main.c:2264) unwraps them on EVERY read, under
		// "bLocale && eContent==FTS5_CONTENT_EXTERNAL". Leaving them wrapped is
		// a wrong answer twice over: the column reads back as the raw blob, and
		// the tokenizer indexes the 16-byte header along with the text.
		if err := fts5LocaleUnwrapExtRow(st, out, rv.I, vals); err != nil {
			return nil, err
		}
		out.byID[rv.I] = vals
		out.rowids = append(out.rowids, rv.I)
	}
	sort.Slice(out.rowids, func(a, b int) bool { return out.rowids[a] < out.rowids[b] })
	return out, nil
}

// isRowidAlias reports whether name is one of SQLite's three spellings of a
// rowid table's implicit key column.
func isRowidAlias(name string) bool {
	return strings.EqualFold(name, "rowid") || strings.EqualFold(name, "oid") || strings.EqualFold(name, "_rowid_")
}

// valueTypeName names v's storage class for an error message.
func valueTypeName(v Value) string {
	switch v.Typ {
	case Null:
		return "NULL"
	case Int:
		return "integer"
	case Float:
		return "real"
	case Text:
		return "text"
	default:
		return "blob"
	}
}

// fts5ExtShadowRows reads one shadow table. Two of the five (%_idx and
// %_config) are WITHOUT ROWID; the returned rowids are meaningful only for the
// rowid tables.
func (p *ReadOnlyPager) fts5ExtShadowRows(name string) (rowids []uint64, records [][]Value, err error) {
	rt, err := p.resolveTable(name)
	if err != nil {
		return nil, nil, err
	}
	// A WITHOUT ROWID shadow (fts5's %_idx is one) is a row store like any
	// other, holding its values in declared order.
	return p.RowsOfRoot(rt.root)
}

// fts5ExtShadowsOf reads the shadow tables fts5ExtVerify compares against.
// hasDocsize is the table's columnsize= setting: a columnsize=0 table has no
// %_docsize at all (fts5_shadow.go's createShadowTables, matching
// fts5_storage.c's sqlite3Fts5StorageOpen), so reading it would be an error
// rather than an empty result.
func (p *ReadOnlyPager) fts5ExtShadowsOf(name string, hasDocsize bool) (*fts5ExtShadows, error) {
	sh := &fts5ExtShadows{data: map[int64][]byte{}, docsize: map[int64][]byte{}, origins: map[int64]int64{}, pgsz: fts5PageSize}
	ids, recs, err := p.fts5ExtShadowRows(name + "_data")
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		blk, ok := fts5RowBlockBytes(recs[i])
		if !ok {
			return nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", int64(id), name+"_data")
		}
		sh.data[int64(id)] = blk
	}
	if rec, ok := sh.data[fts5StructureRowid]; ok && len(rec) >= 4 {
		sh.cookie = uint32(rec[0])<<24 | uint32(rec[1])<<16 | uint32(rec[2])<<8 | uint32(rec[3])
	}
	_, irecs, err := p.fts5ExtShadowRows(name + "_idx")
	if err != nil {
		return nil, err
	}
	for _, rec := range irecs {
		if len(rec) < 3 {
			return nil, fmt.Errorf("fts5: corrupt %s row", name+"_idx")
		}
		sh.idx = append(sh.idx, fts5IdxRow{segid: rec[0].I, term: append([]byte(nil), rec[1].S...), pgno: rec[2].I})
	}
	// A columnsize=0 table has no %_docsize to read: sh.docsize stays empty and
	// fts5_contentless.go's reconstruction sizes the documents from the postings
	// instead.
	var dids []uint64
	var drecs [][]Value
	if hasDocsize {
		dids, drecs, err = p.fts5ExtShadowRows(name + "_docsize")
		if err != nil {
			return nil, err
		}
	}
	for i, id := range dids {
		blob, ok := fts5RowBlockBytes(drecs[i])
		if !ok {
			return nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", int64(id), name+"_docsize")
		}
		sh.docsize[int64(id)] = blob
		if len(drecs[i]) >= 3 && drecs[i][2].Typ == Int {
			sh.origins[int64(id)] = drecs[i][2].I
		}
	}
	// The page budget lives in %_config, exactly as it does for an ordinary
	// table (fts5_config.go's fts5ConfigPgsz); an absent or out-of-range value
	// means the default.
	if _, crecs, cerr := p.fts5ExtShadowRows(name + "_config"); cerr == nil {
		for _, rec := range crecs {
			if len(rec) >= 2 && rec[0].Typ == Text && strings.EqualFold(string(rec[0].S), "pgsz") {
				if n, ok := fts5ConfigInt(rec[1]); ok {
					rng := fts5ConfigCommands["pgsz"]
					if t := int32(n); t >= rng[0] && t <= rng[1] {
						sh.pgsz = int(t)
					}
				}
			}
		}
	}
	return sh, nil
}

// fts5ExtDocsizeSizes decodes one %_docsize blob into its per-column token
// counts, the same varint-per-column encoding fts5EncodeDocsize writes.
func fts5ExtDocsizeSizes(blob []byte, nCol int) ([]int, bool) {
	out := make([]int, nCol)
	b := blob
	for i := 0; i < nCol; i++ {
		v, n := getVarint(b)
		if n == 0 {
			return nil, false
		}
		out[i] = int(v)
		b = b[n:]
	}
	return out, true
}

// fts5LoadExternal is fts5LoadStore for an external-content table: it
// reconstructs the INDEXED DOCUMENTS (this file's comment) into st.rows and
// records, in st.extErr, the reason this engine cannot serve the table when
// there is one. It never fails OpenWrite: a table this engine declines must
// not take every unrelated statement in the session down with it.
func fts5LoadExternal(rp *ReadOnlyPager, st *fts5Store, name string) {
	st.name = name
	src, err := rp.fts5ExtRowsOf(st)
	if err != nil {
		st.extErr = err
		return
	}
	sh, err := rp.fts5ExtShadowsOf(name, st.columnsize)
	if err != nil {
		st.extErr = err
		return
	}
	nCol := len(st.colNames)
	// %_docsize holds exactly one row per indexed document
	// (fts5_storage.c's fts5StorageInsertDocsize / FTS5_STMT_DELETE_DOCSIZE),
	// so its rowids ARE the index's document set.
	for id := range sh.docsize {
		if vals, ok := src.byID[id]; ok {
			st.rows[id] = append([]Value(nil), vals...)
			continue
		}
		// An indexed rowid the content table does not hold. Its document is
		// unrecoverable unless it contributed no tokens at all, which %_docsize
		// itself says: an all-zero size vector is a document with no postings,
		// and an all-NULL row reproduces it exactly.
		sizes, ok := fts5ExtDocsizeSizes(sh.docsize[id], nCol)
		if !ok {
			st.extErr = fmt.Errorf("engine: fts5 table %s: its %%_docsize row for rowid %d does not decode", name, id)
			return
		}
		empty := true
		for _, n := range sizes {
			if n != 0 {
				empty = false
			}
		}
		if empty {
			st.rows[id] = make([]Value, nCol)
			continue
		}
		// The document is UNRESOLVED: it has postings, and the only place they
		// could have come from is a row the content table no longer holds.
		// Real fts5 keeps such an entry until a 'delete' command names it and
		// supplies the values -- which is the one thing that can resolve it
		// here too (fts5ExtDeleteCommand), so it is recorded rather than made
		// an error outright.
		if st.extUnresolved == nil {
			st.extUnresolved = map[int64]bool{}
		}
		st.extUnresolved[id] = true
	}
	st.extBase = maps.Clone(st.rows)
	st.extShadows = sh
	st.extSource = src
}

// fts5ExtRowSourceGuard declines DELETE and UPDATE of an external-content
// table when the index and content table do not hold the same rowids. C drives
// those from xFilter, which scans the content table; this engine drives them
// from the indexed documents, the only rows it can un-index. The two agree only
// when the rowid sets do.
//
// Reads use the finer fts5ExtScanGuard, since a row with no tokens cannot
// change a MATCH.
func (db *DB) fts5ExtRowSourceGuard(vm *vtabMeta, table string) error {
	st, ok := vm.store.(*fts5Store)
	if !ok || st.extContent == "" {
		return nil
	}
	src, err := db.fts5ExtRowsIn(st)
	if err != nil {
		return err
	}
	drift := int64(0)
	found := len(src.byID) != len(st.rows)
	for rid := range src.byID {
		if _, ok := st.rows[rid]; !ok {
			found, drift = true, rid
			break
		}
	}
	if !found {
		for rid := range st.rows {
			if _, ok := src.byID[rid]; !ok {
				found, drift = true, rid
				break
			}
		}
	}
	if !found {
		return nil
	}
	return fmt.Errorf("engine: DELETE/UPDATE of the external-content fts5 table %s is not supported by this engine while its index and content table %s hold different rowids (rowid %d is in one and not the other): C fts5 drives both from a scan of the content table, and this engine can only drive them from the documents the index holds", table, st.extContent, drift)
}

// fts5ExtVerify is the check this file exists for: re-encode the reconstructed
// documents and compare the result with the shadow bytes already in the file.
// Equal images prove the reconstruction is the document set the index was
// built from; anything else is a table this engine must not rewrite.
func fts5ExtVerify(st *fts5Store, base map[int64][]Value, sh *fts5ExtShadows) error {
	nCol := len(st.colNames)
	rowids := make([]int64, 0, len(base))
	for rid := range base {
		rowids = append(rowids, rid)
	}
	sort.Slice(rowids, func(a, b int) bool { return rowids[a] < rowids[b] })
	docs := make([]fts5IndexDoc, 0, len(rowids))
	wantDocsize := make(map[int64][]byte, len(rowids))
	for _, rid := range rowids {
		vals := base[rid]
		cols := make([][]string, nCol)
		sizes := make([]int, nCol)
		for i := 0; i < nCol; i++ {
			var v Value
			if i < len(vals) {
				v = vals[i]
			}
			if !st.unindexed[i] {
				cols[i] = st.tok.tokenize(valueToText(v))
			}
			sizes[i] = len(cols[i])
		}
		wantDocsize[rid] = fts5EncodeDocsize(sizes)
		docs = append(docs, fts5IndexDoc{rowid: rid, cols: cols})
	}
	img, err := fts5BuildIndex(docs, nCol, st.prefixes, sh.pgsz, st.detail, nil)
	if err != nil {
		return err
	}
	fts5SetConfigCookie(img.data[fts5StructureRowid], sh.cookie)
	want := make(map[int64][]byte, len(img.data)+1)
	maps.Copy(want, img.data)

	mismatch := func(what string) error {
		return fmt.Errorf("engine: fts5 table %s: its %s does not match the index this engine builds from content table %s, so the two have drifted apart -- this engine re-encodes the whole index from the documents on every write (fts5_extcontent.go), and cannot rewrite an index it did not derive", st.name, what, st.extContent)
	}
	// The averages record is not required to equal a fresh recount, unlike
	// the other shadow tables. C fts5 moves its totals incrementally by what
	// each write's given values imply, so a 'delete' whose values do not
	// reproduce the row (fts5ExtDeleteCommand) moves only C's totals --
	// fts5delete.test and fts5secure4.test exercise this, and C's own bare
	// 'integrity-check' does not re-derive the record for external content
	// (sqlite3Fts5StorageIntegrity). So the record's decoded value is kept as
	// this table's drift from a fresh recount; it is zero for any table never
	// given a mismatched 'delete'.
	gotRow, gotTotals := fts5decodeAveragesLenient(sh.data[fts5AveragesRowid], nCol)
	st.extRowDrift = gotRow - img.nRow
	st.extColDrift = make([]int64, nCol)
	for i := range img.colTotals {
		st.extColDrift[i] = gotTotals[i] - img.colTotals[i]
	}
	if len(want)+1 != len(sh.data) {
		return mismatch("%_data")
	}
	for id, blk := range want {
		if got, ok := sh.data[id]; !ok || !bytes.Equal(got, blk) {
			return mismatch("%_data")
		}
	}
	if len(img.idx) != len(sh.idx) {
		return mismatch("%_idx")
	}
	// %_idx is WITHOUT ROWID and comes back in (segid, term) b-tree order,
	// which is not the order the encoder emits, so both sides are sorted.
	less := func(rows []fts5IdxRow) func(a, b int) bool {
		return func(a, b int) bool {
			if rows[a].segid != rows[b].segid {
				return rows[a].segid < rows[b].segid
			}
			return bytes.Compare(rows[a].term, rows[b].term) < 0
		}
	}
	a := append([]fts5IdxRow(nil), img.idx...)
	b := append([]fts5IdxRow(nil), sh.idx...)
	sort.Slice(a, less(a))
	sort.Slice(b, less(b))
	for i := range a {
		if a[i].segid != b[i].segid || a[i].pgno != b[i].pgno || !bytes.Equal(a[i].term, b[i].term) {
			return mismatch("%_idx")
		}
	}
	if len(wantDocsize) != len(sh.docsize) {
		return mismatch("%_docsize")
	}
	for id, blob := range wantDocsize {
		if got, ok := sh.docsize[id]; !ok || !bytes.Equal(got, blob) {
			return mismatch("%_docsize")
		}
	}
	return nil
}

// fts5ExtEnsureVerified runs fts5ExtVerify once per store, at the first moment
// an answer actually depends on the index. Reads that never touch it (a scan of
// the content table) and statements that never reach this fts5 table pay
// nothing.
func (s *fts5Store) fts5ExtEnsureVerified() error {
	if s.extContent == "" || s.extVerified {
		return nil
	}
	if s.extErr != nil {
		return s.extErr
	}
	if s.extShadows == nil {
		return fmt.Errorf("engine: internal error: fts5 table %s has no external-content snapshot to verify against", s.name)
	}
	for id := range s.extUnresolved {
		return fmt.Errorf("engine: fts5 table %s: rowid %d is in the index but not in content table %s, so the document its postings were built from cannot be recovered (C fts5 keeps such an entry until a 'delete' command supplies its values, and answers a MATCH that selects it with \"fts5: missing row %d from content table %s\")", s.name, id, s.extContent, id, s.extContent)
	}
	if err := fts5ExtVerify(s, s.extBase, s.extShadows); err != nil {
		s.extErr = err
		return err
	}
	s.extVerified = true
	return nil
}

// fts5ExtDeleteCommand is the "INSERT INTO t(t, rowid, <cols>) VALUES('delete',
// ...)" command channel, which only a contentless or external-content table has
// (fts5_main.c's fts5SpecialDelete). It removes from the index the postings the
// SUPPLIED values imply -- not the ones the content table currently implies --
// which is what makes it usable from an AFTER DELETE trigger, after the content
// row is already gone.
//
// This engine can only remove a document it holds, so the values given have to
// BE that document: anything else is a subtraction whose result is not the
// tokenization of any document set, which is precisely what C fts5 leaves
// behind and this engine cannot express.
func fts5ExtDeleteCommand(st *fts5Store, rowid int64, vals []Value) error {
	if st.contentless {
		// Same subtraction, over the documents fts5_contentless.go read back out
		// of the index -- which for a contentless table is the only copy there
		// is, so "the values given have to BE that document" is if anything
		// tighter here.
		if st.contentlessErr != nil {
			return st.contentlessErr
		}
		if st.clGhosted {
			return fts5GhostedErr(st.name)
		}
		have, indexed := st.tokens[rowid]
		if !indexed {
			return fmt.Errorf("engine: fts5: the 'delete' command names rowid %d, which the index of the contentless table %s holds no document for: C fts5 subtracts the given values' postings from the index whether or not they are there, leaving one this engine cannot re-derive", rowid, st.name)
		}
		if !fts5SameTokensAsValues(st, have, vals) {
			if !fts5ContentlessGhostable(st) {
				return fmt.Errorf("engine: fts5: the 'delete' command names rowid %d of the contentless table %s with values that do not tokenize to the document the index holds there: C fts5 tombstones every term they name and leaves the rowid partly indexed, which this engine holds only in a contentless table with %%_docsize and no prefix index", rowid, st.name)
			}
			ghost, ok := fts5ContentlessGhost(st, have, vals)
			if !ok {
				return fmt.Errorf("engine: fts5: the 'delete' command names rowid %d of the contentless table %s with values that do not tokenize to the document the index holds there, and whose per-column token COUNTS differ from it: C fts5 subtracts the GIVEN values' sizes from its averages record, which then no longer matches a recount of the documents, and this engine derives that record from the documents", rowid, st.name)
			}
			delete(st.rows, rowid)
			if ghost == nil {
				delete(st.tokens, rowid)
				return nil
			}
			st.tokens[rowid] = *ghost
			st.clGhosted = true
			return nil
		}
		delete(st.rows, rowid)
		delete(st.tokens, rowid)
		return nil
	}
	if st.extUnresolved[rowid] {
		// The one shape this can resolve: the index holds postings at this rowid
		// and the content table has no row (fts5content.test 4.7 deletes the
		// content row first, then names it). The given values become the
		// pre-statement document, and fts5ExtVerify decides whether they are
		// exactly what the index holds. Since they must be, the totals drift
		// adjustment below would be a no-op and is skipped.
		if st.extBase == nil {
			st.extBase = map[int64][]Value{}
		}
		st.extBase[rowid] = vals
		delete(st.extUnresolved, rowid)
		delete(st.rows, rowid)
		st.dropTokens(rowid)
		return nil
	}
	return fts5ExtDeleteOrdinary(st, rowid, vals)
}

// fts5ExtDeleteOrdinary is fts5ExtDeleteCommand's path for a rowid the
// extUnresolved shortcut does not cover -- the common case.
//
// fts5StorageDeleteFromIndex (fts5_storage.c:503-604) never consults the
// content table when given values (fts5_storage.c:520), and for every
// 'delete':
//
//  1. per indexed column, subtracts the given value's token count from the
//     column total, failing "database disk image is malformed" if any would go
//     negative (fts5_storage.c:577-584);
//  2. decrements the row counter, failing the same way below 1
//     (fts5_storage.c:590-594).
//
// The per-term tombstone (fts5_hash.c:358-385) uses ctx.iCol = -1 for the whole
// command (fts5_storage.c:535), so it is column-blind: a named term is removed
// at this rowid in every column.
//
// This engine holds each row as one document, so it can follow only when, for
// every indexed column, the union of named terms covers all of the column's
// terms or none (fts5ExtDeleteEffect). Anything else is declined -- after the
// totals arithmetic, so a statement C also corrupts reports exactly that.
// Totals are tracked as drift from a fresh recount (st.extRowDrift/
// extColDrift); see fts5ExtVerify and fts5Store.
func fts5ExtDeleteOrdinary(st *fts5Store, rowid int64, vals []Value) error {
	// The totals arithmetic below starts from st.extRowDrift/extColDrift,
	// which fts5ExtVerify is what seeds from the file -- and which otherwise
	// would not run until this command's own fts5SyncShadows call, too late
	// to be read here. Every OTHER caller of fts5ExtEnsureVerified runs after
	// a row mutation; this one deliberately runs before, since the drift it
	// needs describes the state as of the START of this statement. Safe to
	// call unconditionally: it is a no-op once extVerified is already true,
	// same as every other call site.
	if verr := st.fts5ExtEnsureVerified(); verr != nil {
		return verr
	}
	nCol := len(st.colNames)
	if len(st.extColDrift) != nCol {
		// A table this session created and has never reloaded from a file
		// (fts5_shadow.go's createShadowTables) skips fts5ExtVerify entirely --
		// "its index starts empty... so the store IS the truth" -- so nothing
		// has sized this yet. No prior session means no drift to carry.
		st.extColDrift = make([]int64, nCol)
	}
	have, indexed := st.rows[rowid]

	// The GIVEN values' token SET, unioned across every indexed column, and
	// (separately) each column's own RAW given token count -- the two
	// mechanisms fts5_storage.c keeps apart, per this function's doc comment.
	union := map[string]bool{}
	givenCount := make([]int64, nCol)
	for i := 0; i < nCol; i++ {
		if st.unindexed[i] {
			continue
		}
		var v Value
		if i < len(vals) {
			v = vals[i]
		}
		toks := st.tok.tokenize(valueToText(v))
		givenCount[i] = int64(len(toks))
		for _, t := range toks {
			union[t] = true
		}
	}

	naiveRow, naiveCols := fts5ExtNaiveTotals(st)
	newCols := make([]int64, nCol)
	for i := 0; i < nCol; i++ {
		newCols[i] = naiveCols[i] + st.extColDrift[i] - givenCount[i]
		if newCols[i] < 0 {
			return fmt.Errorf("database disk image is malformed")
		}
	}
	curRow := naiveRow + st.extRowDrift
	if curRow < 1 {
		return fmt.Errorf("database disk image is malformed")
	}
	newRow := curRow - 1

	allFull, allNone := true, true
	for i := 0; i < nCol; i++ {
		if st.unindexed[i] {
			continue
		}
		actual := map[string]bool{}
		if indexed && i < len(have) {
			for _, t := range st.tok.tokenize(valueToText(have[i])) {
				actual[t] = true
			}
		}
		full, none := fts5ExtDeleteEffect(actual, union)
		if !full && !none {
			return fts5ExtDeleteMismatchErr(st, rowid)
		}
		allFull, allNone = allFull && full, allNone && none
	}
	if !allFull && !allNone {
		// Every column individually is fully-covered or untouched, but not
		// the SAME one for all of them: some of the row would still match on
		// its untouched columns, a state this whole-document model has no way
		// to hold as anything other than "unchanged".
		return fts5ExtDeleteMismatchErr(st, rowid)
	}

	removed := indexed && allFull
	naiveRow2, naiveCols2 := naiveRow, append([]int64(nil), naiveCols...)
	if removed {
		naiveRow2--
		for i := 0; i < nCol; i++ {
			if st.unindexed[i] {
				continue
			}
			var v Value
			if i < len(have) {
				v = have[i]
			}
			naiveCols2[i] -= int64(len(st.tok.tokenize(valueToText(v))))
		}
	}
	st.extRowDrift = newRow - naiveRow2
	for i := 0; i < nCol; i++ {
		st.extColDrift[i] = newCols[i] - naiveCols2[i]
	}
	if removed {
		delete(st.rows, rowid)
		st.dropTokens(rowid)
	}
	return nil
}

// fts5ExtDeleteMismatchErr is fts5ExtDeleteOrdinary's decline for a column
// whose actual terms are only PARTLY named by the given values (or a row
// whose columns split between fully-named and untouched): a state C fts5
// leaves behind that this engine's whole-document model cannot hold.
func fts5ExtDeleteMismatchErr(st *fts5Store, rowid int64) error {
	return fmt.Errorf("engine: fts5: the 'delete' command names rowid %d of the external-content table %s with values that do not tokenize to the document the index holds there: C fts5 subtracts the GIVEN values' postings, which would leave the index holding the difference, and this engine re-encodes the whole index from the documents instead", rowid, st.name)
}

// fts5ExtDeleteEffect classifies one column's outcome under a 'delete'
// command's given values, matching C fts5's column-blind hash tombstone
// (fts5_hash.c:358-385, see fts5ExtDeleteOrdinary): full is true when every
// one of the column's actual terms was named (the union covers actual, so
// the whole column becomes unmatchable -- equivalent, for a whole-document
// model, to removing it); none is true when none of them were (untouched).
// Both are vacuously true for an empty column (nothing to cover, nothing
// left unmatched either way). Neither is the PARTIAL case this engine
// declines: some of the column's terms are gone and others still match, a
// state between "present" and "absent" a whole document cannot express.
func fts5ExtDeleteEffect(actual, union map[string]bool) (full, none bool) {
	full, none = true, true
	for t := range actual {
		if union[t] {
			none = false
		} else {
			full = false
		}
	}
	return full, none
}

// fts5ExtNaiveTotals is a fresh (nRow, per-column token count) over st.rows
// as it stands RIGHT NOW -- the same pair fts5BuildIndex derives for
// fts5SyncShadows's own averages record, computed directly rather than
// through a full index build, because fts5ExtDeleteOrdinary needs it before
// every 'delete' command's arithmetic, not just once per write.
func fts5ExtNaiveTotals(st *fts5Store) (nRow int64, colTotals []int64) {
	colTotals = make([]int64, len(st.colNames))
	for _, vals := range st.rows {
		nRow++
		for i := range colTotals {
			if st.unindexed[i] {
				continue
			}
			var v Value
			if i < len(vals) {
				v = vals[i]
			}
			colTotals[i] += int64(len(st.tok.tokenize(valueToText(v))))
		}
	}
	return nRow, colTotals
}

// fts5SameTokensAsValues reports whether an already-tokenized document is what
// vals tokenizes to, column by column, EXACT sequence and all -- the
// contentless 'delete' path's own check (fts5ExtDeleteCommand), which stays
// the older strict-equality rule (fts5_contentless.go's delete handling is
// untouched: see fts5ExtDeleteOrdinary's doc comment for why the
// external-content path now uses a looser, set-based one instead).
func fts5SameTokensAsValues(st *fts5Store, have fts5RowTokens, vals []Value) bool {
	for i := range st.colNames {
		if st.unindexed[i] {
			continue
		}
		var v Value
		if i < len(vals) {
			v = vals[i]
		}
		want := st.tok.tokenize(valueToText(v))
		if i >= len(have.cols) || len(have.cols[i]) != len(want) {
			return false
		}
		for j := range want {
			if have.cols[i][j] != want[j] {
				return false
			}
		}
	}
	return true
}

// fts5ContentlessGhost is what a mismatched 'delete' leaves of a contentless
// table's document: exactly the postings C fts5 keeps. fts5SpecialDelete does
// not check the given values (fts5_main.c:1837), and the tombstone is
// column-blind and per term (ctx.iCol = -1, fts5_storage.c:535): every named
// term at this rowid goes, in every column, and unnamed terms survive at their
// positions. %_docsize loses the row; the averages record loses one row and the
// given values' token counts (fts5_storage.c:577-594). So the row disappears
// from a plain scan but still matches a term the command did not name.
//
// The result marks a hole (fts5RowTokens.holes) at every named position, which
// fts5BuildIndex skips while keeping the rest's positions. nil means every
// position was named and the document is gone.
//
// ok is false -- declined -- when the given values' per-column token counts
// differ from the document's: C would then subtract a different size from the
// averages record than this engine can derive. The caller has already checked
// fts5ContentlessGhostable.
func fts5ContentlessGhost(st *fts5Store, have fts5RowTokens, vals []Value) (*fts5RowTokens, bool) {
	named := map[string]bool{}
	for i := range st.colNames {
		if st.unindexed[i] {
			continue
		}
		var v Value
		if i < len(vals) {
			v = vals[i]
		}
		given := st.tok.tokenize(valueToText(v))
		if i >= len(have.cols) || len(have.cols[i]) != len(given) {
			return nil, false
		}
		for _, t := range given {
			named[t] = true
		}
	}
	cols := make([][]string, len(have.cols))
	holes := make([][]bool, len(have.cols))
	kept := false
	for i, toks := range have.cols {
		cols[i] = append([]string(nil), toks...)
		holes[i] = make([]bool, len(toks))
		for j, t := range toks {
			if named[t] {
				holes[i][j] = true
			} else {
				kept = true
			}
		}
	}
	if !kept {
		return nil, true
	}
	return &fts5RowTokens{cols: cols, holes: holes}, true
}

// fts5GhostedErr is the decline for any further write to a contentless table
// holding a ghost, until a 'delete-all' clears it (fts5Store.clGhosted).
func fts5GhostedErr(name string) error {
	return fmt.Errorf("engine: fts5: the contentless table %s holds a document a mismatched 'delete' command left partly indexed (postings with no %%_docsize row, as C fts5 leaves it); this engine declines further writes to it until a 'delete-all'", name)
}

// fts5ExtDelete runs the 'delete' command:
//
//	INSERT INTO t(t, rowid, <col>, ...) VALUES('delete', <rowid>, <value>, ...)
//
// (fts5UpdateMethod -> fts5SpecialDelete). A non-integer rowid makes it a
// no-op (fts5content.test 5.1). Unnamed columns arrive as NULL. row is the
// already-evaluated VALUES row in stmt.cols order, as C receives apVal.
func (db *DB) fts5ExtDelete(vm *vtabMeta, st *fts5Store, stmt *insertStmt, row []Value, slot int) (bool, int, error) {
	nCol := len(st.colNames)
	vals := make([]Value, nCol)
	rowid := Value{Typ: Null}
	for i, c := range stmt.cols {
		if i == slot {
			continue
		}
		// A DECLARED column wins over the rowid alias, which is SQLite's own
		// name resolution: fts5 refuses a column literally named "rowid"
		// (buildStore) but not one named "oid" or "_rowid_", and such a column
		// shadows the alias.
		target := -1
		for j, n := range st.colNames {
			if strings.EqualFold(n, c) {
				target = j
				break
			}
		}
		if target < 0 && isRowidAlias(c) {
			target = -2
		}
		if target == -1 {
			return true, 0, fmt.Errorf("engine: fts5: the 'delete' command on %s may name only %s, rowid and %s's own columns, not %s", vm.name, vm.name, vm.name, c)
		}
		if target == -2 {
			rowid = row[i]
		} else {
			vals[target] = row[i]
		}
	}
	if rowid.Typ != Int {
		// fts5SpecialDelete's "if( eType1==SQLITE_INTEGER )": anything else
		// leaves the index untouched.
		return true, 1, nil
	}
	if derr := fts5ExtDeleteCommand(st, rowid.I, vals); derr != nil {
		return true, 0, derr
	}
	db.fts5NoteRowsRemoved(vm, 1)
	if serr := db.fts5SyncShadows(vm); serr != nil {
		return true, 0, serr
	}
	return true, 1, nil
}

// fts5ExtDiscardIndex records that this statement discards and rebuilds the
// whole index -- 'delete-all' and 'rebuild' (sqlite3Fts5StorageDeleteAll /
// sqlite3Fts5StorageRebuild). Neither reads the old index, so neither needs
// fts5ExtVerify. Both also reset C's totals (fts5_storage.c:798-832, 843), so
// any drift from a mismatched 'delete' is gone too.
func fts5ExtDiscardIndex(st *fts5Store) {
	st.extUnresolved = nil
	st.extVerified = true
	st.extRowDrift = 0
	st.extColDrift = nil
}

// fts5ExtScanGuard is the check a read of an external-content table needs even
// when it never touches the index. This engine materializes the content
// table's rows and re-applies the WHERE, so a MATCH's narrower row source (the
// index's rowids) must be reproducible from that set. A content row the index
// lacks is (fts5ExtMatchGuard refuses it). An indexed rowid the content table
// lacks is not -- C fails the MATCH with "fts5: missing row ..." -- so that is
// declined here, but only when the entry has postings.
func fts5ExtScanGuard(st *fts5Store, src *fts5ExtRows, sh *fts5ExtShadows) error {
	nCol := len(st.colNames)
	for rid, blob := range sh.docsize {
		if _, present := src.byID[rid]; present {
			continue
		}
		sizes, ok := fts5ExtDocsizeSizes(blob, nCol)
		if !ok {
			return fmt.Errorf("engine: fts5 table %s: its %%_docsize row for rowid %d does not decode", st.name, rid)
		}
		for _, n := range sizes {
			if n != 0 {
				return fmt.Errorf("engine: fts5 table %s: rowid %d is in the index and not in content table %s, so a MATCH selecting it is \"fts5: missing row %d from content table %s\" in C fts5 and this engine cannot tell a MATCH from a plain scan here", st.name, rid, st.extContent, rid, st.extContent)
			}
		}
	}
	return nil
}

// materializeFts5External presents an external-content fts5 table's rows: the
// CONTENT TABLE's, in content_rowid order. Real fts5's plain scan reads exactly
// that (FTS5_STMT_SCAN_ASC over zContent), and a MATCH narrows it to the
// index's rowids -- which fts5ExtScanGuard has established cannot select
// anything this row set does not already hold.
func (p *ReadOnlyPager) materializeFts5External(it FromItem, name string, cols []columnInfo, st *fts5Store) ([]columnInfo, [][]Value, []int64, error) {
	if err := p.fts5ExtRecursionGuard(name, map[string]bool{}); err != nil {
		return nil, nil, nil, err
	}
	if st.extErr != nil {
		if p.fts5ExtIndexOnlyMatch(it, name, st) {
			return cols, nil, nil, nil
		}
		return nil, nil, nil, st.extErr
	}
	// fts5ExtStoreOf has already read both out of this snapshot, so they are
	// taken from the store rather than read a second time.
	src, sh := st.extSource, st.extShadows
	if src == nil || sh == nil {
		return nil, nil, nil, fmt.Errorf("engine: internal error: fts5 table %s was not loaded from this snapshot", name)
	}
	if gerr := fts5ExtScanGuard(st, src, sh); gerr != nil {
		return nil, nil, nil, gerr
	}
	rows := make([][]Value, 0, len(src.rowids))
	for _, rid := range src.rowids {
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: rid}
		copy(row[1:], src.byID[rid])
		rows = append(rows, row)
	}
	return cols, rows, src.rowids, nil
}

// fts5ExtIndexOnlyMatch reports whether this read can be answered without the
// content table at all -- the one case an unreadable content source need not
// fail. C answers a MATCH from the index and reads content only when a column
// is fetched (fts5ColumnMethod -> fts5SeekCursor), while a plain scan reads the
// content table. So with a missing content table and an empty index, a MATCH
// returns zero rows and a scan errors. That is the rule implemented: an index
// with no documents makes a MATCH select nothing.
//
// ponytail: a non-empty index still declines. C answers a rowid-only
// projection there; serving it needs a projection-aware row source.
//
// Deliberately narrow otherwise:
//
//   - tvfWhere must be non-nil (withVtabWhere stamps it); nil means this row
//     source never saw the statement, so "no MATCH" would be an assumption.
//   - the MATCH must be a top-level AND conjunct; under an OR, C scans content
//     and errors.
//   - the MATCH must name this item by table name or explicit qualifier; a
//     bare column could belong to another FROM item.
//   - columnsize must be on: %_docsize's one-row-per-document is what makes
//     emptiness decidable.
func (p *ReadOnlyPager) fts5ExtIndexOnlyMatch(it FromItem, name string, st *fts5Store) bool {
	if it.tvfWhere == nil || !st.columnsize {
		return false
	}
	named := false
	for _, c := range it.tvfWhere {
		mx, ok := c.(MatchExpr)
		if !ok || mx.Not {
			continue
		}
		ce, ok := mx.X.(ColumnExpr)
		if !ok || ce.Schema != "" {
			continue
		}
		if ce.Qualifier != "" {
			if equalFoldName(ce.Qualifier, it.Alias) || equalFoldName(ce.Qualifier, it.Table) {
				named = true
			}
			continue
		}
		if equalFoldName(ce.Name, it.Table) {
			named = true
		}
	}
	if !named {
		return false
	}
	sh, err := p.fts5ExtShadowsOf(name, st.columnsize)
	return err == nil && len(sh.docsize) == 0
}

// fts5ExtMatchGuard is the read path's half of external content: a MATCH is the
// one answer that comes from the index, so this is where the verification is
// charged (memoized per snapshot) and C's index-driven row source reproduced.
//
// indexed is false for a content row the index has no document for; C's MATCH
// never returns such a row, so it is simply false here. That keeps a table
// created over an already-populated content table readable before 'rebuild'.
//
// The write path carries its own verified store and never reaches here.
func (ctx *evalCtx) fts5ExtMatchGuard(t *tableScope) (indexed bool, err error) {
	if t.isFts5 || ctx.pager == nil {
		return true, nil
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	sch, ok := ctx.pager.fts5SchemaTok(name)
	if !ok || sch.ext == nil {
		return true, nil
	}
	if !sch.ext.done {
		sch.ext.done = true
		sch.ext.docids, sch.ext.err = ctx.pager.fts5ExtCheckSnapshot(name)
	}
	if sch.ext.err != nil {
		return false, sch.ext.err
	}
	// The fts5 row shape's slot 0 is the hidden "rowid" column (vtab_fts5.go),
	// which materializeFts5External fills from the content table's
	// content_rowid= value.
	if t.offset < 0 || t.offset >= len(ctx.vals) || ctx.vals[t.offset].Typ != Int {
		return false, fmt.Errorf("engine: fts5 table %s: a MATCH over an external-content table needs the row's own rowid, which this row source does not carry", name)
	}
	return sch.ext.docids[ctx.vals[t.offset].I], nil
}

// fts5ExtStoreOf rebuilds an external-content fts5 table's store out of this
// read snapshot: its declared schema from the stored CREATE, its documents
// from the index's rowids and the content table. nil with a nil error means
// name is not an external-content fts5 table here.
func (p *ReadOnlyPager) fts5ExtStoreOf(name string) (*fts5Store, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Type != "table" || !strings.EqualFold(r.Name, name) || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		_, mod, args, _, perr := parseCreateVirtualTableStmt(r.SQL)
		if perr != nil || !strings.EqualFold(mod, "fts5") {
			continue
		}
		st, serr := (fts5Module{}).buildStore(r.Name, args)
		if serr != nil {
			return nil, serr
		}
		if st.extContent == "" {
			return nil, nil
		}
		fts5LoadExternal(p, st, name)
		return st, nil
	}
	return nil, nil
}

// fts5ExtCheckSnapshot rebuilds the external-content table's documents out of
// this snapshot, runs fts5ExtVerify over them, and returns the INDEXED rowid
// set a MATCH's row source is drawn from (%_docsize holds exactly one row per
// indexed document).
func (p *ReadOnlyPager) fts5ExtCheckSnapshot(name string) (map[int64]bool, error) {
	st, err := p.fts5ExtStoreOf(name)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, nil
	}
	if verr := st.fts5ExtEnsureVerified(); verr != nil {
		return nil, verr
	}
	docids := make(map[int64]bool, len(st.extShadows.docsize))
	for id := range st.extShadows.docsize {
		docids[id] = true
	}
	return docids, nil
}

// fts5ExtIsDrifted reports whether an external-content table's averages
// record differs from a fresh recount of its indexed documents -- the state a
// mismatched 'delete' leaves. bm25()/rank statistics (buildFts5AuxState) are
// gathered from the plain scan, which does not carry that drift, so the
// aux-state builder declines when this is true rather than score wrongly.
func (p *ReadOnlyPager) fts5ExtIsDrifted(name string) (bool, error) {
	st, err := p.fts5ExtStoreOf(name)
	if err != nil || st == nil {
		return false, err
	}
	if verr := st.fts5ExtEnsureVerified(); verr != nil {
		return false, verr
	}
	if st.extRowDrift != 0 {
		return true, nil
	}
	for _, d := range st.extColDrift {
		if d != 0 {
			return true, nil
		}
	}
	return false, nil
}

// fts5ExtRecursionGuard reproduces fts5_main.c's Fts5Config.bLock check: an
// fts5 table whose content= chain leads back to itself is
// "recursively defined fts5 content table" on every read, because fts5 raises
// it the moment xBestIndex is re-entered while its own storage statement is
// being prepared. Two shapes reach it -- "content=<itself>" and a pair naming
// each other (fts5content.test 7.1 and 7.2).
func (p *ReadOnlyPager) fts5ExtRecursionGuard(name string, seen map[string]bool) error {
	low := strings.ToLower(name)
	if seen[low] {
		return fmt.Errorf("engine: recursively defined fts5 content table")
	}
	seen[low] = true
	sch, ok := p.fts5SchemaTok(name)
	if !ok || sch.extContent == "" {
		return nil
	}
	if _, isFts5 := p.fts5SchemaTok(sch.extContent); !isFts5 {
		return nil
	}
	return p.fts5ExtRecursionGuard(sch.extContent, seen)
}
