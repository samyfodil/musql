package engine

import (
	"math"
	"slices"

	"github.com/samyfodil/musql/internal/jit"
)

// The compiled program over generate_series. A loop that reads a series
// cursor runs segProgPeephole's program like a loop over a table: the
// series' values are written into a block, as the cursor would step through
// them, and each block is one kernel call, combined in row order as a
// table's segments are.

// seriesBlockRows is the values one kernel call reads.
const seriesBlockRows = 1 << 16

// segSeriesOpen reports whether in opens a cursor that streams a virtual
// table, which only generate_series does (vtab_stream.go).
func segSeriesOpen(in Instruction) bool {
	ds, ok := in.P4.(*derivedSource)
	return in.Op == OpOpenDerived && ok && ds.vtabStream && ds.vtab != nil && ds.prog == nil
}

// segRunSeries is segRunProgramAll over the series cur streams, filtered as
// its rewind would filter it, or false when cur is not one or a program
// cannot run over it.
func (m *vdbe) segRunSeries(cur *vdbeCursor, plan *segProgPlan) ([]Value, bool) {
	sr, ok := cur.seriesScan()
	if !ok || len(plan.lows) == 0 {
		return nil, false
	}
	out := make([]Value, len(plan.lows))
	for i, low := range plan.lows {
		v, ok := m.segRunSeriesOne(cur, sr, low)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// seriesScan is the filtered series cur streams, started or filtered again
// as rewindVtab would.
func (cur *vdbeCursor) seriesScan() (*seriesCursor, bool) {
	s := cur.vt
	if s == nil {
		return nil, false
	}
	if s.scan == nil {
		sc, err := s.start()
		if err != nil || sc == nil {
			return nil, false
		}
		s.scan = sc
	} else if err := s.scan.cur.Filter(s.scan.idxNum, s.scan.idxStr, s.scan.argv); err != nil {
		return nil, false
	}
	sr, ok := s.scan.cur.(*seriesCursor)
	return sr, ok
}

func (m *vdbe) segRunSeriesOne(cur *vdbeCursor, sr *seriesCursor, low *segLowered) (Value, bool) {
	kern := segProgKernel(low)
	if kern == nil {
		return Value{}, false
	}
	regs, ok := m.segProgRegs(low)
	if !ok {
		return Value{}, false
	}
	var facc [2]float64
	switch low.agg {
	case aggMin:
		facc[0] = math.Inf(1)
	case aggMax:
		facc[0] = math.Inf(-1)
	}
	// The value column is written block by block; start, stop and step are
	// the same on every row.
	bufN := min(sr.count, seriesBlockRows)
	values := make([]int64, bufN)
	blocks := make([][]int64, len(low.cols))
	for i, c := range low.cols {
		switch c {
		case seriesColValue:
			blocks[i] = values
		case seriesColStart, seriesColStop, seriesColStep:
			blocks[i] = slices.Repeat([]int64{[...]int64{0, sr.start, sr.stop, sr.step}[c]}, int(bufN))
		default:
			return Value{}, false
		}
	}
	first := sr.value
	var svc *segSvcRunner
	if len(low.blocks) > 0 {
		svc = newSegSvcRunner(m, cur.tbl)
		svc.column = func(row, col int) Value {
			v := [...]int64{first + int64(row)*sr.step, sr.start, sr.stop, sr.step}
			if col < 0 || col >= len(v) {
				return Value{Typ: Null}
			}
			return Value{Typ: Int, I: v[col]}
		}
	}
	results := make([]segRunResult, 0, sr.count/seriesBlockRows+1)
	v := sr.value
	for done := uint64(0); done < sr.count; {
		n := min(sr.count-done, seriesBlockRows)
		first = v
		for i := range values[:n] {
			values[i] = v
			v += sr.step
		}
		local := slices.Clone(regs)
		var out, ovf int64
		args := &jit.ProgArgs{N: int64(n), Regs: &local[0], Out: &out, Overflow: &ovf, FAcc: facc}
		for i, b := range blocks {
			args.Col[i] = &b[0]
		}
		if !segCallKernel(kern, args, svc, low.blocks, nil, local) {
			return Value{}, false
		}
		facc = args.FAcc
		res := segRunResult{out: out, ovf: ovf != 0}
		if low.agg != aggCountStar && low.agg != aggCount {
			res.rows = local[low.rowsReg]
		}
		results = append(results, res)
		done += n
	}
	return segProgAnswer(low, results, facc)
}
