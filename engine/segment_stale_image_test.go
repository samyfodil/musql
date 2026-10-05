package engine

import (
	"errors"
	"strings"
	"testing"
)

// A SEGMENT COMMIT MUST REFUSE A STALE IMAGE, exactly as the SQLite-format commit
// does (writer.go's "another connection committed while this transaction was
// open" -- doCommitWALLocked, writer.go).
//
// The segment delta is a log of ABSOLUTE puts and kills, keyed by (table, rowid).
// A record built from rows loaded before another connection committed is
// therefore not merely late, it is wrong about what it replaces: two replicated
// nodes lost a committed row to a materializeRow whose "INSERT OR REPLACE"
// appended "kill users/1, put users/3" from a stale image, while users/1 had
// meanwhile become a different row. Nothing refused it, so the row that had been
// committed was deleted by a statement that never saw it.
//
// Refusing with ErrBusy is what makes the driver's busyRetry re-run the statement
// against the other writer's state, which is the same recovery the SQLite path
// has always had.
func TestSegmentStaleImageCommitIsRefused(t *testing.T) {
	segPath := deltaFixture(t, 2)

	a, aerr := OpenWrite(segPath)
	if aerr != nil {
		t.Fatal(aerr)
	}
	defer a.Discard()
	// sqliteFileSession A loads rows, which is what makes its image a snapshot.
	if _, _, qerr := a.Query(`SELECT count(*) FROM t`, nil); qerr != nil {
		t.Fatal(qerr)
	}

	b, berr := OpenWrite(segPath)
	if berr != nil {
		t.Fatal(berr)
	}
	if eerr := b.Exec(`INSERT INTO t VALUES(99,99,'b')`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := b.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := b.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// A now writes from the image it loaded BEFORE that commit.
	if eerr := a.Exec(`INSERT INTO t VALUES(50,50,'a')`); eerr != nil {
		t.Fatal(eerr)
	}
	_, cerr := a.Commit()
	if cerr == nil {
		t.Fatal("A's commit was accepted on a stale image -- its records are absolute puts and kills against rows it never saw")
	}
	if !errors.Is(cerr, ErrBusy) {
		t.Fatalf("stale image must be ErrBusy (so busyRetry re-runs the statement), got %v", cerr)
	}
	if !strings.Contains(cerr.Error(), "another connection committed while this transaction was open") {
		t.Fatalf("stale image must carry the SQLite path's own message, got %v", cerr)
	}

	// ...and the recovery: refresh, re-run, commit. Nothing is lost either way.
	refreshed, rerr := a.RefreshIfStale()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !refreshed {
		t.Fatal("RefreshIfStale reported no rebuild after a commit it had just refused as stale")
	}
	if eerr := a.Exec(`INSERT INTO t VALUES(50,50,'a')`); eerr != nil {
		t.Fatalf("re-running the statement after the refresh: %v", eerr)
	}
	if _, cerr := a.Commit(); cerr != nil {
		t.Fatalf("commit after the refresh: %v", cerr)
	}
	_, rows, qerr := a.Query(`SELECT id FROM t ORDER BY id`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	got := ""
	for _, r := range rows {
		got += itoaForTest(r[0].I) + ";"
	}
	if got != "10;20;50;99;" {
		t.Fatalf("after the retry the table is %q -- want every row, B's included", got)
	}
}

// A SESSION THAT KEEPS ITS OWN COMMITS CURRENT must not be told it is stale: the
// refusal above is only correct if it fires on ANOTHER connection's commit and
// never on this one's, or every second statement of a single writer would answer
// busy.
func TestSegmentOwnCommitsAreNotStale(t *testing.T) {
	segPath := deltaFixture(t, 2)
	a, aerr := OpenWrite(segPath)
	if aerr != nil {
		t.Fatal(aerr)
	}
	defer a.Close()
	for i := 1; i <= 5; i++ {
		if eerr := a.Exec(`INSERT INTO t VALUES(` + itoaForTest(int64(100+i)) + `,1,'x')`); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := a.Commit(); cerr != nil {
			t.Fatalf("commit %d of this session's own run: %v", i, cerr)
		}
	}
}
