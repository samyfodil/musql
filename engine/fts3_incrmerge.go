// This file implements sqlite3Fts3Incrmerge (fts3_write.c) -- fts3's
// data-moving incremental merge -- for the subset of its state machine this
// port reproduces byte-exact. The command channel (fts3Incrmerge) tries it
// whenever there is real work and declines the moment a step needs machinery
// not ported here. automerge (fts3_automerge.go) reuses fts3IncrmergeRun
// unchanged.
//
// # What C does (sqlite3Fts3Incrmerge, ~fts3_write.c:4951)
//
// "while( nRem>0 ){ ...one level... }": each iteration asks
// SQL_FIND_MERGE_LEVEL for the level with the smallest relative level (level %
// 1024) holding at least MAX(2,nMin) segments, merges all of them into one
// segment a level up, and loops -- so one "merge=1000,2" can cascade through
// several levels, consuming an earlier step's own output.
//
// The output segment is appendable: fts3IncrmergeWriter reserves a block range
// up front (16 * nLeafEst blocks, FTS_MAX_APPENDABLE_HEIGHT==16) and writes a
// NULL marker row at its last block. %_segdir.end_block is TEXT "<reserved
// end> <nLeafData>" -- the reserved end, not the last block written. When the
// output fits one leaf, fts3IncrmergeRelease does not use it as the root (C
// "cannot handle segments that fit entirely on the root node with
// start_block!=0"): it writes the leaf to %_segments and makes the root a
// synthetic one-child interior node ([0x01][varint firstBlock]).
//
// # Ported
//
//   - the cascading level selection across every (language, index) of a
//     "languageid="/"prefix=" table -- plain arithmetic on %_segdir.level;
//   - the reservation math (nLeafEst, iStart, iEnd) and the NULL marker row;
//   - the synthetic single-leaf root and its TEXT end_block pair;
//   - fts3SegmentIsMaxLevel's ignore-empty gate, verbatim including its
//     off-by-one-wider bound (fts3IncrmergeIsMaxLevel);
//   - full-drain input cleanup (fts3IncrmergeChomp's "aNode==0" branch),
//     including the reader-position-vs-idx quirk of fts3RemoveSegdirEntry
//     (fts3_write.c:4546-4563) and fts3RepackSegdirLevel (4565-4610), visible
//     only over a hand-corrupted %_segdir;
//   - an input with leaves_end_block<start_block (fts3corrupt4.test):
//     fts3ReadSegmentInto reads it as zero terms (fts3SegReaderNext's
//     EOF-before-read), and SQL_MAX_LEAF_NODE_ESTIMATE may go negative as in C;
//   - a merge whose result is empty after ignore-empty stripping: the
//     reservation, marker and chomp still run (fts3_write.c:5074-5089) and
//     fts3IncrmergeRelease writes no %_segdir row (4157), orphaning the marker
//     as C does.
//
// # Declined
//
//   - the resumable hint (%_stat id=1) that lets a merge stop partway and
//     resume later: this port never leaves one and declines when one exists;
//   - an output that does not fit one leaf (the multi-leaf appendable writer,
//     fts3IncrmergeWriter/Append/Push, is not ported);
//   - promotion (fts3PromoteSegments) after a merge, which only has work when
//     an earlier incremental merge left segments above this level
//     (fts3IncrmergePromotionNeeded);
//   - a tie in SQL_FIND_MERGE_LEVEL's ORDER BY between two levels (possible
//     only with several prefix/languageid indexes): C's pick follows a GROUP BY
//     temp b-tree's internal order.
//
// # Never partially applied
//
// Each cascade step depends on the state the previous one wrote, so the whole
// cascade runs against cloned %_segdir/%_segments (fts3IncrmergeSim) and
// replaces the real rows in one shot only once every step fits. Declining
// touches nothing.
package engine

import (
	"fmt"
	"sort"
)

// fts3IncrmergeRun is sqlite3Fts3Incrmerge for the subset this port
// implements (see file comment). name is the base table name, used only for
// error text. A nil return means the command fully completed -- which
// includes doing nothing at all, exactly like the prior "has no work"
// no-op -- and the real shadow tables now hold the result.
func (db *DB) fts3IncrmergeRun(name string, s *fts3Shadows, sch fts3Schema, nMerge, nMin int) error {
	// fts3DoIncrmerge's own "while( nRem>0 )": nMerge<=0 never enters the
	// loop, not even to consult the hint, so this returns before even
	// resolving %_stat.
	if nMerge <= 0 {
		return nil
	}
	if hinted, err := db.fts3IncrmergeHintPresent(name, s); err != nil {
		return err
	} else if hinted {
		return fmt.Errorf("engine: fts3 table %s: this merge would resume from a prior incremental-merge hint, which is not supported by this write path (see engine/fts3_incrmerge.go)", name)
	}
	if s.segments == nil {
		return fmt.Errorf("engine: fts3 table %s is missing its %%_segments shadow table", name)
	}

	need := nMin
	if need < 2 {
		need = 2 // fts3DoIncrmerge's own MAX(2, nMin); fts3ParseMergeParam already refuses nMin<2.
	}

	sim := newFts3IncrmergeSim(s.segdir, s.segments)
	nRem := nMerge
	didWork := false
	for nRem > 0 {
		rows, err := fts3SegdirRowsOf(sim.segdir)
		if err != nil {
			return err
		}
		level, levelRows, found, ambiguous := fts3IncrmergeSelectLevel(rows, need)
		if !found {
			break // SQL_FIND_MERGE_LEVEL found no level with >= need segments: done.
		}
		if ambiguous {
			return fmt.Errorf("engine: fts3 table %s: two or more of this table's indexes have the same relative level and segment count, so which one C fts3's own query would pick is not decidable here", name)
		}
		// levelRows is still in SQL_SELECT_LEVEL's own order here (idx
		// ASCENDING -- fts3SegdirRowsOf's baseline sort, preserved by
		// fts3IncrmergeSelectLevel's own filter-in-place loop): position i
		// in this slice is exactly the reader position fts3IncrmergeCsr
		// would assign (fts3_write.c:3690-3697, sqlite3Fts3SegReaderNew's
		// first argument), which fts3IncrmergeChomp below needs. Snapshot
		// it before the term-read sort a few lines down reorders the same
		// backing slice newest-first.
		ascRows := append([]fts3SegdirRow(nil), levelRows...)

		// SQL_MAX_LEAF_NODE_ESTIMATE (fts3_write.c:263, bound at
		// fts3IncrmergeWriter:4495-4501) is a bare "SUM(1 + leaves_end_block -
		// start_block)" with no clamp, so a segment with
		// leaves_end_block<start_block contributes a negative term and the sum
		// sizes the reservation as is. fts3corrupt4.test#20's merges reserve
		// ranges entirely below block 1 for exactly this reason.
		var nLeafEst int64
		for _, r := range levelRows {
			nLeafEst += 1 + r.leavesEnd - r.startBlock
		}
		nLeafEst *= 2

		outLevel := level + 1
		iIdx := fts3IncrmergeNextIdx(rows, outLevel)
		ignoreEmpty := false
		if iIdx == 0 {
			ignoreEmpty = fts3IncrmergeIsMaxLevel(rows, outLevel)
		}
		if needed, blocker := fts3IncrmergePromotionNeeded(rows, outLevel); needed {
			return fmt.Errorf("engine: fts3 table %s: level %d already holds a segment above the level this merge would write to, which C fts3 may PROMOTE down onto it -- not supported by this write path", name, blocker)
		}

		// Newest first, matching fts3SegReaderCursor's own resolution order
		// (fts3_merge.go's fts3SegdirNewestFirst / fts3OptimizeIndex).
		sort.Slice(levelRows, func(i, j int) bool { return levelRows[i].idx > levelRows[j].idx })
		byTerm := map[string]fts3TermPostings{}
		for _, r := range levelRows {
			if err := fts3ReadSegmentInto(sim.segments, r, byTerm, sch.descIdx); err != nil {
				return err
			}
		}
		pt := fts3PendingTermsFrom(byTerm, ignoreEmpty, sch.descIdx)
		img := pt.encodeSegment(int(db.pageSize)-fts3NodeOverhead, 0) // nextBlock: unused unless it spills
		if len(img.blocks) > 0 {
			return fmt.Errorf("engine: fts3 table %s: merging level %d would produce a segment spanning more than one leaf node, which this write path's incremental merge does not yet build (see engine/fts3_incrmerge.go)", name, level)
		}

		// Every check passed: mutate the simulation and continue the cascade.
		//
		// Order matters: fts3IncrmergeWriter reserves the new range off
		// SQL_NEXT_SEGMENTS_ID (one past the current max blockid) before
		// fts3IncrmergeChomp deletes the inputs' blocks, so the reservation
		// counts blocks the inputs still hold. %_segdir's new rowid comes after
		// (fts3IncrmergeRelease runs after chomp). So a two-level cascade's
		// second leaf continues from the first step's marker, not from block 1.
		//
		// Reservation and chomp run whether or not pt is empty. In C, bEmpty is
		// known before fts3IncrmergeWriter (fts3_write.c:5074-5078), which
		// reserves and writes the marker (4519) without checking it, and the
		// chomp (5088-5089) is not gated on it either -- only the append loop is
		// (5081-5086). Its one effect is in fts3IncrmergeRelease (4133-4157):
		// with nothing appended it returns ("Empty output segment. This is a
		// no-op."), writing no %_segdir row. So only writeLeaf/writeSegdirRow
		// are gated on !pt.empty().
		//
		// The chomp removes each input by its reader position in ascRows, not
		// its real idx (fts3IncrmergeChomp). The two coincide for every table
		// this write path grows; they differ only over a hand-corrupted %_segdir.
		iStart, iEnd := sim.reserveAppendableRange(nLeafEst)
		if err := fts3IncrmergeChomp(sim, level, ascRows); err != nil {
			return err
		}
		if !pt.empty() {
			sim.writeLeaf(iStart, img.root)
			if err := sim.writeSegdirRow(outLevel, iIdx, iStart, iEnd, img.nLeafData); err != nil {
				return err
			}
		}
		didWork = true
		nRem--
	}

	if didWork {
		sim.commit(s.segdir, s.segments)
	}
	return nil
}

// fts3IncrmergeChomp is fts3IncrmergeChomp (fts3_write.c:4789-4835) for this
// port's subset: every input is fully drained, so only the C's "aNode==0,
// remove the whole input" branch happens, and its "if( nRem!=pCsr->nSegment
// )" repack guard (4829) is always true -- fts3RepackSegdirLevel always runs.
//
// The quirk (fts3RemoveSegdirEntry's SQL_DELETE_SEGDIR_ENTRY +
// SQL_SHIFT_SEGDIR_ENTRY, fts3_write.c:4539-4563): removing input i deletes
// whichever row currently carries idx==i (i's 0-based rank in ascRows), then
// shifts rows at this level with idx>i down by one. Processed from the last i
// down to 0, so later steps see rows earlier steps renumbered. With
// non-canonical idx numbering the deletes can miss and rows survive merely
// relabelled (fts3corrupt4.test's level-0 round).
//
// %_segments cleanup (fts3DeleteSegment, fts3_write.c:2553-2568) is keyed on
// each segment's own start_block/end_block (r.endBlock -- for an appendable
// segment, the reserved range top), unaffected by the quirk.
func fts3IncrmergeChomp(sim *fts3IncrmergeSim, level int64, ascRows []fts3SegdirRow) error {
	for i := len(ascRows) - 1; i >= 0; i-- {
		r := ascRows[i]
		if r.startBlock != 0 {
			for b := r.startBlock; b <= r.endBlock; b++ {
				sim.segments.dropRow(uint64(b))
			}
		}
		rows, err := fts3SegdirRowsOf(sim.segdir)
		if err != nil {
			return err
		}
		for _, row := range rows {
			switch {
			case row.level != level:
			case row.idx == int64(i):
				sim.segdir.dropRow(row.rowid)
			case row.idx > int64(i):
				fts3SegdirRowSetIdx(sim.segdir, row.rowid, row.idx-1)
			}
		}
	}
	return fts3RepackSegdirLevel(sim, level)
}

// fts3RepackSegdirLevel is fts3RepackSegdirLevel (fts3_write.c:4565-4610):
// after the chomp, renumber what remains at level to contiguous 0-based idx in
// ascending order (SQL_SELECT_INDEXES, then SQL_SHIFT_SEGDIR_ENTRY where the
// idx differs). Ported verbatim rather than short-circuited, since a chomp can
// leave a non-contiguous remainder.
func fts3RepackSegdirLevel(sim *fts3IncrmergeSim, level int64) error {
	rows, err := fts3SegdirRowsOf(sim.segdir)
	if err != nil {
		return err
	}
	var atLevel []fts3SegdirRow
	for _, r := range rows {
		if r.level == level {
			atLevel = append(atLevel, r)
		}
	}
	sort.Slice(atLevel, func(i, j int) bool { return atLevel[i].idx < atLevel[j].idx })
	for i, r := range atLevel {
		if r.idx != int64(i) {
			fts3SegdirRowSetIdx(sim.segdir, r.rowid, int64(i))
		}
	}
	return nil
}

// fts3SegdirRowSetIdx rewrites just the idx column (1) of a %_segdir row,
// through a COPY of its Value slice: fts3CloneRowsOnly's clone shares each
// row's backing slice with the real table until commit, so mutating one in
// place here would corrupt the real table even along a path that ultimately
// declines and never commits.
func fts3SegdirRowSetIdx(t *tableMeta, rowid uint64, idx int64) {
	old := t.rows.row(rowid)
	row := make([]Value, len(old))
	copy(row, old)
	row[1] = Value{Typ: Int, I: idx}
	t.putRow(rowid, row)
}

// fts3IncrmergeHintPresent reports a non-empty %_stat resume hint. C fts3 loads the hint (FTS_STAT_INCRMERGEHINT, %_stat id=1)
// unconditionally at the top of sqlite3Fts3Incrmerge and would resume from it
// -- machinery this port does not have, so a non-empty hint always declines.
func (db *DB) fts3IncrmergeHintPresent(name string, s *fts3Shadows) (bool, error) {
	stat := s.stat
	if stat == nil {
		// An fts3 table's %_stat is created on demand, so it is not among the
		// shadows fts3CommandShadows resolves; a previous merge= or
		// automerge= may still have left one behind.
		stat = db.findTableMeta(name + "_stat")
	}
	if stat == nil {
		return false, nil
	}
	row, ok := stat.rows.get(1)
	if !ok || len(row) < 2 {
		return false, nil
	}
	return fts3StatHintNonEmpty(row[1]), nil
}

// fts3IncrmergeSelectLevel is SQL_FIND_MERGE_LEVEL: "SELECT level, count(*)
// FROM %_segdir GROUP BY level HAVING cnt>=? ORDER BY (level % 1024) ASC,
// count(*) DESC LIMIT 1", asked fresh at each cascade iteration over the rows
// as they stand. found is false when nothing qualifies (the loop's stop).
// ambiguous is true when two levels tie on both ORDER BY keys; the caller
// must then decline.
func fts3IncrmergeSelectLevel(rows []fts3SegdirRow, need int) (level int64, levelRows []fts3SegdirRow, found, ambiguous bool) {
	counts := map[int64]int{}
	for _, r := range rows {
		counts[r.level]++
	}
	bestLevel, bestCnt := int64(0), 0
	haveBest := false
	for lvl, cnt := range counts {
		if cnt < need || lvl < 0 {
			// A negative level is never one this engine itself writes
			// (fts3PromoteSegments' level=-1 is a transient scratch value,
			// never left behind); refuse to reason about SQL's % on it
			// rather than guess at a match with C's truncating semantics.
			continue
		}
		rel := lvl % fts3SegdirMaxLevel
		if !haveBest {
			bestLevel, bestCnt, haveBest = lvl, cnt, true
			continue
		}
		bestRel := bestLevel % fts3SegdirMaxLevel
		switch {
		case rel < bestRel, rel == bestRel && cnt > bestCnt:
			bestLevel, bestCnt = lvl, cnt
			ambiguous = false
		case rel == bestRel && cnt == bestCnt && lvl != bestLevel:
			ambiguous = true
		}
	}
	if !haveBest {
		return 0, nil, false, false
	}
	for _, r := range rows {
		if r.level == bestLevel {
			levelRows = append(levelRows, r)
		}
	}
	return bestLevel, levelRows, true, ambiguous
}

// fts3IncrmergeNextIdx is fts3IncrmergeOutputIdx: one past the largest idx
// already at level, or 0 if none. Unlike fts3AllocateSegdirIdx (used by the
// ORDINARY 16-segment flush cascade, fts3_merge.go), this never triggers a
// merge of its own -- C fts3's incremental output idx just keeps climbing.
func fts3IncrmergeNextIdx(rows []fts3SegdirRow, level int64) int64 {
	next := int64(0)
	for _, r := range rows {
		if r.level == level && r.idx+1 > next {
			next = r.idx + 1
		}
	}
	return next
}

// fts3IncrmergeIsMaxLevel is fts3SegmentIsMaxLevel(newLevel): true iff no
// %_segdir row exists at a level in [newLevel+1, ((newLevel/1024)+1)*1024]
// -- note the UPPER bound has no "-1", unlike the promotion guard below
// (fts3PromoteSegments' own iLast); transcribed verbatim from the C rather
// than "corrected", since the two really do use different bounds and this
// port's job is to reproduce the C, not to guess why.
func fts3IncrmergeIsMaxLevel(rows []fts3SegdirRow, newLevel int64) bool {
	hi := ((newLevel / fts3SegdirMaxLevel) + 1) * fts3SegdirMaxLevel
	for _, r := range rows {
		if r.level >= newLevel+1 && r.level <= hi {
			return false
		}
	}
	return true
}

// fts3IncrmergePromotionNeeded reports whether C fts3's fts3PromoteSegments
// would have ANYTHING to fold onto outLevel after this merge writes there:
// any existing %_segdir row at a level in (outLevel, iLast], where iLast is
// the top of outLevel's own 1024-level index block. When true, blocker is one
// such level, for the error message.
//
// This is deliberately a yes/no guard, not a port of fts3PromoteSegments
// itself: promotion only has work when a PRIOR incremental merge already left
// small segments above where this one is about to write, which cannot happen
// on a table's first merge= call and is declined here rather than guessed at
// on a later one (see file comment).
func fts3IncrmergePromotionNeeded(rows []fts3SegdirRow, outLevel int64) (bool, int64) {
	iLast := ((outLevel/fts3SegdirMaxLevel)+1)*fts3SegdirMaxLevel - 1
	for _, r := range rows {
		if r.level > outLevel && r.level <= iLast {
			return true, r.level
		}
	}
	return false, 0
}

// fts3IncrmergeSim is the cascade's working state: CLONES of %_segdir's and
// %_segments' row maps, mutated exactly like the real tables would be but
// never touching them until commit. See file comment for why.
type fts3IncrmergeSim struct {
	segdir, segments *tableMeta
}

func newFts3IncrmergeSim(segdir, segments *tableMeta) *fts3IncrmergeSim {
	return &fts3IncrmergeSim{segdir: fts3CloneRowsOnly(segdir), segments: fts3CloneRowsOnly(segments)}
}

// fts3CloneRowsOnly copies just enough of a tableMeta for fts3SegdirRowsOf /
// fts3ReadSegmentInto / dropRow / putRow / nextRowidForTable / fts3NextBlockID
// to operate on it as a standalone row store -- a shallow copy of the row
// map, sharing each row's Value slice (never mutated in place anywhere in
// this codebase) rather than deep-copying it. nil in, nil out, matching a
// table with no %_segments shadow (a "content=" table still has one; only a
// wholly absent table -- which fts3IncrmergeRun already refuses -- is nil).
func fts3CloneRowsOnly(t *tableMeta) *tableMeta {
	if t == nil {
		return nil
	}
	rows := make(map[uint64][]Value, t.rows.len())
	for rowid, vals := range t.rows.all() {
		rows[rowid] = vals
	}
	return &tableMeta{rows: newRowStore(rows, nil)}
}

// reserveAppendableRange is fts3IncrmergeWriter's reservation for an output
// that fits one leaf: iStart is SQL_NEXT_SEGMENTS_ID (one past the current max
// blockid, before this iteration's chomp) and iEnd the top of the 16*nLeafEst
// range. The NULL marker at iEnd is written immediately, as C's "so nobody
// tries to steal the space just allocated". e.g. merging two single-term
// level-0 segments into an empty %_segments: nLeafEst 4, iStart 1, iEnd 64.
func (sim *fts3IncrmergeSim) reserveAppendableRange(nLeafEst int64) (iStart, iEnd int64) {
	iStart = fts3NextBlockID(sim.segments)
	iEnd = iStart - 1 + nLeafEst*16
	sim.segments.putRow(uint64(iEnd), []Value{{Typ: Null}, {Typ: Null}})
	return iStart, iEnd
}

// writeLeaf stores the merge's one actual leaf node at blockid iStart.
func (sim *fts3IncrmergeSim) writeLeaf(iStart int64, leaf []byte) {
	sim.segments.putRow(uint64(iStart), []Value{{Typ: Null}, {Typ: Blob, S: leaf}})
}

// writeSegdirRow is fts3IncrmergeRelease's single-node special case: real
// fts3 cannot store a segment with start_block!=0 entirely as its root, so
// the root becomes a SYNTHETIC one-child interior node ([0x01][varint
// iStart]) pointing at the one leaf writeLeaf already stored. Called AFTER
// the inputs are chomped (fts3IncrmergeRelease runs after fts3IncrmergeChomp
// in the C), so its rowid is the one the now-shrunk %_segdir hands out next
// -- the same ordering fts3OptimizeIndex already established elsewhere in
// this engine. Verified live against the oracle: X'0101' for iStart=1, and
// X'018101' (an ordinary multi-byte FTS3 varint) once iStart no longer fits
// one byte.
func (sim *fts3IncrmergeSim) writeSegdirRow(level, idx, iStart, iEnd, nLeafData int64) error {
	root := append([]byte{0x01}, fts3PutVarint(nil, uint64(iStart))...)
	rowid, err := nextRowidForTable(sim.segdir)
	if err != nil {
		return err
	}
	sim.segdir.putRow(rowid, []Value{
		{Typ: Int, I: level},
		{Typ: Int, I: idx},
		{Typ: Int, I: iStart},
		{Typ: Int, I: iStart}, // leaves_end_block: the one leaf IS the last one
		{Typ: Text, S: []byte(fmt.Sprintf("%d %d", iEnd, nLeafData))},
		{Typ: Blob, S: root},
	})
	return nil
}

// commit replaces the real shadow tables' row stores with the simulation's
// final ones -- the ONLY point in this whole file that touches segdir/segments
// for real, reached only once every cascade step has been confirmed in scope.
func (sim *fts3IncrmergeSim) commit(segdir, segments *tableMeta) {
	// Replaced wholesale, not mutated through putRow/dropRow, so this goes
	// through replaceRows (schema_write.go) for the cache invalidation AND the
	// rowsMutatedThisSession signal -- see its doc comment for what omitting the
	// latter cost fts3_automerge.go's identical commit. Latent rather than live
	// here: the only caller passing the REAL tables is fts3_command.go's
	// merge=X,Y, and every merge shape probed declines before reaching this
	// function (verified -- a panic planted here is not hit by merge=16,2,
	// merge=100,2, optimize, or an automerge-enabled 30-row load). Correct
	// regardless: reaching this function at all means the rows changed.
	segdir.replaceRows(sim.segdir.rows)
	segments.replaceRows(sim.segments.rows)
}
