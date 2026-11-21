package engine

import "os"

// MUSQL_JIT=0 runs the suite without the JIT (CI does both), and
// MUSQL_FUSION=0 with the JIT but without its fusion pass. The engine itself
// reads no environment.
func init() {
	if os.Getenv("MUSQL_JIT") == "0" {
		envConfig = append(envConfig, WithoutJIT())
	}
	if os.Getenv("MUSQL_FUSION") == "0" {
		envConfig = append(envConfig, WithoutFusion())
	}
	if len(envConfig) > 0 {
		Configure(envConfig...)
	}
	jit0 = jitEnabled
}

// envConfig is the configuration the environment asked the suite for, which
// a test that configures the engine restores (restoreEnvConfig) rather than
// the defaults: those would turn the JIT back on for every test after it.
var envConfig []Option

func restoreEnvConfig() { Configure(envConfig...) }

// columnarOn reports whether the columnar paths are on. WithoutJIT turns them
// off with the JIT, so a test that counts what they served has nothing to
// count, and checks only that the answers agree.
func columnarOn() bool { return jitEnabled }
