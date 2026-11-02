package compat

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// TestNaNBindsAsNull: C stores a NaN bound to a parameter as NULL
// (sqlite3_bind_double, through sqlite3VdbeMemSetDouble), so a NaN never
// reaches a comparison, an aggregate or a table.
func TestNaNBindsAsNull(t *testing.T) {
	dir := t.TempDir()
	mq, err := sql.Open(driver.DriverName, filepath.Join(dir, "n.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	c, err := sql.Open("sqlite3", filepath.Join(dir, "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	nan := math.NaN()
	for _, db := range []*sql.DB{mq, c} {
		for _, s := range []string{`CREATE TABLE t(x REAL)`, `INSERT INTO t VALUES (1.5), (-2.5), (0.0)`} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(s, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO t VALUES (?), (?)`, nan, float32(nan)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT ? IS NULL, typeof(?), ? + 1`, []any{nan, nan, nan}},
		{`SELECT count(*), count(x), sum(x), min(x), max(x) FROM t`, nil},
		{`SELECT count(*) FROM t WHERE x >= ?`, []any{nan}},
		{`SELECT count(*) FROM t WHERE x < ? OR x = ?`, []any{nan, nan}},
		{`SELECT sum(x) FROM t WHERE x <> ?`, []any{nan}},
		{`SELECT x FROM t WHERE x > ? ORDER BY x`, []any{float32(nan)}},
	} {
		got, gerr := realRow(t, mq, q.sql, q.args)
		want, werr := realRow(t, c, q.sql, q.args)
		if (gerr == nil) != (werr == nil) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: musql %v %v, C %v %v", q.sql, got, gerr, want, werr)
		}
	}
}
