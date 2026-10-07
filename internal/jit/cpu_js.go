package jit

import "sync"

// HasVector reports whether the host compiles SIMD128, by asking it to
// compile one vector kernel.
var HasVector = sync.OnceValue(func() bool {
	code, _ := EmitFilterCountSIMD(CondE, false, CondE)
	_, err := Map(code)
	return err == nil
})
