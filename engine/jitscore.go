package engine

import (
	"github.com/samyfodil/musql/internal/jit"
)

// JitScoreStat is what one statement did, for the JIT lowering score: the
// VDBE instructions it interpreted, by opcode; how many times it entered the
// whole-program JIT's native code; how many kernel and program calls ran;
// and, over every program it ran, how many instructions there were and how
// many of them have native code. All zero outside a jitscore build.
type JitScoreStat struct {
	Steps        int64
	Ops          map[string]int64
	Enters       int64
	KernelCalls  int64
	StaticNative int64
	StaticAll    int64
}

// JitScoreEnabled reports whether this is a jitscore build.
func JitScoreEnabled() bool { return jitScore }

// JitScoreTake reads and clears the counts since the last call: one
// statement's, when called around each.
func JitScoreTake() JitScoreStat {
	if !jitScore {
		return JitScoreStat{}
	}
	return jitScoreTake(jit.KernelCallsForTest())
}
