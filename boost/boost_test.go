package boost

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"
)

type valuer struct{ v int64 }

func (v valuer) Value() (sqldriver.Value, error) { return v.v, nil }

const timeDSN = "?_time_decltype=1"

func seed(t *testing.T) (path string, std *sql.DB) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "b.musq")
	std, err := sql.Open("sqlite", path+timeDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { std.Close() })
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n, ts DATETIME)`,
		`INSERT INTO t VALUES(1, 42, 1.5, 'one', x'0102', NULL, '2024-01-02 03:04:05')`,
		`INSERT INTO t VALUES(2, -7, 2.0, '', x'', 5, NULL)`,
		`INSERT INTO t VALUES(3, NULL, NULL, NULL, NULL, 'x', '2023-12-31 23:59:59')`,
		`CREATE TABLE kv(k INTEGER PRIMARY KEY, v INTEGER)`,
	} {
		if _, err := std.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path, std
}

// both runs f against Open's database and against Wrap's.
func both(t *testing.T, f func(t *testing.T, db *DB, std *sql.DB)) {
	t.Run("open", func(t *testing.T) {
		path, std := seed(t)
		db, err := Open(path + timeDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		f(t, db, std)
	})
	t.Run("wrap", func(t *testing.T) {
		_, std := seed(t)
		db, err := Wrap(std)
		if err != nil {
			t.Fatal(err)
		}
		f(t, db, std)
	})
}

func stdRows(t *testing.T, std *sql.DB, q string, args ...any) [][]any {
	t.Helper()
	rows, err := std.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func boostRows(t *testing.T, db *DB, q string, args ...any) [][]any {
	t.Helper()
	rows, err := db.Query(context.Background(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := rows.Columns()
	var out [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestQueryMatchesDatabaseSQL: every storage class, NULL, time coercion,
// named parameters and a Valuer read back as database/sql reads them.
func TestQueryMatchesDatabaseSQL(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		for _, c := range []struct {
			q    string
			args []any
		}{
			{`SELECT * FROM t ORDER BY id`, nil},
			{`SELECT * FROM t WHERE id = ?`, []any{2}},
			{`SELECT * FROM t WHERE id = :id`, []any{sql.Named("id", 3)}},
			{`SELECT * FROM t WHERE id = ?`, []any{valuer{1}}},
			{`SELECT id, s FROM t WHERE s = ?`, []any{"one"}},
			{`SELECT count(*), sum(r), group_concat(s) FROM t`, nil},
			{`SELECT * FROM t WHERE id > 99`, nil},
		} {
			want := stdRows(t, std, c.q, c.args...)
			got := boostRows(t, db, c.q, c.args...)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %v:\n got %#v\nwant %#v", c.q, c.args, got, want)
			}
		}
	})
}

// TestTypedScanMatchesDatabaseSQL: each typed destination gets what
// database/sql's Scan puts there, and an unsupported one is an error.
func TestTypedScanMatchesDatabaseSQL(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		ctx := context.Background()
		type rec struct {
			id int64
			i  int
			r  float64
			s  sql.NullString
			b  []byte
			ts time.Time
		}
		const q = `SELECT id, i, r, s, b, ts FROM t WHERE id <= 2 ORDER BY id`
		var want, got []rec
		rows, err := std.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x rec
			if err := rows.Scan(&x.id, &x.i, &x.r, &x.s, &x.b, &x.ts); err != nil {
				break // row 2's NULL ts, below
			}
			want = append(want, x)
		}
		rows.Close()
		brows, err := db.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for {
			var x rec
			if !brows.Next(&x.id, &x.i, &x.r, &x.s, &x.b, &x.ts) {
				break
			}
			got = append(got, x)
		}
		// Row 2's ts is NULL, which neither scans into a time.Time.
		if err := brows.Err(); err == nil || !strings.Contains(err.Error(), "NULL") {
			t.Fatalf("NULL into *time.Time: err = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("typed scan:\n got %#v\nwant %#v", got, want)
		}

		var u uint8
		err = db.QueryRow(ctx, `SELECT 1`).Scan(&u)
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("*uint8: err = %v", err)
		}
		var n int64
		if err := db.QueryRow(ctx, `SELECT id FROM t WHERE id = 99`).Scan(&n); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("no row: err = %v", err)
		}
		if err := db.QueryRow(ctx, `SELECT id FROM t WHERE id = ?`, 3).Scan(&n); err != nil || n != 3 {
			t.Fatalf("QueryRow = %d, %v", n, err)
		}
		if err := db.QueryRow(ctx, `SELECT id, i FROM t`).Scan(&n); err == nil {
			t.Fatal("too few destinations: no error")
		}
		if _, err := db.Query(ctx, `SELECT nope FROM t`); err == nil {
			t.Fatal("bad query: no error")
		}
		if _, err := db.Query(ctx, `SELECT id FROM t WHERE id = ?`); err == nil {
			t.Fatal("missing argument: no error")
		}
	})
}

// TestExecAndTx: Exec writes, a transaction reads its own writes, Rollback
// undoes them and Commit keeps them, all seen by database/sql afterwards.
func TestExecAndTx(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		ctx := context.Background()
		res, err := db.Exec(ctx, `INSERT INTO t(id, s) VALUES(?, ?)`, 10, "ten")
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("RowsAffected = %d", n)
		}
		var s string
		if err := std.QueryRow(`SELECT s FROM t WHERE id = 10`).Scan(&s); err != nil || s != "ten" {
			t.Fatalf("database/sql after Exec: %q, %v", s, err)
		}

		count := func(q interface {
			QueryRow(context.Context, string, ...any) *Row
		}) int64 {
			var n int64
			if err := q.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := count(db)
		tx, err := db.Begin(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO t(id) VALUES(11)`); err != nil {
			t.Fatal(err)
		}
		if n := count(tx); n != before+1 {
			t.Fatalf("tx sees %d rows, want %d", n, before+1)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Commit after Rollback: %v", err)
		}
		if _, err := tx.Query(ctx, `SELECT 1`); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("Query after Rollback: %v", err)
		}
		if n := count(db); n != before {
			t.Fatalf("after Rollback %d rows, want %d", n, before)
		}

		tx, err = db.Begin(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO t(id) VALUES(12)`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var n int64
		if err := std.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != before+1 {
			t.Fatalf("database/sql after Commit: %d, %v", n, err)
		}
		if n := count(db); n != before+1 {
			t.Fatalf("boost after Commit: %d", n)
		}
	})
}

// TestPoolLimits: with one connection, a second query waits for the first
// rows to be released, a waiter gives up when its context ends, and Close
// refuses new queries.
func TestPoolLimits(t *testing.T) {
	path, _ := seed(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	held, err := db.Query(ctx, `SELECT id FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := db.Query(short, `SELECT 1`); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter past its deadline: %v", err)
	}

	got := make(chan int64, 1)
	go func() {
		var n int64
		if err := db.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
			t.Error(err)
		}
		got <- n
	}()
	select {
	case <-got:
		t.Fatal("second query ran while the only connection was held")
	case <-time.After(50 * time.Millisecond):
	}
	for held.Next() {
	}
	select {
	case n := <-got:
		if n != 3 {
			t.Fatalf("count = %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never got the released connection")
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				var n int64
				if err := db.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 3 {
					t.Errorf("concurrent count = %d, %v", n, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, `SELECT 1`); !errors.Is(err, errClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

// TestCrossConnectionReads: a write through one connection is seen by a
// query on another.
func TestCrossConnectionReads(t *testing.T) {
	path, std := seed(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	held, err := db.Query(ctx, `SELECT 1`) // keeps one connection busy
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for i := range 20 {
		id := 100 + i
		if _, err := std.Exec(`INSERT INTO t(id) VALUES(?)`, id); err != nil {
			t.Fatal(err)
		}
		var n int64
		if err := db.QueryRow(ctx, `SELECT count(*) FROM t WHERE id = ?`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("row %d: %d, %v", id, n, err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO t(id) VALUES(?)`, id+1000); err != nil {
			t.Fatal(err)
		}
		if err := std.QueryRow(`SELECT count(*) FROM t WHERE id = ?`, id+1000).Scan(&n); err != nil || n != 1 {
			t.Fatalf("row %d via database/sql: %d, %v", id+1000, n, err)
		}
	}
}
