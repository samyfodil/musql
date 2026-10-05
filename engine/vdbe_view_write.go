package engine

import "fmt"

// Writes whose target is a VIEW (INSERT, UPDATE, DELETE), lowered into the
// write VM.
//
// A view write is not a special statement in SQLite. sqlite3Insert,
// sqlite3Update and sqlite3DeleteFrom resolve the target like a table and set
// one flag:
//
//	isView = IsView(pTab);                        -- insert.c:980
//	isView = IsView(pTab);                        -- update.c:371
//	isView = IsView(pTab);                        -- delete.c:353
//
// For INSERT it suppresses exactly three things: opening cursors
// (insert.c:1272-1274), the affinity pass (insert.c:1490-1492, so NEW keeps the
// VALUES expressions' types), and the rowid/constraint/store block
// (insert.c:1501). The INSTEAD OF trigger is fired by the ordinary BEFORE call
// outside those guards:
//
//	/* Fire BEFORE or INSTEAD OF triggers */
//	sqlite3CodeRowTrigger(pParse, pTrigger, TK_INSERT, 0, TRIGGER_BEFORE,
//	    pTab, regCols-pTab->nCol-1, onError, endOfLoop);
//	                                              -- insert.c:1494-1496
//
// because sqlite3BeginTrigger stores every INSTEAD OF trigger as a BEFORE
// trigger (trigger.c:259-266, :277).
//
// So the row image is a register block, the fire is the ordinary
// OpFireTriggers, and the bodies are ordinary compileTriggerBodyStmt programs
// (vdbe_trigger.go). The differences: the row layout comes from the view's
// output columns (viewColumnInfos, view_trigger.go), and there is no rowid.
//
// UPDATE and DELETE first materialize the view (sqlite3MaterializeView,
// delete.c:142-170; called at delete.c:428-435 and update.c:630-636) and scan
// it; that is a compiled sub-Program behind OpOpenDerived, the same seam
// compileInsertSelectWrite uses. INSERT materializes nothing.

// viewModifyRejection is sqlite3IsReadOnly's view arm: a write against a view
// with no matching INSTEAD OF trigger is rejected outright.
//
//	if( IsView(pTab)
//	 && (pTrigger==0 || (pTrigger->bReturning && pTrigger->pNext==0))
//	){
//	  sqlite3ErrorMsg(pParse,"cannot modify %s because it is a view",pTab->zName);
//	  return 1;
//	}
//	                                              -- delete.c:124-130
//
// It is a compile-time error, not a decline: the VM never runs, so changes()
// keeps the previous statement's count, as sqlite3VdbeHalt only publishes for
// a VM that reached RUN state (vdbeaux.c:3335, :3481). The wording is
// db.viewModifyError's.
//
// It is asked first, before any shape question, as in C: a RETURNING clause
// alone does not save a view (the synthetic RETURNING trigger is in the same
// list, trigger.c:68-78, hence bReturning above), and an unmodelled shape
// errors on the view rather than declining.
func (db *DB) viewModifyRejection(vm *viewMeta, displayName string, event triggerEventKind) error {
	if len(db.matchingInsteadOfTriggers(vm.name, vm.isTemp, event)) != 0 {
		return nil
	}
	if err := db.viewModifyError(displayName); err != nil {
		return err
	}
	// viewModifyError answers nil only for a name that is not a view at all,
	// which every caller has already ruled out by resolving vm from it. Spelled
	// out rather than returned as-is: a nil error here would be a nil program
	// with nothing to report, which is the one outcome compileWrite cannot use.
	return fmt.Errorf("engine: cannot modify %s because it is a view", vm.name)
}

// compileViewFirePlan compiles the INSTEAD OF triggers on view vm for event
// into a fire plan whose OLD/NEW image is the view's output columns. It is
// compileTriggerFirePlanOrconf's (vdbe_trigger.go) view twin, sharing the
// TriggerPrg memo that bounds a cyclic trigger graph (trigger.c:1269/1376).
// The triggers come from matchingInsteadOfTriggers, and the pseudo-row has
// noRowid (a view exposes none, build.c:3026).
// orconf/orconfSet carry the firing statement's OR clause into the INSTEAD OF
// program. For a view that is the only thing onError reaches in C:
// sqlite3CodeRowTrigger(..., onError, ...) (update.c:984-985) makes it
// getRowTrigger's cache key (trigger.c:1376) and pParse->eOrconf
// (trigger.c:1137). Everything else consuming onError is inside "if( !isView
// ){" (update.c:1028), and sqlite3TriggerColmask returns 0xffffffff for a view
// before looking at orconf (trigger.c:1552-1553). DELETE passes the literal
// OE_Default (delete.c:650-651).
func (db *DB) compileViewFirePlan(vm *viewMeta, viewCols []columnInfo, event triggerEventKind, memo *triggerPrgMemo, orconf conflictAction, orconfSet bool) (*triggerFirePlan, error) {
	trs := db.matchingInsteadOfTriggers(vm.name, vm.isTemp, event)
	if len(trs) == 0 {
		return nil, nil
	}
	// Both wrapped as DECLINES rather than raised: they are trigger.go's own
	// prepare-time checks, so the wrap carries their wording through unchanged
	// instead of this compiler restating it from a different layer.
	if err := db.checkTriggerTempScope(trs); err != nil {
		return nil, declineOrSemantic(err)
	}
	if err := db.checkTriggerBodyTables(trs); err != nil {
		return nil, declineOrSemantic(err)
	}
	// "PRAGMA trusted_schema=OFF" bans an unsafe function or virtual table in
	// an INSTEAD OF body exactly as in a table trigger's
	// (compileTriggerFirePlanOrconf): sqlite3ExprFunctionUsable rejects it
	// while the firing statement's prepare codes the body. Raised, not
	// declined -- it is C SQLite's own error, and the table path raises it.
	for _, tr := range trs {
		if err := db.checkTrustedSchemaTrigger(tr); err != nil {
			return nil, err
		}
	}
	hasNew := event == triggerInsert || event == triggerUpdate
	hasOld := event == triggerDelete || event == triggerUpdate
	trig := &trigCompileCtx{cols: viewCols, noRowid: true, hasNew: hasNew, hasOld: hasOld, memo: memo,
		orconf: orconf, orconfSet: orconfSet}
	plan := &triggerFirePlan{nCols: len(viewCols), hasNew: hasNew, hasOld: hasOld}
	for _, tr := range trs {
		key := triggerPrgKey{tr: tr, orconf: orconf, orconfSet: orconfSet}
		// getRowTrigger's own lookup (trigger.c:1376).
		if done, ok := memo.byTrigger[key]; ok {
			plan.triggers = append(plan.triggers, done)
			continue
		}
		ct := &compiledTrigger{tr: tr}
		// Linked BEFORE the body is coded, which is what terminates a cycle --
		// trigger.c:1269-1270. See triggerPrgMemo.
		memo.byTrigger[key] = ct
		trig.isTemp = tr.isTemp // see compileTriggerFirePlanFor's identical line
		if tr.when != nil {
			w, err := db.compileTriggerGuard(tr.when, trig)
			if err != nil {
				return nil, err
			}
			ct.when = w
		}
		for _, bs := range tr.body {
			prog, err := db.compileTriggerBodyStmt(bs, trig)
			if err != nil {
				return nil, err
			}
			ct.body = append(ct.body, prog)
		}
		plan.triggers = append(plan.triggers, ct)
	}
	return plan, nil
}

// compileViewInsertStmt compiles "INSERT INTO <view> [(cols)] VALUES (...)":
// for each tuple, the expressions into registers, the view-width NEW row
// assembled from them (unnamed columns NULL, first-wins for a doubly named
// one), and one OpFireTriggers for the INSTEAD OF INSERT triggers. No cursor,
// record or store.
//
// The column mapping (insert.c's aTabColMap, insert.c:1078-1084) and arity
// errors are settled at compile time, using view_trigger.go's viewInsertColIdx
// and checkViewInsertArity.
func (db *DB) compileViewInsertStmt(stmt *insertStmt, vm *viewMeta, trig *trigCompileCtx) (*Program, error) {
	// sqlite3IsReadOnly FIRST -- see viewModifyRejection. insert.c:1009 asks it
	// before sqlite3GetVdbe (insert.c:1015) allocates a VM at all, so it
	// precedes every shape question below.
	if rerr := db.viewModifyRejection(vm, stmt.table, triggerInsert); rerr != nil {
		return nil, rerr
	}
	// Declined: an UPSERT (C's prepare-time "cannot UPSERT a view",
	// insert.c:1296; declining keeps changes() unchanged as for any
	// compile-time refusal); a trigger body's INSERT ... SELECT (its source
	// must be read as of the firing, vdbe_live_read.go, and may name
	// NEW./OLD.); a leading WITH, or a subquery in a tuple (they need a
	// pager this emitter does not install).
	//
	// DEFAULT VALUES is served: the parser models it as one empty tuple, and
	// the coding loop gives every column OP_Null as C does for a column with
	// no source (insert.c:1400-1410; sqlite3ColumnExpr is 0 for a view).
	//
	// An explicit OR clause is served: for a view it reaches only the
	// INSTEAD OF program (insert.c:1494-1496); every other onError consumer
	// is inside "if( !isView ){" (insert.c:1500), and the AFTER call
	// (insert.c:1605-1607) codes nothing for a view. compileViewFirePlan
	// takes it (TestR39CViewConflictClause).
	if stmt.upsert != nil ||
		(stmt.selectStmt != nil && trig != nil) ||
		len(stmt.ctes) > 0 ||
		rowExprsContainSubquery(stmt.rows) {
		return nil, fmt.Errorf("%w: INSERT into a view: shape not lowered", errVDBEUnsupported)
	}
	// The view's output layout is a SCHEMA fact -- resolved once, at compile
	// time. The write-plan cache is keyed on db.schemaGen and db.txGen
	// (cachedWriteProgram), and every DDL that could move a view's columns
	// bumps schemaGen, so a cached program can never hold a stale layout.
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, declineOrSemantic(perr)
	}
	viewCols, cerr := pager.viewColumnInfos(vm.name, viewMetaToParsed(vm))
	if cerr != nil {
		// A real error (a view over a missing table, an unknown COLLATE, a
		// trusted_schema refusal). Declined rather than raised, so
		// viewColumnInfos' own wording is what surfaces and the refusal stays
		// a COMPILE-time one, leaving changes() alone (see
		// viewModifyRejection).
		return nil, declineOrSemantic(cerr)
	}
	colIdx, ierr := viewInsertColIdx(stmt.cols, vm.name, viewCols)
	if ierr != nil {
		return nil, declineOrSemantic(ierr)
	}
	for _, rowExprs := range stmt.rows {
		if aerr := checkViewInsertArity(stmt.cols, vm.name, len(viewCols), colIdx, len(rowExprs)); aerr != nil {
			return nil, declineOrSemantic(aerr)
		}
	}
	plan, ferr := db.compileViewFirePlan(vm, viewCols, triggerInsert, trigMemo(trig), stmt.orAction, stmt.explicitOr)
	if ferr != nil {
		return nil, ferr
	}

	c := &compiler{trig: trig}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// OpFireTriggers reads a NEW rowid register unconditionally when the plan
	// supplies NEW. A view has none, so this register is allocated, never
	// written and never read: trigCompileCtx.noRowid makes NEW.rowid fail to
	// resolve at COMPILE time, so no body can reach the value.
	rowidReg := c.allocN(1)
	// srcSlot[j] is the tuple slot that feeds view column j, or -1 for a
	// column the statement names no value for. It is insert.c's aTabColMap
	// (insert.c:1078-1084) with the SAME first-wins rule --
	//
	//	if( aTabColMap[j]==0 ) aTabColMap[j] = i+1;
	//	                                              -- insert.c:1083
	//
	// -- and, for the positional form, insert.c's "k = i - nHidden"
	// (insert.c:1420), where nHidden is 0 for a view.
	srcSlot := make([]int, len(viewCols))
	for j := range srcSlot {
		srcSlot[j] = j // positional
	}
	if stmt.cols != nil {
		for j := range srcSlot {
			srcSlot[j] = -1
		}
		for i, idx := range colIdx {
			if srcSlot[idx] < 0 {
				srcSlot[idx] = i
			}
		}
	}
	if stmt.selectStmt != nil {
		return db.emitViewInsertSelect(c, stmt, vm, viewCols, colIdx, srcSlot, plan, rowidReg, pager)
	}
	// One RETURNING row per VALUES tuple, so the names come back from each
	// iteration and every iteration agrees on them (the same list, resolved
	// once per row from the same view columns).
	var colNames []string
	for _, rowExprs := range stmt.rows {
		base := c.allocN(len(viewCols))
		// The coding loop runs over the target's columns, not the tuple:
		//
		//	for(i=0; i<pTab->nCol; i++, iRegStore++){
		//	  ...
		//	  if( pColumn ){
		//	    j = aTabColMap[i];
		//	    if( j==0 ){ /* default value */ ... continue; }
		//	    k = j - 1;
		//	  }else ... k = i - nHidden;
		//	  ...
		//	  Expr *pX = pList->a[k].pExpr;
		//	  int y = sqlite3ExprCodeTarget(pParse, pX, iRegStore);
		//	                                      -- insert.c:1363-1436
		//
		// So a tuple slot no column maps to is never evaluated ("INSERT INTO
		// v(a,a) VALUES(1, abs(-9223372036854775807-1))" succeeds), and
		// evaluation is in column order. A column with no source gets its
		// DEFAULT, which for a view is OP_Null (insert.c:1404-1407).
		//
		// No affinity pass follows: insert.c:1490 is behind "if( !isView )", so
		// "INSERT INTO v VALUES(1,5)" gives a TEXT view column the INTEGER 5
		// (TestViewF3InsertSkipsAffinity).
		for j := 0; j < len(viewCols); j++ {
			if srcSlot[j] < 0 {
				c.emit(Instruction{Op: OpNull, P2: base + j})
				continue
			}
			r, eerr := c.compileExpr(rowExprs[srcSlot[j]])
			if eerr != nil {
				return nil, eerr
			}
			if r != base+j {
				c.emit(Instruction{Op: OpSCopy, P1: r, P2: base + j})
			}
		}
		// A per-row COPY of the plan so this row's RAISE(IGNORE) jumps to THIS
		// row's end -- the same reason compileInsertStmt copies its own fire
		// plans per row.
		fp := *plan
		c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: rowidReg, P3: -1, P4: &fp})
		// Per ROW, inside the unrolled VALUES loop and before this row's
		// skipAddr: the NEW block is the RETURNING image and a body's
		// RAISE(IGNORE) abandons the returned row with the row. See
		// emitViewReturning.
		// returningSiteOnce: an UNROLLED VALUES list, emitted once per tuple --
		// the table path's own site for that shape.
		names, rerr := c.emitViewReturning(db, vm, viewCols, stmt.returning, base, returningSiteOnce)
		if rerr != nil {
			return nil, rerr
		}
		colNames = names
		fp.skipAddr = c.here()
	}
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, ColNames: colNames}, nil
}

// emitViewInsertSelect finishes compileViewInsertStmt for "INSERT INTO <view>
// [(cols)] SELECT ...": the source compiles to a sub-Program opened with
// OpOpenDerived and scanned, and each row fills the same NEW block and fire as
// the VALUES form. C forks only on where each column's value comes from:
//
//	if( useTempTable ){
//	  sqlite3VdbeAddOp3(v, OP_Column, srcTab, k, iRegStore);
//	}else if( pSelect ){
//	  if( regFromSelect!=regData ){
//	    sqlite3VdbeAddOp2(v, OP_SCopy, regFromSelect+k, iRegStore);
//	  }
//	}else{
//	  sqlite3ExprCode(pParse, pList->a[k].pExpr, iRegStore);
//	}
//	                                              -- insert.c:1423-1433
//
// A view target always has a trigger, so the source is always materialized
// first (useTempTable, insert.c:1167-1169), and a body writing the source table
// cannot see its own rows. pager's frozen image is declared as
// Program.WritePager so cachedWriteProgram never reuses this program.
func (db *DB) emitViewInsertSelect(c *compiler, stmt *insertStmt, vm *viewMeta, viewCols []columnInfo,
	colIdx, srcSlot []int, plan *triggerFirePlan, rowidReg int, pager *ReadOnlyPager) (*Program, error) {
	// "FROM <fts5 table>('query')" is fts5's table-valued call form, rewritten
	// into the plain scan + MATCH it means before anything compiles it --
	// execSelect does this at the top of its own body (query.go), and this
	// route reaches the compiler WITHOUT going through execSelect, so the
	// rewrite has to be asked for explicitly here. See
	// compileInsertSelectWrite's identical call for the measurement.
	srcSel := pager.fts5RewriteTableFunc(stmt.selectStmt)
	prog, serr := compileSubProgram(pager, srcSel, nil)
	if serr != nil {
		// DECLINED, never raised -- including an errVDBESemantic. This is the
		// opposite of compileInsertSelectWrite's top-level arm, and the reason
		// is compileViewMaterialize's: the read compiler's own spelling for a
		// rejection inside a view INSERT's source SELECT has never been
		// measured against the oracle, so it surfaces as a shape this emitter
		// does not model rather than as a semantic error worded from a
		// different layer.
		return nil, declineOrSemantic(serr)
	}
	if aerr := checkViewInsertArity(stmt.cols, vm.name, len(viewCols), colIdx, len(prog.ColNames)); aerr != nil {
		// The C's own PREPARE-time check, on the SELECT's column count:
		// "nColumn = pSelect->pEList->nExpr;" (insert.c:1154) feeds
		// insert.c:1249-1258's two wordings. Checked HERE rather than per row,
		// so a source that yields NO rows still reports it: the count asked for
		// is len(prog.ColNames), the SELECT's DECLARED column count, not a row
		// width. Declined rather than raised, so the wording stays
		// checkViewInsertArity's own.
		return nil, declineOrSemantic(aerr)
	}
	srcCols := pager.derivedColumnInfos(srcSel, prog.ColNames)
	srcCursor := c.allocCursor()
	src := &derivedSource{prog: prog, tbl: &resolvedTable{cols: srcCols, ipkIndex: -1}, pager: pager}
	c.emit(Instruction{Op: OpOpenDerived, P1: srcCursor, P2: c.allocSub(), P4: src})

	base := c.allocN(len(viewCols))
	rewind := c.emit(Instruction{Op: OpRewind, P1: srcCursor})
	loopTop := c.here()
	// Column order, not source order -- insert.c:1363's loop runs over the
	// target's columns, and a source column NO view column maps to is never
	// read. See compileViewInsertStmt's own coding loop for the two oracle
	// measurements that ordering was checked against.
	for j := 0; j < len(viewCols); j++ {
		if srcSlot[j] < 0 {
			c.emit(Instruction{Op: OpNull, P2: base + j})
			continue
		}
		c.emit(Instruction{Op: OpColumn, P1: srcCursor, P2: srcSlot[j], P3: base + j})
	}
	// ONE plan copy for the whole loop, not one per row: unlike the VALUES
	// spelling -- where each tuple is a separately emitted body with its own
	// end address -- this is a single body executed per source row, so there is
	// one row end to patch. A body's RAISE(IGNORE) jumps to it and the scan
	// advances.
	fp := *plan
	c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: rowidReg, P3: -1, P4: &fp})
	// returningSitePerRow: ONE block inside the source loop, which is the same
	// site -- and the same shape -- the table path's "INSERT ... SELECT ...
	// RETURNING" uses. Emitted before skipAddr so a body's RAISE(IGNORE) takes
	// the returned row with it, exactly as in the VALUES spelling.
	colNames, rerr := c.emitViewReturning(db, vm, viewCols, stmt.returning, base, returningSitePerRow)
	if rerr != nil {
		return nil, rerr
	}
	fp.skipAddr = c.here()
	c.emit(Instruction{Op: OpNext, P1: srcCursor, P2: loopTop})
	c.patch(rewind, c.here())

	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: pager, ColNames: colNames}, nil
}

// ---- INSTEAD OF UPDATE / DELETE: the MATERIALIZED view scan ----

// compileViewMaterialize is sqlite3MaterializeView (delete.c:142-170): a
// "SELECT * FROM <db>.<view>" run into an ephemeral table:
//
//	pFrom = sqlite3SrcListAppend(pParse, 0, 0, 0);
//	pFrom->a[0].zName = sqlite3DbStrDup(db, pView->zName);          -- delete.c:157
//	pFrom->a[0].u4.zDatabase = sqlite3DbStrDup(db, db->aDb[iDb].zDbSName);
//	                                                                -- delete.c:161
//	pSel = sqlite3SelectNew(pParse, 0, pFrom, pWhere, 0, 0, pOrderBy,
//	                        SF_IncludeHidden, pLimit);              -- delete.c:165
//	sqlite3SelectDestInit(&dest, SRT_EphemTab, iCur);               -- delete.c:167
//	sqlite3Select(pParse, pSel, &dest);                             -- delete.c:168
//
// (a NULL result list is "*", select.c:144-147). Here the ephemeral is an
// OpOpenDerived row source.
//
// The WHERE is not folded in: C duplicates it (delete.c:155) and still
// applies its own copy over the ephemeral (delete.c:443, :526), so applying
// it once at the scan selects the same rows, without evaluating a random()
// WHERE twice.
//
// Name resolution does differ: the materialization's FROM item has no alias,
// while the caller's WHERE resolves against pTabList, where only the alias
// exists (delete.c:440-443):
//
//	DELETE FROM v AS q WHERE q.a=2   -- ERROR  (UPDATE v AS q ... likewise)
//	DELETE FROM v AS q WHERE a=2     -- works
//	DELETE FROM v      WHERE v.a=2   -- works
//
// Callers refuse an aliased target whose expressions could tell the names
// apart and otherwise ignore the alias (viewAliasedTargetIgnorable).
//
// live is compileInsertSelectWrite's flag: a trigger body's view write
// materializes as of the firing (vdbe_live_read.go), as C rebuilds the
// ephemeral in each SubProgram invocation.
func (db *DB) compileViewMaterialize(schema string, vm *viewMeta, trig *trigCompileCtx) ([]columnInfo, *derivedSource, error) {
	live := trig != nil
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, nil, declineOrSemantic(perr)
	}
	// The row image the trigger fires against is the VIEW's own output layout,
	// exactly as for INSERT (compileViewInsertStmt) and for a plain read of the
	// view (viewOutputRows, view.go). Taken from viewColumnInfos rather than
	// from the compiled program's own columns so every route names and types
	// the OLD/NEW row identically.
	viewCols, cerr := pager.viewColumnInfos(vm.name, viewMetaToParsed(vm))
	if cerr != nil {
		return nil, nil, declineOrSemantic(cerr)
	}
	sel := &SelectStmt{
		Columns: []SelectColumn{{Star: true}},
		// schema is the STATEMENT's own qualifier, carried through so the
		// materialization resolves in the catalog the write target resolved
		// in. fromItemScope and writeTargetScope are ONE function
		// (scopeOfQualifier, temp_schema.go), so the two lookups cannot pick
		// different catalogs -- which is what makes carrying it correct rather
		// than merely faithful to delete.c:161's own zDatabase. Verified for
		// "main." and "temp." both, over a TEMP view with a TEMP trigger.
		From: []FromItem{{Table: vm.name, Schema: schema}},
	}
	prog, serr := compileSubProgram(pager, sel, nil)
	if serr != nil {
		// Declined, never raised, even errVDBESemantic: viewOutputRows owns
		// this family of rejections (missing table, unknown COLLATE,
		// trusted_schema, circular view, column-count mismatch) with its own
		// wording and markPrepareFailed bookkeeping (view.go,
		// view_trigger.go). compileViewInsertStmt does the same.
		return nil, nil, declineOrSemantic(serr)
	}
	if len(prog.ColNames) != len(viewCols) {
		// Unreachable: both sides expand the same view against the same pager
		// (resolveViewColumns mirrors viewColumnInfos). Declined rather than
		// trusted, since everything below indexes one by the other's width.
		return nil, nil, fmt.Errorf("%w: view %s materialized to %d columns, its layout says %d", errVDBEUnsupported, vm.name, len(prog.ColNames), len(viewCols))
	}
	if live {
		if prog.Correlated {
			// See compileInsertSelectWrite's identical refusal: a correlated
			// derived source is materialized by execWithParent rather than
			// runSubOnce, the one path liveLower does not sit on, so it would
			// silently read the FROZEN twin.
			return nil, nil, fmt.Errorf("%w: correlated live view materialization", errVDBEUnsupported)
		}
		prog.LiveSource, prog.LiveTrig = sel, trig // see compileInsertSelectWrite
	}
	// resolvedTable.cols is read only for its LENGTH (openDerivedCursor's
	// nullRow column count); the rows are indexed positionally. viewCols is
	// therefore both correct and the one list the scan scope below also uses,
	// so a cursor read and a name resolution cannot drift apart.
	src := &derivedSource{prog: prog, tbl: &resolvedTable{cols: viewCols, ipkIndex: -1}, pager: pager}
	return viewCols, src, nil
}

// newViewScanCompiler is newWriteScanCompilerAs' (vdbe_write.go) view twin: one
// scope over the MATERIALIZED view's columns, bound to the derived cursor the
// caller then opens as cursor 0.
//
// noRowid is the only field that differs from a table's scan scope, and it is
// the whole of a view's rowid story: "p->tabFlags |= TF_NoVisibleRowid; /* Never
// allow rowid in view */" (build.c:3026), read back by resolve.c:564's
// "if( sqlite3IsRowid(zCol) && VisibleRowid(pTab) )" -- whose else arm sets
// "iCol = pTab->nCol", i.e. no match, i.e. "no such column: rowid". So
// "DELETE FROM v WHERE rowid=1" is an error on the oracle, and this scope makes
// it one here rather than reading the ephemeral's own (meaningless) rowid.
func newViewScanCompiler(name string, viewCols []columnInfo) *compiler {
	scope := compileScope{
		tableScope: tableScope{name: name, cols: viewCols, colIndex: buildColIndex(viewCols), offset: 0, noRowid: true},
		cursor:     0,
	}
	return &compiler{scopes: []compileScope{scope}, nCursor: 1}
}

// viewWriteScan is what beginViewWriteScan hands its two callers: the compiler
// with the loop head already emitted, the view's column layout, the register
// base holding the OLD row, the address of the OpRewind to patch to the loop's
// end, and the WritePager the finished Program must carry.
type viewWriteScan struct {
	c        *compiler
	viewCols []columnInfo
	oldBase  int
	rewind   int
	writePgr *ReadOnlyPager
}

// beginViewWriteScan is the prologue compileViewDeleteStmt and
// compileViewUpdateStmt share: the materialization, the scan compiler, the
// subquery pager, and the OpInit/OpOpenDerived/OpRewind head of the loop plus
// the per-row OLD read.
func (db *DB) beginViewWriteScan(schema string, vm *viewMeta, subq bool, trig *trigCompileCtx) (*viewWriteScan, error) {
	viewCols, src, merr := db.compileViewMaterialize(schema, vm, trig)
	if merr != nil {
		return nil, merr
	}
	// The scope is named for the VIEW, never for an alias: both callers decline
	// an aliased target (see compileViewMaterialize's doc comment for the C and
	// the four measured spellings), so vm.name is the only name a WHERE here
	// can legally qualify with -- which is also the name sqlite3MaterializeView
	// puts in its own FROM item (delete.c:159).
	c := newViewScanCompiler(vm.name, viewCols)
	c.trig = trig
	// The pager lets a three-part "main.v5.b" in the WHERE or a SET
	// resolve: compileColumn validates the qualifier with
	// pager.qualifierResolves and resolves the two-part form, as resolve.c
	// maps zDb to a schema (resolve.c:322-329) and only uses it to skip
	// other schemas' items (resolve.c:420-422). The subq block below
	// replaces it with the subquery snapshot when needed; otherwise only
	// schema-qualified references read it (dbIdxForSchema returns
	// (0, false) for an empty Schema).
	c.pager = src.pager
	var writePgr *ReadOnlyPager
	if trig == nil {
		// The materialization is a frozen snapshot taken at compile time;
		// declaring it stops cachedWriteProgram from reusing this program,
		// which would be a wrong answer:
		//
		//	CREATE VIEW v AS SELECT a,c FROM b;
		//	CREATE TRIGGER vd INSTEAD OF DELETE ON v
		//	  BEGIN INSERT INTO log VALUES(old.a); END;
		//	INSERT INTO b VALUES(1,1); DELETE FROM v;   -- logs 1
		//	INSERT INTO b VALUES(2,2); DELETE FROM v;   -- must log 1 AND 2
		//
		// (TestViewWriteIsNotCachedAcrossAChangedBase). A trigger body has no
		// frozen view: its materialization is a LiveSource re-lowered per
		// firing (vdbe_live_read.go).
		writePgr = src.pager
	}
	if subq {
		// Identical to compileDeleteStmt's own block, including the live/frozen
		// split: a top-level statement's WHERE/SET subqueries read the
		// PRE-STATEMENT image (db.writeSubqueryPager, below), while a trigger
		// BODY's must bind at fire time (vdbe_live_read.go). A leading WITH
		// would have to keep
		// the frozen route, and is declined by both callers anyway.
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, declineOrSemantic(perr)
		}
		c.pager = sp
		if trig != nil {
			c.liveDB = db
		} else {
			writePgr = sp
		}
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenDerived, P1: 0, P2: c.allocSub(), P4: src})
	rewind := c.emit(Instruction{Op: OpRewind, P1: 0})
	// The OLD row, read out of the materialized cursor at the top of every
	// iteration -- delete.c:803's "sqlite3ExprCodeGetColumnOfTable(v, pTab,
	// iDataCur, iCol, iOld+kk+1)" and update.c:912's identical call, both of
	// which read iDataCur, which for a view IS the ephemeral (delete.c:432's
	// "iDataCur = iIdxCur = iTabCur"; update.c hands iDataCur straight to
	// sqlite3MaterializeView at :632).
	oldBase := c.allocN(len(viewCols))
	for j := range viewCols {
		c.emit(Instruction{Op: OpColumn, P1: 0, P2: j, P3: oldBase + j})
	}
	return &viewWriteScan{c: c, viewCols: viewCols, oldBase: oldBase, rewind: rewind, writePgr: writePgr}, nil
}

// emitViewWhere emits the WHERE over the OLD row, returning the jump to patch
// to the row's end (or -1 with no WHERE).
//
// It runs in the same loop as the fire. C uses two loops for a view (bComplex,
// delete.c:358; no WHERE_ONEPASS_MULTIROW, delete.c:498, update.c:732-740; a
// rowid FIFO between them), but nothing a WHERE can contain here observes the
// difference: rows come from the materialization, and a WHERE subquery reads
// the frozen pre-statement pager. changes() in a WHERE could tell, the same
// approximation compileDeleteStmt makes for tables.
func emitViewWhere(c *compiler, where Expr) (int, error) {
	if where == nil {
		return -1, nil
	}
	// See compileDeleteStmt's identical assignment: this is the statement's own
	// top-level WHERE, the same WhereClause tag-20220128a's "pWC->op==TK_AND"
	// guard sees.
	c.inWhereConjunct = true
	// ...and its identical rowLiveSpan, for the identical reason: this WHERE is
	// coded inside the scan loop with cursor 0 -- the MATERIALIZED view, which
	// is what iDataCur is for a view write (delete.c:432, update.c:632) --
	// standing on the row, so a CORRELATED subquery beneath it may read that
	// cursor. See vdbe_write_rowlive.go.
	undoRowLive := c.whereSpan()
	wReg, werr := c.compileExpr(where)
	undoRowLive()
	if werr != nil {
		return -1, werr
	}
	jump := c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	c.inWhereConjunct = false
	return jump, nil
}

// viewAliasedTargetIgnorable reports whether an "AS <alias>" on a view
// UPDATE/DELETE target can be ignored, the only way to serve one: C resolves
// the expressions in two scopes. sqlite3MaterializeView's FROM item has the
// view's name and no alias (delete.c:159-161), while the caller's WHERE
// resolves against pTabList, where the alias replaces the name:
//
//	if( pItem->zAlias!=0 ){
//	  if( sqlite3StrICmp(zTab, pItem->zAlias)!=0 ){
//	    continue;
//	  }
//	}else if( sqlite3StrICmp(zTab, pTab->zName)!=0 ){
//	                                              -- resolve.c:424-429
//
// So only unqualified references resolve in both
// (compat-harness/view_or_alias_promoted_test.go):
//
//	DELETE FROM v AS q WHERE a=2      -- works       (UPDATE likewise)
//	DELETE FROM v AS q WHERE q.a=2    -- ERROR       (UPDATE likewise)
//	DELETE FROM v AS q WHERE v.a=2    -- ERROR       (UPDATE likewise)
//	DELETE FROM v      WHERE v.a=2    -- works
//
// This scan names its scope for the view, so "v." would wrongly resolve; a
// qualifier naming the view or alias is refused, as is any subquery (not
// walked). SET right-hand sides are checked too: update.c resolves pChanges
// against pTabList alone, so "UPDATE v AS q SET a=q.a" is accepted by C but
// this scope cannot see "q".
func viewAliasedTargetIgnorable(alias, viewName string, exprs ...Expr) bool {
	for _, e := range exprs {
		if !viewAliasFreeExpr(e, alias, viewName) {
			return false
		}
	}
	return true
}

// viewAliasFreeExpr is viewAliasedTargetIgnorable's per-expression half. Its
// traversal mirrors containsSubquery's (write_update_delete.go) term for term,
// and its default arm answers FALSE: an expression form this switch does not
// name is one whose qualifiers were not inspected, and the safe answer for an
// uninspected tree is "keep the decline".
func viewAliasFreeExpr(e Expr, alias, viewName string) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		return true
	case ColumnExpr:
		return x.Qualifier == "" ||
			(!equalFoldName(x.Qualifier, alias) && !equalFoldName(x.Qualifier, viewName))
	case SubqueryExpr, ExistsExpr:
		return false
	case FuncExpr:
		for _, a := range x.Args {
			if !viewAliasFreeExpr(a, alias, viewName) {
				return false
			}
		}
		for _, ob := range x.orderByExprs() {
			if !viewAliasFreeExpr(ob, alias, viewName) {
				return false
			}
		}
		return viewAliasFreeExpr(x.Filter, alias, viewName)
	case UnaryExpr:
		return viewAliasFreeExpr(x.X, alias, viewName)
	case BinaryExpr:
		return viewAliasFreeExpr(x.L, alias, viewName) && viewAliasFreeExpr(x.R, alias, viewName)
	case IsNullExpr:
		return viewAliasFreeExpr(x.X, alias, viewName)
	case InExpr:
		if x.Sub != nil {
			return false
		}
		if !viewAliasFreeExpr(x.X, alias, viewName) {
			return false
		}
		for _, a := range x.List {
			if !viewAliasFreeExpr(a, alias, viewName) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return viewAliasFreeExpr(x.X, alias, viewName) &&
			viewAliasFreeExpr(x.Lo, alias, viewName) && viewAliasFreeExpr(x.Hi, alias, viewName)
	case LikeExpr:
		return viewAliasFreeExpr(x.X, alias, viewName) &&
			viewAliasFreeExpr(x.Pattern, alias, viewName) && viewAliasFreeExpr(x.Escape, alias, viewName)
	case GlobExpr:
		return viewAliasFreeExpr(x.X, alias, viewName) && viewAliasFreeExpr(x.Pattern, alias, viewName)
	case CollateExpr:
		return viewAliasFreeExpr(x.X, alias, viewName)
	case CastExpr:
		return viewAliasFreeExpr(x.X, alias, viewName)
	case RowExpr:
		for _, el := range x.Elems {
			if !viewAliasFreeExpr(el, alias, viewName) {
				return false
			}
		}
		return true
	case CaseExpr:
		if !viewAliasFreeExpr(x.Base, alias, viewName) {
			return false
		}
		for _, w := range x.Whens {
			if !viewAliasFreeExpr(w.When, alias, viewName) || !viewAliasFreeExpr(w.Then, alias, viewName) {
				return false
			}
		}
		return viewAliasFreeExpr(x.Else, alias, viewName)
	default:
		return false
	}
}

// viewReturningMeta is the *tableMeta emitReturning (vdbe_write.go) needs for
// a view: its name, resolved output columns, no INTEGER PRIMARY KEY and no
// rowid, so "RETURNING rowid" over a view resolves to nothing, as in C. The
// row image is the NEW/OLD block the INSTEAD OF fire reads.
func viewReturningMeta(vm *viewMeta, viewCols []columnInfo) *tableMeta {
	return &tableMeta{name: vm.name, cols: viewCols, ipkIndex: -1, withoutRowid: true}
}

// emitViewReturning emits one RETURNING row from a view write's NEW/OLD block,
// or nothing without RETURNING. Where it is emitted is the semantics:
//
//   - before the fire plan's skipAddr, so a body's RAISE(IGNORE) drops the
//     RETURNING row too;
//   - from the NEW/OLD block, not what the body wrote ("SET b='Z'" returns 'Z'
//     whatever the body stores, and a row even if it stores nothing);
//   - so each verb's affinity rule follows: UPDATE has applied OpAffinity
//     (update.c:983, unguarded for a view), INSERT has not (insert.c:1490).
func (c *compiler) emitViewReturning(db *DB, vm *viewMeta, viewCols []columnInfo, ret []SelectColumn, rowBase int, site returningSite) ([]string, error) {
	if ret == nil {
		return nil, nil
	}
	// A RETURNING subquery whose FROM names the view is re-evaluated per
	// row. returningSubqueryLifetimes decides from the program (an
	// OpOpenRead of the modified table), but a view subquery opens the
	// base tables, which C freezes, so this must be decided from the AST:
	//
	//	x = v   ->  1|1 then 2|2   per row (the view IS the modified table)
	//	x = t   ->  1|1 then 2|1   frozen (t is some other table)
	total, namesView := returningSubqueryShape(ret, vm.name)
	switch {
	case total == 0 || namesView == 0:
		// Nothing to decide: no subqueries, or none of them names the view, in
		// which case the insn walk's own answer is right.
	case namesView == total:
		// Every one of them names the view: C re-evaluates them per row, and
		// both halves of that have to be said -- the SITE (so an unrolled
		// VALUES list resets its cache slots per tuple) and the CLASSIFICATION
		// (so the block is not wrapped in a run-once guard).
		if site == returningSiteOnce {
			site = returningSitePerRow
		}
		saved := c.retForcePerRow
		c.retForcePerRow = true
		defer func() { c.retForcePerRow = saved }()
	default:
		// MIXED, in C's own terms: one subquery naming the view (per row) beside
		// one that does not (frozen). The table path declines a mixed list for
		// the same reason -- one block, two lifetimes, and a per-block slot
		// allocator -- and this is that case arriving by a different road.
		return nil, fmt.Errorf("%w: RETURNING over a view mixes the two subquery lifetimes (%d of %d name the view)",
			errVDBEUnsupported, namesView, total)
	}
	return c.emitReturning(db, viewReturningMeta(vm, viewCols), ret, rowRegsFor(rowBase, len(viewCols)), -1, site)
}

// returningSubqueryShape counts the SUBQUERIES in a RETURNING list and how many
// of them name view in a FROM clause, at any depth. The pair is what decides
// the list's lifetime: none naming it leaves the ordinary insn-based answer
// alone, all of them naming it is C's per-row lifetime, and a split is C's
// MIXED case, which declines.
//
// Keyed on the NAME the SQL actually wrote rather than on anything resolved: a
// CTE or an alias shadowing that name makes a subquery count as naming the
// view, which costs a per-row re-evaluation or a decline and is never a wrong
// value.
func returningSubqueryShape(ret []SelectColumn, view string) (total, namesView int) {
	each := func(s *SelectStmt) {
		total++
		if selectFromNamesTable(s, view) {
			namesView++
		}
	}
	var walk func(e Expr)
	walk = func(e Expr) {
		if e == nil {
			return
		}
		switch x := e.(type) {
		case SubqueryExpr:
			each(x.Stmt)
		case ExistsExpr:
			each(x.Stmt)
		case InExpr:
			if x.Sub != nil {
				each(x.Sub)
			}
		}
		walkExprOperands(e, walk)
	}
	for _, sc := range ret {
		walk(sc.Expr)
	}
	return total, namesView
}

// selectFromNamesTable reports whether s, or anything nested inside it, has a
// FROM item spelled name.
func selectFromNamesTable(s *SelectStmt, name string) bool {
	found := false
	var sel func(s *SelectStmt)
	var expr func(e Expr)
	expr = func(e Expr) {
		if found || e == nil {
			return
		}
		switch x := e.(type) {
		case SubqueryExpr:
			sel(x.Stmt)
		case ExistsExpr:
			sel(x.Stmt)
		case InExpr:
			sel(x.Sub)
		}
		walkExprOperands(e, expr)
	}
	sel = func(s *SelectStmt) {
		if s == nil || found {
			return
		}
		for _, it := range s.From {
			if equalFoldName(it.Table, name) {
				found = true
				return
			}
			if it.Subquery != nil {
				sel(it.Subquery)
			}
			expr(it.On)
		}
		for _, sc := range s.Columns {
			expr(sc.Expr)
		}
		expr(s.Where)
		expr(s.Having)
		for i := range s.CTEs {
			sel(s.CTEs[i].Select)
		}
		for i := range s.Compound {
			sel(s.Compound[i].Stmt)
		}
	}
	sel(s)
	return found
}

// compileViewDeleteStmt compiles "DELETE FROM <view> [WHERE ...]": materialize
// the view, scan it, and fire the INSTEAD OF DELETE triggers for each row whose
// OLD image satisfies the WHERE. Nothing is deleted: delete.c:845's "if(
// !IsView(pTab) ){" guards the whole delete block (delete.c:835-837). The fire
// is the ordinary BEFORE call:
//
//	sqlite3CodeRowTrigger(pParse, pTrigger,
//	    TK_DELETE, 0, TRIGGER_BEFORE, pTab, iOld, onconf, iLabel
//	);                                            -- delete.c:809-811
//
// with onconf the literal OE_Default (delete.c:650-651), so an enclosing ON
// CONFLICT policy stops at a DELETE's trigger program; compileViewFirePlan's
// memo key settles that at compile time.
func (db *DB) compileViewDeleteStmt(stmt *deleteStmt, vm *viewMeta, trig *trigCompileCtx) (*Program, error) {
	// sqlite3IsReadOnly FIRST -- see viewModifyRejection. delete.c:388 asks it
	// before delete.c:417's sqlite3GetVdbe, so it precedes every shape question
	// below, this verb's RETURNING included.
	if rerr := db.viewModifyRejection(vm, stmt.table, triggerDelete); rerr != nil {
		return nil, rerr
	}
	// Declined:
	//   - a leading WITH: a CTE named like the view would capture the
	//     synthesized "SELECT * FROM <view>" (select.c:6028/6036);
	//   - INDEXED BY: C reports "no such index" for a view, which
	//     resolveJoinSources already raises; one place decides it;
	//   - an alias the WHERE could tell apart from the view's name
	//     (viewAliasedTargetIgnorable); otherwise the alias is ignored, as
	//     delete.c:159 ignores it.
	if len(stmt.ctes) > 0 || stmt.indexedBy != "" ||
		(stmt.alias != "" && !viewAliasedTargetIgnorable(stmt.alias, vm.name, stmt.where)) {
		return nil, fmt.Errorf("%w: DELETE from a view: shape not lowered", errVDBEUnsupported)
	}
	sc, serr := db.beginViewWriteScan(stmt.schema, vm, containsSubquery(stmt.where), trig)
	if serr != nil {
		return nil, serr
	}
	c := sc.c
	plan, ferr := db.compileViewFirePlan(vm, sc.viewCols, triggerDelete, trigMemo(trig), conflictAbort, false)
	if ferr != nil {
		return nil, ferr
	}
	whereJump, werr := emitViewWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	// OpFireTriggers reads an OLD rowid register unconditionally when the plan
	// supplies OLD. A view has none, so this register is allocated, never
	// written and never read: trigCompileCtx.noRowid makes OLD.rowid fail to
	// resolve at COMPILE time (build.c:3026), so no body can reach the value.
	rowidReg := c.allocN(1)
	fp := *plan
	c.emit(Instruction{Op: OpFireTriggers, P1: -1, P2: rowidReg, P3: sc.oldBase, P4: &fp})
	// The OLD block is this verb's RETURNING image -- delete.c's RETURNING reads
	// the row being deleted -- and it is emitted before skipAddr so a body's
	// RAISE(IGNORE) abandons the returned row too. See emitViewReturning.
	// returningSitePerRow: this emitter is a SCAN, one block run per row --
	// the same site the table path's UPDATE/DELETE use, and the same
	// once-vs-per-row subquery lifetime rule then applies unchanged.
	colNames, rerr := c.emitViewReturning(db, vm, sc.viewCols, stmt.returning, sc.oldBase, returningSitePerRow)
	if rerr != nil {
		return nil, rerr
	}
	// RAISE(IGNORE) from a body abandons this row and the scan continues --
	// delete.c's iLabel, resolved at the end of sqlite3GenerateRowDelete
	// (delete.c:877).
	fp.skipAddr = c.here()
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: sc.rewind + 1})
	c.patch(sc.rewind, c.here())
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: sc.writePgr, ColNames: colNames}, nil
}

// compileViewUpdateStmt compiles "UPDATE <view> SET c=expr[,...] [WHERE ...]":
// materialize the view, scan it, and for each row whose OLD image satisfies the
// WHERE build NEW (OLD with the SETs applied, all evaluated against OLD so
// "SET x=y, y=x" swaps), apply the view's column affinities, and fire the
// INSTEAD OF UPDATE triggers. Nothing is stored (update.c:1028). The order is
// update.c's second loop:
//
//	sqlite3ExprCodeGetColumnOfTable(v, pTab, iDataCur, i, k);   -- :912  OLD.*
//	sqlite3ExprCode(pParse, pChanges->a[j].pExpr, k);           -- :954  SET
//	sqlite3ExprCodeGetColumnOfTable(v, pTab, iDataCur, i, k);   -- :964  unchanged
//	sqlite3TableAffinity(v, pTab, regNew);                      -- :983
//	sqlite3CodeRowTrigger(pParse, pTrigger, TK_UPDATE, pChanges,
//	    TRIGGER_BEFORE, pTab, regOldRowid, onError, labelContinue);
//	                                                            -- :984-985
//
// with iDataCur the ephemeral (update.c:632). The affinity at :983 is
// unguarded, unlike insert.c:1490, so "UPDATE v SET t=5" on a TEXT view column
// gives NEW.t '5' and a "WHEN new.t='5'" fires.
//
// Each row's SET is coded beside its fire (eOnePass is off for a view,
// update.c:732-740), so row N's SET observes what row N-1's body wrote
// (TestViewF3UpdateRowAtATime). A correlated subquery reading the base table
// still sees the frozen pre-statement pager (writeSubqueryPager).
func (db *DB) compileViewUpdateStmt(stmt *updateStmt, vm *viewMeta, trig *trigCompileCtx) (*Program, error) {
	// sqlite3IsReadOnly FIRST -- see viewModifyRejection. update.c:411 asks it
	// before update.c:457's sqlite3GetVdbe, so it precedes every shape question
	// below, "UPDATE <view> ... FROM" included.
	if rerr := db.viewModifyRejection(vm, stmt.table, triggerUpdate); rerr != nil {
		return nil, rerr
	}
	// "UPDATE <view> ... FROM" has its own emitter (vdbe_view_update_from.go):
	// update.c reads each value from the join's ephemeral (:952) and skips
	// sqlite3MaterializeView (:629-630), with the data cursor on that
	// ephemeral (:697-699).
	//
	// It is dispatched before the gate below and bypasses two of its clauses,
	// both about the materialize-and-scan shape: the target alias (no
	// second WHERE resolve, update.c:642, so the alias is an ordinary FROM
	// alias) and a leading WITH (writeJoinCTEs excludes a CTE shadowing the
	// target, as the table path does, upfrom1.test 4.1/4.2). INDEXED BY
	// still declines. An explicit OR governs only the INSTEAD OF program and
	// is passed through.
	if stmt.from != nil && stmt.indexedBy == "" {
		return db.compileViewUpdateFromStmt(stmt, vm, trig)
	}
	// Declined, as in compileViewDeleteStmt: WITH, INDEXED BY (including
	// "UPDATE ... FROM ... INDEXED BY"), and an alias the WHERE or a SET
	// right-hand side could tell apart from the view name.
	//
	// An explicit OR clause reaches only the INSTEAD OF program:
	//
	//	sqlite3CodeRowTrigger(pParse, pTrigger, TK_UPDATE, pChanges,
	//	    TRIGGER_BEFORE, pTab, regOldRowid, onError, labelContinue);
	//	                                              -- update.c:984-985
	//
	// (every other consumer is under update.c:987's "if( !isView ){"), so it
	// is handed to compileViewFirePlan.
	if stmt.from != nil || len(stmt.ctes) > 0 || stmt.indexedBy != "" ||
		(stmt.alias != "" && !viewAliasedTargetIgnorable(stmt.alias, vm.name, updateStmtExprs(stmt)...)) {
		return nil, fmt.Errorf("%w: UPDATE of a view: shape not lowered", errVDBEUnsupported)
	}
	subq := containsSubquery(stmt.where) || setsContainSubquery(stmt.sets)
	sc, serr := db.beginViewWriteScan(stmt.schema, vm, subq, trig)
	if serr != nil {
		return nil, serr
	}
	c := sc.c
	// Every SET target resolves to a VIEW column index, up front. "no such
	// column: <c>" for one that is not is C SQLite's own prepare-time error
	// -- the aXRef fill (update.c:473-504) ends in the literal
	// "sqlite3ErrorMsg(pParse, \"no such column: %s\", pChanges->a[i].zEName);"
	// (update.c:500) -- so this declines carrying that exact wording, rather
	// than inventing a second spelling for the same refusal.
	ci := buildColIndex(sc.viewCols)
	setIdx := make([]int, len(stmt.sets))
	for i, a := range stmt.sets {
		idx, ok := ci[r33sFoldIdent(a.col)]
		if !ok {
			return nil, fmt.Errorf("%w: no such column: %s", errVDBEUnsupported, a.col)
		}
		setIdx[i] = idx
	}
	plan, ferr := db.compileViewFirePlan(vm, sc.viewCols, triggerUpdate, trigMemo(trig), stmt.orAction, stmt.explicitOr)
	if ferr != nil {
		return nil, ferr
	}
	whereJump, werr := emitViewWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	// The NEW row starts as a copy of OLD -- update.c:964's
	// sqlite3ExprCodeGetColumnOfTable for every column the SET does not assign
	// (this emitter copies the register it already read at :912 instead of
	// re-reading the cursor, which is the same value: nothing between the two
	// touches the materialized row).
	newBase := c.allocN(len(sc.viewCols))
	for j := range sc.viewCols {
		c.emit(Instruction{Op: OpSCopy, P1: sc.oldBase + j, P2: newBase + j})
	}
	// All right-hand sides are evaluated before any is stored, so
	// assignments are simultaneous ("SET x=y, y=x" swaps), as update.c:954
	// codes each into regNew while the cursor holds OLD. That is also
	// compiler.rowLive's promise (vdbe_write_rowlive.go), restored before
	// OpFireTriggers.
	//
	// A correlated subquery over a table the INSTEAD OF program writes is
	// lowered per row, as on the table path (emitSetValue): C evaluates each
	// row's SETs, then fires (update.c:954, :984), so row 2 sees row 1's
	// trigger writes. Not inside a trigger body (trig), for the table path's
	// reason.
	var liveRow *liveRowCtx
	if trig == nil {
		liveRow = &liveRowCtx{tbl: &tableMeta{name: vm.name, cols: sc.viewCols, withoutRowid: true}, ownSchema: c.ownSchema}
	}
	written := db.tablesALoopCanWrite(&tableMeta{name: vm.name})
	// In a trigger body the body's own seam lowers per row (liveBodyRowSubSelect);
	// the materialization is walked in the order C walks its ephemeral copy.
	savedBodySetLive := c.bodySetLive
	c.bodySetLive = trig != nil
	defer func() { c.bodySetLive = savedBodySetLive }()
	undoSetRowLive := c.rowLiveSpan()
	setReg := make([]int, len(stmt.sets))
	for i, a := range stmt.sets {
		r, eerr := c.emitSetValue(a.expr, false, liveRow, vm.name, written)
		if eerr != nil {
			// No undo: c is local to this compile and a returned error discards
			// it whole. See compileUpdateStmt's identical note.
			return nil, eerr
		}
		setReg[i] = r
	}
	undoSetRowLive()
	// In statement order, so a repeated target ("SET a=1, a=2") lands on the
	// last one written -- writeApplySetList's own rule, and update.c's aXRef
	// fill has the same last-wins shape: "aXRef[j] = i;" (update.c:492) inside
	// a loop over pChanges, so a later assignment to column j overwrites an
	// earlier one.
	for i := range stmt.sets {
		c.emit(Instruction{Op: OpSCopy, P1: setReg[i], P2: newBase + setIdx[i]})
	}
	// update.c:983, over the WHOLE regNew block rather than just the assigned
	// columns -- OP_Affinity spans it, and an unchanged column carried over
	// from the materialization is already an output of the very expression its
	// affinity was derived from, so that half is a no-op in practice.
	for j := range sc.viewCols {
		c.emit(Instruction{Op: OpAffinity, P1: newBase + j, P4: sc.viewCols[j].Aff})
	}
	// One register for both rowids: a view supplies neither (build.c:3026), and
	// trigCompileCtx.noRowid makes NEW.rowid/OLD.rowid fail to resolve at
	// COMPILE time, so nothing can read either. P5 is left 0 -- opFireTriggers
	// then reads the OLD rowid from P2 as well, which is this same unwritten
	// register.
	rowidReg := c.allocN(1)
	fp := *plan
	c.emit(Instruction{Op: OpFireTriggers, P1: newBase, P2: rowidReg, P3: sc.oldBase, P4: &fp})
	// The NEW block, affinity already applied (the OpAffinity pass above), is
	// this verb's RETURNING image. Before skipAddr, so RAISE(IGNORE) takes the
	// returned row with it. See emitViewReturning.
	colNames, rerr := c.emitViewReturning(db, vm, sc.viewCols, stmt.returning, newBase, returningSitePerRow)
	if rerr != nil {
		return nil, rerr
	}
	// RAISE(IGNORE) abandons this row -- update.c's labelContinue, which
	// update.c:985 passes to sqlite3CodeRowTrigger as its ignoreJump.
	fp.skipAddr = c.here()
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: sc.rewind + 1})
	c.patch(sc.rewind, c.here())
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: sc.writePgr, ColNames: colNames}, nil
}

// updateStmtExprs is every expression an UPDATE's own level owns: the WHERE and
// each SET right-hand side. Only viewAliasedTargetIgnorable asks for it.
func updateStmtExprs(stmt *updateStmt) []Expr {
	out := make([]Expr, 0, len(stmt.sets)+1)
	out = append(out, stmt.where)
	for _, a := range stmt.sets {
		out = append(out, a.expr)
	}
	return out
}
