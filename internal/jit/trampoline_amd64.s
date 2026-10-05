//go:build amd64 && (unix || windows)

#include "textflag.h"
#include "funcdata.h"

// func callKernel(code *byte, args *Args)
//
// THE ONE ASSEMBLY FILE THIS PROJECT ALLOWS (AGENTS.md Rule 2). It is not
// execution code: it is the dozen instructions that get Go into generated code
// and back out, and there is no way to write it in Go.
//
// One pointer goes in, in RDI, which is where the generated code expects it --
// System V's first argument register, and the same convention jitllm's own
// trampoline uses.
//
// RBX, R12, R13 and R15 are saved HERE rather than in each generated prologue,
// so kernels perform no pushes at all and RSP is identical on every path
// through one. That matters the moment a kernel has more than one exit.
//
// R14 is not offered on any terms: Go's amd64 register ABI pins the current
// goroutine there, and a kernel that clobbered it corrupts g -- a failure that
// does not reproduce anywhere near its cause.
//
// NOSPLIT because there is no stack to grow and no Go call to make. That is
// what keeps entry cheap, and it is also why a kernel cannot be async-preempted
// while it runs: every kernel must therefore be bounded work.
TEXT ·callKernel(SB), NOSPLIT, $32-16
	NO_LOCAL_POINTERS
	MOVQ BX, 0(SP)
	MOVQ R12, 8(SP)
	MOVQ R13, 16(SP)
	MOVQ R15, 24(SP)
	MOVQ code+0(FP), AX
	MOVQ args+8(FP), DI
	CALL AX
	MOVQ 0(SP), BX
	MOVQ 8(SP), R12
	MOVQ 16(SP), R13
	MOVQ 24(SP), R15
	RET

// func callProgram(code *byte, args *ProgArgs)
//
// Identical to callKernel and separate only because of the ARGUMENT TYPE: the
// GC scans a call's arguments through the declared signature, so the program
// JIT's ProgArgs -- which holds the column pointers the generated code follows
// -- has to be declared here to be seen.
TEXT ·callProgram(SB), NOSPLIT, $32-16
	NO_LOCAL_POINTERS
	MOVQ BX, 0(SP)
	MOVQ R12, 8(SP)
	MOVQ R13, 16(SP)
	MOVQ R15, 24(SP)
	MOVQ code+0(FP), AX
	MOVQ args+8(FP), DI
	CALL AX
	MOVQ 0(SP), BX
	MOVQ 8(SP), R12
	MOVQ 16(SP), R13
	MOVQ 24(SP), R15
	RET
