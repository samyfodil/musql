package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The engine has no relationship to the SQLite file format.
func TestEngineCarriesNoSQLiteFormat(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if strings.Contains(string(b), "SQLite format 3") {
			t.Errorf("%s carries the SQLite file magic: SQLite-format code belongs in convert/sqlite", f)
		}
	}
}

// TestOnlyTheCLICallsTheConverter: in this module, the converter's only
// importers are its own package and the conversion tool.
func TestOnlyTheCLICallsTheConverter(t *testing.T) {
	conv := `"github.com/samyfodil/musql/convert/` + `sqlite"`
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			// compat-harness is a module of its own, whose oracle is C SQLite.
			if rel == "compat-harness" || rel == "convert" || rel == filepath.Join("cmd", "musql-convert") || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), conv) {
			t.Errorf("%s imports the converter: only cmd/musql-convert may", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
