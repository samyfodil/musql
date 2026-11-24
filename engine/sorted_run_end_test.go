package engine

import "testing"

// TestRunEnd: segSortedRows.find's end of a run, galloped to, is the first
// key past the probe from the run's start, for runs of every length up to
// past the array's end.
func TestRunEnd(t *testing.T) {
	for n := range 70 {
		for run := 0; run <= n; run++ {
			for lo := 0; lo+run <= n; lo++ {
				keys := make([]int64, n)
				for i := range keys {
					switch {
					case i < lo:
						keys[i] = 1
					case i < lo+run:
						keys[i] = 2
					default:
						keys[i] = 3
					}
				}
				if got := runEnd(lo, n, func(i int) bool { return keys[i] > 2 }); got != lo+run {
					t.Fatalf("n %d run [%d, %d): end %d", n, lo, lo+run, got)
				}
			}
		}
	}
}
