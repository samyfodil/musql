// This file lowers the FROM-clause form of UPDATE -- "UPDATE t SET c = <expr>
// FROM <sources> [WHERE ...]" -- into the write VM. A shape it declines is an
// error.
//
// # What C does
//
// sqlite3Update is one function with a few "nChangeFrom" tests ("nChangeFrom =
// (pTabList->nSrc>1) ? pChanges->nExpr : 0;", update.c:395), each moving the
// same two-pass shape onto a different pass-one source; pass two is unchanged.
//
// Pass one is updateFromSelect:
//
//	if( nChangeFrom ){
//	  updateFromSelect(
//	      pParse, iEph, pPk, pChanges, pTabList, pWhere, pOrderBy, pLimit
//	  );
//	                                              -- update.c:693-696
//
// building "SELECT <target rowid>, <SET expressions> FROM <srclist> WHERE
// <where>" (update.c:187-273; buildUpdateFromSelect) into an ephemeral table
// (update.c:685) via SRT_Upfrom:
//
//	sqlite3VdbeAddOp2(v, OP_IsNull, regResult, iBreak); VdbeCoverage(v);
//	sqlite3VdbeAddOp3(v, OP_MakeRecord,
//	                  regResult+(i2<0), nResultCol-(i2<0), r1);
//	if( i2<0 ){
//	  sqlite3VdbeAddOp3(v, OP_Insert, iParm, r1, regResult);
//	                                              -- select.c:1366-1373
//
// For a rowid target that is an INTKEY table keyed by target rowid with the
// SET tuple as its record. Three visible consequences:
//
//   - a target row matched several times keeps one entry: OP_Insert overwrites,
//     so the last join row (in plan order) wins;
//   - pass two walks it with OP_Rewind/OP_Next, so rows are visited in
//     ascending target rowid -- which is why triggers fire in rowid order
//     (upfrom2.test 1.1);
//   - an aggregate SET matching nothing still yields one row with a NULL key,
//     dropped by OP_IsNull ("If the UPDATE FROM join is an aggregate that
//     matches no rows ... Don't record that empty row in the output table.",
//     select.c:1363-1365).
//
// Pass two is update.c:847-866's "}else if( pPk || nChangeFrom ){" arm, the
// ordinary per-row loop reading SET values from the ephemeral table:
//
//	sqlite3VdbeAddOp2(v, OP_Rewind, iEph, labelBreak); VdbeCoverage(v);
//	addrTop = sqlite3VdbeCurrentAddr(v);
//	...
//	sqlite3VdbeAddOp2(v, OP_Rowid, iEph, regOldRowid);
//	sqlite3VdbeAddOp3(
//	    v, OP_NotExists, iDataCur, labelContinue, regOldRowid
//	); VdbeCoverage(v);
//	                                              -- update.c:849-864
//
//	if( nChangeFrom ){
//	  int nOff = (isView ? pTab->nCol : nPk);
//	  assert( eOnePass==ONEPASS_OFF );
//	  sqlite3VdbeAddOp3(v, OP_Column, iEph, nOff+j, k);
//	}else{
//	  sqlite3ExprCode(pParse, pChanges->a[j].pExpr, k);
//	}
//	                                              -- update.c:949-955
//
// nOff is 0 for a rowid table, so eph column j is SET expression j. Everything
// after (OLD.* snapshot, NOT NULL, generated columns, STRICT, CHECK,
// MakeRecord, triggers, store) is the single-table UPDATE's code, so
// compileUpdateStmt has exactly three seams: the loop driver, where SET values
// come from, and no WHERE in pass two.
//
// # The multi-match winner
//
// C's answer follows the query plan (see update_from.go), which this cannot
// reproduce, so collapseUpfromRows declines a target row matched more than once
// unless all candidates computed the identical SET tuple (setValueTuplesEqual).
// It is a run-time error, since the ambiguity depends on the data, not the
// statement (compat-harness/update_from_multimatch_test.go).

package engine

import (
	"fmt"
	"sort"
)

// upfromDest is a derived source that is an UPDATE ... FROM pass-one
// destination: SQLite's SRT_Upfrom ephemeral table (select.c:1355). Present on
// derivedSource exactly when OpOpenDerived must COLLAPSE the sub-program's rows
// onto their target row and hand the result back keyed and ordered, instead of
// opening them as an ordinary unkeyed derived cursor.
type upfromDest struct {
	tbl *tableMeta
	// displayName is the target's name AS THE STATEMENT WROTE IT (schema
	// qualifier and all), for error messages.
	displayName string
	// nkey is how many leading sub-program output columns identify the target
	// row: 1 (its rowid) for an ordinary table, len(pkIndex.colIdx) for a
	// WITHOUT ROWID one. See buildUpdateFromSelect.
	nkey int
}

// collapseUpfromRows turns pass one's join rows into the ephemeral table pass
// two walks: one entry per target row, keyed by that row's row-store key, in
// ascending key order, holding the SET-value tuple alone.
//
// This IS select.c:1366-1373's SRT_Upfrom destination plus the OP_Rewind/OP_Next
// walk order update.c:849 gives it -- see this file's doc comment. The one
// deliberate difference is the multi-match rule, also described there.
func collapseUpfromRows(d *upfromDest, rows [][]Value) ([]int64, [][]Value, error) {
	keys := make([]int64, 0, len(rows))
	vals := make([][]Value, 0, len(rows))
	at := make(map[uint64]int, len(rows))
	for _, row := range rows {
		if row[0].Typ == Null {
			// select.c:1366's "OP_IsNull regResult, iBreak", which drops the
			// one row a zero-match aggregate join still produces. Applied
			// unconditionally, exactly as the C applies it: for a
			// non-aggregate join the target is comma-joined LAST
			// (buildUpdateFromSelect), so no outer join can null-extend it and
			// a real row's rowid/PRIMARY KEY can never itself be NULL -- the
			// aggregate case is the only one that reaches here.
			continue
		}
		rowid, kerr := identifyTargetRow(d.tbl, row[:d.nkey])
		if kerr != nil {
			return nil, nil, fmt.Errorf("engine: UPDATE %s: %w", d.displayName, kerr)
		}
		setVals := row[d.nkey:]
		if i, dup := at[rowid]; dup {
			if !setValueTuplesEqual(vals[i], setVals) {
				return nil, nil, fmt.Errorf("engine: UPDATE %s: a target row matched more than one FROM row; UPDATE ... FROM with an ambiguous (multi-match) join is not supported by this write path (C SQLite's choice of matching row is undefined)", d.displayName)
			}
			// Identical duplicate: the undefined choice of winner cannot change
			// the stored result, so the first-recorded tuple stands.
			continue
		}
		at[rowid] = len(keys)
		keys = append(keys, int64(rowid))
		vals = append(vals, setVals)
	}
	// Ascending target-key order -- the ephemeral table is a b-tree and
	// update.c:849 walks it with OP_Rewind/OP_Next. rowidLess is the row
	// store's own SIGNED comparison (btree_write.go), so rows are visited --
	// and their triggers fired -- in the order C's own pass two visits them.
	ord := make([]int, len(keys))
	for i := range ord {
		ord[i] = i
	}
	sort.Slice(ord, func(i, j int) bool { return rowidLess(uint64(keys[ord[i]]), uint64(keys[ord[j]])) })
	sKeys := make([]int64, len(keys))
	sVals := make([][]Value, len(vals))
	for i, o := range ord {
		sKeys[i], sVals[i] = keys[o], vals[o]
	}
	return sKeys, sVals, nil
}

// compileUpdateFromSource compiles pass one of an "UPDATE ... FROM" into the
// derived source compileUpdateStmt opens with OpOpenDerived, or declines with
// errVDBEUnsupported. It also returns the frozen snapshot the sub-program reads,
// which the caller must publish as Program.WritePager so the program is never
// cached across executions (see cachedWriteProgram) -- nil for a TRIGGER BODY,
// whose pass one is a LIVE sub-program holding no image at all.
func (db *DB) compileUpdateFromSource(stmt *updateStmt, tbl *tableMeta, trig *trigCompileCtx) (*derivedSource, *ReadOnlyPager, error) {
	if trig != nil && len(stmt.ctes) > 0 {
		// A trigger body's "UPDATE ... FROM" with a leading WITH declines, as
		// compileUpdateStmt's and compileInsertStmt's guards do: the live
		// re-lowering pushes no enclosing CTE scope, and a CTE shadows a
		// same-named table (select.c:6028/6036), so it could bind a different
		// source. Unreachable today -- the trigger body parser rejects a leading
		// WITH -- but it fails closed if that changes.
		return nil, nil, fmt.Errorf("%w: UPDATE ... FROM in a trigger body with a WITH clause", errVDBEUnsupported)
	}
	if tbl.withoutRowid && tbl.pkIndex == nil {
		// identifyTargetRow (update_from.go) can only recover a WITHOUT ROWID
		// row's opaque row-store key THROUGH its PRIMARY KEY. Unreachable in
		// practice -- such a table always resolves one -- and kept because the
		// alternative is calling identifyTargetRow with nothing to look up.
		return nil, nil, fmt.Errorf("%w: UPDATE ... FROM against a WITHOUT ROWID table with no resolved PRIMARY KEY", errVDBEUnsupported)
	}
	sel, nkey, serr := buildUpdateFromSelect(stmt, tbl)
	if serr != nil {
		// writeJoinCTEs' own errors are prepare-time diagnostics
		// (update_from.go); wrapping keeps their text verbatim.
		return nil, nil, declineOrSemantic(serr)
	}
	// The pre-statement image pass one reads. SQLite's pass one runs over the
	// untouched table by construction (it completes before pass two's first
	// store), and this engine's equivalent is the snapshot every other write
	// subquery already reads (writeSubqueryPager).
	pager, perr := db.SnapshotPager()
	if perr != nil {
		return nil, nil, perr
	}
	// A trigger body's pass one binds at fire time: compileLiveSubProgram
	// lowers it again inside runSubOnce against a fresh image (liveLower),
	// once per firing -- vdbe.c:7581-7582's per-VdbeFrame OP_Once reset.
	// progPager stays nil so nothing frozen reaches the body Program
	// (programReadsFrozenSnapshot). A body whose join names NEW./OLD.
	// ("UPDATE t1 SET c = v FROM map WHERE k=new.a", triggerupfrom.test 1.0)
	// lowers with no enclosing compiler or row, so the reference fails to
	// resolve and the statement declines (see vdbe_live_read.go).
	//
	// pager stays the compile-time pager: derivedColumnInfos asks it for
	// per-column affinity and collation, which are schema facts, and liveLower
	// replaces both program and pager at run time.
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
	if len(prog.ColNames) != nkey+len(stmt.sets) {
		// Unreachable: buildUpdateFromSelect emits exactly that many result
		// columns. Asserted rather than assumed, because the whole pass-two
		// loop indexes the eph row by SET position.
		return nil, nil, fmt.Errorf("%w: UPDATE ... FROM pass one produced %d columns, want %d", errVDBEUnsupported, len(prog.ColNames), nkey+len(stmt.sets))
	}
	if prog.Correlated {
		// compileSubProgram was given no enclosing compiler, so nothing outer
		// can have been bound -- but a correlated derived source is
		// materialized by execWithParent rather than runSubOnce (vdbe.go's
		// OpOpenDerived arm), which is not where collapseUpfromRows sits.
		// Declined rather than silently opened as an ordinary derived cursor.
		return nil, nil, fmt.Errorf("%w: correlated UPDATE ... FROM pass one", errVDBEUnsupported)
	}
	// The eph table's columns are the SET values alone: select.c:1368-1369
	// drops the key column(s) from the record ("regResult+(i2<0)",
	// "nResultCol-(i2<0)"), which is what makes update.c:952's "OP_Column iEph,
	// nOff+j" with nOff==0 read SET expression j.
	srcCols := pager.derivedColumnInfos(sel, prog.ColNames)[nkey:]
	src := &derivedSource{
		prog:   prog,
		tbl:    &resolvedTable{cols: srcCols, ipkIndex: -1},
		pager:  pager,
		upfrom: &upfromDest{tbl: tbl, displayName: stmt.table, nkey: nkey},
	}
	return src, progPager, nil
}
