//go:build sqlite_fts5

// Tests INSERT/UPDATE OR REPLACE against fts5 tables.
package compat

import (
	"database/sql"
	"testing"
)

// agreeFts5Query runs query on both engines and compares results.
func agreeFts5Query(t *testing.T, pure, mattn *sql.DB, q string) {
	t.Helper()
	pRows, pErr := pure.Query(q)
	if pErr != nil {
		t.Fatalf("pure query %q: %v", q, pErr)
	}
	defer pRows.Close()
	mRows, mErr := mattn.Query(q)
	if mErr != nil {
		t.Fatalf("mattn query %q: %v", q, mErr)
	}
	defer mRows.Close()
	pc, po := collectRows(t, pRows)
	mc, mo := collectRows(t, mRows)
	if ok, reason := queryResultsMatch(pc, po, mc, mo, true); !ok {
		t.Errorf("%q diverges: %s\n  pure:  %v\n  mattn: %v", q, reason, po, mo)
	}
}

// TestFts5OrReplaceInsertNormalCollisionAgreesWithOracle reproduces
// fts5fault6.test's own statements (1.0's ten plain INSERTs, then 1.2's
// REPLACE) verbatim: a plain NORMAL-content table's INSERT OR REPLACE onto a
// rowid that ALREADY holds a row -- fts5_main.c's fts5UpdateMethod INSERT
// branch (fts5_main.c:2047-2051), forgiven because replaceForgiven() holds
// for a default-content table.
func TestFts5OrReplaceInsertNormalCollisionAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replaceinsertnormal")
	setup := []string{
		"CREATE VIRTUAL TABLE tt USING fts5(a, b)",
		"INSERT INTO tt VALUES('c d c g g f', 'a a a d g a')",
		"INSERT INTO tt VALUES('c d g b f d', 'b g e c g c')",
		"INSERT INTO tt VALUES('c c f d e d', 'c e g d b c')",
		"INSERT INTO tt VALUES('e a f c e f', 'g b a c d g')",
		"INSERT INTO tt VALUES('c g f b b d', 'g c d c f g')",
		"INSERT INTO tt VALUES('d a g a b b', 'g c g g c e')",
		"INSERT INTO tt VALUES('e f a b c e', 'f d c d c c')",
		"INSERT INTO tt VALUES('e c a g c d', 'b b g f f b')",
		"INSERT INTO tt VALUES('g b d d e b', 'f f b d a c')",
		"INSERT INTO tt VALUES('e a d a e d', 'c e a e f g')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	// rowid 6 already holds ('d a g a b b', 'g c g g c e') from the setup
	// above -- a genuine collision REPLACE must displace.
	fts5RunBoth(t, pure, mattn, "REPLACE INTO tt(rowid, a, b) VALUES(6, 'x y z', 'l l l')")
	agreeFts5Query(t, pure, mattn, "SELECT rowid, a, b FROM tt ORDER BY rowid")
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM tt('x') ORDER BY rowid")
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM tt('d') ORDER BY rowid")
	// The mined statement itself is inside a fault-injection harness that
	// then runs 'integrity-check' -- reproduce that too, since a REPLACE that
	// left tokens/docsize inconsistent is exactly what integrity-check is
	// built to catch.
	fts5RunBoth(t, pure, mattn, "INSERT INTO tt(tt) VALUES('integrity-check')")
}

// TestFts5OrReplaceInsertExternalContentFreshRowidAgreesWithOracle
// reproduces fts5conflict.test 1.1's own statement: an external-content
// table's REPLACE INTO onto a FRESH rowid (no existing row, so REPLACE's own
// forgiveness never enters into it either way -- eConflict stays ABORT for
// external content, fts5_main.c:2003-2005, but nothing collides).
func TestFts5OrReplaceInsertExternalContentFreshRowidAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replaceextfresh")
	fts5RunBoth(t, pure, mattn, "CREATE TABLE t1(x INTEGER PRIMARY KEY, a, b)")
	fts5RunBoth(t, pure, mattn, "CREATE VIRTUAL TABLE ft USING fts5(a, b, content=t1, content_rowid=x)")
	fts5RunBoth(t, pure, mattn, "REPLACE INTO ft(rowid, a, b) VALUES(1, 'a b c', 'a b c')")
	fts5RunBoth(t, pure, mattn, "REPLACE INTO t1 VALUES(1, 'a b c', 'a b c')")
	fts5RunBoth(t, pure, mattn, "INSERT INTO ft(ft) VALUES('integrity-check')")
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM ft('a') ORDER BY rowid")
}

// TestFts5OrReplaceInsertContentlessDeleteAgreesWithOracle reproduces
// fts5contentless.test 5.5 and 5.6 verbatim: a contentless_delete=1 table's
// REPLACE INTO, once onto an EXISTING rowid (genuine collision, forgiven --
// bContentlessDelete is the OTHER half of replaceForgiven()'s OR) and once
// onto a FRESH one.
func TestFts5OrReplaceInsertContentlessDeleteAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replacecontentlessdelete")
	setup := []string{
		"CREATE VIRTUAL TABLE ft USING fts5(x, content='', contentless_delete=1)",
		"INSERT INTO ft(rowid, x) VALUES(1, 'one two three')",
		"INSERT INTO ft(rowid, x) VALUES(2, 'one two four')",
		"INSERT INTO ft(rowid, x) VALUES(3, 'one two five')",
		"INSERT INTO ft(rowid, x) VALUES(4, 'one two seven')",
		"INSERT INTO ft(rowid, x) VALUES(5, 'one two eight')",
		"DELETE FROM ft WHERE rowid=2",
		"UPDATE ft SET x='four six' WHERE rowid=3",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM ft('one') ORDER BY rowid")

	// 5.5: REPLACE onto rowid 3, which ALREADY holds 'four six' (no-op in
	// content, but a genuine collision the engine must still go through
	// DeleteRow+InsertRow for).
	fts5RunBoth(t, pure, mattn, "REPLACE INTO ft(rowid, x) VALUES(3, 'four six')")
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM ft('one') ORDER BY rowid")

	// 5.6: REPLACE onto a FRESH rowid 6.
	fts5RunBoth(t, pure, mattn, "REPLACE INTO ft(rowid, x) VALUES(6, 'one two eleven')")
	agreeFts5Query(t, pure, mattn, "SELECT rowid FROM ft('one') ORDER BY rowid")
}

// TestFts5OrReplaceUpdateSingleRowAgreesWithOracle reproduces
// fts5simple.test 13.1-13.3 verbatim, including the exact pinned result
// (13.2's comment: rowid 2's content moves onto rowid 3, which already
// existed -- fts5_main.c's fts5UpdateMethod, iOld!=iNew REPLACE branch,
// fts5_main.c:2072-2077).
func TestFts5OrReplaceUpdateSingleRowAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replaceupdatesingle")
	setup := []string{
		"CREATE VIRTUAL TABLE xy USING fts5(x)",
		"INSERT INTO xy(rowid, x) VALUES(1, '1 2 3')",
		"INSERT INTO xy(rowid, x) VALUES(2, '2 3 4')",
		"INSERT INTO xy(rowid, x) VALUES(3, '3 4 5')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	fts5RunBoth(t, pure, mattn, "UPDATE OR REPLACE xy SET rowid=3 WHERE rowid = 2")
	agreeFts5Query(t, pure, mattn, "SELECT rowid, x FROM xy ORDER BY rowid")
	fts5RunBoth(t, pure, mattn, "INSERT INTO xy(xy) VALUES('integrity-check')")

	// The oracle's own pinned result (fts5simple.test:316-323): rowid 1
	// unchanged, rowid 2 gone, rowid 3 now holds what WAS rowid 2's content.
	var count int
	if err := pure.QueryRow("SELECT count(*) FROM xy WHERE rowid=2").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("rowid 2 should be gone after the REPLACE-driven move, still has %d rows", count)
	}
	var x string
	if err := pure.QueryRow("SELECT x FROM xy WHERE rowid=3").Scan(&x); err != nil {
		t.Fatalf("rowid 3: %v", err)
	}
	if x != "2 3 4" {
		t.Fatalf("rowid 3 holds %q, want the displaced rowid 2's content %q", x, "2 3 4")
	}
}

// TestFts5OrReplaceUpdateMultiRowCascadeStaysDeclined pins the ONE hazard
// this bucket's design explicitly declines rather than approximates: a
// multi-row UPDATE OR REPLACE that changes more than one row's rowid in the
// SAME statement, which C fts5's per-row xUpdate resolves against
// whatever an EARLIER row of the statement already wrote (fts5_main.c's own
// table scan is sequential), while this write path snapshots every row's OLD
// state before any of them are applied (updateVtab's pends-then-apply
// structure) -- the identical hazard fts3_write.go's own UPDATE path already
// declines (fts3_write.go) for the same reason. Verified one-way:
// the oracle actually cascades all the way down to a single surviving row,
// and this engine must decline rather than reproduce a DIFFERENT (wrong)
// final state.
func TestFts5OrReplaceUpdateMultiRowCascadeStaysDeclined(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replacecascade")
	setup := []string{
		"CREATE VIRTUAL TABLE t USING fts5(x)",
		"INSERT INTO t(rowid, x) VALUES(1, 'aa')",
		"INSERT INTO t(rowid, x) VALUES(2, 'bb')",
		"INSERT INTO t(rowid, x) VALUES(3, 'cc')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	stmt := "UPDATE OR REPLACE t SET rowid=rowid+1"

	// The oracle: this engine's own premise (C fts5 cascades all three
	// rows down to a single surviving one).
	if _, err := mattn.Exec(stmt); err != nil {
		t.Fatalf("test's own premise failed: mattn %q: %v", stmt, err)
	}
	var mCount int
	if err := mattn.QueryRow("SELECT count(*) FROM t").Scan(&mCount); err != nil {
		t.Fatalf("mattn count: %v", err)
	}
	if mCount != 1 {
		t.Fatalf("test's own premise failed: mattn cascade left %d rows, want 1", mCount)
	}

	// This engine: must decline, and the table must be UNTOUCHED by the
	// declined attempt (a decline that partially wrote would be a wrong
	// answer, not merely incomplete).
	if _, err := pure.Exec(stmt); err == nil {
		t.Fatalf("engine ACCEPTED a multi-row UPDATE OR REPLACE that changes rowid across more than one pending row -- must decline (C fts5's cascade depends on per-row scan order this write path's pends-then-apply structure cannot reproduce)")
	}
	var pCount int
	if err := pure.QueryRow("SELECT count(*) FROM t").Scan(&pCount); err != nil {
		t.Fatalf("pure count: %v", err)
	}
	if pCount != 3 {
		t.Fatalf("table row count changed despite the declined attempt: got %d, want 3", pCount)
	}
}

// TestFts5OrReplaceReturningAgreesWithOracle checks the design's own
// unverified claim: unlike OR IGNORE (which can silently drop a source row
// that RETURNING must still report), OR REPLACE never drops a row -- it only
// possibly displaces a DIFFERENT existing one first -- so INSERT ... OR
// REPLACE ... RETURNING should need no special casing at all.
func TestFts5OrReplaceReturningAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5replacereturning")
	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(xyz)",
		"INSERT INTO t1(rowid, xyz) VALUES(1, 'one')",
		"INSERT INTO t1(rowid, xyz) VALUES(2, 'two')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	q := "INSERT OR REPLACE INTO t1(rowid, xyz) VALUES(2, 'new two'), (3, 'three') RETURNING rowid, xyz"
	agreeFts5Query(t, pure, mattn, q)
	agreeFts5Query(t, pure, mattn, "SELECT rowid, xyz FROM t1 ORDER BY rowid")
}

// TestFts5OrReplaceExternalContentUpdateStillDeclines is the excluded class
// this bucket's UPDATE fix must not widen: an external-content table's
// rowid-changing UPDATE OR REPLACE stays declined exactly as a plain UPDATE
// or an UPDATE OR IGNORE already does (TestFts5OrIgnoreExternalContentNever
// Forgiven) -- updateReplaceForgiven() is deliberately narrower than
// replaceForgiven() (excludes contentlessDelete too, see its doc comment),
// so this table never reaches UpdateRowReplace at all.
func TestFts5OrReplaceExternalContentUpdateStillDeclines(t *testing.T) {
	pure, _ := openFts5Pair(t, "fts5replaceextupdate")
	setup := []string{
		"CREATE TABLE tbl(a INTEGER PRIMARY KEY, b, c)",
		"CREATE VIRTUAL TABLE fts_idx USING fts5(b, c, content=tbl, content_rowid=a)",
		"INSERT INTO tbl VALUES(13, 'x y z', '1 2 3')",
		"INSERT INTO tbl VALUES(14, 'a b c', '4 5 6')",
		"INSERT INTO fts_idx(rowid, b, c) VALUES(13, 'x y z', '1 2 3')",
		"INSERT INTO fts_idx(rowid, b, c) VALUES(14, 'a b c', '4 5 6')",
	}
	for _, s := range setup {
		if _, err := pure.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	if _, err := pure.Exec("UPDATE OR REPLACE fts_idx SET rowid=13 WHERE rowid=14"); err == nil {
		t.Fatalf("engine ACCEPTED an external-content rowid-move UPDATE under OR REPLACE -- this shape must stay declined (fts5_main.c:2003-2005's eConflict gate excludes external content), not silently merge two documents under one rowid")
	}
}
