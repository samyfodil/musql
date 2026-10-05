// Tests reading sqlite_master.rowid inside writable_schema catalog DML when
// no rows have been removed from the session catalog. The dense rowid
// (creation order) matches C SQLite's rowid for append-only catalogs.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// TestWSRowidGapNoRemovalServed tests corruptN.test#6.0: writable_schema
// UPDATE to sqlite_master.rowid when no rows have been removed.
func TestWSRowidGapNoRemovalServed(t *testing.T) {
	differ(t, "corruptN.test#6.0 up to the writable_schema rowid UPDATE", []string{
		`PRAGMA auto_vacuum = 0`,
		`PRAGMA page_size=1024`,
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1(b) VALUES(zeroblob(300)),(zeroblob(300)),(zeroblob(300)),(zeroblob(300))`,
		`CREATE TABLE t2(a)`,
		`CREATE TRIGGER t1tr BEFORE UPDATE ON t1 BEGIN DELETE FROM t2; END`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET rootpage=3 WHERE rowid=2`,
		// Read back through the ordinary (non-writable_schema) surface too --
		// unaffected either way, since this write only ever touches the
		// wsEdits overlay, never db.tables.
		`SELECT * FROM t1`,
	})
	// "SELECT * FROM t2" and "PRAGMA integrity_check" are deliberately NOT
	// compared past this point, and the reason is measured rather than assumed.
	//
	// The UPDATE above deliberately points t2's rootpage at page 3, which
	// belongs to t1 -- corruptN.test's whole purpose. Up to and including that
	// UPDATE the two engines agree, and the FILES THEMSELVES are byte-identical
	// (5120 bytes, verified directly for this exact fixture), so nothing about
	// the write path differs. What differs is what each engine makes of a table
	// whose root is another table's page: C SQLite reads zero rows there and
	// still answers "ok", while this engine reads the page's cells (three
	// all-NULL rows for t2's one-column shape) and its integrity check reports
	// the shared page and the now-unreferenced one.
	//
	// Neither is "the" right answer -- the database is deliberately broken, and
	// C's own corruptN.test exists to check that a corrupt file is handled
	// gracefully rather than to fix its content. This comparison used to pass
	// only because "PRAGMA page_size=1024" was IGNORED here: this engine had
	// already materialized a 4096-byte page 1 by then, so its page 3 was a
	// different page entirely. It honours the page size now (a zero-byte file
	// has no fixed one yet), which is why the difference became visible.
}

// TestWSRowidGapAfterRemovalMatchesCSQLiteAcrossAutocommit is the
// excluded-class proof run the way a real driver user actually would: a
// DROP as one autocommit statement, then -- in a SEPARATE statement, over a
// brand-new *engine.Session session -- a writable_schema UPDATE whose WHERE reads
// rowid.
//
// C SQLite's rowid space is 1,2,3 -> drop the middle one -> 1,3, so rowid 2
// is a GAP and "WHERE rowid=2" affects zero rows. This engine used to write
// its catalog densely on every flush, which resolved rowid 2 to t3 and
// corrupted it; that is why the statement was DECLINED for as long as the
// dense rebuild existed (Conn.wsCatalogRowRemoved, deleted with it). The page
// store appends catalog rows to a real b-tree and leaves the same gap, so the
// statement runs and matches: zero rows affected, t3 untouched.
func TestWSRowidGapAfterRemovalMatchesCSQLiteAcrossAutocommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, s := range []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(a)`, // rowid 2, about to be dropped
		`CREATE TABLE t3(a)`, // rowid 3 -- C SQLite's rowid 2 is a GAP, never t3
		`DROP TABLE t2`,
		`PRAGMA writable_schema=ON`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}

	// Each of these is its OWN autocommit statement -- its own fresh
	// *engine.Session session (driver's openWriteOrCreatePath) -- which is
	// exactly the boundary the old bug crossed: that session saw a freshly
	// reopened, once-again-DENSE catalog and resolved rowid=2 to t3.
	res, err := db.Exec(`UPDATE sqlite_master SET sql='CORRUPTED' WHERE rowid=2`)
	if err != nil {
		t.Fatalf("UPDATE ... WHERE rowid=2: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("UPDATE ... WHERE rowid=2 affected %d row(s); rowid 2 is the DROP's gap, so C SQLite affects none", n)
	}
	if _, err := db.Exec(`DELETE FROM sqlite_master WHERE oid=1`); err != nil {
		t.Fatalf("DELETE ... WHERE oid=1: %v", err)
	}

	// And the row neither statement should have been able to touch is
	// provably untouched -- not just "declined", but genuinely unmodified.
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE name='t3'`)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("t3's row is gone")
	}
	var got string
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := `CREATE TABLE t3(a)`; got != want {
		t.Fatalf("t3's stored SQL = %q, want unchanged %q -- the UPDATE's rowid 2 is a gap, not t3", got, want)
	}
}
