package fsio

import (
	"errors"
	"syscall"
)

// ERROR_SHARING_VIOLATION; ERROR_ACCESS_DENIED is syscall's own.
const errorSharingViolation syscall.Errno = 32

// platformRenameRetryable reports whether err is a rename refused because another
// handle has the file open (see Rename).
func platformRenameRetryable(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}
