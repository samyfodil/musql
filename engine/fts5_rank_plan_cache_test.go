//go:build sqlite_fts5

package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFts5RankChangeIsNotServedFromAStalePlan verifies plan cache invalidation
// when fts5 rank function changes (a data write, not a schema change)
func TestFts5RankChangeIsNotServedFromAStalePlan(t *testing.T) {
	sess, err := Create(filepath.Join(t.TempDir(), "rank.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	for _, s := range []string{
		`CREATE VIRTUAL TABLE tt USING fts5(a)`,
		`INSERT INTO tt VALUES('a x x x x')`,
		`INSERT INTO tt VALUES('x x a a a')`,
		`INSERT INTO tt VALUES('x a a x x')`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	const q = `SELECT rowid FROM tt('a') ORDER BY rank`
	if _, _, e := sess.Query(q, nil); e != nil {
		t.Fatalf("with no 'rank' row the default is bm25 and this must answer: %v", e)
	}
	if e := sess.Exec(`INSERT INTO tt(tt, rank) VALUES('rank', 'firstinst()')`); e != nil {
		t.Fatalf("the 'rank' command stores its string verbatim and must succeed: %v", e)
	}
	// Same query with invalid rank function
	_, rows, qerr := sess.Query(q, nil)
	if qerr == nil {
		t.Fatalf("answered %d row(s) from a plan compiled before the rank changed; "+
			"C SQLite raises \"no such function: firstinst\"", len(rows))
	}
	if !strings.Contains(qerr.Error(), "no such function: firstinst") {
		t.Fatalf("error is %q, want C fts5's \"no such function: firstinst\"", qerr)
	}
}
