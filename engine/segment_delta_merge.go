package engine

import "slices"

// Merging a delta into a columnar fast path: computing corrections for rows
// the log superseded, removed, or added. The merge is O(delta log rows), not
// O(rows), using binary search over segment rowids.

// segDeltaCorrection returns the (subtract, add) pair for one table's log
// under a predicate set: rows the log invalidated vs. rows it added that match.
func (src *segSource) segDeltaCorrection(rootPage uint32, segs []*segment, preds []segPred) (sub, add int, served bool) {
	if src.delta == nil {
		return 0, 0, true
	}
	ti, known := src.tableOf[rootPage]
	if !known {
		// No log for this table at all, which is the clean case.
		return 0, 0, true
	}
	live := src.delta.live[ti]
	dead := src.delta.dead[ti]
	if len(live) == 0 && len(dead) == 0 {
		return 0, 0, true
	}
	// SUBTRACT: every rowid the log names -- superseded (live) or removed (dead)
	// -- whose row is still sitting in a segment and matched the predicates
	// there. Both maps are walked, because a live rowid that ALSO exists in a
	// segment is counted twice otherwise: once by the segments and once by the
	// add below.
	for _, m := range []map[int64]bool{dead} {
		for rid := range m {
			matched, ok := segRowMatches(segs, rid, preds)
			if !ok {
				return 0, 0, false
			}
			if matched {
				sub++
			}
		}
	}
	for rid := range live {
		matched, ok := segRowMatches(segs, rid, preds)
		if !ok {
			return 0, 0, false
		}
		if matched {
			sub++
		}
	}
	// ADD: the log's own live rows, tested as ROWS rather than through a column
	// block -- they are row-major and there is no block to read.
	for _, vals := range live {
		matched, ok := rowMatchesPreds(vals, preds)
		if !ok {
			return 0, 0, false
		}
		if matched {
			add++
		}
	}
	return sub, add, true
}

// segRowMatches reports whether the row stored under rid IN THE SEGMENTS
// satisfies preds. A rowid no segment holds matches nothing, which is not a
// refusal: a row the log added has no segment copy to subtract.
func segRowMatches(segs []*segment, rid int64, preds []segPred) (matched, served bool) {
	for _, s := range segs {
		i, hit := s.findRowid(rid)
		if !hit {
			continue
		}
		return rowMatchesPreds(s.rowAt(i), preds)
	}
	return false, true
}

// rowMatchesPreds evaluates preds against one materialized row, using the SAME
// comparator the column kernel uses (compareValues, via segPredHolds) so a row
// tested here and a row tested through a block cannot disagree.
//
// served is false for a column the row does not reach: that row's value is the
// column DEFAULT, which this does not know, so the exact answer is unavailable
// and the caller must decline rather than guess.
func rowMatchesPreds(vals []Value, preds []segPred) (matched, served bool) {
	for _, pr := range preds {
		if pr.Col < 0 || pr.Col >= len(vals) {
			return false, false
		}
		v := vals[pr.Col]
		// A NULL on either side makes no comparison TRUE, whatever the operator
		// -- the same rule the column kernel applies (segment_filter.go's own
		// note on why compareValues alone will NOT do this: it ORDERS a NULL
		// below every value, which is right for a sort and wrong for a WHERE).
		if v.Typ == Null || pr.Val.Typ == Null {
			return false, true
		}
		if !pr.Op.satisfied(compareValues(v, pr.Val)) {
			return false, true
		}
	}
	return true, true
}

// segDeltaSkips is, per segment, the sorted ROW POSITIONS the log invalidates --
// a rowid it superseded or removed -- so a driver walking the blocks can skip
// them with a merge pointer instead of a map probe per row.
//
// That distinction is the whole reason this exists. The log's rowids are few and
// the segment's rows are many, so asking "is this row's rowid in the log?" once
// per row is O(rows) map lookups, which at 100,000 rows costs more than the scan
// the fast path is replacing. Turning each of the log's rowids into a POSITION
// once, up front (segment.findRowid, a binary search) and sorting the result is
// O(delta * log rows), and the walk that consumes it is O(rows + delta).
//
// served is false when a position cannot be resolved exactly, which no caller may
// paper over: skipping the wrong row is a wrong answer.
func (src *segSource) segDeltaSkips(rootPage uint32, segs []*segment) (skips [][]int, live map[int64][]Value, served bool) {
	skips = make([][]int, len(segs))
	if src.delta == nil {
		return skips, nil, true
	}
	ti, known := src.tableOf[rootPage]
	if !known {
		return skips, nil, true
	}
	live = src.delta.live[ti]
	dead := src.delta.dead[ti]
	if len(live) == 0 && len(dead) == 0 {
		return skips, nil, true
	}
	mark := func(rid int64) {
		for si, s := range segs {
			if i, hit := s.findRowid(rid); hit {
				skips[si] = append(skips[si], i)
				return
			}
		}
		// No segment holds it: a row the log ADDED, with nothing to skip.
	}
	for rid := range live {
		mark(rid)
	}
	for rid := range dead {
		mark(rid)
	}
	for si := range skips {
		slices.Sort(skips[si])
	}
	return skips, live, true
}

// segDeltaSum is the delta's correction to a filtered sum: the matching
// segment copies it superseded or removed (sub), its own matching rows (add),
// and the sum of every one of their absolute values, for the caller's overflow
// bound. A NULL value counts toward neither, as sum() skips it.
type segDeltaSum struct {
	subSum, addSum int64
	subN, addN     int
	abs            uint64
}

// segDeltaSumCorrection is segDeltaCorrection for sum(col): which rows the
// delta takes away from and adds to the segments' answer, and by how much.
// served is false where the exact answer is out of reach -- a value that is
// not an integer, a column a row does not reach, an absolute sum past int64.
func (src *segSource) segDeltaSumCorrection(rootPage uint32, segs []*segment, preds []segPred, col int) (segDeltaSum, bool) {
	var dc segDeltaSum
	if src.delta == nil {
		return dc, true
	}
	ti, known := src.tableOf[rootPage]
	if !known {
		return dc, true
	}
	live, dead := src.delta.live[ti], src.delta.dead[ti]
	if len(live) == 0 && len(dead) == 0 {
		return dc, true
	}
	// take folds one row's value into sum, n and dc.abs, if it matches.
	take := func(vals []Value, sum *int64, n *int) bool {
		matched, ok := rowMatchesPreds(vals, preds)
		if !ok {
			return false
		}
		if !matched {
			return true
		}
		if col < 0 || col >= len(vals) {
			return false
		}
		v := vals[col]
		switch v.Typ {
		case Null:
			return true
		case Int:
		default:
			return false
		}
		u := uint64(v.I)
		if v.I < 0 {
			u = -u
		}
		if dc.abs+u < dc.abs || dc.abs+u > 1<<63-1 {
			return false
		}
		dc.abs += u
		*sum += v.I
		*n++
		return true
	}
	// SUBTRACT the segment copy of every rowid the log names: superseded or
	// removed, the segments counted it and the merged scan does not.
	sub := func(rid int64) bool {
		for _, s := range segs {
			if i, hit := s.findRowid(rid); hit {
				return take(s.rowAt(i), &dc.subSum, &dc.subN)
			}
		}
		return true
	}
	for rid := range dead {
		if !sub(rid) {
			return dc, false
		}
	}
	for rid, vals := range live {
		if !sub(rid) || !take(vals, &dc.addSum, &dc.addN) {
			return dc, false
		}
	}
	return dc, true
}
