package sqlite

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests that conversion round-trips preserve all rows and rowids intact.
func TestConvertRoundTripPreservesEveryRow(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.musq")
	stmts := []string{
		`CREATE TABLE plain(a, b, c)`,
		`CREATE TABLE ipk(id INTEGER PRIMARY KEY, name TEXT, score REAL)`,
		`CREATE TABLE wide(i INTEGER, r REAL, s TEXT, bl BLOB, n)`,
	}
	for i := 1; i <= 300; i++ {
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO plain VALUES(%d,'t%d',NULL)`, i, i),
			fmt.Sprintf(`INSERT INTO ipk VALUES(%d,'n%d',%d.5)`, i*3, i, i))
	}
	// Deliberately mixed, so the exception list and the tagged fallback are
	// both on the round trip rather than just the clean path.
	stmts = append(stmts,
		`INSERT INTO wide VALUES(1, 2.5, 'three', x'0401', NULL)`,
		`INSERT INTO wide VALUES('one', 2, 3, 4, 5)`,
		`INSERT INTO wide VALUES(NULL, NULL, NULL, NULL, NULL)`,
		`DELETE FROM plain WHERE a % 7 = 0`, // leave gaps in the rowid sequence
	)
	buildDB(t, src, stmts...)
	execDB(t, src, `VACUUM`) // the rows in segments, the way an import leaves them
	back := roundTrip(t, src)
	for _, tbl := range []string{"plain", "ipk", "wide"} {
		before, after := dumpTableWithRowids(t, src, tbl), dumpTableWithRowids(t, back, tbl)
		if len(before) == 0 {
			t.Fatalf("%s: read back no rows from the source; this test would prove nothing", tbl)
		}
		if before != after {
			t.Fatalf("%s differs after the round trip:\n before %s\n after  %s", tbl, before, after)
		}
	}
}

// dumpTableWithRowids renders every row of a table, rowid first, in rowid
// order, with each value's TYPE as well as its text -- so an integer 1 that
// came back as the text "1" is a difference rather than a match.
func dumpTableWithRowids(t *testing.T, path, table string) string {
	t.Helper()
	out, err := queryDB(t, path, fmt.Sprintf(`SELECT rowid, * FROM "%s" ORDER BY rowid`, strings.ReplaceAll(table, `"`, `""`)))
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	return out
}

// Export is a FIXED POINT: a database exported, imported and exported again is
// byte-identical the second time. That is the strongest byte claim a converter
// can make without a C-written original to compare against -- and it fails for
// any lossy step, an unstable order, or a page layout that depends on anything
// but the content.
//
// A database whose rows were DELETEd from round trips with its content intact;
// its bytes need not match the original's history, because the export writes
// rows into fresh pages.
func TestConvertRoundTripByteIdentityLimits(t *testing.T) {
	build := func(withDeletes bool) string {
		path := filepath.Join(t.TempDir(), "src.musq")
		stmts := []string{`CREATE TABLE t(a INTEGER, b TEXT)`}
		for i := 1; i <= 400; i++ {
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d,'v%d')`, i, i))
		}
		if withDeletes {
			stmts = append(stmts, `DELETE FROM t WHERE a % 3 = 0`)
		}
		buildDB(t, path, stmts...)
		return path
	}
	export := func(src string) []byte {
		out := filepath.Join(t.TempDir(), "x.db")
		if err := Export(src, out, 0); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("export is a fixed point", func(t *testing.T) {
		src := build(false)
		first := export(src)
		second := export(roundTrip(t, src))
		if !bytes.Equal(first, second) {
			t.Fatalf("not byte-identical: %d bytes then %d, first difference at %d",
				len(first), len(second), firstDiffOffset(first, second))
		}
	})

	t.Run("deletions round trip with every row", func(t *testing.T) {
		src := build(true)
		back := roundTrip(t, src)
		before, after := dumpTableWithRowids(t, src, "t"), dumpTableWithRowids(t, back, "t")
		if before == "" || before != after {
			t.Fatalf("rows differ:\n before %s\n after  %s", before, after)
		}
	})
}

func firstDiffOffset(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
