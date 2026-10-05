package engine

import "testing"

// The claim the whole format rests on, over REAL engine data rather than a
// synthetic spike: the same filter, the same 100,000 rows, answered through a
// columnar segment and through the b-tree those rows live in today.
//
//	btree_scan     what a read today costs: walk, decode, compare
//	segment_value  the segment through its general accessor (NULLs, exceptions)
//	segment_slice  the segment as a plain []int64, which is the point
func BenchmarkSegmentScan(b *testing.B) {
	const n = 100_000
	seed := buildBulkUpdateBenchFile(b, n)

	db, err := OpenWrite(seed)
	if err != nil {
		b.Fatal(err)
	}
	tbl := db.tables[0]
	var rows [][]Value
	for _, vals := range tbl.rows.all() {
		rows = append(rows, append([]Value(nil), vals...))
	}
	db.Discard()
	if len(rows) != n {
		b.Fatalf("collected %d rows, want %d", len(rows), n)
	}
	buf, err := buildSegment(tbl.cols, segTestRowids(len(rows)), rows)
	if err != nil {
		b.Fatal(err)
	}
	seg, err := openSegment(buf)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("segment is %d bytes for %d rows (%.1f bytes/row), %d exceptions",
		len(buf), n, float64(len(buf))/float64(n), len(seg.exc))

	b.Run("btree_scan", func(b *testing.B) {
		rp, oerr := Open(seed)
		if oerr != nil {
			b.Fatal(oerr)
		}
		defer rp.Close()
		schema, serr := rp.Schema()
		if serr != nil {
			b.Fatal(serr)
		}
		var root uint32
		for _, r := range schema {
			if r.Type == "table" && r.Name == "t" {
				root = r.RootPage
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			seq, errFn := rp.ScanTable(root)
			for _, vals := range seq {
				if len(vals) > 3 && vals[3].Typ == Int && vals[3].I > 500_000 {
					count++
				}
			}
			if err := errFn(); err != nil {
				b.Fatal(err)
			}
			if count == 0 {
				b.Fatal("no rows matched")
			}
		}
	})

	b.Run("segment_value", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for r := 0; r < seg.nRows; r++ {
				if v := seg.Value(3, r); v.Typ == Int && v.I > 500_000 {
					count++
				}
			}
			if count == 0 {
				b.Fatal("no rows matched")
			}
		}
	})

	b.Run("segment_slice", func(b *testing.B) {
		col, ok := seg.Int64Column(3)
		if !ok {
			b.Fatal("v is not a fixed-width int64 column; the format's premise does not hold for this data")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, x := range col {
				if x > 500_000 {
					count++
				}
			}
			if count == 0 {
				b.Fatal("no rows matched")
			}
		}
	})
}

// The same scan, through a MAPPED segment file rather than a buffer built in
// this process -- which is the configuration the format is actually for, and
// the one where page faults on first touch are real.
func BenchmarkSegmentFileScan(b *testing.B) {
	const n = 100_000
	seed := buildBulkUpdateBenchFile(b, n)

	execDB(b, seed, `VACUUM`) // every row into segments, none left in the delta
	f, err := OpenSegmentFile(seed)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.Logf("segment file is %d bytes for %d rows, mapped=%v", f.m.Len(), n, f.Mapped())

	var segs []*segment
	for _, t := range f.Tables() {
		if t.Name == "t" {
			segs, err = t.openSegments()
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	if len(segs) == 0 {
		b.Fatal("table t is not in the file")
	}
	if kind, why := segs[0].scanKind(3); kind != segScanSlice {
		b.Fatalf("column v lost the fast path in the file: %q", why)
	}

	b.Run("mapped_slice", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, s := range segs {
				col, ok := s.Int64Column(3)
				if !ok {
					b.Fatal("Int64Column declined")
				}
				for _, x := range col {
					if x > 500_000 {
						count++
					}
				}
			}
			if count == 0 {
				b.Fatal("no rows matched")
			}
		}
	})

	b.Run("mapped_accessor", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			count := 0
			for _, s := range segs {
				count += segmentCountGreater(s, 3, 500_000)
			}
			if count == 0 {
				b.Fatal("no rows matched")
			}
		}
	})
}

// The two-predicate filter, which is where the format spike's lowering
// comparison lives: branch-free kernels at 5.2 ns/row flat, a fused typed loop
// at 7.8 near 50% selectivity and 1.5 at 1%. This measures what was built.
func BenchmarkSegmentFilter(b *testing.B) {
	const n = 100_000
	seed := buildBulkUpdateBenchFile(b, n)
	if _, err := CompactSegmentFile(seed); err != nil {
		b.Fatal(err)
	}
	f, err := OpenSegmentFile(seed)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	segs, err := f.Tables()[0].openSegments()
	if err != nil {
		b.Fatal(err)
	}
	// v > 500000 AND k <> 7, the spike's shape: column 3 is v, column 2 is k.
	preds := []segPred{
		{Col: 3, Op: segGT, Val: Value{Typ: Int, I: 500_000}},
		{Col: 2, Op: segNE, Val: Value{Typ: Int, I: 7}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for _, s := range segs {
			count += segFilterCount(s, preds)
		}
		if count == 0 {
			b.Fatal("no rows matched")
		}
	}
}
