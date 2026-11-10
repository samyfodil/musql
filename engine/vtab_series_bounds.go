package engine

// C's generate_series omits every constraint its xBestIndex consumes
// (series.c: "omit = !SQLITE_SERIES_CONSTRAINT_VERIFY || i>=3", and VERIFY is
// 0), and xFilter reads the value with sqlite3_value_numeric_type and
// sqlite3_value_int64. So "WHERE value = '5'" finds the row 5, though the
// column has no affinity and 5 = '5' is false; "value > '3'" and
// "value BETWEEN '3' AND '6'" read their bounds as numbers. This engine tests
// every WHERE conjunct again over the rows the module made, which for those
// bounds answered nothing (#138).
//
// So a bound C consumes is rewritten, here, into the number C reads it as,
// both in the WHERE tested again and in the copy the module is offered.
// Tested again, a consumed bound then holds on every row the module made from
// it, as an omitted one does; every other conjunct is tested as written, as C
// tests it. Which bound C consumes is worked out from the conjuncts the way
// seriesBestIndex walks its constraints: the WHERE's in order, the call's
// arguments after them; for value the last equality, or else the last lower
// and the last upper bound; for start, stop and step the last equality, which
// is an argument's whenever there is one. A bound whose usability is not
// known before the statement runs (one reading a column of this statement's
// own FROM, which may or may not reach the module) leaves its column's bounds
// as they are.
//
// Which constraint this engine's own BestIndex then takes does not change the
// answer: the WHERE is tested whole either way, and a bound tested as written
// against the series' INTEGERs only ever rejects rows.

const (
	seriesValueBoundFn = "\x00series_value_bound"
	seriesIntBoundFn   = "\x00series_int_bound"
)

// seriesValueBound is a value bound as seriesFilter reads it: a number when
// numeric affinity makes the text one (sqlite3_value_numeric_type), else the
// integer sqlite3_value_int64 reads; a BLOB is never given affinity.
func seriesValueBound(v Value) Value {
	switch v.Typ {
	case Null, Int, Float:
		return v
	case Text:
		if n := applyAffinityToValue(v, affNumeric); n.Typ == Int || n.Typ == Float {
			return n
		}
	}
	return Value{Typ: Int, I: seriesInt(v)}
}

// seriesIntBound is a start, stop or step bound as seriesFilter reads it:
// sqlite3_value_int64.
func seriesIntBound(v Value) Value {
	if v.Typ == Null {
		return v
	}
	return Value{Typ: Int, I: seriesInt(v)}
}

// seriesBoundTerm is one constraint seriesBestIndex walks: conj, the index in
// tvfWhere (-1 for a call argument), on column col.
type seriesBoundTerm struct {
	conj  int
	col   int
	op    VtabOp
	known bool // usable, or not, before the statement runs
	right bool // the column is the comparison's right operand
}

// seriesBound is a consumed conjunct's rewrite: the conversion its bound
// takes, and whether the column is the comparison's right operand.
type seriesBound struct {
	fn    string
	right bool
}

// seriesBoundConjuncts is stmt with every bound a generate_series in srcs
// consumes read as C reads it, and those sources' tvfWhere to match. It is
// stmt itself when there is none.
func seriesBoundConjuncts(p *ReadOnlyPager, stmt *SelectStmt, srcs []joinSource) *SelectStmt {
	if stmt == nil || stmt.Where == nil {
		return stmt
	}
	conj := splitTopLevelAnd(stmt.Where)
	own := map[string]bool{}
	for _, it := range stmt.From {
		if it.Alias != "" {
			own[r33sFoldIdent(it.Alias)] = true
		}
		if it.Table != "" {
			own[r33sFoldIdent(it.Table)] = true
		}
	}
	known := func(e Expr) bool {
		return exprRefsOnly(e, func(c ColumnExpr) bool { return c.Qualifier != "" && !own[r33sFoldIdent(c.Qualifier)] })
	}
	rewrite := map[int]seriesBound{}
	claimed := map[int]int{}
	for i := range srcs {
		it := srcs[i].vtabItem
		if it == nil || it.Subquery != nil || len(it.tvfWhere) == 0 {
			continue
		}
		mod, _, err := p.resolveVtabModuleForItem(*it)
		if err != nil {
			continue
		}
		if _, isSeries := mod.(seriesModule); !isSeries {
			continue
		}
		for j, b := range seriesConsumed(*it, known) {
			rewrite[j] = b
			claimed[j]++
		}
	}
	if len(rewrite) == 0 {
		return stmt
	}
	for j, n := range claimed {
		if n != 1 {
			delete(rewrite, j) // two series claim it: neither's bound is it
		}
	}
	// A BETWEEN's two bounds are virtual conjuncts after the real ones
	// (withVtabWhere); C tests the BETWEEN again unless both are consumed.
	between := map[int][2]string{}
	nv := len(conj)
	for j, e := range conj {
		b, ok := e.(BetweenExpr)
		if !ok || b.Not {
			continue
		}
		lo, hi := rewrite[nv], rewrite[nv+1]
		delete(rewrite, nv)
		delete(rewrite, nv+1)
		nv += 2
		if lo.fn != "" && hi.fn != "" {
			between[j] = [2]string{lo.fn, hi.fn}
		}
	}
	if len(rewrite) == 0 && len(between) == 0 {
		return stmt
	}
	bound := seriesBoundExpr
	out := make([]Expr, len(conj))
	copy(out, conj)
	for j, b := range rewrite {
		if j < len(out) {
			out[j] = seriesRewriteBound(out[j], b, bound)
		}
	}
	for j, fns := range between {
		b := out[j].(BetweenExpr)
		b.Lo, b.Hi = bound(fns[0], b.Lo), bound(fns[1], b.Hi)
		out[j] = b
	}
	where := out[0]
	for _, e := range out[1:] {
		where = BinaryExpr{Op: "AND", L: where, R: e}
	}
	s2 := *stmt
	s2.Where = where
	// Each series item offers its module the bounds as rewritten: its own
	// copy of the conjuncts, BETWEEN's bounds rebuilt from the rewritten one.
	tvf := append([]Expr(nil), out...)
	for _, e := range out {
		if b, ok := e.(BetweenExpr); ok && !b.Not {
			tvf = append(tvf, BinaryExpr{Op: ">=", L: b.X, R: b.Lo}, BinaryExpr{Op: "<=", L: b.X, R: b.Hi})
		}
	}
	for i := range srcs {
		if it := srcs[i].vtabItem; it != nil && len(it.tvfWhere) == len(tvf) {
			if mod, _, err := p.resolveVtabModuleForItem(*it); err == nil {
				if _, isSeries := mod.(seriesModule); isSeries {
					it.tvfWhere = tvf
				}
			}
		}
	}
	return &s2
}

// seriesBoundExpr is bound e read through conversion fn: a literal read here,
// so a constant bound stays a constant the series' native loop can take, and
// anything else through fn, at run time.
func seriesBoundExpr(fn string, e Expr) Expr {
	if u, isNeg := e.(UnaryExpr); isNeg && u.Op == "-" {
		if l, isLit := u.X.(LiteralExpr); isLit && (l.Val.Typ == Int || l.Val.Typ == Float) && fn == seriesValueBoundFn {
			return e // a number already
		}
	}
	lit, ok := e.(LiteralExpr)
	if !ok {
		return FuncExpr{Name: fn, Args: []Expr{e}}
	}
	if fn == seriesIntBoundFn {
		lit.Val = seriesIntBound(lit.Val)
	} else {
		lit.Val = seriesValueBound(lit.Val)
	}
	return lit
}

// seriesRewriteBound is conjunct e with its bound -- the side that is not the
// series column, or every element of an IN list -- wrapped in b's conversion.
func seriesRewriteBound(e Expr, b seriesBound, bound func(string, Expr) Expr) Expr {
	switch x := e.(type) {
	case BinaryExpr:
		if b.right {
			x.L = bound(b.fn, x.L)
		} else {
			x.R = bound(b.fn, x.R)
		}
		return x
	case InExpr:
		list := make([]Expr, len(x.List))
		for k, v := range x.List {
			list[k] = bound(b.fn, v)
		}
		x.List = list
		return x
	}
	return e
}

// seriesConsumed is the conjuncts of it's tvfWhere seriesBestIndex consumes,
// each with the conversion its bound takes. A column with a bound whose
// usability is not known is left out whole.
func seriesConsumed(it FromItem, known func(Expr) bool) map[int]seriesBound {
	vcols, _, err := seriesModule{}.Connect(nil)
	if err != nil {
		return nil
	}
	cols := make([]columnInfo, len(vcols))
	for i, c := range vcols {
		cols[i] = columnInfo{Name: c.Name, Hidden: c.Hidden}
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	var terms []seriesBoundTerm
	unknown := map[int]bool{}
	for j, e := range it.tvfWhere {
		col, op, rhs, ok := matchVtabConstraint(e, cols, name)
		var t seriesBoundTerm
		switch {
		case ok:
			// The constraint, seriesBestIndex's own; matchVtabConstraint
			// flipped the operator when the column is on the right.
			_, left := vtabColMatch(e.(BinaryExpr).L, cols, name)
			t = seriesBoundTerm{conj: j, col: col, op: op, known: known(rhs), right: !left}
		case seriesIsValueTerm(e, cols, name):
			// "value IS x" and "value IN (...)": C offers both as equalities.
			t = seriesBoundTerm{conj: j, col: seriesColValue, op: VtabEQ, known: true}
			switch x := e.(type) {
			case InExpr:
				for _, v := range x.List {
					t.known = t.known && known(v)
				}
			case BinaryExpr:
				t.known = known(x.R)
			}
		default:
			continue
		}
		if !t.known {
			unknown[col] = true
		}
		terms = append(terms, t)
	}
	hidden := hiddenColIndices(cols)
	for ai := range it.TableFuncArgs {
		if ai < len(hidden) {
			terms = append(terms, seriesBoundTerm{conj: -1, col: hidden[ai], op: VtabEQ, known: true})
		}
	}
	// seriesBestIndex's walk (series.c:771-824): the last usable equality on
	// value, else the last lower and the last upper bound; for an input, its
	// last equality.
	low, high := -1, -1
	eq := false
	input := map[int]int{}
	for k, t := range terms {
		switch t.col {
		case seriesColValue:
			switch t.op {
			case VtabEQ:
				eq, low, high = true, k, -1
			case VtabGE, VtabGT:
				if !eq {
					low = k
				}
			case VtabLE, VtabLT:
				if !eq {
					high = k
				}
			}
		case seriesColStart, seriesColStop, seriesColStep:
			if t.op == VtabEQ {
				input[t.col] = k
			}
		}
	}
	out := map[int]seriesBound{}
	take := func(k int, fn string) {
		if k >= 0 && terms[k].conj >= 0 && !unknown[terms[k].col] {
			out[terms[k].conj] = seriesBound{fn: fn, right: terms[k].right}
		}
	}
	take(low, seriesValueBoundFn)
	take(high, seriesValueBoundFn)
	for _, k := range input {
		take(k, seriesIntBoundFn)
	}
	return out
}

// seriesIsValueTerm reports whether e is "value IS x" or a "value IN (...)"
// list on it's value column.
func seriesIsValueTerm(e Expr, cols []columnInfo, name string) bool {
	switch x := e.(type) {
	case BinaryExpr:
		if x.Op != "IS" {
			return false
		}
		ci, ok := vtabColMatch(x.L, cols, name)
		return ok && ci == seriesColValue
	case InExpr:
		if x.Not || x.Sub != nil || len(x.List) == 0 {
			return false
		}
		ci, ok := vtabColMatch(x.X, cols, name)
		return ok && ci == seriesColValue
	}
	return false
}
