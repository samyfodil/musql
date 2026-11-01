package engine

import "slices"

// The columnar GROUP BY walk tests a WHERE's IN lists itself, a batch at a
// time beside segApplyPred, with OpInList's keys (inListRun). A value the keys
// cannot hold declines the walk.

// segPlanList is an IN list over a column (segParseInListGroup).
type segPlanList struct {
	col  int
	plan *inListPlan
	not  bool
}

// segList is a segPlanList with its values keyed for this run.
type segList struct {
	col int
	run *inListRun
	not bool
}

// segParseInListGroup reads "Column cursor.col -> r; [SCopy r -> x;] InList x
// -> d; [Not d -> d;] IfNot d -> next", an IN or NOT IN list over a column, as
// compileIn emits it.
func segParseInListGroup(in []Instruction, pc, limit, cursor, nextAt int) (segPlanList, int, bool) {
	var no segPlanList
	if pc+2 >= limit || in[pc].Op != OpColumn || in[pc].P1 != cursor {
		return no, 0, false
	}
	col, reg, p := in[pc].P2, in[pc].P3, pc+1
	if in[p].Op == OpSCopy && in[p].P1 == reg {
		reg, p = in[p].P2, p+1
	}
	if p+1 >= limit {
		return no, 0, false
	}
	list := in[p]
	plan, ok := list.P4.(*inListPlan)
	if list.Op != OpInList || !ok || plan == nil || list.P1 != reg {
		return no, 0, false
	}
	d, not := list.P2, false
	p++
	if in[p].Op == OpNot && in[p].P1 == d && in[p].P2 == d {
		not, p = true, p+1
	}
	if p >= limit {
		return no, 0, false
	}
	if t := in[p]; t.Op != OpIfNot || t.P1 != d || t.P2 != nextAt || t.P3 != 1 {
		return no, 0, false
	}
	return segPlanList{col: col, plan: plan, not: not}, p + 1, true
}

// segBindLists keys each list for this run; false over the rowid alias (stored
// NULL), in a UTF-16 database, or for a value that cannot be keyed.
func (m *vdbe) segBindLists(plan []segPlanList, ipk int) ([]segList, bool) {
	out := make([]segList, 0, len(plan))
	for _, pl := range plan {
		if pl.col < 0 || pl.col == ipk {
			return nil, false
		}
		run, ok := newInListRun(pl.plan, m.params, m.encoding())
		if !ok {
			return nil, false
		}
		out = append(out, segList{col: pl.col, run: run, not: pl.not})
	}
	return out, true
}

// passes reports whether the list's term is TRUE for v; a NULL result passes
// neither IN nor NOT IN. ok is false when v cannot be keyed.
func (l segList) passes(v Value) (bool, bool) {
	r, ok := l.run.membership(v)
	if !ok {
		return false, false
	}
	return r.Typ == Int && (r.I != 0) != l.not, true
}

// segApplyList is segApplyPred for an IN list; false declines the walk.
func segApplyList(s *segment, l segList, lo int, live []uint8) bool {
	if l.run.ints != nil && l.run.aff != affText {
		if kind, _ := s.scanKind(l.col); kind == segScanSlice {
			if full, ok := s.Int64Column(l.col); ok {
				for i, x := range full[lo : lo+len(live)] {
					if live[i] != 0 {
						_, found := slices.BinarySearch(l.run.ints, x)
						live[i] = b2flag(found != l.not)
					}
				}
				return true
			}
		}
	}
	for i := range live {
		if live[i] == 0 {
			continue
		}
		pass, ok := l.passes(s.Value(l.col, lo+i))
		if !ok {
			return false
		}
		live[i] = b2flag(pass)
	}
	return true
}

// segWhere is a columnar walk's WHERE: its comparisons and its IN lists.
type segWhere struct {
	preds []segPred
	lists []segList
}

func (w segWhere) empty() bool { return len(w.preds) == 0 && len(w.lists) == 0 }

// filterable is segPredColsFilterable over every column the WHERE reads.
func (w segWhere) filterable(s *segment) bool {
	if !segPredColsFilterable(s, w.preds) {
		return false
	}
	for _, l := range w.lists {
		if !segPredColsFilterable(s, []segPred{{Col: l.col}}) {
			return false
		}
	}
	return true
}

// apply clears live[i] for each row lo+i of s the WHERE does not pass; false
// declines the walk.
func (w segWhere) apply(s *segment, lo int, live []uint8) bool {
	for _, pr := range w.preds {
		segApplyPred(s, pr, lo, live)
	}
	for _, l := range w.lists {
		if !segApplyList(s, l, lo, live) {
			return false
		}
	}
	return true
}

// matchRow is apply for one of the log's row-major rows.
func (w segWhere) matchRow(vals []Value) (matched, served bool) {
	if matched, served = rowMatchesPreds(vals, w.preds); !matched || !served {
		return matched, served
	}
	for _, l := range w.lists {
		if l.col >= len(vals) {
			return false, false // a short row: the column DEFAULT, not known here
		}
		pass, ok := l.passes(vals[l.col])
		if !ok {
			return false, false
		}
		if !pass {
			return false, true
		}
	}
	return true, true
}
