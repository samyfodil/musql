// This file wires fts5's "rank" hidden column and its bm25()/highlight()/
// snippet() auxiliary functions (fts5_aux.go, fts5_snippet.go) into the
// compiler, as compileFts3Aux/OpFts3Aux wire in fts3's offsets()/matchinfo()/
// snippet(). The rules were pinned against the oracle (gate:
// compat-harness/fts5_aux_test.go).
//
// buildFts5AuxState does one whole-corpus scan -- the query's phrases,
// per-phrase document frequency, row count and average length -- once, at
// compile time (compileSelectScan). That is safe as compileFts3Aux's
// compile-time fts3MatchDocids is: a ReadOnlyPager is an immutable snapshot,
// and a cached Program is only replayed against the same pager with different
// parameter values. That is also why a non-literal MATCH pattern declines: a
// value baked into a cached program cannot read a bound parameter.
//
// The state is cached on the compiler (compiler.fts5Aux) and used by
// compileColumn's "rank" case (compileFts5Rank) and compileFunc's
// fts5AuxFuncName gate (compileFts5Aux).
package engine

import (
	"fmt"
	"strings"
)

// buildFts5AuxState resolves this compile's fts5 auxiliary-function context,
// or (nil, nil) when neither the select list nor ORDER BY uses rank, bm25,
// snippet or highlight (fts5ExprUsesAux) -- the common case, costing nothing.
// scopes is compileSelectScan's resolved FROM. It needs exactly one fts5 table
// with its own top-level AND-conjunct MATCH, and reads the corpus via
// materializeFts5 (the %_content read MATCH answers from).
func (c *compiler) buildFts5AuxState(stmt *SelectStmt, scopes []tableScope) (*fts5AuxState, error) {
	usesAux := false
	for _, sc := range stmt.Columns {
		if !sc.Star && fts5ExprUsesAux(sc.Expr) {
			usesAux = true
			break
		}
	}
	if !usesAux {
		for _, ot := range stmt.OrderBy {
			if fts5ExprUsesAux(ot.Expr) {
				usesAux = true
				break
			}
		}
	}
	if !usesAux {
		return nil, nil
	}

	// Exactly one fts5 table may carry rank/bm25/snippet/highlight. C
	// supports bm25(t1)/bm25(t2) over two MATCHed fts5 tables and resolves
	// a bare "rank" against the one with a MATCH -- but a bare "rank" over
	// two fts5 tables is "ambiguous column name: rank". Reproducing that
	// plus per-table corpus resolution is not done; multi-table declines.
	fts5Idx := -1
	for i := range scopes {
		if c.pager.scopeIsFts5(&scopes[i]) {
			if fts5Idx >= 0 {
				return nil, fmt.Errorf("engine: fts5: rank/bm25 with more than one fts5 table is not supported by this engine")
			}
			fts5Idx = i
		}
	}
	if fts5Idx < 0 {
		// NOT an error: snippet() is fts3's function too (fts3_snippet.go),
		// and fts5ExprUsesAux matches it by NAME alone. Erroring here made
		// every fts3 "SELECT snippet(t) ... MATCH" fail outright -- caught by
		// TestFts3Snippet, which runs in the DEFAULT build where fts5 does not
		// even exist. Claiming nothing lets compileFunc fall through to the
		// fts3 case, and a genuinely fts5-less bm25()/rank goes on declining
		// as the ordinary "unsupported function" it was before.
		return nil, nil
	}
	scope := &scopes[fts5Idx]

	// The table's per-row LOCALES, for fts5_get_locale() (fts5_locale.go).
	// Read here rather than at each of the three states below because it is
	// nil for every table without locale=1 -- which is all but a handful --
	// and a locale=1 table's %_content has to be read exactly once per compile
	// either way.
	scopeTable := scope.tableName
	if scopeTable == "" {
		scopeTable = scope.name
	}
	locales, lerr := c.pager.fts5LocalesOf(scopeTable)
	if lerr != nil {
		return nil, lerr
	}

	var colNames []string
	var textCols []int
	nIndexedCol := 0
	for j, cc := range scope.cols {
		if cc.Hidden {
			continue
		}
		colNames = append(colNames, cc.Name)
		textCols = append(textCols, j)
		if !cc.Unindexed {
			nIndexedCol++
		}
	}

	// Find this table's own top-level MATCH conjunct -- rank/bm25 read the
	// SAME query the row filter itself used, exactly like fts3's aux
	// functions (fts3_search.go's compileFts3Aux) read c.fts3Match.
	probeCtx := &evalCtx{tables: scopes, pager: c.pager}
	// The table's own TOKENIZER, resolved BEFORE the no-MATCH early return
	// below: highlight()/snippet() re-tokenize the row's text to find the
	// boundaries they mark and count, so they need it even when there is no
	// MATCH at all (snippet's unmarked leading window is measured in TOKENS).
	// Resolving it and then not STORING it on the state -- which is what this
	// function did -- made every aux call fall back to the default tokenizer:
	// over "USING fts5(a, tokenize=trigram)", "SELECT highlight(t,0,'[',']')
	// FROM t WHERE t MATCH 'bcd'" returned "abcdef" where C fts5 returns
	// "a[bcd]ef". A wrong answer, not a decline -- the row was selected
	// correctly (MATCH itself always used the right tokenizer) and then
	// rendered with no marker. Gated by compat-harness/fts5_r30_auxtok_test.go.
	tok, terr := probeCtx.fts5TokFor(scope)
	if terr != nil {
		return nil, terr
	}
	var match *MatchExpr
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		me, ok := cj.(MatchExpr)
		if !ok {
			continue
		}
		ts, _, targetOK := probeCtx.fts5MatchTarget(me.X)
		if !targetOK || ts != scope {
			continue
		}
		if me.Not {
			return nil, fmt.Errorf("engine: fts5: rank/bm25 over a NOT MATCH query is not supported by this engine")
		}
		if match != nil {
			return nil, fmt.Errorf("engine: fts5: rank/bm25 with more than one MATCH on the same table is not supported by this engine")
		}
		m := me
		match = &m
	}

	// No MATCH on this table at all is not an error in C: with no WHERE,
	// bm25(t) is -0 for every row, highlight() returns the plain text and
	// snippet() a plain leading window -- what the aux functions already
	// produce from an empty phrase list. "rank" is the exception: NULL, not
	// -0. compileFts5Rank recognizes this as a built state with no phrases,
	// which can only mean no MATCH, since a MATCH parsing to zero phrases
	// declines below.
	if match == nil {
		return &fts5AuxState{scope: scope, scopeName: scope.name, colNames: colNames, textCols: textCols, nCol: len(colNames), tok: tok, locales: locales}, nil
	}

	_, singleCol, _ := probeCtx.fts5MatchTarget(match.X)

	// The MATCH pattern must be a LITERAL: see this file's package comment
	// for why a value baked into a cacheable compiled Program cannot safely
	// read a bound parameter the way a per-execution evaluation could.
	// Real fts5 places no such restriction (verified,
	// TestProbeX_Bm25Param: "SELECT bm25(t) FROM t WHERE t MATCH ?" answers
	// normally, bound to "quick") -- this is a scope limit of this engine's
	// compiler, not a real SQL rejection, so it is declined rather than
	// risking a stale score.
	lit, isLit := match.Pattern.(LiteralExpr)
	if !isLit {
		return nil, fmt.Errorf("engine: fts5: rank/bm25/snippet/highlight over a MATCH with a non-literal query string is not supported by this engine")
	}
	if lit.Val.Typ == Null {
		return nil, fmt.Errorf("engine: fts5: rank/bm25 over a NULL MATCH query is not supported by this engine")
	}
	// The MATCH query goes through that same tokenizer, or a bm25 score would
	// be computed over a token stream the index does not contain -- a wrong
	// score on any table declaring tokenize=ascii/trigram/porter or unicode61
	// with arguments (fts5_tokenizers.go).
	// ...and under the same detail= mode the row filter itself used: a query
	// C fts5 REFUSES over this table must be refused here too, whether it
	// arrives through MATCH or through an aux function reading the same MATCH.
	detail, derr := probeCtx.fts5DetailFor(scope)
	if derr != nil {
		return nil, derr
	}
	node, err := fts5ParseQuery(valueToText(lit.Val), colNames, tok, detail)
	if err != nil {
		return nil, err
	}
	if singleCol >= 0 {
		if detail == fts5DetailNone {
			return nil, errFts5DetailNoneColumn
		}
		node = fts5RestrictColumn(node, singleCol)
	}
	var phrases []fts5Phrase
	fts5CollectPhrases(node, &phrases)
	if len(phrases) == 0 {
		return nil, fmt.Errorf("engine: fts5: rank/bm25 over a phrase-less MATCH query is not supported by this engine")
	}

	// One pass over the whole corpus, read fresh from %_content exactly like
	// a MATCH-less SELECT over this table would be (materializeFts5,
	// fts5_shadow.go) -- row count, total token count, and each phrase's
	// document frequency (mirrors fts5Bm25GetData's xRowCount/
	// xColumnTotalSize/xQueryPhrase).
	name := scope.tableName
	if name == "" {
		name = scope.name
	}
	// An EXTERNAL-CONTENT table's stored averages record can have DRIFTED
	// from a fresh recount of its currently indexed documents
	// (fts5ExtDeleteOrdinary, fts5_extcontent.go) -- a 'delete' command whose
	// given values did not exactly reproduce the row it named moves real
	// fts5's own running totals without moving the content table this
	// engine's corpus walk below reads from. Declined here rather than
	// risking a bm25()/rank score this engine cannot derive: see
	// fts5ExtIsDrifted.
	if sch, isFts5 := c.pager.fts5SchemaTok(name); isFts5 && sch.extContent != "" {
		drifted, derr := c.pager.fts5ExtIsDrifted(name)
		if derr != nil {
			return nil, derr
		}
		if drifted {
			return nil, fmt.Errorf("engine: fts5: rank/bm25 over the external-content table %s is not supported by this engine after a 'delete' command whose given values did not exactly reproduce the row it named: its averages record has drifted from a fresh recount of the content table, and this engine's corpus statistics do not read the averages record directly", name)
		}
	}
	// No FROM item to hand down: an auxiliary function reads the whole corpus,
	// so a nil tvfWhere here is the truth -- this read is not the statement's
	// own MATCH-driven row source and may not take its index-only shortcut.
	_, corpusRows, _, err := c.pager.materializeFts5(FromItem{Table: name}, scope.cols)
	if err != nil {
		return nil, err
	}
	// A CONTENTLESS table's materialized rows are all NULL by construction
	// (fts5_contentless.go: that is what C fts5's xColumn returns for one),
	// so tokenizing them would make every corpus statistic zero -- and an avgdl
	// of 0 turns bm25's b*D/avgdl into +Inf and every score into -0. Its
	// documents come out of the index instead, keyed by the rowid the fts5 row
	// shape carries in slot 0.
	var clDocs map[int64]fts5RowTokens
	var clStats *fts5ContentlessDocs
	// fts5ApiColumnSize's "no way to know" arm (fts5_main.c:2521, and see
	// fts5AuxState.colSizeUnknown): a columnsize=0 table with no content to
	// re-tokenize reports every indexed column's size as -1.
	colSizeUnknown := false
	if sch, isFts5 := c.pager.fts5SchemaTok(name); isFts5 && !sch.columnsize && (sch.cl != nil || sch.contentUnindexed) {
		colSizeUnknown = true
	}
	if sch, isFts5 := c.pager.fts5SchemaTok(name); isFts5 && sch.cl != nil {
		docs, derr := c.pager.fts5ContentlessDocsOf(name)
		if derr != nil {
			return nil, derr
		}
		clDocs = docs.tokens
		// bm25's corpus statistics come from the AVERAGES RECORD in C fts5 --
		// fts5Bm25GetData asks xRowCount and xColumnTotalSize, both of which
		// read sqlite3Fts5IndexGetAverages -- not from a walk of the live rows.
		// The two agree everywhere except on a contentless_delete=1 table, whose
		// averages go on counting a deleted document forever
		// (fts5_contentless.go's fts5ContentlessFlush), so that is the one shape
		// that has to take them from the file. The phrase hit counts below still
		// come from a real query, exactly as xQueryPhrase does.
		if docs.contentlessDelete {
			clStats = docs
		}
	}
	var nRow, totalTokens int64
	docFreq := make([]int64, len(phrases))
	docCols := make([][]string, len(colNames))
	for _, row := range corpusRows {
		nRow++
		if clDocs != nil {
			cols := clDocs[fts5RowidValue(row[0])].cols
			for k := range docCols {
				docCols[k] = nil
				if k < len(cols) {
					docCols[k] = cols[k]
				}
				totalTokens += int64(len(docCols[k]))
			}
			for pi := range phrases {
				if fts5PhraseInAllowedCols(docCols, phrases[pi]) {
					docFreq[pi]++
				}
			}
			continue
		}
		for k, cj := range textCols {
			docCols[k] = tok.tokenize(valueToText(row[cj]))
			totalTokens += int64(len(docCols[k]))
		}
		for pi := range phrases {
			if fts5PhraseInAllowedCols(docCols, phrases[pi]) {
				docFreq[pi]++
			}
		}
	}
	if clStats != nil {
		nRow, totalTokens = clStats.avgRow, 0
		for _, s := range clStats.avgSizes {
			totalTokens += s
		}
	}
	if nRow == 0 {
		return &fts5AuxState{scope: scope, scopeName: scope.name, colNames: colNames, textCols: textCols, nCol: len(colNames), phrases: phrases, nRow: 0, tok: tok, locales: locales, colSizeUnknown: colSizeUnknown, nIndexedCol: nIndexedCol}, nil
	}
	avgdl := float64(totalTokens) / float64(nRow)
	idf := make([]float64, len(phrases))
	for i := range phrases {
		nHit := docFreq[i]
		v := fts5Log((float64(nRow) - float64(nHit) + 0.5) / (float64(nHit) + 0.5))
		if v <= 0.0 {
			v = 1e-06
		}
		idf[i] = v
	}
	return &fts5AuxState{
		scope: scope, scopeName: scope.name, colNames: colNames, textCols: textCols,
		nCol: len(colNames), phrases: phrases, nRow: nRow, avgdl: avgdl, idf: idf,
		tok: tok, locales: locales, colSizeUnknown: colSizeUnknown, nIndexedCol: nIndexedCol,
	}, nil
}

// compileFts5Rank compiles a bare "rank" (or "t.rank") reference; compileColumn
// (vdbe_codegen.go) calls this once it has already matched c.fts5Aux against
// x's qualifier. See buildFts5AuxState's own comment for the NULL-vs-bm25(t)
// split this mirrors.
func (c *compiler) compileFts5Rank() (int, error) {
	if len(c.fts5Aux.phrases) == 0 {
		return c.compileLiteral(Value{Typ: Null}), nil
	}
	fn, argVals, err := c.fts5RankFunction()
	if err != nil {
		return 0, err
	}
	argRegs := make([]int, len(argVals))
	for i, v := range argVals {
		argRegs[i] = c.compileLiteral(v)
	}
	d := c.alloc()
	scopes, cursors := c.gatherRowScopes()
	c.emit(Instruction{Op: OpFts5Aux, P2: d, P4: &fts5AuxCompileInfo{fn: fn, state: c.fts5Aux, argRegs: argRegs, scopes: scopes, cursors: cursors}})
	return d, nil
}

// fts5RankFunction resolves which auxiliary function "rank" calls and with
// which arguments -- fts5FindRankFunction (fts5_main.c). The table's %_config
// 'rank' row names it (parsed here, verbatim); with none it is bm25 with no
// arguments.
//
// The arguments are evaluated as C does, by preparing and stepping "SELECT
// <args>" (fts5_main.c:1211, 1214, 1219, 1227) -- here via selectExprRow. They
// are literal tokens (fts5ConfigSkipLiteral, fts5_config.c:69; ported as
// fts5SkipRankLiteral), which makes compile-time evaluation safe; C still
// compiles them, so they are not RULE #1's sqlite3ValueFromExpr case.
//
// An unknown function name is "no such function: <name>", raised when the
// rank column is first read, as in C.
func (c *compiler) fts5RankFunction() (string, []Value, error) {
	name := c.fts5Aux.scope.tableName
	if name == "" {
		name = c.fts5Aux.scope.name
	}
	cfg, ok := c.pager.fts5ConfigReadOf(name, "rank")
	if !ok {
		return "bm25", nil, nil
	}
	fn, argsText, parsed := fts5ParseRank(valueToText(cfg))
	if !parsed {
		// Only a hand-edited %_config can hold one: fts5SetRank rejects an
		// unparsable rank string rather than storing it.
		return "", nil, fmt.Errorf("engine: fts5: %s's stored rank %q is not of the form \"<function>(<args>)\"", name, valueToText(cfg))
	}
	if !fts5AuxFuncName(fn) {
		return "", nil, fmt.Errorf("engine: no such function: %s", fn)
	}
	var vals []Value
	if argsText != "" {
		sel, perr := ParseSelect("SELECT " + argsText)
		if perr != nil {
			return "", nil, fmt.Errorf("engine: fts5: %s's stored rank arguments %q are not evaluable: %v", name, argsText, perr)
		}
		// ONE compiled statement for the whole list, exactly as
		// fts5FindRankFunction prepares one and reads every column out of its
		// single step. The pager is deliberately NOT passed: C fts5
		// prepares against pConfig->db, but its own grammar has already
		// restricted the text to literals, so there is nothing here that could
		// read a table -- and this runs while compiling a statement that is
		// already reading c.pager.
		row, everr := (*ReadOnlyPager)(nil).selectExprRow(sel.Columns)
		if everr != nil {
			return "", nil, fmt.Errorf("engine: fts5: %s's stored rank arguments %q are not evaluable: %v", name, argsText, everr)
		}
		vals = row
	}
	return strings.ToLower(fn), vals, nil
}

// compileFts5Aux compiles an fts5 auxiliary call (bm25/highlight/snippet). ok
// is false when it cannot be one -- no aux context for this query (c.fts5Aux
// nil, e.g. used only in WHERE) or a first argument that is not a bare
// identifier naming the aux table's scope -- leaving compileFunc to reject the
// name, as compileFts3Aux does. C's errors for the wrong first arguments vary
// ("no such cursor: 0", "no such column", "unable to use function bm25 in the
// requested context"); all are declined here.
func (c *compiler) compileFts5Aux(x FuncExpr) (int, bool, error) {
	if c.fts5Aux == nil {
		return 0, false, nil
	}
	fn := strings.ToLower(x.Name)
	switch fn {
	case "bm25":
		if len(x.Args) < 1 {
			return 0, true, fmt.Errorf("engine: wrong number of arguments to function bm25()")
		}
	case "highlight":
		if len(x.Args) != 4 {
			return 0, true, fmt.Errorf("engine: wrong number of arguments to function highlight()")
		}
	case "snippet":
		if len(x.Args) != 6 {
			return 0, true, fmt.Errorf("engine: wrong number of arguments to function snippet()")
		}
	case "fts5_get_locale":
		// fts5GetLocaleFunction's own arity check counts the arguments AFTER
		// the table (nVal), so its message is about "1" -- but the call as
		// written has two. Verified against the oracle (fts5locale.test
		// 13.1.7/13.1.8): both fts5_get_locale(t) and fts5_get_locale(t,0,0)
		// are "wrong number of arguments to function fts5_get_locale()".
		if len(x.Args) != 2 {
			return 0, true, fmt.Errorf("wrong number of arguments to function fts5_get_locale()")
		}
	default:
		return 0, false, nil
	}
	arg0, isCol := x.Args[0].(ColumnExpr)
	if !isCol || arg0.Schema != "" || arg0.Qualifier != "" || !strings.EqualFold(arg0.Name, c.fts5Aux.scopeName) {
		return 0, false, nil
	}

	base := c.allocN(len(x.Args) - 1)
	for i, a := range x.Args[1:] {
		r, err := c.compileExpr(a)
		if err != nil {
			return 0, true, err
		}
		c.emit(Instruction{Op: OpSCopy, P1: r, P2: base + i})
	}
	argRegs := make([]int, len(x.Args)-1)
	for i := range argRegs {
		argRegs[i] = base + i
	}
	d := c.alloc()
	scopes, cursors := c.gatherRowScopes()
	c.emit(Instruction{Op: OpFts5Aux, P2: d, P4: &fts5AuxCompileInfo{fn: fn, state: c.fts5Aux, argRegs: argRegs, scopes: scopes, cursors: cursors}})
	return d, true, nil
}

// gatherRowScopes copies this compile's table scopes/cursor numbers, exactly
// like compileMatchExpr's own inline copy (vdbe_codegen.go) -- OpFts5Aux needs
// the same "reassemble the current joined row at run time" shape OpMatch/
// OpFts3Aux do (gatherScopesRow, vdbe.go), since bm25/highlight/snippet read
// the fts5 table's own column text, not a single pre-compiled operand.
func (c *compiler) gatherRowScopes() ([]tableScope, []int) {
	scopes := make([]tableScope, len(c.scopes))
	cursors := make([]int, len(c.scopes))
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
		cursors[i] = s.cursor
	}
	return scopes, cursors
}

// fts5AuxCompileInfo is OpFts5Aux's P4 payload: which function (or "rank"),
// the precomputed corpus state (built once at compile time -- see
// buildFts5AuxState's own comment for why that is safe), the trailing
// arguments' already-compiled registers (bm25's weights; highlight's/
// snippet's column+markers -- empty for "rank", which takes none), and the
// scopes/cursors needed to reassemble the current row.
type fts5AuxCompileInfo struct {
	fn      string // "bm25", "highlight" or "snippet"
	state   *fts5AuxState
	argRegs []int
	scopes  []tableScope
	cursors []int
}

// runFts5Aux is OpFts5Aux's runtime body (vdbe.go). ctx must already have
// tables/vals set to the current row (gatherScopesRow); this only adds
// fts5Aux and dispatches. The "rank" COLUMN has no case of its own: it is
// resolved to one of these three at compile time (fts5RankFunction), and is
// never reached with an empty phrases list (compileFts5Rank compiles a plain
// NULL for that case instead), so state.bm25 is always well-formed here.
func runFts5Aux(ctx *evalCtx, info *fts5AuxCompileInfo, args []Value) (Value, error) {
	ctx.fts5Aux = info.state
	switch info.fn {
	case "bm25":
		weights := make([]float64, len(args))
		for i, a := range args {
			weights[i] = valueToFloatLoose(a)
		}
		return info.state.bm25(ctx, weights), nil
	case "highlight":
		return info.state.highlight(ctx, args)
	case "snippet":
		return info.state.snippet(ctx, args)
	case "fts5_get_locale":
		return info.state.getLocale(ctx, args[0])
	}
	return Value{}, fmt.Errorf("engine: internal: unrecognized fts5 aux function %q", info.fn)
}
