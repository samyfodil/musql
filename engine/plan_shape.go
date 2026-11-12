package engine

import (
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
	active int     // inside the outermost select's WHERE or ON
	// underOr is set when a hidden parameter is inside an OR: the OR's
	// analysis (wherePlanOrToIn, the MULTI_OR order) works from literal
	// operands, and declines where a parameter stands, so such a statement
	// is not shaped.
	underOr bool
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
// statement of it: where its hidden parameters start, and which of the
// statement's literals they are ('1' in pattern, by literal ordinal).
type planShape struct {
	base    int
	pattern string
}

// shapeClause marks the outermost select's WHERE or ON as the place literals
// are made parameters, until the returned func.
func (p *parser) shapeClause() (done func()) {
	if p.shape == nil || p.coreDepth != 1 {
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

// shapeClass is the kind of number t is, as a shape key records it, or 0 for
// a token no shape cuts out. The kind is part of the key, so nothing compiled
// from a literal's type is shared across types. An integer from -1 to 1 is
// never cut: the planner estimates "x = K" for one differently
// (whereIsSmallInt, sqlite3ExprIsInteger in whereLoopOutputAdjust). Nor is a
// string or a blob: the segment kernels compare a column with a text literal
// they read at compile time, and not with a parameter.
func shapeClass(t token) byte {
	if t.kind != tkNumber {
		return 0
	}
	v, err := literalNumber(t.text)
	switch {
	case err != nil:
		return 0
	case v.Typ == Float:
		return 'r'
	case v.I >= -1 && v.I <= 1:
		return 0
	}
	return 'i'
}

// shapeCompare is rowOrBinary with a literal operand made a hidden parameter
// where the shape allows: lt and rt are the operands' literal tokens, or -1.
func (p *parser) shapeCompare(op string, l Expr, lt int, r Expr, rt int) Expr {
	if s := p.shape; s != nil && s.active > 0 && p.coreDepth == 1 {
		switch {
		case lt >= 0 && rt < 0 && readsColumn(r):
			l = s.param(lt, l.(LiteralExpr).Val)
		case rt >= 0 && lt < 0 && readsColumn(l):
			r = s.param(rt, r.(LiteralExpr).Val)
		}
	}
	return rowOrBinary(op, l, r)
}

func (s *shapeRecorder) param(tok int, v Value) Expr {
	s.toks = append(s.toks, tok)
	s.vals = append(s.vals, v)
	return ParamExpr{Index: s.base + len(s.toks)}
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
	p.shape = &shapeRecorder{base: plain.params.maxIndex}
	stmt, err := p.parseSelectStmt()
	if err != nil {
		return nil, nil, err
	}
	if len(p.shape.toks) == 0 || p.shape.underOr {
		return stmt, nil, nil
	}
	return stmt, p.shape, nil
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
// unaliased result column is named by its spelling.
func shapeKey(sql string, lits []token, pattern string) string {
	var b strings.Builder
	b.Grow(len(sql) + 8)
	b.WriteString("\x00shape\x00")
	at := 0
	for i, t := range lits {
		b.WriteString(sql[at:t.Start])
		if pattern == "" || pattern[i] == '1' {
			b.WriteByte('\x01')
			b.WriteByte(shapeClass(t))
		} else {
			b.WriteByte('\x02')
			b.WriteString(sql[t.Start:t.End])
		}
		at = t.End
	}
	b.WriteString(sql[at:])
	return b.String()
}

// shapePattern is the pattern of the literals rec made parameters, among lits.
func shapePattern(toks []token, lits []token, rec *shapeRecorder) string {
	pat := []byte(strings.Repeat("0", len(lits)))
	for _, ti := range rec.toks {
		for i, t := range lits {
			if t.Start == toks[ti].Start {
				pat[i] = '1'
			}
		}
	}
	return string(pat)
}

// shapeArgs is args followed by the values of the literals pattern marks, as
// the parser reads them; false when one does not read as it did.
func shapeArgs(args []Value, sh *planShape, lits []token) ([]Value, bool) {
	out := make([]Value, sh.base, sh.base+strings.Count(sh.pattern, "1"))
	copy(out, args)
	for i, t := range lits {
		if sh.pattern[i] != '1' {
			continue
		}
		switch t.kind {
		case tkNumber:
			v, err := literalNumber(t.text)
			if err != nil {
				return nil, false
			}
			out = append(out, v)
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

// shapedPlan is the program compiled for sqlText's shape, and the parameters
// that run it for sqlText, or nil.
func (p *ReadOnlyPager) shapedPlan(sqlText string, args []Value) (*Program, []Value) {
	prog, lits := p.shapedProgram(sqlText)
	if prog == nil {
		return nil, nil
	}
	params, ok := shapeArgs(args, prog.shape, lits)
	if !ok {
		return nil, nil
	}
	planShapeHits.Add(1)
	return prog, params
}

// shapedProgram is the program compiled for sqlText's shape, and sqlText's
// literals, or nil.
func (p *ReadOnlyPager) shapedProgram(sqlText string) (*Program, []token) {
	if planShapesOffForTest || p.sharedPlans == nil && p.planCache == nil || len(p.attachedReaders) > 0 {
		return nil, nil
	}
	toks, err := lex(sqlText)
	if err != nil {
		return nil, nil
	}
	lits := literalTokens(toks)
	if len(lits) == 0 {
		return nil, nil
	}
	m := p.planCacheFor()
	idx := m[shapeIndexKey(shapeKey(sqlText, lits, ""))]
	if idx == nil || idx.shape == nil || len(idx.shape.pattern) != len(lits) {
		return nil, nil
	}
	prog := m[shapeKey(sqlText, lits, idx.shape.pattern)]
	if prog == nil || prog.shape == nil {
		return nil, nil
	}
	return prog, lits
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
	lits := literalTokens(toks)
	sh := &planShape{base: rec.base, pattern: shapePattern(toks, lits, rec)}
	params, aok := shapeArgs(args, sh, lits)
	if !aok {
		return nil, nil, false, nil
	}
	key := shapeKey(sqlText, lits, sh.pattern)
	cols, rows, err = p.execSelect(shaped, nil, params, key)
	m := p.planCacheFor()
	if prog := m[key]; prog != nil {
		prog.shape = sh
		if ik := shapeIndexKey(shapeKey(sqlText, lits, "")); m[ik] == nil && len(m) < planCacheMax {
			m[ik] = &Program{shape: sh}
		}
	}
	return cols, rows, true, err
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
