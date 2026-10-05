package engine

// Tests row key set encoding and equality for UNION recursive CTEs.

import (
	"fmt"
	"path/filepath"
	"testing"
)

// rowKeyValueMatrix is the value set both directions below are checked over.
// Deliberately full of near-misses: 1 vs 1.0 (equal under compareNumeric), 'a'
// vs 'A' (equal only under NOCASE), 'a' vs 'a ' (equal only under RTRIM),
// x'61' vs 'a' (a BLOB never equals TEXT), and the two invalid UTF-8 bytes
// 0x80/0x81, which are byte-different but BOTH decode to U+FFFD and therefore
// compare EQUAL in a UTF-16 database.
//
// The last two entries exist to make the encoding's LENGTH PREFIX load-bearing
// in the multi-column check: they spell a TEXT tag followed by a zero length,
// so ("", "\x03<0>a") and ("\x03<0>", "a") concatenate to the same bytes the
// moment the prefix stops carrying the real length. Without them a
// length-prefix mutation goes undetected -- the tag byte alone happens to
// separate every other pair here.
func rowKeyValueMatrix() []Value {
	return []Value{
		{Typ: Null},
		{Typ: Int, I: 0},
		{Typ: Int, I: 1},
		{Typ: Float, F: 1.0},
		{Typ: Float, F: 1.5},
		{Typ: Float, F: -0.0},
		{Typ: Text, S: []byte("a")},
		{Typ: Text, S: []byte("A")},
		{Typ: Text, S: []byte("a ")},
		{Typ: Text, S: []byte("a\t")},
		{Typ: Text, S: []byte("")},
		{Typ: Text, S: []byte("é")},
		{Typ: Text, S: []byte("É")},
		{Typ: Text, S: []byte("\x80")},
		{Typ: Text, S: []byte("\x81")},
		{Typ: Text, S: []byte("\U00010000")},
		{Typ: Text, S: []byte("\U00010001")},
		{Typ: Blob, S: []byte("a")},
		{Typ: Blob, S: []byte{}},
		{Typ: Text, S: []byte("\x03\x00\x00\x00\x00\x00\x00\x00\x00")},
		{Typ: Text, S: []byte("\x03\x00\x00\x00\x00\x00\x00\x00\x00a")},
	}
}

// TestRowKeySetMatchesKeysEqualCollated is the contract gate: for every
// (value, value, collation, encoding) combination, an identical
// collatedKeyBytes encoding and keysEqualCollated equality must agree. A
// one-way check would not do -- a key that merged too much (a wrong ANSWER, a
// dropped row) and one that split too much (a wrong answer the other way, a
// duplicate row) are different failures and both are caught here.
func TestRowKeySetMatchesKeysEqualCollated(t *testing.T) {
	vals := rowKeyValueMatrix()
	for _, coll := range []string{"", "BINARY", "NOCASE", "RTRIM", "nocase"} {
		for _, enc := range []TextEncoding{UTF8, UTF16LE, UTF16BE} {
			colls := []string{coll}
			for i, a := range vals {
				for j, b := range vals {
					ra, rb := []Value{a}, []Value{b}
					equal := keysEqualCollated(ra, rb, colls, enc)
					sameKey := string(collatedKeyBytes(ra, colls, enc)) == string(collatedKeyBytes(rb, colls, enc))
					if equal != sameKey {
						t.Errorf("coll=%q enc=%v vals[%d]=%v vals[%d]=%v: keysEqualCollated=%v but sameKey=%v",
							coll, enc, i, a, j, b, equal, sameKey)
					}
				}
			}
		}
	}
}

// TestRowKeySetMultiColumnUsesPerColumnCollation pins the per-column half:
// column 0 BINARY and column 1 NOCASE must agree with keysEqualCollated
// row-wise, not collapse to one collation for the whole row.
func TestRowKeySetMultiColumnUsesPerColumnCollation(t *testing.T) {
	colls := []string{"BINARY", "NOCASE"}
	vals := rowKeyValueMatrix()
	for _, a0 := range vals {
		for _, a1 := range vals {
			for _, b0 := range vals {
				for _, b1 := range vals {
					ra, rb := []Value{a0, a1}, []Value{b0, b1}
					equal := keysEqualCollated(ra, rb, colls, UTF8)
					sameKey := string(collatedKeyBytes(ra, colls, UTF8)) == string(collatedKeyBytes(rb, colls, UTF8))
					if equal != sameKey {
						t.Fatalf("%v vs %v: keysEqualCollated=%v sameKey=%v", ra, rb, equal, sameKey)
					}
				}
			}
		}
	}
}

// TestRowKeySetAddKeepsFirstOccurrence pins the survivor rule the linear
// rowsContain-then-append idiom had and the recursion depends on: an equal row
// arriving later is REJECTED, so the first occurrence's own bytes are the ones
// that stay in the result (C SQLite keeps 0x61 for seeds 'a' then 'A' under
// NOCASE -- see recQueue.push, vdbe_recursive_cte.go).
func TestRowKeySetAddKeepsFirstOccurrence(t *testing.T) {
	s := newRowKeySet([]string{"NOCASE"}, UTF8)
	if !s.add([]Value{{Typ: Text, S: []byte("a")}}) {
		t.Fatal("first 'a' was not newly added")
	}
	if s.add([]Value{{Typ: Text, S: []byte("A")}}) {
		t.Error("'A' was added again under NOCASE -- the first occurrence must win")
	}
	if !s.add([]Value{{Typ: Text, S: []byte("b")}}) {
		t.Error("'b' was rejected")
	}
}

// benchRecursiveUnionCTE runs a WHERE-bounded UNION recursion producing n rows,
// whose dedup set therefore grows to n. Doubling n must roughly DOUBLE the
// time; a quadratic membership test quadruples it. Measured (-benchtime 20x
// -count=3, best of 3, ms/op) before and after the rowKeySet:
//
//	n     512   1024   2048   4096   8192  16384  32768
//	before 8.2   22.7   66.2  193.8    --     --     --   (2.8x/2.9x/2.9x)
//	after  4.8   10.2   23.0   51.8  118.6  233.4  437.0  (2.1x/2.3x/2.1x/2.3x/2.0x/1.9x)
//
// End to end, the shape this was found on -- a NON-terminating recursion, run
// to cteRecursionRowCap's 50,000 rows -- went from 29.8s to 0.74s
// engine-direct, against 0.65s for the identical UNION ALL recursion, which
// dedups nothing and was never affected. That last number is the floor: the
// dedup is no longer measurable against the cost of running the arm.
func benchRecursiveUnionCTE(b *testing.B, n int) {
	path := filepath.Join(b.TempDir(), "cterec.sqlite")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	q := fmt.Sprintf(
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<%d) SELECT count(*) FROM c", n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 1 || rows[0][0].I != int64(n) {
			b.Fatalf("n=%d: unexpected result %v", n, rows)
		}
	}
}

func BenchmarkRecursiveUnionCTE512(b *testing.B)  { benchRecursiveUnionCTE(b, 512) }
func BenchmarkRecursiveUnionCTE1024(b *testing.B) { benchRecursiveUnionCTE(b, 1024) }
func BenchmarkRecursiveUnionCTE2048(b *testing.B) { benchRecursiveUnionCTE(b, 2048) }
func BenchmarkRecursiveUnionCTE4096(b *testing.B) { benchRecursiveUnionCTE(b, 4096) }
