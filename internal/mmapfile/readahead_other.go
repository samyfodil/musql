//go:build !darwin && !linux

package mmapfile

// readAhead does nothing where a file just written stays in memory: see the
// darwin twin.
func readAhead([]byte) {}
