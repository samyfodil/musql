package driver

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// A transaction's load builds each full segment on a goroutine of its own
// (engine/segment_cutter.go, async). Every way the transaction can end with a
// build in flight must wait for it or drop it: a ROLLBACK, a failed statement,
// the connection closing, a read in the middle -- and the process dying, after
// which the file holds only what was committed.
const asyncRows = 3*65536 + 123 // three full segments, so a build is in flight

func asyncDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func asyncLoad(t *testing.T, tx *sql.Tx, from, to int) {
	t.Helper()
	st, err := tx.Prepare("INSERT INTO kv VALUES (?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for k := from; k <= to; k++ {
		if _, err := st.Exec(k, fmt.Sprint("v", k)); err != nil {
			t.Fatalf("insert %d: %v", k, err)
		}
	}
}

func asyncCount(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}) (n, sum int64) {
	t.Helper()
	if err := q.QueryRow("SELECT count(*), coalesce(sum(k), 0) FROM kv").Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

func TestTxBulkLoadAsyncBuild(t *testing.T) {
	setup := func(t *testing.T) (*sql.DB, string) {
		path := filepath.Join(t.TempDir(), "a.musq")
		db := asyncDB(t, path)
		if _, err := db.Exec("CREATE TABLE kv(k INTEGER PRIMARY KEY, v TEXT NOT NULL CHECK (v <> 'bad'))"); err != nil {
			t.Fatal(err)
		}
		return db, path
	}
	t.Run("rollback", func(t *testing.T) {
		db, _ := setup(t)
		defer db.Close()
		tx, _ := db.Begin()
		asyncLoad(t, tx, 1, asyncRows)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if n, _ := asyncCount(t, db); n != 0 {
			t.Fatalf("%d rows after rollback", n)
		}
	})
	t.Run("a failed statement, then commit", func(t *testing.T) {
		db, _ := setup(t)
		defer db.Close()
		tx, _ := db.Begin()
		asyncLoad(t, tx, 1, asyncRows)
		if _, err := tx.Exec("INSERT INTO kv VALUES (?, 'x'), (?, 'bad')", asyncRows+1, asyncRows+2); err == nil {
			t.Fatal("the CHECK did not fail")
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		want := int64(asyncRows) * (asyncRows + 1) / 2
		if n, sum := asyncCount(t, db); n != asyncRows || sum != want {
			t.Fatalf("%d rows sum %d, want %d sum %d", n, sum, asyncRows, want)
		}
	})
	t.Run("a read in the middle", func(t *testing.T) {
		db, _ := setup(t)
		defer db.Close()
		tx, _ := db.Begin()
		asyncLoad(t, tx, 1, asyncRows)
		if n, _ := asyncCount(t, tx); n != asyncRows {
			t.Fatalf("inside: %d rows", n)
		}
		asyncLoad(t, tx, asyncRows+1, asyncRows+10)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if n, _ := asyncCount(t, db); n != asyncRows+10 {
			t.Fatalf("after commit: %d rows", n)
		}
	})
	t.Run("close mid-load", func(t *testing.T) {
		db, path := setup(t)
		tx, _ := db.Begin()
		asyncLoad(t, tx, 1, 10)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx, _ = db.Begin()
		asyncLoad(t, tx, 11, asyncRows)
		db.Close() // the transaction is never ended
		db2 := asyncDB(t, path)
		defer db2.Close()
		if n, sum := asyncCount(t, db2); n != 10 || sum != 55 {
			t.Fatalf("reopened: %d rows sum %d, want the 10 committed", n, sum)
		}
	})
}

// The process dies with a segment build in flight: the reopened file holds
// only the committed rows. The child is this test binary, re-run.
func TestTxBulkLoadAsyncCrash(t *testing.T) {
	if path := os.Getenv("MUSQL_ASYNC_CRASH_CHILD"); path != "" {
		db := asyncDB(t, path)
		if _, err := db.Exec("CREATE TABLE kv(k INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		tx, _ := db.Begin()
		asyncLoad(t, tx, 1, 1000)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx, _ = db.Begin()
		asyncLoad(t, tx, 1001, asyncRows)
		os.Exit(3) // no rollback, no close: a build may be running
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no second process to kill here")
	}
	path := filepath.Join(t.TempDir(), "c.musq")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTxBulkLoadAsyncCrash$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MUSQL_ASYNC_CRASH_CHILD="+path)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("child: %v\n%s", err, out)
	}
	db := asyncDB(t, path)
	defer db.Close()
	if n, sum := asyncCount(t, db); n != 1000 || sum != 500500 {
		t.Fatalf("reopened after the crash: %d rows sum %d, want the 1000 committed", n, sum)
	}
	var ic string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ic); err != nil || ic != "ok" {
		t.Fatalf("integrity_check: %q %v", ic, err)
	}
}
