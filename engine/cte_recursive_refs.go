package engine

import "fmt"

// recursiveRefError is the error withExpand (select.c:5770-5850) raises for a
// recursive CTE's body when its recursive arms name the CTE somewhere other
// than their own top-level FROM: C binds the direct references first (marking
// the trailing run of UNION/UNION ALL arms that hold one SF_Recursive), then
// walks the whole body again with zCteErr = "multiple recursive references:
// %s", so a reference in a subquery, an ON clause or a WINDOW definition of a
// recursive arm is that error. A reference in the non-recursive prefix is the
// earlier walk's "circular reference", which the ordinary path already gives.
//
// "" when C would not raise it here, or when a nested WITH redefines the name
// (the reference may then be to that one; left to ordinary resolution).
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
	for _, arm := range core.Compound[first:] {
		nested, shadowed := armNestedSelfRef(arm.Stmt, name)
		if shadowed {
			return ""
		}
		if nested {
			return fmt.Sprintf("multiple recursive references: %s", name)
		}
	}
	// The compound's ORDER BY and LIMIT hang off its last SELECT in C, so they
	// are part of the recursive walk too.
	w := selfRefWalker{name: name}
	for _, o := range core.OrderBy {
		w.expr(o.Expr)
	}
	w.expr(core.LimitParam)
	w.expr(core.OffsetParam)
	if w.found && !w.shadowed {
		return fmt.Sprintf("multiple recursive references: %s", name)
	}
	return ""
}

// armNestedSelfRef reports whether arm names the CTE name anywhere but as a
// plain item of its own top-level FROM, and whether some nested WITH
// redefines name (shadowed: the answer is then not trusted).
func armNestedSelfRef(arm *SelectStmt, name string) (nested, shadowed bool) {
	w := selfRefWalker{name: name}
	if arm == nil {
		return false, false
	}
	for _, it := range arm.From {
		if it.Subquery != nil {
			w.selectRefs(it.Subquery)
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
