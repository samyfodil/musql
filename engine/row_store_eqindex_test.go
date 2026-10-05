package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// Tests that equality-index row store mutations are tracked correctly.
// Tested through a real session to exercise the driver path.
func TestRowStoreEqIndexTracksEveryMutation(t *testing.T) {
	sess, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`,
		`CREATE INDEX tv ON t(v)`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	// FOUR THOUSAND ROWS, not forty, and the number is load-bearing: the compiler
	// only looks for an index seek on a table big enough to be worth one, so at
	// forty rows detectIndexSeekKey never even asks for the candidates and this
	// whole test passed with the invalidation deliberately broken. Verified by
	// mutation both ways -- see the note below.
	const n = 4000
	const mod = 7
	want := map[int64][]int64{} // v -> ascending ids, the oracle
	put := func(id, v int64) {
		for k := range want {
			for i, x := range want[k] {
				if x == id {
					want[k] = append(want[k][:i], want[k][i+1:]...)
					break
				}
			}
		}
		want[v] = append(want[v], id)
		slices.Sort(want[v])
	}
	del := func(id int64) {
		for k := range want {
			for i, x := range want[k] {
				if x == id {
					want[k] = append(want[k][:i], want[k][i+1:]...)
					break
				}
			}
		}
	}
	for i := int64(1); i <= n; i++ {
		if e := sess.Exec(fmt.Sprintf(`INSERT INTO t(id,v,s) VALUES(%d,%d,'r%d')`, i, i%mod, i)); e != nil {
			t.Fatal(e)
		}
		put(i, i%mod)
	}
	// A BOUND PARAMETER: the shape a driver connection sends, and the one the seek
	// is reached through.
	check := func(what string) {
		t.Helper()
		for v := int64(0); v < mod+2; v++ {
			_, rows, qerr := sess.Query(`SELECT id FROM t WHERE v = ?`, []Value{{Typ: Int, I: v}})
			if qerr != nil {
				t.Fatalf("%s v=%d: %v", what, v, qerr)
			}
			got := make([]int64, len(rows))
			for i, r := range rows {
				got[i] = r[0].I
			}
			slices.Sort(got)
			exp := want[v]
			if len(got) != len(exp) {
				t.Errorf("%s: v=%d gave %d rows, want %d", what, v, len(got), len(exp))
				continue
			}
			for i := range exp {
				if got[i] != exp[i] {
					t.Errorf("%s: v=%d row %d = %d, want %d", what, v, i, got[i], exp[i])
					break
				}
			}
		}
	}
	check("initial")

	// A seek has now BUILT the index for column v. Every mutation below must be
	// visible to the next check, and each one is a different invalidation site.
	if e := sess.Exec(`UPDATE t SET v = 3 WHERE id IN (1, 2, 4000)`); e != nil {
		t.Fatal(e)
	}
	put(1, 3)
	put(2, 3)
	put(4000, 3)
	check("after an UPDATE across values")

	if e := sess.Exec(`DELETE FROM t WHERE id IN (8, 23, 3999)`); e != nil {
		t.Fatal(e)
	}
	del(8)
	del(23)
	del(3999)
	check("after a DELETE")

	if e := sess.Exec(`INSERT INTO t(id,v,s) VALUES(100000,3,'new')`); e != nil {
		t.Fatal(e)
	}
	put(100000, 3)
	check("after an INSERT")

	// ADD COLUMN makes every existing row SHORT in the new column, and such a row
	// reads as the column DEFAULT rather than NULL -- so it must appear in the
	// answer for that default, which is what rsEqIndex.short exists for.
	if e := sess.Exec(`ALTER TABLE t ADD COLUMN w INTEGER DEFAULT 7`); e != nil {
		t.Fatal(e)
	}
	_, rows, qerr := sess.Query(`SELECT count(*) FROM t WHERE w = ?`, []Value{{Typ: Int, I: 7}})
	if qerr != nil {
		t.Fatal(qerr)
	}
	live := 0
	for _, ids := range want {
		live += len(ids)
	}
	if len(rows) != 1 || int(rows[0][0].I) != live {
		t.Errorf("w = 7 (the ADD COLUMN default) matched %v, want %d -- a short row must not be lost", rows, live)
	}

	if e := sess.Exec(`DELETE FROM t`); e != nil {
		t.Fatal(e)
	}
	want = map[int64][]int64{}
	check("after a DELETE of everything")
}

// The REFUSALS, directly on the store, because each one is a correctness rule
// rather than an optimisation: a wrong answer here is a row that never comes back.
func TestRowStoreEqIndexRefusals(t *testing.T) {
	mk := func(rows map[uint64][]Value) *rowStore {
		return &rowStore{m: rows}
	}
	// TEXT in the column refuses the whole index: a text comparison needs a
	// collation, which an int64 map cannot carry.
	s := mk(map[uint64][]Value{
		1: {{Typ: Int, I: 1}, {Typ: Int, I: 5}},
		2: {{Typ: Int, I: 2}, {Typ: Text, S: []byte("5")}},
	})
	if _, ok := s.eqRowids(1, 5); ok {
		t.Error("a TEXT value in the column must refuse the index, not index the integers around it")
	}
	// FLOAT too: 5.0 must match "= 5", and that rule does not live here.
	s = mk(map[uint64][]Value{
		1: {{Typ: Int, I: 1}, {Typ: Int, I: 5}},
		2: {{Typ: Int, I: 2}, {Typ: Float, F: 5}},
	})
	if _, ok := s.eqRowids(1, 5); ok {
		t.Error("a FLOAT value in the column must refuse the index")
	}
	// NULL is SKIPPED, not a refusal: it equals nothing, so no probe wants it.
	s = mk(map[uint64][]Value{
		1: {{Typ: Int, I: 1}, {Typ: Int, I: 5}},
		2: {{Typ: Int, I: 2}, {Typ: Null}},
		3: {{Typ: Int, I: 3}, {Typ: Int, I: 5}},
	})
	got, ok := s.eqRowids(1, 5)
	if !ok {
		t.Fatal("a NULL must not refuse the index")
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("v=5 gave %v, want [1 3] ascending", got)
	}
	// A SHORT row joins every answer, because it reads as the column DEFAULT.
	s = mk(map[uint64][]Value{
		1: {{Typ: Int, I: 1}, {Typ: Int, I: 5}},
		2: {{Typ: Int, I: 2}}, // no column 1 at all
	})
	got, ok = s.eqRowids(1, 5)
	if !ok {
		t.Fatal("a short row must not refuse the index")
	}
	if len(got) != 2 {
		t.Errorf("v=5 gave %v, want both rows -- the short row reads as the DEFAULT and may match", got)
	}
	got, ok = s.eqRowids(1, 999)
	if !ok || len(got) != 1 || got[0] != 2 {
		t.Errorf("v=999 gave %v (ok=%v), want just the short row 2", got, ok)
	}
}

// ROLLBACK TO must roll the posting lists back with the rows. A snapshot's store
// once shared the live store's lists, which noteWrite edits in place, so after
// "SAVEPOINT s; UPDATE ... SET v = 99 WHERE id = 5; ROLLBACK TO s" the rows said
// v = 5 and the index said 99, and "WHERE v = 5" lost the row. Mutation-tested:
// dropping rowStore.clone's "cp.eqIdx = nil" fails this.
func TestRowStoreEqIndexRollsBackWithRows(t *testing.T) {
	sess, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	exec := func(s string) {
		t.Helper()
		if e := sess.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	ids := func(v int64) []int64 {
		t.Helper()
		_, rows, qerr := sess.Query(`SELECT id FROM t WHERE v = ?`, []Value{{Typ: Int, I: v}})
		if qerr != nil {
			t.Fatal(qerr)
		}
		var got []int64
		for _, r := range rows {
			got = append(got, r[0].I)
		}
		slices.Sort(got)
		return got
	}
	exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`)
	exec(`CREATE INDEX tv ON t(v)`)
	exec(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<4000) INSERT INTO t SELECT x, x FROM c`)
	exec(`BEGIN`)
	exec(`INSERT INTO t VALUES(4001, 4001)`) // dirty: the row store's index serves
	if got := ids(5); !slices.Equal(got, []int64{5}) {
		t.Fatalf("before: v=5 gave %v", got)
	}
	exec(`SAVEPOINT s`)
	exec(`UPDATE t SET v = 99 WHERE id = 5`)
	exec(`ROLLBACK TO s`)
	if got := ids(5); !slices.Equal(got, []int64{5}) {
		t.Errorf("after ROLLBACK TO: v=5 gave %v, want [5]", got)
	}
	if got := ids(99); !slices.Equal(got, []int64{99}) {
		t.Errorf("after ROLLBACK TO: v=99 gave %v, want [99]", got)
	}
	exec(`ROLLBACK`)
}
