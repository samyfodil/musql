package engine

import (
	"path/filepath"
	"testing"
)

// Tests that JIT aggregate opcodes read from the correct database file.
// Root page numbers are file-local, so cross-database reads must use the
// correct pager.
func TestCrossDatabaseAggregateCountsItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	aux := filepath.Join(dir, "aux.musq")
	nw2, err := Create(aux)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(x INTEGER)`,
		`INSERT INTO t VALUES(1),(2),(3),(4),(5)`,
	} {
		if err := nw2.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := nw2.Close(); err != nil {
		t.Fatal(err)
	}

	nw, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()
	for _, s := range []string{
		`CREATE TABLE t(x INTEGER)`,
		`INSERT INTO t VALUES(7),(9)`,
		`ATTACH DATABASE '` + aux + `' AS aux`,
	} {
		if err := nw.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// The TEMP CATALOG is the shape that reproduces, and it reproduces because
	// the two root numbers COLLIDE: temp's catalog is its file's page 1 and so is
	// main's, so a count that looks temp's root up in main's segments finds
	// main's catalog sitting there and answers about it. An ordinary attached
	// table is checked below too, but it cannot prove much on its own -- a root
	// number the other file does not use declines into the loop and comes out
	// right by luck.
	if err := nw.Exec(`CREATE TEMP TABLE tt(x)`); err != nil {
		t.Fatal(err)
	}
	if err := nw.Exec(`CREATE TEMP TABLE tt2(x)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sql  string
		want int64
	}{
		// main's catalog holds ONE row (t) and temp's holds TWO, so a read of
		// the wrong file cannot pass.
		{`SELECT count(*) FROM temp.sqlite_master`, 2},
		{`SELECT count(*) FROM sqlite_temp_master`, 2},
		{`SELECT count(*) FROM main.sqlite_master`, 1},
		{`SELECT count(*) FROM t`, 2},
		{`SELECT count(*) FROM main.t`, 2},
		{`SELECT count(*) FROM aux.t`, 5},
		{`SELECT sum(x) FROM aux.t`, 15},
		{`SELECT sum(x) FROM t`, 16},
		{`SELECT count(*) FROM aux.t WHERE x > 2`, 3},
		{`SELECT max(x) FROM aux.t`, 5},
		{`SELECT min(x) FROM t`, 7},
	} {
		p, perr := nw.ReadPager()
		if perr != nil {
			t.Fatal(perr)
		}
		_, rows, qerr := p.QueryArgs(c.sql, nil)
		if qerr != nil {
			t.Fatalf("%s: %v", c.sql, qerr)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("%s: got %v", c.sql, rows)
		}
		if got := rows[0][0]; got.Typ != Int || got.I != c.want {
			t.Errorf("%s = %v, want %d", c.sql, got, c.want)
		}
	}
}
