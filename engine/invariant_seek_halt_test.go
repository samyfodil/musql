package engine

import (
	"fmt"
	"testing"
)

// TestInvariantSeekHalts: the empty branch of a join level sought by a key no
// outer row decides leaves the whole join, past the outermost loop, and a key
// an outer row decides, or a function's, does not.
func TestInvariantSeekHalts(t *testing.T) {
	db := memdbTestDB(t)
	for _, q := range []string{
		`CREATE TABLE cd(x INTEGER)`, `CREATE TABLE hd(y INTEGER)`, `CREATE TABLE dd(k INTEGER, yr INTEGER)`,
		`INSERT INTO cd VALUES(1),(2)`, `INSERT INTO hd VALUES(3),(4)`, `INSERT INTO dd VALUES(1, 1991),(2, 1992)`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// halts reports whether the Rewind after q's sorted seek, when it finds
	// nothing, leaves for an address past every loop's Next.
	halts := func(q string) bool {
		t.Helper()
		stmt, err := ParseSelect(q)
		if err != nil {
			t.Fatal(err)
		}
		prog, err := compileSelectScan(p, stmt, nil)
		if err != nil {
			t.Fatal(err)
		}
		in := prog.Insns
		lastNext := -1
		for pc, x := range in {
			if x.Op == OpNext {
				lastNext = pc
			}
		}
		for pc, x := range in {
			if x.Op != OpSortedSeek {
				continue
			}
			for r := pc + 1; r < len(in); r++ {
				if in[r].Op == OpRewind && in[r].P1 == x.P1 {
					tgt := in[r].P2
					return tgt < len(in) && in[tgt].Op == OpGoto && in[tgt].P2 > lastNext
				}
			}
		}
		t.Fatalf("%s: no sorted seek", q)
		return false
	}
	for _, q := range []string{
		`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = 2001`,
		`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = ?`,
		`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = -(1990 + 11)`,
	} {
		if !halts(q) {
			t.Errorf("%s: an empty seek does not end the join", q)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = hd.y`,
		`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = abs(random())`,
	} {
		if halts(q) {
			t.Errorf("%s: a key an outer row or a function decides ends the join", q)
		}
	}
	// And the answers, with the key bound: 0 for a year no row has, the
	// cross product for one a row has.
	for _, tc := range []struct {
		yr   int64
		want string
	}{{2001, "[1:0|]"}, {1991, "[1:4|]"}} {
		_, rows, err := p.QueryArgs(`SELECT count(*) FROM cd, hd, dd WHERE dd.yr = ?`, []Value{{Typ: Int, I: tc.yr}})
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(typedRows(rows)); got != tc.want {
			t.Errorf("yr = %d: %s, want %s", tc.yr, got, tc.want)
		}
	}
}
