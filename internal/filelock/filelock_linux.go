//go:build linux

package filelock

import "golang.org/x/sys/unix"

// ofdSetlk/ofdGetlk are the OFD (open-file-description) byte-range lock
// fcntl commands on Linux. golang.org/x/sys/unix exposes these directly.
const (
	ofdSetlk = unix.F_OFD_SETLK
	ofdGetlk = unix.F_OFD_GETLK
)
