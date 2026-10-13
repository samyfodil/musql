package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkVec1Query times queries against a trained vec1 index over random
// vectors, the way the vec1 bench (compat-harness/vec1) issues them: the
// index's nearest buckets, reranked by exact distance, ten rows back. It
// reports allocations too: a garbage collection that lands in a query is what
// a latency tail is made of. VEC1_BENCH_N and VEC1_BENCH_INDEX (ivfpq, ivfbq)
// choose the data; the index is built once, outside the timing.
func BenchmarkVec1Query(b *testing.B) {
	n, d := 20000, 384
	if v := os.Getenv("VEC1_BENCH_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	index := os.Getenv("VEC1_BENCH_INDEX")
	nbucket := int(math.Sqrt(float64(n)))
	model := fmt.Sprintf(`{nbucket:%d, quantizer:"pq", codesize:%d}`, nbucket, d/8)
	if index == "ivfbq" {
		model = fmt.Sprintf(`{nbucket:%d, quantizer:"bq"}`, nbucket)
	}
	r := rand.New(rand.NewPCG(5, 5))
	blob := func() []byte {
		v := make([]byte, 4*d)
		for j := 0; j < d; j++ {
			binary.LittleEndian.PutUint32(v[4*j:], math.Float32bits(float32(r.NormFloat64())))
		}
		return v
	}
	db, err := Create(filepath.Join(b.TempDir(), "v.musq"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec("CREATE VIRTUAL TABLE v USING vec1(vector)"); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, _, err := db.ExecArgs("INSERT INTO v(rowid, vector) VALUES(?, ?)", []Value{{Typ: Int, I: int64(i)}, {Typ: Blob, S: blob()}}); err != nil {
			b.Fatal(err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		b.Fatal(err)
	}
	_, rows, err := p.QueryArgs("SELECT vec1_train(vector, ?) FROM v", []Value{{Typ: Text, S: []byte(model)}})
	if err != nil {
		b.Fatal(err)
	}
	p.Close()
	if _, _, err := db.ExecArgs("INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", []Value{rows[0][0]}); err != nil {
		b.Fatal(err)
	}
	p, err = db.SnapshotPager()
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	queries := make([][]Value, 200)
	for i := range queries {
		q := blob()
		queries[i] = []Value{{Typ: Blob, S: q}, {Typ: Text, S: []byte(`{k:100, nprobe:0.05}`)}, {Typ: Blob, S: q}}
	}
	const sqlText = "SELECT rowid FROM v(?, ?) ORDER BY vec1_l2_distance(?, vector) LIMIT 10"
	if _, _, err := p.QueryArgs(sqlText, queries[0]); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := p.QueryArgs(sqlText, queries[i%len(queries)]); err != nil {
			b.Fatal(err)
		}
	}
}
