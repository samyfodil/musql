package engine

// How a query reaches a vec1 table: the constraints C SQLite's planner would
// hand vec1's xBestIndex, and what vec1BestIndexMethod makes of them.
//
// A vec1 query's answer depends on its plan, which is unusual. A constraint
// on a metadata column that vec1 consumes is applied inside the index, before
// the K nearest are chosen, and a LIMIT the planner lets it see becomes K. So
// the constraints are rebuilt here as sqlite3WhereExprAnalyze builds them --
// the WHERE's AND terms, a commuted copy where the column is on the right,
// BETWEEN's two halves, an OR of equalities on one column turned into IN, the
// table-valued-function arguments last, then LIMIT and OFFSET
// (sqlite3WhereAddLimit) -- and vec1's rules are applied to them. A shape this
// does not model precisely is declined wherever vec1 could have consumed it.
//
// The conjuncts vec1 consumes are omitted (omit=1 in C) from the residual
// WHERE at compile time (vtabOmitConjuncts), and the run-time plan must
// consume exactly those, or the query declines.

import (
	"fmt"
	"strings"
)

// The constraint operators SQLite offers (SQLITE_INDEX_CONSTRAINT_*).
const (
	vec1CEQ = iota + 1
	vec1CGT
	vec1CLE
	vec1CLT
	vec1CGE
	vec1CMatch
	vec1CLike
	vec1CGlob
	vec1CRegexp
	vec1CNE
	vec1CIsNot
	vec1CIsNotNull
	vec1CIsNull
	vec1CIs
	vec1CLimit
	vec1COffset
)

// vec1Cons is one constraint: a column of the table (in C's numbering: -1
// the rowid, 0 cmd, 1 arg, 2 distance, 3 the vector, 4+ the metadata), its
// operator and right-hand side.
type vec1Cons struct {
	col  int
	op   int
	rhs  Expr   // nil for IS NULL / IS NOT NULL, LIMIT and OFFSET
	in   []Expr // an IN's values; the operator is EQ
	coll string // an explicit COLLATE on the comparison, "" for none
	conj int    // the tvfWhere conjunct, -1 for an argument or LIMIT/OFFSET
	arg  int    // the argument index, for an argument
	// usable is false for a right-hand side naming this table.
	usable bool
}

// vec1PlanState is what vec1BestIndexMethod decided.
type vec1PlanState struct {
	cons []vec1Cons
	// knn: cmd's constraint and arg's (-1 if none), the consumed metadata
	// constraints in order, and whether the LIMIT is K.
	knn          bool
	cmd, arg     int
	meta         []int
	metaOp       []byte
	limit        int
	rowid        int // the rowid plan's constraint, -1 for none
	omit         []int
	declineError error
}

// vec1ColOf is the C column number of e when it names a column of this item
// (COLLATE skipped, its name kept), or false.
func vec1ColOf(e Expr, cols []columnInfo, scope string) (int, string, bool) {
	coll := ""
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			break
		}
		if coll == "" {
			coll = c.Name
		}
		e = c.X
	}
	ce, ok := e.(ColumnExpr)
	if !ok || ce.Schema != "" && !strings.EqualFold(ce.Schema, "main") {
		return 0, "", false
	}
	if ce.Qualifier != "" && !equalFoldName(ce.Qualifier, scope) {
		return 0, "", false
	}
	if isRowidAliasName(ce.Name) {
		for _, c := range cols[1:] {
			if equalFoldName(c.Name, ce.Name) {
				goto named
			}
		}
		return -1, coll, true
	}
named:
	for i, c := range cols {
		if i > 0 && equalFoldName(c.Name, ce.Name) {
			return i - 1, coll, true
		}
	}
	return 0, "", false
}

// vec1Refs reports whether e names a column of this item anywhere outside a
// subquery, or holds a subquery (whose correlation this does not trace).
func vec1Refs(e Expr, cols []columnInfo, scope string) bool {
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || found {
			return
		}
		switch x := e.(type) {
		case ColumnExpr:
			if _, _, ok := vec1ColOf(x, cols, scope); ok {
				found = true
			}
			return
		case SubqueryExpr, ExistsExpr:
			found = true
			return
		case InExpr:
			if x.Sub != nil {
				found = true
				return
			}
		}
		walkExprOperands(e, walk)
	}
	walk(e)
	return found
}

func vec1CollOf(e Expr) string {
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			return ""
		}
		if inner := vec1CollOf(c.X); inner != "" {
			return inner
		}
		return c.Name
	}
}

// vec1BinaryOp maps a comparison to its constraint operator, and the operator
// with the sides swapped.
func vec1BinaryOp(op string) (int, int, bool) {
	switch op {
	case "=", "==":
		return vec1CEQ, vec1CEQ, true
	case "<":
		return vec1CLT, vec1CGT, true
	case "<=":
		return vec1CLE, vec1CGE, true
	case ">":
		return vec1CGT, vec1CLT, true
	case ">=":
		return vec1CGE, vec1CLE, true
	case "!=", "<>":
		return vec1CNE, vec1CNE, true
	case "IS":
		return vec1CIs, vec1CIs, true
	case "IS NOT":
		return vec1CIsNot, vec1CIsNot, true
	}
	return 0, 0, false
}

func vec1IsNullLiteral(e Expr) bool {
	l, ok := e.(LiteralExpr)
	return ok && l.Val.Typ == Null
}

// vec1Constraints builds the constraints (sqlite3WhereExprAnalyze's terms on
// this table). allTerms reports condition 4 of sqlite3WhereAddLimit: every
// WHERE term is one passed to xBestIndex. uncertain is a term shaped in a way
// this does not model, which could have been one.
func vec1Constraints(it FromItem, cols []columnInfo, scope string) (cons []vec1Cons, allTerms, uncertain bool) {
	allTerms = true
	var virtual []vec1Cons // terms exprAnalyze appends after the originals
	for ci, conj := range it.tvfWhere {
		// whereClauseInsert analyses a term with its COLLATE and likely()
		// wrappers skipped (sqlite3ExprSkipCollateAndLikely).
		conj = vec1SkipLikely(conj)
		add := func(c vec1Cons) {
			c.conj = ci
			cons = append(cons, c)
		}
		switch x := conj.(type) {
		case BinaryExpr:
			if x.row != nil {
				uncertain = true
				allTerms = false
				continue
			}
			op, flip, ok := vec1BinaryOp(x.Op)
			if !ok {
				if x.Op == "OR" {
					c, converted, unsure := vec1OrToIn(x, cols, scope)
					if converted {
						c.conj = ci
						virtual = append(virtual, c)
						continue
					}
					uncertain = uncertain || unsure
				}
				allTerms = false
				continue
			}
			// "x IS NULL" is TK_ISNULL (binaryToUnaryIfNull).
			if (op == vec1CIs || op == vec1CIsNot) && vec1IsNullLiteral(x.R) {
				if col, coll, ok := vec1ColOf(x.L, cols, scope); ok {
					o := vec1CIsNull
					if op == vec1CIsNot {
						o = vec1CIsNotNull
					}
					add(vec1Cons{col: col, op: o, coll: coll, usable: true})
					continue
				}
			}
			coll := vec1CollOf(x.L)
			if coll == "" {
				coll = vec1CollOf(x.R)
			}
			lcol, _, lok := vec1ColOf(x.L, cols, scope)
			rcol, _, rok := vec1ColOf(x.R, cols, scope)
			switch {
			case lok && rok:
				// Two columns of the table: neither side is usable, and the
				// term does not reach a LIMIT. vec1 consumes neither.
				add(vec1Cons{col: lcol, op: op, rhs: x.R, coll: coll})
				virtual = append(virtual, vec1Cons{col: rcol, op: flip, rhs: x.L, coll: coll, conj: ci})
				allTerms = false
			case lok:
				add(vec1Cons{col: lcol, op: op, rhs: x.R, coll: coll, usable: !vec1Refs(x.R, cols, scope)})
				if vec1Refs(x.R, cols, scope) {
					allTerms = false
				}
			case rok:
				add(vec1Cons{col: rcol, op: flip, rhs: x.L, coll: coll, usable: !vec1Refs(x.L, cols, scope)})
				if vec1Refs(x.L, cols, scope) {
					allTerms = false
				}
			default:
				// An expression of the column ("a+0 = 5") is not offered.
				allTerms = false
			}
		case IsNullExpr:
			if col, coll, ok := vec1ColOf(x.X, cols, scope); ok {
				o := vec1CIsNull
				if x.Not {
					o = vec1CIsNotNull
				}
				add(vec1Cons{col: col, op: o, coll: coll, usable: true})
				continue
			}
			allTerms = false
		case InExpr:
			col, coll, ok := vec1ColOf(x.X, cols, scope)
			switch {
			case !ok || x.Not:
				// NOT IN is never offered; an IN over an expression is not
				// either.
				allTerms = false
			case x.Sub != nil:
				// "x IN (SELECT ...)" is offered and vec1 would consume it.
				add(vec1Cons{col: col, op: vec1CEQ, coll: coll})
				uncertain = true
				allTerms = false
			case len(x.List) == 1 && !vec1Refs(x.List[0], cols, scope):
				add(vec1Cons{col: col, op: vec1CEQ, rhs: x.List[0], coll: coll, usable: true})
			default:
				usable := true
				for _, e := range x.List {
					if vec1Refs(e, cols, scope) {
						usable = false
					}
				}
				add(vec1Cons{col: col, op: vec1CEQ, in: x.List, coll: coll, usable: usable})
				if !usable {
					allTerms = false
				}
			}
		case BetweenExpr:
			col, coll, ok := vec1ColOf(x.X, cols, scope)
			if !ok || x.Not {
				allTerms = false
				continue
			}
			lo, hi := !vec1Refs(x.Lo, cols, scope), !vec1Refs(x.Hi, cols, scope)
			virtual = append(virtual,
				vec1Cons{col: col, op: vec1CGE, rhs: x.Lo, coll: coll, usable: lo, conj: ci},
				vec1Cons{col: col, op: vec1CLE, rhs: x.Hi, coll: coll, usable: hi, conj: ci})
			if !lo || !hi {
				allTerms = false
			}
		case LikeExpr:
			if col, _, ok := vec1ColOf(x.X, cols, scope); ok && !x.Not && x.Escape == nil {
				add(vec1Cons{col: col, op: vec1CLike, rhs: x.Pattern, usable: !vec1Refs(x.Pattern, cols, scope)})
				continue
			}
			allTerms = false
		case GlobExpr:
			if col, _, ok := vec1ColOf(x.X, cols, scope); ok && !x.Not {
				add(vec1Cons{col: col, op: vec1CGlob, rhs: x.Pattern, usable: !vec1Refs(x.Pattern, cols, scope)})
				continue
			}
			allTerms = false
		default:
			// NOT, CASE, a function of the column: not offered.
			allTerms = false
		}
	}
	cons = append(cons, virtual...)
	for ai, a := range it.TableFuncArgs {
		cons = append(cons, vec1Cons{col: ai, op: vec1CEQ, rhs: a, conj: -1, arg: ai, usable: true})
	}
	return cons, allTerms, uncertain
}

// vec1SkipLikely is sqlite3ExprSkipCollateAndLikely.
func vec1SkipLikely(e Expr) Expr {
	for {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
			continue
		case FuncExpr:
			switch r33sFoldIdent(x.Name) {
			case "likely", "unlikely", "likelihood":
				if len(x.Args) > 0 && x.Over == nil {
					e = x.Args[0]
					continue
				}
			}
		}
		return e
	}
}

// vec1MentionsMeta reports whether e names a metadata column of this table,
// where a constraint vec1 might have consumed could hide.
func vec1MentionsMeta(e Expr, cols []columnInfo, scope string) bool {
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if e == nil || found {
			return
		}
		if ce, ok := e.(ColumnExpr); ok {
			if col, _, ok := vec1ColOf(ce, cols, scope); ok && col > 3 {
				found = true
			}
			return
		}
		walkExprOperands(e, walk)
	}
	walk(e)
	return found
}

// vec1OrToIn is exprAnalyzeOrTerm's case 2 for a virtual table's column: an
// OR of equalities between one column and values not naming the table becomes
// "column IN (values)".
func vec1OrToIn(e Expr, cols []columnInfo, scope string) (c vec1Cons, converted, uncertain bool) {
	var terms []Expr
	var split func(Expr)
	split = func(e Expr) {
		if b, ok := e.(BinaryExpr); ok && b.Op == "OR" && b.row == nil {
			split(b.L)
			split(b.R)
			return
		}
		terms = append(terms, vec1SkipLikely(e))
	}
	split(e)
	col := -2
	var vals []Expr
	for _, t := range terms {
		b, ok := t.(BinaryExpr)
		if !ok || (b.Op != "=" && b.Op != "==") || b.row != nil {
			return vec1Cons{}, false, false
		}
		l, lcoll, lok := vec1ColOf(b.L, cols, scope)
		r, rcoll, rok := vec1ColOf(b.R, cols, scope)
		var cc int
		var v Expr
		switch {
		case lok && !rok:
			cc, v = l, b.R
			if lcoll != "" || vec1CollOf(b.R) != "" {
				uncertain = true
			}
		case rok && !lok:
			cc, v = r, b.L
			if rcoll != "" || vec1CollOf(b.L) != "" {
				uncertain = true
			}
		default:
			return vec1Cons{}, false, false
		}
		if col != -2 && cc != col {
			return vec1Cons{}, false, false
		}
		col = cc
		// The value must not name the table, and its affinity must be none
		// (or the column's, none for a metadata column): a CAST, or a column
		// of another table, stops C's change. Only a literal or a parameter
		// is certain to pass.
		if vec1Refs(v, cols, scope) {
			uncertain = true
		} else if _, isLit := v.(LiteralExpr); !isLit {
			if _, isParam := v.(ParamExpr); !isParam {
				uncertain = true
			}
		}
		vals = append(vals, v)
	}
	if uncertain {
		return vec1Cons{}, false, col > 3
	}
	return vec1Cons{col: col, op: vec1CEQ, in: vals, usable: true}, true, false
}

// vec1LimitTerms is sqlite3WhereAddLimit for a vec1 item: whether LIMIT (and
// OFFSET) constraints are added. uncertain is a statement shaped in a way
// this does not model.
func vec1LimitTerms(stmt *SelectStmt, allTerms bool) (limit, offset, uncertain bool) {
	if stmt == nil || (stmt.Limit == nil && stmt.LimitParam == nil) {
		return false, false, false
	}
	if len(stmt.Compound) > 0 || selectHasWindow(stmt) || orderByHasWindow(stmt) {
		return false, false, true
	}
	if len(stmt.GroupBy) > 0 || stmt.Distinct || stmt.Having != nil || len(stmt.From) != 1 || stmt.From[0].Subquery != nil {
		return false, false, false
	}
	for _, c := range stmt.Columns {
		if !c.Star && containsAggregate(c.Expr) {
			return false, false, false
		}
	}
	for _, o := range stmt.OrderBy {
		if containsAggregate(o.Expr) {
			return false, false, false
		}
	}
	if !allTerms {
		return false, false, false
	}
	if len(stmt.OrderBy) > 0 {
		// vec1 refuses a LIMIT with an ORDER BY whichever way the planner
		// offers it.
		return false, false, false
	}
	hasOffset := stmt.Offset != nil || stmt.OffsetParam != nil
	return true, hasOffset, false
}

// vec1BestIndex is vec1BestIndexMethod over the constraints.
func vec1BestIndex(it FromItem, cols []columnInfo, scope string) *vec1PlanState {
	cons, allTerms, uncertain := vec1Constraints(it, cols, scope)
	limit, offset, limitUncertain := vec1LimitTerms(it.vtabSelect, allTerms)
	if offset {
		cons = append(cons, vec1Cons{col: -1, op: vec1COffset, conj: -1, usable: true})
	}
	if limit {
		cons = append(cons, vec1Cons{col: -1, op: vec1CLimit, conj: -1, usable: true})
	}
	ps := &vec1PlanState{cons: cons, cmd: -1, arg: -1, limit: -1, rowid: -1}
	for i, c := range cons {
		if c.op != vec1CEQ {
			continue
		}
		if c.usable {
			switch c.col {
			case 0:
				ps.cmd = i
			case 1:
				ps.arg = i
			case -1:
				ps.rowid = i
			}
		} else if c.col == 0 || c.col == 1 {
			ps.declineError = fmt.Errorf("%w: vec1: the query vector or parameters depend on another row source", errVDBEUnsupported)
			return ps
		}
	}
	if ps.cmd < 0 {
		return ps
	}
	ps.knn = true
	ps.rowid = -1
	if uncertain {
		ps.declineError = fmt.Errorf("%w: vec1: a WHERE term on a metadata column whose handling by the planner is not modelled", errVDBEUnsupported)
		return ps
	}
	if limitUncertain {
		ps.declineError = fmt.Errorf("%w: vec1: a LIMIT in a statement shape whose planning is not modelled", errVDBEUnsupported)
		return ps
	}
	argv := 1
	if ps.arg >= 0 {
		argv++
	}
	iLimit := -1
	for i, c := range cons {
		if !c.usable {
			continue
		}
		if c.col > 3 {
			if c.coll != "" && !strings.EqualFold(c.coll, "binary") {
				continue
			}
			var op byte
			switch c.op {
			case vec1CEQ:
				op = vec1OpEQ
				if c.in != nil {
					op = vec1OpIN
				}
			case vec1CLT:
				op = vec1OpLT
			case vec1CGT:
				op = vec1OpGT
			case vec1CLE:
				op = vec1OpLE
			case vec1CGE:
				op = vec1OpGE
			case vec1CIs:
				op = vec1OpIS
			case vec1CIsNull:
				op = vec1OpISNULL
			case vec1CIsNotNull:
				op = vec1OpNOTNULL
			}
			if op != 0 {
				ps.meta = append(ps.meta, i)
				ps.metaOp = append(ps.metaOp, op)
				argv++
			}
		}
		if c.op == vec1CLimit {
			iLimit = i
		}
	}
	if iLimit >= 0 && argv == len(cons)-1 {
		ps.limit = iLimit
	}
	for _, i := range append([]int{ps.cmd, ps.arg}, ps.meta...) {
		if i >= 0 && cons[i].conj >= 0 {
			ps.omit = append(ps.omit, cons[i].conj)
		}
	}
	// A conjunct consumed twice (an OR turned IN, BETWEEN's halves) is one
	// omission; one half consumed and the other not cannot be omitted.
	ps.omit = vec1OmitSet(cons, ps.omit, ps)
	return ps
}

// vec1OmitSet is the conjuncts every one of whose constraints was consumed:
// only those can leave the residual WHERE.
func vec1OmitSet(cons []vec1Cons, cand []int, ps *vec1PlanState) []int {
	consumed := map[int]bool{}
	for _, i := range append([]int{ps.cmd, ps.arg}, ps.meta...) {
		if i >= 0 {
			consumed[i] = true
		}
	}
	seen := map[int]bool{}
	var out []int
	for _, conj := range cand {
		if seen[conj] {
			continue
		}
		seen[conj] = true
		all := true
		for i, c := range cons {
			if c.conj == conj && !consumed[i] {
				all = false
			}
		}
		if !all {
			ps.declineError = fmt.Errorf("%w: vec1: a WHERE term only partly applied inside the index", errVDBEUnsupported)
			return nil
		}
		out = append(out, conj)
	}
	return out
}

// vec1ItemCols is the scope a vec1 FROM item resolves against.
func vec1ItemCols(st *vec1Store) []columnInfo { return vtabColumnInfos(st.cols) }

// vec1OmitCandidates is the compile-time half: the conjuncts a vec1 FROM item
// consumes, which leave the residual WHERE.
func vec1OmitCandidates(st *vec1Store, it FromItem, scope string) []int {
	ps := vec1BestIndex(it, vec1ItemCols(st), scope)
	if ps.declineError != nil {
		return nil
	}
	return ps.omit
}
