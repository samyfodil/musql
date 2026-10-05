// This file is the inverse of fts5_index.go: it decodes an fts5 table's %_data
// segment pages back into (term, rowid, column, position) postings, so
// 'integrity-check' can compare them with a fresh tokenization of %_content as
// C's checksum comparison does. fts5_index.go's file comment is the format
// reference.
//
// # The segment merge rule
//
// C fts5 appends a segment per flush, so an UPDATE or DELETE adds newer
// postings rather than rewriting old ones. segid is monotonic and a merge's
// output gets a higher segid than its inputs, so "highest segid wins" is the
// correct priority across levels.
//
// The doclist size field ("varint nPos*2+bDel") is the poslist's byte length
// doubled, low bit set on an UPDATE/DELETE-derived entry. That bit does not by
// itself mean deleted -- a no-op UPDATE rewrites every term with the bit set
// and its positions intact -- and it is not needed: a winning entry with a
// non-empty poslist is the current value, and an empty one is a removal (a real
// posting always has a position).
package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// fts5DecSegment is one segment's leaf-page range, read from the structure
// record. Which LEVEL it sits at doesn't matter for decoding: a leaf page's
// %_data id is segid<<37|pgno regardless (fts5_index.go's fts5SegmentRowid),
// so a flat segment list is all a walk needs.
//
// origin1/origin2/nPgTombstone come from the V2 arm of the record and are zero
// on a legacy one -- see fts5DecodeStructure.
type fts5DecSegment struct {
	segid               int64
	pgnoFirst, pgnoLast int64

	origin1, origin2 uint64
	nPgTombstone     int64
}

// fts5segmentBlockID is fts5SegmentRowid generalized to int64 segid/pgno.
// The decoder's inputs come from a structure record that may be adversarial,
// so this stays in int64 throughout rather than narrowing through
// fts5SegmentRowid's int parameters (only a real concern on a 32-bit
// GOARCH, but free to avoid regardless).
func fts5segmentBlockID(segid, pgno int64) int64 {
	return segid<<(fts5DataPageB+fts5DataHeightB+fts5DataDliB) | pgno
}

// fts5StructureV2Marker is FTS5_STRUCTURE_V2, the four bytes that follow the
// cookie in the structure record of a contentless_delete=1 table. It is
// unambiguous against the legacy record's leading nLevel varint because a
// varint starting 0xFF is at least 16256 while FTS5_MAX_LEVEL is 64
// (fts5_index.c's own argument for the encoding).
var fts5StructureV2Marker = []byte{0xff, 0x00, 0x00, 0x01}

// fts5DecodeStructure parses a %_data structure record (fts5StructureRowid) into
// its flat segment list, reporting whether it was V2.
//
// V2 (fts5StructureDecode) adds FTS5_STRUCTURE_V2 between the cookie and
// nLevel, and five varints after each segment's pgnoLast: iOrigin1, iOrigin2,
// nPgTombstone, nEntryTombstone, nEntry. The origins (what a
// contentless_delete DELETE matches a %_docsize origin against) and
// nPgTombstone are kept. e.g. a two-row contentless_delete=1 table with one
// row deleted:
// x'00000000FF000001010203000201010101010000010201010202010101' -- nLevel 1,
// nSegment 2, write counter 3, (segid 1, pages 1..1, origins 1..1, no
// tombstones, 1 entry), (segid 2, pages 1..1, origins 2..2, 1 tombstone page,
// 1 tombstone, 1 entry).
//
// An empty blob decodes to zero segments, as C tolerates it (the checksum
// comparison then reports "fts5: checksum mismatch"). A non-empty blob that
// does not fully parse is "fts5: corrupt structure record for table ...".
func fts5DecodeStructure(blob []byte) ([]fts5DecSegment, bool, error) {
	if len(blob) == 0 {
		return nil, false, nil
	}
	if len(blob) < 4 {
		return nil, false, fmt.Errorf("structure record shorter than its 4-byte cookie")
	}
	b := blob[4:]
	v2 := false
	if len(b) >= 4 && bytes.Equal(b[:4], fts5StructureV2Marker) {
		b = b[4:]
		v2 = true
	}
	read := func() (uint64, error) {
		v, n := getVarint(b)
		if n == 0 {
			return 0, fmt.Errorf("truncated varint in structure record")
		}
		b = b[n:]
		return v, nil
	}
	nLevel, err := read()
	if err != nil {
		return nil, v2, err
	}
	if _, err := read(); err != nil { // nSegment: informational, not needed to walk
		return nil, v2, err
	}
	if _, err := read(); err != nil { // nWriteCounter: C fts5's own merge scheduler
		return nil, v2, err
	}
	// A structure record this implausible cannot come from a real write path
	// (fts5's own or this engine's): treat it as corrupt rather than spend a
	// long time walking it.
	const maxLevels = 1 << 16
	const maxSegsPerLevel = 1 << 20
	if nLevel > maxLevels {
		return nil, v2, fmt.Errorf("implausible level count %d", nLevel)
	}
	var segs []fts5DecSegment
	for lvl := uint64(0); lvl < nLevel; lvl++ {
		if _, err := read(); err != nil { // nMerge: C fts5's own merge scheduler
			return nil, v2, err
		}
		nSeg, err := read()
		if err != nil {
			return nil, v2, err
		}
		if nSeg > maxSegsPerLevel {
			return nil, v2, fmt.Errorf("implausible segment count %d", nSeg)
		}
		for i := uint64(0); i < nSeg; i++ {
			segid, err := read()
			if err != nil {
				return nil, v2, err
			}
			pgnoFirst, err := read()
			if err != nil {
				return nil, v2, err
			}
			pgnoLast, err := read()
			if err != nil {
				return nil, v2, err
			}
			seg := fts5DecSegment{segid: int64(segid), pgnoFirst: int64(pgnoFirst), pgnoLast: int64(pgnoLast)}
			if v2 {
				var vals [5]uint64
				for k := range vals {
					if vals[k], err = read(); err != nil {
						return nil, v2, err
					}
				}
				// vals[3]/vals[4] are nEntryTombstone and nEntry: fts5's own
				// merge-scheduling accounting (fts5IndexFindDeleteMerge), read
				// past rather than kept, because nothing here schedules merges.
				seg.origin1, seg.origin2 = vals[0], vals[1]
				if vals[2] > maxSegsPerLevel {
					return nil, v2, fmt.Errorf("implausible tombstone page count %d", vals[2])
				}
				seg.nPgTombstone = int64(vals[2])
			}
			if len(segs) >= maxSegsPerLevel {
				return nil, v2, fmt.Errorf("too many segments in structure record")
			}
			segs = append(segs, seg)
		}
	}
	return segs, v2, nil
}

// fts5OriginCounter is the origin value a contentless_delete=1 table's NEXT
// flushed segment takes: one past the highest iOrigin2 any segment carries.
// That is fts5StructureDecode's own derivation ("pRet->nOriginCntr =
// nOriginCntr+1", with nOriginCntr the running MAX of iOrigin2) -- the counter
// is not stored anywhere else, so a table whose segments were all merged away
// starts again from 1, exactly as C fts5 does.
func fts5OriginCounter(segs []fts5DecSegment) int64 {
	var max uint64
	for _, s := range segs {
		if s.origin2 > max {
			max = s.origin2
		}
	}
	if max >= 1<<62 {
		// Only reachable from a hand-corrupted record; keep the counter in a
		// range int64 arithmetic can carry.
		return 1
	}
	return int64(max) + 1
}

// fts5decPageState carries decode state across one segment's leaf pages: the
// term whose doclist (or an in-flight poslist) may continue onto the next page.
// The zero value is right for a segment's first page.
//
// owedPending and owedAwaitingColumn exist because the poslist's "0x01, column
// varint" escape is two separate varints; only a single varint is guaranteed
// not to split across pages, so the 0x01 can end one page and its column start
// the next.
type fts5decPageState struct {
	haveOpenTerm bool
	openTerm     string

	owedBytes          int // raw poslist bytes not yet READ off any page
	owedRowid          int64
	owedPending        []byte // bytes already read but not yet a complete varint
	owedAwaitingColumn bool   // the last complete varint read was the 0x01 escape; its column index is still due
	owedCol            int64  // poslist decode state: current column
	owedPos            int64  // poslist decode state: last position decoded
	owedPositions      []int64
}

// fts5decodeLeafPage decodes one leaf page (fts5_index.go's "A leaf page"),
// calling emit for each completed posting -- a (term, rowid) pair and its
// position list, stripped of fts5MainPrefix. It never panics: every access is
// bounds-checked, and anything it cannot make sense of is an error.
//
// The header's first field h0 ("offset of the first rowid on the page, or 0 if
// a term precedes it") is the only signal for one case: when the previous
// page's open term ended exactly at the boundary, nothing distinguishes "a new
// term starts at offset 4" from "the same term continues" except h0 nonzero.
//
// detail decides what follows a rowid: a size varint plus positions (full), a
// size varint plus column numbers (columns), or nothing but optional 0x00
// delete markers (none). All three normalize into one positions slice; see
// fts5decodePostingsRun.
func fts5decodeLeafPage(page []byte, st *fts5decPageState, detail fts5Detail, emit func(term string, rowid int64, positions []int64)) error {
	if len(page) < 4 {
		return fmt.Errorf("leaf page shorter than its 4-byte header")
	}
	h0 := int(binary.BigEndian.Uint16(page[0:2]))
	szLeaf := int(binary.BigEndian.Uint16(page[2:4]))
	if szLeaf < 4 || szLeaf > len(page) {
		return fmt.Errorf("szLeaf %d out of range for a %d-byte page", szLeaf, len(page))
	}
	if h0 != 0 && (h0 < 4 || h0 > szLeaf) {
		return fmt.Errorf("leaf page header rowid offset %d out of range", h0)
	}
	footer := page[szLeaf:]
	var termStarts []int
	{
		off, cum := 0, 0
		for off < len(footer) {
			v, n := getVarint(footer[off:])
			if n == 0 {
				return fmt.Errorf("leaf page footer has a truncated varint")
			}
			off += n
			cum += int(v)
			if cum < 4 || cum > szLeaf {
				return fmt.Errorf("leaf page footer term offset %d out of range", cum)
			}
			if len(termStarts) > 0 && cum <= termStarts[len(termStarts)-1] {
				return fmt.Errorf("leaf page footer term offsets not strictly increasing")
			}
			termStarts = append(termStarts, cum)
		}
	}

	pos := 4
	leadEnd := szLeaf
	if len(termStarts) > 0 {
		leadEnd = termStarts[0]
	}

	// Finish an in-flight poslist carried over from the previous page, if
	// any (fts5_index.go: "a page whose first content is a poslist
	// continuation" -- verified via a 400-row single-term table with pgsz=32,
	// where every page after the first begins mid-poslist).
	enteredMidPoslist := st.owedBytes > 0
	if enteredMidPoslist {
		take := st.owedBytes
		if avail := leadEnd - pos; take > avail {
			take = avail
		}
		if take < 0 {
			return fmt.Errorf("leaf page too short to continue an in-flight posting")
		}
		if take > 0 {
			newPending, err := fts5decodePoslistChunk(st.owedPending, page[pos:pos+take], detail, &st.owedAwaitingColumn, &st.owedCol, &st.owedPos, &st.owedPositions)
			if err != nil {
				return err
			}
			st.owedPending = newPending
			pos += take
		}
		st.owedBytes -= take
		if st.owedBytes > 0 {
			if pos != szLeaf || leadEnd != szLeaf {
				return fmt.Errorf("in-flight posting does not fit before the next term")
			}
			return nil
		}
		if len(st.owedPending) > 0 || st.owedAwaitingColumn {
			return fmt.Errorf("poslist ended mid-varint")
		}
		emit(st.openTerm, st.owedRowid, st.owedPositions)
		st.owedPositions = nil
	}

	// Does the open term (if any) have more postings on this page, beyond
	// whatever poslist completion just ran? Mid-poslist entry answers this
	// for free (more room before leadEnd); otherwise only h0 can (see this
	// function's doc comment).
	continues := false
	if st.haveOpenTerm {
		if enteredMidPoslist {
			continues = pos < leadEnd
		} else {
			continues = h0 != 0
		}
	}
	if !st.haveOpenTerm && h0 != 0 {
		return fmt.Errorf("leaf page header claims a continuing rowid but no term is open")
	}

	var lastTerm string
	haveLastTerm := false
	if continues {
		newPos, spilled, err := fts5decodePostingsRun(page, pos, leadEnd, szLeaf, len(termStarts) == 0, st.openTerm, detail, emit, st)
		if err != nil {
			return err
		}
		pos = newPos
		if spilled {
			return nil
		}
		if len(termStarts) == 0 {
			lastTerm, haveLastTerm = st.openTerm, true
		}
	}
	st.haveOpenTerm = false

	if pos != leadEnd {
		return fmt.Errorf("leaf page has unexplained bytes before its first term")
	}

	var prevTerm []byte
	for i, start := range termStarts {
		if pos != start {
			return fmt.Errorf("leaf page term offset does not match decoded content")
		}
		raw, newPos, err := fts5decodeTermRecord(page, pos, szLeaf, i == 0, prevTerm)
		if err != nil {
			return err
		}
		// The leading byte is the TERM SPACE: fts5MainPrefix for the main
		// index and fts5MainPrefix+1+ordinal for each prefix= index
		// (fts5_index.go). It is kept in the key rather than stripped, so the
		// prefix spaces compare against a content side that builds the same
		// keys (fts5IntegrityKeys) instead of being read as main-index terms.
		// Rejecting everything but fts5MainPrefix, which this decoder used to
		// do, made 'integrity-check' report corruption on every healthy
		// prefix= table.
		if len(raw) == 0 {
			return fmt.Errorf("term record is empty")
		}
		term := string(raw)
		prevTerm = raw
		pos = newPos

		end := szLeaf
		isLast := i == len(termStarts)-1
		if !isLast {
			end = termStarts[i+1]
		}
		newPos2, spilled, err := fts5decodePostingsRun(page, pos, end, szLeaf, isLast, term, detail, emit, st)
		if err != nil {
			return err
		}
		pos = newPos2
		if spilled {
			return nil
		}
		if isLast {
			lastTerm, haveLastTerm = term, true
		}
	}
	if pos != szLeaf {
		return fmt.Errorf("leaf page has unexplained trailing bytes")
	}
	// Whatever term's doclist ran right up to szLeaf might have more
	// postings on the next page (this function's doc comment) -- leave it
	// open with nothing owed; the next page's own h0 settles it.
	if haveLastTerm {
		st.haveOpenTerm = true
		st.openTerm = lastTerm
	}
	return nil
}

// fts5decodeTermRecord reads one term record starting at pos (which must
// equal a footer-declared offset), returning the decoded bytes (still
// carrying the leading fts5MainPrefix byte) and the position just past it.
// first is true only for the page's very first term, written in full;
// every later term is prefix-compressed against prevTerm, the immediately
// PRECEDING term on this same page (fts5_index.go: prefix compression never
// crosses a page boundary, since prevTerm is reset on every flush).
func fts5decodeTermRecord(page []byte, pos, limit int, first bool, prevTerm []byte) (term []byte, newPos int, err error) {
	if first {
		n, nn := getVarint(page[pos:limit])
		if nn == 0 {
			return nil, 0, fmt.Errorf("truncated term length")
		}
		pos += nn
		if n > uint64(limit-pos) {
			return nil, 0, fmt.Errorf("term length %d exceeds page", n)
		}
		raw := append([]byte(nil), page[pos:pos+int(n)]...)
		return raw, pos + int(n), nil
	}
	nPrefix, n1 := getVarint(page[pos:limit])
	if n1 == 0 {
		return nil, 0, fmt.Errorf("truncated term prefix length")
	}
	pos += n1
	nSuffix, n2 := getVarint(page[pos:limit])
	if n2 == 0 {
		return nil, 0, fmt.Errorf("truncated term suffix length")
	}
	pos += n2
	if nPrefix > uint64(len(prevTerm)) {
		return nil, 0, fmt.Errorf("term prefix %d longer than the previous term", nPrefix)
	}
	if nSuffix > uint64(limit-pos) {
		return nil, 0, fmt.Errorf("term suffix %d exceeds page", nSuffix)
	}
	raw := make([]byte, 0, nPrefix+nSuffix)
	raw = append(raw, prevTerm[:nPrefix]...)
	raw = append(raw, page[pos:pos+int(nSuffix)]...)
	return raw, pos + int(nSuffix), nil
}

// fts5decodePostingsRun decodes postings for one term in [pos, end) of page,
// calling emit for each. end is the next term's offset or szLeaf; isLastRun
// says end == szLeaf, and only such a run may spill its last poslist onto the
// next page (a doclist never runs into the next term within a page). A
// spilled run leaves its state in st and returns spilled=true; the caller
// stops decoding the page.
//
// The first posting of a run has an absolute rowid (first of its term or of its
// page); later ones are deltas.
//
// Under detail=none there is no size varint: the entry is the rowid,
// optionally followed by one or two 0x00 delete markers (fts5SegIterLoadNPos):
//
//	no 0x00        bDel=0, nPos=1   -- live
//	0x00           bDel=1, nPos=0   -- removed
//	0x00 0x00      bDel=1, nPos=1   -- removed and re-added in the same flush
//
// A zero rowid delta is impossible, so the leading 0x00 is unambiguous. nPos==1
// emits one synthetic position (column 0, offset 0), nPos==0 none, so the
// "empty winning poslist means absent" rule carries over, and the content side
// emits the same synthetic position.
//
// Under detail=columns the size varint sizes a column list instead;
// fts5decodePoslistChunk decodes it into the same col<<32 shape with offset 0.
func fts5decodePostingsRun(page []byte, pos, end, szLeaf int, isLastRun bool, term string, detail fts5Detail, emit func(string, int64, []int64), st *fts5decPageState) (newPos int, spilled bool, err error) {
	prevRowid := int64(0)
	first := true
	for pos < end {
		v, n := getVarint(page[pos:end])
		if n == 0 {
			return pos, false, fmt.Errorf("truncated rowid varint")
		}
		pos += n
		var rowid int64
		if first {
			rowid = int64(v)
		} else {
			rowid = prevRowid + int64(v)
		}
		first = false
		prevRowid = rowid

		if detail == fts5DetailNone {
			nPos := 1
			if pos < end && page[pos] == 0 {
				pos++
				nPos = 0
				if pos < end && page[pos] == 0 {
					pos++
					nPos = 1
				}
			}
			var positions []int64
			if nPos == 1 {
				positions = []int64{0}
			}
			emit(term, rowid, positions)
			continue
		}

		sv, n2 := getVarint(page[pos:end])
		if n2 == 0 {
			return pos, false, fmt.Errorf("truncated posting-size varint")
		}
		pos += n2
		if sv > 1<<32 {
			return pos, false, fmt.Errorf("implausible poslist size")
		}
		nBytes := int(sv >> 1)

		avail := end - pos
		take := nBytes
		spill := false
		if take > avail {
			take = avail
			spill = true
		}
		var col, posState int64
		var awaitingColumn bool
		var positions []int64
		var pending []byte
		if take > 0 {
			var perr error
			pending, perr = fts5decodePoslistChunk(nil, page[pos:pos+take], detail, &awaitingColumn, &col, &posState, &positions)
			if perr != nil {
				return pos, false, perr
			}
		}
		pos += take

		if !spill {
			if len(pending) > 0 || awaitingColumn {
				return pos, false, fmt.Errorf("poslist ended mid-varint")
			}
			emit(term, rowid, positions)
			continue
		}
		if !isLastRun || pos != szLeaf {
			return pos, false, fmt.Errorf("posting's poslist does not fit before the next term")
		}
		st.haveOpenTerm = true
		st.openTerm = term
		st.owedBytes = nBytes - take
		st.owedRowid = rowid
		st.owedPending = pending
		st.owedAwaitingColumn = awaitingColumn
		st.owedCol = col
		st.owedPos = posState
		st.owedPositions = positions
		return pos, true, nil
	}
	return pos, false, nil
}

// fts5decodePoslistChunk decodes pending (an incomplete varint from a previous
// call) followed by chunk, appending each position (col<<32|pos, as
// fts5EncodePoslist takes them) to *positions. col, pos and awaitingColumn
// (the 0x01 escape seen, column still due) carry state; the caller zeroes them
// for a fresh posting and threads them across pages.
//
// It returns any trailing bytes that are not yet a complete varint. That is
// not an error by itself; it is corruption only if more bytes were expected
// and none come.
//
// A real position delta is biased by +2 and never negative within a column, so
// a bare value of 1 is unambiguously the "column changed" escape. A
// detail=columns collist uses the same +2-biased accumulation over column
// numbers (sqlite3Fts5PoslistNext64, fts5_vocab.c's FTS5_DETAIL_COLUMNS arm)
// with no escape, so a value below 2 there is corruption.
func fts5decodePoslistChunk(pending, chunk []byte, detail fts5Detail, awaitingColumn *bool, col, pos *int64, positions *[]int64) (newPending []byte, err error) {
	buf := chunk
	if len(pending) > 0 {
		buf = append(append([]byte(nil), pending...), chunk...)
	}
	i := 0
	for i < len(buf) {
		v, n := getVarint(buf[i:])
		if n == 0 {
			// Not a complete varint yet -- carry the tail forward rather
			// than erroring; only the caller knows whether more bytes are
			// actually still due.
			return append([]byte(nil), buf[i:]...), nil
		}
		i += n
		if detail == fts5DetailColumns {
			if v < 2 {
				return nil, fmt.Errorf("collist column delta out of range")
			}
			*pos += int64(v) - 2
			*positions = append(*positions, (*pos)<<32)
			continue
		}
		if *awaitingColumn {
			*col = int64(v)
			*pos = 0
			*awaitingColumn = false
			continue
		}
		if v == 1 {
			*awaitingColumn = true
			continue
		}
		if v < 2 {
			return nil, fmt.Errorf("poslist position delta out of range")
		}
		*pos += int64(v) - 2
		*positions = append(*positions, (*col)<<32|*pos)
	}
	return nil, nil
}

// fts5mergeKey identifies one (term, rowid) pair across every segment that
// mentions it.
type fts5mergeKey struct {
	term  string
	rowid int64
}

// fts5DecodeAllPostings decodes every segment dataTbl's structure record lists
// into the table's final postings: highest segid wins per (term, rowid), and an
// empty winning poslist means absent.
//
// A missing or unreadable blob (the structure record or any implied leaf page)
// is "fts5: corruption found reading blob <id> from table <name>"; a structure
// record that is read but does not parse is "fts5: corrupt structure record
// for table <name>".
func fts5DecodeAllPostings(dataTbl *tableMeta, tableName string, detail fts5Detail) (map[fts5mergeKey][]int64, error) {
	blocks := make(map[int64][]byte, dataTbl.rows.len())
	for id, rec := range dataTbl.rows.all() {
		if blk, ok := fts5RowBlockBytes(rec); ok {
			blocks[int64(id)] = blk
		}
	}
	return fts5DecodePostingsFrom(blocks, tableName, detail)
}

// fts5DecodePostingsFrom is fts5DecodeAllPostings over %_data blocks already
// read out of a snapshot (fts5_extcontent.go's fts5ExtShadowsOf reads them that
// way, and fts5_contentless.go decodes them from a READ pager, where there is
// no tableMeta to walk).
func fts5DecodePostingsFrom(blocks map[int64][]byte, tableName string, detail fts5Detail) (map[fts5mergeKey][]int64, error) {
	final, _, err := fts5DecodePostingsStructure(blocks, tableName, detail)
	return final, err
}

// fts5DecodePostingsStructure is fts5DecodePostingsFrom returning the segment
// list it walked too, which the contentless_delete reader needs for the origin
// counter (fts5_contentless.go).
func fts5DecodePostingsStructure(blocks map[int64][]byte, tableName string, detail fts5Detail) (map[fts5mergeKey][]int64, []fts5DecSegment, error) {
	sblob, ok := blocks[fts5StructureRowid]
	if !ok {
		return nil, nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", fts5StructureRowid, tableName)
	}
	segs, _, err := fts5DecodeStructure(sblob)
	if err != nil {
		return nil, nil, fmt.Errorf("fts5: corrupt structure record for table %q", tableName)
	}

	type rawPosting struct {
		segid     int64
		term      string
		rowid     int64
		positions []int64
	}
	var raws []rawPosting
	totalPages := 0
	// A cap against a structure record that describes far more pages than
	// any real database (this engine's own, or a corrupted one small enough
	// to fit in %_data at all) could plausibly hold -- never reached by a
	// legitimate file, just a backstop against a pathological one.
	const maxTotalPages = 1 << 21
	const maxSegid = 1 << 32

	for _, seg := range segs {
		if seg.segid < 0 || seg.segid > maxSegid {
			return nil, nil, fmt.Errorf("fts5: corrupt structure record for table %q", tableName)
		}
		if seg.pgnoLast < seg.pgnoFirst {
			continue
		}
		if seg.pgnoFirst < 1 || seg.pgnoLast >= 1<<fts5DataPageB {
			return nil, nil, fmt.Errorf("fts5: corrupt structure record for table %q", tableName)
		}
		st := &fts5decPageState{}
		for pgno := seg.pgnoFirst; pgno <= seg.pgnoLast; pgno++ {
			totalPages++
			if totalPages > maxTotalPages {
				return nil, nil, fmt.Errorf("fts5: corrupt structure record for table %q", tableName)
			}
			id := fts5segmentBlockID(seg.segid, pgno)
			page, pok := blocks[id]
			if !pok {
				return nil, nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", id, tableName)
			}
			segid := seg.segid
			if derr := fts5decodeLeafPage(page, st, detail, func(term string, rowid int64, positions []int64) {
				raws = append(raws, rawPosting{segid: segid, term: term, rowid: rowid, positions: positions})
			}); derr != nil {
				return nil, nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", id, tableName)
			}
		}
		if st.owedBytes > 0 {
			// The segment's declared last page ended mid-posting with no
			// further page to continue onto: point the error at the page
			// that would have had to exist.
			id := fts5segmentBlockID(seg.segid, seg.pgnoLast+1)
			return nil, nil, fmt.Errorf("fts5: corruption found reading blob %d from table %q", id, tableName)
		}
	}

	// A TOMBSTONED entry is one C fts5's own iterator steps straight past
	// (fts5MultiIterIsDeleted), so it must not reach the merge below: a
	// contentless_delete=1 DELETE leaves the postings in place and records the
	// rowid in the segment's tombstone hash instead, and the same rowid can be
	// live in a LATER segment at the same time (verified: insert, delete,
	// re-insert leaves segid 1 holding a tombstoned rowid 1 and segid 2 holding
	// a live one).
	tomb := &fts5TombstoneReader{blocks: blocks, table: tableName}
	winner := map[fts5mergeKey]int64{}
	final := map[fts5mergeKey][]int64{}
	for _, r := range raws {
		k := fts5mergeKey{r.term, r.rowid}
		if cur, ok := winner[k]; ok && cur >= r.segid {
			continue
		}
		dead, terr := tomb.deleted(segs, r.segid, r.rowid)
		if terr != nil {
			return nil, nil, terr
		}
		if dead {
			continue
		}
		winner[k] = r.segid
		if len(r.positions) == 0 {
			delete(final, k)
		} else {
			final[k] = r.positions
		}
	}
	return final, segs, nil
}

// fts5TombstoneReader answers "is this rowid tombstoned in this segment?"
// against a contentless_delete=1 index's tombstone hash pages, memoized per
// (segid, rowid).
//
// The format is fts5_index.c's ("6. Tombstone Hash Page", read via
// fts5IndexTombstoneQuery): a segment owns nPgTombstone pages at %_data ids
// FTS5_TOMBSTONE_ROWID(segid, iPg) == (segid+65536)<<37 | iPg; rowid R lives on
// page R % nPgTombstone, probing from slot (R / nPgTombstone) % nSlot forward
// over non-zero slots. The 8-byte header is [key size (4 or 8), rowid-0 flag,
// 2 unused, 4-byte big-endian count], and nSlot is (len-8)/keysize -- except a
// page of 16 bytes or fewer holds exactly one slot.
type fts5TombstoneReader struct {
	blocks map[int64][]byte
	table  string
	cache  map[[2]int64]bool
}

// fts5TombstoneRowid is FTS5_TOMBSTONE_ROWID: the %_data id of tombstone hash
// page ipg of segment segid. fts5 addresses them out of a SECOND segment-id
// space 65536 above the leaves' own, which is why a tombstone page can never
// collide with a leaf.
func fts5TombstoneRowid(segid, ipg int64) int64 {
	return fts5segmentBlockID(segid+(1<<16), ipg)
}

// deleted reports whether segment segid holds a tombstone for rowid.
func (r *fts5TombstoneReader) deleted(segs []fts5DecSegment, segid, rowid int64) (bool, error) {
	if r.cache == nil {
		r.cache = map[[2]int64]bool{}
	}
	key := [2]int64{segid, rowid}
	if v, ok := r.cache[key]; ok {
		return v, nil
	}
	nPg := int64(0)
	for _, s := range segs {
		if s.segid == segid {
			nPg = s.nPgTombstone
			break
		}
	}
	if nPg <= 0 {
		r.cache[key] = false
		return false, nil
	}
	iPg := int64(uint64(rowid) % uint64(nPg))
	id := fts5TombstoneRowid(segid, iPg)
	page, ok := r.blocks[id]
	if !ok {
		return false, fmt.Errorf("fts5: corruption found reading blob %d from table %q", id, r.table)
	}
	out, err := fts5TombstonePageHas(page, nPg, rowid)
	if err != nil {
		return false, fmt.Errorf("fts5: corruption found reading blob %d from table %q", id, r.table)
	}
	r.cache[key] = out
	return out, nil
}

// fts5TombstonePageHas is fts5IndexTombstoneQuery over one hash page. nPg is
// the segment's page count (the same divisor that chose this page), which is
// what the slot index is derived from.
func fts5TombstonePageHas(page []byte, nPg, rowid int64) (bool, error) {
	if len(page) < 8 {
		return false, fmt.Errorf("tombstone hash page shorter than its 8-byte header")
	}
	szKey := 8
	if page[0] == 4 {
		szKey = 4
	}
	nSlot := 1
	if len(page) > 16 {
		nSlot = (len(page) - 8) / szKey
	}
	if nSlot < 1 {
		return false, fmt.Errorf("tombstone hash page has no slots")
	}
	if rowid == 0 {
		// Rowid 0 hashes to slot 0 of every page, which would be
		// indistinguishable from an empty slot, so fts5 keeps it in the header
		// flag byte instead.
		return page[1] != 0, nil
	}
	iSlot := int(uint64(rowid) / uint64(nPg) % uint64(nSlot))
	for nCollide := nSlot; ; nCollide-- {
		off := 8 + iSlot*szKey
		if off+szKey > len(page) {
			return false, fmt.Errorf("tombstone hash slot %d past the end of a %d-byte page", iSlot, len(page))
		}
		var v uint64
		if szKey == 4 {
			v = uint64(binary.BigEndian.Uint32(page[off:]))
		} else {
			v = binary.BigEndian.Uint64(page[off:])
		}
		if v == 0 {
			return false, nil
		}
		if v == uint64(rowid) {
			return true, nil
		}
		if nCollide == 0 {
			return false, nil
		}
		iSlot = (iSlot + 1) % nSlot
	}
}

// fts5RowBlockBytes extracts a %_data row's "block" BLOB column (record
// shape [id, block], see fts5_shadow.go's createShadowTables) as raw bytes.
// It accepts TEXT too (SQLite's blob-affinity column keeps a value in
// whatever storage class it was written with, and sqlite3_column_blob
// reads either one the same way), and reports false for anything else --
// an integer, float or NULL block, from a row hand-corrupted at the SQL
// level, is not readable as index bytes at all.
func fts5RowBlockBytes(rec []Value) ([]byte, bool) {
	if len(rec) < 2 {
		return nil, false
	}
	switch rec[1].Typ {
	case Blob, Text:
		return rec[1].S, true
	default:
		return nil, false
	}
}

// fts5integrityCheckContent re-tokenizes content (as fts5SyncShadows does) into
// fts5DecodeAllPostings' shape, checks every row's %_docsize blob against the
// token counts, and returns the totals the averages record should hold.
//
// A %_docsize mismatch or a %_content/%_docsize row-set mismatch is "database
// disk image is malformed"; a postings disagreement with a well-formed index is
// "fts5: checksum mismatch" instead.
//
// detail picks the posting shape so both sides compare alike: (col, pos) pairs
// for full, one entry per (term, rowid, column) for columns, one synthetic
// position per (term, rowid) for none -- as fts5StorageIntegrityCallback feeds
// (iCol, szCol-1), (0, iCol) and (0, 0) into its checksum, deduplicating
// through a per-row or per-column Fts5Termset.
func fts5integrityCheckContent(content, docsize *tableMeta, st *fts5Store) (postings map[fts5mergeKey][]int64, nRow int64, colTotals []int64, err error) {
	const corruptShadow = "database disk image is malformed"
	nCol := len(st.colNames)
	detail := st.detail
	// %_docsize exists only on a columnsize=1 table; a columnsize=0 one never
	// had it (fts5_shadow.go's createShadowTables), and C fts5 skips the
	// per-row size check there rather than reporting the absence as corruption
	// -- fts5_storage.c's integrity check guards every aColSize use with
	// "if( pConfig->bColumnsize )".
	if docsize != nil && content.rows.len() != docsize.rows.len() {
		return nil, 0, nil, fmt.Errorf(corruptShadow)
	}
	result := map[fts5mergeKey][]int64{}
	totals := make([]int64, nCol)
	for rid, rec := range content.rows.all() {
		sizes := make([]int, nCol)
		for i := 0; i < nCol; i++ {
			// An UNINDEXED column contributes NO tokens, no postings and a 0 in
			// its %_docsize and averages slots -- fts5_storage.c walks it with
			// "if( pConfig->abUnindexed[i]==0 )", and fts5_shadow.go's rebuild
			// already skips it. Tokenizing it here counted its tokens into both
			// totals, so the check reported "database disk image is malformed"
			// for every healthy table declaring one.
			if st.unindexed[i] {
				continue
			}
			var v Value
			if i+1 < len(rec) {
				v = rec[i+1]
			}
			toks := st.tok.tokenize(valueToText(v))
			sizes[i] = len(toks)
			totals[i] += int64(len(toks))
			for pos, tok := range toks {
				for _, k := range fts5IntegrityKeys(tok, int64(rid), st.prefixes) {
					switch detail {
					case fts5DetailNone:
						// One synthetic position per (term, rowid), however many
						// times or in however many columns the term occurs.
						if len(result[k]) == 0 {
							result[k] = []int64{0}
						}
					case fts5DetailColumns:
						// One entry per COLUMN the term occurs in. Columns are
						// walked in ascending order and a repeat within one column
						// adds nothing, so the last entry is the only one to test.
						if n := len(result[k]); n == 0 || result[k][n-1] != int64(i)<<32 {
							result[k] = append(result[k], int64(i)<<32)
						}
					default:
						result[k] = append(result[k], int64(i)<<32|int64(pos))
					}
				}
			}
		}

		if docsize == nil {
			continue
		}
		dsRec, ok := docsize.rows.get(rid)
		dsBlob, bok := fts5RowBlockBytes(dsRec)
		if !ok || !bok {
			return nil, 0, nil, fmt.Errorf(corruptShadow)
		}
		b := dsBlob
		for i := 0; i < nCol; i++ {
			v, n := getVarint(b)
			if n == 0 {
				return nil, 0, nil, fmt.Errorf(corruptShadow)
			}
			b = b[n:]
			if int(v) != sizes[i] {
				return nil, 0, nil, fmt.Errorf(corruptShadow)
			}
		}
	}
	return result, int64(content.rows.len()), totals, nil
}

// fts5IntegrityKeys returns the (term-space term, rowid) keys one token
// contributes to the index: the main-index one, plus one per prefix= index the
// token is long enough for. It is fts5BuildIndex's own term construction,
// reused so both sides of the integrity comparison speak the same keys --
// including the leading term-space byte, which fts5decodeLeafPage now keeps.
//
// fts5_storage.c does the same thing with a per-prefix sqlite3Fts5TermsetAdd
// (its "for(ii=0; ii<pConfig->nPrefix; ii++)" loop over
// sqlite3Fts5IndexCharlenToBytelen), which is what fts5PrefixByteLen is.
func fts5IntegrityKeys(tok string, rowid int64, prefixes []int) []fts5mergeKey {
	keys := []fts5mergeKey{{string(rune(fts5MainPrefix)) + tok, rowid}}
	for pi, k := range prefixes {
		nb, ok := fts5PrefixByteLen(tok, k)
		if !ok {
			continue
		}
		keys = append(keys, fts5mergeKey{string(byte(fts5MainPrefix+1+pi)) + tok[:nb], rowid})
	}
	return keys
}

// fts5decodeAveragesLenient reads the averages record (fts5_index.go: varint
// nRow, then one varint per column). It never errors -- verified: even
// unparsable garbage there (a 3-byte all-continuation-bit blob that cannot
// possibly hold a complete varint) produces no structural complaint from
// C fts5, only a downstream mismatch once the totals it implies disagree
// with a fresh count -- so a short or malformed blob here decodes whatever
// prefix it can and leaves the rest at zero, the same convention already
// documented for a freshly created table's empty averages blob.
func fts5decodeAveragesLenient(blob []byte, nCol int) (nRow int64, colTotals []int64) {
	colTotals = make([]int64, nCol)
	v, n := getVarint(blob)
	if n == 0 {
		return 0, colTotals
	}
	nRow = int64(v)
	b := blob[n:]
	for i := 0; i < nCol; i++ {
		v, n := getVarint(b)
		if n == 0 {
			break
		}
		colTotals[i] = int64(v)
		b = b[n:]
	}
	return nRow, colTotals
}

// fts5postingsEqual reports whether a and b hold identical (term, rowid) ->
// positions maps. Both sides build their position lists in the same
// column-ascending, position-ascending order (fts5BuildIndex's own
// iteration order on the content side; strictly increasing offsets within a
// page and never-reordered decode on the index side), so a plain
// element-wise compare is exact -- no sort needed on either side.
func fts5postingsEqual(a, b map[fts5mergeKey][]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, pa := range a {
		pb, ok := b[k]
		if !ok || len(pa) != len(pb) {
			return false
		}
		for i := range pa {
			if pa[i] != pb[i] {
				return false
			}
		}
	}
	return true
}

// fts5IntegrityCheck implements the 'integrity-check' command
// (fts5_shadow.go's fts5CommandInsert): C fts5's own check hashes every
// (rowid, column, position, term) posting derivable from %_content against
// every posting %_data's segments actually decode to, and errors if they
// differ. This reproduces the same comparison directly (a map compare
// rather than a checksum -- there is no need to match fts5's own hash
// function when the two full posting sets can just be built and compared),
// and reports the same outcomes the oracle does depending on where the two
// sides first disagree -- see fts5DecodeAllPostings's and
// fts5integrityCheckContent's comments for each one's verification.
func (db *DB) fts5IntegrityCheck(vm *vtabMeta) error {
	st, ok := vm.store.(*fts5Store)
	if !ok {
		return fmt.Errorf("engine: internal error: fts5 table %s has no backing store", vm.name)
	}
	// An EXTERNAL-CONTENT table has no %_content to compare the index against.
	// Real fts5's own check does not compare one either unless the command was
	// given a nonzero argument -- fts5_storage.c: "bUseCksum =
	// (eContent==NORMAL || (eContent==EXTERNAL && iArg))". What it does check,
	// that the index decodes and its totals agree, this engine has already had
	// to establish before it could touch the table at all: fts5ExtVerify proved
	// the whole index is the one it builds from the content table's rows
	// (fts5_extcontent.go), which is strictly stronger.
	if st.extContent != "" {
		return st.fts5ExtEnsureVerified()
	}
	// A CONTENTLESS table has no %_content either, and bUseCksum is FALSE for it
	// unconditionally (the EXTERNAL arm above is the only one an argument can
	// turn on), so C fts5's check over one is the STRUCTURAL half alone:
	// sqlite3Fts5IndexIntegrityCheck with a zero checksum, which walks the
	// segments and reports corruption only if they do not decode. That is
	// exactly this decode.
	if st.contentless {
		if st.contentlessErr != nil {
			return st.contentlessErr
		}
		data := db.findTableMeta(vm.name + "_data")
		if data == nil {
			return fmt.Errorf("engine: fts5 table %s is missing a shadow table (%%_data)", vm.name)
		}
		_, err := fts5DecodeAllPostings(data, vm.name, st.detail)
		return err
	}
	content := db.findTableMeta(vm.name + "_content")
	docsize := db.findTableMeta(vm.name + "_docsize")
	data := db.findTableMeta(vm.name + "_data")
	// %_docsize is absent on a columnsize=0 table by construction, so its
	// absence is not a missing shadow table -- fts5integrityCheckContent skips
	// the per-row size check when it is nil.
	if content == nil || data == nil || (st.columnsize && docsize == nil) {
		return fmt.Errorf("engine: fts5 table %s is missing a shadow table (%%_content/%%_docsize/%%_data)", vm.name)
	}
	if !st.columnsize {
		docsize = nil
	}
	nCol := len(st.colNames)

	contentPostings, wantRow, wantTotals, err := fts5integrityCheckContent(content, docsize, st)
	if err != nil {
		return err
	}

	if arec, ok := data.rows.get(uint64(fts5AveragesRowid)); ok {
		if ablob, bok := fts5RowBlockBytes(arec); bok {
			gotRow, gotTotals := fts5decodeAveragesLenient(ablob, nCol)
			if gotRow != wantRow {
				return fmt.Errorf("database disk image is malformed")
			}
			for i := range wantTotals {
				if gotTotals[i] != wantTotals[i] {
					return fmt.Errorf("database disk image is malformed")
				}
			}
		}
		// A block that isn't even readable as bytes (an INTEGER/FLOAT/NULL
		// hand-corrupted at the SQL level) falls through to the segment walk
		// below instead of erroring here -- the averages record on its own
		// is never load-bearing for anything this engine reads (bm25 is
		// unreachable, vtab_fts5.go's package comment), so a corruption
		// confined to it AND not also reflected in the postings/docsize
		// totals above is the one case this check does not chase further.
	}

	indexPostings, err := fts5DecodeAllPostings(data, vm.name, st.detail)
	if err != nil {
		return err
	}
	if !fts5postingsEqual(contentPostings, indexPostings) {
		return fmt.Errorf("fts5: checksum mismatch for table %q", vm.name)
	}
	return nil
}
