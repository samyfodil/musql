// This file bridges the SQL "<fts5tab> MATCH '<query>'" operator (MatchExpr,
// sql_ast.go) to the fts5 tokenizer + query matcher (fts5_tokenizer.go /
// fts5_query.go). Because the engine materializes an fts5 table's rows and
// re-applies the whole WHERE clause over them (like rtree -- see vtab.go), a
// MATCH is evaluated per row here: the row's fts5 columns are tokenized into an
// fts5Doc and the parsed query is matched against it. Default ascending-rowid
// result order follows from the materialization order, exactly as C fts5.
//
// MATCH is only meaningful on an fts5 table; on anything else evalMatch errors
// exactly as C SQLite ("unable to use function MATCH in the requested
// context"), never guessing.
package engine

import (
	"fmt"
	"strings"
)

// fts5SchemaTok is one schema fts5 table's tokenizer, or the reason this engine
// cannot serve it. err is set when the table's tokenize= names a tokenizer this
// engine does not implement -- which happens for a table REAL SQLITE wrote,
// since a CREATE here would have been declined. Answering a MATCH over such a
// table with the unicode61 default would be a WRONG ANSWER (a different token
// stream selects different rows), so every read path carries the error out
// instead.
type fts5SchemaTok struct {
	tok *fts5Tokenizer
	// detail is the table's detail= mode (fts5_detail.go), resolved from the
	// same stored CREATE. It decides which MATCH queries C fts5 REFUSES, so
	// like tok it has to come from the table rather than from a default: over a
	// detail=none table this engine could answer a column-filtered query fts5
	// errors on, which is a wrong answer.
	detail fts5Detail
	err    error
	// extContent is the table's content=<table> option, "" for an ordinary
	// %_content-backed one (fts5_extcontent.go). ext carries the verification
	// that file's index and its content table have not drifted apart, made
	// LAZILY: it is a pointer so that the one MATCH that needs it can fill it
	// in for the rest of the snapshot's reads.
	extContent string
	ext        *fts5ExtCheck
	// cl is the counterpart for a CONTENTLESS table (content='',
	// fts5_contentless.go): its documents are readable only out of its own
	// index, so the decode is memoized here for the snapshot the same way.
	cl *fts5ContentlessCheck
	// columnsize is the table's columnsize= option, i.e. whether it has a
	// %_docsize at all, and contentUnindexed its FTS5_CONTENT_UNINDEXED mode
	// (contentless_unindexed=1 over a table that HAS an UNINDEXED column).
	// Together they decide the two rules columnsize=0 turns on, both of which
	// key on fts5_config.c:683-702 leaving zContent NULL for -- and only for --
	// a contentless, non-content-unindexed, columnsize=0 table:
	// fts5ContentlessScanGuard's refusal, and fts5ApiColumnSize's -1
	// (fts5_main.c:2521).
	columnsize       bool
	contentUnindexed bool
	// locale is the table's locale=1 option (fts5_locale.go). It decides
	// whether %_content carries l<i> columns, which is what fts5_get_locale()
	// reads -- and, for every other table, what makes that function answer
	// NULL without touching the file at all.
	locale bool
}

// fts5ExtCheck memoizes one external-content table's index-vs-content
// verification for a read snapshot. See fts5_extcontent.go for what is checked
// and why only a MATCH needs it.
type fts5ExtCheck struct {
	done bool
	err  error
	// docids is the INDEXED document set, which is where C fts5 draws a
	// MATCH's rowids from.
	docids map[int64]bool
}

// isFts5Table reports whether name is an fts5 virtual table in this read
// snapshot's schema. The result is cached on the pager (see fts5Names).
func (p *ReadOnlyPager) isFts5Table(name string) bool {
	_, ok := p.fts5SchemaTok(name)
	return ok
}

// fts5SchemaTok returns the tokenizer of fts5 table name in this read
// snapshot's schema; ok is false when name is not an fts5 table there.
func (p *ReadOnlyPager) fts5SchemaTok(name string) (fts5SchemaTok, bool) {
	if p == nil {
		return fts5SchemaTok{}, false
	}
	if p.fts5Names == nil {
		p.fts5Names = map[string]fts5SchemaTok{}
		if rows, err := p.Schema(); err == nil {
			for _, r := range rows {
				if r.Type == "table" && isCreateVirtualTableSQL(r.SQL) {
					if _, mod, args, _, perr := parseCreateVirtualTableStmt(r.SQL); perr == nil && strings.EqualFold(mod, "fts5") {
						tok, terr := fts5TokenizerOf(args)
						detail, derr := fts5DetailOf(args)
						if terr == nil {
							terr = derr
						}
						ent := fts5SchemaTok{tok: tok, detail: detail, err: terr}
						if st, serr := (fts5Module{}).buildStore(r.Name, args); serr == nil {
							if st.extContent != "" {
								ent.extContent = st.extContent
								ent.ext = &fts5ExtCheck{}
							}
							if st.contentless {
								ent.cl = &fts5ContentlessCheck{}
							}
							ent.columnsize = st.columnsize
							ent.contentUnindexed = st.contentUnindexed
							ent.locale = st.locale
						}
						p.fts5Names[strings.ToLower(r.Name)] = ent
					}
				}
			}
		}
	}
	st, ok := p.fts5Names[strings.ToLower(name)]
	return st, ok
}

// fts5TokFor resolves the tokenizer a MATCH over tableScope t must use: the
// write path carries it on the scope, the read path looks it up in the
// snapshot's schema.
func (ctx *evalCtx) fts5TokFor(t *tableScope) (*fts5Tokenizer, error) {
	if t.isFts5 {
		return t.fts5Tok, nil
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	st, _ := ctx.pager.fts5SchemaTok(name)
	return st.tok, st.err
}

// fts5DetailFor resolves the detail= mode a MATCH over tableScope t must be
// parsed under, the same two ways fts5TokFor resolves the tokenizer: the write
// path carries it on the scope, the read path looks it up in the snapshot's
// schema.
func (ctx *evalCtx) fts5DetailFor(t *tableScope) (fts5Detail, error) {
	if t.isFts5 {
		return t.fts5Detail, nil
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	st, _ := ctx.pager.fts5SchemaTok(name)
	return st.detail, st.err
}

// evalMatch evaluates "X MATCH Pattern" (and its NOT form). It returns Int 1/0.
//
// pv is the pattern's already-computed VALUE, never its expression: real
// SQLite codes a virtual-table constraint's RIGHT operand -- which for
// "t MATCH q" is q -- into a register before the module ever runs
// (wherecode.c:1584, "        codeExprOrVector(pParse, pRight, iTarget, 1);",
// feeding the OP_VFilter at wherecode.c:1601), and fts3/fts5 then read it as a
// VALUE inside xFilter, never as an AST. Both of this engine's entry points
// now supply it that way: OpMatch reads the register compileFts5Match
// (fts5_match_placement.go) / compileMatchExpr (vdbe_codegen.go) coded
// immediately above it, and a caller still holding the operand as an
// expression compiles it through compileMatchExpr (vdbe_codegen.go). No AST
// reaches this function.
//
// The operand is evaluated BEFORE the target is resolved, which is the order
// C SQLite codes it in and an observable one. "X MATCH Y" parses to the
// infix function call match(Y, X) -- parse.y:1363-1371's likeop rule appends Y
// to the argument list first -- so the pattern is argument 0 and is coded
// first; only then does the function body raise "unable to use function MATCH
// in the requested context". Verified against 3.53.3: "SELECT 'lit' MATCH
// zeroblob(1000000000000)" reports "string or blob too big", not the MATCH
// error, and so does the same pattern over a C fts5 table.
func evalMatch(ctx *evalCtx, x MatchExpr, pv Value) (Value, error) {
	ts, singleCol, ok := ctx.fts5MatchTarget(x.X)
	if !ok {
		return Value{}, fmt.Errorf("engine: unable to use function MATCH in the requested context")
	}
	// A NULL query is NOT "matches no row" for fts5, whatever it is for fts3.
	// The comment that used to sit here said it was, and it was wrong in the
	// direction AGENTS.md invariant 1 names: "SELECT rowid FROM ft WHERE ft
	// MATCH NULL" answered zero rows and claimed success where 3.53.3 reports
	// "fts5: syntax error near """, and "DELETE FROM ft WHERE ft MATCH NULL"
	// likewise reported success. fts5FilterMethod turns the value into text
	// with the ordinary coercion and then substitutes the EMPTY STRING for a
	// NULL one --
	//
	//	fts5_main.c:1504  if( zText==0 ) zText = "";
	//
	// -- and hands that straight to sqlite3Fts5ExprNew (fts5_main.c:1523),
	// whose parser rejects an empty query. There is no NULL rule anywhere on
	// that path. fts3 is genuinely different (it leaves pCsr->pExpr NULL and
	// matches nothing, fts3.c:3375-3378), which is why only this arm changes;
	// verified on the oracle in both directions, over fts5 and fts4, in
	// SELECT/DELETE/UPDATE.
	//
	// valueToText below already renders NULL as "", so the empty-query error
	// comes out of fts5ParseQuery on its own -- exactly where an explicitly
	// empty "ft MATCH ''" already raised it.
	//
	// A MATCH pattern may itself be an fts5_locale() value: fts5ExtractExprText
	// (fts5_main.c:1411) unwraps one on the QUERY side too, and does it with no
	// bLocale guard at all -- so it applies to a locale=0 table as much as to a
	// locale=1 one. The locale it carries goes to the tokenizer, which for
	// every built-in one is locale-BLIND (fts5_locale.go), so dropping it here
	// leaves only the text -- and taking the raw blob's bytes as the query
	// instead would be a wrong answer, not a decline: a random 16-byte header
	// tokenizes to terms no document holds, so the MATCH would quietly select
	// nothing.
	if fts5IsLocaleValue(pv) {
		text, _, decoded := fts5DecodeLocaleValue(pv)
		if !decoded {
			return Value{}, fmt.Errorf("datatype mismatch")
		}
		pv = text
	}
	// A pattern carrying fts5_insttoken()'s subtype turns on
	// pConfig->bPrefixInsttoken (fts5FilterMethod, fts5_main.c:1505-1506),
	// which changes how a prefix term is looked up in a tokendata=1 index. This
	// engine's query path has no such mode, so the MATCH declines rather than
	// answering as an ordinary query.
	if pv.Subtype == fts5InsttokenSubtype {
		return Value{}, fmt.Errorf("%w: an fts5_insttoken() MATCH pattern", errVDBEUnsupported)
	}
	pattern := valueToText(pv)

	// A MATCH is the ONE answer an external-content table takes from its index
	// (fts5_extcontent.go): a plain scan reads the content table and never
	// consults it. So the index-vs-content verification is charged here, once
	// per snapshot, rather than on every read of such a table.
	indexed, verr := ctx.fts5ExtMatchGuard(ts)
	if verr != nil {
		return Value{}, verr
	}
	if !indexed {
		// A content row the index holds no document for: C fts5's MATCH
		// draws its rowids from the index and never reaches it.
		if x.Not {
			return Value{Typ: Int, I: 1}, nil
		}
		return Value{Typ: Int, I: 0}, nil
	}
	tok, terr := ctx.fts5TokFor(ts)
	if terr != nil {
		return Value{}, terr
	}
	detail, derr := ctx.fts5DetailFor(ts)
	if derr != nil {
		return Value{}, derr
	}
	doc, colNames, derr2 := ctx.buildFts5Doc(ts, tok)
	if derr2 != nil {
		return Value{}, derr2
	}
	node, perr := fts5ParseQuery(pattern, colNames, tok, detail)
	if perr != nil {
		return Value{}, perr
	}
	if singleCol >= 0 {
		// sqlite3Fts5ExprNew's implicit column filter for the "col MATCH q"
		// form goes through sqlite3Fts5ParseSetColset too, so detail=none
		// refuses it exactly as it refuses a written one -- and refuses it
		// AFTER the whole expression has parsed, which is why this check sits
		// here rather than inside fts5ParseQuery.
		if detail == fts5DetailNone {
			return Value{}, errFts5DetailNoneColumn
		}
		node = fts5RestrictColumn(node, singleCol)
	}
	matched := node.eval(doc)
	if x.Not {
		matched = !matched
	}
	if matched {
		return Value{Typ: Int, I: 1}, nil
	}
	return Value{Typ: Int, I: 0}, nil
}

// fts5MatchTarget resolves a MATCH left operand to its fts5 table scope and,
// for the "col MATCH" / "t.col MATCH" forms, the text-column index the query is
// restricted to (-1 for the whole-table "t MATCH" form). ok is false when the
// operand is not an fts5 table or column.
func (ctx *evalCtx) fts5MatchTarget(e Expr) (ts *tableScope, singleCol int, ok bool) {
	ce, isCol := e.(ColumnExpr)
	if !isCol {
		return nil, 0, false
	}
	// Whole-table form: an unqualified identifier equal to the scope's DECLARED
	// TABLE NAME. It is not a table reference at all -- it is an ordinary
	// column reference to the HIDDEN column fts5 declares last:
	// sqlite3Fts5ConfigDeclareVtab (fts5_config.c:765) emits "..., %Q HIDDEN,
	// rank HIDDEN)" with pConfig->zName, the name given to CREATE VIRTUAL
	// TABLE, which no alias ever changes. So an ALIAS does not name it and the
	// real table name still does, both verified against the oracle over
	// "CREATE VIRTUAL TABLE t USING fts5(a,b)":
	//
	//	SELECT rowid FROM t AS x2 WHERE x2 MATCH 'alpha' -> no such column: x2
	//	SELECT rowid FROM t AS x2 WHERE t  MATCH 'alpha' -> the matching rows
	//
	// Keying this on the scope's NAME (which is the alias when there is one)
	// got both backwards: it answered rows for a name C SQLite rejects, and
	// declined the spelling C SQLite answers.
	//
	// Only the WRONG half is fixed here. Requiring BOTH names to match makes an
	// ALIASED scope resolve neither spelling: the alias is refused (correct),
	// and the declared name goes on being declined (a gap, exactly as before).
	// Serving the declared name needs one more edit, in a file this stream does
	// not own -- considerMatchTable (join.go) decides WHICH join level a
	// conjunct is tested at by matching this same operand against ts.name
	// ALONE, so under an alias it finds no dependency at all and the conjunct
	// is tested BEFORE any cursor is open. Measured with both edits applied and
	// then reverted: "SELECT o.id, x2.a FROM o, t AS x2 WHERE t MATCH 'alpha'"
	// answers 1 row with join.go teaching considerMatchTable the declared name
	// too, and ZERO rows (a wrong answer) with only this half.
	//
	// The QUALIFIED spelling of the same hidden column ("x2.t MATCH q") is
	// accepted for an aliased scope too -- it is an ordinary qualified column
	// reference, and it is what the table-valued form rewrites to
	// (fts5_tablefunc.go), which is the only way an ALIASED fts5 table's
	// whole-table MATCH is served here at all.
	for i := range ctx.tables {
		t := &ctx.tables[i]
		declared := t.tableName
		if declared == "" {
			// Every scope built outside the SELECT FROM-clause path (the
			// write path's vtabEvalScope, trigger and UPSERT scopes) leaves
			// tableName empty and carries the real table name in name --
			// see tableScope's own doc comment. None of those can be
			// aliased, so name IS the declared name there.
			declared = t.name
		}
		if !strings.EqualFold(declared, ce.Name) || !ctx.isFts5Scope(t) {
			continue
		}
		if ce.Qualifier == "" && strings.EqualFold(t.name, ce.Name) {
			return t, -1, true
		}
		if ce.Qualifier != "" && strings.EqualFold(t.name, ce.Qualifier) {
			return t, -1, true
		}
	}
	// Column form: resolve the column reference to a scope + column index.
	for i := range ctx.tables {
		t := &ctx.tables[i]
		if ce.Qualifier != "" && !strings.EqualFold(t.name, ce.Qualifier) {
			continue
		}
		for j, c := range t.cols {
			if strings.EqualFold(c.Name, ce.Name) {
				if !c.Hidden && ctx.isFts5Scope(t) {
					if tc := fts5TextColPos(t, j); tc >= 0 {
						return t, tc, true
					}
				}
				return nil, 0, false
			}
		}
	}
	return nil, 0, false
}

// isFts5Scope reports whether tableScope t is backed by an fts5 table. The
// write path marks the scope directly (isFts5); the read path detects it via
// the snapshot pager's schema.
func (ctx *evalCtx) isFts5Scope(t *tableScope) bool {
	if t.isFts5 {
		return true
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	// Ask the pager this scope's rows actually come from, not the one
	// compiling the statement. tableScope.dbIdx exists for exactly this
	// (column_scope.go), and its own doc comment says so -- it just named
	// fts3/fts4 as the only consumer, because fts5 was still asking the wrong
	// pager. An fts5 table's shadow tables live in ITS OWN database, and TEMP
	// is its own database here (temp_store.go is db->aDb[1], reached as an
	// attachedReader), so a TEMP fts5 table is invisible to main's schema:
	// "CREATE VIRTUAL TABLE temp.t1 USING fts5(x)" then
	// "SELECT x FROM t1 WHERE t1 MATCH 'foo'" reported
	// "unable to use function MATCH in the requested context" while the oracle
	// answered the row. Same resolution fts3 already does
	// (fts3_search.go).
	return ctx.pager.forDB(t.dbIdx).isFts5Table(name)
}

// buildFts5Doc tokenizes the current row's fts5 text columns (the non-hidden
// columns of scope t) into an fts5Doc, and returns the text-column names for
// col:-filter resolution.
func (ctx *evalCtx) buildFts5Doc(t *tableScope, tok *fts5Tokenizer) (*fts5Doc, []string, error) {
	// A CONTENTLESS table's row carries no text to tokenize -- every column of
	// it reads NULL, which is what C fts5's xColumn returns there too. Its
	// document comes back out of the index instead (fts5_contentless.go), keyed
	// by the rowid the fts5 row shape carries in slot 0.
	if docs, isCl, cerr := ctx.fts5ContentlessDocsFor(t); isCl {
		var colNames []string
		for _, c := range t.cols {
			if !c.Hidden {
				colNames = append(colNames, c.Name)
			}
		}
		if cerr != nil {
			return nil, colNames, cerr
		}
		if t.offset < 0 || t.offset >= len(ctx.vals) || ctx.vals[t.offset].Typ != Int {
			return nil, colNames, fmt.Errorf("engine: fts5 table %s: a MATCH over a contentless table needs the row's own rowid, which this row source does not carry", t.name)
		}
		d := docs[ctx.vals[t.offset].I]
		return &fts5Doc{cols: d.cols, holes: d.holes}, colNames, nil
	}
	var colTokens [][]string
	var colNames []string
	for j, c := range t.cols {
		if c.Hidden {
			continue
		}
		// An UNINDEXED column contributes NO tokens, so nothing in it can be
		// MATCHed -- but it still occupies its column position, which is what
		// makes a "b:<term>" filter naming it match no row rather than shift
		// onto the next column. Verified against the oracle: over
		// fts5(a, b UNINDEXED) holding ('hello world','foo bar') and
		// ('bar baz','hello'), 'foo' and 'b:bar' select nothing while 'a:bar'
		// selects the second row.
		var toks []string
		if !c.Unindexed {
			var text string
			if idx := t.offset + j; idx >= 0 && idx < len(ctx.vals) {
				text = valueToText(ctx.vals[idx])
			}
			toks = tok.tokenize(text)
		}
		colTokens = append(colTokens, toks)
		colNames = append(colNames, c.Name)
	}
	return &fts5Doc{cols: colTokens}, colNames, nil
}

// fts5ContentlessDocsFor returns the documents of tableScope t, keyed by rowid,
// when t is backed by a CONTENTLESS fts5 table; isCl is false when it is not.
//
// Both paths reach it. The READ path decodes the index once per snapshot
// (fts5_contentless.go). The WRITE path carries the live store, whose token map
// IS the document set there -- and it must be consulted rather than the row's
// values, which are NULL: a DELETE or UPDATE of a contentless table is refused
// only once a row REACHES the store, so its WHERE clause is evaluated first,
// and "DELETE FROM t WHERE t MATCH 'x'" that matched nothing would otherwise
// succeed where C fts5 errors.
func (ctx *evalCtx) fts5ContentlessDocsFor(t *tableScope) (docs map[int64]fts5RowTokens, isCl bool, err error) {
	name := t.tableName
	if name == "" {
		name = t.name
	}
	if t.isFts5 {
		if ctx.db == nil {
			return nil, false, nil
		}
		vm := ctx.db.findVtabMeta(name)
		if vm == nil {
			return nil, false, nil
		}
		st, ok := vm.store.(*fts5Store)
		if !ok || !st.contentless {
			return nil, false, nil
		}
		if st.contentlessErr != nil {
			return nil, true, st.contentlessErr
		}
		return st.tokens, true, nil
	}
	if ctx.pager == nil {
		return nil, false, nil
	}
	sch, ok := ctx.pager.fts5SchemaTok(name)
	if !ok || sch.cl == nil {
		return nil, false, nil
	}
	read, rerr := ctx.pager.fts5ContentlessDocsOf(name)
	if rerr != nil {
		return nil, true, rerr
	}
	return read.tokens, true, nil
}

// fts5TextColPos maps a scope column index (into t.cols) to its position among
// the non-hidden text columns (the fts5Doc column order), or -1.
func fts5TextColPos(t *tableScope, scopeIdx int) int {
	pos := 0
	for j, c := range t.cols {
		if c.Hidden {
			continue
		}
		if j == scopeIdx {
			return pos
		}
		pos++
	}
	return -1
}

// fts5RestrictColumn rewrites a parsed query so every phrase is restricted to
// text column col (intersecting any explicit column filter) -- the semantics of
// the "col MATCH q" form, which confines the whole query to that column.
func fts5RestrictColumn(n fts5Node, col int) fts5Node {
	return fts5ApplyColset(n, []int{col})
}

// fts5ApplyColset is fts5_expr.c's sqlite3Fts5ParseSetColset/fts5ParseSetColset:
// a column filter written in front of a PARENTHESIZED GROUP
// (`expr ::= colset COLON LP expr RP` in fts5parse.y) applies to every phrase
// and NEAR set beneath it, however deeply nested.
//
// A leaf carrying no filter of its own takes this one; a leaf that already has
// one keeps the INTERSECTION (fts5MergeColset), and an empty intersection turns
// that leaf into fts5's FTS5_EOF -- a node that matches nothing, which this
// engine spells as an empty but non-nil cols slice (nil means "every column").
func fts5ApplyColset(n fts5Node, cols []int) fts5Node {
	switch x := n.(type) {
	case fts5OrNode:
		return fts5OrNode{fts5ApplyColset(x.l, cols), fts5ApplyColset(x.r, cols)}
	case fts5AndNode:
		return fts5AndNode{fts5ApplyColset(x.l, cols), fts5ApplyColset(x.r, cols)}
	case fts5NotNode:
		return fts5NotNode{fts5ApplyColset(x.l, cols), fts5ApplyColset(x.r, cols)}
	case fts5NearNode:
		x.cols = fts5IntersectCols(x.cols, cols)
		return x
	case fts5Phrase:
		x.cols = fts5IntersectCols(x.cols, cols)
		return x
	}
	return n
}

// fts5IntersectCols is fts5MergeColset with fts5ParseSetColset's "this leaf has
// no colset yet" case folded in: a nil have means every column, so the answer
// is want itself.
func fts5IntersectCols(have, want []int) []int {
	if have == nil {
		return append([]int(nil), want...)
	}
	out := make([]int, 0, len(have))
	for _, c := range have {
		for _, w := range want {
			if c == w {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
