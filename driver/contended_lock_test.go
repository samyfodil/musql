package driver

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

// TestContendedWritesLockFirst: once a connection's autocommit write had to be
// retried, its next writes take the write lock before their first attempt --
// they wait for it, which LockWait reports, instead of running and losing
// their commit to the connection committing beside them. Every retry waits
// for the lock too, so more waits than retries are first attempts.
func TestContendedWritesLockFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.musq")
	db, err := sql.Open(DriverName, "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, w INTEGER, v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var waits, retries atomic.Int64
	BusyRetryAttemptHookForTest = func() { retries.Add(1) }
	defer func() { BusyRetryAttemptHookForTest = nil }()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(dc any) error {
		dc.(*Conn).SetCaptureHooks(&CaptureHooks{LockWait: func(waiting bool) {
			if waiting {
				waits.Add(1)
			}
		}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The other writer commits back to back in explicit transactions, which
	// wait for the lock rather than retry: every retry counted is conn's.
	other, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; !done.Load(); i++ {
			for _, q := range []string{`BEGIN IMMEDIATE`, `INSERT INTO t(w, v) VALUES(1, ?)`, `COMMIT`} {
				var err error
				if q[0] == 'I' {
					_, err = other.ExecContext(t.Context(), q, i)
				} else {
					_, err = other.ExecContext(t.Context(), q)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	for range 20 {
		if _, err := conn.ExecContext(t.Context(), `WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < 3000)
			INSERT INTO t(w, v) SELECT 0, i FROM s`); err != nil {
			t.Fatal(err)
		}
	}
	done.Store(true)
	wg.Wait()
	if retries.Load() == 0 {
		t.Skip("no write was ever retried: nothing contended")
	}
	if waits.Load() <= retries.Load() {
		t.Fatalf("%d lock waits for %d retries: no first attempt took the lock", waits.Load(), retries.Load())
	}
}

// TestHandOffLetsGoOfTheWriteLock: a commit handed off by a write holding the
// write lock -- a contended connection's autocommit write takes it before it
// runs -- lets go of the lock first: the hand-off lands the changes through a
// connection that needs it.
func TestHandOffLetsGoOfTheWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.musq")
	db, err := sql.Open(DriverName, "file:"+path+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	other, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := conn.Raw(func(dc any) error {
		c := dc.(*Conn)
		c.SetCaptureHooks(&CaptureHooks{
			PreCommit: func(context.Context, *Conn, []engine.RowChange) error { return ErrHandOff },
			HandOff: func() error {
				_, err := other.ExecContext(ctx, `INSERT INTO t VALUES('handed off')`)
				return err
			},
		})
		c.contendedUntil = time.Now().Add(time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `CREATE TABLE t(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO t VALUES('discarded')`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := other.QueryRowContext(ctx, `SELECT group_concat(v) FROM t`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "handed off" {
		t.Fatalf("got %q", got)
	}
}
