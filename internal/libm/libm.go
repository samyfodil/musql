// Package libm is the part of glibc's libm C SQLite's math functions call
// (SQLITE_ENABLE_MATH_FUNCTIONS, func.c), giving the answers glibc 2.43 gives
// on x86-64 bit for bit, so a REAL computed here prints the same digits.
//
// glibc builds some of these functions twice on x86-64, plainly and with
// -mfma -mavx2, and its ifunc runs the second on a CPU with FMA and AVX2. The
// two builds disagree in the last bit for roughly one input in a thousand, so
// this package carries both: each madd is a multiply-add GCC fuses in the FMA
// build, and fused is the choice glibc makes on this CPU. Elsewhere a product
// that is added is converted with float64(), which stops the Go compiler from
// fusing it where glibc's build does not.
package libm

import (
	"math"
	"runtime"

	"golang.org/x/sys/cpu"
)

// fused is whether glibc's ifunc picks its FMA builds on this CPU. Other
// architectures get the plain build: glibc on arm64 rounds to integers
// differently, so neither build is its answer there.
var fused = runtime.GOARCH == "amd64" && cpu.X86.HasFMA && cpu.X86.HasAVX2

// madd is a*b+c, rounded once where glibc's FMA build fuses it and twice
// where its plain build does not.
func madd(a, b, c float64) float64 {
	if fused {
		return math.FMA(a, b, c)
	}
	return float64(a*b) + c
}

// oflow and uflow are glibc's __math_oflow and __math_uflow: an infinity or
// a zero with the sign sign selects.
func oflow(sign uint64) float64 {
	if sign != 0 {
		return math.Inf(-1)
	}
	return math.Inf(1)
}

func uflow(sign uint64) float64 {
	if sign != 0 {
		return math.Copysign(0, -1)
	}
	return 0
}

func asuint64(x float64) uint64 { return math.Float64bits(x) }

func asdouble(i uint64) float64 { return math.Float64frombits(i) }

func top12(x float64) uint32 { return uint32(asuint64(x) >> 52) }

func top16(x float64) uint32 { return uint32(asuint64(x) >> 48) }
