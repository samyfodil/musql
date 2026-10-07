package compat

import (
	"os"
	"strconv"

	musqlengine "github.com/samyfodil/musql/engine"
)

// MUSQL_JIT=0 turns the JIT off for an in-process A/B run
// (TestBenchColumnarVsC), and MUSQL_WORKERS=n lets columnar scans use n
// goroutines (engine.WithWorkers). The engine itself reads no environment.
func init() {
	var opts []musqlengine.Option
	if os.Getenv("MUSQL_JIT") == "0" {
		opts = append(opts, musqlengine.WithoutJIT())
	}
	if n, err := strconv.Atoi(os.Getenv("MUSQL_WORKERS")); err == nil && n > 1 {
		opts = append(opts, musqlengine.WithWorkers(n))
	}
	if len(opts) > 0 {
		musqlengine.Configure(opts...)
	}
}
