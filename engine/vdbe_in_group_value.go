package engine

import "strings"

// inSubConvertsGroupValue reports whether "X IN (sub)" is one C answers by
// converting X's own register, where X is a group's value the aggregate's result
// may read again, and the affinity it converts with (rhsAff and rhsCol are the
// subquery column's). sqlite3ExprCodeIN applies the comparison affinity to rLhs in
// place (expr.c:4164), and for a group's column or aggregate rLhs is the AggInfo
// register itself (expr.c:4996, 5342), shared by every later read of that value
// in HAVING, the select list and ORDER BY:
//
//	SELECT a, count(*) FROM t GROUP BY a HAVING a IN (SELECT n FROM s)
//
// prints a text '2' group as the integer 2. An aggregate item here reads its own
// copy of the group's values, so OpInSub declines when the affinity would change
// the probe (inSubPlan.shared) instead of answering with the unconverted value.
func (c *compiler) inSubConvertsGroupValue(x Expr, rhsAff affinity, rhsCol bool, sub *SelectStmt, correlated bool) (affinity, bool) {
	ph, plus := groupValueOperand(x)
	if ph == nil {
		return affNone, false
	}
	var lhsAff affinity
	lhsCol := !plus
	switch p := ph.(type) {
	case groupKeyExpr:
		lhsAff = p.aff
	case groupBareColExpr:
		lhsAff, lhsCol = p.aff, lhsCol && !p.noAff
	case groupAggExpr:
		lhsCol = false
	}
	if plus {
		lhsAff = affNone
	}
	aff := cCompareAffinity(lhsAff, lhsCol, rhsAff, rhsCol)
	if aff == affNone || c.inSubSeeksRowid(sub, correlated) {
		return affNone, false
	}
	regs := c.groupValueRegs(ph)
	if regs == nil || regs.owner == nil {
		return aff, true
	}
	if k, isKey := ph.(groupKeyExpr); isKey && regs.owner.keyColumn(k.idx) == -1 {
		// C codes a GROUP BY expression afresh from its columns, so the probe
		// is a register of its own.
		return affNone, false
	}
	return aff, regs.groupValueReadAgain(ph, sub)
}

// cCompareAffinity is sqlite3CompareAffinity (expr.c:342) as exprINAffinity
// calls it, which unlike comparisonAffinity keeps NUMERIC between two numeric
// columns and tells a column's BLOB affinity (col) from an expression's none:
// both sides with one is NUMERIC if either is numeric and BLOB otherwise, and
// a side without one takes the other's.
func cCompareAffinity(l affinity, lCol bool, r affinity, rCol bool) affinity {
	lHas, rHas := l != affNone || lCol, r != affNone || rCol
	switch {
	case lHas && rHas:
		if isNumericAffinity(l) || isNumericAffinity(r) {
			return affNumeric
		}
		return affNone
	case lHas:
		return l
	}
	return r
}

// groupValueOperand returns the group placeholder x codes to with no instruction
// of its own: sqlite3ExprCodeTemp skips COLLATE (expr.c:5878) and TK_UPLUS codes
// its operand into the same register (expr.c:5552). plus reports a unary plus on
// the way, which takes the column's affinity away (sqlite3ExprAffinity).
func groupValueOperand(x Expr) (ph Expr, plus bool) {
	for {
		switch e := x.(type) {
		case CollateExpr:
			x = e.X
		case UnaryExpr:
			if e.Op != "+" {
				return nil, false
			}
			x, plus = e.X, true
		case groupKeyExpr, groupBareColExpr, groupAggExpr:
			return x, plus
		default:
			return nil, false
		}
	}
}

// groupValueReadAgain reports whether anything in the aggregate's result
// besides this probe might read the register C converts: the same aggregate
// call, or the same column as a key, a bare column or a GROUP BY expression
// over it (recomputed from the column in C). Any subquery body or window value
// counts, since those can read the group by name, and so does anything this
// cannot place.
func (regs *aggResultRegs) groupValueReadAgain(ph Expr, own *SelectStmt) bool {
	plan := regs.owner
	items := append([]*itemPlan{plan.havingPlan}, plan.outPlans...)
	for _, op := range plan.orderPlans {
		items = append(items, op.item)
	}
	var home *itemPlan
	if regs.item != nil && holdsInSub(regs.tree, own) {
		home = regs.item
	}
	same := plan.sameGroupValue(ph, home)
	reads := 0
	var it *itemPlan
	var walk func(e Expr)
	walk = func(e Expr) {
		if e == nil || reads > 1 {
			return
		}
		switch x := e.(type) {
		case groupKeyExpr, groupBareColExpr, groupAggExpr:
			if same(it, x) {
				reads++
			}
			return
		case windowResultExpr, SubqueryExpr, ExistsExpr:
			reads += 2
			return
		case InExpr:
			if x.Sub != nil && x.Sub != own {
				reads += 2
				return
			}
		}
		walkExprOperands(e, walk)
	}
	for _, it = range items {
		switch {
		case it == nil:
		case it == regs.item:
			walk(regs.tree)
		default:
			walk(it.rewritten)
		}
	}
	return reads > 1
}

// sameGroupValue returns whether a placeholder in an item names the register ph
// does in C (one per column, AggInfo_col; one per distinct aggregate call,
// AggInfo_func), answering yes where it cannot tell.
func (plan *aggPlan) sameGroupValue(ph Expr, home *itemPlan) func(*itemPlan, Expr) bool {
	ours := func(owner *aggPlan) bool { return owner == nil || owner == plan }
	switch p := ph.(type) {
	case groupAggExpr:
		want := templateCall(home, p.accIdx)
		return func(it *itemPlan, e Expr) bool {
			x, ok := e.(groupAggExpr)
			if !ok || !ours(x.owner) {
				return false
			}
			got := templateCall(it, x.accIdx)
			return want == nil || got == nil || exprEqual(*want, *got)
		}
	case groupBareColExpr:
		if p.isRowid {
			return func(_ *itemPlan, e Expr) bool {
				switch x := e.(type) {
				case groupKeyExpr:
					return ours(x.owner)
				case groupBareColExpr:
					return ours(x.owner)
				}
				return false
			}
		}
		return plan.readsColumn(p.idx)
	case groupKeyExpr:
		return plan.readsColumn(plan.keyColumn(p.idx))
	}
	return func(*itemPlan, Expr) bool { return true }
}

// readsColumn matches a read of row column col (negative: unknown, so every
// column read matches).
func (plan *aggPlan) readsColumn(col int) func(*itemPlan, Expr) bool {
	return func(_ *itemPlan, e Expr) bool {
		switch x := e.(type) {
		case groupKeyExpr:
			if x.owner != nil && x.owner != plan {
				return false
			}
			switch kc := plan.keyColumn(x.idx); {
			case col < 0 || kc == -2:
				return true
			case kc == -1:
				return plan.exprReadsColumn(plan.groupExprs[x.idx], col)
			default:
				return kc == col
			}
		case groupBareColExpr:
			if x.owner != nil && x.owner != plan {
				return false
			}
			return col < 0 || x.isRowid || x.idx == col
		}
		return false
	}
}

// keyColumn is the row column GROUP BY key k is a bare read of, -1 for an
// expression key, or -2 when it cannot say.
func (plan *aggPlan) keyColumn(k int) int {
	if k < 0 || k >= len(plan.groupExprs) {
		return -2
	}
	e := plan.groupExprs[k]
	for {
		ce, ok := e.(CollateExpr)
		if !ok {
			break
		}
		e = ce.X
	}
	col, ok := e.(ColumnExpr)
	if !ok {
		return -1
	}
	if idx, ok := resolveColumnIndex(plan.scopes, col); ok {
		return idx
	}
	return -2
}

// exprReadsColumn reports whether e might read row column col.
func (plan *aggPlan) exprReadsColumn(e Expr, col int) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if x == nil || found {
			return
		}
		switch y := x.(type) {
		case ColumnExpr:
			idx, ok := resolveColumnIndex(plan.scopes, y)
			found = !ok || idx == col
			return
		case SubqueryExpr, ExistsExpr:
			found = true
			return
		case InExpr:
			if y.Sub != nil {
				found = true
				return
			}
		}
		walkExprOperands(x, walk)
	}
	walk(e)
	return found
}

// templateCall is the aggregate call an item's accIdx'th accumulator was
// planned from, or nil.
func templateCall(it *itemPlan, accIdx int) *FuncExpr {
	if it == nil || accIdx < 0 || accIdx >= len(it.aggTemplates) {
		return nil
	}
	return it.aggTemplates[accIdx].srcCall
}

// holdsInSub reports whether e holds the IN whose subquery is sub.
func holdsInSub(e Expr, sub *SelectStmt) bool {
	found := false
	var walk func(Expr)
	walk = func(x Expr) {
		if x == nil || found {
			return
		}
		if in, ok := x.(InExpr); ok && in.Sub == sub {
			found = true
			return
		}
		walkExprOperands(x, walk)
	}
	walk(e)
	return found
}

// groupValueRegs is the register block ph reads, found as aggResultReg finds it;
// nil when no enclosing compile holds one.
func (c *compiler) groupValueRegs(ph Expr) *aggResultRegs {
	var owner *aggPlan
	switch x := ph.(type) {
	case groupKeyExpr:
		owner = x.owner
	case groupBareColExpr:
		owner = x.owner
	case groupAggExpr:
		owner = x.owner
	}
	for oc := c; oc != nil; oc = oc.outer {
		if oc.aggRegs != nil && (owner == nil || oc.aggRegs.owner == owner) {
			return oc.aggRegs
		}
	}
	return nil
}

// inSubSeeksRowid reports whether C answers "X IN (sub)" with a rowid search
// (sqlite3FindInIndex's IN_INDEX_ROWID, expr.c:3292), which converts a copy of X
// (OP_SeekRowid) and leaves X's register alone: isCandidateForInOpt's shape
// (expr.c:3072: no compound, DISTINCT, aggregate, LIMIT or WHERE, one real table)
// with the rowid as its lone result. Anything not certainly that is answered no.
func (c *compiler) inSubSeeksRowid(sub *SelectStmt, correlated bool) bool {
	if correlated || c.pager == nil || len(sub.Compound) > 0 || sub.ValuesArms > 0 || sub.Distinct ||
		len(sub.GroupBy) > 0 || sub.Having != nil || sub.Where != nil || sub.Limit != nil || sub.LimitParam != nil ||
		len(sub.CTEs) > 0 || len(sub.From) != 1 || len(sub.Columns) != 1 || sub.Columns[0].Star {
		return false
	}
	it := sub.From[0]
	if it.Table == "" || it.Subquery != nil || it.TableFunc {
		return false
	}
	col, ok := sub.Columns[0].Expr.(ColumnExpr)
	if !ok || col.Schema != "" {
		return false
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	if col.Qualifier != "" && !strings.EqualFold(col.Qualifier, name) {
		return false
	}
	if it.Schema == "" {
		if _, isCTE := c.pager.lookupCTE(it.Table); isCTE {
			return false
		}
	}
	rp := c.pager
	if it.Schema != "" || len(c.pager.attachedReaders) > 0 {
		owner, err := c.pager.itemOwner(it)
		if err != nil {
			return false
		}
		rp = owner
	}
	tbl, err := rp.resolveTableIn(fromItemScope(it), it.Table)
	if err != nil || tbl.withoutRowid {
		return false
	}
	for i, ci := range tbl.cols {
		if strings.EqualFold(ci.Name, col.Name) {
			return i == tbl.ipkIndex
		}
	}
	return isRowidName(strings.ToLower(col.Name))
}

// groupValueIsReal reports whether x's group value is a REAL column's.
func groupValueIsReal(x Expr) bool {
	ph, _ := groupValueOperand(x)
	switch ph := ph.(type) {
	case groupKeyExpr:
		return ph.aff == affReal
	case groupBareColExpr:
		return ph.aff == affReal
	}
	return false
}

// inSubConverts reports whether the probe's affinity changes v as C's
// OP_Affinity would change the register holding it.
func inSubConverts(v Value, plan *inSubPlan) bool {
	if plan.sharedReal && v.Typ == Int {
		v = Value{Typ: Float, F: float64(v.I)}
	}
	aff := plan.sharedAff
	return !affinityIsIdentity(v.Typ, aff) && applyAffinityToValue(v, aff).Typ != v.Typ
}
