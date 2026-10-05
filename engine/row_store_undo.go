package engine

// Transaction undo log per row store.
// BEGIN and SAVEPOINT formerly copied every table's row map (O(database) cost).
// This undo log avoids that.
//	  400,000-row table    6.72ms   24.2MB
//
// C SQLite's BEGIN copies nothing; its rollback journal takes a page when that
// page is first written, and a savepoint is a position in that journal
// (pagerOpenSavepoint records nOrig and iOffset, pager.c; pagerPlaybackSavepoint
// replays back to it, pager.c:3452). This is the same shape over the row store:
// while a transaction is open, every put, drop and clear records what it
// replaced, a snapshot holds an OFFSET into the record, and a rollback replays
// it backwards to that offset through the store's own mutators -- so the sorted
// order, the max rowid and the equality and unique indexes are restored by the
// same code that maintains them. A savepoint rolled back to repeatedly keeps its
// offset, and the log is simply replayed to it again.
//
// Only the long-lived snapshots use it (captureSharedSnapshot: BEGIN and
// SAVEPOINT). The statement-scoped ones -- a trigger's, a pragma's, the write
// path's own undo -- still copy, exactly as before.

// rsUndo is one mutation's inverse: put back old (had) or remove rowid (!had),
// or, for a clear, reinstate the whole map it set aside.
type rsUndo struct {
	rowid uint64
	old   []Value
	had   bool
	prior map[uint64][]Value
	// segClear marks a clear of a segment-backed store (row_store_seg.go), which set
	// aside its overlay, its tombstones and the visibility of its whole base.
	segClear      bool
	priorGone     map[uint64]bool
	priorBaseGone bool
	priorN        int
	priorNKnown   bool
}

// startUndo turns the log on, if it is not already, and returns the offset a
// snapshot taken now must roll back to. A page-backed store has its pager's own
// savepoints and returns -1.
func (s *rowStore) startUndo() int {
	if s == nil {
		return -1
	}
	if !s.logging {
		s.logging = true
		s.undo = s.undo[:0]
	}
	return len(s.undo)
}

// undoTo replays the log backwards to mark and truncates it there, leaving the
// store exactly as it was when mark was taken. Logging stays as it was: the
// transaction is still open after a ROLLBACK TO.
func (s *rowStore) undoTo(mark int) {
	if s == nil || mark < 0 || mark > len(s.undo) {
		return
	}
	logging := s.logging
	s.logging = false // the replay must not log itself
	for i := len(s.undo) - 1; i >= mark; i-- {
		u := s.undo[i]
		switch {
		case u.segClear:
			s.m, s.gone, s.baseGone, s.n, s.nKnown = u.prior, u.priorGone, u.priorBaseGone, u.priorN, u.priorNKnown
			s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
			s.overValid, s.memBytes = false, 0 // the prior map's in-memory share is not known
			s.unspilt = nil
			s.rangeKnown = false
			s.invalidateEqIndexes()
			s.uqIdx = nil
		case u.prior != nil:
			s.m = u.prior
			s.max, s.maxKnown, s.sorted, s.sortedValid = 0, false, nil, false
			s.overValid, s.memBytes = false, 0 // the prior map's in-memory share is not known
			s.unspilt = nil
			s.rangeKnown = false
			s.invalidateEqIndexes()
			s.uqIdx = nil
		case u.had:
			s.put(u.rowid, u.old)
		default:
			s.drop(u.rowid)
		}
	}
	clear(s.undo[mark:])
	s.undo = s.undo[:mark]
	s.logging = logging
}

// stopUndo ends the log with the transaction.
func (s *rowStore) stopUndo() {
	if s == nil {
		return
	}
	s.logging = false
	clear(s.undo)
	s.undo = nil
}
