//go:build !linux

package engine

import (
	"errors"
	"os"
)

// newAnonFile has no unnamed files to make here: see the linux twin.
func newAnonFile(dir string) (*os.File, bool) { return nil, false }

// linkAnon is never reached here: newAnonFile made no file.
func linkAnon(f *os.File, dst string) error {
	return errors.New("engine: no unnamed files on this platform")
}
