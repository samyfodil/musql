// This file gates aliased parenthesized join groups. C SQLite rebuilds the column
// list when aliased, affecting coalesced column order and disambiguation.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// r25GroupSchema is the test schema with three tables sharing column k.
var r25GroupSchema = []string{
	`CREATE TABLE a1(k, x)`,
	`CREATE TABLE a2(k, y)`,
	`CREATE TABLE a3(k, z)`,
	`INSERT INTO a1 VALUES(100,'x1'),(7,'x7')`,
	`INSERT INTO a2 VALUES(100,'y1')`,
	`INSERT INTO a3 VALUES(100,'z1'),(5,'z5')`,
	`CREATE TABLE b4(m)`,
	`INSERT INTO b4 VALUES('m')`,
}

func TestR25AliasedJoinGroupSweep(t *testing.T) {
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
	for _, s := range r25GroupSchema {
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

	// joins written between two tables.
	joins := []struct{ op, cond string }{
		{"JOIN", "ON 1"},
		{"JOIN", "USING(k)"},
		{"NATURAL JOIN", ""},
		{"LEFT JOIN", "USING(k)"},
		{"NATURAL LEFT JOIN", ""},
		{"RIGHT JOIN", "USING(k)"},
		{"NATURAL RIGHT JOIN", ""},
		{"FULL JOIN", "USING(k)"},
		{"NATURAL FULL JOIN", ""},
	}
	// Groups selected multiple ways.
	selects := []string{
		`SELECT * FROM %s ORDER BY 1,2`,
		`SELECT j.k FROM %s ORDER BY 1`,
		`SELECT j.k, j.x FROM %s ORDER BY 1,2`,
		`SELECT a1.k FROM %s ORDER BY 1`,
		`SELECT * FROM %s, b4 ORDER BY 1,2`,
		`SELECT * FROM %s FULL JOIN b4 ON true ORDER BY 1,2`,
	}

	n, declined, compared := 0, 0, 0
	for _, j1 := range joins {
		groups := []string{
			fmt.Sprintf("(a1 %s a2 %s) AS j", j1.op, j1.cond),
		}
		for _, j2 := range joins {
			groups = append(groups, fmt.Sprintf("(a1 %s a2 %s %s a3 %s) AS j", j1.op, j1.cond, j2.op, j2.cond))
		}
		for _, g := range groups {
			for _, sel := range selects {
				q := fmt.Sprintf(sel, g)
				n++
				ec, ev, eerr := p.QueryArgs(q, nil)
				cc, cr, cerr := cgoSelect(t, cdb, q, nil)
				if cerr != nil {
					// C SQLite rejects; engine must not answer.
					if eerr == nil {
						t.Errorf("[%s] engine answered what C SQLite rejects (%v)", q, cerr)
					}
					continue
				}
				if eerr != nil {
					declined++
					continue
				}
				compared++
				if len(ec) != len(cc) {
					t.Errorf("[%s] column count: engine %v, cgo %v", q, ec, cc)
					continue
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
	}
	t.Logf("aliased join group: %d statements, %d compared against C SQLite, %d declined", n, compared, declined)
	if compared == 0 && declined == 0 {
		t.Fatal("nothing was actually compared or declined -- the sweep is not exercising anything")
	}
}

// TestR25AliasedJoinGroupFrontMove checks aliased join groups move coalesced columns to the front.
func TestR25AliasedJoinGroupFrontMove(t *testing.T) {
	setup := []string{
		`CREATE TABLE ta(a,b)`, `CREATE TABLE td(b,g)`, `CREATE TABLE te(e,f)`, `CREATE TABLE tc(z)`,
		`INSERT INTO ta VALUES(1,2),(3,4)`,
		`INSERT INTO td VALUES(2,'x'),(4,'y')`,
		`INSERT INTO te VALUES(1,200),(3,300)`,
		`INSERT INTO tc VALUES('p')`,
	}
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
	for _, s := range setup {
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

	// The oracle's front-move, pinned. If a future SQLite stops doing this,
	// this fails and the decline below should be revisited -- nothing else
	// would notice.
	for _, tc := range []struct{ q, want string }{
		{`SELECT * FROM (ta JOIN td USING(b)) AS g`, "b,a,g"},
		{`SELECT * FROM (ta JOIN td USING(b))`, "a,b,g"},
		{`SELECT * FROM (ta NATURAL JOIN td) AS g`, "b,a,g"},
	} {
		cc, _, cerr := cgoSelect(t, cdb, tc.q, nil)
		if cerr != nil {
			t.Fatalf("[%s] oracle rejected the premise: %v", tc.q, cerr)
		}
		if got := strings.Join(cc, ","); got != tc.want {
			t.Fatalf("[%s] the oracle's column order changed: got %s, want %s -- re-measure before relying on the decline below", tc.q, got, tc.want)
		}
	}

	// Engine must still decline aliased spellings with "*".
	for _, q := range []string{
		`SELECT * FROM (ta JOIN td USING(b)) AS g`,
		`SELECT * FROM (ta NATURAL JOIN td) AS g`,
		`SELECT * FROM (ta NATURAL LEFT JOIN td NATURAL JOIN te) AS qq FULL JOIN tc ON true`,
	} {
		if _, _, eerr := p.QueryArgs(q, nil); eerr == nil {
			t.Errorf("[%s] engine ANSWERED it; SQLite moves the coalesced column to the front, so answering in FROM order is a wrong answer", q)
		}
	}
	// Queries without "*" shouldn't decline; coalesced column is not observable.
	if _, _, eerr := p.QueryArgs(`SELECT g.a FROM (ta JOIN td USING(b)) AS g`, nil); eerr != nil {
		t.Errorf("[%s] engine declined a shape nothing can observe the rebuild through: %v", `SELECT g.a FROM (ta JOIN td USING(b)) AS g`, eerr)
	}
	// Unaliased forms need no rebuild and must answer.
	for _, q := range []string{
		`SELECT * FROM (ta JOIN td USING(b))`,
		`SELECT * FROM (ta NATURAL JOIN td)`,
	} {
		if _, _, eerr := p.QueryArgs(q, nil); eerr != nil {
			t.Errorf("[%s] engine declined an UNALIASED group, which the front-move does not touch: %v", q, eerr)
		}
	}
}
