// Tests that changes()/total_changes() remain unchanged when a statement fails at prepare time.
package compat

import "testing"

// cpfRead reads both counters a write statement moves together (setChanges).
const cpfRead = `SELECT changes(), total_changes()`

// cpfSeed leaves changes()/total_changes() at a NON-ZERO baseline (3, 3)
// before the case under test -- a probe that reads 0 where the previous
// statement already left 0 would prove nothing.
var cpfSeed = []string{
	`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
	`INSERT INTO t VALUES(1,2),(2,3),(3,4)`,
}

// cpfBugCases are the six mined shapes: each fails at PREPARE time in real
// SQLite, so changes()/total_changes() must read back UNCHANGED from the
// cpfSeed baseline.
var cpfBugCases = []struct{ name, extraSeed, stmt string }{
	{"insert-no-such-table", ``, `INSERT INTO nosuch VALUES(1)`},
	{"update-no-such-table", ``, `UPDATE nosuch SET a=1`},
	{"delete-no-such-table", ``, `DELETE FROM nosuch`},
	{"update-no-such-column", ``, `UPDATE t SET nosuchcol=1`},
	{"insert-no-such-function", `CREATE TABLE src(a,b)`, `INSERT INTO src VALUES(bogusfn(1),2)`},
	{"insert-into-view-no-trigger", `CREATE VIEW vv AS SELECT * FROM t`, `INSERT INTO vv VALUES(1)`},
}

func TestChangesUnaffectedByPrepareFailure(t *testing.T) {
	for _, c := range cpfBugCases {
		t.Run(c.name, func(t *testing.T) {
			stmts := append([]string{}, cpfSeed...)
			if c.extraSeed != "" {
				stmts = append(stmts, c.extraSeed)
			}
			stmts = append(stmts, cpfRead, c.stmt, cpfRead,
				// ...and once more, to catch a fix that only skips the
				// publish on the FIRST post-failure read.
				cpfRead)
			cfaDiffer(t, c.name, stmts)
		})
	}
}

// TestChangesUnaffectedByMultiRowValuesFailure is the multi-row form of
// insert-no-such-function: the SECOND tuple's bad name must abort resolution
// of the WHOLE statement before the FIRST tuple's otherwise-valid row is ever
// stored, exactly as C SQLite's own compile-time function-name resolution
// would -- so this asserts both the counters AND the table's actual row
// count, not just the counters, to measure "nothing was touched" rather than
// argue it from the single-row case alone.
func TestChangesUnaffectedByMultiRowValuesFailure(t *testing.T) {
	cfaDiffer(t, "multi-row values, second tuple unresolvable", append(append([]string{}, cpfSeed...),
		cpfRead,
		`SELECT count(*) FROM t`,
		`INSERT INTO t VALUES(10,10),(bogusfn(2),2)`,
		cpfRead,
		`SELECT count(*) FROM t`,
	))
}

// TestChangesUnaffectedByTriggerBodyPrepareFailure answers the coordinator's
// question directly: an AFTER trigger whose OWN body statement fails a
// prepare-time check (here, its target table does not exist) leaves the
// FIRING statement's changes()/total_changes() alone too -- verified against
// 3.53.3, and already true of musql before this task's fix, for a reason
// distinct from writeCtx.prepareFailed:
//
// compileTriggerFirePlan (engine/vdbe_trigger.go), called from
// compileInsertStmt (engine/vdbe_write.go) while lowering the FIRING
// statement's own AFTER-trigger fire plan, calls checkTriggerBodyTables
// (engine/trigger.go) EAGERLY -- before compiling a single body opcode --
// which returns a PLAIN, UNWRAPPED error for a trigger body naming a
// nonexistent table ("engine: no such table: main.nosuch"), not one wrapped
// in errVDBEUnsupported. Either way the error propagates as a genuine compile
// failure now (totalWrite, vdbe_write.go, returns it rather than routing the
// statement anywhere else), so tryVDBEWrite returns it directly and
// runWrite -- the only place that ever calls setChanges for this statement
// kind -- is never reached at all. The SAME checkTriggerBodyTables call also
// backs the eager per-trigger validation validateTriggerExprsOnce
// (trigger.go) performs, so a shape reaching the check by that route fails
// exactly as early, before any row touches storage. That is why this needed
// no new writeCtx.prepareFailed site: nothing here reaches the deferred
// setChanges that flag guards.
//
// The outer statement's insert of its OWN row (4,5) is rolled back too
// (C SQLite: a trigger body failure aborts the firing statement), which
// this also asserts via the table's row count, not just the counters.
func TestChangesUnaffectedByTriggerBodyPrepareFailure(t *testing.T) {
	cfaDiffer(t, "AFTER trigger body targets a nonexistent table", append(append([]string{}, cpfSeed...),
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO nosuch VALUES(1); END`,
		cpfRead,
		`SELECT count(*) FROM t`,
		`INSERT INTO t VALUES(4,5)`,
		cpfRead,
		`SELECT count(*) FROM t`,
	))
}

// TestChangesUnaffectedByPrepareFailureReturning is the RETURNING form of the
// same three table-not-found cases -- execReturningViaVM (engine/
// vdbe_write.go) has its own identical deferred setChanges, guarded the same
// way. driver routes ANY Exec of a RETURNING statement through
// queryArgs/ExecReturningArgs (StmtExecContext, driver/stmt.go), so
// cfaRun's plain db.Exec already reaches it -- no Query needed.
func TestChangesUnaffectedByPrepareFailureReturning(t *testing.T) {
	for _, stmt := range []string{
		`INSERT INTO nosuch VALUES(1) RETURNING *`,
		`UPDATE nosuch SET a=1 RETURNING *`,
		`DELETE FROM nosuch RETURNING *`,
	} {
		t.Run(stmt, func(t *testing.T) {
			stmts := append(append([]string{}, cpfSeed...), cpfRead, stmt, cpfRead)
			cfaDiffer(t, stmt, stmts)
		})
	}
}

// TestChangesStillZeroedOnRunTimeFailure is the boundary that makes the fix
// NARROW: each of these statements DOES reach RUN state (its target and every
// name it references resolve) and then fails or matches nothing, so real
// SQLite -- and musql, before and after this fix -- publishes changes()==0,
// a real transition off the non-zero cpfSeed baseline. A fix that stops
// publishing on ANY error, rather than specifically a prepare-time one, would
// leave these reading the STALE baseline instead and fail here.
func TestChangesStillZeroedOnRunTimeFailure(t *testing.T) {
	cases := []struct {
		name      string
		extraSeed []string
		stmt      string
	}{
		{"unique-violation-first-row", nil, `INSERT INTO t VALUES(1,99)`},
		{"where-matched-nothing", nil, `UPDATE t SET a=a WHERE 0`},
		{"insert-select-where-0", nil, `INSERT INTO t SELECT a,b FROM t WHERE 0`},
		{"insert-select-fails-partway",
			[]string{`CREATE TABLE src(a INTEGER, b)`, `INSERT INTO src VALUES(10,10),(20,20),(2,99),(30,30)`},
			`INSERT INTO t SELECT a,b FROM src`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmts := append([]string{}, cpfSeed...)
			stmts = append(stmts, c.extraSeed...)
			stmts = append(stmts, cpfRead, c.stmt, cpfRead)
			cfaDiffer(t, c.name, stmts)
		})
	}
}
