// Select-list aliases referenced from nested subqueries must follow
// C SQLite's visibility rules.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r25NestedAliasSchema = []string{
	`CREATE TABLE tz(x)`,
	`INSERT INTO tz VALUES(0)`,
	`CREATE TABLE ty(k,v)`,
	`INSERT INTO ty VALUES(1,10),(2,20)`,
}

// Every one of these C SQLite answers, and so must this engine.
var r25NestedAliasQ = []string{
	// Alias visible in WHERE subquery.
	`SELECT 2 AS x WHERE (SELECT x AS y WHERE 3>y)`,
	`SELECT 2 AS x WHERE (SELECT x AS y WHERE 1>y)`,
	// The nested statement's own alias shadows the outer one.
	`SELECT 2 AS x WHERE (SELECT 5 AS x WHERE x=2)`,
	`SELECT 2 AS x WHERE (SELECT 5 AS x WHERE x=5)`,
	// A nested FROM shadows it too -- tz.x is 0, not the alias's 2.
	`SELECT 2 AS x WHERE (SELECT 1 FROM tz WHERE x=2)`,
	`SELECT 2 AS x WHERE (SELECT 1 FROM tz WHERE x=0)`,
	// Two levels down, with the shadowing rules composing.
	`SELECT 2 AS x WHERE (SELECT (SELECT x))`,
	`SELECT 2 AS x WHERE (SELECT 1 WHERE (SELECT 3 AS x WHERE x=2))`,
	`SELECT 7 AS x WHERE (SELECT 1 WHERE (SELECT 1 FROM tz WHERE x=0))`,
	// The other two subquery-carrying expression kinds.
	`SELECT 2 AS x WHERE EXISTS(SELECT 1 WHERE x=2)`,
	`SELECT 2 AS x WHERE 3 IN (SELECT x+1)`,
	// The alias expression is not a bare literal.
	`SELECT 1+1 AS x WHERE (SELECT 1 WHERE x=2)`,
	`SELECT abs(-2) AS x WHERE (SELECT 1 WHERE x=2)`,
	`SELECT CASE WHEN 1 THEN 2 ELSE 9 END AS x WHERE (SELECT 1 WHERE x=2)`,
	// GROUP BY exposes the aliases exactly as WHERE does.
	`SELECT 2 AS x, k FROM ty GROUP BY (SELECT 1 WHERE x=2), k ORDER BY k`,
}

// r25NestedAliasDeclinedQ are shapes C SQLite ANSWERS and this engine
// deliberately does not: the alias expression is not scope-free, so splicing
// it into the nested subquery would carry names (a column, an aggregate) whose
// resolution level is exactly the question the descent avoids answering by
// guesswork. Declining costs coverage and can never be wrong; the list is here
// so the boundary is explicit and so serving one of them later shows up as a
// failure to be UPGRADED into the parity list above, not as silence.
var r25NestedAliasDeclinedQ = []string{
	`SELECT k, v+0 AS s FROM ty WHERE (SELECT s)>10`,
	`SELECT k, count(*) AS n FROM ty GROUP BY k HAVING (SELECT 1 WHERE n=1)`,
}

func TestR25NestedResultAliasParity(t *testing.T) {
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
	for _, s := range r25NestedAliasSchema {
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
	for _, q := range r25NestedAliasQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] the oracle rejected the statement this gate is built on: %v", q, cerr)
			continue
		}
		if eerr != nil {
			t.Errorf("[%s] engine declined a statement C SQLite answers: %v", q, eerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		cols := make([]string, len(cc))
		for i := range cols {
			cols[i] = "c"
		}
		if len(eRows) != len(cr) {
			t.Errorf("[%s] row count: engine %d %v, cgo %d %v", q, len(eRows), eRows, len(cr), cr)
			continue
		}
		for i := range eRows {
			for j := range eRows[i] {
				if eRows[i][j] != cr[i][j] {
					t.Errorf("[%s] DIVERGES at row %d col %d: engine %q, cgo %q", q, i, j, eRows[i][j], cr[i][j])
				}
			}
		}
	}
}

// The declined half: C SQLite answers these, this engine must ERROR rather
// than guess which level the alias expression's own names belong to.
func TestR25NestedResultAliasNonScopeFreeDeclines(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range r25NestedAliasSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r25NestedAliasDeclinedQ {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] now ANSWERED -- if it agrees with C SQLite, move it into r25NestedAliasQ", q)
		}
	}
}

// The half that must stay REJECTED. A subquery in the SELECT LIST cannot see
// the aliases at all -- C SQLite says "no such column" for every one of
// these -- so answering any of them is a wrong answer, not extra coverage.
func TestR25NestedResultAliasSelectListRejected(t *testing.T) {
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
	for _, s := range r25NestedAliasSchema {
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
	for _, q := range []string{
		`SELECT 2 AS x, (SELECT x) AS q`,
		`SELECT 2 AS x, (SELECT 7 AS z, x) AS q`,
		`SELECT 2 AS x, EXISTS(SELECT x WHERE x=2) AS q`,
		`SELECT 2 AS x, (SELECT 1 WHERE 3 IN (x, 9)) AS q`,
		`SELECT 2 AS x, (SELECT (SELECT x)) AS q`,
	} {
		if _, _, cerr := cgoSelect(t, cdb, q, nil); cerr == nil {
			t.Errorf("[%s] C SQLite ACCEPTED this; the premise of this gate is stale", q)
			continue
		}
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] engine answered a select-list subquery reading an outer alias; C SQLite rejects it", q)
		}
	}
}
