package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSegmentFromScratch is the claim your direction needs: a database built
// entirely on this format, with no SQLite file involved at any point --
// not as a seed, not as a staging area.
//
// Every statement, DDL and rows alike, goes through the segment session, and the
// answers are compared against the same script run through a SQLite database.
func TestSegmentFromScratch(t *testing.T) {
	script := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`,
		`INSERT INTO t VALUES(10,100,'a')`,
		`INSERT INTO t VALUES(20,200,'b')`,
		`INSERT INTO t VALUES(30,300,'c')`,
		`CREATE TABLE other(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO other VALUES(1,'x')`,
		`CREATE INDEX t_k ON t(k)`,
		`CREATE VIEW v AS SELECT id, k FROM t WHERE k > 150`,
		`UPDATE t SET s = 'bb' WHERE id = 20`,
		`DELETE FROM t WHERE id = 30`,
		`INSERT INTO t VALUES(40,400,'d')`,
	}
	queries := []string{
		`SELECT id, k, s FROM t ORDER BY id`,
		`SELECT count(*) FROM t`,
		`SELECT sum(k) FROM t`,
		`SELECT id, k FROM v ORDER BY id`,
		`SELECT b FROM other`,
		`SELECT id FROM t WHERE k = 200`,
		`SELECT type, name FROM sqlite_master ORDER BY type, name`,
	}

	dir := t.TempDir()
	segPath := filepath.Join(dir, "scratch.musq")
	nw, err := Create(segPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range script {
		if eerr := nw.Exec(s); eerr != nil {
			nw.Discard()
			t.Fatalf("segment %q: %v", s, eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			nw.Discard()
			t.Fatalf("commit after %q: %v", s, cerr)
		}
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// NOTHING but the segment file exists in this directory bar its delta and its
	// lock. The lock is a file of its own on purpose -- a byte-range lock belongs to
	// an inode and a rewrite RENAMES the segment file, so a lock taken on it stops
	// protecting anything the moment the rename lands (see openSegmentLockFile).
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		switch e.Name() {
		case "scratch.musq", "scratch.musq.delta", "scratch.musq.lock":
		default:
			t.Errorf("an unexpected file was produced: %s", e.Name())
		}
	}

	// The oracle: the same script through a SQLite database.
	oracle := filepath.Join(t.TempDir(), "o.musq")
	odb, oerr := Create(oracle)
	if oerr != nil {
		t.Fatal(oerr)
	}
	for _, s := range script {
		if eerr := odb.Exec(s); eerr != nil {
			t.Fatalf("oracle %q: %v", s, eerr)
		}
	}
	if cerr := odb.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatalf("Open: %v", nerr)
	}
	defer np.Close()
	rp, rerr := Open(oracle)
	if rerr != nil {
		t.Fatal(rerr)
	}
	defer rp.Close()
	for _, q := range queries {
		got, want := renderRows(t, np, q), renderRows(t, rp, q)
		if got != want {
			t.Errorf("%s\n segment: %s\n SQLite: %s", q, got, want)
		}
	}
}

// TestSegmentDDLAfterRowsKeepsThem is the ordering hazard a file rewrite has to
// survive: rows written into the delta, THEN a catalog change. The rewrite folds
// the log in, and a record that named a table by its directory index must not be
// left pointing at a different table.
func TestSegmentDDLAfterRowsKeepsThem(t *testing.T) {
	dir := t.TempDir()
	segPath := filepath.Join(dir, "d.musq")
	nw, err := Create(segPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)`,
	} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatal(eerr)
		}
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	// Rows into BOTH tables, in the delta.
	for _, s := range []string{
		`INSERT INTO a VALUES(1,'a1')`,
		`INSERT INTO b VALUES(1,'b1')`,
		`INSERT INTO b VALUES(2,'b2')`,
	} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	// ...then a DROP, which moves every table index after it.
	if eerr := nw.Exec(`DROP TABLE a`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	// ...and more rows, which must land in the RIGHT table.
	if eerr := nw.Exec(`INSERT INTO b VALUES(3,'b3')`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np.Close()
	if got := renderRows(t, np, `SELECT id, v FROM b ORDER BY id`); got != "1:1|3:\"b1\"|;1:2|3:\"b2\"|;1:3|3:\"b3\"|" {
		t.Errorf("b = %q -- a delta record survived a directory move and landed in the wrong table", got)
	}
	if got := renderRows(t, np, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`); got != "3:\"b\"|" {
		t.Errorf("tables = %q, want just b", got)
	}
}

// TestCreateSegmentIsEmptyAndUsable pins the empty file: it opens, holds nothing,
// and accepts a first table.
func TestCreateSegmentIsEmptyAndUsable(t *testing.T) {
	segPath := filepath.Join(t.TempDir(), "e.musq")
	if err := CreateFile(segPath); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	names, err := Tables(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("a fresh segment database holds tables: %v", names)
	}
	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatalf("Open on an empty database: %v", nerr)
	}
	if got := renderRows(t, np, `SELECT count(*) FROM sqlite_master`); got != "1:0|" {
		t.Errorf("sqlite_master of an empty database = %q, want 0 rows", got)
	}
	np.Close()

	nw, werr := OpenWrite(segPath)
	if werr != nil {
		t.Fatal(werr)
	}
	if eerr := nw.Exec(`CREATE TABLE first(x INTEGER)`); eerr != nil {
		t.Fatalf("CREATE TABLE on an empty segment database: %v", eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	names2, err2 := Tables(segPath)
	if err2 != nil {
		t.Fatal(err2)
	}
	if len(names2) != 1 || names2[0] != "first" {
		t.Errorf("tables after the first CREATE = %v, want [first]", names2)
	}
}

var _ = fmt.Sprintf

// TestSegmentDDLKeepsUntouchedTables is the rewrite's quietest failure mode: it
// writes every table out from its row store, so a table this session never
// LOADED would be written EMPTY -- its rows silently gone, with no error
// anywhere. The tables in a test that also writes to them are already loaded,
// which is why this one deliberately touches only one of two.
func TestSegmentDDLKeepsUntouchedTables(t *testing.T) {
	dir := t.TempDir()
	segPath := filepath.Join(dir, "u.musq")
	nw, err := Create(segPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE touched(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE untouched(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO touched VALUES(1,'t1'),(2,'t2')`,
		`INSERT INTO untouched VALUES(1,'u1'),(2,'u2'),(3,'u3')`,
	} {
		if eerr := nw.Exec(s); eerr != nil {
			t.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}
	if cerr := nw.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// A FRESH session that touches only `touched`, then does DDL -- so the rewrite
	// has to load `untouched` itself.
	nw2, w2 := OpenWrite(segPath)
	if w2 != nil {
		t.Fatal(w2)
	}
	if eerr := nw2.Exec(`UPDATE touched SET v = 'changed' WHERE id = 1`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw2.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if eerr := nw2.Exec(`CREATE INDEX i ON touched(v)`); eerr != nil {
		t.Fatal(eerr)
	}
	if _, cerr := nw2.Commit(); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := nw2.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	np, nerr := Open(segPath)
	if nerr != nil {
		t.Fatal(nerr)
	}
	defer np.Close()
	if got := renderRows(t, np, `SELECT id, v FROM untouched ORDER BY id`); got != `1:1|3:"u1"|;1:2|3:"u2"|;1:3|3:"u3"|` {
		t.Errorf("untouched = %q -- the DDL rewrite wrote a table it had not loaded, "+
			"so its rows are gone with no error anywhere", got)
	}
	if got := renderRows(t, np, `SELECT id, v FROM touched ORDER BY id`); got != `1:1|3:"changed"|;1:2|3:"t2"|` {
		t.Errorf("touched = %q", got)
	}
}
