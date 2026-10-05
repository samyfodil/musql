package engine

import "os"

// MUSQL_JIT=0 runs the suite without the JIT (CI does both). The engine itself
// reads no environment.
func init() {
	if os.Getenv("MUSQL_JIT") == "0" {
		Configure(WithoutJIT())
	}
}
