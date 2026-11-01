package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPagerWritesOutAndReadsBack: pages shrink wrote out come back with their
// content and flags, and the commit writes a dontWrite page as zeros.
func TestPagerWritesOutAndReadsBack(t *testing.T) {
	const ps = 512
	f, err := os.Create(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pg := newBtPager(ps, 0, true, func(uint32, []byte) error { return errBtCorrupt })
	pg.file, pg.maxPages = f, 2
	bt := newBtShared(pg, ps, 0)
	for pgno := uint32(1); pgno <= 6; pgno++ {
		m, err := pg.get(bt, pgno, false)
		if err != nil {
			t.Fatal(err)
		}
		pg.write(m)
		for i := range m.aData {
			m.aData[i] = byte(pgno)
		}
	}
	if err := pg.shrink(bt); err != nil {
		t.Fatal(err)
	}
	if len(pg.pages) > 2 {
		t.Fatalf("%d pages still held", len(pg.pages))
	}
	gone := 0
	for pgno := uint32(2); pgno <= 6; pgno++ {
		if pg.pages[pgno] == nil {
			gone++
		}
		m, err := pg.lookup(bt, pgno)
		if err != nil || m == nil {
			t.Fatalf("lookup(%d) = %v, %v", pgno, m, err)
		}
		if !bytes.Equal(m.aData, bytes.Repeat([]byte{byte(pgno)}, ps)) || !pg.pages[pgno].dirty {
			t.Fatalf("page %d came back changed", pgno)
		}
		if pgno == 3 {
			pg.dontWrite(m)
		}
	}
	if gone == 0 {
		t.Fatal("shrink wrote nothing out")
	}
	if m, err := pg.lookup(bt, 9); m != nil || err != nil {
		t.Fatalf("lookup of a page never held = %v, %v", m, err)
	}
	if err := pg.shrink(bt); err != nil {
		t.Fatal(err)
	}
	if err := pg.commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6*ps {
		t.Fatalf("file is %d bytes, want %d", len(got), 6*ps)
	}
	for pgno := 1; pgno <= 6; pgno++ {
		want := bytes.Repeat([]byte{byte(pgno)}, ps)
		if pgno == 3 {
			want = make([]byte, ps) // freed: no content to keep
		}
		if !bytes.Equal(got[(pgno-1)*ps:pgno*ps], want) {
			t.Errorf("page %d holds %d..., want %d...", pgno, got[(pgno-1)*ps], want[0])
		}
	}
}
