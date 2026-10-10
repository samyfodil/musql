package engine

import (
	"runtime"

	"github.com/samyfodil/musql/internal/jit"
)

// Option configures engine behavior through Configure.
type Option func(*config)

type config struct {
	noJIT   bool
	workers int
}

// WithWorkers lets a columnar scan or aggregate split its segments across n
// goroutines. The default, 1, keeps every statement on its caller's goroutine,
// as SQLite does, except a vector top-k search, which uses every core unless
// n is given. Only work whose answer cannot depend on the split is
// parallelized; everything else runs as before.
func WithWorkers(n int) Option { return func(c *config) { c.workers = n } }

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
	segWorkers = max(c.workers, 1)
	vecWorkers = runtime.GOMAXPROCS(0)
	if c.workers > 0 {
		vecWorkers = c.workers
	}
	vmJITEnabled = jitEnabled
}
