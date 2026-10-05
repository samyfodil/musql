package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	musqlengine "github.com/samyfodil/musql/engine"
)

// TestSegmentWriteVsC benchmarks segment format write performance against C SQLite.
// Both use a held connection with bound parameters on a single-row-per-commit workload.
func TestSegmentWriteVsC(t *testing.T) {
	const rows = 10000
	const iters = 500

	seedC := func(drv, mode string) *sql.DB {
		db, err := sql.Open(drv, filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("PRAGMA journal_mode=" + mode); err != nil {
			t.Fatalf("%s %s: %v", drv, mode, err)
		}
		if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`); err != nil {
			t.Fatal(err)
		}
		tx, _ := db.Begin()
		for i := 1; i <= rows; i++ {
			if _, err := tx.Exec(`INSERT INTO t VALUES(?,?,?)`, i, i*7, fmt.Sprint("r", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return db
	}
	timeC := func(db *sql.DB) time.Duration {
		if _, err := db.Exec(`UPDATE t SET k = ? WHERE id = ?`, 0, 1); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for i := 0; i < iters; i++ {
			if _, err := db.Exec(`UPDATE t SET k = ? WHERE id = ?`, i, (i%rows)+1); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / iters
	}

	// Build the seed with C SQLite, import it to the segment format.
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.db")
	segPath := filepath.Join(dir, "n.musq")
	seedDB, serr := sql.Open("sqlite3", seed)
	if serr != nil {
		t.Fatal(serr)
	}
	if _, err := seedDB.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		if _, eerr := seedDB.Exec(`INSERT INTO t VALUES(?,?,?)`, i, i*7, fmt.Sprintf("r%d", i)); eerr != nil {
			t.Fatal(eerr)
		}
	}
	if err := seedDB.Close(); err != nil {
		t.Fatal(err)
	}
	if eerr := sqliteconv.Import(seed, segPath, sqliteconv.ImportOptions{}); eerr != nil {
		t.Fatalf("ImportSQLite: %v", eerr)
	}

	nw, nerr := musqlengine.OpenWrite(segPath)
	if nerr != nil {
		t.Fatalf("OpenWrite: %v", nerr)
	}
	arg := func(a, b int) []musqlengine.Value {
		return []musqlengine.Value{
			{Typ: musqlengine.Int, I: int64(a)}, {Typ: musqlengine.Int, I: int64(b)}}
	}
	if _, _, eerr := nw.ExecArgs(`UPDATE t SET k = ? WHERE id = ?`, arg(0, 1)); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	start := time.Now()
	for i := 0; i < iters; i++ {
		if _, _, eerr := nw.ExecArgs(`UPDATE t SET k = ? WHERE id = ?`, arg(i, (i%rows)+1)); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	segDur := time.Since(start) / iters

	// Benchmark with no fsync per commit to match C's WAL contract.
	nw.NoSyncOnCommit = true
	start = time.Now()
	for i := 0; i < iters; i++ {
		if _, _, eerr := nw.ExecArgs(`UPDATE t SET k = ? WHERE id = ?`, arg(i, (i%rows)+1)); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	segNoSync := time.Since(start) / iters
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}

	cDelete := timeC(seedC("sqlite3", "delete"))
	cWAL := timeC(seedC("sqlite3", "wal"))

	verdict := func(ours, theirs time.Duration) string {
		r := float64(ours) / float64(theirs)
		if r < 1 {
			return fmt.Sprintf("%.2fx FASTER", 1/r)
		}
		return fmt.Sprintf("%.2fx slower", r)
	}
	t.Logf("one row changed per durable commit, %d-row table, %d iterations, held connection", rows, iters)
	t.Logf("%-38s | %12s", "arm", "per commit")
	t.Logf("%-38s | %12s", "musql SEGMENT format (fsync each)", segDur.Round(time.Microsecond))
	t.Logf("%-38s | %12s", "musql SEGMENT format (no fsync)", segNoSync.Round(time.Microsecond))
	t.Logf("%-38s | %12s", "C SQLite, journal_mode=delete", cDelete.Round(time.Microsecond))
	t.Logf("%-38s | %12s", "C SQLite, journal_mode=wal", cWAL.Round(time.Microsecond))
	t.Logf("")
	t.Logf("segment vs C delete: %s", verdict(segDur, cDelete))
	t.Logf("segment vs C wal (both fsync-per-commit: C does NOT, so this favours C): %s", verdict(segDur, cWAL))
	t.Logf("segment no-fsync vs C wal, EQUAL durability: %s", verdict(segNoSync, cWAL))
}
