package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// Tests OR clauses on view INSERT/UPDATE and aliases on view UPDATE/DELETE.
func TestViewOrClausePromoted(t *testing.T) {
	base := []string{
		`CREATE TABLE b(a UNIQUE, c)`,
		`CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a, c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a, new.c); INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
		`INSERT INTO b VALUES(1,10)`,
	}
	// Every INSERT OR <action> spelling.
	for _, s := range []string{
		`INSERT OR IGNORE INTO v VALUES(2,20),(1,99),(3,30)`,
		`INSERT OR REPLACE INTO v VALUES(2,20),(1,99),(3,30)`,
		`INSERT OR FAIL INTO v VALUES(2,20),(1,99),(3,30)`,
		`INSERT OR ABORT INTO v VALUES(2,20),(1,99),(3,30)`,
		`INSERT OR ROLLBACK INTO v VALUES(2,20),(1,99),(3,30)`,
		`REPLACE INTO v VALUES(1,99)`,
		`INSERT OR IGNORE INTO v(a,c) SELECT 2, 20`,
		`INSERT OR REPLACE INTO v DEFAULT VALUES`,
		`UPDATE OR IGNORE v SET c = c + 1`,
		`UPDATE OR REPLACE v SET a = 1`,
		`UPDATE OR FAIL v SET c = c + 1 WHERE a = 1`,
	} {
		t.Run(s, func(t *testing.T) {
			flLockstep(t, s, append(append([]string(nil), base...), s),
				`SELECT a, c FROM b ORDER BY a`,
				`SELECT x FROM log ORDER BY rowid`,
				`SELECT changes(), total_changes()`)
		})
	}
}

// TestViewOrClauseGovernsTheBody tests OR policies on trigger body statements.
func TestViewOrClauseGovernsTheBody(t *testing.T) {
	base := []string{
		`CREATE TABLE b(a)`,
		`CREATE TABLE u(k UNIQUE, tag)`,
		`CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO u VALUES(new.a,'new'); INSERT INTO log VALUES(new.a); END`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO u VALUES(new.a,'new'); INSERT INTO log VALUES(new.a); END`,
		`INSERT INTO b VALUES(1)`,
		`INSERT INTO u VALUES(9,'old')`,
	}
	for _, s := range []string{
		`UPDATE OR IGNORE v SET a=9`,
		`UPDATE OR REPLACE v SET a=9`,
		`UPDATE v SET a=9`,
		`INSERT OR IGNORE INTO v VALUES(9)`,
		`INSERT OR REPLACE INTO v VALUES(9)`,
		`INSERT INTO v VALUES(9)`,
	} {
		t.Run(s, func(t *testing.T) {
			flLockstep(t, s, append(append([]string(nil), base...), s),
				`SELECT k, tag FROM u ORDER BY k`,
				`SELECT x FROM log ORDER BY rowid`)
		})
	}
}

// viewAliasBase is the base schema for view alias tests.
var viewAliasBase = []string{
	`CREATE TABLE b(a)`,
	`CREATE TABLE log(x)`,
	`CREATE VIEW v AS SELECT a FROM b`,
	`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
	`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(new.a); END`,
	`INSERT INTO b VALUES(1),(2)`,
}

func TestViewTargetAliasPromoted(t *testing.T) {
	// The four spellings measured against 3.53.3: with an alias present only
	// the UNQUALIFIED reference resolves in BOTH of the scopes the C uses, and
	// without one the view's own name resolves in the single scope there is.
	// The two ERROR rows are as load-bearing as the two that work -- they are
	// what stops the promotion from becoming a wrong ACCEPT.
	for _, s := range []string{
		`DELETE FROM v AS q WHERE a=2`,      // works
		`DELETE FROM v AS q WHERE q.a=2`,    // ERROR: dies in the materialization
		`DELETE FROM v AS q WHERE v.a=2`,    // ERROR: dies in the second resolve
		`DELETE FROM v WHERE v.a=2`,         // works: no alias, one scope
		`UPDATE v AS q SET a=9 WHERE a=2`,   // works
		`UPDATE v AS q SET a=9 WHERE q.a=2`, // ERROR
		`UPDATE v AS q SET a=9 WHERE v.a=2`, // ERROR
		`UPDATE v SET a=9 WHERE v.a=2`,      // works
	} {
		t.Run(s, func(t *testing.T) {
			flLockstep(t, s, append(append([]string(nil), viewAliasBase...), s),
				`SELECT x FROM log ORDER BY rowid`)
		})
	}
}

// TestViewTargetAliasStillDeclined pins the two aliased shapes 3.53.3 ANSWERS
// and viewAliasedTargetIgnorable still refuses, so the over-decline is a
// recorded cost rather than a silent one -- and so the day it stops being one,
// this test says so instead of rotting.
//
//   - a SUBQUERY in the WHERE: the qualifier walk does not enter one, and a
//     correlated "q."/"v." reference inside would meet the same two scopes;
//   - a SET right-hand side naming the alias: update.c resolves pChanges
//     against pTabList ALONE, so 3.53.3 accepts "SET a=q.a" while this engine's
//     view scan scope is named for the VIEW and cannot see "q" at all.
func TestViewTargetAliasStillDeclined(t *testing.T) {
	for _, s := range []string{
		`DELETE FROM v AS q WHERE a IN (SELECT a FROM b WHERE b.a=2)`,
		`UPDATE v AS q SET a=q.a WHERE a=2`,
	} {
		t.Run(s, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer edb.Close()
			cdb, cerr := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if cerr != nil {
				t.Fatalf("sql.Open: %v", cerr)
			}
			defer cdb.Close()
			cdb.SetMaxOpenConns(1)
			for _, setup := range viewAliasBase {
				if e := edb.Exec(setup); e != nil {
					t.Fatalf("engine setup %q: %v", setup, e)
				}
				if _, e := cdb.Exec(setup); e != nil {
					t.Fatalf("cgo setup %q: %v", setup, e)
				}
			}
			if _, e := cdb.Exec(s); e != nil {
				t.Fatalf("the ORACLE now rejects %q (%v) -- this test's premise is gone; re-measure before editing viewAliasedTargetIgnorable", s, e)
			}
			if e := edb.Exec(s); e == nil {
				t.Fatalf("%q is now SERVED. Move it into TestViewTargetAliasPromoted and drop it here -- but first check it answers what the oracle answers, not merely that it runs", s)
			}
		})
	}
}
