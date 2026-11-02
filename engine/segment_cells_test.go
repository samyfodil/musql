package engine

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"
)

// buildSegmentCells writes the bytes buildSegmentInto writes for the same
// rows: INTEGER and REAL columns, any count of rows, values repeated or not.
func TestSegmentCellsMatchRows(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 9))
	for _, n := range []int{1, 2, 7, 1000, 65536} {
		for _, real := range [][]bool{{false}, {true}, {false, true, false}, {true, true}} {
			cols := make([]columnInfo, len(real))
			cells := make([][]uint64, len(real))
			rows := make([][]Value, n)
			rowids := make([]uint64, n)
			for i := range rows {
				rows[i] = make([]Value, len(real))
				rowids[i] = uint64(i) + 41
			}
			for c, isReal := range real {
				cols[c] = columnInfo{Name: string(rune('a' + c))}
				cells[c] = make([]uint64, n)
				for i := range n {
					x := r.Int64N(int64(n/3+1)) - int64(n/6)
					if r.IntN(10) == 0 {
						x = int64(r.Uint64())
					}
					if isReal {
						f := float64(x) * 0.37
						if r.IntN(20) == 0 {
							f = math.Copysign(0, -1)
						}
						cells[c][i], rows[i][c] = math.Float64bits(f), Value{Typ: Float, F: f}
						continue
					}
					cells[c][i], rows[i][c] = uint64(x), Value{Typ: Int, I: x}
				}
			}
			want, err := buildSegmentInto(nil, cols, rowids, rows)
			if err != nil {
				t.Fatal(err)
			}
			got, err := buildSegmentCells(cols, 41, cells, real)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%d rows, real %v: %d bytes, the row builder %d", n, real, len(got), len(want))
			}
		}
	}
}
