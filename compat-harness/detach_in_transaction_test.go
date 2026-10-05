// This file tests DETACH in an open transaction. C SQLite refuses to detach a
// database that the transaction has touched (either read or written). The untouched
// case is allowed.
package compat

import "testing"

// TestDetachInTransaction covers all five cases, in both directions.
func TestDetachInTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE s(x)", "INSERT INTO s VALUES(1)")

	// Transaction open, untouched attachment.
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE m(a)")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("BEGIN")
	p.agreeExec("DETACH aux")
	p.agreeExec("COMMIT")

	// Read of main only: aux untouched.
	p2 := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p2.agreeExec("CREATE TABLE m(a)")
	p2.agreeExec("ATTACH '{0}' AS aux")
	p2.agreeExec("BEGIN")
	p2.agreeQuery("SELECT a FROM m")
	p2.agreeExec("DETACH aux")
	p2.agreeExec("COMMIT")

	// Read of attachment locks it.
	p3 := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p3.agreeExec("CREATE TABLE m(a)")
	p3.agreeExec("ATTACH '{0}' AS aux")
	p3.agreeExec("BEGIN")
	p3.agreeQuery("SELECT x FROM aux.s")
	p3.declineExec("DETACH aux")
	p3.agreeExec("COMMIT")
	// ...and once the transaction ends, the lock goes with it.
	p3.agreeExec("DETACH aux")

	// Write locks it too.
	p4 := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p4.agreeExec("ATTACH '{0}' AS aux")
	p4.agreeExec("BEGIN")
	p4.agreeExec("INSERT INTO aux.s VALUES(2)")
	p4.declineExec("DETACH aux")
	p4.agreeExec("COMMIT")
	p4.agreeExec("DETACH aux")

	// No transaction at all.
	p5 := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p5.agreeExec("ATTACH '{0}' AS aux")
	p5.agreeExec("DETACH aux")
}

// TestDetachInTransactionSavepoint tests DETACH in a SAVEPOINT (the corpus's
// own shape).
func TestDetachInTransactionSavepoint(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE s(x)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("SAVEPOINT one")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("DETACH aux")
	p.agreeExec("RELEASE one")
	// ...and again, re-attaching the same file under the same name.
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("SAVEPOINT two")
	p.agreeExec("DETACH aux")
	p.agreeExec("ATTACH '{0}' AS aux")
	p.agreeExec("RELEASE two")
	p.agreeQuery("SELECT count(*) FROM aux.s")
}
