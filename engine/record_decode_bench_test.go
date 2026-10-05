package engine

import (
	"fmt"
	"testing"
)

// benchRecords builds n records shaped exactly like compat-harness's benchmark
// table t -- (id INTEGER PRIMARY KEY, sec, k, v, bid INTEGER, payload TEXT) --
// which is the record shape every figure quoted for these benchmarks is
// measured against. The IPK column is stored as a NULL (SQLite keeps its value
// only as the b-tree key), so the header is 0,int,int,int,int,text.
func benchRecords(n int) [][]byte {
	recs := make([][]byte, n)
	for i := range recs {
		recs[i] = encodeRecord([]Value{
			{Typ: Null},
			{Typ: Int, I: int64(i * 7 % 100000)},
			{Typ: Int, I: int64(i % 10)},
			{Typ: Int, I: int64(i * 13 % 1000000)},
			{Typ: Int, I: int64(i%100000 + 1)},
			{Typ: Text, S: []byte(fmt.Sprintf("row-%d-payload", i))},
		})
	}
	return recs
}

// BenchmarkDecodeRecordFull times the eager whole-record decode every table
// scan performs: all six columns. It is the control the masked decode below is
// quoted against, and the A/B subject for the header-pass changes.
func BenchmarkDecodeRecordFull(b *testing.B) {
	recs := benchRecords(4096)
	var buf []Value
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := decodeRecordInto(recs[i&4095], buf)
		if err != nil {
			b.Fatal(err)
		}
		buf = v
	}
}

// BenchmarkDecodeRecordMasked is the column-mask ceiling: the same records as
// BenchmarkDecodeRecordFull, decoded under the masks the compat-harness
// workloads actually need. "all6" is the control -- the identical work
// BenchmarkDecodeRecordFull does, reached through the masked entry point, so
// any difference between them is the mask test itself and nothing else.
func BenchmarkDecodeRecordMasked(b *testing.B) {
	recs := benchRecords(4096)
	for _, m := range []struct {
		name string
		mask columnMask
	}{
		{"all6", allColumns},
		{"col3_only", 1 << 3},      // W4: count(*) WHERE v > ?
		{"col2_col3", 1<<2 | 1<<3}, // W5: k, count(*), sum(v) GROUP BY k
		{"col0_col3", 1<<0 | 1<<3}, // W6: id, v ORDER BY v DESC LIMIT 20
	} {
		b.Run(m.name, func(b *testing.B) {
			var buf []Value
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				v, err := decodeRecordMaskedInto(recs[i&4095], buf, m.mask)
				if err != nil {
					b.Fatal(err)
				}
				buf = v
			}
		})
	}
}
