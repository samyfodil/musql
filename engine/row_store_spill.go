package engine

import (
	"os"
	"runtime"
	"slices"
)

// Row store spilling: rows a transaction wrote, persisted once they outgrow memory.
//
// A table's committed rows already live in the mapped segment file; what a
// session holds is what it wrote (rowStore.m), and it holds every such row
// until the commit writes it. That made the size of one transaction a size of
// the heap: bigsort.test 1.0 -- one INSERT of 300,000 rows of 10 KB inside a
// BEGIN -- held 2.83 GB of rows before its COMMIT could start. C SQLite has no
// such bound because its dirty pages spill to the journal and the database file
// as the page cache fills (pagerStress, pager.c:4626).
//
// Past rowSpillThreshold bytes of in-memory rows a store writes them, in record
// encoding, to an append-only file of its own and keeps in m only a SENTINEL per
// row: a one-Value row of type valueSpilled naming the record's offset and
// length. Every key stays in m, so presence, counts, key walks and the sorted
// caches are untouched; only a read of a row's VALUES resolves a sentinel
// (rowStore.resolve), which every such read in row_store*.go does.
//
// The sentinel is SELF-CONTAINED on purpose. A snapshot (clone) and an undo
// entry that set the whole map aside (rsUndo.prior) keep old maps, and a
// rowid-to-offset index shared by them would answer a restored map's sentinel
// with whatever version of the row was spilled last. A sentinel names its own
// immutable record, so every map that holds it reads exactly the row it held.
// The file is shared between a store and its clones for the same reason it is
// safe to: nothing in it is ever rewritten.
//
// The record round-trip is exact for a stored row: Typ, I, F and S are all a
// stored Value carries (a subtype never reaches storage, storedValues), and
// serialTypeOf keeps REAL as REAL.

// rowSpillThreshold is how many bytes of written rows a store holds in memory
// before it spills them. A variable so a test can make every store spill.
var rowSpillThreshold int64 = 64 << 20

// spilledRows counts rows written to a spill file, so a test can tell that the
// spill ran.
var spilledRows int64

// SpilledRowsForTest reads and clears spilledRows.
func SpilledRowsForTest() int64 { n := spilledRows; spilledRows = 0; return n }

// valueSpilled is a sentinel row's only Value type: I is the record's offset in
// the spill file, F its length. It never leaves the store -- resolve replaces
// it before any caller sees a row.
const valueSpilled ValueType = 0xF0

// spillFile is one append-only file of records, shared by a store and its
// clones.
type spillFile struct {
	f  *os.File
	at int64
}

// newSpillFile creates the file in the system temp directory, unlinked at once
// where the platform allows it, so a crash leaves nothing behind.
func newSpillFile() (*spillFile, error) {
	f, err := os.CreateTemp("", "musql-spill-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	unlinked := os.Remove(name) == nil
	sf := &spillFile{f: f}
	runtime.AddCleanup(sf, func(f *os.File) {
		f.Close()
		if !unlinked {
			os.Remove(f.Name())
		}
	}, f)
	return sf, nil
}

// rowMemSize is roughly what one row costs the heap: its slice header, its
// Values, and their payload bytes.
func rowMemSize(vals []Value) int64 {
	n := int64(24 + 48*len(vals))
	for i := range vals {
		n += int64(len(vals[i].S))
	}
	return n
}

// isSpilled reports whether a row in m is a sentinel.
func isSpilled(vals []Value) bool { return len(vals) == 1 && vals[0].Typ == valueSpilled }

// resolve is a row of m as its caller may see it: a sentinel read back from the
// spill file, anything else as it is.
//
// A row that cannot be read back is a disk failing under the process -- the
// same event as a media error on a mapped segment page, which this format
// already meets as a SIGBUS. Returning anything else (no row, a short row)
// would be a silent wrong answer, so it panics with the cause.
func (s *rowStore) resolve(vals []Value) []Value {
	if !isSpilled(vals) {
		return vals
	}
	off, n := vals[0].I, int(vals[0].F)
	buf := make([]byte, n)
	if _, err := s.spill.f.ReadAt(buf, off); err != nil {
		panic("engine: reading a spilled row back: " + err.Error())
	}
	row, err := decodeRecord(buf)
	if err != nil {
		panic("engine: decoding a spilled row: " + err.Error())
	}
	return row
}

// noteWritten counts a row put into m, and spills them all once they outgrow
// rowSpillThreshold.
func (s *rowStore) noteWritten(rowid uint64, vals []Value) {
	s.memBytes += rowMemSize(vals)
	s.unspilt = append(s.unspilt, rowid)
	if s.memBytes > rowSpillThreshold {
		s.spillOut()
	}
}

// spillOut writes every in-memory row of m to the spill file and leaves a
// sentinel in its place. A failure to write leaves the rows in memory, which is
// where they were: the transaction is no less correct, only bigger.
//
// It visits the keys put since the last spill (unspilt), not the whole map,
// so a spill costs what it writes. Rows set back by an undo or a whole-map
// restore are not in that list -- unspilt is reset with memBytes there -- and
// stay in memory until a later put pushes the store over again; correct, only
// not as small as it could be.
func (s *rowStore) spillOut() {
	if s.spill == nil {
		sf, err := newSpillFile()
		if err != nil {
			s.memBytes = 0 // do not retry on every put
			return
		}
		s.spill = sf
	}
	sf := s.spill
	const chunk = 1 << 20
	var buf []byte
	start := sf.at
	type pending struct {
		rowid uint64
		off   int64
		n     int
	}
	var batch []pending
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		if _, err := sf.f.WriteAt(buf, start); err != nil {
			return false
		}
		for _, p := range batch {
			s.m[p.rowid] = []Value{{Typ: valueSpilled, I: p.off, F: float64(p.n)}}
		}
		spilledRows += int64(len(batch))
		start += int64(len(buf))
		sf.at = start
		buf, batch = buf[:0], batch[:0]
		return true
	}
	for _, rowid := range s.unspilt {
		vals, ok := s.m[rowid]
		if !ok || isSpilled(vals) {
			continue // dropped since, or already written (a key put twice)
		}
		off := start + int64(len(buf))
		before := len(buf)
		buf = appendRecord(buf, vals)
		batch = append(batch, pending{rowid, off, len(buf) - before})
		if len(buf) >= chunk && !flush() {
			return
		}
	}
	if flush() {
		s.memBytes, s.unspilt = 0, s.unspilt[:0]
	}
}

// has reports whether rowid has a row, without reading it back.
func (s *rowStore) has(rowid uint64) bool {
	if s == nil {
		return false
	}
	if _, ok := s.m[rowid]; ok {
		return true
	}
	if s.seg == nil || s.baseGone || s.gone[rowid] {
		return false
	}
	return s.seg.has(rowid)
}

// recordLen is the length of rowid's row as a stored record with column ipk
// NULL (the rowid carries it) -- what appendRecordOf appends. A spilled row's
// is its spill record's, which was encoded from the same stored row.
func (s *rowStore) recordLen(rowid uint64, ipk int) int {
	if vals, ok := s.m[rowid]; ok && isSpilled(vals) {
		return int(vals[0].F)
	}
	vals, _ := s.get(rowid)
	return recordSize(storedIPKNull(vals, ipk))
}

// appendRecordOf appends rowid's row as a stored record: a spilled row's bytes
// exactly as the spill file holds them, any other row encoded.
func (s *rowStore) appendRecordOf(dst []byte, rowid uint64, ipk int) []byte {
	if vals, ok := s.m[rowid]; ok && isSpilled(vals) {
		off, n := vals[0].I, int(vals[0].F)
		dst = slices.Grow(dst, n)
		at := len(dst)
		dst = dst[:at+n]
		if _, err := s.spill.f.ReadAt(dst[at:], off); err != nil {
			panic("engine: reading a spilled row back: " + err.Error()) // see resolve
		}
		return dst
	}
	vals, _ := s.get(rowid)
	return appendRecord(dst, storedIPKNull(vals, ipk))
}

// storedIPKNull is vals with column ipk NULL -- the stored shape, in which the
// rowid carries an INTEGER PRIMARY KEY's value -- copied only when it is not
// already so.
func storedIPKNull(vals []Value, ipk int) []Value {
	if ipk >= 0 && ipk < len(vals) && vals[ipk].Typ != Null {
		vals = append([]Value(nil), vals...)
		vals[ipk] = Value{Typ: Null}
	}
	return vals
}
