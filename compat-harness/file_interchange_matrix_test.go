package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
)

// File interchange through the converter: C SQLite files converted in and back out.
// A fixture whose statements name something only the SQLite format has (today,
// a freelist count: each engine answers its own storage's number) says so in
// nativeSkip: those statements are dropped on the musql side and the rest of the
// fixture is still compared. Dropping the whole fixture would lose the DATA half
// of it. Page size, auto_vacuum, text encoding and WAL were all here once; each
// is a catalog field now and is compared like any other answer.
// interchangeFixture is one feature the format has to carry, as the statements
// that build it and the questions that read it back.
//
// nativeSkip lists the statements to DROP when musql is on either side of the
// conversion, each because it names something only the SQLite format has. The
// rest of the fixture is still compared -- see the file's own doc comment.
type interchangeFixture struct {
	name       string
	write      []string
	read       []string
	nativeSkip []string
}

// keep is stmts without this fixture's nativeSkip entries.
func (f interchangeFixture) keep(stmts []string) []string {
	if len(f.nativeSkip) == 0 {
		return stmts
	}
	drop := make(map[string]bool, len(f.nativeSkip))
	for _, s := range f.nativeSkip {
		drop[s] = true
	}
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if !drop[s] {
			out = append(out, s)
		}
	}
	return out
}

func TestFileInterchangeMatrix(t *testing.T) {
	fixtures := []interchangeFixture{
		{name: "plain", write: []string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REAL, d BLOB)`,
			`INSERT INTO t VALUES(1,'x',1.5,x'00ff'),(2,NULL,-0.0,x'')`},
			read: []string{`SELECT a,b,c,quote(d),typeof(c) FROM t ORDER BY a`}},
		{name: "withoutrowid", write: []string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
			`INSERT INTO t VALUES('a',1),('b',2),('c',3)`},
			read: []string{`SELECT k,v FROM t ORDER BY k`}},
		{name: "strict", write: []string{`CREATE TABLE t(a INT, b TEXT NOT NULL) STRICT`, `INSERT INTO t VALUES(1,'x')`},
			read: []string{`SELECT * FROM t`, `SELECT sql FROM sqlite_master`}},
		{name: "generated", write: []string{`CREATE TABLE t(a INT, b AS (a*2), c AS (a+1) STORED)`, `INSERT INTO t(a) VALUES(1),(2)`},
			read: []string{`SELECT * FROM t ORDER BY a`}},
		{name: "exprindex", write: []string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'x'),(2,'y')`,
			`CREATE INDEX i ON t(abs(a)) WHERE b IS NOT NULL`},
			read: []string{`SELECT a FROM t WHERE abs(a)=2`, `SELECT count(*) FROM t`}},
		{name: "collate", write: []string{`CREATE TABLE t(a COLLATE NOCASE, b COLLATE RTRIM)`,
			`INSERT INTO t VALUES('A','x '),('a','x')`, `CREATE INDEX i ON t(a)`},
			read: []string{`SELECT count(*) FROM t WHERE a='a'`, `SELECT count(*) FROM t WHERE b='x'`}},
		{name: "autoinc", write: []string{`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
			`INSERT INTO t(b) VALUES('x'),('y')`, `DELETE FROM t`, `INSERT INTO t(b) VALUES('z')`},
			read: []string{`SELECT a,b FROM t`, `SELECT * FROM sqlite_sequence`}},
		{name: "trigger+view", write: []string{`CREATE TABLE t(a)`, `CREATE TABLE log(m)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
			`CREATE VIEW v AS SELECT a FROM t`, `INSERT INTO t VALUES(1)`},
			read: []string{`SELECT * FROM log`, `SELECT * FROM v`}},
		{name: "fk", write: []string{`CREATE TABLE p(x PRIMARY KEY)`, `CREATE TABLE c(y REFERENCES p(x))`,
			`INSERT INTO p VALUES(1)`, `INSERT INTO c VALUES(1)`},
			read: []string{`PRAGMA foreign_key_check`, `SELECT * FROM c`}},
		{name: "fts4", write: []string{`CREATE VIRTUAL TABLE f USING fts4(a,b)`,
			`INSERT INTO f VALUES('hello world','x'),('goodbye world','y')`},
			read: []string{`SELECT rowid FROM f WHERE f MATCH 'world' ORDER BY rowid`, `SELECT count(*) FROM f`}},
		{name: "fts3", write: []string{`CREATE VIRTUAL TABLE f USING fts3(a)`, `INSERT INTO f VALUES('alpha beta')`},
			read: []string{`SELECT rowid FROM f WHERE f MATCH 'beta'`}},
		{name: "rtree", write: []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(1,0.0,1.0),(2,5.0,6.0)`},
			read: []string{`SELECT id FROM r WHERE x0>=4.0 ORDER BY id`, `SELECT count(*) FROM r`}},
		{name: "bigrows", write: []string{`CREATE TABLE t(a,b)`,
			`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<3000) INSERT INTO t SELECT n, hex(randomblob(50)) FROM s`,
			`CREATE INDEX i ON t(a)`},
			read: []string{`SELECT count(*), sum(a) FROM t`, `SELECT a FROM t WHERE a=1500`}},
		{name: "overflow", write: []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(zeroblob(200000))`},
			read: []string{`SELECT length(a) FROM t`}},
		{name: "deleted+freelist", write: []string{`CREATE TABLE t(a)`,
			`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<2000) INSERT INTO t SELECT hex(randomblob(60)) FROM s`,
			`DELETE FROM t WHERE rowid%2=0`},
			read: []string{`SELECT count(*) FROM t`, `PRAGMA freelist_count`},
			nativeSkip: []string{`PRAGMA freelist_count`}},
		{name: "attachless-temp", write: []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`,
			`PRAGMA user_version=42`, `PRAGMA application_id=7`},
			read: []string{`PRAGMA user_version`, `PRAGMA application_id`, `SELECT * FROM t`}},
		{name: "autovacuum", write: []string{`PRAGMA auto_vacuum=FULL`, `CREATE TABLE t(a)`,
			`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<1500) INSERT INTO t SELECT hex(randomblob(40)) FROM s`,
			`DELETE FROM t WHERE rowid>100`},
			read: []string{`PRAGMA auto_vacuum`, `SELECT count(*) FROM t`}},
		{name: "pagesize", write: []string{`PRAGMA page_size=16384`, `CREATE TABLE t(a)`, `INSERT INTO t VALUES('x')`},
			read: []string{`PRAGMA page_size`, `SELECT * FROM t`}},
		// The DOCUMENTED way to change an existing database's page size, and
		// the only thing that reaches vacuum.c:282's second
		// sqlite3BtreeSetPageSize: the whole file is re-laid at the new size,
		// index pages and overflow chains included.
		{name: "pagesize-vacuum", write: []string{`CREATE TABLE t(a,b)`,
			`WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<800) INSERT INTO t SELECT n, hex(randomblob(90)) FROM s`,
			`CREATE INDEX i ON t(b)`, `PRAGMA page_size=512`, `VACUUM`},
			read: []string{`PRAGMA page_size`, `SELECT count(*), sum(a) FROM t`, `SELECT a FROM t WHERE b=(SELECT b FROM t WHERE a=400)`}},
		{name: "pagesize-vacuum-grow", write: []string{`PRAGMA page_size=512`, `CREATE TABLE t(a)`,
			`INSERT INTO t VALUES(zeroblob(60000))`, `PRAGMA page_size=8192`, `VACUUM`},
			read: []string{`PRAGMA page_size`, `SELECT length(a) FROM t`}},
		{name: "utf16", write: []string{`PRAGMA encoding='UTF-16le'`, `CREATE TABLE t(a)`, `INSERT INTO t VALUES('héllo')`},
			read: []string{`PRAGMA encoding`, `SELECT a, length(a) FROM t`}},
		// THE CATALOG'S OWN ORDER, which C reads in rowid order -- creation order,
		// kinds interleaved, automatic indexes right behind their table -- and
		// ANALYZE's sqlite_stat1, which the planner reads. The file lists tables
		// and the other objects separately, so without ConvertedTable.Rank an
		// import grouped them by kind and an export handed C tables first; and
		// the import used to drop sqlite_stat1 altogether. The trigger checks the
		// export's restore order: a's rows came from it while C wrote, and a
		// restore that re-fired it would double them.
		{name: "catalog-order", write: []string{`CREATE TABLE a(x UNIQUE)`, `CREATE VIEW v AS SELECT 1`,
			`CREATE TABLE b(y)`, `CREATE INDEX bi ON b(y)`,
			`CREATE TRIGGER tr AFTER INSERT ON b BEGIN INSERT INTO a VALUES(new.y); END`,
			`CREATE TABLE c(z UNIQUE, w UNIQUE)`, `INSERT INTO b VALUES(1),(2)`, `INSERT INTO c VALUES(1,2)`, `ANALYZE`},
			read: []string{`SELECT type, name, tbl_name FROM sqlite_master`, `SELECT rowid, * FROM sqlite_stat1`,
				`SELECT x FROM a ORDER BY x`}},
		{name: "wal", write: []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2)`},
			read: []string{`SELECT count(*) FROM t`, `PRAGMA journal_mode`}},
	}
	for _, fx := range fixtures {
		fx := fx
		// C WRITES, WE READ IT BACK: the import direction.
		t.Run(fx.name+"/import", func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "c.db")
			assertRan(t, "cgo", src, fx.write, runWithDSN(t, "cgo", src, fx.write))
			seg := filepath.Join(dir, "imported.musq")
			if ierr := sqliteconv.Import(src, seg, sqliteconv.ImportOptions{}); ierr != nil {
				t.Fatalf("%s: ImportSQLite: %v", fx.name, ierr)
			}
			reads := fx.keep(fx.read)
			want := fmt.Sprintf("%v", runWithDSN(t, "cgo", src, reads))
			got := fmt.Sprintf("%v", runWithDSN(t, "musql", seg, reads))
			if got != want {
				t.Errorf("[%s] the imported database answers differently\n  cgo:    %s\n  musql: %s", fx.name, want, got)
			}
		})
		// WE WRITE, C READS THE EXPORT: the export direction, plus C's own
		// verdict on the file.
		t.Run(fx.name+"/export", func(t *testing.T) {
			dir := t.TempDir()
			seg := filepath.Join(dir, "x.musq")
			writes := fx.keep(fx.write)
			assertRan(t, "musql", seg, writes, runWithDSN(t, "musql", seg, writes))
			out := filepath.Join(dir, "exported.db")
			if eerr := sqliteconv.Export(seg, out, 0); eerr != nil {
				t.Fatalf("%s: ExportSQLite: %v", fx.name, eerr)
			}
			reads := fx.keep(fx.read)
			want := fmt.Sprintf("%v", runWithDSN(t, "musql", seg, reads))
			got := fmt.Sprintf("%v", runWithDSN(t, "cgo", out, reads))
			if got != want {
				t.Errorf("[%s] the exported database answers differently\n  musql: %s\n  cgo:    %s", fx.name, want, got)
			}
			// ...and C SQLite must find what we handed it sound.
			sdb, oerr := sql.Open("sqlite3", out)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer sdb.Close()
			var ck string
			if qerr := sdb.QueryRow(`PRAGMA integrity_check`).Scan(&ck); qerr != nil {
				t.Errorf("[%s] C SQLite could not integrity_check the export: %v", fx.name, qerr)
			} else if ck != "ok" {
				t.Errorf("[%s] C SQLite reports the export as %q, want ok", fx.name, ck)
			}
		})
	}
}

// assertRan fails the test when any statement of a fixture's setup errored: the
// comparison that follows would be vacuous.
func assertRan(t *testing.T, engine, dsn string, stmts []string, res []map[string]any) {
	t.Helper()
	for i, r := range res {
		if kind, _ := r["kind"].(string); kind == "error" {
			t.Fatalf("%s could not run stmt #%d (%s) on %s: %v -- the comparison would be vacuous",
				engine, i, stmts[i], dsn, r["error"])
		}
	}
}
