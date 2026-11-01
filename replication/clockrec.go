package replication

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"slices"
)

// A clock record packs one row's clock cells, presence included, into
// _repl_clock's rec: the tag most of them share, then each cell's column, its
// own tag where it differs, and its value.
//
//	rec  = uvarint(hlc) str(site) uvarint(cells) cell...
//	cell = str(col) byte(own) [uvarint(hlc) str(site)] str(val)
//	str  = uvarint(len) bytes
//
// top, beside it, is the record's largest hlc.

var errBadRec = errors.New("replication: malformed clock record")

// encodeRec packs cells, which hold one at least.
func encodeRec(cells map[string]clockCell) (rec []byte, top HLC) {
	var keyBuf [16]string
	keys := keyBuf[:0]
	size := 3 * binary.MaxVarintLen64
	for col, c := range cells {
		keys = append(keys, col)
		top = max(top, c.hlc)
		size += 4*binary.MaxVarintLen64 + 1 + len(col) + len(c.site) + len(c.val)
	}
	slices.Sort(keys)
	shared := sharedTag(cells, keys)
	rec = make([]byte, 0, size)
	rec = binary.AppendUvarint(rec, uint64(shared.hlc))
	rec = appendRecStr(rec, shared.site)
	rec = binary.AppendUvarint(rec, uint64(len(cells)))
	for _, col := range keys {
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

type recTag struct {
	hlc  HLC
	site string
}

// sharedTag is the tag most of cells carry, the greatest of those tied.
func sharedTag(cells map[string]clockCell, keys []string) recTag {
	type count struct {
		t recTag
		n int
	}
	var buf [8]count
	counts := buf[:0]
	var shared recTag
	best := 0
	for _, col := range keys {
		t := recTag{cells[col].hlc, cells[col].site}
		i := 0
		for i < len(counts) && counts[i].t != t {
			i++
		}
		if i == len(counts) {
			counts = append(counts, count{t: t})
		}
		counts[i].n++
		if n := counts[i].n; n > best || n == best && TagLess(shared.hlc, shared.site, t.hlc, t.site) {
			shared, best = t, n
		}
	}
	return shared
}

func appendRecStr(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// decodeRec unpacks a record. Each cell's value is a slice of rec.
func decodeRec(rec []byte) (map[string]clockCell, error) {
	return decodeRecIn(rec, nil)
}

// decodeRecIn is decodeRec taking each column and site name from in.
func decodeRecIn(rec []byte, in recIntern) (map[string]clockCell, error) {
	r := recReader{b: rec}
	shared := clockCell{hlc: HLC(r.uvarint()), site: in.str(r.bytes())}
	n := r.uvarint()
	if n > uint64(len(rec)) {
		return nil, errBadRec
	}
	cells := make(map[string]clockCell, n)
	for range n {
		col := in.str(r.bytes())
		c := shared
		switch r.byte() {
		case 0:
		case 1:
			c.hlc, c.site = HLC(r.uvarint()), in.str(r.bytes())
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

// recIntern hands out one string per column or site name: they repeat on
// every record. A nil one copies each name.
type recIntern map[string]string

func (in recIntern) str(b []byte) string {
	if in == nil {
		return string(b)
	}
	if s, ok := in[string(b)]; ok {
		return s
	}
	s := string(b)
	in[s] = s
	return s
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
