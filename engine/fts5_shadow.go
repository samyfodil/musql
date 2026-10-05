// This file keeps an fts5 table's data where C keeps it: the shadow tables
// %_data, %_idx, %_content, %_docsize and %_config, created with the table and
// written on every mutation. With fts5_index.go it makes an fts5 database
// interchangeable (TestFts5FileInterchange gates both directions).
//
// The shadow tables' CREATE text is C's verbatim:
//
//	CREATE TABLE 't_data'(id INTEGER PRIMARY KEY, block BLOB)
//	CREATE TABLE 't_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID
//	CREATE TABLE 't_content'(id INTEGER PRIMARY KEY, c0, c1)
//	CREATE TABLE 't_docsize'(id INTEGER PRIMARY KEY, sz BLOB)
//	CREATE TABLE 't_config'(k PRIMARY KEY, v) WITHOUT ROWID
//
// A contentless_delete=1 table's %_docsize has a third column, "origin"
// (fts5DocsizeColumns).
//
// %_content holds row values verbatim, so a query with no MATCH reads it
// directly (materializeFts5), as C does.
//
// The index is rebuilt from the live rows after each mutation
// (fts5SyncShadows) rather than appended to -- a legal structure (what
// "optimize" produces) with no delete markers or merge scheduling. See
// fts5_index.go.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts5ShadowSuffixes is every shadow table an fts5 table can own, in the order
// C SQLite creates them (the order they appear in sqlite_schema). A
// columnsize=0 table simply has no %_docsize; dropShadowTables skips whatever
// is absent.
var fts5ShadowSuffixes = []string{"_data", "_idx", "_content", "_docsize", "_config"}

// createShadowTables creates a new fts5 table's shadow tables and seeds what C
// writes at CREATE: the %_config version row and the empty %_data averages and
// structure records. A columnsize=0 table has no %_docsize and is otherwise
// identical. isTemp puts the shadows in the table's own catalog, as fts3's
// createShadowTables does ("CREATE VIRTUAL TABLE temp.t1 USING fts5(x)" files
// every shadow in sqlite_temp_master).
func (m fts5Module) createShadowTables(db *DB, name string, st *fts5Store, isTemp bool) error {
	tempKw := ""
	if isTemp {
		tempKw = "TEMP "
	}
	var contentCols strings.Builder
	contentCols.WriteString("id INTEGER PRIMARY KEY")
	for i := range st.colNames {
		// FTS5_CONTENT_UNINDEXED stores ONLY the unindexed columns, keeping
		// each one's ORIGINAL index in its name -- fts5_storage.c's
		// sqlite3Fts5StorageOpen appends "c%d" for i only when
		// "eContent==FTS5_CONTENT_NORMAL || abUnindexed[i]". Verified against
		// the oracle: fts5(a, b UNINDEXED, c, content='',
		// contentless_unindexed=1) declares 't1_content'(id INTEGER PRIMARY
		// KEY, c1).
		if st.contentUnindexed && !st.unindexed[i] {
			continue
		}
		fmt.Fprintf(&contentCols, ", c%d", i)
	}
	// ...and a locale=1 table carries an "l<i>" column per INDEXED column after
	// them (fts5_locale.go).
	fts5LocaleAppendColumns(&contentCols, st)
	type shadow struct {
		suffix string
		sql    string
	}
	stmts := []shadow{
		{"_data", fmt.Sprintf("CREATE %sTABLE %s(id INTEGER PRIMARY KEY, block BLOB)", tempKw, fts3QuoteName(name+"_data"))},
		{"_idx", fmt.Sprintf("CREATE %sTABLE %s(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID", tempKw, fts3QuoteName(name+"_idx"))},
		{"_content", fmt.Sprintf("CREATE %sTABLE %s(%s)", tempKw, fts3QuoteName(name+"_content"), contentCols.String())},
		{"_docsize", fmt.Sprintf("CREATE %sTABLE %s(%s)", tempKw, fts3QuoteName(name+"_docsize"), fts5DocsizeColumns(st))},
		{"_config", fmt.Sprintf("CREATE %sTABLE %s(k PRIMARY KEY, v) WITHOUT ROWID", tempKw, fts3QuoteName(name+"_config"))},
	}
	if !st.columnsize {
		stmts = append(stmts[:3:3], stmts[4])
	}
	// An EXTERNAL-CONTENT or CONTENTLESS table has no %_content at all:
	// fts5_storage.c's sqlite3Fts5StorageOpen creates it only for
	// FTS5_CONTENT_NORMAL and FTS5_CONTENT_UNINDEXED. Its rows are the named
	// table's (fts5_extcontent.go), or nowhere at all (fts5_contentless.go).
	if st.extContent != "" || (st.contentless && !st.contentUnindexed) {
		stmts = append(stmts[:2:2], stmts[3:]...)
	}
	// A shadow name already taken fails the whole CREATE with nothing left
	// behind, as for fts3. Lookups are scoped to this table's catalog
	// (findTableMetaIn), not the temp-first unscoped lookup, so a plain
	// "CREATE TEMP TABLE t1_data(...)" cannot capture a main fts5 table's
	// seeding, or vice versa.
	scope := createScope(isTemp)
	created := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if err := db.CreateTable(s.sql); err != nil {
			for _, n := range created {
				if tbl := db.findTableMetaIn(scope, n); tbl != nil {
					db.removeTableAndIndexes(tbl)
				}
			}
			return err
		}
		created = append(created, name+s.suffix)
	}
	cfg := db.findTableMetaIn(scope, name+"_config")
	cfg.putRow(1, []Value{{Typ: Text, S: []byte("version")}, {Typ: Int, I: fts5CurrentVersion}})
	data := db.findTableMetaIn(scope, name+"_data")
	// Verbatim oracle output for a table with no rows yet: the averages record
	// is an EMPTY blob (nothing has been flushed) and the structure record is
	// four cookie bytes plus three zero varints.
	data.putRow(fts5AveragesRowid, []Value{{Typ: Null}, {Typ: Blob, S: []byte{}}})
	data.putRow(fts5StructureRowid, []Value{{Typ: Null}, {Typ: Blob, S: fts5EncodeStructure(1, 0, 0, fts5ContentlessV2(st, 0))}})
	// An EXTERNAL-CONTENT table created HERE needs no reconstruction and no
	// check of one: its index starts empty and every document it will ever hold
	// goes in through this session's own store, so the store IS the truth.
	// fts5LoadExternal is the other entry (a table recovered from a file), and
	// that one does have to reconstruct and verify.
	if st.extContent != "" {
		st.extBase = map[int64][]Value{}
		st.extVerified = true
	}
	return nil
}

// fts5DocsizeColumns is %_docsize's column list. A contentless_delete=1 table
// gets a third column, "origin" -- fts5_storage.c's sqlite3Fts5StorageOpen
// picks between exactly these two strings, and fts5contentless.test 3.1 pins
// both spellings.
func fts5DocsizeColumns(st *fts5Store) string {
	if st.contentlessDelete {
		return "id INTEGER PRIMARY KEY, sz BLOB, origin INTEGER"
	}
	return "id INTEGER PRIMARY KEY, sz BLOB"
}

// dropShadowTables removes every shadow table of the fts5 virtual table named
// name, which is what C SQLite's DROP TABLE on one does.
//
// sqlite3Fts5DropAll drops "<name>_content" only for FTS5_CONTENT_NORMAL, so
// an external-content table leaves a table of that name -- which is not its
// own, and may well be somebody's -- alone. That is read back off the stored
// module ARGUMENTS (the vtabMeta is still registered at this point; DropTable
// removes it after), never from the content table itself, which may have been
// dropped already.
func (m fts5Module) dropShadowTables(db *DB, name string) {
	keepContent := false
	if vt := db.findVtabMeta(name); vt != nil {
		// A CONTENTLESS table never created one either, so a table of that name
		// is just as much somebody else's there (sqlite3Fts5DropAll drops
		// "<name>_content" only for FTS5_CONTENT_NORMAL, which is neither).
		external, contentless := fts5ContentModeOf(vt.args)
		keepContent = external != "" || contentless
	}
	for _, suffix := range fts5ShadowSuffixes {
		if suffix == "_content" && keepContent {
			continue
		}
		if tbl := db.findTableMeta(name + suffix); tbl != nil {
			db.removeTableAndIndexes(tbl)
		}
	}
}

// fts5ModuleOf returns the fts5 module backing vm, if vm is one. It reports
// false whenever fts5 is not registered at all (RegisterFTS5 is opt-in), which
// is what keeps every call site below inert in a default build.
func fts5ModuleOf(vm *vtabMeta) (fts5Module, bool) {
	m, ok := lookupVtabModule(vm.module)
	if !ok {
		return fts5Module{}, false
	}
	fm, ok := m.(fts5Module)
	return fm, ok
}

// fts5CommandInsert handles fts5's command channel: an INSERT naming the table
// itself as the column carries a command. handled is false for an ordinary
// INSERT, which is unambiguous because fts5 refuses a column named for its
// table ("vtable constructor failed: tt"), as buildStore does.
//
//   - 'optimize' and 'rebuild' make the index what the content implies, which
//     fts5SyncShadows does on every write anyway; %_content, %_docsize and
//     %_config are untouched.
//   - 'integrity-check' decodes %_data (fts5_decode.go) and compares it with a
//     re-tokenization of %_content, as C's checksum does (see
//     fts5DecodeAllPostings and fts5integrityCheckContent).
//   - 'merge' asks for bounded merging, and this index is always merged: it
//     never errors (any value, NULL included), writes no %_config row, and
//     reports changes()==1.
//   - 'pgsz', 'secure-delete', 'automerge', 'usermerge', 'crisismerge',
//     'deletemerge', 'hashsize' and 'insttoken' are fts5_config.go's; 'rank'
//     stores the default rank function verbatim (fts5ParseRank,
//     fts5RankFunction).
//   - 'flush' writes pending terms, of which this engine has none; %_data is
//     unchanged and, unlike a %_config command, the cookie does not move.
//   - 'delete-all' is an error on a content-bearing table ("'delete-all' may
//     only be used with a contentless or external content fts5 table").
//
// rows is the VALUES rows already evaluated, in stmt.cols order -- as C reads
// the command from apVal[2+nCol] and its argument from apVal[2+nCol+1]
// (fts5_main.c:1941, 1976, 1989). nil (a SELECT source) is declined below.
func (db *DB) fts5CommandInsert(vm *vtabMeta, stmt *insertStmt, rows [][]Value) (bool, int, error) {
	slot := -1
	for i, c := range stmt.cols {
		if strings.EqualFold(c, vm.name) {
			slot = i
			break
		}
	}
	if slot < 0 {
		return false, 0, nil
	}
	if stmt.selectStmt != nil || len(rows) != 1 || len(rows[0]) != len(stmt.cols) {
		return true, 0, fmt.Errorf("engine: fts5: only the single-row \"INSERT INTO %s(%s, ...) VALUES(...)\" command form is supported by this engine", vm.name, vm.name)
	}
	// The command's argument travels in the hidden "rank" column: every one of
	// the 141 command statements in the mined fts5 corpus is spelled either
	// "(t)" or "(t, rank)", and any other column beside the table's own is
	// declined rather than silently ignored. The one exception is 'delete',
	// whose whole payload is a rowid and the row's own columns
	// ("INSERT INTO t(t, rowid, a, b) VALUES('delete', ...)", fts5SpecialDelete)
	// -- so that shape is checked once the command name is known.
	rank := -1
	extra := false
	for i, c := range stmt.cols {
		if i == slot {
			continue
		}
		if !strings.EqualFold(c, "rank") {
			extra = true
			continue
		}
		rank = i
	}
	v := rows[0][slot]
	if extra && !(v.Typ == Text && strings.EqualFold(string(v.S), "delete")) {
		for i, c := range stmt.cols {
			if i != slot && !strings.EqualFold(c, "rank") {
				return true, 0, fmt.Errorf("engine: fts5: an INSERT on %s's command channel may name only %s and rank, not %s", vm.name, vm.name, c)
			}
		}
	}
	if v.Typ == Null {
		// Real fts5 treats a NULL there as an ordinary INSERT of an all-NULL
		// row (verified: it adds a row whose %_docsize is all zeroes), not as a
		// command. Not reproduced -- declined rather than guessed at.
		return true, 0, fmt.Errorf("engine: fts5: \"INSERT INTO %s(%s) VALUES(NULL)\" (which C fts5 treats as inserting an empty row, not as a command) is not supported by this engine", vm.name, vm.name)
	}
	// The command NAME is matched case-insensitively (verified: 'OPTIMIZE' and
	// 'Secure-Delete' both work), but a configuration command stores its key
	// VERBATIM, so the original spelling is kept too.
	name := valueToText(v)
	cmd := strings.ToLower(name)
	st, _ := vm.store.(*fts5Store)
	if st != nil && (st.extContent != "" || st.contentless) {
		switch cmd {
		case "delete":
			if st.contentlessDelete {
				// fts5UpdateMethod's own message, verbatim: the 'delete' command
				// subtracts the postings the caller names, which for a
				// contentless_delete=1 table is the wrong mechanism -- a DELETE
				// statement (which leaves a tombstone) is the right one.
				return true, 0, fmt.Errorf("engine: fts5: 'delete' may not be used with a contentless_delete=1 table")
			}
			return db.fts5ExtDelete(vm, st, stmt, rows[0], slot)
		case "delete-all":
			// sqlite3Fts5StorageDeleteAll: %_data, %_idx and %_docsize are
			// emptied and %_data reinitialized. Nothing is read first, so no
			// reconstruction is needed (fts5ExtDiscardIndex) -- which makes this
			// work even over an index this engine could not read back, as in C.
			// It also discards a deferred 'secure-delete' bump for this table, like
			// 'rebuild' (fts5_txn.go).
			db.fts5TxnDiscardTable(vm.name)
			fts5ExtDiscardIndex(st)
			st.contentlessErr = nil
			st.clGhosted = false
			st.rows = map[int64][]Value{}
			st.tokens = nil
			// sqlite3Fts5IndexReinit starts the origin counter over at 1 for a
			// contentless_delete=1 table ("pTmp->nOriginCntr = 1"), which is
			// exactly what an emptied %_data implies: the counter lives nowhere
			// but the structure record. Verified against the oracle, whose next
			// insert after a 'delete-all' takes origin 1 again.
			st.origins = map[int64]int64{}
			st.originCntr = 1
			st.originOpen = false
			st.avgRow, st.avgSizes = 0, make([]int64, len(st.colNames))
			if serr := db.fts5SyncShadows(vm); serr != nil {
				return true, 0, serr
			}
			// sqlite3Fts5IndexReinit writes the averages record as a ZERO-LENGTH
			// blob ('fts5DataWrite(p, FTS5_AVERAGES_ROWID, (const u8*)"", 0)'),
			// not as the explicit zero varints fts5StorageSaveTotals would --
			// the same empty spelling createShadowTables seeds a brand-new table
			// with. 'rebuild' is not like this: it calls DeleteAll and then
			// fts5StorageSaveTotals, so it ends with the varints even over an
			// empty content table.
			if data := db.findTableMeta(vm.name + "_data"); data != nil {
				data.putRow(fts5AveragesRowid, []Value{{Typ: Null}, {Typ: Blob, S: []byte{}}})
			}
			return true, 1, nil
		case "rebuild":
			if st.contentless {
				// fts5SpecialInsert's own message, verbatim: a contentless table
				// has nothing to rebuild the index FROM.
				return true, 0, fmt.Errorf("engine: fts5: 'rebuild' may not be used with a contentless fts5 table")
			}
			// sqlite3Fts5StorageRebuild: delete everything, then index every
			// row the CONTENT table holds right now. Also discards a deferred
			// 'secure-delete' bump for this table -- fts5_txn.go.
			db.fts5TxnDiscardTable(vm.name)
			src, rerr := db.fts5ExtRowsIn(st)
			if rerr != nil {
				return true, 0, rerr
			}
			fts5ExtDiscardIndex(st)
			st.rows = make(map[int64][]Value, len(src.rowids))
			st.tokens = nil
			for _, rid := range src.rowids {
				st.rows[rid] = src.byID[rid]
			}
			if serr := db.fts5SyncShadows(vm); serr != nil {
				return true, 0, serr
			}
			return true, 1, nil
		}
	}
	switch cmd {
	case "rebuild":
		// A plain (non-contentless/external-content) table's 'rebuild' lands
		// here rather than in the switch above. Same discard, same reason --
		// fts5_txn.go.
		db.fts5TxnDiscardTable(vm.name)
		if serr := db.fts5SyncShadows(vm); serr != nil {
			return true, 0, serr
		}
		// Real fts5 reports changes()==1 for a command -- verified by running
		// one after a three-row INSERT (changes 3) and a two-row one
		// (changes 2), where it reads 1 either way.
		return true, 1, nil
	case "optimize", "merge", "flush":
		// Each flushes this table's own deferred 'secure-delete' bump before
		// doing its real work -- verified directly against the oracle for all
		// three (fts5_txn.go's fts5TxnFlushTable).
		db.fts5TxnFlushTable(vm.name)
		if serr := db.fts5SyncShadows(vm); serr != nil {
			return true, 0, serr
		}
		// Real fts5 reports changes()==1 for a command -- verified by running
		// one after a three-row INSERT (changes 3) and a two-row one
		// (changes 2), where it reads 1 either way.
		return true, 1, nil
	case "integrity-check":
		if serr := db.fts5IntegrityCheck(vm); serr != nil {
			return true, 0, serr
		}
		// Verified: changes()==1 here too, same as 'optimize'/'rebuild'.
		return true, 1, nil
	}
	if cmd == "rank" {
		if rank < 0 {
			return true, 0, fmt.Errorf("engine: fts5: the %q command needs a value (\"INSERT INTO %s(%s, rank) VALUES('%s', '<func>(<args>)')\"), which C fts5 requires too", cmd, vm.name, vm.name, name)
		}
		rv := rows[0][rank]
		// Same command-channel flush as 'optimize'/'merge'/'flush' -- fts5_txn.go.
		db.fts5TxnFlushTable(vm.name)
		if serr := db.fts5SetRank(vm, name, rv); serr != nil {
			return true, 0, serr
		}
		if serr := db.fts5SyncShadows(vm); serr != nil {
			return true, 0, serr
		}
		return true, 1, nil
	}
	if _, isConfig := fts5ConfigCommands[cmd]; isConfig {
		if rank < 0 {
			return true, 0, fmt.Errorf("engine: fts5: the %q command needs a value (\"INSERT INTO %s(%s, rank) VALUES('%s', <n>)\"), which C fts5 requires too", cmd, vm.name, vm.name, name)
		}
		rv := rows[0][rank]
		// Every configuration command flushes this table's own deferred
		// 'secure-delete' bump first, same as 'optimize'/'merge'/'flush' --
		// verified directly against the oracle (fts5_txn.go's
		// fts5TxnFlushTable).
		db.fts5TxnFlushTable(vm.name)
		if serr := db.fts5SetConfig(vm, name, rv); serr != nil {
			return true, 0, serr
		}
		// The new configuration reaches the index through fts5SyncShadows,
		// which is also what carries the cookie fts5SetConfig just bumped into
		// the structure record -- C fts5 rewrites it on the spot too.
		if serr := db.fts5SyncShadows(vm); serr != nil {
			return true, 0, serr
		}
		return true, 1, nil
	}
	if cmd == "delete-all" {
		// fts5SpecialInsert's own message, verbatim: on a content-bearing table
		// this is an error in C fts5 too.
		return true, 0, fmt.Errorf("engine: fts5: 'delete-all' may only be used with a contentless or external content fts5 table")
	}
	return true, 0, fmt.Errorf("engine: fts5: the %q command is not supported by this engine ('rank' would name a default rank function, and rank is not reachable here; every other spelling C fts5 rejects too)", cmd)
}

// fts5SyncIfNeeded re-encodes vm's shadow tables when vm is an fts5 table, and
// does nothing otherwise. Every writable-vtab mutation (vtab_write.go) ends
// with this call, so the file image and the live store can never disagree.
func (db *DB) fts5SyncIfNeeded(vm *vtabMeta) error {
	if _, isFts5 := vm.store.(*fts5Store); !isFts5 {
		return nil
	}
	return db.fts5SyncShadows(vm)
}

// fts5SyncShadows rewrites every shadow table of vm from its live store. It is
// called after each mutation, and is the ONLY place fts5 bytes reach the file.
func (db *DB) fts5SyncShadows(vm *vtabMeta) error {
	st, ok := vm.store.(*fts5Store)
	if !ok {
		return fmt.Errorf("engine: internal error: fts5 table %s has no backing store", vm.name)
	}
	// An EXTERNAL-CONTENT table's documents are not stored anywhere this write
	// could rewrite them from, so before the index is re-encoded the
	// reconstruction it will be re-encoded from has to be shown to BE the index
	// already in the file (fts5_extcontent.go).
	if verr := st.fts5ExtEnsureVerified(); verr != nil {
		return verr
	}
	// A CONTENTLESS table's documents live only in the index this write is about
	// to re-encode, so a table whose index could not be read back into documents
	// must not be written at all (fts5_contentless.go).
	if st.contentlessErr != nil {
		return st.contentlessErr
	}
	data := db.findTableMeta(vm.name + "_data")
	idx := db.findTableMeta(vm.name + "_idx")
	// %_content is absent on an external-content or contentless table by
	// construction, and its rows are never written -- fts5_storage.c's
	// INSERT_CONTENT / REPLACE_CONTENT statements are asserted unreachable for
	// either.
	var content *tableMeta
	if st.fts5HasContentShadow() {
		content = db.findTableMeta(vm.name + "_content")
	}
	// %_docsize exists only on a columnsize=1 table (the default); a
	// columnsize=0 one never had it (createShadowTables).
	var docsize *tableMeta
	if st.columnsize {
		docsize = db.findTableMeta(vm.name + "_docsize")
	}
	if data == nil || idx == nil || (st.fts5HasContentShadow() && content == nil) || (st.columnsize && docsize == nil) {
		return fmt.Errorf("engine: fts5 table %s is missing a shadow table (%%_data/%%_idx/%%_content/%%_docsize)", vm.name)
	}
	// Both of these come out of the shadow tables about to be rewritten, so
	// they are read first: the configuration cookie lives in the structure
	// record %_data is about to lose, and the page budget in %_config
	// (fts5_config.go).
	cookie := fts5ConfigCookie(data)
	pgsz := db.fts5ConfigPgsz(vm.name)
	for _, t := range []*tableMeta{data, idx, content, docsize} {
		if t != nil {
			fts3ClearTable(t)
		}
	}

	nCol := len(st.colNames)
	rowids := st.sortedRowids()
	docs := make([]fts5IndexDoc, 0, len(rowids))
	docsizes := make([][]byte, 0, len(rowids))
	for _, rid := range rowids {
		vals := st.rows[rid]
		rec := make([]Value, 0, nCol+1)
		rec = append(rec, Value{Typ: Null}) // id: carried by the b-tree key
		for i := 0; i < nCol; i++ {
			// FTS5_CONTENT_UNINDEXED's %_content has a column only for the
			// UNINDEXED ones (createShadowTables), so the record must skip the
			// rest rather than write NULLs into slots that do not exist.
			if st.contentUnindexed && !st.unindexed[i] {
				continue
			}
			var v Value
			if i < len(vals) {
				v = vals[i]
			}
			rec = append(rec, v)
		}
		// A locale=1 table's "l<i>" columns follow every "c<i>" one
		// (fts5_locale.go).
		rec = fts5LocaleAppendRecord(rec, st, rid)
		// The row's TOKENS come from the per-row cache when this statement did
		// not touch this row -- which, for the single-row INSERT/UPDATE/DELETE
		// the corpus is made of, is every row but one. Without it this rebuild
		// re-tokenized the whole table per statement, making N inserts O(N^2)
		// with tokenization the dominant term (fts5Store.tokens says what the
		// profile showed).
		cached, ok := st.tokens[rid]
		if !ok && st.contentless {
			// For a CONTENTLESS table the "cache" is the document itself
			// (fts5_contentless.go): st.rows holds NULLs, so re-tokenizing it
			// here would silently drop the row's postings. Every writer of
			// st.rows on that path sets st.tokens with it, so a miss is a bug
			// in this engine, never a row to re-derive.
			return fmt.Errorf("engine: internal error: fts5 contentless table %s has no document for rowid %d", vm.name, rid)
		}
		if !ok {
			cols := make([][]string, nCol)
			for i := 0; i < nCol; i++ {
				var v Value
				if i < len(vals) {
					v = vals[i]
				}
				// An UNINDEXED column is STORED in %_content like any other and
				// contributes NOTHING else: no postings, a 0 in its %_docsize
				// varint, and a 0 in its averages slot. Verified against the
				// oracle -- fts5(a, b UNINDEXED) over one row of 'hello world' /
				// 'foo bar' leaves %_docsize x'0200' and averages x'010200', and
				// a table whose every column is UNINDEXED has NO segment page at
				// all (structure record x'00000000000000', %_idx empty).
				if !st.unindexed[i] {
					cols[i] = st.tok.tokenize(valueToText(v))
				}
			}
			cached = fts5RowTokens{cols: cols}
			if st.tokens == nil {
				st.tokens = map[int64]fts5RowTokens{}
			}
			st.tokens[rid] = cached
		}
		cols := cached.cols
		sizes := make([]int, nCol)
		for i := 0; i < nCol; i++ {
			sizes[i] = len(cols[i])
		}
		if content != nil {
			content.putRow(uint64(rid), rec)
		}
		docsizes = append(docsizes, fts5EncodeDocsize(sizes))
		docs = append(docs, fts5IndexDoc{rowid: rid, cols: cols})
	}
	// The ORIGIN of each document is settled before %_docsize is written,
	// because on a contentless_delete=1 table it is a COLUMN of it
	// (fts5_contentless.go). This is the flush C fts5 would be doing here, so
	// it is where the origin counter moves.
	fts5ContentlessFlush(st, docs, db.inTransaction())
	if docsize != nil {
		for i, d := range docs {
			row := []Value{{Typ: Null}, {Typ: Blob, S: docsizes[i]}}
			if st.contentlessDelete {
				row = append(row, Value{Typ: Int, I: st.origins[d.rowid]})
			}
			docsize.putRow(uint64(d.rowid), row)
		}
	}

	// A contentless GHOST (fts5Store.clGhosted) still has postings -- the terms
	// a mismatched 'delete' did not name -- and no %_docsize row, so it joins
	// the documents the index is built from and nothing else. fts5BuildIndex
	// counts every document it is given into nRow and colTotals, so each
	// ghost's own contribution is taken back out afterwards: the averages
	// record then equals a recount of the live documents, which is C fts5's
	// value whenever fts5ContentlessGhost accepted the command (equal
	// per-column counts).
	indexDocs := docs
	var ghosts []fts5IndexDoc
	if st.contentless && st.clGhosted {
		for rid, tk := range st.tokens {
			if _, live := st.rows[rid]; !live {
				ghosts = append(ghosts, fts5IndexDoc{rowid: rid, cols: tk.cols, holes: tk.holes})
			}
		}
		if len(ghosts) > 0 {
			indexDocs = append(append([]fts5IndexDoc(nil), docs...), ghosts...)
			sort.Slice(indexDocs, func(i, j int) bool { return indexDocs[i].rowid < indexDocs[j].rowid })
		}
	}
	img, err := fts5BuildIndex(indexDocs, nCol, st.prefixes, pgsz, st.detail, fts5ContentlessV2(st, len(docs)))
	if err != nil {
		return err
	}
	for _, g := range ghosts {
		img.nRow--
		for c, toks := range g.cols {
			if c < len(img.colTotals) {
				img.colTotals[c] -= int64(len(toks))
			}
		}
	}
	fts5SetConfigCookie(img.data[fts5StructureRowid], cookie)
	// A contentless_delete=1 table's averages are CARRIED rather than derived:
	// its deleted documents go on counting (fts5_contentless.go's
	// fts5ContentlessFlush), so the live rows this rebuild just walked are not
	// what C fts5 would have recorded.
	avgRow, avgTotals := img.nRow, img.colTotals
	switch {
	case st.contentlessDelete:
		avgRow, avgTotals = st.avgRow, st.avgSizes
	case st.extContent != "":
		// An external-content table's averages record can have DRIFTED from
		// this fresh recount -- st.extRowDrift/extColDrift, which
		// fts5ExtDeleteOrdinary is the only writer of (fts5_extcontent.go).
		// Zero for every table that has never had a mismatched 'delete'
		// command, in which case this is the same img.nRow/colTotals every
		// other table gets.
		avgRow += st.extRowDrift
		avgTotals = append([]int64(nil), img.colTotals...)
		for i := range avgTotals {
			if i < len(st.extColDrift) {
				avgTotals[i] += st.extColDrift[i]
			}
		}
	}
	data.putRow(fts5AveragesRowid, []Value{{Typ: Null}, {Typ: Blob, S: fts5EncodeAverages(avgRow, avgTotals)}})
	for id, blk := range img.data {
		data.putRow(uint64(id), []Value{{Typ: Null}, {Typ: Blob, S: blk}})
	}
	// %_idx is WITHOUT ROWID: its map key is an opaque internal identifier and
	// the (segid, term) primary key is what orders its b-tree, so a plain
	// counter is enough here.
	for i, r := range img.idx {
		idx.putRow(uint64(i+1), []Value{{Typ: Int, I: r.segid}, {Typ: Blob, S: r.term}, {Typ: Int, I: r.pgno}})
	}
	return nil
}

// fts5LoadStore refills a reopened fts5 table's store from its %_content
// shadow table -- the counterpart of fts5SyncShadows, and the reason a table
// C SQLite wrote can be read (and then written) here: %_content holds the
// row values in the clear, so no index byte has to be decoded to recover them.
func fts5LoadStore(rp *ReadOnlyPager, st *fts5Store, name string) error {
	// An EXTERNAL-CONTENT table has no %_content: its documents are
	// reconstructed from the index's own rowids and the table it names
	// (fts5_extcontent.go).
	if st.extContent != "" {
		fts5LoadExternal(rp, st, name)
		return nil
	}
	// A CONTENTLESS table has no %_content either, and no external table: its
	// documents come back out of its own index (fts5_contentless.go).
	if st.contentless {
		fts5LoadContentless(rp, st, name)
		return nil
	}
	rowids, records, err := rp.Rows(name + "_content")
	if err != nil {
		return err
	}
	for i, rid := range rowids {
		st.vtabLoadRow(int64(rid), records[i])
	}
	return nil
}

// fts5SegmentShadowQueryGuard declines a SELECT reading an fts5 table's %_data
// or %_idx, the two shadow tables not reproducible against C: they hold the
// segment layout, which differs here (one rebuilt segment vs C's
// per-statement segments and merges). Everything reading through fts5 agrees
// -- MATCH, %_content, %_docsize, integrity-check -- but "SELECT count(*) FROM
// t_data" reads the layout itself.
func (p *ReadOnlyPager) fts5SegmentShadowQueryGuard(stmt *SelectStmt) error {
	// ...and, from the same hook, the OTHER read this engine must refuse for a
	// reason the statement rather than the row values decides: a query with no
	// MATCH over a contentless columnsize=0 table (fts5_contentless.go). It rides
	// here because execSelect (query.go) calls this once per statement, before
	// any row-mode / GROUP BY / aggregate / window dispatch and before the
	// table-valued rewrite -- the only point in the read path where the whole
	// SelectStmt is in hand. A second call site of its own next to this one in
	// query.go would be tidier; this file cannot add it.
	if err := p.fts5ContentlessScanGuard(stmt); err != nil {
		return err
	}
	// Cheap syntactic pre-filter: no FROM item could name one of these
	// without the suffix, so the schema is only read for a statement that
	// might actually be affected.
	if !selectTreeMentionsTable(stmt, func(it FromItem) bool { return fts5LooksLikeSegmentShadowName(it.Table) }) {
		return nil
	}
	names, err := p.fts5SegmentShadowNames()
	if err != nil || len(names) == 0 {
		return err
	}
	var hit string
	if selectTreeMentionsTable(stmt, func(it FromItem) bool {
		n := it.Table
		if names[strings.ToLower(n)] {
			hit = n
			return true
		}
		return false
	}) {
		return fmt.Errorf("engine: unsupported: %s holds an fts5 table's SEGMENT LAYOUT, which is not reproducible against C SQLite (this engine rebuilds the whole index into one segment per write; C fts5 appends a segment per statement and merges them on its own schedule, so the two agree on the postings and not on how many rows it takes to store them)", hit)
	}
	return nil
}

// fts5LooksLikeSegmentShadowName reports whether name could be an fts5
// %_data/%_idx table by SPELLING alone.
func fts5LooksLikeSegmentShadowName(name string) bool {
	low := strings.ToLower(name)
	return strings.HasSuffix(low, "_data") || strings.HasSuffix(low, "_idx")
}

// fts5SegmentShadowNames returns the lower-cased %_data/%_idx names of every
// fts5 virtual table in the live schema.
func (p *ReadOnlyPager) fts5SegmentShadowNames() (map[string]bool, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	var out map[string]bool
	for _, r := range rows {
		if r.Type != "table" || !isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		name, mod, _, _, perr := parseCreateVirtualTableStmt(r.SQL)
		if perr != nil || !strings.EqualFold(mod, "fts5") {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[strings.ToLower(name)+"_data"] = true
		out[strings.ToLower(name)+"_idx"] = true
	}
	return out, nil
}

// materializeFts5 presents an fts5 table's rows from its %_content shadow
// table, which is what C reads for a query with no MATCH. Each row is [rowid,
// col0, ...], as materializeFts3 builds.
//
// it is the FROM item this source stands in for: an external-content table's
// row source is its content table for a plain scan but its index for a MATCH,
// and only the statement says which (fts5ExtIndexOnlyMatch). A caller with no
// statement passes FromItem{Table: name}, whose nil tvfWhere means exactly
// that.
func (p *ReadOnlyPager) materializeFts5(it FromItem, cols []columnInfo) ([]columnInfo, [][]Value, []int64, error) {
	name := it.Table
	// An EXTERNAL-CONTENT table has no %_content; C fts5's own plain scan
	// reads the table it names instead (fts5_extcontent.go).
	if sch, ok := p.fts5SchemaTok(name); ok {
		if sch.extContent != "" {
			st, err := p.fts5ExtStoreOf(name)
			if err != nil || st == nil {
				return nil, nil, nil, err
			}
			return p.materializeFts5External(it, name, cols, st)
		}
		// A CONTENTLESS table has no %_content at all; C fts5's own scan
		// reads %_docsize's rowids and leaves every column NULL
		// (fts5_contentless.go).
		if sch.cl != nil {
			return p.materializeFts5Contentless(it, name, cols)
		}
	}
	return p.materializeFts3(name, cols)
}
