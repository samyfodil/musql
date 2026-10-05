// "<fts3tab> MATCH '<query>'" answered from the index (the %_segdir/%_segments
// doclists fts3_index.go writes), never by rescanning %_content. C fts3 answers
// from the segment b-tree, so a row written into %_content behind the index's
// back matches nothing, and a row deleted from %_content but still indexed
// keeps matching.
//
// A segment is a %_segdir row. With start_block 0 the whole segment is the leaf
// in "root"; otherwise "root" is an interior node and the leaves are
// %_segments blocks start_block..leaves_end_block. Interior nodes are only a
// seek index, so every leaf is decoded in order. A leaf is a run of
//
//	varint nPrefix, varint nSuffix, suffix bytes, varint nDoclist, doclist
//
// entries; the leading 0x00 "height" byte is the first entry's nPrefix, as
// fts3_index.go writes leaves.
//
// Segments are read newest first (level ascending, idx descending, the order
// fts3SegReaderCmp gives). For one (term, docid) the newest segment wins, and
// an empty position list is a delete marker (FTS3_SEGMENT_IGNORE_EMPTY).
//
// A query is evaluated per document as fts3EvalTestExpr does: a phrase matches
// when its position list (of its last token, per column) is non-empty; AND/OR/
// NOT are set operations; NEAR is AND plus fts3EvalNearTrim's proximity test
// ("a NEAR/n b" allows at most n intervening tokens in the same column, either
// order).
//
// offsets(), snippet() and matchinfo() report on the cursor's query, so they
// read this file's per-row evaluation state, including NEAR's edits to the
// position lists. Expensive state (the decoded index, a query's docids and
// doclists, matchinfo statistics, %_stat/%_docsize) is cached on the
// ReadOnlyPager, a fixed snapshot.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// ---- reading the index ----

// fts3TermPostings is one term's postings: docid -> column -> ascending token
// positions. A document present with no REAL column at all is either a
// tombstone (nil map, dropped while reading -- see fts3MergeDoclist) or, only
// when fts3MergeDoclist was allowed to produce one, a byte-length-only match:
// exactly one entry keyed fts3ByteLengthMatchCol, always with a nil/empty
// position slice.
type fts3TermPostings map[int64]map[int][]int

// fts3ByteLengthMatchCol is the sentinel "column" fts3MergeDoclist stores a
// byte-length-only match under (see its allowByteLengthMatch parameter):
// larger than any real column index can ever be, so it satisfies the same
// "iColumn == nCol means no filter" test every column-aware caller already
// applies to a real "no column restriction" (fts3_query.go's iColumn doc
// comment) while never colliding with, or being reachable through, a real
// column lookup (0..nCol-1) or a fixed-size per-column slice indexed by a
// real column (e.g. globalHits' out[i][0][c], guarded by its own existing
// "c >= nCol" skip).
const fts3ByteLengthMatchCol = 1 << 30

// fts3Index is a whole fts3/fts4 table's index, decoded. terms is sorted by
// raw byte order (the order segments store them in), so a prefix query is a
// contiguous range.
type fts3Index struct {
	terms []string
	post  []fts3TermPostings
}

// allDocids returns every docid this index holds an entry for, in ascending
// order. Only a "content=" table needs it (fts3_content.go): those are the rows
// a MATCH can return even though the content table has nothing for them, and
// for every other table the set is exactly %_content's own rowids.
func (ix *fts3Index) allDocids() []int64 {
	seen := map[int64]bool{}
	for _, pl := range ix.post {
		for d := range pl {
			seen[d] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// fts3LoadIndex decodes the term-index segments of fts3/fts4 table name whose
// %_segdir levels start at base (0, or getAbsoluteLevel(langid, 0, 0) with
// "languageid=", fts3_langid.go).
//
// wanted narrows decoding to the query's terms, as C fts3's lazy reader does:
// fts3EvalPhraseStart (fts3.c:4491) opens one reader per token, and a
// non-prefix reader stops at its one term (fts3_write.c:2935-2936; see
// fts3SegmentWant.consume). nil decodes everything, for fts3IndexOnlyDocids
// and fts3AuxRows; fts3MatchDocids passes fts3GatherWantedTerms(root).
func (p *ReadOnlyPager) fts3LoadIndex(name string, base int64, wanted *fts3WantedTerms) (*fts3Index, error) {
	_, segdir, err := p.Rows(name + "_segdir")
	if err != nil {
		return nil, err
	}
	// The stored doclists of an "order=desc" table descend (fts3_index.go), so
	// the decode has to be told. Read off the table's own declaration rather
	// than threaded in: every caller here already resolved this table by name.
	desc := false
	if meta, ok := p.fts3TableInfo(name); ok {
		desc = meta.descIdx
	}
	type segment struct {
		level, idx          int64
		startBlock, endLeaf int64
		root                []byte
	}
	segs := make([]segment, 0, len(segdir))
	for _, row := range segdir {
		if len(row) < 6 {
			return nil, fmt.Errorf("fts3: %s_segdir row has %d columns, expected 6", name, len(row))
		}
		s := segment{}
		if row[0].Typ != Int || row[1].Typ != Int {
			return nil, fmt.Errorf("fts3: %s_segdir row has a non-integer level/idx", name)
		}
		s.level, s.idx = row[0].I, row[1].I
		// A PREFIX index's segments share this table (fts3_prefix.go) and hold
		// TRUNCATED terms, so folding them in would make "MATCH 'al'" find a row
		// whose only token is "alpha". Only the term index is read.
		if !fts3IsTermIndexLevel(s.level, base) {
			continue
		}
		if row[2].Typ == Int {
			s.startBlock = row[2].I
		}
		if row[3].Typ == Int {
			s.endLeaf = row[3].I
		}
		switch row[5].Typ {
		case Blob, Text:
			s.root = row[5].S
		case Null:
		default:
			return nil, fmt.Errorf("fts3: %s_segdir root is not a blob", name)
		}
		segs = append(segs, s)
	}
	// Newest first: level ascending, idx descending.
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].level != segs[j].level {
			return segs[i].level < segs[j].level
		}
		return segs[i].idx > segs[j].idx
	})

	var blocks map[int64][]byte
	needBlocks := false
	for _, s := range segs {
		if s.startBlock != 0 {
			needBlocks = true
			break
		}
	}
	if needBlocks {
		rowids, rows, berr := p.Rows(name + "_segments")
		if berr != nil {
			return nil, berr
		}
		blocks = make(map[int64][]byte, len(rows))
		for i, row := range rows {
			if len(row) >= 2 && (row[1].Typ == Blob || row[1].Typ == Text) {
				blocks[int64(rowids[i])] = row[1].S
			}
		}
	}

	ix := &fts3Index{}
	byTerm := map[string]fts3TermPostings{}
	for _, s := range segs {
		// A fresh tracker per segdir row: C fts3 opens an independent
		// Fts3SegReader per (segment, token) pair -- sqlite3Fts3SegReaderCursor
		// iterates every segdir row for the SAME token (fts3.c:3010-3039) -- so
		// a term already found in a newer segment must still be sought
		// independently in an older one.
		sw := wanted.openForSegment()
		if sw != nil && sw.resolved() {
			// Nothing left for this query's tokens to find in this segment --
			// every real-fts3 reader for it would already be at EOF (an empty
			// query, or one whose every token was already satisfied by a
			// newer segment) without ever calling fts3SegReaderNext once.
			continue
		}
		var nodes [][]byte
		if s.startBlock == 0 {
			nodes = [][]byte{s.root}
		} else {
			if s.endLeaf < s.startBlock {
				return nil, fmt.Errorf("fts3: %s_segdir segment has leaves_end_block < start_block", name)
			}
			for b := s.startBlock; b <= s.endLeaf; b++ {
				blk, ok := blocks[b]
				if !ok {
					return nil, fmt.Errorf("fts3: %s_segments is missing block %d", name, b)
				}
				nodes = append(nodes, blk)
			}
		}
		for _, node := range nodes {
			if sw != nil && sw.resolved() {
				// A multi-block segment whose earlier leaf already resolved
				// every want: C fts3 never issues the read for a LATER
				// block, because that read only happens INSIDE
				// fts3SegReaderNext (fts3_write.c:1394's sqlite3Fts3ReadBlock
				// call), and fts3SegReaderNext is exactly the function that
				// stops being called once every reader is at EOF (the
				// bLookup shortcut, fts3_write.c:2935-2936, or -- for the
				// last continuing prefix reader -- the "no longer matches,
				// stop" break at fts3_write.c:2959-2966).
				break
			}
			if err := fts3DecodeLeafNode(node, func(term string, doclist []byte, nDoclist int) (bool, error) {
				if sw == nil {
					return false, fts3MergeDoclist(byTerm, term, doclist, nDoclist, desc, true)
				}
				if sw.consume(term) {
					if err := fts3MergeDoclist(byTerm, term, doclist, nDoclist, desc, true); err != nil {
						return false, err
					}
				} else if !fts3DoclistTerminatorOK(doclist, nDoclist) {
					// Not a term any remaining token wants -- but C fts3's
					// own reader still structurally parsed it while walking
					// past on the way to (or past) its target
					// (fts3_write.c:2790-2801's do-while calls
					// fts3SegReaderNext unconditionally every iteration, and
					// fts3_write.c:2938 does the same for a prefix
					// continuation), and that parse always includes THIS
					// check (fts3_write.c:1450-1453) -- not merely the
					// node-bound check fts3DecodeLeafNode's own outer loop
					// already applies to every term regardless of whether it
					// is ever decoded further.
					return false, fmt.Errorf("fts3: corrupt doclist (declared doclist does not end in 0x00)")
				}
				return sw.resolved(), nil
			}); err != nil {
				return nil, err
			}
		}
	}
	ix.terms = make([]string, 0, len(byTerm))
	for t := range byTerm {
		ix.terms = append(ix.terms, t)
	}
	sort.Strings(ix.terms)
	ix.post = make([]fts3TermPostings, len(ix.terms))
	for i, t := range ix.terms {
		ix.post[i] = byTerm[t]
	}
	return ix, nil
}

// fts3WantedTerms is the (term, isPrefix) set one MATCH query's phrases
// reference, gathered once (fts3GatherWantedTerms) and reused by
// fts3LoadIndex across every segment. A nil *fts3WantedTerms means "no
// narrowing, decode everything" (see fts3LoadIndex's doc comment for who
// passes nil and why); a non-nil one with no terms at all (an empty query,
// whose root is nil) resolves immediately, matching C fts3 issuing no
// reader at all for it.
type fts3WantedTerms struct {
	exact    []string
	prefixes []string
}

// fts3GatherWantedTerms walks every phrase reachable from root -- the same
// walk newEval's own "collect" closure makes, including a NOT's right operand
// (its doclist is loaded even though it is not "numbered": see newEval's
// comment) -- and returns the token set as (term, isPrefix) pairs, with
// neither column nor NEAR distance attached. That is enough: C fts3 opens
// one Fts3SegReader per query TOKEN with no awareness of either at that layer
// (fts3.c:4491 fts3EvalPhraseStart, fts3.c:3110 fts3TermSegReaderCursor) --
// FTS3_SEGMENT_COLUMN_FILTER is applied AFTER a doclist is already fetched
// (fts3_write.c:3020-3021), matching this engine's own ix.lookup, which
// applies iCol only after ix.post already holds the term's full postings.
func fts3GatherWantedTerms(root *fts3Expr) *fts3WantedTerms {
	w := &fts3WantedTerms{}
	var walk func(e *fts3Expr)
	walk = func(e *fts3Expr) {
		if e == nil {
			return
		}
		if e.eType == fts3qPhrase {
			for _, tok := range e.phrase.tokens {
				if tok.prefix {
					w.prefixes = append(w.prefixes, tok.term)
				} else {
					w.exact = append(w.exact, tok.term)
				}
			}
			return
		}
		walk(e.left)
		walk(e.right)
	}
	walk(root)
	return w
}

// fts3SegmentWant is one %_segdir row's remaining work while fts3LoadIndex
// walks it: which exact terms have not yet been found in THIS segment, and
// which prefixes have not yet been closed. It is opened fresh per segment
// (fts3LoadIndex) because C fts3 opens an independent Fts3SegReader per
// (segment, token) pair -- sqlite3Fts3SegReaderCursor iterates every segdir
// row for the SAME token (fts3.c:3010-3039) -- so a term already found in a
// newer segment must still be sought independently in an older one.
type fts3SegmentWant struct {
	exact    map[string]bool
	prefixes []string
}

// openForSegment returns a fresh per-segment tracker, or nil when w itself is
// nil -- the eager, whole-vocabulary callers (fts3IndexOnlyDocids,
// fts3AuxRows; see fts3LoadIndex's doc comment).
func (w *fts3WantedTerms) openForSegment() *fts3SegmentWant {
	if w == nil {
		return nil
	}
	sw := &fts3SegmentWant{exact: make(map[string]bool, len(w.exact))}
	for _, t := range w.exact {
		sw.exact[t] = true
	}
	sw.prefixes = append(sw.prefixes, w.prefixes...)
	return sw
}

// consume is called once per term the walk parses, in ascending order (the
// order of fts3SegReaderTermCmp, fts3_write.c:1888-1904, which Go's string
// comparison matches), and reports whether its doclist should be decoded.
//
// An exact term is removed once seen: the bLookup shortcut
// (fts3_write.c:2935-2936) never looks for it again. A pending exact term is
// also dropped once the walk passes it: each C reader advances "do {
// fts3SegReaderNext } while( fts3SegReaderTermCmp(...)<0 )"
// (fts3_write.c:2790-2801) and is set EOF if it overshot (:2798). So corrupt
// bytes past where every target was satisfied are never reached
// (fts3corrupt4.test 41.2).
//
// A prefix stays open while term could still be in its range (it shares the
// prefix, or term < prefix). The first term at or past the prefix that does
// not share it closes the range: dropped, but still reported through the
// caller's terminator-only check, as fts3_write.c:2959-2966 runs after :2938
// parsed that term.
func (sw *fts3SegmentWant) consume(term string) (wanted bool) {
	if sw.exact[term] {
		delete(sw.exact, term)
		wanted = true
	}
	for t := range sw.exact {
		if term > t {
			// Passed without matching -- gone forever in this segment, same
			// as a closed prefix range below.
			delete(sw.exact, t)
		}
	}
	kept := sw.prefixes[:0]
	for _, pfx := range sw.prefixes {
		switch {
		case strings.HasPrefix(term, pfx):
			wanted = true
			kept = append(kept, pfx)
		case term < pfx:
			kept = append(kept, pfx) // range not reached yet -- still open
		default:
			// term > pfx and shares none of it: the range's closing term.
			// Drop pfx; never reopens.
		}
	}
	sw.prefixes = kept
	return wanted
}

// resolved reports whether every want in sw has been satisfied: every exact
// term found, every prefix closed. Once true, every real-fts3 reader for this
// segment (one per token, see openForSegment) is at EOF and none of them
// would ever call fts3SegReaderNext again -- so fts3LoadIndex stops decoding
// this segment's remaining terms and nodes right here.
func (sw *fts3SegmentWant) resolved() bool {
	return len(sw.exact) == 0 && len(sw.prefixes) == 0
}

// fts3NodePadding is FTS3_NODE_PADDING (fts3_write.c:40): 2*FTS3_VARINT_MAX
// (fts3Int.h:106) zero bytes that every real node-loading path allocates past
// a node's real end (sqlite3Fts3ReadBlock, fts3_write.c:1247-1256; the
// root-node loader, fts3_write.c:1656-1676) so that fts3's position-list scan
// -- which has no logical-end check of its own, see fts3MergeDoclist -- can
// never read past a byte it owns.
const fts3NodePadding = 2 * 10

// fts3DecodeLeafNode walks one segment leaf node, calling visit with each term,
// its declared doclist length nDoclist, and a doclist slice running from the
// term's doclist to the end of a padded copy of the node (see fts3MergeDoclist
// for why the scan may read past nDoclist). The walk itself always advances by
// nDoclist (fts3SegReaderNext's "pNext = &aDoclist[nDoclist]",
// fts3_write.c:1347).
//
// visit returning stop ends the walk without parsing further terms, as C fts3
// does: only fts3SegReaderNext (fts3_write.c:1334) parses a term, and a lookup
// reader that found its term goes to EOF (fts3_write.c:2935-2936). Callers
// other than fts3LoadIndex never stop.
func fts3DecodeLeafNode(node []byte, visit func(term string, doclist []byte, nDoclist int) (stop bool, err error)) error {
	if len(node) == 0 {
		return nil
	}
	if node[0] != 0x00 {
		// A non-zero height is an INTERIOR node. Only leaves are ever passed
		// here, so this is a corrupt (or unexpected) segment.
		return fmt.Errorf("fts3: segment leaf node has height %d", node[0])
	}
	padded := make([]byte, len(node)+fts3NodePadding)
	copy(padded, node)
	var prev []byte
	off := 0
	for off < len(node) {
		nPrefix, n := fts3GetVarint(node[off:])
		if n == 0 {
			return fmt.Errorf("fts3: corrupt segment node")
		}
		off += n
		nSuffix, n := fts3GetVarint(node[off:])
		if n == 0 {
			return fmt.Errorf("fts3: corrupt segment node")
		}
		off += n
		if nSuffix == 0 || int(nPrefix) > len(prev) || off+int(nSuffix) > len(node) {
			return fmt.Errorf("fts3: corrupt segment node")
		}
		term := make([]byte, 0, int(nPrefix)+int(nSuffix))
		term = append(term, prev[:nPrefix]...)
		term = append(term, node[off:off+int(nSuffix)]...)
		off += int(nSuffix)
		nDoclist, n := fts3GetVarint(node[off:])
		if n == 0 {
			return fmt.Errorf("fts3: corrupt segment node")
		}
		off += n
		// "does not appear to extend past the end of the b-tree node"
		// (fts3_write.c:1450) -- checked against the node's REAL length, never
		// the padding, so a doclist can never be DECLARED to reach into it.
		if nDoclist == 0 || off+int(nDoclist) > len(node) {
			return fmt.Errorf("fts3: corrupt segment node")
		}
		stop, err := visit(string(term), padded[off:], int(nDoclist))
		if err != nil {
			return err
		}
		off += int(nDoclist)
		prev = term
		if stop {
			return nil
		}
	}
	return nil
}

// fts3DoclistTerminatorOK is the "final byte of the doclist is 0x00" check
// (fts3_write.c:1450-1453) for a term the lazy walk passes without decoding.
// C runs it inside fts3SegReaderNext for every term a reader walks over
// (fts3_write.c:2790-2801, :2938), so skipping it would accept a doclist C
// declines as FTS_CORRUPT_VTAB. fts3MergeDoclist repeats it for decoded terms.
func fts3DoclistTerminatorOK(doclist []byte, nDoclist int) bool {
	return nDoclist > 0 && nDoclist <= len(doclist) && doclist[nDoclist-1] == 0
}

// fts3MergeDoclist folds one segment's doclist for term into byTerm. Segments
// arrive newest first, so an existing (term, docid) is skipped, and an empty
// position list is recorded as a tombstone.
//
// doclist is not trimmed to nDoclist: it runs to the end of a padded copy of
// the node (TestFts3DoclistOverrunsIntoNextTermBytes). C's position-list scan
// (fts3PoslistCopy, fts3.c:2099; fts3_write.c:1538) has no bound: "while( *p |
// c ) c = *p++ & 0x80;". The zero padding (fts3NodePadding) stops it within a
// byte or two. nDoclist is used only where C uses it: the check below
// (fts3_write.c:1450-1453) and pEnd, the end of docids (fts3_write.c:1526,
// :1556, :1562-1564).
//
// desc is "order=desc": docids descend and deltas are subtracted
// (fts3GetDeltaVarint3's bDescIdx branch).
//
// allowByteLengthMatch covers a non-empty position list holding no actual
// position (len(cols) == 0 below). Only the read path (fts3LoadIndex) allows
// it, via fts3ByteLengthMatchCol. The write path (fts3ReadSegmentInto, for
// merge/optimize/rebuild) declines: fts3PendingTermsFrom rebuilds doclists from
// decoded positions only (fts3_merge.go), so tolerating it would drop the
// document from the rewritten segment.
func fts3MergeDoclist(byTerm map[string]fts3TermPostings, term string, doclist []byte, nDoclist int, desc bool, allowByteLengthMatch bool) error {
	tp := byTerm[term]
	if tp == nil {
		tp = fts3TermPostings{}
		byTerm[term] = tp
	}
	// pEnd, in fts3_write.c's own naming: &aDoclist[nDoclist]. Kept here as an
	// int offset into doclist rather than a pointer. A doclist handed to this
	// function (whether through fts3DecodeLeafNode's padding or directly, e.g.
	// from a test) is never shorter than its own declared length -- checked
	// defensively rather than assumed, since a bounds violation here would
	// panic rather than merely mis-decode.
	pEnd := nDoclist
	if pEnd <= 0 || pEnd > len(doclist) {
		return fmt.Errorf("fts3: corrupt doclist")
	}
	// fts3_write.c:1450-1453: the byte at the declared boundary must be
	// 0x00, checked before any scanning. A doclist may pass this and still
	// have its scan run past pEnd (fts3corrupt4.test 39.0). C skips the
	// check while a node loads incrementally (nPopulate != 0); nodes here
	// are always fully loaded, so it always runs.
	if doclist[pEnd-1] != 0 {
		return fmt.Errorf("fts3: corrupt doclist (declared doclist does not end in 0x00)")
	}
	var docid int64
	off := 0
	first := true
	for off < pEnd {
		delta, n := fts3GetVarint(doclist[off:])
		if n == 0 {
			return fmt.Errorf("fts3: corrupt doclist")
		}
		off += n
		// The FIRST docid is absolute whichever way the list runs -- the same
		// *pbFirst rule the encoder writes it under (fts3_index.go).
		if desc && !first {
			docid -= int64(delta)
		} else {
			docid += int64(delta)
		}
		first = false
		// A position list ends at a byte scan, not a varint parse: C's
		// "while( *p | c ) c = *p++ & 0x80;" (fts3.c:2099; fts3_write.c:1538)
		// stops at the first 0x00 not continuing a varint. For minimal varints
		// the two agree; for a hand-written %_segdir the scan decides:
		//
		//	doclist 01 02 00 00 03 02 00 -> docids 1 and 4
		//	doclist 01 01 00 05 02 00    -> docids 1 and 6
		//
		// (a varint walk answers docid 1 alone). The first blob also shows the
		// scan may end mid-header: a POS_COLUMN (0x01) whose argument is 0x00
		// ends the list. Bounded by len(doclist), the padded node, not pEnd.
		start := off
		var c byte
		for off < len(doclist) && (doclist[off]|c) != 0 {
			c = doclist[off] & 0x80
			off++
		}
		if off >= len(doclist) {
			// Ran off the end of the padded buffer with no terminator found.
			// fts3DecodeLeafNode always pads by fts3NodePadding, and that
			// padding is always enough (see the function doc comment), so this
			// is here only as a never-panic backstop against a doclist handed
			// in some other way -- e.g. directly by a test -- with too little
			// or no padding at all.
			return fmt.Errorf("fts3: corrupt doclist (unterminated position list)")
		}
		poslist := doclist[start:off]
		off++ // step over the terminating 0x00
		// ...and then SKIP any further zero bytes before the next docid delta
		// (fts3's own "while( p<pEnd && *p==0 ) p++;", fts3_write.c:1556) --
		// bounded by pEnd, NOT by len(doclist): a skip that starts already
		// past pEnd (this document's position list overran its term's own
		// declared window) must not advance at all, matching fts3_write.c's
		// very next check, "if( p>=pEnd )" (:1562-1564), which ends this
		// term's doclist right there rather than looking for a further zero.
		// A delta is never 0 in a doclist fts3 wrote -- docids strictly
		// increase -- so within pEnd this only ever fires on a hand-written
		// blob, which is exactly where it is the difference between finding
		// the next document and stopping early.
		for off < pEnd && doclist[off] == 0 {
			off++
		}
		cols, err := fts3DecodePoslist(poslist)
		if err != nil {
			return err
		}
		if _, seen := tp[docid]; seen {
			continue // an older segment's copy of a pair already resolved
		}
		if len(poslist) == 0 {
			tp[docid] = nil // delete marker: tombstone, never a match
			continue
		}
		if len(cols) == 0 {
			// Position-list BYTES with no position in them. fts3 decides
			// match-ness by the sublist's byte LENGTH, not by its decoded
			// position count, so such a document IS a hit for a bare "t MATCH
			// 'xx'" while being invisible to a column filter -- verified
			// against the oracle on the "01 01 00 05 02 00" blob above, where
			// MATCH 'xx' gives 1 and 6 but MATCH 'a:xx' gives 6 alone.
			// Unreachable for any doclist fts3 or this engine WRITES: a
			// non-empty position list always names at least one position, so
			// this only ever fires on a hand-written blob.
			if !allowByteLengthMatch {
				// The write path (see allowByteLengthMatch's own comment):
				// fts3TermPostings is docid -> column -> positions and cannot
				// express a document that matches with no position anywhere
				// AND still round-trip through a re-encode, so this is
				// DECLINED rather than silently dropped (which is the same
				// missing row the delimiter bug used to produce, or -- worse,
				// on a merge -- a document a rewritten segment quietly loses).
				return fmt.Errorf("fts3: position list with no positions (docid %d, term %q) -- fts3 counts this as a match by byte length, which this index representation cannot express", docid, term)
			}
			// The read path CAN express it: a single entry keyed
			// fts3ByteLengthMatchCol with a nil position slice. ix.lookup
			// turns that into "matched" for a bare/unfiltered lookup (same
			// rule as iColumn's own "== nCol means no filter") and excludes
			// it from a real column filter and, through the ordinary
			// prev/len(prev)==0 checks phraseDoclist and fts3NearTrim already
			// apply to a genuinely absent column, from phrase/NEAR adjacency
			// too -- there is no real position for either to test against.
			tp[docid] = map[int][]int{fts3ByteLengthMatchCol: nil}
			continue
		}
		tp[docid] = cols
	}
	return nil
}

// fts3DecodePoslist decodes one document's already-delimited position list
// (fts3MergeDoclist's byte scan owns the delimiting) into column -> positions.
// A column with no position of its own contributes no entry, so an empty result
// means the bytes named no position at all -- see fts3MergeDoclist's decline.
//
// A POS_COLUMN (0x01) header whose column argument the byte scan cut off ENDS
// the list here, matching fts3's own column walk, which reads the column varint
// only from WITHIN the list it was handed.
func fts3DecodePoslist(poslist []byte) (map[int][]int, error) {
	cols := map[int][]int{}
	col := 0
	var pos int64
	for i := 0; i < len(poslist); {
		// The BYTE decides, not the decoded value: fts3 tests "(*p&0xFE)==0"
		// before reading anything (fts3PoslistPhraseMerge's inner loop,
		// fts3.c:2391; fts3ReadNextPos, fts3.c:2192), so 0x00 ends the column
		// list and 0x01 starts a new column -- while a NON-MINIMAL varint that
		// happens to DECODE to 0 or 1 (0x80 0x00) is an ordinary delta. fts3
		// never writes one; a hand-written %_segdir blob does
		// (fts3corrupt7.test 8.0), and this used to decline it.
		if poslist[i]&0xFE == 0 {
			if poslist[i] == 0 { // POS_END
				break
			}
			i++ // POS_COLUMN
			c, n := fts3GetVarint(poslist[i:])
			if n == 0 {
				break // a dangling column header: the list ended on it
			}
			i += n
			col, pos = int(c), 0
			continue
		}
		v, n := fts3GetVarint(poslist[i:])
		if n == 0 {
			return nil, fmt.Errorf("fts3: corrupt doclist")
		}
		i += n
		// int64 and unchecked, exactly as fts3 accumulates it: "*pVal += iVal"
		// on a sqlite3_int64 (fts3GetDeltaVarint, fts3.c:493) followed by
		// "iPos -= 2". A nine-byte 0xFF varint is -1 there, not an error, and
		// the merge's own "if( iPos1<0 || iPos2<0 ) break" (fts3.c:2379) is
		// what a negative position means, handled just below.
		pos += int64(v) - 2
		if pos < 0 {
			// fts3.c:2379: a negative position ends this column's list for
			// every merge that reads it. Recording it would let a later
			// adjacency test see a position fts3 never compares.
			break
		}
		cols[col] = append(cols[col], int(pos))
	}
	return cols, nil
}

// lookup returns the postings of one query token: the exact term, or the union
// over every term sharing its prefix, with the column filter (iCol < nCol) and
// the '^' first-position filter applied. A nil result means the token appears
// nowhere, which makes its whole phrase match nothing.
func (ix *fts3Index) lookup(tok fts3QueryToken, iCol, nCol int) fts3TermPostings {
	lo := sort.SearchStrings(ix.terms, tok.term)
	out := fts3TermPostings{}
	for i := lo; i < len(ix.terms); i++ {
		if tok.prefix {
			if !strings.HasPrefix(ix.terms[i], tok.term) {
				break
			}
		} else if ix.terms[i] != tok.term {
			break
		}
		for docid, cols := range ix.post[i] {
			if cols == nil {
				continue // tombstone
			}
			merged := out[docid]
			if merged == nil {
				merged = map[int][]int{}
			}
			for c, ps := range cols {
				if iCol < nCol && c != iCol {
					continue
				}
				if len(ps) == 0 {
					// fts3ByteLengthMatchCol (fts3MergeDoclist's byte-length-only
					// match): c is never a real iCol, so this is only ever
					// reached unfiltered (iCol >= nCol). Still has to register
					// as present so this docid counts as a hit for a bare
					// token, matching C fts3's own "doc is in the doclist"
					// test -- with a nil position list, so a later multi-token
					// phrase merge (which requires prev/next BOTH non-empty)
					// or a NEAR trim can never mistake it for a real position.
					if _, ok := merged[c]; !ok {
						merged[c] = nil
					}
					continue
				}
				for _, p := range ps {
					if tok.first && p != 0 {
						continue
					}
					merged[c] = append(merged[c], p)
				}
			}
			if len(merged) > 0 {
				out[docid] = merged
			}
		}
		if !tok.prefix {
			break
		}
	}
	// A prefix union can leave a column's positions out of order (and with
	// duplicates, when two terms share a position -- impossible for distinct
	// terms, but cheap to be exact about). fts3PoslistMerge produces a sorted,
	// duplicate-free union, so do the same.
	for _, cols := range out {
		for c, ps := range cols {
			if len(ps) > 1 {
				sort.Ints(ps)
				k := 0
				for i := 1; i < len(ps); i++ {
					if ps[i] != ps[k] {
						k++
						ps[k] = ps[i]
					}
				}
				cols[c] = ps[:k+1]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// phraseDoclist evaluates one phrase to docid -> column -> positions OF ITS
// LAST TOKEN. Tokens are merged left to right at an exact distance of one,
// which is what fts3EvalPhraseLoad's successive fts3PoslistPhraseMerge calls
// (isExact, nDiff == 1) compute.
func (ix *fts3Index) phraseDoclist(ph *fts3QueryPhrase, nCol int) fts3TermPostings {
	if len(ph.tokens) == 0 {
		return nil
	}
	acc := ix.lookup(ph.tokens[0], ph.iColumn, nCol)
	for i := 1; i < len(ph.tokens) && acc != nil; i++ {
		next := ix.lookup(ph.tokens[i], ph.iColumn, nCol)
		if next == nil {
			return nil
		}
		merged := fts3TermPostings{}
		for docid, cols := range acc {
			ncols, ok := next[docid]
			if !ok {
				continue
			}
			out := map[int][]int{}
			for c, ps := range ncols {
				prev := cols[c]
				if len(prev) == 0 {
					continue
				}
				var keep []int
				for _, p := range ps {
					if fts3PosPresent(prev, p-1) {
						keep = append(keep, p)
					}
				}
				if len(keep) > 0 {
					out[c] = keep
				}
			}
			if len(out) > 0 {
				merged[docid] = out
			}
		}
		if len(merged) == 0 {
			return nil
		}
		acc = merged
	}
	return acc
}

// fts3PosPresent reports whether the sorted position list ps holds p.
func fts3PosPresent(ps []int, p int) bool {
	i := sort.SearchInts(ps, p)
	return i < len(ps) && ps[i] == p
}

// ---- evaluating a parsed query ----

// fts3Eval holds one query's per-phrase doclists, the phrase list in query
// order, and the per-document working state one row's test leaves behind.
//
// That working state is not bookkeeping: C fts3 EDITS each phrase's
// position list in place while testing a row (NEAR trimming, and zeroing every
// phrase of a NEAR cluster that did not match), and offsets() reports exactly
// what is left. So the same edits are recorded here, per document, in over.
type fts3Eval struct {
	nCol    int
	phrases map[*fts3Expr]fts3TermPostings
	order   []*fts3Expr // phrases, left to right -- offsets()' term numbering

	docid int64
	over  map[*fts3Expr]map[int][]int
}

// newEval loads every phrase's doclist and returns the evaluator plus the set
// of documents any phrase mentions (the only ones that can match).
func (ix *fts3Index) newEval(root *fts3Expr, nCol int) (*fts3Eval, map[int64]bool) {
	ev := &fts3Eval{nCol: nCol, phrases: map[*fts3Expr]fts3TermPostings{}}
	candidates := map[int64]bool{}
	// numbered is false inside the RIGHT operand of a NOT: sqlite3Fts3ExprIterate
	// -- which is what numbers the phrases offsets() and matchinfo() report on
	// -- does not descend there. So "two NOT four" is a ONE-phrase query as far
	// as matchinfo('p') is concerned (verified against the oracle), and a
	// phrase written after such a NOT takes the index the excluded one would
	// otherwise have had. Its doclist is still loaded: the NOT itself needs it.
	var collect func(e *fts3Expr, numbered bool)
	collect = func(e *fts3Expr, numbered bool) {
		if e == nil {
			return
		}
		if e.eType == fts3qPhrase {
			dl := ix.phraseDoclist(e.phrase, nCol)
			ev.phrases[e] = dl
			if numbered {
				ev.order = append(ev.order, e)
			}
			for docid := range dl {
				candidates[docid] = true
			}
			return
		}
		collect(e.left, numbered)
		collect(e.right, numbered && e.eType != fts3qNot)
	}
	collect(root, true)
	return ev, candidates
}

// begin resets the per-document working state before testing docid.
func (ev *fts3Eval) begin(docid int64) {
	ev.docid = docid
	ev.over = map[*fts3Expr]map[int][]int{}
}

// posOf is one phrase's position list for the document being tested, as edited
// so far by this document's own NEAR trimming/invalidation.
func (ev *fts3Eval) posOf(p *fts3Expr) map[int][]int {
	if m, ok := ev.over[p]; ok {
		return m
	}
	return ev.phrases[p][ev.docid]
}

// testExpr is fts3_snippet.c's fts3EvalTestExpr for the document begin() named.
func (ev *fts3Eval) testExpr(e *fts3Expr) bool {
	switch e.eType {
	case fts3qNear, fts3qAnd:
		hit := ev.testExpr(e.left) && ev.testExpr(e.right) && ev.nearTest(e)
		// A NEAR cluster that did not match contributes NOTHING to offsets(),
		// even for phrases inside it that DID occur in this row --
		// fts3EvalTestExpr zeroes their position lists here. Verified against
		// the oracle: "(one NEAR/0 three) OR xyz" over the row holding
		// "abc abcd abcde"/"xyz" reports only the xyz term.
		if !hit && e.eType == fts3qNear && (e.parent == nil || e.parent.eType != fts3qNear) {
			p := e
			for ; p.phrase == nil; p = p.left {
				ev.over[p.right] = nil
			}
			ev.over[p] = nil
		}
		return hit
	case fts3qOr:
		// Both sides are tested, never short-circuited: the edits the losing
		// side makes to its own position lists are what offsets() reports for
		// a row the other side matched.
		l := ev.testExpr(e.left)
		r := ev.testExpr(e.right)
		return l || r
	case fts3qNot:
		return ev.testExpr(e.left) && !ev.testExpr(e.right)
	default:
		return len(ev.posOf(e)) > 0
	}
}

// nearTest is fts3EvalNearTest: a no-op except at the ROOT of a NEAR cluster,
// where it trims every phrase's position list against its neighbours -- once
// left-to-right, then once right-to-left -- and fails if any list empties.
func (ev *fts3Eval) nearTest(e *fts3Expr) bool {
	if e.eType != fts3qNear || (e.parent != nil && e.parent.eType == fts3qNear) {
		return true
	}
	// The parser guarantees a NEAR node's right operand is a phrase and its
	// left is a phrase or another NEAR ("(a OR b) NEAR c" and "a NEAR (b OR
	// c)" are both syntax errors), so the walks below are well defined. It is
	// still checked rather than asserted: a panic is this engine's hardest
	// failure, and a cluster this cannot walk simply does not match.
	if !fts3NearClusterShapeOK(e) {
		return false
	}
	leftmost := e
	for leftmost.left != nil {
		leftmost = leftmost.left
	}
	aPos := ev.posOf(leftmost)
	nTok := len(leftmost.phrase.tokens)
	for p := leftmost.parent; p != nil && p.eType == fts3qNear; p = p.parent {
		target := p.right
		res := fts3NearTrim(p.nNear, aPos, nTok, ev.posOf(target), len(target.phrase.tokens))
		if len(res) == 0 {
			return false
		}
		ev.over[target] = res
		aPos, nTok = res, len(target.phrase.tokens)
	}

	// The second pass reads what the first left behind -- the C code edits the
	// phrases' own lists in place, which ev.over reproduces.
	aPos = ev.posOf(e.right)
	nTok = len(e.right.phrase.tokens)
	for p := e.left; p != nil; p = p.left {
		nNear := p.parent.nNear
		target := p
		if p.eType == fts3qNear {
			target = p.right
		}
		res := fts3NearTrim(nNear, aPos, nTok, ev.posOf(target), len(target.phrase.tokens))
		if len(res) == 0 {
			return false
		}
		ev.over[target] = res
		aPos, nTok = res, len(target.phrase.tokens)
	}
	return true
}

// fts3NearClusterShapeOK reports whether every node of the NEAR cluster rooted
// at e has the shape nearTest walks: a right operand that is a phrase, and a
// left operand that is a phrase or another NEAR.
func fts3NearClusterShapeOK(e *fts3Expr) bool {
	for p := e; ; p = p.left {
		if p.eType == fts3qPhrase {
			return p.phrase != nil
		}
		if p.eType != fts3qNear || p.left == nil || p.right == nil ||
			p.right.eType != fts3qPhrase || p.right.phrase == nil {
			return false
		}
	}
}

// fts3NearTrim is fts3EvalNearTrim: keep only those positions of target that
// lie within nNear intervening tokens of a position in aPos, IN THE SAME
// COLUMN, on either side. Positions are of each phrase's LAST token, so the
// allowed gap on the "aPos first" side is nNear+nTokTarget and on the
// "target first" side nNear+nTokA -- the two nParam values fts3EvalNearTrim
// computes.
func fts3NearTrim(nNear int, aPos map[int][]int, nTokA int, target map[int][]int, nTokTarget int) map[int][]int {
	nParam1 := nNear + nTokTarget
	nParam2 := nNear + nTokA
	out := map[int][]int{}
	for c, qs := range target {
		ps := aPos[c]
		if len(ps) == 0 {
			continue
		}
		var keep []int
		for _, q := range qs {
			near := false
			for _, p := range ps {
				if (q > p && q-p <= nParam1) || (p > q && p-q <= nParam2) {
					near = true
					break
				}
			}
			if near {
				keep = append(keep, q)
			}
		}
		if len(keep) > 0 {
			out[c] = keep
		}
	}
	return out
}

// ---- the SQL MATCH operator ----

// fts3MatchScope is a resolved "X MATCH ..." left operand: the FROM scope that
// is an fts3/fts4 table, that table's real name and column names, and the
// column index the query defaults to (len(cols) for the whole-table form).
type fts3MatchScope struct {
	scope *tableScope
	table string
	cols  []string
	// isFts4 is the MODULE (which decides the query language and whether %_stat
	// exists); hasDocsize is the TABLE (a "matchinfo=fts3" fts4 table has none).
	// Keeping them apart is what makes matchinfo()'s 'n'/'a' answerable on such
	// a table while 'l' is not -- see fts3MatchinfoCheckFormat.
	isFts4     bool
	hasDocsize bool
	iDefault   int
	tok        *fts3Tokenizer
	// langidCol is the DECLARED name of the "languageid=" hidden column, "" for
	// a table without the option, and langid the language this MATCH searches
	// -- 0 unless a top-level "<langidCol> = <integer>" conjunct names another
	// one. See fts3_langid.go for why an unconstrained MATCH searches language
	// 0 alone, and nIndex for why the language shifts every %_segdir level.
	langidCol string
	langid    int64
	nIndex    int
	// hasContent marks a "content=" table, whose index legitimately holds
	// docids its content table does not (fts3_content.go) -- so the
	// missing-row decline below does not apply to it.
	hasContent bool
	// dbIdx is the scope's own database (tableScope.dbIdx): every shadow-table
	// read this MATCH makes has to go to it, not to the pager the statement
	// was compiled against. Carried here rather than read back off scope so
	// the compiled-path payloads (matchCompileInfo, fts3AuxCompileInfo) keep
	// it too.
	dbIdx int
}

// pagerIn resolves the database this MATCH's shadow tables live in, given the
// pager the statement is running against. nil when that database is not
// reachable from base, which every caller declines on.
func (ms fts3MatchScope) pagerIn(base *ReadOnlyPager) *ReadOnlyPager {
	return base.forDB(ms.dbIdx)
}

// termIndexBase is the first %_segdir level of this MATCH's language's TERM
// index -- getAbsoluteLevel(p, langid, 0, 0).
func (ms fts3MatchScope) termIndexBase() int64 {
	return fts3LevelBase(ms.langid, ms.nIndex, 0)
}

// fts3ScopeInfo returns the fts3/fts4 module facts about scope t, if it is one.
func fts3ScopeInfo(p *ReadOnlyPager, t *tableScope) (meta *fts3TableMeta, ok bool) {
	if p == nil {
		return nil, false
	}
	// A source in an ATTACHed database is looked up in THAT database: p is the
	// pager compiling the statement, and an fts table's %_segdir/%_segments
	// live beside it, so "SELECT rowid FROM two.t3 WHERE t3 MATCH 'hello'"
	// must read two's index. Resolving it against p instead found main's
	// same-named table and answered 0 rows where C SQLite answers 1 (see
	// tableScope.dbIdx). A dbIdx this pager cannot serve yields a nil pager,
	// which reports "not an fts table" and declines the MATCH cleanly.
	p = p.forDB(t.dbIdx)
	if p == nil {
		return nil, false
	}
	name := t.tableName
	if name == "" {
		name = t.name
	}
	return p.fts3TableInfo(name)
}

// fts3TableInfo reports whether name is an fts3/fts4 virtual table in this read
// snapshot, and if so its column names. Cached on the pager for the same
// reason isFts5Table's set is: a ReadOnlyPager is a fixed schema snapshot.
func (p *ReadOnlyPager) fts3TableInfo(name string) (meta *fts3TableMeta, ok bool) {
	if p == nil {
		return nil, false
	}
	if p.fts3Tables == nil {
		p.fts3Tables = map[string]*fts3TableMeta{}
		if rows, err := p.Schema(); err == nil {
			for _, r := range rows {
				if r.Type != "table" || !isCreateVirtualTableSQL(r.SQL) {
					continue
				}
				_, mod, args, _, perr := parseCreateVirtualTableStmt(r.SQL)
				if perr != nil {
					continue
				}
				m, mok := lookupVtabModule(mod)
				if !mok {
					continue
				}
				fm, fok := m.(fts3Module)
				if !fok {
					continue
				}
				sch, cerr := fm.parseSchemaWith(args, p.fts3Catalog())
				if cerr != nil {
					continue
				}
				p.fts3Tables[strings.ToLower(r.Name)] = &fts3TableMeta{cols: sch.cols, isFts4: fm.isFts4, hasDocsize: sch.hasDocsize, tok: sch.tok, langidCol: sch.langid, nIndex: sch.nIndex(), hasContent: sch.hasContent, descIdx: sch.descIdx}
			}
		}
	}
	m := p.fts3Tables[strings.ToLower(name)]
	if m == nil {
		return nil, false
	}
	return m, true
}

// fts3TableMeta is the cached per-table shape fts3TableInfo hands out.
type fts3TableMeta struct {
	cols       []string
	isFts4     bool
	hasDocsize bool
	tok        *fts3Tokenizer
	langidCol  string
	nIndex     int
	hasContent bool
	// descIdx is "order=desc" (fts3Table.bDescIdx): the stored doclists
	// descend and the table's rows come back newest-docid first.
	descIdx bool
}

// fts3ResolveMatchTarget resolves a MATCH left operand against table scopes.
// C fts3 accepts:
//
//   - the table name, bare or qualified: fts3 declares a hidden column named
//     after the table, so "FROM t AS x WHERE t MATCH 'q'" works and "x MATCH
//     'q'" is "no such column: x";
//   - one of the table's text columns, which confines the query to it.
//
// "docid MATCH q" is not accepted ("unable to use function MATCH in the
// requested context"), so the hidden docid column is skipped.
func fts3ResolveMatchTarget(p *ReadOnlyPager, scopes []tableScope, e Expr) (fts3MatchScope, bool) {
	ce, isCol := e.(ColumnExpr)
	if !isCol {
		return fts3MatchScope{}, false
	}
	for i := range scopes {
		t := &scopes[i]
		if ce.Qualifier != "" && !strings.EqualFold(t.name, ce.Qualifier) {
			continue
		}
		meta, ok := fts3ScopeInfo(p, t)
		if !ok {
			continue
		}
		cols := meta.cols
		name := t.tableName
		if name == "" {
			name = t.name
		}
		// A user column wins over the table-named hidden column, matching the
		// declaration order fts3DeclareVtab emits.
		for j, c := range cols {
			if strings.EqualFold(c, ce.Name) {
				return fts3MatchScope{scope: t, table: name, cols: cols, isFts4: meta.isFts4, hasDocsize: meta.hasDocsize, tok: meta.tok, iDefault: j, langidCol: meta.langidCol, nIndex: meta.nIndex, hasContent: meta.hasContent, dbIdx: t.dbIdx}, true
			}
		}
		if strings.EqualFold(name, ce.Name) {
			return fts3MatchScope{scope: t, table: name, cols: cols, isFts4: meta.isFts4, hasDocsize: meta.hasDocsize, tok: meta.tok, iDefault: len(cols), langidCol: meta.langidCol, nIndex: meta.nIndex, hasContent: meta.hasContent, dbIdx: t.dbIdx}, true
		}
	}
	return fts3MatchScope{}, false
}

// fts3MatchPattern extracts the query string of a MATCH whose pattern is a
// LITERAL, which is the only form that can be resolved at PREPARE time so a
// malformed one is rejected even when the table holds no rows (see
// compileMatchExpr). A NULL literal is the empty query, which C fts3 also
// answers with no rows rather than an error. Any other expression is reported
// as not-a-literal and left to the caller's own pattern REGISTER (the
// compiled routes) -- the one route that used to arrive
// holding an expression).
func fts3MatchPattern(e Expr) (text string, isNull bool, ok bool) {
	lit, isLit := e.(LiteralExpr)
	if !isLit {
		return "", false, false
	}
	if lit.Val.Typ == Null {
		return "", true, true
	}
	return valueToText(lit.Val), false, true
}

// fts3MatchQueryOfValue is what C fts3 does with the value it is handed:
//
//	fts3.c:3365  const char *zQuery = (const char *)sqlite3_value_text(pCons);
//
// The conversion is sqlite3_value_text's, not a cast: an INTEGER query is its
// decimal text and a BLOB its bytes read as text, both of which then go
// through the ordinary query parser. NULL is the one special case -- fts3
// leaves pCsr->pExpr NULL and the table matches nothing, exactly as it does
// for a NULL literal.
func fts3MatchQueryOfValue(v Value) (text string, isNull bool) {
	if v.Typ == Null {
		return "", true
	}
	return valueToText(v), false
}

// fts3MatchTargetHasRow reports whether ms's ROW SOURCE holds at least one
// row -- the condition under which a per-row MATCH test runs at least as often
// as C fts3's xFilter does (see checkFts3Match). It stops at the first row
// rather than reading the table.
//
// A "content=" table answers false: its rows come from another table, or from
// the index alone for a contentless one (fts3_content.go), and neither is
// resolvable from a scope this function was handed. That only ever makes the
// caller decline.
func (p *ReadOnlyPager) fts3MatchTargetHasRow(ms fts3MatchScope) bool {
	if p == nil || ms.hasContent {
		return false
	}
	root, err := p.TableRoot(ms.table + "_content")
	if err != nil {
		return false
	}
	seq, errFn := p.ScanTable(root)
	found := false
	for range seq {
		found = true
		break
	}
	return errFn() == nil && found
}

// checkFts3Match settles everything about an fts3/fts4 MATCH that can be
// settled at PREPARE time -- which is everything except which rows it selects.
// A per-row check would never run at all on an empty table, and C SQLite
// rejects a malformed query (or a MATCH in a place its module cannot see)
// whether or not the table holds a row.
func (c *compiler) checkFts3Match(ms fts3MatchScope, x MatchExpr) error {
	idx, ok := c.fts3MatchScopeIndex(x.X)
	if !ok {
		return fmt.Errorf("engine: unable to use function MATCH in the requested context")
	}
	if _, bound := c.fts3MatchGood[idx]; !bound {
		return fmt.Errorf("engine: unable to use function MATCH in the requested context")
	}
	query, isNull, ok := fts3MatchPattern(x.Pattern)
	if !ok {
		// A non-literal query is settled per row from the register
		// compileMatchExpr codes, as C's xFilter parses the query on every
		// call; a malformed one errors only once a row reaches it.
		//
		// Exception: C calls xFilter once per outer row, while this tests the
		// MATCH per (fts row x outer row). Over an empty fts table C still
		// parses the query and raises "malformed MATCH expression" for a bad
		// one, while this would answer no rows, so an empty target is
		// declined.
		if !ms.pagerIn(c.pager).fts3MatchTargetHasRow(ms) {
			return fmt.Errorf("%w: MATCH against an empty %s table with a non-literal query string (C fts3 parses the query once per outer row, so a malformed one is an error there even with no rows to search)", errVDBEUnsupported, fts3ModuleWord(ms.isFts4))
		}
		return nil
	}
	if isNull {
		// A NULL query is not parsed at all: C fts3 leaves the expression
		// empty and the table matches nothing.
		return nil
	}
	mp := ms.pagerIn(c.pager)
	if mp == nil {
		return fmt.Errorf("engine: unable to use function MATCH in the requested context")
	}
	// RESOLVE THE WHOLE QUERY NOW, not on first use. Two failures are only
	// reachable this way: a malformed query, and an index this reader declines
	// (below) -- both of which C SQLite raises whether or not the table
	// holds a row, while a per-row check over an EMPTY table would silently
	// answer "no rows". The result is memoized on the pager, so the rows this
	// statement then tests cost nothing.
	//
	// c.fts3ContentUnneeded[idx] gates fts3MatchDocids' OWN %_content
	// existence check -- see fts3StmtContentUnneeded's doc comment for the
	// rule this is proven safe under.
	_, err := mp.fts3MatchDocids(ms, query, c.fts3ContentUnneeded[idx])
	return err
}

// fts3MatchResult is one resolved MATCH query over one table: the loaded
// index/expression state, and the docid set it selects.
type fts3MatchResult struct {
	root   *fts3Expr
	ev     *fts3Eval
	docids map[int64]bool
	table  string
	isFts4 bool
	tok    *fts3Tokenizer

	// global memoizes matchinfo()'s 'x' whole-query statistics, which are the
	// same for every row (that is what makes them "global") and cost a pass
	// over every phrase's whole doclist to compute.
	global [][2][]uint32

	// nodeSets memoizes fts3NodeCandidateSet's per-node result (every node
	// reached from root, not just leaves): see that function's comment.
	nodeSets map[*fts3Expr]map[int64]bool
}

// fts3MatchDocids returns the docids of table matching query, computed from
// the index and memoized on this snapshot.
//
// A matched docid with no %_content row is declined: C drives rows from the
// index and fails the content lookup ("database disk image is malformed" for
// "SELECT docid, a ...", while count(*) still counts it). This engine reads
// rows from %_content (materializeFts3), so it can reproduce neither
// (fts3corrupt4/fts3corrupt6).
//
// A "content=" table is exempt: fts3CursorSeek raises SQLITE_CORRUPT_VTAB
// only when zContentTbl is NULL and otherwise returns an all-NULL row, which
// materializeFts3Content builds (fts3_content.go).
//
// contentUnneeded lifts the decline when the caller proved
// (fts3StmtContentUnneeded) that no real column of such a row is read, so
// fts3CursorSeek (fts3.c:1839) never runs: "SELECT rowid ...", "SELECT
// count(*) ...". Only checkFts3Match and compileFts3Aux (for matchinfo()) pass
// true.
func (p *ReadOnlyPager) fts3MatchDocids(ms fts3MatchScope, query string, contentUnneeded bool) (*fts3MatchResult, error) {
	key := fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%v", strings.ToLower(ms.table), ms.iDefault, ms.langid, query, contentUnneeded)
	if p.fts3Matches != nil {
		if s, ok := p.fts3Matches[key]; ok {
			return s, nil
		}
	}
	root, err := fts3ParseQuery(query, ms.cols, ms.isFts4, ms.iDefault, ms.tok)
	if err != nil {
		return nil, err
	}
	// root is already fully parsed here, so its token set is exactly what
	// C fts3's own per-token readers would open (fts3.c:4491) -- narrowing
	// fts3LoadIndex's decode to it is what makes THIS caller's read lazy.
	ix, err := p.fts3LoadIndex(ms.table, ms.termIndexBase(), fts3GatherWantedTerms(root))
	if err != nil {
		return nil, err
	}
	ev, candidates := ix.newEval(root, len(ms.cols))
	set := map[int64]bool{}
	for docid := range candidates {
		ev.begin(docid)
		if ev.testExpr(root) {
			set[docid] = true
		}
	}
	if len(set) > 0 && !ms.hasContent && !contentUnneeded {
		rowids, _, cerr := p.Rows(ms.table + "_content")
		if cerr != nil {
			return nil, cerr
		}
		have := make(map[int64]bool, len(rowids))
		for _, r := range rowids {
			have[int64(r)] = true
		}
		for docid := range set {
			if !have[docid] {
				return nil, fmt.Errorf("engine: %s table %s: its index matches docid %d, which %s_content does not hold; this engine will not guess what C fts3 returns for it", fts3ModuleWord(ms.isFts4), ms.table, docid, ms.table)
			}
		}
	}
	res := &fts3MatchResult{root: root, ev: ev, docids: set, table: ms.table, isFts4: ms.isFts4, tok: ms.tok}
	if p.fts3Matches == nil {
		p.fts3Matches = map[string]*fts3MatchResult{}
	}
	p.fts3Matches[key] = res
	return res, nil
}

// evalMatchExprLangid resolves a MATCH on the compiled path, carrying the
// LANGUAGE an fts4 "languageid=" MATCH searches -- which only a compile that
// saw the whole WHERE clause can resolve (fts3_langid.go) --
// contentUnneeded, compiler.fts3ContentUnneeded for this MATCH's own resolved
// scope (matchCompileInfo.contentUnneeded, set by compileMatchExpr), and pat,
// the QUERY VALUE the OpMatch body read out of the register the compiler coded
// the pattern into. Both destinations take it: fts3's own matcher below and
// fts5's evalMatch (fts5_match.go).
func evalMatchExprLangid(ctx *evalCtx, x MatchExpr, langid int64, contentUnneeded bool, pat Value) (Value, error) {
	if ms, ok := fts3ResolveMatchTarget(ctx.pager, ctx.tables, x.X); ok {
		ms.langid = langid
		return evalFts3Match(ctx, ms, x, contentUnneeded, pat)
	}
	return evalMatch(ctx, x, pat)
}

// evalFts3Match evaluates "X MATCH Pattern" for an fts3/fts4 table by looking
// the current row's docid up in the index-derived match set. pat is the
// pattern's value, as C's xFilter receives it (fts3.c:3365); every caller
// supplies it, so x.Pattern is not read.
func evalFts3Match(ctx *evalCtx, ms fts3MatchScope, x MatchExpr, contentUnneeded bool, pat Value) (Value, error) {
	query, isNull := fts3MatchQueryOfValue(pat)
	matched := false
	if !isNull {
		// The index lives in the SCOPE's database, which is not the pager
		// this statement runs against when the fts table came from an ATTACH.
		mp := ms.pagerIn(ctx.pager)
		if mp == nil {
			return Value{}, fmt.Errorf("engine: unable to use function MATCH in the requested context")
		}
		res, err := mp.fts3MatchDocids(ms, query, contentUnneeded)
		if err != nil {
			return Value{}, err
		}
		docid, dok := fts3ScopeDocid(ctx, ms.scope)
		if !dok {
			return Value{}, fmt.Errorf("engine: unable to use function MATCH in the requested context")
		}
		matched = res.docids[docid]
	}
	if x.Not {
		matched = !matched
	}
	if matched {
		return Value{Typ: Int, I: 1}, nil
	}
	return Value{Typ: Int, I: 0}, nil
}

// ---- offsets() ----

// compileFts3Aux compiles an fts3/fts4 auxiliary function call. ok is false
// when the argument does not name an fts3 table, leaving the caller to reject
// the name. Rules, as C:
//
//   - the argument must be the table's hidden column: "offsets(a)" over a text
//     column is "illegal first argument to offsets";
//   - exactly one argument ("wrong number of arguments to function
//     offsets()");
//   - with no MATCH on the table, the value is '' for every row.
func (c *compiler) compileFts3Aux(x FuncExpr) (int, bool, error) {
	scopes := make([]tableScope, len(c.scopes))
	cursors := make([]int, len(c.scopes))
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
		cursors[i] = s.cursor
	}
	fn := strings.ToLower(x.Name)
	maxArgs := 1
	switch fn {
	case "matchinfo":
		maxArgs = 2 // the optional format string
	case "snippet":
		maxArgs = 6 // the two markers, the ellipsis, the column and the token budget
	}
	if len(x.Args) < 1 || len(x.Args) > maxArgs {
		// Resolve nothing: an arity error here has to look the same whether
		// or not the argument would have named an fts3 table, and real
		// SQLite raises it before ever looking at the argument.
		for i := range scopes {
			if _, isFts3 := fts3ScopeInfo(c.pager, &scopes[i]); isFts3 {
				return 0, true, fmt.Errorf("engine: wrong number of arguments to function %s()", x.Name)
			}
		}
		return 0, false, nil
	}
	ms, idx, ok := fts3ResolveMatchScopeIndex(c.pager, scopes, x.Args[0])
	if !ok {
		return 0, false, nil
	}
	ms, lerr := c.fts3WithLangid(ms)
	if lerr != nil {
		return 0, true, lerr
	}
	if ms.iDefault != len(ms.cols) {
		return 0, true, fmt.Errorf("engine: illegal first argument to %s", x.Name)
	}
	// snippet()'s trailing arguments are ORDINARY expressions C fts3 reads
	// with sqlite3_value_text()/sqlite3_value_int() once per row, so they are
	// compiled to registers here rather than required to be literals (the way
	// matchinfo()'s format string is, which this engine must resolve at prepare
	// time because it decides whether the statement is answerable at all).
	var argRegs []int
	if fn == "snippet" {
		for _, a := range x.Args[1:] {
			reg, cerr := c.compileExpr(a)
			if cerr != nil {
				return 0, true, cerr
			}
			argRegs = append(argRegs, reg)
		}
	}
	// No MATCH bound to THIS table's scope: the cursor has no query, so every
	// row's value is the empty string (or, for matchinfo(), the empty BLOB --
	// see below). This is decided BEFORE the format string is even looked at,
	// which matters: matchinfo()'s format argument is not read at all over an
	// empty cursor, verified against the oracle with an unrecognized
	// character, an out-of-range 'l'/'n'/'a' directive for the table's own
	// module, a non-literal (column-valued) format, and NULL -- all four are
	// the empty blob there, none reaching this engine's own prepare-time
	// format checks below.
	solo, bound := c.fts3MatchGood[idx]
	empty := !bound
	var query string
	var patternExpr Expr
	if bound {
		if solo == nil {
			// The scope's slot is an OR of two or more MATCH queries (see
			// fts3MatchBindings) -- C fts3's own cursor there runs the
			// OR-optimization's two separate index probes rather than
			// opening one query, and no oracle evidence pins what its aux
			// functions would then report over it, so this declines rather
			// than guess.
			return 0, true, fmt.Errorf("%w: %s() over a MATCH bound as an OR of more than one query", errVDBEUnsupported, x.Name)
		}
		// solo.X was resolved to idx by fts3MatchBindings against a
		// DIFFERENT []tableScope copy (vdbe_scan.go's tableScopesOf(srcs)) --
		// resolve it again here, against compileFts3Aux's OWN copy, exactly
		// as fts3MatchScopeIndex does for checkFts3Match. This should never
		// disagree; fail closed rather than report on the wrong table if it
		// somehow does.
		qms, qidx, qok := fts3ResolveMatchScopeIndex(c.pager, scopes, solo.X)
		if !qok || qidx != idx {
			return 0, true, fmt.Errorf("engine: unable to use function MATCH in the requested context")
		}
		var isNull, lit bool
		query, isNull, lit = fts3MatchPattern(solo.Pattern)
		switch {
		case !lit:
			// A non-literal query is settled per row from the register coded
			// below (fts3AuxCompileInfo.patReg), as checkFts3Match does: C re-parses
			// it into pCsr->pExpr on each xFilter call (fts3.c:3363-3378) and the
			// aux functions only test pCsr->pExpr (fts3_snippet.c:1754-1776,
			// 1608-1623, 1440, 1466-1469). The scope is still resolved here.
			if qms, lerr = c.fts3WithLangid(qms); lerr != nil {
				return 0, true, lerr
			}
			ms = qms
			// Same empty-target-table exception as checkFts3Match: this
			// engine's pair-loop execution model never runs a per-row test
			// at all over zero fts rows, while C fts3's xFilter still
			// parses the query once per outer row and would raise a
			// malformed-query error independent of index content.
			if !ms.pagerIn(c.pager).fts3MatchTargetHasRow(ms) {
				return 0, true, fmt.Errorf("%w: %s() over a MATCH against an empty %s table with a non-literal query string (C fts3 parses the query once per outer row, so a malformed one is an error there even with no rows to search)", errVDBEUnsupported, x.Name, fts3ModuleWord(ms.isFts4))
			}
			patternExpr = solo.Pattern
		case isNull:
			empty = true
		default:
			// The auxiliary function reports on the CURSOR's query, so it
			// takes that MATCH's scope -- including which language it
			// searches (fts3_langid.go).
			if qms, lerr = c.fts3WithLangid(qms); lerr != nil {
				return 0, true, lerr
			}
			ms = qms
		}
	}
	format := fts3MatchinfoDefault
	if fn == "matchinfo" && !empty {
		if len(x.Args) == 2 {
			lit, isLit := x.Args[1].(LiteralExpr)
			switch {
			case isLit && lit.Val.Typ != Null:
				format = valueToText(lit.Val)
			case isLit:
				// A literal NULL: sqlite3_value_text() of it is NULL, same as
				// the pointer-column case below, so it falls back to the same
				// default -- verified against the oracle ("matchinfo(t0,
				// NULL) = matchinfo(t0)" is true). format stays
				// fts3MatchinfoDefault.
			default:
				// A non-literal format string is decided per row in C
				// (fts3.c:3832-3836), except a reference to the table's own hidden
				// pointer column, the same column as x.Args[0]: its value is
				// sqlite3_result_pointer's MEM_Null (vdbeapi.c:539-555), and a NULL
				// format is FTS3_MATCHINFO_DEFAULT (fts3_snippet.c:1754-1766).
				// fts3corrupt4.test 44.2's "matchinfo(t0, t0)". Other non-literal
				// formats decline.
				fqms, fidx, fok := fts3ResolveMatchScopeIndex(c.pager, scopes, x.Args[1])
				if !fok || fidx != idx || fqms.iDefault != len(fqms.cols) {
					return 0, true, fmt.Errorf("%w: matchinfo() with a non-literal format string", errVDBEUnsupported)
				}
				// format stays fts3MatchinfoDefault.
			}
		}
		if err := fts3MatchinfoCheckFormat(format, ms.isFts4, ms.hasDocsize); err != nil {
			return 0, true, err
		}
	}
	// contentUnneeded (fts3AuxCompileInfo.contentUnneeded, consulted at
	// runtime by evalFts3Aux/evalFts3Snippet) stays false -- the strict/safe
	// default -- for both "empty" branches: neither ever calls
	// fts3MatchDocids at all (offsets()/matchinfo() answer their fixed
	// no-query value directly; snippet() short-circuits on info.noQuery
	// before it would). Only the "default" (bound, non-empty) case below can
	// set it true, and only for matchinfo().
	var contentUnneeded bool
	switch {
	case empty && fn == "offsets":
		d := c.alloc()
		c.emit(Instruction{Op: OpString8, P4: "", P2: d})
		return d, true, nil
	case empty && fn == "matchinfo":
		// matchinfo() over a cursor with no query is the EMPTY BLOB (X''),
		// for EVERY format string -- verified against the oracle across
		// every directive this engine serves ('p','c','n','a','l','s','x'
		// and the default "pcx"): unlike offsets()'s empty TEXT "", the
		// result does not vary with the format at all once there is no
		// query to report on.
		d := c.alloc()
		c.emit(Instruction{Op: OpBlob, P2: d, P4: []byte{}})
		return d, true, nil
	case empty:
		// snippet() over a cursor with no query IS the empty string -- but
		// only after it has read its arguments, because a NULL marker is an
		// error there and a zero token budget is the empty string whether or
		// not a query exists (both verified against the oracle). So it is
		// emitted like any other call, with no query to report on.
	default:
		// Only matchinfo() may skip fts3MatchDocids' existence check: it never
		// seeks %_content (fts3MatchinfoFunc, fts3.c:3822-3836), while offsets()
		// and snippet() always do (fts3.c:3756, :3776). The function test is a
		// second guard beside c.fts3ContentUnneeded.
		unneeded := fn == "matchinfo" && c.fts3ContentUnneeded[idx]
		if patternExpr == nil {
			// A LITERAL query is known now, so resolve it NOW: a malformed
			// query or an index this reader declines (fts3MatchDocids) is
			// only reachable this way -- both of which C SQLite raises
			// whether or not the table holds a row, while deferring this to
			// the per-row runtime call would silently answer "no rows" over
			// an empty table instead. A NON-LITERAL query has no fixed
			// string to resolve here at all; it is instead evaluated once
			// per row by evalFts3Aux/evalFts3Snippet, exactly the way
			// evalFts3Match already does for the plain MATCH test.
			mp := ms.pagerIn(c.pager)
			if mp == nil {
				return 0, true, fmt.Errorf("engine: unable to use function %s in the requested context", x.Name)
			}
			if _, err := mp.fts3MatchDocids(ms, query, unneeded); err != nil {
				return 0, true, err
			}
		}
		contentUnneeded = unneeded
	}
	// A non-literal query is CODED into a register here, immediately before
	// the OpFts3Aux that reads it back as a value -- the same place, and for
	// the same reason, as the plain MATCH test's own pattern register
	// (compileMatchExpr, vdbe_codegen.go; wherecode.c:1584's
	// "        codeExprOrVector(pParse, pRight, iTarget, 1);"). It is
	// recomputed on every execution of the opcode, i.e. once per row, which is
	// the frequency C fts3 re-reads apVal[0] at (fts3.c:3363-3365, once per
	// xFilter call). The opcode body therefore owns its query's value
	// semantics, and no expression tree is carried past compile time.
	var patReg int
	if patternExpr != nil {
		var perr error
		if patReg, perr = c.compileExpr(patternExpr); perr != nil {
			return 0, true, perr
		}
	}
	d := c.alloc()
	c.emit(Instruction{Op: OpFts3Aux, P2: d, P4: &fts3AuxCompileInfo{
		fn: fn, format: format, ms: ms, query: query, noQuery: empty,
		patReg: patReg, patCompiled: patternExpr != nil,
		argRegs: argRegs, scopes: scopes, cursors: cursors, contentUnneeded: contentUnneeded,
	}})
	return d, true, nil
}

// fts3AuxFuncName reports whether name is an fts3/fts4 AUXILIARY function this
// engine serves.
//
// optimize() is absent from this list because it is not an auxiliary function:
// the three here report on the CURRENT ROW of a MATCH, while optimize() reads
// no row and MUTATES the index it names. It has its own compile path and its
// own opcode -- see fts3_optimize.go, which also carries the oracle rules for
// its two answer strings.
func fts3AuxFuncName(name string) bool {
	return strings.EqualFold(name, "offsets") ||
		strings.EqualFold(name, "matchinfo") ||
		strings.EqualFold(name, "snippet")
}

// fts3AuxCompileInfo is OpFts3Aux's P4 payload: which auxiliary function is
// being called, the resolved fts3 table scope, the MATCH query being reported
// on, and the compile-time scopes/cursors needed to reassemble the current row
// (exactly like matchCompileInfo).
type fts3AuxCompileInfo struct {
	fn      string // lower-case: "offsets", "matchinfo" or "snippet"
	format  string // matchinfo()'s directive string
	ms      fts3MatchScope
	query   string
	noQuery bool // no MATCH on this table: snippet() answers the empty string

	// patCompiled is set when the bound MATCH's query is not a compile-time
	// literal: query is unset and evalFts3Aux/evalFts3Snippet read the current
	// row's query from register patReg, coded just before the opcode (as C
	// re-parses per xFilter, fts3.c:3363-3378). The flag distinguishes "not
	// compiled" from register 0, which is allocatable.
	patReg      int
	patCompiled bool

	argRegs []int // snippet()'s trailing arguments, in call order
	scopes  []tableScope
	cursors []int

	// contentUnneeded is compileFts3Aux's own resolved
	// c.fts3ContentUnneeded[idx], but ALWAYS false for offsets()/snippet()
	// regardless of that map -- see compileFts3Aux's own comment for why only
	// matchinfo() may ever set it true. Threaded to the runtime
	// fts3MatchDocids call evalFts3Aux/evalFts3Snippet each make.
	contentUnneeded bool
}

// fts3TermOffset is one entry of sqlite3Fts3Offsets' aTerm[] array: which
// phrase the term belongs to, how far back from the phrase's recorded position
// this term sits (the phrase's position list holds its LAST token), and how
// far the term's own iterator has advanced through that list.
type fts3TermOffset struct {
	phrase *fts3Expr
	iOff   int
	pos    []int
	next   int
}

// offsets renders offsets() for one row: for each occurrence of each query
// term, "<column> <term index> <byte offset> <byte length>", column by column
// and in byte order within a column. Term indices number the query's tokens
// left to right, so a two-token phrase takes two.
//
// This is sqlite3Fts3Offsets (fts3_snippet.c) with the tokenizer walk replaced
// by lookup: token position N is the column's N'th token. colText supplies the
// row's column values (ok false for NULL, which C skips).
func (r *fts3MatchResult) offsets(docid int64, nCol int, colText func(i int) (string, bool)) (Value, error) {
	if r.root == nil {
		return Value{Typ: Text, S: []byte{}}, nil
	}
	// Re-run this document's own test: the position lists offsets() reports
	// are the ones NEAR trimming and NEAR-cluster invalidation leave behind,
	// and those are per-document state.
	r.ev.begin(docid)
	r.ev.testExpr(r.root)

	var terms []fts3TermOffset
	for _, ph := range r.ev.order {
		n := len(ph.phrase.tokens)
		for i := 0; i < n; i++ {
			terms = append(terms, fts3TermOffset{phrase: ph, iOff: n - i - 1})
		}
	}

	var out []byte
	for iCol := 0; iCol < nCol; iCol++ {
		for i := range terms {
			terms[i].pos = r.ev.posOf(terms[i].phrase)[iCol]
			terms[i].next = 0
		}
		text, ok := colText(iCol)
		if !ok {
			continue
		}
		var spans []fts3TokenSpan
		tokenized := false
		for {
			iMinPos, pTerm := 0, -1
			for i := range terms {
				t := &terms[i]
				if t.next >= len(t.pos) {
					continue
				}
				if v := t.pos[t.next] - t.iOff; pTerm < 0 || v < iMinPos {
					iMinPos, pTerm = v, i
				}
			}
			if pTerm < 0 {
				break
			}
			terms[pTerm].next++
			if !tokenized {
				spans = r.tok.spans(text)
				tokenized = true
			}
			if iMinPos < 0 || iMinPos >= len(spans) {
				// Real fts3 runs its tokenizer out of tokens here and reports
				// SQLITE_CORRUPT_VTAB; reaching it means the index and
				// %_content disagree about this row's text.
				return Value{}, fmt.Errorf("engine: %s table %s: its index places a term at token %d of column %d, which the row's own text does not have", fts3ModuleWord(r.isFts4), r.table, iMinPos, iCol)
			}
			s := spans[iMinPos]
			out = append(out, fmt.Sprintf("%d %d %d %d ", iCol, pTerm, s.start, s.end-s.start)...)
		}
	}
	if len(out) == 0 {
		// fts3StringAppend never ran, so sqlite3_result_text() is handed a
		// NULL pointer -- offsets() is NULL, not the empty string.
		return Value{Typ: Null}, nil
	}
	return Value{Typ: Text, S: out[:len(out)-1]}, nil // drop the trailing space
}

// ---- matchinfo() ----

// matchinfo() directives served; any other is "unrecognized matchinfo
// request: %c", as in C.
//
//	p  1 value    number of matchable phrases in the query
//	c  1 value    number of columns
//	x  3*p*c      per phrase and column: hits in THIS row, hits in that
//	              column over every row the phrase (or its NEAR cluster)
//	              matches, and how many of those rows had at least one
//	y  p*c        per phrase and column: hits in THIS row
//	b  p*ceil(c/32)  the same, as a bitmask: bit (col&31) of word col/32
//	s  c values   per column: the longest run of query phrases that occur
//	              consecutively in it (see lcsPerColumn)
//	n  1 value    total documents in the table          (fts4 only)
//	a  c values   average column length, in tokens      (fts4 only)
//	l  c values   this row's column lengths, in tokens  (fts4 only)
//
// 'y' and 'b' come from the same per-row position lists as x's first value.
// C computes them differently (fts3ExprLHitGather vs
// sqlite3Fts3EvalPhrasePoslist); the identity was checked against the oracle
// over 35 query shapes (OR, NOT, NEAR, column filters, prefixes, absent terms)
// on fts3 and fts4.
const fts3MatchinfoDefault = "pcx"

// (The renderer below is the only place those sizes are actually produced --
// there is no separate size function to keep in step with it. Note in
// particular that 'b' is NOT nCol*nPhrase: it is one BITMASK WORD per 32
// columns per phrase, which is what fts3MatchinfoSize's LHITS_BM case says.)

// fts3MatchinfoCheckFormat mirrors fts3MatchinfoCheck: which directives are
// legal at all, and which need an FTS4 table's %_stat / %_docsize.
//
// 'l' is checked against hasDocsize rather than isFts4 because a
// "matchinfo=fts3" table is an fts4 table with a full %_stat and NO %_docsize
// (fts3MatchinfoOption): the oracle answers 'n' and 'a' there and errors on
// 'l' alone -- verified.
func fts3MatchinfoCheckFormat(format string, isFts4, hasDocsize bool) error {
	for i := 0; i < len(format); i++ {
		switch c := format[i]; c {
		case 'p', 'c', 'x', 'y', 'b', 's':
		case 'l':
			if !hasDocsize {
				return fmt.Errorf("engine: unrecognized matchinfo request: %c", c)
			}
		case 'n', 'a':
			if !isFts4 {
				return fmt.Errorf("engine: unrecognized matchinfo request: %c", c)
			}
		default:
			return fmt.Errorf("engine: unrecognized matchinfo request: %c", c)
		}
	}
	return nil
}

// globalHits computes matchinfo()'s 'x' GLOBAL statistics: for every phrase
// and column, the number of occurrences over all the rows that phrase matches,
// and how many of those rows held at least one.
//
// "All the rows that phrase matches" is deliberately not "all the rows the
// QUERY matches": fts3EvalGatherStats iterates the phrase's own doclist --
// or, when the phrase sits inside a NEAR cluster, that cluster's matching rows
// with the NEAR trimming applied, so only instances meeting the NEAR
// constraint are counted.
func (r *fts3MatchResult) globalHits(nCol int) [][2][]uint32 {
	if r.global != nil {
		return r.global
	}
	out := make([][2][]uint32, len(r.ev.order))
	for i := range out {
		out[i][0] = make([]uint32, nCol)
		out[i][1] = make([]uint32, nCol)
	}
	for i, ph := range r.ev.order {
		root := ph
		for root.parent != nil && root.parent.eType == fts3qNear {
			root = root.parent
		}
		for docid := range r.ev.phrases[ph] {
			if root != ph {
				// The whole NEAR cluster has to match this row, and the
				// counted positions are the ones its trimming leaves.
				r.ev.begin(docid)
				if !r.ev.testExpr(root) {
					continue
				}
			} else {
				r.ev.begin(docid)
			}
			for c, ps := range r.ev.posOf(ph) {
				if c >= nCol {
					continue
				}
				out[i][0][c] += uint32(len(ps))
				if len(ps) > 0 {
					out[i][1][c]++
				}
			}
		}
	}
	r.global = out
	return out
}

// fts3NodeCandidateSet returns node e's docid set for matchinfo 'y'/'b'
// reachability: the docids fts3EvalNextRow (fts3.c:5354-5467) would ever park
// e.iDocid on. A reformulation of its merge-join, equivalent because:
//
//   - AND/NEAR (fts3.c:5354): sorted intersection of the children's
//     sequences. NEAR is treated as AND here ("2. NEAR is treated as AND");
//     position trimming (fts3EvalNearTest) runs after a row is selected and
//     does not change which docids the node visits.
//   - OR: sorted union.
//   - NOT (fts3.c:5460-5463): "pExpr->iDocid = pLeft->iDocid; pExpr->bEof =
//     pLeft->bEof"; its set is the left child's.
//   - phrase leaf: the phrase's own doclist (ev.phrases[e]).
//
// Cursors only advance, so a node parks on row R exactly when R is in its set,
// regardless of scan direction. fts3LHitSuppressed relies on this to test set
// membership instead of simulating cursors (compat-harness/
// fts3_lhit_gather_test.go; TestFts3MatchinfoYSuppressedByPartialCoOccurrence).
func (r *fts3MatchResult) fts3NodeCandidateSet(e *fts3Expr) map[int64]bool {
	if r.nodeSets == nil {
		r.nodeSets = map[*fts3Expr]map[int64]bool{}
	}
	if s, ok := r.nodeSets[e]; ok {
		return s
	}
	var s map[int64]bool
	switch e.eType {
	case fts3qNot:
		s = r.fts3NodeCandidateSet(e.left)
	case fts3qOr:
		l, rr := r.fts3NodeCandidateSet(e.left), r.fts3NodeCandidateSet(e.right)
		s = make(map[int64]bool, len(l)+len(rr))
		for d := range l {
			s[d] = true
		}
		for d := range rr {
			s[d] = true
		}
	case fts3qAnd, fts3qNear:
		l, rr := r.fts3NodeCandidateSet(e.left), r.fts3NodeCandidateSet(e.right)
		small, big := l, rr
		if len(rr) < len(l) {
			small, big = rr, l
		}
		s = make(map[int64]bool, len(small))
		for d := range small {
			if big[d] {
				s[d] = true
			}
		}
	default: // fts3qPhrase
		s = make(map[int64]bool, len(r.ev.phrases[e]))
		for docid := range r.ev.phrases[e] {
			s[docid] = true
		}
	}
	r.nodeSets[e] = s
	return s
}

// fts3LHitSuppressed reports, for the single row docid, which of r.ev.order's
// phrases must report ZERO for matchinfo's 'y'/'b' directives regardless of
// what this row's OWN doclist entry says -- fts3ExprLHitGather's gate
// (fts3_snippet.c:911: "pExpr->bEof==0 && pExpr->iDocid==p->pCursor->
// iPrevId"), which fts3NodeCandidateSet's doc comment proves is exactly ROW
// membership in each strict ancestor's own candidate set. Unlike 'x' (whose
// stats come from a separate whole-table pass, sqlite3Fts3EvalPhraseStats,
// that does not care about reachability through the match tree at all), this is evaluated
// FRESH per row: a phrase can be un-suppressed at one docid and suppressed at
// the next, e.g. inside an OR where a sibling AND's own intersection only
// half-overlaps the phrase's own doclist.
func (r *fts3MatchResult) fts3LHitSuppressed(docid int64) []bool {
	out := make([]bool, len(r.ev.order))
	for i, ph := range r.ev.order {
		for a := ph.parent; a != nil; a = a.parent {
			if !r.fts3NodeCandidateSet(a)[docid] {
				out[i] = true
				break
			}
		}
	}
	return out
}

// fts3LcsIter is one phrase's cursor over its position list for ONE column,
// the Go form of fts3MatchinfoLcs' LcsIterator: iPos is the phrase's current
// position BIASED by iPosOffset (see lcsPerColumn), and eof mirrors the C's
// "pRead == 0".
type fts3LcsIter struct {
	pos  []int // this column's positions, ascending
	k    int   // positions consumed so far
	iPos int
	eof  bool
}

// advance is fts3LcsIteratorAdvance: it consumes the next entry of the
// position list, returning true when that entry is the list terminator (0 or
// 1 in the raw encoding), i.e. the iterator has reached EOF.
//
// The raw list is "firstPos+2" then "delta+2" per entry, and the C adds
// (entry-2) to iPos each time, so iPos advances by firstPos then by each
// delta -- which over a decoded, ascending position slice is exactly
// pos[0], then pos[k]-pos[k-1].
func (it *fts3LcsIter) advance() bool {
	if it.k >= len(it.pos) {
		it.eof = true
		return true
	}
	if it.k == 0 {
		it.iPos += it.pos[0]
	} else {
		it.iPos += it.pos[it.k] - it.pos[it.k-1]
	}
	it.k++
	return false
}

// lcsPerColumn computes matchinfo()'s 's' for the current row: per column, the
// longest run of query phrases occurring consecutively. A port of
// fts3MatchinfoLcs (fts3_snippet.c):
//
//   - each phrase gets a bias iPosOffset, the running negative sum of token
//     counts in phrase order, so adjacent phrases land on the same biased
//     position;
//   - the phrases' position iterators advance in lockstep, always the one with
//     the smallest biased position; the run grows while consecutive phrases
//     share a position and resets otherwise.
//
// A phrase absent from the column is at EOF and breaks runs. The i > 0 guard
// on iters[i-1] replaces C's unguarded pIter[-1], which relies on nThisLcs
// being 0 at i == 0.
func (r *fts3MatchResult) lcsPerColumn(nCol int) []uint32 {
	out := make([]uint32, nCol)
	if r.ev == nil {
		return out
	}
	nPhrase := len(r.ev.order)
	offsets := make([]int, nPhrase)
	nToken := 0
	for i, ph := range r.ev.order {
		if ph.phrase != nil {
			nToken -= len(ph.phrase.tokens)
		}
		offsets[i] = nToken
	}
	iters := make([]fts3LcsIter, nPhrase)
	for iCol := 0; iCol < nCol; iCol++ {
		nLcs, nLive := 0, 0
		for i, ph := range r.ev.order {
			it := &iters[i]
			*it = fts3LcsIter{pos: r.ev.posOf(ph)[iCol], iPos: offsets[i]}
			if len(it.pos) == 0 {
				it.eof = true
				continue
			}
			it.advance()
			nLive++
		}
		for nLive > 0 {
			var adv *fts3LcsIter
			nThisLcs := 0
			for i := range iters {
				it := &iters[i]
				if it.eof {
					nThisLcs = 0
					continue
				}
				if adv == nil || it.iPos < adv.iPos {
					adv = it
				}
				if nThisLcs == 0 || (i > 0 && it.iPos == iters[i-1].iPos) {
					nThisLcs++
				} else {
					nThisLcs = 1
				}
				if nThisLcs > nLcs {
					nLcs = nThisLcs
				}
			}
			if adv == nil {
				break // unreachable while nLive > 0; never spin
			}
			if adv.advance() {
				nLive--
			}
		}
		out[iCol] = uint32(nLcs)
	}
	return out
}

// matchinfo renders the matchinfo() blob for one row: little-endian u32s, one
// group per directive of format, in the order written.
func (r *fts3MatchResult) matchinfo(docid int64, format string, nCol int, stat *fts4Stat, docsize []int64) (Value, error) {
	nPhrase := 0
	if r.ev != nil {
		nPhrase = len(r.ev.order)
	}
	var out []byte
	put := func(v uint32) {
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	var global [][2][]uint32
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case 'p':
			put(uint32(nPhrase))
		case 'c':
			put(uint32(nCol))
		case 'n':
			put(uint32(stat.nDoc))
		case 'a':
			for c := 0; c < nCol; c++ {
				if stat.nDoc == 0 {
					put(0)
					continue
				}
				// The C rounds to nearest: (total + nDoc/2) / nDoc, in u32.
				put(uint32((uint32(stat.colSizes[c]) + uint32(stat.nDoc/2)) / uint32(stat.nDoc)))
			}
		case 'l':
			for c := 0; c < nCol; c++ {
				if c < len(docsize) {
					put(uint32(docsize[c]))
				} else {
					put(0)
				}
			}
		case 'x':
			if global == nil {
				global = r.globalHits(nCol) // memoized across rows
			}
			r.ev.begin(docid)
			r.ev.testExpr(r.root)
			for p := 0; p < nPhrase; p++ {
				local := r.ev.posOf(r.ev.order[p])
				for c := 0; c < nCol; c++ {
					put(uint32(len(local[c])))
					put(global[p][0][c])
					put(global[p][1][c])
				}
			}
		case 'y':
			suppressed := r.fts3LHitSuppressed(docid)
			r.ev.begin(docid)
			r.ev.testExpr(r.root)
			for p := 0; p < nPhrase; p++ {
				local := r.ev.posOf(r.ev.order[p])
				for c := 0; c < nCol; c++ {
					if suppressed[p] {
						put(0)
						continue
					}
					put(uint32(len(local[c])))
				}
			}
		case 's':
			r.ev.begin(docid)
			r.ev.testExpr(r.root)
			for _, v := range r.lcsPerColumn(nCol) {
				put(v)
			}
		case 'b':
			suppressed := r.fts3LHitSuppressed(docid)
			r.ev.begin(docid)
			r.ev.testExpr(r.root)
			nWord := (nCol + 31) / 32
			for p := 0; p < nPhrase; p++ {
				words := make([]uint32, nWord)
				if !suppressed[p] {
					local := r.ev.posOf(r.ev.order[p])
					for c := 0; c < nCol; c++ {
						if len(local[c]) > 0 {
							words[c/32] |= 1 << uint(c&31)
						}
					}
				}
				for _, w := range words {
					put(w)
				}
			}
		default:
			return Value{}, fmt.Errorf("engine: unrecognized matchinfo request: %c", format[i])
		}
	}
	return Value{Typ: Blob, S: out}, nil
}

// fts3AuxQuery resolves the query string an auxiliary function call reports
// on for the CURRENT row: info.query when it was a compile-time literal, or
// pat -- this row's value of the register compileFts3Aux coded the non-literal
// pattern into -- when it was not. See fts3AuxCompileInfo.patCompiled's own
// doc comment. isNull mirrors C fts3 leaving pCsr->pExpr NULL: no query to
// report on.
func fts3AuxQuery(info *fts3AuxCompileInfo, pat Value) (query string, isNull bool) {
	if !info.patCompiled {
		return info.query, false
	}
	return fts3MatchQueryOfValue(pat)
}

// evalFts3Aux evaluates an fts3/fts4 auxiliary function call for the current
// row. args holds snippet()'s trailing arguments and pat this row's
// non-literal MATCH query, all already read out of registers by the caller;
// the other two functions take no arguments, and a literal query ignores pat.
func evalFts3Aux(ctx *evalCtx, info *fts3AuxCompileInfo, args []Value, pat Value) (Value, error) {
	if info.fn == "snippet" {
		return evalFts3Snippet(ctx, info, args, pat)
	}
	query, isNull := fts3AuxQuery(info, pat)
	if isNull {
		// This ROW's evaluated (non-literal) pattern is NULL: C fts3
		// leaves pCsr->pExpr unset for it (fts3.c:3375-3378, zQuery==0 ->
		// *ppExpr=0), so matchinfo()/offsets() answer exactly the same "no
		// query" value the compile-time-empty case already does above
		// (fts3_snippet.c:1620-1623 sqlite3Fts3Offsets, fts3_snippet.c:
		// 1768-1770 sqlite3Fts3Matchinfo -- both just test "if(!pCsr->pExpr)").
		if info.fn == "matchinfo" {
			return Value{Typ: Blob, S: []byte{}}, nil
		}
		return Value{Typ: Text, S: []byte{}}, nil
	}
	mp := info.ms.pagerIn(ctx.pager)
	if mp == nil {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	res, err := mp.fts3MatchDocids(info.ms, query, info.contentUnneeded)
	if err != nil {
		return Value{}, err
	}
	scope := fts3ScopeNamed(ctx.tables, info.ms.scope)
	if scope == nil {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	docid, ok := fts3ScopeDocid(ctx, scope)
	if !ok {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	nCol := len(info.ms.cols)
	if info.fn == "matchinfo" {
		var stat *fts4Stat
		var docsize []int64
		if strings.ContainsAny(info.format, "nal") {
			ds, serr := mp.fts3RowStats(info.ms.table, nCol, info.ms.hasDocsize)
			if serr != nil {
				return Value{}, serr
			}
			stat, docsize = ds.stat, ds.sizes[docid]
		}
		return res.matchinfo(docid, info.format, nCol, stat, docsize)
	}
	return res.offsets(docid, nCol, fts3ColText(ctx, scope))
}

// fts3ColText returns the accessor offsets()/snippet() read the current row's
// own column values through -- what C fts3 gets from
// sqlite3_column_text(pCsr->pStmt, iCol+1). ok is false for an SQL NULL, which
// both functions skip entirely. The columns the fts table declares are
// [docid HIDDEN, col0, ...] (materializeFts3), so the text columns follow the
// hidden ones in declaration order.
func fts3ColText(ctx *evalCtx, scope *tableScope) func(i int) (string, bool) {
	var textCols []int
	for j, c := range scope.cols {
		if !c.Hidden {
			textCols = append(textCols, scope.offset+j)
		}
	}
	return func(i int) (string, bool) {
		if i < 0 || i >= len(textCols) {
			return "", false
		}
		idx := textCols[i]
		if idx < 0 || idx >= len(ctx.vals) || ctx.vals[idx].Typ == Null {
			return "", false
		}
		return valueToText(ctx.vals[idx]), true
	}
}

// ---- snippet() ----

// The markers C fts3 uses when snippet() is called
// with fewer than four arguments (fts3SnippetFunc), and the token budget it
// uses with fewer than six.
const (
	fts3SnippetStart    = "<b>"
	fts3SnippetEnd      = "</b>"
	fts3SnippetEllipsis = "<b>...</b>"
	fts3SnippetNToken   = 15
)

// evalFts3Snippet evaluates snippet() for the current row. It reproduces
// fts3SnippetFunc's own argument handling, whose ORDER is observable: the
// NULL-marker error is raised before both the "zero token budget" and the "no
// query" shortcuts, so "SELECT snippet(t,NULL) FROM t" errors even with no
// MATCH at all (verified against the oracle).
func evalFts3Snippet(ctx *evalCtx, info *fts3AuxCompileInfo, args []Value, pat Value) (Value, error) {
	zStart, zEnd, zEllipsis := fts3SnippetStart, fts3SnippetEnd, fts3SnippetEllipsis
	iCol, nToken := -1, fts3SnippetNToken
	// The C reads the arguments through a fall-through switch on the argument
	// count, so each one only displaces its default when it was written.
	if len(args) >= 1 {
		zStart = valueToText(args[0])
	}
	if len(args) >= 2 {
		zEnd = valueToText(args[1])
	}
	if len(args) >= 3 {
		zEllipsis = valueToText(args[2])
	}
	if len(args) >= 4 {
		iCol = fts3ValueInt(args[3])
	}
	if len(args) >= 5 {
		nToken = fts3ValueInt(args[4])
	}
	for i := 0; i < len(args) && i < 3; i++ {
		if args[i].Typ == Null {
			// sqlite3_value_text() hands back a NULL pointer, which
			// fts3SnippetFunc reports as SQLITE_NOMEM.
			return Value{}, fmt.Errorf("engine: snippet(): argument %d is NULL", i+2)
		}
	}
	if nToken == 0 {
		return Value{Typ: Text, S: []byte{}}, nil
	}
	if info.noQuery {
		// No MATCH on this table, so the cursor has no expression to report
		// on: "" (sqlite3Fts3Snippet's own first line).
		return Value{Typ: Text, S: []byte{}}, nil
	}
	query, isNull := fts3AuxQuery(info, pat)
	if isNull {
		// This ROW's evaluated (non-literal) pattern is NULL: same "no
		// query" answer as info.noQuery above (fts3_snippet.c:1466-1469
		// sqlite3Fts3Snippet's own "if(!pCsr->pExpr)").
		return Value{Typ: Text, S: []byte{}}, nil
	}
	mp := info.ms.pagerIn(ctx.pager)
	if mp == nil {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	res, err := mp.fts3MatchDocids(info.ms, query, info.contentUnneeded)
	if err != nil {
		return Value{}, err
	}
	scope := fts3ScopeNamed(ctx.tables, info.ms.scope)
	if scope == nil {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	docid, ok := fts3ScopeDocid(ctx, scope)
	if !ok {
		return Value{}, fmt.Errorf("engine: unable to use function %s in the requested context", info.fn)
	}
	return res.snippet(docid, len(info.ms.cols), zStart, zEnd, zEllipsis, iCol, nToken, fts3ColText(ctx, scope)), nil
}

// fts3ValueInt is sqlite3_value_int(): sqlite3VdbeIntValue's conversion (which
// bitwiseIntOperand already reproduces -- an INTEGER unchanged, a REAL
// truncated toward zero and saturated, TEXT/BLOB parsed as an INTEGER-ONLY
// leading prefix) followed by the C cast down to 32 bits.
//
// Both halves are observable in snippet()'s arguments, and both were verified
// against the oracle: a token budget of 4294967296 is a budget of ZERO (the
// empty string), and a budget of '1e3' is 1 rather than 1000.
func fts3ValueInt(v Value) int {
	return int(int32(bitwiseIntOperand(v)))
}

// fts3DocStats is FTS4's whole-table %_stat row plus every document's
// %_docsize row -- what matchinfo()'s 'n'/'a' and 'l' directives report.
// Loaded once per table and memoized on the read snapshot, since matchinfo()
// is evaluated per ROW and both shadow tables would otherwise be rescanned for
// each of them.
type fts3DocStats struct {
	stat  *fts4Stat
	sizes map[int64][]int64
}

func (p *ReadOnlyPager) fts3RowStats(table string, nCol int, hasDocsize bool) (*fts3DocStats, error) {
	key := strings.ToLower(table)
	if s, ok := p.fts3DocStats[key]; ok {
		return s, nil
	}
	rowids, rows, err := p.Rows(table + "_stat")
	if err != nil {
		return nil, err
	}
	out := &fts3DocStats{sizes: map[int64][]int64{}}
	for i, row := range rows {
		if rowids[i] != 0 || len(row) < 2 || row[1].Typ != Blob {
			continue
		}
		if out.stat, err = decodeFts4Stat(row[1].S, nCol); err != nil {
			return nil, err
		}
	}
	if out.stat == nil {
		out.stat = &fts4Stat{colSizes: make([]int64, nCol)}
	}
	// A "matchinfo=fts3" table has no %_docsize at all, and reading it would be
	// "no such table" rather than an empty set. Only 'l' needs it, and
	// fts3MatchinfoCheckFormat has already declined that for such a table.
	if hasDocsize {
		rowids, rows, err = p.Rows(table + "_docsize")
		if err != nil {
			return nil, err
		}
	} else {
		rowids, rows = nil, nil
	}
	for i, row := range rows {
		if len(row) < 2 || row[1].Typ != Blob {
			continue
		}
		var sizes []int64
		b := row[1].S
		for off := 0; off < len(b); {
			v, n := fts3GetVarint(b[off:])
			if n == 0 {
				return nil, fmt.Errorf("fts4: corrupt %%_docsize record")
			}
			sizes = append(sizes, int64(v))
			off += n
		}
		out.sizes[int64(rowids[i])] = sizes
	}
	if p.fts3DocStats == nil {
		p.fts3DocStats = map[string]*fts3DocStats{}
	}
	p.fts3DocStats[key] = out
	return out, nil
}

// fts3ScopeNamed finds the scope in tables that corresponds to want (matched by
// name, since the compile-time and run-time scope slices are distinct copies).
func fts3ScopeNamed(tables []tableScope, want *tableScope) *tableScope {
	if want == nil {
		return nil
	}
	for i := range tables {
		if strings.EqualFold(tables[i].name, want.name) {
			return &tables[i]
		}
	}
	return nil
}

// fts3ScopeDocid reads the current row's docid out of scope t -- its hidden
// "docid" column, which materializeFts3 fills from the %_content rowid.
func fts3ScopeDocid(ctx *evalCtx, t *tableScope) (int64, bool) {
	for j, c := range t.cols {
		if !c.Hidden || !strings.EqualFold(c.Name, "docid") {
			continue
		}
		idx := t.offset + j
		if idx < 0 || idx >= len(ctx.vals) {
			return 0, false
		}
		v := ctx.vals[idx]
		if v.Typ != Int {
			return 0, false
		}
		return v.I, true
	}
	return 0, false
}

func fts3ModuleWord(isFts4 bool) string {
	if isFts4 {
		return "fts4"
	}
	return "fts3"
}

// ---- where a MATCH may appear ----

// fts3MatchBindings returns the indices into scopes (this compile's FROM-item
// scopes, joinScopes order) whose fts3/4 MATCH can be served as a per-row test:
// the scopes C's planner can hand a usable constraint. Over vt USING fts4(x)
// holding 'abc','def' and tt(id) holding 1,2:
//
//	SELECT id,x FROM tt LEFT JOIN vt ON vt MATCH 'abc' ORDER BY id
//	    -> (1,abc),(2,abc)                        an ON-clause MATCH
//	SELECT a.x,b.x FROM vt a, vt b
//	    WHERE a.x MATCH 'abc' AND b.x MATCH 'def'
//	    -> (abc,def)    two aliases, each its own MATCH
//	SELECT docid FROM vt WHERE vt MATCH 'abc' OR vt MATCH 'def'
//	    -> 1,2          the OR-optimization unions two probes, the same as
//	                    OR of two membership tests
//	SELECT docid FROM vt WHERE NOT(vt MATCH 'abc')    -- ERROR in both
//	SELECT docid FROM vt WHERE vt MATCH x             -- ERROR in both: the
//	    pattern reads the MATCHed table (see slotScope's MatchExpr case)
//
// The rule: a scope is bound when exactly one top-level conjunct (of WHERE or
// some ON clause) is built purely from MatchExpr leaves on that scope joined by
// OR, and no other MatchExpr on that scope appears anywhere. A stray voids the
// scope's binding, since checkFts3Match cannot tell a slot's leaf from an
// identical stray. An ON-clause slot is refused for a "content=" table:
// fts3MatchesTableIn picks its row source from WHERE alone.
//
// A non-literal query naming a column of another bound fts table creates a
// cross-cursor dependency C's planner cannot order ("WHERE y MATCH x AND x
// MATCH y" errors there); since a real cycle cannot be told from an incidental
// one, any non-literal slot is voided when another scope is also bound.
//
// An unknown expression node voids the entire result.
//
// jts (joinedTablesFor(srcs)) is used only by ftsMatchLeftJoinUnusable
// (where_plan_fts_forced_order.go), to refuse a scope C can never use as an
// index constraint for a LEFT JOIN step.
//
// Known gap: a pattern reading the MATCHed table through a correlated subquery
// ("vt MATCH (SELECT x FROM other WHERE other.id=vt.docid)") is not caught,
// since collectTableRefs does not descend into subqueries, whereas
// sqlite3WhereExprUsage does (whereexpr.c's exprSelectUsage). fts5 shares it
// (fts5WalkMatchExpr). The fix is to walk into the subquery for a reference to
// idx specifically.
func fts3MatchBindings(p *ReadOnlyPager, stmt *SelectStmt, scopes []tableScope, jts []joinedTable) map[int]*MatchExpr {
	validSlot := map[int]int{}
	strayHit := map[int]bool{}
	soloOf := map[int]*MatchExpr{}
	nonLitOf := map[int]bool{}
	known := true

	// mark walks e exactly as fts3CountMatchExpr does (same grammar, same
	// default-deny), but instead of counting it poisons (strayHit) the
	// resolved scope of every MatchExpr it finds -- e is known to NOT be a
	// clean single-scope slot by the time this is called.
	var mark func(e Expr) bool
	mark = func(e Expr) bool {
		sum := func(es ...Expr) bool {
			ok := true
			for _, sub := range es {
				if !mark(sub) {
					ok = false
				}
			}
			return ok
		}
		switch x := e.(type) {
		case nil:
			return true
		case LiteralExpr, ParamExpr, ColumnExpr, RaiseExpr:
			return true
		case MatchExpr:
			if _, idx, ok := fts3ResolveMatchScopeIndex(p, scopes, x.X); ok {
				strayHit[idx] = true
			}
			return sum(x.X, x.Pattern)
		case UnaryExpr:
			return sum(x.X)
		case BinaryExpr:
			return sum(x.L, x.R)
		case IsNullExpr:
			return sum(x.X)
		case CollateExpr:
			return sum(x.X)
		case CastExpr:
			return sum(x.X)
		case BetweenExpr:
			return sum(x.X, x.Lo, x.Hi)
		case LikeExpr:
			return sum(x.X, x.Pattern, x.Escape)
		case GlobExpr:
			return sum(x.X, x.Pattern)
		case InExpr:
			return sum(append([]Expr{x.X}, x.List...)...)
		case FuncExpr:
			if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
				return false
			}
			return sum(x.Args...)
		case CaseExpr:
			es := []Expr{x.Base, x.Else}
			for _, w := range x.Whens {
				es = append(es, w.When, w.Then)
			}
			return sum(es...)
		case RowExpr:
			return sum(x.Elems...)
		case SubqueryExpr, ExistsExpr:
			// A subquery compiles separately and validates its own MATCH.
			return true
		}
		return false
	}

	// slotScope classifies e as a usable per-scope constraint: a MatchExpr leaf
	// (its own solo representative) or an OR of usable constraints for the
	// same scope (solo nil). ok is false otherwise (AND, NOT, non-MATCH, OR of
	// different scopes, unknown node), and the caller walks e with mark so any
	// MatchExpr inside poisons its scope. nonLit tracks whether any leaf's
	// query is not a literal.
	var slotScope func(e Expr) (fts3MatchSlot, bool)
	slotScope = func(e Expr) (fts3MatchSlot, bool) {
		switch x := e.(type) {
		case MatchExpr:
			if x.Not {
				// "X NOT MATCH Y" is NOT (X MATCH Y): parse.y marks the token
				// (parse.y:1362) and wraps the call in TK_NOT (parse.y:1370), and
				// exprAnalyze never lifts it (whereexpr.c:1533). This parser makes one
				// flagged node, so refuse it here like the parenthesized spelling
				// ("unable to use function MATCH in the requested context"). Same in
				// fts5_match_placement.go.
				return fts3MatchSlot{}, false
			}
			ms, idx, ok := fts3ResolveMatchScopeIndex(p, scopes, x.X)
			if !ok {
				return fts3MatchSlot{}, false
			}
			// whereexpr.c:1541-1543: "prereqExpr = sqlite3WhereExprUsage(pMaskSet,
			// pRight); prereqColumn = sqlite3WhereExprUsage(pMaskSet, pLeft); if(
			// (prereqExpr & prereqColumn)==0 )" -- a pattern reading any column of
			// the MATCHed table can never become a WO_AUX constraint, and the
			// remaining "column MATCH expr" call is sqlite3InvalidFunction
			// (main.c:2210), "unable to use function MATCH in the requested
			// context". Covers "t10 MATCH t10.value", "t10 MATCH t10.other",
			// "t10 MATCH t10", "t10 MATCH value", in WHERE or ON, and OR'd with a
			// literal leaf. A pattern naming another table's column ("ft1 MATCH y")
			// still binds. Correlated subqueries are not covered (see the doc
			// comment).
			patternRefs := map[int]bool{}
			collectTableRefs(x.Pattern, scopes, patternRefs)
			if patternRefs[idx] {
				return fts3MatchSlot{}, false
			}
			_, _, isLit := fts3MatchPattern(x.Pattern)
			leaf := x
			return fts3MatchSlot{idx: idx, hasContent: ms.hasContent, nonLit: !isLit, solo: &leaf}, true
		case BinaryExpr:
			if x.Op != "OR" {
				return fts3MatchSlot{}, false
			}
			l, lok := slotScope(x.L)
			r, rok := slotScope(x.R)
			if !lok || !rok || l.idx != r.idx {
				return fts3MatchSlot{}, false
			}
			return fts3MatchSlot{idx: l.idx, hasContent: l.hasContent || r.hasContent, nonLit: l.nonLit || r.nonLit}, true
		}
		return fts3MatchSlot{}, false
	}

	// fromToJts maps a raw stmt.From index (scopes-space: joinScopes
	// flattens groups to one entry per leaf, so scopes[i] is stmt.From[i])
	// to the jts index of the top-level join step it produced. jts collapses
	// a group's span into one entry (resolveJoinSources: "i += it.GroupLen"),
	// so indices after a group are offset.
	//
	// Only an ordinary item and a group's first (connector) item own a jts
	// entry; interior group members map to -1, since their ON clauses are
	// internal to the group, and ftsMatchLeftJoinUnusable declines on
	// onOwner < 0.
	//
	// Built from the same GroupLen span walk resolveJoinSources does. If the
	// count disagrees with len(jts) (never observed; checkFlattenSafe
	// validates GroupLen) every entry is -1.
	fromToJts := make([]int, len(stmt.From))
	j := 0
	for i := 0; i < len(stmt.From); {
		gl := stmt.From[i].GroupLen
		if gl <= 0 {
			gl = 1
		}
		if i+gl > len(stmt.From) {
			gl = len(stmt.From) - i
		}
		fromToJts[i] = j
		for k := i + 1; k < i+gl; k++ {
			fromToJts[k] = -1
		}
		j++
		i += gl
	}
	if j != len(jts) {
		for i := range fromToJts {
			fromToJts[i] = -1
		}
	}
	toJts := func(scopeIdx int) int {
		if scopeIdx < 0 || scopeIdx >= len(fromToJts) {
			return -1
		}
		return fromToJts[scopeIdx]
	}
	walkConjunct := func(cj Expr, isOn bool, onOwner int) {
		if slot, ok := slotScope(cj); ok && (!isOn || !slot.hasContent) {
			// ftsMatchLeftJoinUnusable takes jts-space indices; slot.idx and the
			// pattern-ref keys are scopes-space, so translate here and nowhere else
			// (vdbe_scan.go reads c.fts3MatchGood by scopes index).
			//
			// -1 means a table interior to a group. gatherScopesRow (vdbe.go) reads
			// a group member through the group's shared cursor, which it cannot
			// pair with one scope ("FROM y, (a LEFT JOIN t10 ON a.id=t10.rowid)
			// WHERE t10 MATCH 'apple'" panicked), so poison the scope and decline.
			jtsIdx := toJts(slot.idx)
			refs := fts3MatchConjunctPatternRefs(p, cj, slot.idx, scopes)
			jtsRefs := make(map[int]bool, len(refs))
			unmappable := jtsIdx < 0
			for r := range refs {
				if jr := toJts(r); jr >= 0 {
					jtsRefs[jr] = true
				} else {
					unmappable = true
				}
			}
			if unmappable || ftsMatchLeftJoinUnusable(jts, jtsIdx, isOn, onOwner, jtsRefs) {
				// C SQLite can never hand this MATCH to fts3/4's own
				// xBestIndex for this LEFT JOIN nesting (see
				// ftsMatchLeftJoinUnusable's own doc comment) -- poison the
				// scope exactly like a stray duplicate would, rather than
				// accept a slot that would silently answer wrong.
				if !mark(cj) {
					known = false
				}
				return
			}
			validSlot[slot.idx]++
			soloOf[slot.idx] = slot.solo
			if slot.nonLit {
				nonLitOf[slot.idx] = true
			}
			return
		}
		if !mark(cj) {
			known = false
		}
	}
	for _, cj := range splitTopLevelAnd(stmt.Where) {
		walkConjunct(cj, false, -1)
	}
	for i, it := range stmt.From {
		if it.On == nil {
			continue
		}
		for _, cj := range splitTopLevelAnd(it.On) {
			walkConjunct(cj, true, toJts(i))
		}
	}
	for _, sc := range stmt.Columns {
		if !mark(sc.Expr) {
			known = false
		}
	}
	for _, g := range stmt.GroupBy {
		if !mark(g) {
			known = false
		}
	}
	if !mark(stmt.Having) {
		known = false
	}
	for _, o := range stmt.OrderBy {
		if !mark(o.Expr) {
			known = false
		}
	}
	// A compound arm is compiled as its own statement; anything it holds is
	// checked there.

	if !known {
		return nil
	}
	good := map[int]*MatchExpr{}
	for idx, n := range validSlot {
		if n == 1 && !strayHit[idx] {
			good[idx] = soloOf[idx]
		}
	}
	if len(good) > 1 {
		for idx := range good {
			if nonLitOf[idx] {
				delete(good, idx)
			}
		}
	}
	return good
}

// fts3MatchConjunctPatternRefs collects every table cj's MATCH leaf/leaves
// against scope idx depend on (their PATTERN's own table references, via
// collectTableRefs -- the same walk ftsMatchForcedOrder uses for its
// identical INNER-join question), unioned across an OR of two-or-more
// leaves against the same scope (slotScope's own shape: at most an OR, never
// an AND/NOT/anything else). The common case -- a single leaf, no OR -- is
// just that one leaf's own dependencies. Consumed only by
// ftsMatchLeftJoinUnusable's WHERE-clause branch (where_plan_fts_forced_order.go).
func fts3MatchConjunctPatternRefs(p *ReadOnlyPager, cj Expr, idx int, scopes []tableScope) map[int]bool {
	refs := map[int]bool{}
	var walk func(Expr)
	walk = func(e Expr) {
		switch x := e.(type) {
		case MatchExpr:
			if _, i, ok := fts3ResolveMatchScopeIndex(p, scopes, x.X); ok && i == idx {
				collectTableRefs(x.Pattern, scopes, refs)
			}
		case BinaryExpr:
			if x.Op == "OR" {
				walk(x.L)
				walk(x.R)
			}
		}
	}
	walk(cj)
	return refs
}

// fts3MatchSlot is slotScope's classification of one WHERE/ON top-level
// conjunct: the scope it constrains, and enough about its shape for
// fts3MatchBindings' two post-checks (an ON-clause "content=" refusal, and
// the non-literal cross-cursor-dependency fixup).
type fts3MatchSlot struct {
	idx        int
	hasContent bool
	// nonLit is true when ANY leaf of this slot's query string is not a
	// literal.
	nonLit bool
	// solo is this slot's own MatchExpr when it has exactly one leaf (no
	// OR) -- what compileFts3Aux reports on; nil for an OR of 2+ leaves,
	// which has no single representative query string.
	solo *MatchExpr
}

// fts3ResolveMatchScopeIndex is fts3ResolveMatchTarget, additionally
// reporting WHICH element of scopes matched. A *tableScope taken from one
// []tableScope copy is never == one taken from another -- every caller of
// fts3ResolveMatchTarget (compileMatchExpr, fts3MatchScopeIndex,
// fts3MatchBindings itself) builds its own fresh copy from c.scopes -- so the
// stable identity fts3MatchBindings' per-source "good" set is keyed by is
// this INDEX, not the scope pointer. Behaviorally identical to calling
// fts3ResolveMatchTarget once with the whole slice: its loop body never
// compares scopes against each other, only against e, so resolving one
// scope at a time and returning the first success reproduces the same
// "first match, in order" result.
func fts3ResolveMatchScopeIndex(p *ReadOnlyPager, scopes []tableScope, e Expr) (ms fts3MatchScope, idx int, ok bool) {
	for i := range scopes {
		if m, mok := fts3ResolveMatchTarget(p, scopes[i:i+1], e); mok {
			return m, i, true
		}
	}
	return fts3MatchScope{}, -1, false
}

// fts3MatchScopeIndex is fts3ResolveMatchScopeIndex against THIS compile's
// own c.scopes, for checkFts3Match: it cannot reuse the *tableScope
// compileMatchExpr already resolved (vdbe_codegen.go builds its own fresh
// []tableScope copy of c.scopes for every MATCH node it compiles), so it
// resolves again here, against c.scopes directly, to get the INDEX
// c.fts3MatchGood (set once by compileSelectScan, via fts3MatchBindings) is
// keyed by.
func (c *compiler) fts3MatchScopeIndex(e Expr) (int, bool) {
	scopes := make([]tableScope, len(c.scopes))
	for i, s := range c.scopes {
		scopes[i] = s.tableScope
	}
	_, idx, ok := fts3ResolveMatchScopeIndex(c.pager, scopes, e)
	return idx, ok
}

// fts3CountMatchExpr counts MatchExpr nodes in e, stopping at subquery
// boundaries. It DEFAULT-DENIES: an expression node it does not recognise
// makes known false, so the caller declines rather than assume no MATCH hides
// inside it.
func fts3CountMatchExpr(e Expr) (int, bool) {
	// sum folds a sequence of sub-expressions, starting from "known".
	sum := func(es ...Expr) (int, bool) {
		n, known := 0, true
		for _, sub := range es {
			c, k := fts3CountMatchExpr(sub)
			n += c
			known = known && k
		}
		return n, known
	}
	switch x := e.(type) {
	case nil:
		return 0, true
	case LiteralExpr, ParamExpr, ColumnExpr, RaiseExpr:
		return 0, true
	case MatchExpr:
		n, known := sum(x.X, x.Pattern)
		return n + 1, known
	case UnaryExpr:
		return sum(x.X)
	case BinaryExpr:
		return sum(x.L, x.R)
	case IsNullExpr:
		return sum(x.X)
	case CollateExpr:
		return sum(x.X)
	case CastExpr:
		return sum(x.X)
	case BetweenExpr:
		return sum(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return sum(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return sum(x.X, x.Pattern)
	case InExpr:
		return sum(append([]Expr{x.X}, x.List...)...)
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
			return 0, false
		}
		return sum(x.Args...)
	case CaseExpr:
		es := []Expr{x.Base, x.Else}
		for _, w := range x.Whens {
			es = append(es, w.When, w.Then)
		}
		return sum(es...)
	case RowExpr:
		return sum(x.Elems...)
	case SubqueryExpr, ExistsExpr:
		// A subquery compiles separately and validates its own MATCH.
		return 0, true
	}
	return 0, false
}

// ---- content-existence-check placement ("does this statement ever need
// %_content at all") ----

// fts3StmtContentUnneeded reports whether no row stmt can produce needs a read
// of tableName's (or alias's) %_content. Both fts3MatchDocids' existence check
// (via compiler.fts3ContentUnneeded) and materializeFts3Ascending's row source
// (via FromItem.fts3ContentUnneeded, withVtabWhere) consult it.
//
// C needs %_content only when something reads a real row: fts3CursorSeek
// (fts3.c:1839) is the only place raising SQLITE_CORRUPT_VTAB for a missing
// row (fts3.c:1858), and fts3ColumnMethod calls it only for a user column
// (fts3.c:3500), not for the hidden table column or docid (fts3.c:3462-3505).
// offsets()/snippet() seek (fts3.c:3756, :3776); matchinfo() never does
// (fts3.c:3822-3836; fts3_snippet.c:1754-1774).
//
// The rule (false is the safe default):
//  1. a single-source SELECT with no compound, DISTINCT, GROUP BY/HAVING,
//     ORDER BY or WINDOW;
//  2. WHERE's top-level conjuncts are exactly one MatchExpr on tableName or
//     alias (fts3ExprMatchesTable, the same test materializeFts3Content
//     uses; a false positive only adds candidate rows the MATCH filters);
//  3. every select-list expression is fts3ResultColumnSafe.
//
// wanted, when non-nil, is the lazy term set fts3MatchDocids would gather for
// this MATCH's literal pattern (nil for a non-literal one). It lets
// materializeFts3Ascending offer only the docids this MATCH could touch rather
// than decoding the whole index, which would reintroduce the corrupt-node
// decline the lazy reader avoids (fts3corrupt4.test 43.2;
// comp_r26_g_fts3_lazy_segment_walk_test.go).
func fts3StmtContentUnneeded(p *ReadOnlyPager, stmt *SelectStmt, tableName, alias string) (unneeded bool, wanted *fts3WantedTerms) {
	if stmt.Compound != nil || stmt.Distinct || stmt.GroupBy != nil || stmt.Having != nil ||
		len(stmt.OrderBy) > 0 || len(stmt.Windows) > 0 || len(stmt.From) != 1 || len(stmt.Columns) == 0 {
		// len(stmt.Columns)==0 can't happen for anything parsed from SQL text
		// (the grammar requires at least one select-item) and every synthetic
		// *SelectStmt this package builds internally (update_from.go,
		// view_trigger.go, cte.go, vdbe_window_group.go) always populates at
		// least one column too -- but rule 3 below WALKS stmt.Columns, so an
		// empty list would vacuously satisfy it and return true for a
		// statement this function was never meant to reason about. Guarded
		// explicitly rather than relying on every current and future caller
		// keeping that invariant.
		return false, nil
	}
	conj := splitTopLevelAnd(stmt.Where)
	if len(conj) != 1 {
		return false, nil
	}
	mx, isMatch := conj[0].(MatchExpr)
	if !isMatch {
		return false, nil
	}
	meta, ok := p.fts3TableInfo(tableName)
	if !ok {
		return false, nil
	}
	if !fts3ExprMatchesTable(mx, tableName, meta.cols) &&
		(alias == "" || !fts3ExprMatchesTable(mx, alias, meta.cols)) {
		return false, nil
	}
	solo := len(stmt.Columns) == 1
	for _, sc := range stmt.Columns {
		if sc.Star || !fts3ResultColumnSafe(sc.Expr, tableName, alias, meta, solo) {
			return false, nil
		}
	}
	return true, fts3WantedTermsForMatch(mx, meta)
}

// fts3WantedTermsForMatch mirrors fts3ResolveMatchTarget's own two
// resolutions (the table-named hidden column, or one of the table's real
// columns) to find iDefault, then parses the pattern -- if it's a LITERAL --
// exactly as fts3MatchDocids itself does, and gathers its wanted terms. nil
// (not an error) whenever the pattern isn't a compile-time literal, or the
// query fails to parse; both fall back to the caller's own eager path, which
// is a strictly safe, if less lazy, answer.
func fts3WantedTermsForMatch(mx MatchExpr, meta *fts3TableMeta) *fts3WantedTerms {
	ce, isCol := mx.X.(ColumnExpr)
	if !isCol {
		return nil
	}
	iDefault := len(meta.cols)
	for j, c := range meta.cols {
		if strings.EqualFold(c, ce.Name) {
			iDefault = j
			break
		}
	}
	text, isNull, litOK := fts3MatchPattern(mx.Pattern)
	if !litOK || isNull {
		return nil
	}
	root, err := fts3ParseQuery(text, meta.cols, meta.isFts4, iDefault, meta.tok)
	if err != nil {
		return nil
	}
	return fts3GatherWantedTerms(root)
}

// fts3ResultColumnSafe reports whether SELECT-list expression e is a shape
// C fts3 can answer without ever seeking tableName's %_content row -- see
// fts3StmtContentUnneeded's own doc comment for the C citations. Modeled on
// fts3CountMatchExpr's walk just above (same grammar, same default-deny
// posture): an expression node it does not recognise answers false, since it
// might hide a real-column read this pass has no way to classify. solo is
// true only when e IS the statement's one and only select-list expression
// (fts3StmtContentUnneeded computes it once, for the top-level call alone --
// every recursive call below passes false, so a count() buried inside a
// larger expression never qualifies).
func fts3ResultColumnSafe(e Expr, tableName, alias string, meta *fts3TableMeta, solo bool) bool {
	safe := func(x Expr) bool { return fts3ResultColumnSafe(x, tableName, alias, meta, false) }
	all := func(es ...Expr) bool {
		for _, x := range es {
			if !safe(x) {
				return false
			}
		}
		return true
	}
	switch x := e.(type) {
	case nil, LiteralExpr, ParamExpr:
		// A constant reads no row at all.
		return true
	case ColumnExpr:
		if x.Qualifier != "" && !strings.EqualFold(x.Qualifier, tableName) &&
			(alias == "" || !strings.EqualFold(x.Qualifier, alias)) {
			return false
		}
		if strings.EqualFold(x.Name, "docid") {
			// fts3ColumnMethod's case 1 -- pCsr->iPrevId, no seek.
			return true
		}
		for _, c := range meta.cols {
			if strings.EqualFold(c, x.Name) {
				// A real declared column: the `default:` arm always seeks.
				return false
			}
		}
		if strings.EqualFold(x.Name, tableName) {
			// fts3ColumnMethod's case 0 -- the hidden "table-name" pointer
			// column (fts3.c:3474-3477): sqlite3_result_pointer(), which
			// returns before ever reaching the `default:` arm's
			// fts3CursorSeek. This is a property of the COLUMN being read,
			// not of where in the expression tree it sits, so it applies
			// here exactly as it already does for matchinfo()'s own first
			// argument (fts3ColumnNamesTablePointer) -- e.g. this corpus's
			// own fts3corrupt4.test 44.2, "matchinfo(t0, t0)", whose SECOND
			// argument is the same hidden column again.
			return true
		}
		// Not a declared column: the ordinary rowid aliases resolve to the
		// SAME docid case 1 answers, with no seek either.
		return strings.EqualFold(x.Name, "rowid") || strings.EqualFold(x.Name, "oid") || strings.EqualFold(x.Name, "_rowid_")
	case UnaryExpr:
		return safe(x.X)
	case BinaryExpr:
		return all(x.L, x.R)
	case IsNullExpr:
		return safe(x.X)
	case CollateExpr:
		return safe(x.X)
	case CastExpr:
		return safe(x.X)
	case BetweenExpr:
		return all(x.X, x.Lo, x.Hi)
	case LikeExpr:
		return all(x.X, x.Pattern, x.Escape)
	case GlobExpr:
		return all(x.X, x.Pattern)
	case InExpr:
		if x.Sub != nil {
			// A subquery validates its own content needs separately; this
			// walk has no way to reach inside it.
			return false
		}
		return all(append([]Expr{x.X}, x.List...)...)
	case CaseExpr:
		es := []Expr{x.Base, x.Else}
		for _, w := range x.Whens {
			es = append(es, w.When, w.Then)
		}
		return all(es...)
	case RowExpr:
		return all(x.Elems...)
	case FuncExpr:
		if x.Over != nil || x.Filter != nil || len(x.OrderBy) > 0 {
			return false
		}
		switch {
		case strings.EqualFold(x.Name, "count") && !x.Distinct && solo:
			// count(*)/count(<literal>), and ONLY as the statement's sole
			// select-list item -- the one aggregate shape the mined corpus
			// needs; see fts3StmtContentUnneeded's rule 1 for why a bare
			// aggregate with no GROUP BY still collapses to a single row
			// regardless of how many rows fed it.
			if x.Star {
				return true
			}
			if len(x.Args) == 1 {
				if _, isLit := x.Args[0].(LiteralExpr); isLit {
					return true
				}
			}
			return false
		case strings.EqualFold(x.Name, "matchinfo"):
			// matchinfo() itself is content-free (see this function's own
			// doc comment) regardless of its format directives -- but its
			// FIRST argument must still resolve to the table's own hidden
			// pointer column (a real-COLUMN first argument is the SEPARATE,
			// already-existing "illegal first argument" decline,
			// compileFts3Aux, and needs no special handling here), and any
			// remaining argument (ordinarily the literal format string) is
			// checked by this SAME walk, recursively.
			if len(x.Args) == 0 || !fts3ColumnNamesTablePointer(x.Args[0], tableName, alias, meta.cols) {
				return false
			}
			return all(x.Args[1:]...)
		case strings.EqualFold(x.Name, "offsets") || strings.EqualFold(x.Name, "snippet"):
			// fts3.c:3756 (sqlite3Fts3Offsets) and :3776 (sqlite3Fts3Snippet)
			// ALWAYS seek %_content to recover the row's own text, regardless
			// of arguments -- unlike matchinfo() above, whose data is entirely
			// derived from the index. Named explicitly so they never fall into
			// the general scalar-function case below.
			return false
		case isAggregateFuncName(x.Name):
			// sum()/avg()/group_concat()/etc.: this function only decides
			// whether an expression READS REAL CONTENT, which is orthogonal
			// to the row-collapsing semantics count()'s own "solo" case
			// above exists to gate -- nothing here reproduces that gate for
			// any OTHER aggregate, so this stays conservatively unsafe.
			return false
		}
		// Any other scalar function is as content-free as its arguments:
		// fts3ColumnMethod seeks %_content only when xColumn is called for a
		// declared column, which only an argument can do
		// (fts3corrupt4.test 15.1, "quote(matchinfo(t1,t1))==0").
		return all(x.Args...)
	}
	return false
}

// fts3ColumnNamesTablePointer reports whether e is a ColumnExpr naming
// tableName's own hidden "table pointer" column -- fts3ResolveMatchTarget's
// bare-name branch above ("if strings.EqualFold(name, ce.Name)"), which is
// always the REAL table name and never its alias (verified there: "FROM t AS
// x WHERE t MATCH 'q'" works, "... WHERE x MATCH 'q'" is "no such column:
// x"). alias is still accepted as a QUALIFIER, matching that same function's
// qualifier rule.
func fts3ColumnNamesTablePointer(e Expr, tableName, alias string, cols []string) bool {
	ce, ok := e.(ColumnExpr)
	if !ok {
		return false
	}
	qualifierName := alias
	if qualifierName == "" {
		qualifierName = tableName
	}
	if ce.Qualifier != "" && !strings.EqualFold(ce.Qualifier, qualifierName) {
		return false
	}
	for _, c := range cols {
		if strings.EqualFold(c, ce.Name) {
			return false // a real column wins over the table-named hidden one.
		}
	}
	return strings.EqualFold(ce.Name, tableName)
}
