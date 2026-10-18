package engine

import (
	"maps"
	"slices"
)

// AN APPEND LOAD INTO A TABLE THAT HAS ROWS. A transaction's load
// (insert_bulk_direct.go) arms only on an empty table, because it builds the
// table's segments and the commit rewrites the file from them. A table that
// already has rows took the ordinary path: each row into the row store's map,
// a change-log entry, an undo entry, then at COMMIT a delta record built from
// the log, encoded, and a second copy put into the committed read view.
//
// Rows that arrive past every rowid the table has ever held, in ascending
// order, cannot conflict with a stored row, a deleted one or an earlier delta
// record, and nothing reads them while the load is armed (every other
// statement, read pager and snapshot drains it first, as for the empty-table
// load). So the load streams them into a segment (segStream: each column kept
// with no pointers in it), and the commit writes that segment as one delta
// record (segDeltaSeg) -- no map entry, no log entry, no per-row undo, no
// per-row record.

// appendArmAfter is how many qualifying INSERT statements a transaction runs
// into a table before an append load takes the rest (DB.appendSeen). A
// variable so a test can arm on the first.
var appendArmAfter = 256

// SetAppendArmAfterForTest sets appendArmAfter and returns the old value.
func SetAppendArmAfterForTest(n int) int { old := appendArmAfter; appendArmAfter = n; return old }

// armAppendLoad arms a load for an INSERT into tbl, which has rows: its first
// row must come past every rowid tbl has held (bulkStore checks every row
// against last). nil when the table cannot take one.
func (db *DB) armAppendLoad(tbl *tableMeta) *segBulkLoad {
	floor, ok := db.rowidFloorForAppend(tbl)
	if !ok {
		return nil
	}
	bl := &segBulkLoad{inTx: true, db: db, stream: newSegStream(len(tbl.cols)), last: floor, any: true}
	tbl.bulk = bl
	db.txBulks = append(db.txBulks, tbl)
	return bl
}

// rowidFloorForAppend is the largest rowid tbl has held anywhere a reader
// could meet it again: its visible rows, rows this session deleted (gone), its
// committed segments whether or not a delta record killed the row, and the
// delta's puts and kills. A segment of rowids above it overlaps nothing, so it
// needs no precedence over any earlier record. false for a store the load does
// not handle: not segment-backed, or cleared this session (baseGone hides the
// base, and would hide the new segment with it).
func (db *DB) rowidFloorForAppend(tbl *tableMeta) (uint64, bool) {
	s := tbl.rows
	if s == nil || s.seg == nil || s.baseGone || s.spill != nil {
		return 0, false
	}
	floor, any := s.maxRowid()
	note := func(r uint64) {
		if !any || rowidLess(floor, r) {
			floor, any = r, true
		}
	}
	for r := range s.gone {
		note(r)
	}
	if src := s.seg.rp.segs; src != nil {
		if segs := src.byRoot[s.seg.root]; len(segs) > 0 {
			if last := segs[len(segs)-1]; last.nRows > 0 {
				note(uint64(last.Rowid(last.nRows - 1)))
			}
		}
		if src.delta != nil {
			if ti, known := src.tableOf[s.seg.root]; known {
				for r := range src.delta.live[ti] {
					note(uint64(r))
				}
				for r := range src.delta.dead[ti] {
					note(uint64(r))
				}
			}
		}
	}
	return floor, any
}

// appendLoadRecords finishes every append load into a segment record for the
// commit's batch, each naming its table's index in the file; nil when there is
// none.
func (n *Session) appendLoadRecords() ([]SegDeltaRecord, []*tableMeta, error) {
	var out []SegDeltaRecord
	var tables []*tableMeta
	var ti map[string]int
	for _, t := range n.DB.txBulks {
		bl := t.bulk
		if bl == nil || bl.stream == nil || bl.stream.rows() == 0 {
			continue
		}
		if ti == nil {
			ti = n.effectiveTableIndex()
		}
		raw, err := bl.stream.finish()
		if err != nil {
			return nil, nil, err
		}
		out = append(out, SegDeltaRecord{Table: ti[t.name], Rowid: int64(bl.stream.rowids.at(0)), Seg: raw})
		tables = append(tables, t)
	}
	return out, tables, nil
}

// publishAppendLoads makes the segments a commit just wrote part of the
// session's two read views -- the row stores' base (DB.segments) and the
// committed columnar view (DB.segColumns) -- as the replay of the file would
// make them: one more segment of each table, after its others. Nothing is
// decoded or copied, and the columnar fast paths and JIT kernels read the new
// rows as they read every other segment's.
//
// Each view gets a new source with its own segment lists; the old source is
// left exactly as it was, for anything still holding it. The row store's
// count, largest rowid and upper bound move by the segment; every cache built
// from the old rows (sorted rowids, equality indexes) is dropped.
func (n *Session) publishAppendLoads(tables []*tableMeta, recs []SegDeltaRecord) error {
	db := n.DB
	segs := make([]*segment, len(tables))
	for i := range tables {
		sg, err := openSegment(recs[i].Seg)
		if err != nil {
			return err
		}
		segs[i] = sg
	}
	if rp := db.segments; rp != nil && rp.segs != nil {
		rp.segs = withAppendedSegments(rp.segs, tables, segs, func(t *tableMeta) (uint32, bool) {
			if t.rows == nil || t.rows.seg == nil || t.rows.seg.rp != rp {
				return 0, false
			}
			return t.rows.seg.root, true
		})
	}
	if cols := db.segColumns; cols != nil && !db.segColumnsStale && cols.segs != nil {
		if cols.segs.delta != nil {
			db.flushColsPending(cols.segs.delta) // in commit order, before the new segments
		}
		ti := n.effectiveTableIndex()
		cols.segs = withAppendedSegments(cols.segs, tables, segs, func(t *tableMeta) (uint32, bool) {
			want, ok := ti[t.name]
			if !ok {
				return 0, false
			}
			for root, idx := range cols.segs.tableOf {
				if idx == want {
					return root, true
				}
			}
			return 0, false
		})
		db.segSrcCache = nil
		db.schemaCorruptGen++
	}
	for i, t := range tables {
		s := t.rows
		last := uint64(segs[i].Rowid(segs[i].nRows - 1))
		if s.nKnown {
			s.n += segs[i].nRows
		}
		s.max, s.maxKnown = last, true
		if s.ubKnown && rowidLess(s.ub, last) {
			s.ub = last
		}
		s.sorted, s.sortedValid = nil, false
		s.rangeKnown = false
		s.invalidateEqIndexes()
		t.bulk = nil
	}
	db.txBulks = slices.DeleteFunc(db.txBulks, func(t *tableMeta) bool { return t.bulk == nil })
	return nil
}

// withAppendedSegments is a copy of src whose tables gain segs, each after the
// table's own; rootOf finds a table's root in src.
func withAppendedSegments(src *segSource, tables []*tableMeta, segs []*segment, rootOf func(*tableMeta) (uint32, bool)) *segSource {
	cp := *src
	cp.byRoot = maps.Clone(src.byRoot)
	for i, t := range tables {
		root, ok := rootOf(t)
		if !ok {
			continue
		}
		old := cp.byRoot[root]
		cp.byRoot[root] = append(old[:len(old):len(old)], segs[i])
	}
	return &cp
}

// hasAppendLoad reports whether tbl's rows are an append load the commit
// writes as a segment, not from the change log.
func (t *tableMeta) hasAppendLoad() bool { return t.bulk != nil && t.bulk.stream != nil }

// eachRow yields the stream's rows in order until fn returns false, each a
// []Value of its own whose payloads alias the stream.
func (s *segStream) eachRow(fn func(rowid uint64, vals []Value) bool) {
	for i := 0; i < s.rowids.n; i++ {
		rid := s.rowids.at(i)
		row := make([]Value, s.nCols)
		for c := range s.cols {
			row[c] = s.cols[c].value(i)
		}
		if !fn(rid, row) {
			return
		}
	}
}

// memBytes is what the stream holds, for the transaction's memory bound.
func (s *segStream) memBytes() int {
	n := s.rowids.capBytes(8)
	for c := range s.cols {
		n += s.cols[c].typ.capBytes(1) + s.cols[c].cell.capBytes(8) + cap(s.cols[c].heap)
	}
	return n
}

// settleAppendLoads puts every append load's rows into its table's row store
// as rows the file holds and drops the loads: after a commit wrote their
// segments, or before a rewrite, which writes every table from its row store
// (written is then left set, so the rewrite takes the table).
func (db *DB) settleAppendLoads(written bool) {
	kept := db.txBulks[:0]
	for _, t := range db.txBulks {
		bl := t.bulk
		if bl == nil || bl.stream == nil {
			kept = append(kept, t)
			continue
		}
		was := t.rowsWrittenSinceCommit
		t.rows.reserve(bl.stream.rows())
		bl.stream.eachRow(func(rid uint64, vals []Value) bool {
			t.putRow(rid, vals)
			return true
		})
		t.rowsWrittenSinceCommit = was || written
		t.bulk = nil
	}
	db.txBulks = kept
}
