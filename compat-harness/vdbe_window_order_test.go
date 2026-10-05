// This file is the differential gate for a window query's ORDER BY handling on
// both sides of the OVER clause: the window's own PARTITION BY/ORDER BY compare
// under their key expression's COLLATING SEQUENCE (so 'apple' and 'APPLE' are
// peers under COLLATE NOCASE), the OUTER ORDER BY honors its collation too, an
// outer ORDER BY ORDINAL names an output column rather than a constant, and the
// built-in ranking functions take no arguments (window9/window1/windowerr).
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestWindowOrderParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE fruits(name TEXT COLLATE NOCASE, color TEXT COLLATE NOCASE)`,
		`INSERT INTO fruits VALUES('apple','RED'),('APPLE','yellow'),('pear','YELLOW'),('PEAR','green')`,
		`CREATE TABLE t1(a,b)`,
		`INSERT INTO t1 VALUES(1,10),(2,15),(3,-5),(4,-5),(5,10),(6,-11)`,
	}
	probes := []string{
		`SELECT name, color, dense_rank() OVER (ORDER BY name) FROM fruits ORDER BY name, color`,
		`SELECT name, color, dense_rank() OVER (PARTITION BY name ORDER BY color) FROM fruits ORDER BY name, color`,
		`SELECT name, color, dense_rank() OVER (ORDER BY name), dense_rank() OVER (PARTITION BY name ORDER BY color) FROM fruits ORDER BY color`,
		`SELECT a, sum(b) OVER (ORDER BY a) AS abc FROM t1 ORDER BY 2`,
		`SELECT a, sum(b) OVER (ORDER BY a) AS abc FROM t1 ORDER BY 2 DESC`,
		`SELECT a, sum(b) OVER (ORDER BY a) AS abc FROM t1 ORDER BY abc`,
	}
	errProbes := []string{
		`SELECT row_number(a) OVER () FROM t1`,
		`SELECT rank(a) OVER (ORDER BY a) FROM t1`,
		`SELECT dense_rank(a,b) OVER (ORDER BY a) FROM t1`,
		`SELECT percent_rank(a) OVER (ORDER BY a) FROM t1`,
		`SELECT cume_dist(a) OVER (ORDER BY a) FROM t1`,
		`SELECT row_number() OVER () FROM t1`,
	}
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "e.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range setup {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
		}
		for _, q := range probes {
			out := queryString(t, db, q)
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			if got[q] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		for _, q := range errProbes {
			var n any
			e := db.QueryRow(q).Scan(&n)
			msg := ""
			if e != nil {
				msg = strings.TrimPrefix(e.Error(), "engine: ")
			}
			if drv == "sqlite" {
				got[q] = msg
				continue
			}
			if got[q] != msg {
				t.Errorf("%s\n  engine: %q\n  cgo:    %q", q, got[q], msg)
			}
		}
		db.Close()
	}
}
