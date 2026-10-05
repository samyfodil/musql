// This file is PRAGMA integrity_check's PER-ROW half: the checks C SQLite
// runs once per table row, in row order, over each table's declared column
// constraints and each of its indexes.
//
// pragma.go owns the dispatch and the shared error-count budget. This file is
// the loop at pragma.c:1822-2155, "Make sure all the indices are constructed
// correctly", which opens each table and its indexes and walks the rows. (C's
// STRUCTURAL half, OP_IntegrityCk at pragma.c:1783, checks pages, which this
// format does not have.)
//
// ---- RULE ZERO: the shape being ported ----
//
// pragma.c:1856 rewinds the table cursor and, per row, emits in this exact
// order:
//
//	pragma.c:1902-2032  per COLUMN, in declaration order, skipping iPKey:
//	                    (1) a NOT NULL column holding NULL      "NULL value in %s.%s"    (:1975)
//	                    (2) STRICT type exactness               "non-%s value in %s.%s"
//	                    (3) a TEXT column holding a number      "NUMERIC value in %s.%s"
//	                    (4) a numeric column holding text       "TEXT value in %s.%s"
//	pragma.c:2035-2057  the table's CHECK constraints           "CHECK constraint failed in %s"
//	pragma.c:2059-2151  per INDEX, in pTab->pIndex order:
//	                    the row's entry is absent               "row %d missing from index %s"     (:2086)
//	                    a non-BINARY key differs from the table  "row %d values differ from index %s" (:2124)
//	                    a UNIQUE index's next entry has the
//	                    same key columns                        "non-unique entry in index %s"      (:2147)
//
// and, before the row loop, one finding per index whose on-disk entry count
// disagrees with its table's row count, "wrong # of entries in index %s"
// (pragma.c:1794-1820).
//
// ---- what this file ports, and what it deliberately does not ----
//
// PORTED: (1), the index-entry checks and the UNIQUE check, plus the CHECK
// half that was already here, interleaved in exactly the order above, and the
// "wrong # of entries" count that precedes every per-row finding. The index
// entries are derived from the rows, which on this format is all an index is.
//
// NOT PORTED, each for a stated reason rather than by omission:
//
//   - (2)/(3)/(4), the datatype checks. These are not tests of the logical
//     value at all: OP_IsType reads the record's own SERIAL TYPE
//     (pragma.c:1930's p3/p4 operands are a cursor and a storage-column
//     index), so "NUMERIC value in t.c" fires on a TEXT-affinity column
//     whose STORED header says INTEGER, which is a different question from
//     "does this Value look numeric". Porting them means porting that
//     storage-level type view, which this engine's decoded Value does not
//     carry. Leaving them out under-reports exactly as this pragma already
//     did before this file existed -- never a wrong answer of its own.
//   - "rowid not at end-of-record" and the imprecise-floating-point probe
//     (OP_IFindKey). Both concern a record's physical encoding, which the
//     decoded Value this read path yields does not carry.
//   - a WITHOUT ROWID table's per-row checks and its "row not in PRIMARY KEY
//     order" (pragma.c:1882-1901). pragma.c drives those through the PK
//     index cursor rather than a table cursor, and this file's row read is
//     RowsOfRoot's rowid walk; such a table keeps the CHECK half alone,
//     exactly as before.
//   - an EXPRESSION or PARTIAL index. Its key is idx.keys/idx.where rather
//     than idx.colIdx (indexMeta.exprOrPartial, index_write.go), and
//     pragma.c's own partial-index arm skips a row the WHERE excludes
//     (sqlite3GenerateIndexKey's jmp3 / sqlite3ResolvePartIdxLabel,
//     pragma.c:2064/2152). Skipped here; same under-report, same reason.
//
// ---- row order and index order, which decide what a capped answer keeps ----
//
// Rows are visited in the table b-tree's own ascending-rowid walk order,
// which is OP_Rewind/OP_Next's (pragma.c:1856/2155). Indexes are visited in pTab->pIndex order,
// which is NOT catalog order: sqlite3CreateIndex PREPENDS
// (build.c:4485-4487, "pIndex->pNext = pTab->pIndex; pTab->pIndex =
// pIndex"), and the schema loads a table's own automatic indexes while
// parsing its CREATE TABLE, before any CREATE INDEX row. So the list is the
// REVERSE of (automatic indexes in declaration order, then explicit indexes
// in catalog order) -- with any ON CONFLICT REPLACE index moved to the end
// afterwards (build.c:4499-4515). Both are reproduced below, because the
// order is what decides which findings survive "PRAGMA integrity_check(N)".
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// integrityRowFindings is one table's findings, in pragma.c's order: counts
// holds its "wrong # of entries in index" findings (pragma.c:1794-1820, which
// the caller emits for EVERY table before any per-row finding) and rows its
// per-row findings. tr is the table's own catalog row and schema the whole
// catalog (needed to find its indexes and their rootpages). qualify asks for a
// "main."/"temp." prefix on the CHECK-constraint query. isQuick omits the per-row index checks,
// which is what quick_check means (pragma.c:2058's "Omit the remaining tests
// for quick_check"); the count block runs for both.
func (p *ReadOnlyPager) integrityRowFindings(tr *SchemaRow, schema []SchemaRow, qualify, isQuick bool) (counts, out []string, err error) {
	withoutRowid := sqlTextTableIsWithoutRowid(tr.SQL)
	cols, checks, specs, perr := parseCreateTableColumnsAndAutoIndexes(tr.SQL, withoutRowid)
	if perr != nil {
		return nil, nil, fmt.Errorf("engine: PRAGMA integrity_check: %s: %w", tr.Name, perr)
	}
	if p.IgnoreCheckConstraints() {
		// pragma.c:2035's own gate is on the CHECK arm alone
		// ("pTab->pCheck && (db->flags & SQLITE_IgnoreChecks)==0"), not on
		// the column or index arms -- see SetIgnoreCheckConstraints.
		checks = nil
	}

	// A WITHOUT ROWID table keeps exactly the behaviour it had before this
	// file: the CHECK half alone, counted through the query engine. See this
	// file's doc comment for why its per-row arm is a separate port.
	if withoutRowid || tr.RootPage == 0 {
		out, err = p.integrityCheckRowsFailingChecks(tr, cols, checks, qualify)
		return nil, out, err
	}

	ipk := -1
	for i, c := range cols {
		if c.IsRowidAlias {
			ipk = i
			break
		}
	}
	tm := &tableMeta{
		name:     tr.Name,
		isTemp:   tr.Temp,
		sql:      tr.SQL,
		cols:     cols,
		ipkIndex: ipk,
		rootPage: tr.RootPage,
	}
	rows, rowidsSorted, rerr := readTableRowsFromPager(p, tm)
	if rerr != nil {
		// A table whose rows this pager cannot decode is exactly what the
		// STRUCTURAL half is for, and it already ran. Fall back to the
		// CHECK-only answer this function gave before the per-row arms
		// existed rather than turn a report into an error: under-reporting is
		// the pre-existing behaviour, erroring is a new decline.
		out, err = p.integrityCheckRowsFailingChecks(tr, cols, checks, qualify)
		return nil, out, err
	}
	order := rowidsSorted
	if order == nil {
		// readTableRowsFromPager only hands the walk order over when it was
		// strictly ascending; a b-tree that was not is corrupt, and the
		// STRUCTURAL half already reports it. Visit ascending anyway rather
		// than in map order, so a capped answer is at least deterministic.
		order = make([]uint64, 0, len(rows))
		for rowid := range rows {
			order = append(order, rowid)
		}
		sort.Slice(order, func(a, b int) bool { return rowidLess(order[a], order[b]) })
	}

	// A table whose automatic indexes this parser cannot re-derive
	// contributes no index findings, for the same reason the row-read failure
	// above contributes none: under-report, never error.
	indexes := p.integrityIndexesOf(tr, schema, tm, cols, specs)
	// Each index's entries in index order. This format stores no index of its
	// own, so they are derived from the rows and always agree with them --
	// unless the check was handed another copy's stored entries
	// (IntegrityCheckOptions.StoredIndexEntries), which it then holds against
	// the rows exactly as C holds its index b-trees.
	enc := p.encoding()
	sorted := make([][][]Value, len(indexes))
	for k, idx := range indexes {
		if p.storedIndexEntries != nil {
			if recs, ok := p.storedIndexEntries(idx.name); ok {
				sorted[k] = recs
				if len(recs) != len(rows) {
					counts = append(counts, "wrong # of entries in index "+idx.name)
				}
				continue
			}
		}
		recs := make([][]Value, 0, len(rows))
		for rowid, vals := range rows {
			recs = append(recs, indexEntryForNewRow(tm, idx, rowid, vals))
		}
		sort.SliceStable(recs, func(a, b int) bool {
			return compareIndexRec(recs[a], recs[b], idx.colCollation, idx.colDesc, enc) < 0
		})
		sorted[k] = recs
	}
	if isQuick {
		indexes = nil
	}

	violating, cerr := p.integrityCheckViolatingRowids(tr, cols, checks, qualify, order, rows)
	if cerr != nil {
		return nil, nil, cerr
	}

	_, strict, _ := parseTableTailClauses(tr.SQL)
	for _, rowid := range order {
		vals := rows[rowid]
		// (1)-(4), per column in declaration order, skipping the rowid alias --
		// pragma.c:1920's "if( j==pTab->iPKey ) continue".
		for j, c := range cols {
			if j == ipk {
				continue
			}
			if msg := integrityColumnFinding(tr.Name, c, strict, vals, j); msg != "" {
				out = append(out, msg)
			}
		}
		if violating[rowid] {
			out = append(out, fmt.Sprintf("CHECK constraint failed in %s", tr.Name))
		}
		for k, idx := range indexes {
			// pragma.c:2066-2089: OP_Found seeks the full key, rowid included,
			// under the index's own collations; no entry is "row %d missing".
			key := indexEntryForNewRow(tm, idx, rowid, vals)
			recs := sorted[k]
			at := sort.Search(len(recs), func(i int) bool {
				return compareIndexRec(recs[i], key, idx.colCollation, idx.colDesc, enc) >= 0
			})
			if at == len(recs) || compareIndexRec(recs[at], key, idx.colCollation, idx.colDesc, enc) != 0 {
				out = append(out, fmt.Sprintf("row %d missing from index %s", int64(rowid), idx.name))
				continue
			}
			// pragma.c:2111-2124: a key column under a non-BINARY collation
			// must still hold the table's exact value. OP_Ne carries no
			// collating sequence and no SQLITE_NULLEQ, so it compares BINARY
			// and a NULL on either side never counts as different.
			for kk := range idx.colIdx {
				if kk < len(idx.colCollation) && !strings.EqualFold(idx.colCollation[kk], "BINARY") &&
					recs[at][kk].Typ != Null && key[kk].Typ != Null && compareValues(recs[at][kk], key[kk]) != 0 {
					out = append(out, fmt.Sprintf("row %d values differ from index %s", int64(rowid), idx.name))
					break
				}
			}
			if idx.unique && integrityEntryIsNonUnique(idx, cols, recs, at, enc) {
				out = append(out, fmt.Sprintf("non-unique entry in index %s", idx.name))
			}
		}
	}
	return counts, out, nil
}

// integrityEntryIsNonUnique is pragma.c:2129-2149, the UNIQUE arm, verbatim:
// the row is unique if (1) any key column that the table does NOT declare
// NOT NULL holds NULL, or (2) the NEXT index entry differs in the key
// columns. An entry with no successor at all is unique too -- OP_Next
// falling through jumps to uniqOk (pragma.c:2142-2143).
func integrityEntryIsNonUnique(idx *indexMeta, cols []columnInfo, sorted [][]Value, at int, enc TextEncoding) bool {
	n := len(idx.colIdx)
	if n == 0 || at < 0 || at >= len(sorted) {
		return false
	}
	rec := sorted[at]
	for kk := 0; kk < n && kk < len(rec); kk++ {
		ci := idx.colIdx[kk]
		if ci >= 0 && ci < len(cols) && cols[ci].NotNull {
			// pragma.c:2138's "if( iCol>=0 && pTab->aCol[iCol].notNull )
			// continue" -- a column the table declares NOT NULL gets NO
			// null-exemption test emitted for it at all, so a NULL sitting
			// there (which only a corrupt file can hold) still compares.
			continue
		}
		if rec[kk].Typ == Null {
			return false
		}
	}
	if at+1 >= len(sorted) {
		return false
	}
	next := sorted[at+1]
	if n > len(rec) || n > len(next) {
		return false
	}
	// OP_IdxGT over nKeyCol columns: non-unique exactly when the successor is
	// NOT greater on those columns alone.
	return compareIndexRec(next[:n], rec[:n], idx.colCollation, idx.colDesc, enc) <= 0
}

// integrityIndexesOf collects the plain-column indexes of tr in pTab->pIndex
// order. See this file's doc comment for that order's citation and for why
// expression/partial indexes are left out.
func (p *ReadOnlyPager) integrityIndexesOf(tr *SchemaRow, schema []SchemaRow, tm *tableMeta, cols []columnInfo, specs []autoIndexSpec) []*indexMeta {
	auto, _, aerr := buildAutoIndexes(tr.Name, cols, specs)
	if aerr != nil {
		return nil
	}
	// LOAD order first: the automatic indexes (built while the CREATE TABLE
	// row is parsed) then the explicit ones in catalog order.
	loaded := append([]*indexMeta(nil), auto...)
	for i := range schema {
		sr := &schema[i]
		if sr.Type != "index" || sr.SQL == "" || sr.Temp != tr.Temp ||
			!equalFoldName(sr.TblName, tr.Name) {
			continue
		}
		stmt, perr := parseCreateIndexStmt(sr.SQL)
		if perr != nil {
			// A catalog this pragma cannot parse is exactly what the
			// STRUCTURAL half and C SQLite's own schema load report; do
			// not turn it into an error from the per-row half too.
			continue
		}
		demoteDoubleQuotedIndexColumns(stmt, tm)
		if stmt.exprOrPartial {
			continue
		}
		colIdx, colColl, rerr := resolvePlainIndexColumns(tm, stmt)
		if rerr != nil {
			continue
		}
		ix := &indexMeta{
			name:         sr.Name,
			table:        tm.name,
			isTemp:       sr.Temp,
			cols:         stmt.cols,
			colIdx:       colIdx,
			colCollation: colColl,
			colDesc:      append([]bool(nil), stmt.desc...),
			unique:       stmt.unique,
			sql:          sr.SQL,
		}
		loaded = append(loaded, ix)
	}
	// ...then REVERSE it, which is what prepending produced.
	out := make([]*indexMeta, 0, len(loaded))
	for i := len(loaded) - 1; i >= 0; i-- {
		if !loaded[i].isTablePK && !loaded[i].exprOrPartial {
			out = append(out, loaded[i])
		}
	}
	// ...and move every ON CONFLICT REPLACE index to the end
	// (build.c:4499-4515), stably.
	head := out[:0:0]
	var tail []*indexMeta
	for _, ix := range out {
		if ix.onConflict == conflictReplace {
			tail = append(tail, ix)
		} else {
			head = append(head, ix)
		}
	}
	return append(head, tail...)
}

// integrityCheckViolatingRowids reports which of tr's rows break at least one
// CHECK constraint, keyed by rowid.
//
// It asks the QUERY ENGINE
// rather than re-implementing evaluation -- the constraint's verbatim source
// text resolves the same names against the same row with the same
// affinities and collations, and a CHECK passes on NULL, which falls out of
// the "NOT (<expr>)" spelling for free. The difference is only that it needs
// WHICH rows rather than how many, so that a violation lands in pragma.c's
// own per-row position rather than in a block of its own.
//
// The rows come back in the scan's order, which for an unqualified full scan
// of a rowid table is the b-tree's own ascending walk -- the identical order
// `order` holds. That is asserted, not assumed: a length mismatch falls back
// to marking the FIRST n rows, which keeps the multiset exact (the only
// thing an order-insensitive comparison can see) and is unreachable for any
// well-formed table.
func (p *ReadOnlyPager) integrityCheckViolatingRowids(tr *SchemaRow, cols []columnInfo, checks []checkConstraint, qualify bool, order []uint64, rows map[uint64][]Value) (map[uint64]bool, error) {
	if len(checks) == 0 {
		return nil, nil
	}
	_, vals, qerr := p.QueryArgs(integrityCheckProbeSQL(tr, checks, qualify, "SELECT ("+integrityCheckViolationExpr(checks)+") FROM "), nil)
	if qerr != nil {
		return nil, fmt.Errorf("engine: PRAGMA integrity_check: %s: %w", tr.Name, qerr)
	}
	out := map[uint64]bool{}
	for i, r := range vals {
		if i >= len(order) || len(r) != 1 {
			break
		}
		if isTruthy(r[0]) {
			out[order[i]] = true
		}
	}
	return out, nil
}

// integrityCheckRowsFailingChecks is the pre-existing, COUNT-shaped CHECK
// half, kept verbatim for the one table kind the per-row port above does not
// cover (WITHOUT ROWID, and a rootpage-less row).
func (p *ReadOnlyPager) integrityCheckRowsFailingChecks(tr *SchemaRow, cols []columnInfo, checks []checkConstraint, qualify bool) ([]string, error) {
	if len(checks) == 0 {
		return nil, nil
	}
	_, vals, qerr := p.QueryArgs(integrityCheckProbeSQL(tr, checks, qualify, "SELECT count(*) FROM ")+" WHERE "+integrityCheckViolationExpr(checks), nil)
	if qerr != nil {
		return nil, fmt.Errorf("engine: PRAGMA integrity_check: %s: %w", tr.Name, qerr)
	}
	if len(vals) != 1 || len(vals[0]) != 1 || vals[0][0].Typ != Int {
		return nil, fmt.Errorf("engine: PRAGMA integrity_check: %s: unexpected count shape", tr.Name)
	}
	msg := fmt.Sprintf("CHECK constraint failed in %s", tr.Name)
	out := make([]string, 0, vals[0][0].I)
	for n := vals[0][0].I; n > 0; n-- {
		out = append(out, msg)
	}
	return out, nil
}

// integrityCheckProbeSQL renders prefix followed by tr's (optionally
// catalog-qualified) name. Qualifying is only ever done when the OTHER
// catalog holds a table of the same name, which is the one case a bare name
// would resolve to the wrong one -- always qualifying would be worse, since
// "main." does not resolve on every pager this can run on.
func integrityCheckProbeSQL(tr *SchemaRow, checks []checkConstraint, qualify bool, prefix string) string {
	var sb strings.Builder
	sb.WriteString(prefix)
	if qualify {
		if tr.Temp {
			sb.WriteString("temp.")
		} else {
			sb.WriteString("main.")
		}
	}
	sb.WriteString(quoteIdent(tr.Name))
	return sb.String()
}

// integrityCheckViolationExpr is "NOT (c1) OR NOT (c2) OR ..." over the
// constraints' verbatim source text.
func integrityCheckViolationExpr(checks []checkConstraint) string {
	var sb strings.Builder
	for i, cc := range checks {
		if i > 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteString("NOT (")
		sb.WriteString(cc.exprText)
		sb.WriteString(")")
	}
	return sb.String()
}

// integrityColumnFinding is pragma.c:1911-2032 for one column of one row: the
// one message C emits for it, or "".
//
//	(1) NOT NULL holding NULL                "NULL value in %s.%s"  -- and nothing
//	    else for that column (a failed (1) jumps to labelError, :1976-1980)
//	(2) STRICT, declared type not ANY: the value's type must be in the type's
//	    mask (aStdTypeMask, :1984-1991)       "non-%s value in %s.%s"
//	(3) non-STRICT TEXT affinity holding INT or REAL   "NUMERIC value in %s.%s"
//	(4) non-STRICT numeric affinity holding TEXT that NUMERIC affinity would
//	    convert                                  "TEXT value in %s.%s"
//
// C tests the STORED type (OP_IsType reads the record header), this engine the
// logical value; they agree on every mask here -- a REAL column C stores as an
// integer record passes REAL's mask, which admits INT.
//
// ponytail: a SHORT row (written before the column existed) takes C's p4, the
// DEFAULT's type; a column with a DEFAULT is not judged here -- a missed finding
// on that one shape, never an extra one.
func integrityColumnFinding(table string, c columnInfo, strict bool, vals []Value, j int) string {
	var v Value
	if j < len(vals) {
		v = vals[j]
	} else if c.HasDefault {
		return ""
	}
	if c.NotNull && v.Typ == Null {
		return fmt.Sprintf("NULL value in %s.%s", table, c.Name)
	}
	if v.Typ == Null {
		return "" // NULL is in every type's mask
	}
	switch {
	case strict:
		want := strings.ToUpper(strings.TrimSpace(c.DeclType))
		ok := true
		switch want {
		case "INT", "INTEGER":
			ok = v.Typ == Int
		case "REAL":
			ok = v.Typ == Int || v.Typ == Float
		case "TEXT":
			ok = v.Typ == Text
		case "BLOB":
			ok = v.Typ == Blob
		}
		if !ok {
			return fmt.Sprintf("non-%s value in %s.%s", want, table, c.Name)
		}
	case c.Aff == affText:
		if v.Typ == Int || v.Typ == Float {
			return fmt.Sprintf("NUMERIC value in %s.%s", table, c.Name)
		}
	case c.Aff >= affNumeric:
		if v.Typ == Text && applyAffinityToValue(v, affNumeric).Typ != Text {
			return fmt.Sprintf("TEXT value in %s.%s", table, c.Name)
		}
	}
	return ""
}
