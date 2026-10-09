// The VDBE write path: compiles INSERT / UPDATE / DELETE (and, by delegation,
// CREATE/DROP of tables, views and indexes) into a Program run on the same
// register machine (vdbe.go) as reads.
//
// Expressions (VALUES, SET right-hand sides, WHERE) compile with the read
// path's compileExpr, and the WHERE scan uses the ordinary cursor opcodes over a
// row-store cursor (openRowStoreCursor). The mutation itself delegates to the
// shared row-level helpers (insert_write.go, write_update_delete.go, the DDL
// methods), so rowid assignment, constraint checks and last_insert_rowid live in
// one place.
//
// Write opcodes mutate the in-memory row store (tableMeta.rows), not a live
// b-tree; storage is written at commit (writer.go). A statement that fails to
// compile is an error; there is nothing to fall back to.
package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// writeCtx is one write statement's mutable VM state (see vdbe.wctx): the *DB
// write handle whose logical row store the write opcodes mutate, plus the
// per-statement bookkeeping the delegating opcodes accumulate and report their
// outcome through. A fresh writeCtx is built per statement by runWrite.
type writeCtx struct {
	db *DB

	// changeMark is the capture-log length (rowhook.go) as of this statement's
	// start, truncated back to on rollback. last_insert_rowid deliberately has
	// no counterpart here: it is CONNECTION state that C SQLite does not
	// revert when a statement's rows are undone -- see rollbackInsertedRows
	// (insert_write.go) for the oracle evidence.
	changeMark int

	// fs is the trigger recursion/firing state, shared across every cascaded
	// trigger sub-program of this one statement (see opFireTriggers). Lazily
	// created by fireState.
	fs *triggerFireState

	// journal is this statement's undo log: one entry per storage mutation
	// (insert, delete, update), each of which puts the row store back exactly
	// as it was. runWrite runs them in reverse on any failure -- the engine's
	// equivalent of SQLite's statement journal. The row-store mutations are
	// typed entries rather than closures, and the first few live in
	// journalBuf, so a one-row statement journals without allocating.
	journal    []undoEntry
	journalBuf [2]undoEntry

	// bulkChecked / bulkTbl: whether this statement has decided on a
	// direct-path load (bulkLoadFor), and the table it is loading.
	bulkChecked bool
	bulkTbl     *tableMeta

	rowsAffected int // affected-row count, incremented by the storage opcodes; returned by runWrite

	// aincs is the AUTOINCREMENT state of every table this statement has read
	// a sequence for (aincState, insert_write.go), written back when it
	// completes.
	aincs []*aincState
	// rowsInserted is insert.c's regRowCount -- the rows this statement really
	// INSERTED, which "PRAGMA count_changes" answers as "rows inserted" and
	// which is NOT rowsAffected: OpUpsertStore counts a DO UPDATE into
	// rowsAffected (sqlite3_changes() counts it too) and never into this, since
	// OE_Update jumps past insert.c's AddImm. Only OpInsert increments it. See
	// DB.nRowsInserted (writer.go).
	rowsInserted int

	// pinnedTable models C's OP_CursorLock, which an UPDATE (only) wraps around a
	// REPLACE conflict's victim delete (insert.c:2611). A pinned cursor makes any
	// other write to that table fail with SQLITE_CONSTRAINT_PINNED (btree.c:762),
	// so a victim's DELETE trigger that writes the table being updated aborts the
	// whole statement with "constraint failed".
	//
	// This engine holds no b-tree cursor, so the pin is checked at the opcodes that
	// mutate a row (pinViolation's callers). Statements inside a victim delete's
	// cascade run under OE_Replace (trigger.c:1137), so no IGNORE arm can
	// false-positive.
	pinnedTable *tableMeta

	// returningCols/returningRows carry a RETURNING statement's result set out
	// of an opcode that produced it whole rather than row by row -- today the
	// vtab writer's (vdbe_vtab_write.go). See execReturningViaVM: the VM is the
	// only executor, so a RETURNING statement runs inside a program like every
	// other one, and hands its rows back through here.
	returningCols []string
	returningRows [][]Value

	// prepareFailed is set when the statement fails a check C makes at prepare
	// time (no such table/column/function, a view without INSTEAD OF trigger).
	// sqlite3VdbeHalt only updates changes() for a VM that reached RUN state, so
	// such a statement must leave changes()/total_changes() untouched.
	prepareFailed bool
}

// insertPlan is the P4 payload of OpInsert: the target
// table's metadata, the statement's table name (for byte-identical error
// text), whether an explicit column list was given, and (if so) the resolved
// column indices -- everything insertRowFromValues needs beyond the evaluated
// row values.
type insertPlan struct {
	tbl         *tableMeta
	displayName string
	colsGiven   bool
	colIdx      []int

	// appendArmable is the compile-time half of arming a bulk append
	// (insert_stream_bounded.go): an INSERT ... SELECT whose source runs as a
	// co-routine and whose program does nothing but build and store rows
	// (insertProgramArmable). The run-time half checks the table and session.
	appendArmable bool

	// action / explicitOr are the conflict resolution for THIS insert: the
	// statement's OR-clause (conflictAbort/false for a plain INSERT), applied
	// per candidate row by OpInsert exactly as SQLite bakes the ON CONFLICT
	// algorithm into its generated constraint checks. See storeResolvingConflicts.
	action     conflictAction
	explicitOr bool

	// skipAddr is the row's end label: C's ignoreDest (insert.c:2367, 2593), where
	// an OE_Ignore conflict jumps. The jump skips the row counter (insert.c:1601)
	// and the AFTER triggers, including RETURNING (codeReturningTrigger,
	// trigger.c:1507).
	//
	// Zero means no jump (address 0 is always OpInit). The upsert tail's plain
	// OpInsert leaves it zero: OpUpsertFind already proved there is no conflict.
	skipAddr int

	// replaceDelBefore / replaceDelAfter are tbl's OWN compiled BEFORE/AFTER
	// DELETE trigger programs, fired for each row a REPLACE conflict
	// implicitly deletes. Both nil -- the overwhelmingly common case -- means
	// the victim is simply dropped, which is exactly what the C does when
	// SQLITE_RecTriggers is off or the table has no DELETE trigger
	// (insert.c:2214-2218 leaves pTrigger and regTrigCnt zero, and the
	// OE_Replace arms then take their "no trigger" branch,
	// insert.c:2343-2356). Set by compileReplaceVictimDeletePlans; consumed by
	// storeResolvingConflicts.
	replaceDelBefore *triggerFirePlan
	replaceDelAfter  *triggerFirePlan
}

// upsertPlan is OpUpsertFind's payload: the target table, the resolved target
// constraint columns (empty for a bare "ON CONFLICT" with no target, which
// matches any conflict), the register holding the existing conflicting row's
// rowid, whether the clause is DO NOTHING, and the address to jump to when the
// candidate conflicts but the clause is DO NOTHING (skip the row).
type upsertPlan struct {
	tbl           *tableMeta
	displayName   string
	existRowidReg int
	skipAddr      int
	arms          []upsertArm
	// action/explicitOr are the statement's, for a conflict no clause names:
	// that constraint resolves as it would without an upsert (insert.c:2248-
	// 2254, :2466-2474).
	action     conflictAction
	explicitOr bool
}

// upsertArm is one ON CONFLICT clause as OpUpsertFind sees it: the target it
// names (upsertTargetKey; "" for the clause with no target, which is last and
// takes every conflict no earlier clause did), and where its DO UPDATE block
// starts.
type upsertArm struct {
	key       string
	doNothing bool
	addr      int
}

// updatePlan is the P4 payload of OpUpdateCollect / OpUpdateApply: the target
// table, the statement's table name (for error text), and the SET clauses'
// resolved target column indices (parallel to the evaluated SET values).
type updatePlan struct {
	tbl         *tableMeta
	displayName string
	colIdx      []int

	// setCol[i] reports whether column i is one this UPDATE actually assigns.
	// opUpdateRow uses it to re-read every OTHER column from the row store
	// just before storing, so a BEFORE trigger that modified THIS row is not
	// clobbered by the pre-trigger image the cursor read -- see opUpdateRow.
	// opUpsertReload uses it for the same reason and only where the same thing
	// can have happened: emitUpsertTail emits that opcode alongside a BEFORE
	// UPDATE program only.
	setCol []bool

	// rowRegs is the row image's register block, which opUpsertReload refills
	// after BEFORE triggers, as update.c:1002-1017 does for regNew. The constraint
	// checks, the AFTER UPDATE program's NEW.* and RETURNING all read it. Set only
	// on OpUpsertReload's plan.
	rowRegs []int

	// conflictAware turns on opUpdateRow's ON CONFLICT resolution: the new row is
	// probed against every UNIQUE constraint before it is stored. It is set only
	// when there is a policy to apply (an OR clause or a declared ON CONFLICT
	// default); otherwise the plain post-write check (checkUniqueIndexesForRow)
	// reports violations with C's error text and order.
	//
	// action / explicitOr resolve exactly as insertPlan's do (conflict.go).
	conflictAware bool
	action        conflictAction
	explicitOr    bool

	// skipAddr is the row's labelContinue (update.c:1128), C's ignoreDest on an
	// OE_Ignore resolution. The jump skips the AFTER triggers, RETURNING and the row
	// counter. Meaningful only when conflictAware.
	skipAddr int

	// replaceDelBefore / replaceDelAfter are tbl's OWN compiled BEFORE/AFTER
	// DELETE trigger programs, fired for each row this UPDATE's OR REPLACE
	// resolution implicitly deletes -- insertPlan's two fields of the same name,
	// for the UPDATE half of the same C. sqlite3GenerateConstraintChecks is
	// literally the routine an UPDATE shares with an INSERT (update.c:1031), so
	// insert.c:2220-2222's "if( db->flags&SQLITE_RecTriggers )" and its two
	// OE_Replace arms (:2339-2340, :2613-2615) govern both. Both nil -- the
	// overwhelmingly common case -- means the victim is simply dropped. Set by
	// compileReplaceVictimDeletePlans; consumed by opUpdateRow.
	replaceDelBefore *triggerFirePlan
	replaceDelAfter  *triggerFirePlan

	// upsertBefore / upsertAfter are tbl's own compiled BEFORE/AFTER UPDATE
	// trigger programs for an "ON CONFLICT ... DO UPDATE" branch. They are
	// carried on the plan rather than reached through an OpFireTriggers of
	// their own ONLY so subProgramsOf can see them from the plan the way it
	// sees replaceDelBefore/After; emitUpsertTail emits real OpFireTriggers
	// instructions for them, which is what actually runs them. Both nil for
	// every other updatePlan.
	upsertBefore *triggerFirePlan
	upsertAfter  *triggerFirePlan
}

// typeCheckPlan is OpTypeCheck's P4 payload: the STRICT table whose declared
// column datatypes the row image must satisfy, and the "engine: INSERT into
// <t>" / "engine: UPDATE <t>" prefix its error text carries -- the same
// prefix the row builders (buildFullRow / buildPendingUpdate) wrap
// checkStrictColumnTypes' message in, so the two write paths report a STRICT
// violation byte-identically. The prefix names the statement's own table
// spelling, not tbl.name, for the same reason every other write plan here
// carries a displayName.
type typeCheckPlan struct {
	tbl    *tableMeta
	prefix string
}

// ddlKind selects which existing DDL implementation OpDdl delegates to.
type ddlKind int

const (
	ddlCreateTable ddlKind = iota
	ddlCreateIndex
	ddlCreateView
	ddlDropIndex
	ddlDropTable
	ddlDropView
	ddlAlterTable
	ddlCreateTrigger
	ddlDropTrigger
	ddlCreateVirtualTable
	ddlPragma
	ddlAnalyze
	ddlReindex
	ddlVacuum
	ddlAttach
	ddlDetach

	// ddlKindCount is the sentinel that makes the set ENUMERABLE, so
	// TestDdlKindGuardsAreExhaustive can walk every kind and fail on one that
	// no case of guards() names. Keep it last.
	ddlKindCount
)

// guards reports which DDL interlocks this kind runs first:
// writableSchemaDDLDecline + fts5TxnPendingDDLGuard for CREATE/DROP/ALTER,
// fts5TxnPendingDDLGuard alone for ANALYZE/REINDEX, neither for PRAGMA, VACUUM,
// ATTACH and DETACH.
//
// Every kind is named explicitly; TestDdlKindGuardsAreExhaustive fails on a new
// unnamed kind. The default guards everything, which can only decline.
func (k ddlKind) guards() (writableSchema, fts5Txn bool) {
	switch k {
	case ddlCreateTable, ddlCreateIndex, ddlCreateView, ddlDropIndex, ddlDropTable,
		ddlDropView, ddlAlterTable, ddlCreateTrigger, ddlDropTrigger, ddlCreateVirtualTable:
		return true, true
	case ddlAnalyze, ddlReindex:
		return false, true
	case ddlPragma, ddlVacuum, ddlAttach, ddlDetach:
		// Neither interlock, deliberately: none of these four is a schema
		// edit, and declining them behind a DDL guard would change behaviour
		// this engine has always had (see the doc comment above).
		return false, false
	}
	return true, true
}

// txnPlan is OpTxn's payload: one transaction-control verb and, for the
// savepoint forms, its name. Both are decided at COMPILE time -- C SQLite
// parses them in the grammar and bakes them into the operand (the savepoint
// name is P4 of OP_Savepoint, sqlite3Savepoint, build.c:5303; BEGIN and
// COMMIT/ROLLBACK carry no operand at all beyond OP_AutoCommit's two immediate
// values, sqlite3BeginTransaction, build.c:5245, and sqlite3EndTransaction,
// build.c:5281).
type txnPlan struct {
	verb txnVerb
	name string
}

// ddlPlan is the P4 payload of OpDdl: which DDL or utility statement to run and
// its verbatim SQL text (the existing CreateTable/CreateIndex/execPragma/
// execAttach/... parse it themselves).
type ddlPlan struct {
	kind ddlKind
	sql  string
}

// openRowStoreCursor builds a materialized cursor over tbl's current row store
// for an UPDATE/DELETE WHERE scan, in ascending rowid order, normalizing each
// row as rowEvalCtx does so OpColumn/OpRowid see identical values. Marked
// materialized so rewind never touches a nil pager.
func openRowStoreCursor(tbl *tableMeta) *vdbeCursor {
	// colMask: allColumns because the ZERO value of a column mask is "want
	// nothing", and a cursor that has not opted into masking must never be
	// mistaken for one that asked for an empty row. This cursor cannot reach a
	// masked decode today -- rewind() materializes it from the row store rather
	// than opening a b-tree scan -- so this is stating the invariant, not
	// relying on it.
	return &vdbeCursor{rowStore: tbl, pos: -1, colMask: allColumns}
}

// cachedWriteProgram memoizes compileWrite per statement text for this session.
// A compiled program holds *tableMeta pointers, so it is reusable only while
// they are live. Three guards:
//
//   - db.schemaGen, bumped by every DDL (including TEMP, which leaves the
//     schema cookie alone);
//   - db.txGen, bumped by restoreSnapshot, since a ROLLBACK swaps db.tables for
//     clones while restoring the cookie's number;
//   - Program.WritePager: a program reading a frozen snapshot is never cached,
//     or "INSERT INTO t SELECT ... FROM t" would re-read its first snapshot.
func (db *DB) cachedWriteProgram(sqlText string) (*Program, error) {
	// reverse_unordered_selects is read at compile time -- the scan order a
	// write's subqueries and its one-pass choice take -- so a program compiled
	// under one setting is not the program for the other, as for the read plan
	// cache (SetPragmaTuningValues drops that one).
	reverse := db.pragmaState.ReverseUnorderedSelects()
	if db.writePlans == nil || db.writePlanCookie != db.schemaGen || db.writePlanTxGen != db.txGen ||
		db.writePlanStat1 != db.planStats() || db.writePlanReverse != reverse {
		db.writePlans = make(map[string]*Program)
		db.writePlanCookie, db.writePlanTxGen = db.schemaGen, db.txGen
		db.writePlanStat1 = db.planStats()
		db.writePlanReverse = reverse
	}
	if p, ok := db.writePlans[sqlText]; ok {
		return p, nil
	}
	p, err := db.compileWrite(sqlText)
	if err != nil {
		return nil, err
	}
	if !segPeepholesOffForTest {
		segWriteFilterPeephole(p) // see its doc comment for the shapes it may touch
	}
	if p.WritePager == nil || p.FreshWritePager {
		db.writePlans[sqlText] = p
	}
	return p, nil
}

// tryVDBEWrite compiles sqlText into a write Program and runs it. A compile
// failure is the statement's own error. Returns rows affected and
// last_insert_rowid, matching ExecArgs.
func (db *DB) tryVDBEWrite(sqlText string, args []Value) (rowsAffected, lastInsertID int64, err error) {
	prog, cerr := db.cachedWriteProgram(strings.TrimSpace(sqlText))
	if cerr != nil {
		// A parse/lex error, or the statement's verb is one this write path
		// does not run at all (unsupportedStatementErr, compileWriteProgram's
		// dispatch tail). Either way it is the statement's OWN error and is
		// reported as handled -- there is nowhere else for it to go.
		return 0, 0, cerr
	}
	ra, rerr := db.runWrite(prog, args)
	return ra, db.lastInsertRowid, rerr
}

// execReturningViaVM runs one RETURNING statement inside a program and returns
// its result set -- so RETURNING is
// not a second way into the engine's write execution, just another program.
func (db *DB) execReturningViaVM(sqlText string, args []Value) (cols []string, rows [][]Value, err error) {
	prog, cerr := db.cachedWriteProgram(strings.TrimSpace(sqlText))
	if cerr != nil {
		return nil, nil, cerr
	}
	wc := &writeCtx{db: db, changeMark: db.changeMark()}
	m := &vdbe{
		regs:         make([]Value, prog.NReg),
		cursors:      make([]*vdbeCursor, prog.NCursors),
		recRegs:      make([][]Value, prog.NRecRegs),
		sorters:      make([]*vdbeSorter, prog.NSorters),
		distinctSets: make([]*vdbeDistinctSet, prog.NDistinct),
		subCache:     make([]subCacheEntry, prog.NSubCache),
		params:       args,
		wctx:         wc,
		pager:        prog.WritePager,
	}
	if prog.FreshWritePager {
		sp, serr := db.writeSubqueryPager()
		if serr != nil {
			return nil, nil, serr
		}
		m.pager = sp
	}
	if ferr := db.fkCheckTargets(prog); ferr != nil {
		return nil, nil, ferr
	}
	db.fkBeginStatement()
	db.activeWC = wc
	defer func() { db.activeWC = nil }()
	// A RETURNING statement is an INSERT/UPDATE/DELETE and publishes changes()
	// exactly like one -- see runWrite's identical deferred call for why it has
	// to run on every exit, and for wc.prepareFailed's guard.
	if prog.CountsChanges {
		defer func() {
			if !wc.prepareFailed {
				db.setChanges(int64(wc.rowsAffected))
				// ...and insert.c's regRowCount alongside it, on exactly the
				// same condition: "PRAGMA count_changes"'s row is coded by the
				// same statement whose OP_Halt publishes changes(), so a
				// statement that never reached RUN state must leave both alone.
				db.nRowsInserted = int64(wc.rowsInserted)
			}
		}()
	}
	if aerr := wc.aincBegin(prog); aerr != nil {
		return nil, nil, aerr
	}
	outRows, rerr := m.run(prog.Insns)
	if rerr != nil {
		return nil, nil, wc.resolveHalt(rerr)
	}
	wc.aincEnd() // see runWrite
	if ferr := db.fkFinishStatement(); ferr != nil {
		wc.unwindFKViolation()
		return nil, nil, ferr
	}
	// A bytecode RETURNING program produces its rows via OpResultRow (the
	// m.run return); an opcode that produced the whole result set at once sets
	// them on the write context instead. Names come from the program's
	// ColNames, or from those captured columns.
	// Keyed on the COLUMNS, not the rows: a RETURNING statement that matched
	// nothing still reports its result COLUMNS ("DELETE FROM t WHERE 0
	// RETURNING a" is cols=[a] with zero rows in C SQLite), and keying on
	// returningRows lost them for exactly that case -- the fallback had the
	// names and this handed back the program's empty ones instead.
	if wc.returningCols != nil {
		return wc.returningCols, wc.returningRows, nil
	}
	return prog.ColNames, outRows, nil
}

// runWrite executes a compiled write Program on a fresh VM carrying this *DB
// write handle, returning the rows-affected count the terminal opcode records.
func (db *DB) runWrite(prog *Program, args []Value) (rowsAffected int64, err error) {
	wc := &writeCtx{db: db, changeMark: db.changeMark()}
	// FROM THE PROGRAM'S POOL, not fresh: a prepared INSERT run in a loop is the
	// shape this exists for, and the machine plus its register file was 30% of
	// everything such a loop allocated. See getMachine for what may be reused.
	m := prog.getMachine()
	// RELEASED LAST, not first: defers run LIFO, and every defer registered below
	// this line still reads m. Registering the release here is what puts it after
	// all of them.
	defer prog.putMachine(m)
	// Carry the session's record arena chunk across executions so a run of
	// single-row INSERTs shares allocations. Taken, not shared: a nested write
	// starts its own chunk.
	m.recChunk, m.recChunkSize = db.writeRecChunk, db.writeRecChunkSize
	db.writeRecChunk, db.writeRecChunkSize = nil, 0
	defer func() { db.writeRecChunk, db.writeRecChunkSize = m.recChunk, m.recChunkSize }()
	m.sorters = make([]*vdbeSorter, prog.NSorters)
	m.distinctSets = make([]*vdbeDistinctSet, prog.NDistinct)
	m.subCache = make([]subCacheEntry, prog.NSubCache)
	m.params = args
	m.wctx = wc
	m.pager = prog.WritePager // subqueries read this snapshot (nil if none)
	if prog.FreshWritePager {
		// A cached program's subqueries read the image as it is now, as a
		// fresh compile's would: the snapshot it was compiled against is old.
		sp, serr := db.writeSubqueryPager()
		if serr != nil {
			return 0, serr
		}
		m.pager = sp
	}
	if ferr := db.fkCheckTargets(prog); ferr != nil {
		return 0, ferr
	}
	db.fkBeginStatement()
	db.activeWC = wc
	defer func() { db.activeWC = nil }()
	// The statement's halt publishes changes()/total_changes() last, after every
	// trigger and FK sub-program, on every exit below: a failed statement publishes
	// the rows it kept. A statement that never started (returned above) publishes
	// nothing, as C's prepare-time failures do. wc.prepareFailed covers prepare-time
	// checks discovered by an opcode while running (vdbe_schema_write.go).
	if prog.CountsChanges {
		defer func() {
			if !wc.prepareFailed {
				db.setChanges(int64(wc.rowsAffected))
				// ...and insert.c's regRowCount alongside it, on exactly the
				// same condition: "PRAGMA count_changes"'s row is coded by the
				// same statement whose OP_Halt publishes changes(), so a
				// statement that never reached RUN state must leave both alone.
				db.nRowsInserted = int64(wc.rowsInserted)
			}
		}()
	}
	if aerr := wc.aincBegin(prog); aerr != nil {
		return 0, aerr
	}
	_, rerr := m.run(prog.Insns)
	if rerr != nil {
		herr := wc.resolveHalt(rerr)
		if isConflictFailErr(herr) {
			// ON CONFLICT FAIL keeps the rows before the offending one, so report them.
			// ABORT and ROLLBACK undo them and must report 0.
			return int64(wc.rowsAffected), herr
		}
		return 0, herr
	}
	// autoIncrementEnd runs ahead of OP_Halt's foreign-key check, so a
	// violation undoes its sqlite_sequence write with the rest.
	wc.aincEnd()
	// Immediate foreign keys are checked at the END of the statement, not per
	// row (see fk.go's doc comment), and a violation is an ABORT: unwind this
	// statement's journal exactly as a constraint failure inside it would.
	if ferr := db.fkFinishStatement(); ferr != nil {
		wc.unwindFKViolation()
		return 0, ferr
	}
	return int64(wc.rowsAffected), nil
}

// unwindFKViolation undoes a statement whose end-of-statement immediate FK
// check failed, by unwinding the VM's journal. rowsAffected is zeroed even with
// an empty journal: C's halt publishes 0 for a rolled-back statement
// (vdbeaux.c:3485, 3430, 3441).
func (wc *writeCtx) unwindFKViolation() {
	wc.rollback()
	wc.rowsAffected = 0
}

// ErrConflictRollback is joined onto the error of a statement whose ON
// CONFLICT ROLLBACK violation could not be applied by the engine itself
// because no engine-level transaction was active. A caller running its own
// transaction over this *DB (see driver's Conn.tx) must respond by
// discarding that transaction wholesale -- SQLite's ROLLBACK conflict
// resolution aborts the entire transaction, not just the statement.
var ErrConflictRollback = errors.New("engine: ON CONFLICT ROLLBACK aborted the transaction")

// conflictRollbackErr carries the ErrConflictRollback signal WITHOUT changing
// the reported message. errors.Join would append its own text, and the message
// a constraint violation reports is itself a conformance surface: C SQLite
// says exactly "UNIQUE constraint failed: t2.a, t2.b" for an
// "INSERT OR ROLLBACK" violation and nothing more (gated by
// compat-harness/vdbe_create_onconflict_test.go's exact error-text
// comparison), so the marker rides alongside the error instead of inside its
// text.
type conflictRollbackErr struct{ err error }

func (e conflictRollbackErr) Error() string        { return e.err.Error() }
func (e conflictRollbackErr) Unwrap() error        { return e.err }
func (e conflictRollbackErr) Is(target error) bool { return target == ErrConflictRollback }

// resolveHalt applies a write error's ON CONFLICT unwind policy (see
// conflictHalt) and returns the error to report. A plain (non-conflict) error
// is treated as ABORT.
func (wc *writeCtx) resolveHalt(rerr error) error {
	action := conflictAbort
	var ch conflictHalt
	if errors.As(rerr, &ch) {
		action = ch.action
	}
	switch action {
	case conflictFail:
		// FAIL keeps the rows applied so far. Tag the error so the driver's
		// autocommit finish does not treat it as ABORT and discard them.
		rerr = markConflictFail(conflictFail, rerr)
	case conflictRollback:
		if wc.db.txActive {
			// Undo the WHOLE enclosing transaction, not just this statement
			// -- including any savepoints open inside it (clearTxnState).
			wc.db.restoreSnapshot(wc.db.txSnapshot)
			wc.db.clearTxnState()
			wc.db.truncateChanges(wc.changeMark)
			// The rolled-back rows publish changes()==0 (vdbeaux.c:3450). A trigger's rows
			// stay in total_changes(): each body step already published them.
			wc.rowsAffected = 0
			// ...and TAG IT, exactly as the no-engine-transaction arm below does.
			// A caller that mirrors the transaction has to know it is over: the
			// driver models a SQL "BEGIN" by holding this session and now also
			// calls the engine's own Begin, so txActive is TRUE here and this arm
			// runs -- and untagged, the driver kept believing a transaction was
			// open and the following COMMIT SUCCEEDED where C SQLite answers
			// "cannot commit - no transaction is active" ("BEGIN; INSERT OR
			// ROLLBACK ... <violation>; COMMIT" -- the rows were right in both
			// engines, the COMMIT's own answer was not).
			rerr = conflictRollbackErr{rerr}
		} else {
			// No transaction is active AT THE ENGINE LEVEL -- but the caller
			// may still be inside one it manages itself (driver models a
			// SQL "BEGIN" by holding this *DB open across statements rather
			// than by calling the engine's own Begin, so db.txActive is false
			// there). Undo this statement and tag the error so such a caller
			// knows the whole transaction must be discarded, which is what
			// C SQLite does: after an ON CONFLICT ROLLBACK violation inside
			// BEGIN, the transaction is gone and the following COMMIT itself
			// errors with "cannot commit - no transaction is active".
			wc.rollback()
			rerr = conflictRollbackErr{rerr}
		}
	default: // conflictAbort
		wc.rollback()
	}
	return rerr
}

func (wc *writeCtx) fireState() *triggerFireState {
	if wc.fs == nil {
		wc.fs = &triggerFireState{}
	}
	return wc.fs
}

// rollback unwinds this statement's storage mutations newest first (SQLite's
// statement journal), zeroes the row count and truncates the change-capture log
// (rowhook.go) to the statement's start.
//
// Under journal_mode=off inside a transaction the unwind is skipped, as in C,
// and the stored rows survive; changes() still reads 0, since C's halt-time
// reset is unconditional.
func (wc *writeCtx) rollback() {
	// The deferred FK counters roll back with the statement -- "if(
	// eOp==SAVEPOINT_ROLLBACK ){ db->nDeferredCons = p->nStmtDefCons;
	// db->nDeferredImmCons = p->nStmtDefImmCons; }" (vdbeaux.c:3258-3261) --
	// whether or not it had stored anything: fkDrain moves them per row.
	if st := wc.db.fkStmt; st != nil {
		wc.db.fkDeferred, wc.db.fkDeferredImm = st.deferredBefore, st.deferredImmBefore
	}
	if len(wc.journal) == 0 {
		return
	}
	if !wc.db.journalOffUndoDisabled() {
		for i := len(wc.journal) - 1; i >= 0; i-- {
			wc.journal[i].run()
		}
		wc.db.truncateChanges(wc.changeMark)
	}
	// Every row this statement counted has just been un-stored (or, under the
	// off-mode exception above, never counted in the first place -- real
	// SQLite's own halt path resets nChange unconditionally): C SQLite's
	// statement rollback publishes changes()==0 either way (see runWrite,
	// which does the publishing).
	wc.rowsAffected = 0
}

// ---- compilers ----

// compileWrite dispatches sqlText (already trimmed) to the INSERT/UPDATE/DELETE
// compiler, to OpDdl or to OpTxn by dispatch keyword (writeDispatchKeyword, so
// a leading WITH clause does not hide the verb), mirroring Exec's own dispatch.
// A statement no case names is an ERROR -- unsupportedStatementErr -- not
// something handed to a second executor. See AGENTS.md Rule 1.
func (db *DB) compileWrite(sqlText string) (*Program, error) {
	outer := db.aincCollect
	var ainc []*tableMeta
	db.aincCollect = &ainc
	prog, err := db.compileWriteProgram(sqlText)
	db.aincCollect = outer
	if prog != nil {
		prog.Ainc = ainc
		// changes()/total_changes() are published by INSERT/UPDATE/DELETE and
		// by nothing else -- see Program.CountsChanges. Decided from the
		// leading keyword here, in the one place that has already classified
		// the statement, so every compiled program carries the answer.
		prog.CountsChanges = stmtCountsChanges(sqlText)
	}
	return prog, err
}

// stmtCountsChanges reports whether sqlText is one of the statement kinds that
// publishes changes(): INSERT (including the REPLACE spelling), UPDATE,
// DELETE. writeDispatchKeyword already folds REPLACE and the "INSERT OR ..."
// forms onto the right verb, so this is that classification reused.
func stmtCountsChanges(sqlText string) bool {
	toks, err := lex(sqlText)
	if err != nil {
		return false
	}
	switch writeDispatchKeyword(sqlText, toks) {
	case "INSERT", "REPLACE", "UPDATE", "DELETE":
		return true
	}
	return false
}

func (db *DB) compileWriteProgram(sqlText string) (*Program, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	// A statement that is only whitespace and/or comments lexes to nothing but
	// the terminating EOF token. C SQLite prepares such input as a NO-OP
	// (sqlite3_prepare yields a NULL statement and exec succeeds with no rows),
	// so it compiles to an empty program rather than to an error. Checked
	// before the tkIdent test below, which an EOF token would otherwise fail.
	if len(toks) == 0 || toks[0].kind == tkEOF {
		return compileNoopWrite(), nil
	}
	if toks[0].kind != tkIdent {
		return nil, unsupportedStatementErr(strings.TrimSpace(sqlText))
	}
	// writeDispatchKeywordAt (insert_write.go), not toks[0], so a statement whose
	// verb sits behind a leading "WITH <cte-list>" dispatches on the VERB --
	// the same classification Exec and driver already use.
	// Sniffing toks[0] instead sent every WITH-prefixed INSERT/UPDATE/DELETE
	// past all of these cases and into the fallback. verbAt is that verb's own
	// index in toks, from the same call, for the operand reads below.
	kw, verbAt := writeDispatchKeywordAt(sqlText, toks)
	switch kw {
	case "INSERT", "REPLACE", "UPDATE", "DELETE":
		var prog *Program
		switch kw {
		case "INSERT", "REPLACE":
			// REPLACE is INSERT with OE_Replace (parse.y:1114); parseInsertStmt sets the
			// conflict action itself.
			prog, err = totalWrite(db.compileInsertWrite(sqlText))(sqlText)
		case "UPDATE":
			prog, err = totalWrite(db.compileUpdateWrite(sqlText))(sqlText)
		default:
			prog, err = totalWrite(db.compileDeleteWrite(sqlText))(sqlText)
		}
		if prog != nil {
			// Record the DML target so runWrite can raise an unresolvable foreign key
			// before running, as C does during codegen. Read from verbAt, since a leading
			// WITH puts the verb after the CTE list.
			fkSchema, fkTable := fkDMLTargetName(kw, toks, verbAt)
			if tbl := db.findTableMetaIn(writeTargetScope(fkSchema), fkTable); tbl != nil {
				prog.FKTargets = []*tableMeta{tbl}
				prog.FKReach = fkStatementReach(kw, toks, verbAt)
			}
		}
		return prog, err
	case "CREATE":
		if isCreateVirtualTableStmt(toks) {
			return db.compileDdlWrite(ddlCreateVirtualTable, sqlText)
		}
		if isCreateIndexStmt(toks) {
			return db.compileDdlWrite(ddlCreateIndex, sqlText)
		}
		if isCreateViewStmt(toks) {
			return db.compileDdlWrite(ddlCreateView, sqlText)
		}
		if isCreateTriggerStmt(toks) {
			return db.compileDdlWrite(ddlCreateTrigger, sqlText)
		}

		return db.compileDdlWrite(ddlCreateTable, sqlText)
	case "DROP":
		if len(toks) > 1 && toks[1].kind == tkIdent {
			switch toks[1].upper() {
			case "INDEX":
				return db.compileDdlWrite(ddlDropIndex, sqlText)
			case "TABLE":
				return db.compileDdlWrite(ddlDropTable, sqlText)
			case "VIEW":
				return db.compileDdlWrite(ddlDropView, sqlText)
			case "TRIGGER":
				return db.compileDdlWrite(ddlDropTrigger, sqlText)
			}
		}
	case "ALTER":
		return db.compileDdlWrite(ddlAlterTable, sqlText)
	case "PRAGMA":
		return db.compileDdlWrite(ddlPragma, sqlText)
	case "ANALYZE":
		return db.compileDdlWrite(ddlAnalyze, sqlText)
	case "REINDEX":
		return db.compileDdlWrite(ddlReindex, sqlText)
	case "VACUUM":
		return db.compileDdlWrite(ddlVacuum, sqlText)
	case "ATTACH":
		return db.compileDdlWrite(ddlAttach, sqlText)
	case "DETACH":
		return db.compileDdlWrite(ddlDetach, sqlText)
	case "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE":
		// The verb and (for the savepoint forms) the name are decided HERE, at
		// compile time, exactly as SQLite's grammar decides them before
		// emitting OP_AutoCommit/OP_Savepoint -- see txnPlan. A malformed
		// spelling (a savepoint statement with no name, trailing garbage)
		// leaves ok=false and falls through to the same "unsupported
		// statement" error the old keyword dispatch reported for it.
		if verb, name, ok := parseTxnStmt(strings.TrimSpace(sqlText), toks); ok {
			return compileTxnWrite(verb, name)
		}
		// ...except that a MALFORMED savepoint statement is a syntax error in
		// C, not an unsupported one: `near "123": syntax error` for a bad name
		// token, "incomplete input" when the statement just ended (parse.y:
		// 44-51). See txnStmtSyntaxErr.
		if serr := txnStmtSyntaxErr(toks); serr != nil {
			return nil, serr
		}
	}
	return nil, unsupportedStatementErr(strings.TrimSpace(sqlText))
}

// compileNoopWrite is the empty program: Init, Halt. It runs nothing and
// reports nothing, which is what a whitespace/comments-only statement does.
func compileNoopWrite() *Program {
	c := &compiler{}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg}
}

// compileTxnWrite compiles one transaction-control verb into a single OpTxn --
// the shape sqlite3BeginTransaction (build.c:5245), sqlite3EndTransaction
// (build.c:5281) and sqlite3Savepoint (build.c:5303) each emit: one opcode
// carrying everything the parser already worked out.
func compileTxnWrite(verb txnVerb, name string) (*Program, error) {
	c := &compiler{}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpTxn, P4: &txnPlan{verb: verb, name: name}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg}, nil
}

// totalWrite is what is LEFT of the write path's escape hatch. It used to turn
// a shape the DML sub-compilers could not model (errVDBEUnsupported) into a
// one-instruction program that ran the statement another way; that opcode and
// its drivers are all deleted, so this now passes
// the compile through and lets the error be an error -- AGENTS.md Rule 1's
// answer, and the whole point.
//
// It stays as a named wrapper only because its call sites read as one
// expression, and because the name is what the gate's route census counts.
func totalWrite(prog *Program, err error) func(string) (*Program, error) {
	return func(sqlText string) (*Program, error) {
		if err != nil {
			// RULE #1: a shape the compiler cannot lower is an ERROR. It is
			// never routed elsewhere -- not as a fallback, not
			// temporarily. compileWrite is no longer "TOTAL" by virtue of a
			// hatch; it is total or it declines.
			return nil, err
		}
		return prog, nil
	}
}

// compileInsertWrite compiles "INSERT INTO t [(cols)] VALUES (...)[,(...)]"
// into: for each VALUES tuple, the tuple's value expressions (literals/bound
// parameters -- parseInsertStmt already restricts INSERT VALUES to those)
// compiled into a contiguous register block, an OpMakeRecord packing them, and
// an OpInsertRow delegating the per-row insert; then one OpInsertVerify for the
// statement's UNIQUE-index check. An INSERT ... SELECT (parseInsertStmt now
// parses that form too -- see insertStmt.selectStmt's doc comment) is declined
// explicitly, right below, rather than compiled: sourcing rows from an
// arbitrary SELECT is out of this bytecode compiler's scope. Anything else
// parseInsertStmt rejects (OR IGNORE/REPLACE, UPSERT, ...) surfaces as a parse
// error here. Either way the failure is the statement's own error.
func (db *DB) compileInsertWrite(sqlText string) (*Program, error) {
	stmt, err := parseInsertStmt(sqlText)
	if err != nil {
		return nil, err
	}
	return db.compileInsertStmt(stmt, nil)
}

func (db *DB) compileInsertStmt(stmt *insertStmt, trig *trigCompileCtx) (*Program, error) {
	// A database-qualified target ("INSERT INTO main.t ..."). The qualifier
	// PICKS A CATALOG; it is not a capability gate: delete.c:31-46
	// sqlite3SrcListLookup is the one routine INSERT (insert.c:963), UPDATE
	// (update.c:362) and DELETE (delete.c:345) all resolve their target
	// through, and it goes to build.c:481-496 sqlite3LocateTableItem, which
	// takes zDb straight from the qualifier and hands it to
	// sqlite3LocateTable. So this compiler resolves through the SAME scoped
	// pair the row-level insert path uses (insert_write.go) rather than declining.
	if err := db.checkWriteSchemaQualifier(stmt.schema); err != nil {
		return nil, err
	}
	// The target's own "main."/"temp." qualifier selects a catalog: "INSERT
	// INTO main.tbl" writes MAIN's tbl even while a temp tbl shadows it
	// (tkt2817.test), and an unqualified target resolves temp-first. See
	// temp_schema.go.
	scope := writeTargetScope(stmt.schema)
	// "INSERT INTO sqlite_master VALUES(...)" is a DIFFERENT shape -- a direct
	// catalog write, whose store is schema_write_direct.go's overlay rather
	// than a b-tree. It has its own emitter (vdbe_schema_write.go), which
	// declines outright for the shapes that overlay itself refuses.
	if writableSchemaTarget(stmt.schema, stmt.table) {
		return db.compileSchemaCatalogInsert(stmt, trig)
	}
	tbl := db.findTableMetaIn(scope, stmt.table)
	if tbl == nil {
		// A VIEW target. Not a capability gate and not a different statement
		// kind: sqlite3Insert resolves a view through the SAME
		// sqlite3SrcListLookup (insert.c:963) and then merely SKIPS the cursor,
		// the affinity pass and the row store (insert.c:1273/:1490/:1501),
		// firing the view's INSTEAD OF trigger from the ordinary BEFORE-trigger
		// call at insert.c:1495. That is what compileViewInsertStmt emits; see
		// vdbe_view_write.go. Anything it does not model declines from there.
		if vm := db.findViewMetaIn(scope, stmt.table); vm != nil {
			return db.compileViewInsertStmt(stmt, vm, trig)
		}
		// A WRITABLE VIRTUAL TABLE target (fts3/fts4, fts5, rtree). Not a
		// capability gate either: sqlite3Insert resolves a vtab through the
		// SAME sqlite3SrcListLookup (insert.c:963) and codes the VALUES tuple
		// with the same sqlite3ExprCodeTarget loop (insert.c:1430-1431); only
		// the tail differs, one OP_VUpdate (insert.c:1558-1562) in place of the
		// constraint checks and the row store. That is what
		// compileVtabInsertStmt emits; see vdbe_vtab_write.go, including why
		// UPDATE and DELETE against a vtab are not lowered with it. Anything it
		// does not model declines from there, landing on insertIntoVtab exactly
		// as before.
		if vt := db.findVtabMetaIn(scope, stmt.table); vt != nil {
			return db.compileVtabInsertStmt(stmt, vt, trig)
		}
		// No table, view or vtab: a semantic error, raised at prepare time as C does
		// (sqlite3SrcListLookup: insert.c:963, delete.c:428, update.c:362).
		//
		// A trigger body is the exception: its target may resolve at fire time in
		// another session's catalog (attachedOriginatingFireInsert, attach_write.go),
		// so a body keeps the decline.
		if trig != nil {
			return nil, fmt.Errorf("%w: no such table: %s", errVDBEUnsupported, stmt.table)
		}
		return nil, semanticf("engine: no such table: %s", stmt.table)
	}
	// See table_load.go's package doc comment: this compile-time target
	// resolution is one of the choke points that must load tbl before the
	// bytecode this function emits (OpOpenWrite/OpInsertRow below) touches
	// tbl.rows at run time.
	if err := db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	// autoIncBegin, ahead of the trigger programs as in sqlite3Insert
	// (insert.c:1044).
	if err := db.noteAutoincrement(tbl); err != nil {
		return nil, err
	}
	var firePlan, beforePlan *triggerFirePlan
	if db.tableHasTriggers(tbl.name, tbl.isTemp, triggerInsert) {
		// Triggers fire per row for VALUES and INSERT ... SELECT alike: the BEFORE
		// call (insert.c:1495) and AFTER call (insert.c:1604) sit inside the insert
		// loop. With a trigger the source is materialized (insert.c:1167,
		// OpOpenDerived).
		//
		// An OR clause overrides each trigger body statement's own conflict policy
		// (trigger.c:1137); compileTriggerFirePlanOrconf bakes it in. A declared
		// constraint default is not passed down, as in C.
		//
		// emitInsertRowBody emits checks in C's order: BEFORE triggers (insert.c:1494),
		// then rowid (:1531), then NOT NULL / CHECK / UNIQUE
		// (sqlite3GenerateConstraintChecks, :1569). The order is observable under
		// IGNORE and FAIL. STRICT type checks run before the fire, as in C
		// (insert.c:1487); their position is unobservable since they always abort.
		//
		// Nested triggers compile with the ordinary path (trigger.c:1160), bounded by
		// triggerPrgMemo (trigger.c:1269/1376). DEFAULT VALUES is one empty VALUES
		// tuple (insert.c:1413). For an upsert, BEFORE fires before the probe and AFTER
		// INSERT only on the plain-insert branch (see emitUpsertTail).
		if !db.triggersCompilable(tbl, triggerInsert) {
			return nil, fmt.Errorf("%w: INSERT trigger shape not lowered", errVDBEUnsupported)
		}
		// One memo for BOTH timings and for everything they cascade into --
		// SQLite hangs its TriggerPrg list off the TOP-LEVEL Parse
		// (sqlite3ParseToplevel, trigger.c:1259), so a trigger reached twice
		// down two different paths of one statement is coded once. The orconf
		// half of its key is getRowTrigger's own
		// ("pPrg->pTrigger!=pTrigger || pPrg->orconf!=orconf", trigger.c:1376),
		// which is what keeps one trigger reached under two different policies
		// -- now possible, since a body statement's own OR-clause no longer
		// declines -- coded twice rather than shared.
		memo := trigMemo(trig)
		bp, berr := db.compileTriggerFirePlanOrconf(tbl, triggerInsert, triggerBefore, memo, stmt.orAction, stmt.explicitOr)
		if berr != nil {
			return nil, berr
		}
		ap, aerr := db.compileTriggerFirePlanOrconf(tbl, triggerInsert, triggerAfter, memo, stmt.orAction, stmt.explicitOr)
		if aerr != nil {
			return nil, aerr
		}
		beforePlan, firePlan = bp, ap
	}
	colIdx, err := resolveNamedColumns(tbl, stmt.table, stmt.cols)
	if err != nil {
		return nil, err
	}
	if stmt.selectStmt != nil {
		return db.compileInsertSelectWrite(stmt, tbl, colIdx, trig, beforePlan, firePlan)
	}

	// Column mapping is resolved once at compile time (see buildFullRow for the
	// first-wins / last-wins quirks). DEFAULT VALUES is one empty tuple with every
	// slotOf entry -1, so every column takes its default.
	rows := stmt.rows
	var slotOf []int
	var rowidSlot int
	if stmt.defaultValues {
		slotOf = make([]int, len(tbl.cols))
		for i := range slotOf {
			slotOf[i] = -1
		}
		rowidSlot = -1
		rows = [][]Expr{nil}
	} else {
		var merr error
		slotOf, rowidSlot, merr = insertSlotMap(tbl, stmt, colIdx)
		if merr != nil {
			return nil, merr
		}
	}
	if derr := insertRejectOmittedDefaults(tbl, stmt, slotOf); derr != nil {
		return nil, derr
	}

	// An INSERT codes its values with no sqlite3WhereBegin open: nQueryLoop is
	// the enclosing one, 0 at a top level and in every trigger body this
	// engine fires (compiler.nQueryLoop). An INSERT ... SELECT's SELECT is a
	// statement of its own and balances its own loops.
	c := &compiler{trig: trig, ownSchema: db.targetSchemaName(tbl), nQueryLoopKnown: true}
	// A VALUES subquery reads a frozen pre-statement snapshot: C wraps an
	// uncorrelated subquery in OP_Once (expr.c:3889), so every tuple of a
	// multi-row VALUES sees the pre-statement image. A reference to the target
	// table inside it declines. Built only when there is a subquery.
	//
	// A trigger body takes the live branch below instead (vdbe_live_read.go).
	var writePager *ReadOnlyPager
	freshPager := false
	if rowExprsContainSubquery(rows) {
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, perr
		}
		c.pager = sp
		if trig != nil && len(stmt.ctes) == 0 {
			// Trigger body: reads bind at fire time (vdbe_live_read.go). sp stays the
			// compile-time pager for schema facts, but no frozen snapshot reaches the
			// Program.
			//
			// A leading WITH keeps the frozen route: the live lowering has no CTE scope,
			// and a CTE must shadow a same-named table (select.c:6000, 6028). The parser
			// rejects WITH in a trigger body today, as C does.
			c.liveDB = db
		} else {
			writePager = sp
			freshPager = trig == nil && len(stmt.ctes) == 0
			// The leading WITH clause's CTEs, for the length of THIS COMPILE --
			// the same push compileUpdateStmt/compileDeleteStmt do, and the
			// reason insertStmt.ctes exists at all (dropping the clause
			// resolved a shadowed CTE name to the real table, a wrong answer on
			// both paths).
			if len(stmt.ctes) > 0 {
				defer sp.pushCTEScope(stmt.ctes)()
			}
		}
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenWrite, P1: 0, P4: tbl})
	c.allocCursor()

	// With recursive_triggers=ON, a REPLACE victim's delete fires DELETE
	// triggers (see compileReplaceVictimDeletePlans). Compiled only when some
	// constraint can resolve to OE_Replace (replaceResolutionCoded).
	var delBefore, delAfter *triggerFirePlan
	if db.replaceResolutionCoded(tbl, stmt, slotOf, rowidSlot) {
		var derr error
		delBefore, delAfter, derr = db.compileReplaceVictimDeletePlans(tbl, stmt.table, trigMemo(trig))
		if derr != nil {
			return nil, derr
		}
	}
	plan := &insertPlan{tbl: tbl, displayName: stmt.table, colsGiven: stmt.cols != nil, colIdx: colIdx, action: stmt.orAction, explicitOr: stmt.explicitOr,
		replaceDelBefore: delBefore, replaceDelAfter: delAfter}
	recReg := c.allocRec()  // record register, consumed by OpInsert before the next OpMakeRecord
	rowidReg := c.allocN(1) // rowid register for OpInsert's P3
	var colNames []string
	for _, rowExprs := range rows {
		base := c.allocN(len(tbl.cols))
		regOf := make([]int, len(rowExprs))
		for i, re := range rowExprs {
			r, cerr := c.compileExpr(re)
			if cerr != nil {
				return nil, cerr
			}
			regOf[i] = r
		}
		var rowPlans []*triggerFirePlan  // per-row plan copies whose skipAddr = this row's end
		var rowInsertPlans []*insertPlan // ditto for OpInsert's OE_Ignore jump
		// The BEFORE program, emitted from INSIDE emitInsertRowBody at
		// insert.c:1494-1496's position -- after the NEW.* image is built and
		// before the rowid is allocated. See compiler.insertBeforeFire.
		//
		// NEW.<ipk> and NEW.rowid: a BEFORE program sees the rowid the row will
		// KEEP only when this row supplied it explicitly -- an auto-assigned
		// one reads -1, since C SQLite has not allocated it yet (see
		// compiler.beforeInsertRowid). The AFTER program, and the stored
		// record, always see the real one.
		c.insertBeforeFire = nil
		if beforePlan != nil {
			c.insertBeforeFire = func() error {
				bp := *beforePlan
				rowPlans = append(rowPlans, &bp)
				c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: c.beforeInsertRowid, P3: -1, P4: &bp})
				return nil
			}
		}
		tail := func() error {
			if (beforePlan != nil || firePlan != nil) && tbl.ipkIndex >= 0 {
				c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: base + tbl.ipkIndex})
			}
			// The upsert tail runs after the BEFORE program, as in C (insert.c:1494 vs the
			// probe at :1569). Its AFTER fire is on the plain-insert branch only.
			if stmt.upsert != nil {
				var ap *triggerFirePlan
				if firePlan != nil {
					cp := *firePlan
					ap = &cp
					rowPlans = append(rowPlans, ap)
				}
				// The RETURNING block comes back from the tail rather than
				// from the emission below it: an upsert has TWO row stores
				// (the DO UPDATE arm and the plain-insert arm) and each needs
				// its own, which is why the clause used to be dropped here.
				names, uerr := c.emitUpsertTail(db, tbl, stmt, plan, base, rowidReg, recReg, ap, returningSiteOnce)
				if uerr != nil {
					return uerr
				}
				if names != nil {
					colNames = names
				}
				return nil
			}
			c.emit(Instruction{Op: OpMakeRecord, P1: base, P2: len(tbl.cols), P3: recReg})
			// A per-row COPY of the plan, so this row's OpInsert carries this
			// row's own end label in skipAddr -- the same reason the trigger
			// fire plans just above are copied per row.
			ip := *plan
			rowInsertPlans = append(rowInsertPlans, &ip)
			c.emit(Instruction{Op: OpInsert, P1: 0, P2: recReg, P3: rowidReg, P4: &ip})
			// RETURNING before the user's AFTER programs: sqlite3TriggerList prepends the
			// RETURNING trigger (trigger.c:68), so an AFTER RAISE(IGNORE) cannot suppress
			// an already-captured RETURNING row.
			if stmt.returning != nil {
				names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(base, len(tbl.cols)), rowidReg, returningSiteOnce)
				if rerr != nil {
					return rerr
				}
				colNames = names
			}
			if firePlan != nil {
				ap := *firePlan
				rowPlans = append(rowPlans, &ap)
				c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: rowidReg, P3: -1, P4: &ap})
			}
			return nil
		}
		if berr := c.emitInsertRowBody(db, tbl, stmt, plan, slotOf, rowidSlot, regOf, base, rowidReg, recReg, tail); berr != nil {
			return nil, berr
		}
		for _, rp := range rowPlans {
			rp.skipAddr = c.here() // RAISE(IGNORE) abandons this row -> its end
		}
		for _, ip := range rowInsertPlans {
			// The same label emitInsertRowBody patched its own OE_Ignore skips
			// to (nothing is emitted between the two), which is SQLite's single
			// endOfLoop: a NOT NULL/CHECK IGNORE and a UNIQUE/rowid IGNORE land
			// in the identical place.
			ip.skipAddr = c.here()
		}
	}
	c.emit(Instruction{Op: OpHalt})

	// NSubCache sizes the run-once subquery slots (too short panics). WritePager
	// is the frozen snapshot those subqueries read, and it also stops
	// cachedWriteProgram reusing this program inside a transaction where the
	// snapshot would be stale. Both are zero/nil when there is no subquery.
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: 1, NSubCache: c.nSub, WritePager: writePager, FreshWritePager: freshPager, ColNames: colNames}, nil
}

// emitUpsertTail emits INSERT ... ON CONFLICT DO NOTHING / DO UPDATE for one
// candidate row in registers base.. (rowid in rowidReg). OpUpsertFind probes
// the target: no conflict inserts; a conflict skips (DO NOTHING) or evaluates
// SET/WHERE against the existing row and "excluded" and re-stores it.
//
// It also emits RETURNING once per arm, as C codes it twice (inside
// sqlite3UpsertDoUpdate's sqlite3Update and in sqlite3Insert's AFTER block),
// and returns the column names. OP_Once is per code site (expr.c:3889), so each
// arm has its own once group for RETURNING subqueries, shared across tuples.
func (c *compiler) emitUpsertTail(db *DB, tbl *tableMeta, stmt *insertStmt, plan *insertPlan, base, rowidReg, recReg int, afterPlan *triggerFirePlan, site returningSite) ([]string, error) {
	var colNames []string
	// The clauses in order, each validated (sqlite3UpsertAnalyzeTarget,
	// upsert.c:94-222) and a repeat of an earlier target dropped: it is
	// isDup there, and sqlite3UpsertOfIndex never reaches it.
	var arms []*upsertClause
	var keys []string
	nUpdate := 0
	for up := stmt.upsert; up != nil; up = up.next {
		if err := validateUpsertTarget(db, tbl, up.targetCols, up.targetCollations, up.targetWhere); err != nil {
			return nil, declineOrSemantic(err)
		}
		key := ""
		if up.targetCols != nil {
			key = upsertTargetKey(tbl, up.targetCols)
			if slices.Contains(keys, key) {
				continue
			}
		}
		arms, keys = append(arms, up), append(keys, key)
		if !up.doNothing {
			nUpdate++
		}
	}
	if nUpdate > 1 && stmt.returning != nil && slices.ContainsFunc(stmt.returning, func(r SelectColumn) bool { return containsSubquery(r.Expr) }) {
		// ponytail: each DO UPDATE is its own RETURNING code site with its own
		// OP_Once in C, and the once groups here are per arm KIND, not per arm.
		return nil, fmt.Errorf("%w: RETURNING subquery over more than one DO UPDATE clause", errVDBEUnsupported)
	}

	existBase := c.allocN(len(tbl.cols)) // the conflicting row's columns
	existRowidReg := c.allocN(1)
	c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: existRowidReg}) // stage candidate rowid for the probe

	uplan := &upsertPlan{tbl: tbl, displayName: stmt.table, existRowidReg: existRowidReg, action: plan.action, explicitOr: plan.explicitOr}
	find := c.emit(Instruction{Op: OpUpsertFind, P1: base, P3: existBase, P4: uplan})
	// P2 (no conflict any clause takes) -> plain insert; filled in below.

	// ---- one DO UPDATE block per clause (none for DO NOTHING) ----
	var doUpdateSkips, doneJumps []int
	// Per-row copies of the fire plans, so each carries THIS row's own end
	// label in skipAddr -- compileInsertStmt's identical convention. A
	// triggerFirePlan with an unpatched skipAddr sends a RAISE(IGNORE) to
	// address 0 and loops forever; see the field's doc comment.
	var rowPlans []*triggerFirePlan
	for i, up := range arms {
		uplan.arms = append(uplan.arms, upsertArm{key: keys[i], doNothing: up.doNothing, addr: c.here()})
		if up.doNothing {
			continue
		}
		names, err := c.emitUpsertArm(db, tbl, stmt, up, i, base, rowidReg, recReg, existBase, existRowidReg, site, &doUpdateSkips, &rowPlans)
		if err != nil {
			return nil, err
		}
		if names != nil {
			colNames = names
		}
		// After DO UPDATE completes, jump over the plain-insert block to row-end.
		doneJumps = append(doneJumps, c.emit(Instruction{Op: OpGoto}))
	}

	// ---- plain-insert block (OpUpsertFind's no-conflict jump lands here) ----
	c.patch(find, c.here())
	c.emit(Instruction{Op: OpMakeRecord, P1: base, P2: len(tbl.cols), P3: recReg})
	c.emit(Instruction{Op: OpInsert, P1: 0, P2: recReg, P3: rowidReg, P4: plan})
	// AFTER INSERT fires only on this branch: both upsert outcomes leave
	// sqlite3GenerateConstraintChecks through ignoreDest (insert.c:2361, 2587),
	// which resolves below the AFTER call (insert.c:1604, 1613).
	//
	// RETURNING for the inserted row is emitted before that AFTER program,
	// matching the plain INSERT order. DO NOTHING and a WHERE-false DO UPDATE jump
	// to rowEnd and return no row.
	if stmt.returning != nil {
		names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(base, len(tbl.cols)), rowidReg, site.upsertArm(returningSiteOnceUpsertInsert))
		if rerr != nil {
			return nil, rerr
		}
		colNames = names
	}
	if afterPlan != nil {
		c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: rowidReg, P3: -1, P4: afterPlan})
	}

	// row-end: a DO NOTHING conflict, a WHERE-false DO UPDATE, and the completed
	// DO UPDATE all converge here, skipping the plain insert.
	rowEnd := c.here()
	uplan.skipAddr = rowEnd
	for _, rp := range rowPlans {
		rp.skipAddr = rowEnd // BEFORE RAISE(IGNORE) abandons this DO UPDATE
	}
	for _, a := range doneJumps {
		c.patch(a, rowEnd)
	}
	for _, a := range doUpdateSkips {
		c.patch(a, rowEnd)
	}
	return colNames, nil
}

// emitUpsertArm emits one ON CONFLICT clause's DO UPDATE block -- the code
// sqlite3UpsertDoUpdate generates for it (upsert.c:260-330) -- which
// OpUpsertFind jumps to when a conflict takes this clause. arm numbers the
// clause for the SET list's once-subquery slots.
func (c *compiler) emitUpsertArm(db *DB, tbl *tableMeta, stmt *insertStmt, up *upsertClause, arm, base, rowidReg, recReg, existBase, existRowidReg int, site returningSite, doUpdateSkips *[]int, rowPlans *[]*triggerFirePlan) ([]string, error) {
	var colNames []string
	// A DO UPDATE is an ordinary UPDATE of the conflicting row
	// (sqlite3UpsertDoUpdate calls sqlite3Update, upsert.c:325), so the table's
	// BEFORE/AFTER UPDATE triggers fire around it. With all four triggers logging,
	// C logs BEFORE INSERT, BEFORE UPDATE, AFTER UPDATE: no AFTER INSERT.
	//
	// That call passes onError OE_Abort, not OE_Default, so trigger.c:1137
	// overrides each body statement's own conflict clause with ABORT
	// (conflictAbort/true below). DO NOTHING fires nothing.
	//
	// UPDATE OF column lists are filtered as C's checkColumnOverlap does
	// (trigger.c:1502, 837) via compileUpdateTriggerFirePlan, using the SET
	// columns as pChanges.
	var upBefore, upAfter *triggerFirePlan
	if !up.doNothing && db.tableHasTriggers(tbl.name, tbl.isTemp, triggerUpdate) {
		if !db.triggersCompilable(tbl, triggerUpdate) {
			return nil, fmt.Errorf("%w: upsert DO UPDATE trigger shape not lowered", errVDBEUnsupported)
		}
		upSetCols := make([]string, len(up.sets))
		for i, a := range up.sets {
			upSetCols[i] = a.col
		}
		memo := trigMemo(c.trig)
		bp, berr := db.compileUpdateTriggerFirePlan(tbl, triggerBefore, memo, conflictAbort, true, upSetCols)
		if berr != nil {
			return nil, berr
		}
		ap, aerr := db.compileUpdateTriggerFirePlan(tbl, triggerAfter, memo, conflictAbort, true, upSetCols)
		if aerr != nil {
			return nil, aerr
		}
		upBefore, upAfter = bp, ap
	}
	// chngRowid is update.c's flag (update.c:475): the SET assigns the IPK, so
	// the DO UPDATE row moves to a new rowid. DO UPDATE is a plain sqlite3Update
	// (upsert.c:325), so update.c's rowid-change path applies.
	chngRowid := false
	colIdx := make([]int, len(up.sets))
	for i, a := range up.sets {
		idx := -1
		for j, col := range tbl.cols {
			if equalFoldName(col.Name, a.col) {
				idx = j
				break
			}
		}
		if idx < 0 {
			// C resolves an upsert's SET list like any other UPDATE's
			// (upsert.c:325-326 hands it to sqlite3Update), so an unknown name
			// is resolve.c:785's "no such column: x" -- a prepare-time
			// rejection, not this engine declining to compile the statement.
			return nil, semanticf("engine: no such column: %s", a.col)
		}
		if idx == tbl.ipkIndex {
			// A DO UPDATE that reassigns the INTEGER PRIMARY KEY -- i.e. that
			// MOVES the very row the upsert just found -- used to decline
			// here. It compiles now; see chngRowid below.
			chngRowid = true
		}
		if tbl.cols[idx].IsGenerated() {
			return nil, fmt.Errorf("engine: cannot UPDATE generated column %q", tbl.cols[idx].Name)
		}
		colIdx[i] = idx
	}

	// The rowid the row is re-stored UNDER. Without a rowid-assigning SET it
	// IS the conflicting row's own -- update.c's "regNewRowid is the same
	// register as regOldRowid" when the rowid is not being modified
	// (update.c:882-884) -- so the ordinary DO UPDATE emits not one extra
	// instruction. With one, the SET loop below fills it and OpUpsertStore
	// moves the row.
	newRowidReg := existRowidReg
	if chngRowid {
		newRowidReg = c.allocN(1)
	}
	// Two register scopes: the existing row and "excluded". The target is pushed
	// last so its name shadows the pseudo-table when the target itself is named
	// "excluded", as C does (upsert3.test); an alias ("INSERT INTO excluded AS
	// base") renames the target scope and frees "excluded" again.
	targetScope := tableScope{name: writeScopeName(stmt.alias, tbl), cols: tbl.cols, colIndex: buildColIndex(tbl.cols), noRowid: tbl.withoutRowid}
	// excludedPseudoRowCols, not tbl.cols: the pseudo-row resolves NAMES to
	// this row's registers but contributes NO declared affinity and NO
	// declared collation to a comparison, because resolve.c's excluded. arm
	// leaves the rewritten reference with no column identity at all
	// (:581-585). See excludedPseudoRowCols for the C and for the two wrong
	// answers it retires.
	exclCols := excludedPseudoRowCols(tbl.cols)
	exclScope := tableScope{name: "excluded", cols: exclCols, colIndex: buildColIndex(exclCols), unqualifiedHidden: true, noRowid: tbl.withoutRowid}
	c.pushRegScope(regScope{scope: &exclScope, regs: rowRegsFor(base, len(tbl.cols)), rowidReg: rowidReg})
	c.pushRegScope(regScope{scope: &targetScope, regs: rowRegsFor(existBase, len(tbl.cols)), rowidReg: existRowidReg})

	// Evaluate every SET right-hand side against the old row and excluded first,
	// then apply, so "SET a=b, b=a" swaps.
	//
	// Subqueries in WHERE/SET are lowered live: C codes the block inside the
	// insert loop (upsert.c:325), so they see rows stored earlier, and an
	// uncorrelated one runs once at the first conflict (expr.c:3889). The live
	// stub offers both register scopes, so a reference to either binds as a
	// correlated register read, re-run per conflicting row.
	savedSetPager, savedSetLive, savedSetLiveRow := c.pager, c.liveDB, c.liveRow
	c.pager, c.liveDB = nil, nil
	blockHasSub := upsertSetsHoldSubquery(up.sets) || containsSubquery(up.where)
	if blockHasSub {
		img, ierr := db.SnapshotPager()
		if ierr != nil {
			c.pager, c.liveDB = savedSetPager, savedSetLive
			c.popRegScope()
			c.popRegScope()
			return nil, declineOrSemantic(ierr)
		}
		c.pager = img
		c.liveRow = &liveRowCtx{regScopes: append([]regScope(nil), c.regScopes...), ownSchema: c.ownSchema, trig: c.trig,
			nQueryLoop: c.nQueryLoop, nQueryLoopKnown: c.nQueryLoopKnown}
	}
	// Cache slots for this block and the reset at its head, as in emitReturning,
	// patched after compiling (upsertSetSubqueryLifetimes). C codes the block once
	// (upsert.c:325); here it has two shapes:
	//
	//   returningSitePerRow  INSERT ... SELECT: emitted once inside the source
	//                        loop; the reset clears per-row subqueries.
	//   returningSiteOnce    unrolled VALUES: emitted per tuple; the slot rewind
	//                        makes a once subquery run a single time.
	setSubFirst, setResetAddr, setBlockStart := c.nSub, -1, 0
	// The allocator's high-water mark before the rewind below, so the
	// rewind can never hand a LATER emission a slot an earlier one owns.
	setSubHighWater, setRewound := c.nSub, false
	if blockHasSub {
		// An unrolled VALUES list emits this block per tuple; rewind the allocator to
		// the first tuple's base so runSubOnce serves later tuples the cached value,
		// which is OP_Once. INSERT ... SELECT needs no rewind.
		if once, ok := c.upsertOnceSub[arm]; ok && site == returningSiteOnce {
			setSubFirst = once.base
			c.nSub = setSubFirst
			setRewound = true
		}
		setResetAddr = c.emit(Instruction{Op: OpSubCacheReset, P1: setSubFirst})
		setBlockStart = c.here()
	}
	var setErr error
	// Optional WHERE: a false/NULL result is a silent no-op (skip the row).
	// Inside the block, so its subqueries share the SET list's lifetimes.
	if up.where != nil {
		wReg, werr := c.compileExpr(up.where)
		if werr != nil {
			setErr = werr
		} else {
			*doUpdateSkips = append(*doUpdateSkips, c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1}))
		}
	}
	tmp := make([]int, len(up.sets))
	for i, a := range up.sets {
		if setErr != nil {
			break
		}
		r, werr := c.compileExpr(a.expr)
		if werr != nil {
			setErr = werr
			break
		}
		tmp[i] = r
	}
	c.pager, c.liveDB, c.liveRow = savedSetPager, savedSetLive, savedSetLiveRow
	c.popRegScope()
	c.popRegScope()
	if setErr != nil {
		return nil, setErr
	}
	if setResetAddr >= 0 {
		nPerRow, nOnce := upsertSetSubqueryLifetimes(c.insns[setBlockStart:])
		switch {
		case nPerRow == 0:
			// Nothing here names the proposed row, so every subquery keeps
			// C's OP_Once lifetime (expr.c:3889) and freezes at the first
			// conflicting row. The instruction stays -- its P1 still
			// documents the block's slot base -- and resets nothing, which
			// is emitReturning's own treatment of this half.
			c.insns[setResetAddr].P2 = 0
		case nOnce == 0:
			// A rewind that already happened cannot be undone, since this tuple's
			// sub-programs were compiled against it. Tuples of one VALUES list always
			// classify alike, but decline rather than assume.
			if setRewound {
				return nil, fmt.Errorf("%w: upsert SET subquery lifetime is not stable across VALUES tuples", errVDBEUnsupported)
			}
			c.insns[setResetAddr].P2 = c.nSub - setSubFirst
		default:
			// MIXED, and one contiguous reset cannot serve both lifetimes:
			// see upsertSetSubqueryLifetimes for the two wrong answers the
			// alternatives produce. Declined exactly as the same mix
			// declines in a RETURNING block.
			return nil, fmt.Errorf("%w: upsert SET mixes the two subquery lifetimes (%d per-row, %d once)",
				errVDBEUnsupported, nPerRow, nOnce)
		}
		if site == returningSiteOnce && nPerRow == 0 {
			once, ok := c.upsertOnceSub[arm]
			switch n := c.nSub - setSubFirst; {
			case !ok:
				if c.upsertOnceSub == nil {
					c.upsertOnceSub = map[int]onceSlots{}
				}
				c.upsertOnceSub[arm] = onceSlots{base: setSubFirst, count: n}
			case n != once.count:
				// A later tuple allocated a DIFFERENT number of slots than
				// the first, so the shared numbering does not line up and
				// some slot would be aliased to the wrong subquery.
				// Deterministic compilation makes this unreachable; it
				// declines rather than trusting that.
				return nil, fmt.Errorf("%w: upsert SET subquery slot count differs between VALUES tuples", errVDBEUnsupported)
			}
		}
		if c.nSub < setSubHighWater {
			c.nSub = setSubHighWater
		}
	}
	// The OLD row an UPDATE trigger's OLD.* reads, snapshotted before the
	// SET list overwrites existBase in place -- compileUpdateStmt takes the
	// identical copy for the identical reason. OLD.<ipk> needs no fixing up:
	// OpUpsertFind loaded existBase NORMALIZED, so its IPK slot already
	// holds the conflicting row's own rowid.
	var oldBase int
	if upBefore != nil || upAfter != nil {
		oldBase = c.allocN(len(tbl.cols))
		for i := range tbl.cols {
			c.emit(Instruction{Op: OpSCopy, P1: existBase + i, P2: oldBase + i})
		}
	}
	// A NOT NULL check is one of sqlite3GenerateConstraintChecks', which
	// update.c:1031 codes BELOW the BEFORE UPDATE fire -- so with a BEFORE
	// program it waits for that, with the CHECK block (see below).
	emitNotNull := func(idx int) {
		if tbl.cols[idx].NotNull {
			c.emit(Instruction{Op: OpHaltIfNull, P1: int(conflictAbort), P3: existBase + idx,
				P4: fmt.Sprintf("engine: INSERT into %s: NOT NULL constraint failed: %s.%s", stmt.table, tbl.name, tbl.cols[idx].Name)})
		}
	}
	for i, idx := range colIdx {
		c.emit(Instruction{Op: OpSCopy, P1: tmp[i], P2: existBase + idx})
		c.emit(Instruction{Op: OpAffinity, P1: existBase + idx, P4: tbl.cols[idx].Aff})
		if idx == tbl.ipkIndex {
			// The rowid-assigning SET. OpMustBeInt has no jump target (update.c:894), so a
			// non-integer is a hard "datatype mismatch" with no OR-clause escape (a DO
			// UPDATE runs under OE_Abort, upsert.c:325). It coerces the image's own slot,
			// as OP_MustBeInt does (vdbe.c:2120), so CHECK sees the integer, and
			// newRowidReg is copied from it. NULL never survives, so no NOT NULL check.
			c.emit(Instruction{Op: OpMustBeInt, P1: existBase + idx})
			c.emit(Instruction{Op: OpSCopy, P1: existBase + idx, P2: newRowidReg})
			continue
		}
		if upBefore == nil {
			emitNotNull(idx)
		}
	}
	// A generated column's stored value is the CONFLICTING row's, computed
	// from the values this DO UPDATE has just replaced -- so it is re-derived
	// before the CHECK block, the way the plain-insert path already does
	// (OpComputeGenerated below). Without it "b AS (a*2), CHECK(b<100)"
	// tested the OLD b and let "DO UPDATE SET a=500" through, where real
	// SQLite raises "CHECK constraint failed: b<100".
	if hasGeneratedCols(tbl.cols) {
		c.emit(Instruction{Op: OpComputeGenerated, P1: existBase, P4: tbl})
	}
	// STRICT type check on the re-stored row, as the UPDATE that C codes for DO
	// UPDATE does (upsert.c:325). The candidate row was already checked in
	// emitInsertRowBody.
	if tbl.strict {
		c.emit(Instruction{Op: OpTypeCheck, P1: existBase, P4: &typeCheckPlan{tbl: tbl, prefix: fmt.Sprintf("engine: INSERT into %s", stmt.table)}})
	}
	// DO UPDATE takes the UPDATE rule: aiChng is its SET list, and the IPK keeps
	// its value in the image (ipkReg -1). The CHECK scope's rowid is the NEW rowid
	// (update.c:1031, insert.c:2066, expr.c:5063). When the row does not move,
	// newRowidReg is existRowidReg (update.c:609).
	checksAndRecord := func() error {
		if cerr := c.emitCheckConstraints(db, tbl, existBase, newRowidReg, -1, fmt.Sprintf("engine: INSERT into %s", stmt.table), buildChangeSet(tbl, colIdx)); cerr != nil {
			return cerr
		}
		c.emit(Instruction{Op: OpMakeRecord, P1: existBase, P2: len(tbl.cols), P3: recReg})
		return nil
	}
	if upBefore == nil {
		if cerr := checksAndRecord(); cerr != nil {
			return nil, cerr
		}
	}
	// colIdx is passed so the FK pass knows the changed columns, as the UPDATE
	// behind DO UPDATE does (update.c:1049); an all-false mask would skip every
	// child-side key check. Order follows update.c: record, BEFORE, row-still-there
	// guard (update.c:990), store, AFTER (update.c:1117).
	uplanStore := &updatePlan{tbl: tbl, displayName: stmt.table, colIdx: colIdx,
		upsertBefore: upBefore, upsertAfter: upAfter}
	if upBefore != nil {
		bp := *upBefore
		*rowPlans = append(*rowPlans, &bp)
		// P2 = NEW rowid, P5 = OLD rowid: they differ only when the SET
		// reassigned the IPK, which is exactly when newRowidReg was split
		// off existRowidReg above.
		c.emit(Instruction{Op: OpFireTriggers, P1: existBase, P2: newRowidReg, P3: oldBase, P5: uint16(existRowidReg), P4: &bp})
	}
	if upBefore != nil || upAfter != nil {
		*doUpdateSkips = append(*doUpdateSkips, c.emit(Instruction{Op: OpSkipIfRowGone, P1: existRowidReg, P4: tbl}))
	}
	if upBefore != nil {
		// update.c:1000-1031 in order: reload every column this SET list
		// does NOT assign out of the live row (a BEFORE program may have
		// edited it -- see opUpsertReload), re-derive the generated columns
		// over that image, and only then the constraint checks and the
		// record, so all of them read the row the store will write.
		reload := &updatePlan{tbl: tbl, setCol: make([]bool, len(tbl.cols)), rowRegs: rowRegsFor(existBase, len(tbl.cols))}
		for _, idx := range colIdx {
			if idx >= 0 && idx < len(reload.setCol) {
				reload.setCol[idx] = true
			}
		}
		c.emit(Instruction{Op: OpUpsertReload, P1: existRowidReg, P4: reload})
		if hasGeneratedCols(tbl.cols) {
			c.emit(Instruction{Op: OpComputeGenerated, P1: existBase, P4: tbl})
		}
		for _, idx := range colIdx {
			if idx != tbl.ipkIndex {
				emitNotNull(idx)
			}
		}
		if cerr := checksAndRecord(); cerr != nil {
			return nil, cerr
		}
	}
	c.emit(Instruction{Op: OpUpsertStore, P1: recReg, P2: existRowidReg, P3: newRowidReg, P4: uplanStore})
	// RETURNING for the updated arm reports the updated row: DO UPDATE is an UPDATE,
	// whose RETURNING resolves into the NEW frame (resolve.c:534, 589), i.e.
	// existBase and newRowidReg. The INSERT's RETURNING trigger fires for the
	// UPDATE half of an upsert (trigger.c:855, 1499). Emitted before AFTER UPDATE
	// (trigger.c:68).
	if stmt.returning != nil {
		names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(existBase, len(tbl.cols)), newRowidReg, site.upsertArm(returningSiteOnceUpsertUpdate))
		if rerr != nil {
			return nil, rerr
		}
		colNames = names
	}
	if upAfter != nil {
		ap := *upAfter
		*rowPlans = append(*rowPlans, &ap)
		c.emit(Instruction{Op: OpFireTriggers, P1: existBase, P2: newRowidReg, P3: oldBase, P5: uint16(existRowidReg), P4: &ap})
	}
	return colNames, nil
}

// upsertSetsHoldSubquery reports whether any of an upsert DO UPDATE's SET
// right-hand sides carries a subquery -- the one condition under which the
// candidate row has to be snapshotted onto the machine (OpPseudoRow), since
// only a sub-Program's separate register file cannot read the pushed scope.
func upsertSetsHoldSubquery(sets []assignment) bool {
	for _, a := range sets {
		if containsSubquery(a.expr) {
			return true
		}
	}
	return false
}

// upsertSetSubqueryLifetimes classifies the DO UPDATE SET block's subqueries as
// returningSubqueryLifetimes does: one that names "excluded" is re-evaluated
// per conflicting row (resolve.c:846, 1402), otherwise OP_Once freezes it
// (expr.c:3889). An OpParam reading pseudo-row 2 is that reference.
//
// A mixed block declines: the reset is one contiguous slot range and cannot
// serve both lifetimes. Nested sub-programs are walked too.
func upsertSetSubqueryLifetimes(insns []Instruction) (nPerRow, nOnce int) {
	seen := map[*Program]bool{}
	var walk func([]Instruction)
	count := func(p *Program) {
		if p == nil || seen[p] {
			return
		}
		seen[p] = true
		if progReadsExcludedRow(p, map[*Program]bool{}) {
			nPerRow++
		} else {
			nOnce++
		}
		walk(p.Insns)
		if p.Compound != nil {
			for _, arm := range p.Compound.arms {
				if arm != nil {
					walk(arm.Insns)
				}
			}
		}
	}
	walk = func(in []Instruction) {
		for i := range in {
			switch in[i].Op {
			case OpSubquery, OpExists:
				if p, ok := in[i].P4.(*Program); ok {
					count(p)
				}
			case OpInSub:
				if p, ok := in[i].P4.(*inSubPlan); ok {
					count(p.prog)
				}
			case OpRowSub:
				if p, ok := in[i].P4.(*rowSubPlan); ok {
					count(p.prog)
				}
			}
		}
	}
	walk(insns)
	return nPerRow, nOnce
}

// progReadsExcludedRow reports whether prog, or anything it reaches, reads the
// upsert's proposed row (OpParam P1 == 2). Compound ARMS are walked because a
// compound Program carries no Insns of its own, and nested sub-programs because
// resolve.c:1403's nRef test chains outward through nesting -- a reference
// written two levels in still bumps the DO UPDATE's own NameContext.
func progReadsExcludedRow(prog *Program, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return false
	}
	seen[prog] = true
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			if progReadsExcludedRow(arm, seen) {
				return true
			}
		}
	}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if in.Op == OpParam && in.P1 == 2 {
			return true
		}
		switch p4 := in.P4.(type) {
		case *Program:
			if progReadsExcludedRow(p4, seen) {
				return true
			}
		case *inSubPlan:
			if progReadsExcludedRow(p4.prog, seen) {
				return true
			}
		case *rowSubPlan:
			if progReadsExcludedRow(p4.prog, seen) {
				return true
			}
		case *derivedSource:
			if progReadsExcludedRow(p4.prog, seen) {
				return true
			}
		}
	}
	return false
}

// rowRegsFor returns the per-column register slice base..base+n-1.
func rowRegsFor(base, n int) []int {
	r := make([]int, n)
	for i := range r {
		r[i] = base + i
	}
	return r
}

// emitReturning compiles a RETURNING clause against the affected row's
// registers (rowRegs by column, rowidReg for the rowid) and emits one
// OpResultRow. The IPK column maps to the rowid register. Returns the output
// column names. site says whether subqueries can be served here (see
// returningSite).
func (c *compiler) emitReturning(db *DB, tbl *tableMeta, ret []SelectColumn, rowRegs []int, rowidReg int, site returningSite) ([]string, error) {
	plan, perr := db.buildReturningPlan(tbl, ret)
	if perr != nil {
		// An unknown function in RETURNING is C's prepare-time "no such function"
		// (resolve.c:1290), a semantic error rather than a decline.
		return nil, declineOrSemantic(perr)
	}
	if plan.hasSubquery {
		if site == returningSiteNoSubquery {
			return nil, fmt.Errorf("%w: subquery in RETURNING", errVDBEUnsupported)
		}
		// Subqueries in RETURNING are fine with triggers: lifetimes come from
		// returningSubqueryLifetimes (trigger.c:979), a once-lifetime subquery is frozen
		// whatever writes what it reads, and RETURNING is emitted first among the AFTER
		// programs so a per-row subquery sees earlier rows' cascades only.
	}
	regs := append([]int(nil), rowRegs...)
	if tbl.ipkIndex >= 0 {
		regs[tbl.ipkIndex] = rowidReg
		c.rederiveGeneratedFromRowid(tbl, regs)
	}
	rs := tableScope{name: tbl.name, tableName: tbl.name, cols: tbl.cols, colIndex: buildColIndex(tbl.cols), noRowid: tbl.withoutRowid}
	c.pushRegScope(regScope{scope: &rs, regs: regs, rowidReg: rowidReg})
	defer c.popRegScope()

	// RETURNING subqueries compile live (compileLiveSubProgram) against the
	// database at run time. c.liveDB is set for this block only; the statement's
	// own WHERE/SET subqueries keep the pre-statement snapshot.
	if plan.hasSubquery {
		savedLive, savedPager := c.liveDB, c.pager
		c.liveDB = db
		defer func() { c.liveDB, c.pager = savedLive, savedPager }()
		// Snapshot the affected row onto the machine so live RETURNING sub-programs,
		// which have no enclosing compiler, can read it. C instead rewrites the
		// reference to a register (resolve.c:587). Only done when the list holds a
		// subquery.
		savedRet := c.retRow
		c.retRow = &retRowCompileCtx{cols: tbl.cols, tableName: tbl.name, noRowid: tbl.withoutRowid}
		defer func() { c.retRow = savedRet }()
		c.emit(Instruction{Op: OpPseudoRow, P1: 3, P2: rowidReg, P4: append([]int(nil), regs...)})
		if c.pager == nil {
			// A compile-time pager is still needed for schema facts (arity, affinity,
			// collation) the IN and row-value emitters ask for.
			sp, sperr := db.writeSubqueryPager()
			if sperr != nil {
				return nil, sperr
			}
			c.pager = sp
		}
	}
	// The cache slots this block is about to allocate, so a per-row block can
	// clear them at its own top. Emitted BEFORE the expressions and patched
	// after, because neither the count NOR the lifetime is known until they are
	// compiled -- see the returningSubqueryLifetimes switch below, which is
	// what decides whether the reset resets anything at all.
	subFirst := c.nSub
	// The allocator's high-water mark before any rewind below, so the rewind
	// can never hand a LATER emission a slot an earlier one already owns.
	subHighWater := c.nSub
	// An UNROLLED VALUES list emits this block once PER TUPLE, so a later tuple
	// would allocate fresh cache slots and re-run a subquery C runs ONCE. It
	// rewinds the allocator to the first tuple's base instead: identical slot
	// numbers mean runSubOnce's "if e.done" (vdbe.go) serves tuple 2 the value
	// tuple 1 computed, which IS OP_Once -- the cache check sits ABOVE
	// liveLower, so even a live-lowered sub-program is re-lowered at most once.
	// Undone below for a block that turns out to be all-per-row.
	rewound := false
	onceGroup, once := site.onceGroup()
	if plan.hasSubquery && once && c.returningOnceSubSet[onceGroup] {
		subFirst = c.returningOnceSubBase[onceGroup]
		c.nSub = subFirst
		rewound = true
	}
	// The rewind is emitted for EVERY subquery-bearing block, not just a per-row
	// one, and it starts inert (P2 = 0 clears nothing): the arms below fill in
	// either a slot COUNT (all per row) or a slot LIST (mixed). A once-site block
	// needs it too, and that is what makes a MIXED list compilable there -- the
	// rewind above already shares every slot across VALUES tuples, which is the
	// once half's whole mechanism, so all the per-row half needs is its own slots
	// cleared at the top of each tuple.
	resetAddr := -1
	if plan.hasSubquery {
		resetAddr = c.emit(Instruction{Op: OpSubCacheReset, P1: subFirst})
	}
	blockStart := c.here()
	base := c.allocN(len(plan.outCols))
	for i, oc := range plan.outCols {
		r, cerr := c.compileExpr(oc.expr)
		if cerr != nil {
			if plan.hasSubquery {
				// Decline, not a semantic error: a live sub-program has no enclosing compiler,
				// so a subquery correlated to the affected row fails "no such column", which C
				// answers via NC_UBaseReg (trigger.c:1074).
				return nil, fmt.Errorf("%w: subquery in RETURNING: %v", errVDBEUnsupported, cerr)
			}
			return nil, cerr
		}
		c.emit(Instruction{Op: OpSCopy, P1: r, P2: base + i})
	}
	allPerRow := true
	if plan.hasSubquery {
		nPerRow, nOnce, perRowSlots := returningSubqueryLifetimes(c.insns[blockStart:], tbl.name)
		if c.retForcePerRow && nPerRow == 0 {
			// The target is a view and every subquery names it. The program walk cannot see
			// that (a view opens its base tables), but C's test is the resolved FROM table
			// (trigger.c:979), so these are per-row. Set by emitViewReturning.
			nPerRow, nOnce = nOnce, 0
		}
		switch {
		case nOnce == 0:
			// Every subquery is one C re-evaluates per row. Fresh slots per
			// emission and, at a loop site, a reset at the block's own top.
		case nPerRow == 0:
			// Every subquery is run-once (OP_Once, expr.c:3889; EP_VarSelect only when its
			// FROM names the modified table, trigger.c:998). returningSitePerRow neuters
			// the per-row reset; returningSiteOnce rewinds slots so later tuples read the
			// cached value. A subquery over a view, derived table or other table is frozen
			// in C.
			if resetAddr >= 0 {
				// P2 = 0: the instruction stays (its P1 still documents the
				// block's slot base) and resets nothing, which is the whole
				// difference between the two lifetimes at this site.
				c.insns[resetAddr].P2 = 0
			}
			allPerRow = false
		default:
			// Mixed: per-row and once subqueries interleave, so OpSubCacheReset gets a slot
			// list instead of a range. Per-row slots are fresh each row and once slots keep
			// their value, as C does (trigger.c, vdbe.c:7581). Only reachable at
			// returningSitePerRow.
			if resetAddr < 0 {
				return nil, fmt.Errorf("%w: RETURNING mixes the two subquery lifetimes (%d per-row, %d once) with no rewind to split",
					errVDBEUnsupported, nPerRow, nOnce)
			}
			c.insns[resetAddr].P2 = 0
			c.insns[resetAddr].P4 = perRowSlots
			allPerRow = false
		}
	}
	// Only the per-row SITE resets a whole range: at the once site an all-per-row
	// block gets fresh slots by simply not rewinding (see the switch below), so
	// there is nothing to clear.
	if resetAddr >= 0 && allPerRow && site == returningSitePerRow {
		c.insns[resetAddr].P2 = c.nSub - subFirst
	}
	if plan.hasSubquery && once {
		switch {
		case allPerRow:
			// A rewind that already happened cannot be undone. Tuples of one VALUES list
			// always classify alike, but decline rather than assume.
			if rewound {
				return nil, fmt.Errorf("%w: RETURNING subquery lifetime is not stable across VALUES tuples", errVDBEUnsupported)
			}
		case !c.returningOnceSubSet[onceGroup]:
			c.returningOnceSubBase[onceGroup], c.returningOnceSubSet[onceGroup] = subFirst, true
			c.returningOnceSubCount[onceGroup] = c.nSub - subFirst
		case c.nSub-subFirst != c.returningOnceSubCount[onceGroup]:
			// A later tuple allocated a DIFFERENT number of slots than the
			// first, so the shared numbering does not line up and some slot
			// would be aliased to the wrong subquery. Deterministic compilation
			// makes this unreachable; it declines rather than trusting that.
			// Compared per group, not against the allocator's high-water mark:
			// an upsert's two arms interleave within every tuple, so a group's
			// rewound slots need not be the last ones allocated.
			return nil, fmt.Errorf("%w: RETURNING subquery slot count differs between VALUES tuples", errVDBEUnsupported)
		}
	}
	if c.nSub < subHighWater {
		c.nSub = subHighWater
	}
	c.emit(Instruction{Op: OpResultRow, P1: base, P2: len(plan.outCols)})
	return plan.names, nil
}

// returningSubqueryLifetimes classifies each immediate RETURNING subquery: per
// row (C re-evaluates it) or once (OP_Once, frozen for the statement).
//
// C's test, sqlite3ReturningSubqueryCorrelated (trigger.c:972), is shallow and
// by identity: a subquery is per-row only if an item in its own FROM resolves to
// the modified table. A derived table, a view, or another table over the
// modified data is still frozen.
//
// The question is asked of the emitted program: a real table compiles to an
// OpOpenRead in the subquery's own program, while views, CTEs, derived tables
// and compounds open theirs one level down, which matches C's pSTab test without
// re-deriving name shadowing. Nested subqueries in a per-row body need no check;
// the body is re-run per row.
func returningSubqueryLifetimes(insns []Instruction, table string) (nPerRow, nOnce int, perRowSlots []int) {
	var perRow func(prog *Program) bool
	seen := map[*Program]bool{}
	perRow = func(prog *Program) bool {
		if prog == nil || seen[prog] {
			return false
		}
		seen[prog] = true
		// A COMPOUND Program has no Insns of its own -- its arms ARE the frames
		// (Program.Compound) -- so a walk of Insns alone answers "nothing here"
		// for a UNION and files a subquery naming the affected row as run-once.
		// C's nRef test does not care how the SELECT is spelled: resolving
		// "(SELECT a UNION SELECT 9)" inside a RETURNING list bumps the
		// enclosing NameContext's nRef exactly as the plain arm does
		// (resolve.c:1403-1404), so the arms have to be walked. Frozen once and
		// re-read per row are different answers, so this is not a nicety.
		if prog.Compound != nil {
			for _, arm := range prog.Compound.arms {
				if perRow(arm) {
					return true
				}
			}
		}
		for i := range prog.Insns {
			in := &prog.Insns[i]
			// The second way C sets EP_VarSelect: resolving the subquery bumped the
			// enclosing nRef (resolve.c:1403), i.e. it names the affected row, so it is
			// per-row even over another table. An OpParam reading pseudo-row 3 is that
			// reference (emitReturningParam).
			if in.Op == OpParam && in.P1 == 3 {
				return true
			}
			// P3 is the db index (dbIndexOf, cross_db.go): 0 is this database.
			// An ATTACHed table of the same name is a different table, exactly
			// as it is a different pSTab in C.
			// A table flattenSubquery moved up from a view or derived body is
			// not in the subquery's own FROM list as C resolved it -- the view
			// is (P5 bit 2, emitJoinLoops) -- so it is not C's pSTab match.
			if in.Op == OpOpenRead && in.P3 == 0 && in.P5&2 == 0 {
				if rt, ok := in.P4.(*resolvedTable); ok && equalFoldName(rt.name, table) {
					return true
				}
			}
			// The nRef test is not shallow the way sqlite3ReturningSubqueryCorrelated
			// is: a reference written inside a FURTHER-nested subquery still
			// chains back to the RETURNING NameContext, so it marks this
			// subquery too. Recursed for that half alone -- the OpOpenRead half
			// stays shallow above, matching C's pSTab-identity walk over the
			// subquery's OWN FROM list.
			switch p4 := in.P4.(type) {
			case *Program:
				if perRowNested(p4, seen) {
					return true
				}
			case *inSubPlan:
				if perRowNested(p4.prog, seen) {
					return true
				}
			case *rowSubPlan:
				if perRowNested(p4.prog, seen) {
					return true
				}
			case *derivedSource:
				if perRowNested(p4.prog, seen) {
					return true
				}
			}
		}
		return false
	}
	count := func(prog *Program, slot int) {
		if perRow(prog) {
			nPerRow++
			perRowSlots = append(perRowSlots, slot)
		} else {
			nOnce++
		}
	}
	// The cache SLOT is the opcode's own operand: P2 for OpSubquery/OpExists,
	// P3 for OpInSub/OpRowSub (see their arms in vdbe.go). It is collected here
	// so the MIXED case can reset exactly the per-row slots -- they are not a
	// contiguous range, because the allocator hands slots out in the order the
	// expression list compiles.
	for i := range insns {
		switch p4 := insns[i].P4.(type) {
		case *Program:
			count(p4, insns[i].P2)
		case *inSubPlan:
			count(p4.prog, insns[i].P3)
		case *rowSubPlan:
			count(p4.prog, insns[i].P3)
		}
	}
	return nPerRow, nOnce, perRowSlots
}

// perRowNested reports whether prog, or anything it reaches, reads the
// RETURNING clause's affected row -- resolve.c:1403's nRef test, which reaches
// through nesting. See returningSubqueryLifetimes, whose own OpOpenRead half is
// deliberately NOT recursed.
func perRowNested(prog *Program, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return false
	}
	seen[prog] = true
	// The arms of a compound, for the reason returningSubqueryLifetimes' own
	// perRow states: a compound Program carries no Insns at all.
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			if perRowNested(arm, seen) {
				return true
			}
		}
	}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if in.Op == OpParam && in.P1 == 3 {
			return true
		}
		switch p4 := in.P4.(type) {
		case *Program:
			if perRowNested(p4, seen) {
				return true
			}
		case *inSubPlan:
			if perRowNested(p4.prog, seen) {
				return true
			}
		case *rowSubPlan:
			if perRowNested(p4.prog, seen) {
				return true
			}
		case *derivedSource:
			if perRowNested(p4.prog, seen) {
				return true
			}
		}
	}
	return false
}

// returningSite says what an emitReturning call site offers a RETURNING
// subquery. C gives it one of two lifetimes: per row when its FROM names the
// modified table (trigger.c:972-1013, EP_VarSelect), otherwise OP_Once at the
// first emission (expr.c:3889). See returningSubqueryLifetimes.
type returningSite int

const (
	// returningSiteNoSubquery: a subquery here declines. It is the zero value so a
	// new call site that names none declines instead of claiming a lifetime.
	returningSiteNoSubquery returningSite = iota

	// returningSiteOnce: the block is emitted once per affected row -- an
	// unrolled INSERT ... VALUES tuple -- so each has its own cache slots and
	// nothing needs clearing.
	returningSiteOnce

	// returningSitePerRow: ONE emission executed once per row (a scan loop's
	// body: UPDATE, INSERT ... SELECT). Its slots are shared across rows, so
	// the block clears them at its own top.
	returningSitePerRow

	// returningSiteOnceUpsertUpdate / returningSiteOnceUpsertInsert are
	// returningSiteOnce for an upsert's two arms -- the DO UPDATE arm
	// sqlite3UpsertDoUpdate codes (upsert.c:325-326, update.c:1117-1119) and the
	// arm that really inserted (insert.c:1604-1608). Each is shared across an
	// unrolled VALUES list's tuples, as returningSiteOnce is, and kept apart
	// from the other, as C's two OP_Once wrappers are.
	returningSiteOnceUpsertUpdate
	returningSiteOnceUpsertInsert
)

// returningOnceGroups is how many independent once groups a statement can
// hold: a plain INSERT's, and an upsert's two arms.
const returningOnceGroups = 3

// onceGroup reports whether a site freezes a once-lifetime subquery by sharing
// cache slots across an unrolled VALUES list's tuples, and which shared base
// it uses.
func (s returningSite) onceGroup() (int, bool) {
	switch s {
	case returningSiteOnce:
		return 0, true
	case returningSiteOnceUpsertUpdate:
		return 1, true
	case returningSiteOnceUpsertInsert:
		return 2, true
	}
	return 0, false
}

// upsertArm maps the upsert tail's own site onto the site one of its arms
// emits RETURNING at: its own once group at an unrolled VALUES site, and the
// same per-row site otherwise, where each arm's emission already allocates
// slots of its own.
func (s returningSite) upsertArm(arm returningSite) returningSite {
	if s == returningSiteOnce {
		return arm
	}
	return s
}

// rederiveGeneratedFromRowid re-derives tbl's generated columns for the
// RETURNING image from a copy whose IPK slot holds the decided rowid, and
// repoints regs' generated entries at the copy. regs[tbl.ipkIndex] must already
// be the rowid register.
//
// C computes generated columns after the rowid (insert.c:1545, update.c:975),
// and an IPK reference reads the rowid register (resolve.c:466, expr.c:5063)
// because the IPK slot holds NULL (insert.c:1367). This emitter decides the
// rowid after OpComputeGenerated, so without this RETURNING reported NULL for a
// column like "g AS (id*2)". Re-deriving is safe: generated expressions are
// deterministic and row-local, and C itself computes twice around a BEFORE
// trigger (update.c:1022). The copy keeps the substituted rowid out of the
// stored record.
func (c *compiler) rederiveGeneratedFromRowid(tbl *tableMeta, regs []int) {
	if !hasGeneratedCols(tbl.cols) {
		return
	}
	row := c.allocN(len(tbl.cols))
	for i := range tbl.cols {
		c.emit(Instruction{Op: OpSCopy, P1: regs[i], P2: row + i})
	}
	c.emit(Instruction{Op: OpComputeGenerated, P1: row, P4: tbl})
	for i := range tbl.cols {
		if tbl.cols[i].IsGenerated() {
			regs[i] = row + i
		}
	}
}

// emitCheckConstraints emits tbl's CHECK constraints against the row image in
// registers rowBase.. (and rowidReg for a rowid reference): each expression is
// compiled with the register-backed row scope (compileColumn's rowRegs), then
// an OpIf jumps over an OpHaltError carrying the constraint's own message. A
// NULL result passes, matching C SQLite. prefix is the statement-kind error
// prefix ("engine: INSERT into t"/"engine: UPDATE t").
func (c *compiler) emitCheckConstraints(db *DB, tbl *tableMeta, rowBase, rowidReg, ipkReg int, prefix string, chng *checkChangeSet) error {
	skips, err := c.emitCheckConstraintsAction(db, tbl, rowBase, rowidReg, ipkReg, prefix, conflictAbort, false, chng)
	if len(skips) != 0 {
		panic("emitCheckConstraints: unexpected skip under ABORT")
	}
	return err
}

// emitCheckConstraintsAction emits tbl's CHECK constraints. Under OR IGNORE a
// failing CHECK skips the row (the jump address is returned for the caller to
// patch); otherwise it halts with the constraint's error. With
// ignore_check_constraints ON it emits nothing. db is a parameter, not a
// compiler field, so no call site can forget it.
//
// chng is the UPDATE rule: nil for INSERT, else the assigned columns (see
// checkChangeSet). ipkReg is the register an IPK column reference reads, or -1
// for the image slot: an auto-assigned or SET rowid leaves that slot NULL, while
// in C the IPK and the rowid are one register.
func (c *compiler) emitCheckConstraintsAction(db *DB, tbl *tableMeta, rowBase, rowidReg, ipkReg int, prefix string, action conflictAction, explicitOr bool, chng *checkChangeSet) ([]int, error) {
	if len(tbl.checks) == 0 || db.IgnoreCheckConstraints() {
		return nil, nil
	}
	rowRegs := make([]int, len(tbl.cols))
	for i := range tbl.cols {
		rowRegs[i] = rowBase + i
	}
	if tbl.ipkIndex >= 0 && ipkReg >= 0 {
		rowRegs[tbl.ipkIndex] = ipkReg
	}
	rs := tableScope{name: tbl.name, cols: tbl.cols, colIndex: buildColIndex(tbl.cols), noRowid: tbl.withoutRowid}
	c.pushRegScope(regScope{scope: &rs, regs: rowRegs, rowidReg: rowidReg})
	defer c.popRegScope()
	ignore := explicitOr && action == conflictIgnore
	var skips []int
	for _, cc := range tbl.checks {
		if !chng.evaluates(cc.expr) {
			continue
		}
		// CHECK bodies compile under C's NC_IsCheck, which silently drops a database
		// qualifier (resolve.c:313), and under OP_PureFunc, so a clock-reading
		// date/time function is an error (vdbeaux.c:5643). Both are restored after,
		// since only CHECK bodies get them.
		savedIgnore, savedPure := c.ignoreDbQualifier, c.pureFuncP5
		c.ignoreDbQualifier, c.pureFuncP5 = true, pureCtxCheck
		reg, cerr := c.compileExpr(cc.expr)
		c.ignoreDbQualifier, c.pureFuncP5 = savedIgnore, savedPure
		if cerr != nil {
			return nil, cerr
		}
		okJump := c.emit(Instruction{Op: OpIf, P1: reg, P3: 1}) // TRUE or NULL passes
		if ignore {
			skips = append(skips, c.emit(Instruction{Op: OpGoto})) // fail under IGNORE: skip row
		} else {
			name := cc.name
			if name == "" {
				name = cc.exprText
			}
			ha := conflictAbort
			if explicitOr {
				ha = action
			}
			c.emit(Instruction{Op: OpHaltError, P1: int(ha), P4: fmt.Sprintf("%s: CHECK constraint failed: %s", prefix, name)})
		}
		c.patch(okJump, c.here())
	}
	return skips, nil
}

// insertSlotMap resolves, at compile time, which VALUES expression (by index)
// feeds each column of tbl: slotOf[col] is that index, or -1 when the column
// was not named. rowidSlot is the expression index naming the rowid of a table
// with NO INTEGER PRIMARY KEY column (noColumnRowidTarget), or -1.
//
// It reproduces buildFullRow's two SQLite quirks exactly: a column named twice
// takes its FIRST value, while the rowid slot takes its LAST.
func insertSlotMap(tbl *tableMeta, stmt *insertStmt, colIdx []int) (slotOf []int, rowidSlot int, err error) {
	nVals := 0
	if len(stmt.rows) > 0 {
		nVals = len(stmt.rows[0])
	}
	return insertSlotMapN(tbl, stmt, colIdx, nVals)
}

// insertSlotMapN is insertSlotMap for a source whose value count is known
// independently of stmt.rows (an INSERT ... SELECT's output columns).
func insertSlotMapN(tbl *tableMeta, stmt *insertStmt, colIdx []int, nVals int) (slotOf []int, rowidSlot int, err error) {
	slotOf = make([]int, len(tbl.cols))
	for i := range slotOf {
		slotOf[i] = -1
	}
	rowidSlot = -1
	// Every VALUES tuple must have the same arity; the statement is rejected as
	// a whole if one differs (C SQLite rejects it at prepare time too).
	// A column-list-less INSERT supplies a value for every NON-generated
	// column only (C SQLite counts generated columns out entirely), and
	// each generated slot is filled by computeGeneratedInto instead.
	nInsertable := 0
	for _, c := range tbl.cols {
		if !c.IsGenerated() {
			nInsertable++
		}
	}
	want := nInsertable
	if stmt.cols != nil {
		want = len(colIdx)
	}
	// C's arity messages differ: no column list is "table %S has %d columns but %d
	// values were supplied" (insert.c:1250), a column list is "%d values for %d
	// columns" (insert.c:1257). First, rows of differing width are "all VALUES must
	// have the same number of terms" (select.c:3078).
	if len(stmt.rows) > 1 {
		for _, row := range stmt.rows[1:] {
			if len(row) != len(stmt.rows[0]) {
				return nil, -1, fmt.Errorf("engine: all VALUES must have the same number of terms")
			}
		}
	}
	for _, row := range stmt.rows {
		if len(row) != want {
			if stmt.cols == nil {
				return nil, -1, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, nInsertable, len(row))
			}
			return nil, -1, fmt.Errorf("engine: %d values for %d columns", len(row), len(colIdx))
		}
	}
	if stmt.cols == nil {
		if nVals != nInsertable {
			return nil, -1, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, nInsertable, nVals)
		}
		k := 0
		for i, c := range tbl.cols {
			if c.IsGenerated() {
				continue // no VALUES slot; computed instead
			}
			slotOf[i] = k
			k++
		}
		return slotOf, -1, nil
	}
	if nVals != len(colIdx) {
		return nil, -1, fmt.Errorf("engine: %d columns named but %d values supplied", len(colIdx), nVals)
	}
	for i, idx := range colIdx {
		switch {
		case idx == noColumnRowidTarget:
			rowidSlot = i // last-wins
		case tbl.ipkIndex >= 0 && idx == tbl.ipkIndex:
			slotOf[idx] = i // rowid slot: last-wins
		case slotOf[idx] < 0:
			slotOf[idx] = i // every other column: first-wins
		}
	}
	return slotOf, rowidSlot, nil
}

// compileDeleteWrite compiles "DELETE FROM t [WHERE expr]" into a row-store
// scan (OpOpenWrite/OpRewind/OpNext) whose body evaluates WHERE (reusing
// compileExpr against the table's single scope) and, for each matching row,
// records its rowid (OpDeleteRow); an OpDeleteFlush then removes them all. A
// subquery in WHERE, a nonexistent column, multi-table DELETE, or RETURNING
// all surface as compile failures (compileExpr's own errVDBEUnsupported, or
// parseDeleteStmt's rejection) and fall back.
func (db *DB) compileDeleteWrite(sqlText string) (*Program, error) {
	stmt, err := parseDeleteStmt(sqlText)
	if err != nil {
		return nil, err
	}
	return db.compileDeleteStmt(stmt, nil)
}

func (db *DB) compileDeleteStmt(stmt *deleteStmt, trig *trigCompileCtx) (*Program, error) {
	// See compileInsertStmt's identical prologue for the sqlite3SrcListLookup
	// citations: the qualifier picks a catalog (delete.c:345 resolves DELETE's
	// own target through it), so resolve scoped rather than decline.
	if err := db.checkWriteSchemaQualifier(stmt.schema); err != nil {
		return nil, err
	}
	scope := writeTargetScope(stmt.schema)
	// See compileInsertStmt's identical fork: a direct catalog write has its
	// own emitter (vdbe_schema_write.go).
	if writableSchemaTarget(stmt.schema, stmt.table) {
		return db.compileSchemaCatalogDelete(stmt, trig)
	}
	tbl := db.findTableMetaIn(scope, stmt.table)
	if tbl == nil {
		// DELETE on a view: C materializes it and fires INSTEAD OF triggers without
		// opening or deleting rows (delete.c:353, 428, 592, 809). See
		// compileViewDeleteStmt (vdbe_view_write.go).
		if vm := db.findViewMetaIn(scope, stmt.table); vm != nil {
			return db.compileViewDeleteStmt(stmt, vm, trig)
		}
		// A DELETE whose target is a VIRTUAL TABLE. delete.c:526's
		// sqlite3WhereBegin scans the vtab itself, delete.c:552/582 collect the
		// matching rowids, and delete.c:626/644 replay them into OP_VUpdate --
		// which is what compileVtabDeleteStmt emits; see vdbe_vtab_write.go.
		// Anything it does not model declines from there, landing on deleteVtab
		// exactly as before.
		if vt := db.findVtabMetaIn(scope, stmt.table); vt != nil {
			return db.compileVtabDeleteStmt(stmt, vt, trig)
		}
		// No table, view or vtab: a semantic error, raised at prepare time as C does
		// (sqlite3SrcListLookup: insert.c:963, delete.c:428, update.c:362).
		//
		// A trigger body is the exception: its target may resolve at fire time in
		// another session's catalog (attachedOriginatingFireInsert, attach_write.go),
		// so a body keeps the decline.
		if trig != nil {
			return nil, fmt.Errorf("%w: no such table: %s", errVDBEUnsupported, stmt.table)
		}
		return nil, semanticf("engine: no such table: %s", stmt.table)
	}
	// See table_load.go's package doc comment.
	if err := db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	var firePlan, beforePlan *triggerFirePlan
	if db.tableHasTriggers(tbl.name, tbl.isTemp, triggerDelete) {
		// RETURNING is a trigger on the same list (trigger.c:68), so it needs only the
		// right order; see the emitReturning call below OpDelete. Nested triggers: see
		// compileInsertStmt.
		if !db.triggersCompilable(tbl, triggerDelete) {
			return nil, fmt.Errorf("%w: DELETE trigger shape not lowered", errVDBEUnsupported)
		}
		memo := trigMemo(trig) // see compileInsertStmt's identical note
		bp, berr := db.compileTriggerFirePlan(tbl, triggerDelete, triggerBefore, memo)
		if berr != nil {
			return nil, berr
		}
		ap, aerr := db.compileTriggerFirePlan(tbl, triggerDelete, triggerAfter, memo)
		if aerr != nil {
			return nil, aerr
		}
		beforePlan, firePlan = bp, ap
	}
	// INDEXED BY is validated at compile time, as C does (delete.c:31, 345); "no
	// such index" is an error. The plan is unchanged: a full scan, which
	// where.c:4232 makes safe.
	if ierr := db.checkWriteIndexHint(stmt.indexedBy, tbl); ierr != nil {
		return nil, ierr
	}
	// ...and a partial index the WHERE does not imply leaves the scan no loop
	// (where_plan_indexed_by.go) -- unless C truncates instead of scanning:
	// delete.c:471-474 takes OP_Clear with no WHERE, no trigger (RETURNING is
	// one, trigger.c:68-78) and no sqlite3FkRequired, which for a DELETE is
	// "foreign_keys on, and the table a child or a parent of any foreign key"
	// ("bHaveFK = (sqlite3FkReferences(pTab) || pTab->u.tab.pFKey)", fkey.c:1158).
	// The engine's own reasons for its row-by-row path below are not C's.
	fkRequired := db.fkEnforce && (len(db.fkOf(tbl)) > 0 || len(db.fkNamingParent(tbl)) > 0)
	if stmt.where != nil || beforePlan != nil || firePlan != nil || stmt.returning != nil || fkRequired {
		if nerr := db.writeIndexedByNoSolution(tbl, stmt.schema, stmt.alias, stmt.indexedBy, stmt.where, trig, false); nerr != nil {
			return nil, nerr
		}
	}
	// Truncate optimisation (delete.c:460): OP_Clear instead of a scan when there is
	// no WHERE, trigger (RETURNING counts as one), complex FK or vtab. Decided after
	// INDEXED BY is validated, as in C (delete.c:345 vs 471). captureChanges and
	// fkEnforce keep the row path (one event per row; a stand-in for
	// sqlite3FkRequired), as does a trigger body, whose undo is per row. WITHOUT
	// ROWID tables qualify (delete.c:487).
	if stmt.where == nil && beforePlan == nil && firePlan == nil && stmt.returning == nil &&
		trig == nil && !db.fkEnforce && !db.captureChanges {
		c := newWriteScanCompilerAs(tbl, stmt.alias)
		c.ownSchema = db.targetSchemaName(tbl)
		initAddr := c.emit(Instruction{Op: OpInit})
		c.patch(initAddr, 1)
		c.emit(Instruction{Op: OpClearTable, P4: tbl})
		c.emit(Instruction{Op: OpHalt})
		return &Program{Insns: c.insns, NReg: c.nReg, NCursors: c.nCursor, NSubCache: c.nSub}, nil
	}
	c := newWriteScanCompilerAs(tbl, stmt.alias)
	c.ownSchema = db.targetSchemaName(tbl)
	c.trig = trig
	// nQueryLoop 0 outside the loop; the WHERE span, and for an UPDATE the
	// one-pass kind, say what it is inside (whereSpan, update.c:804).
	c.nQueryLoopKnown = true
	var writePager *ReadOnlyPager
	freshPager := false
	if containsSubquery(stmt.where) {
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, perr
		}
		c.pager = sp
		if trig != nil && len(stmt.ctes) == 0 {
			// Trigger body: reads bind at fire time (vdbe_live_read.go). sp stays the
			// compile-time pager for schema facts, but no frozen snapshot reaches the
			// Program. A leading WITH keeps the frozen route (see compileInsertStmt).
			c.liveDB = db
		} else {
			writePager = sp
			freshPager = trig == nil && len(stmt.ctes) == 0
			// WITH CTEs are visible to this statement's WHERE subqueries, for the length
			// of the compile; re-entered expressions carry Program.CTEScopeSnapshot.
			if len(stmt.ctes) > 0 {
				defer sp.pushCTEScope(stmt.ctes)()
			}
		}
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenWrite, P1: 0, P4: tbl})
	// A rowid point lookup, when the WHERE pins one: the scan fetches that row
	// instead of materializing the table. Pure candidate-set restriction -- the
	// conjunct below still runs on the fetched row. See write_rowid_seek.go.
	if !emitWriteRowidSeekHint(c, tbl, 0, stmt.where) {
		emitWriteIndexSeekHint(c, tbl, 0, stmt.where)
	}
	rewind := c.emit(Instruction{Op: OpRewind, P1: 0})
	loopTop := c.here()

	whereJump := -1
	if stmt.where != nil {
		// stmt.where is this DELETE's own top-level WHERE clause -- delete.c:526
		// feeds it to sqlite3WhereBegin exactly as a SELECT's WHERE is, so it is
		// the same WhereClause tag-20220128a's "pWC->op==TK_AND" guard sees. See
		// compiler.inWhereConjunct's doc comment.
		c.inWhereConjunct = true
		// ...and it is coded INSIDE the scan loop, cursor 0 standing on the
		// candidate row, so a CORRELATED subquery beneath it may read that
		// cursor as its live outer source. See vdbe_write_rowlive.go for the
		// same delete.c:526 citation read the other way round.
		undoRowLive := c.whereSpan()
		wReg, werr := c.compileExpr(stmt.where)
		undoRowLive()
		if werr != nil {
			return nil, werr
		}
		whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1}) // false/NULL -> skip to OpNext
	}
	// DELETE RETURNING reads the pre-delete row, captured here while the cursor is
	// on it and emitted at the AFTER-trigger position below. With triggers,
	// RETURNING reads C's OLD block instead (see retBase below).
	retBase, retRowid := -1, -1
	if stmt.returning != nil && beforePlan == nil && firePlan == nil {
		retBase = c.allocN(len(tbl.cols))
		for i := range tbl.cols {
			c.emit(Instruction{Op: OpColumn, P1: 0, P2: i, P3: retBase + i})
		}
		retRowid = c.allocN(1)
		c.emit(Instruction{Op: OpRowid, P1: 0, P2: retRowid})
	}
	var oldBase, oldRowidReg int
	var rowPlans []*triggerFirePlan
	goneJump := -1
	if beforePlan != nil || firePlan != nil {
		// A trigger for an earlier row may have deleted this one; C skips the whole row
		// here (delete.c:768). Emitted only with triggers, C's ONEPASS_OFF case
		// (delete.c:358, 498).
		goneJump = c.emit(Instruction{Op: OpNotExists, P1: 0, P4: tbl})
		oldBase = c.allocN(len(tbl.cols))
		for i := range tbl.cols {
			c.emit(Instruction{Op: OpColumn, P1: 0, P2: i, P3: oldBase + i})
		}
		oldRowidReg = c.allocN(1)
		c.emit(Instruction{Op: OpRowid, P1: 0, P2: oldRowidReg})
		// RETURNING reads the OLD block, as C's RETURNING pseudo-trigger shares iOld
		// with the AFTER call. A BEFORE program that edited the row therefore reports
		// pre-edit values.
		retBase, retRowid = oldBase, oldRowidReg
	}
	goneJump2 := -1
	if beforePlan != nil {
		bp := *beforePlan
		rowPlans = append(rowPlans, &bp)
		c.emit(Instruction{Op: OpFireTriggers, P1: -1, P2: oldRowidReg, P3: oldBase, P4: &bp})
		// delete.c:817 re-seeks after BEFORE triggers ran, since they may have moved
		// or deleted the row. OLD.* is not re-populated after it, as in C.
		goneJump2 = c.emit(Instruction{Op: OpNotExists, P1: 0, P4: tbl})
	}
	c.emit(Instruction{Op: OpDelete, P1: 0, P4: tbl})
	// RETURNING, at the AFTER-trigger position and FIRST among the AFTER
	// triggers. Both halves of that are C's, and neither is where this emitter
	// used to put it. See emitReturning's own note and returningSiteNoSubquery.
	var colNames []string
	if stmt.returning != nil {
		names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(retBase, len(tbl.cols)), retRowid, returningSitePerRow)
		if rerr != nil {
			return nil, rerr
		}
		colNames = names
	}
	if firePlan != nil {
		ap := *firePlan
		rowPlans = append(rowPlans, &ap)
		c.emit(Instruction{Op: OpFireTriggers, P1: -1, P2: oldRowidReg, P3: oldBase, P4: &ap})
	}
	for _, rp := range rowPlans {
		rp.skipAddr = c.here() // BEFORE RAISE(IGNORE) abandons this delete -> row end
	}
	nextAddr := c.here()
	if whereJump >= 0 {
		c.patch(whereJump, nextAddr)
	}
	if goneJump >= 0 {
		// delete.c's iLabel, resolved at the END of sqlite3GenerateRowDelete:
		// the SAME place a BEFORE RAISE(IGNORE) lands, which is why it is
		// patched alongside them rather than to the OpNext.
		c.patch(goneJump, nextAddr)
	}
	if goneJump2 >= 0 {
		c.patch(goneJump2, nextAddr) // the same iLabel -- delete.c:823
	}
	c.emit(Instruction{Op: OpNext, P1: 0, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpHalt})

	return &Program{Insns: c.insns, NReg: c.nReg, NCursors: c.nCursor, NSubCache: c.nSub, WritePager: writePager, FreshWritePager: freshPager, ColNames: colNames}, nil
}

// compileUpdateWrite compiles UPDATE into a row-store scan: WHERE, then each
// SET right-hand side against the OLD values, packed into a record applied in
// rowid order with per-row constraint checks by the shared helpers.
func (db *DB) compileUpdateWrite(sqlText string) (*Program, error) {
	stmt, err := parseUpdateStmt(sqlText)
	if err != nil {
		return nil, err
	}
	return db.compileUpdateStmt(stmt, nil)
}

func (db *DB) compileUpdateStmt(stmt *updateStmt, trig *trigCompileCtx) (*Program, error) {
	// See compileInsertStmt's identical prologue (update.c:362 is UPDATE's own
	// sqlite3SrcListLookup call).
	if err := db.checkWriteSchemaQualifier(stmt.schema); err != nil {
		return nil, err
	}
	scope := writeTargetScope(stmt.schema)
	// See compileInsertStmt's identical fork: a direct catalog write has its
	// own emitter (vdbe_schema_write.go).
	if writableSchemaTarget(stmt.schema, stmt.table) {
		return db.compileSchemaCatalogUpdate(stmt, trig)
	}
	tbl := db.findTableMetaIn(scope, stmt.table)
	if tbl == nil {
		// UPDATE on a view: C materializes it and fires INSTEAD OF triggers, skipping
		// the store (update.c:371, 630, 1028). See compileViewUpdateStmt. A view with
		// FROM is a different algorithm (update.c:629), refused there.
		if vm := db.findViewMetaIn(scope, stmt.table); vm != nil {
			return db.compileViewUpdateStmt(stmt, vm, trig)
		}
		// UPDATE on a virtual table: updateVirtualTable (update.c:649), emitted by
		// compileVtabUpdateStmt (vdbe_vtab_write.go). Only without FROM
		// (update.c:1230).
		if vt := db.findVtabMetaIn(scope, stmt.table); vt != nil {
			return db.compileVtabUpdateStmt(stmt, vt, trig)
		}
		// No table, view or vtab: a semantic error, raised at prepare time as C does
		// (sqlite3SrcListLookup: insert.c:963, delete.c:428, update.c:362).
		//
		// A trigger body is the exception: its target may resolve at fire time in
		// another session's catalog (attachedOriginatingFireInsert, attach_write.go),
		// so a body keeps the decline.
		if trig != nil {
			return nil, fmt.Errorf("%w: no such table: %s", errVDBEUnsupported, stmt.table)
		}
		return nil, semanticf("engine: no such table: %s", stmt.table)
	}
	// See table_load.go's package doc comment.
	if err := db.ensureTableLoaded(tbl); err != nil {
		return nil, err
	}
	// nChangeFrom, SQLite's own name for it: "nChangeFrom = (pTabList->nSrc>1)
	// ? pChanges->nExpr : 0;" (update.c:395). It moves this ONE function's pass
	// one onto a synthetic join SELECT whose rows land in an ephemeral table
	// keyed by the target row, and leaves pass two -- everything below the loop
	// driver -- alone. The three seams it opens here are the loop driver, where
	// the SET values come from, and the fact that the WHERE clause was consumed
	// by pass one. See vdbe_update_from.go for the port and its C citations.
	upfrom := stmt.from != nil
	// The ON CONFLICT algorithm this UPDATE runs under: an explicit OR-clause
	// of ANY kind (including "OR ABORT", which overrides every declared
	// default uniformly -- effectiveHitAction, conflict.go), or a constraint
	// on the table carrying its own declared default. See updatePlan.
	conflictAware := stmt.explicitOr || db.tableHasDeclaredConflict(tbl)
	// The SET clause's column NAMES, in its own order -- SQLite's pChanges,
	// the argument sqlite3CodeRowTrigger threads through to
	// checkColumnOverlap for the "UPDATE OF <col-list>" gate (trigger.c:1502).
	// See compileUpdateTriggerFirePlan.
	setCols := make([]string, len(stmt.sets))
	for i, a := range stmt.sets {
		setCols[i] = a.col
	}
	var firePlan, beforePlan *triggerFirePlan
	if db.tableHasTriggers(tbl.name, tbl.isTemp, triggerUpdate) {
		// Nested triggers: see compileInsertStmt. The memo key includes orconf, as
		// getRowTrigger's does (trigger.c:1376). RETURNING: see compileDeleteStmt.
		//
		// An IGNORE resolution jumps to labelContinue (update.c:1031, 1128), past
		// RETURNING and AFTER. BEFORE UPDATE fires before constraint checks
		// (update.c:978); emitUpdateNotNull, emitCheckConstraintsAction and the
		// OpMakeRecord run after the fire. STRICT OpTypeCheck sits above the fire with a
		// BEFORE program (update.c:981), below it otherwise (insert.c:2080).
		// filterUpdateTriggers applies the OF-list overlap (trigger.c:1502).
		if !db.triggersCompilable(tbl, triggerUpdate) {
			return nil, fmt.Errorf("%w: UPDATE trigger shape not lowered", errVDBEUnsupported)
		}
		memo := trigMemo(trig) // see compileInsertStmt's identical note
		// Trigger programs run under the firing statement's explicit OR clause, else
		// each body keeps its own (trigger.c:1137). A declared constraint default is
		// not passed down.
		bp, berr := db.compileUpdateTriggerFirePlan(tbl, triggerBefore, memo, stmt.orAction, stmt.explicitOr, setCols)
		if berr != nil {
			return nil, berr
		}
		ap, aerr := db.compileUpdateTriggerFirePlan(tbl, triggerAfter, memo, stmt.orAction, stmt.explicitOr, setCols)
		if aerr != nil {
			return nil, aerr
		}
		beforePlan, firePlan = bp, ap
	}
	colIdx := make([]int, len(stmt.sets))
	// selfRead[i] marks a SET expression holding a subquery that READS THE
	// TARGET TABLE. It is only half the decline test -- the other half is the
	// compiled sub-program, which does not exist until the SET loop far below
	// emits it. See emitSetValue.
	selfRead := make([]bool, len(stmt.sets))
	// hasSubquery drives the frozen snapshot this loop's OWN subqueries read.
	// An UPDATE ... FROM compiles neither its WHERE nor its SET expressions
	// here -- both belong to pass one (update.c:949-955 reads SET expression j
	// back as "OP_Column iEph, nOff+j" instead of coding it) -- so it needs no
	// such pager, and the SET-subquery decline just below cannot apply to it
	// either: pass one runs over the untouched table by construction, which is
	// exactly the pre-update image that decline exists to guarantee.
	hasSubquery := !upfrom && containsSubquery(stmt.where)
	for i, a := range stmt.sets {
		// A SET target may be the rowid pseudo-column, which moves the row
		// (update.c:494). WITHOUT ROWID tables have none.
		idx, ok := resolveColumnOrRowidTarget(tbl, a.col)
		if !ok {
			// update.c:494's own error, so semanticf: as a DECLINE it reached
			// the caller as "unsupported: no such column: nosuch", the real
			// diagnosis wearing a wrapper C has no counterpart for.
			return nil, semanticf("engine: no such column: %s", a.col)
		}
		// C SQLite: "cannot UPDATE generated column". A generated column's
		// value is always derived from the row, never assigned.
		if idx >= 0 && idx < len(tbl.cols) && tbl.cols[idx].IsGenerated() {
			return nil, fmt.Errorf("engine: cannot UPDATE generated column %q", tbl.cols[idx].Name)
		}
		colIdx[i] = idx
		if !upfrom && containsSubquery(a.expr) {
			hasSubquery = true
			// A SET subquery that SCANS THE TABLE BEING UPDATED is the one shape
			// this collect-then-apply loop cannot always serve from its single
			// pre-update snapshot. Recorded here, ASKED BELOW: the decline needs
			// the compiled sub-program, not the AST (see emitSetValue's own
			// comment, and the two reverts it records).
			selfRead[i] = setSubqueryReadsTable(a.expr, tbl.name)
		}
	}

	// Conflict-resolving UPDATE re-seeks the real table per collected key
	// (update.c:847, 868, 875), so a row an earlier iteration's REPLACE deleted and
	// re-created is updated again off its new contents. "UPDATE OR REPLACE t SET
	// k=k+1" over keys 1,2 leaves one row. The re-seek is emitted for every
	// conflict-resolving UPDATE, as C emits one loop head for ONEPASS_OFF.
	if ierr := db.checkWriteIndexHint(stmt.indexedBy, tbl); ierr != nil {
		return nil, ierr
	}
	// update.c codes every UPDATE's scan with sqlite3WhereBegin, a WHERE-less
	// one included (where_plan_indexed_by.go). An UPDATE ... FROM plans the
	// target inside a join this does not build, so a partial hint declines.
	if nerr := db.writeIndexedByNoSolution(tbl, stmt.schema, stmt.alias, stmt.indexedBy, stmt.where, trig, upfrom); nerr != nil {
		return nil, nerr
	}
	// Pass one, compiled: "SELECT <target row key>, <SET exprs> FROM <srclist>
	// WHERE ..." into the SRT_Upfrom ephemeral table (update.c:693-696).
	// Compiled AFTER every cheap decline above, so an UPDATE ... FROM this
	// emitter would refuse anyway never pays for a sub-program compile.
	var upfromSrc *derivedSource
	var upfromPager *ReadOnlyPager
	if upfrom {
		var uerr error
		if upfromSrc, upfromPager, uerr = db.compileUpdateFromSource(stmt, tbl, trig); uerr != nil {
			return nil, uerr
		}
	}
	c := newWriteScanCompilerAs(tbl, stmt.alias)
	c.ownSchema = db.targetSchemaName(tbl)
	c.trig = trig
	// nQueryLoop 0 outside the loop; the WHERE span, and for an UPDATE the
	// one-pass kind, say what it is inside (whereSpan, update.c:804).
	c.nQueryLoopKnown = true
	var writePager *ReadOnlyPager
	freshPager := false
	if hasSubquery {
		sp, perr := db.writeSubqueryPager()
		if perr != nil {
			return nil, perr
		}
		c.pager = sp
		if trig != nil && len(stmt.ctes) == 0 {
			// Trigger body: reads bind at fire time (vdbe_live_read.go). sp stays the
			// compile-time pager for schema facts, but no frozen snapshot reaches the
			// Program. A leading WITH keeps the frozen route (see compileInsertStmt).
			c.liveDB = db
		} else {
			writePager = sp
			freshPager = trig == nil && len(stmt.ctes) == 0
			// See compileDeleteStmt's identical push: a leading WITH clause's
			// CTEs reach this statement's WHERE and SET subqueries, for this
			// compile.
			if len(stmt.ctes) > 0 {
				defer sp.pushCTEScope(stmt.ctes)()
			}
		}
	}
	// With recursive_triggers=ON, a REPLACE victim's delete fires DELETE triggers
	// for UPDATE as for INSERT (update.c:1031, insert.c:2220). The per-row re-seek
	// (scanReseek) handles victims the cascade deleted or kept. Only compiled when
	// conflictAware.
	var delBefore, delAfter *triggerFirePlan
	if conflictAware {
		var derr error
		delBefore, delAfter, derr = db.compileReplaceVictimDeletePlans(tbl, stmt.table, trigMemo(trig))
		if derr != nil {
			return nil, derr
		}
	}
	plan := &updatePlan{tbl: tbl, displayName: stmt.table, colIdx: colIdx,
		conflictAware: conflictAware, action: stmt.orAction, explicitOr: stmt.explicitOr,
		replaceDelBefore: delBefore, replaceDelAfter: delAfter}
	plan.setCol = make([]bool, len(tbl.cols))
	for _, idx := range colIdx {
		if idx >= 0 && idx < len(plan.setCol) {
			plan.setCol[idx] = true
		}
	}

	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenWrite, P1: 0, P4: tbl})

	// scanCur is what OpRewind/OpNext drive. For an ordinary UPDATE that is the
	// target table itself; for an UPDATE ... FROM it is pass one's ephemeral
	// table (update.c:849's "OP_Rewind iEph, labelBreak"), and the target
	// cursor is instead SEEKED per entry, just below.
	scanCur := 0
	upfromRowid := -1
	upfromGone := -1
	if upfrom {
		scanCur = c.allocCursor()
		c.emit(Instruction{Op: OpOpenDerived, P1: scanCur, P2: c.allocSub(), P4: upfromSrc})
	} else {
		// A rowid point lookup, when the WHERE pins one. Only for the ordinary
		// UPDATE: with a FROM clause the scan drives pass one's EPHEMERAL table,
		// whose rowids are its own and have nothing to do with the target's.
		if !emitWriteRowidSeekHint(c, tbl, scanCur, stmt.where) {
			emitWriteIndexSeekHint(c, tbl, scanCur, stmt.where)
		}
	}
	// THE ORDER THIS LOOP VISITS ROWS IN, when a SET subquery may depend on it
	// (a per-row lowering reads the rows before it -- see the liveRow block
	// below). Two-pass, C walks a rowid-keyed ephemeral table (update.c:776),
	// which is this loop's own order; so is a one-pass scan no index can drive.
	// Otherwise it is whatever where.c chose for the WHERE, which the ported
	// planner answers (updateOnePassOrder) and OpAutoIndexOrder installs.
	hasIndex := false
	for _, idx := range db.indexes {
		if indexBelongsTo(idx, tbl) && idx != tbl.pkIndex {
			hasIndex = true
			break
		}
	}
	twoPass := beforePlan != nil || firePlan != nil || containsSubquery(stmt.where)
	onePassRowid := stmt.indexedBy == "" && !db.pragmaState.ReverseUnorderedSelects() && (stmt.where == nil || !hasIndex || stmt.notIndexed)
	orderLive := !upfrom && (twoPass || onePassRowid)
	setSub := !upfrom && setsContainSubquery(stmt.sets)
	if upfrom {
		c.nQueryLoopKnown = false // UPDATE ... FROM's join is not planned here
	}
	if setSub || !upfrom && returningContainsSubquery(stmt.returning) {
		neverOnePass := twoPass || db.updateNeverOnePass(tbl, stmt, colIdx)
		var key *autoIndexKey
		var nRow logEst
		oneRow, ok := false, false
		if !neverOnePass {
			key, nRow, oneRow, ok = db.updateOnePassOrder(tbl, stmt, trig)
		}
		forced := key != nil && updateKeyForcesTwoPass(key, colIdx, len(tbl.cols))
		if setSub && !orderLive {
			if neverOnePass || db.updateEveryIndexForcesTwoPass(tbl, stmt, colIdx) {
				orderLive = true // two passes, or no plan that is not rowid order
			} else if ok {
				orderLive = true
				if key != nil && !forced {
					c.emit(Instruction{Op: OpAutoIndexOrder, P1: scanCur, P4: key})
				}
			}
		}
		switch {
		case ok && !forced && !oneRow:
			// ONEPASS_MULTI: sqlite3WhereBegin has added the loop's nRowOut to
			// pParse->nQueryLoop (where.c:7198) and no sqlite3WhereEnd has put
			// it back before a SET value or the RETURNING list is coded
			// (update.c:804), so every subquery there is planned as run that
			// many times -- which is what can make its plan an automatic index.
			c.nQueryLoop += nRow
		case ok || neverOnePass:
			// ONEPASS_OFF and ONEPASS_SINGLE code them after that
			// sqlite3WhereEnd, at the enclosing nQueryLoop.
		default:
			c.nQueryLoopKnown = false // no plan to say which
		}
	}
	rewind := c.emit(Instruction{Op: OpRewind, P1: scanCur})
	loopTop := c.here()
	if upfrom {
		// update.c:861: seek the target by the eph entry's key (collapseUpfromRows),
		// positioning the cursor and dropping a row a cascade deleted. P5 because the
		// cursor has no position of its own; see OpNotExists.
		upfromRowid = c.allocN(1)
		c.emit(Instruction{Op: OpRowid, P1: scanCur, P2: upfromRowid})
		upfromGone = c.emit(Instruction{Op: OpNotExists, P1: 0, P3: upfromRowid, P4: tbl, P5: 1})
	}

	// !upfrom: an UPDATE ... FROM's WHERE was consumed by pass one -- it is the
	// synthetic join SELECT's own WHERE (update.c:222-223 dups it into it), and
	// update.c:642's "if( nChangeFrom==0 && sqlite3ResolveExprNames(&sNC,
	// pWhere) )" is where the C stops resolving it against the target at all.
	whereJump := -1
	if stmt.where != nil && !upfrom {
		// stmt.where is this UPDATE's own top-level WHERE clause -- update.c:742/
		// 1273 feeds it to sqlite3WhereBegin exactly as a SELECT's WHERE is, so it
		// is the same WhereClause tag-20220128a's "pWC->op==TK_AND" guard sees.
		// See compiler.inWhereConjunct's doc comment.
		c.inWhereConjunct = true
		// ...and, like a SELECT's, it is coded inside the scan loop with cursor
		// 0 standing on the candidate row, so a CORRELATED subquery beneath it
		// may read that cursor. See vdbe_write_rowlive.go.
		undoRowLive := c.whereSpan()
		wReg, werr := c.compileExpr(stmt.where)
		undoRowLive()
		if werr != nil {
			return nil, werr
		}
		whereJump = c.emit(Instruction{Op: OpIfNot, P1: wReg, P3: 1})
	}

	// Re-seek the real table before reading the row (update.c:875), so OLD.* and
	// unassigned columns (update.c:906, 961) see a row a cascade rewrote, and a
	// deleted row drops out. Emitted when the statement's writes can change
	// unvisited rows: triggers or a conflict clause (update.c:733's ONEPASS_OFF).
	// Skipped for UPDATE ... FROM, whose OpNotExists already does it.
	//
	// Placed after the WHERE: C evaluates WHERE in pass one over the untouched
	// table, which the frozen snapshot models.
	scanReseek := -1
	if (beforePlan != nil || firePlan != nil || conflictAware) && !upfrom {
		scanReseek = c.emit(Instruction{Op: OpNotExists, P1: 0, P4: tbl})
	}

	// The new row image: every column read from the cursor's OLD row, then the
	// SET targets overwritten with their evaluated expressions. SQLite builds
	// the same register block before OP_MakeRecord/OP_Insert.
	rowBase := c.allocN(len(tbl.cols))
	for i := range tbl.cols {
		c.emit(Instruction{Op: OpColumn, P1: 0, P2: i, P3: rowBase + i})
	}
	rowidReg := c.allocN(1)
	if upfrom {
		// The eph entry's key IS this row's rowid, already in a register and
		// already used to seek the cursor. COPIED rather than re-read, because
		// an IPK SET below overwrites rowidReg with the NEW rowid while
		// OpUpdateRow still recovers the OLD one from the seeked cursor.
		c.emit(Instruction{Op: OpSCopy, P1: upfromRowid, P2: rowidReg})
	} else {
		c.emit(Instruction{Op: OpRowid, P1: 0, P2: rowidReg})
	}

	// Snapshot the OLD row (pre-SET) for an AFTER UPDATE trigger's OLD.*.
	var oldBase, oldRowidReg int
	if beforePlan != nil || firePlan != nil {
		oldBase = c.allocN(len(tbl.cols))
		for i := range tbl.cols {
			c.emit(Instruction{Op: OpSCopy, P1: rowBase + i, P2: oldBase + i})
		}
		oldRowidReg = c.allocN(1)
		c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: oldRowidReg})
	}

	// SET expressions read the OLD row, so "SET a=b, b=a" swaps. A correlated SET
	// subquery may read cursor 0 as its live outer source (update.c:954; see
	// vdbe_write_rowlive.go).
	//
	// A row-dependent SET subquery over the target is lowered per row, so it depends
	// on visit order. This loop walks ascending rowid, which matches C's two-pass
	// form (update.c:733, 776) and its one-pass form only when no index can drive
	// the scan (where.c:4240). Other cases decline.
	//
	// written is every table the loop can write (triggers, FK actions). A SET
	// subquery over any of them is lowered per row too, since C evaluates SET
	// before that row's BEFORE triggers and after the previous row's AFTER ones.
	// It does not widen twoPass, which must stay C's condition.
	written := db.tablesALoopCanWrite(tbl)
	// WITHOUT ROWID qualifies too: the loop walks PK order, as C's pass two and its
	// PK b-tree do. In a trigger body the per-row lowering is the body's own live
	// seam (liveBodyRowSubSelect), given the same condition as bodySetLive.
	var liveRow *liveRowCtx
	if orderLive && trig == nil {
		liveRow = &liveRowCtx{tbl: tbl, alias: stmt.alias, ownSchema: c.ownSchema, ctes: stmt.ctes,
			nQueryLoop: c.nQueryLoop, nQueryLoopKnown: c.nQueryLoopKnown}
	}
	savedBodySetLive := c.bodySetLive
	c.bodySetLive = orderLive && trig != nil
	defer func() { c.bodySetLive = savedBodySetLive }()
	undoSetRowLive := c.rowLiveSpan()
	setReg := make([]int, len(stmt.sets))
	for i, a := range stmt.sets {
		if upfrom {
			// update.c:949-955: with nChangeFrom set, SET expression j is not
			// coded here at all -- it is column nOff+j of the ephemeral table,
			// evaluated in the JOIN's scope during pass one. nOff is nPk, which
			// is 0 for a rowid table, and collapseUpfromRows drops the key
			// column(s) from the stored tuple for a WITHOUT ROWID target too
			// (select.c:1368-1369 does the same), so the offset is 0 either way.
			r := c.allocN(1)
			c.emit(Instruction{Op: OpColumn, P1: scanCur, P2: i, P3: r})
			setReg[i] = r
			continue
		}
		r, werr := c.emitSetValue(a.expr, selfRead[i], liveRow, tbl.name, written)
		if werr != nil {
			// No undo on this path: c is local to this compile and a returned
			// error discards it whole, so there is nothing left to restore for.
			return nil, werr
		}
		setReg[i] = r
	}
	undoSetRowLive()
	c.bodySetLive = savedBodySetLive
	// skips collects every "this row's constraint violation resolves to IGNORE,
	// skip it" jump, patched below to labelContinue -- SQLite's ignoreDest for
	// an UPDATE (update.c:1032). Only a conflict-resolving UPDATE can produce
	// one: with no OR-clause and no declared default every action below is
	// ABORT, which halts instead of jumping.
	var skips []int
	// Apply each SET: a column takes the value with affinity; the rowid slot (IPK
	// or rowid alias) moves the row and must be an integer. OP_MustBeInt has no
	// jump target (update.c:894), so NULL or non-integral is "datatype mismatch"
	// even under OR IGNORE.
	for i, idx := range colIdx {
		switch {
		case idx == noColumnRowidTarget:
			c.emit(Instruction{Op: OpSCopy, P1: setReg[i], P2: rowidReg})
			c.emit(Instruction{Op: OpMustBeInt, P1: rowidReg})
		case idx == tbl.ipkIndex:
			c.emit(Instruction{Op: OpSCopy, P1: setReg[i], P2: rowidReg})
			c.emit(Instruction{Op: OpAffinity, P1: rowidReg, P4: tbl.cols[idx].Aff})
			c.emit(Instruction{Op: OpMustBeInt, P1: rowidReg})
			c.emit(Instruction{Op: OpNull, P2: rowBase + idx}) // stored NULL; the rowid carries the value
		default:
			c.emit(Instruction{Op: OpSCopy, P1: setReg[i], P2: rowBase + idx})
			c.emit(Instruction{Op: OpAffinity, P1: rowBase + idx, P4: tbl.cols[idx].Aff})
		}
	}
	// NOT NULL checks are emitted below the BEFORE fire (emitUpdateNotNull).
	// Re-derive generated columns from the NEW values before CHECK, so a CHECK over
	// a generated column sees the new value.
	if hasGeneratedCols(tbl.cols) {
		c.emit(Instruction{Op: OpComputeGenerated, P1: rowBase, P4: tbl})
	}
	// STRICT type check over the full post-SET row, after OpComputeGenerated, so a
	// generated column's new value is checked (C type-checks a VIRTUAL generated
	// column where it computes it, expr.c:4410). With a BEFORE program it runs
	// before the fire (sqlite3TableAffinity, update.c:981; insert.c:179), ahead of
	// NOT NULL; without one, inside the constraint checks after NOT NULL
	// (insert.c:2080). The order is observable in which error is reported.
	if tbl.strict && beforePlan != nil {
		c.emit(Instruction{Op: OpTypeCheck, P1: rowBase, P4: &typeCheckPlan{tbl: tbl, prefix: fmt.Sprintf("engine: UPDATE %s", stmt.table)}})
	}
	// NEW.<ipk> = the new rowid, so a trigger reading it sees the value the
	// rowid register carries rather than the OpNull the SET loop left in the
	// row slot. AHEAD of OpMakeRecord now, where it used to sit behind it --
	// which is only possible because the record's own IPK slot is inert:
	// opUpdateRow overwrites full[ipkIndex] with the new rowid before
	// computeGeneratedInto and nulls it again after, which is update.c:942's
	// "sqlite3VdbeAddOp2(v, OP_Null, 0, k)" done one layer down.
	if (beforePlan != nil || firePlan != nil) && tbl.ipkIndex >= 0 {
		c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: rowBase + tbl.ipkIndex})
	}
	var rowPlans []*triggerFirePlan
	rowGoneJump := -1
	if beforePlan != nil {
		bp := *beforePlan
		rowPlans = append(rowPlans, &bp)
		// OpFireTriggers P2 = NEW rowid, P5 = OLD rowid (differ only on an IPK SET).
		//
		//	/* Fire any BEFORE UPDATE triggers. This happens before constraints
		//	** are verified. One could argue that this is wrong. */
		//	                                              -- update.c:979-980
		c.emit(Instruction{Op: OpFireTriggers, P1: rowBase, P2: rowidReg, P3: oldBase, P5: uint16(oldRowidReg), P4: &bp})
		// update.c:981-1019: with a BEFORE program, re-seek the row (OP_NotExists,
		// update.c:997) and reload every column the SET does not assign, then
		// recompute generated columns, because the trigger may have changed them
		// (trigger1-18.0). A seek, not OpSkipIfRowGone: it refreshes the cursor the
		// OpColumns below read, so CHECK sees the trigger's writes. opUpdateRow's
		// setCol merge (ticket e25d9ea771) still makes the store right.
		rowGoneJump = c.emit(Instruction{Op: OpNotExists, P1: 0, P4: tbl})
		assigned := make([]bool, len(tbl.cols))
		for _, idx := range colIdx {
			if idx >= 0 && idx < len(assigned) {
				assigned[idx] = true
			}
		}
		for i := range tbl.cols {
			if assigned[i] || i == tbl.ipkIndex || tbl.cols[i].IsGenerated() {
				continue
			}
			c.emit(Instruction{Op: OpColumn, P1: 0, P2: i, P3: rowBase + i})
		}
		if hasGeneratedCols(tbl.cols) {
			c.emit(Instruction{Op: OpComputeGenerated, P1: rowBase, P4: tbl})
		}
	} else if firePlan != nil {
		// The row may be gone: an AFTER trigger for an earlier row may have deleted it
		// (update.c:990). Skip the store, AFTER triggers and RETURNING; the loop scans a
		// materialized list and would otherwise resurrect it. A presence test is enough
		// here; only a BEFORE program can change this row since the top re-seek, and
		// that case is handled above.
		rowGoneJump = c.emit(Instruction{Op: OpSkipIfRowGone, P1: oldRowidReg, P4: tbl})
	}
	// Constraint checks run below the BEFORE fire and row-gone guard, at
	// update.c:1030 (update.c:978: "This happens before constraints are
	// verified"). The order is observable under IGNORE and FAIL. OpMakeRecord stays
	// below the NOT NULL ON CONFLICT REPLACE substitution, which writes rowBase.
	if nerr := c.emitUpdateNotNull(tbl, stmt, colIdx, rowBase, &skips); nerr != nil {
		return nil, nerr
	}
	// The STRICT datatype check, when no BEFORE program moved it above the fire
	// -- insert.c:2080's "sqlite3TableAffinity(v, pTab, regNewData+1)" under
	// bAffinityDone, between the NOT NULL loop and the CHECK loop. See the
	// conditional emission above for the oracle measurement.
	if tbl.strict && beforePlan == nil {
		c.emit(Instruction{Op: OpTypeCheck, P1: rowBase, P4: &typeCheckPlan{tbl: tbl, prefix: fmt.Sprintf("engine: UPDATE %s", stmt.table)}})
	}
	checkSkips, cerr := c.emitCheckConstraintsAction(db, tbl, rowBase, rowidReg, rowidReg, fmt.Sprintf("engine: UPDATE %s", stmt.table), stmt.orAction, stmt.explicitOr, buildChangeSet(tbl, colIdx))
	if cerr != nil {
		return nil, cerr
	}
	skips = append(skips, checkSkips...)
	recReg := c.allocRec()
	c.emit(Instruction{Op: OpMakeRecord, P1: rowBase, P2: len(tbl.cols), P3: recReg})
	c.emit(Instruction{Op: OpUpdateRow, P1: 0, P2: recReg, P3: rowidReg, P4: plan})
	// RETURNING FIRST among the AFTER programs -- see compileInsertStmt's row
	// tail for the C (trigger.c:68-78 prepends the synthetic trigger) and for
	// the 3.53.3 measurement. The UPDATE spelling of it: with "CREATE TRIGGER
	// t1au AFTER UPDATE ON t1 BEGIN SELECT RAISE(IGNORE); END" over three rows,
	// "UPDATE t1 SET b='Z' RETURNING a,b" answers all three on the oracle;
	// emitted after the fire it would answer none.
	var colNames []string
	if stmt.returning != nil {
		names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(rowBase, len(tbl.cols)), rowidReg, returningSitePerRow)
		if rerr != nil {
			return nil, rerr
		}
		colNames = names
	}
	if firePlan != nil {
		ap := *firePlan
		rowPlans = append(rowPlans, &ap)
		c.emit(Instruction{Op: OpFireTriggers, P1: rowBase, P2: rowidReg, P3: oldBase, P5: uint16(oldRowidReg), P4: &ap})
	}

	// labelContinue -- update.c:1128 "sqlite3VdbeResolveLabel(v,
	// labelContinue)", which sits AFTER the row counter and the AFTER trigger
	// programs (and so after the RETURNING row SQLite codes as one of them).
	// Every "skip this row" jump this loop can make lands here.
	nextAddr := c.here()
	// A BEFORE RAISE(IGNORE) lands on labelContinue, past RETURNING
	// (update.c:985, 1128), so an ignored row returns nothing.
	for _, rp := range rowPlans {
		rp.skipAddr = nextAddr
	}
	if whereJump >= 0 {
		c.patch(whereJump, nextAddr)
	}
	if scanReseek >= 0 {
		c.patch(scanReseek, nextAddr) // update.c:877's labelContinue
	}
	if rowGoneJump >= 0 {
		c.patch(rowGoneJump, nextAddr)
	}
	if upfromGone >= 0 {
		c.patch(upfromGone, nextAddr) // update.c:863's labelContinue
	}
	for _, a := range skips {
		c.patch(a, nextAddr)
	}
	plan.skipAddr = nextAddr // OpUpdateRow's own OE_Ignore jump
	c.emit(Instruction{Op: OpNext, P1: scanCur, P2: loopTop})
	c.patch(rewind, c.here())
	c.emit(Instruction{Op: OpHalt})

	if upfrom {
		// Pass one reads a FROZEN snapshot (compileUpdateFromSource), so this
		// program must never be reused by a later execution -- see
		// compileInsertSelectWrite's identical declaration and the analyze6/
		// count.test regression it records. cachedWriteProgram refuses to cache
		// a program whose WritePager is set.
		writePager = upfromPager
	}
	return &Program{Insns: c.insns, NReg: c.nReg, NCursors: c.nCursor, NRecRegs: c.nRec, NSubCache: c.nSub, WritePager: writePager, FreshWritePager: freshPager, ColNames: colNames}, nil
}

// compileDdlWrite compiles a CREATE TABLE / CREATE [UNIQUE] INDEX / DROP
// INDEX / DROP TABLE / DROP VIEW statement into a single delegating OpDdl
// (plus OpInit/OpHalt), so DDL is routed through the VDBE dispatcher even
// though its opcode body simply calls the existing
// db.CreateTable/CreateIndex/DropIndex/DropTable/DropView.
func (db *DB) compileDdlWrite(kind ddlKind, sqlText string) (*Program, error) {
	c := &compiler{}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpDdl, P4: &ddlPlan{kind: kind, sql: sqlText}})
	c.emit(Instruction{Op: OpHalt})
	return &Program{Insns: c.insns, NReg: c.nReg}, nil
}

// newWriteScanCompilerAs is newWriteScanCompiler with the statement's alias
// (parseWriteTargetSuffix), which replaces the table name in scope. "" means
// none.
//
// noRowid: a WITHOUT ROWID table has no rowid pseudo-column, so "rowid" fails
// "no such column" at name resolution (resolve.c:628, build.c:2731) instead of
// binding the row store's internal key.
func newWriteScanCompilerAs(tbl *tableMeta, alias string) *compiler {
	scope := compileScope{
		tableScope: tableScope{name: writeScopeName(alias, tbl), cols: tbl.cols, colIndex: buildColIndex(tbl.cols), offset: 0, noRowid: tbl.withoutRowid},
		cursor:     0,
	}
	return &compiler{scopes: []compileScope{scope}, nCursor: 1, writeTbl: tbl, writeAlias: alias}
}

// emitSetValue compiles one SET right-hand side. When selfRead says it holds a
// subquery over the table being updated and the emitted program is
// row-dependent, it declines, or re-emits live when liveRow is non-nil.
//
// expr.c:3889: an uncorrelated subquery runs once under OP_Once, over the
// pre-update image, which the frozen snapshot answers. A correlated one
// (EP_VarSelect, resolve.c:1403) re-runs per row against the partially updated
// table. The question is asked of the compiled program, after the emit; an AST
// predicate missed correlation inside FILTER, OVER and table-function args.
//
// selfRead is only the name check; a view or CTE over the target is caught by
// asking the program (programReadsTable). written widens the check to every
// table the row loop can write (tablesALoopCanWrite).
func (c *compiler) emitSetValue(e Expr, selfRead bool, liveRow *liveRowCtx, target string, written []string) (int, error) {
	mark := c.here()
	r, err := c.compileExpr(e)
	if err != nil {
		return 0, err
	}
	for _, name := range append([]string{target}, written...) {
		if selfRead {
			break
		}
		selfRead = programReadsTable(&Program{Insns: c.insns[mark:]}, name)
	}
	if !selfRead || !insnsHoldRowDependentSubquery(c.insns[mark:]) {
		return r, nil
	}
	// A trigger body's SET was lowered live by its own seam already
	// (liveBodyRowSubSelect); in a loop that visits rows in C's order that IS
	// the per-row answer.
	if liveRow == nil && c.liveDB != nil && c.bodySetLive && insnsRowDependenceIsLive(c.insns[mark:]) {
		return r, nil
	}
	if liveRow != nil {
		c.insns = c.insns[:mark]
		c.liveRow = liveRow
		r, err = c.compileExpr(e)
		c.liveRow = nil
		if err != nil {
			return 0, err
		}
		if insnsRowDependenceIsLive(c.insns[mark:]) {
			return r, nil
		}
	}
	return 0, fmt.Errorf("%w: UPDATE SET subquery over the target table or a table its triggers write", errVDBEUnsupported)
}

// tablesALoopCanWrite names every table a write to tbl's rows can go on to
// write while its row loop runs: tbl itself, the target of every statement in
// a trigger on it -- of any event, since the body's own writes fire more -- and
// the child of every foreign key whose action rewrites rows, followed to a
// fixed point. By NAME and over-approximated on purpose: naming a table the
// loop does not write only costs a per-row lowering (or a decline where no
// per-row lowering is available), while missing one is a wrong answer.
func (db *DB) tablesALoopCanWrite(tbl *tableMeta) []string {
	seen := map[string]bool{strings.ToLower(tbl.name): true}
	out := []string{tbl.name}
	add := func(name string) {
		if k := strings.ToLower(name); name != "" && !seen[k] {
			seen[k] = true
			out = append(out, name)
		}
	}
	for i := 0; i < len(out); i++ {
		name := out[i]
		for _, trs := range [][]*triggerMeta{db.triggers, db.foreignTempTriggers} {
			for _, tr := range trs {
				if !equalFoldName(tr.table, name) {
					continue
				}
				for _, b := range tr.body {
					switch {
					case b.insert != nil:
						add(b.insert.table)
					case b.update != nil:
						add(b.update.table)
					case b.delete != nil:
						add(b.delete.table)
					}
				}
			}
		}
		if t := db.findTableMeta(name); t != nil && db.fkEnforce {
			for _, fk := range db.fkNamingParent(t) {
				if fk.child != nil && (fkActionPerformsDML(fk.onUpdate) || fkActionPerformsDML(fk.onDelete)) {
					add(fk.child.name)
				}
			}
		}
	}
	return out
}

// insnsRowDependenceIsLive reports whether every sub-program in insns whose
// value can change from row to row is re-lowered for each row. A live
// correlated body is (execWithParent). A live UNCORRELATED one runs once per
// statement (runSubOnce), which is right unless its value still depends on the
// row -- a table-valued function argument naming it (programNeedsRowContext) --
// and a frozen one answers from the pre-update image.
func insnsRowDependenceIsLive(insns []Instruction) bool {
	for i := range insns {
		var p *Program
		switch p4 := insns[i].P4.(type) {
		case *Program:
			p = p4
		case *inSubPlan:
			p = p4.prog
		case *rowSubPlan:
			p = p4.prog
		}
		if p == nil || p.LiveSource == nil {
			if insnsHoldRowDependentSubquery(insns[i : i+1]) {
				return false
			}
			continue
		}
		if !p.Correlated && programNeedsRowContext(p) {
			return false
		}
	}
	return true
}

// insnsHoldRowDependentSubquery reports whether the emitted instructions carry a
// sub-program whose value can differ per row: Program.Correlated (C's
// EP_VarSelect) or programNeedsRowContext (a vtab source with run-time
// arguments). The recursive walk over-approximates, which only declines.
func insnsHoldRowDependentSubquery(insns []Instruction) bool {
	return insnsHoldRowDependentSubquerySeen(insns, map[*Program]bool{})
}

func insnsHoldRowDependentSubquerySeen(insns []Instruction, seen map[*Program]bool) bool {
	// Keep the payload list in step with vdbe_live_read.go's
	// programNeedsRowContextSeen -- the two walks visit the same P4 types, and
	// a new one that carries a *Program must be added to both.
	var sub func(p *Program) bool
	sub = func(p *Program) bool {
		if p == nil || seen[p] {
			return false
		}
		seen[p] = true
		if p.Correlated || programNeedsRowContext(p) {
			return true
		}
		return insnsHoldRowDependentSubquerySeen(p.Insns, seen)
	}
	for i := range insns {
		switch p4 := insns[i].P4.(type) {
		case *Program:
			if sub(p4) {
				return true
			}
		case *inSubPlan:
			if p4.correlated || sub(p4.prog) {
				return true
			}
		case *rowSubPlan:
			if p4.correlated || sub(p4.prog) {
				return true
			}
		case *derivedSource:
			if p4.vtab != nil || sub(p4.prog) {
				return true
			}
		}
	}
	return false
}

// writeSubqueryPager returns the frozen snapshot a write statement's WHERE/SET
// subqueries read against -- SQLite evaluates them over the pre-statement image
// (the collect-then-apply model), which a snapshot taken before the scan gives.
// It is both the compile-time pager (so the sub-programs compile and resolve
// their tables) and the run-time pager (stashed on the Program as WritePager).
func (db *DB) writeSubqueryPager() (*ReadOnlyPager, error) {
	return db.SnapshotPager()
}

// storedBodyPager is writeSubqueryPager for a stored trigger body: the same
// frozen snapshot without a delegated session's originating-database readers.
// A stored trigger's names bind only in its own database; C fails with "no such
// table: aux.m1" otherwise. Only a delegated session's readers are hidden
// (db.originReadersBefore > 0), as SnapshotPager does.
func (db *DB) storedBodyPager() (*ReadOnlyPager, error) {
	p, err := db.SnapshotPager()
	if err != nil {
		return nil, err
	}
	if db.originReadersBefore > 0 {
		p.attachedReaders, p.originReadersBefore = nil, 0
	}
	return p, nil
}

// conflictHalt is the error a halting write opcode raises, carrying the ON
// CONFLICT action that governs how runWrite unwinds: FAIL leaves rows applied
// so far in place, ABORT undoes this statement, ROLLBACK undoes the whole
// transaction (or, with no explicit transaction, behaves like ABORT). It
// mirrors SQLite's OE_Fail/OE_Abort/OE_Rollback halt codes.
type conflictHalt struct {
	action conflictAction
	err    error
}

func (h conflictHalt) Error() string { return h.err.Error() }
func (h conflictHalt) Unwrap() error { return h.err }

// ---- write opcode handlers (dispatched from vdbe.go's run loop) ----

// opInsert implements OP_Insert with the statement's ON CONFLICT action baked
// in. The record (r[P2]) and rowid (r[P3]) are already built; this probes the
// unique indexes and rowid, resolves a conflict and stores. Returns a jump
// target, or -1: an IGNORE resolution jumps to plan.skipAddr (C's ignoreDest).
func (m *vdbe) opInsert(op *Instruction) (int, error) {
	plan := op.P4.(*insertPlan)
	rowid := uint64(m.regs[op.P3].I)
	// The register-built row is full-width but carries NULL in every
	// generated column's slot (the compiler emits no VALUES expression for
	// one -- see insertSlotMap). Derive them here, BEFORE conflict detection,
	// so a UNIQUE index or a CHECK over a generated column sees its real
	// value, and so a STORED column's computed value is what reaches the
	// record. computeGeneratedInto is a no-op for a table with none.
	if err := computeGeneratedInto(plan.tbl.name, plan.tbl.cols, m.recRegs[op.P2]); err != nil {
		return -1, err
	}
	if err := checkRowRecordLength(m.recRegs[op.P2]); err != nil {
		return -1, err
	}
	applied, err := m.storeResolvingConflicts(plan, rowid, m.recRegs[op.P2])
	if err != nil {
		return -1, err
	}
	if !applied {
		// OE_Ignore: nothing stored, nothing counted, and everything the
		// compiler emitted after this instruction for this row -- an AFTER
		// trigger program, a RETURNING row -- is skipped, exactly as
		// sqlite3GenerateConstraintChecks' jump to endOfLoop skips SQLite's own.
		// A zero skipAddr is "no label" (see insertPlan.skipAddr): fall through,
		// which is what every emitter that has nothing after the store wants.
		if plan.skipAddr == 0 {
			return -1, nil
		}
		return plan.skipAddr, nil
	}
	m.wctx.rowsAffected++
	m.wctx.rowsInserted++ // insert.c's "OP_AddImm regRowCount, 1"
	return -1, nil
}

// replaceResolutionCoded reports whether any constraint check this INSERT emits
// can resolve to OE_Replace. That is C's gate on compiling the victim's DELETE
// trigger body (sqlite3GenerateRowDelete from the OE_Replace arms,
// insert.c:2314, 2601), and so on whether that body's errors are this INSERT's.
//
// Each onError resolves as in C: the statement's OR clause if given, else the
// constraint's declared clause, else ABORT (insert.c:2245, 2466; same as
// effectiveHitAction). The rowid arm counts only when the rowid is supplied
// explicitly (insert.c:2241, 1570). An upsert clause covering a constraint makes
// it IGNORE/UPDATE, never REPLACE (insert.c:2253, 2477).
func (db *DB) replaceResolutionCoded(tbl *tableMeta, stmt *insertStmt, slotOf []int, rowidSlot int) bool {
	covered := func(key string) bool {
		for up := stmt.upsert; up != nil; up = up.next {
			if up.targetCols == nil || upsertTargetKey(tbl, up.targetCols) == key {
				return true
			}
		}
		return false
	}
	// The rowid/INTEGER PRIMARY KEY constraint (insert.c:2241-2250).
	pkChng := rowidSlot >= 0 || (tbl.ipkIndex >= 0 && tbl.ipkIndex < len(slotOf) && slotOf[tbl.ipkIndex] >= 0)
	if pkChng && !tbl.withoutRowid && !covered(hitTargetKey(tbl, conflictHit{target: []string{"rowid"}})) {
		onError := conflictAbort
		if tbl.ipkIndex >= 0 {
			onError = tbl.cols[tbl.ipkIndex].RowidConflict
		}
		if stmt.explicitOr {
			onError = stmt.orAction
		}
		if onError == conflictReplace {
			return true
		}
	}
	// Every UNIQUE constraint (insert.c:2466-2475). An expression or partial
	// index is NOT skipped here, unlike declaredHitAction's own scan: it is a
	// real UNIQUE index to the C, and an explicit OR REPLACE resolves it like
	// any other (it can never carry a declared clause of its own -- only a
	// CREATE TABLE constraint can, and those are always column-keyed).
	for _, idx := range db.indexes {
		if !idx.unique || !indexBelongsTo(idx, tbl) || covered(targetKey(idx.cols)) {
			continue
		}
		onError := idx.onConflict
		if stmt.explicitOr {
			onError = stmt.orAction
		}
		if onError == conflictReplace {
			return true
		}
	}
	return false
}

// compileReplaceVictimDeletePlans compiles tbl's BEFORE/AFTER DELETE triggers
// for a REPLACE resolution's implicit victim delete. C does this only with
// recursive_triggers ON (insert.c:2220), passing the triggers to
// sqlite3GenerateRowDelete at both OE_Replace arms (insert.c:2339, 2613), the
// same codegen as an ordinary DELETE row (delete.c:745).
//
// The policy is literally OE_Replace, overriding each body statement's clause
// (trigger.c:1137), hence the orconf in triggerPrgMemo's key. After the AFTER
// program, uniqueness is re-checked (insert.c:2626; see replaceRecheckHit).
//
// Returns nils when C would have no trigger (flag off or no DELETE trigger). A
// trigger that cannot be lowered is errVDBEUnsupported.
func (db *DB) compileReplaceVictimDeletePlans(tbl *tableMeta, displayName string, memo *triggerPrgMemo) (before, after *triggerFirePlan, err error) {
	if !db.hasReplaceVictimDeleteTriggers(tbl) {
		return nil, nil, nil
	}
	if !db.triggersCompilable(tbl, triggerDelete) {
		return nil, nil, fmt.Errorf("%w: a conflict-resolving write on %s while PRAGMA recursive_triggers is ON, and %s has a DELETE trigger this compiler cannot lower", errVDBEUnsupported, displayName, displayName)
	}
	b, berr := db.compileTriggerFirePlanOrconf(tbl, triggerDelete, triggerBefore, memo, conflictReplace, true)
	if berr != nil {
		return nil, nil, berr
	}
	a, aerr := db.compileTriggerFirePlanOrconf(tbl, triggerDelete, triggerAfter, memo, conflictReplace, true)
	if aerr != nil {
		return nil, nil, aerr
	}
	return b, a, nil
}

// pinViolation raises SQLITE_CONSTRAINT_PINNED (btree.c:762) when the row store
// is about to change for a table whose enclosing UPDATE pinned its cursor (see
// writeCtx.pinnedTable). The action is ABORT, rolling the statement back.
func (wc *writeCtx) pinViolation(tbl *tableMeta) error {
	if wc == nil || wc.pinnedTable == nil || wc.pinnedTable != tbl {
		return nil
	}
	return conflictHalt{action: conflictAbort, err: errors.New("engine: constraint failed")}
}

// pinForVictimDelete installs the pin around one victim delete and returns its
// release -- insert.c:2611-2618's OP_CursorLock/OP_CursorUnlock pair. Nested
// rather than assigned so a cascade that reaches a second conflict-resolving
// UPDATE restores the outer pin instead of clearing it.
func (wc *writeCtx) pinForVictimDelete(tbl *tableMeta) func() {
	prev := wc.pinnedTable
	wc.pinnedTable = tbl
	return func() { wc.pinnedTable = prev }
}

// fireReplaceVictimDeleteRow removes one REPLACE victim, firing the compiled
// DELETE trigger programs around it as sqlite3GenerateRowDelete does
// (delete.c:745):
//
//   - a row already gone is a no-op;
//   - a BEFORE RAISE(IGNORE) skips the removal and AFTER, and the victim
//     survives;
//   - a BEFORE body that deleted the row leaves nothing to remove;
//   - an AFTER RAISE(IGNORE) arrives after the row is gone.
//
// The removal is journaled. before/after are parameters so INSERT and UPDATE
// share this code.
func (m *vdbe) fireReplaceVictimDeleteRow(tbl *tableMeta, before, after *triggerFirePlan, rid uint64) error {
	db := m.wctx.db
	oldVals, ok := tbl.rows.get(rid)
	if !ok {
		return nil
	}
	// The OLD row a trigger body sees is the NORMALIZED image (IPK column
	// filled in from the rowid, REAL affinity restored) -- the same image
	// compileDeleteStmt's own OpColumn reads give an ordinary DELETE's
	// OpFireTriggers.
	oldRow := normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, rid, oldVals)
	if before != nil {
		if err := m.firePlanForRow(before, nil, 0, oldRow, int64(rid)); err != nil {
			if isRaiseIgnore(err) {
				return nil // the victim survives
			}
			return err
		}
		if _, still := tbl.rows.get(rid); !still {
			return nil // the BEFORE body already deleted it
		}
	}
	victim := tbl.rows.row(rid)
	db.noteRowChange(tbl, RowDelete, rid, victim, nil)
	db.fkRowMutated(tbl, rid, rid, victim, nil, nil)
	tbl.dropRow(rid)
	m.wctx.undoPut(tbl, rid, victim)
	if after != nil {
		if err := m.firePlanForRow(after, nil, 0, oldRow, int64(rid)); err != nil && !isRaiseIgnore(err) {
			return err
		}
	}
	return nil
}

// storeResolvingConflicts stores one candidate row, resolving a UNIQUE/rowid
// conflict per plan.action. Returns whether the row was stored (false only for
// OR IGNORE skipping a conflicting row). Every storage effect -- the store
// itself and any OR REPLACE victim deletion -- pushes an undo closure onto the
// statement journal, so a later row's failure rolls the whole statement back.
func (m *vdbe) storeResolvingConflicts(plan *insertPlan, rowid uint64, full []Value) (bool, error) {
	db, tbl := m.wctx.db, plan.tbl
	// See writeCtx.pinnedTable: an INSERT reached from inside an enclosing
	// UPDATE's REPLACE victim delete, against that UPDATE's own table.
	if perr := m.wctx.pinViolation(tbl); perr != nil {
		return false, perr
	}
	if bl := m.bulkLoadFor(plan); bl != nil {
		stored, err := m.bulkStore(bl, tbl, rowid, full)
		if err != nil || stored {
			return stored, err
		}
		// Out of order: the built rows are in the row store now; this one
		// takes the ordinary path below.
	}
	hits := findRowConflicts(db, tbl, rowid, full)
	if len(hits) > 0 {
		nonReplace, actions, replaceRowids := classifyHits(hits, func(h conflictHit) conflictAction {
			return effectiveHitAction(db, tbl, h, plan.action, plan.explicitOr)
		})
		if len(nonReplace) > 0 {
			if actions[0] == conflictIgnore {
				return false, nil // OR IGNORE: skip this row, keep going
			}
			// ABORT/FAIL/ROLLBACK surface the constraint error; the action rides
			// on the conflictHalt so runWrite unwinds correctly (FAIL keeps
			// prior rows, ABORT undoes the statement, ROLLBACK the transaction).
			return false, conflictHalt{action: actions[0],
				err: fmt.Errorf("engine: INSERT into %s: %w", plan.displayName, uniqueConflictError(tbl, nonReplace[0]))}
		}
		if plan.replaceDelBefore != nil || plan.replaceDelAfter != nil {
			// recursive_triggers is ON with DELETE triggers, so the victim's removal is a
			// real row delete with BEFORE/AFTER programs (insert.c:2339, 2612), run under
			// REPLACE whatever the statement's OR clause (enterOrconfReplace, trigger.go).
			restore := m.wctx.fireState().enterOrconfReplace()
			var fireErr error
			for _, rid := range replaceRowids {
				if err := m.fireReplaceVictimDeleteRow(tbl, plan.replaceDelBefore, plan.replaceDelAfter, rid); err != nil {
					fireErr = err
					break
				}
			}
			restore()
			if fireErr != nil {
				// The trigger program's own error (a RAISE, or a constraint
				// its writes violated), passed through UNWRAPPED exactly as
				// the ordinary AFTER-trigger propagation does -- C SQLite
				// surfaces RAISE(ABORT,'msg') as exactly 'msg'.
				return false, fireErr
			}
			// insert.c re-tests every uniqueness constraint once a replace
			// trigger has fired, because the program may have changed the very
			// rows just probed -- and the retest's own resolution is hardcoded
			// OE_Abort, never REPLACE again (insert.c:2663
			// "sqlite3UniqueConstraint(pParse, OE_Abort, pIdx);" and
			// insert.c:2704 "sqlite3RowidConstraint(pParse, OE_Abort, pTab);").
			// replaceRecheckHit (conflict.go) is that filter.
			if rh, ok := replaceRecheckHit(db, tbl, findRowConflicts(db, tbl, rowid, full), plan.action, plan.explicitOr); ok {
				return false, conflictHalt{action: conflictAbort,
					err: fmt.Errorf("engine: INSERT into %s: %w", plan.displayName, uniqueConflictError(tbl, rh))}
			}
		} else {
			// The plans are absent because replaceResolutionCoded said no
			// constraint of this statement could resolve to OE_Replace, so a
			// victim reaching here at RUN time means that compile-time answer
			// was wrong -- and dropping the row below would SILENTLY skip its
			// DELETE triggers, which is exactly invariant 2's failure mode. An
			// error instead: unreachable by construction, and cheap enough to
			// keep as the ratchet that says so.
			if len(replaceRowids) > 0 && db.hasReplaceVictimDeleteTriggers(tbl) {
				return false, fmt.Errorf("engine: INSERT into %s: internal: REPLACE victim with no compiled DELETE trigger plan", plan.displayName)
			}
			for _, rid := range replaceRowids {
				victim := tbl.rows.row(rid)
				db.noteRowChange(tbl, RowDelete, rid, victim, nil)
				db.fkRowMutated(tbl, rid, rid, victim, nil, nil)
				tbl.dropRow(rid)
				m.wctx.undoPut(tbl, rid, victim)
			}
		}
	}
	if err := db.checkIndexExprsForRow(tbl, rowid, full); err != nil {
		return false, err
	}
	m.storeRow(tbl, rowid, full)
	return true, nil
}

// storeRow writes one row into the table's row store and journals its undo. It
// is the shared tail of an insert and an update's re-store: the row image is
// already final (affinity/NOT NULL/CHECK done, IPK column nulled by the
// compiler for a rowid table).
func (m *vdbe) storeRow(tbl *tableMeta, rowid uint64, full []Value) {
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null} // stored NULL; the rowid carries the value
	}
	tbl.putRow(rowid, full)
	m.wctx.db.noteRowChange(tbl, RowInsert, rowid, nil, full)
	m.wctx.db.fkRowMutated(tbl, rowid, rowid, nil, full, nil)
	if !tbl.withoutRowid {
		m.wctx.db.lastInsertRowid = int64(rowid)
	}
	m.wctx.undoDrop(tbl, rowid)
}

// opDelete implements OP_Delete: remove the row the cursor points at from the
// table's row store, immediately, and count it. The cursor iterates a snapshot
// (openRowStoreCursor), so deleting under it is safe -- exactly SQLite's own
// "delete the record the cursor points at, the cursor stays usable" contract.
func (m *vdbe) opDelete(op *Instruction) error {
	tbl := op.P4.(*tableMeta)
	if perr := m.wctx.pinViolation(tbl); perr != nil { // see writeCtx.pinnedTable
		return perr
	}
	rowid := m.cursors[op.P1].rowid
	victim := tbl.rows.row(rowid)
	m.wctx.db.noteRowChange(tbl, RowDelete, rowid, victim, nil) // capture log (rowhook.go)
	m.wctx.db.fkRowMutated(tbl, rowid, rowid, victim, nil, nil)
	tbl.dropRow(rowid)
	m.wctx.undoPut(tbl, rowid, victim)
	m.wctx.rowsAffected++
	return nil
}

// opUpdateRow stores an UPDATE: remove the cursor's row and re-store the new
// image under the new rowid, C's OP_Delete + OP_Insert (OPFLAG_ISUPDATE).
// SET evaluation, affinity, NOT NULL, CHECK and the new rowid were done by
// earlier instructions. Each row is journaled so a later failure unwinds the
// statement. Returns a jump target, or -1: an IGNORE resolution leaves the row
// and jumps to plan.skipAddr.
func (m *vdbe) opUpdateRow(op *Instruction) (int, error) {
	plan := op.P4.(*updatePlan)
	tbl := plan.tbl
	oldRowid := m.cursors[op.P1].rowid
	newRowid := uint64(m.regs[op.P3].I)
	full := m.recRegs[op.P2]
	// A BEFORE trigger may have updated this row after the cursor read it, so
	// columns the statement does not assign are re-read from the store (ticket
	// e25d9ea771, triggerC.test 10.3). Done before computeGeneratedInto. Read once;
	// the restore arms below reuse it.
	oldVals := tbl.rows.row(oldRowid)
	if oldVals != nil && plan.setCol != nil {
		for i := range full {
			if i < len(oldVals) && i < len(plan.setCol) && !plan.setCol[i] {
				full[i] = oldVals[i]
			}
		}
	}
	// Recompute generated columns from the NEW values, with the IPK slot holding
	// the NEW rowid: codegen nulled it when SET assigns the rowid alias, which made
	// "y AS (k*2)" NULL and let an FK on it pass vacuously.
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Int, I: int64(newRowid)}
	}
	if err := computeGeneratedInto(tbl.name, tbl.cols, full); err != nil {
		return -1, err
	}
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null} // stored NULL; the rowid carries the value
	}
	// insert.c:2611-2618's pinned cursor: this UPDATE is running inside some
	// enclosing UPDATE's REPLACE victim delete, against the very table that
	// statement is walking. See writeCtx.pinnedTable.
	if perr := m.wctx.pinViolation(tbl); perr != nil {
		return -1, perr
	}
	// C overwrites in place when the rowid is unchanged: OP_Delete only for
	// "hasFK>1 || chngKey" (update.c:1086), then a same-size overwrite
	// (btree.c:9644). The conflict-aware path still deletes first so its probe
	// cannot hit the row's former self; a rowid change is a real delete+insert.
	if plan.conflictAware || newRowid != oldRowid {
		tbl.dropRow(oldRowid)
	}
	if plan.conflictAware {
		// The UNIQUE/rowid probe, resolved by the ON CONFLICT action: the same
		// sqlite3GenerateConstraintChecks an INSERT uses (update.c:1031), with the row
		// already removed so it cannot conflict with itself.
		hits := findRowConflicts(m.wctx.db, tbl, newRowid, full)
		if len(hits) > 0 {
			nonReplace, actions, replaceRowids := classifyHits(hits, func(h conflictHit) conflictAction {
				return effectiveHitAction(m.wctx.db, tbl, h, plan.action, plan.explicitOr)
			})
			if len(nonReplace) > 0 {
				// THIS row's own write never happened, so it goes straight back
				// -- unconditionally, exactly like the post-write branch below
				// and for the same reason: under "PRAGMA journal_mode=off" the
				// statement journal's unwind is skipped entirely
				// (DB.journalOffUndoDisabled), so a row that never passed its
				// own check must not be left behind alongside the earlier rows
				// that did.
				tbl.putRow(oldRowid, oldVals)
				if actions[0] == conflictIgnore {
					return plan.skipAddr, nil // OE_Ignore: skip this row, keep scanning
				}
				// ABORT/FAIL/ROLLBACK ride on the conflictHalt so resolveHalt
				// unwinds correctly (FAIL keeps prior rows, ABORT undoes the
				// statement, ROLLBACK the transaction).
				return -1, conflictHalt{action: actions[0],
					err: fmt.Errorf("engine: UPDATE %s: %w", plan.displayName, uniqueConflictError(tbl, nonReplace[0]))}
			}
			if plan.replaceDelBefore != nil || plan.replaceDelAfter != nil {
				// recursive_triggers ON with DELETE triggers: each victim is a real row delete
				// with its own programs, under OE_Replace, then the uniqueness re-check, as in
				// storeResolvingConflicts (insert.c:2339, 2613). A victim that is one of this
				// statement's matched rows is gone from the store and the loop's re-seek drops
				// it, so no extra bookkeeping is needed.
				undoOrconf := m.wctx.fireState().enterOrconfReplace()
				// insert.c:2611-2618's OP_CursorLock, which the INSERT half of
				// this block deliberately does not take (isUpdate is 0 there).
				unpin := m.wctx.pinForVictimDelete(tbl)
				var fireErr error
				for _, rid := range replaceRowids {
					if err := m.fireReplaceVictimDeleteRow(tbl, plan.replaceDelBefore, plan.replaceDelAfter, rid); err != nil {
						fireErr = err
						break
					}
				}
				unpin()
				undoOrconf()
				if fireErr == nil {
					// insert.c re-tests every uniqueness constraint once a
					// replace trigger has fired, under a hardcoded OE_Abort
					// (insert.c:2663 / :2704) -- replaceRecheckHit is that
					// filter.
					if rh, ok := replaceRecheckHit(m.wctx.db, tbl, findRowConflicts(m.wctx.db, tbl, newRowid, full), plan.action, plan.explicitOr); ok {
						fireErr = conflictHalt{action: conflictAbort,
							err: fmt.Errorf("engine: UPDATE %s: %w", plan.displayName, uniqueConflictError(tbl, rh))}
					}
				}
				if fireErr != nil {
					// THIS row's own write never happened, so it goes straight
					// back -- the same unconditional restore the non-REPLACE
					// branch above does, and for the same journal_mode=off
					// reason. Everything the cascade DID do is journaled, and
					// wc.rollback unwinds it on the way out.
					tbl.putRow(oldRowid, oldVals)
					return -1, fireErr
				}
			} else {
				// The plans are absent because nothing about this statement can
				// fire a victim's DELETE triggers -- see storeResolvingConflicts'
				// identical guard for why a victim reaching here ANYWAY is an
				// error rather than a silent trigger skip.
				if len(replaceRowids) > 0 && m.wctx.db.hasReplaceVictimDeleteTriggers(tbl) {
					tbl.putRow(oldRowid, oldVals)
					return -1, fmt.Errorf("engine: UPDATE %s: internal: REPLACE victim with no compiled DELETE trigger plan", plan.displayName)
				}
				for _, rid := range replaceRowids {
					victim := tbl.rows.row(rid)
					m.wctx.db.noteRowChange(tbl, RowDelete, rid, victim, nil)
					m.wctx.db.fkRowMutated(tbl, rid, rid, victim, nil, nil)
					tbl.dropRow(rid)
					m.wctx.undoPut(tbl, rid, victim)
				}
			}
		}
		tbl.putRow(newRowid, full)
	} else {
		if newRowid != oldRowid {
			if _, exists := tbl.rows.get(newRowid); exists {
				tbl.putRow(oldRowid, oldVals) // this row is not applied at all
				return -1, fmt.Errorf("engine: UPDATE %s: UNIQUE constraint failed: duplicate rowid %d", plan.displayName, int64(newRowid))
			}
		}
		tbl.putRow(newRowid, full)
	}
	// The plain UNIQUE check runs after this row is stored, so a violation means
	// the write never counted: put the row back unjournaled. Under
	// journal_mode=off the statement unwind is skipped, and earlier rows' writes
	// survive while this one must not, as in C.
	if !plan.conflictAware {
		if err := m.wctx.db.checkUniqueIndexesForRow(tbl, newRowid, full); err != nil {
			tbl.dropRow(newRowid)
			tbl.putRow(oldRowid, oldVals)
			return -1, fmt.Errorf("engine: UPDATE %s: %w", plan.displayName, err)
		}
	}
	if err := m.wctx.db.checkIndexExprsForRow(tbl, newRowid, full); err != nil {
		tbl.dropRow(newRowid)
		tbl.putRow(oldRowid, oldVals)
		return -1, err
	}
	m.wctx.undoMove(tbl, newRowid, oldRowid, oldVals)
	m.wctx.db.noteRowUpdate(tbl, oldRowid, newRowid, oldVals, full) // capture log (rowhook.go)
	m.wctx.db.fkRowMutated(tbl, oldRowid, newRowid, oldVals, full, fkColMask(len(tbl.cols), plan.colIdx))
	m.wctx.rowsAffected++
	return -1, nil
}

// opUpsertFind implements OP_UpsertFind (see the opcode doc). It probes the
// target constraint for the candidate row and steers execution: no conflict ->
// jump to the plain-insert block; conflict on a non-target constraint -> error;
// conflict on the target -> load the existing row and (DO NOTHING) skip or
// (DO UPDATE) fall through to the SET block.
func (m *vdbe) opUpsertFind(op *Instruction) (target int, err error) {
	plan := op.P4.(*upsertPlan)
	tbl := plan.tbl
	candBase := op.P1
	full := make([]Value, len(tbl.cols))
	copy(full, m.regs[candBase:candBase+len(tbl.cols)])
	candRowid := uint64(m.regs[plan.existRowidReg].I) // caller staged the candidate rowid here; overwritten below on a hit

	hits := findRowConflicts(m.wctx.db, tbl, candRowid, full)
	if len(hits) == 0 {
		return op.P2, nil // no conflict: jump to the plain-insert block
	}
	// sqlite3GenerateConstraintChecks with pUpsert set: each constraint in
	// upsertCheckOrder takes the first clause naming it, or the target-less
	// one, else its own action. A REPLACE deletes its row and the checks go
	// on; the plain-insert block does those deletes, so a clause or an
	// IGNORE/FAIL reached after one -- C has deleted the row by then -- is
	// declined rather than answered without it.
	var victims []uint64
	for _, h := range upsertCheckOrder(tbl, plan.arms, hits) {
		if slices.Contains(victims, h.rowid) {
			continue // that row is gone by the time C checks this constraint
		}
		arm := plan.armFor(tbl, h)
		if arm == nil {
			act := effectiveHitAction(m.wctx.db, tbl, h, plan.action, plan.explicitOr)
			if act == conflictReplace {
				victims = append(victims, h.rowid)
				continue
			}
			if len(victims) > 0 && act != conflictAbort && act != conflictRollback {
				return -1, fmt.Errorf("%w: upsert: a REPLACE resolved ahead of a non-REPLACE conflict", errVDBEUnsupported)
			}
			if act == conflictIgnore {
				return plan.skipAddr, nil
			}
			return -1, conflictHalt{action: act,
				err: fmt.Errorf("engine: INSERT into %s: %w", plan.displayName, uniqueConflictError(tbl, h))}
		}
		if len(victims) > 0 {
			return -1, fmt.Errorf("%w: upsert: a REPLACE resolved ahead of the ON CONFLICT clause", errVDBEUnsupported)
		}
		m.regs[plan.existRowidReg] = Value{Typ: Int, I: int64(h.rowid)}
		// Load the existing row (normalized: IPK column filled from its rowid)
		// into the DO UPDATE scope's registers.
		norm := normalizeRow(tbl.name, tbl.cols, tbl.ipkIndex, h.rowid, tbl.rows.row(h.rowid))
		copy(m.regs[op.P3:op.P3+len(tbl.cols)], norm)
		if arm.doNothing {
			return plan.skipAddr, nil // DO NOTHING: skip this row
		}
		return arm.addr, nil
	}
	// Only REPLACE victims: the plain insert deletes them, as long as it
	// resolves every one of these conflicts to REPLACE too.
	for _, h := range hits {
		if effectiveHitAction(m.wctx.db, tbl, h, plan.action, plan.explicitOr) != conflictReplace {
			return -1, fmt.Errorf("%w: upsert: a REPLACE victim also conflicts on a constraint the plain insert resolves differently", errVDBEUnsupported)
		}
	}
	return op.P2, nil
}

// armFor is sqlite3UpsertOfIndex (upsert.c:247): the first clause naming h's
// constraint, or the target-less clause, which is always last; nil when no
// clause applies.
func (p *upsertPlan) armFor(tbl *tableMeta, h conflictHit) *upsertArm {
	key := hitTargetKey(tbl, h)
	for i := range p.arms {
		if p.arms[i].key == "" || p.arms[i].key == key {
			return &p.arms[i]
		}
	}
	return nil
}

// upsertCheckOrder puts hits (findRowConflicts' order: the rowid, then the
// indexes as pTab->pIndex lists them) in the order sqlite3GenerateConstraint-
// Checks tests them when an upsert names targets. The targeted indexes come
// first, in clause order (insert.c:2150-2186); the rowid check is deferred to
// just after the index of the clause whose next clause is none, target-less or
// the rowid -- or stays first when the first clause names the rowid
// (upsertIpkDelay, insert.c:2253-2266, :2400-2404, :2673-2680); then every
// other index. A lone target-less clause changes no order (insert.c:2139-2149).
func upsertCheckOrder(tbl *tableMeta, arms []upsertArm, hits []conflictHit) []conflictHit {
	if len(arms) == 0 || arms[0].key == "" {
		return hits
	}
	rowidKey := hitTargetKey(tbl, conflictHit{target: []string{"rowid"}})
	var rowidHits []conflictHit
	byKey := map[string][]conflictHit{}
	var keyOrder []string
	for _, h := range hits {
		k := hitTargetKey(tbl, h)
		if len(h.target) == 1 && h.target[0] == "rowid" {
			rowidHits = append(rowidHits, h)
			continue
		}
		if _, ok := byKey[k]; !ok {
			keyOrder = append(keyOrder, k)
		}
		byKey[k] = append(byKey[k], h)
	}
	var out []conflictHit
	rowidPlaced := false
	placeRowid := func() {
		if !rowidPlaced {
			out, rowidPlaced = append(out, rowidHits...), true
		}
	}
	if arms[0].key == rowidKey {
		placeRowid()
	}
	done := map[string]bool{}
	for i, a := range arms {
		if a.key == "" || a.key == rowidKey {
			continue
		}
		out = append(out, byKey[a.key]...)
		done[a.key] = true
		// sqlite3UpsertNextIsIPK (upsert.c:227): duplicates are already gone.
		if i+1 == len(arms) || arms[i+1].key == "" || arms[i+1].key == rowidKey {
			placeRowid()
		}
	}
	placeRowid()
	for _, k := range keyOrder {
		if !done[k] {
			out = append(out, byKey[k]...)
		}
	}
	return out
}

// opUpsertStore implements OP_UpsertStore: re-store the conflicting row under a
// (possibly new) rowid -- the storage half of DO UPDATE, journaled like an
// UPDATE so a later failure rolls the statement back.
func (m *vdbe) opUpsertStore(op *Instruction) error {
	plan := op.P4.(*updatePlan)
	tbl := plan.tbl
	if perr := m.wctx.pinViolation(tbl); perr != nil { // see writeCtx.pinnedTable
		return perr
	}
	oldRowid := uint64(m.regs[op.P2].I)
	newRowid := uint64(m.regs[op.P3].I)
	full := m.recRegs[op.P1]
	if tbl.ipkIndex >= 0 {
		full[tbl.ipkIndex] = Value{Typ: Null}
	}
	oldVals := tbl.rows.row(oldRowid)
	tbl.dropRow(oldRowid)
	if newRowid != oldRowid {
		if _, exists := tbl.rows.get(newRowid); exists {
			tbl.putRow(oldRowid, oldVals)
			return fmt.Errorf("engine: INSERT into %s: UNIQUE constraint failed: duplicate rowid %d", plan.displayName, int64(newRowid))
		}
	}
	tbl.putRow(newRowid, full)
	// See opUpdateRow's identical comment: the UNIQUE check is a post-write
	// whole-table scan, so a violation means THIS row's own write must be put
	// straight back and never journaled, same as the rowid-collision branch
	// above -- otherwise a "PRAGMA journal_mode=off" statement rollback that
	// skips the journal replay (DB.journalOffUndoDisabled) would leave this
	// row's own never-valid write sitting alongside an earlier row's genuinely
	// applied one.
	if err := m.wctx.db.checkUniqueIndexesForRow(tbl, newRowid, full); err != nil {
		tbl.dropRow(newRowid)
		tbl.putRow(oldRowid, oldVals)
		return fmt.Errorf("engine: INSERT into %s: %w", plan.displayName, err)
	}
	if err := m.wctx.db.checkIndexExprsForRow(tbl, newRowid, full); err != nil {
		tbl.dropRow(newRowid)
		tbl.putRow(oldRowid, oldVals)
		return err
	}
	m.wctx.undoMove(tbl, newRowid, oldRowid, oldVals)
	m.wctx.db.noteRowUpdate(tbl, oldRowid, newRowid, oldVals, full)
	m.wctx.db.fkRowMutated(tbl, oldRowid, newRowid, oldVals, full, fkColMask(len(tbl.cols), plan.colIdx))
	m.wctx.rowsAffected++
	return nil
}

// opUpsertReload is update.c's after-BEFORE-trigger reload loop (:1000-1017)
// for a DO UPDATE: the BEFORE program may have edited the row since
// OpUpsertFind read it, so every column the SET does not assign is reloaded into
// the registers. It runs right after the fire, so constraint checks, the stored
// record, AFTER NEW.* and RETURNING all see the reloaded row.
//
// Skipped, as in C's loop: the IPK (the register holds the real rowid) and
// generated columns, which OpComputeGenerated re-derives next
// (update.c:1018).
func (m *vdbe) opUpsertReload(op *Instruction) {
	plan := op.P4.(*updatePlan)
	tbl := plan.tbl
	cur := tbl.rows.row(uint64(m.regs[op.P1].I))
	if cur == nil {
		return // OpSkipIfRowGone, just above, already jumped past a gone row
	}
	for i := range plan.rowRegs {
		if i < len(cur) && i < len(plan.setCol) && !plan.setCol[i] && i != tbl.ipkIndex && !tbl.cols[i].IsGenerated() {
			m.regs[plan.rowRegs[i]] = cur[i]
		}
	}
}

// opTxn runs OpTxn: one transaction-control verb, already parsed into its
// txnPlan at compile time. The bodies are the same six calls the write path's
// old keyword dispatch made (see txn.go's package doc comment), reached now
// through an opcode rather than through the statement text a second time --
// which is how SQLite reaches them too: OP_AutoCommit (vdbe.c:4013) and
// OP_Savepoint (vdbe.c:3823) read operands, never SQL.
func (m *vdbe) opTxn(op *Instruction) error {
	plan := op.P4.(*txnPlan)
	switch plan.verb {
	case txnBegin:
		return m.wctx.db.beginTxn()
	case txnCommit:
		return m.wctx.db.commitTxn()
	case txnRollback:
		return m.wctx.db.rollbackTxn()
	case txnSavepoint:
		return m.wctx.db.openSavepoint(plan.name)
	case txnRelease:
		return m.wctx.db.releaseSavepoint(plan.name)
	case txnRollbackTo:
		return m.wctx.db.rollbackToSavepoint(plan.name)
	}
	return fmt.Errorf("engine: vdbe: unknown transaction verb %d", plan.verb)
}

func (m *vdbe) opDdl(op *Instruction) error {
	plan := op.P4.(*ddlPlan)
	// This is now the ONLY route a DDL statement takes: the keyword dispatch
	// that used to carry the same guard (execNonRowStatement, insert_write.go)
	// is deleted. WHICH of the two interlocks a kind runs is ddlKind.guards(),
	// which reproduces that dispatch's per-keyword split rather than guarding
	// every kind -- PRAGMA/VACUUM/ATTACH/DETACH run neither.
	wsGuard, fts5Guard := plan.kind.guards()
	if (wsGuard && m.wctx.db.writableSchemaEditsActive()) || (fts5Guard && len(m.wctx.db.fts5TxnPending) > 0) {
		toks, lerr := lex(strings.TrimSpace(plan.sql))
		if lerr != nil {
			return lerr
		}
		kw := "DDL"
		if len(toks) > 0 && toks[0].kind == tkIdent {
			kw = toks[0].upper()
		}
		if wsGuard {
			if err := m.wctx.db.writableSchemaDDLDecline(kw, toks); err != nil {
				return err
			}
		}
		if fts5Guard {
			if err := m.wctx.db.fts5TxnPendingDDLGuard(kw); err != nil {
				return err
			}
		}
	}
	switch plan.kind {
	case ddlCreateTable:
		return m.wctx.db.noteSchemaChange(m.wctx.db.CreateTable(plan.sql))
	case ddlCreateIndex:
		return m.wctx.db.noteSchemaChange(m.wctx.db.CreateIndex(plan.sql))
	case ddlCreateView:
		return m.wctx.db.noteSchemaChange(m.wctx.db.CreateView(plan.sql))
	case ddlDropIndex:
		return m.wctx.db.noteSchemaChange(m.wctx.db.DropIndex(plan.sql))
	case ddlDropTable:
		return m.wctx.db.noteSchemaChange(m.wctx.db.DropTable(plan.sql))
	case ddlDropView:
		return m.wctx.db.noteSchemaChange(m.wctx.db.DropView(plan.sql))
	case ddlAlterTable:
		db := m.wctx.db
		err := db.AlterTable(plan.sql)
		if undo := db.wsAlterUndo; undo != nil {
			db.wsAlterUndo = nil
			if err != nil {
				db.restoreSnapshot(undo)
			}
		}
		return db.noteSchemaChange(err)
	case ddlCreateTrigger:
		return m.wctx.db.noteSchemaChange(m.wctx.db.CreateTrigger(plan.sql))
	case ddlDropTrigger:
		return m.wctx.db.noteSchemaChange(m.wctx.db.DropTrigger(plan.sql))
	case ddlCreateVirtualTable:
		// The one CREATE the old keyword dispatch did NOT wrap in
		// noteSchemaChange -- preserved verbatim rather than regularized, since
		// db.CreateVirtualTable (vtab.go) already publishes what it changed.
		return m.wctx.db.CreateVirtualTable(plan.sql)
	case ddlPragma:
		return m.wctx.db.execPragma(plan.sql)
	case ddlAnalyze:
		return m.wctx.db.execAnalyze(plan.sql)
	case ddlReindex:
		return m.wctx.db.execReindex(plan.sql)
	case ddlVacuum:
		return m.wctx.db.execVacuum(plan.sql)
	case ddlAttach:
		return m.wctx.db.execAttach(plan.sql)
	case ddlDetach:
		return m.wctx.db.execDetach(plan.sql)
	}
	return fmt.Errorf("engine: vdbe: unknown DDL kind %d", plan.kind)
}

// xferShapedSelect is xferOptimization's syntactic half (insert.c:3037,
// 3052-3086): "SELECT * FROM <one table>" with no WITH and nothing else.
func xferShapedSelect(sel *SelectStmt) bool {
	return len(sel.CTEs) == 0 && len(sel.From) == 1 && sel.From[0].Subquery == nil && sel.Where == nil &&
		len(sel.OrderBy) == 0 && len(sel.GroupBy) == 0 && sel.Limit == nil && sel.LimitParam == nil &&
		len(sel.Compound) == 0 && !sel.Distinct && len(sel.Columns) == 1 && sel.Columns[0].Star &&
		sel.Columns[0].StarQualifier == ""
}

// compileInsertSelectWrite compiles "INSERT INTO t [(cols)] SELECT ...": the
// source compiles to a sub-Program opened as a row source (OpOpenDerived) and
// each row goes through the same per-row sequence as a VALUES tuple. Source
// rows are materialized first so "INSERT INTO t SELECT * FROM t" does not see
// its own rows. beforePlan/firePlan are the target's INSERT triggers, fired per
// source row (insert.c:1495, 1604).
func (db *DB) compileInsertSelectWrite(stmt *insertStmt, tbl *tableMeta, colIdx []int, trig *trigCompileCtx, beforePlan, firePlan *triggerFirePlan) (*Program, error) {
	// A trigger body's INSERT ... SELECT reads the database as of the firing, so
	// the source is lowered live (vdbe_live_read.go). The compile still decides
	// arity, names and xfer eligibility. A leading WITH keeps the frozen route.
	live := trig != nil && len(stmt.ctes) == 0
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, fmt.Errorf("engine: INSERT into %s: %w", stmt.table, perr)
	}
	// C SQLite validates every table a statement names at PREPARE time,
	// including one reachable only through a subquery that short-circuit
	// evaluation would never reach. See validateSelectTablesExist.
	if verr := validateSelectTablesExist(pager, stmt.selectStmt); verr != nil {
		return nil, fmt.Errorf("engine: INSERT into %s: %w", stmt.table, verr)
	}
	// Rewrite fts5's table-valued form "FROM f('query')" into a scan + MATCH, as
	// execSelect does (query.go); this path bypasses execSelect.
	srcSel := pager.fts5RewriteTableFunc(stmt.selectStmt)
	// In a trigger body the source SELECT may name NEW./OLD.; trigOnlyOuter gives
	// an outer that resolves only those, as OpParam reads of the firing row. nil
	// trig gives a nil outer. The SELECT runs as a coroutine at the INSERT's
	// nQueryLoop (see compiler.nQueryLoop).
	srcOuter := trigOnlyOuter(trig, nil)
	if srcOuter != nil {
		srcOuter.nQueryLoopKnown = true
	}
	// "INSERT INTO t2 SELECT * FROM t1" may be copied by xferOptimization
	// (insert.c:3012), which never reaches sqlite3WhereBegin -- so for that
	// syntactic shape (insert.c:3052-3086) an INDEXED BY partial hint on t1
	// that leaves no loop is a decline, not "no query solution": whether the
	// copy happens rests on its semantic half, not ported here.
	leave := func() {}
	if stmt.cols == nil && len(stmt.ctes) == 0 {
		// When the copy provably happens it reads the source b-tree in ROWID
		// order and never plans (insert.c:3273-3304), so an INDEXED BY hint --
		// even one naming no index -- neither orders nor refuses it, and a new
		// rowid follows the source's. NOT INDEXED is that scan. The narrowed
		// gate excludes every case C decides at run time (emptyDestTest,
		// insert.c:3248-3271): no index on the destination, onError ABORT or
		// ROLLBACK.
		if _, ok := db.xferOptimizationEligible(tbl, srcSel, stmt.returning != nil, stmt.orAction, stmt.explicitOr); ok {
			cp := *srcSel
			cp.From = append([]FromItem(nil), srcSel.From...)
			cp.From[0].IndexedBy, cp.From[0].NotIndexed = "", true
			srcSel = &cp
		} else if xferShapedSelect(srcSel) && srcSel.From[0].IndexedBy != "" {
			// Unproven: C may still copy, deciding at RUN time on an empty
			// destination (emptyDestTest), and that copy's rowid order differs
			// from the hinted scan's.
			return nil, fmt.Errorf("%w: INSERT ... SELECT * over INDEXED BY that C may answer by a row copy", errVDBEUnsupported)
		}
	}
	if xferShapedSelect(srcSel) && pager != nil {
		leave = pager.enterFromBody()
	}
	prog, cerr := compileSubProgram(pager, srcSel, srcOuter)
	leave()
	if errors.Is(cerr, errVDBESemantic) {
		// A PREPARE-time rejection C SQLite also makes ("no such function",
		// "no such column"): propagated exactly as it was, sentinel and wording
		// intact, so TestPrepareFailureIsAHardCompileError's changes() rule
		// keeps holding. Only the CAPABILITY half is rewritten below.
		return nil, cerr
	}
	if cerr != nil && trig != nil {
		// A trigger body's source keeps a decline: it compiles with no enclosing scope,
		// so NEW does not resolve here.
		return nil, cerr
	}
	if cerr != nil {
		// A SELECT the compiler cannot lower has nowhere else to run, so this is a hard
		// error with execSelect's wording (query.go). %v, not %w: dropping
		// errVDBEUnsupported marks it as the statement's own error.
		return nil, fmt.Errorf("engine: INSERT into %s: engine: VDBE-only: %v (no fallback)", stmt.table, cerr)
	}
	// A column-list-less INSERT supplies a value for every NON-generated
	// column only (C SQLite counts generated columns out entirely), and
	// each generated slot is filled by computeGeneratedInto instead.
	nInsertable := 0
	for _, c := range tbl.cols {
		if !c.IsGenerated() {
			nInsertable++
		}
	}
	want := nInsertable
	if stmt.cols != nil {
		want = len(colIdx)
	}
	// xferEligible: the shape C's xferOptimization handles (insert.c:3012), tried
	// before the arity check (insert.c:1030) and bypassing it; see
	// xferOptimizationEligible (insert_write.go). Eligibility guarantees one
	// source column per dest column, so srcCursor can be indexed by dest column.
	xferEligible := false
	if len(prog.ColNames) != want {
		if stmt.cols == nil && hasGeneratedCols(tbl.cols) {
			// RETURNING disables xferOptimization (insert.c:1032: it is a trigger), so such
			// a statement falls through to the arity error as in C. A real trigger on the
			// destination is checked inside xferOptimizationEligible.
			_, xferEligible = db.xferOptimizationEligible(tbl, stmt.selectStmt, stmt.returning != nil, stmt.orAction, stmt.explicitOr)
		}
		if !xferEligible {
			// insert.c:1257 -- with an explicit column list the message is
			// "%d values for %d columns", values first; insert.c:1250-1252's
			// "table ... has N columns but M values were supplied" is the
			// no-column-list form.
			if stmt.cols != nil {
				return nil, fmt.Errorf("engine: %d values for %d columns", len(prog.ColNames), want)
			}
			return nil, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, want, len(prog.ColNames))
		}
	}
	slotOf, rowidSlot, merr := insertSlotMapN(tbl, stmt, colIdx, want)
	if merr != nil {
		return nil, merr
	}
	if derr := insertRejectOmittedDefaults(tbl, stmt, slotOf); derr != nil {
		return nil, derr
	}

	c := &compiler{}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	c.emit(Instruction{Op: OpOpenWrite, P1: 0, P4: tbl})
	c.allocCursor() // cursor 0: the write target

	srcCols := pager.derivedColumnInfos(stmt.selectStmt, prog.ColNames)
	srcCursor := c.allocCursor()
	if live {
		if prog.Correlated {
			// Unreachable by construction (compileSubProgram was given no
			// enclosing compiler, so nothing outer can have been bound), but a
			// correlated derived source is materialized by execWithParent
			// rather than runSubOnce (vdbe.go's OpOpenDerived arm), which is
			// the one path liveLower does not sit on -- so it would silently
			// read the FROZEN twin. Declined rather than answered from the
			// wrong image; the shape declines, i.e. the status quo.
			return nil, fmt.Errorf("%w: correlated live source SELECT", errVDBEUnsupported)
		}
		// LiveTrig beside LiveSource, always. liveLower re-lowers LiveSource
		// under trigOnlyOuter(LiveTrig), so a program that records one without
		// the other is re-lowered with NOTHING to resolve NEW./OLD. against --
		// which is how "INSERT INTO log SELECT 'a', (SELECT new.a)" in a
		// trigger body reported "no such table: new" while the same subquery
		// written in a body UPDATE's SET list compiled. Three sites set
		// LiveSource; only compileLiveSubProgram was setting the context.
		prog.LiveSource, prog.LiveTrig = stmt.selectStmt, trig
		prog.LiveNQLKnown = true // at the INSERT's nQueryLoop, 0: see srcOuter above
	}
	src := &derivedSource{prog: prog, tbl: &resolvedTable{cols: srcCols, ipkIndex: -1}, pager: pager}
	c.emit(Instruction{Op: OpOpenDerived, P1: srcCursor, P2: c.allocSub(), P4: src})

	// See the identical block in compileInsertStmt: the victim delete's own
	// DELETE trigger programs, compiled rather than declined -- and only when
	// some constraint of this statement can resolve to OE_Replace at all.
	var delBefore, delAfter *triggerFirePlan
	if db.replaceResolutionCoded(tbl, stmt, slotOf, rowidSlot) {
		var derr error
		delBefore, delAfter, derr = db.compileReplaceVictimDeletePlans(tbl, stmt.table, trigMemo(trig))
		if derr != nil {
			return nil, derr
		}
	}
	plan := &insertPlan{tbl: tbl, displayName: stmt.table, colsGiven: stmt.cols != nil, colIdx: colIdx, action: stmt.orAction, explicitOr: stmt.explicitOr,
		replaceDelBefore: delBefore, replaceDelAfter: delAfter}
	// The source runs as a CO-ROUTINE, feeding the loop below one row at a
	// time, exactly when sqlite3Insert codes it as one: it always does
	// (insert.c:1140-1156) and falls back to a temp table only for
	// "pTrigger || readsTable(pParse, iDb, pTab)" (insert.c:1167-1169). pTrigger
	// counts the synthetic RETURNING trigger (trigger.c:72-81) and, here, the
	// DELETE programs a REPLACE victim fires. A trigger body's own INSERT keeps
	// the materialized route, whose live re-lowering sits on runSubOnce.
	if trig == nil && !live && beforePlan == nil && firePlan == nil && delBefore == nil && delAfter == nil &&
		stmt.returning == nil && !programReadsTable(prog, tbl.name) {
		src.stream, src.sinkPlan = streamAlways, plan
	}
	recReg := c.allocRec()
	rowidReg := c.allocN(1)
	base := c.allocN(len(tbl.cols))
	valReg := c.allocN(want) // one register per SELECT output column

	rewind := c.emit(Instruction{Op: OpRewind, P1: srcCursor})
	loopTop := c.here()
	if xferEligible {
		// Source and dest column indexes coincide here (eligibility matched the
		// generated flags), so read srcCursor at dest index i, skipping generated
		// columns, in insertSlotMapN's order.
		k := 0
		for i, col := range tbl.cols {
			if col.IsGenerated() {
				continue
			}
			c.emit(Instruction{Op: OpColumn, P1: srcCursor, P2: i, P3: valReg + k})
			k++
		}
	} else {
		for i := 0; i < want; i++ {
			c.emit(Instruction{Op: OpColumn, P1: srcCursor, P2: i, P3: valReg + i})
		}
	}
	regOf := make([]int, want)
	for i := range regOf {
		regOf[i] = valReg + i
	}
	var colNames []string
	// The per-row copies of the target's own trigger plans. ONE copy each for
	// the whole loop, not one per row: unlike the VALUES source -- where every
	// tuple is a separately emitted row body with its own end address -- this
	// is a single body executed per source row, so there is exactly one row end
	// to patch. That is the same reason plan itself needs no per-row copy here.
	var beforeCopy, afterCopy *triggerFirePlan
	// The BEFORE program is emitted inside emitInsertRowBody at insert.c:1494's
	// position, after NEW.* is built and before rowid allocation and constraint
	// checks, as for VALUES. One copy for the loop body.
	c.insertBeforeFire = nil
	if beforePlan != nil {
		c.insertBeforeFire = func() error {
			bp := *beforePlan
			beforeCopy = &bp
			c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: c.beforeInsertRowid, P3: -1, P4: &bp})
			return nil
		}
	}
	insertTail := func() error {
		if (beforePlan != nil || firePlan != nil) && tbl.ipkIndex >= 0 {
			// ...and the REAL rowid from here on: the stored record and the
			// AFTER program both see it.
			c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: base + tbl.ipkIndex})
		}
		if stmt.upsert != nil {
			// Upsert over a SELECT source: the same tail as VALUES, run per source row
			// (insert.c:1569). OpUpsertFind probes the live store, so a source row can
			// conflict with one an earlier source row inserted. BEFORE already fired;
			// AFTER and RETURNING are emitted by emitUpsertTail per arm, since this loop's
			// own RETURNING below is unreachable for an upsert.
			var ap *triggerFirePlan
			if firePlan != nil {
				cp := *firePlan
				ap = &cp
				afterCopy = ap
			}
			names, uerr := c.emitUpsertTail(db, tbl, stmt, plan, base, rowidReg, recReg, ap, returningSitePerRow)
			if uerr != nil {
				return uerr
			}
			if names != nil {
				colNames = names
			}
			return nil
		}
		c.emit(Instruction{Op: OpMakeRecord, P1: base, P2: len(tbl.cols), P3: recReg})
		c.emit(Instruction{Op: OpInsert, P1: 0, P2: recReg, P3: rowidReg, P4: plan})
		if firePlan != nil {
			ap := *firePlan
			afterCopy = &ap
			c.emit(Instruction{Op: OpFireTriggers, P1: base, P2: rowidReg, P3: -1, P4: &ap})
		}
		if stmt.returning != nil {
			// RETURNING inside the loop right after OpInsert (insert.c:1604), reading the
			// post-insert image and assigned rowid. One emission for the single loop body.
			names, rerr := c.emitReturning(db, tbl, stmt.returning, rowRegsFor(base, len(tbl.cols)), rowidReg, returningSitePerRow)
			if rerr != nil {
				return rerr
			}
			colNames = names
		}
		return nil
	}
	if err := c.emitInsertRowBody(db, tbl, stmt, plan, slotOf, rowidSlot, regOf, base, rowidReg, recReg, insertTail); err != nil {
		return nil, err
	}
	// The row's END -- SQLite's single endOfLoop (insert.c:1613), which is also
	// where emitInsertRowBody just patched its own NOT NULL/CHECK OE_Ignore
	// skips (nothing is emitted between the two). A UNIQUE/rowid conflict whose
	// effective action is IGNORE jumps here, past the RETURNING emitted above,
	// so it produces no result row -- see insertPlan.skipAddr.
	plan.skipAddr = c.here()
	// The same label for the trigger plans: a body's RAISE(IGNORE) abandons
	// THIS source row and the scan advances, which is what the row-level path's
	// "if isRaiseIgnore(ferr) { continue }" did.
	for _, fp := range []*triggerFirePlan{beforeCopy, afterCopy} {
		if fp != nil {
			fp.skipAddr = plan.skipAddr
		}
	}
	c.emit(Instruction{Op: OpNext, P1: srcCursor, P2: loopTop})
	c.patch(rewind, c.here())

	c.emit(Instruction{Op: OpHalt})

	// WritePager declares the frozen source snapshot so cachedWriteProgram refuses
	// to cache this program (else repeated "INSERT INTO ev SELECT y FROM ev" would
	// re-read its first snapshot). A live source has no frozen view and leaves it
	// nil, or programReadsFrozenSnapshot would decline the body.
	progPager := pager
	if live {
		progPager = nil
	}
	// The compile-time half of letting the source run unbounded
	// (insert_stream_bounded.go): a co-routine source, and a program that does
	// nothing but build and store rows from it.
	if src.stream == streamAlways {
		plan.appendArmable = insertProgramArmable(c.insns, tbl, plan, src, srcCursor)
	}
	return &Program{Insns: c.insns, NReg: c.nReg, NRecRegs: c.nRec, NCursors: c.nCursor, NSubCache: c.nSub, ColNames: colNames, WritePager: progPager}, nil
}

// emitUpdateNotNull emits NOT NULL checks for the columns the SET list assigns
// (insert.c:1979: unchanged columns are not checked), at update.c:1030's
// position below the BEFORE fire. The per-column rule is the INSERT one
// (insert.c:2007): REPLACE substitutes the DEFAULT and re-checks as ABORT,
// IGNORE jumps to ignoreDest, anything else halts.
func (c *compiler) emitUpdateNotNull(tbl *tableMeta, stmt *updateStmt, colIdx []int, rowBase int, skips *[]int) error {
	for _, idx := range colIdx {
		if idx == noColumnRowidTarget || idx == tbl.ipkIndex || !tbl.cols[idx].NotNull {
			continue
		}
		msg := fmt.Sprintf("engine: UPDATE %s: NOT NULL constraint failed: %s.%s", stmt.table, tbl.name, tbl.cols[idx].Name)
		switch act := effectiveNotNullAction(tbl, idx, stmt.orAction, stmt.explicitOr); act {
		case conflictIgnore:
			*skips = append(*skips, c.emit(Instruction{Op: OpIsNull, P1: rowBase + idx}))
		case conflictReplace:
			col := tbl.cols[idx]
			if col.HasDefault && !col.DefaultKnown && col.DefaultDeferred == nil {
				// See emitInsertRowBody's identical decline: a DEFAULT this
				// compiler can neither fold to a constant nor re-emit per
				// row (random()/randomblob()) leaves nothing to substitute.
				return fmt.Errorf("%w: UPDATE %s: NOT NULL ON CONFLICT REPLACE substituting a non-constant DEFAULT for column %s", errVDBEUnsupported, stmt.table, col.Name)
			}
			if col.HasDefault {
				notNull := c.emit(Instruction{Op: OpNotNull, P1: rowBase + idx})
				if col.DefaultKnown {
					c.emit(Instruction{Op: OpSCopy, P1: c.compileLiteral(col.DefaultValue), P2: rowBase + idx})
				} else {
					reg, derr := c.compileExpr(col.DefaultDeferred)
					if derr != nil {
						return derr
					}
					c.emit(Instruction{Op: OpSCopy, P1: reg, P2: rowBase + idx})
				}
				c.emit(Instruction{Op: OpAffinity, P1: rowBase + idx, P4: col.Aff})
				c.patch(notNull, c.here())
			}
			// REPLACE demoted to ABORT for the re-check: reached with the
			// substituted DEFAULT above, or directly when there was none.
			c.emit(Instruction{Op: OpHaltIfNull, P1: int(conflictAbort), P3: rowBase + idx, P4: msg})
		default: // ABORT / FAIL / ROLLBACK
			c.emit(Instruction{Op: OpHaltIfNull, P1: int(act), P3: rowBase + idx, P4: msg})
		}
	}
	return nil
}

// emitInsertRowBody emits the per-row instruction sequence shared by the VALUES
// and SELECT sources: place the row's values into their column slots, apply
// affinity, enforce NOT NULL and CHECK, choose the rowid, and store the record.
// regOf[i] is the register holding source value i.
func (c *compiler) emitInsertRowBody(db *DB, tbl *tableMeta, stmt *insertStmt, plan *insertPlan, slotOf []int, rowidSlot int, regOf []int, base, rowidReg, recReg int, tail func() error) error {
	// skips collects every "this row is IGNOREd, skip it" jump; they are patched
	// to the instruction right after OpInsert (the next VALUES tuple, or the
	// scan's OpNext). A UNIQUE/rowid conflict under IGNORE is handled inside
	// OpInsert (it stores nothing); a NOT NULL / CHECK / non-integer-rowid
	// violation under IGNORE is a compile-time skip emitted here.
	var skips []int
	// SQLite's nSeenReplace, as a compile-time bool: did the NOT NULL loop
	// below emit a DEFAULT substitution for any column? It gates the second
	// OpComputeGenerated exactly as "nSeenReplace>0" gates the C's second
	// sqlite3ComputeGeneratedColumns (insert.c:2049-2055).
	replaceSubstituted := false

	for i := range tbl.cols {
		c.emit(Instruction{Op: OpNull, P2: base + i})
	}
	for col, srcIdx := range slotOf {
		if srcIdx >= 0 {
			c.emit(Instruction{Op: OpSCopy, P1: regOf[srcIdx], P2: base + col})
		}
	}
	// Unnamed columns take their declared DEFAULT, before OpAffinity (C
	// coerces a default like any value) and before OpComputeGenerated. The IPK is
	// skipped: C ignores a DEFAULT on the rowid alias. Unfoldable defaults were
	// already declined by insertRejectOmittedDefaults.
	for i, col := range tbl.cols {
		if i == tbl.ipkIndex || !col.HasDefault || slotOf[i] >= 0 || col.IsGenerated() {
			continue
		}
		switch {
		case col.DefaultKnown:
			c.emit(Instruction{Op: OpSCopy, P1: c.compileLiteral(col.DefaultValue), P2: base + i})
		case col.DefaultDeferred != nil:
			// A clock-reading DEFAULT (columnInfo.DefaultDeferred), COMPILED
			// rather than folded to a literal here: C SQLite evaluates a
			// DEFAULT per INSERT, and a literal would freeze the reading into
			// the program -- which a cached write plan would then hand to
			// every later statement as well.
			reg, derr := c.compileExpr(col.DefaultDeferred)
			if derr != nil {
				return derr
			}
			c.emit(Instruction{Op: OpSCopy, P1: reg, P2: base + i})
		}
	}
	for i, col := range tbl.cols {
		c.emit(Instruction{Op: OpAffinity, P1: base + i, P4: col.Aff})
	}
	// Generated columns are derived once the supplied values are in place and
	// affinity-coerced, and BEFORE the NOT NULL / CHECK enforcement and
	// OpMakeRecord below -- so a NOT NULL or CHECK over a generated column
	// tests its real value, and a STORED column's value reaches the record.
	if hasGeneratedCols(tbl.cols) {
		c.emit(Instruction{Op: OpComputeGenerated, P1: base, P4: tbl})
	}
	// A STRICT table's datatype check on the NEW.* image, ahead of a BEFORE
	// program only: "if( !isView ){ sqlite3TableAffinity(v, pTab,
	// regCols+1); }" (insert.c:1487-1492) sits inside "if( tmask &
	// TRIGGER_BEFORE )" (insert.c:1443). Without one, the only check is the
	// one below the NOT NULL loop.
	if tbl.strict && c.insertBeforeFire != nil {
		c.emit(Instruction{Op: OpTypeCheck, P1: base, P4: &typeCheckPlan{tbl: tbl, prefix: fmt.Sprintf("engine: INSERT into %s", stmt.table)}})
	}
	// A non-integer explicit rowid is always "datatype mismatch" and aborts the
	// statement, whatever the OR clause: C emits one-operand OP_MustBeInt
	// (insert.c:1534, 1466), which goes to abort_due_to_error (vdbe.c:2111) with
	// OE_Abort. See compiler.beforeInsertRowid: -1 unless the row supplies an
	// explicit rowid, which the BEFORE program then sees.
	trigRowid := c.allocN(1)
	c.beforeInsertRowid = trigRowid
	c.emit(Instruction{Op: OpInteger, P1: -1, P2: trigRowid})
	// The candidate is an explicit rowid target or the IPK column: only NULL
	// allocates a fresh rowid; anything else must coerce losslessly to INTEGER
	// ('12' and 2.0 do; '2.5', 'abc', blobs do not).
	rowidSrc := -1
	switch {
	case rowidSlot >= 0:
		rowidSrc = regOf[rowidSlot]
	case tbl.ipkIndex >= 0 && slotOf[tbl.ipkIndex] >= 0:
		rowidSrc = base + tbl.ipkIndex
	}
	// Part one: the rowid the BEFORE program sees, -1 unless explicit
	// (insert.c:1452-1467). OpMustBeInt stays ahead of the fire, as in C.
	if rowidSrc >= 0 {
		c.emit(Instruction{Op: OpSCopy, P1: rowidSrc, P2: rowidReg})
		isNull := c.emit(Instruction{Op: OpIsNull, P1: rowidReg})
		c.emit(Instruction{Op: OpMustBeInt, P1: rowidReg, P3: int(conflictAbort)}) // P2==0: abort on non-int
		c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: trigRowid})              // explicit rowid: visible to BEFORE
		c.patch(isNull, c.here())
	}
	// NEW.* for the BEFORE program, then the program. The rowid slot holds the
	// -1/explicit value and generated columns are re-derived from it
	// (insert.c:1473-1483). Nothing is emitted here without a BEFORE plan.
	if c.insertBeforeFire != nil {
		if tbl.ipkIndex >= 0 {
			c.emit(Instruction{Op: OpSCopy, P1: trigRowid, P2: base + tbl.ipkIndex})
		}
		if hasGeneratedCols(tbl.cols) {
			c.emit(Instruction{Op: OpComputeGenerated, P1: base, P4: tbl})
		}
		if ferr := c.insertBeforeFire(); ferr != nil {
			return ferr
		}
	}
	// Part two: the rowid the row keeps, allocated only now (insert.c:1531-1540).
	// Allocating before the fire would reserve it while the BEFORE program ran, so
	// a body inserting into the same table would collide.
	if rowidSrc >= 0 {
		notNull := c.emit(Instruction{Op: OpNotNull, P1: rowidReg})
		c.emit(Instruction{Op: OpNewRowid, P2: rowidReg, P4: tbl})
		c.patch(notNull, c.here())
	} else {
		c.emit(Instruction{Op: OpNewRowid, P2: rowidReg, P4: tbl})
	}
	if tbl.autoIncrement {
		c.emit(Instruction{Op: OpMemMax, P1: rowidReg, P4: tbl}) // autoIncStep, insert.c:1542
	}
	// Generated columns again, over the kept rowid: C's second pass
	// (insert.c:1552-1560). Without it AFTER programs and CHECKs see a column
	// derived from a NULL rowid. Only emitted for tables with both. STRICT
	// OpTypeCheck stays ahead of the fire, as in C (insert.c:1487); its position is
	// unobservable since it always aborts.
	if hasGeneratedCols(tbl.cols) && tbl.ipkIndex >= 0 {
		c.emit(Instruction{Op: OpSCopy, P1: rowidReg, P2: base + tbl.ipkIndex})
		c.emit(Instruction{Op: OpComputeGenerated, P1: base, P4: tbl})
	}
	// NOT NULL with an effective REPLACE action substitutes the column's DEFAULT,
	// before generated columns are recomputed, so a generated column sees the
	// substituted value (gencol1.test 7.20).
	for i := range tbl.cols {
		if i == tbl.ipkIndex || !tbl.cols[i].NotNull || tbl.cols[i].IsGenerated() {
			continue
		}
		if !tbl.cols[i].HasDefault || (!tbl.cols[i].DefaultKnown && tbl.cols[i].DefaultDeferred == nil) {
			continue
		}
		if effectiveNotNullAction(tbl, i, plan.action, plan.explicitOr) != conflictReplace {
			continue
		}
		replaceSubstituted = true
		isNull := c.emit(Instruction{Op: OpIsNull, P1: base + i})
		notNull := c.emit(Instruction{Op: OpGoto})
		c.patch(isNull, c.here())
		if tbl.cols[i].DefaultKnown {
			c.emit(Instruction{Op: OpSCopy, P1: c.compileLiteral(tbl.cols[i].DefaultValue), P2: base + i})
		} else {
			// A clock-reading DEFAULT is re-emitted here, as C's OE_Replace arm re-emits any
			// DEFAULT (insert.c:2008). The OpIsNull guard skips it when the earlier fill
			// left a value, and such defaults are replayable.
			reg, derr := c.compileExpr(tbl.cols[i].DefaultDeferred)
			if derr != nil {
				return derr
			}
			c.emit(Instruction{Op: OpSCopy, P1: reg, P2: base + i})
		}
		// Apply the column's affinity to the substituted DEFAULT: C copies it raw
		// (insert.c:2013) and its whole-row affinity pass runs after this loop, while
		// ours ran before. Otherwise DEFAULT '7' in an INT column stays TEXT.
		c.emit(Instruction{Op: OpAffinity, P1: base + i, P4: tbl.cols[i].Aff})
		c.patch(notNull, c.here())
	}
	// Recompute generated columns when a REPLACE substitution was emitted, C's
	// nSeenReplace pass (insert.c:2049-2055), decided at compile time here too.
	if replaceSubstituted && hasGeneratedCols(tbl.cols) {
		c.emit(Instruction{Op: OpComputeGenerated, P1: base, P4: tbl})
	}
	for i, col := range tbl.cols {
		if i == tbl.ipkIndex || !col.NotNull {
			continue
		}
		switch effectiveNotNullAction(tbl, i, plan.action, plan.explicitOr) {
		case conflictIgnore:
			skips = append(skips, c.emit(Instruction{Op: OpIsNull, P1: base + i}))
		case conflictReplace:
			// REPLACE substitutes the DEFAULT and re-checks NOT NULL as ABORT, so a NULL or
			// missing DEFAULT still fails.
			if tbl.cols[i].HasDefault && !tbl.cols[i].DefaultKnown && tbl.cols[i].DefaultDeferred == nil {
				// A DEFAULT this compiler could neither fold to a constant
				// (DefaultKnown) nor defer-compile per row (DefaultDeferred)
				// had no value to substitute above -- e.g. random()/
				// randomblob(), whose output can never match an
				// independently-seeded oracle. Still declined, rather than
				// substituting something else.
				return fmt.Errorf("engine: unsupported: INSERT into %s: NOT NULL ON CONFLICT REPLACE substituting a non-constant DEFAULT for column %s is not supported by this write path", stmt.table, tbl.cols[i].Name)
			}
			// REPLACE demoted to ABORT for the re-check: reached with the
			// substituted DEFAULT above, or directly when there was none.
			c.emit(Instruction{Op: OpHaltIfNull, P1: int(conflictAbort), P3: base + i,
				P4: fmt.Sprintf("engine: INSERT into %s: NOT NULL constraint failed: %s.%s", stmt.table, tbl.name, tbl.cols[i].Name)})
		default: // ABORT / FAIL / ROLLBACK
			c.emit(Instruction{Op: OpHaltIfNull, P1: int(effectiveNotNullAction(tbl, i, plan.action, plan.explicitOr)), P3: base + i,
				P4: fmt.Sprintf("engine: INSERT into %s: NOT NULL constraint failed: %s.%s", stmt.table, tbl.name, tbl.cols[i].Name)})
		}
	}
	// STRICT type check after the NOT NULL loop, before the first CHECK
	// (bAffinityDone, insert.c:2079/2407/2716; NOT NULL precedes at :1959). The
	// order decides which error is reported and whether OR IGNORE skips silently.
	if tbl.strict {
		c.emit(Instruction{Op: OpTypeCheck, P1: base, P4: &typeCheckPlan{tbl: tbl, prefix: fmt.Sprintf("engine: INSERT into %s", stmt.table)}})
	}
	// The CHECK constraints, which move WITH the rowid because they read it:
	// sqlite3GenerateConstraintChecks runs at insert.c:1569, past both the fire
	// and the allocation.
	checkSkips, err := c.emitCheckConstraintsAction(db, tbl, base, rowidReg, rowidReg, fmt.Sprintf("engine: INSERT into %s", stmt.table), plan.action, plan.explicitOr, nil)
	if err != nil {
		return err
	}
	skips = append(skips, checkSkips...)
	if terr := tail(); terr != nil {
		return terr
	}
	for _, a := range skips {
		c.patch(a, c.here())
	}
	return nil
}

// insertRejectOmittedDefaults reports the same error the row builder always has
// (checkNotNullAndDefault) for an omitted column whose DEFAULT clause this
// write path declines to reproduce -- now only random()/randomblob(), whose
// output can never match an independently-seeded oracle, and anything the
// default folder cannot evaluate. A FOLDED default (DefaultKnown) and a DEFERRED one
// (DefaultDeferred, a clock reading) are both emitted per row by
// emitInsertRowBody instead.
func insertRejectOmittedDefaults(tbl *tableMeta, stmt *insertStmt, slotOf []int) error {
	for i, col := range tbl.cols {
		if i == tbl.ipkIndex {
			continue
		}
		if slotOf[i] < 0 && col.HasDefault && !col.DefaultKnown && col.DefaultDeferred == nil {
			return fmt.Errorf("engine: INSERT into %s: DEFAULT column values are not supported by this write path (column %s omitted)", stmt.table, col.Name)
		}
	}
	return nil
}

// undoEntry is one journal entry: a row-store mutation's inverse -- drop a row
// it added, put back a row it replaced or removed, or both for an update --
// or, for anything else, a function.
type undoEntry struct {
	fn        func()
	tbl       *tableMeta
	dropRid   uint64
	putRid    uint64
	putVals   []Value
	drop, put bool
	// dropMore is how many further consecutive rowids, after dropRid, this
	// entry drops: an INSERT of rowids n, n+1, n+2 ... into one table is one
	// entry, not one per row.
	dropMore uint64
}

func (e *undoEntry) run() {
	if e.fn != nil {
		e.fn()
		return
	}
	if e.drop {
		for k := uint64(0); k <= e.dropMore; k++ {
			e.tbl.dropRow(e.dropRid + k)
		}
	}
	if e.put {
		e.tbl.putRow(e.putRid, e.putVals)
	}
}

func (wc *writeCtx) journalAppend(e undoEntry) {
	if wc.journal == nil {
		wc.journal = wc.journalBuf[:0]
	}
	wc.journal = append(wc.journal, e)
}

// undoFn journals an arbitrary undo step.
func (wc *writeCtx) undoFn(fn func()) { wc.journalAppend(undoEntry{fn: fn}) }

// undoPut journals putting vals back at rid.
func (wc *writeCtx) undoPut(tbl *tableMeta, rid uint64, vals []Value) {
	wc.journalAppend(undoEntry{tbl: tbl, putRid: rid, putVals: vals, put: true})
}

// undoDrop journals dropping the row at rid, extending the last entry when it
// drops the rowid just before this one in the same table. Drops of distinct
// rowids undo in any order, so a run is exact.
func (wc *writeCtx) undoDrop(tbl *tableMeta, rid uint64) {
	if n := len(wc.journal); n > 0 {
		if l := &wc.journal[n-1]; l.fn == nil && l.drop && !l.put && l.tbl == tbl && l.dropRid+l.dropMore+1 == rid && rid != 0 {
			l.dropMore++
			return
		}
	}
	wc.journalAppend(undoEntry{tbl: tbl, dropRid: rid, drop: true})
}

// undoMove journals an update's inverse: drop the row at newRid, then put
// oldVals back at oldRid.
func (wc *writeCtx) undoMove(tbl *tableMeta, newRid, oldRid uint64, oldVals []Value) {
	wc.journalAppend(undoEntry{tbl: tbl, dropRid: newRid, drop: true, putRid: oldRid, putVals: oldVals, put: true})
}
