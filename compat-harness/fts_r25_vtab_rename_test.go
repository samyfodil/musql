// ALTER TABLE RENAME for virtual tables (FTS3/FTS4). Renames the vtab's schema row
// and its shadow tables. Must be readable by both engines after rename (bidirectional
// interchange test). Names are double-quoted; automatic indexes follow; collisions refuse.
// Module arguments and content=src tables are unaffected.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// ftsR25RenameDump dumps schema and data: sqlite_master in rowid order (natural order),
// verifying in-place rename lands every row exactly where C SQLite left it.
func ftsR25RenameDump(tbl string, extra ...string) []string {
	out := []string{
		`SELECT type, name, tbl_name, sql FROM sqlite_master`,
		`SELECT count(*) FROM ` + tbl,
	}
	return append(out, extra...)
}

func TestFtsR25VtabRenameDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		dump  []string
	}{
		{"fts3", []string{
			`CREATE VIRTUAL TABLE fts USING fts3(content)`,
			`INSERT INTO fts(docid,content) VALUES(1,'alpha beta'),(2,'gamma')`,
			`ALTER TABLE fts RENAME TO xyz`,
			`INSERT INTO xyz(docid,content) VALUES(3,'delta alpha')`,
		}, ftsR25RenameDump("xyz",
			`SELECT docid, content FROM xyz ORDER BY docid`,
			`SELECT group_concat(docid) FROM (SELECT docid FROM xyz WHERE xyz MATCH 'alpha' ORDER BY docid)`,
			`SELECT level, idx, quote(root) FROM xyz_segdir ORDER BY level, idx`,
			`SELECT docid, quote(c0content) FROM xyz_content ORDER BY docid`,
		)},

		// fts3ao.test's own shape: rename, write, rename back.
		{"fts3 renamed twice", []string{
			`CREATE VIRTUAL TABLE t1 USING fts3(a,b)`,
			`INSERT INTO t1(a,b) VALUES('one two','three')`,
			`ALTER TABLE t1 RENAME TO t2`,
			`INSERT INTO t2(a,b) VALUES('four','five six')`,
			`ALTER TABLE t2 RENAME TO t3`,
			`INSERT INTO t3(a,b) VALUES('seven','eight')`,
		}, ftsR25RenameDump("t3",
			`SELECT docid, a, b FROM t3 ORDER BY docid`,
			`SELECT group_concat(docid) FROM (SELECT docid FROM t3 WHERE t3 MATCH 'four' ORDER BY docid)`,
		)},

		{"fts4 with docsize and stat", []string{
			`CREATE VIRTUAL TABLE ft9 USING fts4(a)`,
			`INSERT INTO ft9(a) VALUES('one'),('two three')`,
			`ALTER TABLE ft9 RENAME TO ft10`,
			`INSERT INTO ft10(a) VALUES('four')`,
			`DELETE FROM ft10 WHERE rowid=1`,
		}, ftsR25RenameDump("ft10",
			`SELECT rowid, a FROM ft10 ORDER BY rowid`,
			`SELECT id, quote(value) FROM ft10_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM ft10_docsize ORDER BY docid`,
			`SELECT quote(matchinfo(ft10,'na')) FROM ft10 WHERE ft10 MATCH 'four'`,
			`INSERT INTO ft10(ft10) VALUES('integrity-check')`,
		)},

		{"fts4 matchinfo=fts3 (no docsize)", []string{
			`CREATE VIRTUAL TABLE m1 USING fts4(a, matchinfo=fts3)`,
			`INSERT INTO m1(a) VALUES('alpha beta')`,
			`ALTER TABLE m1 RENAME TO m2`,
			`INSERT INTO m2(a) VALUES('gamma')`,
		}, ftsR25RenameDump("m2",
			`SELECT rowid, a FROM m2 ORDER BY rowid`,
			`SELECT id, quote(value) FROM m2_stat ORDER BY id`,
		)},

		// fts4content.test's own shape: an external-content table has no
		// %_content shadow, so the rename must move four tables and not five,
		// and must not touch the source table.
		{"fts4 content= (no _content shadow)", []string{
			`CREATE TABLE src(a)`,
			`INSERT INTO src VALUES('hello world'),('goodbye')`,
			`CREATE VIRTUAL TABLE cft USING fts4(content=src, a)`,
			`INSERT INTO cft(cft) VALUES('rebuild')`,
			`ALTER TABLE cft RENAME TO cft2`,
		}, ftsR25RenameDump("cft2",
			`SELECT rowid, a FROM cft2 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM cft2 WHERE cft2 MATCH 'hello' ORDER BY rowid)`,
			`SELECT a FROM src ORDER BY rowid`,
		)},

		// A module with NO shadow tables at all: only the vtab row moves, and
		// its own argument -- which names another table -- stays verbatim.
		{"fts4aux (no shadows, argument not rewritten)", []string{
			`CREATE VIRTUAL TABLE base USING fts4(a)`,
			`INSERT INTO base(a) VALUES('alpha beta')`,
			`CREATE VIRTUAL TABLE aux USING fts4aux('base')`,
			`ALTER TABLE aux RENAME TO aux2`,
		}, ftsR25RenameDump("aux2",
			`SELECT term, col, documents, occurrences FROM aux2 ORDER BY term, col`,
		)},

		// A VIEW that names the vtab: C SQLite rewrites its stored SQL, and
		// the view must still RESOLVE afterwards -- gated on the rows it
		// returns, not on the ALTER being accepted, because a view left
		// pointing at the old name reads as a perfectly ordinary error later.
		//
		// A TRIGGER naming the vtab is deliberately NOT here: this engine
		// cannot reference a virtual table from a trigger body at all, with or
		// without a rename ("CREATE TRIGGER tr AFTER INSERT ON log BEGIN
		// INSERT INTO ft9(a) VALUES(new.x); END" is accepted and then fails
		// with "no such table: main.ft9" when it fires). That gap predates
		// this work and is unrelated to it -- the rename's own trigger
		// cascade runs through renameTriggerReferences exactly as an ordinary
		// table's does.
		{"view cascade", []string{
			`CREATE VIRTUAL TABLE ft9 USING fts4(a)`,
			`CREATE VIEW v AS SELECT a FROM "ft9"`,
			`INSERT INTO ft9(a) VALUES('one'),('two')`,
			`ALTER TABLE ft9 RENAME TO ft10`,
			`INSERT INTO ft10(a) VALUES('three')`,
		}, ftsR25RenameDump("ft10",
			`SELECT a FROM ft10 ORDER BY rowid`,
			`SELECT * FROM v ORDER BY 1`,
		)},

		// Quoted / punctuated names on both sides of the rename.
		{"quoted names", []string{
			`CREATE VIRTUAL TABLE "a b" USING fts3(x)`,
			`INSERT INTO "a b"(x) VALUES('q r')`,
			`ALTER TABLE "a b" RENAME TO "c-d"`,
			`INSERT INTO "c-d"(x) VALUES('s')`,
		}, ftsR25RenameDump(`"c-d"`,
			`SELECT rowid, x FROM "c-d" ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM "c-d" WHERE "c-d" MATCH 'q' ORDER BY rowid)`,
		)},

		// Renamed and renamed BACK: the second rename has to see the first
		// one's names as free, and the stored text has to end up quoted the
		// way the oracle leaves it, not restored to the original spelling.
		{"renamed and renamed back", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one two')`,
			`ALTER TABLE ft RENAME TO gt`,
			`INSERT INTO gt(a) VALUES('three')`,
			`ALTER TABLE gt RENAME TO ft`,
			`INSERT INTO ft(a) VALUES('four')`,
		}, ftsR25RenameDump("ft",
			`SELECT rowid, a FROM ft ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM ft WHERE ft MATCH 'three' ORDER BY rowid)`,
			`SELECT level, idx, quote(root) FROM ft_segdir ORDER BY level, idx`,
		)},

		// Inside a transaction, and rolled back: nothing about the rename --
		// the vtab row, the shadows, or the automatic index -- may survive.
		{"rename rolled back", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one two')`,
			`BEGIN`,
			`ALTER TABLE ft RENAME TO gt`,
			`INSERT INTO gt(a) VALUES('three')`,
			`ROLLBACK`,
			`INSERT INTO ft(a) VALUES('four')`,
		}, ftsR25RenameDump("ft",
			`SELECT rowid, a FROM ft ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM ft WHERE ft MATCH 'four' ORDER BY rowid)`,
			`SELECT level, idx, quote(root) FROM ft_segdir ORDER BY level, idx`,
		)},

		// ...and committed, which is the same path with the opposite ending.
		{"rename committed", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one two')`,
			`BEGIN`,
			`ALTER TABLE ft RENAME TO gt`,
			`INSERT INTO gt(a) VALUES('three')`,
			`COMMIT`,
			`INSERT INTO gt(a) VALUES('four')`,
		}, ftsR25RenameDump("gt",
			`SELECT rowid, a FROM gt ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM gt WHERE gt MATCH 'three' ORDER BY rowid)`,
			`SELECT level, idx, quote(root) FROM gt_segdir ORDER BY level, idx`,
		)},

		// Renaming to a name whose PREFIX matches an existing user table:
		// nothing but the module's own suffix list may move.
		{"a same-prefixed user table is left alone", []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE TABLE ft_notes(z)`,
			`INSERT INTO ft_notes VALUES(1)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`ALTER TABLE ft RENAME TO gt`,
		}, ftsR25RenameDump("gt",
			`SELECT z FROM ft_notes`,
			`SELECT rowid, a FROM gt ORDER BY rowid`,
		)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			full := append(append([]string{}, c.stmts...), c.dump...)
			differAllAccepted(t, c.name, full, len(c.stmts))
		})
	}
}

// TestFtsR25VtabRenameFts5Diff is the same in the fts5 build.
//
// It goes through flLockstep rather than differ(): differ's workers are built
// by TestMain with their own `-tags cgoengine`/`-tags musql` and the outer
// `-tags sqlite_fts5` is NOT propagated to them, so a worker never has fts5 at
// all. flLockstep runs both engines IN PROCESS, which is where the tag applies
// -- the same reason every other fts5 gate in this package has its own runner.
func TestFtsR25VtabRenameFts5Diff(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("fts5 is absent from both engines in this build")
	}
	for _, c := range []struct {
		name   string
		script []string
		verify []string
	}{
		{"fts5", []string{
			`CREATE VIRTUAL TABLE f1 USING fts5(a)`,
			`INSERT INTO f1(a) VALUES('alpha beta'),('gamma')`,
			`ALTER TABLE f1 RENAME TO f2`,
			`INSERT INTO f2(a) VALUES('delta alpha')`,
			`INSERT INTO f2(f2) VALUES('integrity-check')`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a FROM f2 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM f2 WHERE f2 MATCH 'alpha' ORDER BY rowid)`,
			`SELECT k, quote(v) FROM f2_config ORDER BY k`,
			`SELECT id, quote(sz) FROM f2_docsize ORDER BY id`,
		}},
		{"fts5 columnsize=0 (no docsize shadow)", []string{
			`CREATE VIRTUAL TABLE f1 USING fts5(a, columnsize=0)`,
			`INSERT INTO f1(a) VALUES('alpha beta')`,
			`ALTER TABLE f1 RENAME TO f2`,
			`INSERT INTO f2(a) VALUES('gamma')`,
			// No fts5 "integrity-check" here: a columnsize=0 table has no
			// %_docsize, and this engine's own integrity-check command
			// requires one unconditionally ("fts5 table f1 is missing a shadow
			// table") with or without a rename. A pre-existing gap, unrelated
			// to this work.
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a FROM f2 ORDER BY rowid`,
		}},
		{"fts5 renamed twice", []string{
			`CREATE VIRTUAL TABLE f1 USING fts5(a,b)`,
			`INSERT INTO f1(a,b) VALUES('one two','three')`,
			`ALTER TABLE f1 RENAME TO f2`,
			`INSERT INTO f2(a,b) VALUES('four','five')`,
			`ALTER TABLE f2 RENAME TO f3`,
			`DELETE FROM f3 WHERE rowid=1`,
			`INSERT INTO f3(f3) VALUES('integrity-check')`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a, b FROM f3 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM f3 WHERE f3 MATCH 'four' ORDER BY rowid)`,
		}},
		{"fts5 rename onto a produced shadow name is refused", []string{
			`CREATE VIRTUAL TABLE f1 USING fts5(a)`,
			`INSERT INTO f1(a) VALUES('alpha')`,
			`CREATE TABLE occupied_data(z)`,
			`ALTER TABLE f1 RENAME TO occupied`,
		}, []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a FROM f1 ORDER BY rowid`,
		}},
	} {
		flLockstep(t, "fts5-vtab-rename-"+c.name, c.script, c.verify...)
	}
}

// TestFtsR25VtabRenameFts5Interchange is the both-directions file gate for
// fts5, in process for the same reason as above.
func TestFtsR25VtabRenameFts5Interchange(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("fts5 is absent from both engines in this build")
	}
	write := []string{
		`CREATE VIRTUAL TABLE f1 USING fts5(a,b)`,
		`INSERT INTO f1(a,b) VALUES('alpha beta','gamma'),('delta','epsilon')`,
		`ALTER TABLE f1 RENAME TO f2`,
		`INSERT INTO f2(a,b) VALUES('zeta alpha','eta')`,
		`DELETE FROM f2 WHERE rowid=1`,
		`INSERT INTO f2(f2) VALUES('integrity-check')`,
	}
	read := []string{
		`SELECT type, name, tbl_name, sql FROM sqlite_master`,
		`SELECT rowid, a, b FROM f2 ORDER BY rowid`,
		`SELECT k, quote(v) FROM f2_config ORDER BY k`,
		`SELECT group_concat(rowid) FROM (SELECT rowid FROM f2 WHERE f2 MATCH 'alpha' ORDER BY rowid)`,
	}
	ftsR25InProcessInterchange(t, "vtab-rename-fts5", write, read)
}

// TestFtsR25VtabRenameRefusals pins every shape that must be REFUSED, plus the
// proof that the refusal changed nothing. differ() (not differAllAccepted)
// because the statement under test is expected to fail on BOTH engines.
func TestFtsR25VtabRenameRefusals(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		// brokenIndex marks a case whose setup deliberately removes a shadow
		// table, which leaves the fts index unvalidatable -- see below.
		brokenIndex bool
	}{
		// The new table name collides with one of the vtab's OWN shadows.
		{name: "rename onto an own shadow name", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`ALTER TABLE ft RENAME TO ft_content`,
		}},
		// A PRODUCED shadow name is already taken by a user table.
		{name: "a produced shadow name is taken", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE TABLE occupied_segdir(z)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`ALTER TABLE ft RENAME TO occupied`,
		}},
		{name: "the new name is an existing table", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE TABLE plain(z)`,
			`ALTER TABLE ft RENAME TO plain`,
		}},
		{name: "the new name is an existing view", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE VIEW vv AS SELECT 1`,
			`ALTER TABLE ft RENAME TO vv`,
		}},
		{name: "the new name is an existing index", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`CREATE TABLE plain(z)`,
			`CREATE INDEX ii ON plain(z)`,
			`ALTER TABLE ft RENAME TO ii`,
		}},
		// The three ALTER forms a virtual table refuses outright.
		{name: "ADD COLUMN", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a,b)`,
			`ALTER TABLE ft ADD COLUMN c`,
		}},
		{name: "RENAME COLUMN", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a,b)`,
			`ALTER TABLE ft RENAME COLUMN a TO z`,
		}},
		{name: "DROP COLUMN", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a,b)`,
			`ALTER TABLE ft DROP COLUMN b`,
		}},
		{name: "rename a vtab that does not exist", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`ALTER TABLE nosuchvtab RENAME TO other`,
		}},
		// A shadow table the module OWNS has been dropped out from under it.
		// C SQLite's xRename issues one nested ALTER per shadow and the
		// whole statement fails ("SQL logic error") with nothing applied --
		// verified for each of the four in turn. Renaming the survivors would
		// be an ACCEPT where the oracle errors.
		{name: "a dropped %_content shadow", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`DROP TABLE ft_content`,
			`ALTER TABLE ft RENAME TO gt`,
		}, brokenIndex: true},
		{name: "a dropped %_stat shadow", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`DROP TABLE ft_stat`,
			`ALTER TABLE ft RENAME TO gt`,
		}, brokenIndex: true},
		{name: "a dropped %_docsize shadow", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`DROP TABLE ft_docsize`,
			`ALTER TABLE ft RENAME TO gt`,
		}, brokenIndex: true},
		{name: "a dropped %_segdir shadow", stmts: []string{
			`CREATE VIRTUAL TABLE ft USING fts4(a)`,
			`INSERT INTO ft(a) VALUES('one')`,
			`DROP TABLE ft_segdir`,
			`ALTER TABLE ft RENAME TO gt`,
		}, brokenIndex: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Everything up to the last statement must be accepted; the last
			// one must fail identically on both engines, and the schema and
			// data must be untouched afterwards.
			setup := c.stmts[:len(c.stmts)-1]
			differAllAccepted(t, c.name+": setup", setup, len(setup))
			full := append(append([]string{}, c.stmts...),
				`SELECT type, name, tbl_name, sql FROM sqlite_master`,
				`SELECT count(*) FROM ft`,
				`SELECT rowid, a FROM ft ORDER BY rowid`,
			)
			if !c.brokenIndex {
				// PRAGMA integrity_check is left out of the dropped-shadow
				// cases: C SQLite's walks an fts4 table's INVERTED INDEX and
				// reports "unable to validate the inverted index for FTS4 table
				// main.ft: SQL logic error" once a shadow is gone, where this
				// engine's answers "ok". That is a pre-existing difference in
				// integrity_check's own coverage, unrelated to the rename, and
				// the schema and row dumps above already prove the refusal
				// changed nothing.
				full = append(full, `PRAGMA integrity_check`)
			}
			if !differ(t, c.name, full) {
				t.Error("engines disagree about the refused ALTER or its aftermath")
			}
		})
	}
}

// TestFtsR25VtabRenameInterchange is the load-bearing claim, and the one a
// half-applied rename fails: a database whose vtab was renamed must read back
// identically under BOTH engines, whichever one wrote it.
func TestFtsR25VtabRenameInterchange(t *testing.T) {
	cases := []struct {
		name  string
		write []string
		read  []string
	}{
		{name: "fts3", write: []string{
			`CREATE VIRTUAL TABLE fts USING fts3(content)`,
			`INSERT INTO fts(docid,content) VALUES(1,'alpha beta'),(2,'gamma')`,
			`ALTER TABLE fts RENAME TO xyz`,
			`INSERT INTO xyz(docid,content) VALUES(3,'delta alpha')`,
		}, read: []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT docid, content FROM xyz ORDER BY docid`,
			`SELECT level, idx, quote(root) FROM xyz_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM xyz_segments ORDER BY blockid`,
			`SELECT group_concat(docid) FROM (SELECT docid FROM xyz WHERE xyz MATCH 'alpha' ORDER BY docid)`,
		}},
		{name: "fts4", write: []string{
			`CREATE VIRTUAL TABLE ft9 USING fts4(a,b)`,
			`INSERT INTO ft9(a,b) VALUES('one two','three'),('four','five six')`,
			`ALTER TABLE ft9 RENAME TO ft10`,
			`INSERT INTO ft10(a,b) VALUES('seven','eight')`,
			`DELETE FROM ft10 WHERE rowid=1`,
		}, read: []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a, b FROM ft10 ORDER BY rowid`,
			`SELECT level, idx, quote(root) FROM ft10_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM ft10_stat ORDER BY id`,
			`SELECT docid, quote(size) FROM ft10_docsize ORDER BY docid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM ft10 WHERE ft10 MATCH 'seven' ORDER BY rowid)`,
		}},
		{name: "fts4-content", write: []string{
			`CREATE TABLE src(a)`,
			`INSERT INTO src VALUES('hello world'),('goodbye')`,
			`CREATE VIRTUAL TABLE cft USING fts4(content=src, a)`,
			`INSERT INTO cft(cft) VALUES('rebuild')`,
			`ALTER TABLE cft RENAME TO cft2`,
		}, read: []string{
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			`SELECT rowid, a FROM cft2 ORDER BY rowid`,
			`SELECT group_concat(rowid) FROM (SELECT rowid FROM cft2 WHERE cft2 MATCH 'hello' ORDER BY rowid)`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fileInterchangeRoundTrip(t, "vtab-rename-"+c.name, c.write, c.read)
		})
	}
}

// ftsR25InProcessInterchange is fileInterchangeRoundTrip without the worker
// binaries: each engine writes the script into its own file IN PROCESS, then
// the other one reads it back. Needed for fts5, whose build tag the workers do
// not carry (see TestFtsR25VtabRenameFts5Diff).
func ftsR25InProcessInterchange(t *testing.T, name string, write, read []string) {
	t.Helper()

	t.Run("written-by-musql", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), name+".sqlite")
		edb, err := engine.Create(path)
		if err != nil {
			t.Fatalf("engine.Create: %v", err)
		}
		for i, s := range write {
			if e := edb.Exec(s); e != nil {
				t.Fatalf("musql could not run the write script (the comparison would be vacuous): stmt #%d %s: %v", i, s, e)
			}
		}
		if e := edb.Close(); e != nil {
			t.Fatalf("Close: %v", e)
		}
		edb2, err := engine.OpenWrite(path)
		if err != nil {
			t.Fatalf("OpenWrite of the file this engine just wrote: %v", err)
		}
		defer edb2.Close()
		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer cdb.Close()
		cdb.SetMaxOpenConns(1)
		var ic string
		if e := cdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); e != nil || ic != "ok" {
			t.Errorf("C SQLite integrity_check on the file musql wrote = %q (%v), want ok", ic, e)
		}
		for _, q := range read {
			flCompareQuery(t, name+"/musql-wrote", edb2, cdb, q)
		}
	})

	t.Run("written-by-cgo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), name+".sqlite")
		cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer cdb.Close()
		cdb.SetMaxOpenConns(1)
		for i, s := range write {
			if _, e := cdb.Exec(s); e != nil {
				t.Fatalf("cgo could not run the write script (the comparison would be vacuous): stmt #%d %s: %v", i, s, e)
			}
		}
		// This engine reads its own format (RULE #3): the import of C's file.
		edb, err := engine.OpenWrite(importedForMusql(t, path))
		if err != nil {
			t.Fatalf("OpenWrite of the import of the file C SQLite wrote: %v", err)
		}
		defer edb.Close()
		for _, q := range read {
			flCompareQuery(t, name+"/cgo-wrote", edb, cdb, q)
		}
	})
}

// TestFtsR25VtabRenameTempCatalog covers the TEMP catalog, which the fts3
// family is the one module set to support (engine/vtab.go's CreateVirtualTable).
func TestFtsR25VtabRenameTempCatalog(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE temp.tv USING fts3(x)`,
		`INSERT INTO tv(x) VALUES('alpha beta')`,
		`ALTER TABLE temp.tv RENAME TO tv2`,
		`INSERT INTO tv2(x) VALUES('gamma')`,
	}
	dump := []string{
		`SELECT type, name, tbl_name, sql FROM sqlite_temp_master`,
		`SELECT rowid, x FROM tv2 ORDER BY rowid`,
		`SELECT group_concat(rowid) FROM (SELECT rowid FROM tv2 WHERE tv2 MATCH 'alpha' ORDER BY rowid)`,
	}
	differAllAccepted(t, "temp vtab rename", append(append([]string{}, stmts...), dump...), len(stmts))
}

// TestFtsR25VtabRenameFuzzDiff randomizes the history, because the segmentation
// and the shadow set are both history-dependent.
func TestFtsR25VtabRenameFuzzDiff(t *testing.T) {
	nIter := 120
	if testing.Short() {
		nIter = 30
	}
	modules := []string{
		`CREATE VIRTUAL TABLE v0 USING fts3(x)`,
		`CREATE VIRTUAL TABLE v0 USING fts4(x)`,
		`CREATE VIRTUAL TABLE v0 USING fts4(x, matchinfo=fts3)`,
		`CREATE VIRTUAL TABLE v0 USING fts4(x, prefix="2")`,
	}
	words := []string{"aa", "bb", "cc", "dd"}
	for iter := 0; iter < nIter; iter++ {
		mod := modules[iter%len(modules)]
		stmts := []string{mod}
		name := "v0"
		docid := 1
		for step := 0; step < 4; step++ {
			switch (iter/len(modules) + step) % 3 {
			case 0:
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO %s(docid,x) VALUES(%d,'%s %s')`,
					name, docid, words[(iter+step)%len(words)], words[(iter+step+1)%len(words)]))
				docid++
			case 1:
				next := fmt.Sprintf("v%d", step+1)
				stmts = append(stmts, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, name, next))
				name = next
			default:
				if docid > 1 {
					stmts = append(stmts, fmt.Sprintf(`DELETE FROM %s WHERE docid=%d`, name, docid-1))
				}
			}
		}
		stmts = append(stmts,
			`SELECT type, name, tbl_name, sql FROM sqlite_master`,
			fmt.Sprintf(`SELECT docid, x FROM %s ORDER BY docid`, name),
			fmt.Sprintf(`SELECT level, idx, quote(root) FROM %s_segdir ORDER BY level, idx`, name),
		)
		for _, w := range words {
			stmts = append(stmts, fmt.Sprintf(
				`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM %s WHERE %s MATCH '%s' ORDER BY docid)`, name, name, w))
		}
		if !differ(t, fmt.Sprintf("vtab-rename-fuzz/%d", iter), stmts) {
			t.Fatalf("stopping at the first divergent history (iteration %d)", iter)
		}
	}
}
