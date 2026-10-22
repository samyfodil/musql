package driver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fastRow struct {
	ID    int64
	Int   *int64   `db:"i"`
	Real  *float64 `db:"r"`
	S     sql.NullString
	B     []byte
	N     any
	TS    *time.Time `db:"ts"`
	Extra string     `db:"-"`
}

// TestQueryStructMatchesDatabaseSQL: QueryStruct fills each field with what
// database/sql scans into the same destination, NULLs included, and refuses a
// column or field it cannot pair rather than leave a zero value behind.
func TestQueryStructMatchesDatabaseSQL(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "s.musq")+"?_time_decltype=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n, ts DATETIME)`,
		`INSERT INTO t VALUES(1, 42, 1.5, 'one', x'0102', NULL, '2024-01-02 03:04:05')`,
		`INSERT INTO t VALUES(2, -7, 2.0, '', x'', 5, NULL)`,
		`INSERT INTO t VALUES(3, NULL, NULL, NULL, NULL, 'x', '2023-12-31 23:59:59')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const q = `SELECT id, i, r, s, b, n, ts FROM t WHERE id >= ? ORDER BY id`
	var want []fastRow
	rows, err := db.Query(q, 1)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r fastRow
		if err := rows.Scan(&r.ID, &r.Int, &r.Real, &r.S, &r.B, &r.N, &r.TS); err != nil {
			t.Fatal(err)
		}
		want = append(want, r)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st := PrepareFast(conn, q)
	for range 2 {
		var got []fastRow
		if err := QueryStruct(ctx, st, []any{1}, func(r *fastRow) error {
			got = append(got, *r)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("QueryStruct:\n got %+v\nwant %+v", got, want)
		}
	}
	one, err := QueryRowStruct[fastRow](ctx, st, []any{2})
	if err != nil || !reflect.DeepEqual(one, want[1]) {
		t.Fatalf("QueryRowStruct = %+v, %v; want %+v", one, err, want[1])
	}
	if _, err := QueryRowStruct[fastRow](ctx, st, []any{9}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no row: %v, want sql.ErrNoRows", err)
	}

	type plain struct {
		ID int64
		I  int64
	}
	for _, c := range []struct {
		name, sql, want string
		run             func(*FastStmt) error
	}{
		{"column without field", `SELECT id, i, s FROM t`, `column "s" has no field`, nil},
		{"field without column", `SELECT id FROM t`, `field driver.plain.I has no column`, nil},
		{"repeated column", `SELECT id, i, i FROM t`, `repeats a column`, nil},
		{"NULL into int64", `SELECT id, i FROM t WHERE id = 3`, `converting NULL`, nil},
		{"not a struct", `SELECT id FROM t`, `needs a struct type`, func(st *FastStmt) error {
			_, err := QueryRowStruct[int64](ctx, st, nil)
			return err
		}},
	} {
		st := PrepareFast(conn, c.sql)
		run := c.run
		if run == nil {
			run = func(st *FastStmt) error {
				_, err := QueryRowStruct[plain](ctx, st, nil)
				return err
			}
		}
		if err := run(st); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
