package compat

// Temporary census helper for round 30's fts5 bucket: dumps the mined segments
// of one corpus file so a decline's ROOT can be read in context. Enabled only
// when TCL_DUMP_SEGMENTS names a file; inert otherwise.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFts5R30DumpSegments(t *testing.T) {
	rel := os.Getenv("TCL_DUMP_SEGMENTS")
	if rel == "" {
		t.Skip("set TCL_DUMP_SEGMENTS=<file.test> to dump its mined segments")
	}
	src, err := os.ReadFile(filepath.Join(tclCorpusDir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	for si, stmts := range tclSegments(string(src)) {
		for i, s := range stmts {
			t.Logf("SEG\t%s#%d\t%d\t%s", rel, si, i, strings.Join(strings.Fields(s), " "))
		}
	}
}
