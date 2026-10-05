package sqlite

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// makeDB creates a C SQLite database at the requested page size: a musql
// database, exported.
func makeDB(t *testing.T, pageSize int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("db_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if eerr := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, val REAL)"); eerr != nil {
		t.Fatal(eerr)
	}
	for i := 1; i <= 500; i++ {
		if _, _, eerr := db.ExecArgs("INSERT INTO t(id,name,val) VALUES(?,?,?)", []Value{
			{Typ: Int, I: int64(i)},
			{Typ: Text, S: []byte(fmt.Sprintf("row-%d-with-some-padding-text", i))},
			{Typ: Float, F: float64(i) * 1.5},
		}); eerr != nil {
			t.Fatal(eerr)
		}
	}
	if cerr := db.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	toSQLiteInPlace(t, path, pageSize)
	return path
}

func TestPagerReadsRealDatabase(t *testing.T) {
	for _, pageSize := range []int{512, 1024, 4096, 65536} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := makeDB(t, pageSize)

			p, err := openPager(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer p.Close()

			h := p.hdr
			if got := int(h.PageSize); got != pageSize {
				t.Errorf("PageSize = %d, want %d", got, pageSize)
			}
			if h.TextEncoding != UTF8 {
				t.Errorf("TextEncoding = %d, want UTF8", h.TextEncoding)
			}
			if h.WriteVersion != 1 { // DELETE journal mode => version 1
				t.Errorf("WriteVersion = %d, want 1 (rollback journal)", h.WriteVersion)
			}
			if p.walUnreadable {
				t.Error("walUnreadable for a cleanly-closed rollback-journal db")
			}

			// Page count must match the file length, and be > 1 (multi-page).
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantPages := uint32(len(raw) / pageSize)
			if p.PageCount() != wantPages {
				t.Errorf("PageCount = %d, want %d", p.PageCount(), wantPages)
			}
			if wantPages < 2 {
				t.Fatalf("expected a multi-page db, got %d pages", wantPages)
			}

			// Every page the pager serves must equal the raw on-disk bytes.
			for pg := uint32(1); pg <= p.PageCount(); pg++ {
				got, err := p.page(pg)
				if err != nil {
					t.Fatalf("Page(%d): %v", pg, err)
				}
				want := raw[int(pg-1)*pageSize : int(pg)*pageSize]
				if !bytes.Equal(got, want) {
					t.Fatalf("page %d bytes differ from on-disk", pg)
				}
			}

			// Out-of-range pages error.
			if _, err := p.page(0); err == nil {
				t.Error("Page(0) should error")
			}
			if _, err := p.page(p.PageCount() + 1); err == nil {
				t.Error("Page(count+1) should error")
			}
		})
	}
}

func TestParseHeaderErrors(t *testing.T) {
	good := make([]byte, HeaderSize)
	copy(good, headerMagic)
	good[16], good[17] = 0x10, 0x00 // page size 4096
	good[18], good[19] = 1, 1
	good[21], good[22], good[23] = 64, 32, 32
	good[56+3] = 1 // UTF8 text encoding (offset 56, big-endian, low byte)
	if _, err := ParseHeader(good); err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}

	cases := map[string]func([]byte){
		"bad-magic":     func(b []byte) { b[0] = 'X' },
		"zero-pagesize": func(b []byte) { b[16], b[17] = 0, 0 },
		"npot-pagesize": func(b []byte) { b[16], b[17] = 0x03, 0x00 }, // 768, not power of two
		"bad-fraction":  func(b []byte) { b[21] = 65 },
		"bad-encoding":  func(b []byte) { b[56+3] = 9 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			mut(b)
			if _, err := ParseHeader(b); err == nil {
				t.Errorf("%s: expected error, got none", name)
			}
		})
	}
	if _, err := ParseHeader(make([]byte, 50)); err == nil {
		t.Error("short buffer should error")
	}
}
