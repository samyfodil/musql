package engine

import (
	"errors"
	"fmt"
)

// Triggers lowered into the write VM. A write against a table with BEFORE or
// AFTER triggers compiles a beforePlan and a firePlan and emits an
// OpFireTriggers either side of each row's store (vdbe_write.go); the handler
// runs each matching trigger's body, compiled once as a sub-program whose
// NEW.*/OLD.* are OpParam reads of the firing row (trigCompileCtx). INSTEAD OF
// triggers go through vdbe_view_write.go. A body statement the write compiler
// cannot lower makes the statement an error. triggersCompilable is the gate.
//
// "UPDATE OF <col-list>" is a compile-time filter over the matched triggers, as
// in C (checkColumnOverlap, trigger.c:781-788; compileUpdateTriggerFirePlan).
//
// A body statement writing a table with its own triggers compiles at any depth:
// its sub-program carries its own OpFireTriggers, as C codes a body INSERT with
// the ordinary sqlite3Insert (trigger.c:1160) and so that table's triggers.
// triggerPrgMemo bounds the compile over a cyclic trigger graph (C's
// TriggerPrg list, trigger.c:1269/1376), and emitInsertRowBody allocates the
// rowid after the BEFORE program, as insert.c does (:1494-1496 vs :1539; see
// compiler.insertBeforeFire).

// compiledTrigger is one trigger resolved to bytecode: an optional WHEN guard
// program and the body statements' programs, run in order per firing row.
type compiledTrigger struct {
	tr   *triggerMeta
	when *Program   // nil if no WHEN clause
	body []*Program // one per body statement, in order
}

// triggerPrgMemo is this compile's TriggerPrg list, C's Parse.pTriggerPrg on
// the top-level Parse, searched before any trigger is coded:
//
//	for(pPrg=pRoot->pTriggerPrg;
//	    pPrg && (pPrg->pTrigger!=pTrigger || pPrg->orconf!=orconf);
//	    pPrg=pPrg->pNext
//	);
//	                                              -- trigger.c:1376-1379
//
// and on a miss, codeRowTrigger links the new, still-empty entry before coding
// any body statement:
//
//	pPrg->pNext = pTop->pTriggerPrg;
//	pTop->pTriggerPrg = pPrg;
//	                                              -- trigger.c:1269-1270
//
// That order is the mechanism: a trigger whose body writes its own table
// re-enters the compiler and finds its half-built entry, getting the same
// sub-program back, so a cyclic graph compiles in finite time (without it,
// "CREATE TRIGGER tt AFTER INSERT ON t BEGIN INSERT INTO t VALUES(new.a+1);
// END" overflowed the stack). Recursion is bounded at run time, where C bounds
// it (OP_Program's identity walk and SQLITE_LIMIT_TRIGGER_DEPTH,
// vdbe.c:7499-7509; triggerEnter/maxTriggerDepth).
//
// The key includes orconf (codeTriggerProgram's "pParse->eOrconf =
// (orconf==OE_Default)?pStep->orconf:(u8)orconf;", trigger.c:1137): a
// REPLACE victim's delete triggers are coded under OE_Replace
// (compileReplaceVictimDeletePlans), so one trigger can need two compiled forms
// in one statement.
type triggerPrgKey struct {
	tr        *triggerMeta
	orconf    conflictAction
	orconfSet bool
}

type triggerPrgMemo struct {
	byTrigger map[triggerPrgKey]*compiledTrigger
}

func newTriggerPrgMemo() *triggerPrgMemo {
	return &triggerPrgMemo{byTrigger: map[triggerPrgKey]*compiledTrigger{}}
}

// trigMemo returns the memo a nested trigger-body compile must SHARE with the
// statement that is firing it (SQLite's sqlite3ParseToplevel walk), or a fresh
// one when this is the top-level statement's own compile.
func trigMemo(trig *trigCompileCtx) *triggerPrgMemo {
	if trig == nil || trig.memo == nil {
		return newTriggerPrgMemo()
	}
	return trig.memo
}

// triggerFirePlan is OpFireTriggers' payload: the compiled triggers to fire for
// one row of tbl, plus which of NEW/OLD this event supplies.
//
// The triggers are held by POINTER so a cycle in the trigger graph can be a
// cycle in the compiled structure too: the entry a memo hit hands back may
// still be empty at that instant and is filled in by the compile already in
// progress above (see triggerPrgMemo).
type triggerFirePlan struct {
	// nCols is the width of the OLD/NEW row image the parent's registers hold
	// -- the target's column count. It is a COUNT rather than the *tableMeta it
	// used to be because a VIEW's INSTEAD OF plan has no table (see
	// trigCompileCtx.cols), and the count was the only thing ever read off it.
	nCols    int
	triggers []*compiledTrigger
	hasNew   bool
	hasOld   bool
	// skipAddr is where a RAISE(IGNORE) from a fired trigger jumps: the row end,
	// abandoning the row's DML. Every emitter must set it: opFireTriggers returns it
	// verbatim and the run loop takes any target >= 0, so an unset 0 jumps to OpInit
	// and the statement loops forever (insertPlan.skipAddr uses the opposite
	// convention, 0 = fall through). Treating 0 as fall-through would turn that hang
	// into running the DML RAISE(IGNORE) abandons, a quiet wrong answer. Every
	// emitter sets it today (rowPlans; vdbe_view_write.go's three verbs).
	skipAddr int
}

// triggersCompilable reports whether every trigger on tbl for event can be
// lowered: at least one match and no INSTEAD OF. false means the caller
// declines. Timing does not matter (BEFORE is lowered), and an "UPDATE OF"
// list is a filter, not a refusal: a table whose only trigger fails the
// overlap codes no trigger machinery, as triggersReallyExist returns an empty
// mask (trigger.c:837).
//
// tbl is local, so matches require tr.tableAttachName == "": a trigger on a
// same-named attached table (trigger1.test 10.x has t4 in main, temp and an
// attachment) must not count. db.foreignTempTriggers is consulted as
// matchingTriggers does (without those filters, which are what mark an entry
// foreign); otherwise "INSERT INTO aux.t4" would not fire its foreign temp
// trigger. The rest already handles those entries (matchingTriggers,
// attachedTriggerMirrorSafe, checkTriggerTempScope's tr.isTemp).
func (db *DB) triggersCompilable(tbl *tableMeta, event triggerEventKind) bool {
	any := false
	for _, tr := range db.triggers {
		if tr.tableAttachName != "" {
			continue
		}
		if tr.insteadOf {
			if tr.tableIsTemp == tbl.isTemp && equalFoldName(tr.table, tbl.name) {
				return false // an INSTEAD OF trigger routes through view DML
			}
			continue
		}
		if tr.event != event || tr.tableIsTemp != tbl.isTemp || !equalFoldName(tr.table, tbl.name) {
			continue
		}
		any = true
	}
	for _, tr := range db.foreignTempTriggers {
		if tr.insteadOf || tr.event != event || !equalFoldName(tr.table, tbl.name) {
			continue
		}
		any = true
	}
	return any
}

// compileTriggerFirePlan compiles the AFTER triggers on tbl for event into a
// fire plan. It returns errVDBEUnsupported for any construct outside this
// slice (a body statement that will not compile, a WHEN that will not compile),
// so the caller declines the whole statement.
func (db *DB) compileTriggerFirePlan(tbl *tableMeta, event triggerEventKind, timing triggerTiming, memo *triggerPrgMemo) (*triggerFirePlan, error) {
	return db.compileTriggerFirePlanOrconf(tbl, event, timing, memo, conflictAbort, false)
}

// compileTriggerFirePlanOrconf is compileTriggerFirePlan with SQLite's OTHER
// codeRowTrigger argument spelled out: orconf/orconfSet are the ON CONFLICT
// policy this trigger program runs under (trigger.c:1137 -- see
// trigCompileCtx.orconf). orconfSet false is OE_Default, i.e. every body
// statement keeps its own clause, which is what every caller except the
// REPLACE victim delete wants.
func (db *DB) compileTriggerFirePlanOrconf(tbl *tableMeta, event triggerEventKind, timing triggerTiming, memo *triggerPrgMemo, orconf conflictAction, orconfSet bool) (*triggerFirePlan, error) {
	return db.compileTriggerFirePlanFor(tbl, event, memo, orconf, orconfSet, db.matchingTriggers(tbl.name, tbl.isTemp, event, timing))
}

// compileUpdateTriggerFirePlan is compileTriggerFirePlanOrconf with "UPDATE OF
// <col-list>" applied: only triggers whose OF list overlaps the SET list are
// coded. In C that is a compile-time name test (checkColumnOverlap,
// trigger.c:781-788) at sqlite3CodeRowTrigger (trigger.c:1502) and in
// triggersReallyExist's mask (trigger.c:837), so a table whose only trigger
// fails it gets an empty plan (nil). filterUpdateTriggers is
// checkColumnOverlap.
func (db *DB) compileUpdateTriggerFirePlan(tbl *tableMeta, timing triggerTiming, memo *triggerPrgMemo, orconf conflictAction, orconfSet bool, setCols []string) (*triggerFirePlan, error) {
	trs := filterUpdateTriggers(db.matchingTriggers(tbl.name, tbl.isTemp, triggerUpdate, timing), setCols)
	return db.compileTriggerFirePlanFor(tbl, triggerUpdate, memo, orconf, orconfSet, trs)
}

// compileTriggerFirePlanFor is the shared body: trs is the already-matched
// (and, for an UPDATE, already OF-filtered) trigger list to code.
func (db *DB) compileTriggerFirePlanFor(tbl *tableMeta, event triggerEventKind, memo *triggerPrgMemo, orconf conflictAction, orconfSet bool, trs []*triggerMeta) (*triggerFirePlan, error) {
	if len(trs) == 0 {
		return nil, nil
	}
	if err := db.checkTriggerTempScope(trs); err != nil {
		return nil, err
	}
	if err := db.checkTriggerBodyTables(trs); err != nil {
		return nil, err
	}
	// "PRAGMA trusted_schema=OFF" bans an unsafe function inside a stored
	// trigger body. That guard used to live ONLY on the route that has since
	// been deleted (its sole caller), so the compiled fire path did not
	// enforce it at all -- and deleting that route would have dropped the
	// enforcement silently rather than noisily.
	//
	// Enforced here, at COMPILE time, which is also where the C enforces it:
	// sqlite3ExprFunctionUsable rejects an SQLITE_FUNC_UNSAFE function while
	// resolving the body, and a trigger's body is coded during the FIRING
	// statement's prepare (trigger.c's codeRowTrigger), not at fire time.
	for _, tr := range trs {
		if err := db.checkTrustedSchemaTrigger(tr); err != nil {
			return nil, err
		}
	}
	hasNew := event == triggerInsert || event == triggerUpdate
	hasOld := event == triggerDelete || event == triggerUpdate
	trig := &trigCompileCtx{cols: tbl.cols, hasNew: hasNew, hasOld: hasOld, memo: memo, orconf: orconf, orconfSet: orconfSet}
	plan := &triggerFirePlan{nCols: len(tbl.cols), hasNew: hasNew, hasOld: hasOld}
	for _, tr := range trs {
		key := triggerPrgKey{tr: tr, orconf: orconf, orconfSet: orconfSet}
		// getRowTrigger's own lookup (trigger.c:1376): an entry for this
		// trigger AND THIS orconf already on the list is REUSED, cycle or not.
		if done, ok := memo.byTrigger[key]; ok {
			plan.triggers = append(plan.triggers, done)
			continue
		}
		ct := &compiledTrigger{tr: tr}
		// Linked BEFORE the body is coded, which is what terminates a cycle --
		// see triggerPrgMemo for the C's own ordering (trigger.c:1269).
		memo.byTrigger[key] = ct
		// Per trigger, not per plan: trs may mix a TEMP trigger with a MAIN
		// one on the same table and event, and only the TEMP one's body may
		// resolve into an attachment (trigCompileCtx.isTemp's own citation).
		trig.isTemp = tr.isTemp
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

// compileTriggerGuard compiles a WHEN expression into a one-column program,
// true iff the value is truthy. A WHEN with a subquery uses the live read seam
// (compiler.liveDB, vdbe_live_read.go), re-evaluated per firing as C's per-frame
// OP_Once bits give it (vdbe.c:7582), and installs c.pager, which the
// IN/row-value emitters need before compiling the subquery (compileIn; nil would
// panic). A subquery-free WHEN compiles as before.
func (db *DB) compileTriggerGuard(when Expr, trig *trigCompileCtx) (*Program, error) {
	c := &compiler{trig: trig, nQueryLoopKnown: true} // coded in the trigger program: see compiler.nQueryLoop
	if containsSubquery(when) {
		// storedBodyPager, not writeSubqueryPager: a WHEN clause is part of the
		// stored body and binds inside the trigger's own database.
		sp, perr := db.storedBodyPager()
		if perr != nil {
			return nil, declineOrSemantic(perr)
		}
		c.pager, c.liveDB = sp, db
	}
	init := c.emit(Instruction{Op: OpInit})
	c.patch(init, 1)
	r, err := c.compileExpr(when)
	if err != nil {
		if c.liveDB != nil && !errors.Is(err, errVDBEUnsupported) {
			// Every WHEN-with-a-subquery declined outright before this slice,
			// and this wrap kept the whole firing statement on the route that
			// answered them. That route is gone, so the decline IS the
			// statement's own error now. It stays a DECLINE rather than a hard
			// compile error so it travels through the channel the rest of the
			// write compiler reports through, and so the shape it names stays
			// findable in a decline census.
			return nil, fmt.Errorf("%w: trigger WHEN subquery: %v", errVDBEUnsupported, err)
		}
		return nil, err
	}
	c.emit(Instruction{Op: OpResultRow, P1: r, P2: 1})
	c.emit(Instruction{Op: OpHalt})
	prog := &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub}
	// The same fail-closed check compileTriggerBodyStmt makes, for the same
	// reason and now for the same shapes: a guard is re-evaluated per firing
	// row, so anything in it bound to the firing statement's compile-time
	// image would answer from a snapshot taken before the statement stored a
	// row. Nothing this compile can emit is frozen today -- every subquery
	// went through liveSubSelect and a WHEN has no FROM of its own -- so this
	// declines nothing that reaches it; it is here so that an expression form
	// that later learns to open a cursor cannot silently start reading stale.
	if programReadsFrozenSnapshot(prog) {
		return nil, fmt.Errorf("%w: trigger WHEN reading a frozen snapshot", errVDBEUnsupported)
	}
	return prog, nil
}

// compileTriggerBodyStmt compiles one trigger body statement into a sub-program
// whose NEW/OLD references resolve via OpParam (trig). All four of
// codeTriggerProgram's step kinds reach a lowering: INSERT/UPDATE/DELETE become
// write sub-programs, and a bare SELECT becomes a materialize-and-discard --
// see compileTriggerBodyDiscardSelect for the TK_SELECT arm and what it drops.
func (db *DB) compileTriggerBodyStmt(bs triggerBodyStmt, trig *trigCompileCtx) (*Program, error) {
	prog, err := db.compileTriggerBodyStmtInner(bs, trig)
	if err != nil {
		return nil, err
	}
	if prog != nil {
		prog.NeedsStmtImage = programNeedsStmtImage(prog)
	}
	if prog != nil && programReadsFrozenSnapshot(prog) {
		// The sub-program would read a snapshot taken at compile time, but a
		// body sees the store as of the firing:
		//
		//   CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN
		//     INSERT INTO t2 SELECT count(*) FROM t1; END;
		//   INSERT INTO t1 VALUES(1); INSERT INTO t1 VALUES(2);
		//   -- t2 holds 1 then 2, NOT 0 then 1
		//
		// Live bodies are re-lowered per firing (vdbe_live_read.go) and never
		// reach here. This is a guard for a body still holding a compile-time
		// image the live seam cannot reproduce (an OpOpenRead cursor, a CTE or
		// read-side vtab source, or a payload programReadsFrozenSnapshot does
		// not know).
		return nil, fmt.Errorf("%w: trigger body statement reading tables", errVDBEUnsupported)
	}
	return prog, nil
}

// programReadsFrozenSnapshot reports whether prog reads through a pager fixed at
// compile time: an OpOpenRead cursor, an OpOpenDerived source or subquery bound
// to such an image, or a WritePager for subqueries. A live read
// (Program.LiveSource, vdbe_live_read.go) is not frozen.
//
// A sub-program neither live nor frozen ("(SELECT 7 LIMIT new.a)" in an upsert
// SET opens nothing and reads only the trigger row) is checked by recursing
// with the same test, so an image at any depth is still refused. The subquery
// opcodes are listed explicitly because the live route sets no WritePager; a P4
// that is not a live *Program counts as frozen, so a new payload fails closed.
func programReadsFrozenSnapshot(prog *Program) bool {
	return programReadsFrozenSnapshotSeen(prog, map[*Program]bool{})
}

func programReadsFrozenSnapshotSeen(prog *Program, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return false
	}
	seen[prog] = true
	if prog.WritePager != nil {
		return true
	}
	// A COMPOUND Program carries NO Insns -- its arms are the frames -- so the
	// loop below answers "reads nothing frozen" for every UNION, which is this
	// predicate failing OPEN in the one place its whole doc comment says it
	// must fail closed. The arms hold the OpOpenRead / WritePager the check is
	// looking for, so they are asked the same question.
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			if arm == nil || programReadsFrozenSnapshotSeen(arm, seen) {
				return true
			}
		}
	}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		switch in.Op {
		case OpOpenRead:
			return true
		case OpOpenDerived:
			ds, ok := in.P4.(*derivedSource)
			if !ok {
				return true
			}
			if ds.vtabWrite != nil {
				// The WRITE-side scan of a virtual table is LIVE by
				// construction and needs no LiveSource to make it so: its rows
				// come from the module's own write-side source, read inside the
				// opcode at RUN time (vtabWriteRows, vdbe_vtab_write.go), never
				// from a pager fixed at this compile. It is the one derived
				// source that holds no image at all -- which is also why the
				// program carrying it stays cacheable.
				continue
			}
			if ds.prog == nil || ds.prog.LiveSource == nil {
				return true
			}
		case OpSubquery, OpExists:
			p, ok := in.P4.(*Program)
			if !ok {
				return true
			}
			if p.LiveSource == nil && programReadsFrozenSnapshotSeen(p, seen) {
				return true
			}
		case OpInSub:
			p, ok := in.P4.(*inSubPlan)
			if !ok || p.prog == nil {
				return true
			}
			if p.prog.LiveSource == nil && programReadsFrozenSnapshotSeen(p.prog, seen) {
				return true
			}
		case OpRowSub:
			p, ok := in.P4.(*rowSubPlan)
			if !ok || p.prog == nil {
				return true
			}
			if p.prog.LiveSource == nil && programReadsFrozenSnapshotSeen(p.prog, seen) {
				return true
			}
		}
	}
	return false
}

func (db *DB) compileTriggerBodyStmtInner(bs triggerBodyStmt, trig *trigCompileCtx) (*Program, error) {
	// See triggerBodyStmtDeferredLiteralErr's doc comment (trigger.go):
	// decline rather than let a poisoned literal in an INSERT/UPDATE/DELETE
	// body statement reach the full write compiler unaudited. Wrapped in
	// errVDBEUnsupported so it stays in the channel the rest of the write
	// compiler reports declines through; with no fallback left, that decline is
	// the firing statement's own error. CREATE TRIGGER still matches the oracle
	// either way.
	if (bs.insert != nil || bs.update != nil || bs.delete != nil) && triggerBodyStmtDeferredLiteralErr(bs) != "" {
		return nil, fmt.Errorf("%w: trigger body DML statement with a magnitude-deferred literal", errVDBEUnsupported)
	}
	// A body statement other than a bare SELECT publishes changes() when it
	// finishes (C's OP_ResetCount after each non-SELECT step; runOneTrigger
	// reads the flag; conn_state.go).
	//
	// An INSERT/UPDATE step carries the program's imposed ON CONFLICT
	// policy, if any: pParse->eOrconf (trigger.c:1137), passed as
	// sqlite3Insert's/sqlite3Update's onError (trigger.c:1149-1165). A
	// DELETE takes none: sqlite3DeleteFrom has no onconf
	// (trigger.c:1170-1174).
	//
	// A TEMP trigger's unqualified target resolving only in an attached
	// database is written by that database's session (insert.c:966 takes the
	// database from the resolved table; vdbe_trigger_routed.go). Checked
	// first, because the compilers below resolve only this session's
	// catalogs.
	if name, attached := db.routedTriggerBodyTarget(bs, trig); name != "" {
		return db.compileRoutedTriggerBody(bs, trig, name, attached)
	}
	switch {
	case bs.insert != nil:
		return withCountsChanges(db.compileInsertStmt(applyOrconfInsert(trig.orconf, trig.orconfSet, bs.insert), trig))
	case bs.update != nil:
		return withCountsChanges(db.compileUpdateStmt(applyOrconfUpdate(trig.orconf, trig.orconfSet, bs.update), trig))
	case bs.delete != nil:
		return withCountsChanges(db.compileDeleteStmt(bs.delete, trig))
	case bs.sel != nil:
		// A trigger body SELECT (commonly "SELECT RAISE(ABORT,'msg')"). Its
		// result is discarded; a RAISE inside it aborts the statement, and any
		// per-row runtime error surfaces.
		if len(bs.sel.From) != 0 || len(bs.sel.Compound) != 0 {
			return db.compileTriggerBodyDiscardSelect(bs.sel, trig)
		}
		// A FROM-less body step whose expressions carry a SUBQUERY still has
		// to READ, and it must read the firing instant like every other body
		// statement -- "SELECT RAISE(IGNORE) WHERE EXISTS (SELECT ... WHERE a
		// = new.a)" is SQLite's own tkt3554.test idiom. Without a pager the
		// subquery declined "outside a cursor-driven scope"; with the stored
		// body pager AND the live seam it compiles and re-lowers per firing,
		// which is the pairing compileTriggerGuard already makes for a WHEN
		// clause carrying a subquery, for the same reason.
		if selectNoFromHasSubquery(bs.sel) {
			sp, perr := db.storedBodyPager()
			if perr != nil {
				return nil, declineOrSemantic(perr)
			}
			return compileSelectNoFromTrig(sp, bs.sel, nil, trig, nil, db)
		}
		return compileSelectNoFromTrig(nil, bs.sel, nil, trig, nil, nil)
	default:
		return nil, fmt.Errorf("%w: empty trigger body statement", errVDBEUnsupported)
	}
}

// compileTriggerBodyDiscardSelect lowers a FROM-bearing bare SELECT body step,
// codeTriggerProgram's TK_SELECT arm:
//
//	default: assert( pStep->op==TK_SELECT ); {
//	  SelectDest sDest;
//	  Select *pSelect = sqlite3SelectDup(db, pStep->pSelect, 0);
//	  sqlite3SelectDestInit(&sDest, SRT_Discard, 0);
//	  sqlite3Select(pParse, pSelect, &sDest);
//	  sqlite3SelectDelete(db, pSelect);
//	  break;
//	}
//	                                              -- trigger.c:1179-1186
//
// SRT_Discard stores nothing (select.c:1506-1513) but still evaluates the
// result expressions, so errors and RAISEs reach the firing statement. It emits
// no OP_ResetCount (unlike trigger.c:1157/1166/1174), so this does not
// republish changes(). The port materializes the SELECT through OpOpenDerived
// (runSubOnce runs it to completion) and opens nothing else.
//
// The SELECT is lowered live (compileLiveSubProgram), so it reads the database
// as of the firing. ORDER BY and DISTINCT are dropped, as sqlite3Select drops
// them for SRT_Discard before resolving names (select.c:7637-7653), so "SELECT x
// FROM t1 ORDER BY nosuchcol" in a body raises nothing in C
// (TestTriggerBodySelectOrderByBadColumnIsIgnored). bs.sel is the trigger's
// persistent body, so a copy is modified.
//
// Anything the read compiler cannot lower (a RAISE in the select list, a
// NEW./OLD. reference, a vtab source) returns an error and declines.
func (db *DB) compileTriggerBodyDiscardSelect(sel *SelectStmt, trig *trigCompileCtx) (*Program, error) {
	run := sel
	if len(sel.OrderBy) != 0 || sel.Distinct {
		cp := *sel
		cp.OrderBy = nil
		cp.Distinct = false
		run = &cp
	}
	// A body SELECT is a statement of the trigger program, planned at the
	// firing statement's nQueryLoop (trigger.c:1288) -- 0 wherever this engine
	// fires one: see compiler.nQueryLoop's note on trigger bodies.
	prog, err := db.compileLiveSubProgram(run, trig, nil, 0, true)
	if err != nil {
		// A rejection C makes is raised as C raises it: the body is coded
		// while the FIRING statement is prepared (codeRowTrigger), so "no such
		// column" there fails that statement, fired or not. Only a gap in
		// this compiler declines.
		return nil, declineOrSemantic(err)
	}
	// storedBodyPager, not SnapshotPager: this SELECT is the stored body's own,
	// so it must not see the ORIGINATING database of a delegated write.
	pager, perr := db.storedBodyPager()
	if perr != nil {
		return nil, declineOrSemantic(perr)
	}
	c := &compiler{}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// resolvedTable.cols is read only for its LENGTH (openDerivedCursor's
	// null-row width) and no cursor read follows, so the derived column infos
	// are taken from the lowering's own output names -- the same call
	// compileInsertSelectWrite makes for its source.
	src := &derivedSource{prog: prog, tbl: &resolvedTable{cols: pager.derivedColumnInfos(run, prog.ColNames), ipkIndex: -1}, pager: pager}
	c.emit(Instruction{Op: OpOpenDerived, P1: c.allocCursor(), P2: c.allocSub(), P4: src})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NCursors: c.nCursor, NSubCache: c.nSub}, nil
}

// withCountsChanges marks a compiled trigger body sub-program as one that
// publishes changes() on completion; see Program.CountsChanges.
func withCountsChanges(prog *Program, err error) (*Program, error) {
	if prog != nil {
		prog.CountsChanges = true
	}
	return prog, err
}

func equalFoldName(a, b string) bool { return len(a) == len(b) && foldEq(a, b) }

func foldEq(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// opFireTriggers runs every trigger in the plan for one affected row. NEW/OLD
// come from the parent VM's registers (P1/P3 bases, P2 the NEW rowid); each
// body sub-program runs sharing this statement's write context (journal, fire
// state) so a body failure rolls the whole statement back and a cascade shares
// recursion bookkeeping.
func (m *vdbe) opFireTriggers(op *Instruction) (int, error) {
	plan := op.P4.(*triggerFirePlan)
	// Snapshot OLD/NEW out of the parent registers (a body write can move them).
	var newRow, oldRow []Value
	var newRowid, oldRowid int64
	if plan.hasNew && op.P1 >= 0 {
		newRow = append([]Value(nil), m.regs[op.P1:op.P1+plan.nCols]...)
		newRowid = m.regs[op.P2].I
	}
	if plan.hasOld && op.P3 >= 0 {
		oldRow = append([]Value(nil), m.regs[op.P3:op.P3+plan.nCols]...)
		oldRowid = m.regs[op.P2].I // DELETE: the rowid reg holds the deleted rowid
		if op.P5 != 0 {            // UPDATE: OLD rowid is in a separate register
			oldRowid = m.regs[op.P5].I
		}
	}
	if err := m.firePlanForRow(plan, newRow, newRowid, oldRow, oldRowid); err != nil {
		if isRaiseIgnore(err) {
			// RAISE(IGNORE): abandon this row's DML (and any later trigger)
			// -- jump to row-end. No changes are rolled back.
			return plan.skipAddr, nil
		}
		return -1, err
	}
	return -1, nil
}

// firePlanForRow runs every trigger in plan for ONE row's OLD/NEW image,
// honoring the recursion guard exactly as SQLite's OP_Program does
// (triggerEnter/leave). A RAISE(IGNORE) is returned as an ERROR rather than
// swallowed, because what it means is the CALLER's: for a statement's own
// BEFORE/AFTER program it abandons the row's DML (opFireTriggers jumps to
// plan.skipAddr), while for a REPLACE conflict's victim delete it means the
// victim SURVIVES and the enclosing statement carries on
// (fireReplaceVictimDeleteRow, vdbe_write.go) -- the same split
// sqlite3GenerateRowDelete's own end label gives the two C call sites.
func (m *vdbe) firePlanForRow(plan *triggerFirePlan, newRow []Value, newRowid int64, oldRow []Value, oldRowid int64) error {
	fs := m.wctx.fireState()
	for _, ct := range plan.triggers {
		skip, err := m.wctx.db.triggerEnter(fs, ct.tr)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		err = m.runOneTrigger(ct, newRow, newRowid, oldRow, oldRowid)
		fs.leave()
		if err != nil {
			return err
		}
	}
	return nil
}

// runOneTrigger evaluates a trigger's WHEN and runs its body sub-programs with
// the given OLD/NEW row visible to OpParam. last_insert_rowid is saved and
// restored around the whole body, matching SQLite.
func (m *vdbe) runOneTrigger(ct *compiledTrigger, newRow []Value, newRowid int64, oldRow []Value, oldRowid int64) error {
	db := m.wctx.db
	if ct.when != nil {
		rows, err := m.runTriggerSub(ct.when, newRow, newRowid, oldRow, oldRowid)
		if err != nil {
			return err
		}
		if len(rows) == 0 || len(rows[0]) == 0 || !isTruthy(rows[0][0]) {
			return nil // WHEN false/NULL: this trigger does not fire for this row
		}
	}
	defer db.enterTriggerFrame()()
	saved := db.lastInsertRowid
	// Its ANSWERABILITY is part of the same saved value: a body that inserts
	// into an fts3 table publishes a docid the outer statement must not see.
	// Verified -- an AFTER INSERT trigger storing docid 500 in an fts4 table
	// leaves the firing "INSERT INTO g VALUES(1)" reporting 1, not 500.
	savedOpaque := db.lastRowidOpaque
	// A trigger's cascaded writes are NOT counted in the top-level statement's
	// affected-row total (SQLite's sqlite3_changes() excludes trigger changes),
	// so restore the count around the body -- while keeping every other effect
	// (the shared journal, last_insert_rowid save/restore). The body starts
	// from ZERO, not from the outer count, because each body statement
	// publishes its OWN row count as changes() when it finishes (SQLite's
	// per-frame Vdbe.nChange, reset by OP_Program and republished by the
	// OP_ResetCount after every non-SELECT step -- see conn_state.go).
	savedCount := m.wctx.rowsAffected
	m.wctx.rowsAffected = 0
	// insert.c's regRowCount is restored the same way and for a sharper reason:
	// the C only CODES it for a statement with pParse->pTriggerTab==0, so a
	// trigger body's own INSERT has no such counter at all and cannot contribute
	// to the firing statement's "rows inserted". Zeroed rather than left alone
	// so nothing a body statement does can leak into it either.
	savedInserted := m.wctx.rowsInserted
	m.wctx.rowsInserted = 0
	defer func() {
		db.lastInsertRowid, db.lastRowidOpaque = saved, savedOpaque
		m.wctx.rowsAffected = savedCount
		m.wctx.rowsInserted = savedInserted
	}()
	for _, prog := range ct.body {
		if _, err := m.runTriggerSub(prog, newRow, newRowid, oldRow, oldRowid); err != nil {
			return err
		}
		if prog.CountsChanges {
			db.setChanges(int64(m.wctx.rowsAffected))
			m.wctx.rowsAffected = 0
		}
	}
	return nil
}

// runTriggerSub runs one trigger sub-program (a WHEN guard or a body statement)
// on a fresh VM that SHARES this statement's write context, with OLD/NEW set so
// the sub-program's OpParam reads resolve.
func (m *vdbe) runTriggerSub(prog *Program, newRow []Value, newRowid int64, oldRow []Value, oldRowid int64) ([][]Value, error) {
	sub := &vdbe{
		regs:         make([]Value, prog.NReg),
		cursors:      make([]*vdbeCursor, prog.NCursors),
		recRegs:      make([][]Value, prog.NRecRegs),
		sorters:      make([]*vdbeSorter, prog.NSorters),
		distinctSets: make([]*vdbeDistinctSet, prog.NDistinct),
		subCache:     make([]subCacheEntry, prog.NSubCache),
		pager:        prog.WritePager,
		wctx:         m.wctx,
		trigNew:      newRow,
		trigNewRowid: newRowid,
		trigOld:      oldRow,
		trigOldRowid: oldRowid,
	}
	if prog.NeedsStmtImage {
		img, err := m.wctx.db.SnapshotPager()
		if err != nil {
			return nil, err
		}
		sub.stmtImage = img
	}
	return sub.run(prog.Insns)
}

// selectNoFromHasSubquery reports whether a FROM-less SELECT carries a subquery
// anywhere the compile will evaluate -- its result list or its WHERE. Those are
// the only two a FROM-less body step has (no join, no GROUP BY output, and its
// ORDER BY/DISTINCT are dropped before this point).
func selectNoFromHasSubquery(sel *SelectStmt) bool {
	if sel == nil {
		return false
	}
	if containsSubquery(sel.Where) {
		return true
	}
	for _, c := range sel.Columns {
		if !c.Star && containsSubquery(c.Expr) {
			return true
		}
	}
	return false
}
