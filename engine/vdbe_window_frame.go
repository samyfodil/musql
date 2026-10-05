// This file implements a window function's explicit frame -- "ROWS | RANGE |
// GROUPS BETWEEN <bound> AND <bound> [EXCLUDE ...]" -- on top of
// vdbe_window.go's per-partition ordering. windowAggregate recomputes each
// row's accumulator over a span of the ordered partition, so a frame is a
// different span; windowFrameSpans computes it per row, once per (partition,
// spec).
//
// Over (a,b) = (1,A) (2,B) (3,C) (3,D) (5,E) (7,F) ordered by a, with one
// two-row peer group:
//
//	ROWS   BETWEEN 1 PRECEDING AND 1 FOLLOWING -> 3  6  8 11 15 12
//	RANGE  BETWEEN 1 PRECEDING AND 1 FOLLOWING -> 3  9  8  8  5  7
//	GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING -> 3  9 13 13 18 12
//	RANGE  BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW -> 1 3 9 9 14 21
//	ROWS   BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW -> 1 3 6 9 14 21
//
// and, over the whole partition:
//
//	EXCLUDE CURRENT ROW -> 20 19 18 18 16 14   (drops the row itself)
//	EXCLUDE GROUP       -> 20 19 15 15 16 14   (drops the row AND its peers)
//	EXCLUDE TIES        -> 21 21 18 18 21 21   (drops the peers, KEEPS the row)
//
// An empty frame is not an error: sum() is NULL and count() 0.
package engine

import (
	"fmt"
	"math"
)

// windowFrameSpan is one row's frame: the half-open [lo, hi) range of
// positions within its ordered partition, plus the peer group (also
// half-open) that an EXCLUDE clause removes from it.
type windowFrameSpan struct {
	lo, hi         int
	peerLo, peerHi int
}

// frameRowIncluded reports whether position j of the partition takes part in
// the frame of position k, applying the EXCLUDE clause. Called per candidate
// row rather than folded into lo/hi because EXCLUDE can punch a hole in the
// MIDDLE of a frame, which a single range cannot express.
func frameRowIncluded(f windowFrameSpan, frame *WindowFrame, k, j int) bool {
	if j < f.lo || j >= f.hi {
		return false
	}
	if frame == nil || !frame.Exclude {
		return true
	}
	switch frame.ExcludeKind {
	case ExcludeCurrentRow:
		return j != k
	case ExcludeGroup:
		return j < f.peerLo || j >= f.peerHi
	case ExcludeTies:
		return j == k || j < f.peerLo || j >= f.peerHi
	}
	return true
}

// checkWindowFrame validates a frame at compile time with C's wording:
//
//   - bounds order UNBOUNDED PRECEDING < N PRECEDING < CURRENT ROW < N
//     FOLLOWING < UNBOUNDED FOLLOWING, and the start may not come after the
//     end ("unsupported frame specification");
//   - UNBOUNDED PRECEDING only starts and UNBOUNDED FOLLOWING only ends a frame
//     (the grammar has no other production), so "BETWEEN UNBOUNDED PRECEDING
//     AND UNBOUNDED PRECEDING" is 'near "PRECEDING": syntax error' and the
//     FOLLOWING twin likewise -- the two cases the ordering rule misses;
//   - RANGE with an offset bound needs exactly one ORDER BY expression ("RANGE
//     with offset PRECEDING/FOLLOWING requires one ORDER BY expression").
func checkWindowFrame(spec *WindowSpec) error {
	f := spec.Frame
	if f == nil {
		return nil
	}
	if f.Start.Type > f.End.Type {
		return fmt.Errorf("engine: unsupported frame specification")
	}
	if f.End.Type == FrameUnboundedPreceding {
		return fmt.Errorf(`engine: near "PRECEDING": syntax error`)
	}
	if f.Start.Type == FrameUnboundedFollowing {
		return fmt.Errorf(`engine: near "FOLLOWING": syntax error`)
	}
	if f.Mode == FrameRange && (f.Start.Offset != nil || f.End.Offset != nil) && len(spec.OrderBy) != 1 {
		return fmt.Errorf("engine: RANGE with offset PRECEDING/FOLLOWING requires one ORDER BY expression")
	}
	return nil
}

// windowFrameSpans computes every row's frame span for one already-ordered
// partition. part holds the batch indices in window order; ends[k] is the
// index one past position k's last peer (windowPeerEnds), from which the peer
// group's start is re-derived here.
//
// A nil frame is SQLite's default -- RANGE BETWEEN UNBOUNDED PRECEDING AND
// CURRENT ROW, i.e. through the current row's last peer -- which is exactly
// what this engine computed before frames existed.
func (m *vdbe) windowFrameSpans(plan *windowPlan, call windowCall, part, ends []int, batch, vals [][]Value) ([]windowFrameSpan, error) {
	spec := call.spec
	np := len(part)
	starts := peerStarts(ends)
	spans := make([]windowFrameSpan, np)
	for k := range spans {
		spans[k].peerLo, spans[k].peerHi = starts[k], ends[k]
	}
	var f *WindowFrame
	if spec != nil {
		f = spec.Frame
	}
	if f == nil {
		for k := range spans {
			spans[k].lo, spans[k].hi = 0, ends[k]
		}
		return spans, nil
	}

	switch f.Mode {
	case FrameRows:
		startOff, err := m.frameIntOffset(call.startOffReg, true)
		if err != nil {
			return nil, err
		}
		endOff, err := m.frameIntOffset(call.endOffReg, false)
		if err != nil {
			return nil, err
		}
		for k := range spans {
			spans[k].lo = clampIndex(rowBound(f.Start.Type, k, startOff, np, true), np)
			spans[k].hi = clampIndex(rowBound(f.End.Type, k, endOff, np, false), np)
		}
	case FrameGroups:
		startOff, err := m.frameIntOffset(call.startOffReg, true)
		if err != nil {
			return nil, err
		}
		endOff, err := m.frameIntOffset(call.endOffReg, false)
		if err != nil {
			return nil, err
		}
		gIdx, gLo, gHi := peerGroups(starts, ends)
		nG := len(gLo)
		for k := range spans {
			g := gIdx[k]
			spans[k].lo = clampIndex(groupBound(f.Start.Type, g, startOff, gLo, gHi, nG, np, true), np)
			spans[k].hi = clampIndex(groupBound(f.End.Type, g, endOff, gLo, gHi, nG, np, false), np)
		}
	default: // FrameRange
		if err := m.rangeFrameSpans(plan, call, part, starts, ends, batch, vals, spans); err != nil {
			return nil, err
		}
	}
	for k := range spans {
		if spans[k].hi < spans[k].lo {
			spans[k].hi = spans[k].lo // an entirely empty frame; not an error
		}
	}
	return spans, nil
}

// rowBound resolves one ROWS-mode bound to a position within the partition:
// an inclusive index for a start bound, an EXCLUSIVE one for an end bound.
func rowBound(t FrameBoundType, k int, off int64, np int, isStart bool) int {
	switch t {
	case FrameUnboundedPreceding:
		return 0
	case FramePreceding:
		return offsetIndex(k, -off, isStart)
	case FrameCurrentRow:
		return offsetIndex(k, 0, isStart)
	case FrameFollowing:
		return offsetIndex(k, off, isStart)
	default: // FrameUnboundedFollowing
		return np
	}
}

// groupBound is rowBound in PEER-GROUP space: the offset counts groups, and
// the resulting group's first (start bound) or one-past-last (end bound)
// position is what bounds the frame.
func groupBound(t FrameBoundType, g int, off int64, gLo, gHi []int, nG, np int, isStart bool) int {
	target := g
	switch t {
	case FrameUnboundedPreceding:
		return 0
	case FramePreceding:
		target = g - int(off)
	case FrameCurrentRow:
	case FrameFollowing:
		target = g + int(off)
	default: // FrameUnboundedFollowing
		return np
	}
	if isStart {
		if target < 0 {
			return 0
		}
		if target >= nG {
			return np
		}
		return gLo[target]
	}
	if target < 0 {
		return 0
	}
	if target >= nG {
		return np
	}
	return gHi[target]
}

// offsetIndex applies a signed row offset to position k, returning the
// inclusive index for a start bound and the exclusive one for an end bound.
func offsetIndex(k int, delta int64, isStart bool) int {
	pos := int64(k) + delta
	if !isStart {
		pos++
	}
	// Clamped generously here; clampIndex does the final bounding.
	if pos < -1 {
		pos = -1
	}
	if pos > 1<<30 {
		pos = 1 << 30
	}
	return int(pos)
}

func clampIndex(i, np int) int {
	if i < 0 {
		return 0
	}
	if i > np {
		return np
	}
	return i
}

// peerStarts re-derives each position's peer-group START from windowPeerEnds'
// result: positions sharing an end share a group, and the group runs from the
// first such position.
func peerStarts(ends []int) []int {
	starts := make([]int, len(ends))
	i := 0
	for i < len(ends) {
		j := ends[i]
		for k := i; k < j && k < len(ends); k++ {
			starts[k] = i
		}
		if j <= i {
			j = i + 1
		}
		i = j
	}
	return starts
}

// peerGroups numbers the peer groups of an ordered partition, returning each
// position's group index plus every group's [lo, hi) position range.
func peerGroups(starts, ends []int) (gIdx, gLo, gHi []int) {
	gIdx = make([]int, len(ends))
	i := 0
	for i < len(ends) {
		j := ends[i]
		if j <= i {
			j = i + 1
		}
		for k := i; k < j && k < len(ends); k++ {
			gIdx[k] = len(gLo)
		}
		gLo = append(gLo, i)
		gHi = append(gHi, j)
		i = j
	}
	return gIdx, gLo, gHi
}

// frameOffsetReg reads one frame bound's offset out of the register
// planWindowFrameOffsets coded it into (vdbe_window_codegen.go). reg is
// 1-BIASED, so 0 means "this bound carries no offset" and ok is false; the
// bounds test is defensive (AGENTS.md invariant 2), never expected to fire.
func (m *vdbe) frameOffsetReg(reg int) (Value, bool) {
	if reg <= 0 || reg-1 >= len(m.regs) {
		return Value{}, false
	}
	return m.regs[reg-1], true
}

// frameIntOffset resolves a ROWS/GROUPS "<N>" offset. C requires a
// non-negative integer and reports "frame starting offset must be a
// non-negative integer" (or "ending") at run time -- windowCheckValue on
// regStart/regEnd (window.c:2941, 2945) -- so "-1 PRECEDING" parses and fails
// only once rows are fetched. A non-constant offset is loaded as NULL
// (planWindowFrameOffsets), as sqlite3WindowOffsetExpr substitutes at parse
// time (window.c:1163-1170), which fails here with the same message.
func (m *vdbe) frameIntOffset(reg int, isStart bool) (int64, error) {
	v, ok := m.frameOffsetReg(reg)
	if !ok {
		return 0, nil // this bound has no offset
	}
	if num, ok := frameOffsetNumeric(v); ok {
		if n, ok := frameIntValue(num); ok && n >= 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("engine: frame %s offset must be a non-negative integer", frameEndWord(isStart))
}

// frameOffsetNumeric applies the NUMERIC affinity C SQLite applies to a
// frame offset before checking its sign: INTEGER and REAL pass through, TEXT
// converts only if the WHOLE string is a number, and BLOB never converts.
// Verified directly (window1.test section 22 plus probes): "'2.0'", "'1.2'"
// and "'  3 '" are all accepted, while "''", "'2.0x'", NULL, x'1234' -- and
// even x'3132', whose bytes spell "12" -- are "frame starting offset must be a
// non-negative number".
func frameOffsetNumeric(v Value) (Value, bool) {
	switch v.Typ {
	case Int, Float:
		return v, true
	case Text:
		if isFloat, i, f, ok := parseFullNumeric(string(v.S)); ok {
			if isFloat {
				return Value{Typ: Float, F: f}, true
			}
			return Value{Typ: Int, I: i}, true
		}
	}
	return Value{}, false
}

// partitionArgValue reads a window function's i-th argument at the FIRST row
// of the partition -- which is where C SQLite evaluates ntile()'s N:
// "ntile(a) OVER (ORDER BY a)" over a = 1..5 answers 1 for every row, the
// value a has in the partition's first row (verified). The argument was
// computed for EVERY row on the way into the batch, exactly as SQLite computes
// it for every row of its sub-select (window.c:1047); only this one is read.
func (m *vdbe) partitionArgValue(plan *windowPlan, call windowCall, i int, batch, vals [][]Value, part []int) (Value, error) {
	if len(part) == 0 {
		return Value{Typ: Null}, nil
	}
	ri := part[0]
	return m.windowOperand(plan, batch[ri], call.argCol(i))
}

// frameIntValue extracts a whole-number offset from v, accepting the INTEGER
// and the integral-REAL spellings ("ROWS 2.0 PRECEDING") and rejecting
// everything else.
func frameIntValue(v Value) (int64, bool) {
	switch v.Typ {
	case Int:
		return v.I, true
	case Float:
		n := int64(v.F)
		if float64(n) == v.F {
			return n, true
		}
	}
	return 0, false
}

func frameEndWord(isStart bool) string {
	if isStart {
		return "starting"
	}
	return "ending"
}

// rangeFrameSpans computes RANGE frames, which measure bounds against the ORDER
// BY key's value:
//
//   - UNBOUNDED PRECEDING/FOLLOWING are the partition's ends;
//   - CURRENT ROW is the whole peer group, so the default frame gives tied rows
//     the same running total;
//   - "<N> PRECEDING/FOLLOWING" spans rows whose key is within N in the ORDER
//     BY's direction (DESC makes PRECEDING mean larger keys); see
//     rangeKeys.offsetBound.
//
// The offset arithmetic uses this engine's "+"/"-", inheriting numeric
// affinity; a non-numeric offset is "frame starting offset must be a
// non-negative number" (number, not integer as for ROWS/GROUPS).
func (m *vdbe) rangeFrameSpans(plan *windowPlan, call windowCall, part, starts, ends []int, batch, vals [][]Value, spans []windowFrameSpan) error {
	spec := call.spec
	np := len(part)
	f := spec.Frame
	needKeys := f.Start.Offset != nil || f.End.Offset != nil
	if !needKeys {
		for k := range spans {
			spans[k].lo = rangePeerBound(f.Start.Type, starts[k], ends[k], np, true)
			spans[k].hi = rangePeerBound(f.End.Type, starts[k], ends[k], np, false)
		}
		return nil
	}

	// Exactly one ORDER BY term is guaranteed by checkWindowFrame.
	ot := spec.OrderBy[0]
	r := rangeKeys{keys: make([]Value, np), desc: ot.Desc, np: np, enc: m.encoding()}
	if names := m.windowKeyCollations(plan, []Expr{ot.Expr}, batch, vals); len(names) > 0 {
		r.coll = names[0]
	}
	for i, ri := range part {
		v, err := m.windowOperand(plan, batch[ri], call.orderCol(0))
		if err != nil {
			return err
		}
		r.keys[i] = v
	}
	// The NULL-keyed rows form ONE contiguous block, at the front or the back
	// depending on the term's effective NULLS placement (OrderTerm.NullsFirst,
	// which partitionAndOrder already sorted by), so trimming NULLs off both
	// ends leaves exactly the non-NULL run -- whichever end they landed on,
	// and an empty run for an all-NULL partition.
	r.nnLo, r.nnHi = 0, np
	for r.nnLo < r.nnHi && r.keys[r.nnLo].Typ == Null {
		r.nnLo++
	}
	for r.nnHi > r.nnLo && r.keys[r.nnHi-1].Typ == Null {
		r.nnHi--
	}

	startOff, err := m.frameRangeOffset(call.startOffReg, true)
	if err != nil {
		return err
	}
	endOff, err := m.frameRangeOffset(call.endOffReg, false)
	if err != nil {
		return err
	}
	start, err := r.newBound(f.Start.Type, startOff)
	if err != nil {
		return err
	}
	end, err := r.newBound(f.End.Type, endOff)
	if err != nil {
		return err
	}
	for k := range spans {
		lo, lerr := r.bound(start, k, spans[k], true)
		if lerr != nil {
			return lerr
		}
		hi, herr := r.bound(end, k, spans[k], false)
		if herr != nil {
			return herr
		}
		spans[k].lo, spans[k].hi = lo, hi
	}
	return nil
}

// rangeBound is one RANGE bound prepared for a whole partition: its kind, its
// offset, and -- for a PRECEDING bound, whose test shifts the CANDIDATE key
// (see rangeKeys.offsetBound) -- every candidate's shifted key, computed ONCE
// here rather than once per (row, candidate) pair. A FOLLOWING bound shifts the
// CURRENT key instead, which is per row and so stays in offsetBound.
type rangeBound struct {
	typ     FrameBoundType
	off     Value
	shifted []Value // FramePreceding only; nil otherwise
}

func (r rangeKeys) newBound(t FrameBoundType, off Value) (rangeBound, error) {
	b := rangeBound{typ: t, off: off}
	if t != FramePreceding {
		return b, nil
	}
	b.shifted = make([]Value, len(r.keys))
	for j, key := range r.keys {
		// A TEXT/BLOB candidate is left alone -- C SQLite skips the
		// arithmetic for one, and coercing it to a number here would drag it
		// onto the numeric line.
		if !isRangeNumeric(key) {
			b.shifted[j] = key
			continue
		}
		v, err := evalArith(r.op(), key, off)
		if err != nil {
			return b, err
		}
		b.shifted[j] = v
	}
	return b, nil
}

// op is the arithmetic a bound's offset applies in this ordering's direction:
// "+" walks toward the END of an ASC partition, "-" toward the end of a DESC
// one.
func (r rangeKeys) op() string {
	if r.desc {
		return "-"
	}
	return "+"
}

// rangeKeys is one partition's ORDER BY key column, as RANGE framing sees it:
// the keys in window order, the term's collation and direction, and the
// [nnLo, nnHi) run of NON-NULL keys within it.
type rangeKeys struct {
	keys []Value
	coll string
	// enc is the database's text encoding: BINARY (and RTRIM) comparison is
	// memcmp over the ENCODED bytes, so a RANGE frame's peer-group boundaries
	// over a TEXT key depend on it exactly as ORDER BY does (utf16.go).
	enc        TextEncoding
	desc       bool
	nnLo, nnHi int
	np         int
}

// isRangeNumeric reports whether v is a number for RANGE-framing purposes.
// It is the value's TYPE that decides, never whether a string could be read as
// one: with keys 1, 2.5, '3', x'34' and NULL, "RANGE BETWEEN 10 PRECEDING AND
// 10 FOLLOWING" pairs 1 with 2.5 and leaves '3' framing alone (verified).
func isRangeNumeric(v Value) bool { return v.Typ == Int || v.Typ == Float }

// rangePeerBound resolves an offset-free RANGE bound: UNBOUNDED at the
// partition's ends, CURRENT ROW at the current peer group's. An offset bound
// whose key is non-numeric lands on the same answer by a different route --
// see rangeKeys.offsetBound.
func rangePeerBound(t FrameBoundType, peerLo, peerHi, np int, isStart bool) int {
	switch t {
	case FrameUnboundedPreceding:
		return 0
	case FrameUnboundedFollowing:
		return np
	default: // FrameCurrentRow (an offset bound never reaches here)
		if isStart {
			return peerLo
		}
		return peerHi
	}
}

// bound resolves one RANGE bound of position k to a frame edge: an inclusive
// index for a start bound, an exclusive one for an end bound. UNBOUNDED is the
// partition's edge and CURRENT ROW the peer group's; an offset bound is
// offsetBound's job.
func (r rangeKeys) bound(b rangeBound, k int, peers windowFrameSpan, isStart bool) (int, error) {
	if b.typ != FramePreceding && b.typ != FrameFollowing {
		return rangePeerBound(b.typ, peers.peerLo, peers.peerHi, r.np, isStart), nil
	}
	return r.offsetBound(b, k, peers, isStart)
}

// offsetBound resolves an "<N> PRECEDING/FOLLOWING" RANGE bound by scanning for
// the first (start) or one-past-last (end) row whose key passes the bound's
// test against key[k]. ponytail: a linear scan per row, O(n^2) per partition
// like the accumulator; binary search if window queries get hot.
//
// Which side the offset is added to is observable. C always shifts the value on
// the PRECEDING side (ASC; DESC flips the sign and the comparison):
//
//	<N> PRECEDING:  key[j] + N  <cmp>  key[k]
//	<N> FOLLOWING:  key[k] + N  <cmp>  key[j]
//
// In double arithmetic that differs from shifting key[k] by -N: over REAL keys
// one ulp apart at 1e16, "RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING" frames
// 1e16 with 9999999999999998 (which +1 rounds up to 1e16) but not with
// 1.0000000000000002e16 (1e16+1 rounds down). window.c adds the offset to the
// boundary cursor for PRECEDING and the current-row cursor for FOLLOWING.
//
// Two rules for non-numeric keys:
//
//  1. A TEXT, BLOB or NULL key has no distance, so its offset bound degenerates
//     to its own peer boundary: an offset-offset frame is exactly the peer
//     group, never empty, even for an inverted "1 PRECEDING AND 2 PRECEDING"
//     (window1.test section 19). It is per row and by type: with keys 1, 2.5,
//     '3', x'34', NULL, "RANGE BETWEEN 10 PRECEDING AND 10 FOLLOWING" pairs 1
//     with 2.5 and leaves '3' alone.
//  2. NULL-keyed rows are outside an offset bound's reach: the scan runs over
//     [nnLo, nnHi) and clamps to that run's edge. Visible only with explicit
//     NULLS FIRST/LAST: under NULLS LAST, "UNBOUNDED PRECEDING AND 10
//     FOLLOWING" does not sweep the trailing NULL block into numeric rows'
//     frames, and "100 FOLLOWING AND UNBOUNDED FOLLOWING" starts at nnHi and
//     frames exactly the NULL block. UNBOUNDED bounds always cover it.
func (r rangeKeys) offsetBound(b rangeBound, k int, peers windowFrameSpan, isStart bool) (int, error) {
	key := r.keys[k]
	if !isRangeNumeric(key) {
		if isStart {
			return peers.peerLo, nil
		}
		return peers.peerHi, nil
	}
	// A FOLLOWING bound compares each raw candidate against the SHIFTED current
	// key; a PRECEDING bound compares each SHIFTED candidate (newBound already
	// shifted them all) against the raw current key.
	lhs, rhs := b.shifted, key
	if b.typ == FrameFollowing {
		v, err := evalArith(r.op(), key, b.off)
		if err != nil {
			return 0, err
		}
		lhs, rhs = r.keys, v
	}
	// inFrame reports whether candidate j is on the frame's side of the
	// comparison. For an ASC order a start bound admits candidates at or after
	// the boundary and an end bound those at or before it; DESC reverses both.
	inFrame := func(j int) bool {
		c := compareValuesCollatedEnc(lhs[j], rhs, r.coll, r.enc)
		if r.desc {
			c = -c
		}
		if isStart {
			return c >= 0
		}
		return c <= 0
	}
	// A PRECEDING start bound can never begin AFTER the current peer group, nor
	// a FOLLOWING end bound stop BEFORE it: C SQLite walks those boundaries
	// out from the current row and simply never moves them past it. With exact
	// arithmetic the clamp is unreachable (key+N >= key for N >= 0), but
	// ROUNDING reaches it -- over INTEGER keys at the int64 extremes,
	// "ORDER BY a RANGE BETWEEN 1.5 PRECEDING AND 0.5 FOLLOWING" frames
	// -9223372036854775807 with ITSELF in C SQLite even though both shifted
	// limits round to -9223372036854775808 and so exclude it, which without the
	// clamp is an EMPTY frame here (verified: cgo answers the row, this engine
	// answered NULL until this clamp existed).
	if isStart {
		for j := r.nnLo; j < r.nnHi; j++ {
			if inFrame(j) {
				return r.clampStart(b.typ, j, peers), nil
			}
		}
		return r.clampStart(b.typ, r.nnHi, peers), nil
	}
	for j := r.nnHi - 1; j >= r.nnLo; j-- {
		if inFrame(j) {
			return r.clampEnd(b.typ, j+1, peers), nil
		}
	}
	return r.clampEnd(b.typ, r.nnLo, peers), nil
}

func (r rangeKeys) clampStart(t FrameBoundType, lo int, peers windowFrameSpan) int {
	if t == FramePreceding && lo > peers.peerLo {
		return peers.peerLo
	}
	return lo
}

func (r rangeKeys) clampEnd(t FrameBoundType, hi int, peers windowFrameSpan) int {
	if t == FrameFollowing && hi < peers.peerHi {
		return peers.peerHi
	}
	return hi
}

// frameRangeOffset evaluates a RANGE offset, which must be a non-negative
// number ("frame starting offset must be a non-negative number").
//
// An infinite offset ("9e999 PRECEDING") declines: C accepts it, but "key +/-
// inf" is NaN for an infinite key and C's boundary cursors stop mid-scan,
// giving answers (counts of 6 over 5 rows) no span can express. A finite
// offset over infinite keys is supported.
func (m *vdbe) frameRangeOffset(reg int, isStart bool) (Value, error) {
	v, ok := m.frameOffsetReg(reg)
	if !ok {
		return Value{Typ: Null}, nil // this bound has no offset
	}
	if num, ok := frameOffsetNumeric(v); ok {
		switch {
		case num.Typ == Float && math.IsInf(num.F, 1):
			return Value{}, fmt.Errorf("%w: RANGE frame with an infinite offset", errVDBEUnsupported)
		case num.Typ == Int && num.I >= 0, num.Typ == Float && num.F >= 0:
			return num, nil
		}
	}
	return Value{}, fmt.Errorf("engine: frame %s offset must be a non-negative number", frameEndWord(isStart))
}

// ---- the positional window functions ----
//
// ntile, lead and lag work off the PARTITION's own ordering and ignore the
// frame entirely; first_value, last_value and nth_value read the FRAME. All
// six were verified directly against mattn/go-sqlite3 over the file's own
// (1,A)..(7,F) fixture -- see each function's own note.

// windowNtile fills ntile(N): the partition split into N groups as evenly as
// possible with the LARGER groups first, each row taking its group's 1-based
// number. Verified: over 6 rows, ntile(3) -> 1 1 2 2 3 3, ntile(4) -> 1 1 2 2
// 3 4 (sizes 2,2,1,1), and ntile(10) -> 1..6 (a group per row, never an empty
// one). N must be a positive integer -- ntile(0) and ntile(NULL) are both real
// SQLite's "argument of ntile must be a positive integer".
func (m *vdbe) windowNtile(plan *windowPlan, call windowCall, ci int, part []int, batch, vals [][]Value) error {
	if len(part) == 0 {
		return nil
	}
	v, err := m.partitionArgValue(plan, call, 0, batch, vals, part)
	if err != nil {
		return err
	}
	n, ok := frameIntValue(v)
	if !ok || n < 1 {
		return fmt.Errorf("engine: argument of ntile must be a positive integer")
	}
	np := int64(len(part))
	if n > np {
		n = np
	}
	base, extra := np/n, np%n
	pos := int64(0)
	for g := int64(0); g < n; g++ {
		size := base
		if g < extra {
			size++ // the first "extra" groups take the leftover row
		}
		for i := int64(0); i < size; i++ {
			vals[part[pos]][ci] = Value{Typ: Int, I: g + 1}
			pos++
		}
	}
	return nil
}

// windowLeadLag fills lead/lag(expr [, offset [, default]]): the value of expr
// at the row offset positions after (lead) or before (lag) the current one
// WITHIN THE PARTITION, or the default (NULL when omitted) when that lands
// outside it. The frame is ignored. Verified: lead(b) -> B C D E F NULL,
// lag(b,2,'z') -> z z A B C D, and an offset of 0 yields the row's own value
// while a NEGATIVE offset simply reverses the direction (lead(b,-1) == lag(b)).
func (m *vdbe) windowLeadLag(plan *windowPlan, call windowCall, ci int, part []int, batch, vals [][]Value) error {
	dir := int64(1)
	if call.name == "lag" {
		dir = -1
	}
	for k, ri := range part {
		// The OFFSET and the DEFAULT are evaluated at the CURRENT row, not
		// once for the partition: "lead(a,a) OVER (ORDER BY a)" over a = 1..5
		// answers 2, 4, NULL, NULL, NULL -- each row using its own a as the
		// offset -- and "lag(a,1,a*100)" answers 100 for the first row
		// (verified directly).
		off := int64(1)
		if len(call.args) > 1 {
			v, err := m.windowOperand(plan, batch[ri], call.argCol(1))
			if err != nil {
				return err
			}
			n, ok := frameIntValue(v)
			if !ok {
				return fmt.Errorf("engine: second argument to %s must be an integer", call.name)
			}
			off = n
		}
		j := int64(k) + dir*off
		if j >= 0 && j < int64(len(part)) {
			src := part[j]
			v, err := m.windowOperand(plan, batch[src], call.argCol(0))
			if err != nil {
				return err
			}
			vals[ri][ci] = v
			continue
		}
		def := Value{Typ: Null}
		if len(call.args) > 2 {
			v, err := m.windowOperand(plan, batch[ri], call.argCol(2))
			if err != nil {
				return err
			}
			def = v
		}
		vals[ri][ci] = def
	}
	return nil
}

// windowFrameValue fills first_value/last_value/nth_value: expr evaluated at
// the first, last, or N-th (1-based) row of the row's own FRAME, honouring any
// EXCLUDE clause, and NULL when the frame holds too few rows. Verified:
// last_value(b) OVER (ORDER BY a) -> A B D D E F (the default frame runs
// through the last PEER, so both rows of the a=3 tie see D), and
// nth_value(b,2) OVER (... UNBOUNDED PRECEDING AND CURRENT ROW) -> NULL B B B
// B B. nth_value's N must be a positive integer: C SQLite's "second
// argument to nth_value must be a positive integer".
func (m *vdbe) windowFrameValue(plan *windowPlan, call windowCall, ci int, part []int, spans []windowFrameSpan, batch, vals [][]Value) error {
	for k, ri := range part {
		// nth_value's N is evaluated at the CURRENT row, not once per
		// partition: "nth_value(b,b+1) OVER (ORDER BY b,a RANGE BETWEEN
		// UNBOUNDED PRECEDING AND CURRENT ROW)" walks up with b (verified --
		// window3.test, which is what caught this).
		nth := int64(1)
		if call.name == "nth_value" {
			v, err := m.windowOperand(plan, batch[ri], call.argCol(1))
			if err != nil {
				return err
			}
			n, ok := frameIntValue(v)
			if !ok || n < 1 {
				return fmt.Errorf("engine: second argument to nth_value must be a positive integer")
			}
			nth = n
		}
		want := nth
		if call.name == "last_value" {
			want = 0 // resolved below by taking the final included row
		}
		var chosen = -1
		seen := int64(0)
		for j := spans[k].lo; j < spans[k].hi; j++ {
			if !frameRowIncluded(spans[k], call.spec.Frame, k, j) {
				continue
			}
			seen++
			if want == 0 {
				chosen = j // keep overwriting: the last one wins
				continue
			}
			if seen == want {
				chosen = j
				break
			}
		}
		if chosen < 0 {
			vals[ri][ci] = Value{Typ: Null}
			continue
		}
		src := part[chosen]
		v, err := m.windowOperand(plan, batch[src], call.argCol(0))
		if err != nil {
			return err
		}
		vals[ri][ci] = v
	}
	return nil
}
