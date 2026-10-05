package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestPagesR25DumpSegment prints one mined segment verbatim, so a decline can be
// read in the context the corpus actually replays it in. Purely a development
// aid: it does nothing unless PAGES_R25_FILE is set.
//
//	PAGES_R25_FILE=corruptN.test PAGES_R25_SEG=1 go test -run TestPagesR25DumpSegment -v ./
func TestPagesR25DumpSegment(t *testing.T) {
	rel := os.Getenv("PAGES_R25_FILE")
	if rel == "" {
		t.Skip("set PAGES_R25_FILE (and optionally PAGES_R25_SEG) to dump a segment")
	}
	src, err := os.ReadFile(filepath.Join(tclCorpusDir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	segments := tclSegments(string(src))
	want := -1
	if s := os.Getenv("PAGES_R25_SEG"); s != "" {
		want, _ = strconv.Atoi(s)
	}
	for si, stmts := range segments {
		if want >= 0 && si != want {
			continue
		}
		fmt.Printf("==== %s#%d (%d statements)\n", rel, si, len(stmts))
		for i, s := range stmts {
			fmt.Printf("  #%d %s\n", i, s)
		}
	}
}
