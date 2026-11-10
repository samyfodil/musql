package engine

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// C's generate_series omits every constraint its xBestIndex consumes
// (series.c: "omit = !SQLITE_SERIES_CONSTRAINT_VERIFY || i>=3", and VERIFY is
// 0), and xFilter reads the value with sqlite3_value_numeric_type and
// sqlite3_value_int64. So "WHERE value = '5'" finds the row 5, though the
// column has no affinity and 5 = '5' is false; "value > '3'" and
// "value BETWEEN '3' AND '6'" read their bounds as numbers. This engine tests
// every WHERE conjunct again over the rows the module made, which for those
// bounds answered nothing (#138).
//
// So a constraint C consumes is rewritten, here, into a test that holds on
// exactly the rows seriesFilter makes from it, and every other conjunct is
// tested as written, as C tests it. The module is offered the consumed
// constraints alone, so it narrows the series by nothing C does not.
//
// Which constraints C consumes is worked out the way seriesBestIndex walks
// them, in the order sqlite3WhereBegin builds them: the WHERE's conjuncts, the
// call's arguments, then the virtual terms exprAnalyze adds, walking the
// conjuncts from the last -- a BETWEEN's ">=" and "<=", an OR's combined
// comparison (whereCombineDisjuncts) and its IN (exprAnalyzeOrTerm). The
// rowid is the value. A column whose constraints C may or may not be able to
// use (a bound reading this statement's own FROM), or whose IN would run the
// series once per value, is left as it is.

const (
	seriesValueBoundFn = "\x00series_value_bound"
	seriesIntBoundFn   = "\x00series_int_bound"
	seriesStepBoundFn  = "\x00series_step_bound"
	seriesInFn         = "\x00series_in"
)

// seriesColRowid is the rowid, which seriesBestIndex constrains as value.
const seriesColRowid = -1

var seriesOpBit = [...]int{VtabEQ: seriesValueEq, VtabGT: seriesValueGT, VtabLE: seriesValueLE, VtabLT: seriesValueLT, VtabGE: seriesValueGE}

// seriesValueBound is the bound seriesFilter reads from "value op v", as an
// INTEGER: the value an equality selects, the smallest value a lower bound
// lets through, or the largest an upper bound does. NULL when it lets nothing
// through.
func seriesValueBound(v Value, op VtabOp) Value {
	if v.Typ == Null {
		return v
	}
	lo, hi, ok := seriesValueRange(seriesOpBit[op], func() Value { return v })
	switch {
	case !ok:
		return Value{}
	case op == VtabLE || op == VtabLT:
		return Value{Typ: Int, I: hi}
	}
	return Value{Typ: Int, I: lo}
}

// seriesIntBound is a start, stop or step bound as seriesFilter reads it:
// sqlite3_value_int64, and a step of 0 as 1.
func seriesIntBound(v Value, step bool) Value {
	if v.Typ == Null {
		return v
	}
	n := seriesInt(v)
	if step && n == 0 {
		n = 1
	}
	return Value{Typ: Int, I: n}
}

// seriesIn is "value IN (...)" as C runs a consumed one: the series once for
// every distinct value of the list (an ephemeral index, keyed with the
// column's affinity). It is 1 for a row one of them makes and 0 for one none
// does; a row two of them make, which C produces twice, is declined.
func seriesIn(args []Value) (Value, error) {
	aff, row := affinity(args[0].I), args[1]
	var seen []Value
	hits := 0
	for _, v := range args[2:] {
		if v.Typ == Null {
			continue
		}
		v = applyAffinityToValue(v, aff)
		if slices.ContainsFunc(seen, func(s Value) bool { return compareValues(s, v) == 0 }) {
			continue
		}
		seen = append(seen, v)
		if b := seriesValueBound(v, VtabEQ); b.Typ == Int && row.Typ == Int && b.I == row.I {
			hits++
		}
	}
	if hits > 1 {
		return Value{}, fmt.Errorf("%w: generate_series makes this row for two values of the IN list", errVDBEUnsupported)
	}
	return Value{Typ: Int, I: int64(hits)}, nil
}

type seriesTermKind int

const (
	seriesTermReal    seriesTermKind = iota
	seriesTermArg                    // a call argument
	seriesTermBetween                // a BETWEEN's ">=" (half 0) or "<=" (half 1)
	seriesTermOr                     // an OR's combined comparison, not its child
	seriesTermOrIn                   // an OR's IN, its only child
)

// seriesTerm is one constraint seriesBestIndex walks, on column col.
type seriesTerm struct {
	kind     seriesTermKind
	conj     int // the conjunct it is or comes from; -1 for an argument
	half     int
	col      int
	op       VtabOp
	is       bool // "IS", which only value takes as an equality
	commuted bool // the column is the right operand as written
	colX     Expr
	rhs      Expr
	list     []Expr // an IN of two values or more
	known    bool   // usable, or not, before the statement runs
	loop     bool   // an IN of start, stop or step, which C runs the series once for each value of
}

// seriesScope resolves a generate_series item's columns.
type seriesScope struct {
	cols  []columnInfo
	name  string
	rowid bool // an unqualified rowid is this item's
	known func(Expr) bool
}

func (s seriesScope) column(e Expr) (int, bool) {
	if ci, ok := vtabColMatch(e, s.cols, s.name); ok {
		return ci, true
	}
	c, ok := e.(ColumnExpr)
	if !ok || !isRowidAliasName(c.Name) {
		return 0, false
	}
	if c.Qualifier == "" {
		return seriesColRowid, s.rowid
	}
	return seriesColRowid, equalFoldName(c.Qualifier, s.name)
}

// comparison is e as a constraint, column on the left.
func (s seriesScope) comparison(e Expr) (seriesTerm, bool) {
	b, ok := e.(BinaryExpr)
	if !ok {
		return seriesTerm{}, false
	}
	op, ok := binaryToVtabOp(b.Op)
	is := false
	if !ok && b.Op == "IS" {
		if l, isLit := b.R.(LiteralExpr); isLit && l.Val.Typ == Null {
			return seriesTerm{}, false // "x IS NULL", TK_ISNULL
		}
		op, ok, is = VtabEQ, true, true
	}
	if !ok {
		return seriesTerm{}, false
	}
	if ci, ok := s.column(b.L); ok {
		return seriesTerm{col: ci, op: op, is: is, colX: b.L, rhs: b.R, known: s.known(b.R)}, true
	}
	if ci, ok := s.column(b.R); ok {
		return seriesTerm{col: ci, op: flipVtabOp(op), is: is, commuted: true, colX: b.R, rhs: b.L, known: s.known(b.L)}, true
	}
	return seriesTerm{}, false
}

// real is conjunct e as a constraint: a comparison or an IN.
func (s seriesScope) real(e Expr) (seriesTerm, bool) {
	in, ok := e.(InExpr)
	if !ok {
		return s.comparison(e)
	}
	ci, ok := s.column(in.X)
	if !ok || in.Not || in.Sub == nil && len(in.List) == 0 {
		return seriesTerm{}, false
	}
	t := seriesTerm{col: ci, op: VtabEQ, colX: in.X, known: in.Sub == nil}
	for _, v := range in.List {
		t.known = t.known && s.known(v)
	}
	if len(in.List) == 1 {
		t.rhs = in.List[0]
	} else {
		t.list = in.List
		t.loop = ci != seriesColValue && ci != seriesColRowid
	}
	return t, true
}

// terms is every constraint on the item, in C's order, or false for a WHERE
// whose virtual terms are not worked out here.
func (s seriesScope) terms(conj []Expr, args int) ([]seriesTerm, bool) {
	var terms []seriesTerm
	for j, e := range conj {
		if t, ok := s.real(e); ok {
			t.kind, t.conj = seriesTermReal, j
			terms = append(terms, t)
		}
	}
	hidden := hiddenColIndices(s.cols)
	for ai := 0; ai < args && ai < len(hidden); ai++ {
		terms = append(terms, seriesTerm{kind: seriesTermArg, conj: -1, col: hidden[ai], op: VtabEQ, known: true})
	}
	for j := len(conj) - 1; j >= 0; j-- {
		switch x := conj[j].(type) {
		case BetweenExpr:
			if ci, ok := s.column(x.X); ok && !x.Not {
				for h, rhs := range []Expr{x.Lo, x.Hi} {
					op := []VtabOp{VtabGE, VtabLE}[h]
					terms = append(terms, seriesTerm{kind: seriesTermBetween, conj: j, half: h, col: ci, op: op, colX: x.X, rhs: rhs, known: s.known(rhs)})
				}
			}
		case BinaryExpr:
			if x.Op != "OR" {
				continue
			}
			ts, ok := s.orTerms(j, x)
			if !ok {
				return nil, false
			}
			terms = append(terms, ts...)
		}
	}
	return terms, true
}

// orTerms is the virtual terms exprAnalyzeOrTerm adds for OR conjunct j: for
// two disjuncts, a comparison combined from each pair of their subterms, then
// an IN when every disjunct is "col = x" on one column.
func (s seriesScope) orTerms(j int, e Expr) ([]seriesTerm, bool) {
	ors := splitTopLevelOr(e)
	var out []seriesTerm
	if len(ors) == 2 {
		a, okA := s.subterms(ors[0])
		b, okB := s.subterms(ors[1])
		if !okA || !okB {
			return nil, false
		}
		for _, x := range a {
			for _, y := range b {
				if t, ok := seriesCombine(x, y); ok {
					t.kind, t.conj = seriesTermOr, j
					out = append(out, t)
				}
			}
		}
	}
	in := seriesTerm{kind: seriesTermOrIn, conj: j, op: VtabEQ, known: true}
	for i, d := range ors {
		t, ok := s.comparison(d)
		if !ok || t.op != VtabEQ || t.is || i > 0 && t.col != in.col {
			return out, true
		}
		// The IN would convert what a disjunct compares (whereexpr.c:891).
		if aff, ok := seriesExprAffinity(t.rhs); !ok {
			in.known = false
		} else if aff != affNone && aff != seriesColAffinity(t.col) {
			return out, true
		}
		in.col, in.colX, in.known = t.col, t.colX, in.known && t.known
		in.list = append(in.list, t.rhs)
	}
	in.loop = in.col != seriesColValue && in.col != seriesColRowid
	return append(out, in), true
}

// subterms is a disjunct's terms as whereNthSubterm walks them: its
// conjuncts, then the halves of each BETWEEN among them, from the last.
func (s seriesScope) subterms(d Expr) ([]seriesTerm, bool) {
	parts := splitTopLevelAnd(d)
	var out []seriesTerm
	for _, p := range parts {
		if b, ok := p.(BinaryExpr); ok && b.Op == "OR" {
			return nil, false
		}
		if t, ok := s.comparison(p); ok && !t.is {
			out = append(out, t)
		}
	}
	for k := len(parts) - 1; k >= 0; k-- {
		if b, ok := parts[k].(BetweenExpr); ok && !b.Not {
			if ci, ok := s.column(b.X); ok {
				out = append(out,
					seriesTerm{col: ci, op: VtabGE, colX: b.X, rhs: b.Lo, known: s.known(b.Lo)},
					seriesTerm{col: ci, op: VtabLE, colX: b.X, rhs: b.Hi, known: s.known(b.Hi)})
			}
		}
	}
	return out, true
}

// seriesCombine is whereCombineDisjuncts: "x op1 y OR x op2 y" as one
// comparison, when both are equalities or bounds on the same side.
func seriesCombine(a, b seriesTerm) (seriesTerm, bool) {
	if a.col != b.col || a.commuted != b.commuted || !exprEqual(a.rhs, b.rhs) {
		return seriesTerm{}, false
	}
	bit := func(op VtabOp) int { return [...]int{VtabEQ: 1, VtabLT: 2, VtabLE: 4, VtabGT: 8, VtabGE: 16}[op] }
	e := bit(a.op) | bit(b.op)
	if e&^7 != 0 && e&^25 != 0 {
		return seriesTerm{}, false
	}
	t := a
	if e&(e-1) != 0 {
		t.op = VtabGE
		if e&6 != 0 {
			t.op = VtabLE
		}
	}
	return t, true
}

func seriesColAffinity(col int) affinity {
	switch col {
	case seriesColValue:
		return affNone
	case seriesColRowid:
		return affInteger
	}
	return affNumeric
}

// seriesExprAffinity is sqlite3ExprAffinity for an expression whose affinity
// needs no schema, or false.
func seriesExprAffinity(e Expr) (affinity, bool) {
	switch x := e.(type) {
	case CollateExpr:
		return seriesExprAffinity(x.X)
	case LiteralExpr, CastExpr:
		return exprAffinity(nil, e), true
	case ColumnExpr, SubqueryExpr:
		return affNone, false
	}
	return affNone, true
}

// seriesTake is which of terms seriesBestIndex consumes (series.c:753-842):
// for value the last equality, or else the last lower and the last upper
// bound; for start, stop and step the last equality.
func seriesTake(terms []seriesTerm, skip func(seriesTerm) bool) []int {
	low, high := -1, -1
	eq := false
	input := map[int]int{}
	for k, t := range terms {
		if skip(t) {
			continue
		}
		switch t.col {
		case seriesColValue, seriesColRowid:
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
		default:
			if t.op == VtabEQ && !t.is {
				input[t.col] = k
			}
		}
	}
	var out []int
	for _, k := range append([]int{low, high}, slices.Collect(maps.Values(input))...) {
		if k >= 0 {
			out = append(out, k)
		}
	}
	return out
}

// bound is consumed constraint t as a test that holds on exactly the rows
// seriesFilter makes from it.
func (t seriesTerm) bound() Expr {
	switch {
	case t.col == seriesColStart || t.col == seriesColStop:
		return BinaryExpr{Op: "=", L: t.colX, R: seriesBoundExpr(seriesIntBoundFn, t.rhs, VtabEQ)}
	case t.col == seriesColStep:
		return BinaryExpr{Op: "=", L: t.colX, R: seriesBoundExpr(seriesStepBoundFn, t.rhs, VtabEQ)}
	case t.list != nil:
		args := append([]Expr{LiteralExpr{Val: Value{Typ: Int, I: int64(seriesColAffinity(t.col))}}, t.colX}, t.list...)
		return FuncExpr{Name: seriesInFn, Args: args}
	}
	op := "="
	switch t.op {
	case VtabGE, VtabGT:
		op = ">="
	case VtabLE, VtabLT:
		op = "<="
	}
	return BinaryExpr{Op: op, L: t.colX, R: seriesBoundExpr(seriesValueBoundFn, t.rhs, t.op)}
}

// seriesBoundExpr is bound e read through conversion fn: a literal read here,
// so a constant bound stays a constant the series' native loop can take, and
// anything else through fn, at run time.
func seriesBoundExpr(fn string, e Expr, op VtabOp) Expr {
	if v, ok := seriesLiteral(e); ok {
		switch fn {
		case seriesValueBoundFn:
			v = seriesValueBound(v, op)
		default:
			v = seriesIntBound(v, fn == seriesStepBoundFn)
		}
		return LiteralExpr{Val: v}
	}
	args := []Expr{e}
	if fn == seriesValueBoundFn {
		args = append(args, LiteralExpr{Val: Value{Typ: Int, I: int64(op)}})
	}
	return FuncExpr{Name: fn, Args: args}
}

func seriesLiteral(e Expr) (Value, bool) {
	switch x := e.(type) {
	case LiteralExpr:
		return x.Val, true
	case UnaryExpr:
		l, ok := x.X.(LiteralExpr)
		switch {
		case !ok || x.Op != "-":
		case l.Val.Typ == Float:
			return Value{Typ: Float, F: -l.Val.F}, true
		case l.Val.Typ == Int && l.Val.I != math.MinInt64:
			return Value{Typ: Int, I: -l.Val.I}, true
		}
	}
	return Value{}, false
}

var seriesTrue = LiteralExpr{Val: Value{Typ: Int, I: 1}}

// seriesPlan is what one generate_series item makes of the conjuncts.
type seriesPlan struct {
	it        *FromItem
	at        map[int]Expr    // conjunct j as the WHERE tests it
	halves    map[[2]int]Expr // a BETWEEN half the module is offered in its place
	blank     map[int]bool    // a conjunct the module is not offered
	blankHalf map[[2]int]bool
	extra     []Expr // consumed virtual terms the WHERE tests besides
}

// seriesItemPlan is the plan for it, or false when it changes nothing.
func seriesItemPlan(it *FromItem, conj []Expr, rowid bool, known func(Expr) bool) (seriesPlan, bool, error) {
	vcols, _, err := seriesModule{}.Connect(nil)
	if err != nil {
		return seriesPlan{}, false, nil
	}
	cols := make([]columnInfo, len(vcols))
	for i, c := range vcols {
		cols[i] = columnInfo{Name: c.Name, Hidden: c.Hidden}
	}
	name := it.Alias
	if name == "" {
		name = it.Table
	}
	s := seriesScope{cols: cols, name: name, rowid: rowid, known: known}
	terms, ok := s.terms(conj, len(it.TableFuncArgs))
	if !ok {
		return seriesPlan{}, false, nil
	}
	// The rowid is value's: a column with a constraint whose usability, or
	// whose rows, cannot be told here is left whole.
	slot := func(col int) int {
		if col == seriesColRowid {
			return seriesColValue
		}
		return col
	}
	unknown := map[int]bool{}
	for _, t := range terms {
		if !t.known {
			unknown[slot(t.col)] = true
		}
	}
	skip := func(t seriesTerm) bool { return unknown[slot(t.col)] }
	pl := seriesPlan{it: it, at: map[int]Expr{}, halves: map[[2]int]Expr{}, blank: map[int]bool{}, blankHalf: map[[2]int]bool{}}
	taken := map[int]bool{}
	for _, k := range seriesTake(terms, skip) {
		if terms[k].loop {
			return seriesPlan{}, false, fmt.Errorf("%w: generate_series runs once for each value of an IN on %s", errVDBEUnsupported, cols[terms[k].col].Name)
		}
		taken[k] = true
	}
	both := map[int]int{}
	for k, t := range terms {
		if t.kind == seriesTermArg || skip(t) {
			continue
		}
		if !taken[k] {
			switch t.kind {
			case seriesTermReal:
				pl.blank[t.conj] = true
			case seriesTermBetween:
				pl.blankHalf[[2]int{t.conj, t.half}] = true
			}
			continue
		}
		switch t.kind {
		case seriesTermReal, seriesTermOrIn:
			pl.at[t.conj] = t.bound()
		case seriesTermBetween:
			pl.halves[[2]int{t.conj, t.half}] = t.bound()
			both[t.conj]++
		case seriesTermOr:
			pl.extra = append(pl.extra, t.bound())
		}
	}
	// A BETWEEN is tested again unless both its halves are consumed.
	for j, n := range both {
		if n == 2 {
			b := conj[j].(BetweenExpr)
			b.Lo = pl.halves[[2]int{j, 0}].(BinaryExpr).R
			b.Hi = pl.halves[[2]int{j, 1}].(BinaryExpr).R
			pl.at[j] = b
			continue
		}
		for h := range 2 {
			if e, ok := pl.halves[[2]int{j, h}]; ok {
				pl.extra = append(pl.extra, e)
			}
		}
	}
	if len(pl.at) == 0 && len(pl.extra) == 0 && len(pl.blank) == 0 && len(pl.blankHalf) == 0 {
		return seriesPlan{}, false, nil
	}
	return pl, true, nil
}

// seriesBoundConjuncts is stmt with every constraint a generate_series in srcs
// consumes tested as C's series makes its rows, and those sources' tvfWhere
// offering the module the consumed constraints alone. It is stmt itself when
// there is none.
func seriesBoundConjuncts(p *ReadOnlyPager, stmt *SelectStmt, srcs []joinSource) (*SelectStmt, error) {
	if stmt == nil || stmt.Where == nil {
		return stmt, nil
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
	var betweens []int
	for j, e := range conj {
		if b, ok := e.(BetweenExpr); ok && !b.Not {
			betweens = append(betweens, j)
		}
	}
	var plans []seriesPlan
	at := map[int]Expr{}
	var extra []Expr
	for i := range srcs {
		it := srcs[i].vtabItem
		if it == nil || it.Subquery != nil || len(it.tvfWhere) != len(conj)+2*len(betweens) {
			continue
		}
		mod, _, err := p.resolveVtabModuleForItem(*it)
		if err != nil {
			continue
		}
		if _, isSeries := mod.(seriesModule); !isSeries {
			continue
		}
		pl, ok, err := seriesItemPlan(it, conj, len(stmt.From) == 1, known)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for j, e := range pl.at {
			if _, dup := at[j]; dup {
				return stmt, nil // two series consume it: neither's bound is it
			}
			at[j] = e
		}
		extra = append(extra, pl.extra...)
		plans = append(plans, pl)
	}
	if len(plans) == 0 {
		return stmt, nil
	}
	out := slices.Clone(conj)
	for j, e := range at {
		out[j] = e
	}
	out = append(out, extra...)
	where := out[0]
	for _, e := range out[1:] {
		where = BinaryExpr{Op: "AND", L: where, R: e}
	}
	s2 := *stmt
	s2.Where = where
	for _, pl := range plans {
		tvf := slices.Clone(out[:len(conj)])
		for j := range pl.blank {
			tvf[j] = seriesTrue
		}
		for _, j := range betweens {
			b := conj[j].(BetweenExpr)
			for h, half := range []Expr{BinaryExpr{Op: ">=", L: b.X, R: b.Lo}, BinaryExpr{Op: "<=", L: b.X, R: b.Hi}} {
				k := [2]int{j, h}
				switch e, ok := pl.halves[k]; {
				case ok:
					half = e
				case pl.blankHalf[k]:
					half = seriesTrue
				}
				tvf = append(tvf, half)
			}
		}
		pl.it.tvfWhere = append(tvf, extra...)
	}
	return &s2, nil
}
