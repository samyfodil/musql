package driver

import (
	"database/sql/driver"
	"math"
	"unsafe"

	"github.com/samyfodil/musql/engine"
)

// valueSlab hands out driver.Values whose payloads live in chunks shared by
// the values of a few rows. Boxing an int64, float64 or string into an
// interface otherwise allocates once per value (twice for TEXT, whose bytes
// are copied first), which was most of the driver's allocations on a point
// lookup. A value is written once and never again, and a chunk is never
// reused, so every Value handed out stays valid for as long as its holder
// keeps it. A chunk covers at most valueSlabRows rows and is sized exactly to
// them, so a value kept long pins little.
type valueSlab struct {
	words []uint64 // an INTEGER's or a REAL's eight bytes
	strs  []string
	text  []byte
	left  int // rows the current chunks were sized for and have not served
}

const valueSlabRows = 16

// fill sizes fresh chunks for the rows from rows[0] on.
func (s *valueSlab) fill(rows [][]engine.Value) {
	rows = rows[:min(len(rows), valueSlabRows)]
	var nw, ns, nb int
	for _, row := range rows {
		for _, v := range row {
			switch v.Typ {
			case engine.Int, engine.Float:
				nw++
			case engine.Text:
				ns++
				nb += len(v.S)
			}
		}
	}
	*s = valueSlab{left: len(rows)}
	if nw > 0 {
		s.words = make([]uint64, 0, nw)
	}
	if ns > 0 {
		s.strs = make([]string, 0, ns)
		s.text = make([]byte, 0, nb)
	}
}

// eface is the runtime's layout of an empty interface: its dynamic type, and a
// pointer to the value for a type that is not itself a pointer.
type eface struct {
	typ, data unsafe.Pointer
}

func typeOf(v any) unsafe.Pointer { return (*eface)(unsafe.Pointer(&v)).typ }

var (
	int64Type   = typeOf(int64(0))
	float64Type = typeOf(float64(0))
	stringType  = typeOf("")
)

func boxAt(typ, data unsafe.Pointer) (v driver.Value) {
	e := (*eface)(unsafe.Pointer(&v))
	e.typ, e.data = typ, data
	return v
}

// value boxes v into the current chunks, which fill sized for its row.
func (s *valueSlab) value(v engine.Value) driver.Value {
	switch v.Typ {
	case engine.Int:
		s.words = append(s.words, uint64(v.I))
		return boxAt(int64Type, unsafe.Pointer(&s.words[len(s.words)-1]))
	case engine.Float:
		s.words = append(s.words, math.Float64bits(v.F))
		return boxAt(float64Type, unsafe.Pointer(&s.words[len(s.words)-1]))
	case engine.Text:
		at := len(s.text)
		s.text = append(s.text, v.S...)
		s.strs = append(s.strs, unsafe.String(unsafe.SliceData(s.text[at:]), len(v.S)))
		return boxAt(stringType, unsafe.Pointer(&s.strs[len(s.strs)-1]))
	}
	return engineValueToDriver(v)
}
