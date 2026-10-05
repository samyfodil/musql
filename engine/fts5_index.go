// This file encodes an fts5 table's inverted index into the exact bytes C fts5
// reads: the %_data and %_idx shadow tables. Unlike %_content rows, the index
// is a private binary format C fts5 refuses or misreads if a field is wrong,
// so it decides whether an exported database interchanges.
// compat-harness/fts5_diff_test.go gates it in both directions.
//
// # %_data rowids
//
// id 1 is the averages record, id 10 the structure record, and every other id a
// segment page at a bit-packed address (fts5_index.c's FTS5_DATA_ID_B=16,
// DLI_B=1, HEIGHT_B=5, PAGE_B=31):
//
//	segid<<37 | dlidx<<36 | height<<31 | pgno
//
// so segment 1's first leaf is id 137438953473 == 2^37+1.
//
// # The averages record
//
//	varint nRow, then one varint per column: that column's total token count
//
// e.g. one row of 'hello world' / 'foo bar' is x'010202'. A fresh table's is an
// empty blob and an emptied one's explicit zeros; both read as zero, and each
// is written where C writes it.
//
// # The structure record
//
//	4-byte cookie (0), varint nLevel, varint nSegment, varint nWriteCounter,
//	then per level: varint nMerge, varint nSeg, then per segment:
//	varint iSegid, varint pgnoFirst, varint pgnoLast
//
// The empty table's is 7 zero bytes; one row makes x'000000000101010001010101'.
// A contentless_delete=1 table writes V2 instead (FTS5_STRUCTURE_V2 after the
// cookie, five more varints per segment; see fts5_contentless.go and
// fts5_decode.go).
//
// This engine writes one segment holding the whole index, rebuilt from every
// live row on each write -- what C's "optimize" produces, and legal for it to
// read. C instead appends a segment per flush and merges later; the two agree
// on what the index means, not on how many segments it took.
//
// ponytail: rebuild-all is O(index) per write statement. The upgrade path is
// appending a level-0 segment per statement and merging, i.e. fts5's own
// scheduler -- worth doing only once a workload measures it.
//
// # A leaf page
//
//	u16 offset of the first rowid on the page, or 0 if a term precedes it
//	u16 szLeaf: offset of the page footer (== page length when the footer is
//	            empty)
//	then, repeated: a term record followed by its doclist
//	then the footer: the term offset array
//
// A term record is "varint nTerm, term bytes" for a page's first term and
// "varint nPrefix, varint nSuffix, suffix bytes" after that, prefix-compressed
// against the previous term on the page. Every term starts with a term-space
// byte: '0' (FTS5_MAIN_PREFIX) for the main index, '1' and up for each prefix=
// index, so one segment holds them all in one sorted stream. The footer holds
// one varint per term: the first absolute, the rest deltas.
//
// A doclist entry is "varint rowid, varint nPos*2+bDel, poslist". The rowid is
// absolute when first of its term or of its page, else a delta. A position is
// "(iPos-iPrev)+2", preceded by "0x01, varint iCol" when the column changes
// (iPrev resets). bDel is never set: a rebuilt index omits deleted rows.
//
// That is detail=full. The other modes change only what follows the rowid
// (sqlite3Fts5HashWrite, fts5HashAddPoslistSize in fts5_hash.c):
//
//	detail=none     nothing follows the rowid for a live entry (no size
//	                varint; only a delete marker writes anything), so the
//	                doclist is a bare rowid list.
//	detail=columns  the size varint sizes a COLUMN LIST: one varint per
//	                column the term occurs in, (iCol - iPrevCol + 2) from 0,
//	                no 0x01 escape; tokens arrive column-major, so each
//	                column appears once. fts5ApiPhraseNextColumn reads it
//	                back with "*piCol += (iIncr-2)".
//
// # Page splitting and %_idx
//
// A leaf is flushed at pgsz (4050 by default, FTS5_DEFAULT_PAGE_SIZE, or the
// table's "pgsz") -- see appendDoclist for fts5FlushOneHash's exact rules. A
// page can end past pgsz, since a rowid is appended without a flush test.
//
// %_idx is the segment's b-tree: one row per leaf page that starts a term,
// holding the shortest prefix of that term sorting above the previous page's
// last term, with pgno == (leaf pgno << 1) | hasDoclistIndex. e.g. a two-page
// segment leaves (empty term, 2) and ("0word0364", 4).
//
// A doclist index (the dlidx<<36 space) is never written, so every %_idx pgno's
// low bit is 0. It only accelerates seeks into a multi-page doclist; the flag
// tells C whether to look for one (TestFts5FileInterchange covers it).
package engine

import (
	"fmt"
	"maps"
	"sort"
)

const (
	// fts5PageSize is FTS5_DEFAULT_PAGE_SIZE, the leaf size fts5 targets.
	fts5PageSize = 4050
	// fts5MainPrefix is FTS5_MAIN_PREFIX: the byte every term of the main
	// (non-"prefix=") index carries. Verified: 'bar' is stored as "0bar". A
	// prefix= index's terms carry this byte plus one plus that index's ordinal
	// in the declared list, which is what keeps the term spaces separate and
	// ordered inside one segment (fts5BuildIndex).
	fts5MainPrefix = '0'
	// fts5CurrentVersion is the value of %_config's "version" row.
	fts5CurrentVersion = 4

	fts5AveragesRowid  = 1
	fts5StructureRowid = 10

	fts5DataPageB   = 31
	fts5DataHeightB = 5
	fts5DataDliB    = 1
)

// fts5SegmentRowid is FTS5_SEGMENT_ROWID: the %_data id of leaf page pgno of
// segment segid (height and the doclist-index flag are always 0 here).
func fts5SegmentRowid(segid, pgno int) int64 {
	return int64(segid)<<(fts5DataPageB+fts5DataHeightB+fts5DataDliB) | int64(pgno)
}

// fts5IndexDoc is one document as the index sees it: its rowid and, per
// column, its tokens (a token's position is its index).
type fts5IndexDoc struct {
	rowid int64
	cols  [][]string
	// holes marks, per column and position, a token a mismatched contentless
	// 'delete' tombstoned (fts5ContentlessGhost): it keeps its slot, so every
	// surviving token keeps its real position, and it emits no posting. nil
	// for every ordinary document. It is a MASK rather than an in-band marker
	// because an empty token string is a real token: unicode61 with
	// categories that make combining marks token characters emits one when
	// diacritic folding removes the mark, and C fts5 indexes it.
	holes [][]bool
}

// fts5IdxRow is one row of the %_idx shadow table.
type fts5IdxRow struct {
	segid int64
	term  []byte
	pgno  int64
}

// fts5IndexImage is the encoded index: the %_data blocks by id, the %_idx
// rows, and the per-column token totals the averages record needs.
type fts5IndexImage struct {
	data      map[int64][]byte
	idx       []fts5IdxRow
	colTotals []int64
	nRow      int64
}

// fts5Posting is one document's appearance of a term: the rowid and the
// already-encoded position list.
type fts5Posting struct {
	rowid   int64
	poslist []byte
}

// fts5MaxLeafSize is the largest leaf page the format can address: both header
// fields are 16-bit page offsets, so a page at or past 65536 bytes could not be
// described at all. A single token longer than a page is the only way to reach
// it, and this engine DECLINES the write rather than emit a page C SQLite
// would decode from the wrong offset.
const fts5MaxLeafSize = 0xffff

// fts5PrefixByteLen returns how many bytes of tok its first k characters
// occupy; ok is false when tok has fewer than k characters. It counts
// characters as the tokenizer advances (fts5NextRune) and keeps the original
// bytes: the ascii tokenizer treats every byte >= 0x80 as a token character
// without decoding, and a []rune round-trip would turn those into U+FFFD.
func fts5PrefixByteLen(tok string, k int) (int, bool) {
	b := []byte(tok)
	n := 0
	for i := 0; i < k; i++ {
		if n >= len(b) {
			return 0, false
		}
		_, sz := fts5NextRune(b, n)
		n += sz
	}
	return n, true
}

// fts5BuildIndex encodes docs (which must be in ascending rowid order) into
// the single-segment index image described in this file's comment. nCol is the
// table's column count, prefixes its prefix= lengths in declaration order,
// pgsz its leaf page budget -- fts5PageSize unless the table's %_config says
// otherwise (fts5_config.go's fts5ConfigPgsz) -- detail the table's detail=
// mode, which decides the doclist shape (fts5_detail.go), and v2 the origin
// state of a contentless_delete=1 table (nil for every other kind).
func fts5BuildIndex(docs []fts5IndexDoc, nCol int, prefixes []int, pgsz int, detail fts5Detail, v2 *fts5StructV2) (*fts5IndexImage, error) {
	img := &fts5IndexImage{data: map[int64][]byte{}, colTotals: make([]int64, nCol), nRow: int64(len(docs))}

	// Invert: term -> postings, in document order (which is rowid order, so
	// each term's doclist comes out ascending without a second sort). A term's
	// map key is the FULL on-disk term, leading term-space byte included, so
	// the sort below orders the main index ('0...') ahead of prefix index 1
	// ('1...') ahead of prefix index 2, exactly as the format requires.
	postings := map[string][]fts5Posting{}
	for _, d := range docs {
		// perTerm collects each term's (col, pos) pairs for this document, in
		// the order fts5's own tokenizer would have emitted them: column by
		// column, position by position.
		var terms []string
		perTerm := map[string][]int64{}
		add := func(term string, at int64) {
			if _, seen := perTerm[term]; !seen {
				terms = append(terms, term)
			}
			perTerm[term] = append(perTerm[term], at)
		}
		for col, toks := range d.cols {
			if col < nCol {
				img.colTotals[col] += int64(len(toks))
			}
			for pos, tok := range toks {
				// A hole (fts5IndexDoc.holes) keeps its position and emits
				// nothing.
				if d.holes != nil && col < len(d.holes) && pos < len(d.holes[col]) && d.holes[col][pos] {
					continue
				}
				at := int64(col)<<32 | int64(pos)
				add(string(rune(fts5MainPrefix))+tok, at)
				if len(prefixes) == 0 {
					continue
				}
				// A PREFIX INDEX is the same postings keyed by a truncated
				// token, in its own term space. Verified against the oracle:
				// the leading byte is FTS5_MAIN_PREFIX plus one plus the
				// prefix's ORDINAL IN THE DECLARED LIST (prefix='2 3' writes
				// '1ab'/'2abc' where prefix='3 2' writes '1abc'/'2ab'); the
				// length counts CHARACTERS, not bytes ('日本語' under prefix=2
				// is x'31E697A5E69CAC'); a token SHORTER than the length
				// contributes nothing while one of exactly that length
				// contributes its whole self; and the truncated postings MERGE
				// -- 'abx aby abz' leaves one '1ab' whose poslist is positions
				// 0, 1 and 2.
				for pi, k := range prefixes {
					nb, ok := fts5PrefixByteLen(tok, k)
					if !ok {
						continue
					}
					pt := make([]byte, 0, 1+nb)
					pt = append(pt, byte(fts5MainPrefix+1+pi))
					pt = append(pt, tok[:nb]...)
					add(string(pt), at)
				}
			}
		}
		for _, t := range terms {
			postings[t] = append(postings[t], fts5Posting{rowid: d.rowid, poslist: fts5EncodeDoclistTail(perTerm[t], detail)})
		}
	}
	if len(postings) == 0 {
		img.data[fts5StructureRowid] = fts5EncodeStructure(1, 0, 0, v2)
		return img, nil
	}

	terms := make([]string, 0, len(postings))
	for t := range postings {
		terms = append(terms, t)
	}
	sort.Strings(terms)

	w := newFts5SegWriter(1, pgsz)
	for _, t := range terms {
		w.appendTerm([]byte(t))
		w.appendDoclist(postings[t], detail)
	}
	nLeaf := w.finish()
	for id, page := range w.pages {
		// The 16-bit limit binds szLeaf -- the leaf's own length in page[2..3]
		// -- not the whole page. C writes exactly that:
		//
		//	fts5PutU16(&pPage->buf.p[2], (u16)pPage->buf.n);   fts5_index.c:4425
		//
		// and appends the page index after it, so a page may exceed 0xffff
		// while its leaf does not (fts5misc.test 5.1, pgsz=65536). Every page
		// index offset points inside the leaf, so those fit too. A flush fires
		// at buf+pgidx >= pgsz <= 65536, so this is an assertion, not a live
		// restriction.
		if szLeaf := int(page[2])<<8 | int(page[3]); szLeaf > fts5MaxLeafSize {
			return nil, fmt.Errorf("engine: fts5: a single term is too long to index (leaf page %d would be %d bytes, past the format's 16-bit page offsets)", id&0x7fffffff, szLeaf)
		}
	}
	maps.Copy(img.data, w.pages)
	img.idx = w.idx
	img.data[fts5StructureRowid] = fts5EncodeStructure(1, 1, nLeaf, v2)
	return img, nil
}

// fts5EncodeDoclistTail encodes what follows one document's rowid in a doclist
// entry, for the table's detail= mode. Each position is (iCol<<32)|iOffset, in
// ascending order.
func fts5EncodeDoclistTail(positions []int64, detail fts5Detail) []byte {
	switch detail {
	case fts5DetailNone:
		// Nothing follows the rowid at all. The caller skips the size varint
		// too, so this is only ever the empty slice.
		return nil
	case fts5DetailColumns:
		return fts5EncodeCollist(positions)
	}
	return fts5EncodePoslist(positions)
}

// fts5EncodePoslist encodes one document's positions for a single term. See
// this file's comment for the "0x01, column" escape and the +2 bias.
func fts5EncodePoslist(positions []int64) []byte {
	var out []byte
	var buf [9]byte
	var prev int64 // starts at column 0, offset 0
	for _, pos := range positions {
		if pos&^0xffffffff != prev&^0xffffffff {
			out = append(out, 0x01)
			out = append(out, buf[:putVarint(buf[:], uint64(pos>>32))]...)
			prev = pos &^ 0xffffffff
		}
		out = append(out, buf[:putVarint(buf[:], uint64(pos-prev+2))]...)
		prev = pos
	}
	return out
}

// fts5EncodeCollist is detail=columns' replacement for the position list: one
// varint per column the term occurs in, (iCol - iPrevCol + 2) with iPrevCol
// starting at 0. positions arrive column-major and ascending (fts5BuildIndex's
// own iteration order), so "the column changed" is the only test needed --
// sqlite3Fts5HashWrite's `if( iCol!=p->iCol )` guard, which is what keeps a
// term occurring five times in one column to a single varint.
func fts5EncodeCollist(positions []int64) []byte {
	var out []byte
	var buf [9]byte
	prevCol := int64(0)
	last := int64(-1)
	for _, pos := range positions {
		col := pos >> 32
		if col == last {
			continue
		}
		out = append(out, buf[:putVarint(buf[:], uint64(col-prevCol+2))]...)
		prevCol, last = col, col
	}
	return out
}

// fts5StructV2 is the extra per-segment state of a contentless_delete=1
// table's structure record (FTS5_STRUCTURE_V2). nil means the legacy record.
//
// origin1/origin2 bound the origins of the documents a segment holds:
// fts5StorageContentlessDelete hands a deleted row's %_docsize origin to
// sqlite3Fts5IndexContentlessDelete, which tombstones it in every segment
// whose range contains it. This engine's one segment must therefore span
// 1..origin2, and origin2 also stores the origin counter, since
// fts5StructureDecode recovers it as max(origin2)+1.
//
// nEntry is the row count; for a fully merged segment, the live documents. C
// uses it only for 'deletemerge' scheduling (fts5IndexFindDeleteMerge).
type fts5StructV2 struct {
	origin1, origin2 uint64
	nEntry           uint64
}

// fts5EncodeStructure builds the structure record for this engine's shape:
// nSeg (0 or 1) segments at level 0 spanning leaves 1..nLeaf; v2 non-nil
// writes V2.
//
// nWriteCounter is the total number of leaf pages ever written (1, 2 and 5 for
// a one-, two- and five-page segment from one INSERT). A rebuild has written
// nLeaf, which makes the record byte-identical to C's for a table built by one
// statement; C uses the counter only for merge scheduling.
//
// An empty contentless_delete=1 table has x'00000000FF000001000000' (an
// ordinary one x'00000000000000'): the V2 marker is written with no segment,
// so the first insert takes origin 1 rather than the legacy, un-deletable
// format.
func fts5EncodeStructure(segid, nSeg, nLeaf int, v2 *fts5StructV2) []byte {
	out := []byte{0, 0, 0, 0} // configuration cookie
	if v2 != nil {
		out = append(out, fts5StructureV2Marker...)
	}
	var buf [9]byte
	put := func(v uint64) { out = append(out, buf[:putVarint(buf[:], v)]...) }
	if nSeg == 0 {
		// No terms at all: nLevel 0, exactly as a freshly created table's.
		put(0)
		put(0)
		put(0)
		return out
	}
	put(1) // nLevel
	put(uint64(nSeg))
	put(uint64(nLeaf))
	put(0) // level 0: nMerge
	put(uint64(nSeg))
	put(uint64(segid))
	put(1) // pgnoFirst
	put(uint64(nLeaf))
	if v2 != nil {
		put(v2.origin1)
		put(v2.origin2)
		put(0) // nPgTombstone: a rebuilt index has no deleted rows left in it
		put(0) // nEntryTombstone
		put(v2.nEntry)
	}
	return out
}

// fts5EncodeAverages builds the averages record: the live row count followed by
// each column's total token count.
func fts5EncodeAverages(nRow int64, colTotals []int64) []byte {
	var out []byte
	var buf [9]byte
	out = append(out, buf[:putVarint(buf[:], uint64(nRow))]...)
	for _, t := range colTotals {
		out = append(out, buf[:putVarint(buf[:], uint64(t))]...)
	}
	return out
}

// fts5EncodeDocsize builds one %_docsize row: a varint per column holding that
// column's token count.
func fts5EncodeDocsize(sizes []int) []byte {
	var out []byte
	var buf [9]byte
	for _, s := range sizes {
		out = append(out, buf[:putVarint(buf[:], uint64(s))]...)
	}
	return out
}

// ---- the segment writer ----

// fts5SegWriter accumulates one segment's leaf pages. Its field names follow
// fts5_index.c's Fts5SegWriter so the flush rules stay checkable against it.
type fts5SegWriter struct {
	segid int
	// pgsz is the leaf budget both flush rules measure against, fts5's own
	// "pgsz" configuration value (fts5_config.go). It is a per-TABLE setting,
	// not a constant, which is why it lives here rather than beside
	// fts5PageSize.
	pgsz  int
	pages map[int64][]byte
	idx   []fts5IdxRow

	buf   []byte // current leaf, including its 4-byte header
	pgidx []byte // current leaf's term offset array (its footer)
	pgno  int

	firstTermInPage  bool
	firstRowidInPage bool
	prevTerm         []byte // previous term ON THIS PAGE, for prefix compression
	prevPgidxOff     int    // previous term's page offset, for the footer's deltas
	lastTerm         []byte // last term written anywhere, for the b-tree separator

	btterm  []byte // pending %_idx separator
	iBtPage int    // leaf page that separator covers
}

func newFts5SegWriter(segid, pgsz int) *fts5SegWriter {
	return &fts5SegWriter{
		segid:            segid,
		pgsz:             pgsz,
		pages:            map[int64][]byte{},
		buf:              make([]byte, 4, pgsz+64),
		pgno:             1,
		firstTermInPage:  true,
		firstRowidInPage: true,
		iBtPage:          1,
	}
}

// flushLeaf writes the current leaf page and starts the next one.
func (w *fts5SegWriter) flushLeaf() {
	// szLeaf: where the footer starts. The header's first field (the offset of
	// the page's first rowid) was already filled in, if any.
	w.buf[2] = byte(len(w.buf) >> 8)
	w.buf[3] = byte(len(w.buf))
	page := append(w.buf, w.pgidx...)
	w.pages[fts5SegmentRowid(w.segid, w.pgno)] = page

	w.buf = make([]byte, 4, w.pgsz+64)
	w.pgidx = nil
	w.pgno++
	w.firstTermInPage = true
	w.firstRowidInPage = true
	w.prevTerm = nil
	w.prevPgidxOff = 0
}

func (w *fts5SegWriter) appendTerm(term []byte) {
	var buf [9]byte
	if len(w.buf)+len(w.pgidx)+len(term)+2 >= w.pgsz && len(w.buf) > 4 {
		w.flushLeaf()
	}
	off := len(w.buf)
	if w.firstTermInPage {
		if w.pgno != 1 {
			// This leaf is not the segment's leftmost, so the b-tree needs a
			// separator: the shortest prefix of this term that sorts above
			// every term on the page just written.
			n := fts5CommonPrefix(w.lastTerm, term) + 1
			if n > len(term) {
				n = len(term)
			}
			w.flushBtree()
			w.btterm = append([]byte(nil), term[:n]...)
			w.iBtPage = w.pgno
		}
		w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(len(term)))]...)
		w.buf = append(w.buf, term...)
	} else {
		nPrefix := fts5CommonPrefix(w.prevTerm, term)
		w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(nPrefix))]...)
		w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(len(term)-nPrefix))]...)
		w.buf = append(w.buf, term[nPrefix:]...)
	}
	w.pgidx = append(w.pgidx, buf[:putVarint(buf[:], uint64(off-w.prevPgidxOff))]...)
	w.prevPgidxOff = off
	w.prevTerm = append([]byte(nil), term...)
	w.lastTerm = w.prevTerm
	w.firstTermInPage = false
	// A term clears "first rowid in page" for HEADER purposes (a rowid behind
	// a term is not the page's entry point) -- fts5WriteAppendTerm's own
	// "pWriter->bFirstRowidInPage = 0". The rowid that follows is still
	// written absolute, because it is its doclist's first (appendDoclist).
	w.firstRowidInPage = false
}

// appendDoclist writes one term's whole doclist, a port of the leaf-writing
// loop in fts5FlushOneHash (fts5_index.c) -- the writer C uses for an ordinary
// INSERT. (fts5WriteAppendRowid/PoslistData belong to the merge path and lay
// the same content out differently.)
//
// The rules, in C's order:
//
//	if( pgsz>=(pBuf->n + pPgidx->n + nDoclist + 1) ){ blit the whole doclist }
//
// -- the whole-doclist fast path, never flushing. Otherwise, per entry: the
// rowid is appended with no flush test (absolute, and stamped into the page
// header, when it opens a page); then
//
//	detail=none      if( (pBuf->n + pPgidx->n)>=pgsz ) flush
//	otherwise        if( (pBuf->n+pPgidx->n+nCopy)<=pgsz ) blit size+poslist
//	                 else split it with fts5PoslistPrefix, flushing whenever
//	                      the page reaches pgsz
//
// where nCopy is the size varint and the poslist together.
func (w *fts5SegWriter) appendDoclist(postings []fts5Posting, detail fts5Detail) {
	// nDoclist is the length of the blob fts5's hash would have handed the
	// flusher: an absolute first rowid, deltas after it, each followed by
	// whatever tail the detail= mode writes.
	nDoclist := 0
	prev := int64(0)
	for i, p := range postings {
		d := p.rowid
		if i > 0 {
			d = p.rowid - prev
		}
		prev = p.rowid
		nDoclist += varintLen(uint64(d))
		if detail != fts5DetailNone {
			nDoclist += varintLen(uint64(len(p.poslist))<<1) + len(p.poslist)
		}
	}
	whole := w.pgsz >= len(w.buf)+len(w.pgidx)+nDoclist+1

	var buf [9]byte
	prev = 0
	for i, p := range postings {
		// The rowid. A page-opening one is absolute and records its own offset
		// in the page header; every other one is a delta, and the first entry's
		// "delta" is the rowid itself (the C starts iPrev at 0).
		if w.firstRowidInPage {
			w.buf[0] = byte(len(w.buf) >> 8)
			w.buf[1] = byte(len(w.buf))
			w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(p.rowid))]...)
			w.firstRowidInPage = false
		} else {
			d := p.rowid
			if i > 0 {
				d = p.rowid - prev
			}
			w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(d))]...)
		}
		prev = p.rowid

		if whole {
			if detail != fts5DetailNone {
				w.buf = append(w.buf, buf[:putVarint(buf[:], uint64(len(p.poslist))<<1)]...)
				w.buf = append(w.buf, p.poslist...)
			}
			continue
		}
		if detail == fts5DetailNone {
			// A rebuilt index never writes the 0x00 delete markers the C copies
			// here, so the rowid is the whole entry -- but the flush test after
			// it is the same one.
			if len(w.buf)+len(w.pgidx) >= w.pgsz {
				w.flushLeaf()
			}
			continue
		}
		n := putVarint(buf[:], uint64(len(p.poslist))<<1)
		entry := append(buf[:n:n], p.poslist...)
		if len(w.buf)+len(w.pgidx)+len(entry) <= w.pgsz {
			w.buf = append(w.buf, entry...)
			continue
		}
		for iPos := 0; ; {
			nSpace := w.pgsz - len(w.buf) - len(w.pgidx)
			k := len(entry) - iPos
			if k > nSpace {
				k = fts5PoslistPrefix(entry[iPos:], nSpace)
			}
			w.buf = append(w.buf, entry[iPos:iPos+k]...)
			iPos += k
			if len(w.buf)+len(w.pgidx) >= w.pgsz {
				w.flushLeaf()
			}
			if iPos >= len(entry) {
				break
			}
		}
	}
}

// fts5PoslistPrefix is ext/fts5/fts5_index.c's function of the same name: the
// length of the longest whole-varint prefix of aBuf that fits in nMax bytes --
// except that the FIRST varint is always taken, however small nMax is (the C
// reads it before testing "if( ret<nMax )", and asserts the result is
// positive). That is what keeps a split from stalling on a page with no room
// left, and it is why a leaf can end a byte or two past pgsz.
func fts5PoslistPrefix(aBuf []byte, nMax int) int {
	_, ret := getVarint(aBuf)
	if ret == 0 {
		return len(aBuf)
	}
	if ret < nMax {
		for ret < len(aBuf) {
			_, i := getVarint(aBuf[ret:])
			if i == 0 || ret+i > nMax {
				break
			}
			ret += i
		}
	}
	return ret
}

// finish writes the last leaf and the final b-tree entry, returning the number
// of leaf pages in the segment.
func (w *fts5SegWriter) finish() int {
	if len(w.buf) > 4 {
		w.flushLeaf()
	}
	nLeaf := w.pgno - 1
	if w.pgno > 1 {
		w.flushBtree()
	}
	return nLeaf
}

// flushBtree emits the pending %_idx row. pgno carries the leaf page number
// shifted left one bit; the low bit would flag a doclist index, which this
// engine never writes (see this file's comment).
func (w *fts5SegWriter) flushBtree() {
	w.idx = append(w.idx, fts5IdxRow{
		segid: int64(w.segid),
		term:  append([]byte(nil), w.btterm...),
		pgno:  int64(w.iBtPage) << 1,
	})
}

func fts5CommonPrefix(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
