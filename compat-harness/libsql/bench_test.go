package libsql

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

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

// BenchmarkVectorTopK is vector_top_k over a libsql_vector_idx index on 100k
// vectors of 384 dimensions: musql exact (the default), musql approximate (a
// tuning option given), and libSQL's DiskANN graph. Each logs its recall@10
// against the exact answer, its first search, and when musql's graph, built
// in the background, was ready. Two datasets: independent Gaussian vectors,
// where no index has structure to exploit, and clustered ones, which is what
// embeddings look like. Building the indexes is not timed.
func BenchmarkVectorTopK(b *testing.B) {
	const n, d, k = 100_000, 384, 10
	for _, data := range []string{"random", "clustered"} {
		q := blob(rand.New(rand.NewPCG(9, 9)), d)
		if data == "clustered" {
			q = clusteredBlobs(rand.New(rand.NewPCG(9, 9)), 1, d)[0]
		}
		exact := map[int64]bool{}
		for _, e := range []struct{ name, driver, dsn, opts string }{
			{"musql-exact", "sqlite", ":memory:", ""},
			{"musql-approx", "sqlite", ":memory:", ", 'search_l=200'"},
			{"libsql", "libsql", "file::memory:", ""},
		} {
			b.Run(data+"/"+e.name, func(b *testing.B) {
				db := open(b, e.driver, e.dsn)
				if data == "clustered" {
					fillRows(b, db, d, clusteredBlobs(rand.New(rand.NewPCG(1, 1)), n, d))
				} else {
					fill(b, db, n, d)
				}
				if _, err := db.Exec("CREATE INDEX docs_i ON docs(libsql_vector_idx(emb" + e.opts + "))"); err != nil {
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
				start := time.Now()
				got := ids()
				first := time.Since(start)
				// An approximate index answers exactly while its graph builds
				// in the background; wait for it, and report how long it took.
				engine.VecGraphsBuiltForTest()
				built := time.Since(start)
				if e.name == "musql-approx" {
					got = ids()
				}
				if e.name == "musql-exact" {
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
					b.Logf("%s %s: recall@%d %d/%d, first search %v, graph ready after %v", data, e.name, k, hit, k, first.Round(time.Millisecond), built.Round(time.Millisecond))
				}
				for b.Loop() {
					if len(ids()) != k {
						b.Fatal("short")
					}
				}
			})
		}
	}
}

// clusteredBlobs is n float32 vectors around 1000 random centres, the
// structure embeddings have.
func clusteredBlobs(r *rand.Rand, n, d int) [][]byte {
	cr := rand.New(rand.NewPCG(42, 42))
	centres := make([][]float64, 1000)
	for i := range centres {
		centres[i] = make([]float64, d)
		for j := range centres[i] {
			centres[i][j] = cr.NormFloat64()
		}
	}
	out := make([][]byte, n)
	for i := range out {
		c := centres[r.IntN(len(centres))]
		v := make([]byte, 0, 4*d)
		for j := range d {
			v = binary.LittleEndian.AppendUint32(v, math.Float32bits(float32(c[j]+0.5*r.NormFloat64())))
		}
		out[i] = v
	}
	return out
}

// fillRows is fill with the given vectors.
func fillRows(b *testing.B, db *sql.DB, d int, vecs [][]byte) {
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
	for _, v := range vecs {
		if _, err := ins.Exec(v); err != nil {
			b.Fatal(err)
		}
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkVectorCrossover finds where the graph overtakes the exact search:
// musql's two modes on clustered vectors of 384 dimensions at growing sizes,
// the graph timed once it is ready.
func BenchmarkVectorCrossover(b *testing.B) {
	const d, k = 384, 10
	// The index with no option must stay exact at every size measured.
	defer engine.SetVecAutoGraphRowsForTest(engine.SetVecAutoGraphRowsForTest(math.MaxInt))
	q := clusteredBlobs(rand.New(rand.NewPCG(9, 9)), 1, d)[0]
	for _, n := range []int{250_000, 500_000, 1_000_000} {
		db := open(b, "sqlite", ":memory:")
		fillRows(b, db, d, clusteredBlobs(rand.New(rand.NewPCG(1, 1)), n, d))
		for _, mode := range []struct{ name, opts string }{{"exact", ""}, {"graph", ", 'search_l=200'"}} {
			if _, err := db.Exec("CREATE INDEX i_" + mode.name + " ON docs(libsql_vector_idx(emb" + mode.opts + "))"); err != nil {
				b.Fatal(err)
			}
			stmt := fmt.Sprintf("SELECT id FROM vector_top_k('i_%s', ?, %d)", mode.name, k)
			b.Run(fmt.Sprintf("n=%d/%s", n, mode.name), func(b *testing.B) {
				rows, err := db.Query(stmt, q)
				if err != nil {
					b.Fatal(err)
				}
				rows.Close()
				engine.VecGraphsBuiltForTest()
				for b.Loop() {
					rows, err := db.Query(stmt, q)
					if err != nil {
						b.Fatal(err)
					}
					for rows.Next() {
					}
					rows.Close()
				}
			})
		}
	}
}
