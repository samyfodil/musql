package vec1

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// TestVec1RecallSeeds trains the same index over the same data many times in
// each engine and compares the recall each one reaches. Training is random in
// vec1 (its seeds come from sqlite3_randomness), so one run of each says little
// about whether musql's recall differs from vec1's: the spread between runs of
// ONE engine can be as wide as the gap between the two. This puts a number on
// it.
//
// VEC1_RECALL_RUNS sets the runs per engine (default 0: skipped); VEC1_BENCH_N,
// _D, _Q and _INDEX (ivfpq, ivfopq, ivf, ivfbq) the data and the index.
func TestVec1RecallSeeds(t *testing.T) {
	runs := envInt("VEC1_RECALL_RUNS", 0)
	if runs == 0 {
		t.Skip("VEC1_RECALL_RUNS not set")
	}
	n := envInt("VEC1_BENCH_N", 20000)
	d := envInt("VEC1_BENCH_D", 384)
	nq := envInt("VEC1_BENCH_Q", 200)
	index := envStr("VEC1_BENCH_INDEX", "ivfpq")
	data := envStr("VEC1_BENCH_DATA", "random")
	threads := envInt("VEC1_BENCH_THREADS", 4)
	r := rand.New(rand.NewPCG(11, 11))
	var vecs [][]float32
	if data == "clustered" {
		vecs = benchClustered(r, n+nq, d)
	} else {
		vecs = benchRandom(r, n+nq, d)
	}
	queries := vecs[n:]
	vecs = vecs[:n]
	truth := exactTop(vecs, queries, 10)
	nbucket := int(math.Sqrt(float64(n)))
	var model string
	k, nprobe := 100, 0.05
	switch index {
	case "ivf":
		model, k = fmt.Sprintf(`{nbucket:%d}`, nbucket), 10
	case "ivfopq":
		model = fmt.Sprintf(`{nbucket:%d, quantizer:"opq", codesize:32}`, nbucket)
	case "ivfbq":
		model = fmt.Sprintf(`{nbucket:%d, quantizer:"bq"}`, nbucket)
	default:
		model = fmt.Sprintf(`{nbucket:%d, quantizer:"pq", codesize:%d}`, nbucket, d/8)
	}
	rerank := index != "ivf"
	got := map[string][]float64{}
	for run := 0; run < runs; run++ {
		for _, eng := range []string{"C", "musql"} {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("%s-%d.db", eng, run))
			var db *sql.DB
			var err error
			if eng == "C" {
				db, err = openOracle(path)
			} else {
				db, err = sql.Open(driver.DriverName, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			mustExec(t, db, fmt.Sprintf("SELECT vec1_config('nthread', %d)", threads))
			mustExec(t, db, "CREATE VIRTUAL TABLE v USING vec1(vector)")
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			ins, err := tx.Prepare("INSERT INTO v(rowid, vector) VALUES(?, ?)")
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range vecs {
				if _, err := ins.Exec(i+1, vecBlob(v)); err != nil {
					t.Fatal(err)
				}
			}
			ins.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var blob []byte
			step := max(1, n/(100*nbucket))
			if err := db.QueryRow(fmt.Sprintf("SELECT vec1_train(vector, ?) FROM v WHERE rowid %% %d = 0", step), model).Scan(&blob); err != nil {
				t.Fatalf("%s train: %v", eng, err)
			}
			mustExec(t, db, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", blob)
			params := fmt.Sprintf(`{k:%d, nprobe:%g}`, k, nprobe)
			q := "SELECT rowid FROM v(?, ?)"
			if rerank {
				q = "SELECT rowid FROM v(?, ?) ORDER BY vec1_l2_distance(?, vector) LIMIT 10"
			}
			hits := 0
			for qi, qv := range queries {
				qb := vecBlob(qv)
				args := []any{qb, params}
				if rerank {
					args = append(args, qb)
				}
				rows, err := db.Query(q, args...)
				if err != nil {
					t.Fatalf("%s query: %v", eng, err)
				}
				want := map[int64]bool{}
				for _, id := range truth[qi] {
					want[id] = true
				}
				cnt := 0
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					if cnt++; cnt <= 10 && want[id] {
						hits++
					}
				}
				rows.Close()
			}
			db.Close()
			rec := float64(hits) / float64(10*len(queries))
			got[eng] = append(got[eng], rec)
			t.Logf("RECALL %-6s run %d  %.4f", eng, run, rec)
		}
	}
	stats := func(xs []float64) (mean, sd float64) {
		for _, x := range xs {
			mean += x
		}
		mean /= float64(len(xs))
		for _, x := range xs {
			sd += (x - mean) * (x - mean)
		}
		if len(xs) > 1 {
			sd = math.Sqrt(sd / float64(len(xs)-1))
		}
		return
	}
	cm, cs := stats(got["C"])
	mm, ms := stats(got["musql"])
	se := math.Sqrt(cs*cs/float64(runs) + ms*ms/float64(runs))
	z := 0.0
	if se > 0 {
		z = (mm - cm) / se
	}
	t.Logf("SUMMARY %s n=%d d=%d q=%d runs=%d  C %.4f +- %.4f   musql %.4f +- %.4f   musql-C %+.4f (z %.2f)", index, n, d, nq, runs, cm, cs, mm, ms, mm-cm, z)
}
