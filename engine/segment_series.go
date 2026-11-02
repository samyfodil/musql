package engine

import (
	"math"
	"slices"
	"strings"

	"github.com/samyfodil/musql/internal/jit"
)

// The compiled program over generate_series. A loop that reads a series
// cursor runs segProgPeephole's program like a loop over a table: the
// series' values are written into a block, as the cursor would step through
// them, and each block is one kernel call, combined in row order as a
// table's segments are. A loop over one series inside a loop over another
// (segSeriesPairPeephole) reads their product the same way, the inner
// series' columns numbered after the outer's.

// seriesBlockRows is the rows one kernel call reads.
const seriesBlockRows = 1 << 16

// seriesCols is a series' columns, seriesColValue to seriesColStep.
const seriesCols = 4

// segSeriesOpen reports whether in opens a cursor that streams a virtual
// table, which only generate_series does (vtab_stream.go).
func segSeriesOpen(in Instruction) bool {
	ds, ok := in.P4.(*derivedSource)
	return in.Op == OpOpenDerived && ok && ds.vtabStream && isSeriesSource(ds) && ds.prog == nil
}

// isSeriesSource reports whether ds is generate_series: the one table-valued
// function a series program reads (seriesOf); any other would only add a
// guard that never serves.
func isSeriesSource(ds *derivedSource) bool {
	return ds.vtab != nil && strings.EqualFold(ds.vtab.Table, "generate_series")
}

// segSeriesInner reports whether in opens a table-valued function a loop
// may read inside another without streaming it: one whose arguments do not
// depend on the outer row, so it is the same series at every rewind.
func segSeriesInner(in Instruction) bool {
	ds, ok := in.P4.(*derivedSource)
	return in.Op == OpOpenDerived && ok && isSeriesSource(ds) && ds.prog == nil && len(ds.vtabCorr) == 0 && !ds.rightJoin
}

// segSeriesPairPeephole is segProgPeephole for a loop over one series inside
// a loop over another, the inner one's bounds not depending on the outer:
//
//	Init; AggReset; OpenDerived A; OpenDerived B; Rewind A; Rewind B;
//	<body>; AggStep; Next B; Next A; Close B; Close A; AggResult...;
//	ResultRow; Halt
//
// B's columns are read as A's past A's own, so the body lowers as a loop
// over one source.
func segSeriesPairPeephole(prog *Program) bool {
	if !jitEnabled || !jit.Available || prog == nil {
		return false
	}
	in := prog.Insns
	end := len(in)
	if end < 14 || in[0].Op != OpInit || in[1].Op != OpAggReset || !segSeriesOpen(in[2]) || !segSeriesInner(in[3]) ||
		in[end-1].Op != OpHalt || in[end-2].Op != OpResultRow {
		return false
	}
	outer, inner := in[2].P1, in[3].P1
	if outer == inner {
		return false
	}
	nAgg := 0
	for i := end - 3; i >= 0 && in[i].Op == OpAggResult; i-- {
		nAgg++
	}
	if nAgg == 0 || in[end-2].P2 != nAgg || !segAggOutputsAreBare(segAggPlanOf(in[end-2-nAgg])) {
		return false
	}
	const rewindA, rewindB = 4, 5
	closeA := end - 3 - nAgg
	closeB, nextA, nextB, aggAt := closeA-1, closeA-2, closeA-3, closeA-4
	if aggAt <= rewindB || in[rewindA].Op != OpRewind || in[rewindA].P1 != outer || in[rewindA].P2 != closeB ||
		in[rewindB].Op != OpRewind || in[rewindB].P1 != inner || in[rewindB].P2 != nextA ||
		in[nextB].Op != OpNext || in[nextB].P1 != inner || in[nextB].P2 != rewindB+1 ||
		in[nextA].Op != OpNext || in[nextA].P1 != outer || in[nextA].P2 != rewindB ||
		in[closeB].Op != OpClose || in[closeB].P1 != inner || in[closeA].Op != OpClose || in[closeA].P1 != outer ||
		in[aggAt].Op != OpAggStep || in[aggAt].P3 != 0 {
		return false
	}
	dstBase := in[closeA+1].P1
	if in[end-2].P1 != dstBase {
		return false
	}
	for i := range nAgg {
		if in[closeA+1+i].P1 != dstBase+i {
			return false
		}
	}
	plan, ok := in[aggAt].P4.(*aggPlan)
	if !ok {
		return false
	}
	body := slices.Clone(in[rewindB+1 : aggAt])
	for i := range body {
		if body[i].Op == OpColumn && body[i].P1 == inner {
			body[i].P1, body[i].P2 = outer, seriesCols+body[i].P2
		}
	}
	pplan := segProgPlanFor(body, rewindB+1, outer, aggAt, plan, nAgg, []int{outer, inner})
	if pplan == nil {
		return false
	}
	pplan.src.seriesSrc = []*derivedSource{in[2].P4.(*derivedSource), in[3].P4.(*derivedSource)}
	// As segProgPeephole: the outer Rewind becomes a Goto to a guard past the
	// Halt, and nothing moves.
	guardAt := len(in)
	out := make([]Instruction, len(in), len(in)+7)
	copy(out, in)
	out[rewindA] = Instruction{Op: OpGoto, P2: guardAt}
	out = append(out,
		Instruction{Op: OpSegProgram, P1: dstBase, P2: outer, P3: guardAt + 3, P4: pplan},
		in[rewindA],
		Instruction{Op: OpGoto, P2: rewindB},
		in[closeB],
		in[closeA],
		Instruction{Op: OpResultRow, P1: dstBase, P2: nAgg},
		Instruction{Op: OpHalt},
	)
	prog.Insns = out
	return true
}

// segRunSeries is segRunProgramAll over the series the plan's loop reads,
// each filtered as its rewind would filter it, or false when a cursor does
// not stream one or a program cannot run over them.
func (m *vdbe) segRunSeries(plan *segProgPlan) ([]Value, bool) {
	if len(plan.lows) == 0 {
		return nil, false
	}
	rows := seriesRows{n: 1}
	var tbl *resolvedTable
	for i, c := range plan.src.series {
		if c < 0 || c >= len(m.cursors) || m.cursors[c] == nil {
			return nil, false
		}
		var sr *seriesCursor
		ok := false
		switch {
		case m.cursors[c].vt != nil:
			sr, ok = m.cursors[c].seriesScan()
		case i < len(plan.src.seriesSrc):
			sr, ok = m.seriesOf(plan.src.seriesSrc[i])
		}
		if !ok || sr.count != 0 && rows.n > math.MaxUint64/sr.count {
			return nil, false
		}
		rows.srs = append(rows.srs, sr)
		rows.n *= sr.count
		if tbl == nil {
			tbl = m.cursors[c].tbl
		}
	}
	out := make([]Value, len(plan.lows))
	for i, low := range plan.lows {
		v, ok := m.segRunSeriesOne(tbl, rows, low)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// seriesOf is the series ds reads, filtered as its stream would be, for a
// cursor that materializes it.
func (m *vdbe) seriesOf(ds *derivedSource) (*seriesCursor, bool) {
	p, err := m.dbPager(ds.dbIdx)
	if err != nil {
		return nil, false
	}
	sc, err := p.startVtabStream(*ds.vtab, m.params, m.outer, ds.vtabTrig, m, nil)
	if err != nil || sc == nil {
		return nil, false
	}
	sr, ok := sc.cur.(*seriesCursor)
	return sr, ok
}

// seriesRows is the rows a loop over its series reads: their product, the
// outer series' index changing slowest.
type seriesRows struct {
	srs []*seriesCursor // outer first
	n   uint64
}

// fill writes column col of rows r0.. into dst: col's series is
// col/seriesCols and its column there col%seriesCols.
func (s seriesRows) fill(dst []int64, r0 uint64, col int) {
	sr := s.srs[col/seriesCols]
	if c := col % seriesCols; c != seriesColValue {
		v := [...]int64{0, sr.start, sr.stop, sr.step}[c]
		for j := range dst {
			dst[j] = v
		}
		return
	}
	// Each value of the series repeats for every row of those inside it.
	run := uint64(1)
	for _, x := range s.srs[col/seriesCols+1:] {
		run *= x.count
	}
	k, at := r0/run%sr.count, r0%run
	v := sr.start + int64(k)*sr.step
	for j := range dst {
		dst[j] = v
		if at++; at == run {
			at = 0
			k++
			v += sr.step
			if k == sr.count {
				k, v = 0, sr.start
			}
		}
	}
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

func (m *vdbe) segRunSeriesOne(tbl *resolvedTable, rows seriesRows, low *segLowered) (Value, bool) {
	for _, c := range low.cols {
		if c < 0 || c >= seriesCols*len(rows.srs) {
			return Value{}, false
		}
	}
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
	bufN := min(rows.n, seriesBlockRows)
	blocks := make([][]int64, len(low.cols))
	for i, c := range low.cols {
		blocks[i] = make([]int64, bufN)
		if c%seriesCols != seriesColValue {
			rows.fill(blocks[i], 0, c)
		}
	}
	var r0 uint64
	var svc *segSvcRunner
	if len(low.blocks) > 0 {
		svc = newSegSvcRunner(m, tbl)
		svc.column = func(row, col int) Value {
			if col < 0 || col >= seriesCols*len(rows.srs) {
				return Value{Typ: Null}
			}
			var v [1]int64
			rows.fill(v[:], r0+uint64(row), col)
			return Value{Typ: Int, I: v[0]}
		}
	}
	// A DISTINCT aggregate's values come back through Sel, into one set,
	// sized for every value up to a million, as distinct values need.
	var set *int64Set
	var rd *segRealDistinct
	var vals []int64
	if low.distinct {
		vals = make([]int64, bufN)
		if low.distinctReal() {
			rd = &segRealDistinct{}
			rd.seen.reserve(int(min(rows.n, 1<<20)))
		} else {
			set = &int64Set{}
			set.reserve(int(min(rows.n, 1<<20)))
		}
	}
	results := make([]segRunResult, 0, rows.n/seriesBlockRows+1)
	for r0 = 0; r0 < rows.n; {
		n := min(rows.n-r0, seriesBlockRows)
		for i, c := range low.cols {
			if c%seriesCols == seriesColValue {
				rows.fill(blocks[i][:n], r0, c)
			}
		}
		local := slices.Clone(regs)
		var out, ovf int64
		args := &jit.ProgArgs{N: int64(n), Regs: &local[0], Out: &out, Overflow: &ovf, FAcc: facc}
		for i, b := range blocks {
			args.Col[i] = &b[0]
		}
		if low.distinct {
			args.Sel = &vals[0]
		}
		if !segCallKernel(kern, args, svc, low.blocks, nil, local) {
			return Value{}, false
		}
		facc = args.FAcc
		res := segRunResult{out: out, ovf: ovf != 0, set: set, real: rd}
		switch {
		case low.distinct:
			res.addDistinct(vals[:out])
		case low.agg != aggCountStar && low.agg != aggCount:
			res.rows = local[low.rowsReg]
		}
		results = append(results, res)
		r0 += n
	}
	return segProgAnswer(low, results, facc)
}
