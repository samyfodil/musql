package jit

import (
	"fmt"
	"runtime"
	"unsafe"
)

// On js/wasm a kernel is a WebAssembly module, compiled and instantiated by
// the JS host and kept in a JS-side WebAssembly.Table. Go cannot call_indirect
// into a table it does not own, so the "trampoline" is two imports the host
// supplies (examples/wasm/musql.js): compile_kernel and call_kernel. Both
// are synchronous, which browsers allow off the main thread -- run musql in a
// Web Worker. A host that does not want the JIT supplies a compile_kernel
// that returns -1; every kernel then declines and the VDBE answers.

// Available is true on js/wasm: whether a kernel actually compiles is the
// host's decision, reported by Map.
const Available = true

//go:wasmimport musqljit compile_kernel
func compileKernel(p unsafe.Pointer, n int32) int32

//go:wasmimport musqljit call_kernel
func callKernel(slot int32, args unsafe.Pointer)

// Code is a compiled kernel: its slot in the host's table.
type Code struct {
	slot int32
	Size int
}

// Map compiles a module emitted by this package.
func Map(code []byte) (*Code, error) {
	if len(code) == 0 {
		return nil, fmt.Errorf("jit: empty module")
	}
	slot := compileKernel(unsafe.Pointer(&code[0]), int32(len(code)))
	runtime.KeepAlive(code)
	if slot < 0 {
		return nil, fmt.Errorf("jit: host did not compile the kernel")
	}
	return &Code{slot: slot, Size: len(code)}, nil
}

// Call runs the kernel once over args. Every pointer it follows is reachable
// from args, which stays alive across the call.
func (c *Code) Call(args *Args) {
	callKernel(c.slot, unsafe.Pointer(args))
	runtime.KeepAlive(args)
}

// Close leaves the table slot in place: kernels are cached for the life of
// the process, and the only Close is a lost compile race, so the leak is
// bounded by the number of predicate shapes.
func (c *Code) Close() error { return nil }

// Call2 is the program JIT's entry, which has no wasm emitter yet.
func (c *Code) Call2(args *ProgArgs) { panic("jit: no program JIT on js/wasm") }
