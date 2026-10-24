package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

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
