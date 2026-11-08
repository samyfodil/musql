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
	"unsafe"
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
	// made is the buckets in the order they were made, and unordered whether
	// one was made below the one before it: until then made is already the
	// drain's order (a scan in key order, the columnar walk's dense groups).
	made      []*hashAggBucket
	unordered bool
	// slab is where each bucket, its key and its accumulators are carved from.
	slab aggSlab
}

// aggSlab carves a hash aggregate's groups out of chunks, so a group costs no
// allocation of its own: its bucket, key, accumulator set and items. Each
// piece is capped at its length, so an append to one never reaches the next.
// Chunks grow with the groups made, from 16 groups' worth to 1024, so at
// most that many groups' worth is made and not used; an exact slab makes
// each piece alone (newAggAccs).
type aggSlab struct {
	exact   bool
	n       int
	buckets []hashAggBucket
	accs    []aggAccumulators
	items   []aggItem
	ptrs    []*aggItem
	sets    [][]*aggItem
	vals    []Value
	keys    []byte
}

// key is k as a string carved from the slab's bytes, which nothing writes
// again: a bucket map's key.
func (s *aggSlab) key(k []byte) string {
	if len(k) == 0 {
		return ""
	}
	if len(s.keys) < len(k) {
		s.keys = make([]byte, max(len(k), s.chunk(1)*16))
	}
	out := s.keys[:len(k):len(k)]
	s.keys = s.keys[len(k):]
	copy(out, k)
	return unsafe.String(&out[0], len(out))
}

// cloneValues is vs with every TEXT and BLOB carved from the slab's bytes: a
// group's key, taken from a record that is only a view of its registers, or
// its anchor.
func (s *aggSlab) cloneValues(vs []Value) []Value {
	if vs == nil {
		return nil
	}
	out := s.values(vs)
	for i := range out {
		if b := out[i].S; b != nil {
			if len(s.keys) < len(b) {
				s.keys = make([]byte, max(len(b), s.chunk(1)*16))
			}
			out[i].S = s.keys[:len(b):len(b)]
			s.keys = s.keys[len(b):]
			copy(out[i].S, b)
		}
	}
	return out
}

func (s *aggSlab) chunk(need int) int {
	if s.exact {
		return need
	}
	return max(need, min(max(s.n, 16), 1024))
}

func (s *aggSlab) bucket() *hashAggBucket {
	if len(s.buckets) == 0 {
		s.buckets = make([]hashAggBucket, s.chunk(1))
	}
	b := &s.buckets[0]
	s.buckets = s.buckets[1:]
	s.n++
	return b
}

func (s *aggSlab) acc() *aggAccumulators {
	if len(s.accs) == 0 {
		s.accs = make([]aggAccumulators, s.chunk(1))
	}
	a := &s.accs[0]
	s.accs = s.accs[1:]
	return a
}

func (s *aggSlab) values(vs []Value) []Value {
	n := len(vs)
	if len(s.vals) < n {
		// A group asks for its key, its anchor and its rowids in turn, so a
		// chunk sized by one request would leave too little for the next.
		s.vals = make([]Value, max(n, s.chunk(1)*16))
	}
	out := s.vals[:n:n]
	s.vals = s.vals[n:]
	copy(out, vs)
	return out
}

func (s *aggSlab) itemSets(n int) [][]*aggItem {
	if n == 0 {
		return [][]*aggItem{}
	}
	if len(s.sets) < n {
		s.sets = make([][]*aggItem, s.chunk(1)*n)
	}
	out := s.sets[:n:n]
	s.sets = s.sets[n:]
	return out
}

// clone is cloneAggTemplates over the slab.
func (s *aggSlab) clone(templates []*aggItem) []*aggItem {
	n := len(templates)
	if n == 0 {
		return nil
	}
	if len(s.items) < n {
		s.items = make([]aggItem, s.chunk(1)*n)
	}
	if len(s.ptrs) < n {
		s.ptrs = make([]*aggItem, s.chunk(1)*n)
	}
	accs, items := s.ptrs[:n:n], s.items[:n:n]
	s.ptrs, s.items = s.ptrs[n:], s.items[n:]
	for i, tmpl := range templates {
		accs[i] = &items[i]
		resetAggItem(accs[i], tmpl)
	}
	return accs
}

// newAggAccs makes a group's accumulator set, one item per template.
func (s *aggSlab) newAggAccs(plan *aggPlan) *aggAccumulators {
	a := s.acc()
	*a = aggAccumulators{out: s.itemSets(len(plan.outPlans)), check: plan.anchorCheck, rowFree: plan.rowFree, rowWidth: plan.rowWidth, keep: plan.anchorKeep}
	for i, it := range plan.outPlans {
		a.out[i] = s.clone(it.aggTemplates)
	}
	if plan.havingPlan != nil {
		a.having = s.clone(plan.havingPlan.aggTemplates)
	}
	if plan.magnet != nil {
		a.magnets = s.clone(plan.magnet.sites)
		a.magnetS = plan.magnet.allocS
	}
	a.order = s.itemSets(len(plan.orderPlans))
	for i, op := range plan.orderPlans {
		if op.item != nil {
			a.order[i] = s.clone(op.item.aggTemplates)
		}
	}
	if !s.exact && len(a.magnets) == 0 {
		// With no census site the anchor is the group's first row, taken once
		// (magnetWalk).
		a.slab = s
	}
	return a
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
	if plan.rowFree {
		// Nothing reads the row (aggPlan.rowFree): the steps read their
		// registers.
		if n := plan.nCols + len(plan.scopes); len(m.aggNullRow) < n {
			m.aggNullRow = make([]Value, n)
		}
		return m.hashAggStepRow(plan, keyVals, seg, m.aggNullRow[:plan.nCols:plan.nCols], m.aggNullRow[plan.nCols:plan.nCols+len(plan.scopes)])
	}
	if plan.rowCols != nil && m.hashAgg != nil {
		// The row is read only for a group's anchor, which a group with no
		// census site took on its first row (magnetWalk): an existing such
		// group steps from its registers alone.
		m.aggKeyBuf = hashAggKeyBytes(m.aggKeyBuf[:0], keyVals)
		if b := m.hashAgg.buckets[string(m.aggKeyBuf)]; b != nil && b.accs.seenRow && len(b.accs.magnets) == 0 && b.accs.check == nil {
			if n := plan.nCols + len(plan.scopes); len(m.aggNullRow) < n {
				m.aggNullRow = make([]Value, n)
			}
			m.aggAccs = b.accs
			return m.aggStep(m.aggRowCtx(plan, m.aggNullRow[:plan.nCols:plan.nCols], m.aggNullRow[plan.nCols:plan.nCols+len(plan.scopes)]), seg)
		}
	}
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
		h := m.hashAgg
		b = h.slab.bucket()
		*b = hashAggBucket{key: h.slab.cloneValues(keyVals), accs: h.slab.newAggAccs(plan)}
		h.buckets[h.slab.key(m.aggKeyBuf)] = b
		if n := len(h.made); n > 0 && !h.unordered && !keyTupleLess(h.made[n-1].key, b.key, m.encoding()) {
			h.unordered = true
		}
		h.made = append(h.made, b)
	}
	m.aggAccs = b.accs
	// JSON subtype is cleared here for grouped queries, matching the sorted drain path.
	clearJSONSubtype(row)
	return m.aggStep(m.aggRowCtx(plan, row, rowids), seg)
}

// stepGathered is step's body for each of rows, the registers native code
// gathered at it (gatherAggStep), in the order it gathered them. The
// registers are lent to each row in turn and then hold again what native
// code left in them.
func (m *vdbe) stepGathered(jx *vmJIT, step *Instruction, rows [][]Value) error {
	if len(rows) == 0 {
		return nil
	}
	if vmJITCountSyncs {
		vmJITAggGathered.Add(int64(len(rows)))
	}
	plan := step.P4.(*aggPlan)
	regs := m.regs[jx.aggLo:jx.aggHi]
	m.aggSaved = append(m.aggSaved[:0], regs...)
	defer func() {
		copy(regs, m.aggSaved)
		clear(m.aggSaved)
	}()
	key := m.regs[jx.aggKeyLo : jx.aggKeyLo+jx.aggKeyN : jx.aggKeyLo+jx.aggKeyN]
	if plan.rowFree {
		for _, r := range rows {
			copy(regs, r)
			if err := m.hashAggStep(plan, key, step.P2); err != nil {
				return err
			}
		}
		return nil
	}
	// The row a new group takes as its anchor, its columns the key's values
	// (aggPlan.rowFromKeys); nothing reads the rest of it.
	n := plan.nCols + len(plan.scopes)
	row := growValues(&m.aggRowBuf, n)
	for _, r := range rows {
		copy(regs, r)
		for i, c := range plan.keyCols {
			if c >= 0 {
				row[c] = key[i]
			}
		}
		if err := m.hashAggStepRow(plan, key, step.P2, row[:plan.nCols:plan.nCols], row[plan.nCols:]); err != nil {
			return err
		}
	}
	return nil
}

// hashAggSort is OpHashAggSort's body: finalize m.hashAgg's buckets into
// group-key order (keyTupleLess), descending in the keys desc marks as sorter1
// sorts them, or with made the order they were made in, reporting whether
// there is at least one group -- mirroring vdbeSorter.sort's "jump if empty"
// convention.
func (m *vdbe) hashAggSort(desc []bool, made bool) bool {
	if m.hashAgg == nil {
		return false
	}
	h := m.hashAgg
	h.sorted = h.made
	if !made && (h.unordered || desc != nil) {
		// BINARY order is memcmp over the encoded text, which in a UTF-16
		// database is not UTF-8 byte order (utf16CompareUTF8); the sorted
		// drain's sorter orders its groups the same way.
		enc := m.encoding()
		sort.Slice(h.sorted, func(i, j int) bool {
			if desc != nil {
				return keyTupleLessDesc(h.sorted[i].key, h.sorted[j].key, desc, enc)
			}
			return keyTupleLess(h.sorted[i].key, h.sorted[j].key, enc)
		})
	}
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
