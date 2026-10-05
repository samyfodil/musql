// Writable virtual tables: the optional VtabUpdater interface (SQLite's
// xUpdate) a module implements to accept INSERT/UPDATE/DELETE, the vtabStore
// contract the engine uses to persist, clone and read a writable vtab's rows,
// and the module side of DML whose target is a virtual table (the compiled side
// is vdbe_vtab_write.go).
//
// A writable vtab's rows are stored in the database file like an ordinary
// table's and recovered at open (schema_load_objects.go), so it gets durability
// and transaction rollback (txSnapshot clones the store) without the read path
// needing a reference to the live *DB.
package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// vtabConstraintErr marks a vtabStore write error as SQLITE_CONSTRAINT-class,
// the only kind OP_VUpdate's OR IGNORE drops: with that rc and
// SQLITE_VTAB_CONSTRAINT_SUPPORT declared (VTable.bConstraint, vtab.c:1353),
// OE_Ignore rewrites rc to SQLITE_OK (vdbe.c:8750-8759); every other failure
// aborts the statement.
//
// rtree (rtree.c:3656) and fts3/fts4 (fts3.c:642) declare that support
// unconditionally. fts5 declares it only for eContent==FTS5_CONTENT_NORMAL
// (fts5_main.c:445), so external-content, contentless and
// contentless_unindexed=1 tables get no forgiveness: "UPDATE OR IGNORE" over a
// rowid collision still errors there (vtab_fts5.go's
// updateRowMasked/UpdateRowSet gate on this).
type vtabConstraintErr struct{ err error }

func (e vtabConstraintErr) Error() string { return e.err.Error() }
func (e vtabConstraintErr) Unwrap() error { return e.err }

// vtabConstraint wraps err -- a vtabStore write's genuine constraint-
// violation return -- so insertIntoVtab/updateVtab's OR IGNORE handling can
// recognize it (vtabIgnorable). Error() delegates, so the plain (non-OR)
// path, which wraps every store error identically in "engine: INSERT/UPDATE
// %s: %w" regardless of this type, reports byte-identical text to before
// this existed.
func vtabConstraint(err error) error { return vtabConstraintErr{err} }

// vtabIgnorable reports whether err -- as returned directly from a
// vtabStore write, before insertIntoVtab/updateVtab wrap it in their own
// "engine: ... %w" -- is a vtabConstraintErr, i.e. genuinely eligible for an
// OR IGNORE clause to drop the row silently rather than abort the statement.
func vtabIgnorable(err error) bool {
	var ce vtabConstraintErr
	return errors.As(err, &ce)
}

// VtabUpdater is the OPTIONAL interface a VirtualTable's backing store
// implements to become WRITABLE, mirroring SQLite's xUpdate. A module whose
// store does not implement it is read-only, and INSERT/UPDATE/DELETE against
// its table is declined cleanly. Row values are the module's full declared
// columns, in declaration order.
type VtabUpdater interface {
	// InsertRow stores a new row and returns the rowid assigned to it (the
	// module decides how an absent/NULL rowid-alias column is auto-assigned).
	InsertRow(vals []Value) (rowid int64, err error)
	// UpdateRow replaces the row currently identified by oldRowid with vals and
	// returns the row's possibly-changed rowid.
	UpdateRow(oldRowid int64, vals []Value) (newRowid int64, err error)
	// DeleteRow removes the row identified by rowid.
	DeleteRow(rowid int64) error
}

// vtabSetListUpdater is the OPTIONAL extra a store implements when its UPDATE
// semantics depend on WHICH columns the SET list named, not only on the new
// row's values. C SQLite hands xUpdate a whole row and marks the untouched
// columns with sqlite3_value_nochange(); this is that mark, as a mask over the
// store's declared columns (so index 0 is the hidden rowid slot).
//
// fts5's contentless tables are the reason it exists: fts5ContentlessUpdate
// decides between "a %_content-only write", "delete and reindex" and an error
// purely from which columns are modified, and two of those three arrive here
// carrying byte-identical row values (fts5_contentless.go).
type vtabSetListUpdater interface {
	UpdateRowSet(oldRowid int64, vals []Value, modified []bool) (newRowid int64, err error)
}

// vtabUpdateRow applies one UPDATE to a store, preferring its SET-list-aware
// entry point when it has one.
func vtabUpdateRow(store vtabStore, oldRowid int64, vals []Value, modified []bool) (int64, error) {
	if su, ok := store.(vtabSetListUpdater); ok {
		return su.UpdateRowSet(oldRowid, vals, modified)
	}
	return store.UpdateRow(oldRowid, vals)
}

// vtabUpdateRowConflict is vtabUpdateRow's OR REPLACE-aware counterpart.
// updateVtab calls it for every pending row: the ordinary case is identical to
// vtabUpdateRow, while a row whose SET moves its rowid onto one an *fts5Store
// already holds gets fts5's forgiveness (fts5Store.UpdateRowReplace) instead of
// a UNIQUE error. updateVtab has already ensured at most one pending row both
// moves its rowid and carries REPLACE, and UpdateRowReplace equals
// updateRowMasked when the rowid does not move, so calling it unconditionally is
// safe.
func vtabUpdateRowConflict(store vtabStore, orAction conflictAction, oldRowid int64, vals []Value, modified []bool) (int64, error) {
	if orAction == conflictReplace {
		if fst, isFts5 := store.(*fts5Store); isFts5 && fst.updateReplaceForgiven() {
			newRowid, _, err := fst.UpdateRowReplace(oldRowid, vals, modified)
			return newRowid, err
		}
	}
	return vtabUpdateRow(store, oldRowid, vals, modified)
}

// vtabStore is a writable virtual table's live, in-session backing store. It
// is a VtabUpdater (writes) and a VirtualTable (reads, so a fresh Connect can
// drive it), plus the engine-internal hooks needed to present its rows,
// materialize/recover them through the file, and clone them for a transaction
// snapshot.
type vtabStore interface {
	VtabUpdater
	VirtualTable
	vtabColumns() []VtabColumn
	vtabRows() (rowids []int64, rows [][]Value) // full declared rows, ascending rowid
	vtabClone() vtabStore
	vtabLoadRow(rowid int64, record []Value)

	// vtabDetach makes every value this store holds own its bytes. A store outlives
	// its statement, and a segment value aliases the file mapping, which a file
	// rewrite unmaps; a later shadow-table write then read freed memory (fts5
	// "INSERT INTO t(t,rank) VALUES('secure-delete',1)" then a DELETE in a
	// transaction). It is on the interface so a new writable module cannot forget
	// it.
	vtabDetach()
}

// writableVtabModule is implemented by a VtabModule whose tables are writable:
// newWritableStore builds a fresh, empty backing store for a CREATE VIRTUAL
// TABLE from the table name (for error text) and the raw module arguments.
type writableVtabModule interface {
	newWritableStore(tableName string, args []string) (vtabStore, error)
}

// vtabColumnInfos builds the columnInfo slice (for WHERE/SET evaluation and the
// read path) from a store's declared VtabColumns.
func vtabColumnInfos(cols []VtabColumn) []columnInfo {
	out := make([]columnInfo, len(cols))
	for i, c := range cols {
		out[i] = columnInfo{Name: c.Name, DeclType: c.Type, Aff: vtabColumnAffinity(c), Hidden: c.Hidden, Unindexed: c.Unindexed}
	}
	return out
}

// vtabEvalScope builds a single-table evalCtx scope over a vtab's columns.
func vtabEvalScope(name string, cols []columnInfo) tableScope {
	return tableScope{name: name, cols: cols, colIndex: buildColIndex(cols), offset: 0}
}

// writeRowSelected is a decline: no caller reaches it. Every DELETE/UPDATE
// WHERE is compiled into the scan, as in C (updateVirtualTable hands it to
// sqlite3WhereBegin, update.c:1196/1273; delete.c:526, update.c:742; views
// through sqlite3MaterializeView, delete.c:142; an upsert's DO UPDATE through
// upsert.c:325-326), and opVWrite hands the module a non-nil selection (empty
// when nothing matched). A WHERE and a trigger WHEN are the same compiled
// primitive in C:
//
//	sqlite3ExprIfFalse(&sSubParse, pWhen, iEndTrigger, SQLITE_JUMPIFNULL);
//	                                                   -- trigger.c:1320 (WHEN)
//	sqlite3ExprIfFalse(pParse, pE, addrCont, SQLITE_JUMPIFNULL);
//	                                         -- wherecode.c:2683 (a WHERE term)
//
// If this error is ever seen, give that caller a compiled selection (RULE #1).
func writeRowSelected(ctx *evalCtx, where Expr) (bool, error) {
	return false, fmt.Errorf("%w: virtual-table write WHERE reached without a compiled row selection", errVDBEUnsupported)
}

// writeApplySetList writes an UPDATE's SET right-hand sides into newVals, which
// the caller seeded with the OLD row, so a RHS sees pre-update values, as C
// codes each changed column inside the scan beside the OP_VColumn carrying the
// unchanged ones (update.c:1282, 1284; an ordinary table at update.c:954).
//
// A negative setIdx entry is a target with no slot (fts3's rowid synonyms,
// fts3SetRowidSynonymNoOp): its RHS is still evaluated, then discarded, as C
// codes "SET rowid=expr" into apVal[1] (update.c:494-498, 1291) and fts3 reads
// the rowid from the docid cell instead (fts3_write.c:5762-5765). "UPDATE t SET
// rowid=abs(-9223372036854775807-1)" must still raise "integer overflow".
//
// The two passes are C's order: changed columns first (update.c:1282), the
// rowid last (update.c:1291), so "SET a=zeroblob(1e12), rowid=<overflow>" and
// the reverse both report "string or blob too big". Affinity is the caller's (a
// view UPDATE needs it, update.c:983; a vtab does not).
func writeApplySetList(ctx *evalCtx, newVals []Value, sets []assignment, setIdx []int) error {
	// A DECLINE, for the same reason writeRowSelected is one: every live caller
	// arrives with values already computed by the COMPILED scan and applies
	// them through writeApplySetListPre. Measured before changing it -- with
	// this body replaced by one that errors, the engine suite AND the whole
	// fts/vtab/rtree compat-harness set both produce their exact baselines,
	// including the rowid-synonym no-op cases, which take the Pre route.
	//
	// It is RULE #1's answer for a shape the compiler did not lower. If this is
	// ever seen, give that caller a compiled SET block.
	return fmt.Errorf("%w: virtual-table write SET reached without compiled values", errVDBEUnsupported)
}

// writeApplySetListPre is writeApplySetList's compiled twin: the right-hand
// sides were evaluated in the scan (update.c:1282), and vals holds them in
// statement order. Only the slot mapping remains, last-wins for a repeated
// target as C's aXRef fill (update.c:492). The two-pass split cannot apply
// here; compileVtabUpdateStmt ensures no slotless target reaches this route
// (see that check), so the negative-index skip is unreachable.
func writeApplySetListPre(newVals []Value, vals []Value, setIdx []int) error {
	if len(vals) != len(setIdx) {
		// Unreachable: OpVWriteRow's P3 is len(stmt.sets), which is what
		// setIdx is built from. Reported rather than indexed off the end
		// (AGENTS.md invariant 2).
		//
		// MEASURED as a mutation, and it ESCAPED: replacing this condition with
		// "false" fails no test in vtab_update_delete_codegen_test.go. That is
		// the correct outcome for a guard whose whole claim is unreachability
		// -- a test that caught it would be asserting the two widths CAN
		// disagree, which is the bug this exists to survive rather than a
		// property to pin. Same measurement and same reasoning as
		// compileViewMaterialize's column-count guard.
		return fmt.Errorf("engine: internal error: %d compiled SET values for %d targets", len(vals), len(setIdx))
	}
	for k, slot := range setIdx {
		if slot < 0 {
			continue
		}
		newVals[slot] = vals[k]
	}
	return nil
}

// resolveVtabInsertColumns maps an INSERT column list to positions in the
// vtab's declared columns (case-insensitive). When names is nil (implicit
// "INSERT INTO t VALUES(...)"), the target is every NON-HIDDEN column in
// declaration order -- so a module with hidden columns (fts5's leading hidden
// "rowid" slot) takes only its visible columns positionally, exactly like real
// SQLite. A module with no hidden columns (rtree) is unaffected: the implicit
// list is simply [0,1,...,n-1].
func resolveVtabInsertColumns(cols []columnInfo, displayName string, names []string) ([]int, error) {
	if names == nil {
		var idx []int
		for j, c := range cols {
			if !c.Hidden {
				idx = append(idx, j)
			}
		}
		return idx, nil
	}
	idx := make([]int, len(names))
	for i, n := range names {
		pos := vtabColumnPos(cols, n)
		if pos < 0 {
			// insert.c:1097-1098: an IDLIST entry matching no declared column
			// but spelling the rowid (sqlite3IsRowid) is the rowid
			// pseudo-column, xUpdate's argv[1], after OP_MustBeInt. rtree lands
			// here and ignores argv[1], taking the id column as rowid
			// (rtreeUpdate, rtree.c:3181): over rtree(id,minX,maxX),
			// "(rowid,minX,maxX) VALUES(5,1,2)" stores rowid 1;
			// "(rowid,id,minX,maxX) VALUES(5,9,1,2)" stores 9; 'x' as the rowid
			// is "datatype mismatch". So it is checked, then discarded.
			if isRowidAliasName(n) {
				// fts5 is reached here only by the "oid"/"_rowid_" spellings
				// -- "rowid" matches its synthetic slot by name above -- and
				// all three mean the same slot, so route them to it.
				if slot := vtabSyntheticRowidSlot(cols); slot >= 0 {
					idx[i] = slot
					continue
				}
				idx[i] = noColumnRowidTarget
				continue
			}
			return nil, fmt.Errorf("engine: table %s has no column named %s", displayName, n)
		}
		idx[i] = pos
	}
	return idx, nil
}

// vtabColumnPos is resolveVtabInsertColumns's declared-column lookup, shared
// with vtabCheckCommandRowids so the two can never disagree about which name
// reaches which slot.
func vtabColumnPos(cols []columnInfo, name string) int {
	for j, c := range cols {
		if equalFoldName(c.Name, name) {
			return j
		}
	}
	return -1
}

// vtabIsRowidTarget reports whether an INSERT column-list entry names C's rowid
// pseudo-column, argv[1] (only an explicit IDLIST can; insert.c:1077,
// 1097-1098). Two shapes: noColumnRowidTarget, for a module with no rowid
// column (rtree), and this engine's fts5 model's leading hidden "rowid" column
// (C fts5 declares none, so naming it names the pseudo-column, which fts5's
// xUpdate uses). An ordinary declared column is never it: "rtree(rowid, minX,
// maxX)" has a real column named rowid.
func vtabIsRowidTarget(cols []columnInfo, idx int) bool {
	if idx == noColumnRowidTarget {
		return true
	}
	return idx >= 0 && idx == vtabSyntheticRowidSlot(cols)
}

// vtabSyntheticRowidSlot returns the declared-column index this engine uses to
// model a module that really does take xUpdate's argv[1] as the row's rowid: a
// LEADING HIDDEN column named rowid, which is how vtab_fts5.go carries storage
// slot 0. Real fts5's declared schema has no such column, so all three rowid
// spellings land here. -1 for a module whose declared columns hold no such slot
// (rtree), where argv[1] is checked and then goes nowhere.
func vtabSyntheticRowidSlot(cols []columnInfo) int {
	if len(cols) > 0 && cols[0].Hidden && isRowidAliasName(cols[0].Name) {
		return 0
	}
	return -1
}

// vtabRowidMustBeInt ports OP_MustBeInt (vdbe.c:2105) as insert.c:1534 emits it
// for a vtab's rowid. NULL is skipped (OP_IsNull, insert.c:1531), letting the
// module assign one. Otherwise NUMERIC affinity must yield an integer, else
// "datatype mismatch" before the module is called: '5', '5.0' and 5.0 give rowid
// 5; 'x', 5.5 and x'0102' fail, as for an ordinary table (applyAffinityToValue
// + rowidFromValue).
func vtabRowidMustBeInt(v Value) (Value, error) {
	if v.Typ == Null {
		return v, nil
	}
	r, err := rowidFromValue(applyAffinityToValue(v, affInteger))
	if err != nil {
		return v, err
	}
	return Value{Typ: Int, I: int64(r)}, nil
}

// vtabNamesTable reports whether an INSERT column list names the table itself
// -- the fts5/fts3 COMMAND CHANNEL spelling, which never reaches the ordinary
// row path.
func vtabNamesTable(names []string, table string) bool {
	for _, n := range names {
		if strings.EqualFold(n, table) {
			return true
		}
	}
	return false
}

// vtabCheckCommandRowids applies vtabRowidMustBeInt to a COMMAND-CHANNEL
// INSERT's rowid argument, which never reaches insertIntoVtab's row loop.
// OP_MustBeInt is coded before OP_VUpdate whatever the row means, so an
// unusable rowid fails the statement before fts5 is handed the command at all:
// verified against the 3.53.3 oracle that "INSERT INTO t(t, rowid, a)
// VALUES('delete', 'x', 'zulu')" is "datatype mismatch" -- this engine used to
// perform the delete instead.
//
// rows is the command INSERT's VALUES rows ALREADY EVALUATED, one []Value per
// row, in stmt.cols order -- see insertIntoVtab's command-channel block for why
// the whole row is evaluated once, at that boundary, rather than here.
func vtabCheckCommandRowids(stmt *insertStmt, cols []columnInfo, rows [][]Value) error {
	if stmt.cols == nil || stmt.selectStmt != nil {
		return nil
	}
	for i, n := range stmt.cols {
		pos := vtabColumnPos(cols, n)
		if pos < 0 && !isRowidAliasName(n) {
			continue // the table's own name, rank, or a name the row path rejects
		}
		if pos >= 0 && !vtabIsRowidTarget(cols, pos) {
			continue
		}
		for _, row := range rows {
			if i >= len(row) {
				continue
			}
			if _, cerr := vtabRowidMustBeInt(row[i]); cerr != nil {
				return cerr
			}
		}
	}
	return nil
}

// vtabReturningPlan is the virtual-table analog of returningPlan (returning_
// write.go): a RETURNING clause resolved against a vtab's own declared
// columns rather than a *tableMeta. The two cannot share a type --
// returningPlan is keyed on a *tableMeta, and its rows are coded in-line by
// emitReturning (vdbe_write.go) off the register block the write itself
// built. A vtabStore row has neither: it comes back from a VtabUpdater
// already in hand as a []Value, not from this program's own registers and
// not looked up by rowid. So this file builds and captures its own, storing
// the result on the SAME *returningState the caller threads in (ret.plan/
// ret.rows are plain fields, set here directly).
type vtabReturningPlan struct {
	// caseSensitiveLike is the connection's "PRAGMA case_sensitive_like",
	// captured where the plan is BUILT (both call sites are *DB methods) so
	// captureVtabRow can hand it to an evalCtx that has no pager -- which is
	// the common case, since a pager is taken only when the list holds a
	// subquery. See evalCtx.caseSensitiveLike for the divergence.
	caseSensitiveLike bool

	scope   tableScope
	outCols []outputColumn
	names   []string
	// hasSubquery is true when any output expression contains one: computed
	// once so the per-row capture loop only pays for a fresh snapshot pager
	// (below) when a subquery could actually need to read it.
	hasSubquery bool

	// progs is one compiled program per output column (selfRowExpr, the seam CHECK
	// constraints, generated columns and index keys use; C's pParse->iSelfTab,
	// expr.c:5047-5068). C codes RETURNING the same way, against the written row's
	// registers under NC_UBaseReg (codeReturningTrigger, trigger.c:1074-1091). A
	// nil entry makes compileVtabInsertStmt refuse the statement (compiled()), so
	// one reaching captureVtabRow is a hard error.
	progs []*selfRowExpr
}

// compiled reports whether EVERY output column lowered, which is what
// compileVtabInsertStmt requires before it will lower an
// "INSERT INTO <vtab> ... RETURNING" at all.
//
// The bar is deliberately all-or-nothing. Lowering the statement while one
// output column still needed a separate evaluator would move the ROUTE without
// moving the EVALUATION -- compileVtabInsertStmt's own list named that as the
// reason RETURNING was declined in the first place, and it is the one outcome
// AGENTS.md Rule 1 must never reward.
func (p *vtabReturningPlan) compiled() bool {
	for _, prog := range p.progs {
		if prog == nil || prog.prog == nil {
			return false
		}
	}
	return true
}

// buildVtabReturningPlan resolves cols against scope (the vtab's columns, via
// vtabEvalScope) with buildReturningPlan's validation (expandSelectList,
// checkExprSupported, validateColumnRefs), always, so an invalid RETURNING
// column is rejected at prepare. pager, when non-nil, lets a column hold a
// subquery over a real table ("... RETURNING (SELECT b FROM t2)"); without one
// such a column comes back program-less.
func buildVtabReturningPlan(scope tableScope, cols []SelectColumn, mode colNameMode, pager *ReadOnlyPager) (*vtabReturningPlan, error) {
	outCols, err := expandSelectList(cols, []tableScope{scope}, mode)
	if err != nil {
		return nil, err
	}
	planCtx := &evalCtx{tables: []tableScope{scope}}
	names := make([]string, len(outCols))
	progs := make([]*selfRowExpr, len(outCols))
	hasSubquery := false
	for i, oc := range outCols {
		if err := checkExprSupported(oc.expr); err != nil {
			return nil, err
		}
		if err := validateColumnRefs(oc.expr, planCtx); err != nil {
			return nil, fmt.Errorf("engine: RETURNING: %w", err)
		}
		if containsSubquery(oc.expr) {
			hasSubquery = true
		}
		names[i] = oc.name
		// Compiled here, at the same "prepare" this function already is, so
		// there is exactly ONE evaluator per column -- two evaluators of one
		// expression is how this engine's worst wrong answers have arrived
		// (see maxEvalExprCallSites' MATCH-pair note,
		// no_interpreter_gate_test.go). An expression that will not lower
		// comes back nil, which captureVtabRow turns into a clean
		// errVDBEUnsupported.
		//
		// ignoreDbQualifier is false: RETURNING is resolved under
		// NC_UBaseReg (trigger.c:1075), which is not one of the two contexts
		// resolve.c:316 drops the database qualifier for.
		progs[i] = compileSelfRowExprPaged(scope, oc.expr, false, pager, pureCtxNone)
	}
	return &vtabReturningPlan{scope: scope, outCols: outCols, names: names, hasSubquery: hasSubquery, progs: progs}, nil
}

// captureVtabRow evaluates plan's output expressions against one row just
// written to vm.store (rowid + its full declared-column values) and appends
// the result directly to ret.rows -- see vtabReturningPlan's doc comment for
// why this bypasses returningState.capture. pager is non-nil only when plan
// needs one (a subquery is present); a plain column/expression RETURNING list
// costs nothing extra.
func captureVtabRow(ret *returningState, plan *vtabReturningPlan, rowid int64, vals []Value, args []Value, pager *ReadOnlyPager) error {
	ctx := &evalCtx{tables: []tableScope{plan.scope}, vals: vals, rowids: []Value{{Typ: Int, I: rowid}}, params: args, pager: pager,
		caseSensitiveLike: plan.caseSensitiveLike}
	out := make([]Value, len(plan.outCols))
	for i := range plan.outCols {
		var v Value
		var err error
		// Each column runs its compiled program (selfRowExpr.runnable answers a
		// nil entry); compileVtabInsertStmt's guard is plan.compiled(), so this
		// is the whole capture. A table-reading subquery lowers via
		// compileSelfRowExprPaged against the write path's subquery pager. A
		// non-runnable program is an error, never a second evaluator.
		prog := plan.progs[i]
		if !prog.runnable(ctx) {
			return fmt.Errorf("%w: RETURNING column %d of a virtual table write", errVDBEUnsupported, i)
		}
		v, err = prog.eval(ctx)
		if err != nil {
			return fmt.Errorf("engine: RETURNING: %w", err)
		}
		out[i] = v
	}
	ret.rows = append(ret.rows, out)
	if ret.plan == nil {
		// Signals "this statement HAS a RETURNING clause" to callers that key
		// on it (execReturningViaVM, vdbe_write.go) -- see returningState's
		// own doc comment. tbl stays nil: nothing here ever reads it, because
		// the capture loop above is this file's own.
		ret.plan = &returningPlan{names: plan.names}
	}
	return nil
}

// captureVtabCommandRow is the RETURNING capture for a command-channel insert
// ("INSERT INTO g(g) VALUES('rebuild')", and fts3/fts4's): no row is stored, but
// the BEFORE trigger is still coded over regCols, holding nothing for user
// columns and the rowid substitution. "INSERT INTO g(g) VALUES('rebuild')
// RETURNING rowid, x, y" answers one row "-1, NULL, NULL" (fts5); "INSERT INTO
// f(f) VALUES('optimize') RETURNING x" one NULL row (fts4).
func captureVtabCommandRow(ret *returningState, vplan *vtabReturningPlan, cols []columnInfo, args []Value) error {
	if ret == nil || vplan == nil {
		return nil
	}
	return captureVtabRow(ret, vplan, vtabBeforeTriggerRowid,
		vtabCandidateRow(cols, make([]Value, len(cols)), vtabBeforeTriggerRowid), args, nil)
}

// vtabCandidateRow is the row a vtab's RETURNING reads: the tuple as written
// with each column's declared affinity applied (sqlite3TableAffinity,
// insert.c:1491, before the BEFORE trigger at :1494), never the module's stored
// image: "INSERT INTO q VALUES(1,2.7,3.9) RETURNING x0" over rtree_i32 answers
// 2.7/real (INT-declared coordinates, rtree.c:3695, leave a lossy value alone)
// though rtree stores REAL.
//
// The synthetic rowid slot carries beforeRowid, this engine's model of argv[1]
// (vtabSyntheticRowidSlot; regCols+0, filled at insert.c:1452-1465): "INSERT
// INTO g VALUES('a','b') RETURNING rowid" answers -1 per row while "SELECT
// rowid FROM g" then reads 1, 2, 3; an explicit rowid 7 answers 7.
func vtabCandidateRow(cols []columnInfo, full []Value, beforeRowid int64) []Value {
	out := make([]Value, len(full))
	copy(out, full)
	rowidSlot := vtabSyntheticRowidSlot(cols)
	for i := range out {
		if i >= len(cols) {
			break
		}
		if i == rowidSlot {
			out[i] = Value{Typ: Int, I: beforeRowid}
			continue
		}
		out[i] = applyAffinityToValue(out[i], cols[i].Aff)
	}
	return out
}

// vtabBeforeTriggerRowid is the rowid a virtual table's RETURNING clause sees
// when the statement did not supply one: -1, never the one the module goes on
// to assign. insert.c:1448-1454 -- "on a BEFORE trigger, we do not know what
// the unique ID will be ... so we substitute a rowid of -1" -- and a virtual
// table's RETURNING trigger is always a BEFORE one (trigger.c:843-853). A
// statement that DOES name a rowid in its column list passes that value
// through instead (insert.c:1461-1465); a NULL there falls back to this.
const vtabBeforeTriggerRowid = -1

// insertIntoVtab executes an INSERT whose target is the writable virtual table
// vm, the Go side of xUpdate. pre holds the statement's tuples already
// evaluated by the compiled route (OpVInsert), as OP_VUpdate receives them
// (vdbe.c:8736-8743); see vtabInsertRowValues.
//
// preWidth is the source's column count for "INSERT INTO <vtab> SELECT ...",
// -1 otherwise: C checks nColumn at prepare (insert.c:1249-1258), so a source
// yielding no rows still reports an arity mismatch (vtabInsertPlan.srcWidth).
func (db *DB) insertIntoVtab(vm *vtabMeta, stmt *insertStmt, pre [][]Value, preWidth int, args []Value, outer *evalCtx, ret *returningState) (int, error) {
	// An fts3/fts4 table has no vtabStore at all: its rows go into the
	// ordinary %_content/%_segdir/... shadow tables (vtab_fts3.go), so it
	// cannot go through the general per-row markVtabInsertRowid call below --
	// insertIntoFts3 (vtab_fts3.go) publishes its own rowid directly. The mark
	// is SPLIT for it: total_changes() still goes opaque (the module's own
	// shadow-table row counts are not reproduced), while the rowid is
	// published on the paths that know it -- and given up only when the
	// statement failed part-way, which leaves C SQLite reporting a row
	// this engine's staging never wrote.
	if fm, isFts3 := fts3ModuleOf(vm); isFts3 {
		db.totalChangesOpaque = true
		n, ferr := db.insertIntoFts3(vm, fm, stmt, pre, preWidth, args, outer, ret)
		if ferr != nil {
			db.markLastRowidOpaque()
		}
		return n, ferr
	}
	// A virtual table's module implements its own writes as ordinary SQL over
	// shadow tables on the same connection, which moves C SQLite's
	// last_insert_rowid()/total_changes() by amounts that are pure module
	// internals -- so those two stop being answerable for this session. See
	// markConnStateOpaque (conn_state.go) for the measurements.
	db.markConnStateOpaque()
	if vm.loadErr != nil {
		return 0, vm.loadErr
	}
	if vm.store == nil {
		return 0, fmt.Errorf("engine: table %s may not be modified (virtual table is read-only)", stmt.table)
	}
	cols := vtabColumnInfos(vm.store.vtabColumns())
	var vplan *vtabReturningPlan
	if stmt.returning != nil {
		// INSERT OR IGNORE ... RETURNING is served: a vtab's OP_VUpdate has no
		// jump to endOfLoop on OE_Ignore (vdbe.c:8750-8759 only rewrites rc),
		// so C's RETURNING fires for every source row, including an ignored
		// one ("INSERT OR IGNORE INTO r VALUES(1,9,9) RETURNING rowid,id,x0"
		// answers a row while r keeps the original id=1). The capture runs
		// before the module write, so the ignored row's output exists.
		if outer != nil {
			// C forbids RETURNING inside a trigger body (build.c:1443-1444).
			return 0, fmt.Errorf("engine: INSERT ... RETURNING inside a trigger body is not supported (C SQLite forbids RETURNING in triggers)")
		}
		if stmt.selectStmt != nil {
			// The per-row capture below is wired into the VALUES-sourced loop
			// only -- unmeasured for a SELECT source, so declined rather than
			// guessed.
			return 0, fmt.Errorf("engine: INSERT ... SELECT ... RETURNING into a virtual table is not supported by this write path")
		}
		// Resolved and validated even when ret is nil (the caller discards
		// rows): C rejects an invalid RETURNING column at prepare.
		scope := vtabEvalScope(stmt.table, cols)
		mode := colNameMode{full: db.fullColumnNames, short: !db.shortColumnNamesOff}
		// The SAME frozen snapshot the compile-time build used
		// (declineVtabInsertReturning, vdbe_vtab_write.go), so a RETURNING
		// subquery over a real table lowers on BOTH builds. Passing nil here
		// left this second plan program-less and tripped the compiled route's
		// own "not lowered" assertion below at RUN time, after the compile had
		// already accepted the statement.
		retPager, rperr := db.writeSubqueryPager()
		if rperr != nil {
			return 0, rperr
		}
		p, verr := buildVtabReturningPlan(scope, stmt.returning, mode, retPager)
		if p != nil {
			p.caseSensitiveLike = db.caseSensitiveLike
		}
		if verr != nil {
			return 0, verr
		}
		if pre != nil && !p.compiled() {
			// compileVtabInsertStmt lowers a statement only if every RETURNING
			// column lowers (declineVtabInsertReturning), so this is unreachable;
			// it is an error rather than a different evaluator.
			return 0, fmt.Errorf("engine: INSERT into %s: RETURNING output column not lowered on the compiled route", stmt.table)
		}
		vplan = p
	}

	// An INSERT naming the fts5 table itself is its command channel, not a
	// row (fts5_shadow.go's fts5CommandInsert). C fts5 receives values:
	// fts5UpdateMethod (fts5_main.c:1941) reads the command name and
	// argument from apVal[2+nCol] and apVal[2+nCol+1] (fts5_main.c:1976,
	// 1989), fts5SpecialDelete (fts5_main.c:1837) the 'delete' payload from
	// apVal[1] and &apVal[2], all registers insert.c coded (insert.c:1431,
	// 1561; vdbe.c:8736-8743). So the row is evaluated once here and every
	// command primitive takes values, as fts3's command channel does.
	if _, isFts5 := vm.store.(*fts5Store); isFts5 {
		var cmdRows [][]Value
		if vtabNamesTable(stmt.cols, vm.name) && stmt.selectStmt == nil {
			// A command row is not a table row -- its "columns" are the table's
			// own name and rank, which map to no declared slot -- so the only
			// part of vtabInsertRowValues's target list that applies is its
			// arity. (A SELECT-sourced command is left to fts5CommandInsert's
			// own decline below; unmeasured.)
			var cerr error
			if cmdRows, cerr = db.vtabInsertRowValues(stmt, make([]int, len(stmt.cols)), pre, preWidth, args, outer); cerr != nil {
				return 0, cerr
			}
			if err := vtabCheckCommandRowids(stmt, vtabColumnInfos(vm.store.vtabColumns()), cmdRows); err != nil {
				return 0, err
			}
		}
		if handled, n, cerr := db.fts5CommandInsert(vm, stmt, cmdRows); handled {
			if cerr == nil {
				if rerr := captureVtabCommandRow(ret, vplan, cols, args); rerr != nil {
					return n, rerr
				}
			}
			return n, cerr
		}
	}
	// OR IGNORE is the one non-ABORT action OP_VUpdate handles generically
	// for any vtab (vdbe.c:8750; insert.c:1562 puts the action in P5).
	// REPLACE is per module (rtree.c:3191; fts5UpdateMethod), and
	// FAIL/ROLLBACK and UPSERT (rejected for any vtab, insert.c:1291-1294)
	// stay declined.
	if stmt.upsert != nil || (stmt.orAction != conflictAbort && stmt.orAction != conflictIgnore && stmt.orAction != conflictReplace) {
		return 0, fmt.Errorf("engine: INSERT into a virtual table with an OR/ON CONFLICT/UPSERT clause is not supported by this write path")
	}
	if stmt.orAction == conflictReplace {
		if _, isFts5 := vm.store.(*fts5Store); !isFts5 {
			// Ported for fts5 only (vtabInsertRowOne below) -- the only store
			// this bucket's measured statements exercise. rtree's own REPLACE
			// support (rtree.c:3179-3200 -- unconditional, no content-mode gate
			// at all, simpler than fts5's) is feasible but UNMEASURED: no mined
			// TCL corpus statement anywhere under ext/rtree/test/ uses REPLACE.
			// Left declined here exactly as before rather than guessed at.
			return 0, fmt.Errorf("engine: INSERT into a virtual table with an OR REPLACE clause is not supported by this write path")
		}
	}
	// "INSERT INTO <vtab> DEFAULT VALUES" used to be declined here. It is
	// served now, by vtabInsertRowValues' own defaultValues arm -- see it for
	// the C and for the oracle measurements. Nothing else on this path needed
	// to change: the arm hands the row loop below one all-NULL row of exactly
	// the width colIdx resolves to, which is what the C's coding loop produces
	// for a target whose columns declare no DEFAULT (sqlite3ColumnExpr returns
	// 0 and the coder emits OP_Null, insert.c:1404-1410) -- and neither fts3,
	// fts5 nor rtree declares one in its sqlite3_declare_vtab schema.
	colIdx, err := resolveVtabInsertColumns(cols, stmt.table, stmt.cols)
	if err != nil {
		return 0, err
	}
	// Both spellings -- a VALUES list and a source SELECT -- come back as one
	// []Value per row in target-column order, through the same helper fts3's
	// own write path uses (vtab_fts3.go's vtabInsertRowValues), including its
	// two guards for the SELECT form.
	valueRows, err := db.vtabInsertRowValues(stmt, colIdx, pre, preWidth, args, outer)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, vals := range valueRows {
		full := make([]Value, len(cols)) // NULL for any unprovided column
		// The rowid a BEFORE trigger sees (vtabBeforeTriggerRowid): -1 unless
		// the statement's own column list NAMED a rowid and supplied a
		// non-NULL one, in which case it is that value. insert.c:1452-1465 --
		// "if( ipkColumn<0 ){ OP_Integer -1 }else{ code the expression;
		// OP_NotNull past an OP_Integer -1; OP_MustBeInt }" -- with
		// ipkColumn set from the IDLIST alone for a virtual table, whose
		// pTab->iPKey is -1, by insert.c:1096's sqlite3IsRowid branch.
		beforeRowid := int64(vtabBeforeTriggerRowid)
		for i, v := range vals {
			if vtabIsRowidTarget(cols, colIdx[i]) {
				cv, cerr := vtabRowidMustBeInt(v)
				if cerr != nil {
					return 0, cerr
				}
				v = cv
				if v.Typ == Int {
					beforeRowid = v.I
				}
			}
			if colIdx[i] == noColumnRowidTarget {
				// The C's argv[1], which this module does not read -- see
				// resolveVtabInsertColumns for rtree's own measurement. It has
				// been checked; it stores nowhere.
				continue
			}
			full[colIdx[i]] = v
		}
		// A RETURNING subquery's pager is snapshotted before this row's
		// InsertRow but after the statement's earlier rows: "INSERT INTO
		// t1(a,b,c) VALUES(3,6,7),(4,8,9) RETURNING (SELECT count(*) FROM t1)"
		// over a one-row t1 answers 2 then 3. One whole-statement snapshot
		// would give both rows the same count.
		var pager *ReadOnlyPager
		if ret != nil && vplan != nil && vplan.hasSubquery {
			pager, err = db.SnapshotPager()
			if err != nil {
				return count, err
			}
		}
		// The RETURNING row is captured here, before the module write, over
		// the candidate tuple: for a vtab, triggersReallyExist forces
		// TRIGGER_BEFORE (trigger.c:843-853), coded at insert.c:1494 over
		// regCols with only sqlite3TableAffinity applied (insert.c:1491) and a
		// rowid of -1 (insert.c:1448-1454); OP_VUpdate never writes the
		// assigned rowid back.
		//
		//	INSERT INTO r VALUES(1,2,3) RETURNING rowid,id,x0,typeof(x0)
		//	                                  -> -1 | 1 | 2.0 | real
		//	INSERT INTO r(id,x0,x1) VALUES(NULL,4,5) RETURNING id,rowid
		//	                                  -> NULL | -1
		//	INSERT INTO q VALUES(1,2.7,3.9) RETURNING x0,typeof(x0)   (rtree_i32)
		//	                                  -> 2.7 | real
		//	INSERT INTO f VALUES('c') RETURNING (SELECT count(*) FROM f)
		//	                                  -> the count BEFORE this row
		if ret != nil && vplan != nil {
			if cerr := captureVtabRow(ret, vplan, beforeRowid, vtabCandidateRow(cols, full, beforeRowid), args, pager); cerr != nil {
				return count, cerr
			}
		}
		rowid, ierr := db.vtabInsertRowOne(vm, stmt, full)
		if ierr != nil {
			if stmt.orAction == conflictIgnore && vtabIgnorable(ierr) {
				// vdbe.c:8750 -- OE_Ignore rewrites this row's SQLITE_CONSTRAINT
				// rc to SQLITE_OK before "if(rc) goto abort_due_to_error", so
				// the row is silently dropped: not counted (p->nChange++ sits
				// in the ELSE of that same rc check), no RETURNING row (below),
				// and last_insert_rowid() untouched -- its own db->lastRowid
				// assignment only fires when rc==SQLITE_OK, which this row's
				// never was (see markVtabInsertRowid).
				continue
			}
			return 0, fmt.Errorf("engine: INSERT into %s: %w", stmt.table, ierr)
		}
		db.markVtabInsertRowid(rowid)
		count++
		// An r-tree's rows live in %_node (rtree_shadow.go), and the
		// per-row snapshot taken at the TOP of the next iteration reads them
		// from the file image -- so for that snapshot to see this row, as the
		// oracle measurement above requires ("answers 2 then 3"), the shadows
		// have to be re-encoded HERE and not only once at the end. Gated on
		// the statement actually taking those snapshots, because the
		// re-encode is a whole-tree bulk load: paying it per row for every
		// ordinary multi-row INSERT would be quadratic for nothing.
		if vplan != nil && vplan.hasSubquery {
			if serr := db.rtreeSyncIfNeeded(vm); serr != nil {
				return count, serr
			}
		}
	}
	if serr := db.fts5SyncIfNeeded(vm); serr != nil {
		return 0, serr
	}
	if serr := db.rtreeSyncIfNeeded(vm); serr != nil {
		return 0, serr
	}
	return count, nil
}

// vtabInsertRowOne inserts one resolved row (declared column order, rowid
// first) into vm's store, honoring OR REPLACE for fts5: "If the conflict-mode
// is REPLACE, first remove the current entry (if any)" (fts5UpdateMethod,
// fts5_main.c:2044-2051). Anything else is vm.store.InsertRow.
//
// A displacing row counts as a removal for fts5's secure-delete bump
// (fts5SecureDeleteGuard/fts5NoteRowsRemoved), checked before
// fst.InsertRowReplace mutates anything, with the exact count: whether this
// row collides is one read-only lookup.
func (db *DB) vtabInsertRowOne(vm *vtabMeta, stmt *insertStmt, full []Value) (int64, error) {
	if stmt.orAction != conflictReplace {
		return vm.store.InsertRow(full)
	}
	fst, isFts5 := vm.store.(*fts5Store)
	if !isFts5 || !fst.replaceForgiven() {
		return vm.store.InsertRow(full)
	}
	if full[0].Typ != Null {
		if _, exists := fst.rows[fts5RowidValue(full[0])]; exists {
			// Not deferrable: this call site predates fts5SecureDeleteGuard's
			// deferrable parameter and was never verified against the
			// in-transaction deferral path, unlike deleteVtab's own displaced
			// removal. Keeping it a hard decline here preserves exactly the
			// behavior this INSERT OR REPLACE path already shipped and tested.
			if gerr := db.fts5SecureDeleteGuard(vm, stmt.table, 1, 1, false); gerr != nil {
				return 0, gerr
			}
		}
	}
	rowid, removed, err := fst.InsertRowReplace(full)
	if err != nil {
		return 0, err
	}
	if removed {
		db.fts5NoteRowsRemoved(vm, 1)
	}
	return rowid, nil
}

// deleteVtab executes a DELETE whose target is the writable virtual table vm.
//
// outer is the enclosing trigger row when this is a trigger body step (as for
// insertIntoVtab): "CREATE TRIGGER r1 AFTER UPDATE ON t1 BEGIN DELETE FROM t2
// WHERE id = OLD.a; END" over an rtree t2 (alter.test 17.100). nil at top
// level.
//
// pre is the compiled route's row selection (OpVWrite): the WHERE was coded
// into a scan of this row source and already chose the rowids.
func (db *DB) deleteVtab(vm *vtabMeta, stmt *deleteStmt, args []Value, outer *evalCtx, pre vtabWriteRowSet) (int, error) {
	// A virtual table's module implements its own writes as ordinary SQL over
	// shadow tables on the same connection, which moves C SQLite's
	// last_insert_rowid()/total_changes() by amounts that are pure module
	// internals -- so those two stop being answerable for this session. See
	// markConnStateOpaque (conn_state.go) for the measurements.
	db.markConnStateOpaque()
	// Asked HERE rather than only where a DELETE is dispatched, because the
	// COMPILED route reaches this function directly. fts3's
	// own fts3MutationCommon already sets it for that family; a store-backed
	// module (rtree, fts5) had nothing else that did.
	// An fts3/fts4 table has no vtabStore: its rows live in the ordinary
	// %_content/%_segdir/... shadow tables, and removing one appends a segment
	// of DELETE MARKERS rather than rewriting the index (fts3_write.go).
	if fm, isFts3 := fts3ModuleOf(vm); isFts3 {
		return db.deleteFromFts3(vm, fm, stmt, args, outer, pre)
	}
	if vm.loadErr != nil {
		return 0, vm.loadErr
	}
	if vm.store == nil {
		return 0, fmt.Errorf("engine: table %s may not be modified (virtual table is read-only)", stmt.table)
	}
	if stmt.returning != nil {
		return 0, fmt.Errorf("engine: DELETE ... RETURNING from a virtual table is not supported by this write path")
	}
	if containsSubquery(stmt.where) {
		return 0, fmt.Errorf("engine: DELETE from a virtual table with a subquery in WHERE is not supported by this write path")
	}
	cols := vtabColumnInfos(vm.store.vtabColumns())
	scope := vtabEvalScope(stmt.table, cols)
	if fst, isFts5 := vm.store.(*fts5Store); isFts5 {
		scope.isFts5 = true
		scope.fts5Tok = fst.tok
		scope.fts5Detail = fst.detail
	}
	rowids, rows := vm.store.vtabRows()
	var toDelete []int64
	for i, rid := range rowids {
		if pre != nil {
			// The compiled route's scan read THIS row source (vtabWriteRows),
			// so "selected" is a membership test and not a second opinion.
			if _, ok := pre[rid]; !ok {
				continue
			}
		} else if stmt.where != nil {
			ctx := &evalCtx{tables: []tableScope{scope}, vals: rows[i], rowids: []Value{{Typ: Int, I: rid}}, params: args, outer: outer}
			sel, everr := writeRowSelected(ctx, stmt.where)
			if everr != nil {
				return 0, fmt.Errorf("engine: DELETE from %s: %w", stmt.table, everr)
			}
			if !sel {
				continue
			}
		}
		toDelete = append(toDelete, rid)
	}
	// The rows are removed in ascending ROWID order, not scan order: a virtual
	// table is a rowid table to delete.c, so its two-pass DELETE collects the
	// matches into a RowSet (OP_RowSetAdd, delete.c:582) and reads them back
	// with OP_RowSetRead (:626), which yields them sorted. It is observable
	// wherever a removal's effect depends on what is still there -- an r-tree's
	// node bytes, which C's delete-order leaves behind.
	slices.Sort(toDelete)
	// fts5's 'secure-delete' turns the first REMOVAL into a %_config version
	// bump. Inside a transaction that bump is DEFERRABLE for a DELETE
	// (fts5_txn.go); fts5NoteRowsRemoved below defers it.
	if gerr := db.fts5SecureDeleteGuard(vm, stmt.table, len(toDelete), len(toDelete), true); gerr != nil {
		return 0, gerr
	}
	for _, rid := range toDelete {
		if derr := vm.store.DeleteRow(rid); derr != nil {
			return 0, fmt.Errorf("engine: DELETE from %s: %w", stmt.table, derr)
		}
	}
	db.fts5NoteRowsRemoved(vm, len(toDelete))
	if serr := db.fts5SyncIfNeeded(vm); serr != nil {
		return 0, serr
	}
	// A DELETE that removed nothing wrote no node: rtree.c's xUpdate never
	// ran, so a missing %_node goes unnoticed there too (rtreeA.test 3.3.0).
	if len(toDelete) > 0 {
		if serr := db.rtreeSyncIfNeeded(vm); serr != nil {
			return 0, serr
		}
	}
	return len(toDelete), nil
}

// updateVtab executes an UPDATE whose target is the writable virtual table vm.
// outer is the enclosing TRIGGER row context, exactly as in deleteVtab: an
// OLD./NEW. reference in this UPDATE's WHERE or SET resolves through it.
//
// pre is the COMPILED route's already-decided row selection, carrying one more
// thing than a DELETE's: each selected row's SET right-hand sides, already
// evaluated, in STATEMENT order. See deleteVtab's own note and vtabWriteRowSet.
func (db *DB) updateVtab(vm *vtabMeta, stmt *updateStmt, args []Value, outer *evalCtx, pre vtabWriteRowSet) (int, error) {
	// A virtual table's module implements its own writes as ordinary SQL over
	// shadow tables on the same connection, which moves C SQLite's
	// last_insert_rowid()/total_changes() by amounts that are pure module
	// internals -- so those two stop being answerable for this session. See
	// markConnStateOpaque (conn_state.go) for the measurements.
	db.markConnStateOpaque()
	// See deleteVtab's identical line: the COMPILED route reaches this function
	// directly, rather than through the dispatch that used to set a
	// store-backed module's own disqualification.
	// See deleteVtab: an fts3/fts4 table's rows are its shadow tables', and an
	// UPDATE is a delete and an insert into the same index segment.
	if fm, isFts3 := fts3ModuleOf(vm); isFts3 {
		return db.updateFts3(vm, fm, stmt, args, outer, pre)
	}
	if vm.loadErr != nil {
		return 0, vm.loadErr
	}
	if vm.store == nil {
		return 0, fmt.Errorf("engine: table %s may not be modified (virtual table is read-only)", stmt.table)
	}
	if stmt.from != nil {
		return 0, fmt.Errorf("engine: UPDATE ... FROM against a virtual table is not supported by this write path")
	}
	if stmt.returning != nil {
		return 0, fmt.Errorf("engine: UPDATE ... RETURNING against a virtual table is not supported by this write path")
	}
	// OR IGNORE only -- see insertIntoVtab's identical gate for why it alone
	// of the non-ABORT actions is generic (vdbe.c:8750; update.c:1348 threads
	// the statement's action into OP_VUpdate's P5 exactly like insert.c does).
	// REPLACE, like insertIntoVtab's own gate, is ported for fts5 only.
	if stmt.orAction != conflictAbort && stmt.orAction != conflictIgnore && stmt.orAction != conflictReplace {
		return 0, fmt.Errorf("engine: UPDATE against a virtual table with an OR clause is not supported by this write path")
	}
	if stmt.orAction == conflictReplace {
		if _, isFts5 := vm.store.(*fts5Store); !isFts5 {
			return 0, fmt.Errorf("engine: UPDATE against a virtual table with an OR clause is not supported by this write path")
		}
	}
	if containsSubquery(stmt.where) {
		return 0, fmt.Errorf("engine: UPDATE against a virtual table with a subquery in WHERE is not supported by this write path")
	}
	cols := vtabColumnInfos(vm.store.vtabColumns())
	setIdx := make([]int, len(stmt.sets))
	for i, a := range stmt.sets {
		if containsSubquery(a.expr) {
			return 0, fmt.Errorf("engine: UPDATE against a virtual table with a subquery in SET is not supported by this write path")
		}
		pos := -1
		for j, c := range cols {
			if equalFoldName(c.Name, a.col) {
				pos = j
				break
			}
		}
		if pos < 0 {
			return 0, fmt.Errorf("engine: UPDATE %s: no such column: %s", stmt.table, a.col)
		}
		setIdx[i] = pos
	}
	// The SET list as a per-column mask, for a store that needs to know which
	// columns were named rather than only what they now hold
	// (vtabSetListUpdater).
	modified := make([]bool, len(cols))
	for _, pos := range setIdx {
		modified[pos] = true
	}
	scope := vtabEvalScope(stmt.table, cols)
	if fst, isFts5 := vm.store.(*fts5Store); isFts5 {
		scope.isFts5 = true
		scope.fts5Tok = fst.tok
		scope.fts5Detail = fst.detail
	}
	rowids, rows := vm.store.vtabRows()

	type pending struct {
		oldRowid int64
		newVals  []Value
	}
	var pends []pending
	for i, rid := range rowids {
		var setVals []Value
		var ctx *evalCtx
		if pre != nil {
			// See deleteVtab's identical branch: membership, not a second
			// opinion. setVals are this row's SET right-hand sides as the
			// compiled scan evaluated them.
			sv, ok := pre[rid]
			if !ok {
				continue
			}
			setVals = sv
		} else {
			ctx = &evalCtx{tables: []tableScope{scope}, vals: rows[i], rowids: []Value{{Typ: Int, I: rid}}, params: args, outer: outer}
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
		newVals := append([]Value(nil), rows[i]...) // start from the OLD row
		if pre != nil {
			if serr := writeApplySetListPre(newVals, setVals, setIdx); serr != nil {
				return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, serr)
			}
		} else if everr := writeApplySetList(ctx, newVals, stmt.sets, setIdx); everr != nil {
			return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, everr)
		}
		pends = append(pends, pending{oldRowid: rid, newVals: newVals})
	}
	// An OR REPLACE that changes ITS OWN rowid can only ever be forgiven
	// against whatever the table held BEFORE this statement began: this write
	// path snapshots every pending row's old state into pends above and then
	// applies them afterward, so it cannot reproduce a cascade where one
	// row's displacement changes what a LATER row of the SAME statement
	// collides with (or no longer does) -- exactly the hazard fts3_write.go's
	// UPDATE path already declines for the identical reason (see its
	// "an OR REPLACE that displaces a row while updating more than one row"
	// error). An unchanged rowid can never collide, so only a SET list that
	// actually moves the rowid is at risk, and only when more than one row is
	// pending.
	var fts5Replace *fts5Store
	if stmt.orAction == conflictReplace {
		if fst, isFts5 := vm.store.(*fts5Store); isFts5 && fst.updateReplaceForgiven() {
			fts5Replace = fst
		}
	}
	replaceExtra := 0
	if fts5Replace != nil {
		for _, pd := range pends {
			target := pd.oldRowid
			if pd.newVals[0].Typ != Null {
				target = fts5RowidValue(pd.newVals[0])
			}
			if target == pd.oldRowid {
				continue
			}
			if len(pends) > 1 {
				return 0, fmt.Errorf("engine: UPDATE %s: an OR REPLACE that changes rowid while updating more than one row is not supported by this write path (C fts5 re-checks each row's rowid collision against whatever an earlier row of this same statement already wrote, and this write path snapshots every row's old state before any of them are applied)", stmt.table)
			}
			if _, exists := fts5Replace.rows[target]; exists {
				replaceExtra++
			}
		}
	}
	// An UPDATE removes the old row from the index before writing the new
	// one, counting for secure-delete like a DELETE (fts5_config.go), except
	// a %_content-only UPDATE of a contentless table, which removes nothing
	// (fts5UpdateRemovesRows). The guard runs before any row is touched, so
	// it uses len(pends) as an upper bound (an ignored row only lowers the
	// real count). replaceExtra adds the one extra removal a rowid-moving
	// OR REPLACE onto an occupied rowid causes (fts5_main.c:2072-2077),
	// computed exactly: the hazard check limits that to one pending row.
	nRemoved := fts5UpdateRemovesRows(vm, modified, len(pends)) + replaceExtra
	// UPDATE's bump is NOT deferrable (fts5SecureDeleteGuard's own comment
	// says why), so this stays a hard decline inside a transaction.
	if gerr := db.fts5SecureDeleteGuard(vm, stmt.table, len(pends), nRemoved, false); gerr != nil {
		return 0, gerr
	}
	applied := 0
	for _, pd := range pends {
		if _, uerr := vtabUpdateRowConflict(vm.store, stmt.orAction, pd.oldRowid, pd.newVals, modified); uerr != nil {
			if stmt.orAction == conflictIgnore && vtabIgnorable(uerr) {
				// vdbe.c:8750, same OE_Ignore mechanism as insertIntoVtab's
				// identical check: this row's rc was rewritten to SQLITE_OK
				// before p->nChange++ ran, so C SQLite's changes() does not
				// count it either -- it is simply left as it was.
				continue
			}
			return 0, fmt.Errorf("engine: UPDATE %s: %w", stmt.table, uerr)
		}
		applied++
	}
	// Recompute against how many rows ACTUALLY left the index (applied), not
	// the pre-loop match count the guard above conservatively used: an
	// OE_Ignore row never left it. fts5UpdateRemovesRows's answer only
	// depends on the SET list's shape, not on nSelected's value, so
	// recomputing with the smaller, real count is exact, not another
	// approximation.
	if applied != len(pends) {
		nRemoved = fts5UpdateRemovesRows(vm, modified, applied)
	}
	db.fts5NoteRowsRemoved(vm, nRemoved)
	if serr := db.fts5SyncIfNeeded(vm); serr != nil {
		return 0, serr
	}
	if applied > 0 { // see deleteVtab
		if serr := db.rtreeSyncIfNeeded(vm); serr != nil {
			return 0, serr
		}
	}
	return applied, nil
}
