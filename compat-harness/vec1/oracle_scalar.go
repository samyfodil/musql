//go:build !vec1simd

package vec1

// The oracle for the differential tests: vec1 built without SIMD
// (VEC1_TEST_SCALAR) and without contracting a*b+c into a fused multiply-add,
// so every distance it computes is the one its portable C defines. The SIMD
// builds sum in a different order (and fuse on AVX2/NEON), which moves the low
// bits of a distance with the platform; the scalar build is the only answer
// every platform shares, and the one musql reproduces. Benchmarks use the
// optimized build instead (oracle_simd.go, -tags vec1simd).
//
// vec1.c 0.7's scalar fallback leaves VEC1_COVAR_MULTIMATCH_MULT undefined
// and does not compile; 1 is the value its AVX2 build uses. The flags are
// cgo's allowed set, so contraction is turned off by pragma rather than by
// -ffp-contract=off. csrc/vec1.c is vec1 0.7 as released (public domain), and
// csrc/sqlite3.h the 3.53.3 header mattn/go-sqlite3 builds, whose symbols the
// extension links against (VEC1_STATIC).

/*
#cgo CFLAGS: -O2 -DNDEBUG -DVEC1_STATIC -DVEC1_TEST_SCALAR -DVEC1_COVAR_MULTIMATCH_MULT=1 -Wno-unused-function -Wno-unused-variable -Wno-unused-but-set-variable
#cgo LDFLAGS: -lm
#if defined(__clang__)
#pragma clang fp contract(off)
#elif defined(__GNUC__)
#pragma GCC optimize("fp-contract=off")
#endif
#include "csrc/vec1.c"
*/
import "C"

const oracleBuild = "scalar"

func registerVec1() { C.sqlite3_vec1_extra_init(nil) }
