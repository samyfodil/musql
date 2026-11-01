package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// TestMultiColumnSeekWeighsEveryClass: the key a multi-column seek asks first
// is weighed by its segments' counts for BLOB and REAL probes too, present or
// absent, and every answer is the scan's.
func TestMultiColumnSeekWeighsEveryClass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.musq")
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b BLOB, r REAL)`}
	for i := 1; i <= 3000; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, x'%04x', %d.5)`, i, i%3, i%40, i%25))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, q := range []string{
		`SELECT id FROM t WHERE a = 1 AND b = x'0007'`,
		`SELECT id FROM t WHERE a = 1 AND b = x'ffee'`,
		`SELECT id FROM t WHERE a = 2 AND r = 3.5`,
		`SELECT id FROM t WHERE a = 2 AND r = 9.75`,
		`SELECT id FROM t WHERE b = x'0003' AND r = 3.5`,
	} {
		indexSeeksOffForTest = true
		_, want, err := p.QueryArgs(q, nil)
		indexSeeksOffForTest = false
		if err != nil {
			t.Fatal(err)
		}
		_, got, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %v\nwant %v", q, got, want)
		}
	}
}
