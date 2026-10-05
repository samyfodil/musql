// This file implements the FTS3/FTS4 MATCH query language parser.
// Key quirks: AND/OR/NOT are keywords (UPPER-CASE only); column filters work
// only before bare tokens, not phrases; leading '-' is a delimiter, not NOT.
//
//	  tokens but NOT an fts3 table's -- yet inside a QUOTED phrase it is
//	  accepted for both, because getNextString does not consult the flag.
//	  That asymmetry is real, not a transcription slip.
//	- An empty query (an empty string, '   ', '()', '""') is not an error: it
//	  parses to no expression at all and matches no rows.
//
// The parse produces the same TREE SHAPE the C parser builds, including which
// operand each binary operator adopts, because NEAR clusters are evaluated by
// walking that exact shape (see fts3_search.go's near test).
package engine

import (
	"fmt"
	"strings"
)

// fts3 expression node types. The VALUES matter: with parentheses enabled,
// fts3_expr.c's opPrecedence() returns the node type itself, so NEAR(1) binds
// tighter than NOT(2), which binds tighter than AND(3), which binds tighter
// than OR(4).
const (
	fts3qNear = 1 + iota
	fts3qNot
	fts3qAnd
	fts3qOr
	fts3qPhrase
)

// fts3DefaultNear is SQLITE_FTS3_DEFAULT_NEAR_PARAM: the nearness a bare
// "NEAR" (no "/N") means.
const fts3DefaultNear = 10

// fts3MaxExprDepth is SQLITE_FTS3_MAX_EXPR_DEPTH, the depth past which real
// fts3 rejects a query with "FTS expression tree is too large".
const fts3MaxExprDepth = 12

// fts3MaxNest is SQLITE_MAX_EXPR_DEPTH, the bracket nesting C fts3 refuses
// to descend past.
const fts3MaxNest = 1000

// fts3QueryToken is one token of a phrase: the folded term text, whether it
// was written with a trailing '*' (prefix match), and whether it was written
// with a leading '^' (must be the first token of its column).
type fts3QueryToken struct {
	term   string
	prefix bool
	first  bool
}

// fts3QueryPhrase is a phrase node's payload: the token sequence, and the
// column the phrase is restricted to. iColumn == len(columns) means "any
// column" -- that is the C representation too (fts3TermSelect applies a
// column filter only when iColumn < nColumn), and it is what makes the
// "<col> MATCH <query>" form work by simply defaulting every phrase to that
// column.
type fts3QueryPhrase struct {
	iColumn int
	tokens  []fts3QueryToken
}

// fts3Expr is a node of a parsed fts3 MATCH query. It mirrors the C Fts3Expr
// exactly -- including the parent pointer, which the NEAR cluster test walks.
type fts3Expr struct {
	eType  int
	nNear  int
	parent *fts3Expr
	left   *fts3Expr
	right  *fts3Expr
	phrase *fts3QueryPhrase
}

// parse result codes, mirroring the SQLITE_OK / SQLITE_DONE / SQLITE_ERROR
// trio fts3_expr.c's parser threads through every function.
const (
	fts3rcOK = iota
	fts3rcDone
	fts3rcError
)

// fts3Parse is fts3_expr.c's ParseContext.
type fts3Parse struct {
	cols        []string
	iDefaultCol int
	bFts4       bool
	nNest       int
	// tok is the TABLE's tokenizer: a query string is tokenized by the same
	// one the documents were, so "MATCH 'CAFÉ'" finds a unicode61 table's
	// 'cafe' (fts3_tokenizer.go).
	tok *fts3Tokenizer
}

// fts3ParseQuery parses a MATCH query string against a table whose columns are
// cols. iDefaultCol is the column index every phrase defaults to, or len(cols)
// for the whole-table "t MATCH q" form. bFts4 enables the '^' modifier on bare
// tokens. A nil root with a nil error is the EMPTY query, which matches no
// rows.
func fts3ParseQuery(text string, cols []string, bFts4 bool, iDefaultCol int, tok *fts3Tokenizer) (*fts3Expr, error) {
	// fts3.c:3363-3378 (fts3FilterMethod) always reads the query with
	// "zQuery = sqlite3_value_text(pCons)" and hands it to
	// sqlite3Fts3ExprParse with a length of -1, never sqlite3_value_bytes().
	// fts3ExprParseUnbalanced (fts3_expr.c:1010-1012) then does
	// "if (n<0) n = (int)strlen(z)" -- so the ENTIRE structural parse (not
	// just each token's own tokenizer call, which fts3TokenizerInput already
	// truncates) runs only over the bytes up to the query's first NUL byte;
	// anything after is never seen. A MATCH pattern built from a raw BLOB can
	// carry an embedded NUL -- fts3corrupt4.test/fts3corrupt6.test/
	// fts3matchinfo2.test all mine the identical fuzzed x'...' literal, whose
	// bytes after its one 0x00 include an unmatched '"' that this parser
	// would otherwise reject as a syntax error (an unterminated quoted
	// phrase) even though C SQLite's parser never reaches it at all.
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	p := &fts3Parse{cols: cols, iDefaultCol: iDefaultCol, bFts4: bFts4, tok: tok}
	root, _, rc := p.exprParse(text)
	if rc == fts3rcOK && p.nNest != 0 {
		rc = fts3rcError
	}
	if rc != fts3rcOK {
		return nil, fmt.Errorf("malformed MATCH expression: [%s]", text)
	}
	// Real fts3 REBALANCES the AND/OR spine before checking the depth, so a
	// long "a OR b OR c OR ..." chain is fine while a deeply nested one is
	// not. Rebalancing cannot change which rows match (AND and OR are
	// associative and this engine evaluates them as set operations), so only
	// the depth LIMIT has to be reproduced -- and fts3BalancedDepth
	// deliberately over-estimates it, so this rejects a superset of what real
	// fts3 rejects. A statement rejected here is declined at prepare time,
	// never answered differently.
	if root != nil && fts3BalancedDepth(root) > fts3MaxExprDepth {
		return nil, fmt.Errorf("FTS expression tree is too large (maximum depth %d)", fts3MaxExprDepth)
	}
	return root, nil
}

// exprParse is fts3_expr.c's fts3ExprParse: it consumes nodes from the front
// of z until the input runs out or an unmatched ')' is reached, joining
// adjacent phrases with an implicit AND and threading binary operators in by
// precedence.
func (p *fts3Parse) exprParse(z string) (*fts3Expr, int, int) {
	var pRet, pPrev *fts3Expr
	nIn := len(z)
	zIn := 0
	rc := fts3rcOK
	isRequirePhrase := true

	for rc == fts3rcOK {
		var e *fts3Expr
		var nByte int
		e, nByte, rc = p.getNextNode(z[zIn : zIn+nIn])
		if rc == fts3rcOK && e != nil {
			eType := e.eType
			// A node returned by a bracketed sub-parse is an OPERAND even
			// though it is a binary node -- that is what pLeft != 0 marks.
			isPhrase := eType == fts3qPhrase || e.left != nil

			// A binary operator where an operand was required ("AND",
			// "one OR", "NOT one") is a syntax error.
			if !isPhrase && isRequirePhrase {
				return nil, 0, fts3rcError
			}
			if isPhrase && !isRequirePhrase {
				pAnd := &fts3Expr{eType: fts3qAnd}
				fts3InsertBinaryOperator(&pRet, pPrev, pAnd)
				pPrev = pAnd
			}
			// NEAR's operands must both be phrases: "(a OR b) NEAR c" and
			// "a NEAR (b OR c)" are both errors.
			if pPrev != nil && ((eType == fts3qNear && !isPhrase && pPrev.eType != fts3qPhrase) ||
				(eType != fts3qPhrase && isPhrase && pPrev.eType == fts3qNear)) {
				return nil, 0, fts3rcError
			}
			if isPhrase {
				if pRet != nil {
					pPrev.right = e
					e.parent = pPrev
				} else {
					pRet = e
				}
			} else {
				fts3InsertBinaryOperator(&pRet, pPrev, e)
			}
			isRequirePhrase = !isPhrase
			pPrev = e
		}
		nIn -= nByte
		zIn += nByte
	}

	// A trailing binary operator ("one AND") leaves an operand outstanding.
	if rc == fts3rcDone && pRet != nil && isRequirePhrase {
		rc = fts3rcError
	}
	if rc == fts3rcDone {
		rc = fts3rcOK
	}
	if rc != fts3rcOK {
		return nil, len(z) - nIn, rc
	}
	return pRet, len(z) - nIn, fts3rcOK
}

// fts3InsertBinaryOperator is fts3_expr.c's insertBinaryOperator: it splices
// pNew into the tree above the highest ancestor of pPrev that binds no more
// tightly than pNew does.
func fts3InsertBinaryOperator(ppHead **fts3Expr, pPrev, pNew *fts3Expr) {
	pSplit := pPrev
	for pSplit.parent != nil && pSplit.parent.eType <= pNew.eType {
		pSplit = pSplit.parent
	}
	if pSplit.parent != nil {
		pSplit.parent.right = pNew
		pNew.parent = pSplit.parent
	} else {
		*ppHead = pNew
	}
	pNew.left = pSplit
	pSplit.parent = pNew
}

// fts3Keyword is one entry of fts3_expr.c's aKeyword[] table.
type fts3Keyword struct {
	z     string
	eType int
}

// fts3Keywords is aKeyword[], minus the parenOnly flag: this engine only ever
// reproduces the parentheses-enabled grammar, where all four are live.
var fts3Keywords = []fts3Keyword{
	{"OR", fts3qOr},
	{"AND", fts3qAnd},
	{"NOT", fts3qNot},
	{"NEAR", fts3qNear},
}

// getNextNode is fts3_expr.c's getNextNode: one keyword, bracketed group,
// quoted phrase or bare token from the front of z.
func (p *fts3Parse) getNextNode(z string) (*fts3Expr, int, int) {
	n := len(z)
	zi := 0
	for zi < n && fts3IsSpaceByte(z[zi]) {
		zi++
	}
	if zi == n {
		return nil, 0, fts3rcDone
	}

	for _, kw := range fts3Keywords {
		if n-zi < len(kw.z) || z[zi:zi+len(kw.z)] != kw.z {
			continue
		}
		nNear := fts3DefaultNear
		nKey := len(kw.z)
		if kw.eType == fts3qNear {
			if fts3ByteAt(z, zi+4) == '/' && fts3ByteAt(z, zi+5) >= '0' && fts3ByteAt(z, zi+5) <= '9' {
				// sqlite3Fts3ReadInt returns -1 on overflow, which leaves
				// nKey unchanged (nKey += 1 + -1) and nNear at its default;
				// the '/' then fails the terminator test below, so the whole
				// "NEAR/..." is treated as an ordinary token.
				if nRead, val, ok := fts3ReadInt(z[zi+nKey+1:]); ok {
					nKey += 1 + nRead
					nNear = val
					if nNear >= 1000000000 {
						nNear = 1000000000
					}
				}
			}
		}
		// A keyword only counts as one if what follows it ends the token.
		cNext := fts3ByteAt(z, zi+nKey)
		if fts3IsSpaceByte(cNext) || cNext == '"' || cNext == '(' || cNext == ')' || cNext == 0 {
			return &fts3Expr{eType: kw.eType, nNear: nNear}, zi + nKey, fts3rcOK
		}
		// Not a keyword after all ("ORacle"). Fall through.
	}

	// A quoted phrase. fts3 has no escape for an embedded quote, so the
	// phrase simply runs to the next '"' -- and an UNTERMINATED quote is a
	// syntax error.
	if z[zi] == '"' {
		ii := 1
		for zi+ii < n && z[zi+ii] != '"' {
			ii++
		}
		nConsumed := zi + ii + 1
		if zi+ii == n {
			return nil, nConsumed, fts3rcError
		}
		e := p.getNextString(z[zi+1 : zi+ii])
		return e, nConsumed, fts3rcOK
	}

	if z[zi] == '(' {
		p.nNest++
		if p.nNest > fts3MaxNest {
			return nil, 0, fts3rcError
		}
		e, nConsumed, rc := p.exprParse(z[zi+1:])
		return e, zi + 1 + nConsumed, rc
	}
	if z[zi] == ')' {
		p.nNest--
		return nil, zi + 1, fts3rcDone
	}

	// A bare token, possibly with a column filter in front of it. The filter
	// is recognised HERE, i.e. only for a bare token -- never for the quoted
	// phrase or bracketed group handled above.
	iCol := p.iDefaultCol
	iColLen := 0
	for ii, zStr := range p.cols {
		nStr := len(zStr)
		if n-zi > nStr && z[zi+nStr] == ':' && fts3EqualFoldASCII(z[zi:zi+nStr], zStr) {
			iCol = ii
			iColLen = zi + nStr + 1
			break
		}
	}
	e, nConsumed, rc := p.getNextToken(iCol, z[iColLen:])
	return e, nConsumed + iColLen, rc
}

// getNextToken is fts3_expr.c's getNextToken: the first token of z becomes a
// one-token phrase in column iCol.
func (p *fts3Parse) getNextToken(iCol int, z string) (*fts3Expr, int, int) {
	n := len(z)
	spans := p.tok.spans(z)
	if len(spans) == 0 {
		if n == 0 {
			return nil, 0, fts3rcDone
		}
		// No token, but there was input: consume it all, unless a barred
		// character (a quote or bracket the tokenizer would have swallowed)
		// cuts it short.
		if iBarred := fts3FindBarredChar(z, n); iBarred >= 0 {
			return nil, iBarred, fts3rcOK
		}
		return nil, n, fts3rcOK
	}
	s := spans[0]
	// A token may not span a '"', '(' or ')'. If it did, re-tokenize only the
	// text up to that character -- which is what turns 'a:"x y"' and
	// 'a:(x y)' into EMPTY queries rather than column-filtered ones.
	if iBarred := fts3FindBarredChar(z, s.end); iBarred >= 0 {
		return p.getNextToken(iCol, z[:iBarred])
	}
	tok := fts3QueryToken{term: s.term}
	iStart, iEnd := s.start, s.end
	if iEnd < n && z[iEnd] == '*' {
		tok.prefix = true
		iEnd++
	}
	// The legacy '-' NOT is unreachable with parentheses enabled; '^' is
	// FTS4-only here (a quoted phrase accepts it either way -- see
	// getNextString).
	for p.bFts4 && iStart > 0 && z[iStart-1] == '^' {
		tok.first = true
		iStart--
	}
	return &fts3Expr{eType: fts3qPhrase, phrase: &fts3QueryPhrase{
		iColumn: iCol,
		tokens:  []fts3QueryToken{tok},
	}}, iEnd, fts3rcOK
}

// getNextString is fts3_expr.c's getNextString: the WHOLE of z becomes one
// multi-token phrase. Note two differences from getNextToken, both faithful:
// the phrase always takes the DEFAULT column (a quoted phrase can carry no
// column filter), and '^' is honoured regardless of fts3-vs-fts4.
func (p *fts3Parse) getNextString(z string) *fts3Expr {
	ph := &fts3QueryPhrase{iColumn: p.iDefaultCol}
	for _, s := range p.tok.spans(z) {
		ph.tokens = append(ph.tokens, fts3QueryToken{
			term:   s.term,
			prefix: s.end < len(z) && z[s.end] == '*',
			first:  s.start > 0 && z[s.start-1] == '^',
		})
	}
	return &fts3Expr{eType: fts3qPhrase, phrase: ph}
}

// fts3FindBarredChar is fts3_expr.c's findBarredChar: the offset of the first
// character within z[:n] that a token may not span, or -1.
func fts3FindBarredChar(z string, n int) int {
	for i := 0; i < n && i < len(z); i++ {
		if z[i] == '"' || z[i] == '(' || z[i] == ')' {
			return i
		}
	}
	return -1
}

// fts3IsSpaceByte is fts3isspace().
func fts3IsSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// fts3ByteAt reads z[i], returning 0 past the end -- the NUL terminator the C
// code relies on when it peeks one byte past a keyword.
func fts3ByteAt(z string, i int) byte {
	if i < 0 || i >= len(z) {
		return 0
	}
	return z[i]
}

// fts3ReadInt is sqlite3Fts3ReadInt: leading decimal digits, refusing (ok ==
// false) a value past 0x7fffffff exactly as the C function's -1 return does.
func fts3ReadInt(z string) (n int, val int, ok bool) {
	var v uint64
	for n < len(z) && z[n] >= '0' && z[n] <= '9' {
		v = v*10 + uint64(z[n]-'0')
		if v > 0x7fffffff {
			return 0, 0, false
		}
		n++
	}
	return n, int(v), true
}

// fts3EqualFoldASCII is sqlite3_strnicmp()==0 over equal-length strings: ASCII
// case folding only, never Unicode's (strings.EqualFold would fold e.g. K to
// KELVIN SIGN, which C SQLite does not).
func fts3EqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// fts3BalancedDepth over-estimates the depth C fts3 would see AFTER
// fts3ExprBalance rebalances the tree: an AND (or OR) spine of n operands is
// rebuilt as a balanced tree, so it contributes ceil(log2(n)) levels rather
// than n-1. NOT is left alone by the rebalance, and a NEAR cluster is not
// touched at all. Over-estimating is the safe direction: it can only make
// this engine DECLINE a query C fts3 accepts, never accept one it rejects.
func fts3BalancedDepth(e *fts3Expr) int {
	if e == nil {
		return 0
	}
	switch e.eType {
	case fts3qAnd, fts3qOr:
		var leaves []*fts3Expr
		var collect func(n *fts3Expr)
		collect = func(n *fts3Expr) {
			if n != nil && n.eType == e.eType {
				collect(n.left)
				collect(n.right)
				return
			}
			leaves = append(leaves, n)
		}
		collect(e)
		best := 0
		for _, l := range leaves {
			if d := fts3BalancedDepth(l); d > best {
				best = d
			}
		}
		levels := 0
		for 1<<levels < len(leaves) {
			levels++
		}
		return best + levels
	case fts3qPhrase:
		return 1
	default:
		l, r := fts3BalancedDepth(e.left), fts3BalancedDepth(e.right)
		if r > l {
			l = r
		}
		return l + 1
	}
}
