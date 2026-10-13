//go:build linux

package mmapfile

import "golang.org/x/sys/unix"

// readAheadMax is the largest mapping readAhead populates: a search's first
// touches of a small index are what populating saves (each one a minor fault
// in the first few queries), and past this the populating itself would cost
// more than the searches it speeds up.
const readAheadMax = 256 << 20

// readAhead maps the pages of data that are already in the page cache into the
// process (MADV_POPULATE_READ, Linux 5.14), so a search does not take a fault
// at each first touch of a list or a vector. A kernel without it answers
// EINVAL, and nothing is lost: the bytes read the same either way.
func readAhead(data []byte) {
	if len(data) <= readAheadMax {
		unix.Madvise(data, unix.MADV_POPULATE_READ)
	}
}
