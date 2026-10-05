// This file chooses secondary indexes for "WHERE <indexed-col> = <const>" seeks.
// A seek prunes candidates; the full WHERE clause is re-evaluated on results.
package engine

import (
	"strings"
)

// indexSeekCandidate describes one secondary index over an ordinary rowid table
// that is usable for a LEADING-column equality seek: its b-tree root page, the
// table column (index into the resolved table's cols) its leading indexed
// column resolves to, and that leading column's collating sequence. Every
// candidate secondaryIndexSeekCandidates returns has ALREADY been verified to
// have a plain (non-expression) leading column whose INDEX collation equals the
// column's own declared collation -- which is the collation a bare
// "col = <literal/param>" comparison resolves to -- so a seek probing this
// index under coll reproduces that comparison's equality exactly (see
// SeekIndexRowidsSegments and detectIndexSeekKey).
type indexSeekCandidate struct {
	root       uint32
	leadingCol int
	coll       string
}

// secondaryIndexSeekCandidates returns every secondary index over the named
// ordinary rowid table that detectIndexSeekKey may use for a leading-column
// equality seek. It reads sqlite_schema for the table's 'index' rows and, for
// each, recovers the leading indexed column and its collation:
//
//   - Only an explicit CREATE INDEX (its SQL text is present in sqlite_schema)
//     is considered, and it is parsed with the write path's own
//     parseCreateIndexStmt. An expression or partial index -- which
//     this engine never materializes -- is skipped, as is any index whose
//     leading entry is an expression rather than a bare column, or whose
//     LEADING column is DESC (see below).
//   - AUTOMATIC indexes (UNIQUE/PRIMARY KEY, whose sqlite_schema SQL column is
//     NULL) are DECLINED outright: their leading column's sort direction is not
//     recoverable from the index row itself (it lives in the TABLE's own
//     constraint text -- see autoIndexSpec.desc, which the write path does now
//     honour), so a UNIQUE(col DESC) -- stored in descending b-tree order --
//     could not be told apart from an ascending one, risking a mis-navigated
//     seek (a subset -> a WRONG).
//     Declining them is always safe (full scan) and costs only the optimization.
//
// Two further shapes are DECLINED for the same superset-safety reason:
//
//   - A DESC LEADING column, kept from when the seek walked an index b-tree in
//     its stored order. Only desc[0] is checked.
//   - A leading-column INDEX collation that differs from that column's declared
//     collation (an explicit "CREATE INDEX ... (col COLLATE X)" where X is not
//     the column's own declared collation): the b-tree is then ordered by a
//     collation the "col = key" comparison does not use, so seeking it could
//     miss case/space-variant matches the comparison would accept.
//
// Anything that can't be positively resolved is skipped rather than guessed, so
// the returned set only ever contains safe-to-seek indexes.
func (p *ReadOnlyPager) secondaryIndexSeekCandidates(tableName string, tableRoot uint32, cols []columnInfo) ([]indexSeekCandidate, error) {
	if tableName == "" {
		return nil, nil
	}
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	// An index belongs to ONE table in ONE catalog (build.c:4485-4486 links it
	// into pTab->pIndex; sqlite3FindIndex, build.c:526-541, looks a name up in
	// ONE database's idxHash) -- so matching by tbl_name ALONE picks up the
	// other catalog's index the moment a TEMP table shadows a MAIN one of the
	// same name. That reads a b-tree keyed on rows the scanned table does not
	// have: "SELECT k FROM temp.t1 WHERE k=30" seeked MAIN's index and answered
	// no rows for a row that exists.
	//
	// The scanned table is identified by its ROOT PAGE, which is what the
	// cursor is actually opened on, so the owner row is found by root and its
	// catalog then gates every candidate. An owner that cannot be identified
	// declines to the full scan rather than guessing -- the same
	// superset-safety rule the rest of this function follows.
	ownerTemp, found, namesakes := false, false, 0
	for i := range rows {
		if rows[i].Type != "table" || !equalFoldName(rows[i].Name, tableName) {
			continue
		}
		namesakes++
		if rows[i].RootPage == tableRoot {
			ownerTemp, found = rows[i].Temp, true
		}
	}
	if !found {
		return nil, nil
	}
	if namesakes > 1 {
		// A main/temp name COLLISION: the catalog an INDEX row carries is
		// DERIVED on read (markTempSchemaRows) from its owner's name plus
		// creation order, and that derivation cannot see a schema qualifier --
		// C SQLite stores "CREATE INDEX main.tbli ON tbl(...)" as
		// "CREATE INDEX tbli ON tbl(a,b,c)" (verified against the 3.53.3
		// oracle), because ITS two schema b-trees encode the catalog by the
		// row's LOCATION. So under a collision an index's Temp flag may name
		// the wrong side, and seeking it reads a b-tree keyed on another
		// table's rows -- "SELECT a FROM temp.tbl WHERE a=1" answered no rows
		// off an index holding MAIN's. Decline to the full scan, which is
		// always correct and only costs the optimization.
		return nil, nil
	}
	colIndexOf := func(name string) (int, bool) {
		for j := range cols {
			if equalFoldName(cols[j].Name, name) {
				return j, true
			}
		}
		return 0, false
	}

	var out []indexSeekCandidate
	for i := range rows {
		r := &rows[i]
		// RootPage 0 is how a SEGMENT table's index is reported -- the format
		// carries an index's SQL and not its data -- and it used to be skipped
		// here, which made every "WHERE sec = ?" on our own format a full scan:
		// 156.51ms against C SQLite's 12.34us at 100,000 rows. It is admitted now
		// when the table is segment-backed, and the seek is served from the
		// column's own index instead of an index b-tree (segment_seek.go's
		// seekIndexRowids). A root of 0 on a table that is NOT segment-backed is
		// still skipped: there is genuinely nothing to seek.
		if r.Type != "index" || !equalFoldName(r.TblName, tableName) ||
			r.Temp != ownerTemp {
			continue
		}
		if r.RootPage == 0 && !p.ScannedFromSegments(tableRoot) {
			continue
		}
		// Only explicit CREATE INDEX rows (SQL text present) are usable;
		// automatic indexes (SQL NULL) are declined -- see this function's doc
		// comment.
		if r.SQL == "" {
			continue
		}
		stmt, perr := parseCreateIndexStmt(r.SQL)
		if perr != nil || stmt.exprOrPartial || len(stmt.cols) == 0 || stmt.cols[0] == "" {
			continue // unparseable, expression/partial index, or non-plain leading column
		}
		if len(stmt.desc) > 0 && stmt.desc[0] {
			continue // DESC leading column -- ascending seek reader would mis-navigate
		}
		leadingName := stmt.cols[0]
		explicitColl := stmt.collate[0]

		colIdx, ok := colIndexOf(leadingName)
		if !ok {
			continue
		}
		whereColl := effectiveCollation(cols[colIdx].Collation)
		idxColl := whereColl
		if explicitColl != "" {
			idxColl = strings.ToUpper(explicitColl)
		}
		if !equalFoldName(idxColl, whereColl) {
			continue // index ordered by a collation the comparison doesn't use -- unsafe
		}
		out = append(out, indexSeekCandidate{root: r.RootPage, leadingCol: colIdx, coll: whereColl})
	}
	return out, nil
}
