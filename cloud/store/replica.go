package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samyfodil/musql/engine"
)

// ErrClosed is a history closed while it was being read.
var ErrClosed = errors.New("store: history is closed")

// shipBudget bounds the record bytes one Ship call returns.
const shipBudget = 4 << 20

// Shipment is one log record as a replica receives it: Record is its bytes as
// the log holds them (length, body, checksum). A base comes with its files,
// open; the receiver closes them.
type Shipment struct {
	Seq     uint64
	Record  []byte
	Base    bool
	Segment *os.File // SegmentSize bytes
	Delta   *os.File // DeltaSize bytes; nil when 0
	// SegmentSize and DeltaSize are the base's.
	SegmentSize, DeltaSize int64
}

// Close closes a base's files.
func (s *Shipment) Close() {
	if s.Segment != nil {
		s.Segment.Close()
	}
	if s.Delta != nil {
		s.Delta.Close()
	}
}

// Ship returns the records a replica at seq after needs next, in order: the
// one after it and on, or, when the log no longer has that one (or after is
// 0, or the log went back past it), the newest base and on. With nothing to
// send it returns none and a channel closed when there may be.
func (h *History) Ship(after uint64) ([]Shipment, <-chan struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, ErrClosed
	}
	n := len(h.entries)
	if n > 0 && h.entries[n-1].seq == after {
		if h.notify == nil {
			h.notify = make(chan struct{})
		}
		return nil, h.notify, nil
	}
	if n == 0 {
		return nil, nil, errors.New("store: the history has no records")
	}
	start := -1
	if after != 0 {
		for i := range h.entries {
			if h.entries[i].seq == after+1 {
				start = i
				break
			}
		}
	}
	if start < 0 {
		start = h.baseOf(n - 1)
	}
	var out []Shipment
	var total int64
	for i := start; i < n && total < shipBudget; i++ {
		e := &h.entries[i]
		end := h.size
		if i+1 < n {
			end = h.entries[i+1].rec
		}
		b := make([]byte, end-e.rec)
		if _, err := h.log.ReadAt(b, e.rec); err != nil {
			closeAll(out)
			return nil, nil, err
		}
		total += int64(len(b))
		sh := Shipment{Seq: e.seq, Record: b, Base: e.isBase()}
		if sh.Base {
			musq, delta := h.baseFiles(e.seq)
			f, err := os.Open(musq)
			if err != nil {
				closeAll(out)
				return nil, nil, err
			}
			sh.Segment, sh.SegmentSize, sh.DeltaSize = f, e.size, e.deltaLen
			if e.deltaLen > 0 {
				if sh.Delta, err = os.Open(delta); err != nil {
					f.Close()
					closeAll(out)
					return nil, nil, err
				}
			}
			total += e.size + e.deltaLen
		}
		out = append(out, sh)
	}
	return out, nil, nil
}

func closeAll(s []Shipment) {
	for i := range s {
		s[i].Close()
	}
}

// OpenReplica opens the history in dir of a database kept as a copy of
// another's (its records arrive through Apply), and brings the file at dbPath
// to the log's last state. A replica's history is never fed by the commit
// hook: nothing commits to the file but Apply.
func OpenReplica(dbPath, dir string) (*History, error) {
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
	if len(h.entries) == 0 {
		// Nothing applied yet: a file left without a log is not a state.
		for _, s := range []string{p, p + ".delta"} {
			if err := os.Remove(s); err != nil && !os.IsNotExist(err) {
				h.log.Close()
				return nil, err
			}
		}
		return h, nil
	}
	at := false
	if err := engine.CommitState(p, func(c engine.CommitInfo) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		at = h.atLogEnd(c)
		return nil
	}); err != nil || !at {
		// A crash between logging a record and applying it, or inside an
		// install, leaves the file behind the log or its pair mismatched.
		if err := engine.ReplaceState(p, func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.rollForward()
		}); err != nil {
			h.log.Close()
			return nil, err
		}
	}
	return h, nil
}

// atLogEnd reports whether the file, whose committed state is c, is the
// state after the log's last record.
func (h *History) atLogEnd(c engine.CommitInfo) bool {
	last := len(h.entries) - 1
	bi := h.baseOf(last)
	sum, err := fileSum(h.path)
	if err != nil || sum != h.entries[bi].sum {
		return false
	}
	logged, err := h.spanBytes(bi, last)
	if err != nil || int64(len(logged)) != c.DeltaEnd {
		return false
	}
	if c.DeltaEnd > 0 {
		fileDelta, err := readRange(h.path+".delta", 0, c.DeltaEnd)
		if err != nil || !bytes.Equal(fileDelta, logged) {
			return false
		}
	}
	h.loggedEnd = c.DeltaEnd
	return true
}

// Seq is the last record applied (or logged); 0 for none.
func (h *History) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Apply logs one shipped record and applies it to the replica's file, under
// the file's write lock: a reader sees the state before it or after it. A
// batch must follow the last record applied; a base may come at any seq (the
// primary's log was pruned or started over), and one at or before the last
// seq starts the replica's log over. segment and delta are a base's files
// (delta nil when the base has none).
func (h *History) Apply(rec []byte, segment, delta io.Reader) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	if len(rec) < 4 || int(binary.LittleEndian.Uint32(rec))+8 != len(rec) {
		return errors.New("store: malformed record")
	}
	e, err := decodeRecord(rec[4:], 0)
	if err != nil {
		return fmt.Errorf("store: shipped record: %w", err)
	}
	if !e.isBase() {
		if len(h.entries) == 0 || e.seq != h.seq+1 || e.from != h.loggedEnd {
			return fmt.Errorf("store: record %d (delta byte %d) does not follow record %d (delta byte %d)", e.seq, e.from, h.seq, h.loggedEnd)
		}
		if err := h.writeRecord(rec); err != nil {
			return err
		}
		e = h.entries[len(h.entries)-1]
		b := make([]byte, e.n)
		if _, err := h.log.ReadAt(b, e.off); err != nil {
			return err
		}
		if err := engine.ReplaceState(h.path, func() error { return writeDelta(h.path+".delta", e.from, b) }); err != nil {
			return err
		}
		h.loggedEnd = e.from + e.n
		return nil
	}
	if e.seq <= h.seq {
		if err := h.resetLocked(); err != nil {
			return err
		}
	}
	musq, bdelta := h.baseFiles(e.seq)
	if err := receiveFile(segment, e.size, musq); err != nil {
		return err
	}
	if sum, err := fileSum(musq); err != nil || sum != e.sum {
		os.Remove(musq)
		if err == nil {
			err = fmt.Errorf("store: base %d does not match its checksum", e.seq)
		}
		return err
	}
	if e.deltaLen > 0 {
		if err := receiveFile(delta, e.deltaLen, bdelta); err != nil {
			os.Remove(musq)
			return err
		}
	}
	if err := syncDir(h.dir); err != nil {
		return err
	}
	if err := h.writeRecord(rec); err != nil {
		return err
	}
	if err := engine.ReplaceState(h.path, func() error { return h.installBase(musq, bdelta, e.deltaLen) }); err != nil {
		return err
	}
	h.loggedEnd = e.deltaLen
	return nil
}

// installBase makes the file pair a base's. The old delta goes first, as in
// rollForward: a crash between leaves a segment file alone, never a delta
// paired with the wrong one.
func (h *History) installBase(musq, bdelta string, deltaLen int64) error {
	dir := filepath.Dir(h.path)
	if err := os.Remove(h.path + ".delta"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	tmp := h.path + ".install"
	os.Remove(tmp)
	// The engine never writes a segment file in place, so the base can be
	// the file itself.
	if os.Link(musq, tmp) != nil {
		if err := copyFile(musq, tmp, -1); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, h.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if deltaLen > 0 {
		if err := copyFile(bdelta, h.path+".delta", deltaLen); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// resetLocked empties the log and drops its bases.
func (h *History) resetLocked() error {
	for i := range h.entries {
		if h.entries[i].isBase() {
			musq, delta := h.baseFiles(h.entries[i].seq)
			os.Remove(musq)
			os.Remove(delta)
		}
	}
	if err := h.log.Truncate(0); err != nil {
		return err
	}
	if err := h.log.Sync(); err != nil {
		return err
	}
	h.entries, h.size, h.seq, h.loggedEnd = nil, 0, 0, 0
	return nil
}

// receiveFile writes exactly n bytes of r to dst through a synced temporary
// file.
func receiveFile(r io.Reader, n int64, dst string) error {
	if r == nil {
		return fmt.Errorf("store: %s: no content", filepath.Base(dst))
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	got, err := io.Copy(f, io.LimitReader(r, n))
	if err == nil && got != n {
		err = fmt.Errorf("store: %s: %d bytes, want %d", filepath.Base(dst), got, n)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
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

// writeDelta writes b at off of the delta and syncs it. Anything past the end
// is a torn tail left by a crash, and is cut.
func writeDelta(p string, off int64, b []byte) error {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < off {
		return fmt.Errorf("store: %s is %d bytes, the record starts at %d", p, fi.Size(), off)
	}
	if _, err := f.WriteAt(b, off); err != nil {
		return err
	}
	if end := off + int64(len(b)); fi.Size() > end {
		if err := f.Truncate(end); err != nil {
			return err
		}
	}
	return f.Sync()
}
