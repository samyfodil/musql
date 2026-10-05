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

// resolveWindowSpec resolves a window specification's base-window REFERENCE
// (WindowSpec.Ref) against the statement's "WINDOW <name> AS (...)" clause,
// returning the effective spec. Both spellings of a reference go through it:
// the bare "OVER win", and "OVER (win ORDER BY ...)" which refines a base.
//
// The inheritance and override rules, each verified directly against
// mattn/go-sqlite3:
//
//	WINDOW w AS (PARTITION BY a%2)   +  OVER (w ORDER BY a)
//	        -> the base's PARTITION BY, the reference's ORDER BY
//	WINDOW w AS (ORDER BY a)         +  OVER (w ROWS 1 PRECEDING)
//	        -> the base's ORDER BY, the reference's FRAME
//	WINDOW w AS (ORDER BY a), w2 AS (w ROWS 1 PRECEDING) + OVER w2
//	        -> a named window may itself reference another (same answer)
//
// and the three errors, with SQLite's own wording: "no such window: %s",
// "cannot override PARTITION clause of window: %s" (a reference may NEVER
// supply its own PARTITION BY), and "cannot override ORDER BY clause of
// window: %s" (only when the base already has one).
func resolveWindowSpec(spec *WindowSpec, windows []NamedWindow, depth int) (*WindowSpec, error) {
	if spec == nil || spec.Ref == "" {
		return spec, nil
	}
	if depth > len(windows)+1 {
		// A cycle ("WINDOW w AS (w)"): bounded rather than recursed forever.
		return nil, fmt.Errorf("engine: circular reference: %s", spec.Ref)
	}
	var base *WindowSpec
	for _, nw := range windows {
		if equalFoldName(nw.Name, spec.Ref) {
			base = nw.Spec
			break
		}
	}
	if base == nil {
		return nil, fmt.Errorf("engine: no such window: %s", spec.Ref)
	}
	base, err := resolveWindowSpec(base, windows, depth+1)
	if err != nil {
		return nil, err
	}
	if len(spec.PartitionBy) > 0 {
		return nil, fmt.Errorf("engine: cannot override PARTITION clause of window: %s", spec.Ref)
	}
	if len(spec.OrderBy) > 0 && len(base.OrderBy) > 0 {
		return nil, fmt.Errorf("engine: cannot override ORDER BY clause of window: %s", spec.Ref)
	}
	out := &WindowSpec{PartitionBy: base.PartitionBy, OrderBy: base.OrderBy, Frame: base.Frame}
	if len(spec.OrderBy) > 0 {
		out.OrderBy = spec.OrderBy
	}
	if spec.Frame != nil {
		out.Frame = spec.Frame
	}
	return out, nil
}
