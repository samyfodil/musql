package engine

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCatchUpOverAnAppendedSegment: a batch that appends a segment (an append
// load) folds in like any other, and the session then answers and indexes as
// one opened on the file does.
func TestCatchUpOverAnAppendedSegment(t *testing.T) {
	defer SetAppendArmAfterForTest(SetAppendArmAfterForTest(1))
	path, a, b := catchUpPair(t)
	mustCommit(t, b, `CREATE TABLE q(id INTEGER PRIMARY KEY, v TEXT)`, `INSERT INTO q VALUES(1, 'one')`)
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{a, b} {
		if _, err := s.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
	}
	buildIndexes(t, a)
	b.SetCommitsEachStatement(true)
	if err := b.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 302; i++ {
		if err := b.Exec(fmt.Sprintf(`INSERT INTO q VALUES(%d, 'n%d')`, i, i)); err != nil {
			t.Fatal(err)
		}
	}
	if b.tableNamed("q").bulk == nil {
		t.Fatal("the inserts armed no append load")
	}
	if err := b.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	mustCommit(t, a, `UPDATE p SET s = 'a' WHERE id = 10`)
	fresh, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Discard()
	for _, q := range []string{`SELECT count(*), max(id), min(v) FROM q`, `SELECT id, s FROM p WHERE k = 3 ORDER BY id`, `SELECT s FROM p WHERE id = 10`} {
		if got, want := render(t, a, q), render(t, fresh, q); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", q, got, want)
		}
	}
	indexesExact(t, path, a)
}

// TestDeltaMoved answers from the bytes around where a session's view of the
// delta ends, in both length widths.
func TestDeltaMoved(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []uint32{1, segDeltaVersion} {
		data, ends, sums := craftDelta(v, 99, craftedBodies()...)
		path := filepath.Join(dir, fmt.Sprintf("v%d.musq", v))
		if err := os.WriteFile(segDeltaPath(path), data, 0o644); err != nil {
			t.Fatal(err)
		}
		at := func(end int64, cksum uint64) *Session {
			return &Session{segPath: path, deltaEnd: end, appendAt: SegDeltaAppendState{offset: end, cksum: cksum, version: v, known: true}}
		}
		var now [2]fileStamp
		if moved, ok := at(ends[0], sums[0]).deltaMoved(now); !ok || !moved {
			t.Errorf("v%d: a batch past the view: moved %v ok %v", v, moved, ok)
		}
		if moved, ok := at(ends[1], sums[1]).deltaMoved(now); !ok || moved {
			t.Errorf("v%d: the view at the end: moved %v ok %v", v, moved, ok)
		}
		if _, ok := at(ends[0], sums[1]).deltaMoved(now); ok {
			t.Errorf("v%d: a checksum not there: ok", v)
		}
		unknown := at(ends[0], sums[0])
		unknown.appendAt.known = false
		if _, ok := unknown.deltaMoved(now); ok {
			t.Errorf("v%d: an unknown view: ok", v)
		}
		zeroed := append(data[:ends[0]:ends[0]], make([]byte, 16)...)
		if err := os.WriteFile(segDeltaPath(path), zeroed, 0o644); err != nil {
			t.Fatal(err)
		}
		if moved, ok := at(ends[0], sums[0]).deltaMoved(now); !ok || moved {
			t.Errorf("v%d: a zero length word: moved %v ok %v", v, moved, ok)
		}
		binary.LittleEndian.PutUint32(zeroed[ends[0]:], 1)
		if err := os.WriteFile(segDeltaPath(path), zeroed, 0o644); err != nil {
			t.Fatal(err)
		}
		if moved, ok := at(ends[0], sums[0]).deltaMoved(now); !ok || !moved {
			t.Errorf("v%d: a length word set: moved %v ok %v", v, moved, ok)
		}
	}
	if _, ok := (&Session{segPath: filepath.Join(dir, "none.musq"), appendAt: SegDeltaAppendState{known: true}}).deltaMoved([2]fileStamp{}); ok {
		t.Error("a missing delta: ok")
	}
}
