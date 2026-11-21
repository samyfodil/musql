package engine

import (
	"encoding/binary"
	"testing"
)

// TestTextBoundsValid: a TEXT column reads natively only when its bounds
// never step back and none passes the heap's end.
func TestTextBoundsValid(t *testing.T) {
	for _, c := range []struct {
		bounds []uint32
		want   bool
	}{
		{[]uint32{0, 10, 10, 50}, true},
		{[]uint32{0, 0}, true},
		{[]uint32{0, 10, 51}, false},
		{[]uint32{0, 60, 40}, false},
		{[]uint32{5, 4, 50}, false},
		{[]uint32{0, 20, 10, 50}, false},
		{[]uint32{51}, false},
	} {
		s := &segment{nRows: len(c.bounds) - 1, heap: make([]byte, 50), cols: []segColumn{{blockOff: 8}}}
		s.buf = make([]byte, 8+4*len(c.bounds))
		for i, x := range c.bounds {
			binary.LittleEndian.PutUint32(s.buf[8+4*i:], x)
		}
		if got := s.textBoundsValid(0); got != c.want {
			t.Errorf("bounds %v: valid %v, want %v", c.bounds, got, c.want)
		}
	}
}
