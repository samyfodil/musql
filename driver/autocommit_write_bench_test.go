package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// Autocommit write benchmarks, by JOURNAL MODE -- the shape an application that
// never calls Begin actually runs, and the one C SQLite is fastest at. On this
// format every mode commits the same way (one batch appended to the delta), so
// the modes should be within noise of each other; a gap between them is a
// finding.
func benchAutocommitUpdate(b *testing.B, mode string, rows int) {
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "w.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=" + mode); err != nil {
		b.Fatalf("journal_mode=%s: %v", mode, err)
	}
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`); err != nil {
		b.Fatal(err)
	}
	// The fixture loads inside ONE transaction: as autocommit statements it would
	// be the benchmark, run `rows` times, before timing started.
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?,?,?)`, i, i*7, fmt.Sprint("row", i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec(`UPDATE t SET v = v + 1 WHERE id = ?`, (i%rows)+1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAutocommitUpdateDelete(b *testing.B) {
	benchAutocommitUpdate(b, "delete", 10000)
}

func BenchmarkAutocommitUpdateTruncate(b *testing.B) {
	benchAutocommitUpdate(b, "truncate", 10000)
}

func BenchmarkAutocommitUpdateWAL(b *testing.B) {
	benchAutocommitUpdate(b, "wal", 10000)
}

// BenchmarkAutocommitUpdateWALLarge is the same statement against eight times
// the rows. A cost that barely moves says the per-statement work is independent
// of the database's size; one that scales with it is a whole-file or whole-log
// pass. WAL scales here and the rollback-journal modes do not -- that is the
// log walk, not the commit.
func BenchmarkAutocommitUpdateWALLarge(b *testing.B) {
	benchAutocommitUpdate(b, "wal", 80000)
}
