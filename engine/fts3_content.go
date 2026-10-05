// This file implements FTS4's "content=" option. C fts3 implements it as one
// field:
//
//	p->zContentTbl == NULL   an ordinary table: the rows live in %_content
//	p->zContentTbl == ""     contentless: the rows live nowhere
//	p->zContentTbl == "xxx"  external content: the rows live in table xxx
//
// fts3ReadExprList composes "SELECT rowid, x.'a', x.'b' FROM 'main'.'xxx' AS x"
// for the last two and the %_content form for the first, and every read (plain
// scan, MATCH column lookup, 'rebuild', 'integrity-check') runs that. So a
// contentless table is an external-content table named "", which never
// exists and makes those reads fail with "SQL logic error". One rule here too.
//
// # What changes
//
//   - No %_content table (fts3CreateTables skips it).
//   - With no declared columns, the columns come from "SELECT * FROM %Q.%Q" on
//     the content table, so its INTEGER PRIMARY KEY is an fts column and a
//     missing content table fails the CREATE ("no such table: main.t7").
//     Declared columns are used verbatim and the content table is not consulted,
//     so "fts4(content=nosuchtable, x)" creates.
//   - The row source depends on the query. Without MATCH, rows are the content
//     table's; with one, they are the index's docids, and a docid the content
//     table lacks comes back with every column NULL (fts3CursorSeek raises
//     SQLITE_CORRUPT_VTAB only for an ordinary table).
//   - A write never touches the content table. INSERT needs an INTEGER docid
//     ("constraint failed", fts3InsertData) and writes only the index,
//     %_docsize and %_stat. DELETE/UPDATE are driven by the content table, so a
//     row it lacks is a no-op.
//   - fts3IsEmpty always answers false, so emptying the table does not throw
//     the shadow tables away (fts3_write.go): it leaves delete markers.
//   - DROP TABLE leaves "<name>_content" alone: fts3DestroyMethod's fifth
//     statement is prefixed "%s", substituted with "--".
//
// # Declined
//
//   - a content table that is not an ordinary table (a view or another vtab):
//     fts3ContentColumns takes result column names from a prepared SELECT, not
//     a schema lookup;
//   - a WITHOUT ROWID content table, which C fails too (see
//     fts3WithoutRowidContentErr), except for a content-unneeded MATCH;
//   - "languageid=" together with "content=" (see parseSchemaWith);
//   - a MATCH over an unreadable content table (contentless or missing) for a
//     statement that reads a real column -- C errors, and NULL columns would be
//     a wrong answer (see materializeFts3Content);
//   - a MATCH over a row source resolved without the statement's WHERE, when
//     index and content table disagree, since the WHERE is what identifies the
//     index-driven row source.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts3Catalog resolves the column names of a "content=" option's table, in
// declaration order -- fts3ContentColumns' "SELECT * FROM %Q.%Q" reduced to
// the one thing it is used for. Both sides of the engine can supply one (the
// write session's *DB and the read snapshot's *ReadOnlyPager), and a nil
// fts3Catalog means "no database here", which only an external-content table
// that declares no columns of its own ever needs.
type fts3Catalog func(table string) ([]string, error)

// hasContentOption reports whether args carry a "content=" option at all --
// parseSchemaWith's own per-argument rules reduced to that one question, so DROP
// TABLE can ask it without a catalog and without the rest of the schema having
// to still be resolvable (its content table may itself have been dropped).
func (m fts3Module) hasContentOption(args []string) bool {
	if !m.isFts4 {
		// fts3 has no module options: "fts3(content=t1)" declares a COLUMN
		// named content (parseSchemaWith).
		return false
	}
	haveTokenizer := false
	found := false
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !haveTokenizer && fts3IsTokenizerArg(a) {
			haveTokenizer = true
			continue
		}
		key, ok := fts3SpecialColumnKey(a)
		if !ok {
			continue
		}
		if strings.EqualFold(key, "content") {
			// Not "return true": a LATER argument cannot unset it, but the
			// loop still has to honour the tokenizer rule above for the ones
			// before it, and stopping early would skip that.
			found = true
		}
	}
	return found
}

// fts3ContentDependent names an fts4 table that would take its column list
// from the table called name, or "" if none. DropTable consults it.
//
// C caches a vtab's declared columns on the connection: a "content=t7" table
// derives them at its first xConnect, ALTER of t7 reconnects it, but DROP does
// not. After "DROP TABLE t7" the connection's ft7 keeps its old columns
// whatever t7 comes back as -- and recreated as t7(x,y) (fts4content.test 6.2)
// every read is "SQL logic error". This engine re-derives columns per
// statement, so it would answer from the new t7. Declining the DROP prevents
// that state; an fts4 table declaring its own columns is unaffected.
func (db *DB) fts3ContentDependent(name string) string {
	for _, vt := range db.vtabs {
		fm, isFts3 := fts3ModuleOf(vt)
		if !isFts3 || !fm.isFts4 {
			continue
		}
		sch, err := fm.parseSchemaWith(vt.args, func(string) ([]string, error) {
			// Enough to answer "would this table have had to derive?": a
			// one-column stand-in makes parseSchemaWith take the derivation
			// branch without needing the real table, which may be the very one
			// being dropped.
			return []string{"\x00probe"}, nil
		})
		if err != nil || !sch.hasContent || !strings.EqualFold(sch.content, name) {
			continue
		}
		if len(sch.cols) == 1 && sch.cols[0] == "\x00probe" {
			return vt.name
		}
	}
	return ""
}

// fts3ContentColumnsOf is fts3ContentColumns: the content table's own columns,
// or the error C SQLite fails the CREATE with when it has none to give.
func fts3ContentColumnsOf(cat fts3Catalog, table string) ([]string, error) {
	if cat == nil {
		return nil, fmt.Errorf("engine: fts4: a content=%s table that declares no columns of its own cannot be resolved here", table)
	}
	cols, err := cat(table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		// "SELECT * FROM x" over a table with no columns is not reachable in
		// SQL, so this is the empty-name case: "content=" alone is
		// "no such table: main." on the oracle, exactly as spelled here.
		return nil, fmt.Errorf("engine: no such table: main.%s", table)
	}
	return cols, nil
}

// fts3Catalog builds the write session's catalog. A content table that is not
// an ordinary table in this session is "no such table", which is the CREATE
// error C SQLite gives for a missing one -- see this file's decline list for
// the shapes that reach it for a different reason.
func (db *DB) fts3Catalog() fts3Catalog {
	if db == nil {
		return nil
	}
	return func(table string) ([]string, error) {
		tbl := db.findTableMeta(table)
		if tbl == nil {
			return nil, fmt.Errorf("engine: no such table: main.%s", table)
		}
		return columnInfoNames(tbl.cols), nil
	}
}

// fts3Catalog builds a read snapshot's catalog, the counterpart of the write
// session's above.
func (p *ReadOnlyPager) fts3Catalog() fts3Catalog {
	if p == nil {
		return nil
	}
	return func(table string) ([]string, error) {
		rt, err := p.resolveTable(table)
		if err != nil {
			return nil, err
		}
		return columnInfoNames(rt.cols), nil
	}
}

// columnInfoNames is the name list of cols, in order.
func columnInfoNames(cols []columnInfo) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// fts3ContentSource is one "content=" table's rows, resolved: docids in
// ascending order and, for each, the [docid, col0, ...] row the virtual table
// presents -- the same shape fts3ContentRows builds from %_content.
type fts3ContentSource struct {
	docids []int64
	rows   [][]Value
	byID   map[int64]int
}

// fts3ProjectContent turns a content table's (rowid, row) pairs into that shape
// by matching the fts table's declared column names against the content
// table's, as fts3ReadExprList's "x.'%q'" does: a declared column the content
// table lacks is an error ("SQL logic error"); extra columns are ignored.
//
// A "languageid=" table reads its language id the same way, from the content
// table's column of that name (already excluded from sch.cols), appended as
// the row's trailing slot where fts3RowLangid/fts3ContentRowWidth expect it.
//
// matchLangid overrides that column for a MATCH-driven read, unconditionally:
// fts3ColumnMethod's case 2 (fts3.c:3462-3499) checks pCsr->pExpr first and
// answers from the cursor's iLangid, never seeking content (fts3.c:3496-3497).
// So under MATCH the content's stored value is never consulted, even when it
// disagrees. A nil matchLangid (non-MATCH reads) reads content's own column.
func fts3ProjectContent(sch fts3Schema, contentCols []columnInfo, ipk int, rowids []uint64, records [][]Value, matchLangid *int64) (*fts3ContentSource, error) {
	pick := make([]int, len(sch.cols))
	for i, want := range sch.cols {
		pick[i] = -1
		for j, have := range contentCols {
			if strings.EqualFold(have.Name, want) {
				pick[i] = j
				break
			}
		}
		if pick[i] < 0 {
			return nil, fmt.Errorf("engine: %s table: its content table has no column %q", "fts4", want)
		}
	}
	langidPick := -1
	if sch.langid != "" {
		for j, have := range contentCols {
			if strings.EqualFold(have.Name, sch.langid) {
				langidPick = j
				break
			}
		}
		if langidPick < 0 && matchLangid == nil {
			return nil, fmt.Errorf("engine: %s table: its content table has no column %q", "fts4", sch.langid)
		}
	}
	width := len(sch.cols) + 1
	if sch.langid != "" {
		width++
	}
	order := make([]int, len(rowids))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return rowidLess(rowids[order[a]], rowids[order[b]]) })
	src := &fts3ContentSource{
		docids: make([]int64, len(order)),
		rows:   make([][]Value, len(order)),
		byID:   make(map[int64]int, len(order)),
	}
	for n, i := range order {
		rid := rowids[i]
		// An INTEGER PRIMARY KEY column stores NULL and reads back as the
		// rowid, which normalizeRow already knows how to do -- and an fts
		// table over "p1(id INTEGER PRIMARY KEY, ...)" really does read id
		// back as its docid (verified).
		full := normalizeRow("", contentCols, ipk, rid, records[i])
		row := make([]Value, width)
		row[0] = Value{Typ: Int, I: int64(rid)}
		for c, j := range pick {
			if j < len(full) {
				row[c+1] = full[j]
			}
		}
		if matchLangid != nil {
			// UNCONDITIONAL priority over content's own column, whether or not
			// content even has one -- fts3ColumnMethod's "if (pCsr->pExpr)"
			// fires before any content-seeking branch, per this function's own
			// doc comment.
			row[width-1] = Value{Typ: Int, I: *matchLangid}
		} else if langidPick >= 0 && langidPick < len(full) {
			row[width-1] = full[langidPick]
		}
		src.docids[n] = int64(rid)
		src.rows[n] = row
		src.byID[int64(rid)] = n
	}
	return src, nil
}

// fts3ContentSourceOf reads a "content=" table's rows through a read snapshot.
// A CONTENTLESS table (content="") has no such table by construction, so this
// is where its "SQL logic error" comes from. matchLangid is fts3ProjectContent's
// own parameter, threaded through unchanged.
func (p *ReadOnlyPager) fts3ContentSourceOf(sch fts3Schema, matchLangid *int64) (*fts3ContentSource, error) {
	rt, err := p.resolveTable(sch.content)
	if err != nil {
		return nil, err
	}
	if rt.withoutRowid {
		return nil, fts3WithoutRowidContentErr(sch.content)
	}
	rowids, records, err := p.RowsOfRoot(rt.root)
	if err != nil {
		return nil, err
	}
	return fts3ProjectContent(sch, rt.cols, rt.ipkIndex, rowids, records, matchLangid)
}

// fts3WithoutRowidContentErr is the error a WITHOUT ROWID content table gets.
// fts3ReadExprList's first output column is literally "rowid", which such a
// table does not have, so C SQLite fails EVERY read of the fts table with
// "SQL logic error" -- verified over "wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID":
// the CREATE and "PRAGMA table_info" both succeed and report a and b, while
// "SELECT * FROM fw", "SELECT rowid,* FROM fw" and 'rebuild' are each that
// error (a MATCH still returns its docids, the shape declined above). This
// engine's own row store keys such a table by an INTERNAL, never-exposed
// identifier (schema_write.go's tableMeta.withoutRowid), so handing that out
// as a docid would be a wrong answer rather than the error C fts3 gives.
func fts3WithoutRowidContentErr(name string) error {
	return fmt.Errorf("engine: fts4 content=%s: a WITHOUT ROWID content table has no rowid for fts3 to read its docids from", name)
}

// fts3ContentSourceIn reads a "content=" table's rows out of the write
// session, the counterpart of fts3ContentSourceOf above. DELETE, UPDATE and
// 'rebuild' all drive off this -- always with matchLangid nil, since none of
// them are a MATCH: C fts3's own 'rebuild' reads languageid off the
// content table unconditionally too (fts3ReadExprList's zContentTbl!=0
// branch), never from a cursor's iLangid.
//
// sch.content names an ORDINARY table whose row store may not be set up yet
// (table_load.go), so it is loaded before its rows are read.
func (db *DB) fts3ContentSourceIn(sch fts3Schema) (*fts3ContentSource, error) {
	tbl := db.findTableMeta(sch.content)
	if tbl == nil {
		return nil, fmt.Errorf("engine: no such table: main.%s", sch.content)
	}
	if tbl.withoutRowid {
		return nil, fts3WithoutRowidContentErr(sch.content)
	}
	if err := db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	rowids := make([]uint64, 0, tbl.rows.len())
	records := make([][]Value, 0, tbl.rows.len())
	for rid, rec := range tbl.rows.all() {
		rowids = append(rowids, rid)
		records = append(records, rec)
	}
	return fts3ProjectContent(sch, tbl.cols, tbl.ipkIndex, rowids, records, nil)
}

// fts3IndexOnlyDocids returns the docids table's index holds that its content
// table does not, ascending -- rows only a MATCH can reach.
//
// langid picks which language's %_segdir level range to read
// (getAbsoluteLevel): the MATCH's language, or 0 otherwise, mirroring
// fts3FilterMethod's default (fts3.c:3371-3372). That is safe for a non-MATCH
// caller, since materializeFts3Content discards the result then.
func (p *ReadOnlyPager) fts3IndexOnlyDocids(table string, sch fts3Schema, src *fts3ContentSource, langid int64) ([]int64, error) {
	// wanted=nil: every docid ANY term names is needed here, not just the
	// ones a particular query's tokens ask for (see fts3LoadIndex's doc
	// comment) -- this preserves the original eager, whole-vocabulary read.
	ix, err := p.fts3LoadIndex(table, fts3LevelBase(langid, sch.nIndex(), 0), nil)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, d := range ix.allDocids() {
		if _, ok := src.byID[d]; !ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// fts3ContentMatchLangid decides whether this read of a "content=" +
// "languageid=" table is MATCH-driven and, if so, which language it searches,
// via fts3MatchesTableIn and fts3ResolveMatchLangid. nil, nil means not
// MATCH-driven.
//
// elsewhere=nil is safe: compileMatchExpr already resolved over the same
// conjuncts at compile time and would have declined the statement if a JOIN ON
// or HAVING made the language ambiguous. Materialization runs only for
// statements that compiled, so this reaches the same langid.
func fts3ContentMatchLangid(it FromItem, name string, sch fts3Schema) (*int64, error) {
	if sch.langid == "" {
		return nil, nil
	}
	if !fts3MatchesTableIn(it.tvfWhere, name, sch.cols) &&
		(it.Alias == "" || !fts3MatchesTableIn(it.tvfWhere, it.Alias, sch.cols)) {
		return nil, nil
	}
	// The scope this MATCH's langid column would be qualified with, mirroring
	// tableScope.name's own rule: the alias if there is one,
	// the table's real name otherwise. Only .name is read by fts3IsLangidRef,
	// so a minimal stand-in tableScope is enough -- fts3ResolveMatchLangid and
	// its helpers never touch any other fts3MatchScope/tableScope field.
	exposed := name
	if it.Alias != "" {
		exposed = it.Alias
	}
	ms := fts3MatchScope{langidCol: sch.langid, scope: &tableScope{name: exposed}}
	langid, err := fts3ResolveMatchLangid(ms, it.tvfWhere, nil)
	if err != nil {
		return nil, err
	}
	return &langid, nil
}

// materializeFts3Content is materializeFts3 for a "content=" table: the content
// table's rows, plus -- only when the statement has a MATCH on this table -- an
// all-NULL row per docid the index holds and the content table lacks.
//
// Adding those unconditionally would be wrong: without MATCH, C scans only the
// content table. With one, the MATCH is a top-level AND conjunct that filters
// every row returned here, leaving exactly C's index-driven row set.
//
// # An unreadable content table
//
// fts3RowidMethod (fts3.c:3445-3448) and fts3ColumnMethod's docid case
// (fts3.c:3480-3482) answer off the cursor. Only the default arm, a real user
// column (fts3.c:3497-3504), calls fts3CursorSeek, which runs the content
// SELECT lazily per row. So a query proven never to read a real column
// (it.fts3ContentUnneeded, fts3StmtContentUnneeded's rules) never surfaces the
// content table's error -- missing, wrong shape (WITHOUT ROWID) or missing a
// column. e.g. "SELECT docid FROM ft WHERE ft MATCH 'N'" succeeds over
// content=nosuchtable, while "SELECT * ..." and offsets() fail (and
// fts3ResultColumnSafe declines those, so fts3ContentUnneeded is false).
//
// Falling back to an empty content source is then safe: every row is built
// from the index-only docids with all-NULL real columns, exactly what C
// returns. This can only add rows a hard error was dropping.
func (p *ReadOnlyPager) materializeFts3Content(it FromItem, name string, cols []columnInfo, sch fts3Schema) ([]columnInfo, [][]Value, []int64, error) {
	matchLangid, err := fts3ContentMatchLangid(it, name, sch)
	if err != nil {
		return nil, nil, nil, err
	}
	src, err := p.fts3ContentSourceOf(sch, matchLangid)
	if err != nil {
		if !it.fts3ContentUnneeded {
			return nil, nil, nil, err
		}
		// This exact statement shape never seeks a content row in C fts3
		// (see this function's own doc comment), so whatever went wrong
		// resolving the content table is never actually observed there --
		// answer as if it held zero rows, letting every docid the INDEX
		// holds surface as "extra" below instead.
		src = &fts3ContentSource{byID: map[int64]int{}}
	}
	langid := int64(0)
	if matchLangid != nil {
		langid = *matchLangid
	}
	extra, err := p.fts3IndexOnlyDocids(name, sch, src, langid)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(extra) > 0 {
		if it.tvfWhere == nil {
			// withVtabWhere stamps a non-nil (possibly empty) conjunct list on
			// every FROM item it sees, so nil means this one reached here
			// without one and the question below cannot be answered.
			return nil, nil, nil, fmt.Errorf("engine: %s table %s: its index holds %d docid(s) its content table (%s) does not, and this row source was resolved without the statement's WHERE clause, so which of the two C fts3 would have scanned is not decidable here", "fts4", name, len(extra), sch.content)
		}
		if !fts3MatchesTableIn(it.tvfWhere, name, sch.cols) &&
			(it.Alias == "" || !fts3MatchesTableIn(it.tvfWhere, it.Alias, sch.cols)) {
			// No MATCH on this table among the statement's top-level
			// conjuncts, so its row source is the content table ALONE --
			// exactly what C fts3 scans (verified: over a table whose index
			// holds docid 21 and whose content table holds only rowid 7,
			// "SELECT rowid,* FROM ft3" is {7,...} and nothing else).
			// withVtabWhere puts every top-level WHERE conjunct here, and a
			// "content=" table's own compiler placement (fts3MatchBindings)
			// refuses any OTHER MATCH against it -- including a JOIN's own ON
			// clause, deliberately, so this row source and that placement
			// decision never disagree -- so this really is "no MATCH on this
			// table".
			extra = nil
		}
	}
	rows := make([][]Value, 0, len(src.rows)+len(extra))
	rowids := make([]int64, 0, len(src.rows)+len(extra))
	i, j := 0, 0
	for i < len(src.rows) || j < len(extra) {
		// Merged ASCENDING by docid, which is the order both sources are
		// already in and the order C fts3 returns either of them in.
		if j >= len(extra) || (i < len(src.rows) && src.docids[i] < extra[j]) {
			rows = append(rows, fts3PadRow(src.rows[i], len(cols)))
			rowids = append(rowids, src.docids[i])
			i++
			continue
		}
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: extra[j]}
		if sch.langid != "" && matchLangid != nil {
			// fts3ColumnMethod's case 2 (fts3.c:3462-3499) answers langid from the
			// cursor's iLangid whenever pCsr->pExpr is set, for every row --
			// including index-only ones. Leaving it NULL would drop the row from
			// its own MATCH's "AND langid=N". extra is only non-empty when
			// fts3MatchesTableIn found a MATCH, the same test that set
			// matchLangid, so matchLangid is non-nil here when sch.langid is set.
			row[len(cols)-1] = Value{Typ: Int, I: *matchLangid}
		}
		rows = append(rows, row)
		rowids = append(rowids, extra[j])
		j++
	}
	return cols, rows, rowids, nil
}

// fts3PadRow widens a projected content row to the table's declared column
// count, which for a "languageid=" table is one wider than its user columns --
// a combination this engine declines, so in practice the two already match.
func fts3PadRow(row []Value, n int) []Value {
	if len(row) == n {
		return row
	}
	out := make([]Value, n)
	copy(out, row)
	return out
}

// fts3MatchesTableIn reports whether any of conj is a top-level "<table> MATCH
// ..." (or "<col> MATCH ..." over one of table's columns), or an OR of two
// such -- what makes a "content=" table's row source its index. withVtabWhere
// already split top-level ANDs, and fts3MatchBindings refuses a "content="
// MATCH anywhere else (including JOIN ON), so a MATCH not seen here cannot
// exist. The OR case matters because "ct MATCH 'a' OR ct MATCH 'b'" is answered
// from the index over both, so a docid only the second branch selects must
// reach this row source.
func fts3MatchesTableIn(conj []Expr, table string, cols []string) bool {
	for _, e := range conj {
		if fts3ExprMatchesTable(e, table, cols) {
			return true
		}
	}
	return false
}

// fts3ExprMatchesTable is fts3MatchesTableIn's per-conjunct test, recursing
// through OR exactly as fts3MatchBindings' slotScope does, so the two never
// disagree about what counts as a MATCH constraint against table.
func fts3ExprMatchesTable(e Expr, table string, cols []string) bool {
	switch x := e.(type) {
	case MatchExpr:
		ce, isCol := x.X.(ColumnExpr)
		if !isCol {
			return false
		}
		if ce.Qualifier != "" && !strings.EqualFold(ce.Qualifier, table) {
			return false
		}
		if strings.EqualFold(ce.Name, table) {
			return true
		}
		for _, c := range cols {
			if strings.EqualFold(ce.Name, c) {
				return true
			}
		}
		return false
	case BinaryExpr:
		if x.Op != "OR" {
			return false
		}
		return fts3ExprMatchesTable(x.L, table, cols) || fts3ExprMatchesTable(x.R, table, cols)
	}
	return false
}
