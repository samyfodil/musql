package driver_test

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"
)

// TestNoFromPlanCached runs the same FROM-less reads again and again across
// bindings, writes, transactions, a schema change and the clock: the plan
// kept for each text must answer what a fresh compile answers.
func TestNoFromPlanCached(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "n.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run := func(q string, args ...any) ([]int64, error) {
		rows, err := db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		var out []int64
		for rows.Next() {
			vals := make([]sql.NullInt64, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			for _, v := range vals {
				out = append(out, v.Int64)
			}
		}
		return out, rows.Err()
	}
	ints := func(q string, args ...any) []int64 {
		t.Helper()
		out, err := run(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}
	want := func(q string, got []int64, w ...int64) {
		t.Helper()
		if len(got) != len(w) {
			t.Fatalf("%s: got %v, want %v", q, got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("%s: got %v, want %v", q, got, w)
			}
		}
	}

	const bound = "SELECT ?1 * 2, ?2 IS NULL, coalesce(?2, -1)"
	for _, n := range []int64{3, -8, 1 << 40} {
		want(bound, ints(bound, n, nil), 2*n, 1, -1)
		want(bound, ints(bound, n, n+1), 2*n, 0, n+1)
	}

	exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)")
	const state = "SELECT changes(), last_insert_rowid(), total_changes()"
	exec("INSERT INTO t(k) VALUES(1)")
	want(state, ints(state), 1, 1, 1)
	exec("INSERT INTO t(k) VALUES(2), (3)")
	want(state, ints(state), 2, 3, 3)
	exec("UPDATE t SET k = k + 10")
	want(state, ints(state), 3, 3, 6)

	const data = "SELECT (SELECT count(*) FROM t), EXISTS (SELECT 1 FROM t WHERE k = ?1)"
	want(data, ints(data, 12), 3, 1)
	want(data, ints(data, 99), 3, 0)
	exec("INSERT INTO t(k) VALUES(99)")
	want(data, ints(data, 99), 4, 1)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM t WHERE k = 99"); err != nil {
		t.Fatal(err)
	}
	var n, e int64
	if err := tx.QueryRow(data, 99).Scan(&n, &e); err != nil || n != 3 || e != 0 {
		t.Fatalf("%s inside a transaction: %d %d %v", data, n, e, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	want(data, ints(data, 99), 4, 1)
	exec("DROP TABLE t")
	if _, err := run(data, 1); err == nil {
		t.Fatal("a read of a dropped table succeeded")
	}
	exec("CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER)")
	want(data, ints(data, 1), 0, 0)

	// A LIMIT read from the data is folded into the compile, so a plan that
	// folded one must not answer the next run, in the same transaction, after
	// a write moves it.
	const limited = "SELECT 1 LIMIT (SELECT count(*) FROM t WHERE k > 100)"
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	limitRows := func() int {
		t.Helper()
		rows, err := tx.Query(limited)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	none := limitRows()
	if _, err := tx.Exec("INSERT INTO t(k) VALUES(101)"); err != nil {
		t.Fatal(err)
	}
	if one := limitRows(); none != 0 || one != 1 {
		t.Fatalf("%s in one transaction: %d rows, then %d after a row matched; want 0, then 1", limited, none, one)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	const clock = "SELECT CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"
	before := ints(clock)[0]
	time.Sleep(30 * time.Millisecond)
	if after := ints(clock)[0]; after <= before {
		t.Fatalf("%s: %d, then %d after 30ms", clock, before, after)
	}
	const rnd = "SELECT random()"
	if a, b := ints(rnd)[0], ints(rnd)[0]; a == b {
		t.Fatalf("%s twice: %d both times", rnd, a)
	}

	// A GROUP BY term is checked against the bindings before the compile:
	// the check stands on the second run as on the first.
	const grouped = "SELECT 1 GROUP BY abs(?1)"
	_, first := run(grouped, int64(math.MinInt64))
	want(grouped, ints(grouped, 1), 1)
	if _, again := run(grouped, int64(math.MinInt64)); (again == nil) != (first == nil) {
		t.Fatalf("%s bound to the minimum: %v at first, %v after a run", grouped, first, again)
	}
}
