package libm

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

var refFuncs = map[string]func(x, y float64) float64{
	"exp":   func(x, _ float64) float64 { return Exp(x) },
	"log":   func(x, _ float64) float64 { return Log(x) },
	"log2":  func(x, _ float64) float64 { return Log2(x) },
	"pow":   Pow,
	"log10": func(x, _ float64) float64 { return Log10(x) },
	"expm1": func(x, _ float64) float64 { return Expm1(x) },
	"log1p": func(x, _ float64) float64 { return Log1p(x) },
	"sinh":  func(x, _ float64) float64 { return Sinh(x) },
	"cosh":  func(x, _ float64) float64 { return Cosh(x) },
	"tanh":  func(x, _ float64) float64 { return Tanh(x) },
}

// TestAgainstGlibc compares each function with what glibc answered for the
// same inputs, as testdata/mathref.c records them: MUSQL_LIBM_REF names a
// directory with a nofma and an fma directory of <function>.bin files, written
// by mathref on a machine without FMA and on one with it.
func TestAgainstGlibc(t *testing.T) {
	dir := os.Getenv("MUSQL_LIBM_REF")
	if dir == "" {
		t.Skip("MUSQL_LIBM_REF names no directory of glibc's answers")
	}
	defer func(f bool) { fused = f }(fused)
	for _, variant := range []string{"nofma", "fma"} {
		fused = variant == "fma"
		for name, fn := range refFuncs {
			b, err := os.ReadFile(filepath.Join(dir, variant, name+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			bad := 0
			for off := 0; off+24 <= len(b); off += 24 {
				x := math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
				y := math.Float64frombits(binary.LittleEndian.Uint64(b[off+8:]))
				want := math.Float64frombits(binary.LittleEndian.Uint64(b[off+16:]))
				got := fn(x, y)
				if math.Float64bits(got) == math.Float64bits(want) || got != got && want != want {
					continue
				}
				if bad++; bad <= 5 {
					t.Errorf("%s %s(%x, %x) = %x, glibc %x", variant, name, x, y, got, want)
				}
			}
			t.Logf("%s %s: %d records", variant, name, len(b)/24)
			if bad > 0 {
				t.Errorf("%s %s: %d of %d differ", variant, name, bad, len(b)/24)
			}
		}
	}
}
