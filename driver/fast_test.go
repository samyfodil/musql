package driver

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fastValuer struct{ v int64 }

func (f fastValuer) Value() (sqldriver.Value, error) { return f.v, nil }

// TestFastStmtMatchesDatabaseSQL: a FastStmt answers what db.Query answers,
// for every storage class, NULL, named parameters, a Valuer, inside a
// transaction, and with the connection's time coercion.
func TestFastStmtMatchesDatabaseSQL(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "f.musq")+"?_time_decltype=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n, ts DATETIME)`,
		`INSERT INTO t VALUES(1, 42, 1.5, 'one', x'0102', NULL, '2024-01-02 03:04:05')`,
		`INSERT INTO t VALUES(2, -7, 2.0, '', x'', 5, NULL)`,
		`INSERT INTO t VALUES(3, 1, NULL, 'three', NULL, 'x', '2023-12-31 23:59:59')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	all := func(t *testing.T, query string, args ...any) [][]any {
		t.Helper()
		var out [][]any
		st := PrepareFast(conn, query)
		err := st.Query(ctx, args, func(scan func(dest ...any) error) error {
			row := make([]any, 7)
			ptrs := make([]any, len(row))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := scan(ptrs[:cap(ptrs)]...); err != nil {
				return err
			}
			out = append(out, row)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return out
	}
	ref := func(t *testing.T, query string, args ...any) [][]any {
		t.Helper()
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][]any
		for rows.Next() {
			row := make([]any, 7)
			ptrs := make([]any, len(row))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		return out
	}
	for _, c := range []struct {
		q    string
		args []any
	}{
		{`SELECT * FROM t ORDER BY id`, nil},
		{`SELECT * FROM t WHERE id > ?`, []any{1}},
		{`SELECT * FROM t WHERE i = :v OR id = :v`, []any{sql.Named("v", 1)}},
		{`SELECT * FROM t WHERE i = ?`, []any{fastValuer{42}}},
		{`SELECT * FROM t WHERE s = ?`, []any{"three"}},
		{`SELECT * FROM t WHERE id = ?`, []any{uint8(2)}},
	} {
		if got, want := all(t, c.q, c.args...), ref(t, c.q, c.args...); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %v:\n got %#v\nwant %#v", c.q, c.args, got, want)
		}
	}

	// Typed destinations.
	var (
		id   int64
		i    int
		r    float64
		s    string
		b    []byte
		ts   time.Time
		nstr sql.NullString
	)
	st := PrepareFast(conn, `SELECT id, i, r, s, b, n, ts FROM t WHERE id = ?`)
	if err := st.QueryRow(ctx, []any{1}, &id, &i, &r, &s, &b, &nstr, &ts); err != nil {
		t.Fatal(err)
	}
	if id != 1 || i != 42 || r != 1.5 || s != "one" || !reflect.DeepEqual(b, []byte{1, 2}) || nstr.Valid ||
		!ts.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("got %v %v %v %q %v %v %v", id, i, r, s, b, nstr, ts)
	}
	if err := st.QueryRow(ctx, []any{99}, &id, &i, &r, &s, &b, &nstr, &ts); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing row: %v", err)
	}
	if err := st.QueryRow(ctx, []any{3}, &id, &i, &r, &s, &b, &nstr, &ts); err == nil || !strings.Contains(err.Error(), "NULL") {
		t.Fatalf("NULL into float64: %v", err)
	}
	if err := st.QueryRow(ctx, nil, &id); err == nil {
		t.Fatal("missing argument accepted")
	}
	if err := PrepareFast(conn, `SELECT 1; SELECT 2`).QueryRow(ctx, nil, &id); err == nil {
		t.Fatal("a script accepted")
	}

	// Inside a transaction on the same connection: its own writes are seen.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t(id, i) VALUES(10, 100)`); err != nil {
		t.Fatal(err)
	}
	if err := PrepareFast(conn, `SELECT i FROM t WHERE id = 10`).QueryRow(ctx, nil, &i); err != nil || i != 100 {
		t.Fatalf("in transaction: %v %v", i, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := PrepareFast(conn, `SELECT i FROM t WHERE id = 10`).QueryRow(ctx, nil, &i); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after rollback: %v", err)
	}
}
