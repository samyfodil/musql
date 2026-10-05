// This file gates the schema re-validation C SQLite runs before an ALTER
// TABLE: a trigger or view left dangling by an earlier DROP makes a rename
// fail, even when it is unrelated to the altered table. ADD COLUMN and DROP
// TABLE do not re-parse, and only a missing table trips it.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// alterRevalidateCases: "keep" and "t1" both exist; "ff" never does.
var alterRevalidateCases = []struct{ name, setup, alter string }{
	// Re-parsed forms: a dangling trigger/view fails the ALTER.
	{"rename to, trigger on the table", `CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN INSERT INTO ff VALUES(1); END`, `ALTER TABLE t1 RENAME TO t9`},
	{"rename to, trigger on another table", `CREATE TRIGGER tr2 AFTER INSERT ON keep BEGIN INSERT INTO ff VALUES(1); END`, `ALTER TABLE t1 RENAME TO t9`},
	{"rename to, view over missing table", `CREATE VIEW v1 AS SELECT * FROM ff`, `ALTER TABLE t1 RENAME TO t9`},
	{"rename column", `CREATE TRIGGER tr3 AFTER INSERT ON keep BEGIN INSERT INTO ff VALUES(1); END`, `ALTER TABLE t1 RENAME COLUMN a TO aa`},
	{"drop column", `CREATE TRIGGER tr5 AFTER INSERT ON keep BEGIN INSERT INTO ff VALUES(1); END`, `ALTER TABLE t1 DROP COLUMN b`},
	{"trigger body SELECT over missing table", `CREATE TRIGGER tr6 AFTER INSERT ON keep BEGIN SELECT * FROM ff; END`, `ALTER TABLE t1 RENAME TO t9`},

	// Boundaries: these must all stay ACCEPTED.
	{"add column does not re-parse", `CREATE TRIGGER tr4 AFTER INSERT ON keep BEGIN INSERT INTO ff VALUES(1); END`, `ALTER TABLE t1 ADD COLUMN c`},
	{"drop table does not re-parse", `CREATE TRIGGER tr9 AFTER INSERT ON keep BEGIN INSERT INTO ff VALUES(1); END`, `DROP TABLE t1`},
	{"missing function is fine", `CREATE TRIGGER tr7 AFTER INSERT ON keep BEGIN SELECT nofunc(1); END`, `ALTER TABLE t1 RENAME TO t9`},
	{"missing column is fine", `CREATE TRIGGER tr8 AFTER INSERT ON keep BEGIN INSERT INTO t1 VALUES(new.x, 1); END`, `ALTER TABLE t1 RENAME TO t9`},
	{"clean schema renames", `CREATE TRIGGER trA AFTER INSERT ON keep BEGIN INSERT INTO t1 VALUES(1,2); END`, `ALTER TABLE t1 RENAME TO t9`},
	// A WITH-bound name is NOT a table: it must not be reported missing.
	{"CTE name is not a table", `CREATE VIEW v2 AS WITH cte AS (SELECT 1 AS z) SELECT * FROM cte`, `ALTER TABLE t1 RENAME TO t9`},
	// A derived table has no table name of its own.
	{"derived table", `CREATE VIEW v3 AS SELECT * FROM (SELECT * FROM keep)`, `ALTER TABLE t1 RENAME TO t9`},

	// CTEs referencing themselves, a sibling, or an outer WITH are not dangling
	// table references.
	{
		"self-referencing CTE (RECURSIVE keyword not required for name resolution)",
		// Columns p/q/r avoid colliding with keep's column x.
		`CREATE VIEW v10 AS
		 WITH t3(p,q,r) AS (
		   SELECT 1,2,NULL FROM keep
		   UNION
		   SELECT p,q,NULL FROM t3, keep
		 )
		 SELECT * FROM t3`,
		`ALTER TABLE t1 RENAME TO t9`,
	},
	{
		"sibling CTE reference within the same WITH clause",
		`CREATE VIEW v11 AS
		 WITH p AS ( SELECT 1 FROM keep ),
		      g AS ( SELECT 1 FROM p, keep )
		 SELECT 1 FROM g`,
		`ALTER TABLE t1 RENAME TO t9`,
	},
	{
		"nested WITH referencing an outer CTE's name",
		`CREATE VIEW v12 AS WITH x AS (WITH y AS (SELECT * FROM x) SELECT 1) SELECT 1`,
		`ALTER TABLE t1 RENAME TO t9`,
	},
	// A dangling name beside a legitimate CTE reference must still block the ALTER.
	{
		"non-CTE dangling name beside a real CTE still fails (control)",
		`CREATE VIEW v13 AS WITH p AS ( SELECT 1 FROM keep ) SELECT 1 FROM p, ff`,
		`ALTER TABLE t1 RENAME TO t9`,
	},
	// A view over a VIEW resolves too (views count as tables here).
	{"view over a view", `CREATE VIEW v4 AS SELECT * FROM keep`, `ALTER TABLE t1 RENAME TO t9`},
}

func TestAlterRevalidatesSchemaParity(t *testing.T) {
	for _, tc := range alterRevalidateCases {
		t.Run(tc.name, func(t *testing.T) {
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

			for _, s := range []string{`CREATE TABLE t1(a,b)`, `CREATE TABLE keep(x)`, tc.setup} {
				eErr := edb.Exec(s)
				_, cErr := cdb.Exec(s)
				if (eErr == nil) != (cErr == nil) {
					t.Fatalf("setup [%s] disagrees\n  engine=%v\n  cgo=%v", s, eErr, cErr)
				}
			}

			eErr := edb.Exec(tc.alter)
			_, cErr := cdb.Exec(tc.alter)
			if (eErr == nil) != (cErr == nil) {
				t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", tc.alter, eErr, cErr)
			}
			if eErr != nil {
				if got := strings.TrimPrefix(eErr.Error(), "engine: "); got != cErr.Error() {
					t.Errorf("[%s] error text mismatch\n  engine: %q\n  cgo:    %q", tc.alter, got, cErr.Error())
				}
			}
		})
	}
}

// alterRenameShadowCases checks shadowing: with a TEMP table of the same name
// as a MAIN one, an unqualified ALTER resolves to the temp table and leaves the
// main catalog's objects (triggers, foreign keys, views, indexes) untouched.
// A temp trigger on the main table being altered must still block it.
var alterRenameShadowCases = []struct {
	name     string
	setup    []string
	alter    string
	checkSQL []string // run after alter; every row/column compared
	// checkExec runs after checkSQL and compares only exec-error agreement, which
	// shows a trigger's body is unchanged by checking that it still fires.
	checkExec []string
	// checkSQLAfterExec runs after checkExec, to verify its side effect.
	checkSQLAfterExec []string
}{
	{
		name: "shadowed: main trigger with a stale bad column is untouched",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TRIGGER tt1 AFTER DELETE ON t1 WHEN old.nope=1 BEGIN SELECT 1; END`,
			`CREATE TEMP TABLE t1(c PRIMARY KEY)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT type,name FROM sqlite_master WHERE name='tt1'`,
			`SELECT type,name FROM sqlite_master WHERE name='t1'`,
			`SELECT type,name FROM sqlite_temp_master WHERE type='table'`,
		},
		// "t1" now names only the main table, so this fires tt1, whose body still
		// references the same missing column.
		checkExec: []string{
			`INSERT INTO t1 VALUES(1,2)`,
			`DELETE FROM t1`,
		},
	},
	{
		name: "shadowed: main table's REFERENCES clause is untouched",
		setup: []string{
			`CREATE TABLE t1(a,b PRIMARY KEY)`,
			`CREATE TABLE t3(x REFERENCES t1)`,
			`CREATE TEMP TABLE t1(c PRIMARY KEY)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT sql FROM sqlite_master WHERE name='t3'`,
		},
	},
	{
		name: "shadowed: main trigger's ON clause is untouched",
		setup: []string{
			`CREATE TABLE t1(a,b PRIMARY KEY)`,
			`CREATE TABLE log(who)`,
			`CREATE TRIGGER trg1 AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('t1'); END`,
			`CREATE TEMP TABLE t1(c PRIMARY KEY)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT type,name FROM sqlite_master WHERE name='trg1'`,
		},
		// trg1 fires for an insert into main t1 and must not fire for one into t9.
		checkExec: []string{
			`INSERT INTO t1(a,b) VALUES(1,2)`,
			`INSERT INTO t9(c) VALUES(3)`,
		},
		checkSQLAfterExec: []string{
			`SELECT who FROM log`,
		},
	},
	{
		name: "shadowed: main view is untouched",
		setup: []string{
			`CREATE TABLE t1(a,b PRIMARY KEY)`,
			`CREATE VIEW vv AS SELECT * FROM t1`,
			`CREATE TEMP TABLE t1(c PRIMARY KEY)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT sql FROM sqlite_master WHERE name='vv'`,
		},
	},
	{
		name: "shadowed: main table's own index (explicit and automatic) is untouched",
		setup: []string{
			`CREATE TABLE t1(a,b PRIMARY KEY)`,
			`CREATE INDEX ix1 ON t1(a)`,
			`CREATE TEMP TABLE t1(c PRIMARY KEY)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT type,name,tbl_name,sql FROM sqlite_master WHERE type='index'`,
		},
	},
	{
		// Without a shadow, a temp trigger on the main table being altered still
		// blocks the ALTER.
		name: "unshadowed: temp trigger on the altered main table still blocks",
		setup: []string{
			`CREATE TABLE m1(x PRIMARY KEY)`,
			`CREATE TEMP TRIGGER ttx AFTER DELETE ON m1 WHEN old.nope=1 BEGIN SELECT 1; END`,
		},
		alter: `ALTER TABLE m1 RENAME TO m9`,
	},
}

func TestAlterRenameShadowedSchema(t *testing.T) {
	for _, tc := range alterRenameShadowCases {
		t.Run(tc.name, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1)

			for _, s := range tc.setup {
				execErr, panicked, panicVal := tclSafeExecArgs(godb, s)
				if panicked {
					t.Fatalf("setup %q: engine PANICKED: %v", s, panicVal)
				}
				_, cerr := cgodb.Exec(s)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("setup %q disagrees\n  go:  %v\n  cgo: %v", s, execErr, cerr)
				}
			}

			execErr, panicked, panicVal := tclSafeExecArgs(godb, tc.alter)
			if panicked {
				t.Fatalf("alter %q: engine PANICKED: %v", tc.alter, panicVal)
			}
			_, cerr := cgodb.Exec(tc.alter)
			if (execErr != nil) != (cerr != nil) {
				t.Fatalf("alter %q accept/reject disagrees\n  go:  %v\n  cgo: %v", tc.alter, execErr, cerr)
			}
			if execErr != nil {
				return // rejected on both sides -- nothing left to compare
			}

			runCheckSQL := func(q string) {
				goCols, goRows, qerr, qpanicked, qpanicVal := tclSafeGoQuery(godb, q)
				if qpanicked {
					t.Fatalf("check %q: engine PANICKED: %v", q, qpanicVal)
				}
				if qerr != nil {
					t.Fatalf("check %q: engine query error: %v", q, qerr)
				}
				cgoCols, cgoRows, cqerr := tclRunCGOQuery(cgodb, q)
				if cqerr != nil {
					t.Fatalf("check %q: cgo query error: %v", q, cqerr)
				}
				if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
					t.Fatalf("check %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", q, reason, goCols, goRows, cgoCols, cgoRows)
				}
			}
			for _, q := range tc.checkSQL {
				runCheckSQL(q)
			}
			for _, s := range tc.checkExec {
				execErr, panicked, panicVal := tclSafeExecArgs(godb, s)
				if panicked {
					t.Fatalf("checkExec %q: engine PANICKED: %v", s, panicVal)
				}
				_, cerr := cgodb.Exec(s)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("checkExec %q disagrees\n  go:  %v\n  cgo: %v", s, execErr, cerr)
				}
			}
			for _, q := range tc.checkSQLAfterExec {
				runCheckSQL(q)
			}
		})
	}
}

// ---- the cross-catalog half of the same rule ----
//
// The rule is asymmetric: renaming a MAIN table also rewrites TEMP views and
// triggers, while renaming a TEMP table never touches MAIN. See
// alterSkipsCatalog in engine/alter_write.go.
var alterCrossCatalogCases = []struct {
	name              string
	setup             []string
	alter             string
	checkSQL          []string
	checkExec         []string
	checkSQLAfterExec []string
	// wantEngineDecline marks a shape C SQLite accepts and this engine refuses
	// (see renameTriggerReferences). The engine must reject it and leave the
	// schema unchanged.
	wantEngineDecline bool
	// afterDecline runs on the engine only and checks that the refusal changed
	// nothing. Entries are "query -> expected cell" in type-tagged form.
	afterDecline map[string]string
}{
	{
		// A MAIN rename must rewrite a TEMP view's body.
		name: "main rename rewrites a temp view",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TEMP VIEW tv AS SELECT a FROM t1`,
		},
		alter:    `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{`SELECT type,name,sql FROM sqlite_temp_master ORDER BY name`},
	},
	{
		// ...and a TEMP trigger's ON clause, so it keeps firing.
		name: "main rename rewrites a temp trigger's ON clause",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TABLE log(who)`,
			`CREATE TEMP TRIGGER ton AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES('fired'); END`,
		},
		alter:             `ALTER TABLE t1 RENAME TO t9`,
		checkSQL:          []string{`SELECT type,name,tbl_name FROM sqlite_temp_master ORDER BY name`},
		checkExec:         []string{`INSERT INTO t9(a,b) VALUES(1,2)`},
		checkSQLAfterExec: []string{`SELECT who FROM log`},
	},
	{
		// ...and a TEMP trigger's body target.
		name: "main rename rewrites a temp trigger's body target",
		setup: []string{
			`CREATE TABLE t1(a)`,
			`CREATE TABLE drv(x)`,
			`CREATE TEMP TRIGGER ttr AFTER INSERT ON drv BEGIN INSERT INTO t1(a) VALUES(7); END`,
		},
		alter:             `ALTER TABLE t1 RENAME TO t9`,
		checkExec:         []string{`INSERT INTO drv VALUES(1)`},
		checkSQLAfterExec: []string{`SELECT a FROM t9`},
	},
	{
		// A dangling TEMP trigger fails a MAIN table's ALTER.
		name: "main rename is blocked by a dangling temp trigger",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TABLE keep(a)`,
			`CREATE TEMP TABLE tmpgone(a)`,
			`CREATE TEMP TRIGGER ttr AFTER INSERT ON keep BEGIN INSERT INTO tmpgone VALUES(1); END`,
			`DROP TABLE tmpgone`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
	},
	{
		// A TEMP table's ALTER never looks at MAIN, so a dangling MAIN trigger does
		// not block it.
		name: "temp rename is NOT blocked by a dangling main trigger",
		setup: []string{
			`CREATE TABLE keep(a)`,
			`CREATE TABLE gone(a)`,
			`CREATE TRIGGER mtr AFTER INSERT ON keep BEGIN INSERT INTO gone VALUES(1); END`,
			`CREATE TEMP TABLE tt(a)`,
			`DROP TABLE gone`,
		},
		alter:    `ALTER TABLE tt RENAME TO tt9`,
		checkSQL: []string{`SELECT type,name FROM sqlite_temp_master ORDER BY name`},
	},
	{
		// A TEMP rename leaves every MAIN object alone.
		name: "temp rename leaves main view, FK and index untouched",
		setup: []string{
			`CREATE TABLE t1(a PRIMARY KEY)`,
			`CREATE TABLE t3(x REFERENCES t1)`,
			`CREATE VIEW mv AS SELECT a FROM t1`,
			`CREATE INDEX ix1 ON t1(a)`,
			`CREATE TEMP TABLE t1(a)`,
			`CREATE INDEX temp.tix ON t1(a)`,
		},
		alter: `ALTER TABLE t1 RENAME TO t9`,
		checkSQL: []string{
			`SELECT type,name,tbl_name,sql FROM main.sqlite_master ORDER BY name`,
			`SELECT type,name,tbl_name,sql FROM sqlite_temp_master ORDER BY name`,
		},
	},
	{
		// A TEMP rename does reach a temp trigger on a MAIN table whose body writes
		// the renamed temp table.
		name: "temp rename rewrites a temp trigger bound to a main table",
		setup: []string{
			`CREATE TABLE keep(a)`,
			`CREATE TEMP TABLE tt(a)`,
			`CREATE TEMP TRIGGER ttr AFTER INSERT ON keep BEGIN INSERT INTO tt VALUES(5); END`,
		},
		alter:             `ALTER TABLE tt RENAME TO tt9`,
		checkExec:         []string{`INSERT INTO keep VALUES(1)`},
		checkSQLAfterExec: []string{`SELECT a FROM tt9`},
	},
	{
		// A temp view resolves the bare name temp-first, so it means the shadow and
		// must be left as written.
		name: "shadowed main rename leaves a temp view untouched",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TEMP TABLE t1(a,b)`,
			`CREATE TEMP VIEW tv AS SELECT a FROM t1`,
		},
		alter:    `ALTER TABLE main.t1 RENAME TO t9`,
		checkSQL: []string{`SELECT type,name,sql FROM sqlite_temp_master ORDER BY name`},
	},
	{
		// A TEMP trigger on a MAIN table with a TEMP shadow of that name: its ON
		// clause follows the rename and it must still fire.
		name: "shadowed main rename keeps a temp trigger firing (altertab 12.x)",
		setup: []string{
			`CREATE TABLE t1(a)`,
			`CREATE TABLE t2(w)`,
			`CREATE TEMP TRIGGER r1 AFTER INSERT ON main.t2 BEGIN INSERT INTO t1(a) VALUES(new.w); END`,
			`CREATE TEMP TABLE t2(x)`,
		},
		alter:             `ALTER TABLE main.t2 RENAME TO t3`,
		checkExec:         []string{`INSERT INTO t3 VALUES('WWW')`},
		checkSQLAfterExec: []string{`SELECT a FROM t1`},
	},
	{
		// RENAME COLUMN reaches temp views and triggers the same way...
		name: "main rename-column rewrites a temp view",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TEMP VIEW tv AS SELECT a FROM t1`,
		},
		alter:    `ALTER TABLE t1 RENAME COLUMN a TO z`,
		checkSQL: []string{`SELECT type,name,sql FROM sqlite_temp_master ORDER BY name`},
	},
	{
		// ...but with a shadow it reaches neither, since references resolve by identity.
		name: "shadowed main rename-column leaves temp objects untouched",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE TABLE other(x)`,
			`CREATE TEMP TABLE t1(a,b)`,
			`CREATE TEMP VIEW tv AS SELECT a FROM t1`,
		},
		alter:    `ALTER TABLE main.t1 RENAME COLUMN a TO z`,
		checkSQL: []string{`SELECT type,name,sql FROM sqlite_temp_master ORDER BY name`},
	},
	{
		// ...and a TEMP rename-column never reaches MAIN.
		name: "temp rename-column leaves a main view untouched",
		setup: []string{
			`CREATE TABLE t1(a,b)`,
			`CREATE VIEW mv AS SELECT a FROM t1`,
			`CREATE TEMP TABLE t1(a,b)`,
		},
		alter: `ALTER TABLE t1 RENAME COLUMN a TO z`,
		checkSQL: []string{
			`SELECT type,name,sql FROM main.sqlite_master ORDER BY name`,
			`SELECT type,name,sql FROM sqlite_temp_master ORDER BY name`,
		},
	},
	{
		// The one deliberate decline. C SQLite rewrites the body's step target by
		// name but leaves the FROM item on the temp shadow; a whole-text rewrite
		// cannot produce both, so this engine refuses.
		name: "shadowed main rename with a split temp trigger is declined",
		setup: []string{
			`CREATE TABLE t1(a)`,
			`CREATE TEMP TABLE t1(a)`,
			`CREATE TEMP TABLE drv(x)`,
			`CREATE TEMP TRIGGER ttr AFTER INSERT ON drv BEGIN INSERT INTO t1(a) SELECT a FROM t1; END`,
		},
		alter:             `ALTER TABLE main.t1 RENAME TO t9`,
		wantEngineDecline: true,
		afterDecline: map[string]string{
			`SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name='t1'`: "I:1",
			`SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name='t9'`: "I:0",
		},
	},
}

func TestAlterCrossCatalogSchema(t *testing.T) {
	for _, tc := range alterCrossCatalogCases {
		t.Run(tc.name, func(t *testing.T) {
			godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer godb.Discard()
			cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer cgodb.Close()
			cgodb.SetMaxOpenConns(1)

			for _, s := range tc.setup {
				execErr, panicked, panicVal := tclSafeExecArgs(godb, s)
				if panicked {
					t.Fatalf("setup %q: engine PANICKED: %v", s, panicVal)
				}
				_, cerr := cgodb.Exec(s)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("setup %q disagrees\n  go:  %v\n  cgo: %v", s, execErr, cerr)
				}
			}

			execErr, panicked, panicVal := tclSafeExecArgs(godb, tc.alter)
			if panicked {
				t.Fatalf("alter %q: engine PANICKED: %v", tc.alter, panicVal)
			}
			_, cerr := cgodb.Exec(tc.alter)

			if tc.wantEngineDecline {
				if execErr == nil {
					t.Fatalf("alter %q: engine ACCEPTED a shape this path must decline", tc.alter)
				}
				if cerr != nil {
					t.Fatalf("alter %q: premise gone -- cgo now rejects it too: %v", tc.alter, cerr)
				}
				for q, want := range tc.afterDecline {
					_, rows, qerr, qpanicked, qpanicVal := tclSafeGoQuery(godb, q)
					if qpanicked {
						t.Fatalf("afterDecline %q: engine PANICKED: %v", q, qpanicVal)
					}
					if qerr != nil {
						t.Fatalf("afterDecline %q: engine query error: %v", q, qerr)
					}
					if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != want {
						t.Fatalf("afterDecline %q: the refused ALTER changed the schema\n  got  %v\n  want [[%s]]", q, rows, want)
					}
				}
				return
			}

			if (execErr != nil) != (cerr != nil) {
				t.Fatalf("alter %q accept/reject disagrees\n  go:  %v\n  cgo: %v", tc.alter, execErr, cerr)
			}
			if execErr != nil {
				return // rejected on both sides -- nothing left to compare
			}

			runCheckSQL := func(q string) {
				goCols, goRows, qerr, qpanicked, qpanicVal := tclSafeGoQuery(godb, q)
				if qpanicked {
					t.Fatalf("check %q: engine PANICKED: %v", q, qpanicVal)
				}
				if qerr != nil {
					t.Fatalf("check %q: engine query error: %v", q, qerr)
				}
				cgoCols, cgoRows, cqerr := tclRunCGOQuery(cgodb, q)
				if cqerr != nil {
					t.Fatalf("check %q: cgo query error: %v", q, cqerr)
				}
				if ok, reason := queryResultsMatch(goCols, goRows, cgoCols, cgoRows, true); !ok {
					t.Fatalf("check %q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", q, reason, goCols, goRows, cgoCols, cgoRows)
				}
			}
			for _, q := range tc.checkSQL {
				runCheckSQL(q)
			}
			for _, s := range tc.checkExec {
				execErr, panicked, panicVal := tclSafeExecArgs(godb, s)
				if panicked {
					t.Fatalf("checkExec %q: engine PANICKED: %v", s, panicVal)
				}
				_, cerr := cgodb.Exec(s)
				if (execErr != nil) != (cerr != nil) {
					t.Fatalf("checkExec %q disagrees\n  go:  %v\n  cgo: %v", s, execErr, cerr)
				}
			}
			for _, q := range tc.checkSQLAfterExec {
				runCheckSQL(q)
			}
		})
	}
}
