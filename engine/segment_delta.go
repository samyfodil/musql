package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/samyfodil/musql/internal/fsio"
	"github.com/samyfodil/musql/internal/fsstamp"
	"github.com/samyfodil/musql/internal/mmapfile"
)

// The DELTA half of this format: what a write appends so a persisted
// segment file survives it.
//
// docs/format-design.md section 4 names the shape and the reason. Columnar
// layouts are bad at row-at-a-time writes, so a write does not touch the
// columnar segments at all -- it appends a ROW-MAJOR record to a log beside
// them, and a read merges the two. Compaction (segment_compact.go) folds the log
// back into segments and empties it.
//
// Before this, a persisted ".musq" recorded its source's change counter and page
// count and REFUSED to attach once the database had moved on (errSegmentsStale),
// so any write invalidated the file entirely. That is what stage 4 was: not a
// missing optimisation, a format that could not survive being written to.
//
// ---- the layout, and why it is the WAL's ----
//
//	header  32 bytes: magic, version, and the SEGMENT FILE THIS EXTENDS, named
//	        by that file's own (srcChangeCounter, srcPageCount) -- so a delta can
//	        never be paired with a segment file it was not built against
//	batches one per commit, each ending in the state it brings the pair TO:
//
//	        nBytes    uint32   length of the record area
//	        records   nBytes
//	        endCtr    uint32   the database's change counter after this commit
//	        endPages  uint32   ...and its page count
//	        cksum     uint64   CUMULATIVE over every preceding batch
//
// The batch trailer is a COMMIT MARKER and the checksum is cumulative, both
// deliberately copied from the WAL, because the failure modes are identical: a
// writer killed mid-append leaves a partial batch, and a reader must stop at the
// last COMPLETE one rather than at the last byte it can parse. Cumulative means
// the newest good batch's checksum commits to the whole prefix, so a torn tail
// cannot be mistaken for a shorter history. (See engine/wal_scan.go for the
// limit of that property -- it detects a TRUNCATION, not an in-place corruption
// of bytes already read, which is why replay re-verifies every batch rather than
// caching where it got to.)
//
// ---- what a reader does with it ----
//
// Replaying the log gives an OVERLAY per table: the current value of every rowid
// the log touched, plus the set of rowids it removed. Last write to a rowid
// wins, which is what makes the log append-only -- an UPDATE appends rather than
// rewriting, and the replay resolves it.
//
// A read then merges overlay and segments in ROWID ORDER, because that is the
// order a b-tree walk produces and every caller of a scan already depends on it.

const (
	segDeltaMagic = "MQSD"
	// segDeltaVersion 2 widened a batch's length prefix from 4 bytes to 8: one
	// commit is one batch (its trailer is the commit marker), so a commit of 4 GiB
	// or more wrapped the length. A version-1 delta is still read and appended to
	// with its 4-byte prefix. Version 3 adds the segment record
	// (segDeltaSeg); its batches are otherwise version 2's. Version 4 changes
	// only the checksum (segDeltaChecksum). Version 5 adds the patch record
	// (segDeltaPatch). Every new delta is version 5.
	segDeltaVersion   = 5
	segDeltaHdrSize   = 32
	segDeltaSuffix    = ".delta"
	segDeltaTrailerSz = 16 // endCtr + endPages + cksum
)

// errDeltaNotForThisFile is returned when a delta names a different segment file
// than the one being attached.
var errDeltaNotForThisFile = errors.New("engine: segment delta was built against a different segment file")

// Record kinds. A tombstone carries no values.
//
// A segment record (version 3) carries a whole segment of one table's rows,
// every rowid above any the table had before it (insert_append_delta.go), so it
// conflicts with no earlier record: kind, table index, first rowid, the
// segment's length, a pad byte count p, p zero bytes so the segment starts
// 8-aligned in the file, then the segment as buildSegment lays it out.
//
// A patch record (version 5) is a put of some columns only, over the row the
// log holds for the rowid or, failing that, the committed row: kind, table
// index, rowid, the body's length, then the body -- how many columns, each
// column's index (uvarints), and their values as one record (encodeRecord).
// It is written only over a committed row of the table's full width (an
// UPDATE's row written in place, SegDeltaRecord.patch), so the row it lands
// on always has every column it names.
const (
	segDeltaPut   = 1
	segDeltaKill  = 2
	segDeltaSeg   = 3
	segDeltaPatch = 4
)

// segDeltaSegHdr is a segment record's bytes before its padding.
const segDeltaSegHdr = 13 + 4 + 1

// segDeltaSegPad is the padding a segment record at file offset pos needs.
func segDeltaSegPad(pos int64) int64 { return (8 - (pos+segDeltaSegHdr)%8) % 8 }

// SegDeltaRecord is one row mutation, as the delta stores it.
//
// Vals is the row exactly as a scan must hand it back -- the stored row, with
// the IPK column left NULL the way the b-tree record does, because a merged scan
// feeds the same consumers a segment scan does.
type SegDeltaRecord struct {
	Table int // index into the segment file's table directory
	Rowid int64
	Kill  bool
	Vals  []Value
	// Seg, when set, makes this a segment record (segDeltaSeg): Rowid is its
	// first rowid, Vals is unused.
	Seg []byte

	// patch, when set on a put, is the columns the put changed (true) over the
	// committed row: a version-5 delta writes only those (segDeltaPatch).
	patch []bool

	// src, when set, is the row store the put's row is read from WHILE the batch
	// is written, instead of Vals: a commit then holds one row at a time rather
	// than every row it writes, and a spilled row's record is copied as it lies
	// in the spill file (rowStore.recordOf). ipk is its table's INTEGER PRIMARY
	// KEY column, written NULL (the rowid carries it), or -1.
	src *rowStore
	ipk int

	// size is recordLen plus one, once measured: a commit measures every put
	// to decide whether it rewrites the file (compactionDueAfter) and again
	// to lay the batch out (writeSegDeltaBatch).
	size int
}

// recordLen is the length of the put's record.
func (r *SegDeltaRecord) recordLen() int {
	if r.size > 0 {
		return r.size - 1
	}
	var n int
	if r.src != nil {
		n = r.src.recordLen(uint64(r.Rowid), r.ipk)
	} else {
		n = recordSize(r.Vals)
	}
	r.size = n + 1
	return n
}

// patched reports whether a version-v delta writes this put as a patch record
// (segDeltaPatch) rather than its whole row.
func (r *SegDeltaRecord) patched(v uint32) bool {
	return v >= 5 && r.patch != nil && !r.Kill && r.src == nil && r.Seg == nil && len(r.patch) == len(r.Vals)
}

// patchValues is the values a patch record carries, in column order, in
// scratch's storage, and how many bytes the column list before them takes.
func (r *SegDeltaRecord) patchValues(scratch *[]Value) ([]Value, int) {
	vals := (*scratch)[:0]
	n := 0
	for c, set := range r.patch {
		if set {
			vals = append(vals, r.Vals[c])
			n += uvarintLen(uint64(c))
		}
	}
	*scratch = vals
	return vals, n + uvarintLen(uint64(len(vals)))
}

// uvarintLen is how many bytes binary.AppendUvarint writes for x.
func uvarintLen(x uint64) int {
	n := 1
	for ; x >= 0x80; x >>= 7 {
		n++
	}
	return n
}

// patchLen is the length of the put's patch record body.
func (r *SegDeltaRecord) patchLen(scratch *[]Value) int {
	vals, head := r.patchValues(scratch)
	return head + recordSize(vals)
}

// appendPatchTo appends the put's patch record body to dst.
func (r *SegDeltaRecord) appendPatchTo(dst []byte, scratch *[]Value) []byte {
	vals, _ := r.patchValues(scratch)
	dst = binary.AppendUvarint(dst, uint64(len(vals)))
	for c, set := range r.patch {
		if set {
			dst = binary.AppendUvarint(dst, uint64(c))
		}
	}
	return appendRecord(dst, vals)
}

// appendRecordTo appends the put's record to dst.
func (r *SegDeltaRecord) appendRecordTo(dst []byte) []byte {
	if r.src != nil {
		return r.src.appendRecordOf(dst, uint64(r.Rowid), r.ipk)
	}
	return appendRecord(dst, r.Vals)
}

// segDeltaState is a replayed delta: per table, the rows it has and the rowids
// it removed, plus the database state the last complete batch brought it to.
type segDeltaState struct {
	// live is the current value of every rowid the log put, per table index.
	live map[int]map[int64][]Value
	// dead is every rowid the log removed, per table index. A rowid that was
	// killed and later put is in live and not here; the reverse is also true.
	dead map[int]map[int64]bool

	endCtr   uint32
	endPages uint32
	// cksum is the running checksum through the last complete batch, which the
	// next append continues from.
	cksum uint64
	// bytes is how far the last complete batch reached, which is where the next
	// append starts and where a torn tail is truncated back to.
	bytes int64
	// batches is how many complete batches were replayed, which is what a
	// compaction policy counts.
	batches int
	// version is the delta's format (segDeltaVersion), which fixes the width of
	// every batch's length prefix (segDeltaLenWidth).
	version uint32

	// mapping, when set, is the mapped delta the replayed rows alias: they decode
	// with their bytes left in place (decodeRecord), so the log is never on the
	// heap. It lives exactly as long as the pager that holds this state, and is
	// released with the segment file's own mapping (segFileCloser) -- so a delta
	// row is covered by every protection a segment row already has
	// (detachRowsFromMapping, unloading before a close). Safe against a writer
	// appending meanwhile: a replay references only complete batches, and a writer
	// writes and truncates only past the last complete one.
	mapping *mmapfile.File

	// base, when set, is the committed row of table ti at rowid as the segment
	// file holds it: what a patch record lands on when the log holds no row
	// for its rowid. A replay that wants only where the log ends
	// (countersOnly) has none and skips patches.
	base         func(ti int, rowid int64) ([]Value, bool)
	countersOnly bool

	// segs is each table's segments that segment records carried, in log
	// order, every rowid above any earlier record's: buildSegmentPager reads
	// them after the file's own segments. They alias the mapping when there is
	// one, exactly as live's rows do.
	segs map[int][]*segment

	// posMu guards posCache, namedPositions' kept answer for one table (see
	// segment_delta_merge.go): a statement's parallel scans share this state.
	posMu    sync.Mutex
	posCache *namedPosCache
}

// release unmaps the delta this state's rows alias. Safe on nil and twice.
func (st *segDeltaState) release() {
	if st != nil && st.mapping != nil {
		st.mapping.Close()
		st.mapping = nil
	}
}

// mapSegDelta maps the delta at path, or answers nil for one that does not
// exist. The caller releases it.
func mapSegDelta(path string) (*mmapfile.File, []byte, error) {
	m, err := mmapfile.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return m, m.Data(), nil
}

// segDeltaLenWidth is a batch's length-prefix width in a delta of version v.
func segDeltaLenWidth(v uint32) int64 {
	if v == 1 {
		return 4
	}
	return 8
}

// rowsFor is the overlay's live rows for one table, sorted by rowid so a merged
// scan can walk it alongside the segments.
func (st *segDeltaState) rowsFor(table int) []segOverlayRow {
	m := st.live[table]
	if len(m) == 0 {
		return nil
	}
	out := make([]segOverlayRow, 0, len(m))
	for rowid, vals := range m {
		out = append(out, segOverlayRow{rowid: rowid, vals: vals})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rowid < out[j].rowid })
	return out
}

// segOverlayRow is one delta row, ready to merge.
type segOverlayRow struct {
	rowid int64
	vals  []Value
}

// segDeltaPath is the delta beside a segment file.
func segDeltaPath(segPath string) string { return segPath + segDeltaSuffix }

// writeSegDeltaHeader creates an empty delta for the segment file identified by
// (baseCtr, basePages).
func writeSegDeltaHeader(path string, baseCtr, basePages uint32) error {
	return os.WriteFile(path, segDeltaHeader(baseCtr, basePages), 0o644)
}

// segDeltaHeader is a delta's header for the segment file at (baseCtr, basePages).
func segDeltaHeader(baseCtr, basePages uint32) []byte {
	hdr := make([]byte, segDeltaHdrSize)
	copy(hdr[0:4], segDeltaMagic)
	binary.LittleEndian.PutUint32(hdr[4:], segDeltaVersion)
	binary.LittleEndian.PutUint32(hdr[8:], baseCtr)
	binary.LittleEndian.PutUint32(hdr[12:], basePages)
	binary.LittleEndian.PutUint64(hdr[24:], segDeltaChecksum(segDeltaVersion, hdr[:24], 0))
	return hdr
}

// segDeltaChecksum is the running checksum of a version-v delta, seeded from
// the previous value. It needs to notice a torn append, and it needs the value
// to depend on every preceding byte; it does not need to be cryptographic.
//
// Version 4 is CRC-32C, which the CPU computes many bytes at a time. Versions 1
// to 3 are FNV-1a 64 a byte at a time, one dependent multiply per byte: a few
// percent of an UPDATE of thousands of rows, and of every replay. Both are
// chained across calls, so a batch summed in pieces sums as it would whole.
func segDeltaChecksum(v uint32, b []byte, seed uint64) uint64 {
	if v >= 4 {
		return uint64(crc32.Update(uint32(seed), segDeltaCRC, b))
	}
	h := seed
	if h == 0 {
		h = 14695981039346656037
	}
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

var segDeltaCRC = crc32.MakeTable(crc32.Castagnoli)

// SegDeltaAppendState is where the last append left off: the byte offset the next
// batch starts at and the running checksum it chains from.
//
// A writer that holds the file for its whole session carries this instead of
// re-deriving it. Without it every append read and REPLAYED the entire delta to
// find its own tail, which is O(log) per commit and quadratic over a run of them
// -- the same shape that made the WAL's commit path slow (engine/wal_scan.go).
//
// It is sound here in the way it was NOT sound there: these are bytes this
// session just wrote, and the state is only ever advanced by the append that
// produced them. A zero value means "unknown", and the append derives it the slow
// way exactly once.
type SegDeltaAppendState struct {
	offset  int64
	cksum   uint64
	version uint32
	known   bool

	// f is the delta held open across commits, and fi what it was when opened:
	// a commit that finds the path still naming that file (os.SameFile) at the
	// carried length writes through it instead of opening the file twice --
	// once to read the tail, once to write -- each open costing a path lookup
	// and the runtime's poller registration. Not kept on Windows, where an open
	// handle stops another process renaming or deleting the file.
	f  *os.File
	fi os.FileInfo
	// lockF is the lock file held open for the change word (segment_change.go):
	// two stores a commit, which an open and a close each would have doubled
	// the cost of. Never renamed, so nothing to re-verify; not kept on Windows,
	// for f's reason.
	lockF *os.File

	// lastBatch is the records of the batch the last append wrote, when it fit
	// in one chunk; nil otherwise. Session.commitMainLocked applies it to the
	// committed read view instead of re-reading the whole pair.
	lastBatch []byte
	// buf is the buffer the session's commits encode their batches into
	// (writeSegDeltaBatch's encBuf), kept from one to the next.
	buf []byte
}

// release closes the held delta, if any, and forgets the append point.
func (c *SegDeltaAppendState) release() {
	if c.f != nil {
		c.f.Close()
	}
	if c.lockF != nil {
		c.lockF.Close()
	}
	*c = SegDeltaAppendState{}
}

// appendSegmentDeltaAt is AppendSegmentDelta carrying the append point forward.
func appendSegmentDeltaAt(segPath string, baseCtr, basePages uint32, recs []SegDeltaRecord, endCtr, endPages uint32, carry *SegDeltaAppendState, sync syncMode) error {
	if len(recs) == 0 {
		return nil
	}
	// Every change to the pair is announced in the lock file's change word
	// (segment_change.go), so a reader's freshness check can skip the stamps.
	if carry != nil && carry.lockF == nil && runtime.GOOS != "windows" {
		carry.lockF, _ = openSegmentLockFile(segPath) // nil: stored by path below
	}
	var lockF *os.File
	if carry != nil {
		lockF = carry.lockF
	}
	endChange, cerr := segChangeBeginOn(segPath, lockF)
	if cerr != nil {
		return cerr
	}
	defer endChange()
	path := segDeltaPath(segPath)
	// The held descriptor answers for the path while its file is still linked
	// (fstat: no lookup, no allocation); otherwise the path is stat'ed.
	var fileSize int64
	held := false
	if carry != nil && carry.f != nil {
		if size, _, linked, ok := fsstamp.OfFile(carry.f.Fd()); ok && linked {
			fileSize, held = size, true
		}
	}
	var fi os.FileInfo
	var err error
	created := false
	if !held {
		fi, err = os.Stat(path)
		if err == nil {
			fileSize = fi.Size()
		}
	}
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if werr := writeSegDeltaHeader(path, baseCtr, basePages); werr != nil {
			return werr
		}
		created = true
		if carry != nil {
			carry.known = false // the file is new; derive below
		}
		fi, fileSize = nil, 0
	}
	exists := held || fi != nil
	// The held descriptor is good only while the path still names its file.
	var f *os.File
	if carry != nil && carry.f != nil {
		if held || (fi != nil && carry.fi != nil && os.SameFile(fi, carry.fi)) {
			f = carry.f
		} else {
			carry.f.Close()
			carry.f, carry.fi = nil, nil
		}
	}
	// THE APPEND POINT IS DERIVED FROM THE BYTES, every time.
	//
	// A carried "where I last wrote" is only sound while nothing else has written,
	// and that cannot be established from the file's SIZE: another writer can leave
	// it the same length with different bytes, and the carried CHECKSUM then belongs
	// to a chain that no longer exists. The batch written from it carries a checksum
	// a later replay rejects, replay stops there, and the next append truncates every
	// good batch after that point away.
	//
	// That is the same lesson as caching the WAL's scan (engine/wal_scan.go), and
	// this file learned it the same way: two replicated nodes, and the node
	// that inserted a row lost it while its peer kept it.
	//
	// The caller holds the segment-file write lock (withSegmentWriteLock), so the
	// state read here is still the state written to.
	//
	// ONCE, not twice: this used to read and replay the delta HERE and then again
	// after the batch was built, which is the same answer for double the work on
	// the hot path. It has to come before the batch, because the batch's checksum
	// chains onto this state's.
	//
	// AND, when the carry can be VERIFIED, not at all. The replay is O(delta) per
	// commit and therefore quadratic over a run of them -- measured at 450us per
	// commit on the 500-commit segment-vs-C write bench, against C's 7us WAL append
	// -- so the common case has to be O(1) without giving up what the replay was
	// reinstated for.
	//
	// What it was reinstated for: another writer appending between our appends.
	// TWO checks, and they are not equal in strength -- said plainly because the
	// weaker one is the one that actually fires:
	//
	//   - the file's SIZE against the carried offset. An append only ever GROWS the
	//     delta, so any other writer's batch moves the length and this catches it.
	//     A mutation test confirms it is the one doing the work: removing the
	//     trailer check below leaves the concurrent-writer test green.
	//   - the last batch's TRAILER against the carried checksum. Every batch ends
	//     with the running checksum of the whole chain up to and including itself,
	//     so this catches a same-length REPLACEMENT of our own last batch, which the
	//     size check cannot. Kept although it is not reachable through this
	//     protocol today -- a writer overwrites only at its own replay's stop point,
	//     and a batch this session wrote and verified is not one -- because it costs
	//     an 8-byte read and it is the difference between "cannot happen" and
	//     "cannot happen silently".
	//
	// What NEITHER sees, stated rather than implied: bit-rot INSIDE a batch this
	// session already replayed and verified, which leaves both the length and the
	// tail trailer untouched. The full replay would notice and would then overwrite
	// the damaged tail -- discarding committed batches to do it. So neither answer
	// survives a corrupted delta; what this one gives up is detecting the damage at
	// append time rather than at the next open, and what it buys is a commit that
	// does not re-read its own history.
	var st *segDeltaState
	//
	// A WRITTEN-AHEAD TAIL (writeDeltaTail) leaves the file's size where it was,
	// so the size check above cannot see another writer's batch inside it. What
	// does: no batch is empty, so the 8 bytes at the carried offset -- where the
	// next batch's length prefix would go -- are zero only while nobody has
	// appended there. Every append leaves them zero after its own batch.
	if carry != nil && carry.known && carry.offset > int64(segDeltaHdrSize) && exists && fileSize >= carry.offset {
		vf := f
		if vf == nil {
			vf, _ = os.Open(path)
			if vf != nil {
				defer vf.Close()
			}
		}
		if vf != nil {
			var tail [16]byte
			n := min(fileSize-carry.offset, 8)
			_, rerr := fsio.ReadAt(vf, tail[:8+n], carry.offset-8)
			if rerr == nil && binary.LittleEndian.Uint64(tail[:8]) == carry.cksum && binary.LittleEndian.Uint64(tail[8:]) == 0 {
				st = &segDeltaState{bytes: carry.offset, cksum: carry.cksum,
					endCtr: endCtr, endPages: endPages, version: carry.version}
			}
		}
	}
	if st == nil {
		// MAPPED, and released before return: only the replay's counters are kept.
		m, data, rerr := mapSegDelta(path)
		if rerr != nil {
			return rerr
		}
		var serr error
		st, serr = replaySegDelta(data, baseCtr, basePages, nil) // where it ends only
		if m != nil {
			defer m.Close()
		}
		if serr != nil {
			return serr
		}
	}

	if f == nil {
		f, err = os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		ofi, serr := f.Stat()
		if carry != nil && serr == nil && runtime.GOOS != "windows" {
			carry.f, carry.fi = f, ofi
		} else {
			defer f.Close()
		}
	}

	// ONE WRITER AT A TIME, for the whole read-decide-write: the caller holds the
	// segment-file write lock (withSegmentWriteLock), so this function takes none
	// of its own -- locking the DELTA as well would be a second lock ordering for
	// no gain, and the delta does not always exist for a rewrite to lock.
	//
	// At st.bytes, not at the file's end: a previous torn append is OVERWRITTEN
	// rather than kept, exactly as a WAL writer overwrites an aborted tail.
	if st.version < 3 && hasSegRecord(recs) {
		return errDeltaPredatesSegments
	}
	var keep *[]byte
	if carry != nil {
		keep = &carry.lastBatch
	}
	var encBuf *[]byte
	if carry != nil {
		encBuf = &carry.buf
	}
	end, cksum, werr := writeSegDeltaBatch(f, st.bytes, st.cksum, st.version, recs, endCtr, endPages, keep, encBuf)
	if werr != nil {
		return werr
	}
	// WHAT LIES PAST THE BATCH must not read as one: a torn earlier append, or
	// the written-ahead tail. The tail is zeros already, and zeros end the log
	// (replaySegDelta); a torn append is overwritten with zeros here, up to 8
	// bytes -- the next length prefix, which is all a replay or the next append's
	// check reads. The file is never cut back, so a tail written ahead stays.
	if end < fileSize {
		var zero [8]byte
		if _, err := fsio.WriteAt(f, zero[:min(fileSize-end, 8)], end); err != nil {
			return err
		}
	}
	// DURABILITY IS THE CALLER'S CHOICE, and naming it is the only way a
	// comparison against C SQLite means anything: C's WAL default
	// (synchronous=NORMAL) does NOT fsync a commit, so a segment commit that does
	// is strictly more durable and strictly slower. syncNone is how a caller asks
	// for C's WAL contract -- the batch is still atomic (its trailer is the commit
	// marker and a torn tail is excluded on replay), it is just not yet on the
	// platter, so a power loss can lose the last commits. syncFull is C's
	// "PRAGMA fullfsync" (fsyncFile).
	//
	// fdatasync, not fsync, where the platform has one: a batch written into the
	// tail changes no metadata a read needs, and fsync would also commit the
	// mtime -- a filesystem journal commit, which is most of what a growing
	// append costs (measured on ext4: 10.7 ms growing, 2.8 ms into written
	// bytes, for 600 KB). Growth still syncs the size: fdatasync covers it.
	if sync != syncNone {
		var err error
		if sync == syncFull {
			err = fsyncFile(f, true)
		} else {
			err = datasyncFile(f)
		}
		if err != nil {
			return err
		}
		// A DELTA THIS APPEND CREATED is reachable only through its directory
		// entry, and fsyncing the file does not make the entry durable: a power
		// loss could drop the whole file, and with it a commit already reported.
		// C fsyncs the directory after creating a rollback journal for the same
		// reason (unixSync's dirsync, os_unix.c).
		if created {
			syncDeltaDir(segPath)
		}
	}
	if carry != nil {
		carry.offset, carry.cksum, carry.version, carry.known = end, cksum, st.version, true
	}
	return nil
}

// syncDeltaDir is syncDir, as a variable so a test can see the call.
var syncDeltaDir = syncDir

// segDeltaChunk is how much of a batch is encoded before it is written: the
// commit holds this much, not the batch.
const segDeltaChunk = 1 << 20

// writeSegDeltaBatch writes recs as one batch at off, chaining its checksum from
// seed, and returns where it ends and the chain's new value.
//
// STREAMED, in segDeltaChunk pieces: the batch used to be built whole in memory
// before one write, so a commit held a second copy of everything it wrote -- a
// 2.83 GB insert (bigsort.test 1.0) peaked at 23 GB of RSS between this and the
// compaction after it. The batch's LENGTH leads it and the checksum chains from
// that first byte, so the total is computed first (recordSize), and every
// record's encoding is checked against its prediction: a mismatch fails the
// commit rather than writing a batch whose length lies.
//
// encBuf, when not nil, is a buffer to encode into, which the batch hands
// back for the next.
func writeSegDeltaBatch(f *os.File, off int64, seed uint64, version uint32, recs []SegDeltaRecord, endCtr, endPages uint32, keep, encBuf *[]byte) (end int64, cksum uint64, err error) {
	lenWidth := segDeltaLenWidth(version)
	var n int64
	var scratch []Value // a patch record's values, gathered (patchValues)
	// Each put's record length, measured once; on the stack for a small batch.
	var sizesBuf [16]int32
	sizes := sizesBuf[:0]
	if len(recs) > len(sizesBuf) {
		sizes = make([]int32, 0, len(recs))
	}
	sizes = sizes[:len(recs)]
	for i := range recs {
		r := &recs[i]
		if r.Seg != nil {
			n += segDeltaSegHdr + segDeltaSegPad(off+lenWidth+n) + int64(len(r.Seg))
			continue
		}
		n += 13
		switch {
		case r.patched(version):
			sizes[i] = int32(r.patchLen(&scratch))
			n += 4 + int64(sizes[i])
		case !r.Kill:
			sizes[i] = int32(r.recordLen())
			n += 4 + int64(sizes[i])
		}
	}
	if lenWidth == 4 && n > math.MaxUint32 {
		return 0, 0, fmt.Errorf("engine: segment delta: a %d-byte commit does not fit this version-1 delta's batch length; compact it first", n)
	}
	// Sized to the batch when it is smaller than a chunk: a one-row commit
	// allocated (and zeroed) the whole megabyte.
	// The buffer the session keeps for its commits (SegDeltaAppendState.buf),
	// when one is given: a pool's was dropped at every GC, so an UPDATE of
	// thousands of rows allocated a fresh one about every commit.
	want := min(n+lenWidth+segDeltaTrailerSz, segDeltaChunk+64)
	var buf []byte
	switch {
	case encBuf != nil && int64(cap(*encBuf)) >= want:
		buf = (*encBuf)[:0]
	case want < segDeltaChunk:
		buf = make([]byte, 0, want)
	default:
		buf = make([]byte, 0, segDeltaChunk+64)
	}
	if encBuf != nil {
		defer func() {
			if cap(buf) <= segDeltaChunk+64 {
				*encBuf = buf[:0]
			}
		}()
	}
	if lenWidth == 4 {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(n))
	} else {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
	}
	at, cksum := off, seed
	flush := func() error {
		cksum = segDeltaChecksum(version, buf, cksum)
		if _, err := fsio.WriteAt(f, buf, at); err != nil {
			return err
		}
		at += int64(len(buf))
		buf = buf[:0]
		return nil
	}
	written := int64(0)
	for i := range recs {
		r := &recs[i]
		if r.Seg != nil {
			var hdr [segDeltaSegHdr]byte
			hdr[0] = segDeltaSeg
			binary.LittleEndian.PutUint32(hdr[1:], uint32(r.Table))
			binary.LittleEndian.PutUint64(hdr[5:], uint64(r.Rowid))
			binary.LittleEndian.PutUint32(hdr[13:], uint32(len(r.Seg)))
			pad := segDeltaSegPad(off + lenWidth + written)
			hdr[17] = byte(pad)
			buf = append(buf, hdr[:]...)
			var zeros [8]byte
			buf = append(buf, zeros[:pad]...)
			written += segDeltaSegHdr + pad
			// The segment goes out as it is, not copied into buf.
			if err := flush(); err != nil {
				return 0, 0, err
			}
			cksum = segDeltaChecksum(version, r.Seg, cksum)
			if _, err := fsio.WriteAt(f, r.Seg, at); err != nil {
				return 0, 0, err
			}
			at += int64(len(r.Seg))
			written += int64(len(r.Seg))
			continue
		}
		var hdr [13]byte
		patched := r.patched(version)
		switch {
		case r.Kill:
			hdr[0] = segDeltaKill
		case patched:
			hdr[0] = segDeltaPatch
		default:
			hdr[0] = segDeltaPut
		}
		binary.LittleEndian.PutUint32(hdr[1:], uint32(r.Table))
		binary.LittleEndian.PutUint64(hdr[5:], uint64(r.Rowid))
		buf = append(buf, hdr[:]...)
		written += 13
		if !r.Kill {
			want := int(sizes[i])
			buf = binary.LittleEndian.AppendUint32(buf, uint32(want))
			before := len(buf)
			if patched {
				buf = r.appendPatchTo(buf, &scratch)
			} else {
				buf = r.appendRecordTo(buf)
			}
			if got := len(buf) - before; got != want {
				return 0, 0, fmt.Errorf("engine: segment delta: row %d encoded to %d bytes where %d were predicted", r.Rowid, got, want)
			}
			written += 4 + int64(want)
		}
		if len(buf) >= segDeltaChunk {
			if err := flush(); err != nil {
				return 0, 0, err
			}
		}
	}
	if written != n {
		return 0, 0, fmt.Errorf("engine: segment delta: batch wrote %d bytes where %d were predicted", written, n)
	}
	// The batch's records, for a caller that applies them to a delta state it
	// already holds (applyBatch, as a replay of this batch would). Only when no
	// chunk went out early, so buf still holds every record: a single-row
	// commit always, a bulk one never.
	if keep != nil {
		*keep = nil
		if at == off {
			*keep = append((*keep)[:0], buf[lenWidth:lenWidth+n]...)
		}
	}
	var tr [segDeltaTrailerSz]byte
	binary.LittleEndian.PutUint32(tr[0:], endCtr)
	binary.LittleEndian.PutUint32(tr[4:], endPages)
	buf = append(buf, tr[:8]...)
	// The checksum covers everything before it, so it is known once the last
	// chunk is summed, and goes out in the same write: one call, not two.
	cksum = segDeltaChecksum(version, buf, cksum)
	buf = binary.LittleEndian.AppendUint64(buf, cksum)
	if _, err := fsio.WriteAt(f, buf, at); err != nil {
		return 0, 0, err
	}
	return at + int64(len(buf)), cksum, nil
}

// segDeltaBatchBytes is the most writeSegDeltaBatch appends for recs: the wider
// length prefix, every record as it measures them, and the trailer.
func segDeltaBatchBytes(recs []SegDeltaRecord) int64 {
	n := int64(8 + segDeltaTrailerSz)
	for i := range recs {
		if recs[i].Seg != nil {
			n += segDeltaSegHdr + 7 + int64(len(recs[i].Seg)) // at most 7 pad bytes
			continue
		}
		n += 13
		if !recs[i].Kill {
			n += 4 + int64(recs[i].recordLen())
		}
	}
	return n
}

// segDeltaBatchBytesExceed reports whether the batch recs would make in a
// current (version-5) delta is over limit, measuring only until the answer is
// known: a bulk load's batch is far past the limit long before its last
// record, and measuring a record can mean encoding it. A patched put counts as
// its patch record, which is what such a delta takes (an older delta takes the
// whole row, and compacts a little later than it might). The compaction policy
// is its only caller.
func segDeltaBatchBytesExceed(recs []SegDeltaRecord, limit int64) bool {
	// Most batches are far from the limit: an upper bound from each value's
	// type and length settles that without measuring a record.
	bound := int64(8 + segDeltaTrailerSz)
	for i := range recs {
		r := &recs[i]
		switch {
		case r.Seg != nil:
			bound += segDeltaSegHdr + 7 + int64(len(r.Seg))
		case r.Kill:
			bound += segDeltaMinRecord
		case r.src != nil:
			bound = limit + 1 // nothing to bound it by: measure below
		case r.patched(segDeltaVersion):
			bound += segDeltaMinRecord + 4 + 10 // the count and a column, at most
			for c, set := range r.patch {
				if set {
					bound += 3 + int64(recordBound(r.Vals[c:c+1]))
				}
			}
		default:
			bound += segDeltaMinRecord + 4 + int64(recordBound(r.Vals))
		}
		if bound > limit {
			break
		}
	}
	if bound <= limit {
		return false
	}
	var scratch []Value
	n := int64(8 + segDeltaTrailerSz)
	for i := range recs {
		if n > limit {
			return true
		}
		r := &recs[i]
		switch {
		case r.Seg != nil:
			n += segDeltaSegHdr + 7 + int64(len(r.Seg))
		case r.Kill:
			n += segDeltaMinRecord
		case r.patched(segDeltaVersion):
			n += segDeltaMinRecord + 4 + int64(r.patchLen(&scratch))
		default:
			n += segDeltaMinRecord + 4 + int64(r.recordLen())
		}
	}
	return n > limit
}

// segDeltaBatchBound is at least the bytes a batch of recs takes in any
// version, from each value's type and length alone; false when a record has
// no values to bound (a segment, or a row read from a spilled store while the
// batch is written).
func segDeltaBatchBound(recs []SegDeltaRecord) (int64, bool) {
	n := int64(8 + segDeltaTrailerSz)
	for i := range recs {
		r := &recs[i]
		if r.Seg != nil || !r.Kill && r.src != nil {
			return 0, false
		}
		n += 13
		if !r.Kill {
			// A patch record's body is its count, the columns and a record of
			// some of the values: within the whole row's bound plus a varint
			// per column.
			n += 4 + int64(recordBound(r.Vals)+10*len(r.Vals))
		}
	}
	return n, true
}

// segDeltaMinRecord is the smallest a delta record can be: a kill's header,
// which a put's record only adds to.
const segDeltaMinRecord = 13

// syncMode is how a commit makes its batch durable.
type syncMode int

const (
	syncNone   syncMode = iota // no fsync: C's WAL synchronous=NORMAL contract
	syncNormal                 // fsync(2), what C issues by default
	syncFull                   // F_FULLFSYNC on darwin, under "PRAGMA fullfsync=ON"
)

// replaySegDelta walks a delta's complete batches and returns the overlay they
// describe. It stops at the first batch that is truncated or whose checksum
// fails, so a torn tail is ignored rather than half-applied.
//
// base is the segment file's committed rows, for patch records
// (segDeltaState.base); nil for a caller that wants only where the log ends
// and what its checksum is, which then leaves the rows patches touch out.
func replaySegDelta(data []byte, baseCtr, basePages uint32, base func(ti int, rowid int64) ([]Value, bool)) (*segDeltaState, error) {
	st := &segDeltaState{
		live:         map[int]map[int64][]Value{},
		dead:         map[int]map[int64]bool{},
		endCtr:       baseCtr,
		endPages:     basePages,
		bytes:        segDeltaHdrSize,
		base:         base,
		countersOnly: base == nil,
	}
	if len(data) < segDeltaHdrSize {
		return st, nil // absent or empty: the segment file stands alone
	}
	if string(data[0:4]) != segDeltaMagic {
		return nil, fmt.Errorf("engine: segment delta: bad magic %q", data[0:4])
	}
	st.version = binary.LittleEndian.Uint32(data[4:])
	if st.version < 1 || st.version > segDeltaVersion {
		return nil, fmt.Errorf("engine: segment delta: version %d, want 1 to %d", st.version, segDeltaVersion)
	}
	if binary.LittleEndian.Uint64(data[24:]) != segDeltaChecksum(st.version, data[:24], 0) {
		return nil, fmt.Errorf("engine: segment delta: header checksum fails")
	}
	if c, p := binary.LittleEndian.Uint32(data[8:]), binary.LittleEndian.Uint32(data[12:]); c != baseCtr || p != basePages {
		return nil, fmt.Errorf("%w (delta was built for counter %d pages %d; this file is counter %d pages %d)",
			errDeltaNotForThisFile, c, p, baseCtr, basePages)
	}
	st.cksum = segDeltaChecksum(st.version, data[:24], 0)

	w := segDeltaLenWidth(st.version)
	off := int64(segDeltaHdrSize)
	for off+w+segDeltaTrailerSz <= int64(len(data)) {
		var n int64
		if w == 4 {
			n = int64(binary.LittleEndian.Uint32(data[off:]))
		} else {
			n = int64(binary.LittleEndian.Uint64(data[off:]))
		}
		// A ZERO LENGTH ENDS THE LOG. No batch is empty (an append with no
		// records writes nothing), so a zero prefix is never history: it is the
		// unwritten tail of a delta whose space was written ahead of its batches.
		// Said here rather than left to the checksum, which zeros merely fail.
		if n == 0 {
			break
		}
		end := off + w + n + segDeltaTrailerSz
		if n < 0 || end < off || end > int64(len(data)) {
			break // truncated batch
		}
		batch := data[off:end]
		want := binary.LittleEndian.Uint64(batch[len(batch)-8:])
		if segDeltaChecksum(st.version, batch[:len(batch)-8], st.cksum) != want {
			break // torn or corrupt: this batch and everything after it is not history
		}
		if err := st.applyBatch(batch[w : w+n]); err != nil {
			return nil, err
		}
		st.endCtr = binary.LittleEndian.Uint32(batch[w+n:])
		st.endPages = binary.LittleEndian.Uint32(batch[w+n+4:])
		st.cksum = want
		st.bytes = end
		st.batches++
		off = end
	}
	return st, nil
}

// applyBatch folds one batch's records into the overlay, last write per rowid
// winning.
func (st *segDeltaState) applyBatch(body []byte) error {
	for off := 0; off < len(body); {
		if off+13 > len(body) {
			return fmt.Errorf("engine: segment delta: record header runs past the batch")
		}
		kind := body[off]
		table := int(binary.LittleEndian.Uint32(body[off+1:]))
		rowid := int64(binary.LittleEndian.Uint64(body[off+5:]))
		off += 13
		switch kind {
		case segDeltaSeg:
			if off+5 > len(body) {
				return fmt.Errorf("engine: segment delta: segment length runs past the batch")
			}
			n := int(binary.LittleEndian.Uint32(body[off:]))
			pad := int(body[off+4])
			off += 5 + pad
			if pad > 7 || n <= 0 || off+n > len(body) {
				return fmt.Errorf("engine: segment delta: segment runs past the batch")
			}
			if err := st.addSegment(table, body[off:off+n]); err != nil {
				return err
			}
			off += n
		case segDeltaKill:
			st.kill(table, rowid)
		case segDeltaPut:
			if off+4 > len(body) {
				return fmt.Errorf("engine: segment delta: value length runs past the batch")
			}
			n := int(binary.LittleEndian.Uint32(body[off:]))
			off += 4
			if n < 0 || off+n > len(body) {
				return fmt.Errorf("engine: segment delta: values run past the batch")
			}
			vals, err := decodeRecord(body[off : off+n])
			if err != nil {
				return fmt.Errorf("engine: segment delta: decoding row %d: %w", rowid, err)
			}
			off += n
			st.put(table, rowid, vals)
		case segDeltaPatch:
			if off+4 > len(body) {
				return fmt.Errorf("engine: segment delta: patch length runs past the batch")
			}
			n := int(binary.LittleEndian.Uint32(body[off:]))
			off += 4
			if n < 0 || off+n > len(body) {
				return fmt.Errorf("engine: segment delta: patch runs past the batch")
			}
			if err := st.applyPatch(table, rowid, body[off:off+n]); err != nil {
				return err
			}
			off += n
		default:
			return fmt.Errorf("engine: segment delta: unknown record kind %d", kind)
		}
	}
	return nil
}

// applyPatch applies a patch record's body (segDeltaPatch) to rowid's row: the
// log's, else the committed row (the delta's own segments, then the file's).
func (st *segDeltaState) applyPatch(table int, rowid int64, body []byte) error {
	k, w := binary.Uvarint(body)
	if w <= 0 || k == 0 || k > uint64(len(body)) {
		return fmt.Errorf("engine: segment delta: patch of row %d: bad column count", rowid)
	}
	cols := make([]int, k)
	p := w
	for i := range cols {
		c, w := binary.Uvarint(body[p:])
		if w <= 0 || c > 1<<16 {
			return fmt.Errorf("engine: segment delta: patch of row %d: bad column", rowid)
		}
		cols[i], p = int(c), p+w
	}
	vals, err := decodeRecord(body[p:])
	if err != nil || len(vals) != len(cols) {
		return fmt.Errorf("engine: segment delta: decoding patch of row %d: %v", rowid, err)
	}
	if st.countersOnly {
		return nil // only where the log ends is wanted; the row is not
	}
	row, ok := st.live[table][rowid]
	if !ok && !st.dead[table][rowid] {
		row, ok = st.committedRow(table, rowid)
	}
	if !ok {
		return fmt.Errorf("engine: segment delta: patch of row %d, which has no row to patch", rowid)
	}
	out := make([]Value, len(row))
	copy(out, row)
	for i, c := range cols {
		if c >= len(out) {
			return fmt.Errorf("engine: segment delta: patch of row %d names column %d of %d", rowid, c, len(out))
		}
		out[c] = vals[i]
	}
	st.put(table, rowid, out)
	return nil
}

// committedRow is rowid's row in the segments the log itself carried, else in
// the segment file (base).
func (st *segDeltaState) committedRow(table int, rowid int64) ([]Value, bool) {
	for _, sg := range st.segs[table] {
		if i, hit := sg.findRowid(rowid); hit {
			return sg.rowAt(i), true
		}
	}
	if st.base == nil {
		return nil, false
	}
	return st.base(table, rowid)
}

// applyRecs applies a batch's records as applyBatch would apply the batch
// written from them: each put's row exactly as decodeRecord reads it back --
// the type and that type's one field, every other field zero -- and each
// kill. false, having applied nothing, when a put reads its row from a store
// while it is written (SegDeltaRecord.src): the caller decodes the batch.
func (st *segDeltaState) applyRecs(recs []SegDeltaRecord) bool {
	total := 0
	for i := range recs {
		if recs[i].Seg != nil || (!recs[i].Kill && recs[i].src != nil) {
			return false
		}
		if !recs[i].Kill && !canonicalDeltaRow(recs[i].Vals) {
			total += len(recs[i].Vals)
		}
	}
	// The values of every row that needs rebuilding from one allocation, not
	// one per row; each capped so no row can grow into the next.
	slab := make([]Value, total)
	var live map[int64][]Value
	liveTable := -1
	for i := range recs {
		r := &recs[i]
		if r.Kill {
			st.kill(r.Table, r.Rowid)
			continue
		}
		var vals []Value
		if canonicalDeltaRow(r.Vals) {
			// Already what the rebuild below would produce, and a store's row is
			// never edited in place: the view shares it.
			vals = r.Vals[:len(r.Vals):len(r.Vals)]
		} else {
			vals = slab[:len(r.Vals):len(r.Vals)]
			slab = slab[len(r.Vals):]
			for c, v := range r.Vals {
				switch v.Typ {
				case Int:
					vals[c] = Value{Typ: Int, I: v.I}
				case Float:
					vals[c] = Value{Typ: Float, F: v.F}
				case Text, Blob:
					b := v.S[:len(v.S):len(v.S)]
					if b == nil {
						b = []byte{} // decodeRecord's empty value is a non-nil slice
					}
					vals[c] = Value{Typ: v.Typ, S: b}
				default:
					vals[c] = Value{Typ: Null}
				}
			}
		}
		// put, with the table's map looked up once per run of its rows.
		if r.Table != liveTable || live == nil {
			if st.live[r.Table] == nil {
				st.live[r.Table] = make(map[int64][]Value, len(recs)-i)
			}
			live, liveTable = st.live[r.Table], r.Table
		}
		live[r.Rowid] = vals
		// A patched put was a committed row neither put nor killed by the log
		// when its UPDATE read it, outside any transaction, so it is in no
		// dead set: only another put can be.
		if d := st.dead[r.Table]; len(d) > 0 && r.patch == nil {
			delete(d, r.Rowid)
		}
	}
	return true
}

// canonicalDeltaRow reports whether vals is already in the form a replayed
// record decodes to: every value only the fields its type uses, and a TEXT or
// BLOB a non-nil slice with no spare capacity.
func canonicalDeltaRow(vals []Value) bool {
	for i := range vals {
		v := &vals[i]
		if v.Subtype != 0 {
			return false
		}
		switch v.Typ {
		case Int:
			if v.F != 0 || v.S != nil {
				return false
			}
		case Float:
			if v.I != 0 || v.S != nil {
				return false
			}
		case Text, Blob:
			if v.I != 0 || v.F != 0 || v.S == nil || cap(v.S) != len(v.S) {
				return false
			}
		case Null:
			if v.I != 0 || v.F != 0 || v.S != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (st *segDeltaState) put(table int, rowid int64, vals []Value) {
	if st.live[table] == nil {
		st.live[table] = map[int64][]Value{}
	}
	st.live[table][rowid] = vals
	delete(st.dead[table], rowid)
}

func (st *segDeltaState) kill(table int, rowid int64) {
	delete(st.live[table], rowid)
	if st.dead[table] == nil {
		st.dead[table] = map[int64]bool{}
	}
	st.dead[table][rowid] = true
}

// empty reports whether the overlay changes nothing, in which case a reader can
// use the segments exactly as they are -- including every fast path that reads a
// raw column block.
func (st *segDeltaState) empty() bool {
	if st == nil {
		return true
	}
	for _, m := range st.live {
		if len(m) > 0 {
			return false
		}
	}
	for _, m := range st.dead {
		if len(m) > 0 {
			return false
		}
	}
	return true
}

// emptyFor is empty() for one table, which is what a per-table fast path needs:
// a write to one table must not cost every other table its columnar reads.
func (st *segDeltaState) emptyFor(table int) bool {
	return st == nil || (len(st.live[table]) == 0 && len(st.dead[table]) == 0)
}

// segCleanFor reports whether the table at rootPage can be read STRAIGHT from
// its segments -- no delta rows to splice in, none removed.
//
// Every fast path in this package reads a raw column block: it takes an int64
// slice out of the mapping and hands it to a kernel, which is the whole reason
// they are fast and the reason none of them can see a row-major log. So each one
// asks this first and declines when the answer is false, and the VDBE runs its
// ordinary path.
//
// That is the LSM trade, not a gap: a write costs the table its columnar fast
// paths until compaction folds the log back in (segment_compact.go), which is
// exactly the knob a compaction policy turns. Declining is per TABLE, because a
// write to one table must not cost every other table its fast paths.
//
// The merged ROW scan (scanSegments) is unaffected -- it merges, so it stays
// correct and stays available.
//
// A WRITE SESSION's live row store (segSource.live) needs no case here, and a
// check for one was removed rather than kept as belt-and-braces: a live table is
// not in byRoot at all, so every fast path already declines on its own lookup.
// A mutation deleting the check failed nothing, which is the definition of a
// guard that cannot be trusted -- if a live table is ever ALSO given a byRoot
// entry, this is where the case belongs.
func (p *ReadOnlyPager) segCleanFor(rootPage uint32) bool {
	if p == nil || p.segs == nil {
		return false
	}
	return p.segs.cleanFor(rootPage)
}

// cleanFor is overlayFor's clean answer without building the overlay, which
// sorts every delta row: asked once per point read, that made a lookup O(delta).
func (src *segSource) cleanFor(rootPage uint32) bool {
	if src.delta == nil {
		return true
	}
	ti, ok := src.tableOf[rootPage]
	return !ok || src.delta.emptyFor(ti)
}

// deltaTouches reports whether the delta puts or kills rowid rid of the table at
// rootPage: when it does not, the segments' copy of that one row is the answer
// even though the table as a whole is not clean.
func (src *segSource) deltaTouches(rootPage uint32, rid int64) bool {
	if src.delta == nil {
		return false
	}
	ti, ok := src.tableOf[rootPage]
	if !ok || src.delta.emptyFor(ti) {
		return false
	}
	if _, live := src.delta.live[ti][rid]; live {
		return true
	}
	return src.delta.dead[ti][rid]
}

// readFileIfExists reads a file, treating absence as empty rather than an error --
// a segment file with no delta beside it is the ordinary case.
func readFileIfExists(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// withSegmentWriteLock runs fn holding an exclusive lock on the segment file.
//
// ONE LOCK FOR EVERY STATE TRANSITION. A segment database changes state two ways --
// a delta batch is appended, or the file is rewritten (segment_ddl.go) -- and both
// read the current state, decide, and write. Guarding only the append left the
// rewrite free to run concurrently with it, and a rewrite that had read a stale
// state wrote the file with a counter LOWER than the delta had already reached:
// "delta was built for counter 11 pages 1; this file is counter 7".
//
// The lock is on the SEGMENT file rather than the delta because the segment file
// always exists, and a rewrite deletes the delta. It is an OFD byte-range lock, so
// two connections inside one process conflict exactly as two processes do -- which
// is what a held session per connection requires, since they share a file and not
// a mutex.
func withSegmentWriteLock(segPath string, fn func() error) error {
	return withSegmentWriteLockWait(segPath, BusyTimeout, fn)
}

// withSegmentWriteLockWait is withSegmentWriteLock waiting at most wait for the
// lock -- a connection's own busy_timeout.
func withSegmentWriteLockWait(segPath string, wait time.Duration, fn func() error) error {
	f, err := openSegmentLockFile(segPath)
	if err != nil {
		return err
	}
	defer f.Close()
	// BOTH bytes: the state, and the reserved byte a BEGIN IMMEDIATE holds, so a
	// commit cannot land inside another connection's write transaction.
	if lerr := acquireLock(f, segStateByte, 2, true, wait); lerr != nil {
		return segmentLockBusy(lerr, "write")
	}
	defer releaseLock(f, segStateByte, 2)
	return fn()
}

// The lock file's two bytes, C's SHARED/RESERVED split (os_unix.c's
// unixLock): a reader holds the STATE byte shared, a state transition holds it
// exclusive, and a write transaction holds the RESERVED byte from its BEGIN
// IMMEDIATE to its end -- which excludes other writers and lets readers in, as
// C's RESERVED lock does. Locks were taken on the whole file before there was
// a second thing to lock.
const (
	segStateByte    = 0
	segReservedByte = 1
)

// withSegmentReadLock runs fn holding a SHARED lock on the segment file.
//
// A reader has to take one for the same reason a SQLite reader takes SHARED: a
// state transition is not atomic across two files. A rewrite renames the new
// segment file into place and THEN removes the delta, and a reader that looks
// between those two steps sees a new file beside an old delta and correctly
// refuses the pair ("delta was built for counter 33; this file is counter 35").
//
// Refusing is the right answer to that state -- applying a stale log twice is a
// wrong answer, not a slow one (segment_compact.go) -- but it is not an
// acceptable answer to a reader that merely arrived at the wrong moment. The
// lock makes the transition unobservable instead.
//
// A missing file is not an error: there is nothing to lock and nothing to read
// half of.
func withSegmentReadLock(segPath string, fn func() error) error {
	return withSegmentReadLockWait(segPath, BusyTimeout, fn)
}

// withSegmentReadLockWait is withSegmentReadLock waiting at most wait.
func withSegmentReadLockWait(segPath string, wait time.Duration, fn func() error) error {
	f, err := openSegmentLockFile(segPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if lerr := acquireLock(f, segStateByte, 1, false, wait); lerr != nil {
		return segmentLockBusy(lerr, "read")
	}
	defer releaseLock(f, segStateByte, 1)
	return fn()
}

// segmentLockSuffix names the file every lock on a segment database is taken on.
const segmentLockSuffix = ".lock"

// RemoveDatabaseFiles deletes a database file and every file the engine keeps
// beside it: the delta log, the lock file, and a rollback journal. It is for a
// database whose life is over -- an in-memory one whose last connection has
// closed -- and reports nothing, since any of them may never have existed.
func RemoveDatabaseFiles(path string) {
	for _, p := range []string{path, path + segDeltaSuffix, path + segmentLockSuffix, path + "-journal"} {
		os.Remove(p)
	}
}

// openSegmentLockFile opens (creating if needed) the file locks are taken on.
//
// IT IS A FILE OF ITS OWN, and that is the whole point. A byte-range lock belongs
// to an INODE, and a rewrite publishes its result by RENAMING a new file over the
// segment file -- so a lock taken on the segment file protects nothing the moment
// the rename lands. A reader that opened the path after it got the new inode, took
// its lock against no one, and saw the new file beside the delta the rewrite had
// not removed yet: "delta was built for counter 13 pages 1; this file is counter
// 15".
//
// Neither ordering of the rename and the delta removal fixes that -- removing the
// delta first loses rows if the rename never happens -- so the lock has to outlive
// both, which means living somewhere neither touches.
func openSegmentLockFile(segPath string) (*os.File, error) {
	return os.OpenFile(segPath+segmentLockSuffix, os.O_RDWR|os.O_CREATE, 0o644)
}

// SegmentLockBusyFragment names this format's own BUSY source, so a caller
// that classifies busy outcomes can tell it from the SQLite lock ladder's.
//
// It exists because a new busy SOURCE that cannot be named lands in whatever
// "other" bucket a classifier keeps, and an unclassified busy shape is itself a
// finding rather than noise -- N4's hazard X21, which is what caught this: the
// segment lock returned a bare ErrBusy and N4 correctly refused to average it away.
const SegmentLockBusyFragment = "segment write lock"

// segmentLockBusy names which segment lock timed out.
func segmentLockBusy(err error, kind string) error {
	if !errors.Is(err, ErrBusy) {
		return err
	}
	return fmt.Errorf("%w: another connection holds the %s (%s)", ErrBusy, SegmentLockBusyFragment, kind)
}

// addSegment takes a segment record's bytes as one of table's segments. A
// segment's int64 blocks are read in place, so bytes that are not 8-aligned in
// memory (a batch replayed from a heap copy, not the mapped file) are copied to
// memory that is.
func (st *segDeltaState) addSegment(table int, raw []byte) error {
	if uintptr(unsafe.Pointer(unsafe.SliceData(raw)))%8 != 0 {
		raw = alignedCopy(raw)
	}
	sg, err := openSegment(raw)
	if err != nil {
		return fmt.Errorf("engine: segment delta: segment record: %w", err)
	}
	if st.segs == nil {
		st.segs = map[int][]*segment{}
	}
	st.segs[table] = append(st.segs[table], sg)
	return nil
}

// alignedCopy is b in 8-aligned memory of its own.
func alignedCopy(b []byte) []byte {
	words := make([]uint64, (len(b)+7)/8)
	out := unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(words))), len(b))
	copy(out, b)
	return out
}

// errDeltaPredatesSegments refuses a segment record for a delta older than
// version 3, which an older reader would fail on: the caller rewrites the file
// instead, which leaves a version-3 delta behind.
var errDeltaPredatesSegments = errors.New("engine: segment delta: version predates segment records")

func hasSegRecord(recs []SegDeltaRecord) bool {
	for i := range recs {
		if recs[i].Seg != nil {
			return true
		}
	}
	return false
}
