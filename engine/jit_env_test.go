package engine

import "os"

// MUSQL_JIT=0 runs the suite without the JIT (CI does both), and
// MUSQL_FUSION=0 with the JIT but without its fusion pass. The engine itself
// reads no environment.
func init() {
	var opts []Option
	if os.Getenv("MUSQL_JIT") == "0" {
		opts = append(opts, WithoutJIT())
	}
	if os.Getenv("MUSQL_FUSION") == "0" {
		opts = append(opts, WithoutFusion())
	}
	if len(opts) > 0 {
		Configure(opts...)
	}
}
