// FTS3 segment promotion: after a segment is flushed to a level,
// relabel all rows in that level-block (and above, up to the limit) if all
// candidate segments are within 1.5x the new segment's size. This is a pure
// level/idx rename; nothing is merged or re-encoded. Declines gracefully on
// unparsable or missing size fields rather than erroring.
package engine

import (
	"sort"
	"strconv"
	"strings"
)

// fts3PromoteSegments promotes segments at the given level. iAbsLevel is
// the level a segment holding nByte leaf-data bytes was just written to.
func fts3PromoteSegments(segdir *tableMeta, iAbsLevel, nByte int64) {
	iLast := (iAbsLevel/fts3SegdirMaxLevel+1)*fts3SegdirMaxLevel - 1
	nLimit := (nByte * 3) / 2

	type candidate struct {
		rowid      uint64
		level, idx int64
	}
	var inRange, above []candidate
	for rowid, v := range segdir.rows.all() {
		if len(v) < 6 || v[0].Typ != Int || v[1].Typ != Int {
			return // see doc comment: a shape this engine never wrote anywhere
			// in the table means "cannot safely reason about this table" here.
		}
		level := v[0].I
		if level < iAbsLevel || level > iLast {
			continue // outside the 1024-level block iAbsLevel belongs to.
		}
		c := candidate{rowid: rowid, level: level, idx: v[1].I}
		inRange = append(inRange, c)
		if level > iAbsLevel {
			above = append(above, c)
		}
	}
	if len(above) == 0 {
		// fts3_write.c:3156 "int bOk = 0" with an empty SQL_SELECT_LEVEL_RANGE2
		// result: the while loop never runs, bOk stays 0 -- nothing above to fold.
		return
	}
	for _, c := range above {
		nSize := fts3EndBlockLeafBytes(segdir.rows.row(c.rowid)[4])
		if nSize <= 0 || nSize > nLimit {
			// fts3_write.c:3170-3178: one candidate that is too big, or whose
			// size cannot be determined at all, cancels the WHOLE promotion --
			// not just that one row.
			return
		}
	}

	// Every candidate above iAbsLevel qualified: relabel every row in
	// [iAbsLevel, iLast] -- the just-written one included -- onto iAbsLevel,
	// visited highest level first (oldest data, since a higher level has been
	// through more merges) then lowest idx first, giving idx 0 to the oldest.
	// Matches fts3_write.c's SQL_SELECT_LEVEL_RANGE2 "ORDER BY level DESC, idx
	// ASC" driving pUpdate1's iIdx++ counter (fts3_write.c:3196-3217).
	sort.Slice(inRange, func(i, j int) bool {
		if inRange[i].level != inRange[j].level {
			return inRange[i].level > inRange[j].level
		}
		return inRange[i].idx < inRange[j].idx
	})
	for newIdx, c := range inRange {
		old := segdir.rows.row(c.rowid)
		row := make([]Value, len(old))
		copy(row, old)
		row[0] = Value{Typ: Int, I: iAbsLevel}
		row[1] = Value{Typ: Int, I: int64(newIdx)}
		segdir.putRow(c.rowid, row)
	}
}

// fts3EndBlockLeafBytes is fts3ReadEndBlockField's SECOND half (*pnByte): the
// segment's own recorded leaf-byte size, read out of the two-integer TEXT
// form "<last block id> <leaf bytes>" fts3StoreSegment always writes. A bare
// INTEGER end_block (C fts3's "old version of FTS" case, which never
// recorded one -- sqlite3_column_text on an integer cell still yields its
// digits with no following space, so fts3ReadEndBlockField's own second scan
// loop finds nothing and leaves *pnByte at its initialized 0) or malformed
// text reads as 0, which the caller already treats as "cannot determine, do
// not promote" -- matching C fts3 exactly (fts3_write.c:3170-3178).
func fts3EndBlockLeafBytes(v Value) int64 {
	if v.Typ != Text && v.Typ != Blob {
		return 0
	}
	s := string(v.S)
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s[i+1:]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
