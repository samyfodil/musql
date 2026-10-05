package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPragmaR24DumpSegment prints one mined segment verbatim so a decline can be
// read IN CONTEXT (the statements that preceded it decide what the oracle does).
// Off unless R24_SEG names it, as "<file>.test#<n>".
func TestPragmaR24DumpSegment(t *testing.T) {
	spec := os.Getenv("R24_SEG")
	if spec == "" {
		t.Skip("set R24_SEG=<file>.test#<n>")
	}
	for _, one := range strings.Split(spec, ",") {
		parts := strings.SplitN(one, "#", 2)
		if len(parts) != 2 {
			t.Fatalf("bad R24_SEG %q", one)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			t.Fatalf("bad R24_SEG %q: %v", one, err)
		}
		src, err := os.ReadFile(filepath.Join(tclCorpusDir, parts[0]))
		if err != nil {
			t.Fatalf("%v", err)
		}
		segs := tclSegments(string(src))
		if n >= len(segs) {
			t.Fatalf("%s has %d segments", parts[0], len(segs))
		}
		for i, s := range segs[n] {
			fmt.Printf("%s\t%d\t%s\n", one, i, strings.Join(strings.Fields(s), " "))
		}
	}
}
