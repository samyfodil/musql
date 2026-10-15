package engine

import (
	"encoding/binary"
	"os"
	"testing"
)

// TestSegmentDeltaZeroTailEndsTheLog: zeros after the last batch are the
// unwritten tail, not history, and the next commit appends over them.
func TestSegmentDeltaZeroTailEndsTheLog(t *testing.T) {
	segPath := deltaFixture(t, 6, `UPDATE t SET k = 42 WHERE id = 20`)
	good := renderT(t, segPath)
	deltaPath := segDeltaPath(segPath)
	data, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	tailed := append(append([]byte(nil), data...), make([]byte, 4096)...)
	if err := os.WriteFile(deltaPath, tailed, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := replaySegDelta(tailed, baseCtr, basePages)
	if err != nil {
		t.Fatal(err)
	}
	if st.bytes != int64(len(data)) || st.batches != 1 {
		t.Errorf("replay reached byte %d after %d batches, want %d after 1", st.bytes, st.batches, len(data))
	}
	if got := renderT(t, segPath); got != good {
		t.Errorf("a zero tail changed the answer\n before:\n%s\n after:\n%s", good, got)
	}
	stmt := `INSERT INTO t VALUES(500,5,'over the tail')`
	execDB(t, segPath, stmt)
	want := renderT(t, noDeltaFixture(t, 6, `UPDATE t SET k = 42 WHERE id = 20`, stmt))
	if got := renderT(t, segPath); got != want {
		t.Errorf("a commit over a zero tail\n want:\n%s\n got:\n%s", want, got)
	}

	// A half-written batch inside the zeros is torn, like any other.
	torn := append(append([]byte(nil), data...), 0x40, 0, 0, 0, 0, 0, 0, 0, 0xde, 0xad)
	torn = append(torn, make([]byte, 4096)...)
	st2, err := replaySegDelta(torn, baseCtr, basePages)
	if err != nil {
		t.Fatal(err)
	}
	if st2.bytes != int64(len(data)) || st2.batches != 1 {
		t.Errorf("a torn batch in the tail: byte %d after %d batches, want %d after 1", st2.bytes, st2.batches, len(data))
	}
}

// TestSegmentDeltaEmptyBatchIsNotHistory: a zero-length batch whose trailer
// checksums correctly is still the end of the log. Without the rule it
// would replay as a commit and move the counter.
func TestSegmentDeltaEmptyBatchIsNotHistory(t *testing.T) {
	segPath := deltaFixture(t, 6, `UPDATE t SET k = 42 WHERE id = 20`)
	data, err := os.ReadFile(segDeltaPath(segPath))
	if err != nil {
		t.Fatal(err)
	}
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := replaySegDelta(data, baseCtr, basePages)
	if err != nil {
		t.Fatal(err)
	}
	w := segDeltaLenWidth(st.version)
	b := make([]byte, w+8)
	binary.LittleEndian.PutUint32(b[w:], st.endCtr+1)
	binary.LittleEndian.PutUint32(b[w+4:], st.endPages)
	b = binary.LittleEndian.AppendUint64(b, segDeltaChecksum(b, st.cksum))
	st2, err := replaySegDelta(append(append([]byte(nil), data...), b...), baseCtr, basePages)
	if err != nil {
		t.Fatal(err)
	}
	if st2.batches != st.batches || st2.endCtr != st.endCtr {
		t.Errorf("an empty batch replayed: %d batches, counter %d; want %d, %d", st2.batches, st2.endCtr, st.batches, st.endCtr)
	}
}

// TestSegmentDeltaCreateSyncsDirectory: the commit that creates the delta
// makes its directory entry durable; later appends need not.
func TestSegmentDeltaCreateSyncsDirectory(t *testing.T) {
	var calls int
	orig := syncDeltaDir
	syncDeltaDir = func(p string) error { calls++; return orig(p) }
	defer func() { syncDeltaDir = orig }()
	segPath := deltaFixture(t, 6)
	if _, err := os.Stat(segDeltaPath(segPath)); !os.IsNotExist(err) {
		t.Fatalf("fixture already has a delta: %v", err)
	}
	execDB(t, segPath, `UPDATE t SET k = 1 WHERE id = 20`)
	if calls != 1 {
		t.Fatalf("creating the delta synced its directory %d times, want 1", calls)
	}
	execDB(t, segPath, `UPDATE t SET k = 2 WHERE id = 20`)
	if calls != 1 {
		t.Errorf("an append to an existing delta synced the directory (%d calls)", calls)
	}
}
