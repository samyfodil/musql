package jit

import "unsafe"

// THE WRITE BARRIER, for native code that stores a string or blob pointer.
//
// Go's compiled code guards every pointer store with a test of
// runtime.writeBarrier.enabled: off (between collections, and while the GC
// sweeps) the store is a plain store; on (while it marks) the store goes
// through gcWriteBarrier, which shades the old and the new pointer. The
// whole-program JIT makes the same test and keeps the same fast path. Its slow
// path differs in one way: with the flag on, native code does not store at
// all but exits at the instruction, and the VDBE makes the store with its
// ordinary Go assignment, barrier included. The old and new pointer the
// barrier sees are then exactly the ones Go's compiled code would hand it,
// because it is Go's compiled code.
//
// Reading the flag once per entry is sound, and is what each kernel's prologue
// does (into ProgArgs.WB): the flag changes only with the world stopped
// (gcStart sets it, mark termination clears it, both under stopTheWorld), and
// the world cannot stop while a goroutine runs native code. Generated code has
// no safe point, a preemption signal that lands in it is refused
// (isAsyncSafePoint finds no Go function at its pc), and the trampoline is
// NOSPLIT. So the flag native code reads at entry is the flag until it
// returns; fuel (VMFuel) bounds how long that holds a stop off, as it always
// has. On js/wasm there is one thread, and no Go code runs while the host is
// inside a kernel.
//
// writeBarrier is pushed by the runtime itself (its "//go:linkname
// writeBarrier", mgc.go, go.dev/issue/67401: "Do not remove or change the
// type signature"), so pulling it here is a link the linker allows. Only its
// address is used; Go code never reads the flag through this declaration.

//go:linkname runtimeWriteBarrier runtime.writeBarrier
var runtimeWriteBarrier struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// wbFlagAddr is the address of the runtime's "enabled" byte, which the
// prologue of every whole-program kernel loads.
var wbFlagAddr = uintptr(unsafe.Pointer(&runtimeWriteBarrier.enabled))

// WriteBarrierOnForTest reads the flag, for a test that checks the address is
// the runtime's: it must be seen on while a collection marks.
func WriteBarrierOnForTest() bool {
	return runtimeWriteBarrier.enabled
}

// wbForcedOn is a flag that is always on, for ForceWriteBarrierOnForTest.
var wbForcedOn = [8]byte{1}

// ForceWriteBarrierOnForTest makes the kernels emitted until restore runs read
// a write-barrier flag that is always on, so that every store of a pointer
// takes the slow path (an exit). Forcing the flag on is always safe; nothing
// can force it off.
func ForceWriteBarrierOnForTest() (restore func()) {
	old := wbFlagAddr
	wbFlagAddr = uintptr(unsafe.Pointer(&wbForcedOn[0]))
	return func() { wbFlagAddr = old }
}
