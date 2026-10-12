//go:build !jitscore

package jit

// scoreCalls is off outside a jitscore build, so the calls below compile to
// nothing: an ordinary build pays no counter.
const scoreCalls = false

func noteCall() {}

func swapKernelCalls() int64 { return 0 }
