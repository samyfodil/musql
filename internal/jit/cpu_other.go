//go:build !amd64 && !arm64

package jit

// HasVector is false on every architecture without an encoder.
func HasVector() bool { return false }
