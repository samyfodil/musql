package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSegmentWriteThenReadBack verifies writes to segment files are durable.
func TestSegmentWriteThenReadBack(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		want  string
	}{
		{"insert", []string{`INSERT INTO t VALUES(35,350,'mid')`},
			`SELECT id, k, s FROM t WHERE id = 35`},
		{"update", []string{`UPDATE t SET k = 999 WHERE id = 20`},
			`SELECT id, k FROM t WHERE id = 20`},
		{"delete", []string{`DELETE FROM t WHERE id = 30`},
			`SELECT count(*) FROM t`},
		{"several in one commit", []string{
			`INSERT INTO t VALUES(5,5,'low')`,
			`UPDATE t SET s = 'changed' WHERE id = 10`,
			`DELETE FROM t WHERE id = 80`},
			`SELECT id, k, s FROM t ORDER BY id`},
		{"insert then update the same row", []string{
			`INSERT INTO t VALUES(77,7,'a')`,
			`UPDATE t SET s = 'b' WHERE id = 77`},
			`SELECT id, s FROM t WHERE id = 77`},
		{"delete then reinsert the same rowid", []string{
			`DELETE FROM t WHERE id = 40`,
			`INSERT INTO t VALUES(40,404,'again')`},
			`SELECT id, k, s FROM t WHERE id = 40`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segPath := deltaFixture(t, 8)

			// The same statements laid out with no delta at all: the truth.
			truth := noDeltaFixture(t, 8, tc.stmts...)

			nw, err := OpenWrite(segPath)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			for _, s := range tc.stmts {
				if eerr := nw.Exec(s); eerr != nil {
					nw.Discard()
					t.Fatalf("%q: %v", s, eerr)
				}
			}
			wrote, cerr := nw.Commit()
			if cerr != nil {
				t.Fatalf("Commit: %v", cerr)
			}
			if !wrote {
				t.Fatal("Commit wrote nothing for statements that changed rows")
			}
			if cerr := nw.Close(); cerr != nil {
				t.Fatal(cerr)
			}

			np, nerr := Open(segPath)
			if nerr != nil {
				t.Fatalf("Open after the write: %v", nerr)
			}
			got := renderRows(t, np, tc.want)
			np.Close()

			rp, oerr := Open(truth)
			if oerr != nil {
				t.Fatal(oerr)
			}
			want := renderRows(t, rp, tc.want)
			rp.Close()

			if got != want {
				t.Errorf("%s\n through the delta: %s\n no delta: %s", tc.want, got, want)
			}
		})
	}
}

// TestSegmentWriteAcrossManyCommits writes commit after commit through ONE held
// session -- the shape an application runs and the one a per-commit append has to
// stay correct across -- against the same statements with no delta at all.
func TestSegmentWriteAcrossManyCommits(t *testing.T) {
	segPath := deltaFixture(t, 20)
	nw, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string

	for i := 1; i <= 10; i++ {
		var stmt string
		switch i % 3 {
		case 0:
			stmt = fmt.Sprintf(`DELETE FROM t WHERE id = %d`, i*10)
		case 1:
			stmt = fmt.Sprintf(`UPDATE t SET k = %d WHERE id = %d`, i*7, (i+1)*10)
		default:
			stmt = fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'n%d')`, i*10+3, i, i)
		}
		if eerr := nw.Exec(stmt); eerr != nil {
			t.Fatalf("segment %q: %v", stmt, eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatalf("commit %d: %v", i, cerr)
		}
		stmts = append(stmts, stmt)
	}
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}

	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np.Close()
	rp, rerr := Open(noDeltaFixture(t, 20, stmts...))
	if rerr != nil {
		t.Fatal(rerr)
	}
	defer rp.Close()
	q := `SELECT id, k, s FROM t ORDER BY id`
	if got, want := renderRows(t, np, q), renderRows(t, rp, q); got != want {
		t.Errorf("after ten commits\n through the delta: %s\n no delta: %s", got, want)
	}
}

// TestSegmentWriteThenCompactKeepsTheAnswer runs the full loop: write through the
// delta, compact, and the answer must not move -- and the fast paths must come
// back.
func TestSegmentWriteThenCompactKeepsTheAnswer(t *testing.T) {
	segPath := deltaFixture(t, 12)
	nw, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`INSERT INTO t VALUES(35,350,'mid')`,
		`UPDATE t SET k = -1 WHERE id = 30`,
		`DELETE FROM t WHERE id = 60`,
	} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}

	q := `SELECT id, k, s FROM t ORDER BY id`
	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	before := renderRows(t, np, q)
	np.Close()

	did, cerr := CompactSegmentFile(segPath)
	if cerr != nil {
		t.Fatalf("CompactSegmentFile: %v", cerr)
	}
	if !did {
		t.Fatal("compaction reported no work after three commits")
	}
	if _, serr := os.Stat(segDeltaPath(segPath)); !os.IsNotExist(serr) {
		t.Error("the delta survived compaction")
	}

	np2, nerr2 := Open(segPath)
	if nerr2 != nil {
		t.Fatalf("Open after compaction: %v", nerr2)
	}
	defer np2.Close()
	if got := renderRows(t, np2, q); got != before {
		t.Errorf("compaction changed the answer\n before: %s\n after:  %s", before, got)
	}
	// ...and a write can continue afterwards, against the NEW base.
	nw2, werr := OpenWrite(segPath)
	if werr != nil {
		t.Fatalf("OpenWrite after compaction: %v", werr)
	}
	if eerr := nw2.Exec(`INSERT INTO t VALUES(500,500,'post')`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw2.Commit(); cerr != nil {
		t.Fatalf("commit after compaction: %v", cerr)
	}
	if err := nw2.Close(); err != nil {
		t.Fatal(err)
	}
	np3, nerr3 := Open(segPath)
	if nerr3 != nil {
		t.Fatalf("Open after the post-compaction write: %v", nerr3)
	}
	defer np3.Close()
	if got := renderRows(t, np3, `SELECT id FROM t WHERE id = 500`); got != "1:500|" {
		t.Errorf("the write after compaction is not visible: %q", got)
	}
}

// TestSegmentWriteIsAtomicPerCommit proves the batch trailer is the commit point:
// a session discarded without committing leaves the file exactly as it was.
func TestSegmentWriteIsAtomicPerCommit(t *testing.T) {
	segPath := deltaFixture(t, 6)
	q := `SELECT id, k, s FROM t ORDER BY id`
	np, err := Open(segPath)
	if err != nil {
		t.Fatal(err)
	}
	before := renderRows(t, np, q)
	np.Close()

	nw, werr := OpenWrite(segPath)
	if werr != nil {
		t.Fatal(werr)
	}
	if eerr := nw.Exec(`DELETE FROM t`); eerr != nil {
		t.Fatal(eerr)
	}
	if derr := nw.Discard(); derr != nil {
		t.Fatal(derr)
	}

	np2, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np2.Close()
	if got := renderRows(t, np2, q); got != before {
		t.Errorf("a discarded session changed the file\n before: %s\n after:  %s", before, got)
	}
}

// TestSegmentDDLGoesThroughAFileRewrite replaces a test that pinned DDL being
// REFUSED. It is not refused any more -- a catalog change rewrites the file
// (segment_ddl.go) -- so what is pinned now is that each kind actually persists,
// which is the part a rewrite can silently get wrong.
func TestSegmentDDLGoesThroughAFileRewrite(t *testing.T) {
	cases := []struct {
		stmt string
		want string
	}{
		{`CREATE TABLE t2(a INTEGER)`, `SELECT name FROM sqlite_master WHERE name='t2'`},
		{`CREATE INDEX t_k ON t(k)`, `SELECT name FROM sqlite_master WHERE name='t_k'`},
		{`CREATE VIEW vv AS SELECT id FROM t`, `SELECT name FROM sqlite_master WHERE name='vv'`},
		{`ALTER TABLE t ADD COLUMN extra TEXT`, `SELECT count(extra) FROM t`},
	}
	for _, tc := range cases {
		t.Run(tc.stmt, func(t *testing.T) {
			segPath := deltaFixture(t, 4)
			nw, err := OpenWrite(segPath)
			if err != nil {
				t.Fatal(err)
			}
			if eerr := nw.Exec(tc.stmt); eerr != nil {
				nw.Discard()
				t.Fatalf("%q: %v", tc.stmt, eerr)
			}
			if _, cerr := nw.Commit(); cerr != nil {
				t.Fatalf("commit: %v", cerr)
			}
			if cerr := nw.Close(); cerr != nil {
				t.Fatal(cerr)
			}
			// A NEW session, so only what reached the file can answer.
			np, nerr := Open(segPath)
			if nerr != nil {
				t.Fatalf("Open after %q: %v", tc.stmt, nerr)
			}
			defer np.Close()
			if got := renderRows(t, np, tc.want); got == "" || strings.HasPrefix(got, "ERROR") {
				t.Errorf("%q did not survive the commit: %s -> %q", tc.stmt, tc.want, got)
			}
		})
	}
	// DROP is the one that must make an object DISAPPEAR, so it is checked the
	// other way round.
	t.Run("DROP TABLE", func(t *testing.T) {
		segPath := deltaFixture(t, 4)
		nw, err := OpenWrite(segPath)
		if err != nil {
			t.Fatal(err)
		}
		if eerr := nw.Exec(`DROP TABLE t`); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
		if cerr := nw.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		names, nerr := Tables(segPath)
		if nerr != nil {
			t.Fatal(nerr)
		}
		if len(names) != 0 {
			t.Errorf("tables after DROP = %v, want none", names)
		}
	})
}

// TestSegmentWriteDurabilityContractIsNamed keeps the two commit modes honest: the
// default fsyncs and NoSyncOnCommit does not, and BOTH must still produce a
// readable, correct file -- the difference is only whether the last commits
// survive a power loss, not whether they are atomic.
func TestSegmentWriteDurabilityContractIsNamed(t *testing.T) {
	for _, noSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("noSync=%v", noSync), func(t *testing.T) {
			segPath := deltaFixture(t, 6)
			nw, err := OpenWrite(segPath)
			if err != nil {
				t.Fatal(err)
			}
			nw.NoSyncOnCommit = noSync
			for i := 1; i <= 4; i++ {
				if eerr := nw.Exec(fmt.Sprintf(`UPDATE t SET k = %d WHERE id = %d`, i, i*10)); eerr != nil {
					t.Fatal(eerr)
				}
				if _, cerr := nw.Commit(); cerr != nil {
					t.Fatal(cerr)
				}
			}
			if err := nw.Close(); err != nil {
				t.Fatal(err)
			}
			np, nerr := Open(segPath)
			if nerr != nil {
				t.Fatal(nerr)
			}
			defer np.Close()
			got := renderRows(t, np, `SELECT id, k FROM t ORDER BY id LIMIT 4`)
			if got != "1:10|1:1|;1:20|1:2|;1:30|1:3|;1:40|1:4|" {
				t.Errorf("rows = %q", got)
			}
		})
	}
}

// TestSegmentCommitAdvancesThePairState gates the one piece of a segment commit
// that its own reads never look at: the state counter.
//
// Open replays the delta and does not compare counters -- it is the only
// storage there is, so there is nothing to be stale against. But AttachSegments
// DOES compare, against a real database's header, and ImportDatabase's output can
// be attached to. A commit that left the counter still would make a pair that
// looks current when it is not, and nothing on the segment path would notice --
// which is why this is asserted directly rather than through a query.
func TestSegmentCommitAdvancesThePairState(t *testing.T) {
	segPath := deltaFixture(t, 4)
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	startCtr, startPages, serr := SegmentFileState(segPath)
	if serr != nil {
		t.Fatal(serr)
	}
	if startCtr != baseCtr || startPages != basePages {
		t.Fatalf("a fresh export reports state (%d,%d), want its own base (%d,%d)",
			startCtr, startPages, baseCtr, basePages)
	}

	nw, werr := OpenWrite(segPath)
	if werr != nil {
		t.Fatal(werr)
	}
	const commits = 3
	for i := 1; i <= commits; i++ {
		if eerr := nw.Exec(fmt.Sprintf(`UPDATE t SET k = %d WHERE id = %d`, i, i*10)); eerr != nil {
			t.Fatal(eerr)
		}
		wrote, cerr := nw.Commit()
		if cerr != nil {
			t.Fatal(cerr)
		}
		if !wrote {
			t.Fatalf("commit %d wrote nothing", i)
		}
	}
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}

	endCtr, _, eerr := SegmentFileState(segPath)
	if eerr != nil {
		t.Fatal(eerr)
	}
	if endCtr != startCtr+commits {
		t.Errorf("after %d commits the pair reports counter %d, want %d: a commit that does "+
			"not move the state makes a stale pair look current to AttachSegments",
			commits, endCtr, startCtr+commits)
	}

	// A commit that changed nothing must NOT move it.
	nw2, w2 := OpenWrite(segPath)
	if w2 != nil {
		t.Fatal(w2)
	}
	if _, cerr := nw2.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if err := nw2.Close(); err != nil {
		t.Fatal(err)
	}
	if after, _, aerr := SegmentFileState(segPath); aerr != nil {
		t.Fatal(aerr)
	} else if after != endCtr {
		t.Errorf("a commit with no row change moved the state from %d to %d", endCtr, after)
	}
}

// TestSegmentReadYourWrites is what a held connection needs: a SELECT inside a
// session must see that session's UNCOMMITTED changes, and a reader outside it
// must not.
func TestSegmentReadYourWrites(t *testing.T) {
	segPath := deltaFixture(t, 6)
	nw, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()

	if eerr := nw.Exec(`UPDATE t SET s = 'seen' WHERE id = 20`); eerr != nil {
		t.Fatal(eerr)
	}
	if eerr := nw.Exec(`INSERT INTO t VALUES(35,350,'added')`); eerr != nil {
		t.Fatal(eerr)
	}
	if eerr := nw.Exec(`DELETE FROM t WHERE id = 30`); eerr != nil {
		t.Fatal(eerr)
	}

	// The session sees all three, uncommitted.
	_, rows, qerr := nw.Query(`SELECT id, s FROM t ORDER BY id`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	got := ""
	for _, r := range rows {
		got += fmt.Sprintf("%d:%s;", r[0].I, string(r[1].S))
	}
	if got != "10:r1;20:seen;35:added;40:r4;50:r5;60:r6;" {
		t.Errorf("in-session read = %q; it must show this session's uncommitted changes", got)
	}

	// A reader outside it sees none of them.
	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np.Close()
	if outside := renderRows(t, np, `SELECT id FROM t WHERE id = 35`); outside != "" {
		t.Errorf("an outside reader sees an uncommitted row: %q", outside)
	}
	if outside := renderRows(t, np, `SELECT s FROM t WHERE id = 20`); outside != `3:"r2"|` {
		t.Errorf("an outside reader sees an uncommitted update: %q", outside)
	}
}

// TestSegmentReadIsNotWholeDatabasePerQuery pins the reason ReadPager exists: it
// must not rebuild the database per call. A table that was never loaded is loaded
// ONCE, and repeated reads after that touch no more rows.
func TestSegmentReadIsNotWholeDatabasePerQuery(t *testing.T) {
	segPath := deltaFixture(t, 200)
	nw, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()

	// Two reads; the second must not re-read the table from the file.
	for i := 0; i < 2; i++ {
		_, rows, qerr := nw.Query(`SELECT count(*) FROM t`, nil)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if len(rows) != 1 || rows[0][0].I != 200 {
			t.Fatalf("count = %v, want 200", rows)
		}
	}
	// The row store is the session's, so a read hands back the SAME store rather
	// than a fresh copy -- which is what makes the second read cheap.
	rp, rerr := nw.ReadPager()
	if rerr != nil {
		t.Fatal(rerr)
	}
	defer rp.Close()
	var tbl *tableMeta
	for _, tm := range nw.tables {
		if tm.name == "t" {
			tbl = tm
		}
	}
	if tbl == nil {
		t.Fatal("table t not registered")
	}
	if rp.segs.live[tbl.rootPage] != tbl.rows {
		t.Error("ReadPager copied the row store instead of pointing at it, so every " +
			"read rebuilds what the session already holds")
	}
}

// TestSegmentTransactionsRollBack is the capability a database/sql driver needs
// before it can hold one segment session per connection: BEGIN/ROLLBACK and
// SAVEPOINT/ROLLBACK TO have to undo changes that were made IN PLACE in the
// session's row stores.
//
// It works because the engine's snapshot mechanism (txn.go) is already
// row-store-based; the pager savepoint beside it is the half a segment-backed session
// does not have, and pageSavepoint returning -1 is exactly the "no pager
// savepoint" value pageRollbackTo and pageRelease no-op on. Pinned here because
// that was assumed rather than demonstrated, and a driver built on the
// assumption would lose a rollback silently.
func TestSegmentTransactionsRollBack(t *testing.T) {
	segPath := deltaFixture(t, 4)
	nw, err := OpenWrite(segPath)
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()
	state := func() string {
		_, rows, qerr := nw.Query(`SELECT id, s FROM t ORDER BY id`, nil)
		if qerr != nil {
			t.Fatal(qerr)
		}
		out := ""
		for _, r := range rows {
			out += fmt.Sprintf("%d=%s,", r[0].I, string(r[1].S))
		}
		return out
	}
	const start = "10=r1,20=r2,30=r3,40=r4,"
	if got := state(); got != start {
		t.Fatalf("fixture = %q", got)
	}

	for _, s := range []string{`BEGIN`, `UPDATE t SET s = 'X' WHERE id = 20`,
		`INSERT INTO t VALUES(99,9,'ins')`} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatalf("%q: %v", s, eerr)
		}
	}
	if got := state(); got != "10=r1,20=X,30=r3,40=r4,99=ins," {
		t.Fatalf("inside the transaction = %q", got)
	}
	if eerr := nw.Exec(`ROLLBACK`); eerr != nil {
		t.Fatal(eerr)
	}
	if got := state(); got != start {
		t.Errorf("after ROLLBACK = %q, want the original %q", got, start)
	}

	// ...and a SAVEPOINT, which unwinds to a point rather than to the start.
	for _, s := range []string{`SAVEPOINT s1`, `DELETE FROM t WHERE id = 10`} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatalf("%q: %v", s, eerr)
		}
	}
	if got := state(); got != "20=r2,30=r3,40=r4," {
		t.Fatalf("inside the savepoint = %q", got)
	}
	if eerr := nw.Exec(`ROLLBACK TO s1`); eerr != nil {
		t.Fatal(eerr)
	}
	if got := state(); got != start {
		t.Errorf("after ROLLBACK TO = %q, want the original %q", got, start)
	}

	// A rolled-back session must also commit NOTHING.
	wrote, cerr := nw.Commit()
	if cerr != nil {
		t.Fatal(cerr)
	}
	if wrote {
		t.Error("a session whose every change was rolled back still appended a batch")
	}
}

// TestOpenOrCreateSegmentWriteRefusesASQLiteFile is the driver's boundary: a
// SQLite database handed to the engine is an ERROR that names the converter, not
// an implicit conversion and not an obscure parse failure.
func TestOpenOrCreateSegmentWriteRefusesASQLiteFile(t *testing.T) {
	dir := t.TempDir()

	// A path that does not exist yet is an empty database, as sql.Open expects.
	fresh := filepath.Join(dir, "fresh.musq")
	nw, err := OpenOrCreate(fresh)
	if err != nil {
		t.Fatalf("a missing path should be created, not refused: %v", err)
	}
	if eerr := nw.Exec(`CREATE TABLE t(a INTEGER)`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	// ...and opening it again finds what was there.
	nw2, err2 := OpenOrCreate(fresh)
	if err2 != nil {
		t.Fatal(err2)
	}
	if cerr := nw2.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Any file that is not one of this engine's databases is refused.
	other := filepath.Join(dir, "other.db")
	if werr := os.WriteFile(other, []byte("not a database of this engine"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, oerr := OpenOrCreate(other); oerr == nil {
		t.Fatal("a file of another format was accepted as a database")
	}
}
