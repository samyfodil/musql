package mmapfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReadOut checks that a slice of the mapping reads back as the same
// bytes where the platform reads rather than faults, and that bytes outside
// the mapping are left to the caller.
func TestReadOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	want := make([]byte, 1<<16)
	for i := range want {
		want[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	part := f.Data()[1000:5000]
	got, ok := f.ReadOut(part)
	if ok != (readNotFault && f.Mapped()) {
		t.Fatalf("ReadOut ok = %v, want %v", ok, readNotFault && f.Mapped())
	}
	if ok && !bytes.Equal(got, want[1000:5000]) {
		t.Fatal("ReadOut read other bytes")
	}
	if _, ok := f.ReadOut(make([]byte, 10)); ok {
		t.Fatal("ReadOut took bytes outside the mapping")
	}
}
