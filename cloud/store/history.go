// Package store keeps each hosted database's history: a log of the bytes
// every commit added to the database's files, and bases (whole copies) where
// the engine replaced its segment file. Any committed state in the log can be
// written back out exactly. See cloud/HISTORY.md.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/samyfodil/musql/engine"
)

var (
	// ErrBeforeHistory is a restore point older than the oldest base kept.
	ErrBeforeHistory = errors.New("store: the time is before the oldest snapshot kept")
	// ErrFuture is a restore point after now.
	ErrFuture = errors.New("store: the time is in the future")
)

const (
	kindBatch      = 1
	kindBase       = 2
	kindManualBase = 3

	hdrLen  = 4 + 1 + 8 + 8 + 4 // length, kind, seq, nanos, counter
	baseLen = 8 + 32 + 8        // segment size, sha256, delta prefix length
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// entry is one log record, indexed in memory; a batch's bytes stay in the log.
type entry struct {
	kind    byte
	seq     uint64
	at      time.Time
	counter uint32
	rec     int64 // the record's offset in the log
	// A batch: the delta bytes [from, from+n), at off in the log.
	from, off, n int64
	// A base.
	size     int64
	sum      [32]byte
	deltaLen int64
}

func (e *entry) isBase() bool { return e.kind != kindBatch }

// Snapshot is a base: a point a restore replays from.
type Snapshot struct {
	Seq    uint64
	At     time.Time
	Size   int64
	Manual bool
}

// ID is the snapshot's name in the API.
func (s Snapshot) ID() string { return strconv.FormatUint(s.Seq, 10) }

// History is one database's log. Its methods are safe for concurrent use.
type History struct {
	dir string
	// Logf reports a commit the hook could not log; the next sync catches up.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	path    string // the database file, absolute
	log     *os.File
	size    int64 // the log's length
	entries []entry
	// What the log holds of the current base: the delta's first loggedEnd
	// bytes, from the delta file deltaFI (nil before one is seen).
	loggedEnd int64
	deltaFI   os.FileInfo
	// baseFI is the live segment file, when it is known to be the latest
	// base's: the engine replaces that file and never writes it, so the same
	// file is the same bytes.
	baseFI os.FileInfo
	seq    uint64
	closed bool
	// notify is closed at the next record written, or at Close.
	notify chan struct{}
}

var open sync.Map // absolute database path -> *History

// OnCommit is the engine's commit hook (engine.WithCommitHook): it logs the
// commit for the database's History, if one is open.
func OnCommit(c engine.CommitInfo) {
	p, err := filepath.Abs(c.Path)
	if err != nil {
		return
	}
	v, ok := open.Load(p)
	if !ok {
		return
	}
	h := v.(*History)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.path != p {
		return
	}
	if err := h.syncLocked(c, 0); err != nil {
		if h.Logf != nil {
			h.Logf("history %s: %v", p, err)
		}
	}
}

// Open opens (or starts) the history in dir of the database at dbPath, which
// must exist, and brings log and file to one state: a commit the log missed
// is logged; a file behind its log is rolled forward to the log's end.
func Open(dbPath, dir string) (*History, error) {
	p, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h := &History{dir: dir, path: p}
	if err := h.load(); err != nil {
		return nil, err
	}
	if _, loaded := open.LoadOrStore(p, h); loaded {
		h.log.Close()
		return nil, fmt.Errorf("store: %s already has an open history", p)
	}
	if err := engine.CommitState(p, func(c engine.CommitInfo) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.repairLocked(c)
	}); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Close stops logging; the files stay.
func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if h.notify != nil {
		close(h.notify)
		h.notify = nil
	}
	open.CompareAndDelete(h.path, h)
	return h.log.Close()
}

// Path is the database file the history follows.
func (h *History) Path() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path
}

func (h *History) logPath() string { return filepath.Join(h.dir, "log") }

func (h *History) baseFiles(seq uint64) (musq, delta string) {
	b := filepath.Join(h.dir, "base-"+strconv.FormatUint(seq, 10))
	return b + ".musq", b + ".delta"
}

// load reads the log, truncating a torn last record.
func (h *History) load() error {
	f, err := os.OpenFile(h.logPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	h.log = f
	h.entries = nil
	r := io.NewSectionReader(f, 0, 1<<62)
	var off int64
	var hdr [hdrLen]byte
	buf := make([]byte, 0, 1<<16)
	for {
		if _, err := r.ReadAt(hdr[:4], off); err != nil {
			break
		}
		n := int64(binary.LittleEndian.Uint32(hdr[:4]))
		if n < hdrLen-4 {
			break
		}
		if int64(cap(buf)) < n+4 {
			buf = make([]byte, n+4)
		}
		b := buf[:n+4]
		if _, err := r.ReadAt(b, off+4); err != nil {
			break
		}
		e, err := decodeRecord(b, off)
		if errors.Is(err, errBadCRC) {
			break
		}
		if err != nil {
			return fmt.Errorf("store: %s: %w", h.logPath(), err)
		}
		h.entries = append(h.entries, e)
		off += 4 + n + 4
	}
	if len(h.entries) > 0 && !h.entries[0].isBase() {
		return fmt.Errorf("store: %s does not start with a base", h.logPath())
	}
	if fi, err := f.Stat(); err == nil && fi.Size() > off {
		if err := f.Truncate(off); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	h.size = off
	h.loggedEnd = 0
	if len(h.entries) > 0 {
		last := h.entries[len(h.entries)-1]
		h.seq = last.seq
		h.loggedEnd = h.spanEnd(len(h.entries) - 1)
	}
	return nil
}

var errBadCRC = errors.New("bad record checksum")

// decodeRecord parses a record's body and checksum (b, the record without its
// length), found at offset off of a log.
func decodeRecord(b []byte, off int64) (entry, error) {
	if len(b) < hdrLen {
		return entry{}, fmt.Errorf("short record at %d", off)
	}
	n := len(b) - 4
	body := b[:n]
	if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(b[n:]) {
		return entry{}, errBadCRC
	}
	e := entry{kind: body[0], seq: binary.LittleEndian.Uint64(body[1:]),
		at: time.Unix(0, int64(binary.LittleEndian.Uint64(body[9:]))), counter: binary.LittleEndian.Uint32(body[17:]), rec: off}
	pl := body[hdrLen-4:]
	switch e.kind {
	case kindBatch:
		if len(pl) < 8 {
			return entry{}, fmt.Errorf("short batch record at %d", off)
		}
		e.from = int64(binary.LittleEndian.Uint64(pl))
		e.off, e.n = off+4+hdrLen-4+8, int64(len(pl)-8)
	case kindBase, kindManualBase:
		if len(pl) != baseLen {
			return entry{}, fmt.Errorf("bad base record at %d", off)
		}
		e.size = int64(binary.LittleEndian.Uint64(pl))
		copy(e.sum[:], pl[8:40])
		e.deltaLen = int64(binary.LittleEndian.Uint64(pl[40:]))
	default:
		return entry{}, fmt.Errorf("unknown record kind %d at %d", e.kind, off)
	}
	return e, nil
}

// baseOf is the index of the base entry i replays from.
func (h *History) baseOf(i int) int {
	for ; i > 0 && !h.entries[i].isBase(); i-- {
	}
	return i
}

// spanEnd is the delta length the state after entry i has.
func (h *History) spanEnd(i int) int64 {
	e := h.entries[i]
	if e.isBase() {
		return e.deltaLen
	}
	return e.from + e.n
}

func (h *History) now() time.Time {
	t := time.Now()
	if n := len(h.entries); n > 0 && t.Before(h.entries[n-1].at) {
		return h.entries[n-1].at // a clock stepping back must not reorder history
	}
	return t
}

// appendRecord writes one record and syncs it.
func (h *History) appendRecord(e entry, payload ...[]byte) error {
	n := hdrLen - 4
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, 4, 4+n+4)
	binary.LittleEndian.PutUint32(b, uint32(n))
	b = append(b, e.kind)
	b = binary.LittleEndian.AppendUint64(b, e.seq)
	b = binary.LittleEndian.AppendUint64(b, uint64(e.at.UnixNano()))
	b = binary.LittleEndian.AppendUint32(b, e.counter)
	for _, p := range payload {
		b = append(b, p...)
	}
	b = binary.LittleEndian.AppendUint32(b, crc32.Checksum(b[4:], crcTable))
	return h.writeRecord(b)
}

// writeRecord appends one encoded record, syncs it and indexes it.
func (h *History) writeRecord(b []byte) error {
	e, err := decodeRecord(b[4:], h.size)
	if err != nil {
		return err
	}
	if _, err := h.log.WriteAt(b, h.size); err != nil {
		return err
	}
	if err := h.log.Sync(); err != nil {
		return err
	}
	h.size += int64(len(b))
	h.entries = append(h.entries, e)
	h.seq = e.seq
	if h.notify != nil {
		close(h.notify)
		h.notify = nil
	}
	return nil
}

// syncLocked brings the log up to the committed state c. force is a base
// kind to take even when a batch would do (0 for none).
func (h *History) syncLocked(c engine.CommitInfo, force byte) error {
	segFI, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	end := c.DeltaEnd
	dfi, err := os.Stat(h.path + ".delta")
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		dfi, end = nil, 0
	} else if dfi.Size() < end {
		return fmt.Errorf("store: %s.delta is %d bytes, its commits end at %d", h.path, dfi.Size(), end)
	}
	sameBase, err := h.isLatestBase(segFI)
	if err != nil {
		return err
	}
	sameDelta := h.loggedEnd == 0 || (h.deltaFI != nil && dfi != nil && os.SameFile(h.deltaFI, dfi))
	if force != 0 || !sameBase || !sameDelta || end < h.loggedEnd {
		k := force
		if k == 0 {
			k = kindBase
		}
		return h.writeBase(c.Counter, end, dfi, k)
	}
	if end == h.loggedEnd {
		h.deltaFI = dfi
		return nil
	}
	b, err := readRange(h.path+".delta", h.loggedEnd, end)
	if err != nil {
		return err
	}
	var from [8]byte
	binary.LittleEndian.PutUint64(from[:], uint64(h.loggedEnd))
	e := entry{kind: kindBatch, seq: h.seq + 1, at: h.now(), counter: c.Counter, from: h.loggedEnd}
	if err := h.appendRecord(e, from[:], b); err != nil {
		return err
	}
	h.loggedEnd, h.deltaFI = end, dfi
	return nil
}

// isLatestBase reports whether the segment file is the latest base's.
func (h *History) isLatestBase(segFI os.FileInfo) (bool, error) {
	if len(h.entries) == 0 {
		return false, nil
	}
	if h.baseFI != nil {
		return os.SameFile(h.baseFI, segFI), nil
	}
	b := h.entries[h.baseOf(len(h.entries)-1)]
	if b.size != segFI.Size() {
		return false, nil
	}
	sum, err := fileSum(h.path)
	if err != nil {
		return false, err
	}
	return sum == b.sum, nil
}

// writeBase logs the files as they stand as a new base: the segment file
// linked (or copied) and the delta's committed prefix copied, both synced
// before the record names them.
func (h *History) writeBase(counter uint32, end int64, dfi os.FileInfo, kind byte) error {
	seq := h.seq + 1
	musq, delta := h.baseFiles(seq)
	os.Remove(musq)
	// Taken under the commit lock, so it is the file just linked or copied.
	segFI, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	if os.Link(h.path, musq) != nil {
		if err := copyFile(h.path, musq, -1); err != nil {
			return err
		}
	}
	sum, err := fileSum(musq)
	if err != nil {
		return err
	}
	fi, err := os.Stat(musq)
	if err != nil {
		return err
	}
	if end > 0 {
		if err := copyFile(h.path+".delta", delta, end); err != nil {
			return err
		}
	}
	if err := syncDir(h.dir); err != nil {
		return err
	}
	pl := make([]byte, 0, baseLen)
	pl = binary.LittleEndian.AppendUint64(pl, uint64(fi.Size()))
	pl = append(pl, sum[:]...)
	pl = binary.LittleEndian.AppendUint64(pl, uint64(end))
	e := entry{kind: kind, seq: seq, at: h.now(), counter: counter, size: fi.Size(), sum: sum, deltaLen: end}
	if err := h.appendRecord(e, pl); err != nil {
		return err
	}
	h.loggedEnd, h.deltaFI, h.baseFI = end, dfi, segFI
	return nil
}

// Sync logs whatever the database has committed that the log lacks.
func (h *History) Sync() error {
	return h.withState(func(c engine.CommitInfo) error { return h.syncLocked(c, 0) })
}

// Snapshot takes a base now.
func (h *History) Snapshot(manual bool) (Snapshot, error) {
	var sn Snapshot
	err := h.withState(func(c engine.CommitInfo) error {
		k := byte(kindBase)
		if manual {
			k = kindManualBase
		}
		if err := h.syncLocked(c, k); err != nil {
			return err
		}
		sn = snapshotOf(&h.entries[len(h.entries)-1])
		return nil
	})
	return sn, err
}

// SnapshotIfChanged takes a base when anything was logged since the last one.
func (h *History) SnapshotIfChanged() error {
	return h.withState(func(c engine.CommitInfo) error {
		if err := h.syncLocked(c, 0); err != nil {
			return err
		}
		if h.entries[len(h.entries)-1].isBase() {
			return nil
		}
		return h.syncLocked(c, kindBase)
	})
}

// withState runs fn under the database's shared lock (no commit lands
// meanwhile) and the history's mutex, in that order: the commit hook takes
// them in the same order.
func (h *History) withState(fn func(engine.CommitInfo) error) error {
	h.mu.Lock()
	p := h.path
	h.mu.Unlock()
	return engine.CommitState(p, func(c engine.CommitInfo) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return errors.New("store: history is closed")
		}
		if h.path != p {
			return errors.New("store: the database moved during the call")
		}
		return fn(c)
	})
}

func snapshotOf(e *entry) Snapshot {
	return Snapshot{Seq: e.seq, At: e.at, Size: e.size + e.deltaLen, Manual: e.kind == kindManualBase}
}

// Snapshots lists the bases, oldest first.
func (h *History) Snapshots() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Snapshot
	for i := range h.entries {
		if h.entries[i].isBase() {
			out = append(out, snapshotOf(&h.entries[i]))
		}
	}
	return out
}

// Oldest is the earliest time a restore can name.
func (h *History) Oldest() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) == 0 {
		return time.Time{}
	}
	return h.entries[0].at
}

// Restore writes the database as it was at time at to dst (which must not
// exist) and dst's delta beside it.
func (h *History) Restore(at time.Time, dst string) error {
	if at.After(time.Now()) {
		return ErrFuture
	}
	if err := h.Sync(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	i := -1
	for j := range h.entries {
		if h.entries[j].at.After(at) {
			break
		}
		i = j
	}
	if i < 0 {
		return ErrBeforeHistory
	}
	return h.materialize(i, dst)
}

// RestoreSnapshot writes the state a base holds to dst.
func (h *History) RestoreSnapshot(seq uint64, dst string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.entries {
		if e := &h.entries[i]; e.seq == seq && e.isBase() {
			return h.materialize(i, dst)
		}
	}
	return fmt.Errorf("store: no snapshot %d", seq)
}

// materialize writes the state after entry i to dst and dst.delta, each
// complete and synced before it takes its name.
func (h *History) materialize(i int, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("store: %s exists", dst)
	}
	bi := h.baseOf(i)
	b := h.entries[bi]
	musq, bdelta := h.baseFiles(b.seq)
	// A base is verified before use, so a file the engine ever wrote in place
	// is an error here rather than a wrong restore.
	sum, err := fileSum(musq)
	if err != nil {
		return err
	}
	if sum != b.sum {
		return fmt.Errorf("store: snapshot %d is damaged (its checksum does not match)", b.seq)
	}
	deltaEnd := h.spanEnd(i)
	if deltaEnd > 0 {
		tmp := dst + ".delta.tmp"
		f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		werr := func() error {
			if b.deltaLen > 0 {
				src, err := os.Open(bdelta)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, io.LimitReader(src, b.deltaLen))
				src.Close()
				if err != nil {
					return err
				}
			}
			at := b.deltaLen
			for j := bi + 1; j <= i; j++ {
				e := h.entries[j]
				if e.from != at {
					return fmt.Errorf("store: log record %d starts at delta byte %d, want %d", e.seq, e.from, at)
				}
				if _, err := io.Copy(f, io.NewSectionReader(h.log, e.off, e.n)); err != nil {
					return err
				}
				at += e.n
			}
			return f.Sync()
		}()
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(tmp)
			return werr
		}
		if err := os.Rename(tmp, dst+".delta"); err != nil {
			os.Remove(tmp)
			return err
		}
	} else {
		os.Remove(dst + ".delta")
	}
	if err := copyFile(musq, dst, -1); err != nil {
		os.Remove(dst + ".delta")
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// Retarget makes the history follow the database at newPath from now on
// (an in-place restore wrote the database there) and logs it as a base.
func (h *History) Retarget(newPath string) error {
	p, err := filepath.Abs(newPath)
	if err != nil {
		return err
	}
	return engine.CommitState(p, func(c engine.CommitInfo) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return errors.New("store: history is closed")
		}
		if _, loaded := open.LoadOrStore(p, h); loaded {
			return fmt.Errorf("store: %s already has an open history", p)
		}
		open.CompareAndDelete(h.path, h)
		h.path = p
		return h.writeBase(c.Counter, c.DeltaEnd, statOrNil(p+".delta"), kindBase)
	})
}

// Prune drops history before the newest base taken at or before cutoff.
func (h *History) Prune(cutoff time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := 0
	for i := range h.entries {
		if h.entries[i].at.After(cutoff) {
			break
		}
		if h.entries[i].isBase() {
			keep = i
		}
	}
	if keep == 0 {
		return nil
	}
	from := h.entries[keep].rec
	tmp := h.logPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.NewSectionReader(h.log, from, h.size-from))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, h.logPath())
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := syncDir(h.dir); err != nil {
		return err
	}
	dropped := slices.Clone(h.entries[:keep])
	h.log.Close()
	baseFI, deltaFI := h.baseFI, h.deltaFI
	if err := h.load(); err != nil {
		return err
	}
	h.baseFI, h.deltaFI = baseFI, deltaFI
	for _, e := range dropped {
		if e.isBase() {
			musq, delta := h.baseFiles(e.seq)
			os.Remove(musq)
			os.Remove(delta)
		}
	}
	return nil
}

// repairLocked brings a freshly loaded log and the file to one state.
func (h *History) repairLocked(c engine.CommitInfo) error {
	if len(h.entries) == 0 {
		return h.syncLocked(c, kindBase)
	}
	end := c.DeltaEnd
	if _, err := os.Stat(h.path + ".delta"); err != nil {
		end = 0
	}
	segFI, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	var cur [32]byte
	haveSum := false
	// The newest base the file is, then where in that base's span its delta is.
	for bi := len(h.entries) - 1; bi >= 0; bi-- {
		b := &h.entries[bi]
		if !b.isBase() || b.size != segFI.Size() {
			continue
		}
		musq, _ := h.baseFiles(b.seq)
		if bfi, err := os.Stat(musq); err != nil || !os.SameFile(bfi, segFI) {
			if !haveSum {
				if cur, err = fileSum(h.path); err != nil {
					return err
				}
				haveSum = true
			}
			if cur != b.sum {
				continue
			}
		}
		last := len(h.entries) - 1
		ni := bi + 1
		for ni <= last && !h.entries[ni].isBase() {
			ni++
		}
		spanLast := ni - 1
		logged, err := h.spanBytes(bi, spanLast)
		if err != nil {
			return err
		}
		var fileDelta []byte
		if end > 0 {
			if fileDelta, err = readRange(h.path+".delta", 0, end); err != nil {
				return err
			}
		}
		if spanLast == last && bytes.HasPrefix(fileDelta, logged) {
			// The file is the log's end, or past it: log what it has more.
			h.baseFI = segFI
			h.loggedEnd, h.deltaFI = int64(len(logged)), statOrNil(h.path+".delta")
			return h.syncLocked(c, 0)
		}
		if atEntryBoundary(h, bi, spanLast, logged, fileDelta) {
			return h.rollForward()
		}
		break
	}
	// At no logged point: the file is what the database is.
	h.baseFI = nil
	return h.syncLocked(c, kindBase)
}

// atEntryBoundary reports whether fileDelta is the state after one of the
// entries bi..spanLast.
func atEntryBoundary(h *History, bi, spanLast int, logged, fileDelta []byte) bool {
	if !bytes.HasPrefix(logged, fileDelta) {
		return false
	}
	for j := bi; j <= spanLast; j++ {
		if h.spanEnd(j) == int64(len(fileDelta)) {
			return true
		}
	}
	return false
}

// spanBytes is the delta after entry last, which replays from base bi.
func (h *History) spanBytes(bi, last int) ([]byte, error) {
	b := h.entries[bi]
	var out []byte
	if b.deltaLen > 0 {
		_, delta := h.baseFiles(b.seq)
		var err error
		if out, err = readRange(delta, 0, b.deltaLen); err != nil {
			return nil, err
		}
	}
	for j := bi + 1; j <= last; j++ {
		e := h.entries[j]
		chunk := make([]byte, e.n)
		if _, err := h.log.ReadAt(chunk, e.off); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// rollForward replaces the file with the log's last state. The old delta goes
// first: a crash then leaves a segment file alone, a state this repair meets
// again, never a delta paired with the wrong file.
func (h *History) rollForward() error {
	tmp := h.path + ".rollforward"
	os.Remove(tmp)
	os.Remove(tmp + ".delta")
	if err := h.materialize(len(h.entries)-1, tmp); err != nil {
		return err
	}
	if err := os.Remove(h.path + ".delta"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDir(filepath.Dir(h.path)); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.path); err != nil {
		return err
	}
	if _, err := os.Stat(tmp + ".delta"); err == nil {
		if err := os.Rename(tmp+".delta", h.path+".delta"); err != nil {
			return err
		}
	}
	if err := syncDir(filepath.Dir(h.path)); err != nil {
		return err
	}
	// The file is a copy, not the base's link: the next commit's check hashes
	// it, finds the base, and goes on with batches.
	h.baseFI = nil
	h.deltaFI = statOrNil(h.path + ".delta")
	return nil
}

func statOrNil(p string) os.FileInfo {
	fi, err := os.Stat(p)
	if err != nil {
		return nil
	}
	return fi
}

func readRange(p string, from, to int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, to-from)
	if _, err := f.ReadAt(b, from); err != nil {
		return nil, err
	}
	return b, nil
}

func fileSum(p string) ([32]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	s := sha256.New()
	if _, err := io.Copy(s, f); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	s.Sum(out[:0])
	return out, nil
}

// copyFile copies src (its first n bytes; all when n < 0) to dst through a
// synced temporary file.
func copyFile(src, dst string, n int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var r io.Reader = in
	if n >= 0 {
		r = io.LimitReader(in, n)
	}
	_, err = io.Copy(out, r)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
