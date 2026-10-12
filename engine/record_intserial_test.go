package engine

import (
	"math"
	"math/rand"
	"testing"
)

// TestIntSerialMatchesWidthTable holds intSerial to the first width of
// intSerialWidths that fits, at every width's edges and at random.
func TestIntSerialMatchesWidthTable(t *testing.T) {
	want := func(i int64) (uint64, int) {
		for _, w := range intSerialWidths {
			if fitsSignedWidth(i, w.bytes) {
				return w.serialType, w.bytes
			}
		}
		t.Fatalf("%d fits no width", i)
		return 0, 0
	}
	vals := []int64{0, 1, -1, math.MinInt64, math.MaxInt64}
	for _, b := range []uint{8, 16, 24, 32, 48} {
		e := int64(1) << (b - 1)
		vals = append(vals, e, e-1, e+1, -e, -e-1, -e+1)
	}
	rng := rand.New(rand.NewSource(1))
	for range 100000 {
		vals = append(vals, rng.Int63()>>rng.Intn(63)*int64(1-2*rng.Intn(2)))
	}
	for _, i := range vals {
		gs, gw := intSerial(i)
		ws, ww := want(i)
		if gs != ws || gw != ww {
			t.Fatalf("intSerial(%d) = %d,%d; the table says %d,%d", i, gs, gw, ws, ww)
		}
	}
}
