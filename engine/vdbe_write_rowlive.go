package engine

// The write path's UPDATE/DELETE row loop is ROW-LIVE, and saying so is the
// whole of this file.
//
// compiler.rowLive (vdbe_codegen.go) is the promise that THIS compile's open
// cursors still stand on the correlated row at the instant a sub-Program
// compiled beneath it executes. compileColumn refuses to emit an
// OpOuterColumn/OpOuterRowid against a compile that does not make that promise,
// so every correlated reference out of an UPDATE's WHERE or SET, or a DELETE's
// WHERE, declined with "correlated reference %q to a non-row-live outer scope"
// and the whole statement failed with it -- AGENTS.md Rule 1: what the
// compiler cannot lower is an ERROR, never routed elsewhere to be answered
// another way. newWriteScanCompilerAs (vdbe_write.go) never set the flag -- not because
// the row is not live there, but because nothing had asked. That is the same
// omission compileScanWindow carried until recently (vdbe_window.go),
// where four oracle-verified shapes were declining for want of one line.
//
// # The C
//
// C SQLite cannot have this gap, because it has no notion of a frame at all: it
// emits ONE flat program with ONE cursor namespace, and a correlated TK_COLUMN
// is coded as a plain OP_Column against whichever cursor number owns it --
//
//	iReg = sqlite3ExprCodeGetColumn(pParse, pExpr->y.pTab,
//	                         pExpr->iColumn, iTab, target,
//	                         pExpr->op2);          -- expr.c:5104-5106
//
// with "int iTab = pExpr->iTable;" (expr.c:5023) read straight off the node, at
// whatever query level resolve.c bound it. Nothing in sqlite3ExprCodeTarget
// distinguishes an inner cursor from an outer one. What makes the read correct
// is purely WHERE the sub-select's code was emitted, and for both statements it
// is inside the scan loop with the target cursor positioned:
//
//   - DELETE: "pWInfo = sqlite3WhereBegin(pParse, pTabList, pWhere, 0, 0,0,
//     wcf, iTabCur+1);" (delete.c:526) -- the WHERE is ordinary WHERE codegen
//     over iTabCur, indistinguishable from a SELECT's.
//   - UPDATE: the same call for its own WHERE ("pWInfo = sqlite3WhereBegin(
//     pParse, pTabList, pWhere,0,0,0,flags,iIdxCur);", update.c:742), and each
//     SET right-hand side coded inline in the pass-two loop --
//     "sqlite3ExprCode(pParse, pChanges->a[j].pExpr, k);" (update.c:954) -- ten
//     lines above "sqlite3ExprCodeGetColumnOfTable(v, pTab, iDataCur, i, k);"
//     (update.c:964), which is the proof that iDataCur stands on the row being
//     updated at exactly that point.
//
// musql's emitted loop has the same shape: OpOpenWrite/OpRewind/OpNext over
// cursor 0, with the WHERE and the SET values compiled inside it (and, for
// UPDATE ... FROM, an OpNotExists re-seek of cursor 0 per ephemeral entry --
// update.c:861-864 -- before either). So the read an OpOuterColumn does against
// that frame is C's own OP_Column against iDataCur/iTabCur, one frame removed.
//
// A VIEW write is the same loop over a different cursor 0 -- the MATERIALIZED
// view, which IS iDataCur there ("iDataCur = iIdxCur = iTabCur", delete.c:432;
// update.c hands iDataCur straight to sqlite3MaterializeView at :632) -- and it
// was carrying the identical omission. emitViewWhere and compileViewUpdateStmt's
// SET loop (vdbe_view_write.go) take the identical spans.
//
// # Why the FROZEN subquery snapshot is still the right image
//
// A correlated sub-Program is re-run per outer row (runSub -> execWithParent,
// vdbe.go) instead of once, but it still reads Program.WritePager -- the ONE
// snapshot writeSubqueryPager takes before the scan. C evaluates a correlated
// subquery against the LIVE table, so the two can only disagree if something
// writes the subquery's SOURCE during the statement. Both ways that can happen
// were already closed, and this promotion does not open either:
//
//   - the subquery reads the TARGET table -- emitSetValue's own
//     insnsHoldRowDependentSubquery guard (vdbe_write.go) declines the whole SET
//     when the compiled sub-program is row-dependent, exactly as before. This
//     promotion is what finally makes that guard's Program.Correlated arm
//     LOAD-BEARING: update_set_selfread_codegen_test.go recorded dropping that
//     arm as a mutation ESCAPE, and gave the measured reason -- "every
//     correlated spelling is refused EARLIER today -- compileColumn's
//     'correlated reference to a non-row-live outer scope' for the ordinary
//     form". That earlier refusal is what this file removes, so the arm is now
//     the only thing holding those shapes;
//   - the subquery reads a table a TRIGGER or FK action writes during the
//     loop -- the same guard, asked of every such table rather than only the
//     target (tablesALoopCanWrite). This bullet used to say a trigger could not
//     reach this route; it can, through the OUTER statement's own SET, and
//     "UPDATE c SET n=(SELECT count(*) FROM b WHERE b.id>=c.id)" with a trigger
//     on c inserting into b answered 3,2,1 where C answers 3,3,3.
//
// And for a WHERE, the frozen image is not an approximation but the exact
// semantics: both statements evaluate their WHERE in PASS ONE, over the
// untouched table (delete.c's "Two-pass approach - use a FIFO for rowids/PK
// values", delete.c:522; update.c's ephemeral-of-matched-rowids, update.c:849),
// so a correlated WHERE subquery in C reads the pre-statement image too.
//
// The answer this reproduces is unchanged from the route it replaced, which
// took ONE db.SnapshotPager() up front and reused it for BOTH the WHERE match
// phase and the SET evaluation phase -- the same
// image this compiles against. That is why the promotion moves no differential
// tally -- and why a differential test cannot prove it. See
// engine/write_correlated_codegen_test.go for the compile-level proof.

// rowLiveSpan marks THIS compile's open cursors as holding the correlated row
// for the span of the returned undo, and restores the previous value.
//
// A SPAN rather than a field set once at construction, because an UPDATE/DELETE
// compiles expressions on BOTH sides of its row loop: the WHERE and the SET
// values run with cursor 0 positioned on the row, while a RETURNING list is
// emitted AFTER the row has been rewritten or deleted and must keep declining
// (a correlated read there would be a wrong VALUE where the gap today is an
// honest decline -- AGENTS.md invariant 1). Same save/restore shape as
// compileScanWindow's (vdbe_window.go) and compileScanAggregate's
// (vdbe_agg_codegen.go) own brackets, which exist for the same reason.
// whereSpan is rowLiveSpan for an UPDATE's or DELETE's own WHERE, which also
// tells a trigger body's subqueries to read the statement-start image
// (liveWhereSubSelect).
//
// The WHERE is coded inside sqlite3WhereBegin, after where.c:7198 has added
// the loop's nRowOut to pParse->nQueryLoop -- a number the port cannot give
// for a WHERE holding a subquery, the only kind with anything nested to plan
// -- so inside the span nQueryLoop is not known.
func (c *compiler) whereSpan() func() {
	undo := c.rowLiveSpan()
	saved, savedKnown := c.inWhere, c.nQueryLoopKnown
	c.inWhere, c.nQueryLoopKnown = true, false
	return func() { c.inWhere, c.nQueryLoopKnown = saved, savedKnown; undo() }
}

func (c *compiler) rowLiveSpan() func() {
	saved := c.rowLive
	c.rowLive = true
	return func() { c.rowLive = saved }
}

// updateOnePassOrder is the order C's ONE-PASS UPDATE loop visits tbl's rows
// in, from the ported planner: a nil key for the ascending rowid scan, and ok
// false where the port cannot say, which leaves a per-row SET subquery over the
// table declined.
//
// C plans the loop with sqlite3WhereBegin(..., WHERE_ONEPASS_DESIRED |
// WHERE_ONEPASS_MULTIROW, ...) over the target alone (update.c:742), after
// resetting the item's colUsed so the SET list does not count (update.c:558)
// -- which is exactly the planning input of "SELECT 1 FROM tbl WHERE <where>"
// with the target's own hints, bar one arm: WHERE_ONEPASS_DESIRED withholds the
// covering-index full scan (where.c:4240), which c.onePassPlan switches off.
// A trigger body's loop plans the same way: its sub-parse starts from the
// firing statement's nQueryLoop (trigger.c:1288), which seeds every candidate
// path of a one-table plan alike (where.c:5921) and so cannot change which wins.
// Reverse order (where.c:7126) is the planner's own to apply.
//
// oneRow reports a WHERE_ONEROW plan, which C runs ONEPASS_SINGLE.
//
// trig is the trigger a BODY statement belongs to, whose NEW and OLD the
// WHERE may name: they plan as constants (wherePlanBoundOuter).
func (db *DB) updateOnePassOrder(tbl *tableMeta, stmt *updateStmt, trig *trigCompileCtx) (key *autoIndexKey, nRow logEst, oneRow, ok bool) {
	p, err := db.segmentReadPager()
	if err != nil || p == nil {
		return nil, 0, false, false
	}
	sel := &SelectStmt{
		Columns: []SelectColumn{{Expr: LiteralExpr{Val: Value{Typ: Int, I: 1}}}},
		From:    []FromItem{{Table: tbl.name, Schema: stmt.schema, Alias: stmt.alias, IndexedBy: stmt.indexedBy, NotIndexed: stmt.notIndexed}},
		Where:   stmt.where,
	}
	var won wherePlanWinner
	c := &compiler{pager: p, onePassPlan: true, planWinner: &won, trig: trig}
	srcs, err := resolveJoinSources(p, c, sel.From)
	if err != nil || len(srcs) != 1 || srcs[0].tbl == nil || !equalFoldName(srcs[0].tbl.name, tbl.name) {
		return nil, 0, false, false
	}
	key, nRow, ok = wherePlanSingleTableIndexOrder(p, c, srcs, sel)
	return key, nRow, won.oneRow, ok
}

// subqueryPlansAutoIndex reports whether C plans sel -- a subquery coded
// inside a loop that runs nQueryLoop times -- with an AUTOMATIC INDEX on its
// one FROM table. That index is built under OP_Once (constructAutomaticIndex,
// where.c) the FIRST time the subquery runs and reused every time after, so
// the subquery reads that table as it stood then, not as the statement has
// since left it. Measured against 3.53.3 over a table sqlite_stat1 says holds
// 7 rows: "UPDATE t SET d = (SELECT count(*) FROM t t2 WHERE t2.d = t.d)"
// gives every row 20, the first row's count, where a live read gives
// 20,20,20,19,19,19,...
//
// ponytail: one FROM table. A join inside the subquery can put the automatic
// index on one table and read another live, which one image cannot express.
//
// outer is the compiler sel's correlated references resolve against, carrying
// the enclosing loop's nQueryLoop.
func subqueryPlansAutoIndex(p *ReadOnlyPager, sel *SelectStmt, outer *compiler) bool {
	if p == nil || sel == nil || outer == nil || !outer.nQueryLoopKnown ||
		len(sel.From) != 1 || len(sel.Compound) > 0 || sel.From[0].Subquery != nil {
		return false
	}
	var won wherePlanWinner
	c := &compiler{pager: p, outer: outer, nQueryLoop: outer.nQueryLoop, nQueryLoopKnown: true, planWinner: &won}
	srcs, err := resolveJoinSources(p, c, sel.From)
	if err != nil || len(srcs) != 1 {
		return false
	}
	wherePlanSingleTableIndexOrder(p, c, srcs, sel)
	return won.auto
}

// updateNeverOnePass and updateKeyForcesTwoPass report whether C runs this
// UPDATE ONEPASS_OFF -- two passes, the second walking the matched rowids
// ascending (update.c:776), which is this loop's own order -- although it has
// no trigger and no WHERE subquery. Each arm is one of C's:
//
//   - chngKey, bReplace and hasFK withhold WHERE_ONEPASS_MULTIROW outright
//     (update.c:733-739), whatever the plan: a SET that moves the rowid or a
//     WITHOUT ROWID PRIMARY KEY, REPLACE resolution -- the statement's own OR
//     REPLACE, or an updated index declaring it (update.c:579-581) -- and any
//     foreign key the SET touches, child or parent side (sqlite3FkRequired).
//   - given the plan: a WHERE_MULTI_OR one is never one-pass without
//     WHERE_DUPLICATES_OK (where.c:7227), and a one-pass scan through an index
//     whose key the SET updates falls back (update.c:757-766) -- that index is
//     opened for writing (aToOpen, indexColumnIsBeingUpdated, :575-586).
//
// Measured before this existed, over t(a,b) with an index on b:
// "UPDATE t SET b=(SELECT sum(b) FROM t t2 WHERE t2.a<=t.a) WHERE b>0" walked
// index order here and rowid order in C.
func (db *DB) updateNeverOnePass(tbl *tableMeta, stmt *updateStmt, colIdx []int) bool {
	mask, chngRowid := updateSetMask(colIdx, len(tbl.cols), tbl.ipkIndex)
	if chngRowid {
		return true
	}
	if tbl.withoutRowid && tbl.pkIndex != nil && maskHasAny(mask, tbl.pkIndex.colIdx) {
		return true // chngPk
	}
	if stmt.explicitOr && stmt.orAction == conflictReplace {
		return true
	}
	for _, idx := range db.indexes {
		if !stmt.explicitOr && idx.unique && idx.onConflict == conflictReplace && indexBelongsTo(idx, tbl) && maskHasAny(mask, idx.colIdx) {
			return true
		}
	}
	if db.fkEnforce {
		for _, fk := range db.fkOf(tbl) {
			if fkAssignsChildCols(fk, mask) {
				return true
			}
		}
		for _, fk := range db.fkReferencing(tbl) {
			if maskHasAny(mask, fk.parentIdx) {
				return true
			}
		}
	}
	return false
}

// updateEveryIndexForcesTwoPass reports whether the order is ascending rowid
// whichever plan where.c picks, so none needs asking for -- which matters after
// ANALYZE, where the ported planner cannot say: every index that could drive a
// one-pass scan is one the SET updates, so a scan through it falls back to two
// passes (update.c:757-766), and the only other scan is the table's own in
// rowid order. Not under reverse_unordered_selects, which walks that table
// scan backward (where.c:7126) but leaves the fallback's second pass forward.
func (db *DB) updateEveryIndexForcesTwoPass(tbl *tableMeta, stmt *updateStmt, colIdx []int) bool {
	if db.pragmaState.ReverseUnorderedSelects() || tbl.withoutRowid {
		return false
	}
	mask, _ := updateSetMask(colIdx, len(tbl.cols), tbl.ipkIndex)
	for _, idx := range db.indexes {
		if indexBelongsTo(idx, tbl) && !maskHasAny(mask, idx.colIdx) {
			return false
		}
	}
	return true
}

func updateKeyForcesTwoPass(key *autoIndexKey, colIdx []int, nCols int) bool {
	if key.multiOr != nil {
		return true
	}
	// A key naming any column is an index's (autoIndexKey.root is not set on
	// this path); the reversed rowid scan's key is the rowid alone (-1), which
	// maskHasAny skips -- that scan reads the table b-tree, iCur == iDataCur.
	mask, _ := updateSetMask(colIdx, nCols, -1)
	return maskHasAny(mask, key.cols)
}

// updateSetMask is the columns an UPDATE's SET list assigns, and whether it
// assigns the rowid (a rowid alias, or the INTEGER PRIMARY KEY at ipk).
func updateSetMask(colIdx []int, nCols, ipk int) (mask []bool, chngRowid bool) {
	mask = make([]bool, nCols)
	for _, i := range colIdx {
		if i == noColumnRowidTarget || (i >= 0 && i == ipk) {
			chngRowid = true
		}
		if i >= 0 && i < nCols {
			mask[i] = true
		}
	}
	return mask, chngRowid
}

func maskHasAny(mask []bool, cols []int) bool {
	for _, c := range cols {
		if c >= 0 && c < len(mask) && mask[c] {
			return true
		}
	}
	return false
}
