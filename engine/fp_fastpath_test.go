package engine

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

// TestFp10FastPathIsExact holds Clinger's fast path in sqliteFp10Convert2 to
// the general conversion it stands in for, and both to strconv's correctly
// rounded parse of the same decimal: random significands below 2^53 (short
// and long, round values and halfway-looking ones) at every power of ten the
// fast path takes.
func TestFp10FastPathIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 400000; i++ {
		var d uint64
		switch i % 4 {
		case 0:
			d = uint64(rng.Int63n(1 << 53))
		case 1:
			d = uint64(rng.Intn(1000000))
		case 2:
			d = uint64(rng.Int63n(1<<53-1<<52)) + 1<<52
		default:
			d = uint64(rng.Intn(100))*1000000000 + 5
		}
		if d == 0 {
			continue
		}
		p := rng.Intn(45) - 22
		fast := sqliteFp10Convert2(d, p)
		gen := fp10ConvertGeneral(d, p)
		if math.Float64bits(fast) != math.Float64bits(gen) {
			t.Fatalf("d=%d p=%d: fast %v, general %v", d, p, fast, gen)
		}
		want, err := strconv.ParseFloat(strconv.FormatUint(d, 10)+"e"+strconv.Itoa(p), 64)
		if err != nil || math.Float64bits(fast) != math.Float64bits(want) {
			t.Fatalf("d=%d p=%d: %v, strconv %v", d, p, fast, want)
		}
	}
}
