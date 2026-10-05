package engine

import "fmt"

// Writes whose target is a writable virtual table (INSERT, UPDATE, DELETE),
// lowered into the write VM.
//
// sqlite3Insert treats a virtual table like any table: it codes each VALUES
// expression into registers with the ordinary loop
//
//	Expr *pX = pList->a[k].pExpr;
//	int y = sqlite3ExprCodeTarget(pParse, pX, iRegStore);
//	                                              -- insert.c:1430-1431
//
// and replaces the constraint checks and store with one opcode:
//
//	if( IsVirtual(pTab) ){
//	  const char *pVTab = (const char *)sqlite3GetVTable(db, pTab);
//	  sqlite3VtabMakeWritable(pParse, pTab);
//	  sqlite3VdbeAddOp4(v, OP_VUpdate, 1, pTab->nCol+2, regIns, pVTab, P4_VTAB);
//	  sqlite3VdbeChangeP5(v, onError==OE_Default ? OE_Abort : onError);
//	  sqlite3MayAbort(pParse);
//	}else
//	                                              -- insert.c:1558-1564
//
// whose body gathers the registers and calls xUpdate (vdbe.c:8733-8743). So
// every expression is coded, and everything downstream (slot mapping, rowid,
// conflict action, index maintenance) is the module's. The Go xUpdate is
// insertIntoVtab / insertIntoFts3 (vtab_write.go, vtab_fts3.go), fed from a
// register block. sqlite3VtabMakeWritable (vtab.c:1225) only arranges an
// OP_VBegin (vtab.c:1220-1221), which this engine has no use for: a vtab's
// store is in-session and written at Close.
//
// UPDATE and DELETE scan the virtual table (update.c:1273 in
// updateVirtualTable, reached via update.c:647-652; delete.c:526 and its vtab
// arm delete.c:633-645; TRUNCATE is withheld from a vtab, delete.c:474),
// coding the WHERE and each SET (update.c:1282) in the scan, collecting into
// an ephemeral (update.c:1320-1327) or RowSet (delete.c:582, 626) replayed into
// OP_VUpdate (update.c:1343-1348; delete.c:644). The cursor is OpOpenDerived
// over openMaterializedCursor, with rows from the module's own write-side
// source (vtabWriteRows), so the scanned set and the mutated set are the same
// by construction.
//
// Two deliberate deviations from OP_VUpdate:
//
//  1. INSERT's register block is the VALUES tuple in tuple order, not C's
//     column-order apVal layout (insert.c:1137, 1502-1537, using aTabColMap,
//     insert.c:1083). The module resolves the IDLIST itself
//     (resolveVtabInsertColumns / fts3InsertTargets), including the rowid
//     pseudo-column (insert.c:1097-1098) and the fts command channel, so there
//     is one resolver. SET slot mapping likewise stays with the module.
//
//  2. One instruction covers the whole statement, where C emits one
//     OP_VUpdate per row. C fts3 keeps pending terms on the connection
//     (Fts3Table.aIndex[].hPending, fts3Int.h:312-316) until sync
//     (sqlite3Fts3PendingTermsFlush, fts3_write.c:3346, via fts3SyncMethod,
//     fts3.c:3556); this engine keeps them in a local of the module call, so
//     per-row calls would write one segment per row, a different on-disk
//     image. The per-row xUpdate work happens inside the opcode.

// vtabInsertPlan is OpVInsert's P4: this engine's P4_VTAB (vdbe.c:8723) plus
// what C bakes into other operands: the conflict action (insert.c:1562, P5) and
// the IDLIST (insert.c:1078-1084).
//
// stmt is held whole because insertIntoVtab (xUpdate) reads its OR clause,
// IDLIST, RETURNING list and table name, and splitting them out would mean a
// second copy of a mapping that must agree with fts3's and fts5's. No
// expression in it is walked: the tuples are registers, and each RETURNING
// column is a compiled program (selfRowExpr, buildVtabReturningPlan).
type vtabInsertPlan struct {
	vm   *vtabMeta
	stmt *insertStmt

	// srcWidth is the SOURCE SELECT's column count for the "INSERT INTO <vtab>
	// SELECT ..." spelling, and -1 for every other one. It is insert.c's
	// nColumn ("nColumn = pSelect->pEList->nExpr;", insert.c:1154), which the C
	// checks against the target's arity at PREPARE time (insert.c:1249-1258),
	// and it is carried because the ROW-WIDTH check the module side performs is
	// per row -- so a source that yields NO rows would pass an arity error
	// silently. Verified against the 3.53.3 oracle: "INSERT INTO f SELECT x,x
	// FROM s" is "table f has 1 columns but 2 values were supplied" whether s
	// holds rows or not.
	srcWidth int
}

// compileVtabInsertStmt compiles "INSERT INTO <vtab> [(cols)] VALUES (...)"
// into: every tuple's expressions compiled into one contiguous register block
// per tuple, and one OpVInsert handing those blocks to the module. See this
// file's doc comment for insert.c's own shape and for the two ways this
// deviates from it.
func (db *DB) compileVtabInsertStmt(stmt *insertStmt, vm *vtabMeta, trig *trigCompileCtx) (*Program, error) {
	// Declined (a hard error; there is no other executor):
	//   - a RETURNING list that does not lower against the written row
	//     (declineVtabInsertReturning);
	//   - a trigger body's INSERT ... SELECT: the source must be read as of the
	//     firing (vdbe_live_read.go) and may name NEW./OLD.;
	//   - an UPSERT: C rejects it at prepare time for any virtual table ("UPSERT
	//     not implemented for virtual table", insert.c:1291-1293);
	//   - a leading WITH (its CTE scope would need pushing).
	// TestVtabInsertDeclinedShapesDeclineCleanly pins each.
	//
	// DEFAULT VALUES is served (one empty tuple, insert.c:1214; the module expands
	// it, vtabInsertRowValues), as is INSERT ... SELECT (emitVtabInsertSelect). A
	// subquery in a tuple compiles against the frozen write snapshot
	// (writeSubqueryPager), carried as WritePager ("INSERT INTO ft(docid,x)
	// VALUES((SELECT 40),'d40')", TestFts3StatementSubTransactionFlush).
	if stmt.upsert != nil ||
		(stmt.selectStmt != nil && trig != nil) ||
		len(stmt.ctes) > 0 {
		return nil, fmt.Errorf("%w: INSERT into a virtual table: shape not lowered", errVDBEUnsupported)
	}
	if stmt.returning != nil {
		if rerr := db.declineVtabInsertReturning(stmt, trig, vm); rerr != nil {
			return nil, rerr
		}
	}
	if stmt.selectStmt != nil {
		return db.emitVtabInsertSelect(stmt, vm, trig)
	}
	if len(stmt.rows) == 0 {
		// Defensive: a VALUES-less INSERT that is not defaultValues has no
		// tuple to code. Nothing produces one today.
		return nil, fmt.Errorf("%w: INSERT into a virtual table with no VALUES", errVDBEUnsupported)
	}
	// Every tuple must be the same width for one register stride. Tuples
	// that disagree are an error in both engines, worded by
	// vtabInsertRowValues, so decline to that path. Whether the width fits
	// the target is decided per module at run time; vtabInsertRowValues
	// keeps that arity check (TestVtabInsertArityIsStillChecked).
	width := len(stmt.rows[0])
	for _, rowExprs := range stmt.rows {
		if len(rowExprs) != width {
			return nil, fmt.Errorf("%w: INSERT into a virtual table with ragged VALUES tuples", errVDBEUnsupported)
		}
	}

	c := &compiler{trig: trig}
	var vtabValuesPager *ReadOnlyPager
	if rowExprsContainSubquery(stmt.rows) {
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, declineOrSemantic(perr)
		}
		c.pager, vtabValuesPager = sp, sp
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// One contiguous block per tuple, all reserved up front so the tuples are
	// laid out at a fixed stride -- OpVInsert's P2/P3.
	//
	// ALL tuples are coded BEFORE the single OpVInsert -- insertIntoVtab is
	// handed the whole [][]Value before its row loop starts -- and that
	// ordering is OBSERVABLE:
	// "INSERT INTO t(docid,c) VALUES(last_insert_rowid(),'x'),(...)" -- fts3e's
	// own idiom, cited in vtabInsertRowValues -- reads the PRE-statement rowid
	// in every tuple either way. Coding per tuple and inserting between them
	// would change that answer.
	base := c.allocN(width * len(stmt.rows))
	for r, rowExprs := range stmt.rows {
		for i, e := range rowExprs {
			// EVERY tuple slot is coded, including one that maps to no declared
			// column: the C's own coding loop runs over the target's COLUMNS and
			// so skips such a slot (insert.c:1363-1436), but this engine's vtab
			// row path evaluates the whole tuple (vtabInsertRowValues) and then
			// maps it. Making it column-driven instead would be a separate,
			// oracle-measured change to a behaviour this engine does not have
			// today.
			reg, eerr := c.compileExpr(e)
			if eerr != nil {
				return nil, eerr
			}
			dst := base + r*width + i
			if reg != dst {
				c.emit(Instruction{Op: OpSCopy, P1: reg, P2: dst})
			}
		}
	}
	c.emit(Instruction{Op: OpVInsert, P1: base, P2: width, P3: len(stmt.rows), P4: &vtabInsertPlan{vm: vm, stmt: stmt, srcWidth: -1}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: vtabValuesPager}, nil
}

// declineVtabInsertReturning is compileVtabInsertStmt's whole RETURNING test:
// nil to lower, an errVDBEUnsupported otherwise.
//
// C codes RETURNING as an inline trigger over the write's register block
// (codeReturningTrigger, sqlite3ExprCodeFactorable under NC_UBaseReg,
// trigger.c:1074-1091); here each output column is a compiled program
// captureVtabRow runs (selfRowExpr), built by buildVtabReturningPlan.
//
// For a virtual table that trigger is forced BEFORE (trigger.c:843-853):
//
//	if( IsVirtual(pTab) ){
//	  if( op!=TK_INSERT ){ sqlite3ErrorMsg(... "%s RETURNING is not available
//	                                            on virtual tables" ...); }
//	  p->tr_tm = TRIGGER_BEFORE;
//	}else{
//	  p->tr_tm = TRIGGER_AFTER;
//	}
//
// so it is coded at insert.c:1494 over regCols, whose rowid is the statement's
// or -1 when none was named (insert.c:1448-1465), with affinity applied
// (insert.c:1491), before OP_VUpdate (which writes no rowid back). This engine
// captures the same candidate tuple before the write (vtabCandidateRow,
// insertIntoVtab).
//
// Declined:
//   - a source SELECT: insertIntoVtab refuses it with its own wording;
//   - a plan that does not build (unknown column, unsupported expression):
//     buildVtabReturningPlan's error, C's prepare-time one (resolve.c:1290);
//   - a column that builds but does not lower: a subquery reading a table.
//     It needs a pager and the per-row snapshot insertIntoVtab takes ("...
//     VALUES(3,6,7),(4,8,9) RETURNING (SELECT count(*) FROM t1)" answers 2
//     then 3 over a one-row t1).
func (db *DB) declineVtabInsertReturning(stmt *insertStmt, trig *trigCompileCtx, vm *vtabMeta) error {
	if trig != nil {
		// Redundant, kept as C's own fork: a trigger cannot carry RETURNING
		// ("cannot use RETURNING in a trigger", build.c:1443-1444; this
		// parser rejects it too). If one could, runVInsert would publish its
		// rows onto the write context the body shares with the firing
		// statement, overwriting that statement's result set.
		return fmt.Errorf("%w: trigger body INSERT ... RETURNING into a virtual table", errVDBEUnsupported)
	}
	if stmt.selectStmt != nil {
		return fmt.Errorf("%w: INSERT ... SELECT ... RETURNING into a virtual table", errVDBEUnsupported)
	}
	if vm.store == nil {
		// A store-less target: fts3/fts4 (rows in %_content/%_segdir) and
		// read-only modules. buildVtabReturningPlan needs vm.store's columns,
		// so the module decides: insertIntoFts3 builds the same plan from
		// fts3DeclaredColumns and captures each candidate tuple (the IsVirtual
		// fork only decides BEFORE vs AFTER), erroring for a column that does
		// not lower; a read-only module refuses the write ("may not be
		// modified").
		return nil
	}
	cols := vtabColumnInfos(vm.store.vtabColumns())
	mode := colNameMode{full: db.fullColumnNames, short: !db.shortColumnNamesOff}
	// The frozen write snapshot, so a RETURNING subquery over a real table can
	// resolve and read it -- the same pager compileInsertStmt gives an ordinary
	// table's VALUES subqueries.
	retPager, rperr := db.writeSubqueryPager()
	if rperr != nil {
		return declineOrSemantic(rperr)
	}
	plan, perr := buildVtabReturningPlan(vtabEvalScope(stmt.table, cols), stmt.returning, mode, retPager)
	if plan != nil {
		plan.caseSensitiveLike = db.caseSensitiveLike
	}
	if perr != nil {
		return fmt.Errorf("%w: INSERT into a virtual table with a RETURNING clause this plan builder rejects (%v)", errVDBEUnsupported, perr)
	}
	if !plan.compiled() {
		return fmt.Errorf("%w: INSERT into a virtual table with a RETURNING output column this compiler cannot lower", errVDBEUnsupported)
	}
	return nil
}

// emitVtabInsertSelect compiles "INSERT INTO <vtab> [(cols)] SELECT ...": the
// source is a sub-Program opened with OpOpenDerived and scanned, each row is
// collected by OpVInsertRow, and one OpVInsert hands the collection to the
// module.
//
// That is C's shape: every fts write reads its shadow tables, so readsTable
// selects template 4 (insert.c:1167-1169), which drains the SELECT into an
// ephemeral before touching the target:
//
//	sqlite3VdbeAddOp2(v, OP_OpenEphemeral, srcTab, nColumn);
//	addrL = sqlite3VdbeAddOp1(v, OP_Yield, dest.iSDParm);
//	sqlite3VdbeAddOp3(v, OP_MakeRecord, regFromSelect, nColumn, regRec);
//	sqlite3VdbeAddOp2(v, OP_NewRowid, srcTab, regTempRowid);
//	sqlite3VdbeAddOp3(v, OP_Insert, srcTab, regRec, regTempRowid);
//	                                              -- insert.c:1189-1193
//
// and reads it back (insert.c:1424, 1615). So "INSERT INTO h SELECT w FROM h"
// over an fts3 table holding 'a','b' ends with 4 rows. Every source column is
// passed unmapped, in source order; the module maps them.
//
// A trigger body is declined by the caller, so the snapshot is always a
// top-level statement's; it is declared as WritePager so cachedWriteProgram
// never reuses the program (a second run would re-read the first's image).
func (db *DB) emitVtabInsertSelect(stmt *insertStmt, vm *vtabMeta, trig *trigCompileCtx) (*Program, error) {
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, declineOrSemantic(perr)
	}
	// "FROM <fts5 table>('query')" is fts5's table-valued call form, rewritten
	// into the plain scan + MATCH it means before anything compiles it --
	// execSelect does this at the top of its own body (query.go), and this
	// route reaches the compiler without going through execSelect, so the
	// rewrite has to be applied here. See compileInsertSelectWrite's identical
	// call for the measurement.
	srcSel := pager.fts5RewriteTableFunc(stmt.selectStmt)
	prog, serr := compileSubProgram(pager, srcSel, nil)
	if serr != nil {
		// Wrapped as errVDBEUnsupported rather than re-raised as-is --
		// including an errVDBESemantic. Unlike compileInsertSelectWrite's
		// top-level arm (whose hardening rests on "the read path has nowhere
		// else to go"), the WORDING for a vtab INSERT belongs to the module
		// half: insertIntoVtab / insertIntoFts3 wrap every rejection as
		// "engine: INSERT into <t>: ..." with their own markConnStateOpaque
		// bookkeeping, which this compiler has no opcode for. Same choice
		// compileViewMaterialize makes, for the same reason.
		return nil, declineOrSemantic(serr)
	}
	width := len(prog.ColNames)
	srcCols := pager.derivedColumnInfos(srcSel, prog.ColNames)

	c := &compiler{trig: trig}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	srcCursor := c.allocCursor()
	src := &derivedSource{prog: prog, tbl: &resolvedTable{cols: srcCols, ipkIndex: -1}, pager: pager}
	c.emit(Instruction{Op: OpOpenDerived, P1: srcCursor, P2: c.allocSub(), P4: src})
	valReg := c.allocN(width)
	rewind := c.emit(Instruction{Op: OpRewind, P1: srcCursor})
	loopTop := c.here()
	for i := 0; i < width; i++ {
		c.emit(Instruction{Op: OpColumn, P1: srcCursor, P2: i, P3: valReg + i})
	}
	c.emit(Instruction{Op: OpVInsertRow, P1: valReg, P2: width})
	c.emit(Instruction{Op: OpNext, P1: srcCursor, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpVInsert, P3: -1, P4: &vtabInsertPlan{vm: vm, stmt: stmt, srcWidth: width}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: pager}, nil
}

// opVInsertRow runs OpVInsertRow: append the source row the scan cursor just
// read to this statement's collected row list. It is SQLite's OP_MakeRecord +
// OP_NewRowid + OP_Insert into srcTab (insert.c:1191-1193).
func (m *vdbe) opVInsertRow(op *Instruction) {
	// COPIED out of the registers rather than aliased, for opVWriteRow's
	// reason: these outlive the loop iteration that produced them, because
	// every later row overwrites the same register block. That is not a
	// lifetime a register block can express.
	vals := make([]Value, op.P2)
	copy(vals, m.regs[op.P1:op.P1+op.P2])
	m.vtabInsert = append(m.vtabInsert, vals)
}

// opVInsert runs OpVInsert: gather the per-tuple register blocks into rows and
// hand them to the virtual table's writer. It is SQLite's OP_VUpdate body,
// which does the same two things and nothing else -- copy the registers into
// apArg, call pModule->xUpdate (vdbe.c:8736-8743).
//
// outer is nil on this route BY CONSTRUCTION: it exists only for
// vtabInsertRowValues' own expression evaluation, which the precomputed rows
// bypass entirely (see its pre parameter), and compileVtabInsertStmt declines
// every shape that could reach that evaluation by another door. A RETURNING
// clause is one of the doors that is now OPEN rather than declined, and it is
// open only because its own capture compiles too -- see runVInsert.
func (m *vdbe) opVInsert(op *Instruction) error {
	plan := op.P4.(*vtabInsertPlan)
	if op.P3 < 0 {
		// The SELECT source: OpVInsertRow already collected the rows off the
		// scan cursor. It still has to arrive NON-NIL when the source yielded
		// none, because nil is insertIntoVtab's "no rows were precomputed;
		// evaluate the row source here" signal -- exactly the invariant
		// opVWrite's own empty-set note states, and here it would send a
		// SELECT-sourced statement into that arm with no VALUES tuples to
		// find.
		rows := m.vtabInsert
		if rows == nil {
			rows = [][]Value{}
		}
		return m.runVInsert(plan, rows)
	}
	rows := make([][]Value, op.P3)
	for r := range rows {
		// COPIED out of the registers rather than aliased. Nothing observable
		// depends on that today -- replacing the copy with a sub-slice of
		// m.regs was mutation-tested and every test still passed, because no
		// register is written again before this call returns and the module
		// side copies whatever it keeps (insertIntoVtab's own "full"). It stays
		// because the module side is free to retain a row (insertIntoFts3
		// STAGES every row before committing any of them) and a register block
		// is not a lifetime this file controls. The C is under no such
		// obligation -- OP_VUpdate hands xUpdate the Mem* cells themselves
		// (vdbe.c:8739) -- because there the module owns the copying rule.
		vals := make([]Value, op.P2)
		copy(vals, m.regs[op.P1+r*op.P2:op.P1+(r+1)*op.P2])
		rows[r] = vals
	}
	return m.runVInsert(plan, rows)
}

// runVInsert is opVInsert's tail for both row sources: hand the rows to the
// module and publish the counters and, with RETURNING, the result set.
//
// ret is non-nil exactly for statements whose every RETURNING column lowered,
// so the capture inside insertIntoVtab runs compiled programs
// (captureVtabRow). C lands the rows in a holding table (trigger.c:1096-1098);
// here they land on the write context for execReturningViaVM. For a virtual
// table C's RETURNING is a BEFORE trigger over the candidate registers (see
// declineVtabInsertReturning).
func (m *vdbe) runVInsert(plan *vtabInsertPlan, rows [][]Value) error {
	var ret *returningState
	if plan.stmt.returning != nil {
		ret = &returningState{}
	}
	// m.params, not nil: a RETURNING column may use a bound parameter
	// ("RETURNING id, ?"), which with nil answered NULL silently.
	// outer is nil: a trigger body cannot carry RETURNING
	// (build.c:1443-1444), so there is no enclosing row.
	n, err := m.wctx.db.insertIntoVtab(plan.vm, plan.stmt, rows, plan.srcWidth, m.params, nil, ret)
	if ret != nil {
		// The COLUMNS are published even when no row was captured, and
		// published as a NON-NIL empty slice in that case: execReturningViaVM
		// keys on "wc.returningCols != nil" (vdbe_write.go), so a nil would
		// make it fall through to the program's own ColNames instead of
		// answering from the write context.
		//
		// Reachable, not defensive: "INSERT INTO g(g) VALUES('rebuild')
		// RETURNING x" -- fts5's COMMAND CHANNEL, which is not a row and stores
		// none -- captures nothing and answers with no columns and no rows
		// (measured before this promotion; pinned by
		// TestVtabInsertReturningCompiledAnswers).
		cols := []string{}
		if ret.plan != nil {
			cols = ret.plan.names
		}
		m.wctx.returningCols, m.wctx.returningRows = cols, ret.rows
		// haltCount, which the NON-returning tail below deliberately does not
		// apply, and it is load-bearing here rather than cosmetic. A RETURNING
		// capture that raises on the SECOND row returns a POSITIVE count
		// alongside its error (insertIntoVtab's "return count, cerr"), and
		// sqlite3VdbeHalt's rule is that an ABORT/ROLLBACK halt publishes 0
		// while only a FAIL halt keeps its partial count (vdbeaux.c:3481).
		// Measured before this promotion:
		// "INSERT INTO g VALUES(1,'p'),(1000000000000,'q') RETURNING
		// zeroblob(x)" leaves changes() at 0 and rows-inserted at 2.
		m.wctx.rowsAffected = int(haltCount(n, err))
		if n < 0 {
			n = 0
		}
		m.wctx.rowsInserted = n
		return err
	}
	// Recorded even when the statement ERRORED, and recorded by ASSIGNMENT:
	// one OpVInsert is one whole statement, however many rows it turns out to
	// have stored, and an OR IGNORE run that stops partway still owes its
	// predecessors' count to runWrite's deferred setChanges.
	m.wctx.rowsAffected = n
	// insert.c's regRowCount, which "PRAGMA count_changes" answers and which
	// changes() is not, with insert.c's own clamp ("regRowCount never goes
	// below zero"). Assigning it inside a TRIGGER BODY sub-program is safe for
	// the same reason the rowsAffected assignment above is: runTriggerSub saves
	// and restores both around every body (vdbe_trigger.go), so a
	// body's count cannot leak into the firing statement's.
	if n < 0 {
		n = 0
	}
	m.wctx.rowsInserted = n
	return err
}

// ---- UPDATE / DELETE ----

// vtabWriteRowSet is what a compiled vtab UPDATE/DELETE scan hands its module
// half: rowid -> that row's evaluated SET values in statement order. It is
// C's ephemeral (update.c:1320-1327) and RowSet (delete.c:582) in one; a
// DELETE's entries have nil values.
//
// Non-nil is the signal, as for insertIntoVtab's pre: the compiled route has
// already selected the rows, and the module must not select again. An empty
// set must survive (opVWrite makes one when nothing matched), or "DELETE FROM
// f WHERE ..." matching nothing would become "every row".
type vtabWriteRowSet map[int64][]Value

// vtabWritePlan is OpVWrite's P4: the table and statement; exactly one of
// del/upd is set. The statement is held whole because the module side reads
// its OR clause, SET column names (it owns slot mapping) and table name. No
// expression in it is evaluated on this route: the WHERE and SET values are
// the register block the scan filled.
type vtabWritePlan struct {
	vm  *vtabMeta
	del *deleteStmt
	upd *updateStmt
}

// vtabWriteScope is the ONE scope a virtual table's DELETE/UPDATE resolves its
// WHERE and SET names in. It is the MODULE half's own scope, returned by the
// module half's own two builders (fts3WriteScope, fts3_write.go, and
// vtabEvalScope, vtab_write.go) rather than rebuilt here, so a name cannot
// resolve one way for the compiled WHERE and another for the module's slot
// mapping -- which matters most for the names that are NOT ordinary columns:
// fts3's hidden "docid" and its "languageid=" column, and fts5's own hidden
// leading slot.
//
// name is the statement's spelling of the target, never an alias -- both
// callers decline an aliased target.
func (db *DB) vtabWriteScope(vm *vtabMeta, name string) (tableScope, error) {
	if fm, isFts3 := fts3ModuleOf(vm); isFts3 {
		sch, err := fm.schemaOf(db, vm)
		if err != nil {
			return tableScope{}, err
		}
		return fts3WriteScope(name, sch), nil
	}
	if vm.loadErr != nil {
		return tableScope{}, vm.loadErr
	}
	if vm.store == nil {
		return tableScope{}, fmt.Errorf("virtual table %s is read-only", vm.name)
	}
	scope := vtabEvalScope(name, vtabColumnInfos(vm.store.vtabColumns()))
	// deleteVtab/updateVtab set these three on their own scope for the same
	// reason: an fts5 MATCH in a WHERE resolves through them. Both compilers
	// below decline a MATCH outright, so today they only ever make an fts5
	// column reference resolve the way the module half's own scope resolves it;
	// they are carried anyway so the two scopes stay the SAME object, field for
	// field.
	if fst, isFts5 := vm.store.(*fts5Store); isFts5 {
		scope.isFts5 = true
		scope.fts5Tok = fst.tok
		scope.fts5Detail = fst.detail
	}
	return scope, nil
}

// vtabWriteRows is the run-time row source OpOpenDerived's vtabWrite branch
// scans: the module's own, the same rows deleteVtab/updateVtab (and the fts3
// variants) iterate when OpVWrite hands them the selection, so the WHERE and
// the module see one set. It is not materializeVtab (the read path's
// pager-based source): a write program has no pager, and a compile-time
// snapshot would be a frozen copy of the live store.
//
// The layout matches the scan scope: fts3MutationRows gives [docid, col0..,
// langid?] as fts3WriteScope names it; vtabStore.vtabRows gives full declared
// rows in rowid order against the same vtabColumns vtabWriteScope reads.
func (db *DB) vtabWriteRows(vm *vtabMeta) ([]int64, [][]Value, error) {
	// deleteVtab/updateVtab's own two prologue lines, run HERE as well because
	// this is the first thing the compiled statement does and they are the
	// first thing the module half does -- before its row loop touches a row.
	// Both are idempotent, and both matter for ORDERING
	// rather than for eventual state: markConnStateOpaque makes
	// total_changes()/last_insert_rowid() DECLINE for the rest of the session
	// (conn_state.go), so a WHERE that calls one of them must see the same
	// answer on both routes, and a statement whose WHERE then ERRORS must leave
	// the same session state behind on both.
	db.markConnStateOpaque()
	if fm, isFts3 := fts3ModuleOf(vm); isFts3 {
		sch, err := fm.schemaOf(db, vm)
		if err != nil {
			return nil, nil, err
		}
		// The same prologue deleteFromFts3/updateFts3 run, and running it twice
		// is safe by inspection: it resolves shadow tables and asks
		// fts3DeclineAutomergeInTxn, which is a pure check. Running it HERE additionally makes its errors -- a missing
		// shadow table, an automerge-enabled table inside a transaction --
		// surface before any expression is evaluated, which is where
		// deleteFromFts3/updateFts3 raise them too.
		s, err := db.fts3MutationCommon(vm, fm, sch)
		if err != nil {
			return nil, nil, err
		}
		return db.fts3MutationRows(sch, s)
	}
	if vm.loadErr != nil {
		return nil, nil, vm.loadErr
	}
	if vm.store == nil {
		// Unreachable: vtabWriteScope declined a read-only module at compile
		// time. Reported rather than dereferenced (AGENTS.md invariant 2).
		return nil, nil, fmt.Errorf("engine: table %s may not be modified (virtual table is read-only)", vm.name)
	}
	rowids, rows := vm.store.vtabRows()
	return rowids, rows, nil
}

// vtabWriteScan is what beginVtabWriteScan hands its two callers: the compiler
// with the loop head already emitted, the register holding the current row's
// rowid, the loop's top address and the address of the OpRewind to patch to the
// loop's end.
type vtabWriteScan struct {
	c        *compiler
	loopTop  int
	rewind   int
	rowidReg int
}

// beginVtabWriteScan is the shared prologue of compileVtabDeleteStmt and
// compileVtabUpdateStmt: the scan scope, the OpInit/OpOpenDerived/OpRewind
// loop head, and the per-row rowid read.
//
// No OLD row is loaded into registers: the WHERE and SET read their columns
// off the cursor, and the module re-reads the OLD row from the same source
// (fts3 writes delete markers from the stored text; updateVtab seeds newVals
// from it). The rowid is read, as C carries it into OP_VUpdate
// (update.c:1289) or the RowSet (delete.c:552, replayed at delete.c:644).
func (db *DB) beginVtabWriteScan(vm *vtabMeta, name string, trig *trigCompileCtx) (*vtabWriteScan, error) {
	scope, serr := db.vtabWriteScope(vm, name)
	if serr != nil {
		// Wrapped as errVDBEUnsupported rather than raised as-is. Every error
		// this can produce -- a read-only module, a compress= table, an
		// unparseable fts3 schema -- is one deleteVtab / deleteFromFts3 also
		// word, with their own connection-state bookkeeping, so this compiler
		// does not restate it as its own. Identical to compileViewMaterialize's
		// choice for its viewColumnInfos error.
		return nil, declineOrSemantic(serr)
	}
	// ownSchema lets compileColumn resolve a three-part "schema.table.column"
	// with no pager, as the ordinary write path does ("DELETE FROM ft WHERE
	// main.ft.docid=20", TestFts3StatementSubTransactionMutationPlan). It is
	// the target's schema only: lookupName skips a table of another schema
	// (resolve.c:421), so another attached database's qualifier must still
	// fail. A vtab's catalog is its own isTemp.
	own := localSchemaOr(db.localSchema)
	if vm != nil && vm.isTemp {
		own = "temp"
	}
	c := &compiler{scopes: []compileScope{{tableScope: scope, cursor: 0}}, nCursor: 1, ownSchema: own}
	c.trig = trig
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	// resolvedTable.cols is read only for its LENGTH (openMaterializedCursor's
	// column count); the rows are indexed positionally. Using the scan scope's
	// own column list means a cursor read and a name resolution cannot drift.
	src := &derivedSource{vtabWrite: vm, tbl: &resolvedTable{cols: scope.cols, ipkIndex: -1}}
	c.emit(Instruction{Op: OpOpenDerived, P1: 0, P4: src})
	rewind := c.emit(Instruction{Op: OpRewind, P1: 0})
	loopTop := c.here()
	rowidReg := c.allocN(1)
	c.emit(Instruction{Op: OpRowid, P1: 0, P2: rowidReg})
	return &vtabWriteScan{c: c, loopTop: loopTop, rewind: rewind, rowidReg: rowidReg}, nil
}

// vtabWriteDeclined is the shape gate both compilers share:
//
//   - a subquery: the module halves refuse one with their own wording, and
//     declining keeps this route pager-free, so the program stays cacheable
//     (cachedWriteProgram) while reading live rows;
//   - a MATCH in a SET right-hand side: a per-row full-text probe neither
//     module evaluates. A MATCH in WHERE compiles (vtabWriteWhereDeclined).
func vtabWriteDeclined(e Expr) bool {
	return containsSubquery(e) || fts3WhereHasMatch(e)
}

// vtabWriteWhereDeclined is vtabWriteDeclined for a WHERE clause, which admits
// the one shape a SET right-hand side does not: a MATCH. Both compilers bind it
// with bindVtabWhereMatch and emit it, so declining it here would refuse a
// statement the emitter can serve.
func vtabWriteWhereDeclined(e Expr) bool {
	return containsSubquery(e)
}

// vtabReturningRejected raises C's prepare-time refusal of "UPDATE/DELETE ...
// RETURNING" on any virtual table. RETURNING is a synthetic TEMP trigger
// (sqlite3AddReturning, build.c:1448-1477) spliced into the trigger list
// (trigger.c:68-78), and the first walk over it rejects it for a virtual table
// (triggersReallyExist, trigger.c:838-852):
//
//	}else if( p->op==TK_RETURNING ){
//	  p->op = op;
//	  if( IsVirtual(pTab) ){
//	    if( op!=TK_INSERT ){
//	      sqlite3ErrorMsg(pParse,
//	        "%s RETURNING is not available on virtual tables",
//	        op==TK_DELETE ? "DELETE" : "UPDATE");
//	    }
//	    p->tr_tm = TRIGGER_BEFORE;
//	  }else{
//	    p->tr_tm = TRIGGER_AFTER;
//	  }
//
// INSERT survives as a BEFORE trigger, so "INSERT INTO f VALUES('b') RETURNING
// rowid" answers -1 (insert.c:1452-1453).
//
// errVDBESemantic, not errVDBEUnsupported: it is a real C error. C raises it at
// update.c:370 / delete.c:352, after the table lookup and expression-level "no
// such function", before SET/WHERE name resolution:
//
//	UPDATE f SET nosuchcol='b' RETURNING x     oracle: the RETURNING refusal
//	UPDATE f SET x='b' WHERE nosuchcol=1 RETURNING x
//	                                           oracle: the RETURNING refusal
//	UPDATE f SET x=nosuchfn(1) RETURNING x     oracle: "no such function: nosuchfn"
//	UPDATE f INDEXED BY nosuchidx SET x='b' RETURNING x
//	                                           oracle: "no such index: nosuchidx"
//
// This engine reports the RETURNING refusal for the last two, still an error.
func vtabReturningRejected(returning []SelectColumn, verb string) error {
	if returning == nil {
		return nil
	}
	return semanticf("engine: %s RETURNING is not available on virtual tables", verb)
}

// compileVtabDeleteStmt compiles "DELETE FROM <vtab> [WHERE ...]" into: scan the
// virtual table, and for each row whose OLD image satisfies the WHERE, record
// its rowid; one OpVWrite then hands the whole set to the module. That is
// delete.c's own shape for a virtual table -- the WHERE coded into
// sqlite3WhereBegin (delete.c:526), the rowid alone collected
// ("sqlite3ExprCodeGetColumnOfTable(v, pTab, iTabCur, -1, iKey);" at :552, then
// OP_RowSetAdd at :582), and the collection replayed into OP_VUpdate with
// nArg==1 (OP_RowSetRead at :626, OP_VUpdate at :644).
//
// P5 on that OP_VUpdate is the LITERAL OE_Abort (delete.c:645), not the
// statement's clause -- a DELETE has no OR clause to give it one.
func (db *DB) compileVtabDeleteStmt(stmt *deleteStmt, vm *vtabMeta, trig *trigCompileCtx) (*Program, error) {
	// Declined:
	//   - a leading WITH (its CTEs would serve only WHERE subqueries, which
	//     are declined anyway);
	//   - INDEXED BY: a vtab has no index to name;
	//   - an "AS <alias>" on the target: the scan scope is named for the
	//     table. C accepts it (delete.c:526's scan sees the alias); serving it
	//     means giving the scope the alias.
	if err := vtabReturningRejected(stmt.returning, "DELETE"); err != nil {
		return nil, err
	}
	if len(stmt.ctes) > 0 || stmt.indexedBy != "" || stmt.alias != "" ||
		vtabWriteWhereDeclined(stmt.where) {
		return nil, fmt.Errorf("%w: DELETE from a virtual table: shape not lowered", errVDBEUnsupported)
	}
	sc, serr := db.beginVtabWriteScan(vm, stmt.table, trig)
	if serr != nil {
		return nil, serr
	}
	c := sc.c
	matchPager, merr := db.bindVtabWhereMatch(c, stmt.table, stmt.where)
	if merr != nil {
		return nil, merr
	}
	whereJump, werr := emitVtabWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	c.emit(Instruction{Op: OpVWriteRow, P2: sc.rowidReg})
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: sc.loopTop})
	c.patch(sc.rewind, c.here())
	c.emit(Instruction{Op: OpVWrite, P4: &vtabWritePlan{vm: vm, del: stmt}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: matchPager}, nil
}

// compileVtabUpdateStmt compiles "UPDATE <vtab> SET c=expr[,...] [WHERE ...]":
// scan the vtab, and for each row satisfying the WHERE evaluate every SET
// right-hand side against it and record them with the rowid; one OpVWrite
// then hands the set to the module. That is updateVirtualTable:
//
//	pWInfo = sqlite3WhereBegin(...WHERE_ONEPASS_DESIRED...);           -- :1273
//	sqlite3ExprCode(pParse, pChanges->a[aXRef[i]].pExpr, regArg+2+i);  -- :1282
//	sqlite3VdbeAddOp2(v, OP_Rowid, iCsr, regArg);                      -- :1289
//	OP_MakeRecord / OP_NewRowid / OP_Insert into ephemTab;        -- :1320-1327
//	OP_Rewind ephemTab; OP_Column ephemTab, i, regArg+i;          -- :1339-1345
//	sqlite3VdbeAddOp4(v, OP_VUpdate, 0, nArg, regArg, pVTab, P4_VTAB); -- :1348
//
// C also copies unchanged columns (OP_VColumn+OPFLAG_NOCHNG, :1284-1285) and the
// new rowid (:1293) into the argument vector, which is the new row; here the
// module seeds the new row from the OLD one itself (updateVtab's newVals), so
// only the rowid and SET values need carrying.
//
// No affinity pass: updateVirtualTable never calls sqlite3TableAffinity
// (writeApplySetList).
func (db *DB) compileVtabUpdateStmt(stmt *updateStmt, vm *vtabMeta, trig *trigCompileCtx) (*Program, error) {
	// Declined as in compileVtabDeleteStmt (WITH, INDEXED BY, alias), plus:
	//   - UPDATE ... FROM: a different algorithm (update.c:1230, 1265) that
	//     the module halves also refuse;
	//   - a SET target the scan scope does not name: "no such column: <c>",
	//     C's prepare-time error (update.c:500). Otherwise "SET nosuchcol=<an
	//     erroring expression>" would raise the expression's error instead.
	//
	// That second check also makes evaluation order exact: writeApplySetList
	// evaluates slotted targets before slotless ones, while this emitter
	// codes all in statement order. The only slotless target is an fts3 SET
	// of rowid/oid/_rowid_ (fts3SetRowidSynonymNoOp), which fts3WriteScope
	// does not name, so it is declined here (see below for how it is
	// served); every accepted statement has statement order == pass order.
	//
	// An explicit OR clause is not declined: the module gate decides it
	// (updateVtab admits OR IGNORE for any store and OR REPLACE for fts5,
	// vdbe.c:8750 and update.c:1349; updateFts3 admits OR REPLACE).
	if err := vtabReturningRejected(stmt.returning, "UPDATE"); err != nil {
		return nil, err
	}
	if stmt.from != nil || len(stmt.ctes) > 0 || stmt.indexedBy != "" ||
		stmt.alias != "" || vtabWriteWhereDeclined(stmt.where) {
		return nil, fmt.Errorf("%w: UPDATE of a virtual table: shape not lowered", errVDBEUnsupported)
	}
	for _, a := range stmt.sets {
		if vtabWriteDeclined(a.expr) {
			return nil, fmt.Errorf("%w: UPDATE of a virtual table: subquery or MATCH in SET", errVDBEUnsupported)
		}
	}
	sc, serr := db.beginVtabWriteScan(vm, stmt.table, trig)
	if serr != nil {
		return nil, serr
	}
	c := sc.c
	// Every SET target must be named by the scan scope (the module's write
	// scope), so the module's setIdx loop maps it to a real slot; the
	// mapping itself stays with the module. r33sFoldIdent folds ASCII only,
	// stricter than the modules' Unicode folding, so this can only decline
	// something the module would accept, never the reverse.
	ci := c.scopes[0].colIndex
	for _, a := range stmt.sets {
		if _, ok := ci[r33sFoldIdent(a.col)]; ok {
			continue
		}
		// "SET rowid/oid/_rowid_=..." on an fts3/fts4 table is a silent no-op:
		// only "docid" moves the row. The right-hand side is still evaluated
		// (an erroring one aborts the statement). C resolves the synonym to
		// chngRowid (update.c:494-498) into apVal[1] (update.c:1291), but fts3
		// reads the new rowid from the docid cell and consults apVal[1] only
		// when that is NULL (fts3_write.c:5762-5765), never on UPDATE.
		// updateFts3 implements it (fts3SetRowidSynonymNoOp); this only stops
		// declining the shape.
		if isRowidAliasName(r33sFoldIdent(a.col)) {
			continue
		}
		return nil, fmt.Errorf("%w: no such column: %s", errVDBEUnsupported, a.col)
	}
	matchPager, merr := db.bindVtabWhereMatch(c, stmt.table, stmt.where)
	if merr != nil {
		return nil, merr
	}
	whereJump, werr := emitVtabWhere(c, stmt.where)
	if werr != nil {
		return nil, werr
	}
	// Every right-hand side is evaluated BEFORE any of them is applied, so the
	// assignments are simultaneous ("SET x=y, y=x" swaps rather than chains),
	// every one of them read off the OLD row. The C says the same thing
	// structurally: :1282 codes
	// each changed column into its own regArg slot while the cursor still holds
	// the old row.
	setBase := c.allocN(len(stmt.sets))
	for i, a := range stmt.sets {
		r, eerr := c.compileExpr(a.expr)
		if eerr != nil {
			return nil, eerr
		}
		if r != setBase+i {
			c.emit(Instruction{Op: OpSCopy, P1: r, P2: setBase + i})
		}
	}
	c.emit(Instruction{Op: OpVWriteRow, P1: setBase, P2: sc.rowidReg, P3: len(stmt.sets)})
	if whereJump >= 0 {
		c.patch(whereJump, c.here())
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: sc.loopTop})
	c.patch(sc.rewind, c.here())
	c.emit(Instruction{Op: OpVWrite, P4: &vtabWritePlan{vm: vm, upd: stmt}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: matchPager}, nil
}

// bindVtabWhereMatch runs the MATCH placement passes over a vtab write's
// WHERE, so "DELETE FROM t WHERE t MATCH ..." (and the UPDATE twin) compile
// the MATCH. Their input is a synthesized one-item SelectStmt over the
// target.
//
// The pager is the write path's subquery pager, which can resolve the fts
// shadow tables at compile time (OpMatch takes its runtime pager from the
// row's evalCtx). It is returned and every caller declares it as
// Program.WritePager: the bindings hold pager-derived state, and a cached
// program re-run under another pager would read the wrong shadow tables, a
// wrong answer the harness cannot see (it re-prepares every statement). A
// WHERE with no MATCH returns nil and stays cacheable.
func (db *DB) bindVtabWhereMatch(c *compiler, table string, where Expr) (*ReadOnlyPager, error) {
	if !fts3WhereHasMatch(where) {
		return nil, nil
	}
	sp, perr := db.writeSubqueryPager()
	if perr != nil {
		return nil, declineOrSemantic(perr)
	}
	// fts5 only. fts3/fts4 resolve the MATCH target through
	// fts3ResolveMatchTarget, which needs a pager at run time that the
	// write path's evalCtx lacks; fts5MatchTarget uses the scope's isFts5
	// flag. fts3 is left as the module refuses it ("DELETE from an %s
	// table with a MATCH in WHERE is not supported by this write path").
	//
	// To serve fts3 later: fts3MatchBindings needs a []joinedTable matching
	// the one-item FROM; a nil list fails its length check and poisons
	// every entry to -1, so the MATCH silently reads as unmappable.
	if !sp.isFts5Table(table) {
		return nil, fmt.Errorf("%w: MATCH in WHERE against a non-fts5 virtual table", errVDBEUnsupported)
	}
	c.pager = sp
	syn := &SelectStmt{From: []FromItem{{Table: table}}, Where: where}
	scopes := []tableScope{c.scopes[0].tableScope}
	c.fts5MatchGood = fts5MatchBindings(sp, syn, scopes)
	return sp, nil
}

// emitVtabWhere emits the statement's WHERE over the row the scan cursor is on,
// returning the jump to patch to the row's end (or -1 when there is no WHERE).
func emitVtabWhere(c *compiler, where Expr) (int, error) {
	if where == nil {
		return -1, nil
	}
	// See compileDeleteStmt's identical assignment: this is the statement's own
	// top-level WHERE, the same WhereClause tag-20220128a's "pWC->op==TK_AND"
	// guard sees -- delete.c:526 and update.c:1273 both hand it to
	// sqlite3WhereBegin exactly as a SELECT's own WHERE is handed over.
	c.inWhereConjunct = true
	wReg, werr := c.compileExpr(where)
	if werr != nil {
		return -1, werr
	}
	jump := c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	c.inWhereConjunct = false
	return jump, nil
}

// opVWriteRow runs OpVWriteRow: record the row the scan cursor is on as one the
// WHERE selected, together with its already-evaluated SET values. It is
// SQLite's OP_MakeRecord+OP_Insert into the ephemeral table (update.c:1320-1327)
// and its OP_RowSetAdd (delete.c:582).
func (m *vdbe) opVWriteRow(op *Instruction) {
	if m.vtabWrite == nil {
		m.vtabWrite = vtabWriteRowSet{}
	}
	var vals []Value
	if op.P3 > 0 {
		// COPIED out of the registers rather than aliased, for opVInsert's
		// reason and one more of its own: these outlive the loop iteration that
		// produced them, because every later row overwrites the same register
		// block. That is not a lifetime a register block can express.
		vals = make([]Value, op.P3)
		copy(vals, m.regs[op.P1:op.P1+op.P3])
	}
	// The rowid register was filled by OpRowid off a MATERIALIZED cursor, whose
	// rowids are the module's own (openMaterializedCursor) -- never the all-zero
	// placeholder an ordinary derived table gets, and never NULL.
	m.vtabWrite[m.regs[op.P2].I] = vals
}

// opVWrite runs OpVWrite: hand every row the scan selected to the virtual
// table's writer. It is SQLite's OP_VUpdate at the end of the second loop
// (update.c:1348 / delete.c:644), with this engine's one-call-per-statement
// deviation (see this file's doc comment).
//
// args/outer are nil on this route BY CONSTRUCTION: they exist only for the
// module halves' writeRowSelected/writeApplySetList, which a non-nil selection
// bypasses entirely and which now decline outright (vtab_write.go), and the
// compilers above decline every shape that could reach them by another door.
func (m *vdbe) opVWrite(op *Instruction) error {
	plan := op.P4.(*vtabWritePlan)
	sel := m.vtabWrite
	if sel == nil {
		// The scan matched no row, so OpVWriteRow never ran. It still has to
		// arrive NON-NIL: nil is this parameter's "no compiled selection was
		// made; decide which rows here" signal, and a WHERE-less statement
		// would then select every row. See vtabWriteRowSet.
		sel = vtabWriteRowSet{}
	}
	// One statement, one selection -- the ephemeral table the C drops at
	// update.c:1357.
	m.vtabWrite = nil
	var n int
	var err error
	if plan.del != nil {
		n, err = m.wctx.db.deleteVtab(plan.vm, plan.del, nil, nil, sel)
	} else {
		n, err = m.wctx.db.updateVtab(plan.vm, plan.upd, nil, nil, sel)
	}
	// Recorded even when the statement ERRORED, and by ASSIGNMENT -- for the
	// reasons opVInsert's identical pair states: one OpVWrite is one whole
	// statement however many rows it turns out to have touched, and an OR
	// IGNORE run that stops partway still owes its predecessors' count to
	// runWrite's deferred setChanges.
	m.wctx.rowsAffected = n
	return err
}
