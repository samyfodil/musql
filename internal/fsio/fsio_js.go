package fsio

import (
	"errors"
	"io"
	"os"
	"unsafe"
)

// The host reads or writes b during the call and keeps nothing.
//
//go:wasmimport musqljit file_pread
//go:noescape
func filePread(fd int32, p unsafe.Pointer, n int32, off int64) int32

//go:wasmimport musqljit file_pwrite
//go:noescape
func filePwrite(fd int32, p unsafe.Pointer, n int32, off int64) int32

var errIO = errors.New("fsio: host file operation failed")

// ReadAt reads len(b) bytes at off, as f.ReadAt does: a short read is io.EOF.
func ReadAt(f *os.File, b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n := filePread(int32(f.Fd()), unsafe.Pointer(unsafe.SliceData(b)), int32(len(b)), off)
	if n < 0 {
		return 0, errIO
	}
	if int(n) < len(b) {
		return int(n), io.EOF
	}
	return int(n), nil
}

// WriteAt writes b at off, as f.WriteAt does.
func WriteAt(f *os.File, b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n := filePwrite(int32(f.Fd()), unsafe.Pointer(unsafe.SliceData(b)), int32(len(b)), off)
	if n < 0 {
		return 0, errIO
	}
	if int(n) < len(b) {
		return int(n), io.ErrShortWrite
	}
	return int(n), nil
}

//go:wasmimport musqljit file_fsync
func fileFsync(fd int32) int32

// Sync is f.Sync.
func Sync(f *os.File) error {
	if fileFsync(int32(f.Fd())) < 0 {
		return errIO
	}
	return nil
}
