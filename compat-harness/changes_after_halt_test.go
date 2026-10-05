// This file gates changes(), total_changes(), and last_insert_rowid() after
// halted statements and transaction commits. These counters must be read from
// the correct session, not from a stale driver copy.
package compat

import (
	"fmt"
	"testing"
)

// cahRead is the three counters, read the way a caller would.
const cahRead = `SELECT changes(), total_changes(), last_insert_rowid()`

// cahConstraints are the constraint kinds a halt can come from. Each table's
// THIRD inserted row violates it, so an OR FAIL keeps two and an OR ABORT
// keeps none -- the difference the change counter is supposed to show.
var cahConstraints = []struct{ name, create, rows string }{
	{"notnull", `CREATE TABLE t(a, b NOT NULL)`,
		`VALUES(10,'x'),(20,'y'),(30,NULL),(40,'z')`},
	{"check", `CREATE TABLE t(a, b CHECK(b<>'bad'))`,
		`VALUES(10,'x'),(20,'y'),(30,'bad'),(40,'z')`},
	{"unique", `CREATE TABLE t(a, b UNIQUE)`,
		`VALUES(10,'x'),(20,'y'),(30,'seed'),(40,'z')`},
	{"pk", `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`VALUES(10,'x'),(20,'y'),(0,'dup'),(40,'z')`},
}

// TestChangesAfterHaltedStatement is the defect itself: a halted statement
// inside a transaction, read before AND after the transaction ends.
func TestChangesAfterHaltedStatement(t *testing.T) {
	for _, c := range cahConstraints {
		for _, or := range []string{"", "OR FAIL", "OR ABORT", "OR ROLLBACK", "OR IGNORE", "OR REPLACE"} {
			for _, tail := range []string{"COMMIT", "ROLLBACK"} {
				cfaDiffer(t, fmt.Sprintf("%s/%s/%s", c.name, or, tail), []string{
					c.create,
					`INSERT INTO t VALUES(0,'seed')`,
					`BEGIN`,
					fmt.Sprintf(`INSERT %s INTO t %s`, or, c.rows),
					cahRead,
					tail,
					cahRead,
					`SELECT a FROM t ORDER BY a`,
					// ...and once more, to catch a counter that is correct
					// only until the next read moves it.
					cahRead,
				})
			}
		}
	}
}

// TestChangesAfterHaltedUpdateDelete covers the other two DML kinds, which
// reach the halt from a different place in the write path.
func TestChangesAfterHaltedUpdateDelete(t *testing.T) {
	seed := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b NOT NULL)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r'),(4,'s')`,
	}
	for _, stmt := range []string{
		`UPDATE t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		`UPDATE OR FAIL t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		`UPDATE OR ABORT t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		`UPDATE OR IGNORE t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		`UPDATE OR ROLLBACK t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`,
		// an UPDATE that moves the PRIMARY KEY onto an existing one
		`UPDATE OR FAIL t SET a = a + 1 WHERE a >= 2`,
		// a DELETE cannot violate NOT NULL, so it is the SUCCESS control
		`DELETE FROM t WHERE a > 2`,
		`DELETE FROM t WHERE a > 99`,
	} {
		for _, tail := range []string{"COMMIT", "ROLLBACK"} {
			stmts := append(append([]string{}, seed...),
				`BEGIN`, stmt, cahRead, tail, cahRead,
				`SELECT a, b FROM t ORDER BY a`)
			cfaDiffer(t, stmt+"/"+tail, stmts)
		}
	}
}

// TestChangesAfterHaltThroughSavepoints runs the same halt under the savepoint
// spellings, which end the transaction through a different driver path than
// COMMIT/ROLLBACK do.
func TestChangesAfterHaltThroughSavepoints(t *testing.T) {
	for _, tail := range [][]string{
		{`RELEASE sp`},
		{`ROLLBACK TO sp`, `COMMIT`},
		{`ROLLBACK TO sp`, `ROLLBACK`},
		{`RELEASE sp`, `COMMIT`},
	} {
		for _, or := range []string{"OR FAIL", "OR ABORT"} {
			stmts := []string{
				`CREATE TABLE t(a, b NOT NULL)`,
				`INSERT INTO t VALUES(0,'seed')`,
				`BEGIN`,
				`SAVEPOINT sp`,
				fmt.Sprintf(`INSERT %s INTO t VALUES(10,'x'),(20,'y'),(30,NULL),(40,'z')`, or),
				cahRead,
			}
			stmts = append(stmts, tail...)
			stmts = append(stmts, cahRead, `SELECT a FROM t ORDER BY a`)
			cfaDiffer(t, fmt.Sprintf("%s/%v", or, tail), stmts)
		}
	}
	// ...and a bare SAVEPOINT with no BEGIN, which starts the transaction
	// itself -- the shape whose RELEASE must also commit it.
	for _, or := range []string{"OR FAIL", "OR ABORT"} {
		cfaDiffer(t, "bare-savepoint/"+or, []string{
			`CREATE TABLE t(a, b NOT NULL)`,
			`INSERT INTO t VALUES(0,'seed')`,
			`SAVEPOINT sp`,
			fmt.Sprintf(`INSERT %s INTO t VALUES(10,'x'),(20,'y'),(30,NULL),(40,'z')`, or),
			cahRead,
			`RELEASE sp`,
			cahRead,
			`SELECT a FROM t ORDER BY a`,
		})
	}
}

// TestChangesAfterHaltWithTriggers matters because total_changes() counts
// trigger rows that changes() does not (engine/conn_state.go), so a halt
// partway through a trigger-firing statement is where the two counters are
// most likely to be published from different places.
func TestChangesAfterHaltWithTriggers(t *testing.T) {
	for _, or := range []string{"", "OR FAIL", "OR ABORT", "OR IGNORE"} {
		for _, tail := range []string{"COMMIT", "ROLLBACK"} {
			cfaDiffer(t, "trigger/"+or+"/"+tail, []string{
				`CREATE TABLE t(a, b NOT NULL)`,
				`CREATE TABLE log(v)`,
				`INSERT INTO t VALUES(0,'seed')`,
				`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
				`BEGIN`,
				fmt.Sprintf(`INSERT %s INTO t VALUES(10,'x'),(20,'y'),(30,NULL),(40,'z')`, or),
				cahRead,
				tail,
				cahRead,
				`SELECT a FROM t ORDER BY a`,
				`SELECT v FROM log ORDER BY v`,
			})
		}
	}
}

// TestChangesAcrossCleanTransaction is the control the fix must not disturb:
// no halt anywhere, every statement succeeding. If this ever diverges the
// change above went too far.
func TestChangesAcrossCleanTransaction(t *testing.T) {
	for _, tail := range []string{"COMMIT", "ROLLBACK"} {
		cfaDiffer(t, "clean/"+tail, []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
			`INSERT INTO t VALUES(1,'p')`,
			cahRead,
			`BEGIN`,
			`INSERT INTO t VALUES(2,'q'),(3,'r')`,
			cahRead,
			`UPDATE t SET b='z' WHERE a<3`,
			cahRead,
			`DELETE FROM t WHERE a=99`,
			cahRead,
			tail,
			cahRead,
			`SELECT a, b FROM t ORDER BY a`,
		})
	}
	// ...and the autocommit halt, which never went through the held session
	// at all and was already right -- kept so a regression there is caught by
	// the same file.
	for _, or := range []string{"OR FAIL", "OR ABORT", "OR ROLLBACK"} {
		cfaDiffer(t, "autocommit/"+or, []string{
			`CREATE TABLE t(a, b NOT NULL)`,
			`INSERT INTO t VALUES(0,'seed')`,
			fmt.Sprintf(`INSERT %s INTO t VALUES(10,'x'),(20,'y'),(30,NULL),(40,'z')`, or),
			cahRead,
			`SELECT a FROM t ORDER BY a`,
		})
	}
}
