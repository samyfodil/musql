package engine

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// COMPACTION CARRIES AN UNTOUCHED SEGMENT OVER AS ITS BYTES, and rebuilds only
// what the delta touched (mergeSegmentsWithDelta). Two properties, both checked
// against a model of the table: the answer is the delta's, and every segment no
// log record falls in is byte-identical in the new file.
func TestCompactionCarriesUntouchedSegmentsVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	n := 3*segmentRows + 100 // three full segments and a short tail
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	model := map[int64]int64{}
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER)`,
		fmt.Sprintf(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<%d) INSERT INTO t SELECT x, x*10 FROM c`, n),
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	for i := int64(1); i <= int64(n); i++ {
		model[i] = i * 10
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	if e := sess.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := CompactSegmentFile(path); e != nil {
		t.Fatal(e)
	}

	segBytes := func() [][]byte {
		t.Helper()
		f, ferr := OpenSegmentFile(path)
		if ferr != nil {
			t.Fatal(ferr)
		}
		defer f.Close()
		var out [][]byte
		for _, tb := range f.Tables() {
			if tb.Name == "t" {
				for _, raw := range tb.segs {
					out = append(out, bytes.Clone(raw))
				}
			}
		}
		return out
	}
	round := func(stmts []string, apply func(), same, differ []int) (before, after [][]byte) {
		t.Helper()
		before = segBytes()
		s, oerr := OpenWrite(path)
		if oerr != nil {
			t.Fatal(oerr)
		}
		for _, q := range stmts {
			if e := s.Exec(q); e != nil {
				t.Fatalf("%s: %v", q, e)
			}
		}
		if _, e := s.Commit(); e != nil {
			t.Fatal(e)
		}
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
		apply()
		SegsCarriedForTest()
		if did, e := CompactSegmentFile(path); e != nil || !did {
			t.Fatalf("CompactSegmentFile: %v, %v", did, e)
		}
		if got := SegsCarriedForTest(); got != int64(len(same)) {
			t.Errorf("%v: %d segments carried over, want %d", stmts, got, len(same))
		}
		after = segBytes()
		for _, i := range same {
			if !bytes.Equal(before[i], after[i]) {
				t.Errorf("%v: segment %d was rebuilt; no delta record touches it", stmts, i)
			}
		}
		for _, i := range differ {
			if bytes.Equal(before[i], after[i]) {
				t.Errorf("%v: segment %d is unchanged; the delta touched it", stmts, i)
			}
		}
		s, oerr = OpenWrite(path)
		if oerr != nil {
			t.Fatal(oerr)
		}
		defer s.Discard()
		_, rows, qerr := s.Query(`SELECT id, v FROM t ORDER BY id`, nil)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if len(rows) != len(model) {
			t.Fatalf("%v: %d rows after compaction, want %d", stmts, len(rows), len(model))
		}
		for _, r := range rows {
			if want, ok := model[r[0].I]; !ok || r[1].I != want {
				t.Fatalf("%v: row %d = %d, want %d (present %v)", stmts, r[0].I, r[1].I, want, ok)
			}
		}
		return before, after
	}

	if got := len(segBytes()); got != 4 {
		t.Fatalf("the fixture has %d segments, want 4", got)
	}
	// An update inside segment 1 and an append past the short tail: 0 and 2 are
	// untouched and full, 1 is touched, and the tail absorbs the append.
	round([]string{
		`UPDATE t SET v = -1 WHERE id = 70000`,
		fmt.Sprintf(`INSERT INTO t VALUES(%d, 7)`, n+1000),
	}, func() {
		model[70000] = -1
		model[int64(n+1000)] = 7
	}, []int{0, 2}, []int{1, 3})
	// A delete inside segment 0 and a row before every segment: segment 0 is
	// touched by a removal alone, and the new first row merges into its rebuild
	// rather than becoming a one-row segment ahead of it.
	round([]string{
		`DELETE FROM t WHERE id = 5`,
		`INSERT INTO t VALUES(-3, 1)`,
	}, func() {
		delete(model, 5)
		model[-3] = 1
	}, []int{1, 2, 3}, []int{0})
	if got := len(segBytes()); got != 4 {
		t.Fatalf("%d segments after the second round, want 4", got)
	}
}
