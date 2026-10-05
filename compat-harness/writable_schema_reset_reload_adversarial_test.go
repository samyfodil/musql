// Adversarial coverage for schema reload operations.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// wsAdvStep represents a single step in schema reload testing.
type wsAdvStep struct {
	sql     string
	decline bool
}

func runWsAdvCase(t *testing.T, name string, steps []wsAdvStep) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
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

		for i, step := range steps {
			goCols, goRows, goErr := wsGoRun(godb, step.sql)
			if step.decline {
				if goErr == nil {
					t.Fatalf("step %d %q: expected DECLINE, got cols=%v rows=%v", i, step.sql, goCols, goRows)
				}
				continue
			}
			if goErr != nil {
				t.Fatalf("step %d %q: musql errored: %v", i, step.sql, goErr)
			}
			cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, step.sql)
			if cgoErr != nil {
				t.Fatalf("step %d %q: C SQLite errored where musql accepted: %v", i, step.sql, cgoErr)
			}
			if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
				t.Fatalf("step %d %q diverges:\n  pure: %v %v\n  real: %v %v", i, step.sql, goCols, goRows, cgoCols, cgoRows)
			}
		}
	})
}

// (A) STRICT tail added to a table already WITHOUT ROWID -- withoutRowid
// unchanged (still true), only STRICT newly appears in the tail. Exercises
// the "STRICT and WITHOUT ROWID compose idempotently" comment in
// sql_parser.go (both the auto-PK-NOT-NULL rule from finalizeWithoutRowidPK
// AND the STRICT-table rule apply to the same pk column) via the RELOAD path
// specifically, not a plain CREATE.
func TestWritableSchemaResetReloadAdversarial_WithoutRowidPlusStrict(t *testing.T) {
	runWsAdvCase(t, "without-rowid-plus-strict", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT) WITHOUT ROWID`},
		{sql: `INSERT INTO t1 VALUES(1,'a'),('two','b')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT) WITHOUT ROWID, STRICT' WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		// ANY PK column under STRICT is still exempt from the affinity coercion
		// (strictColAffinity: ANY -> BLOB, i.e. none), but table_info's declared
		// type text must reflect the object musql now holds post-reload.
		{sql: `SELECT name, type FROM pragma_table_info('t1')`},
		// A NULL PK insert must now fail: WITHOUT ROWID's own PK-NOT-NULL rule
		// already forced this pre-reload too, so this alone doesn't
		// discriminate STRICT's effect -- the discriminating check is the
		// BLOB-typed value below.
		{sql: `INSERT INTO t1(x) VALUES('c')`, decline: true},
	})
}

// (B) STRICT tail added to a table with an AUTOINCREMENT rowid-alias PK and a
// SEPARATE ANY-typed column. Confirms (1) AUTOINCREMENT survives the reload
// unchanged (an implicit-NULL insert into the PK still auto-assigns, it does
// NOT trip the new STRICT NOT NULL rule -- the rowid alias is exempt, per
// build.c's "p->iPKey!=ii" condition and sql_parser.go's mirror), and (2)
// the ANY column's affinity really becomes BLOB (no coercion) post-reload,
// not merely that inserts succeed -- storing an integer-looking TEXT value
// and reading it back must come back as TEXT, not coerced to INTEGER the way
// pre-STRICT NUMERIC-default affinity would.
func TestWritableSchemaResetReloadAdversarial_AutoincrementPlusAnyAffinity(t *testing.T) {
	runWsAdvCase(t, "autoincrement-plus-any-affinity", []wsAdvStep{
		{sql: `CREATE TABLE t1(id INTEGER PRIMARY KEY AUTOINCREMENT, x ANY)`},
		{sql: `INSERT INTO t1(x) VALUES('5')`}, // pre-STRICT: NUMERIC-ish default affinity may coerce
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		// implicit-NULL PK insert must still succeed (AUTOINCREMENT rowid
		// alias is exempt from STRICT's PK-NOT-NULL rule).
		{sql: `INSERT INTO t1(x) VALUES('7')`},
		// A text value into the now-BLOB-affinity ANY column must round-trip
		// as TEXT, unconverted.
		{sql: `INSERT INTO t1(x) VALUES('99')`},
		{sql: `SELECT typeof(x), x FROM t1 ORDER BY id`},
		{sql: `SELECT seq FROM sqlite_sequence WHERE name='t1'`},
	})
}

// (C) A CHECK constraint (column-level) alongside a STRICT tail change:
// checks must keep enforcing post-reload, and STRICT's own NOT-NULL PK rule
// must not disturb the CHECK constraint's own text/behavior.
func TestWritableSchemaResetReloadAdversarial_CheckConstraintSurvives(t *testing.T) {
	runWsAdvCase(t, "check-constraint-survives", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x INTEGER CHECK(x > 0))`},
		{sql: `INSERT INTO t1 VALUES(1, 5)`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1 VALUES(2, -1)`, decline: true}, // CHECK still enforced
		{sql: `INSERT INTO t1(x) VALUES(9)`, decline: true},  // now also NOT NULL on PK
	})
}

// (D) A UNIQUE column constraint (automatic index) alongside a STRICT tail
// change: uniqueness must still be enforced post-reload, pinning the doc
// comment's claim that db.indexes' automatic entries for this table need no
// update because STRICT doesn't touch them.
func TestWritableSchemaResetReloadAdversarial_UniqueIndexSurvives(t *testing.T) {
	runWsAdvCase(t, "unique-index-survives", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT UNIQUE)`},
		{sql: `INSERT INTO t1 VALUES(1, 'a')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1 VALUES(2, 'a')`, decline: true}, // UNIQUE(x) still enforced
		{sql: `INSERT INTO t1 VALUES(3, 'b')`},
	})
}

// (E) Reverse direction: a table that IS STRICT has its tail edited to
// REMOVE the STRICT keyword. C SQLite's reload is symmetric -- it just
// re-parses whatever text is there now -- so downgrading must work exactly
// like upgrading: the PK's implicit NOT NULL (STRICT's rule) must LAPSE, and
// a NULL PK insert that used to fail must now succeed again.
func TestWritableSchemaResetReloadAdversarial_StrictRemoved(t *testing.T) {
	runWsAdvCase(t, "strict-removed", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT) STRICT`},
		{sql: `INSERT INTO t1 VALUES(1,'a')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql='CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)' WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1(x) VALUES('b')`}, // PK now nullable again -- must succeed
		{sql: `SELECT id, x FROM t1 ORDER BY x`},
	})
}

// (F) Two DIFFERENT tables, each independently eligible for the reload
// (a plain STRICT-tail addition, no other edit), inside the SAME
// writable_schema session -- writableSchemaReload's loop must apply BOTH,
// not just the first it sees in map iteration order.
func TestWritableSchemaResetReloadAdversarial_TwoTablesBothReload(t *testing.T) {
	runWsAdvCase(t, "two-tables-both-reload", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`},
		{sql: `CREATE TABLE t2(id ANY PRIMARY KEY, y TEXT)`},
		{sql: `INSERT INTO t1 VALUES(1,'a')`},
		{sql: `INSERT INTO t2 VALUES(1,'b')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name IN ('t1','t2')`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1(x) VALUES('c')`, decline: true}, // t1's PK now NOT NULL
		{sql: `INSERT INTO t2(y) VALUES('d')`, decline: true}, // t2's PK now NOT NULL too
	})
}

// (G) One table's edit fits the reload shape, and a DIFFERENT table's row in the
// SAME session has its name column changed away from what its sql text creates.
// C does not decline anything here: RESET succeeds, and the reload's
// sqlite3CheckObjectName (build.c:1046-1050) fails on t2renamed's row, so every
// later statement answers "malformed database schema" -- t1's STRICT included,
// since the load is whole-schema. Replayed against C.
func TestWritableSchemaResetReloadAdversarial_MixedSessionAllOrNothing(t *testing.T) {
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`},
		{sql: `CREATE TABLE t2(a, b)`},
		{sql: `INSERT INTO t1 VALUES(1,'a')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`},
		{sql: `UPDATE sqlite_schema SET name='t2renamed' WHERE name='t2'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1(x) VALUES('c')`, wantErrSubstr: "malformed database schema (t2renamed)"},
	})
}

// (H) The sql edit is accompanied, on the SAME row, by a rootpage assignment of
// the row's OWN current value: a no-op in C, whose reload binds t1 to the b-tree
// it already had (build.c:2673). Served, and replayed against C -- the STRICT tail
// takes, so an implicit-NULL primary key now fails.
func TestWritableSchemaResetReloadAdversarial_RootpageSetToSameValueStillDeclines(t *testing.T) {
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`},
		{sql: `INSERT INTO t1 VALUES(1,'a')`},
		{sql: `PRAGMA writable_schema=ON`},
		// t1 is the first object, so its root is 2 in both engines.
		{sql: `UPDATE sqlite_schema SET sql=(sql||' STRICT'), rootpage=2 WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `SELECT sql, rootpage FROM sqlite_schema`, query: true},
		{sql: `INSERT INTO t1(x) VALUES('c')`, wantErrSubstr: "NOT NULL constraint failed"},
		{sql: `SELECT * FROM t1`, query: true},
	})
}

// (I) tbl_name assigned its own value, sql untouched: a no-op, which C reloads
// without complaint. Replayed against C.
func TestWritableSchemaResetReloadAdversarial_TblNameTouchedDeclines(t *testing.T) {
	wsrpRun(t, 4096, []wsrpStep{
		{sql: `CREATE TABLE t1(a, b)`},
		{sql: `INSERT INTO t1 VALUES(1,2)`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET tbl_name='t1' WHERE name='t1'`},
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `SELECT * FROM t1`, query: true},
		{sql: `INSERT INTO t1 VALUES(3,4)`},
		{sql: `SELECT count(*) FROM t1`, query: true},
	})
}

// (J) PRAGMA table_info reflects the reloaded object's declared column
// TYPES, not merely that DML enforcement changed -- a second, independent
// read path (pragma_table_info / table_info) from the one
// TestWritableSchemaResetReloadsStrictTailChange already checks
// (sqlite_master.sql text) and the one INSERT-enforcement already checks.
func TestWritableSchemaResetReloadAdversarial_TableInfoReflectsReload(t *testing.T) {
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

	setup := []string{
		`CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`,
		`INSERT INTO t1 VALUES(1,'a')`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`,
	}
	for _, s := range setup {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("musql setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("real setup %q: %v", s, err)
		}
	}
	if _, _, err := godb.ExecArgs(`PRAGMA writable_schema=RESET`, nil); err != nil {
		t.Fatalf("musql RESET: %v", err)
	}
	if _, err := cgodb.Exec(`PRAGMA writable_schema=RESET`); err != nil {
		t.Fatalf("real RESET: %v", err)
	}

	goCols, goRows, goErr := wsGoRun(godb, `SELECT name, "notnull", pk FROM pragma_table_info('t1')`)
	if goErr != nil {
		t.Fatalf("musql table_info: %v", goErr)
	}
	cgoCols, cgoRows, cgoErr := wsCGORun(cgodb, `SELECT name, "notnull", pk FROM pragma_table_info('t1')`)
	if cgoErr != nil {
		t.Fatalf("real table_info: %v", cgoErr)
	}
	if !wsSameResult(goCols, goRows, cgoCols, cgoRows) {
		t.Fatalf("post-reload pragma_table_info diverges:\n  pure: %v %v\n  real: %v %v", goCols, goRows, cgoCols, cgoRows)
	}
	// id's notnull must now be 1 (STRICT's implicit PK NOT NULL). Cell values
	// are normalizeEngineValue's typed form ("T:id", "I:1"), not bare text.
	found := false
	for _, r := range goRows {
		if r[0] == "T:id" {
			found = true
			if r[1] != "I:1" {
				t.Errorf("post-reload table_info: id.notnull = %q, want I:1 (STRICT PK)", r[1])
			}
		}
	}
	if !found {
		t.Fatalf("post-reload table_info: no row for column id (goRows=%v cgoRows=%v)", goRows, cgoRows)
	}
}

// (K) A CASE-DIFFERENT but byte-different tail spelling: the mined shape's
// own oracle-verified example concatenates "STRICT" with no leading space
// (`sql||'STRICT'`), producing e.g. "...)STRICT" with no space before the
// keyword. Confirms the reload's re-lex/re-parse tolerates that exact
// no-space adjacency (a lexer edge case distinct from the well-spaced forms
// every other case here uses), matching the shipped test's own setup text
// but checked here against table_info/typeof rather than just integrity/NOT
// NULL, and additionally exercised with an explicit "WITHOUT ROWID"WITHOUT
// SPACE before it is avoided since that's a different token entirely --
// this only pins the STRICT-with-no-preceding-space case.
func TestWritableSchemaResetReloadAdversarial_NoSpaceBeforeStrictKeyword(t *testing.T) {
	runWsAdvCase(t, "no-space-before-strict-keyword", []wsAdvStep{
		{sql: `CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`},
		{sql: `INSERT INTO t1 VALUES(1,'a')`},
		{sql: `PRAGMA writable_schema=ON`},
		{sql: `UPDATE sqlite_schema SET sql=(sql||'STRICT') WHERE name='t1'`}, // no space, exactly like strict2.test
		{sql: `PRAGMA writable_schema=RESET`},
		{sql: `INSERT INTO t1(x) VALUES('b')`, decline: true},
	})
}

// (L) ROLLBACK after a successful reload must undo it completely -- both the
// catalog overlay (already covered generally by the writable_schema write
// path's own probe) AND the LIVE tableMeta mutation writableSchemaReload
// performs directly on tbl.cols/strict/pkIndex/autoIncrement, which no
// other codepath in this subsystem does (every other writable_schema
// operation only ever touches the wsEdits OVERLAY, never db.tables itself --
// see schema_write_direct.go's own package doc comment). Confirms
// txn.go's restoreSnapshot -- a wholesale db.tables slice-pointer swap back
// to the snapshot taken at BEGIN -- discards the reload's in-place tbl
// mutation the same way it discards any other write.
func TestWritableSchemaResetReloadAdversarial_RollbackUndoesReload(t *testing.T) {
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "pure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()

	for _, s := range []string{
		`CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`,
		`INSERT INTO t1 VALUES(1,'a')`,
		`BEGIN`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`,
		`PRAGMA writable_schema=RESET`,
	} {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	// Inside the transaction, STRICT is armed: an implicit-NULL PK insert
	// must fail.
	if _, _, err := godb.ExecArgs(`INSERT INTO t1(x) VALUES('b')`, nil); err == nil {
		t.Fatalf("STRICT was not armed by the in-transaction reload")
	}
	if _, _, err := godb.ExecArgs(`ROLLBACK`, nil); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	// Post-ROLLBACK: the table must be back to its PRE-reload, non-STRICT
	// shape -- an implicit-NULL PK insert must now SUCCEED again, and
	// sqlite_master.sql must show the ORIGINAL (pre-edit) text.
	if _, _, err := godb.ExecArgs(`INSERT INTO t1(x) VALUES('b')`, nil); err != nil {
		t.Fatalf("post-ROLLBACK insert failed: %v (reload was not undone)", err)
	}
	p, err := godb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(`SELECT sql FROM sqlite_master WHERE name='t1'`, nil)
	if err != nil {
		t.Fatalf("post-ROLLBACK sql read: %v", err)
	}
	if len(rows) != 1 || strings.Contains(string(rows[0][0].S), "STRICT") {
		t.Errorf("post-ROLLBACK sqlite_master.sql = %v, want the ORIGINAL (non-STRICT) text", rows)
	}
	// writable_schema itself must stay OFF: unlike the catalog edit, the FLAG
	// is connection-level PRAGMA state, not part of the B-tree transaction --
	// verified directly against mattn/go-sqlite3 3.53.3 (both directions:
	// "BEGIN; PRAGMA writable_schema=ON; ROLLBACK" leaves it ON, and this
	// exact RESET-inside-a-transaction shape leaves it OFF) -- matching this
	// file's own package doc comment ("the flag survives COMMIT/ROLLBACK").
	// Only the catalog row edit the RESET's reload consumed is transactional,
	// checked above.
	_, wsRows, err := p.QueryArgs(`PRAGMA writable_schema`, nil)
	if err != nil {
		t.Fatalf("post-ROLLBACK writable_schema read: %v", err)
	}
	if len(wsRows) != 1 || wsRows[0][0].I != 0 {
		t.Errorf("post-ROLLBACK PRAGMA writable_schema = %v, want 0 (OFF -- the flag itself is non-transactional and does not revert)", wsRows)
	}
}
