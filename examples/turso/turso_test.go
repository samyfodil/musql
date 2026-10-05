package turso

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/hrana"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// serve starts a Hrana server over a fresh musql database and returns a Turso
// client (libsql-client-go) connected to it over HTTP.
func serve(t *testing.T, token, scheme string) *sql.DB {
	t.Helper()
	backing, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "srv.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backing.Close() })
	srv := hrana.NewServer(backing, hrana.WithAuthToken(token))
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { hs.Close(); srv.Close() })
	dsn := strings.Replace(hs.URL, "http", scheme, 1)
	if token != "" {
		dsn += "?authToken=" + token
	}
	client, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestTursoClientAgainstMusql(t *testing.T) {
	for _, scheme := range []string{"http", "ws"} {
		t.Run(scheme, func(t *testing.T) { runClient(t, serve(t, "", scheme)) })
	}
}

func runClient(t *testing.T, db *sql.DB) {
	ctx := context.Background()
	must := func(q string, args ...any) sql.Result {
		t.Helper()
		r, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	must(`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, score REAL, big INTEGER, data BLOB)`)
	r := must(`INSERT INTO t(name, score, big, data) VALUES(?, ?, ?, ?)`, "ada", 1.5, int64(1)<<62, []byte{0, 1, 2})
	if n, _ := r.RowsAffected(); n != 1 {
		t.Fatalf("rows affected %d", n)
	}
	if id, _ := r.LastInsertId(); id != 1 {
		t.Fatalf("last insert id %d", id)
	}
	must(`INSERT INTO t(name, score) VALUES(:n, :s)`, sql.Named("n", "bob"), sql.Named("s", 2.25))

	var (
		name  string
		score float64
		big   sql.NullInt64
		data  []byte
	)
	if err := db.QueryRowContext(ctx, `SELECT name, score, big, data FROM t WHERE id = ?`, 1).Scan(&name, &score, &big, &data); err != nil {
		t.Fatal(err)
	}
	if name != "ada" || score != 1.5 || big.Int64 != int64(1)<<62 || !bytes.Equal(data, []byte{0, 1, 2}) {
		t.Fatalf("got %q %v %v %v", name, score, big, data)
	}
	var nullBig sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT big FROM t WHERE name = 'bob'`).Scan(&nullBig); err != nil || nullBig.Valid {
		t.Fatalf("NULL round trip: %v %v", nullBig, err)
	}

	// A constraint violation surfaces as an error, and the stream survives it.
	if _, err := db.ExecContext(ctx, `INSERT INTO t(name) VALUES('ada')`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate insert: %v", err)
	}

	// An interactive transaction rolled back leaves nothing behind.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO t(name) VALUES('ghost')`); err != nil {
		t.Fatal(err)
	}
	var inTx int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&inTx); err != nil || inTx != 3 {
		t.Fatalf("count inside tx: %d %v", inTx, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// ...and a committed one keeps its rows.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx.ExecContext(ctx, `INSERT INTO t(name) VALUES('carol')`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("count after rollback+commit: %d %v", n, err)
	}

	rows, err := db.QueryContext(ctx, `SELECT id, name FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var id int
		var nm string
		rows.Scan(&id, &nm)
		names = append(names, nm)
	}
	rows.Close()
	if strings.Join(names, ",") != "ada,bob,carol" {
		t.Fatalf("rows %v", names)
	}
}

func TestAuthToken(t *testing.T) {
	for _, scheme := range []string{"http", "ws"} {
		db := serve(t, "s3cret", scheme)
		if _, err := db.Exec(`CREATE TABLE a(x)`); err != nil {
			t.Fatalf("%s with the right token: %v", scheme, err)
		}
	}
}
