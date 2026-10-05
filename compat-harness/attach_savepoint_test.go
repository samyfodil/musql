// This file tests SAVEPOINTs across attached database writes.
package compat

import "testing"

// TestAttachSavepointRollbackTo is the base case: an undo must reach both
// databases.
func TestAttachSavepointRollbackTo(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE t1(a)")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO t1 VALUES(1)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO t1 VALUES(2)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("ROLLBACK TO s1")
	// the read must not still serve the discarded row
	p.agreeQuery("SELECT a FROM t1 ORDER BY a")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	// work after the rollback is kept
	p.agreeExec("INSERT INTO aux.t2 VALUES(3)")
	p.agreeExec("RELEASE s1")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a FROM t1 ORDER BY a")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
}

// TestAttachSavepointOpenedBeforeFirstWrite is the lazy-open case: the
// savepoint precedes the attachment's very first write, so a ROLLBACK TO must
// discard everything that session has.
func TestAttachSavepointOpenedBeforeFirstWrite(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE t1(a)")
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("ROLLBACK TO s1")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
}

// TestAttachSavepointNested covers a stack, a RELEASE of an inner savepoint,
// and an outer rollback that must undo what the released inner one did.
func TestAttachSavepointNested(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("SAVEPOINT outer")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeExec("SAVEPOINT inner")
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeExec("RELEASE inner")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("ROLLBACK TO outer")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
}

// TestAttachSavepointWithoutTransaction pins the form where the SAVEPOINT
// itself opens the transaction, with no BEGIN at all -- then a RELEASE of it
// COMMITS, and a ROLLBACK TO does not.
func TestAttachSavepointWithoutTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeExec("SAVEPOINT s2")
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeExec("ROLLBACK TO s2")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
	p.agreeExec("RELEASE s1")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
}

// TestAttachSavepointWholeRollback keeps the pre-existing behaviour honest: a
// whole-transaction ROLLBACK still discards an attachment's writes, savepoints
// or not.
func TestAttachSavepointWholeRollback(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t2(a)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("ATTACH '{0}' AS aux")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO aux.t2 VALUES(1)")
	p.agreeExec("SAVEPOINT s1")
	p.agreeExec("INSERT INTO aux.t2 VALUES(2)")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT a FROM aux.t2 ORDER BY a")
}
