// Package compat gates INSTEAD OF view write paths against C SQLite.
package compat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// viewF3Schema is the base table, log, view, and INSTEAD OF triggers.
var viewF3Schema = []string{
	`CREATE TABLE base(k INTEGER PRIMARY KEY, a TEXT, b TEXT)`,
	`CREATE TABLE log(seq INTEGER PRIMARY KEY, what TEXT)`,
	`CREATE VIEW v AS SELECT k, a, b FROM base`,
	`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN
		INSERT INTO base(k,a,b) VALUES(new.k, new.a, new.b);
		INSERT INTO log(what) VALUES('ins '||quote(new.k)||' '||quote(new.a)||' '||quote(new.b));
	END`,
	`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN
		UPDATE base SET k=new.k, a=new.a, b=new.b WHERE k=old.k;
		INSERT INTO log(what) VALUES('upd '||quote(old.k)||' '||quote(old.a)||'->'||quote(new.a)||' '||quote(old.b)||'->'||quote(new.b));
	END`,
	`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN
		DELETE FROM base WHERE k=old.k;
		INSERT INTO log(what) VALUES('del '||quote(old.k)||' '||quote(old.a));
	END`,
}

// viewF3Dump queries the final state: base rows, fire log, and integrity_check.
var viewF3Dump = []string{
	`SELECT k, quote(a), quote(b) FROM base ORDER BY k`,
	`SELECT seq, what FROM log ORDER BY seq`,
	`PRAGMA integrity_check`,
}

func viewF3Case(stmts ...string) []string {
	out := append([]string{}, viewF3Schema...)
	out = append(out, stmts...)
	return append(out, viewF3Dump...)
}

// viewF3Seed fills the view with three rows through the INSTEAD OF INSERT
// trigger, then clears the log so a case's own fires stand alone.
var viewF3Seed = []string{
	`INSERT INTO v VALUES(1,'a1','b1')`,
	`INSERT INTO v VALUES(2,'a2','b2')`,
	`INSERT INTO v VALUES(3,'a3',NULL)`,
	`DELETE FROM log`,
}

func viewF3SeededCase(stmts ...string) []string {
	return viewF3Case(append(append([]string{}, viewF3Seed...), stmts...)...)
}

// TestViewF3InsertTakesValues pins the INSERT boundary: every spelling of the
// row source, mapped onto the view's column layout, with changes() read back
// because a view DML publishes 0 rather than what its trigger body wrote.
func TestViewF3InsertTakesValues(t *testing.T) {
	differ(t, "F3 insert positional/named/select", viewF3Case(
		`INSERT INTO v VALUES(1,'a1','b1')`,
		`SELECT changes()`,
		// A named list, out of view-column order.
		`INSERT INTO v(b,k,a) VALUES('b2',2,'a2')`,
		// A named list that omits a column: the omitted one is NULL, not a
		// default -- a view has none.
		`INSERT INTO v(k,a) VALUES(3,'a3')`,
		// An INSERT ... SELECT source, whose rows are already values.
		`INSERT INTO v SELECT 4,'a4','b4'`,
		`INSERT INTO v(k,a,b) SELECT k+10, upper(a), b FROM base WHERE k<=2`,
		// Multi-row VALUES: one fire per tuple, in tuple order.
		`INSERT INTO v VALUES(20,'a20','b20'),(21,'a21','b21')`,
		// Expressions in the tuple: evaluated at the boundary, so the trigger
		// body sees values.
		`INSERT INTO v VALUES(30, 'a'||'30', upper('b30'))`,
	))
}

// TestViewF3InsertValuesEvaluatedOnce pins that the tuple is evaluated ONCE,
// before any fire -- the property "at the boundary" actually means. Real
// SQLite codes it into registers ahead of sqlite3CodeRowTrigger
// (insert.c:1431, then :1495), so last_insert_rowid() in the tuple reads the
// value from BEFORE the body ran, and reads the same value for both rows of a
// two-tuple INSERT even though the first tuple's body moved it.
func TestViewF3InsertValuesEvaluatedOnce(t *testing.T) {
	differ(t, "F3 insert tuple evaluated once", viewF3Case(
		`INSERT INTO base VALUES(7,'seed','seed')`,
		`SELECT last_insert_rowid()`,
		`INSERT INTO v VALUES(last_insert_rowid()+1, 'from lirid', NULL)`,
		`SELECT last_insert_rowid()`,
		// Two tuples: BOTH read the pre-statement rowid, because both were
		// coded before the first body ran.
		`INSERT INTO v VALUES(last_insert_rowid()+100,'t1',NULL),(last_insert_rowid()+200,'t2',NULL)`,
		`SELECT last_insert_rowid(), changes()`,
	))
}

// TestViewF3UpdateWhereAndSetList pins the UPDATE's two scan expressions: what
// the WHERE selects (the log shows exactly which rows fired) and what the SET
// list computes (each right-hand side against the OLD row, so assignments are
// simultaneous).
func TestViewF3UpdateWhereAndSetList(t *testing.T) {
	differ(t, "F3 update where + set list", viewF3SeededCase(
		// Simultaneous assignment: both right-hand sides read the OLD row, so
		// this swaps rather than chains.
		`UPDATE v SET a=b, b=a WHERE k=1`,
		// A right-hand side over a column the SET list does not name.
		`UPDATE v SET b=a||'/'||quote(b) WHERE k=2`,
		// Multi-row, and a target given after a dependent right-hand side.
		`UPDATE v SET b=upper(a), a=lower(a) WHERE k>=2`,
		// No WHERE at all: every view row fires.
		`UPDATE v SET a=a||'.'`,
		// A rowid-moving assignment through the view's own key column.
		`UPDATE v SET k=k+100 WHERE k=1`,
	))
}

// TestViewF3UpdateWhereTruthiness is the mutation-sensitive half of the WHERE
// seam: a WHERE that is NULL, or 0, or a string that converts to 0, selects
// NOTHING, and the log proves the trigger body did not run for those rows. A
// seam that answered "not an error" instead of isTruthy fires for all of them.
func TestViewF3UpdateWhereTruthiness(t *testing.T) {
	differ(t, "F3 update where truthiness", viewF3SeededCase(
		`UPDATE v SET a='never' WHERE NULL`,
		`UPDATE v SET a='never' WHERE 0`,
		`UPDATE v SET a='never' WHERE 'abc'`,
		`UPDATE v SET a='never' WHERE 0.0`,
		`UPDATE v SET a='never' WHERE b IS NOT NULL AND NULL`,
		// The other direction, so the case cannot pass by never firing: a
		// non-zero STRING is true, and fires for every row.
		`UPDATE v SET a=a||'!' WHERE '2'`,
		`UPDATE v SET a=a||'?' WHERE -1`,
	))
}

// TestViewF3DeleteWhere pins the DELETE's WHERE -- the same seam, reached from
// filterViewRows, which collects every matching view row BEFORE any body runs.
func TestViewF3DeleteWhere(t *testing.T) {
	differ(t, "F3 delete where", viewF3SeededCase(
		// Selects nothing: no fire, no row gone.
		`DELETE FROM v WHERE NULL`,
		`DELETE FROM v WHERE 0`,
		`DELETE FROM v WHERE 'abc'`,
		`SELECT count(*) FROM base`,
		// One row, then a multi-row predicate over a NULL column.
		`DELETE FROM v WHERE k=2`,
		`DELETE FROM v WHERE b IS NULL`,
		`SELECT k, quote(a), quote(b) FROM base ORDER BY k`,
		// No WHERE at all: every view row fires.
		`INSERT INTO v VALUES(8,'a8','b8')`,
		`DELETE FROM v`,
	))
}

// TestViewF3EvaluationErrorsAbort pins that an expression the boundary or the
// scan cannot evaluate aborts the whole statement and writes NOTHING -- the
// half a refactor that swallowed an error would break silently. "integer
// overflow" is the probe because 1/0 is NULL in SQLite, not an error.
func TestViewF3EvaluationErrorsAbort(t *testing.T) {
	differ(t, "F3 evaluation errors abort", viewF3SeededCase(
		// In the INSERT's tuple: no row, no fire.
		`INSERT INTO v VALUES(9, abs(-9223372036854775807-1), 'x')`,
		// In a SET right-hand side, alone and beside a good one: the whole
		// statement aborts, so the good assignment is not written either.
		`UPDATE v SET a=abs(-9223372036854775807-1) WHERE k=1`,
		`UPDATE v SET a='ok', b=abs(-9223372036854775807-1) WHERE k=1`,
		`UPDATE v SET b=abs(-9223372036854775807-1), a='ok' WHERE k=1`,
		// In the WHERE itself, for both verbs.
		`UPDATE v SET a='ok' WHERE abs(-9223372036854775807-1)=1`,
		`DELETE FROM v WHERE abs(-9223372036854775807-1)=1`,
		// And the statement after them all still works, so the abort left no
		// half-open state behind.
		`UPDATE v SET a='after' WHERE k=1`,
	))
}

// ---------------------------------------------------------------------------
// Affinity: the half an affinity-neutral gate cannot see.
// ---------------------------------------------------------------------------

// viewF3AffSchema gives the view one column of every affinity and a sink that
// records the TYPE and the VALUE of each NEW.* the trigger saw. Neither body
// writes abase, so each case's NEW row is independent of the ones before it
// and the OLD row never moves.
var viewF3AffSchema = []string{
	`CREATE TABLE abase(k INTEGER PRIMARY KEY, t TEXT, i INTEGER, r REAL, n NUMERIC, b BLOB)`,
	`CREATE TABLE asink(seq INTEGER PRIMARY KEY, d TEXT)`,
	`CREATE VIEW av AS SELECT k,t,i,r,n,b FROM abase`,
	`CREATE TRIGGER avu INSTEAD OF UPDATE ON av BEGIN
		INSERT INTO asink(d) VALUES(
			'UPD t='||typeof(new.t)||':'||quote(new.t)||
			' i='||typeof(new.i)||':'||quote(new.i)||
			' r='||typeof(new.r)||':'||quote(new.r)||
			' n='||typeof(new.n)||':'||quote(new.n)||
			' b='||typeof(new.b)||':'||quote(new.b));
	END`,
	`CREATE TRIGGER avi INSTEAD OF INSERT ON av BEGIN
		INSERT INTO asink(d) VALUES(
			'INS t='||typeof(new.t)||':'||quote(new.t)||
			' i='||typeof(new.i)||':'||quote(new.i)||
			' r='||typeof(new.r)||':'||quote(new.r)||
			' n='||typeof(new.n)||':'||quote(new.n)||
			' b='||typeof(new.b)||':'||quote(new.b));
	END`,
	`INSERT INTO abase VALUES(1,'t0',0,0.0,0,x'00')`,
}

func viewF3AffCase(stmts ...string) []string {
	out := append([]string{}, viewF3AffSchema...)
	out = append(out, stmts...)
	return append(out, `SELECT seq, d FROM asink ORDER BY seq`,
		`SELECT k, typeof(t), quote(t), typeof(i), quote(i) FROM abase ORDER BY k`)
}

// TestViewF3UpdateAppliesViewAffinity is the case an affinity-neutral gate
// misses entirely: update.c:983 applies the VIEW's own column affinities to
// NEW.* before the body runs, with no isView guard. Both directions are here --
// numbers landing in a TEXT column and text landing in the numeric ones --
// together with the two that must NOT convert, because a fix that coerced
// everything would pass a one-directional test.
func TestViewF3UpdateAppliesViewAffinity(t *testing.T) {
	differ(t, "F3 update applies view affinity", viewF3AffCase(
		// TEXT affinity stringifies an INTEGER and a REAL...
		`UPDATE av SET t=5 WHERE k=1`,
		`UPDATE av SET t=5.5 WHERE k=1`,
		// ...but never touches a BLOB or a NULL.
		`UPDATE av SET t=x'41' WHERE k=1`,
		`UPDATE av SET t=NULL WHERE k=1`,
		// INTEGER affinity converts a fully-numeric string, keeps a real-valued
		// one real, and leaves a non-numeric string alone.
		`UPDATE av SET i='7' WHERE k=1`,
		`UPDATE av SET i='7.5' WHERE k=1`,
		`UPDATE av SET i='abc' WHERE k=1`,
		`UPDATE av SET i='  9  ' WHERE k=1`,
		// REAL affinity.
		`UPDATE av SET r='7' WHERE k=1`,
		`UPDATE av SET r=7 WHERE k=1`,
		// NUMERIC affinity, including the integer-valued-real fold.
		`UPDATE av SET n='7' WHERE k=1`,
		`UPDATE av SET n='7.0' WHERE k=1`,
		`UPDATE av SET n='7.25' WHERE k=1`,
		// BLOB affinity converts nothing at all -- the guard against a blanket
		// coercion.
		`UPDATE av SET b='7' WHERE k=1`,
		`UPDATE av SET b=7 WHERE k=1`,
		// Several at once, and one computed from another column.
		`UPDATE av SET t=1+1, i='3', r='4', n='5', b='6' WHERE k=1`,
		`UPDATE av SET t=k*100 WHERE k=1`,
	))
}

// TestViewF3InsertSkipsAffinity is the other side of that asymmetry, and it is
// what stops "apply the view's affinities" from being fixed in the wrong place:
// insert.c:1490-1491 SKIPS sqlite3TableAffinity for a view, so an INSTEAD OF
// INSERT trigger sees the tuple's values raw. Same view, same columns, same
// literals as the UPDATE case above -- and a different answer.
func TestViewF3InsertSkipsAffinity(t *testing.T) {
	differ(t, "F3 insert skips view affinity", viewF3AffCase(
		`INSERT INTO av VALUES(2,5,'7','7','7','7')`,
		`INSERT INTO av VALUES(3,5.5,'7.5','7.5','7.5','7.5')`,
		`INSERT INTO av(k,t,i) VALUES(4,5,'9')`,
		`INSERT INTO av SELECT 5,5,'7','7','7','7'`,
	))
}

// TestViewF3UpdateFromAppliesViewAffinity covers the OTHER view UPDATE loop.
// "UPDATE <view> ... FROM <sources>" walks the join rather than the view's rows
// (compileViewUpdateFromStmt, engine/vdbe_view_update_from.go), and it reaches
// update.c's affinity block by the
// same route: with nChangeFrom set, update.c reads each changed column out of
// the join's ephemeral table (OP_Column, update.c:952) instead of coding its
// expression, then falls through to the very same unguarded
// "if( tmask&TRIGGER_BEFORE ){ sqlite3TableAffinity(...)" at update.c:982-983.
// It carried the identical wrong answer and is fixed by the identical call.
func TestViewF3UpdateFromAppliesViewAffinity(t *testing.T) {
	differ(t, "F3 update-from applies view affinity", []string{
		`CREATE TABLE abase(k INTEGER PRIMARY KEY, t TEXT, i INTEGER)`,
		`CREATE TABLE asink(seq INTEGER PRIMARY KEY, d TEXT)`,
		`CREATE TABLE m(k INTEGER, tv, iv)`,
		`CREATE VIEW av AS SELECT k,t,i FROM abase`,
		`CREATE TRIGGER avu INSTEAD OF UPDATE ON av BEGIN
			INSERT INTO asink(d) VALUES('t='||typeof(new.t)||':'||quote(new.t)||' i='||typeof(new.i)||':'||quote(new.i));
		END`,
		`INSERT INTO abase VALUES(1,'a',0)`,
		// An INTEGER heading for the TEXT column and a numeric string heading
		// for the INTEGER one -- both directions, as in the plain-path case.
		`INSERT INTO m VALUES(1, 5, '7')`,
		`UPDATE av SET t = m.tv, i = m.iv FROM m WHERE m.k = av.k`,
		`SELECT seq, d FROM asink ORDER BY seq`,
		`SELECT k, typeof(t), quote(t) FROM abase ORDER BY k`,
	})
}

// viewF3WhenCase builds a view with ONE INSTEAD OF UPDATE trigger carrying the
// given WHEN clause. One trigger per case on purpose: several triggers on one
// view would also be testing the order they fire in, which is not this file's
// question.
func viewF3WhenCase(when string, stmts ...string) []string {
	out := []string{
		`CREATE TABLE wbase(k INTEGER PRIMARY KEY, t TEXT, i INTEGER)`,
		`CREATE TABLE wlog(seq INTEGER PRIMARY KEY, what TEXT)`,
		`CREATE VIEW wv AS SELECT k,t,i FROM wbase`,
		`CREATE TRIGGER wv1 INSTEAD OF UPDATE ON wv WHEN ` + when + ` BEGIN
			INSERT INTO wlog(what) VALUES('fired t='||typeof(new.t)||':'||quote(new.t)||' i='||typeof(new.i)||':'||quote(new.i));
		END`,
		`INSERT INTO wbase VALUES(1,'t0',0)`,
	}
	out = append(out, stmts...)
	return append(out, `SELECT seq, what FROM wlog ORDER BY seq`)
}

// TestViewF3WhenClauseSeesAffinity pins that the WHEN clause is evaluated
// AFTER the affinity pass, so its truth can depend on it. That ordering is
// update.c's literally consecutive pair -- sqlite3TableAffinity at :983,
// sqlite3CodeRowTrigger (which codes the WHEN, trigger.c's codeRowTrigger) at
// :984 -- and it is why an engine that skipped the affinity pass did not
// merely record a different value, it FAILED TO FIRE AT ALL.
func TestViewF3WhenClauseSeesAffinity(t *testing.T) {
	// FIRES only if the assigned INTEGER 5 became the TEXT '5'.
	differ(t, "F3 when typeof text", viewF3WhenCase(`typeof(new.t)='text'`,
		`UPDATE wv SET t=5 WHERE k=1`,
		`UPDATE wv SET t=5.5 WHERE k=1`,
		`UPDATE wv SET t=x'41' WHERE k=1`,
	))
	// The converse, so the pair cannot both pass by never firing: this one
	// fires only if it did NOT convert.
	differ(t, "F3 when typeof integer", viewF3WhenCase(`typeof(new.t)='integer'`,
		`UPDATE wv SET t=5 WHERE k=1`,
		`UPDATE wv SET t='5' WHERE k=1`,
	))
	// The equality spelling, verified against 3.53.3 directly: the oracle
	// fires and an engine without the affinity pass does not.
	differ(t, "F3 when equality", viewF3WhenCase(`new.t = '5'`,
		`UPDATE wv SET t=5 WHERE k=1`,
		`UPDATE wv SET t=6 WHERE k=1`,
	))
	// The numeric direction: a fully-numeric string into an INTEGER-affinity
	// view column.
	differ(t, "F3 when numeric affinity", viewF3WhenCase(`typeof(new.i)='integer'`,
		`UPDATE wv SET i='7' WHERE k=1`,
		`UPDATE wv SET i='abc' WHERE k=1`,
		`UPDATE wv SET i='7.5' WHERE k=1`,
	))
}

// ---------------------------------------------------------------------------
// The row-at-a-time divergence, PINNED.
// ---------------------------------------------------------------------------

// viewF3LastRows renders a run's FINAL statement result as one compact string,
// so a pin can be a readable literal rather than the whole JSON transcript.
func viewF3LastRows(res []map[string]any) string {
	if len(res) == 0 {
		return "<no results>"
	}
	rows, _ := res[len(res)-1]["rows"].([]any)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		cells, _ := r.([]any)
		cs := make([]string, 0, len(cells))
		for _, c := range cells {
			cs = append(cs, fmt.Sprint(c))
		}
		out = append(out, strings.Join(cs, "|"))
	}
	return strings.Join(out, " ; ")
}

// TestViewF3UpdateRowAtATime is the row-at-a-time half of a view UPDATE, and
// it used to be TestViewF3UpdateIsNotRowAtATime -- a two-case PIN on a known
// wrong answer. Batch P (engine/vdbe_view_write.go's compileViewUpdateStmt)
// closed one of the two, so that case is now an ordinary oracle comparison and
// this one keeps the remaining pin beside it.
//
// The rule, and where it lives in the C. For a view eOnePass is ONEPASS_OFF --
// pTrigger is never NULL, so update.c:732-740 withholds
// WHERE_ONEPASS_MULTIROW -- which makes the UPDATE genuinely TWO-PASS: the
// WHERE loop opens at update.c:742, writes only rowids into an ephemeral FIFO
// (update.c:776, or :798 for a WITHOUT ROWID target) and CLOSES at
// update.c:805. The second loop (update.c:873-878) then walks that FIFO, and it
// is there that the SET right-hand sides are coded (sqlite3ExprCode,
// update.c:954), the affinity pass runs (update.c:983) and the INSTEAD OF
// trigger fires (sqlite3CodeRowTrigger, update.c:984-985) -- all per row. So
// row N's SET expressions observe what row N-1's trigger body wrote.
//
//   - "total_changes" is that rule, observed through a connection-state
//     counter. It now MATCHES the oracle: the compiled emitter is that second
//     loop, and OpConnState reads the live *DB inside a write run
//     (connStateSource, engine/conn_state.go). Asserted as an equality, not
//     pinned -- if it ever stops matching, the promotion regressed.
//
//   - "reads-mutated-base" is the same rule observed through a CORRELATED
//     SUBQUERY over the base table the bodies are writing. It was pinned wrong
//     while every write subquery read the pre-statement snapshot
//     (writeSubqueryPager); a SET subquery over a table the row loop writes is
//     now lowered per row, and it matches.
//
// The `open` field on that case is a PIN, not an expectation, borrowed from
// failreturning_r39c_test.go and orfail_survivors_test.go: it fails if this
// engine moves off it in EITHER direction -- including onto the oracle's
// answer, which is the signal to delete the pin rather than a reason to
// celebrate quietly.
func TestViewF3UpdateRowAtATime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
		// open is what THIS engine answers today, for a case still pinned
		// WRONG; "" means the case must simply MATCH the oracle.
		open string
	}{
		{
			// total_changes() advances by one per trigger-body UPDATE, so the
			// three rows read 3, 4 and 5 -- which this engine now answers too,
			// because compileViewUpdateStmt fires per row (update.c:954/:984)
			// instead of collecting every (OLD, NEW) pair first.
			name: "total_changes",
			stmts: []string{
				`CREATE TABLE base(k INTEGER PRIMARY KEY, a TEXT)`,
				`CREATE VIEW v AS SELECT k,a FROM base`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE base SET a=new.a WHERE k=old.k; END`,
				`INSERT INTO base VALUES(1,'a1'),(2,'a2'),(3,'a3')`,
				`UPDATE v SET a = 'tc=' || total_changes()`,
				`SELECT k,a FROM base ORDER BY k`,
			},
		},
		{
			// A second observable of the same ordering by a different
			// mechanism: the SET reads the BASE TABLE the bodies are writing,
			// through a CORRELATED subquery so SQLite cannot hoist it out of
			// the scan. Row N reads what row N-1's body just wrote, so the
			// oracle's values chain; this engine reads the pre-statement
			// snapshot for all three.
			//
			// The correlation is load-bearing, and so is reading a TABLE
			// rather than a counter: an uncorrelated "(SELECT n FROM ctr)" is
			// hoisted and runs once, and last_insert_rowid() is RESTORED when
			// a trigger program exits -- both answer identically on the two
			// engines and would pin nothing. total_changes() above and this
			// are the two spellings that actually diverge; the oracle-side
			// assertion below is what rejected the other three.
			name: "reads-mutated-base",
			stmts: []string{
				`CREATE TABLE base(k INTEGER PRIMARY KEY, a TEXT)`,
				`CREATE VIEW v AS SELECT k,a FROM base`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE base SET a=new.a WHERE k=old.k; END`,
				`INSERT INTO base VALUES(1,'a1'),(2,'a2'),(3,'a3')`,
				`UPDATE v SET a = 'p:' || coalesce((SELECT b2.a FROM base b2 WHERE b2.k = v.k - 1), 'none')`,
				`SELECT k,a FROM base ORDER BY k`,
			},
			// Oracle: "p:none ; p:p:none ; p:p:p:none" -- and this engine's
			// answer since the view's SET subquery over a table its INSTEAD OF
			// program writes is lowered per row (compileViewUpdateStmt's
			// emitSetValue).
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := viewF3LastRows(run(t, "cgo", tc.stmts))
			got := viewF3LastRows(run(t, "musql", tc.stmts))
			b, _ := json.Marshal(tc.stmts)
			if tc.open == "" {
				// An ordinary oracle comparison: this shape is SERVED.
				if got != want {
					t.Errorf("%s: musql=%q oracle=%q\n  sql: %s", tc.name, got, want, b)
				}
				return
			}
			if got != tc.open {
				t.Errorf("%s: this engine now answers %q, not the pinned %q.\n"+
					"  oracle: %q\n  sql: %s\n"+
					"  If it now EQUALS the oracle, drop this case's `open` field so it\n"+
					"  becomes a plain comparison -- the divergence is gone. A stale pin\n"+
					"  is what lets the next regression here pass unnoticed.",
					tc.name, got, tc.open, want, b)
			}
			if want == tc.open {
				t.Errorf("%s: the ORACLE now answers %q, which is what this engine was\n"+
					"  pinned to as WRONG. The divergence this case documents is gone;\n"+
					"  drop this case's `open` field.", tc.name, want)
			}
		})
	}
}
