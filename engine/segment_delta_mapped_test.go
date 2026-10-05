package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSecondConnectionMapsTheDelta opens a database whose delta is large and
// not yet compacted -- the window after a big commit -- from a second session,
// and requires its heap to stay far below the delta's size. The delta used to
// be read onto the heap of every connection that opened the file in that
// window; bigsort.test 1.0's 2.8 GB log would not open under a 2 GB cap.
func TestSecondConnectionMapsTheDelta(t *testing.T) {
	skipUnlessMapped(t)
	compactionOffForTest = true
	defer func() { compactionOffForTest = false }()
	path := filepath.Join(t.TempDir(), "d.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`CREATE TABLE t(a, b)`, `BEGIN`,
		`WITH d(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM d WHERE x < 20000) INSERT INTO t SELECT x, randomblob(10000) FROM d`,
		`COMMIT`} {
		if err := n.Exec(s); err != nil {
			t.Fatal(err)
		}
		if _, err := n.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	n.Discard()
	fi, err := os.Stat(path + segDeltaSuffix)
	if err != nil {
		t.Fatal(err)
	}
	heap := func() int64 {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	before := heap()
	r, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	_, rows, err := r.Query(`SELECT count(*), sum(length(b)) FROM t`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].I != 20000 || rows[0][1].I != 20000*10000 {
		t.Fatalf("read back %d rows, %d bytes", rows[0][0].I, rows[0][1].I)
	}
	if grew := heap() - before; grew > fi.Size()/4 {
		t.Errorf("opening a %d MiB delta grew the heap by %d MiB", fi.Size()>>20, grew>>20)
	}
}
