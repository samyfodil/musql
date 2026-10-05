package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestBusyTimeoutIsHonoured verifies that PRAGMA busy_timeout and the DSN
// parameter control how long statements wait on locks.
func TestBusyTimeoutIsHonoured(t *testing.T) {
	for _, drv := range []string{"sqlite3", "sqlite"} {
		db, err := sql.Open(drv, filepath.Join(t.TempDir(), "g.db")+"?_busy_timeout=250")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		a := renderQuery(db, `PRAGMA busy_timeout`)
		db.Exec(`PRAGMA busy_timeout=120`)
		b := renderQuery(db, `PRAGMA busy_timeout`)
		db.Close()
		if a != "[timeout][250]" || b != "[timeout][120]" {
			t.Errorf("%s: busy_timeout reads %s then %s, want 250 then 120", drv, a, b)
		}
	}

	for _, via := range []string{"dsn", "pragma"} {
		t.Run(via, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.db")
			holder, err := sql.Open("sqlite", p)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			holder.SetMaxOpenConns(1)
			for _, s := range []string{`CREATE TABLE t(x)`, `PRAGMA locking_mode=exclusive`, `INSERT INTO t VALUES(1)`} {
				if _, err := holder.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			dsn := p
			if via == "dsn" {
				dsn += "?_busy_timeout=100"
			}
			other, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.SetMaxOpenConns(1)
			if via == "pragma" {
				if _, err := other.Exec(`PRAGMA busy_timeout=100`); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			_, werr := other.Exec(`INSERT INTO t VALUES(2)`)
			took := time.Since(start)
			if werr == nil {
				t.Fatal("the blocked write succeeded; the exclusive lock was not held")
			}
			if took > 2*time.Second {
				t.Errorf("the blocked write waited %v with busy_timeout=100ms (the fixed 5s default, not the connection's)", took)
			}
		})
	}
}
