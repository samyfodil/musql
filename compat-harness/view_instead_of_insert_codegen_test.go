package compat

import "testing"

// TestViewInsteadOfInsertCodegen verifies that "INSERT INTO <view> VALUES"
// compiles to bytecode correctly, matching C SQLite on row assembly, trigger
// firing, connection state, and declined shapes.
func TestViewInsteadOfInsertCodegen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// ---- NEW row assembly ----
		{"positional", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1,2)`, `SELECT a,c FROM b`}},
		{"multi-row", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1,2),(3,4),(5,6)`, `SELECT a,c FROM b ORDER BY rowid`}},
		{"named-out-of-order", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v(c,a) VALUES('x','y')`, `SELECT a,c FROM b`}},
		{"named-partial-leaves-null", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v(c) VALUES(7)`, `SELECT a IS NULL, c FROM b`}},
		{"doubly-named-column-first-wins", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v(a,a) VALUES(1,2)`, `SELECT a, c IS NULL FROM b`}},
		{"view-rename-list", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`, `CREATE VIEW v(p,q) AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.p,new.q); END`,
			`INSERT INTO v(q,p) VALUES(1,2)`, `SELECT x,y FROM log`}},
		{"expr-values", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1+2, upper('q'))`, `SELECT a,c FROM b`}},
		// Unmapped tuple slot is never evaluated (loses first-wins race).
		{"unmapped-tuple-expression-is-not-evaluated", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v(a,a) VALUES(1, abs(-9223372036854775807-1))`, `SELECT a, c IS NULL FROM b`}},
		{"mapped-tuple-expression-still-errors", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v(a,a) VALUES(abs(-9223372036854775807-1), 1)`, `SELECT count(*) FROM b`}},

		// No affinity conversion for views
		{"no-affinity-conversion", []string{
			`CREATE TABLE b(t TEXT)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT t FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(typeof(new.t)); END`,
			`INSERT INTO v VALUES(5)`, `SELECT x FROM log`}},

		// ---- the firing itself ----
		{"when-clause", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v WHEN new.a > 2 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(3),(5)`, `SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"when-subquery-binds-at-fire-time", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(1)`, `INSERT INTO s VALUES(9)`, `INSERT INTO v VALUES(2)`,
			`SELECT x FROM log ORDER BY rowid`}},
		{"two-triggers-newest-first", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER v1 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('one'); END`,
			`CREATE TRIGGER v2 INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES('two'); END`,
			`INSERT INTO v VALUES(1)`, `SELECT x FROM log ORDER BY rowid`}},
		{"raise-ignore-skips-one-row", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(IGNORE) WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(2),(3)`, `SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"raise-abort-undoes-the-statement", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN SELECT RAISE(ABORT,'no') WHERE new.a=2; INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(2),(3)`, `SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"body-read-is-live", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log SELECT count(*) FROM b; END`,
			`INSERT INTO v VALUES(1),(2),(3)`, `SELECT x FROM log ORDER BY rowid`}},
		{"view-body-writes-another-view", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`, `CREATE VIEW w AS SELECT a FROM b`,
			`CREATE TRIGGER wi INSTEAD OF INSERT ON w BEGIN INSERT INTO log VALUES(new.a*10); END`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO w VALUES(new.a); END`,
			`INSERT INTO v VALUES(4)`, `SELECT x FROM log`}},
		{"view-insert-from-a-table-trigger-body", []string{
			`CREATE TABLE t(a)`, `CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a+100); END`,
			`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO v VALUES(new.a); END`,
			`INSERT INTO t VALUES(1),(2)`, `SELECT x FROM log ORDER BY rowid`, `SELECT changes()`}},
		{"body-constraint-failure-rolls-the-statement-back", []string{
			`CREATE TABLE b(a UNIQUE)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO v VALUES(1),(1)`, `SELECT count(*) FROM b`, `SELECT count(*) FROM log`, `SELECT changes()`}},

		// Connection state (changes, last_insert_rowid)
		{"changes-and-last-insert-rowid", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log VALUES(last_insert_rowid()); END`,
			`INSERT INTO v VALUES(9)`,
			`SELECT changes()`, `SELECT total_changes()`, `SELECT last_insert_rowid()`, `SELECT x FROM log`}},

		// ---- scope: temp, qualified, view-of-view ----
		{"temp-view", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE TEMP VIEW v AS SELECT a FROM b`,
			`CREATE TEMP TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(3)`, `SELECT x FROM log`}},
		{"schema-qualified-target", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO main.v VALUES(6)`, `SELECT x FROM log`}},
		{"view-of-view", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`,
			`CREATE VIEW inner1 AS SELECT a FROM b`, `CREATE VIEW v AS SELECT a FROM inner1`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(5)`, `SELECT x FROM log`}},

		// Write-plan caching (compiled program cached by schema/tx gen)
		{"same-sql-thrice-in-one-txn", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log SELECT count(*) FROM b; END`,
			`BEGIN`, `INSERT INTO v VALUES(1)`, `INSERT INTO v VALUES(1)`, `INSERT INTO v VALUES(1)`, `COMMIT`,
			`SELECT x FROM log ORDER BY rowid`}},
		{"view-replaced-mid-session", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE log(x,y)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1,2)`,
			`DROP TRIGGER vi`, `DROP VIEW v`,
			`CREATE VIEW v AS SELECT c AS a, a AS c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1,2)`,
			`SELECT x,y FROM log ORDER BY rowid`}},
		{"txn-rollback", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`BEGIN`, `INSERT INTO v VALUES(1)`, `ROLLBACK`, `INSERT INTO v VALUES(2)`,
			`SELECT x FROM log ORDER BY rowid`}},

		// Declined shapes (must match oracle behavior)
		{"declined-no-instead-of-trigger", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`, `INSERT INTO b VALUES(1)`,
			`INSERT INTO v VALUES(2)`, `SELECT changes()`, `SELECT count(*) FROM b`}},
		{"declined-upsert-on-view", []string{
			`CREATE TABLE b(a)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`INSERT INTO b VALUES(1)`,
			`INSERT INTO v VALUES(2) ON CONFLICT(a) DO NOTHING`, `SELECT changes()`, `SELECT count(*) FROM b`}},
		// DEFAULT VALUES and INSERT SELECT are now compiled (were declines).
		{"default-values", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a IS NULL); END`,
			`INSERT INTO v DEFAULT VALUES`, `SELECT x FROM log`}},
		{"insert-select-source", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(7),(8)`, `INSERT INTO v SELECT z FROM s`,
			`SELECT x FROM log ORDER BY rowid`}},
		// Arity error for SELECT source.
		{"insert-select-source-wrong-arity", []string{
			`CREATE TABLE b(a,c)`, `CREATE TABLE s(z)`, `CREATE TABLE log(x)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO s VALUES(7)`, `INSERT INTO v SELECT z FROM s`,
			`SELECT count(*) FROM log`}},
		{"declined-or-clause-governs-body", []string{
			`CREATE TABLE b(a UNIQUE)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`INSERT INTO b VALUES(1)`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); INSERT INTO log VALUES(new.a); END`,
			`INSERT OR IGNORE INTO v VALUES(1),(2)`, `SELECT a FROM b ORDER BY a`, `SELECT x FROM log ORDER BY rowid`}},
		{"declined-replace-spelling", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`REPLACE INTO v VALUES(6)`, `SELECT x FROM log`}},
		{"declined-new-rowid-in-body", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.rowid); END`,
			`INSERT INTO v VALUES(1)`, `SELECT count(*) FROM log`}},
		// A view column ACTUALLY NAMED "rowid" is a real column and must still
		// resolve -- the noRowid rule must not swallow it.
		{"view-column-named-rowid", []string{
			`CREATE TABLE b(r, x)`, `CREATE TABLE log(y)`,
			`CREATE VIEW v(rowid, x) AS SELECT r, x FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.rowid); END`,
			`INSERT INTO v VALUES(77, 1)`, `SELECT y FROM log`}},
		{"declined-arity-and-unknown-column", []string{
			`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`,
			`INSERT INTO v VALUES(1)`, `INSERT INTO v(a,c) VALUES(1)`, `INSERT INTO v(zz) VALUES(1)`,
			`SELECT count(*) FROM b`, `SELECT changes()`}},
		{"declined-view-over-missing-table", []string{
			`CREATE TABLE log(x)`, `CREATE VIEW v AS SELECT a FROM nosuch`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO v VALUES(1)`, `SELECT count(*) FROM log`}},

		// UPDATE and DELETE still work alongside INSERT
		{"update-and-delete-still-work", []string{
			`CREATE TABLE b(a)`, `CREATE TABLE log(k,x)`, `CREATE VIEW v AS SELECT a FROM b`,
			`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a); END`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES('u', old.a); END`,
			`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES('d', old.a); END`,
			`INSERT INTO v VALUES(1),(2)`,
			`UPDATE v SET a=9 WHERE a=1`, `DELETE FROM v WHERE a=2`,
			`SELECT k,x FROM log ORDER BY rowid`}},
	} {
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}
