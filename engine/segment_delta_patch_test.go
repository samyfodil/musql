package engine

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSegDeltaPatchRoundTrip: a put with a patch mask is written by a
// version-5 delta as a patch record (segDeltaPatch) and replays, over the
// committed row the replay is given, to the row the put carried; a version-4
// delta writes the whole row instead. A patch lands on the log's own row
// when the log holds one, and only the columns it names change.
func TestSegDeltaPatchRoundTrip(t *testing.T) {
	committed := map[int64][]Value{}
	for r := int64(1); r <= 40; r++ {
		committed[r] = []Value{{Typ: Null}, {Typ: Int, I: r}, {Typ: Text, S: []byte(fmt.Sprintf("c%d", r))}, {Typ: Float, F: float64(r) / 2}}
	}
	base := func(ti int, rowid int64) ([]Value, bool) {
		if ti != 0 {
			return nil, false
		}
		v, ok := committed[rowid]
		return v, ok
	}
	set := []bool{false, true, false, true} // columns 1 and 3
	want := map[int64][]Value{}
	var recs []SegDeltaRecord
	for r := int64(1); r <= 40; r += 3 {
		row := append([]Value(nil), committed[r]...)
		row[1] = Value{Typ: Int, I: r * 1000}
		row[3] = Value{Typ: Text, S: []byte("p")}
		recs = append(recs, SegDeltaRecord{Table: 0, Rowid: r, Vals: row, patch: set})
		want[r] = row
	}
	// A row the log puts whole and then patches: the patch lands on that put.
	put := []Value{{Typ: Null}, {Typ: Int, I: -1}, {Typ: Text, S: []byte("put")}, {Typ: Int, I: 9}}
	patched := append([]Value(nil), put...)
	patched[1] = Value{Typ: Int, I: -2}
	recs = append(recs, SegDeltaRecord{Table: 0, Rowid: 2, Vals: put}, SegDeltaRecord{Table: 0, Rowid: 2, Vals: patched, patch: set})
	want[2] = patched

	for _, version := range []uint32{4, 5} {
		path := filepath.Join(t.TempDir(), "d.delta")
		hdr := segDeltaHeaderV(version, 5, 6)
		if err := os.WriteFile(path, hdr, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = writeSegDeltaBatch(f, segDeltaHdrSize, segDeltaChecksum(version, hdr[:24], 0), version, recs, 9, 10, nil, nil)
		f.Close()
		if err != nil {
			t.Fatalf("v%d: %v", version, err)
		}
		data, _ := os.ReadFile(path)
		patches := 0
		counting := func(ti int, rowid int64) ([]Value, bool) {
			patches++
			return base(ti, rowid)
		}
		st, err := replaySegDelta(data, 5, 6, counting)
		if err != nil {
			t.Fatalf("v%d: %v", version, err)
		}
		if wantPatches := map[uint32]int{4: 0, 5: len(want) - 1}[version]; patches != wantPatches {
			t.Errorf("v%d: %d patches read the committed row, want %d", version, patches, wantPatches)
		}
		for r, w := range want {
			if got := st.live[0][r]; !reflect.DeepEqual(normalizeForCompare(got), normalizeForCompare(w)) {
				t.Errorf("v%d: row %d = %v, want %v", version, r, got, w)
			}
		}
		// Only where the log ends is wanted: the patches are skipped, not refused.
		if _, err := replaySegDelta(data, 5, 6, nil); err != nil {
			t.Errorf("v%d: a replay for the log's end only: %v", version, err)
		}
	}
}

// segDeltaHeaderV is segDeltaHeader for a delta of version v.
func segDeltaHeaderV(v uint32, baseCtr, basePages uint32) []byte {
	hdr := segDeltaHeader(baseCtr, basePages)
	hdr[4], hdr[5], hdr[6], hdr[7] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	sum := segDeltaChecksum(v, hdr[:24], 0)
	for i := range 8 {
		hdr[24+i] = byte(sum >> (8 * i))
	}
	return hdr
}

// normalizeForCompare makes a row comparable whatever spare capacity or nil
// versus empty its byte slices carry.
func normalizeForCompare(row []Value) []string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = fmt.Sprintf("%d:%d:%v:%q", v.Typ, v.I, v.F, v.S)
	}
	return out
}

// TestUpdatePatchesReplay: a gathered UPDATE of committed rows, written in
// place, commits patch records, and every reader of the file -- a fresh
// open, which replays them over the segment file, and the file a compaction
// writes from them -- sees the rows the writer does. The table's rows are in
// the file's segments and in a segment a load appended to the delta.
func TestUpdatePatchesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.musq")
	n, err := OpenOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	exec := func(sql string, args ...Value) {
		t.Helper()
		if _, _, err := n.ExecArgs(sql, args); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if _, err := n.CommitIfAutocommit(); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w TEXT)`)
	exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 3000) INSERT INTO t SELECT i, i % 10, i, 'w' || i FROM c`)
	if err := n.rewriteFile(); err != nil { // every row in the file's segments
		t.Fatal(err)
	}
	if err := n.BeginWriteTxn(false); err != nil {
		t.Fatal(err)
	}
	for i := 3001; i <= 6000; i++ { // a load past the last rowid: a segment in the delta
		if _, _, err := n.ExecArgs(`INSERT INTO t VALUES(?, ?, ?, ?)`, []Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i % 10)}, {Typ: Int, I: int64(i)}, {Typ: Text, S: []byte("x")}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	// Native code gathers the first; the second's concatenation leaves it to
	// the VDBE's own OP_UpdateRow. Both write the row in place.
	for _, k := range []int64{3, 4, 3} {
		exec(`UPDATE t SET v = v * 2 + ? WHERE k = ?`, Value{Typ: Int, I: 7}, Value{Typ: Int, I: k})
		exec(`UPDATE t SET w = w || 'u' WHERE k = ?`, Value{Typ: Int, I: k + 2})
	}
	const q = `SELECT id, k, v, w FROM t ORDER BY id`
	_, want, err := n.Query(q, nil)
	if err != nil {
		t.Fatal(err)
	}

	patches := 0
	m, data, err := mapSegDelta(segDeltaPath(path))
	if err != nil {
		t.Fatal(err)
	}
	f, err := OpenSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	commit := f.committedRows()
	if _, err := replaySegDelta(data, f.srcChangeCounter, f.srcPageCount, func(ti int, rowid int64) ([]Value, bool) {
		patches++
		return commit(ti, rowid)
	}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if m != nil {
		m.Close()
	}
	// Every row an UPDATE reads from the file's segments goes in place: 300
	// per statement among the first 3,000 rows, not just its first row (which
	// is all that did while the cursor lost track of its own writes).
	if patches < 1000 {
		t.Fatalf("the UPDATEs wrote %d patch records, want at least 1,000", patches)
	}

	check := func(label string) {
		t.Helper()
		p, err := Open(path)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer p.Close()
		_, got, err := p.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", label, len(got), len(want))
		}
		for i := range want {
			if !reflect.DeepEqual(normalizeForCompare(got[i]), normalizeForCompare(want[i])) {
				t.Fatalf("%s: row %d = %v, want %v", label, i, got[i], want[i])
			}
		}
	}
	check("a fresh open")
	if err := n.rewriteFile(); err != nil {
		t.Fatal(err)
	}
	check("after a compaction")
}

// TestSegDeltaBatchBoundCovers: segDeltaBatchBound, which settles a
// compaction check without measuring records, is never below the bytes a
// batch actually takes, patch records included.
func TestSegDeltaBatchBoundCovers(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for trial := 0; trial < 200; trial++ {
		var recs []SegDeltaRecord
		for i := 0; i < 1+rng.Intn(40); i++ {
			r := SegDeltaRecord{Table: rng.Intn(3), Rowid: rng.Int63() - rng.Int63(), Kill: rng.Intn(5) == 0}
			if !r.Kill {
				r.Vals = make([]Value, 1+rng.Intn(6))
				for c := range r.Vals {
					switch rng.Intn(4) {
					case 0:
						r.Vals[c] = Value{Typ: Int, I: rng.Int63() - rng.Int63()}
					case 1:
						r.Vals[c] = Value{Typ: Float, F: rng.Float64()}
					case 2:
						r.Vals[c] = Value{Typ: Text, S: make([]byte, rng.Intn(300))}
					}
				}
				if rng.Intn(2) == 0 {
					r.patch = make([]bool, len(r.Vals))
					for c := range r.patch {
						r.patch[c] = rng.Intn(2) == 0
					}
					r.patch[rng.Intn(len(r.patch))] = true
				}
			}
			recs = append(recs, r)
		}
		bound, ok := segDeltaBatchBound(recs)
		if !ok {
			t.Fatal("no bound for plain records")
		}
		for _, version := range []uint32{1, 4, 5} {
			f, err := os.Create(filepath.Join(t.TempDir(), "d"))
			if err != nil {
				t.Fatal(err)
			}
			end, _, err := writeSegDeltaBatch(f, 0, 0, version, recs, 1, 1, nil, nil)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if end > bound {
				t.Fatalf("trial %d v%d: batch takes %d bytes, bound says at most %d", trial, version, end, bound)
			}
		}
	}
}
