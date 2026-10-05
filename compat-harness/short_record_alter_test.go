// This file gates reading rows narrower than the schema, which C SQLite
// leaves after ALTER TABLE ADD COLUMN (no row is rewritten). Such rows must
// read back padded with the column defaults; see padStoredRow.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// alterWidenedSetup builds the fixture with C SQLite, so the short records are
// real.
var alterWidenedSetup = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
	`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
	`CREATE TABLE t2(a, b REAL)`,
	`INSERT INTO t2 VALUES(1, 2.0),(3, 4.0)`,
	// Three widenings: no default, a constant default, and a text default.
	`ALTER TABLE t1 ADD COLUMN c`,
	`ALTER TABLE t1 ADD COLUMN d DEFAULT 9`,
	`ALTER TABLE t1 ADD COLUMN e TEXT DEFAULT 'zz'`,
	`ALTER TABLE t2 ADD COLUMN c REAL DEFAULT 1.5`,
	// One row written AFTER the widening is full width, so a table mixes both.
	`INSERT INTO t1 VALUES(4,'w',5,6,'q')`,
	`CREATE INDEX t1d ON t1(d)`,
}

// alterWidenedReads are compared cell for cell between the two engines.
var alterWidenedReads = []string{
	`SELECT * FROM t1`,
	`SELECT a,b,c,d,e FROM t1`,
	`SELECT typeof(c), typeof(d), typeof(e) FROM t1`,
	`SELECT count(*), sum(d) FROM t1`,
	`SELECT * FROM t1 WHERE d=9`,
	`SELECT * FROM t1 WHERE e='zz'`,
	`SELECT * FROM t1 WHERE c IS NULL`,
	`SELECT * FROM t1 ORDER BY d, a`,
	`SELECT * FROM t2`,
	`SELECT typeof(c) FROM t2`,
	`SELECT t1.a, t2.c FROM t1 JOIN t2 ON t1.a=t2.a`,
	`PRAGMA integrity_check`,
}

func TestAlterWidenedRowsMatchCSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "widened.db")
	cgodb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	for _, s := range alterWidenedSetup {
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("real %q: %v", s, err)
		}
	}

	// Read side: the same rows C SQLite reports.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("PANIC reading a real-SQLite file widened by ALTER TABLE ADD COLUMN: %v", r)
			}
		}()
		// The C SQLite file is brought in through the converter.
		segPath := path + ".musq"
		if ierr := sqliteconv.Import(path, segPath, sqliteconv.ImportOptions{}); ierr != nil {
			t.Fatalf("ImportSQLite: %v", ierr)
		}
		p, oerr := engine.Open(segPath)
		if oerr != nil {
			t.Fatalf("engine.Open: %v", oerr)
		}
		defer p.Close()
		for _, q := range alterWidenedReads {
			goCols, goVals, qerr := p.QueryArgs(q, nil)
			if qerr != nil {
				t.Errorf("%q: this engine errored: %v", q, qerr)
				continue
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, q)
			if cerr != nil {
				t.Fatalf("%q: C SQLite errored: %v", q, cerr)
			}
			if ok, why := queryResultsMatch(goCols, wsNormalize(goVals), cgoCols, cgoRows, false); !ok {
				t.Errorf("%q diverges: %s\n  pure: %v %v\n  real: %v %v", q, why, goCols, wsNormalize(goVals), cgoCols, cgoRows)
			}
		}
	}()

	// Write side: the row store must be full width too, or an UPDATE of a
	// pre-ALTER row reads past its end.
	writes := []string{
		`UPDATE t1 SET c=100 WHERE a=1`,
		`UPDATE t1 SET e='new' WHERE d=9`,
		`DELETE FROM t1 WHERE a=3`,
		`INSERT INTO t1(a,b) VALUES(9,'later')`,
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("PANIC writing a real-SQLite file widened by ALTER TABLE ADD COLUMN: %v", r)
			}
		}()
		// Both engines run the same statements over the same starting rows.
		segW := path + ".w.musq"
		if ierr := sqliteconv.Import(path, segW, sqliteconv.ImportOptions{}); ierr != nil {
			t.Fatalf("ImportSQLite: %v", ierr)
		}
		godb, oerr := engine.OpenWrite(segW)
		if oerr != nil {
			t.Fatalf("engine.OpenWrite: %v", oerr)
		}
		for _, w := range writes {
			if _, _, werr := godb.ExecArgs(w, nil); werr != nil {
				godb.Discard()
				t.Fatalf("pure %q: %v", w, werr)
			}
		}
		p, perr := godb.SnapshotPager()
		if perr != nil {
			godb.Discard()
			t.Fatalf("SnapshotPager: %v", perr)
		}
		goCols, goVals, qerr := p.QueryArgs(`SELECT * FROM t1`, nil)
		p.Close()
		godb.Discard() // leave the oracle's file untouched for the compare below
		if qerr != nil {
			t.Fatalf("post-write SELECT: %v", qerr)
		}
		for _, w := range writes {
			if _, err := cgodb.Exec(w); err != nil {
				t.Fatalf("real %q: %v", w, err)
			}
		}
		cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, `SELECT * FROM t1`)
		if cerr != nil {
			t.Fatalf("real post-write SELECT: %v", cerr)
		}
		if ok, why := queryResultsMatch(goCols, wsNormalize(goVals), cgoCols, cgoRows, false); !ok {
			t.Errorf("post-write rows diverge: %s\n  pure: %v %v\n  real: %v %v", why, goCols, wsNormalize(goVals), cgoCols, cgoRows)
		}
	}()
	cgodb.Close()
}
