package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLockingModeExclusiveAcrossAJournalModeChange tests exclusive locking mode
// when journal mode changes while the lock is held.
func TestLockingModeExclusiveAcrossAJournalModeChange(t *testing.T) {
	differ(t, "exclusive first, then WAL", []string{
		"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)",
		"PRAGMA locking_mode=exclusive",
		"PRAGMA journal_mode=wal",
		"INSERT INTO t VALUES(2)",
		"SELECT count(*) FROM t",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		"PRAGMA locking_mode=normal",
		"PRAGMA locking_mode", "PRAGMA main.locking_mode",
		"PRAGMA journal_mode=delete",
		"SELECT count(*) FROM t", "PRAGMA journal_mode", "PRAGMA integrity_check",
	})
	differ(t, "WAL first, then exclusive", []string{
		"CREATE TABLE t(a)", "PRAGMA journal_mode=wal", "INSERT INTO t VALUES(1)",
		"PRAGMA locking_mode=exclusive",
		"INSERT INTO t VALUES(2)",
		"PRAGMA locking_mode=normal",
		"PRAGMA locking_mode", "PRAGMA main.locking_mode",
		"SELECT count(*) FROM t", "PRAGMA integrity_check",
	})
	differ(t, "a long sequence under the mode", []string{
		"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)", "PRAGMA locking_mode=exclusive",
		"SELECT count(*) FROM t", "INSERT INTO t VALUES(2)", "SELECT count(*) FROM t",
		"CREATE TEMP TABLE tt(x)", "INSERT INTO tt VALUES(9)", "SELECT * FROM tt",
		"BEGIN", "INSERT INTO t VALUES(3)", "SELECT count(*) FROM t", "COMMIT",
		"CREATE INDEX i ON t(a)", "REINDEX", "VACUUM", "SELECT count(*) FROM t",
		"PRAGMA locking_mode=normal", "SELECT count(*) FROM t", "PRAGMA integrity_check",
	})
}

// TestLockingModeExclusiveThroughDriver tests exclusive mode with both drivers
// across different journal modes and access patterns.
func TestLockingModeExclusiveThroughDriver(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		for _, c := range []struct {
			journal, act  string
			bRead, bWrite bool
			shm           bool
		}{
			{"delete", "none", true, true, false},
			{"delete", "read", true, false, false},
			{"delete", "write", false, false, false},
			{"wal", "none", true, true, true},
			{"wal", "read", false, false, false},
			{"wal", "write", false, false, false},
		} {
			drv, c := drv, c
			t.Run(fmt.Sprintf("%s/%s-%s", drv, c.journal, c.act), func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "x.db")
				a, err := sql.Open(drv, p)
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				a.SetMaxOpenConns(1)
				for _, s := range []string{`CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`,
					`PRAGMA journal_mode=` + c.journal, `PRAGMA locking_mode=exclusive`} {
					if _, err := a.Exec(s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
				if got := renderQuery(a, `PRAGMA locking_mode`); got != "[locking_mode][exclusive]" {
					t.Fatalf("did not enter exclusive mode: %s", got)
				}
				switch c.act {
				case "read":
					if _, err := a.Exec(`SELECT count(*) FROM t`); err != nil {
						t.Fatal(err)
					}
				case "write":
					if _, err := a.Exec(`INSERT INTO t VALUES(2)`); err != nil {
						t.Fatal(err)
					}
				}
				bdsn := p + "?_busy_timeout=50"
				if drv == "sqlite" {
					bdsn = p + "?_busy_timeout=50ms"
				}
				b, err := sql.Open(drv, bdsn)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				b.SetMaxOpenConns(1)
				_, rerr := b.Exec(`SELECT count(*) FROM t`)
				_, werr := b.Exec(`INSERT INTO t VALUES(9)`)
				if (rerr == nil) != c.bRead {
					t.Errorf("second connection read: want ok=%v, got %v", c.bRead, rerr)
				}
				if (werr == nil) != c.bWrite {
					t.Errorf("second connection write: want ok=%v, got %v", c.bWrite, werr)
				}
				if drv == "sqlite3" {
					if _, err := os.Stat(p + "-shm"); (err == nil) != c.shm {
						t.Errorf("-shm present=%v, want %v", err == nil, c.shm)
					}
				}
			})
		}
	}
}
