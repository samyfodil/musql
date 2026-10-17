package engine

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/samyfodil/musql/internal/fsio"
)

// A WRITTEN-AHEAD DELTA TAIL. A commit that grows a file pays the filesystem
// for the growth: on ext4 a 600 KB append with fsync costs 10.7 ms, and with
// fdatasync it is the same, because the new size is metadata the read needs.
// Written into bytes the file already has, it is 2.8 ms. fallocate does not
// help (10.6 ms): its extents are "unwritten" and converting them is the same
// journal commit. So a rewrite leaves a delta whose space is already written,
// as zeros, and commits write their batches into it.
//
// Zeros end the log (replaySegDelta), so the tail is invisible to every reader;
// what changes is that the delta's SIZE no longer moves with an append, which
// three things relied on and no longer do:
//   - the append point's check for another writer (appendSegmentDeltaAt);
//   - a session's "nothing touched the pair" stamp (pairStamp's probe);
//   - every size the compaction policy measures (deltaLogicalSize).

// deltaTailWanted is false on js/wasm, whose files are memory: there is no
// journal commit to avoid, and the tail would be resident.
const deltaTailWanted = runtime.GOOS != "js"

// deltaTailSize is how much is written ahead for a segment file of segSize
// bytes: a sixteenth of it, well under the compaction threshold (a quarter of
// the file), so the policy is never pushed by it. An eighth fit the same
// commits but cost 12% more disk than the file it served.
func deltaTailSize(segSize int64) int64 {
	t := min(max(segSize/16, 16<<10), 8<<20)
	return (t + 4095) &^ 4095
}

// writeDeltaTail creates segPath's delta as a header naming (baseCtr,
// basePages) and size zero bytes, written in full and synced before it is
// renamed into place, so a crash leaves either this delta or none -- never a
// torn header, which would make the pair unreadable.
//
// It is an optimisation: a caller that has already committed ignores its
// error, and the next commit creates the delta the slower way.
func writeDeltaTail(segPath string, baseCtr, basePages uint32, size int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(segPath), filepath.Base(segPath)+".delta-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op once the rename has taken it
	werr := writeZeroTail(tmp, segDeltaHeader(baseCtr, basePages), size)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if err := fsio.Rename(tmpPath, segDeltaPath(segPath)); err != nil {
		return err
	}
	syncDeltaDir(segPath)
	return nil
}

func writeZeroTail(f *os.File, hdr []byte, size int64) error {
	if _, err := fsio.WriteAt(f, hdr, 0); err != nil {
		return err
	}
	zeros := make([]byte, min(size, 1<<20))
	for off := int64(0); off < size; off += int64(len(zeros)) {
		if _, err := fsio.WriteAt(f, zeros[:min(int64(len(zeros)), size-off)], int64(len(hdr))+off); err != nil {
			return err
		}
	}
	return fsio.Sync(f)
}
