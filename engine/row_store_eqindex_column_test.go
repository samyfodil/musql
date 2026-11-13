package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// TestRowStoreEqIndexReadsOneColumn: a segment-backed store's posting list,
// built from its column alone (allFor), names the rows its full rows do: a
// column ADD COLUMN gave a default, a log over the segments (updates, deletes,
// inserts) and the store's own writes.
func TestRowStoreEqIndexReadsOneColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`}
	for i := 1; i <= 900; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, 's%d')`, i, i%9, i%4))
	}
	stmts = append(stmts, `VACUUM`, `ALTER TABLE t ADD COLUMN c INTEGER DEFAULT 5`)
	for _, q := range stmts {
		if err := n.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	for _, logged := range []bool{false, true} {
		if logged {
			n, err = OpenWrite(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{`UPDATE t SET v = 100, c = 7 WHERE id % 7 = 0`, `DELETE FROM t WHERE id % 11 = 0`, `INSERT INTO t VALUES(1000, 1, 's2', 5)`} {
				if err := n.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
		}
		n, err = OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		db := n.DB
		tbl := db.findTableMetaIn(scopeMain, "t")
		if err := db.ensureTableLoaded(tbl); err != nil {
			t.Fatal(err)
		}
		rs := tbl.rows
		if rs.seg == nil {
			t.Fatal("the table did not get the segment-backed store")
		}
		rs.put(5, []Value{{Typ: Null}, {Typ: Int, I: 3}, {Typ: Text, S: []byte("s9")}, {Typ: Int, I: 5}})
		rs.drop(6)
		for col := 1; col <= 3; col++ {
			want := map[string][]uint64{}
			for rid, vals := range rs.all() {
				k := fmt.Sprint(vals[col].Typ, vals[col].I, string(vals[col].S))
				want[k] = append(want[k], rid)
			}
			idx, ok := rs.eqIndexFor(col)
			if !ok {
				t.Fatalf("logged=%v col %d: no index", logged, col)
			}
			got := map[string][]uint64{}
			for v, rids := range idx.rows {
				got[fmt.Sprint(Int, v, "")] = append(slices.Clone(rids), idx.short...)
			}
			for v, rids := range idx.bytes {
				got[fmt.Sprint(Text, int64(0), v)] = append(slices.Clone(rids), idx.short...)
			}
			for k := range want {
				slices.Sort(want[k])
				slices.Sort(got[k])
				if fmt.Sprint(want[k]) != fmt.Sprint(got[k]) {
					t.Fatalf("logged=%v col %d key %s:\n got %v\nwant %v", logged, col, k, got[k], want[k])
				}
			}
		}
		n.Discard()
	}
}
