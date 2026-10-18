package engine

import (
	"testing"
)

// segRecFixtureRows is three rows past the fixture's largest rowid (60), as a
// segment of table t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT): the IPK column
// stored NULL, the rowid carrying it.
func segRecFixtureSegment(t *testing.T) []byte {
	t.Helper()
	cols := []columnInfo{{Name: "id", DeclType: "INTEGER"}, {Name: "k", DeclType: "INTEGER"}, {Name: "s", DeclType: "TEXT"}}
	rowids := []uint64{100, 101, 102}
	rows := [][]Value{
		{{Typ: Null}, {Typ: Int, I: 7}, {Typ: Text, S: []byte("a")}},
		{{Typ: Null}, {Typ: Int, I: 8}, {Typ: Text, S: []byte("bb")}},
		{{Typ: Null}, {Typ: Null}, {Typ: Text, S: []byte("ccc")}},
	}
	raw, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// appendFixtureBatch appends recs to segPath's delta as one commit.
func appendFixtureBatch(t *testing.T, segPath string, recs []SegDeltaRecord) {
	t.Helper()
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendSegmentDeltaAt(segPath, baseCtr, basePages, recs, baseCtr+1, basePages, nil, syncFull); err != nil {
		t.Fatal(err)
	}
}

// TestSegmentDeltaSegmentRecord: a batch carrying a segment record beside
// ordinary puts and kills replays to the rows the same changes made through
// SQL give, and a table changed only by a segment record stays clean -- its
// segment is one more of the table's, readable by the columnar fast paths.
func TestSegmentDeltaSegmentRecord(t *testing.T) {
	want := renderT(t, noDeltaFixture(t, 6,
		`DELETE FROM t WHERE id = 20`,
		`UPDATE t SET k = 99 WHERE id = 30`,
		`INSERT INTO t VALUES(100,7,'a'),(101,8,'bb'),(102,NULL,'ccc')`))

	segPath := noDeltaFixture(t, 6)
	appendFixtureBatch(t, segPath, []SegDeltaRecord{
		{Table: 0, Rowid: 20, Kill: true},
		{Table: 0, Rowid: 30, Vals: []Value{{Typ: Null}, {Typ: Int, I: 99}, {Typ: Text, S: []byte("r3")}}},
		{Table: 0, Rowid: 100, Seg: segRecFixtureSegment(t)},
	})
	if got := renderT(t, segPath); got != want {
		t.Errorf("segment record beside puts and kills\n want:\n%s\n got:\n%s", want, got)
	}

	only := noDeltaFixture(t, 6)
	appendFixtureBatch(t, only, []SegDeltaRecord{{Table: 0, Rowid: 100, Seg: segRecFixtureSegment(t)}})
	if segTableIsDirty(t, only) {
		t.Error("a table changed only by a segment record reads as dirty")
	}
	wantOnly := renderT(t, noDeltaFixture(t, 6, `INSERT INTO t VALUES(100,7,'a'),(101,8,'bb'),(102,NULL,'ccc')`))
	if got := renderT(t, only); got != wantOnly {
		t.Errorf("segment record alone\n want:\n%s\n got:\n%s", wantOnly, got)
	}
	// A later commit through SQL appends after it, and both survive.
	execDB(t, only, `INSERT INTO t VALUES(200,1,'z')`, `UPDATE t SET k = 5 WHERE id = 101`)
	wantMore := renderT(t, noDeltaFixture(t, 6,
		`INSERT INTO t VALUES(100,7,'a'),(101,5,'bb'),(102,NULL,'ccc'),(200,1,'z')`))
	if got := renderT(t, only); got != wantMore {
		t.Errorf("a commit after a segment record\n want:\n%s\n got:\n%s", wantMore, got)
	}
}
