package vec1

import (
	"database/sql"
	"math/rand/v2"
	"testing"
)

// TestVec1Recall compares what each engine's own training buys: models
// trained independently (training is randomized, so they differ), each used
// by its own engine, must find the true nearest neighbours about as often.
// musql's mean recall@10 over several models may not fall more than 0.05
// below the oracle's, nor below 0.8, on clustered data.
func TestVec1Recall(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	const dims, n, nq, trials = 32, 4000, 40, 12
	vecs := clusteredVecs(r, n, dims, 40)
	queries := clusteredVecs(r, nq, dims, 40)
	truth := exactTop(vecs, queries, 10)
	for _, cf := range []struct{ name, model string }{
		{"ivfpq", `{nbucket: 40, codesize: 8, quantizer:"pq"}`},
		{"opq", `{nbucket: 40, codesize: 8, quantizer:"opq", nopq_round: 3}`},
		{"ivfbq", `{nbucket: 40, quantizer:"bq"}`},
		{"ivf", `{nbucket: 40}`},
	} {
		t.Run(cf.name, func(t *testing.T) {
			m, c := engines(t)
			trial := func(db *sql.DB) float64 {
				mustExec(t, db, "DROP TABLE IF EXISTS v")
				mustExec(t, db, "CREATE VIRTUAL TABLE v USING vec1(vector)")
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				for i, v := range vecs {
					if _, err := tx.Exec("INSERT INTO v(rowid, vector) VALUES(?, ?)", i+1, vecBlob(v)); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				mustExec(t, db, "INSERT INTO v(cmd, arg) VALUES('rebuild', ?)", trainModel(t, db, "v", cf.model))
				hits := 0
				for qi, q := range queries {
					qb := vecBlob(q)
					rows, err := db.Query("SELECT rowid FROM v(?, '{k:50, nprobe:4}') ORDER BY vec1_l2_distance(?, vector) LIMIT 10", qb, qb)
					if err != nil {
						t.Fatal(err)
					}
					want := map[int64]bool{}
					for _, id := range truth[qi] {
						want[id] = true
					}
					for rows.Next() {
						var id int64
						if err := rows.Scan(&id); err != nil {
							t.Fatal(err)
						}
						if want[id] {
							hits++
						}
					}
					rows.Close()
				}
				return float64(hits) / float64(10*nq)
			}
			// One trained model's recall@10 moves by several points from
			// run to run (0.81 to 0.96 for ivfbq in either engine), so a
			// few models each are averaged.
			recall := func(db *sql.DB) float64 {
				sum := 0.0
				for range trials {
					sum += trial(db)
				}
				return sum / trials
			}
			got, want := recall(m), recall(c)
			t.Logf("recall@10 musql %.3f, C %.3f", got, want)
			if got < want-0.05 || got < 0.8 {
				t.Fatalf("musql's recall@10 %.3f against the oracle's %.3f", got, want)
			}
		})
	}
}
