//go:build darwin

package mmapfile

import (
	"sync"

	"golang.org/x/sys/unix"
)

// readAhead asks for data, a whole mapping, to be read in (MADV_WILLNEED).
// On macOS a segment file musql has just written is not left in memory the
// way C SQLite's is, so a search's first touch of each scattered vector was a
// read from the disk: traced on an M4 at 1M rows, 100 re-ranked rows cost
// 5 ms a query cold and 0.13 ms once read, where C SQLite + vec1's whole
// query took 2.2 ms. A file larger than a quarter of physical memory is not
// read ahead: it would push out more than it brings in.
func readAhead(data []byte) {
	if uint64(len(data)) <= physMem()/4 {
		unix.Madvise(data, unix.MADV_WILLNEED)
	}
}

var physMem = sync.OnceValue(func() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
})
