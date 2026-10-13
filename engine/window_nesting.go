package engine

// windowCallInside returns the first window-function call written inside the
// arguments of fc when fc is an aggregate or itself a window call, or inside
// that window call's FILTER, PARTITION BY or ORDER BY.
func windowCallInside(fc FuncExpr) (FuncExpr, bool) {
	var found FuncExpr
	var ok bool
	look := func(e Expr) {
		if ok || e == nil {
			return
		}
		walkExprShallow(e, func(f FuncExpr) bool {
			if f.Over != nil {
				found, ok = f, true
			}
			return !ok
		})
	}
	if fc.Over == nil && !isAggregateCall(fc) {
		return found, false
	}
	for _, a := range fc.Args {
		look(a)
	}
	if fc.Over != nil {
		look(fc.Filter)
		for _, p := range fc.Over.PartitionBy {
			look(p)
		}
		for _, o := range fc.Over.OrderBy {
			look(o.Expr)
		}
	}
	return found, ok
}

// windowMisuseErr is resolve.c's "misuse of window function X()" for a SELECT:
// a window call written inside the arguments, FILTER, PARTITION BY or ORDER BY
// of an aggregate or another window call (resolve.c:1265-1277), or anywhere in
// the WHERE, GROUP BY or HAVING clause. Raised ahead of the compile paths, whose
// own declines for the same shapes would otherwise word it differently.
func windowMisuseErr(stmt *SelectStmt) error {
	var err error
	firstWindow := func(e Expr) (FuncExpr, bool) {
		var found FuncExpr
		var ok bool
		walkExprShallow(e, func(f FuncExpr) bool {
			if f.Over != nil {
				found, ok = f, true
			}
			return !ok
		})
		return found, ok
	}
	// rowInCase reports a row value as a CASE result inside e: a row value is
	// legal only as an operand of a comparison, so C refuses it where the CASE
	// produces a scalar.
	var rowInCase func(e Expr) bool
	rowInCase = func(e Expr) bool {
		found := false
		var walk func(Expr)
		walk = func(x Expr) {
			if x == nil || found {
				return
			}
			if ce, ok := x.(CaseExpr); ok {
				for _, w := range ce.Whens {
					if _, r := w.Then.(RowExpr); r {
						found = true
					}
				}
				if _, r := ce.Else.(RowExpr); r {
					found = true
				}
			}
			wherePlanWalkChildren(x, walk)
		}
		walk(e)
		return found
	}
	nested := func(e Expr) {
		if err != nil || e == nil {
			return
		}
		walkExprShallow(e, func(fc FuncExpr) bool {
			if inner, bad := windowCallInside(fc); bad {
				err = semanticf("engine: misuse of window function %s()", inner.callName())
			} else if fc.Over != nil {
				for _, a := range fc.Args {
					if rowInCase(a) {
						err = semanticf("engine: row value misused")
					}
				}
			}
			return err == nil
		})
	}
	forbidden := func(e Expr) {
		if err != nil || e == nil {
			return
		}
		if w, ok := firstWindow(e); ok {
			err = semanticf("engine: misuse of window function %s()", w.callName())
		}
	}
	for _, c := range stmt.Columns {
		if !c.Star {
			nested(c.Expr)
		}
	}
	for _, o := range stmt.OrderBy {
		nested(o.Expr)
	}
	forbidden(stmt.Where)
	for _, g := range stmt.GroupBy {
		forbidden(g)
	}
	forbidden(stmt.Having)
	return err
}
