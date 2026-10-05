// This file tests PRAGMA ignore_check_constraints behavior.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaR25ICCCase is one program run on ONE connection, and what the last
// statement plus one probe query must do.
type pragmaR25ICCCase struct {
	name  string
	stmts []string
	// wantErr is whether the LAST statement must fail.
	wantErr bool
	// probe is a query run afterwards whose cells -- every row, every column,
	// sorted, joined with "|" -- are compared against want. It is the SIDE
	// EFFECT, the only thing that tells "accepted" apart from "accepted and
	// then ignored".
	probe string
	want  string
}

var pragmaR25ICCCases = []pragmaR25ICCCase{
	// ---- the flag suppresses the CHECK and the row is STORED ----
	{"insert-stores-the-row", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
	}, false, `SELECT a FROM t1`, "5"},

	// ...and turning it back OFF restores enforcement, on the SAME table with
	// the SAME statement text. This is the write-plan-cache case: db.writePlans
	// is keyed on the text and invalidated only by schemaGen/txGen, so a fix
	// that does not drop it stores BOTH rows here -- and passes if the literal
	// is changed, which is how to tell the cache from the flag.
	{"off-restores-enforcement-same-text", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`INSERT INTO t1 VALUES(5)`,
	}, true, `SELECT count(*) FROM t1`, "1"},

	// The same script with a DIFFERENT literal, which a cache-blind fix gets
	// right -- kept beside it so the pair cannot both be "passing" for the
	// wrong reason.
	{"off-restores-enforcement-new-text", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`INSERT INTO t1 VALUES(6)`,
	}, true, `SELECT count(*) FROM t1`, "1"},

	{"update-is-suppressed-too", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`INSERT INTO t1 VALUES(20)`,
		`PRAGMA ignore_check_constraints=ON`,
		`UPDATE t1 SET a=1`,
	}, false, `SELECT a FROM t1`, "1"},

	// OR IGNORE does not SKIP the row: with no check there is nothing to skip.
	{"or-ignore-still-inserts", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT OR IGNORE INTO t1 VALUES(2)`,
	}, false, `SELECT count(*) FROM t1`, "1"},

	// An upsert's DO UPDATE is the third compiled site.
	{"upsert-do-update", []string{
		`CREATE TABLE t1(k INTEGER PRIMARY KEY, a CHECK(a>10))`,
		`INSERT INTO t1 VALUES(1,20)`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(1,99) ON CONFLICT(k) DO UPDATE SET a=3`,
	}, false, `SELECT a FROM t1`, "3"},

	// A trigger body's write is suppressed as well -- the flag is connection
	// state, not statement state.
	{"a-trigger-bodys-write-too", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`CREATE TABLE src(a)`,
		`CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO t1 VALUES(NEW.a); END`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO src VALUES(1)`,
	}, false, `SELECT a FROM t1`, "1"},

	// An FK cascade landing on a CHECK is suppressed (fk.go's call site --
	// guarding only the compiler leaves this one rejecting).
	{"an-fk-cascade-into-a-check", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(x INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(y INTEGER REFERENCES p ON DELETE SET NULL, CHECK(y IS NOT NULL))`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1)`,
		`PRAGMA ignore_check_constraints=ON`,
		`DELETE FROM p WHERE x=1`,
	}, false, `SELECT count(*) FROM c WHERE y IS NULL`, "1"},

	// ALTER TABLE ADD COLUMN is implemented in terms of pragma_quick_check, so
	// the flag reaches it transitively: a new column whose CHECK the back-filled
	// default violates is added anyway (alter_write.go's call site).
	{"alter-add-column-back-fill", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1)`,
		`PRAGMA ignore_check_constraints=ON`,
		`ALTER TABLE t1 ADD COLUMN z DEFAULT 1 CHECK(z>5)`,
	}, false, `SELECT count(*) FROM t1 WHERE z=1`, "1"},

	// ---- what the flag does NOT touch ----
	{"not-null-is-untouched", []string{
		`CREATE TABLE t1(a NOT NULL, c CHECK(c>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(NULL,1)`,
	}, true, `SELECT count(*) FROM t1`, "0"},

	{"strict-typecheck-is-untouched", []string{
		`CREATE TABLE t1(a INT CHECK(a>10)) STRICT`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES('x')`,
	}, true, `SELECT count(*) FROM t1`, "0"},

	{"unique-is-untouched", []string{
		`CREATE TABLE t1(a UNIQUE CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
		`INSERT INTO t1 VALUES(5)`,
	}, true, `SELECT count(*) FROM t1`, "1"},

	// The CHECK is still PARSED and still STORED verbatim: the flag changes
	// enforcement, never the schema.
	{"the-check-is-still-stored", []string{
		`PRAGMA ignore_check_constraints=ON`,
		`CREATE TABLE t1(a CHECK(a>10))`,
		`INSERT INTO t1 VALUES(1)`,
	}, false, `SELECT sql FROM sqlite_master WHERE name='t1'`, `CREATE TABLE t1(a CHECK(a>10))`},

	{"it-survives-a-commit", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`BEGIN`,
		`INSERT INTO t1 VALUES(1)`,
		`COMMIT`,
		`INSERT INTO t1 VALUES(2)`,
	}, false, `SELECT count(*) FROM t1`, "2"},

	// ---- integrity_check is the flag's SECOND enforcement site ----
	{"integrity-check-reports-each-row", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
		`INSERT INTO t1 VALUES(4)`,
		`INSERT INTO t1 VALUES(40)`,
		`PRAGMA ignore_check_constraints=OFF`,
	}, false, `PRAGMA integrity_check`,
		"CHECK constraint failed in t1|CHECK constraint failed in t1"},

	{"integrity-check-is-suppressed-while-on", []string{
		`CREATE TABLE t1(a CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5)`,
	}, false, `PRAGMA quick_check`, "ok"},

	{"integrity-check-names-the-table-not-the-constraint", []string{
		`CREATE TABLE t1(a, b, CONSTRAINT cc CHECK(a>10), CHECK(b>0))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t1 VALUES(5,-1)`,
		`PRAGMA ignore_check_constraints=OFF`,
	}, false, `PRAGMA integrity_check`, "CHECK constraint failed in t1"},

	// ---- the UPDATE rule: a CHECK is re-evaluated only when this statement
	// could have invalidated it. Independent of the pragma, but only reachable
	// through it, since nothing else puts a violating row in the table.
	{"update-of-an-unrelated-column-does-not-recheck", []string{
		`CREATE TABLE t(a,b,CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t VALUES(5,1)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`UPDATE t SET b=2`,
	}, false, `SELECT a, b FROM t`, "2|5"},

	// A WHERE-clause reference does not count as "assigned".
	{"a-where-reference-does-not-count", []string{
		`CREATE TABLE t(a,b,CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t VALUES(5,1)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`UPDATE t SET b=2 WHERE a<10`,
	}, false, `SELECT a, b FROM t`, "2|5"},

	// CHECK(0) names no column at all, so no UPDATE ever evaluates it.
	{"check-of-a-constant-is-never-re-evaluated", []string{
		`CREATE TABLE t(a,b,CHECK(0))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t VALUES(1,1)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`UPDATE t SET b=2`,
	}, false, `SELECT a, b FROM t`, "1|2"},

	// ...and the three that must STILL FAIL. "Skip the check on UPDATE" is as
	// wrong as no rule at all.
	{"assigning-the-column-re-evaluates-whatever-the-value", []string{
		`CREATE TABLE t(a,b,CHECK(a>10))`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t VALUES(5,1)`,
		`PRAGMA ignore_check_constraints=OFF`,
		`UPDATE t SET a=a`,
	}, true, `SELECT a, b FROM t`, "1|5"},

	{"moving-the-rowid-re-evaluates-an-ipk-check", []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, b, CHECK(id>100))`,
		`INSERT INTO t VALUES(200,1)`,
		`UPDATE t SET id=2`,
	}, true, `SELECT id FROM t`, "200"},

	{"a-generated-column-is-reached-transitively", []string{
		`CREATE TABLE t(a, b AS (a*2), CHECK(b<100))`,
		`INSERT INTO t(a) VALUES(1)`,
		`UPDATE t SET a=500`,
	}, true, `SELECT a FROM t`, "1"},

	{"a-stored-generated-column-too", []string{
		`CREATE TABLE t(a, b AS (a*2) STORED, CHECK(b<100))`,
		`INSERT INTO t(a) VALUES(1)`,
		`UPDATE t SET a=500`,
	}, true, `SELECT a FROM t`, "1"},

	// An INSERT with an AUTO-assigned rowid evaluates a CHECK over the IPK
	// COLUMN against the rowid it just assigned, not against the NULL the
	// column slot holds.
	{"an-auto-rowid-is-visible-to-an-ipk-check", []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, b, CHECK(id>100))`,
		`INSERT INTO t(b) VALUES(1)`,
	}, true, `SELECT count(*) FROM t`, "0"},

	// The value-parse boundary: a spelling this engine declines must be
	// declined by the SETTER, never treated as OFF (which is what C SQLite's
	// sqlite3GetBoolean does with it, and what this engine does not reproduce).
	{"the-getter-reports-the-flag", []string{
		`PRAGMA ignore_check_constraints=ON`,
	}, false, `PRAGMA ignore_check_constraints`, "1"},

	{"the-getter-reports-off-again", []string{
		`PRAGMA ignore_check_constraints=ON`,
		`PRAGMA temp.ignore_check_constraints=OFF`,
	}, false, `PRAGMA ignore_check_constraints`, "0"},
}

// iccProbeCells runs probe on a database/sql handle and renders every cell of
// every row, sorted, joined with "|".
func iccProbeCells(db *sql.DB, probe string) (string, error) {
	rows, err := db.Query(probe)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var cells []string
	for rows.Next() {
		raw := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range raw {
			ptr[i] = &raw[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			return "", err
		}
		for _, v := range raw {
			cells = append(cells, fmt.Sprintf("%v", derefICCValue(v)))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sort.Strings(cells)
	return strings.Join(cells, "|"), nil
}

func derefICCValue(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// runICCCase plays one case on one driver and returns the last statement's
// error plus the probe's cells.
func runICCCase(t *testing.T, driver, path string, tc pragmaR25ICCCase) (lastErr error, cells string, probeErr error) {
	t.Helper()
	db, err := sql.Open(driver, path)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // the flag is per-connection
	for i, s := range tc.stmts {
		_, eerr := db.Exec(s)
		if i == len(tc.stmts)-1 {
			lastErr = eerr
			break
		}
		if eerr != nil {
			t.Fatalf("%s: stmt #%d %q: %v", driver, i, s, eerr)
		}
	}
	cells, probeErr = iccProbeCells(db, tc.probe)
	return lastErr, cells, probeErr
}

// TestPragmaR25IgnoreCheckConstraintsOracleSpec is the measurement: it asserts
// the recorded `want` against the ORACLE, so this file's premise cannot go
// stale silently.
func TestPragmaR25IgnoreCheckConstraintsOracleSpec(t *testing.T) {
	for _, tc := range pragmaR25ICCCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lastErr, cells, perr := runICCCase(t, "sqlite3", filepath.Join(t.TempDir(), "o.db"), tc)
			if tc.wantErr != (lastErr != nil) {
				t.Fatalf("oracle: last stmt %q: err=%v, wantErr=%v", tc.stmts[len(tc.stmts)-1], lastErr, tc.wantErr)
			}
			if perr != nil {
				t.Fatalf("oracle: %s: %v", tc.probe, perr)
			}
			if cells != tc.want {
				t.Fatalf("oracle: %s = %q, want %q", tc.probe, cells, tc.want)
			}
		})
	}
}

// TestPragmaR25IgnoreCheckConstraintsServed is the same battery against musql
// through the driver, where the flag has to survive the one-session-per-
// statement model (Conn.ignoreCheckConstraints).
func TestPragmaR25IgnoreCheckConstraintsServed(t *testing.T) {
	for _, tc := range pragmaR25ICCCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lastErr, cells, perr := runICCCase(t, "sqlite", filepath.Join(t.TempDir(), "m.db"), tc)
			if tc.wantErr != (lastErr != nil) {
				t.Fatalf("musql: last stmt %q: err=%v, wantErr=%v", tc.stmts[len(tc.stmts)-1], lastErr, tc.wantErr)
			}
			if perr != nil {
				t.Fatalf("musql: %s: %v", tc.probe, perr)
			}
			if cells != tc.want {
				t.Fatalf("musql: %s = %q, want %q", tc.probe, cells, tc.want)
			}
		})
	}
}

// TestPragmaR25IgnoreCheckConstraintsEngineDirect is the battery a THIRD time,
// straight against engine.DB + SnapshotPager -- the path TestTCLCorpus itself
// replays on. The driver's own state machine is not in it, so a flag that only
// the driver carries would pass the test above and still leave the mined corpus
// unchanged (the corpus runs engine-direct).
func TestPragmaR25IgnoreCheckConstraintsEngineDirect(t *testing.T) {
	for _, tc := range pragmaR25ICCCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db, err := engine.Create(filepath.Join(t.TempDir(), "m.db"))
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Discard()
			var lastErr error
			for i, s := range tc.stmts {
				_, _, eerr := db.ExecArgs(s, nil)
				if i == len(tc.stmts)-1 {
					lastErr = eerr
					break
				}
				if eerr != nil {
					t.Fatalf("stmt #%d %q: %v", i, s, eerr)
				}
			}
			if tc.wantErr != (lastErr != nil) {
				t.Fatalf("engine-direct: last stmt %q: err=%v, wantErr=%v", tc.stmts[len(tc.stmts)-1], lastErr, tc.wantErr)
			}
			pager, perr := db.SnapshotPager()
			if perr != nil {
				t.Fatalf("SnapshotPager: %v", perr)
			}
			_, vals, qerr := pager.QueryArgs(tc.probe, nil)
			if qerr != nil {
				t.Fatalf("engine-direct: %s: %v", tc.probe, qerr)
			}
			var cells []string
			for _, r := range vals {
				for _, v := range r {
					cells = append(cells, engineValueText(v))
				}
			}
			sort.Strings(cells)
			if got := strings.Join(cells, "|"); got != tc.want {
				t.Fatalf("engine-direct: %s = %q, want %q", tc.probe, got, tc.want)
			}
		})
	}
}

// engineValueText renders one engine.Value the way database/sql's %v would.
func engineValueText(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "<nil>"
	case engine.Int:
		return fmt.Sprintf("%d", v.I)
	case engine.Float:
		return fmt.Sprintf("%v", v.F)
	default:
		return string(v.S)
	}
}
