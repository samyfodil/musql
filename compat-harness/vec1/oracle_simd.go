//go:build vec1simd

package vec1

// The benchmark build of the oracle: vec1 compiled the way its documentation
// says to build it for speed, "-O3 -DNDEBUG -mavx2 -mfma" on x86-64 (here
// -march=haswell, which is both and passes cgo's flag check) and -O3
// with NEON (the default) on arm64.

/*
#cgo CFLAGS: -O3 -DNDEBUG -DVEC1_STATIC -Wno-unused-function -Wno-unused-variable -Wno-unused-but-set-variable
#cgo amd64 CFLAGS: -march=haswell
#cgo LDFLAGS: -lm
#include "csrc/vec1.c"
*/
import "C"

const oracleBuild = "simd"

func registerVec1() { C.sqlite3_vec1_extra_init(nil) }
