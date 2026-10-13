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

// JitProgramStats reads and clears what the compiled programs emitted since
// the last call looked like: IR steps in and out of fusion, machine code
// bytes, registers the allocator placed and left in the file, copies it
// coalesced away, and the lengths of the runs fused (jit.ProgramStats).
func JitProgramStats() jit.ProgramStats { return jit.ProgramStatsForTest() }
