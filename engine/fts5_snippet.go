// This file implements the fts5 snippet() and highlight() auxiliary functions
// (ext/fts5/fts5_aux.c). See fts5_aux.go for how the per-query match context
// (fts5AuxState) is built and threaded to these methods, and why they are served
// only over a pure-conjunction MATCH query (every phrase surfaces, so the set of
// highlighted instances is unambiguous).
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts5TokenSpan is one token of the original column text: its folded form (for
// phrase matching, identical to the fts5 tokenizer's output) and the [start,end) byte
// offsets of its ORIGINAL text, which highlight/snippet copy verbatim.
type fts5TokenSpan struct {
	tok   string
	start int
	end   int
}

// The tokens-with-offsets themselves come from (*fts5Tok).spans
// (fts5_tokenizers.go), so highlight()/snippet() follow the TABLE'S tokenizer
// rather than assuming the unicode61 default.

// fts5PhraseAllowsCol reports whether phrase p may match in text column col
// (p.cols == nil means every column).
func fts5PhraseAllowsCol(p fts5Phrase, col int) bool {
	if p.cols == nil {
		return true
	}
	for _, c := range p.cols {
		if c == col {
			return true
		}
	}
	return false
}

// fts5MatchedRuns returns the coalesced matched token-index ranges in text column
// col: for every query phrase allowed in col, each occurrence covers the tokens
// [start, start+nTerm-1]; ranges are merged when they overlap or touch-with-
// overlap (fts5's CInstIter merges an instance whose start is <= the current
// range end). The returned runs are sorted, disjoint (a gap of >= 1 token
// between them), and each is [startTok, endTok].
func (s *fts5AuxState) fts5MatchedRuns(col int, toks []string) [][2]int {
	var inst [][2]int
	for _, p := range s.phrases {
		if !fts5PhraseAllowsCol(p, col) {
			continue
		}
		sz := len(p.terms)
		if sz == 0 {
			continue
		}
		for _, start := range fts5PhraseStarts(toks, nil, p.terms) {
			inst = append(inst, [2]int{start, start + sz - 1})
		}
	}
	if len(inst) == 0 {
		return nil
	}
	sort.Slice(inst, func(i, j int) bool {
		if inst[i][0] != inst[j][0] {
			return inst[i][0] < inst[j][0]
		}
		return inst[i][1] < inst[j][1]
	})
	runs := inst[:0:0]
	cur := inst[0]
	for _, in := range inst[1:] {
		if in[0] <= cur[1] { // overlaps/starts within the current range -> merge
			if in[1] > cur[1] {
				cur[1] = in[1]
			}
			continue
		}
		runs = append(runs, cur)
		cur = in
	}
	runs = append(runs, cur)
	return runs
}

// fts5RenderMarked reconstructs the highlight()/snippet() body for one column:
// it walks the column's tokens copying the ORIGINAL text verbatim and inserting
// open/close around each coalesced matched run. A faithful port of
// fts5HighlightCb plus its caller's post-loop "close if still open". runs are
// the coalesced matched token ranges (from the position iBestStart onward, for
// snippet). rangeEnd < 0 means "no window" (highlight, whole column); otherwise
// only tokens in [rangeStart, rangeEnd] are emitted (snippet). It returns the
// produced text and the byte offset up to which zIn was copied (iOff), for the
// caller's trailing handling.
func fts5RenderMarked(zIn string, spans []fts5TokenSpan, runs [][2]int, open, close string, rangeStart, rangeEnd int) (string, int) {
	var out strings.Builder
	iOff := 0
	bOpen := false
	ri := 0
	curStart, curEnd := -1, -1
	if len(runs) > 0 {
		curStart, curEnd = runs[0][0], runs[0][1]
	}
	advance := func() {
		ri++
		if ri < len(runs) {
			curStart, curEnd = runs[ri][0], runs[ri][1]
		} else {
			curStart, curEnd = -1, -1
		}
	}
	for iPos, sp := range spans {
		iStartOff, iEndOff := sp.start, sp.end
		if rangeEnd >= 0 {
			if iPos < rangeStart || iPos > rangeEnd {
				continue
			}
			if rangeStart != 0 && iPos == rangeStart {
				iOff = iStartOff // clip the left edge to the window's first token
			}
		}
		// Close marker: an open highlight, this token is at/after the next run
		// (or no runs remain), and there is original text past what's copied.
		if bOpen && (iPos <= curStart || curStart < 0) && iStartOff > iOff {
			out.WriteString(close)
			bOpen = false
		}
		// Open marker before the first token of a run: copy verbatim text up to
		// it, then the open marker.
		if iPos == curStart && !bOpen {
			out.WriteString(zIn[iOff:iStartOff])
			out.WriteString(open)
			iOff = iStartOff
			bOpen = true
		}
		// Last token of the run: copy the matched span verbatim, advance.
		if iPos == curEnd {
			if !bOpen {
				out.WriteString(open)
				bOpen = true
			}
			out.WriteString(zIn[iOff:iEndOff])
			iOff = iEndOff
			advance()
		}
		// Window's last token (snippet only): flush any open run, then copy up to
		// this token's end so the window closes cleanly.
		if iPos == rangeEnd {
			if bOpen {
				if curStart >= 0 && iPos >= curStart {
					out.WriteString(zIn[iOff:iEndOff])
					iOff = iEndOff
				}
				out.WriteString(close)
				bOpen = false
			}
			out.WriteString(zIn[iOff:iEndOff])
			iOff = iEndOff
		}
	}
	if bOpen {
		out.WriteString(close)
	}
	return out.String(), iOff
}

// rawColumnText returns the current row's ORIGINAL (untokenized) text for text
// column col (0-based among the fts5 text columns).
func (s *fts5AuxState) rawColumnText(ctx *evalCtx, col int) string {
	if col < 0 || col >= len(s.textCols) {
		return ""
	}
	idx := s.scope.offset + s.textCols[col]
	if idx < 0 || idx >= len(ctx.vals) {
		return ""
	}
	return valueToText(ctx.vals[idx])
}

// highlight implements highlight(t, colnum, open, close): the column's original
// text with open/close markers inserted around each matched phrase run. args
// is [colnum, open, close] -- the call's own trailing arguments, already
// evaluated by the VDBE's OpFts5Aux (fts5_vdbe_aux.go), which checks the
// 4-argument arity before calling this, so args is always exactly length 3
// here.
func (s *fts5AuxState) highlight(ctx *evalCtx, args []Value) (Value, error) {
	col := fts5AuxColArgValue(args[0])
	open := valueToText(args[1])
	closeMark := valueToText(args[2])
	// An out-of-range column yields the empty string (fts5's xColumnText returns
	// SQLITE_RANGE, and highlight() returns "").
	if col < 0 || col >= s.nCol {
		return Value{Typ: Text, S: []byte{}}, nil
	}
	// A NULL column yields NULL, exactly as it does for snippet() below:
	// fts5HighlightFunction guards its whole result production on "else if(
	// ctx.zIn )", and sqlite3_value_text of a NULL value is a NULL pointer, so
	// sqlite3_result_* is never called and the result stays NULL. (SQLITE_RANGE
	// is the one case that yields "" -- the out-of-range column above.)
	// Verified against the oracle over fts5(a, b) holding one row with b NULL,
	// one with b '', one with b 12345 and one with b x'00': only the NULL one
	// answers NULL, the other three answer '', '12345' and ''. This is also
	// what a CONTENTLESS table gives for EVERY column, since fts5ApiColumnText
	// returns a NULL pointer for one outright (fts5_main.c).
	colIdx := s.scope.offset + s.textCols[col]
	if colIdx >= 0 && colIdx < len(ctx.vals) && ctx.vals[colIdx].Typ == Null {
		return Value{Typ: Null}, nil
	}
	zIn := s.rawColumnText(ctx, col)
	spans := s.tok.spans(zIn)
	toks := make([]string, len(spans))
	for i, sp := range spans {
		toks[i] = sp.tok
	}
	runs := s.fts5MatchedRuns(col, toks)
	body, iOff := fts5RenderMarked(zIn, spans, runs, open, closeMark, 0, -1)
	return Value{Typ: Text, S: []byte(body + zIn[iOff:])}, nil
}

// fts5SnippetInst is one phrase instance in a column: the phrase's index (into
// s.phrases, for aSeen and phrase size) and its first-token offset.
type fts5SnippetInst struct {
	ip int
	io int
}

// fts5SnippetColData caches one column's tokenization for snippet's best-window
// scan: token spans, the folded tokens, the instance list (sorted by offset then
// phrase index -- fts5's xInst order), the sentence-start token positions, and
// the token count.
type fts5SnippetColData struct {
	spans      []fts5TokenSpan
	toks       []string
	insts      []fts5SnippetInst
	sentStarts []int
	nDoc       int
}

// fts5ColumnData tokenizes column i and derives its snippet scan inputs.
func (s *fts5AuxState) fts5ColumnData(ctx *evalCtx, i int) fts5SnippetColData {
	zIn := s.rawColumnText(ctx, i)
	spans := s.tok.spans(zIn)
	toks := make([]string, len(spans))
	for k, sp := range spans {
		toks[k] = sp.tok
	}
	// Instances: each phrase allowed in column i, at each occurrence offset.
	var insts []fts5SnippetInst
	for ip := range s.phrases {
		p := s.phrases[ip]
		if !fts5PhraseAllowsCol(p, i) {
			continue
		}
		for _, off := range fts5PhraseStarts(toks, nil, p.terms) {
			insts = append(insts, fts5SnippetInst{ip: ip, io: off})
		}
	}
	sort.Slice(insts, func(a, b int) bool {
		if insts[a].io != insts[b].io {
			return insts[a].io < insts[b].io
		}
		return insts[a].ip < insts[b].ip
	})
	return fts5SnippetColData{
		spans:      spans,
		toks:       toks,
		insts:      insts,
		sentStarts: fts5SentenceStarts(zIn, spans),
		nDoc:       len(spans),
	}
}

// fts5SentenceStarts ports fts5SentenceFinderCb: token 0 always starts a
// sentence; a later token starts one iff it is preceded (in the ORIGINAL bytes)
// by at least one ASCII whitespace byte and, before that run, a '.' or ':'.
func fts5SentenceStarts(zIn string, spans []fts5TokenSpan) []int {
	var out []int
	for iPos, sp := range spans {
		if iPos == 0 {
			out = append(out, 0)
			continue
		}
		i := sp.start - 1
		var c byte
		moved := false
		for i >= 0 {
			c = zIn[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				break
			}
			i--
			moved = true
		}
		if moved && (c == '.' || c == ':') {
			out = append(out, iPos)
		}
	}
	return out
}

// fts5SnippetScore ports fts5SnippetScore: over column col's instances, score a
// window of nToken tokens starting at iPos -- +1000 for each distinct phrase
// with an instance in the window, +1 for each repeat. Returns the score and the
// recentred window start (iFirst shifted to centre the covered span, clamped to
// the column).
func (s *fts5AuxState) fts5SnippetScore(cd fts5SnippetColData, iPos, nToken int) (nScore, iAdj int) {
	iEnd := iPos + nToken
	iFirst := -1
	iLast := 0
	seen := make([]bool, len(s.phrases))
	for _, in := range cd.insts {
		if in.io >= iPos && in.io < iEnd {
			if seen[in.ip] {
				nScore += 1
			} else {
				nScore += 1000
			}
			seen[in.ip] = true
			if iFirst < 0 {
				iFirst = in.io
			}
			iLast = in.io + len(s.phrases[in.ip].terms)
		}
	}
	iAdj = iFirst - (nToken-(iLast-iFirst))/2
	if iAdj+nToken > cd.nDoc {
		iAdj = cd.nDoc - nToken
	}
	if iAdj < 0 {
		iAdj = 0
	}
	return nScore, iAdj
}

// snippet implements snippet(t, colnum, open, close, ellipsis, ntoken) -- a
// faithful port of fts5SnippetFunction (best-window selection with sentence-
// boundary bias, leading/trailing ellipsis, and the highlight marker walk).
// args is [colnum, open, close, ellipsis, ntoken] -- the call's own trailing
// arguments, already evaluated (see highlight's identical doc comment above);
// both callers check the 6-argument arity first, so args is always exactly
// length 5 here.
//
// The window-centering formula and the sentence-start bonus (fts5SnippetScore,
// below) were hand-verified against the oracle across a dozen-plus (match
// position, ntoken) combinations spanning the start/middle/end of a document,
// with and without sentence punctuation (compat-harness/zz_probe_aux_test.go's
// TestProbeX_SnippetWindow/TestProbeX_SnippetSentence, deleted after landing)
// and match exactly for every ntoken >= 1 tried, including ntoken values large
// enough that the window clamps to the whole document. ntoken <= 0 is declined
// below rather than guessed: the oracle's own behavior there (a real, non-empty
// window -- not the empty/whole-column results a literal clamp-to-0 would
// produce by the existing formula, traced by hand and confirmed NOT to match)
// was not pinned to a formula this file's ntoken>=1 evidence gives any
// confidence in.
func (s *fts5AuxState) snippet(ctx *evalCtx, args []Value) (Value, error) {
	iCol := fts5AuxColArgValue(args[0])
	open := valueToText(args[1])
	closeMark := valueToText(args[2])
	ellips := valueToText(args[3])
	nToken := int(int64(valueToFloatLoose(args[4]))) // sqlite3_value_int64
	if nToken <= 0 {
		return Value{}, fmt.Errorf("engine: fts5: snippet() with a token count <= 0 is not supported by this engine")
	}
	if nToken > 64 {
		nToken = 64
	}

	iBestCol := 0
	if iCol >= 0 {
		iBestCol = iCol
	}
	iBestStart := 0
	nBestScore := 0
	nColSize := 0

	for i := 0; i < s.nCol; i++ {
		if iCol >= 0 && iCol != i {
			continue
		}
		cd := s.fts5ColumnData(ctx, i)
		for _, in := range cd.insts {
			nScore, iAdj := s.fts5SnippetScore(cd, in.io, nToken)
			if nScore > nBestScore {
				nBestScore, iBestCol, iBestStart, nColSize = nScore, i, iAdj, cd.nDoc
			}
			if len(cd.sentStarts) != 0 && cd.nDoc > nToken {
				jj := 0
				for jj < len(cd.sentStarts)-1 {
					if cd.sentStarts[jj+1] > in.io {
						break
					}
					jj++
				}
				if cd.sentStarts[jj] < in.io {
					nScore2, _ := s.fts5SnippetScore(cd, cd.sentStarts[jj], nToken)
					if cd.sentStarts[jj] == 0 {
						nScore2 += 120
					} else {
						nScore2 += 100
					}
					if nScore2 > nBestScore {
						nBestScore, iBestCol, iBestStart, nColSize = nScore2, i, cd.sentStarts[jj], cd.nDoc
					}
				}
			}
		}
	}

	if iBestCol < 0 || iBestCol >= s.nCol {
		return Value{Typ: Text, S: []byte{}}, nil
	}
	// A NULL best column yields NULL (fts5 leaves zOut unset when zIn is NULL).
	colIdx := s.scope.offset + s.textCols[iBestCol]
	if colIdx >= 0 && colIdx < len(ctx.vals) && ctx.vals[colIdx].Typ == Null {
		return Value{Typ: Null}, nil
	}
	zIn := s.rawColumnText(ctx, iBestCol)
	spans := s.tok.spans(zIn)
	if nColSize == 0 {
		nColSize = len(spans)
	}
	toks := make([]string, len(spans))
	for k, sp := range spans {
		toks[k] = sp.tok
	}
	allRuns := s.fts5MatchedRuns(iBestCol, toks)
	// Advance past matched runs that begin before the window (fts5's CInstIter
	// advance: while iter.iStart >= 0 && iter.iStart < iBestStart).
	var runs [][2]int
	for _, r := range allRuns {
		if r[0] >= iBestStart {
			runs = append(runs, r)
		}
	}
	rangeStart := iBestStart
	rangeEnd := iBestStart + nToken - 1

	var out strings.Builder
	if iBestStart > 0 {
		out.WriteString(ellips)
	}
	body, iOff := fts5RenderMarked(zIn, spans, runs, open, closeMark, rangeStart, rangeEnd)
	out.WriteString(body)
	if rangeEnd >= nColSize-1 {
		out.WriteString(zIn[iOff:])
	} else {
		out.WriteString(ellips)
	}
	return Value{Typ: Text, S: []byte(out.String())}, nil
}

// fts5AuxColArgValue coerces an already-evaluated fts5 aux-function
// column-number argument to an int (sqlite3_value_int semantics: truncate
// toward zero).
func fts5AuxColArgValue(v Value) int {
	switch v.Typ {
	case Int:
		return int(v.I)
	case Float:
		return int(int64(v.F))
	case Null:
		return 0
	default:
		return int(int64(valueToFloatLoose(v)))
	}
}
