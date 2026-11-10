package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// A CATALOG RECORD (segDeltaCatalog) is how a commit that changes the catalog
// but not the table directory -- a view, a trigger, a header word -- reaches
// the file without rewriting it: the whole catalog after the commit, appended
// in the commit's batch like any row. A replay keeps the last one, and an open
// applies it over the file's own catalog (SegmentFile.applyDeltaCatalog), so
// every reader of SegmentFile.Catalog and of each table's Rank and Rowid sees
// the catalog as it now is. A rewrite or a compaction folds it into the file.
//
// Each table's Rank and Rowid are part of it: they live in the directory, and a
// DROP VIEW renumbers the ranks after it. The AUTOINCREMENT counters
// (ConvertedCatalog.Sequences) are not: the engine keeps them in
// sqlite_sequence's own rows, and the file's are kept as they were.

// deltaCatalog is a decoded catalog record.
type deltaCatalog struct {
	ranks  []uint32 // each table's Rank, in directory order
	rowids []int64  // ...and its Rowid
	cat    ConvertedCatalog
}

// errDeltaPredatesCatalog refuses a catalog record for a delta older than
// version 7, which cannot hold one: the commit rewrites the file instead, and
// the delta that rewrite starts is the current version.
var errDeltaPredatesCatalog = errors.New("engine: segment delta: version predates catalog records")

// hasCatalogRecord reports whether recs holds a catalog record.
func hasCatalogRecord(recs []SegDeltaRecord) bool {
	for i := range recs {
		if recs[i].Catalog != nil {
			return true
		}
	}
	return false
}

// encodeDeltaCatalog is a catalog record's body for tables, as the directory
// lists them, and cat.
func encodeDeltaCatalog(tables []ConvertedTable, cat ConvertedCatalog) []byte {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, uint32(len(tables)))
	for _, t := range tables {
		b = binary.LittleEndian.AppendUint32(b, t.Rank)
		b = binary.LittleEndian.AppendUint64(b, uint64(t.Rowid))
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(cat.Objects)))
	for _, o := range cat.Objects {
		b = appendLenString(b, o.Type)
		b = appendLenString(b, o.Name)
		b = appendLenString(b, o.TblName)
		b = appendLenString(b, o.SQL)
		b = binary.LittleEndian.AppendUint32(b, o.Rank)
		b = binary.LittleEndian.AppendUint64(b, uint64(o.Rowid))
	}
	for _, w := range []uint32{cat.SchemaVersion, cat.UserVersion, cat.ApplicationID, cat.Encoding,
		cat.PageSize, cat.AutoVacuumPlus1, cat.JournalWAL} {
		b = binary.LittleEndian.AppendUint32(b, w)
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(cat.RootEdits)))
	for _, re := range cat.RootEdits {
		b = appendLenString(b, re.Name)
		b = binary.LittleEndian.AppendUint64(b, uint64(re.Root))
	}
	return appendLenString(b, cat.CaptureGuard)
}

// decodeDeltaCatalog reads a catalog record's body. Its strings are copies, so
// the record outlives the mapping it was read from.
func decodeDeltaCatalog(body []byte) (*deltaCatalog, error) {
	d := &segDirReader{buf: body}
	dc := &deltaCatalog{}
	n := int(d.u32())
	if n < 0 || n > d.remaining()/12 {
		return nil, fmt.Errorf("engine: segment delta: catalog record claims %d tables", n)
	}
	for i := 0; i < n; i++ {
		dc.ranks = append(dc.ranks, d.u32())
		dc.rowids = append(dc.rowids, int64(d.u64()))
	}
	nObjs := int(d.u32())
	for i := 0; i < nObjs && d.err == nil; i++ {
		o := ConvertedObject{Type: d.str(), Name: d.str(), TblName: d.str(), SQL: d.str()}
		o.Rank = d.u32()
		o.Rowid = int64(d.u64())
		dc.cat.Objects = append(dc.cat.Objects, o)
	}
	for _, w := range []*uint32{&dc.cat.SchemaVersion, &dc.cat.UserVersion, &dc.cat.ApplicationID, &dc.cat.Encoding,
		&dc.cat.PageSize, &dc.cat.AutoVacuumPlus1, &dc.cat.JournalWAL} {
		*w = d.u32()
	}
	nEdits := int(d.u32())
	for i := 0; i < nEdits && d.err == nil; i++ {
		name := d.str()
		dc.cat.RootEdits = append(dc.cat.RootEdits, ConvertedRootEdit{Name: name, Root: int64(d.u64())})
	}
	dc.cat.CaptureGuard = d.str()
	if d.err != nil {
		return nil, fmt.Errorf("engine: segment delta: catalog record is malformed: %w", d.err)
	}
	if d.remaining() != 0 {
		return nil, fmt.Errorf("engine: segment delta: catalog record has %d bytes past its end", d.remaining())
	}
	return dc, nil
}

// applyDeltaCatalog makes dc the file's catalog, as an open of the pair reads
// it. It names every table of the directory; one that does not was written for
// another file, which the delta's own pairing already refuses.
func (f *SegmentFile) applyDeltaCatalog(dc *deltaCatalog) error {
	if dc == nil {
		return nil
	}
	if len(dc.ranks) != len(f.tables) {
		return fmt.Errorf("engine: segment delta: catalog record names %d tables; the file has %d", len(dc.ranks), len(f.tables))
	}
	// The directory is shared by every handle over the mapping
	// (segment_file_share.go); this handle's catalog is its own.
	f.tables = slices.Clone(f.tables)
	for i := range f.tables {
		f.tables[i].Rank, f.tables[i].Rowid = dc.ranks[i], dc.rowids[i]
	}
	seqs := f.catalog.Sequences
	f.catalog = dc.cat
	f.catalog.Sequences = seqs
	return nil
}
