package boost

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDestinationMatrixMatchesDatabaseSQL scans every value class into every
// destination type through both database/sql and boost. Where both succeed
// the values must be equal, and where database/sql refuses boost must refuse
// too: boost may be stricter, never laxer.
func TestDestinationMatrixMatchesDatabaseSQL(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		ctx := context.Background()
		values := []string{
			`0`, `1`, `5`, `-1`, `2147483648`, `-2147483649`, `9223372036854775807`,
			`1.5`, `-0.0`, `1e300`, `2.0`,
			`'text'`, `''`, `'42'`, `'true'`, `'2024-01-02 03:04:05'`,
			`x'00ff'`, `x''`, `NULL`,
			`(SELECT ts FROM t WHERE id = 1)`,
		}
		dests := []func() any{
			func() any { return new(int64) },
			func() any { return new(int) },
			func() any { return new(int32) },
			func() any { return new(float64) },
			func() any { return new(bool) },
			func() any { return new(string) },
			func() any { return new([]byte) },
			func() any { return new(time.Time) },
			func() any { return new(any) },
			func() any { return new(sql.NullString) },
			func() any { return new(sql.NullInt64) },
			func() any { return new(sql.NullFloat64) },
			func() any { return new(sql.NullBool) },
			func() any { return new(sql.NullTime) },
		}
		for _, v := range values {
			q := `SELECT ` + v
			for _, mk := range dests {
				want, got := mk(), mk()
				werr := std.QueryRow(q).Scan(want)
				gerr := db.QueryRow(ctx, q).Scan(got)
				name := fmt.Sprintf("%s into %T", v, want)
				switch {
				case werr != nil && gerr == nil:
					t.Errorf("%s: database/sql refuses (%v), boost gives %#v", name, werr, reflect.ValueOf(got).Elem().Interface())
				case werr == nil && gerr == nil && !reflect.DeepEqual(got, want):
					t.Errorf("%s: boost %#v, database/sql %#v", name, reflect.ValueOf(got).Elem().Interface(), reflect.ValueOf(want).Elem().Interface())
				}
			}
		}
	})
}

// TestStringsOutliveTheirRows: strings cut from a shared chunk keep their
// bytes after the rows close and the connection runs other queries, across
// enough rows to span several chunks.
func TestStringsOutliveTheirRows(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		ctx := context.Background()
		const q = `WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 100)
			SELECT i, 'row-' || i || '-' || hex(randomblob(i % 7)), CAST('b' || i AS BLOB), '' FROM c`
		rows, err := db.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for {
			var i int64
			var s, b, empty string
			if !rows.Next(&i, &s, &b, &empty) {
				break
			}
			if !strings.HasPrefix(s, fmt.Sprintf("row-%d-", i)) || b != fmt.Sprintf("b%d", i) || empty != "" {
				t.Fatalf("row %d: %q %q %q", i, s, b, empty)
			}
			kept = append(kept, s, b)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		snapshot := append([]string(nil), kept...)
		for i := range snapshot {
			snapshot[i] = strings.Clone(snapshot[i])
		}
		for range 20 {
			var s string
			if err := db.QueryRow(ctx, `SELECT 'overwrite-' || hex(randomblob(64))`).Scan(&s); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(kept, snapshot) {
			t.Fatal("a string changed after its rows were closed")
		}
	})
}

// TestArgumentErrors: the argument errors database/sql reports are reported.
func TestArgumentErrors(t *testing.T) {
	both(t, func(t *testing.T, db *DB, std *sql.DB) {
		ctx := context.Background()
		for _, c := range []struct {
			name string
			args []any
		}{
			{"too many", []any{1, 2}},
			{"unsupported type", []any{struct{}{}}},
			{"failing Valuer", []any{badValuer{}}},
		} {
			_, werr := std.Query(`SELECT ?`, c.args...)
			_, gerr := db.Query(ctx, `SELECT ?`, c.args...)
			_, eerr := db.Exec(ctx, `SELECT ?`, c.args...)
			if werr == nil || gerr == nil || eerr == nil {
				t.Errorf("%s: database/sql %v, boost Query %v, Exec %v", c.name, werr, gerr, eerr)
			}
		}
		if _, err := db.Exec(ctx, `INSERT INTO nope VALUES(1)`); err == nil {
			t.Error("Exec on a missing table: no error")
		}
		if _, err := db.Exec(ctx, `SELEC 1`); err == nil {
			t.Error("Exec of bad SQL: no error")
		}
		done, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := db.Query(done, `SELECT 1`); !errors.Is(err, context.Canceled) {
			t.Errorf("Query on a cancelled ctx: %v", err)
		}
		if _, err := db.Exec(done, `SELECT 1`); !errors.Is(err, context.Canceled) {
			t.Errorf("Exec on a cancelled ctx: %v", err)
		}
		if _, err := db.Begin(done, nil); err == nil {
			t.Error("Begin on a cancelled ctx: no error")
		}
		if r := db.QueryRow(ctx, `SELECT nope`); r.Err() == nil {
			t.Error("Row.Err of a failed query: nil")
		}
		rows, err := db.Query(ctx, `SELECT 1 AS a, 2 AS b`)
		if err != nil {
			t.Fatal(err)
		}
		if cols := rows.Columns(); !reflect.DeepEqual(cols, []string{"a", "b"}) {
			t.Errorf("Columns = %v", cols)
		}
		var a int64
		if err := rows.Scan(&a); err == nil {
			t.Error("Scan before Next: no error")
		}
		rows.Close()
		if rows.Columns() != nil || rows.Next() || rows.Close() != nil {
			t.Error("closed rows still answer")
		}
	})
}

type badValuer struct{}

func (badValuer) Value() (sqldriver.Value, error) { return nil, errors.New("valuer failed") }

// TestPoolFailurePaths: a failed dial gives its slot back, a bad connection
// is dropped and its slot handed to a waiter, and Close wakes waiters.
func TestPoolFailurePaths(t *testing.T) {
	path, _ := seed(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := db.pool
	ctx := context.Background()
	db.SetMaxOpenConns(2)

	held, err := p.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.dsn = path + "?_busy_timeout=never"
	if _, err := p.get(ctx); err == nil {
		t.Fatal("dial with a bad DSN: no error")
	}
	if p.open != 1 {
		t.Fatalf("after a failed dial open=%d, want 1", p.open)
	}
	p.dsn = path

	db.SetMaxOpenConns(1)
	got := make(chan error, 1)
	go func() {
		c, err := p.get(ctx)
		if err == nil {
			p.put(c, false)
		}
		got <- err
	}()
	waitForWaiter(t, p)
	p.put(held, true) // bad: closed, and its slot goes to the waiter
	if err := <-got; err != nil {
		t.Fatalf("waiter after a bad connection: %v", err)
	}

	held, err = p.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := p.get(ctx)
		got <- err
	}()
	waitForWaiter(t, p)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-got; !errors.Is(err, errClosed) {
		t.Fatalf("waiter at Close: %v", err)
	}
	p.put(held, false) // returned after Close: closed, not kept
	if p.open != 0 || len(p.idle) != 0 {
		t.Fatalf("after Close: open=%d idle=%d", p.open, len(p.idle))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := db.Exec(ctx, `SELECT 1`); !errors.Is(err, errClosed) {
		t.Fatalf("Exec after Close: %v", err)
	}
	if _, err := db.Begin(ctx, nil); !errors.Is(err, errClosed) {
		t.Fatalf("Begin after Close: %v", err)
	}
}

func waitForWaiter(t *testing.T, p *pool) {
	t.Helper()
	for range 500 {
		p.mu.Lock()
		n := len(p.waiters)
		p.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no waiter queued")
}

// TestWrapRefusesOtherDrivers: Wrap checks the connection is musql's.
func TestWrapRefusesOtherDrivers(t *testing.T) {
	other, err := sql.Open("boost-test-other", "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := Wrap(other); err == nil || !strings.Contains(err.Error(), "musql") {
		t.Fatalf("Wrap of another driver: %v", err)
	}
	_, std := seed(t)
	db, err := Wrap(std)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	if s := std.Stats(); s.MaxOpenConnections != 3 {
		t.Fatalf("SetMaxOpenConns did not reach the wrapped db: %+v", s)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := std.Ping(); err != nil {
		t.Fatalf("Close closed the wrapped db: %v", err)
	}
}

func init() { sql.Register("boost-test-other", otherDriver{}) }

type otherDriver struct{}

func (otherDriver) Open(string) (sqldriver.Conn, error) { return otherConn{}, nil }

type otherConn struct{}

func (otherConn) Prepare(string) (sqldriver.Stmt, error) { return nil, io.EOF }
func (otherConn) Close() error                           { return nil }
func (otherConn) Begin() (sqldriver.Tx, error)           { return nil, io.EOF }
