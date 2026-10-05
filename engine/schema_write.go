// This file is the write-side counterpart of schema.go: CREATE TABLE
// support, which registers a new table's metadata and (empty) row store. The
// catalog change reaches the file when the Session commits it, as a rewrite
// (segment_ddl.go).
package engine

import (
	"errors"
	"fmt"
	"strings"
)

// tableMeta is what the write side knows about a table -- created this session
// (CreateTable) or loaded from the catalog (schema_load_objects.go): its column
// metadata (sql_parser.go's columnInfo), its verbatim CREATE TABLE text, and its
// live row store.
//
// rows is the single source of truth for the table's current content: keyed by
// rowid, each entry holds that row's column values in the stored shape (the
// INTEGER PRIMARY KEY rowid-alias column, if any, stored as NULL -- see the IPK
// handling in Insert/Update). INSERT adds entries; UPDATE removes-then-
// reinserts (under a possibly new rowid); DELETE removes entries.
type tableMeta struct {
	// aliasOf names the table whose row store this one shares, after a direct
	// sqlite_schema write gave it that table's rootpage (segmentRootpageEditPlan's
	// rootEditAlias). Empty for every ordinary table.
	aliasOf string

	// schemaSeq is this object's CREATION rank within its database: the order
	// its sqlite_schema row must appear in, which C SQLite gives by simply
	// appending each new object's row to the catalog b-tree. The catalog this
	// engine writes is ordered by it, so the schema reports objects in the order
	// they were created rather than grouped by kind. Assigned by
	// DB.nextSchemaSeq at CREATE time, recovered from the row's position at load,
	// and carried unchanged through ALTER TABLE (which C SQLite likewise applies
	// in place, leaving the row's rowid alone).
	schemaSeq uint64

	// isTemp puts this table in the TEMP catalog rather than main -- see
	// temp_schema.go. Two tables may share a name only across catalogs.
	isTemp   bool
	name     string
	sql      string // verbatim CREATE TABLE text, as the catalog stores it
	cols     []columnInfo
	ipkIndex int                // index into cols of the INTEGER PRIMARY KEY rowid-alias column, or -1
	rows     *rowStore // live row store (row_store.go); nil until loaded

	// loaded records whether rows has been set up yet. CreateTable sets it true
	// immediately (a freshly created table's empty store IS its content); the
	// catalog load leaves it false, and ensureTableLoaded (table_load.go) gives
	// the table its store on first use.
	loaded bool

	// checks is every CHECK constraint (column- or table-level) this
	// table's own CREATE TABLE text declared, already parsed into an
	// expression AST and validated as a shape this write path can enforce
	// byte-exact against C SQLite (see finalizeCheckConstraints below) --
	// populated by CreateTable and OpenWrite's catalog load (schema_load_objects.go) alike, kept in
	// sync by alter_write.go whenever it edits tbl.sql (RENAME COLUMN/DROP
	// COLUMN), and evaluated per candidate row (checkTableChecks, or the
	// compiled CHECK block). nil for a table with no CHECK constraint.
	checks []checkConstraint

	// withoutRowid is true for "CREATE TABLE ... WITHOUT ROWID": ipkIndex is
	// always -1 (forceNoRowidAlias), rows store every column's literal value,
	// and rows' map key is an internal identifier assigned like a hidden rowid
	// (nextRowidForTable) but never readable as rowid (tableScope.noRowid). The
	// PRIMARY KEY tuple keys and orders the stored b-tree (pkIndex).
	withoutRowid bool

	// pkIndex is non-nil exactly when withoutRowid: a unique indexMeta over the
	// PRIMARY KEY columns in clause order, built by finalizeWithoutRowidPK and
	// buildAutoIndexes and registered in db.indexes so every UNIQUE and
	// conflict mechanism treats it generically (checkUniqueIndexesForRow,
	// findRowConflicts, validateUpsertTarget, hitTargetKey,
	// uniqueConflictError). isTablePK gives it no catalog row: C creates no
	// sqlite_autoindex entry for a WITHOUT ROWID table's own key.
	pkIndex *indexMeta

	// strict is true for "CREATE TABLE ... STRICT": declared types must be
	// INT/INTEGER/REAL/TEXT/BLOB/ANY (strictColTypeOf, in
	// parseCreateTableColumnsAndAutoIndexes, which also gives ANY no affinity
	// and every PRIMARY KEY column NOT NULL), and every value stored in a
	// non-ANY column must be representable after affinity
	// (checkStrictColumnTypes, run by OpTypeCheck). Recovered on open from the
	// stored CREATE text (parseTableTailClauses).
	strict bool

	// loadErr is why this table cannot be used at all: its catalog row names
	// no b-tree (rootpage 0), which only a direct catalog write leaves behind.
	// C loads such a row and fails every cursor open over it with
	// SQLITE_CORRUPT, so the table is listed and unusable rather than absent
	// -- see resolveTableIn's identical read-path refusal.
	loadErr error

	// autoIncrement is true for "INTEGER PRIMARY KEY AUTOINCREMENT" (see
	// detectAutoIncrementColumn): TF_Autoincrement. An auto-assigned rowid is
	// one past the larger of the table's current max and its sqlite_sequence
	// row, which each statement reads at its start and writes back at its end
	// (sqlite3AutoincrementBegin/End, insert.c:460-575) -- see aincState,
	// insert_write.go.
	autoIncrement bool

	// rootPage is this table's key in the session: the synthetic root it was
	// handed when created (nextSegmentRoot), which the segment file records so
	// every later session keys it the same (SegmentFile.RootOf). It is what the
	// schema this session reports calls the table's rootpage.
	rootPage uint32

	// rowsMutatedThisSession says tbl.rows' CONTENT has been WRITTEN this
	// session -- not merely read in, which is what loaded says. A catalog
	// rewrite reloads every table it can from the file (segment_ddl.go),
	// and this is what stops it reloading one whose rows only this
	// session holds. Set by putRow/dropRow, the only two functions that write
	// tbl.rows, and BY HAND by every wholesale replace (VACUUM's renumbering,
	// fts3's %_segdir/%_segments clone): one of those that forgot it once left
	// every document written under automerge unreachable by MATCH. Never reset
	// within a session; carried across ROLLBACK by cloneTableMeta (txn.go).
	rowsMutatedThisSession bool

	// inFile says this table IS the segment file's table of its name: it was
	// loaded from the file, or a rewrite has written it there since. A table
	// created this session is not -- not even when the file holds one of its
	// name, dropped earlier in the session -- so a rewrite must not reload it
	// from there (segment_ddl.go).
	inFile bool

	// rowsWrittenSinceCommit is rowsMutatedThisSession's per-commit twin, set
	// by the same functions and cleared by every commit that writes. A delta
	// batch is built from RowChange records, and a virtual table's shadow
	// writes produce none (DB.mutatedOutsideChangeLog), so such a commit needs
	// a full rewrite. The session-long flag would force a rewrite on every
	// commit after the first write; this one only on the commits that need it.
	rowsWrittenSinceCommit bool
}

// putRow stores vals under rowid in the live row store. Every row write from
// outside row_store.go goes through putRow, dropRow or replaceRows, which buys
// rowsMutatedThisSession (see its doc comment): a precise, comprehensive "was
// this table's on-disk content actually changed this session" signal with no
// separate audit of the call sites.
func (t *tableMeta) putRow(rowid uint64, vals []Value) {
	t.rows.put(rowid, storedValues(vals))
	t.rowsMutatedThisSession, t.rowsWrittenSinceCommit = true, true
}

// storedValues is vals with every subtype dropped: storage has no place for
// one (OP_MakeRecord writes no subtype), but the row store keeps the Value
// struct, so the subtype survived and was visible:
//
//	CREATE TABLE t3(b); INSERT INTO t3 VALUES(json(TRUE));
//	SELECT count(*) FROM t3, t1 WHERE NOT json_quote(b);
//
// answered 0 where C answers 1 (subtype1.test#1), and subtype(b) was 74.
//
// It copies only when some value carries a subtype. Not in place: the slice is
// the statement's own row, which RETURNING and NEW read afterwards, where C's
// register keeps its subtype.
func storedValues(vals []Value) []Value {
	carries := false
	for i := range vals {
		if vals[i].Subtype != 0 {
			carries = true
			break
		}
	}
	if !carries {
		return vals
	}
	out := make([]Value, len(vals))
	copy(out, vals)
	for i := range out {
		out[i].Subtype = 0
	}
	return out
}

// dropRow removes rowid from the live row store.
func (t *tableMeta) dropRow(rowid uint64) {
	t.rows.drop(rowid)
	t.rowsMutatedThisSession, t.rowsWrittenSinceCommit = true, true
}

// replaceRows swaps t.rows for an entirely new store, and sets
// rowsMutatedThisSession, which is what makes the new rows SURVIVE a catalog
// rewrite: without it the rewrite reloads the table from the file
// (segment_ddl.go).
//
// That is not hypothetical: fts3_automerge.go's own wholesale replace once
// omitted it, and every document written to an fts3/fts4 table while
// automerge was enabled was silently unreachable by MATCH afterwards -- while a
// plain scan still returned it and integrity_check still said "ok".
func (t *tableMeta) replaceRows(rows *rowStore) {
	t.rows = rows
	t.rowsMutatedThisSession, t.rowsWrittenSinceCommit = true, true
}

// findTableMeta returns the tableMeta for name (case-insensitive), or nil.
func (db *DB) findTableMeta(name string) *tableMeta {
	return db.findTableMetaIn(scopeAny, name)
}

// CreateTable parses a single "CREATE TABLE name (col ...)" and registers the
// table's metadata and an empty row store; its catalog row (sql=createSQL
// verbatim, as C writes it) reaches the file at commit. Anything outside the
// grammar parseCreateTableColumnsAndAutoIndexes understands is rejected.
//
// STRICT (tableMeta.strict): the CREATE-time half lives in
// parseCreateTableColumnsAndAutoIndexes, so the read path derives identical
// columns from the stored text; the runtime half is checkStrictColumnTypes.
//
// WITHOUT ROWID: one PRIMARY KEY (one or more columns, any type, ASC only; see
// finalizeWithoutRowidPK) and no other table-level key. Such a table is
// ordered by its PRIMARY KEY tuple.
//
// Every UNIQUE/PRIMARY KEY constraint, inline or table-level, except the
// INTEGER PRIMARY KEY that becomes a rowid table's rowid alias, registers an
// automatic index (sqlite_autoindex_<name>_<N>, buildAutoIndexes) enforced like
// an explicit index; a WITHOUT ROWID table's own PRIMARY KEY is the table
// (isTablePK).
func (db *DB) CreateTable(createSQL string) error {
	// CREATE TABLE ... AS SELECT (CTAS): a completely different table-shape
	// (columns derived from the SELECT's own result, see createTableAsSelect)
	// and population (the SELECT's rows, not a column-list-only DDL
	// statement with no rows at all) -- checked first, before any of the
	// ordinary column-list parsing below, which doesn't understand "AS" at
	// all and would otherwise misparse or reject it.
	ctasName, ctasTemp, ctasSel, isCTAS, err := tryParseCreateTableAsSelect(createSQL)
	if err != nil {
		return err
	}
	if isCTAS {
		return db.createTableAsSelect(ctasName, ctasTemp, ctasSel)
	}

	name, isTemp, ifNotExists, err := parseCreateTableName(createSQL)
	if err != nil {
		return err
	}
	if err := db.checkReservedObjectName(name); err != nil {
		return err
	}
	// Every collision below is scoped to the catalog this table is being
	// created in: "CREATE TEMP TABLE tbl" and "CREATE TABLE main.tbl" both
	// succeed and coexist (verified directly; tkt2817.test) -- see
	// temp_schema.go.
	scope := createScope(isTemp)
	if db.findTableMetaIn(scope, name) != nil {
		if ifNotExists {
			return nil // "CREATE TABLE IF NOT EXISTS" over an existing table is a no-op
		}
		return fmt.Errorf("engine: table %s already exists", name)
	}
	if db.findViewMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: view %s already exists", name)
	}
	// An INDEX of the same name collides too -- tables, views and indexes
	// share ONE namespace in SQLite (only triggers get their own). Verified
	// directly: this is a hard error even under IF NOT EXISTS, which only
	// ever suppresses a same-kind collision.
	if db.findIndexMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: there is already an index named %s", name)
	}
	withoutRowid, strict, err := parseTableTailClauses(createSQL)
	if err != nil {
		return err
	}
	cols, checks, specs, err := parseCreateTableColumnsAndAutoIndexes(createSQL, withoutRowid)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return emptyColumnListErr(createSQL)
	}
	checks, err = finalizeCheckConstraints(name, cols, checks, withoutRowid)
	if err != nil {
		return err
	}
	// See generated_column_validate.go: a generated column's expression is
	// VALIDATED here, the same place a CHECK's is, and for the same reason --
	// accepting one C SQLite refuses leaves a file whose schema C cannot
	// load.
	if err := validateGeneratedColumns(name, cols, specs, true); err != nil {
		return err
	}
	ipk := -1
	for i, c := range cols {
		if c.IsRowidAlias {
			ipk = i
			break
		}
	}
	autoIdx, autoIdxBySpec, err := buildAutoIndexes(name, cols, specs)
	if err != nil {
		return err
	}
	// An automatic index belongs to its table's catalog (temp_schema.go).
	for _, ix := range autoIdx {
		ix.isTemp = isTemp
	}
	pkIndex, err := finalizeWithoutRowidPK(name, withoutRowid, cols, specs, autoIdxBySpec)
	if err != nil {
		return err
	}
	autoIncrement, err := finalizeAutoIncrement(name, createSQL, withoutRowid, cols, ipk)
	if err != nil {
		return err
	}
	db.tables = append(db.tables, &tableMeta{
		schemaSeq:     db.nextSchemaSeq(isTemp),
		isTemp:        isTemp,
		name:          name,
		sql:           storedSchemaSQL("TABLE", createSQL, isTemp),
		cols:          cols,
		ipkIndex:      ipk,
		rows:          newRowStore(map[uint64][]Value{}, nil),
		loaded:        true, // freshly created this session: nothing on disk to defer (see tableMeta.loaded)
		checks:        checks,
		withoutRowid:  withoutRowid,
		strict:        strict,
		pkIndex:       pkIndex,
		autoIncrement: autoIncrement,
	})
	// The table is brand new and still empty, so every automatic index is
	// trivially unique -- no validateUniqueIndex check is needed here the
	// way CreateIndex needs one against a table's pre-existing rows. Each takes
	// its creation rank right after the table's, which is where C SQLite
	// puts their rows ("CREATE TABLE t1(a UNIQUE, b UNIQUE, c)" reports t1,
	// sqlite_autoindex_t1_1, sqlite_autoindex_t1_2 -- verified).
	for _, ix := range autoIdx {
		if ix.isTablePK && ix.sql == "" {
			// A WITHOUT ROWID table's PRIMARY KEY writes no sqlite_schema row
			// (segmentSchemaRows skips it), so it takes no rowid either.
			ix.schemaSeq = db.tables[len(db.tables)-1].schemaSeq
			continue
		}
		ix.schemaSeq = db.nextSchemaSeq(isTemp)
	}
	db.indexes = append(db.indexes, autoIdx...)
	db.bumpSchema(isTemp)
	created := db.tables[len(db.tables)-1]
	if err := db.newTableStore(created); err != nil {
		return err
	}
	for _, ix := range autoIdx {
		if err := db.checkNewIndex(ix, created); err != nil {
			return err
		}
	}
	if autoIncrement {
		return db.createSequenceTable(isTemp)
	}
	return nil
}

// createSequenceTable is sqlite3EndTable's "CREATE TABLE %Q.sqlite_sequence(
// name,seq)" (build.c:2919-2930): the database's first AUTOINCREMENT table
// creates it, after the table's own automatic indexes, unless it already has
// one. It is an ordinary table from then on -- the user may read, update and
// delete its rows, and each INSERT statement reads and writes them
// (aincState, insert_write.go).
func (db *DB) createSequenceTable(isTemp bool) error {
	if db.sequenceTable(isTemp) != nil {
		return nil
	}
	// The nested CREATE's own duplicate check: a table whose name differs
	// only in case is not pSeqTab (build.c:2968 compares with strcmp) but
	// still collides.
	if db.findTableMetaIn(createScope(isTemp), sqliteSequenceTableName) != nil {
		return fmt.Errorf("engine: table %s already exists", sqliteSequenceTableName)
	}
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(sqliteSequenceCreateSQL, false)
	if err != nil {
		return err
	}
	seq := &tableMeta{
		schemaSeq: db.nextSchemaSeq(isTemp),
		isTemp:    isTemp,
		name:      sqliteSequenceTableName,
		sql:       storedSchemaSQL("TABLE", sqliteSequenceCreateSQL, isTemp),
		cols:      cols,
		ipkIndex:  -1,
		rows:      newRowStore(map[uint64][]Value{}, nil),
		loaded:    true,
	}
	db.tables = append(db.tables, seq)
	return db.newTableStore(seq)
}

// sequenceTable is the database's pSeqTab: the table named exactly
// "sqlite_sequence" (build.c:2968 compares with strcmp), or nil.
func (db *DB) sequenceTable(isTemp bool) *tableMeta {
	if t := db.findTableMetaIn(createScope(isTemp), sqliteSequenceTableName); t != nil && t.name == sqliteSequenceTableName {
		return t
	}
	return nil
}

// nextSchemaSeq hands out the next creation rank (DB.schemaSeq, each meta's
// schemaSeq), taken when the object is constructed rather than appended,
// because CREATE TABLE ... AS SELECT appends after its SELECT runs and CREATE
// VIRTUAL TABLE after its shadow tables (C writes the vtab's row first: fts4
// lists ft, ft_content, ft_segments, ...).
//
// It is also the object's sqlite_schema rowid, numbered as C's OP_NewRowid on
// the schema b-tree (max(rowid)+1 for that database's catalog): dropping the
// newest object returns its number, dropping an older one leaves a gap
// (alter.test#3). TEMP numbers separately. db.schemaSeqReserved holds a number
// taken but not yet registered (a vtab's, before its shadow tables) so it is
// not handed out twice; it is cleared per top-level statement, so a failed
// CREATE leaves no gap.
func (db *DB) nextSchemaSeq(isTemp bool) uint64 {
	slot := 0
	if isTemp {
		slot = 1
	}
	n := db.schemaSeqReserved[slot]
	see := func(temp bool, seq uint64) {
		if temp == isTemp && seq > n {
			n = seq
		}
	}
	for _, t := range db.tables {
		see(t.isTemp, t.schemaSeq)
	}
	for _, ix := range db.indexes {
		see(ix.isTemp, ix.schemaSeq)
	}
	for _, v := range db.views {
		see(v.isTemp, v.schemaSeq)
	}
	for _, g := range db.triggers {
		see(g.isTemp, g.schemaSeq)
	}
	for _, vt := range db.vtabs {
		see(vt.isTemp, vt.schemaSeq)
	}
	n++
	db.schemaSeqReserved[slot] = n
	return n
}


func (db *DB) bumpSchema(isTemp bool) {
	db.schemaGen++
	if isTemp {
		// The TEMP database has its own header, so its own cookie moves --
		// C SQLite bumps the schema cookie of the database the DDL touched
		// (sqlite3ChangeCookie takes the iDb, build.c:2036). A session that has
		// not opened the temp database yet opens it here: the DDL that bumped
		// this is about to put something in it.
		if t, terr := db.tempDatabase(); terr == nil {
			t.cookie++
		} else {
			db.deferErr(terr)
		}
		return
	}
	db.schemaCookie++
}

// finalizeWithoutRowidPK is a no-op (nil, nil) when !withoutRowid. Otherwise it
// checks that specs (parsed with forceNoRowidAlias, so the PRIMARY KEY never
// collapsed into a rowid alias) has exactly one PRIMARY KEY (none is C's
// "PRIMARY KEY missing on table %s"), and returns the autoIdx entry
// buildAutoIndexes built for it as tbl.pkIndex. autoIdx must be
// buildAutoIndexes' bySpec result, aligned with specs even when a redundant
// constraint folded into an earlier index (which is then the PK). The index is
// marked isTablePK (no catalog row) and its columns forced NotNull (table_info
// reports notnull=1 for a WITHOUT ROWID PK column, unlike a rowid table's).
// Shared by CreateTable and OpenWrite's catalog load.
func finalizeWithoutRowidPK(name string, withoutRowid bool, cols []columnInfo, specs []autoIndexSpec, autoIdxBySpec []*indexMeta) (*indexMeta, error) {
	if !withoutRowid {
		return nil, nil
	}
	for i, s := range specs {
		if s.kind != "pk" {
			continue
		}
		// A DESC-ordered PK column used to be declined outright here. It no
		// longer needs to be: buildAutoIndexes (index_write.go) populates this
		// spec's indexMeta.colDesc unconditionally, and the export's b-tree
		// honors it exactly like any other DESC index (compareIndexRec). This
		// mirrors build.c:2354's
		// convertToWithoutRowidTable, which builds a WITHOUT ROWID table's
		// clustered key as an entirely ordinary sqlite3CreateIndex-built
		// Index (DESC and all) and only afterward repoints its
		// OP_CreateBtree to BTREE_BLOBKEY -- there is no DESC-specific
		// machinery of its own on the C side either, so none is needed here.
		pkIndex := autoIdxBySpec[i]
		pkIndex.isTablePK = true
		for _, ci := range pkIndex.colIdx {
			cols[ci].NotNull = true
		}
		return pkIndex, nil
	}
	// build.c:2728 -- the message is exactly this, with no explanation of WHY a
	// WITHOUT ROWID table needs one.
	return nil, fmt.Errorf("engine: CREATE TABLE %s: PRIMARY KEY missing on table %s", name, name)
}

// finalizeAutoIncrement checks that createSQL's AUTOINCREMENT column (if any;
// detectAutoIncrementColumn) is cols[ipk], the table's INTEGER PRIMARY KEY
// rowid alias. detectAutoIncrementColumn only checks the shape, and a WITHOUT
// ROWID table never has a rowid alias (forceNoRowidAlias), so AUTOINCREMENT
// there is rejected here, as C allows it only on a rowid table's INTEGER
// PRIMARY KEY. Shared by CreateTable and OpenWrite's catalog load.
func finalizeAutoIncrement(name, createSQL string, withoutRowid bool, cols []columnInfo, ipk int) (bool, error) {
	autoIncCol, hasAutoInc, err := detectAutoIncrementColumn(createSQL)
	if err != nil {
		return false, fmt.Errorf("engine: CREATE TABLE %s: %w", name, err)
	}
	if !hasAutoInc {
		return false, nil
	}
	if withoutRowid {
		// build.c:2723-2724's own words, and its own rejection -- so no
		// statement context in front of it either: "AUTOINCREMENT not allowed
		// on WITHOUT ROWID tables".
		return false, errors.New("engine: AUTOINCREMENT not allowed on WITHOUT ROWID tables")
	}
	if ipk < 0 || !equalFoldName(cols[ipk].Name, autoIncCol) {
		return false, fmt.Errorf("engine: CREATE TABLE %s: AUTOINCREMENT column %s must be the table's own single-column INTEGER PRIMARY KEY (rowid alias)", name, autoIncCol)
	}
	return true, nil
}

// detectAutoIncrementColumn scans createSQL's top-level column/constraint list
// (paren-depth-tracked segments, as parseCreateTableColumnsAndAutoIndexes
// splits it; that parser skips the AUTOINCREMENT token harmlessly) for the one
// supported shape: an inline "PRIMARY KEY [ASC] AUTOINCREMENT" on a column whose
// declared type is exactly INTEGER, the spelling the corpus uses (aggnested.test,
// join8.test, tkt-d82e3f3721.test).
//
// Anything else (table-level "PRIMARY KEY(col AUTOINCREMENT)", which C also
// accepts; a non-INTEGER or DESC column; more than one AUTOINCREMENT) returns
// ok=false with an unsupported error. finalizeAutoIncrement then checks the
// column is the rowid alias.
func detectAutoIncrementColumn(createSQL string) (colName string, ok bool, err error) {
	toks, lerr := lex(createSQL)
	if lerr != nil {
		return "", false, lerr
	}

	// Find the outer "(" ... ")" column-list span, exactly like
	// parseCreateTableColumnsAndAutoIndexes' own identical walk.
	depth := 0
	start := -1
	end := -1
	for i, t := range toks {
		switch {
		case t.kind == tkPunct && t.text == "(":
			if depth == 0 {
				start = i + 1
			}
			depth++
		case t.kind == tkPunct && t.text == ")":
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if start < 0 || end < 0 {
		return "", false, nil
	}
	inner := toks[start:end]

	// Split into top-level (paren-depth-0 relative to inner) comma-separated
	// segments -- the same split parseCreateTableColumnsAndAutoIndexes
	// performs over the identical span.
	var segments [][]token
	var cur []token
	d := 0
	for _, t := range inner {
		switch {
		case t.kind == tkPunct && t.text == "(":
			d++
			cur = append(cur, t)
		case t.kind == tkPunct && t.text == ")":
			d--
			cur = append(cur, t)
		case t.kind == tkPunct && t.text == "," && d == 0:
			segments = append(segments, cur)
			cur = nil
		default:
			cur = append(cur, t)
		}
	}
	if len(cur) > 0 {
		segments = append(segments, cur)
	}

	autoIncCount := 0
	for _, seg := range segments {
		for _, t := range seg {
			if t.kind == tkIdent && t.upper() == "AUTOINCREMENT" {
				autoIncCount++
			}
		}
	}
	if autoIncCount == 0 {
		return "", false, nil
	}
	if autoIncCount > 1 {
		return "", false, fmt.Errorf("at most one AUTOINCREMENT column is supported by this write path")
	}

	tableConstraintKeywords := map[string]bool{
		"PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true, "CONSTRAINT": true,
	}
	for _, seg := range segments {
		if len(seg) == 0 {
			continue
		}
		hasAI := false
		for _, t := range seg {
			if t.kind == tkIdent && t.upper() == "AUTOINCREMENT" {
				hasAI = true
				break
			}
		}
		if !hasAI {
			continue
		}
		first := seg[0]
		// A table-level constraint segment: the ONE constraint spelling this
		// write path implements is "[CONSTRAINT nm] PRIMARY KEY ( nm [ASC]
		// AUTOINCREMENT )" -- SQLite's alternate spelling of the same inline
		// shape (autoinc.test's t7; probed: it aliases the rowid
		// and seeds sqlite_sequence exactly like the inline form). Whether
		// the named column really is an exactly-INTEGER rowid alias is
		// finalizeAutoIncrement's ipk check -- the parser's collapse only
		// ever aliases such a column (parseCreateTableColumnsAndAutoIndexes'
		// identical PRIMARY-case rule). A DESC or COLLATE inside
		// the list stays unsupported here (their rowid-alias semantics were
		// not pinned), as does any other constraint kind.
		if first.kind == tkIdent && tableConstraintKeywords[first.upper()] {
			k := 0
			if first.upper() == "CONSTRAINT" {
				k = 2 // skip "CONSTRAINT <name>"
			}
			if k+1 < len(seg) && seg[k].kind == tkIdent && seg[k].upper() == "PRIMARY" &&
				seg[k+1].kind == tkIdent && seg[k+1].upper() == "KEY" {
				j := k + 2
				if j < len(seg) && seg[j].kind == tkPunct && seg[j].text == "(" {
					j++
					nm := ""
					if j < len(seg) && seg[j].kind == tkIdent {
						nm = seg[j].text
						j++
					} else if j < len(seg) && seg[j].kind == tkString {
						nm = seg[j].str
						j++
					}
					if nm != "" {
						if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "ASC" {
							j++
						}
						if j+1 < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "AUTOINCREMENT" &&
							seg[j+1].kind == tkPunct && seg[j+1].text == ")" {
							return nm, true, nil
						}
					}
				}
			}
			return "", false, fmt.Errorf("AUTOINCREMENT is only supported on an inline single-column INTEGER PRIMARY KEY, or the equivalent PRIMARY KEY(col [ASC] AUTOINCREMENT) table constraint, by this write path")
		}
		// This segment is a column definition: "name TYPE... PRIMARY KEY
		// [ASC] AUTOINCREMENT ...". Walk it looking for that exact shape.
		name := first.text
		// The declared type ends at the first column-constraint keyword, not at
		// PRIMARY: "id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT" is type
		// INTEGER. C requires the type to be exactly "INTEGER" for a rowid
		// alias:
		//
		//	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT           ok
		//	id INTEGER DEFAULT 5 PRIMARY KEY AUTOINCREMENT          ok
		//	id INTEGER UNIQUE PRIMARY KEY AUTOINCREMENT             ok
		//	id INTEGER CONSTRAINT cx NOT NULL PRIMARY KEY AUTO...   ok
		//	id INTEGER COLLATE NOCASE PRIMARY KEY AUTOINCREMENT     ok
		//	id INTEGER CHECK(id>0) PRIMARY KEY AUTOINCREMENT        ok
		//	id INT NOT NULL PRIMARY KEY AUTOINCREMENT               ERROR
		//	id BIGINT NOT NULL PRIMARY KEY AUTOINCREMENT            ERROR
		//	id INTEGER(10) NOT NULL PRIMARY KEY AUTOINCREMENT       ERROR
		//
		// (a sized integer keeps its parenthesized part in the type).
		columnConstraintKeywords := map[string]bool{
			"CONSTRAINT": true, "PRIMARY": true, "NOT": true, "NULL": true,
			"UNIQUE": true, "CHECK": true, "DEFAULT": true, "COLLATE": true,
			"REFERENCES": true, "GENERATED": true, "AS": true,
		}
		typeEnd := 1
		for typeEnd < len(seg) && !(seg[typeEnd].kind == tkIdent && columnConstraintKeywords[seg[typeEnd].upper()]) {
			typeEnd++
		}
		declType := joinTypeTokens(seg[1:typeEnd])
		j := typeEnd
		for j < len(seg) && !(seg[j].kind == tkIdent && seg[j].upper() == "PRIMARY") {
			j++
		}
		if j >= len(seg) {
			// parse.y:414 has "ccons ::= PRIMARY KEY sortorder onconf autoinc" and
			// :430-431 make autoinc an optional AUTOINCR, so the keyword exists in
			// the grammar ONLY after PRIMARY KEY: C reaches it as a parse error at
			// the token rather than as a rule about the column.
			return "", false, fmt.Errorf("near \"AUTOINCREMENT\": syntax error")
		}
		j++ // past PRIMARY
		if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "KEY" {
			j++
		}
		if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "DESC" {
			// A DESC key is not the rowid (build.c:1868-1871 requires
			// sortOrder!=SQLITE_SO_DESC), so autoInc reaches the error arm at
			// build.c:1883-1886.
			return "", false, fmt.Errorf("AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
		}
		if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "ASC" {
			j++
		}
		// ccons ::= PRIMARY KEY sortorder onconf autoinc (parse.y:414): a
		// conflict clause may sit between the key and AUTOINCREMENT.
		if _, nk, ok, perr := parseTrailingConflictClause(seg, j); perr != nil {
			return "", false, perr
		} else if ok {
			j = nk
		}
		if !(j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "AUTOINCREMENT") {
			return "", false, fmt.Errorf("column %s: unsupported AUTOINCREMENT placement", name)
		}
		if !equalFoldName(strings.TrimSpace(declType), "INTEGER") {
			// build.c:1885-1886 -- the whole message, which names neither the column
			// nor its declared type.
			return "", false, fmt.Errorf("AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
		}
		return name, true, nil
	}
	return "", false, fmt.Errorf("unsupported AUTOINCREMENT placement")
}

// parseCreateTableName recovers just the table name from a CREATE TABLE
// statement's SQL text (CREATE [TEMP|TEMPORARY] TABLE [IF NOT EXISTS]
// [schema.]name ...), the same prefix parseCreateTableColumnsAndAutoIndexes walks past
// before parsing the column list -- kept separate rather than having that
// function also return the name, since it's a read-side function this
// writer would rather not change the signature of.
func parseCreateTableName(sqlText string) (name string, isTemp, ifNotExists bool, err error) {
	toks, err := lex(sqlText)
	if err != nil {
		return "", false, false, err
	}
	p := newParser(sqlText, toks)
	name, isTemp, err = parseCreateTableNamePrefix(p)
	return name, isTemp, p.sawIfNotExists, err
}

// parseCreateTableNamePrefix parses "CREATE [TEMP|TEMPORARY] TABLE [IF NOT
// EXISTS] [schema.]name" off the front of p, leaving p positioned right
// after it -- at "(" for an ordinary CREATE TABLE's column list, or at "AS"
// for a CREATE TABLE ... AS SELECT (CTAS, see tryParseCreateTableAsSelect,
// which keeps parsing from there). Factored out of parseCreateTableName so
// both callers share this identical prefix grammar/errors -- unchanged
// behavior from before this function existed, just no longer tied to
// building its own throwaway parser over a freshly-lexed copy of the text.
func parseCreateTableNamePrefix(p *parser) (string, bool, error) {
	if !p.consumeKeyword("CREATE") {
		return "", false, fmt.Errorf("engine: CREATE TABLE: expected CREATE, got %q", p.tokenDesc(p.peek()))
	}
	isTemp := p.consumeKeyword("TEMP") || p.consumeKeyword("TEMPORARY")
	if !p.consumeKeyword("TABLE") {
		return "", false, fmt.Errorf("engine: CREATE TABLE: expected TABLE, got %q", p.tokenDesc(p.peek()))
	}
	if p.consumeKeyword("IF") {
		if !p.consumeKeyword("NOT") || !p.consumeKeyword("EXISTS") {
			return "", false, fmt.Errorf("engine: CREATE TABLE: expected NOT EXISTS after IF")
		}
		p.sawIfNotExists = true
	}
	// A single-quoted table NAME ("CREATE TABLE 't_content'(...)" -- the
	// spelling fts3/fts4 use for their shadow tables) is an identifier here,
	// not a string literal; see sql_parser.go's asCreateTableIdent.
	t, tOK := asCreateTableIdent(p.peek())
	if !tOK {
		return "", false, fmt.Errorf("engine: CREATE TABLE: expected table name, got %q", p.tokenDesc(p.peek()))
	}
	p.next()
	name := t.text
	if p.peekIsPunct(".") {
		p.next()
		t2 := p.peek()
		if t2.kind != tkIdent {
			return "", false, fmt.Errorf("engine: CREATE TABLE: expected table name after schema qualifier")
		}
		p.next()
		// "main."/"temp." SELECT one of this engine's two catalogs
		// (temp_schema.go); anything else names an ATTACHed database, which
		// this single-file write path has no concept of at all -- silently
		// discarding the qualifier and creating the table in THIS file anyway
		// would put it somewhere C SQLite never would (verified directly
		// against C SQLite's own "unknown database <name>" rejection,
		// misc7.test/misc8.test).
		scope, ok := scopeOfQualifier(t.text)
		if !ok {
			return "", false, fmt.Errorf("engine: CREATE TABLE: unknown database %s", t.text)
		}
		// "CREATE TEMP TABLE main.q(a)" is rejected outright by C SQLite
		// ("temporary table name must be unqualified"), verified directly,
		// while "CREATE TEMP TABLE temp.q(a)" -- the qualifier agreeing with
		// the keyword -- is accepted.
		if isTemp && scope != scopeTemp {
			return "", false, fmt.Errorf("engine: temporary table name must be unqualified")
		}
		isTemp = scope == scopeTemp
		name = t2.text
	}
	return name, isTemp, nil
}

// tryParseCreateTableAsSelect detects and parses the CREATE TABLE ... AS
// SELECT (CTAS) form: "CREATE [TEMP|TEMPORARY] TABLE [IF NOT EXISTS]
// [schema.]name AS <select-stmt>". ok is false (name/stmt both zero) whenever
// createSQL's CREATE TABLE prefix is followed by anything other than AS --
// ordinarily "(", the column-list form -- which is not itself an error:
// CreateTable falls through to its usual column-list parsing in that case.
// C SQLite's CTAS grammar has no column list of its own (the SELECT's own
// result columns become the new table's columns, see createTableAsSelect), so
// a "CREATE TABLE name (cols) AS SELECT ..." form, if written, is left for
// the ordinary column-list parser below to reject on its own terms, exactly
// like C SQLite does.
func tryParseCreateTableAsSelect(createSQL string) (name string, isTemp bool, stmt *SelectStmt, ok bool, err error) {
	toks, err := lex(createSQL)
	if err != nil {
		return "", false, nil, false, err
	}
	p := newParser(createSQL, toks)
	name, isTemp, err = parseCreateTableNamePrefix(p)
	if err != nil {
		return "", false, nil, false, err
	}
	if !p.consumeKeyword("AS") {
		return "", false, nil, false, nil
	}
	sel, err := p.parseSelectStmt()
	if err != nil {
		return "", false, nil, false, err
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return "", false, nil, false, fmt.Errorf("engine: CREATE TABLE ... AS SELECT: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	return name, isTemp, sel, true, nil
}

// createTableAsSelect implements CREATE TABLE t AS SELECT ...: t's columns are
// the SELECT's result names (expandSelectList over the leftmost arm, as an
// ordinary SELECT reports them), each typed from its source:
// compoundColumnAffinity for a compound, otherwise a direct column reference's
// declared type (any other expression gets none). t is a plain rowid table with
// no PRIMARY KEY/UNIQUE/NOT NULL/DEFAULT, as in C, even when copying an INTEGER
// PRIMARY KEY. It is filled by running the SELECT (execSelect) and feeding each
// row through insertRowFromValues.
func (db *DB) createTableAsSelect(name string, isTemp bool, sel *SelectStmt) error {
	if err := db.checkReservedObjectName(name); err != nil {
		return err
	}
	scope := createScope(isTemp)
	if db.findTableMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: table %s already exists", name)
	}
	if db.findViewMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: view %s already exists", name)
	}
	if db.findIndexMetaIn(scope, name) != nil {
		return fmt.Errorf("engine: there is already an index named %s", name)
	}
	for _, c := range sel.Columns {
		if c.Star && len(sel.From) == 0 {
			return fmt.Errorf("engine: CREATE TABLE %s AS SELECT: no tables specified", name)
		}
	}

	pager, err := db.SnapshotPager()
	if err != nil {
		return fmt.Errorf("engine: CREATE TABLE %s AS SELECT: %w", name, err)
	}
	// C SQLite reserves the new table's schema row BEFORE running the
	// SELECT, so a catalog this SELECT reads shows one extra all-NULL row --
	// in the catalog the new table belongs to, and only there. Mark the
	// snapshot so every read of that catalog within this statement (including
	// a nested subquery's own count) sees it; ReadOnlyPager.ctasPlaceholder
	// has the oracle evidence. Without this, "CREATE TABLE x AS SELECT ...
	// FROM sqlite_master" came out one row short -- a pre-existing wrong
	// answer, independent of any temp object.
	pager.ctasPlaceholder = true
	pager.ctasPlaceholderScope = scope
	// A TEMP catalog read routes to the TEMP database's own pager (itemOwner,
	// cross_db.go), so that pager needs the same mark -- the reserved row is in
	// the catalog the new table belongs to, and for a TEMP table that is temp's.
	if scope == scopeTemp {
		for _, ar := range pager.attachedReaders {
			if ar.name == "temp" {
				ar.pager.ctasPlaceholder, ar.pager.ctasPlaceholderScope = true, scope
			}
		}
	}
	// A table reachable only through a short-circuited subquery must still
	// be validated at prepare time, as in C (validateSelectTablesExist).
	if err := validateSelectTablesExist(pager, sel); err != nil {
		return fmt.Errorf("engine: CREATE TABLE %s AS SELECT: %w", name, err)
	}

	// scopes gives expandSelectList the schema (column names/types, per
	// FROM-item alias) needed to expand "*"/"t.*" and to resolve a direct
	// column reference's own source columnInfo below -- a schema-only need,
	// even though resolveFrom (join.go) also loads every joined table's rows
	// (needed for an ordinary join execution, not for this): harmless here,
	// just extra work pager.execSelect's own later call redoes properly.
	var scopes []tableScope
	if len(sel.From) > 0 {
		jts, _, ferr := pager.resolveFrom(sel.From, nil)
		if ferr != nil {
			return ferr
		}
		scopes = buildScopes(jts)
	}
	outCols, err := expandSelectList(sel.Columns, scopes, defaultColNameMode)
	if err != nil {
		return err
	}
	if len(outCols) == 0 {
		return fmt.Errorf("engine: CREATE TABLE %s AS SELECT: result has no columns", name)
	}
	// A duplicate output name is renamed, not rejected: CTAS builds its table
	// through sqlite3ResultSetOfSelect (select.c:2465), i.e.
	// sqlite3ColumnsFromExprList's ":N" uniquifier; only an explicit column
	// list rejects duplicates. r32mUniqueColumnNames does the renaming: a name
	// already ending in ":<digits>" has that tail replaced ('a:9','a:9' is
	// a:9,"a:1"), and a taken suffix keeps bumping ('a:1',a,a is a:1,a,"a:2").
	ctasNames := make([]string, len(outCols))
	for i := range outCols {
		ctasNames[i] = outCols[i].name
	}
	uniqNames, uniqOK := r32mUniqueColumnNames(ctasNames)
	if !uniqOK {
		// Past the fifth repeat SQLite uses sqlite3_randomness (random.c:59), so
		// there is nothing to match. A CTAS is an exec statement, whose column
		// names the harness never compares (distinct.test#4 only counts rows), so
		// finish deterministically rather than decline; the schema still needs
		// unique names.
		uniqNames = r32mFinishUniqueColumnNames(ctasNames)
	}
	for i := range outCols {
		outCols[i].name = uniqNames[i]
	}

	// schemaCtx is a schema-only evalCtx (no row values, as validateColumnRefs
	// uses resolveColumn at plan time): only used here to
	// recover a direct column reference's own columnInfo (for its DeclType)
	// and each output expression's AFFINITY, never to evaluate a value. It
	// carries the pager because exprAffinity's scalar-subquery case needs a
	// schema to resolve the subquery's single output column through
	// (derivedColumnInfos) -- without it "(SELECT s FROM t)" would silently
	// come out with no affinity where SQLite gives it s's own.
	schemaCtx := &evalCtx{tables: scopes, pager: pager}
	cols := make([]columnInfo, len(outCols))
	// flexnumCols are output positions whose folded affinity is
	// SQLITE_AFF_FLEXNUM. It is stored as " NUM", so later statements and
	// reopens see NUMERIC; but this CTAS's own rows go in under FLEXNUM, for
	// which applyAffinity skips sqlite3VdbeIntegerAffinity
	// ("affinity<=SQLITE_AFF_REAL"), so a REAL NUMERIC would fold stays REAL:
	//
	//	CREATE TABLE u AS VALUES(CAST(1 AS REAL)),(CAST(1 AS REAL))
	//	                  UNION ALL SELECT NULL;
	//	  sqlite_master:  CREATE TABLE u(column1 NUM)
	//	  typeof/quote:   real|1.0, real|1.0, null|NULL
	//	INSERT INTO u VALUES(CAST(1 AS REAL));   -- on ANY connection
	//	  typeof/quote:   ..., integer|1         <- plain NUMERIC again
	var flexnumCols []int
	if len(sel.Compound) > 0 {
		// arms holds every arm's schema-only expanded select list: the
		// leftmost (resolved above) plus one per sel.Compound entry
		// (resolveArmOutputs, sql_compound.go), so compoundColumnAffinity
		// sees every arm at each output position.
		arms := make([]armOutput, 0, 1+len(sel.Compound))
		arms = append(arms, armOutput{ctx: schemaCtx, outCols: outCols, ok: true})
		for _, arm := range sel.Compound {
			arms = append(arms, pager.resolveArmOutputs(arm.Stmt))
		}
		// A multi-row VALUES clause is ONE arm, not one per row -- arms above
		// is the parser's desugaring, which for this purpose is the wrong
		// chain (see valuesFold, sql_ast.go). compoundColumnTypes walks the
		// arms SQLite actually folds; it declines the shapes armColClassify
		// cannot classify (a scalar-subquery arm, most of all), and those fall
		// back to the per-arm walk below, which resolves them through
		// exprAffinity instead.
		folded, foldedOK := pager.compoundColumnTypes(sel, true)
		if len(folded) != len(outCols) {
			foldedOK = false
		}
		for i, oc := range outCols {
			aff := compoundColumnAffinity(arms, i)
			if foldedOK {
				aff = folded[i].aff
				if folded[i].flexnum {
					// SQLITE_AFF_FLEXNUM: createTableStmt writes it " NUM",
					// not " INT"/" REAL" -- but the rows THIS statement stores
					// still go in under FLEXNUM itself. See flexnumCols.
					aff = affNumeric
					flexnumCols = append(flexnumCols, i)
				}
			}
			cols[i] = columnInfo{Name: oc.name, DeclType: canonicalAffinityTypeName(aff), Aff: aff}
		}
	} else {
		for i, oc := range outCols {
			var declType string
			if colRef, isCol := oc.expr.(ColumnExpr); isCol {
				if _, _, srcCol, _, rerr := resolveColumn(schemaCtx, colRef.Qualifier, colRef.Name); rerr == nil && srcCol != nil {
					declType = srcCol.DeclType
				}
			}
			// The column's affinity is the output expression's (exprAffinity mirrors
			// sqlite3ExprAffinity: column, CAST, scalar subquery, COLLATE
			// pass-through), as sqlite3SubqueryColumnTypes fills Column.affinity
			// from which createTableStmt writes the stored text:
			//
			//	CREATE TABLE u AS SELECT cast(i AS text) AS a FROM t;
			//	INSERT INTO u VALUES(5); SELECT typeof(a), a FROM u;
			//	  -- text|1  text|5      (TEXT affinity converts the 5)
			//
			// declType keeps the source column's spelling; the schema is re-read
			// from the generated text.
			cols[i] = columnInfo{Name: oc.name, DeclType: declType, Aff: exprAffinity(schemaCtx, oc.expr)}
		}
	}

	createSQL := buildCTASCreateSQL(name, cols)
	// Re-derive cols/sql through the ordinary CREATE TABLE column parser
	// rather than using the columnInfo slice just built directly: this keeps
	// tbl.cols byte-for-byte consistent with what parseCreateTableColumnsAndAutoIndexes
	// would recover from tbl.sql later (query.go's resolveTable, and
	// OpenWrite's catalog load (schema_load_objects.go) on a future reopen) -- exactly the same
	// invariant an ordinary (non-CTAS) CreateTable already maintains between
	// its createSQL argument and that same parser, rather than a second,
	// independent (and possibly divergent) code path for CTAS alone.
	parsedCols, _, _, err := parseCreateTableColumnsAndAutoIndexes(createSQL, false)
	if err != nil {
		return fmt.Errorf("engine: CREATE TABLE %s AS SELECT: internal error building schema %q: %w", name, createSQL, err)
	}

	tbl := &tableMeta{schemaSeq: db.nextSchemaSeq(isTemp), isTemp: isTemp, name: name, sql: withTempKeywordIf(createSQL, isTemp), cols: parsedCols, ipkIndex: -1, rows: newRowStore(map[uint64][]Value{}, nil), loaded: true}
	if err := db.newTableStore(tbl); err != nil {
		return err
	}

	// Populate before registering the table (db.tables/db.schemaCookie), so a
	// SELECT error leaves db.tables untouched.
	selCols, selRows, err := pager.execSelect(sel, nil, nil, "")
	if err != nil {
		return err
	}
	if len(selCols) != len(tbl.cols) {
		return fmt.Errorf("engine: internal error: CREATE TABLE %s AS SELECT: column count mismatch (%d select columns, %d table columns)", name, len(selCols), len(tbl.cols))
	}
	// Store this statement's own rows under FLEXNUM where that is the folded
	// affinity: apply it here and neutralize the column's own NUMERIC for the
	// population pass only, so tbl.cols keeps matching the CREATE TABLE text
	// it was parsed from (which is what every later insert and every reopen
	// uses). See flexnumCols above.
	for _, i := range flexnumCols {
		if i < len(tbl.cols) {
			defer func(i int, a affinity) { tbl.cols[i].Aff = a }(i, tbl.cols[i].Aff)
			tbl.cols[i].Aff = affNone
		}
	}
	for _, rowVals := range selRows {
		for _, i := range flexnumCols {
			if i < len(rowVals) && rowVals[i].Typ != Float {
				rowVals[i] = applyAffinityToValue(rowVals[i], affNumeric)
			}
		}
		if _, ierr := db.insertRowFromValues(tbl, name, false, nil, rowVals); ierr != nil {
			return ierr
		}
	}

	db.tables = append(db.tables, tbl)
	db.bumpSchema(isTemp)
	return nil
}

// canonicalAffinityTypeName renders a as the exact declared-type TEXT real
// SQLite's own CREATE TABLE AS SELECT writes for a column of that affinity.
// It IS SQLite's createTableStmt azType[] table, which is indexed by
// affinity alone -- so a compound CTAS column's declared type is never any
// arm's own verbatim decltype text ("VARCHAR(10)"/"BIGINT" are never
// preserved), always one of exactly these five spellings. SQLITE_AFF_FLEXNUM
// shares " NUM" with SQLITE_AFF_NUMERIC, which is why this engine's single
// affNumeric covers both.
func canonicalAffinityTypeName(a affinity) string {
	switch a {
	case affInteger:
		return "INT"
	case affText:
		return "TEXT"
	case affReal:
		return "REAL"
	case affNumeric:
		return "NUM"
	default: // affNone
		return ""
	}
}

// compoundColumnAffinity is the affinity CREATE TABLE ... AS SELECT gives
// output column i of a compound, given every arm's resolved select list
// leftmost first. A port of sqlite3SubqueryColumnTypes' compound loop with aff
// == SQLITE_AFF_NONE (what CTAS passes). A probing-derived rule it replaced
// disagreed on 104 of 400 pairs, e.g.:
//
//	SELECT i FROM t UNION ALL SELECT cast(i AS text)  cgo (none)  was INT
//	SELECT i FROM t UNION ALL SELECT x'00'            cgo INT     was (none)
//	SELECT i FROM t UNION ALL SELECT abs(i)           cgo (none)  was INT
//	SELECT s FROM t UNION ALL SELECT i COLLATE nocase cgo (none)  was TEXT
//
// The algorithm:
//
//  1. The affinity is the first arm's; only if none does it move right,
//     remembering skipped arms' data-type bits. First wins; no later arm
//     changes INTEGER to REAL.
//  2. Once set (and if other arms exist), every other arm's data-type bits are
//     ORed in, and the affinity is demoted to none on a contradiction: TEXT
//     against possibly-numeric, numeric against possibly-text.
//  3. A numeric affinity surviving step 2 becomes FLEXNUM (" NUM") when the
//     first arm is literally a CAST ("SELECT cast(i AS integer) UNION ALL
//     SELECT x'00'" is NUM).
func compoundColumnAffinity(arms []armOutput, i int) affinity {
	// armExpr is arm k's expression at this output position. An arm this
	// engine could not resolve at all reports false, and every use below
	// then takes the conservative branch (no affinity of its own, and the
	// "could be anything" data-type bits, which can only demote).
	armExpr := func(k int) (Expr, *evalCtx, bool) {
		if k < 0 || k >= len(arms) || !arms[k].ok || i >= len(arms[k].outCols) {
			return nil, nil, false
		}
		return arms[k].outCols[i].expr, arms[k].ctx, true
	}
	affAt := func(k int) affinity {
		e, ctx, ok := armExpr(k)
		if !ok {
			return affNone
		}
		return exprAffinity(ctx, e)
	}
	dataAt := func(k int) int {
		e, ctx, ok := armExpr(k)
		if !ok {
			return exprDataAny
		}
		return exprDataType(ctx, e)
	}

	// Step 1: the leftmost arm that has an affinity at all establishes it.
	k, m := 0, 0
	aff := affAt(0)
	for aff == affNone && k+1 < len(arms) {
		m |= dataAt(k)
		k++
		aff = affAt(k)
	}
	if aff == affNone {
		return affNone
	}
	// Step 2/3 run only when some arm other than the establishing one exists
	// -- SQLite's own "(pS2->pNext || pS2!=pSelect)" guard.
	if k+1 >= len(arms) && k == 0 {
		return aff
	}
	for j := k + 1; j < len(arms); j++ {
		m |= dataAt(j)
	}
	switch {
	case aff == affText && m&exprDataNumeric != 0:
		return affNone
	case isNumericAffinity(aff) && m&exprDataText != 0:
		return affNone
	}
	if isNumericAffinity(aff) {
		if e, _, ok := armExpr(0); ok {
			if _, isCast := e.(CastExpr); isCast {
				return affNumeric // SQLITE_AFF_FLEXNUM, which prints " NUM"
			}
		}
	}
	return aff
}

// exprDataType's bit vocabulary, from SQLite's sqlite3ExprDataType: what
// storage classes an expression's value could POSSIBLY have. Only the first
// two bits are ever tested (by compoundColumnAffinity's demotion step); the
// blob bit exists because SQLite's own combinations set it, and because a
// blob literal must NOT read as "could be numeric" -- that is exactly the
// case the probed rule got backwards ("SELECT i UNION ALL SELECT x'00'" is
// INT, not typeless).
const (
	exprDataNumeric = 0x01
	exprDataText    = 0x02
	exprDataBlob    = 0x04
	exprDataAny     = exprDataNumeric | exprDataText | exprDataBlob
)

// exprDataType ports sqlite3ExprDataType. Note which nodes are NOT here:
// every comparison, every arithmetic operator and every numeric literal
// falls to the default 0x01 ("could be numeric"), and NULL alone contributes
// nothing at all.
func exprDataType(ctx *evalCtx, e Expr) int {
	for {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
		case UnaryExpr:
			if x.Op == "+" { // TK_UPLUS: transparent, like COLLATE
				e = x.X
				continue
			}
			return exprDataNumeric
		case LiteralExpr:
			switch x.Val.Typ {
			case Null:
				return 0
			case Text:
				return exprDataText
			case Blob:
				return exprDataBlob
			}
			return exprDataNumeric
		case BinaryExpr:
			if x.Op == "||" { // TK_CONCAT
				return exprDataText | exprDataBlob
			}
			return exprDataNumeric
		case FuncExpr, ParamExpr:
			return exprDataAny
		case ColumnExpr, CastExpr, SubqueryExpr:
			switch a := exprAffinity(ctx, e); {
			case isNumericAffinity(a):
				return exprDataNumeric | exprDataBlob
			case a == affText:
				return exprDataText | exprDataBlob
			default:
				return exprDataAny
			}
		case CaseExpr:
			// Only the THEN arms and the ELSE carry a value -- SQLite walks
			// the odd entries of its own when/then list plus the trailing
			// ELSE, never the WHEN conditions or a CASE's base operand.
			res := 0
			for _, w := range x.Whens {
				res |= exprDataType(ctx, w.Then)
			}
			if x.Else != nil {
				res |= exprDataType(ctx, x.Else)
			}
			return res
		default:
			return exprDataNumeric
		}
	}
}

// normalizeSchemaSQL rewrites a CREATE statement into the text SQLite stores:
// "CREATE <kind> " followed by the verbatim source from the object-name token
// to the last token (sqlite3EndTable and sqlite3CreateIndex use
// sqlite3MPrintf("CREATE %s %.*s", zType, n, pParse->sNameToken.z)):
//
//	CREATE TABLE main.t1(a, b, c)        -> CREATE TABLE t1(a, b, c)
//	CREATE   TABLE   IF NOT EXISTS q2 (a) -> CREATE TABLE q2 (a)
//	CREATE  UNIQUE  INDEX  main.qi1  ON  q1 ( a )
//	                                     -> CREATE UNIQUE INDEX qi1  ON  q1 ( a )
//	CREATE  VIEW  main.qv1  AS  SELECT 1 -> CREATE VIEW qv1  AS  SELECT 1
//
// A trailing ";" ("if( pEnd2->z[0]!=';' )") and trailing whitespace are
// dropped. kind is "TABLE", "INDEX", "VIEW" or "TRIGGER"; a UNIQUE index keeps
// "UNIQUE". Text it cannot confidently re-render is returned unchanged.
func normalizeSchemaSQL(kind, sqlText string) string {
	toks, err := lex(sqlText)
	if err != nil {
		return sqlText
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return sqlText
	}
	// TEMP/TEMPORARY is kept in the text this session holds in memory, and
	// taken back out when the row is written (withoutTempKeyword): C SQLite's
	// sqlite_temp_master stores plain
	// "CREATE TABLE ...", and the TEMP database's own file is what says the
	// object is temp (temp_store.go). In memory the keyword is still what
	// several readers classify off (isTempCreateSQL, temp_schema.go).
	temp := ""
	switch {
	case kw("TEMP"):
		temp = "TEMP "
	case kw("TEMPORARY"):
		temp = "TEMPORARY "
	}
	unique := kw("UNIQUE")
	if !kw(kind) {
		return sqlText
	}
	if kw("IF") {
		if !kw("NOT") || !kw("EXISTS") {
			return sqlText
		}
	}
	if i >= len(toks) || toks[i].kind != tkIdent {
		return sqlText
	}
	// "schema.name": the stored text starts at the NAME, dropping the
	// qualifier -- SQLite points sNameToken at the second name for exactly
	// this reason.
	name := i
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && toks[i+2].kind == tkIdent {
		name = i + 2
	}
	end := -1
	for j := len(toks) - 1; j >= name; j-- {
		if toks[j].kind == tkEOF || (toks[j].kind == tkPunct && toks[j].text == ";") {
			continue
		}
		end = toks[j].End
		break
	}
	if end < 0 || end > len(sqlText) || toks[name].Start >= end {
		return sqlText
	}
	prefix := "CREATE " + temp + kind + " "
	if unique {
		prefix = "CREATE " + temp + "UNIQUE " + kind + " "
	}
	return prefix + sqlText[toks[name].Start:end]
}

// allSQLKeywords is SQLite's full keyword list -- BOTH the ones its grammar
// lets fall back to an identifier and the ones it does not. ctasIdent quotes
// any of them, which is what SQLite's own identPut does (verified: a CTAS
// column aliased "abort", a fallback keyword, still comes back quoted).
// nonIdentifierKeywords (sql_parser.go) is the strictly smaller set that
// cannot be a bare COLUMN NAME at all; the two are deliberately separate.
var allSQLKeywords = map[string]bool{
	"ABORT": true, "ACTION": true, "ADD": true, "AFTER": true, "ALL": true,
	"ALTER": true, "ALWAYS": true, "ANALYZE": true, "AND": true, "AS": true,
	"ASC": true, "ATTACH": true, "AUTOINCREMENT": true, "BEFORE": true,
	"BEGIN": true, "BETWEEN": true, "BY": true, "CASCADE": true, "CASE": true,
	"CAST": true, "CHECK": true, "COLLATE": true, "COLUMN": true,
	"COMMIT": true, "CONFLICT": true, "CONSTRAINT": true, "CREATE": true,
	"CROSS": true, "CURRENT": true, "CURRENT_DATE": true,
	"CURRENT_TIME": true, "CURRENT_TIMESTAMP": true, "DATABASE": true,
	"DEFAULT": true, "DEFERRABLE": true, "DEFERRED": true, "DELETE": true,
	"DESC": true, "DETACH": true, "DISTINCT": true, "DO": true, "DROP": true,
	"EACH": true, "ELSE": true, "END": true, "ESCAPE": true, "EXCEPT": true,
	"EXCLUDE": true, "EXCLUSIVE": true, "EXISTS": true, "EXPLAIN": true,
	"FAIL": true, "FILTER": true, "FIRST": true, "FOLLOWING": true,
	"FOR": true, "FOREIGN": true, "FROM": true, "FULL": true,
	"GENERATED": true, "GLOB": true, "GROUP": true, "GROUPS": true,
	"HAVING": true, "IF": true, "IGNORE": true, "IMMEDIATE": true, "IN": true,
	"INDEX": true, "INDEXED": true, "INITIALLY": true, "INNER": true,
	"INSERT": true, "INSTEAD": true, "INTERSECT": true, "INTO": true,
	"IS": true, "ISNULL": true, "JOIN": true, "KEY": true, "LAST": true,
	"LEFT": true, "LIKE": true, "LIMIT": true, "MATCH": true,
	"MATERIALIZED": true, "NATURAL": true, "NO": true, "NOT": true,
	"NOTHING": true, "NOTNULL": true, "NULL": true, "NULLS": true, "OF": true,
	"OFFSET": true, "ON": true, "OR": true, "ORDER": true, "OTHERS": true,
	"OUTER": true, "OVER": true, "PARTITION": true, "PLAN": true,
	"PRAGMA": true, "PRECEDING": true, "PRIMARY": true, "QUERY": true,
	"RAISE": true, "RANGE": true, "RECURSIVE": true, "REFERENCES": true,
	"REGEXP": true, "REINDEX": true, "RELEASE": true, "RENAME": true,
	"REPLACE": true, "RESTRICT": true, "RETURNING": true, "RIGHT": true,
	"ROLLBACK": true, "ROW": true, "ROWS": true, "SAVEPOINT": true,
	"SELECT": true, "SET": true, "TABLE": true, "TEMP": true,
	"TEMPORARY": true, "THEN": true, "TIES": true, "TO": true,
	"TRANSACTION": true, "TRIGGER": true, "UNBOUNDED": true, "UNION": true,
	"UNIQUE": true, "UPDATE": true, "USING": true, "VACUUM": true,
	"VALUES": true, "VIEW": true, "VIRTUAL": true, "WHEN": true,
	"WHERE": true, "WINDOW": true, "WITH": true, "WITHOUT": true,
}

// buildCTASCreateSQL renders name/cols into a verbatim "CREATE TABLE
// name(...)" SQL text for a just-derived CTAS table (see
// createTableAsSelect): every column is double-quoted (so a non-identifier
// result-column name -- an unaliased expression's own verbatim source text,
// e.g. "a+1" or "count(*)" -- round-trips through the lexer as a single
// quoted identifier, exactly like any ordinary SELECT's default column
// naming already produces) with its declared type, if any, trailing,
// space-separated -- and no other clause (constraint, PRIMARY KEY, ...) at
// all, matching C SQLite's own CTAS-generated schema: a plain rowid table
// with zero constraints.
func buildCTASCreateSQL(name string, cols []columnInfo) string {
	// Column type names come from the AFFINITY, not the source column's
	// declared type: SQLite's own azType[] table, indexed by affinity.
	typeOf := func(c columnInfo) string {
		switch c.Aff {
		case affText:
			return " TEXT"
		case affNumeric:
			return " NUM"
		case affInteger:
			return " INT"
		case affReal:
			return " REAL"
		}
		return "" // BLOB / no affinity: no type name at all
	}
	// SQLite breaks the definition across lines once its own size estimate
	// reaches 50: identLength(z) = len(z) + one per embedded quote + 2, summed
	// as identLength(col)+5 per column plus identLength(table).
	identLen := func(z string) int { return len(z) + strings.Count(z, `"`) + 2 }
	n := identLen(name)
	for _, c := range cols {
		n += identLen(c.Name) + 5
	}
	sep, sep2, end := "", ",", ")"
	if n >= 50 {
		sep, sep2, end = "\n  ", ",\n  ", "\n)"
	}

	var sb strings.Builder
	sb.WriteString("CREATE TABLE ")
	sb.WriteString(ctasIdent(name))
	sb.WriteByte('(')
	for i, c := range cols {
		if i == 0 {
			sb.WriteString(sep)
		} else {
			sb.WriteString(sep2)
		}
		sb.WriteString(ctasIdent(c.Name))
		sb.WriteString(typeOf(c))
	}
	sb.WriteString(end)
	return sb.String()
}

// ctasIdent renders one identifier the way SQLite's identPut does: quoted
// ONLY when it has to be -- it is empty, starts with a digit, contains a
// character outside [A-Za-z0-9_], or is a KEYWORD (any keyword, including the
// ones SQLite's grammar otherwise lets fall back to an identifier: verified
// directly, a CTAS column aliased "abort" comes back as "abort" quoted).
// An embedded double quote is doubled.
func ctasIdent(name string) string {
	quote := name == ""
	if !quote {
		if c := name[0]; c >= '0' && c <= '9' {
			quote = true
		}
	}
	if !quote {
		for i := 0; i < len(name); i++ {
			c := name[i]
			if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				quote = true
				break
			}
		}
	}
	if !quote && allSQLKeywords[strings.ToUpper(name)] {
		quote = true
	}
	if !quote {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteIdent renders name as a double-quoted SQL identifier, doubling any
// embedded '"' per standard SQL quoting -- safe for any string, not just an
// ordinary identifier-shaped one.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// checkReservedObjectName rejects a user object named with the "sqlite_"
// prefix SQLite reserves: CREATE TABLE/INDEX/VIEW/TRIGGER and ALTER TABLE ...
// RENAME TO all report "object name reserved for internal use: <name>",
// case-insensitively, quoted or not, IF NOT EXISTS notwithstanding. "sqlitex"
// is fine. db.internalSchemaInit is C's db->init.busy, for ANALYZE creating
// sqlite_stat1 (storeStat1).
//
// "PRAGMA writable_schema=ON" skips the whole check (sqlite3CheckObjectName,
// build.c:1038; off, build.c:1054 refuses every "sqlite_" name):
//
//	CREATE TABLE sqlite_nope(x)                 OK
//	CREATE VIEW  sqlite_myview AS SELECT 1      OK
//	ALTER TABLE t1 RENAME TO sqlite_bad         OK
//	CREATE INDEX <any name> ON sqlite_nope(x)   table sqlite_nope may not be indexed
//	CREATE TRIGGER ... ON sqlite_nope           cannot create trigger on system table
//	ALTER TABLE sqlite_nope ADD COLUMN b        table sqlite_nope may not be altered
//	DROP TABLE sqlite_nope                      table sqlite_nope may not be dropped
//
// The system-table refusals are keyed on the table and live elsewhere
// (index_write.go, trigger.go, alter_write.go, drop_write.go).
// sqlite_stat1 and sqlite_sequence need no special case: they are real tables
// once created (ensureStat1Table, createSequenceTable), so a later CREATE hits
// "table already exists".
func (db *DB) checkReservedObjectName(name string) error {
	if db.internalSchemaInit || !strings.HasPrefix(r33sFoldIdent(name), "sqlite_") {
		return nil
	}
	if !db.writableSchema {
		return fmt.Errorf("engine: object name reserved for internal use: %s", name)
	}
	return nil
}

// sqliteSequenceCreateSQL is the exact CREATE TABLE text C SQLite gives its
// own "sqlite_sequence" table (build.c:2927): two untyped columns, "name" (an
// AUTOINCREMENT table's name) and "seq" (the largest rowid it has handed out).
const sqliteSequenceCreateSQL = "CREATE TABLE sqlite_sequence(name,seq)"

// sqliteSequenceTableName is AUTOINCREMENT's bookkeeping table. Every database
// has its own, created with that database's first AUTOINCREMENT table
// (build.c:2925's per-database pDb->pSchema->pSeqTab check) -- main's in main's
// file, TEMP's in the temp database's (temp_store.go).
const sqliteSequenceTableName = "sqlite_sequence"

// parseTableTailClauses scans past a CREATE TABLE's top-level column list and
// reports which options follow: WITHOUT ROWID (tableMeta.withoutRowid/pkIndex)
// and STRICT (tableMeta.strict). AUTOINCREMENT and CHECK are handled elsewhere
// (detectAutoIncrementColumn, finalizeCheckConstraints).
//
// The options are order-independent and may repeat ("...) STRICT, WITHOUT
// ROWID", "...) STRICT, STRICT"); C requires the comma ("...) STRICT WITHOUT
// ROWID" is a syntax error), which this scan does not enforce, since no stored
// schema row can lack it.
//
// After the tail only an optional ";" and EOF are allowed, so "CREATE TABLE
// t(...); INSERT INTO t VALUES(1);" in one Exec is the usual multi-statement
// error rather than silently dropping the INSERT.
func parseTableTailClauses(sqlText string) (withoutRowid, strict bool, err error) {
	toks, err := lex(sqlText)
	if err != nil {
		return false, false, err
	}
	depth := 0
	seenOpen := false
	for i, t := range toks {
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
			seenOpen = true
		case t.kind == tkPunct && t.text == ")":
			depth--
			if seenOpen && depth == 0 {
				tail := toks[i+1:]
				for j := 0; j < len(tail); j++ {
					switch {
					case tail[j].kind == tkIdent && tail[j].upper() == "WITHOUT":
						if j+1 >= len(tail) || tail[j+1].kind != tkIdent || tail[j+1].upper() != "ROWID" {
							return false, false, fmt.Errorf("engine: CREATE TABLE: unexpected trailing input near %q", tail[j].text)
						}
						withoutRowid = true
						j++
						if j+1 < len(tail) && tail[j+1].kind == tkPunct && tail[j+1].text == "," {
							j++
						}
					case tail[j].kind == tkIdent && !tail[j].quoted && tail[j].upper() == "STRICT":
						// A QUOTED spelling is NOT the option: C SQLite
						// rejects `CREATE TABLE s1 (a INT) "STRICT"` with
						// `unknown table option: "STRICT"` (verified
						// directly), so quoted must fall through to the
						// trailing-input rejection below rather than
						// silently enabling type enforcement.
						strict = true
						if j+1 < len(tail) && tail[j+1].kind == tkPunct && tail[j+1].text == "," {
							j++
						}
					case tail[j].kind == tkPunct && tail[j].text == ";":
						continue
					case tail[j].kind == tkEOF:
						return withoutRowid, strict, nil
					default:
						return false, false, fmt.Errorf("engine: CREATE TABLE: unexpected trailing input near %q (multi-statement Exec is not supported by this write path)", tail[j].text)
					}
				}
				return withoutRowid, strict, nil
			}
		}
	}
	return withoutRowid, strict, nil
}

// tailDeclaresStrict reports whether tail, the tokens after a CREATE TABLE's
// top-level ")", carry STRICT. It is parseTableTailClauses' tolerant sibling
// for parseCreateTableColumnsAndAutoIndexes, which runs on the read path too
// (resolveTableIn, alter re-derivations) where the tail was already accepted;
// sqlTextTableIsWithoutRowid is the same split for WITHOUT ROWID. Quoted
// spellings do not match, and the scan stops at the first ";".
func tailDeclaresStrict(tail []token) bool {
	for _, t := range tail {
		switch {
		case t.kind == tkEOF:
			return false
		case t.kind == tkPunct && t.text == ";":
			return false
		case t.kind == tkIdent && !t.quoted && t.upper() == "STRICT":
			return true
		}
	}
	return false
}

// ---- CHECK constraint parsing/validation (write path only) ----
//
// parseCreateTableColumnsAndAutoIndexes recovers each CHECK's raw text and
// owning column (shared with the read path; see checkConstraint) but never
// parses or judges it. parseCheckExprText parses that text with the ordinary
// expression grammar, and finalizeCheckConstraints confirms C would accept it
// and that it can be enforced exactly, declining anything else. Called from
// CreateTable and OpenWrite's catalog load.

// parseCheckExprText parses a CHECK constraint's own parenthesized body
// (exprText, already extracted verbatim by parseCheckClauseStructural) as a
// single, stand-alone expression.
func parseCheckExprText(exprText string) (Expr, error) {
	toks, err := lex(exprText)
	if err != nil {
		return nil, err
	}
	p := newParser(exprText, toks)
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	return expr, nil
}

// r32nParseSpannedExpr is parseCheckExprText with double-quoted-span recording
// on and every span shifted by base -- the byte offset of exprText's first
// byte within the CREATE TABLE it was sliced out of (checkConstraint's
// r32nBodyOff / columnInfo.R32NGeneratedOff). The renameFixQuotes port
// (alter_write.go) needs offsets into the WHOLE stored text, since that is
// what it splices.
func r32nParseSpannedExpr(exprText string, base int) (Expr, error) {
	toks, err := lex(exprText)
	if err != nil {
		return nil, err
	}
	p := newParser(exprText, toks)
	p.r32nDQRecord = true
	p.r32nDQBase = base
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("engine: unexpected trailing input near %q", p.tokenDesc(p.peek()))
	}
	return expr, nil
}

// finalizeCheckConstraints parses every one of checks' raw exprText into an
// expression AST and validates it, returning a NEW slice (checks itself is
// never mutated) with every entry's .expr populated, or the first
// unsupported/invalid CHECK's own error. tblName/cols are the table's own
// name and FINAL column list (every column already parsed, including any
// declared AFTER a forward-referencing CHECK -- verified directly: "CREATE
// TABLE t(a INT CHECK(b>0), b INT)" is accepted by C SQLite, so this
// cannot validate column references until the whole column list is known,
// which is exactly why this is a separate pass over CreateTable's already-
// complete cols, not folded into the per-segment loop that discovers each
// checkConstraint in the first place).

// emptyColumnListErr is C's own diagnosis for a CREATE TABLE whose column list
// contains no column definition: its grammar requires at least one, so the
// parser fails on the first token INSIDE the parens and parse.y:44-51's
// %syntax_error action reports `near "X": syntax error` for it. "CREATE TABLE
// x()" is near ")", where this engine said "no columns" -- a true statement
// about the table and not a message C has any counterpart for.
func emptyColumnListErr(createSQL string) error {
	if toks, lerr := lex(createSQL); lerr == nil {
		for i, t := range toks {
			if t.kind != tkPunct || t.text != "(" {
				continue
			}
			if i+1 < len(toks) && toks[i+1].kind != tkEOF {
				return fmt.Errorf("engine: near %q: syntax error", toks[i+1].text)
			}
			break
		}
	}
	return errors.New("engine: incomplete input")
}

func finalizeCheckConstraints(tblName string, cols []columnInfo, checks []checkConstraint, withoutRowid bool) ([]checkConstraint, error) {
	if len(checks) == 0 {
		return checks, nil
	}
	// One scope serves both validation and compilation, so they cannot
	// disagree about a bare name. Splitting them (validation forcing
	// noRowid=false) accepted "CREATE TABLE b2(x INT PRIMARY KEY,
	// CHECK(rowid<100)) WITHOUT ROWID", which C rejects with "no such
	// column: rowid" (VisibleRowid, sqliteInt.h:2552; TF_NoVisibleRowid,
	// build.c:2731; resolve.c:564-568).
	checkScope := checkProgramScope(tblName, cols, withoutRowid)
	// checkConstraintTable lets a three-part "main.t.c" / "xyzzy.t.c" reference
	// resolve: inside a CHECK the schema part is ignored entirely (see evalCtx).
	schemaCtx := &evalCtx{tables: []tableScope{checkScope}, checkConstraintTable: tblName}
	out := make([]checkConstraint, len(checks))
	for i, cc := range checks {
		expr, perr := parseCheckExprText(cc.exprText)
		if perr != nil {
			return nil, fmt.Errorf("engine: CREATE TABLE %s: CHECK(%s): %w", tblName, cc.exprText, perr)
		}
		if err := validateCheckExpr(tblName, schemaCtx, expr); err != nil {
			return nil, err
		}
		if fn, unsafe := exprCallsUnsafeSchemaFunc(expr); unsafe {
			return nil, fmt.Errorf("%w: CREATE TABLE %s: CHECK(%s) calls %s(), which a schema object may not use unless it is TEMP and trusted (see exprCallsUnsafeSchemaFunc)", errVDBEUnsupported, tblName, cc.exprText, fn)
		}
		cc.expr = expr
		// COMPILE it here, once, against the finished column list -- the same
		// register-backed row scope emitCheckConstraintsAction (vdbe_write.go)
		// pushes for the compiled INSERT/UPDATE path, and the same one C SQLite
		// codes a CHECK under (pParse->iSelfTab = -(regNewData+1),
		// insert.c:2066, then sqlite3ExprIfTrue at insert.c:2087). This is what
		// lets checkTableChecksChanged run opcodes rather than walk the tree.
		cc.prog = compileSelfRowExpr(checkScope, expr, true /* NC_IsCheck: resolve.c:316 drops the db qualifier */, pureCtxCheck)
		out[i] = cc
	}
	return out, nil
}

// checkProgramScope is the row-in-registers scope a CHECK body is resolved and
// compiled against, for table tblName with columns cols, shared by
// finalizeCheckConstraints and refreshRowPrograms (alter_write.go). Both build
// it once per table.
//
// withoutRowid is C's VisibleRowid (sqliteInt.h:2552): a WITHOUT ROWID table
// has TF_NoVisibleRowid (build.c:2731) and lookupName refuses "rowid"
// (resolve.c:564-568), while a rowid table resolves it to column -1:
//
//	CREATE TABLE a(x INT, CHECK(rowid<100))                     accepted
//	  INSERT INTO a(rowid,x) VALUES(500,3)  CHECK constraint failed: rowid<100
//	CREATE TABLE q(x INTEGER PRIMARY KEY, CHECK(rowid<100))     accepted
//	  UPDATE q SET x=900 ...                CHECK constraint failed: rowid<100
//	CREATE TABLE b2(x INT PRIMARY KEY, CHECK(rowid<100)) WITHOUT ROWID
//	                                        no such column: rowid
//
// An INTEGER PRIMARY KEY column reference reads its own register, since
// normalizeRow puts the rowid in that slot.
func checkProgramScope(tblName string, cols []columnInfo, withoutRowid bool) tableScope {
	return tableScope{name: tblName, cols: cols, colIndex: buildColIndex(cols), noRowid: withoutRowid}
}

// validateCheckExpr confirms e is a CHECK this write path can enforce exactly:
//
//   - no subquery ("subqueries prohibited in CHECK constraints" in C);
//   - no aggregate call ("misuse of aggregate function");
//   - qualified column references only to this table itself
//     (checkNoQualifiedColumnRef);
//   - every column reference names one of this table's columns or its rowid
//     pseudo-column, else C's CREATE-time "no such column: x";
//   - no bound parameter (CREATE text has no bind context; it would be NULL
//     forever);
//   - every function/operator is one checkExprSupported (expr_supported.go)
//     can lower, as for any WHERE/SET expression.
func validateCheckExpr(tblName string, schemaCtx *evalCtx, e Expr) error {
	if containsSubquery(e) {
		// resolve.c:923's "%s prohibited in %s", whose zIn for NC_IsCheck is
		// "CHECK constraints" (:918).
		return fmt.Errorf("engine: CREATE TABLE %s: subqueries prohibited in CHECK constraints", tblName)
	}
	if containsAggregate(e) {
		return fmt.Errorf("engine: CREATE TABLE %s: aggregate functions are not supported in a CHECK constraint by this write path", tblName)
	}
	if err := checkNoQualifiedColumnRef(tblName, e); err != nil {
		return err
	}
	if err := validateColumnRefs(e, schemaCtx); err != nil {
		return err
	}
	return checkExprSupported(e)
}

// checkNoQualifiedColumnRef rejects a bound parameter anywhere in e (see
// validateCheckExpr) and a qualified column reference naming a table other
// than the one being created. A self-qualified reference is legal and common:
//
//	CREATE TABLE t3(x,y,z, CHECK( t3.x<25 ))
//	  INSERT (99,2,3)   CHECK constraint failed: t3.x<25
//	CREATE TABLE t810(a, CHECK( main.t810.a>0 ))
//	  INSERT (-5)       CHECK constraint failed: main.t810.a>0
//	CREATE TABLE t811(b, CHECK( xyzzy.t811.b BETWEEN 5 AND 10 ))
//	  INSERT (1)        CHECK constraint failed: xyzzy.t811.b BETWEEN 5 AND 10
//	CREATE TABLE f(true INT, false INT, x INT CHECK (5 IN (f.false)))
//	  INSERT (1,9,3)    CHECK constraint failed: 5 IN (f.false)
//
// so the schema part of a three-part reference is ignored entirely (looser than
// qualifierResolvesLocally). Another table's qualifier is "no such column:
// other.c" at CREATE. Mirrors checkExprSupported's traversal.
func checkNoQualifiedColumnRef(tblName string, e Expr) error {
	switch x := e.(type) {
	case nil, LiteralExpr:
		return nil
	case ParamExpr:
		return fmt.Errorf("engine: bound parameters are not supported in a CHECK constraint by this write path")
	case ColumnExpr:
		if x.Qualifier != "" && !equalFoldName(x.Qualifier, tblName) {
			return fmt.Errorf("engine: no such column: %s.%s", x.Qualifier, x.Name)
		}
		return nil
	case FuncExpr:
		return checkNoQualifiedColumnRefAll(tblName, x.walkArgs()...)
	case UnaryExpr:
		return checkNoQualifiedColumnRef(tblName, x.X)
	case BinaryExpr:
		return checkNoQualifiedColumnRefAll(tblName, x.L, x.R)
	case IsNullExpr:
		return checkNoQualifiedColumnRef(tblName, x.X)
	case InExpr:
		if err := checkNoQualifiedColumnRef(tblName, x.X); err != nil {
			return err
		}
		return checkNoQualifiedColumnRefAll(tblName, x.List...)
	case BetweenExpr:
		return checkNoQualifiedColumnRefAll(tblName, x.X, x.Lo, x.Hi)
	case LikeExpr:
		return checkNoQualifiedColumnRefAll(tblName, x.X, x.Pattern)
	case GlobExpr:
		return checkNoQualifiedColumnRefAll(tblName, x.X, x.Pattern)
	case CollateExpr:
		return checkNoQualifiedColumnRef(tblName, x.X)
	case CastExpr:
		return checkNoQualifiedColumnRef(tblName, x.X)
	case CaseExpr:
		if x.Base != nil {
			if err := checkNoQualifiedColumnRef(tblName, x.Base); err != nil {
				return err
			}
		}
		for _, w := range x.Whens {
			if err := checkNoQualifiedColumnRef(tblName, w.When); err != nil {
				return err
			}
			if err := checkNoQualifiedColumnRef(tblName, w.Then); err != nil {
				return err
			}
		}
		if x.Else != nil {
			return checkNoQualifiedColumnRef(tblName, x.Else)
		}
		return nil
	default:
		return nil
	}
}

func checkNoQualifiedColumnRefAll(tblName string, es ...Expr) error {
	for _, e := range es {
		if err := checkNoQualifiedColumnRef(tblName, e); err != nil {
			return err
		}
	}
	return nil
}
