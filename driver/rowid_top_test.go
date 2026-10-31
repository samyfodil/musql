package driver

import (
	"context"
	"database/sql"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"testing"
)

// TestNewRowidIsOnePastTheTop: every rowid a connection hands out is one past
// the largest the table holds as that connection reads it, across other
// connections' commits it catches up with, deletes of the top row (its own, in
// the same transaction, or another connection's), rollbacks, and compactions
// (VACUUM) that move the rows into segments. A largest rowid kept too low hands
// out one a row already has; kept too high, it skips the one C SQLite reuses.
func TestNewRowidIsOnePastTheTop(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		testNewRowidIsOnePastTheTop(t, seed)
	}
}

func testNewRowidIsOnePastTheTop(t *testing.T, seed uint64) {
	ctx := context.Background()
	db, err := sql.Open(DriverName, "file:"+filepath.Join(t.TempDir(), "top.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, pad BLOB)`); err != nil {
		t.Fatal(err)
	}
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		if conns[i], err = db.Conn(ctx); err != nil {
			t.Fatal(err)
		}
		defer conns[i].Close()
	}
	// top is the largest rowid, and 0 for an empty table, whose first rowid is 1.
	top := func(m map[int64]int64) int64 {
		mx, found := int64(0), false
		for id := range m {
			if !found || id > mx {
				mx, found = id, true
			}
		}
		return mx
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	committed := map[int64]int64{}
	step := func(step int64) {
		if rng.IntN(15) == 0 {
			// Every row into segments, the delta empty: the top is now a
			// segment's.
			if _, err := conns[rng.IntN(len(conns))].ExecContext(ctx, `VACUUM`); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := conns[rng.IntN(len(conns))].BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		// A failed step rolls back, or closing its connection waits for it.
		defer tx.Rollback()
		rows := maps.Clone(committed)
		for range 1 + rng.IntN(6) {
			switch r := rng.IntN(10); {
			case r < 4:
				res, err := tx.Exec(`INSERT INTO t(v, pad) VALUES(?, randomblob(1000))`, step)
				if err != nil {
					t.Fatalf("seed %d step %d: insert: %v", seed, step, err)
				}
				id, err := res.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if want := top(rows) + 1; id != want {
					t.Fatalf("seed %d step %d: new rowid %d, want %d", seed, step, id, want)
				}
				rows[id] = step
			case r < 7:
				// The top row or the one under it, mostly; any row now and then.
				id := top(rows) - int64(rng.IntN(2))
				if r == 6 {
					id = rng.Int64N(max(id, 0) + 1)
				}
				res, err := tx.Exec(`DELETE FROM t WHERE id = ?`, id)
				if err != nil {
					t.Fatal(err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					t.Fatal(err)
				}
				_, had := rows[id]
				if (n == 1) != had {
					t.Fatalf("seed %d step %d: delete of %d removed %d rows, had %v", seed, step, id, n, had)
				}
				delete(rows, id)
			case r < 8:
				// A row rewritten where it is moves no top, here or in the
				// connections that catch up with it.
				id := rng.Int64N(max(top(rows), 0) + 1)
				res, err := tx.Exec(`UPDATE t SET v = ? WHERE id = ?`, step, id)
				if err != nil {
					t.Fatal(err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					t.Fatal(err)
				}
				_, had := rows[id]
				if (n == 1) != had {
					t.Fatalf("seed %d step %d: update of %d changed %d rows, had %v", seed, step, id, n, had)
				}
				if had {
					rows[id] = step
				}
			default:
				id := top(rows) + int64(rng.IntN(3)) - 1
				res, err := tx.Exec(`INSERT OR IGNORE INTO t(id, v, pad) VALUES(?, ?, randomblob(1000))`, id, step)
				if err != nil {
					t.Fatal(err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					t.Fatal(err)
				}
				_, had := rows[id]
				if (n == 0) != had {
					t.Fatalf("seed %d step %d: insert of %d added %d rows, had %v", seed, step, id, n, had)
				}
				if !had {
					rows[id] = step
				}
			}
		}
		if rng.IntN(4) == 0 {
			err = tx.Rollback()
		} else if err = tx.Commit(); err == nil {
			committed = rows
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range int64(700) {
		step(i)
	}
	got := map[int64]int64{}
	rs, err := conns[0].QueryContext(ctx, `SELECT id, v FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var id, v int64
		if err := rs.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		got[id] = v
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, committed) {
		t.Fatalf("seed %d: the table holds %d rows, want %d", seed, len(got), len(committed))
	}
}
