package engine

import (
	"sync"
	"sync/atomic"

	"github.com/samyfodil/musql/internal/jit"
)

// Running a compiled loop body over the columnar blocks.
//
// One mapped kernel per lowered program, cached for the process like the fixed
// ones: a program is a few hundred bytes and the same statement recompiles to
// the identical IR every time.

var segProgCache sync.Map // *segLowered -> *jit.Code

// segProgKernel returns the machine code for this lowered body, emitting it on
// first use. nil when the platform or the program declines.
func segProgKernel(low *segLowered) *jit.Code {
	if c, ok := segProgCache.Load(low); ok {
		if c == nil {
			return nil
		}
		return c.(*jit.Code)
	}
	code, err := jit.EmitProgram(low.insns, len(low.cols))
	if err != nil {
		segProgCache.Store(low, (*jit.Code)(nil))
		return nil
	}
	k, err := jit.Map(code)
	if err != nil {
		segProgCache.Store(low, (*jit.Code)(nil))
		return nil
	}
	actual, _ := segProgCache.LoadOrStore(low, k)
	if got := actual.(*jit.Code); got != k {
		k.Close()
		return got
	}
	return k
}

// segRunProgramAll answers every aggregate the statement computes from the
// segments, or reports false to decline -- in which case the loop the
// recogniser left in the program runs.
//
// One lowered program per aggregate, all sharing the same body and differing
// only in the accumulate at the end, so N aggregates are N passes over the same
// int64 blocks. Two passes over a column beat one pass through the VDBE by
// enough that a shared-accumulator IR is not worth the complexity: the whole
// point of "SELECT min(v), max(v) FROM t" being here is that it measured 4.49x
// SLOWER than C SQLite while every single-aggregate form measured faster.
func (m *vdbe) segRunProgramAll(p *ReadOnlyPager, rootPage uint32, plan *segProgPlan, ipkCol int) ([]Value, bool) {
	if plan == nil || len(plan.lows) == 0 {
		return nil, false
	}
	out := make([]Value, len(plan.lows))
	for i, low := range plan.lows {
		v, ok := m.segRunOne(p, rootPage, low, ipkCol)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

func (m *vdbe) segRunOne(p *ReadOnlyPager, rootPage uint32, low *segLowered, ipkCol int) (Value, bool) {
	// A row-major delta beside the segments makes a raw block read wrong; see
	// segCleanFor.
	if !p.segCleanFor(rootPage) {
		return Value{}, false
	}
	if p == nil || p.segs == nil || low == nil {
		return Value{}, false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return Value{}, false
	}
	kern := segProgKernel(low)
	if kern == nil {
		return Value{}, false
	}

	regs := make([]int64, low.nRegs+2)
	// Bound parameters go into the registers the body reads them from. A
	// non-integer bound declines: every value in a compiled program is an
	// int64, and coercing here is exactly where SQLite's cross-class ordering
	// would be got wrong.
	for i, reg := range low.consts {
		idx := low.cvIndex[i]
		if idx < 1 || idx > len(m.params) {
			return Value{}, false
		}
		v := m.params[idx-1]
		if v.Typ != Int {
			return Value{}, false
		}
		if reg >= len(regs) {
			return Value{}, false
		}
		regs[reg] = v.I
	}

	var total int64
	matched := int64(0)
	for _, s := range segs {
		blocks, isRowid, okCols := segProgColumns(s, low.cols, low.nullCol, ipkCol)
		if !okCols {
			return Value{}, false
		}
		// A rowid-alias column has no block of its own -- the values live in
		// the segment's rowid block -- and the program reads a flat int64
		// array, so one is materialized here. It is the only allocation on
		// this path and only for a query that names the INTEGER PRIMARY KEY.
		for i := range blocks {
			if !isRowid[i] {
				continue
			}
			buf := make([]int64, s.nRows)
			for r := 0; r < s.nRows; r++ {
				buf[r] = int64(s.Rowid(r))
			}
			blocks[i] = buf
		}
		if s.nRows == 0 {
			continue
		}
		var out, ovf int64
		// min()/max() of a whole column, with no predicate: the zone map
		// already holds the segment's extreme, so no row is read.
		if extreme, ok := segZoneExtreme(s, low); ok {
			out = extreme
			regs[low.rowsReg] += int64(s.nRows)
		} else {
			args := &jit.ProgArgs{N: int64(s.nRows), Regs: &regs[0], Out: &out, Overflow: &ovf}
			for i, b := range blocks {
				if len(b) < s.nRows {
					return Value{}, false
				}
				args.Col[i] = &b[0]
			}
			kern.Call2(args)
		}
		if ovf != 0 {
			// sum() overflowed int64. SQLite switches to floating point there
			// and this cannot, so the whole statement goes back to the loop.
			return Value{}, false
		}
		switch low.agg {
		case aggCountStar, aggCount:
			total += out
			matched += out
		case aggMin, aggMax:
			// Combine ACROSS segments: each call answered over its own rows and
			// started from its own seed, so a segment that matched nothing must
			// not vote.
			n := regs[low.rowsReg]
			regs[low.rowsReg] = 0
			if n == 0 {
				continue
			}
			if matched == 0 || (low.agg == aggMin && out < total) ||
				(low.agg == aggMax && out > total) {
				total = out
			}
			matched += n
		default:
			sum := total + out
			if (out > 0 && sum < total) || (out < 0 && sum > total) {
				return Value{}, false
			}
			total = sum
			matched += regs[low.rowsReg]
			regs[low.rowsReg] = 0
		}
	}
	// func.c's finalizers, for the all-integer non-NULL case this path is the
	// only one that reaches: sumFinalize (2003) returns the int64 or NULL at
	// cnt==0, avgFinalize (2020) returns (double)iSum/(double)cnt or NULL at
	// cnt==0, totalFinalize (2034) returns (double)iSum and 0.0 at no rows at
	// all -- which is the one that does NOT go NULL.
	switch low.agg {
	case aggMin, aggMax:
		// minmaxFinalize (func.c:2107) returns NULL for no rows, like sum.
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Int, I: total}, true
	case aggSum:
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Int, I: total}, true
	case aggAvg:
		if matched == 0 {
			return Value{Typ: Null}, true
		}
		return Value{Typ: Float, F: float64(total) / float64(matched)}, true
	case aggTotal:
		return Value{Typ: Float, F: float64(total)}, true
	}
	return Value{Typ: Int, I: total}, true
}

// segZoneExtremeHits counts segments answered from a zone map, for tests.
var segZoneExtremeHits atomic.Int64

// segZoneExtreme answers a predicate-free min(col) or max(col) over one segment
// from its zone map. The body must hold nothing but loads of its one column and
// copies between registers, with no bound parameters, so every register it
// writes -- the accumulated one included -- holds that column's value.
func segZoneExtreme(s *segment, low *segLowered) (int64, bool) {
	n := len(low.insns)
	if (low.agg != aggMin && low.agg != aggMax) || len(low.cols) != 1 || low.nullCol[0] ||
		len(low.consts) != 0 || n < 2 {
		return 0, false
	}
	holds := map[int]bool{} // registers holding the column's value
	for _, in := range low.insns[:n-1] {
		switch {
		case in.Op == jit.POpLoadCol && in.B == 0:
			holds[in.A] = true
		case in.Op == jit.POpLoadReg && holds[in.B]:
			holds[in.A] = true
		default:
			return 0, false
		}
	}
	acc := low.insns[n-1]
	if (acc.Op != jit.POpAccMin && acc.Op != jit.POpAccMax) || !holds[acc.A] {
		return 0, false
	}
	z, ok := s.intZones(low.cols[0])
	if !ok {
		return 0, false
	}
	segZoneExtremeHits.Add(1)
	if low.agg == aggMin {
		return z.min, true
	}
	return z.max, true
}
