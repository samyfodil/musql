// This file gates conflict clauses with triggers.
// Each scenario verifies trigger firing order and behavior against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

// tcScenario is one test script: DDL, conflict statements, and read-back queries.
type tcScenario struct {
	name    string
	ddl     []string
	actions []string
	reads   []string
}

// tcQueryRows runs a query and renders results with type tags.
func tcQueryRows(t *testing.T, label string, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: query %q: %v", label, q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s: columns %q: %v", label, q, err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s: scan %q: %v", label, q, err)
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = fmt.Sprintf("%T(%v)", c, c)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows %q: %v", label, q, err)
	}
	return out
}

// runTrigConflictScenario runs a scenario against both engines and compares results.
func runTrigConflictScenario(t *testing.T, pageSize int, sc tcScenario) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("trigconflict_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)

	for _, s := range sc.ddl {
		execPlainBoth(t, db, sdb, s)
	}
	for _, s := range sc.actions {
		execConflictBoth(t, db, sdb, s)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	// Export to SQLite format for C SQLite to read and verify.
	exported := filepath.Join(t.TempDir(), fmt.Sprintf("trigconflict_%d_export.db", pageSize))
	if xerr := sqliteconv.Export(path, exported, pageSize); xerr != nil {
		t.Fatalf("ExportSQLite(pageSize=%d): %v", pageSize, xerr)
	}
	fdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3, %s): %v", exported, err)
	}
	defer fdb.Close()
	var integrity string
	if err := fdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
	}
	for _, q := range sc.reads {
		got := tcQueryRows(t, "engine file", fdb, q)
		want := tcQueryRows(t, "C SQLite", sdb, q)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: %q diverges:\n  engine:      %v\n  C SQLite: %v", sc.name, q, got, want)
		}
	}
}

// logTriggers builds triggers that log each firing to a log table.
func logTriggers(events ...string) []string {
	out := []string{`CREATE TABLE log(ev TEXT, a, b)`}
	for _, e := range events {
		f := strings.Fields(e)
		timing, event := f[0], f[1]
		row := "new.a, new.b"
		if event == "DELETE" {
			row = "old.a, old.b"
		}
		out = append(out, fmt.Sprintf(
			`CREATE TRIGGER tr_%s_%s %s ON t BEGIN INSERT INTO log VALUES('%s', %s); END`,
			strings.ToLower(timing), strings.ToLower(event), e, e, row))
	}
	return out
}

// tcReads is the standard read-back queries for target table and trigger log.
var tcReads = []string{
	`SELECT rowid, a, b FROM t ORDER BY rowid`,
	`SELECT rowid, ev, a, b FROM log ORDER BY rowid`,
}

// TestConflictAgainstTriggeredTableMatchesCSQLite tests conflict clauses with triggers.
func TestConflictAgainstTriggeredTableMatchesCSQLite(t *testing.T) {
	uniqT := `CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`
	seed := `INSERT INTO t VALUES(1,'one'),(2,'two')`
	// The candidate list every INSERT scenario uses: one fresh row, one
	// conflicting with the seeded a=1, one more fresh row -- so the conflict
	// lands in the MIDDLE and FAIL's "keep the rows before it, never attempt
	// the ones after" is distinguishable from ABORT's "undo them all".
	rows := `VALUES(3,'three'),(1,'dup'),(4,'four')`

	scenarios := []tcScenario{
		{
			name:    "or-ignore/before+after",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), seed)...),
			actions: []string{`INSERT OR IGNORE INTO t ` + rows},
			reads:   tcReads,
		},
		{
			name:    "or-replace/before+after+delete-triggers",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT", "BEFORE DELETE", "AFTER DELETE"), seed)...),
			actions: []string{`INSERT OR REPLACE INTO t ` + rows},
			reads:   tcReads,
		},
		{
			name:    "or-fail/before+after",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), seed)...),
			actions: []string{`INSERT OR FAIL INTO t ` + rows},
			reads:   tcReads,
		},
		{
			name:    "or-abort/before+after",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), seed)...),
			actions: []string{`INSERT OR ABORT INTO t ` + rows},
			reads:   tcReads,
		},
		{
			name:    "or-rollback-no-txn/before+after",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), seed)...),
			actions: []string{`INSERT OR ROLLBACK INTO t ` + rows},
			reads:   tcReads,
		},
		{
			name: "replace-into-ipk",
			ddl: append([]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`},
				append(logTriggers("BEFORE INSERT", "AFTER INSERT", "AFTER DELETE"), seed)...),
			actions: []string{`REPLACE INTO t VALUES(1,'newone')`},
			reads:   tcReads,
		},
		{
			name:    "replace-own-earlier-row",
			ddl:     append([]string{uniqT}, logTriggers("BEFORE INSERT", "AFTER INSERT")...),
			actions: []string{`INSERT OR REPLACE INTO t VALUES(1,'first'),(1,'second'),(2,'other')`},
			reads:   tcReads,
		},
		{
			name: "or-ignore/when-gated",
			ddl: []string{uniqT, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t WHEN new.a>2 BEGIN INSERT INTO log VALUES('B', new.a, new.b); END`,
				`CREATE TRIGGER ta AFTER INSERT ON t WHEN new.a>2 BEGIN INSERT INTO log VALUES('A', new.a, new.b); END`,
				`INSERT INTO t VALUES(1,'one'),(3,'three')`},
			actions: []string{`INSERT OR IGNORE INTO t VALUES(1,'dup'),(3,'dup3'),(4,'four')`},
			reads:   tcReads,
		},
		{
			name: "before-trigger-creates-the-conflict",
			ddl: []string{uniqT, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t WHEN new.b='seed' BEGIN INSERT INTO t VALUES(new.a,'planted'); END`},
			actions: []string{`INSERT OR IGNORE INTO t VALUES(7,'seed')`},
			reads:   tcReads,
		},
		{
			name: "or-replace/self-cascade",
			ddl: []string{uniqT, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tr AFTER INSERT ON t WHEN new.a<10 BEGIN INSERT INTO t VALUES(new.a+10,'cascade'); END`,
				`INSERT INTO t VALUES(1,'one')`},
			actions: []string{`INSERT OR REPLACE INTO t VALUES(1,'dup'),(2,'two')`},
			reads:   tcReads,
		},

		{
			name: "declared-unique-on-conflict-ignore",
			ddl: append([]string{`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT IGNORE, b TEXT)`},
				append(logTriggers("BEFORE INSERT", "AFTER INSERT"), `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t VALUES(1,'dup'),(2,'two')`},
			reads:   tcReads,
		},
		{
			name: "declared-unique-on-conflict-replace",
			ddl: append([]string{`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT REPLACE, b TEXT)`},
				append(logTriggers("BEFORE INSERT", "AFTER INSERT", "AFTER DELETE"), `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t VALUES(1,'dup'),(2,'two')`},
			reads:   tcReads,
		},

		{
			name:    "upsert-do-nothing",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO NOTHING`, `INSERT INTO t VALUES(5,'five') ON CONFLICT(a) DO NOTHING`},
			reads:   tcReads,
		},
		{
			name:    "upsert-do-update-no-update-trigger",
			ddl:     append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"), `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
			reads:   tcReads,
		},

		{
			name: "insert-select-plain",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"),
				`CREATE TABLE src(a,b)`, `INSERT INTO src VALUES(3,'three'),(4,'four')`, `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t SELECT a,b FROM src`},
			reads:   append(tcReads, `SELECT rowid, a, b FROM src ORDER BY rowid`),
		},
		{
			name: "insert-select-conflict-aborts",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"),
				`CREATE TABLE src(a,b)`, `INSERT INTO src VALUES(3,'three'),(1,'dup')`, `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT INTO t SELECT a,b FROM src`},
			reads:   append(tcReads, `SELECT rowid, a, b FROM src ORDER BY rowid`),
		},
		{
			name: "insert-select-or-ignore",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"),
				`CREATE TABLE src(a,b)`, `INSERT INTO src VALUES(1,'dup'),(3,'three')`, `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT OR IGNORE INTO t SELECT a,b FROM src`},
			reads:   append(tcReads, `SELECT rowid, a, b FROM src ORDER BY rowid`),
		},
		{
			name: "insert-select-or-replace",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE INSERT", "AFTER INSERT"),
				`CREATE TABLE src(a,b)`, `INSERT INTO src VALUES(1,'dup'),(3,'three')`, `INSERT INTO t VALUES(1,'one')`)...),
			actions: []string{`INSERT OR REPLACE INTO t SELECT a,b FROM src`},
			reads:   append(tcReads, `SELECT rowid, a, b FROM src ORDER BY rowid`),
		},
		{
			name: "insert-select-self",
			ddl: []string{`CREATE TABLE t(a INTEGER, b TEXT)`, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A', new.a, new.b); END`,
				`INSERT INTO t VALUES(1,'one'),(2,'two')`},
			actions: []string{`INSERT INTO t SELECT a+10,b FROM t`},
			reads:   tcReads,
		},

		{
			name: "update-or-ignore/before+after",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE UPDATE", "AFTER UPDATE"),
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`)...),
			actions: []string{`UPDATE OR IGNORE t SET a=a+1 WHERE a>=2`},
			reads:   tcReads,
		},
		{
			name: "update-or-replace/before+after+delete-triggers",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE UPDATE", "AFTER UPDATE", "BEFORE DELETE", "AFTER DELETE"),
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`)...),
			actions: []string{`UPDATE OR REPLACE t SET a=a+1 WHERE a>=2`},
			reads:   tcReads,
		},
		{
			name: "update-or-fail/before+after",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE UPDATE", "AFTER UPDATE"),
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`)...),
			actions: []string{`UPDATE OR FAIL t SET a=a+1 WHERE a>=2`},
			reads:   tcReads,
		},
		{
			name: "update-or-abort/before+after",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE UPDATE", "AFTER UPDATE"),
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`)...),
			actions: []string{`UPDATE OR ABORT t SET a=a+1 WHERE a>=2`},
			reads:   tcReads,
		},
		{
			name: "update-or-replace/victim-is-a-bystander",
			ddl: append([]string{uniqT}, append(logTriggers("BEFORE UPDATE", "AFTER UPDATE", "AFTER DELETE"),
				`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three'),(13,'thirteen')`)...),
			actions: []string{`UPDATE OR REPLACE t SET a=a+10 WHERE a<=3`},
			reads:   tcReads,
		},
		{
			name: "update-or-ignore/of-col-list",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT, c TEXT)`, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE UPDATE OF b ON t BEGIN INSERT INTO log VALUES('OFB', old.a, new.a); END`,
				`INSERT INTO t VALUES(1,'one','x'),(2,'two','y')`},
			actions: []string{`UPDATE OR IGNORE t SET c='z'`, `UPDATE OR IGNORE t SET b='q'`},
			reads:   []string{`SELECT rowid, a, b, c FROM t ORDER BY rowid`, `SELECT rowid, ev, a, b FROM log ORDER BY rowid`},
		},
		{
			name: "update-declared-on-conflict-ignore",
			ddl: append([]string{`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT IGNORE, b TEXT)`},
				append(logTriggers("BEFORE UPDATE", "AFTER UPDATE"),
					`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`)...),
			actions: []string{`UPDATE t SET a=a+1 WHERE a>=2`},
			reads:   tcReads,
		},

		{
			name: "or-ignore/generated-column",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT, c AS (a*2))`, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B', new.a, new.c); END`,
				`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A', new.a, new.c); END`,
				`INSERT INTO t(a,b) VALUES(1,'one')`},
			actions: []string{`INSERT OR IGNORE INTO t(a,b) VALUES(1,'dup'),(2,'two')`},
			reads:   []string{`SELECT rowid, a, b, c FROM t ORDER BY rowid`, `SELECT rowid, ev, a, b FROM log ORDER BY rowid`},
		},
		{
			name: "or-replace/applied-defaults",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT DEFAULT 'dflt', c INTEGER DEFAULT 7)`, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B', new.b, new.c); END`,
				`INSERT INTO t(a) VALUES(1)`},
			actions: []string{`INSERT OR REPLACE INTO t(a) VALUES(1),(2)`},
			reads:   []string{`SELECT rowid, a, b, c FROM t ORDER BY rowid`, `SELECT rowid, ev, a, b FROM log ORDER BY rowid`},
		},
		{
			name: "or-replace/without-rowid",
			ddl: []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) WITHOUT ROWID`, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B', new.a, new.b); END`,
				`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A', new.a, new.b); END`,
				`INSERT INTO t VALUES(1,'one')`},
			actions: []string{`INSERT OR REPLACE INTO t VALUES(1,'dup'),(2,'two')`, `INSERT OR IGNORE INTO t VALUES(1,'nope'),(3,'three')`},
			reads:   []string{`SELECT a, b FROM t ORDER BY a`, `SELECT rowid, ev, a, b FROM log ORDER BY rowid`},
		},
		{
			name: "or-replace/autoincrement",
			ddl: append([]string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT)`},
				append(logTriggers("AFTER INSERT", "AFTER DELETE"), `INSERT INTO t VALUES(1,'one'),(2,'two')`)...),
			actions: []string{`INSERT OR REPLACE INTO t VALUES(1,'newone')`, `INSERT INTO t VALUES(NULL,'next')`},
			reads:   append(tcReads, `SELECT name, seq FROM sqlite_sequence ORDER BY name`),
		},
		{
			name: "or-replace/raise-ignore-in-before",
			ddl: []string{uniqT, `CREATE TABLE log(ev TEXT, a, b)`,
				`CREATE TRIGGER tskip BEFORE INSERT ON t WHEN new.a=5 BEGIN SELECT RAISE(IGNORE); END`,
				`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B', new.a, new.b); END`,
				`INSERT INTO t VALUES(1,'one')`},
			actions: []string{`INSERT OR REPLACE INTO t VALUES(5,'skipped'),(1,'replaced')`},
			reads:   tcReads,
		},
	}

	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			for _, mode := range engineModes {
				t.Run("vdbemode-"+mode, func(t *testing.T) {
					for _, sc := range scenarios {
						t.Run(sc.name, func(t *testing.T) { runTrigConflictScenario(t, pageSize, sc) })
					}
				})
			}
		})
	}
}

// TestTriggerProgramInheritsOuterConflictPolicy tests that an outer conflict clause
// is inherited by statements in the trigger program.
func TestTriggerProgramInheritsOuterConflictPolicy(t *testing.T) {
	bodyConflicts := []string{
		`CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`,
		`CREATE TABLE other(x INTEGER UNIQUE, y TEXT)`,
		`INSERT INTO other VALUES(1,'existing')`,
	}
	otherReads := []string{`SELECT rowid, a, b FROM t ORDER BY rowid`, `SELECT rowid, x, y FROM other ORDER BY rowid`}

	var scenarios []tcScenario
	for _, timing := range []string{"BEFORE", "AFTER"} {
		for _, clause := range []string{"", "OR IGNORE", "OR REPLACE", "OR ABORT"} {
			scenarios = append(scenarios, tcScenario{
				name: fmt.Sprintf("body-insert/%s/%s", timing, strings.ReplaceAll(clause, " ", "_")),
				ddl: append(append([]string(nil), bodyConflicts...),
					fmt.Sprintf(`CREATE TRIGGER tr %s INSERT ON t BEGIN INSERT INTO other VALUES(1,'from-trigger'); END`, timing)),
				actions: []string{fmt.Sprintf(`INSERT %s INTO t VALUES(9,'nine')`, clause)},
				reads:   otherReads,
			})
		}
	}
	scenarios = append(scenarios,
		tcScenario{
			name: "body-clause-overridden-by-outer",
			ddl: append(append([]string(nil), bodyConflicts...),
				`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR IGNORE INTO other VALUES(1,'from-trigger'); END`),
			actions: []string{`INSERT OR REPLACE INTO t VALUES(9,'nine')`},
			reads:   otherReads,
		},
		tcScenario{
			name: "body-clause-used-when-no-outer",
			ddl: append(append([]string(nil), bodyConflicts...),
				`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR IGNORE INTO other VALUES(1,'from-trigger'); END`),
			actions: []string{`INSERT INTO t VALUES(9,'nine')`},
			reads:   otherReads,
		},
		tcScenario{
			name: "body-update",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`, `CREATE TABLE other(x INTEGER UNIQUE, y TEXT)`,
				`INSERT INTO other VALUES(1,'one'),(2,'two')`,
				`CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE other SET x=1 WHERE x=2; END`},
			actions: []string{`INSERT OR REPLACE INTO t VALUES(9,'nine')`},
			reads:   otherReads,
		},
		tcScenario{
			name: "two-levels-deep",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`, `CREATE TABLE mid(m INTEGER UNIQUE)`,
				`CREATE TABLE other(x INTEGER UNIQUE, y TEXT)`, `INSERT INTO other VALUES(1,'existing')`,
				`CREATE TRIGGER t1 AFTER INSERT ON t BEGIN INSERT INTO mid VALUES(new.a); END`,
				`CREATE TRIGGER t2 AFTER INSERT ON mid BEGIN INSERT INTO other VALUES(1,'deep'); END`},
			actions: []string{`INSERT OR REPLACE INTO t VALUES(9,'nine')`},
			reads:   append(otherReads, `SELECT rowid, m FROM mid ORDER BY rowid`),
		},
		tcScenario{
			name: "delete-step-resets-the-policy",
			ddl: []string{`CREATE TABLE t(a INTEGER UNIQUE, b TEXT)`, `CREATE TABLE u(b2 INTEGER UNIQUE)`,
				`CREATE TABLE other(x INTEGER UNIQUE, y TEXT)`,
				`INSERT INTO other VALUES(1,'existing')`, `INSERT INTO u VALUES(5)`,
				`CREATE TRIGGER td AFTER DELETE ON u BEGIN INSERT INTO other VALUES(1,'from-delete-trigger'); END`,
				`CREATE TRIGGER ti AFTER INSERT ON t BEGIN DELETE FROM u; END`},
			actions: []string{`INSERT OR IGNORE INTO t VALUES(9,'nine')`},
			reads:   append(otherReads, `SELECT rowid, b2 FROM u ORDER BY rowid`),
		},
	)

	for _, mode := range engineModes {
		t.Run("vdbemode-"+mode, func(t *testing.T) {
			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) { runTrigConflictScenario(t, 4096, sc) })
			}
		})
	}
}

// TestUpsertDoUpdateWithUpdateTriggersRunsThem tests that DO UPDATE fires UPDATE triggers.
func TestUpsertDoUpdateWithUpdateTriggersRunsThem(t *testing.T) {
	for _, mode := range engineModes {
		t.Run("vdbemode-"+mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upsert_update_trigger.sqlite")
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()
			for _, s := range []string{
				`CREATE TABLE t(a INTEGER UNIQUE, b TEXT, c TEXT)`,
				`CREATE TABLE log(ev TEXT)`,
				`CREATE TRIGGER tu AFTER UPDATE OF b ON t BEGIN INSERT INTO log VALUES('U'); END`,
				`INSERT INTO t VALUES(1,'one','x')`,
			} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			for i, s := range []string{
				`INSERT INTO t VALUES(1,'dup','y') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
				`INSERT INTO t VALUES(1,'dup','y') ON CONFLICT(a) DO UPDATE SET b='q' WHERE t.c='x'`,
			} {
				if _, _, err := db.ExecArgs(s, nil); err != nil {
					t.Errorf("%q: expected success, got %v", s, err)
					continue
				}
				p, perr := db.SnapshotPager()
				if perr != nil {
					t.Fatalf("SnapshotPager: %v", perr)
				}
				_, rows, qerr := p.QueryArgs(`SELECT count(*) FROM log`, nil)
				p.Close()
				if qerr != nil {
					t.Fatalf("count log: %v", qerr)
				}
				if got, want := rows[0][0].I, int64(i+1); got != want {
					t.Errorf("%q: log has %d rows, want %d -- the row changed but the UPDATE trigger did not fire", s, got, want)
				}
			}
			accepted := []string{
				`INSERT INTO t VALUES(1,'dup','y') ON CONFLICT(a) DO NOTHING`,
				`INSERT INTO t VALUES(1,'dup','y') ON CONFLICT(a) DO UPDATE SET c=excluded.c`, // "UPDATE OF b" does not cover c
			}
			for _, s := range accepted {
				if _, _, err := db.ExecArgs(s, nil); err != nil {
					t.Errorf("%q: expected success, got %v", s, err)
				}
			}
		})
	}
}
