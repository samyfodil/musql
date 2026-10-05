// The compiler half of an aggregate query's result phase: the select-list,
// HAVING and ORDER BY expressions evaluated once per group after every
// accumulator is finalized. An itemPlan's rewritten tree (sql_group.go) becomes a
// Program whose three placeholder nodes read registers, as C codes them with no
// instructions at all:
//
//	expr.c:4977/:4996  TK_AGG_COLUMN   -> "return AggInfoColumnReg(pAggInfo,
//	                                      pExpr->iAgg);"
//	expr.c:5333/:5342  TK_AGG_FUNCTION -> "return AggInfoFuncReg(pInfo,
//	                                      pExpr->iAgg);"
//	sqliteInt.h:2951-2957  both macros: pAggInfo->iFirstReg + a FIXED index.
//
// The blocks are filled as C fills them: finalized aggregates (OP_AggFinal into
// AggInfoFuncReg, select.c:6733, 6786; aggResult's aggVals), the bare-column
// anchor row (updateAccumulator's tail, select.c:6958-6961, under the regHit
// gate at 6955-6957 that is the magnet register; aggAccumulators.magnetWalk),
// and the GROUP BY key from the sorter record (select.c:8657).
//
// The item is then ordinary codegen: HAVING via sqlite3ExprIfFalse and the select
// list via selectInnerLoop (select.c:8746-8748 sorted drain; 8905-8911 whole
// table). A subquery body's references to the group become the same placeholders
// read across the frame (rewriteAggItemBodies, vdbe_agg_item_subst.go), and a
// bare name inside one resolves against this query's FROM items published as
// register scopes over the anchor row (aggAnchorRegScopesUsable), lookupName's
// inner-first walk (resolve.c:703-704).
package engine

import "fmt"

// aggResultRegs is the register block an aggregate result expression's
// placeholders read: one register per finalized accumulator (groupAggExpr.accIdx),
// per GROUP BY key column (groupKeyExpr.idx), and per anchor-row column / table
// rowid (groupBareColExpr). C packs these as base+index (AggInfoColumnReg /
// AggInfoFuncReg); keeping the three widths separate makes an out-of-range index
// a decline rather than a read of a neighbouring block. nil on every compiler
// except compileAggItemProgram's, so a placeholder anywhere else is an error
// (as compiler.winRegs is for windowResultExpr).
type aggResultRegs struct {
	// owner is the aggregate query this block belongs to -- C's AggInfo
	// identity (see the owner note above groupKeyExpr, sql_group.go). A
	// placeholder naming a DIFFERENT owner walks straight past this block.
	owner *aggPlan

	agg   []int
	key   []int
	col   []int
	rowid []int
}

// The four register blocks aggResultReg can be asked for.
const (
	aggRegAgg   = iota // a finalized accumulator (groupAggExpr)
	aggRegKey          // a GROUP BY key column (groupKeyExpr)
	aggRegCol          // an anchor-row column (groupBareColExpr)
	aggRegRowid        // an anchor-row per-table rowid (groupBareColExpr.isRowid)
)

// block returns the register slice one of the four placeholder kinds resolves
// against.
func (r *aggResultRegs) block(which int) []int {
	switch which {
	case aggRegAgg:
		return r.agg
	case aggRegKey:
		return r.key
	case aggRegCol:
		return r.col
	case aggRegRowid:
		return r.rowid
	}
	return nil
}

// aggResultReg resolves one placeholder to its register and emits the read, or
// declines (no block in the chain, or an index outside the owning block).
//
// The chain walk serves a placeholder inside a subquery body
// (rewriteAggItemBodies): in C one flat program reads AggInfo registers directly
// (expr.c:4996); here the body is a separate Program, so the read crosses frames
// with OpOuterAggReg (OpOuterColumn's P5 convention, on a register).
//
// Every compiler up to the owner is marked correlated, so compileScalarSubquery /
// compileExistsSubquery / compileInSubquery give the body no run-once cache: it
// must re-run per group, and runSubOnce has no parent frame for OpOuterAggReg
// (removing the mark panics).
//
// The walk stops at the block the placeholder owns, not the first one met: a body
// that is itself an aggregate query has its own block indexing its own
// accumulators. owner is C's pExpr->pAggInfo (expr.c:4996, 5342; chosen by
// resolve.c:1355-1361). A nil owner (the item's own level) takes the first block.
//
// The aggregate query's own scan compiler is skipped by the chain
// (compileAggItemProgram hangs off enclosing.outer) but must be marked too, since
// its correlated flag becomes the body sub-Program's Correlated; aggSkipped is
// the back-pointer markAggCorrelated uses.
func (c *compiler) aggResultReg(which, idx int, owner *aggPlan) (int, error) {
	level := 0
	for oc := c; oc != nil; oc = oc.outer {
		if oc.aggRegs == nil || (owner != nil && oc.aggRegs.owner != owner) {
			level++
			continue
		}
		regs := oc.aggRegs.block(which)
		if idx < 0 || idx >= len(regs) {
			break
		}
		if level == 0 {
			// C's own answer, and the whole point of the model: no instruction
			// at all, just the register (expr.c:4996, expr.c:5342).
			return regs[idx], nil
		}
		markAggCorrelated(c, oc)
		d := c.alloc()
		c.emit(Instruction{Op: OpOuterAggReg, P1: regs[idx], P3: d, P5: uint16(level)})
		return d, nil
	}
	return 0, fmt.Errorf("%w: aggregate-result placeholder outside an aggregate result compile", errVDBEUnsupported)
}

// markAggCorrelated marks every compile from c up to (not including) oc as
// correlated, plus each aggregate SCAN compiler those item compilers were hung
// past (aggSkipped). Marking is not optional -- see aggResultReg's own doc, and
// the mutation test recorded there.
func markAggCorrelated(c, oc *compiler) {
	for p := c; p != nil && p != oc; p = p.outer {
		p.correlated = true
		if p.aggSkipped != nil {
			p.aggSkipped.correlated = true
		}
	}
}

// aggResultRegOrBuffer is aggResultReg with the window projection's buffering
// behind it (ph is the placeholder, since the buffer holds expressions). A window
// projection is compiled with no chain (compileWindowRowProgram), so the walk
// finds no block; such a placeholder ("SELECT (0,0) IN (SELECT MIN(c0), NTILE(1)
// OVER())", window1.test#25) is lifted into the buffered row and coded in the
// enclosing context, as C lifts outside references (selectWindowRewriteExprCb,
// window.c:756-771, 802-819). Tried only after aggResultReg declines.
func (c *compiler) aggResultRegOrBuffer(which, idx int, ph Expr, owner *aggPlan) (int, error) {
	reg, err := c.aggResultReg(which, idx, owner)
	if err == nil {
		return reg, nil
	}
	// This compile's OWN buffer first, then an enclosing one -- compileColumn's
	// order for the identical question (vdbe_codegen.go), and for its reason: a
	// placeholder written inside a SUBQUERY of the projection has no buffer of
	// its own, and the register it gets lives in the projection's block, read
	// across exactly the frames a correlated column read is read across.
	if r, ok := c.bufOut.slotPlaceholder(c, ph); ok {
		return r, nil
	}
	level := 1
	for oc := c.outer; oc != nil; oc = oc.outer {
		if r, ok := oc.bufOut.slotPlaceholder(oc, ph); ok {
			// Without the marking the enclosing sub-Program is run through
			// runSubOnce, whose exec has NO parent frame, and the
			// OpOuterAggReg below walks off a nil one. Same rule, same reason,
			// as aggResultReg's own marking above.
			markAggCorrelated(c, oc)
			d := c.alloc()
			c.emit(Instruction{Op: OpOuterAggReg, P1: r, P3: d, P5: uint16(level)})
			return d, nil
		}
		level++
	}
	return 0, err
}

// aggItemProgram is one itemPlan's rewritten tree compiled against that block.
// It is attached to the itemPlan at COMPILE time and is immutable thereafter
// (an aggPlan rides in an OpAggResult's P4 and so lives inside a cached
// Program, which several executions may share); the register file it runs on is
// per-execution, supplied by the running vdbe.
type aggItemProgram struct {
	// prog is nil when the tree did not lower. compileAggItemProgram returns
	// nil to say "I could not"; its caller decides what that means, and
	// compileAggItemPrograms makes it a hard errVDBEUnsupported for the whole
	// statement (AGENTS.md Rule 1).
	prog *Program

	// The four blocks, in register order, and their widths. The widths are what
	// run seeds and what runnable checks: the program addresses each block BY
	// POSITION, so a short input would answer from a cleared register -- a
	// wrong value rather than a panic, which is why it is checked and not left
	// to copy's own clamping (selfRowExpr.runnable's rule, vdbe_run.go).
	aggBase, nAgg     int
	keyBase, nKey     int
	colBase, nCols    int
	rowidBase, nRowid int

	resultReg int
}

// aggItemLowerable reports whether e, an itemPlan's rewritten tree, may be
// compiled by compileAggItemProgram, and the group-key block width it needs.
//
// An allow-list, since that compile has no cursor scopes. Notes on what is
// allowed and refused:
//
//   - a ColumnExpr still standing is a correlated reference to an enclosing
//     query (rewriteGroupExpr replaced the rest); compileAggItemProgram hands
//     the chain bindAggOuterRefs proved, so compileColumn emits OpOuterColumn.
//     TRUE/FALSE's FallbackLiteral is consulted last, as for any row read.
//   - a subquery body is allowed: rewriteAggItemBodies replaced its references
//     to this query with placeholders, C's model (walker.c:82,
//     expr.c:7458-7469, 4996). What that pass could not settle, or a reference
//     further out, declines in the body's own compile.
//   - an fts5 auxiliary function is refused: it needs a live MATCH cursor.
//   - a nested aggregate or window call cannot appear (every aggregate became a
//     groupAggExpr) and is refused rather than compiled with its OVER dropped.
//
// Anything unlisted is refused.
func aggItemLowerable(e Expr) (nKey int, ok bool) {
	both := func(a, b Expr) (int, bool) {
		na, aok := aggItemLowerable(a)
		if !aok {
			return 0, false
		}
		nb, bok := aggItemLowerable(b)
		if !bok {
			return 0, false
		}
		return max(na, nb), true
	}
	list := func(seed int, es ...Expr) (int, bool) {
		n := seed
		for _, x := range es {
			nx, xok := aggItemLowerable(x)
			if !xok {
				return 0, false
			}
			n = max(n, nx)
		}
		return n, true
	}
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, groupAggExpr, ColumnExpr:
		return 0, true
	case groupKeyExpr:
		if x.idx < 0 {
			return 0, false
		}
		return x.idx + 1, true
	case groupBareColExpr:
		if x.idx < 0 || x.rowidTableIdx < 0 {
			return 0, false
		}
		return 0, true
	case UnaryExpr:
		return aggItemLowerable(x.X)
	case IsNullExpr:
		return aggItemLowerable(x.X)
	case CollateExpr:
		return aggItemLowerable(x.X)
	case CastExpr:
		return aggItemLowerable(x.X)
	case BinaryExpr:
		return both(x.L, x.R)
	case BetweenExpr:
		return list(0, x.X, x.Lo, x.Hi)
	case LikeExpr:
		return list(0, x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return both(x.X, x.Pattern)
	case RowExpr:
		return list(0, x.Elems...)
	case SubqueryExpr, ExistsExpr:
		// The body's own placeholders are reached by compileExpr, not by this
		// walk: they live in a SelectStmt, which this function does not enter
		// (and need not -- a body placeholder resolves against the block this
		// program declares, whose widths are fixed by the plan, never by the
		// key width this walk is measuring).
		return 0, true
	case InExpr:
		n, nok := aggItemLowerable(x.X)
		if !nok {
			return 0, false
		}
		return list(n, x.List...)
	case CaseExpr:
		n, nok := list(0, x.Base, x.Else)
		if !nok {
			return 0, false
		}
		for _, w := range x.Whens {
			nw, wok := both(w.When, w.Then)
			if !wok {
				return 0, false
			}
			n = max(n, nw)
		}
		return n, true
	case FuncExpr:
		if x.Over != nil || x.Star || isAggregateCall(x) || fts5AuxFuncName(x.Name) {
			return 0, false
		}
		return list(0, x.Args...)
	}
	return 0, false
}

// aggAnchorRegScopesUsable reports whether plan's FROM scopes may be published
// to the item compiler as register scopes over the anchor row. It is
// windowProjScopeLowerable's test clause for clause: resolveRowReg matches by
// name and has no USING/NATURAL skip (tableScope.coalesced) or RIGHT/FULL
// fallback (coalesceFallback), so on such a join both copies count and
// regScopeStrict reports "ambiguous" where C picks the left-most table
// (resolve.c:441-446). Not publishing keeps the item compiling as before with
// only qualified spellings substituted. The offset check guards the layout:
// scopes must tile [0, nCols) contiguously or a read lands in a neighbour.
//
// The two join clauses are blind (removing them changes no answer: the
// ambiguity error declines on its own) but are kept so the two seams stay
// identical.
func aggAnchorRegScopesUsable(plan *aggPlan) bool {
	total := 0
	for i := range plan.scopes {
		s := &plan.scopes[i]
		// Neither coalesced nor coalesceFallback refuses the whole plan: the
		// block represents coalesced exactly (only name resolution differs,
		// resolve.c:447-449), and a fallback is a property of a reference to
		// that column, which resolveRowReg declines on its own.
		if s.offset != total {
			return false
		}
		total += len(s.cols)
	}
	return total == plan.nCols
}

// compileAggItemProgram compiles it.rewritten against the register block, or
// returns nil. compileAggItemPrograms turns nil into errVDBEUnsupported for the
// statement (RULE #1). nil comes from (a) aggItemLowerable refusing, (b)
// compileExpr failing, or (c) a compile allocating VM state this program lacks:
// its machine has registers, a subquery cache, the pager and parameters only, so
// an opcode needing cursors, record registers, sorters or distinct sets would
// panic (compileSelfRowExpr's check).
//
// nSub is allowed: aggItemVM sizes a subCache from NSubCache, cleared per item per
// group (a body is not necessarily pure, so this is deliberately not OP_Once).
func compileAggItemProgram(pager *ReadOnlyPager, enclosing *compiler, plan *aggPlan, it *itemPlan) *aggItemProgram {
	if it == nil || it.rewritten == nil {
		return nil
	}
	// Every subquery body this item carries, rewritten so that its references
	// to the AGGREGATE query's own row are placeholders resolving against the
	// block below -- and every HOISTED aggregate call substituted by the
	// groupAggExpr standing for its already-finalized value. See
	// rewriteAggItemBodies (vdbe_agg_item_subst.go).
	//
	// Whether the group's ANCHOR ROW may be exposed to this compile as
	// register-backed row scopes, which is what lets a body's own compileColumn
	// resolve a BARE reference to the aggregate query inner-first instead of
	// this pass having to decide it by name alone (aggAnchorRegScopesUsable,
	// and aggItemSubst.bodyColumn's unqualified arm).
	anchor := aggAnchorRegScopesUsable(plan)
	tree, ok := rewriteAggItemBodies(plan, it, anchor)
	if !ok {
		return nil
	}
	nKey, ok := aggItemLowerable(tree)
	if !ok {
		return nil
	}
	p := &aggItemProgram{
		nAgg:   len(it.aggTemplates),
		nKey:   nKey,
		nCols:  plan.nCols,
		nRowid: len(plan.scopes),
	}
	// The enclosing query's outer chain, minus the aggregate query itself. A
	// ColumnExpr left in the item is a correlated reference further out,
	// already proven resolvable by bindAggOuterRefs; handing that chain over
	// lets compileColumn emit OpOuterColumn, and levels line up because both
	// chains start at enclosing.outer and aggItemVM points this frame at
	// m.parent.
	//
	// The aggregate query's own cursors are skipped: they are wherever the
	// scan left them, not on the anchor row. Its FROM items are offered
	// instead as register scopes over the anchor row (below), which is the row
	// a reference to this query must read.
	var (
		outer    *compiler
		rowOuter *evalCtx
	)
	if enclosing != nil {
		outer, rowOuter = enclosing.outer, enclosing.outerRowCtx()
	}
	// A trigger body's NEW/OLD resolve in an item exactly as anywhere else in
	// the body (pParse->pTriggerTab, resolve.c:525-543): an OpParam read of
	// the trigger row, which aggItemVM copies onto the item's machine.
	c := &compiler{pager: pager, outer: outer, rowOuter: rowOuter, trig: enclosingTriggerCtx(enclosing)}
	if enclosing != nil {
		// A subquery here is coded in the output phase, after sqlite3WhereEnd
		// put pParse->nQueryLoop back to its pre-loop value (select.c:8715
		// then :8729-8748 for GROUP BY, :8904 then :8910 for a whole-table
		// aggregate) -- which the caller hands over in enclosing's own fields.
		c.nQueryLoop, c.nQueryLoopKnown = enclosing.nQueryLoop, enclosing.nQueryLoopKnown
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)
	p.aggBase = c.allocN(p.nAgg)
	p.keyBase = c.allocN(p.nKey)
	p.colBase = c.allocN(p.nCols)
	p.rowidBase = c.allocN(p.nRowid)
	c.aggSkipped = enclosing
	c.aggRegs = &aggResultRegs{
		owner: plan,
		agg:   rowRegsFor(p.aggBase, p.nAgg),
		key:   rowRegsFor(p.keyBase, p.nKey),
		col:   rowRegsFor(p.colBase, p.nCols),
		rowid: rowRegsFor(p.rowidBase, p.nRowid),
	}
	if anchor {
		// The anchor-row block, also published as this query's FROM items in
		// register form (as compileWindowProjection does for a buffered row), so
		// a bare name in a subquery body resolves by the compiler: the body's
		// FROM first, then outward one level at a time (resolve.c:703-704),
		// emitting the same OpOuterAggReg as the qualified placeholder.
		//
		// rowLive promises these registers hold this group's anchor row while
		// anything beneath runs; regScopeStrict makes a name two of them offer
		// ambiguous, as SQLite reports, rather than walk outward.
		c.rowLive = true
		c.regScopeStrict = true
		for i := range plan.scopes {
			// A copy, not &plan.scopes[i]: regScope holds the pointer for the
			// whole compile and the plan outlives it.
			sc := plan.scopes[i]
			c.pushRegScope(regScope{
				scope:    &sc,
				regs:     rowRegsFor(p.colBase+sc.offset, len(sc.cols)),
				rowidReg: p.rowidBase + i,
			})
		}
	}
	reg, err := c.compileExpr(tree)
	if err != nil {
		return nil
	}
	if c.nCursor != 0 || c.nRec != 0 || c.nSorter != 0 || c.nDistinct != 0 {
		return nil
	}
	// No OpResultRow: this program yields ONE value and run reads it straight
	// out of its register, the same reason compileSelfRowExpr gives -- it runs
	// once per group per item.
	c.emit(Instruction{Op: OpHalt})
	p.resultReg = reg
	p.prog = &Program{Insns: c.insns, NReg: c.nReg, NSubCache: c.nSub}
	return p
}


// compileAggItemPrograms compiles every item of plan -- select list, HAVING and
// each non-ordinal ORDER BY term -- so OpAggResult can run one instead of
// walking its tree. Called once, where the aggPlan is assembled; an item that
// does not lower simply keeps a nil program.
func compileAggItemPrograms(pager *ReadOnlyPager, enclosing *compiler, plan *aggPlan) error {
	stamp := func(it *itemPlan) error {
		if it == nil {
			return nil
		}
		if it.prog = compileAggItemProgram(pager, enclosing, plan, it); it.prog == nil {
			return fmt.Errorf("%w: aggregate result expression", errVDBEUnsupported)
		}
		return nil
	}
	for _, it := range plan.outPlans {
		if err := stamp(it); err != nil {
			return err
		}
	}
	if err := stamp(plan.havingPlan); err != nil {
		return err
	}
	for _, op := range plan.orderPlans {
		if err := stamp(op.item); err != nil {
			return err
		}
	}
	return nil
}

// runnable reports whether p may be run for this group; false is an error in
// aggResult (there is no other route):
//
//   - a nil program cannot come from the compiler (compileAggItemPrograms
//     refuses such statements) but an aggPlan rides in a cached Program, so it
//     is checked;
//   - each input must be at least as wide as its block, since run copies by
//     position and a short one would answer from a cleared register. Only the
//     group key can be short (its width comes from OpAggResult's record
//     register).
func (p *aggItemProgram) runnable(m *vdbe, aggVals, groupKey, bareVals, bareRowids []Value) bool {
	return p != nil && p.prog != nil &&
		len(aggVals) >= p.nAgg && len(groupKey) >= p.nKey &&
		len(bareVals) >= p.nCols && len(bareRowids) >= p.nRowid
}

// run seeds the four register blocks for one group and runs the program.
func (p *aggItemProgram) run(m *vdbe, aggVals, groupKey, bareVals, bareRowids []Value) (Value, error) {
	pm := m.aggItemVM(p.prog)
	copy(pm.regs[p.aggBase:p.aggBase+p.nAgg], aggVals)
	copy(pm.regs[p.keyBase:p.keyBase+p.nKey], groupKey)
	copy(pm.regs[p.colBase:p.colBase+p.nCols], bareVals)
	copy(pm.regs[p.rowidBase:p.rowidBase+p.nRowid], bareRowids)
	if _, err := pm.run(p.prog.Insns); err != nil {
		return Value{}, err
	}
	return pm.regs[p.resultReg], nil
}

// aggItemVM returns the machine an aggregate item's program runs on, reused
// across items and groups (a fresh machine cost 331ns/1192B per evaluation). It
// cannot be re-entered: a body runs on its own vdbe (execWithParent), and
// OpAggResult appears only in the aggregate's scan body, never in an item
// program or a body beneath one. The frame chain carries a body's view of the
// group (OpOuterAggReg walks back to these registers).
//
// Registers are cleared on each acquisition so no previous item's value is
// readable (run seeds only each item's width).
//
// It carries the pager, bound parameters and enclosing evalCtx chain, and not
// m.wctx (write state an item must never see).
func (m *vdbe) aggItemVM(prog *Program) *vdbe {
	if m.aggVM == nil {
		m.aggVM = &vdbe{pager: m.pager, params: m.params}
	}
	vm := m.aggVM
	// The enclosing query's evalCtx, for the ONE thing an item program reads it
	// for: a connection-state function's source (connStateValue, conn_state.go),
	// which resolves through exactly this chain. Nothing
	// else on this machine looks at it -- the aggregate opcodes that do never
	// appear in an item program, and encoding/likeCaseSensitive both answer
	// from the pager.
	vm.outer = m.outer
	// The frame an OpOuterColumn in an ITEM reads: the aggregate program's own
	// level 1, since compileAggItemProgram gave the item compiler
	// enclosing.outer as ITS level 1. A body run beneath this machine chains to
	// THIS one (execWithParent), so its own level 1 is this register block --
	// which is what OpOuterAggReg reads -- and its level 2 lands here again.
	vm.parent = m.parent
	// The pseudo-rows OpParam reads off the running machine (newMachine's
	// identical copy): a trigger body's NEW/OLD, an upsert's excluded row, a
	// RETURNING row.
	vm.trigOld, vm.trigNew = m.trigOld, m.trigNew
	vm.trigOldRowid, vm.trigNewRowid = m.trigOldRowid, m.trigNewRowid
	vm.trigExcluded, vm.trigExcludedRowid = m.trigExcluded, m.trigExcludedRowid
	vm.trigReturn, vm.trigReturnRowid = m.trigReturn, m.trigReturnRowid
	if cap(vm.regs) < prog.NReg {
		vm.regs = make([]Value, prog.NReg)
	} else {
		vm.regs = vm.regs[:prog.NReg]
		clear(vm.regs)
	}
	// The run-once cache slots an UNCORRELATED body in this item takes. Sized
	// and CLEARED per acquisition rather than kept: the slot numbering belongs
	// to ONE item's program and this machine is shared by every item of every
	// group, so a kept cache would answer item B out of item A's slot. Clearing
	// it also makes each evaluation re-run the body, which a body that is not a
	// pure function of its inputs requires -- see compileAggItemProgram's note.
	if cap(vm.subCache) < prog.NSubCache {
		vm.subCache = make([]subCacheEntry, prog.NSubCache)
	} else {
		vm.subCache = vm.subCache[:prog.NSubCache]
		clear(vm.subCache)
	}
	return vm
}
