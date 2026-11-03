//go:build !amd64 && !arm64 && !wasm

package jit

import "github.com/samyfodil/musql/internal/fir"

// KernelOK reports whether f compiles into a program here: not yet on this
// architecture, where a call to one runs in Go.
func KernelOK(*fir.Func) bool { return false }
