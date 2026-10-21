package engine

import (
	"encoding/binary"
	"math/rand/v2"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/samyfodil/musql/internal/fsio"
	"github.com/samyfodil/musql/internal/mmapfile"
)

// THE CHANGE WORD: a statement's freshness check without a system call.
//
// A held session asks before every statement whether another connection has
// moved the file pair under it (RefreshIfStale), and answered by stamping both
// files: an fstat of the segment file and a stat of the delta's PATH, which
// fails when there is no delta and so is a full lookup every time. Twelve
// connections reading at once on macOS spent 90% of their time in those two
// calls, serialised on the file's vnode: 16 us a point read, against 2.7 us
// on one connection.
//
// So every change to the pair also writes a word in the lock file -- which
// every connection already opens and nothing renames -- and a session maps it
// and compares. It is a random value, not a counter: a writer does not need to
// read it first, two writers cannot collide on it, and a torn read is just a
// value no one stored. Its low bit says whether a change is in progress:
//
//   - segChangeBegin stores an ODD value before a writer touches either file,
//     and the func it returns stores an EVEN one once it is done;
//   - a reader that sees the even value it recorded at its last full check
//     skips the stamps: nothing has begun since;
//   - any other value -- a different one, or an odd one -- sends it to the full
//     check, and only an even value is ever recorded, so a writer that died
//     between the two leaves every reader checking in full until the next
//     change completes.
//
// It covers what this engine writes; it cannot see the file being replaced
// by something else entirely (cp over an open database), which C SQLite's WAL
// readers cannot either -- they read the wal-index, not the file. A file of a
// version before 6 may be written by a build that does not keep the word, so a
// session trusts it only on a file this build's format wrote (changeWordOK).

// changeWordVersion is the first segment file version whose every writer keeps
// the word.
const changeWordVersion = 6

// changeWordOff is where the word sits in the lock file: a page in, clear of
// the bytes the locks are taken on (segStateByte, segReservedByte), which
// Windows' LockFileEx makes unwritable to anyone else while they are held.
const changeWordOff = 4096

// segChangeBegin marks a change to segPath's pair as begun and returns the
// func that marks it done. The change must not go ahead when the mark could not
// be stored: readers would go on trusting the word. The end's own failure needs
// no handling -- it leaves the word odd, and an odd word is checked in full.
func segChangeBegin(segPath string) (func(), error) { return segChangeBeginOn(segPath, nil) }

// segChangeBeginOn is segChangeBegin storing through lockF, segPath's lock file
// already open, when it is not nil.
func segChangeBeginOn(segPath string, lockF *os.File) (func(), error) {
	if err := storeChangeWord(segPath, lockF, rand.Uint64()|1); err != nil {
		return nil, err
	}
	return func() { storeChangeWord(segPath, lockF, rand.Uint64()&^1) }, nil
}

// storeChangeWord stores v through f, or through segPath's lock file when f is
// nil -- which it does not create: with no lock file no session has the file
// open (opening one creates it), so there is no one to warn, and a converter's
// or a VACUUM INTO's output gains no sidecar.
func storeChangeWord(segPath string, f *os.File, v uint64) error {
	if f == nil {
		var err error
		if f, err = os.OpenFile(segPath+segmentLockSuffix, os.O_RDWR, 0); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer f.Close()
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := fsio.WriteAt(f, b[:], changeWordOff)
	return err
}

// changeWord is a session's mapping of its lock file's change word, or on js,
// where nothing maps, the lock file it reads the word from.
type changeWord struct {
	m *mmapfile.File
	f *os.File
}

// mapChangeWord maps segPath's change word, or answers nil where it cannot be
// read without a system call: no word written yet, or a platform where the
// "mapping" would be a copy (mmapfile reads the file there), which would never
// see another connection's store.
func mapChangeWord(segPath string) *changeWord {
	// On js one positioned read of the word replaces the two stamps a
	// statement would otherwise take, each a call out to the host.
	if runtime.GOOS == "js" {
		f, err := os.Open(segPath + segmentLockSuffix)
		if err != nil {
			return nil
		}
		var b [8]byte
		if n, err := fsio.ReadAt(f, b[:], changeWordOff); err != nil || n != 8 {
			f.Close()
			return nil // no word written yet
		}
		return &changeWord{f: f}
	}
	// Where nothing maps, Open reads the whole file to answer nil, and a
	// session without a word asks again at every commit.
	if !mmapfile.CanMapAt {
		return nil
	}
	m, err := mmapfile.Open(segPath + segmentLockSuffix)
	if err != nil {
		return nil
	}
	if !m.Mapped() || m.Len() < changeWordOff+8 {
		m.Close()
		return nil
	}
	return &changeWord{m: m}
}

// load reads the word. The mapping is page-aligned, so the word is 8-aligned
// and the load is one access; a store racing it from another process gives
// either value, or a mix of the two that equals neither.
func (w *changeWord) load() uint64 {
	if w.f != nil {
		// A failed or short read is an odd value, which no one records: the
		// session checks in full.
		var b [8]byte
		if n, err := fsio.ReadAt(w.f, b[:], changeWordOff); err != nil || n != 8 {
			return 1
		}
		return binary.LittleEndian.Uint64(b[:])
	}
	word := w.m.Data()[changeWordOff : changeWordOff+8]
	if !segLittleEndian {
		return binary.LittleEndian.Uint64(word)
	}
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(unsafe.SliceData(word))))
}

func (w *changeWord) close() {
	if w == nil {
		return
	}
	if w.f != nil {
		w.f.Close()
		return
	}
	w.m.Close()
}
