package engine

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
)

// planShapeHits counts the statements a shape's program ran, for a test.
var planShapeHits atomic.Int64

// Statements that differ only in the literals their outermost WHERE and ON
// compare with a column share one compiled program (#164). Those literals
// become hidden parameters, numbered after the statement's own, so the program
// reads them as it reads a bound "?" -- a program the plan cache already keeps
// across values -- and every statement of that shape binds its own literals to
// them.
//
// Only a literal that is the whole of one side of = == != <> < <= > >=, the
// other side reading a column, is made a parameter. Every other literal stays
// in the program as written, and in the cache key: a literal elsewhere can
// decide what is compiled (a result column's name, LIMIT, a LIKE pattern, an
// IN list). A schema with a partial index keeps every literal, since whether
// the index can serve a WHERE depends on the literal's value.

// shapeRecorder is the parser's record of the literals it made parameters.
type shapeRecorder struct {
	base   int     // the statement's own parameters; the hidden ones follow
	toks   []int   // the token each hidden parameter was, in parameter order
	vals   []Value // and its value
	active int     // inside the outermost select's WHERE, ON or result list
	// result is set inside the outermost select's result list, where a
	// literal that is a whole operand of + - * / % or of a sign is made a
	// parameter too, unless noResult. names are the columns such a literal
	// names (shapeName).
	result     int
	noResult   bool
	resultMade bool
	names      []shapeName
	// depth is the parser's coreDepth where literals are made parameters: 1,
	// the outermost select's core, or 0, an INSERT, UPDATE or DELETE.
	depth int
	// plain is set inside a write's VALUES row or SET, whose parameters no plan
	// reads: a value stored is a bound parameter's like any other. planned
	// is, per parameter, whether a plan may read the literal it stands for
	// (shapeLit).
	plain   int
	planned []bool
}

// made is how many hidden parameters the parser has made; 0 when it makes
// none.
func (s *shapeRecorder) made() int {
	if s == nil {
		return 0
	}
	return len(s.toks)
}

// planShape is what a program compiled for a shape needs to run another
// statement of it: where its hidden parameters start, which of the
// statement's literals they are ('1' in pattern, by literal ordinal), and
// which literal each one is, in their own order (order). The parser numbers
// them as it makes them, which is not always the literals' order: in
// "5 > (a = 7) + 4" the 7 is made first, inside the operand the 5 is
// compared with.
type planShape struct {
	base    int
	pattern string
	order   []int
	names   []shapeName
	// or is set when the statement has an OR, whose analysis compares
	// literals: its key records which are equal.
	or bool
}

// shapeName is a result column named by its spelling, which holds a literal
// made a parameter: col is its place in the result, from its first token and
// next the token after it. A statement of the shape spells it with its own
// literals, and the tokens around them are the shape's, so its name is read
// off its own text as the parser reads it (parseSelectList).
type shapeName struct {
	col, from, next int
}

// shapeColNames is names, a shape's program's column names, with each
// column a literal of sh names spelled as sql spells it.
func shapeColNames(names []string, sh *planShape, sql string, toks []token) []string {
	if len(sh.names) == 0 {
		return names
	}
	names = slices.Clone(names)
	for _, n := range sh.names {
		names[n.col] = shapeSpan(sql, toks, n.from, n.next)
	}
	return names
}

// shapeSpan is the text of toks[from:next] as a result column's name: up to
// the start of the token after it, trimmed (parseSelectList).
func shapeSpan(sql string, toks []token, from, next int) string {
	end := toks[next-1].End
	if t := toks[next]; t.Start >= end && t.Start <= len(sql) {
		end = t.Start
	}
	return strings.TrimSpace(sql[toks[from].Start:end])
}

// shapeClause marks the outermost select's WHERE or ON as the place literals
// are made parameters, until the returned func.
func (p *parser) shapeClause() (done func()) {
	if p.shape == nil || p.coreDepth != 1 || p.shape.depth != 1 {
		return func() {}
	}
	p.shape.active++
	return func() { p.shape.active-- }
}

// litTok is the index of the token e was parsed from at start, when e is that
// one literal token and nothing more, and one a shape may cut out; or -1.
func (p *parser) litTok(start int, e Expr) int {
	if p.shape == nil || p.pos != start+1 {
		return -1
	}
	if _, ok := e.(LiteralExpr); !ok || shapeClass(p.toks[start]) == 0 {
		return -1
	}
	return start
}

// shapeClass is the kind of literal t is, as a shape key records it, or 0 for
// a token no shape cuts out. The kind is part of the key, so nothing compiled
// from a literal's type is shared across types. An integer from -1 to 1 is
// never cut: the planner estimates "x = K" for one differently
// (whereIsSmallInt, sqlite3ExprIsInteger in whereLoopOutputAdjust). Nor is a
// number a sign changes as a literal and not as a value: a REAL zero ("-0.0"
// is -0.0, 0 - X is +0.0) and 2**63, which parseUnary folds with its "-"
// into SMALLEST_INT64 (or, in hex, "hex literal too big"). A string is cut:
// a TEXT bound is read when the statement runs, by the segment filters
// (segPlanPreds) as by the loop, unless it holds a NUL, which the planner's
// comparisons of operands cannot answer for (pcxConv). An integer past 31
// bits, or one a hex literal makes negative, is a class of its own: those
// comparisons answer for it differently.
func shapeClass(t token) byte {
	if t.kind == tkString {
		if strings.IndexByte(t.str, 0) >= 0 {
			return 0
		}
		return 's'
	}
	if t.kind != tkNumber {
		return 0
	}
	v, err := literalNumber(t.text)
	switch {
	case err != nil:
		return 0
	case v.Typ == Float:
		if v.F == 0 || v.F == 1<<63 {
			return 0
		}
		return 'r'
	case v.I >= -1 && v.I <= 1, v.I == math.MinInt64:
		return 0
	case v.I < 0:
		return 'n'
	case v.I > 0x7fffffff:
		return 'I'
	}
	return 'i'
}

// shapeArith is the arithmetic l op r with each literal operand made a hidden
// parameter inside the outermost result list: lt and rt are the operands'
// literal tokens, or -1. The value is computed when the row is, from a
// parameter as from a literal.
func (p *parser) shapeArith(op string, l Expr, lt int, r Expr, rt int) Expr {
	if s := p.shape; s != nil && s.result > 0 && p.coreDepth == s.depth {
		if lt >= 0 {
			l = s.param(lt, l.(LiteralExpr).Val)
		}
		if rt >= 0 {
			r = s.param(rt, r.(LiteralExpr).Val)
		}
	}
	return BinaryExpr{Op: op, L: l, R: r}
}

// shapeSign is x, the operand of a sign, made a hidden parameter inside the
// outermost result list when it is one literal token, xt. "-9223372036854775808"
// never reaches here: parseUnary folds it into one literal.
func (p *parser) shapeSign(x Expr, xt int) Expr {
	if s := p.shape; s != nil && s.result > 0 && p.coreDepth == s.depth && xt >= 0 {
		return s.param(xt, x.(LiteralExpr).Val)
	}
	return x
}

// shapeResultColumn marks the outermost select's result list as a place
// literals are made parameters, until the returned func.
func (p *parser) shapeResultColumn() (done func()) {
	if p.shape == nil || p.shape.noResult || p.coreDepth != 1 || p.shape.depth != 1 {
		return func() {}
	}
	p.shape.active++
	p.shape.result++
	return func() { p.shape.active--; p.shape.result-- }
}

// shapeCompare is rowOrBinary with a literal operand made a hidden parameter
// where the shape allows: lt and rt are the operands' literal tokens, or -1.
func (p *parser) shapeCompare(op string, l Expr, lt int, r Expr, rt int) Expr {
	if s := p.shape; s != nil && s.active > 0 && p.coreDepth == s.depth {
		switch {
		case lt >= 0 && rt < 0 && readsColumn(r):
			l = s.param(lt, l.(LiteralExpr).Val)
		case rt >= 0 && lt < 0 && readsColumn(l):
			r = s.param(rt, r.(LiteralExpr).Val)
		}
	}
	return rowOrBinary(op, l, r)
}

// shapeInList is list, the value list of an IN over x, with its numbers made
// hidden parameters where the shape allows, as shapeCompare makes one side
// of a comparison: x reads a column, and each number is a whole element. A
// string stays literal: native code matches a list of TEXTs as written
// (inListPlan.texts).
func (p *parser) shapeInList(x Expr, list []Expr) []Expr {
	s := p.shape
	if s == nil || s.active == 0 || p.coreDepth != s.depth || len(p.inLits) != len(list) || !readsColumn(x) {
		return list
	}
	var out []Expr
	for i, tok := range p.inLits {
		if tok < 0 || p.toks[tok].kind != tkNumber {
			continue
		}
		if out == nil {
			out = slices.Clone(list)
		}
		out[i] = s.param(tok, list[i].(LiteralExpr).Val)
	}
	if out == nil {
		return list
	}
	return out
}

// shapeBetween is the bounds of a BETWEEN over x with each number made a
// hidden parameter where the shape allows, as shapeInList makes a list's.
// The planner reads the BETWEEN as two comparisons with x.
func (p *parser) shapeBetween(x, lo, hi Expr) (Expr, Expr) {
	s := p.shape
	if s == nil || s.active == 0 || p.coreDepth != s.depth || !readsColumn(x) {
		return lo, hi
	}
	bounds := [2]Expr{lo, hi}
	for i, tok := range p.betweenLits {
		if tok >= 0 && p.toks[tok].kind == tkNumber {
			bounds[i] = s.param(tok, bounds[i].(LiteralExpr).Val)
		}
	}
	return bounds[0], bounds[1]
}

func (s *shapeRecorder) param(tok int, v Value) Expr {
	if s.result > 0 {
		s.resultMade = true
	}
	s.toks = append(s.toks, tok)
	s.vals = append(s.vals, v)
	s.planned = append(s.planned, s.plain == 0)
	if s.plain > 0 {
		return ParamExpr{Index: s.base + len(s.toks)}
	}
	return ParamExpr{Index: s.base + len(s.toks), shapeLit: &v}
}

// rewind forgets the parameters made after the first n, for a parser that
// goes back to read them again another way.
func (s *shapeRecorder) rewind(n int) {
	if s != nil {
		s.toks, s.vals, s.planned = s.toks[:n], s.vals[:n], s.planned[:n]
	}
}

// shapeWriteClause marks a write's own VALUES row or SET value (plain), or
// its WHERE, as the place literals are made parameters, until the returned
// func. In a value, a literal operand of arithmetic or of a sign is made one
// too, as in a select's result list.
func (p *parser) shapeWriteClause(plain bool) (done func()) {
	s := p.shape
	if s == nil || s.depth != 0 || p.coreDepth != 0 {
		return func() {}
	}
	s.active++
	if !plain {
		return func() { s.active-- }
	}
	s.result++
	s.plain++
	return func() { s.active--; s.result--; s.plain-- }
}

// shapeWriteValue is v, one whole value of a write's VALUES row or SET parsed
// from start, made a hidden parameter when it is one literal token.
func (p *parser) shapeWriteValue(start int, v Expr) Expr {
	s := p.shape
	if s == nil || s.depth != 0 || p.coreDepth != 0 {
		return v
	}
	tok := p.litTok(start, v)
	if tok < 0 {
		return v
	}
	s.plain++
	defer func() { s.plain-- }()
	return s.param(tok, v.(LiteralExpr).Val)
}

// shapeOperand is e as the planner's comparisons of operands see it: a
// shape's hidden parameter is the literal it stands for. Every statement
// sharing the program compares the same way, since the shape key records
// which of their literals are equal (shapeKey).
func shapeOperand(e Expr) Expr {
	if p, ok := e.(ParamExpr); ok && p.shapeLit != nil {
		return LiteralExpr{Val: *p.shapeLit}
	}
	return e
}

// readsColumn reports whether e names a column outside any subquery.
func readsColumn(e Expr) bool {
	found := false
	nfMapExpr(e, func(c ColumnExpr) Expr {
		found = true
		return c
	}, func(s *SelectStmt) *SelectStmt { return s })
	return found
}

// parseSelectShape parses sql, which ParseSelect accepts, with its shape's
// literals as hidden parameters. The recorder is nil when none is made one.
func parseSelectShape(sql string) (*SelectStmt, *shapeRecorder, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, nil, err
	}
	plain := newParser(sql, toks)
	if _, err := plain.parseSelectStmt(); err != nil {
		return nil, nil, err
	}
	p := newParser(sql, toks)
	p.shape = &shapeRecorder{base: plain.params.maxIndex, depth: 1}
	stmt, err := p.parseSelectStmt()
	if err != nil {
		return nil, nil, err
	}
	if p.shape.resultMade && !shapeResultsFree(stmt, toks) {
		// A result column a GROUP BY, HAVING, ORDER BY or window may match
		// is compared with them as written, and a "*" moves the columns the
		// names count.
		p = newParser(sql, toks)
		p.shape = &shapeRecorder{base: plain.params.maxIndex, depth: 1, noResult: true}
		if stmt, err = p.parseSelectStmt(); err != nil {
			return nil, nil, err
		}
	}
	if len(p.shape.toks) == 0 {
		return stmt, nil, nil
	}
	return stmt, p.shape, nil
}

// shapeResultsFree reports whether stmt's result columns are matched with
// nothing else in it: no GROUP BY, HAVING or ORDER BY, no window, and no
// "*" among them.
func shapeResultsFree(stmt *SelectStmt, toks []token) bool {
	if len(stmt.GroupBy) > 0 || stmt.Having != nil || len(stmt.OrderBy) > 0 {
		return false
	}
	for _, c := range stmt.Columns {
		if c.Star {
			return false
		}
	}
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && (strings.EqualFold(t.text, "OVER") || strings.EqualFold(t.text, "WINDOW")) {
			return false
		}
	}
	return true
}

// literalTokens are toks' literals a shape key cuts out (shapeClass).
func literalTokens(toks []token) []token {
	var lits []token
	for _, t := range toks {
		if shapeClass(t) != 0 {
			lits = append(lits, t)
		}
	}
	return lits
}

// shapeKey is sql with the literals pattern marks cut out and the others kept
// as written; with no pattern, every literal is cut out (the skeleton every
// statement of the shape has). The text between them is kept byte for byte: an
// unaliased result column is named by its spelling. Under or, a literal cut
// out is followed by the first other literal of the same value, and the first
// equal to it ignoring case: an OR's analysis is decided by which operands are
// equal (wherePlanOrToIn, wherePlanCombineDisjuncts), and the planner compares
// a hidden parameter as the literal it stands for (shapeOperand).
func shapeKey(sql string, lits []token, pattern string, or bool) string {
	var b strings.Builder
	b.Grow(len(sql) + 8)
	b.WriteString("\x00shape\x00")
	var same, fold []int
	if or && pattern != "" {
		same, fold = shapeEqualLiterals(lits)
	}
	at := 0
	for i, t := range lits {
		b.WriteString(sql[at:t.Start])
		if pattern == "" || pattern[i] == '1' {
			b.WriteByte('\x01')
			b.WriteByte(shapeClass(t))
			if same != nil {
				fmt.Fprintf(&b, "%d,%d,", same[i], fold[i])
			}
		} else {
			b.WriteByte('\x02')
			b.WriteString(sql[t.Start:t.End])
		}
		at = t.End
	}
	b.WriteString(sql[at:])
	return b.String()
}

// hasOr reports whether toks hold the keyword OR.
func hasOr(toks []token) bool {
	for _, t := range toks {
		if t.kind == tkIdent && !t.quoted && strings.EqualFold(t.text, "OR") {
			return true
		}
	}
	return false
}

// shapeEqualLiterals is, for each of lits, the first other literal of the
// same type and value (same) and the first equal to it with ASCII case folded
// (fold), as whereLiteralsMayCompareEqual compares them; -1 for none. Both nil
// when no two are equal.
func shapeEqualLiterals(lits []token) (same, fold []int) {
	if len(lits) < 2 {
		return nil, nil
	}
	keys := make([]string, len(lits))
	for i, t := range lits {
		keys[i] = string(shapeClass(t)) + t.str
		if t.kind == tkNumber {
			v, err := literalNumber(t.text)
			if err != nil {
				return nil, nil
			}
			keys[i] = fmt.Sprintf("%c%d %x", shapeClass(t), v.I, math.Float64bits(v.F))
		}
	}
	first := func(key func(string) string) ([]int, bool) {
		at := make(map[string][2]int, len(keys))
		for i, k := range keys {
			k = key(k)
			if e, ok := at[k]; !ok {
				at[k] = [2]int{i, -1}
			} else if e[1] < 0 {
				at[k] = [2]int{e[0], i}
			}
		}
		out, any := make([]int, len(keys)), false
		for i, k := range keys {
			e := at[key(k)]
			out[i] = e[0]
			if e[0] == i {
				out[i] = e[1]
			}
			any = any || out[i] >= 0
		}
		return out, any
	}
	same, s := first(func(k string) string { return k })
	fold, f := first(func(k string) string {
		b := []byte(k)
		for i, c := range b {
			b[i] = toLowerASCII(c)
		}
		return string(b)
	})
	if !s && !f {
		return nil, nil
	}
	return same, fold
}

// shapePattern is the pattern of the literals rec made parameters, among
// lits, and the literal each of its parameters is (planShape.order).
func shapePattern(toks []token, lits []token, rec *shapeRecorder) (string, []int) {
	pat := []byte(strings.Repeat("0", len(lits)))
	order := make([]int, len(rec.toks))
	for k, ti := range rec.toks {
		for i, t := range lits {
			if t.Start == toks[ti].Start {
				pat[i] = '1'
				order[k] = i
			}
		}
	}
	return string(pat), order
}

// shapeArgs is args followed by the values of the literals sh's parameters
// are, in their order, as the parser reads them; false when one does not
// read as it did.
func shapeArgs(args []Value, sh *planShape, lits []token) ([]Value, bool) {
	out := make([]Value, sh.base, sh.base+len(sh.order))
	copy(out, args)
	for _, i := range sh.order {
		if i >= len(lits) {
			return nil, false
		}
		switch t := lits[i]; t.kind {
		case tkNumber:
			v, err := literalNumber(t.text)
			if err != nil {
				return nil, false
			}
			out = append(out, v)
		case tkString:
			out = append(out, Value{Typ: Text, S: []byte(t.str)})
		default:
			return nil, false
		}
	}
	return out, true
}

// planShapesOffForTest compiles every statement with its literals, for a test
// comparing a shape's answers with a statement's own.
var planShapesOffForTest bool

// shapeIndexKey is where the plan cache keeps a skeleton's planShape: which
// of its literals a statement of the shape binds. Which ones the parser makes
// parameters depends on the statement's structure alone, which the skeleton
// fixes; keeping it in the cache's own map drops it whenever the cache is.
func shapeIndexKey(skeleton string) string { return "\x00shape-of" + skeleton }

// shapedPlan is the program compiled for sqlText's shape, the parameters
// that run it for sqlText, and its columns' names as sqlText spells them; or
// nil.
func (p *ReadOnlyPager) shapedPlan(sqlText string, args []Value) (*Program, []Value, []string) {
	prog, toks, lits := p.shapedProgram(sqlText)
	if prog == nil {
		return nil, nil, nil
	}
	params, ok := shapeArgs(args, prog.shape, lits)
	if !ok {
		return nil, nil, nil
	}
	planShapeHits.Add(1)
	return prog, params, shapeColNames(prog.ColNames, prog.shape, sqlText, toks)
}

// shapedProgram is the program compiled for sqlText's shape, and sqlText's
// tokens and literals, or nil.
func (p *ReadOnlyPager) shapedProgram(sqlText string) (*Program, []token, []token) {
	if planShapesOffForTest || p.sharedPlans == nil && p.planCache == nil || len(p.attachedReaders) > 0 {
		return nil, nil, nil
	}
	toks, err := lex(sqlText)
	if err != nil {
		return nil, nil, nil
	}
	lits := literalTokens(toks)
	if len(lits) == 0 {
		return nil, nil, nil
	}
	m := p.planCacheFor()
	idx := m[shapeIndexKey(shapeKey(sqlText, lits, "", false))]
	if idx == nil || idx.shape == nil || len(idx.shape.pattern) != len(lits) {
		return nil, nil, nil
	}
	prog := m[shapeKey(sqlText, lits, idx.shape.pattern, idx.shape.or)]
	if prog == nil || prog.shape == nil {
		return nil, nil, nil
	}
	return prog, toks, lits
}

// execShaped compiles and runs stmt, the parse of sqlText, as its shape's
// program when it has one, and keeps that program for the shape. ok is false
// when it has none: no literal its WHERE or ON compares with a column, or a
// statement this does not make shapes of.
func (p *ReadOnlyPager) execShaped(stmt *SelectStmt, sqlText string, args []Value) (cols []string, rows [][]Value, ok bool, err error) {
	if planShapesOffForTest || len(p.attachedReaders) > 0 || p.OwnsTempDatabase() || len(stmt.CTEs) > 0 || len(stmt.Compound) > 0 ||
		!p.fromPlainTables(stmt.From) {
		return nil, nil, false, nil
	}
	shaped, rec, perr := parseSelectShape(sqlText)
	if perr != nil || rec == nil {
		return nil, nil, false, nil
	}
	toks, lerr := lex(sqlText)
	if lerr != nil {
		return nil, nil, false, nil
	}
	if rec.madeString(toks) && p.fromHasCollatedColumn(stmt.From) {
		return nil, nil, false, nil
	}
	lits := literalTokens(toks)
	pattern, order := shapePattern(toks, lits, rec)
	sh := &planShape{base: rec.base, pattern: pattern, order: order, names: rec.names, or: hasOr(toks)}
	params, aok := shapeArgs(args, sh, lits)
	if !aok {
		return nil, nil, false, nil
	}
	key := shapeKey(sqlText, lits, sh.pattern, sh.or)
	cols, rows, err = p.execSelect(shaped, nil, params, key)
	m := p.planCacheFor()
	if prog := m[key]; prog != nil && shapeNamesSpelled(prog.ColNames, sh, sqlText, toks) {
		prog.shape = sh
		if ik := shapeIndexKey(shapeKey(sqlText, lits, "", false)); m[ik] == nil {
			// The shape's program, just stored, stays: the index names it.
			planCacheRoom(m, ik, key)
			m[ik] = &Program{shape: sh}
		}
	}
	return cols, rows, true, err
}

// shapeNamesSpelled reports whether each column sh renames for a statement
// of the shape is named by its spelling in names, as compiled for sqlText:
// only then is another statement's spelling its name.
func shapeNamesSpelled(names []string, sh *planShape, sqlText string, toks []token) bool {
	for _, n := range sh.names {
		if n.col >= len(names) || names[n.col] != shapeSpan(sqlText, toks, n.from, n.next) {
			return false
		}
	}
	return true
}

// madeString reports whether s made a string literal a parameter.
func (s *shapeRecorder) madeString(toks []token) bool {
	for _, ti := range s.toks {
		if toks[ti].kind == tkString {
			return true
		}
	}
	return false
}

// fromHasCollatedColumn reports whether a table from names has a column of a
// collation other than BINARY. A string compared with such a column stays a
// literal: the segment program compares the column with it as a service
// block, which a TEXT parameter would send to the loop instead, a slower
// way to the same answer.
func (p *ReadOnlyPager) fromHasCollatedColumn(from []FromItem) bool {
	for _, it := range from {
		tbl, err := p.resolveTable(it.Table)
		if err != nil {
			return true
		}
		for _, c := range tbl.cols {
			if c.Collation != "" && !strings.EqualFold(c.Collation, "BINARY") {
				return true
			}
		}
	}
	return false
}

// fromPlainTables reports whether from names ordinary tables of the main
// schema only, in a schema with no partial index: no view, subquery, virtual
// or table-valued source, whose constraints a module or a pushed-down WHERE
// may read at compile time.
func (p *ReadOnlyPager) fromPlainTables(from []FromItem) bool {
	rows, err := p.Schema()
	if err != nil || len(from) == 0 {
		return false
	}
	plain := map[string]bool{}
	for _, r := range rows {
		switch {
		case r.Type == "index" && indexSQLIsPartial(r.SQL):
			return false
		case r.Type == "table" && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(r.SQL)), "CREATE VIRTUAL"):
			plain[r33sFoldIdent(r.Name)] = true
		}
	}
	for _, it := range from {
		if it.Subquery != nil || it.TableFunc || it.GroupLen != 0 || len(it.NestedGroupSpan) != 0 ||
			it.Schema != "" && !strings.EqualFold(it.Schema, "main") || !plain[r33sFoldIdent(it.Table)] {
			return false
		}
	}
	return true
}

// maxShapeLiterals bounds the writes shapes are made of: past it a statement
// is a bulk load, whose two more parses on a shape's first run would cost
// more than the shape saves.
const maxShapeLiterals = 999

// shapedWrite is the program compiled for the shape of sqlText, an INSERT,
// UPDATE or DELETE, and the parameters that run it for sqlText; nil when
// sqlText makes no shape. A shape's first statement compiles its program,
// which writePlans keeps beside the programs it keeps by text, under the same
// guards.
//
// A write's shape makes parameters of the literals of its VALUES rows and of
// a SET value that is not a column list, which are bound parameters like any
// other, and of the literals its WHERE compares with a column, which a plan
// reads as the literals they stand for, under a select's rules
// (parseWriteShape).
func (db *DB) shapedWrite(sqlText string, args []Value) (*Program, []Value) {
	if planShapesOffForTest || len(db.attached) != 0 {
		return nil, nil
	}
	toks, err := lex(sqlText)
	if err != nil {
		return nil, nil
	}
	lits := literalTokens(toks)
	if len(lits) == 0 || len(lits) > maxShapeLiterals {
		return nil, nil
	}
	ik := shapeIndexKey(shapeKey(sqlText, lits, "", false))
	idx := db.writePlans[ik]
	if idx != nil {
		if idx.shape == nil || len(idx.shape.pattern) != len(lits) {
			return nil, nil
		}
		if prog := db.writePlans[shapeKey(sqlText, lits, idx.shape.pattern, idx.shape.or)]; prog != nil {
			params, ok := shapeArgs(args, prog.shape, lits)
			if !ok {
				return nil, nil
			}
			planShapeHits.Add(1)
			return prog, params
		}
	}
	// Whether a statement makes a shape depends on what its skeleton fixes and
	// on the schema, which drops writePlans when it changes, so a skeleton
	// that makes none is remembered as one.
	none := func() (*Program, []Value) {
		if idx == nil {
			db.writePlans[ik] = &Program{}
		}
		return nil, nil
	}
	stmt, sh := db.parseWriteShape(sqlText, toks, lits)
	if stmt == nil {
		return none()
	}
	params, ok := shapeArgs(args, sh, lits)
	if !ok {
		return none()
	}
	prog, keep, err := db.compileCacheable(sqlText, stmt)
	if err != nil || !keep {
		return none()
	}
	prog.shape = sh
	db.writePlans[shapeKey(sqlText, lits, sh.pattern, sh.or)] = prog
	if idx == nil {
		db.writePlans[ik] = &Program{shape: sh}
	}
	return prog, params
}

// parseWriteShape parses sqlText, an INSERT, UPDATE or DELETE lexed as toks,
// with its shape's literals as hidden parameters, and returns it with its
// shape; nil when it makes none. None is made of a statement with a WITH, a
// RETURNING (whose columns are named by their spelling) or an UPDATE's FROM,
// or whose target is not an ordinary table. A target with a partial index
// makes none, as a select's table makes none (fromPlainTables), and one with
// a collated column none from a string its WHERE compares
// (fromHasCollatedColumn).
func (db *DB) parseWriteShape(sqlText string, toks, lits []token) (any, *planShape) {
	kw, _ := writeDispatchKeywordAt(sqlText, toks)
	parse := func(p *parser) (stmt any, schema, table string, ok bool, err error) {
		switch kw {
		case "INSERT", "REPLACE":
			st, perr := parseInsertStmtTokens(p, false)
			if perr != nil {
				return nil, "", "", false, perr
			}
			defer func() { st.params = p.params.info() }()
			stmt, schema, table = st, st.schema, st.table
			ok = len(st.ctes) == 0 && !st.leadingWith && st.returning == nil
		case "UPDATE":
			st, perr := parseUpdateStmtTokens(p)
			if perr != nil {
				return nil, "", "", false, perr
			}
			defer func() { st.params = p.params.info() }()
			stmt, schema, table = st, st.schema, st.table
			ok = len(st.ctes) == 0 && st.returning == nil && st.from == nil
		case "DELETE":
			st, perr := parseDeleteStmtTokens(p)
			if perr != nil {
				return nil, "", "", false, perr
			}
			defer func() { st.params = p.params.info() }()
			stmt, schema, table = st, st.schema, st.table
			ok = len(st.ctes) == 0 && st.returning == nil
		default:
			return nil, "", "", false, nil
		}
		if p.peekIsPunct(";") {
			p.next()
		}
		if p.peek().kind != tkEOF {
			return nil, "", "", false, nil
		}
		return stmt, schema, table, ok, nil
	}
	plain := newParser(sqlText, toks)
	if _, _, _, ok, err := parse(plain); err != nil || !ok {
		return nil, nil
	}
	p := newParser(sqlText, toks)
	p.shape = &shapeRecorder{base: plain.params.maxIndex}
	stmt, schema, table, ok, err := parse(p)
	rec := p.shape
	if err != nil || !ok || len(rec.toks) == 0 || writableSchemaTarget(schema, table) {
		return nil, nil
	}
	tbl := db.findTableMetaIn(writeTargetScope(schema), table)
	if tbl == nil {
		return nil, nil
	}
	for _, ix := range db.indexes {
		if ix.where != nil && ix.isTemp == tbl.isTemp && equalFoldName(ix.table, tbl.name) {
			return nil, nil
		}
	}
	for i, ti := range rec.toks {
		if rec.planned[i] && toks[ti].kind == tkString && tableHasCollatedColumn(tbl) {
			return nil, nil
		}
	}
	pattern, order := shapePattern(toks, lits, rec)
	return stmt, &planShape{base: rec.base, pattern: pattern, order: order, or: hasOr(toks)}
}

// tableHasCollatedColumn reports whether a column of tbl has a collation other
// than BINARY.
func tableHasCollatedColumn(tbl *tableMeta) bool {
	for _, c := range tbl.cols {
		if c.Collation != "" && !strings.EqualFold(c.Collation, "BINARY") {
			return true
		}
	}
	return false
}
