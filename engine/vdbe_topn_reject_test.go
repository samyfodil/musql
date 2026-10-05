package engine

// ORDER BY LIMIT row rejection verified against the exact insert decision.

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// TestSorterLosesMatchesInsert verifies the loses() decision matches insert.
func TestSorterLosesMatchesInsert(t *testing.T) {
	for _, desc := range []bool{false, true} {
		for _, nulls := range []NullsOrder{NullsDefault, NullsFirst, NullsLast} {
			for _, bound := range []int{1, 2, 3, 8} {
				for _, card := range []int{1, 2, 5} {
					for seed := int64(0); seed < 8; seed++ {
						checkLosesMatchesInsert(t, desc, nulls, bound, card, seed)
					}
				}
			}
		}
	}
}

func checkLosesMatchesInsert(t *testing.T, desc bool, nulls NullsOrder, bound, card int, seed int64) {
	t.Helper()
	ki := &sorterKeyInfo{nKey: 1, desc: []bool{desc}, nulls: []NullsOrder{nulls}, coll: []string{"BINARY"}, bound: bound}
	s := newSorter(ki, UTF8)
	rng := rand.New(rand.NewSource(seed))
	for row := 0; row < 60; row++ {
		var k Value
		switch n := rng.Intn(card + 1); {
		case n == card:
			k = Value{Typ: Null}
		default:
			k = Value{Typ: Int, I: int64(n)}
		}
		rec := []Value{k, {Typ: Int, I: int64(row)}}

		predicted := s.loses([]Value{k})
		before := append([]sorterEntry(nil), s.entries...)
		s.insert(rec)
		kept := !sameEntries(before, s.entries)

		if predicted == kept {
			t.Fatalf("desc=%v nulls=%v bound=%d card=%d seed=%d row=%d key=%v: loses said %v but insert %s the entry",
				desc, nulls, bound, card, seed, row, k, predicted,
				map[bool]string{true: "KEPT", false: "discarded"}[kept])
		}
	}
}

// sameEntries reports whether two entry slices hold exactly the same entries in
// the same slots -- the cheapest way to ask "did insert change anything", since
// insert either appends, replaces the root, or does nothing at all.
func sameEntries(a, b []sorterEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].seq != b[i].seq {
			return false
		}
	}
	return true
}

// TestOrderByLimitRejectMatchesUnbounded is the end-to-end half: the bounded
// program (which now skips a losing row's OpMakeRecord entirely) must return
// byte-identical rows to the same ORDER BY with no LIMIT at all, sliced by
// hand. It differs from TestVDBETopNMatchesFullSort in the shapes it aims at:
// an ORDER BY key that is NOT in the select list (so the skipped record's
// payload and key are different columns), a DESC key, a NULL-heavy key, and a
// SELECT DISTINCT whose duplicate probe must still run for every row -- if
// OpSorterCheck were hoisted above OpDistinct, a losing row would never be
// remembered and a later duplicate would reach the sorter with a different key.
func TestOrderByLimitRejectMatchesUnbounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topn.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, c TEXT)`); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 1; i <= 300; i++ {
		b := Value{Typ: Null}
		if i%5 != 0 {
			b = Value{Typ: Int, I: int64(rng.Intn(6))}
		}
		if _, _, err := db.ExecArgs(`INSERT INTO t VALUES(?,?,?,?)`, []Value{
			{Typ: Int, I: int64(i)},
			{Typ: Int, I: int64(rng.Intn(4))},
			b,
			{Typ: Text, S: []byte(fmt.Sprintf("s%d", rng.Intn(3)))},
		}); err != nil {
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

	cases := []struct{ sel, order string }{
		{"id, a", "ORDER BY b DESC"},                 // key not in the select list, DESC
		{"id, a", "ORDER BY b"},                      // ...ascending, NULLs first
		{"id, a", "ORDER BY b NULLS LAST, a DESC"},   // explicit NULLS placement + second key
		{"a, b", "ORDER BY a"},                       // heavy ties on the leading key
		{"c, a", "ORDER BY c DESC, a"},               // TEXT key
		{"DISTINCT a, b", "ORDER BY a DESC, b DESC"}, // DISTINCT over its own output columns
		// DISTINCT whose ORDER BY key is a column OUTSIDE the select list. This
		// is the shape the check's PLACEMENT (after OpDistinct, never before)
		// exists for: the surviving key for a group of output-duplicates is the
		// FIRST one scanned, so a losing row that skipped the DISTINCT probe
		// would let a later duplicate reach the sorter carrying a different key.
		{"DISTINCT a", "ORDER BY b DESC"},
		{"DISTINCT a", "ORDER BY b"},
		{"DISTINCT c", "ORDER BY b DESC, id"},
	}
	for _, tc := range cases {
		ref := fmt.Sprintf("SELECT %s FROM t %s", tc.sel, tc.order)
		_, want, err := p.QueryArgs(ref, nil)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		for _, lim := range []int{1, 2, 7, 20, 299, 300, 500} {
			for _, off := range []int{0, 1, 13, 250} {
				q := fmt.Sprintf("%s LIMIT %d OFFSET %d", ref, lim, off)
				_, got, err := p.QueryArgs(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				lo := min(off, len(want))
				hi := min(lo+lim, len(want))
				exp := want[lo:hi]
				if len(got) != len(exp) {
					t.Fatalf("%s: got %d rows, want %d", q, len(got), len(exp))
				}
				for r := range exp {
					for c := range exp[r] {
						if !valuesIdentical(got[r][c], exp[r][c]) {
							t.Fatalf("%s: row %d col %d: got %v, want %v", q, r, c, got[r][c], exp[r][c])
						}
					}
				}
			}
		}
	}
}
