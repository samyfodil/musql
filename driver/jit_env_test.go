package driver_test

import (
	"os"

	"github.com/samyfodil/musql/engine"
)

// MUSQL_JIT=0 turns the JIT off for an A/B benchmark run, and MUSQL_FUSION=0
// its fusion pass alone. The engine itself reads no environment.
func init() {
	var opts []engine.Option
	if os.Getenv("MUSQL_JIT") == "0" {
		opts = append(opts, engine.WithoutJIT())
	}
	if os.Getenv("MUSQL_FUSION") == "0" {
		opts = append(opts, engine.WithoutFusion())
	}
	if len(opts) > 0 {
		engine.Configure(opts...)
	}
}
