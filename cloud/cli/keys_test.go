package cli

import (
	"io"
	"testing"
)

type chunks struct{ parts []string }

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts = c.parts[1:]
	return n, nil
}

// TestKeyReader: an alias is rewritten even when a read cuts it in two.
func TestKeyReader(t *testing.T) {
	r := &keyReader{r: &chunks{parts: []string{"ab\x1bO", "Hc\x1b", "[4~d\x1b[", "8", "~\x1bOD"}}}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ab\x1b[Hc\x1b[Fd\x1b[F\x1b[D"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
