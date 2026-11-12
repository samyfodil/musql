package engine

// This file makes a VIRTUAL TABLE source CORRELATED: its module's
// BestIndex/Filter pair is driven once per outer row of the join, with
// a constraint whose right-hand side names a table bound earlier in the
// join nesting.
//
// # The mechanism
//
// annotateVtabCorrelations records, on a virtual-table source bound at
// execution depth >= 1, WHERE conjuncts of the form "vtabcol = <expr>"
// where expr resolves entirely to cursors bound before that level.
//
// emitVtabCorrelatedOpen emits this source's OpOpenDerived inside the join,
// right before its level's OpRewind, with right-hand sides compiled under
// a scope narrowed to outer levels. The module is driven once per outer row.
//
// The conjunct remains in the residual WHERE, so every row is re-tested
// against the constraint that produced it. This can only remove rows, never
// add them.
//
// # Boundary conditions
//
// - Equality operators only, but for a right-hand side that reads only an
//   enclosing query's row, which is fixed for the whole run.
// - Only sources whose rows are driven by the module's BestIndex/Filter.
// - INNER joins only; RIGHT/FULL need the full row set materialized upfront.
// - Not combined with an index-order key from the planner.

// vtabCorrTerm is one virtual-table constraint whose right-hand side is
// computed from the outer loop rather than folded standalone.
//
// conj indexes FromItem.tvfWhere, and col/op are what matchVtabConstraint
// resolved it to. Both are re-checked at runtime (vtabCorrValueIndex) to ensure
// the value was computed for the correct constraint.
//
// rhs is set at compile time and cleared once compiled; reg is the register
// it was compiled into and is the only half carried into execution.
type vtabCorrTerm struct {
	conj int
	arg  int // 1 + the FromItem.TableFuncArgs index this term computes; 0 for a WHERE conjunct
	elem int // 1 + the index of the value an IN loop (vtabInLoopFn) conjunct takes; 0 otherwise
	col  int
	op   VtabOp
	rhs  Expr
	reg  int
}

// vtabCorrValue is a vtabCorrTerm with its register READ -- what
// buildVtabConstraints is handed in place of a fold it cannot perform.
type vtabCorrValue struct {
	conj int
	arg  int
	elem int
	col  int
	op   VtabOp
	v    Value
}

// vtabCorrValueIndex is the entry of corr holding the pre-computed right-hand
// side for tvfWhere conjunct conj, if one was compiled for exactly the
// (column, operator) pair the caller re-derived, or -1. See vtabCorrTerm's doc
// comment for why the pair is re-checked rather than trusted.
func vtabCorrValueIndex(corr []vtabCorrValue, conj, col int, op VtabOp) int {
	for i, cv := range corr {
		if cv.arg == 0 && cv.elem == 0 && cv.conj == conj {
			if cv.col != col || cv.op != op {
				return -1
			}
			return i
		}
	}
	return -1
}

// vtabCorrArgFor is the pre-computed value for a table-valued function's ai'th
// argument, re-checked against the hidden column it was compiled for.
func vtabCorrArgFor(corr []vtabCorrValue, ai, col int) (Value, bool) {
	if i := vtabCorrArgIndex(corr, ai, col); i >= 0 {
		return corr[i].v, true
	}
	return Value{}, false
}

// vtabCorrArgIndex is vtabCorrArgFor's entry of corr, or -1.
func vtabCorrArgIndex(corr []vtabCorrValue, ai, col int) int {
	for i, cv := range corr {
		if cv.arg == ai+1 {
			if cv.col != col {
				return -1
			}
			return i
		}
	}
	return -1
}

// annotateVtabCorrelations records, on every INNER virtual-table source bound
// at execution depth >= 1, the "vtabcol = <outer expr>" conjuncts whose tables
// are all bound before it. It mutates only srcs[i].vtabCorr, so a compile that
// skips it emits the prior behavior (above-the-loops materialization).
func annotateVtabCorrelations(c *compiler, srcs []joinSource, plan joinPlan) {
	// Cleared first, unconditionally: srcs outlives one plan (a codegen entry
	// point may build a plan, fail, and re-plan the same sources), and an
	// annotation left over from a DIFFERENT execution order would name levels
	// that no longer bind what it assumed. Every path out of this function
	// leaves each source annotated for THIS plan or not at all.
	for i := range srcs {
		srcs[i].vtabCorr = nil
	}
	if c == nil || len(plan.execOrder) != len(srcs) {
		return
	}
	// A RIGHT/FULL JOIN's unmatched-row sweep needs the inner side's whole row
	// set marked -- see this file's doc comment -- unless that side is itself
	// a table-valued function whose arguments read the left side. C then
	// builds the function's rows again for the sweep, with the left side
	// NULL-extended (sqlite3WhereRightJoinLoop), and matches by rowid; such a
	// source, when it is the statement's one RIGHT/FULL operand, is driven
	// that way (rightJoinCorr).
	rightTVF := -1
	for i := range srcs {
		if !srcs[i].rightOuter {
			continue
		}
		if rightTVF >= 0 || srcs[i].vtabItem == nil || !srcs[i].vtabItem.TableFunc || !srcs[i].vtabDrivesBestIndex {
			return
		}
		rightTVF = i
	}
	order := plan.execOrder
	for level := 0; level < len(order); level++ {
		origIdx := order[level]
		s := srcs[origIdx]
		if s.vtabItem == nil || !s.vtabDrivesBestIndex {
			continue
		}
		if level < len(plan.autoIdxKeys) && plan.autoIdxKeys[level] != nil {
			continue
		}
		// The cursors bound STRICTLY BEFORE this level: the only ones
		// positioned where emitVtabCorrelatedOpen computes the value.
		outer := make(map[int]bool, level)
		for k := 0; k < level; k++ {
			for _, sc := range sourceScopes(srcs[order[k]]) {
				outer[sc.cursor] = true
			}
		}
		var terms []vtabCorrTerm
		// Table-valued function arguments are treated the same way as
		// WHERE conjuncts, resolved against enclosing query scopes.
		if s.vtabItem.TableFunc {
			hidden := hiddenColIndices(s.scope.cols)
			for ai, arg := range s.vtabItem.TableFuncArgs {
				if ai < len(hidden) && tvfArgIsLateral(c, arg, outer) {
					terms = append(terms, vtabCorrTerm{arg: ai + 1, col: hidden[ai], op: VtabEQ, rhs: arg})
				}
			}
		}
		if origIdx == rightTVF {
			if level > 0 && len(terms) > 0 {
				srcs[origIdx].vtabCorr = terms
			}
			continue
		}
		if rightTVF >= 0 {
			continue
		}
		if level == 0 || s.left {
			if !s.left {
				terms = append(terms, enclosingRowTerms(c, s)...)
			}
			if len(terms) > 0 {
				srcs[origIdx].vtabCorr = withConstantTerms(s, terms)
			}
			continue
		}
		for j, conj := range s.vtabItem.tvfWhere {
			ci, op, rhs, ok := matchVtabConstraint(conj, s.scope.cols, s.scope.name)
			if !ok || op != VtabEQ {
				continue
			}
			// Skip literals and constants that already fold standalone.
			if !keyRefsOnlyCursors(c, rhs, outer) || keyRefsOnlyCursors(c, rhs, nil) {
				continue
			}
			terms = append(terms, vtabCorrTerm{conj: j, col: ci, op: op, rhs: rhs})
		}
		terms = append(terms, inRowsTerms(s)...)
		if len(terms) > 0 {
			srcs[origIdx].vtabCorr = withConstantTerms(s, terms)
		}
	}
}

// inRowsTerms is the rows of every IN subquery s consumes that reads an
// enclosing query's row (vtabInRowsFn): fixed for one run of the statement,
// so computed where any level opens.
func inRowsTerms(s joinSource) []vtabCorrTerm {
	var terms []vtabCorrTerm
	for j, conj := range s.vtabItem.tvfWhere {
		if f, isLoop := conj.(FuncExpr); isLoop && f.Name == vtabInLoopFn {
			for k, e := range f.Args[3:] {
				if rows, ok := e.(FuncExpr); ok && rows.Name == vtabInRowsFn {
					terms = append(terms, vtabCorrTerm{conj: j, elem: k + 1, op: VtabEQ, rhs: e})
				}
			}
		}
	}
	return terms
}

// withConstantTerms adds to a correlated source's terms its arguments and
// constraints that read no row at all, so they are coded once into the
// program with the rest rather than compiled and run again for every outer
// row (foldVtabInputValue).
func withConstantTerms(s joinSource, terms []vtabCorrTerm) []vtabCorrTerm {
	constant := func(e Expr) bool { return exprRefsOnly(e, func(ColumnExpr) bool { return false }) }
	has := func(arg, conj int) bool {
		for _, t := range terms {
			if arg > 0 && t.arg == arg || arg == 0 && t.arg == 0 && t.elem == 0 && t.conj == conj {
				return true
			}
		}
		return false
	}
	if s.vtabItem.TableFunc {
		hidden := hiddenColIndices(s.scope.cols)
		for ai, arg := range s.vtabItem.TableFuncArgs {
			if ai < len(hidden) && !has(ai+1, 0) && constant(arg) {
				terms = append(terms, vtabCorrTerm{arg: ai + 1, col: hidden[ai], op: VtabEQ, rhs: arg})
			}
		}
	}
	for j, conj := range s.vtabItem.tvfWhere {
		if ci, op, rhs, ok := matchVtabConstraint(conj, s.scope.cols, s.scope.name); ok && !has(0, j) && constant(rhs) {
			terms = append(terms, vtabCorrTerm{conj: j, col: ci, op: op, rhs: rhs})
		}
	}
	return terms
}

// enclosingRowTerms is the outermost level's "vtabcol <op> <expr>" conjuncts
// whose right-hand side reads an enclosing query's row and nothing of this
// query's FROM: constant for one run of the subquery, as C codes it
// (wherecode.c:1584), where a standalone fold cannot see that row.
func enclosingRowTerms(c *compiler, s joinSource) []vtabCorrTerm {
	var terms []vtabCorrTerm
	for j, conj := range s.vtabItem.tvfWhere {
		ci, op, rhs, ok := matchVtabConstraint(conj, s.scope.cols, s.scope.name)
		if ok && tvfArgIsLateral(c, rhs, nil) {
			terms = append(terms, vtabCorrTerm{conj: j, col: ci, op: op, rhs: rhs})
		}
		// A consumed IN's values the same way, each its own term.
		if f, isLoop := conj.(FuncExpr); isLoop && f.Name == vtabInLoopFn && len(f.Args) > 3 {
			if _, isSub := f.Args[3].(SubqueryExpr); isSub {
				continue
			}
			for k, e := range f.Args[3:] {
				if rows, ok := e.(FuncExpr); ok && rows.Name == vtabInRowsFn || tvfArgIsLateral(c, e, nil) {
					terms = append(terms, vtabCorrTerm{conj: j, elem: k + 1, op: VtabEQ, rhs: e})
				}
			}
		}
	}
	return terms
}

// tvfArgIsLateral reports whether a table-valued function argument reads a
// row: a table bound strictly before this level (allowed), or an enclosing
// query's, and nothing else. A name this query's own FROM binds anywhere else
// -- the function itself, or a later table -- is not lateral, and neither is a
// constant, which folds standalone as before (foldVtabInputValue). Both leave
// the argument on that fold, which refuses what it cannot answer.
func tvfArgIsLateral(c *compiler, arg Expr, allowed map[int]bool) bool {
	// A scalar subquery with no FROM of its own ("(SELECT t.json)") reads
	// nothing but enclosing rows, so its result expression is what is lateral
	// or not. The argument still compiles as the subquery.
	if sq, ok := arg.(SubqueryExpr); ok && sq.Stmt != nil && len(sq.Stmt.From) == 0 && len(sq.Stmt.Compound) == 0 &&
		len(sq.Stmt.CTEs) == 0 && len(sq.Stmt.Columns) == 1 && !sq.Stmt.Columns[0].Star && sq.Stmt.Where == nil &&
		len(sq.Stmt.GroupBy) == 0 && sq.Stmt.Having == nil && sq.Stmt.Limit == nil && sq.Stmt.LimitParam == nil {
		arg = sq.Stmt.Columns[0].Expr
	}
	if !walkExprShallow(arg, func(fc FuncExpr) bool {
		return fc.Over == nil && fc.Filter == nil && len(fc.OrderBy) == 0 && !isAggregateCall(fc)
	}) {
		return false
	}
	reads := false
	ok := exprRefsOnly(arg, func(x ColumnExpr) bool {
		cursor, _, _, found, fb, hard := resolveInScopes(c.scopes, x, c.pager)
		if hard != nil || fb.has {
			return false
		}
		if found {
			reads = true
			return allowed[cursor]
		}
		for oc := c.outer; oc != nil; oc = oc.outer {
			_, _, _, ofound, ofb, ohard := resolveInScopes(oc.scopes, x, oc.pager)
			if ohard != nil || ofb.has {
				return false
			}
			if ofound {
				reads = true
				return true
			}
		}
		return false
	})
	return ok && reads
}

// emitVtabCorrelatedOpen emits a correlated virtual-table source's
// OpOpenDerived at the top of its join level, inside every outer loop,
// right before this level's OpRewind. A no-op unless annotateVtabCorrelations
// recorded terms for this source.
//
// Right-hand sides are compiled under a narrowed scope including only
// levels bound strictly before this one. Must run before emitJoinSeekHint
// at the same level, as this is the only open such a source gets. stream is
// derivedSource.vtabStream.
func (c *compiler) emitVtabCorrelatedOpen(srcs []joinSource, order []int, level int, s joinSource, stream bool) error {
	if len(s.vtabCorr) == 0 {
		return nil
	}
	saved := c.scopes
	var visible []compileScope
	for i := 0; i < level; i++ {
		visible = append(visible, sourceScopes(srcs[order[i]])...)
	}
	c.scopes = visible
	defer func() { c.scopes = saved }()

	terms := make([]vtabCorrTerm, len(s.vtabCorr))
	for i, t := range s.vtabCorr {
		reg, err := c.compileExpr(t.rhs)
		if err != nil {
			return err
		}
		t.rhs = nil // compile-time only; the payload carries the register
		t.reg = reg
		terms[i] = t
	}
	c.emit(Instruction{Op: OpOpenDerived, P1: s.scope.cursor, P2: s.derivedSlot,
		P4: &derivedSource{vtab: s.vtabItem, vtabTrig: c.trig, vtabCorr: terms, tbl: s.tbl, dbIdx: s.dbIdx, rightJoin: s.rightOuter, vtabStream: stream}})
	return nil
}

// vtabStreamable reports whether the virtual table s, at join level level,
// may stream (derivedSource.vtabStream): no RIGHT JOIN sweeps it, and no
// automatic index, multi-OR grouping or join seek works on its rows.
func vtabStreamable(plan joinPlan, level int, s joinSource) bool {
	return !s.rightOuter && s.joinIdxSeek == nil && s.joinSeekKeyExpr == nil &&
		(level >= len(plan.autoIdxKeys) || plan.autoIdxKeys[level] == nil)
}
