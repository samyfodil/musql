package vec1

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestProbe runs the statements in $VEC1_PROBE (one per line) through both
// engines and prints what each answers, marking a difference. A tool for
// reading vec1's behaviour, not a check.
func TestProbe(t *testing.T) {
	src := os.Getenv("VEC1_PROBE")
	if src == "" {
		t.Skip("VEC1_PROBE not set")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	m, c := engines(t)
	for _, s := range strings.Split(string(b), "\n") {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}
		got, want := render(m, s), render(c, s)
		if got == want {
			fmt.Printf("> %s\n%s\n", s, want)
		} else {
			fmt.Printf("> %s\n!!! DIFF\n--- musql\n%s--- C\n%s\n", s, got, want)
		}
	}
}
