package engine

import (
	"sync"
	"sync/atomic"
)

// segThreads is how many goroutines a columnar scan may use (WithThreads).
var segThreads = 1

// segEach runs work(i) for every segment index, on up to segThreads
// goroutines, and reports whether every call succeeded. A call that fails
// stops the others taking new segments. With one thread, or one segment, it
// is a plain loop on the caller's goroutine.
func segEach(n int, work func(i int) bool) bool {
	workers := min(segThreads, n)
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
