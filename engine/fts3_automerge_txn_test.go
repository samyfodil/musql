package engine

import (
	"fmt"
	"strings"
	"testing"
)

func automergeTxnDB(t *testing.T, stmts ...string) *Session {
	t.Helper()
	db, err := Create(t.TempDir() + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

// fillLevel0 issues n autocommit INSERTs to a table, each writing one level-0
// segment; the 17th cascades others into level 1.
func fillLevel0(t *testing.T, db *Session, n int) {
	t.Helper()
	for i := range n {
		if err := db.Exec(fmt.Sprintf("INSERT INTO ft VALUES('w%d x')", i)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFts3AutomergeTxnAtLevelZeroIsServed verifies that in-transaction writes
// are served when all segments are at level 0 and no merge is possible.
func TestFts3AutomergeTxnAtLevelZeroIsServed(t *testing.T) {
	automergeTxnDB(t,
		"CREATE VIRTUAL TABLE vt0 USING fts5(c0)",
		"CREATE VIRTUAL TABLE vt1 USING fts4(c0)",
		"INSERT INTO vt1(c0) VALUES(0)",
		"BEGIN",
		"UPDATE vt1 SET c0 = 0",
		"INSERT INTO vt1(c0) VALUES (0), (0)",
		"UPDATE vt0 SET c0 = 0",
		"INSERT INTO vt1(c0) VALUES (0)",
		"UPDATE vt1 SET c0 = 0",
		"INSERT INTO vt1(vt1) VALUES('automerge=1')",
		"UPDATE vt1 SET c0 = 0",
		"DROP TABLE vt1",
		"SAVEPOINT x",
		"INSERT INTO vt0 VALUES('x')",
		"COMMIT",
	)
}

// TestFts3AutomergeTxnAboveLevelZeroStillDeclines verifies that in-transaction
// writes are declined when segments exist at level 1, but COMMIT succeeds.
func TestFts3AutomergeTxnAboveLevelZeroStillDeclines(t *testing.T) {
	db := automergeTxnDB(t, "CREATE VIRTUAL TABLE ft USING fts4(c)")
	fillLevel0(t, db, 17)
	for _, s := range []string{"INSERT INTO ft(ft) VALUES('automerge=1')", "BEGIN"} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Exec("INSERT INTO ft VALUES('e f')"); err == nil || !strings.Contains(err.Error(), "automerge enabled") {
		t.Fatalf("in-transaction write over a level-1 segment: err = %v, want the automerge decline", err)
	}
	if err := db.Exec("COMMIT"); err != nil {
		t.Errorf("COMMIT after a DECLINED write: %v, want success (nothing was written)", err)
	}
}

// TestFts3AutomergeTxnNearlyFullLevelZeroDeclines verifies that in-transaction
// writes are declined when nearly enough segments exist to trigger a cascade.
func TestFts3AutomergeTxnNearlyFullLevelZeroDeclines(t *testing.T) {
	db := automergeTxnDB(t, "CREATE VIRTUAL TABLE ft USING fts4(c)")
	fillLevel0(t, db, 14)
	for _, s := range []string{"INSERT INTO ft(ft) VALUES('automerge=1')", "BEGIN"} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Exec("INSERT INTO ft VALUES('e f')"); err == nil || !strings.Contains(err.Error(), "automerge enabled") {
		t.Fatalf("in-transaction write with 14 level-0 segments: err = %v, want the automerge decline", err)
	}
}

// TestFts3AutomergeCommitBackstopIsConservative verifies that COMMIT is
// declined when automerge might fire due to an in-transaction cascade, with
// the transaction remaining open for rollback.
func TestFts3AutomergeCommitBackstopIsConservative(t *testing.T) {
	db := automergeTxnDB(t, "CREATE VIRTUAL TABLE ft USING fts4(c)")
	fillLevel0(t, db, 15)
	for _, s := range []string{
		"BEGIN",
		"INSERT INTO ft VALUES('a1 b')",
		"SAVEPOINT s",
		"INSERT INTO ft VALUES('a2 b')",
		"INSERT INTO ft(ft) VALUES('automerge=1')",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	mx, _, ok := db.fts3TableLevelShape("ft")
	if !ok || mx < 1 {
		t.Skipf("fixture did not cascade into level 1 (mx=%d ok=%v); the backstop is not reached", mx, ok)
	}
	err := db.Exec("COMMIT")
	if err == nil || !strings.Contains(err.Error(), "cannot COMMIT") {
		t.Fatalf("COMMIT after an in-transaction cascade under automerge: err = %v, want the backstop decline", err)
	}
	if err := db.Exec("ROLLBACK"); err != nil {
		t.Errorf("ROLLBACK after the declined COMMIT: %v, want success (the transaction must still be open)", err)
	}
}
