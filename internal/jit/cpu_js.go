package jit

import "sync"

// HasVector reports whether the host compiles SIMD128, by asking it to
// compile one vector kernel.
var HasVector = sync.OnceValue(func() bool {
	code, _ := EmitFilterCountSIMD(CondE, false, CondE)
	_, err := Map(code)
	return err == nil
})

// VMMinWork is the least native work worth an entry from the VDBE (see
// engine/vdbe_jit.go's vmJITMinWork). An entry crosses into the JS host and
// back, ~10ns against ~4.7ns natively (BenchmarkWasmEntry), but the figure
// is measured, not scaled: 8 made a recursive CTE slower than no JIT at
// all, and 12 through 24 measured the same on every demo workload.
const VMMinWork = 24
