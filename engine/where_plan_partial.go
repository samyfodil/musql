package engine

import "strings"

// PARTIAL INDEXES: matching conditions against WHERE terms to determine usability.
// Uses pcx, a mirror of C Expr for exact tree comparison, since the AST normalizes
// differently. Every node mirrored exactly, or pcxUnknown; verdicts are three-valued:
// unknowns that could decide the index usability decline the plan, harmless ones don't.

type pcxOp uint8

const (
	pcxUnknown  pcxOp = iota
	pcxColumn         // TK_COLUMN of an item of this FROM clause, or of the index's own table (tab -1)
	pcxOtherCol       // a column of some other cursor: an enclosing query's, NEW/OLD, excluded
	pcxInteger
	pcxFloat
	pcxString
	pcxBlob
	pcxNull
	pcxTrueFalse
	pcxVariable
	pcxUMinus
	pcxUPlus
	pcxNot
	pcxBitNot
	pcxEq
	pcxNe
	pcxLt
	pcxLe
	pcxGt
	pcxGe
	pcxIs
	pcxIsNot
	pcxIsNull
	pcxNotNull
	pcxTruth
	pcxAnd
	pcxOr
	pcxPlus
	pcxMinus
	pcxStar
	pcxSlash
	pcxRem
	pcxConcat
	pcxBitAnd
	pcxBitOr
	pcxLShift
	pcxRShift
	pcxIn
	pcxBetween
	pcxCollate
	pcxFunction
	pcxCase
	pcxCast
	pcxSelect // TK_SELECT / TK_EXISTS: EP_xIsSelect
	pcxVector
)

// pcx mirrors one C Expr node. opaque means nothing below it is known; tok is
// the token for sqlite3ExprCompare strcmp(), known only when tokOK.
type pcx struct {
	op       pcxOp
	opaque   bool
	tab, col int // pcxColumn
	iv       int64
	intVal   bool // EP_IntValue
	tok      string
	tokOK    bool
	name     string // TK_COLLATE's collation, TK_FUNCTION's name
	truth    bool   // pcxTrueFalse: the value
	op2      pcxOp  // pcxTruth: pcxIs or pcxIsNot
	commuted bool   // EP_Commuted
	sel      bool   // EP_xIsSelect
	caseBase bool   // pcxCase: a CASE with a base expression
	l, r     *pcx
	list     []*pcx
}

var pcxUnknownNode = &pcx{op: pcxUnknown}

type tri int8

const (
	triNo tri = iota
	triYes
	triUnknown
)

// pcxColKind classifies a ColumnExpr for pcxConv.
type pcxColKind uint8

const (
	pcxColNone pcxColKind = iota
	pcxColHere
	pcxColOther
)

// pcxConv mirrors the AST as C Expr. col resolves a column reference: its FROM
// item and column, with rowid and PRIMARY KEY both -1. notNull reports whether a
// column cannot be NULL.
type pcxConv struct {
	col     func(ce ColumnExpr) (tab, col int, kind pcxColKind)
	notNull func(ce ColumnExpr) bool
}

func (cv *pcxConv) conv(e Expr) *pcx {
	switch x := e.(type) {
	case whereFixedCol:
		// EP_FixedCol: compare only the column, not the constant.
		return cv.conv(x.col)
	case ColumnExpr:
		tab, col, kind := cv.col(x)
		switch kind {
		case pcxColHere:
			return &pcx{op: pcxColumn, tab: tab, col: col}
		case pcxColOther:
			return &pcx{op: pcxOtherCol, opaque: true}
		}
		if x.FallbackLiteral != nil {
			// ExprIdToTrueFalse: spelling not kept, compares as unknown.
			return &pcx{op: pcxTrueFalse, opaque: true, truth: x.FallbackLiteral.I != 0}
		}
		return pcxUnknownNode
	case LiteralExpr:
		if x.aff != affNone || x.materialized || x.coll != "" || x.deferredErr != "" {
			return pcxUnknownNode
		}
		switch x.Val.Typ {
		case Null:
			return &pcx{op: pcxNull}
		case Int:
			// EP_IntValue: only non-negative values; negative is folded MinInt64 or hex.
			if x.Val.I >= 0 && x.Val.I <= 0x7fffffff {
				return &pcx{op: pcxInteger, iv: x.Val.I, intVal: true}
			}
			if x.Val.I > 0 {
				return &pcx{op: pcxInteger, opaque: true}
			}
			return pcxUnknownNode
		case Float:
			return &pcx{op: pcxFloat, opaque: true}
		case Text:
			if strings.IndexByte(string(x.Val.S), 0) >= 0 {
				return pcxUnknownNode // strcmp would stop at the NUL
			}
			return &pcx{op: pcxString, tok: string(x.Val.S), tokOK: true}
		case Blob:
			return &pcx{op: pcxBlob, opaque: true}
		}
		return pcxUnknownNode
	case ParamExpr:
		return &pcx{op: pcxVariable, opaque: true}
	case UnaryExpr:
		switch strings.ToUpper(x.Op) {
		case "-", "+":
			op := pcxUMinus
			if x.Op == "+" {
				op = pcxUPlus
			}
			in := cv.conv(x.X)
			if in.op == pcxUPlus && !in.opaque {
				// +/- over UPLUS rewrites in place, not stacked.
				c := *in
				c.op = op
				return &c
			}
			return &pcx{op: op, l: in}
		case "NOT":
			if _, lit := x.X.(LiteralExpr); lit {
				// foldEmptyIn's TK_TRUEFALSE and user's "NOT <literal>" appear identical.
				return pcxUnknownNode
			}
			return &pcx{op: pcxNot, l: cv.conv(x.X)}
		case "~":
			return &pcx{op: pcxBitNot, l: cv.conv(x.X)}
		}
		return pcxUnknownNode
	case BinaryExpr:
		op := strings.ToUpper(x.Op)
		if op == "IS" || op == "IS NOT" {
			if lit, ok := x.R.(LiteralExpr); ok && lit.Val.Typ == Null {
				// sqlite3PExprIs (parse.y:1418): "IS NULL" / "IS NOT NULL".
				return cv.isNull(x.L, op == "IS NOT")
			}
			l, r := cv.conv(x.L), cv.conv(x.R)
			pop := pcxIs
			if op == "IS NOT" {
				pop = pcxIsNot
			}
			if rr := cv.conv(whereSkipCollate(x.R)); rr.op == pcxTrueFalse {
				// "x IS TRUE" becomes TK_TRUTH with op2 TK_IS.
				return &pcx{op: pcxTruth, op2: pop, l: l, r: r}
			}
			return &pcx{op: pop, l: l, r: r}
		}
		var pop pcxOp
		switch op {
		case "=", "==":
			pop = pcxEq
		case "!=", "<>":
			pop = pcxNe
		case "<":
			pop = pcxLt
		case "<=":
			pop = pcxLe
		case ">":
			pop = pcxGt
		case ">=":
			pop = pcxGe
		case "AND":
			pop = pcxAnd
		case "OR":
			pop = pcxOr
		case "+":
			pop = pcxPlus
		case "-":
			pop = pcxMinus
		case "*":
			pop = pcxStar
		case "/":
			pop = pcxSlash
		case "%":
			pop = pcxRem
		case "||":
			pop = pcxConcat
		case "&":
			pop = pcxBitAnd
		case "|":
			pop = pcxBitOr
		case "<<":
			pop = pcxLShift
		case ">>":
			pop = pcxRShift
		case "->", "->>":
			return &pcx{op: pcxFunction, opaque: true, name: op}
		default:
			return pcxUnknownNode
		}
		return &pcx{op: pop, l: cv.conv(x.L), r: cv.conv(x.R)}
	case IsNullExpr:
		return cv.isNull(x.X, x.Not)
	case InExpr:
		var in *pcx
		switch {
		case x.Sub != nil:
			in = &pcx{op: pcxIn, sel: true, l: cv.conv(x.X)}
		case len(x.List) == 1:
			if _, row := x.X.(RowExpr); row {
				return pcxUnknownNode
			}
			switch pcxConstantKind(x.List[0]) {
			case triYes:
				// parse.y:1516: a one-element list of a constant is "x = +c".
				in = &pcx{op: pcxEq, l: cv.conv(x.X), r: &pcx{op: pcxUPlus, l: cv.conv(x.List[0])}}
			case triNo:
				in = &pcx{op: pcxIn, l: cv.conv(x.X), list: []*pcx{cv.conv(x.List[0])}}
			default:
				return pcxUnknownNode
			}
		case len(x.List) > 1:
			in = &pcx{op: pcxIn, l: cv.conv(x.X)}
			for _, a := range x.List {
				in.list = append(in.list, cv.conv(a))
			}
		default:
			return pcxUnknownNode
		}
		if x.Not {
			return &pcx{op: pcxNot, l: in}
		}
		return in
	case BetweenExpr:
		b := &pcx{op: pcxBetween, l: cv.conv(x.X), list: []*pcx{cv.conv(x.Lo), cv.conv(x.Hi)}}
		if x.Not {
			return &pcx{op: pcxNot, l: b}
		}
		return b
	case LikeExpr:
		return pcxNotIf(x.Not, &pcx{op: pcxFunction, opaque: true, name: "like"})
	case GlobExpr:
		return pcxNotIf(x.Not, &pcx{op: pcxFunction, opaque: true, name: "glob"})
	case MatchExpr:
		return pcxNotIf(x.Not, &pcx{op: pcxFunction, opaque: true, name: "match"})
	case CollateExpr:
		return &pcx{op: pcxCollate, name: x.Name, l: cv.conv(x.X)}
	case FuncExpr:
		f := &pcx{op: pcxFunction, name: x.Name}
		if x.Star || x.Distinct || x.Over != nil || x.Filter != nil {
			f.opaque = true
			return f
		}
		for _, a := range x.Args {
			f.list = append(f.list, cv.conv(a))
		}
		return f
	case CaseExpr:
		if x.isBool {
			// desugarIsBool's "X IS TRUE" / "X IS FALSE" -- "CASE WHEN X" or
			// "CASE WHEN NOT X" -- which C builds as TK_TRUTH, op2 TK_IS
			// (resolve.c). No iif(), though its CASE is spelled like one. Its
			// IS NOT forms are a NOT over this node, which answers every
			// question here as op2 TK_ISNOT does.
			if len(x.Whens) != 1 {
				return pcxUnknownNode
			}
			// A keyword that resolves to a column never made the node
			// TK_TRUTH (resolve.c:1427-1431): it is the ordinary TK_IS
			// compileCase compiles it as.
			if operand, kw, ok := isBoolParts(x); ok {
				isCol := false
				switch k := kw.(type) {
				case ColumnExpr:
					_, _, kind := cv.col(k)
					isCol = kind != pcxColNone
				case whereFixedCol:
					isCol = true
				default:
					return pcxUnknownNode
				}
				if isCol {
					return cv.conv(BinaryExpr{Op: "IS", L: operand, R: kw})
				}
			}
			operand, want := x.Whens[0].When, true
			if u, ok := operand.(UnaryExpr); ok && strings.EqualFold(u.Op, "NOT") {
				operand, want = u.X, false
			}
			return &pcx{op: pcxTruth, op2: pcxIs, l: cv.conv(operand), r: &pcx{op: pcxTrueFalse, opaque: true, truth: want}}
		}
		c := &pcx{op: pcxCase, caseBase: x.Base != nil}
		for _, w := range x.Whens {
			c.list = append(c.list, cv.conv(w.When), cv.conv(w.Then))
		}
		if x.Else != nil {
			c.list = append(c.list, cv.conv(x.Else))
		}
		return c
	case CastExpr:
		return &pcx{op: pcxCast, opaque: true}
	case SubqueryExpr:
		return &pcx{op: pcxSelect, sel: true, opaque: true}
	case ExistsExpr:
		return pcxNotIf(x.Not, &pcx{op: pcxSelect, sel: true, opaque: true})
	case RowExpr:
		return &pcx{op: pcxVector, opaque: true}
	}
	return pcxUnknownNode
}

func pcxNotIf(not bool, e *pcx) *pcx {
	if not {
		return &pcx{op: pcxNot, l: e}
	}
	return e
}

// isNull is sqlite3PExprIsNull (parse.y:1388) followed by resolve.c:1075's NOT
// NULL strength reduction: a literal operand folds to the integer 0 or 1 at
// parse time, and a column that cannot be NULL does in a WHERE clause -- which
// is declined as unknown, because whether it does depends on every enclosing
// name context being a WHERE (NC_Where) and on EP_CanBeNull.
func (cv *pcxConv) isNull(operand Expr, not bool) *pcx {
	p := operand
	for {
		u, ok := p.(UnaryExpr)
		if !ok || (u.Op != "+" && u.Op != "-") {
			break
		}
		p = u.X
	}
	if lit, ok := p.(LiteralExpr); ok && lit.Val.Typ != Null {
		v := int64(0)
		if not {
			v = 1
		}
		return &pcx{op: pcxInteger, iv: v, intVal: true}
	}
	switch c := p.(type) {
	case ColumnExpr:
		if cv.notNull(c) {
			return pcxUnknownNode
		}
	case whereFixedCol:
		if cv.notNull(c.col) {
			return pcxUnknownNode
		}
	}
	op := pcxIsNull
	if not {
		op = pcxNotNull
	}
	return &pcx{op: op, l: cv.conv(operand)}
}

// pcxConstantKind is sqlite3ExprIsConstant (expr.c) over the shapes parse.y's
// one-element IN rewrite meets: a literal, a parameter, and a +/- of one are
// constant; a column is not; anything else is not decided here.
func pcxConstantKind(e Expr) tri {
	switch x := e.(type) {
	case LiteralExpr:
		if x.aff != affNone || x.materialized || x.coll != "" {
			return triUnknown
		}
		return triYes
	case ParamExpr:
		return triYes
	case UnaryExpr:
		if x.Op == "+" || x.Op == "-" {
			return pcxConstantKind(x.X)
		}
	case ColumnExpr, whereFixedCol:
		return triNo
	}
	return triUnknown
}

// commute is exprCommute (whereexpr.c:116): the operands swap, a relational
// operator mirrors, and EP_Commuted toggles when the two orders disagree on
// the collating sequence.
func (e *pcx) commute(toggle bool) *pcx {
	if e == nil || e.op == pcxUnknown || e.opaque {
		return pcxUnknownNode
	}
	c := *e
	c.l, c.r = e.r, e.l
	switch e.op {
	case pcxGt:
		c.op = pcxLt
	case pcxLt:
		c.op = pcxGt
	case pcxGe:
		c.op = pcxLe
	case pcxLe:
		c.op = pcxGe
	}
	if toggle {
		c.commuted = !c.commuted
	}
	return &c
}

// pcxCompare is sqlite3ExprCompare (expr.c:6564): 0 identical, 1 differing
// only by a top-level COLLATE, 2 otherwise -- and -1 when this port cannot
// tell. b is always the partial index's condition, which pcxPartialOK has
// confined to shapes whose every node is known.
func pcxCompare(a, b *pcx, iTab int) int {
	if a == nil || b == nil {
		if a == b {
			return 0
		}
		return 2
	}
	if a.op == pcxUnknown || b.op == pcxUnknown {
		return -1
	}
	if a.op == pcxVariable {
		// exprCompareVariable (expr.c:6508): against a value, the answer is the
		// BOUND value's -- which also makes the statement re-prepare on every
		// rebind. Against anything else sqlite3ValueFromExpr has no value.
		if b.op == pcxColumn || b.op == pcxOtherCol {
			return 2
		}
		return -1
	}
	if a.intVal || b.intVal {
		if a.intVal && b.intVal && a.iv == b.iv {
			return 0
		}
		return 2
	}
	if a.op != b.op {
		if a.op == pcxCollate {
			if a.opaque {
				return -1
			}
			if c := pcxCompare(a.l, b, iTab); c < 0 {
				return -1
			} else if c < 2 {
				return 1
			}
		}
		if b.op == pcxCollate {
			if c := pcxCompare(a, b.l, iTab); c < 0 {
				return -1
			} else if c < 2 {
				return 1
			}
		}
		return 2
	}
	switch a.op {
	case pcxFunction:
		if !strings.EqualFold(a.name, b.name) {
			return 2
		}
	case pcxNull:
		return 0
	case pcxCollate:
		if !strings.EqualFold(a.name, b.name) {
			return 2
		}
	case pcxString:
		if !a.tokOK || !b.tokOK {
			return -1
		}
		if a.tok != b.tok {
			return 2
		}
	}
	if a.commuted != b.commuted {
		return 2
	}
	if a.sel || b.sel {
		return 2
	}
	if a.opaque || b.opaque {
		return -1
	}
	unknown := false
	child := func(x, y *pcx) bool {
		switch pcxCompare(x, y, iTab) {
		case 0:
			return false
		case -1:
			unknown = true
			return false
		}
		return true
	}
	if child(a.l, b.l) || child(a.r, b.r) {
		return 2
	}
	if (a.list == nil) != (b.list == nil) || len(a.list) != len(b.list) {
		return 2 // sqlite3ExprListCompare's 1, which the caller reads as "differ"
	}
	for i := range a.list {
		if child(a.list[i], b.list[i]) {
			return 2
		}
	}
	if unknown {
		return -1
	}
	switch a.op {
	case pcxColumn:
		if a.col != b.col {
			return 2
		}
		if a.tab != b.tab && a.tab != iTab {
			return 2
		}
	case pcxTruth:
		if a.op2 != b.op2 {
			return 2
		}
	}
	return 0
}

func triOr(a, b tri) tri {
	if a == triYes || b == triYes {
		return triYes
	}
	if a == triUnknown || b == triUnknown {
		return triUnknown
	}
	return triNo
}

// pcxImpliesNotNull is exprImpliesNotNull (expr.c:6698).
func pcxImpliesNotNull(p, nn *pcx, iTab int, seenNot bool) tri {
	if p == nil {
		return triUnknown
	}
	res := triNo
	switch pcxCompare(p, nn, iTab) {
	case 0:
		if nn.op != pcxNull {
			return triYes
		}
		return triNo
	case -1:
		res = triUnknown
	}
	var walk tri
	switch p.op {
	case pcxUnknown:
		return triUnknown
	case pcxIn:
		if seenNot && p.sel {
			walk = triNo
		} else {
			walk = pcxImpliesNotNull(p.l, nn, iTab, true)
		}
	case pcxBetween:
		if seenNot {
			walk = triNo
		} else if len(p.list) != 2 {
			walk = triUnknown
		} else {
			walk = triOr(triOr(pcxImpliesNotNull(p.list[0], nn, iTab, true),
				pcxImpliesNotNull(p.list[1], nn, iTab, true)),
				pcxImpliesNotNull(p.l, nn, iTab, true))
		}
	case pcxEq, pcxNe, pcxLt, pcxLe, pcxGt, pcxGe, pcxPlus, pcxMinus,
		pcxBitOr, pcxLShift, pcxRShift, pcxConcat:
		seenNot = true
		fallthrough
	case pcxStar, pcxRem, pcxBitAnd, pcxSlash:
		walk = triOr(pcxImpliesNotNull(p.r, nn, iTab, seenNot), pcxImpliesNotNull(p.l, nn, iTab, seenNot))
	case pcxCollate, pcxUPlus, pcxUMinus:
		walk = pcxImpliesNotNull(p.l, nn, iTab, seenNot)
	case pcxTruth:
		if seenNot || p.op2 != pcxIs {
			walk = triNo
		} else {
			walk = pcxImpliesNotNull(p.l, nn, iTab, true)
		}
	case pcxBitNot, pcxNot:
		walk = pcxImpliesNotNull(p.l, nn, iTab, true)
	default:
		walk = triNo
	}
	if walk != triNo && p.opaque {
		// A recursing arm over a node whose children are not known.
		walk = triUnknown
	}
	return triOr(res, walk)
}

// pcxIsNotTrue is sqlite3ExprIsNotTrue (expr.c:6774).
func pcxIsNotTrue(e *pcx) tri {
	if e == nil || e.op == pcxUnknown {
		return triUnknown
	}
	switch e.op {
	case pcxNull:
		return triYes
	case pcxTrueFalse:
		if !e.truth {
			return triYes
		}
		return triNo
	}
	v, is := pcxIsInteger(e)
	if is == triYes && v == 0 {
		return triYes
	}
	if is == triUnknown {
		return triUnknown
	}
	return triNo
}

// pcxIsInteger is sqlite3ExprIsInteger(p, &v, 0) (expr.c).
func pcxIsInteger(e *pcx) (int64, tri) {
	if e == nil || e.op == pcxUnknown {
		return 0, triUnknown
	}
	if e.intVal {
		return e.iv, triYes
	}
	switch e.op {
	case pcxUPlus:
		return pcxIsInteger(e.l)
	case pcxUMinus:
		v, is := pcxIsInteger(e.l)
		return -v, is
	case pcxInteger:
		return 0, triNo // a token past 32 bits
	}
	return 0, triNo
}

// pcxIsIIF is sqlite3ExprIsIIF (expr.c): iif(x,y), iif(x,y,<not true>), or the
// CASE WHEN x THEN y [ELSE <not true>] END it is shorthand for.
func pcxIsIIF(e *pcx) tri {
	switch e.op {
	case pcxFunction:
		if !strings.EqualFold(e.name, "iif") && !strings.EqualFold(e.name, "if") {
			return triNo
		}
		if e.opaque {
			return triUnknown
		}
	case pcxCase:
		if e.caseBase {
			return triNo
		}
	default:
		return triNo
	}
	switch len(e.list) {
	case 2:
		return triYes
	case 3:
		return pcxIsNotTrue(e.list[2])
	}
	return triNo
}

// pcxImplies is sqlite3ExprImpliesExpr (expr.c:6847).
func pcxImplies(e1, e2 *pcx, iTab int) tri {
	acc := triNo
	switch pcxCompare(e1, e2, iTab) {
	case 0:
		return triYes
	case -1:
		acc = triUnknown
	}
	if e2.op == pcxOr {
		acc = triOr(acc, triOr(pcxImplies(e1, e2.l, iTab), pcxImplies(e1, e2.r, iTab)))
		if acc == triYes {
			return triYes
		}
	}
	if e2.op == pcxNotNull {
		acc = triOr(acc, pcxImpliesNotNull(e1, e2.l, iTab, false))
		if acc == triYes {
			return triYes
		}
	}
	if e1.op == pcxUnknown {
		return triUnknown
	}
	switch pcxIsIIF(e1) {
	case triYes:
		if len(e1.list) == 0 {
			return triUnknown
		}
		r := pcxImplies(e1.list[0], e2, iTab)
		if r == triNo {
			return acc
		}
		return r
	case triUnknown:
		return triUnknown
	}
	return acc
}

// pcxPartialOK confines a partial index's condition to the shapes this file
// mirrors EXACTLY on the index's side of every comparison: comparisons, IS,
// IS [NOT] NULL, NOT and AND over the table's own columns and literals whose
// tokens sqlite3ExprCompare can be answered for. Anything else leaves the index
// to wherePlanIndexList's old verdict.
func pcxPartialOK(e *pcx) bool {
	if e == nil || e.opaque {
		return false
	}
	switch e.op {
	case pcxColumn, pcxNull:
		return true
	case pcxInteger:
		return e.intVal
	case pcxString:
		return e.tokOK
	case pcxUMinus, pcxUPlus, pcxCollate:
		return pcxPartialOK(e.l)
	case pcxNot, pcxIsNull, pcxNotNull:
		return e.l != nil && e.l.op == pcxColumn
	case pcxEq, pcxNe, pcxLt, pcxLe, pcxGt, pcxGe, pcxIs, pcxIsNot, pcxAnd:
		return pcxPartialOK(e.l) && pcxPartialOK(e.r)
	}
	return false
}

// whereUsablePartialIndex checks if a partial index is usable with the terms in the WhereClause.
func (b *wherePlanIdxBuild) whereUsablePartialIndex(iTab int, pw *pcx) tri {
	it := &b.items[iTab]
	// "if( jointype & JT_LTORJ ) return 0": the gate declines a RIGHT JOIN,
	// the only source of that flag, before this file runs.
	if pw.op == pcxAnd {
		l := b.whereUsablePartialIndex(iTab, pw.l)
		if l == triNo {
			return triNo
		}
		r := b.whereUsablePartialIndex(iTab, pw.r)
		if r == triNo {
			return triNo
		}
		if l == triUnknown || r == triUnknown {
			return triUnknown
		}
		return triYes
	}
	terms := b.terms
	if b.nWC > 0 {
		terms = terms[:b.nWC]
	}
	res := triNo
	for i := range terms {
		t := &terms[i]
		if t.outerON && t.onItem != iTab {
			continue
		}
		if it.outer && !t.outerON {
			continue
		}
		if t.vnull {
			continue // TERM_VNULL
		}
		if t.pcx == nil {
			res = triUnknown
			continue
		}
		yes := pcxImplies(t.pcx, pw, iTab)
		if yes == triNo {
			continue
		}
		// "&& !sqlite3ExprImpliesExpr(pParse, pExpr, pWhere, -1)": a term that
		// implies the condition without naming this table's columns.
		switch pcxImplies(t.pcx, pw, -1) {
		case triYes:
			continue
		case triUnknown:
			yes = triUnknown
		}
		if yes == triYes {
			return triYes
		}
		res = triUnknown
	}
	return res
}

// wherePartIdxMask is wherePartIdxExpr (where.c:3915) with pItem == 0: every
// "col = constant" / "col IS constant" conjunct of the condition under the
// BINARY collating sequence, whose column has an affinity of TEXT or above,
// is one an index scan reads back from the condition instead of the table.
// Like the C, it follows AND only down the right-hand side and then the one
// left operand -- "a AND b AND c" reaches c and "a AND b", never a or b.
func wherePartIdxMask(pw *pcx, cols []columnInfo, m uint64) uint64 {
	if pw.op == pcxAnd {
		m = wherePartIdxMask(pw.r, cols, m)
		pw = pw.l
	}
	if pw.op != pcxEq && pw.op != pcxIs {
		return m
	}
	l, r := pw.l, pw.r
	if l.op != pcxColumn {
		return m
	}
	// sqlite3ExprIsConstant(0, pRight), over pcxPartialOK's shapes; the first
	// COLLATE on the way down is the one sqlite3ExprCollSeq finds.
	coll := ""
	p := r
	for p.op == pcxCollate || p.op == pcxUPlus || p.op == pcxUMinus {
		if p.op == pcxCollate && coll == "" {
			coll = p.name
		}
		p = p.l
	}
	if p.op != pcxInteger && p.op != pcxString && p.op != pcxNull {
		return m
	}
	if l.col < 0 || l.col >= len(cols) {
		return m
	}
	// sqlite3ExprCompareCollSeq: an explicit COLLATE on the constant, else the
	// column's declared one.
	if coll == "" {
		coll = effectiveCollation(cols[l.col].Collation)
	}
	if !strings.EqualFold(coll, "BINARY") {
		return m
	}
	if cols[l.col].Aff == affNone { // "aff>=SQLITE_AFF_TEXT"
		return m
	}
	if l.col < 63 {
		m &^= uint64(1) << uint(l.col)
	}
	return m
}

// wherePlanPartialWhere mirrors a CREATE INDEX's WHERE over its own table, or
// answers nil when pcxPartialOK refuses it.
func wherePlanPartialWhere(tbl *resolvedTable, tableName string, where Expr) *pcx {
	cv := &pcxConv{
		col: func(ce ColumnExpr) (int, int, pcxColKind) {
			if ce.Schema != "" || ce.Qualifier != "" && !equalFoldName(ce.Qualifier, tableName) {
				return 0, 0, pcxColNone
			}
			for j := range tbl.cols {
				if equalFoldName(tbl.cols[j].Name, ce.Name) {
					if tbl.cols[j].IsRowidAlias {
						return -1, xnRowid, pcxColHere
					}
					return -1, j, pcxColHere
				}
			}
			return 0, 0, pcxColNone
		},
		// NC_PartIdx is not NC_Where: resolve.c:1075 never reduces the condition.
		notNull: func(ColumnExpr) bool { return false },
	}
	pw := cv.conv(where)
	if !pcxPartialOK(pw) {
		return nil
	}
	return pw
}

// wherePlanTermConv is pcxConv over one FROM clause, for a WHERE term.
func wherePlanTermConv(jts []joinedTable, scopes []tableScope) *pcxConv {
	return &pcxConv{
		col: func(ce ColumnExpr) (int, int, pcxColKind) {
			if tab, col, ok := wherePlanColumnRef(jts, scopes, ce); ok {
				return tab, col, pcxColHere
			}
			if _, _, _, _, bound := wherePlanBoundOuter(scopes, ce); bound {
				return 0, 0, pcxColOther
			}
			return 0, 0, pcxColNone
		},
		notNull: func(ce ColumnExpr) bool {
			tab, col, ok := wherePlanColumnRef(jts, scopes, ce)
			if !ok {
				return true // not known to be nullable
			}
			if col < 0 || tab >= len(jts) || jts[tab].tbl == nil || col >= len(jts[tab].tbl.cols) {
				return true
			}
			return jts[tab].tbl.cols[col].NotNull
		},
	}
}
