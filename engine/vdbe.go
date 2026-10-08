// This file implements the VDBE execution loop: a register machine that runs a
// compiled Program (vdbe_op.go, produced by vdbe_codegen.go) and yields result
// rows. Opcode bodies reuse the package's value primitives (compareValues,
// applyAffinityToValue, castValueEnc, callScalarFuncEnc, likeMatch, isTruthy,
// ...) for every semantic decision; the VM only sequences them -- the split C
// keeps between vdbe.c and vdbemem.c/func.c.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"unsafe"
)

// vdbe holds one execution's mutable state: the register file, the open
// table cursors (for the cursor opcodes -- vdbe_cursor.go), the record
// registers and sorter cursors (for the ORDER BY sorter opcodes --
// vdbe_sorter.go), and the bound parameters (for OpVariable). A fresh vdbe is
// used per run; registers, cursors, and sorters are not shared across
// executions.
type vdbe struct {
	regs         []Value
	jx           *vmJIT // native code for the program run() is given, when it has some
	jitEnters    int    // entries into jx, which the fuel test counts
	cursors      []*vdbeCursor
	recRegs      [][]Value
	sorters      []*vdbeSorter
	distinctSets []*vdbeDistinctSet
	pager        *ReadOnlyPager
	params       []Value

	// Rows materialized by a segment fast path that produces a row SET rather
	// than one value (OpSegOrderLimit), handed out one at a time by
	// OpSegEmitRow.
	segRows [][]Value
	segRow  int

	// outer is the enclosing query's evalCtx when this program is executing a
	// correlated subquery (threaded in by execOuter); nil for a top-level run.
	// The aggregate opcodes thread it onto every evalCtx they build so a
	// subquery inside an aggregate item still resolves an outer reference.
	outer *evalCtx

	// parent is the enclosing execution's vdbe when this program is a
	// CORRELATED subquery's body run in bytecode (set by execWithParent); nil
	// for a top-level run and for an uncorrelated subquery (which runs
	// standalone via runSubOnce). It is the frame OpOuterColumn/OpOuterRowid
	// walk outward -- P5 levels up -- to read the enclosing row's live cursors,
	// which is what makes a correlated subquery see the current outer row. See
	// the OpOuterColumn opcode doc comment (vdbe_op.go).
	parent *vdbe

	// aggAccs is the live per-group accumulator state for the aggregate / GROUP
	// BY opcodes (vdbe_agg.go): (re)initialized by OpAggReset, advanced by
	// OpAggStep, read by OpAggResult. nil for a program with no aggregates.
	aggAccs *aggAccumulators

	// colMasks is the per-cursor column mask cursorColumnMasks derived from this
	// program, memoized on first OpOpenRead; colMasksDone distinguishes "not
	// derived yet" from the nil the derivation returns when it declines to
	// narrow anything.
	colMasks     []columnMask
	colMasksDone bool

	// aggRowBuf/aggRowidBuf are gatherCursorRow's reusable per-row buffers --
	// see its doc comment for why reuse is safe here.
	aggRowBuf   []Value
	aggRowidBuf []Value
	// A POINTER, not the struct: an evalCtx is 304 bytes, a third of this whole
	// machine, and only the aggregate opcodes ever touch it. Inline it and every
	// point lookup pays for a field it never reads -- and a machine is allocated
	// per statement AND per nested subquery.
	aggCtx *evalCtx

	// fnArgBuf is OpFunction's reusable argument vector -- sqlite3_context.argv,
	// which C allocates once per opcode (P4_FUNCCTX, vdbe.c:8866). Allocating per
	// call cost one heap allocation per row per scalar call.
	//
	// It is a copy of the registers rather than C's pointers because
	// coerceUTF16BlobArgs rewrites its arguments in place. Reuse is safe: nothing
	// retains the slice (a returned Value aliasing an argument's bytes aliases the
	// register's), and OpFunction is not re-entrant -- no scalar function runs a
	// Program.
	fnArgBuf []Value

	// aggVM is the register machine an aggregate item's compiled result
	// expression runs on (aggItemVM, vdbe_agg_item_codegen.go): built lazily,
	// reused across every item of every group. It carries a register file, this
	// machine's pager and its bound parameters and nothing else.
	aggVM *vdbe

	// aggKeyBuf is hashAggStep's reusable group-key encoding buffer -- see its
	// doc comment for why handing back the same bytes every row is safe.
	aggKeyBuf []byte

	// recChunk is the unused tail of the current record arena, and recChunkSize
	// the size the last chunk was cut to -- see recAlloc.
	recChunk     []Value
	recChunkSize int

	// groupBatch is the collected-entries batch for a GROUP BY + DISTINCT
	// query's OpGroupBatchAppend/OpGroupBatchFinal opcodes (vdbe_group_
	// distinct.go): one entry per HAVING-passing group, appended during the
	// sorted drain and resolved all at once by OpGroupBatchFinal. nil for a
	// program with no GROUP BY + DISTINCT.
	groupBatch [][]Value

	// windowBatch is the collected [cols.., rowids..] payload of every row a
	// WINDOW-function query's scan produced (OpWindowAppend), resolved all at
	// once by OpWindowFinal (vdbe_window.go). nil for a program with no
	// window function.
	windowBatch [][]Value

	// hashAgg is the live hash-aggregate state for a GROUP BY program compiled
	// by the hash fast path (compileScanGroupByHash, vdbe_agg_codegen.go;
	// runtime in vdbe_hashagg.go): one bucket per distinct group key, built up
	// by OpHashAggStep during the scan and drained in group-key order by
	// OpHashAggSort/OpHashAggData/OpHashAggNext. nil for a program compiled by
	// the sort-based GROUP BY path (or with no GROUP BY at all).
	hashAgg *hashAggState

	// subCache holds the run-once cache slots for the subquery opcodes
	// (OpSubquery/OpExists/OpInSub/OpRowSub, vdbe_op.go): subCache[i] is filled the
	// first time a subquery opcode with slot i executes and reused thereafter.
	// len == prog.NSubCache; nil for a program with no compiled subquery.
	subCache []subCacheEntry

	// wctx is the write-mode state (the *DB write handle plus the per-statement
	// mutation bookkeeping) for the WRITE opcodes (OpInsertRow/OpDeleteRow/...,
	// see vdbe_write.go). It is nil for every read execution (execOuter) -- a
	// read program never contains a write opcode -- and non-nil only for a
	// write run (runWrite). Carrying it here (rather than a separate write VM)
	// lets the write path reuse this file's entire expression/cursor opcode
	// switch verbatim for INSERT VALUES / SET / WHERE evaluation.
	wctx *writeCtx

	// stmtImage is the database as it stood when this TRIGGER BODY statement
	// began, for its WHERE's subqueries (Program.LiveStmtImage). nil unless the
	// body program needs it.
	stmtImage *ReadOnlyPager
	// firstImages is each LiveFirstImage sub-program's image, taken at its
	// first run in this frame -- OP_Once's lifetime, a trigger firing being a
	// frame of its own.
	firstImages map[*Program]*ReadOnlyPager

	// vtabWrite is the row set a compiled virtual-table UPDATE/DELETE's scan
	// has selected so far (OpVWriteRow), replayed into the module by the single
	// OpVWrite that ends the loop. It is SQLite's own ephemeral table
	// (update.c:1320-1327) / RowSet (delete.c:582) under another name -- both
	// of which likewise live only for the length of one statement, which is why
	// this is per-execution state on the machine rather than anything the
	// (cacheable) Program carries. nil for every other program.
	vtabWrite vtabWriteRowSet

	// vtabInsert is the row list a compiled "INSERT INTO <vtab> SELECT ..."
	// scan has collected so far (OpVInsertRow), replayed into the module by the
	// single OpVInsert that follows the loop. It is SQLite's srcTab ephemeral
	// table -- "OP_OpenEphemeral, srcTab, nColumn" then OP_MakeRecord/
	// OP_NewRowid/OP_Insert per yielded row (insert.c:1189-1193), read back by
	// the insertion loop's "OP_Column, srcTab, k" (insert.c:1424) -- under
	// another name, and like vtabWrite above it lives only for the length of
	// one statement. A LIST, not a map: an INSERT keeps its source's row ORDER
	// and its DUPLICATES, neither of which vtabWriteRowSet's rowid keying can
	// express. nil for every other program.
	vtabInsert [][]Value
	// schemaWrite is the same idea for a compiled DIRECT SCHEMA-CATALOG write
	// ("UPDATE/DELETE FROM sqlite_master" under PRAGMA writable_schema=ON):
	// the catalog row set its scan cursor was opened over, plus the rows
	// OpSchemaWriteRow has selected so far, replayed into the overlay by the
	// single OpSchemaWrite that ends the loop. Per-execution state on the
	// machine for vtabWrite's reason -- the Program is cacheable and this is
	// one statement's ephemeral table. nil for every other program.
	schemaWrite *schemaWriteState

	// trigOld/trigNew are the OLD/NEW row values a trigger body sub-program
	// reads via OpParam (set by OpFireTriggers before it runs the body). nil
	// outside a trigger body. The rowids are the OLD/NEW rowid pseudo-columns.
	trigOld, trigNew           []Value
	trigOldRowid, trigNewRowid int64

	// trigExcluded and trigReturn carry the other two pseudo-rows: an UPSERT's
	// "excluded" row (OpParam P1 == 2) and a RETURNING clause's affected row
	// (OpParam P1 == 3). OpPseudoRow sets them at the head of the owning block.
	//
	// They are separate from trigNew because they coexist (an upsert inside a
	// trigger body), and resolve.c gates the three on different things
	// (pParse->pTriggerTab at resolve.c:525, bReturning at :528, NC_UUpsert at
	// :547).
	trigExcluded      []Value
	trigExcludedRowid int64
	trigReturn        []Value
	trigReturnRowid   int64

	// caseSensitiveLike is the connection's "PRAGMA case_sensitive_like" for a
	// machine that has neither a pager nor a write context to read it from --
	// today only selfRowExpr.eval's pooled machine (vdbe_run.go). See
	// likeCaseSensitive below.
	caseSensitiveLike bool

	// recq is a recursive CTE's queue, for the one kind of program that has
	// one -- compileRecursiveCTE's (vdbe_recursive_cte.go), opened by
	// OpRecQueueOpen. nil for every other program.
	recq *recQueue

	// recCur is the recursive CTE row a recursive step reads through its
	// self-reference (OpOpenDerived's recSelf source). Set by OpRecQueuePop on
	// the queue's machine and copied onto every machine a step runs on, exactly
	// as trigOld/trigNew are (execOuterTrig, execWithParent).
	recCur *recCurrentRow

	// yield, resumePC and done make this machine a CO-ROUTINE: a program run
	// with yield set returns from run() at every OpResultRow, handing back that
	// one row, and the next run() call resumes at resumePC. done records that
	// the program reached its end. This is SQLite's OP_Yield between an
	// OP_InitCoroutine'd sub-select and its consumer (insert.c:1140-1156 for an
	// INSERT ... SELECT source, select.c:7266's fromClauseTermCanBeCoroutine
	// for a FROM-clause subquery), and it is what lets a source's rows reach
	// their consumer one at a time instead of all being built first. See
	// subStream (vdbe_stream.go). A machine without yield runs exactly as it
	// always has.
	yield    bool
	resumePC int
	done     bool

	// likePlan is the last LIKE pattern this machine analysed (likePlanFor).
	likePlan *likePlan

	// firstRow stops the run at its first result row, handing back one empty
	// row instead of a copy: all EXISTS asks is whether there is one.
	firstRow bool

	// accumChecked/accumulates memoize whether this machine's program holds an
	// opcode that gathers every row before producing any (programAccumulates),
	// which disqualifies its outermost derived source from streaming.
	accumChecked bool
	accumulates  bool

	// sink is the INSERT a streamed source's rows ultimately feed, shared down
	// the whole chain of co-routines that produce them. nil outside such a
	// chain. See streamSink.
	sink *streamSink
}

// runSub is the subquery opcodes' shared dispatcher: an uncorrelated subquery
// runs once and is cached (runSubOnce, keyed by slot), while a correlated one
// re-runs on every evaluation against the enclosing frame (execWithParent, no
// cache) so it observes the current outer row. Which of the two applies is a
// compile-time property of the sub-Program (whether it bound any outer-scope
// column -- see compileScalarSubquery, vdbe_codegen.go), carried to the opcode
// in p5Correlated / inSubPlan.correlated / rowSubPlan.correlated.
func (m *vdbe) runSub(correlated bool, slot int, prog *Program) ([][]Value, error) {
	if correlated {
		return prog.execWithParent(m)
	}
	return m.runSubOnce(slot, prog, m.pager)
}

// dbPager resolves a compile-time db index (dbIndexOf: 0 for this Program's
// pager, i+1 for an ATTACHed database) to its *ReadOnlyPager. Used by
// OpOpenRead and OpOpenDerived (a foreign-qualified view's body runs against
// its own pager).
//
// The index is baked in at compile time, while the attached set can change
// between runs. Cached programs are dropped when it does, but a stale index is
// still reported as an error rather than indexing off the slice.
func (m *vdbe) dbPager(dbIdx int) (*ReadOnlyPager, error) {
	if dbIdx == 0 {
		return m.pager, nil
	}
	if dbIdx-1 >= len(m.pager.attachedReaders) {
		return nil, fmt.Errorf("vdbe: attached database index %d is out of range (%d attached)", dbIdx, len(m.pager.attachedReaders))
	}
	return m.pager.attachedReaders[dbIdx-1].pager, nil
}

// runSubOnce runs prog (an uncorrelated subquery, derived table or view) at
// most once per execution, caching its rows in slot -- OP_Once plus
// materialize. pager is normally m.pager, but a foreign-qualified view's body
// (OpOpenDerived, derivedSource.dbIdx) runs against the attached pager it was
// compiled for (see dbPager, resolveViewSource).
func (m *vdbe) runSubOnce(slot int, prog *Program, pager *ReadOnlyPager) ([][]Value, error) {
	e := &m.subCache[slot]
	if e.done {
		return e.rows, nil
	}
	// A LIVE sub-program (a trigger's WHEN guard subquery, a trigger body
	// statement's read) is lowered again HERE, against the database as it
	// stands at this instant, rather than run against the image the firing
	// statement compiled over -- see vdbe_live_read.go. Below the cache check
	// on purpose: OP_Once's already-run bits live in the per-invocation
	// VdbeFrame (vdbe.c:7582), so the re-lowering is paid once per firing,
	// never once per row. A no-op (one nil compare) for every other program.
	prog, pager, err := m.liveLower(prog, pager)
	if err != nil {
		return nil, err
	}
	rows, err := prog.execTrig(pager, m.params, m)
	if err != nil {
		return nil, err
	}
	e.rows = rows
	e.done = true
	return rows, nil
}

// runVtabOnce is runSubOnce for a virtual-table row source (resolveVtabSource):
// materializeVtab is called directly, and its rowids are cached too, since a
// vtab cursor exposes a real rowid (openMaterializedCursor). pager is ds.tbl's
// owning pager; m.params supplies the current bound parameters for a
// table-valued function's arguments and pushed-down constraints, which is why
// this runs at execution time.
//
// ds.vtabTrig (the trigger context the source was resolved under) and m (whose
// trigNew/trigOld hold the firing row) let "json_each(NEW.x)" in a trigger body
// name the firing row.
func (m *vdbe) runVtabOnce(slot int, ds *derivedSource, pager *ReadOnlyPager) ([][]Value, []int64, error) {
	e := &m.subCache[slot]
	if e.done {
		return e.rows, e.rowids, nil
	}
	_, rows, rowids, err := pager.materializeVtab(*ds.vtab, m.params, m.outer, ds.vtabTrig, m, nil)
	if err != nil {
		return nil, nil, err
	}
	e.rows = rows
	e.rowids = rowids
	e.done = true
	return rows, rowids, nil
}

// runCTEOnce is runSubOnce for an ordinary CTE row source (resolveCTESource):
// resolveCTERows is called directly, with m.params, at execution time. A
// recursive CTE never reaches here; it opens a compiled queue program. A CTE
// exposes no rowid (openDerivedCursor).
func (m *vdbe) runCTEOnce(slot int, ds *derivedSource, pager *ReadOnlyPager) ([][]Value, error) {
	e := &m.subCache[slot]
	if e.done {
		return e.rows, nil
	}
	_, rows, err := pager.resolveCTERows(ds.cte.name, ds.cte.binding, m.params)
	if err != nil {
		return nil, err
	}
	e.rows = rows
	e.done = true
	return rows, nil
}

// gatherScopesRow builds a MATCH opcode's per-row evalCtx.vals -- one flat
// []Value spanning every table in scopes at its tableScope.offset -- from the
// current row of each backing cursor, as gatherCursorRow does for aggregates.
// A NullRow'd LEFT-join cursor contributes NULLs.
//
// colBases, when non-nil, is compileScope.colBase per scope: a member of a
// materialized parenthesized join group shares its cursor with the others, and
// cur.row is the group's full row, so colBases[i] locates this member's slice.
// Exactly len(ts.cols) values are copied, never len(cur.row), so a copy cannot
// overrun.
func (m *vdbe) gatherScopesRow(scopes []tableScope, cursors []int, colBases []int) ([]Value, error) {
	n := 0
	for _, ts := range scopes {
		if end := ts.offset + len(ts.cols); end > n {
			n = end
		}
	}
	row := make([]Value, n)
	for i, ts := range scopes {
		cur := m.cursors[cursors[i]]
		vals, err := cur.fullRow()
		if err != nil {
			return nil, err
		}
		cb := 0
		if colBases != nil {
			cb = colBases[i]
		}
		w := len(ts.cols)
		if cb+w > len(vals) {
			w = len(vals) - cb // defensive: never read past the shared row either
		}
		if w > 0 {
			copy(row[ts.offset:ts.offset+w], vals[cb:cb+w])
		}
	}
	return row, nil
}

// buildOuterEvalCtx reconstitutes the enclosing queries' evalCtx chain from
// their live frames, so an aggregate opcode's context can resolve a correlated
// column through evalCtx.outer. frames is Program.OuterFrames (index 0 = the
// immediate parent); parent is the invoking frame. The chain is built
// outermost first. A missing frame or closed cursor contributes nothing, so an
// unresolvable reference is a "no such column" error, never a wrong value.
func buildOuterEvalCtx(frames []outerFrameInfo, parent *vdbe) (*evalCtx, error) {
	if len(frames) == 0 || parent == nil {
		return nil, nil
	}
	// frames[i] belongs to the frame i steps ABOVE the sub-Program, i.e. parent
	// walked i times through .parent.
	at := make([]*vdbe, len(frames))
	fr := parent
	for i := range frames {
		at[i] = fr
		if fr == nil {
			continue
		}
		fr = fr.parent
	}
	var ctx *evalCtx
	for i := len(frames) - 1; i >= 0; i-- {
		f, m := frames[i], at[i]
		if m == nil {
			continue
		}
		vals, rowids, err := m.gatherFrameRow(f.scopes, f.cursors)
		if err != nil {
			return nil, err
		}
		ctx = &evalCtx{tables: f.scopes, vals: vals, rowids: rowids, outer: ctx, pager: m.pager, params: m.params}
	}
	return ctx, nil
}

// gatherFrameRow is gatherScopesRow plus the parallel per-scope rowid slice
// (the rowid/oid/_rowid_ pseudo-column resolveColumn reads), and tolerant of a
// cursor that is not open: buildOuterEvalCtx runs at an arbitrary point in the
// enclosing statement's execution, where gatherScopesRow's callers (a MATCH/
// fts opcode inside that statement's own scan body) can assume every cursor is
// live and this cannot.
func (m *vdbe) gatherFrameRow(scopes []tableScope, cursors []int) ([]Value, []Value, error) {
	n := 0
	for _, ts := range scopes {
		if end := ts.offset + len(ts.cols); end > n {
			n = end
		}
	}
	row := make([]Value, n)
	rowids := make([]Value, len(scopes))
	for i, ts := range scopes {
		if i >= len(cursors) || cursors[i] < 0 || cursors[i] >= len(m.cursors) {
			continue
		}
		cur := m.cursors[cursors[i]]
		if cur == nil {
			continue
		}
		vals, err := cur.fullRow()
		if err != nil {
			return nil, nil, err
		}
		end := ts.offset + len(vals)
		if end > n {
			end = n
		}
		if end > ts.offset {
			copy(row[ts.offset:end], vals)
		}
		if !cur.rowidNull {
			rowids[i] = Value{Typ: Int, I: int64(cur.rowid)}
		}
	}
	return row, rowids, nil
}

// inSubMembership is IN's three-valued logic against a materialized subquery
// result (first column only): an empty set makes IN false and NOT IN true even
// for a NULL X; otherwise matches and NULLs are tracked and combined by
// finalizeIn.
//
// aff is the combined comparison affinity (comparisonAffinity) and is applied
// to both operands, as OpEq does: TEXT x IN (SELECT intcol) compares
// numerically (subquery.test).
func inSubMembership(xs []Value, rows [][]Value, affs []affinity, colls []string, not bool, enc TextEncoding) Value {
	if len(rows) == 0 {
		// An EMPTY set is FALSE (and NOT IN TRUE) whatever the probe holds,
		// NULL components included -- verified for the row-value form too:
		// "(NULL,1) IN (SELECT 1,2 WHERE 0)" is 0 and its NOT IN is 1.
		return boolValue(not)
	}
	anyTrue, anyNull := false, false
	for _, row := range rows {
		// One row's row-value equality is "x[0]=row[0] AND x[1]=row[1] AND ...",
		// under three-valued AND: a definite FALSE on ANY column decides the
		// whole row FALSE even when another column compared NULL, so the scan
		// below stops at the first definite mismatch but keeps looking after a
		// NULL one. Confirmed against the oracle: "(1,NULL) IN (SELECT 3,4)" is
		// 0 (column 0 definitely differs), while "(1,NULL) IN (SELECT 1,2)" is
		// NULL and "(1,NULL) IN (SELECT 1,2 UNION ALL SELECT 3,4)" is NULL.
		eq, sawNull := true, false
		for i := range xs {
			lv, rv := xs[i], row[i]
			if lv.Typ == Null || rv.Typ == Null {
				sawNull = true
				continue
			}
			switch {
			case isNumericAffinity(affs[i]):
				lv = applyAffinityToValue(lv, affNumeric)
				rv = applyAffinityToValue(rv, affNumeric)
			case affs[i] == affText:
				lv = applyAffinityToValue(lv, affText)
				rv = applyAffinityToValue(rv, affText)
			}
			if compareValuesCollatedEnc(lv, rv, colls[i], enc) != 0 {
				eq = false
				break
			}
		}
		switch {
		case !eq: // a definite non-match: contributes nothing
		case sawNull:
			anyNull = true
		default:
			anyTrue = true
		}
	}
	return finalizeIn(anyTrue, anyNull, not)
}

// autoIndexOrderCursor permutes a materialized cursor's rows into the order
// SQLite's transient automatic index over the same table yields them in: key
// order, where the key is constructAutomaticIndex's (constrained columns, then
// covering columns, then rowid) and each column compares under its own
// collating sequence. See autoIndexKey (where_plan.go) for why one sort
// reproduces the seek's per-probe visit order, and OpAutoIndexOrder
// (vdbe_op.go) for the guarantee that only the ORDER changes.
//
// The rowid is the last key column, so no two rows can tie; the sort is stable
// anyway rather than relying on that.
func autoIndexOrderCursor(cur *vdbeCursor, key *autoIndexKey, enc TextEncoding) {
	// Both orders below compare rows by COLUMN, so a row store's deferred
	// fix-ups (vdbeCursor.lazyNorm) have to be applied first: a stored IPK slot
	// is NULL where the key wants the rowid.
	cur.normalizeRowsNow()
	if key != nil && key.multiOr != nil {
		// A WHERE_MULTI_OR loop's order is a CONCATENATION of one sub-scan per
		// disjunct, not a single b-tree key. See multiOrOrderCursor
		// (vdbe_cursor.go) and multiOrOrder (vdbe_op.go).
		multiOrOrderCursor(cur, key.multiOr, enc)
		return
	}
	if key == nil || len(key.cols) == 0 || len(cur.rowids) != len(cur.rows) || len(cur.rows) < 2 {
		return
	}
	at := func(i, kc int) Value {
		if kc < 0 {
			// The trailing XN_ROWID column. Compared as the signed integer
			// OpRowid reports, which is how the b-tree orders it.
			return Value{Typ: Int, I: int64(cur.rowids[i])}
		}
		if row := cur.rows[i]; kc < len(row) {
			return row[kc]
		}
		return Value{Typ: Null}
	}
	perm := make([]int, len(cur.rows))
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(x, y int) bool {
		a, b := perm[x], perm[y]
		for k, kc := range key.cols {
			c := compareValuesCollatedEnc(at(a, kc), at(b, kc), key.colls[k], enc)
			if c != 0 {
				if k < len(key.desc) && key.desc[k] {
					// A DESC key column: the b-tree stores it reversed, so the
					// walk visits its values from largest to smallest. NULLs,
					// which sort first ascending, therefore come LAST here --
					// which is exactly what negating the comparison gives.
					return c > 0
				}
				return c < 0
			}
		}
		return false
	})
	rows := make([][]Value, len(perm))
	rowids := make([]uint64, len(perm))
	for i, p := range perm {
		rows[i], rowids[i] = cur.rows[p], cur.rowids[p]
	}
	cur.rows, cur.rowids = rows, rowids
}

// rowSubCmpColumn compares ONE column position of a row-value comparison under
// that position's affinity and collating sequence, exactly as the scalar
// comparison opcode at that position would (the same three lines
// inSubMembership uses above, factored out so the two can never drift). ok is
// false when either side is NULL, i.e. when the position's own comparison is
// the UNKNOWN of three-valued logic rather than a definite ordering.
func rowSubCmpColumn(lv, rv Value, aff affinity, coll string, enc TextEncoding) (int, bool) {
	if lv.Typ == Null || rv.Typ == Null {
		return 0, false
	}
	switch {
	case isNumericAffinity(aff):
		lv = applyAffinityToValue(lv, affNumeric)
		rv = applyAffinityToValue(rv, affNumeric)
	case aff == affText:
		lv = applyAffinityToValue(lv, affText)
		rv = applyAffinityToValue(rv, affText)
	}
	return compareValuesCollatedEnc(lv, rv, coll, enc), true
}

// rowSubCompare evaluates a row-value comparison against a multi-column
// subquery -- "(a,b) <op> (SELECT x,y FROM t)" or mirrored (OpRowSub). probe
// holds the row value's elements and rows the subquery's result.
//
// Only the first row is consulted, and no rows means a row of all NULLs --
// exactly as a zero-row scalar subquery is NULL:
//
//	(1,2) =      (SELECT a,b FROM e) -> NULL    ==  (1,2) =      (NULL,NULL)
//	(1,2) IS     (SELECT a,b FROM e) -> 0       ==  (1,2) IS     (NULL,NULL)
//	(1,2) IS NOT (SELECT a,b FROM e) -> 1       ==  (1,2) IS NOT (NULL,NULL)
//
// The comparison is desugarRowCompare/desugarRowLex's equivalence, evaluated
// here because the subquery must run once:
//
//   - "=" / "IS": AND of per-column comparisons; "<>" / "IS NOT": OR of their
//     negations. A definite difference anywhere decides, even with a NULL
//     elsewhere. IS / IS NOT are NULL-safe and never yield NULL.
//   - "<" "<=" ">" ">=": lexicographic. The first unequal position decides; a
//     NULL reached first makes the result NULL; all-equal falls to whether the
//     operator admits equality.
func rowSubCompare(probe []Value, rows [][]Value, plan *rowSubPlan, enc TextEncoding) Value {
	sub := make([]Value, len(probe)) // the zero Value is NULL: the no-rows case
	if len(rows) > 0 {
		copy(sub, rows[0])
	}
	lhs, rhs := probe, sub
	if plan.subOnLeft {
		lhs, rhs = sub, probe
	}
	switch plan.op {
	case "<", "<=", ">", ">=":
		for i := range lhs {
			cmp, ok := rowSubCmpColumn(lhs[i], rhs[i], plan.affs[i], plan.colls[i], enc)
			if !ok {
				return Value{Typ: Null}
			}
			if cmp != 0 {
				wantLess := plan.op == "<" || plan.op == "<="
				return boolValue((cmp < 0) == wantLess)
			}
		}
		// Every position compared equal: only "<=" and ">=" are satisfied.
		return boolValue(plan.op == "<=" || plan.op == ">=")
	}
	nullSafe := plan.op == "IS" || plan.op == "IS NOT"
	// negated is the OR family: a definite per-column difference makes it TRUE
	// where the AND family makes it FALSE.
	negated := plan.op == "!=" || plan.op == "<>" || plan.op == "IS NOT"
	anyDiff, anyNull := false, false
	for i := range lhs {
		lv, rv := lhs[i], rhs[i]
		if nullSafe {
			switch {
			case lv.Typ == Null && rv.Typ == Null: // NULL IS NULL: equal
			case lv.Typ == Null || rv.Typ == Null:
				anyDiff = true
			default:
				if cmp, _ := rowSubCmpColumn(lv, rv, plan.affs[i], plan.colls[i], enc); cmp != 0 {
					anyDiff = true
				}
			}
			continue
		}
		cmp, ok := rowSubCmpColumn(lv, rv, plan.affs[i], plan.colls[i], enc)
		switch {
		case !ok:
			anyNull = true
		case cmp != 0:
			anyDiff = true
		}
	}
	switch {
	case anyDiff:
		return boolValue(negated)
	case anyNull: // never reachable for IS / IS NOT, which are NULL-safe
		return Value{Typ: Null}
	default:
		return boolValue(!negated)
	}
}

// exec runs prog to completion and returns the rows emitted by its
// OpResultRow instructions. A FROM-less SELECT emits at most one row, but the
// loop is written generally (any number of OpResultRow executions accumulate).
// pager is only consulted by the cursor opcodes (OpOpenRead); it may be nil
// for a program that never emits one (e.g. every FROM-less program compiled
// by compileSelectNoFrom).
func (prog *Program) exec(pager *ReadOnlyPager, params []Value) (rows [][]Value, err error) {
	return prog.execOuter(pager, nil, params)
}

// execTrig is exec for a LIVE sub-program lowered inside a trigger body: it
// seeds the fresh machine with the firing row so the OpParam reads that
// lowering emitted for NEW./OLD. read the same values the enclosing body does.
//
// A sub-program runs on its own machine (execOuter builds one carrying the
// pager and the params and nothing else), which is exactly why this has to be
// threaded rather than inherited. Everything else stays unshared on purpose --
// no cursors, no write context, no frame -- so this widens what a live
// sub-program can see by precisely the one row its compile was allowed to
// name. See vdbe_live_read.go's trigOnlyOuter.
func (prog *Program) execTrig(pager *ReadOnlyPager, params []Value, from *vdbe) (rows [][]Value, err error) {
	return prog.execOuterTrig(pager, nil, params, from)
}

// execCompoundBody runs a Compound-carrying Program (see Program.Compound's
// own doc comment, vdbe_op.go) by delegating straight to the compiled
// compoundProgram's own execOuter, discarding the column names it also returns
// (the caller already has prog.ColNames, computed identically by
// compileSubProgramCompound from the same first arm). This is the
// CURSOR-FRAME-LESS entry: a compound whose arms bound an outer CURSOR runs
// through execWithParent instead, which threads the live parent frame into
// every arm (compoundProgram.execFrom). outer is the live enclosing ROW, which
// is a different channel and is threaded here -- dropping it left an arm's
// aggregate evaluation (aggResult, vdbe_agg.go) with no view of the row the
// compound was being evaluated for.
func (prog *Program) execCompoundBody(pager *ReadOnlyPager, outer *evalCtx, params []Value, from *vdbe) (rows [][]Value, err error) {
	_, rows, err = prog.Compound.execFrom(pager, params, nil, outer, from)
	return rows, err
}

// likeCaseSensitive reports the connection's "PRAGMA case_sensitive_like"
// setting for this run, read from the pager SnapshotPager stamped it onto (the
// VDBE analogue of evalCtx.likeCaseSensitive). A nil pager -- a pager-free run
// that touches no connection -- defaults to false (case-insensitive), matching
// the engine's prior unconditional LIKE behavior.
func (m *vdbe) likeCaseSensitive() bool {
	if m == nil {
		return false
	}
	if m.pager != nil {
		return m.pager.caseSensitiveLike
	}
	// A WRITE program has no pager of its own, so the flag comes from the
	// session it is writing for. Without this, "INSERT INTO t VALUES('AbC')
	// RETURNING x LIKE 'ab%'" under "PRAGMA case_sensitive_like=ON" answered 1
	// where the 3.53.3 oracle answers 0 -- the compiled RETURNING is evaluated
	// by this machine, and it was reading a nil pager. The plain SELECT
	// spelling already agreed, which is the control that pins it here.
	if m.wctx != nil && m.wctx.db != nil {
		return m.wctx.db.caseSensitiveLike
	}
	// A POOLED, session-less machine (selfRowExpr.eval, vdbe_run.go) carries
	// the flag directly, seeded from the evalCtx that drives it.
	return m.caseSensitiveLike
}

// execOuter is exec threaded with an enclosing-query evalCtx (outer), used
// when the program is a correlated subquery's body -- so an outer reference
// inside an aggregate item's subquery still resolves. Most callers use exec
// (outer == nil); tryVDBEScan/tryVDBENoFrom/tryVDBENoFromAggregate (vdbe_run.go)
// and compoundProgram.execFrom pass a non-nil one.
func (prog *Program) execOuter(pager *ReadOnlyPager, outer *evalCtx, params []Value) (rows [][]Value, err error) {
	return prog.execOuterTrig(pager, outer, params, nil)
}

// execOuterTrig is execOuter with an optional machine to take the firing
// trigger row from -- see execTrig. from is nil for every ordinary run, which
// is byte-for-byte execOuter's old behaviour.
func (prog *Program) execOuterTrig(pager *ReadOnlyPager, outer *evalCtx, params []Value, from *vdbe) (rows [][]Value, err error) {
	if prog.Compound != nil {
		return prog.execCompoundBody(pager, outer, params, from)
	}
	m := prog.newMachine(pager, outer, params, from)
	// Restore the WITH-clause scope this Program was compiled under, for the
	// span of its whole run -- see Program.CTEScopeSnapshot's doc comment. A
	// no-op (nil snapshot) for every Program compiled with no WITH clause in
	// scope, the overwhelmingly common case.
	if prog.CTEScopeSnapshot != nil && pager != nil {
		saved := pager.cteScopes
		pager.cteScopes = prog.CTEScopeSnapshot
		defer func() { pager.cteScopes = saved }()
	}
	rows, err = m.run(prog.Insns)
	// BACK TO THE POOL, because this machine is finished and nobody else can
	// reach it: the rows it produced are fresh slices (OpResultRow copies out of
	// the registers rather than handing them out), and m itself is not stored
	// anywhere -- it is a local that dies here.
	//
	// UNLESS IT YIELDED. A machine that has executed OP_Yield is suspended, not
	// done: it keeps resumePC so a co-routine can step it again (vdbe_stream.go,
	// which builds and holds its own). No caller can resume THIS one, since the
	// only reference is the local above -- but releasing a suspended machine is
	// the kind of thing that becomes wrong the moment someone adds the caller,
	// so the state that says "finished" is what it is tested on.
	if !m.yield {
		prog.putMachine(m)
	}
	return rows, err
}

// newMachine builds the machine execOuterTrig runs prog on, without running
// it: the register file and cursor tables sized from prog, the pager, the
// bound parameters, the enclosing row, and the pseudo-rows taken from from (nil
// for none). A co-routine (subStream, vdbe_stream.go) is the same machine,
// resumed rather than run once.
func (prog *Program) newMachine(pager *ReadOnlyPager, outer *evalCtx, params []Value, from *vdbe) *vdbe {
	m := prog.getMachine()
	m.jx = prog.jitCode()
	m.sorters = make([]*vdbeSorter, prog.NSorters)
	m.distinctSets = make([]*vdbeDistinctSet, prog.NDistinct)
	m.subCache = make([]subCacheEntry, prog.NSubCache)
	m.pager = pager
	m.params = params
	m.outer = outer
	if from != nil {
		m.trigOld, m.trigNew = from.trigOld, from.trigNew
		m.trigOldRowid, m.trigNewRowid = from.trigOldRowid, from.trigNewRowid
		// The upsert "excluded" row and the RETURNING affected row travel the
		// same channel and for the same reason -- see vdbe.trigExcluded.
		m.trigExcluded, m.trigExcludedRowid = from.trigExcluded, from.trigExcludedRowid
		m.trigReturn, m.trigReturnRowid = from.trigReturn, from.trigReturnRowid
		// A recursive CTE's current row travels the same way: a recursive
		// step runs on its own machine, and so can anything nested inside it
		// (vdbe.recCur).
		m.recCur = from.recCur
	}
	return m
}

// execWithParent runs prog (a CORRELATED subquery's standalone body) in a fresh
// vdbe whose parent points at the enclosing execution's vdbe, so the
// sub-Program's OpOuterColumn/OpOuterRowid read the enclosing row's live
// cursors (the correlation source). Unlike runSubOnce, it is invoked afresh on
// every evaluation of the enclosing expression -- a correlated subquery's
// result depends on the current outer row, so it is never cached. The
// sub-Program runs with the SAME pager and bound-parameter array as the
// enclosing statement (a subquery shares its statement's single bind array).
func (prog *Program) execWithParent(parent *vdbe) (rows [][]Value, err error) {
	// A LIVE correlated body -- an UPDATE SET subquery that reads the table
	// being updated (vdbe_live_read.go's liveRowCtx) -- is lowered again on
	// every call, against the database as this outer row sees it. A no-op for
	// every other program.
	prog, pager, err := parent.liveLower(prog, parent.pager)
	if err != nil {
		return nil, err
	}
	return prog.execWithParentOn(parent, pager)
}

// execWithParentFirst is execWithParent for EXISTS: it stops at the first
// result row and returns it empty (see vdbe.firstRow).
func (prog *Program) execWithParentFirst(parent *vdbe) ([][]Value, error) {
	prog, pager, err := parent.liveLower(prog, parent.pager)
	if err != nil {
		return nil, err
	}
	return prog.execWithParentRun(parent, pager, true)
}

// execWithParentOn is execWithParent over an explicit pager: the parent's own
// for every ordinary body, the fresh image liveLower built for a live one.
func (prog *Program) execWithParentOn(parent *vdbe, pager *ReadOnlyPager) (rows [][]Value, err error) {
	return prog.execWithParentRun(parent, pager, false)
}

func (prog *Program) execWithParentRun(parent *vdbe, pager *ReadOnlyPager, firstRow bool) (rows [][]Value, err error) {
	if prog.Compound != nil {
		// A compound has no machine of its own -- its arms ARE the frames --
		// so the parent is handed to each arm directly rather than wrapped in
		// another level. See compoundProgram.execFrom.
		_, rows, err = prog.Compound.execFrom(pager, parent.params, parent, nil, nil)
		return rows, err
	}
	// The aggregate opcodes resolve a correlated
	// reference through evalCtx.outer, not through OpOuterColumn -- see
	// Program.OuterFrames and buildOuterEvalCtx.
	var outerCtx *evalCtx
	if !prog.outerCtxUnread() {
		var oerr error
		if outerCtx, oerr = buildOuterEvalCtx(prog.OuterFrames, parent); oerr != nil {
			return nil, oerr
		}
	}
	// From the machine pool, as execOuterTrig's machines are: a correlated
	// subquery runs once per outer row, and a fresh machine for every run was
	// most of what such a query allocated. See vdbe_machine_pool.go for what
	// may be reused.
	m := prog.getMachine()
	m.jx = prog.jitCode()
	m.sorters = make([]*vdbeSorter, prog.NSorters)
	m.distinctSets = make([]*vdbeDistinctSet, prog.NDistinct)
	m.subCache = make([]subCacheEntry, prog.NSubCache)
	m.pager, m.params, m.parent, m.outer = pager, parent.params, parent, outerCtx
	m.firstRow = firstRow
	// The four pseudo-rows travel down here exactly as they do in
	// execOuterTrig above, and for the identical reason: OpParam reads them off
	// the RUNNING machine, not off a frame, so a sub-program invoked through
	// this path -- a CORRELATED subquery's body -- sees an empty slot unless
	// the parent's is copied onto it. Without this a RETURNING subquery that is
	// both correlated to an outer cursor and reads the affected row answered
	// rowid 0 and NULL columns instead of the row: a WRONG ANSWER where the
	// gap it replaced was an honest decline.
	m.trigOld, m.trigNew = parent.trigOld, parent.trigNew
	m.trigOldRowid, m.trigNewRowid = parent.trigOldRowid, parent.trigNewRowid
	m.trigExcluded, m.trigExcludedRowid = parent.trigExcluded, parent.trigExcludedRowid
	m.trigReturn, m.trigReturnRowid = parent.trigReturn, parent.trigReturnRowid
	m.recCur = parent.recCur
	// See execOuter's identical bracket and Program.CTEScopeSnapshot's doc
	// comment -- this is the path a CORRELATED subquery's Program (an
	// aggregate with an outer reference beside its own GROUP BY, most
	// commonly) actually runs through, so the CTE scope has to be restored
	// here too, not only in execOuter.
	if prog.CTEScopeSnapshot != nil && pager != nil {
		saved := pager.cteScopes
		pager.cteScopes = prog.CTEScopeSnapshot
		defer func() { pager.cteScopes = saved }()
	}
	rows, err = m.run(prog.Insns)
	// Released only once finished, as execOuterTrig releases its own.
	if !m.yield {
		prog.putMachine(m)
	}
	return rows, err
}

// likePlanFor is pat analysed for the byte matcher, reused while the pattern
// stays the same -- which a constant pattern does for the whole run.
func (m *vdbe) likePlanFor(pat Value) *likePlan {
	if pat.Typ != Text {
		return nil
	}
	if lp := m.likePlan; lp != nil && bytes.Equal(lp.src, pat.S) {
		return lp
	}
	m.likePlan = newLikePlan(pat.S)
	return m.likePlan
}

// outerCtxUnread reports whether no instruction of prog can read the
// machine's enclosing evalCtx (m.outer), so a correlated run need not build it:
// buildOuterEvalCtx gathers the whole outer row, on every run, once per outer
// row. Only the aggregate, window, fts auxiliary, virtual-table and function
// opcodes read m.outer, so this is a WHITELIST of opcodes that do not -- an
// opcode missing from it keeps the context, which is the safe direction.
func (prog *Program) outerCtxUnread() bool {
	if len(prog.Insns) == 0 {
		return false
	}
	if v := (*outerCtxVerdict)(atomic.LoadPointer(&prog.outerCtx)); v != nil && v.insns == &prog.Insns[0] && v.n == len(prog.Insns) {
		return v.unread
	}
	unread := true
	for i := range prog.Insns {
		switch prog.Insns[i].Op {
		case OpInit, OpOpenRead, OpRewind, OpNext, OpClose, OpHalt, OpGoto,
			OpColumn, OpRowid, OpOuterColumn, OpOuterRowid,
			OpInteger, OpVariable, OpNull, OpString8, OpReal, OpSCopy, OpCopy,
			OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIf, OpIfNot, OpIsNull, OpNotNull,
			OpSeekRowidHint, OpSeekIndexHint, OpAutoIndexOrder, OpResultRow:
		default:
			unread = false
		}
	}
	atomic.StorePointer(&prog.outerCtx, unsafe.Pointer(&outerCtxVerdict{insns: &prog.Insns[0], n: len(prog.Insns), unread: unread}))
	return unread
}

// outerCtxVerdict is outerCtxUnread's cached answer and the instructions it is
// about.
type outerCtxVerdict struct {
	insns  *Instruction
	n      int
	unread bool
}

// outerFrame returns the ancestor vdbe levels frames up (levels >= 1): the
// frame OpOuterColumn/OpOuterRowid read for a correlated column reference. The
// compiler only ever emits a level the parent chain actually reaches (it walked
// exactly that many enclosing compile scopes to resolve the reference), so this
// never dereferences a nil parent for a well-formed program.
func (m *vdbe) outerFrame(levels int) *vdbe {
	fr := m
	for i := 0; i < levels; i++ {
		fr = fr.parent
	}
	return fr
}

// cursorColumnMasks derives, per cursor, the set of columns insns reads from
// it. SQLite never decodes a column no opcode names (vdbe.c:3135's header
// loop stops at p2; the value read touches aOffset[p2]..aOffset[p2+1],
// vdbe.c:3192-3195). This engine decodes a row per cell, so the equivalent is
// stated up front over the program.
//
// Returns nil (decode everything) when any opcode consumes a cursor's whole
// row. That list is for performance, not correctness: a masked row read via
// vdbeCursor.fullRow re-decodes rather than return undecoded slots.
func cursorColumnMasks(insns []Instruction, ncursors int) []columnMask {
	for i := range insns {
		if readsWholeCursorRows(insns[i].Op) {
			return nil
		}
	}
	masks := make([]columnMask, ncursors)
	for i := range insns {
		in := &insns[i]
		if in.Op != OpColumn || in.P1 < 0 || in.P1 >= ncursors {
			continue
		}
		c := in.P2
		if c < 0 {
			// Not a column number this engine emits; decline to narrow rather
			// than fold it onto some bit.
			return nil
		}
		if c > 63 {
			c = 63 // "column 63 or above", per columnMask's convention
		}
		masks[in.P1] |= 1 << uint(c)
	}
	return masks
}

// readsWholeCursorRows reports whether an opcode's body reads a cursor's whole
// current row (gatherCursorRow / gatherScopesRow / gatherFrameRow) or can run a
// sub-program that reaches back into this frame's cursors through
// buildOuterEvalCtx. See cursorColumnMasks for why an omission here is a
// performance bug and not a correctness one.
func readsWholeCursorRows(op OpCode) bool {
	switch op {
	case OpAggStep, OpHashAggStep: // gatherCursorRow
		return true
	case OpMatch, OpFts3Aux, OpFts5Aux: // gatherScopesRow
		return true
	case OpSubquery, OpExists, OpInSub, OpRowSub, OpOpenDerived: // -> execWithParent -> gatherFrameRow
		return true
	case OpOuterColumn, OpOuterRowid: // this program is itself somebody's sub-program
		return true
	}
	return false
}

// cursorMask returns cursor c's column mask, deriving every cursor's mask from
// insns on first use (once per execution -- a walk of a program that is about
// to scan a whole table).
func (m *vdbe) cursorMask(insns []Instruction, c int) columnMask {
	if !m.colMasksDone {
		m.colMasks = cursorColumnMasks(insns, len(m.cursors))
		m.colMasksDone = true
	}
	if c < 0 || c >= len(m.colMasks) {
		return allColumns
	}
	return m.colMasks[c]
}

// run executes the instruction stream on m, returning the rows its OpResultRow
// instructions emit. It is shared by execOuter (read runs: m.wctx == nil) and
// runWrite (write runs: m.wctx set, no OpResultRow so the returned rows are
// empty and the WRITE opcodes report their outcome through m.wctx instead --
// see vdbe_write.go).
func (m *vdbe) run(insns []Instruction) (rows [][]Value, err error) {
	pc := 0
	if m.yield {
		pc = m.resumePC
	}
	// The program's native code, when it has some (vdbe_jit.go). Dispatch
	// then reads jx.marked, the same program with OpJIT at each pc that enters
	// it; everything else here keeps reading insns. jitSkip is the pc native
	// code just handed back, which the VDBE runs itself, once.
	code := insns
	jx := m.jx
	if jx != nil && (len(insns) != jx.n || &insns[0] != jx.insns) {
		jx = nil
	}
	if jx != nil {
		code = jx.marked
	}
	jitSkip := -1
	for pc < len(insns) {
		op := &code[pc]
	dispatch:
		switch op.Op {
		case OpJIT:
			if pc == jitSkip {
				jitSkip = -1
				op = &insns[pc]
				goto dispatch
			}
			m.jitEnters++
			pc = jx.enter(m.regs, pc)
			jitSkip = pc
			continue

		case OpInit:
			pc = op.P2
			continue

		case OpGoto:
			pc = op.P2
			continue

		case OpGosub: // vdbe.c:1119
			m.regs[op.P1] = Value{Typ: Int, I: int64(pc)}
			pc = op.P2
			continue

		case OpReturn: // vdbe.c:1152
			if r := m.regs[op.P1]; r.Typ == Int {
				if r.I < 0 || r.I >= int64(len(insns)) {
					return nil, fmt.Errorf("engine: OP_Return to address %d, outside the program", r.I)
				}
				pc = int(r.I) + 1
				continue
			}
			if op.P3 == 0 {
				return nil, errors.New("engine: OP_Return through a register holding no return address")
			}

		case OpHalt:
			m.done = true
			return rows, nil

		case OpRaise:
			if conflictAction(op.P1) == conflictIgnore {
				return nil, errRaiseIgnore
			}
			msg := "constraint failed" // SQLite's message for a NULL RAISE() argument
			if mv := m.regs[op.P2]; mv.Typ != Null {
				msg = valueToText(mv)
			}
			re := &raiseError{action: conflictAction(op.P1), msg: msg}
			return nil, conflictHalt{action: conflictAction(op.P1), err: re}

		case OpLimitCounter:
			// The same coercion the compile-time fold applies
			// (limitOffsetValueToCount), so a LIMIT/OFFSET that CAN be folded
			// and one that has to be coded cannot disagree about what a value
			// means: NUMERIC affinity, and anything that will not become an
			// INTEGER is "datatype mismatch" -- OP_MustBeInt with a zero jump
			// target (vdbe.c:2111-2113), which is how select.c:2549 and :2558
			// spell it.
			n, cerr := limitOffsetValueToCount(m.regs[op.P1])
			if cerr != nil {
				return nil, cerr
			}
			if op.P2 == 1 && n < 0 {
				// codeOffset emits "OP_IfPos iOffset" (select.c:2492), which
				// never fires on a negative, so a negative OFFSET skips
				// nothing. This engine's counter loop tests TRUTHINESS, which a
				// negative passes, so the clamp has to be stated here.
				n = 0
			}
			m.regs[op.P1] = Value{Typ: Int, I: n}

		case OpPseudoRow:
			// Snapshot a pseudo-row out of this machine's registers so a
			// sub-program compiled inside the owning block can read it (it has its
			// own register file). C needs no copy: lookupName rewrites both to a
			// TK_REGISTER (resolve.c:572-576, :587-593) in the same program.
			//
			// Registers are listed individually because a RETURNING row's are not
			// contiguous: its INTEGER PRIMARY KEY slot is the rowid register, so
			// "RETURNING <ipk>" reports the assigned rowid (see emitReturning).
			cols, _ := op.P4.([]int)
			row := make([]Value, len(cols))
			for i, r := range cols {
				row[i] = m.regs[r]
			}
			rid := int64(0)
			if op.P2 >= 0 {
				if rv := m.regs[op.P2]; rv.Typ == Int {
					rid = rv.I
				}
			}
			if op.P1 == 3 {
				m.trigReturn, m.trigReturnRowid = row, rid
			} else {
				m.trigExcluded, m.trigExcludedRowid = row, rid
			}

		case OpParam:
			var src []Value
			var rid int64
			switch op.P1 {
			case 0:
				src, rid = m.trigOld, m.trigOldRowid
			case 2:
				src, rid = m.trigExcluded, m.trigExcludedRowid
			case 3:
				src, rid = m.trigReturn, m.trigReturnRowid
			default:
				src, rid = m.trigNew, m.trigNewRowid
			}
			if src == nil {
				// No such row on this machine AT ALL. Reported for the rowid
				// spelling too, which used to answer a bare 0 and was the only
				// arm of this opcode that did not fail closed -- so a
				// sub-machine that never received the row (see execWithParent)
				// returned rowid 0 as though it were the answer. A wrong value
				// is worse than an error (AGENTS.md invariant 1), and 0 is a
				// perfectly legal rowid, so nothing downstream could tell.
				return nil, fmt.Errorf("vdbe: OpParam has no trigger row to read column %d from", op.P2)
			}
			if op.P2 < 0 {
				m.regs[op.P3] = Value{Typ: Int, I: rid}
			} else if op.P2 < len(src) {
				m.regs[op.P3] = src[op.P2]
			} else {
				// A machine with no firing row cannot answer NEW./OLD.
				// Reported rather than indexed: a live sub-program that was
				// lowered under a trigger context but reached on a machine
				// that has none would otherwise read off the end of an empty
				// slice, which is AGENTS.md invariant 2's hardest failure.
				return nil, fmt.Errorf("vdbe: OpParam has no trigger row to read column %d from", op.P2)
			}

		case OpResultRow:
			if m.firstRow {
				return [][]Value{nil}, nil
			}
			row := make([]Value, op.P2)
			copy(row, m.regs[op.P1:op.P1+op.P2])
			rows = append(rows, row)
			if m.yield {
				// OP_Yield: hand this row to the consumer and resume after it.
				m.resumePC = pc + 1
				return rows, nil
			}

		case OpRecQueueOpen:
			spec, ok := op.P4.(*recQueueSpec)
			if !ok || spec == nil {
				return nil, fmt.Errorf("vdbe: OpRecQueueOpen without a queue spec")
			}
			m.recq = newRecQueue(spec, m.encoding())

		case OpRecQueueFill:
			if ferr := m.recQueueFill(op); ferr != nil {
				return nil, ferr
			}

		case OpRecQueuePop:
			ok, perr := m.recQueuePop(op)
			if perr != nil {
				return nil, perr
			}
			if !ok {
				pc = op.P2
				continue
			}

		case OpRecQueueOffset:
			skip, oerr := m.recQueueOffset()
			if oerr != nil {
				return nil, oerr
			}
			if skip {
				pc = op.P2
				continue
			}

		case OpRecQueueLimit:
			done, lerr := m.recQueueLimit()
			if lerr != nil {
				return nil, lerr
			}
			if done {
				pc = op.P2
				continue
			}

		case OpIf:
			if m.jumpIf(op.P1, op.P3, true) {
				pc = op.P2
				continue
			}

		case OpIfNot:
			if m.jumpIf(op.P1, op.P3, false) {
				pc = op.P2
				continue
			}

		case OpIsNull:
			if m.regs[op.P1].Typ == Null {
				pc = op.P2
				continue
			}

		case OpNotNull:
			if m.regs[op.P1].Typ != Null {
				pc = op.P2
				continue
			}

		case OpInteger:
			m.regs[op.P2] = Value{Typ: Int, I: int64(op.P1)}

		case OpInt64:
			m.regs[op.P2] = Value{Typ: Int, I: op.P4.(int64)}

		case OpReal:
			m.regs[op.P2] = Value{Typ: Float, F: op.P4.(float64)}

		case OpString8:
			m.regs[op.P2] = Value{Typ: Text, S: []byte(op.P4.(string))}

		case OpNull:
			m.regs[op.P2] = Value{Typ: Null}

		case OpBlob:
			m.regs[op.P2] = Value{Typ: Blob, S: op.P4.([]byte)}

		case OpVariable:
			m.regs[op.P2] = paramAt(m.params, op.P1)

		case OpCopy:
			m.regs[op.P2] = copyValue(m.regs[op.P1])

		case OpSCopy:
			m.regs[op.P2] = m.regs[op.P1]

		case OpAdd, OpSubtract, OpMultiply, OpDivide, OpRemainder:
			// r[P3] = r[P2] OP r[P1] (left in P2, right in P1).
			if v, ok := intArith(op.Op, m.regs[op.P2], m.regs[op.P1]); ok {
				// vdbe.c:1908's integer arm, taken before any coercion.
				m.regs[op.P3] = v
				break
			}
			v, aerr := evalArith(arithOpName(op.Op), m.regs[op.P2], m.regs[op.P1])
			if aerr != nil {
				return nil, aerr
			}
			m.regs[op.P3] = v

		case OpConcat:
			m.regs[op.P3] = concatValuesEnc(m.regs[op.P2], m.regs[op.P1], m.encoding())

		case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			if jump, target := m.compareOp(op); jump {
				pc = target
				continue
			}

		case OpNot:
			v := m.regs[op.P1]
			if v.Typ == Null {
				m.regs[op.P2] = Value{Typ: Null}
			} else {
				m.regs[op.P2] = boolValue(!isTruthy(v))
			}

		case OpNegative:
			m.regs[op.P2] = negateValueUnary(m.regs[op.P1])

		case OpBitNot:
			m.regs[op.P2] = bitNotValueUnary(m.regs[op.P1])

		case OpAffinity:
			if aff := op.P4.(affinity); !affinityIsIdentity(m.regs[op.P1].Typ, aff) {
				m.regs[op.P1] = applyAffinityToValue(m.regs[op.P1], aff)
			}

		case OpRealAffinity:
			if v := m.regs[op.P1]; v.Typ == Int {
				m.regs[op.P1] = Value{Typ: Float, F: float64(v.I)}
			}

		case OpCast:
			m.regs[op.P1] = castValueEnc(m.regs[op.P1], op.P4.(string), m.encoding())

		case OpFunction:
			args := growValues(&m.fnArgBuf, op.P2) // vdbe.c:8866; see fnArgBuf
			copy(args, m.regs[op.P1:op.P1+op.P2])
			var v Value
			var ferr error
			switch f := op.P4.(type) {
			case *ScalarFunction:
				// Resolved when the program was built, as C's OP_Function
				// carries its FuncDef (vdbe.c:8850): no lookup per call.
				v, ferr = f.call(args)
			case string:
				if f == "rtreecheck" {
					v, ferr = m.rtreecheck(args) // reads the database; see rtree_check.go
				} else {
					v, ferr = callScalarFuncEnc(f, args, m.likeCaseSensitive(), m.encoding(), op.P5)
				}
			}
			if ferr != nil {
				return nil, ferr
			}
			m.regs[op.P3] = v

		case OpConnState:
			v, cerr := m.connStateValue(op.P4.(string))
			if cerr != nil {
				return nil, cerr
			}
			m.regs[op.P3] = v

		case OpLike:
			x, pat := m.regs[op.P1], m.regs[op.P3]
			escReg, _ := op.P4.(int)
			var (
				esc     rune
				hasEsc  bool
				escNull bool
			)
			if escReg >= 0 {
				r, isNull, eerr := likeEscapeRune(m.regs[escReg])
				if eerr != nil {
					return nil, eerr
				}
				if isNull {
					escNull = true
				} else {
					esc, hasEsc = r, true
				}
			}
			if escNull || x.Typ == Null || pat.Typ == Null {
				m.regs[op.P2] = Value{Typ: Null}
			} else {
				cs := m.likeCaseSensitive()
				var matched bool
				if lp := m.likePlanFor(pat); !hasEsc && x.Typ == Text && lp != nil && lp.ok {
					matched = lp.match(x.S, cs)
				} else if hasEsc {
					matched = likeMatchEscape(valueToText(pat), valueToText(x), esc, cs)
				} else {
					matched = likeMatch(valueToText(pat), valueToText(x), cs)
				}
				if op.P5&p5LikeNot != 0 {
					matched = !matched
				}
				m.regs[op.P2] = boolValue(matched)
			}

		case OpGlob:
			x, pat := m.regs[op.P1], m.regs[op.P3]
			if x.Typ == Null || pat.Typ == Null {
				m.regs[op.P2] = Value{Typ: Null}
			} else {
				matched := globMatch(valueToText(pat), valueToText(x))
				if op.P5&p5GlobNot != 0 {
					matched = !matched
				}
				m.regs[op.P2] = boolValue(matched)
			}

		case OpMatch:
			info := op.P4.(*matchCompileInfo)
			row, gerr := m.gatherScopesRow(info.scopes, info.cursors, info.colBases)
			if gerr != nil {
				return nil, gerr
			}
			ctx := &evalCtx{tables: info.scopes, vals: row, outer: m.outer, pager: m.pager, params: m.params}
			// The pattern was CODED into a register just above this
			// instruction (matchCompileInfo.patReg); read it as a value, the
			// way fts3's xFilter reads apVal[0]. BOTH compile routes code one
			// now (compileMatchExpr's fts3 arm, vdbe_codegen.go, and
			// compileFts5Match, fts5_match_placement.go), so an unset flag is
			// an internal inconsistency -- reported rather than read as
			// register 0, which is allocatable and would answer with some
			// other expression's value (a wrong answer, not a crash).
			if !info.patCompiled {
				return nil, fmt.Errorf("vdbe: OpMatch carries no compiled pattern register")
			}
			v, merr := evalMatchExprLangid(ctx, info.expr, info.langid, info.contentUnneeded, m.regs[info.patReg])
			if merr != nil {
				return nil, merr
			}
			m.regs[op.P2] = v

		case OpFts3Aux:
			info := op.P4.(*fts3AuxCompileInfo)
			row, gerr := m.gatherScopesRow(info.scopes, info.cursors, nil)
			if gerr != nil {
				return nil, gerr
			}
			ctx := &evalCtx{tables: info.scopes, vals: row, outer: m.outer, pager: m.pager, params: m.params}
			args := make([]Value, len(info.argRegs))
			for i, r := range info.argRegs {
				args[i] = m.regs[r]
			}
			// A non-literal MATCH query was coded into a register just above
			// this instruction (fts3AuxCompileInfo.patReg); read it as a
			// value, the way fts3's xFilter reads apVal[0]. A literal one
			// leaves patCompiled false and carries its string on the payload.
			var pat Value
			if info.patCompiled {
				pat = m.regs[info.patReg]
			}
			v, aerr := evalFts3Aux(ctx, info, args, pat)
			if aerr != nil {
				return nil, aerr
			}
			m.regs[op.P2] = v

		case OpFts3Optimize:
			v, oerr := evalFts3Optimize(m.pager, op.P4.(*fts3OptimizeInfo))
			if oerr != nil {
				return nil, oerr
			}
			m.regs[op.P2] = v

		case OpFts5Aux:
			info := op.P4.(*fts5AuxCompileInfo)
			row, gerr := m.gatherScopesRow(info.scopes, info.cursors, nil)
			if gerr != nil {
				return nil, gerr
			}
			ctx := &evalCtx{tables: info.scopes, vals: row, outer: m.outer, pager: m.pager, params: m.params}
			args := make([]Value, len(info.argRegs))
			for i, r := range info.argRegs {
				args[i] = m.regs[r]
			}
			v, aerr := runFts5Aux(ctx, info, args)
			if aerr != nil {
				return nil, aerr
			}
			m.regs[op.P2] = v

		case OpWindowAppend:
			m.windowBatch = append(m.windowBatch, m.recRegs[op.P1])

		case OpWindowFinal:
			extra, werr := m.windowFinal(op.P4.(*windowPlan))
			if werr != nil {
				return nil, werr
			}
			rows = append(rows, extra...)
			if m.yield && len(rows) > 0 {
				m.resumePC = pc + 1
				return rows, nil
			}

		case OpComputeGenerated:
			tbl := op.P4.(*tableMeta)
			if gerr := computeGeneratedInto(tbl.name, tbl.cols, m.regs[op.P1:op.P1+len(tbl.cols)]); gerr != nil {
				return nil, gerr
			}

		case OpTypeCheck:
			// A STRICT table's datatype check over the row image in
			// r[P1..P1+ncol-1] -- checkStrictColumnTypes (insert_write.go),
			// the SAME function the row-at-a-time enforcement sites call
			// (insert_write.go, fk.go), so the two
			// cannot drift on which values a STRICT column accepts. Returned
			// as a PLAIN error, not a conflictHalt: see OpTypeCheck's own
			// doc comment (vdbe_op.go) for why an OR-clause must not soften
			// it.
			tc := op.P4.(*typeCheckPlan)
			if serr := checkStrictColumnTypes(tc.tbl, m.regs[op.P1:op.P1+len(tc.tbl.cols)]); serr != nil {
				return nil, fmt.Errorf("%s: %w", tc.prefix, serr)
			}

		case OpBitAnd, OpBitOr, OpShiftLeft, OpShiftRight:
			// r[P3] = r[P2] OP r[P1]
			m.regs[op.P3] = bitwiseBinaryValue(bitwiseOpName[op.Op], m.regs[op.P2], m.regs[op.P1])

		case OpOpenRead:
			rp, perr := m.dbPager(op.P3)
			if perr != nil {
				return nil, perr
			}
			cur := m.openCursorInto(op.P1, rp, op.P4.(*resolvedTable))
			cur.streamable = op.P5&1 != 0
			if cur.streamable && !cur.hasGeneratedCols() {
				// Only a streaming scan masks: a materialized cursor retains
				// every row, so a masked row would outlive its record, and it
				// decodes each row once anyway.
				//
				// A generated-column table is excluded for correctness: a
				// generated column is computed, not read. normalize() runs
				// computeGeneratedInto over the decoded row, so under a mask
				// "c AS (a+b)" would be computed from undecoded NULLs and
				// stored. expandStoredRow also inserts a slot per VIRTUAL
				// column, so record positions stop matching the mask.
				cur.colMask = m.cursorMask(insns, op.P1).widenIfTotal(len(cur.tbl.cols))
			}
			m.cursors[op.P1] = cur

		case OpSeekRowidHint:
			if cursorHasNoBtree(m.cursors[op.P1]) {
				break
			}
			// Only an INTEGER key can equal a rowid; for any other storage
			// class leave the cursor in full-scan mode (see OpSeekRowidHint's
			// doc comment, vdbe_op.go). The keyed WHERE conjunct is evaluated
			// in the loop body regardless, so this only prunes the scan.
			if key := m.regs[op.P2]; key.Typ == Int {
				cur := m.cursors[op.P1]
				cur.seekConfigured = true
				cur.seekKey = key.I
				cur.seekReseek = op.P3 == 1 // correlated join seek: re-seek per rewind
			} else if op.P3 == 1 {
				// Correlated join seek with a non-integer key this iteration:
				// leave the cursor in full-scan mode for THIS outer row (no
				// integer rowid can match), but keep the re-seek flag so a later
				// outer row whose key IS an integer still re-materializes rather
				// than reusing this full scan.
				cur := m.cursors[op.P1]
				cur.seekConfigured = false
				cur.seekReseek = true
			}

		case OpSeekIndexHint:
			// Coerce the key with the exact affinity the WHERE comparison
			// applies (P4.aff, pre-computed by detectIndexSeekKey), then
			// configure the cursor to seek the secondary index b-tree for
			// candidate rowids on its next OpRewind rather than full-scanning.
			// The keyed WHERE conjunct is re-evaluated on every fetched row, so
			// this only prunes the scan; see OpSeekIndexHint (vdbe_op.go) and
			// vdbeCursor.idxSeek* (vdbe_cursor.go).
			if cursorHasNoBtree(m.cursors[op.P1]) {
				break
			}
			cur := m.cursors[op.P1]
			switch h := op.P4.(type) {
			case *indexSeekHint:
				cur.idxSeekConfigured = true
				cur.idxSeekProbe = applyAffinityToValue(m.regs[op.P2], h.aff)
				cur.idxSeekColl = h.coll
				cur.idxSeekCol = h.leadingCol
			default:
				return nil, fmt.Errorf("vdbe: OpSeekIndexHint without a plan")
			}
			cur.seekReseek = op.P3 == 1 // correlated join seek: re-seek per rewind

		case OpAutoIndexOrder:
			cur := m.cursors[op.P1]
			if cur != nil && cur.rowStore != nil {
				// A WRITE loop's cursor: the order is kept on it and applied at
				// every materialization (applyWriteOrder), never skipped -- a
				// per-row SET subquery was lowered on the promise of it.
				cur.writeOrder = op.P4.(*autoIndexKey)
				cur.applyWriteOrder()
				break
			}
			if cur == nil || cursorHasNoBtree(cur) || cur.streamable || cur.seekReseek {
				// Nothing to permute (no b-tree, or a lazy pull), or a correlated
				// seek that re-materializes a different row set per rewind.
				//
				// A non-correlated seek is permuted like any cursor: it
				// materializes once and its rows are a superset of the answer, so
				// sorting that superset and letting the loop filter keeps the same
				// relative order. Declining leaves the pre-port scan order, never a
				// different row set -- see OpAutoIndexOrder.
				break
			}
			// The cursor materializes on its first rewind; force that now so the
			// permutation below is the one every later rewind re-iterates.
			cur.params = m.params // see vdbeCursor.params
			if rerr := cur.rewind(); rerr != nil {
				return nil, rerr
			}
			autoIndexOrderCursor(cur, op.P4.(*autoIndexKey), m.encoding())

		case OpMultiOrTag:
			cur := m.cursors[op.P1]
			if cur == nil || cur.pos < 0 || cur.pos >= len(cur.rows) {
				return nil, fmt.Errorf("vdbe: OpMultiOrTag on a cursor with no current materialized row")
			}
			if len(cur.multiOrGrp) != len(cur.rows) {
				cur.multiOrGrp = make([]int, len(cur.rows))
			}
			cur.multiOrGrp[cur.pos] = int(m.regs[op.P2].I)

		case OpMultiOrSort:
			cur := m.cursors[op.P1]
			if cur == nil || len(cur.rows) == 0 {
				break // nothing bound, or an empty table: nothing was tagged
			}
			if err := multiOrSortTagged(cur, op.P4.(*multiOrOrder), m.encoding()); err != nil {
				return nil, err
			}

		case OpOpenDerived:
			ds := op.P4.(*derivedSource)
			dsPager, perr := m.dbPager(ds.dbIdx)
			if perr != nil {
				return nil, perr
			}
			if ds.catalogScope != scopeAny {
				// The rowids stay all-zero: a catalog row's real rowid is not
				// reproducible here (this writer densely rebuilds the schema
				// table on flush, and a temp row's rowid is the TEMP database's
				// own in C SQLite), which is exactly what
				// schemaCatalogQueryGuard declines a rowid/oid/_rowid_
				// reference for (query.go).
				crows, cerr := dsPager.schemaCatalogRows(ds.catalogScope)
				if cerr != nil {
					return nil, cerr
				}
				m.cursors[op.P1] = openMaterializedCursor(ds.tbl, crows, nil)
				break
			}
			if ds.vtabWrite != nil {
				// The WRITE-side scan of a virtual table -- SQLite's
				// sqlite3WhereBegin over the vtab itself (update.c:1273,
				// delete.c:526). Its rows come from the MODULE's own write-side
				// source rather than from dsPager, which is exactly what keeps
				// the compiled route's row set identical to the one the module
				// half then mutates (vtabWriteRows, vdbe_vtab_write.go). No
				// subCache slot: this reads live state, once, at the top of the
				// program, and caching it across executions is the very thing
				// that would make it stale.
				if m.wctx == nil || m.wctx.db == nil {
					// Emitted only into a WRITE program, so unreachable --
					// reported rather than dereferenced (AGENTS.md invariant 2).
					return nil, fmt.Errorf("vdbe: virtual-table write scan with no write session")
				}
				rowids, rows, verr := m.wctx.db.vtabWriteRows(ds.vtabWrite)
				if verr != nil {
					return nil, verr
				}
				m.cursors[op.P1] = openMaterializedCursor(ds.tbl, rows, rowids)
				break
			}
			if ds.schemaWrite {
				// The WRITE-side scan of the schema catalog -- an ORDINARY
				// table scan in C SQLite once the writable_schema flag
				// stops tabIsReadOnly (delete.c:103-107) refusing the write,
				// so its WHERE reaches the same sqlite3WhereBegin every other
				// DELETE/UPDATE's does (delete.c:526, update.c:742). Its rows
				// come from the OVERLAY's own current view of the catalog
				// rather than from dsPager, which is what keeps this scan's
				// row set identical to the one OpSchemaWrite then edits (see
				// derivedSource.schemaWrite). No subCache slot: it reads live
				// state, once, at the top of the program.
				db, derr := m.schemaWriteDB()
				if derr != nil {
					return nil, derr
				}
				crows, cerr := db.wsCurrentCatalog()
				if cerr != nil {
					return nil, cerr
				}
				vals := make([][]Value, len(crows))
				rowids := make([]int64, len(crows))
				for i, r := range crows {
					vals[i] = r.vals
					rowids[i] = r.rowid
				}
				m.schemaWrite = &schemaWriteState{rows: crows}
				m.cursors[op.P1] = openMaterializedCursor(ds.tbl, vals, rowids)
				break
			}
			if ds.vtab != nil {
				if len(ds.vtabCorr) > 0 {
					// A CORRELATED source (vtab_correlated.go): this opcode
					// sits INSIDE the join, at the top of its own level, and
					// the constraint values are in registers the outer loops
					// just wrote -- C's own "codeExprOrVector(pParse, pRight,
					// iTarget, 1)" (wherecode.c:1584) feeding the OP_VFilter
					// below it. No subCache slot: the row set differs per outer
					// row, which is the entire point.
					corr := make([]vtabCorrValue, len(ds.vtabCorr))
					for i, t := range ds.vtabCorr {
						corr[i] = vtabCorrValue{conj: t.conj, arg: t.arg, col: t.col, op: t.op, v: m.regs[t.reg]}
					}
					_, rows, rowids, verr := dsPager.materializeVtab(*ds.vtab, m.params, m.outer, ds.vtabTrig, m, corr)
					if verr != nil {
						return nil, verr
					}
					m.cursors[op.P1] = openMaterializedCursor(ds.tbl, rows, rowids)
					break
				}
				rows, rowids, verr := m.runVtabOnce(op.P2, ds, dsPager)
				if verr != nil {
					return nil, verr
				}
				m.cursors[op.P1] = openMaterializedCursor(ds.tbl, rows, rowids)
				break
			}
			if ds.recSelf != nil {
				cur, rerr := m.openRecursiveSelf(ds)
				if rerr != nil {
					return nil, rerr
				}
				m.cursors[op.P1] = cur
				break
			}
			if ds.cte != nil {
				rows, cerr := m.runCTEOnce(op.P2, ds, dsPager)
				if cerr != nil {
					return nil, cerr
				}
				m.cursors[op.P1] = openDerivedCursor(ds.tbl, rows, ds.keepSubtype)
				break
			}
			srcPager := ds.pager
			if srcPager == nil {
				srcPager = dsPager
			}
			if m.streamsDerived(ds, insns) {
				// A co-routine rather than a materialization: see subStream
				// (vdbe_stream.go) and derivedSource.stream.
				m.cursors[op.P1] = openStreamCursor(ds.tbl, m.newSubStream(ds, srcPager))
				break
			}
			var rows [][]Value
			var derr error
			switch {
			case !ds.prog.Correlated:
				rows, derr = m.runSubOnce(op.P2, ds.prog, srcPager)
			case srcPager == m.pager:
				// A derived table whose body binds a column of a query
				// enclosing THIS one (resolveDerivedSource's barrier compile):
				// its rows depend on the current outer row, so it is
				// materialized against the live parent frame and never cached
				// -- exactly what runSub does for a correlated scalar/IN
				// subquery.
				rows, derr = ds.prog.execWithParent(m)
			default:
				// A correlated body over a DIFFERENT pager: execWithParent
				// would run it against the enclosing statement's pager, which
				// is not the database it was compiled against. Unreachable by
				// construction -- the only derived source carrying its own
				// pager is a foreign-qualified VIEW, whose body is compiled
				// with no enclosing scope and so is never correlated -- but
				// reported rather than answered from the wrong file.
				return nil, fmt.Errorf("vdbe: correlated derived table over a foreign database")
			}
			if derr != nil {
				return nil, derr
			}
			if ds.viewUpfrom != nil {
				// update.c:243-247's SRT_Table destination for a VIEW target:
				// the join's rows go in as they are, auto-keyed, with no
				// collapse. All that is left is the ambiguity refusal, which
				// is a property of the DATA and so belongs here rather than in
				// a compile decline -- see checkViewUpfromRows.
				if verr := checkViewUpfromRows(ds.viewUpfrom, rows); verr != nil {
					return nil, verr
				}
			}
			if ds.upfrom != nil {
				// select.c:1355's SRT_Upfrom destination: collapse the join's
				// rows onto one KEYED entry per target row. The cursor is
				// materialized rather than derived precisely because those keys
				// must read back through OpRowid (update.c:861's "OP_Rowid
				// iEph, regOldRowid"), which an ordinary derived cursor answers
				// with a zero placeholder.
				keys, vals, uerr := collapseUpfromRows(ds.upfrom, rows)
				if uerr != nil {
					return nil, uerr
				}
				m.cursors[op.P1] = openMaterializedCursor(ds.tbl, vals, keys)
				break
			}
			m.cursors[op.P1] = openDerivedCursor(ds.tbl, rows, ds.keepSubtype)

		case OpRewind:
			cur := m.cursors[op.P1]
			// P4, when the row-filter recogniser attached one, is a compiled
			// candidate-set restriction for a columnar scan (see
			// segment_row_filter.go). Every other cursor ignores it.
			cur.segFilter, _ = op.P4.(*segRowFilter)
			// rewind() can reach multiOrOrderCursor, which
			// evaluates compiled WHERE disjuncts; those may carry a bound
			// parameter. See vdbeCursor.params.
			cur.params = m.params
			if rerr := cur.rewind(); rerr != nil {
				return nil, rerr
			}
			if !cur.advance() {
				if cur.streamErrPending != nil {
					return nil, cur.streamErrPending
				}
				pc = op.P2
				continue
			}

		case OpNext:
			cur := m.cursors[op.P1]
			if cur.advance() {
				pc = op.P2
				continue
			}
			if cur.streamErrPending != nil {
				return nil, cur.streamErrPending
			}

		case OpColumn:
			if cerr := m.cursors[op.P1].readColumn(int(op.P2), &m.regs[op.P3]); cerr != nil {
				return nil, cerr
			}

		case OpRowid:
			cur := m.cursors[op.P1]
			if cur.rowidNull {
				m.regs[op.P2] = Value{Typ: Null}
			} else {
				m.regs[op.P2] = Value{Typ: Int, I: int64(cur.rowid)}
			}

		case OpOuterColumn:
			fr := m.outerFrame(int(op.P5))
			if cerr := fr.cursors[op.P1].readColumn(int(op.P2), &m.regs[op.P3]); cerr != nil {
				return nil, cerr
			}

		case OpOuterAggReg:
			// The aggregate-result register block of the frame P5 levels up --
			// see the opcode's own comment (vdbe_op.go) for why C needs no
			// equivalent and this engine does.
			m.regs[op.P3] = m.outerFrame(int(op.P5)).regs[op.P1]

		case OpOuterRowid:
			cur := m.outerFrame(int(op.P5)).cursors[op.P1]
			if cur.rowidNull {
				m.regs[op.P2] = Value{Typ: Null}
			} else {
				m.regs[op.P2] = Value{Typ: Int, I: int64(cur.rowid)}
			}

		case OpClearSubtype:
			// The subtype loss a serialized record gives C SQLite for free
			// -- see the opcode's own comment (vdbe_op.go).
			m.regs[op.P1].Subtype = 0

		case OpClose:
			if cur := m.cursors[op.P1]; cur != nil {
				cur.close()
			}

		case OpNullRow:
			m.cursors[op.P1].nullRow()

		case OpRightJoinMark:
			m.cursors[op.P1].markMatched()

		case OpRightJoinSweepRewind:
			cur := m.cursors[op.P1]
			if !cur.materialized {
				// The main pass never reached this cursor's OpRewind (e.g. an
				// empty preceding table) -- materialize it now so the sweep
				// below has rows to scan.
				if rerr := cur.rewind(); rerr != nil {
					return nil, rerr
				}
			}
			if !cur.sweepUnmatchedFrom(0) {
				pc = op.P2
				continue
			}
			for _, oc := range op.P4.([]int) {
				m.cursors[oc].nullRow()
			}

		case OpRightJoinSweepNext:
			cur := m.cursors[op.P1]
			if cur.sweepUnmatchedFrom(cur.pos + 1) {
				for _, oc := range op.P4.([]int) {
					m.cursors[oc].nullRow()
				}
				pc = op.P2
				continue
			}

		case OpSorterOpen:
			m.sorters[op.P1] = newSorter(op.P4.(*sorterKeyInfo), m.encoding())

		case OpMakeRecord:
			rec := m.recAlloc(op.P2)
			for i := 0; i < op.P2; i++ {
				rec[i] = copyValue(m.regs[op.P1+i])
			}
			m.recRegs[op.P3] = rec

		case OpSorterCheck:
			s := m.sorters[op.P1]
			if s.loses(m.regs[op.P3 : op.P3+s.keyInfo.nKey]) {
				pc = op.P2
				continue
			}

		case OpSorterInsert:
			m.sorters[op.P1].insert(m.recRegs[op.P2])

		case OpSorterSort:
			if !m.sorters[op.P1].sort() {
				pc = op.P2
				continue
			}

		case OpSorterData:
			row := m.sorters[op.P1].data()
			if n := min(op.P3, len(row)); n > 0 {
				// P3: this record round-trip LOSES the JSON subtype on its
				// leading n values -- see OpSorterData's own doc comment.
				clearJSONSubtype(row[:n])
			}
			m.recRegs[op.P2] = row

		case OpRecordColumn:
			m.regs[op.P3] = m.recRegs[op.P1][op.P2]

		case OpSorterNext:
			if m.sorters[op.P1].next() {
				pc = op.P2
				continue
			}

		case OpDistinctOpen:
			// P4 carries the output columns' collating sequences when at least
			// one is non-BINARY (distinctCollations); nil is every column
			// BINARY -- the same convention OpGroup uses.
			dcolls, _ := op.P4.([]string)
			m.distinctSets[op.P1] = newDistinctSet(dcolls, m.encoding())

		case OpDistinct:
			if m.distinctSets[op.P1].checkAndAdd(m.recRegs[op.P3]) {
				pc = op.P2
				continue
			}

		case OpAggReset:
			m.aggAccs = newAggAccs(op.P4.(*aggPlan))

		case OpSegFilterCount:
			// The JITted VDBE's filter. The cursor's table is columnar, so the
			// count comes from the segments' own blocks -- four rows per
			// compare where the loop this replaced did one per row.
			//
			// It DECLINES rather than guesses: a column that is not a
			// fixed-width block, a NULL, a value in the exception list, or a
			// machine without vectors all send it back to the loop, which is
			// still in the program because the peephole only runs when it can
			// prove the whole shape.
			plan, ok := op.P4.(*segFilterPlan)
			if !ok || m.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegFilterCount without a plan")
			}
			cur := m.cursors[op.P2]
			if cur == nil || cur.tbl == nil || cur.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegFilterCount on a closed cursor")
			}
			// Resolve each bound from its register now, since a parameter has
			// no value until the statement runs. A non-integer bound (TEXT,
			// REAL, NULL) declines rather than being coerced: the kernel
			// compares int64 lanes, and cross-class ordering and NULL logic are
			// what a coercion would get wrong.
			//
			// Every one of these reads the cursor's own pager, not m.pager: a
			// root page is file-local, so counting root N in main for a cursor
			// over TEMP or an attachment answers about a different table.
			if preds, okBounds := m.segPlanPreds(plan); okBounds && (len(plan.semis) > 0 || len(plan.ins) > 0 || len(plan.likes) > 0) {
				semis, okSemi := m.segSemis(plan.semis)
				ins, okIn := m.segIns(plan.ins)
				likes, okLike := m.segLikes(plan.likes)
				if okSemi && okIn && okLike {
					if total, served := cur.pager.segSemiCountTable(cur.tbl.root, cur.tbl, preds, semis, ins, likes); served {
						segFilterServed.Add(1)
						m.regs[op.P1] = Value{Typ: Int, I: int64(total)}
						pc = op.P3
						continue
					}
				}
			} else if okBounds {
				if plan.isSum {
					if v, served := cur.pager.segFilterSumTable(cur.tbl.root, preds, plan.sumCol); served {
						segFilterServed.Add(1)
						m.regs[op.P1] = v
						pc = op.P3
						continue
					}
				} else if total, served := cur.pager.segFilterCountTable(cur.tbl.root, preds); served {
					segFilterServed.Add(1)
					m.regs[op.P1] = Value{Typ: Int, I: int64(total)}
					pc = op.P3
					continue
				}
			}
			segFilterDeclined.Add(1)
			// Not served: fall through to the loop, which the peephole left in
			// the program precisely so this branch has somewhere to go.

		case OpSegOrderLimit:
			// The ORDER BY ... LIMIT twin of OpSegFilterCount: same guard
			// shape, same decline-by-falling-through, but it produces a row
			// SET which OpSegEmitRow then walks.
			plan, okPlan := op.P4.(*segOrderPlan)
			if !okPlan || m.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegOrderLimit without a plan")
			}
			cur := m.cursors[op.P2]
			if cur == nil || cur.tbl == nil || cur.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegOrderLimit on a closed cursor")
			}
			// The int64 path first, because it is measured faster; the Value
			// path when a key or output column is not a clean int64 block --
			// a TEXT key, a BLOB column, a NULL-bearing one. Before the second
			// existed, all of those declined to the loop.
			rows, served := cur.pager.segOrderLimitTable(cur.tbl.root, plan, cur.tbl.ipkIndex)
			if !served {
				rows, served = cur.pager.segOrderLimitValues(cur.tbl.root, plan, cur.tbl.ipkIndex)
			}
			if served {
				segFilterServed.Add(1)
				m.segRows, m.segRow = rows[min(plan.offset, len(rows)):], 0
				pc = op.P3
				continue
			}
			segFilterDeclined.Add(1)

		case OpSegDistinct:
			dplan, okPlan := op.P4.(*segDistinctPlan)
			cur := m.cursors[op.P2]
			if !okPlan || cur == nil || cur.tbl == nil || cur.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegDistinct without a plan or an open cursor")
			}
			if rows, served := cur.pager.segDistinctTable(cur.tbl.root, dplan.col, cur.tbl.ipkIndex); served {
				segFilterServed.Add(1)
				m.segRows, m.segRow = rows, 0
				pc = op.P3
				continue
			}
			segFilterDeclined.Add(1)

		case OpSegProgram:
			pplan, okPlan := op.P4.(*segProgPlan)
			if !okPlan || m.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegProgram without a plan")
			}
			cur := m.cursors[op.P2]
			if cur == nil || cur.tbl == nil || cur.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegProgram on a closed cursor")
			}
			if vs, served := m.segRunProgramAll(cur.pager, cur.tbl.root, pplan, cur.tbl.ipkIndex); served {
				segFilterServed.Add(1)
				// P1 is the FIRST destination register; the statement's N
				// aggregates land in P1..P1+N-1, which is the range the
				// recogniser's own ResultRow reads (see segProgPeephole).
				for i, v := range vs {
					m.regs[op.P1+i] = v
				}
				pc = op.P3
				continue
			}
			segFilterDeclined.Add(1)

		case OpSegHashAgg:
			gplan, okPlan := op.P4.(*segGroupPlan)
			if !okPlan || m.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegHashAgg without a plan")
			}
			cur := m.cursors[op.P1]
			if cur == nil || cur.tbl == nil || cur.pager == nil {
				return nil, fmt.Errorf("vdbe: OpSegHashAgg on a closed cursor")
			}
			if m.segHashAggTable(cur.pager, cur.tbl.root, gplan) {
				segFilterServed.Add(1)
				pc = op.P2
				continue
			}
			segFilterDeclined.Add(1)

		case OpSegEmitRow:
			if m.segRow >= len(m.segRows) {
				pc = op.P2
				continue
			}
			row := m.segRows[m.segRow]
			m.segRow++
			for i := 0; i < op.P3 && i < len(row); i++ {
				m.regs[op.P1+i] = row[i]
			}

		case OpAggStep:
			plan := op.P4.(*aggPlan)
			var row, rowids []Value
			if op.P3 == 0 { // source: plan.cursors (one or more joined table cursors)
				var gerr error
				row, rowids, gerr = m.gatherCursorRow(plan)
				if gerr != nil {
					return nil, gerr
				}
			} else { // source: record register [cols.., rowids..] -- see aggRowSplit
				row, rowids = aggRowSplit(plan, m.recRegs[op.P1])
			}
			if serr := m.aggStep(m.aggRowCtx(plan, row, rowids), op.P2); serr != nil {
				return nil, serr
			}

		case OpGroupSame:
			// P4 carries the GROUP BY keys' collating sequences when at least
			// one is non-BINARY (planGroupByStmt's groupColls); nil is every
			// key BINARY, which keysEqualGrouping answers with keysEqual
			// itself, byte for byte as this opcode always has.
			colls, _ := op.P4.([]string)
			if keysEqualGrouping(m.recRegs[op.P1], m.recRegs[op.P3], colls, m.encoding()) {
				// The anchor certificate's key half: a group whose rows spell
				// an equal key differently reports the first row's spelling,
				// so it must not depend on which row came first.
				if a := m.aggAccs; a != nil && a.check != nil && a.check.key && !a.anchorRisk &&
					!anchorKeysIdentical(m.recRegs[op.P1], m.recRegs[op.P3]) {
					a.anchorRisk = true
				}
				pc = op.P2
				continue
			}

		case OpRecCopy:
			src := m.recRegs[op.P1]
			dst := make([]Value, len(src))
			for i := range src {
				dst[i] = copyValue(src[i])
			}
			m.recRegs[op.P2] = dst

		case OpGroupBatchAppend:
			m.groupBatch = append(m.groupBatch, m.recRegs[op.P1])

		case OpGroupBatchFinal:
			extra := m.groupBatchFinal(op.P4.(*groupBatchPlan))
			rows = append(rows, extra...)
			if m.yield && len(rows) > 0 {
				m.resumePC = pc + 1
				return rows, nil
			}

		case OpHashAggStep:
			if herr := m.hashAggStep(op.P4.(*aggPlan), m.recRegs[op.P1], op.P2); herr != nil {
				return nil, herr
			}

		case OpHashAggSort:
			if !m.hashAggSort() {
				pc = op.P2
				continue
			}

		case OpHashAggData:
			m.recRegs[op.P2] = m.hashAggData()

		case OpHashAggNext:
			if m.hashAggNext() {
				pc = op.P2
				continue
			}

		case OpAggResult:
			info := op.P4.(*aggResultInfo)
			var groupKey []Value
			if op.P3 >= 0 {
				groupKey = m.recRegs[op.P3]
			}
			v, rerr := m.aggResult(info, groupKey)
			if rerr != nil {
				return nil, rerr
			}
			m.regs[op.P1] = v

		case OpSubquery:
			sub := op.P4.(*Program)
			rows, serr := m.runSub(op.P5&p5Correlated != 0, op.P2, sub)
			if serr != nil {
				return nil, serr
			}
			if sub.OrderUnproven && firstRowDependsOnOrder(rows) {
				return nil, fmt.Errorf("%w: a scalar subquery's first row, over rows whose arrival order is not provably C SQLite's (see anchorPlanOrderProvable)", errVDBEUnsupported)
			}
			// P3 is the destination WIDTH: 0/1 for the ordinary scalar form,
			// >1 for a ROW-VALUE subquery operand (emitRowSubqueryProbe), which
			// lands the first row's whole tuple in a consecutive block. No rows
			// leaves every destination NULL either way.
			width := op.P3
			if width < 1 {
				width = 1
			}
			for i := 0; i < width; i++ {
				if len(rows) == 0 || i >= len(rows[0]) {
					m.regs[op.P1+i] = Value{Typ: Null}
					continue
				}
				m.regs[op.P1+i] = rows[0][i]
			}

		case OpExists:
			var rows [][]Value
			var serr error
			if sub := op.P4.(*Program); op.P5&p5Correlated != 0 && sub.Compound == nil && sub.NSorters == 0 {
				// Correlated, so run once per outer row: stop at the first row.
				// No sorter, so stopping early leaves no spilled sort behind.
				rows, serr = sub.execWithParentFirst(m)
			} else {
				rows, serr = m.runSub(op.P5&p5Correlated != 0, op.P2, sub)
			}
			if serr != nil {
				return nil, serr
			}
			exists := len(rows) > 0
			if op.P3 != 0 {
				exists = !exists
			}
			m.regs[op.P1] = boolValue(exists)

		case OpInSub:
			plan := op.P4.(*inSubPlan)
			rows, serr := m.runSub(plan.correlated, op.P3, plan.prog)
			if serr != nil {
				return nil, serr
			}
			xs := m.regs[op.P1 : op.P1+len(plan.affs)]
			if set := m.inSetFor(plan, op.P3, rows); set != nil {
				m.regs[op.P2] = set.membership(xs[0], plan.not)
			} else {
				m.regs[op.P2] = inSubMembership(xs, rows, plan.affs, plan.colls, plan.not, m.encoding())
			}

		case OpRowSub:
			plan := op.P4.(*rowSubPlan)
			rows, serr := m.runSub(plan.correlated, op.P3, plan.prog)
			if serr != nil {
				return nil, serr
			}
			m.regs[op.P2] = rowSubCompare(m.regs[op.P1:op.P1+len(plan.affs)], rows, plan, m.encoding())

		case OpSubCacheReset:
			// vdbe.c:7581-7582's per-invocation OP_Once reset, for a RETURNING
			// block that runs more than once per statement. The bounds are the
			// compiler's own allocN-style range, but they are CHECKED rather
			// than trusted: a slot index off the end of subCache would panic,
			// and AGENTS.md invariant 2 ranks a panic as the hardest failure.
			// See OpSubCacheReset's doc comment (vdbe_op.go).
			if op.P1 < 0 || op.P2 < 0 || op.P1+op.P2 > len(m.subCache) {
				return nil, fmt.Errorf("vdbe: OpSubCacheReset range [%d,%d) is outside the %d cache slots", op.P1, op.P1+op.P2, len(m.subCache))
			}
			clear(m.subCache[op.P1 : op.P1+op.P2])
			// P4, when present, is an explicit slot LIST -- the MIXED-lifetime
			// RETURNING block, whose per-row subqueries are not a contiguous
			// range because the slot allocator hands them out in the order the
			// expression list compiles, interleaved with the run-once ones. Same
			// bounds check, same reason (AGENTS.md invariant 2).
			if slots, ok := op.P4.([]int); ok {
				for _, sl := range slots {
					if sl < 0 || sl >= len(m.subCache) {
						return nil, fmt.Errorf("vdbe: OpSubCacheReset slot %d is outside the %d cache slots", sl, len(m.subCache))
					}
					m.subCache[sl] = subCacheEntry{}
				}
			}

		case OpOpenWrite:
			// See table_load.go's package doc comment: this is the single
			// choke point every VDBE-compiled table SCAN (a SELECT, a
			// subquery, a trigger body's own reads, an UPDATE/DELETE's
			// compiled WHERE scan) funnels through -- openRowStoreCursor's
			// first OpRewind lazily materializes tbl.rows into the cursor's
			// own rowids/rows slices (materializeRowStore, vdbe_cursor.go),
			// which needs tbl.rows to actually be loaded first.
			owTbl := op.P4.(*tableMeta)
			if err := m.wctx.db.ensureTableLoaded(owTbl); err != nil {
				return nil, err
			}
			m.openRowStoreCursorInto(op.P1, owTbl)

		case OpHaltError:
			return nil, conflictHalt{action: conflictAction(op.P1), err: errors.New(op.P4.(string))}

		case OpHaltIfNull:
			if m.regs[op.P3].Typ == Null {
				return nil, conflictHalt{action: conflictAction(op.P1), err: errors.New(op.P4.(string))}
			}

		case OpMustBeInt:
			// SQLite's OP_MustBeInt: coerce r[P1] to an INTEGER where that is
			// lossless (INTEGER affinity handles a numeric TEXT; an integral
			// REAL converts), else jump to P2 -- or, with P2 == 0, halt with
			// "datatype mismatch" exactly as C SQLite does for a rowid that
			// cannot be an integer.
			if v := applyAffinityToValue(m.regs[op.P1], affInteger); v.Typ == Int {
				m.regs[op.P1] = v
			} else if v.Typ == Float && v.F == float64(int64(v.F)) {
				m.regs[op.P1] = Value{Typ: Int, I: int64(v.F)}
			} else if op.P2 != 0 {
				pc = op.P2
				continue
			} else {
				return nil, conflictHalt{action: conflictAction(op.P3), err: errors.New("engine: datatype mismatch")}
			}

		case OpNewRowid:
			tbl := op.P4.(*tableMeta)
			var rid uint64
			var rerr error
			switch db := m.wctx.db; {
			case tbl.autoIncrement:
				rid, rerr = m.wctx.aincNewRowid(tbl)
			default:
				rid, rerr = db.autoRowid(tbl)
			}
			if rerr != nil {
				return nil, rerr
			}
			m.regs[op.P2] = Value{Typ: Int, I: int64(rid)}

		case OpMemMax:
			if werr := m.wctx.aincStep(op.P4.(*tableMeta), m.regs[op.P1]); werr != nil {
				return nil, werr
			}

		case OpFireTriggers:
			target, werr := m.opFireTriggers(op)
			if werr != nil {
				return nil, werr
			}
			if target >= 0 {
				pc = target
				continue
			}

		case OpTriggerBodyRouted:
			if werr := m.opTriggerBodyRouted(op); werr != nil {
				return nil, werr
			}

		case OpUpsertFind:
			target, werr := m.opUpsertFind(op)
			if werr != nil {
				return nil, werr
			}
			if target >= 0 {
				pc = target
				continue
			}

		case OpUpsertStore:
			if werr := m.opUpsertStore(op); werr != nil {
				return nil, werr
			}
			if werr := m.fkDrain(); werr != nil {
				return nil, werr
			}

		case OpUpsertReload:
			m.opUpsertReload(op)

		case OpInsert:
			target, werr := m.opInsert(op)
			if werr != nil {
				return nil, werr
			}
			if werr := m.fkDrain(); werr != nil {
				return nil, werr
			}
			if target >= 0 {
				pc = target
				continue
			}

		case OpNotExists:
			// A SEEK, not a presence test. The cursor iterates a SNAPSHOT of
			// the row store (openRowStoreCursor), so it still yields a row a
			// trigger body removed underneath it AND it still yields the
			// CONTENT that row had before a trigger body rewrote it.
			// reseekRowStore answers both halves the way vdbe.c:5536/5540 does
			// -- see its doc comment, and OpNotExists' for delete.c's own
			// guard and for why the jump skips the whole row.
			if cur := m.cursors[op.P1]; cur != nil {
				if tbl, ok := op.P4.(*tableMeta); ok {
					if op.P5 != 0 {
						// P3 is the key register, as OP_NotExists takes it
						// (vdbe.c:5521/5524). The key-less form below is the case
						// where the cursor is already on the row; here it has no
						// position, because the loop is driven by the UPDATE ... FROM
						// ephemeral table (update.c:861-864). The key was loaded by
						// OpRowid from that table, so it is always an Integer.
						cur.rowid = uint64(m.regs[op.P3].I)
						cur.rowidNull = false
					}
					// A WITHOUT ROWID target is re-seeked by its PRIMARY KEY
					// record, update.c's other pass-two arm (OP_RowData +
					// OP_NotFound at :868-869, vs OP_Rowid + OP_NotExists at
					// :875-877); see reseekRowStoreByPK. Not for the
					// key-register form: that loop's keys are internal ids
					// (collapseUpfromRows), just written into the cursor above.
					//
					// m.wctx is set for every program emitting this opcode, but
					// is checked rather than trusted -- a nil dereference would
					// be a panic, and the internal-id seek is correct for every
					// shape but a keyless identity move.
					found := false
					if op.P5 == 0 && tbl.withoutRowid && m.wctx != nil {
						found = cur.reseekRowStoreByPK(m.wctx.db, tbl)
					} else {
						found = cur.reseekRowStore(tbl)
					}
					if !found {
						pc = op.P2
						continue
					}
				}
			}

		case OpDelete:
			if werr := m.opDelete(op); werr != nil {
				return nil, werr
			}
			if werr := m.fkDrain(); werr != nil {
				return nil, werr
			}

		case OpClearTable:
			tbl := op.P4.(*tableMeta)
			if perr := m.wctx.pinViolation(tbl); perr != nil { // see writeCtx.pinnedTable
				return nil, perr
			}
			n := tbl.rows.clear()
			m.wctx.rowsAffected += int(n)

		case OpSkipIfRowGone:
			tbl := op.P4.(*tableMeta)
			if _, ok := tbl.rows.get(uint64(m.regs[op.P1].I)); !ok {
				pc = op.P2
				continue
			}

		case OpUpdateRow:
			target, werr := m.opUpdateRow(op)
			if werr != nil {
				return nil, werr
			}
			if werr := m.fkDrain(); werr != nil {
				return nil, werr
			}
			if target >= 0 {
				pc = target
				continue
			}

		case OpVWriteRow:
			m.opVWriteRow(op)

		case OpVWrite:
			if werr := m.opVWrite(op); werr != nil {
				return nil, werr
			}

		case OpVInsertRow:
			m.opVInsertRow(op)

		case OpVInsert:
			if werr := m.opVInsert(op); werr != nil {
				return nil, werr
			}

		case OpSchemaWritePre:
			if werr := m.opSchemaWritePre(op); werr != nil {
				return nil, werr
			}

		case OpSchemaWriteRow:
			m.opSchemaWriteRow(op)

		case OpSchemaWrite:
			if werr := m.opSchemaWrite(op); werr != nil {
				return nil, werr
			}

		case OpDdl:
			if werr := m.opDdl(op); werr != nil {
				return nil, werr
			}

		case OpTxn:
			if werr := m.opTxn(op); werr != nil {
				return nil, werr
			}

		default:
			return nil, fmt.Errorf("engine: vdbe: unknown opcode %d", op.Op)
		}
		pc++
	}
	m.done = true
	return rows, nil
}

// jumpIf implements OpIf (wantTrue) / OpIfNot (!wantTrue): it reports whether
// the branch is taken. A NULL register takes the branch iff jumpIfNull!=0,
// exactly like SQLite's OP_If/OP_IfNot P3 semantics; otherwise the branch is
// taken when the register's truthiness (isTruthy, the same test WHERE uses)
// equals wantTrue.
func (m *vdbe) jumpIf(reg, jumpIfNull int, wantTrue bool) bool {
	v := m.regs[reg]
	if v.Typ == Null {
		return jumpIfNull != 0
	}
	return isTruthy(v) == wantTrue
}

// compareOp implements the six comparison opcodes, OP_Eq..OP_Ge at the value
// level: P5's affinity nibble is applied to local copies of both operands
// (never the registers), then compared under P4's collation. With p5NullEq
// (IS / IS NOT) NULLs compare equal and the result is definite; otherwise a
// NULL operand yields NULL in store mode, or jumps iff p5JumpIfNull. jump is
// false in store mode (the result went to r[P2]).
func (m *vdbe) compareOp(op *Instruction) (jump bool, target int) {
	l := m.regs[op.P3] // left operand
	r := m.regs[op.P1] // right operand
	nullEq := op.P5&p5NullEq != 0

	// "Common case of comparison of two integers", vdbe.c:2288-2314. C tests
	// `(flags1 & flags3 & MEM_Int)!=0` first -- before NULL handling (:2315),
	// the affinity block (:2346) and collation (:2383) -- and answers from the
	// two i64s. The order matters: with TEXT affinity, two integers would
	// otherwise be stringified and "10 < 9" become true. C's own TEXT branch is
	// guarded by `((flags1|flags3) & MEM_Str)!=0` (:2360) for the same reason.
	// p5 is irrelevant here: neither operand is NULL, and collation applies
	// only to TEXT.
	if l.Typ == Int && r.Typ == Int {
		res := 0
		switch {
		case l.I > r.I:
			res = 1
		case l.I < r.I:
			res = -1
		}
		return m.deliverCompare(op, compareResult(op.Op, res))
	}

	if l.Typ == Null || r.Typ == Null {
		if !nullEq {
			// Genuine NULL comparison: result is NULL.
			if op.P5&p5StoreP2 != 0 {
				m.regs[op.P2] = Value{Typ: Null}
				return false, 0
			}
			if op.P5&p5JumpIfNull != 0 {
				return true, op.P2
			}
			return false, 0
		}
		// p5NullEq: NULLs compare equal, one-NULL compares unequal.
		bothNull := l.Typ == Null && r.Typ == Null
		res := compareResult(op.Op, bothNullOrder(bothNull))
		return m.deliverCompare(op, res)
	}

	if aff := affinity(op.P5 & p5AffMask); aff != affNone {
		// Skipped outright when the coercion is provably the identity: this is
		// the innermost step of every filtered scan, and the call costs a
		// 48-byte Value copy in and another out even for the overwhelmingly
		// common integer-column-vs-integer-key comparison it leaves untouched.
		if !affinityIsIdentity(l.Typ, aff) {
			l = applyAffinityToValue(l, aff)
		}
		if !affinityIsIdentity(r.Typ, aff) {
			r = applyAffinityToValue(r, aff)
		}
	}
	// P4 carries the collating sequence (compCollation). It only matters for a
	// TEXT/TEXT pair; numeric and BLOB comparisons ignore it. Absent means
	// BINARY.
	coll, _ := op.P4.(string)
	if coll == "" {
		coll = "BINARY"
	}
	res := compareResult(op.Op, compareValuesCollatedEnc(l, r, coll, m.encoding()))
	return m.deliverCompare(op, res)
}

// deliverCompare writes a definite boolean comparison outcome either into
// r[P2] (store mode) or as a jump decision (jump mode).
func (m *vdbe) deliverCompare(op *Instruction, res bool) (jump bool, target int) {
	if op.P5&p5StoreP2 != 0 {
		m.regs[op.P2] = boolValue(res)
		return false, 0
	}
	if res {
		return true, op.P2
	}
	return false, 0
}

// bothNullOrder maps the p5NullEq "both NULL?" question onto a compareValues-
// style ordering result (0 == equal) so compareResult can decide each operator.
func bothNullOrder(bothNull bool) int {
	if bothNull {
		return 0 // equal
	}
	return 1 // unequal (order sign is irrelevant for =/<> under NULLEQ)
}

// compareResult turns a compareValues sign (-1/0/1) into the boolean outcome
// of one comparison opcode.
func compareResult(opc OpCode, c int) bool {
	switch opc {
	case OpEq:
		return c == 0
	case OpNe:
		return c != 0
	case OpLt:
		return c < 0
	case OpLe:
		return c <= 0
	case OpGt:
		return c > 0
	case OpGe:
		return c >= 0
	}
	return false
}

// arithOpName maps an arithmetic opcode back to the operator string evalArith
// expects, so the VM reuses this package's shared arithmetic verbatim.
func arithOpName(opc OpCode) string {
	switch opc {
	case OpAdd:
		return "+"
	case OpSubtract:
		return "-"
	case OpMultiply:
		return "*"
	case OpDivide:
		return "/"
	case OpRemainder:
		return "%"
	}
	return ""
}

// paramAt resolves a 1-based bound-parameter index against params, returning
// NULL for an out-of-range reference (an unbound host parameter is NULL).
func paramAt(params []Value, idx int) Value {
	if idx < 1 || idx > len(params) {
		return Value{Typ: Null}
	}
	return params[idx-1]
}

// copyValue makes a deep copy of v (its byte slice included), for OpCopy.
// fkDrain applies the foreign key work of the row the write opcode just stored,
// before anything after it in the program -- its AFTER triggers included. See
// DB.fkDrain.
func (m *vdbe) fkDrain() error {
	if m.wctx == nil || m.wctx.db == nil || m.wctx.db.fkStmt == nil {
		return nil
	}
	return m.wctx.db.fkDrain(m.wctx.db.fkStmt)
}

func copyValue(v Value) Value {
	if v.S != nil {
		s := make([]byte, len(v.S))
		copy(s, v.S)
		v.S = s
	}
	return v
}

// recAlloc returns n Values for one OpMakeRecord result, carved from a
// bump-allocated arena.
//
// Records cannot be reused across rows: sorters, OpWindowAppend and storeRow
// retain them. But successive records can take disjoint slices of one chunk,
// with the same lifetime, for one allocation per chunk instead of per row. The
// three-index slice caps each record so an append cannot spill into the next.
//
// The chunk grows by doubling from the first record's width, because a
// single-row INSERT makes one small record (a fixed 256-Value chunk wasted
// most of it) while a scan makes hundreds of thousands (per-record allocation
// is far worse). Doubling reaches recChunkValues after a few small chunks.
func (m *vdbe) recAlloc(n int) []Value {
	const recChunkValues = 256
	if len(m.recChunk) < n {
		size := 2 * m.recChunkSize
		if size > recChunkValues {
			size = recChunkValues
		}
		if size < n {
			size = n // the first chunk, and any record wider than the cap
		}
		m.recChunkSize = size
		m.recChunk = make([]Value, size)
	}
	out := m.recChunk[:n:n]
	m.recChunk = m.recChunk[n:]
	return out
}
