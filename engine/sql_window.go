// Shape detection for SQL WINDOW functions ("fn(...) OVER (...)").
// Window functions are implemented in vdbe_window.go.
package engine

import (
	"fmt"
)

// windowResultExpr is a placeholder for a window function call's precomputed
// result, standing for that row's value for the idx'th window function.
type windowResultExpr struct{ idx int }

func (windowResultExpr) exprNode() {}

// selectHasWindow reports whether stmt's select list contains any window
// function call ("... OVER ...").
func selectHasWindow(stmt *SelectStmt) bool {
	for _, c := range stmt.Columns {
		if !c.Star && exprHasWindow(c.Expr) {
			return true
		}
	}
	return false
}

// orderByHasWindow reports whether any of stmt's ORDER BY terms contains a
// window function call ("... OVER ...").
func orderByHasWindow(stmt *SelectStmt) bool {
	for _, ot := range stmt.OrderBy {
		if exprHasWindow(ot.Expr) {
			return true
		}
	}
	return false
}

// exprHasWindow reports whether e's tree contains a window function call.
func exprHasWindow(e Expr) bool {
	found := false
	walkExprShallow(e, func(fc FuncExpr) bool {
		if fc.Over != nil {
			found = true
		}
		return !found
	})
	return found
}

// walkExprShallow visits every FuncExpr node in e in left-to-right order,
// WITHOUT descending into a window function's own OVER spec (partition/order
// exprs) -- those are handled separately by the planner. It does descend into
// a FuncExpr's ordinary arguments. visit returns false to stop the walk early.
func walkExprShallow(e Expr, visit func(FuncExpr) bool) bool {
	switch x := e.(type) {
	case nil:
		return true
	case FuncExpr:
		if !visit(x) {
			return false
		}
		for _, a := range x.walkArgs() {
			if !walkExprShallow(a, visit) {
				return false
			}
		}
		return true
	case UnaryExpr:
		return walkExprShallow(x.X, visit)
	case BinaryExpr:
		return walkExprShallow(x.L, visit) && walkExprShallow(x.R, visit)
	case IsNullExpr:
		return walkExprShallow(x.X, visit)
	case InExpr:
		if !walkExprShallow(x.X, visit) {
			return false
		}
		for _, a := range x.List {
			if !walkExprShallow(a, visit) {
				return false
			}
		}
		return true
	case BetweenExpr:
		return walkExprShallow(x.X, visit) && walkExprShallow(x.Lo, visit) && walkExprShallow(x.Hi, visit)
	case LikeExpr:
		return walkExprShallow(x.X, visit) && walkExprShallow(x.Pattern, visit) && walkExprShallow(x.Escape, visit)
	case GlobExpr:
		return walkExprShallow(x.X, visit) && walkExprShallow(x.Pattern, visit)
	case CollateExpr:
		return walkExprShallow(x.X, visit)
	case CastExpr:
		return walkExprShallow(x.X, visit)
	case CaseExpr:
		if x.Base != nil && !walkExprShallow(x.Base, visit) {
			return false
		}
		for _, w := range x.Whens {
			if !walkExprShallow(w.When, visit) || !walkExprShallow(w.Then, visit) {
				return false
			}
		}
		if x.Else != nil {
			return walkExprShallow(x.Else, visit)
		}
		return true
	default:
		return true
	}
}

// chainNamedWindows is sqlite3WindowChain applied to a WINDOW clause as the
// grammar builds it (windowdefn_list, parse.y): each definition after the FIRST
// resolves its base name against the definitions written BEFORE it, the latest
// of a repeated name winning, and the first definition's base is never looked
// at, so a forward reference is silently dropped:
//
//	WINDOW w2 AS (w3 ORDER BY v), w3 AS (PARTITION BY g)   -- w2 is just ORDER BY v
//	WINDOW w1 AS (PARTITION BY g), w2 AS (nope ORDER BY v) -- no such window: nope
//
// The errors are "no such window: %s" and "cannot override %s of window: %s"
// for a PARTITION BY of its own, an ORDER BY when the base has one, or a base
// that spells out a frame (even though this definition names none). The result
// holds the effective spec of every definition (the base's PARTITION BY and
// ORDER BY, the definition's own frame) with Ref cleared.
func chainNamedWindows(windows []NamedWindow) ([]NamedWindow, error) {
	out := make([]NamedWindow, len(windows))
	for i, nw := range windows {
		out[i] = nw
		spec := nw.Spec
		if spec == nil || spec.Ref == "" {
			continue
		}
		if i == 0 {
			cp := *spec
			cp.Ref, cp.Bare = "", false
			out[i].Spec = &cp
			continue
		}
		eff, err := refineWindow(spec, out[:i])
		if err != nil {
			return nil, err
		}
		out[i].Spec = eff
	}
	return out, nil
}

// findWindowDef is windowFind: the latest definition of name.
func findWindowDef(windows []NamedWindow, name string) *WindowSpec {
	for i := len(windows) - 1; i >= 0; i-- {
		if equalFoldName(windows[i].Name, name) {
			return windows[i].Spec
		}
	}
	return nil
}

// refineWindow is sqlite3WindowChain for a spec that names a base window.
func refineWindow(spec *WindowSpec, windows []NamedWindow) (*WindowSpec, error) {
	base := findWindowDef(windows, spec.Ref)
	if base == nil {
		return nil, fmt.Errorf("engine: no such window: %s", spec.Ref)
	}
	switch {
	case len(spec.PartitionBy) > 0:
		return nil, fmt.Errorf("engine: cannot override PARTITION clause of window: %s", spec.Ref)
	case len(spec.OrderBy) > 0 && len(base.OrderBy) > 0:
		return nil, fmt.Errorf("engine: cannot override ORDER BY clause of window: %s", spec.Ref)
	case base.Frame != nil:
		return nil, fmt.Errorf("engine: cannot override frame specification of window: %s", spec.Ref)
	}
	out := &WindowSpec{PartitionBy: base.PartitionBy, OrderBy: base.OrderBy, Frame: spec.Frame}
	if len(spec.OrderBy) > 0 {
		out.OrderBy = spec.OrderBy
	}
	return out, nil
}

// resolveWindowSpec resolves a window specification's base-window REFERENCE
// (WindowSpec.Ref) against the statement's WINDOW clause (already chained by
// chainNamedWindows), returning the effective spec. The two spellings differ,
// as sqlite3WindowUpdate has them:
//
//	OVER win          the named window whole, its frame included
//	OVER (win ...)    sqlite3WindowChain's refinement: the base's PARTITION BY,
//	                  the reference's ORDER BY or else the base's, the
//	                  reference's own frame; a PARTITION BY of its own, a second
//	                  ORDER BY, or a base that spells out a frame is an error
//
// The latest definition of a repeated name wins in both.
func resolveWindowSpec(spec *WindowSpec, windows []NamedWindow, depth int) (*WindowSpec, error) {
	if spec == nil || spec.Ref == "" {
		return spec, nil
	}
	if spec.Bare {
		base := findWindowDef(windows, spec.Ref)
		if base == nil {
			return nil, fmt.Errorf("engine: no such window: %s", spec.Ref)
		}
		return &WindowSpec{PartitionBy: base.PartitionBy, OrderBy: base.OrderBy, Frame: base.Frame}, nil
	}
	return refineWindow(spec, windows)
}
