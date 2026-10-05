// OVER and WINDOW keywords used as identifiers.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r25KwSchema = []string{
	`CREATE TABLE over(x, over)`,
	`CREATE TABLE window(x, window)`,
	`INSERT INTO over VALUES(1, 2), (3, 4), (5, 6)`,
	`INSERT INTO window VALUES(1, 2), (3, 4), (5, 6)`,
	`CREATE TABLE t4(x, y)`,
	`INSERT INTO t4 VALUES(7, 8)`,
}

var r25KwQ = []string{
	// window6.test 5.0-5.4: OVER and WINDOW as alias, table name, column name
	// and window name, in every combination the test file uses.
	`SELECT sum(x) over FROM over`,
	`SELECT sum(x) over over FROM over WINDOW over AS ()`,
	`SELECT sum(over) over over over FROM over over WINDOW over AS (ORDER BY over)`,
	`SELECT sum(window) OVER window window FROM window window window window AS (ORDER BY window)`,
	`SELECT count(*) OVER win FROM over WINDOW win AS (ORDER BY x ROWS BETWEEN +2 FOLLOWING AND +3 FOLLOWING)`,
	// window6.test 4.1: WINDOW as a FROM alias followed by a comma.
	`SELECT * FROM t4 window, t4`,
	// The pairs that differ ONLY in what follows the word -- this is where a
	// rule that guessed instead of looking ahead comes apart.
	`SELECT * FROM t4 window`,
	`SELECT * FROM t4 window w AS ()`,
	`SELECT * FROM t4 window WINDOW w AS ()`,
	`SELECT * FROM t4 window window AS ()`,
	`SELECT * FROM window window window w AS ()`,
	`SELECT window.x FROM t4 window`,
	`SELECT 1 window`,
	`SELECT 1 WINDOW w AS ()`,
	`SELECT count(*) OVER w window FROM t4 WINDOW w AS ()`,
	`SELECT count(*) OVER w WINDOW w AS ()`,
	// A QUOTED spelling is never the keyword.
	`SELECT sum(x) "over" FROM over`,
	`SELECT * FROM t4 "window"`,
	// altertab3.test 12.1's shape: OVER followed by punctuation.
	`SELECT sum(x) OVER, count(*) FROM over`,
}

func TestR25OverWindowKeywordParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range r25KwSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r25KwQ {
		ec, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] the oracle rejected the statement this gate is built on: %v", q, cerr)
			continue
		}
		if eerr != nil {
			t.Errorf("[%s] engine declined a statement C SQLite answers: %v", q, eerr)
			continue
		}
		// Column NAMES are half the point here: which reading won decides
		// whether the result column is named "over"/"window" (the alias) or
		// keeps its expression text.
		if len(ec) != len(cc) {
			t.Errorf("[%s] column count: engine %v, cgo %v", q, ec, cc)
			continue
		}
		for i := range ec {
			if ec[i] != cc[i] {
				t.Errorf("[%s] column %d named %q, C SQLite names it %q", q, i, ec[i], cc[i])
			}
		}
		eRows := engineRowsToStrings(ev)
		cols := make([]string, len(cc))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
