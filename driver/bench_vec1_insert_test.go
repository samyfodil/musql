package driver_test

import (
	"database/sql"
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// BenchmarkVec1Insert is the vec1 bench's insert stage at driver level: rows of
// a 384-dimension float vector inserted through one prepared statement in one
// transaction, then committed. Reported per row.
func BenchmarkVec1Insert(b *testing.B) {
	const rows, dim = 20000, 384
	r := rand.New(rand.NewSource(7))
	blobs := make([][]byte, rows)
	for i := range blobs {
		v := make([]byte, 4*dim)
		for j := 0; j < dim; j++ {
			binary.LittleEndian.PutUint32(v[4*j:], math.Float32bits(float32(r.NormFloat64())))
		}
		blobs[i] = v
	}
	b.ReportAllocs()
	for it := 0; it < b.N; it++ {
		b.StopTimer()
		dir, err := os.MkdirTemp("", "v1i")
		if err != nil {
			b.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, "v.musq"))
		if err != nil {
			b.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("CREATE VIRTUAL TABLE v USING vec1(vector)"); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		st, err := tx.Prepare("INSERT INTO v(rowid, vector) VALUES(?, ?)")
		if err != nil {
			b.Fatal(err)
		}
		for i, v := range blobs {
			if _, err := st.Exec(i+1, v); err != nil {
				b.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.Close()
		os.RemoveAll(dir)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*rows), "ns/row")
}
