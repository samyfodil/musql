package engine

// This file tests fts5_decode.go round-trip correctness and robustness against
// adversarial bytes.

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

func init() { RegisterFTS5() }

func newFts5DecodeTestDB(t *testing.T) *Session {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "fts5decode.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return db
}

// fts5FakeDataTable builds a %_data tableMeta from an encoded index image.
func fts5FakeDataTable(data map[int64][]byte) *tableMeta {
	t := &tableMeta{rows: newRowStore(map[uint64][]Value{}, nil)}
	for id, blk := range data {
		t.rows.put(uint64(id), []Value{{Typ: Null}, {Typ: Blob, S: append([]byte(nil), blk...)}})
	}
	return t
}

// fts5wantPostings computes the expected postings map from index documents.
func fts5wantPostings(docs []fts5IndexDoc) map[fts5mergeKey][]int64 {
	want := map[fts5mergeKey][]int64{}
	for _, d := range docs {
		for col, toks := range d.cols {
			for pos, tok := range toks {
				// The decoder keys on the ON-DISK term, leading TERM-SPACE
				// byte included, so that a prefix= index's terms stay distinct
				// from the main index's (fts5IntegrityKeys).
				k := fts5mergeKey{string(rune(fts5MainPrefix)) + tok, d.rowid}
				want[k] = append(want[k], int64(col)<<32|int64(pos))
			}
		}
	}
	return want
}

func TestFts5DecodeRoundTrip(t *testing.T) {
	manyRows := func(n, nCol int) []fts5IndexDoc {
		docs := make([]fts5IndexDoc, n)
		for i := 0; i < n; i++ {
			cols := make([][]string, nCol)
			for c := 0; c < nCol; c++ {
				cols[c] = []string{fmt.Sprintf("word%04d", i), "shared", fmt.Sprintf("col%d", c)}
			}
			docs[i] = fts5IndexDoc{rowid: int64(i + 1), cols: cols}
		}
		return docs
	}

	cases := []struct {
		name string
		docs []fts5IndexDoc
		nCol int
	}{
		{"single row", []fts5IndexDoc{{rowid: 1, cols: [][]string{{"hello", "world"}}}}, 1},
		{"shared terms across rowids", []fts5IndexDoc{
			{rowid: 1, cols: [][]string{{"x", "y"}}},
			{rowid: 2, cols: [][]string{{"x"}}},
			{rowid: 5, cols: [][]string{{"y", "x"}}},
			{rowid: 100, cols: [][]string{{"x"}}},
		}, 1},
		{"two columns", []fts5IndexDoc{
			{rowid: 1, cols: [][]string{{"hello", "world"}, {"foo", "bar"}}},
			{rowid: 2, cols: [][]string{{"second", "row"}, {"baz"}}},
		}, 2},
		{"negative rowid", []fts5IndexDoc{{rowid: -3, cols: [][]string{{"neg"}, {"x"}}}}, 2},
		{"a term repeated many times in one row (multi-byte position deltas)", []fts5IndexDoc{
			{rowid: 1, cols: [][]string{repeatToks("rep", 200)}},
		}, 1},
		{"multi-page, single term shared by every row", manyRows(3000, 1), 1},
		{"multi-page, two columns", manyRows(1500, 2), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, err := fts5BuildIndex(c.docs, c.nCol, nil, fts5PageSize, fts5DetailFull, nil)
			if err != nil {
				t.Fatalf("fts5BuildIndex: %v", err)
			}
			data := fts5FakeDataTable(img.data)
			got, err := fts5DecodeAllPostings(data, "t", fts5DetailFull)
			if err != nil {
				t.Fatalf("fts5DecodeAllPostings: %v", err)
			}
			want := fts5wantPostings(c.docs)
			if !fts5postingsEqual(got, want) {
				t.Errorf("decoded postings do not match fts5BuildIndex's own input\n got=%v\nwant=%v", got, want)
			}
		})
	}
}

// repeatToks returns a column whose single term "tok" occurs n times, so its
// poslist needs multi-byte position-delta varints once n is large enough
// (the "over 63 apart" case fts5EncodePoslist's bias never gets tested by a
// short document).
func repeatToks(tok string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = tok
	}
	return out
}

// TestFts5DecodeAllPostingsNeverPanics mutates a real, valid multi-page,
// multi-segment-shaped index image (one truncated/extended/byte-flipped/
// deleted block per iteration, sometimes several) and decodes it repeatedly.
// The only two acceptable outcomes are a correct decode or a clean error;
// a panic fails the test immediately via recover.
func TestFts5DecodeAllPostingsNeverPanics(t *testing.T) {
	var docs []fts5IndexDoc
	for i := 1; i <= 500; i++ {
		docs = append(docs, fts5IndexDoc{rowid: int64(i), cols: [][]string{{fmt.Sprintf("word%04d", i), "shared", "qqq"}}})
	}
	img, err := fts5BuildIndex(docs, 1, nil, fts5PageSize, fts5DetailFull, nil)
	if err != nil {
		t.Fatalf("fts5BuildIndex: %v", err)
	}
	ids := make([]int64, 0, len(img.data))
	for id := range img.data {
		ids = append(ids, id)
	}

	rng := rand.New(rand.NewSource(1))
	const iterations = 20000
	for iter := 0; iter < iterations; iter++ {
		data := fts5FakeDataTable(img.data)
		nMutate := 1 + rng.Intn(3)
		for m := 0; m < nMutate; m++ {
			id := ids[rng.Intn(len(ids))]
			rec, ok := data.rows.get(uint64(id))
			if !ok {
				continue
			}
			switch rng.Intn(4) {
			case 0: // delete the row entirely
				data.rows.drop(uint64(id))
			case 1: // truncate
				if blk := rec[1].S; len(blk) > 0 {
					rec[1].S = append([]byte(nil), blk[:rng.Intn(len(blk))]...)
				}
			case 2: // extend with garbage
				extra := make([]byte, rng.Intn(20))
				rng.Read(extra)
				rec[1].S = append(append([]byte(nil), rec[1].S...), extra...)
			case 3: // flip random bytes in place
				blk := append([]byte(nil), rec[1].S...)
				for k := 0; k < 1+rng.Intn(5) && len(blk) > 0; k++ {
					blk[rng.Intn(len(blk))] = byte(rng.Intn(256))
				}
				rec[1].S = blk
			}
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d: fts5DecodeAllPostings panicked: %v", iter, r)
				}
			}()
			fts5DecodeAllPostings(data, "t", fts5DetailFull)
		}()
	}
}

// TestFts5DecodeLeafPageNeverPanics throws purely random bytes of random
// lengths at fts5decodeLeafPage directly -- the lowest-level, highest-risk
// function (varint-driven offsets, prefix-compressed terms, poslist
// continuation state) -- both from a fresh page state and from one that
// claims a poslist is already in flight, since that is the state a
// corrupted PRECEDING page could leave behind.
func TestFts5DecodeLeafPageNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	const iterations = 50000
	for iter := 0; iter < iterations; iter++ {
		n := rng.Intn(600)
		page := make([]byte, n)
		rng.Read(page)
		for _, open := range []bool{false, true} {
			st := &fts5decPageState{}
			if open {
				st.haveOpenTerm = true
				st.openTerm = "x"
				if rng.Intn(2) == 0 {
					st.owedBytes = 1 + rng.Intn(5)
				}
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("iteration %d (open=%v): fts5decodeLeafPage panicked on a %d-byte page: %v\npage=% x", iter, open, n, r, page)
					}
				}()
				fts5decodeLeafPage(page, st, fts5DetailFull, func(string, int64, []int64) {})
			}()
		}
	}
}

// TestFts5DecodeStructureNeverPanics does the same for the structure record
// decoder in isolation.
func TestFts5DecodeStructureNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	const iterations = 50000
	for iter := 0; iter < iterations; iter++ {
		n := rng.Intn(200)
		blob := make([]byte, n)
		rng.Read(blob)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d: fts5DecodeStructure panicked on a %d-byte blob: %v\nblob=% x", iter, n, r, blob)
				}
			}()
			fts5DecodeStructure(blob)
		}()
	}
}

// TestFts5IntegrityCheckCommand exercises the full command-channel wiring
// (fts5_shadow.go's fts5CommandInsert -> fts5IntegrityCheck) through
// ordinary SQL, the same path a real "INSERT INTO t(t) VALUES
// ('integrity-check')" statement takes -- a healthy table must succeed
// silently, and directly corrupting %_data (via a plain UPDATE, the same
// shape the oracle probes used) must produce the exact verified error text.
func TestFts5IntegrityCheckCommand(t *testing.T) {
	db := newFts5DecodeTestDB(t)
	mustExec := func(sql string) {
		t.Helper()
		if err := db.Exec(sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	mustExec(`CREATE VIRTUAL TABLE t USING fts5(a, b)`)
	mustExec(`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`)
	mustExec(`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`)
	mustExec(`INSERT INTO t(rowid,a,b) VALUES(3,'third row','qux')`)

	if err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("integrity-check on a healthy table: %v", err)
	}

	// This engine writes its whole index as ONE segment, so the first leaf
	// page is always %_data id 137438953473 (fts5_index.go: "the first leaf
	// page of segment 1"). Deleting every page-holding row past id 10
	// reproduces the exact oracle scenario this task's spec quotes.
	mustExec(`DELETE FROM t_data WHERE id > 10`)
	err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`)
	if err == nil {
		t.Fatal("integrity-check accepted a table whose only leaf page was deleted")
	}
	const want = `fts5: corruption found reading blob 137438953473 from table "t"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestFts5DecodeLeafPageExactBoundaryContinuation is a regression test,
// hand-built at the byte level, for the bug a real-SQLite-written file
// found (compat-harness/fts5_integrity_test.go's "deletes scattered through
// a multi-page, multi-segment table"): a term's LAST posting on a page can
// land exactly on szLeaf with nothing left mid-poslist to signal a
// continuation, and the page's own header/footer are indistinguishable
// from "this term is simply done here" in that case -- only the NEXT
// page's h0 field (offset of its first rowid, fts5_index.go's "A leaf
// page") says otherwise. This engine's own single-segment encoder never
// happens to hit that exact alignment in the round-trip tests above (their
// row counts don't land a posting on a 4050-byte boundary), so this
// constructs the two pages directly instead of going through
// fts5BuildIndex.
func TestFts5DecodeLeafPageExactBoundaryContinuation(t *testing.T) {
	// Page 1: header, then the term "y" (nTerm=2, "0y"), then ONE posting
	// (rowid=1 absolute, size=2 i.e. 1 poslist byte, poslist byte 4 -> pos
	// 2) that lands EXACTLY on szLeaf=10, then the footer (one entry: term
	// "y" starts at offset 4).
	page1 := []byte{
		0x00, 0x00, // h0 = 0 (a term precedes anything on this page)
		0x00, 0x0a, // szLeaf = 10
		0x02, 0x30, 0x79, // term record: nTerm=2, "0y"
		0x01, 0x02, 0x04, // posting: rowid=1 (absolute), size=2 (1 poslist byte), poslist byte 4 (pos 2)
		0x04, // footer: term "y" at absolute offset 4
	}
	// Page 2: h0=4 (a rowid, not a term, starts this page), ONE more
	// posting for the SAME still-open term "y" (rowid=2 -- ABSOLUTE, not a
	// delta: fts5_index.go's "the first of its term or the first on its
	// page" applies here via the "first on its page" half even though the
	// term itself isn't new -- size=2, poslist byte 4 -> pos 2), no footer
	// at all (nothing new starts on this page).
	page2 := []byte{
		0x00, 0x04, // h0 = 4: a rowid, not a term, opens this page
		0x00, 0x07, // szLeaf = 7
		0x02, 0x02, 0x04, // posting: rowid=2 (absolute), size=2, poslist byte 4 (pos 2)
	}

	var got []struct {
		term  string
		rowid int64
		pos   []int64
	}
	emit := func(term string, rowid int64, positions []int64) {
		got = append(got, struct {
			term  string
			rowid int64
			pos   []int64
		}{term, rowid, append([]int64(nil), positions...)})
	}

	st := &fts5decPageState{}
	if err := fts5decodeLeafPage(page1, st, fts5DetailFull, emit); err != nil {
		t.Fatalf("page1: %v", err)
	}
	// Page 1's own posting decodes and emits immediately (nothing withholds
	// an already-complete posting) -- what this test is really pinning is
	// the STATE left behind for page 2.
	if len(got) != 1 {
		t.Fatalf("page1 emitted %d postings, want exactly 1: %v", len(got), got)
	}
	if !st.haveOpenTerm || st.openTerm != "0y" || st.owedBytes != 0 {
		t.Fatalf("after page1: haveOpenTerm=%v openTerm=%q owedBytes=%d, want an open term \"y\" with nothing owed", st.haveOpenTerm, st.openTerm, st.owedBytes)
	}

	if err := fts5decodeLeafPage(page2, st, fts5DetailFull, emit); err != nil {
		t.Fatalf("page2: %v", err)
	}
	want := []struct {
		term  string
		rowid int64
		pos   []int64
	}{
		{"0y", 1, []int64{2}},
		{"0y", 2, []int64{2}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d postings, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].term != want[i].term || got[i].rowid != want[i].rowid || len(got[i].pos) != 1 || got[i].pos[0] != want[i].pos[0] {
			t.Errorf("posting %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
