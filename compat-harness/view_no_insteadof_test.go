// Tests writes against views with no matching INSTEAD OF trigger.
package compat

import "testing"

// TestViewNoInsteadOfChangesIsPreserved verifies that changes() is preserved when a write fails.
func TestViewNoInsteadOfChangesIsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"insert", `INSERT INTO v VALUES(9,9)`},
		{"update", `UPDATE v SET c=9`},
		{"delete", `DELETE FROM v WHERE a=1`},
		// A shape the view compiler does not model either way: the rejection
		// must still win, because sqlite3IsReadOnly runs before sqlite3Insert
		// looks at the row source at all.
		{"insert-select", `INSERT INTO v SELECT a,c FROM b`},
		{"insert-default-values", `INSERT INTO v DEFAULT VALUES`},
		{"update-from", `UPDATE v SET c=m.nv FROM m WHERE m.k=v.a`},
		// RETURNING does not save a view: pTrigger is non-NULL for one
		// (trigger.c:68-78), which is exactly why delete.c:125 tests bReturning.
		{"insert-returning", `INSERT INTO v VALUES(9,9) RETURNING a`},
		{"update-returning", `UPDATE v SET c=9 RETURNING a`},
		{"delete-returning", `DELETE FROM v RETURNING a`},
	} {
		differ(t, "view with no INSTEAD OF trigger: "+tc.name, []string{
			`CREATE TABLE b(a,c)`,
			`CREATE TABLE m(k,nv)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
			tc.stmt,
			`SELECT changes(), total_changes()`,
			`SELECT a,c FROM b ORDER BY a`,
		})
	}
}

// TestViewNoInsteadOfWrongVerbStillRejects pins the per-EVENT half of the
// guard: matchingInsteadOfTriggers is asked for THIS statement's event, so a
// view carrying only an INSTEAD OF INSERT trigger still rejects an UPDATE and a
// DELETE. The C reads the same way -- sqlite3TriggersExist is called with the
// statement's own op (update.c:377, delete.c:352) before sqlite3IsReadOnly sees
// the list.
func TestViewNoInsteadOfWrongVerbStillRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"update-with-only-insert-trigger", `UPDATE v SET c=9`},
		{"delete-with-only-insert-trigger", `DELETE FROM v`},
	} {
		differ(t, "view rejects the wrong verb: "+tc.name, []string{
			`CREATE TABLE b(a,c)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
			tc.stmt,
			`SELECT changes(), total_changes()`,
			`SELECT a,c FROM b ORDER BY a`,
		})
	}
}

// TestTriggerBodyDiscardSelectAnswers is the oracle half of the FROM-bearing
// bare SELECT body promotion (compileTriggerBodyDiscardSelect,
// engine/vdbe_trigger.go). Every case below was already answered correctly
// before the promotion -- which is why the shape sat where it did -- so this
// file cannot prove the promotion; engine/view_reject_body_select_codegen_test.go
// does that. What these pin is that the COMPILED route did not change any answer,
// including the three that a materialize-and-discard could plausibly get wrong:
// whether the discarded rows are evaluated at all, whether the read is live,
// and what an ORDER BY does.
func TestTriggerBodyDiscardSelectAnswers(t *testing.T) {
	differ(t, "body SELECT: a per-row error in a DISCARDED select-list expression still aborts", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT abs(x) FROM s; END`,
		`INSERT INTO s VALUES(-9223372036854775808)`,
		`INSERT INTO t VALUES(1)`,
		`SELECT a FROM t`,
		`SELECT changes(), total_changes()`,
	})
	differ(t, "body SELECT: no rows, no error", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT abs(x) FROM s; END`,
		`INSERT INTO t VALUES(1)`,
		`SELECT a FROM t`,
		`SELECT changes(), total_changes()`,
	})
	// LIVE: the body must see the row the firing statement just stored. A
	// compile-time snapshot sees an empty table and raises nothing.
	differ(t, "body SELECT: the body sees the firing statement's own row", []string{
		`CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON s BEGIN SELECT abs(x) FROM s; END`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO s VALUES(-9223372036854775808)`,
		`SELECT x FROM s ORDER BY x`,
		`SELECT changes(), total_changes()`,
	})
	// A bare SELECT step emits no OP_ResetCount (trigger.c:1179-1186 vs the
	// three DML arms at :1157/:1166/:1174), so it must not republish changes().
	differ(t, "body SELECT: a bare SELECT step does not republish changes()", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s; END`,
		`INSERT INTO t VALUES(1),(2)`,
		`SELECT changes(), total_changes()`,
	})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"where", `SELECT x FROM s WHERE x>1`},
		{"aggregate", `SELECT count(*) FROM s`},
		{"limit", `SELECT x FROM s LIMIT 1`},
		{"derived table", `SELECT y FROM (SELECT x AS y FROM s)`},
		{"compound", `SELECT x FROM s UNION ALL SELECT x FROM s`},
		{"distinct", `SELECT DISTINCT x FROM s`},
		{"order by", `SELECT x FROM s ORDER BY x DESC`},
	} {
		differ(t, "body SELECT shape: "+tc.name, []string{
			`CREATE TABLE t(a)`,
			`CREATE TABLE s(x)`,
			`INSERT INTO s VALUES(3),(1),(2),(1)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN ` + tc.body + `; END`,
			`INSERT INTO t VALUES(1)`,
			`SELECT a FROM t`,
			`SELECT changes(), total_changes()`,
		})
	}
}

// TestSchemaQualifiedViewUpdateRejectsAnUnknownDatabase pins a WRONG ANSWER
// this batch retired, found by mutation-testing an unrelated promotion rather
// than by looking for it.
//
// "UPDATE v5 SET b = nosuchdb.v5.b+1" used to SUCCEED and fire the view's
// INSTEAD OF trigger: the compiled path declined the three-part reference for
// want of a pager, and the route it fell back to validated no database
// qualifier at all. resolve.c does: a zDb that matches no entry of
// db->aDb leaves pSchema NULL (resolve.c:313-336), nothing in the FROM list can
// then match (resolve.c:420-422), and lookupName reports
// "no such column: %s.%s.%s" (resolve.c:786-787).
//
// The "main." spelling in the same position must keep ANSWERING, which is what
// makes this a fix rather than a new refusal.
func TestSchemaQualifiedViewUpdateRejectsAnUnknownDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"unknown database qualifier", `UPDATE v5 SET b = nosuchdb.v5.b+1`},
		{"unknown database qualifier in the WHERE", `UPDATE v5 SET b = b+1 WHERE nosuchdb.v5.x>2`},
		{"main-qualified still answers", `UPDATE v5 SET b = main.v5.b+9900000 WHERE main.v5.x BETWEEN 3 AND 5`},
		{"main-qualified DELETE still answers", `DELETE FROM v5 WHERE main.v5.x>2`},
		{"unqualified, the control", `UPDATE v5 SET b = b+9900000 WHERE x BETWEEN 3 AND 5`},
	} {
		differ(t, "schema-qualified column in a view write: "+tc.name, []string{
			`CREATE TABLE b(x,b)`,
			`CREATE TABLE log(k,v)`,
			`CREATE VIEW v5 AS SELECT x,b FROM b`,
			`CREATE TRIGGER v5u INSTEAD OF UPDATE ON v5 BEGIN INSERT INTO log VALUES(new.x,new.b); END`,
			`CREATE TRIGGER v5d INSTEAD OF DELETE ON v5 BEGIN INSERT INTO log VALUES(old.x,-1); END`,
			`INSERT INTO b VALUES(1,10),(3,30),(4,40),(6,60)`,
			tc.stmt,
			`SELECT k,v FROM log ORDER BY k,v`,
			`SELECT changes(), total_changes()`,
		})
	}
}

// TestTriggerBodyWritingATriggerlessViewFailsAtPrepare is the one case whose
// answer this batch had to MEASURE rather than derive, because the hard error
// moved it: a trigger BODY statement writing a view with no INSTEAD OF trigger.
//
// codeTriggerProgram codes every body step unconditionally -- a WHEN guard is a
// run-time jump, not a compile-time one -- so the body's sqlite3Insert reaches
// sqlite3IsReadOnly (insert.c:1009) while the FIRING statement is still being
// prepared, and the whole firing statement fails before it writes a row. The
// "when-false" case is the sharp one: the body never runs, and the statement
// still fails.
//
// On the old decline route the error surfaced only once the firing statement
// was already running, so it had applied rows by the time it was raised.
func TestTriggerBodyWritingATriggerlessViewFailsAtPrepare(t *testing.T) {
	for _, tc := range []struct {
		name string
		when string
	}{
		{"when-true", ``},
		{"when-false", ` WHEN 0`},
	} {
		differ(t, "trigger body writes a trigger-less view: "+tc.name, []string{
			`CREATE TABLE t(x)`,
			`CREATE TABLE b(a,c)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER tr AFTER INSERT ON t` + tc.when + ` BEGIN INSERT INTO v VALUES(new.x,1); END`,
			`INSERT INTO t VALUES(1),(2)`,
			`SELECT changes(), total_changes()`,
			`SELECT x FROM t ORDER BY x`,
			`SELECT a,c FROM b ORDER BY a`,
		})
	}
}
