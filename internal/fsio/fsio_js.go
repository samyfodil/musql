package fsio

import (
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"unsafe"
	"weak"
)

// fds caches each open file's descriptor number. On js/wasm os.File.Fd also
// resets the descriptor's blocking mode, a syscall/js round trip, every call;
// a file's number never changes while it is open. Weak keys, dropped with the
// file.
var (
	fdMu sync.Mutex
	fds  = map[weak.Pointer[os.File]]int32{}
)

func fdOf(f *os.File) int32 {
	wp := weak.Make(f)
	fdMu.Lock()
	fd, ok := fds[wp]
	fdMu.Unlock()
	if ok {
		return fd
	}
	fd = int32(f.Fd())
	fdMu.Lock()
	fds[wp] = fd
	fdMu.Unlock()
	runtime.AddCleanup(f, func(wp weak.Pointer[os.File]) {
		fdMu.Lock()
		delete(fds, wp)
		fdMu.Unlock()
	}, wp)
	return fd
}

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
	n := filePread(fdOf(f), unsafe.Pointer(unsafe.SliceData(b)), int32(len(b)), off)
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
	n := filePwrite(fdOf(f), unsafe.Pointer(unsafe.SliceData(b)), int32(len(b)), off)
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
	if fileFsync(fdOf(f)) < 0 {
		return errIO
	}
	return nil
}
