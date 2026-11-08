package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestGroupByOrderByAliasReadsNoAnchor: an ORDER BY naming an output alias
// reads that output column, not the group's row, so a GROUP BY whose result
// names only its keys keeps just those columns of its anchor -- even when the
// alias is also a table column's name, which the alias shadows. A bare or
// qualified table column in the ORDER BY still reads the anchor.
func TestGroupByOrderByAliasReadsNoAnchor(t *testing.T) {
	p := argRegsFixture(t)
	for _, tc := range []struct {
		sql    string
		narrow bool
	}{
		{`SELECT k, count(*) AS n FROM t GROUP BY k ORDER BY n DESC, k`, true},
		{`SELECT k, count(*) AS s FROM t GROUP BY k ORDER BY s`, true},
		{`SELECT k, count(*) AS n FROM t GROUP BY k ORDER BY s`, false},
		{`SELECT k, count(*) AS s FROM t GROUP BY k ORDER BY t.s`, false},
	} {
		pl := aggPlanOf(t, p, tc.sql)
		if narrow := pl.rowCols != nil && pl.anchorKeep != nil; narrow != tc.narrow {
			t.Errorf("%s: rowCols %v anchorKeep %v, want narrow=%v", tc.sql, pl.rowCols, pl.anchorKeep, tc.narrow)
		}
	}
}

// TestGroupByAnchorKeepSpellsAsCopied: a group's key columns read from its
// anchor spell exactly as when the anchor is copied on every row -- here forced
// by a bare column -- including keys that compare equal but are spelled
// differently (1 and 1.0, 'a' and 'A' under NOCASE).
func TestGroupByAnchorKeepSpellsAsCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE u(x, y TEXT COLLATE NOCASE, z)`); err != nil {
		t.Fatal(err)
	}
	vals := []string{`1, 'a'`, `1.0, 'A'`, `2, 'b'`, `2, 'b'`, `2.0, 'B'`, `2, 'b'`, `3, 'c'`, `3.0, 'c'`}
	for i, v := range vals {
		if err := db.Exec(fmt.Sprintf(`INSERT INTO u VALUES(%s, %d)`, v, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, key := range []string{"x", "y"} {
		keep := fmt.Sprintf(`SELECT %s, count(*) AS n FROM u GROUP BY %[1]s ORDER BY n DESC`, key)
		copied := fmt.Sprintf(`SELECT %s, count(*) AS n, z FROM u GROUP BY %[1]s ORDER BY n DESC`, key)
		if pl := aggPlanOf(t, p, keep); pl.anchorKeep == nil {
			t.Fatalf("%s: no anchorKeep, so this compares nothing", keep)
		}
		_, got, err := p.Query(keep)
		if err != nil {
			t.Fatal(err)
		}
		_, want, err := p.Query(copied)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d groups, %d copied", keep, len(got), len(want))
		}
		for i := range got {
			if g, w := fmt.Sprint(typedRows([][]Value{got[i]})), fmt.Sprint(typedRows([][]Value{want[i][:2]})); g != w {
				t.Errorf("%s group %d: %s, copied %s", keep, i, g, w)
			}
		}
	}
}
