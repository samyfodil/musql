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
