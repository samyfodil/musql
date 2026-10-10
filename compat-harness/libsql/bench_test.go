package libsql

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// BenchmarkKNN is exact nearest-neighbour search the way libSQL applications
// write it without an index: every row's distance to the query, the closest
// ten kept. The table holds n float32 embeddings of d dimensions.
func BenchmarkKNN(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		for _, d := range []int{384} {
			for _, e := range []struct {
				name, driver, dsn string
				workers           int
			}{
				{"musql", "sqlite", ":memory:", 1},
				{"musql-8cores", "sqlite", ":memory:", 8},
				{"libsql", "libsql", "file::memory:", 1},
			} {
				b.Run(fmt.Sprintf("%s/n=%d/d=%d", e.name, n, d), func(b *testing.B) {
					engine.Configure(engine.WithWorkers(e.workers))
					defer engine.Configure()
					db := open(b, e.driver, e.dsn)
					fill(b, db, n, d)
					q := blob(rand.New(rand.NewPCG(9, 9)), d)
					for _, f := range []string{"cos", "l2"} {
						b.Run(f, func(b *testing.B) {
							stmt := fmt.Sprintf("SELECT id FROM docs ORDER BY vector_distance_%s(emb, ?) LIMIT 10", f)
							for b.Loop() {
								rows, err := db.Query(stmt, q)
								if err != nil {
									b.Fatal(err)
								}
								k := 0
								for rows.Next() {
									k++
								}
								rows.Close()
								if k != 10 {
									b.Fatalf("%d rows", k)
								}
							}
							b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds()/1e6, "Mrows/s")
						})
					}
				})
			}
		}
	}
}

func fill(b *testing.B, db *sql.DB, n, d int) {
	b.Helper()
	if _, err := db.Exec("CREATE TABLE docs(id INTEGER PRIMARY KEY, emb F32_BLOB(" + fmt.Sprint(d) + "))"); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	ins, err := tx.Prepare("INSERT INTO docs(emb) VALUES (?)")
	if err != nil {
		b.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 1))
	for range n {
		if _, err := ins.Exec(blob(r, d)); err != nil {
			b.Fatal(err)
		}
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	// Compacted, as a table that is read more than written is.
	if _, err := db.Exec("VACUUM"); err != nil {
		b.Fatal(err)
	}
}

// blob is a random float32 vector in libSQL's layout.
func blob(r *rand.Rand, d int) []byte {
	out := make([]byte, 0, 4*d)
	for range d {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(r.NormFloat64())))
	}
	return out
}

// BenchmarkKNNFirst is the first search on a freshly compacted table: what a
// search costs before any per-segment state exists.
func BenchmarkKNNFirst(b *testing.B) {
	const n, d = 100_000, 384
	for _, workers := range []int{1, 8} {
		b.Run(fmt.Sprintf("musql-%dw", workers), func(b *testing.B) {
			engine.Configure(engine.WithWorkers(workers))
			defer engine.Configure()
			q := blob(rand.New(rand.NewPCG(9, 9)), d)
			for b.Loop() {
				b.StopTimer()
				db := open(b, "sqlite", ":memory:")
				fill(b, db, n, d)
				b.StartTimer()
				rows, err := db.Query("SELECT id FROM docs ORDER BY vector_distance_cos(emb, ?) LIMIT 10", q)
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
				}
				rows.Close()
				b.StopTimer()
				db.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkVectorInsert inserts rows written the libSQL way, the vector as
// text through vector(), 250 to a statement as the browser race does.
func BenchmarkVectorInsert(b *testing.B) {
	const rows, d = 2000, 384
	r := rand.New(rand.NewPCG(3, 3))
	var stmts []string
	for lo := 0; lo < rows; lo += 250 {
		var vals []string
		for i := lo; i < lo+250; i++ {
			vals = append(vals, fmt.Sprintf("(%d, vector('%s'))", i+1, randVec(r, d)))
		}
		stmts = append(stmts, "INSERT INTO v VALUES "+strings.Join(vals, ","))
	}
	for _, e := range []struct{ name, driver, dsn string }{
		{"musql", "sqlite", ":memory:"},
		{"libsql", "libsql", "file::memory:"},
	} {
		b.Run(e.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				db := open(b, e.driver, e.dsn)
				db.Exec(fmt.Sprintf("CREATE TABLE v(id INTEGER PRIMARY KEY, emb F32_BLOB(%d))", d))
				b.StartTimer()
				for _, s := range stmts {
					if _, err := db.Exec(s); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				db.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkVectorTopK is vector_top_k over a libsql_vector_idx index: libSQL
// walks its DiskANN graph, musql searches the column exactly. Each run logs
// libSQL's recall@10 against the exact answer, which musql always returns.
// The index build is not timed.
func BenchmarkVectorTopK(b *testing.B) {
	const n, d, k = 100_000, 384, 10
	q := blob(rand.New(rand.NewPCG(9, 9)), d)
	exact := map[int64]bool{}
	for _, e := range []struct{ name, driver, dsn string }{
		{"musql", "sqlite", ":memory:"},
		{"libsql", "libsql", "file::memory:"},
	} {
		b.Run(e.name, func(b *testing.B) {
			db := open(b, e.driver, e.dsn)
			fill(b, db, n, d)
			if _, err := db.Exec("CREATE INDEX docs_i ON docs(libsql_vector_idx(emb))"); err != nil {
				b.Fatal(err)
			}
			ids := func() []int64 {
				rows, err := db.Query(fmt.Sprintf("SELECT id FROM vector_top_k('docs_i', ?, %d)", k), q)
				if err != nil {
					b.Fatal(err)
				}
				defer rows.Close()
				var out []int64
				for rows.Next() {
					var id int64
					rows.Scan(&id)
					out = append(out, id)
				}
				return out
			}
			got := ids()
			if e.name == "musql" {
				for _, id := range got {
					exact[id] = true
				}
			} else {
				hit := 0
				for _, id := range got {
					if exact[id] {
						hit++
					}
				}
				b.Logf("libSQL recall@%d: %d/%d", k, hit, k)
			}
			for b.Loop() {
				if len(ids()) != k {
					b.Fatal("short")
				}
			}
		})
	}
}
