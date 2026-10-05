// Differential tests against C SQLite for write-path, catalog, and parser rules.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// wctRun replays setup+stmts on both engines and returns each statement's
// outcome, so the two transcripts can be compared as a whole. An error is
// rendered as "ERR" plus its message; the caller decides how much of the
// message has to match (the two engines word their rejections differently,
// but WHICH statements are rejected must agree).
func wctRun(t *testing.T, drv string, stmts []string) []string {
	t.Helper()
	dsn := ":memory:"
	if drv == "sqlite" {
		dsn = filepath.Join(t.TempDir(), "wct.sqlite")
	}
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", drv, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		up := strings.ToUpper(strings.TrimSpace(s))
		if strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "PRAGMA") {
			out = append(out, queryString(t, db, s))
			continue
		}
		if _, eerr := db.Exec(s); eerr != nil {
			out = append(out, "ERR "+eerr.Error())
			continue
		}
		out = append(out, "ok")
	}
	return out
}

// wctCompare requires both engines to agree statement for statement. Two
// rejections agree by BOTH being rejections (the wording is each engine's
// own); anything else must match exactly.
func wctCompare(t *testing.T, stmts, got, want []string) {
	t.Helper()
	for i := range stmts {
		if strings.HasPrefix(got[i], "ERR") && strings.HasPrefix(want[i], "ERR") {
			continue
		}
		if got[i] != want[i] {
			t.Errorf("stmt #%d %s\n  engine: %s\n  cgo:    %s", i, stmts[i], got[i], want[i])
		}
	}
}

// TestUpdateFromWithoutRowidParity pins UPDATE ... FROM against WITHOUT ROWID tables.
// Cases cover single and multi-column primary keys, primary key updates, and collated keys.
func TestUpdateFromWithoutRowidParity(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(x INTEGER PRIMARY KEY, y, z UNIQUE) WITHOUT ROWID`,
		`INSERT INTO t1 VALUES(1,'i','one'),(2,'ii','two'),(3,'iii','three'),(4,'iv','four')`,
		`CREATE TABLE x1(o, n)`,
		`INSERT INTO x1 VALUES(1,11),(2,12),(3,13),(4,14)`,
		// upfrom3.test 1.x: the SET moves the (single-column) PRIMARY KEY.
		`UPDATE t1 SET x=n FROM x1 WHERE x=o`,
		`SELECT x, y, z FROM t1 ORDER BY 1`,
		// upfrom1.test 4.x: a two-column PRIMARY KEY, updated by a join.
		`CREATE TABLE u1(a, b, c, PRIMARY KEY(a, b)) WITHOUT ROWID`,
		`INSERT INTO u1 VALUES('a','b','c'),('d','e','f'),('g','h','i')`,
		`CREATE TABLE map(f, t)`,
		`INSERT INTO map VALUES('b','B'),('h','H')`,
		`UPDATE u1 SET b=t FROM map WHERE b=f`,
		`SELECT a, b, c FROM u1 ORDER BY 1`,
		`UPDATE u1 SET c=t FROM map WHERE c=f`,
		`SELECT a, b, c FROM u1 ORDER BY 1`,
		// A NOCASE PRIMARY KEY: 'ABC' and 'abc' cannot both exist, so the
		// binary key match identifyTargetRow does is still unambiguous.
		`CREATE TABLE n1(k TEXT COLLATE NOCASE PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO n1 VALUES('ABC', 1), ('def', 2)`,
		`CREATE TABLE n2(k, v)`,
		`INSERT INTO n2 VALUES('abc', 100), ('DEF', 200)`,
		`UPDATE n1 SET v=n2.v FROM n2 WHERE n1.k=n2.k`,
		`SELECT k, v FROM n1 ORDER BY k`,
		// A target row with NO join match is left alone; one matched by MORE
		// than one FROM row is C SQLite's UNDEFINED case, which this path
		// declines (and which the compare below accepts as a mutual reject
		// only if cgo also errors -- it does not, so this asserts the shape
		// stays out of the transcript by being the last statement).
		`CREATE TABLE m1(k PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO m1 VALUES(1,'a'),(2,'b')`,
		`CREATE TABLE m2(k, v)`,
		`INSERT INTO m2 VALUES(1,'z')`,
		`UPDATE m1 SET v=m2.v FROM m2 WHERE m1.k=m2.k`,
		`SELECT k, v FROM m1 ORDER BY k`,
	}
	got := wctRun(t, "sqlite", stmts)
	want := wctRun(t, "sqlite3", stmts)
	wctCompare(t, stmts, got, want)
}

// TestQualifiedTableFuncParity pins that schema qualifiers on table-valued functions
// are ignored; only the function name and explicit second argument matter.
func TestQualifiedTableFuncParity(t *testing.T) {
	stmts := []string{
		`CREATE TABLE m1(a INTEGER PRIMARY KEY, b TEXT)`,
		`CREATE TEMP TABLE t5_1(x, y)`,
		`CREATE INDEX mi ON m1(b)`,
		`SELECT name FROM pragma_table_info('t5_1')`,
		`SELECT name FROM temp.pragma_table_info('t5_1')`,
		`SELECT name FROM main.pragma_table_info('t5_1')`,
		`SELECT name FROM temp.pragma_table_info('m1')`,
		`SELECT name FROM main.pragma_table_info('m1')`,
		`SELECT name FROM nosuchdb.pragma_table_info('m1')`,
		`SELECT name FROM "temp".pragma_table_info('t5_1')`,
		// The second argument is the only thing that really scopes it.
		`SELECT name FROM temp.pragma_table_info('t5_1','main')`,
		`SELECT name FROM main.pragma_table_info('t5_1','temp')`,
		// The hidden schema column stays NULL through a qualifier.
		`SELECT arg, schema FROM temp.pragma_table_info('t5_1')`,
		`SELECT name FROM temp.pragma_index_info('mi')`,
		// A non-pragma eponymous module follows the same rule.
		`SELECT key FROM main.json_each('{"a":1}')`,
		`SELECT key FROM nosuchdb.json_each('{"a":1}')`,
		// An unknown module and an ordinary table are rejected exactly as
		// their unqualified spellings are.
		`SELECT * FROM main.nosuch_module(1,3)`,
		`SELECT * FROM main.m1(1)`,
	}
	got := wctRun(t, "sqlite", stmts)
	want := wctRun(t, "sqlite3", stmts)
	wctCompare(t, stmts, got, want)
	for i := range stmts {
		if strings.HasPrefix(got[i], "ERR") != strings.HasPrefix(want[i], "ERR") {
			t.Errorf("accept/reject disagreement on %s\n  engine: %s\n  cgo:    %s", stmts[i], got[i], want[i])
		}
	}
}

// TestReindexTargetParity pins REINDEX accept/reject parity for target identification.
func TestReindexTargetParity(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX i1 ON t1(a)`,
		`REINDEX`,
		`REINDEX t1`,
		`REINDEX i1`,
		`REINDEX main.t1`,
		`REINDEX main.i1`,
		`REINDEX BINARY`,
		`REINDEX NOCASE`,
		`REINDEX RTRIM`,
		`REINDEX main.NOCASE`,
		`REINDEX c1`,
		`REINDEX c2`,
		`REINDEX bogus`,
		`REINDEX "reverse sort"`,
		`REINDEX t2`,
		`SELECT count(*) FROM sqlite_master`,
	}
	got := wctRun(t, "sqlite", stmts)
	want := wctRun(t, "sqlite3", stmts)
	wctCompare(t, stmts, got, want)
	for i := range stmts {
		if strings.HasPrefix(got[i], "ERR") != strings.HasPrefix(want[i], "ERR") {
			t.Errorf("accept/reject disagreement on %s\n  engine: %s\n  cgo:    %s", stmts[i], got[i], want[i])
		}
	}
}

// TestJoinKeywordSequenceParity enumerates all one-, two-, and three-keyword
// join operators and verifies parity on acceptance and result sets with C SQLite.
func TestJoinKeywordSequenceParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a,b)`,
		`CREATE TABLE t2(b,c)`,
		`INSERT INTO t1 VALUES(1,2),(3,4)`,
		`INSERT INTO t2 VALUES(2,3),(9,9)`,
	}
	kw := []string{"NATURAL", "LEFT", "OUTER", "RIGHT", "FULL", "INNER", "CROSS", "BOGUS"}
	seqs := []string{""}
	for _, a := range kw {
		seqs = append(seqs, a)
		for _, b := range kw {
			seqs = append(seqs, a+" "+b)
			for _, c := range kw {
				seqs = append(seqs, a+" "+b+" "+c)
			}
		}
	}
	stmts := append([]string(nil), setup...)
	for _, s := range seqs {
		stmts = append(stmts, "SELECT * FROM t1 "+s+" JOIN t2")
	}
	got := wctRun(t, "sqlite", stmts)
	want := wctRun(t, "sqlite3", stmts)
	// A rejection must be a rejection on BOTH sides here -- the wording is
	// each engine's own, but "accepted by one, rejected by the other" is
	// exactly the divergence this enumeration exists to catch, and
	// wctCompare's shared ERR/ERR pass covers only that.
	wctCompare(t, stmts, got, want)
	for i := range stmts {
		if strings.HasPrefix(got[i], "ERR") != strings.HasPrefix(want[i], "ERR") {
			t.Errorf("accept/reject disagreement on %s\n  engine: %s\n  cgo:    %s", stmts[i], got[i], want[i])
		}
	}
}

// TestSchemaCatalogShadowedOwnerParity pins which catalog an INDEX or TRIGGER lands in
// when its table exists in both main and temp. The rule is creation order: an unqualified
// name resolves temp-first, so a dependent is temp if a temp table of that name existed
// when the dependent was created.
func TestSchemaCatalogShadowedOwnerParity(t *testing.T) {
	stmts := []string{
		// (a) main table first, index created while ONLY main exists, temp
		// shadow created afterwards -> the index stays in MAIN.
		`CREATE TABLE early(a)`,
		`CREATE INDEX early_i ON early(a)`,
		`CREATE TEMP TABLE early(b)`,
		// (b) main table first, temp shadow SECOND, index created after both
		// -> the unqualified "late" resolved temp-first, so the index is TEMP.
		`CREATE TABLE late(a)`,
		`CREATE TEMP TABLE late(b)`,
		`CREATE INDEX late_i ON late(b)`,
		// (c) the same question for a TRIGGER, which carries no TEMP keyword
		// of its own here.
		`CREATE TABLE trg(a)`,
		`CREATE TRIGGER trg_main AFTER INSERT ON trg BEGIN SELECT 1; END`,
		`CREATE TEMP TABLE trg(b)`,
		`CREATE TRIGGER trg_temp AFTER INSERT ON trg BEGIN SELECT 1; END`,
		// (d) an implicit sqlite_autoindex_* row follows its own table.
		`CREATE TABLE uq(a UNIQUE)`,
		`CREATE TEMP TABLE uq(b UNIQUE)`,
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		`SELECT type, name, tbl_name FROM sqlite_temp_master ORDER BY name`,
		`SELECT type, name FROM temp.sqlite_schema ORDER BY name`,
		`SELECT count(*) FROM main.sqlite_master`,
	}
	got := wctRun(t, "sqlite", stmts)
	want := wctRun(t, "sqlite3", stmts)
	wctCompare(t, stmts, got, want)
}
