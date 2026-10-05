// This file implements the FTS3/FTS4 snippet() auxiliary function.
// snippet() picks a window of the document via a scoring pass over phrase
// position lists: a window covering a phrase no earlier fragment covered scores
// 1000, and every other hit scores 1.
//
// Defaults: zStart="<b>", zEnd="</b>", zEllipsis="<b>...</b>", iCol=-1, nToken=15.
// NULL arguments are errors. nToken is clamped to [-64, 64]; negative means per
// fragment. No MATCH returns empty string; a fragment with SQL NULL column
// returns NULL overall.
//
// Up to four fragments are emitted, trying one, two, ... until all phrases are
// covered or four are used. The best window scores 1000 for first phrase
// occurrences, 1 for others; ties go earliest. Windows end on phrase occurrences
// plus the window at token 0. After choosing, the window is re-centered so
// highlighted terms are in the middle rather than at the right edge.
package engine

import "strings"

// fts3SnippetFragment is fts3_snippet.c's SnippetFragment: one chosen window.
type fts3SnippetFragment struct {
	iCol    int    // column the snippet is extracted from
	iPos    int    // index of the window's first token
	covered uint64 // mask of query phrases this window covers
	hlmask  uint64 // mask of window offsets to highlight
}

// fts3SnippetPhrase is SnippetPhrase. The C holds two raw cursors into the
// phrase's position list; since this engine has already decoded that list into
// a sorted []int, the cursors are INDEXES into it -- and "index == len(pos)" is
// the C's exhausted "pHead == 0" state, including for a phrase with no
// positions in this column at all (its pList is NULL there).
type fts3SnippetPhrase struct {
	nToken int
	pos    []int
	head   int
	tail   int
}

// advance is fts3SnippetAdvance: move *idx to the first position >= iNext, or
// past the end. A cursor already past the end stays there, matching the C's
// "if( pIter )" guard.
func (p *fts3SnippetPhrase) advance(idx *int, iNext int) {
	for *idx < len(p.pos) && p.pos[*idx] < iNext {
		*idx++
	}
}

// fts3SnippetIter is SnippetIter: the candidate-window iterator.
type fts3SnippetIter struct {
	nSnippet int // window length, in tokens
	aPhrase  []fts3SnippetPhrase
	iCurrent int // first token of the current candidate; < 0 before the first
}

// fts3SnippetNoEnd is the 0x7FFFFFFF sentinel fts3SnippetNextCandidate uses to
// mean "no phrase has another occurrence".
const fts3SnippetNoEnd = 0x7FFFFFFF

// next is fts3SnippetNextCandidate, with the C's return value inverted: true
// while a candidate remains.
//
// The FIRST candidate always starts at token 0, even when it scores nothing --
// that is what makes snippet() render the head of the document for a row whose
// match lies in another column.
func (it *fts3SnippetIter) next() bool {
	if it.iCurrent < 0 {
		it.iCurrent = 0
		for i := range it.aPhrase {
			p := &it.aPhrase[i]
			p.advance(&p.head, it.nSnippet)
		}
		return true
	}
	iEnd := fts3SnippetNoEnd
	for i := range it.aPhrase {
		p := &it.aPhrase[i]
		if p.head < len(p.pos) && p.pos[p.head] < iEnd {
			iEnd = p.pos[p.head]
		}
	}
	if iEnd == fts3SnippetNoEnd {
		return false
	}
	// The next candidate is the window ENDING at the earliest remaining
	// occurrence.
	iStart := iEnd - it.nSnippet + 1
	it.iCurrent = iStart
	for i := range it.aPhrase {
		p := &it.aPhrase[i]
		p.advance(&p.head, iEnd+1)
		p.advance(&p.tail, iStart)
	}
	return true
}

// details is fts3SnippetDetails: score the current candidate and work out which
// phrases it covers and which of its token offsets are highlighted.
//
// A phrase's position list holds the position of its LAST token, so a hit at
// window offset k highlights offsets k, k-1, ... k-nToken+1 -- which is the
// "mHighlight |= (mPos>>j)" loop.
func (it *fts3SnippetIter) details(mCovered uint64) (iPos, iScore int, mCover, mHighlight uint64) {
	iStart := it.iCurrent
	for i := range it.aPhrase {
		p := &it.aPhrase[i]
		for k := p.tail; k < len(p.pos); k++ {
			iCsr := p.pos[k]
			if iCsr >= iStart+it.nSnippet || iCsr < iStart {
				break
			}
			mPhrase := uint64(1) << uint(i%64)
			mPos := uint64(1) << uint(iCsr-iStart)
			// 1000 for the first occurrence of a phrase nothing has covered
			// yet, 1 for every other hit.
			if (mCover|mCovered)&mPhrase != 0 {
				iScore++
			} else {
				iScore += 1000
			}
			mCover |= mPhrase
			for j := 0; j < p.nToken && j < it.nSnippet; j++ {
				mHighlight |= mPos >> uint(j)
			}
		}
	}
	return iStart, iScore, mCover, mHighlight
}

// bestSnippet is fts3BestSnippet: the highest-scoring window of nSnippet tokens
// in column iCol, with ties going to the earliest. pmSeen accumulates every
// phrase that occurs in the column at all, which is what tells the caller
// whether another fragment is needed.
//
// The per-document position lists come from the evaluator's own state (the
// same lists offsets() reports), so a phrase zeroed by a failed NEAR cluster
// contributes nothing here either.
func (r *fts3MatchResult) bestSnippet(nSnippet, iCol int, mCovered uint64, pmSeen *uint64) (fts3SnippetFragment, int) {
	it := &fts3SnippetIter{nSnippet: nSnippet, iCurrent: -1, aPhrase: make([]fts3SnippetPhrase, len(r.ev.order))}
	for i, ph := range r.ev.order {
		it.aPhrase[i].nToken = len(ph.phrase.tokens)
		it.aPhrase[i].pos = r.ev.posOf(ph)[iCol]
		if len(it.aPhrase[i].pos) > 0 {
			*pmSeen |= uint64(1) << uint(i%64)
		}
	}
	frag := fts3SnippetFragment{iCol: iCol}
	iBestScore := -1
	for it.next() {
		iPos, iScore, mCover, mHighlight := it.details(mCovered)
		if iScore > iBestScore {
			frag.iPos, frag.hlmask, frag.covered = iPos, mHighlight, mCover
			iBestScore = iScore
		}
	}
	return frag, iBestScore
}

// fts3SnippetMaxFragments is the SizeofArray(aSnippet) bound: C fts3 never
// splits a snippet into more than four fragments.
const fts3SnippetMaxFragments = 4

// snippet renders the snippet() value for one row. colText supplies the row's
// own column values (ok false for an SQL NULL), exactly as for offsets().
func (r *fts3MatchResult) snippet(docid int64, nCol int, zStart, zEnd, zEllipsis string, iCol, nToken int, colText func(i int) (string, bool)) Value {
	if r.root == nil {
		return Value{Typ: Text, S: []byte{}}
	}
	// Re-run this document's own test: the position lists the scoring pass
	// reads are the ones NEAR trimming and NEAR-cluster invalidation leave
	// behind, and those are per-document state.
	r.ev.begin(docid)
	r.ev.testExpr(r.root)

	if nToken < -64 {
		nToken = -64
	}
	if nToken > 64 {
		nToken = 64
	}

	var aSnippet [fts3SnippetMaxFragments]fts3SnippetFragment
	nSnippet, nFToken := 1, 0
	for ; ; nSnippet++ {
		var mCovered, mSeen uint64
		if nToken >= 0 {
			nFToken = (nToken + nSnippet - 1) / nSnippet
		} else {
			nFToken = -nToken
		}
		for iSnip := 0; iSnip < nSnippet; iSnip++ {
			iBestScore := -1
			aSnippet[iSnip] = fts3SnippetFragment{}
			for iRead := 0; iRead < nCol; iRead++ {
				if iCol >= 0 && iRead != iCol {
					continue
				}
				sF, iS := r.bestSnippet(nFToken, iRead, mCovered, &mSeen)
				if iS > iBestScore {
					aSnippet[iSnip], iBestScore = sF, iS
				}
			}
			mCovered |= aSnippet[iSnip].covered
		}
		// Every phrase that occurs in this row is now shown somewhere -- or
		// there is no fragment left to show it in.
		if mSeen == mCovered || nSnippet == fts3SnippetMaxFragments {
			break
		}
	}

	var out fts3SnippetBuf
	for i := 0; i < nSnippet; i++ {
		r.snippetText(&aSnippet[i], i, i == nSnippet-1, nFToken, zStart, zEnd, zEllipsis, &out, colText)
	}
	if !out.any {
		// fts3StringAppend never ran, so sqlite3_result_text() is handed a
		// NULL pointer -- snippet() is NULL, not the empty string.
		return Value{Typ: Null}
	}
	if out.b == nil {
		out.b = []byte{}
	}
	return Value{Typ: Text, S: out.b}
}

// fts3SnippetBuf is fts3_snippet.c's StrBuffer. "any" records whether
// fts3StringAppend was ever CALLED, which is not the same as whether it wrote
// anything: a zero-length append still allocates the buffer there, and that is
// the difference between snippet() returning '' and returning NULL.
type fts3SnippetBuf struct {
	b   []byte
	any bool
}

func (s *fts3SnippetBuf) append(z string) {
	s.any = true
	s.b = append(s.b, z...)
}

// appendZ appends z the way fts3StringAppend does when it is handed nAppend ==
// -1: through strlen(), so an embedded NUL byte TRUNCATES it. That is the case
// for the markers, the ellipsis, and the document tail below; every other
// append passes an explicit length and is unaffected.
func (s *fts3SnippetBuf) appendZ(z string) {
	if i := strings.IndexByte(z, 0); i >= 0 {
		z = z[:i]
	}
	s.append(z)
}

// snippetText is fts3SnippetText: render one fragment, with its tokenizer walk
// turned into an indexed walk over the column's token spans (token position N
// of a column IS the N'th token of its text -- the same identity offsets()
// relies on).
func (r *fts3MatchResult) snippetText(frag *fts3SnippetFragment, iFragment int, isLast bool, nSnippet int, zOpen, zClose, zEllipsis string, out *fts3SnippetBuf, colText func(i int) (string, bool)) {
	zDoc, ok := colText(frag.iCol)
	if !ok {
		// An SQL NULL column: the C returns before appending anything at all.
		return
	}
	spans := r.tok.spans(zDoc)
	iPos, hlmask := frag.iPos, frag.hlmask
	iEnd := 0
	isShiftDone := false
	for iCurrent := 0; ; iCurrent++ {
		if iCurrent >= len(spans) {
			// The last token of the snippet is also the last token of the
			// column: append whatever punctuation follows it. Reached with
			// iEnd == 0 when iPos names a token the column does not have, in
			// which case the WHOLE column is the answer -- verified against
			// the oracle over a column holding "  ...  " (no tokens at all).
			out.appendZ(zDoc[iEnd:])
			return
		}
		iBegin, iFin := spans[iCurrent].start, spans[iCurrent].end
		if iCurrent < iPos {
			continue
		}
		if !isShiftDone {
			iPos, hlmask = fts3SnippetShift(nSnippet, len(spans)-iCurrent, iPos, hlmask)
			isShiftDone = true
			// The leading "..." is needed when this is not the first fragment,
			// or when the fragment does not begin at position 0 of its column.
			if iPos > 0 || iFragment > 0 {
				out.appendZ(zEllipsis)
			} else if iBegin > 0 {
				out.append(zDoc[:iBegin])
			}
			if iCurrent < iPos {
				continue
			}
		}
		if iCurrent >= iPos+nSnippet {
			if isLast {
				out.appendZ(zEllipsis)
			}
			return
		}
		isHighlight := hlmask&(uint64(1)<<uint(iCurrent-iPos)) != 0
		if iCurrent > iPos {
			out.append(zDoc[iEnd:iBegin])
		}
		if isHighlight {
			out.appendZ(zOpen)
		}
		out.append(zDoc[iBegin:iFin])
		if isHighlight {
			out.appendZ(zClose)
		}
		iEnd = iFin
	}
}

// fts3SnippetShift is fts3SnippetShift: slide the window forward so the
// highlighted terms sit in its middle rather than at its right edge, but no
// further than the document has tokens. nAvail is the number of tokens from the
// window's first token to the end of the column -- the C counts them by
// re-tokenizing from that byte offset, which is the same number.
//
// The C's "nShift = (rc==SQLITE_DONE)+iCurrent-nSnippet" over a tokenizer that
// reports positions 0,1,2,... works out to exactly min(nDesired, nAvail-nSnippet).
func fts3SnippetShift(nSnippet, nAvail, iPos int, hlmask uint64) (int, uint64) {
	if hlmask == 0 {
		return iPos, hlmask
	}
	// Every bit of hlmask is < nSnippet by construction (details() only ever
	// sets offsets inside the window), so both scans terminate; they are
	// bounded anyway rather than asserted, because a shift count this engine
	// let run off the end would be a panic.
	nLeft := 0
	for nLeft < 64 && hlmask&(uint64(1)<<uint(nLeft)) == 0 {
		nLeft++
	}
	nRight := 0
	for nRight < nSnippet && nSnippet-1-nRight < 64 && hlmask&(uint64(1)<<uint(nSnippet-1-nRight)) == 0 {
		nRight++
	}
	if nLeft >= 64 || nRight >= nSnippet {
		return iPos, hlmask
	}
	nDesired := (nLeft - nRight) / 2
	if nDesired <= 0 {
		return iPos, hlmask
	}
	nShift := nDesired
	if nAvail-nSnippet < nShift {
		nShift = nAvail - nSnippet
	}
	if nShift > 0 {
		iPos += nShift
		hlmask >>= uint(nShift)
	}
	return iPos, hlmask
}
