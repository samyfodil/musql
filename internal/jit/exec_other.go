//go:build !((amd64 || arm64) && (unix || windows)) && !js

package jit

import "fmt"

// Available reports whether this build can emit and run machine code (false on unsupported targets).
const Available = false

// Code is the unavailable-platform stand-in.
type Code struct{ Size int }

func Map(code []byte) (*Code, error) { return nil, fmt.Errorf("jit: not available on this platform") }
func (c *Code) Call(args *Args)      { panic("jit: not available on this platform") }
func (c *Code) Close() error         { return nil }

// Call2 is the unavailable-platform stand-in for the program JIT's entry.
func (c *Code) Call2(args *ProgArgs) { panic("jit: not available on this platform") }
