//go:build arm64 && (unix || windows)

#include "textflag.h"
#include "funcdata.h"

// func callKernel(code *byte, args *Args)
//
// The arm64 half of the one thing this project allows to be written in assembly
// (AGENTS.md Rule 2): not execution code, but the handful of instructions that
// get Go into generated code and back, which cannot be written in Go.
//
// One pointer goes in, in R0 -- AAPCS' first argument register, and what the
// emitted prologue in filter_arm64.go reads its Args from.
//
// Nothing is saved here, where the amd64 trampoline saves four registers. It
// does not need to be. callKernel is an ABI0 function called only from Go,
// and Go treats every register as clobbered across such a call except the
// ones it reserves (below), so a kernel may use X0-X17 and V0-V31. The filter
// kernels happen to stay inside X0-X10 and V0-V7; the vector-search kernels
// (vector_neon_arm64.go) use X11-X17 and up to V27. AAPCS's callee-saved
// registers (X19-X29, the low halves of V8-V15) would bind a C caller, and a
// kernel never has one.
//
// What it does need is the FRAME. A zero-size frame makes this a leaf in the
// assembler's eyes and LR is then not saved -- and BL overwrites LR, so the
// return would go back into the kernel. The 16 bytes below exist only to make
// the prologue save it.
//
// X28, X27 and X18 are not offered on any terms: Go pins the goroutine in X28,
// the assembler keeps its temporary in X27, and X18 is the platform register
// darwin reserves. A kernel that wrote one corrupts state whose failure does
// not appear anywhere near its cause.
//
// NOSPLIT because there is no stack to grow and no Go call to make -- which is
// also why a kernel cannot be async-preempted, so each must be bounded work.
TEXT ·callKernel(SB), NOSPLIT, $16-16
	NO_LOCAL_POINTERS
	MOVD code+0(FP), R1
	MOVD args+8(FP), R0
	BL   (R1)
	RET

// func callProgram(code *byte, args *ProgArgs)
//
// See the amd64 twin: separate only so the GC sees ProgArgs' pointers through
// this declaration.
TEXT ·callProgram(SB), NOSPLIT, $16-16
	NO_LOCAL_POINTERS
	MOVD code+0(FP), R1
	MOVD args+8(FP), R0
	BL   (R1)
	RET

// func flushICache(start *byte, n uintptr)
//
// THE SECOND SYMBOL IN THIS FILE, and the file count is what AGENTS.md Rule 2
// actually enforces: one .s per architecture with an emitter, nothing else. It
// is the same category as the trampoline above -- a dozen instructions that get
// Go into and out of generated code, not execution code, and not writable in Go.
//
// It exists because mprotect is NOT enough on arm64. The code was written
// through an ordinary Go copy(), so it sits in the DATA cache; the instruction
// side fetches from the point of unification and sees whatever the recycled
// physical page held before. musql hit exactly that on an M4 the moment the
// row pre-filter started mapping many small programs: SIGILL at a fixed offset
// inside a freshly mapped page, where the executed word was an unallocated SIMD
// encoding and the word EMITTED there was an ordinary CMP. Apple documents
// sys_icache_invalidate for this; these are the instructions it runs.
//
// The stride is 16 bytes, the ARM architectural MINIMUM cache line. Reading the
// real line size out of CTR_EL0 would let it take longer steps, but a stride
// larger than the true line SKIPS lines, and the programs here are ~100 bytes,
// so the loop is a handful of iterations either way.
TEXT ·flushICache(SB), NOSPLIT, $0-16
	MOVD start+0(FP), R0
	MOVD n+8(FP), R1
	CBZ  R1, done
	ADD  R0, R1, R1 // end

	MOVD R0, R2
dloop:
	WORD $0xD50B7B22 // DC CVAU, R2  -- clean data cache to the point of unification
	ADD  $16, R2
	CMP  R1, R2
	BLO  dloop
	WORD $0xD5033B9F // DSB ISH

	MOVD R0, R2
iloop:
	WORD $0xD50B7522 // IC IVAU, R2  -- invalidate instruction cache to PoU
	ADD  $16, R2
	CMP  R1, R2
	BLO  iloop
	WORD $0xD5033B9F // DSB ISH
	WORD $0xD5033FDF // ISB

done:
	RET
