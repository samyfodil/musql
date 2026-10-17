//go:build (amd64 || arm64) && unix

package jit

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// Code is a mapped, executable kernel.
type Code struct {
	page  []byte
	entry *byte
	Size  int
}

// Map copies code into a fresh mapping and makes it executable (W^X order).
func Map(code []byte) (*Code, error) {
	if len(code) == 0 {
		return nil, fmt.Errorf("jit: refusing to map empty code")
	}
	page, err := unix.Mmap(-1, 0, len(code),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("jit: mmap %d bytes: %w", len(code), err)
	}
	copy(page, code)
	if err := unix.Mprotect(page, unix.PROT_READ|unix.PROT_EXEC); err != nil {
		unix.Munmap(page)
		return nil, fmt.Errorf("jit: mprotect: %w", err)
	}
	// The code was written through an ordinary copy, so on arm64 it is still in
	// the data cache and the instruction side would fetch stale memory. A no-op
	// on amd64, which is coherent by architecture.
	flushICache(&page[0], uintptr(len(code)))
	c := &Code{page: page, entry: &page[0], Size: len(code)}
	// An RX mapping is not memory the GC knows how to reclaim.
	runtime.SetFinalizer(c, func(c *Code) { c.Close() })
	return c, nil
}

// Close unmaps the code. Calling it while a kernel runs is a segfault, so the
// caller owns that ordering.
func (c *Code) Close() error {
	if c.page == nil {
		return nil
	}
	p := c.page
	c.page, c.entry = nil, nil
	runtime.SetFinalizer(c, nil)
	return unix.Munmap(p)
}
