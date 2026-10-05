// This file gates ATTACH 'file:/name?vfs=memdb', the shared in-RAM database URI.
// Every statement runs on both engines and must agree; both refusing counts as
// agreement.
package compat

import "testing"

// TestEngineAttachMemdbShared verifies that a re-ATTACH of the same shared
// name after DETACH shows a genuinely fresh store (not persisted rows).
func TestEngineAttachMemdbShared(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH 'file:/cmp-memdb-shared-1?vfs=memdb' AS m1")
	p.agreeQuery("SELECT count(*) FROM m1.sqlite_master")
	p.agreeExec("CREATE TABLE m1.t(x)")
	p.agreeExec("INSERT INTO m1.t VALUES(1),(2),(3)")
	p.agreeQuery("SELECT x FROM m1.t ORDER BY x")
	p.agreeExec("DETACH m1")

	// Re-ATTACH: store should be genuinely fresh, not persisted.
	p.agreeExec("ATTACH 'file:/cmp-memdb-shared-1?vfs=memdb' AS m1")
	p.agreeQuery("SELECT count(*) FROM m1.sqlite_master")
	p.agreeExec("DETACH m1")
}

// TestEngineAttachMemdbUnshared verifies that the unshared URI spelling
// routes to a private in-memory store like ':memory:'. PRAGMA m1.journal_mode
// is checked engine-side by TestAttachMemdbJournalModeReadsMemory, since
// attachPair cannot run a schema-qualified PRAGMA getter.
func TestEngineAttachMemdbUnshared(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH 'file:cmp-memdb-unshared-1?vfs=memdb' AS m1")
	p.agreeExec("CREATE TABLE m1.t(x)")
	p.agreeExec("INSERT INTO m1.t VALUES(7)")
	p.agreeQuery("SELECT x FROM m1.t")
	p.agreeExec("DETACH m1")
}

// TestEngineAttachMemdbSameSessionAlias verifies locking rules for two
// aliases of the same shared store in one session.
func TestEngineAttachMemdbSameSessionAlias(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH 'file:/cmp-memdb-alias-1?vfs=memdb' AS m1")
	p.agreeExec("ATTACH 'file:/cmp-memdb-alias-1?vfs=memdb' AS m2")
	p.agreeExec("CREATE TABLE m1.t(a)")
	p.agreeExec("INSERT INTO m1.t VALUES(1)")
	p.agreeQuery("SELECT count(*) FROM m2.t")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO m1.t VALUES(2)")
	p.agreeQuery("SELECT count(*) FROM m2.t")
	p.agreeExec("ROLLBACK")

	p.agreeExec("BEGIN")
	p.agreeQuery("SELECT count(*) FROM m2.t")
	p.agreeExec("INSERT INTO m1.t VALUES(2)")
	p.agreeQuery("SELECT count(*) FROM m2.t")
	p.agreeQuery("SELECT count(*) FROM m1.t")
	p.agreeExec("COMMIT")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT count(*) FROM m1.t")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO m1.t VALUES(3)")
	p.agreeExec("INSERT INTO m2.t VALUES(4)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT count(*) FROM m1.t")
}

// TestEngineAttachFileAliasTransaction verifies locking rules for two
// aliases of the same file.
func TestEngineAttachFileAliasTransaction(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t(a)", "INSERT INTO t VALUES(1)")
	p := newAttachPair(t, []string{goAux, goAux}, []string{cgoAux, cgoAux})
	p.agreeExec("ATTACH '{0}' AS a1")
	p.agreeExec("ATTACH '{1}' AS a2")
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO a1.t VALUES(2)")
	p.agreeQuery("SELECT count(*) FROM a2.t")
	p.agreeQuery("SELECT count(*) FROM a1.t")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT count(*) FROM a2.t")
	p.agreeExec("ROLLBACK")
	p.agreeQuery("SELECT count(*) FROM a1.t")
	p.agreeQuery("SELECT count(*) FROM a2.t")

	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO a1.t VALUES(2)")
	p.agreeExec("COMMIT")
	p.agreeQuery("SELECT count(*) FROM a2.t")
}

// TestEngineAttachMemdbModeCombinationDeclines verifies that vfs=memdb
// with a mode= parameter declines cleanly.
func TestEngineAttachMemdbModeCombinationDeclines(t *testing.T) {
	p := newAttachPair(t, nil, nil)
	p.agreeExec("ATTACH 'file:/cmp-memdb-mode-1?vfs=memdb' AS m1")
	p.agreeExec("DETACH m1")
	p.declineExec("ATTACH 'file:/cmp-memdb-mode-1?vfs=memdb&mode=rw' AS m1")
}
