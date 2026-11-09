package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNestedExistsCompilesInLinearTime: EXISTS nested sixteen deep, each
// level's body naming a column and holding two EXISTS of its own (turso's
// deep-correlated-subqueries-prepare-once), compiles in about the time its
// size takes. Trial compiles of WHERE sub-selects and the blanked recompile
// of each EXISTS body once made every level double the work.
func TestNestedExistsCompilesInLinearTime(t *testing.T) {
	n, err := OpenOrCreate(filepath.Join(t.TempDir(), "deep.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	for _, s := range []string{`CREATE TABLE r(k INTEGER)`, `CREATE TABLE g(k INTEGER)`, `INSERT INTO r VALUES (1), (2)`, `INSERT INTO g VALUES (1)`} {
		if _, _, err := n.ExecArgs(s, nil); err != nil {
			t.Fatal(err)
		}
		n.CommitIfAutocommit()
	}
	const depth = 16
	var b strings.Builder
	b.WriteString("SELECT r0.k FROM r r0 WHERE EXISTS (SELECT 1 FROM g g0 WHERE g0.k = r0.k)")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, " AND EXISTS (SELECT r%d.k FROM r r%d WHERE r%d.k = r%d.k AND EXISTS (SELECT 1 FROM g g%d WHERE g%d.k = r%d.k)", i, i, i, i-1, i, i, i)
	}
	b.WriteString(strings.Repeat(")", depth))
	start := time.Now()
	_, rows, err := n.Query(b.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0].I != 1 {
		t.Fatalf("rows %v, want [[1]]", rows)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("compiling %d nested levels took %v", depth, d)
	}
}
