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
// - Equality operators only.
// - Only sources whose rows are driven by the module's BestIndex/Filter.
// - INNER joins only; RIGHT/FULL need the full row set materialized upfront.
// - Not combined with an index-order key from the planner.

// vtabCorrTerm is one virtual-table constraint whose right-hand side is
// computed from the outer loop rather than folded standalone.
//
// conj indexes FromItem.tvfWhere, and col/op are what matchVtabConstraint
// resolved it to. Both are re-checked at runtime (vtabCorrValueFor) to ensure
// the value was computed for the correct constraint.
//
// rhs is set at compile time and cleared once compiled; reg is the register
// it was compiled into and is the only half carried into execution.
type vtabCorrTerm struct {
	conj int
	arg  int // 1 + the FromItem.TableFuncArgs index this term computes; 0 for a WHERE conjunct
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
	col  int
	op   VtabOp
	v    Value
}

// vtabCorrValueFor returns the pre-computed right-hand side for tvfWhere
// conjunct conj, if one was compiled for exactly the (column, operator) pair
// the caller re-derived. See vtabCorrTerm's doc comment for why the pair is
// re-checked rather than trusted.
func vtabCorrValueFor(corr []vtabCorrValue, conj, col int, op VtabOp) (Value, bool) {
	for _, cv := range corr {
		if cv.arg == 0 && cv.conj == conj {
			if cv.col != col || cv.op != op {
				return Value{}, false
			}
			return cv.v, true
		}
	}
	return Value{}, false
}

// vtabCorrArgFor is vtabCorrValueFor for a table-valued function's ai'th
// argument, re-checked against the hidden column it was compiled for.
func vtabCorrArgFor(corr []vtabCorrValue, ai, col int) (Value, bool) {
	for _, cv := range corr {
		if cv.arg == ai+1 {
			return cv.v, cv.col == col
		}
	}
	return Value{}, false
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
			if len(terms) > 0 {
				srcs[origIdx].vtabCorr = terms
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
		if len(terms) > 0 {
			srcs[origIdx].vtabCorr = terms
		}
	}
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
// at the same level, as this is the only open such a source gets.
func (c *compiler) emitVtabCorrelatedOpen(srcs []joinSource, order []int, level int, s joinSource) error {
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
		P4: &derivedSource{vtab: s.vtabItem, vtabTrig: c.trig, vtabCorr: terms, tbl: s.tbl, dbIdx: s.dbIdx, rightJoin: s.rightOuter}})
	return nil
}
