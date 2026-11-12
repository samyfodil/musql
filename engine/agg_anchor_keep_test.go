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

// TestGroupByJoinKeysFromNarrowAnchor: a GROUP BY over a join whose keys come
// from different tables keeps only those columns of each group's anchor, and
// reads them back at their columns, as the whole copied row answers -- keys
// spelled differently but equal included.
func TestGroupByJoinKeysFromNarrowAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "join.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE a(id INTEGER, p1, p2, p3, k TEXT COLLATE NOCASE, q REAL)`,
		`CREATE TABLE b(id INTEGER, r1, r2, r3, r4, r5, y)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 40 {
		k := []string{`'a'`, `'A'`, `'b'`, `'B'`, `'c'`}[i%5]
		y := []string{`1`, `1.0`, `2`, `'2'`}[i%4]
		if err := db.Exec(fmt.Sprintf(`INSERT INTO a VALUES(%d, %d, %d, %d, %s, %d.25)`, i%7, i, i, i, k, i)); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(fmt.Sprintf(`INSERT INTO b VALUES(%d, %d, %d, %d, %d, %d, %s)`, i%7, i, i, i, i, i, y)); err != nil {
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
	keep := `SELECT a.k, b.y, sum(a.q) AS s, count(*) AS n FROM a JOIN b ON b.id = a.id GROUP BY a.k, b.y ORDER BY s, n`
	copied := `SELECT a.k, b.y, sum(a.q) AS s, count(*) AS n, b.r5 FROM a JOIN b ON b.id = a.id GROUP BY a.k, b.y ORDER BY s, n`
	if pl := aggPlanOf(t, p, keep); pl.rowCols == nil || len(pl.rowCols) != 2 {
		t.Fatalf("%s: rowCols %v, want the two keys", keep, pl.rowCols)
	}
	_, got, err := p.Query(keep)
	if err != nil {
		t.Fatal(err)
	}
	_, want, err := p.Query(copied)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("%d groups, %d copied", len(got), len(want))
	}
	for i := range got {
		if g, w := fmt.Sprint(typedRows([][]Value{got[i]})), fmt.Sprint(typedRows([][]Value{want[i][:4]})); g != w {
			t.Errorf("group %d: %s, copied %s", i, g, w)
		}
	}
}
