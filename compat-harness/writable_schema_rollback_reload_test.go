// Tests the writable-schema reload mechanism, verifying post-rollback schema state.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// wsRunSetup runs statements on both engines, requiring agreement on success/failure.
func wsRunSetup(t *testing.T, godb *engine.Session, cgodb *sql.DB, stmts []string) {
	t.Helper()
	for _, s := range stmts {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("musql %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("C SQLite %q: %v", s, err)
		}
	}
}

// TestWritableSchemaRollbackReloadVanishesTable verifies that corrupted tables
// are removed from the schema after a rolled-back edit on both engines.
func TestWritableSchemaRollbackReloadVanishesTable(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()

	wsRunSetup(t, godb, cgodb, []string{
		`CREATE TABLE t1(x)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE table t(d CHECK(T(#0)'`,
		`BEGIN`,
		`CREATE TABLE t2(y)`,
		`ROLLBACK`,
	})

	// C SQLite: t1's reload failed to parse, so the connection's live
	// schema no longer has it.
	if _, err := cgodb.Exec(`SELECT * FROM t1`); err == nil {
		t.Fatalf("C SQLite still resolves t1 post-rollback -- this test's own premise is wrong")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("C SQLite post-rollback error = %v, want \"no such table\"", err)
	}

	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.QueryArgs(`SELECT * FROM t1`, nil); err == nil {
		t.Fatalf("musql still resolves t1 post-rollback -- reloadSchemaFromEdits did not vanish it")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("musql post-rollback error = %v, want \"no such table\"", err)
	}

	// t2, created inside the rolled-back transaction, is gone on both --
	// restoreSnapshot's own job, unaffected by this fix.
	if _, err := cgodb.Exec(`SELECT * FROM t2`); err == nil {
		t.Fatalf("C SQLite still resolves t2 post-rollback")
	}
	if _, _, err := p.QueryArgs(`SELECT * FROM t2`, nil); err == nil {
		t.Fatalf("musql still resolves t2 post-rollback")
	}
}

// TestWritableSchemaRollbackReloadVanishesTableAndIndex pins table.test
// 5.2.2's own mechanism the same way: the blanket UPDATE (no WHERE) corrupts
// BOTH t0's row and its explicit index "t"'s row, so the reload must vanish
// an object of EACH kind -- t0 directly (wsReloadVanishObject's table
// branch, which also cascades and removes "t" as t0's own index) and,
// independently of iteration order, "t" on its own (the direct-index
// branch, reached first if the map visits "t" before "t0").
func TestWritableSchemaRollbackReloadVanishesTableAndIndex(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()

	wsRunSetup(t, godb, cgodb, []string{
		`CREATE TABLE t0(a,b)`,
		`CREATE INDEX t ON t0(a)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE a.b(a UNIQUE'`,
		`BEGIN`,
		`CREATE TABLE t1(x)`,
		`ROLLBACK`,
	})

	if _, err := cgodb.Exec(`SELECT * FROM t0`); err == nil {
		t.Fatalf("C SQLite still resolves t0 post-rollback -- this test's own premise is wrong")
	}
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.QueryArgs(`SELECT * FROM t0`, nil); err == nil {
		t.Fatalf("musql still resolves t0 post-rollback")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("musql post-rollback t0 error = %v, want \"no such table\"", err)
	}

	// The index is gone from the LIVE schema too (not just t0): db.indexes
	// no longer resolves "t" at all -- checked here via db.tables' own
	// integrity rather than a fresh CREATE, since wsReloadVanishObject
	// deliberately leaves the "t" entry in db.wsEdits (see that function's
	// own doc comment) precisely so a later CREATE under that SAME name
	// stays declined -- C SQLite's own physical row for "t" still exists
	// post-reload, so a naive re-CREATE would otherwise risk a row-COUNT
	// divergence this write path's one-row-per-name keying cannot represent.
	if _, _, err := godb.ExecArgs(`CREATE TABLE t9(a)`, nil); err != nil {
		t.Fatalf("musql CREATE TABLE t9: %v", err)
	}
	if _, _, err := godb.ExecArgs(`CREATE INDEX t ON t9(a)`, nil); err == nil {
		t.Fatalf("musql accepted CREATE INDEX t reusing the vanished index's own name -- see writableSchemaDDLDecline's existing same-name guard, which wsReloadVanishObject deliberately keeps live for exactly this")
	}
}

// TestWritableSchemaRollbackToSavepointReloads exercises the SAVEPOINT/
// ROLLBACK TO variant of the same mechanism (OP_Savepoint's
// SAVEPOINT_ROLLBACK case, vdbe.c:3939 -- the same DBFLAG_SchemaChange gate
// sqlite3RollbackAll uses, scoped to one savepoint instead of the whole
// transaction). Neither of this bucket's two mined statements uses
// ROLLBACK TO, but rollbackToSavepoint wires the identical reload call, so
// this pins that it neither panics nor diverges from the oracle.
func TestWritableSchemaRollbackToSavepointReloads(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()

	wsRunSetup(t, godb, cgodb, []string{
		`CREATE TABLE t1(x)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE table t(d CHECK(T(#0)'`,
		`BEGIN`,
		`SAVEPOINT s1`,
		`CREATE TABLE t2(y)`,
		`ROLLBACK TO s1`,
		`RELEASE s1`,
		`COMMIT`,
	})

	if _, err := cgodb.Exec(`SELECT * FROM t1`); err == nil {
		t.Fatalf("C SQLite still resolves t1 post-rollback-to-savepoint -- this test's own premise is wrong")
	}
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.QueryArgs(`SELECT * FROM t1`, nil); err == nil {
		t.Fatalf("musql still resolves t1 post-rollback-to-savepoint")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("musql post-rollback-to-savepoint t1 error = %v, want \"no such table\"", err)
	}
}

// TestWritableSchemaRollbackToSavepointReloadsUnderJournalModeOff is the
// regression this bucket's own rollbackToSavepoint fix (engine/txn.go)
// originally shipped with the schema-reload call nested INSIDE the
// `!db.journalOffUndoDisabled()` guard -- so under journal_mode=off, where
// that guard is false, the reload never ran at all and t1 kept resolving.
//
// C SQLite's OP_Savepoint (vdbe.c:3865-3939) never makes that mistake:
// its SAVEPOINT_ROLLBACK case computes isSchemaChange from
// db->mDbFlags & DBFLAG_SchemaChange and, in its own per-database loop,
// calls sqlite3BtreeSavepoint -- the STATE RESTORE journal_mode=off turns
// into a no-op via sqlite3PagerSavepoint's OFF-mode fast path (pager.c).
// Only AFTER that whole loop finishes does vdbe.c:3939 run
// `if( isSchemaChange ){ sqlite3ResetAllSchemasOfConnection(db); ... }`,
// unconditionally, with no re-check of what the (possibly no-op) restore
// did. The two are independent mechanisms in the C, and gating the second
// on the first was the bug.
//
// Confirmed directly against the oracle (real sqlite3, .dbconfig defensive
// off): this exact statement sequence answers "no such table: t1" for the
// final SELECT, while t2 -- created inside the rolled-back-to savepoint --
// still resolves, since journal_mode=off's row/table-data undo is a
// SEPARATE, genuinely skipped mechanism (see engine/journal_mode_off_test.go
// and this file's own package doc comment) from the schema reload pinned
// here.
func TestWritableSchemaRollbackToSavepointReloadsUnderJournalModeOff(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1) // journal_mode is per-connection state
	defer cgodb.Close()

	wsRunSetup(t, godb, cgodb, []string{
		`CREATE TABLE t1(x)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE table t(d CHECK(T(#0)' WHERE name='t1'`,
		`PRAGMA journal_mode=OFF`,
		`BEGIN`,
		`SAVEPOINT s1`,
		`CREATE TABLE t2(y)`,
		`ROLLBACK TO s1`,
	})

	// t1's corrupted row predates the transaction entirely (the writable_schema
	// UPDATE above already committed in autocommit), yet the reload the
	// in-transaction CREATE TABLE t2 triggers must still vanish it here -- the
	// exact live-oracle counter-example that caught this regression.
	if _, err := cgodb.Exec(`SELECT * FROM t1`); err == nil {
		t.Fatalf("C SQLite still resolves t1 post-rollback-to-savepoint under journal_mode=off -- this test's own premise is wrong")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("C SQLite post-rollback-to-savepoint t1 error under journal_mode=off = %v, want \"no such table\"", err)
	}
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.QueryArgs(`SELECT * FROM t1`, nil); err == nil {
		t.Fatalf("musql still resolves t1 post-rollback-to-savepoint under journal_mode=off -- reloadSchemaFromEdits did not fire (the regression this test pins)")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("musql post-rollback-to-savepoint t1 error under journal_mode=off = %v, want \"no such table\"", err)
	}

	// t2 is the OPPOSITE assertion: journal_mode=off's row/table-data undo is
	// genuinely skipped (DB.journalOffUndoDisabled), so the table a ROLLBACK TO
	// would ordinarily discard must SURVIVE on both engines -- the schema
	// reload above must not over-reach and vanish it too.
	if _, err := cgodb.Exec(`SELECT * FROM t2`); err != nil {
		t.Fatalf("C SQLite lost t2 post-rollback-to-savepoint under journal_mode=off: %v", err)
	}
	if _, _, err := p.QueryArgs(`SELECT * FROM t2`, nil); err != nil {
		t.Fatalf("musql lost t2 post-rollback-to-savepoint under journal_mode=off: %v", err)
	}
}

// TestWritableSchemaDDLInsideTransactionStillDeclinesRootpageEdit pins
// corruptN.test 6.0's own shape (a ROOTPAGE edit, not sql-only) as a DDL
// running INSIDE a transaction after that edit -- this must stay declined
// exactly as it did before this fix, since wsVanishReloadPlan/
// wsEditIsSQLOnly excludes any edit touching rootpage. Piece (2) of the
// bucket investigation (a live b-tree read handle keyed by rootpage) is a
// separate, unbuilt capability -- see reloadSchemaFromEdits' own doc
// comment.
func TestWritableSchemaDDLInsideTransactionStillDeclinesRootpageEdit(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()

	for _, s := range []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE t2(a)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET rootpage=999 WHERE name='t1'`,
		`BEGIN`,
	} {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if _, _, err := godb.ExecArgs(`CREATE TABLE t3(a)`, nil); err == nil {
		t.Fatalf("musql allowed a schema-changing DDL inside a transaction over a ROOTPAGE edit -- a later ROLLBACK cannot safely reload this (see wsVanishReloadPlan's own doc comment)")
	}
}

// TestWritableSchemaDDLInsideTransactionStillDeclinesNameEdit pins fkey1.test
// 8.2's own shape (a NAME edit on an auto-index row, not sql-only) the same
// way -- must stay declined.
func TestWritableSchemaDDLInsideTransactionStillDeclinesNameEdit(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()

	for _, s := range []string{
		`CREATE TABLE t1(a UNIQUE)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET name='sqlite_autoindex_t1_9' WHERE name='sqlite_autoindex_t1_1'`,
		`BEGIN`,
	} {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if _, _, err := godb.ExecArgs(`CREATE TABLE t3(a)`, nil); err == nil {
		t.Fatalf("musql allowed a schema-changing DDL inside a transaction over a NAME edit -- a later ROLLBACK cannot safely reload this")
	}
}

// TestWritableSchemaDDLInsideTransactionStillDeclinesUnreproducibleColumnChange
// pins the boundary wsVanishReloadPlan draws deliberately narrow: a
// WELL-FORMED replacement sql text (parses fine, just not the byte-identical
// tail-only shape writableSchemaReload already covers) is a shape real
// SQLite's own reload WOULD apply but this write path cannot re-derive a
// live object from -- so it must stay declined here exactly as it did
// before this fix, not silently guessed at.
func TestWritableSchemaDDLInsideTransactionStillDeclinesUnreproducibleColumnChange(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()

	for _, s := range []string{
		`CREATE TABLE t1(a,b)`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b,c)' WHERE name='t1'`,
		`BEGIN`,
	} {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if _, _, err := godb.ExecArgs(`CREATE TABLE t2(y)`, nil); err == nil {
		t.Fatalf("musql allowed a schema-changing DDL inside a transaction over an unreproducible column-list change")
	}
}
