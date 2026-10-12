package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestVecFastComponentAsGeneral holds parseVectorText's one-pass component
// parse to its general scan: the same vector, or the same error, for texts
// of every component shape -- spaces around and inside numbers, signs,
// exponents, long digit runs, empty components, NULs, bad characters.
func TestVecFastComponentAsGeneral(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	parts := []string{"1", "-2.5", "+3", "0.000123", "1e5", "-7.25E-3", "12345678901234567890123", "00000000000000000001", " 4 ", "5 6", "", " ", "x", "1.2.3", "1e", "1e99999", ".5", "5.", "-", "\x00", "\t8\n", "9e+2", "nan", "1_0"}
	for trial := 0; trial < 20000; trial++ {
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", rng.Intn(2)) + "[")
		k := rng.Intn(6)
		for i := 0; i < k; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(parts[rng.Intn(len(parts))])
		}
		b.WriteString("]" + strings.Repeat(" ", rng.Intn(2)))
		s := b.String()
		for _, typ := range []vecType{vecF32, vecF64} {
			fast, ferr := parseVectorText([]byte(s), typ)
			vecSlowComponentsForTest = true
			slow, serr := parseVectorText([]byte(s), typ)
			vecSlowComponentsForTest = false
			if fmt.Sprint(ferr) != fmt.Sprint(serr) || fmt.Sprintf("%v", fast) != fmt.Sprintf("%v", slow) {
				t.Fatalf("%q: fast %v %v, general %v %v", s, fast, ferr, slow, serr)
			}
		}
	}
}

// BenchmarkParseVectorText parses a 384-component vector, one pass and the
// general scan.
func BenchmarkParseVectorText(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 384; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%g", rng.NormFloat64())
	}
	sb.WriteString("]")
	s := []byte(sb.String())
	for _, slow := range []bool{false, true} {
		b.Run(fmt.Sprintf("general=%v", slow), func(b *testing.B) {
			vecSlowComponentsForTest = slow
			defer func() { vecSlowComponentsForTest = false }()
			for b.Loop() {
				if _, err := parseVectorText(s, vecF32); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
