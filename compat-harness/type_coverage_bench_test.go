package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	mush "github.com/samyfodil/musql/driver"
)

// TestTypeCoverageVsC benchmarks indexes across column types against C SQLite
func TestTypeCoverageVsC(t *testing.T) {
	const n = 100000
	type engine struct {
		label, driver string
	}
	engines := []engine{{"musql", mush.DriverName}, {"C SQLite", "sqlite3"}}
	dir := t.TempDir()

	type row struct{ eq, rng string }
	workloads := []struct {
		name  string
		col   string
		probe func(i int) any
	}{
		{"INTEGER eq", "iv", func(i int) any { return i%n + 1 }},
		{"TEXT eq", "tv", func(i int) any { return fmt.Sprintf("k%06d", i%n) }},
		{"REAL eq", "rv", func(i int) any { return float64(i%n) + 0.5 }},
		{"BLOB eq", "bv", func(i int) any { return []byte(fmt.Sprintf("b%06d", i%n)) }},
	}
	results := map[string]map[string]time.Duration{}

	for _, e := range engines {
		db, err := sql.Open(e.driver, filepath.Join(dir, e.label+".db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range []string{
			`CREATE TABLE t(id INTEGER PRIMARY KEY, iv INTEGER, tv TEXT, rv REAL, bv BLOB)`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", e.label, err)
			}
		}
		tx, _ := db.Begin()
		st, _ := tx.Prepare(`INSERT INTO t VALUES(?,?,?,?,?)`)
		for i := 0; i < n; i++ {
			if _, err := st.Exec(i+1, i+1, fmt.Sprintf("k%06d", i), float64(i)+0.5,
				[]byte(fmt.Sprintf("b%06d", i))); err != nil {
				t.Fatalf("%s seed: %v", e.label, err)
			}
		}
		st.Close()
		tx.Commit()
		for _, ix := range []string{"iv", "tv", "rv", "bv"} {
			if _, err := db.Exec(fmt.Sprintf(`CREATE INDEX x_%s ON t(%s)`, ix, ix)); err != nil {
				t.Fatalf("%s index %s: %v", e.label, ix, err)
			}
		}
		results[e.label] = map[string]time.Duration{}
		for _, w := range workloads {
			q, err := db.Prepare(fmt.Sprintf(`SELECT id FROM t WHERE %s = ?`, w.col))
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			q.QueryRow(w.probe(1)).Scan(&id) // warm
			// Report best round to minimize noise
			rng := rand.New(rand.NewSource(0x5eed))
			const (
				iters  = 2000
				rounds = 5
			)
			best := time.Duration(0)
			for r := 0; r < rounds; r++ {
				t0 := time.Now()
				for i := 0; i < iters; i++ {
					if err := q.QueryRow(w.probe(rng.Intn(n))).Scan(&id); err != nil && err != sql.ErrNoRows {
						t.Fatalf("%s %s: %v", e.label, w.name, err)
					}
				}
				if d := time.Since(t0) / iters; best == 0 || d < best {
					best = d
				}
			}
			results[e.label][w.name] = best
			q.Close()
		}
		db.Close()
	}

	t.Logf("%-14s | %12s | %12s | %8s", "workload", "musql", "C SQLite", "musql/C")
	for _, w := range workloads {
		m := results["musql"][w.name]
		c := results["C SQLite"][w.name]
		t.Logf("%-14s | %10.2fµs | %10.2fµs | %7.2fx",
			w.name, float64(m.Nanoseconds())/1000, float64(c.Nanoseconds())/1000,
			float64(m)/float64(c))
	}
}
