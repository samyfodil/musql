// Pure syntax gaps: parser constructs that C SQLite accepts but this engine's
// parser previously rejected. Includes COLLATE on IS TRUE/FALSE, HAVING without
// GROUP BY, REGEXP, UPDATE/DELETE aliases, INDEXED BY, RETURNING in INSERT,
// WITH subqueries, and VALUES in FROM.
//
//	SET (a,b) = <row-value>           expanded into ordinary assignments
//	FROM t1, t2 ON <expr>             a comma join takes a constraint too
//	'quoted' as a NAME                SQLite's "nm ::= STRING" leniency
//	INSERT INTO t WITH ... SELECT     the source select's own CTE list
//
// Every case runs against both engines and must AGREE: same rows and column
// names when both accept, or a rejection on both. Error TEXT is deliberately
// not compared (the two engines word errors differently); what is compared is
// the accept/reject decision, which is what "never wrong" is about.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// parserGapSetup is the schema every case below runs against. Deliberately
// tiny and fully deterministic: the point is the grammar, not the data.
//
// It must never USE a construct under test. The 'sq' table exists so the
// string-as-name cases have something to name, but its setup spells every
// identifier with DOUBLE quotes -- a spelling that already worked before this
// round. Single quotes here would make the setup itself regress whenever the
// nm-as-STRING rule does, and t.Fatalf would then abort every subtest at
// setup: measured, that turned a mutation run into 123 identical
// "engine setup" failures that localize nothing, instead of the handful of
// cases that actually detect the change.
var parserGapSetup = []string{
	`CREATE TABLE t1(a,b,c)`,
	`CREATE TABLE t2(x,y,z)`,
	`CREATE TABLE empt(a,b)`,
	`CREATE TABLE anch(g,v,label)`,
	`CREATE TABLE "sq"("!c","#d")`,
	`CREATE INDEX i1 ON t1(a)`,
	`CREATE INDEX i2 ON t2(x)`,
	`INSERT INTO t1 VALUES(1,10,'p'),(3,30,'q'),(2,20,'r')`,
	`INSERT INTO t2 VALUES(1,2,3)`,
	`INSERT INTO anch VALUES(1,10,'a10'),(1,30,'a30'),(1,20,'a20')`,
	`INSERT INTO "sq" VALUES(5,'hello')`,
}

// parserGapCases are read-only statements: both engines must return the same
// columns and rows, or both must reject.
var parserGapCases = []string{
	// ---- "IS [NOT] TRUE/FALSE COLLATE <name>": the collation is skipped
	// (sqlite3ExprSkipCollate) and, unlike a plain COLLATE, never resolved --
	// so even a bogus name is accepted.
	`SELECT 0.5 IS TRUE COLLATE NOCASE`,
	`SELECT 0.5 IS TRUE COLLATE RTRIM`,
	`SELECT 0.5 IS TRUE COLLATE BINARY`,
	`SELECT 0.5 IS TRUE COLLATE BOGUS`,
	`SELECT 0.0 IS FALSE COLLATE NOCASE`,
	`SELECT NULL IS TRUE COLLATE NOCASE`,
	`SELECT NULL IS FALSE COLLATE NOCASE`,
	`SELECT 'a' IS NOT TRUE COLLATE RTRIM`,
	`SELECT 0.5 IS TRUE COLLATE NOCASE COLLATE RTRIM`,
	`SELECT (0.5 IS TRUE COLLATE NOCASE) + 1`,
	`SELECT 0.5 IS TRUE COLLATE NOCASE = 1`,

	// ---- HAVING without GROUP BY, on a whole-table aggregate ----
	`SELECT count(*) FROM t1 HAVING count(*)>1`,
	`SELECT count(*) FROM t1 HAVING count(*)<1`,
	`SELECT count(*) FROM empt HAVING count(*)=0`, // the empty case: still ONE row
	`SELECT count(*) FROM t1 WHERE 0 HAVING count(*)=0`,
	`SELECT count(*) FROM t1 HAVING NULL`,
	`SELECT count(*) FROM t1 HAVING 'abc'`,
	`SELECT count(*) AS n FROM t1 HAVING n=3`,
	`SELECT count(*) FROM t1 HAVING count(*)=3 LIMIT 1`,
	`SELECT count(*) FROM t1 HAVING count(*)=3 LIMIT 0`,
	`SELECT count(*) FROM t1 HAVING count(*)=3 ORDER BY 1`,
	// A bare column in HAVING resolves against the same anchor row a bare
	// select-list column does -- the FIRST scanned row, or the honored
	// min/max row when the query has one.
	`SELECT count(*) FROM anch HAVING label='a10'`,
	`SELECT count(*) FROM anch HAVING label='a20'`,
	`SELECT max(v) FROM anch HAVING label='a30'`,
	`SELECT max(v) FROM anch HAVING label='a10'`,
	`SELECT min(v), label FROM anch HAVING label='a10'`,
	`SELECT group_concat(label) FROM anch HAVING v=10`,
	`SELECT count(*) FROM t1, t2 HAVING t1.c='p'`,
	// ---- and the rejection: "HAVING clause on a non-aggregate query" ----
	`SELECT 1 HAVING 0`,
	`SELECT a FROM t1 HAVING count(*)>1`,
	`SELECT 1 FROM t1 HAVING max(a)>0`,
	`SELECT * FROM t1 HAVING count(*)>0`,
	`SELECT (SELECT count(*)) FROM t1 HAVING 1`,
	`SELECT count(*) OVER () FROM t1 HAVING 1`,

	// ---- REGEXP: parses, then fails to resolve regexp() on both sides ----
	`SELECT 'a' REGEXP 'b'`,
	`SELECT 'a' NOT REGEXP 'b'`,
	`SELECT * FROM t1 WHERE a REGEXP 'x'`,
	// REGEXP stays a keyword: never a bare alias, still fine after AS.
	`SELECT a REGEXP FROM t1`,
	`SELECT 1 regexp FROM t1`,
	`SELECT a AS regexp FROM t1`,

	// ---- a subquery body may carry its own WITH clause ----
	`SELECT * FROM (WITH tmp(q) AS (SELECT a FROM t1) SELECT q FROM tmp)`,
	`SELECT * FROM (WITH w AS (SELECT * FROM t1) SELECT 10)`,
	`SELECT * FROM (WITH abc(d,e) AS (SELECT x,y FROM t2) SELECT * FROM abc)`,
	`SELECT (WITH c AS (SELECT 5 AS v) SELECT v FROM c)`,
	`SELECT EXISTS (WITH c AS (SELECT 5) SELECT * FROM c)`,
	`SELECT * FROM t1 WHERE a IN (WITH r(n) AS (VALUES(1) UNION ALL SELECT n+2 FROM r WHERE n<6) SELECT n FROM r)`,
	`SELECT EXISTS (WITH RECURSIVE r(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM r WHERE n<3) SELECT * FROM r WHERE n=3)`,
	`SELECT * FROM (WITH RECURSIVE r(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM r WHERE n<4) SELECT n FROM r) AS s, t1 WHERE s.n=t1.a`,
	// The inner WITH must NOT reach a sibling CTE's body: with3.test 2.0,
	// which is also the case that caught the compile-vs-run scope split
	// (right rows under a column named after the wrong x1).
	`WITH x1 AS (SELECT 10), x2 AS (SELECT 11), x3 AS (SELECT * FROM x1 UNION ALL SELECT * FROM x2), x4 AS (WITH x1 AS (SELECT 12), x2 AS (SELECT 13) SELECT * FROM x3) SELECT * FROM x4`,
	`WITH q AS (SELECT 1 AS z) SELECT * FROM (WITH q AS (SELECT 2 AS z) SELECT * FROM q)`,
	`WITH q AS (SELECT 1 AS z) SELECT * FROM (SELECT * FROM q)`,
	`WITH q AS (SELECT 1 AS z) SELECT (SELECT z FROM q)`,

	// ---- a COMMA join takes an ON/USING constraint ----
	`SELECT * FROM t1, t2 ON t1.a=t2.x`,
	`SELECT * FROM t1, t2, t1 AS t3 ON t1.a=t3.a`,
	`SELECT count(*) FROM t1, t2 ON t1.a=t2.x`,
	`SELECT * FROM t1, t2 ON t1.a=t2.x WHERE t1.b>5`,
	`SELECT * FROM (SELECT 123), (SELECT 456) ON likely(0 OR 1) OR 0`,
	`SELECT * FROM t1, t1 AS q USING (a)`,
	`SELECT * FROM t1, t2 USING (a)`, // no common column: rejected on both
	`SELECT * FROM t1 JOIN t2 ON t1.a=t2.x, t1 AS t4 ON t2.x=t4.a`,

	// ---- a single-quoted STRING is a NAME wherever the grammar wants one,
	// and still a LITERAL wherever it wants an expression ----
	`SELECT * FROM 'sq'`,
	`SELECT 'sq'.'!c' FROM 'sq'`,
	`SELECT main.'sq'.'!c' FROM 'sq'`,
	`SELECT '!c' FROM 'sq'`, // a literal: the string itself, not the column
	`SELECT "!c" FROM 'sq'`, // double-quoted: the column
	`SELECT 'sq'.'!c' AS 'z' FROM 'sq' AS 'sq'`,
	`SELECT 'nope'.'x' FROM 'sq'`, // rejected on both
	`SELECT 1 WHERE 'a'='a'`,      // still two literals
	`SELECT typeof('abc')`,

	// ---- "(VALUES (...), (...))" is a derived table, not a join subtree ----
	`SELECT * FROM (VALUES(1),(2))`,
	`SELECT * FROM (VALUES(1))`,
	`SELECT * FROM (VALUES(1,2),(3,4)) AS v`,
	`SELECT * FROM (VALUES(CAST(44 AS REAL)),(55))`,
	`SELECT * FROM t2 CROSS JOIN (VALUES(7),(8))`,
}

// parserGapWriteCases are statement sequences: each runs on both engines in
// order, and every statement's accept/reject must agree. A trailing SELECT
// checks the two engines ended up with the same DATA, not just the same
// verdicts -- a construct that parses and is then silently ignored is exactly
// the failure this repository fears most.
var parserGapWriteCases = [][]string{
	// UPDATE/DELETE "AS alias": the alias replaces the table's name.
	{`UPDATE t1 AS q SET b=q.b+1 WHERE q.a=2`, `SELECT a,b FROM t1 ORDER BY a`},
	{`UPDATE t1 AS q SET b=b+1 WHERE t1.a=1`, `SELECT a,b FROM t1 ORDER BY a`},
	{`DELETE FROM t1 AS q WHERE q.a=3`, `SELECT a FROM t1 ORDER BY a`},
	{`DELETE FROM t1 AS q WHERE t1.a=3`, `SELECT a FROM t1 ORDER BY a`},
	{`UPDATE t1 AS q SET b=99 WHERE EXISTS(SELECT 1 FROM t1 WHERE t1.a<q.a)`, `SELECT a,b FROM t1 ORDER BY a`},
	{`UPDATE t1 AS q SET b=98 WHERE q.a IN (SELECT a FROM t1)`, `SELECT a,b FROM t1 ORDER BY a`},
	{`UPDATE t1 AS q SET q.b=1 WHERE q.a=1`},   // SET target cannot be qualified
	{`UPDATE t1 AS q SET b=b+1 RETURNING q.a`}, // RETURNING does not see the alias
	{`UPDATE t1 q SET b=b+1`},      // a bare (AS-less) alias is a syntax error
	{`DELETE FROM t1 q WHERE a=1`}, //   "
	{`UPDATE t1 AS q INDEXED BY i1 SET b=b+1 WHERE q.a=1`, `SELECT a,b FROM t1 ORDER BY a`},

	// INDEXED BY / NOT INDEXED: no-ops whose name is still checked.
	{`UPDATE t1 INDEXED BY i1 SET b=b+1 WHERE a=1`, `SELECT a,b FROM t1 ORDER BY a`},
	{`DELETE FROM t1 INDEXED BY i1 WHERE a=99`, `SELECT a FROM t1 ORDER BY a`},
	{`UPDATE t1 NOT INDEXED SET b=b+1 WHERE a=1`, `SELECT a,b FROM t1 ORDER BY a`},
	{`DELETE FROM t1 NOT INDEXED WHERE a=99`, `SELECT a FROM t1 ORDER BY a`},
	{`UPDATE t1 INDEXED BY nosuchidx SET b=b+1 WHERE a=1`}, // no such index
	{`DELETE FROM t1 INDEXED BY nosuchidx WHERE a=1`},      //   "
	{`UPDATE t1 INDEXED BY i2 SET b=b+1 WHERE a=1`},        // i2 is on t2: still "no such index"
	// NEITHER hint is legal inside a trigger body (the reference says so in
	// its own words); an AS alias there IS. This is the case that first
	// landed WRONG -- the engine created four triggers C SQLite refuses.
	{`CREATE TRIGGER e1 AFTER INSERT ON t2 BEGIN UPDATE t1 NOT INDEXED SET b=b+1; END`},
	{`CREATE TRIGGER e2 AFTER INSERT ON t2 BEGIN UPDATE t1 INDEXED BY i1 SET b=b+1 WHERE a=1; END`},
	{`CREATE TRIGGER e3 AFTER INSERT ON t2 BEGIN DELETE FROM t1 NOT INDEXED WHERE a=123; END`},
	{`CREATE TRIGGER e4 AFTER INSERT ON t2 BEGIN DELETE FROM t1 INDEXED BY i1 WHERE a=123; END`},
	{`CREATE TRIGGER e5 AFTER INSERT ON t2 BEGIN UPDATE t1 AS q SET b=q.b+1; END`,
		`INSERT INTO t2 VALUES(4,5,6)`, `SELECT a,b FROM t1 ORDER BY a`},
	{`CREATE TRIGGER e6 AFTER INSERT ON t2 BEGIN DELETE FROM t1 AS q WHERE q.a=1; END`,
		`INSERT INTO t2 VALUES(4,5,6)`, `SELECT a FROM t1 ORDER BY a`},

	// REGEXP inside a trigger body: the CREATE succeeds on both engines, and
	// only the INSERT that fires it fails (no regexp() function exists).
	{`CREATE TABLE rx(p)`, `CREATE TRIGGER rxt AFTER INSERT ON t2 BEGIN INSERT INTO rx VALUES(new.x REGEXP 'abc'); END`,
		`INSERT INTO t2 VALUES(9,9,9)`, `SELECT count(*) FROM rx`, `SELECT count(*) FROM t2`},

	// UPDATE SET (col, ...) = <row-value>, expanded into ordinary assignments.
	{`UPDATE t1 SET (c) = 99 WHERE a=1`, `SELECT a,b,c FROM t1 ORDER BY a`},
	{`UPDATE t1 SET (b,c) = (a,b) WHERE a=1`, `SELECT a,b,c FROM t1 ORDER BY a`},
	{`UPDATE t1 SET (b,c) = (c,b)`, `SELECT a,b,c FROM t1 ORDER BY a`}, // a swap, not a double copy
	{`UPDATE t1 SET (b,c) = (SELECT y,z FROM t2 WHERE x=t1.a)`, `SELECT a,b,c FROM t1 ORDER BY a`},
	{`UPDATE t1 SET (b,c) = (SELECT y,z FROM t2 WHERE x=999)`, `SELECT a,b,c FROM t1 ORDER BY a`}, // no row -> NULLs
	{`UPDATE t1 SET a = 8, (b,c) = (SELECT 123,456) WHERE a=1`, `SELECT a,b,c FROM t1 ORDER BY a`},
	{`UPDATE t1 SET (b,c) = (1,2), (a) = (9) WHERE a=3`, `SELECT a,b,c FROM t1 ORDER BY a`},
	{`UPDATE t1 SET (b,b) = (SELECT 1,2) WHERE a=1`, `SELECT a,b,c FROM t1 ORDER BY a`}, // a repeat is legal
	{`UPDATE t1 SET (b,c) = (SELECT y FROM t2)`},                                        // 2 columns assigned 1 values
	{`UPDATE t1 SET (b,c) = (SELECT x,y,z FROM t2)`},                                    // 2 columns assigned 3 values
	{`UPDATE t1 SET (b,c) = 5`},                 //   "
	{`UPDATE t1 SET (nosuch,c) = (SELECT 1,2)`}, // no such column
	{`CREATE TABLE up1(k PRIMARY KEY,m,n)`, `INSERT INTO up1 VALUES(1,2,3)`,
		`INSERT INTO up1 VALUES(1,9,9) ON CONFLICT(k) DO UPDATE SET (m,n)=(SELECT 7,8)`,
		`INSERT INTO up1 VALUES(1,9,9) ON CONFLICT(k) DO UPDATE SET (m,n)=(70,80)`,
		`SELECT * FROM up1`},
	{`CREATE TABLE tg(p,q,r)`, `INSERT INTO tg VALUES(0,0,0)`,
		`CREATE TRIGGER tgr AFTER INSERT ON t2 BEGIN UPDATE tg SET (p,q,r)=(SELECT new.x,new.x+1,new.x+2); END`,
		`INSERT INTO t2 VALUES(5,6,7)`, `SELECT * FROM tg`},

	// A single-quoted STRING as a name on the write side.
	{`INSERT INTO 'sq'('!c') VALUES(6)`, `SELECT * FROM 'sq'`},
	{`UPDATE 'sq' SET '#d'=11`, `SELECT * FROM 'sq'`},
	{`DELETE FROM 'sq' WHERE '!c'=5`, `SELECT * FROM 'sq'`}, // a LITERAL: deletes nothing
	{`DELETE FROM 'sq' WHERE "!c"=5`, `SELECT * FROM 'sq'`}, // the column: deletes the row
	{`DROP TABLE 'sq'`, `SELECT count(*) FROM sqlite_master WHERE name='sq'`},

	// The source SELECT of an INSERT may carry its own WITH clause, written
	// AFTER the table name (SQLite's "insert_cmd INTO xfullname idlist_opt
	// select", where select itself may begin with WITH).
	{`CREATE TABLE wt(n)`,
		`INSERT INTO wt WITH s(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM s WHERE x<10) SELECT * FROM s`,
		`SELECT n FROM wt ORDER BY n`},
	{`CREATE TABLE wl(k,v,w)`,
		`INSERT INTO wl WITH map(k,v) AS (VALUES(1,'cte1'),(2,'cte2')) SELECT 1, (SELECT v FROM map WHERE k=1), 2`,
		`SELECT * FROM wl`},

	// A CTE-prefixed derived table inside a view body and inside an INSERT.
	{`CREATE VIEW vv AS SELECT * FROM (WITH abc(d,e) AS (SELECT x,y FROM t2) SELECT * FROM abc)`, `SELECT * FROM vv`},
	{`CREATE TABLE seqt(n)`,
		`INSERT INTO seqt SELECT * FROM (WITH RECURSIVE seq(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n<5) SELECT n FROM seq)`,
		`SELECT n FROM seqt ORDER BY n`},
}

// TestInsertSelectReturningRuns pins the PARSE half of
// "INSERT ... SELECT ... RETURNING". The SELECT's last FROM item used to
// swallow RETURNING as its own implicit alias, so the statement died in the
// parser complaining about whatever followed ("INSERT: unexpected trailing
// input near \"*\""). C SQLite reserves the word -- "SELECT a RETURNING
// FROM t" is a syntax error there, verified directly -- so it can never be an
// alias.
//
// This used to assert the statement reached the EXECUTOR and was declined
// there, "so the day the executor learns the shape, this test is the thing
// that says the parse was already right". That day came: the executor captures
// the SELECT source's row images now (insert_write.go's insertFromSelect), so
// the statement runs and the parse claim is proved by its RESULT.
func TestInsertSelectReturningRuns(t *testing.T) {
	edb, _ := newParserGapPair(t)
	cols, rows, err := edb.ExecReturningArgs(`INSERT INTO t2 SELECT * FROM t1 RETURNING *`, nil)
	if err != nil {
		t.Fatalf("INSERT ... SELECT ... RETURNING: %v", err)
	}
	if len(cols) == 0 {
		t.Errorf("RETURNING reported no columns: cols=%v rows=%v", cols, rows)
	}
}

func TestParserGapParity(t *testing.T) {
	for _, c := range parserGapCases {
		t.Run(c, func(t *testing.T) {
			edb, sdb := newParserGapPair(t)
			p, err := edb.SnapshotPager()
			if err != nil {
				t.Fatalf("SnapshotPager: %v", err)
			}
			defer p.Close()
			gotCols, gotRows, eerr := p.Query(c)
			cgoCols, cgoRows, cerr := cgoSelect(t, sdb, c, nil)
			if (eerr == nil) != (cerr == nil) {
				t.Fatalf("accept/reject disagrees\n  engine=%v\n  cgo=%v", eerr, cerr)
			}
			if eerr != nil {
				return
			}
			norm := make([][]string, len(gotRows))
			for i, r := range gotRows {
				cells := make([]string, len(r))
				for j, v := range r {
					cells[j] = normalizeEngineValue(v)
				}
				norm[i] = cells
			}
			ordered := strings.Contains(strings.ToUpper(c), "ORDER BY")
			if ok, why := queryResultsMatch(gotCols, norm, cgoCols, cgoRows, ordered); !ok {
				t.Errorf("%s\n  %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", c, why, gotCols, norm, cgoCols, cgoRows)
			}
		})
	}
}

func TestParserGapWriteParity(t *testing.T) {
	for _, seq := range parserGapWriteCases {
		t.Run(seq[0], func(t *testing.T) {
			edb, sdb := newParserGapPair(t)
			for _, s := range seq {
				if isParserGapQuery(s) {
					p, err := edb.SnapshotPager()
					if err != nil {
						t.Fatalf("SnapshotPager: %v", err)
					}
					gotCols, gotRows, eerr := p.Query(s)
					p.Close()
					cgoCols, cgoRows, cerr := cgoSelect(t, sdb, s, nil)
					if (eerr == nil) != (cerr == nil) {
						t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
					}
					if eerr != nil {
						continue
					}
					norm := make([][]string, len(gotRows))
					for i, r := range gotRows {
						cells := make([]string, len(r))
						for j, v := range r {
							cells[j] = normalizeEngineValue(v)
						}
						norm[i] = cells
					}
					ordered := strings.Contains(strings.ToUpper(s), "ORDER BY")
					if ok, why := queryResultsMatch(gotCols, norm, cgoCols, cgoRows, ordered); !ok {
						t.Errorf("[%s] %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", s, why, gotCols, norm, cgoCols, cgoRows)
					}
					continue
				}
				eerr := edb.Exec(s)
				_, cerr := sdb.Exec(s)
				if (eerr == nil) != (cerr == nil) {
					t.Fatalf("[%s] accept/reject disagrees\n  engine=%v\n  cgo=%v", s, eerr, cerr)
				}
			}
		})
	}
}

// isParserGapQuery routes a statement to the read side or the write side the
// same way the mined-corpus harness does -- by its leading verb, so a
// "DELETE ... RETURNING" is an exec on both engines, not a query.
func isParserGapQuery(s string) bool {
	verb, ok := engine.LeadingStatementVerb(s)
	if !ok {
		return false
	}
	switch verb {
	case "SELECT", "VALUES", "WITH":
		return true
	}
	return false
}

// newParserGapPair builds one fresh engine DB and one fresh cgo connection,
// both loaded with parserGapSetup. A pair per case keeps the write cases from
// seeing each other's mutations.
func newParserGapPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "pg.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Discard() })
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	sdb.SetMaxOpenConns(1)
	t.Cleanup(func() { sdb.Close() })
	for _, s := range parserGapSetup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := sdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	return edb, sdb
}
