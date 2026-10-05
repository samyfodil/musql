// This file implements the runtime side of SELECT DISTINCT applied on top of
// GROUP BY (the OpGroupBatchAppend/OpGroupBatchFinal opcodes, vdbe_op.go),
// compiled by compileScanGroupBy (vdbe_agg_codegen.go). It adds no new
// dedup/sort semantics of its own: groupBatchFinal is DISTINCT-dedup by output
// tuple, then a sort by the ORDER BY keys if there are any, then a
// LIMIT/OFFSET slice -- over a batch collected during the (group-key-sorted)
// drain. See compileScanGroupBy's doc comment for why recovering the groups'
// first-seen order where the batch needs it (via each entry's collected minimum scan-order
// sequence number -- a stand-in this package already relies on elsewhere for
// the identical tie-break, valid over any join, not just a single table) is
// necessary for a faithful compile.
package engine

import "sort"

// groupBatchFinal resolves m.groupBatch (one entry per HAVING-passing group,
// in group-key-sorted order, each shaped [minSeq, groupKey[0..nGroup),
// out[0..nOut), orderKeys[0..nOrder) if plan.hasOrder]) into the query's
// final result rows:
//
//  1. Unless the batch is already in emission order (no ORDER BY and not
//     plan.scanOrder: the sorter's own order, see below), re-sort a COPY of it
//     by its collected minSeq (the group's minimum scan-order sequence
//     number), recovering the groups' FIRST-SEEN scan order -- lost once the
//     scan phase's sorter1 re-ordered them by group key for boundary
//     detection.
//  2. DISTINCT-dedup that first-seen-ordered sequence by its output tuple
//     (keysEqual), keeping the first (earliest-scanned) survivor of each
//     equivalence class.
//  3. Sort the survivors by their ORDER BY key columns (per-column DESC and
//     NULLS placement, stable, via orderTermLess) if plan.hasOrder; otherwise
//     they are already in the order SQLite emits them.
//  4. Slice by LIMIT/OFFSET (clamped start/end).
//  5. Return each survivor's output tuple as one result row.
func (m *vdbe) groupBatchFinal(plan *groupBatchPlan) [][]Value {
	entries := append([][]Value(nil), m.groupBatch...)

	// With no ORDER BY and the grouping done by the sorter, the batch is
	// already in the order SQLite emits: the GROUP BY sorter's own, under each
	// key's collation (select.c:8501 builds its KeyInfo from pGroupBy), and
	// SELECT DISTINCT is an unordered ephemeral index over the result row
	// (select.c:8263-8272) that keeps the first row EMITTED in that order.
	// Re-sorting by scan order and then by the key tuple under BINARY both
	// kept the wrong duplicate and put a NOCASE key's groups in byte order.
	drainOrder := !plan.hasOrder && !plan.scanOrder
	if !drainOrder {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i][0].I < entries[j][0].I
		})
	}

	outStart := 1 + plan.nGroup
	outSpan := func(e []Value) []Value { return e[outStart : outStart+plan.nOut] }

	kept := entries[:0]
	for _, e := range entries {
		dup := false
		for _, k := range kept {
			if keysEqualCollated(outSpan(k), outSpan(e), plan.outColl, m.encoding()) {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, e)
		}
	}

	if plan.hasOrder {
		orderStart := outStart + plan.nOut
		orderSpan := func(e []Value) []Value { return e[orderStart : orderStart+plan.nOrder] }
		sort.SliceStable(kept, func(i, j int) bool {
			a, b := orderSpan(kept[i]), orderSpan(kept[j])
			for k := range a {
				ot := OrderTerm{Desc: plan.orderDesc[k], Nulls: plan.orderNulls[k]}
				less, equal := orderTermLess(ot, a[k], b[k], atOrEmpty(plan.orderColl, k), m.encoding())
				if equal {
					continue
				}
				return less
			}
			return false
		})
	}

	start, end := 0, len(kept)
	if plan.offset != nil {
		o := *plan.offset
		switch {
		case o < 0:
			o = 0
		case int(o) > len(kept):
			o = int64(len(kept))
		}
		start = int(o)
	}
	if plan.limit != nil && *plan.limit >= 0 {
		if l := start + int(*plan.limit); l < end {
			end = l
		}
	}
	kept = kept[start:end]

	rows := make([][]Value, len(kept))
	for i, e := range kept {
		rows[i] = append([]Value(nil), outSpan(e)...)
	}
	return rows
}
