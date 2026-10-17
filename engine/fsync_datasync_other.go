//go:build !linux

package engine

import "os"

// datasyncFile is fsyncFile where there is no fdatasync to prefer: darwin's
// fsync(2) is already the plain sync, and elsewhere there is only the one call.
func datasyncFile(f *os.File) error { return fsyncFile(f, false) }
