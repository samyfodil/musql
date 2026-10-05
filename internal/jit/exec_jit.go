//go:build (amd64 || arm64) && (unix || windows)

package jit

import "runtime"

// Available reports whether this build can emit and run machine code: amd64
// and arm64 on any OS that can map executable memory (exec_unix.go,
// exec_windows.go).
const Available = true

//go:noescape
func callKernel(code *byte, args *Args)

// Call enters the kernel with args in the first argument register: RDI on
// amd64, X0 on arm64. Each architecture's trampoline puts it there.
//
// args must stay reachable for the whole call, and it does -- it is an argument
// of this function and the trampoline declares it as a pointer, so the GC sees
// it. Every pointer a kernel follows must be reachable FROM args for the same
// reason; one smuggled in as a uintptr is invisible and its memory can be
// collected mid-kernel.
func (c *Code) Call(args *Args) {
	if c.entry == nil {
		panic("jit: Call on closed code")
	}
	callKernel(c.entry, args)
	runtime.KeepAlive(c)
	runtime.KeepAlive(args)
}

// Call2 enters the kernel with a *ProgArgs, for code emitted by EmitProgram.
//
// A second entry point rather than a generic one because the trampoline's
// signature is what makes the GC scan the argument: callKernel declares a
// *Args, so a *ProgArgs has to reach it through its own declaration or the
// collector would not see the column pointers inside it.
func (c *Code) Call2(args *ProgArgs) {
	if c.entry == nil {
		panic("jit: Call2 on closed code")
	}
	callProgram(c.entry, args)
	runtime.KeepAlive(c)
	runtime.KeepAlive(args)
}

//go:noescape
func callProgram(code *byte, args *ProgArgs)
