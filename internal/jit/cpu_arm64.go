//go:build arm64

package jit

// HasVector reports whether vector kernels can be emitted (always true on arm64).
func HasVector() bool { return true }
