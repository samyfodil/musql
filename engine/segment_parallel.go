package engine

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// segWorkers is how many goroutines a columnar scan may use (WithWorkers).
var segWorkers = 1

// vecWorkers is how many goroutines a vector top-k search may use. Unlike a
// scan it defaults to every core: a search reads every row of a column and
// its answer is exact whatever the split, so the cores are free speed.
// WithWorkers, when given, sets it too.
var vecWorkers = runtime.GOMAXPROCS(0)

// segEach runs work(i) for every segment index, on up to segWorkers
// goroutines, and reports whether every call succeeded. A call that fails
// stops the others taking new segments. With one worker, or one segment, it
// is a plain loop on the caller's goroutine.
func segEach(n int, work func(i int) bool) bool {
	return segEachOn(segWorkers, n, work)
}

// segEachOn is segEach on up to workers goroutines.
func segEachOn(workers, n int, work func(i int) bool) bool {
	workers = min(workers, n)
	if workers <= 1 {
		for i := range n {
			if !work(i) {
				return false
			}
		}
		return true
	}
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if !work(i) {
					failed.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	return !failed.Load()
}
