//go:build sqlite_fts5

// Tests for fts5 EXTERNAL CONTENT tables: "content=<table>" with or without
// "content_rowid=<column>". Covers schema, shadow tables, queries, writes, and
// index/content disagreement handling. Self-verifying against the oracle.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r32pFixture is one external-content schema plus the statements that fill it.
// The SCHEMA axis deliberately varies: where the content= option sits in the
// argument list, whether content_rowid= is given at all, whether the rowid
// column is an INTEGER PRIMARY KEY / a WITHOUT ROWID primary key / the table's
// implicit rowid, whether the content table has columns the fts5 table does not
// index, whether the fts5 columns appear in the content table's own order (the
// "every fixture put the shared column first" trap), and the tokenizer/prefix/
// UNINDEXED options that change the bytes.
type r32pFixture struct {
	name  string
	setup []string
}

var r32pFixtures = []r32pFixture{
	{"implicit rowid", []string{
		`CREATE TABLE src(a, b)`,
		`INSERT INTO src(rowid,a,b) VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu'),(4,'','lonely')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"integer primary key", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu'),(4,'','lonely')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"quoted names", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(-40,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content='src', content_rowid='id')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"content option first", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(content_rowid=id, content=src, a, b)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"abbreviated option key", []string{
		// fts5ConfigParseSpecial matches an option key as a case-insensitive
		// PREFIX of the full name, so "c=" IS "content=" -- fts5content.test
		// 7.1 writes exactly that.
		`CREATE TABLE src(a, b)`,
		`INSERT INTO src(rowid,a,b) VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, c=src)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"columns out of order", []string{
		// The fts5 columns are NOT in the content table's declaration order,
		// and the content table has a column the index never sees.
		`CREATE TABLE src(id INTEGER PRIMARY KEY, b, skipped, a)`,
		`INSERT INTO src VALUES(1,'beta gamma','never indexed','shared alpha'),(2,'zulu','nope','alpha only'),(3,'shared zulu zulu','no','shared')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"without rowid content", []string{
		// fts5content.test 4.1's shape: the content table has no rowid at all
		// and content_rowid= names a quoted primary-key column.
		`CREATE TABLE src(a, "key col" PRIMARY KEY, b, c) WITHOUT ROWID`,
		`INSERT INTO src VALUES('shared alpha',1,'beta gamma','ignored'),('alpha only',-40,'zulu','ignored')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid='key col')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"unindexed column", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"prefix index", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id, prefix='2 4')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"ascii tokenizer", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id, tokenize=ascii)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"detail columns", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id, detail=columns)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"typed and null values", []string{
		// The values are not all TEXT: fts5 tokenizes whatever
		// sqlite3_value_text() makes of them, and a NULL contributes nothing.
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,42,'beta gamma'),(2,NULL,'zulu'),(3,3.5,NULL),(4,x'736861726564','shared')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"indexed by insert not rebuild", []string{
		// The documented pattern: rows go into the content table and into the
		// index separately, never through a 'rebuild'.
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'shared alpha','beta gamma')`,
		`INSERT INTO src VALUES(2,'alpha only','zulu')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'alpha only','zulu')`,
	}},
	{"trigger maintained", []string{
		// fts5content.test 2.1's own fixture, which is what the fts5
		// documentation tells users to write.
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`CREATE TRIGGER src_ai AFTER INSERT ON src BEGIN INSERT INTO t(rowid,a,b) VALUES(new.id,new.a,new.b); END`,
		`CREATE TRIGGER src_ad AFTER DELETE ON src BEGIN INSERT INTO t(t,rowid,a,b) VALUES('delete',old.id,old.a,old.b); END`,
		`CREATE TRIGGER src_au AFTER UPDATE ON src BEGIN INSERT INTO t(t,rowid,a,b) VALUES('delete',old.id,old.a,old.b); INSERT INTO t(rowid,a,b) VALUES(new.id,new.a,new.b); END`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma')`,
		`INSERT INTO src VALUES(2,'alpha only','zulu')`,
		`INSERT INTO src VALUES(3,'shared','shared zulu zulu')`,
		`DELETE FROM src WHERE id=2`,
		`UPDATE src SET b = b || ' extra' WHERE id=3`,
		`INSERT INTO src VALUES(4,'','lonely')`,
	}},
}

// r32pReadouts is the READOUT axis. Round 31 proved a battery can report
// wrong=0 over shapes that are silently wrong because every one of its queries
// read the same way, so these deliberately reach the row source through a bare
// scan, an aggregate with no bare column at all, a rowid lookup, a GROUP BY, a
// MATCH in each of its SQL forms, an aux function and ORDER BY rank.
var r32pReadouts = []string{
	`SELECT rowid, a, b FROM t ORDER BY rowid`,
	`SELECT * FROM t ORDER BY rowid`,
	`SELECT count(*) FROM t`,
	`SELECT count(*), count(a), count(b) FROM t`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t ORDER BY rowid)`,
	`SELECT rowid, a FROM t WHERE rowid=1`,
	`SELECT rowid, a FROM t WHERE rowid=2`,
	`SELECT typeof(a), typeof(b) FROM t ORDER BY rowid`,
	`SELECT a IS NULL, b IS NULL FROM t ORDER BY rowid`,
	`SELECT count(*) FROM t GROUP BY (rowid%2) ORDER BY 1`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'zulu' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared AND alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared NOT alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alph*' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'nosuchterm')`,
	`SELECT count(*) FROM t WHERE t MATCH 'shared'`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE a MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(a||'/'||ifnull(b,'')),'') FROM (SELECT a,b FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(highlight(t,0,'[',']')),'') FROM (SELECT * FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(snippet(t,-1,'[',']','...',4)),'') FROM (SELECT * FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rank, rowid)`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
}

// r32pExtShadowDump is fts5ShadowDump WITHOUT the %_content query: an
// external-content table has none, which TestR32PExtSchema is what checks.
var r32pExtShadowDump = []string{
	`SELECT id, quote(block) FROM t_data ORDER BY id`,
	`SELECT quote(segid), quote(term), quote(pgno) FROM t_idx ORDER BY segid, term`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, v FROM t_config`,
	`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY name`,
}

// r32pRun writes both databases and returns their DSNs.
//
// A setup this engine declines is SKIPPED rather than failed, with the reason
// printed: the one fixture that reaches it is the trigger-maintained pattern,
// which needs a trigger body to be able to write into a virtual table at all --
// a pre-existing gap outside this stream's files (see the round-32 report). The
// fixture is left in place so it starts running the moment that lands.
func r32pRun(t *testing.T, setup []string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	if err := fts5Exec(t, "sqlite3", dsn["sqlite3"], setup); err != nil {
		t.Fatalf("oracle setup: %v", err)
	}
	if err := fts5Exec(t, "sqlite", dsn["sqlite"], setup); err != nil {
		t.Skipf("this engine declines the setup C fts5 accepts: %v", err)
	}
	return dsn
}

// r32pCompare runs one statement against both databases and reports the four
// outcomes. Both erroring counts as agreement here (the message wording of a
// decline is not this gate's subject); everything else must match.
func r32pCompare(t *testing.T, label string, dsn map[string]string, stmt string) {
	t.Helper()
	goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], stmt)
	cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], stmt)
	switch {
	case goErr != nil && cgoErr != nil:
	case goErr != nil:
		t.Errorf("[%s] this engine DECLINES a statement C fts5 answers %q\n  sql: %s\n  err: %v", label, cgoOut, stmt, goErr)
	case cgoErr != nil:
		t.Errorf("[%s] this engine ANSWERS %q a statement C fts5 REFUSES\n  sql: %s\n  cgo err: %v", label, goOut, stmt, cgoErr)
	case goOut != cgoOut:
		t.Errorf("[%s] DIVERGES\n  sql: %s\n%s", label, stmt, fts5DiffLines(goOut, cgoOut))
	}
}

// TestR32PExtSchema checks which shadow tables an external-content table owns,
// and that DROP TABLE leaves the content table -- and a same-named "<t>_content"
// table that is not its own -- alone.
func TestR32PExtSchema(t *testing.T) {
	setup := []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`CREATE TABLE t_content(x)`,
		`INSERT INTO t_content VALUES('not a shadow table')`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}
	dsn := r32pRun(t, setup)
	for _, q := range []string{
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		`SELECT x FROM t_content`,
	} {
		r32pCompare(t, "before drop", dsn, q)
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], []string{`DROP TABLE t`}); err != nil {
			t.Fatalf("%s DROP: %v", drv, err)
		}
	}
	for _, q := range []string{
		`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
		`SELECT x FROM t_content`,
		`SELECT count(*) FROM src`,
	} {
		r32pCompare(t, "after drop", dsn, q)
	}
}

// TestR32PExtShadowBytes pins the shadow BYTES. Every fixture here indexes the
// whole table in ONE statement ('rebuild', or a single INSERT), which is the
// case where C fts5 also holds exactly one segment -- the same scoping
// fts5_diff_test.go's TestFts5ShadowLayoutDiff explains.
func TestR32PExtShadowBytes(t *testing.T) {
	for _, fx := range r32pFixtures {
		fx := fx
		if fx.name == "trigger maintained" || fx.name == "indexed by insert not rebuild" {
			continue // several statements: the two engines legitimately differ in segment count
		}
		t.Run(fx.name, func(t *testing.T) {
			dsn := r32pRun(t, fx.setup)
			for _, q := range r32pExtShadowDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("%s DIVERGES\n  sql: %s\n%s", fx.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestR32PExtQueries is the answer matrix over every fixture and every readout.
func TestR32PExtQueries(t *testing.T) {
	for _, fx := range r32pFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			dsn := r32pRun(t, fx.setup)
			for _, q := range r32pReadouts {
				r32pCompare(t, fx.name, dsn, q)
			}
		})
	}
}

// TestR32PExtInterchange has each engine read back the file the OTHER wrote,
// and C SQLite integrity_check it. That is what proves the index this engine
// writes for an external-content table is one C fts5 can search, not merely
// one it can parse.
func TestR32PExtInterchange(t *testing.T) {
	for _, fx := range r32pFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				dsn := filepath.Join(t.TempDir(), "fts5.db")
				if err := fts5Exec(t, writer, dsn, fx.setup); err != nil {
					if writer == "sqlite" {
						t.Skipf("this engine declines the setup: %v", err)
					}
					t.Fatalf("%s writes: %v", writer, err)
				}
				for _, q := range r32pReadouts {
					goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
					switch {
					case goErr != nil && cgoErr != nil:
					case goErr != nil && writer == "sqlite":
						t.Errorf("[%s wrote] this engine cannot read back its own file\n  sql: %s\n  err: %v", writer, q, goErr)
					case goErr != nil:
						// C SQLite may leave the index in several segments,
						// which this engine declines to rewrite but must still
						// scan; only a MATCH may refuse.
						if !strings.Contains(q, "MATCH") && !strings.Contains(q, "rank") {
							t.Errorf("[%s wrote] this engine cannot read it back\n  sql: %s\n  err: %v", writer, q, goErr)
						}
					case cgoErr != nil:
						t.Errorf("[%s wrote] C SQLite cannot read it\n  sql: %s\n  err: %v", writer, q, cgoErr)
					case goOut != cgoOut:
						t.Errorf("[%s wrote] the two engines read it differently\n  sql: %s\n%s", writer, q, fts5DiffLines(goOut, cgoOut))
					}
				}
				ic, err := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), `PRAGMA integrity_check`)
				if err != nil {
					t.Fatalf("[%s wrote] integrity_check: %v", writer, err)
				}
				if ic != "integrity_check\nT:ok" {
					t.Errorf("[%s wrote] C SQLite reports integrity_check = %q", writer, ic)
				}
			}
		})
	}
}

// r32pWriteSteps are applied one at a time to a table already in sync, with the
// whole readout matrix re-run after each -- so a write whose effect on the
// index differs shows up as a divergent ANSWER, not only as divergent bytes.
var r32pWriteSteps = []struct {
	name string
	sql  []string
}{
	{"index a new content row", []string{
		`INSERT INTO src VALUES(9,'shared newrow','tail')`,
		`INSERT INTO t(rowid,a,b) VALUES(9,'shared newrow','tail')`,
	}},
	{"delete command after content delete", []string{
		`DELETE FROM src WHERE id=2`,
		`INSERT INTO t(t,rowid,a,b) VALUES('delete',2,'alpha only','zulu')`,
	}},
	{"delete command before content delete", []string{
		`INSERT INTO t(t,rowid,a,b) VALUES('delete',2,'alpha only','zulu')`,
		`DELETE FROM src WHERE id=2`,
	}},
	{"delete command with a non-integer rowid", []string{
		`INSERT INTO t(t,rowid,a,b) VALUES('delete',NULL,'alpha only','zulu')`,
	}},
	{"update through delete and reinsert", []string{
		`INSERT INTO t(t,rowid,a,b) VALUES('delete',3,'shared','shared zulu zulu')`,
		`UPDATE src SET a='replaced text' WHERE id=3`,
		`INSERT INTO t(rowid,a,b) VALUES(3,'replaced text','shared zulu zulu')`,
	}},
	{"delete-all", []string{
		`INSERT INTO t(t) VALUES('delete-all')`,
	}},
	{"delete-all then rebuild", []string{
		`INSERT INTO t(t) VALUES('delete-all')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"rebuild after a content change", []string{
		`UPDATE src SET b='rebuilt zulu' WHERE id=1`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}},
	{"integrity-check", []string{
		`INSERT INTO t(t) VALUES('integrity-check')`,
	}},
	{"DELETE FROM the fts5 table", []string{
		`DELETE FROM t WHERE rowid=2`,
	}},
	{"optimize", []string{
		`INSERT INTO t(t) VALUES('optimize')`,
	}},
}

// TestR32PExtWrites applies each write step to a freshly built, in-sync
// external-content table and re-checks every readout afterwards.
func TestR32PExtWrites(t *testing.T) {
	base := []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu'),(3,'shared','shared zulu zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}
	for _, step := range r32pWriteSteps {
		step := step
		t.Run(step.name, func(t *testing.T) {
			dsn := r32pRun(t, base)
			goErr := fts5Exec(t, "sqlite", dsn["sqlite"], step.sql)
			cgoErr := fts5Exec(t, "sqlite3", dsn["sqlite3"], step.sql)
			switch {
			case goErr != nil && cgoErr != nil:
				return
			case goErr != nil:
				t.Errorf("[%s] this engine DECLINES a write C fts5 performs\n  err: %v", step.name, goErr)
				return
			case cgoErr != nil:
				t.Errorf("[%s] this engine PERFORMS a write C fts5 refuses\n  cgo err: %v", step.name, cgoErr)
				return
			}
			for _, q := range r32pReadouts {
				r32pCompare(t, step.name, dsn, q)
			}
			// The bytes too, wherever both engines still hold one segment --
			// which they do here only after a command that rewrites the whole
			// index.
			if step.name == "delete-all" || step.name == "delete-all then rebuild" || step.name == "rebuild after a content change" {
				for _, q := range r32pExtShadowDump {
					goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
					if goErr != nil || cgoErr != nil {
						t.Fatalf("%s: %v / %v", q, goErr, cgoErr)
					}
					if goOut != cgoOut {
						t.Errorf("[%s] shadow bytes DIVERGE\n  sql: %s\n%s", step.name, q, fts5DiffLines(goOut, cgoOut))
					}
				}
			}
		})
	}
}

// r32pDriftFixtures are the shapes where the index and the content table
// deliberately DISAGREE. Real fts5 keeps working (it edits its index
// incrementally and never re-derives it); this engine re-encodes the whole
// index from the documents, so several of these are declines. The claim here is
// only the never-wrong one: agree, or refuse.
var r32pDriftFixtures = []r32pFixture{
	{"content rows never indexed", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
	}},
	{"content row deleted without a delete command", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`DELETE FROM src WHERE id=2`,
	}},
	{"content text changed under the index", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`UPDATE src SET a='completely different words' WHERE id=1`,
	}},
	{"content text changed keeping the token count", []string{
		// The one a %_docsize-only check would miss: same number of tokens in
		// every column, different terms.
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`UPDATE src SET a='xxxxxx yyyyy' WHERE id=1`,
	}},
	{"index holds a rowid the content table never had", []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`INSERT INTO t(rowid,a,b) VALUES(99,'phantom shared','phantom')`,
	}},
	{"content table does not exist", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=nosuchtable)`,
	}},
	{"content table is the fts5 table itself", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, c=t)`,
	}},
}

// TestR32PExtDrift is the never-wrong claim: over a drifted table this engine
// may refuse, but it may not answer something C fts5 does not.
func TestR32PExtDrift(t *testing.T) {
	for _, fx := range r32pDriftFixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			goSetup := fts5Exec(t, "sqlite", dsn["sqlite"], fx.setup)
			cgoSetup := fts5Exec(t, "sqlite3", dsn["sqlite3"], fx.setup)
			if cgoSetup != nil {
				if goSetup == nil {
					t.Errorf("[%s] this engine accepts a setup C fts5 refuses: %v", fx.name, cgoSetup)
				}
				return
			}
			if goSetup != nil {
				return // a decline of the setup itself; nothing further to compare
			}
			for _, q := range r32pReadouts {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				switch {
				case goErr != nil:
					// A decline. Allowed here -- this whole fixture set is the
					// drift the engine cannot re-derive.
				case cgoErr != nil:
					t.Errorf("[%s] this engine ANSWERS %q a statement C fts5 REFUSES\n  sql: %s\n  cgo err: %v", fx.name, goOut, q, cgoErr)
				case goOut != cgoOut:
					t.Errorf("[%s] WRONG ANSWER over a drifted table\n  sql: %s\n%s", fx.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestR32PExtOptionErrors is the CREATE-time matrix: which content= /
// content_rowid= spellings C fts5 accepts, and which it refuses. It compares
// acceptance only (the message wording is not this gate's subject), so a shape
// this engine declines at CREATE where fts5 accepts it is reported and a shape
// it accepts where fts5 refuses is a failure.
func TestR32PExtOptionErrors(t *testing.T) {
	cases := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content=src)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id, content_rowid=id)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=nosuchtable)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content="src")`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, con=src)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content_=src)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content_r=id, content=src)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, contentless_delete=1, content=src)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, columnsize=0)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=nosuchcol)`,
		`CREATE VIRTUAL TABLE t USING fts5(a, nosuchfts5col, content=src)`,
	}
	for _, create := range cases {
		create := create
		t.Run(create, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			setup := []string{`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`, create}
			goErr := fts5Exec(t, "sqlite", dsn["sqlite"], setup)
			cgoErr := fts5Exec(t, "sqlite3", dsn["sqlite3"], setup)
			if goErr == nil && cgoErr != nil {
				t.Errorf("this engine ACCEPTS a CREATE C fts5 refuses\n  sql: %s\n  cgo err: %v", create, cgoErr)
			}
			if goErr != nil || cgoErr != nil {
				return
			}
			for _, q := range []string{
				`SELECT type, name, tbl_name FROM sqlite_master ORDER BY name`,
				`SELECT count(*) FROM t`,
			} {
				r32pCompare(t, "create", dsn, q)
			}
		})
	}
}

// r32pOpen is a tiny helper for the cases that need a live handle rather than
// the one-shot Exec/Query pair above.
func r32pOpen(t *testing.T, drv, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// TestR32PExtChanges compares changes() after each write to an
// external-content table -- a readout no other test here reads, and the one
// that separates "the row was not there" from "the statement did nothing".
func TestR32PExtChanges(t *testing.T) {
	base := []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'shared alpha','beta gamma'),(2,'alpha only','zulu')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content=src, content_rowid=id)`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}
	writes := []string{
		`INSERT INTO t(rowid,a,b) VALUES(7,'seven','7')`,
		`INSERT INTO t(t,rowid,a,b) VALUES('delete',2,'alpha only','zulu')`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`INSERT INTO t(t) VALUES('delete-all')`,
	}
	for _, w := range writes {
		w := w
		t.Run(w, func(t *testing.T) {
			dsn := r32pRun(t, base)
			out := map[string]string{}
			bad := map[string]error{}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				db := r32pOpen(t, drv, dsn[drv])
				res, err := db.Exec(w)
				if err != nil {
					bad[drv] = err
					db.Close()
					continue
				}
				n, _ := res.RowsAffected()
				out[drv] = fmt.Sprintf("%d", n)
				db.Close()
			}
			if bad["sqlite"] != nil || bad["sqlite3"] != nil {
				if bad["sqlite"] == nil {
					t.Errorf("this engine performs %q which C fts5 refuses: %v", w, bad["sqlite3"])
				}
				return
			}
			if out["sqlite"] != out["sqlite3"] {
				t.Errorf("changes() DIVERGES for %q: go %s, cgo %s", w, out["sqlite"], out["sqlite3"])
			}
		})
	}
}

// r32pLexQueries is the MATCH-query LEXER matrix. fts5's expression lexer is
// not the tokenizer: it has ten single-character tokens ("(){}:+*,-^), quoted
// strings, and BAREWORDS made only of letters, digits, '_', 0x1A and every
// non-ASCII character (sqlite3Fts5IsBareword, ext/fts5/fts5_buffer.c). Every
// other ASCII punctuation mark is `fts5: syntax error near "<c>"`.
//
// This engine used to treat all of them as ordinary bareword characters, which
// ANSWERED 19 of the 20 queries below where C fts5 refuses -- a wrong answer,
// not a decline, and one no fixture in the corpus happened to reach. The
// twentieth, "-{a}:alpha", is the colset-INVERT production: it silently dropped
// both the '-' and the filter and returned every matching row.
var r32pLexQueries = []string{
	// The MINUS colset-invert productions (fts5parse.y: `colset ::= MINUS LCP
	// colsetlist RCP` and `colset ::= MINUS STRING`).
	`-{a}:alpha`,
	`-a:alpha`,
	`-{a b}:alpha`,
	`-b:alpha`,
	`-{b}:beta`,
	`-a:(alpha OR beta)`,
	`-{a}:NEAR(alpha beta)`,
	// A '-' anywhere else, including the hyphenated word everyone tries first.
	`alpha-beta`,
	`-alpha`,
	`alpha -`,
	`alpha - beta`,
	// Every other ASCII punctuation mark that is neither a token nor a
	// bareword character.
	`alpha.beta`, `alpha/beta`, `a@b`, `alpha!`, `alpha?`, `alpha=1`,
	`alpha[1]`, `alpha;beta`, `alpha~b`, `alpha|b`, `alpha#b`, `alpha$b`,
	`alpha%b`, `alpha&b`, `alpha<b`, `alpha>b`, `alpha\b`, `alpha'b`,
	// ...and the ones that ARE legal, so the rule cannot be "refuse more".
	`alpha`, `alpha_beta`, `alpha1`, `_alpha`, `"alpha-beta"`, `"alpha.beta"`,
	`café`, `日本語`, `a:alpha`, `{a b}:alpha`, `^alpha`, `alpha*`,
	`alpha AND beta`, `alpha OR beta`, `alpha NOT beta`, `alpha + beta`,
	`NEAR(alpha beta, 2)`,
}

// TestR32PLexQueries compares the lexer's verdict with the oracle's over every
// spelling above, on an ordinary fts5 table (the rule has nothing to do with
// content=) and over each of the three tokenizers, since the EXPRESSION lexer
// runs before the tokenizer ever sees a term.
func TestR32PLexQueries(t *testing.T) {
	for _, tok := range []string{"", ", tokenize=ascii", ", tokenize=trigram", ", tokenize=porter"} {
		tok := tok
		t.Run("tokenize"+tok, func(t *testing.T) {
			dsn := r32pRun(t, []string{
				fmt.Sprintf(`CREATE VIRTUAL TABLE t USING fts5(a, b%s)`, tok),
				`INSERT INTO t(rowid,a,b) VALUES(1,'alpha beta','gamma delta'),(2,'alpha_beta café','日本語'),(3,'alpha1 _alpha','beta')`,
			})
			for _, q := range r32pLexQueries {
				stmt := fmt.Sprintf(`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH '%s' ORDER BY rowid)`, strings.ReplaceAll(q, "'", "''"))
				r32pCompare(t, "lex "+q, dsn, stmt)
			}
		})
	}
}
