package boost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStressReturnsEveryConnection mixes every path -- reads, both scan
// styles, QueryStruct broken off early, scan errors, failing queries, Exec,
// committed and rolled-back transactions, cancelled waits and limit changes
// -- across goroutines on a small pool. Run under -race it is the race pass;
// afterwards every connection must be back, the pool within its limits, and
// no goroutine left behind.
func TestStressReturnsEveryConnection(t *testing.T) {
	path, _ := seed(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(3)
	goroutines := runtime.NumGoroutine()
	// A leaked connection shows as waits that never end; the deadline turns
	// that into a failure instead of a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := range 150 {
				if err := stressStep(ctx, db, rng, g*1000+i); err != nil {
					errs <- fmt.Errorf("goroutine %d step %d: %w", g, i, err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			db.SetMaxIdleConns(i % 3)
			db.SetMaxOpenConns(2 + i%3)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	p := db.pool
	p.mu.Lock()
	open, idle, waiters := p.open, len(p.idle), len(p.waiters)
	p.mu.Unlock()
	if open != idle || waiters != 0 || idle > p.maxIdle {
		t.Fatalf("after the workload: open=%d idle=%d waiters=%d maxIdle=%d", open, idle, waiters, p.maxIdle)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if p.open != 0 {
		t.Fatalf("after Close: open=%d", p.open)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > goroutines {
		t.Fatalf("goroutines: %d before, %d after", goroutines, n)
	}
}

func stressStep(ctx context.Context, db *DB, rng *rand.Rand, id int) error {
	switch rng.Intn(9) {
	case 0: // Next with destinations, to the end
		rows, err := db.Query(ctx, `SELECT id, s FROM t WHERE id <= 2 ORDER BY id`)
		if err != nil {
			return err
		}
		var n int64
		var s sql.NullString
		for rows.Next(&n, &s) {
		}
		return rows.Err()
	case 1: // Next then Scan, closed early
		rows, err := db.Query(ctx, `SELECT id FROM t`)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			var n int64
			return rows.Scan(&n)
		}
		return rows.Err()
	case 2: // QueryStruct broken off after one row
		type r struct{ ID int64 }
		for _, err := range QueryStruct[r](ctx, db, `SELECT id FROM t`) {
			return err
		}
		return nil
	case 3: // a scan error ends Next and releases the connection
		rows, err := db.Query(ctx, `SELECT i FROM t WHERE id = 3`)
		if err != nil {
			return err
		}
		var n int64
		if rows.Next(&n) || rows.Err() == nil {
			return errors.New("NULL into *int64 was not an error")
		}
		return nil
	case 4: // a failing query
		if _, err := db.Query(ctx, `SELECT nope FROM t`); err == nil {
			return errors.New("bad column: no error")
		}
		return nil
	case 5:
		_, err := db.Exec(ctx, `INSERT OR REPLACE INTO kv(k, v) VALUES(?, ?)`, id%50, id)
		if err != nil && !isBusy(err) {
			return err
		}
		return nil
	case 6, 7: // a transaction, committed or rolled back
		tx, err := db.Begin(ctx, nil)
		if err != nil {
			if isBusy(err) {
				return nil
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT OR REPLACE INTO kv(k, v) VALUES(?, ?)`, id%50, -id); err != nil {
			tx.Rollback()
			if isBusy(err) {
				return nil
			}
			return err
		}
		var v int64
		if err := tx.QueryRow(ctx, `SELECT v FROM kv WHERE k = ?`, id%50).Scan(&v); err != nil || v != int64(-id) {
			tx.Rollback()
			return fmt.Errorf("tx reads its own write: %d, %v", v, err)
		}
		if rng.Intn(2) == 0 {
			if err := tx.Commit(); err != nil && !isBusy(err) {
				return err
			}
			return nil
		}
		return tx.Rollback()
	default: // a wait cut short by its context
		short, cancel := context.WithTimeout(ctx, time.Duration(rng.Intn(200))*time.Microsecond)
		defer cancel()
		rows, err := db.Query(short, `SELECT count(*) FROM t`)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil
			}
			return err
		}
		rows.Close()
		return nil
	}
}

func isBusy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy"))
}
