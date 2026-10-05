package engine

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// VDBE ceiling benchmark: measure VDBE cost against a direct cursor loop.

func BenchmarkW4Ceiling(b *testing.B) {
	p := benchScanDB(b)
	tbl, err := p.resolveTable("t")
	if err != nil {
		b.Fatal(err)
	}
	const want = 3 // column v
	const threshold = 500_000

	b.Run("vdbe", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := p.QueryArgs("SELECT count(*) FROM t WHERE v > 500000", nil); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("cursor", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cur := openCursor(p, tbl)
			cur.streamable = true
			cur.colMask = 1 << want
			if rerr := cur.rewind(); rerr != nil {
				b.Fatal(rerr)
			}
			n := 0
			for cur.advance() {
				var v Value
				if cerr := cur.readColumn(want, &v); cerr != nil {
					b.Fatal(cerr)
				}
				if v.Typ == Int && v.I > threshold {
					n++
				}
			}
			if cur.streamErrPending != nil {
				b.Fatal(cur.streamErrPending)
			}
			runtime.KeepAlive(n)
		}
	})

	// W4's predicate wrapped in one scalar call, so (vdbe_abs - vdbe) is the
	// cost of a single OpFunction per row and nothing else. It is here because
	// that opcode resolves its callee by NAME per row
	// (callScalarFuncEnc(op.P4.(string), ...), vdbe.go), where C resolves a
	// FuncDef* at prepare time and OP_Function just calls pCtx->pFunc->xSFunc
	// (vdbe.c:8850) -- the same departure the arithmetic and comparison arms
	// had, in the opcode that runs once per row per function call in a query.
	b.Run("vdbe_abs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := p.QueryArgs("SELECT count(*) FROM t WHERE abs(v) > 500000", nil); err != nil {
				b.Fatal(err)
			}
		}
	})

}

// BenchmarkScalarDispatch prices the HALF of OpFunction that is still a
// departure from C after the argv fix: the callee is found by switching on a
// NAME string, once per row, where C holds a resolved FuncDef* in P4 and calls
// pCtx->pFunc->xSFunc directly (vdbe.c:8879).
//
// "direct" calls the function body with no lookup at all -- what a resolved
// callee would cost. "dispatch/*" goes through callScalarFuncEnc's 45-case
// string switch, which Go lowers to a binary search on length and then value,
// so the arms are chosen to differ in NAME LENGTH (3, 6, 7) rather than in
// source position, which that lowering does not care about.
//
// (direct - dispatch) is the whole budget for resolving the callee at compile
// time. It is measured before that is built, because a codegen change that
// threads a resolved function through P4 is a large edit and the last three
// ports each turned out to be worth less than the one before.
func BenchmarkScalarDispatch(b *testing.B) {
	args := []Value{{Typ: Int, I: -42}}

	b.Run("direct/fnAbs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			v, err := fnAbs(args[0])
			if err != nil {
				b.Fatal(err)
			}
			sinkValue = v
		}
	})

	for _, name := range []string{"abs", "typeof", "subtype"} {
		b.Run("dispatch/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v, err := callScalarFuncEnc(name, args, false, UTF8, 0)
				if err != nil {
					b.Fatal(err)
				}
				sinkValue = v
			}
		})
	}
}

// sinkValue keeps a benchmark's result from being optimized away without the
// cost of a runtime.KeepAlive per iteration.
var sinkValue Value

// BenchmarkCollationDispatch prices the other standing departure: a comparison
// carries its collation as a NAME, and collatedTextCompareEnc re-derives the
// collating function from that name on every TEXT/TEXT comparison -- through
// asciiFold, which is []byte(s) followed by string(b), so twice per call. C
// resolves a CollSeq* at prepare and hands it to sqlite3MemCompare
// (vdbe.c:2383).
//
// "bytes" is the floor: what a resolved BINARY collation would cost, which is
// the comparison and nothing else. The gap is the budget for carrying a
// resolved collation in P4.
//
// Integer operands are deliberately NOT measured here -- compareOp's ported
// integer arm (vdbe.c:2288) now answers those before any collation is
// consulted, which is why this probe had to be written against TEXT.
func BenchmarkCollationDispatch(b *testing.B) {
	l := Value{Typ: Text, S: []byte("the quick brown fox")}
	r := Value{Typ: Text, S: []byte("the quick brown foy")}
	var sink int

	b.Run("bytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sink = bytesCompareForBench(l.S, r.S)
		}
	})

	for _, coll := range []string{"BINARY", "NOCASE", "RTRIM"} {
		b.Run("collated/"+coll, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sink = compareValuesCollatedEnc(l, r, coll, UTF8)
			}
		})
	}
	runtime.KeepAlive(sink)
}

// exprSQL is the steelman for a code generator, and the shape the nine-workload
// scoreboard does not contain: same I/O as W4 -- one full scan of the same
// b-tree, one aggregate -- but ~15 opcodes per row instead of ~6. If generated
// code is worth anything on this engine it is worth it HERE, where the work is
// arithmetic on values already in hand rather than reading pages.
//
// Paired with exprFused below, which computes the identical predicate with no
// VDBE at all, so (vdbe - fused) at this opcode count against (vdbe - fused) at
// W4's isolates the per-opcode execution cost from the decode and the
// b-tree walk, both of which are common to every arm.
const exprSQL = `SELECT count(*) FROM t WHERE v*2 + k*3 - sec > 1000000 AND bid < 90000 AND k <> 7`

// BenchmarkExprCeiling is BenchmarkW4Ceiling's vdbe arm over exprSQL.
func BenchmarkExprCeiling(b *testing.B) {
	p := benchScanDB(b)
	b.Run("vdbe", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := p.QueryArgs(exprSQL, nil); err != nil {
				b.Fatal(err)
			}
		}
	})

}

// bytesCompareForBench is bytes.Compare behind a non-inlinable call, so the
// "bytes" floor arm pays the same call overhead the collated arms do and the
// difference between them is the collation lookup alone.
//
//go:noinline
func bytesCompareForBench(a, b []byte) int { return bytes.Compare(a, b) }

// benchScanPager is the shared 100k-row fixture the scan benchmarks run over:
// seeding it costs far more than the scans, so it is built once per benchmark
// process.
var benchScanPager *ReadOnlyPager

func benchScanDB(b *testing.B) *ReadOnlyPager {
	b.Helper()
	if benchScanPager != nil {
		return benchScanPager
	}
	dir, err := os.MkdirTemp("", "musql-p2-bench")
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(dir, "bench.musq")
	db, err := Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
	} {
		if err := db.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(0x5eed))
	for i := 1; i <= 100_000; i++ {
		if err := db.Exec(fmt.Sprintf(
			"INSERT INTO t(id,sec,k,v,bid,payload) VALUES(%d,%d,%d,%d,%d,'row-%d-payload')",
			i, rng.Intn(100_000), rng.Intn(10), rng.Intn(1_000_000), 1+rng.Intn(100_000), i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	benchScanPager = p
	return p
}
