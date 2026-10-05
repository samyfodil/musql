package engine

import (
	"fmt"
	"slices"
)

// Turning a commit's row changes into delta records -- the writer's side of
// segment_delta.go.
//
// The engine already produces exactly the input this needs: RowChange
// (rowhook.go) is one logical row mutation with its table, its rowid, its kind
// and its new values, recorded per commit when a session asks for it
// (EnableRowChangeCapture). So the delta does not need a second capture
// mechanism bolted onto the write path -- it needs a translation, which is this
// file.
//
// WHAT THE RECORD HOLDS. A segment scan yields the STORED row: the INTEGER
// PRIMARY KEY column is a NULL in it and the rowid carries the value, because
// that is what a b-tree record holds and what every consumer of a scan already
// expects. Under full capture a RowChange's New is
// the row as the statement wrote it, IPK column included, so the translation
// has to put that column back to NULL; under delta-only capture it already is. Getting this wrong is not a slow answer: the merged scan
// would hand back a row whose id column read as its value where the segment's
// own rows read NULL, and the two halves of one table would disagree.

// segDeltaRecordsInto translates one commit's row changes into delta records,
// appending onto dst, so a session can keep one record buffer across commits
// (Session.recBuf) instead of allocating 48 bytes per row written on every one.
//
// A change that logged its row (New) is written as that row, a record per change
// as always. One that did not -- its table's store had spilled, noteRowChange --
// is written as the row's FINAL state, read through its store: a put of the row
// the table holds now, or a kill when it holds none. The two mix safely: replay
// keeps the last write per rowid (segDeltaState.put/kill), and a rowid's last
// entry carries either its latest value or the store's final state, which are
// the same. Reading the store is what lets a spilled store be the only holder of
// its rows. A row touched twice in a row is one record.
//
// The records reference the store's rows and copy nothing, so the caller must
// clear the buffer once the batch is appended, or it pins those rows until the
// next commit.
func segDeltaRecordsInto(dst []SegDeltaRecord, changes []RowChange, tableIndex map[string]int, ipkOf map[string]int, storeOf func(table string) *rowStore) ([]SegDeltaRecord, error) {
	out := slices.Grow(dst[:0], len(changes))
	emit := func(ti int, table string, rowid int64) {
		if k := len(out) - 1; k >= 0 && out[k].Table == ti && out[k].Rowid == rowid {
			return // the same row twice in a row
		}
		st := storeOf(table)
		if st == nil {
			out = append(out, SegDeltaRecord{Table: ti, Rowid: rowid, Kill: true})
			return
		}
		ipk := -1
		if i, have := ipkOf[table]; have {
			ipk = i
		}
		// A row in memory goes in as its values; a spilled one is read while
		// the batch is written (SegDeltaRecord.src), so a commit never holds
		// what the store let go of.
		if vals, ok := st.m[uint64(rowid)]; ok {
			if isSpilled(vals) {
				out = append(out, SegDeltaRecord{Table: ti, Rowid: rowid, src: st, ipk: ipk})
			} else {
				out = append(out, SegDeltaRecord{Table: ti, Rowid: rowid, Vals: storedIPKNull(vals, ipk)})
			}
			return
		}
		if vals, ok := st.get(uint64(rowid)); ok {
			out = append(out, SegDeltaRecord{Table: ti, Rowid: rowid, Vals: storedIPKNull(vals, ipk)})
			return
		}
		out = append(out, SegDeltaRecord{Table: ti, Rowid: rowid, Kill: true})
	}
	for _, ch := range changes {
		if ch.Temp {
			// A TEMP table is session-private and never persisted, so its rows have
			// no place in the delta -- and letting one through is worse than losing
			// it: tableIndex is keyed by NAME, so a temp "t" resolved to main's "t"
			// and its rows were written as main's.
			continue
		}
		ti, ok := tableIndex[ch.Table]
		if !ok {
			continue
		}
		switch ch.Kind {
		case RowDelete:
			out = append(out, SegDeltaRecord{Table: ti, Rowid: ch.Rowid, Kill: true})
		case RowInsert, RowUpdate:
			if ch.moved {
				// The row left its old rowid, which a put under the new one does
				// not say: without this the old row came back on the next open.
				if ch.New != nil {
					out = append(out, SegDeltaRecord{Table: ti, Rowid: ch.movedFrom, Kill: true})
				} else {
					emit(ti, ch.Table, ch.movedFrom)
				}
			}
			if ch.New != nil {
				ipk := -1
				if i, have := ipkOf[ch.Table]; have {
					ipk = i
				}
				out = append(out, SegDeltaRecord{Table: ti, Rowid: ch.Rowid, Vals: storedIPKNull(ch.New, ipk)})
				continue
			}
			emit(ti, ch.Table, ch.Rowid)
		case RowSchema:
		default:
			return nil, fmt.Errorf("engine: segment delta: unknown row change kind %d", ch.Kind)
		}
	}
	if len(out) == 0 {
		// A caller tests len(recs)==0 to mean "nothing for this file"
		// (commitMainLocked); returning nil keeps that test exact.
		return nil, nil
	}
	return out, nil
}

// storeOf is segDeltaRecordsInto's storeOf over this session's tables of one
// catalog. A table no longer there -- dropped in this transaction, which makes
// the commit a rewrite that follows these records -- has no store, and holds no
// rows.
func (db *DB) storeOf(temp bool) func(string) *rowStore {
	scope := createScope(temp)
	memo := map[string]*rowStore{} // one lookup per table, not per row
	return func(table string) *rowStore {
		if st, ok := memo[table]; ok {
			return st
		}
		var st *rowStore
		if tbl := db.findTableMetaIn(scope, table); tbl != nil && tbl.isTemp == temp && db.ensureTableLoaded(tbl) == nil {
			st = tbl.rows
		}
		memo[table] = st
		return st
	}
}

// SegmentFileTableIndex is the name->directory-index map SegDeltaRecordsFor
// needs, together with each table's IPK column, read straight off a segment file.
func SegmentFileTableIndex(f *SegmentFile) (tableIndex map[string]int, ipkOf map[string]int) {
	tableIndex, ipkOf = map[string]int{}, map[string]int{}
	for i, t := range f.Tables() {
		tableIndex[t.Name] = i
		ipkOf[t.Name] = t.IPK
	}
	return tableIndex, ipkOf
}

// SegmentFileState is the database state a segment file plus its delta currently
// describe -- what AttachSegments compares against the live database, exposed so
// a caller appending to the delta can name the state its commit produced.
func SegmentFileState(segPath string) (endCtr, endPages uint32, err error) {
	// Under a SHARED lock, for the reason Open takes one: the file and its
	// delta are two files and a rewrite moves them one at a time.
	lerr := withSegmentReadLock(segPath, func() error {
		var serr error
		endCtr, endPages, serr = segmentFileStateUnlocked(segPath)
		return serr
	})
	return endCtr, endPages, lerr
}

// segmentFileStateUnlocked is SegmentFileState taking no lock, for a caller that
// already holds the segment file's write lock -- see openSegmentsUnlocked for why
// re-locking from inside a locked section deadlocks against itself.
func segmentFileStateUnlocked(segPath string) (endCtr, endPages uint32, err error) {
	f, oerr := OpenSegmentFile(segPath)
	if oerr != nil {
		return 0, 0, oerr
	}
	defer f.Close()
	base, basePages := f.srcChangeCounter, f.srcPageCount
	// MAPPED, and released before return: only the end state is kept.
	m, data, rerr := mapSegDelta(segDeltaPath(segPath))
	if rerr != nil {
		return 0, 0, rerr
	}
	if m != nil {
		defer m.Close()
	}
	st, serr := replaySegDelta(data, base, basePages)
	if serr != nil {
		return 0, 0, serr
	}
	return st.endCtr, st.endPages, nil
}

// SegmentFileBase is the identity a delta must name to be paired with this
// segment file.
func SegmentFileBase(segPath string) (baseCtr, basePages uint32, err error) {
	f, err := OpenSegmentFile(segPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return f.srcChangeCounter, f.srcPageCount, nil
}
