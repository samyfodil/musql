package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// A segment FILE: the columnar side of docs/format-design.md on disk, and the
// half of "mmap + JIT" that the format needs to be worth anything.
//
// A segment's int64 column block is only free to read if the bytes behind it
// are already in the address space and 8-byte aligned. A mapping gives both; a
// read into a fresh buffer gives neither for free. So every segment in this
// file starts at an 8-byte boundary, and OpenSegmentFile hands the mapped bytes
// straight to openSegment without copying them.
//
// Layout, all integers little-endian:
//
//	header      24 bytes: magic, version, table count, directory length, and
//	            the SOURCE database's change counter and page count
//	directory   one entry per table: name, CREATE text, column names, the index
//	            of the INTEGER PRIMARY KEY column (-1 for none), and the
//	            (offset, length) of each of its segments; then the CATALOG --
//	            every index, view and trigger, and every AUTOINCREMENT counter
//	segments    each 8-byte aligned, in directory order
const (
	segFileMagic   = "MQSF"
	segFileVersion = 4
	segFileHdrSize = 24
)

// SegmentFile is a mapped segment file.
type SegmentFile struct {
	m      *mmapfile.File
	tables []SegmentFileTable

	// srcChangeCounter and srcPageCount are the source database's, as it stood
	// when this file was written. A segment file is DERIVED data, and derived
	// data that has silently fallen behind its source is not slow, it is
	// WRONG -- so AttachSegments refuses one whose source has moved on rather
	// than answering from it. See errSegmentsStale.
	srcChangeCounter uint32
	srcPageCount     uint32

	// catalog is every index, view and trigger, and every AUTOINCREMENT counter.
	// Without it the file holds rows and no schema, which is why a round trip
	// used to drop half a database -- see segment_catalog.go.
	catalog ConvertedCatalog
}

// Catalog is the non-table half of the schema this file carries.
func (f *SegmentFile) Catalog() ConvertedCatalog { return f.catalog }

// errSegmentsStale is returned when a segment file was built from a different
// state of the database than the one being read.
var errSegmentsStale = errors.New("engine: segment file is stale: the database has changed since it was written")

// SegmentFileTable is one table's entry: what it was, and where its segments
// are in the mapping.
type SegmentFileTable struct {
	Name string
	SQL  string
	Cols []string
	// IPK is the INTEGER PRIMARY KEY column's index, or -1. See
	// ConvertedTable.IPK for why it has to be stored rather than derived.
	IPK int
	// Rank is ConvertedTable.Rank, as the file recorded it.
	Rank uint32
	// Rowid is ConvertedTable.Rowid, as the file recorded it.
	Rowid int64
	// Root is ConvertedTable.Root, as the file recorded it.
	Root uint32
	segs [][]byte
}

// RootOf is the synthetic root table ti is keyed by: the one the session that
// wrote the file held it under, or its directory position for a file that
// recorded none (the converter's, or one written before the field existed).
//
// It has to be the session's. A rewrite used to re-key every table to its
// position, and a session holding a table across it kept the old number: after
// "CREATE TABLE a; CREATE TABLE b; DROP TABLE a" b's place was 0 while the
// session still held it at 1, so the next table created was handed 2, the
// rewrite after it put that one at place 1, and b's reload read its rows --
// "CREATE TABLE c(y); INSERT INTO c VALUES(1); SELECT count(*) FROM b" answered
// 1, through the driver, which holds one session across autocommit statements.
func (f *SegmentFile) RootOf(ti int) uint32 {
	if r := f.tables[ti].Root; r != 0 {
		return r
	}
	return uint32(segRootBase + ti)
}

// Segments is the number of segments this table was written as.
func (t SegmentFileTable) Segments() int { return len(t.segs) }

// Tables is every table in the file.
func (f *SegmentFile) Tables() []SegmentFileTable { return f.tables }

// Mapped reports whether the bytes came from a mapping rather than a read.
func (f *SegmentFile) Mapped() bool { return f != nil && f.m.Mapped() }

// Close releases the mapping.
func (f *SegmentFile) Close() error {
	if f == nil {
		return nil
	}
	f.tables = nil
	return f.m.Close()
}


// WriteSegmentFileWithCatalog writes converted tables and the rest of the
// database's schema out as one segment file. It is segFileWriter over segments
// already in memory; a caller producing them one at a time streams them in
// instead (CompactSegmentFile).
func WriteSegmentFileWithCatalog(path string, tables []ConvertedTable, cat ConvertedCatalog, srcChangeCounter, srcPageCount uint32) error {
	w, err := newSegFileWriter(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer w.discard()
	for ti, t := range tables {
		for _, raw := range t.Segments {
			if err := w.addSegment(ti, raw); err != nil {
				return err
			}
		}
	}
	return w.finish(path, tables, cat, srcChangeCounter, srcPageCount)
}

// segFileWriter builds a segment file holding ONE segment in memory at a time.
//
// The directory records every segment's offset and comes before them, and how
// many segments each table has is not known until the last is built -- so the
// segments go to a scratch file beside the target as they arrive, each 8-byte
// aligned there, and finish writes the header and directory and then copies
// the scratch in after them. The layout is exactly the in-memory writer's:
// segments start at the 8-aligned end of the directory, each 8-aligned.
//
// It exists because the in-memory writer held every segment of the database and
// then a whole second image of the file: compacting a 600 MB delta peaked at
// 5 GB of RSS, a 2.83 GB one (bigsort.test 1.0) at 23 GB.
//
// Up to segWriterMemBytes the segments are simply held -- most rewrites are a
// small database's DDL, and a scratch file per CREATE TABLE cost the engine
// suite 8% -- and the scratch file only takes over past that.
type segFileWriter struct {
	dir     string
	scratch *os.File // nil while the segments fit in mem
	mem     []byte   // the segments so far, laid out as the scratch would hold them
	at      int64
	segs    [][][2]int64 // per table: each segment's (offset, length) in mem or the scratch
}

// segWriterMemBytes is how many bytes of segments a writer holds before it
// moves them to a scratch file.
var segWriterMemBytes int64 = 16 << 20

func newSegFileWriter(dir string) (*segFileWriter, error) {
	return &segFileWriter{dir: dir}, nil
}

// addSegment appends one of table ti's segments, in order.
func (w *segFileWriter) addSegment(ti int, raw []byte) error {
	for len(w.segs) <= ti {
		w.segs = append(w.segs, nil)
	}
	at := segAlign8Int64(w.at)
	if w.scratch == nil && at+int64(len(raw)) > segWriterMemBytes {
		f, err := os.CreateTemp(w.dir, ".segments-*")
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(w.mem, 0); err != nil {
			f.Close()
			os.Remove(f.Name())
			return err
		}
		w.scratch, w.mem = f, nil
	}
	if w.scratch != nil {
		if _, err := w.scratch.WriteAt(raw, at); err != nil {
			return err
		}
	} else {
		for int64(len(w.mem)) < at {
			w.mem = append(w.mem, 0)
		}
		w.mem = append(w.mem, raw...)
	}
	w.segs[ti] = append(w.segs[ti], [2]int64{at, int64(len(raw))})
	w.at = at + int64(len(raw))
	return nil
}

// finish writes the file at path: tables' metadata (their Segments are not
// read -- addSegment supplied them), the catalog, and the scratch's segments.
func (w *segFileWriter) finish(path string, tables []ConvertedTable, cat ConvertedCatalog, srcChangeCounter, srcPageCount uint32) error {
	var dir []byte
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(tables)))
	// The directory records each segment's offset, which is not known until the
	// directory's own length is -- so lay the directory out first with zeroed
	// offsets, remember where each one sits, then fill them in.
	type patch struct {
		at      int
		scratch int64
	}
	var patches []patch
	for ti, t := range tables {
		dir = appendLenString(dir, t.Name)
		dir = appendLenString(dir, t.SQL)
		dir = binary.LittleEndian.AppendUint32(dir, uint32(len(t.Cols)))
		for _, c := range t.Cols {
			dir = appendLenString(dir, c)
		}
		// The IPK column, which version 2 did not record and could not
		// reconstruct. It is not a convenience: that column is the ROWID under
		// another name and the stored record holds a NULL in it (see
		// ConvertedTable.IPK), so a file without it cannot be converted back to
		// SQLite, and COMPACTION cannot rebuild its own segments. "The format is
		// self-sufficient" was false for exactly this field.
		dir = binary.LittleEndian.AppendUint64(dir, uint64(int64(t.IPK)))
		var segs [][2]int64
		if ti < len(w.segs) {
			segs = w.segs[ti]
		}
		dir = binary.LittleEndian.AppendUint32(dir, uint32(len(segs)))
		for _, sg := range segs {
			patches = append(patches, patch{at: len(dir), scratch: sg[0]})
			dir = binary.LittleEndian.AppendUint64(dir, 0) // offset, patched below
			dir = binary.LittleEndian.AppendUint64(dir, uint64(sg[1]))
		}
	}

	// The CATALOG, after every table entry: what the file needs to be restored
	// from, and to be opened as a database at all.
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(cat.Objects)))
	for _, o := range cat.Objects {
		dir = appendLenString(dir, o.Type)
		dir = appendLenString(dir, o.Name)
		dir = appendLenString(dir, o.TblName)
		dir = appendLenString(dir, o.SQL)
	}
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(cat.Sequences)))
	for _, sq := range cat.Sequences {
		dir = appendLenString(dir, sq.Table)
		dir = binary.LittleEndian.AppendUint64(dir, uint64(sq.Seq))
	}
	// The schema cookie, after the sequences so a v4 reader that predates it stops
	// at a clean boundary rather than mid-record. The two application-owned words
	// follow it on the same terms -- see ConvertedCatalog.UserVersion.
	dir = binary.LittleEndian.AppendUint32(dir, cat.SchemaVersion)
	dir = binary.LittleEndian.AppendUint32(dir, cat.UserVersion)
	dir = binary.LittleEndian.AppendUint32(dir, cat.ApplicationID)
	dir = binary.LittleEndian.AppendUint32(dir, cat.Encoding)
	dir = binary.LittleEndian.AppendUint32(dir, cat.PageSize)
	dir = binary.LittleEndian.AppendUint32(dir, cat.AutoVacuumPlus1)
	// THE CATALOG ORDER: every table's Rank, then every object's -- the one
	// thing the split into a table list and an object list loses. C lists
	// sqlite_schema in rowid order, which is creation order ("SELECT
	// group_concat(name) FROM sqlite_schema"), and a reader rebuilds that
	// order from these (segment_open.go). Last, like every field before it,
	// so a reader that predates it stops at a clean boundary.
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(tables)+len(cat.Objects)))
	for _, t := range tables {
		dir = binary.LittleEndian.AppendUint32(dir, t.Rank)
	}
	for _, o := range cat.Objects {
		dir = binary.LittleEndian.AppendUint32(dir, o.Rank)
	}
	// ...and every entry's sqlite_schema ROWID, which is not its rank: C numbers a
	// new row max(rowid)+1, so a DROP leaves a gap the rank closes (SchemaRow.Rowid).
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(tables)+len(cat.Objects)))
	for _, t := range tables {
		dir = binary.LittleEndian.AppendUint64(dir, uint64(t.Rowid))
	}
	for _, o := range cat.Objects {
		dir = binary.LittleEndian.AppendUint64(dir, uint64(o.Rowid))
	}
	dir = binary.LittleEndian.AppendUint32(dir, cat.JournalWAL)
	// ...and the catalog rows whose rootpage names storage other than their own
	// (ConvertedCatalog.RootEdits), last, on the same terms.
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(cat.RootEdits)))
	for _, re := range cat.RootEdits {
		dir = appendLenString(dir, re.Name)
		dir = binary.LittleEndian.AppendUint64(dir, uint64(re.Root))
	}
	// ...and every table's ROUTING ROOT (SegmentFile.RootOf), on the same terms.
	dir = binary.LittleEndian.AppendUint32(dir, uint32(len(tables)))
	for _, t := range tables {
		dir = binary.LittleEndian.AppendUint32(dir, t.Root)
	}
	// ...and the capture guard (ConvertedCatalog.CaptureGuard), last.
	dir = appendLenString(dir, cat.CaptureGuard)

	head := make([]byte, segFileHdrSize+len(dir))
	copy(head, segFileMagic)
	binary.LittleEndian.PutUint32(head[4:], segFileVersion)
	binary.LittleEndian.PutUint32(head[8:], uint32(len(tables)))
	binary.LittleEndian.PutUint32(head[12:], uint32(len(dir)))
	binary.LittleEndian.PutUint32(head[16:], srcChangeCounter)
	binary.LittleEndian.PutUint32(head[20:], srcPageCount)
	copy(head[segFileHdrSize:], dir)
	// Every segment starts 8-byte aligned, or the zero-copy column cast is not
	// available: the scratch's offsets are 8-aligned, so they stay so after a
	// base that is.
	base := segAlign8Int64(int64(len(head)))
	for _, p := range patches {
		binary.LittleEndian.PutUint64(head[segFileHdrSize+p.at:], uint64(base+p.scratch))
	}
	out, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.WriteAt(head, 0); err != nil {
		out.Close()
		return err
	}
	if w.scratch == nil {
		if _, err := out.WriteAt(w.mem, base); err != nil {
			out.Close()
			return err
		}
	} else if w.at > 0 {
		if _, err := w.scratch.Seek(0, io.SeekStart); err != nil {
			out.Close()
			return err
		}
		if _, err := out.Seek(base, io.SeekStart); err != nil {
			out.Close()
			return err
		}
		// A LimitedReader over the file, not a SectionReader: it is what lets
		// os.File.ReadFrom hand the copy to copy_file_range on Linux.
		if _, err := io.Copy(out, io.LimitReader(w.scratch, w.at)); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}

// discard removes the scratch file, if there is one; it is safe after finish.
func (w *segFileWriter) discard() {
	w.mem = nil
	if w.scratch == nil {
		return
	}
	name := w.scratch.Name()
	w.scratch.Close()
	os.Remove(name)
}

func segAlign8Int64(n int64) int64 { return (n + 7) &^ 7 }

// OpenSegmentFile maps a segment file. The segments it hands back alias the
// mapping, so they cost nothing to open and are invalid after Close.
func OpenSegmentFile(path string) (*SegmentFile, error) {
	m, err := mmapfile.Open(path)
	if err != nil {
		return nil, err
	}
	buf := m.Data()
	if len(buf) < segFileHdrSize || string(buf[:4]) != segFileMagic {
		m.Close()
		return nil, fmt.Errorf("segment file: not a segment file")
	}
	if v := binary.LittleEndian.Uint32(buf[4:]); v != segFileVersion {
		m.Close()
		return nil, fmt.Errorf("segment file: version %d, this build writes %d", v, segFileVersion)
	}
	nTables := int(binary.LittleEndian.Uint32(buf[8:]))
	dirLen := int(binary.LittleEndian.Uint32(buf[12:]))
	if segFileHdrSize+dirLen > len(buf) {
		m.Close()
		return nil, fmt.Errorf("segment file: directory overruns the file")
	}
	d := &segDirReader{buf: buf[segFileHdrSize : segFileHdrSize+dirLen]}
	if got := int(d.u32()); got != nTables {
		m.Close()
		return nil, fmt.Errorf("segment file: header says %d tables, directory says %d", nTables, got)
	}
	f := &SegmentFile{m: m,
		srcChangeCounter: binary.LittleEndian.Uint32(buf[16:]),
		srcPageCount:     binary.LittleEndian.Uint32(buf[20:])}
	for i := 0; i < nTables; i++ {
		t := SegmentFileTable{Name: d.str(), SQL: d.str()}
		nCols := int(d.u32())
		for c := 0; c < nCols; c++ {
			t.Cols = append(t.Cols, d.str())
		}
		t.IPK = int(int64(d.u64()))
		nSegs := int(d.u32())
		for sgi := 0; sgi < nSegs; sgi++ {
			off, ln := d.u64(), d.u64()
			if d.err != nil || off+ln > uint64(len(buf)) {
				m.Close()
				return nil, fmt.Errorf("segment file: table %q segment %d overruns the file", t.Name, sgi)
			}
			t.segs = append(t.segs, buf[off:off+ln])
		}
		f.tables = append(f.tables, t)
	}
	nObjs := int(d.u32())
	for i := 0; i < nObjs; i++ {
		f.catalog.Objects = append(f.catalog.Objects, ConvertedObject{
			Type: d.str(), Name: d.str(), TblName: d.str(), SQL: d.str(),
		})
	}
	nSeqs := int(d.u32())
	for i := 0; i < nSeqs; i++ {
		f.catalog.Sequences = append(f.catalog.Sequences, ConvertedSequence{
			Table: d.str(), Seq: int64(d.u64()),
		})
	}
	// The trailing words may be absent in a file written before they existed; a
	// short read leaves each 0, which is what those files meant.
	if d.remaining() >= 4 {
		f.catalog.SchemaVersion = d.u32()
	}
	if d.remaining() >= 4 {
		f.catalog.UserVersion = d.u32()
	}
	if d.remaining() >= 4 {
		f.catalog.ApplicationID = d.u32()
	}
	if d.remaining() >= 4 {
		f.catalog.Encoding = d.u32()
	}
	if d.remaining() >= 4 {
		f.catalog.PageSize = d.u32()
	}
	if d.remaining() >= 4 {
		f.catalog.AutoVacuumPlus1 = d.u32()
	}
	if d.remaining() >= 4 {
		if n := int(d.u32()); n == len(f.tables)+len(f.catalog.Objects) {
			for i := range f.tables {
				f.tables[i].Rank = d.u32()
			}
			for i := range f.catalog.Objects {
				f.catalog.Objects[i].Rank = d.u32()
			}
		}
	}
	if d.remaining() >= 4 {
		if n := int(d.u32()); n == len(f.tables)+len(f.catalog.Objects) {
			for i := range f.tables {
				f.tables[i].Rowid = int64(d.u64())
			}
			for i := range f.catalog.Objects {
				f.catalog.Objects[i].Rowid = int64(d.u64())
			}
		}
	}
	if d.remaining() >= 4 {
		f.catalog.JournalWAL = d.u32()
	}
	if d.remaining() >= 4 {
		n := int(d.u32())
		for i := 0; i < n && d.err == nil; i++ {
			name := d.str()
			f.catalog.RootEdits = append(f.catalog.RootEdits, ConvertedRootEdit{Name: name, Root: int64(d.u64())})
		}
	}
	if d.remaining() >= 4 {
		if n := int(d.u32()); n == len(f.tables) {
			for i := range f.tables {
				f.tables[i].Root = d.u32()
			}
		}
	}
	if d.remaining() >= 4 {
		f.catalog.CaptureGuard = d.str()
	}
	if d.err != nil {
		m.Close()
		return nil, fmt.Errorf("segment file: directory is malformed: %w", d.err)
	}
	return f, nil
}

// openSegments opens every segment of one table, over the mapped bytes.
func (t SegmentFileTable) openSegments() ([]*segment, error) {
	out := make([]*segment, 0, len(t.segs))
	for i, raw := range t.segs {
		s, err := openSegment(raw)
		if err != nil {
			return nil, fmt.Errorf("table %q segment %d: %w", t.Name, i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func appendLenString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// segDirReader walks the directory, remembering the first overrun rather than
// panicking on it: a malformed file is an error at open, never a fault in a
// scan.
type segDirReader struct {
	buf []byte
	pos int
	err error
}

func (d *segDirReader) need(n int) bool {
	if d.err != nil || d.pos+n > len(d.buf) {
		if d.err == nil {
			d.err = fmt.Errorf("directory truncated at byte %d", d.pos)
		}
		return false
	}
	return true
}

// remaining is how many bytes of the directory are still unread, so a field
// APPENDED to the format can be read when present and defaulted when not.
func (d *segDirReader) remaining() int {
	if d.err != nil {
		return 0
	}
	return len(d.buf) - d.pos
}

func (d *segDirReader) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v
}

func (d *segDirReader) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return v
}

func (d *segDirReader) str() string {
	n := int(d.u32())
	if !d.need(n) {
		return ""
	}
	s := string(d.buf[d.pos : d.pos+n])
	d.pos += n
	return s
}
