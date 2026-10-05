package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDumpCorpusSegment prints a corpus segment's statements in order.
// Set CORPUS_SEGMENT="file.test#N[,...]" to dump a specific segment.
func TestDumpCorpusSegment(t *testing.T) {
	spec := os.Getenv("CORPUS_SEGMENT")
	if spec == "" {
		t.Skip("set CORPUS_SEGMENT=file.test#N[,file.test#N...]")
	}
	for _, one := range strings.Split(spec, ",") {
		parts := strings.SplitN(one, "#", 2)
		n, _ := strconv.Atoi(parts[1])
		src, err := os.ReadFile(filepath.Join(tclCorpusDir, parts[0]))
		if err != nil {
			t.Fatal(err)
		}
		segs := tclSegments(string(src))
		if n >= len(segs) {
			t.Fatalf("%s has %d segments", parts[0], len(segs))
		}
		var b strings.Builder
		for i, s := range segs[n] {
			fmt.Fprintf(&b, "%3d| %s\n", i, s)
		}
		t.Logf("=== %s (%d statements)\n%s", one, len(segs[n]), b.String())
	}
}
