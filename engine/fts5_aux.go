// This file implements fts5's auxiliary-function layer -- the "rank" hidden
// column and the bm25() ranking function (ext/fts5/fts5_aux.c) -- on top of the
// MATCH matcher (fts5_query.go). snippet()/highlight() live in fts5_snippet.go.
// fts5_vdbe_aux.go is what makes any of this REACHABLE: the state built here is
// compiled into the VDBE Program (compileSelectScan, vdbe_scan.go) -- see that
// file's package comment for the compile-time architecture and why the MATCH
// pattern must be a literal.
//
// ARCHITECTURE (how per-match context threads to the aux functions). Real fts5
// exposes an Fts5ExtensionApi to aux functions: xPhraseCount/xQueryPhrase
// (corpus phrase stats), xRowCount/xColumnTotalSize (corpus size), xInstCount/
// xInst (this row's phrase instances), xColumnSize (this row's token count).
// This engine has no inverted index -- it MATERIALIZES an fts5 table's rows and
// re-applies MATCH per row (see vtab_fts5.go) -- so the same information is
// recomputed directly: fts5_vdbe_aux.go's buildFts5AuxState scans the whole
// materialized corpus ONCE (per compile) to produce the query's phrase list,
// per-phrase document frequency, row count and average document length, exactly
// mirroring fts5Bm25GetData; then, per output row, the OpFts5Aux opcode
// tokenizes that row's columns and counts phrase instances, exactly mirroring
// fts5Bm25Function. The resulting bm25 double is computed with the SAME
// operation order C SQLite uses; fts5_log.go's fts5Log stands in for
// log(). Every k1=1.2/b=0.75/floor-at-1e-06 constant, the column-weight
// default-1.0 rule, and the arithmetic itself (D/avgdl per row, IDF per
// phrase from the whole corpus) were hand-derived from raw oracle output --
// e.g. -9.765013054830288611e-07 for a single-phrase, single-occurrence,
// 3-row corpus -- and matched to the last printed digit. compat-harness/
// fts5_aux_test.go's TestFts5Bm25RawPrecision goes further: it reads bm25()'s
// float64 straight off database/sql (not through any string formatting) from
// both engines and compares with Go's == on the bits themselves -- BIT-IDENTICAL
// against the oracle (mattn/go-sqlite3, glibc log()); see fts5_log.go.
//
// PHRASE COLLECTION (fts5CollectPhrases) is NOT limited to a pure conjunction.
// Verified against the oracle (compat-harness/zz_probe_aux_test.go, deleted
// after landing -- see each rule's own citation below): bm25/highlight sum/mark
// EVERY phrase of an OR, AND or NEAR node using that row's own PLAIN occurrence
// count, with no proximity trimming and no OR-branch masking -- i.e. exactly as
// if every phrase were flattened into one bag, regardless of which branch made
// the row match; a repeated phrase ("x OR x") is counted TWICE, not deduped. A
// NOT node is the one real exception: its RIGHT (excluded) side contributes
// NOTHING, for every row, even one that is physically present in a row that
// matched only via an unrelated sibling OR branch (TestProbeX_NotInsideOr:
// "(alpha NOT bravo) OR charlie" over a row containing "bravo charlie" scores
// as charlie-alone, "bravo" contributing zero despite being right there in the
// text) -- fts5's synchronized-scan poslist propagation zeroes a NOT's excluded
// side unconditionally, not just for rows where it would have mattered, so
// fts5CollectPhrases simply never descends into one.
//
// Anything this layer still cannot make byte-exact (more than one fts5 table,
// a MATCH not reachable as a top-level AND conjunct on the SAME table, SQL-level
// NOT MATCH, a non-literal MATCH pattern, a MATCH that parses to zero phrases,
// ...) is DECLINED cleanly -- an error, never a wrong score.
package engine

import (
	"fmt"
	"strings"
)

// bm25 constants from fts5_aux.c (fts5Bm25Function).
const (
	fts5Bm25K1 = float64(1.2)
	fts5Bm25B  = float64(0.75)
)

// fts5AuxState is the per-query fts5 auxiliary-function context (see the file
// doc comment). It is immutable once built.
type fts5AuxState struct {
	scope     *tableScope  // the fts5 table's scope (its offset/cols read the current row)
	scopeName string       // the scope's name/alias, for "t.rank"/"bm25(t)" qualifier matching
	colNames  []string     // text column names, in declaration order (fts5Doc column order)
	textCols  []int        // scope-column index of each text column (parallel to colNames), for reading raw column text
	nCol      int          // len(colNames)
	phrases   []fts5Phrase // the query's phrases, in query (left-to-right) order
	nRow      int64        // xRowCount: total rows in the corpus
	avgdl     float64      // xColumnTotalSize(-1)/xRowCount: average total tokens per row
	idf       []float64    // per-phrase IDF (fts5Bm25GetData's aIDF)
	// tok is the fts5 table's TOKENIZER (nil = the unicode61 default). Both the
	// query and every column text below go through it, so bm25/snippet/
	// highlight follow a tokenize=ascii/trigram table's own token stream.
	tok *fts5Tokenizer
	// locales is what fts5_get_locale() reads: the table's per-row locale
	// strings out of %_content, keyed by rowid and indexed by declared column
	// (fts5_locale.go's fts5LocalesOf). nil for every table that holds none,
	// which the function answers as NULL.
	locales map[int64][]Value
	// colSizeUnknown is fts5ApiColumnSize's second arm (fts5_main.c:2521):
	//
	//	}else if( !pConfig->zContent || eContent==FTS5_CONTENT_UNINDEXED ){
	//	  for(i..) if( abUnindexed[i]==0 ) pCsr->aColumnSize[i] = -1;
	//
	// i.e. on a columnsize=0 table with no content to re-tokenize, every INDEXED
	// column's size is reported as MINUS ONE and every UNINDEXED one keeps its 0.
	// So bm25's D -- xColumnSize(-1), the sum over the columns -- is the NEGATED
	// count of indexed columns, not the row's real token count. nIndexedCol
	// carries that count. Verified by the gate asking the oracle: over
	// fts5columnsize.test 3.2's "(x, y UNINDEXED, z, columnsize=0, content='')"
	// the real scores are -3e-06 where a token-count D gives -1e-06.
	colSizeUnknown bool
	nIndexedCol    int
}

// getLocale implements fts5_get_locale(<table>, iCol) for the current row --
// a port of fts5GetLocaleFunction (fts5_aux.c:750). arg is the already-
// evaluated second argument; the first is the table name the caller matched.
//
// Its three rejections are the C's own, in the C's own wording (the arity one
// is the caller's, since it can be seen before any row is read):
//
//   - the argument is not an INTEGER by sqlite3_value_numeric_type -- which
//     means text is coerced, so '0' passes and '0.0' does not, and an already
//     REAL 0.0 does not either (that call takes bTryForInt=0, so it never
//     folds a real back to an integer). Verified against the oracle by
//     fts5locale.test 13.2.5 through 13.2.7.
//   - iCol outside 0..nCol-1 is SQLITE_RANGE, whose message is
//     "column index out of range".
//   - otherwise the stored locale, as TEXT, or NULL when the row/column has
//     none. A locale=0 table simply has no locales at all, so it answers NULL
//     for every column without reading anything.
func (s *fts5AuxState) getLocale(ctx *evalCtx, arg Value) (Value, error) {
	iCol, isInt := fts5NumericInt(arg)
	if !isInt {
		return Value{}, fmt.Errorf("non-integer argument passed to function fts5_get_locale()")
	}
	if iCol < 0 || iCol >= int64(s.nCol) {
		return Value{}, fmt.Errorf("column index out of range")
	}
	if len(s.locales) == 0 {
		return Value{}, nil
	}
	// The fts5 row shape carries the rowid in slot 0 of the table's scope --
	// the same slot buildFts5Doc reads to key a contentless table's document.
	if s.scope.offset < 0 || s.scope.offset >= len(ctx.vals) || ctx.vals[s.scope.offset].Typ != Int {
		return Value{}, fmt.Errorf("engine: fts5 table %s: fts5_get_locale() needs the row's own rowid, which this row source does not carry", s.scope.name)
	}
	locs := s.locales[ctx.vals[s.scope.offset].I]
	if int(iCol) >= len(locs) {
		return Value{}, nil
	}
	return locs[iCol], nil
}

// fts5NumericInt is sqlite3_value_numeric_type(v)==SQLITE_INTEGER: an integer
// is one, and TEXT that parses ENTIRELY as an integer is one after the
// affinity applyNumericAffinity(pMem,0) applies. A REAL is not (bTryForInt is
// 0 there, so no fold back to integer happens), and neither is a blob -- that
// call only rewrites a value carrying MEM_Str.
func fts5NumericInt(v Value) (int64, bool) {
	switch v.Typ {
	case Int:
		return v.I, true
	case Text:
		if isFloat, i, _, ok := parseFullNumeric(string(v.S)); ok && !isFloat {
			return i, true
		}
	}
	return 0, false
}

// matchesRankQualifier reports whether a "rank" (or "t.rank") reference's
// qualifier names this fts5 scope: an empty qualifier always does; a qualifier
// must equal the scope's name/alias.
func (s *fts5AuxState) matchesRankQualifier(qualifier string) bool {
	return qualifier == "" || strings.EqualFold(qualifier, s.scopeName)
}

// fts5AuxFuncName reports whether name is an fts5 auxiliary function this engine
// implements (routed through OpFts5Aux rather than the ordinary scalar-func
// path). snippet()/highlight() are added by fts5_snippet.go once registered.
//
// fts5_get_locale is one of these and not a plain scalar: sqlite3Fts5AuxInit
// registers it in the SAME table as snippet/highlight/bm25 (fts5_aux.c:806),
// with the same calling convention -- argument 0 names the table and the value
// comes from the current row.
func fts5AuxFuncName(name string) bool {
	switch strings.ToLower(name) {
	case "bm25", "snippet", "highlight", "fts5_get_locale":
		return true
	}
	return false
}

// fts5ExprUsesAux reports whether expression e references the fts5 "rank"
// hidden column or an fts5 auxiliary function -- the gate that decides whether a
// row-mode SELECT needs an fts5AuxState built at all.
func fts5ExprUsesAux(e Expr) bool {
	switch x := e.(type) {
	case ColumnExpr:
		return x.Schema == "" && strings.EqualFold(x.Name, "rank")
	case FuncExpr:
		if fts5AuxFuncName(x.Name) {
			return true
		}
		for _, a := range x.walkArgs() {
			if fts5ExprUsesAux(a) {
				return true
			}
		}
		return false
	case UnaryExpr:
		return fts5ExprUsesAux(x.X)
	case BinaryExpr:
		return fts5ExprUsesAux(x.L) || fts5ExprUsesAux(x.R)
	case IsNullExpr:
		return fts5ExprUsesAux(x.X)
	case CollateExpr:
		return fts5ExprUsesAux(x.X)
	case CastExpr:
		return fts5ExprUsesAux(x.X)
	case BetweenExpr:
		return fts5ExprUsesAux(x.X) || fts5ExprUsesAux(x.Lo) || fts5ExprUsesAux(x.Hi)
	case LikeExpr:
		return fts5ExprUsesAux(x.X) || fts5ExprUsesAux(x.Pattern)
	case GlobExpr:
		return fts5ExprUsesAux(x.X) || fts5ExprUsesAux(x.Pattern)
	case InExpr:
		if fts5ExprUsesAux(x.X) {
			return true
		}
		for _, it := range x.List {
			if fts5ExprUsesAux(it) {
				return true
			}
		}
		return false
	case CaseExpr:
		if x.Base != nil && fts5ExprUsesAux(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if fts5ExprUsesAux(w.When) || fts5ExprUsesAux(w.Then) {
				return true
			}
		}
		return x.Else != nil && fts5ExprUsesAux(x.Else)
	}
	return false
}

// fts5CollectPhrases appends every phrase leaf of a MATCH tree to out, in
// left-to-right (query) order -- the order fts5 assigns phrase indices
// (xPhraseCount/xInst), and the order aFreq/idf below are indexed by. See this
// file's package comment for the oracle evidence behind each connective's rule:
// AND/OR/NEAR all flatten fully (every phrase counted with its own PLAIN
// per-row occurrence count, regardless of which branch made the row match; a
// phrase repeated across branches is collected -- and so counted -- more than
// once); a NOT's excluded (right) side is never descended into at all, since
// its phrases contribute nothing to any row that could possibly reach here. A
// NEAR node's phrases carry no individual column filter of their own (fts5's
// grammar attaches it to the whole nearset, fts5NearNode's doc comment) --
// x.cols is copied onto each one here so fts5PhraseInAllowedCols/
// fts5CountPhraseInColumn (which only ever look at a phrase's OWN cols) see it.
func fts5CollectPhrases(n fts5Node, out *[]fts5Phrase) {
	switch x := n.(type) {
	case fts5AndNode:
		fts5CollectPhrases(x.l, out)
		fts5CollectPhrases(x.r, out)
	case fts5OrNode:
		fts5CollectPhrases(x.l, out)
		fts5CollectPhrases(x.r, out)
	case fts5NotNode:
		fts5CollectPhrases(x.l, out)
	case fts5NearNode:
		for _, p := range x.phrases {
			if x.cols != nil {
				p.cols = x.cols
			}
			*out = append(*out, p)
		}
	case fts5Phrase:
		*out = append(*out, x)
	}
}

// fts5CountPhraseInColumn returns the number of (possibly overlapping) starting
// positions in toks at which the phrase's term sequence occurs -- one fts5
// "instance" per start, exactly as fts5's position lists record them.
func fts5CountPhraseInColumn(toks []string, terms []fts5Term) int {
	m := len(terms)
	if m == 0 {
		return 0
	}
	cnt := 0
	for start := 0; start+m <= len(toks); start++ {
		ok := true
		for i, t := range terms {
			if t.prefix {
				if !strings.HasPrefix(toks[start+i], t.text) {
					ok = false
					break
				}
			} else if toks[start+i] != t.text {
				ok = false
				break
			}
		}
		if ok {
			cnt++
		}
	}
	return cnt
}

// fts5PhraseInAllowedCols reports whether phrase p occurs at least once in any
// of its allowed columns of the tokenized document cols (p.cols == nil means
// every column) -- the per-row predicate xQueryPhrase's row callback counts.
func fts5PhraseInAllowedCols(cols [][]string, p fts5Phrase) bool {
	if p.cols == nil {
		for _, toks := range cols {
			if fts5PhraseInColumn(toks, nil, p.terms) {
				return true
			}
		}
		return false
	}
	for _, c := range p.cols {
		if c >= 0 && c < len(cols) && fts5PhraseInColumn(cols[c], nil, p.terms) {
			return true
		}
	}
	return false
}

// scopeIsFts5 reports whether scope t is backed by an fts5 table (write path
// marks it; read path detects it via the snapshot schema).
func (p *ReadOnlyPager) scopeIsFts5(t *tableScope) bool {
	if t.isFts5 {
		return true
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	return p.isFts5Table(name)
}

// bm25 computes the fts5 bm25 score for the current row (ctx's fts5 columns),
// a byte-for-byte port of fts5Bm25Function: for each phrase, aFreq is the
// column-weighted instance count in the row; D is the row's total token count;
// the score sums aIDF[i]*(aFreq*(k1+1))/(aFreq + k1*(1-b + b*D/avgdl)) and is
// negated. weights[c] is column c's weight (default 1.0 for c >= len(weights)).
func (s *fts5AuxState) bm25(ctx *evalCtx, weights []float64) Value {
	doc, _, derr := ctx.buildFts5Doc(s.scope, s.tok)
	if derr != nil || doc == nil {
		// A row whose document cannot be built has no score; the MATCH that
		// selected it would have raised the same error first (evalMatch), so
		// this is unreachable rather than a silent zero.
		return Value{Typ: Null}
	}
	weight := func(c int) float64 {
		if c < len(weights) {
			return weights[c]
		}
		return 1.0
	}
	// aFreq[i] is phrase i's column-weighted instance count in this row. The
	// query is a pure conjunction (buildFts5AuxState declined otherwise), so
	// every phrase is present in every matched row and every phrase's position
	// list surfaces to the auxiliary API -- there is no boolean-tree suppression
	// to model, and each phrase's full occurrence count is exactly xInst's.
	aFreq := make([]float64, len(s.phrases))
	for pi := range s.phrases {
		p := s.phrases[pi]
		if p.cols == nil {
			for c := 0; c < len(doc.cols); c++ {
				n := fts5CountPhraseInColumn(doc.cols[c], p.terms)
				for k := 0; k < n; k++ {
					aFreq[pi] += weight(c)
				}
			}
		} else {
			for _, c := range p.cols {
				if c < 0 || c >= len(doc.cols) {
					continue
				}
				n := fts5CountPhraseInColumn(doc.cols[c], p.terms)
				for k := 0; k < n; k++ {
					aFreq[pi] += weight(c)
				}
			}
		}
	}
	D := 0.0
	if s.colSizeUnknown {
		D = -float64(s.nIndexedCol)
	} else {
		for _, toks := range doc.cols {
			D += float64(len(toks))
		}
	}
	k1 := fts5Bm25K1
	b := fts5Bm25B
	score := 0.0
	for i := range s.phrases {
		score = score + s.idf[i]*(aFreq[i]*(k1+1.0)/(aFreq[i]+k1*(1.0-b+b*D/s.avgdl)))
	}
	return Value{Typ: Float, F: -1.0 * score}
}
