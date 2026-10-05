package engine

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Render query rows as "a|b;c|d" for comparison.
func queryOne(t *testing.T, n *Session, sql string) string {
	t.Helper()
	p, err := n.ReadPager()
	if err != nil {
		t.Fatalf("%s: read pager: %v", sql, err)
	}
	_, rows, qerr := p.QueryArgs(sql, nil)
	if qerr != nil {
		t.Fatalf("%s: %v", sql, qerr)
	}
	var out []string
	for _, r := range rows {
		cells := make([]string, 0, len(r))
		for _, v := range r {
			switch v.Typ {
			case Null:
				cells = append(cells, "NULL")
			case Text:
				cells = append(cells, string(v.S))
			case Int:
				cells = append(cells, strconv.FormatInt(v.I, 10))
			default:
				cells = append(cells, "?")
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
	return strings.Join(out, ";")
}

// Direct catalog writes with PRAGMA writable_schema behave as on SQLite format.
func TestWritableSchemaOnSegments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.musq")
	nw, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a, b, c)`,
		`INSERT INTO t1 VALUES('x','y','z')`,
		`CREATE TABLE t2(d)`,
		`INSERT INTO t2 VALUES(7)`,
		`PRAGMA writable_schema = ON`,
	} {
		if _, _, eerr := nw.ExecArgs(s, nil); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}

	// A SQL-TEXT EDIT. The scan of sqlite_master sees it immediately (C has
	// written it into the b-tree); the SCHEMA only changes at RESET, which is
	// prepare.c's whole-schema reload.
	if _, _, eerr := nw.ExecArgs(`UPDATE sqlite_master SET sql='CREATE TABLE t1(a,b)' WHERE name='t1'`, nil); eerr != nil {
		t.Fatalf("UPDATE sqlite_master: %v", eerr)
	}
	if got := queryOne(t, nw, `SELECT sql FROM sqlite_master WHERE name='t1'`); got != "CREATE TABLE t1(a,b)" {
		t.Errorf("the scan does not show the edit: %q", got)
	}
	if _, _, eerr := nw.ExecArgs(`PRAGMA writable_schema = RESET`, nil); eerr != nil {
		t.Fatalf("RESET after a sql edit: %v", eerr)
	}
	// Two columns now, and the row that was stored under three keeps its first
	// two values -- the edit rewrites the catalog, never the data.
	if got := queryOne(t, nw, `SELECT * FROM t1`); got != "x|y" {
		t.Errorf("after the reload: SELECT * FROM t1 = %q, want x|y", got)
	}

	// A DELETED CATALOG ROW. The object is gone after the reload.
	for _, s := range []string{
		`PRAGMA writable_schema = ON`,
		`DELETE FROM sqlite_master WHERE name='t2'`,
		`PRAGMA writable_schema = RESET`,
	} {
		if _, _, eerr := nw.ExecArgs(s, nil); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	if got := queryOne(t, nw, `SELECT name FROM sqlite_master`); got != "t1" {
		t.Errorf("after deleting t2's row the catalog is %q, want t1", got)
	}
	p, perr := nw.ReadPager()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, qerr := p.QueryArgs(`SELECT * FROM t2`, nil); qerr == nil {
		t.Errorf("t2 still resolves after its catalog row was deleted")
	}

	// THE SHAPE THIS FORMAT HAS NO SPELLING FOR: a rootpage naming no object's
	// storage. The UPDATE is accepted, as C accepts it; the RESET that would load
	// it declines with a clean error saying "not reproducible against C SQLite"
	// (C reads an interior page or fails past its end of file, depending on a page
	// layout this format does not have), so the harness books it out of scope.
	if _, _, eerr := nw.ExecArgs(`PRAGMA writable_schema = ON`, nil); eerr != nil {
		t.Fatal(eerr)
	}
	if _, _, eerr := nw.ExecArgs(`UPDATE sqlite_master SET rootpage=4 WHERE name='t1'`, nil); eerr != nil {
		t.Fatalf("the rootpage UPDATE itself was refused; C accepts it: %v", eerr)
	}
	_, _, rerr := nw.ExecArgs(`PRAGMA writable_schema = RESET`, nil)
	if rerr == nil || !strings.Contains(rerr.Error(), "not reproducible against C SQLite") {
		t.Errorf("RESET over rootpage=4: want the out-of-scope decline, got %v", rerr)
	}
	// Put t1's own rootpage back (a no-op edit), so what persists below is the
	// sql edit alone.
	for _, s := range []string{
		`UPDATE sqlite_master SET rootpage=2 WHERE name='t1'`,
		`PRAGMA writable_schema = OFF`,
	} {
		if _, _, eerr := nw.ExecArgs(s, nil); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}

	// AND IT PERSISTS. The edited catalog is what the next open reads, because
	// the reload left the session holding the edited objects and the commit
	// writes the directory from those.
	if cerr := nw.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	re, oerr := OpenWrite(path)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer re.Discard()
	if got := queryOne(t, re, `SELECT type, name, sql FROM sqlite_master`); got != "table|t1|CREATE TABLE t1(a,b)" {
		t.Errorf("after the reopen the catalog is %q", got)
	}
	if got := queryOne(t, re, `SELECT * FROM t1`); got != "x|y" {
		t.Errorf("after the reopen SELECT * FROM t1 = %q, want x|y", got)
	}
}

// TestApplicationWordsSurviveAReopen pins the two header words an APPLICATION
// owns: "PRAGMA user_version" and "PRAGMA application_id".
//
// C SQLite keeps them in the file header (offsets 60 and 68). This format has
// no header, so they live in the catalog (ConvertedCatalog.UserVersion) -- and
// before they did, "PRAGMA user_version=11" was forgotten at the next open, which
// is losing the application's own data: a migration number is the standard thing
// stored there, so every open would have re-run every migration.
func TestApplicationWordsSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.musq")
	nw, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a)`,
		`PRAGMA user_version = 11`,
		`PRAGMA application_id = 252`,
	} {
		if _, _, eerr := nw.ExecArgs(s, nil); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	if got := queryOne(t, nw, `SELECT * FROM pragma_user_version`); got != "11" {
		t.Errorf("in the session: user_version = %q", got)
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	re, oerr := OpenWrite(path)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer re.Discard()
	if got := queryOne(t, re, `SELECT * FROM pragma_user_version`); got != "11" {
		t.Errorf("after the reopen: user_version = %q, want 11", got)
	}
	if got := queryOne(t, re, `SELECT * FROM pragma_application_id`); got != "252" {
		t.Errorf("after the reopen: application_id = %q, want 252", got)
	}
}

// TestRecordedHeaderPropertiesSurviveAReopen pins the three properties C SQLite
// keeps in its file header and this format keeps in its catalog: the text
// ENCODING, the PAGE SIZE a database was created at, and the AUTO_VACUUM mode.
//
// All three used to DECLINE, described as things "the format does not have" --
// which was wrong: our format can carry anything the C SQLite format carries.
// None of them is a page. Each is a value a database
// carries, the next connection reads back, and a pragma answers; the only reason
// they were missing is that nobody had written the field.
//
// The encoding is the load-bearing one, because it is not just a value: a UTF-16
// database STORES its text in UTF-16 here, and `hex(CAST(a AS BLOB))` over 'héllo'
// answers 6800E9006C006C006F00 exactly as C does (verified against the oracle in
// all three encodings, and after a reopen).
func TestRecordedHeaderPropertiesSurviveAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.musq")
	nw, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		// Every one of these has to run BEFORE the schema exists, which is C's own
		// rule for all three.
		`PRAGMA page_size = 8192`,
		`PRAGMA encoding = 'UTF-16le'`,
		`PRAGMA auto_vacuum = 2`,
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES('héllo')`,
	} {
		if _, _, eerr := nw.ExecArgs(s, nil); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	want := map[string]string{
		`PRAGMA page_size`: "8192",
		`PRAGMA encoding`:  "UTF-16le",
		// auto_vacuum has no eponymous table function in this engine (nor in C's
		// list), so the pragma itself is the probe.
		`PRAGMA auto_vacuum`: "2",
		// The STORED BYTES, which is what makes the encoding real rather than a
		// label: 'héllo' is 6800E9006C006C006F00 in UTF-16le.
		"SELECT hex(CAST(a AS BLOB)) FROM t": "6800E9006C006C006F00",
	}
	for q, w := range want {
		if got := queryOne(t, nw, q); got != w {
			t.Errorf("in the session: %s = %q, want %q", q, got, w)
		}
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	re, oerr := OpenWrite(path)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer re.Discard()
	for q, w := range want {
		if got := queryOne(t, re, q); got != w {
			t.Errorf("after the reopen: %s = %q, want %q", q, got, w)
		}
	}
}
