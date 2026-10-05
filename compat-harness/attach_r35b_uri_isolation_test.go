package compat

// File URI ATTACH handling: relocates relative paths to avoid shared files
// between test instances, while preserving absolute paths, authorities, and
// query parameters that are already unambiguous.

import (
	"path/filepath"
	"strings"
	"testing"
)

// tclR35BIsolateFileURI relocates a "file:" URI's relative path, returning
// ok=false for URIs that should not be modified.
func tclR35BIsolateFileURI(uri, dir string) (string, bool) {
	if !strings.HasPrefix(uri, "file:") {
		return "", false
	}
	rest := uri[len("file:"):]
	tail := ""
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest, tail = rest[:i], rest[i:]
	}
	// Skip absolute paths, authorities, and empty paths.
	if rest == "" || strings.HasPrefix(rest, "/") {
		return "", false
	}
	// Skip percent-encoded paths.
	if strings.ContainsRune(rest, '%') {
		return "", false
	}
	return "file:" + filepath.Join(dir, rest) + tail, true
}

// TestR35BIsolateFileURI tests the file URI path relocation logic.
func TestR35BIsolateFileURI(t *testing.T) {
	const dir = "/tmp/seg"
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"file:test.db2", "file:/tmp/seg/test.db2", true},
		{"file:./test2.db?8_3_names=1", "file:/tmp/seg/test2.db?8_3_names=1", true},
		{"file:test.db2?mode=rw", "file:/tmp/seg/test.db2?mode=rw", true},
		{"file:a/b.db?x=1&y=2#frag", "file:/tmp/seg/a/b.db?x=1&y=2#frag", true},
		// Left alone: already unambiguous, names no file, or not a URI here.
		{"file:/abs/x.db", "", false},
		{"file://localhost/abs/x.db", "", false},
		{"file:?mode=memory", "", false},
		{"file:a%2Fb.db", "", false},
		{"FILE:x.db", "", false},
		{"test.db2", "", false},
		{":memory:", "", false},
	} {
		got, ok := tclR35BIsolateFileURI(tc.in, dir)
		if ok != tc.ok || got != tc.want {
			t.Errorf("tclR35BIsolateFileURI(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
