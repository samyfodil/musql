package compat

import (
	"os"

	musqlengine "github.com/samyfodil/musql/engine"
)

// MUSQL_JIT=0 turns the JIT off for an in-process A/B run
// (TestBenchColumnarVsC). The engine itself reads no environment.
func init() {
	if os.Getenv("MUSQL_JIT") == "0" {
		musqlengine.Configure(musqlengine.WithoutJIT())
	}
}
