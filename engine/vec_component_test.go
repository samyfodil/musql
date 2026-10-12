package engine

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
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
// general scan, for components of 17 digits and of the 6 a %.6g prints.
func BenchmarkParseVectorText(b *testing.B) {
	for _, format := range []string{"%g", "%.6g"} {
		rng := rand.New(rand.NewSource(1))
		var sb strings.Builder
		sb.WriteString("[")
		for i := 0; i < 384; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, format, rng.NormFloat64()*float64(1+rng.Intn(3)))
		}
		sb.WriteString("]")
		s := []byte(sb.String())
		for _, slow := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/general=%v", format, slow), func(b *testing.B) {
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
}

// TestVecTightComponentAsAtoF holds the one-pass number parse to sqliteAtoF,
// the port of C's parser: whenever it takes a component it gives the same
// double, bit for bit, over random digit strings, exponents and the edges of
// the significand, power-of-ten and subnormal ranges.
func TestVecTightComponentAsAtoF(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	check := func(num string) {
		t.Helper()
		for _, term := range []string{",", "]"} {
			// Padded so the eight-byte reads of vecPlainComponent have room.
			s := []byte(num + term + ",1.5,2.5,3.5,4.5,5.5,6.5,7.5,8.5")
			for name, f := range map[string]func([]byte, int) (float64, int, bool){"tight": vecTightComponent, "plain": vecPlainComponent, "either": vecFastComponent} {
				x, e, ok := f(s, 0)
				if !ok {
					continue
				}
				if e != len(num) {
					t.Fatalf("%s %q: ended at %d, want %d", name, num, e, len(num))
				}
				want, rc := sqliteAtoF([]byte(num))
				if rc <= 0 || math.Float64bits(x) != math.Float64bits(want) {
					t.Fatalf("%s %q: got %v (%#x), sqliteAtoF %v rc=%d (%#x)", name, num, x, math.Float64bits(x), want, rc, math.Float64bits(want))
				}
			}
		}
	}
	for _, num := range []string{"0", "-0", "+0", "0.0", "1", "-1", ".5", "5.", "1e0", "1e22", "1e23", "1e-22", "1e-23", "9007199254740993", "9007199254740992", "123456789012345678", "1234567890123456789",
		"4.9e-324", "2.2250738585072014e-308", "1.7976931348623157e308", "1e309", "1e-400", "0.000000000000000000001", "5e-324", "1.5e-45", "3.4028235e38", "3.4028236e38", "00000000000000000001", "-.0e5"} {
		check(num)
	}
	for trial := 0; trial < 300000; trial++ {
		var b strings.Builder
		if rng.Intn(3) == 0 {
			b.WriteByte("+-"[rng.Intn(2)])
		}
		nd := 1 + rng.Intn(19)
		dot := rng.Intn(nd+1) - 1 // -1: none
		for i := 0; i < nd; i++ {
			if i == dot {
				b.WriteByte('.')
			}
			b.WriteByte(byte('0' + rng.Intn(10)))
		}
		if rng.Intn(3) == 0 {
			b.WriteByte("eE"[rng.Intn(2)])
			switch rng.Intn(3) {
			case 0:
				b.WriteByte('-')
			case 1:
				b.WriteByte('+')
			}
			fmt.Fprintf(&b, "%d", rng.Intn(400))
		}
		check(b.String())
	}
	for trial := 0; trial < 300000; trial++ {
		// The shape vecPlainComponent reads: up to 9 digits each side of the point.
		var b strings.Builder
		if rng.Intn(2) == 0 {
			b.WriteByte('-')
		}
		for i, n := 0, 1+rng.Intn(9); i < n; i++ {
			b.WriteByte(byte('0' + rng.Intn(10)))
		}
		if rng.Intn(4) != 0 {
			b.WriteByte('.')
			for i, n := 0, rng.Intn(10); i < n; i++ {
				b.WriteByte(byte('0' + rng.Intn(10)))
			}
		}
		check(b.String())
	}
	for trial := 0; trial < 100000; trial++ {
		check(strconv.FormatFloat(math.Float64frombits(rng.Uint64()), "eEfg"[rng.Intn(4)], -1, 64))
		check(fmt.Sprintf("%.6g", rng.NormFloat64()*float64(1+rng.Intn(3))))
	}
}
