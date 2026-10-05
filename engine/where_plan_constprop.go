package engine

// Package engine implements WHERE-clause constant propagation. Top-level
// "COLUMN = CONSTANT" terms tag other occurrences as fixed columns. The planner
// reads fixed columns as constants, allowing join reordering.

// whereFixedCol is an EP_FixedCol column: col, fixed to val.
type whereFixedCol struct {
	col ColumnExpr
	val Expr
}

func (whereFixedCol) exprNode() {}

// wherePlanPropagateConstants returns WHERE and ON clauses with constants
// propagated, as the planner reads them.
func wherePlanPropagateConstants(jts []joinedTable, scopes []tableScope, where Expr) (Expr, []joinedTable, bool) {
	type site struct {
		e   Expr
		on  int // -1 for the WHERE clause, else the jts index whose ON holds it
		off bool
	}
	var sites []site
	for _, cj := range whereSplitTopAnd(where) {
		sites = append(sites, site{e: cj, on: -1})
	}
	for i := range jts {
		for _, cj := range whereSplitTopAnd(jts[i].on) {
			sites = append(sites, site{e: cj, on: i, off: !jts[i].onToWhere})
		}
	}
	if len(sites) < 2 { // "p->pWhere->op==TK_AND"
		return where, jts, true
	}
	type konst struct {
		tab, col int
		val      Expr
		aff      affinity
		site     int
		right    bool // the defining column is the term's RIGHT operand
	}
	ok := true
	for {
		// findConstInWhere: the AND tree right to left.
		var consts []konst
		hasAffBlob := false
		for si := len(sites) - 1; si >= 0; si-- {
			if sites[si].off {
				continue
			}
			be, isBin := sites[si].e.(BinaryExpr)
			if !isBin || !(be.Op == "=" || be.Op == "==") {
				continue
			}
			try := func(colE, valE Expr, right bool) {
				ce, isCol := colE.(ColumnExpr)
				if !isCol {
					return
				}
				tab, col, found := wherePlanColumnRef(jts, scopes, ce)
				if !found {
					return
				}
				isConst, sure := wherePlanIsConstant(valE)
				if !sure {
					ok = false
				}
				if !isConst {
					return
				}
				// constInsert: a value with no affinity, a BINARY comparison,
				// and not a column already present.
				if _, has := whereStaticAffinityOf(jts, scopes, valE); has || whereConstHasAffinity(valE) {
					return
				}
				if !equalFoldName(whereCompareCollation(jts, scopes, be.L, be.R), "BINARY") {
					return
				}
				for _, k := range consts {
					if k.tab == tab && k.col == col {
						return
					}
				}
				aff := affInteger
				if col >= 0 {
					aff = jts[tab].tbl.cols[col].Aff
				}
				if !isNumericAffinity(aff) && aff != affText {
					hasAffBlob = true // "<= SQLITE_AFF_BLOB"
				}
				consts = append(consts, konst{tab: tab, col: col, val: valE, aff: aff, site: si, right: right})
			}
			try(be.R, be.L, true)
			try(be.L, be.R, false)
		}
		if len(consts) == 0 || !ok {
			break
		}
		changed := false
		// propagateConstantExprRewriteOne.
		rewriteOne := func(e Expr, si int, right, top, ignoreBlob bool) Expr {
			ce, isCol := e.(ColumnExpr)
			if !isCol {
				return e
			}
			tab, col, found := wherePlanColumnRef(jts, scopes, ce)
			if !found {
				return e
			}
			for _, k := range consts {
				if top && k.site == si && k.right == right {
					continue // "if( pColumn==pExpr ) continue;"
				}
				if k.tab != tab || k.col != col {
					continue
				}
				if ignoreBlob && !isNumericAffinity(k.aff) && k.aff != affText {
					break
				}
				changed = true
				return whereFixedCol{col: ce, val: k.val}
			}
			return e
		}
		// propagateConstantExprRewrite, over one site: pre-order, never into a
		// subquery. isRoot is the site's own comparison, whose direct operand
		// can be the defining column itself.
		var walk func(e Expr, si int, isRoot bool) Expr
		child := func(e Expr, si int, right, isRoot bool) Expr {
			if ce, isCol := e.(ColumnExpr); isCol {
				return rewriteOne(ce, si, right, isRoot, hasAffBlob)
			}
			return walk(e, si, false)
		}
		walk = func(e Expr, si int, isRoot bool) Expr {
			switch x := e.(type) {
			case ColumnExpr:
				return rewriteOne(x, si, false, false, hasAffBlob)
			case BinaryExpr:
				if hasAffBlob && whereConstCmpOp(x.Op) {
					x.L = rewriteOne(x.L, si, false, isRoot, false)
					if a, _ := whereStaticAffinityOf(jts, scopes, x.L); a != affText {
						x.R = rewriteOne(x.R, si, true, isRoot, false)
					}
				}
				x.L = child(x.L, si, false, isRoot)
				x.R = child(x.R, si, true, isRoot)
				if x.row != nil {
					// The comparison the planner reads for a desugared row
					// value (where_plan_rowvalue.go). C's walk meets its
					// TK_VECTOR operands, which no rewrite arm names, and
					// descends to each element as an ordinary node --
					// bIgnoreAffBlob and all -- but never into an IN's VALUES
					// rows (withOperands).
					x.row = x.row.withOperands(func(el Expr) Expr { return walk(el, si, false) })
				}
				return x
			case UnaryExpr:
				x.X = walk(x.X, si, false)
				return x
			case CollateExpr:
				x.X = walk(x.X, si, false)
				return x
			case CastExpr:
				x.X = walk(x.X, si, false)
				return x
			case IsNullExpr:
				x.X = walk(x.X, si, false)
				return x
			case BetweenExpr:
				x.X, x.Lo, x.Hi = walk(x.X, si, false), walk(x.Lo, si, false), walk(x.Hi, si, false)
				return x
			case LikeExpr:
				x.X, x.Pattern, x.Escape = walk(x.X, si, false), walk(x.Pattern, si, false), walk(x.Escape, si, false)
				return x
			case GlobExpr:
				x.X, x.Pattern = walk(x.X, si, false), walk(x.Pattern, si, false)
				return x
			case InExpr:
				x.X = walk(x.X, si, false)
				list := make([]Expr, len(x.List))
				for i, a := range x.List {
					list[i] = walk(a, si, false)
				}
				x.List = list
				return x
			case CaseExpr:
				x.Base, x.Else = walk(x.Base, si, false), walk(x.Else, si, false)
				whens := make([]WhenClause, len(x.Whens))
				for i, w := range x.Whens {
					whens[i] = WhenClause{When: walk(w.When, si, false), Then: walk(w.Then, si, false)}
				}
				x.Whens = whens
				return x
			case FuncExpr:
				if x.Over != nil || x.Filter != nil {
					ok = false
					return x
				}
				args := make([]Expr, len(x.Args))
				for i, a := range x.Args {
					args[i] = walk(a, si, false)
				}
				x.Args = args
				return x
			case nil, LiteralExpr, ParamExpr, whereFixedCol, SubqueryExpr, ExistsExpr:
				// sqlite3SelectWalkNoop: the walk does not enter a subquery.
				return e
			}
			ok = false // a node this does not walk could hold a fixable column
			return e
		}
		for si := range sites {
			if !sites[si].off {
				sites[si].e = walk(sites[si].e, si, true)
			}
		}
		if !changed || !ok {
			break
		}
	}
	if !ok {
		return nil, nil, false
	}
	var w Expr
	out := append([]joinedTable(nil), jts...)
	ons := make([]Expr, len(jts))
	for _, s := range sites {
		if s.on < 0 {
			w = whereAndOf(w, s.e)
		} else {
			ons[s.on] = whereAndOf(ons[s.on], s.e)
		}
	}
	for i := range out {
		if out[i].on != nil {
			out[i].on = ons[i]
		}
	}
	return w, out, true
}

func whereAndOf(a, b Expr) Expr {
	if a == nil {
		return b
	}
	return BinaryExpr{Op: "AND", L: a, R: b}
}

// whereConstCmpOp is "pExpr->op>=TK_EQ && pExpr->op<=TK_GE || pExpr->op==TK_IS".
func whereConstCmpOp(op string) bool {
	switch op {
	case "=", "==", "<", "<=", ">", ">=":
		return true
	}
	return equalFoldName(op, "IS")
}

// whereConstHasAffinity is sqlite3ExprAffinity(pValue)!=0 for the constant
// shapes wherePlanIsConstant admits: a CAST carries one; a literal, a parameter
// and arithmetic over them do not. A fixed column carries its column's.
func whereConstHasAffinity(e Expr) bool {
	switch x := e.(type) {
	case CastExpr, whereFixedCol:
		return true
	case CollateExpr:
		return whereConstHasAffinity(x.X)
	}
	return false
}

// wherePlanIsConstant is sqlite3ExprIsConstant, as exprIsConstantValue
// (sql_parser.go) ports it, over a tree that may also hold fixed columns --
// constant to exprNodeIsConstant (expr.c:2582). sure false for a node neither
// classifies, which the caller declines on rather than guess.
func wherePlanIsConstant(e Expr) (isConst, sure bool) {
	switch x := e.(type) {
	case FuncExpr:
		if x.Star || x.Distinct || x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 ||
			isAggregateFuncName(x.Name) || nonConstantFuncs[r33sFoldIdent(x.Name)] {
			return false, true
		}
		isConst, sure = true, true
		for _, a := range x.Args {
			c, s := wherePlanIsConstant(a)
			isConst, sure = isConst && c, sure && s
		}
		return isConst, sure
	case CaseExpr:
		isConst, sure = true, true
		for _, sub := range append([]Expr{x.Base, x.Else}, whenExprs(x.Whens)...) {
			if sub == nil {
				continue
			}
			c, s := wherePlanIsConstant(sub)
			isConst, sure = isConst && c, sure && s
		}
		return isConst, sure
	case LiteralExpr, ParamExpr, whereFixedCol:
		return true, true
	case UnaryExpr:
		return wherePlanIsConstant(x.X)
	case CollateExpr:
		return wherePlanIsConstant(x.X)
	case CastExpr:
		return wherePlanIsConstant(x.X)
	case BinaryExpr:
		if equalFoldName(x.Op, "AND") || equalFoldName(x.Op, "OR") {
			return false, false
		}
		l, ls := wherePlanIsConstant(x.L)
		r, rs := wherePlanIsConstant(x.R)
		return l && r, ls && rs
	case ColumnExpr, SubqueryExpr, ExistsExpr:
		return false, true
	}
	return false, false
}

func whenExprs(ws []WhenClause) []Expr {
	out := make([]Expr, 0, 2*len(ws))
	for _, w := range ws {
		out = append(out, w.When, w.Then)
	}
	return out
}
