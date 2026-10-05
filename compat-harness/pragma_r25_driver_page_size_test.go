// Tests PRAGMA page_size behavior through the driver with VACUUM INTO.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaR25DriverVacuumIntoPageSize runs the program on ONE connection of the
// named driver and returns the page size of the copy it wrote.
func pragmaR25DriverVacuumIntoPageSize(t *testing.T, driverName, dir, request string) int {
	t.Helper()
	src := filepath.Join(dir, driverName+"-src.db")
	copyPath := filepath.Join(dir, driverName+"-copy.db")
	db, err := sql.Open(driverName, src)
	if err != nil {
		t.Fatalf("open %s: %v", driverName, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one logical connection: the request is per-connection
	stmts := []string{
		`PRAGMA page_size=4096`,
		`CREATE TABLE t1(a, b)`,
		`INSERT INTO t1 VALUES(1, 'x'), (2, 'y')`,
		`PRAGMA page_size=` + request,
		`VACUUM INTO '` + copyPath + `'`,
	}
	for _, s := range stmts {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s %q: %v", driverName, s, eerr)
		}
	}
	return pageSizeOf(t, copyPath)
}

func TestPragmaR25DriverCarriesThePageSizeRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
	}{
		// The rule itself: a usable request sizes the copy.
		{"1024", "1024"},
		{"512-the-smallest-legal-size", "512"},
		{"2048", "2048"},
		// ...and an unusable one leaves it at the source's size, because
		// vacuum.c's second SetPageSize call simply does nothing rather than
		// clearing what the first one already set.
		{"1000-not-a-power-of-two", "1000"},
		{"256-below-the-minimum", "256"},
		{"0", "0"},
		// Setting it back to the size the source already is.
		{"4096-the-source-own-size", "4096"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			want := pragmaR25DriverVacuumIntoPageSize(t, "sqlite3", dir, tc.request)
			got := pragmaR25DriverVacuumIntoPageSize(t, "sqlite", dir, tc.request)
			if got != want {
				t.Errorf("DIVERGES: PRAGMA page_size=%s then VACUUM INTO through the driver\n  cgo:    copy page_size=%d\n  musql: copy page_size=%d",
					tc.request, want, got)
			}
		})
	}

	// The request is NOT consumed: a second VACUUM INTO comes out at the same
	// size. Through the driver that is the interesting half, since each
	// statement runs on its own session.
	t.Run("the-request-survives-two-copies", func(t *testing.T) {
		dir := t.TempDir()
		for _, drv := range []string{"sqlite3", "sqlite"} {
			src := filepath.Join(dir, drv+"-src.db")
			db, err := sql.Open(drv, src)
			if err != nil {
				t.Fatalf("open %s: %v", drv, err)
			}
			db.SetMaxOpenConns(1)
			for _, s := range []string{
				`PRAGMA page_size=4096`,
				`CREATE TABLE t1(a)`,
				`INSERT INTO t1 VALUES(1)`,
				`PRAGMA page_size=1024`,
				`VACUUM INTO '` + filepath.Join(dir, drv+"-c1.db") + `'`,
				`VACUUM INTO '` + filepath.Join(dir, drv+"-c2.db") + `'`,
			} {
				if _, eerr := db.Exec(s); eerr != nil {
					db.Close()
					t.Fatalf("%s %q: %v", drv, s, eerr)
				}
			}
			db.Close()
			for _, n := range []string{"-c1.db", "-c2.db"} {
				if ps := pageSizeOf(t, filepath.Join(dir, drv+n)); ps != 1024 {
					t.Errorf("%s%s page_size=%d, want 1024 (the request must not be consumed)", drv, n, ps)
				}
			}
		}
	})

	// The setter INSIDE a transaction, consumed after it. VACUUM INTO itself
	// cannot run there -- C SQLite answers "cannot VACUUM from within a
	// transaction", which is why the Conn never has to push the request into a
	// held session -- so the question is whether the request survives the
	// COMMIT. Whatever the oracle does is the target.
	t.Run("set-inside-a-transaction-consumed-after-it", func(t *testing.T) {
		dir := t.TempDir()
		sizeOf := func(drv string) int {
			src := filepath.Join(dir, drv+"-src.db")
			cp := filepath.Join(dir, drv+"-copy.db")
			db, err := sql.Open(drv, src)
			if err != nil {
				t.Fatalf("open %s: %v", drv, err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, s := range []string{
				`PRAGMA page_size=4096`,
				`CREATE TABLE t1(a)`,
				`INSERT INTO t1 VALUES(1)`,
				`BEGIN`,
				`PRAGMA page_size=1024`,
				`INSERT INTO t1 VALUES(2)`,
				`COMMIT`,
				`VACUUM INTO '` + cp + `'`,
			} {
				if _, eerr := db.Exec(s); eerr != nil {
					t.Fatalf("%s %q: %v", drv, s, eerr)
				}
			}
			return pageSizeOf(t, cp)
		}
		want, got := sizeOf("sqlite3"), sizeOf("sqlite")
		if got != want {
			t.Errorf("DIVERGES when the request is set inside a transaction\n  cgo:    copy page_size=%d\n  musql: copy page_size=%d", want, got)
		}
	})
}
