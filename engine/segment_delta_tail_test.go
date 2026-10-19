package engine

import (
	"os"
	"testing"
	"time"
)

// TestRewriteLeavesAWrittenAheadTail: a rewrite leaves the delta as a header
// and zeros, the next commit writes into that space without growing the file,
// and a fresh reader sees exactly the committed rows.
func TestRewriteLeavesAWrittenAheadTail(t *testing.T) {
	segPath := deltaFixture(t, 30)
	seg, err := os.Stat(segPath)
	if err != nil {
		t.Fatal(err)
	}
	d0, err := os.Stat(segDeltaPath(segPath))
	if err != nil {
		t.Fatalf("the rewrite left no delta: %v", err)
	}
	if want := segDeltaHdrSize + deltaTailSize(seg.Size()); d0.Size() != want {
		t.Fatalf("delta is %d bytes after a rewrite, want header + tail = %d", d0.Size(), want)
	}
	if got := len(deltaLogical(t, segPath)); got != segDeltaHdrSize {
		t.Fatalf("a fresh tail replays to byte %d, want %d", got, segDeltaHdrSize)
	}
	stmts := []string{`INSERT INTO t VALUES(500,5,'into the tail')`, `UPDATE t SET k = 7 WHERE id = 20`}
	execDB(t, segPath, stmts...)
	d1, err := os.Stat(segDeltaPath(segPath))
	if err != nil {
		t.Fatal(err)
	}
	if d1.Size() != d0.Size() {
		t.Errorf("a commit into the tail moved the delta from %d to %d bytes", d0.Size(), d1.Size())
	}
	if got, want := renderT(t, segPath), renderT(t, noDeltaFixture(t, 30, stmts...)); got != want {
		t.Errorf("rows after commits into the tail\n want:\n%s\n got:\n%s", want, got)
	}
}

// hideAppend puts the delta's mtime back, which is what a filesystem whose
// timestamps are coarser than two commits apart shows a reader: with the size
// unchanged too, only the bytes say anything happened.
func hideAppend(t *testing.T, segPath string, fi os.FileInfo) {
	t.Helper()
	if err := os.Chtimes(segDeltaPath(segPath), time.Now(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(segDeltaPath(segPath))
	if err != nil {
		t.Fatal(err)
	}
	if now.Size() != fi.Size() || !now.ModTime().Equal(fi.ModTime()) {
		t.Fatalf("the append was not hidden: size %d -> %d", fi.Size(), now.Size())
	}
}

// TestHeldSessionSeesAnAppendIntoTheTail: another connection's commit into the
// written-ahead tail, invisible to size and mtime, is still seen by a held
// session's freshness check (pairStamp's probe), and a commit by the held
// session after it neither loses that row nor reuses its state counter.
func TestHeldSessionSeesAnAppendIntoTheTail(t *testing.T) {
	segPath := deltaFixture(t, 6)
	a, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Discard()
	a.change = nil // the change word alone would send it to the stamps anyway
	fi, err := os.Stat(segDeltaPath(segPath))
	if err != nil {
		t.Fatal(err)
	}
	ctr0, _, err := SegmentFileState(segPath)
	if err != nil {
		t.Fatal(err)
	}
	execDB(t, segPath, `INSERT INTO t VALUES(700,7,'other')`)
	hideAppend(t, segPath, fi)

	if _, err := a.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	_, rows, err := a.Query(`SELECT count(*) FROM t WHERE id = 700`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].I != 1 {
		t.Fatalf("the held session did not see a commit made into the tail")
	}
	if err := a.Exec(`INSERT INTO t VALUES(800,8,'mine')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	ctr, _, err := SegmentFileState(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if ctr != ctr0+2 {
		t.Errorf("two commits moved the state counter %d -> %d, want +2", ctr0, ctr)
	}
	r, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	_, rows, err = r.Query(`SELECT group_concat(id) FROM t WHERE id >= 700`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rows[0][0].S); got != "700,800" {
		t.Errorf("rows after both commits: %q, want both, \"700,800\"", got)
	}
}

// TestCarriedAppendPointRejectsAnotherWritersBatch: an append point carried
// from this writer's last batch is not trusted once another writer's batch
// sits there, though the file's size did not move.
func TestCarriedAppendPointRejectsAnotherWritersBatch(t *testing.T) {
	segPath := deltaFixture(t, 6)
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	put := func(rowid int64) []SegDeltaRecord {
		return []SegDeltaRecord{{Table: 0, Rowid: rowid, Vals: []Value{{Typ: Null}, {Typ: Int, I: rowid}, {Typ: Text, S: []byte("x")}}}}
	}
	var mine, other SegDeltaAppendState
	defer mine.release()
	defer other.release()
	ctr := baseCtr
	for _, step := range []struct {
		carry *SegDeltaAppendState
		rowid int64
	}{{&mine, 901}, {&other, 902}, {&mine, 903}} {
		ctr++
		if err := appendSegmentDeltaAt(segPath, baseCtr, basePages, put(step.rowid), ctr, basePages, step.carry, syncNone); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(segDeltaPath(segPath))
	if err != nil {
		t.Fatal(err)
	}
	st, err := replaySegDelta(data, baseCtr, basePages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.batches != 3 {
		t.Fatalf("%d batches replay, want 3: the stale append point wrote over another writer's batch", st.batches)
	}
	for _, r := range []int64{901, 902, 903} {
		if _, ok := st.live[0][r]; !ok {
			t.Errorf("row %d is not in the delta", r)
		}
	}
}
