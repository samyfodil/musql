// Package engine implements the GROUP BY hash-aggregate fast-path opcodes.
// Hash aggregates store accumulators in buckets indexed by encoded group keys,
// then sort buckets and drain them in order. Two key tuples hash identically
// iff they compare equal under GROUP BY's equality rule, and buckets are sorted
// by the same order as the sort-based path produces, ensuring byte-identical
// results.
package engine

import (
	"math"
	"sort"
)

// hashAggBucket is one GROUP BY hash-aggregate bucket: the group's own
// (first-seen) key tuple and its live accumulator set, built up by
// OpHashAggStep as matching rows stream past in scan order.
type hashAggBucket struct {
	key  []Value
	accs *aggAccumulators
}

// hashAggState is the VM's live hash-aggregate state for one compiled GROUP
// BY program (vdbe.hashAgg). buckets accumulates one entry per distinct
// group-key tuple seen so far (OpHashAggStep), keyed by hashAggKeyBytes.
// sorted/pos are populated once, by OpHashAggSort, from buckets: sorted in
// group-key ascending order (keyTupleLess), then walked by OpHashAggData/
// OpHashAggNext exactly like a vdbeSorter's post-sort entries.
type hashAggState struct {
	buckets map[string]*hashAggBucket
	sorted  []*hashAggBucket
	pos     int
}

// hashAggStep is OpHashAggStep's body: find or create keyVals' bucket, make
// it the live accumulator set (m.aggAccs, so it's what aggStep/OpAggResult
// operate on), and step this row (gathered from the currently-positioned
// join cursors, exactly OpAggStep's P3==0/gatherCursorRow mode) into it.
//
// seg is the STEP SEGMENT (aggItem.stepSeg -- see aggStep). A body that emits
// more than one step opcode per row runs this more than once for that row, and
// everything ahead of the aggStep call is idempotent for a row on purpose: the
// bucket lookup finds the bucket the first call created, and re-gathering
// re-reads cursors that have not moved. Only which accumulators advance
// differs.
func (m *vdbe) hashAggStep(plan *aggPlan, keyVals []Value, seg int) error {
	row, rowids, gerr := m.gatherCursorRow(plan)
	if gerr != nil {
		return gerr
	}
	return m.hashAggStepRow(plan, keyVals, seg, row, rowids)
}

// hashAggStepRow is hashAggStep with the row already in hand, for a caller that
// has one without a positioned cursor to gather it from -- the columnar GROUP BY
// driver (segment_group.go), which reads its values out of int64 column blocks.
//
// The gather moved OUT rather than the bucket lookup moving in: the two are
// independent (the gather reads cursors, the lookup reads the key bytes), so
// doing it first changes nothing, and it is the half a columnar caller replaces.
func (m *vdbe) hashAggStepRow(plan *aggPlan, keyVals []Value, seg int, row, rowids []Value) error {
	if m.hashAgg == nil {
		m.hashAgg = &hashAggState{buckets: make(map[string]*hashAggBucket)}
	}
	// Key bytes are reused per row: buckets store their own copy, so a lookup key
	// is discarded after use. String(...) indexing avoids per-row allocations.
	m.aggKeyBuf = hashAggKeyBytes(m.aggKeyBuf[:0], keyVals)
	b := m.hashAgg.buckets[string(m.aggKeyBuf)]
	if b == nil {
		b = &hashAggBucket{key: append([]Value(nil), keyVals...), accs: newAggAccs(plan)}
		m.hashAgg.buckets[string(m.aggKeyBuf)] = b
	}
	m.aggAccs = b.accs
	// JSON subtype is cleared here for grouped queries, matching the sorted drain path.
	clearJSONSubtype(row)
	return m.aggStep(m.aggRowCtx(plan, row, rowids), seg)
}

// hashAggSort is OpHashAggSort's body: finalize m.hashAgg's buckets into
// group-key-ascending order (keyTupleLess), reporting whether there is at
// least one group -- mirroring vdbeSorter.sort's "jump if empty" convention.
func (m *vdbe) hashAggSort() bool {
	if m.hashAgg == nil {
		return false
	}
	h := m.hashAgg
	h.sorted = make([]*hashAggBucket, 0, len(h.buckets))
	for _, b := range h.buckets {
		h.sorted = append(h.sorted, b)
	}
	sort.Slice(h.sorted, func(i, j int) bool {
		return keyTupleLess(h.sorted[i].key, h.sorted[j].key)
	})
	h.pos = 0
	return len(h.sorted) > 0
}

// hashAggData is OpHashAggData's body: make the current (post-sort) bucket's
// accumulators live (so a following OpAggResult finalizes THIS group) and
// return its group-key tuple, mirroring vdbeSorter.data.
func (m *vdbe) hashAggData() []Value {
	b := m.hashAgg.sorted[m.hashAgg.pos]
	m.aggAccs = b.accs
	return b.key
}

// hashAggNext is OpHashAggNext's body: advance to the next bucket in sorted
// order, mirroring vdbeSorter.next.
func (m *vdbe) hashAggNext() bool {
	m.hashAgg.pos++
	return m.hashAgg.pos < len(m.hashAgg.sorted)
}

// hashAggKeyBytes canonically encodes a GROUP BY key tuple into bytes such
// that two tuples match iff keysEqual(a, b). Int(1) and Float(1.0) encode
// identically, as they compare equal. buf is reused per-row by appending.
func hashAggKeyBytes(buf []byte, vals []Value) []byte {
	return appendCollatedKeyBytes(buf, vals, nil, UTF8)
}

// floatAsCanonicalInt reports whether f equals some int64 under compareNumeric's
// int/float comparison: finite, in range, zero fractional part, and (-0.0 == 0.0).
func floatAsCanonicalInt(f float64) (int64, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	if f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
		return 0, false
	}
	i := int64(f)
	if f-float64(i) != 0 {
		return 0, false
	}
	return i, true
}
