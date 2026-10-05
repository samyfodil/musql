package engine

import (
	"fmt"
	"os"
	"testing"
)

// A database's delta, merged over its segments, must answer exactly what
// the same statements answer with no delta at all (noDeltaFixture). A delta
// paired with a segment file it was not built against is refused; that is
// TestCompactionIsCrashSafe's case, the state a half-finished compaction leaves.

// TestSegmentDeltaMergedReadMatchesNoDelta covers every kind of write.
func TestSegmentDeltaMergedReadMatchesNoDelta(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"insert past the end", []string{`INSERT INTO t VALUES(500,5000,'new')`}},
		// BETWEEN two segment rows, which is the merge's ordering branch.
		{"insert between two segment rows", []string{`INSERT INTO t VALUES(35,350,'mid')`}},
		{"insert before every segment row", []string{`INSERT INTO t VALUES(1,1,'first')`}},
		{"several inserts interleaved", []string{
			`INSERT INTO t VALUES(15,15,'a')`,
			`INSERT INTO t VALUES(45,45,'b')`,
			`INSERT INTO t VALUES(5,5,'c')`}},
		{"insert into a gap left by a delete", []string{`DELETE FROM t WHERE id = 40`, `INSERT INTO t VALUES(37,999,'refilled')`}},
		{"update in place", []string{`UPDATE t SET k = -1 WHERE id = 30`}},
		{"update many", []string{`UPDATE t SET s = 'x' WHERE id <= 50`}},
		{"delete one", []string{`DELETE FROM t WHERE id = 60`}},
		{"delete the first row", []string{`DELETE FROM t WHERE id = 10`}},
		{"delete the last row", []string{`DELETE FROM t WHERE id = 80`}},
		{"delete every row", []string{`DELETE FROM t`}},
		{"delete then insert lower", []string{
			`DELETE FROM t WHERE id = 50`,
			`INSERT INTO t VALUES(25,25,'lower')`}},
		{"insert then update then delete", []string{
			`INSERT INTO t VALUES(77,7,'a')`,
			`UPDATE t SET s = 'b' WHERE id = 77`,
			`DELETE FROM t WHERE id = 20`}},
		{"a row re-inserted under a deleted rowid", []string{
			`DELETE FROM t WHERE id = 50`,
			`INSERT INTO t VALUES(50,55,'again')`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderT(t, deltaFixture(t, 8, tc.stmts...))
			want := renderT(t, noDeltaFixture(t, 8, tc.stmts...))
			if got != want {
				t.Errorf("merged read disagrees with no delta\n no delta:\n%s\n segments+delta:\n%s", want, got)
			}
		})
	}
}

// TestSegmentDeltaAcrossManyCommits appends commit after commit, checking the
// merged answer after each -- so a bug that only appears once the log has
// several batches, or once a rowid is touched twice, is caught.
func TestSegmentDeltaAcrossManyCommits(t *testing.T) {
	segPath := deltaFixture(t, 30)
	var stmts []string
	for i := 1; i <= 12; i++ {
		var stmt string
		switch i % 3 {
		case 0:
			stmt = fmt.Sprintf(`DELETE FROM t WHERE id = %d`, i*10)
		case 1:
			stmt = fmt.Sprintf(`UPDATE t SET k = %d WHERE id = %d`, i*100, (i+2)*10)
		default:
			// An odd rowid, so it lands BETWEEN the fixture's multiples of ten.
			stmt = fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'c%d')`, i*10+3, i, i)
		}
		execDB(t, segPath, stmt)
		stmts = append(stmts, stmt)
		if got, want := renderT(t, segPath), renderT(t, noDeltaFixture(t, 30, stmts...)); got != want {
			t.Fatalf("after commit %d (%s) the merged read disagrees\n no delta:\n%s\n segments+delta:\n%s",
				i, stmt, want, got)
		}
	}
}

// TestSegmentDeltaRejectsATornTail proves the batch trailer is a commit marker:
// a partially written batch must be ignored, leaving the state of the last
// complete commit.
func TestSegmentDeltaRejectsATornTail(t *testing.T) {
	segPath := deltaFixture(t, 6, `UPDATE t SET k = 42 WHERE id = 20`)
	good := renderT(t, segPath)

	deltaPath := segDeltaPath(segPath)
	data, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	baseCtr, basePages, err := SegmentFileBase(segPath)
	if err != nil {
		t.Fatal(err)
	}
	// Half of a second batch: a plausible length prefix and nothing after it.
	torn := append(append([]byte(nil), data...), 0x40, 0x00, 0x00, 0x00, 0xde, 0xad)
	if werr := os.WriteFile(deltaPath, torn, 0o644); werr != nil {
		t.Fatal(werr)
	}
	st, rerr := replaySegDelta(torn, baseCtr, basePages)
	if rerr != nil {
		t.Fatalf("a torn tail should be ignored, not an error: %v", rerr)
	}
	if st.bytes != int64(len(data)) {
		t.Errorf("replay reached byte %d, want %d: the torn batch was not excluded", st.bytes, len(data))
	}
	if got := renderT(t, segPath); got != good {
		t.Errorf("a torn tail changed the answer\n before:\n%s\n after:\n%s", good, got)
	}

	// And a corrupted CHECKSUM inside a complete batch is excluded the same way.
	bad := append([]byte(nil), data...)
	bad[len(bad)-1] ^= 0xff
	st2, rerr2 := replaySegDelta(bad, baseCtr, basePages)
	if rerr2 != nil {
		t.Fatalf("a bad checksum should be ignored, not an error: %v", rerr2)
	}
	if st2.batches != 0 {
		t.Errorf("a batch with a broken checksum was replayed (%d batches)", st2.batches)
	}
}
