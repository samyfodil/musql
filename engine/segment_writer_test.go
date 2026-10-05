package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSegFileWriterScratchMatchesMemory writes the same tables through
// segFileWriter twice -- once held in memory, once moved to its scratch file
// partway -- and requires the two files to be byte for byte the same, odd
// segment lengths included (the 8-byte alignment between them).
func TestSegFileWriterScratchMatchesMemory(t *testing.T) {
	tables := []ConvertedTable{
		{Name: "a", SQL: "CREATE TABLE a(x)", Cols: []string{"x"}, IPK: -1},
		{Name: "b", SQL: "CREATE TABLE b(y)", Cols: []string{"y"}, IPK: -1},
	}
	segs := [][2]int{{0, 1001}, {0, 77}, {1, 5000}, {1, 3}, {1, 40000}}
	write := func(limit int64) []byte {
		saved := segWriterMemBytes
		segWriterMemBytes = limit
		defer func() { segWriterMemBytes = saved }()
		dir := t.TempDir()
		w, err := newSegFileWriter(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer w.discard()
		for i, s := range segs {
			if err := w.addSegment(s[0], bytes.Repeat([]byte{byte(i + 1)}, s[1])); err != nil {
				t.Fatal(err)
			}
		}
		if (limit < 1<<20) != (w.scratch != nil) {
			t.Fatalf("limit %d: scratch in use = %v", limit, w.scratch != nil)
		}
		path := filepath.Join(dir, "f.musq")
		if err := w.finish(path, tables, ConvertedCatalog{}, 3, 4); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	inMem, viaScratch := write(1<<30), write(6000)
	if !bytes.Equal(inMem, viaScratch) {
		t.Fatalf("the scratch path wrote %d bytes where memory wrote %d, or different ones", len(viaScratch), len(inMem))
	}
}
