package engine

import (
	"math"
	"math/bits"
)

// Zone maps: the minimum and maximum of an int64 column over each run of
// segFilterBatch rows, and over the whole segment. Built on first use and kept
// with the segment, like the NULL indicators (segment_nullidx.go); nothing is
// written to the file, so a file built by any version reads the same.
//
// They answer two questions without touching the rows: can any row of this run
// satisfy a predicate (skip it), and must every row (take it whole). And the
// segment-wide range bounds what a sum over the column can reach.
type segZones struct {
	min, max   int64
	zmin, zmax []int64 // per run of segFilterBatch rows
}

// intZones returns col's zones, or false when col is not a clean int64 block.
func (s *segment) intZones(col int) (*segZones, bool) {
	s.zoneMu.Lock()
	defer s.zoneMu.Unlock()
	if z, ok := s.zones[col]; ok {
		return z, z != nil
	}
	if s.zones == nil {
		s.zones = map[int]*segZones{}
	}
	vals, ok := segCleanInt64Column(s, col)
	if !ok || len(vals) < s.nRows {
		s.zones[col] = nil
		return nil, false
	}
	z := &segZones{min: math.MaxInt64, max: math.MinInt64}
	for lo := 0; lo < s.nRows; lo += segFilterBatch {
		hi := min(lo+segFilterBatch, s.nRows)
		mn, mx := vals[lo], vals[lo]
		for _, v := range vals[lo+1 : hi] {
			mn, mx = min(mn, v), max(mx, v)
		}
		z.zmin, z.zmax = append(z.zmin, mn), append(z.zmax, mx)
		z.min, z.max = min(z.min, mn), max(z.max, mx)
	}
	s.zones[col] = z
	return z, true
}

// zoneVerdict says what a run's range means for p: segZoneNone (no row can
// match), segZoneAll (every row does) or segZoneSome.
const (
	segZoneSome = iota
	segZoneNone
	segZoneAll
)

func segZoneVerdict(p segPred, mn, mx int64) int {
	x := p.Val.I
	switch p.Op {
	case segGT:
		if mx <= x {
			return segZoneNone
		}
		if mn > x {
			return segZoneAll
		}
	case segGE:
		if mx < x {
			return segZoneNone
		}
		if mn >= x {
			return segZoneAll
		}
	case segLT:
		if mn >= x {
			return segZoneNone
		}
		if mx < x {
			return segZoneAll
		}
	case segLE:
		if mn > x {
			return segZoneNone
		}
		if mx <= x {
			return segZoneAll
		}
	case segEQ:
		if x < mn || x > mx {
			return segZoneNone
		}
		if mn == x && mx == x {
			return segZoneAll
		}
	case segNE:
		if mn == x && mx == x {
			return segZoneNone
		}
		if x < mn || x > mx {
			return segZoneAll
		}
	}
	return segZoneSome
}

// segSumCannotOverflow reports whether n values within [mn, mx] sum without any
// partial sum overflowing an int64, in any order: n * max(|mn|, |mx|) fits.
func segSumCannotOverflow(n int, mn, mx int64) bool {
	abs := func(v int64) uint64 {
		if v < 0 {
			return uint64(-(v + 1)) + 1
		}
		return uint64(v)
	}
	hi, lo := bits.Mul64(uint64(n), max(abs(mn), abs(mx)))
	return hi == 0 && lo <= math.MaxInt64
}

// segFilterSumZoned is one segment's sum(col) over rows satisfying preds, all
// integer comparisons on clean int64 columns, when the column's range proves
// the sum cannot overflow. It reports false when that proof or a zone is not
// available; the checked loop answers then.
func segFilterSumZoned(s *segment, preds []segPred, col int) (sum int64, matched int, ok bool) {
	vals, okv := segCleanInt64Column(s, col)
	vz, okz := s.intZones(col)
	if !okv || !okz || !segSumCannotOverflow(s.nRows, vz.min, vz.max) {
		return 0, 0, false
	}
	zones := make([]*segZones, len(preds))
	for i, p := range preds {
		if p.Val.Typ != Int {
			return 0, 0, false
		}
		if zones[i], okz = s.intZones(p.Col); !okz {
			return 0, 0, false
		}
	}
	var flags [segFilterBatch]uint8
	for b, lo := 0, 0; lo < s.nRows; b, lo = b+1, lo+segFilterBatch {
		hi := min(lo+segFilterBatch, s.nRows)
		all := true
		skip := false
		for i, p := range preds {
			switch segZoneVerdict(p, zones[i].zmin[b], zones[i].zmax[b]) {
			case segZoneNone:
				skip = true
			case segZoneSome:
				all = false
			}
		}
		if skip {
			continue
		}
		if all {
			for _, v := range vals[lo:hi] {
				sum += v
			}
			matched += hi - lo
			continue
		}
		live := flags[:hi-lo]
		for i := range live {
			live[i] = 1
		}
		for _, p := range preds {
			segApplyPred(s, p, lo, live)
		}
		for i, v := range vals[lo:hi] {
			m := int64(live[i])
			sum += v & -m
			matched += int(m)
		}
	}
	return sum, matched, true
}
