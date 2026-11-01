package engine

import (
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func deltaRec(kind byte, table uint32, rowid int64, payload []byte) []byte {
	b := []byte{kind}
	b = binary.LittleEndian.AppendUint32(b, table)
	b = binary.LittleEndian.AppendUint64(b, uint64(rowid))
	switch kind {
	case segDeltaSeg:
		b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
		b = append(b, 0)
		b = append(b, payload...)
	case segDeltaPut, segDeltaPatch:
		b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
		b = append(b, payload...)
	}
	return b
}

// craftDelta builds a delta of the given version whose header ends in seed,
// one batch per body; ends and sums are each batch's end and checksum.
func craftDelta(version uint32, seed uint64, bodies ...[]byte) (data []byte, ends []int64, sums []uint64) {
	data = make([]byte, segDeltaHdrSize)
	copy(data, segDeltaMagic)
	binary.LittleEndian.PutUint64(data[segDeltaHdrSize-8:], seed)
	cksum, w := seed, segDeltaLenWidth(version)
	for i, body := range bodies {
		start := len(data)
		if w == 4 {
			data = binary.LittleEndian.AppendUint32(data, uint32(len(body)))
		} else {
			data = binary.LittleEndian.AppendUint64(data, uint64(len(body)))
		}
		data = append(data, body...)
		data = binary.LittleEndian.AppendUint32(data, uint32(i+1))
		data = binary.LittleEndian.AppendUint32(data, 7)
		cksum = segDeltaChecksum(version, data[start:], cksum)
		data = binary.LittleEndian.AppendUint64(data, cksum)
		ends, sums = append(ends, int64(len(data))), append(sums, cksum)
	}
	return data, ends, sums
}

func craftedBodies() [][]byte {
	return [][]byte{
		slices.Concat(deltaRec(segDeltaPut, 1, 10, []byte("row")), deltaRec(segDeltaKill, 1, 11, nil)),
		slices.Concat(deltaRec(segDeltaPatch, 2, 5, []byte("p")), deltaRec(segDeltaSeg, 3, 0, []byte("segment"))),
	}
}

func TestDeltaTouchedReadsEveryRecord(t *testing.T) {
	for _, v := range []uint32{1, segDeltaVersion} {
		data, ends, sums := craftDelta(v, 99, craftedBodies()...)
		rows, segs, ok := deltaTouched(data, v, segDeltaHdrSize, ends[1], 99, sums[1])
		if !ok {
			t.Fatalf("v%d: not ok", v)
		}
		if want := map[int][]int64{1: {10, 11}, 2: {5}}; !maps.EqualFunc(rows, want, slices.Equal) {
			t.Errorf("v%d: rows %v, want %v", v, rows, want)
		}
		if !segs[3] || len(segs) != 1 {
			t.Errorf("v%d: segs %v", v, segs)
		}
		if _, _, ok := deltaTouched(data, v, ends[0], ends[1], sums[0], sums[1]); !ok {
			t.Errorf("v%d: from the second batch: not ok", v)
		}
	}
}

func TestDeltaTouchedRefusesWhatReplayWouldNot(t *testing.T) {
	v := uint32(segDeltaVersion)
	data, ends, sums := craftDelta(v, 99, craftedBodies()...)
	corrupt := slices.Clone(data)
	corrupt[segDeltaHdrSize+10] ^= 0xff
	unknown, uends, usums := craftDelta(v, 99, []byte{9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	for name, tc := range map[string]struct {
		data                 []byte
		version              uint32
		from, to             int64
		cksum, end           uint64
		wantRows, wantResult bool
	}{
		"version 0":          {data, 0, segDeltaHdrSize, ends[1], 99, sums[1], false, false},
		"from in the header": {data, v, 8, ends[1], 99, sums[1], false, false},
		"from past to":       {data, v, ends[1], ends[0], sums[1], sums[0], false, false},
		"to past the data":   {data, v, segDeltaHdrSize, int64(len(data)) + 1, 99, sums[1], false, false},
		"wrong checksum":     {data, v, segDeltaHdrSize, ends[1], 98, sums[1], false, false},
		"to inside a word":   {data, v, ends[0], ends[0] + 3, sums[0], sums[1], false, false},
		"to inside a batch":  {data, v, segDeltaHdrSize, ends[0] - 1, 99, sums[0], false, false},
		"corrupt batch":      {corrupt, v, segDeltaHdrSize, ends[1], 99, sums[1], false, false},
		"unknown record":     {unknown, v, segDeltaHdrSize, uends[0], 99, usums[0], false, false},
		"another end":        {data, v, segDeltaHdrSize, ends[1], 99, sums[0], true, false},
	} {
		rows, _, ok := deltaTouched(tc.data, tc.version, tc.from, tc.to, tc.cksum, tc.end)
		if ok != tc.wantResult || (rows != nil) != tc.wantRows {
			t.Errorf("%s: rows %v ok %v", name, rows, ok)
		}
	}
}

func TestBatchTouchedRefusesMalformedRecords(t *testing.T) {
	seg := deltaRec(segDeltaSeg, 3, 0, []byte("segment"))
	badPad := slices.Clone(seg)
	badPad[17] = 8
	put := deltaRec(segDeltaPut, 1, 1, []byte("row"))
	for name, body := range map[string][]byte{
		"short header":    seg[:12],
		"short seg":       seg[:15],
		"seg padding":     badPad,
		"seg past body":   seg[:len(seg)-1],
		"short put":       put[:15],
		"put past body":   put[:len(put)-1],
		"unknown kind":    deltaRec(7, 1, 1, nil),
		"empty seg bytes": deltaRec(segDeltaSeg, 3, 0, nil),
	} {
		if batchTouched(body, map[int][]int64{}, map[int]bool{}) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDeltaBatchesFrom(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) *os.File {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	for _, v := range []uint32{1, segDeltaVersion} {
		data, ends, sums := craftDelta(v, 99, craftedBodies()...)
		whole := write("whole", data)
		got, ok := deltaBatchesFrom(whole, v, segDeltaHdrSize, 99, 1<<20)
		if !ok || len(got) != 2 || got[1].end != ends[1] || got[1].cksum != sums[1] || got[0].endCtr != 1 || got[1].endPages != 7 {
			t.Fatalf("v%d: %v %v", v, got, ok)
		}
		if got, ok := deltaBatchesFrom(whole, v, ends[0], sums[0], 1<<20); !ok || len(got) != 1 {
			t.Fatalf("v%d: from the second batch: %d batches, %v", v, len(got), ok)
		}
	}
	v := uint32(segDeltaVersion)
	data, ends, sums := craftDelta(v, 99, craftedBodies()...)
	whole := write("v", data)
	for name, tc := range map[string]struct {
		version uint32
		from    int64
		cksum   uint64
		limit   int64
	}{
		"version 0":          {0, segDeltaHdrSize, 99, 1 << 20},
		"a newer version":    {segDeltaVersion + 1, segDeltaHdrSize, 99, 1 << 20},
		"from in the header": {v, 8, 99, 1 << 20},
		"wrong checksum":     {v, segDeltaHdrSize, 98, 1 << 20},
		"past the limit":     {v, segDeltaHdrSize, 99, 10},
	} {
		if got, ok := deltaBatchesFrom(whole, tc.version, tc.from, tc.cksum, tc.limit); ok {
			t.Errorf("%s: %d batches, ok", name, len(got))
		}
	}
	torn := write("torn", data[:ends[1]-3])
	if got, ok := deltaBatchesFrom(torn, v, segDeltaHdrSize, 99, 1<<20); !ok || len(got) != 1 {
		t.Errorf("torn: %d batches, %v; want the first", len(got), ok)
	}
	corrupt := slices.Clone(data)
	corrupt[ends[0]+10] ^= 0xff
	if got, ok := deltaBatchesFrom(write("corrupt", corrupt), v, segDeltaHdrSize, 99, 1<<20); !ok || len(got) != 1 {
		t.Errorf("corrupt: %d batches, %v; want the first", len(got), ok)
	}
	zero := slices.Concat(data[:ends[0]], make([]byte, 8))
	if got, ok := deltaBatchesFrom(write("zero", zero), v, segDeltaHdrSize, 99, 1<<20); !ok || len(got) != 1 || got[0].cksum != sums[0] {
		t.Errorf("zero length: %d batches, %v", len(got), ok)
	}
}
