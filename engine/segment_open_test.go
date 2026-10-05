package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenSegmentSeesTheDelta opens a segment file whose writes are still in the
// log, with no source database present.
func TestOpenSegmentSeesTheDelta(t *testing.T) {
	stmts := []string{`INSERT INTO t VALUES(35,350,'mid')`, `DELETE FROM t WHERE id = 20`, `UPDATE t SET k = -1 WHERE id = 30`}
	segPath := deltaFixture(t, 6, stmts...)
	truth, terr := Open(noDeltaFixture(t, 6, stmts...))
	if terr != nil {
		t.Fatal(terr)
	}
	want := renderRows(t, truth, `SELECT id, k, s FROM t ORDER BY id`)
	truth.Close()

	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatalf("Open: %v", nerr)
	}
	defer np.Close()
	if got := renderRows(t, np, `SELECT id, k, s FROM t ORDER BY id`); got != want {
		t.Errorf("a segment open missed the delta\n want: %s\n got:  %s", want, got)
	}
}

// TestOpenSegmentReportsIndexesWithoutUsingThem is the safety argument for
// carrying an index's SQL and not its data: it must be REPORTED, so a
// convert-back rebuilds it, and never SEEKED into, because there is nothing
// there. A seek would return no rows -- a wrong answer, not a slow one.
func TestOpenSegmentReportsIndexesWithoutUsingThem(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "orig.musq")
	buildDB(t, orig,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)`,
		`CREATE INDEX t_k ON t(k)`,
		`INSERT INTO t VALUES(10,100),(20,200),(30,300)`)
	execDB(t, orig, `VACUUM`) // the rows in segments
	np, nerr := Open(orig)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np.Close()

	schema, serr := np.Schema()
	if serr != nil {
		t.Fatal(serr)
	}
	var sawIndex bool
	for _, r := range schema {
		if r.Type == "index" && r.Name == "t_k" {
			sawIndex = true
			if r.RootPage != 0 {
				t.Errorf("index t_k has RootPage %d in a segment open; a planner that seeks "+
					"into it reads a b-tree this file does not carry", r.RootPage)
			}
			if r.SQL == "" {
				t.Error("index t_k has no SQL, so a convert-back could not rebuild it")
			}
		}
	}
	if !sawIndex {
		t.Error("a segment open does not report the index at all, so a convert-back would drop it")
	}
	// ...and a query the index would have served still answers correctly.
	if got := renderRows(t, np, `SELECT id FROM t WHERE k = 200`); got != "1:20|" {
		t.Errorf("SELECT id FROM t WHERE k = 200 = %q, want \"1:20|\"", got)
	}
}

// renderRows is renderQuery's rows as one comparable string.
func renderRows(t *testing.T, rp *ReadOnlyPager, q string) string {
	t.Helper()
	return strings.Join(renderQuery(t, rp, q), ";")
}
