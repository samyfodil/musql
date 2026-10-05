package engine

import "fmt"

// ConvertedTable is one table in columnar form, with the schema text it was
// created from so it can be rebuilt exactly.
type ConvertedTable struct {
	Name     string
	SQL      string
	Cols     []string
	Segments [][]byte

	// IPK is the index of the INTEGER PRIMARY KEY column, or -1. It is not a
	// convenience: that column is the ROWID under another name, the stored
	// record holds a NULL in it, and an INSERT that named both it and the
	// rowid would set the two from different values. Restoring a table means
	// naming the rowid and leaving this column out.
	IPK int

	// Rank is this table's 1-based position in sqlite_schema order, or 0
	// where none was recorded. See ConvertedCatalog's
	// catalog-order note (segment_file.go).
	Rank uint32
	// Rowid is its sqlite_schema rowid, or 0 where none was recorded. See
	// SchemaRow.Rowid.
	Rowid int64
	// Root is the synthetic root a session keys this table by, or 0 for its
	// directory position (segRootBase + index). See SegmentFile.RootOf.
	Root uint32
}

// Rows is how many rows the table held, across every segment.
func (c ConvertedTable) Rows() int {
	n := 0
	for _, raw := range c.Segments {
		if s, err := openSegment(raw); err == nil {
			n += s.nRows
		}
	}
	return n
}

// segmentRows is how many rows one segment holds. Bounded so that a mapping
// covers a predictable amount on 32-bit targets, and so a rewrite touches one
// segment rather than a table.
const segmentRows = 1 << 16

// segmentBytes bounds a segment by its rows' size as well: 65,536 rows of
// 10 KB blobs is a 650 MB segment, and building one holds about twice that --
// bigsort.test 1.0's compaction peaked at 4 GB of heap on segments alone. A
// narrow table never reaches it; its segments stay segmentRows long.
const segmentBytes = 32 << 20

// segCutter collects one table's rows in rowid order and builds them into
// segments as they fill -- segmentRows rows or segmentBytes of row data,
// whichever comes first -- handing each to emit, so a builder holds one
// segment's rows at a time and never the table.
type segCutter struct {
	cols   []columnInfo
	emit   func(raw []byte) error
	what   string // the table, for an error
	rowids []uint64
	rows   [][]Value
	bytes  int64
}

// add appends one row, cutting a segment when it fills one.
func (c *segCutter) add(rowid uint64, vals []Value) error {
	c.rowids = append(c.rowids, rowid)
	c.rows = append(c.rows, vals)
	c.bytes += rowMemSize(vals)
	if len(c.rows) >= segmentRows || c.bytes >= segmentBytes {
		return c.flush()
	}
	return nil
}

// flush builds whatever rows are pending into a segment.
func (c *segCutter) flush() error {
	if len(c.rows) == 0 {
		return nil
	}
	raw, err := buildSegment(c.cols, c.rowids, c.rows)
	if err != nil {
		return fmt.Errorf("building a segment of %s: %w", c.what, err)
	}
	clear(c.rows) // let the rows go before the next segment's arrive
	c.rowids, c.rows, c.bytes = c.rowids[:0], c.rows[:0], 0
	return c.emit(raw)
}

// segmentFull reports whether a segment of n rows and raw bytes is as large
// as segCutter would make one -- so carrying it over leaves the table cut the
// way building it afresh would.
func segmentFull(n int, raw []byte) bool { return n >= segmentRows || len(raw) >= segmentBytes }
