package engine

import "github.com/samyfodil/musql/internal/jit"

// Option configures engine behavior through Configure.
type Option func(*config)

type config struct {
	noJIT bool
}

// WithoutJIT turns off all generated machine code: the segment filter kernels
// and the whole-program JIT. Queries then run on the plain VDBE.
func WithoutJIT() Option { return func(c *config) { c.noJIT = true } }

// Configure sets process-wide engine behavior; options not given take their
// defaults (the JIT is on where the platform has an emitter, amd64 and arm64).
// The JIT's kernel cache is per process, so these settings are too. Call it
// before opening databases; it is not safe to call while statements run.
func Configure(opts ...Option) {
	var c config
	for _, o := range opts {
		o(&c)
	}
	jitEnabled = !c.noJIT && jit.Available
	vmJITEnabled = jitEnabled
}
