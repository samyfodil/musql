//go:build jitscore

package jit

import "sync/atomic"

// scoreCalls is on in a jitscore build: every kernel and program call is
// counted, for the JIT lowering score (scripts/jit-score).
const scoreCalls = true

var kernelCalls atomic.Int64

func noteCall() { kernelCalls.Add(1) }

func swapKernelCalls() int64 { return kernelCalls.Swap(0) }
