package compat

// Tests that FAIL-halted RETURNING statements keep survivor rows through the
// driver, comparing two code paths (queryReturningArgs vs execArgs).

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r39cFailSetup has a trigger that fails on the second row
var r39cFailSetup = []string{
	`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`,
	`CREATE TRIGGER g1 BEFORE INSERT ON t WHEN new.a = 6 BEGIN SELECT RAISE(FAIL,'no6'); END`,
	`INSERT INTO t(a,b,c) VALUES(1,'x',10)`,
}

const r39cFailDML = `INSERT INTO t(a,b,c) VALUES(4,'q',40),(6,'r',60)`

// r39cDriverTable runs setup and DML, then reads the table back
func r39cDriverTable(t *testing.T, driver, dsn, dml string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s: open: %v", driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range r39cFailSetup {
		if _, serr := db.Exec(s); serr != nil {
			t.Fatalf("%s: setup %q: %v", driver, s, serr)
		}
	}
	if rows, qerr := db.Query(dml); qerr == nil {
		for rows.Next() {
		}
		rows.Close()
	}
	rows, qerr := db.Query(`SELECT a FROM t ORDER BY a`)
	if qerr != nil {
		t.Fatalf("%s: readback: %v", driver, qerr)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a int64
		if serr := rows.Scan(&a); serr != nil {
			t.Fatalf("%s: scan: %v", driver, serr)
		}
		out = append(out, r39cNormAny(a))
	}
	return strings.Join(out, ",")
}

func TestR39CFailReturningKeepsSurvivorsThroughDriver(t *testing.T) {
	for _, tc := range []struct {
		name string
		dml  string
		// open records what this engine answers today, and is a PIN, not an
		// expectation -- the case fails if the engine moves off it in EITHER
		// direction, including onto the oracle's, which is the signal to delete
		// the pin.
		open string
	}{
		// Control: same statement without RETURNING
		{"no-returning", r39cFailDML, ""},
		// With RETURNING clause
		{"returning", r39cFailDML + ` RETURNING a`, ""},
	} {
		want := r39cDriverTable(t, "sqlite3", t.TempDir()+"/oracle.sqlite", tc.dml)
		got := r39cDriverTable(t, "sqlite", t.TempDir()+"/musql.sqlite", tc.dml)
		if tc.open != "" {
			if got != tc.open {
				t.Errorf("%s: %s\n  this engine now answers %q, not the pinned %q.\n"+
					"  If it now equals the oracle's %q, DELETE this case's `open` field --\n"+
					"  a stale pin is what lets the next regression here pass unnoticed.",
					tc.name, tc.dml, got, tc.open, want)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %s\n  oracle: %s\n  engine: %s", tc.name, tc.dml, want, got)
		}
	}
}

// TestR39CFailReturningEngineDirect verifies the engine keeps survivors
// correctly through both RETURNING and non-RETURNING paths
func TestR39CFailReturningEngineDirect(t *testing.T) {
	for _, dml := range []string{r39cFailDML, r39cFailDML + ` RETURNING a`} {
		db, err := engine.Create(t.TempDir()+"/r39c.sqlite")
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		for _, s := range r39cFailSetup {
			if _, _, serr := db.ExecArgs(s, nil); serr != nil {
				t.Fatalf("setup %q: %v", s, serr)
			}
		}
		if engine.StatementHasReturning(dml) {
			db.ExecReturningArgs(dml, nil)
		} else {
			db.ExecArgs(dml, nil)
		}
		pager, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("snapshot: %v", perr)
		}
		_, rows, qerr := pager.QueryArgs(`SELECT a FROM t ORDER BY a`, nil)
		if qerr != nil {
			t.Fatalf("readback: %v", qerr)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r39cNorm(r[0]))
		}
		db.Close()
		// C SQLite (measured with mattn/go-sqlite3 3.53.3): the RAISE(FAIL)
		// halt keeps the row written before it, so a=1 AND a=4 survive.
		if want := "I:1,I:4"; strings.Join(got, ",") != want {
			t.Errorf("%s\n  want %s (C SQLite)\n  got  %s", dml, want, strings.Join(got, ","))
		}
	}
}
