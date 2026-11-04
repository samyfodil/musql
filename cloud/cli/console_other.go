//go:build !windows

package cli

// consoleVT is a no-op: a terminal here interprets escape sequences already.
func consoleVT(int) (restore func()) { return func() {} }
