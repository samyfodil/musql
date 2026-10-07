package filelock

import (
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"
)

// On js/wasm the "filesystem" is the host's fs object -- in a browser, an
// in-memory one that only this Go instance can see -- so there is no other
// process to coordinate with and the whole lock table can live here. What it
// keeps is the property the engine relies on: locks belong to an *os.File, so
// two connections in this process conflict exactly as two OFD locks would.
// Under Node the fs is the real one, and other processes are not excluded;
// that is acceptable for the test runner and nothing else uses it.

type held struct {
	start, end int64
	excl       bool
}

func (h held) overlaps(o held) bool { return h.start < o.end && o.start < h.end }

func span(start, length int64) held {
	if length == 0 {
		return held{start: start, end: math.MaxInt64}
	}
	return held{start: start, end: start + length}
}

// outside returns the parts of h not covered by cut.
func outside(h, cut held) []held {
	var out []held
	if h.start < cut.start {
		out = append(out, held{h.start, cut.start, h.excl})
	}
	if cut.end < h.end {
		out = append(out, held{cut.end, h.end, h.excl})
	}
	return out
}

type fileID struct{ dev, ino uint64 }

var (
	mu    sync.Mutex
	locks = map[fileID]map[*os.File][]held{}
)

func idOf(f *os.File) (fileID, error) {
	fi, err := f.Stat()
	if err != nil {
		return fileID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, fmt.Errorf("filelock: no file identity for %s", f.Name())
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// LockRange takes a non-blocking byte-range lock with POSIX semantics (length 0
// is "to the end of the file"). ok=false (err=nil) means another *os.File
// holds the range incompatibly.
func LockRange(f *os.File, start, length int64, exclusive bool) (ok bool, err error) {
	id, err := idOf(f)
	if err != nil {
		return false, err
	}
	mu.Lock()
	defer mu.Unlock()
	req := span(start, length)
	req.excl = exclusive
	owners := locks[id]
	if owners == nil {
		owners = map[*os.File][]held{}
		locks[id] = owners
	}
	for o, hs := range owners {
		if o == f {
			continue
		}
		// A closed file's locks went with it, as an OFD lock does.
		if o.Fd() == ^uintptr(0) {
			delete(owners, o)
			continue
		}
		for _, h := range hs {
			if h.overlaps(req) && (h.excl || exclusive) {
				return false, nil
			}
		}
	}
	var rest []held
	for _, h := range owners[f] {
		if h.overlaps(req) {
			rest = append(rest, outside(h, req)...)
		} else {
			rest = append(rest, h)
		}
	}
	owners[f] = append(rest, req)
	return true, nil
}

// UnlockRange releases any part of [start, start+length) this file holds.
func UnlockRange(f *os.File, start, length int64) error {
	id, err := idOf(f)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	cut := span(start, length)
	var rest []held
	for _, h := range locks[id][f] {
		if h.overlaps(cut) {
			rest = append(rest, outside(h, cut)...)
		} else {
			rest = append(rest, h)
		}
	}
	if owners := locks[id]; owners != nil {
		if len(rest) == 0 {
			delete(owners, f)
		} else {
			owners[f] = rest
		}
	}
	return nil
}
