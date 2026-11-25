package engine

import (
	"slices"
	"strings"
)

// countCrossJoin rewrites "SELECT count(*) FROM a, b, ... WHERE ..." to the
// product of the tables' own counts,
//
//	SELECT (SELECT count(*) FROM a WHERE <a's terms>) * (SELECT count(*) FROM b WHERE <b's terms>) ...
//
// when every WHERE conjunct reads columns of one table at most. The joined
// rows are then every pair of an a row passing a's terms and a b row passing
// b's, so their count is the product. C walks the nested loop instead, which
// for two 10,000-row tables is 10^8 steps (#394).
//
// The rewrite must not raise an error the loop would not, nor miss one it
// would. The loop skips the inner table's terms once the outer table has no
// row left, so a term that can fail at run time is refused: only columns,
// literals, parameters and operators that cannot fail are taken (no function,
// subquery, LIKE or GLOB). The sources are ordinary tables and generate_series
// over literals, never a view, subquery or virtual table, whose rows are
// produced by code that can fail. A prepare-time error ("no such column",
// an unknown collation) is left to the statement as written: it is compiled
// first, with count(*) replaced by a constant, and an error refuses the
// rewrite.
//
// A qualified column goes with the table of that name or alias, which must be
// unique in the FROM; an unqualified one with the one table that declares it
// (the probe has refused a name two declare); a term with no column goes with
// the first table.
// Like countUnionAll, only for a statement with no enclosing query.
func (p *ReadOnlyPager) countCrossJoin(stmt *SelectStmt) *SelectStmt {
	if p == nil || stmt == nil || len(stmt.Columns) != 1 || len(stmt.From) < 2 {
		return nil
	}
	if stmt.Distinct || len(stmt.GroupBy) != 0 || stmt.Having != nil ||
		len(stmt.Compound) != 0 || len(stmt.OrderBy) != 0 || stmt.Limit != nil || stmt.Offset != nil ||
		stmt.LimitParam != nil || stmt.OffsetParam != nil || len(stmt.Windows) != 0 || len(stmt.CTEs) != 0 ||
		stmt.ValuesArms != 0 || stmt.FromNestedParenJoin || stmt.FromParenthesized {
		return nil
	}
	col := stmt.Columns[0]
	fn, ok := col.Expr.(FuncExpr)
	if col.Star || !ok || !strings.EqualFold(fn.Name, "count") || !fn.Star || len(fn.Args) != 0 ||
		fn.Distinct || fn.Over != nil || fn.Filter != nil || len(fn.OrderBy) != 0 {
		return nil
	}
	names := make([]string, len(stmt.From))
	cols := make([][]string, len(stmt.From))
	for i, it := range stmt.From {
		if cols[i], ok = p.countCrossJoinSource(it, i); !ok {
			return nil
		}
		names[i] = it.Alias
		if names[i] == "" {
			names[i] = it.Table
		}
		for _, prev := range names[:i] {
			if strings.EqualFold(prev, names[i]) {
				return nil
			}
		}
	}
	terms := make([][]Expr, len(stmt.From))
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		owner := 0
		seen := false
		okTerm := countCrossJoinSafe(cj, func(c ColumnExpr) bool {
			if c.Schema != "" {
				return false
			}
			at := -1
			for i, n := range names {
				if c.Qualifier != "" && strings.EqualFold(n, c.Qualifier) {
					at = i
				}
				if c.Qualifier == "" && slices.ContainsFunc(cols[i], func(n string) bool { return strings.EqualFold(n, c.Name) }) {
					if at >= 0 {
						return false
					}
					at = i
				}
			}
			if at < 0 || (seen && at != owner) {
				return false
			}
			owner, seen = at, true
			return true
		})
		if !okTerm {
			return nil
		}
		terms[owner] = append(terms[owner], cj)
	}
	probe := *stmt
	probe.Columns = []SelectColumn{{Expr: LiteralExpr{Val: Value{Typ: Int, I: 1}}, Text: "1"}}
	if _, err := compileSubProgram(p, &probe, nil); err != nil {
		return nil
	}
	var product Expr
	for i, it := range stmt.From {
		it.Join, it.On = JoinCross, nil
		count := SubqueryExpr{Stmt: &SelectStmt{
			Columns: []SelectColumn{{Expr: FuncExpr{Name: "count", Star: true}, Text: "count(*)"}},
			From:    []FromItem{it},
			Where:   andConjuncts(terms[i]),
		}}
		if product == nil {
			product = count
			continue
		}
		product = BinaryExpr{Op: "*", L: product, R: count}
	}
	out := *stmt
	out.Columns = []SelectColumn{col}
	out.Columns[0].Expr = product
	out.From = nil
	out.Where = nil
	return &out
}

// countCrossJoinSource returns the columns of a FROM item that is an ordinary
// table or a generate_series over literals, joined to the items before it with
// no condition of its own, and false for any other item.
func (p *ReadOnlyPager) countCrossJoinSource(it FromItem, i int) ([]string, bool) {
	if it.Subquery != nil || it.Schema != "" || it.On != nil || len(it.Using) != 0 || it.Natural ||
		it.GroupLen != 0 || it.GroupAlias != "" || it.HasGroupAlias || it.GroupRebuildID != 0 ||
		it.NestFromWrapDepth != 0 || len(it.NestedGroupSpan) != 0 || it.NestedNonLeading ||
		it.NestedGroupID != 0 || it.UpdateTarget {
		return nil, false
	}
	if i > 0 && it.Join != JoinCross && it.Join != JoinInner {
		return nil, false
	}
	if _, isCTE := p.lookupCTE(it.Table); isCTE {
		return nil, false
	}
	if it.TableFunc {
		if !strings.EqualFold(it.Table, "generate_series") || len(it.TableFuncArgs) < 2 {
			return nil, false
		}
		for _, a := range it.TableFuncArgs {
			if u, ok := a.(UnaryExpr); ok && u.Op == "-" {
				a = u.X
			}
			if l, ok := a.(LiteralExpr); !ok || l.Val.Typ != Int {
				return nil, false
			}
		}
		return []string{"value", "start", "stop", "step"}, true
	}
	if len(it.TableFuncArgs) != 0 {
		return nil, false
	}
	if vt, err := p.isVtabItem(it); err != nil || vt {
		return nil, false
	}
	rt, err := p.resolveTableIn(fromItemScope(it), it.Table)
	if err != nil {
		return nil, false
	}
	names := make([]string, len(rt.cols))
	for i, c := range rt.cols {
		names[i] = c.Name
	}
	return names, true
}

// countCrossJoinSafe reports whether e is built only from what cannot fail at
// run time, handing each column to col, which can refuse it.
func countCrossJoinSafe(e Expr, col func(ColumnExpr) bool) bool {
	switch x := e.(type) {
	case LiteralExpr, ParamExpr:
		return true
	case ColumnExpr:
		return col(x)
	case UnaryExpr:
		return countCrossJoinSafe(x.X, col)
	case BinaryExpr:
		// "||" can fail: its result can pass the length limit.
		return x.Op != "||" && countCrossJoinSafe(x.L, col) && countCrossJoinSafe(x.R, col)
	case IsNullExpr:
		return countCrossJoinSafe(x.X, col)
	case BetweenExpr:
		return countCrossJoinSafe(x.X, col) && countCrossJoinSafe(x.Lo, col) && countCrossJoinSafe(x.Hi, col)
	case InExpr:
		if x.Sub != nil || !countCrossJoinSafe(x.X, col) {
			return false
		}
		for _, v := range x.List {
			if !countCrossJoinSafe(v, col) {
				return false
			}
		}
		return true
	case CollateExpr:
		return countCrossJoinSafe(x.X, col)
	case CastExpr:
		return countCrossJoinSafe(x.X, col)
	}
	return false
}
