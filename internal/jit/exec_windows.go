//go:build windows && (amd64 || arm64)

package jit

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// W^X on Windows: allocate read/write, copy, then flip to execute+read, through
// kernel32 so it stays inside the standard library. FlushInstructionCache is
// what arm64 needs (its caches are not coherent) and nearly free on amd64.
var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc      = kernel32.NewProc("VirtualAlloc")
	procVirtualFree       = kernel32.NewProc("VirtualFree")
	procVirtualProtect    = kernel32.NewProc("VirtualProtect")
	procFlushInstrCache   = kernel32.NewProc("FlushInstructionCache")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
)

const (
	memCommit       = 0x1000
	memReserve      = 0x2000
	memRelease      = 0x8000
	pageReadWrite   = 0x04
	pageExecuteRead = 0x20
)

// Code is a mapped, executable kernel.
type Code struct {
	addr  uintptr
	entry *byte
	Size  int
}

// Map copies code into a fresh allocation and makes it executable (W^X order).
func Map(code []byte) (*Code, error) {
	if len(code) == 0 {
		return nil, fmt.Errorf("jit: refusing to map empty code")
	}
	addr, _, err := procVirtualAlloc.Call(0, uintptr(len(code)), memCommit|memReserve, pageReadWrite)
	if addr == 0 {
		return nil, fmt.Errorf("jit: VirtualAlloc %d bytes: %w", len(code), err)
	}
	base := unsafe.Add(unsafe.Pointer(nil), addr)
	copy(unsafe.Slice((*byte)(base), len(code)), code)
	var old uint32
	if ok, _, err := procVirtualProtect.Call(addr, uintptr(len(code)), pageExecuteRead, uintptr(unsafe.Pointer(&old))); ok == 0 {
		procVirtualFree.Call(addr, 0, memRelease)
		return nil, fmt.Errorf("jit: VirtualProtect: %w", err)
	}
	proc, _, _ := procGetCurrentProcess.Call()
	procFlushInstrCache.Call(proc, addr, uintptr(len(code)))
	c := &Code{addr: addr, entry: (*byte)(base), Size: len(code)}
	// An executable allocation is not memory the GC knows how to reclaim.
	runtime.SetFinalizer(c, func(c *Code) { c.Close() })
	return c, nil
}

// Close frees the code. Calling it while a kernel runs is a crash, so the caller
// owns that ordering.
func (c *Code) Close() error {
	if c.addr == 0 {
		return nil
	}
	addr := c.addr
	c.addr, c.entry = 0, nil
	runtime.SetFinalizer(c, nil)
	if ok, _, err := procVirtualFree.Call(addr, 0, memRelease); ok == 0 {
		return fmt.Errorf("jit: VirtualFree: %w", err)
	}
	return nil
}
