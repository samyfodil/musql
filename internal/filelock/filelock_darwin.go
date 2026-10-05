//go:build darwin

package filelock

// ofdSetlk/ofdGetlk are the OFD (open-file-description) byte-range lock
// fcntl commands on Darwin. These are private-but-stable Apple constants:
// they are not exposed by golang.org/x/sys/unix (Darwin's public headers
// only advertise the whole-file-description-unaware F_SETLK/F_SETLKW/
// F_GETLK), but the values are fixed ABI and this is the same approach
// used by e.g. github.com/ncruces/go-sqlite3's OS-level lock support.
const (
	ofdSetlk = 90
	ofdGetlk = 92
)
