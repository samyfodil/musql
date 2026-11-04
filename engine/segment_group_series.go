package engine

// GROUP BY over generate_series. The series is written into INTEGER segments
// of seriesBlockRows rows, one column for each the plan reads, and the bulk
// walk answers over them as it does over a table's (segGroupBulk). Only the
// bulk shape is taken: anything it declines runs the loop.

// segGroupSeriesMaxRows bounds the series a walk writes out, a block of eight
// bytes a row per column it reads.
const segGroupSeriesMaxRows = 1 << 24

// segHashAggSeries is segHashAggTable for a plan over the series cur streams.
func (m *vdbe) segHashAggSeries(cur *vdbeCursor, plan *segGroupPlan) bool {
	if plan.bare || len(plan.preds) > 0 || len(plan.lists) > 0 || plan.agg == nil {
		return false
	}
	if saved := m.hashAgg; saved != nil && len(saved.buckets) > 0 {
		return false
	}
	sr, ok := cur.seriesScan()
	if !ok || sr.count > segGroupSeriesMaxRows {
		return false
	}
	width := 0
	read := append(append([]int(nil), plan.keyCols...), plan.argCols...)
	if plan.keyProg != nil {
		read = append(read, plan.keyProg.cols...)
	}
	for _, c := range read {
		if c >= seriesCols {
			return false
		}
		width = max(width, c+1)
	}
	if width == 0 {
		return false
	}
	segs, ok := seriesSegments(seriesRows{srs: []*seriesCursor{sr}, n: sr.count}, width)
	if !ok {
		return false
	}
	saved := m.hashAgg
	m.hashAgg = nil
	row := growValues(&m.aggRowBuf, plan.agg.nCols)
	rowids := growValues(&m.aggRowidBuf, len(plan.agg.scopes))
	keyVals := make([]Value, len(plan.keyRegs))
	if m.segGroupBulk(segs, plan, -1, row, rowids, keyVals, segWhere{}, make([][]int, len(segs)), nil, make([]segColPlan, width)) == bulkAnswered {
		return true
	}
	m.hashAgg = saved
	return false
}

// seriesSegments writes rows' first width columns into segments of
// seriesBlockRows rows, rowids from 1.
func seriesSegments(rows seriesRows, width int) ([]*segment, bool) {
	cols := make([]columnInfo, width)
	for c := range cols {
		cols[c] = columnInfo{DeclType: "INTEGER", Aff: affInteger}
	}
	real := make([]bool, width)
	vals := make([]int64, min(rows.n, seriesBlockRows))
	var segs []*segment
	for r0 := uint64(0); r0 < rows.n; r0 += seriesBlockRows {
		n := min(rows.n-r0, seriesBlockRows)
		cells := make([][]uint64, width)
		for c := range cells {
			rows.fill(vals[:n], r0, c)
			cells[c] = make([]uint64, n)
			for i, v := range vals[:n] {
				cells[c][i] = uint64(v)
			}
		}
		raw, err := buildSegmentCells(cols, r0+1, cells, real)
		if err != nil {
			return nil, false
		}
		s, err := openSegment(raw)
		if err != nil {
			return nil, false
		}
		segs = append(segs, s)
	}
	return segs, true
}
