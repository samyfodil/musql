package engine

import (
	"os"
	"runtime"
	"slices"

	"github.com/samyfodil/musql/internal/mmapfile"
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

// valueSpilled is a record sentinel's Value type: I is the record's offset in
// the spill file, F its length. valueSpilledSeg is a segment sentinel's: I is
// the spill segment's index (spillFile.segs), F the row's position in it.
// Neither leaves the store -- resolve replaces it before any caller sees a row.
const (
	valueSpilled    ValueType = 0xF0
	valueSpilledSeg ValueType = 0xF1
)

// spillFile is one append-only file of records, or of segments, shared by a
// store and its clones.
//
// Where the platform maps a range of a file (mmapfile.CanMapAt) a spill writes
// SEGMENTS, the same bytes a commit's rewrite would build from those rows, and
// maps each one back: a read of a spilled row is a segment row read, and a
// commit that finds a spilled segment's rows still exactly that segment's
// copies its bytes rather than reading the rows back and building them again
// (spillRun, segment_ddl.go). Elsewhere it writes records.
type spillFile struct {
	f    *os.File
	at   int64
	segs []*spillSeg
	res  *spillMaps // what the cleanup releases: the file's segment mappings
}

// spillSeg is one segment in a spill file, mapped.
type spillSeg struct {
	seg *segment
	m   *mmapfile.File
}

// spillMaps is a spill file's mappings, apart from the spillFile so its cleanup
// can release them.
type spillMaps struct{ ms []*mmapfile.File }

// spillSegments is whether spills write segments (see spillFile). A variable
// so a test can run the record path where the platform maps.
var spillSegments = mmapfile.CanMapAt

// newSpillFile creates the file in the system temp directory, unlinked at once
// where the platform allows it, so a crash leaves nothing behind.
func newSpillFile() (*spillFile, error) {
	f, err := os.CreateTemp("", "musql-spill-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	unlinked := os.Remove(name) == nil
	sf := &spillFile{f: f, res: &spillMaps{}}
	type held struct {
		f   *os.File
		res *spillMaps
	}
	runtime.AddCleanup(sf, func(h held) {
		for _, m := range h.res.ms {
			m.Close()
		}
		h.f.Close()
		if !unlinked {
			os.Remove(h.f.Name())
		}
	}, held{f, sf.res})
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
func isSpilled(vals []Value) bool {
	return len(vals) == 1 && (vals[0].Typ == valueSpilled || vals[0].Typ == valueSpilledSeg)
}

// spilledSegRow is a segment sentinel's segment and row, or nil.
func (s *rowStore) spilledSegRow(vals []Value) (*spillSeg, int) {
	if len(vals) != 1 || vals[0].Typ != valueSpilledSeg {
		return nil, 0
	}
	return s.spill.segs[vals[0].I], int(vals[0].F)
}

// segRowOwned is row i of a spill segment, its bytes copied out of the
// mapping: a row handed to a caller may outlive the spill file.
func segRowOwned(sg *spillSeg, i int) []Value {
	row := sg.seg.rowAt(i)
	for c := range row {
		if row[c].S != nil {
			row[c].S = append([]byte(nil), row[c].S...)
		}
	}
	return row
}

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
	if sg, i := s.spilledSegRow(vals); sg != nil {
		return segRowOwned(sg, i)
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

// spillReader is resolve for a walk over many rows: spilled records are read
// a window at a time rather than one read each. A walk in rowid order meets
// the records mostly in the order they were spilled, so most come out of the
// window already read; one that does not starts a new window. Each window is
// a fresh buffer, as each resolve's is, so a row handed out stays valid.
type spillReader struct {
	buf []byte
	off int64 // the file offset of buf[0]
}

// spillWindow is how much a spillReader reads at once.
const spillWindow = 1 << 20

func (r *spillReader) resolve(s *rowStore, vals []Value) []Value {
	if !isSpilled(vals) {
		return vals
	}
	if sg, i := s.spilledSegRow(vals); sg != nil {
		return segRowOwned(sg, i)
	}
	off, n := vals[0].I, int64(vals[0].F)
	if off < r.off || off+n > r.off+int64(len(r.buf)) {
		size := min(max(n, spillWindow), s.spill.at-off)
		if size < n {
			return s.resolve(vals)
		}
		buf := make([]byte, size)
		if _, err := s.spill.f.ReadAt(buf, off); err != nil {
			panic("engine: reading a spilled row back: " + err.Error())
		}
		r.buf, r.off = buf, off
	}
	row, err := decodeRecord(r.buf[off-r.off : off-r.off+n : off-r.off+n])
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
	if spillSegments {
		if s.spillOutSegments() {
			s.memBytes, s.unspilt = 0, s.unspilt[:0]
		}
		return
	}
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
	if vals, ok := s.m[rowid]; ok && isSpilled(vals) && vals[0].Typ == valueSpilled {
		return int(vals[0].F)
	}
	if vals, ok := s.m[rowid]; ok {
		// A segment-spilled row is measured where it is mapped: nothing that
		// outlives this call keeps it, so its bytes need no copy.
		if sg, i := s.spilledSegRow(vals); sg != nil {
			return recordSize(storedIPKNull(sg.seg.rowAt(i), ipk))
		}
	}
	vals, _ := s.get(rowid)
	return recordSize(storedIPKNull(vals, ipk))
}

// appendRecordOf appends rowid's row as a stored record: a spilled row's bytes
// exactly as the spill file holds them, any other row encoded.
func (s *rowStore) appendRecordOf(dst []byte, rowid uint64, ipk int) []byte {
	if vals, ok := s.m[rowid]; ok && isSpilled(vals) && vals[0].Typ == valueSpilled {
		off, n := vals[0].I, int(vals[0].F)
		dst = slices.Grow(dst, n)
		at := len(dst)
		dst = dst[:at+n]
		if _, err := s.spill.f.ReadAt(dst[at:], off); err != nil {
			panic("engine: reading a spilled row back: " + err.Error()) // see resolve
		}
		return dst
	}
	if vals, ok := s.m[rowid]; ok {
		if sg, i := s.spilledSegRow(vals); sg != nil {
			return appendRecord(dst, storedIPKNull(sg.seg.rowAt(i), ipk)) // encoded at once: no copy needed
		}
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

// spillOutSegments is spillOut writing segments: the rows put since the last
// spill, in rowid order, cut as a commit would cut them (segCutter), each
// segment written at a page boundary and mapped back, and a segment sentinel
// left for each row. It reports false, leaving every row it has not yet
// replaced in memory, when a write or a mapping fails.
func (s *rowStore) spillOutSegments() bool {
	sf := s.spill
	rowids := make([]uint64, 0, len(s.unspilt))
	nCols := 0
	for _, rowid := range s.unspilt {
		if vals, ok := s.m[rowid]; ok && !isSpilled(vals) {
			rowids = append(rowids, rowid)
			nCols = max(nCols, len(vals))
		}
	}
	if len(rowids) == 0 {
		return true
	}
	radixSortRowids(rowids)
	rowids = slices.Compact(rowids) // a key put twice
	if nCols == 0 {
		return false // rows of no columns are not a segment; keep them
	}
	page := int64(os.Getpagesize())
	ok := true
	start := 0
	cut := &segCutter{cols: make([]columnInfo, nCols), what: "a spill", emit: func(raw []byte) error {
		at := (sf.at + page - 1) / page * page
		if _, err := sf.f.WriteAt(raw, at); err != nil {
			return err
		}
		m, err := mmapfile.MapAt(sf.f, at, len(raw))
		if err != nil {
			return err
		}
		seg, err := openSegment(m.Data())
		if err != nil {
			m.Close()
			return err
		}
		sf.at = at + int64(len(raw))
		sf.res.ms = append(sf.res.ms, m)
		idx := int64(len(sf.segs))
		sf.segs = append(sf.segs, &spillSeg{seg: seg, m: m})
		for i := 0; i < seg.nRows; i++ {
			s.m[rowids[start+i]] = []Value{{Typ: valueSpilledSeg, I: idx, F: float64(i)}}
		}
		start += seg.nRows
		spilledRows += int64(seg.nRows)
		return nil
	}}
	for _, rowid := range rowids {
		if cut.add(rowid, s.m[rowid]) != nil {
			ok = false
			break
		}
	}
	if ok && cut.flush() != nil {
		ok = false
	}
	return ok
}
