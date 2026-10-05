package engine

// Gates automerge=N's trigger, closing mined statements flagged by investigation.
import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fts3AutomergeSegdirLevels reads t_segdir's (level, idx) pairs ordered.
func fts3AutomergeSegdirLevels(t *testing.T, db *Session, table string) []string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(fmt.Sprintf(`SELECT level, idx FROM %s_segdir ORDER BY level, idx`, table), nil)
	if err != nil {
		t.Fatalf("reading %s_segdir: %v", table, err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("level=%d idx=%d", r[0].I, r[1].I))
	}
	return out
}

func fts3AutomergeStatRow2(t *testing.T, db *Session, table string) (present bool, val int64) {
	t.Helper()
	stat := db.findTableMeta(table + "_stat")
	if stat == nil {
		return false, 0
	}
	row, ok := stat.rows.get(fts3StatAutoincrmergeID)
	if !ok || len(row) < 2 {
		return false, 0
	}
	return true, row[1].I
}

// The two mined statements.

// TestFts3AutomergeMinedFts4Growth2Statement0: automerge=2 succeeds and clamps value in x1_stat.
func TestFts3AutomergeMinedFts4Growth2Statement0(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE x1 USING fts4`,
		`INSERT INTO x1(x1) VALUES('automerge=2')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	present, val := fts3AutomergeStatRow2(t, db, "x1")
	if !present || val != 2 {
		t.Errorf("x1_stat row id=2 = (present=%v, val=%d), want (true, 2)", present, val)
	}
}

// TestFts3AutomergeMinedFts3Fault2Statement8: automerge=8 creates t8_stat on demand, and
// automerge= is the command that creates it here (fts3Automerge's own
// !m.isFts4 branch), matching merge='s identical, already-tested behavior
// for the SAME reason (fts3DoIncrmerge's own sqlite3Fts3CreateStatTable).
func TestFts3AutomergeMinedFts3Fault2Statement8(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t8 USING fts3`,
		`INSERT INTO t8 VALUES('the quick brown fox')`,
		`INSERT INTO t8 VALUES('jumped over the')`,
		`INSERT INTO t8 VALUES('lazy dog')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if db.findTableMeta("t8_stat") != nil {
		t.Fatalf("t8_stat already exists before automerge=8 ran")
	}
	if err := db.Exec(`INSERT INTO t8(t8) VALUES('automerge=8')`); err != nil {
		t.Fatalf("automerge=8: %v", err)
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT name FROM sqlite_master WHERE name LIKE 't8%' ORDER BY name`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, string(r[0].S))
	}
	want := []string{"t8", "t8_content", "t8_segdir", "t8_segments", "t8_stat"}
	if len(names) != len(want) {
		t.Fatalf("sqlite_master names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("sqlite_master names = %v, want %v", names, want)
			break
		}
	}

	present, val := fts3AutomergeStatRow2(t, db, "t8")
	if !present || val != 8 {
		t.Errorf("t8_stat row id=2 = (present=%v, val=%d), want (true, 8)", present, val)
	}
}

// ---- automerge='s own clamp values (fts3_command.go header comment) ----

func TestFts3AutomergeCommandClampsValue(t *testing.T) {
	cases := []struct {
		cmd  string
		want int64
	}{
		{"automerge=4", 4},
		{"automerge=17", 8}, // > fts3MergeCount(16) clamps to 8
		{"automerge=1", 8},  // ==1 clamps to 8
		{"automerge=x", 0},  // non-numeric parses as 0 (fts3Getint, no remainder check)
		{"automerge=0", 0},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`CREATE VIRTUAL TABLE t USING fts4(a)`); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(fmt.Sprintf(`INSERT INTO t(t) VALUES('%s')`, c.cmd)); err != nil {
				t.Fatalf("%s: %v", c.cmd, err)
			}
			present, val := fts3AutomergeStatRow2(t, db, "t")
			if !present || val != c.want {
				t.Errorf("%s: t_stat row id=2 = (present=%v, val=%d), want (true, %d)", c.cmd, present, val, c.want)
			}
		})
	}
}

// ---- the automatic trigger itself, isolated from the ordinary write path ----

// fts3AutomergeMxProviderLevel is the ABSOLUTE level
// fts3AutomergeWriteRootSegment's own row lands at: relative level 5
// (1029%1024==5, so fts3SegdirMxLevel reads mxLevel=5) but OUTSIDE the
// 0..1023 block a merge of level-0's pair (outputting to level 1) checks for
// promotion candidates -- fts3IncrmergePromotionNeeded's own range is
// (outLevel, iLast] = (1, 1023] (fts3_incrmerge.go), and 1029 falls outside
// it. A row at plain level 5 would instead trip that guard and decline
// (verified: this test originally used level 5 and hit exactly that error),
// which is fts3IncrmergeRun's own CORRECT, already-tested behavior for a
// case C fts3 handles via promotion and this port declines -- not a
// defect in what this test means to exercise. Using 1029 isolates the
// TRIGGER decision (this file's own new code) from that separate, unrelated
// decline.
const fts3AutomergeMxProviderLevel = 1029

// fts3AutomergeWriteRootSegment hand-writes a single ROOT-ONLY %_segdir row
// (start_block=0, matching every segment small enough to fit one leaf --
// vtab_fts3.go's fts3StoreSegment) at an arbitrary level, purely so
// fts3SegdirMxLevel (fts3_automerge.go) sees a non-zero relative level.
// Nothing in fts3IncrmergeRun's own logic validates a level against the
// table's DECLARED index count (fts3SegdirRowsOf and
// fts3IncrmergePromotionNeeded are pure functions of the raw level numbers
// already in %_segdir), so a plain "fts4(a)" table with no "prefix=" or
// "languageid=" option tolerates a row at a level that would, on a real
// table, belong to a second index -- exactly like a hand-written shadow
// table anywhere else in this engine's own fts3 tests. Its root and
// end_block are the minimal valid shapes fts3SegdirRowsOf and
// fts3PromoteSegments already tolerate (fts3_merge.go, fts3_promote.go) --
// this row is never read as a real segment by anything in these tests.
func fts3AutomergeWriteRootSegment(db *Session, segdir *tableMeta, level, idx int64) {
	rowid, _ := nextRowidForTable(segdir)
	segdir.putRow(rowid, []Value{
		{Typ: Int, I: level},
		{Typ: Int, I: idx},
		{Typ: Int, I: 0},
		{Typ: Int, I: 0},
		{Typ: Text, S: []byte("0 1")},
		{Typ: Blob, S: []byte{0}},
	})
}

// TestFts3AutomergeTriggerBoundary calls fts3MaybeAutomerge directly
// (fts3_automerge.go) -- this file's own new decision logic in isolation
// from the ordinary write path -- at both sides of fts3.c:3558-3568's own
// A>64 boundary, and with automerge off, over a table already holding TWO
// small level-0 segments that fts3IncrmergeRun (fts3_incrmerge.go, already
// exhaustively tested on its own) can merge into one within a single leaf.
// The only thing under test here is WHETHER that merge gets invoked, and
// exactly at the boundary the C formula specifies -- the merge's own
// byte-exactness is fts3_incrmerge_test.go's job, reused unchanged.
func TestFts3AutomergeTriggerBoundary(t *testing.T) {
	newTable := func(t *testing.T) (*Session, *tableMeta, *tableMeta) {
		t.Helper()
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
				t.Fatal(err)
			}
		}
		segdir := db.findTableMeta("t_segdir")
		segments := db.findTableMeta("t_segments")
		if segdir == nil || segments == nil {
			t.Fatal("missing shadow tables")
		}
		if got := fts3AutomergeSegdirLevels(t, db, "t"); len(got) != 2 {
			t.Fatalf("before test, t_segdir = %v, want 2 rows", got)
		}
		return db, segdir, segments
	}
	sch := fts3Schema{}

	t.Run("automerge off: nothing happens regardless of nLeafAdd", func(t *testing.T) {
		db, segdir, segments := newTable(t)
		fts3AutomergeWriteRootSegment(db, segdir, fts3AutomergeMxProviderLevel, 0)
		before := fts3AutomergeSegdirLevels(t, db, "t")
		if err := db.fts3MaybeAutomerge("t", sch, segdir, segments, 0, 1000); err != nil {
			t.Fatalf("nAuto=0: %v", err)
		}
		if got := fts3AutomergeSegdirLevels(t, db, "t"); !equalStrSlices(got, before) {
			t.Errorf("t_segdir changed with automerge off: got %v, want unchanged %v", got, before)
		}
	})

	t.Run("nLeafAdd<=4: below fts3.c:3558's own gate, nothing happens", func(t *testing.T) {
		db, segdir, segments := newTable(t)
		fts3AutomergeWriteRootSegment(db, segdir, fts3AutomergeMxProviderLevel, 0)
		before := fts3AutomergeSegdirLevels(t, db, "t")
		if err := db.fts3MaybeAutomerge("t", sch, segdir, segments, 2, 4); err != nil {
			t.Fatalf("nLeafAdd=4: %v", err)
		}
		if got := fts3AutomergeSegdirLevels(t, db, "t"); !equalStrSlices(got, before) {
			t.Errorf("t_segdir changed at nLeafAdd=4 (the gate is nLeafAdd>4): got %v, want unchanged %v", got, before)
		}
	})

	t.Run("A<=64: mxLevel=5, nLeafAdd=8 -> A=60, nothing happens", func(t *testing.T) {
		db, segdir, segments := newTable(t)
		fts3AutomergeWriteRootSegment(db, segdir, fts3AutomergeMxProviderLevel, 0)
		before := fts3AutomergeSegdirLevels(t, db, "t")
		if err := db.fts3MaybeAutomerge("t", sch, segdir, segments, 2, 8); err != nil {
			t.Fatalf("nLeafAdd=8: %v", err)
		}
		if got := fts3AutomergeSegdirLevels(t, db, "t"); !equalStrSlices(got, before) {
			t.Errorf("t_segdir changed at A=60 (<=64, should not merge): got %v, want unchanged %v", got, before)
		}
	})

	t.Run("A>64: mxLevel=5, nLeafAdd=9 -> A=67.5, the two level-0 segments merge", func(t *testing.T) {
		db, segdir, segments := newTable(t)
		fts3AutomergeWriteRootSegment(db, segdir, fts3AutomergeMxProviderLevel, 0)
		if err := db.fts3MaybeAutomerge("t", sch, segdir, segments, 2, 9); err != nil {
			t.Fatalf("nLeafAdd=9: %v", err)
		}
		got := fts3AutomergeSegdirLevels(t, db, "t")
		want := []string{"level=1 idx=0", fmt.Sprintf("level=%d idx=0", fts3AutomergeMxProviderLevel)}
		if !equalStrSlices(got, want) {
			t.Errorf("t_segdir after A=67.5 (>64, should merge) = %v, want %v", got, want)
		}
		// The merge must not have changed a single answer MATCH gives.
		if got := fts3IncrmergeMatch(t, db, "alpha"); len(got) != 1 || got[0] != 1 {
			t.Errorf("MATCH 'alpha' after automerge = %v, want [1]", got)
		}
		if got := fts3IncrmergeMatch(t, db, "beta"); len(got) != 1 || got[0] != 2 {
			t.Errorf("MATCH 'beta' after automerge = %v, want [2]", got)
		}
	})
}

// equalStrSlices is defined in trigger_replace_delete_test.go, same package.

// ---- what stays declined (fts3_automerge.go's own "What remains declined") ----

// TestFts3AutomergeServedInsideExplicitTransactionAtLevelZero is what used to
// be TestFts3AutomergeDeclinesInsideExplicitTransaction, and it pinned the
// wrong side of the oracle: C SQLite ACCEPTS all three of these
// transactional writes and commits them. Every segment of t sits at relative
// level 0, so fts3SyncMethod's A = nLeafAdd*mxLevel is 0 (fts3.c:3566-3568)
// and the trigger cannot merge anything -- see fts3DeclineAutomergeInTxn.
// Measured against 3.53.3 on exactly this fixture, and this engine now writes
// the same bytes: the row (2,'x'), ONE %_segdir row (0,0,0,0,'0 7'), and
// MATCH 'x' -> 2. The shapes that DO stay declined (a segment above level 0,
// or a level 0 close to cascading) are pinned in fts3_automerge_txn_test.go.
func TestFts3AutomergeServedInsideExplicitTransactionAtLevelZero(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(a) VALUES('alpha')`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
		`BEGIN`,
		`INSERT INTO t(a) VALUES('beta')`,
		`DELETE FROM t WHERE docid=1`,
		`UPDATE t SET a='x' WHERE docid=2`,
		`COMMIT`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got, want := fts3AutomergeSegdirLevels(t, db, "t"), []string{"level=0 idx=0"}; !equalStrSlices(got, want) {
		t.Errorf("t_segdir after the committed transaction = %v, want the oracle's %v", got, want)
	}
	if got := fts3IncrmergeMatch(t, db, "x"); len(got) != 1 || got[0] != 2 {
		t.Errorf("MATCH 'x' = %v, want [2]", got)
	}

	// A table WITHOUT automerge is completely unaffected by this guard.
	if err := db.Exec(`CREATE VIRTUAL TABLE u USING fts4(a)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`BEGIN`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO u(a) VALUES('gamma')`); err != nil {
		t.Fatalf("ordinary transactional INSERT into a non-automerge table: %v", err)
	}
	if err := db.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
}

// TestFts3AutomergeDeclinesMultiFlushInsert pins that a single INSERT which
// itself splits into more than one %_segdir write (a multi-language INSERT
// into a "languageid=" table, fts3_langid.go/fts3_txn.go's "mid-flushed"
// chunks) declines cleanly while automerge is enabled, rather than only
// accounting for its FINAL chunk's own leaf count -- see
// fts3_automerge.go's own "What remains declined".
func TestFts3AutomergeDeclinesMultiFlushInsert(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE VIRTUAL TABLE t USING fts4(a, languageid="lid")`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := fts3AutomergeSegdirLevels(t, db, "t")

	// Two languages in one statement forces a mid-statement flush
	// (fts3_langid.go's own "iPrevLangid != iLangid" rule).
	err = db.Exec(`INSERT INTO t(a, lid) VALUES('zero zero', 0), ('one two', 1)`)
	if err == nil {
		t.Fatal("multi-language INSERT with automerge enabled: want an error, got none")
	}
	if got := fts3AutomergeSegdirLevels(t, db, "t"); !equalStrSlices(got, before) {
		t.Errorf("t_segdir changed by a DECLINED multi-flush INSERT: got %v, want unchanged %v", got, before)
	}

	// The identical statement succeeds fine WITHOUT automerge -- confirming
	// this decline is genuinely about the automerge combination, not a
	// regression in multi-language INSERT support itself.
	if err := db.Exec(`CREATE VIRTUAL TABLE u USING fts4(a, languageid="lid")`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO u(a, lid) VALUES('zero zero', 0), ('one two', 1)`); err != nil {
		t.Fatalf("multi-language INSERT without automerge: %v", err)
	}
}

// ---- an end-to-end, live-oracle-pinned growth scenario ----

// fts3AutomergeBigInsertTerms builds an INSERT with n rows of DISTINCT,
// non-prefix-compressible terms, so ITS OWN segment write spills into
// several leaf nodes -- what a C fts3 xSync's own nLeafAdd tally
// (ext/fts3/fts3_write.c:2320/2435) counts.
func fts3AutomergeBigInsertTerms(n int) (sqlText string, args []Value) {
	var vals []string
	for i := 0; i < n; i++ {
		vals = append(vals, "(?)")
		args = append(args, Value{Typ: Text, S: []byte(strings.Repeat(fmt.Sprintf("term%03d", i), 8))})
	}
	return "INSERT INTO t(a) VALUES" + strings.Join(vals, ","), args
}

// TestFts3AutomergeGrowthTriggersThenDeclinesCleanly is the live-oracle-
// verified end-to-end case: a table grown large enough (via ordinary DML
// alone, page_size=512 to keep the run fast) that fts3.c:3529's own trigger
// condition is crossed for real, with automerge on. Verified against
// mattn/go-sqlite3 3.53.3 (scratch probe, not left in the tree): C fts3
// does NOT merely skip the merge here -- it performs a REAL incremental
// merge that spills into the general appendable multi-leaf b-tree writer and
// leaves a resumable %_stat hint behind (fts3_incrmerge.go's own declined
// shapes), which this write path does not reproduce. So the triggering
// INSERT must fail CLEANLY here -- not silently skip the merge (which would
// leave %_segdir less merged than C fts3's own, the wrong answer
// fts3_command.go's original decline comment warned about) and not corrupt
// anything either: the table must remain fully readable, unchanged, and
// pass its own integrity-check afterward, exactly like every other decline
// in this write path.
func TestFts3AutomergeGrowthTriggersThenDeclinesCleanly(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	// page_size=512 keeps the run fast: fts3 sizes its leaves from it.
	if err := db.Exec(`PRAGMA page_size=512`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE VIRTUAL TABLE t USING fts4(a)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO t(t) VALUES('automerge=2')`); err != nil {
		t.Fatal(err)
	}
	// 16 same-shaped rounds build 16 level-0 segments; the 17th's own cascade
	// (fts3AllocateSegdirIdx, fts3_merge.go) merges them into one level-1
	// segment big enough to survive fts3PromoteSegments' own demotion check
	// against a same-sized new level-0 write (engine/fts3_promote.go) -- so
	// mxLevel stays 1 afterward, exactly as SQL_SELECT_MXLEVEL reads it in
	// C fts3 too (it runs AFTER the same promotion step, inside the same
	// sqlite3Fts3PendingTermsFlush call fts3SyncMethod makes first).
	for round := 0; round < 17; round++ {
		sqlText, args := fts3AutomergeBigInsertTerms(500)
		if _, _, err := db.ExecArgs(sqlText, args); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.QueryArgs(`SELECT count(*) FROM t`, nil)
	p.Close()
	if err != nil {
		t.Fatal(err)
	}
	beforeCount := rows[0][0].I

	// The 18th statement's own leaf count (nLeafAdd, no cascade of its own
	// needed) combined with mxLevel=1 crosses fts3.c:3566-3568's A>64 --
	// verified this actually triggers C fts3's own incremental merge
	// (scratch oracle probe): this write path's own fts3IncrmergeRun then
	// hits its already-established "spans more than one leaf node" decline
	// (fts3_incrmerge.go).
	sqlText, args := fts3AutomergeBigInsertTerms(350)
	n, _, err := db.ExecArgs(sqlText, args)
	if err == nil {
		t.Fatalf("18th INSERT (should trigger automerge into an unported merge shape): want an error, got n=%d", n)
	}
	if !strings.Contains(err.Error(), "spanning more than one leaf node") {
		t.Errorf("18th INSERT error = %q, want it to name the multi-leaf-spill decline (fts3_incrmerge.go)", err.Error())
	}

	// A declined statement leaves NOTHING behind: same row count, still
	// internally consistent.
	if err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		t.Fatalf("integrity-check after the declined 18th INSERT: %v", err)
	}
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows2, err := p2.QueryArgs(`SELECT count(*) FROM t`, nil)
	p2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := rows2[0][0].I; got != beforeCount {
		t.Errorf("row count after the declined 18th INSERT = %d, want unchanged %d", got, beforeCount)
	}
}
