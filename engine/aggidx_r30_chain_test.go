package engine

import (
	"path/filepath"
	"testing"
)

// TestR30IndexChainIsReverseCreationOrder verifies index chain ordering.
func TestR30IndexChainIsReverseCreationOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t(a, b, o)",
		"CREATE INDEX first_made ON t(a)",
		"CREATE INDEX second_made ON t(b)",
		"CREATE INDEX third_made ON t(a,b)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(s, err)
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

	tbl, err := p.resolveTable("t")
	if err != nil {
		t.Fatal(err)
	}
	idxs, _, ok := wherePlanIndexList(p, tbl, "t", "", false, nil)
	if !ok {
		t.Fatal("wherePlanIndexList declined a plain three-index table")
	}
	got := make([]string, len(idxs))
	for i := range idxs {
		got[i] = idxs[i].name
	}
	want := []string{"sqlite_rowid", "third_made", "second_made", "first_made"}
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain = %v, want %v (sPk first, then REVERSE creation order)", got, want)
		}
	}
	if !idxs[0].ipk {
		t.Errorf("chain[0] = %q, which is not the fake sPk", got[0])
	}

	// An INDEXED BY hint drops the sPk entirely and leaves exactly the named
	// index -- whereLoopAddBtree's "pProbe = pSrc->u2.pIBIndex" with
	// "pProbe = (pSrc->fg.isIndexedBy ? 0 : pProbe->pNext)".
	only, _, ok := wherePlanIndexList(p, tbl, "t", "second_made", false, nil)
	if !ok || len(only) != 1 || only[0].name != "second_made" || only[0].ipk {
		t.Errorf("INDEXED BY chain = %v (ok=%v), want exactly [second_made] and no sPk", only, ok)
	}

	// NOT INDEXED stops the chain at the sPk: no real index of the table is
	// priced, and none of them has to be reproducible for the answer to be
	// exact.
	ni, _, ok := wherePlanIndexList(p, tbl, "t", "", true, nil)
	if !ok || len(ni) != 1 || !ni[0].ipk {
		t.Errorf("NOT INDEXED chain = %v (ok=%v), want exactly the sPk", ni, ok)
	}
}
