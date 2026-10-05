// This file implements the on-disk encoding of an FTS3/FTS4 full-text index:
// the varint dialect, the "simple" tokenizer, the per-term doclist,
// and the segment b-tree leaf node that a %_segdir row's "root" column holds.
// It is pure encoding -- no schema, no statement handling; vtab_fts3.go drives it.
// %_segdir/%_segments are ordinary tables in the file, so their contents are
// part of the file format: a byte difference here is a byte difference in every
// C SQLite database we write.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// ---- varints ----

// FTS3 has its own varint format, not the SQLite file-format varint in varint.go:
// file-format varints are big-endian (most significant group first), FTS3's are
// little-endian (least significant group first). Values under 128 are one byte
// either way; larger values show the difference.

// fts3PutVarint appends v to dst as an FTS3 varint and returns the new slice.
func fts3PutVarint(dst []byte, v uint64) []byte {
	for v>>7 != 0 {
		dst = append(dst, byte(0x80|(v&0x7f)))
		v >>= 7
	}
	return append(dst, byte(v))
}

// fts3GetVarint decodes an FTS3 varint from the front of b, returning the
// value and the number of bytes consumed. A truncated varint consumes what is
// there and reports n == 0, which every caller treats as a corrupt node.
func fts3GetVarint(b []byte) (v uint64, n int) {
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
		if shift >= 64 {
			return 0, 0
		}
	}
	return 0, 0
}

// ---- the "simple" tokenizer ----

// FTS3/FTS4's default tokenizer is "simple" (fts5's is unicode61; they are not
// the same). Its rules:
//   - A byte is a token character iff it is an ASCII alphanumeric (0-9, A-Z, a-z)
//     or >= 0x80; every other byte is a delimiter.
//   - ASCII A-Z folds to lowercase; bytes >= 0x80 pass through unchanged.
//   - Positions count tokens from 0 and restart at 0 for each column.

// fts3IsTokenChar reports whether byte c is part of a token for the "simple"
// tokenizer.
func fts3IsTokenChar(c byte) bool {
	return c >= 0x80 ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// fts3TokenSpan is one token as the tokenizer's own xNext reports it: the
// folded term, and the BYTE range of the raw text it came from. The offsets
// are what the query parser needs (a trailing '*' or leading '^' is
// recognised by looking at the byte just outside the span, exactly as
// fts3_expr.c does) and what offsets()/snippet() report.
type fts3TokenSpan struct {
	term       string
	start, end int
}

// fts3TokenizeSpans returns every token of s with its byte range.
func fts3TokenizeSpans(s string) []fts3TokenSpan {
	var out []fts3TokenSpan
	i := 0
	for i < len(s) {
		if !fts3IsTokenChar(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && fts3IsTokenChar(s[j]) {
			j++
		}
		out = append(out, fts3TokenSpan{term: fts3FoldTerm(s[i:j]), start: i, end: j})
		i = j
	}
	return out
}

// fts3TokenizeSpansDelim is fts3TokenizeSpans for a "simple" tokenizer built
// with an EXPLICIT delimiter list (fts3_tokenizer.go's fts3NewSimple): the
// default alphanumeric test is replaced wholesale by simpleDelim, under which
// a byte is a delimiter only when it is ASCII and listed -- so every byte
// >= 0x80, and every unlisted ASCII byte, is a token character.
func fts3TokenizeSpansDelim(s string, delim *[128]bool) []fts3TokenSpan {
	isTok := func(c byte) bool { return c >= 0x80 || !delim[c] }
	var out []fts3TokenSpan
	i := 0
	for i < len(s) {
		if !isTok(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isTok(s[j]) {
			j++
		}
		out = append(out, fts3TokenSpan{term: fts3FoldTerm(s[i:j]), start: i, end: j})
		i = j
	}
	return out
}

// fts3FoldTerm lower-cases the ASCII letters of one token, leaving every other
// byte (in particular every byte >= 0x80) alone.
func fts3FoldTerm(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// ---- doclists ----

// fts3Hit is one occurrence of a term: which column of the document it was
// found in, and its position within that column.
type fts3Hit struct {
	col int
	pos int
}

// fts3Doclist accumulates one term's occurrences across the documents of a
// single flush. docids is in ascending order and hits[docid] is that
// document's occurrences in (column, position) order.
// Ascending order is a requirement: a doclist is delta-encoded and cannot
// express a backwards step. vtab_fts3.go declines a statement whose docids
// are not strictly ascending.
type fts3Doclist struct {
	docids []int64
	hits   map[int64][]fts3Hit
}

// fts3PendingTerms is the in-memory term index for one flush: the contents of
// exactly one new segment.
type fts3PendingTerms struct {
	terms map[string]*fts3Doclist
	// desc is the table's "order=desc" bit, affecting only how doclist encodes.
	// docids accumulated here stay ascending either way.
	desc bool
}

func newFts3PendingTerms(desc bool) *fts3PendingTerms {
	return &fts3PendingTerms{terms: map[string]*fts3Doclist{}, desc: desc}
}

// add records that term occurs in document docid at (col, pos). Documents must
// be added in ascending docid order, and within a document in ascending
// (col, pos) order -- the doclist encoding is delta-based and has no way to
// represent anything else.
func (pt *fts3PendingTerms) add(term string, docid int64, col, pos int) {
	dl := pt.docidSlot(term, docid)
	dl.hits[docid] = append(dl.hits[docid], fts3Hit{col: col, pos: pos})
}

// addDelete records a delete marker for term at docid: the document appears
// in the doclist with an empty position list. A term deleted and re-inserted
// at the same docid (an UPDATE) ends up as an ordinary entry.
func (pt *fts3PendingTerms) addDelete(term string, docid int64) {
	pt.docidSlot(term, docid)
}

// docidSlot returns term's doclist with docid opened as its current document.
func (pt *fts3PendingTerms) docidSlot(term string, docid int64) *fts3Doclist {
	dl := pt.terms[term]
	if dl == nil {
		dl = &fts3Doclist{hits: map[int64][]fts3Hit{}}
		pt.terms[term] = dl
	}
	if len(dl.docids) == 0 || dl.docids[len(dl.docids)-1] != docid {
		dl.docids = append(dl.docids, docid)
	}
	return dl
}

func (pt *fts3PendingTerms) empty() bool { return len(pt.terms) == 0 }

// encodeDoclist encodes one term's doclist:
//  for each document, ascending (or descending for order=desc tables):
//    varint docid delta
//    for each occurrence:
//      varint 1 + varint column (only when column changes; column 0 is implicit)
//      varint position - previousPosition + 2
//    varint 0 (end of document's position list)
// A document with no occurrences is a delete marker (see addDelete).
func (dl *fts3Doclist) encode(desc bool) []byte {
	var out []byte
	var prevDocid int64
	order := dl.docids
	if desc {
		order = make([]int64, len(dl.docids))
		for i, d := range dl.docids {
			order[len(dl.docids)-1-i] = d
		}
	}
	// first distinguishes the first document: docid 0 is a valid docid, so we
	// can't use "prevDocid == 0" as the marker.
	first := true
	for _, docid := range order {
		if desc && !first {
			out = fts3PutVarint(out, uint64(prevDocid-docid))
		} else {
			out = fts3PutVarint(out, uint64(docid-prevDocid))
		}
		prevDocid, first = docid, false
		prevCol, prevPos := 0, 0
		for _, h := range dl.hits[docid] {
			if h.col != prevCol {
				out = fts3PutVarint(out, 1)
				out = fts3PutVarint(out, uint64(h.col))
				prevCol, prevPos = h.col, 0
			}
			out = fts3PutVarint(out, uint64(h.pos-prevPos+2))
			prevPos = h.pos
		}
		out = append(out, 0)
	}
	return out
}

// ---- segment leaf nodes ----

// ---- spilling a segment across %_segments ----

// fts3NodeHeaderReserve is the space fts3's interior-node builder keeps at the
// front of every node buffer for the node header it only fills in at the end:
// one height byte plus FTS3_VARINT_MAX (10). fts3TreeFinishNode then writes
// the header so that it ENDS at this offset -- the varint for the leftmost
// child block is right-aligned into the reserve and the unused bytes in front
// of it are simply never written out. The reserve is counted in the node's
// size check even though the header is usually shorter, so it is part of the
// node's effective capacity, not just an allocation detail.
const fts3NodeHeaderReserve = 1 + 10

// fts3VarintLen returns the number of bytes fts3PutVarint writes for v.
func fts3VarintLen(v uint64) int {
	n := 1
	for v>>7 != 0 {
		v >>= 7
		n++
	}
	return n
}

// fts3SegmentImage is one flush's whole segment, ready to store: the bytes for
// %_segdir's row plus the %_segments blocks it spilled into, if any.
//
// A segment SMALL enough to fit one leaf node needs no blocks at all: root
// holds that leaf and start_block / leaves_end_block / end_block are 0. Once
// the terms outgrow one node (nodeSize == pgsz-35), C fts3 writes each full
// leaf into %_segments as its own block and builds an INTERIOR node tree over
// them, whose topmost node becomes root; blocks then holds, in ascending
// blockid order from firstBlock: every leaf, then every non-root interior
// level bottom-up. Verified against the oracle -- see fts3_shadow_test.go's
// spill cases, which compare the raw %_segdir and %_segments contents.
type fts3SegmentImage struct {
	root       []byte
	blocks     [][]byte // consecutive blockids starting at firstBlock
	firstBlock int64    // %_segdir.start_block (0: no blocks at all)
	leavesEnd  int64    // %_segdir.leaves_end_block
	endBlock   int64    // the integer half of %_segdir.end_block
	nLeafData  int64    // its second half: total bytes of leaf entries
	// nLeaves is the number of leaf nodes this segment write produced.
	// For a segment that fits in the root: 1. For spilled segments: 1 plus
	// the number of mid-stream leaf flushes. Not len(blocks) because interior
	// nodes also live there.
	nLeaves int64
}

// fts3InteriorNode is one node of the interior tree fts3 builds over spilled
// segment leaves. data holds the term list; the header is prepended by finish.
// nEntry tells the next sibling where its leftmost child is: a node with n
// terms separates n+1 children.
type fts3InteriorNode struct {
	data    []byte
	term    string
	hasTerm bool
	nEntry  int
}

// finish prepends the node header: [height byte][varint leftmost child block][terms].
func (n *fts3InteriorNode) finish(height int, leftChild int64) []byte {
	out := make([]byte, 0, fts3NodeHeaderReserve+len(n.data))
	out = append(out, byte(height))
	out = fts3PutVarint(out, uint64(leftChild))
	return append(out, n.data...)
}

// fts3InteriorTree is the interior-node tree under construction: levels[0] is
// the height-1 nodes (whose children are leaves), levels[1] their parents, and
// so on. The last level always holds exactly ONE node -- a second node at a
// level only ever appears by pushing a term into the level above, which
// creates that level -- and that node is the segment's root.
type fts3InteriorTree struct {
	nodeSize int
	levels   [][]*fts3InteriorNode
}

// addTerm appends term to the current node at height h+1, or if it doesn't fit,
// starts a new sibling and pushes term into the parent. A node's first term
// carries no prefix varint; the size check counts one anyway but skips it for
// an empty node, so an oversized term still fits.
func (t *fts3InteriorTree) addTerm(h int, term string) {
	for len(t.levels) <= h {
		t.levels = append(t.levels, nil)
	}
	if n := len(t.levels[h]); n > 0 {
		cur := t.levels[h][n-1]
		nPrefix := fts3CommonPrefixLen(cur.term, term)
		nSuffix := len(term) - nPrefix
		nReq := fts3NodeHeaderReserve + len(cur.data) +
			fts3VarintLen(uint64(nPrefix)) + fts3VarintLen(uint64(nSuffix)) + nSuffix
		if nReq <= t.nodeSize || !cur.hasTerm {
			if cur.hasTerm {
				cur.data = fts3PutVarint(cur.data, uint64(nPrefix))
			}
			cur.data = fts3PutVarint(cur.data, uint64(nSuffix))
			cur.data = append(cur.data, term[nPrefix:]...)
			cur.nEntry++
			cur.term, cur.hasTerm = term, true
			return
		}
		// Full: the term becomes a separator in the PARENT, and this level
		// gets a fresh, still-empty right sibling.
		t.addTerm(h+1, term)
		t.levels[h] = append(t.levels[h], &fts3InteriorNode{})
		return
	}
	t.levels[h] = append(t.levels[h], &fts3InteriorNode{})
	t.addTerm(h, term)
}

// encodeSegment encodes every pending term into one segment, spilling into
// %_segments blocks once terms outgrow a single nodeSize-byte node. Terms
// accumulate into a leaf buffer; when the next entry would exceed nodeSize,
// the buffer is flushed as a block and a separator term is pushed into the
// interior tree. Leaf first terms are stored in full (nPrefix == 0).
// An entry larger than nodeSize alone is not an error.
func (pt *fts3PendingTerms) encodeSegment(nodeSize int, nextBlock int64) *fts3SegmentImage {
	terms := make([]string, 0, len(pt.terms))
	for t := range pt.terms {
		terms = append(terms, t)
	}
	sort.Strings(terms)

	img := &fts3SegmentImage{}
	tree := &fts3InteriorTree{nodeSize: nodeSize}
	var leaf []byte
	prev := ""
	for _, term := range terms {
		dl := pt.terms[term].encode(pt.desc)
		nPrefix := fts3CommonPrefixLen(prev, term)
		nSuffix := len(term) - nPrefix
		nReq := fts3VarintLen(uint64(nPrefix)) + fts3VarintLen(uint64(nSuffix)) + nSuffix +
			fts3VarintLen(uint64(len(dl))) + len(dl)
		if len(leaf) > 0 && len(leaf)+nReq > nodeSize {
			img.blocks = append(img.blocks, leaf)
			img.nLeaves++ // mid-stream leaf flush
			// Separator is one byte past the shared prefix; the clamp prevents panic.
			sepLen := nPrefix + 1
			if sepLen > len(term) {
				sepLen = len(term)
			}
			tree.addTerm(0, term[:sepLen])
			leaf, prev = nil, ""
			nPrefix, nSuffix = 0, len(term)
			nReq = 1 + fts3VarintLen(uint64(len(term))) + len(term) +
				fts3VarintLen(uint64(len(dl))) + len(dl)
		}
		img.nLeafData += int64(nReq)
		leaf = fts3PutVarint(leaf, uint64(nPrefix))
		leaf = fts3PutVarint(leaf, uint64(nSuffix))
		leaf = append(leaf, term[nPrefix:]...)
		leaf = fts3PutVarint(leaf, uint64(len(dl)))
		leaf = append(leaf, dl...)
		prev = term
	}

	if len(tree.levels) == 0 {
		// Whole segment fits in the root node; nothing spills.
		img.root = leaf
		img.nLeaves = 1
		return img
	}

	img.firstBlock = nextBlock
	iFree := nextBlock + int64(len(img.blocks))
	img.leavesEnd = iFree // final leaf's blockid
	img.blocks = append(img.blocks, leaf)
	img.nLeaves++ // final leaf write
	iFree++

	// Interior levels, bottom up. Each level's nodes are written as
	// consecutive blocks, and each node's leftmost child follows the previous
	// node's: a node holding n separator terms covers n+1 children.
	childStart := img.firstBlock
	for h := 0; h < len(tree.levels); h++ {
		nodes := tree.levels[h]
		if h == len(tree.levels)-1 {
			// Topmost node is the root; goes in %_segdir, not %_segments.
			img.root = nodes[0].finish(h+1, childStart)
			img.endBlock = iFree - 1
			break
		}
		levelStart := iFree
		child := childStart
		for _, n := range nodes {
			img.blocks = append(img.blocks, n.finish(h+1, child))
			iFree++
			child += int64(n.nEntry + 1)
		}
		childStart = levelStart
	}
	return img
}

// fts3CommonPrefixLen returns the number of leading bytes a and b share.
func fts3CommonPrefixLen(a, b string) int {
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

// ---- %_docsize / %_stat blobs (FTS4 only) ----

// encodeFts4Docsize encodes one document's per-column token counts: one FTS3
// varint per column, in column order, and nothing else.
func encodeFts4Docsize(sizes []int) []byte {
	var out []byte
	for _, n := range sizes {
		out = fts3PutVarint(out, uint64(n))
	}
	return out
}

// fts4Stat is the decoded form of FTS4's %_stat row id=0: document count,
// token count per column, and total bytes of indexed text. Its encoding is
// nColumn+2 FTS3 varints in that order.
type fts4Stat struct {
	nDoc     int64
	colSizes []int64
	nByte    int64
}

func (s *fts4Stat) encode() []byte {
	out := fts3PutVarint(nil, uint64(s.nDoc))
	for _, n := range s.colSizes {
		out = fts3PutVarint(out, uint64(n))
	}
	return fts3PutVarint(out, uint64(s.nByte))
}

// decodeFts4Stat parses a %_stat id=0 blob for a table with nCol columns. An
// empty blob is the "no rows yet" state (all zeroes). A blob of any other
// length is a shape this engine did not write and cannot safely update, so it
// is declined rather than guessed at.
func decodeFts4Stat(b []byte, nCol int) (*fts4Stat, error) {
	s := &fts4Stat{colSizes: make([]int64, nCol)}
	if len(b) == 0 {
		return s, nil
	}
	vals := make([]uint64, 0, nCol+2)
	for off := 0; off < len(b); {
		v, n := fts3GetVarint(b[off:])
		if n == 0 {
			return nil, fmt.Errorf("fts4: corrupt %%_stat record")
		}
		vals = append(vals, v)
		off += n
	}
	if len(vals) != nCol+2 {
		return nil, fmt.Errorf("fts4: %%_stat record holds %d values, expected %d", len(vals), nCol+2)
	}
	s.nDoc = int64(vals[0])
	for i := 0; i < nCol; i++ {
		s.colSizes[i] = int64(vals[1+i])
	}
	s.nByte = int64(vals[nCol+1])
	return s, nil
}

// ---- shadow-table SQL ----

// fts3QuoteName renders name as a single-quoted SQL string literal: every
// embedded quote doubles.
func fts3QuoteName(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// merge folds other's documents into pt. Every docid in other must sort at or
// after pt's newest: the doclist encoding is docid-delta based and cannot
// represent otherwise. A docid present in both appends other's hits after pt's.
func (pt *fts3PendingTerms) merge(other *fts3PendingTerms) {
	for term, odl := range other.terms {
		for _, docid := range odl.docids {
			dl := pt.docidSlot(term, docid)
			dl.hits[docid] = append(dl.hits[docid], odl.hits[docid]...)
		}
	}
}
