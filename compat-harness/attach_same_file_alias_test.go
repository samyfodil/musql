// Test ATTACH of the same file under multiple aliases.
// Verify read-only PRAGMA operations and write access through different aliases work correctly.
package compat

import "testing"

// TestEngineAttachSameFileTwiceReadsAndWrites tests ATTACH of same file twice with reads/writes.
func TestEngineAttachSameFileTwiceReadsAndWrites(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("ATTACH '{0}' AS aux1")
	p.agreeExec("CREATE TABLE aux1.t1(a, b)")
	p.agreeExec("INSERT INTO aux1.t1 VALUES(1, 2)")
	p.agreeExec("ATTACH '{0}' AS aux2") // Second alias must successfully attach.
	p.agreeQuery("SELECT * FROM aux2.t1 ORDER BY a")

	// Verify second alias can see data written through first alias.
	goCols, goRows, goErr, _, _, _ := p.query("SELECT * FROM aux2.t1 ORDER BY a")
	if goErr != nil {
		t.Fatalf("SELECT * FROM aux2.t1: %v", goErr)
	}
	if len(goCols) != 2 || len(goRows) != 1 || goRows[0][0] != "I:1" || goRows[0][1] != "I:2" {
		t.Fatalf("aux2 did not see aux1's row: cols=%v rows=%v", goCols, goRows)
	}

	p.agreeExec("BEGIN")
	// The SAME alias writing again: not a conflict (it already holds the only
	// open write session on this file).
	p.agreeExec("INSERT INTO aux1.t1 VALUES(3, 4)")
	// The OTHER alias's write, while aux1's is still open: C SQLite's lock
	// ladder rejects this ("database is locked"); this engine declines it too.
	goErr2, cgoErr2 := p.exec("INSERT INTO aux2.t1 VALUES(5, 6)")
	if goErr2 == nil {
		t.Error("engine ACCEPTED a write through aux2 while aux1's write was still open -- C SQLite would reject this with \"database is locked\"")
	}
	if cgoErr2 == nil {
		t.Fatal("test premise broken: C SQLite accepted the interleaved aux2 write")
	}
	p.agreeExec("COMMIT")

	// Post-commit, aux2 must see BOTH rows -- the one from before it was even
	// attached, and the one aux1 wrote inside the just-committed transaction.
	p.agreeQuery("SELECT * FROM aux2.t1 ORDER BY a")
	goCols, goRows, goErr, _, _, _ = p.query("SELECT * FROM aux2.t1 ORDER BY a")
	if goErr != nil {
		t.Fatalf("post-commit SELECT * FROM aux2.t1: %v", goErr)
	}
	if len(goRows) != 2 || goRows[0][0] != "I:1" || goRows[1][0] != "I:3" {
		t.Fatalf("aux2 did not see both committed rows: cols=%v rows=%v", goCols, goRows)
	}
}

// TestEngineAttachRoutedReadOnlyPragmaDoesNotBlockReattach tests read-only PRAGMA doesn't block re-ATTACH.
func TestEngineAttachRoutedReadOnlyPragmaDoesNotBlockReattach(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t(x)", "INSERT INTO t VALUES(1)")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})

	p.agreeExec("ATTACH '{0}' AS t2")
	// Every routed form integrity_check/quick_check accepts, none of which
	// write anything real (pragma.go's execPragma "integrity_check",
	// "quick_check" cases: an Exec discards the row either way).
	p.agreeExec("PRAGMA t2.integrity_check")
	p.agreeExec("PRAGMA t2.quick_check")
	p.agreeExec("PRAGMA t2.integrity_check=t")
	p.agreeExec("PRAGMA t2.integrity_check=sqlite_schema")
	p.agreeExec("PRAGMA t2.integrity_check(5)")

	// The bug: this used to fail with "already written into as t2" even
	// though nothing above wrote a single byte.
	p.agreeExec("ATTACH '{0}' AS t3")

	// Confirm t3 really attached (not just that both sides silently agreed to
	// decline it) and reads t2's own table correctly.
	goCols, goRows, goErr, _, _, _ := p.query("SELECT x FROM t3.t")
	if goErr != nil {
		t.Fatalf("t3 did not actually attach: %v", goErr)
	}
	if len(goRows) != 1 || goRows[0][0] != "I:1" {
		t.Fatalf("t3.t did not read back t2's own row: cols=%v rows=%v", goCols, goRows)
	}
	p.agreeQuery("SELECT x FROM t3.t")

	// A GENUINE write is still tracked correctly: once one alias really
	// writes, the OTHER is declined while that write is still open
	// (mutualReject against the oracle's own lock semantics), matching
	// TestEngineAttachSameFileTwiceReadsAndWrites.
	p.agreeExec("BEGIN")
	p.agreeExec("INSERT INTO t2.t VALUES(2)")
	goErr, cgoErr := p.exec("INSERT INTO t3.t VALUES(3)")
	if goErr == nil {
		t.Error("engine ACCEPTED a write through t3 while t2's write was still open")
	}
	if cgoErr == nil {
		t.Fatal("test premise broken: C SQLite accepted the interleaved t3 write")
	}
	p.agreeExec("COMMIT")
}

// TestEngineAttachSameFileTwiceReaderStaysFresh tests that readers see sibling writes.
func TestEngineAttachSameFileTwiceReaderStaysFresh(t *testing.T) {
	for _, order := range []string{"reader-first", "writer-first"} {
		t.Run(order, func(t *testing.T) {
			goAux, cgoAux := buildAuxPair(t, "aux", "CREATE TABLE t1(a, b)", "INSERT INTO t1 VALUES(0, 0)")
			p := newAttachPair(t, []string{goAux}, []string{cgoAux})

			if order == "reader-first" {
				p.agreeExec("ATTACH '{0}' AS aux1")
				p.agreeExec("ATTACH '{0}' AS aux2")
			} else {
				p.agreeExec("ATTACH '{0}' AS aux2")
				p.agreeExec("ATTACH '{0}' AS aux1")
			}

			// Opens aux1's wdb for a pure read (Bug A's shape); must NOT mark
			// aux1 as having written.
			p.agreeExec("PRAGMA aux1.integrity_check")

			// A genuine write through the OTHER alias.
			p.agreeExec("INSERT INTO aux2.t1 VALUES(1, 2)")

			// The read-only-opened alias must see it.
			p.agreeQuery("SELECT a, b FROM aux1.t1 ORDER BY a")
			goCols, goRows, goErr, _, _, _ := p.query("SELECT a, b FROM aux1.t1 ORDER BY a")
			if goErr != nil {
				t.Fatalf("SELECT a, b FROM aux1.t1: %v", goErr)
			}
			if len(goCols) != 2 || len(goRows) != 2 || goRows[1][0] != "I:1" || goRows[1][1] != "I:2" {
				t.Fatalf("aux1 did not see aux2's write: cols=%v rows=%v", goCols, goRows)
			}
		})
	}
}
