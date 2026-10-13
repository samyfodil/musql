//go:build jitscore

package engine

import "sync/atomic"

// jitScore is on in a jitscore build: the VDBE counts every instruction it
// interprets, by opcode, and every program it runs with how much of it has
// native code -- the JIT lowering score's raw material (scripts/jit-score).
const jitScore = true

var (
	jsSteps                  atomic.Int64
	jsOps                    [256]atomic.Int64
	jsEnters                 atomic.Int64
	jsStaticNat, jsStaticAll atomic.Int64
)

func jitScoreStep(op OpCode) {
	jsSteps.Add(1)
	jsOps[op].Add(1)
}

func jitScoreEnter() { jsEnters.Add(1) }

func jitScoreProgram(n, native int) {
	jsStaticAll.Add(int64(n))
	jsStaticNat.Add(int64(native))
}

func jitScoreTake(calls int64) JitScoreStat {
	st := JitScoreStat{Steps: jsSteps.Swap(0), Enters: jsEnters.Swap(0), KernelCalls: calls,
		StaticNative: jsStaticNat.Swap(0), StaticAll: jsStaticAll.Swap(0), Ops: map[string]int64{}}
	for i := range jsOps {
		if n := jsOps[i].Swap(0); n > 0 {
			st.Ops[OpCode(i).String()] = n
		}
	}
	return st
}
