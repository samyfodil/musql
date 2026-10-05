// This file lowers the FROM-clause form of an UPDATE whose target is a view --
// "UPDATE v SET c = <expr> FROM <sources> [WHERE ...]".
//
// # Why it is its own emitter
//
// updateFromSelect has a separate destination arm for a view:
//
//	}else if( IsView(pTab) ){
//	  for(i=0; i<pTab->nCol; i++){
//	    pList = sqlite3ExprListAppend(pParse, pList, exprRowColumn(pParse, i));
//	  }
//	  eDest = SRT_Table;
//	}                                             -- update.c:243-247
//
// Pass one projects the view's own columns then the SET expressions into an
// SRT_Table ephemeral, which auto-assigns keys, so every join row survives --
// unlike the table path's SRT_Upfrom (select.c:1366-1373), which keeps one
// entry per target rowid. So:
//
//   - the INSTEAD OF trigger fires once per join row;
//   - fires run in plan order (a view has no rowid to sort by, build.c:3026);
//   - two join rows carrying the same view row both fire, so which lands last
//     is the plan's choice -- refused (checkViewUpfromRows).
//
// update.c then points the data cursor at that ephemeral:
//
//	if( nChangeFrom ){
//	  updateFromSelect(
//	      pParse, iEph, pPk, pChanges, pTabList, pWhere, pOrderBy, pLimit
//	  );
//	  if( isView ) iDataCur = iEph;
//	}                                             -- update.c:693-700
//
// so pass two is compileViewUpdateStmt's, reading from the ephemeral: OLD.*
// (update.c:912), unchanged columns (964), affinity (983) and the fire
// (984-985). The differences are update.c's "if( nChangeFrom )" tests -- where
// SET values come from (949-955) and no pass-two WHERE (642) -- and no separate
// view materialization (629-630: "nChangeFrom==0 && isView").
//
// Fire order follows C's plan, which flips with cardinality; this engine fires
// in its own join order, so the set of fires agrees but their order can
// differ. Closing that would mean reproducing a cost-based planner.

package engine

import "fmt"

// viewUpfromDest is a derived source that is a VIEW's "UPDATE ... FROM" pass
// one -- update.c:243-247's SRT_Table destination. Present on derivedSource
// exactly when OpOpenDerived must apply the multi-match refusal below before
// opening the rows as an ordinary derived cursor.
type viewUpfromDest struct {
	// nOld is the view's own column count: how many LEADING sub-program output
	// columns are the OLD row. The SET values follow, one per assignment.
	nOld int
	// displayName is the target as the statement wrote it (schema qualifier and
	// all), for error messages -- the updateStmt's own stmt.table.
	displayName string
	// orderBlind: the INSTEAD OF programs cannot tell the order they fire in
	// (viewUpfromOrderBlind), so a view row several FROM rows match fires
	// once per match, as C does, and no multi-match refusal is needed.
	orderBlind bool
}

// checkViewUpfromRows refuses two join rows carrying the same view row: C fires
// the INSTEAD OF trigger for both, so which lands last is the plan's choice.
// Identity is hashAggKeyBytes over the OLD tuple (GROUP BY's NULL-equal,
// Int/Float-folded identity). It is a run-time error, like collapseUpfromRows',
// since the ambiguity depends on the data. Identical view rows share a bucket:
// they always match the same partners, so a repeated tuple is a multi-match.
func checkViewUpfromRows(d *viewUpfromDest, rows [][]Value) error {
	if d.orderBlind {
		for _, row := range rows {
			if len(row) < d.nOld {
				return fmt.Errorf("engine: UPDATE %s: pass one produced a %d-column row, want at least %d", d.displayName, len(row), d.nOld)
			}
		}
		return nil
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if len(row) < d.nOld {
			// Unreachable: compileViewUpdateFromSource asserts pass one's
			// arity against the same viewColumnInfos this nOld came from.
			// Reported rather than sliced, per AGENTS.md invariant 2.
			return fmt.Errorf("engine: UPDATE %s: pass one produced a %d-column row, want at least %d", d.displayName, len(row), d.nOld)
		}
		key := string(hashAggKeyBytes(nil, row[:d.nOld]))
		if _, dup := seen[key]; dup {
			return fmt.Errorf("engine: UPDATE %s: a view row matched more than one FROM row; UPDATE ... FROM against a view with an ambiguous (multi-match) join is not supported by this write path (C SQLite fires the INSTEAD OF trigger once per join row, in query-plan order)", d.displayName)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// compileViewUpdateFromSource compiles pass one: the join SELECT yielding the
// view's columns then the SET values (update.c:243-247, with the SET
// expressions appended at 258-262). It returns the snapshot the caller
// publishes as Program.WritePager (so the program is never cached), nil for a
// trigger body, whose pass one is a live sub-program.
//
// The projection is "<view>.*" so a view with two same-named columns projects
// both. The view is comma-joined last, so an outer join inside FROM cannot
// null-extend it (see buildUpdateFromSelect, upfrom4.test 120/210). pager is
// the caller's compile-time snapshot; derivedColumnInfos asks it only for
// schema facts.
func (db *DB) compileViewUpdateFromSource(stmt *updateStmt, vm *viewMeta, viewCols []columnInfo, pager *ReadOnlyPager, trig *trigCompileCtx) (*derivedSource, *ReadOnlyPager, error) {
	viewScope := vm.name
	if stmt.alias != "" {
		viewScope = stmt.alias
	}
	cols := make([]SelectColumn, 0, 1+len(stmt.sets))
	cols = append(cols, SelectColumn{Star: true, StarQualifier: viewScope})
	for _, a := range stmt.sets {
		cols = append(cols, SelectColumn{Expr: a.expr})
	}
	from := make([]FromItem, 0, len(stmt.from)+1)
	from = append(from, stmt.from...)
	// Schema is the statement's own qualifier, carried for the reason
	// buildUpdateFromSelect's identical line records in full (update_from.go,
	// citing update.c:222/228/230 and expr.c:1913): without it pass one
	// resolves the view name temp-first and joins against a TEMP view of the
	// same name while the write target is MAIN's.
	from = append(from, FromItem{Table: vm.name, Schema: stmt.schema, Alias: stmt.alias, UpdateTarget: true})
	joinCTEs, jerr := writeJoinCTEs(stmt.ctes, vm.name, stmt.table, stmt.sets, stmt.where)
	if jerr != nil {
		// writeJoinCTEs' own errors are prepare-time diagnostics, carried out
		// verbatim behind errVDBEUnsupported rather than restated here.
		return nil, nil, declineOrSemantic(jerr)
	}
	sel := &SelectStmt{CTEs: joinCTEs, Columns: cols, From: from, Where: stmt.where}

	// A trigger BODY's pass one must bind at FIRE time, not at the firing
	// statement's compile time -- see compileUpdateFromSource's identical seam
	// and vdbe_live_read.go for the C. progPager stays nil there so nothing
	// frozen reaches the body Program, which is what programReadsFrozenSnapshot
	// (vdbe_trigger.go) demands of a compiled body.
	//
	// pager stays on as the COMPILE-time pager either way, and
	// derivedSource.pager is inert for a live source (liveLower replaces both
	// program and pager at run time).
	var prog *Program
	var cerr error
	progPager := pager
	if trig != nil {
		prog, cerr = db.compileLiveSubProgram(sel, trig, nil, 0, false)
		progPager = nil
	} else {
		prog, cerr = compileSubProgram(pager, sel, nil)
	}
	if cerr != nil {
		return nil, nil, cerr // already wrapped errVDBEUnsupported
	}
	if len(prog.ColNames) != len(viewCols)+len(stmt.sets) {
		// The star expansion disagreed with viewColumnInfos about the view's
		// width; declined rather than mis-slicing a row.
		return nil, nil, fmt.Errorf("%w: UPDATE ... FROM against a view whose column layout could not be resolved", errVDBEUnsupported)
	}
	if prog.Correlated {
		// compileSubProgram was given no enclosing compiler, so nothing outer
		// can have been bound -- but a correlated derived source is
		// materialized by execWithParent rather than runSubOnce (vdbe.go's
		// OpOpenDerived arm), which is where checkViewUpfromRows sits.
		return nil, nil, fmt.Errorf("%w: correlated UPDATE ... FROM pass one over a view", errVDBEUnsupported)
	}
	// resolvedTable.cols is read only for its LENGTH (openDerivedCursor's
	// nullRow column count), and this cursor's rows are the WHOLE pass-one row:
	// the view's own columns followed by the SET values. Unlike the table
	// path's source, nothing is dropped -- SRT_Table writes the full record.
	srcCols := pager.derivedColumnInfos(sel, prog.ColNames)
	src := &derivedSource{
		prog:       prog,
		tbl:        &resolvedTable{cols: srcCols, ipkIndex: -1},
		pager:      pager,
		viewUpfrom: &viewUpfromDest{nOld: len(viewCols), displayName: stmt.table},
	}
	return src, progPager, nil
}

// compileViewUpdateFromStmt compiles "UPDATE <view> SET ... FROM <sources>
// [WHERE ...]": run the join into the ephemeral, then per entry build OLD from
// its leading columns and NEW with the SET values (trailing columns) written
// over them, apply the view's affinities, and fire the INSTEAD OF UPDATE
// triggers. Nothing is stored (update.c's write half is past "if( !isView ){",
// 1028), so changes() reads 0.
//
// The emission order is update.c's second loop, as in compileViewUpdateStmt:
//
//	sqlite3ExprCodeGetColumnOfTable(v, pTab, iDataCur, i, k);   -- :912  OLD.*
//	sqlite3VdbeAddOp3(v, OP_Column, iEph, nOff+j, k);           -- :952  SET
//	sqlite3ExprCodeGetColumnOfTable(v, pTab, iDataCur, i, k);   -- :964  unchanged
//	sqlite3TableAffinity(v, pTab, regNew);                      -- :983
//	sqlite3CodeRowTrigger(pParse, pTrigger, TK_UPDATE, pChanges,
//	    TRIGGER_BEFORE, pTab, regOldRowid, onError, labelContinue);
//	                                                            -- :984-985
//
// with iDataCur the ephemeral (update.c:698) and nOff pTab->nCol (950-951).
// Pass one already did the WHERE, the SET evaluation (read at 952, so "SET
// x=y, y=x" swaps and an aggregate SET is legal) and avoids materializing the
// view.
func (db *DB) compileViewUpdateFromStmt(stmt *updateStmt, vm *viewMeta, trig *trigCompileCtx) (*Program, error) {
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, declineOrSemantic(perr)
	}
	// The row image the trigger fires against is the VIEW's own output layout,
	// from viewColumnInfos rather than from pass one's own column list, so all
	// three routes name and type the OLD/NEW row identically -- see
	// compileViewMaterialize's identical call and its reason.
	viewCols, cerr := pager.viewColumnInfos(vm.name, viewMetaToParsed(vm))
	if cerr != nil {
		return nil, declineOrSemantic(cerr)
	}
	ci := buildColIndex(viewCols)
	setIdx := make([]int, len(stmt.sets))
	for i, a := range stmt.sets {
		idx, ok := ci[r33sFoldIdent(a.col)]
		if !ok {
			// update.c:500's own prepare-time "no such column: %s", declined
			// with that wording carried out verbatim -- exactly as
			// compileViewUpdateStmt does with the same lookup.
			return nil, fmt.Errorf("%w: no such column: %s", errVDBEUnsupported, a.col)
		}
		setIdx[i] = idx
	}
	src, writePgr, serr := db.compileViewUpdateFromSource(stmt, vm, viewCols, pager, trig)
	if serr != nil {
		return nil, serr
	}
	// The firing statement's OR clause governs the INSTEAD OF program -- the
	// one thing onError reaches for a VIEW target (update.c:984-985).
	plan, ferr := db.compileViewFirePlan(vm, viewCols, triggerUpdate, trigMemo(trig), stmt.orAction, stmt.explicitOr)
	if ferr != nil {
		return nil, ferr
	}
	if plan == nil {
		// No matching INSTEAD OF UPDATE trigger. compileViewUpdateStmt never
		// reaches this state -- viewModifyRejection raises C SQLite's own
		// "cannot modify %s because it is a view" first -- and neither does
		// this emitter, since its one caller asks the same question before
		// dispatching here. Reported rather than emitting an OpFireTriggers
		// with a nil plan, per AGENTS.md invariant 2.
		return nil, fmt.Errorf("%w: UPDATE ... FROM against a view with no INSTEAD OF UPDATE trigger", errVDBEUnsupported)
	}
	src.viewUpfrom.orderBlind = viewUpfromOrderBlind(plan)

	// The scan scope is named for the VIEW even though nothing this emitter
	// compiles reads it: every user expression (the SET right-hand sides and
	// the WHERE) went into pass one's SELECT and was resolved there. Kept so
	// the compiler is built the one way newViewScanCompiler builds it, and so a
	// later expression emitted here cannot silently resolve against a
	// rowid-bearing scope (noRowid -- build.c:3026).
	c := newViewScanCompiler(vm.name, viewCols)
	c.trig = trig
	c.pager = pager
	nv := len(viewCols)
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenDerived, P1: 0, P2: c.allocSub(), P4: src})
	rewind := c.emit(Instruction{Op: OpRewind, P1: 0})
	loopTop := c.here()

	// OLD.* -- update.c:912's sqlite3ExprCodeGetColumnOfTable over iDataCur,
	// which for a view with a FROM clause IS the ephemeral (update.c:698).
	oldBase := c.allocN(nv)
	for j := 0; j < nv; j++ {
		c.emit(Instruction{Op: OpColumn, P1: 0, P2: j, P3: oldBase + j})
	}
	// NEW starts as a copy of OLD (update.c:964's carry of every unassigned
	// column -- this emitter copies the register it just read rather than
	// re-reading the cursor, which is the same value: nothing between the two
	// touches the row), then each SET value is read out of the ephemeral at
	// offset nCol+j (update.c:950-952). Written in STATEMENT order, so a
	// repeated target ("SET a=1, a=2") lands on the last one -- update.c's
	// aXRef fill has the same last-wins shape ("aXRef[j] = i;", update.c:492).
	newBase := c.allocN(nv)
	for j := 0; j < nv; j++ {
		c.emit(Instruction{Op: OpSCopy, P1: oldBase + j, P2: newBase + j})
	}
	for i := range stmt.sets {
		c.emit(Instruction{Op: OpColumn, P1: 0, P2: nv + i, P3: newBase + setIdx[i]})
	}
	// update.c:983, over the WHOLE regNew block and UNGUARDED for a view where
	// insert.c's matching call sits behind "if( !isView )" (insert.c:1490-1491)
	// -- see compileViewUpdateStmt's own note and the oracle measurement
	// behind it.
	for j := 0; j < nv; j++ {
		c.emit(Instruction{Op: OpAffinity, P1: newBase + j, P4: viewCols[j].Aff})
	}
	// One unwritten register for both rowids: a view supplies neither
	// (build.c:3026), and trigCompileCtx.noRowid makes NEW.rowid/OLD.rowid fail
	// to resolve at COMPILE time, so nothing can read either. P5 is left 0, so
	// opFireTriggers reads the OLD rowid from P2 -- this same register.
	rowidReg := c.allocN(1)
	fp := *plan
	c.emit(Instruction{Op: OpFireTriggers, P1: newBase, P2: rowidReg, P3: oldBase, P4: &fp})
	// The NEW block is the RETURNING image, as in compileViewUpdateStmt --
	// before skipAddr, so RAISE(IGNORE) takes the returned row with it.
	colNames, rerr := c.emitViewReturning(db, vm, viewCols, stmt.returning, newBase, returningSitePerRow)
	if rerr != nil {
		return nil, rerr
	}
	// RAISE(IGNORE) abandons this row -- update.c's labelContinue, which
	// update.c:985 passes to sqlite3CodeRowTrigger as its ignoreJump. MUST be
	// set: see triggerFirePlan.skipAddr for the infinite loop a zero causes.
	fp.skipAddr = c.here()
	c.emit(Instruction{Op: OpNext, P1: 0, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: writePgr, ColNames: colNames}, nil
}

// viewUpfromOrderBlind reports whether plan's INSTEAD OF programs cannot tell
// the order they fire in: every body step is a SELECT that raises nothing but
// IGNORE and holds no subquery that could, and no WHEN raises either. Then nothing the
// statement leaves behind depends on the order C's second loop visits pass
// one's rows in (update.c:952-985), and RETURNING's row order is unspecified.
func viewUpfromOrderBlind(plan *triggerFirePlan) bool {
	for _, ct := range plan.triggers {
		if !orderBlindExpr(ct.tr.when) {
			return false
		}
		for _, bs := range ct.tr.body {
			if bs.sel == nil || len(bs.sel.Compound) != 0 || len(bs.sel.CTEs) != 0 ||
				!orderBlindExpr(bs.sel.LimitParam) || !orderBlindExpr(bs.sel.OffsetParam) {
				return false
			}
			blind := true
			walkSelectForAnchor(bs.sel, func(e Expr) {
				blind = blind && orderBlindExpr(e)
			})
			if !blind {
				return false
			}
		}
	}
	return true
}

// orderBlindExpr is a whitelist walk: false for a RAISE other than IGNORE
// (which abandons only its own row), a subquery, a window call and any node
// kind it does not know.
func orderBlindExpr(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ColumnExpr, ParamExpr:
		return true
	case RaiseExpr:
		return x.Ignore
	case InExpr:
		if x.Sub != nil {
			return false
		}
	case FuncExpr:
		if x.Over != nil {
			return false
		}
	case UnaryExpr, IsNullExpr, CollateExpr, CastExpr, BinaryExpr, BetweenExpr, LikeExpr, GlobExpr, RowExpr, CaseExpr:
	default:
		return false
	}
	ok := true
	walkExprOperands(e, func(sub Expr) {
		ok = ok && orderBlindExpr(sub)
	})
	return ok
}
