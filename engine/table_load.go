// This file implements ensureTableLoaded: giving a table its row store on first
// write-path use. The store reads committed rows from the segment file on demand,
// holding only what the session writes. Every write-path site that reads tbl.rows
// calls this first; forgetting it reads a nil store.
package engine

import "fmt"

// ensureTableLoaded gives tbl its row store on first write-path use,
// memoized via tbl.loaded. A no-op for tables created this session.
func (db *DB) ensureTableLoaded(tbl *tableMeta) error {
	if tbl.loadErr != nil {
		return tbl.loadErr // see tableMeta.loadErr
	}
	if tbl.loaded {
		return nil
	}
	if tbl.isTemp {
		// TEMP tables are in-memory only: no file to read from.
		return fmt.Errorf("engine: loading table %s: %w", tbl.name, errTempNotLoaded)
	}
	// Create a store that reads committed rows on demand from the segment file.
	tbl.rows = newSegRowStore(db.segments, tbl)
	tbl.loaded = true
	return nil
}

// readTableRowsFromPager reads tbl's rows from rp, applying generated-column
// widening, IPK-NULL substitution, and pre-ALTER-ADD-COLUMN padding. Returns
// rowids in sorted order when available; nil if the scan order is not ascending.
func readTableRowsFromPager(rp *ReadOnlyPager, tbl *tableMeta) (rows map[uint64][]Value, rowidsSorted []uint64, err error) {
	// Read by root page, not by name, since temp and main tables may share names.
	rowids, vals, rerr := rp.RowsOfRoot(tbl.rootPage)
	if rerr != nil {
		return nil, nil, rerr
	}
	// Check if rowids are strictly ascending (well-formed b-tree order).
	rowidsSorted = rowids
	for i := 1; i < len(rowids); i++ {
		if !rowidLess(rowids[i-1], rowids[i]) {
			rowidsSorted = nil
			break
		}
	}
	rows = make(map[uint64][]Value, len(rowids))
	for i, rowid := range rowids {
		// Expand to full width: one Value per declared column.
		row, err := fullStoredRow(tbl, rowid, vals[i])
		if err != nil {
			return nil, nil, err
		}
		rows[rowid] = row
	}

	return rows, rowidsSorted, nil
}
