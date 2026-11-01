package driver

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRetriedWriteIsNotStarved: a retried autocommit statement holds the write
// lock, so a connection committing back to back cannot make it lose every
// attempt until its busy timeout.
func TestRetriedWriteIsNotStarved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starve.musq")
	var dbs []*sql.DB
	for range 2 {
		db, err := sql.Open(DriverName, "file:"+path+"?_pragma=busy_timeout(3000)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		defer db.Close()
		dbs = append(dbs, db)
	}
	if _, err := dbs[0].Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, w INTEGER, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	const loads, rows = 10, 3000
	var done atomic.Bool
	var quick int
	var quickErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		for ; !done.Load(); quick++ {
			if _, err := dbs[1].Exec(`INSERT INTO t(w, v) VALUES(1, ?)`, quick); err != nil {
				quickErr = err
				return
			}
		}
	})
	var loadErr error
	for range loads {
		if _, loadErr = dbs[0].Exec(`WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
			INSERT INTO t(w, v) SELECT 0, i FROM s`, rows); loadErr != nil {
			break
		}
	}
	done.Store(true)
	wg.Wait()
	if loadErr != nil {
		t.Fatalf("the long statement beside a connection committing back to back: %v", loadErr)
	}
	if quickErr != nil {
		t.Fatalf("the short statements: %v", quickErr)
	}
	var got int
	if err := dbs[0].QueryRow(`SELECT count(*) FROM t`).Scan(&got); err != nil || got != loads*rows+quick {
		t.Fatalf("count(*) = %d, %v; want %d", got, err, loads*rows+quick)
	}
}
