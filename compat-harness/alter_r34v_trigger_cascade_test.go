package compat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestR34VAlterTriggerCascade verifies ALTER TABLE's cascade into dependent
// triggers, testing 516 (body x WHEN x ALTER) combinations and comparing both
// the stored trigger text and the rows it produces.
func TestR34VAlterTriggerCascade(t *testing.T) {
	for _, b := range r34vBodies {
		for _, w := range r34vWhens {
			if !r34vWhenFits(b, w) {
				continue
			}
			for _, a := range r34vAlters {
				name := fmt.Sprintf("%s/%s/%s", b.name, w.name, a.name)
				t.Run(name, func(t *testing.T) {
					if b.openOn != "" && b.openOn == a.name {
						r34vAssertStillOpen(t, name, r34vCase(b, w, a))
						return
					}
					differ(t, "r34v-"+name, r34vCase(b, w, a))
				})
			}
		}
	}
}

type r34vBody struct {
	name             string
	event            string // trigger event clause
	sql              string // body between BEGIN and END
	usesNew, usesOld bool   // constrain WHEN clauses
	openOn           string // known-unsupported ALTER
}

type r34vWhen struct {
	name             string
	sql              string // WHEN expression
	usesNew, usesOld bool
}

type r34vAlter struct {
	name string
	sql  string
}

// r34vBodies covers trigger bodies naming t1's columns from various positions.
var r34vBodies = []r34vBody{
	{"values", "AFTER INSERT", `INSERT INTO t1(a,b) VALUES(new.p, new.p);`, true, false, ""},
	{"insert-select", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, b FROM t1;`, false, false, ""},
	{"insert-select-where", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, b FROM t1 WHERE b = new.p;`, true, false, ""},
	{"groupby", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT count(*), 1 FROM t1 GROUP BY b;`, false, false, ""},
	{"groupby-having", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT count(*), 1 FROM t1 GROUP BY a HAVING count(b)>0;`, false, false, ""},
	{"groupby-alias", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT b AS z, count(*) FROM t1 GROUP BY z HAVING z IS NOT NULL;`, false, false, ""},
	{"compound", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, a FROM t1 UNION ALL SELECT b, b FROM t1;`, false, false, ""},
	{"compound-orderby", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, a FROM t1 UNION ALL SELECT b, b FROM t1 ORDER BY 1;`, false, false, ""},
	{"cte-named-a", "AFTER INSERT", `INSERT INTO t1(a,b) WITH a AS (SELECT 1 AS z) SELECT z, z FROM a;`, false, false, ""},
	{"cte-named-b", "AFTER INSERT", `INSERT INTO t1(a,b) WITH b AS (SELECT 1 AS z) SELECT z, z FROM b;`, false, false, ""},
	{"cte-collist", "AFTER INSERT", `INSERT INTO t1(a,b) WITH a(a) AS (SELECT 1) SELECT a, a FROM a;`, false, false, "rename-a"},
	{"alias-a", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a.a, a.b FROM t1 AS a;`, false, false, ""},
	{"alias-bare", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT x.a, x.b FROM t1 x;`, false, false, ""},
	{"derived-alias-a", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a.z, a.z FROM (SELECT 1 AS z) AS a;`, false, false, ""},
	{"comma-join", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT t1.a, t1.b FROM t1, t2;`, false, false, ""},
	{"inner-join-on", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT t1.a, t1.b FROM t1 JOIN t2 ON t1.a = t2.p;`, false, false, ""},
	{"derived", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, b FROM (SELECT a, b FROM t1);`, false, false, ""},
	{"scalar-subquery", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT 1, (SELECT max(b) FROM t1);`, false, false, ""},
	{"orderby", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, b FROM t1 ORDER BY b;`, false, false, ""},
	{"window", "AFTER INSERT", `INSERT INTO t1(a,b) SELECT a, count(*) OVER w FROM t1 WINDOW w AS (PARTITION BY b);`, false, false, ""},
	{"upsert", "AFTER INSERT", `INSERT INTO t1(a,b) VALUES(new.p,1) ON CONFLICT(a) DO UPDATE SET b=b+1;`, true, false, ""},
	{"update-set", "AFTER UPDATE", `UPDATE t1 SET a = b WHERE b IS NOT NULL;`, false, false, ""},
	{"update-set-new", "AFTER UPDATE", `UPDATE t1 SET b = new.p WHERE a = old.p;`, true, true, ""},
	{"delete-where", "AFTER DELETE", `DELETE FROM t1 WHERE a = old.p;`, false, true, ""},
	{"bare-select", "AFTER INSERT", `SELECT b FROM t1;`, false, false, ""},
	{"bare-select-groupby", "AFTER INSERT", `SELECT count(*) FROM t1 GROUP BY b;`, false, false, ""},
	{"raise", "BEFORE INSERT", `SELECT RAISE(IGNORE) WHERE new.p < 0;`, true, false, ""},
	{"two-steps", "AFTER INSERT", `INSERT INTO t1(a,b) VALUES(1,2); UPDATE t1 SET b = a;`, false, false, ""},
}

var r34vWhens = []r34vWhen{
	{"nowhen", "", false, false},
	{"when-new", "new.p > 0", true, false},
	{"when-old", "old.p IS NOT NULL", false, true},
	{"when-sub", "EXISTS(SELECT 1 FROM t1 WHERE b IS NOT NULL)", false, false},
}

// r34vAlters covers both rename directions, the drop, and the table rename --
// the four forms whose cascade rewrites (or must refuse to rewrite) a trigger.
var r34vAlters = []r34vAlter{
	{"rename-a", `ALTER TABLE t1 RENAME COLUMN a TO aaa`},
	{"rename-b", `ALTER TABLE t1 RENAME COLUMN b TO bbb`},
	{"drop-b", `ALTER TABLE t1 DROP COLUMN b`},
	{"rename-table", `ALTER TABLE t1 RENAME TO t1x`},
	{"add-col", `ALTER TABLE t1 ADD COLUMN c`},
	{"rename-p", `ALTER TABLE t2 RENAME COLUMN p TO ppp`},
}

// r34vAssertStillOpen runs a case this engine is KNOWN to decline and fails only
// if it has started AGREEING with the oracle -- the self-verifying form of a
// known-gap marker, so the marker cannot outlive the gap.
//
// The one case it guards is a CTE whose declared COLUMN list rebinds the renamed
// name ("WITH a(a) AS (SELECT 1) SELECT a, a FROM a"): both select-list "a"s
// resolve to the CTE's column, so 3.53.3 rewrites neither, and the two positions
// a whole-text rewrite CAN tell apart (the WITH prelude's name and its FROM
// reference, r34vBoundNameToken in engine/alter_write.go) are not enough here.
// engine/alter_write.go's r34vCTEOutputsColumn therefore declines the rename.
// Serving it needs the rewrite to be driven by RESOLUTION rather than by token
// position -- i.e. a byte span recorded on every ColumnExpr, not just the bare
// double-quoted one (ColumnExpr.R32NDQSpan, engine/sql_ast.go) -- at which point
// this marker must be deleted along with r34vCTEOutputsColumn's CTE branch.
func r34vAssertStillOpen(t *testing.T, name string, stmts []string) {
	t.Helper()
	got := map[string][]byte{}
	for _, eng := range engineOrder {
		b, _ := json.Marshal(run(t, eng, stmts))
		got[eng] = b
	}
	if string(got["cgo"]) == string(got["musql"]) {
		t.Fatalf("[%s] now AGREES with C SQLite. The known gap is closed: delete this body's openOn marker so the case joins the sweep, and re-check whether engine/alter_write.go's r34vCTEOutputsColumn branch is still needed.\n  sql: %v", name, stmts)
	}
}

// r34vWhenFits keeps a WHEN clause off a trigger whose event does not define the
// pseudo-row it names (C SQLite rejects "old" on an INSERT trigger and "new"
// on a DELETE one, so the CREATE would fail identically on both engines and the
// case would measure nothing).
func r34vWhenFits(b r34vBody, w r34vWhen) bool {
	usesNew, usesOld := b.usesNew || w.usesNew, b.usesOld || w.usesOld
	switch {
	case strings.HasSuffix(b.event, "INSERT"):
		return !usesOld
	case strings.HasSuffix(b.event, "DELETE"):
		return !usesNew
	}
	return true
}

func r34vCase(b r34vBody, w r34vWhen, a r34vAlter) []string {
	when := ""
	if w.sql != "" {
		when = " WHEN " + w.sql
	}
	return []string{
		`CREATE TABLE t1(a UNIQUE, b)`,
		`CREATE TABLE t2(p, q)`,
		`INSERT INTO t1 VALUES(1,10),(2,20)`,
		`CREATE TRIGGER tr ` + b.event + ` ON t2` + when + ` BEGIN ` + b.sql + ` END`,
		a.sql,
		// The stored text, exactly as rewritten (or not).
		`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name, sql`,
		// ...and what the trigger DOES afterwards: fire it three ways, so an
		// INSERT/UPDATE/DELETE trigger all get their turn on one statement list.
		`INSERT INTO t2 VALUES(1,2)`,
		`UPDATE t2 SET q = q + 1`,
		`DELETE FROM t2 WHERE p = 1`,
		// Whichever name t1 now has; the other errors on both engines.
		`SELECT * FROM t1 ORDER BY rowid`,
		`SELECT * FROM t1x ORDER BY rowid`,
		`SELECT * FROM t2 ORDER BY rowid`,
		// PRAGMA-level view of the same schema, in case the text agrees and the
		// live objects do not.
		`PRAGMA table_info(t1)`,
	}
}

// TestR34VRenameColumnForeignKeyParent pins renameColumnFunc's FKey branch
// (alter.c:1626): renaming a PARENT table's column rewrites every CHILD's
// "REFERENCES <parent>(<cols>)" list that names it.
//
// This engine left the child reading the OLD name -- a wrong answer, and not a
// cosmetic one, since fk.go resolves foreign keys out of that stored text. It
// was invisible until the sqlite_master.sql trigger clause was retired, because
// altercol.test's own readback ("SELECT sql FROM sqlite_master") runs over a
// schema that holds a trigger; the mined corpus found it the moment the read
// was answered.
func TestR34VRenameColumnForeignKeyParent(t *testing.T) {
	differ(t, "r34v-fk-parent-col", []string{
		`CREATE TABLE p1(c, d, PRIMARY KEY(c, d))`,
		`CREATE TABLE c1(a, b, FOREIGN KEY (a, b) REFERENCES p1(c, d))`,
		// No column list: nothing to rewrite, and the "(" test must not run off
		// the end of the statement looking for one.
		`CREATE TABLE c2(a, b, FOREIGN KEY (a, b) REFERENCES p1)`,
		// A same-named column of an UNRELATED table stays put.
		`CREATE TABLE other(d)`,
		`ALTER TABLE p1 RENAME COLUMN d TO "silly name"`,
		`SELECT name, sql FROM sqlite_master ORDER BY name`,
	})
	// A SELF-referential key: the same text is both parent and child.
	differ(t, "r34v-fk-parent-col-self", []string{
		`CREATE TABLE p1(c PRIMARY KEY, d REFERENCES p1(c))`,
		`ALTER TABLE p1 RENAME COLUMN c TO ccc`,
		`SELECT name, sql FROM sqlite_master ORDER BY name`,
	})
	// A quoted old name, and a new one that needs no quotes.
	differ(t, "r34v-fk-parent-col-quoted", []string{
		`CREATE TABLE p1(c, "d e", PRIMARY KEY(c, "d e"))`,
		`CREATE TABLE c1(a, b, FOREIGN KEY (a, b) REFERENCES p1(c, "d e"))`,
		`ALTER TABLE p1 RENAME COLUMN "d e" TO reasonable`,
		`SELECT name, sql FROM sqlite_master ORDER BY name`,
	})
	// A key pointing at a DIFFERENT parent must not be touched, even though its
	// column list spells the renamed name.
	differ(t, "r34v-fk-parent-col-not-parent", []string{
		`CREATE TABLE p1(c, d)`,
		`CREATE TABLE p2(c, d, PRIMARY KEY(c,d))`,
		`CREATE TABLE c1(a, b, FOREIGN KEY (a, b) REFERENCES p2(c, d))`,
		`ALTER TABLE p1 RENAME COLUMN d TO ddd`,
		`SELECT name, sql FROM sqlite_master ORDER BY name`,
	})
}
