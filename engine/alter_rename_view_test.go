package engine

import (
	"testing"
)

// TestAlterRenameViewEmptyInList verifies that ALTER TABLE RENAME correctly
// handles views with empty IN() predicates.
func TestAlterRenameViewEmptyInList(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     []string
		alter     string
		wantTable string
		wantView  string
	}{
		{"bare column inside empty IN()", []string{
			`CREATE TABLE t1(a, b, c, d)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (b IN ())`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TABLE t1(a, bbb, c, d)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (b IN ())`},
		{"table-qualified column inside empty IN()", []string{
			`CREATE TABLE t1(x)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE (t1.x IN ())`,
		}, `ALTER TABLE t1 RENAME TO t2`,
			`CREATE TABLE "t2"(x)`,
			`CREATE VIEW v1 AS SELECT * FROM "t2" WHERE (t1.x IN ())`},
		{"row-value tuple inside empty IN()", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE ((a,b) IN ())`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TABLE t1(a, bbb)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE ((a,b) IN ())`},
		{"scalar subquery inside empty IN()", []string{
			`CREATE TABLE t1(a,b,c)`,
			`CREATE TABLE t2(a,b,c)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE (SELECT t1.a FROM t1, t2) IN () OR t1.a=5`,
		}, `ALTER TABLE t2 RENAME TO t3`,
			`CREATE TABLE "t3"(a,b,c)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE (SELECT t1.a FROM t1, t2) IN () OR t1.a=5`},
		// EP_HasFunc: a function call in the operand keeps it mapped, so the
		// column reference inside it IS rewritten -- the arm that makes a
		// blanket "any IN () is unsafe" rule wrong rather than merely narrow.
		{"function call inside empty IN() is NOT exempt", []string{
			`CREATE TABLE t1(a, b, c, d)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (abs(b) IN ())`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TABLE t1(a, bbb, c, d)`,
			`CREATE VIEW v1 AS SELECT * FROM t1 WHERE a=1 OR (abs(bbb) IN ())`},
		// An IN () that has nothing to do with the rename must not stop the
		// rewrite from reaching the rest of the same view.
		{"empty IN() elsewhere in the view", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE VIEW v1 AS SELECT b FROM t1 WHERE (a IN ()) OR c=2`,
		}, `ALTER TABLE t1 RENAME c TO ccc`,
			`CREATE TABLE t1(a, b, ccc)`,
			`CREATE VIEW v1 AS SELECT b FROM t1 WHERE (a IN ()) OR ccc=2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Exec(tc.alter); err != nil {
				t.Fatalf("%q: %v", tc.alter, err)
			}
			found := false
			var all []string
			for _, tb := range db.tables {
				all = append(all, tb.sql)
				if tb.sql == tc.wantTable {
					found = true
				}
			}
			if !found {
				t.Errorf("after %q: no table reads %q; got %q", tc.alter, tc.wantTable, all)
			}
			if got := db.views[0].sql; got != tc.wantView {
				t.Errorf("after %q: view sql = %q, want %q", tc.alter, got, tc.wantView)
			}
		})
	}
}

// TestAlterRenameViewColumnSharesTableName pins the shape this test used to
// assert a DECLINE for: a column sharing the renamed table's own name. The
// decline's own doc comment recorded that C SQLite is not confused by it
// -- it operates on the resolved AST, not raw tokens, and
// "CREATE TABLE t1(t1 INTEGER, b); CREATE VIEW v1 AS SELECT * FROM t1;
// ALTER TABLE t1 RENAME TO t2" leaves v1 reading `SELECT * FROM "t2"` --
// and declined anyway because this write path's rewrite is token-based and
// could not tell a bare "t1" table reference from a bare "t1" column one.
//
// It can now, and provably: the FROM clause is scanned for table-NAME
// positions and that scan is trusted only where it AGREES with the parser's
// own From list (r45FlatViewColumnRefSkips, alter_write.go). So the shape is
// served, and this asserts the rewrite the oracle produces rather than the
// refusal. compat-harness's TestViewRenameShadowedName covers the same
// family against the real oracle.
func TestAlterRenameViewColumnSharesTableName(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t1(t1 INTEGER, b TEXT)`,
		`CREATE VIEW v1 AS SELECT * FROM t1`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Exec(`ALTER TABLE t1 RENAME TO t2`); err != nil {
		t.Fatalf("ALTER TABLE t1 RENAME TO t2: %v", err)
	}
	// Exactly what 3.53.3 leaves behind: the FROM item rewritten and quoted,
	// the COLUMN named t1 untouched.
	if got, want := db.views[0].sql, `CREATE VIEW v1 AS SELECT * FROM "t2"`; got != want {
		t.Errorf("view sql = %q, want %q", got, want)
	}
	if got, want := db.tables[0].sql, `CREATE TABLE "t2"(t1 INTEGER, b TEXT)`; got != want {
		t.Errorf("table sql = %q, want %q", got, want)
	}
}

// TestAlterRenameViewGroupSelfJoinLiteralExempt pins altertab.test 19.100
// (ticket f50af3e8a565776b, verified against 3.53.3): a table renamed out
// from under a view whose FROM clause references it three times (once bare,
// twice inside a parenthesized join group -- one aliased, one not) is not
// "ambiguous column name" the way TestAlterRenameViewAmbiguousColumnDeclinesCleanly's
// shape genuinely is, because the view's select list ("SELECT 1") never
// resolves a single column. viewRenameAmbiguity's own coarse "two FROM
// sources share a name" rule used to fire on the post-rename text's THREE
// same-named "t3"s regardless, declining the whole ALTER with "ambiguous
// column name: t3" where C SQLite runs it to completion --
// viewRenameAmbiguityExempt (alter_write.go) is the fix; see its own doc
// comment for the exact select.c citation.
func TestAlterRenameViewGroupSelfJoinLiteralExempt(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t1(x)`,
		`CREATE VIEW t2 AS SELECT 1 FROM t1, (t1 AS a0, t1)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Exec(`ALTER TABLE t1 RENAME TO t3`); err != nil {
		t.Fatalf("ALTER TABLE t1 RENAME TO t3: %v", err)
	}
	if got, want := db.tables[0].sql, `CREATE TABLE "t3"(x)`; got != want {
		t.Errorf("table sql: got %q, want %q", got, want)
	}
	if got, want := db.views[0].sql, `CREATE VIEW t2 AS SELECT 1 FROM "t3", ("t3" AS a0, "t3")`; got != want {
		t.Errorf("view sql: got %q, want %q", got, want)
	}
}

// TestAlterRenameViewSafeSubset pins the cases checkViewRenameSafe/
// checkViewColumnRenameSafe DO allow through, complementing
// compat-harness/alter_rename_view_test.go's differ()-gated versions of the
// same shapes with a check differ() cannot make: that the rewrite is a
// genuine no-op (same *string value, not just an equal one) when the
// renamed identifier never appears in the view's text at all.
func TestAlterRenameViewSafeSubset(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE rr(a, b)`,
		`CREATE VIEW vv AS SELECT * FROM rr`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Exec(`ALTER TABLE rr RENAME a TO c`); err != nil {
		t.Fatalf("ALTER TABLE rr RENAME a TO c: %v", err)
	}
	if got, want := db.views[0].sql, `CREATE VIEW vv AS SELECT * FROM rr`; got != want {
		t.Errorf("view sql: got %q, want %q (unchanged -- \"a\" never appears in it)", got, want)
	}
	if got, want := db.tables[0].sql, `CREATE TABLE rr(c, b)`; got != want {
		t.Errorf("table sql: got %q, want %q", got, want)
	}
}

// TestAlterRenameColumnTriggerCascadeQuoting pins a fix alongside this
// file's view-rewrite work: applyTriggerRename used to build its
// replacement text via an unconditional quoteIdent(newName), which is
// correct for a TABLE rename's cascade into a trigger body but wrong for a
// COLUMN rename's -- verified directly against 3.53.3: renaming column "a"
// to "aaa" leaves a trigger's own body reading "ORDER BY aaa", never
// `ORDER BY "aaa"`. This was unreachable through compat-harness because
// engine/query.go's own sqlite_master.sql guard blocks reading ANY row's
// sql back once ANY trigger exists in the schema (see this package's
// query.go), so nothing ever compared the over-quoted text against the
// oracle; this reads db.triggers[0].sql directly instead, bypassing that
// guard the same way the guard's own existence implies nothing else could.
func TestAlterRenameColumnTriggerCascadeQuoting(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT sum(b) OVER w FROM t1 WINDOW w AS (ORDER BY a); END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Exec(`ALTER TABLE t1 RENAME a TO aaa`); err != nil {
		t.Fatalf("ALTER TABLE t1 RENAME a TO aaa: %v", err)
	}
	want := `CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT sum(b) OVER w FROM t1 WINDOW w AS (ORDER BY aaa); END`
	if got := db.triggers[0].sql; got != want {
		t.Errorf("trigger sql: got %q, want %q (bare \"aaa\", not quoted)", got, want)
	}
	// The trigger must still actually FIRE against the renamed column --
	// this is the functional half; the quoting fix touches text only.
	if err := db.Exec(`INSERT INTO t1 VALUES(1,2)`); err != nil {
		t.Errorf("trigger no longer fires after the rename: %v", err)
	}
}
