package driver_test

import (
	"os"

	"github.com/samyfodil/musql/engine"
)

// MUSQL_JIT=0 turns the JIT off for an A/B benchmark run. The engine itself
// reads no environment.
func init() {
	if os.Getenv("MUSQL_JIT") == "0" {
		engine.Configure(engine.WithoutJIT())
	}
}
