package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/samyfodil/musql/internal/fsio"
)

// FormatVersions is the segment file format this build writes, the oldest one
// it reads, and the delta log format it writes.
func FormatVersions() (file, minFile, delta uint32) {
	return segFileVersion, segFileMinVersion, segDeltaVersion
}

// ErrStateMoved is InstallSegmentFile's refusal of a database that committed
// after the caller read its state.
var ErrStateMoved = errors.New("engine: the database changed since its state was read")

// InstallSegmentFile makes the segment file at src the database at path, in
// place of what path holds, as a commit after path's last: src's change counter
// is raised past path's, so every session reloads at its next statement, as
// after another connection's commit, and a transaction begun on the replaced
// state cannot commit over it. counter is path's state as the caller read it
// (CommitState); a commit since is ErrStateMoved.
//
// src must have no delta. path's delta is removed before src is renamed in: a
// crash between the two leaves path's segment file alone, an older state, and
// src where it was, and calling InstallSegmentFile again finishes the install.
func InstallSegmentFile(path, src string, counter uint32) error {
	if fi, err := os.Stat(segDeltaPath(src)); err == nil && fi.Size() > 0 {
		return fmt.Errorf("engine: %s has a delta; compact it before installing it", src)
	}
	return withSegmentWriteLock(path, func() error {
		done, err := segChangeBegin(path)
		if err != nil {
			return err
		}
		defer done()
		cur, err := commitStateUnlocked(path)
		if err != nil {
			return err
		}
		if cur.Counter != counter {
			return ErrStateMoved
		}
		ctr, _, err := SegmentFileBase(src)
		if err != nil {
			return err
		}
		if ctr <= cur.Counter {
			if err := setSegFileCounter(src, cur.Counter+1); err != nil {
				return err
			}
		}
		if err := fsio.Remove(segDeltaPath(path)); err != nil && !os.IsNotExist(err) {
			return err
		}
		syncDir(path)
		if err := fsio.Rename(src, path); err != nil { // see fsio.Rename
			return err
		}
		return syncDir(path)
	})
}

// PinnedFiles is a database's committed state held open: its segment file,
// then the first DeltaLen bytes of its delta, readable whatever the database
// commits or compacts after, and the formats the two were written in.
type PinnedFiles struct {
	Seg, Delta                *os.File // Delta is nil when the state has none
	SegSize, DeltaLen         int64
	Counter                   uint32
	FileVersion, DeltaVersion uint32
}

// Close closes the files.
func (p *PinnedFiles) Close() error {
	err := p.Seg.Close()
	if p.Delta != nil {
		err = errors.Join(err, p.Delta.Close())
	}
	return err
}

// openFilesStopRenames is whether a file held open cannot be renamed over or
// removed, as on Windows.
const openFilesStopRenames = runtime.GOOS == "windows"

// PinCommittedFiles opens path's committed state, holding the shared lock so
// that no commit lands meanwhile. What is open stays that state: a commit after
// appends to the delta past DeltaLen, and a compaction replaces both files by
// rename. The files are opened, not linked: a session trusts a descriptor of
// its own while the file has a name (heldStamp), so another name would hide the
// rename from it. Where a file held open would stop the rename (Windows), the
// state is copied into dir instead, under the lock, and the copies are open.
func PinCommittedFiles(path, dir string) (*PinnedFiles, error) {
	p := &PinnedFiles{}
	err := withSegmentReadLock(path, func() error {
		info, err := commitStateUnlocked(path)
		if err != nil {
			return err
		}
		p.Counter, p.DeltaLen = info.Counter, info.DeltaEnd
		if p.Seg, err = pinFile(path, filepath.Join(dir, filepath.Base(path)), -1); err != nil {
			return err
		}
		if p.DeltaLen > 0 {
			p.Delta, err = pinFile(segDeltaPath(path), segDeltaPath(filepath.Join(dir, filepath.Base(path))), p.DeltaLen)
		}
		return err
	})
	if err == nil {
		var fi os.FileInfo
		if fi, err = p.Seg.Stat(); err == nil {
			p.SegSize = fi.Size()
			p.FileVersion, err = headerVersion(p.Seg, segFileMagic)
		}
	}
	if err == nil && p.Delta != nil {
		p.DeltaVersion, err = headerVersion(p.Delta, segDeltaMagic)
	}
	if err != nil {
		if p.Seg != nil {
			p.Close()
		}
		return nil, err
	}
	return p, nil
}

// pinFile opens the file at path, or where openFilesStopRenames, copies its
// first n bytes (all of them for n < 0) to cp and opens the copy.
func pinFile(path, cp string, n int64) (*os.File, error) {
	if !openFilesStopRenames {
		return os.Open(path)
	}
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := os.Create(cp)
	if err != nil {
		return nil, err
	}
	var r io.Reader = src
	if n >= 0 {
		r = io.LimitReader(src, n)
	}
	if _, err := io.Copy(dst, r); err != nil {
		dst.Close()
		return nil, err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		dst.Close()
		return nil, err
	}
	return dst, nil
}

// headerVersion is the format version in the header of f, which starts with
// magic.
func headerVersion(f *os.File, magic string) (uint32, error) {
	var b [8]byte
	if _, err := f.ReadAt(b[:], 0); err != nil {
		return 0, err
	}
	if string(b[:4]) != magic {
		return 0, fmt.Errorf("engine: %s: bad magic %q", f.Name(), b[:4])
	}
	return binary.LittleEndian.Uint32(b[4:]), nil
}

// setSegFileCounter rewrites the change counter in the header of the segment
// file at path, which nothing has mapped, and makes it durable.
func setSegFileCounter(path string, ctr uint32) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], ctr)
	if _, err := f.WriteAt(b[:], 16); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
