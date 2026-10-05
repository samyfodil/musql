// This file implements the fts5 MATCH query grammar and row matching.
// The grammar:
//
//	expr   := or
//	or     := and ( 'OR' and )*
//	and    := prim ( ( 'AND' | 'NOT' | <implicit> ) prim )*
//	prim   := '(' expr ')' | colspec? ( nearset | phrase )
//	nearset := 'NEAR' '(' phrase+ ( ',' N )? ')'
//	colspec := bareword ':' | '{' bareword+ '}' ':'
//	phrase := unit ( '+' unit )*
//	unit   := bareword '*'? | '"' string '"' '*'?
//
// Query terms are tokenized the same way as indexed documents, so they fold
// identically. AND/OR/NOT/NEAR are operators only in uppercase.
package engine

import (
	"fmt"
	"strings"
)

// fts5Doc is a tokenized row: one folded-token list per fts5 text column, in
// declaration order.
type fts5Doc struct {
	cols [][]string
	// holes is a contentless GHOST's tombstone mask: positions that survive
	// a mismatch 'delete' named. They keep their position but match nothing.
	holes [][]bool
}

// holesFor is column c's hole mask, nil when the document has none.
func (d *fts5Doc) holesFor(c int) []bool {
	if d == nil || c < 0 || c >= len(d.holes) {
		return nil
	}
	return d.holes[c]
}

// fts5IsHole reports whether position i of a column is a tombstoned slot.
func fts5IsHole(holes []bool, i int) bool {
	return i < len(holes) && holes[i]
}

// fts5Term is one token of a phrase: its folded text and whether it is a
// prefix ("tok*") term.
type fts5Term struct {
	text   string
	prefix bool
}

// fts5Node is a node of the parsed MATCH expression tree.
type fts5Node interface {
	eval(doc *fts5Doc) bool
}

type fts5OrNode struct{ l, r fts5Node }
type fts5AndNode struct{ l, r fts5Node }

// fts5NotNode is "l NOT r" == l present AND r absent.
type fts5NotNode struct{ l, r fts5Node }

// fts5Phrase matches when its term sequence appears consecutively.
// cols == nil means every column. anchored ("^phrase") means starting at position 0.
type fts5Phrase struct {
	cols     []int
	terms    []fts5Term
	anchored bool
}

// fts5NearNode is "NEAR(phrase1 phrase2 ..., N)": it matches a row where all
// its phrases occur, within a single column, close enough together to satisfy
// the proximity constraint N (default 10). cols is the nearset's column filter
// ("{a b}: NEAR(...)"; nil means every column); the phrases carry no individual
// column filter (fts5's grammar attaches the filter to the whole nearset).
type fts5NearNode struct {
	cols    []int
	nNear   int
	phrases []fts5Phrase
}

func (n fts5OrNode) eval(d *fts5Doc) bool  { return n.l.eval(d) || n.r.eval(d) }
func (n fts5AndNode) eval(d *fts5Doc) bool { return n.l.eval(d) && n.r.eval(d) }
func (n fts5NotNode) eval(d *fts5Doc) bool { return n.l.eval(d) && !n.r.eval(d) }

// eval matches when some single column holds all phrases within the NEAR window.
// A NEAR match is confined to ONE column.
func (n fts5NearNode) eval(d *fts5Doc) bool {
	var colList []int
	if n.cols == nil {
		colList = make([]int, len(d.cols))
		for i := range colList {
			colList[i] = i
		}
	} else {
		colList = n.cols
	}
	nTerm := make([]int, len(n.phrases))
	for i := range n.phrases {
		nTerm[i] = len(n.phrases[i].terms)
	}
	for _, c := range colList {
		if c < 0 || c >= len(d.cols) {
			continue
		}
		occ := make([][]int, len(n.phrases))
		ok := true
		for i := range n.phrases {
			occ[i] = fts5PhraseStarts(d.cols[c], d.holesFor(c), n.phrases[i].terms)
			if len(occ[i]) == 0 {
				ok = false
				break
			}
		}
		if ok && fts5NearMatchColumn(occ, nTerm, n.nNear) {
			return true
		}
	}
	return false
}

// fts5PhraseStarts returns the ascending list of (possibly overlapping) starting
// token positions at which the phrase's term sequence occurs in toks -- the
// positions fts5 records in the phrase's position list (first token of each
// instance).
func fts5PhraseStarts(toks []string, holes []bool, terms []fts5Term) []int {
	m := len(terms)
	if m == 0 {
		return nil
	}
	var out []int
	for start := 0; start+m <= len(toks); start++ {
		ok := true
		for i, t := range terms {
			if fts5IsHole(holes, start+i) {
				ok = false
				break
			}
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
			out = append(out, start)
		}
	}
	return out
}

// fts5NearMatchColumn reports whether the phrases can be positioned within
// the NEAR window: iMax - nTerm[i] - nNear <= pos_i <= iMax for each phrase i.
func fts5NearMatchColumn(occ [][]int, nTerm []int, nNear int) bool {
	k := len(occ)
	idx := make([]int, k)
	pos := make([]int, k)
	for i := 0; i < k; i++ {
		pos[i] = occ[i][0]
	}
	iMax := pos[0]
	for {
		bMatch := true
		for i := 0; i < k; i++ {
			iMin := iMax - nTerm[i] - nNear
			if pos[i] < iMin || pos[i] > iMax {
				bMatch = false
				for pos[i] < iMin {
					idx[i]++
					if idx[i] >= len(occ[i]) {
						return false
					}
					pos[i] = occ[i][idx[i]]
				}
				if pos[i] > iMax {
					iMax = pos[i]
				}
			}
		}
		if bMatch {
			return true
		}
	}
}

func (p fts5Phrase) eval(d *fts5Doc) bool {
	hit := fts5PhraseInColumn
	if p.anchored {
		hit = fts5PhraseAtColumnStart
	}
	if p.cols == nil {
		for c, toks := range d.cols {
			if hit(toks, d.holesFor(c), p.terms) {
				return true
			}
		}
		return false
	}
	for _, c := range p.cols {
		if c >= 0 && c < len(d.cols) && hit(d.cols[c], d.holesFor(c), p.terms) {
			return true
		}
	}
	return false
}

// fts5PhraseAtColumnStart is fts5PhraseInColumn restricted to position 0: the
// "^phrase" form. Only the START is anchored -- the phrase may run on, and the
// column may hold more tokens after it.
func fts5PhraseAtColumnStart(toks []string, holes []bool, terms []fts5Term) bool {
	if len(terms) == 0 || len(terms) > len(toks) {
		return false
	}
	for i, t := range terms {
		if fts5IsHole(holes, i) {
			return false
		}
		if t.prefix {
			if !strings.HasPrefix(toks[i], t.text) {
				return false
			}
		} else if toks[i] != t.text {
			return false
		}
	}
	return true
}

// fts5PhraseInColumn reports whether terms appears as a consecutive run in toks.
func fts5PhraseInColumn(toks []string, holes []bool, terms []fts5Term) bool {
	m := len(terms)
	if m == 0 {
		return false
	}
	for start := 0; start+m <= len(toks); start++ {
		ok := true
		for i, t := range terms {
			if fts5IsHole(holes, start+i) {
				ok = false
				break
			}
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
			return true
		}
	}
	return false
}

// ---- query lexer ----

type fts5TokKind int

const (
	fts5EOF   fts5TokKind = iota
	fts5LP                // (
	fts5RP                // )
	fts5LB                // {
	fts5RB                // }
	fts5Colon             // :
	fts5Plus              // +
	fts5Star              // *
	fts5Comma             // , (only meaningful inside NEAR(...))
	fts5Caret             // ^ (initial-token operator, prefixes a phrase)
	fts5Minus             // - (only legal in front of a colset: fts5parse.y's MINUS productions)
	fts5Str               // "quoted"
	fts5Word              // bareword
)

type fts5Tok struct {
	kind fts5TokKind
	text string
}

// fts5IsBarewordRune is sqlite3Fts5IsBareword (ext/fts5/fts5_buffer.c), which
// decides what a MATCH query's unquoted words may be made of: every non-ASCII
// character, the 52 letters, the 10 digits, '_' and the unicode SUBSTITUTE
// character 0x1A. Everything else -- every other ASCII punctuation mark --
// terminates the word, and since the only ones the grammar has a token for are
// "(){}:+*,-^ and whitespace, anything else is fts5's own
// `fts5: syntax error near "<c>"`.
//
// This engine used to treat every non-token character as part of a bareword,
// which ANSWERED 19 of the 20 queries this rule refuses -- "alpha.beta",
// "alpha!", "a@b" and so on -- where C fts5 raises a syntax error. The C
// works one BYTE at a time ("t & 0x80"), so every byte of a multi-byte UTF-8
// character is a bareword byte; taking the rune is the same rule.
func fts5IsBarewordRune(r rune) bool {
	switch {
	case r >= 0x80:
		return true
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_' || r == 0x1A:
		return true
	}
	return false
}

func fts5LexQuery(s string) ([]fts5Tok, error) {
	var toks []fts5Tok
	rs := []rune(s)
	i := 0
	n := len(rs)
	for i < n {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == '(':
			toks = append(toks, fts5Tok{fts5LP, "("})
			i++
		case r == ')':
			toks = append(toks, fts5Tok{fts5RP, ")"})
			i++
		case r == '{':
			toks = append(toks, fts5Tok{fts5LB, "{"})
			i++
		case r == '}':
			toks = append(toks, fts5Tok{fts5RB, "}"})
			i++
		case r == ':':
			toks = append(toks, fts5Tok{fts5Colon, ":"})
			i++
		case r == '+':
			toks = append(toks, fts5Tok{fts5Plus, "+"})
			i++
		case r == '*':
			toks = append(toks, fts5Tok{fts5Star, "*"})
			i++
		case r == ',':
			toks = append(toks, fts5Tok{fts5Comma, ","})
			i++
		case r == '^':
			toks = append(toks, fts5Tok{fts5Caret, "^"})
			i++
		case r == '-':
			toks = append(toks, fts5Tok{fts5Minus, "-"})
			i++
		case r == '"':
			i++
			start := i
			for i < n && rs[i] != '"' {
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("fts5: unterminated string in MATCH query")
			}
			toks = append(toks, fts5Tok{fts5Str, string(rs[start:i])})
			i++
		default:
			// fts5ExprGetToken's default arm: a word must START with a bareword
			// character, and anything else is a syntax error rather than the
			// start of a term.
			if !fts5IsBarewordRune(r) {
				return nil, fmt.Errorf("fts5: syntax error near \"%c\"", r)
			}
			start := i
			for i < n && fts5IsBarewordRune(rs[i]) {
				i++
			}
			toks = append(toks, fts5Tok{fts5Word, string(rs[start:i])})
		}
	}
	return toks, nil
}

// ---- query parser ----

type fts5Parser struct {
	toks     []fts5Tok
	pos      int
	colNames []string       // fts5 text-column names, for col: filter resolution
	tok      *fts5Tokenizer // the TABLE's tokenizer: a query term is tokenized exactly as the column text was
	// detail is the TABLE's detail= mode. It makes two whole query shapes
	// hard errors in C fts5 rather than empty results (fts5_detail.go),
	// which this engine has to reproduce because it could answer them: it
	// matches from %_content, where it always has full positions.
	detail fts5Detail
}

// checkDetailString is sqlite3Fts5ParseNode's FTS5_STRING arm
// (ext/fts5/fts5_expr.c): under detail!=full a nearset is only legal when it is
// exactly one phrase of exactly one un-anchored term. It runs at every point
// the grammar reduces a `cnearset`, i.e. once per bare phrase and once per
// NEAR(...) group, BEFORE any column filter on it is applied -- which is the
// order that decides which error a query violating both rules reports.
func (p *fts5Parser) checkDetailString(phrases []fts5Phrase) error {
	if p.detail == fts5DetailFull {
		return nil
	}
	first := phrases[0]
	if len(phrases) != 1 || len(first.terms) > 1 || (len(first.terms) > 0 && first.anchored) {
		return fts5DetailPhraseErr(len(phrases))
	}
	return nil
}

// checkDetailColset is sqlite3Fts5ParseSetColset's guard: under detail=none the
// index holds no column information at all, so any column filter is an error.
func (p *fts5Parser) checkDetailColset(hadColspec bool) error {
	if hadColspec && p.detail == fts5DetailNone {
		return errFts5DetailNoneColumn
	}
	return nil
}

func (p *fts5Parser) peek() fts5Tok {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return fts5Tok{fts5EOF, ""}
}

func (p *fts5Parser) peekAt(k int) fts5Tok {
	if p.pos+k < len(p.toks) {
		return p.toks[p.pos+k]
	}
	return fts5Tok{fts5EOF, ""}
}

func (p *fts5Parser) next() fts5Tok {
	t := p.peek()
	p.pos++
	return t
}

// fts5ParseQuery parses a MATCH query string against the given fts5 text-column
// names. It returns a matchable tree, or an error for a syntax error or an
// unsupported construct (which the caller surfaces as a clean decline).
func fts5ParseQuery(q string, colNames []string, tok *fts5Tokenizer, detail fts5Detail) (fts5Node, error) {
	toks, err := fts5LexQuery(q)
	if err != nil {
		return nil, err
	}
	p := &fts5Parser{toks: toks, colNames: colNames, tok: tok, detail: detail}
	if p.peek().kind == fts5EOF {
		return nil, fmt.Errorf("fts5: empty MATCH query")
	}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != fts5EOF {
		return nil, fmt.Errorf("fts5: unexpected %q in MATCH query", p.peek().text)
	}
	return node, nil
}

func fts5IsKeyword(t fts5Tok, kw string) bool {
	return t.kind == fts5Word && t.text == kw
}

func (p *fts5Parser) parseOr() (fts5Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for fts5IsKeyword(p.peek(), "OR") {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = fts5OrNode{left, right}
	}
	return left, nil
}

func (p *fts5Parser) parseAnd() (fts5Node, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case t.kind == fts5EOF || t.kind == fts5RP || fts5IsKeyword(t, "OR"):
			return left, nil
		case fts5IsKeyword(t, "AND"):
			p.next()
			right, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			left = fts5AndNode{left, right}
		case fts5IsKeyword(t, "NOT"):
			p.next()
			right, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			left = fts5NotNode{left, right}
		default:
			// Implicit AND: the next token starts another primary.
			right, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			left = fts5ImplicitAnd(left, right)
		}
	}
}

func (p *fts5Parser) parsePrimary() (fts5Node, error) {
	// A bare leading operator keyword is a syntax error.
	if t := p.peek(); fts5IsKeyword(t, "AND") || fts5IsKeyword(t, "OR") || fts5IsKeyword(t, "NOT") {
		return nil, fmt.Errorf("fts5: syntax error near %q", t.text)
	}

	// Optional column filter. A leading '-' INVERTS it -- fts5parse.y's
	// `colset ::= MINUS LCP colsetlist RCP` and `colset ::= MINUS STRING`,
	// both of which run sqlite3Fts5ParseColsetInvert over the set the
	// un-inverted production would have built. '-' appears in NO other
	// production, so it is only ever legal here.
	var cols []int
	hadColspec := false
	invert := false
	if p.peek().kind == fts5Minus {
		p.next()
		invert = true
	}
	switch {
	case p.peek().kind == fts5LB:
		p.next()
		var names []string
		for p.peek().kind == fts5Word {
			names = append(names, p.next().text)
		}
		if p.peek().kind != fts5RB {
			return nil, fmt.Errorf("fts5: expected '}' in column filter")
		}
		p.next()
		if p.peek().kind != fts5Colon {
			return nil, fmt.Errorf("fts5: expected ':' after column filter")
		}
		p.next()
		if len(names) == 0 {
			return nil, fmt.Errorf("fts5: empty column filter")
		}
		idx, err := p.resolveCols(names)
		if err != nil {
			return nil, err
		}
		cols = idx
		hadColspec = true
	case p.peek().kind == fts5Word && p.peekAt(1).kind == fts5Colon &&
		!fts5IsKeyword(p.peek(), "AND") && !fts5IsKeyword(p.peek(), "OR") && !fts5IsKeyword(p.peek(), "NOT"):
		name := p.next().text
		p.next() // colon
		idx, err := p.resolveCols([]string{name})
		if err != nil {
			return nil, err
		}
		cols = idx
		hadColspec = true
	}
	if invert {
		if !hadColspec {
			// "-" with no colset behind it: no production accepts it.
			return nil, fmt.Errorf("fts5: syntax error near \"-\"")
		}
		cols = fts5InvertColset(cols, len(p.colNames))
	}

	// NEAR(phrase ... , N) proximity set, optionally carrying the column filter
	// parsed above. "NEAR" is special ONLY when followed by '('; everywhere
	// else it is an ordinary bareword term.
	//
	// It used to be a syntax error in phrase position, on the belief that real
	// fts5 refuses it. It does not, and the C says why: fts5ExprGetToken
	// (fts5_expr.c:205-266) recognises exactly THREE keywords -- OR, NOT and
	// AND -- and NEAR is always FTS5_STRING. It becomes an operator only by
	// reducing the grammar rule `nearset ::= STRING LP nearphrases
	// neardist_opt RP` (fts5parse.y:156), which needs the '('.
	//
	// Measured against 3.53.3 over a table holding 'abc def' -- every one of
	// these is ACCEPTED, and the first two are the corpus declines
	// (fts5misc.test's table-valued "x1('abc NEAR \"..\" NEAR def')"):
	//
	//	abc NEAR def            0 rows   -- abc AND near AND def; no "near" token
	//	abc NEAR ".." NEAR def  0 rows
	//	NEAR                    0 rows
	//	NEAR NEAR               0 rows
	//	NEAR(abc def)           1 row    -- the operator
	//
	// The other spellings were already right and stay untouched: near(abc def),
	// Near(abc def) and foo(abc def) all answer 0 rows on both engines, because
	// a bareword that is not exactly "NEAR" parses as a term followed by a
	// parenthesized group rather than as a proximity set.
	if fts5IsKeyword(p.peek(), "NEAR") && p.peekAt(1).kind == fts5LP {
		node, err := p.parseNearSet(cols)
		if err != nil {
			return nil, err
		}
		if near, ok := node.(fts5NearNode); ok {
			if derr := p.checkDetailString(near.phrases); derr != nil {
				return nil, derr
			}
		}
		if derr := p.checkDetailColset(hadColspec); derr != nil {
			return nil, derr
		}
		return node, nil
	}

	if p.peek().kind == fts5LP {
		p.next()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != fts5RP {
			return nil, fmt.Errorf("fts5: expected ')'")
		}
		p.next()
		if hadColspec {
			// fts5parse.y's `expr ::= colset COLON LP expr RP`, whose action is
			// sqlite3Fts5ParseSetColset: the filter pushes down into every leaf
			// of the group, intersecting any filter a leaf already carries.
			if derr := p.checkDetailColset(hadColspec); derr != nil {
				return nil, derr
			}
			node = fts5ApplyColset(node, cols)
		}
		return node, nil
	}

	anchored := false
	if p.peek().kind == fts5Caret {
		p.next()
		anchored = true
	}
	terms, err := p.parsePhrase()
	if err != nil {
		return nil, err
	}
	ph := fts5Phrase{cols: cols, terms: terms, anchored: anchored}
	// `cnearset ::= nearset` / `cnearset ::= colset COLON nearset`: a bare
	// phrase IS a one-phrase nearset, so it takes the same detail!=full check,
	// and it takes it BEFORE the colset one (fts5parse.y runs
	// sqlite3Fts5ParseNode first and sqlite3Fts5ParseSetColset second).
	if derr := p.checkDetailString([]fts5Phrase{ph}); derr != nil {
		return nil, derr
	}
	if derr := p.checkDetailColset(hadColspec); derr != nil {
		return nil, derr
	}
	return ph, nil
}

// parsePhrase parses one phrase: a sequence of units joined by '+'.
func (p *fts5Parser) parsePhrase() ([]fts5Term, error) {
	terms, err := p.parseUnit()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == fts5Plus {
		p.next()
		more, err := p.parseUnit()
		if err != nil {
			return nil, err
		}
		terms = append(terms, more...)
	}
	return terms, nil
}

// An EMPTY PHRASE -- one whose text tokenizes to no terms at all -- is legal in
// C fts5 and matches NO ROW; it is not an error. Under the default tokenizer
// only a quoted all-punctuation string reaches it ('"+++"'), but under trigram
// EVERY query shorter than three characters does, which is why it matters here.
//
// Verified against the oracle, over fts5(x, y) holding 'hello world':
//
//\t'""' '"+++"' '""*' '"+++"*' 'x:"+++"' '^"+++"' '{x y}:"+++"'   no rows
//\t'"+++" AND hello'                                              no rows
//\t'"+++" OR hello'                                               row 1
//\t'hello NOT "+++"'                                              row 1
//\t'"+++" NOT hello'                                              no rows
//\t'"+++" + hello'                                                row 1
//
// -- every one of which falls out of "an empty phrase evaluates false", which
// is what fts5Phrase.eval does (fts5PhraseInColumn returns false for no terms).
//
// TWO shapes do not, because fts5 DROPS an empty phrase rather than ANDing it:
//
//\t'hello "+++"'        row 1   (the empty phrase is dropped, not ANDed)
//\t'NEAR("+++" hello)'  row 1   (same)
//
// This engine used to DECLINE both, because it builds an implicit AND as an
// ordinary AND node and would otherwise have answered "no rows" -- a wrong
// answer. The two functions below port the drop instead.

// fts5DropEmptyPhrase is sqlite3Fts5ParseNearset's discard rule
// (ext/fts5/fts5_expr.c): when a phrase is appended to a nearset that already
// holds one, an incoming ZERO-TERM phrase is freed and the previous last one
// kept, and a previous last one that is zero-term is freed and replaced by the
// incoming phrase. Only ever ONE of the two, and only when nPhrase>0 -- so a
// nearset's FIRST phrase is kept whatever it is, which is why an all-empty
// nearset ends up holding exactly one empty phrase and matches nothing
// ('NEAR(ab cd)' over a trigram table is no rows, verified).
func fts5DropEmptyPhrase(near []fts5Phrase, ph fts5Phrase) []fts5Phrase {
	if len(near) == 0 {
		return append(near, ph)
	}
	if len(ph.terms) == 0 {
		return near
	}
	if len(near[len(near)-1].terms) == 0 {
		near[len(near)-1] = ph
		return near
	}
	return append(near, ph)
}

// fts5ImplicitAnd is sqlite3Fts5ParseImplicitAnd (ext/fts5/fts5_expr.c). An
// FTS5_EOF node -- which is what a nearset holding an empty phrase becomes --
// is dropped rather than joined: if the RIGHT side is one the result is the
// left alone, and if the left's own rightmost leaf is one the right REPLACES
// it. Anything else is an ordinary AND.
//
// "The left's rightmost leaf" is the C's pPrev: it descends one level into an
// FTS5_AND node, which is exactly this engine's left-associated fts5AndNode.
func fts5ImplicitAnd(left, right fts5Node) fts5Node {
	if fts5IsEmptyPhraseNode(right) {
		return left
	}
	if and, ok := left.(fts5AndNode); ok {
		if fts5IsEmptyPhraseNode(and.r) {
			and.r = right
			return and
		}
	} else if fts5IsEmptyPhraseNode(left) {
		return right
	}
	return fts5AndNode{left, right}
}

// fts5IsEmptyPhraseNode reports whether n is the FTS5_EOF node above: a leaf
// -- a bare phrase or a NEAR set -- that holds a phrase with no terms.
// sqlite3Fts5ParseNode marks the whole node EOF as soon as ANY of its phrases
// is empty, and fts5DropEmptyPhrase has already removed every empty phrase a
// nearset could shed, so this only fires on a leaf that is entirely empty.
func fts5IsEmptyPhraseNode(n fts5Node) bool {
	switch x := n.(type) {
	case fts5Phrase:
		return len(x.terms) == 0
	case fts5NearNode:
		for _, ph := range x.phrases {
			if len(ph.terms) == 0 {
				return true
			}
		}
	}
	return false
}

// parseUnit parses one phrase unit -- a bareword or quoted string, optionally
// prefixed ("*") -- and tokenizes it into folded terms.
func (p *fts5Parser) parseUnit() ([]fts5Term, error) {
	t := p.peek()
	var raw string
	switch t.kind {
	case fts5Word:
		p.next()
		raw = t.text
	case fts5Str:
		p.next()
		raw = t.text
	default:
		return nil, fmt.Errorf("fts5: expected a search term, got %q", t.text)
	}
	toks := p.tok.tokenize(raw)
	terms := make([]fts5Term, len(toks))
	for i, tok := range toks {
		terms[i] = fts5Term{text: tok}
	}
	if p.peek().kind == fts5Star {
		// A '*' after a unit that tokenized to NOTHING is not an error: fts5
		// answers '""*' and '"+++"*' with no rows, exactly as it answers the
		// same queries without the '*' (verified).
		p.next()
		if len(terms) > 0 {
			terms[len(terms)-1].prefix = true
		}
	}
	return terms, nil
}

// parseNearSet parses "NEAR '(' phrase+ [ ',' N ] ')'" (the NEAR keyword
// already peeked). cols is the nearset's column filter (nil = every column). N
// defaults to 10 (FTS5_DEFAULT_NEARDIST) when omitted.
func (p *fts5Parser) parseNearSet(cols []int) (fts5Node, error) {
	p.next() // NEAR
	p.next() // '(' (caller verified)
	var phrases []fts5Phrase
	for {
		t := p.peek()
		if t.kind != fts5Word && t.kind != fts5Str {
			break
		}
		terms, err := p.parsePhrase()
		if err != nil {
			return nil, err
		}
		// sqlite3Fts5ParseNearset drops an empty phrase as it appends it, so
		// the nearset the evaluator sees never mixes empty and non-empty ones
		// (fts5DropEmptyPhrase).
		phrases = fts5DropEmptyPhrase(phrases, fts5Phrase{terms: terms})
	}
	if len(phrases) == 0 {
		return nil, fmt.Errorf("fts5: NEAR group with no phrases")
	}
	nNear := 10
	if p.peek().kind == fts5Comma {
		p.next()
		nt := p.peek()
		if nt.kind != fts5Word {
			return nil, fmt.Errorf("fts5: expected integer in NEAR, got %q", nt.text)
		}
		n, ok := fts5ParseNonNegInt(nt.text)
		if !ok {
			return nil, fmt.Errorf("fts5: expected integer, got %q", nt.text)
		}
		p.next()
		nNear = n
	}
	if p.peek().kind != fts5RP {
		return nil, fmt.Errorf("fts5: expected ')' to close NEAR")
	}
	p.next()
	return fts5NearNode{cols: cols, nNear: nNear, phrases: phrases}, nil
}

// fts5ParseNonNegInt parses an all-digits token as a non-negative int. It
// returns ok=false for an empty string, a non-digit character, or overflow.
func fts5ParseNonNegInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 1 << 30, true // clamp; such a window matches any same-column co-occurrence
		}
	}
	return n, true
}

// fts5InvertColset is sqlite3Fts5ParseColsetInvert (ext/fts5/fts5_expr.c):
// every column index the set does NOT hold, in ascending order. An inverted set
// can be EMPTY ("-{a b}:" over a two-column table selects nothing), which is
// not the same as no filter at all -- it matches no row.
func fts5InvertColset(cols []int, nCol int) []int {
	in := make(map[int]bool, len(cols))
	for _, c := range cols {
		in[c] = true
	}
	out := make([]int, 0, nCol)
	for i := 0; i < nCol; i++ {
		if !in[i] {
			out = append(out, i)
		}
	}
	return out
}

func (p *fts5Parser) resolveCols(names []string) ([]int, error) {
	out := make([]int, 0, len(names))
	for _, n := range names {
		found := -1
		for i, c := range p.colNames {
			if strings.EqualFold(c, n) {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("fts5: no such column: %s", n)
		}
		out = append(out, found)
	}
	return out, nil
}
