// This file implements a recursive-descent parser for the SELECT grammar
// documented in query.go, plus a lightweight parser that recovers column
// names/declared-types/PRIMARY KEY info out of a CREATE TABLE statement's
// SQL text (as read from sqlite_schema) so query.go can compute type
// affinity and detect the INTEGER PRIMARY KEY rowid alias.
package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// parser walks a fixed token slice by index; it never backtracks except for
// the single-token lookahead used to disambiguate "NOT IN/BETWEEN/LIKE" from
// a leading unary NOT.
type parser struct {
	src    string
	toks   []token
	pos    int
	// nestedGroupSeq numbers each OUTERMOST non-leading parenthesized join
	// group this statement contains -- see FromItem.NestedGroupID.
	nestedGroupSeq int
	params *paramTracker // this statement's bound-parameter numbering state; see paramTracker's doc comment

	// sawIfNotExists records that the DDL prefix just parsed carried an
	// "IF NOT EXISTS" clause, so the caller can turn a name collision into a
	// no-op instead of an error (see parseCreateTableNamePrefix).
	sawIfNotExists bool

	// fromParen records whether the FROM clause currently being parsed used a
	// parenthesized term (a derived table or a parenthesized join/table). It is
	// set by parseFromElement and read (with save/restore across the parse, so
	// a nested subquery's own FROM doesn't leak out) by parseSelectCore, which
	// stores it into SelectStmt.FromParenthesized. See that field's doc comment.
	fromParen bool

	// fromNestedParen records whether the FROM clause currently being parsed
	// used a NESTED parenthesized join (a parenthesized join term opened while
	// already inside one). parenJoinDepth is the current parenthesized-join
	// nesting depth, tracked by parseFromElement. Read/reset by parseSelectCore
	// (same save/restore discipline as fromParen) into FromNestedParenJoin.
	fromNestedParen bool
	parenJoinDepth  int

	// groupRebuildSeq hands out FromItem.GroupRebuildID values: one per
	// parenthesized join group whose column list C SQLite rebuilds, so
	// two sibling groups in one FROM clause stay distinguishable. Never
	// reset -- it only has to be unique WITHIN one parse, and a subquery's
	// own groups sharing the counter is harmless (they are checked against
	// their own FROM list, never the enclosing one).
	groupRebuildSeq int

	// allowUnknownCollation suppresses parseCollate's rejection of an unimplemented
	// collation name; unknownCollations counts the ones let through. Set only by
	// parseSelectAllowingInertCollation (collate_inert.go), which keeps the result
	// only if every such name sits where nothing consumes it, and by
	// parseCreateViewStmt: C never checks a view's COLLATE names when storing it
	// (build.c:2990), only on first use (view_collate.go). Anywhere else an unknown
	// collation must be rejected.
	allowUnknownCollation bool
	unknownCollations     int

	// inTriggerBody is set while parsing the statements (and WHEN clause) of a
	// CREATE TRIGGER body (parseTriggerBody / the WHEN parse). It is the gate
	// that makes a "RAISE(...)" expression legal: outside a trigger body,
	// parsePrimary rejects RAISE with "RAISE() may only be used within a
	// trigger-program" -- exactly C SQLite's own prepare-time error, which
	// surfaces even for a statement that would match zero rows. Because a
	// subquery shares this same *parser, a RAISE buried in a top-level
	// statement's subquery is rejected here too, at parse time.
	inTriggerBody bool

	// raiseIgnoreDeclined is set while parsing a trigger body INSERT/UPDATE/DELETE:
	// RAISE(IGNORE) inside one abandons the whole trigger program, an unwind this
	// engine does not reproduce. parseRaiseExpr then sets sawUnsafeRaiseIgnore and
	// CreateTrigger declines. RAISE(IGNORE) in WHEN or a bare SELECT step is fine.
	raiseIgnoreDeclined  bool
	sawUnsafeRaiseIgnore bool

	// hasWith mirrors SQLite's Parse.bHasWith: set once a WITH clause has been
	// seen ANYWHERE earlier in this statement's text (its grammar action sits
	// on the first CTE's name, "withnm ::= nm"), and never cleared. It is a
	// whole-statement flag, not a per-SELECT one -- a WITH in an enclosing
	// statement or in an earlier subquery counts -- which is exactly what a
	// single *parser per statement already gives, since every subquery shares
	// it. sqlite3MultiValues reads it as its condition (a); see valuesFold.
	hasWith bool

	// schemaBody marks a parse of a STORED schema object's own text rather
	// than of a statement the caller wrote, which is SQLite's db->init.busy --
	// sqlite3MultiValues' condition (b). Set by parseCreateViewStmt, because a
	// view's SELECT is always re-parsed out of sqlite_schema (see valuesFold).
	schemaBody bool

	// r32nDQRecord records the byte span of each unqualified double-quoted column
	// reference in ColumnExpr.R32NDQSpan, offset by r32nDQBase so a sliced
	// sub-expression still reports offsets into the whole CREATE text. Set only by
	// the renameFixQuotes port (alter_write.go).
	r32nDQRecord bool
	r32nDQBase   int

	// inColumnDefault is set while parsing a column DEFAULT expression and switches
	// "X IS [NOT] TRUE/FALSE" to defaultClauseIsBool's desugar. Never reset: a
	// DEFAULT cannot contain a subquery or nested SELECT.
	inColumnDefault bool
}

func newParser(src string, toks []token) *parser {
	return &parser{src: src, toks: toks, params: &paramTracker{}}
}

// r32nParseSchemaViewSelect re-parses a stored "CREATE VIEW ... AS <select>"
// text's BODY with double-quoted-span recording on, so the renameFixQuotes
// port (alter_write.go) can splice the view's own stored text. The body starts
// after the first UNQUOTED "AS" at paren depth 0, which is unambiguous: the
// optional column-rename list is parenthesized, and a quoted name never lexes
// as a keyword. schemaBody mirrors parseCreateViewStmt's own setting -- a
// view's SELECT is always re-parsed out of sqlite_schema (see valuesFold).
func r32nParseSchemaViewSelect(sqlText string) (*SelectStmt, error) {
	toks, err := lex(sqlText)
	if err != nil {
		return nil, err
	}
	depth, at := 0, -1
	for i, t := range toks {
		if t.kind == tkPunct && t.text == "(" {
			depth++
		} else if t.kind == tkPunct && t.text == ")" {
			depth--
		} else if depth == 0 && t.kind == tkIdent && !t.quoted && t.upper() == "AS" {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("engine: CREATE VIEW: expected AS")
	}
	p := newParser(sqlText, toks)
	p.schemaBody = true
	p.r32nDQRecord = true
	p.pos = at + 1
	return p.parseSelectStmt()
}

// r32nDQSpan returns t's own byte span for ColumnExpr.R32NDQSpan: the zero
// span unless span recording is on AND t is the bare DOUBLE-QUOTED spelling,
// the only one SQLite's resolver can demote to a string literal (lookupName,
// resolve.c:721 -- the "x" spelling demotes; the bare, [x] and backtick
// spellings never do, since only the first sets EP_DblQuoted).
func (p *parser) r32nDQSpan(t token) byteSpan {
	if !p.r32nDQRecord || !t.dquoted {
		return byteSpan{}
	}
	return byteSpan{p.r32nDQBase + t.Start, p.r32nDQBase + t.End}
}

// paramTracker implements SQLite's bound-parameter numbering for one statement,
// subqueries included (they share the parser). maxIndex is the largest index
// assigned; names maps a named parameter (with sigil) to its first index, so
// repeats reuse it.
type paramTracker struct {
	maxIndex int
	names    map[string]int
}

// assignAnonymous implements a bare "?": SQLite assigns it one more than the
// largest parameter index already assigned anywhere to its left (by ANY
// form -- "?", "?NNN", or a named parameter), not a running count of bare
// "?"s seen -- so e.g. "?, ?5, ?" numbers as 1, 5, 6, not 1, 5, 2.
func (pt *paramTracker) assignAnonymous() int {
	pt.maxIndex++
	return pt.maxIndex
}

// assignExplicit implements "?NNN": the parameter's index is exactly NNN,
// but it still raises maxIndex if NNN is larger than anything seen so far
// (so a later bare "?" continues numbering from there) -- and does NOT raise
// it (leaving a "gap" in NNN-space with no assigned name) if NNN is smaller,
// e.g. "?1, ?5, ?" -- ?5 is index 5, the trailing bare ? is index 6, even
// though indices 2-4 were never explicitly named or assigned to anything.
func (pt *paramTracker) assignExplicit(n int) int {
	if n > pt.maxIndex {
		pt.maxIndex = n
	}
	return n
}

// assignNamed implements ":name"/"@name"/"$name": the first time a given
// exact name (sigil included -- verified against C SQLite/mattn that
// ":foo" and "@foo" are DISTINCT parameters, not unified) appears, it's
// assigned one more than the largest index assigned so far (exactly like a
// bare "?"); every later occurrence of the identical name resolves to that
// same index instead of consuming a new one.
func (pt *paramTracker) assignNamed(name string) int {
	if idx, ok := pt.names[name]; ok {
		return idx
	}
	if pt.names == nil {
		pt.names = make(map[string]int)
	}
	pt.maxIndex++
	pt.names[name] = pt.maxIndex
	return pt.maxIndex
}

// info snapshots pt into an exported ParamInfo (sql_ast.go), for a parsed
// statement to hand back to its caller.
func (pt *paramTracker) info() ParamInfo {
	out := ParamInfo{NumParams: pt.maxIndex}
	if len(pt.names) > 0 {
		out.ParamNames = make(map[string]int, len(pt.names))
		for k, v := range pt.names {
			out.ParamNames[k] = v
		}
	}
	return out
}

// sqliteMaxVariableNumber is SQLITE_LIMIT_VARIABLE_NUMBER's default value,
// 32766 (sqliteLimit.h) -- the ceiling expr.c:1350 names in its own error
// message, and the oracle's build uses the default.
const sqliteMaxVariableNumber = 32766

// makeParamExpr builds a ParamExpr from one already-lexed tkParam token's raw
// text (e.g. "?", "?7", ":name", "@name", "$name"), resolving its final
// index via p.params per the numbering rules documented on paramTracker.
func (p *parser) makeParamExpr(raw string) (Expr, error) {
	switch raw[0] {
	case '?':
		if len(raw) == 1 {
			return ParamExpr{Index: p.params.assignAnonymous()}, nil
		}
		n, err := strconv.Atoi(raw[1:])
		if err != nil || n < 1 || n > sqliteMaxVariableNumber {
			// expr.c:1349-1351's own wording and its own bounds: "bOk==0 ||
			// i<1 || i>db->aLimit[SQLITE_LIMIT_VARIABLE_NUMBER]" -- so a
			// number that does not parse, ?0, and one past the ceiling are all
			// the same message. The UPPER bound is not cosmetic: without it
			// "?32767" was accepted here and rejected by 3.53.3.
			return nil, fmt.Errorf("engine: variable number must be between ?1 and ?%d", sqliteMaxVariableNumber)
		}
		return ParamExpr{Index: p.params.assignExplicit(n)}, nil
	case ':', '@', '$':
		return ParamExpr{Index: p.params.assignNamed(raw), Name: raw}, nil
	default:
		return nil, fmt.Errorf("engine: internal: unrecognized parameter token %q", raw)
	}
}

func (p *parser) peek() token { return p.toks[p.pos] }

// peekAt looks n tokens ahead of the current position without consuming
// anything, clamped to the final (tkEOF) token if n runs past the end of the
// token slice -- toks is always terminated by a single tkEOF (see lex), so
// clamping is exactly what repeated next() calls at end-of-input would also
// observe.
func (p *parser) peekAt(n int) token {
	i := p.pos + n
	if i >= len(p.toks) {
		i = len(p.toks) - 1
	}
	return p.toks[i]
}

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) peekIsKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tkIdent && t.upper() == kw
}

func (p *parser) peekIsPunct(s string) bool {
	t := p.peek()
	return t.kind == tkPunct && t.text == s
}

// consumeKeyword consumes and reports whether the current token is the
// case-insensitive keyword kw.
func (p *parser) consumeKeyword(kw string) bool {
	if p.peekIsKeyword(kw) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectPunct(s string) error {
	if !p.peekIsPunct(s) {
		return fmt.Errorf("engine: expected %q, got %q", s, p.tokenDesc(p.peek()))
	}
	p.next()
	return nil
}

// eofTokenDesc is how tokenDesc renders the end-of-input token, and the one
// spelling incompleteIfAtEOF keys on -- see there.
const eofTokenDesc = "<end of input>"

func (p *parser) tokenDesc(t token) string {
	switch t.kind {
	case tkEOF:
		return eofTokenDesc
	case tkString:
		return "'" + t.str + "'"
	default:
		return t.text
	}
}

// ---- SELECT parsing ----

// ParseSelect parses a single SELECT statement (see query.go for the
// supported grammar). It reports a parse error for anything outside that
// grammar, including any trailing garbage after the statement (a single
// trailing ';' is tolerated).
func ParseSelect(sql string) (*SelectStmt, error) {
	stmt, err := parseSelectStatement(sql)
	return stmt, incompleteIfAtEOF(err)
}

// incompleteIfAtEOF rewrites a parse error at end of input to C's "incomplete
// input" (parse.y:44-51: any syntax error on the empty end token). Keyed on the
// rendered token, since tokenDesc is the one place tkEOF is rendered.
func incompleteIfAtEOF(err error) error {
	if err != nil && strings.Contains(err.Error(), eofTokenDesc) {
		return errors.New("engine: incomplete input")
	}
	return err
}

func parseSelectStatement(sql string) (*SelectStmt, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := newParser(sql, toks)
	stmt, err := p.parseSelectStmt()
	if err != nil {
		// A statement rejected ONLY for naming a collation this engine does not
		// implement may still be one C SQLite runs, because it resolves the
		// name lazily -- see collate_inert.go, which re-parses and serves it
		// only where nothing can consume the collation, and otherwise leaves
		// this error exactly as it is.
		if isUnknownCollationErr(err) {
			if s, ok := parseSelectAllowingInertCollation(sql); ok {
				return s, nil
			}
		}
		return nil, err
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if p.peek().kind != tkEOF {
		// C's own wording for a token it cannot continue from -- `near "(":
		// syntax error` over "SELECT * FROM (SELECT 1) AS x(a,b)", verified
		// against 3.53.3.
		return nil, fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(p.peek()))
	}
	// Params is only ever set on the outermost statement ParseSelect itself
	// parses -- see SelectStmt.Params' doc comment -- computed AFTER the
	// full parse (including every subquery reached via the same *parser/
	// *paramTracker) so it reflects every placeholder anywhere in the
	// statement's text, not just those in the first arm/clause parsed.
	stmt.Params = p.params.info()
	return stmt, nil
}

// parseWithClause parses "WITH [RECURSIVE] name [(cols)] AS (select) [, ...]";
// the caller has seen WITH. RECURSIVE is accepted but unused: recursion is
// detected structurally (detectRecursiveShape). A repeated name is "duplicate
// WITH table name: %s".
func (p *parser) parseWithClause() ([]CTEDef, error) {
	if !p.consumeKeyword("WITH") {
		return nil, fmt.Errorf("engine: internal: parseWithClause called without a leading WITH")
	}
	p.consumeKeyword("RECURSIVE")
	p.hasWith = true // SQLite's Parse.bHasWith; see the field's doc comment

	var defs []CTEDef
	for {
		t := p.peek()
		if t.kind != tkIdent {
			return nil, fmt.Errorf("engine: WITH: expected CTE name, got %q", p.tokenDesc(t))
		}
		p.next()
		def := CTEDef{Name: t.text}

		if p.peekIsPunct("(") {
			p.next()
			for {
				ct := p.peek()
				if ct.kind != tkIdent {
					return nil, fmt.Errorf("engine: WITH: expected column name, got %q", p.tokenDesc(ct))
				}
				p.next()
				def.ColNames = append(def.ColNames, ct.text)
				if p.peekIsPunct(",") {
					p.next()
					continue
				}
				break
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}

		if !p.consumeKeyword("AS") {
			return nil, fmt.Errorf("engine: WITH: expected AS, got %q", p.tokenDesc(p.peek()))
		}
		// "AS MATERIALIZED" is an optimization fence (select.c:7797): the CTE is never
		// flattened, which can change the answer:
		//
		//	WITH v(a) AS              (SELECT a FROM t LIMIT 2)
		//	  SELECT * FROM v ORDER BY 1 DESC   -->  3, 2   (flattened)
		//	WITH v(a) AS MATERIALIZED (SELECT a FROM t LIMIT 2)
		//	  SELECT * FROM v ORDER BY 1 DESC   -->  3, 1   (fenced)
		//
		// "NOT MATERIALIZED" behaves as the default.
		if p.consumeKeyword("MATERIALIZED") {
			def.Materialized = cteM10dYes
		} else if p.peekIsKeyword("NOT") {
			save := p.pos
			p.next() // NOT
			if p.consumeKeyword("MATERIALIZED") {
				def.Materialized = cteM10dNo
			} else {
				// A bare "NOT" not introducing "NOT MATERIALIZED": restore and
				// let the expectPunct("(") below report the real parse error.
				p.pos = save
			}
		}
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		sel, err := p.parseSelectStmt()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		def.Select = sel

		for _, existing := range defs {
			if equalFoldName(existing.Name, def.Name) {
				return nil, fmt.Errorf("engine: duplicate WITH table name: %s", def.Name)
			}
		}
		defs = append(defs, def)

		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	return defs, nil
}

// verbAfterLeadingWith parses a leading "WITH [RECURSIVE] <cte-list>" off toks
// far enough to find the statement verb it introduces (SELECT, INSERT, REPLACE,
// UPDATE or DELETE). ok=false if there is no WITH or it fails to parse; callers
// then report the real error on the full parse. Dispatch only: the CTEs are
// discarded and parsed again for real.
func verbAfterLeadingWith(sqlText string, toks []token) (verb string, ok bool) {
	verb, _, ok = verbAfterLeadingWithAt(sqlText, toks)
	return verb, ok
}

// verbAfterLeadingWithAt is verbAfterLeadingWith plus the INDEX in toks of the
// verb token it found, for the callers that go on to read the statement's own
// operands out of toks positionally (fkDMLTargetName, fkStatementReach) and so
// need to know where the statement proper begins. Nothing short of this parse
// finds that boundary: a CTE list carries nested parens and commas and may be
// RECURSIVE, so no fixed-token lookahead can skip it.
func verbAfterLeadingWithAt(sqlText string, toks []token) (verb string, at int, ok bool) {
	if len(toks) == 0 || toks[0].kind != tkIdent || toks[0].upper() != "WITH" {
		return "", 0, false
	}
	p := newParser(sqlText, toks)
	if _, err := p.parseWithClause(); err != nil {
		return "", 0, false
	}
	t := p.peek()
	if t.kind != tkIdent {
		return "", 0, false
	}
	return t.upper(), p.pos, true
}

// leadingStatementVerbUncached is LeadingStatementVerb without the cache: the
// keyword that decides dispatch, which for a WITH-prefixed statement is the verb
// after the CTE list (verbAfterLeadingWith). ok=false if the text does not lex
// or has no tokens.
func leadingStatementVerbUncached(sqlText string) (verb string, ok bool) {
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) == 0 || toks[0].kind != tkIdent {
		return "", false
	}
	kw := toks[0].upper()
	if kw != "WITH" {
		return kw, true
	}
	if v, ok := verbAfterLeadingWith(sqlText, toks); ok {
		return v, true
	}
	return kw, true
}

// parseSelectStmt parses a full SELECT: optional WITH, a core, compound arms,
// then the statement-level ORDER BY and LIMIT/OFFSET, which apply to the whole
// compound. Every subquery position parses through here, so WITH is accepted in
// all of them, as in C.
func (p *parser) parseSelectStmt() (*SelectStmt, error) {
	var ctes []CTEDef
	if p.peekIsKeyword("WITH") {
		var err error
		ctes, err = p.parseWithClause()
		if err != nil {
			return nil, err
		}
	}
	valuesCore := p.peekIsKeyword("VALUES")
	stmt, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	stmt.CTEs = ctes

	compoundWritten := false
	for {
		op, matched := p.tryParseCompoundOp()
		if !matched {
			break
		}
		compoundWritten = true
		arm, err := p.parseSelectCore()
		if err != nil {
			return nil, err
		}
		stmt.Compound = append(stmt.Compound, CompoundArm{Op: op, Stmt: arm})
	}

	// A bare VALUES (no compound operator written) takes no ORDER BY or LIMIT: it is
	// a select core, not a statement ("VALUES(1),(2) LIMIT 1" is a syntax error).
	// Test compoundWritten, not len(stmt.Compound): a multi-row VALUES desugars into
	// UNION ALL arms itself.
	if valuesCore && !compoundWritten {
		if p.peekIsKeyword("ORDER") {
			return nil, fmt.Errorf(`engine: near "ORDER": syntax error`)
		}
		if p.peekIsKeyword("LIMIT") {
			return nil, fmt.Errorf(`engine: near "LIMIT": syntax error`)
		}
	}

	if p.consumeKeyword("ORDER") {
		if !p.consumeKeyword("BY") {
			return nil, fmt.Errorf("engine: expected BY after ORDER")
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			desc := false
			if p.consumeKeyword("ASC") {
			} else if p.consumeKeyword("DESC") {
				desc = true
			}
			nulls := NullsDefault
			if p.consumeKeyword("NULLS") {
				if p.consumeKeyword("FIRST") {
					nulls = NullsFirst
				} else if p.consumeKeyword("LAST") {
					nulls = NullsLast
				} else {
					return nil, fmt.Errorf("engine: expected FIRST or LAST after NULLS")
				}
			}
			stmt.OrderBy = append(stmt.OrderBy, OrderTerm{Expr: e, Desc: desc, Nulls: nulls})
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
	}

	if p.consumeKeyword("LIMIT") {
		lit, param, err := p.parseLimitTerm()
		if err != nil {
			return nil, fmt.Errorf("engine: LIMIT: %w", err)
		}
		if p.peekIsPunct(",") {
			// SQLite's "LIMIT offset, count" alternate syntax.
			p.next()
			lit2, param2, err := p.parseLimitTerm()
			if err != nil {
				return nil, fmt.Errorf("engine: LIMIT: %w", err)
			}
			stmt.Offset, stmt.OffsetParam = lit, param
			stmt.Limit, stmt.LimitParam = lit2, param2
		} else {
			stmt.Limit, stmt.LimitParam = lit, param
			if p.consumeKeyword("OFFSET") {
				lit2, param2, err := p.parseLimitTerm()
				if err != nil {
					return nil, fmt.Errorf("engine: OFFSET: %w", err)
				}
				stmt.Offset, stmt.OffsetParam = lit2, param2
			}
		}
	}

	// ORDER BY or LIMIT before a compound operator is its own error
	// (parserDoubleLinkSelect, parse.y:555-559): "%s clause should come after %s not
	// before", ORDER BY winning over LIMIT, naming the following operator.
	if stmt.OrderBy != nil || stmt.Limit != nil || stmt.LimitParam != nil ||
		stmt.Offset != nil || stmt.OffsetParam != nil {
		if op, matched := p.tryParseCompoundOp(); matched {
			clause := "LIMIT"
			if stmt.OrderBy != nil {
				clause = "ORDER BY"
			}
			return nil, fmt.Errorf("engine: %s clause should come after %s not before", clause, op)
		}
	}

	return stmt, nil
}

// parseLimitTerm parses one LIMIT/OFFSET operand, which may be any expression
// ("LIMIT 1+1", "LIMIT (SELECT ...)"). A plain integer literal (optionally
// negated) is the fast path and stays a constant; anything else becomes a
// LimitParam expression resolved once before dispatch, as a bound "LIMIT ?"
// always was. Exactly one of lit/param is non-nil on success.
func (p *parser) parseLimitTerm() (lit *int64, param Expr, err error) {
	if p.peek().kind == tkParam {
		t := p.next()
		e, err := p.makeParamExpr(t.text)
		if err != nil {
			return nil, nil, err
		}
		return nil, e, nil
	}
	save := p.pos
	if n, lerr := p.parseSignedIntLiteral(); lerr == nil {
		// ...but only when the literal IS the whole term: "LIMIT 1+1" starts
		// with one and is not one, and must reach the expression path.
		if !p.peekStartsExprContinuation() {
			return &n, nil, nil
		}
	}
	p.pos = save
	e, eerr := p.parseExpr()
	if eerr != nil {
		return nil, nil, eerr
	}
	return nil, e, nil
}

// peekStartsExprContinuation reports whether the parser sits on a token that
// would continue an expression -- a binary operator or a "(" -- which is how
// parseLimitTerm tells "LIMIT 1" from "LIMIT 1+1" after the literal fast path
// has already consumed the 1. Anything it does not recognize ends the term,
// which is the safe direction: the literal is then used as-is, exactly as
// before, and a genuinely unparseable tail is reported by the caller's own
// trailing-input check.
func (p *parser) peekStartsExprContinuation() bool {
	t := p.peek()
	switch t.kind {
	case tkPunct:
		switch t.text {
		case "+", "-", "*", "/", "%", "|", "&", "<", ">", "=", "!", "~", "(", "<<", ">>", "<=", ">=", "==", "!=", "<>", "||":
			return true
		}
	case tkIdent:
		switch t.upper() {
		case "AND", "OR", "IS", "IN", "NOT", "LIKE", "GLOB", "MATCH", "REGEXP", "BETWEEN", "COLLATE":
			return true
		}
	}
	return false
}

// tryParseCompoundOp attempts to consume one compound-operator keyword
// sequence (UNION [ALL], INTERSECT, EXCEPT) that would introduce another
// select-core arm; it restores the parser position and reports matched ==
// false if the next tokens don't form one (e.g. WHERE/ORDER/LIMIT/
// end-of-statement, which instead terminate the statement).
func (p *parser) tryParseCompoundOp() (op CompoundOp, matched bool) {
	save := p.pos
	switch {
	case p.consumeKeyword("UNION"):
		if p.consumeKeyword("ALL") {
			return "UNION ALL", true
		}
		return "UNION", true
	case p.consumeKeyword("INTERSECT"):
		return "INTERSECT", true
	case p.consumeKeyword("EXCEPT"):
		return "EXCEPT", true
	}
	p.pos = save
	return "", false
}

// parseSelectCore parses one select-core: "SELECT [DISTINCT|ALL]
// <select-list> [FROM ...] [WHERE ...] [GROUP BY ... [HAVING ...]]". Unlike
// parseSelectStmt, it never consumes ORDER BY/LIMIT/OFFSET or a compound
// operator -- those are parsed once, by parseSelectStmt, for the statement
// as a whole (see SelectStmt.Compound's doc comment). This is also what a
// compound arm's own select-core parse calls, so "SELECT a FROM t1 UNION
// SELECT b FROM t2 ORDER BY 1" parses ORDER BY as belonging to the whole
// compound rather than being swallowed (or rejected) by the second arm.
func (p *parser) parseSelectCore() (*SelectStmt, error) {
	// "VALUES (expr, ...) [, (expr, ...) ...]" is SQLite's row-constructor-
	// as-SELECT shorthand -- a distinct select-core alternative to SELECT,
	// not a SELECT variant -- so it's checked here, before requiring the
	// SELECT keyword. See parseValuesSelectCore's own doc comment for why
	// this single-tuple case ("VALUES(0) UNION ALL SELECT ...", the mined
	// TCL corpus's own extremely common recursive-CTE-seed idiom) needs no
	// further special-casing anywhere else in this package at all.
	if p.peekIsKeyword("VALUES") {
		return p.parseValuesSelectCore()
	}
	if !p.consumeKeyword("SELECT") {
		return nil, fmt.Errorf("engine: expected SELECT, got %q", p.tokenDesc(p.peek()))
	}
	stmt := &SelectStmt{}

	// DISTINCT and ALL are mutually exclusive (verified against the reference
	// engine: "SELECT DISTINCT ALL x ..." is a syntax error); ALL is the
	// default, no-op behavior, so nothing needs to be recorded for it.
	if p.consumeKeyword("DISTINCT") {
		stmt.Distinct = true
		if p.peekIsKeyword("ALL") {
			return nil, fmt.Errorf("engine: unexpected ALL after DISTINCT")
		}
	} else {
		p.consumeKeyword("ALL")
	}

	cols, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	stmt.Columns = cols

	// FROM is optional: "SELECT <exprs>" with no table yields a single row of
	// constant expressions (e.g. SELECT 1 IN (2,3)).
	if p.consumeKeyword("FROM") {
		// Save/restore fromParen around this clause's parse so a nested
		// subquery's own parenthesized FROM (parsed via a reentrant
		// parseSelectStmt inside parseFromClause) doesn't leak into this
		// statement's FromParenthesized, and vice-versa.
		savedParen, savedNested := p.fromParen, p.fromNestedParen
		p.fromParen, p.fromNestedParen = false, false
		items, err := p.parseFromClause(true) // the statement's own FROM clause always starts leading
		if err != nil {
			return nil, err
		}
		stmt.From = items
		markRebuiltGroupStars(stmt.Columns, stmt.From)
		stmt.FromParenthesized = p.fromParen
		stmt.FromNestedParenJoin = p.fromNestedParen
		p.fromParen, p.fromNestedParen = savedParen, savedNested
	}

	if p.consumeKeyword("WHERE") {
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Where = w
	}

	if p.consumeKeyword("GROUP") {
		if !p.consumeKeyword("BY") {
			return nil, fmt.Errorf("engine: expected BY after GROUP")
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			stmt.GroupBy = append(stmt.GroupBy, e)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
	}

	// HAVING is parsed independently of GROUP BY, as in C's grammar: on a
	// whole-table aggregate it filters the one implicit group. "HAVING clause on a
	// non-aggregate query" is a resolve-time error (requireAggregateForHaving), so a
	// view with one is accepted at CREATE and fails on use, as in C.
	if p.consumeKeyword("HAVING") {
		h, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		stmt.Having = h
	}

	// Top-level "WINDOW <name> AS (<spec>) [, ...]" clause, following HAVING
	// (SQLite's grammar). A window function's "OVER <name>" resolves against
	// these definitions -- see sql_window.go.
	if p.consumeKeyword("WINDOW") {
		for {
			nt := p.next()
			if nt.kind != tkIdent {
				return nil, fmt.Errorf("engine: expected window name after WINDOW")
			}
			if !p.consumeKeyword("AS") {
				return nil, fmt.Errorf("engine: expected AS in WINDOW clause")
			}
			spec, err := p.parseWindowSpec()
			if err != nil {
				return nil, err
			}
			stmt.Windows = append(stmt.Windows, NamedWindow{Name: nt.text, Spec: spec})
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
	}

	return stmt, nil
}

// peekOpensOverClause reports whether OVER opens an over-clause rather than
// being the identifier "over" in alias position. The grammar is
// "OVER LP window RP | OVER nm", so it is the keyword only before "(" or a name;
// a reserved word cannot be an nm:
//
//	SELECT sum(x) over FROM over               -- 9,   "over" is the ALIAS
//	SELECT sum(x) OVER, count(*) FROM over     -- 9|3, ditto (altertab3.test)
//	SELECT sum(x) "over" FROM over             -- 9,   quoted: never the keyword
//	SELECT sum(x) over over FROM over WINDOW over AS ()   -- 9,9,9, the CLAUSE
func (p *parser) peekOpensOverClause() bool {
	t := p.peek()
	if t.kind != tkIdent || t.quoted || t.upper() != "OVER" {
		return false
	}
	nt := p.peekAt(1)
	if nt.kind == tkPunct && nt.text == "(" {
		return true
	}
	name, ok := objectNameToken(nt)
	if !ok || name == "" {
		return false
	}
	return nt.kind != tkIdent || nt.quoted || !nonIdentifierKeywords[nt.upper()]
}

// parseWindowSpec parses a window definition after the OVER keyword or a
// WINDOW-clause "AS": either a bare "<windowname>" reference or a parenthesized
// "( [<basename>] [PARTITION BY ...] [ORDER BY ...] [frame] )".
// parseSortList parses parse.y's sortlist -- "expr [ASC|DESC] [NULLS
// FIRST|LAST]" terms separated by commas -- for a window's ORDER BY and an
// aggregate call's own (parse.y:1238).
func (p *parser) parseSortList() ([]OrderTerm, error) {
	var terms []OrderTerm
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		desc := false
		if p.consumeKeyword("ASC") {
		} else if p.consumeKeyword("DESC") {
			desc = true
		}
		nulls := NullsDefault
		if p.consumeKeyword("NULLS") {
			if p.consumeKeyword("FIRST") {
				nulls = NullsFirst
			} else if p.consumeKeyword("LAST") {
				nulls = NullsLast
			} else {
				return nil, fmt.Errorf("engine: expected FIRST or LAST after NULLS")
			}
		}
		terms = append(terms, OrderTerm{Expr: e, Desc: desc, Nulls: nulls})
		if !p.peekIsPunct(",") {
			return terms, nil
		}
		p.next()
	}
}

func (p *parser) parseWindowSpec() (*WindowSpec, error) {
	if !p.peekIsPunct("(") {
		// Bare "OVER <windowname>".
		nt := p.next()
		if nt.kind != tkIdent {
			return nil, fmt.Errorf("engine: expected window name or ( after OVER")
		}
		return &WindowSpec{Ref: nt.text}, nil
	}
	p.next() // "("
	spec := &WindowSpec{}
	// Optional leading base window name: any identifier that is not the start
	// of a PARTITION/ORDER/frame clause and not the closing ")".
	if p.peek().kind == tkIdent && !p.peekIsKeyword("PARTITION") && !p.peekIsKeyword("ORDER") &&
		!p.peekIsKeyword("ROWS") && !p.peekIsKeyword("RANGE") && !p.peekIsKeyword("GROUPS") {
		spec.Ref = p.next().text
	}
	if p.consumeKeyword("PARTITION") {
		if !p.consumeKeyword("BY") {
			return nil, fmt.Errorf("engine: expected BY after PARTITION")
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			spec.PartitionBy = append(spec.PartitionBy, e)
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
	}
	if p.consumeKeyword("ORDER") {
		if !p.consumeKeyword("BY") {
			return nil, fmt.Errorf("engine: expected BY after ORDER")
		}
		terms, err := p.parseSortList()
		if err != nil {
			return nil, err
		}
		spec.OrderBy = terms
	}
	if p.peekIsKeyword("ROWS") || p.peekIsKeyword("RANGE") || p.peekIsKeyword("GROUPS") {
		frame, err := p.parseWindowFrame()
		if err != nil {
			return nil, err
		}
		spec.Frame = frame
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return spec, nil
}

// parseWindowFrame parses "ROWS|RANGE|GROUPS <frame>" where <frame> is either
// "BETWEEN <bound> AND <bound>" or a single "<bound>" (shorthand for "BETWEEN
// <bound> AND CURRENT ROW"). An optional trailing "EXCLUDE ..." clause is
// consumed and flagged (declined at execution).
func (p *parser) parseWindowFrame() (*WindowFrame, error) {
	fr := &WindowFrame{}
	switch {
	case p.consumeKeyword("ROWS"):
		fr.Mode = FrameRows
	case p.consumeKeyword("RANGE"):
		fr.Mode = FrameRange
	case p.consumeKeyword("GROUPS"):
		fr.Mode = FrameGroups
	default:
		return nil, fmt.Errorf("engine: expected ROWS, RANGE or GROUPS")
	}
	if p.consumeKeyword("BETWEEN") {
		start, err := p.parseFrameBound(true)
		if err != nil {
			return nil, err
		}
		if !p.consumeKeyword("AND") {
			return nil, fmt.Errorf("engine: expected AND in window frame")
		}
		end, err := p.parseFrameBound(false)
		if err != nil {
			return nil, err
		}
		fr.Start, fr.End = start, end
	} else {
		// Single-bound shorthand: "<mode> <start>" == "BETWEEN <start> AND
		// CURRENT ROW".
		start, err := p.parseFrameBound(true)
		if err != nil {
			return nil, err
		}
		fr.Start = start
		fr.End = FrameBound{Type: FrameCurrentRow}
	}
	if p.consumeKeyword("EXCLUDE") {
		fr.Exclude = true
		// Record which exclude kind (NO OTHERS | CURRENT ROW | GROUP | TIES)
		// so the frame builder can apply it exactly.
		switch {
		case p.consumeKeyword("NO"):
			if !p.consumeKeyword("OTHERS") {
				return nil, fmt.Errorf("engine: expected OTHERS after EXCLUDE NO")
			}
			fr.ExcludeKind = ExcludeNoOthers
		case p.consumeKeyword("CURRENT"):
			if !p.consumeKeyword("ROW") {
				return nil, fmt.Errorf("engine: expected ROW after EXCLUDE CURRENT")
			}
			fr.ExcludeKind = ExcludeCurrentRow
		case p.consumeKeyword("GROUP"):
			fr.ExcludeKind = ExcludeGroup
		case p.consumeKeyword("TIES"):
			fr.ExcludeKind = ExcludeTies
		default:
			return nil, fmt.Errorf("engine: expected NO OTHERS, CURRENT ROW, GROUP or TIES after EXCLUDE")
		}
	}
	return fr, nil
}

// parseFrameBound parses one window-frame endpoint. parse.y has different
// productions for the two ends:
//
//	frame_bound_s ::= frame_bound | UNBOUNDED PRECEDING
//	frame_bound_e ::= frame_bound | UNBOUNDED FOLLOWING
//	frame_bound   ::= expr PRECEDING|FOLLOWING | CURRENT ROW
//
// so "UNBOUNDED FOLLOWING" as a start is a syntax error at that keyword. The
// rest is left to semantic checks.
func (p *parser) parseFrameBound(isStart bool) (FrameBound, error) {
	if p.consumeKeyword("UNBOUNDED") {
		if p.consumeKeyword("PRECEDING") {
			if !isStart {
				return FrameBound{}, fmt.Errorf(`engine: near "PRECEDING": syntax error`)
			}
			return FrameBound{Type: FrameUnboundedPreceding}, nil
		}
		if p.consumeKeyword("FOLLOWING") {
			if isStart {
				return FrameBound{}, fmt.Errorf(`engine: near "FOLLOWING": syntax error`)
			}
			return FrameBound{Type: FrameUnboundedFollowing}, nil
		}
		return FrameBound{}, fmt.Errorf("engine: expected PRECEDING or FOLLOWING after UNBOUNDED")
	}
	if p.consumeKeyword("CURRENT") {
		if !p.consumeKeyword("ROW") {
			return FrameBound{}, fmt.Errorf("engine: expected ROW after CURRENT")
		}
		return FrameBound{Type: FrameCurrentRow}, nil
	}
	// "<expr> PRECEDING|FOLLOWING".
	off, err := p.parseExpr()
	if err != nil {
		return FrameBound{}, err
	}
	if p.consumeKeyword("PRECEDING") {
		return FrameBound{Type: FramePreceding, Offset: off}, nil
	}
	if p.consumeKeyword("FOLLOWING") {
		return FrameBound{Type: FrameFollowing, Offset: off}, nil
	}
	return FrameBound{}, fmt.Errorf("engine: expected PRECEDING or FOLLOWING in window frame")
}

// parseValuesSelectCore parses "VALUES (expr, ...) [, (...) ...]". "VALUES(1,2,3)"
// is "SELECT 1,2,3" with columns named column1, column2, ...; each further tuple
// becomes a UNION ALL arm on the first, giving one flat compound chain that the
// compound and recursive-CTE machinery handle unchanged (a single-tuple VALUES
// seed plus "UNION ALL SELECT ..." is the usual recursive CTE shape).
//
// That desugaring is right for execution but wrong for typing: sqlite3MultiValues
// folds a multi-row VALUES into one compound arm, which ValuesArms/ValuesFold
// record, so "SELECT g FROM t2 UNION ALL VALUES('x'),('z')" types as two arms
// (else affinity, CTAS types and even row counts come out wrong).
func (p *parser) parseValuesSelectCore() (*SelectStmt, error) {
	if !p.consumeKeyword("VALUES") {
		return nil, fmt.Errorf("engine: internal: parseValuesSelectCore called without a leading VALUES")
	}

	parseTuple := func() (*SelectStmt, error) {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var cols []SelectColumn
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			cols = append(cols, SelectColumn{Expr: e, Alias: fmt.Sprintf("column%d", len(cols)+1), HasAlias: true})
			if p.peekIsPunct(",") {
				p.next()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &SelectStmt{Columns: cols}, nil
	}

	first, err := parseTuple()
	if err != nil {
		return nil, err
	}
	constant := valuesRowIsConstant(first)
	// sqlite3MultiValues (insert.c:679-783) puts each further row into a co-routine
	// or onto a UNION ALL chain, and only the co-routine checks row width while
	// parsing (insert.c:774). A UNION ALL mismatch surfaces at resolve time
	// (resolve.c:2090), which a CREATE TRIGGER body never reaches (altertab2.test).
	// Replay that choice here; the compound's width check covers the rest.
	coroutine, coWidth := false, 0
	left := first // pLeft's own row: condition (d)'s subject
	for p.peekIsPunct(",") {
		p.next()
		tup, err := parseTuple()
		if err != nil {
			return nil, err
		}
		rowConstant := valuesRowIsConstant(tup)
		if p.hasWith || p.schemaBody || !rowConstant || (!coroutine && !valuesRowNoAffinity(left)) {
			coroutine, left = false, tup // conditions (a)-(d): UNION ALL
		} else {
			if !coroutine {
				coroutine, coWidth = true, len(left.Columns)
			}
			if len(tup.Columns) != coWidth {
				return nil, fmt.Errorf("engine: all VALUES must have the same number of terms")
			}
		}
		constant = constant && rowConstant
		first.Compound = append(first.Compound, CompoundArm{Op: "UNION ALL", Stmt: tup})
	}
	if len(first.Compound) > 0 {
		first.ValuesArms = len(first.Compound)
		first.ValuesFold = p.multiValuesFold(first, constant)
	}
	return first, nil
}

// multiValuesFold decides which of sqlite3MultiValues' two methods SQLite uses
// for this multi-row VALUES clause -- see valuesFold for the rule and the
// evidence. constant reports whether every row is a constant expression list.
func (p *parser) multiValuesFold(first *SelectStmt, constant bool) valuesFold {
	if !constant {
		// Condition (c). SQLite codes the rows it has already seen into a
		// co-routine and only then falls back, leaving a chain this engine
		// does not model. Nothing here can be answered from the shape alone.
		return valuesFoldUnanalyzable
	}
	if p.hasWith || p.schemaBody {
		return valuesFoldUnionAll // conditions (a) and (b)
	}
	// Condition (d): the co-routine is only started when nothing in the FIRST
	// row has an affinity of its own. Among constants that means a CAST --
	// sqlite3ExprAffinity looks through COLLATE to find it, and nothing else
	// a constant can be built from has one.
	for _, c := range first.Columns {
		if exprIsCastThroughCollate(c.Expr) {
			return valuesFoldUnionAll
		}
	}
	return valuesFoldCoroutine
}

// valuesRowNoAffinity is exprListIsNoAffinity (insert.c:615-626): every value
// constant, and none with an affinity of its own -- among constants, a CAST
// (see multiValuesFold).
func valuesRowNoAffinity(row *SelectStmt) bool {
	if !valuesRowIsConstant(row) {
		return false
	}
	for _, c := range row.Columns {
		if exprIsCastThroughCollate(c.Expr) {
			return false
		}
	}
	return true
}

// exprIsCastThroughCollate reports whether e is a CAST, looking through the
// COLLATE wrappers sqlite3ExprAffinity's EP_Skip loop looks through.
func exprIsCastThroughCollate(e Expr) bool {
	for {
		switch x := e.(type) {
		case CollateExpr:
			e = x.X
		case CastExpr:
			return true
		default:
			return false
		}
	}
}

// valuesRowIsConstant reports whether every value in one VALUES row is a
// constant expression, SQLite's sqlite3ExprIsConstant (exprIsConst with
// eCode 1). Non-constant there is exactly: a column reference, a bare
// identifier, an aggregate, a subquery (the walker fails on any SELECT) and a
// RAISE. A bound parameter IS constant at that eCode, and so is a function
// call whose arguments are all constant and whose FuncDef carries
// SQLITE_FUNC_CONSTANT or SQLITE_FUNC_SLOCHNG -- which is every built-in
// scalar function except the five declared VFUNCTION (c:137277).
func valuesRowIsConstant(row *SelectStmt) bool {
	for _, c := range row.Columns {
		if !exprIsConstantValue(c.Expr) {
			return false
		}
	}
	return true
}

// nonConstantFuncs are SQLite's VFUNCTION built-ins: the scalar functions that
// carry neither SQLITE_FUNC_CONSTANT nor SQLITE_FUNC_SLOCHNG, and so make an
// expression non-constant even with constant arguments. The date/time
// functions are deliberately absent: they are SQLITE_FUNC_SLOCHNG, which
// exprNodeIsConstantFunction accepts.
var nonConstantFuncs = map[string]bool{
	"random": true, "randomblob": true, "last_insert_rowid": true,
	"changes": true, "total_changes": true,
	// Registered without SQLITE_DETERMINISTIC (the bit SQLITE_FUNC_CONSTANT
	// shares) and not SLOCHNG: load_extension is SFUNCTION (sqliteInt.h:2144),
	// and the extension functions pass flags of their own (rtree.c:4326-4331,
	// fts3_tokenizer.c:480, fts5_main.c:3821-3845).
	"load_extension": true, "fts3_tokenizer": true,
	"rtreenode": true, "rtreedepth": true, "rtreecheck": true,
	"fts5": true, "fts5_insttoken": true, "fts5_locale": true,
}

func exprIsConstantValue(e Expr) bool {
	switch x := e.(type) {
	case LiteralExpr, ParamExpr:
		return true
	case CollateExpr:
		return exprIsConstantValue(x.X)
	case CastExpr:
		return exprIsConstantValue(x.X)
	case UnaryExpr:
		return exprIsConstantValue(x.X)
	case BinaryExpr:
		return exprIsConstantValue(x.L) && exprIsConstantValue(x.R)
	case CaseExpr:
		if x.Base != nil && !exprIsConstantValue(x.Base) {
			return false
		}
		for _, w := range x.Whens {
			if !exprIsConstantValue(w.When) || !exprIsConstantValue(w.Then) {
				return false
			}
		}
		return x.Else == nil || exprIsConstantValue(x.Else)
	case FuncExpr:
		// pDef->xFinalize!=0 (an aggregate) and EP_WinFunc both abort the
		// walk, as does a FILTER, which only a window aggregate can carry.
		if x.Star || x.Distinct || x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
			return false
		}
		if isAggregateFuncName(x.Name) || nonConstantFuncs[r33sFoldIdent(x.Name)] {
			return false
		}
		for _, a := range x.Args {
			if !exprIsConstantValue(a) {
				return false
			}
		}
		return true
	default:
		// ColumnExpr, SubqueryExpr, ExistsExpr, InExpr, RaiseExpr, and
		// anything else this package grows: not provably constant.
		return false
	}
}

// parseSignedIntLiteral parses an optionally-negated integer literal, as
// used by LIMIT/OFFSET (this package doesn't support arbitrary expressions
// there, only literals, which covers real-world usage).
func (p *parser) parseSignedIntLiteral() (int64, error) {
	neg := false
	if p.peekIsPunct("-") {
		p.next()
		neg = true
	}
	t := p.peek()
	if t.kind != tkNumber || strings.ContainsAny(t.text, ".eE") {
		return 0, fmt.Errorf("expected an integer literal, got %q", p.tokenDesc(t))
	}
	p.next()
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("integer literal %q out of range", t.text)
	}
	if neg {
		n = -n
	}
	return n, nil
}

// peekQualifiedStar reports, without consuming, whether the parser is at a
// "<ident>.*" select-list item, and the qualifier. Checked before expression
// parsing, since "t.*" is only valid as a select-list item.
func (p *parser) peekQualifiedStar() (qualifier string, ok bool) {
	t := p.peek()
	if t.kind != tkIdent {
		return "", false
	}
	dot := p.peekAt(1)
	if dot.kind != tkPunct || dot.text != "." {
		return "", false
	}
	star := p.peekAt(2)
	if star.kind != tkPunct || star.text != "*" {
		return "", false
	}
	return t.text, true
}

func (p *parser) parseSelectList() ([]SelectColumn, error) {
	var cols []SelectColumn
	for {
		if p.peekIsPunct("*") {
			p.next()
			cols = append(cols, SelectColumn{Star: true})
		} else if qualifier, ok := p.peekQualifiedStar(); ok {
			p.next() // qualifier ident
			p.next() // "."
			p.next() // "*"
			cols = append(cols, SelectColumn{Star: true, StarQualifier: qualifier})
		} else {
			start := p.pos
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			text := strings.TrimSpace(p.src[p.toks[start].Start:p.toks[p.pos-1].End])
			alias := ""
			hasAlias := false
			if p.consumeKeyword("AS") {
				a := p.next()
				name, ok := aliasTokenText(a)
				if !ok {
					return nil, fmt.Errorf("engine: expected alias after AS, got %q", p.tokenDesc(a))
				}
				alias, hasAlias = name, true
			} else if name, ok := aliasTokenText(p.peek()); ok && !p.peekStopsAlias(false) {
				// isClauseKeyword's upper-cased-keyword check is only ever
				// meaningful for a tkIdent (a bare keyword spelling); a
				// tkString token's own .text field is always "" (its decoded
				// value lives in .str -- see aliasTokenText), so upper()
				// trivially returns "" there and isClauseKeyword("") is
				// false, i.e. a string alias is never mistakenly excluded.
				p.next()
				alias, hasAlias = name, true
			}
			col := SelectColumn{Expr: e, Alias: alias, HasAlias: hasAlias, Text: text, RawText: text}
			if colRef, ok := e.(ColumnExpr); ok && !hasAlias {
				col.Text = colRef.Name
			}
			cols = append(cols, col)
		}
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	return cols, nil
}

// aliasTokenText reports the alias name t spells: an identifier, or (legacy
// leniency) a single-quoted string, including an empty one. Applies to
// result-column, table and derived-table aliases, AS or bare. Not used for DDL
// names, though C is lenient there too.
func aliasTokenText(t token) (name string, ok bool) {
	switch t.kind {
	case tkIdent:
		return t.text, true
	case tkString:
		return t.str, true
	}
	return "", false
}

// objectNameToken reports the schema-object name t spells, SQLite's "nm": an
// identifier or a single-quoted string. Applied only where a name is required
// (table/column names, FROM items, INSERT/UPDATE/DELETE targets, the qualifier of
// a dotted column reference); in expression position a bare string stays a
// literal ("DELETE FROM '@abc' WHERE '!pqr'=6" compares the string). Same rule
// as aliasTokenText for a different grammar production.
func objectNameToken(t token) (name string, ok bool) {
	switch t.kind {
	case tkIdent:
		return t.text, true
	case tkString:
		return t.str, true
	}
	return "", false
}

// isClauseKeyword reports whether an upper-cased identifier terminates the
// select-list / stops an implicit (AS-less) alias from being consumed.
// UNION/INTERSECT/EXCEPT must be included here (not just ORDER/LIMIT):
// without them, a compound select's first arm with no explicit alias --
// e.g. "SELECT c4 FROM t4 UNION SELECT c1 FROM t1" -- would swallow "UNION"
// itself as an implicit column alias for c4, exactly as it would swallow
// "FROM" if that weren't excluded either.
func isClauseKeyword(u string) bool {
	switch u {
	case "FROM", "WHERE", "ORDER", "GROUP", "HAVING", "LIMIT", "AS",
		"UNION", "INTERSECT", "EXCEPT", "WINDOW",
		// RETURNING ends a SELECT like FROM/WHERE do, so "INSERT ... SELECT ...
		// RETURNING" does not alias the last item "RETURNING". C reserves the word.
		"RETURNING":
		return true
	}
	return false
}

// peekStopsAlias reports whether the current token ends an implicit alias
// (isClauseKeyword, or isFromAliasStopKeyword in FROM), resolving WINDOW by
// lookahead: it is an ordinary identifier unless "WINDOW <name> AS (" follows.
//
//	SELECT * FROM t4 window              -- alias (nothing follows)
//	SELECT * FROM t4 window, t4          -- alias (window6.test 4.1)
//	SELECT * FROM t4 window w AS ()      -- CLAUSE, no alias
//	SELECT * FROM t4 window WINDOW w AS () -- alias, THEN the clause
//	SELECT 1 window                      -- select-list alias, column "window"
//	SELECT 1 WINDOW w AS ()              -- CLAUSE, column named "1"
//
// A quoted "window" always aliases.
func (p *parser) peekStopsAlias(inFrom bool) bool {
	t := p.peek()
	u := t.upper()
	if u == "WINDOW" && !p.peekStartsWindowClause() {
		return false
	}
	if (u == "ON" || u == "USING") && !t.quoted {
		// ON and USING are not in parse.y's %fallback ID list, so they never alias
		// ("SELECT 1 ON" is a syntax error; quoted spellings alias). This lets
		// "INSERT INTO t1 SELECT 1,'k' ON CONFLICT ..." reach the upsert clause.
		return true
	}
	if inFrom {
		return isFromAliasStopKeyword(u)
	}
	return isClauseKeyword(u)
}

// peekStartsWindowClause reports whether the current token opens a
// "WINDOW <name> AS ( ... )" clause -- see peekStopsAlias for the alternative
// (an ordinary alias named "window") and for the oracle-verified statements
// that pin the split.
func (p *parser) peekStartsWindowClause() bool {
	t := p.peek()
	if t.kind != tkIdent || t.quoted || t.upper() != "WINDOW" {
		return false
	}
	nt := p.peekAt(1)
	if _, ok := objectNameToken(nt); !ok {
		return false
	}
	if nt.kind == tkIdent && !nt.quoted && nonIdentifierKeywords[nt.upper()] {
		return false // a reserved word cannot be the window's name
	}
	as := p.peekAt(2)
	if as.kind != tkIdent || as.quoted || as.upper() != "AS" {
		return false
	}
	open := p.peekAt(3)
	return open.kind == tkPunct && open.text == "("
}

// isFromAliasStopKeyword extends isClauseKeyword with C's TK_JOIN_KW set, which
// cannot be an implicit table alias: "FROM t1 OUTER NATURAL INNER JOIN t2" must
// fail, not alias t1 as OUTER (join.test). An unrecognized word does alias
// ("FROM t1 BOGUS JOIN t2").
func isFromAliasStopKeyword(u string) bool {
	if isClauseKeyword(u) {
		return true
	}
	switch u {
	case "JOIN", "INNER", "CROSS", "LEFT", "RIGHT", "FULL", "OUTER", "NATURAL", "ON", "USING",
		"INDEXED", "NOT":
		return true
	}
	return false
}

// parseJoinConstraint parses the optional "ON <expr>" / "USING (<cols>)" after
// any join element, explicit or comma (C hangs it off the FROM element). kind is
// the element's join kind; natural forbids both forms.
func (p *parser) parseJoinConstraint(item *FromItem, kind JoinKind, natural bool) error {
	switch {
	case p.consumeKeyword("ON"):
		if natural {
			return fmt.Errorf("engine: cannot use ON clause with NATURAL join")
		}
		on, err := p.parseExpr()
		if err != nil {
			return err
		}
		item.On = on
		if kind == JoinCross {
			// "CROSS JOIN t ON <cond>" is accepted (SQLite's own grammar
			// allows an optional join-constraint on any join operator,
			// including a comma/CROSS join) and behaves exactly like
			// JoinInner: an inline filter, no NULL-extension. CROSS vs
			// INNER is purely a query-planner hint in C SQLite with
			// no observable semantic difference once a join-constraint
			// is present.
			item.Join = JoinInner
		}
	case p.consumeKeyword("USING"):
		if natural {
			return fmt.Errorf("engine: cannot use USING clause with NATURAL join")
		}
		using, uerr := p.parseUsingList()
		if uerr != nil {
			return uerr
		}
		item.Using = using
		if kind == JoinCross {
			// Same JoinCross -> JoinInner promotion as an explicit ON
			// above -- see that case's comment. Purely cosmetic (join.go/
			// vdbe_join_codegen.go only ever consult .left/.on, never
			// .Join itself, once a condition is present).
			item.Join = JoinInner
		}
	case natural:
		// NATURAL with no ON/USING -- the common-column condition (or,
		// with no common columns at all, no condition -- a true cross
		// join) is computed later, at plan time (join.go's
		// desugarJoinItem), once the joined tables' schemas are known.
		// Nothing more to parse here.
	default:
		// No ON/USING/NATURAL: the constraint is optional on every join operator. A
		// plain JOIN is a cross product; LEFT/RIGHT/FULL with no condition always match,
		// so NULL-extension only fires when the other side is empty. Both executors
		// already treat a nil condition as always matching.
	}
	return nil
}

// markRebuiltGroupStars stamps FromItem.GroupRebuildStarred on every member of a
// rebuilt parenthesized join group that some "*" in cols expands over (bare "*",
// or "X.*" naming a member or the group alias). That is the one way the rebuilt
// column order reaches the result (checkRebuiltJoinGroups). Other positional
// uses read the select list, which this covers.
func markRebuiltGroupStars(cols []SelectColumn, items []FromItem) {
	bare := false
	var quals []string
	for _, sc := range cols {
		if !sc.Star {
			continue
		}
		if sc.StarQualifier == "" {
			bare = true
			continue // keep scanning: a LATER qualified star still needs recording in quals
		}
		quals = append(quals, r33sFoldIdent(sc.StarQualifier))
	}
	if !bare && len(quals) == 0 {
		return
	}
	starred := make(map[int]bool)
	// qualStarred additionally records which groups a QUALIFIED star (as
	// opposed to only a bare one) reaches -- FromItem.GroupRebuildQualStarred's
	// own doc comment explains why checkOneRebuiltGroup needs the distinction.
	qualStarred := make(map[int]bool)
	for _, it := range items {
		if it.GroupRebuildID == 0 || (starred[it.GroupRebuildID] && qualStarred[it.GroupRebuildID]) {
			continue
		}
		if bare {
			starred[it.GroupRebuildID] = true
		}
		name := it.Alias
		if name == "" {
			name = it.Table
		}
		for _, q := range quals {
			if equalFoldName(q, name) || (it.GroupAlias != "" && equalFoldName(q, it.GroupAlias)) {
				starred[it.GroupRebuildID] = true
				qualStarred[it.GroupRebuildID] = true
				break
			}
		}
	}
	for i := range items {
		if starred[items[i].GroupRebuildID] {
			items[i].GroupRebuildStarred = true
		}
		if qualStarred[items[i].GroupRebuildID] {
			items[i].GroupRebuildQualStarred = true
		}
	}
}

// peekLeadingJoinConstraint reports whether the current token is a BARE ON or
// USING keyword -- the join-constraint the first element of a table-reference
// list has a grammar slot for but no join operator to hang it on. The quoted
// spellings are ordinary identifiers ("SELECT a FROM src \"on\"" is accepted by
// the oracle, aliasing src as "on"), so they are not it; peekStopsAlias applies
// the same !quoted rule for the same reason.
func (p *parser) peekLeadingJoinConstraint() (kw string, ok bool) {
	t := p.peek()
	if t.kind != tkIdent || t.quoted {
		return "", false
	}
	switch t.upper() {
	case "ON", "USING":
		return t.upper(), true
	}
	return "", false
}

// parseFromClause parses the table-reference list after FROM: a first element,
// then comma joins or "[NATURAL] [INNER|CROSS|LEFT|RIGHT|FULL [OUTER]] JOIN"
// elements, each with an optional ON/USING. NATURAL and USING conditions are
// computed at plan time (desugarJoinItem), once column lists are known.
// Parenthesized subtrees with outer joins are kept atomic (checkFlattenSafe).
// leading is true only for a FROM clause's own first element; it feeds
// FromItem.NestedNonLeading and nothing else.
func (p *parser) parseFromClause(leading bool) ([]FromItem, error) {
	first, err := p.parseFromElement(leading)
	if err != nil {
		return nil, err
	}
	// The first element has an ON/USING slot too (parse.y's seltablist, empty
	// stl_prefix), and the parser shifts an adjacent ON into it; sqlite3ProcessJoin
	// then rejects it ("a JOIN clause is required before ON"). That is C's
	// documented UPSERT ambiguity, resolved in favour of the JOIN: so
	// "INSERT INTO t1 SELECT a,b FROM src ON CONFLICT(a) DO UPDATE ..." is a syntax
	// error, and "WHERE true" (or anything) before the ON reaches the upsert. Applies
	// to every table-reference list, including parenthesized groups and
	// UPDATE ... FROM.
	if kw, isCon := p.peekLeadingJoinConstraint(); isCon {
		return nil, fmt.Errorf("engine: a JOIN clause is required before %s", kw)
	}
	checkFlattenSafe(first, leading)
	items := first

	for {
		if p.peekIsPunct(",") {
			p.next()
			elems, err := p.parseFromElement(false) // reached via ",": never leading
			if err != nil {
				return nil, err
			}
			elems[0].Join = JoinCross
			// A comma join takes ON/USING too ("FROM t1, t2 ON t1.b=t2.b" equals
			// "t1 JOIN t2 ON ..."); in "FROM t1, t2, t3 ON ..." it binds to the last comma,
			// which parsing it against the element just read reproduces.
			if err := p.parseJoinConstraint(&elems[0], JoinCross, false); err != nil {
				return nil, err
			}
			checkFlattenSafe(elems, false)
			items = append(items, elems...)
			continue
		}

		kind, natural, matched, err := p.tryParseJoinKind()
		if err != nil {
			return nil, err
		}
		if !matched {
			break
		}

		elems, err := p.parseFromElement(false) // reached via a join operator: never leading
		if err != nil {
			return nil, err
		}
		item := &elems[0]
		item.Join = kind
		item.Natural = natural
		// Recorded BEFORE parseJoinConstraint, which promotes an explicit
		// "CROSS JOIN ... ON/USING" to JoinInner: SQLite keeps JT_CROSS set
		// alongside JT_INNER there, and JT_CROSS is a reorder barrier whether
		// or not a join-constraint is present. See FromItem.CrossKeyword.
		item.CrossKeyword = kind == JoinCross

		if err := p.parseJoinConstraint(item, kind, natural); err != nil {
			return nil, err
		}

		checkFlattenSafe(elems, false)
		items = append(items, elems...)
	}
	return items, nil
}

// checkFlattenSafe validates a FROM element flattened from a parenthesized join
// subtree (more than one item). An all-INNER/CROSS group is associative and can
// be flattened; with an outer join inside or connecting it,
// "(t0 LEFT JOIN t1) JOIN t2" is not "t0 LEFT JOIN (t1 JOIN t2)", so instead
// elems[0].GroupLen = len(elems) marks the span to be evaluated as one nested
// join (resolveGroupSource). elems[0] carries how the whole
// group attaches; the rest keep the group's internal structure.
//
// parse.y:777-816 splices a group flat only when it is leading, unaliased and
// has no ON/USING; otherwise it builds an SF_NestedFrom subquery. So these also
// force atomicity:
//
//   - an alias (HasGroupAlias): the members must stay one unit for
//     resolveGroupSource's alias scoping ("... CROSS JOIN (t2 CROSS JOIN t0)
//     AS a1 ON (a1.c0 < a1.c1)", joinI.test);
//   - an outward ON/USING: it may name any member, but a flattened span binds
//     its ON at the first member's level ("v1 INNER JOIN (v2 CROSS JOIN t0) ON
//     (t0.c0 < t0.c1)", joinI.test).
func checkFlattenSafe(elems []FromItem, leading bool) {
	if len(elems) <= 1 {
		return
	}
	if elems[0].HasGroupAlias || elems[0].On != nil || elems[0].Using != nil {
		elems[0].GroupLen = len(elems)
		return
	}
	// A non-leading group containing USING/NATURAL is atomic too: the coalesce
	// representative is the first earlier item with the column
	// (tableAndColumnIndex, select.c:379), and flattening would let it escape the
	// group ("u JOIN (t JOIN w USING(a))" would bind u to w). A leading group has
	// nothing before it.
	if !leading {
		for i := 1; i < len(elems); i++ {
			if elems[i].Natural || len(elems[i].Using) != 0 {
				elems[0].GroupLen = len(elems)
				return
			}
		}
	}
	for i := range elems {
		switch elems[i].Join {
		case JoinCross, JoinInner:
			// associative/commutative -- safe to flatten.
		default:
			elems[0].GroupLen = len(elems)
			return
		}
	}
}

// parseFromElement parses one FROM element: one FromItem for a table or a
// derived table "( SELECT ... ) [AS] alias [(cols)]", or several for a
// parenthesized join subtree flattened in (see checkFlattenSafe). "( SELECT"
// starts a derived table; anything else after "(" is a join subtree. Join/On/
// Using/Natural are left for the caller to fill on the first item.
//
// leading is passed from parseFromClause. When false and the element is a
// subtree, every resulting item is marked NestedNonLeading.
func (p *parser) parseFromElement(leading bool) ([]FromItem, error) {
	if p.peekIsPunct("(") {
		p.fromParen = true
		// A derived table's body is a full select-statement: "(SELECT ...)",
		// "(WITH ... SELECT ...)" or "(VALUES (...), (...))". The last two
		// used to fall through to the parenthesized-join-subtree branch
		// below, which then tried to read "WITH"/"VALUES" as a table name
		// ("expected \")\", got <cte-name>", "expected table name, got 55").
		// C SQLite accepts both as derived tables -- "SELECT * FROM
		// (VALUES(1),(2))" yields one column named "column1" over two rows,
		// verified directly.
		next := p.peekAt(1)
		if next.kind == tkIdent && (next.upper() == "SELECT" || next.upper() == "WITH" || next.upper() == "VALUES") {
			item, err := p.parseDerivedTable()
			if err != nil {
				return nil, err
			}
			return []FromItem{item}, nil
		}
		// Parenthesized join subtree: "( <table-ref-list> )". A paren-join
		// opened while already inside one is a NESTED parenthesized join --
		// SQLite ":N"-renames its columns (see FromNestedParenJoin).
		if p.parenJoinDepth > 0 {
			p.fromNestedParen = true
		}
		p.next() // consume "("
		p.parenJoinDepth++
		// The paren's content is its own table-reference list, so its first element is
		// leading within it (parse.y reduces the inner seltablist independently).
		// Passing true matters for NestFromWrapDepth, which increments: passing the
		// outer leading double-counted a splicing inner paren ("q1 JOIN ((q2 JOIN q3)
		// JOIN q4)" has one SF_NestedFrom layer). NestedNonLeading and GroupRebuildID
		// are stamped over the whole flattened content below either way.
		inner, err := p.parseFromClause(true)
		p.parenJoinDepth--
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		if !leading {
			// Reached via a comma or join operator: mark every resolved item (at any
			// depth) NestedNonLeading, and give the whole group one ID, overwriting nested
			// ones so the outermost group wins (FromItem.NestedGroupID).
			p.nestedGroupSeq++
			for i := range inner {
				inner[i].NestedNonLeading = true
				inner[i].NestedGroupID = p.nestedGroupSeq
			}
		}
		// A group alias from a deeper paren does not reach out through this one
		// ("t1 JOIN ((t2 CROSS JOIN t0) AS a1) ON v>a1.c0" is "no such column"); that
		// outer-into-nested case stays declined below.
		//
		// parse.y:810-816 does not flatten a re-wrapped group: it wraps the inner
		// seltablist as one opaque SF_NestedFrom item. So when this paren's content
		// starts with an already-aliased group's connector, collapse that span into one
		// FromItem.NestedGroupSpan stand-in first; then a sibling's "x.b" resolves
		// against an ordinary item named x. Recursive by construction (parsing is
		// inside-out). Only the connector position is handled; an aliased group at
		// another position inside a further re-wrap stays declined below.
		if len(inner) > 0 && inner[0].GroupAlias != "" && inner[0].GroupLen > 0 {
			innerLen := inner[0].GroupLen
			opaque := FromItem{Alias: inner[0].GroupAlias, NestedGroupSpan: append([]FromItem(nil), inner[:innerLen]...)}
			inner = append([]FromItem{opaque}, inner[innerLen:]...)
		}
		for i := range inner {
			if inner[i].GroupAlias != "" {
				return nil, fmt.Errorf("engine: unsupported: parenthesized join group alias %q re-wrapped in further parentheses", inner[i].GroupAlias)
			}
		}
		// "( <one table-ref> ) [AS] alias" moves the alias onto that element (parse.y's
		// nSrc==1 branch), the outer alias winning: "SELECT x.b FROM (t2 AS y) AS x"
		// works, "SELECT y.b ..." does not.
		//
		// A group of two or more takes the alias as an extra qualifier over every member
		// (FromItem.GroupAlias): C builds an SF_NestedFrom subquery whose members stay
		// addressable, so both "j1.a" and "t1.a" work over "(t1 JOIN t2 ON a=c) AS j1".
		// The column-list rewrites C's rebuild performs are declined at compile time by
		// checkRebuiltJoinGroups, keyed on FromItem.GroupRebuildID (an alias, or a
		// non-leading position, triggers the rebuild).
		alias, hasAlias, aerr := p.tryParseFromAlias()
		if aerr != nil {
			return nil, aerr
		}
		if len(inner) == 1 {
			// parse.y:777-816: a leading one-element term with no alias is spliced in
			// unchanged, keeping its inner alias. Anywhere else it is rebuilt carrying only
			// the outer alias, so "SELECT x.c FROM ta JOIN (tb AS x) ON 1" is "no such
			// column: x.c".
			switch {
			case hasAlias:
				inner[0].Alias = alias
			case !leading:
				inner[0].Alias = ""
				// A re-wrapped group's members still carry the inner group
				// alias as a qualifier (FromItem.GroupAlias); it is dropped
				// with the alias.
				for i := range inner[0].NestedGroupSpan {
					inner[0].NestedGroupSpan[i].GroupAlias = ""
				}
			}
			return inner, nil
		}
		if hasAlias || !leading {
			// Rule 115 splices a group in flat only when it is leading AND
			// unaliased AND carries no trailing ON/USING; anything else is
			// rebuilt. hasAlias, not alias != "", because the test is on the
			// alias TOKEN's length -- "AS \"\"" rebuilds too. See
			// FromItem.GroupRebuildID for why the ON/USING half needs no
			// test of its own.
			p.groupRebuildSeq++
			for i := range inner {
				inner[i].GroupRebuildID = p.groupRebuildSeq
				inner[i].HasGroupAlias = hasAlias
				// See FromItem.NestFromWrapDepth: INCREMENTED (not
				// overwritten, unlike GroupRebuildID above), so a member
				// wrapped again by an enclosing paren accumulates depth 2,
				// 3, ... instead of losing the inner wrap's own count.
				inner[i].NestFromWrapDepth++
			}
		}
		if hasAlias {
			for i := range inner {
				inner[i].GroupAlias = alias
			}
		}
		return inner, nil
	}
	item, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	return []FromItem{item}, nil
}

// parseDerivedTable parses a derived table: "( SELECT ... )" followed by an
// optional alias (AS-prefixed or a bare trailing identifier, exactly like an
// ordinary table reference). The parser position is on the opening "(" of the
// subquery. SQLite has no FROM-clause column-alias list -- "(SELECT ...) x(a,
// b)" is a syntax error in C SQLite 3.53.3 -- so no such list is parsed
// here either: a trailing "(" after the alias is left for ParseSelect's
// trailing-input check to reject, matching C SQLite's own rejection.
func (p *parser) parseDerivedTable() (FromItem, error) {
	if err := p.expectPunct("("); err != nil {
		return FromItem{}, err
	}
	sub, err := p.parseSelectStmt()
	if err != nil {
		return FromItem{}, err
	}
	if err := p.expectPunct(")"); err != nil {
		return FromItem{}, err
	}
	item := FromItem{Subquery: sub}
	alias, ok, err := p.tryParseFromAlias()
	if err != nil {
		return FromItem{}, err
	}
	if ok {
		item.Alias = alias
	}
	return item, nil
}

// tryParseFromAlias consumes a FROM-item alias if one is next: "AS <name>", or
// a bare trailing identifier that is not itself a clause/join keyword (the
// implicit-alias rule this package already applies to a plain table reference
// and to a select-list item). ok is false when nothing was consumed.
func (p *parser) tryParseFromAlias() (alias string, ok bool, err error) {
	if p.consumeKeyword("AS") {
		a := p.next()
		name, isName := aliasTokenText(a)
		if !isName {
			return "", false, fmt.Errorf("engine: expected alias after AS, got %q", p.tokenDesc(a))
		}
		return name, true, nil
	}
	if name, isName := aliasTokenText(p.peek()); isName && !p.peekStopsAlias(true) {
		p.next()
		return name, true, nil
	}
	return "", false, nil
}

// parseUsingList parses the parenthesized, comma-separated column-name list
// following the USING keyword ("( col {, col} )"), matching SQLite's own
// idlist grammar: at least one column name is required (an empty "USING ()"
// is a parse error, exactly like C SQLite).
func (p *parser) parseUsingList() ([]string, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var cols []string
	for {
		id := p.next()
		if id.kind != tkIdent {
			return nil, fmt.Errorf("engine: expected column name in USING(...), got %q", p.tokenDesc(id))
		}
		cols = append(cols, id.text)
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return cols, nil
}

// The join-type bits SQLite's sqlite3JoinType (build.c) ORs together, one per
// written keyword. Reproduced verbatim because the ACCEPT/REJECT rule below is
// stated in terms of the combined mask, not of the keyword sequence: a
// three-keyword operator is legal exactly when its bits are.
const (
	jtInner = 1 << iota
	jtCross
	jtNatural
	jtLeft
	jtRight
	jtOuter
)

// joinKeywordMask maps one join-operator keyword to its bits. C SQLite
// tokenizes exactly these seven as TK_JOIN_KW, which is why they -- and only
// they -- cannot be swallowed as a table's implicit alias (see
// isFromAliasStopKeyword): "FROM t1 BOGUS JOIN t2" aliases t1 as BOGUS and is a
// plain JOIN, while "FROM t1 LEFT BOGUS JOIN t2" is "unknown or unsupported
// join type: LEFT BOGUS" (join.test 3.4d, verified directly).
func joinKeywordMask(u string) (int, bool) {
	switch u {
	case "NATURAL":
		return jtNatural, true
	case "LEFT":
		return jtLeft | jtOuter, true
	case "OUTER":
		return jtOuter, true
	case "RIGHT":
		return jtRight | jtOuter, true
	case "FULL":
		return jtLeft | jtRight | jtOuter, true
	case "INNER":
		return jtInner, true
	case "CROSS":
		return jtInner | jtCross, true
	}
	return 0, false
}

// tryParseJoinKind consumes one join operator: JOIN, or up to three names then
// JOIN, restoring position and reporting matched == false otherwise. Names may
// come in any order, as in C's grammar ("joinop ::= JOIN_KW nm nm JOIN"), and
// sqlite3JoinType ORs their bits and rejects:
//
//	an unrecognized name anywhere      -- "LEFT BOGUS", "NATURAL AAA BBB"
//	INNER and OUTER both set           -- "INNER OUTER", "LEFT INNER", "CROSS LEFT"
//	OUTER set with neither LEFT nor RIGHT -- bare "OUTER", "OUTER OUTER",
//	                                      "OUTER NATURAL"
//
// So "LEFT RIGHT JOIN" is FULL (join8.test), "OUTER LEFT NATURAL JOIN" is NATURAL
// LEFT, and repeats are harmless.
func (p *parser) tryParseJoinKind() (kind JoinKind, natural, matched bool, err error) {
	save := p.pos
	if p.consumeKeyword("JOIN") {
		return JoinInner, false, true, nil
	}
	first := p.peek()
	if first.kind != tkIdent {
		return 0, false, false, nil
	}
	if _, ok := joinKeywordMask(first.upper()); !ok {
		return 0, false, false, nil
	}
	mask, bad := 0, false
	var words []string
	for len(words) < 3 {
		t := p.peek()
		if t.kind != tkIdent || t.upper() == "JOIN" {
			break
		}
		p.next()
		words = append(words, t.text)
		m, ok := joinKeywordMask(t.upper())
		if !ok {
			bad = true
			break
		}
		mask |= m
	}
	if !p.consumeKeyword("JOIN") {
		// Not a join operator after all (the FROM clause ends here, or the
		// statement is malformed and the caller reports the leftover tokens).
		p.pos = save
		return 0, false, false, nil
	}
	if bad || mask&(jtInner|jtOuter) == jtInner|jtOuter || mask&(jtOuter|jtLeft|jtRight) == jtOuter {
		return 0, false, false, fmt.Errorf("engine: unknown or unsupported join type: %s", strings.Join(words, " "))
	}
	switch {
	case mask&jtLeft != 0 && mask&jtRight != 0:
		kind = JoinFull
	case mask&jtLeft != 0:
		kind = JoinLeft
	case mask&jtRight != 0:
		kind = JoinRight
	case mask&jtCross != 0:
		kind = JoinCross
	default:
		kind = JoinInner
	}
	return kind, mask&jtNatural != 0, true, nil
}

// parseTableRef parses one FROM-clause table reference: a table name, an
// optional alias (AS-prefixed, or a bare trailing identifier that isn't
// itself a clause/join keyword -- exactly the same implicit-alias rule
// already used for the base FROM table and for select-list items), and an
// optional index hint ("INDEXED BY <index-name>" or "NOT INDEXED"). The hint
// constrains which index the query planner may use, never the rows a full
// scan returns -- except that a PARTIAL index the WHERE does not imply leaves
// no plan at all, which C reports as "no query solution"
// (where_plan_indexed_by.go). Join
// is left at its zero value (JoinCross); the caller (parseFromClause) fills
// it in for every item after the first.
func (p *parser) parseTableRef() (FromItem, error) {
	tbl := p.next()
	tblName, ok := objectNameToken(tbl)
	if !ok {
		return FromItem{}, fmt.Errorf("engine: expected table name, got %q", p.tokenDesc(tbl))
	}
	item := FromItem{Table: tblName}
	// An optional database qualifier: "schema.table" (e.g. "main.t", "aux.t").
	// The first identifier becomes the Schema; the identifier after the dot is
	// the actual table name. resolveFrom (join.go) validates the qualifier
	// against this pager's own schema and strips it, or declines. (A column
	// reference's own "t.c" dot is parsed separately in parsePrimary; this dot
	// only appears in FROM-item position.)
	if p.peekIsPunct(".") {
		p.next() // the "."
		t2 := p.next()
		n2, ok := objectNameToken(t2)
		if !ok {
			return FromItem{}, fmt.Errorf("engine: expected table name after schema qualifier, got %q", p.tokenDesc(t2))
		}
		item.Schema = tblName
		item.Table = n2
	}
	// A TABLE-VALUED FUNCTION call: "name(arg, arg, ...)" -- e.g.
	// "generate_series(1,10)" (vtab.go). The arguments are ordinary scalar
	// expressions, evaluated (as constants, against any bound parameters) at
	// resolveFrom time and bound positionally to the module's hidden columns.
	//
	// A schema QUALIFIER on one is dropped, because C SQLite ignores it
	// outright: an eponymous virtual table is found by NAME, and the database
	// the qualifier names is never even validated. Verified directly against
	// mattn/go-sqlite3 3.53.3, with a main m1(a,b), a temp t5_1(x,y) and an
	// attached aux -- "main.pragma_table_info('t5_1')" answers the TEMP
	// table's x,y; "temp.pragma_table_info('m1')" answers the MAIN table's
	// a,b; "aux.pragma_table_info('m1')" answers a,b as well, and so does
	// "nosuchdb.pragma_table_info('m1')" with nothing of that name attached;
	// the hidden "schema" column reads NULL through every one of them, and
	// only an explicit SECOND ARGUMENT ("pragma_table_info('t5_1','main')")
	// actually scopes the lookup. The same holds for a non-pragma module:
	// "temp.json_each('{\"a\":1}')" and "nosuchdb.json_each(...)" both answer
	// the ordinary rows, and an unknown module or a plain table reports
	// exactly the unqualified error ("no such table: main.nosuch_module",
	// "'m1' is not a function"). A "(" here is what makes this unambiguous --
	// SQLite's grammar has no other production for one after a FROM name.
	// csv01.test's "SELECT name FROM temp.pragma_table_info('t5_1')" is the
	// mined statement that died on this as "unexpected trailing input".
	if p.peekIsPunct("(") {
		item.Schema = ""
		p.next() // "("
		item.TableFunc = true
		if !p.peekIsPunct(")") {
			for {
				arg, err := p.parseExpr()
				if err != nil {
					return FromItem{}, err
				}
				item.TableFuncArgs = append(item.TableFuncArgs, arg)
				if p.peekIsPunct(",") {
					p.next()
					continue
				}
				break
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return FromItem{}, err
		}
	}
	if p.consumeKeyword("AS") {
		a := p.next()
		name, ok := aliasTokenText(a)
		if !ok {
			return FromItem{}, fmt.Errorf("engine: expected alias after AS, got %q", p.tokenDesc(a))
		}
		item.Alias = name
	} else if name, ok := aliasTokenText(p.peek()); ok && !p.peekStopsAlias(true) {
		p.next()
		item.Alias = name
	}
	if p.consumeKeyword("INDEXED") {
		if !p.consumeKeyword("BY") {
			return FromItem{}, fmt.Errorf("engine: expected BY after INDEXED, got %q", p.tokenDesc(p.peek()))
		}
		idx := p.next()
		if idx.kind != tkIdent {
			return FromItem{}, fmt.Errorf("engine: expected index name after INDEXED BY, got %q", p.tokenDesc(idx))
		}
		item.IndexedBy = idx.text
	} else if p.peekIsKeyword("NOT") {
		save := p.pos
		p.next()
		if p.consumeKeyword("INDEXED") {
			item.NotIndexed = true
		} else {
			p.pos = save
		}
	}
	return item, nil
}

// ---- expression parsing (precedence climbing, matching SQLite's table:
// https://www.sqlite.org/lang_expr.html#operators_and_parse_affecting_attributes ) ----

func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.consumeKeyword("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: "OR", L: l, R: r}
	}
	return l, nil
}

// parseAnd implements AND, which binds tighter than OR but looser than
// unary NOT: each AND operand is parsed via parseNotLevel (rather than
// parseComparison directly) so a "NOT" prefixing any operand -- not just the
// first -- is scoped to that operand alone, e.g. "a AND NOT b" parses as
// "a AND (NOT b)", not as a parse error or a NOT that reaches past the AND.
func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNotLevel()
	if err != nil {
		return nil, err
	}
	for p.consumeKeyword("AND") {
		r, err := p.parseNotLevel()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: "AND", L: l, R: r}
	}
	return l, nil
}

// parseNotLevel implements unary NOT, which binds tighter than AND/OR but looser
// than comparisons, IS, IN, BETWEEN and LIKE: "NOT a AND b" is "(NOT a) AND b",
// "NOT a = b" is "NOT (a = b)" (SELECT NOT 0 AND 0 is 0).
func (p *parser) parseNotLevel() (Expr, error) {
	if p.consumeKeyword("NOT") {
		x, err := p.parseNotLevel()
		if err != nil {
			return nil, err
		}
		return UnaryExpr{Op: "NOT", X: x}, nil
	}
	return p.parseComparison()
}

// compareOps is the LOOSER of SQLite's two comparison tiers -- the one that
// also holds IS / IN / LIKE / GLOB / MATCH / REGEXP / BETWEEN / ISNULL /
// NOTNULL. The relational operators below bind TIGHTER than all of these, a
// tier of their own.

// defaultConstantKeywords are the bare identifiers a DEFAULT expression may
// legitimately contain: the boolean literals, which are not column references
// even though this package's expression parser represents them as ColumnExpr
// nodes (see parsePrimary's literal fallback). Every OTHER bare identifier in
// a DEFAULT is a column reference, which is exactly what makes it
// non-constant. SQLite's three time keywords belong to the same category but
// are not listed here: parsePrimary lowers them to date()/time()/datetime()
// calls, and a function call is already constant-eligible.
var defaultConstantKeywords = map[string]bool{
	"TRUE": true, "FALSE": true,
}

// checkDefaultIsConstant rejects a parenthesized "DEFAULT (<expr>)" that
// references any bare identifier: "default value of column [y] is not constant"
// (default.test), whether or not the column exists. Functions, arithmetic,
// CASE, CURRENT_TIMESTAMP, TRUE and NULL are fine. Only the parenthesized form
// can hold one; an unparsable expression is left alone.
func checkDefaultIsConstant(createSQL, colName string, seg []token, j int) error {
	if j+1 >= len(seg) || seg[j+1].kind != tkPunct || seg[j+1].text != "(" {
		return nil
	}
	close := matchParen(seg, j+1)
	if close < 0 || close <= j+2 {
		return nil
	}
	inner := seg[j+2 : close]
	toks := append(append([]token(nil), inner...), token{kind: tkEOF})
	p := newParser(createSQL, toks)
	e, err := p.parseExpr()
	if err != nil {
		// RAISE() is rejected by the shared parser outside a trigger, and
		// C SQLite calls a DEFAULT containing one "not constant" rather
		// than reporting the RAISE error -- verified directly:
		// "c1 INT DEFAULT (RAISE(IGNORE))" is
		// "default value of column [c1] is not constant".
		if strings.Contains(err.Error(), "RAISE() may only be used") {
			return fmt.Errorf("engine: default value of column [%s] is not constant", colName)
		}
		return nil // otherwise unparseable here: not this check's business
	}
	// A subquery is never constant either: exprIsConst walks with
	// sqlite3SelectWalkFail as its SELECT callback (expr.c:2866's
	// sqlite3ExprIsConstantOrFunction, reached from build.c:1742), so
	// "DEFAULT ((SELECT 1))" and "DEFAULT (EXISTS(SELECT 1))" fail the same
	// way a column reference does.
	if exprHasColumnRef(e) || containsSubquery(e) {
		return fmt.Errorf("engine: default value of column [%s] is not constant", colName)
	}
	return nil
}

// parseColumnDefault recovers the column's "DEFAULT ..." clause (DEFAULT at
// seg[j]) as the constant value an INSERT omitting the column stores, before
// affinity. end is just past the clause, so the caller's constraint scan skips
// its tokens (else "DEFAULT (x IS NOT NULL)" marked the column NOT NULL).
//
// parse.y's productions:
//
//	ccons ::= DEFAULT term.        -- NULL|FLOAT|INTEGER|STRING|BLOB|CURRENT_TIME|CURRENT_DATE|CURRENT_TIMESTAMP
//	ccons ::= DEFAULT PLUS term.
//	ccons ::= DEFAULT MINUS term.
//	ccons ::= DEFAULT id.          -- TRUE/FALSE, else the identifier's own TEXT
//	ccons ::= DEFAULT LP expr RP.
//
// "DEFAULT abc" stores 'abc', "DEFAULT +'5'" stores TEXT '5', "DEFAULT -'abc'"
// stores 0, and "DEFAULT 1+2" is a syntax error.
//
// known is false (an INSERT omitting the column then declines) for clock and RNG
// defaults (unreplayableDefaultNames), for anything foldDefaultValue cannot fold
// to a constant (C defers an unknown function to INSERT time, so this is never an
// error here), and for leftover tokens that C's grammar would have rejected.
func parseColumnDefault(createSQL string, seg []token, j int) (val Value, known bool, end int, deferred Expr) {
	k := j + 1
	if k >= len(seg) {
		return Value{}, false, k, nil
	}
	t := seg[k]
	var span []token
	switch {
	case t.kind == tkPunct && t.text == "(":
		closeIdx := matchParen(seg, k)
		if closeIdx < 0 {
			return Value{}, false, len(seg), nil
		}
		span, end = seg[k+1:closeIdx], closeIdx+1
	case t.kind == tkPunct && (t.text == "+" || t.text == "-"):
		if k+1 >= len(seg) || !isDefaultTerm(seg[k+1]) {
			// SQLite's PLUS/MINUS productions take a term, never an id:
			// "DEFAULT +TRUE" is a syntax error (verified directly).
			return Value{}, false, k + 1, nil
		}
		span, end = seg[k:k+2], k+2
	case isDefaultTerm(t):
		span, end = seg[k:k+1], k+1
	case t.kind == tkIdent && (t.quoted || !nonIdentifierKeywords[t.upper()]):
		// The "ccons ::= DEFAULT id" production: SQLite turns the identifier
		// into a STRING literal and then folds the two boolean spellings
		// (sqlite3ExprIdToTrueFalse). A RESERVED keyword is not an id at all
		// ("DEFAULT NOT"/"DEFAULT SELECT"/"DEFAULT PRIMARY" are each a syntax
		// error), so those fall through to the unrecognized case below, which
		// leaves the token for the constraint scanner to make sense of.
		if !t.quoted {
			switch t.upper() {
			case "TRUE":
				return Value{Typ: Int, I: 1}, true, k + 1, nil
			case "FALSE":
				return Value{Typ: Int, I: 0}, true, k + 1, nil
			}
		}
		return Value{Typ: Text, S: []byte(t.text)}, true, k + 1, nil
	default:
		return Value{}, false, k, nil
	}
	if len(span) == 0 {
		return Value{}, false, end, nil
	}
	// A clause naming the CLOCK cannot FOLD, but it can still be kept and
	// evaluated per INSERT, which is what C SQLite does -- see
	// columnInfo.DefaultDeferred. Anything else defaultSpanIsReplayable
	// refuses (random()/randomblob(), a bound parameter) is declined here as
	// it always was.
	foldable := defaultSpanIsReplayable(span)
	if !foldable && !spanIsClockOnlyUnfoldable(span) {
		return Value{}, false, end, nil
	}
	// Parsed through the ordinary expression parser so the clause gets
	// exactly the semantics every other expression in this engine has --
	// including parseUnary's "-9223372036854775808 is one INTEGER token
	// sequence" fold, without which "DEFAULT -9223372036854775808" would
	// store a REAL where C SQLite stores the exact INTEGER (verified
	// directly).
	toks := append(append([]token(nil), span...), token{kind: tkEOF})
	p := newParser(createSQL, toks)
	p.inColumnDefault = true
	e, perr := p.parseExpr()
	if perr != nil || p.peek().kind != tkEOF {
		return Value{}, false, end, nil
	}
	if !foldable {
		// Clock-only: keep the expression instead of folding it. Every insert
		// path evaluates it fresh for the row being stored.
		if deferredDefaultEvaluable(e) {
			return Value{}, false, end, e
		}
		return Value{}, false, end, nil
	}
	v, ok := foldDefaultValue(e)
	return v, ok, end, nil
}

// spanIsClockOnlyUnfoldable reports whether span's unfoldable names are ALL
// clock readings -- no random()/randomblob() anywhere. Matched on the raw
// TOKENS for the same reason defaultSpanIsReplayable is: no walker gap can
// let one through.
func spanIsClockOnlyUnfoldable(span []token) bool {
	sawClock := false
	for _, t := range span {
		if t.kind == tkParam {
			// A CREATE TABLE has no bind context; C SQLite rejects
			// "DEFAULT (?)" outright. Same rule as defaultSpanIsReplayable.
			return false
		}
		if t.kind != tkIdent || t.quoted {
			continue
		}
		if randomDefaultNames[t.upper()] {
			return false
		}
		if unreplayableDefaultNames[t.upper()] {
			sawClock = true
		}
	}
	return sawClock
}

// deferredDefaultEvaluable reports whether e can be evaluated with no row and
// no bind context at INSERT time -- the same conditions foldDefaultValue
// checks, minus the fold itself (which is exactly what fails for a clock
// reading).
func deferredDefaultEvaluable(e Expr) bool {
	if e == nil || containsSubquery(e) || containsAggregate(e) {
		return false
	}
	if _, unsafe := exprCallsUnsafeSchemaFunc(e); unsafe {
		return false
	}
	return checkExprSupported(e) == nil
}

// randomDefaultNames is unreplayableDefaultNames' non-clock half: the two
// whose value can never match an independently-seeded oracle, so a DEFAULT
// naming one stays declined however it is written.
var randomDefaultNames = map[string]bool{"RANDOM": true, "RANDOMBLOB": true}

// isDefaultTerm reports whether t is SQLite's DEFAULT-clause "term"
// nonterminal (parse.y: term ::= NULL|FLOAT|BLOB|STRING|INTEGER|CTIME_KW).
// A QUOTED identifier never is: "DEFAULT \"NULL\"" stores the TEXT 'NULL',
// not SQL NULL (verified directly) -- it is the id production instead.
func isDefaultTerm(t token) bool {
	switch t.kind {
	case tkNumber, tkString, tkBlob:
		return true
	case tkIdent:
		if t.quoted {
			return false
		}
		switch t.upper() {
		case "NULL", "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
			return true
		}
	}
	return false
}

// unreplayableDefaultNames are the identifiers whose appearance anywhere in
// a DEFAULT clause makes its value unreproducible -- the wall clock or the
// random number generator. See parseColumnDefault's doc comment. Matched on
// the raw TOKENS rather than on the parsed tree so no walker gap can let one
// through; a quoted spelling is exempt because it can only ever be an
// identifier (a column, which a DEFAULT may not reference at all), never one
// of these functions or keywords, and a 'now' STRING is a tkString, never a
// tkIdent.
var unreplayableDefaultNames = map[string]bool{
	"CURRENT_TIME": true, "CURRENT_DATE": true, "CURRENT_TIMESTAMP": true,
	"RANDOM": true, "RANDOMBLOB": true,
	"DATE": true, "TIME": true, "DATETIME": true,
	"JULIANDAY": true, "UNIXEPOCH": true, "STRFTIME": true,
}

// defaultSpanIsReplayable reports whether span -- the tokens of one DEFAULT
// clause's expression -- is free of both unreplayableDefaultNames and bound
// parameters (a CREATE TABLE has no bind context, so a "?" could only ever
// fold to NULL; C SQLite rejects it outright as "default value of column
// [v] is not constant", verified directly).
func defaultSpanIsReplayable(span []token) bool {
	for _, t := range span {
		if t.kind == tkParam {
			return false
		}
		if t.kind == tkIdent && !t.quoted && unreplayableDefaultNames[t.upper()] {
			return false
		}
	}
	return true
}

// foldDefaultValue evaluates a DEFAULT expression to its constant value by
// compiling it as a one-column FROM-less program and running it once (as
// foldLimitOffsetExpr does); ok is false when that is not byte-exact. It runs
// only at DDL time (CREATE TABLE text, ALTER ADD COLUMN), never during a
// statement.
//
// This is wider than C: only ALTER's check uses sqlite3ValueFromExpr
// (alter.c:389), and for CREATE TABLE C keeps the expression and codes it per
// INSERT (insert.c:1395, 1407, 1415). The end state is columnInfo carrying the
// DEFAULT's Program; compiling instead of walking the AST is the part done now.
// Subqueries and aggregates are refused first: there is no pager or
// accumulator here.
func foldDefaultValue(e Expr) (Value, bool) {
	if e == nil || containsSubquery(e) || containsAggregate(e) {
		return Value{}, false
	}
	if _, unsafe := exprCallsUnsafeSchemaFunc(e); unsafe {
		return Value{}, false
	}
	if err := checkExprSupported(e); err != nil {
		return Value{}, false
	}
	prog, err := compileSelectNoFrom(&SelectStmt{Columns: []SelectColumn{{Expr: e}}})
	if err != nil {
		return Value{}, false
	}
	rows, err := prog.exec(nil, nil)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		return Value{}, false
	}
	return rows[0][0], true
}

// exprHasColumnRef reports whether e references a column or a bound parameter
// (defaultConstantKeywords excepted). A parameter counts: C rejects
// "DEFAULT (?)" as not constant too. Unhandled node kinds report false.
func exprHasColumnRef(e Expr) bool {
	switch x := e.(type) {
	case ParamExpr:
		return true
	case ColumnExpr:
		return !defaultConstantKeywords[strings.ToUpper(x.Name)]
	case UnaryExpr:
		return exprHasColumnRef(x.X)
	case BinaryExpr:
		return exprHasColumnRef(x.L) || exprHasColumnRef(x.R)
	case CollateExpr:
		return exprHasColumnRef(x.X)
	case CastExpr:
		return exprHasColumnRef(x.X)
	case IsNullExpr:
		return exprHasColumnRef(x.X)
	case LikeExpr:
		return exprHasColumnRef(x.X) || exprHasColumnRef(x.Pattern) || exprHasColumnRef(x.Escape)
	case GlobExpr:
		return exprHasColumnRef(x.X) || exprHasColumnRef(x.Pattern)
	case BetweenExpr:
		return exprHasColumnRef(x.X) || exprHasColumnRef(x.Lo) || exprHasColumnRef(x.Hi)
	case InExpr:
		if exprHasColumnRef(x.X) {
			return true
		}
		for _, a := range x.List {
			if exprHasColumnRef(a) {
				return true
			}
		}
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprHasColumnRef(a) {
				return true
			}
		}
	case CaseExpr:
		if x.Base != nil && exprHasColumnRef(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasColumnRef(w.When) || exprHasColumnRef(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprHasColumnRef(x.Else)
	}
	return false
}

// firstUnknownFuncName returns the name of the first function call in e that
// this engine does not implement. Used only for a GENERATED COLUMN's
// expression, which C SQLite compiles at CREATE TABLE time and therefore
// rejects when the function is undefined. A function C SQLite has but this
// engine does not would also be reported here -- which turns a later
// evaluation failure into an earlier CREATE failure, never a wrong ANSWER,
// and such a table would be unusable here either way.
func firstUnknownFuncName(e Expr) (string, bool) {
	switch x := e.(type) {
	case FuncExpr:
		if !supportedFuncs[r33sFoldIdent(x.Name)] && !isAggregateFuncName(r33sFoldIdent(x.Name)) {
			return x.Name, true
		}
		for _, a := range x.walkArgs() {
			if n, bad := firstUnknownFuncName(a); bad {
				return n, true
			}
		}
	case UnaryExpr:
		return firstUnknownFuncName(x.X)
	case BinaryExpr:
		if n, bad := firstUnknownFuncName(x.L); bad {
			return n, true
		}
		return firstUnknownFuncName(x.R)
	case CollateExpr:
		return firstUnknownFuncName(x.X)
	case CastExpr:
		return firstUnknownFuncName(x.X)
	case IsNullExpr:
		return firstUnknownFuncName(x.X)
	case BetweenExpr:
		for _, a := range []Expr{x.X, x.Lo, x.Hi} {
			if n, bad := firstUnknownFuncName(a); bad {
				return n, true
			}
		}
	case InExpr:
		if n, bad := firstUnknownFuncName(x.X); bad {
			return n, true
		}
		for _, a := range x.List {
			if n, bad := firstUnknownFuncName(a); bad {
				return n, true
			}
		}
	case CaseExpr:
		for _, a := range []Expr{x.Base, x.Else} {
			if a != nil {
				if n, bad := firstUnknownFuncName(a); bad {
					return n, true
				}
			}
		}
		for _, w := range x.Whens {
			if n, bad := firstUnknownFuncName(w.When); bad {
				return n, true
			}
			if n, bad := firstUnknownFuncName(w.Then); bad {
				return n, true
			}
		}
	}
	return "", false
}

// isAggregateFuncName reports whether name is one of the aggregate function
// names, which supportedFuncs (a SCALAR-function set) deliberately excludes.
func isAggregateFuncName(name string) bool {
	switch name {
	case "count", "sum", "total", "avg", "group_concat", "string_agg",
		"min", "max", "json_group_array", "json_group_object",
		"jsonb_group_array", "jsonb_group_object":
		return true
	}
	return false
}

// expandTableConstraintSegments rewrites the comma-split column/constraint
// segment list so every TABLE-level constraint sits in a segment of its own,
// since SQLite's grammar makes the comma between them optional. A segment
// that does not start with a constraint keyword (i.e. a column definition) is
// passed through untouched -- a COLUMN's own constraints are comma-less by
// nature and are parsed by the column path below.
func expandTableConstraintSegments(segments [][]token, startsConstraint map[string]bool) [][]token {
	out := make([][]token, 0, len(segments))
	for _, seg := range segments {
		if len(seg) == 0 || seg[0].kind != tkIdent || !startsConstraint[seg[0].upper()] {
			out = append(out, seg)
			continue
		}
		out = append(out, splitTableConstraints(seg, startsConstraint)...)
	}
	return out
}

// splitTableConstraints cuts one comma-separated segment into its individual
// table constraints. A new one starts at any depth-0 CONSTRAINT/PRIMARY/
// UNIQUE/CHECK/FOREIGN keyword that is not part of the constraint already
// being scanned -- everything inside parentheses (a CHECK expression, a column
// list) is at depth > 0 and can never split.
func splitTableConstraints(seg []token, startsConstraint map[string]bool) [][]token {
	var out [][]token
	i := 0
	for i < len(seg) {
		start := i
		if seg[i].kind == tkIdent && seg[i].upper() == "CONSTRAINT" {
			i++
			// objectNameToken, not tkIdent: a constraint name is SQLite's "nm"
			// production, so 'x' / "x" / [x] / `x` are all legal spellings. A
			// single-quoted one lexes as tkString, and skipping only tkIdent
			// left the following keyword unconsumed -- which silently DROPPED
			// the whole constraint. See parseCreateTableColumnsAndAutoIndexes
			// below for the wrong answers that produced.
			if i < len(seg) {
				if _, ok := objectNameToken(seg[i]); ok {
					i++
				}
			}
			if i >= len(seg) {
				out = append(out, seg[start:i]) // a name with nothing after it
				break
			}
		}
		if !(seg[i].kind == tkIdent && startsConstraint[seg[i].upper()]) {
			// Not a constraint keyword: hand the rest over whole and let the
			// caller's own validation report it.
			out = append(out, seg[start:])
			break
		}
		i++ // the constraint's own keyword (PRIMARY/UNIQUE/CHECK/FOREIGN)
		depth := 0
		for i < len(seg) {
			t := seg[i]
			switch {
			case t.kind == tkPunct && t.text == "(":
				depth++
			case t.kind == tkPunct && t.text == ")":
				depth--
			}
			if depth == 0 && t.kind == tkIdent && startsConstraint[t.upper()] {
				break
			}
			i++
		}
		out = append(out, seg[start:i])
	}
	return out
}

// fkShape is a table-level "FOREIGN KEY (child...) REFERENCES parent(cols...)"
// reduced to what CREATE TABLE must VALIDATE about it. This write path does
// not enforce foreign keys at all, but C SQLite still rejects a
// malformed definition at CREATE time, and accepting one lets every later
// statement in the file diverge (table.test).
type fkShape struct {
	child      []string
	parent     string
	parentCols []string
}

// parseFKConstraintShape pulls the child column list, the referenced table,
// and the referenced column list (if written) out of a table-level FOREIGN
// KEY constraint's tokens.
func parseFKConstraintShape(rest []token) fkShape {
	var fk fkShape
	fk.child = extractParenIdentList(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i].kind != tkIdent || rest[i].upper() != "REFERENCES" {
			continue
		}
		if i+1 < len(rest) && rest[i+1].kind == tkIdent {
			fk.parent = rest[i+1].text
			fk.parentCols = extractParenIdentList(rest[i+1:])
		}
		break
	}
	return fk
}

// validateForeignKeyShapes reproduces C's CREATE TABLE checks on a FOREIGN KEY:
//
//	FOREIGN KEY(a,b) REFERENCES t4(x)   -> number of columns in foreign key
//	                                       does not match the number of
//	                                       columns in the referenced table
//	FOREIGN KEY(nosuch) REFERENCES t4(x)-> unknown column "nosuch" in foreign
//	                                       key definition
//
// A missing referenced column or table is not checked, as in C.
func validateForeignKeyShapes(cols []columnInfo, fks []fkShape) error {
	has := func(name string) bool {
		for _, c := range cols {
			if equalFoldName(c.Name, name) {
				return true
			}
		}
		return false
	}
	for _, fk := range fks {
		if len(fk.parentCols) > 0 && len(fk.child) != len(fk.parentCols) {
			return fmt.Errorf("engine: number of columns in foreign key does not match the number of columns in the referenced table")
		}
		for _, c := range fk.child {
			if !has(c) {
				return fmt.Errorf("engine: unknown column %q in foreign key definition", c)
			}
		}
	}
	return nil
}

// validateNoDuplicateColumns rejects "CREATE TABLE t(a,b,a)" with real
// SQLite's own "duplicate column name: a". The comparison is
// case-INSENSITIVE and a quoted spelling counts, while the message echoes the
// DUPLICATE as written -- verified directly: "(a,b,A)" reports "A"
// (table.test).
func validateNoDuplicateColumns(cols []columnInfo) error {
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		k := r33sFoldIdent(c.Name)
		if seen[k] {
			return fmt.Errorf("engine: duplicate column name: %s", c.Name)
		}
		seen[k] = true
	}
	return nil
}

// nonIdentifierKeywords are the keywords C refuses as a bare column name,
// derived from the oracle per keyword and re-derived by
// compat-harness/vdbe_keyword_column_test.go. CROSS, FULL, INNER, LEFT,
// NATURAL, OUTER, RIGHT, OVER, FILTER, WINDOW and the rest are valid names.
var nonIdentifierKeywords = map[string]bool{
	"ADD": true, "ALL": true, "ALTER": true, "AND": true, "AS": true,
	"AUTOINCREMENT": true, "BETWEEN": true, "CASE": true, "CHECK": true,
	"COLLATE": true, "COMMIT": true, "CONSTRAINT": true, "CREATE": true,
	"DEFAULT": true, "DEFERRABLE": true, "DELETE": true, "DISTINCT": true,
	"DROP": true, "ELSE": true, "ESCAPE": true, "EXCEPT": true, "EXISTS": true,
	"FOREIGN": true, "FROM": true, "GROUP": true, "HAVING": true, "IN": true,
	"INDEX": true, "INSERT": true, "INTERSECT": true, "INTO": true, "IS": true,
	"ISNULL": true, "JOIN": true, "LIMIT": true, "NOT": true, "NOTHING": true,
	"NOTNULL": true, "NULL": true, "ON": true, "OR": true, "ORDER": true,
	"PRIMARY": true, "REFERENCES": true, "RETURNING": true, "SELECT": true,
	"SET": true, "TABLE": true, "THEN": true, "TO": true, "TRANSACTION": true,
	"UNION": true, "UNIQUE": true, "UPDATE": true, "USING": true, "VALUES": true,
	"WHEN": true, "WHERE": true,
}

var compareOps = map[string]bool{
	"=": true, "==": true, "!=": true, "<>": true,
}

// relationalOps bind tighter than compareOps, as in SQLite's precedence table:
//
//	SELECT 0 LIKE 0 < 2          -> 0, i.e. 0 LIKE (0 < 2), not (0 LIKE 0) < 2
//	SELECT 0 == 0 < 2            -> 0, i.e. 0 == (0 < 2)
//	SELECT 2 BETWEEN 1 AND 2 < 3 -> 0, i.e. 2 BETWEEN 1 AND (2 < 3)
var relationalOps = map[string]bool{
	"<": true, "<=": true, ">": true, ">=": true,
}

// desugarRowCompare rewrites a comparison of two row values into the equivalent
// scalar boolean tree, or returns false (the BinaryExpr is kept and declined
// downstream as "row value misused"):
//
//	(a,b) =  (c,d)   ==  a=c  AND b=d          "==" behaves identically to "="
//	(a,b) <> (c,d)   ==  a<>c OR  b<>d          "!=" identically to "<>"
//	(a,b) IS (c,d)   ==  a IS c AND b IS d      (NULL-safe; never yields NULL)
//	(a,b) IS NOT (c,d) == a IS NOT c OR b IS NOT d
//
// which gives C's NULL behavior for free. "<" etc. go through desugarRowLex.
// Row vs scalar and mismatched arity return false. A subquery operand is left
// for compileRowSubCompare, since these rewrites evaluate operands more than
// once. Both operands must be RowExprs of equal arity.
func desugarRowCompare(op string, l, r Expr) (Expr, bool) {
	lr, lok := l.(RowExpr)
	rr, rok := r.(RowExpr)
	if !lok && !rok {
		return nil, false // ordinary scalar comparison; nothing to do
	}
	if !lok || !rok || len(lr.Elems) != len(rr.Elems) {
		// One side is a row value and the other is not, or the two arities
		// differ: not a valid row-value comparison. Leave it to be declined.
		return nil, false
	}
	var combine string
	switch op {
	case "=", "==", "IS":
		combine = "AND"
	case "!=", "<>", "IS NOT":
		combine = "OR"
	case "<", "<=", ">", ">=":
		acc, ok := desugarRowLex(op, lr.Elems, rr.Elems)
		if !ok {
			return nil, false
		}
		return withRowOrigin(acc, &rowValueOrigin{op: op, l: lr.Elems, r: rr.Elems}), true
	default:
		return nil, false
	}
	var acc Expr
	for i := range lr.Elems {
		cmp := BinaryExpr{Op: op, L: lr.Elems[i], R: rr.Elems[i]}
		if acc == nil {
			acc = cmp
		} else {
			acc = BinaryExpr{Op: combine, L: acc, R: cmp}
		}
	}
	return withRowOrigin(acc, &rowValueOrigin{op: op, l: lr.Elems, r: rr.Elems}), true
}

// withRowOrigin records on a desugared tree's root the row-value comparison it
// was built from (BinaryExpr.row). A RowExpr always has two or more elements,
// so every desugaring's root is a BinaryExpr -- an AND or an OR -- and the
// type test is only a guard.
func withRowOrigin(e Expr, o *rowValueOrigin) Expr {
	if b, ok := e.(BinaryExpr); ok {
		b.row = o
		return b
	}
	return e
}

// desugarRowLex rewrites a lexicographic row comparison ("<", "<=", ">", ">=")
// of equal-length lists:
//
//	(a1,...,aN) OP (b1,...,bN)
//	    ==  a1 <strict> b1  OR  (a1 = b1 AND (a2,...,aN) OP (b2,...,bN))
//	(a1) OP (b1)
//	    ==  a1 OP b1                                        (base case)
//
// <strict> drops a trailing "=", and the chaining test is "=", not IS, which
// gives the right three-valued results:
//
//	(NULL,1) < (1,2)  ->  NULL<1 OR (NULL=1 AND 1<2)  ->  NULL OR NULL  -> NULL
//	(1,NULL) < (2,3)  ->  1<2   OR (...)              ->  1 OR ...      -> 1
//	(1,NULL) < (1,3)  ->  1<1   OR (1=1 AND NULL<3)   ->  0 OR NULL     -> NULL
//	(1,NULL) < (0,3)  ->  1<0   OR (1=0 AND NULL<3)   ->  0 OR 0        -> 0
//
// Verified exhaustively against C (compat-harness/vdbe_rowvalue_test.go).
// Collation and affinity follow per element. Elements before the last appear
// twice, so a comparison containing random()/randomblob() is declined.
func desugarRowLex(op string, ls, rs []Expr) (Expr, bool) {
	if len(ls) == 0 {
		return nil, false // arity 0 is not reachable from the grammar
	}
	if len(ls) == 1 {
		return BinaryExpr{Op: op, L: ls[0], R: rs[0]}, true
	}
	if exprCallsNondeterministicFunc(ls[0]) || exprCallsNondeterministicFunc(rs[0]) {
		return nil, false // would be evaluated twice; see the doc comment
	}
	tail, ok := desugarRowLex(op, ls[1:], rs[1:])
	if !ok {
		return nil, false
	}
	strict := strings.TrimSuffix(op, "=") // "<=" -> "<", ">=" -> ">"
	return BinaryExpr{
		Op: "OR",
		L:  BinaryExpr{Op: strict, L: ls[0], R: rs[0]},
		R: BinaryExpr{
			Op: "AND",
			L:  BinaryExpr{Op: "=", L: ls[0], R: rs[0]},
			R:  tail,
		},
	}, true
}

// desugarRowBetween rewrites "X [NOT] BETWEEN Lo AND Hi" for row values:
//
//	(a,b) BETWEEN (l1,l2) AND (h1,h2)  ==  (a,b)>=(l1,l2) AND (a,b)<=(h1,h2)
//	(a,b) NOT BETWEEN ...              ==  NOT ( the above )
//
// the same identity compileBetween uses for scalars (verified exhaustively). All
// three must be row values of one arity, else "row value misused". X is written
// four times, so it carries desugarRowLex's single-evaluation restriction.
func desugarRowBetween(x, lo, hi Expr, not bool) (Expr, bool) {
	xr, ok := x.(RowExpr)
	if !ok {
		return nil, false // ordinary scalar BETWEEN; nothing to do
	}
	for _, el := range xr.Elems {
		if exprCallsNondeterministicFunc(el) {
			return nil, false
		}
	}
	ge, ok := desugarRowCompare(">=", xr, lo)
	if !ok {
		return nil, false
	}
	le, ok := desugarRowCompare("<=", xr, hi)
	if !ok {
		return nil, false
	}
	var acc Expr = BinaryExpr{Op: "AND", L: ge, R: le,
		row: &rowValueOrigin{op: "BETWEEN", l: xr.Elems, lo: lo.(RowExpr).Elems, hi: hi.(RowExpr).Elems}}
	if not {
		acc = UnaryExpr{Op: "NOT", X: acc}
	}
	return acc, true
}

// desugarRowIn rewrites "X [NOT] IN ((...),(...))" for a row value X and a
// value list of same-arity rows into a disjunction of row equalities:
//
//	(a,b) IN ((1,2),(3,4))  ==  (a,b)=(1,2) OR (a,b)=(3,4)
//
// NOT IN is its negation. An empty list is foldEmptyIn's. The subquery form is
// left for compileInSubquery. With two or more list rows X is written more than
// once, so a nondeterministic X declines: C evaluates it once
// ("(abs(random())%2, 1) IN ((0,1),(1,1))" is always 1).
func desugarRowIn(x Expr, list []Expr, sub *SelectStmt, not bool) (Expr, bool) {
	xr, ok := x.(RowExpr)
	if !ok {
		return nil, false // ordinary scalar IN; nothing to do
	}
	if sub != nil {
		return nil, false // subquery right-hand side: not supported here
	}
	// An EMPTY list never reaches here: makeIn folds "<anything> IN ()" for
	// every operand shape, row value included, before consulting this function
	// (parse.y:1491 -- see foldEmptyIn).
	if len(list) > 1 {
		for _, el := range xr.Elems {
			if exprCallsNondeterministicFunc(el) {
				return nil, false // would be evaluated once per list element
			}
		}
	}
	if len(list) > 1 {
		var ok bool
		if list, ok = rowInKeyCollations(xr, list); !ok {
			return nil, false
		}
	}
	var acc Expr
	rows := make([][]Expr, 0, len(list))
	for _, el := range list {
		eq, ok := desugarRowCompare("=", xr, el)
		if !ok {
			// A list element is not a same-arity row value: not a supported
			// row-value IN. Decline the whole thing.
			return nil, false
		}
		rows = append(rows, el.(RowExpr).Elems)
		if acc == nil {
			acc = eq
		} else {
			acc = BinaryExpr{Op: "OR", L: acc, R: eq}
		}
	}
	// C's parser makes the list "IN (VALUES ...)" (parse.y:1531,
	// sqlite3ExprListToValues): one IN term however long the list, so even a
	// one-row list's root carries the IN rather than the equality it became.
	acc = withRowOrigin(acc, &rowValueOrigin{op: "IN", l: xr.Elems, list: rows})
	if not {
		acc = UnaryExpr{Op: "NOT", X: acc}
	}
	return acc, true
}

// rowInKeyCollations gives every row of a row-value IN list the collation C
// compares it under. C tests against one ephemeral index whose per-field
// collation comes from the LHS element or else the last VALUES row
// (expr.c:3750), while the desugared OR compares each row under its own. So an
// explicit COLLATE is removed from every row but the last, and the last row's
// applied to all. A COLLATE below an element's top is refused.
func rowInKeyCollations(xr RowExpr, list []Expr) ([]Expr, bool) {
	last, isRow := list[len(list)-1].(RowExpr)
	if !isRow || len(last.Elems) != len(xr.Elems) {
		return list, true // not a same-arity row list: desugarRowCompare declines
	}
	top := func(e Expr) (Expr, string, bool) {
		if c, isColl := e.(CollateExpr); isColl {
			e = c.X
			if _, deeper := exprCollation(e); deeper {
				return nil, "", false
			}
			return e, c.Name, true
		}
		if _, explicit := exprCollation(e); explicit {
			return nil, "", false
		}
		return e, "", true
	}
	names := make([]string, len(xr.Elems))
	for i, e := range last.Elems {
		_, name, ok := top(e)
		if !ok {
			return nil, false
		}
		names[i] = name
	}
	out := make([]Expr, len(list))
	for k, el := range list {
		r, isRow := el.(RowExpr)
		if !isRow || len(r.Elems) != len(xr.Elems) {
			return list, true
		}
		elems := make([]Expr, len(r.Elems))
		for i, e := range r.Elems {
			if _, lhsExplicit := exprCollation(xr.Elems[i]); lhsExplicit {
				elems[i] = e // the LHS's own sequence wins every row alike
				continue
			}
			base, _, ok := top(e)
			if !ok {
				return nil, false
			}
			if names[i] != "" {
				base = CollateExpr{X: base, Name: names[i]}
			}
			elems[i] = base
		}
		out[k] = RowExpr{Elems: elems}
	}
	return out, true
}

// makeIn builds the node for "X [NOT] IN <rhs>" after parseInRHS has classified
// the right-hand side. It is the single place SQLite's own grammar action for
// that production lives (parse.y:1491), so both spellings -- the "IN" case and
// the postfix "NOT IN" case in parseComparison -- go through it.
func makeIn(x Expr, list []Expr, sub *SelectStmt, not bool) Expr {
	if sub == nil && len(list) == 0 {
		return foldEmptyIn(x, not)
	}
	if de, ok := desugarRowIn(x, list, sub, not); ok {
		return de
	}
	return InExpr{X: x, List: list, Sub: sub, Not: not}
}

// foldEmptyIn implements SQLite's empty value-list fold (parse.y:1491):
//
//	expr(A) ::= expr(A) in_op(N) LP exprlist(Y) RP. [IN] {
//	  if( Y==0 ){
//	    Expr *pB = sqlite3Expr(pParse->db, TK_STRING, N ? "true" : "false");
//	    if( pB ) sqlite3ExprIdToTrueFalse(pB);
//	    if( !ExprHasProperty(A, EP_HasFunc) ){
//	      sqlite3ExprUnmapAndDelete(pParse, A);
//	      A = pB;
//	    }else{
//	      A = sqlite3PExpr(pParse, N ? TK_OR : TK_AND, pB, A);
//	    }
//	  }else{ ... }
//
// It is a grammar action, so the operand is dropped unresolved: "nosuchcol IN
// ()" is fine, as are "CHECK(b IN ())" and an index WHERE naming a missing
// column (altertab3.test, alterqf.test).
//
// The constant is TK_TRUEFALSE, which sqlite3ExprIsInteger does not treat as an
// ordinal, so "ORDER BY (x NOT IN ())" sorts by a constant (and is an error in a
// compound). It is encoded here as "NOT <opposite constant>", which has the same
// value, no affinity or collation, and is not an ordinal.
//
// With EP_HasFunc the operand stays under the AND/OR, since a function might be
// an aggregate that changes the query's shape ("SELECT count(*) IN () FROM t"
// is one row). pB is the left operand, as in C. A surviving row value is spread
// over the chain element by element: C never codes the vector (expr.c:4874,
// 2373), but the resolver and analyzeAggregate still walk it (rowvalue.test
// 36.0-2):
//
//	SELECT (1,2,3,max(x)) IN () FROM t1;   -- ONE row (aggregate), 0
//	SELECT (max(x),1,23) IN () FROM t1;    -- ONE row (aggregate), 0
func foldEmptyIn(x Expr, not bool) Expr {
	// "NOT <opposite>" is 1 for "NOT IN ()" and 0 for "IN ()".
	pB := UnaryExpr{Op: "NOT", X: LiteralExpr{Val: boolValue(!not)}}
	if !exprHasFuncCall(x) {
		return pB
	}
	op := "AND"
	if not {
		op = "OR"
	}
	if row, isRow := x.(RowExpr); isRow {
		var acc Expr = pB
		for _, el := range row.Elems {
			acc = BinaryExpr{Op: op, L: acc, R: el}
		}
		return acc
	}
	return BinaryExpr{Op: op, L: pB, R: x}
}

// exprHasFuncCall reports EP_HasFunc (sqliteInt.h:3112) for a parsed operand.
// Only sqlite3ExprFunction (expr.c:1191) sets it, and it propagates through
// sqlite3ExprAttachSubtrees (expr.c:1002) and through x.pList (exprSetHeight,
// expr.c:850), but not through a subquery (x.pSelect) or COLLATE
// (sqlite3ExprAddCollateToken assigns pLeft directly). So
// "(SELECT abs(b)) IN ()" and "abs(a) COLLATE nocase IN ()" fold, while
// "abs(a) IN ()" does not.
//
//	TK_FUNCTION   sqlite3ExprFunction    FuncExpr, and the infix functions the
//	                                     same routine builds: LIKE/GLOB/MATCH
//	                                     (parse.y:1363 likeop) and -> / ->>
//	                                     (parse.y:306 PTR). CTIME_KW is one too
//	                                     (parse.y:1330), and parsePrimary
//	                                     already lowers it to FuncExpr.
//	TK_VECTOR     parse.y:1333           RowExpr -- propagates from its elements
//	                                     explicitly, in the grammar action.
//	TK_CAST       parse.y:1228           CastExpr -- ExprAttachSubtrees(E).
//	TK_CASE       parse.y:1569           CaseExpr -- pLeft is the base, x.pList
//	                                     the WHEN/THEN pairs plus ELSE.
//	TK_BETWEEN    parse.y:1475           BetweenExpr -- pLeft, x.pList={Lo,Hi}.
//	TK_IN         parse.y:1538           InExpr -- pLeft plus x.pList, EXCEPT
//	                                     for the subquery arm (x.pSelect).
//	TK_RAISE      parse.y:1828           RaiseExpr -- the message is pLeft.
//	TK_COLLATE    sqlite3ExprAddCollateToken -- blocked, see above.
func exprHasFuncCall(e Expr) bool {
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr, ColumnExpr:
		return false
	case FuncExpr, LikeExpr, GlobExpr, MatchExpr:
		return true
	case CollateExpr, SubqueryExpr, ExistsExpr:
		return false
	case UnaryExpr:
		return exprHasFuncCall(x.X)
	case BinaryExpr:
		return exprHasFuncCall(x.L) || exprHasFuncCall(x.R)
	case IsNullExpr:
		return exprHasFuncCall(x.X)
	case CastExpr:
		return exprHasFuncCall(x.X)
	case RaiseExpr:
		return exprHasFuncCall(x.Msg)
	case BetweenExpr:
		return exprHasFuncCall(x.X) || exprHasFuncCall(x.Lo) || exprHasFuncCall(x.Hi)
	case InExpr:
		if exprHasFuncCall(x.X) {
			return true
		}
		if x.Sub != nil {
			return false // x.pSelect: EP_Propagate does not cross it
		}
		for _, it := range x.List {
			if exprHasFuncCall(it) {
				return true
			}
		}
		return false
	case CaseExpr:
		if exprHasFuncCall(x.Base) || exprHasFuncCall(x.Else) {
			return true
		}
		for _, w := range x.Whens {
			if exprHasFuncCall(w.When) || exprHasFuncCall(w.Then) {
				return true
			}
		}
		return false
	case RowExpr:
		for _, el := range x.Elems {
			if exprHasFuncCall(el) {
				return true
			}
		}
		return false
	default:
		// An operand shape this switch does not name. EP_HasFunc's job here is
		// to decide whether the operand SURVIVES the fold; claiming it does is
		// the conservative answer, since that keeps the tree (and any name in
		// it) exactly where an unfolded IN() would have left it.
		return true
	}
}

// desugarRowCase rewrites a base-form CASE whose base and WHEN labels are row
// values into a searched CASE using "=" (not IS):
//
//	CASE (b,c) WHEN (1,2) THEN 'x' ELSE '-' END
//	    ==  CASE WHEN (b,c)=(1,2) THEN 'x' ELSE '-' END
//
// so "CASE (NULL,2) WHEN (NULL,2) ..." takes ELSE (rowvalue8.test). A
// multi-column subquery base against row labels is left as a BinaryExpr for
// compileRowSubCompare (rowSubOperands); an empty subquery reads as NULLs and
// falls to ELSE. With more than one label the base is written more than once,
// so a nondeterministic base declines.
func desugarRowCase(ce CaseExpr) (Expr, bool) {
	if ce.Base == nil {
		return nil, false // searched CASE: no base to compare labels against
	}
	baseRow, baseIsRow := ce.Base.(RowExpr)
	involvesRow := baseIsRow
	for _, w := range ce.Whens {
		if _, ok := w.When.(RowExpr); ok {
			involvesRow = true
		}
	}
	if !involvesRow {
		// An ordinary scalar base-form CASE, which compileCase already
		// compiles directly. Leave it completely alone.
		return nil, false
	}
	if len(ce.Whens) > 1 {
		dup := []Expr{ce.Base}
		if baseIsRow {
			dup = baseRow.Elems
		}
		for _, el := range dup {
			if exprCallsNondeterministicFunc(el) {
				return nil, false // would be evaluated once per label
			}
		}
	}
	out := CaseExpr{Whens: make([]WhenClause, 0, len(ce.Whens)), Else: ce.Else}
	for _, w := range ce.Whens {
		cond, ok := desugarRowCompare("=", ce.Base, w.When)
		if !ok {
			// The one pair desugarRowCompare declines but this engine still
			// answers: a row value against a multi-column subquery, in either
			// operand order. Anything else (an arity mismatch, a row value
			// against a scalar) stays declined, matching SQLite's own "row
			// value misused" for those spellings.
			if _, _, _, isRowSub := rowSubOperands("=", ce.Base, w.When); !isRowSub {
				return nil, false
			}
			cond = BinaryExpr{Op: "=", L: ce.Base, R: w.When}
		}
		out.Whens = append(out.Whens, WhenClause{When: cond, Then: w.Then})
	}
	return out, true
}

// rowOrBinary builds a comparison of L and R: the row-value desugaring when
// either operand is a supported row value, else the ordinary scalar
// BinaryExpr. Used at every comparison-construction site in parseComparison so
// row values are handled uniformly wherever a comparison operator appears.
func rowOrBinary(op string, l, r Expr) Expr {
	if de, ok := desugarRowCompare(op, l, r); ok {
		return de
	}
	return BinaryExpr{Op: op, L: l, R: r}
}

// parseRelational is SQLite's "< <= > >=" precedence tier, which binds
// TIGHTER than everything parseComparison handles (= == != <> IS IN LIKE GLOB
// MATCH REGEXP BETWEEN ISNULL NOTNULL) -- see relationalOps for the evidence.
func (p *parser) parseRelational() (Expr, error) {
	left, err := p.parseBitwise()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tkPunct || !relationalOps[t.text] {
			return left, nil
		}
		op := t.text
		p.next()
		r, err := p.parseBitwise()
		if err != nil {
			return nil, err
		}
		left = rowOrBinary(op, left, r)
	}
}

func (p *parser) parseComparison() (Expr, error) {
	left, err := p.parseRelational()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == tkIdent && t.upper() == "ISNULL":
			p.next()
			left = IsNullExpr{X: left}
		case t.kind == tkIdent && t.upper() == "NOTNULL":
			p.next()
			left = IsNullExpr{X: left, Not: true}
		case t.kind == tkIdent && t.upper() == "IS":
			p.next()
			not := p.consumeKeyword("NOT")
			switch {
			case p.consumeKeyword("NULL"):
				left = IsNullExpr{X: left, Not: not}
			case isBareBoolKeyword(p.peek()):
				// "X IS [NOT] TRUE/FALSE" is a truth test, a different production from
				// "X IS <expr>": "2 IS TRUE" is 1. Desugared (desugarIsBool) into existing node
				// kinds rather than a new Expr kind, so no tree walker can miss it.
				kw := p.next()
				want := kw.upper() == "TRUE"
				ident := ColumnExpr{Name: kw.text, FallbackLiteral: boolKeywordFallback(kw)}
				// "X IS TRUE COLLATE c": C parses it as "X IS (TRUE COLLATE c)" and recognizes
				// the truth test through sqlite3ExprSkipCollate, which drops the collation
				// unresolved, so the name is consumed and not validated ("0.5 IS TRUE COLLATE
				// BOGUS" is 1). Chained COLLATEs likewise.
				for p.peekIsKeyword("COLLATE") {
					p.next()
					if p.peek().kind != tkIdent {
						return nil, fmt.Errorf("engine: expected collation name after COLLATE")
					}
					p.next()
				}
				if p.inColumnDefault {
					left = defaultClauseIsBool(left, want, not)
				} else {
					left = desugarIsBool(left, ident, want, not)
				}
			case p.peekIsKeyword("DISTINCT"):
				// "X IS [NOT] DISTINCT FROM Y" is NULL-safe (in)equality: IS DISTINCT FROM is
				// "IS NOT", IS NOT DISTINCT FROM is "IS".
				p.next() // DISTINCT
				if !p.consumeKeyword("FROM") {
					return nil, fmt.Errorf("engine: expected FROM after DISTINCT, got %q", p.tokenDesc(p.peek()))
				}
				r, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				op := "IS NOT"
				if not {
					op = "IS"
				}
				left = rowOrBinary(op, left, r)
			default:
				r, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				// The same truth test with the keyword parenthesized or under COLLATE: "2 IS
				// (TRUE)" is 1, since "LP expr RP" leaves no node (parse.y:1176) and resolve
				// tests through sqlite3ExprSkipCollateAndLikely (resolve.c:1422). likely() and
				// unary "+" are not skipped ("2 IS likely(TRUE)" is 0).
				kw, isKw := boolKeywordOperand(r)
				switch {
				case isKw && p.inColumnDefault:
					left = defaultClauseIsBool(left, kw.FallbackLiteral.I != 0, not)
				case isKw:
					left = desugarIsBool(left, kw, kw.FallbackLiteral.I != 0, not)
				default:
					op := "IS"
					if not {
						op = "IS NOT"
					}
					left = rowOrBinary(op, left, r)
				}
			}
		case t.kind == tkIdent && t.upper() == "IN":
			p.next()
			list, sub, err := p.parseInRHS()
			if err != nil {
				return nil, err
			}
			left = makeIn(left, list, sub, false)
		case t.kind == tkIdent && t.upper() == "BETWEEN":
			p.next()
			lo, hi, err := p.parseBetweenBounds()
			if err != nil {
				return nil, err
			}
			if de, ok := desugarRowBetween(left, lo, hi, false); ok {
				left = de
			} else {
				left = BetweenExpr{X: left, Lo: lo, Hi: hi}
			}
		case t.kind == tkIdent && t.upper() == "LIKE":
			p.next()
			pat, err := p.parseRelational()
			if err != nil {
				return nil, err
			}
			esc, err := p.parseLikeEscape()
			if err != nil {
				return nil, err
			}
			left = LikeExpr{X: left, Pattern: pat, Escape: esc}
		case t.kind == tkIdent && t.upper() == "GLOB":
			p.next()
			pat, err := p.parseRelational()
			if err != nil {
				return nil, err
			}
			left = GlobExpr{X: left, Pattern: pat}
		case t.kind == tkIdent && t.upper() == "MATCH":
			p.next()
			pat, err := p.parseRelational()
			if err != nil {
				return nil, err
			}
			left = MatchExpr{X: left, Pattern: pat}
		case t.kind == tkIdent && t.upper() == "REGEXP":
			p.next()
			pat, err := p.parseRelational()
			if err != nil {
				return nil, err
			}
			left = regexpCall(left, pat)
		case t.kind == tkIdent && t.upper() == "NOT":
			save := p.pos
			p.next()
			switch {
			case p.consumeKeyword("NULL"):
				// Postfix "expr NOT NULL": an alternate spelling of "expr
				// NOTNULL" / "expr IS NOT NULL", distinct from a leading unary
				// NOT (parseNotLevel) and from "IS NOT NULL" (handled above,
				// under the "IS" case) -- disambiguated here purely by
				// lookahead (NOT immediately followed by NULL in postfix
				// position, i.e. with a left operand already parsed).
				left = IsNullExpr{X: left, Not: true}
			case p.consumeKeyword("IN"):
				list, sub, err := p.parseInRHS()
				if err != nil {
					return nil, err
				}
				left = makeIn(left, list, sub, true)
			case p.consumeKeyword("BETWEEN"):
				lo, hi, err := p.parseBetweenBounds()
				if err != nil {
					return nil, err
				}
				if de, ok := desugarRowBetween(left, lo, hi, true); ok {
					left = de
				} else {
					left = BetweenExpr{X: left, Lo: lo, Hi: hi, Not: true}
				}
			case p.consumeKeyword("LIKE"):
				pat, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				esc, err := p.parseLikeEscape()
				if err != nil {
					return nil, err
				}
				left = LikeExpr{X: left, Pattern: pat, Escape: esc, Not: true}
			case p.consumeKeyword("GLOB"):
				pat, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				left = GlobExpr{X: left, Pattern: pat, Not: true}
			case p.consumeKeyword("MATCH"):
				pat, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				left = MatchExpr{X: left, Pattern: pat, Not: true}
			case p.consumeKeyword("REGEXP"):
				pat, err := p.parseRelational()
				if err != nil {
					return nil, err
				}
				left = UnaryExpr{Op: "NOT", X: regexpCall(left, pat)}
			default:
				p.pos = save
				return left, nil
			}
		case t.kind == tkPunct && (compareOps[t.text] || relationalOps[t.text]):
			// A relational operator is handled HERE as well as in
			// parseRelational, so that a LOOSER construct's result can still
			// be its left operand -- SQLite's grammar is a yacc precedence
			// table, not nested productions, and reduces "1 IN (0,1)" to an
			// expr that "< 2" then applies to. Verified directly:
			// "SELECT 1 IN (0,1) < 2" is 1, i.e. (1 IN (0,1)) < 2. The RHS is
			// still parsed at the tighter tier, which is what makes
			// "0 LIKE 0 < 2" group as 0 LIKE (0 < 2).
			op := t.text
			p.next()
			r, err := p.parseRelational()
			if err != nil {
				return nil, err
			}
			left = rowOrBinary(op, left, r)
		default:
			return left, nil
		}
	}
}

// regexpCall builds "X REGEXP Y" as the call regexp(Y, X), pattern first, as C's
// grammar does. No build ships a regexp() function, so it fails at run time with
// "no such function: REGEXP", not at parse time (a CREATE TRIGGER using it is
// accepted).
func regexpCall(x, pattern Expr) Expr {
	return FuncExpr{Name: "regexp", Args: []Expr{pattern, x}}
}

// parseInRHS parses the right side of "X [NOT] IN": a parenthesized list or
// subquery (parseInClause), or a bare table name, which is desugared to
// "IN (SELECT * FROM <table>)"; a table with more than one column then fails the
// subquery's one-column check.
func (p *parser) parseInRHS() (list []Expr, sub *SelectStmt, err error) {
	if p.peekIsPunct("(") {
		return p.parseInClause()
	}
	t := p.peek()
	if t.kind != tkIdent {
		return nil, nil, fmt.Errorf("engine: expected '(' or a table name after IN, got %q", p.tokenDesc(t))
	}
	p.next()
	tableSub := &SelectStmt{
		Columns: []SelectColumn{{Star: true}},
		From:    []FromItem{{Table: t.text}},
	}
	return nil, tableSub, nil
}

// parseInClause parses the parenthesized right-hand side of "X [NOT] IN
// (...)": either a subquery ("(SELECT ...)", returned as sub with list ==
// nil) or a comma-separated value list (returned as list with sub == nil).
// The two forms are disambiguated by a single-token lookahead for SELECT (or
// WITH, or VALUES -- see peekStartsInSubquery) right after '(', much like
// parsePrimary's "(" case for a plain scalar subquery.
func (p *parser) parseInClause() (list []Expr, sub *SelectStmt, err error) {
	if err := p.expectPunct("("); err != nil {
		return nil, nil, err
	}
	if p.peekStartsInSubquery() {
		s, err := p.parseSelectStmt()
		if err != nil {
			return nil, nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, nil, err
		}
		return nil, s, nil
	}
	if p.peekIsPunct(")") {
		p.next()
		return list, nil, nil
	}
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, nil, err
		}
		list = append(list, e)
		if p.peekIsPunct(",") {
			p.next()
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, nil, err
	}
	// "X IN ((SELECT ...))": a redundantly-parenthesized subquery is NOT a
	// one-element list containing a scalar subquery (which would match only
	// the subquery's first row); SQLite treats it as the subquery form,
	// matching every row. A single list element that is a bare subquery
	// (any depth of extra parens flattens to one SubqueryExpr) is therefore
	// unwrapped to the sub form. A multi-element list, or a subquery inside
	// a larger expression (e.g. "(SELECT ...)+0"), stays a value list.
	if len(list) == 1 {
		if sq, ok := list[0].(SubqueryExpr); ok {
			return nil, sq.Stmt, nil
		}
	}
	return list, nil, nil
}

func (p *parser) parseBetweenBounds() (lo, hi Expr, err error) {
	// Both bounds parse at the RELATIONAL tier, which binds tighter than
	// BETWEEN itself: verified directly, "SELECT 2 BETWEEN 1 AND 2 < 3" is 0,
	// i.e. 2 BETWEEN 1 AND (2 < 3), not (2 BETWEEN 1 AND 2) < 3.
	lo, err = p.parseRelational()
	if err != nil {
		return nil, nil, err
	}
	if !p.consumeKeyword("AND") {
		return nil, nil, fmt.Errorf("engine: expected AND in BETWEEN, got %q", p.tokenDesc(p.peek()))
	}
	hi, err = p.parseRelational()
	if err != nil {
		return nil, nil, err
	}
	return lo, hi, nil
}

// parseLikeEscape parses an optional "ESCAPE <expr>" clause trailing a LIKE
// pattern (C SQLite's grammar allows an arbitrary expression here, not
// just a literal -- verified directly: a computed concatenation as the
// ESCAPE expression is accepted and evaluated per row -- so this is just
// another parseConcat, same precedence as the pattern itself). Returns
// nil, nil when no ESCAPE keyword follows.
func (p *parser) parseLikeEscape() (Expr, error) {
	if !p.consumeKeyword("ESCAPE") {
		return nil, nil
	}
	return p.parseBitwise()
}

// parseBitwise implements SQLite's binary bitwise tier (& | << >>): looser
// than + and -, tighter than the comparison operators, all four sharing one
// left-associative tier. It sits directly above parseConcat here because
// this package's "||" is (pre-existing, see parseArrow's comment) parsed at
// a looser tier than C SQLite's -- so the bitwise tier is inserted
// relative to the EXISTING chain, keeping & | << >> looser than || and +/-
// and tighter than comparisons, which is what C SQLite does for every
// operand shape the corpus exercises.
func (p *parser) parseBitwise() (Expr, error) {
	l, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for p.peekIsPunct("&") || p.peekIsPunct("|") || p.peekIsPunct("<<") || p.peekIsPunct(">>") {
		op := p.next().text
		r, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: op, L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseConcat() (Expr, error) {
	l, err := p.parseAddSub()
	if err != nil {
		return nil, err
	}
	for p.peekIsPunct("||") {
		p.next()
		r, err := p.parseAddSub()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: "||", L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAddSub() (Expr, error) {
	l, err := p.parseMulDiv()
	if err != nil {
		return nil, err
	}
	for p.peekIsPunct("+") || p.peekIsPunct("-") {
		op := p.next().text
		r, err := p.parseMulDiv()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: op, L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseMulDiv() (Expr, error) {
	l, err := p.parseArrow()
	if err != nil {
		return nil, err
	}
	for p.peekIsPunct("*") || p.peekIsPunct("/") || p.peekIsPunct("%") {
		op := p.next().text
		r, err := p.parseArrow()
		if err != nil {
			return nil, err
		}
		l = BinaryExpr{Op: op, L: l, R: r}
	}
	return l, nil
}

// parseArrow implements the json1 "->" and "->>" operators, which bind tighter
// than * and / ("'{"a":1}' -> '$.a' * 2" is 2). In SQLite they share a tier with
// "||", which this parser places lower. Each becomes a FuncExpr named "->" or
// "->>" (jsonExtractFunc), run by the ordinary function machinery.
func (p *parser) parseArrow() (Expr, error) {
	l, err := p.parseCollate()
	if err != nil {
		return nil, err
	}
	for p.peekIsPunct("->") || p.peekIsPunct("->>") {
		op := p.next().text
		r, err := p.parseCollate()
		if err != nil {
			return nil, err
		}
		l = FuncExpr{Name: op, Args: []Expr{l, r}}
	}
	return l, nil
}

// knownCollations are the collations implemented: BINARY (memcmp), NOCASE (ASCII
// case fold only) and RTRIM (ignores trailing spaces, not tabs). Custom
// collations cannot be registered, so any other name is "no such collation
// sequence", as in C without one registered.
var knownCollations = map[string]bool{
	"BINARY": true, "NOCASE": true, "RTRIM": true,
}

// parseCollate implements postfix "expr COLLATE name", which binds tighter than
// concatenation and arithmetic but looser than unary operators: "-x COLLATE
// NOCASE" is "(-x) COLLATE NOCASE". Chained clauses are accepted.
func (p *parser) parseCollate() (Expr, error) {
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peekIsKeyword("COLLATE") {
		p.next()
		nameTok := p.peek()
		if nameTok.kind != tkIdent {
			return nil, fmt.Errorf("engine: expected collation name after COLLATE")
		}
		p.next()
		name := nameTok.text
		if !knownCollations[asciiFold(name, false)] {
			// ...except on the ONE retry that proves the name can never be
			// consumed -- see collate_inert.go, which is the only setter of
			// this flag and returns the error below unchanged for every shape
			// it cannot prove.
			if p.allowUnknownCollation {
				p.unknownCollations++
				x = CollateExpr{X: x, Name: name}
				continue
			}
			// Stricter than C, which resolves a collation name only where it is consumed:
			//
			//	SELECT 'a' COLLATE BOGUS                     ok
			//	SELECT 1 COLLATE BOGUS                       ok
			//	SELECT a COLLATE BOGUS FROM t                ok
			//	SELECT ('a' = 'a') COLLATE BOGUS             ok
			//	SELECT a FROM t UNION
			//	  SELECT b COLLATE BOGUS FROM t              ok
			//	CREATE VIEW v AS SELECT a FROM t
			//	  ORDER BY 1 COLLATE BOGUS                   ok -- and then
			//	SELECT * FROM v                              ERROR
			//	SELECT * FROM t WHERE a COLLATE BOGUS = 'x'  ERROR
			//	SELECT * FROM t ORDER BY a COLLATE BOGUS     ERROR
			//	SELECT * FROM t GROUP BY a COLLATE BOGUS     ERROR
			//	SELECT DISTINCT a COLLATE BOGUS FROM t       ERROR
			//	SELECT max(a COLLATE BOGUS) FROM t           ERROR
			//	CREATE INDEX i ON t(a COLLATE BOGUS)         ERROR
			//
			// Erroring eagerly can only decline a statement C would run; going lazy means
			// checking at every site that consumes a collation, and missing one would
			// accept a statement C rejects. (allowUnknownCollation covers the inert and
			// view cases.) The name is reported as written.
			return nil, fmt.Errorf("engine: no such collation sequence: %s", name)
		}
		x = CollateExpr{X: x, Name: name}
	}
	return x, nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits only --
// used to confirm a tkNumber token is a plain integer literal (as opposed to
// one with a '.' fraction or exponent, which is a REAL-valued token that
// must never be folded into parseUnary's MinInt64 special case above).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (p *parser) parseUnary() (Expr, error) {
	// A prefix NOT is legal wherever an operand may start, parsed at
	// parseNotLevel's tier: "2 * NOT 0 = 1" is 2, "2 * NOT 0 AND 0" is 0, "-NOT 0" is
	// -1.
	if p.peekIsKeyword("NOT") {
		return p.parseNotLevel()
	}
	if p.peekIsPunct("-") || p.peekIsPunct("+") || p.peekIsPunct("~") {
		op := p.next().text
		// "-9223372036854775808" folds to the integer MinInt64 at parse time, as in C;
		// without it the positive literal overflows to REAL and loses precision. Only
		// "-" immediately followed by a digit-only literal of value MaxInt64+1 (leading
		// zeros allowed) qualifies; "- (9223372036854775808)" is ordinary negation.
		if op == "-" {
			if t := p.peek(); t.kind == tkNumber && isAllDigits(t.text) {
				if mag, err := strconv.ParseUint(t.text, 10, 64); err == nil && mag == uint64(math.MaxInt64)+1 {
					p.next()
					return LiteralExpr{Val: Value{Typ: Int, I: math.MinInt64}}, nil
				}
			}
		}
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return UnaryExpr{Op: op, X: x}, nil
	}
	return p.parsePrimary()
}

// castAffinityName is the canonical spelling CastExpr.Type carries for each of
// SQLite's five affinities. The evaluator switches on these, so every accepted
// spelling of a CAST target is folded to one of them at parse time.
var castAffinityName = [...]string{
	affNone:    "BLOB",
	affText:    "TEXT",
	affNumeric: "NUMERIC",
	affInteger: "INTEGER",
	affReal:    "REAL",
}

// parseCastTypeName parses a CAST target type and returns its affinity's
// canonical name. The target is a column-style typename resolved with
// sqlite3AffinityType, so every name resolves ("CAST(x AS INT)",
// "AS varchar(10)", "AS \"unsigned big int\""; an unknown name is NUMERIC).
//
//   - An empty typename is legal and NUMERIC (not BLOB, which is a typeless
//     column's affinity), so typeAffinity("") is not used.
//   - A string literal is a legal typename ('text' is TEXT).
//   - A bare non-identifier keyword is a syntax error (nonIdentifierKeywords).
//   - At most two size arguments.
func (p *parser) parseCastTypeName() (string, error) {
	var name strings.Builder
	for {
		t := p.peek()
		switch {
		case t.kind == tkIdent:
			if !t.quoted && nonIdentifierKeywords[t.upper()] {
				if name.Len() == 0 {
					return "", fmt.Errorf("engine: %q is not a usable CAST type name (it is a reserved word)", t.text)
				}
				// A keyword ENDS the typename rather than joining it, so
				// "cast(x AS int) + 1" still parses its own way out.
				return castAffinityName[typeAffinity(name.String())], nil
			}
			name.WriteString(p.next().text)
		case t.kind == tkString:
			name.WriteString(p.next().str)
		default:
			// An empty typename is legal and means NUMERIC -- see above.
			if name.Len() == 0 {
				return castAffinityName[affNumeric], nil
			}
			return p.finishCastTypeName(name.String())
		}
		name.WriteByte(' ')
	}
}

// finishCastTypeName consumes the optional "(size)" / "(size,scale)" that may
// follow a CAST typename and resolves the name to its affinity. The size is
// syntax only -- it never affects the affinity -- so it is parsed and dropped,
// exactly as a column declaration's is.
func (p *parser) finishCastTypeName(name string) (string, error) {
	if t := p.peek(); t.kind == tkPunct && t.text == "(" {
		p.next()
		for n := 0; ; n++ {
			if n == 2 {
				return "", fmt.Errorf("engine: a CAST type takes at most 2 size arguments, got more in %q", name)
			}
			if s := p.peek(); s.kind == tkPunct && (s.text == "+" || s.text == "-") {
				p.next()
			}
			if p.peek().kind != tkNumber {
				return "", fmt.Errorf("engine: expected a size in CAST type, got %q", p.tokenDesc(p.peek()))
			}
			p.next()
			if t := p.peek(); t.kind == tkPunct && t.text == "," {
				p.next()
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return "", err
		}
	}
	return castAffinityName[typeAffinity(name)], nil
}

// peekStartsSubquerySelect reports whether the current token begins a SELECT
// statement in subquery position: "(SELECT ...)" or "(WITH ... SELECT ...)".
// WITH is reserved, so there is no ambiguity; C accepts a CTE list in scalar,
// EXISTS, IN and derived-table subqueries.
func (p *parser) peekStartsSubquerySelect() bool {
	return p.peekIsKeyword("SELECT") || p.peekIsKeyword("WITH")
}

// peekStartsInSubquery is peekStartsSubquerySelect plus VALUES, for IN's right
// side only: "1 IN (VALUES(1),(2))" is the subquery form, and
// "(a,b) IN (VALUES(1,2))" is row-value membership.
func (p *parser) peekStartsInSubquery() bool {
	return p.peekStartsSubquerySelect() || p.peekIsKeyword("VALUES")
}

// peekStartsParenSubquery answers the same in ordinary expression position:
// peekStartsSubquerySelect plus an unquoted VALUES, which reads as a scalar
// subquery exactly like "(SELECT ...)":
//
//	SELECT (VALUES(1))                -> 1
//	SELECT (VALUES(1),(2))            -> 1        (first ROW, not a row value)
//	SELECT (VALUES(1,2))              -> error: sub-select returns 2 columns
//	SELECT (VALUES(1,2)) = (1,2)      -> 1        (same as "(SELECT 1,2)=(1,2)")
//
// VALUES is reserved, so bare "values" is never a column, but a quoted one always
// is.
func (p *parser) peekStartsParenSubquery() bool {
	return p.peekStartsSubquerySelect() || (p.peekIsKeyword("VALUES") && !p.peek().quoted)
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch {
	case t.kind == tkNumber:
		p.next()
		v, err := literalNumber(t.text)
		if err != nil {
			if p.inTriggerBody && errors.Is(err, errHexLiteralTooBig) {
				// C never checks a numeric literal's magnitude at parse time (expr.c:910). For an
				// ordinary statement raising here is equivalent, but a CREATE TRIGGER body is
				// never code-generated at CREATE (trigger.c:323), so hold the error on the node
				// and raise it only if the literal is compiled.
				return LiteralExpr{Val: Value{Typ: Int, I: 0}, deferredErr: err.Error()}, nil
			}
			return nil, err
		}
		return LiteralExpr{Val: v}, nil

	case t.kind == tkString:
		// A string followed by "." is SQLite's "nm DOT nm" column reference,
		// not a literal: "SELECT '@abc'.'!pqr' FROM '@abc'" reads the column
		// (verified directly), while the same string ALONE stays a literal --
		// "SELECT '!pqr' FROM '@abc'" returns the text. See objectNameToken.
		// The lookahead is unambiguous: no expression can follow a string
		// literal with a ".", so there is nothing else this could be.
		if p.peekAt(1).kind == tkPunct && p.peekAt(1).text == "." {
			return p.parseQualifiedNameRef()
		}
		p.next()
		return LiteralExpr{Val: Value{Typ: Text, S: []byte(t.str)}}, nil

	case t.kind == tkBlob:
		p.next()
		return LiteralExpr{Val: Value{Typ: Blob, S: t.blob}}, nil

	case t.kind == tkParam:
		p.next()
		return p.makeParamExpr(t.text)

	case t.kind == tkPunct && t.text == "(":
		p.next()
		if p.peekStartsParenSubquery() {
			sub, err := p.parseSelectStmt()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return SubqueryExpr{Stmt: sub}, nil
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		// A comma makes this a parenthesized row value. Supported row-value comparisons
		// are desugared by the parser; a RowExpr anywhere else is declined downstream as
		// "row value misused".
		if p.peekIsPunct(",") {
			elems := []Expr{e}
			for p.peekIsPunct(",") {
				p.next() // consume ","
				el, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				elems = append(elems, el)
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return RowExpr{Elems: elems}, nil
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return e, nil

	case t.kind == tkIdent && t.upper() == "NULL":
		p.next()
		return LiteralExpr{Val: Value{Typ: Null}}, nil

	case t.kind == tkIdent && t.upper() == "CASE":
		p.next()
		var ce CaseExpr
		if !p.peekIsKeyword("WHEN") {
			base, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ce.Base = base
		}
		for p.consumeKeyword("WHEN") {
			cond, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if !p.consumeKeyword("THEN") {
				return nil, fmt.Errorf("engine: expected THEN in CASE, got %q", p.tokenDesc(p.peek()))
			}
			res, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ce.Whens = append(ce.Whens, WhenClause{When: cond, Then: res})
		}
		if len(ce.Whens) == 0 {
			// Named with the offending token so incompleteIfAtEOF can give a CASE
			// that ran out of input C's own "incomplete input" (parse.y:44-51).
			return nil, fmt.Errorf("engine: CASE requires at least one WHEN clause, got %q", p.tokenDesc(p.peek()))
		}
		if p.consumeKeyword("ELSE") {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			ce.Else = e
		}
		if !p.consumeKeyword("END") {
			return nil, fmt.Errorf("engine: expected END to close CASE, got %q", p.tokenDesc(p.peek()))
		}
		if rewritten, ok := desugarRowCase(ce); ok {
			return rewritten, nil
		}
		return ce, nil

	case t.kind == tkIdent && t.upper() == "EXISTS":
		p.next()
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		if !p.peekStartsParenSubquery() {
			return nil, fmt.Errorf("engine: expected SELECT after EXISTS(, got %q", p.tokenDesc(p.peek()))
		}
		sub, err := p.parseSelectStmt()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return ExistsExpr{Stmt: sub}, nil

	case t.kind == tkIdent && t.upper() == "CAST":
		p.next()
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if !p.consumeKeyword("AS") {
			return nil, fmt.Errorf("engine: expected AS in CAST, got %q", p.tokenDesc(p.peek()))
		}
		typ, terr := p.parseCastTypeName()
		if terr != nil {
			return nil, terr
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return CastExpr{X: x, Type: typ}, nil

	case t.kind == tkIdent && t.upper() == "RAISE" && !t.quoted && p.peekAt(1).kind == tkPunct && p.peekAt(1).text == "(":
		// RAISE(...) is a dedicated trigger-program expression, not an ordinary
		// function call: C SQLite recognizes it in the grammar and rejects
		// it outside a trigger-program at PREPARE time (verified directly:
		// "SELECT RAISE(IGNORE)" errors even though it would run). A bare
		// identifier "raise" NOT followed by "(" is still an ordinary column
		// reference (a column literally named "raise" is legal), so this case
		// only triggers on "raise (".
		return p.parseRaiseExpr()

	case t.kind == tkIdent:
		p.next()
		if p.peekIsPunct("(") {
			p.next()
			var args []Expr
			star := false
			distinct := false
			// DISTINCT as the first argument token is parsed for any function, as in C's
			// grammar. "count(DISTINCT *)" is a syntax error, which falls out because "*" is
			// special-cased only without DISTINCT.
			if p.peekIsKeyword("DISTINCT") {
				p.next()
				distinct = true
			} else if p.peekIsKeyword("ALL") {
				// A leading ALL is a no-op quantifier (count(ALL x) is count(x)), never a
				// column named "all" unless quoted.
				p.next()
			}
			if !distinct && p.peekIsPunct("*") {
				// A lone "*" as the entire argument list is the special
				// count(*) syntax (checked for the "count" name specifically
				// at plan time in sql_agg.go; any other spelling parses here
				// too but is rejected there). Anything else starting with
				// "*" -- i.e. "*" isn't immediately followed by ")" -- falls
				// through to ordinary expression parsing, where a leading
				// "*" is simply invalid (there's no left operand for it).
				save := p.pos
				p.next()
				if p.peekIsPunct(")") {
					star = true
				} else {
					p.pos = save
				}
			}
			if !star && !p.peekIsPunct(")") && !p.peekIsKeyword("ORDER") {
				for {
					a, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peekIsPunct(",") {
						p.next()
						continue
					}
					break
				}
			}
			// An aggregate's own ORDER BY (parse.y:1238, "idj LP distinct
			// exprlist ORDER BY sortlist RP"). Not after a "*": that is a
			// separate production, "idj LP STAR RP", so "count(* ORDER BY x)"
			// is C's syntax error. The terms are attached below, once FILTER
			// and OVER are known, which is the order C attaches them in.
			var orderBy []OrderTerm
			if !star && p.peekIsKeyword("ORDER") {
				p.next()
				if !p.consumeKeyword("BY") {
					return nil, fmt.Errorf("engine: expected BY after ORDER")
				}
				terms, err := p.parseSortList()
				if err != nil {
					return nil, err
				}
				orderBy = terms
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			name := r33sFoldIdent(t.text)
			// likelihood()'s probability argument is checked when the call is coded
			// (compileFunc), as in C: a view body is stored without being prepared, so
			// "CREATE VIEW v AS SELECT likelihood(1, 9)" succeeds and only its use fails. A
			// parser flag cannot express that, because the view is re-parsed through this
			// same production on use. NameAsWritten keeps the spelling the message echoes.
			fe := FuncExpr{Name: name, NameAsWritten: t.text, Args: args, Star: star, Distinct: distinct}
			// FILTER (WHERE <cond>) and OVER <window> modifiers (SQLite's
			// aggregate/window-function grammar). FILTER may only precede OVER;
			// it is parsed so it can be declined cleanly (sql_window.go), never
			// silently dropped.
			if p.peekIsKeyword("FILTER") {
				p.next()
				if err := p.expectPunct("("); err != nil {
					return nil, err
				}
				if !p.consumeKeyword("WHERE") {
					return nil, fmt.Errorf("engine: expected WHERE in FILTER clause")
				}
				fcond, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
				fe.Filter = fcond
			}
			if p.peekOpensOverClause() {
				p.next()
				spec, err := p.parseWindowSpec()
				if err != nil {
					return nil, err
				}
				fe.Over = spec
			}
			// sqlite3ExprAddFunctionOrderBy (expr.c:1219), run after
			// sqlite3WindowAttach exactly as parse.y:1310-1313 orders them:
			// a zero-argument call drops the clause without resolving it
			// (:1238-1242 -- "count(ORDER BY nosuch)" is 1 in 3.53.3), a
			// windowed call refuses it (:1243-1246, the message is
			// sqlite3ExprOrderByAggregateError's, :1203-1206), and so does an
			// over-long list (:1248-1252). A FILTER alone is no window
			// (IsWindowFunc, sqliteInt.h:3212).
			if len(orderBy) > 0 && len(args) > 0 {
				if fe.Over != nil {
					return nil, semanticf("engine: ORDER BY may not be used with non-aggregate %s()", t.text)
				}
				if len(orderBy) > 2000 {
					return nil, semanticf("engine: too many terms in ORDER BY clause")
				}
				fe.OrderBy = orderBy
			}
			if (name == "iif" || name == "if") && !star && !distinct && len(fe.OrderBy) == 0 &&
				fe.Filter == nil && fe.Over == nil && len(args) >= 2 {
				return rewriteIifToCase(args), nil
			}
			return fe, nil
		}
		// CURRENT_DATE / CURRENT_TIME / CURRENT_TIMESTAMP are keywords in expression
		// position (CTIME_KW is not in %fallback), so a column of that name never
		// shadows them, unlike TRUE/FALSE:
		//
		//   SELECT current_date FROM q    -> '2024-01-01'  (the KEYWORD wins)
		//   SELECT q.current_date FROM q  -> 'cd-col'      (qualified: column)
		//   SELECT "current_date" FROM q  -> 'cd-col'      (quoted: column)
		//
		// Each lowers to its zero-argument function ("now"). The result column name
		// still comes from the source text.
		if !t.quoted {
			switch t.upper() {
			case "CURRENT_DATE":
				return FuncExpr{Name: "date", CurrentTimeKw: true}, nil
			case "CURRENT_TIME":
				return FuncExpr{Name: "time", CurrentTimeKw: true}, nil
			case "CURRENT_TIMESTAMP":
				return FuncExpr{Name: "datetime", CurrentTimeKw: true}, nil
			}
		}
		name := t.text
		if p.peekIsPunct(".") {
			// table.column: the qualifier is kept and matched against a
			// table alias/name in evalCtx's scope chain at eval time (see
			// resolveColumn), which is what lets a
			// correlated subquery distinguish its own (possibly aliased)
			// table from the enclosing query's. A qualified "t.true"/"t.false"
			// is NEVER eligible for the TRUE/FALSE literal fallback below --
			// verified directly against mattn/go-sqlite3 -- so no
			// FallbackLiteral is set here at all.
			p.next()
			col := p.next()
			colName, ok := objectNameToken(col)
			if !ok {
				return nil, fmt.Errorf("engine: expected column name after '.', got %q", p.tokenDesc(col))
			}
			// A THREE-part reference "schema.table.column" (e.g. "main.t.c"):
			// the first identifier is the database qualifier, the middle the
			// table alias/name, the last the column. validateColumnRefs
			// (query.go) validates the Schema against this pager's own schema
			// and otherwise treats the reference as the plain two-part
			// "Qualifier.Name" (see ColumnExpr.Schema's doc comment).
			if p.peekIsPunct(".") {
				p.next() // the second "."
				col3 := p.next()
				col3Name, ok := objectNameToken(col3)
				if !ok {
					return nil, fmt.Errorf("engine: expected column name after '.', got %q", p.tokenDesc(col3))
				}
				return ColumnExpr{Schema: name, Qualifier: colName, Name: col3Name}, nil
			}
			return ColumnExpr{Qualifier: name, Name: colName}, nil
		}
		return ColumnExpr{Name: name, FallbackLiteral: identLiteralFallback(t), R32NDQSpan: p.r32nDQSpan(t)}, nil

	default:
		if t.kind == tkEOF {
			// C SQLite distinguishes running OUT of input from meeting the
			// wrong token: "SELECT * FROM t WHERE" is "incomplete input", not
			// a complaint about a token that is not there. Verified against
			// 3.53.3.
			return nil, fmt.Errorf("engine: incomplete input")
		}
		// parse.y:44-51's other half: a syntax error that is NOT at end of
		// input is `near "X": syntax error`, naming the offending token and
		// nothing else. parserSyntaxError is the whole of C's wording here.
		return nil, fmt.Errorf("engine: near %q: syntax error", p.tokenDesc(t))
	}
}

// rewriteIifToCase expands iif()/if() into a searched CASE from the argument
// list verbatim (expr.c:4608, INLINEFUNC_iif): "iif(a,b,c,d,e)" is "CASE WHEN a
// THEN b WHEN c THEN d ELSE e END", a trailing unpaired argument being ELSE. if
// is an alias (func.c:3442); both need at least two arguments.
func rewriteIifToCase(args []Expr) CaseExpr {
	ce := CaseExpr{}
	i := 0
	for ; i+1 < len(args); i += 2 {
		ce.Whens = append(ce.Whens, WhenClause{When: args[i], Then: args[i+1]})
	}
	if i < len(args) {
		ce.Else = args[i]
	}
	return ce
}

// parseQualifiedNameRef parses a dotted column reference whose first
// component sits at p's current position and is spelled as a STRING literal --
// "'tbl'.'col'" or "'db'.'tbl'.'col'". The identifier-first spellings are
// handled inline in parsePrimary's tkIdent case, which shares this one's
// objectNameToken rule for the components after each ".". Split out only
// because a leading string has to be distinguished from a plain literal by
// lookahead first; the resulting ColumnExpr is identical either way.
func (p *parser) parseQualifiedNameRef() (Expr, error) {
	first := p.next()
	name, ok := objectNameToken(first)
	if !ok {
		return nil, fmt.Errorf("engine: expected a table or column name, got %q", p.tokenDesc(first))
	}
	if err := p.expectPunct("."); err != nil {
		return nil, err
	}
	second := p.next()
	secondName, ok := objectNameToken(second)
	if !ok {
		return nil, fmt.Errorf("engine: expected column name after '.', got %q", p.tokenDesc(second))
	}
	if p.peekIsPunct(".") {
		p.next()
		third := p.next()
		thirdName, ok := objectNameToken(third)
		if !ok {
			return nil, fmt.Errorf("engine: expected column name after '.', got %q", p.tokenDesc(third))
		}
		return ColumnExpr{Schema: name, Qualifier: secondName, Name: thirdName}, nil
	}
	return ColumnExpr{Qualifier: name, Name: secondName}, nil
}

// parseRaiseExpr parses RAISE(...), positioned at RAISE:
//
//	RAISE ( IGNORE )
//	RAISE ( ROLLBACK|ABORT|FAIL , message-expr )
//
// Outside a trigger body it is rejected with C's prepare-time wording. Wrong
// arity is rejected too, though not with C's exact wording.
func (p *parser) parseRaiseExpr() (Expr, error) {
	p.next() // RAISE
	if !p.inTriggerBody {
		return nil, fmt.Errorf("engine: RAISE() may only be used within a trigger-program")
	}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	if p.consumeKeyword("IGNORE") {
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		if p.raiseIgnoreDeclined {
			p.sawUnsafeRaiseIgnore = true
		}
		return RaiseExpr{Ignore: true}, nil
	}
	var action conflictAction
	switch {
	case p.consumeKeyword("ROLLBACK"):
		action = conflictRollback
	case p.consumeKeyword("ABORT"):
		action = conflictAbort
	case p.consumeKeyword("FAIL"):
		action = conflictFail
	default:
		return nil, fmt.Errorf("engine: RAISE: expected IGNORE, ROLLBACK, ABORT, or FAIL, got %q", p.tokenDesc(p.peek()))
	}
	if err := p.expectPunct(","); err != nil {
		return nil, err
	}
	msg, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return RaiseExpr{Action: action, Msg: msg}, nil
}

// identLiteralFallback returns ColumnExpr.FallbackLiteral for a bare identifier
// in unqualified-reference position. Two mutually exclusive rules: a
// double-quoted identifier that resolves to no column is the string literal of
// its text (C's double-quoted-string misfeature), and an unquoted TRUE/FALSE is
// 1/0 (boolKeywordFallback).
//
// Kept as a code block (not prose) because gofmt rewrites two adjacent single
// quotes in doc prose into a curly quote:
//
//	SELECT "nosuch"        -> 'nosuch'
//	SELECT '' <= ""        -> 1
//	SELECT """"""""        -> '"""'   (escape-decoded, then a literal)
//	SELECT "a" FROM t      -> the COLUMN a (a real column always wins)
//	SELECT t."nosuch"      -> no such column: t.nosuch  (qualified: never)
//	SELECT [nosuch]        -> no such column: nosuch    (wrong quote)
//	SELECT `nosuch`        -> no such column: nosuch    (wrong quote)
//
// Both apply only on an exact unqualified no-such-column miss
// (unqualifiedColumnNotFoundErr).
func identLiteralFallback(t token) *Value {
	if t.dquoted {
		v := Value{Typ: Text, S: []byte(t.text)}
		return &v
	}
	return boolKeywordFallback(t)
}

// boolKeywordFallback returns ColumnExpr.FallbackLiteral's value for a bare
// identifier token t reached in unqualified-reference position: non-nil
// (pointing at the integer 1 or 0) only when t is an UNQUOTED spelling of
// "TRUE" or "FALSE" (case-insensitive) -- see FallbackLiteral's doc comment
// (sql_ast.go) for the full disambiguation this implements and why it can't
// simply be resolved to a literal here at parse time.
func boolKeywordFallback(t token) *Value {
	if t.quoted {
		return nil
	}
	switch t.upper() {
	case "TRUE":
		return &Value{Typ: Int, I: 1}
	case "FALSE":
		return &Value{Typ: Int, I: 0}
	default:
		return nil
	}
}

// isBareBoolKeyword reports whether t is an unquoted TRUE or FALSE, for the
// "X IS [NOT] TRUE/FALSE" truth test. Quoted spellings are ordinary identifiers
// ("2 IS [true]" is a column comparison).
func isBareBoolKeyword(t token) bool {
	return t.kind == tkIdent && !t.quoted && (t.upper() == "TRUE" || t.upper() == "FALSE")
}

// desugarIsBool builds "X IS [NOT] TRUE/FALSE" from existing nodes:
//
//	X IS TRUE  = CASE WHEN X THEN 1 ELSE 0 END
//	X IS FALSE = CASE WHEN (NOT X) THEN 1 ELSE 0 END
//
// A NULL X does not match WHEN in either, giving 0; NOT X separates NULL from
// falsy. Both are always 0 or 1, so IS NOT is a plain NOT around them.
//
// The keyword node (kw, a ColumnExpr with a FallbackLiteral) stands in the arm
// whose value it is (THEN for TRUE, ELSE for FALSE). C resolves the right
// operand first and makes the node TK_TRUTH only if it stayed TK_TRUEFALSE
// (resolve.c:1427-1431, 747), so with a column named "true", "a IS true"
// compares against that column; compileCase uses the resolved operand to pick.
func desugarIsBool(x Expr, kw ColumnExpr, want, not bool) Expr {
	result := CaseExpr{
		Whens:  []WhenClause{{When: x, Then: kw}},
		Else:   LiteralExpr{Val: Value{Typ: Int, I: 0}},
		isBool: true,
	}
	if !want {
		result.Whens[0] = WhenClause{When: UnaryExpr{Op: "NOT", X: x}, Then: LiteralExpr{Val: Value{Typ: Int, I: 1}}}
		result.Else = kw
		result.isBoolFalse = true
	}
	if not {
		return UnaryExpr{Op: "NOT", X: result}
	}
	return result
}

// isBoolParts takes an isBool CaseExpr back apart into the "X IS kw" it was
// built from: the tested operand and the arm holding the keyword. ok is false
// for any other CASE, and for an isBool one some rewrite has reshaped past
// recognition.
func isBoolParts(x CaseExpr) (operand, kw Expr, ok bool) {
	if !x.isBool || x.Base != nil || len(x.Whens) != 1 {
		return nil, nil, false
	}
	if !x.isBoolFalse {
		return x.Whens[0].When, x.Whens[0].Then, true
	}
	u, isNot := x.Whens[0].When.(UnaryExpr)
	if !isNot || !strings.EqualFold(u.Op, "NOT") || x.Else == nil {
		return nil, nil, false
	}
	return u.X, x.Else, true
}

// boolKeywordOperand reports whether r, the right operand of an ordinary
// "X IS [NOT] r", is the bare TRUE/FALSE keyword under nothing but COLLATE
// clauses -- the operand resolveExprStep tests through
// sqlite3ExprSkipCollateAndLikely (resolve.c:1422). Parentheses need no
// stripping: parsePrimary returns the inner node, as parse.y:1176 does. A
// double-quoted "true" carries a TEXT fallback and a bracketed one none, and
// both stay ordinary comparisons (sqlite3ExprIdToTrueFalse refuses EP_Quoted,
// expr.c:2337).
func boolKeywordOperand(r Expr) (ColumnExpr, bool) {
	for {
		c, ok := r.(CollateExpr)
		if !ok {
			break
		}
		r = c.X
	}
	ce, ok := r.(ColumnExpr)
	if !ok || ce.Qualifier != "" || ce.FallbackLiteral == nil || ce.FallbackLiteral.Typ != Int {
		return ColumnExpr{}, false
	}
	return ce, true
}

// defaultClauseIsBool builds "X IS [NOT] TRUE/FALSE" for a column DEFAULT
// expression. There C compares with NULL-safe equality against 1 or 0 rather
// than testing truthiness: the TK_IS -> TK_TRUTH rewrite happens only in
// resolveExprStep (resolve.c:1419), and a DEFAULT is never resolved
// (sqlite3AddDefaultValue, build.c:1729), so it codegens as an ordinary
// TK_IS/TK_ISNOT comparison with SQLITE_NULLEQ (expr.c:5180):
//
//	DEFAULT (5 IS TRUE)             -> 0   (5 != 1, literal-eq; truthy(5) would be 1)
//	DEFAULT (5 IS NOT TRUE)         -> 1   (5 != 1; truthy(5) NOT TRUE would be 0)
//	DEFAULT ('00:00:00' IS FALSE)   -> 0   (TEXT never numeric-equals 0)
//	DEFAULT ('00:00:00' IS NOT FALSE) -> 1
//	DEFAULT (NULL IS TRUE)          -> 0   (NULLEQ: NULL != 1)
//	DEFAULT (NULL IS NOT FALSE)     -> 1   (NULLEQ: NULL != 0)
//
// The plain BinaryExpr "IS"/"IS NOT" is already that comparison, so only the
// tree differs from desugarIsBool's.
func defaultClauseIsBool(x Expr, want, not bool) Expr {
	rhs := int64(0)
	if want {
		rhs = 1
	}
	op := "IS"
	if not {
		op = "IS NOT"
	}
	return BinaryExpr{Op: op, L: x, R: LiteralExpr{Val: Value{Typ: Int, I: rhs}}}
}

// errHexLiteralTooBig is literalNumber's sentinel for a hex literal whose
// magnitude exceeds 64 bits ("hex literal too big: <text>"). parsePrimary
// checks for exactly this error (via errors.Is) to decide whether to defer it
// rather than raise it immediately -- see LiteralExpr.deferredErr's doc
// comment (sql_ast.go) for why only this specific literal-magnitude error
// gets that treatment.
var errHexLiteralTooBig = errors.New("hex literal too big")

// literalNumber parses a lexed number token into a Value, choosing Int or
// Float exactly as SQLite does: integer syntax that overflows int64 falls
// back to a float, matching SQLite's own literal handling.
func literalNumber(text string) (Value, error) {
	if len(text) > 2 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
		// Hex literal: the digits are a 64-bit unsigned pattern read as signed
		// (0xffffffffffffffff is -1). More than 64 bits is "hex literal too big",
		// with no REAL fallback.
		u, err := strconv.ParseUint(text[2:], 16, 64)
		if err != nil {
			return Value{}, fmt.Errorf("engine: %w: %s", errHexLiteralTooBig, text)
		}
		return Value{Typ: Int, I: int64(u)}, nil
	}
	if !strings.ContainsAny(text, ".eE") {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return Value{Typ: Int, I: n}, nil
		}
	}
	// codeReal (expr.c:4312) reads a REAL literal with sqlite3AtoF, which keeps
	// about 19 significant digits (see parseFloatSaturating) and turns an
	// exponent out of range into +/-Inf or 0 rather than an error -- "SELECT
	// 1e500" is Inf with typeof 'real'. Text that did not lex as a whole
	// number is still invalid.
	f, rc := sqliteAtoF([]byte(text))
	if rc <= 0 {
		return Value{}, fmt.Errorf("engine: invalid numeric literal %q", text)
	}
	return Value{Typ: Float, F: f}, nil
}

// ---- CREATE TABLE column-info parsing ----

// affinity is one of SQLite's five type affinities, per
// https://www.sqlite.org/datatype3.html#type_affinity
type affinity int

const (
	affNone affinity = iota // BLOB affinity, or "no affinity" for a computed expression
	affText
	affNumeric
	affInteger
	affReal
)

// typeAffinity implements SQLite's column-affinity determination rule
// (the five-step algorithm at the link above), applied to a column's
// declared type name.
func typeAffinity(declType string) affinity {
	t := strings.ToUpper(declType)
	switch {
	case strings.Contains(t, "INT"):
		return affInteger
	case strings.Contains(t, "CHAR") || strings.Contains(t, "CLOB") || strings.Contains(t, "TEXT"):
		return affText
	case strings.Contains(t, "BLOB") || t == "":
		return affNone
	case strings.Contains(t, "REAL") || strings.Contains(t, "FLOA") || strings.Contains(t, "DOUB"):
		return affReal
	default:
		return affNumeric
	}
}

// ---- STRICT tables (https://www.sqlite.org/stricttables.html) ----

// strictColType is a STRICT table column's datatype. C accepts exactly INT,
// INTEGER, REAL, TEXT, BLOB and ANY (case-insensitive, after dequoting), with no
// width; anything else is `unknown datatype for <tbl>.<col>: "<type>"` and no
// type is `missing datatype for <tbl>.<col>`. strictColNone is the zero value.
type strictColType uint8

const (
	strictColNone strictColType = iota
	strictColAny
	strictColBlob
	strictColInt
	strictColInteger
	strictColReal
	strictColText
)

// strictColTypeNames is each strictColType's CANONICAL, upper-case spelling
// -- SQLite's own sqlite3StdType[] table, and what its runtime type error
// names the column's type as, INDEPENDENT of how the CREATE TABLE text
// spelled it: a column declared "int" reports `cannot store REAL value in
// INT column t.b` while one declared "INTEGER" reports `... in INTEGER
// column t.a` (verified directly -- INT and INTEGER are distinct entries,
// even though they share INTEGER affinity).
var strictColTypeNames = [...]string{"", "ANY", "BLOB", "INT", "INTEGER", "REAL", "TEXT"}

func (t strictColType) String() string { return strictColTypeNames[t] }

// strictColTypeOf maps a column's declared type name to its strictColType,
// reporting false for anything a STRICT table may not declare (including
// the empty string, i.e. a column with no declared type at all -- which is
// its own distinct SQLite error, see strictColType's doc comment).
func strictColTypeOf(declType string) (strictColType, bool) {
	for t := strictColAny; t <= strictColText; t++ {
		if equalFoldName(declType, strictColTypeNames[t]) {
			return t, true
		}
	}
	return strictColNone, false
}

// strictColAffinity is the affinity of a STRICT column of type t. It differs from
// typeAffinity only for ANY, which has no affinity in a STRICT table (TEXT '123'
// stays text) but is NUMERIC in an ordinary one.
func strictColAffinity(t strictColType) affinity {
	if t == strictColAny {
		return affNone
	}
	return typeAffinity(strictColTypeNames[t])
}

// columnInfo describes one column of a table, as recovered from its CREATE
// TABLE SQL text.
type columnInfo struct {
	Name     string
	DeclType string
	Aff      affinity
	// Hidden marks a virtual-table HIDDEN column (vtab.go): one that is a real,
	// name-resolvable column (usable in WHERE and in an explicit select-list
	// reference, and carried in every materialized row) but omitted from a
	// bare "*" expansion (expandSelectList, query.go) -- exactly SQLite's own
	// hidden-column semantics for table-valued functions such as
	// generate_series (whose start/stop/step are HIDDEN input columns). Always
	// false for an ordinary base-table column.
	Hidden bool
	// Unindexed carries VtabColumn.Unindexed (vtab.go) through to the MATCH
	// evaluator: an fts5 column declared "<name> UNINDEXED" is stored and
	// selectable like any other, but contributes no tokens, so buildFts5Doc
	// (fts5_match.go) must not tokenize it. Always false otherwise.
	Unindexed    bool
	IsRowidAlias bool // INTEGER PRIMARY KEY: this column aliases the rowid
	NotNull      bool // inline "NOT NULL" column constraint; enforced by Insert/Update
	// RowidConflict is the declared ON CONFLICT action of this column's own PRIMARY
	// KEY when it is the rowid alias (IsRowidAlias), conflictAbort if none. The
	// rowid alias has no index to hold it (unlike indexMeta.onConflict), so it
	// lives here; conflict.go's declaredHitAction uses it for rowid collisions when
	// the statement has no OR clause (insert4.test 8.x).
	RowidConflict conflictAction
	// NotNullConflict is the declared ON CONFLICT action of this column's NOT NULL
	// constraint (conflictAbort if none), the default for a NOT NULL violation when
	// the statement has no OR clause (conflict.test 5.x). REPLACE substitutes the
	// column's DEFAULT, or acts as ABORT when there is none.
	NotNullConflict conflictAction
	HasDefault      bool // this column declared an inline "DEFAULT ..." constraint
	// DefaultValue is the DEFAULT clause's constant-folded value, stored by an
	// INSERT that omits the column before affinity is applied ("t TEXT DEFAULT 5"
	// stores '5'). DefaultKnown says the fold succeeded; with HasDefault but not
	// DefaultKnown, an INSERT omitting the column declines (see
	// parseColumnDefault). A DEFAULT on the rowid alias is ignored by C, so readers
	// skip tbl.ipkIndex.
	DefaultValue Value
	DefaultKnown bool
	// DefaultDeferred is the DEFAULT expression, kept when the fold failed only
	// because it reads the clock; C evaluates DEFAULTs per INSERT, so every insert
	// path evaluates it fresh. nil for random()/randomblob() (still declined) and
	// for anything the codegen cannot lower.
	DefaultDeferred Expr
	// defaultProg is DefaultDeferred compiled (compileDeferredDefaults), so
	// applyColumnDefaults runs opcodes per row. nil when there is no deferred
	// default; a selfRowExpr with a nil Program when it did not compile, which eval
	// retries. Compiled against an empty scope (a DEFAULT references no column),
	// which is the same for every table, so it never needs re-stamping after ALTER.
	defaultProg *selfRowExpr
	// NoAffinity marks a DERIVED table's (or view's) output column whose
	// defining expression is COMPUTED -- SQLite's AFF_NONE, as opposed to the
	// AFF_BLOB a real typeless COLUMN carries. The two are indistinguishable
	// in Aff (both affNone here) but behave differently in a comparison: a
	// BLOB-affinity column defends its storage class against a TEXT-affinity
	// operand, while a computed one does not. See isMaterializedRef
	// for the verified evidence. Always false for a real table
	// column; set only by derivedColumnInfos (join.go).
	NoAffinity bool

	// NoCollation marks a reference with no collating sequence at all, which differs
	// from a declared BINARY only in a fall-through: sqlite3BinaryCompareCollSeq
	// consults the right operand only when the left has none:
	//
	//	expr.c:436   pColl = sqlite3ExprCollSeq(pParse, pLeft);
	//	expr.c:437   if( !pColl ){
	//	expr.c:438     pColl = sqlite3ExprCollSeq(pParse, pRight);
	//	expr.c:439   }
	//
	// and a BINARY-declared column resolves to db->pDfltColl (callback.c:171). An
	// upsert's "excluded.<col>" has none (resolve.c:582, expr.c:254-302). Set only
	// by excludedPseudoRowCols, beside NoAffinity; NEW/OLD is the inverse (see
	// trigPseudoRowCols).
	NoCollation bool

	Collation string // this column's DECLARED collating sequence -- "BINARY" (the
	// default, when no column-level COLLATE clause was given), "NOCASE", or "RTRIM";
	// other names are rejected at CREATE/ALTER. It is the default collation for a
	// bare reference to the column in a comparison, ORDER BY, GROUP BY or DISTINCT
	// without an explicit COLLATE (topExprCollation).

	// GeneratedExpr is the source text of a "[GENERATED ALWAYS] AS (<expr>)
	// [STORED|VIRTUAL]" column (empty for an ordinary column; see IsGenerated).
	// GeneratedStored picks the storage kind:
	//
	//   - STORED:  computed when the row is written and PRESENT in the record.
	//   - VIRTUAL: computed on every read and ABSENT from the record entirely.
	//
	// VIRTUAL is the default. Virtual columns are skipped in the record, the rest
	// keep declaration order (storedColumnSlots).
	GeneratedExpr   string
	GeneratedStored bool

	// R32NGeneratedOff is the byte offset of GeneratedExpr's first byte within
	// the CREATE TABLE text it was sliced out of (0 when there is none). Only
	// the renameFixQuotes port reads it (alter_write.go), to re-parse the
	// body with span recording on and get offsets into the WHOLE stored text.
	R32NGeneratedOff int

	// genProg is GeneratedExpr compiled against the row-in-registers block
	// (compileGeneratedColumns), re-stamped by refreshRowPrograms after an in-place
	// ALTER, so computeGeneratedInto runs opcodes per row, as
	// sqlite3ComputeGeneratedColumns does (insert.c:285, 353, 372). nil means not
	// compiled. It addresses columns by position, so it belongs to the column list
	// it was compiled against (selfRowExpr.cols).
	genProg *selfRowExpr

	// genDeps are the indexes of the columns this generated column's
	// expression READS, stamped beside genProg by compileGeneratedColumns.
	// computeGeneratedInto needs them because a generated column may read
	// ANOTHER one, and then the two must be evaluated in dependency order --
	// C does it on demand with COLFLAG_NOTAVAIL (expr.c:5070-5080) rather
	// than in column order. nil for a non-generated column.
	genDeps []int
}

// IsGenerated reports whether c is a generated (computed) column -- see
// GeneratedExpr.
func (c columnInfo) IsGenerated() bool { return c.GeneratedExpr != "" }

// autoIndexSpec is one inline or table-level UNIQUE / non-rowid-alias PRIMARY
// KEY constraint that C enforces with an automatic index
// (sqlite_autoindex_<table>_<N>). cols are column names in constraint order;
// buildAutoIndexes resolves them and numbers N in spec order. kind ("pk"/"u")
// and inline are used only by DROP COLUMN's error wording: an inline UNIQUE gets
// "cannot drop UNIQUE column", a table-level one the generic "error in table
// ... after drop column".
type autoIndexSpec struct {
	cols   []string
	kind   string // "pk" or "u"
	inline bool

	// desc is parallel to cols: true where that column was declared
	// DESC-ordered. It is part of the automatic index's own physical b-tree
	// order -- C SQLite reports desc=1 for "UNIQUE(a DESC)"'s
	// sqlite_autoindex too (verified via PRAGMA index_xinfo) -- see
	// indexMeta.colDesc for what writing it ascending instead costs.
	desc []bool

	// colls is parallel to cols: the CONSTRAINT-LEVEL collating sequence that
	// column named ("UNIQUE(a COLLATE nocase)"), or "" where it named none --
	// in which case the COLUMN's own declared collation governs, exactly as it
	// always did. See buildAutoIndexes (index_write.go), and
	// extractParenIdentListFull for the grammar.
	colls []string

	// anyDesc is true if this PRIMARY KEY declared any column DESC. Per-column DESC
	// (desc) is honoured for WITHOUT ROWID keys and UNIQUE indexes alike;
	// anyDesc itself is consulted only for "PRIMARY KEY(x DESC AUTOINCREMENT)",
	// which collapses into the rowid alias and stays declined.
	anyDesc bool

	// onConflict is the constraint's declared ON CONFLICT action (conflictAbort if
	// none), copied onto the indexMeta buildAutoIndexes builds. Never set for the
	// PRIMARY KEY that becomes the rowid alias (see columnInfo.RowidConflict).
	onConflict conflictAction

	// onConflictSet distinguishes an explicit ON CONFLICT ABORT from none. It matters
	// when two constraints build the same index: C errors "conflicting ON CONFLICT
	// clauses specified" only when both are explicit and differ.
	onConflictSet bool
}

// checkConstraint is one CHECK(...) from a CREATE TABLE. The parser recovers only
// the structural half (raw text and owning column), since the read path must
// tolerate any CHECK body. Parsing exprText into expr and validating it is the
// write path's job (finalizeCheckConstraints); expr is nil when produced here.
type checkConstraint struct {
	// name is the constraint's own explicit name, from a "CONSTRAINT name
	// CHECK(...)" prefix (column- or table-level alike), or "" for an
	// unnamed CHECK. C SQLite's own runtime violation wording is "CHECK
	// constraint failed: <name>" when named, or "CHECK constraint failed:
	// <exprText>" (the verbatim source text below) when not -- verified
	// directly against C SQLite.
	name string
	// exprText is the CHECK's own parenthesized body, sliced verbatim out
	// of the original CREATE TABLE source text (byte-for-byte, including
	// whatever internal whitespace the author wrote) and then
	// strings.TrimSpace'd -- exactly matching C SQLite's own unnamed-
	// CHECK error wording, verified directly: "CHECK ( a  >  0 )" reports
	// "CHECK constraint failed: a  >  0", a TrimSpace of the parenthesized
	// body, NOT a re-rendered/normalized "a > 0".
	exprText string
	// expr is exprText parsed into this package's ordinary expression AST
	// (see sql_ast.go) -- nil until finalizeCheckConstraints (schema_write.go)
	// populates it; evaluated per candidate row by insert_write.go's
	// checkTableChecks.
	expr Expr
	// ownerCol is the column whose own definition declared this CHECK, or "" for a
	// table-level CHECK. DROP COLUMN drops a CHECK owned by the column, but rejects
	// one elsewhere that references it, as C does.
	ownerCol string
	// r32nBodyOff is the byte offset of exprText's first byte within the
	// CREATE TABLE text it was sliced out of. Only the renameFixQuotes port
	// reads it (alter_write.go); see columnInfo.R32NGeneratedOff.
	r32nBodyOff int
	// prog is expr COMPILED against the table's row-in-registers block --
	// SQLite's own shape for a CHECK (sqlite3ExprIfTrue under
	// pParse->iSelfTab = -(regNewData+1), insert.c:2066/2087). Populated by
	// finalizeCheckConstraints alongside expr, re-stamped by refreshRowPrograms
	// (alter_write.go) after an ALTER edits the column list in place, and the
	// only thing checkTableChecksChanged evaluates; see selfRowExpr
	// (vdbe_codegen.go), whose cols field is why the re-stamp is needed.
	prog *selfRowExpr
}

// findMatchingParen returns the index, within toks, of the ")" that closes
// the "(" at toks[openIdx] (which must itself be an open-paren token),
// tracking nested-paren depth -- used to recover a CHECK constraint's own
// parenthesized expression body regardless of what punctuation/nesting it
// contains (e.g. "CHECK(a > (1+2))").
func findMatchingParen(toks []token, openIdx int) (int, error) {
	depth := 0
	for i := openIdx; i < len(toks); i++ {
		if toks[i].kind == tkPunct && toks[i].text == "(" {
			depth++
		} else if toks[i].kind == tkPunct && toks[i].text == ")" {
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return -1, fmt.Errorf("engine: CREATE TABLE: unterminated CHECK expression")
}

// parseCheckClauseStructural extracts a "CHECK ( ... )" clause's raw expression
// text starting at the CHECK token seg[idx], by paren matching only. Returns the
// checkConstraint (name and ownerCol left for the caller) and the index of its
// closing ")".
func parseCheckClauseStructural(createSQL string, seg []token, idx int) (checkConstraint, int, error) {
	if idx+1 >= len(seg) || !(seg[idx+1].kind == tkPunct && seg[idx+1].text == "(") {
		return checkConstraint{}, 0, fmt.Errorf("engine: CREATE TABLE: expected '(' after CHECK")
	}
	openIdx := idx + 1
	closeIdx, err := findMatchingParen(seg, openIdx)
	if err != nil {
		return checkConstraint{}, 0, err
	}
	raw := createSQL[seg[openIdx].End:seg[closeIdx].Start]
	exprText := strings.TrimSpace(raw)
	// Where the trimmed body actually starts, so the renameFixQuotes port can
	// re-parse it and still report offsets into createSQL. raw's leading run is
	// all whitespace and exprText's first byte is not, so the first occurrence
	// of exprText in raw is exactly the trim point.
	off := seg[openIdx].End
	if exprText != "" {
		off += strings.Index(raw, exprText)
	}
	return checkConstraint{exprText: exprText, r32nBodyOff: off}, closeIdx, nil
}

// asCreateTableIdent accepts a single-quoted string where CREATE TABLE expects
// an identifier, as C does ("CREATE TABLE 'a b'(q)"). FTS3/FTS4 write their
// shadow tables this way ("CREATE TABLE 't_content'(docid INTEGER PRIMARY KEY,
// 'c0a', 'c1b')"). Applied only to the table name and a column definition's
// leading name, the positions checked against C.
func asCreateTableIdent(t token) (token, bool) {
	switch t.kind {
	case tkIdent:
		return t, true
	case tkString:
		return token{kind: tkIdent, text: t.str, quoted: true, Start: t.Start, End: t.End}, true
	}
	return t, false
}

// parseCreateTableColumnsAndAutoIndexes parses a CREATE TABLE's columns plus
// every UNIQUE/PRIMARY KEY constraint needing an automatic index
// (autoIndexSpec) and every CHECK's structural shape (checkConstraint). A
// single-INTEGER-column PRIMARY KEY, inline or table-level, becomes the rowid
// alias and gets no spec. forceNoRowidAlias disables that for a WITHOUT ROWID
// table.
//
// STRICT is detected here from the table options (tailDeclaresStrict), so read
// and write paths get identical columns:
//
//   - only INT/INTEGER/REAL/TEXT/BLOB/ANY, and a type is required
//     (strictColTypeOf);
//   - ANY has no affinity (strictColAffinity);
//   - every PRIMARY KEY column is NOT NULL, except the INTEGER PRIMARY KEY
//     rowid alias.
func parseCreateTableColumnsAndAutoIndexes(createSQL string, forceNoRowidAlias bool) ([]columnInfo, []checkConstraint, []autoIndexSpec, error) {
	toks, err := lex(createSQL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("engine: parsing CREATE TABLE: %w", err)
	}
	i := 0
	kw := func(s string) bool {
		if toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: expected CREATE")
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TABLE") {
		return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: expected TABLE")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	nameTok, nameOK := asCreateTableIdent(toks[i])
	if !nameOK {
		return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: expected table name")
	}
	// The table's own name, needed only by STRICT's CREATE-TABLE-time errors
	// ("unknown datatype for <tbl>.<col>", "missing datatype for <tbl>.<col>"),
	// which name the table exactly as C SQLite's do.
	tblName := nameTok.text
	i++
	if toks[i].kind == tkPunct && toks[i].text == "." {
		schemaTok := toks[i-1]
		i++
		if toks[i].kind != tkIdent {
			return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: expected table name after schema qualifier")
		}
		// See parseCreateTableName's (schema_write.go) identical check and
		// doc comment: a non-"main"/"temp" schema qualifier names an
		// ATTACHed database this write path has no concept of.
		if !equalFoldName(schemaTok.text, "main") && !equalFoldName(schemaTok.text, "temp") {
			return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: unknown database %s", schemaTok.text)
		}
		tblName = toks[i].text
		i++
	}
	if !(toks[i].kind == tkPunct && toks[i].text == "(") {
		return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: expected '('")
	}
	i++

	// Split the column/constraint list into top-level (paren-depth-0)
	// comma-separated segments.
	var segments [][]token
	var cur []token
	depth := 0
	for {
		if i >= len(toks) || toks[i].kind == tkEOF {
			return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: unterminated column list")
		}
		t := toks[i]
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
			cur = append(cur, t)
			i++
		case t.kind == tkPunct && t.text == ")":
			if depth == 0 {
				segments = append(segments, cur)
				i++
				goto doneSegments
			}
			depth--
			cur = append(cur, t)
			i++
		case t.kind == tkPunct && t.text == "," && depth == 0:
			segments = append(segments, cur)
			cur = nil
			i++
		default:
			cur = append(cur, t)
			i++
		}
	}
doneSegments:

	// toks[i:] is everything after the column list's closing ")" -- the
	// table-option tail. See this function's doc comment for why STRICT is
	// read from the text here rather than passed in.
	strict := tailDeclaresStrict(toks[i:])

	var cols []columnInfo
	var checks []checkConstraint
	var specs []autoIndexSpec
	// fkConstraints collects each table-level FOREIGN KEY's shape so its
	// column counts can be validated once every column is known -- see
	// validateForeignKeyShapes.
	var fkConstraints []fkShape
	tableConstraintKeywords := map[string]bool{
		"PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true, "CONSTRAINT": true,
	}
	// NULL is one of these: SQLite's grammar has a bare "NULL" column
	// constraint (parse.y's `ccons ::= NULL onconf`), so "id INTEGER NULL"
	// declares the type INTEGER and then a no-op constraint -- not the type
	// "INTEGER NULL". Without it here that column was not a rowid alias, and
	// "id INTEGER NULL PRIMARY KEY AUTOINCREMENT" was declined outright.
	colConstraintStart := map[string]bool{
		"PRIMARY": true, "NOT": true, "NULL": true, "UNIQUE": true, "CHECK": true,
		"CONSTRAINT": true, "DEFAULT": true, "COLLATE": true, "REFERENCES": true,
		"GENERATED": true, "AS": true,
	}

	// A comma between TABLE constraints is optional in SQLite's grammar, so one
	// comma-separated segment may hold several of them -- "CONSTRAINT one
	// PRIMARY KEY(a) CONSTRAINT two CHECK(b<10) UNIQUE(b) CONSTRAINT three"
	// declares three enforced constraints plus a dangling name (schema5.test
	// 1.3, verified directly: inserting (10,11,12) is "CHECK constraint
	// failed: two"). Expand those into one segment each up front, so the loop
	// below keeps handling exactly one constraint per segment; taking only the
	// first, as this used to, silently DROPPED the rest.
	segments = expandTableConstraintSegments(segments, tableConstraintKeywords)

	// carriedName is a "CONSTRAINT <name>" that named no constraint of its own
	// (it ended its segment): SQLite's grammar makes it the name of the NEXT
	// constraint declared, wherever that falls. A trailing one at the very end
	// names nothing and is simply accepted -- "CREATE TABLE t(a,b,c,
	// CONSTRAINT one)" is legal (verified directly), while a bare "CONSTRAINT"
	// with no name at all stays a syntax error in both engines.
	carriedName := ""
	// sawTableConstraint closes the column list: in parse.y (223, 251, 462) all
	// columns precede the first table constraint, so "CREATE TABLE t(a,
	// CHECK(a>0), b)" is a syntax error near "b".
	sawTableConstraint := false
	primaryKeyCount := 0 // total PRIMARY KEY declarations seen (column- or table-level); >1 is a real CREATE TABLE error
	// deferredPK is the spec of a WITHOUT ROWID table's PRIMARY KEY that would
	// have been the rowid alias in a rowid table, or -1. See its use at the end.
	deferredPK := -1
	for _, seg := range segments {
		if len(seg) == 0 {
			continue
		}
		// A single-quoted column NAME ("CREATE TABLE t('c0a', 'c1b')" -- the
		// spelling fts3/fts4 use for their shadow tables) is an identifier
		// here, not a string literal; see asCreateTableIdent. Rewritten in
		// place so every rule below sees an ordinary quoted identifier. A
		// leading string can never be a table-constraint keyword, so this is
		// unambiguous.
		if seg[0].kind == tkString {
			seg[0], _ = asCreateTableIdent(seg[0])
		}
		first := seg[0]
		if first.kind == tkIdent && tableConstraintKeywords[first.upper()] {
			sawTableConstraint = true
			// Table-level constraint: "[CONSTRAINT name] PRIMARY KEY(...)", "UNIQUE(...)",
			// "CHECK(...)" or FOREIGN KEY. A CONSTRAINT name prefix is stripped first so
			// CHECK can keep its name.
			kwTok := first
			rest := seg
			constraintName := ""
			if kwTok.upper() == "CONSTRAINT" {
				k := 1
				// See the objectNameToken note in splitTableConstraints above:
				// a quoted constraint name is legal and used to drop the whole
				// constraint. Verified against mattn/go-sqlite3 3.53.3 that
				// "CONSTRAINT 'u' UNIQUE(a)" really enforces, and that
				// "CONSTRAINT 'c' CHECK(a>10)" really raises.
				if k < len(seg) {
					if nm, ok := objectNameToken(seg[k]); ok {
						constraintName = nm
						k++
					}
				}
				if k >= len(seg) {
					if constraintName == "" {
						// A bare "CONSTRAINT" naming nothing: not a
						// constraint, and CONSTRAINT cannot be a column name
						// either -- see the PRIMARY case below.
						return nil, nil, nil, fmt.Errorf("engine: near \",\": syntax error")
					}
					// "CONSTRAINT <name>" with no constraint after it: the
					// name belongs to whatever is declared next.
					carriedName = constraintName
					continue
				}
				kwTok = seg[k]
				rest = seg[k:]
			}
			// This segment declares a real constraint, so any carried name is
			// now spent (whether or not this constraint kind records one).
			inheritedName := carriedName
			carriedName = ""
			switch kwTok.upper() {
			case "PRIMARY":
				// "PRIMARY KEY(col [ASC] AUTOINCREMENT)" is the table-constraint spelling of
				// "col INTEGER PRIMARY KEY AUTOINCREMENT": it aliases the rowid; anything but
				// one exactly-INTEGER column is "AUTOINCREMENT is only allowed on an INTEGER
				// PRIMARY KEY". DESC/COLLATE spellings stay declined.
				rest, hasAutoinc := stripPKConstraintAutoincrement(rest)
				names, colDesc, colColls := extractParenIdentListFull(rest)
				anyDesc := anyTrue(colDesc)
				if len(names) == 0 {
					// A bare "PRIMARY" with no "(col, ...)" is not a
					// constraint at all, and PRIMARY cannot be a column name
					// either -- C SQLite says near ",": syntax error
					// (see nonIdentifierKeywords). Silently skipping it, as
					// this used to, accepted a CREATE TABLE SQLite rejects.
					return nil, nil, nil, fmt.Errorf("engine: near \",\": syntax error")
				}
				onConf, onConfSet, cerr := extractTrailingOnConflict(rest)
				if cerr != nil {
					return nil, nil, nil, cerr
				}
				primaryKeyCount++
				if hasAutoinc && (anyDesc || (len(names) == 1 && colColls[0] != "")) {
					return nil, nil, nil, fmt.Errorf("engine: unsupported: a DESC-ordered or COLLATE-carrying AUTOINCREMENT PRIMARY KEY constraint is not supported by this write path (its rowid-alias semantics are unpinned)")
				}
				collapsed, integerPK := false, false
				if len(names) == 1 {
					for idx := range cols {
						if equalFoldName(cols[idx].Name, names[0]) &&
							equalFoldName(strings.TrimSpace(cols[idx].DeclType), "INTEGER") {
							integerPK = true
							if !forceNoRowidAlias {
								cols[idx].IsRowidAlias = true
								cols[idx].RowidConflict = onConf
								collapsed = true
							}
							break
						}
					}
				}
				if collapsed {
					// Single INTEGER-column PRIMARY KEY: the rowid alias
					// itself, no autoindex (see doc comment above) -- its
					// own declared ON CONFLICT action instead lives on
					// the collapsed-into column's own RowidConflict
					// field, just set above (see that field's doc
					// comment).
					continue
				}
				if hasAutoinc {
					// The constraint carried AUTOINCREMENT but no rowid alias
					// came of it (a non-INTEGER or unknown column, several
					// columns, or a WITHOUT ROWID table): C SQLite rejects
					// every probed variant of this ("AUTOINCREMENT is only
					// allowed on an INTEGER PRIMARY KEY"; "AUTOINCREMENT not
					// allowed on WITHOUT ROWID tables"), and so does this
					// path -- erroring, never quietly dropping the keyword.
					if forceNoRowidAlias {
						return nil, nil, nil, fmt.Errorf("engine: AUTOINCREMENT not allowed on WITHOUT ROWID tables")
					}
					return nil, nil, nil, fmt.Errorf("engine: AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
				}
				if integerPK {
					deferredPK = len(specs)
				}
				specs = append(specs, autoIndexSpec{cols: names, kind: "pk", inline: false, desc: colDesc, colls: colColls, anyDesc: anyDesc, onConflict: onConf, onConflictSet: onConfSet})
			case "UNIQUE":
				names, colDesc, colColls := extractParenIdentListFull(rest)
				if len(names) == 0 {
					return nil, nil, nil, fmt.Errorf("engine: near \",\": syntax error") // see the PRIMARY case
				}
				onConf, onConfSet, cerr := extractTrailingOnConflict(rest)
				if cerr != nil {
					return nil, nil, nil, cerr
				}
				specs = append(specs, autoIndexSpec{cols: names, kind: "u", inline: false, desc: colDesc, colls: colColls, onConflict: onConf, onConflictSet: onConfSet})
			case "CHECK":
				cc, closeIdx, cerr := parseCheckClauseStructural(createSQL, rest, 0)
				if cerr != nil {
					return nil, nil, nil, cerr
				}
				// A table-level CHECK's trailing ON CONFLICT is legal but a no-op in C
				// (conflict.test 16.2/16.3): the keyword is validated and discarded.
				if _, _, cerr2 := extractTrailingOnConflict(rest[closeIdx+1:]); cerr2 != nil {
					return nil, nil, nil, cerr2
				}
				if constraintName == "" {
					constraintName = inheritedName
				}
				cc.name = constraintName
				checks = append(checks, cc)
			case "FOREIGN":
				// A FOREIGN KEY table constraint is deliberately not enforced
				// by this write path, but it must at least be WELL FORMED:
				// bare "FOREIGN" with no "KEY (col, ...)" is not a constraint,
				// and FOREIGN cannot be a column name either -- see the
				// PRIMARY case above.
				if len(rest) < 2 || rest[1].kind != tkIdent || rest[1].upper() != "KEY" ||
					len(extractParenIdentList(rest)) == 0 {
					return nil, nil, nil, fmt.Errorf("engine: near \",\": syntax error")
				}
				fkConstraints = append(fkConstraints, parseFKConstraintShape(rest))
			}
			continue
		}

		if sawTableConstraint {
			// See sawTableConstraint's declaration: the column list is closed.
			return nil, nil, nil, fmt.Errorf("engine: near %q: syntax error", seg[0].text)
		}

		name := seg[0].text
		// A COLUMN-level "REFERENCES parent(cols...)" may name at most ONE
		// referenced column -- C SQLite: "foreign key on c should
		// reference only one column of table t4" (table.test), verified
		// directly. The table-level form has its own count rule instead (see
		// validateForeignKeyShapes).
		for j := 1; j < len(seg); j++ {
			if seg[j].kind != tkIdent || seg[j].upper() != "REFERENCES" {
				continue
			}
			if j+1 < len(seg) && seg[j+1].kind == tkIdent {
				if pc := extractParenIdentList(seg[j+1:]); len(pc) > 1 {
					return nil, nil, nil, fmt.Errorf("engine: foreign key on %s should reference only one column of table %s", name, seg[j+1].text)
				}
			}
			break
		}
		// A RESERVED keyword cannot be a bare column name -- C SQLite's
		// grammar makes only SOME keywords fall back to an identifier, and
		// "CREATE TABLE t(a, ON, c)" is "near \"ON\": syntax error"
		// (alter.test/index.test). The exact set was derived FROM the oracle,
		// one CREATE TABLE per keyword -- see nonIdentifierKeywords -- and a
		// QUOTED spelling stays legal, which is why seg[0].quoted exempts it.
		if seg[0].kind == tkIdent && !seg[0].quoted && nonIdentifierKeywords[seg[0].upper()] {
			return nil, nil, nil, fmt.Errorf("engine: near %q: syntax error", seg[0].text)
		}
		j := 1
		var typeToks []token
		for j < len(seg) {
			tt := seg[j]
			if tt.kind == tkIdent && colConstraintStart[tt.upper()] {
				break
			}
			typeToks = append(typeToks, tt)
			j++
		}
		if gerr := validateColumnDefGrammar(seg, 1, j); gerr != nil {
			return nil, nil, nil, gerr
		}
		isColPK := false
		pkDesc := false
		isColUnique := false
		notNull := false
		pkConflict := conflictAbort
		uniqueConflict := conflictAbort
		pkConflictSet, uniqueConflictSet := false, false
		notNullConflict := conflictAbort
		hasDefault := false
		defaultValue := Value{}
		defaultKnown := false
		var defaultDeferred Expr
		hasCollate := false
		collateName := ""
		hasGenerated := false
		generatedExpr := ""
		generatedExprOff := 0
		generatedStored := false
		pendingCheckName := "" // most recent unconsumed "CONSTRAINT name" seen since the last constraint that consumed one
		for j < len(seg) {
			// "[GENERATED ALWAYS] AS (<expr>) [STORED|VIRTUAL]": the AS marks the clause;
			// the expression text is captured like a CHECK's, and the trailing keyword
			// (default VIRTUAL) picks the storage kind.
			if seg[j].kind == tkIdent && seg[j].upper() == "GENERATED" {
				// Skip the optional "GENERATED ALWAYS" prefix; the "AS" that
				// follows is handled by the next iteration.
				j++
				if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "ALWAYS" {
					j++
				}
				continue
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "AS" {
				gc, closeIdx, gerr := parseCheckClauseStructural(createSQL, seg, j)
				if gerr != nil {
					return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: column %s: %v", name, gerr)
				}
				if gc.exprText == "" {
					return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: column %s: empty generated-column expression", name)
				}
				// A generated column's expression is COMPILED, so RAISE() in
				// one is C SQLite's "RAISE() may only be used within a
				// trigger-program" (gencol1.test). Verified directly; note
				// SQLite ACCEPTS RAISE inside a CHECK or a view body, which
				// is why this is checked here rather than in the shared
				// expression parser.
				if toks, lerr := lex(gc.exprText); lerr == nil {
					gp := newParser(gc.exprText, append(toks, token{kind: tkEOF}))
					ge, perr := gp.parseExpr()
					if perr != nil && strings.Contains(perr.Error(), "RAISE() may only be used") {
						return nil, nil, nil, fmt.Errorf("engine: RAISE() may only be used within a trigger-program")
					}
					// A generated column's expression is COMPILED, so a call
					// to a function that does not exist is a CREATE-time
					// error -- C SQLite: "no such function: f2"
					// (trustschema1.test), verified directly. An ordinary
					// column's DEFAULT is not compiled this way and is
					// unaffected.
					if perr == nil {
						if fn, bad := firstUnknownFuncName(ge); bad {
							return nil, nil, nil, fmt.Errorf("engine: no such function: %s", fn)
						}
						// resolve.c refuses a subquery under NC_GenCol --
						// sqlite3ResolveNotValid(..., "subqueries", NC_SelfRef,
						// ...), worded "%s prohibited in %s" (resolve.c:921-923).
						if containsSubquery(ge) {
							return nil, nil, nil, fmt.Errorf("engine: subqueries prohibited in generated columns")
						}
						// A function C has but does not mark SQLITE_FUNC_CONSTANT is rejected in a
						// generated column (resolve.c:1220). The compile-option diagnostics are the two
						// that reach here; see compileOptionDiagFuncName.
						if exprCallsCompileOptionDiag(ge) {
							return nil, nil, nil, fmt.Errorf("engine: non-deterministic functions prohibited in generated columns")
						}
					}
				}
				if hasDefault {
					// "b DEFAULT 5 AS (a+1)" -- the DEFAULT was read first;
					// see the DEFAULT branch below for the other ordering
					// and for why the combination is rejected at all.
					return nil, nil, nil, fmt.Errorf("engine: error in generated column %q", name)
				}
				hasGenerated = true
				generatedExpr = gc.exprText
				generatedExprOff = gc.r32nBodyOff
				generatedStored = false // SQLite's default is VIRTUAL
				j = closeIdx + 1
				if j < len(seg) && seg[j].kind == tkIdent {
					switch seg[j].upper() {
					case "STORED":
						generatedStored = true
						j++
					case "VIRTUAL":
						j++
					}
				}
				continue
			}
			// A column-level "CONSTRAINT name" names the following constraint (usually
			// CHECK); capture it and continue.
			if seg[j].kind == tkIdent && seg[j].upper() == "CONSTRAINT" {
				if j+1 < len(seg) {
					if nm, ok := objectNameToken(seg[j+1]); ok {
						pendingCheckName = nm
						j += 2
						continue
					}
				}
				j++
				continue
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "CHECK" {
				cc, closeIdx, cerr := parseCheckClauseStructural(createSQL, seg, j)
				if cerr != nil {
					return nil, nil, nil, cerr
				}
				// A column-level CHECK does not accept a trailing ON CONFLICT in C's grammar
				// (conflict.test 16.1); reject with C's wording.
				if closeIdx+1 < len(seg) && seg[closeIdx+1].kind == tkIdent && seg[closeIdx+1].upper() == "ON" {
					return nil, nil, nil, fmt.Errorf(`engine: CREATE TABLE: near "ON": syntax error`)
				}
				cc.name = pendingCheckName
				cc.ownerCol = name
				checks = append(checks, cc)
				pendingCheckName = ""
				j = closeIdx + 1
				continue
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "PRIMARY" {
				isColPK = true
				k := j + 1
				if k < len(seg) && seg[k].kind == tkIdent && seg[k].upper() == "KEY" {
					k++
				}
				// sortorder ::= ASC | DESC | (empty) (parse.y:938-940); ASC is
				// the default, so only DESC changes anything.
				if k < len(seg) && seg[k].kind == tkIdent && seg[k].upper() == "DESC" {
					pkDesc = true
					k++
				} else if k < len(seg) && seg[k].kind == tkIdent && seg[k].upper() == "ASC" {
					k++
				}
				if act, nk, ok, perr := parseTrailingConflictClause(seg, k); perr != nil {
					return nil, nil, nil, perr
				} else if ok {
					pkConflict, pkConflictSet = act, true
					j = nk
					continue
				}
				j = k
				continue
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "UNIQUE" {
				isColUnique = true
				if act, nk, ok, perr := parseTrailingConflictClause(seg, j+1); perr != nil {
					return nil, nil, nil, perr
				} else if ok {
					uniqueConflict, uniqueConflictSet = act, true
					j = nk
					continue
				}
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "NOT" &&
				j+1 < len(seg) && seg[j+1].kind == tkIdent && seg[j+1].upper() == "NULL" {
				notNull = true
				if act, nk, ok, perr := parseTrailingConflictClause(seg, j+2); perr != nil {
					return nil, nil, nil, perr
				} else if ok {
					notNullConflict = act
					j = nk
					continue
				}
			}
			// "ccons ::= NULL onconf" (parse.y:412) has no action at all: a bare
			// NULL constraint's conflict clause is parsed and dropped.
			if seg[j].kind == tkIdent && seg[j].upper() == "NULL" && (j == 0 || seg[j-1].kind != tkIdent || seg[j-1].upper() != "NOT") {
				if _, nk, ok, perr := parseTrailingConflictClause(seg, j+1); perr != nil {
					return nil, nil, nil, perr
				} else if ok {
					j = nk
					continue
				}
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "ON" &&
				j+1 < len(seg) && seg[j+1].kind == tkIdent && seg[j+1].upper() == "CONFLICT" {
				// An ON CONFLICT not consumed by a PRIMARY KEY, UNIQUE or NOT NULL constraint
				// above is in a position C's grammar does not allow; reject it.
				return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: ON CONFLICT clause in this position is not supported by this write path")
			}
			if seg[j].kind == tkIdent && seg[j].upper() == "DEFAULT" {
				// A generated column may not have a DEFAULT, and C's message depends on which
				// came first: "b AS (a+1) DEFAULT 5" is "cannot use DEFAULT on a generated
				// column", "b DEFAULT 5 AS (a+1)" is `error in generated column "b"`.
				if hasGenerated {
					return nil, nil, nil, fmt.Errorf("engine: cannot use DEFAULT on a generated column")
				}
				hasDefault = true
				if derr := checkDefaultIsConstant(createSQL, name, seg, j); derr != nil {
					return nil, nil, nil, derr
				}
				dv, dknown, dend, ddefer := parseColumnDefault(createSQL, seg, j)
				// Only another column constraint may follow a DEFAULT, so a leftover token means
				// C would have rejected the table ("DEFAULT 1+2"). Keep parsing tolerantly but do
				// not trust the partially folded default. NULL and DEFERRABLE are legal
				// followers too.
				if dknown && dend < len(seg) {
					nx := seg[dend]
					if !(nx.kind == tkIdent && (colConstraintStart[nx.upper()] || nx.upper() == "NULL" || nx.upper() == "DEFERRABLE")) {
						dknown = false
					}
				}
				// Same rule for a DEFERRED default: a leftover token means the
				// CREATE TABLE is one C SQLite would have rejected, so do
				// not trust the expression parsed out of its prefix either.
				if ddefer != nil && dend < len(seg) {
					nx := seg[dend]
					if !(nx.kind == tkIdent && (colConstraintStart[nx.upper()] || nx.upper() == "NULL" || nx.upper() == "DEFERRABLE")) {
						ddefer = nil
					}
				}
				defaultValue, defaultKnown, defaultDeferred = dv, dknown, ddefer
				j = dend
				continue
			}
			// A column-level COLLATE sets the column's default collation: BINARY, NOCASE or
			// RTRIM. Any other name is rejected rather than treated as BINARY, which would
			// return wrong rows.
			if seg[j].kind == tkIdent && seg[j].upper() == "COLLATE" && j+1 < len(seg) {
				// The collation name is usually a bare identifier (.text),
				// but SQLite also accepts a quoted string there (a lexer
				// tkString token carries its decoded value in .str, not
				// .text -- see sql_lexer.go).
				hasCollate = true
				if seg[j+1].kind == tkString {
					collateName = seg[j+1].str
				} else {
					collateName = seg[j+1].text
				}
			}
			// Skip a column-level REFERENCES clause whole: its "ON DELETE SET DEFAULT" tail
			// ends in DEFAULT, which this loop would otherwise read as a second DEFAULT and
			// lose the real one.
			if seg[j].kind == tkIdent && seg[j].upper() == "REFERENCES" {
				j = skipColumnReferencesClause(seg, j)
				continue
			}
			j++
		}
		if hasCollate && !knownCollations[asciiFold(collateName, false)] {
			// C's own words for the same rejection: sqlite3LocateCollSeq's
			// "no such collation sequence: %s" (callback.c:230). The three
			// built-ins are exactly BINARY/NOCASE/RTRIM in C too, and this
			// engine registers no others, so the two agree on WHICH names are
			// refused -- only the wording differed.
			return nil, nil, nil, fmt.Errorf("engine: no such collation sequence: %s", collateName)
		}
		collation := "BINARY"
		if hasCollate {
			collation = strings.ToUpper(collateName)
		}

		if isColPK {
			primaryKeyCount++
		}
		declType := joinTypeTokens(typeToks)
		aff := typeAffinity(declType)
		if strict {
			// STRICT column vocabulary (strictColType/strictColAffinity) with C's
			// "missing"/"unknown datatype" wording. The echoed type is reconstructed by
			// joinTypeTokens, which normalizes whitespace inside a multi-token type where C
			// echoes the raw text.
			st, ok := strictColTypeOf(declType)
			if !ok {
				if declType == "" {
					return nil, nil, nil, fmt.Errorf("engine: missing datatype for %s.%s", tblName, name)
				}
				return nil, nil, nil, fmt.Errorf("engine: unknown datatype for %s.%s: %q", tblName, name, declType)
			}
			aff = strictColAffinity(st)
		}
		rowidAlias := !forceNoRowidAlias && isColPK && !pkDesc && equalFoldName(strings.TrimSpace(declType), "INTEGER")
		if hasGenerated && rowidAlias {
			// C SQLite: "generated columns cannot be part of the PRIMARY
			// KEY" for a rowid table -- the INTEGER PRIMARY KEY alias IS the
			// rowid, which a computed value can never back.
			return nil, nil, nil, fmt.Errorf("engine: CREATE TABLE: column %s: generated columns cannot be the INTEGER PRIMARY KEY", name)
		}
		newCol := columnInfo{Name: name, DeclType: declType, Aff: aff, IsRowidAlias: rowidAlias, NotNull: notNull, HasDefault: hasDefault,
			DefaultValue: defaultValue, DefaultKnown: defaultKnown, DefaultDeferred: defaultDeferred, Collation: collation,
			GeneratedExpr: generatedExpr, GeneratedStored: generatedStored,
			R32NGeneratedOff: generatedExprOff}
		if rowidAlias {
			newCol.RowidConflict = pkConflict
		}
		if notNull {
			newCol.NotNullConflict = notNullConflict
		}
		cols = append(cols, newCol)
		// A column-level PRIMARY KEY that did NOT collapse into the rowid
		// alias (non-integer type, a DESC-ordered key, or forceNoRowidAlias --
		// see rowidAlias above) still enforces uniqueness like any other
		// PRIMARY KEY, so it needs its own autoindex. A column-level UNIQUE is
		// independent of PRIMARY KEY-ness (e.g. "id INTEGER PRIMARY KEY
		// UNIQUE" gets an autoindex for the redundant UNIQUE even though id is
		// the rowid alias) and is always recorded when present.
		if isColPK && !rowidAlias {
			if forceNoRowidAlias && !pkDesc && equalFoldName(strings.TrimSpace(declType), "INTEGER") {
				deferredPK = len(specs)
			}
			specs = append(specs, autoIndexSpec{cols: []string{name}, kind: "pk", inline: true, desc: []bool{pkDesc}, anyDesc: pkDesc, onConflict: pkConflict, onConflictSet: pkConflictSet})
		}
		if isColUnique {
			specs = append(specs, autoIndexSpec{cols: []string{name}, kind: "u", inline: true, onConflict: uniqueConflict, onConflictSet: uniqueConflictSet})
		}
	}

	// A table with two PRIMARY KEY declarations (column-level, table-level,
	// or one of each) is itself a CREATE TABLE error in C SQLite, not a
	// silently-accepted redundancy -- verified directly (misc1.test). This
	// write path had no such check at all before this fix, silently
	// accepting e.g. "CREATE TABLE t(a INTEGER PRIMARY KEY, b, PRIMARY
	// KEY(b))" and building two independent autoindex specs for it instead
	// of erroring.
	if primaryKeyCount > 1 {
		// C names the table and quotes it -- `table "q" has more than one
		// primary key` -- and prepends no statement context.
		return nil, nil, nil, fmt.Errorf("engine: table %q has more than one primary key", createTableNameOf(createSQL))
	}
	// Specs are in the order C creates the indexes, which numbers them
	// (build.c:4100): text order, except that a WITHOUT ROWID table's single-column
	// INTEGER PRIMARY KEY is converted to an index at the end of the CREATE
	// (build.c:1868, 2388), after every UNIQUE. Otherwise the PK takes a UNIQUE's
	// autoindex name and C reads the file as malformed.
	if deferredPK >= 0 && deferredPK < len(specs)-1 {
		pk := specs[deferredPK]
		specs = append(append(specs[:deferredPK:deferredPK], specs[deferredPK+1:]...), pk)
	}

	if strict || forceNoRowidAlias {
		// PRIMARY KEY columns are NOT NULL in a WITHOUT ROWID table
		// (convertToWithoutRowidTable, build.c:2362) and in a STRICT table. Applied over
		// specs, which covers inline and table-level forms; the rowid alias produces no
		// spec and stays nullable, as in C. The planner reads this notnull
		// (whereIndexColumnNotNull); finalizeWithoutRowidPK applies the same rule on the
		// write path, idempotently.
		for _, s := range specs {
			if s.kind != "pk" {
				continue
			}
			for _, cn := range s.cols {
				for i := range cols {
					if equalFoldName(cols[i].Name, cn) {
						cols[i].NotNull = true
					}
				}
			}
		}
	}

	if derr := validateNoDuplicateColumns(cols); derr != nil {
		return nil, nil, nil, derr
	}
	if ferr := validateForeignKeyShapes(cols, fkConstraints); ferr != nil {
		return nil, nil, nil, ferr
	}
	compileGeneratedColumns(tblName, cols)
	compileDeferredDefaults(cols)
	return cols, checks, specs, nil
}

// compileDeferredDefaults compiles every clock-reading DEFAULT
// (columnInfo.DefaultDeferred) into a program stamped on its columnInfo, so
// applyColumnDefaults runs opcodes instead of walking the clause, as C codes a
// default per INSERT (insert.c:1395, 1407, 1415; 2013 for REPLACE). Runs where
// the column list is derived, beside compileGeneratedColumns.
//
// The scope is empty, with noRowid: a DEFAULT may reference no column
// (checkDefaultIsConstant rejects that at CREATE), an empty list is the same for
// every table so the program never goes stale, and it matches the empty context
// applyColumnDefaults evaluates in.
func compileDeferredDefaults(cols []columnInfo) {
	for i := range cols {
		cols[i].defaultProg = nil
		if cols[i].DefaultDeferred != nil {
			// pureCtxNone: a DEFAULT clause is where "CURRENT_TIMESTAMP"
			// LEGITIMATELY reads the clock, per INSERT.
			cols[i].defaultProg = compileSelfRowExpr(tableScope{noRowid: true}, cols[i].DefaultDeferred, false, pureCtxNone)
		}
	}
}

// compileGeneratedColumns compiles every generated column's body against the
// row-in-registers block and stamps genProg, as sqlite3ComputeGeneratedColumns
// codes them (insert.c:285, 353, 372). Runs where every table's column list is
// derived from its CREATE text; refreshRowPrograms re-enters for in-place ALTER
// edits. Every generated column is restamped, nil if its body no longer
// parses, so no stale program survives. noRowid because C excludes rowid in a
// generated column (resolve.c:626: "y AS (rowid*2)" is "no such column").
func compileGeneratedColumns(tblName string, cols []columnInfo) {
	if !hasGeneratedCols(cols) {
		return
	}
	scope := tableScope{name: tblName, cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	for i := range cols {
		if !cols[i].IsGenerated() {
			continue
		}
		cols[i].genProg = nil
		cols[i].genDeps = nil
		if e, err := cachedGeneratedExpr(cols[i].GeneratedExpr); err == nil {
			cols[i].genProg = compileSelfRowExpr(scope, e, false /* NC_GenCol is NOT in resolve.c:316's mask */, pureCtxGenerated)
			cols[i].genDeps = generatedExprDeps(e, scope.colIndex)
		}
	}
}

// parseTrailingConflictClause checks whether seg[idx:] begins with "ON CONFLICT
// <action>" (IGNORE/REPLACE/ABORT/FAIL/ROLLBACK), a column constraint's optional
// trailing clause. ok is false when it does not; an unknown action after a real
// "ON CONFLICT" is a parse error.
func parseTrailingConflictClause(seg []token, idx int) (action conflictAction, next int, ok bool, err error) {
	if idx+1 >= len(seg) || seg[idx].kind != tkIdent || seg[idx].upper() != "ON" ||
		seg[idx+1].kind != tkIdent || seg[idx+1].upper() != "CONFLICT" {
		return conflictAbort, idx, false, nil
	}
	if idx+2 >= len(seg) || seg[idx+2].kind != tkIdent {
		return 0, idx, false, fmt.Errorf("engine: CREATE TABLE: expected IGNORE/REPLACE/ABORT/FAIL/ROLLBACK after ON CONFLICT")
	}
	switch seg[idx+2].upper() {
	case "IGNORE":
		return conflictIgnore, idx + 3, true, nil
	case "REPLACE":
		return conflictReplace, idx + 3, true, nil
	case "ABORT":
		return conflictAbort, idx + 3, true, nil
	case "FAIL":
		return conflictFail, idx + 3, true, nil
	case "ROLLBACK":
		return conflictRollback, idx + 3, true, nil
	default:
		return 0, idx, false, fmt.Errorf("engine: CREATE TABLE: expected IGNORE/REPLACE/ABORT/FAIL/ROLLBACK after ON CONFLICT, got %q", seg[idx+2].text)
	}
}

// extractTrailingOnConflict finds a trailing "ON CONFLICT <action>" in a
// table-level PRIMARY KEY/UNIQUE/CHECK segment and returns the action, or
// conflictAbort when absent. PRIMARY KEY/UNIQUE store it; CHECK validates and
// discards it (a no-op in C, conflict.test 16.2/16.3).
func extractTrailingOnConflict(seg []token) (act conflictAction, declared bool, err error) {
	for i := 0; i+1 < len(seg); i++ {
		if seg[i].kind == tkIdent && seg[i].upper() == "ON" &&
			seg[i+1].kind == tkIdent && seg[i+1].upper() == "CONFLICT" {
			act, _, ok, err := parseTrailingConflictClause(seg, i)
			if err != nil {
				return 0, false, err
			}
			if !ok {
				return 0, false, fmt.Errorf("engine: CREATE TABLE: expected IGNORE/REPLACE/ABORT/FAIL/ROLLBACK after ON CONFLICT")
			}
			return act, true, nil
		}
	}
	return conflictAbort, false, nil
}

// extractParenIdentList finds the first "( ident [, ident...] )" in seg and
// returns the identifier names, used to parse table-level "PRIMARY KEY
// (col1, col2)" constraints.
func extractParenIdentList(seg []token) []string {
	names, _ := extractParenIdentListDesc(seg)
	return names
}

// isSortOrderKeyword reports whether tok is a trailing ASC/DESC in a constraint's
// column list. COLLATE is handled separately. Any other unexpected identifier is
// collected as a column name so it fails loudly in buildAutoIndexes rather than
// enforcing uniqueness under the wrong collation.
func isSortOrderKeyword(tok token, haveName bool) bool {
	if !haveName {
		return false
	}
	u := tok.upper()
	return u == "ASC" || u == "DESC"
}

// extractParenIdentListDesc is extractParenIdentList extended to report each
// column's own DESC flag ("PRIMARY KEY(a DESC, b)" -> [true false]). Both the
// per-column flags (autoIndexSpec.desc, which becomes the automatic index's
// physical b-tree order -- see indexMeta.colDesc) and the "any of them"
// summary (autoIndexSpec.anyDesc, see its own doc comment for what still
// consults it) come from here; extractParenIdentList is the names-only
// wrapper its other callers use.
func extractParenIdentListDesc(seg []token) (names []string, desc []bool) {
	names, desc, _ = extractParenIdentListFull(seg)
	return names, desc
}

// extractParenIdentListFull is extractParenIdentListDesc plus each column's
// constraint-level COLLATE, the whole of SQLite's indexed-column grammar:
//
//	eidlist ::= nm collate sortorder
//
// colls is "" where the constraint names none (the column's own collation then
// governs). Accepts "PRIMARY KEY(a COLLATE nocase)" and "PRIMARY KEY('a' ASC)".
// The COLLATE decides which rows conflict, so it cannot be dropped.
func extractParenIdentListFull(seg []token) (names []string, desc []bool, colls []string) {
	i := 0
	for i < len(seg) && !(seg[i].kind == tkPunct && seg[i].text == "(") {
		i++
	}
	if i >= len(seg) {
		return nil, nil, nil
	}
	i++
	for i < len(seg) && !(seg[i].kind == tkPunct && seg[i].text == ")") {
		// A STRING in a name position is SQLite's own backwards compatibility
		// ("nm ::= STRING"); its decoded value is in .str, not .text.
		if seg[i].kind == tkString && len(names) == len(colls) && (len(names) == 0 || i > 0 && seg[i-1].kind == tkPunct && seg[i-1].text == ",") {
			names = append(names, seg[i].str)
			desc = append(desc, false)
			colls = append(colls, "")
			i++
			continue
		}
		if seg[i].kind == tkIdent {
			if isSortOrderKeyword(seg[i], len(names) > 0) {
				if seg[i].upper() == "DESC" && len(desc) > 0 {
					desc[len(desc)-1] = true
				}
				i++
				continue
			}
			// "COLLATE <name>" attaches to the column just collected. The
			// name may be an identifier or a quoted string, exactly like a
			// column-level COLLATE clause.
			if seg[i].upper() == "COLLATE" && !seg[i].quoted && len(names) > 0 && i+1 < len(seg) {
				nx := seg[i+1]
				switch {
				case nx.kind == tkString:
					colls[len(colls)-1] = nx.str
				case nx.kind == tkIdent:
					colls[len(colls)-1] = nx.text
				default:
					// Not a name at all -- leave both tokens for the ordinary
					// path below, which collects them and fails loudly.
					names = append(names, seg[i].text)
					desc = append(desc, false)
					colls = append(colls, "")
					i++
					continue
				}
				i += 2
				continue
			}
			names = append(names, seg[i].text)
			desc = append(desc, false)
			colls = append(colls, "")
		}
		i++
	}
	return names, desc, colls
}

// stripPKConstraintAutoincrement removes AUTOINCREMENT closing a table-level
// "PRIMARY KEY ( sortlist AUTOINCREMENT )" and reports whether it was there.
// rest starts at PRIMARY; anything else is returned unchanged. A quoted
// "autoincrement" is a name.
func stripPKConstraintAutoincrement(rest []token) ([]token, bool) {
	depth := 0
	for i, t := range rest {
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			if depth == 1 {
				if i > 0 && rest[i-1].kind == tkIdent && !rest[i-1].quoted && rest[i-1].upper() == "AUTOINCREMENT" {
					out := make([]token, 0, len(rest)-1)
					out = append(out, rest[:i-1]...)
					out = append(out, rest[i:]...)
					return out, true
				}
				return rest, false
			}
			depth--
		}
	}
	return rest, false
}

// validateColumnDefGrammar holds a column definition to parse.y: seg[typeStart:
// consStart] is the typetoken (parse.y:347) and seg[consStart:] the carglist of
// ccons (parse.y:387). The constraint loop skips tokens it does not know, so
// without this "b AS (c+1) %TYPE%" was accepted. Parenthesized expressions are
// skipped whole.
func validateColumnDefGrammar(seg []token, typeStart, consStart int) error {
	bad := func(i int) error {
		if i >= len(seg) {
			return fmt.Errorf("engine: near \")\": syntax error")
		}
		return fmt.Errorf("engine: near %q: syntax error", seg[i].text)
	}
	kw := func(i int, words ...string) bool {
		if i >= len(seg) || seg[i].kind != tkIdent || seg[i].quoted {
			return false
		}
		return slices.Contains(words, seg[i].upper())
	}
	punct := func(i int, p string) bool { return i < len(seg) && seg[i].kind == tkPunct && seg[i].text == p }
	// nm/ids: an identifier -- any keyword but the ones that never fall back to
	// ID (parse.y:272's %fallback list; nonIdentifierKeywords) -- or a string.
	name := func(i int) bool {
		if i >= len(seg) {
			return false
		}
		t := seg[i]
		return t.kind == tkString || t.kind == tkIdent && (t.quoted || !nonIdentifierKeywords[t.upper()])
	}
	// skip moves past the parenthesized group starting at seg[i], or reports
	// -1 when it never closes.
	skip := func(i int) int {
		depth := 0
		for ; i < len(seg); i++ {
			if seg[i].kind != tkPunct {
				continue
			}
			switch seg[i].text {
			case "(":
				depth++
			case ")":
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	}

	// typetoken ::= typename [LP signed [COMMA signed] RP]; typename ::= ids+.
	i := typeStart
	for i < consStart && name(i) {
		i++
	}
	if i < consStart {
		if i == typeStart || !punct(i, "(") {
			return bad(i)
		}
		i++
		signed := func() bool { // plus_num | minus_num
			if punct(i, "+") || punct(i, "-") {
				i++
			}
			if i < consStart && seg[i].kind == tkNumber {
				i++
				return true
			}
			return false
		}
		if !signed() {
			return bad(i)
		}
		if punct(i, ",") {
			i++
			if !signed() {
				return bad(i)
			}
		}
		if !punct(i, ")") {
			return bad(i)
		}
		if i++; i < consStart {
			return bad(i)
		}
	}

	onconf := func() bool { // [ON CONFLICT resolvetype]
		if !kw(i, "ON") {
			return true
		}
		if !kw(i+1, "CONFLICT") || !kw(i+2, "ROLLBACK", "ABORT", "FAIL", "IGNORE", "REPLACE") {
			return false
		}
		i += 3
		return true
	}
	initDeferred := func() bool { // [INITIALLY DEFERRED|IMMEDIATE]
		if !kw(i, "INITIALLY") {
			return true
		}
		if !kw(i+1, "DEFERRED", "IMMEDIATE") {
			return false
		}
		i += 2
		return true
	}
	for i < len(seg) {
		if seg[i].kind != tkIdent || seg[i].quoted {
			return bad(i)
		}
		ok := true
		switch seg[i].upper() {
		case "CONSTRAINT":
			i++
			if ok = name(i); ok {
				i++
			}
		case "DEFAULT":
			i++
			switch {
			case punct(i, "("):
				if i = skip(i); i < 0 {
					return bad(len(seg))
				}
			case punct(i, "+"), punct(i, "-"): // DEFAULT PLUS|MINUS term
				i++
				if ok = i < len(seg) && (seg[i].kind == tkNumber || seg[i].kind == tkString || seg[i].kind == tkBlob ||
					kw(i, "NULL", "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP")); ok {
					i++
				}
			case i < len(seg) && (seg[i].kind == tkNumber || seg[i].kind == tkString || seg[i].kind == tkBlob ||
				kw(i, "NULL", "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP") || name(i)): // term | id
				i++
			default:
				ok = false
			}
		case "NULL":
			i++
			ok = onconf()
		case "NOT":
			switch i++; {
			case kw(i, "NULL"):
				i++
				ok = onconf()
			case kw(i, "DEFERRABLE"):
				i++
				ok = initDeferred()
			default:
				ok = false
			}
		case "PRIMARY":
			if ok = kw(i+1, "KEY"); ok {
				i += 2
				if kw(i, "ASC", "DESC") {
					i++
				}
				if ok = onconf(); ok && kw(i, "AUTOINCREMENT") {
					i++
				}
			}
		case "UNIQUE":
			i++
			ok = onconf()
		case "CHECK":
			i++
			if ok = punct(i, "("); ok {
				if i = skip(i); i < 0 {
					return bad(len(seg))
				}
			}
		case "REFERENCES":
			i++
			if ok = name(i); !ok {
				break
			}
			if i++; punct(i, "(") {
				if i = skip(i); i < 0 {
					return bad(len(seg))
				}
			}
			for ok { // refargs
				switch {
				case kw(i, "MATCH"):
					if ok = name(i + 1); ok {
						i += 2
					}
					continue
				case kw(i, "ON") && kw(i+1, "INSERT", "DELETE", "UPDATE"):
					i += 2
					switch { // refact
					case kw(i, "SET") && kw(i+1, "NULL", "DEFAULT"), kw(i, "NO") && kw(i+1, "ACTION"):
						i += 2
					case kw(i, "CASCADE", "RESTRICT"):
						i++
					default:
						ok = false
					}
					continue
				}
				break
			}
		case "DEFERRABLE":
			i++
			ok = initDeferred()
		case "COLLATE":
			i++
			if ok = name(i); ok {
				i++
			}
		case "GENERATED":
			if ok = kw(i+1, "ALWAYS") && kw(i+2, "AS"); !ok {
				break
			}
			i += 2
			fallthrough
		case "AS":
			i++
			if ok = punct(i, "("); !ok {
				break
			}
			if i = skip(i); i < 0 {
				return bad(len(seg))
			}
			// generated ::= LP expr RP ID: an identifier here is the storage
			// type; a keyword that starts another ccons is not one.
			if i < len(seg) && seg[i].kind == tkIdent && (seg[i].quoted || !colDefConstraintKeyword[seg[i].upper()]) {
				i++
			}
		default:
			ok = false
		}
		if !ok {
			return bad(i)
		}
	}
	return nil
}

// colDefConstraintKeyword is every keyword that opens a ccons (parse.y:389-424).
var colDefConstraintKeyword = map[string]bool{
	"CONSTRAINT": true, "DEFAULT": true, "NULL": true, "NOT": true, "PRIMARY": true,
	"UNIQUE": true, "CHECK": true, "REFERENCES": true, "DEFERRABLE": true,
	"COLLATE": true, "GENERATED": true, "AS": true,
}

// joinTypeTokens reconstructs a declared type name ("VARCHAR(10)") from its
// tokens, well enough for typeAffinity's substring matching.
func joinTypeTokens(toks []token) string {
	var sb strings.Builder
	for i, t := range toks {
		if i > 0 && t.kind == tkIdent {
			sb.WriteByte(' ')
		}
		switch t.kind {
		case tkString:
			sb.WriteString(t.str)
		default:
			sb.WriteString(t.text)
		}
	}
	return sb.String()
}

// anyTrue reports whether any element of b is true.
func anyTrue(b []bool) bool {
	for _, v := range b {
		if v {
			return true
		}
	}
	return false
}

// skipColumnReferencesClause returns the index just past a column-level
// "REFERENCES parent[(cols)] [ON DELETE|UPDATE <action>] [MATCH name]
// [[NOT] DEFERRABLE [INITIALLY ...]]" clause starting at seg[j] (the
// REFERENCES keyword). It exists because the clause's own tail contains
// keywords the column-constraint scanner would otherwise mistake for fresh
// column constraints -- see its call site for the DEFAULT case that made this
// necessary. It shares parseFKTrailingClauses (pragma_parse.go) so the two
// parsers cannot disagree about where such a clause ends.
func skipColumnReferencesClause(seg []token, j int) int {
	j++ // REFERENCES
	if j < len(seg) && seg[j].kind == tkIdent {
		j++ // parent table name
	}
	if j < len(seg) && seg[j].kind == tkPunct && seg[j].text == "(" {
		if end := matchParen(seg, j); end > j {
			j = end + 1
		} else {
			return len(seg)
		}
	}
	var discard pragmaFK
	return parseFKTrailingClauses(seg, j, &discard)
}

// createTableNameOf pulls the table name out of a CREATE TABLE statement's
// own text -- for an error message that has to name it (C quotes the table in
// "table \"q\" has more than one primary key"). Returns "" if the text does
// not start like a CREATE TABLE, which only an internal caller could manage.
func createTableNameOf(createSQL string) string {
	toks, err := lex(createSQL)
	if err != nil {
		return ""
	}
	i := 0
	kw := func(want string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && !toks[i].quoted && toks[i].upper() == want {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return ""
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TABLE") {
		return ""
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	if i >= len(toks) || !isNameToken(toks[i]) {
		return ""
	}
	name := toks[i].text
	// "schema.name": the NAME is the token after the dot.
	if i+2 < len(toks) && toks[i+1].kind == tkPunct && toks[i+1].text == "." && isNameToken(toks[i+2]) {
		name = toks[i+2].text
	}
	return name
}
