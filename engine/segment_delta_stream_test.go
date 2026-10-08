package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestSegDeltaBatchStreamsAndReplays writes one batch far larger than a chunk
// (writeSegDeltaBatch) and replays it, in both length-prefix widths: version 2,
// which every new delta is, and version 1, which an existing delta still is and
// must keep being appended to with its 4-byte prefix.
func TestSegDeltaBatchStreamsAndReplays(t *testing.T) {
	blob := bytes.Repeat([]byte{0xAB}, 3000)
	var recs []SegDeltaRecord
	for i := int64(1); i <= 2000; i++ { // ~6 MB: six chunks
		recs = append(recs, SegDeltaRecord{Table: 0, Rowid: i, Vals: []Value{{Typ: Int, I: i}, {Typ: Blob, S: blob}}})
	}
	recs = append(recs, SegDeltaRecord{Table: 0, Rowid: 7, Kill: true})
	for _, version := range []uint32{1, 2} {
		path := filepath.Join(t.TempDir(), "d.delta")
		hdr := make([]byte, segDeltaHdrSize)
		copy(hdr[0:4], segDeltaMagic)
		binary.LittleEndian.PutUint32(hdr[4:], version)
		binary.LittleEndian.PutUint32(hdr[8:], 5)
		binary.LittleEndian.PutUint32(hdr[12:], 6)
		binary.LittleEndian.PutUint64(hdr[24:], segDeltaChecksum(hdr[:24], 0))
		if err := os.WriteFile(path, hdr, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		end, _, err := writeSegDeltaBatch(f, segDeltaHdrSize, segDeltaChecksum(hdr[:24], 0), segDeltaLenWidth(version), recs, 9, 10, nil)
		f.Close()
		if err != nil {
			t.Fatalf("v%d: %v", version, err)
		}
		data, _ := os.ReadFile(path)
		if int64(len(data)) != end {
			t.Fatalf("v%d: file is %d bytes, batch claims to end at %d", version, len(data), end)
		}
		st, err := replaySegDelta(data, 5, 6)
		if err != nil {
			t.Fatalf("v%d replay: %v", version, err)
		}
		if st.batches != 1 || st.bytes != end || st.endCtr != 9 || st.version != version {
			t.Fatalf("v%d: replay saw batches=%d bytes=%d endCtr=%d version=%d", version, st.batches, st.bytes, st.endCtr, st.version)
		}
		if len(st.live[0]) != 1999 || !st.dead[0][7] {
			t.Fatalf("v%d: replay holds %d live rows (want 1999) dead[7]=%v", version, len(st.live[0]), st.dead[0][7])
		}
		if got := st.live[0][1500]; len(got) != 2 || got[0].I != 1500 || !bytes.Equal(got[1].S, blob) {
			t.Fatalf("v%d: row 1500 read back as %v", version, got)
		}
	}
}
