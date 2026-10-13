package engine

import "fmt"

// recursiveRefError is the error C raises for a recursive CTE's body, in the
// order it raises them:
//
//   - withExpand (select.c:5770-5850) binds the direct self-references of the
//     trailing run of UNION/UNION ALL arms (marking them SF_Recursive), then
//     walks the whole body again with zCteErr = "multiple recursive
//     references: %s": a reference anywhere else in a recursive arm -- a
//     subquery, an ON clause, a WINDOW definition, the compound's ORDER BY or
//     LIMIT, or the body of a CTE of the body's own WITH that such a place
//     names -- is that error. A reference in the non-recursive prefix is the
//     earlier walk's "circular reference", which the ordinary path already
//     gives.
//   - generateWithRecursiveQuery (select.c:2690, 2778-2782) then refuses a
//     window function in the recursive SELECT and an aggregate or GROUP BY in
//     any recursive arm.
//
// "" when C would raise none of these, or when a nested WITH redefines the
// name (the reference may then be to that one; left to ordinary resolution).
func recursiveRefError(name string, core *SelectStmt) string {
	if core == nil || len(core.Compound) == 0 {
		return ""
	}
	last := core.Compound[len(core.Compound)-1]
	if last.Op != "UNION" && last.Op != "UNION ALL" {
		return ""
	}
	first := len(core.Compound)
	for first > 0 && core.Compound[first-1].Op == last.Op && topFromReferences(core.Compound[first-1].Stmt, name) {
		first--
	}
	if first == len(core.Compound) {
		return ""
	}
	defs := map[string]*SelectStmt{}
	for _, d := range core.CTEs {
		if equalFoldName(d.Name, name) {
			return ""
		}
		defs[r33sFoldIdent(d.Name)] = d.Select
	}
	multiple := fmt.Sprintf("multiple recursive references: %s", name)
	for _, arm := range core.Compound[first:] {
		nested, shadowed := armNestedSelfRef(arm.Stmt, name, defs)
		if shadowed {
			return ""
		}
		if nested {
			return multiple
		}
	}
	// The compound's ORDER BY and LIMIT hang off its last SELECT in C, so they
	// are part of the recursive walk too.
	w := selfRefWalker{name: name, defs: defs}
	for _, o := range core.OrderBy {
		w.expr(o.Expr)
	}
	w.expr(core.LimitParam)
	w.expr(core.OffsetParam)
	if w.shadowed {
		return ""
	}
	if w.found {
		return multiple
	}
	if lastArm := last.Stmt; lastArm != nil && (selectHasWindow(lastArm) || orderByHasWindow(lastArm)) {
		return "cannot use window functions in recursive queries"
	}
	for i := len(core.Compound) - 1; i >= first; i-- {
		arm := core.Compound[i].Stmt
		if arm == nil {
			continue
		}
		agg := len(arm.GroupBy) > 0 || containsAggregate(arm.Having)
		for _, c := range arm.Columns {
			if !c.Star && containsAggregate(c.Expr) {
				agg = true
			}
		}
		if agg {
			return "recursive aggregate queries not supported"
		}
	}
	return ""
}

// armNestedSelfRef reports whether arm names the CTE name anywhere but as a
// plain item of its own top-level FROM, and whether some nested WITH
// redefines name (shadowed: the answer is then not trusted). defs are the
// CTEs of the body's own WITH, whose bodies a reference walks into.
func armNestedSelfRef(arm *SelectStmt, name string, defs map[string]*SelectStmt) (nested, shadowed bool) {
	w := selfRefWalker{name: name, defs: defs}
	if arm == nil {
		return false, false
	}
	for _, it := range arm.From {
		if it.Subquery != nil {
			w.selectRefs(it.Subquery)
		} else if !cteFromItemRefs(it, name) {
			w.cteRef(it)
		}
		w.expr(it.On)
		for _, a := range it.TableFuncArgs {
			w.expr(a)
		}
	}
	w.selectBody(arm)
	return w.found, w.shadowed
}

type selfRefWalker struct {
	name            string
	found, shadowed bool
	defs            map[string]*SelectStmt
	visited         map[string]bool
}

// cteRef follows a FROM item naming one of the body's own CTEs into its body.
func (w *selfRefWalker) cteRef(it FromItem) {
	if it.Subquery != nil || it.Schema != "" || w.found || w.shadowed {
		return
	}
	key := r33sFoldIdent(it.Table)
	body, ok := w.defs[key]
	if !ok || w.visited[key] {
		return
	}
	if w.visited == nil {
		w.visited = map[string]bool{}
	}
	w.visited[key] = true
	w.selectRefs(body)
}

// selectRefs walks a whole nested SELECT, its FROM included.
func (w *selfRefWalker) selectRefs(s *SelectStmt) {
	if s == nil || w.found || w.shadowed {
		return
	}
	for _, d := range s.CTEs {
		if equalFoldName(d.Name, w.name) {
			w.shadowed = true
			return
		}
		if _, ok := w.defs[r33sFoldIdent(d.Name)]; ok {
			// A nested WITH redefining one of the body's own CTEs: which body
			// a reference reaches is no longer the one in defs.
			w.shadowed = true
			return
		}
	}
	for _, d := range s.CTEs {
		w.selectRefs(d.Select)
	}
	for _, it := range s.From {
		if it.Subquery != nil {
			w.selectRefs(it.Subquery)
		} else if cteFromItemRefs(it, w.name) {
			w.found = true
			return
		} else {
			w.cteRef(it)
		}
		w.expr(it.On)
		for _, a := range it.TableFuncArgs {
			w.expr(a)
		}
	}
	w.selectBody(s)
	for _, arm := range s.Compound {
		w.selectRefs(arm.Stmt)
	}
}

// selectBody walks every expression of s except its FROM items.
func (w *selfRefWalker) selectBody(s *SelectStmt) {
	for _, c := range s.Columns {
		w.expr(c.Expr)
	}
	w.expr(s.Where)
	for _, g := range s.GroupBy {
		w.expr(g)
	}
	w.expr(s.Having)
	for _, o := range s.OrderBy {
		w.expr(o.Expr)
	}
	for _, nw := range s.Windows {
		w.window(nw.Spec)
	}
	w.expr(s.LimitParam)
	w.expr(s.OffsetParam)
}

func (w *selfRefWalker) window(spec *WindowSpec) {
	if spec == nil {
		return
	}
	for _, e := range spec.PartitionBy {
		w.expr(e)
	}
	for _, o := range spec.OrderBy {
		w.expr(o.Expr)
	}
}

func (w *selfRefWalker) expr(e Expr) {
	if e == nil || w.found || w.shadowed {
		return
	}
	switch x := e.(type) {
	case SubqueryExpr:
		w.selectRefs(x.Stmt)
		return
	case ExistsExpr:
		w.selectRefs(x.Stmt)
		return
	case InExpr:
		w.selectRefs(x.Sub)
	case FuncExpr:
		w.expr(x.Filter)
		w.window(x.Over)
	case RaiseExpr:
		w.expr(x.Msg)
		return
	}
	walkExprOperands(e, w.expr)
}
