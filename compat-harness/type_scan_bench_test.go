package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	mush "github.com/samyfodil/musql/driver"
	musqlengine "github.com/samyfodil/musql/engine"
)

// TestTypeScanVsC: scan performance on non-integer column types.
func TestTypeScanVsC(t *testing.T) {
	const n = 100000
	dir := t.TempDir()
	type work struct{ name, sql string }
	// Value column (unique) and group column (~10 distinct).
	works := []work{
		{"INTEGER filter count", `SELECT count(*) FROM t WHERE iv < 50000`},
		{"TEXT    filter count", `SELECT count(*) FROM t WHERE tv < 'k050000'`},
		{"REAL    filter count", `SELECT count(*) FROM t WHERE rv < 50000.5`},
		{"BLOB    filter count", `SELECT count(*) FROM t WHERE bv < x'62303530303030'`},
		{"INTEGER group by", `SELECT gi, count(*) FROM t GROUP BY gi`},
		{"TEXT    group by", `SELECT gt, count(*) FROM t GROUP BY gt`},
		{"REAL    group by", `SELECT gr, count(*) FROM t GROUP BY gr`},
		{"BLOB    group by", `SELECT gb, count(*) FROM t GROUP BY gb`},
		{"INTEGER order limit", `SELECT id FROM t ORDER BY iv DESC LIMIT 20`},
		{"TEXT    order limit", `SELECT id FROM t ORDER BY tv DESC LIMIT 20`},
		{"REAL    order limit", `SELECT id FROM t ORDER BY rv DESC LIMIT 20`},
		{"BLOB    order limit", `SELECT id FROM t ORDER BY bv DESC LIMIT 20`},
	}
	results := map[string]map[string]time.Duration{}
	arm := map[string]string{}

	for _, e := range []struct{ label, driver string }{{"musql", mush.DriverName}, {"C SQLite", "sqlite3"}} {
		db, err := sql.Open(e.driver, filepath.Join(dir, e.label+".db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY,
			iv INTEGER, tv TEXT, rv REAL, bv BLOB,
			gi INTEGER, gt TEXT, gr REAL, gb BLOB)`); err != nil {
			t.Fatal(err)
		}
		tx, _ := db.Begin()
		st, _ := tx.Prepare(`INSERT INTO t VALUES(?,?,?,?,?,?,?,?,?)`)
		for i := 0; i < n; i++ {
			if _, err := st.Exec(i+1, i, fmt.Sprintf("k%06d", i), float64(i)+0.5,
				[]byte(fmt.Sprintf("b%06d", i)),
				i%10, fmt.Sprintf("g%d", i%10), float64(i%10)+0.5,
				[]byte(fmt.Sprintf("h%d", i%10))); err != nil {
				t.Fatalf("%s seed: %v", e.label, err)
			}
		}
		st.Close()
		tx.Commit()

		results[e.label] = map[string]time.Duration{}
		for _, w := range works {
			// Warm, then best of 3 rounds of 10 -- each round touches all 100,000
			// rows, so 10 is already a lot of work per sample.
			drain := func() {
				rows, err := db.Query(w.sql)
				if err != nil {
					t.Fatalf("%s %s: %v", e.label, w.name, err)
				}
				for rows.Next() {
				}
				rows.Close()
			}
			drain()
			// WHICH ARM ANSWERED, not which arm the timing suggests. The engine
			// exports these counters precisely so a number labelled "columnar"
			// cannot be a VDBE number wearing a columnar label -- see
			// ProgramUsesSegFilterForTest's own doc comment, which says this repo
			// has already published one such number.
			if e.label == "musql" {
				musqlengine.ResetSegFilterCountersForTest()
				drain()
				served, declined := musqlengine.SegFilterCountersForTest()
				arm[w.name] = fmt.Sprintf("served=%d declined=%d", served, declined)
			}
			best := time.Duration(0)
			for r := 0; r < 3; r++ {
				t0 := time.Now()
				for i := 0; i < 10; i++ {
					drain()
				}
				if d := time.Since(t0) / 10; best == 0 || d < best {
					best = d
				}
			}
			results[e.label][w.name] = best
		}
		db.Close()
	}

	t.Logf("%-22s | %12s | %12s | %8s | %s", "workload", "musql", "C SQLite", "musql/C", "columnar arm")
	for _, w := range works {
		m, c := results["musql"][w.name], results["C SQLite"][w.name]
		t.Logf("%-22s | %10.2fµs | %10.2fµs | %7.2fx | %s",
			w.name, float64(m.Nanoseconds())/1000, float64(c.Nanoseconds())/1000,
			float64(m)/float64(c), arm[w.name])
	}
}
