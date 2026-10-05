package engine

// Reading one column's cells directly, whatever its physical type.
//
// The columnar aggregate and order paths work on []int64 blocks.
// GROUP BY over TEXT, REAL or BLOB keys falls back to the VDBE row loop.
//
// WHY THIS IS SAFE, and it is the whole argument: a reader produces exactly the
// Value that segment.Value would produce for the same cell -- same type, same
// bytes, same float bits. Everything downstream is then the SHARED code the
// interpreter loop uses (hashAggStepRow, the aggregate steps, the key encoder
// that already canonicalises a float equal to an integer). So the fast path and
// the loop cannot disagree about a value, which is the only thing they could
// disagree about.
//
// The NULL and exception gates live in the accessors this uses. A column with
// either is refused, because then a cell no longer IS the value.

// segColKind says how a column's cells are read.
type segColKind uint8

const (
	segColRowid segColKind = iota // the row's rowid, for an INTEGER PRIMARY KEY
	segColInt
	segColFloat
	segColBytes
	// segColGeneric goes through segment.Value. It is what a column carrying
	// NULLs or exceptions needs, and it is chosen ONCE PER COLUMN rather than
	// per row -- which is the whole saving even when it is what gets chosen.
	segColGeneric
)

// segColReader reads one column's cells without going through segment.Value,
// which builds a 48-byte Value and re-checks the NULL bitmap and exception map
// per cell -- both already known empty for a column that got this far.
type segColReader struct {
	kind  segColKind
	ints  []int64
	flts  []float64
	cells []uint64 // packed heap offset (low 32) | length (high 32)
	heap  []byte
	text  bool // a bytes column is TEXT rather than BLOB
	col   int  // for segColGeneric
}

// segColReaders builds one reader per wanted column, reporting false when any
// column cannot be read directly -- a NULL-bearing one, one carrying an
// exception, an unaligned block. The caller declines the whole statement then,
// exactly as it did when the only readable shape was int64.
func segColReaders(s *segment, want []int, ipkCol int) ([]segColReader, bool) {
	out := make([]segColReader, len(want))
	for i, c := range want {
		if c == ipkCol && ipkCol >= 0 {
			out[i] = segColReader{kind: segColRowid}
			continue
		}
		if col, ok := segCleanInt64Column(s, c); ok {
			out[i] = segColReader{kind: segColInt, ints: col}
			continue
		}
		if col, ok := s.Float64Column(c); ok {
			out[i] = segColReader{kind: segColFloat, flts: col}
			continue
		}
		if cells, heap, ok := s.BytesColumn(c); ok {
			out[i] = segColReader{
				kind: segColBytes, cells: cells, heap: heap,
				text: s.cols[c].phys == PhysText,
			}
			continue
		}
		return nil, false
	}
	return out, true
}

// segColReadersLenient is segColReaders for a caller that can HANDLE any column
// rather than needing to decline one -- the ORDER BY path, whose comparator
// already places NULLs and applies collation. A column the direct accessors
// refuse gets segColGeneric, so the decision is still made once per column
// instead of once per cell.
func segColReadersLenient(s *segment, want []int, ipkCol int) []segColReader {
	out := make([]segColReader, len(want))
	for i, c := range want {
		if one, ok := segColReaders(s, want[i:i+1], ipkCol); ok {
			out[i] = one[0]
			continue
		}
		out[i] = segColReader{kind: segColGeneric, col: c}
	}
	return out
}

// value is the cell at row, as the Value segment.Value would have produced.
// Reports false for a row the column does not reach or a cell that addresses
// outside the heap -- the caller declines rather than substituting anything,
// because a substituted value is a wrong answer and this path exists to be fast,
// not to guess.
func (r *segColReader) value(s *segment, row int) (Value, bool) {
	switch r.kind {
	case segColRowid:
		return Value{Typ: Int, I: int64(s.Rowid(row))}, true
	case segColGeneric:
		return s.Value(r.col, row), true
	case segColInt:
		if row < 0 || row >= len(r.ints) {
			return Value{}, false
		}
		return Value{Typ: Int, I: r.ints[row]}, true
	case segColFloat:
		if row < 0 || row >= len(r.flts) {
			return Value{}, false
		}
		return Value{Typ: Float, F: r.flts[row]}, true
	default:
		if row < 0 || row >= len(r.cells) {
			return Value{}, false
		}
		raw := r.cells[row]
		o, n := uint32(raw), uint32(raw>>32)
		if int(o)+int(n) > len(r.heap) {
			return Value{}, false
		}
		b := r.heap[o : o+n : o+n]
		if r.text {
			return Value{Typ: Text, S: b}, true
		}
		return Value{Typ: Blob, S: b}, true
	}
}
