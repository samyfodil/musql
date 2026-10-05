// The compiler half of the window path: a window function's operands (its
// spec's PARTITION BY / ORDER BY keys and, for positional functions, its
// arguments) become extra batch columns, and its frame-bound offsets become
// registers.
//
// This is sqlite3WindowRewrite's shape (window.c:958): every operand is appended
// to a generated sub-select's expression list (window.c:1029-1030, :1047) and the
// window machinery reads them back with OP_Column only (window.c:1681, :1694,
// :1942, :1952, :1966, :1975, :1982). musql's batch (OpWindowAppend,
// vdbe_window.go) is that ephemeral table. Frame offsets have no row in scope and
// are coded into registers (window.c:2940, :2944).
package engine

import "fmt"

// windowOperandLowerable reports whether e may be compiled as an ordinary
// per-row scalar expression.
//
// A nested window call (a FuncExpr still carrying Over) is refused: compileFunc
// has no Over case and would silently drop the OVER. A bare aggregate is refused
// explicitly rather than relying on it failing elsewhere.
//
// A subquery operand lowers, except for specOutwardAggInSubquery's family (a
// FROM-less subquery whose bare aggregate re-associates outward), e.g.
//
//	SELECT a, sum(a) OVER (ORDER BY (SELECT sum(t1.a) OVER ())) FROM t1
//
// compileScanWindow already applies that check to spec keys; this covers
// arguments. walkExprShallow does not descend into subqueries, matching
// selectWindowRewriteSelectCb's prune at a nested Select (window.c:831-842).
func windowOperandLowerable(pager *ReadOnlyPager, e Expr, scopes []tableScope) bool {
	if containsSubquery(e) && specOutwardAggInSubquery(pager, e, scopes) {
		return false
	}
	ok := true
	walkExprShallow(e, func(fc FuncExpr) bool {
		if fc.Over != nil || isAggregateCall(fc) {
			ok = false
		}
		return ok
	})
	return ok
}

// windowPositionalArgs reports whether the window machinery itself reads this
// function's ARGUMENTS back: ntile's N, lead/lag's value/offset/default, and
// first_value/last_value/nth_value's value and N (all vdbe_window_frame.go).
func windowPositionalArgs(name string) bool {
	switch name {
	case "ntile", "lead", "lag", "first_value", "last_value", "nth_value":
		return true
	}
	return false
}

// windowAggArgsLowerable reports whether an ordinary aggregate window function
// may have its argument lowered into batch columns for aggItem.step to read.
//
// SQLite buffers arguments as pWin->iArgCol (window.c:1046-1047) and reads them
// with OP_Column (window.c:1681). This is an allow-list so that
// stampWindowAggRegs' positional mapping is provable: for these names
// planAggregateCallKind sets expr=Args[0] and, for group_concat/string_agg,
// sepExpr=Args[1]. The FILTER is buffered regardless (see planWindowOperands).
//
// json_group_array/json_group_object are excluded: they are SQLITE_SUBTYPE
// aggregates (json.c:5699, :5706), for which SQLite sets pWin->bExprArgs and
// re-codes the argument at step time (window.c:1041-1044, :1733). Buffering them
// would evaluate rows C never evaluates, e.g. over t(a)=(1,2,3,7):
//
//	SELECT a, json_group_array(CASE WHEN a=7 THEN abs(-9223372036854775808)
//	  ELSE a END) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING)
//
// answers in C, while the sum() spelling raises "integer overflow" because a
// buffered operand is computed for every buffered row.
func windowAggArgsLowerable(call *windowCall) bool {
	switch call.name {
	case "count", "sum", "total", "avg", "min", "max":
		return len(call.args) == 1
	case "group_concat", "string_agg":
		return len(call.args) == 1 || len(call.args) == 2
	}
	return false
}

// planWindowOperands assigns every lowerable window operand a batch column
// (numbered after the entry's [cols.., rowids..] block, read by
// vdbe.windowOperand) and returns the expressions in column order. Calls sharing
// one spec pointer share its key columns.
//
// The "must" flag on add separates two kinds of operand:
//   - spec keys and positional-function arguments are read only through
//     vdbe.windowOperand, so a refusal declines the statement here (RULE #1);
//   - an ordinary aggregate's argument and FILTER have a second lowering, the
//     step-arg program (compileWindowStepArgs), so a refusal leaves the column
//     at -1 and windowAggOperandsServed decides later whether either served it.
//
// SQLite likewise never evaluates an unbuffered operand elsewhere except the
// bExprArgs step-time re-code (window.c:1041-1044, :1733).
func planWindowOperands(pager *ReadOnlyPager, plan *windowPlan) ([]Expr, error) {
	var (
		opExprs []Expr
		refused error
	)
	add := func(e Expr, must bool) int {
		if e == nil {
			return -1
		}
		if !windowOperandLowerable(pager, e, plan.scopes) {
			if must && refused == nil {
				refused = fmt.Errorf("%w: window operand is a nested window call, an aggregate, "+
					"or a subquery aggregate that re-associates with this query", errVDBEUnsupported)
			}
			return -1
		}
		opExprs = append(opExprs, e)
		return len(opExprs) - 1
	}
	// The outer projection's buffered columns come first, at exactly 0..k-1,
	// because windowBufCols already assigned those numbers. They are plain
	// ColumnExprs or aggregate-result placeholders, appended directly rather than
	// through add. emitWindowOperands recognises them by position and returns
	// their compile error unwrapped: it is a resolution failure, not a capability
	// gap.
	for _, e := range plan.projLifts {
		opExprs = append(opExprs, e)
	}
	for ci := range plan.calls {
		call := &plan.calls[ci]
		call.partCols, call.orderCols, call.argCols = nil, nil, nil
		shared := -1
		for cj := 0; cj < ci; cj++ {
			if plan.calls[cj].spec == call.spec {
				shared = cj
				break
			}
		}
		switch {
		case shared >= 0:
			call.partCols = plan.calls[shared].partCols
			call.orderCols = plan.calls[shared].orderCols
		case call.spec != nil:
			for _, e := range call.spec.PartitionBy {
				call.partCols = append(call.partCols, add(e, true))
			}
			for _, ot := range call.spec.OrderBy {
				call.orderCols = append(call.orderCols, add(ot.Expr, true))
			}
		}
		call.filterCol = -1
		if windowPositionalArgs(call.name) {
			for _, a := range call.args {
				call.argCols = append(call.argCols, add(a, true))
			}
		} else {
			if windowAggArgsLowerable(call) {
				// An ordinary aggregate's ARGUMENT, which only aggItem.step
				// reads -- through the register seam aggItem.rowRegs already
				// provides. See windowAggArgsLowerable for the allow-list and
				// stampWindowAggRegs (vdbe_window.go) for the stamping.
				for _, a := range call.args {
					call.argCols = append(call.argCols, add(a, false))
				}
			}
			// The FILTER is buffered for every window function, not gated on the
			// argument allow-list: SQLite appends it outside the bExprArgs branch
			// (window.c:1049-1052) and windowAggStep reads it with OP_Column
			// (window.c:1694) before re-coding a SUBTYPE aggregate's arguments
			// (window.c:1733).
			call.filterCol = add(call.filter, false)
		}
	}
	plan.nOps = len(opExprs)
	if refused != nil {
		return nil, refused
	}
	return opExprs, nil
}

// emitWindowOperands computes each lowered operand into its register of the
// block at opBase, from inside the scan body. A failed compile declines the
// statement (RULE #1); a projection lift (i < len(projLifts)) returns its error
// unwrapped.
//
// The block is allocated once by the caller because emitJoinLoops may emit this
// body more than once (LEFT/FULL JOIN NULL-extension, RIGHT JOIN sweep). A
// failed compile truncates exactly its own instructions, which is safe because
// nothing jumps into the block.
func emitWindowOperands(c *compiler, opBase int, opExprs []Expr, plan *windowPlan) error {
	for i, e := range opExprs {
		mark := len(c.insns)
		r, err := c.compileExpr(e)
		if err != nil {
			c.insns = c.insns[:mark]
			if i < len(plan.projLifts) {
				// The compiled projection reads this column by number, so a missing
				// one is an error (and a nil plan.proj would panic in windowFinal).
				return err
			}
			// Every operand reaching here must lower: aggItem.rowValue fails at
			// run time on an unfilled register, so decline at compile time.
			return fmt.Errorf("%w: window operand: %v", errVDBEUnsupported, err)
		}
		c.emit(Instruction{Op: OpSCopy, P1: r, P2: opBase + i})
	}
	return nil
}

// frameOffsetConst is sqlite3ExprIsConstant(0, pExpr) applied to a frame
// bound's offset (exprNodeIsConstant, expr.c:2541, with eCode==1 and no Parse;
// subqueries fail per exprIsConst, expr.c:2618-2626). It returns the normalized
// expression because one arm rewrites the node.
//
// Rejected: TK_COLUMN and relatives (expr.c:2575-2588), TK_FUNCTION
// (expr.c:2562-2567), TK_RAISE (expr.c:2589-2598). A bound parameter is
// constant (expr.c:2599-2615), so "ROWS ? PRECEDING" is legal.
//
// TK_ID (expr.c:2568-2573) is first tried as true/false and becomes a
// constant literal; sqlite3WindowOffsetExpr runs at parse time, so a column
// named "true" can never shadow it. Here that is ColumnExpr.FallbackLiteral.
//
// LIKE/GLOB/MATCH/REGEXP are function calls in the grammar
// (parse.y:1363-1382) and so non-constant. CAST and CASE stay constant.
//
// Composite arms build fresh nodes: the AST is shared with the prepared
// statement and must not be edited in place.
func frameOffsetConst(e Expr) (Expr, bool) {
	both := func(a, b Expr) (Expr, Expr, bool) {
		na, oka := frameOffsetConst(a)
		nb, okb := frameOffsetConst(b)
		return na, nb, oka && okb
	}
	switch x := e.(type) {
	case nil:
		return nil, true
	case LiteralExpr, ParamExpr:
		return e, true
	case ColumnExpr:
		if x.FallbackLiteral != nil && x.Qualifier == "" {
			return LiteralExpr{Val: *x.FallbackLiteral}, true // sqlite3ExprIdToTrueFalse
		}
		return e, false
	case RaiseExpr, FuncExpr, LikeExpr, GlobExpr, MatchExpr, SubqueryExpr, ExistsExpr:
		return e, false
	case UnaryExpr:
		n, ok := frameOffsetConst(x.X)
		x.X = n
		return x, ok
	case BinaryExpr:
		l, r, ok := both(x.L, x.R)
		x.L, x.R = l, r
		return x, ok
	case IsNullExpr:
		n, ok := frameOffsetConst(x.X)
		x.X = n
		return x, ok
	case CollateExpr:
		n, ok := frameOffsetConst(x.X)
		x.X = n
		return x, ok
	case CastExpr:
		n, ok := frameOffsetConst(x.X)
		x.X = n
		return x, ok
	case BetweenExpr:
		lo, hi, ok := both(x.Lo, x.Hi)
		n, okx := frameOffsetConst(x.X)
		x.X, x.Lo, x.Hi = n, lo, hi
		return x, ok && okx
	case InExpr:
		if x.Sub != nil {
			return e, false
		}
		n, ok := frameOffsetConst(x.X)
		list := make([]Expr, len(x.List))
		for i, it := range x.List {
			ni, oki := frameOffsetConst(it)
			list[i], ok = ni, ok && oki
		}
		x.X, x.List = n, list
		return x, ok
	case CaseExpr:
		base, els, ok := both(x.Base, x.Else)
		whens := make([]WhenClause, len(x.Whens))
		for i, w := range x.Whens {
			nw, nt, okw := both(w.When, w.Then)
			whens[i], ok = WhenClause{When: nw, Then: nt}, ok && okw
		}
		x.Base, x.Else, x.Whens = base, els, whens
		return x, ok
	default:
		// RowExpr, and anything added later: treated as non-constant, which is
		// the fail-safe direction -- SQLite's own answer for a non-constant
		// offset is an error, never a value.
		return e, false
	}
}

// planWindowFrameOffsets codes each frame's offset into its own register once,
// ahead of the scan. C codes them per partition (window.c:2940, :2944); the
// expression is constant, so once per statement gives the same value.
//
// A non-constant offset is replaced by NULL, as sqlite3WindowOffsetExpr does
// (window.c:1163-1170), so windowCheckValue's "frame starting offset must be a
// non-negative integer" falls out of the existing numeric test.
//
// Registers are 1-biased so zero means "no offset", like aggItem.rowRegs.
func planWindowFrameOffsets(c *compiler, plan *windowPlan) error {
	for ci := range plan.calls {
		call := &plan.calls[ci]
		if call.spec == nil || call.spec.Frame == nil {
			continue
		}
		var err error
		if call.startOffReg, err = compileFrameOffset(c, call.spec.Frame.Start.Offset); err != nil {
			return err
		}
		if call.endOffReg, err = compileFrameOffset(c, call.spec.Frame.End.Offset); err != nil {
			return err
		}
	}
	return nil
}

// compileFrameOffset codes one frame bound's offset into a fresh register and
// returns that register 1-biased (0 when the bound carries no offset). A
// constant expression the compiler cannot lower is a clean DECLINE, never a
// fallback -- AGENTS.md Rule 1.
func compileFrameOffset(c *compiler, e Expr) (int, error) {
	if e == nil {
		return 0, nil
	}
	dst := c.alloc()
	e, ok := frameOffsetConst(e)
	if !ok {
		c.emit(Instruction{Op: OpNull, P2: dst}) // sqlite3WindowOffsetExpr, window.c:1167
		return dst + 1, nil
	}
	r, err := c.compileExpr(e)
	if err != nil {
		return 0, fmt.Errorf("%w: window frame offset: %v", errVDBEUnsupported, err)
	}
	c.emit(Instruction{Op: OpSCopy, P1: r, P2: dst})
	return dst + 1, nil
}

// buildWindowProjList merges the outer select list and ORDER BY into one list
// (windowPlan.projExprs), recording each ORDER BY term's position in
// windowPlan.projOrder (-1 for a term naming an output column). One list means
// one evaluation order for errors, as SQLite codes both against the same
// ephemeral row (selectWindowRewriteEList, window.c:1022-1023).
func buildWindowProjList(plan *windowPlan) {
	plan.projExprs = append(plan.projExprs, plan.outs...)
	plan.projOrder = make([]int, len(plan.orderExprs))
	for j, e := range plan.orderExprs {
		if e == nil { // an ORDER BY ordinal/alias: taken from the projected row
			plan.projOrder[j] = -1
			continue
		}
		plan.projOrder[j] = len(plan.projExprs)
		plan.projExprs = append(plan.projExprs, e)
	}
}

// windowProjection is the compiled outer projection of a window query: one
// Program that reads a batch row's columns and the window results out of
// registers and leaves each projected value in a register.
//
// C rewrites every column, aggregate and foreign-window node of the outer list
// into a read of the ephemeral cursor (selectWindowRewriteExprCb,
// window.c:805-818), and a window function node codes as its regResult
// (expr.c:5358-5360). The batch entry is that row and windowFinal's per-call
// values are regResult.
type windowProjection struct {
	prog *Program

	// The register block, laid out exactly as compileSelfRowExpr lays out its
	// iSelfTab block (vdbe_codegen.go): the rowid register sits "immediately
	// prior to the first column" (expr.c:5051), then the batch's [cols..]
	// block, then one register per window call.
	rowidReg int
	colBase  int
	winBase  int
	nCols    int
	nRowids  int
	nWin     int

	// chained says this program was compiled with the scan's own compiler as
	// its outer chain (compileWindowRowProgram's chainOuter), so its machine
	// must be given that scan's frame as a parent for OpOuterAggReg to walk.
	// Only a STEP-ARG program is; see compileWindowRowProgram.
	chained bool

	// buf is the BUFFERED block: the references this projection could not
	// resolve out of the row above and had computed into batch operand columns
	// instead (windowBufCols). bufRegs[i] is the register operand column i is
	// seeded into, copied out of buf once the compile is done so run touches no
	// compile-time state.
	buf     *windowBufCols
	bufRegs []int

	// resultReg[i] is where projExprs[i]'s value lands. Two entries may name
	// the SAME register (two bare reads of one column both resolve to that
	// column's row register), which is why run copies values out rather than
	// handing the register file over.
	resultReg []int
}

// windowProjScopeLowerable reports whether a window query's row scope can be
// addressed as a register block.
//
// A RIGHT/FULL JOIN coalesce chain is refused: which source it reads depends on
// row values, not a static register. A scope whose columns are not exactly its
// slice of [cols..] is refused because the program addresses by position.
//
// Joins lower: compileWindowProjection sets compiler.regScopeStrict, so
// resolveRowReg raises "ambiguous column name" at compile time for a name
// offered by two scopes rather than letting the last win. C needs no such rule
// because selectWindowRewriteExprCb (window.c:748) rewrites already-resolved
// nodes; this compiler re-resolves, so it owes the check.
//
// A join group alias is unknown to resolveRowReg and falls through to "no such
// column", the fail-safe direction.
func windowProjScopeLowerable(plan *windowPlan) bool {
	if len(plan.scopes) == 0 {
		return plan.nCols == 0
	}
	total := 0
	for _, s := range plan.scopes {
		if s.coalesceFallback != nil || s.coalesced != nil || s.offset != total {
			return false
		}
		total += len(s.cols)
	}
	// A joined scope reads its rowid out of the batch entry's trailing rowid
	// block, so there must be one slot per scope for it to read --
	// compileScanWindow emits exactly one OpRowid per join source. A SINGLE
	// scope may legitimately have none (a FROM-less or derived-only plan), and
	// resolveRowReg answers a rowid reference against such a scope only when
	// noRowid is false, which such a plan never has.
	return total == plan.nCols && (len(plan.scopes) == 1 || plan.nRowids == len(plan.scopes))
}

// compileWindowProjection compiles plan.projExprs against a register block
// holding one batch row plus the window results. It is total: a projection that
// does not lower is an error.
//
//   - An ambiguous reference over a join raises "ambiguous column name"
//     (resolve.c:785) via compiler.regScopeStrict.
//   - An uncorrelated subquery gets a run-once cache slot (Program.NSubCache,
//     sized by windowProjection.machine).
//   - A reference to an enclosing query is buffered into the batch
//     (windowBufCols), including one inside a subquery of the projection via
//     compileColumn's outer-chain bufOut arm.
//
// The machine carries only registers and the subquery cache, so a compile that
// allocated cursors, record registers, sorters or distinct sets is refused
// (compileSelfRowExpr's rule) rather than panicking at run time.
//
// A select-list subquery naming the window query's own row lowers via
// compileColumn's outer-regScope arm; this is C lifting such a TK_COLUMN into
// the ephemeral row (window.c:756-771, :802-819). bindOuterAggRef consults the
// register row under the regRow flag so nested window queries see it too.
//
// This program is not made a frame in the parent chain: buildOuterEvalCtx would
// read cursors OpWindowFinal has already exhausted, a wrong value. C buffers
// outer references as columns instead (window.c:788-817), and so does this.
//
// An aggregate call never reaches here: C rejects one in a non-aggregate window
// query's ORDER BY (disallowAggregatesInOrderByCb, window.c:942-949, :987-991),
// and a select-list aggregate routes to compileScanGroupedWindow.
func compileWindowProjection(parent *compiler, plan *windowPlan) (*windowProjection, error) {
	plan.projLifts = nil
	p, err := compileWindowRowProgram(parent, plan, plan.projExprs, len(plan.calls), true, false)
	if err != nil {
		return nil, err
	}
	plan.projLifts = p.buf.exprs
	return p, nil
}

// windowStepArgsLowerable reports whether call's arguments may be computed by a
// compiled program at step time, which is where SQLite computes exactly these.
//
// They are the SUBTYPE aggregates json_group_array/json_group_object
// (json.c:5699, :5706): sqlite3WindowRewrite sets pWin->bExprArgs and does not
// buffer their arguments (window.c:1041-1044); windowAggStep re-codes them at
// step time, repointing OP_Column reads at the frame's current row
// (window.c:1728-1745) before OP_AggStep (window.c:1749). So they compile
// against the buffered row's columns (compileWindowRowProgram) and run per step.
//
// The rowRegs mapping is positional and provable: planAggregateCallKind sets
// expr=Args[0] (and sepExpr=Args[1] for json_group_object) and refuses other
// arities.
func windowStepArgsLowerable(call *windowCall) bool {
	switch call.name {
	case "json_group_array", "jsonb_group_array":
		return len(call.args) == 1
	case "json_group_object", "jsonb_group_object":
		return len(call.args) == 2
	}
	return false
}

// compileWindowStepArgs compiles one aggregate window call's arguments into a
// program windowAggregate runs once per stepped row, or returns nil.
//
// It requires a lowered FILTER column: C's OP_IfNot on the FILTER jumps past
// the argument coding (window.c:1694-1696, :1758), and windowAggregate does the
// same by reading that column first.
//
// Two families reach it:
//   - SUBTYPE aggregates (windowStepArgsLowerable), whose arguments C re-codes
//     at step time (window.c:1728-1745); nested window calls are refused for
//     windowOperandLowerable's reason.
//   - an ordinary aggregate whose argument got no batch column
//     (windowAggFallsBackToStepArgs). Evaluating it per step against the
//     buffered row yields the same values. If this also refuses, the statement
//     declines (windowAggOperandsServed).
func compileWindowStepArgs(parent *compiler, plan *windowPlan, call *windowCall) *windowProjection {
	fallback := windowAggFallsBackToStepArgs(call)
	if !windowStepArgsLowerable(call) && !fallback {
		return nil
	}
	if call.filter != nil && call.filterCol < 0 {
		return nil
	}
	for _, a := range call.args {
		// The nested-OVER refusal applies to both families; the rest of
		// windowOperandLowerable's rule is about what may be BUFFERED, which
		// the fallback family is here precisely because it could not be.
		if a == nil {
			return nil
		}
		if fallback {
			if nestedWindowCall(a) {
				return nil
			}
			continue
		}
		if !windowOperandLowerable(parent.pager, a, plan.scopes) {
			return nil
		}
	}
	// nWin is zero: window results do not exist yet at step time. No
	// outer-reference lift (false): a step argument is evaluated per stepped
	// row, matching C (window.c:1728-1745). A refusal returns nil;
	// windowAggOperandsServed combines it with the batch-column answer.
	p, err := compileWindowRowProgram(parent, plan, call.args, 0, false, true)
	if p != nil {
		p.chained = true
	}
	if err != nil {
		return nil
	}
	return p
}

// windowAggFallsBackToStepArgs reports whether call is an ordinary aggregate
// whose arguments got no batch columns, leaving a step-arg program as the only
// way to serve them. Only windowAggArgsLowerable's names qualify, since only
// their args[0]/args[1] mapping onto aggExprArg/aggExprSep is provable.
func windowAggFallsBackToStepArgs(call *windowCall) bool {
	if !windowAggArgsLowerable(call) || len(call.args) == 0 {
		return false
	}
	for i := range call.args {
		if call.argCol(i) < 0 {
			return true
		}
	}
	return false
}

// nestedWindowCall reports whether e contains a call carrying its own OVER
// clause. It is windowOperandLowerable's first refusal on its own, kept
// separate because the step-arg fallback needs THAT rule and not the rest:
// compiling a window call as an ordinary function silently drops the OVER,
// which is a wrong answer, while "this cannot be BUFFERED" is exactly the
// condition the fallback exists to handle.
func nestedWindowCall(e Expr) bool {
	nested := false
	walkExprShallow(e, func(fc FuncExpr) bool {
		if fc.Over != nil {
			nested = true
		}
		return !nested
	})
	return nested
}

// windowAggOperandsServed returns why an ordinary aggregate window call would
// reach aggItem.rowValue with an unfilled slot, or nil. rowValue fails at run
// time on one, so it is declined here, once the step-arg compile is known.
//
// A slot is unfilled when windowAggArgsLowerable refused the shape,
// windowOperandLowerable refused an argument, or compileWindowStepArgs refused
// too. The FILTER column is checked the same way (window.c:1694-1696).
//
// Ranking functions take no arguments, and positional functions' operands are
// already refused at planning time.
func windowAggOperandsServed(call *windowCall) error {
	if isWindowFunctionName(call.name) || windowPositionalArgs(call.name) {
		return nil
	}
	if call.filter != nil && call.filterCol < 0 {
		return fmt.Errorf("%w: a window aggregate's FILTER is an expression this scan body cannot buffer", errVDBEUnsupported)
	}
	if call.stepArgs != nil {
		// The SUBTYPE family: its arguments are re-coded at step time
		// (window.c:1728-1745) and stampWindowStepArgRegs points the
		// accumulator at those slots instead of at batch columns.
		return nil
	}
	for i := range call.args {
		if call.argCol(i) < 0 {
			return fmt.Errorf("%w: a window aggregate's argument %d is an expression this scan body cannot buffer, and it has no step-argument program either", errVDBEUnsupported, i)
		}
	}
	return nil
}

// compileWindowRowProgram is the shared builder: exprs compiled against one
// batch row's register block, plus nWin registers holding the window calls'
// already computed results.
func compileWindowRowProgram(parent *compiler, plan *windowPlan, exprs []Expr, nWin int, lift, chainOuter bool) (*windowProjection, error) {
	// A scope the register block cannot address by position (coalesce chain,
	// RIGHT/FULL fallback, mismatched layout): every column reference is
	// buffered into the batch instead, as C does with every outer column
	// reference (window.c:788-817), so the scan body resolves it with the
	// join's own rules. A step-arg program (lift == false) has no buffering
	// seam and still refuses.
	regAddressable := windowProjScopeLowerable(plan)
	if !regAddressable && !lift {
		return nil, fmt.Errorf("%w: window projection over a row scope its register block cannot address", errVDBEUnsupported)
	}
	// Register scopes are FROM items, so a name two offer is ambiguous
	// (regScopeStrict). rowLive makes the register row the correlated row for
	// subqueries compiled beneath (window.c:756-771).
	c := &compiler{pager: parent.pager, regScopeStrict: true, rowLive: true}
	// Only the step-arg program gets the enclosing chain, for aggResultReg:
	// an aggregate item's placeholder in a window aggregate's argument is
	// coded only here (C re-codes it at step time, window.c:1728-1745), read
	// with OpOuterAggReg. The projection gets none: it lifts outer references
	// into the buffered row instead (window.c:756-771, :802-819).
	if chainOuter {
		c.outer = parent
	}
	if lift {
		c.nQueryLoop, c.nQueryLoopKnown = plan.projNQL, plan.projNQLKnown
	}
	initAddr := c.emit(Instruction{Op: OpInit})
	c.patch(initAddr, 1)

	p := &windowProjection{nCols: plan.nCols, nRowids: plan.nRowids, nWin: nWin}
	// One rowid register per joined table (the batch's trailing rowid block),
	// with a single one reserved even for a rowid-less plan so the block still
	// sits "immediately prior to the first column" (expr.c:5051) exactly as
	// compileSelfRowExpr's does.
	p.rowidReg = c.allocN(max(1, p.nRowids))
	p.colBase = c.allocN(p.nCols)
	p.winBase = c.allocN(p.nWin)
	if p.nWin > 0 {
		c.winRegs = rowRegsFor(p.winBase, p.nWin)
	}
	for i := 0; regAddressable && i < len(plan.scopes); i++ {
		// A copy, not &plan.scopes[i]: regScope holds the pointer for the whole
		// compile and the plan outlives it.
		sc := plan.scopes[i]
		c.pushRegScope(regScope{
			scope:    &sc,
			regs:     rowRegsFor(p.colBase+sc.offset, len(sc.cols)),
			// In range in every admitted shape: a single scope only ever asks
			// for slot 0, and a JOIN is admitted only when nRowids == len(scopes).
			rowidReg: p.rowidReg + i,
		})
	}
	// Whatever names a query ENCLOSING this one is BUFFERED as a batch column
	// instead of resolved here, which is what C does with every column
	// reference in an outer window projection -- see windowBufCols. Installed
	// AFTER the register scopes are pushed, so a name this row does hold is
	// resolved by resolveRowReg and never reaches the buffering arm at all.
	if lift {
		// scopes is what the seam REFUSES to buffer, because the register
		// block serves it instead. With no register block (!regAddressable)
		// there is nothing it must refuse: every reference goes to the batch.
		bufScopes := plan.scopes
		if !regAddressable {
			bufScopes = nil
		}
		p.buf = &windowBufCols{scopes: bufScopes, aff: parent.affCtx()}
		c.bufOut = p.buf
	}
	for _, e := range exprs {
		r, err := c.compileExpr(e)
		if err != nil {
			// compileExpr's own wording, unwrapped: the one error that reaches
			// here for a statement C SQLite ANSWERS is a capability gap
			// (already errVDBEUnsupported-wrapped by whoever raised it), and
			// the one that reaches here for a statement it REJECTS is the
			// ambiguous-column error, which must keep resolve.c:785's wording.
			return nil, err
		}
		p.resultReg = append(p.resultReg, r)
	}
	// nSub is NOT in this set -- the machine sizes its run-once cache from
	// Program.NSubCache below. See the doc comment's "Subqueries" section.
	if c.nCursor != 0 || c.nRec != 0 || c.nSorter != 0 || c.nDistinct != 0 {
		return nil, fmt.Errorf("%w: window projection needs VM state its register machine has none of (cursors %d, record registers %d, sorters %d, distinct sets %d)",
			errVDBEUnsupported, c.nCursor, c.nRec, c.nSorter, c.nDistinct)
	}
	if p.buf != nil {
		p.bufRegs = p.buf.regs
	}
	// No OpResultRow: this program leaves each value in a register and run
	// reads them out, the same reason compileSelfRowExpr gives -- it runs once
	// per row of the batch.
	c.emit(Instruction{Op: OpHalt})
	p.prog = &Program{Insns: c.insns, NReg: c.nReg, NSubCache: c.nSub}
	return p, nil
}

// machine returns the register machine p's program runs on, built once per
// windowFinal. It carries registers, the run-once subquery cache, the pager
// (for case_sensitive_like and text encoding) and bound parameters, nothing
// else. One cache per windowFinal matches OP_Once: a correlated window query
// re-runs windowFinal on a fresh machine per enclosing row.
func (p *windowProjection) machine(m *vdbe) *vdbe {
	pm := &vdbe{
		regs:     make([]Value, p.prog.NReg),
		subCache: make([]subCacheEntry, p.prog.NSubCache),
		pager:    m.pager,
		params:   m.params,
	}
	if p.chained {
		// A step-arg program compiled with the scan's own compiler as its outer
		// chain (compileWindowRowProgram's chainOuter) reads across frames with
		// OpOuterAggReg, whose P5 counts hops from HERE. m is the machine
		// running the very program that compiler emitted, so level 1 is m and
		// every deeper level is m's own parent chain -- the correspondence
		// aggResultReg's compile-time walk counted.
		pm.parent = m
	}
	return pm
}

// run seeds pm's registers from one batch entry and its window values, runs
// the program and returns the projected values. Registers are cleared first so
// no value from a previous row is ever readable.
func (p *windowProjection) run(pm *vdbe, entry, winVals []Value) ([]Value, error) {
	clear(pm.regs)
	// The batch entry's trailing rowid block, one slot per joined table
	// (compileScanWindow emits one OpRowid per source into it). Bounded by what
	// the entry actually carries, since a shorter entry must leave the tail
	// registers at the NULL the clear above put there.
	if n := min(p.nRowids, len(entry)-p.nCols); n > 0 {
		copy(pm.regs[p.rowidReg:p.rowidReg+n], entry[p.nCols:p.nCols+n])
	}
	if p.nCols > 0 {
		copy(pm.regs[p.colBase:p.colBase+p.nCols], entry)
	}
	// The entry's OPERAND tail, whose leading len(bufRegs) columns are the
	// references this projection buffered (planWindowOperands puts them
	// first). One at a time, not a block copy: these registers are allocated as
	// the compile discovers each reference, so they are not contiguous.
	// Bounded by the entry like the rowid block above.
	for i, r := range p.bufRegs {
		if j := p.nCols + p.nRowids + i; j < len(entry) {
			pm.regs[r] = entry[j]
		}
	}
	copy(pm.regs[p.winBase:p.winBase+p.nWin], winVals)
	if _, err := pm.run(p.prog.Insns); err != nil {
		return nil, err
	}
	out := make([]Value, len(p.resultReg))
	for i, r := range p.resultReg {
		out[i] = pm.regs[r]
	}
	return out, nil
}

// windowProjScopeOffers reports whether any of this query's FROM scopes offers
// x, by name, rowid, or join-group alias.
//
// It is used instead of resolveRowReg because resolveRowReg refuses a name two
// scopes offer, and that refusal must surface as "ambiguous column name"
// (resolve.c:785). Lifting on its answer would buffer the ambiguous reference
// into the scan body, which would pick a cursor and answer. So only names this
// FROM does not offer at all, which must belong to an enclosing query, are lifted.
func windowProjScopeOffers(scopes []tableScope, x ColumnExpr) bool {
	lname := r33sFoldIdent(x.Name)
	for i := range scopes {
		s := &scopes[i]
		if x.Qualifier != "" && !equalFoldName(x.Qualifier, s.name) && !equalFoldName(x.Qualifier, s.groupAlias) {
			continue
		}
		if _, ok := s.colIndex[lname]; ok {
			return true
		}
		if !s.noRowid && isRowidAliasName(lname) {
			return true
		}
	}
	return false
}

// windowBufCols buffers the outer-projection column references that the
// register block cannot serve because they name an enclosing query.
//
// It is selectWindowRewriteExprCb's TK_COLUMN case: C appends every outer
// column reference to the generated sub-select and rewrites it to read the
// ephemeral row (window.c:788-817); that sub-select runs inside the enclosing
// query (window.c:1068-1070) and is marked correlated (window.c:1080).
//
// musql already has this query's own columns in the register block, so only
// names no scope of its FROM offers are buffered, each as one more batch column
// compiled in the scan body (OpOuterColumn). Buffering per row is the same
// value: the enclosing row does not move while the window scan runs.
//
// Not buffered: a name the FROM does offer (even ambiguously -- see
// windowProjScopeOffers), and a bare TRUE/FALSE (FallbackLiteral).
//
// A subquery of the projection reaches this seam through compileColumn's
// outer-chain bufOut arm: names this FROM offers come from the register block
// (window.c:756-770), others are buffered. C reads those off the live outer
// cursor instead; this machine has none, and the value is the same.
//
// aff carries affinity/collation metadata for compiler.affCtx only, since the
// projection compile has no chain. Without it, over o(c TEXT COLLATE NOCASE),
//
//	SELECT (SELECT (o.c = 'APPLE') || count(*) OVER () FROM inr LIMIT 1) FROM o
//
// compared BINARY. C keeps both on the ephemeral column (select.c:2381,
// :2426-2429) and preserves explicit COLLATE (window.c:806, :818). It is not
// compiler.outer: an OpOuterColumn here would read exhausted cursors.
type windowBufCols struct {
	// scopes is the window query's own FROM, for the refusal above.
	scopes []tableScope
	// aff is the SCAN BODY's own scope chain (the window query's compile plus
	// its outer chain) -- where a buffered reference is actually compiled, so
	// the affinity and collation the projection sees are the ones that
	// reference really has.
	aff *evalCtx
	// exprs is the buffered references in batch operand-column order, and
	// regs[i] is the projection register column i is seeded into. Registers
	// are allocated as each reference is first seen, so they are NOT
	// contiguous -- windowProjection.run copies them one at a time.
	exprs []Expr
	regs  []int
}

// slot returns the register holding x's buffered value, claiming a new batch
// column for it the first time it is seen, or reports false when x is not a
// reference this seam may buffer at all (see the type's doc comment).
//
// A nil receiver -- every compile but the one projection -- answers false, so
// the call site in compileColumn needs no guard of its own.
func (b *windowBufCols) slot(c *compiler, x ColumnExpr) (int, bool) {
	if b == nil || x.FallbackLiteral != nil || windowProjScopeOffers(b.scopes, x) {
		return 0, false
	}
	for i, e := range b.exprs {
		// sqlite3ExprCompare's dedup ("for(i=0; i<p->pSub->nExpr; i++){ if(
		// 0==sqlite3ExprCompare(0, p->pSub->a[i].pExpr, pExpr, -1) ){ iCol = i;
		// break; } }", window.c:794-801), narrowed to what a ColumnExpr can be.
		// Two spellings this equality separates get a column each, which costs
		// one extra buffered read and can never be wrong.
		if prev, ok := e.(ColumnExpr); ok && prev == x {
			return b.regs[i], true
		}
	}
	r := c.alloc()
	b.exprs = append(b.exprs, x)
	b.regs = append(b.regs, r)
	return r, true
}

// slotPlaceholder is slot for an aggregate-result placeholder (groupAggExpr /
// groupKeyExpr / groupBareColExpr, sql_group.go) placed inside a window query by
// rewriteAggItemBodies. The projection cannot reach the item's register block,
// but the scan body can (aggResultReg, OpOuterAggReg), so the value is buffered
// per row and read back as a batch column -- the same lift C does
// (window.c:756-771, :802-819). The value is constant per group and the body
// re-runs per group. There is no "already offered" case for a placeholder.
func (b *windowBufCols) slotPlaceholder(c *compiler, e Expr) (int, bool) {
	if b == nil {
		return 0, false
	}
	for i, prev := range b.exprs {
		if aggPlaceholderSame(prev, e) {
			return b.regs[i], true
		}
	}
	r := c.alloc()
	b.exprs = append(b.exprs, e)
	b.regs = append(b.regs, r)
	return r, true
}

// aggPlaceholderSame is slotPlaceholder's dedup test -- sqlite3ExprCompare's
// role (window.c:794-801) narrowed to the three placeholder nodes. Written as a
// type switch rather than as interface equality because b.exprs also holds
// ColumnExpr entries and "==" on two interfaces of different dynamic types is
// merely false, but on a future non-comparable node kind would panic.
func aggPlaceholderSame(a, b Expr) bool {
	switch x := a.(type) {
	case groupAggExpr:
		y, ok := b.(groupAggExpr)
		return ok && x == y
	case groupKeyExpr:
		y, ok := b.(groupKeyExpr)
		return ok && x == y
	case groupBareColExpr:
		y, ok := b.(groupBareColExpr)
		return ok && x == y
	}
	return false
}
