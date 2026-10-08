//go:build !darwin

package engine

import (
	"os"

	"github.com/samyfodil/musql/internal/fsio"
)

// fsyncFile makes f durable. Only darwin has a stronger sync than fsync(2) to
// choose between (fsync_darwin.go); everywhere else full and plain are one call.
func fsyncFile(f *os.File, full bool) error { return fsio.Sync(f) }
