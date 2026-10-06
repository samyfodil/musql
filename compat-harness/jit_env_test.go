package compat

import (
	"os"
	"strconv"

	musqlengine "github.com/samyfodil/musql/engine"
)

// MUSQL_JIT=0 turns the JIT off for an in-process A/B run
// (TestBenchColumnarVsC), and MUSQL_THREADS=n lets columnar scans use n
// goroutines (engine.WithThreads). The engine itself reads no environment.
func init() {
	var opts []musqlengine.Option
	if os.Getenv("MUSQL_JIT") == "0" {
		opts = append(opts, musqlengine.WithoutJIT())
	}
	if n, err := strconv.Atoi(os.Getenv("MUSQL_THREADS")); err == nil && n > 1 {
		opts = append(opts, musqlengine.WithThreads(n))
	}
	if len(opts) > 0 {
		musqlengine.Configure(opts...)
	}
}
