package engine

// recAlloc's arena (vdbe.go) now GROWS its chunk from the first record's own
// width instead of starting at recChunkValues, which is what stopped a
// single-row INSERT allocating 12,288 B to hand back 288. The two properties
// that sizing must not break are pinned here, because the way it breaks is
// silent: every consumer of an OpMakeRecord result RETAINS it, so two records
// that overlap by even one Value make one stored row alias another.

import "testing"

// TestRecAllocRecordsAreDisjoint hands out records in the widths the real
// workloads use and demands that no two ever share storage, and that an append
// to one cannot reach into the next (the three-index slice cap).
func TestRecAllocRecordsAreDisjoint(t *testing.T) {
	for _, width := range []int{1, 3, 6, 255, 256, 257, 1000} {
		m := &vdbe{}
		const records = 40
		out := make([][]Value, records)
		for i := range out {
			out[i] = m.recAlloc(width)
			if len(out[i]) != width || cap(out[i]) != width {
				t.Fatalf("width %d: recAlloc returned len=%d cap=%d, want both %d", width, len(out[i]), cap(out[i]), width)
			}
			// A stamp no other record can produce, written through this record
			// and through an append that must reallocate rather than spill.
			for j := range out[i] {
				out[i][j] = Value{Typ: Int, I: int64(i*1_000_000 + j)}
			}
			_ = append(out[i], Value{Typ: Int, I: -1})
		}
		for i := range out {
			for j := range out[i] {
				if want := int64(i*1_000_000 + j); out[i][j].I != want {
					t.Fatalf("width %d: record %d value %d = %d, want %d -- records overlap", width, i, j, out[i][j].I, want)
				}
			}
		}
	}
}

// TestRecAllocChunkRampsToTheCap is the anti-regression half: shrinking the
// first chunk to one record is only safe because the chunk then DOUBLES, so a
// scan-shaped program still reaches recChunkValues and keeps paying one
// allocation per ~256 Values rather than one per row. Counting allocations
// directly is what a wall clock cannot do on this box.
func TestRecAllocChunkRampsToTheCap(t *testing.T) {
	const (
		width   = 3       // workload 6's record width
		records = 100_000 // and its row count
	)
	m := &vdbe{}
	chunks, values := 0, 0
	for i := 0; i < records; i++ {
		if len(m.recChunk) < width {
			chunks++
		}
		r := m.recAlloc(width)
		values += len(r)
	}
	// One allocation per 256 Values is the fixed-chunk baseline; the doubling
	// ramp adds the seven small chunks it climbs through (3+6+...+192 Values).
	if maxChunks := records*width/256 + 12; chunks > maxChunks {
		t.Errorf("%d records of %d Values took %d chunk allocations, want <= %d -- the arena is not reaching recChunkValues", records, width, chunks, maxChunks)
	}
	if values != records*width {
		t.Fatalf("handed out %d Values, want %d", values, records*width)
	}

	// ...while a program that makes ONE record must not allocate a chunk for
	// 256 of them: that waste was 36.1% of workload 7a's allocated bytes.
	one := &vdbe{}
	one.recAlloc(6)
	if got := cap(one.recChunk) + 6; got != 6 {
		t.Errorf("a single 6-Value record cut a %d-Value chunk, want exactly 6", got)
	}
}
