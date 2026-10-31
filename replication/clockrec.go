package replication

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
)

// A clock record is one row's clock cells, its presence among them, packed
// into _repl_clock's rec: the tag most of them share, then each cell's column,
// its own tag where it differs, and its value. The cells still merge one at a
// time (mergeInto); a record only stores them, one row per key instead of one
// per cell.
//
//	rec  = uvarint(hlc) str(site) uvarint(cells) cell...
//	cell = str(col) byte(own) [uvarint(hlc) str(site)] str(val)
//	str  = uvarint(len) bytes
//
// top, beside it, is the record's largest hlc, which OpenStore starts the
// clock past.

var errBadRec = errors.New("replication: malformed clock record")

// encodeRec packs cells, which hold one at least.
func encodeRec(cells map[string]clockCell) (rec []byte, top HLC) {
	type tag struct {
		hlc  HLC
		site string
	}
	counts := make(map[tag]int, 2)
	var shared tag
	best := 0
	for _, c := range cells {
		t := tag{c.hlc, c.site}
		counts[t]++
		if n := counts[t]; n > best || n == best && TagLess(shared.hlc, shared.site, t.hlc, t.site) {
			shared, best = t, n
		}
		top = max(top, c.hlc)
	}
	rec = binary.AppendUvarint(rec, uint64(shared.hlc))
	rec = appendRecStr(rec, shared.site)
	rec = binary.AppendUvarint(rec, uint64(len(cells)))
	for _, col := range slices.Sorted(maps.Keys(cells)) {
		c := cells[col]
		rec = appendRecStr(rec, col)
		if c.hlc == shared.hlc && c.site == shared.site {
			rec = append(rec, 0)
		} else {
			rec = append(rec, 1)
			rec = binary.AppendUvarint(rec, uint64(c.hlc))
			rec = appendRecStr(rec, c.site)
		}
		rec = binary.AppendUvarint(rec, uint64(len(c.val)))
		rec = append(rec, c.val...)
	}
	return rec, top
}

func appendRecStr(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// decodeRec unpacks a record. Each cell's value is a slice of rec.
func decodeRec(rec []byte) (map[string]clockCell, error) {
	r := recReader{b: rec}
	shared := clockCell{hlc: HLC(r.uvarint()), site: string(r.bytes())}
	n := r.uvarint()
	if n > uint64(len(rec)) {
		return nil, errBadRec
	}
	cells := make(map[string]clockCell, n)
	for range n {
		col := string(r.bytes())
		c := shared
		switch r.byte() {
		case 0:
		case 1:
			c.hlc, c.site = HLC(r.uvarint()), string(r.bytes())
		default:
			r.bad = true
		}
		c.val = r.bytes()
		if r.bad {
			return nil, errBadRec
		}
		cells[col] = c
	}
	if r.bad || len(r.b) > 0 {
		return nil, errBadRec
	}
	return cells, nil
}

type recReader struct {
	b   []byte
	bad bool
}

func (r *recReader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.bad, r.b = true, nil
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *recReader) byte() byte {
	if len(r.b) == 0 {
		r.bad = true
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *recReader) bytes() []byte {
	n := r.uvarint()
	if n > uint64(len(r.b)) {
		r.bad, r.b = true, nil
		return nil
	}
	v := r.b[:n:n]
	r.b = r.b[n:]
	return v
}

// readRec is the clock's cells of tbl's row pk, by column: none when the clock
// holds no record of it.
func readRec(ctx context.Context, q dbtx, tbl string, pk []byte) (map[string]clockCell, error) {
	var rec []byte
	err := q.QueryRowContext(ctx, `SELECT rec FROM main._repl_clock WHERE tbl=? AND pk=?`, tbl, pk).Scan(&rec)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]clockCell{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeRec(rec)
}

// recArgs appends one record's insert arguments, four: to won when the clock
// held a record of the row already (had), else to fresh.
func recArgs(fresh, won []any, tbl string, pk []byte, cells map[string]clockCell, had bool) ([]any, []any) {
	rec, top := encodeRec(cells)
	if had {
		return fresh, append(won, tbl, pk, int64(top), rec)
	}
	return append(fresh, tbl, pk, int64(top), rec), won
}
