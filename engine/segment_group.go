package engine

import "slices"

// GROUP BY answered by feeding existing hash aggregation from columnar blocks.
// Replaces only the row source: writes group keys and aggregate arguments into
// registers, then calls hashAggStepRow. Every aggregate keeps working.

// segGroupPlan is what OpSegHashAgg carries: which columns feed which
// registers, and where the key tuple lives.
type segGroupPlan struct {
	keyCols []int // table column for each GROUP BY key
	keyRegs []int // register the scan body writes that key into
	argCols []int // table column for each lowered aggregate argument
	argRegs []int // register the scan body writes that argument into
	agg     *aggPlan
	seg     int // the AggStep segment selector (OpHashAggStep's P2)
	// preds is the WHERE, when there is one: only rows satisfying every
	// predicate reach the aggregate, as in the loop this replaces.
	preds []segPlanPred
}

// segHashAggTable drives the whole grouped scan from segments, or declines to
// let the ordinary loop run. A decline is all or nothing: the walk uses its own
// hash table so any refusal can fall back cleanly.
func (m *vdbe) segHashAggTable(p *ReadOnlyPager, rootPage uint32, plan *segGroupPlan) bool {
	saved := m.hashAgg
	if saved != nil && len(saved.buckets) > 0 {
		return false // not the fresh table this walk may throw away
	}
	// The WHERE's bounds resolve now, when the parameters are bound. A bound no
	// predicate kernel models (a NULL) declines the whole walk, as it does for
	// the filtered count.
	var preds []segPred
	if len(plan.preds) > 0 {
		var ok bool
		if preds, ok = m.segPlanPreds(&segFilterPlan{preds: plan.preds}); !ok {
			return false
		}
		// The rowid alias is stored as NULL and substituted on read, so neither
		// a block nor a log row can be compared on it here.
		for _, pr := range preds {
			if pr.Col == segTableIPK(m, plan) {
				return false
			}
		}
	}
	m.hashAgg = nil
	if !m.segHashAggWalk(p, rootPage, plan, preds) {
		m.hashAgg = saved
		return false
	}
	return true
}

func (m *vdbe) segHashAggWalk(p *ReadOnlyPager, rootPage uint32, plan *segGroupPlan, preds []segPred) bool {
	if p == nil || p.segs == nil || plan == nil || plan.agg == nil {
		return false
	}
	segs, ok := p.segs.byRoot[rootPage]
	if !ok {
		return false
	}
	// Logs are merged: positions the log invalidated are skipped as blocks are walked.
	skips, deltaLive, mergeable := p.segs.segDeltaSkips(rootPage, segs)
	if !mergeable {
		return false
	}
	// The row every step will be handed: correctly shaped, never read. See the
	// file comment for why that is safe, and aggPlanReadsOnlyLoweredCols for
	// the condition the recogniser checked.
	row := growValues(&m.aggRowBuf, plan.agg.nCols)
	rowids := growValues(&m.aggRowidBuf, len(plan.agg.scopes))
	// Reused across rows. hashAggStepRow copies the key into the bucket the
	// first time it sees one (append([]Value(nil), keyVals...)) and otherwise
	// only hashes it, so nothing downstream retains this slice.
	keyVals := make([]Value, len(plan.keyRegs))
	if len(deltaLive) == 0 && !segAnySkips(skips) {
		switch m.segGroupBulk(segs, plan, segTableIPK(m, plan), row, rowids, keyVals, preds) {
		case bulkAnswered:
			return true
		case bulkFailed:
			return false
		}
	}
	for si, s := range segs {
		skip, sk := skips[si], 0
		// TYPED READERS, not int64 blocks. segOrderColumns refuses anything that
		// is not a clean fixed-width int64 block, which is why a GROUP BY over a
		// TEXT, REAL or BLOB key declined to the VDBE's row loop entirely and
		// measured 1.2x-1.9x SLOWER than C SQLite where an INTEGER key measured
		// 0.30x. A reader produces exactly the Value segment.Value would, and
		// everything after this is the shared code the loop uses, so the two
		// cannot disagree. See segment_colreader.go.
		keyCols, okKeys := segColReaders(s, plan.keyCols, segTableIPK(m, plan))
		if !okKeys {
			return false
		}
		argCols, okArgs := segColReaders(s, plan.argCols, segTableIPK(m, plan))
		if !okArgs {
			return false
		}
		// The WHERE is evaluated a batch at a time into live, by the same
		// segApplyPred the filtered count uses, so the two cannot disagree on
		// which rows match. A column it cannot filter exactly (a NULL, an
		// exception, a short row's default) declines the whole walk.
		if len(preds) > 0 && !segPredColsFilterable(s, preds) {
			return false
		}
		var flags [segFilterBatch]uint8
		batchLo := -1
		for r := 0; r < s.nRows; r++ {
			// The log's own copy of this row supersedes or removes the block's,
			// and both sides are position-ordered, so one pointer walks the skip
			// list alongside the scan.
			for sk < len(skip) && skip[sk] < r {
				sk++
			}
			if sk < len(skip) && skip[sk] == r {
				sk++
				continue
			}
			if len(preds) > 0 {
				if lo := r - r%segFilterBatch; lo != batchLo {
					batchLo = lo
					live := flags[:min(lo+segFilterBatch, s.nRows)-lo]
					for i := range live {
						live[i] = 1
					}
					for _, pr := range preds {
						segApplyPred(s, pr, lo, live)
					}
				}
				if flags[r-batchLo] == 0 {
					continue
				}
			}
			for i := range plan.keyRegs {
				val, okv := keyCols[i].value(s, r)
				if !okv {
					return false
				}
				m.regs[plan.keyRegs[i]] = val
				// ...and into the ROW at that column's position. The register
				// alone is not enough: a GROUP BY key in the select list is
				// finalized from the gathered row, not from the bucket's key
				// tuple, so leaving the row empty returned NULL for the key of
				// every group -- caught by the differential check, which is the
				// only reason this comment is not a bug.
				if c := plan.keyCols[i]; c >= 0 && c < len(row) {
					row[c] = val
				}
			}
			for i := range plan.argRegs {
				val, okv := argCols[i].value(s, r)
				if !okv {
					return false
				}
				m.regs[plan.argRegs[i]] = val
				if c := plan.argCols[i]; c >= 0 && c < len(row) {
					row[c] = val
				}
			}
			for i, reg := range plan.keyRegs {
				keyVals[i] = m.regs[reg]
			}
			if err := m.hashAggStepRow(plan.agg, keyVals, plan.seg, row, rowids); err != nil {
				return false
			}
		}
	}
	// ...AND THE LOG'S OWN LIVE ROWS, in rowid order so the grouped answer does
	// not depend on a map's iteration order. They are row-major, so their values
	// come out of the row rather than a block -- and a value the lowered plan
	// cannot represent as an int64 declines the WHOLE statement rather than
	// contributing a wrong one, which is the same bargain every block read here
	// already makes.
	if len(deltaLive) > 0 {
		rids := make([]int64, 0, len(deltaLive))
		for rid := range deltaLive {
			rids = append(rids, rid)
		}
		slices.Sort(rids)
		for _, rid := range rids {
			vals := deltaLive[rid]
			if len(preds) > 0 {
				matched, served := rowMatchesPreds(vals, preds)
				if !served {
					return false
				}
				if !matched {
					continue
				}
			}
			if !segGroupStepRow(m, plan, row, rowids, keyVals, rid, vals) {
				return false
			}
		}
	}
	return true
}

// segGroupStepRow feeds ONE row-major row through the same aggregate step the
// block walk uses. It reports false to decline the whole statement, never to skip
// a row: a row the lowered plan cannot represent is a row the answer needs.
func segGroupStepRow(m *vdbe, plan *segGroupPlan, row, rowids, keyVals []Value, rid int64, vals []Value) bool {
	col := func(c int, isRowid bool) (Value, bool) {
		if isRowid || c < 0 {
			return Value{Typ: Int, I: rid}, true
		}
		if c >= len(vals) {
			// Short row: the value is the column DEFAULT, which this does not
			// know.
			return Value{}, false
		}
		if vals[c].Typ != Int {
			return Value{}, false
		}
		return vals[c], true
	}
	ipk := segTableIPK(m, plan)
	for i, c := range plan.keyCols {
		v, ok := col(c, c == ipk)
		if !ok {
			return false
		}
		m.regs[plan.keyRegs[i]] = v
		if c >= 0 && c < len(row) {
			row[c] = v
		}
	}
	for i, c := range plan.argCols {
		v, ok := col(c, c == ipk)
		if !ok {
			return false
		}
		m.regs[plan.argRegs[i]] = v
		if c >= 0 && c < len(row) {
			row[c] = v
		}
	}
	for i, reg := range plan.keyRegs {
		keyVals[i] = m.regs[reg]
	}
	return m.hashAggStepRow(plan.agg, keyVals, plan.seg, row, rowids) == nil
}

// segTableIPK is the rowid-alias column of the cursor this plan scans, or -1.
// A column declared INTEGER PRIMARY KEY is stored as NULL and substituted on
// read, so its values come from the segment's rowid block (see
// segOrderLimitTable's note).
func segTableIPK(m *vdbe, plan *segGroupPlan) int {
	if len(plan.agg.cursors) == 0 {
		return -1
	}
	cur := m.cursors[plan.agg.cursors[0]]
	if cur == nil || cur.tbl == nil {
		return -1
	}
	return cur.tbl.ipkIndex
}

func segAnySkips(skips [][]int) bool {
	for _, s := range skips {
		if len(s) > 0 {
			return true
		}
	}
	return false
}
