package engine

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// TestSeriesValueConstraintsNarrowExactly checks the value= / >= / > / <= / <
// narrowing ported from seriesFilter: with numeric bounds the series it
// generates is exactly the values that pass the constraints, which is what C
// answers without re-testing them. Series run near both ends of int64, both
// ways, so the wrapping arithmetic is exercised.
func TestSeriesValueConstraintsNarrowExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	anchors := []int64{0, 1, -1, 100, -100, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1}
	steps := []int64{1, 2, 3, 7, -1, -2, -5, 1 << 40, -(1 << 40), math.MaxInt64, math.MinInt64}
	for n := 0; n < 20000; n++ {
		step := steps[rng.Intn(len(steps))]
		count := int64(rng.Intn(60))
		start := anchors[rng.Intn(len(anchors))] + int64(rng.Intn(20)-10)
		stop := seriesLast(start, step, count)
		if stop == nil {
			continue
		}
		vals := seriesValues(start, stop.Int64(), step)
		var argv []Value
		idx := seriesHasStart | seriesHasStop | seriesHasStep
		argv = append(argv, Value{Typ: Int, I: start}, Value{Typ: Int, I: stop.Int64()}, Value{Typ: Int, I: step})
		type cons struct {
			op  string
			arg Value
		}
		var cs []cons
		pick := func() Value {
			var v int64
			if len(vals) > 0 && rng.Intn(3) > 0 {
				v = vals[rng.Intn(len(vals))] + int64(rng.Intn(5)-2)
			} else {
				v = anchors[rng.Intn(len(anchors))] + int64(rng.Intn(7)-3)
			}
			// A REAL near the ends of int64 saturates as in C (seriesRealToI64),
			// which an exact comparison does not.
			if rng.Intn(3) == 0 && v > -1<<62 && v < 1<<62 {
				return Value{Typ: Float, F: float64(v) + []float64{0, 0.5, -0.5, 0.25}[rng.Intn(4)]}
			}
			return Value{Typ: Int, I: v}
		}
		switch rng.Intn(4) {
		case 0:
			idx |= seriesValueEq
			cs = append(cs, cons{"=", pick()})
		case 1:
			op := []string{">=", ">"}[rng.Intn(2)]
			idx |= map[string]int{">=": seriesValueGE, ">": seriesValueGT}[op]
			cs = append(cs, cons{op, pick()})
		case 2:
			op := []string{"<=", "<"}[rng.Intn(2)]
			idx |= map[string]int{"<=": seriesValueLE, "<": seriesValueLT}[op]
			cs = append(cs, cons{op, pick()})
		default:
			lop, hop := []string{">=", ">"}[rng.Intn(2)], []string{"<=", "<"}[rng.Intn(2)]
			idx |= map[string]int{">=": seriesValueGE, ">": seriesValueGT}[lop] | map[string]int{"<=": seriesValueLE, "<": seriesValueLT}[hop]
			cs = append(cs, cons{lop, pick()}, cons{hop, pick()})
		}
		for _, c := range cs {
			argv = append(argv, c.arg)
		}
		var want []int64
		for _, v := range vals {
			ok := true
			for _, c := range cs {
				r := compareValues(Value{Typ: Int, I: v}, c.arg)
				switch c.op {
				case "=":
					ok = ok && r == 0
				case ">=":
					ok = ok && r >= 0
				case ">":
					ok = ok && r > 0
				case "<=":
					ok = ok && r <= 0
				case "<":
					ok = ok && r < 0
				}
			}
			if ok {
				want = append(want, v)
			}
		}
		var cur seriesCursor
		if err := cur.Filter(idx, "", argv); err != nil {
			t.Fatalf("series(%d,%d,%d) %v: %v", start, stop, step, cs, err)
		}
		var got []int64
		for ; !cur.Eof(); cur.Next() {
			v, _ := cur.Column(seriesColValue)
			got = append(got, v.I)
			if len(got) > len(vals)+1 {
				break
			}
		}
		if !equalInt64s(got, want) {
			t.Fatalf("series(%d,%d,%d) %v: got %v, want %v", start, stop, step, cs, got, want)
		}
		if h, _ := cur.Column(seriesColStart); h.I != start {
			t.Fatalf("start column %d, want %d", h.I, start)
		}
	}
}

// seriesLast is the value count-1 steps from start, or nil past int64.
func seriesLast(start, step, count int64) *big.Int {
	if count == 0 {
		count = 1
	}
	v := new(big.Int).Mul(big.NewInt(step), big.NewInt(count-1))
	v.Add(v, big.NewInt(start))
	if !v.IsInt64() {
		return nil
	}
	return v
}

func seriesValues(start, stop, step int64) []int64 {
	var out []int64
	v := big.NewInt(start)
	for len(out) < 100 {
		if step > 0 && v.Cmp(big.NewInt(stop)) > 0 || step < 0 && v.Cmp(big.NewInt(stop)) < 0 || !v.IsInt64() {
			break
		}
		out = append(out, v.Int64())
		v.Add(v, big.NewInt(step))
	}
	return out
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
