package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoCurlyQuotesInSource prevents gofmt from corrupting SQL in comments.
// Go 1.19+ gofmt converts '' to curly quotes in doc comments, corrupting SQL
// empty strings. Use go vet instead of gofmt -w on packages.
func TestNoCurlyQuotesInSource(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	var bad []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable tree entry: not this gate's business
		}
		if d.IsDir() {
			// .claude may hold sibling worktrees -- other checkouts of this
			// same repo, whose state is not this commit's to police.
			switch d.Name() {
			case ".git", ".claude", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		scanned++
		// Written as escapes, not literals: this file is scanned too, and a
		// literal curly quote here would make the gate fail on itself.
		for _, q := range []string{"\u201c", "\u201d"} {
			if i := strings.Index(string(b), q); i >= 0 {
				line := 1 + strings.Count(string(b[:i]), "\n")
				rel, _ := filepath.Rel(root, path)
				bad = append(bad, rel+":"+strconv.Itoa(line))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// A gate that scanned nothing passes vacuously -- the same trap
	// no_interpreter_gate_test.go guards against.
	if scanned < 100 {
		t.Fatalf("only %d .go files scanned from %s -- the gate would pass vacuously", scanned, root)
	}
	if len(bad) > 0 {
		t.Fatalf("curly quotes in %d Go source file(s): %v\n\n"+
			"These are almost certainly '' (an empty SQL string, or an escaped quote inside one)\n"+
			"that `gofmt -w` rewrote via go/doc/comment's troff-quote rule. Restore them to ''\n"+
			"and do not run gofmt -w across a package in this repo. See this test's doc comment.",
			len(bad), bad)
	}
}
