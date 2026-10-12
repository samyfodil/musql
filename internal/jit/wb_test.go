package jit

import (
	"runtime"
	"testing"
	"time"
)

var wbSink [][]byte

// TestWriteBarrierFlagIsTheRuntimes checks the address the kernels read is the
// flag the runtime sets: off with no collection running, and seen on while
// collections run in a loop beside this goroutine.
func TestWriteBarrierFlagIsTheRuntimes(t *testing.T) {
	runtime.GC()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer close(stop)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for range 1000 {
			if WriteBarrierOnForTest() {
				return
			}
			wbSink = append(wbSink[:0], make([]byte, 64))
		}
	}
	t.Fatal("the flag was never seen on while the GC ran")
}
