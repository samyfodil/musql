package engine

import (
	"math"
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/jit"
)

// CREATE TABLE AS from a projection over generate_series, column by column.
// The SELECT's program is the loop
//
//	Init; OpenDerived S; Rewind S; <body>; ResultRow; Next S; Close S; Halt
//
// Each result column becomes a compiled program over the body that hands back
// the column's value (POpEmitVal), run a segment's worth of rows at a time over
// the series' values in blocks, and the columns' cells make the segment
// directly (buildSegmentCells) instead of a row of Values each.

// ctasSeriesHits counts tables ctasSeriesSegments built, for tests.
var ctasSeriesHits atomic.Int64

// CTASSeriesHitsForTest returns ctasSeriesHits and zeroes it.
func CTASSeriesHitsForTest() int64 { return ctasSeriesHits.Swap(0) }

// ctasSeriesSegments is the new table's segments, cut every segmentRows rows,
// and its row count, or false when sel is not such a projection, or a value
// is not an INTEGER or a non-NULL REAL, or the table's columns would change
// one (an affinity).
func (p *ReadOnlyPager) ctasSeriesSegments(sel *SelectStmt, cols []columnInfo) ([][]byte, uint64, bool) {
	if !jitEnabled || !jit.Available || len(sel.CTEs) != 0 || len(sel.Compound) != 0 || sel.Distinct || len(sel.From) != 1 ||
		sel.GroupBy != nil || sel.Having != nil || len(sel.OrderBy) != 0 || sel.Limit != nil || sel.Offset != nil ||
		sel.LimitParam != nil || sel.OffsetParam != nil {
		return nil, 0, false
	}
	for _, c := range cols {
		if c.Aff != affNone || c.IsGenerated() {
			return nil, 0, false
		}
	}
	prog, err := compileSelectScanRow(p, sel, nil, nil)
	if err != nil || prog.Compound != nil {
		return nil, 0, false
	}
	in := prog.Insns
	end := len(in)
	if end < 7 || in[0].Op != OpInit || !segSeriesOpen(in[1]) || in[2].Op != OpRewind || in[2].P4 != nil {
		return nil, 0, false
	}
	cursor := in[1].P1
	resultAt := end - 4
	if in[2].P1 != cursor || in[2].P2 != end-2 || in[resultAt].Op != OpResultRow || in[resultAt].P2 != len(cols) ||
		in[end-3].Op != OpNext || in[end-3].P1 != cursor || in[end-3].P2 != 3 ||
		in[end-2].Op != OpClose || in[end-2].P1 != cursor || in[end-1].Op != OpHalt {
		return nil, 0, false
	}
	// A jump out of the body reads as skipping the row, which only the
	// Next's address means here.
	for _, x := range in[3:resultAt] {
		if _, _, jump, _ := segInsnRegs(x); jump && (x.P2 < 3 || x.P2 >= resultAt) && x.P2 != end-3 {
			return nil, 0, false
		}
	}
	base, ok := segLowerBody(in[3:resultAt], 3, cursor, resultAt, nil)
	if !ok {
		return nil, 0, false
	}
	for _, c := range base.cols {
		if c != seriesColValue {
			return nil, 0, false
		}
	}
	// One program per column, each handing back its value; the rows a
	// WHERE skips, every one skips.
	lows := make([]*segLowered, len(cols))
	real := make([]bool, len(cols))
	for j := range cols {
		reg := in[resultAt].P1 + j
		base.readNatively(reg)
		low := base.clone()
		low.add(jit.ProgInsn{Op: jit.POpEmitVal, A: reg})
		lows[j], real[j] = low, low.real[reg]
	}
	ds := in[1].P4.(*derivedSource)
	m := prog.newMachine(p, nil, nil, nil)
	dsPager, err := m.dbPager(ds.dbIdx)
	if err != nil {
		return nil, 0, false
	}
	sc, err := dsPager.startVtabStream(*ds.vtab, nil, nil, ds.vtabTrig, m, nil)
	if err != nil || sc == nil {
		return nil, 0, false
	}
	sr, ok := sc.cur.(*seriesCursor)
	if !ok {
		return nil, 0, false
	}
	kerns := make([]*jit.Code, len(lows))
	regs := make([][]int64, len(lows))
	var svc *segSvcRunner
	for j, low := range lows {
		if kerns[j] = segProgKernel(low); kerns[j] == nil {
			return nil, 0, false
		}
		if regs[j], ok = m.segProgRegs(low); !ok {
			return nil, 0, false
		}
	}
	rows := seriesRows{srs: []*seriesCursor{sr}, n: sr.count}
	var r0 uint64
	if len(base.blocks) > 0 {
		svc = newSegSvcRunner(m, ds.tbl)
		svc.column = func(row, col int) Value {
			var v [1]int64
			rows.fill(v[:], r0+uint64(row), col)
			return Value{Typ: Int, I: v[0]}
		}
	}
	values := make([]int64, min(rows.n, segmentRows))
	cells := make([][]uint64, len(cols))
	for j := range cells {
		cells[j] = make([]uint64, len(values))
	}
	var segs [][]byte
	var stored uint64
	for r0 = 0; r0 < rows.n; {
		n := min(rows.n-r0, segmentRows)
		rows.fill(values[:n], r0, seriesColValue)
		got := -1
		for j, low := range lows {
			local := slices.Clone(regs[j])
			var out, ovf int64
			args := &jit.ProgArgs{N: int64(n), Regs: &local[0], Out: &out, Overflow: &ovf, Sel: (*int64)(unsafe.Pointer(&cells[j][0]))}
			for i := range low.cols {
				args.Col[i] = &values[0]
			}
			if !segCallKernel(kerns[j], args, svc, low.blocks, nil, local) || ovf != 0 || got >= 0 && int(out) != got {
				return nil, 0, false
			}
			got = int(out)
			if real[j] {
				for _, b := range cells[j][:got] {
					if f := math.Float64frombits(b); f != f {
						return nil, 0, false
					}
				}
			}
		}
		if got > 0 {
			view := make([][]uint64, len(cells))
			for j := range cells {
				view[j] = cells[j][:got]
			}
			raw, err := buildSegmentCells(cols, stored+1, view, real)
			if err != nil {
				return nil, 0, false
			}
			segs = append(segs, raw)
			stored += uint64(got)
		}
		r0 += n
	}
	ctasSeriesHits.Add(1)
	return segs, stored, true
}
