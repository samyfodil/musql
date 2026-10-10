package libsql

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

// BenchmarkKNN is exact nearest-neighbour search the way libSQL applications
// write it without an index: every row's distance to the query, the closest
// ten kept. The table holds n float32 embeddings of d dimensions.
func BenchmarkKNN(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		for _, d := range []int{384} {
			for _, e := range []struct{ name, driver, dsn string }{
				{"musql", "sqlite", ":memory:"},
				{"libsql", "libsql", "file::memory:"},
			} {
				b.Run(fmt.Sprintf("%s/n=%d/d=%d", e.name, n, d), func(b *testing.B) {
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
}

// blob is a random float32 vector in libSQL's layout.
func blob(r *rand.Rand, d int) []byte {
	out := make([]byte, 0, 4*d)
	for range d {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(r.NormFloat64())))
	}
	return out
}
