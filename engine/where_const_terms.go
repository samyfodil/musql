package engine

// Constant WHERE terms are coded once, before the loops open. sqlite3WhereBegin
// (where.c, the loop over sWLB.pWC just before WHERE_WANT_DISTINCT is handled)
// does it for every term whose prereqAll is 0 and that has no non-deterministic
// function outside a sub-select, with sqlite3ExprIfFalse jumping to iBreak. The
// term is therefore evaluated even when the table is empty -- so an error it
// raises (abs() of the smallest integer) is raised then -- and a false one skips
// the scan outright.
//
// This port hoists the terms it can prove constant and leaves every other one
// where it was, per row. Narrower than C, never wider: a sub-select, a MATCH, a
// column it cannot prove belongs to an enclosing query, a function outside
// constWhereFuncs all stay in the loop.

// constWhereFuncs are the built-in scalar functions registered with
// SQLITE_FUNC_CONSTANT (func.c, date.c's PURE_DATE), which is what the walker
// behind exprIsDeterministic looks for (EP_ConstFunc). The ones C registers
// without it -- random, the connection-state functions, sqlite_version, the
// date/time keywords -- are absent on purpose: they run once per row.
var constWhereFuncs = map[string]bool{
	"abs": true, "coalesce": true, "ifnull": true, "nullif": true, "iif": true, "if": true,
	"length": true, "octet_length": true, "lower": true, "upper": true, "hex": true,
	"unhex": true, "quote": true, "typeof": true, "substr": true, "substring": true,
	"round": true, "replace": true, "instr": true, "trim": true, "ltrim": true,
	"rtrim": true, "printf": true, "format": true, "char": true, "unicode": true,
	"zeroblob": true, "min": true, "max": true, "sign": true, "concat": true,
	"concat_ws": true, "likely": true, "unlikely": true, "likelihood": true,
	"like": true, "glob": true, "soundex": true,
	"ceil": true, "ceiling": true, "floor": true, "trunc": true, "ln": true,
	"log": true, "log10": true, "log2": true, "exp": true, "pow": true, "power": true,
	"mod": true, "sqrt": true, "sin": true, "cos": true, "tan": true, "asin": true,
	"acos": true, "atan": true, "atan2": true, "sinh": true, "cosh": true, "tanh": true,
	"asinh": true, "acosh": true, "atanh": true, "degrees": true, "radians": true,
	"pi": true,
}

// constWhereTerm reports whether conjunct e may be coded before the loops, and
// whether it reads a column of an enclosing query to be so (outer). noColumns
// forbids column references altogether (a join group's scopes are not one per
// source, so an outer-only test cannot be trusted there).
func constWhereTerm(e Expr, scopes []tableScope, noColumns bool) (hoist, outer bool) {
	ok, outer, sub := constTermWalk(e, scopes, noColumns, false)
	return ok && !sub, outer && ok
}

// levelZeroConstTerm reports whether e holds a sub-select, each of them a
// FROM-less one over constants, and nothing else that names a table of this
// FROM clause. C does not hoist such a term (exprIsDeterministic refuses any
// sub-select), but its prereqAll is 0, so where.c codes it inside the first
// loop, whatever the planner puts there, rather than at the innermost one.
func levelZeroConstTerm(e Expr, scopes []tableScope, noColumns bool) bool {
	ok, _, sub := constTermWalk(e, scopes, noColumns, true)
	return ok && sub
}

// constSubTerm reports whether every sub-select in e is a FROM-less one over
// constants, whatever else e names: such a sub-select uses no table
// (prereqAll 0, no TERM_VARSELECT), so where.c codes the term at the level of
// the tables it names outside them, like any other.
func constSubTerm(e Expr, scopes []tableScope) bool {
	ok, _, sub := constTermWalkCols(e, scopes, false, true, true)
	return ok && sub
}

func constTermWalk(e Expr, scopes []tableScope, noColumns, allowSub bool) (hoist, outer, sawSub bool) {
	return constTermWalkCols(e, scopes, noColumns, allowSub, false)
}

// constTermWalkCols is constTermWalk; anyColumn accepts every column outside a
// sub-select.
func constTermWalkCols(e Expr, scopes []tableScope, noColumns, allowSub, anyColumn bool) (hoist, outer, sawSub bool) {
	inSub := 0
	var ok func(Expr) bool
	ok = func(e Expr) bool {
		switch x := e.(type) {
		case nil:
			return true
		case LiteralExpr, ParamExpr:
			return true
		case SubqueryExpr:
			sawSub = true
			inSub++
			defer func() { inSub-- }()
			return allowSub && constSelect(x.Stmt, ok)
		case ExistsExpr:
			sawSub = true
			inSub++
			defer func() { inSub-- }()
			return allowSub && constSelect(x.Stmt, ok)
		case ColumnExpr:
			if anyColumn {
				return inSub == 0
			}
			// "X IS TRUE" carries the keyword as a name (desugarIsBool); with no
			// column of that name in scope it is the constant, not an outer reference.
			if kw := r33sFoldIdent(x.Name); x.Qualifier == "" && (kw == "true" || kw == "false") && !noColumns && columnIsOuterOnly(x, scopes) {
				return true
			}
			outer = true
			return !noColumns && columnIsOuterOnly(x, scopes)
		case UnaryExpr:
			return ok(x.X)
		case BinaryExpr:
			return ok(x.L) && ok(x.R)
		case IsNullExpr:
			return ok(x.X)
		case InExpr:
			if x.Sub != nil || !ok(x.X) {
				return false
			}
			for _, a := range x.List {
				if !ok(a) {
					return false
				}
			}
			return true
		case BetweenExpr:
			return ok(x.X) && ok(x.Lo) && ok(x.Hi)
		case LikeExpr:
			return ok(x.X) && ok(x.Pattern) && ok(x.Escape)
		case GlobExpr:
			return ok(x.X) && ok(x.Pattern)
		case CollateExpr:
			return ok(x.X)
		case CastExpr:
			return ok(x.X)
		case CaseExpr:
			if !ok(x.Base) || !ok(x.Else) {
				return false
			}
			for _, w := range x.Whens {
				if !ok(w.When) || !ok(w.Then) {
					return false
				}
			}
			return true
		case FuncExpr:
			if x.Over != nil || x.Filter != nil || x.Star || x.Distinct {
				return false
			}
			name := r33sFoldIdent(x.Name)
			if f := udfs[name]; f != nil {
				if !f.Deterministic {
					return false
				}
			} else if !constWhereFuncs[name] || nonConstantFuncs[name] {
				return false
			}
			for _, a := range x.Args {
				if !ok(a) {
					return false
				}
			}
			return true
		}
		return false
	}
	hoist = ok(e)
	return hoist, outer && hoist, sawSub
}

// constSelect reports whether sel is a FROM-less SELECT of one row whose every
// expression ok accepts.
func constSelect(sel *SelectStmt, ok func(Expr) bool) bool {
	if sel == nil || len(sel.From) != 0 || len(sel.CTEs) != 0 || len(sel.Compound) != 0 ||
		len(sel.GroupBy) != 0 || sel.Having != nil || len(sel.OrderBy) != 0 ||
		sel.Limit != nil || sel.Offset != nil || sel.LimitParam != nil || sel.OffsetParam != nil || sel.Distinct {
		return false
	}
	if !ok(sel.Where) {
		return false
	}
	for _, c := range sel.Columns {
		if c.Star || !ok(c.Expr) {
			return false
		}
	}
	return true
}

// columnIsOuterOnly reports whether x cannot name a column or rowid of any
// scope in this FROM clause, i.e. it can only be a reference to an enclosing
// query (or an error compileExpr reports either way).
func columnIsOuterOnly(x ColumnExpr, scopes []tableScope) bool {
	if x.UsingPinned || x.UsingRepr {
		return false
	}
	lname := r33sFoldIdent(x.Name)
	for _, ts := range scopes {
		if ts.fromExists || ts.name == "" {
			return false
		}
		if x.Qualifier != "" {
			if equalFoldName(ts.name, x.Qualifier) || equalFoldName(ts.groupAlias, x.Qualifier) {
				return false
			}
			continue
		}
		switch lname {
		case "rowid", "oid", "_rowid_":
			return false
		}
		if _, ok := ts.colIndex[lname]; ok {
			return false
		}
		if _, ok := ts.coalesced[lname]; ok {
			return false
		}
		if equalFoldName(ts.name, x.Name) { // "tab MATCH q" names the table itself
			return false
		}
	}
	return true
}

// splitConstWhereTerms separates the conjuncts coded before the loops from the
// rest, keeping each group's order.
func splitConstWhereTerms(cjs []Expr, scopes []tableScope, grouped bool) (consts, outerConsts, rest []Expr) {
	for _, cj := range cjs {
		switch hoist, outer := constWhereTerm(cj, scopes, grouped); {
		case hoist && outer:
			outerConsts = append(outerConsts, cj)
		case hoist:
			consts = append(consts, cj)
		default:
			rest = append(rest, cj)
		}
	}
	return consts, outerConsts, rest
}

// exprAlwaysTrue and exprAlwaysFalse are ExprAlwaysTrue/ExprAlwaysFalse
// (sqliteInt.h:3154) for what the parser flags EP_IsTrue/EP_IsFalse: a
// 32-bit integer literal (parse.y:1200, sqlite3ExprInt32) and TRUE/FALSE.
func exprAlwaysTrue(e Expr) bool {
	l, ok := e.(LiteralExpr)
	return ok && !l.materialized && l.Val.Typ == Int && l.Val.I > 0 && l.Val.I <= 1<<31-1
}

func exprAlwaysFalse(e Expr) bool {
	l, ok := e.(LiteralExpr)
	return ok && !l.materialized && l.Val.Typ == Int && l.Val.I == 0
}

// simplifiedAndOr is sqlite3ExprSimplifiedAndOr (expr.c:2373), which the
// expression, IfTrue and IfFalse coders all run first: "x OR true" is true and
// "x AND false" is false, so x is never evaluated -- an error it would raise
// never is. changed is false when pExpr is returned as it came.
func simplifiedAndOr(e Expr) (alt Expr, changed bool) {
	b, ok := e.(BinaryExpr)
	if !ok || (b.Op != "AND" && b.Op != "OR") {
		return e, false
	}
	r, _ := simplifiedAndOr(b.R)
	l, _ := simplifiedAndOr(b.L)
	switch {
	case exprAlwaysTrue(l) || exprAlwaysFalse(r):
		if b.Op == "AND" {
			return r, true
		}
		return l, true
	case exprAlwaysTrue(r) || exprAlwaysFalse(l):
		if b.Op == "AND" {
			return l, true
		}
		return r, true
	}
	return e, false
}

// exprIsBoolean reports whether e always evaluates to 0, 1 or NULL.
func exprIsBoolean(e Expr) bool {
	switch x := e.(type) {
	case BinaryExpr:
		switch x.Op {
		case "<", "<=", ">", ">=", "=", "==", "!=", "<>", "AND", "OR", "IS", "IS NOT":
			return true
		}
	case IsNullExpr, InExpr, BetweenExpr, LikeExpr, GlobExpr, ExistsExpr:
		return true
	case UnaryExpr:
		return x.Op == "NOT"
	}
	return false
}

// emitWriteConstTerms codes an UPDATE's or DELETE's constant WHERE terms before
// its scan opens (the same sqlite3WhereBegin step) and returns the conjuncts
// left for the loop. Each jump is for the caller to patch to the end of the
// loop, where an empty table's Rewind goes.
func (c *compiler) emitWriteConstTerms(where Expr) (loopWhere Expr, jumps []int, err error) {
	if where == nil {
		return nil, nil, nil
	}
	scopes := make([]tableScope, len(c.scopes))
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
	}
	consts, outerConsts, rest := splitConstWhereTerms(splitTopLevelAnd(where), scopes, false)
	for _, cj := range append(consts, outerConsts...) {
		c.inWhereConjunct = true
		reg, cerr := c.compileExpr(cj)
		if cerr != nil {
			return nil, nil, cerr
		}
		jumps = append(jumps, c.emit(Instruction{Op: OpIfNot, P1: reg, P3: 1}))
	}
	return andConjuncts(rest), jumps, nil
}

// existsJoinable is the part of existsToJoin's test (select.c:7326) that the
// EXISTS's own select decides: one plain table, no aggregate, no LIMIT, no
// compound. Whether the EXISTS is a top-level AND term of a SELECT's WHERE is
// the caller's.
func existsJoinable(sel *SelectStmt) bool {
	return len(sel.From) == 1 && sel.From[0].Subquery == nil && !sel.From[0].TableFunc &&
		len(sel.Compound) == 0 && sel.Limit == nil && sel.LimitParam == nil &&
		sel.Offset == nil && sel.OffsetParam == nil && !selectIsAggregateQuery(sel)
}

// isExistsBody reports whether stmt is the select of the EXISTS this compiler
// is the body of. selectInnerLoop codes no result list for an SRT_Exists
// destination (select.c, the "eDest!=SRT_Exists" arm), including an aggregate
// query's, whose aggregates still step and whose HAVING still runs.
func (c *compiler) isExistsBody(stmt *SelectStmt) bool {
	return c.outer != nil && c.outer.existsStmt == stmt
}

// existsJoinPos is where existsToJoin (select.c) puts the table of one EXISTS
// in the enclosing SELECT's join: level is its nesting level, and levels[i] is
// that of the enclosing FROM clause's source i, whose scopes are scopes.
//
// This port runs the EXISTS as a subquery per enclosing row, so a term of its
// WHERE that where.c would code at an enclosing level -- one naming only
// enclosing tables, or a FROM-less sub-select (prereqAll 0, coded at the
// outermost level) -- is tested before the subquery's loop when that level is
// outside the EXISTS table's, and inside it otherwise.
type existsJoinPos struct {
	level  int
	scopes []tableScope
	levels []int

	// src is the EXISTS table as the planned join holds it, when its loop is
	// an inner full scan or an automatic index: an empty table there ends
	// the whole join the first time its level is reached (addrHalt, as for
	// any inner table -- constructAutomaticIndex's Rewind leaves the same
	// way). cursor is the enclosing program's own read of it, opened by
	// emitJoinLoops to tell; -1 until then.
	src          *joinSource
	cursor, flag int
}

// before reports whether conjunct cj of the EXISTS's WHERE is coded at a level
// outside the EXISTS table's.
func (p *existsJoinPos) before(cj Expr) bool {
	if p == nil {
		return false
	}
	at := 0
	refs := map[int]bool{}
	collectTableRefs(cj, p.scopes, refs)
	for r := range refs {
		if r < len(p.levels) && p.levels[r] > at {
			at = p.levels[r]
		}
	}
	return at < p.level
}

// planExistsJoinTerms records, for the SELECT c compiles over srcs, what
// existsToJoin does to each EXISTS it converts: where its table sits in the
// join (existsJoinPos, from the planned order, or else from the rule that keeps
// it right of every table its terms name, where.c's fromExists prerequisites),
// and the constant terms of its WHERE, which sqlite3WhereBegin then codes
// before the enclosing loops open like any other: an EXISTS over a table the
// enclosing query never reaches still raises their error.
func planExistsJoinTerms(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) {
	c.existsPos, c.existsHoist = nil, nil
	var subs []*SelectStmt
	var levelOf []int
	if ej := c.existsJoinPlan; ej != nil {
		subs = ej.exists
		levelOf = make([]int, len(ej.srcs))
		for level, iTab := range ej.order {
			if iTab < len(levelOf) {
				levelOf[iTab] = level
			}
		}
	} else {
		subs = existsToJoinSubs(p, c, srcs, stmt)
	}
	if len(subs) == 0 {
		return
	}
	orig := tableScopesOf(srcs)
	c.existsPos = make(map[*SelectStmt]*existsJoinPos, len(subs))
	for k, sub := range subs {
		cjs := splitTopLevelAnd(sub.Where)
		pos := &existsJoinPos{scopes: orig}
		pos.cursor = -1
		if levelOf != nil {
			pos.level, pos.levels = levelOf[len(srcs)+k], levelOf[:len(srcs)]
			ej := c.existsJoinPlan
			var key *autoIndexKey
			if pos.level < len(ej.keys) {
				key = ej.keys[pos.level]
			}
			if x := &ej.srcs[len(srcs)+k]; pos.level > 0 && x.tbl != nil && x.derived == nil && x.vtabItem == nil &&
				x.cteItem == nil && (key == nil || key.root == 0 && key.multiOr == nil) {
				pos.src = x
			}
		} else {
			// No plan: the enclosing tables in FROM order, and the EXISTS
			// table outermost unless a term naming it also names one of them.
			pos.levels = make([]int, len(srcs))
			for i := range pos.levels {
				pos.levels[i] = i
			}
			for _, cj := range cjs {
				if hoist, _ := constWhereTerm(cj, orig, false); hoist {
					continue // names no column of the EXISTS table
				}
				refs := map[int]bool{}
				collectTableRefs(cj, orig, refs)
				if len(refs) > 0 {
					pos.level = len(srcs) + k
					break
				}
			}
		}
		c.existsPos[sub] = pos
		for _, cj := range cjs {
			if hoist, outer := constWhereTerm(cj, orig, false); hoist && !outer {
				c.existsHoist = append(c.existsHoist, cj)
			}
		}
	}
}

// existsToJoinSubs is the EXISTS selects existsToJoin converts in stmt, by the
// test wherePlanExistsToJoin applies, for when the planner could not follow
// the rewrite.
func existsToJoinSubs(p *ReadOnlyPager, c *compiler, srcs []joinSource, stmt *SelectStmt) []*SelectStmt {
	if p == nil || c == nil || c.onePassPlan || stmt == nil || stmt.Where == nil ||
		stmt.Offset != nil || stmt.OffsetParam != nil {
		return nil
	}
	var subs []*SelectStmt
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case BinaryExpr:
			if equalFoldName(x.Op, "AND") {
				walk(x.L)
				walk(x.R)
			}
		case ExistsExpr:
			if !x.Not && existsToJoinCandidate(x.Stmt) {
				subs = append(subs, x.Stmt)
			}
		}
	}
	walk(stmt.Where)
	if len(srcs)+len(subs) >= 64 {
		return nil
	}
	return subs
}
