package compat

// Tests for pragma reporters: PRAGMA pragma_list and the column-naming flags.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// tailR30PragmaPair opens one Go engine and one cgo oracle over fresh files.
func tailR30PragmaPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	godb, err := engine.Create(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { godb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	return godb, cdb
}

// tailR30GoQueryRows runs stmt through the Go engine's READ side and returns
// its columns and normalized rows, failing the test on any error.
func tailR30GoQueryRows(t *testing.T, godb *engine.Session, stmt string) ([]string, [][]string) {
	t.Helper()
	cols, rows, err, panicked, pv := tclSafeGoQuery(godb, stmt)
	if panicked {
		t.Fatalf("go engine PANICKED on %q: %v", stmt, pv)
	}
	if err != nil {
		t.Fatalf("go engine declined %q: %v", stmt, err)
	}
	return cols, rows
}

// tailR30AssertSame runs stmt on both engines' read sides and requires an
// identical column list and row set, in order.
func tailR30AssertSame(t *testing.T, godb *engine.Session, cdb *sql.DB, stmt string) {
	t.Helper()
	gotCols, gotRows := tailR30GoQueryRows(t, godb, stmt)
	wantCols, wantRows, cerr := tclRunCGOQuery(cdb, stmt)
	if cerr != nil {
		t.Fatalf("oracle rejected %q, so this gate cannot pin it: %v", stmt, cerr)
	}
	if len(gotCols) != len(wantCols) {
		t.Fatalf("%q: column count %v, oracle %v", stmt, gotCols, wantCols)
	}
	for i := range gotCols {
		if gotCols[i] != wantCols[i] {
			t.Errorf("%q: column %d is %q, oracle says %q", stmt, i, gotCols[i], wantCols[i])
		}
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("%q: %d row(s), oracle %d\n  go:  %v\n  cgo: %v", stmt, len(gotRows), len(wantRows), gotRows, wantRows)
	}
	for r := range gotRows {
		if len(gotRows[r]) != len(wantRows[r]) {
			t.Fatalf("%q row %d: width %d, oracle %d", stmt, r, len(gotRows[r]), len(wantRows[r]))
		}
		for c := range gotRows[r] {
			if gotRows[r][c] != wantRows[r][c] {
				t.Errorf("%q row %d col %d: %q, oracle says %q", stmt, r, c, gotRows[r][c], wantRows[r][c])
			}
		}
	}
}

// tailR30Both runs a statement on BOTH engines' write sides, keeping them in
// lockstep, and fails if either rejects it.
func tailR30Both(t *testing.T, godb *engine.Session, cdb *sql.DB, stmt string) {
	t.Helper()
	if err, panicked, pv := tclSafeExecArgs(godb, stmt); panicked {
		t.Fatalf("go engine PANICKED on %q: %v", stmt, pv)
	} else if err != nil {
		t.Fatalf("go engine rejected %q: %v", stmt, err)
	}
	if _, err := cdb.Exec(stmt); err != nil {
		t.Fatalf("oracle rejected %q: %v", stmt, err)
	}
}

// TestTailR30PragmaList verifies PRAGMA pragma_list and its table-valued function form.
func TestTailR30PragmaList(t *testing.T) {
	godb, cdb := tailR30PragmaPair(t)
	for _, stmt := range []string{
		"SELECT * FROM pragma_pragma_list",
		"SELECT * FROM pragma_pragma_list ORDER BY name",
		"SELECT count(*) FROM pragma_pragma_list",
		"SELECT * FROM pragma_pragma_list WHERE name='pragma_list'",
		"SELECT name FROM pragma_pragma_list WHERE name LIKE 'temp%' ORDER BY 1",
		"SELECT rowid, name FROM pragma_pragma_list ORDER BY rowid LIMIT 4",
	} {
		tailR30AssertSame(t, godb, cdb, stmt)
	}

	// The bare pragma, through the read side. "PRAGMA pragma_list" is not a
	// SELECT, so tclRunCGOQuery is asked for it directly.
	tailR30AssertSame(t, godb, cdb, "PRAGMA pragma_list")

	// pragma_list does not accept arguments.
	tailR30AssertSame(t, godb, cdb, "PRAGMA pragma_list=1")

	// A qualifier names a database and pragma_list is not per-database.
	tailR30AssertSame(t, godb, cdb, "PRAGMA main.pragma_list")
	tailR30AssertSame(t, godb, cdb, "PRAGMA temp.pragma_list")

	// Verify that invalid forms are rejected.
	for _, stmt := range []string{
		"SELECT * FROM pragma_pragma_list('x')",
		"SELECT arg FROM pragma_pragma_list",
		"SELECT schema FROM pragma_pragma_list",
	} {
		if _, _, cerr := tclRunCGOQuery(cdb, stmt); cerr == nil {
			t.Fatalf("premise broken: the oracle ACCEPTS %q, so this engine must too", stmt)
		}
		if _, _, err, panicked, pv := tclSafeGoQuery(godb, stmt); panicked {
			t.Fatalf("go engine PANICKED on %q: %v", stmt, pv)
		} else if err == nil {
			t.Errorf("%q: this engine accepted a shape the oracle rejects", stmt)
		}
	}
}

// TestTailR30ColumnNameFlagGetters verifies the column-naming flag getters and setters.
func TestTailR30ColumnNameFlagGetters(t *testing.T) {
	godb, cdb := tailR30PragmaPair(t)
	tailR30Both(t, godb, cdb, "CREATE TABLE tabc(a,b,c)")
	tailR30Both(t, godb, cdb, "INSERT INTO tabc VALUES(1,2,3)")

	// Check defaults on a fresh connection.
	tailR30AssertSame(t, godb, cdb, "PRAGMA short_column_names")
	tailR30AssertSame(t, godb, cdb, "PRAGMA full_column_names")

	for _, set := range []struct{ short, full string }{
		{"ON", "OFF"}, {"OFF", "OFF"}, {"OFF", "ON"}, {"ON", "ON"},
		{"1", "0"}, {"0", "1"}, {"yes", "no"}, {"false", "true"},
	} {
		tailR30Both(t, godb, cdb, "PRAGMA short_column_names="+set.short)
		tailR30Both(t, godb, cdb, "PRAGMA full_column_names="+set.full)
		tailR30AssertSame(t, godb, cdb, "PRAGMA short_column_names")
		tailR30AssertSame(t, godb, cdb, "PRAGMA full_column_names")
		// ...and the naming rule the flags describe really moved with them.
		tailR30AssertSame(t, godb, cdb, "SELECT a, b+1, tabc.c FROM tabc")
		tailR30AssertSame(t, godb, cdb, "SELECT * FROM tabc")
	}

	// Qualifiers have no effect on these pragmas.
	tailR30AssertSame(t, godb, cdb, "PRAGMA main.short_column_names")
	tailR30AssertSame(t, godb, cdb, "PRAGMA temp.full_column_names")
}
