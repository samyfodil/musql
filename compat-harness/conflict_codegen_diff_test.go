package compat

// This file gates conflict handling in UPDATE and INSERT statements against the oracle.

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// differExecOnce is differ() for a script containing a statement that FAILS
// PARTWAY, which the worker-based differ() cannot compare: its runOne() calls
// db.Query and, when that returns an error, RE-RUNS the same text through
// db.Exec. driver surfaces a write error at Query while mattn's lazily
// stepped rows surface it at rows.Err(), so a partially-applied
// "UPDATE OR FAIL" is executed ONCE by the oracle and TWICE by musql -- a
// harness artifact that looks exactly like a divergence. Here every statement
// runs exactly once: Exec for a write, Query for a read.
//
// nSetup is how many LEADING statements must succeed on BOTH engines. A script
// whose setup both engines rejected agrees vacuously (this file's first draft
// had three CHECK cases whose seed INSERT violated the CHECK, so the table was
// empty and the UPDATE under test had nothing to do -- they passed with the fix
// reverted), so the guard is not optional.
func differExecOnce(t *testing.T, name string, nSetup int, stmts []string) {
	t.Helper()
	engines := []struct{ label, driver string }{
		{"cgo", "sqlite3"},
		{"musql", driver.DriverName},
	}
	got := map[string]string{}
	for _, e := range engines {
		db, err := sql.Open(e.driver, filepath.Join(t.TempDir(), "x.db"))
		if err != nil {
			t.Fatalf("[%s] open %s: %v", name, e.label, err)
		}
		db.SetMaxOpenConns(1)
		var out []any
		for i, s := range stmts {
			var res any
			if isReadStmt(s) {
				res = queryOne(db, s)
			} else if _, err := db.Exec(s); err != nil {
				res = "ERR"
			} else {
				res = "OK"
			}
			if i < nSetup && res == "ERR" {
				db.Close()
				t.Fatalf("[%s] %s REJECTED setup statement #%d %q -- the comparison below would be vacuous",
					name, e.label, i, s)
			}
			out = append(out, res)
		}
		db.Close()
		b, _ := json.Marshal(out)
		got[e.label] = string(b)
	}
	if got["cgo"] != got["musql"] {
		t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:    %v\n  cgo:    %s\n  musql: %s",
			name, stmts, got["cgo"], got["musql"])
	}
}

// isReadStmt: which statements must go through Query rather than Exec. A
// RETURNING statement belongs here even though it writes -- Exec DISCARDS its
// result set, so comparing it through Exec compares nothing but "did it error".
// That was this file's own first bug: with OpInsert's OE_Ignore jump reverted
// (a conflict-skipped row wrongly emitting its RETURNING row) every INSERT ...
// RETURNING case below still passed.
func isReadStmt(s string) bool {
	u := strings.ToUpper(strings.TrimSpace(s))
	return strings.HasPrefix(u, "SELECT") || strings.Contains(u, " RETURNING ")
}

// normCell renders a scanned value the way the worker's normalize() does --
// tagged by storage class, TEXT and a UTF-8 blob collapsing together -- so a
// driver's choice of string vs []byte cannot masquerade as a divergence.
func normCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return "I:" + strconv.FormatInt(x, 10)
	case float64:
		return "F:" + strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "I:1"
		}
		return "I:0"
	case string:
		return "T:" + x
	case []byte:
		if utf8.Valid(x) {
			return "T:" + string(x)
		}
		return "X:" + hex.EncodeToString(x)
	default:
		return fmt.Sprintf("?:%v", x)
	}
}

func queryOne(db *sql.DB, s string) any {
	rows, err := db.Query(s)
	if err != nil {
		return "ERR"
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "ERR"
	}
	out := []any{cols}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "ERR"
		}
		norm := make([]string, len(cols))
		for i, c := range cells {
			norm[i] = normCell(c)
		}
		out = append(out, norm)
	}
	if rows.Err() != nil {
		return "ERR"
	}
	return out
}

// conflictCodegenCases -- every statement here is also listed in
// engine/vdbe_conflict_shapes_test.go's conflictShapesCompiled, which asserts
// it really compiles. The list is duplicated rather than shared because
// compat-harness is a separate nested module and cannot import an engine test
// file; keep the two in step when adding a case.
var conflictCodegenCases = []struct {
	name   string
	nSetup int
	stmts  []string
}{
	// ---- UPDATE, explicit OR-clause, every action ----
	{"upd-or-replace-cascade", 2, []string{
		`CREATE TABLE t(a,b UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR REPLACE t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-ignore-cascade", 2, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR IGNORE t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-fail-cascade", 2, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR FAIL t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-abort-cascade", 2, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR ABORT t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-rollback-in-txn", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`BEGIN`,
		`UPDATE OR ROLLBACK t SET b=b+10`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-rollback-autocommit", 2, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR ROLLBACK t SET b=b+10`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-abort-in-txn", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`BEGIN`,
		`UPDATE OR ABORT t SET b=b+10`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-fail-in-txn", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`BEGIN`,
		`UPDATE OR FAIL t SET b=b+10`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-replace-savepoint-rollback", 4, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`BEGIN`,
		`SAVEPOINT s1`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`,
		`ROLLBACK TO s1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	// The same shape WITHOUT "PRAGMA journal_mode=off", which this format declines
	// (see execJournalMode's segment branch: under off C SQLite KEEPS what a
	// ROLLBACK is meant to undo, and a segment commit has nothing to skip, so
	// accepting it would make every later ROLLBACK a wrong answer). What the case
	// is FOR is the OR IGNORE conflict count, which does not depend on the journal.
	{"upd-or-ignore-no-journal", 3, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE OR IGNORE t SET b=b+10`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- UPDATE, per-constraint declared ON CONFLICT default ----
	{"upd-declared-replace", 2, []string{
		`CREATE TABLE t(a,b UNIQUE ON CONFLICT REPLACE)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-fail", 2, []string{
		`CREATE TABLE t(a,b INTEGER UNIQUE ON CONFLICT FAIL)`,
		`INSERT INTO t VALUES(1,1),(2,2),(3,3),(4,13)`,
		`UPDATE t SET b=b+10`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-ignore", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-ignore-total-changes", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE t SET a=1 WHERE b='y'`,
		`SELECT total_changes()`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	// An explicit OR-clause overrides EVERY declared default uniformly, in both
	// directions -- effectiveHitAction (engine/conflict.go).
	{"upd-declared-ignore-overridden-by-replace", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-abort-overridden-by-ignore", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT ABORT,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR IGNORE t SET a=1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-replace-overridden-by-abort", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR ABORT t SET a=1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// A declared default on ONE constraint makes the whole statement
	// conflict-aware, but the UNDECLARED constraint keeps its own ABORT: this is
	// the case that would go unreported if the conflict probe covered fewer
	// indexes than the plain post-write re-validation it replaces.
	{"upd-declared-elsewhere-plain-unique-still-aborts", 2, []string{
		`CREATE TABLE t(a UNIQUE, b NOT NULL ON CONFLICT IGNORE)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE t SET a=1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// Two constraints, two DIFFERENT declared defaults on one table.
	{"upd-declared-per-constraint-mix", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE, b UNIQUE ON CONFLICT REPLACE)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t SET b=10 WHERE a=3`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},

	// ---- NOT NULL, per its own effective action ----
	{"upd-notnull-or-fail-mid-scan", 2, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r'),(4,'s')`,
		`UPDATE OR FAIL t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-notnull-or-ignore-mid-scan", 2, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
		`UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-notnull-declared-ignore", 2, []string{
		`CREATE TABLE t(a,b NOT NULL ON CONFLICT IGNORE)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
		`UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// NOT NULL ON CONFLICT REPLACE substitutes the column's own DEFAULT...
	{"upd-notnull-declared-replace-with-default", 2, []string{
		`CREATE TABLE t(a,b NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// The substituted DEFAULT is affinity-coerced like any other stored value,
	// so a TEXT default into an INT column lands as an INTEGER.
	{"upd-notnull-declared-replace-default-affinity", 2, []string{
		`CREATE TABLE t(a, b INT NOT NULL ON CONFLICT REPLACE DEFAULT '5')`,
		`INSERT INTO t VALUES(1,1),(2,2)`,
		`UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b END`,
		`SELECT a,b,typeof(b) FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// A generated column derived from the column the REPLACE substituted must
	// see the SUBSTITUTED value -- gencol1.test 7.20/7.21's rule, which the
	// INSERT compiler already carries; the UPDATE side reaches it through
	// OpComputeGenerated, emitted after the substitution.
	{"upd-notnull-declared-replace-default-generated", 2, []string{
		`CREATE TABLE t(a, c NOT NULL ON CONFLICT REPLACE DEFAULT 'ccc', g AS (c||'!'))`,
		`INSERT INTO t(a,c) VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET c = CASE a WHEN 2 THEN NULL ELSE c END`,
		`SELECT a,c,g FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// ...and, with no DEFAULT to substitute, demotes to ABORT.
	{"upd-notnull-or-replace-no-default", 2, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE OR REPLACE t SET b = NULL WHERE a=2`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},

	// ---- CHECK, per the statement's own OR-clause. The seed rows must SATISFY
	// the CHECK or the whole case is vacuous (see differExecOnce's nSetup).
	{"upd-check-or-ignore-mid-scan", 2, []string{
		`CREATE TABLE t(a,b, CHECK(b<10))`,
		`INSERT INTO t VALUES(1,1),(2,9),(3,3)`,
		`UPDATE OR IGNORE t SET b=b+1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// OE_Replace on a CHECK is demoted to OE_Abort -- insert.c:2095
	// "if( onError==OE_Replace ) onError = OE_Abort; /* IMP: R-26383-51744 */".
	{"upd-check-or-replace-demoted-to-abort", 2, []string{
		`CREATE TABLE t(a,b, CHECK(b<10))`,
		`INSERT INTO t VALUES(1,1),(2,9)`,
		`UPDATE OR REPLACE t SET b=b+1`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-check-or-fail-mid-scan", 2, []string{
		`CREATE TABLE t(a,b, CHECK(b<10))`,
		`INSERT INTO t VALUES(1,1),(2,9),(3,3)`,
		`UPDATE OR FAIL t SET b=b+1`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},

	// ---- index shapes the conflict probe must see ----
	{"upd-or-replace-two-unique-indexes", 4, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX i1 ON t(a)`,
		`CREATE UNIQUE INDEX i2 ON t(b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE OR REPLACE t SET a=2,b=20 WHERE a=3`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-two-victims-at-once", 4, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX i1 ON t(a)`,
		`CREATE UNIQUE INDEX i2 ON t(b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE OR REPLACE t SET a=1,b=20 WHERE a=3`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-ignore-partial-unique-index", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX px ON t(a) WHERE b>0`,
		`INSERT INTO t VALUES(1,1),(2,1),(3,-1)`,
		`UPDATE OR IGNORE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-expression-unique-index", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ex ON t(abs(a))`,
		`INSERT INTO t VALUES(1,'x'),(-2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=-1 WHERE b='z'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-desc-unique-index", 3, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ux ON t(a DESC)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
	}},
	{"upd-or-replace-multicolumn-unique", 2, []string{
		`CREATE TABLE t(a,b,c, UNIQUE(a,b))`,
		`INSERT INTO t VALUES(1,1,'p'),(1,2,'q'),(2,2,'r')`,
		`UPDATE OR REPLACE t SET a=1 WHERE c='r'`,
		`SELECT a,b,c,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-ignore-nocase-collation", 2, []string{
		`CREATE TABLE t(a TEXT COLLATE NOCASE UNIQUE ON CONFLICT IGNORE, b)`,
		`INSERT INTO t VALUES('A','x'),('b','y')`,
		`UPDATE t SET a='a'`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// A UNIQUE constraint on a GENERATED column: the probe must see the
	// RECOMPUTED value, not the stored NULL. Reading the stored NULL instead is
	// both a wrong answer (all three rows updated, every g collapsed to 2) and
	// a structurally corrupt index, which is what this case is pinned against.
	{"upd-declared-ignore-generated-unique", 2, []string{
		`CREATE TABLE t(a, g AS (a*2) UNIQUE ON CONFLICT IGNORE, b)`,
		`INSERT INTO t(a,b) VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE t SET a=1`,
		`SELECT a,g,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-ignore-null-is-never-a-conflict", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(NULL,'x'),(NULL,'y'),(1,'z')`,
		`UPDATE OR IGNORE t SET a=NULL`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-without-rowid", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2)`,
		`UPDATE OR REPLACE t SET k='a' WHERE k='b'`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},

	// ---- the conflict clause combined with the rest of the UPDATE grammar ----
	{"upd-or-ignore-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR IGNORE t SET a=1 RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y' RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-ignore-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE t SET a=1 RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-check-returning", 2, []string{
		`CREATE TABLE t(a,b, CHECK(b<10))`,
		`INSERT INTO t VALUES(1,1),(2,9),(3,3)`,
		`UPDATE OR IGNORE t SET b=b+1 RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-notnull-returning", 2, []string{
		`CREATE TABLE t(a,b NOT NULL)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-indexed-by", 3, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE INDEX ix ON t(b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR IGNORE t INDEXED BY ix SET a=1 WHERE b='y'`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-schema-qualified", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR REPLACE main.t SET a=1 WHERE b='y'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-ignore-where-subquery", 4, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`INSERT INTO s VALUES(2)`,
		`UPDATE OR IGNORE t SET a=1 WHERE b IN (SELECT 'y' FROM s)`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-or-replace-affinity-text", 2, []string{
		`CREATE TABLE t(a TEXT UNIQUE, b)`,
		`INSERT INTO t VALUES('1','x'),('2','y')`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`,
		`SELECT a,typeof(a),b,rowid FROM t ORDER BY rowid`,
	}},
	{"upd-or-ignore-affinity-integer", 2, []string{
		`CREATE TABLE t(a INTEGER UNIQUE, b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR IGNORE t SET a='1' WHERE b='y'`,
		`SELECT a,typeof(a),b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// A non-rowid parent key, deliberately: an UPDATE that moves the ROWID is
	// declined by this compiler (see engine/vdbe_conflict_shapes_test.go), so an
	// INTEGER PRIMARY KEY parent here would measure that decline instead of the
	// conflict codegen.
	{"upd-or-replace-fk-parent", 5, []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(k UNIQUE)`,
		`CREATE TABLE c(x REFERENCES p(k))`,
		`INSERT INTO p VALUES(1),(2)`,
		`INSERT INTO c VALUES(2)`,
		`UPDATE OR REPLACE p SET k=1 WHERE k=2`,
		`SELECT k FROM p ORDER BY rowid`,
		`SELECT x FROM c ORDER BY rowid`,
	}},
	// The updated row must never conflict with its OWN former self: the probe
	// runs with the row already removed from the store. Without that, an
	// "UPDATE OR REPLACE" that leaves the UNIQUE column alone would delete the
	// very row it is updating.
	{"upd-or-replace-row-never-conflicts-with-itself", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR REPLACE t SET b='q' WHERE a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"upd-declared-ignore-row-never-conflicts-with-itself", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE t SET b=b||'!'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// Two conflict-resolving statements in a row over the same cached plan.
	{"upd-or-replace-twice-same-plan", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='z'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT count(*) FROM t`,
	}},
	// The index must agree with the table after a REPLACE / an IGNORE, so read
	// back through it as well as through the table scan.
	{"upd-or-replace-index-agrees-with-table", 4, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ux ON t(a)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`,
		`SELECT a,b FROM t WHERE a=1`,
		`SELECT a,b FROM t WHERE a=2`,
		`SELECT a,b FROM t ORDER BY a`,
	}},
	{"upd-or-ignore-index-agrees-with-table", 4, []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ux ON t(a)`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR IGNORE t SET a=1`,
		`SELECT a,b FROM t WHERE a=1`,
		`SELECT a,b FROM t WHERE a=2`,
		`SELECT a,b FROM t ORDER BY a`,
	}},

	// ---- INSERT ... VALUES ... RETURNING carrying an OR-clause ----
	// Before this codegen every one of these was a flat ERROR -- the
	// OR-clause/RETURNING combination was declined outright -- and the
	// declared-default ones were worse, silently RETURNING a row the conflict
	// had skipped.
	{"ins-or-ignore-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1,'y') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-ignore-returning-multirow", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z'),(1,'w'),(3,'q') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-replace-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR REPLACE INTO t VALUES(1,'y'),(2,'z') RETURNING a,b,rowid`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-abort-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR ABORT INTO t VALUES(2,'z'),(1,'y') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-fail-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR FAIL INTO t VALUES(2,'z'),(1,'y'),(3,'q') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-returning-notnull", 1, []string{
		`CREATE TABLE t(a NOT NULL, b)`,
		`INSERT OR IGNORE INTO t VALUES(NULL,'y'),(2,'z') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-returning-check", 1, []string{
		`CREATE TABLE t(a CHECK(a>0), b)`,
		`INSERT OR IGNORE INTO t VALUES(-1,'y'),(2,'z') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-replace-returning-notnull-default", 1, []string{
		`CREATE TABLE t(a NOT NULL DEFAULT 7, b)`,
		`INSERT OR REPLACE INTO t VALUES(NULL,'y'),(2,'z') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-declared-ignore-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(1,'y') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-declared-replace-returning", 2, []string{
		`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE, b)`,
		`INSERT INTO t VALUES(1,'x'),(2,'w')`,
		`INSERT INTO t VALUES(1,'y'),(3,'z') RETURNING a,b`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-ignore-returning-star", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING *`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-returning-ipk", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-replace-returning-expression", 2, []string{
		`CREATE TABLE t(a UNIQUE,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR REPLACE INTO t VALUES(1,'y') RETURNING a+100 AS z, upper(b)`,
		`SELECT a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-returning-generated", 2, []string{
		`CREATE TABLE t(a UNIQUE, g AS (a*2), b)`,
		`INSERT INTO t(a,b) VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t(a,b) VALUES(1,'y'),(2,'z') RETURNING a,g,b`,
		`SELECT a,g,b FROM t ORDER BY rowid`,
	}},

	// ---- a non-integer rowid is ALWAYS a statement ABORT, whatever the
	// OR-clause. C SQLite emits the ONE-OPERAND
	// "sqlite3VdbeAddOp1(v, OP_MustBeInt, regRowid);" (insert.c:1534), so
	// OP_MustBeInt's body takes its "if( pOp->p2==0 ){ rc = SQLITE_MISMATCH;
	// goto abort_due_to_error; }" arm (vdbe.c:2111-2113) with no jump target
	// and no onError. Round 1 let emitInsertRowBody's ignoreRowidMismatch
	// soften it into a row SKIP under OR IGNORE, which SILENTLY STORED a row
	// the oracle refuses, and let it ride the OR-clause under OR FAIL, which
	// kept rows the oracle discards. Both directions are gated here.
	//
	// The aborting cases below deliberately do NOT read changes() when a
	// SUCCESSFUL write precedes them in the same script: driver discards
	// the throwaway autocommit session of a FAILED RETURNING statement
	// (queryReturningArgs -> finishAutocommit, driver/conn.go) without
	// c.noteConnState, so changes() keeps the previous statement's value where
	// C SQLite publishes 0 (vdbeaux.c:3481-3487). That is a pre-existing,
	// RETURNING-path-wide divergence -- it reproduces on a plain
	// "INSERT INTO t VALUES(1,'y') RETURNING a" with no OR-clause at all -- and
	// is untouched by this cluster, so it is disclosed rather than pinned here.
	{"ins-or-ignore-rowid-mismatch-returning", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES('abc','y'),(2,'z') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},
	{"ins-or-ignore-rowid-mismatch-real-returning", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1.5,'y'),(2,'z') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},
	{"ins-or-ignore-rowid-mismatch-blob-returning", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(x'0102','y'),(2,'z') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},
	{"ins-or-ignore-rowid-mismatch-single-returning", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES('abc','y') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},
	{"ins-or-ignore-rowid-mismatch-alias-returning", 2, []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t(rowid,a,b) VALUES('abc',1,'y') RETURNING a,b`,
		`SELECT rowid,a,b FROM t ORDER BY rowid`,
	}},
	{"ins-or-ignore-rowid-mismatch-noreturning", 2, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES('abc','y'),(2,'z')`,
		`SELECT k,b FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"ins-or-fail-rowid-mismatch-returning", 1, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT OR FAIL INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"ins-or-fail-rowid-mismatch-noreturning", 1, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT OR FAIL INTO t VALUES(1,'x'),('abc','y')`,
		`SELECT k,b FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	// The INSERT ... SELECT spelling of the same rule -- insert.c reaches the
	// SAME insert.c:1534 OP_MustBeInt on the pSelect path (the rowid register
	// is initialized at tag-20191021-001 and then flows into the identical
	// "if( !appendFlag )" block), and this compiler shares emitInsertRowBody
	// between the two sources.
	{"sel-or-ignore-rowid-mismatch", 3, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE s(k,b)`,
		`INSERT INTO s VALUES('abc','y'),(2,'z')`,
		`INSERT OR IGNORE INTO t SELECT k,b FROM s`,
		`SELECT k,b FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"sel-or-fail-rowid-mismatch", 3, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE s(k,b)`,
		`INSERT INTO s VALUES(1,'x'),('abc','y')`,
		`INSERT OR FAIL INTO t SELECT k,b FROM s`,
		`SELECT k,b FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	// ...and the control: a UNIQUE conflict on the SAME path still SKIPS under
	// OR IGNORE, so the fix above narrowed the rowid rule only.
	{"sel-or-ignore-unique-still-skips", 3, []string{
		`CREATE TABLE t(a UNIQUE, b)`,
		`CREATE TABLE s(a,b)`,
		`INSERT INTO s VALUES(1,'x'),(1,'y'),(2,'z')`,
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"ins-or-replace-rowid-mismatch-returning", 1, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT OR REPLACE INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},
	{"ins-or-rollback-rowid-mismatch-returning", 1, []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT OR ROLLBACK INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`,
		`SELECT k,b FROM t ORDER BY k`,
	}},

	// ---- STRICT x conflict-aware. main's OpTypeCheck promotion (f391a75) and
	// this one meet here: compileUpdateStmt now emits the type check AND takes
	// the conflict-resolving path for the same statement, an intersection
	// neither batch measured on its own.
	{"strict-upd-or-replace", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR REPLACE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-or-ignore", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR IGNORE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-or-ignore-typeviolation", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR IGNORE t SET a='abc'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-or-replace-typeviolation", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y')`,
		`UPDATE OR REPLACE t SET b=x'0102'`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-declared-onconflict", 2, []string{
		`CREATE TABLE t(a INT UNIQUE ON CONFLICT IGNORE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-or-fail", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x'),(2,'y'),(3,'z')`,
		`UPDATE OR FAIL t SET a=1`,
		`SELECT a,b,rowid FROM t ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-upd-or-ignore-generated-typeviolation", 2, []string{
		`CREATE TABLE g(a INT, b BLOB AS (a), c INT UNIQUE) STRICT`,
		`INSERT INTO g(a,c) VALUES(NULL,1),(NULL,2)`,
		`UPDATE OR IGNORE g SET a=1`,
		`SELECT a,c,rowid FROM g ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"strict-ins-or-ignore-returning", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY a`,
		`SELECT changes()`,
	}},
	{"strict-ins-or-ignore-returning-typeviolation", 2, []string{
		`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT OR IGNORE INTO t VALUES('nope','y') RETURNING a,b`,
		`SELECT a,b FROM t ORDER BY a`,
	}},

	// ---- WITHOUT ROWID x conflict-aware, PRIMARY KEY NOT reassigned. A SET
	// that DOES reassign it used to be declined and now compiles, on
	// update.c:868-869's PRIMARY-KEY-RECORD re-seek (reseekRowStoreByPK,
	// engine/vdbe_cursor.go); those spellings live in
	// conflict_identity_move_test.go, which reads the table back rather than
	// comparing statement outputs.
	{"wr-upd-or-replace-nonpk", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2),('c',3)`,
		`UPDATE OR REPLACE t SET v=1 WHERE k='c'`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"wr-upd-or-ignore-nonpk", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2),('c',3)`,
		`UPDATE OR IGNORE t SET v=1`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"wr-upd-or-replace-nonpk-cascade", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v INTEGER UNIQUE) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2),('c',3),('d',13)`,
		`UPDATE OR REPLACE t SET v=v+10`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"wr-upd-declared-onconflict-nonpk", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE ON CONFLICT IGNORE) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1),('b',2),('c',3)`,
		`UPDATE t SET v=1`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"wr-ins-or-ignore-returning", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1)`,
		`INSERT OR IGNORE INTO t VALUES('a',9),('b',2) RETURNING k,v`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
	{"wr-ins-or-replace-returning", 2, []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',1)`,
		`INSERT OR REPLACE INTO t VALUES('a',9),('b',2) RETURNING k,v`,
		`SELECT k,v FROM t ORDER BY k`,
		`SELECT changes()`,
	}},
}

func TestConflictCodegenMatchesC(t *testing.T) {
	for _, c := range conflictCodegenCases {
		differExecOnce(t, c.name, c.nSetup, c.stmts)
	}
}
