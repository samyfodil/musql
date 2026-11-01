package engine

import (
	"slices"
	"testing"
)

func TestWherePlanExplainShapes(t *testing.T) {
	b := &wherePlanIdxBuild{
		items: []whereItem{{cols: []columnInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}}},
		terms: []whereIdxTerm{{op: woEq}, {op: woGT}, {op: woLT}, {op: woGT, vector: true}, {op: woIn}},
	}
	idx := &whereIndexInfo{name: "i", aiColumn: []int{0, 1, 2}}

	for name, w := range map[string]*whereIdxLoop{"no loop": nil, "no index": {}, "too many equalities": {idx: idx, nEq: 2, lTerm: []int{0}}} {
		if a := b.accessOf(w); a.known {
			t.Errorf("accessOf(%s) = %+v, want unknown", name, a)
		}
	}
	if a := b.accessOf(&whereIdxLoop{autoIdx: true}); !a.known || !a.auto {
		t.Errorf("accessOf(automatic index) = %+v", a)
	}
	short := &whereIndexInfo{name: "s", aiColumn: []int{1}}
	if a := b.accessOf(&whereIdxLoop{idx: short, nEq: 2, lTerm: []int{0, 0}}); !slices.Equal(a.eq, []int{1, xnExpr}) {
		t.Errorf("accessOf(past the key columns) = %+v", a)
	}

	for name, w := range map[string]*whereIdxLoop{
		"automatic index": {autoIdx: true, idx: idx},
		"a vector bound":  {idx: idx, nEq: 1, lTerm: []int{0, 3}},
		"an IN bound":     {idx: idx, nEq: 1, lTerm: []int{0, 4}},
	} {
		if e := b.explainLoop(w); e.verb != "" {
			t.Errorf("explainLoop(%s) = %+v, want none", name, e)
		}
	}
	for _, tc := range []struct {
		w    *whereIdxLoop
		want string
	}{
		{&whereIdxLoop{idx: &whereIndexInfo{name: "r", aiColumn: []int{xnRowid, xnExpr}}, nEq: 2, lTerm: []int{0, 0}}, "SEARCH t USING INDEX r (rowid=? AND <expr>=?)"},
		{&whereIdxLoop{idx: idx, nEq: 2, nSkip: 1, lTerm: []int{-1, 0}}, "SEARCH t USING INDEX i (ANY(a) AND b=?)"},
		{&whereIdxLoop{idx: idx, nEq: 1, lTerm: []int{0, 1, 2}, idxOnly: true}, "SEARCH t USING COVERING INDEX i (a=? AND b>? AND b<?)"},
	} {
		if got := b.explainLoop(tc.w).line("t"); got != tc.want {
			t.Errorf("explainLoop = %q, want %q", got, tc.want)
		}
	}
}
