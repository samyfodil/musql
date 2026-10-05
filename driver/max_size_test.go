package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dbBytes is everything the database at p holds on disk: the segment file and
// its delta.
func dbBytes(t *testing.T, p string) int64 {
	t.Helper()
	var n int64
	for _, f := range []string{p, p + ".delta"} {
		if st, err := os.Stat(f); err == nil {
			n += st.Size()
		}
	}
	return n
}

func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func isFull(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database or disk is full")
}

// TestMaxSize: "PRAGMA max_size" fails the commit that would leave the file over
// the limit, leaves nothing of it behind, and lets deletes make room again.
func TestMaxSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if got := count(t, db, `PRAGMA max_size`); got != 0 {
		t.Fatalf("default max_size = %d, want 0", got)
	}
	for _, bad := range []string{`PRAGMA max_size = -1`, `PRAGMA max_size = 'x'`, `PRAGMA aux.max_size = 10`} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	if _, err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	limit := dbBytes(t, p) + 64<<10
	if _, err := db.Exec(`PRAGMA max_size = ` + strconv.FormatInt(limit, 10)); err != nil {
		t.Fatal(err)
	}
	if got := count(t, db, `PRAGMA max_size`); int64(got) != limit {
		t.Fatalf("max_size = %d, want %d", got, limit)
	}

	// Fill it, one autocommit row at a time, until a commit is refused.
	row := strings.Repeat("x", 1000)
	var full int
	for i := 0; i < 10000; i++ {
		_, err := db.Exec(`INSERT INTO t(v) VALUES(?)`, row)
		if isFull(err) {
			full = i
			break
		}
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if full == 0 {
		t.Fatal("never refused")
	}
	if got := dbBytes(t, p); got > limit {
		t.Fatalf("file is %d bytes, over the %d limit", got, limit)
	}
	if got := count(t, db, `SELECT count(*) FROM t`); got != full {
		t.Fatalf("%d rows after the refusal, want %d: the refused insert left something", got, full)
	}

	// A transaction is refused whole: its insert into t does not land without
	// the table it could not create.
	_, err = db.Exec(`BEGIN; INSERT INTO t(v) VALUES('small'); CREATE TABLE big AS SELECT v FROM t; COMMIT`)
	if !isFull(err) {
		t.Fatalf("an over-limit transaction: %v, want full", err)
	}
	db.Exec(`ROLLBACK`)
	if got := count(t, db, `SELECT count(*) FROM t`); got != full {
		t.Fatalf("%d rows after the refused transaction, want %d", got, full)
	}
	if got := count(t, db, `SELECT count(*) FROM sqlite_master WHERE name='big'`); got != 0 {
		t.Fatal("the refused CREATE TABLE landed")
	}

	// Deleting makes room, though a delete only appends to the log: the commit
	// that would not fit is a rewrite of the live rows instead.
	if _, err := db.Exec(`DELETE FROM t WHERE id % 2 = 0`); err != nil {
		t.Fatalf("delete on a full database: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t(v) VALUES(?)`, row); err != nil {
		t.Fatalf("insert after the delete: %v", err)
	}
	if got := dbBytes(t, p); got > limit {
		t.Fatalf("file is %d bytes, over the %d limit", got, limit)
	}

	// Reopened, every committed row is there and the refused ones are not.
	want := count(t, db, `SELECT count(*) FROM t`)
	db.Close()
	db2, err := sql.Open("sqlite", p+"?_pragma=max_size("+strconv.FormatInt(limit, 10)+")")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if got := count(t, db2, `SELECT count(*) FROM t`); got != want {
		t.Fatalf("reopened: %d rows, want %d", got, want)
	}
	if got := count(t, db2, `PRAGMA max_size`); int64(got) != limit {
		t.Fatalf("max_size from the DSN = %d, want %d", got, limit)
	}
	// ...and a connection without the pragma is unlimited: the limit is the
	// connection's, not the file's.
	db3, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	if got := count(t, db3, `PRAGMA max_size`); got != 0 {
		t.Fatalf("a fresh connection's max_size = %d, want 0", got)
	}
}
