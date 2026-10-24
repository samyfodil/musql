package engine

import (
	"encoding/binary"
	"os"
	"slices"

	"github.com/samyfodil/musql/internal/fsio"
)

// A SESSION CATCHING UP pays for what changed, not for what the table holds.
//
// Another connection's commit makes a session reopen from the file
// (Session.reopen). That re-read the pair whole -- the freshness check
// replayed the delta to learn the state it ends at, the reopen replayed it
// again to rebuild its rows -- and the indexes the session's stores had built,
// the UNIQUE conflict probes (row_store_uniqindex.go) and the equality posting
// lists (row_store_eqindex.go), went with the old stores, so the next write
// rebuilt them from every row of the table. Two connections taking turns on
// one file paid all of it on every turn: a replicated node's first local write
// after each apply cost 12 ms at 3,000 rows and 211 ms at 17,000.
//
// What a turn changed is the batches the delta grew by. So the freshness
// check reads the word past the session's own view of the delta (deltaMoved),
// the reopen replays only the batches past its last replay into the state it
// already has (Session.caughtUp), and the indexes move into the reopened
// stores, moved for exactly the rows those batches wrote: out under the row
// the session last saw, in under the row the file now holds (carryOut,
// carryIn). So does the committed column view (carryColumns). Each step first
// checks that the bytes before where it starts reading are the ones it read
// before -- the same segment file, and the delta's running checksum where it
// left off -- and anything else reads the pair whole, as before.

// catchUpMaxBytes is the most delta a session's catch-ups read onto the heap,
// between two reads of the pair whole: past it, the reopen maps the pair as an
// open does, and the heap the earlier catch-ups held goes with the old state.
const catchUpMaxBytes = 32 << 20

// deltaBatch is one complete batch of a delta (deltaBatchesFrom).
type deltaBatch struct {
	body             []byte // its records
	endCtr, endPages uint32
	cksum            uint64
	end              int64 // the byte after its trailer
}

// deltaBatchesFrom reads the complete batches of the delta open as f, whose
// version is version, from byte from on, onto the heap: as replaySegDelta walks
// them, up to the first one truncated, of length zero, or failing its
// checksum. The running checksum at from must be cksum -- the 8 bytes before
// from hold it, whether they end the header or a batch -- or ok is false: the
// bytes before from are not the ones that checksum was taken over. ok is false
// too past limit bytes of batches.
func deltaBatchesFrom(f *os.File, version uint32, from int64, cksum uint64, limit int64) (batches []deltaBatch, ok bool) {
	if version < 1 || version > segDeltaVersion || from < segDeltaHdrSize {
		return nil, false
	}
	var word [8]byte
	if k, _ := fsio.ReadAt(f, word[:], from-8); k < len(word) || binary.LittleEndian.Uint64(word[:]) != cksum {
		return nil, false
	}
	w := segDeltaLenWidth(version)
	read := int64(0)
	for off := from; ; {
		if k, _ := fsio.ReadAt(f, word[:w], off); k < int(w) {
			break // the log ends here
		}
		var n int64
		if w == 4 {
			n = int64(binary.LittleEndian.Uint32(word[:]))
		} else {
			n = int64(binary.LittleEndian.Uint64(word[:]))
		}
		end := off + w + n + segDeltaTrailerSz
		if n <= 0 || end < off {
			break // zero ends the log, as in replay; the rest is not a batch
		}
		if read += end - off; read > limit {
			return nil, false
		}
		batch := make([]byte, end-off)
		if k, _ := fsio.ReadAt(f, batch, off); int64(k) < end-off {
			break // truncated
		}
		want := binary.LittleEndian.Uint64(batch[len(batch)-8:])
		if segDeltaChecksum(version, batch[:len(batch)-8], cksum) != want {
			break // torn or corrupt: not history
		}
		batches = append(batches, deltaBatch{
			body:     batch[w : w+n],
			endCtr:   binary.LittleEndian.Uint32(batch[w+n:]),
			endPages: binary.LittleEndian.Uint32(batch[w+n+4:]),
			cksum:    want,
			end:      end,
		})
		cksum, off = want, end
	}
	return batches, true
}

// deltaMoved answers RefreshIfStale's question -- has a batch landed past this
// session's view? -- from the delta's bytes around where the view ends: the
// view is current while the checksum the session's own append or replay left
// there is still there, and the length word after it is zero, as an append
// leaves it and every later one overwrites (appendSegmentDeltaAt's own check).
// ok is false when the segment file is not the one the session last stamped
// or the checksum is not there, and the caller replays the pair. Under the
// caller's read lock.
func (n *Session) deltaMoved(now [2]fileStamp) (moved, ok bool) {
	if now[0] != n.seenStat[0] || !n.appendAt.known || n.appendAt.offset != n.deltaEnd {
		return false, false
	}
	f, err := os.Open(segDeltaPath(n.segPath))
	if err != nil {
		return false, false
	}
	defer f.Close()
	var b [16]byte
	k, _ := fsio.ReadAt(f, b[:], n.deltaEnd-8)
	if k < 8 || binary.LittleEndian.Uint64(b[:8]) != n.appendAt.cksum {
		return false, false
	}
	w := int(segDeltaLenWidth(n.appendAt.version))
	if k < 8+w {
		return false, true // the delta ends where the view does
	}
	if w == 4 {
		return binary.LittleEndian.Uint32(b[8:]) != 0, true
	}
	return binary.LittleEndian.Uint64(b[8:]) != 0, true
}

// caughtUp is the pair at segPath read from where this session's own read of
// it ended: the segment file, a new handle on the one the session reads, and
// the session's replayed delta with every batch appended since folded in. ok
// is false -- and nothing of the session's is changed -- unless the segment
// file is the one the session read and the delta still holds the checksum the
// session's replay ended on, where it ended.
//
// The state is the session's own, advanced in place: from the moment caughtUp
// returns, the session's pager reads the new state, and the caller must take
// it over (ReadOnlyPager.detachDelta) or rebuild the session from the file.
// before is called first, while the session still reads the state it had, with
// the batches read; it is how the indexes learn the rows the batches replace.
// Under the caller's read lock.
func (n *Session) caughtUp(segPath string, before func([]deltaBatch)) (f *SegmentFile, st *segDeltaState, ok bool) {
	if n.src == nil || n.src.segs == nil || n.src.segs.file == nil || n.src.segs.file.shared == nil {
		return nil, nil, false
	}
	st = deltaStateOf(n.src)
	if st == nil || st.version == 0 || st.countersOnly {
		return nil, nil, false
	}
	f, err := OpenSegmentFile(segPath)
	if err != nil {
		return nil, nil, false
	}
	if f.shared == nil || f.shared.key != n.src.segs.file.shared.key ||
		f.srcChangeCounter != n.baseCtr || f.srcPageCount != n.basePages {
		f.Close()
		return nil, nil, false
	}
	df, err := os.Open(segDeltaPath(segPath))
	if err != nil {
		f.Close()
		return nil, nil, false
	}
	batches, ok := deltaBatchesFrom(df, st.version, st.bytes, st.cksum, catchUpMaxBytes-st.heap)
	df.Close()
	if !ok {
		f.Close()
		return nil, nil, false
	}
	// The batches replay into an overlay of their own first, every row a patch
	// lands on read through the state as it stands -- so a batch that does not
	// replay leaves that state as it was.
	nd := &segDeltaState{
		live: map[int]map[int64][]Value{}, dead: map[int]map[int64]bool{}, version: st.version,
		base: func(ti int, rowid int64) ([]Value, bool) {
			if vals, have := st.live[ti][rowid]; have {
				return vals, true
			}
			if st.dead[ti][rowid] {
				return nil, false
			}
			return st.committedRow(ti, rowid)
		},
	}
	for _, b := range batches {
		if err := nd.applyBatch(b.body); err != nil {
			f.Close()
			return nil, nil, false
		}
	}
	if before != nil {
		before(batches)
	}
	// A rowid is in an overlay's live rows or its dead ones, never both, so the
	// order of the two folds does not matter. The batches' segments come after
	// every earlier one, as their rowids do.
	for ti, rows := range nd.live {
		for rowid, vals := range rows {
			st.put(ti, rowid, vals)
		}
	}
	for ti, rows := range nd.dead {
		for rowid := range rows {
			st.kill(ti, rowid)
		}
	}
	for ti, segs := range nd.segs {
		if st.segs == nil {
			st.segs = map[int][]*segment{}
		}
		st.segs[ti] = append(st.segs[ti], segs...)
	}
	if len(batches) > 0 {
		last := batches[len(batches)-1]
		st.heap += last.end - st.bytes
		st.endCtr, st.endPages, st.cksum, st.bytes = last.endCtr, last.endPages, last.cksum, last.end
		st.batches += len(batches)
	}
	st.base = f.committedRows()
	st.posMu.Lock()
	st.posCache = nil
	st.posMu.Unlock()
	return f, st, true
}

// deltaStateOf is the replayed delta p was built over, nil when it was not
// built over a segment file's: the state the pager releases on Close, which is
// there even when it holds no row (ReadOnlyPager.segs.delta is nil then).
func deltaStateOf(p *ReadOnlyPager) *segDeltaState {
	if p == nil {
		return nil
	}
	c, ok := p.closer.(segFileCloser)
	if !ok {
		return nil
	}
	return c.st
}

// detachDelta stops p's Close releasing its delta state, which a pager built
// by a session's catch-up has taken over (Session.caughtUp).
func (p *ReadOnlyPager) detachDelta() {
	if c, ok := p.closer.(segFileCloser); ok {
		c.st = nil
		p.closer = c
	}
}

// deltaTouched reads what the complete batches of a delta between byte from
// and byte to wrote: per table index, every rowid a put, patch or kill names,
// and every table a segment record extends. data is the delta from its first
// byte. The running checksum must be cksum at from and end at to, which says
// the batches between are the history that leads from the delta a session saw
// to the one it reopened at. false when it is not, or a batch does not read
// back the way replay reads it.
func deltaTouched(data []byte, version uint32, from, to int64, cksum, end uint64) (rows map[int][]int64, segs map[int]bool, ok bool) {
	if version < 1 || from < segDeltaHdrSize || from > to || to > int64(len(data)) {
		return nil, nil, false
	}
	if binary.LittleEndian.Uint64(data[from-8:]) != cksum {
		return nil, nil, false
	}
	rows, segs = map[int][]int64{}, map[int]bool{}
	w := segDeltaLenWidth(version)
	for off := from; off < to; {
		if off+w > to {
			return nil, nil, false
		}
		var n int64
		if w == 4 {
			n = int64(binary.LittleEndian.Uint32(data[off:]))
		} else {
			n = int64(binary.LittleEndian.Uint64(data[off:]))
		}
		next := off + w + n + segDeltaTrailerSz
		if n <= 0 || next < off || next > to {
			return nil, nil, false
		}
		batch := data[off:next]
		want := binary.LittleEndian.Uint64(batch[len(batch)-8:])
		if segDeltaChecksum(version, batch[:len(batch)-8], cksum) != want {
			return nil, nil, false
		}
		if !batchTouched(batch[w:w+n], rows, segs) {
			return nil, nil, false
		}
		cksum, off = want, next
	}
	return rows, segs, cksum == end
}

// batchTouched adds what one batch body's records name to rows and segs, as
// applyBatch reads them; false when a record does not parse.
func batchTouched(body []byte, rows map[int][]int64, segs map[int]bool) bool {
	for off := 0; off < len(body); {
		if off+13 > len(body) {
			return false
		}
		kind := body[off]
		table := int(binary.LittleEndian.Uint32(body[off+1:]))
		rowid := int64(binary.LittleEndian.Uint64(body[off+5:]))
		off += 13
		switch kind {
		case segDeltaSeg:
			if off+5 > len(body) {
				return false
			}
			n := int(binary.LittleEndian.Uint32(body[off:]))
			pad := int(body[off+4])
			off += 5 + pad
			if pad > 7 || n <= 0 || off+n > len(body) {
				return false
			}
			segs[table] = true
			off += n
		case segDeltaKill:
			rows[table] = append(rows[table], rowid)
		case segDeltaPut, segDeltaPatch:
			if off+4 > len(body) {
				return false
			}
			n := int(binary.LittleEndian.Uint32(body[off:]))
			off += 4
			if n < 0 || off+n > len(body) {
				return false
			}
			rows[table] = append(rows[table], rowid)
			off += n
		default:
			return false
		}
	}
	return true
}

// carriedTable is one table's indexes on their way from a session's old stores
// to its reopened ones (carryOut, carryIn): the rowids the batches since the
// session's view wrote, and those rows as it saw them.
type carriedTable struct {
	t    *tableMeta
	ti   int
	uq   map[*indexMeta]*rsUniqIndex
	eq   map[int]*rsEqIndex
	rids []int64
	was  [][]Value
}

// caughtUpTouched is the batches past this session's view of the delta, and
// what they wrote, from the batches its catch-up read past its replay
// (caughtUp, which calls this before it folds them in): the view ends at one
// of them, or where the replay did, with the checksum the view's own append or
// replay left. false when it ends nowhere among them like that.
func (n *Session) caughtUpTouched(batches []deltaBatch) (tail []deltaBatch, rows map[int][]int64, segs map[int]bool, ok bool) {
	st := deltaStateOf(n.src)
	if st == nil || !n.appendAt.known || n.appendAt.offset != n.deltaEnd {
		return nil, nil, nil, false
	}
	k := 0
	if n.deltaEnd != st.bytes || n.appendAt.cksum != st.cksum {
		k = slices.IndexFunc(batches, func(b deltaBatch) bool { return b.end == n.deltaEnd })
		if k < 0 || batches[k].cksum != n.appendAt.cksum {
			return nil, nil, nil, false
		}
		k++
	}
	rows, segs = map[int][]int64{}, map[int]bool{}
	for _, b := range batches[k:] {
		if !batchTouched(b.body, rows, segs) {
			return nil, nil, nil, false
		}
	}
	return batches[k:], rows, segs, true
}

// carryColumns moves this session's committed column view (DB.segColumns)
// into fresh, the session caughtUp reopened, moved past tail -- the batches
// after this session's view of the delta -- the way each commit of its own
// moves it (extendCommittedColumns), where fresh's first read would otherwise
// read the pair whole to build its own. A view marked stale, over another
// file, or a tail with a segment record, leaves fresh to build its own.
func (n *Session) carryColumns(fresh *Session, tail []deltaBatch) {
	db := n.DB
	cols := db.segColumns
	if cols == nil || db.segColumnsStale || cols.segs == nil || cols.segs.file == nil || cols.segs.file.shared == nil ||
		fresh.src.segs == nil || fresh.src.segs.file == nil || fresh.src.segs.file.shared == nil ||
		cols.segs.file.shared.key != fresh.src.segs.file.shared.key {
		return
	}
	segs := map[int]bool{}
	for _, b := range tail {
		if !batchTouched(b.body, map[int][]int64{}, segs) || len(segs) > 0 {
			return
		}
	}
	st := cols.segs.delta
	if st == nil {
		st = &segDeltaState{live: map[int]map[int64][]Value{}, dead: map[int]map[int64]bool{}, version: fresh.appendAt.version}
	}
	if st.base == nil {
		// A view its own commits started has none: they apply whole rows. A patch
		// in the tail lands on the file's row.
		st.base = cols.segs.file.committedRows()
	}
	db.flushColsPending(st)
	for _, b := range tail {
		if st.applyBatch(b.body) != nil {
			return
		}
	}
	st.endCtr, st.endPages = fresh.endCtr, fresh.endPages
	st.batches += len(tail)
	st.posMu.Lock()
	st.posCache = nil
	st.posMu.Unlock()
	cols.segs.delta = st
	fresh.DB.segColumns, fresh.DB.segColumnsStale = cols, false
	db.segColumns = nil
}

// reopenedTouched is what the batches between this session's view of the delta
// and fresh's wrote, fresh being the session read whole from the same segment
// file: read from fresh's mapping of the delta, between the checksum the view's
// own append or replay left and the one fresh's replay ended on.
func (n *Session) reopenedTouched(fresh *Session) (rows map[int][]int64, segs map[int]bool, ok bool) {
	if fresh.baseCtr != n.baseCtr || fresh.basePages != n.basePages ||
		!n.appendAt.known || n.appendAt.offset != n.deltaEnd || !fresh.appendAt.known ||
		n.appendAt.version != fresh.appendAt.version {
		return nil, nil, false
	}
	if fresh.deltaEnd == n.deltaEnd && fresh.appendAt.cksum == n.appendAt.cksum {
		return map[int][]int64{}, map[int]bool{}, true
	}
	st := deltaStateOf(fresh.src)
	if st == nil || st.mapping == nil {
		return nil, nil, false
	}
	return deltaTouched(st.mapping.Data(), fresh.appendAt.version, n.deltaEnd, fresh.deltaEnd, n.appendAt.cksum, fresh.appendAt.cksum)
}

// carryOut takes the indexes this session's stores built, with the rows the
// batches since its view wrote as the session saw them, for carryIn. Run while
// the stores still read that view, with nothing written in them that the file
// does not hold (RefreshIfStale reopens only then), so what they hold is the
// delta's state where this session's view of it ends.
func (n *Session) carryOut(rows map[int][]int64, segs map[int]bool) []carriedTable {
	var out []carriedTable
	for _, t := range n.DB.tables {
		s := t.rows
		// A table written since its last commit holds rows no change log may
		// describe -- a virtual table's shadow rows, a load's -- which no batch
		// would move out of its indexes.
		if s == nil || t.isTemp || t.aliasOf != "" || t.rowsWrittenSinceCommit || t.bulk != nil ||
			len(s.uqIdx) == 0 && len(s.eqIdx) == 0 {
			continue
		}
		ti, inFile := n.tableIndex[t.name]
		if !inFile || segs[ti] {
			continue
		}
		c := carriedTable{t: t, ti: ti, uq: s.uqIdx, eq: map[int]*rsEqIndex{}}
		for col, ix := range s.eqIdx {
			if ix != nil {
				c.eq[col] = ix
			}
		}
		c.rids = slices.Clone(rows[ti])
		slices.Sort(c.rids)
		c.rids = slices.Compact(c.rids)
		c.was = make([][]Value, len(c.rids))
		for i, r := range c.rids {
			c.was[i] = s.row(uint64(r))
		}
		s.uqIdx, s.eqIdx = nil, nil
		out = append(out, c)
	}
	return out
}

// carryIn puts carried indexes into n, a session reopened from the same
// segment file, moving each for the rows carryOut took: out under the row as
// the old session saw it, in under the row n reads now.
func (n *Session) carryIn(carried []carriedTable) {
	for _, c := range carried {
		ft := n.DB.tableNamed(c.t.name)
		if ti, inFile := n.tableIndex[c.t.name]; ft == nil || !inFile || ti != c.ti ||
			ft.rootPage != c.t.rootPage || ft.ipkIndex != c.t.ipkIndex || len(ft.cols) != len(c.t.cols) {
			continue
		}
		uq := map[*indexMeta]*rsUniqIndex{}
		for idx, ix := range c.uq {
			if fi := n.DB.uniqueIndexLike(idx, ft); fi != nil {
				uq[fi] = ix
			}
		}
		if len(uq) == 0 && len(c.eq) == 0 {
			continue
		}
		if err := n.DB.ensureTableLoaded(ft); err != nil || ft.rows == nil {
			continue
		}
		fs := ft.rows
		fs.uqIdx, fs.eqIdx = uq, c.eq
		for i, r := range c.rids {
			now := fs.row(uint64(r))
			fs.noteWrite(uint64(r), c.was[i], now)
			fs.noteUniqWrite(uint64(r), c.was[i], now)
		}
	}
}

// tableNamed is db's main-database table named name, or nil.
func (db *DB) tableNamed(name string) *tableMeta {
	for _, t := range db.tables {
		if !t.isTemp && equalFoldName(t.name, name) {
			return t
		}
	}
	return nil
}

// uniqueIndexLike is db's UNIQUE index on tbl with idx's name and key columns,
// or nil.
func (db *DB) uniqueIndexLike(idx *indexMeta, tbl *tableMeta) *indexMeta {
	for _, fi := range db.indexes {
		if fi.unique && !fi.exprOrPartial && equalFoldName(fi.name, idx.name) && indexBelongsTo(fi, tbl) &&
			slices.Equal(fi.colIdx, idx.colIdx) && slices.Equal(fi.colCollation, idx.colCollation) {
			return fi
		}
	}
	return nil
}
