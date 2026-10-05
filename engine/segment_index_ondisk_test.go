package engine

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
)

// TestSegmentDiskIndexSurvivesATypeChange verifies on-disk indexes remain
// correct when indexed columns change type (TEXT, NULL, REAL), and are dropped
// when they're no longer all-integer. The scan provides the oracle.
func TestSegmentDiskIndexSurvivesATypeChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ty.musq")
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	if e := sess.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v)`); e != nil {
		t.Fatal(e)
	}
	// An untyped column -- no affinity, so a value is stored as written, which is
	// the only way to get a genuine mixed-type column.
	for i := 1; i <= 500; i++ {
		if e := sess.Exec(fmt.Sprintf(`INSERT INTO t(id,v) VALUES(%d,%d)`, i, i%50)); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := sess.Commit(); e != nil { // writes the segment, and its index
		t.Fatal(e)
	}
	// Check whether column col carries an on-disk index.
	indexedColumns := func(what string, col int) int {
		p, oerr := Open(path)
		if oerr != nil {
			t.Fatalf("%s: %v", what, oerr)
		}
		defer p.Close()
		n := 0
		for root, segs := range p.segs.byRoot {
			if root == schemaRootPage {
				continue
			}
			for _, sg := range segs {
				if col < len(sg.cols) && sg.cols[col].idxOff != 0 {
					n++
				}
			}
		}
		return n
	}
	if indexedColumns("after the integer load", 1) == 0 {
		t.Fatal("a clean all-integer column got no on-disk index, so this test proves nothing")
	}

	// The oracle, and the question, at every stage.
	check := func(stage string) {
		t.Helper()
		for _, probe := range []int64{0, 7, 49, 50, 999} {
			_, rows, qerr := sess.Query(`SELECT count(*) FROM t WHERE v = ?`, []Value{{Typ: Int, I: probe}})
			if qerr != nil {
				t.Fatalf("%s v=%d: %v", stage, probe, qerr)
			}
			// Count by scanning, independently.
			_, all, aerr := sess.Query(`SELECT v FROM t`, nil)
			if aerr != nil {
				t.Fatalf("%s scan: %v", stage, aerr)
			}
			want := 0
			for _, r := range all {
				if r[0].Typ == Int && r[0].I == probe {
					want++
				}
			}
			if len(rows) != 1 || int(rows[0][0].I) != want {
				t.Errorf("%s: count(v = %d) = %v, the scan says %d", stage, probe, rows, want)
			}
		}
	}
	check("integers only")

	// TEXT value in the delta; index must stay correct for segment rows.
	if e := sess.Exec(`INSERT INTO t(id,v) VALUES(1001,'seven')`); e != nil {
		t.Fatal(e)
	}
	if e := sess.Exec(`UPDATE t SET v = 'x' WHERE id = 7`); e != nil {
		t.Fatal(e)
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	check("a TEXT value in the delta")

	// A NULL, likewise.
	if e := sess.Exec(`UPDATE t SET v = NULL WHERE id = 8`); e != nil {
		t.Fatal(e)
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	check("a NULL in the delta")

	// Force rewrite (DDL change) to fold delta into segments.
	if e := sess.Exec(`CREATE INDEX tv ON t(v)`); e != nil {
		t.Fatal(e)
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	check("after the rewrite folded a mixed column in")
	if n := indexedColumns("after the rewrite", 1); n != 0 {
		t.Errorf("column v holds TEXT and NULL now and still has %d on-disk index(es) -- "+
			"an int64 index over a column that is not all int64 answers the wrong rows", n)
	}

	// REAL value; must not be answered from int64 index (compares equal to int).
	if e := sess.Exec(`UPDATE t SET v = 3.0 WHERE id = 300`); e != nil {
		t.Fatal(e)
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	_, rows, qerr := sess.Query(`SELECT count(*) FROM t WHERE v = ?`, []Value{{Typ: Int, I: 3}})
	if qerr != nil {
		t.Fatal(qerr)
	}
	_, all, _ := sess.Query(`SELECT v FROM t`, nil)
	want := 0
	for _, r := range all {
		if (r[0].Typ == Int && r[0].I == 3) || (r[0].Typ == Float && r[0].F == 3) {
			want++
		}
	}
	if len(rows) != 1 || int(rows[0][0].I) != want {
		t.Errorf("count(v = 3) with a REAL 3.0 present = %v, want %d -- a float equals an integer in SQLite", rows, want)
	}
}

// TestSegDiskIndexAnswersExactlyWhatTheBlockHolds verifies indexes return
// exactly what the block holds, by querying the index directly against a scan.
func TestSegDiskIndexAnswersExactlyWhatTheBlockHolds(t *testing.T) {
	cols := []columnInfo{{Name: "a"}, {Name: "b"}}
	const n = 600
	rowids := make([]uint64, n)
	rows := make([][]Value, n)
	for i := 0; i < n; i++ {
		rowids[i] = uint64(i*3 + 1) // SPACED, so a position is not a rowid
		rows[i] = []Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i % 7)}}
	}
	buf, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(buf)
	if err != nil {
		t.Fatal(err)
	}
	for c := 0; c < 2; c++ {
		if sg.cols[c].idxOff == 0 {
			t.Fatalf("column %d is a clean int64 block and got no index -- this test would prove nothing", c)
		}
		x, ok := sg.diskIndexFor(c)
		if !ok {
			t.Fatalf("column %d has an index block the reader refuses", c)
		}
		// Every value the block holds, and some it does not.
		for _, probe := range []int64{-1, 0, 1, 3, 6, 7, 299, 599, 600, 1 << 40} {
			var want []int32
			for i := 0; i < n; i++ {
				if rows[i][c].I == probe {
					want = append(want, int32(i))
				}
			}
			got := x.rowsFor(probe, nil)
			if len(got) != len(want) {
				t.Fatalf("column %d, v=%d: index gave %d rows, the block holds %d", c, probe, len(got), len(want))
			}
			for k := range want {
				if got[k] != want[k] {
					t.Fatalf("column %d, v=%d: index row %d is %d, the block says %d (order must be ascending)",
						c, probe, k, got[k], want[k])
				}
			}
			cnt, served := x.countFor(probe)
			if !served {
				t.Errorf("column %d, v=%d: countFor declined an int64 index", c, probe)
			} else if cnt != len(want) {
				t.Errorf("column %d, v=%d: count %d, rows %d", c, probe, cnt, len(want))
			}
		}
	}

	// ...and every shape that must NOT get one, because an int64 index over it
	// would answer about values it cannot represent.
	for _, tc := range []struct {
		name string
		val  Value
	}{
		{"a TEXT value", Value{Typ: Text, S: []byte("x")}},
		{"a REAL value", Value{Typ: Float, F: 1.5}},
		{"a NULL", Value{Typ: Null}},
	} {
		mixed := make([][]Value, n)
		for i := range mixed {
			mixed[i] = []Value{{Typ: Int, I: int64(i)}, {Typ: Int, I: int64(i % 7)}}
		}
		mixed[n/2][1] = tc.val
		mbuf, berr := buildSegment(cols, rowids, mixed)
		if berr != nil {
			t.Fatal(berr)
		}
		msg, oerr := openSegment(mbuf)
		if oerr != nil {
			t.Fatal(oerr)
		}
		if _, ok := msg.diskIndexFor(1); ok {
			t.Errorf("%s in the column and it still has a usable int64 index", tc.name)
		}
		// The clean column beside it keeps its own, which is what makes the
		// refusal per COLUMN rather than per segment.
		if _, ok := msg.diskIndexFor(0); !ok {
			t.Errorf("%s in column 1 cost column 0 its index too", tc.name)
		}
	}
}

// A CORRUPT INDEX RECORD MUST DECLINE, never panic and never answer.
//
// This structure decides which rows a query sees, and a file may be anything --
// a review pointed out that the first version validated only the
// final prefix sum, which leaves damaged bytes able to return rows that do not
// hold the value, or to index out of range. Every field is checked now, and this
// walks a valid record corrupting one thing at a time.
func TestSegDiskIndexRefusesACorruptRecord(t *testing.T) {
	cols := []columnInfo{{Name: "a"}}
	const n = 64
	rowids := make([]uint64, n)
	rows := make([][]Value, n)
	for i := 0; i < n; i++ {
		rowids[i] = uint64(i + 1)
		rows[i] = []Value{{Typ: Int, I: int64(i % 8)}}
	}
	good, err := buildSegment(cols, rowids, rows)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := openSegment(good)
	if err != nil {
		t.Fatal(err)
	}
	off := sg.cols[0].idxOff
	if off == 0 {
		t.Fatal("no index was built, so this test proves nothing")
	}
	if _, ok := openSegColumnIndex(good, off, n, sg.heap); !ok {
		t.Fatal("the UNDAMAGED record is refused, so every case below would pass vacuously")
	}

	nVals := int(binary.LittleEndian.Uint32(good[int(off)+4:]))
	valsAt := int(off) + segIdxHdr
	startsAt := valsAt + nVals*8
	postsAt := startsAt + (nVals+1)*4

	for _, tc := range []struct {
		name  string
		break_ func(b []byte)
	}{
		{"a version it does not implement", func(b []byte) { binary.LittleEndian.PutUint16(b[off:], 99) }},
		{"a codec it does not implement", func(b []byte) { binary.LittleEndian.PutUint16(b[off+2:], 99) }},
		{"nVals larger than the row count", func(b []byte) { binary.LittleEndian.PutUint32(b[off+4:], uint32(n+1)) }},
		{"a row count that is not the segment's", func(b []byte) { binary.LittleEndian.PutUint32(b[off+8:], uint32(n-1)) }},
		{"a byte length shorter than its own arrays", func(b []byte) { binary.LittleEndian.PutUint32(b[off+12:], segIdxHdr) }},
		{"a byte length past the end of the buffer", func(b []byte) { binary.LittleEndian.PutUint32(b[off+12:], 1<<30) }},
		{"values out of order", func(b []byte) { binary.LittleEndian.PutUint64(b[valsAt:], 1<<62) }},
		{"two equal values", func(b []byte) {
			binary.LittleEndian.PutUint64(b[valsAt+8:], binary.LittleEndian.Uint64(b[valsAt:]))
		}},
		{"a first prefix sum that is not zero", func(b []byte) { binary.LittleEndian.PutUint32(b[startsAt:], 1) }},
		{"prefix sums that go backwards", func(b []byte) { binary.LittleEndian.PutUint32(b[startsAt+4:], 0) }},
		{"a last prefix sum that is not the row count", func(b []byte) {
			binary.LittleEndian.PutUint32(b[startsAt+nVals*4:], uint32(n-1))
		}},
		{"a prefix sum past the row count", func(b []byte) { binary.LittleEndian.PutUint32(b[startsAt+4:], uint32(n+5)) }},
		{"a row position out of range", func(b []byte) { binary.LittleEndian.PutUint32(b[postsAt:], uint32(n)) }},
	} {
		bad := make([]byte, len(good))
		copy(bad, good)
		tc.break_(bad)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: PANICKED (%v) -- a malformed file must be declined, never panic", tc.name, r)
				}
			}()
			if _, ok := openSegColumnIndex(bad, off, n, sg.heap); ok {
				t.Errorf("%s: accepted", tc.name)
			}
		}()
	}
}
