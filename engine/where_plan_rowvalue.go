package engine

// Row value comparisons in the planner.
// The parser desugars them into scalar AND/OR trees which execute.
// The planner arms them as:
//
//   - a vector RANGE ("<" "<=" ">" ">=") is an ordinary range term on the
//     column of its FIRST element (exprMightBeIndexed, whereexpr.c:1080), its
//     affinity and collation the first elements' too (sqlite3ExprAffinity and
//     sqlite3ExprCollSeq step into a[0] of a TK_VECTOR, expr.c:74, 274);
//   - a vector "=" / "IS" drives nothing itself, but in an AND clause is
//     sliced into one "Li op Ri" term per field, APPENDED to the clause and
//     analysed there, and the original is then marked TERM_CODED|TERM_VIRTUAL
//     with eOperator WO_ROWVAL (tag-20220128a, whereexpr.c:1455-1489);
//   - a vector IN over a value list is "IN (VALUES ...)" (parse.y:1531), and
//     in an AND clause gets one VIRTUAL child per field, each a WO_IN term
//     on that field priced like "x IN (SELECT ...)" (whereexpr.c:1491-1520,
//     where.c:3337);
//   - a vector BETWEEN gets the usual two virtual children, which are vector
//     ranges (whereexpr.c:1290);
//   - "<>", "IS NOT", and anything under a NOT are opaque terms.
//
// Pricing the desugared OR/AND tree instead -- "(c,d) > (30,2)" as "c > 30 OR
// (c = 30 AND d > 2)", even as a WHERE_MULTI_OR loop -- walks a different index
// than C does, and every order-sensitive output of such a statement was a
// wrong answer (compat-harness/rowvalue_plan_test.go). So the parser records
// the comparison as written on the tree's root (BinaryExpr.row), and the
// planner reads that instead of the tree below it.

// rowValueOrigin is a row-value comparison as written.
type rowValueOrigin struct {
	// op is the comparison operator as parsed -- "=" "==" "IS" "!=" "<>"
	// "IS NOT" "<" "<=" ">" ">=" -- or "BETWEEN" or "IN".
	op string
	// l is the left operand's elements; r the right's for a comparison.
	l, r []Expr
	// lo/hi are a BETWEEN's bounds.
	lo, hi []Expr
	// list is an IN's value rows, each as wide as l.
	list [][]Expr
}

// whereRowOrigin answers the row-value comparison e is the desugared root of,
// or nil.
func whereRowOrigin(e Expr) *rowValueOrigin {
	if b, ok := e.(BinaryExpr); ok {
		return b.row
	}
	return nil
}

// elems is every operand element of the comparison.
func (o *rowValueOrigin) elems() []Expr {
	out := make([]Expr, 0, len(o.l)+len(o.r)+len(o.lo)+len(o.hi))
	out = append(append(append(append(out, o.l...), o.r...), o.lo...), o.hi...)
	for _, row := range o.list {
		out = append(out, row...)
	}
	return out
}

// isRange is the vector inequality exprMightBeIndexed steps into
// ("op>=TK_GT && op<=TK_GE", whereexpr.c:1080).
func (o *rowValueOrigin) isRange() bool {
	switch o.op {
	case "<", "<=", ">", ">=":
		return true
	}
	return false
}

// isEq is the vector "==" or "IS" tag-20220128a slices.
func (o *rowValueOrigin) isEq() bool {
	return o.op == "=" || o.op == "==" || equalFoldName(o.op, "IS")
}

// withOperands is o with f applied to every operand element that
// propagateConstants' walk reaches: the vectors of a comparison or a BETWEEN,
// and an IN's left vector -- but NOT its value rows, which are a VALUES
// subquery by then and which sqlite3SelectWalkNoop does not enter.
func (o *rowValueOrigin) withOperands(f func(Expr) Expr) *rowValueOrigin {
	m := func(es []Expr) []Expr {
		if es == nil {
			return nil
		}
		out := make([]Expr, len(es))
		for i, e := range es {
			out[i] = f(e)
		}
		return out
	}
	n := *o
	n.l, n.r, n.lo, n.hi = m(o.l), m(o.r), m(o.lo), m(o.hi)
	return &n
}

// whereSplitTopAnd is splitTopLevelAnd for the planner: a desugared row-value
// equality or BETWEEN is an AND to the executor but ONE term to C, so it is not
// split. (splitTopLevelAnd itself stays as it is -- the executor's pushdown
// wants the conjuncts it actually evaluates.)
func whereSplitTopAnd(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(BinaryExpr); ok && b.Op == "AND" && b.row == nil {
		return append(whereSplitTopAnd(b.L), whereSplitTopAnd(b.R)...)
	}
	return []Expr{e}
}

// whereIsPlainOr is a TK_OR node, as opposed to the OR a desugared row value
// happens to be rooted in.
func whereIsPlainOr(e Expr) bool {
	b, ok := e.(BinaryExpr)
	return ok && equalFoldName(b.Op, "OR") && b.row == nil
}

// whereRowCmpExpr builds the vector comparison "l op r" -- a vector BETWEEN's
// child (whereexpr.c:1302) -- for the planner alone: its row origin is all the
// analysis reads, and its tree holds the RowExprs rather than a desugaring,
// since nothing ever executes it.
func whereRowCmpExpr(op string, l, r []Expr) BinaryExpr {
	return BinaryExpr{Op: op, L: RowExpr{Elems: l}, R: RowExpr{Elems: r},
		row: &rowValueOrigin{op: op, l: l, r: r}}
}

// whereRowCollation is sqlite3BinaryCompareCollSeq (expr.c:424) over two
// vectors. It tests EP_Collate on each WHOLE vector, which a COLLATE on any
// element sets (EP_Propagate), and then asks sqlite3ExprCollSeq of that vector
// -- which steps into its FIRST element only (expr.c:274). So a COLLATE on a
// later element chooses which side answers without being the answer: "(c+0,
// d COLLATE nocase) > (x, 2)" compares under c+0's collation -- none, so
// BINARY -- where "c+0 > x" would take x's declared one.
func whereRowCollation(jts []joinedTable, scopes []tableScope, l, r []Expr) string {
	flagged := func(v []Expr) bool {
		for _, e := range v {
			if wherePlanExprHasCollate(e) {
				return true
			}
		}
		return false
	}
	collSeq := func(e Expr) (string, bool) {
		if n, ok := exprCollation(e); ok {
			return effectiveCollation(n), true
		}
		if n, ok := wherePlanDeclaredCollation(jts, scopes, e); ok {
			return effectiveCollation(n), true
		}
		return "", false
	}
	var n string
	var ok bool
	switch {
	case flagged(l):
		n, ok = collSeq(l[0])
	case flagged(r):
		n, ok = collSeq(r[0])
	default:
		if n, ok = collSeq(l[0]); !ok {
			n, ok = collSeq(r[0])
		}
	}
	if !ok {
		return "BINARY" // "pColl ? pColl->zName : sqlite3StrBINARY" (where.c:394)
	}
	return n
}

// whereRowInFields is exprAnalyze's vector-IN arm (whereexpr.c:1491-1520) for
// "(l1, ..., lN) IN (VALUES ...)": one VIRTUAL child per field, each inserted
// over the SAME pExpr with u.x.iField = i+1, analysed, and made a child of the
// IN term by markTermAsChild. t is the IN term as analysed, at index self.
func whereRowInFields(jts []joinedTable, scopes []tableScope, o *rowValueOrigin, t whereIdxTerm, self int,
	maskOf func(Expr) whereMask) []whereIdxTerm {
	// The child's exprAnalyze reads the same pExpr as the IN term's own:
	// prereqLeft is the whole left vector's usage and prereqRight the VALUES'
	// (exprSelectUsage), so its WO_ALL/WO_EQUIV opMask is the IN's.
	var prereqLeft whereMask
	for _, l := range o.l {
		prereqLeft |= maskOf(l)
	}
	opMask := woEquiv
	if prereqLeft&t.prereqRight == 0 {
		opMask = ^uint16(0) // WO_ALL
	}
	// indexInAffinityOk (where.c:319) prices each field as "Li = Ri", Ri the
	// field of pX->x.pSelect->pEList -- the LAST row, since
	// sqlite3ExprListToValues chains each row's Select onto the one before it
	// (expr.c:1119-1125).
	last := o.list[len(o.list)-1]
	out := make([]whereIdxTerm, 0, len(o.l))
	for i, l := range o.l {
		c := t
		c.virt, c.parent = true, self
		c.cursor, c.column, c.rCursor, c.rColumn = -1, xnRowid, -1, xnRowid
		c.op, c.iField, c.inCount = 0, i+1, -1
		// A loop on this field seeks with the VALUES rows' field-i values
		// under ONE comparison affinity and collation, the last row's
		// (exprINAffinity, expr.c:3463, reads pSelect->pEList, and
		// sqlite3CodeRhsOfIN's KeyInfo takes sqlite3BinaryCompareCollSeq of
		// the same pair, expr.c:3752) -- where the WHERE this engine evaluates
		// compares every row under its own. Where some row's differs from the
		// last's, the two row sets can too, and a winning loop that uses the
		// field declines, as for "x IN (SELECT y COLLATE ...)"
		// (wherePlanInRhsRefuse). Affinities differ only by class: OP_Eq and
		// friends treat every affinity >= SQLITE_AFF_NUMERIC alike, TEXT on its
		// own, and the rest not at all (vdbe.c:2349-2360).
		affClass := func(a affinity) int {
			switch {
			case isNumericAffinity(a):
				return 2
			case a == affText:
				return 1
			}
			return 0
		}
		lastAff := whereComparisonAffinity(jts, scopes, l, last[i])
		lastColl := whereCompareCollation(jts, scopes, l, last[i])
		for _, row := range o.list {
			if affClass(whereComparisonAffinity(jts, scopes, l, row[i])) != affClass(lastAff) ||
				whereCompareCollation(jts, scopes, l, row[i]) != lastColl {
				c.inRefuse = true
			}
		}
		// "pLeft = pLeft->x.pList->a[pTerm->u.x.iField-1].pExpr" (whereexpr.c:
		// 1211): the element as written -- exprMightBeIndexed skips no COLLATE
		// on it.
		if tab, col, ok := wherePlanColumnRef(jts, scopes, l); ok {
			c.cursor, c.column = tab, col
			c.op = woIn & opMask
		}
		c.aff, c.coll = lastAff, lastColl
		out = append(out, c)
	}
	return out
}
