// This file implements fts3/fts4 segment merging: when a level reaches
// FTS3_MERGE_COUNT (16), its segments merge one level up.
//
// # WHEN it fires, and where the output goes
//
// A new segment's index at a level is one past the highest already there. When
// that reaches FTS3_MERGE_COUNT (16), the 16 segments at that level are instead
// merged into ONE segment at the level ABOVE, and the new segment takes idx 0:
//
//	16 single-row INSERTs -> L0 idx 0..15
//	the 17th             -> L1 idx 0 holding docids 1..16, L0 idx 0 holding 17
//
// The allocation at the level above goes through the same rule, so it cascades.
// A DELETE or UPDATE allocates its index the same way.
//
// # Delete markers are dropped ONLY when nothing older survives
//
// A merge whose output level is ABOVE every level currently present drops the
// delete markers it read (there is no older segment left for one to mask), and
// a term left with no documents at all disappears with them:
//
//	5 INSERTs, DELETE docid 3, UPDATE docid 2, 9 more INSERTs, then the 17th
//	  -> the L1 segment's "common" doclist holds 1,4,5,...,14 -- docid 2 and
//	     docid 3 are simply GONE, not present as markers
//
// But a merge into a level that already holds a segment KEEPS them, because
// that segment is older and still holds the row:
//
//	17 INSERTs (so L1 idx 0 exists), DELETE docid 5, 15 more INSERTs, then one
//	  more -> L0 merges into L1 idx 1, and "MATCH common" still omits docid 5
//
// The distinguishing case -- output at L1 idx 0 with level-1 otherwise EMPTY
// and a level-2 segment present, reachable only through a full cascade -- was
// probed directly: the marker is KEPT there. So the rule is the OUTPUT LEVEL
// against the levels present, never the output idx.
//
// # The output's blocks are numbered BEFORE the inputs are freed
//
// Output blocks are numbered before inputs are freed: write the output first,
// then drop the inputs, so the "one past the largest blockid" rule works.
package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// fts3MaxMergeDepth bounds the cascade. Sixteen segments per level means the
// levels a real table reaches are tiny (level 2 needs 272 flushes, level 3
// needs 4352), but %_segdir is an ordinary table a test can write into
// directly, so a hand-built one must hit an error rather than recurse forever.
const fts3MaxMergeDepth = 64

// fts3SegdirRow is one %_segdir row decoded from the write path's in-memory
// table, carrying the rowid so a merge can drop exactly the rows it consumed.
type fts3SegdirRow struct {
	rowid                           uint64
	level, idx                      int64
	startBlock, leavesEnd, endBlock int64
	root                            []byte
}

// fts3EndBlockID reads the LAST BLOCK ID out of an end_block value. The column
// holds TEXT "<last block id> <total leaf bytes>" (see fts3StoreSegment for the
// oracle evidence), but a hand-written shadow table can hold a bare integer, so
// both are accepted; anything else yields 0, which makes the block range below
// empty rather than wild.
func fts3EndBlockID(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Text, Blob:
		s := string(v.S)
		if i := strings.IndexByte(s, ' '); i >= 0 {
			s = s[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// fts3SegdirRowsOf decodes every %_segdir row, ordered by (level, idx).
func fts3SegdirRowsOf(segdir *tableMeta) ([]fts3SegdirRow, error) {
	out := make([]fts3SegdirRow, 0, segdir.rows.len())
	for rid, row := range segdir.rows.all() {
		if len(row) < 6 {
			return nil, fmt.Errorf("engine: fts3: %%_segdir row has %d columns, expected 6", len(row))
		}
		if row[0].Typ != Int || row[1].Typ != Int {
			return nil, fmt.Errorf("engine: fts3: %%_segdir row has a non-integer level/idx")
		}
		s := fts3SegdirRow{rowid: rid, level: row[0].I, idx: row[1].I}
		if row[2].Typ == Int {
			s.startBlock = row[2].I
		}
		if row[3].Typ == Int {
			s.leavesEnd = row[3].I
		}
		s.endBlock = fts3EndBlockID(row[4])
		switch row[5].Typ {
		case Blob, Text:
			s.root = row[5].S
		case Null:
		default:
			return nil, fmt.Errorf("engine: fts3: %%_segdir root is not a blob")
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].level != out[j].level {
			return out[i].level < out[j].level
		}
		return out[i].idx < out[j].idx
	})
	return out, nil
}

// fts3ReadSegmentInto folds one whole segment's terms into byTerm. Callers pass
// segments NEWEST first, which is what fts3MergeDoclist's newest-wins
// resolution (and its tombstones) expects. desc is the table's "order=desc"
// bit, which is how the stored doclists' deltas run.
func fts3ReadSegmentInto(segments *tableMeta, s fts3SegdirRow, byTerm map[string]fts3TermPostings, desc bool) error {
	var nodes [][]byte
	if s.startBlock == 0 {
		// No %_segments blocks at all: the root IS the one leaf.
		nodes = [][]byte{s.root}
	} else {
		if segments == nil {
			return fmt.Errorf("engine: fts3: a segment spills into %%_segments, which this table is missing")
		}
		if s.leavesEnd < s.startBlock {
			// fts3SegReaderNext's own EOF-before-any-read check
			// (fts3_write.c:1390): a non-root reader's iCurrentBlock is
			// seeded to iStartLeaf-1 (sqlite3Fts3SegReaderNew,
			// fts3_write.c:1666-1668), and the VERY FIRST call to
			// fts3SegReaderNext tests "iCurrentBlock>=iLeafEndBlock" --
			// true here since leaves_end_block<start_block -- and returns
			// SQLITE_OK (EOF) BEFORE ever touching %_segments. So this
			// segment silently contributes ZERO terms, never an error, to
			// any reader built straight off the raw %_segdir row with no
			// term-narrowing: an ordinary merge cascade, 'optimize', or
			// 'integrity-check' (fts3_merge.go's fts3MergeLevel,
			// fts3_command.go's fts3OptimizeIndex and its
			// integrity-check scan -- every caller of this function). A
			// plain "MATCH term" query goes through a DIFFERENT path
			// (fts3SelectLeaf, fts3.c:3025-3029, which narrows by term
			// BEFORE ever constructing a reader) and is unaffected -- see
			// fts3_search.go's own separate copy of this loop, which
			// still errors for this shape and must keep doing so.
			return nil
		}
		for b := s.startBlock; b <= s.leavesEnd; b++ {
			row, ok := segments.rows.get(uint64(b))
			if !ok || len(row) < 2 || (row[1].Typ != Blob && row[1].Typ != Text) {
				return fmt.Errorf("engine: fts3: %%_segments is missing block %d", b)
			}
			nodes = append(nodes, row[1].S)
		}
	}
	for _, node := range nodes {
		// A MERGE, like C fts3's own, always needs the whole node -- never
		// narrowed to particular query tokens -- so the visit closure never
		// signals stop. allowByteLengthMatch=false: this is the WRITE path,
		// which re-encodes from decoded positions and has no way to preserve
		// a doclist entry it could not decode -- see fts3MergeDoclist's own
		// comment.
		if err := fts3DecodeLeafNode(node, func(term string, doclist []byte, nDoclist int) (bool, error) {
			return false, fts3MergeDoclist(byTerm, term, doclist, nDoclist, desc, false)
		}); err != nil {
			return err
		}
	}
	return nil
}

// fts3PendingTermsFrom rebuilds a flushable term set from resolved postings.
// A nil postings entry is a delete marker: kept as one (the docid appears with
// an empty position list) unless ignoreEmpty, in which case it is dropped, and
// a term all of whose documents drop that way never gets created at all.
func fts3PendingTermsFrom(byTerm map[string]fts3TermPostings, ignoreEmpty bool, desc bool) *fts3PendingTerms {
	pt := newFts3PendingTerms(desc)
	for term, tp := range byTerm {
		docids := make([]int64, 0, len(tp))
		for d := range tp {
			docids = append(docids, d)
		}
		sort.Slice(docids, func(i, j int) bool { return docids[i] < docids[j] })
		for _, docid := range docids {
			cols := tp[docid]
			if cols == nil {
				if !ignoreEmpty {
					pt.addDelete(term, docid)
				}
				continue
			}
			colIdx := make([]int, 0, len(cols))
			for c := range cols {
				colIdx = append(colIdx, c)
			}
			sort.Ints(colIdx)
			for _, c := range colIdx {
				for _, pos := range cols[c] {
					pt.add(term, docid, c, pos)
				}
			}
		}
	}
	return pt
}

// fts3AllocateSegdirIdx is fts3AllocateSegdirIdx: one past the highest idx at
// level, or -- when that has reached fts3MergeCount -- 0, after merging the
// level into the one above. The merge allocates ITS index the same way, so this
// pair recurses exactly as C fts3's does.
//
// cascadeLeaves is the number of LEAF NODES a triggered cascade wrote (0 when
// none was needed) -- p->nLeafAdd's own contribution from whatever merge(s)
// this allocation set off (fts3_write.c:2320/2435 fire inside a cascade's
// own fts3SegmentMerge calls exactly as they do for an ordinary flush; see
// engine/fts3_automerge.go, the caller that needs this).
func (db *DB) fts3AllocateSegdirIdx(segdir, segments *tableMeta, m fts3Module, level int64, depth int, desc bool) (idx, cascadeLeaves int64, err error) {
	rows, err := fts3SegdirRowsOf(segdir)
	if err != nil {
		return 0, 0, err
	}
	next := int64(0)
	for _, r := range rows {
		if r.level == level && r.idx+1 > next {
			next = r.idx + 1
		}
	}
	if next < fts3MergeCount {
		return next, 0, nil
	}
	cascadeLeaves, err = db.fts3MergeLevel(segdir, segments, m, level, depth, desc)
	if err != nil {
		return 0, 0, err
	}
	return 0, cascadeLeaves, nil
}

// fts3MergeLevel merges every segment at level into ONE segment at level+1,
// returning the number of LEAF NODES it wrote in doing so (its own output
// segment, if any, plus whatever a further cascade at level+1 wrote) -- see
// fts3AllocateSegdirIdx's own doc comment for why.
func (db *DB) fts3MergeLevel(segdir, segments *tableMeta, m fts3Module, level int64, depth int, desc bool) (leavesWritten int64, err error) {
	if depth >= fts3MaxMergeDepth {
		return 0, fmt.Errorf("engine: this %s table's %%_segdir is full at %d consecutive levels; merging it is not supported by this write path", m.name, depth)
	}
	rows, err := fts3SegdirRowsOf(segdir)
	if err != nil {
		return 0, err
	}
	inputs := make([]fts3SegdirRow, 0, fts3MergeCount)
	for _, r := range rows {
		if r.level == level {
			inputs = append(inputs, r)
		}
	}
	// Newest first: at one level a higher idx is the newer segment, and
	// fts3MergeDoclist resolves whatever it sees first as the winner.
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].idx > inputs[j].idx })
	byTerm := map[string]fts3TermPostings{}
	for _, in := range inputs {
		if err := fts3ReadSegmentInto(segments, in, byTerm, desc); err != nil {
			return 0, err
		}
	}

	// fts3SegmentMaxLevel's own read (fts3_write.c:3272-3275) happens BEFORE
	// fts3AllocateSegdirIdx's recursive cascade -- captured here, from the
	// `rows` already read above, before that allocation can itself cascade a
	// level further and change what %_segdir holds, so the promotion check
	// below (fts3_write.c:3330's "iNewLevel<iMaxLevel") compares against the
	// same snapshot C fts3's own iMaxLevel would have.
	origMaxLevel := level
	for _, r := range rows {
		if fts3SameSegdirIndex(r.level, level) && r.level > origMaxLevel {
			origMaxLevel = r.level
		}
	}

	// The output's index is allocated BEFORE it is written, which is what makes
	// the cascade above happen in C fts3's order.
	outIdx, cascadeLeaves, err := db.fts3AllocateSegdirIdx(segdir, segments, m, level+1, depth+1, desc)
	if err != nil {
		return 0, err
	}
	leavesWritten = cascadeLeaves
	// Re-read: a cascade may have added a level and removed level+1's segments.
	if rows, err = fts3SegdirRowsOf(segdir); err != nil {
		return 0, err
	}
	// "Above every level present" is asked WITHIN THIS INDEX: a prefix index's
	// segments live in their own 1024-level block (fts3_prefix.go) and say
	// nothing about whether an older term-index segment still holds the row.
	maxLevel := level
	for _, r := range rows {
		if fts3SameSegdirIndex(r.level, level) && r.level > maxLevel {
			maxLevel = r.level
		}
	}
	ignoreEmpty := level+1 > maxLevel

	pt := fts3PendingTermsFrom(byTerm, ignoreEmpty, desc)
	if !pt.empty() {
		img := pt.encodeSegment(int(db.pageSize)-fts3NodeOverhead, fts3NextBlockID(segments))
		if err := fts3StoreSegment(segdir, segments, level+1, outIdx, img); err != nil {
			return 0, err
		}
		leavesWritten += img.nLeaves
		// fts3_write.c:3330 -- the iLevel!=PENDING branch only promotes when
		// this merge's own output level sits BELOW the max level that already
		// existed before it ran (origMaxLevel, above): a merge whose output
		// already IS the table's tallest level for this index has nothing
		// above it to fold down.
		if level+1 < origMaxLevel {
			fts3PromoteSegments(segdir, level+1, img.nLeafData)
		}
	}
	// Only now free the inputs, so the output's block ids were numbered above
	// theirs (see this file's comment).
	for _, in := range inputs {
		segdir.dropRow(in.rowid)
		if segments == nil || in.startBlock == 0 {
			continue
		}
		for b := in.startBlock; b <= in.endBlock; b++ {
			segments.dropRow(uint64(b))
		}
	}
	return leavesWritten, nil
}
