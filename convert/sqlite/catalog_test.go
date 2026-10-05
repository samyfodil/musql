package sqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// The converter's promise, both directions, with no SQL run against the SQLite
// format: a database is built with SQL, written out with Export, read back with
// Import, and the two musql databases are compared. C's
// own reading of an exported file is compat-harness's to check -- it has the
// oracle (convert_for_oracle_test.go).

// buildDB builds a database at path from stmts.
func buildDB(t testing.TB, path string, stmts ...string) {
	t.Helper()
	n, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			n.Discard()
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

// execDB runs stmts against an existing database and commits them.
func execDB(t testing.TB, path string, stmts ...string) {
	t.Helper()
	n, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			n.Discard()
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

// roundTrip exports src to the SQLite format and imports that back into a new
// database, whose path it returns.
func roundTrip(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	sq, back := filepath.Join(dir, "x.db"), filepath.Join(dir, "back.musq")
	if err := Export(src, sq, 0); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := Import(sq, back, ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return back
}

// queryDB renders q's answer over the database at path, "" and the
// error when it fails.
func queryDB(t *testing.T, path, q string) (string, error) {
	t.Helper()
	n, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	_, rows, err := n.Query(q, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range rows {
		for _, c := range r {
			fmt.Fprintf(&b, "%d:%d:%v:%q|", c.Typ, c.I, c.F, string(c.S))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// catalogOf renders every schema object's type, name, table and SQL.
func catalogOf(t *testing.T, path string) string {
	t.Helper()
	out, err := queryDB(t, path, `SELECT type, name, tbl_name, coalesce(sql,'') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func seqOf(t *testing.T, path string) string {
	t.Helper()
	out, err := queryDB(t, path, `SELECT name, seq FROM sqlite_sequence ORDER BY name`)
	if err != nil {
		return "<no sqlite_sequence>"
	}
	return out
}

// allTablesOf renders every row of every table, so a restore that invented rows
// in a table nobody thought to check is still caught.
func allTablesOf(t *testing.T, path string) string {
	t.Helper()
	n, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := n.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`, nil)
	n.Discard()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		name := string(r[0].S)
		out, qerr := queryDB(t, path, `SELECT * FROM "`+strings.ReplaceAll(name, `"`, `""`)+`" ORDER BY rowid`)
		if qerr != nil {
			t.Fatalf("reading %s: %v", name, qerr)
		}
		b.WriteString(name + ":\n" + out)
	}
	return b.String()
}

// TestRoundTripKeepsTheWholeCatalog: every object kind survives both directions
// -- and a trigger is not FIRED by the rows the export writes, which would leave
// rows in its target table the source never had.
func TestRoundTripKeepsTheWholeCatalog(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.musq")
	buildDB(t, src,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`,
		`CREATE TABLE log(id INTEGER PRIMARY KEY, msg TEXT)`,
		`CREATE INDEX t_k ON t(k)`,
		`CREATE UNIQUE INDEX t_s ON t(s)`,
		`CREATE VIEW v AS SELECT id, k FROM t WHERE k > 10`,
		`CREATE TRIGGER t_ins AFTER INSERT ON t BEGIN INSERT INTO log(msg) VALUES('added'); END`,
		`INSERT INTO t VALUES(1,5,'a'),(2,20,'b'),(3,30,'c')`,
	)
	back := roundTrip(t, src)
	if got, want := catalogOf(t, back), catalogOf(t, src); got != want {
		t.Errorf("the round trip changed the catalog\n source:\n%s\n round-tripped:\n%s", want, got)
	}
	if got, want := allTablesOf(t, back), allTablesOf(t, src); got != want {
		t.Errorf("a table's contents changed\n source:\n%s\n round-tripped:\n%s", want, got)
	}
}

// TestRoundTripKeepsAutoincrementState: sqlite_sequence, where AUTOINCREMENT's
// next rowid lives. Losing it makes a restored table hand out a rowid the
// source had already used.
func TestRoundTripKeepsAutoincrementState(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.musq")
	buildDB(t, src,
		`CREATE TABLE s(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`,
		`INSERT INTO s(v) VALUES('a'),('b'),('c')`,
		`DELETE FROM s WHERE id = 3`,
	)
	back := roundTrip(t, src)
	if got, want := seqOf(t, back), seqOf(t, src); got != want {
		t.Errorf("sqlite_sequence after the round trip = %q, want %q: the next AUTOINCREMENT "+
			"rowid would repeat one the source already used", got, want)
	}
	execDB(t, back, `INSERT INTO s(v) VALUES('d')`)
	if got, err := queryDB(t, back, `SELECT max(id) FROM s`); err != nil || got != "1:4:0:\"\"|\n" {
		t.Errorf("the next AUTOINCREMENT rowid after the round trip = %q, %v; want 4", got, err)
	}
}

// TestExportImportIsLossless: every object, every row, every AUTOINCREMENT
// counter -- INCLUDING writes still in the delta, not yet folded into segments.
func TestExportImportIsLossless(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.musq")
	buildDB(t, src,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`,
		`CREATE TABLE log(id INTEGER PRIMARY KEY AUTOINCREMENT, msg TEXT)`,
		`CREATE INDEX t_k ON t(k)`,
		`CREATE VIEW v AS SELECT id, k FROM t WHERE k > 10`,
		`CREATE TRIGGER t_ins AFTER INSERT ON t BEGIN INSERT INTO log(msg) VALUES('x'); END`,
		`INSERT INTO t VALUES(10,5,'a'),(20,20,'b'),(30,30,'c')`,
		`DELETE FROM log`,
	)
	execDB(t, src, `VACUUM`)
	// Writes AFTER the segments were laid out, so they are in the delta.
	execDB(t, src, `UPDATE t SET k = 99 WHERE id = 20`, `DELETE FROM t WHERE id = 30`)
	back := roundTrip(t, src)
	if got, want := catalogOf(t, back), catalogOf(t, src); got != want {
		t.Errorf("catalog differs\n source:\n%s\n round-tripped:\n%s", want, got)
	}
	if got, want := seqOf(t, back), seqOf(t, src); got != want {
		t.Errorf("sqlite_sequence = %q, want %q", got, want)
	}
	if got, want := allTablesOf(t, back), allTablesOf(t, src); got != want {
		t.Errorf("rows differ -- the delta was not folded in\n source:\n%s\n round-tripped:\n%s", want, got)
	}
}

// toSQLiteInPlace replaces the database just built at path with its export to
// C's format, at the same path, so a test of the SQLite READER (the structural
// check, the pager) has a real C-format file to read and to corrupt. The delta
// beside it goes too.
func toSQLiteInPlace(t testing.TB, path string, pageSize int) {
	t.Helper()
	tmp := path + ".export"
	if err := Export(path, tmp, pageSize); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := os.Remove(path + ".delta"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestImportRefusesACheckViolation: a source holding a row its own CHECK
// constraint forbids fails integrity_check in C, so the import refuses it --
// and leaves nothing behind. The CHECK half runs on the converted copy (no SQL
// runs on the source), which is what this pins.
func TestImportRefusesACheckViolation(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.musq")
	buildDB(t, src,
		`CREATE TABLE t(a CHECK (a > 0))`,
		`PRAGMA ignore_check_constraints = ON`,
		`INSERT INTO t VALUES(-1)`)
	sq := filepath.Join(t.TempDir(), "x.db")
	if err := Export(src, sq, 0); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "back.musq")
	err := Import(sq, dst, ImportOptions{})
	if !errors.Is(err, ErrSourceCorrupt) {
		t.Fatalf("Import of a CHECK-violating source: err = %v, want ErrSourceCorrupt", err)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("a refused import left %s behind", dst)
	}
	if err := Import(sq, dst, ImportOptions{Force: true}); err != nil {
		t.Errorf("Force should convert it anyway: %v", err)
	}
}
