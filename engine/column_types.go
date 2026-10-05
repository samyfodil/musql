package engine

import "iter"

// Column typing census for the columnar format's physical-type decision.
// A column block holds one physical type, but SQLite columns are dynamically
// typed. The answer is a declared physical type plus exceptions for rows that
// don't fit. This census guides that choice.

// ColumnTypeCensus is one column's storage-class histogram over stored rows.
// Classes are SQLite's five storage classes. Columns added by ALTER TABLE do
// not contribute to earlier rows.
type ColumnTypeCensus struct {
	Table  string
	Column int
	Null   int64
	Int    int64
	Real   int64
	Text   int64
	Blob   int64
}

// Rows is the count of rows with stored values, including NULLs.
func (c ColumnTypeCensus) Rows() int64 { return c.Null + c.Int + c.Real + c.Text + c.Blob }

// Typed is the count of non-NULL rows.
func (c ColumnTypeCensus) Typed() int64 { return c.Int + c.Real + c.Text + c.Blob }

// Majority returns the most common non-NULL class and its row count.
// This is the physical type a segment would declare.
func (c ColumnTypeCensus) Majority() (class string, n int64) {
	for _, cand := range []struct {
		name string
		n    int64
	}{{"int", c.Int}, {"real", c.Real}, {"text", c.Text}, {"blob", c.Blob}} {
		if cand.n > n {
			class, n = cand.name, cand.n
		}
	}
	return class, n
}

// Exceptions is the rows that would NOT fit the declared physical type, i.e.
// the size of the side list. Zero means the column is cleanly typed and its
// block can be read by generated code with no per-row branch at all.
func (c ColumnTypeCensus) Exceptions() int64 {
	_, n := c.Majority()
	return c.Typed() - n
}

// CensusColumnTypes walks every ordinary table and counts each column's
// storage classes. Virtual tables and indexes are skipped.
func (p *ReadOnlyPager) CensusColumnTypes() ([]ColumnTypeCensus, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	var out []ColumnTypeCensus
	for _, r := range rows {
		if r.Type != "table" || r.RootPage == 0 {
			continue
		}
		byCol := map[int]*ColumnTypeCensus{}
		seq, errFn := p.ScanTable(r.RootPage)
		censusRows(seq, r.Name, byCol)
		if serr := errFn(); serr != nil {
			return nil, serr
		}
		for i := 0; i < len(byCol); i++ {
			if c := byCol[i]; c != nil {
				out = append(out, *c)
			}
		}
	}
	return out, nil
}

func censusRows(seq iter.Seq2[uint64, []Value], table string, byCol map[int]*ColumnTypeCensus) {
	for _, vals := range seq {
		for i, v := range vals {
			c := byCol[i]
			if c == nil {
				c = &ColumnTypeCensus{Table: table, Column: i}
				byCol[i] = c
			}
			switch v.Typ {
			case Null:
				c.Null++
			case Int:
				c.Int++
			case Float:
				c.Real++
			case Text:
				c.Text++
			case Blob:
				c.Blob++
			}
		}
	}
}

// PhysicalType is the encoding one column block is stored in. A fixed-width
// type is what generated code can address as base + i*width with no per-row
// branch; PhysTagged is the correct-but-slow fallback for a column whose values
// genuinely do not agree on a type.
type PhysicalType uint8

const (
	PhysNull    PhysicalType = iota // no non-NULL value; the block is the NULL bitmap alone
	PhysInt64                       // 8 bytes, little-endian two's complement
	PhysFloat64                     // 8 bytes, little-endian IEEE754
	PhysText                        // 8 bytes: (uint32 heap offset, uint32 length)
	PhysBlob                        // 8 bytes, same shape as PhysText
	PhysTagged                      // per-value tag; the escape hatch, priced at ~114ns/row
)

func (t PhysicalType) String() string {
	switch t {
	case PhysNull:
		return "null"
	case PhysInt64:
		return "int64"
	case PhysFloat64:
		return "float64"
	case PhysText:
		return "text"
	case PhysBlob:
		return "blob"
	default:
		return "tagged"
	}
}

// Width is the bytes one row occupies in this column's block. Every fixed-width
// type is 8 bytes, deliberately: a uniform stride means a segment's column
// blocks are all page-alignable at the same granularity and generated code
// shifts by a constant rather than multiplying by a loaded width.
func (t PhysicalType) Width() int {
	if t == PhysNull || t == PhysTagged {
		return 0
	}
	return 8
}

// maxExceptionRate is the share of a column's non-NULL values that may miss the
// declared physical type before the column is stored tagged instead.
//
// The trade it encodes: an exception costs a side-list entry and a patch after
// the block scan, so a few are far cheaper than tagging every row, and many are
// not. The number itself is a guess until the census says what real columns do
// -- see compat-harness's TestColumnTypeCensus, which exists to replace it.
const maxExceptionRate = 0.05

// PlanColumn chooses the physical type for a column and reports how many of its
// rows would go to the exception list. A column with no non-NULL value has no
// type to choose; one whose majority class does not cover enough of its rows is
// stored tagged, and its exception count is meaningless (reported as 0).
func PlanColumn(c ColumnTypeCensus) (PhysicalType, int64) {
	typed := c.Typed()
	if typed == 0 {
		return PhysNull, 0
	}
	class, n := c.Majority()
	exceptions := typed - n
	if float64(exceptions) > maxExceptionRate*float64(typed) {
		return PhysTagged, 0
	}
	switch class {
	case "int":
		return PhysInt64, exceptions
	case "real":
		return PhysFloat64, exceptions
	case "text":
		return PhysText, exceptions
	default:
		return PhysBlob, exceptions
	}
}
