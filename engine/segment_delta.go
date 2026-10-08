package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"time"

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
	segDeltaMagic     = "MQSD"
	// segDeltaVersion 2 widened a batch's length prefix from 4 bytes to 8: one
	// commit is one batch (its trailer is the commit marker), so a commit of 4 GiB
	// or more wrapped the length. A version-1 delta is still read and appended to
	// with its 4-byte prefix; every new delta is version 2.
	segDeltaVersion   = 2
	segDeltaHdrSize   = 32
	segDeltaSuffix    = ".delta"
	segDeltaTrailerSz = 16 // endCtr + endPages + cksum
)

// errDeltaNotForThisFile is returned when a delta names a different segment file
// than the one being attached.
var errDeltaNotForThisFile = errors.New("engine: segment delta was built against a different segment file")

// Record kinds. A tombstone carries no values.
const (
	segDeltaPut  = 1
	segDeltaKill = 2
)

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

	// src, when set, is the row store the put's row is read from WHILE the batch
	// is written, instead of Vals: a commit then holds one row at a time rather
	// than every row it writes, and a spilled row's record is copied as it lies
	// in the spill file (rowStore.recordOf). ipk is its table's INTEGER PRIMARY
	// KEY column, written NULL (the rowid carries it), or -1.
	src *rowStore
	ipk int
}

// recordLen is the length of the put's record.
func (r *SegDeltaRecord) recordLen() int {
	if r.src != nil {
		return r.src.recordLen(uint64(r.Rowid), r.ipk)
	}
	return recordSize(r.Vals)
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
	hdr := make([]byte, segDeltaHdrSize)
	copy(hdr[0:4], segDeltaMagic)
	binary.LittleEndian.PutUint32(hdr[4:], segDeltaVersion)
	binary.LittleEndian.PutUint32(hdr[8:], baseCtr)
	binary.LittleEndian.PutUint32(hdr[12:], basePages)
	binary.LittleEndian.PutUint64(hdr[24:], segDeltaChecksum(hdr[:24], 0))
	return os.WriteFile(path, hdr, 0o644)
}

// segDeltaChecksum is the running checksum, seeded from the previous value. It
// is the FNV-1a 64 mixing step over 8-byte groups, chosen because it is a dozen
// lines and this file does not need a cryptographic hash -- it needs to notice a
// torn append, and it needs the value to depend on every preceding byte.
func segDeltaChecksum(b []byte, seed uint64) uint64 {
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
}

// release closes the held delta, if any, and forgets the append point.
func (c *SegDeltaAppendState) release() {
	if c.f != nil {
		c.f.Close()
	}
	*c = SegDeltaAppendState{}
}

// appendSegmentDeltaAt is AppendSegmentDelta carrying the append point forward.
func appendSegmentDeltaAt(segPath string, baseCtr, basePages uint32, recs []SegDeltaRecord, endCtr, endPages uint32, carry *SegDeltaAppendState, sync syncMode) error {
	if len(recs) == 0 {
		return nil
	}
	path := segDeltaPath(segPath)
	fi, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if werr := writeSegDeltaHeader(path, baseCtr, basePages); werr != nil {
			return werr
		}
		if carry != nil {
			carry.known = false // the file is new; derive below
		}
		fi = nil
	}
	// The held descriptor is good only while the path still names its file.
	var f *os.File
	if carry != nil && carry.f != nil {
		if fi != nil && carry.fi != nil && os.SameFile(fi, carry.fi) {
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
	if carry != nil && carry.known && carry.offset > int64(segDeltaHdrSize) && fi != nil && fi.Size() == carry.offset {
		vf := f
		if vf == nil {
			vf, _ = os.Open(path)
			if vf != nil {
				defer vf.Close()
			}
		}
		if vf != nil {
			var tail [8]byte
			_, rerr := vf.ReadAt(tail[:], carry.offset-8)
			if rerr == nil && binary.LittleEndian.Uint64(tail[:]) == carry.cksum {
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
		st, serr = replaySegDelta(data, baseCtr, basePages)
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
	end, cksum, werr := writeSegDeltaBatch(f, st.bytes, st.cksum, segDeltaLenWidth(st.version), recs, endCtr, endPages)
	if werr != nil {
		return werr
	}
	// A torn tail past where this batch started is cut off. When the file
	// ended exactly there, nothing lies past the batch and the call is skipped.
	if fi == nil || fi.Size() != st.bytes {
		if err := f.Truncate(end); err != nil {
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
	if sync != syncNone {
		if err := fsyncFile(f, sync == syncFull); err != nil {
			return err
		}
	}
	if carry != nil {
		carry.offset, carry.cksum, carry.version, carry.known = end, cksum, st.version, true
	}
	return nil
}

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
func writeSegDeltaBatch(f *os.File, off int64, seed uint64, lenWidth int64, recs []SegDeltaRecord, endCtr, endPages uint32) (end int64, cksum uint64, err error) {
	var n int64
	sizes := make([]int32, len(recs)) // each put's record length, measured once
	for i := range recs {
		r := &recs[i]
		n += 13
		if !r.Kill {
			sizes[i] = int32(r.recordLen())
			n += 4 + int64(sizes[i])
		}
	}
	if lenWidth == 4 && n > math.MaxUint32 {
		return 0, 0, fmt.Errorf("engine: segment delta: a %d-byte commit does not fit this version-1 delta's batch length; compact it first", n)
	}
	// Sized to the batch when it is smaller than a chunk: a one-row commit
	// allocated (and zeroed) the whole megabyte.
	buf := make([]byte, 0, min(n+lenWidth+segDeltaTrailerSz, segDeltaChunk+64))
	if lenWidth == 4 {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(n))
	} else {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
	}
	at, cksum := off, seed
	flush := func() error {
		cksum = segDeltaChecksum(buf, cksum)
		if _, err := f.WriteAt(buf, at); err != nil {
			return err
		}
		at += int64(len(buf))
		buf = buf[:0]
		return nil
	}
	written := int64(0)
	for i := range recs {
		r := &recs[i]
		var hdr [13]byte
		if r.Kill {
			hdr[0] = segDeltaKill
		} else {
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
			buf = r.appendRecordTo(buf)
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
	var tr [segDeltaTrailerSz]byte
	binary.LittleEndian.PutUint32(tr[0:], endCtr)
	binary.LittleEndian.PutUint32(tr[4:], endPages)
	buf = append(buf, tr[:8]...)
	// The checksum covers everything before it, so it is known once the last
	// chunk is summed, and goes out in the same write: one call, not two.
	cksum = segDeltaChecksum(buf, cksum)
	buf = binary.LittleEndian.AppendUint64(buf, cksum)
	if _, err := f.WriteAt(buf, at); err != nil {
		return 0, 0, err
	}
	return at + int64(len(buf)), cksum, nil
}

// segDeltaBatchBytes is the most writeSegDeltaBatch appends for recs: the wider
// length prefix, every record as it measures them, and the trailer.
func segDeltaBatchBytes(recs []SegDeltaRecord) int64 {
	n := int64(8 + segDeltaTrailerSz)
	for i := range recs {
		n += 13
		if !recs[i].Kill {
			n += 4 + int64(recs[i].recordLen())
		}
	}
	return n
}

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
func replaySegDelta(data []byte, baseCtr, basePages uint32) (*segDeltaState, error) {
	st := &segDeltaState{
		live:     map[int]map[int64][]Value{},
		dead:     map[int]map[int64]bool{},
		endCtr:   baseCtr,
		endPages: basePages,
		bytes:    segDeltaHdrSize,
	}
	if len(data) < segDeltaHdrSize {
		return st, nil // absent or empty: the segment file stands alone
	}
	if string(data[0:4]) != segDeltaMagic {
		return nil, fmt.Errorf("engine: segment delta: bad magic %q", data[0:4])
	}
	st.version = binary.LittleEndian.Uint32(data[4:])
	if st.version != 1 && st.version != segDeltaVersion {
		return nil, fmt.Errorf("engine: segment delta: version %d, want 1 or %d", st.version, segDeltaVersion)
	}
	if binary.LittleEndian.Uint64(data[24:]) != segDeltaChecksum(data[:24], 0) {
		return nil, fmt.Errorf("engine: segment delta: header checksum fails")
	}
	if c, p := binary.LittleEndian.Uint32(data[8:]), binary.LittleEndian.Uint32(data[12:]); c != baseCtr || p != basePages {
		return nil, fmt.Errorf("%w (delta was built for counter %d pages %d; this file is counter %d pages %d)",
			errDeltaNotForThisFile, c, p, baseCtr, basePages)
	}
	st.cksum = segDeltaChecksum(data[:24], 0)

	w := segDeltaLenWidth(st.version)
	off := int64(segDeltaHdrSize)
	for off+w+segDeltaTrailerSz <= int64(len(data)) {
		var n int64
		if w == 4 {
			n = int64(binary.LittleEndian.Uint32(data[off:]))
		} else {
			n = int64(binary.LittleEndian.Uint64(data[off:]))
		}
		end := off + w + n + segDeltaTrailerSz
		if n < 0 || end < off || end > int64(len(data)) {
			break // truncated batch
		}
		batch := data[off:end]
		want := binary.LittleEndian.Uint64(batch[len(batch)-8:])
		if segDeltaChecksum(batch[:len(batch)-8], st.cksum) != want {
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
		default:
			return fmt.Errorf("engine: segment delta: unknown record kind %d", kind)
		}
	}
	return nil
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
	_, _, clean := p.segs.overlayFor(rootPage)
	return clean
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
