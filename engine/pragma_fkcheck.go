// This file implements PRAGMA [schema.]foreign_key_check[(<table>)] -- both
// the single-table form and the unqualified whole-database one. See
// pragmaForeignKeyCheck for the semantics (every one verified directly against
// mattn/go-sqlite3) and fkCheckWholeDatabaseRows for why the whole-database
// form's unreproducible internal visiting order does not make its ROW SET a
// guess, only (in one narrow case) its error TEXT.
package engine

import (
	"fmt"
	"strings"
)

// fkCheckPlan is one foreign key of the table being checked, resolved against
// its parent, in fkid order (see pragmaForeignKeyCheck).
type fkCheckPlan struct {
	// parent is the referenced table name AS WRITTEN in the child's CREATE
	// TABLE text, which is what the pragma's own "parent" column reports --
	// not the parent's own schema spelling. Verified directly against
	// mattn/go-sqlite3: "CREATE TABLE Chi(x REFERENCES PAR(ID))" against a
	// table actually named "Par" reports parent='PAR', while the "table"
	// column reports the SCHEMA's spelling ('Chi') whatever case the pragma's
	// own argument used.
	parent   string
	childIdx []int // index into the child's own column list, one per FK column

	// missingParent: the referenced table does not exist at all. That is NOT
	// an error (unlike every other way a foreign key can fail to resolve) --
	// C SQLite reports EVERY fully-non-NULL child row as a violation of
	// it, naming the absent table in the "parent" column. Verified directly.
	missingParent bool

	// parentRows holds the parent key tuple of every parent row, and
	// affs/colls the parent key COLUMNS' affinity and collating sequence.
	// All three are empty when missingParent.
	parentRows [][]Value
	affs       []affinity
	colls      []string
}

// pragmaForeignKeyCheck implements the SINGLE-TABLE form of
// PRAGMA [schema.]foreign_key_check(<table>), which reports one row --
// (table, rowid, parent, fkid) -- per foreign-key violation, ROW-MAJOR:
// ordered by child rowid, and within one rowid by fkid ascending. fkid is the
// same id PRAGMA foreign_key_list reports, i.e. REVERSE declaration order (see
// pragmaForeignKeyList). Every rule below was verified directly against
// mattn/go-sqlite3, several of them surprising:
//
//   - It runs regardless of "PRAGMA foreign_keys" (which this engine always
//     reports as 0): the pragma is a scan, not the enforcement machinery.
//   - A child row violates a foreign key only when EVERY one of its FK
//     columns is non-NULL and no parent row matches -- MATCH SIMPLE, the only
//     match mode SQLite implements. A composite key with ANY NULL column is
//     satisfied vacuously.
//   - A MISSING PARENT TABLE is not an error; see fkCheckPlan.missingParent.
//   - Parent columns that are NOT uniquely indexed IS an error ("foreign key
//     mismatch"), and it poisons the WHOLE statement -- a table with one good
//     and one mismatched foreign key reports no rows at all, not the good
//     one's violations.
//   - No explicit parent column list ("x REFERENCES p") targets the parent's
//     PRIMARY KEY; a parent with no PRIMARY KEY at all is a mismatch.
//   - A WITHOUT ROWID child reports rowid as NULL (its rows still come back
//     in the table's own b-tree, i.e. PRIMARY KEY, order).
//   - A name that resolves to a VIEW, or to sqlite_master/sqlite_schema,
//     answers ZERO rows; a name that resolves to nothing at all is an error.
//     A table with no foreign keys answers zero rows.
//
// The UNQUALIFIED form (no table named) checks every table in ONE catalog.
// Its ROW ORDER is C SQLite's internal schema HASH-TABLE iteration order,
// which is not reproducible here (five tables created mmm,aaa,zzz,bbb,kkk
// report as bbb,zzz,kkk,mmm,aaa -- verified directly). That order is NOT the
// same thing as the row SET being a guess, though: see
// fkCheckWholeDatabaseRows for why this engine's own (different) visiting
// order still produces the identical set of rows, just possibly grouped by
// table in a different sequence -- which is exactly what an explicit
// "ORDER BY ...table..." on the caller's own SELECT (the only shape this is
// wrapped for -- vtab_pragma.go) irons back out. Its EXEC form (a bare
// unqualified "PRAGMA foreign_key_check") is served too, because an Exec
// discards a pragma's rows in C SQLite anyway and what is left is the
// accept/decline decision plus the error: see fkCheckWholeDatabase.
func (p *ReadOnlyPager) pragmaForeignKeyCheck(scope schemaScope, stmt *PragmaStmt) (cols []string, rows [][]Value, err error) {
	cols = []string{"table", "rowid", "parent", "fkid"}
	if !stmt.HasValue {
		// The whole-database form. Its OWN row order is not a guess -- see
		// fkCheckWholeDatabaseRows -- which is what makes this servable as a
		// QUERY now (vtab_pragma.go's pragma_foreign_key_check wrapper,
		// fkey5.test 13.12: "SELECT *, '|' FROM pragma_foreign_key_check AS x
		// ORDER BY x.\"table\"").
		return p.fkCheckWholeDatabaseRows(scope)
	}
	// An UNQUALIFIED single-table argument resolves across every attached
	// database, main first -- the same pragmaObjectOwner rule
	// table_info/index_list/foreign_key_list already apply (attach_write.go),
	// which foreign_key_check needs too: fkey5.test 13.0's
	// "SELECT *, 'x' FROM pragma_foreign_key_check('t1')" names a table that
	// exists ONLY in an attached "aux", not main.
	if stmt.Schema == "" {
		if owner := p.pragmaObjectOwner(stmt.ValueText); owner != nil {
			return owner.pragmaForeignKeyCheckTable(scopeAny, stmt.ValueText)
		}
	}
	return p.pragmaForeignKeyCheckTable(scope, stmt.ValueText)
}

// fkCheckWholeDatabase runs the UNQUALIFIED "PRAGMA [schema.]foreign_key_check"
// -- every table of ONE catalog -- and returns only its ERROR, which is all an
// Exec of a row-returning pragma keeps (execPragma's single-table case makes
// the identical split, and for the identical reason).
//
// Every rule below was verified directly against mattn/go-sqlite3 3.53.3:
//
//   - The unqualified form checks MAIN ONLY. With a violating main table and a
//     violating TEMP table both present it reports the main one alone, and
//     "PRAGMA temp.foreign_key_check" reports the temp one alone; an ATTACHed
//     database likewise answers only for itself. (An attached qualifier never
//     reaches here -- attach_write.go routes it to that database's own session,
//     where it arrives unqualified.)
//   - VIEWs and VIRTUAL TABLEs are skipped, exactly as the single-table form
//     answers zero rows for them. An fts shadow table is an ordinary table with
//     no foreign keys, so it contributes nothing either.
//   - A "foreign key mismatch" ANYWHERE aborts the WHOLE pragma -- with
//     "bad(x REFERENCES p(a))" beside a perfectly good child, the bare pragma
//     reports the mismatch and none of the good child's violations.
//
// The one thing the hash order costs: with TWO mismatching tables, WHICH
// mismatch is reported depends on which is visited first. So this DECLINES when
// the tables disagree on the error text, and answers when they cannot (one
// distinct message, or none). A decline is mirrored onto both engines by the
// corpus harness and can never cost correctness.
func (p *ReadOnlyPager) fkCheckWholeDatabase(scope schemaScope) error {
	_, _, err := p.fkCheckWholeDatabaseRows(scope)
	return err
}

// fkCheckWholeDatabaseRows is fkCheckWholeDatabase's ROW-returning form -- the
// same scan (MAIN only once unqualified, views/virtual tables skipped, a
// "foreign key mismatch" ANYWHERE aborting the whole pragma, and the identical
// decline when two mismatched tables disagree on their error text), but
// collecting every table's violation rows instead of discarding them. This is
// what makes pragma_foreign_key_check's own bare/whole-database form servable
// as a QUERY, not just an Exec (pragmaForeignKeyCheck, vtab_pragma.go).
//
// The scan order here is this engine's OWN schema order, not C SQLite's
// internal hash-table iteration -- but unlike the single "which error wins"
// question above, that never makes the ROW SET wrong: each table's own
// violations already come back in a fixed (rowid, fkid) order
// (pragmaForeignKeyCheckTable), so visiting tables in a different sequence only
// ever reorders whole per-table BLOCKS relative to each other, never reshuffles
// rows within one. The differential corpus compares order-insensitively unless
// the SQL itself sorts, and a stable sort by "table" undoes exactly
// that block-level reordering while leaving each table's own internal order
// untouched -- which is why every gate for this shape (fkey5.test 13.12,
// compat-harness/pragma_fkcheck_test.go) pins the ORDER BY "table" spelling.
func (p *ReadOnlyPager) fkCheckWholeDatabaseRows(scope schemaScope) (cols []string, rows [][]Value, err error) {
	cols = []string{"table", "rowid", "parent", "fkid"}
	if scope == scopeAny {
		scope = scopeMain // the unqualified form is MAIN only -- see fkCheckWholeDatabase's doc comment
	}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	rows = [][]Value{}
	var mismatch error
	for i := range schemaRows {
		r := &schemaRows[i]
		if r.Type != "table" || !scope.accepts(r.Temp) || isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		_, trows, terr := p.pragmaForeignKeyCheckTable(scope, r.Name)
		if terr != nil {
			if mismatch != nil && mismatch.Error() != terr.Error() {
				return nil, nil, fmt.Errorf("%w: PRAGMA foreign_key_check with no table named: two tables report a different error (%v / %v) and which one C SQLite reports depends on its schema hash-table visit order", errVDBEUnsupported, mismatch, terr)
			}
			mismatch = terr
			continue
		}
		if mismatch == nil {
			rows = append(rows, trows...)
		}
	}
	if mismatch != nil {
		return nil, nil, mismatch
	}
	return cols, rows, nil
}

// pragmaForeignKeyCheckTable is the single-table check over an already-resolved
// table NAME -- see pragmaForeignKeyCheck for every rule it implements.
func (p *ReadOnlyPager) pragmaForeignKeyCheckTable(scope schemaScope, tableName string) (cols []string, rows [][]Value, err error) {
	cols = []string{"table", "rowid", "parent", "fkid"}
	rows = [][]Value{}
	schemaRows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	// The schema catalog has no foreign keys and no sqlite_schema row
	// describing itself (see resolveTableIn), so it would otherwise fall into
	// the "no such table" error below; C SQLite answers zero rows for it.
	if isMainSchemaCatalogName(tableName) || isTempSchemaCatalogName(tableName) {
		return cols, rows, nil
	}
	tr := findSchemaRowIn(schemaRows, scope, "table", tableName)
	if tr == nil {
		if findSchemaRowIn(schemaRows, scope, "view", tableName) != nil {
			return cols, rows, nil
		}
		return nil, nil, fmt.Errorf("engine: no such table: %s", tableName)
	}
	// A VIRTUAL table has no foreign keys at all, and no CREATE TABLE text
	// parsePragmaTableDef could parse -- C SQLite answers zero rows for
	// one, verified directly against "CREATE VIRTUAL TABLE ft USING
	// fts4(a,b)". Its SHADOW tables (ft_content, ...) are ordinary tables and
	// fall through to the normal path, which likewise answers zero rows for
	// them.
	if isCreateVirtualTableSQL(tr.SQL) {
		return cols, rows, nil
	}
	// See pragmaIndexList's identical guard: with a temp AND a main table of
	// this name, an index row's tbl_name no longer says which of the two it
	// belongs to, so the parent-side uniqueness search below could attribute
	// the other catalog's index here -- turning a genuine "foreign key
	// mismatch" into silently-reported rows.
	if findSchemaRowIn(schemaRows, scopeMain, "table", tableName) != nil &&
		findSchemaRowIn(schemaRows, scopeTemp, "table", tableName) != nil {
		return nil, nil, fmt.Errorf("engine: unsupported: PRAGMA foreign_key_check(%s): a temp and a main table share this name, and an index row records only its tbl_name -- which of the two it belongs to is not recoverable", tableName)
	}
	def, err := parsePragmaTableDef(tr.SQL)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA foreign_key_check: %w", err)
	}
	if len(def.fks) == 0 {
		return cols, rows, nil
	}
	plans, err := p.fkCheckPlans(schemaRows, tr, def)
	if err != nil {
		return nil, nil, err
	}
	rowidVals, childRows, err := p.fkCheckChildRows(tr, def)
	if err != nil {
		return nil, nil, err
	}
	// ponytail: O(child rows x parent rows x foreign keys) -- a full parent
	// scan per candidate, not the index SEEK C SQLite performs. Fine for
	// the fixtures this pragma is exercised on; the upgrade path is
	// index_seek_candidates.go's secondaryIndexSeekCandidates, which already
	// resolves an equality probe against a unique index (with the rowid-alias
	// case going through a rowid seek instead) -- the
	// parent key here is by construction exactly such an index, so plugging it
	// in only needs the index NAME threaded through fkCheckParentKey.
	key := make([]Value, 0, 4)
	for ri, crow := range childRows {
		for fkid, pl := range plans {
			key = key[:0]
			allSet := true
			for _, ci := range pl.childIdx {
				v := crow[ci]
				if v.Typ == Null {
					allSet = false
					break
				}
				key = append(key, v)
			}
			if !allSet || (!pl.missingParent && fkCheckParentHas(pl, key, p.encoding())) {
				continue
			}
			rows = append(rows, []Value{
				{Typ: Text, S: []byte(tr.Name)},
				rowidVals[ri],
				{Typ: Text, S: []byte(pl.parent)},
				{Typ: Int, I: int64(fkid)},
			})
		}
	}
	return cols, rows, nil
}

// fkCheckParentHas reports whether any parent row's key tuple equals the
// child's key.
//
// This is deliberately NOT expressed as generated SQL ("NOT EXISTS (SELECT 1
// FROM parent WHERE p.k = c.v)"): C SQLite performs an INDEX SEEK, which
// applies the PARENT column's affinity to the child value before comparing,
// and neither "=" (which uses the two operands' COMBINED comparison affinity)
// nor IN (which uses the LEFT operand's) does that. Verified directly: with a
// parent "k TEXT" holding the TEXT '1' and a child "v" (no declared type, so
// no affinity) holding the INTEGER 1, foreign_key_check reports NO violation,
// while "p.k = c.v" and "v IN (SELECT k FROM p)" are both 0 -- in BOTH
// engines. CAST is not affinity either. So the comparison runs here, in Go,
// over already-fetched values, exactly as inSubMembership (vdbe.go) does for
// its own already-materialized set: applyAffinityToValue with the parent
// column's affinity, then compareValuesCollatedEnc under the parent column's
// collating sequence.
//
// Only the CHILD side takes the affinity: the parent's stored values already
// had that same affinity applied when they were written, which is precisely
// why the index entries a real seek probes are already in that form.
func fkCheckParentHas(pl fkCheckPlan, key []Value, enc TextEncoding) bool {
	for _, prow := range pl.parentRows {
		match := true
		for i, v := range key {
			if compareValuesCollatedEnc(applyAffinityToValue(v, pl.affs[i]), prow[i], pl.colls[i], enc) != 0 {
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

// fkCheckPlans resolves every foreign key of def against its parent, in fkid
// (reverse declaration) order. Any resolution failure other than a wholly
// absent parent table is a "foreign key mismatch" error for the WHOLE
// statement -- see pragmaForeignKeyCheck.
func (p *ReadOnlyPager) fkCheckPlans(schemaRows []SchemaRow, tr *SchemaRow, def *pragmaTableDef) ([]fkCheckPlan, error) {
	n := len(def.fks)
	plans := make([]fkCheckPlan, 0, n)
	for declIdx := n - 1; declIdx >= 0; declIdx-- {
		fk := def.fks[declIdx]
		pl := fkCheckPlan{parent: fk.toTable}
		for _, fromCol := range fk.fromCols {
			ci := pragmaColumnIndex(def.cols, fromCol)
			if ci < 0 {
				return nil, fkCheckMismatch(tr.Name, fk.toTable)
			}
			pl.childIdx = append(pl.childIdx, ci)
		}
		// A foreign key's parent is resolved in the CHILD's own catalog.
		pscope := createScope(tr.Temp)
		pr := findSchemaRowIn(schemaRows, pscope, "table", fk.toTable)
		if pr == nil {
			// A VIEW of that name is a mismatch (it has no unique index to
			// seek), not a missing table -- verified directly.
			if findSchemaRowIn(schemaRows, pscope, "view", fk.toTable) != nil {
				return nil, fkCheckMismatch(tr.Name, fk.toTable)
			}
			pl.missingParent = true
			plans = append(plans, pl)
			continue
		}
		pdef, err := parsePragmaTableDef(pr.SQL)
		if err != nil {
			return nil, fmt.Errorf("engine: PRAGMA foreign_key_check: %w", err)
		}
		keyNames, err := fkCheckParentKey(schemaRows, tr, pr, pdef, fk)
		if err != nil {
			return nil, err
		}
		for _, kn := range keyNames {
			pc := pdef.cols[pragmaColumnIndex(pdef.cols, kn)] // fkCheckParentKey proved it resolves
			pl.affs = append(pl.affs, typeAffinity(pc.declType))
			pl.colls = append(pl.colls, fkCheckCollation(pc.collate))
		}
		if pl.parentRows, err = p.fkCheckFetch(pr.Name, keyNames); err != nil {
			return nil, err
		}
		plans = append(plans, pl)
	}
	return plans, nil
}

// fkCheckParentKey resolves the parent-side COLUMNS one foreign key seeks on
// and proves they are actually seekable -- C SQLite's own
// sqlite3FkLocateIndex rule, each clause verified directly:
//
//   - An omitted parent column list targets the parent's PRIMARY KEY (its
//     rowid-alias column when it has one); a parent with none is a mismatch.
//   - A single-column key naming the parent's rowid alias needs no index at
//     all -- the table's own b-tree is the seek target. This is why a TEXT
//     '1' child value satisfies a reference to rowid 1 (INTEGER affinity)
//     while '1abc' does not.
//   - Otherwise SOME UNIQUE, non-partial index must cover exactly that SET of
//     columns (order-insensitively: "UNIQUE(a,b)" serves "REFERENCES
//     po(b,a)"), and each of its key columns' collating sequence must equal
//     that column's own declared default (a NOCASE index over a BINARY column
//     is a mismatch even though it is unique).
//   - The child and parent key column counts must agree.
func fkCheckParentKey(schemaRows []SchemaRow, tr, pr *SchemaRow, pdef *pragmaTableDef, fk pragmaFK) ([]string, error) {
	keyNames := fk.toCols
	if keyNames == nil {
		if pdef.rowidAlias != "" {
			keyNames = []string{pdef.rowidAlias}
		} else {
			keyNames = pragmaPrimaryKeyColumns(pdef)
		}
	}
	if len(keyNames) == 0 || len(keyNames) != len(fk.fromCols) {
		return nil, fkCheckMismatch(tr.Name, fk.toTable)
	}
	for _, kn := range keyNames {
		if pragmaColumnIndex(pdef.cols, kn) < 0 {
			return nil, fkCheckMismatch(tr.Name, fk.toTable)
		}
	}
	if len(keyNames) == 1 && pdef.rowidAlias != "" && equalFoldName(keyNames[0], pdef.rowidAlias) {
		return keyNames, nil
	}
	if fkCheckUniquelyIndexed(schemaRows, pr, pdef, keyNames) {
		return keyNames, nil
	}
	return nil, fkCheckMismatch(tr.Name, fk.toTable)
}

// fkCheckUniquelyIndexed reports whether keyNames is covered, as a set, by a
// UNIQUE non-partial index of pr whose per-column collations match those
// columns' own declared defaults -- see fkCheckParentKey.
func fkCheckUniquelyIndexed(schemaRows []SchemaRow, pr *SchemaRow, pdef *pragmaTableDef, keyNames []string) bool {
	// An AUTOMATIC index (a UNIQUE/PRIMARY KEY constraint's own) always keys
	// on its columns' declared collations, so it never needs the collation
	// check below. A WITHOUT ROWID table's PRIMARY KEY is one of these too,
	// even though no separate b-tree backs it (parsePragmaTableDef's doc
	// comment).
	for _, spec := range pdef.autoIdx {
		if sameColumnSet(spec.cols, keyNames) {
			return true
		}
	}
	for _, r := range schemaRows {
		if r.Type != "index" || !equalFoldName(r.TblName, pr.Name) || r.Temp != pr.Temp || r.SQL == "" {
			continue
		}
		names, _, colls, unique, err := parsePragmaIndexColumns(r.SQL)
		if err != nil || !unique || !sameColumnSet(names, keyNames) {
			continue
		}
		if _, partial := parsePragmaIndexHeader(r.SQL); partial {
			continue
		}
		ok := true
		for i, nm := range names {
			ci := pragmaColumnIndex(pdef.cols, nm)
			if ci < 0 || (colls[i] != "" && !equalFoldName(colls[i], fkCheckCollation(pdef.cols[ci].collate))) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// fkCheckChildRows reads every row of the table being checked, returning the
// pragma's own "rowid" cell alongside (NULL for a WITHOUT ROWID table, which
// has none -- verified directly) and the row's full column list, positionally
// matching def.cols.
//
// The rows come back through this engine's ordinary read path (QueryArgs), so
// a WITHOUT ROWID table is scanned in its own PRIMARY KEY order and an
// ordinary table in rowid order -- exactly the order the pragma's own
// row-major output uses. (Only the FETCH goes through SQL; the comparison
// deliberately does not -- see fkCheckParentHas.)
func (p *ReadOnlyPager) fkCheckChildRows(tr *SchemaRow, def *pragmaTableDef) (rowidVals []Value, rows [][]Value, err error) {
	withoutRowid := sqlTextTableIsWithoutRowid(tr.SQL)
	var sb strings.Builder
	sb.WriteString("SELECT ")
	if !withoutRowid {
		alias := fkCheckRowidAlias(def.cols)
		if alias == "" {
			return nil, nil, fmt.Errorf("engine: unsupported: PRAGMA foreign_key_check(%s): every rowid spelling (rowid, _rowid_, oid) is shadowed by a declared column of this table, so its rowid is not selectable", tr.Name)
		}
		sb.WriteString(alias)
		sb.WriteString(", ")
	}
	for i, c := range def.cols {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(quoteIdent(c.name))
	}
	sb.WriteString(" FROM ")
	sb.WriteString(quoteIdent(tr.Name))
	_, raw, err := p.QueryArgs(sb.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	rowidVals = make([]Value, 0, len(raw))
	rows = make([][]Value, 0, len(raw))
	for _, r := range raw {
		if withoutRowid {
			rowidVals = append(rowidVals, Value{Typ: Null})
			rows = append(rows, r)
			continue
		}
		rowidVals = append(rowidVals, r[0])
		rows = append(rows, r[1:])
	}
	return rowidVals, rows, nil
}

// fkCheckFetch reads the named columns of every row of one table -- the
// parent-side key tuples fkCheckParentHas probes.
func (p *ReadOnlyPager) fkCheckFetch(tableName string, colNames []string) ([][]Value, error) {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	for i, c := range colNames {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(quoteIdent(c))
	}
	sb.WriteString(" FROM ")
	sb.WriteString(quoteIdent(tableName))
	_, rows, err := p.QueryArgs(sb.String(), nil)
	return rows, err
}

// fkCheckRowidAlias picks the rowid spelling that is NOT shadowed by a
// declared column of the table (a column literally named "rowid" shadows the
// pseudo-column, exactly as it does in any other SELECT -- yet C SQLite's
// foreign_key_check still reports the TRUE rowid there, verified directly),
// or "" when all three spellings are taken.
func fkCheckRowidAlias(cols []pragmaColumn) string {
	for _, cand := range []string{"rowid", "_rowid_", "oid"} {
		taken := false
		for _, c := range cols {
			if equalFoldName(c.name, cand) {
				taken = true
				break
			}
		}
		if !taken {
			return cand
		}
	}
	return ""
}

// fkCheckCollation renders a column's declared COLLATE name ("" for none) as
// the collating sequence compareValuesCollatedEnc wants.
func fkCheckCollation(declared string) string {
	if declared == "" {
		return "BINARY"
	}
	return declared
}

// fkCheckMismatch is C SQLite's own "foreign key mismatch" error, which
// aborts the WHOLE pragma rather than skipping the one unresolvable foreign
// key -- see pragmaForeignKeyCheck.
func fkCheckMismatch(child, parent string) error {
	return fmt.Errorf("engine: foreign key mismatch - %q referencing %q", child, parent)
}

// pragmaColumnIndex returns the position of the column named name in cols
// (case-insensitively), or -1.
func pragmaColumnIndex(cols []pragmaColumn, name string) int {
	for i := range cols {
		if equalFoldName(cols[i].name, name) {
			return i
		}
	}
	return -1
}

// pragmaPrimaryKeyColumns returns def's PRIMARY KEY column names in key
// order, or nil if it has none (or names a column the table doesn't have).
func pragmaPrimaryKeyColumns(def *pragmaTableDef) []string {
	out := make([]string, len(def.pkPos))
	for i := range def.cols {
		pos := def.pkPos[r33sFoldIdent(def.cols[i].name)]
		if pos < 1 || pos > len(out) {
			continue
		}
		out[pos-1] = def.cols[i].name
	}
	for _, n := range out {
		if n == "" {
			return nil
		}
	}
	return out
}

// sameColumnSet reports whether a and b name the same SET of columns
// (case-insensitively, order-insensitively) -- how C SQLite matches a
// candidate parent index against a foreign key's referenced columns.
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if equalFoldName(x, y) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
