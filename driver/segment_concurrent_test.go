package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestSegmentCrossConnectionReadAfterWrite verifies that rows committed by one
// connection are immediately visible to all others. Held sessions must refresh.
func TestSegmentCrossConnectionReadAfterWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xconn.musq")
	a, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a.SetMaxOpenConns(4)
	b.SetMaxOpenConns(4)
	if _, err := a.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 200; i++ {
		if _, err := a.Exec(`INSERT INTO t VALUES(?,?)`, i, fmt.Sprint("v", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		var v string
		if err := b.QueryRow(`SELECT v FROM t WHERE id=?`, i).Scan(&v); err != nil {
			t.Fatalf("row %d on the OTHER pool: %v", i, err)
		}
		if err := a.QueryRow(`SELECT v FROM t WHERE id=?`, i).Scan(&v); err != nil {
			t.Fatalf("row %d on the SAME pool: %v", i, err)
		}
	}
}

// TestSegmentConcurrentWritersLoseNothing: two pools writing at the same time, and
// every row either connection was TOLD it committed is in the file at the end.
//
// The sequential test above cannot see this class, because a statement that starts
// after the other connection's commit revalidates and is never stale. The loss
// needs the other commit to land WHILE a statement is in flight -- then the
// records it appends were computed from rows it loaded before that commit, and the
// segment delta's records are absolute puts and kills by rowid, so a stale one does
// not merely arrive late, it is wrong about the row it replaces. Two replicated
// nodes lost rows exactly this way (a materializeRow REPLACE appending "kill
// users/1, put users/3" while users/1 had become a different row), which is what
// the engine's stale-image refusal and the retry's own revalidation now prevent --
// and both are needed: without the refusal the record lands, and without the
// revalidation the RETRY re-runs over the refused attempt's own applied rows and
// reports "UNIQUE constraint failed" for a row nobody committed.
//
// Probabilistic by nature, so it is written to make the window wide: interleaved
// writers on their own pools, each one also REPLACING rows to move rowids around.
func TestSegmentConcurrentWritersLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conc.musq")
	seed, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	const writers = 4
	const perWriter = 60
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			db, oerr := sql.Open(DriverName, path) // its own pool, like a separate process
			if oerr != nil {
				errs <- oerr
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("w%d-%d", w, i)
				if _, eerr := db.Exec(`INSERT INTO t VALUES(?,?)`, id, "one"); eerr != nil {
					errs <- fmt.Errorf("writer %d insert %s: %w", w, id, eerr)
					return
				}
				// REPLACE moves the row to a NEW rowid, which is what makes a stale
				// record destructive rather than late.
				if _, eerr := db.Exec(`INSERT OR REPLACE INTO t VALUES(?,?)`, id, "two"); eerr != nil {
					errs <- fmt.Errorf("writer %d replace %s: %w", w, id, eerr)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}

	// Every writer's every row, read by a connection that saw none of the writing.
	check, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var n int
	if err := check.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != writers*perWriter {
		t.Errorf("the file holds %d rows, want %d -- a committed row was lost", n, writers*perWriter)
	}
	rows, err := check.Query(`SELECT id, v FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		var id, v string
		if serr := rows.Scan(&id, &v); serr != nil {
			t.Fatal(serr)
		}
		seen[id] = v
	}
	// The whole missing set, per writer, not the first one: which rows are gone
	// says what lost them -- a scatter is a stale record, a whole writer's run is a
	// file rewrite that dropped batches it had not loaded.
	missing := map[int][]int{}
	stale := map[int][]int{}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			id := fmt.Sprintf("w%d-%d", w, i)
			if v, ok := seen[id]; !ok {
				missing[w] = append(missing[w], i)
			} else if v != "two" {
				stale[w] = append(stale[w], i)
			}
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		for w := 0; w < writers; w++ {
			if len(missing[w]) > 0 {
				t.Errorf("writer %d: %d of %d rows MISSING, though every INSERT reported success: %v",
					w, len(missing[w]), perWriter, missing[w])
			}
			if len(stale[w]) > 0 {
				t.Errorf("writer %d: %d rows kept the pre-REPLACE value, though every REPLACE reported success: %v",
					w, len(stale[w]), stale[w])
			}
		}
	}
}
