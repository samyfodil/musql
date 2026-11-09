package engine

import (
	"bytes"
	"math"
	"testing"
)

// TestSegmentNumericColumn pins PhysNumeric: a column of INTEGERs and REALs is
// one double block when every value reads back as itself, and is not when one
// would not -- an INTEGER past 2^53, a REAL that is integral (-0.0 among
// them). Each value comes back with its type through Value and segColumnRun,
// and the streamed build writes the same bytes.
func TestSegmentNumericColumn(t *testing.T) {
	const exact = 1 << 53
	base := []Value{
		{Typ: Int, I: 1}, {Typ: Float, F: 2.5}, {Typ: Int, I: -7}, {Typ: Int, I: exact}, {Typ: Int, I: -exact},
		{Typ: Float, F: 1e20}, {Typ: Float, F: -1e300}, {Typ: Float, F: 0.1}, {Typ: Null}, {Typ: Int, I: 0},
		{Typ: Float, F: math.Inf(1)}, {Typ: Float, F: 4503599627370495.5}, {Typ: Int, I: math.MaxInt32},
	}
	cases := []struct {
		name    string
		extra   Value
		numeric bool
	}{
		{"fits", Value{Typ: Int, I: 3}, true},
		{"an INTEGER past 2^53", Value{Typ: Int, I: exact + 1}, false},
		{"the largest INTEGER", Value{Typ: Int, I: math.MaxInt64}, false},
		{"an integral REAL", Value{Typ: Float, F: 3}, false},
		{"a REAL -0.0", Value{Typ: Float, F: math.Copysign(0, -1)}, false},
		{"a REAL 2^53", Value{Typ: Float, F: exact}, false},
		{"a REAL past 2^53", Value{Typ: Float, F: 2 * exact}, true},
	}
	cols := []columnInfo{{Name: "id"}, {Name: "x"}}
	for _, tc := range cases {
		vals := append(append([]Value(nil), base...), tc.extra)
		var rowids []uint64
		var rows [][]Value
		st := newSegStream(len(cols))
		for i, v := range vals {
			rowids = append(rowids, uint64(i+1))
			row := []Value{{Typ: Int, I: int64(i)}, v}
			rows = append(rows, row)
			st.add(uint64(i+1), row)
		}
		raw, err := buildSegment(cols, rowids, rows)
		if err != nil {
			t.Fatal(err)
		}
		s, err := openSegment(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.cols[1].phys == PhysNumeric; got != tc.numeric {
			t.Errorf("%s: numeric %v, want %v (%v)", tc.name, got, tc.numeric, s.cols[1].phys)
		}
		if segHasNumeric(raw) != tc.numeric {
			t.Errorf("%s: segFlagNumeric %v, want %v", tc.name, segHasNumeric(raw), tc.numeric)
		}
		plans := make([]segColPlan, len(cols))
		run := make([]Value, len(vals))
		segColumnRun(plans, s, 1, 0, rowids, run, 1)
		for i, want := range vals {
			if got := s.Value(1, i); !sameNumericValue(got, want) {
				t.Errorf("%s: row %d reads %v, want %v", tc.name, i, got, want)
			}
			if !sameNumericValue(run[i], want) {
				t.Errorf("%s: row %d runs %v, want %v", tc.name, i, run[i], want)
			}
		}
		streamed, err := st.finish()
		if err != nil {
			t.Fatal(err)
		}
		if want, _ := buildSegmentWith(nil, cols, rowids, rows, false); !bytes.Equal(streamed, want) {
			t.Errorf("%s: streamed segment differs from buildSegmentWith", tc.name)
		}
	}
}

// sameNumericValue is whether a and b are the same INTEGER, the same REAL to
// the bit, or both NULL.
func sameNumericValue(a, b Value) bool {
	if a.Typ != b.Typ {
		return false
	}
	switch a.Typ {
	case Int:
		return a.I == b.I
	case Float:
		return math.Float64bits(a.F) == math.Float64bits(b.F)
	}
	return true
}

// TestSegmentNumericFlagRequired refuses a PhysNumeric column in a segment
// whose flags do not declare one, as an older writer's would not.
func TestSegmentNumericFlagRequired(t *testing.T) {
	rows := [][]Value{{{Typ: Int, I: 1}}, {{Typ: Float, F: 1.5}}}
	raw, err := buildSegment([]columnInfo{{Name: "x"}}, []uint64{1, 2}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if !segHasNumeric(raw) {
		t.Fatal("no PhysNumeric column")
	}
	raw[4] &^= segFlagNumeric
	if _, err := openSegment(raw); err == nil {
		t.Fatal("a PhysNumeric column opened without segFlagNumeric")
	}
}
