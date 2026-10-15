package engine

import (
	"fmt"
	"slices"
)

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

	// reuse builds each segment in the previous one's memory: for an emit
	// that has finished with a segment's bytes when it returns (one that
	// writes them out), so a long load allocates one segment's worth, not the
	// table's.
	reuse bool
	buf   []byte

	// async builds each full segment on a goroutine of its own while rows go
	// on arriving -- a transaction's load (insert_bulk_direct.go), whose build
	// otherwise stalled the statement loop once per segment. One build at a
	// time is in flight; wait emits it, so segments still arrive in order.
	async   bool
	pending *segBuild

	// own copies each row's values into storage the cutter keeps (flat) rather
	// than holding the caller's slice, and reuses that storage, and the rowid
	// and row lists, segment after segment: a load's rows then cost no
	// allocation of their own, and the caller may reuse its slice once add
	// returns. Only for a caller that hands over no row it reads again.
	own  bool
	flat []Value
	// free is the storage of the last async build, back for the next segment.
	free *segRowsBuf
}

// segRowsBuf is one segment's row storage: what an own cutter recycles.
type segRowsBuf struct {
	rowids []uint64
	rows   [][]Value
	flat   []Value
}

// segBuild is an async segment build in flight.
type segBuild struct {
	done chan struct{}
	raw  []byte
	err  error
	// rows is the build's input, the cutter's again once the build is done
	// (own cutters only).
	rows *segRowsBuf
}

// wait finishes the build in flight, if any, and emits it.
func (c *segCutter) wait() error {
	b := c.pending
	if b == nil {
		return nil
	}
	<-b.done
	c.pending = nil
	if r := b.rows; r != nil {
		clear(r.rows) // the rows' text belongs to the segment now; pin none of it
		clear(r.flat)
		c.free = &segRowsBuf{rowids: r.rowids[:0], rows: r.rows[:0], flat: r.flat[:0]}
	}
	if b.err != nil {
		return b.err
	}
	return c.emit(b.raw)
}

// finish builds and emits every pending row: flush, then wait.
func (c *segCutter) finish() error {
	if err := c.flush(); err != nil {
		return err
	}
	return c.wait()
}

// add appends one row, cutting a segment when it fills one.
func (c *segCutter) add(rowid uint64, vals []Value) error {
	if c.own {
		if len(c.flat)+len(vals) > cap(c.flat) {
			// A new array, twice the last: the rows so far stay in the old one,
			// which they keep alive, so nothing is copied. The last array is
			// the one reused, and soon holds a whole segment.
			c.flat = make([]Value, 0, max(2*cap(c.flat), 1024*len(vals), len(vals)))
		}
		at := len(c.flat)
		c.flat = append(c.flat, vals...)
		vals = c.flat[at:len(c.flat):len(c.flat)]
		if len(c.rows) == cap(c.rows) {
			c.rows = slices.Grow(c.rows, max(len(c.rows), 1024))
			c.rowids = slices.Grow(c.rowids, cap(c.rows)-len(c.rowids))
		}
	}
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
	if c.async {
		if err := c.wait(); err != nil {
			return err
		}
		// The rows go to the build, and the next segment's into new slices.
		b := &segBuild{done: make(chan struct{})}
		cols, what, rowids, rows := c.cols, c.what, c.rowids, c.rows
		if c.own {
			b.rows = &segRowsBuf{rowids: rowids, rows: rows, flat: c.flat}
			if f := c.free; f != nil {
				c.rowids, c.rows, c.flat, c.free = f.rowids, f.rows, f.flat, nil
			} else {
				c.rowids, c.rows, c.flat = make([]uint64, 0, len(rowids)), make([][]Value, 0, len(rows)), make([]Value, 0, cap(c.flat))
			}
			c.bytes = 0
		} else {
			c.rowids, c.rows, c.bytes = make([]uint64, 0, len(rowids)), make([][]Value, 0, len(rows)), 0
		}
		go func() {
			defer close(b.done)
			raw, err := buildSegmentInto(nil, cols, rowids, rows)
			if err != nil {
				err = fmt.Errorf("building a segment of %s: %w", what, err)
			}
			b.raw, b.err = raw, err
		}()
		c.pending = b
		return nil
	}
	var dst []byte
	if c.reuse {
		dst = c.buf
	}
	raw, err := buildSegmentInto(dst, c.cols, c.rowids, c.rows)
	if err != nil {
		return fmt.Errorf("building a segment of %s: %w", c.what, err)
	}
	clear(c.rows) // let the rows go before the next segment's arrive
	c.rowids, c.rows, c.bytes = c.rowids[:0], c.rows[:0], 0
	if c.own {
		clear(c.flat)
		c.flat = c.flat[:0]
	}
	if c.reuse {
		c.buf = raw
	}
	return c.emit(raw)
}

// segmentFull reports whether a segment of n rows and raw bytes is as large
// as segCutter would make one -- so carrying it over leaves the table cut the
// way building it afresh would.
func segmentFull(n int, raw []byte) bool { return n >= segmentRows || len(raw) >= segmentBytes }
