// The live read seam: a sub-SELECT inside a write program that must read the
// database as it stands when it runs, not the image the statement was
// compiled against.
//
// In C a trigger body is a SubProgram invoked with OP_Program
// (trigger.c:1231, 1414-1415) and reads the same live cursors, so the only
// question is OP_Once. An uncorrelated subquery is bracketed in OP_Once
// ("if( !ExprHasProperty(pExpr, EP_VarSelect) ){ addrOnce =
// sqlite3VdbeAddOp0(v, OP_Once);", expr.c:3889-3890; IN at 3641/3674; "We are
// inside a trigger" among the reasons to repeat, expr.c:3879-3884), and
// OP_Once's bits live in the per-invocation VdbeFrame that OP_Program zeroes
// each firing (vdbe.c:7581-7582, read at vdbe.c:2711-2715). So: once per firing,
// against the database as that firing sees it
// (compat-harness/live_read_k_test.go):
//
//	CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM t)>1
//	  BEGIN INSERT INTO log VALUES(new.a); END;
//	INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r');       -- log holds 2,3
//
//	CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN
//	  INSERT INTO t2 SELECT count(*) FROM t1; END;
//	INSERT INTO t1 VALUES(1); INSERT INTO t1 VALUES(2);  -- t2 holds 1 then 2
//
// Here a compiled read binds its cursors to an immutable image (OpOpenRead's
// *resolvedTable root resolved against one *ReadOnlyPager), and a write program
// has one snapshot per statement (writeSubqueryPager). The equivalent of
// reading live is to lower the SELECT again against a fresh image when the
// sub-program runs, pager and root pages together (swapping only the pager
// could make a root name another table). That is still a compile, opcodes only
// (RULE #1); C itself re-prepares when the schema moves (sqlite3Reprepare).
//
// compileLiveSubProgram lowers with no enclosing row and only trigOnlyOuter as
// its enclosing compiler: a trigger context, a RETURNING affected-row context,
// and a schema-only scope for that row; no pager, cursor scopes, CTEs or
// readable registers. Anything else (a correlated column, an enclosing WITH's
// CTE) fails to resolve and declines, because:
//
//   - the run-time lowering has no enclosing compiler to correlate against;
//   - a NEW./OLD. folded as a literal through compiler.rowOuter loses its
//     affinity and declared collation
//     (TestTriggerWhenSubqueryDeclaredCollationIsDeclined). The admitted
//     pseudo-rows are OpParam reads published to affCtx as schema scopes, so
//     both survive (trigPseudoRowScopes; trigOnlyOuter's scope).
//
// Both lowerings get the identical context, so a shape that lowers now lowers
// the same way at fire time. That holds only for references the lowering
// resolves: a table-valued function's argument is deferred to run time
// (buildVtabConstraints) and once broke this. It is handled by carrying the
// trigger context with the source (derivedSource.vtabTrig, stamped from c.trig
// at the OpOpenDerived and used by foldVtabInputValue), so "json_each(NEW.x)"
// compiles its argument as the body did and reads the same row (runSubOnce's
// execTrig seeds trigOld/trigNew). C codes such an argument as an ordinary
// "hidden-column = <arg>" term (sqlite3WhereTabFuncArgs, whereexpr.c:1902;
// expr.c:4950) inside the trigger sub-program (resolve.c:525-543); attach.test
// 5.10.
package engine

import (
	"fmt"
)

// compileLiveSubProgram lowers sel twice: now, against a snapshot of the
// current database, as the decline gate and the source of compile-time facts
// (column count, output affinities/collations, names) and the program RULE #1
// checks inspect; and again at run time from Program.LiveSource (liveLower).
// There is no outer row (see the file doc). nQueryLoop/nqlKnown are the
// enclosing pParse->nQueryLoop where sel is coded, seeding the planner.
func (db *DB) compileLiveSubProgram(sel *SelectStmt, trig *trigCompileCtx, ret *retRowCompileCtx, nQueryLoop logEst, nqlKnown bool) (*Program, error) {
	pager, err := db.SnapshotPager()
	if err != nil {
		return nil, declineOrSemantic(err)
	}
	outer := trigOnlyOuter(trig, ret)
	if outer != nil {
		outer.nQueryLoop, outer.nQueryLoopKnown = nQueryLoop, nqlKnown
	}
	prog, err := compileSubProgram(pager, sel, outer)
	if err != nil {
		return nil, err
	}
	if prog.Correlated {
		// Unreachable by construction (nothing outer was offered, so nothing
		// outer can have been bound), but a correlated program is run by
		// execWithParent against the enclosing FRAME rather than through
		// runSubOnce, which is the one path liveLower does not sit on. Refused
		// rather than silently executed against the frozen twin.
		return nil, fmt.Errorf("%w: correlated live sub-program", errVDBEUnsupported)
	}
	prog.LiveSource = sel
	prog.LiveTrig = trig
	prog.LiveRet = ret
	prog.LiveNQueryLoop, prog.LiveNQLKnown = nQueryLoop, nqlKnown
	return prog, nil
}

// trigOnlyOuter is the one enclosing compiler a live sub-program gets: a
// trigger context and a schema-only scope for the RETURNING affected row,
// nothing else (no pager, cursor scopes, rowOuter, CTEs or readable registers).
// So outside names still decline, while NEW./OLD. resolve through
// resolveTriggerParam to an OpParam read of the running machine's trigger row,
// reproducible at fire time and keeping affinity and collation (affCtx exposes
// NEW/OLD as scopes under a trigger context; collate6.test).
//
// ret is the RETURNING affected row, answered by resolve.c's NC_UBaseReg
// pseudo-row (resolve.c:528-536) and read via OpParam from the row OpPseudoRow
// snapshotted. Without it "... RETURNING a,(SELECT count(*) FROM s WHERE x=a)"
// was "no such column: a" where C answers per row (EP_VarSelect,
// resolve.c:1403-1404).
//
// nil trig and nil ret give a nil outer.
func trigOnlyOuter(trig *trigCompileCtx, ret *retRowCompileCtx) *compiler {
	if trig == nil && ret == nil {
		return nil
	}
	c := &compiler{trig: trig, retRow: ret}
	if ret != nil {
		// The affected row is also a schema scope, so a reference contributes
		// the column's declared affinity and collation, as C's RETURNING arm
		// sets "pExpr->y.pTab = pTab" and "pExpr->op2 = TK_COLUMN"
		// (resolve.c:590, 593), which sqlite3ExprCollSeq (expr.c:254-262) and
		// sqlite3ExprAffinity (expr.c:45) follow. The excluded. arm sets neither
		// (resolve.c:581-585), which is why exclCompileCtx's scope is the
		// opposite (excludedPseudoRowCols). Over t(a INTEGER, v TEXT COLLATE
		// NOCASE, n INTEGER) with s(x) holding one row:
		//
		//	INSERT INTO t VALUES(1,'ABC',5)
		//	  RETURNING (SELECT CASE WHEN v='abc' THEN 'hit' ELSE 'miss' END FROM s)
		//	    hit (NOCASE)
		//	  RETURNING (SELECT CASE WHEN n='5' THEN 'hit' ELSE 'miss' END FROM s)
		//	    hit (INTEGER affinity)
		//
		// regs is nil and the scope schema-only: this stub is only ever an
		// outer, and reading an enclosing register block requires rowLive
		// (compileColumn, bindOuterAggRef), which it does not set. The value
		// travels via emitReturningParam's OpParam read.
		rs := tableScope{name: ret.tableName, tableName: ret.tableName, cols: ret.cols, colIndex: buildColIndex(ret.cols), noRowid: ret.noRowid}
		c.regScopes = []regScope{{scope: &rs}}
	}
	return c
}

// programNeedsRowContext reports whether prog, or a sub-program it reaches,
// holds a table-valued function source, whose arguments are folded at run time
// against the machine's row context (buildVtabConstraints). It no longer refuses
// anything for the live seam (derivedSource.vtabTrig carries the trigger
// context; live_read_codegen_test.go, trigger_tvf_live_row_test.go). What it
// still answers is whether the result can differ row to row, which
// Program.Correlated does not capture (nothing outer was bound);
// insnsHoldRowDependentSubquery (vdbe_write.go) uses it. It is true even for a
// constant argument (resolveFrom defers all of them), which is the safe
// direction there.
func programNeedsRowContext(prog *Program) bool {
	return programNeedsRowContextSeen(prog, map[*Program]bool{})
}

func programNeedsRowContextSeen(prog *Program, seen map[*Program]bool) bool {
	if prog == nil || seen[prog] {
		return false
	}
	seen[prog] = true
	for i := range prog.Insns {
		in := &prog.Insns[i]
		switch p4 := in.P4.(type) {
		case *derivedSource:
			if p4.vtab != nil && p4.vtab.TableFunc {
				return true
			}
			if programNeedsRowContextSeen(p4.prog, seen) {
				return true
			}
		case *Program:
			if programNeedsRowContextSeen(p4, seen) {
				return true
			}
		case *inSubPlan:
			if programNeedsRowContextSeen(p4.prog, seen) {
				return true
			}
		case *rowSubPlan:
			if programNeedsRowContextSeen(p4.prog, seen) {
				return true
			}
		}
	}
	return false
}

// liveLower returns the (program, pager) a sub-program must run as: the given
// pair for an ordinary program, or for a live one a fresh image of the
// database and prog.LiveSource lowered against it. It sits in runSubOnce, the
// one path for uncorrelated sub-programs (OpSubquery, OpExists, OpInSub,
// OpRowSub, OpOpenDerived), and so inherits its per-execution cache: OP_Once
// under a per-firing frame, paid once per firing.
func (m *vdbe) liveLower(prog *Program, pager *ReadOnlyPager) (*Program, *ReadOnlyPager, error) {
	if prog == nil || prog.LiveSource == nil {
		return prog, pager, nil
	}
	if m.wctx == nil || m.wctx.db == nil {
		// A live sub-program is only ever emitted into a WRITE program (the
		// only compile that has no pager of its own), so this cannot happen --
		// reported rather than dereferenced, per AGENTS.md invariant 2.
		return nil, nil, fmt.Errorf("vdbe: live sub-program with no write session to read")
	}
	var live *ReadOnlyPager
	if prog.LiveFirstImage {
		if live = m.firstImages[prog]; live == nil {
			var err error
			if live, err = m.wctx.db.SnapshotPager(); err != nil {
				return nil, nil, err
			}
			if m.firstImages == nil {
				m.firstImages = map[*Program]*ReadOnlyPager{}
			}
			m.firstImages[prog] = live
		}
	} else if prog.LiveStmtImage {
		if live = m.stmtImageFor(); live == nil {
			// runTriggerSub takes the image for every body program holding such
			// a sub-program (NeedsStmtImage), so this cannot happen -- reported
			// rather than answered from the live database, which is the wrong
			// image.
			return nil, nil, fmt.Errorf("vdbe: statement-start sub-program with no statement image")
		}
	} else {
		var err error
		if live, err = m.wctx.db.SnapshotPager(); err != nil {
			return nil, nil, err
		}
	}
	if prog.LiveRow != nil && len(prog.LiveRow.ctes) > 0 {
		// The UPDATE's own WITH clause, pushed exactly as compileUpdateStmt
		// pushed it for the first lowering.
		defer live.pushCTEScope(prog.LiveRow.ctes)()
	}
	sub, err := compileSubProgram(live, prog.LiveSource, liveOuter(prog))
	if err != nil {
		return nil, nil, err
	}
	// The two lowerings must agree on ARITY. Everything downstream of the
	// re-lowering was sized by the FIRST one -- a derived source's column list
	// (derivedColumnInfos, whose *resolvedTable openDerivedCursor indexes rows
	// by), a row-value probe's width, an IN test's column count -- so a second
	// lowering that produced a different shape would index off the end of a
	// row. It cannot happen (same AST, same schema, nil outer both times), but
	// "cannot happen" plus an index is how invariant 3 gets broken, so it is
	// reported rather than assumed.
	if len(sub.ColNames) != len(prog.ColNames) {
		return nil, nil, fmt.Errorf("vdbe: live sub-program re-lowered to %d columns, was %d", len(sub.ColNames), len(prog.ColNames))
	}
	return sub, live, nil
}

// liveSubSelect is the compiler's side of the seam: it lowers sel LIVE when
// this compile is one whose reads must bind at run time (compiler.liveDB --
// today, a trigger's WHEN guard and a trigger body statement), and by the
// ordinary frozen route otherwise. Every subquery emitter goes through it so
// there is exactly one place the two routes are chosen between.
func (c *compiler) liveSubSelect(sel *SelectStmt) (*Program, error) {
	if c.liveDB != nil && c.inWhere && c.liveRow == nil {
		return c.liveWhereSubSelect(sel)
	}
	if c.liveDB != nil && c.rowLive && c.liveRow == nil {
		if prog, err := c.liveBodyRowSubSelect(sel); prog != nil || err != nil {
			return prog, err
		}
	}
	if c.liveDB != nil {
		return c.liveDB.compileLiveSubProgram(sel, c.trig, c.retRow, c.nQueryLoop, c.nQueryLoopKnown)
	}
	if c.liveRow != nil {
		prog, err := compileSubProgram(c.pager, sel, c.liveRow.outer())
		if err != nil {
			return nil, err
		}
		prog.LiveSource = sel
		prog.LiveRow = c.liveRow
		prog.LiveFirstImage = subqueryPlansAutoIndex(c.pager, sel, c.liveRow.outer())
		return prog, nil
	}
	return compileSubProgram(c.pager, sel, c)
}

// liveBodyRowSubSelect lowers a subquery in a trigger body's UPDATE or DELETE,
// inside its row loop, against that row as well as the trigger pseudo-rows, or
// returns (nil, nil) when it is not correlated to the row (the trigger-only
// route handles that). Without it
//
//	CREATE TRIGGER t0 AFTER INSERT ON e BEGIN
//	  UPDATE c SET n = (SELECT count(*) FROM b WHERE b.id >= c.id); END;
//
// was "no such column: c.id"; C reads the body statement's cursor
// (expr.c:5104-5106; vdbe_write_rowlive.go).
//
// Re-lowered per row, it sees the database as each row does, C's answer for a
// SET (update.c:954). A WHERE runs in pass one over the image before the body
// statement writes (delete.c:522, update.c:849); the two agree unless the
// subquery reads a table the loop writes (tablesALoopCanWrite), which declines.
func (c *compiler) liveBodyRowSubSelect(sel *SelectStmt) (*Program, error) {
	lr := c.liveRowStub()
	if lr == nil {
		return nil, nil
	}
	pager, err := c.liveDB.SnapshotPager()
	if err != nil {
		return nil, declineOrSemantic(err)
	}
	prog, err := compileSubProgram(pager, sel, lr.outer())
	if err != nil || !prog.Correlated {
		return nil, nil // the trigger-only route answers, or reports, it
	}
	// Over a table the loop writes, a per-row lowering sees the rows before it,
	// which is C's answer only when the loop visits rows in C's order -- the
	// condition compileUpdateStmt sets bodySetLive for (and the view path, whose
	// loop walks the materialization as C's does). A SET outside it declines;
	// the WHERE never reaches here (liveWhereSubSelect).
	if !c.bodySetLive {
		for _, name := range c.liveDB.tablesALoopCanWrite(lr.tbl) {
			if programReadsTable(prog, name) {
				return nil, fmt.Errorf("%w: a trigger body's correlated SET subquery over %s, which the same statement writes, in a one-pass loop C walks in index order", errVDBEUnsupported, name)
			}
		}
	}
	prog.LiveSource = sel
	prog.LiveRow = lr
	prog.LiveFirstImage = subqueryPlansAutoIndex(pager, sel, lr.outer())
	return prog, nil
}

// liveRowStub is the row an UPDATE or DELETE compile stands on, as a
// liveRowCtx: its target table when newWriteScanCompilerAs built it, else the
// one scope a view scan compiler has (newViewScanCompiler), as a table of the
// view's own columns. nil for any other compile.
func (c *compiler) liveRowStub() *liveRowCtx {
	if c.writeTbl != nil {
		return &liveRowCtx{tbl: c.writeTbl, alias: c.writeAlias, ownSchema: c.ownSchema, trig: c.trig,
			nQueryLoop: c.nQueryLoop, nQueryLoopKnown: c.nQueryLoopKnown}
	}
	if len(c.scopes) == 1 {
		s := c.scopes[0]
		return &liveRowCtx{tbl: &tableMeta{name: s.name, cols: s.cols, withoutRowid: s.noRowid}, ownSchema: c.ownSchema, trig: c.trig,
			nQueryLoop: c.nQueryLoop, nQueryLoopKnown: c.nQueryLoopKnown}
	}
	return nil
}

// liveWhereSubSelect lowers a subquery in a trigger body's UPDATE or DELETE
// WHERE against the database as it stood when that statement began. C
// evaluates such a WHERE in pass one (update.c:738 withholds
// WHERE_ONEPASS_MULTIROW for EP_Subquery; delete.c:497 via NC_Subquery). A
// top-level statement gets that image from writeSubqueryPager; a body statement
// runs per firing, so runTriggerSub takes it at statement start
// (Program.NeedsStmtImage) and liveLower lowers against it, per row if
// correlated. Reading live instead,
//
//	CREATE TRIGGER t0 AFTER INSERT ON e BEGIN
//	  DELETE FROM c WHERE EXISTS(SELECT 1 FROM c AS c2 WHERE c2.id = c.id - 1); END;
//
// deleted row 2 and then kept row 3 (the Halloween problem,
// segmentSnapshotPager).
func (c *compiler) liveWhereSubSelect(sel *SelectStmt) (*Program, error) {
	pager, err := c.liveDB.SnapshotPager()
	if err != nil {
		return nil, declineOrSemantic(err)
	}
	lr := c.liveRowStub()
	outer := trigOnlyOuter(c.trig, c.retRow)
	if lr != nil {
		outer = lr.outer()
	}
	prog, err := compileSubProgram(pager, sel, outer)
	if err != nil {
		return nil, err
	}
	prog.LiveSource = sel
	if lr != nil {
		prog.LiveRow = lr
	} else {
		prog.LiveTrig, prog.LiveRet = c.trig, c.retRow
	}
	prog.LiveStmtImage = true
	return prog, nil
}

// stmtImageFor is the statement-start image of the trigger body statement m
// runs inside, found on m or the nearest enclosing frame that took one.
func (m *vdbe) stmtImageFor() *ReadOnlyPager {
	for p := m; p != nil; p = p.parent {
		if p.stmtImage != nil {
			return p.stmtImage
		}
	}
	return nil
}

// programNeedsStmtImage reports whether prog holds a sub-program lowered
// against the statement-start image (Program.LiveStmtImage).
func programNeedsStmtImage(prog *Program) bool {
	for i := range prog.Insns {
		var p *Program
		switch p4 := prog.Insns[i].P4.(type) {
		case *Program:
			p = p4
		case *inSubPlan:
			p = p4.prog
		case *rowSubPlan:
			p = p4.prog
		case *derivedSource:
			p = p4.prog
		}
		if p != nil && p.LiveStmtImage {
			return true
		}
	}
	return false
}

// liveOuter is the enclosing compiler a live sub-program's run-time lowering
// resolves against: the same one its compile-time lowering was handed, rebuilt
// from what the Program kept.
func liveOuter(prog *Program) *compiler {
	if prog.LiveRow != nil {
		return prog.LiveRow.outer()
	}
	outer := trigOnlyOuter(prog.LiveTrig, prog.LiveRet)
	if outer != nil {
		outer.nQueryLoop, outer.nQueryLoopKnown = prog.LiveNQueryLoop, prog.LiveNQLKnown
	}
	return outer
}

// liveRowCtx is the third outside name a live sub-program may use: the row an
// UPDATE is rewriting, for a SET subquery that reads the updated table and is
// correlated to that row. C codes each SET in the pass-two loop (update.c:954),
// and a correlated subquery has EP_VarSelect (resolve.c:1403-1404) so no
// OP_Once (expr.c:3889): it re-runs per row over the live b-tree, seeing
// earlier rows' writes. So the body is lowered again per call (execWithParent)
// against a fresh image.
//
// The write loop's cursor 0 sits on the row for the whole SET span
// (vdbe_write_rowlive.go), so the stub (the target's scope on cursor 0,
// row-live) binds a reference to an OpOuterColumn one frame up, identically in
// both lowerings. compileUpdateStmt hands it out only where nothing but a WITH
// (pushed by both lowerings) could otherwise resolve: no trigger context.
//
// ponytail: an image per row, so a self-correlated UPDATE of n rows builds n
// images. Cache the image on a row-store write generation if that ever shows up.
type liveRowCtx struct {
	tbl       *tableMeta
	alias     string
	ownSchema string
	ctes      []CTEDef // the UPDATE's leading WITH, in scope for both lowerings
	// trig is the trigger a BODY statement's row belongs to, so NEW/OLD still
	// resolve beside the row (liveBodyRowSubSelect); nil at the top level.
	trig *trigCompileCtx
	// regScopes replaces tbl for an upsert's DO UPDATE: the row there is two
	// REGISTER blocks (the existing row and "excluded"), not a cursor, and a
	// reference binds to them as OpOuterAggReg (emitUpsertArm).
	regScopes []regScope
	// nQueryLoop is pParse->nQueryLoop where the row's SET values are coded,
	// when nQueryLoopKnown -- so a subquery of the SET is planned as C plans
	// it, as run that many times.
	nQueryLoop      logEst
	nQueryLoopKnown bool
}

func (r *liveRowCtx) outer() *compiler {
	if r.regScopes != nil {
		return &compiler{regScopes: append([]regScope(nil), r.regScopes...), rowLive: true, ownSchema: r.ownSchema, trig: r.trig,
			nQueryLoop: r.nQueryLoop, nQueryLoopKnown: r.nQueryLoopKnown}
	}
	c := newWriteScanCompilerAs(r.tbl, r.alias)
	c.ownSchema = r.ownSchema
	c.trig = r.trig
	c.rowLive = true
	c.nQueryLoop, c.nQueryLoopKnown = r.nQueryLoop, r.nQueryLoopKnown
	return c
}
