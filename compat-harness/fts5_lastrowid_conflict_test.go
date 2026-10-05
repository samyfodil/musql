//go:build sqlite_fts5

// Tests last_insert_rowid() behavior with fts5 tables under OR-IGNORE
// conflict resolution. OR IGNORE behaves differently depending on whether
// the fts5 table uses default content mode or external/contentless modes.
package compat

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// openFts5Pair opens one persistent connection per engine against separate
// files, so connection-scoped state (last_insert_rowid(), an explicit
// transaction) is tracked exactly like fts5conflict.test's own db handle,
// rather than fts5Exec/fts5Query's fresh-connection-per-call pattern (which
// would lose that state).
func openFts5Pair(t *testing.T, name string) (pure, mattn *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	var err error
	pure, err = sql.Open("sqlite", filepath.Join(dir, name+".pure.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	pure.SetMaxOpenConns(1)
	t.Cleanup(func() { pure.Close() })
	mattn, err = sql.Open("sqlite3", filepath.Join(dir, name+".mattn.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	mattn.SetMaxOpenConns(1)
	t.Cleanup(func() { mattn.Close() })
	return pure, mattn
}

func fts5RunBoth(t *testing.T, pure, mattn *sql.DB, stmt string) {
	t.Helper()
	if _, err := pure.Exec(stmt); err != nil {
		t.Fatalf("pure %q: %v", stmt, err)
	}
	if _, err := mattn.Exec(stmt); err != nil {
		t.Fatalf("mattn %q: %v", stmt, err)
	}
}

func fts5LastInsertRowid(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var rowid int64
	if err := db.QueryRow("SELECT last_insert_rowid()").Scan(&rowid); err != nil {
		t.Fatalf("last_insert_rowid(): %v", err)
	}
	return rowid
}

// TestFts5LastInsertRowidAgreesWithOracle reproduces fts5lastrowid.test's
// own statements (1.1 through 1.6: plain multi-row INSERTs, inside an
// explicit transaction, an explicit and NEGATIVE rowid, and two
// INSERT...SELECT spellings) directly against the live oracle.
func TestFts5LastInsertRowidAgreesWithOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5lastrowid")
	fts5RunBoth(t, pure, mattn, "CREATE VIRTUAL TABLE t1 USING fts5(str)")

	// 1.1: three plain inserts.
	for _, s := range []string{
		"INSERT INTO t1 VALUES('one string')",
		"INSERT INTO t1 VALUES('two string')",
		"INSERT INTO t1 VALUES('three string')",
	} {
		fts5RunBoth(t, pure, mattn, s)
	}
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.1: last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}

	// 1.2: the same, inside an explicit transaction -- the value must
	// survive COMMIT.
	fts5RunBoth(t, pure, mattn, "BEGIN")
	for _, s := range []string{
		"INSERT INTO t1 VALUES('one string')",
		"INSERT INTO t1 VALUES('two string')",
		"INSERT INTO t1 VALUES('three string')",
	} {
		fts5RunBoth(t, pure, mattn, s)
	}
	fts5RunBoth(t, pure, mattn, "COMMIT")
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.2: last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}

	// 1.3: an explicit NEGATIVE rowid.
	fts5RunBoth(t, pure, mattn, "INSERT INTO t1(rowid, str) VALUES(-22, 'some more text')")
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.3: last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}

	// 1.4: explicit rowids inside a transaction, read BOTH before and after
	// COMMIT (both must agree; the pre-COMMIT read exercises a live,
	// uncommitted mid-transaction value).
	fts5RunBoth(t, pure, mattn, "BEGIN")
	for _, s := range []string{
		"INSERT INTO t1(rowid, str) VALUES(45, 'some more text')",
		"INSERT INTO t1(rowid, str) VALUES(46, 'some more text')",
		"INSERT INTO t1(rowid, str) VALUES(222, 'some more text')",
	} {
		fts5RunBoth(t, pure, mattn, s)
	}
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.4 (pre-COMMIT): last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}
	fts5RunBoth(t, pure, mattn, "COMMIT")
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.4 (post-COMMIT): last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}

	// 1.5: INSERT ... SELECT sourcing several rows, no explicit rowid column.
	fts5RunBoth(t, pure, mattn, "CREATE TABLE x1(x)")
	fts5RunBoth(t, pure, mattn, "INSERT INTO x1 VALUES('john'), ('paul'), ('george'), ('ringo')")
	fts5RunBoth(t, pure, mattn, "INSERT INTO t1 SELECT x FROM x1")
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.5: last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}

	// 1.6: INSERT ... SELECT with an explicit rowid EXPRESSION column.
	fts5RunBoth(t, pure, mattn, "INSERT INTO t1(rowid, str) SELECT rowid+10, x FROM x1")
	if p, m := fts5LastInsertRowid(t, pure), fts5LastInsertRowid(t, mattn); p != m {
		t.Errorf("after 1.6: last_insert_rowid() diverges: pure=%d mattn=%d", p, m)
	}
}

// TestFts5OrIgnorePlainTableAgreesWithOracle reproduces fts5conflict.test's
// foreach_detail_mode OR-IGNORE block (3.1.1/3.1.2/3.1.3) against a PLAIN
// default-content table -- the one fts5 content mode that gets
// SQLITE_VTAB_CONSTRAINT_SUPPORT (fts5_main.c:445) and so is genuinely
// OE_Ignore-eligible.
func TestFts5OrIgnorePlainTableAgreesWithOracle(t *testing.T) {
	for _, detail := range []string{"full", "col", "none"} {
		t.Run("detail="+detail, func(t *testing.T) {
			pure, mattn := openFts5Pair(t, "fts5ignore-"+detail)
			fts5RunBoth(t, pure, mattn, "CREATE VIRTUAL TABLE t1 USING fts5(xyz, detail="+detail+")")
			fts5RunBoth(t, pure, mattn, "BEGIN")
			fts5RunBoth(t, pure, mattn, "INSERT INTO t1(rowid, xyz) VALUES(13, 'thirteen documents')")
			fts5RunBoth(t, pure, mattn, "INSERT INTO t1(rowid, xyz) VALUES(14, 'fourteen documents')")
			fts5RunBoth(t, pure, mattn, "INSERT INTO t1(rowid, xyz) VALUES(15, 'fifteen documents')")
			fts5RunBoth(t, pure, mattn, "COMMIT")

			agreeExecErr := func(stmt string) (pRA, mRA int64) {
				t.Helper()
				pRes, pErr := pure.Exec(stmt)
				mRes, mErr := mattn.Exec(stmt)
				if (pErr == nil) != (mErr == nil) {
					t.Fatalf("%q: error-vs-success diverges: pure=%v mattn=%v", stmt, pErr, mErr)
				}
				if pErr != nil {
					return -1, -1
				}
				pRA, _ = pRes.RowsAffected()
				mRA, _ = mRes.RowsAffected()
				if pRA != mRA {
					t.Errorf("%q: RowsAffected diverges: pure=%d mattn=%d", stmt, pRA, mRA)
				}
				return pRA, mRA
			}
			agreeRows := func(q string) {
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

			// 3.1.1: a single-row INSERT OR IGNORE colliding with an
			// existing rowid -- the row must be silently dropped.
			agreeExecErr("INSERT OR IGNORE INTO t1(rowid, xyz) VALUES(14, 'new text')")
			agreeRows("SELECT rowid, xyz FROM t1 ORDER BY rowid")

			// 3.1.2: UPDATE OR IGNORE moving a rowid onto an existing one.
			agreeExecErr("UPDATE OR IGNORE t1 SET rowid=13 WHERE rowid=15")
			agreeRows("SELECT rowid, xyz FROM t1 ORDER BY rowid")

			// 3.1.3: INSERT ... SELECT OR IGNORE, every row conflicting.
			agreeExecErr("INSERT OR IGNORE INTO t1(rowid, xyz) SELECT 13,'some text' UNION ALL SELECT 14,'some text' UNION ALL SELECT 15,'some text'")
			agreeRows("SELECT rowid, xyz FROM t1 ORDER BY rowid")
		})
	}
}

// TestFts5OrIgnoreReturningMatchesOracle is the fts5 sibling of
// vtab_lastrowid_conflict_test.go's TestRtreeOrIgnoreReturningStaysDeclined
// -- see that test's doc comment for the shared mechanism (RETURNING is a
// synthesized AFTER trigger insert.c places straight after OP_VUpdate,
// unconditionally; a vtab's OP_VUpdate has no jump-to-endOfLoop on
// OE_Ignore, so C SQLite's RETURNING fires for EVERY source row,
// including one OE_Ignore silently dropped).
//
// IT USED TO ASSERT A DECLINE, and it is an UPGRADE that it no longer can: the
// engine now answers this shape, and answers it identically to the oracle. So
// the assertion moved from "must be refused" to "must agree", which is the only
// form that can still catch a regression -- a decline assertion passes just as
// happily when the feature is broken as when it is absent.
//
// Measured on both engines at the commit that closed it:
//
//	RETURNING              mattn [2 3]      pure [2 3]
//	SELECT rowid ... after mattn [1 2 3]    pure [1 2 3]
//	xyz WHERE rowid=2      mattn "two"      pure "two"
//
// The last row is the one that matters and the easiest to get wrong: the
// IGNOREd row keeps its ORIGINAL text, while still being reported by RETURNING.
func TestFts5OrIgnoreReturningMatchesOracle(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5ignorereturning")
	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(xyz)",
		"INSERT INTO t1(rowid, xyz) VALUES(1, 'one')",
		"INSERT INTO t1(rowid, xyz) VALUES(2, 'two')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}
	q := "INSERT OR IGNORE INTO t1(rowid, xyz) VALUES(2, 'new two'), (3, 'three') RETURNING rowid"

	scan := func(db *sql.DB, q string) ([]int64, error) {
		rows, err := db.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}

	mGot, mErr := scan(mattn, q)
	if mErr != nil {
		t.Fatalf("test's own premise failed: mattn %q: %v", q, mErr)
	}
	if len(mGot) != 2 {
		t.Fatalf("test's own premise failed: mattn RETURNING produced %v, want two rows (one ignored, one applied)", mGot)
	}
	pGot, pErr := scan(pure, q)
	if pErr != nil {
		t.Fatalf("engine declined a shape it used to decline and now serves: %v", pErr)
	}
	if !slices.Equal(pGot, mGot) {
		t.Fatalf("RETURNING rowid: engine %v, oracle %v", pGot, mGot)
	}

	// The rows themselves, and the IGNOREd row's surviving text.
	for _, q := range []string{
		"SELECT rowid FROM t1 ORDER BY rowid",
	} {
		m, merr := scan(mattn, q)
		p, perr := scan(pure, q)
		if merr != nil || perr != nil {
			t.Fatalf("%s: mattn err %v, engine err %v", q, merr, perr)
		}
		if !slices.Equal(p, m) {
			t.Fatalf("%s: engine %v, oracle %v", q, p, m)
		}
	}
	var mTxt, pTxt string
	if err := mattn.QueryRow("SELECT xyz FROM t1 WHERE rowid=2").Scan(&mTxt); err != nil {
		t.Fatalf("mattn rowid=2: %v", err)
	}
	if err := pure.QueryRow("SELECT xyz FROM t1 WHERE rowid=2").Scan(&pTxt); err != nil {
		t.Fatalf("engine rowid=2: %v", err)
	}
	if pTxt != mTxt {
		t.Fatalf("the IGNOREd row's text: engine %q, oracle %q", pTxt, mTxt)
	}
}

// TestFts5OrIgnoreExternalContentNeverForgiven is the excluded class this
// bucket's design got WRONG on first pass and this file's own probing
// corrected: an EXTERNAL-CONTENT table (content=<table>) has no %_content of
// its own to raise a genuine constraint on -- verified against the oracle
// that moving its rowid onto an existing one, with or without OR IGNORE,
// raises NO error at all (C fts5 just re-indexes at the new rowid; there
// is nothing to collide with). This engine still declines that shape
// outright (a pre-existing, narrower-than-real decline this bucket does not
// widen), so the assertion here is simply that OR IGNORE does not turn it
// into a SILENT, WRONG "row skipped" -- the decline must stay a decline.
func TestFts5OrIgnoreExternalContentNeverForgiven(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5ignoreextcontent")
	setup := []string{
		"CREATE TABLE tbl(a INTEGER PRIMARY KEY, b, c)",
		"CREATE VIRTUAL TABLE fts_idx USING fts5(b, c, content=tbl, content_rowid=a)",
		"INSERT INTO tbl VALUES(13, 'x y z', '1 2 3')",
		"INSERT INTO tbl VALUES(14, 'a b c', '4 5 6')",
		"INSERT INTO fts_idx(rowid, b, c) VALUES(13, 'x y z', '1 2 3')",
		"INSERT INTO fts_idx(rowid, b, c) VALUES(14, 'a b c', '4 5 6')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}

	// The oracle: no error, under a plain UPDATE OR under OR IGNORE, moving
	// fts_idx's rowid onto one that already exists.
	if _, err := mattn.Exec("UPDATE fts_idx SET rowid=13 WHERE rowid=14"); err != nil {
		t.Fatalf("test's own premise failed: mattn plain UPDATE rowid-collision: %v", err)
	}
	// Reset for the OR IGNORE probe below.
	if _, err := mattn.Exec("UPDATE fts_idx SET rowid=14 WHERE rowid=13"); err != nil {
		t.Fatalf("mattn reset: %v", err)
	}
	if _, err := mattn.Exec("UPDATE OR IGNORE fts_idx SET rowid=13 WHERE rowid=14"); err != nil {
		t.Fatalf("test's own premise failed: mattn OR IGNORE rowid-collision: %v", err)
	}

	// This engine: still declines (does not silently apply, and -- the
	// property this bucket's fix must guarantee -- does not silently DROP
	// the row under OR IGNORE either).
	if _, err := pure.Exec("UPDATE fts_idx SET rowid=13 WHERE rowid=14"); err == nil {
		t.Fatalf("engine ACCEPTED a plain external-content rowid-move UPDATE this write path does not correctly express (must stay declined)")
	}
	if _, err := pure.Exec("UPDATE OR IGNORE fts_idx SET rowid=13 WHERE rowid=14"); err == nil {
		t.Fatalf("engine ACCEPTED an external-content rowid-move UPDATE under OR IGNORE -- this shape must stay declined, not silently become a dropped row (fts5's bConstraint is false for external-content: fts5_main.c:445)")
	}
}

// TestFts5OrIgnoreContentlessUnindexedNeverForgiven is the excluded class
// vtab_write.go's own doc comment names directly: a contentless_unindexed=1
// table DOES raise a genuine SQLITE_CONSTRAINT on a %_content rowid
// collision (its %_content really does hold UNINDEXED column values), but
// fts5_main.c:445 still refuses it SQLITE_VTAB_CONSTRAINT_SUPPORT (eContent
// is FTS5_CONTENT_UNINDEXED, never FTS5_CONTENT_NORMAL) -- so OR IGNORE must
// NOT forgive it either, unlike the plain-table case above. Verified
// directly against the oracle.
func TestFts5OrIgnoreContentlessUnindexedNeverForgiven(t *testing.T) {
	pure, mattn := openFts5Pair(t, "fts5ignorecontentlessunindexed")
	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(xyz, meta UNINDEXED, content='', contentless_delete=1, contentless_unindexed=1)",
		"INSERT INTO t1(rowid, xyz, meta) VALUES(13, 'thirteen documents', 'm13')",
		"INSERT INTO t1(rowid, xyz, meta) VALUES(14, 'fourteen documents', 'm14')",
	}
	for _, s := range setup {
		fts5RunBoth(t, pure, mattn, s)
	}

	// The oracle itself: a plain UPDATE moving the rowid onto an existing
	// one DOES raise a genuine constraint here (unlike the external-content
	// and plain contentless_delete cases), and OR IGNORE does NOT forgive it
	// -- both engines must agree the statement still fails.
	plain := "UPDATE t1 SET rowid=13, xyz=xyz, meta=meta WHERE rowid=14"
	ignore := "UPDATE OR IGNORE t1 SET rowid=13, xyz=xyz, meta=meta WHERE rowid=14"
	if _, err := mattn.Exec(plain); err == nil {
		t.Fatalf("test's own premise failed: mattn plain UPDATE rowid-collision on a contentless_unindexed=1 table did not error")
	}
	if _, err := mattn.Exec(ignore); err == nil {
		t.Fatalf("test's own premise failed: mattn OR IGNORE rowid-collision on a contentless_unindexed=1 table did not error -- fts5's own bConstraint gate would need re-checking")
	}

	if _, err := pure.Exec(plain); err == nil {
		t.Fatalf("engine ACCEPTED a plain UPDATE rowid-collision on a contentless_unindexed=1 table (must decline)")
	}
	if _, err := pure.Exec(ignore); err == nil {
		t.Fatalf("engine ACCEPTED an OR IGNORE rowid-collision on a contentless_unindexed=1 table -- must stay declined (C fts5 refuses OE_Ignore forgiveness here too), not silently drop the row")
	}
}

// TestFts5OrReplaceFailAbortRollbackStillDecline is the excluded class this
// bucket's fix must not widen: every OTHER conflict action, and UPSERT,
// against an fts5 table stays a clean decline. REPLACE itself is deliberately
// NOT in this list any more -- see TestFts5OrReplaceAgreesWithOracle below,
// which is what this bucket's fix flips it to (a genuine, oracle-matching
// success, not a decline) -- FAIL/ABORT/ROLLBACK/UPSERT are unaffected by
// this bucket and stay declined exactly as before.
func TestFts5OrReplaceFailAbortRollbackStillDecline(t *testing.T) {
	pure, _ := openFts5Pair(t, "fts5excluded")
	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING fts5(xyz)",
		"INSERT INTO t1(rowid, xyz) VALUES(1, 'one')",
		"INSERT INTO t1(rowid, xyz) VALUES(2, 'two')",
	}
	for _, s := range setup {
		if _, err := pure.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	declines := []string{
		"INSERT OR FAIL INTO t1(rowid, xyz) VALUES(2, 'new two')",
		"INSERT OR ABORT INTO t1(rowid, xyz) VALUES(2, 'new two')",
		"INSERT OR ROLLBACK INTO t1(rowid, xyz) VALUES(2, 'new two')",
		"UPDATE OR FAIL t1 SET rowid=2 WHERE rowid=1",
		"UPDATE OR ABORT t1 SET rowid=2 WHERE rowid=1",
		"UPDATE OR ROLLBACK t1 SET rowid=2 WHERE rowid=1",
		"INSERT INTO t1(rowid, xyz) VALUES(3, 'three') ON CONFLICT(rowid) DO NOTHING",
	}
	for _, stmt := range declines {
		if _, err := pure.Exec(stmt); err == nil {
			t.Errorf("%q: engine ACCEPTED a conflict action this write path does not implement (must decline, never approximate)", stmt)
		}
	}
	var count int
	if err := pure.QueryRow("SELECT count(*) FROM t1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("table row count changed despite every attempt declining: got %d, want 2", count)
	}
}
