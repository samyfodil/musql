package engine

// hashAggStep reuses one buffer for all rows; verify bytes are byte-identical.

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"
)

// Check buffer append and reuse; no residue across rows.
func TestAppendCollatedKeyBytesReuseIsByteIdentical(t *testing.T) {
	matrix := rowKeyValueMatrix()
	const prefix = "\xff stale bytes from the previous row \x00\x01"
	for _, enc := range []TextEncoding{UTF8, UTF16LE, UTF16BE} {
		for _, colls := range [][]string{nil, {"BINARY", "BINARY"}, {"NOCASE", "RTRIM"}, {"RTRIM", "NOCASE"}} {
			var buf []byte // reused across every row below, never reset except by [:0]
			for _, a := range matrix {
				for _, b := range matrix {
					row := []Value{a, b}
					want := collatedKeyBytes(row, colls, enc)

					if got := appendCollatedKeyBytes([]byte(prefix), row, colls, enc); string(got) != prefix+string(want) {
						t.Fatalf("enc=%v colls=%v row=(%v,%v): append onto a non-empty buffer gave %x, want %x",
							enc, colls, a, b, got, prefix+string(want))
					}

					buf = appendCollatedKeyBytes(buf[:0], row, colls, enc)
					if string(buf) != string(want) {
						t.Fatalf("enc=%v colls=%v row=(%v,%v): reused buffer gave %x, fresh gave %x",
							enc, colls, a, b, buf, want)
					}
				}
			}
		}
	}
}

// TestGroupByHashKeyMatchesSortedPath is the end-to-end differential the
// encoding exists for: the SAME grouping, computed once by the hash-aggregate
// fast path (whose bucket identity is the encoded key) and once by the
// sort-based scan+drain (whose group boundaries are keysEqual/compareValues
// over the raw values), must produce the same groups with the same counts.
// A trailing ORDER BY is what forces the second path (compileScanGroupBy's
// hasOrder guard), so the two statements differ only in emission order -- which
// is why the comparison is over a canonicalized multiset of (key, count).
//
// The rows are the near-miss matrix GROUP BY equality actually has to get
// right: NULL groups with NULL, INTEGER 1 groups with REAL 1.0 (and with 1.0's
// negative zero sibling for 0), a BLOB never groups with the TEXT of the same
// bytes, and the empty string / 'a' / 'a ' stay three groups under BINARY.
func TestGroupByHashKeyMatchesSortedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gk.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// No declared column type: the key column keeps each row's own storage
	// class, so one column really does carry all five of them at once.
	if err := db.Exec(`CREATE TABLE g(id INTEGER PRIMARY KEY, k, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	keys := []Value{
		{Typ: Null},
		{Typ: Int, I: 0},
		{Typ: Float, F: -0.0},
		{Typ: Int, I: 1},
		{Typ: Float, F: 1.0},
		{Typ: Float, F: 1.5},
		{Typ: Int, I: 1},
		{Typ: Text, S: []byte("")},
		{Typ: Text, S: []byte("a")},
		{Typ: Text, S: []byte("A")},
		{Typ: Text, S: []byte("a ")},
		{Typ: Blob, S: []byte("a")},
		{Typ: Blob, S: []byte{}},
		{Typ: Text, S: []byte("1")},
		{Typ: Null},
		{Typ: Float, F: 1.0},
	}
	for i, k := range keys {
		if _, _, err := db.ExecArgs(`INSERT INTO g(id,k,v) VALUES(?,?,?)`,
			[]Value{{Typ: Int, I: int64(i + 1)}, k, {Typ: Int, I: int64(i)}}); err != nil {
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

	// canon renders a result set as an order-insensitive multiset of rows, so
	// only the GROUPING (which rows share a group, and each group's key and
	// aggregate) is compared, never the emission order the two paths choose.
	canon := func(rows [][]Value) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			s := ""
			for _, v := range r {
				s += fmt.Sprintf("|%d:%v:%q", v.Typ, v.I, string(v.S))
				if v.Typ == Float {
					s += fmt.Sprintf(":%g", v.F)
				}
			}
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}

	for _, agg := range []string{"count(*)", "sum(v)", "min(v)", "max(v)", "group_concat(v)"} {
		hashSQL := fmt.Sprintf("SELECT k, %s FROM g GROUP BY k", agg)
		sortSQL := hashSQL + " ORDER BY k"
		_, hashRows, err := p.QueryArgs(hashSQL, nil)
		if err != nil {
			t.Fatalf("%s: %v", hashSQL, err)
		}
		_, sortRows, err := p.QueryArgs(sortSQL, nil)
		if err != nil {
			t.Fatalf("%s: %v", sortSQL, err)
		}
		got, want := canon(hashRows), canon(sortRows)
		if len(got) != len(want) {
			t.Fatalf("%s: hash path produced %d groups, sorted path %d\nhash: %v\nsort: %v",
				agg, len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: group %d differs\nhash: %s\nsort: %s", agg, i, got[i], want[i])
			}
		}
	}
}
