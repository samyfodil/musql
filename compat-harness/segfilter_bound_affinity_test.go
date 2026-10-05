package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// TestSegFilterBoundTakesComparisonAffinity verifies that filter-count optimizations
// apply comparison affinity to bound parameters correctly.
func TestSegFilterBoundTakesComparisonAffinity(t *testing.T) {
	dir := t.TempDir()
	mq, err := sql.Open(driver.DriverName, filepath.Join(dir, "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	c, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)
	for _, db := range []*sql.DB{mq, c} {
		for _, s := range []string{
			`CREATE TABLE ti(x INTEGER)`,
			`CREATE TABLE tt(s TEXT)`,
			`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<99) INSERT INTO ti SELECT i % 5 FROM n`,
			`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<99) INSERT INTO tt SELECT i % 5 FROM n`,
		} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(s, err)
			}
		}
	}
	count := func(db *sql.DB, q string, arg any) int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(q, arg).Scan(&n); err != nil {
			t.Fatalf("%s [%v]: %v", q, arg, err)
		}
		return n
	}
	engine.ResetSegFilterCountersForTest()
	for _, q := range []string{
		`SELECT count(*) FROM ti WHERE x = ?`,
		`SELECT count(*) FROM ti WHERE x < ?`,
		`SELECT count(*) FROM ti WHERE x >= ?`,
		`SELECT count(*) FROM tt WHERE s = ?`,
		`SELECT count(*) FROM tt WHERE s > ?`,
	} {
		for _, arg := range []any{"1", "1.0", " 2 ", "abc", int64(3), 3.0, 2.5, []byte("1")} {
			if got, want := count(mq, q, arg), count(c, q, arg); got != want {
				t.Errorf("%s [%#v]: musql %d, C %d", q, arg, got, want)
			}
		}
		for _, lit := range []string{"'1'", "'1.0'", "' 2 '", "'abc'", "3", "3.0", "2.5"} {
			var got, want int64
			lq := q[:len(q)-1] + lit
			if err := mq.QueryRow(lq).Scan(&got); err != nil {
				t.Fatal(lq, err)
			}
			if err := c.QueryRow(lq).Scan(&want); err != nil {
				t.Fatal(lq, err)
			}
			if got != want {
				t.Errorf("%s: musql %d, C %d", lq, got, want)
			}
		}
	}
	if served, _ := engine.SegFilterCountersForTest(); served == 0 {
		t.Fatal("the filter-count fast path served nothing: this test compared the loop, not the fast path")
	}
}
