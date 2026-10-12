package engine

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// TestVecFastAtoFMatchesAtoF holds vector text's fast number parse to
// sqliteAtoF on every input it accepts: random significands, fractions,
// leading zeros, signs, exponents, and the edge shapes ".5", "5.", "-0", "1e0".
func TestVecFastAtoFMatchesAtoF(t *testing.T) {
	cases := []string{".5", "5.", "-0", "+0", "-0.0", "1e0", "1E-0", "0.000000000000000001", "123456789012345678",
		"1234567890123456789", "9.999999999999999e22", "1e22", "1e23", "1e-22", "1e-23", "4.9e-324", "1.7976931348623157e308",
		"1e9999", "1e-9999", "-.000", "00012.500e+3"}
	rng := rand.New(rand.NewSource(4))
	for range 300000 {
		var s string
		switch rng.Intn(4) {
		case 0:
			s = fmt.Sprintf("%g", rng.NormFloat64()*math.Pow(10, float64(rng.Intn(40)-20)))
		case 1:
			s = fmt.Sprintf("%.*f", rng.Intn(18), rng.Float64()*float64(rng.Intn(100000)))
		case 2:
			s = fmt.Sprintf("%de%d", rng.Int63n(1<<62), rng.Intn(700)-350)
		default:
			s = fmt.Sprintf("-%d.%0*d", rng.Intn(1000), rng.Intn(12)+1, rng.Intn(1000000))
		}
		cases = append(cases, s)
	}
	took := 0
	for _, s := range cases {
		got, ok := vecFastAtoF([]byte(s))
		if !ok {
			continue
		}
		took++
		want, rc := sqliteAtoF([]byte(s))
		if rc <= 0 || math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("%q: fast %v, sqliteAtoF %v (rc %d)", s, got, want, rc)
		}
	}
	if took < len(cases)/2 {
		t.Fatalf("the fast path took only %d of %d -- it is tested on too little", took, len(cases))
	}
}
