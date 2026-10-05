// This file implements fts5's contentless tables -- "content=''"
// (FTS5_CONTENT_NONE). They store no documents: the shadow set is %_data,
// %_idx, %_config and, unless columnsize=0, %_docsize. Every other fts5 path
// here re-tokenizes visible rows; this file reads %_data's inverted index back
// into documents and everything downstream runs on those.
//
// # What C does with a contentless table
//
//	xColumn        NULL for every user column (fts5ColumnMethod guards on
//	               eContent!=FTS5_CONTENT_NONE).
//	plain scan     the rowids of %_docsize (fts5ConfigParse), one all-NULL row
//	               per indexed document, in rowid order.
//	columnsize=0   zContent is NULL; any query with no MATCH is "<name>: table
//	               does not support scanning" (see below).
//	DELETE         "cannot DELETE from contentless fts5 table: <name>", unless
//	               contentless_delete=1.
//	UPDATE         "cannot UPDATE contentless fts5 table: <name>", unless every
//	               modified column is UNINDEXED, or contentless_delete=1 and all
//	               indexed columns were modified.
//	'rebuild'      refused.
//	'delete'       allowed: the caller supplies the values to subtract.
//	'delete-all'   allowed.
//
// # The index is the document set
//
// For detail=full a position is (column << 32) | offset, and
// fts5StorageInsertCallback numbers a column's tokens 0..szCol-1 with no gaps
// (FTS5_TOKEN_COLOCATED is only set by synonym tokenizers, none of which exist
// here). So every document is recoverable. The reconstruction is checked: a
// slot claimed twice is refused, and per-column counts must match %_docsize.
// That is stronger than external content's byte check: it compares postings,
// not layout, so a multi-segment file from C reads and writes here.
//
// # contentless_delete=1
//
// The one contentless table a row can be deleted from. C cannot rewrite a
// segment, so it records the rowid in a tombstone (fts5MultiIterIsDeleted),
// which needs:
//
//	%_docsize      a third column, "origin" (sqlite3Fts5IndexGetOrigin at write
//	               time), saying which segments can hold the row.
//	structure      the V2 record (FTS5_STRUCTURE_V2): five more varints per
//	record         segment -- iOrigin1, iOrigin2, nPgTombstone, nEntryTombstone,
//	               nEntry (fts5_decode.go, fts5_index.go).
//	tombstone      a second %_data address space (FTS5_TOMBSTONE_ROWID) holding
//	pages          a hash of deleted rowids per segment; fts5_decode.go reads it.
//
// This engine never writes a tombstone: rebuilding the index after every
// statement reaches the state fts5's merge reaches after applying them. It must
// write the origin state, though: with a legacy structure record
// sqlite3Fts5IndexGetOrigin returns 0 and a later C DELETE skips the tombstone,
// leaving the row matchable forever. So every contentless_delete=1 table gets
// the V2 record.
//
// C recovers the next origin as max(iOrigin2)+1 (fts5StructureDecode); this
// engine's one segment spans 1..(counter-1), which covers every document's
// origin and carries the counter across a reopen. Exception: a table with
// every row deleted has no segment here, so a reopen restarts origins at 1
// where C keeps counting. No query result depends on it.
//
// # columnsize=0
//
// No %_docsize, so:
//
//   - rowids and per-column counts come from the postings
//     (fts5ContentlessSlotsFromPostings), exact for detail=full;
//   - a document that tokenized to nothing is unrecoverable. The averages record
//     still counts it, so fts5ContentlessRead refuses the table on mismatch;
//   - zContent is NULL, so fts5_main.c:1623 refuses any query without a MATCH,
//     even a rowid equality (fts5columnsize.test 2.7.1-2.7.3). See
//     fts5ContentlessScanGuard.
//
// # Still declined
//
//	detail=none     positions are not (fully) stored, so the reconstruction
//	detail=columns  cannot be checked.
//	UPDATE of a     C allows it when SET names every indexed column; this write
//	contentless_    path gets the whole new row, not the SET list.
//	delete=1 table
//
// # contentless_unindexed=1
//
// On a table with an UNINDEXED column this is a third mode,
// FTS5_CONTENT_UNINDEXED: %_content returns, holding c<i> for each UNINDEXED
// column only, under its original index, and fts5ConfigMakeExprlist reads
// those and NULL for the rest. Otherwise it is the plain contentless table.
// fts5Store.contentUnindexed carries the mode; fts5ContentlessLoadUnindexed and
// fts5ContentlessFillUnindexed overlay the stored columns on the write and read
// paths.
package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// fts5ContentlessScanGuard reproduces fts5_main.c:1623 --
//
//	}else if( pConfig->zContent==0 ){
//	  fts5SetVtabError(pTab,"%s: table does not support scanning",pConfig->zName);
//
// -- for the one kind of table whose zContent is NULL (fts5_config.c:683-702):
//
//	eContent == FTS5_CONTENT_NONE  &&  !FTS5_CONTENT_UNINDEXED  &&  !bColumnsize
//
// A plain contentless table scans %_docsize, and a contentless_unindexed=1 one
// is promoted to FTS5_CONTENT_UNINDEXED and scans its %_content, so both still
// scan.
//
// C raises the error in xFilter whenever there is no MATCH. With no xFilter to
// hang it on, the test is syntactic per SelectStmt node, and narrower: a FROM
// item is served only when it is the node's only reference to the table and a
// top-level AND conjunct of the node is a MATCH naming it. Everything else
// declines -- a gap at worst, never rows where C errors.
//
// It walks the tree itself because the VDBE compiles a statement in one pass;
// execSelect is not re-entered per derived table, CTE or compound arm. The
// table-valued spelling "FROM t('query')" counts as a MATCH, since
// fts5_tablefunc.go's rewrite runs after this guard.
func (p *ReadOnlyPager) fts5ContentlessScanGuard(stmt *SelectStmt) error {
	// Cheap pre-filter: the schema is only consulted for a statement that names
	// a table this could be about.
	if !selectTreeMentionsTable(stmt, p.fts5TableCannotScan) {
		return nil
	}
	return p.fts5ScanGuardWalk(stmt)
}

// fts5TableCannotScan reports whether the FROM item names an fts5 table whose
// zContent C fts5 would leave NULL -- see fts5ContentlessScanGuard.
func (p *ReadOnlyPager) fts5TableCannotScan(it FromItem) bool {
	if it.Table == "" {
		return false
	}
	sch, ok := p.fts5SchemaTok(it.Table)
	return ok && sch.cl != nil && !sch.contentUnindexed && !sch.columnsize
}

// fts5ScanGuardWalk applies fts5ContentlessScanGuard's rule to every SelectStmt
// node in the tree: each node's own FROM against its own WHERE. An
// EXPRESSION subquery (a scalar/EXISTS/IN one, anywhere) naming such a table is
// refused outright rather than analyzed -- it is rare, and the alternative is a
// second copy of exprTreeMentionsTable's Expr switch.
func (p *ReadOnlyPager) fts5ScanGuardWalk(stmt *SelectStmt) error {
	if stmt == nil {
		return nil
	}
	for _, it := range stmt.From {
		if it.Subquery != nil {
			if err := p.fts5ScanGuardWalk(it.Subquery); err != nil {
				return err
			}
			continue
		}
		if !p.fts5TableCannotScan(it) {
			continue
		}
		if !fts5FromHasItem(stmt.From, it) || !fts5StmtMatchesTable(stmt, it) {
			return fmt.Errorf("%s: table does not support scanning", it.Table)
		}
	}
	for _, cte := range stmt.CTEs {
		if err := p.fts5ScanGuardWalk(cte.Select); err != nil {
			return err
		}
	}
	for _, arm := range stmt.Compound {
		if err := p.fts5ScanGuardWalk(arm.Stmt); err != nil {
			return err
		}
	}
	// Everything a FROM item's own walk above did NOT cover: a JOIN ON, the
	// select list, WHERE/HAVING/GROUP BY/ORDER BY. exprTreeMentionsTable only
	// reports a table named in a SUBQUERY's FROM, so a plain "t MATCH ..." in
	// this node's WHERE is not a hit here.
	var hit string
	catch := func(it FromItem) bool {
		if p.fts5TableCannotScan(it) {
			hit = it.Table
			return true
		}
		return false
	}
	for _, it := range stmt.From {
		if exprTreeMentionsTable(it.On, catch) {
			return fmt.Errorf("%s: table does not support scanning", hit)
		}
	}
	for _, c := range stmt.Columns {
		if exprTreeMentionsTable(c.Expr, catch) {
			return fmt.Errorf("%s: table does not support scanning", hit)
		}
	}
	if exprTreeMentionsTable(stmt.Where, catch) || exprTreeMentionsTable(stmt.Having, catch) {
		return fmt.Errorf("%s: table does not support scanning", hit)
	}
	for _, e := range stmt.GroupBy {
		if exprTreeMentionsTable(e, catch) {
			return fmt.Errorf("%s: table does not support scanning", hit)
		}
	}
	for _, ot := range stmt.OrderBy {
		if exprTreeMentionsTable(ot.Expr, catch) {
			return fmt.Errorf("%s: table does not support scanning", hit)
		}
	}
	return nil
}

// fts5FromHasItem reports whether from holds exactly one reference to it, by the
// name and alias the guard above matched on.
func fts5FromHasItem(from []FromItem, it FromItem) bool {
	n := 0
	for _, f := range from {
		if f.Subquery == nil && strings.EqualFold(f.Table, it.Table) && strings.EqualFold(f.Alias, it.Alias) {
			n++
		}
	}
	return n == 1
}

// fts5StmtMatchesTable reports whether stmt gives the FROM item it a MATCH real
// fts5's xBestIndex would have received: a top-level AND conjunct of its own
// WHERE spelled "<name> MATCH ..." or "<name>.<col> MATCH ...", or the
// table-valued call form "FROM t(...)" that fts5_tablefunc.go turns into one.
// A negated MATCH does not count -- fts5 never sees a constraint under NOT.
func fts5StmtMatchesTable(stmt *SelectStmt, it FromItem) bool {
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	for _, f := range stmt.From {
		if f.TableFunc && strings.EqualFold(f.Table, it.Table) && len(f.TableFuncArgs) > 0 {
			return true
		}
	}
	for _, cj := range fts3AndConjuncts(stmt.Where) {
		m, ok := cj.(MatchExpr)
		if !ok || m.Not {
			continue
		}
		ce, ok := m.X.(ColumnExpr)
		if !ok {
			continue
		}
		// "t MATCH ..." names the table itself; "t.c MATCH ..." is fts5's
		// single-column form, which reaches xBestIndex as the same constraint.
		if ce.Qualifier == "" && strings.EqualFold(ce.Name, name) {
			return true
		}
		if strings.EqualFold(ce.Qualifier, name) {
			return true
		}
	}
	return false
}

// fts5ContentlessDecline is the set of contentless shapes this engine does not
// serve, checked at CREATE (and again whenever a stored CREATE is re-parsed, so
// a table C SQLite wrote declines the same way). See this file's comment for
// what each one would take.
func fts5ContentlessDecline(opts fts5Options, unindexed []bool) error {
	// detail= below full is not refused at CREATE: creating and writing such a
	// table is reproducible, and only reading documents back out of the index
	// needs positions. fts5ContentlessDocsOf refuses at read instead.
	return nil
}

// fts5DetailSpelling is a detail= mode's own name, for an error message.
func fts5DetailSpelling(d fts5Detail) string {
	for _, n := range fts5DetailNames {
		if n.val == d {
			return n.name
		}
	}
	return "full"
}

// fts5ContentlessRowSourceGuard is the contentless counterpart of
// fts5ExtRowSourceGuard, run before a DELETE or UPDATE (fts5SecureDeleteGuard).
//
// C refuses both per row, so a statement selecting nothing succeeds -- the row
// set is load-bearing. This engine cannot compute it when the WHERE has a
// MATCH: the write path's rows are all NULL, and the document map that answers
// a MATCH is not reachable from the vtab write path's evalCtx (no db, no
// pager; deleteVtab / updateVtab).
//
// So selecting no rows from a table that has documents is declined: it could be
// a real empty selection or a MATCH evaluated blind. One or more rows needs no
// guard (DeleteRow/UpdateRow give fts5's own message), and an empty table has
// no row set to get wrong.
//
// For contentless_delete=1 this declines statements C performs. Giving those
// evalCtx literals the db would remove the guard: evalMatch's
// fts5ContentlessDocsFor already has a write-path arm.
func fts5ContentlessRowSourceGuard(vm *vtabMeta, nRemoved int) error {
	st, ok := vm.store.(*fts5Store)
	if !ok || !st.contentless || nRemoved > 0 || len(st.rows) == 0 {
		return nil
	}
	return fmt.Errorf("engine: a DELETE or UPDATE of the contentless fts5 table %s that selects no row is not supported by this engine: C fts5 refuses one that selects a row and succeeds when none is selected, and this write path cannot reach the documents that decide which rows a MATCH over a contentless table selects", st.name)
}

// fts5UpdateRemovesRows is how many of the nSelected rows an UPDATE actually
// removes from the index -- all of them, except a contentless table whose SET
// names no indexed column and does not move the rowid (UpdateRowSet, fts5's
// bSeenIndex==0 arm), which rewrites only %_content. Visible through
// 'secure-delete', whose %_config version bump counts removals.
func fts5UpdateRemovesRows(vm *vtabMeta, modified []bool, nSelected int) int {
	st, ok := vm.store.(*fts5Store)
	if !ok || !st.contentless || nSelected == 0 {
		return nSelected
	}
	if len(modified) > 0 && modified[0] {
		// The rowid is in the SET list; whether it CHANGES is per row, and a
		// row that moves is a delete and a re-insert. Counting it as a removal
		// is the conservative side of a bump that is sticky either way.
		return nSelected
	}
	for i := range st.colNames {
		if !st.unindexed[i] && i+1 < len(modified) && modified[i+1] {
			return nSelected
		}
	}
	return 0
}

// fts5ContentlessDocs is a contentless fts5 table's document set, read back out
// of its own index: the indexed rowids in ascending order (which is %_docsize's
// row set, C fts5's own row source for a scan) and each one's tokenized
// columns.
type fts5ContentlessDocs struct {
	rowids []int64
	tokens map[int64]fts5RowTokens
	// origins and originCntr are the contentless_delete=1 origin state read
	// back out of %_docsize's third column and the structure record, and
	// avgRow/avgSizes its averages record, which counts deleted documents too
	// (fts5Store.avgRow). All four are zero-valued on a plain contentless table,
	// which has no origins and whose averages the rebuild derives.
	origins           map[int64]int64
	originCntr        int64
	avgRow            int64
	avgSizes          []int64
	contentlessDelete bool
	// ghosts are rowids the index holds postings for with NO %_docsize row --
	// what a mismatched 'delete' command leaves (fts5ContentlessGhost). Their
	// tokens are in tokens with a hole mask; they are NOT in rowids, which is
	// the scan's row source, exactly as C fts5's scan skips them.
	ghosts map[int64]bool
}

// fts5ContentlessCheck memoizes one contentless table's index read for a read
// snapshot, the way fts5ExtCheck memoizes external content's verification: the
// first query that needs the documents pays for the decode, the rest of the
// snapshot's reads share it.
type fts5ContentlessCheck struct {
	done bool
	err  error
	docs *fts5ContentlessDocs
}

// fts5ContentlessRead reconstructs the documents of contentless fts5 table name
// from its %_data index and %_docsize row set. See this file's comment for why
// the reconstruction is exact and how it is checked.
func fts5ContentlessRead(rp *ReadOnlyPager, st *fts5Store, name string) (*fts5ContentlessDocs, error) {
	sh, err := rp.fts5ExtShadowsOf(name, st.columnsize)
	if err != nil {
		return nil, err
	}
	postings, segs, err := fts5DecodePostingsStructure(sh.data, name+"_data", st.detail)
	if err != nil {
		return nil, err
	}
	nCol := len(st.colNames)
	docs := &fts5ContentlessDocs{
		tokens:            make(map[int64]fts5RowTokens, len(sh.docsize)),
		origins:           make(map[int64]int64, len(sh.origins)),
		originCntr:        fts5OriginCounter(segs),
		contentlessDelete: st.contentlessDelete,
	}
	for id, o := range sh.origins {
		docs.origins[id] = o
	}
	docs.avgRow, docs.avgSizes = fts5decodeAveragesLenient(sh.data[fts5AveragesRowid], nCol)
	// %_docsize holds exactly one row per indexed document (fts5_storage.c's
	// fts5StorageInsertDocsize / FTS5_STMT_DELETE_DOCSIZE), so its rowids ARE
	// the document set -- including a document that tokenized to nothing at all
	// and therefore has no posting anywhere.
	if st.columnsize {
		for id, blob := range sh.docsize {
			sz, ok := fts5ExtDocsizeSizes(blob, nCol)
			if !ok {
				return nil, fmt.Errorf("engine: fts5 table %s: its %%_docsize row for rowid %d does not decode", name, id)
			}
			// Each column's token slots, sized by what %_docsize says the document
			// holds. The empty string is the "not filled yet" marker below, which no
			// real token can collide with: fts5's tokenizers never emit a
			// zero-length token, so a term that is nothing but its term-space byte
			// only appears in a hand-corrupted index -- where it is reported as the
			// hole it leaves rather than accepted.
			cols := make([][]string, nCol)
			for i := 0; i < nCol; i++ {
				if sz[i] > 0 {
					cols[i] = make([]string, sz[i])
				}
			}
			docs.tokens[id] = fts5RowTokens{cols: cols}
			docs.rowids = append(docs.rowids, id)
		}
	} else {
		// A columnsize=0 CONTENTLESS_UNINDEXED table still HAS a %_content (it
		// is FTS5_CONTENT_UNINDEXED, fts5_config.c:690), and that is C fts5's
		// own row source for it -- so its rowids come from there and only the
		// per-column sizes from the postings. Every other columnsize=0 table has
		// nothing but the postings.
		var seed []uint64
		if st.contentUnindexed {
			ids, _, cerr := rp.Rows(name + "_content")
			if cerr != nil {
				return nil, cerr
			}
			seed = ids
		}
		fts5ContentlessSlotsFromPostings(docs, postings, nCol, seed)
	}
	sort.Slice(docs.rowids, func(i, j int) bool { return docs.rowids[i] < docs.rowids[j] })

	for key, positions := range postings {
		// Only the MAIN term space carries the documents; a prefix index is the
		// same postings keyed by a truncated token (fts5_index.go's
		// fts5BuildIndex), and re-deriving it from the tokens is exactly what
		// the re-encode does.
		if len(key.term) == 0 || key.term[0] != fts5MainPrefix {
			continue
		}
		tok := key.term[1:]
		row, ok := docs.tokens[key.rowid]
		if !ok {
			// Postings for a rowid %_docsize has no row for: a GHOST, if this
			// is a table kind a mismatched 'delete' can leave one in (see
			// fts5ContentlessGhostable). Its slots grow to fit the postings as
			// they arrive, every position with no posting stays a hole, and it
			// joins tokens but not rowids. Any other kind still refuses:
			// unreachable on a columnsize=0 table, where the slot table was
			// built from these very postings.
			if !fts5ContentlessGhostable(st) {
				return nil, fmt.Errorf("engine: fts5 table %s: its index holds postings for rowid %d, which %%_docsize has no row for", name, key.rowid)
			}
			row = fts5RowTokens{cols: make([][]string, nCol), holes: make([][]bool, nCol)}
			if docs.ghosts == nil {
				docs.ghosts = map[int64]bool{}
			}
			docs.ghosts[key.rowid] = true
			docs.tokens[key.rowid] = row
		}
		if docs.ghosts[key.rowid] {
			for _, at := range positions {
				col := int(at >> 32)
				off := int(at & 0xffffffff)
				if col < 0 || col >= nCol || off < 0 {
					return nil, fmt.Errorf("engine: fts5 table %s: its index puts a token of rowid %d at column %d position %d, which is out of range", name, key.rowid, col, off)
				}
				for len(row.cols[col]) <= off {
					row.cols[col] = append(row.cols[col], "")
					row.holes[col] = append(row.holes[col], true)
				}
				if !row.holes[col][off] {
					return nil, fmt.Errorf("engine: fts5 table %s: its index puts two tokens of rowid %d at column %d position %d, so the document it was built from is not recoverable", name, key.rowid, col, off)
				}
				row.cols[col][off] = tok
				row.holes[col][off] = false
			}
			docs.tokens[key.rowid] = row
			continue
		}
		for _, at := range positions {
			col := int(at >> 32)
			off := int(at & 0xffffffff)
			if col < 0 || col >= nCol || off < 0 || off >= len(row.cols[col]) {
				return nil, fmt.Errorf("engine: fts5 table %s: its index puts a token of rowid %d at column %d position %d, which its %%_docsize row does not describe", name, key.rowid, col, off)
			}
			if row.cols[col][off] != "" {
				return nil, fmt.Errorf("engine: fts5 table %s: its index puts two tokens of rowid %d at column %d position %d, so the document it was built from is not recoverable", name, key.rowid, col, off)
			}
			row.cols[col][off] = tok
		}
	}
	// Every slot %_docsize accounted for must have been filled: a hole means the
	// index and %_docsize disagree about the document, and re-encoding what was
	// read would silently rewrite the index. On a columnsize=0 table the slots
	// come from the postings themselves, so a hole means the index holds a token
	// at position N of a column and none at some position below it -- equally
	// unrecoverable, and equally a refusal.
	for rid, row := range docs.tokens {
		if docs.ghosts[rid] {
			continue // a ghost's holes are the tombstones themselves
		}
		for col, toks := range row.cols {
			for off, t := range toks {
				if t == "" {
					if !st.columnsize {
						return nil, fmt.Errorf("engine: fts5 table %s: its index holds %d tokens in column %d of rowid %d and none at position %d, so the document it was built from is not recoverable", name, len(toks), col, rid, off)
					}
					return nil, fmt.Errorf("engine: fts5 table %s: its %%_docsize row for rowid %d counts %d tokens in column %d and its index holds no token at position %d", name, rid, len(toks), col, off)
				}
			}
		}
	}
	// A columnsize=0 table's document set is recoverable only as far as the
	// postings go: a document that tokenized to NOTHING at all leaves no posting
	// and, with no %_docsize, no other trace either. Real fts5 still counts it in
	// the averages record, so that record is what says whether any are missing --
	// and if they are, the documents this engine would re-encode are not the ones
	// the index was built from. Refuse rather than silently drop them.
	if !st.columnsize && !st.contentUnindexed && docs.avgRow != int64(len(docs.rowids)) {
		return nil, fmt.Errorf("engine: fts5 table %s: its averages record counts %d documents and its index holds postings for %d, so the %d that tokenized to nothing cannot be recovered from a columnsize=0 table (there is no %%_docsize to name them)", name, docs.avgRow, len(docs.rowids), docs.avgRow-int64(len(docs.rowids)))
	}
	// A ghost is taken back only in the state this engine writes one: C fts5
	// subtracted the GIVEN values' sizes from the averages record
	// (fts5_storage.c:577-594), and this engine derives that record from the
	// %_docsize documents, so the two agree only when the given counts equaled
	// the document's -- fts5ContentlessGhost's own condition. A file whose record
	// differs (a mismatched 'delete' naming a different number of tokens, written
	// by C fts5) would have that record silently rewritten by the next
	// re-encode here; refuse it instead.
	if len(docs.ghosts) > 0 {
		sizes := make([]int64, nCol)
		for _, rid := range docs.rowids {
			for i, toks := range docs.tokens[rid].cols {
				sizes[i] += int64(len(toks))
			}
		}
		if docs.avgRow != int64(len(docs.rowids)) || !slices.Equal(sizes, docs.avgSizes) {
			return nil, fmt.Errorf("engine: fts5 table %s: it holds a document a mismatched 'delete' command left partly indexed, and its averages record (%d rows, sizes %v) is not the one its %%_docsize rows add up to (%d rows, sizes %v), so the 'delete' named a different number of tokens than the document held", name, docs.avgRow, docs.avgSizes, len(docs.rowids), sizes)
		}
	}
	return docs, nil
}

// fts5ContentlessGhostable reports whether table kind st can hold a ghost
// (fts5ContentlessDocs.ghosts): a plain contentless table with %_docsize and
// no prefix index. columnsize=0 has no %_docsize row to be missing,
// contentless_delete refuses 'delete', and contentless_unindexed has a
// %_content row a ghost would need to account for.
//
// A prefix index is excluded because the hole mask cannot describe it: the
// tombstone drops a whole (key, rowid) entry, and naming 'abc' drops the 'ab'
// prefix entry even while 'abd' survives (fts5SetupPrefixIter,
// fts5_index.c:6736-6745, 7445-7449). So C misses the ghost for 'ab*' but
// finds it for 'abd'.
//
// All four keep the refusal: fts5ContentlessGhost declines to create a ghost,
// and fts5ContentlessRead refuses a file holding one.
func fts5ContentlessGhostable(st *fts5Store) bool {
	return st.contentless && st.columnsize && !st.contentlessDelete && !st.contentUnindexed && len(st.prefixes) == 0
}

// fts5ContentlessSlotsFromPostings sizes each document's per-column slots from
// the postings, which is all a columnsize=0 table has (no %_docsize). For
// detail=full a column's offsets are exactly 0..szCol-1, so the highest seen
// is szCol-1; the caller fills every slot and refuses any hole, keeping it a
// checked reconstruction.
func fts5ContentlessSlotsFromPostings(docs *fts5ContentlessDocs, postings map[fts5mergeKey][]int64, nCol int, seed []uint64) {
	maxOff := map[int64][]int{}
	newRow := func() []int {
		got := make([]int, nCol)
		for i := range got {
			got[i] = -1
		}
		return got
	}
	// seed is a rowid set known independently of the postings (a
	// contentless_unindexed=1 table's %_content). A rowid in it with no posting
	// at all is a real document of zero indexed tokens, which nothing else here
	// could have recovered.
	for _, id := range seed {
		maxOff[int64(id)] = newRow()
	}
	for key, positions := range postings {
		if len(key.term) == 0 || key.term[0] != fts5MainPrefix {
			continue
		}
		got, ok := maxOff[key.rowid]
		if !ok {
			got = newRow()
			maxOff[key.rowid] = got
		}
		for _, at := range positions {
			col := int(at >> 32)
			off := int(at & 0xffffffff)
			if col < 0 || col >= nCol || off < 0 {
				// Left for the fill loop to report against the slot table it
				// could not build: a position outside the declared columns is a
				// corrupt index, not a sizing question.
				continue
			}
			if off > got[col] {
				got[col] = off
			}
		}
	}
	for rid, got := range maxOff {
		cols := make([][]string, nCol)
		for i := 0; i < nCol; i++ {
			if got[i] >= 0 {
				cols[i] = make([]string, got[i]+1)
			}
		}
		docs.tokens[rid] = fts5RowTokens{cols: cols}
		docs.rowids = append(docs.rowids, rid)
	}
}

// fts5LoadContentless is fts5LoadStore for a contentless table: it fills the
// store's rows (one all-NULL row per indexed document -- what C fts5's
// xColumn returns for one) and its TOKEN CACHE, which for a contentless table
// is not a cache at all but the documents themselves, since there is nothing to
// re-tokenize them from.
//
// Like fts5LoadExternal it never fails OpenWrite: a table this engine cannot
// read must not take the session's unrelated statements down with it.
func fts5LoadContentless(rp *ReadOnlyPager, st *fts5Store, name string) {
	st.name = name
	docs, err := fts5ContentlessRead(rp, st, name)
	if err != nil {
		st.contentlessErr = err
		return
	}
	nCol := len(st.colNames)
	st.tokens = make(map[int64]fts5RowTokens, len(docs.rowids)+len(docs.ghosts))
	for _, rid := range docs.rowids {
		st.rows[rid] = fts5ContentlessRow(nCol)
		st.tokens[rid] = docs.tokens[rid]
	}
	// A ghost read back out of the file (fts5ContentlessDocs.ghosts) is held
	// exactly as the command that made it left it -- in tokens, not in rows --
	// so a re-encode reproduces its postings, and the table is frozen for
	// writes until a 'delete-all' as it is in the session that made it.
	for rid := range docs.ghosts {
		st.tokens[rid] = docs.tokens[rid]
		st.clGhosted = true
	}
	if uerr := fts5ContentlessLoadUnindexed(rp, st, name); uerr != nil {
		st.contentlessErr = uerr
	}
	if st.contentlessDelete {
		st.origins = make(map[int64]int64, len(docs.origins))
		for rid, o := range docs.origins {
			if _, live := st.rows[rid]; live {
				st.origins[rid] = o
			}
		}
		st.originCntr = docs.originCntr
		st.originOpen = false
		st.avgRow, st.avgSizes = docs.avgRow, docs.avgSizes
	}
}

// fts5ContentlessLoadUnindexed overlays the UNINDEXED columns of a
// contentless_unindexed=1 table onto the all-NULL rows the index reconstruction
// produced. Those columns are the one part of such a table that IS stored: its
// %_content holds a c<i> for each of them and nothing else, in the original
// column order, so reading it back is positional.
//
// A row %_content has no entry for keeps its NULLs. Real fts5 cannot produce
// one -- fts5StorageInsert writes %_content and the index together -- but a
// hand-built or truncated file can, and answering NULL there is what
// fts5ColumnMethod's own missing-row behaviour amounts to.
func fts5ContentlessLoadUnindexed(rp *ReadOnlyPager, st *fts5Store, name string) error {
	if !st.contentUnindexed {
		return nil
	}
	rowids, records, err := rp.Rows(name + "_content")
	if err != nil {
		return err
	}
	stored := make([]int, 0, len(st.colNames))
	for i := range st.colNames {
		if st.unindexed[i] {
			stored = append(stored, i)
		}
	}
	for i, rid := range rowids {
		vals, ok := st.rows[int64(rid)]
		if !ok {
			continue
		}
		// records[i] is [id, c<stored[0]>, c<stored[1]>, ...].
		for j, col := range stored {
			if j+1 < len(records[i]) {
				vals[col] = records[i][j+1]
			}
		}
	}
	return nil
}

// fts5ContentlessFlush is the contentless_delete=1 bookkeeping not derivable
// from the live documents: it gives each new document the origin of the
// segment about to be written, adds it to the averages totals, and advances the
// origin counter. It runs once per fts5SyncShadows. fts5's rules:
//
//   - an origin is taken only when a segment is actually written
//     (fts5FlushOneHash, inside "if( sqlite3Fts5HashIsEmpty(pHash)==0 )"), so
//     rows that tokenize to nothing and DELETE-only statements leave the
//     counter alone;
//   - a flush happens once per transaction, so rows inserted in one explicit
//     transaction share an origin (originOpen);
//   - the totals only grow: fts5StorageContentlessDelete does not decrement
//     them, so a deleted document keeps counting.
func fts5ContentlessFlush(st *fts5Store, docs []fts5IndexDoc, inTxn bool) {
	if !st.contentlessDelete {
		return
	}
	if st.origins == nil {
		st.origins = map[int64]int64{}
	}
	if len(st.avgSizes) != len(st.colNames) {
		st.avgSizes = make([]int64, len(st.colNames))
	}
	if st.originCntr < 1 {
		st.originCntr = 1
	}
	// The previous transaction's flush closed when it committed; its origin is
	// spent even though no statement of this one has run yet.
	if st.originOpen && !inTxn {
		st.originCntr++
		st.originOpen = false
	}
	fresh := false
	hasPosting := false
	for _, d := range docs {
		if _, ok := st.origins[d.rowid]; !ok {
			st.origins[d.rowid] = st.originCntr
			st.avgRow++
			for i, col := range d.cols {
				if i < len(st.avgSizes) {
					st.avgSizes[i] += int64(len(col))
				}
			}
			fresh = true
		}
		if !hasPosting && st.origins[d.rowid] == st.originCntr {
			for _, col := range d.cols {
				if len(col) > 0 {
					hasPosting = true
					break
				}
			}
		}
	}
	if !fresh || !hasPosting {
		return
	}
	if inTxn {
		st.originOpen = true
		return
	}
	st.originCntr++
}

// fts5ContentlessV2 is the V2 structure state to write for st, or nil when the
// table is not a contentless_delete=1 one. nDoc is the number of documents the
// segment holds.
//
// The segment spans origins 1..(counter-1) because this engine writes exactly
// one of them and it holds every live document, whatever origin each was given:
// sqlite3Fts5IndexContentlessDelete tombstones a row in every segment whose
// range CONTAINS the row's origin, so a range that missed one would leave that
// row undeletable by C SQLite. A table that has never flushed a segment has
// counter 1 and no segment at all, and its record is the bare marker plus three
// zeros -- byte-identical to the oracle's x'00000000FF000001000000'.
func fts5ContentlessV2(st *fts5Store, nDoc int) *fts5StructV2 {
	if !st.contentlessDelete {
		return nil
	}
	// counter-1 covers every document that HAS a posting, and only those need
	// covering. A document whose origin equals the counter is one the flush that
	// gave it that origin did not write a segment for, which (see
	// fts5ContentlessFlush) happens only when nothing in that batch tokenized to
	// anything -- so there is no posting to tombstone, and C fts5 leaves such
	// a %_docsize origin outside every segment's range too. Clamping the range
	// UP to cover it instead was measurably wrong: it made the reopened counter
	// one too high, and every origin after it.
	hi := st.originCntr - 1
	if hi < 1 {
		hi = 1
	}
	return &fts5StructV2{origin1: 1, origin2: uint64(hi), nEntry: uint64(nDoc)}
}

// fts5ContentlessRow is one contentless row's stored values: all NULL, because
// fts5ColumnMethod returns nothing for every user column of such a table.
func fts5ContentlessRow(nCol int) []Value {
	vals := make([]Value, nCol)
	for i := range vals {
		vals[i] = Value{Typ: Null}
	}
	return vals
}

// fts5ContentlessStoreOf rebuilds a contentless fts5 table's store out of this
// read snapshot. nil with a nil error means name is not a contentless fts5
// table here.
func (p *ReadOnlyPager) fts5ContentlessStoreOf(name string) (*fts5Store, error) {
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
		if !st.contentless {
			return nil, nil
		}
		return st, nil
	}
	return nil, nil
}

// fts5ContentlessDocsOf reads the documents of contentless fts5 table name out
// of this snapshot, memoized on the snapshot's schema entry so a statement that
// scans and MATCHes decodes the index once.
func (p *ReadOnlyPager) fts5ContentlessDocsOf(name string) (*fts5ContentlessDocs, error) {
	sch, ok := p.fts5SchemaTok(name)
	if !ok || sch.cl == nil {
		return nil, nil
	}
	if !sch.cl.done {
		sch.cl.done = true
		switch {
		case sch.detail != fts5DetailFull:
			// The one shape the reconstruction cannot do, moved here from
			// CREATE: rebuilding a contentless table's documents out of its
			// own index needs detail=full's gapless 0..szCol-1 token
			// numbering. detail=none records no positions and detail=columns
			// only the column, so the rebuild would be a GUESS -- declined
			// rather than guessed, while CREATE and every WRITE go through.
			sch.cl.err = fmt.Errorf("%w: fts5: reading a CONTENTLESS table back requires detail=full, and %s is detail=%s: its index records no positions to rebuild the documents from", errVDBEUnsupported, name, fts5DetailSpelling(sch.detail))
		default:
			st, err := p.fts5ContentlessStoreOf(name)
			if err != nil {
				sch.cl.err = err
			} else if st != nil {
				sch.cl.docs, sch.cl.err = fts5ContentlessRead(p, st, name)
			}
		}
	}
	return sch.cl.docs, sch.cl.err
}

// materializeFts5Contentless presents a contentless fts5 table's rows: one
// all-NULL row per indexed document, in rowid order. That is exactly what real
// fts5's scan of such a table produces -- FTS5_STMT_SCAN_ASC over %_docsize
// selecting nothing but "T.rowid" (fts5_config.c's fts5ConfigMakeExprlist),
// with fts5ColumnMethod declining to fill in any column.
func (p *ReadOnlyPager) materializeFts5Contentless(it FromItem, name string, cols []columnInfo) ([]columnInfo, [][]Value, []int64, error) {
	docs, err := p.fts5ContentlessDocsOf(name)
	if err != nil {
		return nil, nil, nil, err
	}
	if docs == nil {
		return nil, nil, nil, fmt.Errorf("engine: internal error: fts5 table %s is not contentless in this snapshot", name)
	}
	// A GHOST (fts5ContentlessDocs.ghosts) has postings and no %_docsize row,
	// and C fts5's two row sources disagree about it: a MATCH is answered
	// from the INDEX and returns it, while every other read -- a scan, count(*),
	// even "WHERE rowid=N" -- walks %_docsize and does not. Measured on the
	// oracle over one ghost: MATCH 'q' returns it, "SELECT rowid FROM tt" and
	// "WHERE rowid=2" do not.
	//
	// Leaving a ghost OUT of a MATCH C fts5 answers from the index would be
	// a missing row -- a wrong answer, not a decline -- so only a MATCH this
	// can prove targets this item takes it in, a statement with no MATCH at all
	// is an ordinary scan and leaves it out, and anything else declines.
	rowids := docs.rowids
	if len(docs.ghosts) > 0 {
		switch fts5ClassifyMatch(it) {
		case fts5MatchesItem:
			rowids = append([]int64(nil), docs.rowids...)
			for rid := range docs.ghosts {
				rowids = append(rowids, rid)
			}
			sort.Slice(rowids, func(i, j int) bool { return rowids[i] < rowids[j] })
		case fts5MatchUnclassified:
			return nil, nil, nil, fmt.Errorf("engine: fts5 table %s holds a document a mismatched 'delete' command left partly indexed, which C fts5 returns from a MATCH on this table and from nothing else; this read's MATCH is not one this engine can place, so it is declined rather than risk omitting that row", name)
		}
	}
	rows := make([][]Value, 0, len(rowids))
	byRowid := make(map[int64][]Value, len(rowids))
	for _, rid := range rowids {
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: rid}
		for i := 1; i < len(row); i++ {
			row[i] = Value{Typ: Null}
		}
		byRowid[rid] = row
		rows = append(rows, row)
	}
	// ...with the UNINDEXED columns of a contentless_unindexed=1 table filled
	// in from its %_content, which holds exactly those.
	if err := p.fts5ContentlessFillUnindexed(name, cols, byRowid); err != nil {
		return nil, nil, nil, err
	}
	return cols, rows, rowids, nil
}

// fts5MatchDrive is how a statement's WHERE reaches one fts5 FROM item.
type fts5MatchDrive int

const (
	fts5NoMatch           fts5MatchDrive = iota // no MATCH anywhere: an ordinary scan
	fts5MatchesItem                             // a top-level MATCH provably naming this item
	fts5MatchUnclassified                       // any other MATCH, or no statement to read
)

// fts5ClassifyMatch classifies it's statement for materializeFts5Contentless.
// Only a top-level AND conjunct "<item> MATCH ..." -- the table-named column
// bare (fts5 forbids a column sharing its table's name) or qualified by the
// item's alias or table -- counts as naming it; a bare ordinary column could
// belong to another FROM item, a MATCH under OR is not a constraint fts5 sees,
// and a MATCH naming another item says nothing about this one. All of those,
// and a row source never handed the statement (nil tvfWhere, withVtabWhere's
// convention), are fts5MatchUnclassified.
func fts5ClassifyMatch(it FromItem) fts5MatchDrive {
	if it.tvfWhere == nil {
		return fts5MatchUnclassified
	}
	matched, other := false, false
	for _, c := range it.tvfWhere {
		if mx, ok := c.(MatchExpr); ok && !mx.Not {
			if ce, ok := mx.X.(ColumnExpr); ok && ce.Schema == "" &&
				((ce.Qualifier == "" && equalFoldName(ce.Name, it.Table)) ||
					(ce.Qualifier != "" && (equalFoldName(ce.Qualifier, it.Alias) || equalFoldName(ce.Qualifier, it.Table)))) {
				matched = true
				continue
			}
			other = true
			continue
		}
		if fts5ExprHasMatch(c) {
			other = true
		}
	}
	switch {
	case other:
		return fts5MatchUnclassified
	case matched:
		return fts5MatchesItem
	}
	return fts5NoMatch
}

// fts5ExprHasMatch reports whether e contains a MATCH anywhere outside a
// subquery (whose own FROM it would belong to).
func fts5ExprHasMatch(e Expr) bool {
	if e == nil {
		return false
	}
	if _, ok := e.(MatchExpr); ok {
		return true
	}
	found := false
	walkExprOperands(e, func(sub Expr) {
		if !found && fts5ExprHasMatch(sub) {
			found = true
		}
	})
	return found
}

// fts5ContentlessFillUnindexed overlays a contentless_unindexed=1 table's
// stored columns onto the rows materializeFts5Contentless built. It is the read
// snapshot's twin of fts5ContentlessLoadUnindexed, and does nothing for any
// other contentless table (whose %_content does not exist).
//
// cols is the DECLARED schema -- [rowid HIDDEN, col1, ...] -- so a text column's
// slot is its fts5 column index plus one, and only the UNINDEXED ones are
// present in %_content, in order.
func (p *ReadOnlyPager) fts5ContentlessFillUnindexed(name string, cols []columnInfo, byRowid map[int64][]Value) error {
	st, err := p.fts5ContentlessStoreOf(name)
	if err != nil || st == nil || !st.contentUnindexed {
		return err
	}
	rowids, records, rerr := p.Rows(name + "_content")
	if rerr != nil {
		return rerr
	}
	stored := make([]int, 0, len(st.colNames))
	for i := range st.colNames {
		if st.unindexed[i] {
			stored = append(stored, i)
		}
	}
	for i, rid := range rowids {
		row, ok := byRowid[int64(rid)]
		if !ok {
			continue
		}
		for j, col := range stored {
			if j+1 < len(records[i]) && col+1 < len(row) {
				row[col+1] = records[i][j+1]
			}
		}
	}
	return nil
}
