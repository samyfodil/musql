package engine

// FTS3 incremental merge tests verify that merge= commands correctly move
// segments between levels and preserve search results. Merges with non-canonical
// segment indices are accepted rather than declined.
import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fts3IncrmergeSegdirRows reads t_segdir into comparable strings ordered by level and idx.
func fts3IncrmergeSegdirRows(t *testing.T, db *Session) []string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT level, idx, start_block, leaves_end_block, typeof(end_block), end_block, quote(root) FROM t_segdir ORDER BY level, idx`, nil)
	if err != nil {
		t.Fatalf("reading t_segdir: %v", err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("level=%v idx=%v start=%v leavesEnd=%v end_block(%s)=%v root=%s", r[0].I, r[1].I, r[2].I, r[3].I, r[4].S, fts3IncrmergeEndBlockText(r[5]), r[6].S))
	}
	return out
}

// fts3IncrmergeEndBlockText renders end_block's value as a string.
func fts3IncrmergeEndBlockText(v Value) string {
	if v.Typ == Int {
		return fmt.Sprintf("%d", v.I)
	}
	return string(v.S)
}

func fts3IncrmergeSegmentsRows(t *testing.T, db *Session) []string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT blockid, block IS NULL, quote(block) FROM t_segments ORDER BY blockid`, nil)
	if err != nil {
		t.Fatalf("reading t_segments: %v", err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("blockid=%v isnull=%v block=%s", r[0].I, r[1].I != 0, r[2].S))
	}
	return out
}

func fts3IncrmergeMatch(t *testing.T, db *Session, term string) []int64 {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT docid FROM t WHERE t MATCH ? ORDER BY docid`, []Value{{Typ: Text, S: []byte(term)}})
	if err != nil {
		t.Fatalf("MATCH %q: %v", term, err)
	}
	var out []int64
	for _, r := range rows {
		out = append(out, r[0].I)
	}
	return out
}

// TestFts3IncrmergeMovesData checks that a single-leaf merge correctly moves
// segments between levels and reserves space with a marker block.
func TestFts3IncrmergeMovesData(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('alpha')`,
		`INSERT INTO t(a) VALUES('beta')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 2 {
		t.Fatalf("before merge, t_segdir = %v, want 2 rows", got)
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err != nil {
		t.Fatalf("merge=1000,2: %v", err)
	}

	segdir := fts3IncrmergeSegdirRows(t, db)
	want := []string{`level=1 idx=0 start=1 leavesEnd=1 end_block(text)=64 21 root=X'0101'`}
	if len(segdir) != 1 || segdir[0] != want[0] {
		t.Errorf("t_segdir after merge = %v, want %v", segdir, want)
	}

	segments := fts3IncrmergeSegmentsRows(t, db)
	wantSeg := []string{
		`blockid=1 isnull=false block=X'0005616C7068610301020000046265746103020200'`,
		`blockid=64 isnull=true block=NULL`,
	}
	if len(segments) != 2 || segments[0] != wantSeg[0] || segments[1] != wantSeg[1] {
		t.Errorf("t_segments after merge = %v, want %v", segments, wantSeg)
	}

	// Verify search results are unchanged.
	if got := fts3IncrmergeMatch(t, db, "alpha"); len(got) != 1 || got[0] != 1 {
		t.Errorf("MATCH 'alpha' after merge = %v, want [1]", got)
	}
	if got := fts3IncrmergeMatch(t, db, "beta"); len(got) != 1 || got[0] != 2 {
		t.Errorf("MATCH 'beta' after merge = %v, want [2]", got)
	}

	// A second merge should be a no-op.
	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err != nil {
		t.Fatalf("second merge=1000,2: %v", err)
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 1 || got[0] != want[0] {
		t.Errorf("t_segdir after a second (no-op) merge = %v, want unchanged %v", got, want)
	}
}

// TestFts3IncrmergeEmptyResultOrphansMarker checks that a merge whose output
// is entirely empty leaves an orphaned reservation marker and no segdir row.
func TestFts3IncrmergeEmptyResultOrphansMarker(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a, order=DESC)`,
		`INSERT INTO t(a) VALUES(0)`,
		`INSERT INTO t(a) VALUES(0)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 2 {
		t.Fatalf("before UPDATE, t_segdir = %v, want 2 rows", got)
	}

	if err := db.Exec(`UPDATE t SET a=NULL`); err != nil {
		t.Fatalf("UPDATE t SET a=NULL: %v", err)
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 4 {
		t.Fatalf("after UPDATE, t_segdir = %v, want 4 rows (2 original + 2 delete markers)", got)
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1,4')`); err != nil {
		t.Fatalf("merge=1,4 over an entirely-empty result: %v", err)
	}

	segdir := fts3IncrmergeSegdirRows(t, db)
	if len(segdir) != 0 {
		t.Errorf("t_segdir after an entirely-empty merge = %v, want ZERO rows", segdir)
	}
	segments := fts3IncrmergeSegmentsRows(t, db)
	want := []string{`blockid=128 isnull=true block=NULL`}
	if len(segments) != 1 || segments[0] != want[0] {
		t.Errorf("t_segments after an entirely-empty merge = %v, want %v (the orphaned reservation marker)", segments, want)
	}

	// Verify the table still passes integrity check.
	if err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Errorf("'integrity-check' after an entirely-empty merge: %v", err)
	}

	// A second merge should be a no-op.
	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1,4')`); err != nil {
		t.Fatalf("second merge=1,4: %v", err)
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 0 {
		t.Errorf("t_segdir after a second (no-op) merge = %v, want still ZERO rows", got)
	}
	if got := fts3IncrmergeSegmentsRows(t, db); len(got) != 1 || got[0] != want[0] {
		t.Errorf("t_segments after a second (no-op) merge = %v, want unchanged %v", got, want)
	}
}

// TestFts3IncrmergeCascades checks that a single merge command can cascade
// across multiple levels, with each level's output fed to the next.
func TestFts3IncrmergeCascades(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('m1')`,
		`INSERT INTO t(a) VALUES('m2')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
		`INSERT INTO t(a) VALUES('m3')`,
		`INSERT INTO t(a) VALUES('m4')`,
		`INSERT INTO t(t) VALUES('merge=1000,2')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	segdir := fts3IncrmergeSegdirRows(t, db)
	want := `level=2 idx=0 start=129 leavesEnd=129 end_block(text)=192 29 root=X'018101'`
	if len(segdir) != 1 || segdir[0] != want {
		t.Errorf("t_segdir after cascading merge = %v, want [%s]", segdir, want)
	}

	segments := fts3IncrmergeSegmentsRows(t, db)
	wantLeaf := `blockid=129 isnull=false block=X'00026D3103010200010132030202000101330303020001013403040200'`
	wantMarker := `blockid=192 isnull=true block=NULL`
	if len(segments) != 2 || segments[0] != wantLeaf || segments[1] != wantMarker {
		t.Errorf("t_segments after cascading merge = %v, want [%s %s]", segments, wantLeaf, wantMarker)
	}

	for docid, term := range map[int64]string{1: "m1", 2: "m2", 3: "m3", 4: "m4"} {
		got := fts3IncrmergeMatch(t, db, term)
		if len(got) != 1 || got[0] != docid {
			t.Errorf("MATCH %q after cascading merge = %v, want [%d]", term, got, docid)
		}
	}
}

// TestFts3IncrmergeNoOpStillCreatesStatTable checks that a no-op merge still
// creates the FTS3 statistics table.
func TestFts3IncrmergeNoOpStillCreatesStatTable(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts3(a)`,
		`INSERT INTO t(a) VALUES('solo')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3IncrmergeSegdirRows(t, db)

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err != nil {
		t.Fatalf("merge=1000,2: %v", err)
	}

	if got := fts3IncrmergeSegdirRows(t, db); len(got) != len(before) || got[0] != before[0] {
		t.Errorf("t_segdir changed by a no-op merge: got %v, want unchanged %v", got, before)
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT count(*) FROM sqlite_master WHERE name='t_stat'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][0].I != 1 {
		t.Errorf("t_stat not created by a no-op merge= on a bare fts3 table")
	}
}

// TestFts3IncrmergeDeclinesResumeHint checks that a merge declines when a
// resume hint row exists and leaves the segment directory unchanged.
func TestFts3IncrmergeDeclinesResumeHint(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('x')`,
		`INSERT INTO t(a) VALUES('y')`,
		`INSERT INTO t_stat VALUES(1, X'0102')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3IncrmergeSegdirRows(t, db)
	if len(before) != 2 {
		t.Fatalf("before merge, t_segdir = %v, want 2 rows", before)
	}

	err = db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`)
	if err == nil {
		t.Fatal("merge=1000,2 with a resume hint present: want an error, got none")
	}

	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 2 || got[0] != before[0] || got[1] != before[1] {
		t.Errorf("t_segdir changed by a DECLINED merge: got %v, want unchanged %v", got, before)
	}
}

// TestFts3IncrmergeCorrupt4NonCanonicalIdx checks that a merge works correctly
// with non-canonical segment indices and missing backing segments.
func TestFts3IncrmergeCorrupt4NonCanonicalIdx(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts3(a, b)`,
		`INSERT INTO t_segdir VALUES(0,2,1111,0,0,X'00')`,
		`INSERT INTO t_segdir VALUES(0,3,0,0,0,X'00013003010200')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=107,2')`); err != nil {
		t.Fatalf("merge=107,2 over a non-canonical-idx corrupt shape: %v", err)
	}

	segdir := fts3IncrmergeSegdirRows(t, db)
	wantSegdir := `level=2 idx=0 start=3 leavesEnd=3 end_block(text)=66 7 root=X'0103'`
	if len(segdir) != 1 || segdir[0] != wantSegdir {
		t.Errorf("t_segdir after merge=107,2 = %v, want [%s]", segdir, wantSegdir)
	}

	segments := fts3IncrmergeSegmentsRows(t, db)
	wantSegments := []string{
		`blockid=-35488 isnull=true block=NULL`,
		`blockid=-35487 isnull=true block=NULL`,
		`blockid=1 isnull=false block=X'00013003010200'`,
		`blockid=2 isnull=false block=X'00013003010200'`,
		`blockid=3 isnull=false block=X'00013003010200'`,
		`blockid=66 isnull=true block=NULL`,
	}
	if len(segments) != len(wantSegments) {
		t.Fatalf("t_segments after merge=107,2 = %v, want %v", segments, wantSegments)
	}
	for i := range wantSegments {
		if segments[i] != wantSegments[i] {
			t.Errorf("t_segments[%d] after merge=107,2 = %q, want %q", i, segments[i], wantSegments[i])
		}
	}

	// A second merge should be a no-op.
	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=107,2')`); err != nil {
		t.Fatalf("second merge=107,2: %v", err)
	}
	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 1 || got[0] != wantSegdir {
		t.Errorf("t_segdir after a second (no-op) merge = %v, want unchanged [%s]", got, wantSegdir)
	}
}

// TestFts3IncrmergeNonCanonicalIdxRealContent checks that a merge with
// non-canonical indices preserves search results and passes integrity checks.
func TestFts3IncrmergeNonCanonicalIdxRealContent(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('alpha')`,
		`INSERT INTO t(a) VALUES('beta')`,
		`UPDATE t_segdir SET idx=5 WHERE idx=0`,
		`UPDATE t_segdir SET idx=7 WHERE idx=1`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err != nil {
		t.Fatalf("merge=1000,2 over non-canonical idx 5,7: %v", err)
	}

	segdir := fts3IncrmergeSegdirRows(t, db)
	wantSegdir := `level=2 idx=0 start=129 leavesEnd=129 end_block(text)=192 21 root=X'018101'`
	if len(segdir) != 1 || segdir[0] != wantSegdir {
		t.Errorf("t_segdir after merge = %v, want [%s]", segdir, wantSegdir)
	}

	segments := fts3IncrmergeSegmentsRows(t, db)
	wantSegments := []string{
		`blockid=129 isnull=false block=X'0005616C7068610301020000046265746103020200'`,
		`blockid=192 isnull=true block=NULL`,
	}
	if len(segments) != len(wantSegments) || segments[0] != wantSegments[0] || segments[1] != wantSegments[1] {
		t.Errorf("t_segments after merge = %v, want %v", segments, wantSegments)
	}

	// Verify search results are correct after the merge.
	if got := fts3IncrmergeMatch(t, db, "alpha"); len(got) != 1 || got[0] != 1 {
		t.Errorf("MATCH 'alpha' after merge = %v, want [1]", got)
	}
	if got := fts3IncrmergeMatch(t, db, "beta"); len(got) != 1 || got[0] != 2 {
		t.Errorf("MATCH 'beta' after merge = %v, want [2]", got)
	}
	if err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Errorf("'integrity-check' after merge = %v, want no error", err)
	}
}

// TestFts3IncrmergeDeclinesMultiLeafSpill checks that a merge whose output
// would span multiple leaf nodes is declined and leaves the directory unchanged.
func TestFts3IncrmergeDeclinesMultiLeafSpill(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE VIRTUAL TABLE t USING fts4(a)`); err != nil {
		t.Fatal(err)
	}
	// Two inserts with enough distinct terms to exceed one leaf when merged.
	words := func(seed, n int) string {
		ws := make([]string, n)
		for i := range ws {
			rev := fmt.Sprintf("%04d", i)
			rev = string([]byte{rev[3], rev[2], rev[1], rev[0]})
			ws[i] = fmt.Sprintf("w%d%sxxxxxxxxxxxxxxxxxxxx", seed, rev)
		}
		return strings.Join(ws, " ")
	}
	for _, s := range []string{
		fmt.Sprintf(`INSERT INTO t(a) VALUES('%s')`, words(1, 300)),
		fmt.Sprintf(`INSERT INTO t(a) VALUES('%s')`, words(2, 300)),
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3IncrmergeSegdirRows(t, db)
	if len(before) != 2 {
		t.Fatalf("before merge, t_segdir = %v, want 2 rows", before)
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err == nil {
		t.Fatal("merge=1000,2 needing more than one leaf: want an error, got none")
	}

	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 2 || got[0] != before[0] || got[1] != before[1] {
		t.Errorf("t_segdir changed by a DECLINED merge: got %v, want unchanged %v", got, before)
	}
}

// TestFts3IncrmergeDeclinesPromotion checks that a merge declines when
// promotion (moving a segment to a higher level) would be needed.
func TestFts3IncrmergeDeclinesPromotion(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		// Placeholder segment at level 2 to trigger promotion.
		`INSERT INTO t_segdir VALUES(2, 0, 0, 0, 0, X'00')`,
		`INSERT INTO t(a) VALUES('p')`,
		`INSERT INTO t(a) VALUES('q')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3IncrmergeSegdirRows(t, db)
	if len(before) != 3 {
		t.Fatalf("before merge, t_segdir = %v, want 3 rows", before)
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err == nil {
		t.Fatal("merge=1000,2 that would need promotion: want an error, got none")
	}

	if got := fts3IncrmergeSegdirRows(t, db); len(got) != 3 || got[0] != before[0] || got[1] != before[1] || got[2] != before[2] {
		t.Errorf("t_segdir changed by a DECLINED merge: got %v, want unchanged %v", got, before)
	}
}

// TestFts3IncrmergeDeclinesAmbiguousLevelTie checks that a merge declines when
// multiple levels tie on merge selection criteria.
func TestFts3IncrmergeDeclinesAmbiguousLevelTie(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a, prefix="1,1")`,
		`INSERT INTO t(a) VALUES('cat')`,
		`INSERT INTO t(a) VALUES('dog')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3IncrmergeSegdirRows(t, db)
	if len(before) != 6 { // 2 rows each at level 0, 1024, 2048
		t.Fatalf("before merge, t_segdir = %v, want 6 rows (2 per index)", before)
	}

	if err := db.Exec(`INSERT INTO t(t) VALUES('merge=1000,2')`); err == nil {
		t.Fatal("merge=1000,2 over a tied level selection: want an error, got none")
	}

	got := fts3IncrmergeSegdirRows(t, db)
	if len(got) != len(before) {
		t.Errorf("t_segdir changed by a DECLINED merge: got %v, want unchanged %v", got, before)
	}
}
