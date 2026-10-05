// Tests virtual table write-path behavior: last_insert_rowid() after INSERT
// and OR IGNORE conflict handling, using rtree test cases.
package compat

import (
	"database/sql"
	"fmt"
	"testing"
)

// rtreeLastInsertRowid executes a statement and reads last_insert_rowid()
// via a follow-up SQL query.
func rtreeLastInsertRowid(t *testing.T, db *sql.DB, stmt string) (execErr error, rowid int64, queryErr error) {
	t.Helper()
	_, execErr = db.Exec(stmt)
	if execErr != nil {
		return execErr, 0, nil
	}
	row := db.QueryRow("SELECT last_insert_rowid()")
	queryErr = row.Scan(&rowid)
	return nil, rowid, queryErr
}

// TestRtreeLastInsertRowidAgreesWithOracle checks explicit and auto-assigned
// rowids in rtree INSERT operations.
func TestRtreeLastInsertRowidAgreesWithOracle(t *testing.T) {
	pure, mattn, _, _ := openPair(t, "rtreelastrowid")
	setup := "CREATE VIRTUAL TABLE t8 USING rtree(idx, x1, x2, y1, y2)"
	if _, err := pure.Exec(setup); err != nil {
		t.Fatalf("pure setup: %v", err)
	}
	if _, err := mattn.Exec(setup); err != nil {
		t.Fatalf("mattn setup: %v", err)
	}

	cases := []string{
		"INSERT INTO t8 VALUES(1, 1.0, 1.0, 2.0, 2.0)",    // explicit id
		"INSERT INTO t8 VALUES(NULL, 1.0, 1.0, 2.0, 2.0)", // auto-assigned
		"INSERT INTO t8 VALUES(50, 3.0, 3.0, 4.0, 4.0)",   // explicit, non-contiguous
		"INSERT INTO t8 VALUES(-7, 5.0, 5.0, 6.0, 6.0)",   // explicit, negative
	}
	for _, stmt := range cases {
		pErr, pRowid, pQerr := rtreeLastInsertRowid(t, pure, stmt)
		mErr, mRowid, mQerr := rtreeLastInsertRowid(t, mattn, stmt)
		if pErr != nil || mErr != nil {
			t.Fatalf("%q: pure err=%v mattn err=%v", stmt, pErr, mErr)
		}
		if pQerr != nil || mQerr != nil {
			t.Fatalf("%q: last_insert_rowid() query: pure err=%v mattn err=%v", stmt, pQerr, mQerr)
		}
		if pRowid != mRowid {
			t.Errorf("%q: last_insert_rowid() diverges: pure=%d mattn=%d", stmt, pRowid, mRowid)
		}
	}

	// Inside an explicit transaction: the value survives commit AND is
	// re-readable afterward (conn_state.go's markVtabInsertRowid explicitly
	// claims this; check it, not just the autocommit case above).
	if _, err := pure.Exec("BEGIN"); err != nil {
		t.Fatalf("pure BEGIN: %v", err)
	}
	if _, err := mattn.Exec("BEGIN"); err != nil {
		t.Fatalf("mattn BEGIN: %v", err)
	}
	for _, stmt := range []string{
		"INSERT INTO t8 VALUES(100, 7.0, 7.0, 8.0, 8.0)",
		"INSERT INTO t8 VALUES(101, 9.0, 9.0, 10.0, 10.0)",
	} {
		if _, err := pure.Exec(stmt); err != nil {
			t.Fatalf("pure %q: %v", stmt, err)
		}
		if _, err := mattn.Exec(stmt); err != nil {
			t.Fatalf("mattn %q: %v", stmt, err)
		}
	}
	if _, err := pure.Exec("COMMIT"); err != nil {
		t.Fatalf("pure COMMIT: %v", err)
	}
	if _, err := mattn.Exec("COMMIT"); err != nil {
		t.Fatalf("mattn COMMIT: %v", err)
	}
	var pRowid, mRowid int64
	if err := pure.QueryRow("SELECT last_insert_rowid()").Scan(&pRowid); err != nil {
		t.Fatalf("pure post-COMMIT last_insert_rowid(): %v", err)
	}
	if err := mattn.QueryRow("SELECT last_insert_rowid()").Scan(&mRowid); err != nil {
		t.Fatalf("mattn post-COMMIT last_insert_rowid(): %v", err)
	}
	if pRowid != mRowid {
		t.Errorf("post-COMMIT last_insert_rowid() diverges: pure=%d mattn=%d", pRowid, mRowid)
	}

	// An INSERT ... SELECT sourcing several rows: last_insert_rowid() is the
	// LAST one actually stored (conn_state.go's markVtabInsertRowid doc
	// comment), which only a multi-row source can distinguish from "the one
	// row this statement inserted".
	if _, err := pure.Exec("CREATE TABLE src(idx, x1, x2, y1, y2)"); err != nil {
		t.Fatalf("pure CREATE src: %v", err)
	}
	if _, err := mattn.Exec("CREATE TABLE src(idx, x1, x2, y1, y2)"); err != nil {
		t.Fatalf("mattn CREATE src: %v", err)
	}
	seed := "INSERT INTO src VALUES(200, 1,2,3,4), (201, 1,2,3,4), (202, 1,2,3,4)"
	if _, err := pure.Exec(seed); err != nil {
		t.Fatalf("pure seed src: %v", err)
	}
	if _, err := mattn.Exec(seed); err != nil {
		t.Fatalf("mattn seed src: %v", err)
	}
	pErr, pRowid, pQerr := rtreeLastInsertRowid(t, pure, "INSERT INTO t8 SELECT * FROM src")
	mErr, mRowid, mQerr := rtreeLastInsertRowid(t, mattn, "INSERT INTO t8 SELECT * FROM src")
	if pErr != nil || mErr != nil || pQerr != nil || mQerr != nil {
		t.Fatalf("INSERT...SELECT: pure(err=%v,qerr=%v) mattn(err=%v,qerr=%v)", pErr, pQerr, mErr, mQerr)
	}
	if pRowid != mRowid {
		t.Errorf("INSERT...SELECT last_insert_rowid() diverges: pure=%d mattn=%d", pRowid, mRowid)
	}
}

// TestRtreeOrIgnoreAgreesWithOracle is rtree1.test's section 12 (12.1/12.3's
// IGNORE rows): both the DUPLICATE-ID constraint and the COORDINATE (x1<=x2)
// constraint are OE_Ignore-eligible per rtree.c's single
// rtreeConstraintError helper (used for both), so both are covered here for
// INSERT and UPDATE, single-row and multi-row (INSERT...SELECT / UPDATE
// matching several rows where only some conflict).
func TestRtreeOrIgnoreAgreesWithOracle(t *testing.T) {
	fixture := func(t *testing.T, db *sql.DB) {
		t.Helper()
		stmts := []string{
			"CREATE VIRTUAL TABLE t1 USING rtree_i32(idx, x1, x2, y1, y2)",
			"INSERT INTO t1 VALUES(1, 1, 2, 3, 4)",
			"INSERT INTO t1 VALUES(2, 2, 3, 4, 5)",
			"INSERT INTO t1 VALUES(3, 3, 4, 5, 6)",
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("fixture %q: %v", s, err)
			}
		}
	}

	agree := func(t *testing.T, pure, mattn *sql.DB, stmt string) {
		t.Helper()
		pRes, pErr := pure.Exec(stmt)
		mRes, mErr := mattn.Exec(stmt)
		if (pErr == nil) != (mErr == nil) {
			t.Fatalf("%q: error-vs-success diverges: pure=%v mattn=%v", stmt, pErr, mErr)
		}
		if pErr != nil {
			return // both declined/rejected identically
		}
		pRA, _ := pRes.RowsAffected()
		mRA, _ := mRes.RowsAffected()
		if pRA != mRA {
			t.Errorf("%q: RowsAffected diverges: pure=%d mattn=%d", stmt, pRA, mRA)
		}
	}
	agreeQuery := func(t *testing.T, pure, mattn *sql.DB, q string) {
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

	t.Run("insert-duplicate-id", func(t *testing.T) {
		pure, mattn, _, _ := openPair(t, "rtreeignore1")
		fixture(t, pure)
		fixture(t, mattn)
		agree(t, pure, mattn, "INSERT OR IGNORE INTO t1 VALUES(2, 7, 7, 7, 7)")
		agreeQuery(t, pure, mattn, "SELECT * FROM t1 ORDER BY idx")
	})

	t.Run("insert-select-mixed-conflict", func(t *testing.T) {
		pure, mattn, _, _ := openPair(t, "rtreeignore2")
		fixture(t, pure)
		fixture(t, mattn)
		for _, db := range []*sql.DB{pure, mattn} {
			if _, err := db.Exec("CREATE TABLE source(idx, x1, x2, y1, y2)"); err != nil {
				t.Fatalf("CREATE source: %v", err)
			}
			// idx=5 is fresh (no conflict); idx=2 collides with the fixture.
			if _, err := db.Exec("INSERT INTO source VALUES(5, 8, 8, 8, 8), (2, 7, 7, 7, 7)"); err != nil {
				t.Fatalf("seed source: %v", err)
			}
		}
		agree(t, pure, mattn, "INSERT OR IGNORE INTO t1 SELECT * FROM source")
		agreeQuery(t, pure, mattn, "SELECT * FROM t1 ORDER BY idx")
	})

	t.Run("insert-coordinate-constraint", func(t *testing.T) {
		pure, mattn, _, _ := openPair(t, "rtreeignore3")
		fixture(t, pure)
		fixture(t, mattn)
		// x1(7) > x2(6): the OTHER rtreeConstraintError arm.
		agree(t, pure, mattn, "INSERT OR IGNORE INTO t1 VALUES(9, 7, 6, 7, 7)")
		agreeQuery(t, pure, mattn, "SELECT * FROM t1 ORDER BY idx")
	})

	t.Run("update-duplicate-id-single-row", func(t *testing.T) {
		pure, mattn, _, _ := openPair(t, "rtreeignore4")
		fixture(t, pure)
		fixture(t, mattn)
		agree(t, pure, mattn, "UPDATE OR IGNORE t1 SET idx = 2 WHERE idx = 3")
		agreeQuery(t, pure, mattn, "SELECT * FROM t1 ORDER BY idx")
	})

	t.Run("update-duplicate-id-multi-row-mixed", func(t *testing.T) {
		pure, mattn, _, _ := openPair(t, "rtreeignore5")
		fixture(t, pure)
		fixture(t, mattn)
		if _, err := pure.Exec("INSERT INTO t1 VALUES(4, 4, 5, 6, 7)"); err != nil {
			t.Fatalf("pure extra row: %v", err)
		}
		if _, err := mattn.Exec("INSERT INTO t1 VALUES(4, 4, 5, 6, 7)"); err != nil {
			t.Fatalf("mattn extra row: %v", err)
		}
		// Matches idx IN (3,4); moving 3->4 collides (4 already exists after
		// 3 does, in ascending WHERE-match order) while 4->... only one of
		// the two actually applies -- exactly rtree1.test's 12.3.ignore
		// shape (partial application, RowsAffected must still agree).
		agree(t, pure, mattn, "UPDATE OR IGNORE t1 SET idx = ((idx+1)%5)+1 WHERE idx > 2")
		agreeQuery(t, pure, mattn, "SELECT * FROM t1 ORDER BY idx")
	})
}

// TestRtreeOrIgnoreReturningMatchesTheOracle is the excluded class THIS FILE's
// own probing found, not the scoping pass that designed this bucket: real
// SQLite's RETURNING clause is a synthesized AFTER trigger (trigger.c's
// codeReturningTrigger) that insert.c places straight after OP_VUpdate,
// UNCONDITIONALLY -- unlike the ordinary-table path
// (sqlite3GenerateConstraintChecks), a vtab's OP_VUpdate has no jump-to-
// endOfLoop on OE_Ignore (vdbe.c:8750-8759 rewrites rc and falls straight
// through to the next opcode). So RETURNING fires for EVERY source row of an
// INSERT OR IGNORE against a virtual table, INCLUDING one OE_Ignore silently
// dropped: verified against the oracle below, both idx=2 (which conflicts
// and is dropped) and idx=3 (which applies) appear in the RETURNING output.
// This write path used to capture only a row it actually applied, which would
// silently OMIT the ignored row -- a wrong answer, not merely an incomplete
// one -- so it declined the combination outright. The capture is a BEFORE
// trigger now (vtabCandidateRow, engine/vtab_write.go), running before the
// module write rather than after it, which is exactly what makes the ignored
// row's output available; the combination is SERVED and compared here.
func TestRtreeOrIgnoreReturningMatchesTheOracle(t *testing.T) {
	pure, mattn, _, _ := openPair(t, "rtreeignorereturning")
	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING rtree(idx, x1, x2, y1, y2)",
		"INSERT INTO t1 VALUES(1, 1, 2, 3, 4)",
		"INSERT INTO t1 VALUES(2, 2, 3, 4, 5)",
	}
	for _, db := range []*sql.DB{pure, mattn} {
		for _, s := range setup {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%q: %v", s, err)
			}
		}
	}
	q := "INSERT OR IGNORE INTO t1 VALUES(2, 9, 9, 9, 9), (3, 5, 6, 7, 8) RETURNING idx"

	read := func(db *sql.DB, who string) []int {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s %q: %v", who, q, err)
		}
		defer rows.Close()
		var got []int
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s scan: %v", who, err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s %q: %v", who, q, err)
		}
		return got
	}
	want := read(mattn, "mattn")
	if len(want) != 2 {
		t.Fatalf("test's own premise failed: mattn RETURNING produced %v, want two rows (one ignored, one applied)", want)
	}
	if got := read(pure, "musql"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("INSERT OR IGNORE ... RETURNING against a virtual table: musql %v, oracle %v", got, want)
	}

	// ...and the IGNOREd row really was dropped on both sides, which is what
	// makes the second returned row the interesting one.
	for _, p := range []struct {
		db  *sql.DB
		who string
	}{{pure, "musql"}, {mattn, "mattn"}} {
		var n int
		if err := p.db.QueryRow("SELECT count(*) FROM t1 WHERE idx=2 AND x1=2").Scan(&n); err != nil {
			t.Fatalf("%s count: %v", p.who, err)
		}
		if n != 1 {
			t.Errorf("%s: the ORIGINAL idx=2 row was not kept (count=%d)", p.who, n)
		}
	}
}

// TestRtreeOrReplaceFailAbortRollbackStillDecline is the excluded class this
// bucket's fix must NOT widen: every OTHER conflict action against a
// virtual table stays a clean decline (never a wrong write). REPLACE is a
// PER-MODULE decision in C SQLite (rtree.c:3191 checks
// sqlite3_vtab_on_conflict() itself and deletes-then-retries internally --
// not something OP_VUpdate's generic handling does), and
// FAIL/ABORT/ROLLBACK need whole/partial-statement rollback semantics this
// write path does not implement.
func TestRtreeOrReplaceFailAbortRollbackStillDecline(t *testing.T) {
	pure, _, _, _ := openPair(t, "rtreeexcluded")
	stmts := []string{
		"CREATE VIRTUAL TABLE t1 USING rtree(idx, x1, x2, y1, y2)",
		"INSERT INTO t1 VALUES(1, 1, 2, 3, 4)",
		"INSERT INTO t1 VALUES(2, 2, 3, 4, 5)",
	}
	for _, s := range stmts {
		if _, err := pure.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	declines := []string{
		"INSERT OR REPLACE INTO t1 VALUES(2, 7, 7, 7, 7)",
		"INSERT OR FAIL INTO t1 VALUES(2, 7, 7, 7, 7)",
		"INSERT OR ABORT INTO t1 VALUES(2, 7, 7, 7, 7)",
		"INSERT OR ROLLBACK INTO t1 VALUES(2, 7, 7, 7, 7)",
		"UPDATE OR REPLACE t1 SET idx = 2 WHERE idx = 1",
		"UPDATE OR FAIL t1 SET idx = 2 WHERE idx = 1",
		"UPDATE OR ABORT t1 SET idx = 2 WHERE idx = 1",
		"UPDATE OR ROLLBACK t1 SET idx = 2 WHERE idx = 1",
		"INSERT INTO t1 VALUES(3, 1, 2, 3, 4) ON CONFLICT(idx) DO NOTHING",
	}
	for _, stmt := range declines {
		if _, err := pure.Exec(stmt); err == nil {
			t.Errorf("%q: engine ACCEPTED a conflict action this write path does not implement (must decline, never approximate)", stmt)
		}
	}
	// The table must be untouched by every declined attempt above (a decline
	// that partially wrote would be a wrong answer, not merely incomplete).
	var count int
	if err := pure.QueryRow("SELECT count(*) FROM t1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("table row count changed despite every attempt declining: got %d, want 2", count)
	}
}

// TestRtreeLastInsertRowidStaysOpaqueWhenNothingApplied pins the ONE honest
// remaining gap this bucket leaves behind (see vtab_write.go's
// insertIntoVtab): when EVERY row of an INSERT is dropped by OR IGNORE, real
// SQLite leaves last_insert_rowid() at its PRE-statement value (verified
// against the oracle below), but this engine's markConnStateOpaque -- called
// unconditionally at insertIntoVtab's entry because total_changes() is
// genuinely unrecoverable vtab-internal noise on EVERY attempt, matched or
// not -- is only cleared by a row that actually succeeds. A statement where
// nothing does leaves last_insert_rowid() DECLINED rather than silently
// reporting the (correct, but uncomputed) unchanged value. Declining is safe
// (never wrong); this test exists so a future fix that closes the gap has
// something to flip from FAIL to PASS.
func TestRtreeLastInsertRowidStaysOpaqueWhenNothingApplied(t *testing.T) {
	pure, mattn, _, _ := openPair(t, "rtreeopaquegap")
	stmts := []string{
		"CREATE VIRTUAL TABLE t1 USING rtree(idx, x1, x2, y1, y2)",
		"INSERT INTO t1 VALUES(1, 1, 2, 3, 4)",
	}
	for _, s := range stmts {
		if _, err := pure.Exec(s); err != nil {
			t.Fatalf("pure fixture %q: %v", s, err)
		}
		if _, err := mattn.Exec(s); err != nil {
			t.Fatalf("mattn fixture %q: %v", s, err)
		}
	}
	var mattnBefore int64
	if err := mattn.QueryRow("SELECT last_insert_rowid()").Scan(&mattnBefore); err != nil {
		t.Fatalf("mattn pre-statement last_insert_rowid(): %v", err)
	}
	if mattnBefore != 1 {
		t.Fatalf("test's own premise failed: mattn last_insert_rowid()=%d, want 1", mattnBefore)
	}

	// Every row of this statement conflicts with idx=1: nothing is applied.
	if _, err := pure.Exec("INSERT OR IGNORE INTO t1 VALUES(1, 5, 6, 7, 8)"); err != nil {
		t.Fatalf("pure INSERT OR IGNORE (all-conflict): %v", err)
	}
	if _, err := mattn.Exec("INSERT OR IGNORE INTO t1 VALUES(1, 5, 6, 7, 8)"); err != nil {
		t.Fatalf("mattn INSERT OR IGNORE (all-conflict): %v", err)
	}

	var mattnAfter int64
	if err := mattn.QueryRow("SELECT last_insert_rowid()").Scan(&mattnAfter); err != nil {
		t.Fatalf("mattn post-statement last_insert_rowid(): %v", err)
	}
	if mattnAfter != mattnBefore {
		t.Fatalf("test's own premise failed: C SQLite moved last_insert_rowid() from %d to %d across an all-conflict OR IGNORE INSERT", mattnBefore, mattnAfter)
	}

	// This engine: currently DECLINES (opaque) rather than answering 1. If
	// this now returns 1 without error, the gap has been closed -- update
	// this test to assert agreement instead of a decline.
	err := pure.QueryRow("SELECT last_insert_rowid()").Scan(new(int64))
	if err == nil {
		t.Fatalf("known gap closed: last_insert_rowid() after an all-conflict OR IGNORE INSERT no longer declines -- update this test (and vtab_write.go's doc comment) to assert agreement with the oracle instead")
	}
}
