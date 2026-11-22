// Package engine implements the GROUP BY hash-aggregate fast-path opcodes.
// Hash aggregates store accumulators in buckets indexed by encoded group keys,
// then sort buckets and drain them in order. Two key tuples hash identically
// iff they compare equal under GROUP BY's equality rule, and buckets are sorted
// by the same order as the sort-based path produces, ensuring byte-identical
// results.
package engine

import (
	"math"
	"math/bits"
	"sort"
	"unsafe"
)

// hashAggBucket is one GROUP BY hash-aggregate bucket: the group's own
// (first-seen) key tuple and its live accumulator set, built up by
// OpHashAggStep as matching rows stream past in scan order.
type hashAggBucket struct {
	key  []Value
	// first is the rows stepped before this group's first, on a plan C
	// answers as a stream (aggPlan.cStreams).
	first int64
	accs *aggAccumulators
	// at is the group's index into a dense aggregate's state (hashAggState.dense).
	at int32
}

// hashAggState is the VM's live hash-aggregate state for one compiled GROUP
// BY program (vdbe.hashAgg). buckets accumulates one entry per distinct
// group-key tuple seen so far (OpHashAggStep), keyed by hashAggKeyBytes.
// sorted/pos are populated once, by OpHashAggSort, from buckets: sorted in
// group-key ascending order (keyTupleLess), then walked by OpHashAggData/
// OpHashAggNext exactly like a vdbeSorter's post-sort entries.
type hashAggState struct {
	buckets map[string]*hashAggBucket
	// ints is the buckets of the keys of INTEGERs and NULLs (hashAggIntKey),
	// which buckets does not hold, and small those of a key of one INTEGER
	// below hashAggSmallKeys, indexed by it, which ints does not hold.
	ints   map[hashAggIntKey]*hashAggBucket
	small  []*hashAggBucket
	sorted []*hashAggBucket
	pos    int
	// made is the buckets in the order they were made, and unordered whether
	// one was made below the one before it: until then made is already the
	// drain's order (a scan in key order, the columnar walk's dense groups).
	made      []*hashAggBucket
	unordered bool
	// slab is where each bucket, its key and its accumulators are carved from.
	slab aggSlab

	// A plan denseAggPlan accepts keeps no accumulator set per group: each
	// group's accumulators are values, len(denseItems) of them (denseOf),
	// with its anchor flags in denseFlags[at], and the one set denseAccs,
	// whose items are denseItems, is loaded with the group a step or the
	// drain works on (denseCur).
	dense      [][]aggSumState
	denseFlags []uint8
	denseAccs  *aggAccumulators
	denseItems []*aggItem
	denseCur   *hashAggBucket

	// stream is set on a plan C answers as a stream (aggPlan.cStreams): rows
	// is the rows stepped, emitted how many of sorted's groups had a result
	// row when the last went out, and billed what streamReads counted so far.
	stream  bool
	rows    int64
	emitted int
	billed  int64
}

// denseChunk is how many groups the first of dense's chunks holds; each
// chunk holds twice the one before it, so a new group never moves the values
// of those made before it.
const denseChunk = 64

// denseChunkOf is the chunk group at's values are in, and its place there.
func denseChunkOf(at int32) (chunk, off int) {
	chunk = bits.Len(uint(at)/denseChunk+1) - 1
	return chunk, int(at) - denseChunk*(1<<chunk-1)
}

// denseOf is group at's accumulator values.
func (h *hashAggState) denseOf(at int32) []aggSumState {
	n := len(h.denseItems)
	c, off := denseChunkOf(at)
	return h.dense[c][off*n : off*n+n]
}

// denseAggOffForTest keeps every group's own accumulator set, so a test can
// hold dense groups to it.
var denseAggOffForTest bool

const (
	denseSeen = 1 << iota
	denseHit
	denseAnchor
)

// denseAggPlan reports whether plan's groups can keep their accumulators as
// aggSumState values: every aggregate a count, sum, total or avg with no
// DISTINCT or ORDER BY, no census site or anchor certificate, and an anchor
// that is no row or the group's key (rowFromKeys), so a group's state is its
// accumulators' numbers and three flags.
func denseAggPlan(plan *aggPlan) bool {
	if plan.anchorCheck != nil || plan.magnet != nil && len(plan.magnet.sites) > 0 || !plan.rowFree && !plan.rowFromKeys() {
		return false
	}
	ok := func(it *itemPlan) bool {
		if it == nil {
			return true
		}
		for _, t := range it.aggTemplates {
			switch t.kind {
			case aggCountStar, aggCount, aggSum, aggTotal, aggAvg:
			default:
				return false
			}
			if t.distinct || t.orderBy != nil {
				return false
			}
		}
		return true
	}
	if !ok(plan.havingPlan) {
		return false
	}
	for _, it := range plan.outPlans {
		if !ok(it) {
			return false
		}
	}
	for _, op := range plan.orderPlans {
		if !ok(op.item) {
			return false
		}
	}
	return true
}

// startDense makes h a dense aggregate over plan: the one accumulator set
// groups are loaded into.
func (h *hashAggState) startDense(plan *aggPlan) {
	a := newAggAccs(plan)
	h.denseAccs = a
	for _, set := range a.out {
		h.denseItems = append(h.denseItems, set...)
	}
	h.denseItems = append(h.denseItems, a.having...)
	for _, set := range a.order {
		h.denseItems = append(h.denseItems, set...)
	}
}

// accsOf is b's accumulator set: its own, or a dense aggregate's one set
// loaded with b's state.
func (h *hashAggState) accsOf(b *hashAggBucket) *aggAccumulators {
	if h.denseAccs == nil {
		return b.accs
	}
	if h.denseCur == b {
		return h.denseAccs
	}
	h.storeDense()
	ds := h.denseOf(b.at)
	for k, it := range h.denseItems {
		it.aggSumState = ds[k]
	}
	a, f := h.denseAccs, h.denseFlags[b.at]
	a.seenRow, a.magnetHit, a.haveAnchor = f&denseSeen != 0, f&denseHit != 0, f&denseAnchor != 0
	// A row-free plan's anchor is the shared NULL row every step points it
	// at, so it is kept; conv is each group's result's own.
	a.conv = nil
	h.denseCur = b
	return a
}

// storeDense saves the loaded group's state from the one accumulator set.
func (h *hashAggState) storeDense() {
	b := h.denseCur
	if b == nil {
		return
	}
	ds := h.denseOf(b.at)
	for k, it := range h.denseItems {
		ds[k] = it.aggSumState
	}
	a := h.denseAccs
	var f uint8
	if a.seenRow {
		f |= denseSeen
	}
	if a.magnetHit {
		f |= denseHit
	}
	if a.haveAnchor {
		f |= denseAnchor
	}
	h.denseFlags[b.at] = f
	h.denseCur = nil
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
	// nVals is the values every group so far asked for, which sizes the next
	// chunk of them.
	nVals int
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
	s.nVals += n
	if len(s.vals) < n {
		// A group asks for its key, its anchor and its rowids in turn, so a
		// chunk sized by one request would leave too little for the next:
		// it holds the chunk's groups at what the groups made so far took
		// each, or 16 before there are any.
		per := 16
		if s.n > 0 {
			per = s.nVals/s.n + 1
		}
		s.vals = make([]Value, max(n, s.chunk(1)*per))
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
	*a = aggAccumulators{out: s.itemSets(len(plan.outPlans)), check: plan.anchorCheck, rowFree: plan.rowFree, rowWidth: plan.rowWidth, cols: plan.rowCols, keep: plan.anchorKeep,
		fromKeys: plan.rowFromKeys()}
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
		var a *aggAccumulators
		if b, _, _ := m.hashAggFind(keyVals); b != nil {
			a = m.hashAgg.accsOf(b)
		}
		if a != nil && a.seenRow && len(a.magnets) == 0 && a.check == nil {
			if n := plan.nCols + len(plan.scopes); len(m.aggNullRow) < n {
				m.aggNullRow = make([]Value, n)
			}
			m.aggAccs = a
			if plan.cStreams {
				m.hashAgg.rows++
			}
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
		if !denseAggOffForTest && denseAggPlan(plan) {
			m.hashAgg.startDense(plan)
		}
	}
	b, ik, isInt := m.hashAggFind(keyVals)
	if b == nil {
		h := m.hashAgg
		b = h.slab.bucket()
		if h.denseAccs != nil {
			at := int32(len(h.denseFlags))
			*b = hashAggBucket{key: h.slab.cloneValues(keyVals), at: at}
			if c, _ := denseChunkOf(at); c == len(h.dense) {
				h.dense = append(h.dense, make([]aggSumState, denseChunk<<c*len(h.denseItems)))
			}
			h.denseFlags = append(h.denseFlags, 0)
		} else {
			*b = hashAggBucket{key: h.slab.cloneValues(keyVals), accs: h.slab.newAggAccs(plan)}
		}
		if k, isSmall := hashAggSmall(ik, isInt); isSmall {
			if k >= len(h.small) {
				h.small = append(h.small, make([]*hashAggBucket, max(1024, 1<<bits.Len(uint(k)))-len(h.small))...)
			}
			h.small[k] = b
		} else if isInt {
			if h.ints == nil {
				h.ints = map[hashAggIntKey]*hashAggBucket{}
			}
			h.ints[ik] = b
		} else {
			h.buckets[h.slab.key(m.aggKeyBuf)] = b
		}
		if n := len(h.made); n > 0 && !h.unordered && !keyTupleLess(h.made[n-1].key, b.key, m.encoding()) {
			h.unordered = true
		}
		h.made = append(h.made, b)
		if plan.cStreams {
			h.stream = true
			b.first = h.rows
		}
	}
	if plan.cStreams {
		m.hashAgg.rows++
	}
	m.aggAccs = m.hashAgg.accsOf(b)
	// JSON subtype is cleared here for grouped queries, matching the sorted drain path.
	clearJSONSubtype(row)
	return m.aggStep(m.aggRowCtx(plan, row, rowids), seg)
}

// hashAggIntKey is a group key of up to four INTEGERs and NULLs by value,
// nulls marking the NULLs: hashed and compared in place, where the string
// key's bytes are hashed again at every growth of the table.
type hashAggIntKey struct {
	v        [4]int64
	n, nulls uint8
}

// hashAggIntKeyOf is vals as a hashAggIntKey, which two keys share exactly
// when their string keys (hashAggKeyBytes) are the same: an integer-valued
// REAL keys as its INTEGER. False for any other kind, or more than four.
func hashAggIntKeyOf(vals []Value) (hashAggIntKey, bool) {
	k := hashAggIntKey{n: uint8(len(vals))}
	if len(vals) > len(k.v) {
		return k, false
	}
	for i, v := range vals {
		switch v.Typ {
		case Int:
			k.v[i] = v.I
		case Null:
			k.nulls |= 1 << i
		case Float:
			n, ok := floatAsCanonicalInt(v.F)
			if !ok {
				return k, false
			}
			k.v[i] = n
		default:
			return k, false
		}
	}
	return k, true
}

// hashAggFind is the bucket of keyVals, or nil, and the key it is or would be
// kept under: ik where isInt, else m.aggKeyBuf's bytes.
func (m *vdbe) hashAggFind(keyVals []Value) (b *hashAggBucket, ik hashAggIntKey, isInt bool) {
	h := m.hashAgg
	if ik, isInt = hashAggIntKeyOf(keyVals); isInt && !hashAggIntKeysOffForTest {
		if k, isSmall := hashAggSmall(ik, true); isSmall {
			if k < len(h.small) {
				return h.small[k], ik, true
			}
			return nil, ik, true
		}
		return h.ints[ik], ik, true
	}
	// Key bytes are reused per row: buckets store their own copy, so a lookup
	// key is discarded after use. String(...) indexing avoids per-row
	// allocations.
	m.aggKeyBuf = hashAggKeyBytes(m.aggKeyBuf[:0], keyVals)
	return h.buckets[string(m.aggKeyBuf)], ik, false
}

// hashAggIntKeysOffForTest keys every group by its bytes, so a test can hold
// the INTEGER keys to them.
var hashAggIntKeysOffForTest bool

// groups is how many groups h has made.
func (h *hashAggState) groups() int { return len(h.made) }

// hashAggSmallKeys bounds the keys hashAggState.small holds: 512 KiB of
// pointers at most, where a map grown to as many groups costs far more.
const hashAggSmallKeys = 1 << 16

// hashAggSmall is the index in hashAggState.small of a key of one INTEGER
// ik, where it has one.
func hashAggSmall(ik hashAggIntKey, isInt bool) (int, bool) {
	if !isInt || ik.n != 1 || ik.nulls != 0 || ik.v[0] < 0 || ik.v[0] >= hashAggSmallKeys {
		return 0, false
	}
	return int(ik.v[0]), true
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
	if step.Op == OpAggStep {
		// Every row steps from its registers alone, so one context serves the
		// batch.
		row, rowids := m.aggStepNullRow(plan)
		ctx := m.aggRowCtx(plan, row, rowids)
		for _, r := range rows {
			copy(regs, r)
			if err := m.aggStep(ctx, step.P2); err != nil {
				return err
			}
		}
		return nil
	}
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
		if hashAggSortInts(h.sorted, desc) {
			h.pos = 0
			return len(h.sorted) > 0
		}
		enc := m.encoding()
		if !isUTF16(enc) && hashAggSortKeys(h.sorted, desc) {
			h.pos = 0
			return len(h.sorted) > 0
		}
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

// hashAggSortInts sorts buckets whose keys are all INTEGERs or NULLs as
// keyTupleLessDesc orders them, by stable radix passes over a columnar copy
// of the keys, the last column first, each followed by a stable partition
// that puts its NULLs first, or last where the column is descending; it
// reports false, with buckets untouched, for any other key.
func hashAggSortInts(buckets []*hashAggBucket, desc []bool) bool {
	n := len(buckets)
	if n == 0 {
		return false
	}
	nk := len(buckets[0].key)
	vals := make([]int64, n*nk)
	var nulls []bool
	for i, b := range buckets {
		if len(b.key) != nk {
			return false
		}
		for k, v := range b.key {
			switch v.Typ {
			case Int:
				vals[k*n+i] = v.I
			case Null:
				if nulls == nil {
					nulls = make([]bool, n*nk)
				}
				nulls[k*n+i] = true
			default:
				return false
			}
		}
	}
	order := make([]int32, n)
	for i := range order {
		order[i] = int32(i)
	}
	keys, tk, to := make([]uint64, n), make([]uint64, n), make([]int32, n)
	for k := nk - 1; k >= 0; k-- {
		flip := uint64(1) << 63
		down := k < len(desc) && desc[k]
		if down {
			flip = ^uint64(0) >> 1
		}
		col := vals[k*n : (k+1)*n]
		and, or := ^uint64(0), uint64(0)
		for i, j := range order {
			u := uint64(col[j]) ^ flip
			keys[i] = u
			and &= u
			or |= u
		}
		for shift := uint(0); shift < 64; shift += 8 {
			if (and^or)>>shift&0xff == 0 {
				continue
			}
			var at [256]int
			for _, u := range keys {
				at[u>>shift&0xff]++
			}
			sum := 0
			for d, c := range at {
				at[d] = sum
				sum += c
			}
			for i, u := range keys {
				d := u >> shift & 0xff
				tk[at[d]], to[at[d]] = u, order[i]
				at[d]++
			}
			keys, tk = tk, keys
			order, to = to, order
		}
		if nulls != nil {
			isNull := nulls[k*n : (k+1)*n]
			out := to[:0]
			for _, j := range order {
				if isNull[j] != down {
					out = append(out, j)
				}
			}
			for _, j := range order {
				if isNull[j] == down {
					out = append(out, j)
				}
			}
			order, to = out, order
		}
	}
	sorted := make([]*hashAggBucket, n)
	for i, j := range order {
		sorted[i] = buckets[j]
	}
	copy(buckets, sorted)
	return true
}

// hashAggSortKeys sorts buckets as keyTupleLessDesc orders them in a UTF-8
// database, by the keys appendSortKey builds; false, with buckets untouched,
// where a key holds a REAL.
func hashAggSortKeys(buckets []*hashAggBucket, desc []bool) bool {
	if len(buckets) == 0 {
		return false
	}
	nk := len(buckets[0].key)
	perm := make([]int, len(buckets))
	for i := range perm {
		perm[i] = i
	}
	at := func(i, k int) Value {
		if k < len(buckets[i].key) {
			return buckets[i].key[k]
		}
		return Value{}
	}
	if !sortBySortKeys(perm, nk, at, func(k int) bool { return k < len(desc) && desc[k] }, nil) {
		return false
	}
	sorted := make([]*hashAggBucket, len(buckets))
	for i, j := range perm {
		sorted[i] = buckets[j]
	}
	copy(buckets, sorted)
	return true
}

// hashAggData is OpHashAggData's body: make the current (post-sort) bucket's
// accumulators live (so a following OpAggResult finalizes THIS group) and
// return its group-key tuple, mirroring vdbeSorter.data.
func (m *vdbe) hashAggData() []Value {
	b := m.hashAgg.sorted[m.hashAgg.pos]
	m.aggAccs = m.hashAgg.accsOf(b)
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

// streamReads is what a GROUP BY C answers as a stream (aggPlan.cStreams)
// has read beyond what it already counted. C walks its rows in group order
// and stops once the LIMIT is met: it reads the rows of every group up to the
// last with a result row, and the first row of the group after it, which is
// how it learns that group ended. The hash aggregate stepped every row, but
// counts only those; where its groups did not come in key order, all of them.
func (h *hashAggState) streamReads() int64 {
	n := h.rows
	if !h.unordered && h.emitted > 0 && len(h.sorted) == len(h.made) && h.emitted < len(h.made) && h.made[h.emitted] == h.sorted[h.emitted] {
		n = min(h.made[h.emitted].first+1, h.rows)
	}
	if n <= h.billed {
		return 0
	}
	d := n - h.billed
	h.billed = n
	return d
}
