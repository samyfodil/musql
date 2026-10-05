package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// Compaction folds the row-major delta back into columnar segments.
// The delta is the cheap append half of persistence; compaction restores
// the columnar fast paths that the delta disrupts.
//
// ---- crash safety ----
//
// The new segment file is written to a TEMPORARY path in the same directory,
// fsynced, then renamed over the old one; only then is the delta removed. Every
// interruption leaves a readable pair:
//
//   - before the rename: the old segments plus the whole delta, which is the
//     state that was already being read. The temp file is garbage and is
//     overwritten by the next attempt.
//   - after the rename, before the delta is gone: the NEW segments plus a delta
//     whose header names the OLD file's identity, so replaySegDelta rejects the
//     pairing (errDeltaNotForThisFile) rather than applying the log twice.
//
// That last case is the one worth stating plainly, because applying a log twice
// is not a slow answer, it is a wrong one: the log's rows would be re-put over
// segments that already contain them (harmless) and its tombstones re-applied to
// rowids a later insert had reused (not harmless). Naming the base file in the
// delta's header is what makes the pairing checkable instead of assumed.

// CompactSegmentFile folds segPath's delta into its segments and removes the
// delta, leaving a segment file whose own recorded state is the one the pair had.
//
// It reports whether anything was done: a missing or empty delta is not an error
// and not work.
func CompactSegmentFile(segPath string) (bool, error) {
	f, err := OpenSegmentFile(segPath)
	if err != nil {
		return false, err
	}
	baseCtr, basePages := f.srcChangeCounter, f.srcPageCount
	deltaPath := segDeltaPath(segPath)
	// MAPPED, not read: the replay's rows alias these bytes, and a read put the
	// whole log on the heap beside them.
	dm, derr := mmapfile.Open(deltaPath)
	if derr != nil {
		f.Close()
		if os.IsNotExist(derr) {
			return false, nil
		}
		return false, derr
	}
	defer dm.Close()
	st, rerr := replaySegDelta(dm.Data(), baseCtr, basePages)
	if rerr != nil {
		f.Close()
		return false, rerr
	}
	if st.empty() {
		f.Close()
		// A delta that carries no surviving change still moved the pair's
		// recorded state, so it cannot simply be deleted: rewriting the segment
		// file at the new state is what makes removing it safe.
		if st.endCtr == baseCtr && st.endPages == basePages {
			return false, os.Remove(deltaPath)
		}
	}

	w, werr := newSegFileWriter(filepath.Dir(segPath))
	if werr != nil {
		f.Close()
		return false, werr
	}
	defer w.discard()
	tables, berr := mergeSegmentsWithDelta(f, st, w)
	// The CATALOG rides through unchanged. Compaction rewrites the ROWS; it has no
	// business touching the schema, and calling the catalog-less WriteSegmentFile
	// here silently dropped every index, view, trigger and AUTOINCREMENT counter
	// from the file it produced -- caught by TestExportImportIsLossless, which
	// compacts on the way out.
	cat := f.Catalog()
	if berr != nil {
		f.Close()
		return false, berr
	}

	tmp, terr := os.CreateTemp(filepath.Dir(segPath), filepath.Base(segPath)+".compact-*")
	if terr != nil {
		f.Close()
		return false, terr
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // a no-op once the rename below has taken it

	// The compacted file records the state the PAIR was at, which is what makes
	// it a drop-in replacement: a reader that accepted segments+delta accepts
	// this file alone. The old file stays mapped until here: carried segments
	// were written to the scratch straight out of it.
	werr = w.finish(tmpPath, tables, cat, st.endCtr, st.endPages)
	f.Close()
	if werr != nil {
		return false, werr
	}
	if serr := syncSegFile(tmpPath); serr != nil {
		return false, serr
	}
	if rerr := os.Rename(tmpPath, segPath); rerr != nil {
		return false, rerr
	}
	syncDir(segPath)
	// Only now: before the rename the delta is the only record of these rows.
	if rerr := os.Remove(deltaPath); rerr != nil && !os.IsNotExist(rerr) {
		return false, rerr
	}
	syncDir(segPath)
	return true, nil
}

// segsCarried counts segments compaction carried over as their bytes. A test
// needs it because a rebuilt untouched segment is byte-identical to the one it
// replaces -- the encoding is deterministic -- so the file cannot say which
// happened.
var segsCarried atomic.Int64

// SegsCarriedForTest reads and clears segsCarried.
func SegsCarriedForTest() int64 { return segsCarried.Swap(0) }

// mergeSegmentsWithDelta is the compacted content of every table: its segment
// rows with the log's removals dropped and its puts substituted, plus the rows
// the log added, all in rowid order.
//
// Each segment goes to w as soon as it is built, and the returned tables carry
// only their metadata: the compaction holds one segment's rows at a time, not
// the database.
func mergeSegmentsWithDelta(f *SegmentFile, st *segDeltaState, w *segFileWriter) ([]ConvertedTable, error) {
	var out []ConvertedTable
	for ti, t := range f.Tables() {
		segs, serr := t.openSegments()
		if serr != nil {
			return nil, serr
		}
		ct := ConvertedTable{Name: t.Name, SQL: t.SQL, Cols: t.Cols, IPK: t.IPK, Rank: t.Rank, Rowid: t.Rowid, Root: t.Root}
		colInfos := make([]columnInfo, len(ct.Cols))
		for i, n := range ct.Cols {
			colInfos[i].Name = n
		}
		dead := st.dead[ti]
		deadIDs := make([]int64, 0, len(dead))
		for rowid := range dead {
			deadIDs = append(deadIDs, rowid)
		}
		slices.Sort(deadIDs)
		// pending is the run of rows to re-encode, cut into segments as it fills
		// (segCutter) -- so a table is never held, only its next segment -- and
		// flushed whenever a segment is carried over whole, and at the end.
		cut := &segCutter{cols: colInfos, what: t.Name, emit: func(raw []byte) error { return w.addSegment(ti, raw) }}
		var emitErr error
		flush := func() error {
			if err := cut.flush(); err != nil {
				return fmt.Errorf("engine: compaction: %w", err)
			}
			return nil
		}
		emit := func(rowid uint64, vals []Value) {
			if emitErr == nil {
				emitErr = cut.add(rowid, vals)
			}
		}
		dRows := st.rowsFor(ti)
		di := 0
		for k, sg := range segs {
			nCols := len(sg.cols)
			if sg.nRows > 0 {
				lo, hi := int64(sg.Rowid(0)), int64(sg.Rowid(sg.nRows-1))
				for di < len(dRows) && dRows[di].rowid < lo {
					emit(uint64(dRows[di].rowid), dRows[di].vals)
					di++
				}
				// A SEGMENT NO LOG RECORD TOUCHES IS CARRIED OVER AS ITS BYTES.
				// Re-encoding it would produce the same rows, and it was most of
				// what a compaction cost: an append-only load leaves every segment
				// but the last untouched, and rebuilding them all made each
				// compaction O(table): 31% of a sustained load of 20,000-row
				// transactions, whose commits compact every other time, and most
				// of a bulk UPDATE's.
				//
				// Untouched means no put and no removal in [lo, hi]; the segments
				// partition the rowid range in order, so a put outside every
				// segment lands in pending between them. Two more conditions:
				// its column count is the table's (a segment is rebuilt against
				// the current column list), and it is full or nothing follows it
				// -- a short segment with rows after it is merged into them, so
				// appends fill the tail segment instead of leaving one short
				// segment per compaction.
				hitDead := func() bool {
					j, _ := slices.BinarySearch(deadIDs, lo)
					return j < len(deadIDs) && deadIDs[j] <= hi
				}
				touched := (di < len(dRows) && dRows[di].rowid <= hi) || hitDead()
				if !touched && nCols == len(t.Cols) && (segmentFull(sg.nRows, t.segs[k]) || di == len(dRows)) {
					if err := flush(); err != nil {
						return nil, err
					}
					// Written straight from the mapping, which stays open until
					// the new file is finished (CompactSegmentFile).
					if err := w.addSegment(ti, t.segs[k]); err != nil {
						return nil, err
					}
					segsCarried.Add(1)
					continue
				}
			}
			for i := 0; i < sg.nRows; i++ {
				rowid := int64(sg.Rowid(i))
				for di < len(dRows) && dRows[di].rowid < rowid {
					emit(uint64(dRows[di].rowid), dRows[di].vals)
					di++
				}
				if di < len(dRows) && dRows[di].rowid == rowid {
					emit(uint64(rowid), dRows[di].vals)
					di++
					continue
				}
				if dead[rowid] {
					continue
				}
				w := sg.Width(i)
				if w > nCols {
					w = nCols
				}
				row := make([]Value, w)
				for c := 0; c < w; c++ {
					row[c] = sg.Value(c, i)
				}
				emit(uint64(rowid), row)
			}
		}
		for ; di < len(dRows); di++ {
			emit(uint64(dRows[di].rowid), dRows[di].vals)
		}
		if emitErr != nil {
			return nil, emitErr
		}
		if err := flush(); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	// Table order is the input file's, unchanged: WriteSegmentFile writes the
	// slice it is given in order, so a compacted file's table INDEXES are the
	// same as the file it replaces. A delta numbers its records by that index,
	// so reordering here would silently repoint every future record.
	return out, nil
}

// syncSegFile fsyncs a freshly written segment file, so the rename that follows
// publishes bytes that are actually on the device.
func syncSegFile(path string) error {
	// Opened for writing: Windows' FlushFileBuffers refuses a read-only handle.
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if serr := fh.Sync(); serr != nil {
		fh.Close()
		return serr
	}
	return fh.Close()
}

// syncDir fsyncs the directory containing path so a create or rename of its
// entry is itself durable. Best-effort: some filesystems and platforms do not
// permit fsync on a directory handle.
func syncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer d.Close()
	d.Sync() // best-effort, as above
	return nil
}
