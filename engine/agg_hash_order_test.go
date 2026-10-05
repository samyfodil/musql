package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// A GROUP BY whose ORDER BY asks for exactly the order the hash drain already
// emits must NOT build a sorter -- and one that asks for any other order must.
//
// Both directions are asserted. The first is the optimization; the second is
// what keeps it honest, because the cheap way to "pass" the first is to take
// the hash path for every ORDER BY, which would silently emit ascending group
// order for a query that asked for descending.
func TestGroupByOrderKeyUsesHashDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t(k INTEGER, j INTEGER, v INTEGER)`)
	for i := 0; i < 60; i++ {
		db.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,%d)`, i%5, i%3, i))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()

	for _, tc := range []struct {
		sql      string
		wantHash bool
	}{
		{`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`, true},
		{`SELECT k, j, count(*) FROM t GROUP BY k, j ORDER BY k, j`, true},
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k LIMIT 2`, true},
		// Not the hash drain's order:
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY k DESC`, false},
		{`SELECT k, j, count(*) FROM t GROUP BY k, j ORDER BY j, k`, false},
		// A PREFIX of the keys leaves the rest unordered, so it is not the same
		// order and must not be claimed.
		{`SELECT k, j, count(*) FROM t GROUP BY k, j ORDER BY k`, false},
		// An expression that is not a group key at all.
		{`SELECT k, count(*) FROM t GROUP BY k ORDER BY count(*)`, false},
	} {
		stmt, perr := ParseSelect(tc.sql)
		if perr != nil {
			t.Fatalf("%s: %v", tc.sql, perr)
		}
		prog, cerr := compileSelectScan(rp, stmt, nil)
		if cerr != nil {
			t.Fatalf("%s: %v", tc.sql, cerr)
		}
		hash := false
		for _, in := range prog.Insns {
			if in.Op == OpHashAggData {
				hash = true
			}
		}
		if hash != tc.wantHash {
			t.Errorf("%s: hash drain=%v, want %v", tc.sql, hash, tc.wantHash)
		}
	}
}

// ...and the answers must be identical either way. The order is the whole
// claim, so comparing a rendered, order-SENSITIVE string is the point: a hash
// drain that grouped correctly but emitted groups in a different sequence would
// pass any order-insensitive check.
func TestGroupByOrderKeyAnswersIdentically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h2.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t(k INTEGER, j INTEGER, v INTEGER)`)
	for i := 0; i < 400; i++ {
		n := fmt.Sprintf("%d", (i*7)%11)
		if i%23 == 0 {
			n = "NULL"
		}
		db.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%s,%d,%d)`, n, i%4, i-200))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rp, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()

	for _, pair := range [][2]string{
		// The hash-drained query, and the same answer forced through the
		// sorter by asking for the reverse and reversing it back is not
		// expressible in SQL here -- so instead each ascending query is
		// compared against its own DESC twin, which takes the sorter path and
		// whose rows must be the exact reverse.
		{`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`,
			`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k DESC`},
		{`SELECT k, j, count(*) FROM t GROUP BY k, j ORDER BY k, j`,
			`SELECT k, j, count(*) FROM t GROUP BY k, j ORDER BY k DESC, j DESC`},
	} {
		asc := renderQuery(t, rp, pair[0])
		desc := renderQuery(t, rp, pair[1])
		if len(asc) != len(desc) {
			t.Fatalf("%s: %d rows, reversed twin %d", pair[0], len(asc), len(desc))
		}
		for i := range asc {
			if asc[i] != desc[len(desc)-1-i] {
				t.Errorf("%s row %d: hash drain %q, sorter (reversed) %q",
					pair[0], i, asc[i], desc[len(desc)-1-i])
			}
		}
	}
}
