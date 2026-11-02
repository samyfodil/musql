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
	"asinh": func(x, _ float64) float64 { return Asinh(x) },
	"acosh": func(x, _ float64) float64 { return Acosh(x) },
	"atanh": func(x, _ float64) float64 { return Atanh(x) },
	"sin":   func(x, _ float64) float64 { return Sin(x) },
	"cos":   func(x, _ float64) float64 { return Cos(x) },
	"tan":   func(x, _ float64) float64 { return Tan(x) },
	"asin":  func(x, _ float64) float64 { return Asin(x) },
	"acos":  func(x, _ float64) float64 { return Acos(x) },
	"atan":  func(x, _ float64) float64 { return Atan(x) },
	"atan2": Atan2,
	// What C SQLite calls that needs no port: Go's are exact too, and
	// radians/degrees multiply by M_PI/180.0 and 180.0/M_PI, which are the
	// doubles nearest pi/180 and 180/pi.
	"fmod":    math.Mod,
	"sqrt":    func(x, _ float64) float64 { return math.Sqrt(x) },
	"radians": func(x, _ float64) float64 { return x * (math.Pi / 180) },
	"degrees": func(x, _ float64) float64 { return x * (180 / math.Pi) },
}

// ibmFuncs are the functions glibc takes from IBM's library, which is not
// correctly rounded: these may differ from it by an ulp.
var ibmFuncs = map[string]bool{"sin": true, "cos": true, "tan": true, "asin": true, "acos": true, "atan": true, "atan2": true}

// TestAgainstGlibc compares each function with what glibc answered for the
// same inputs, as testdata/mathref.c and mathwc.c record them: MUSQL_LIBM_REF
// names a directory with a nofma and an fma directory of <function>.bin
// files, written on a machine without FMA and on one with it. A build with no
// directory is skipped: a machine without FMA can record only nofma.
func TestAgainstGlibc(t *testing.T) {
	dir := os.Getenv("MUSQL_LIBM_REF")
	if dir == "" {
		t.Skip("MUSQL_LIBM_REF names no directory of glibc's answers")
	}
	defer func(f bool) { fused = f }(fused)
	for _, variant := range []string{"nofma", "fma"} {
		if _, err := os.Stat(filepath.Join(dir, variant)); os.IsNotExist(err) {
			t.Logf("no %s records in %s", variant, dir)
			continue
		}
		fused = variant == "fma"
		for name, fn := range refFuncs {
			// <name>.bin is mathref's sample, <name>-wc.bin the inputs
			// CORE-MATH lists as hardest to round, where there is one.
			for _, file := range []string{name + ".bin", name + "-wc.bin"} {
				b, err := os.ReadFile(filepath.Join(dir, variant, file))
				if os.IsNotExist(err) && file != name+".bin" {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				bad, ulp, refereed := 0, 0, 0
				for off := 0; off+24 <= len(b); off += 24 {
					x := math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
					y := math.Float64frombits(binary.LittleEndian.Uint64(b[off+8:]))
					want := math.Float64frombits(binary.LittleEndian.Uint64(b[off+16:]))
					got := fn(x, y)
					if math.Float64bits(got) == math.Float64bits(want) || got != got && want != want {
						continue
					}
					if ibmFuncs[name] {
						// Where glibc is IBM's, the answer is the correctly
						// rounded one, which bigRef referees: every
						// disagreement over an ulp, and one in 64 of the rest.
						near := math.Nextafter(want, math.Inf(1)) == got || math.Nextafter(want, math.Inf(-1)) == got
						if near {
							ulp++
						}
						ref, ok := bigRef(name, x, y)
						if !ok && near || ok && near && ulp%64 != 1 {
							continue
						}
						if ok && math.Float64bits(got) == math.Float64bits(ref) {
							refereed++
							continue
						}
						if ok {
							if bad++; bad <= 5 {
								t.Errorf("%s %s(%x, %x) = %x, correctly rounded %x, glibc %x", variant, name, x, y, got, ref, want)
							}
							continue
						}
					}
					if bad++; bad <= 5 {
						t.Errorf("%s %s(%x, %x) = %x, glibc %x", variant, name, x, y, got, want)
					}
				}
				t.Logf("%s %s: %d records, %d an ulp from glibc, %d refereed", variant, file, len(b)/24, ulp, refereed)
				if bad > 0 {
					t.Errorf("%s %s: %d of %d differ", variant, file, bad, len(b)/24)
				}
			}
		}
	}
}
