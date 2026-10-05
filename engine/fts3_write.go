// This file implements DELETE and UPDATE on an fts3/fts4 table. C fts3 does not
// rewrite the index when a row goes away: it appends a segment holding a delete
// marker for each of the row's terms, and the reader (fts3MergeDoclist)
// resolves each (term, docid) newest-segment-first, dropping it when the
// newest entry has no positions. A wrong encoding here makes a database C
// reads differently, so compat-harness/fts3_shadow_test.go gates both
// directions.
//
// # A delete marker
//
// A doclist entry with a docid delta and nothing but POS_END
// (fts3PendingListAppend with iCol == -1, from fts3DeleteTerms). Deleting
// docid 1 ('aa bb' / 'cc') from a two-column fts4 table adds
//
//	00 | 02 "aa" 02 <01 00> | 00 02 "bb" 02 <01 00> | 00 02 "cc" 02 <01 00>
//
// one entry per term of the deleted row, in term order.
//
// # One statement is one segment
//
// C accumulates pending terms and flushes at fts3PendingTermsDocid's points: a
// docid not greater than the previous, or an equal one that is not the insert
// half of the delete just recorded. A multi-row DELETE/UPDATE visits rows in
// ascending rowid order (ONEPASS_MULTI is off for vtabs, so a RowSet drains
// smallest-first), so neither fires: "DELETE FROM t WHERE docid IN (3,1)"
// writes one segment holding both rows' markers.
//
// # An UPDATE is a delete and an insert into the same segment
//
// No flush between an UPDATE's halves (same docid, previous op the delete), so
// a term both rows share becomes an ordinary entry: "UPDATE t SET a=a WHERE
// docid=4" writes a segment byte-identical to the original INSERT's.
//
// # Emptying the table wipes everything
//
// fts3DeleteByRowid asks fts3IsEmpty whether this is the last row and then
// calls fts3DeleteAll: %_content, %_segments, %_segdir, %_docsize and %_stat
// are emptied and pending terms dropped. So "DELETE FROM t" leaves no index,
// and an UPDATE of the only row restarts %_segdir at idx 0.
//
// # Declined
//
//   - a MATCH in the WHERE clause;
//   - RETURNING, a subquery in WHERE or SET, and every OR-conflict clause but
//     OR REPLACE (the one mode fts3 resolves itself, see updateFts3);
//   - a multi-row UPDATE of an "order=desc" table with a WHERE (see below);
//   - a new segment that would spill past the node size, or a %_segdir already
//     holding fts3MergeCount level-0 segments -- INSERT's limits.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts3SetRowidSynonymNoOp is updateFts3's setIdx sentinel for "SET
// rowid=.../oid=.../_rowid_=...": a SILENT NO-OP on an fts3/fts4 table (see
// updateFts3's own comment for the oracle evidence). -2, not -1: -1 is
// "no such column yet" while this loop is still searching, and the sentinel
// must be distinguishable from that or every no-op SET would misreport
// itself as unresolved. Every real row slot is >= 0.
const fts3SetRowidSynonymNoOp = -2

// fts3ContentRows returns the fts3 table's rows as the virtual table presents
// them -- [docid, col0, ...] -- in ASCENDING docid order, which is the order
// C SQLite's DELETE and UPDATE visit them in (see this file's comment).
// %_content stores the docid in its rowid key, so slot 0 of the stored record
// is NULL and the docid is filled in here.
func fts3ContentRows(content *tableMeta, sch fts3Schema) ([]int64, [][]Value, error) {
	nCol := len(sch.cols)
	width := nCol + 1
	if sch.langid != "" {
		// One more slot for the LANGUAGE column, which a "languageid=" table
		// keeps past its user columns in %_content and exposes as a trailing
		// hidden column (fts3_langid.go).
		width++
	}
	keys := make([]uint64, 0, content.rows.len())
	for rid := range content.rows.all() {
		keys = append(keys, rid)
	}
	sort.Slice(keys, func(i, j int) bool { return rowidLess(keys[i], keys[j]) })
	// Read through uncompress=, as fts3ReadExprList's statement does. Every
	// caller -- a DELETE's or UPDATE's scan, 'rebuild', 'integrity-check', a
	// REPLACE's displaced row -- prepares that statement whether or not
	// %_content holds a row. See fts3ContentCodec.
	codec, err := newFts3ContentCodec(sch, sch.uncompress)
	if err != nil {
		return nil, nil, err
	}
	docids := make([]int64, len(keys))
	rows := make([][]Value, len(keys))
	for i, k := range keys {
		rec, err := codec.row(sch, content.rows.row(k))
		if err != nil {
			return nil, nil, err
		}
		row := make([]Value, width)
		row[0] = Value{Typ: Int, I: int64(k)}
		for j := 1; j < width && j < len(rec); j++ {
			row[j] = rec[j]
		}
		docids[i] = int64(k)
		rows[i] = row
	}
	return docids, rows, nil
}

// fts3MutationRows is the row source a DELETE or UPDATE visits: %_content for
// an ordinary table, and the CONTENT TABLE for a "content=" one -- which is
// what makes a DELETE whose row the content table does not hold a complete
// no-op there (fts3_content.go), and what makes one against a CONTENTLESS
// table an error, since the table it names is "".
func (db *DB) fts3MutationRows(sch fts3Schema, s *fts3Shadows) ([]int64, [][]Value, error) {
	if !sch.hasContent {
		return fts3ContentRows(s.content, sch)
	}
	src, err := db.fts3ContentSourceIn(sch)
	if err != nil {
		return nil, nil, err
	}
	for i := range src.rows {
		src.rows[i] = fts3PadRow(src.rows[i], fts3ContentRowWidth(sch))
	}
	return src.docids, src.rows, nil
}

// fts3ContentRowWidth is the [docid, col0, ...] width fts3ContentRows builds,
// which a "content=" source has to match so the shared mutation code can index
// it the same way.
func fts3ContentRowWidth(sch fts3Schema) int {
	if sch.langid != "" {
		return len(sch.cols) + 2
	}
	return len(sch.cols) + 1
}

// fts3RowLangid reads a row's stored language id out of the trailing slot
// fts3ContentRows gives it. 0 for a table without "languageid=", and for a row
// whose %_content record predates the column.
func fts3RowLangid(sch fts3Schema, row []Value) int64 {
	if sch.langid == "" || len(row) <= len(sch.cols)+1 {
		return 0
	}
	n, _ := fts3LangidValue(row[len(sch.cols)+1])
	return n
}

// fts3WriteScope builds the evalCtx scope a DELETE/UPDATE's WHERE and SET
// expressions are evaluated in: the table's declared columns, which for an
// fts3 table are [docid HIDDEN, col0, ...] plus, for a "languageid=" one, the
// trailing hidden language column (fts3_langid.go).
func fts3WriteScope(name string, sch fts3Schema) tableScope {
	cols := make([]columnInfo, 0, len(sch.cols)+2)
	cols = append(cols, columnInfo{Name: "docid", DeclType: "INTEGER", Aff: typeAffinity("INTEGER"), Hidden: true})
	for _, c := range sch.cols {
		cols = append(cols, columnInfo{Name: c})
	}
	if sch.langid != "" {
		cols = append(cols, columnInfo{Name: sch.langid, Hidden: true})
	}
	return tableScope{name: name, cols: cols, colIndex: buildColIndex(cols), offset: 0}
}

// fts3WhereHasMatch reports whether e holds (or might hold) a MATCH operator.
// It DEFAULT-DENIES through fts3CountMatchExpr, so an expression shape that
// walker does not recognise is treated as "might", and the statement declines.
func fts3WhereHasMatch(e Expr) bool {
	n, known := fts3CountMatchExpr(e)
	return !known || n > 0
}

// fts3MutationCommon is the validation both DELETE and UPDATE share, plus the
// shadow-table lookups they both need. The compiled route's scan open
// (vtabWriteRows, vdbe_vtab_write.go) runs it too, so its errors land before
// any expression is evaluated there as well; it is idempotent.
//
// It used to take a "verb" string that no line of its body ever read -- the
// three error messages it can raise all name the module and the table instead.
// Dropped rather than threaded through a third caller.
func (db *DB) fts3MutationCommon(vm *vtabMeta, m fts3Module, sch fts3Schema) (*fts3Shadows, error) {
	// Rebuilding the shadow tables' b-trees is a full-materialize shape.
	s := &fts3Shadows{
		content: db.findTableMeta(vm.name + "_content"),
		segdir:  db.findTableMeta(vm.name + "_segdir"),
	}
	// A "content=" table has no %_content shadow at all; its rows come from
	// somewhere else entirely (fts3_content.go), and nothing here writes one.
	if (s.content == nil && !sch.hasContent) || s.segdir == nil {
		return nil, fmt.Errorf("engine: %s table %s is missing its shadow tables", m.name, vm.name)
	}
	s.segments = db.findTableMeta(vm.name + "_segments")
	if err := fts3ResolveFts4Shadows(db, vm.name, m, sch, s); err != nil {
		return nil, err
	}
	// A DELETE/UPDATE against an automerge-enabled table INSIDE an explicit
	// transaction or savepoint is not yet supported -- see
	// engine/fts3_automerge.go's own file comment. Checked before any row is
	// visited, so a decline here leaves the table exactly as it found it.
	if derr := db.fts3DeclineAutomergeInTxn(m, vm.name); derr != nil {
		return nil, derr
	}
	return s, nil
}

// fts3ResolveFts4Shadows fills in the two shadow tables only fts4 has. They are
// resolved SEPARATELY because "matchinfo=fts3" is an fts4 table with %_stat but
// no %_docsize at all (fts3MatchinfoOption) -- everything downstream then reads
// a nil s.docsize as "this table keeps no per-document sizes".
func fts3ResolveFts4Shadows(db *DB, name string, m fts3Module, sch fts3Schema, s *fts3Shadows) error {
	if !m.isFts4 {
		return nil
	}
	s.stat = db.findTableMeta(name + "_stat")
	if s.stat == nil {
		return fmt.Errorf("engine: fts4 table %s is missing its %%_stat shadow table", name)
	}
	if !sch.hasDocsize {
		return nil
	}
	s.docsize = db.findTableMeta(name + "_docsize")
	if s.docsize == nil {
		return fmt.Errorf("engine: fts4 table %s is missing its %%_docsize shadow table", name)
	}
	return nil
}

// fts3Shadows is one fts3/fts4 table's shadow tables, resolved once.
type fts3Shadows struct {
	content, segments, segdir, docsize, stat *tableMeta
}

// fts3Mutation is one DELETE or UPDATE statement in progress: the pending
// terms it is accumulating, the %_content/%_docsize edits it has staged, and
// the running %_stat image. Nothing is written until the whole statement has
// been simulated and its new segment has been encoded and accepted, so a
// statement this path declines leaves the table exactly as it found it.
type fts3Mutation struct {
	m    fts3Module
	nCol int
	// sch carries the notindexed= flags: a column left out of the index
	// contributes no terms, no %_docsize count and no %_stat bytes on either
	// side of a mutation (parseSchemaWith) -- and, through
	// sch.hasContent, whether this table keeps a %_content row store at all.
	sch fts3Schema

	pt *fts3PendingSet
	// wiped records that fts3DeleteAll ran: every shadow table is emptied at
	// commit and the new segment (if any) starts at %_segdir idx 0.
	wiped bool

	deleted map[int64]bool  // %_content/%_docsize rows to remove
	putRows []fts3StagedRow // %_content/%_docsize rows to (re)write
	live    map[int64]bool  // docids %_content holds as the simulation goes

	stat *fts4Stat // running %_stat image (fts4 only)

	// vtabName is the fts3/fts4 table's own name, which the per-transaction
	// segment is keyed on (fts3_txn.go).
	vtabName string

	// stmtSubTxn records that this statement's query plan is not the
	// single-docid lookup, so C fts3 opened a statement journal and flushed
	// every table's pending terms before the statement wrote anything --
	// fts3MutationOpensStmtSubTxn, applied at commit with the other seal rules.
	stmtSubTxn bool

	// haveLangid records whether pt.langid has been set from a row yet, and
	// langidSplit that some row disagreed with it. Real fts3 FLUSHES when the
	// language changes, splitting the statement across segments. UPDATE now
	// reproduces that instead of declining on it (mid/midFlushed below), so
	// these two fields feed only deleteFromFts3's OWN decline now: a DELETE's
	// rows are already required strictly ascending with no per-operation
	// split mechanism of its own to generalize, so a DELETE whose rows do not
	// all share one language is still declined -- but only at commit: a
	// DELETE that empties the table ends in fts3DeleteAll's wipe, which
	// throws the pending terms away and therefore never needs the decline at
	// all (see deleteFromFts3's own check, "!mu.wiped").
	haveLangid  bool
	langidSplit bool

	// mid is fts3PendingTermsDocid's flush rule tracked per operation
	// (fts3MidFlush) -- nil except for an UPDATE that needs it: a
	// docid-changing one, a WHERE-less one on an "order=DESC" table (rows
	// visited newest first; descFullScan), or one on a "languageid=" table.
	// midFlushed holds the pending sets a transition sealed off, in order;
	// each becomes its own segment at commit, ahead of mu.pt. Sealing writes
	// nothing, so a later decline or constraint failure undoes it.
	mid        *fts3MidFlush
	midFlushed []*fts3PendingSet
}

// noteMutationOp applies mu.mid's per-operation flush rule (a no-op without
// one) and, when it fires, seals mu.pt into mu.midFlushed and starts a fresh
// pending set in this operation's language (the old row's for a delete half,
// the new one for an insert half). C sets p->iPrevLangid to the operation
// just recorded (fts3_write.c:906) and adds the following terms under it
// (fts3PendingTermsAdd), so the new set must carry it before the terms go in.
func (mu *fts3Mutation) noteMutationOp(docid int64, isDelete bool, langid int64) {
	if mu.mid == nil || !mu.mid.note(docid, isDelete, langid) {
		return
	}
	mu.midFlushed = append(mu.midFlushed, mu.pt)
	mu.pt = newFts3PendingSet(mu.sch.prefixes, mu.sch.descIdx)
	mu.pt.langid = langid
}

// useLangid adopts langid as this statement's language, recording a SPLIT if a
// row disagrees with one already adopted.
func (mu *fts3Mutation) useLangid(langid int64) {
	if mu.haveLangid && mu.pt.langid != langid {
		mu.langidSplit = true
		return
	}
	mu.pt.langid, mu.haveLangid = langid, true
}

// touchedDocids returns every docid this statement recorded a term for, in
// ASCENDING order -- what the per-transaction segment's backwards-docid rule
// keys on (fts3_txn.go). A DELETE's marker docids and an UPDATE's re-inserted
// ones both count, which is why this reads the pending terms rather than
// putRows.
func (mu *fts3Mutation) touchedDocids() []int64 {
	seen := map[int64]bool{}
	for _, dl := range mu.pt.terms() {
		for _, d := range dl.docids {
			seen[d] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// deleteRow is fts3DeleteByRowid plus fts3DeleteTerms for one row: it records a
// marker for each of the row's terms, removes the row, and performs
// fts3DeleteAll's wipe when that empties the table. It returns the row's
// per-column token counts and indexed byte count for %_stat, or nil sizes
// when the wipe ran.
//
// The wipe can only happen on a statement's last row: rows are visited
// ascending, a DELETE empties %_content only at its final row, and an UPDATE
// puts each row back.
func (mu *fts3Mutation) deleteRow(docid int64, vals []Value) (sizes []int64, nByte int64) {
	sizes, nByte = fts3DeleteTermsInto(mu.pt, mu.sch, mu.nCol, docid, vals)
	delete(mu.live, docid)
	if len(mu.live) == 0 && !mu.sch.hasContent {
		// This row was the last one: fts3DeleteAll throws away the pending
		// terms and empties every shadow table, so nothing accumulated so far
		// in this statement survives.
		//
		// A "content=" table never reaches it: fts3IsEmpty short-circuits to
		// "not empty" there ("If using the content=xxx option, assume the
		// table is never empty"). Verified -- deleting the only row of one
		// leaves a SECOND %_segdir row full of delete markers and a %_stat of
		// X'00000000', where an ordinary table would have been wiped clean.
		mu.wipe()
		return nil, 0
	}
	// Each row is visited exactly once, delete-then-insert, so a docid staged
	// for (re)insertion is never afterwards deleted -- deleted and putRows stay
	// disjoint and commit can apply them in either order.
	mu.deleted[docid] = true
	return sizes, nByte
}

// deleteRowNoWipe is deleteRow WITHOUT fts3IsEmpty's "is this the last row"
// check -- what a docid-changing UPDATE's delete half uses instead of
// deleteRow, for every row of the statement (C fts3 compiles a rowid-
// changing UPDATE as a DELETE-only xUpdate call followed by an INSERT-only
// one, never the single combined call an ordinary same-docid UPDATE makes, so
// it never reaches fts3DeleteByRowid's wipe check at all).
//
// Verified: "UPDATE t SET docid=3 WHERE docid=7" over a table holding ONLY
// docid 7 leaves the ORIGINAL insert segment untouched and adds two more (a
// delete-marker-only one, then an insert-only one at docid 3) -- where
// deleteRow's wipe branch would instead have thrown every shadow table away.
func (mu *fts3Mutation) deleteRowNoWipe(docid int64, vals []Value) (sizes []int64, nByte int64) {
	sizes, nByte = fts3DeleteTermsInto(mu.pt, mu.sch, mu.nCol, docid, vals)
	delete(mu.live, docid)
	mu.deleted[docid] = true
	return sizes, nByte
}

// fts3DeleteTermsInto is fts3DeleteTerms' inner loop: record a DELETE MARKER in
// pt for every term of one row's own text, and return the row's per-column
// token counts and total indexed byte count -- what %_stat's decrement needs.
//
// It is shared with the INSERT path because REPLACE's displacement half is the
// SAME C code: sqlite3Fts3UpdateMethod calls fts3DeleteByRowid (and so
// fts3DeleteTerms) for the conflicting docid before inserting the new row
// (vtab_fts3.go).
func fts3DeleteTermsInto(pt *fts3PendingSet, sch fts3Schema, nCol int, docid int64, vals []Value) (sizes []int64, nByte int64) {
	sizes = make([]int64, nCol)
	for c := 0; c < nCol; c++ {
		v := vals[c+1]
		if v.Typ == Null || !sch.indexed(c) {
			// sqlite3_column_text() is NULL, so fts3PendingTermsAdd returns
			// early and sqlite3_column_bytes() contributes 0. A notindexed=
			// column is skipped the same way, on this side too: C fts3
			// writes no delete marker for text it never indexed.
			continue
		}
		text := valueToText(v)
		nByte += int64(len(text))
		terms := sch.tok.tokenize(text)
		sizes[c] = int64(len(terms))
		for _, term := range terms {
			pt.addDelete(term, docid)
		}
	}
	return sizes, nByte
}

// wipe is fts3DeleteAll: discard the pending terms and empty every shadow
// table. The caller's own per-row %_stat deltas are zeroed with it (the C
// memsets aSzDel/aSzIns), which is why this returns nothing for the caller to
// account for.
func (mu *fts3Mutation) wipe() {
	langid := mu.pt.langid
	mu.pt = newFts3PendingSet(mu.sch.prefixes, mu.sch.descIdx)
	mu.pt.langid = langid
	mu.wiped = true
	mu.deleted = map[int64]bool{}
	mu.putRows = nil
	mu.live = map[int64]bool{}
	// A wipe throws away everything accumulated so far, not just mu.pt: any
	// chunk noteMutationOp had already sealed off is gone with it too (commit
	// discards the whole transaction's accumulating segment here, the same
	// rule this file's header comment already documents), and the operation
	// history a docid-changing UPDATE's mid.note compares against no longer
	// applies to whatever this statement writes next.
	mu.midFlushed = nil
	if mu.mid != nil {
		mu.mid = &fts3MidFlush{}
	}
	if mu.stat != nil {
		mu.stat = &fts4Stat{colSizes: make([]int64, mu.nCol)}
	}
}

// insertRow is fts3InsertData plus fts3InsertTerms for one row.
func (mu *fts3Mutation) insertRow(docid int64, vals []Value) (sizes []int64, nByte int64) {
	sizes = make([]int64, mu.nCol)
	// %_content stores the docid in the rowid key, so its own slot is NULL.
	// A "languageid=" row carries one more column past the user ones
	// (fts3_langid.go), which vals already holds in the same slot.
	rec := make([]Value, len(vals))
	copy(rec, vals)
	rec[0] = Value{Typ: Null}
	for c := 0; c < mu.nCol; c++ {
		v := vals[c+1]
		if v.Typ == Null || !mu.sch.indexed(c) {
			continue
		}
		text := valueToText(v)
		nByte += int64(len(text))
		terms := mu.sch.tok.tokenize(text)
		sizes[c] = int64(len(terms))
		for pos, term := range terms {
			mu.pt.add(term, docid, c, pos)
		}
	}
	delete(mu.deleted, docid)
	mu.live[docid] = true
	intSizes := make([]int, mu.nCol)
	for i, n := range sizes {
		intSizes[i] = int(n)
	}
	mu.putRows = append(mu.putRows, fts3StagedRow{docid: docid, vals: rec, sizes: intSizes, nByte: nByte})
	return sizes, nByte
}

// applyDocTotals is fts3UpdateDocTotals: fold one xUpdate call's document
// count change and per-column size deltas into the running %_stat image. The
// arithmetic is u32 and SATURATES at zero rather than wrapping, which is
// observable on a table whose %_stat a direct shadow-table write has already
// pushed out of step with its rows.
func (mu *fts3Mutation) applyDocTotals(nChng int, szIns, szDel []int64, byteIns, byteDel int64) {
	if mu.stat == nil {
		return
	}
	mu.stat.applyDocTotals(mu.nCol, nChng, szIns, szDel, byteIns, byteDel)
}

// applyDocTotals is the arithmetic itself, on a %_stat image with no statement
// around it -- REPLACE's displacement half needs it from the INSERT path
// (vtab_fts3.go), where there is no fts3Mutation.
func (s *fts4Stat) applyDocTotals(nCol, nChng int, szIns, szDel []int64, byteIns, byteDel int64) {
	if nChng < 0 && s.nDoc < int64(-nChng) {
		s.nDoc = 0
	} else {
		s.nDoc = int64(uint32(s.nDoc + int64(nChng)))
	}
	sat := func(x, ins, del int64) int64 {
		if uint32(x)+uint32(ins) < uint32(del) {
			return 0
		}
		return int64(uint32(x) + uint32(ins) - uint32(del))
	}
	for c := 0; c < nCol; c++ {
		var ins, del int64
		if c < len(szIns) {
			ins = szIns[c]
		}
		if c < len(szDel) {
			del = szDel[c]
		}
		s.colSizes[c] = sat(s.colSizes[c], ins, del)
	}
	s.nByte = sat(s.nByte, byteIns, byteDel)
}

// commit writes the simulated statement out: the wipe (if any), the new
// %_segdir segments (one per index -- fts3_prefix.go) and the staged
// %_content/%_docsize/%_stat edits.
//
// The segments go FIRST because allocating one can cascade a level merge, whose
// only failure (fts3MaxMergeDepth, reachable solely from a hand-built shadow
// table) must land before %_content moves. A wipe cannot reach it: it empties
// %_segdir, so the allocation is immediate.
func (mu *fts3Mutation) commit(db *DB, s *fts3Shadows) error {
	// An UPDATE rewrites %_content through compress= (fts3InsertData's own
	// statement); every value is computed before the first write.
	var contentVals [][]Value
	if len(mu.putRows) > 0 && s.content != nil && !mu.sch.hasContent {
		codec, err := newFts3ContentCodec(mu.sch, mu.sch.compress)
		if err != nil {
			return err
		}
		contentVals = make([][]Value, len(mu.putRows))
		for i, r := range mu.putRows {
			if contentVals[i], err = codec.row(mu.sch, r.vals); err != nil {
				return err
			}
		}
	}
	// The statement sub-transaction flush point runs BEFORE this statement's
	// own first write, whichever shape that turns out to be -- the wipe, a
	// mid-statement chunk (below) or the final one. Moved here (rather than
	// left inside fts3TxnSegmentForMutation, which now only ever sees the
	// LAST chunk) so a docid-changing UPDATE's own mid-statement flushes see
	// it applied first too; nothing observable happens between "this
	// statement opened a statement journal" being decided and here, so this
	// is exactly where C fts3's OP_Transaction would have run it.
	db.fts3TxnStmtSeal(mu.stmtSubTxn)
	if mu.wiped {
		fts3ClearTable(s.content) // nil for a "content=" table, which never wipes
		fts3ClearTable(s.segdir)
		fts3ClearTable(s.segments)
		fts3ClearTable(s.docsize)
		fts3ClearTable(s.stat)
		// A wipe (fts3DeleteAll) threw the whole index away, so anything this
		// transaction had accumulated is gone with it -- exactly as C fts3
		// discards its pending terms there (see this file's "emptying the table
		// wipes everything").
		db.fts3TxnDiscard()
	}
	// A docid-changing UPDATE may have already sealed off one or more chunks
	// mid-statement (noteMutationOp); each becomes its own %_segdir segment,
	// ahead of the final (still-accumulating) one below. wipe() above already
	// discarded these when it ran, so this is a no-op then.
	hadMidFlush := len(mu.midFlushed) > 0
	if hadMidFlush && db.fts3ReadAutoincrmerge(mu.vtabName) != 0 {
		// See fts3_automerge.go's own file comment for why this combination
		// (more than one %_segdir write from a single statement, with
		// automerge enabled) is declined before any of them is flushed.
		return fmt.Errorf("engine: %s table %s: a single DELETE/UPDATE that flushes more than one %%_segdir write of its own (a docid-changing or \"order=desc\"-scanning UPDATE) is not supported by this write path while automerge is enabled (see engine/fts3_automerge.go)", mu.m.name, mu.vtabName)
	}
	for _, chunk := range mu.midFlushed {
		if rerr := db.fts3TxnFlushMid(mu.vtabName, mu.sch.prefixes, chunk.langid, mu.sch.descIdx, chunk, mu.m, s.segdir, s.segments); rerr != nil {
			return rerr
		}
	}
	if txnSeg := db.fts3TxnSegmentForMutation(mu.vtabName, mu.sch.prefixes, mu.pt.langid, mu.sch.descIdx, mu.touchedDocids(), false); txnSeg != nil {
		if rerr := db.fts3TxnMergeAndRewrite(txnSeg, mu.pt, mu.m, s.segdir, s.segments); rerr != nil {
			return rerr
		}
	} else if rerr := db.fts3AutocommitFlushAndAutomerge(mu.m, mu.vtabName, mu.sch, s.segdir, s.segments, hadMidFlush, mu.pt); rerr != nil {
		return rerr
	}
	for docid := range mu.deleted {
		if s.content != nil && !mu.sch.hasContent {
			s.content.dropRow(uint64(docid))
		}
		if s.docsize != nil {
			s.docsize.dropRow(uint64(docid))
		}
	}
	for i, r := range mu.putRows {
		// "%_content is left exactly as it is": a "content=" table's rows are
		// the other table's, and fts3 never writes through to them.
		if s.content != nil && !mu.sch.hasContent {
			s.content.putRow(uint64(r.docid), contentVals[i])
		}
		if s.docsize != nil {
			s.docsize.putRow(uint64(r.docid), []Value{{Typ: Null}, {Typ: Blob, S: encodeFts4Docsize(r.sizes)}})
		}
	}
	if mu.stat != nil {
		s.stat.putRow(0, []Value{{Typ: Null}, {Typ: Blob, S: mu.stat.encode()}})
	}
	return nil
}

// fts3ClearTable empties a shadow table's row store, the way fts3DeleteAll's
// "DELETE FROM %_xxx" does. A nil table (an fts3 table has no %_docsize or
// %_stat) is a no-op.
func fts3ClearTable(t *tableMeta) {
	if t == nil {
		return
	}
	for rid := range t.rows.all() {
		t.dropRow(rid)
	}
}

// newFts3Mutation starts a statement's simulation from the table's current
// state.
func (db *DB) newFts3Mutation(m fts3Module, s *fts3Shadows, sch fts3Schema, vtabName string) (*fts3Mutation, error) {
	nCol := len(sch.cols)
	mu := &fts3Mutation{
		m: m, nCol: nCol, sch: sch, vtabName: vtabName,
		pt:      newFts3PendingSet(sch.prefixes, sch.descIdx),
		deleted: map[int64]bool{},
		live:    map[int64]bool{},
	}
	if s.content != nil && !sch.hasContent {
		for rid := range s.content.rows.all() {
			mu.live[int64(rid)] = true
		}
	}
	if m.isFts4 {
		st, err := db.fts3LoadStat(s.stat, nCol)
		if err != nil {
			return nil, err
		}
		mu.stat = st
	}
	return mu, nil
}

// ---- DELETE ----

// deleteFromFts3 executes a DELETE whose target is the fts3/fts4 virtual table
// vm. outer is the enclosing TRIGGER row context (see deleteVtab), so an
// OLD./NEW. reference in the WHERE resolves. pre is the COMPILED route's
// already-decided row selection (see deleteVtab's own note and
// vtabWriteRowSet, vdbe_vtab_write.go).
func (db *DB) deleteFromFts3(vm *vtabMeta, m fts3Module, stmt *deleteStmt, args []Value, outer *evalCtx, pre vtabWriteRowSet) (int, error) {
	sch, err := m.schemaOf(db, vm)
	if err != nil {
		return 0, err
	}
	s, err := db.fts3MutationCommon(vm, m, sch)
	if err != nil {
		return 0, err
	}
	if stmt.returning != nil {
		return 0, fmt.Errorf("engine: DELETE ... RETURNING from an %s table is not supported by this write path", m.name)
	}
	if containsSubquery(stmt.where) {
		return 0, fmt.Errorf("engine: DELETE from an %s table with a subquery in WHERE is not supported by this write path", m.name)
	}
	if fts3WhereHasMatch(stmt.where) {
		return 0, fmt.Errorf("engine: DELETE from an %s table with a MATCH in WHERE is not supported by this write path", m.name)
	}
	scope := fts3WriteScope(stmt.table, sch)
	docids, rows, err := db.fts3MutationRows(sch, s)
	if err != nil {
		return 0, err
	}

	mu, err := db.newFts3Mutation(m, s, sch, vm.name)
	if err != nil {
		return 0, err
	}
	mu.stmtSubTxn = fts3MutationOpensStmtSubTxn(stmt.where)
	n := 0
	for i, docid := range docids {
		if pre != nil {
			// The compiled route's scan read THIS row source (vtabWriteRows,
			// which calls fts3MutationRows just as this function did above), so
			// "selected" is a membership test and not a second opinion.
			if _, ok := pre[docid]; !ok {
				continue
			}
		} else if stmt.where != nil {
			ctx := &evalCtx{tables: []tableScope{scope}, vals: rows[i], rowids: []Value{{Typ: Int, I: docid}}, params: args, outer: outer}
			sel, everr := writeRowSelected(ctx, stmt.where)
			if everr != nil {
				return 0, fmt.Errorf("engine: DELETE from %s: %w", stmt.table, everr)
			}
			if !sel {
				continue
			}
		}
		mu.useLangid(fts3RowLangid(sch, rows[i]))
		szDel, byteDel := mu.deleteRow(docid, rows[i])
		nChng := -1
		if szDel == nil {
			// The wipe ran: the C zeroes nChng and every size delta with it.
			nChng = 0
		}
		mu.applyDocTotals(nChng, nil, szDel, 0, byteDel)
		n++
	}
	if n == 0 {
		// A statement that matched nothing still opened its statement journal,
		// and that is where the flush lives -- so the seal has to happen here
		// too, where there is no segment to commit (fts3_txn.go).
		db.fts3TxnStmtSeal(mu.stmtSubTxn)
		return 0, nil
	}
	if mu.langidSplit && !mu.wiped {
		return 0, fmt.Errorf("engine: DELETE from %s: a DELETE whose rows do not share one %s language id is not supported by this write path (C fts3 splits it across index segments)", stmt.table, m.name)
	}
	if err := mu.commit(db, s); err != nil {
		return 0, err
	}
	return n, nil
}

// ---- UPDATE ----

// updateFts3 executes an UPDATE whose target is the fts3/fts4 virtual table vm.
// outer is the enclosing TRIGGER row context (see deleteVtab). pre is the
// COMPILED route's already-decided row selection plus each selected row's
// evaluated SET right-hand sides (see updateVtab's own note and
// vtabWriteRowSet, vdbe_vtab_write.go).
func (db *DB) updateFts3(vm *vtabMeta, m fts3Module, stmt *updateStmt, args []Value, outer *evalCtx, pre vtabWriteRowSet) (int, error) {
	sch, err := m.schemaOf(db, vm)
	if err != nil {
		return 0, err
	}
	s, err := db.fts3MutationCommon(vm, m, sch)
	if err != nil {
		return 0, err
	}
	if stmt.from != nil {
		return 0, fmt.Errorf("engine: UPDATE ... FROM against an %s table is not supported by this write path", m.name)
	}
	if stmt.returning != nil {
		return 0, fmt.Errorf("engine: UPDATE ... RETURNING against an %s table is not supported by this write path", m.name)
	}
	// REPLACE is the one conflict mode fts3 handles ITSELF, and only for a
	// docid that MOVES: sqlite3Fts3UpdateMethod asks
	// sqlite3_vtab_on_conflict(db)==SQLITE_REPLACE and, when it is, deletes
	// whatever row already holds the new docid (fts3DeleteByRowid) instead of
	// inserting the new %_content row up front and letting the duplicate-rowid
	// constraint fire. Every other mode -- ABORT/FAIL/ROLLBACK/IGNORE -- takes
	// the up-front-insert branch, whose SQLITE_CONSTRAINT the VDBE then acts on
	// per mode (OE_Ignore skips the row and the statement CONTINUES), which
	// this row loop does not model. See the displacement block below.
	replace := stmt.orAction == conflictReplace
	if stmt.orAction != conflictAbort && !replace {
		return 0, fmt.Errorf("engine: UPDATE against an %s table with an OR clause other than OR REPLACE is not supported by this write path", m.name)
	}
	if containsSubquery(stmt.where) {
		return 0, fmt.Errorf("engine: UPDATE against an %s table with a subquery in WHERE is not supported by this write path", m.name)
	}
	if fts3WhereHasMatch(stmt.where) {
		return 0, fmt.Errorf("engine: UPDATE against an %s table with a MATCH in WHERE is not supported by this write path", m.name)
	}
	colNames := sch.cols
	nCol := len(colNames)
	setIdx := make([]int, len(stmt.sets))
	docidChanges := false
	for i, a := range stmt.sets {
		if containsSubquery(a.expr) || fts3WhereHasMatch(a.expr) {
			return 0, fmt.Errorf("engine: UPDATE against an %s table with a subquery or MATCH in SET is not supported by this write path", m.name)
		}
		pos := -1
		for j, c := range colNames {
			if strings.EqualFold(c, a.col) {
				pos = j + 1
				break
			}
		}
		if pos < 0 && sch.langid != "" && strings.EqualFold(a.col, sch.langid) {
			pos = nCol + 1
		}
		if pos < 0 && strings.EqualFold(a.col, "docid") {
			// Slot 0 of a row array is the docid (fts3WriteScope). Real
			// fts3's own delete-then-insert order and mid-statement flush
			// point are reproduced in the row loop below
			// (fts3_txn.go's fts3MidFlush).
			pos = 0
			docidChanges = true
		}
		if pos < 0 && (strings.EqualFold(a.col, "rowid") || strings.EqualFold(a.col, "oid") || strings.EqualFold(a.col, "_rowid_")) {
			// Unlike an ordinary table, "rowid"/"oid"/"_rowid_" as a SET target
			// do not move the docid here -- only "docid" does. The assignment is
			// dropped but the RHS is still evaluated (writeApplySetList): "UPDATE
			// t SET rowid=99 WHERE docid=1" leaves the docid at 1, while "SET
			// rowid=abs(-9223372036854775807-1), a='beta'" raises "integer
			// overflow". sqlite3Update codes the synonym into apVal[1]
			// (update.c:494-498, 1291), but fts3's xUpdate reads the new rowid
			// from the docid cell apVal[3+nColumn] and uses apVal[1] only when
			// that is NULL (fts3_write.c:5762-5765), which on UPDATE it never is
			// (update.c:1284).
			pos = fts3SetRowidSynonymNoOp
		}
		if pos < 0 && pos != fts3SetRowidSynonymNoOp {
			return 0, fmt.Errorf("engine: UPDATE %s: no such column: %s", stmt.table, a.col)
		}
		setIdx[i] = pos
	}
	scope := fts3WriteScope(stmt.table, sch)
	docids, rows, err := db.fts3MutationRows(sch, s)
	if err != nil {
		return 0, err
	}
	// A WHERE-less UPDATE on an "order=DESC" table is FTS3_FULLSCAN_SEARCH
	// with idxStr unset, so fts3FilterMethod scans in bDescIdx order
	// (fts3.c:3356-3359). Reproduced by reversing the ascending rows and
	// applying the per-operation flush rule (fts3MidFlush): the flush
	// condition (fts3_write.c:903) fires whenever a docid does not
	// increase, so it flushes between every pair of rows. With a WHERE the
	// plan (a docid lookup visits ascending) is a cost-based choice, so it
	// stays declined.
	descFullScan := sch.descIdx && stmt.where == nil && !docidChanges
	if descFullScan {
		for l, r := 0, len(docids)-1; l < r; l, r = l+1, r-1 {
			docids[l], docids[r] = docids[r], docids[l]
			rows[l], rows[r] = rows[r], rows[l]
		}
	}
	// OR REPLACE's displacement half deletes a row this statement is not
	// otherwise visiting, and needs its stored text to write the delete
	// markers for -- fts3DeleteTerms reads it back out of %_content by rowid.
	var rowByDocid map[int64][]Value
	if replace && docidChanges {
		rowByDocid = make(map[int64][]Value, len(docids))
		for i, d := range docids {
			rowByDocid[d] = rows[i]
		}
	}

	mu, err := db.newFts3Mutation(m, s, sch, vm.name)
	if err != nil {
		return 0, err
	}
	mu.stmtSubTxn = fts3MutationOpensStmtSubTxn(stmt.where)
	// A language-tracking table (sch.langid != "") needs mu.mid for the SAME
	// reason a docid-changing or descFullScan UPDATE does: fts3MidFlush's
	// langid clause (fts3PendingTermsDocid's "p->iPrevLangid!=iLangid") can
	// fire between two operations of ONE statement whenever a row's language
	// actually changes, or when a multi-row UPDATE's rows do not all share
	// one. For a table where every row/operation shares one language this is
	// a harmless no-op: the clause never fires (langid == f.langid always),
	// leaving the pre-existing docid-ordering behavior exactly as before.
	if docidChanges || descFullScan || sch.langid != "" {
		mu.mid = &fts3MidFlush{}
		if !mu.stmtSubTxn {
			// Seed from what this table's transaction is already accumulating
			// (fts3TxnPeekLastDocid/LangId), so a statement that is not the first
			// write of its transaction flushes where C's connection-wide
			// iPrevDocid/iPrevLangid would. Unseeded when the statement's
			// stmtSubTxn preseal cleared it, or nothing is accumulating.
			// descFullScan never reaches here (a WHERE-less statement always opens
			// a statement sub-transaction); a language-tracking single-docid UPDATE
			// can.
			if d, have := db.fts3TxnPeekLastDocid(vm.name); have {
				mu.mid.docid, mu.mid.have = d, true
			}
			if l, have := db.fts3TxnPeekLastLangid(vm.name); have {
				mu.mid.langid = l
			}
		}
	}
	n := 0
	displacedAny := false
	for i, docid := range docids {
		var setVals []Value
		var ctx *evalCtx
		if pre != nil {
			// See deleteFromFts3's identical branch: the compiled scan read
			// this same row source, so this is a membership test. setVals are
			// this row's SET right-hand sides as that scan evaluated them.
			sv, ok := pre[docid]
			if !ok {
				continue
			}
			setVals = sv
		} else {
			ctx = &evalCtx{tables: []tableScope{scope}, vals: rows[i], rowids: []Value{{Typ: Int, I: docid}}, params: args, outer: outer}
			if stmt.where != nil {
				sel, everr := writeRowSelected(ctx, stmt.where)
				if everr != nil {
					return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, everr)
				}
				if !sel {
					continue
				}
			}
		}
		// SET RHS sees the OLD row; a fts3SetRowidSynonymNoOp target's RHS is
		// evaluated for its errors and its value then discarded --
		// see where setIdx[k] was assigned above. (The compiled route never
		// carries one: compileVtabUpdateStmt requires every SET target to be a
		// name fts3WriteScope holds, and it holds none of the three rowid
		// spellings -- see writeApplySetListPre for why the two-pass split that
		// sentinel exists for cannot survive precomputation.)
		newVals := append([]Value(nil), rows[i]...)
		if pre != nil {
			if serr := writeApplySetListPre(newVals, setVals, setIdx); serr != nil {
				return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, serr)
			}
		} else if everr := writeApplySetList(ctx, newVals, stmt.sets, setIdx); everr != nil {
			return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, everr)
		}
		// A row's language decides which %_segdir levels both halves of the
		// update land in. Real fts3 flushes between them when it CHANGES,
		// leaving delete markers under the old language and a fresh segment
		// under the new one (verified); this path builds one segment, so it
		// declines instead (fts3_langid.go).
		newLangid := int64(0)
		if sch.langid != "" {
			var langidOK bool
			if newLangid, langidOK = fts3LangidValue(newVals[nCol+1]); !langidOK {
				return 0, fmt.Errorf("engine: UPDATE %s: constraint failed", stmt.table)
			}
			// %_content stores it as an INTEGER, whatever the SET wrote.
			newVals[nCol+1] = Value{Typ: Int, I: newLangid}
		}
		newDocid := docid
		displaced := false
		if docidChanges {
			// %_content's rowid takes INTEGER affinity before fts3 ever sees
			// it (sqlite3_value_int64 on the bound parameter), which is more
			// lenient than an ordinary rowid-alias UPDATE's own coercion --
			// verified: "SET docid='5'" and "SET docid=3.0" both succeed
			// here, where the same values against an ORDINARY table's rowid
			// fail. A NULL new docid is left alone: the oracle does not
			// error OR auto-assign for it, unlike every other shape this
			// engine's rowidFromValue call handles, and reproducing THAT
			// needs more than this rule accounts for.
			v := applyAffinityToValue(newVals[0], affInteger)
			if v.Typ == Null {
				return 0, fmt.Errorf("engine: UPDATE of an %s table's docid to NULL is not supported by this write path", m.name)
			}
			rid, rerr := rowidFromValue(v)
			if rerr != nil {
				return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, rerr)
			}
			newDocid = int64(rid)
			newVals[0] = Value{Typ: Int, I: newDocid}
			if newDocid != docid && mu.live[newDocid] {
				if !replace {
					// The new docid is already a LIVE row (%_content's own
					// INTEGER PRIMARY KEY), the same conflict an ordinary
					// duplicate-rowid INSERT hits. Real fts3 catches this
					// before removing the OLD row (fts3InsertData's own
					// conflict check), so nothing of this row's own change has
					// been recorded yet -- returning here leaves the table
					// exactly as it was found, same as every other decline in
					// this file.
					return 0, fmt.Errorf("engine: UPDATE %s: constraint failed", stmt.table)
				}
				// Under OR REPLACE the conflicting row is DISPLACED instead --
				// see the delete block below.
				displaced = true
			}
		}
		mu.useLangid(fts3RowLangid(sch, rows[i]))
		mu.useLangid(newLangid)
		// The no-wipe, split-call treatment is keyed on whether THIS ROW's
		// docid actually moves at RUNTIME, not on whether the SET clause
		// syntactically names it: C fts3's own xUpdate is asserted
		// "apVal[0]==apVal[1]" for a single combined call, so "SET
		// docid=docid" -- the computed value equal to the row's own --
		// verified to take the SAME path an ordinary column-only UPDATE does
		// (deleteRow's wipe check included), not this one. docidChanges only
		// widens what the SET-parsing loop above accepts and what newDocid is
		// computed from; whether THIS row is treated as a relocation is
		// decided fresh per row.
		rowDocidMoves := docidChanges && newDocid != docid
		// xUpdate's order: delete the old row, then insert the new one, into
		// the same pending terms unless noteMutationOp just sealed them
		// (fts3MidFlush).
		//
		// Whether the old row's delete can wipe the table depends on what
		// %_content holds then. Under ABORT the up-front fts3InsertData has
		// already added the new row, so it never can (deleteRowNoWipe). Under
		// REPLACE there is no up-front insert, so it can: on an fts4 table
		// whose only row is docid 7, "UPDATE OR REPLACE t SET docid=3 WHERE
		// docid=7" leaves one %_segdir row (wiped), where plain UPDATE leaves
		// five.
		nChng := 0
		var szDel []int64
		var byteDel int64
		// foldDelete accumulates one fts3DeleteByRowid call into the running
		// (nChng, aSzDel, byteDel) triple the C carries across BOTH of an
		// xUpdate's deletes. A nil sizes slice is the wipe, which zeroes every
		// one of them ("*pnChng = 0" plus the aSzDel memset) -- including a
		// displacement's own contribution from the call before it.
		foldDelete := func(sz []int64, nByte int64) {
			if sz == nil {
				nChng, szDel, byteDel = 0, nil, 0
				return
			}
			nChng--
			if szDel == nil {
				szDel = make([]int64, mu.nCol)
			}
			for c := range sz {
				szDel[c] += sz[c]
			}
			byteDel += nByte
		}
		if displaced {
			// OR REPLACE's own half, and it runs FIRST -- before this row's
			// own delete, let alone its insert (sqlite3Fts3UpdateMethod's
			// "if( sqlite3_vtab_on_conflict(p->db)==SQLITE_REPLACE )"
			// branch). Its markers therefore land under the DISPLACED docid,
			// which is greater than this row's own, so the old row's delete
			// that follows steps backwards and seals a segment off between
			// them. Verified against 3.53.3 on a three-row fts4 table
			// (docids 1,2,3): "UPDATE OR REPLACE t SET docid=2 WHERE docid=1"
			// leaves a marker-only segment for docid 2's text and then a
			// second segment carrying docid 1's markers beside docid 2's new
			// terms.
			old, ok := rowByDocid[newDocid]
			if !ok {
				// mu.live and rowByDocid are built from the same %_content
				// scan, so this is unreachable -- but a missing row would
				// index past the end of a nil slice in fts3DeleteTermsInto,
				// and this path must never panic.
				return 0, fmt.Errorf("engine: UPDATE %s: %s_content has no row for the displaced docid %d", stmt.table, stmt.table, newDocid)
			}
			// fts3DeleteTerms reads the DISPLACED row's own language out of
			// %_content (langidFromSelect) and writes its markers there, so a
			// displacement across languages splits the statement across
			// indexes exactly as any other language change does.
			oldLangid := fts3RowLangid(sch, old)
			mu.useLangid(oldLangid)
			mu.noteMutationOp(newDocid, true, oldLangid)
			foldDelete(mu.deleteRow(newDocid, old))
		}
		// Each half's own noteMutationOp call carries the LANGUAGE that half
		// actually writes under -- langidFromSelect's OLD-row language for a
		// delete, the SET-clause's NEW one for an insert
		// (fts3_write.c:1102/5816, sqlite3Fts3UpdateMethod) -- not
		// mu.pt.langid, which useLangid leaves UNCHANGED across a language
		// change (see useLangid's own comment) and so is stale exactly when
		// it matters here.
		mu.noteMutationOp(docid, true, fts3RowLangid(sch, rows[i]))
		if rowDocidMoves && !replace {
			foldDelete(mu.deleteRowNoWipe(docid, rows[i]))
		} else {
			foldDelete(mu.deleteRow(docid, rows[i]))
		}
		if displaced {
			displacedAny = true
		}
		mu.noteMutationOp(newDocid, false, newLangid)
		szIns, byteIns := mu.insertRow(newDocid, newVals)
		nChng++
		mu.applyDocTotals(nChng, szIns, szDel, byteIns, byteDel)
		n++
	}
	if n == 0 {
		// See deleteFromFts3's own zero-row branch: the flush is the statement
		// journal's, not the writes'.
		db.fts3TxnStmtSeal(mu.stmtSubTxn)
		return 0, nil
	}
	if displacedAny && n > 1 {
		// The delete markers a row's delete half writes come from %_content as
		// it stands AT THAT MOMENT (fts3DeleteTerms re-reads the row by rowid),
		// while this loop works from the one snapshot it took before the
		// statement began. The two agree for every shape but this one: a
		// displacement overwrites a docid that a LATER row of the same
		// statement may itself be updating, and C fts3 then retires the
		// DISPLACING row's text where this loop would retire the original's.
		// Verified against 3.53.3 -- on an fts4 table holding (1,'aa'),
		// (2,'bb'),(3,'cc'), "UPDATE OR REPLACE t SET docid=docid+1" cascades
		// down to a SINGLE row (4,'cc'). Declined rather than approximated.
		return 0, fmt.Errorf("engine: UPDATE %s: an OR REPLACE that displaces a row while updating more than one row is not supported by this write path (C fts3 re-reads %s_content for each row's delete markers, so one row's displacement changes what the next one retires)", stmt.table, stmt.table)
	}
	// A langid CHANGE (or a multi-row UPDATE whose rows do not all share one
	// language) is no longer declined here: mu.mid -- widened above to track
	// every language-tracking UPDATE, not just a docid-changing one -- has
	// already split the statement into one %_segdir segment per (docid,
	// language) transition as each operation was recorded, exactly as real
	// fts3's own fts3PendingTermsDocid does (fts3_langid.go's "A langid
	// CHANGE splits the segment"). mu.langidSplit/mu.haveLangid still feed
	// deleteFromFts3's OWN, separate decline (a DELETE has no per-operation
	// mid-flush machinery at all -- see that function's own check), but play
	// no further role here.
	if sch.descIdx && n > 1 && !descFullScan {
		// An "order=desc" table is scanned newest first, so an UPDATE of
		// several rows flushes between each: one segment per row, descending
		// ("UPDATE d SET x='q'" over docids 1..4 leaves four). With a WHERE
		// the visit order is the plan's ("WHERE docid IN (1,3)" visits
		// ascending and leaves one segment), which this does not derive, so it
		// declines. The WHERE-less case is unambiguous and handled above
		// (descFullScan). A DELETE never splits: its RowSet iterates
		// ascending.
		return 0, fmt.Errorf("engine: UPDATE %s: an UPDATE of more than one row of an \"order=desc\" %s table is not supported by this write path (C fts3 scans it newest-docid first and writes one index segment per row)", stmt.table, m.name)
	}
	if err := mu.commit(db, s); err != nil {
		return 0, err
	}
	return n, nil
}
