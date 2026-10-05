package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/internal/mmapfile"
)

// skipUnlessMapped skips a test that measures what mapping a file saves, on a
// platform where mmapfile reads files instead (Windows).
func skipUnlessMapped(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := mmapfile.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !m.Mapped() {
		t.Skip("files are read, not mapped, on this platform")
	}
}
