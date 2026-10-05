// This file is the differential gate for the FROM-less statement shapes this
// engine used to decline outright -- the conformance corpus's single largest
// "VDBE-only: FROM-less statement not compilable to bytecode" bucket, which
// named a SHAPE and no construct until the compiler's own reason was threaded
// through it (tryVDBENoFrom, vdbe_run.go). Each test below pins one construct
// that bucket turned out to be made of, against C SQLite as the oracle.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// flLockstep runs script against both engines statement by statement, failing
// on any accept/reject disagreement, then compares the result of every query in
// verify. It is deliberately strict about WHICH statement diverged: a trigger
// script that silently stops applying halfway looks identical to one that never
// ran at all when only the final table state is compared.
func flLockstep(t *testing.T, name string, script []string, verify ...string) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("[%s] engine.Create: %v", name, err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("[%s] sql.Open: %v", name, err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for i, s := range script {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("[%s] stmt #%d accept/reject disagrees: %s\n  engine: %v\n  cgo:    %v", name, i, s, eErrOrNil(eerr), cerr)
		}
	}
	for _, q := range verify {
		flCompareQuery(t, name, edb, cdb, q)
	}
}

// flCompareQuery runs q on both engines and compares column names and rows.
func flCompareQuery(t *testing.T, name string, edb *engine.Session, cdb *sql.DB, q string) {
	t.Helper()
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("[%s] SnapshotPager: %v", name, err)
	}
	defer p.Close()
	ec, ev, eerr := p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(t, cdb, q, nil)
	if (eerr == nil) != (cerr == nil) {
		t.Errorf("[%s] %s accept/reject disagrees\n  engine: %v\n  cgo:    %v", name, q, eErrOrNil(eerr), cerr)
		return
	}
	if eerr != nil {
		return
	}
	if ok, reason := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, true); !ok {
		t.Errorf("[%s] %s DIVERGES: %s\n  engine: %v %v\n  cgo:    %v %v", name, q, reason, ec, engineRowsToStrings(ev), cc, cr)
	}
}

// flExprParity compares each statement on both engines: accept/reject, the
// error text when both reject, and otherwise column names and rows. Column
// names matter as much as values here -- an unaliased result column is named
// from its VERBATIM source text, so a construct that compiles but renders its
// own name differently is still a divergence.
func flExprParity(t *testing.T, stmts ...string) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	for _, q := range stmts {
		flCompareQuery(t, "expr", edb, cdb, q)
	}
}

// substring() is substr()'s SQL-standard spelling -- the same function under a
// second name, error message included.
func TestFromlessSubstringParity(t *testing.T) {
	flExprParity(t,
		`SELECT ifnull(substring('abcdefg',NULL),'nil')`,
		`SELECT substring('abcdefg',2,3)`,
		`SELECT substring('abcdefg',2)`,
		`SELECT substring('abcdefg')`,
		`SELECT substring(x'6162636465',2,2)`,
		`SELECT typeof(substring('abc',1,1))`,
	)
}

// strftime()/format()/printf() with NO arguments at all is legal and yields
// NULL -- they are registered fully variadic and return without setting a
// result. concat() is the contrast that keeps the rule honest: it really does
// reject its 0-argument form.
func TestFromlessZeroArgVariadicParity(t *testing.T) {
	flExprParity(t,
		`SELECT strftime()`,
		`SELECT typeof(strftime())`,
		`SELECT quote(format()), quote(format(NULL,1,2,3))`,
		`SELECT quote(printf())`,
		`SELECT quote(printf(NULL))`,
		`SELECT format('%d',5)`,
		`SELECT quote(concat())`,
	)
}

// The other half of the same rule: a bare "(SELECT b)" beside a GROUP BY is a
// FROM-less subquery correlated to the COLLAPSED group, whose row also lives
// only in an evalCtx. The values it must see are the same ones a BARE column
// of a grouped query shows, so the min()/max() cases are the load-bearing ones
// -- SQLite makes a bare column follow the min/max row, and a wrong answer
// here would look like a plausible one.
func TestFromlessGroupedCorrelatedSubqueryParity(t *testing.T) {
	flLockstep(t, "grouped correlated FROM-less subquery", []string{
		`CREATE TABLE g(a,b,c)`,
		`INSERT INTO g VALUES(1,10,'x'),(1,20,'y'),(1,30,'z'),(2,40,'p'),(2,50,'q')`,
		`CREATE TABLE t2(c,d)`,
		`INSERT INTO t2 VALUES(1,10),(2,40)`,
	},
		`SELECT a, b FROM g GROUP BY a ORDER BY a`,
		`SELECT a, (SELECT b) FROM g GROUP BY a ORDER BY a`,
		`SELECT a, (SELECT b), (SELECT c) FROM g GROUP BY a ORDER BY a`,
		`SELECT a, max(b), (SELECT b) FROM g GROUP BY a ORDER BY a`,
		`SELECT a, min(b), (SELECT b) FROM g GROUP BY a ORDER BY a`,
		`SELECT a, (SELECT b+1) FROM g GROUP BY a ORDER BY a`,
		`SELECT a, count(*), (SELECT c) FROM g GROUP BY a ORDER BY a`,
		`SELECT c, (SELECT b) FROM g GROUP BY a ORDER BY a`,
		// subquery.test's own two, the ones in the corpus bucket.
		`SELECT a, (SELECT (SELECT d FROM t2 WHERE a=c)) FROM g GROUP BY a ORDER BY a`,
	)
}

// A lone "*" is a legal spelling of an EMPTY argument list for ANY function,
// not just count(*): SQLite parses it as zero arguments and then applies the
// ordinary arity rule, so a 0-argument function answers and every other one
// reports a wrong-argument-count error rather than a syntax error.
func TestFromlessFuncStarArgParity(t *testing.T) {
	flLockstep(t, "f(*) is f()",
		[]string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1)`},
		`SELECT sqlite_version(*)`,
		`SELECT sqlite_version(*) FROM t`,
		`SELECT changes(*)`,
		`SELECT total_changes(*)`,
		`SELECT last_insert_rowid(*)`,
		`SELECT date(*)`,
		`SELECT typeof(char(*))`,
		`SELECT count(*) FROM t`,
		// Arity still applies at zero args -- these must stay rejections.
		`SELECT abs(*)`,
		`SELECT max(*)`,
		`SELECT length(*)`,
		// DISTINCT with "*" is a syntax error, not an empty argument list.
		`SELECT sqlite_version(DISTINCT *)`,
	)
}

// SQLite's double-quoted-string misfeature: a "..."-spelled identifier that
// resolves to no column becomes a STRING literal. The gate pins the three
// boundaries that keep it from over-firing -- a real column still wins, the
// other two quote spellings do NOT fall back, and a QUALIFIED reference never
// does -- plus the result-column NAME, which for a fallen-back reference is
// the source span with its quotes, not the bare identifier.
func TestFromlessDoubleQuotedStringParity(t *testing.T) {
	flLockstep(t, "double-quoted string",
		[]string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES('col-a')`},
		`SELECT "nosuch"`,
		`SELECT "nosuch" FROM t`,
		`SELECT typeof("nosuch")`,
		`SELECT "nosuch" = 'nosuch'`,
		`SELECT '' <= ""`,
		`SELECT "" <= ''`,
		`SELECT """"""""`,
		`SELECT ""`,
		`SELECT hex("")`,
		`SELECT LIKE("!","","!")""WHeRE""`,
		`SELECT "nosuch" COLLATE NOCASE = 'NOSUCH'`,
		`SELECT count(*) FROM t WHERE "nosuch"='x'`,
		`SELECT "nosuch" FROM t GROUP BY "nosuch"`,
		`SELECT "nosuch" FROM t ORDER BY "nosuch"`,
		`SELECT "nosuch" AS z`,
		`SELECT 1 WHERE ""`,
		// A real column always wins over the fallback, and then the column's
		// own declared name is reported (not the quoted span).
		`SELECT "a" FROM t`,
		// The fallback is spelling-specific and unqualified-only: these three
		// must all stay hard "no such column" rejections on BOTH engines.
		`SELECT [nosuch]`,
		"SELECT `nosuch`",
		`SELECT t."nosuch" FROM t`,
		// The other FallbackLiteral rule still behaves, naming included.
		`SELECT true`, `SELECT false`, `SELECT "true"`,
	)
}

// CURRENT_DATE/CURRENT_TIME/CURRENT_TIMESTAMP are KEYWORDS in expression
// position: a real column of the same name never shadows one (the OPPOSITE
// precedence from the TRUE/FALSE keyword-literals), and only a QUALIFIED or
// QUOTED spelling reaches the column. The clock values themselves are compared
// only through relationships that hold for any instant -- equality with the
// documented equivalent, typeof, and length -- so this gate cannot flake on a
// second boundary between the two engines.
func TestFromlessCurrentTimeKeywordParity(t *testing.T) {
	flLockstep(t, "current-time keywords",
		[]string{
			`CREATE TABLE q("current_date" TEXT, b TEXT)`,
			`INSERT INTO q VALUES('cd-col','b-col')`,
		},
		`SELECT CURRENT_DATE==date('now')`,
		`SELECT CURRENT_TIME==time('now')`,
		`SELECT CURRENT_TIMESTAMP==datetime('now')`,
		`SELECT typeof(CURRENT_TIMESTAMP), typeof(CURRENT_DATE), typeof(CURRENT_TIME)`,
		`SELECT length(CURRENT_TIME), length(CURRENT_DATE), length(CURRENT_TIMESTAMP)`,
		`SELECT CuRrEnT_dAtE==date('now')`,
		// The keyword WINS over a real column of that name; the qualified and
		// quoted spellings read the column instead.
		`SELECT current_date==date('now') FROM q`,
		`SELECT q.current_date FROM q`,
		`SELECT "current_date" FROM q`,
		`SELECT current_date==date('now'), b FROM q`,
	)
	// A DEFAULT / CHECK clause naming one still parses, keeps its schema text
	// byte-for-byte, and stays constant-eligible.
	flLockstep(t, "current-time keywords in DDL", []string{
		`CREATE TABLE d1(a INT, b DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE d2(a INT, b DEFAULT (CURRENT_TIMESTAMP))`,
		`CREATE TABLE d3(a INT, b DEFAULT CURRENT_DATE, c DEFAULT CURRENT_TIME)`,
		`CREATE TABLE d4(a INT, b DEFAULT (nosuchcol))`,
		`CREATE TABLE d5(x, CHECK(CURRENT_DATE IS NOT NULL))`,
		`INSERT INTO d5 VALUES(1)`,
	}, `SELECT name, sql FROM sqlite_master WHERE name LIKE 'd%' ORDER BY name`, `SELECT * FROM d5`)
}

// A trigger body step that is a FROM-less SELECT naming NEW.*/OLD.* -- the
// "SELECT RAISE(IGNORE) WHERE ..." / "SELECT CASE WHEN old.a=1 THEN
// RAISE(IGNORE) END" idiom SQLite's own test suite leans on. The firing row
// lives only in an evalCtx (there is no cursor for an OpOuterColumn to read),
// and execNoFrom used to ignore that context entirely and decline with
// "no such table: new".
func TestFromlessTriggerNewOldParity(t *testing.T) {
	// tkt3554.test 1.x: a BEFORE INSERT trigger that suppresses the insert with
	// RAISE(IGNORE) when an overlapping row already exists. Oracle: the three
	// inserts leave exactly one row, ('a',9000,12000).
	flLockstep(t, "RAISE(IGNORE) WHERE EXISTS", []string{
		`CREATE TABLE test ( obj, t1, t2, PRIMARY KEY(obj, t1, t2) )`,
		`CREATE TRIGGER test_insert BEFORE INSERT ON test BEGIN
		   UPDATE test SET t1 = new.t1 WHERE obj = new.obj AND new.t1 < t1 AND new.t2 >= t1;
		   UPDATE test SET t2 = new.t2 WHERE obj = new.obj AND new.t2 > t2 AND new.t1 <= t2;
		   SELECT RAISE(IGNORE) WHERE EXISTS (
		     SELECT obj FROM test WHERE obj = new.obj AND new.t1 >= t1 AND new.t2 <= t2);
		 END`,
		`INSERT INTO test VALUES('a', 10000, 11000)`,
		`INSERT INTO test VALUES('a', 9000, 10500)`,
		`INSERT INTO test VALUES('a', 10000, 12000)`,
	}, `SELECT * FROM test ORDER BY obj, t1, t2`)

	// trigger3.test 6: a nested firing -- the AFTER INSERT body's UPDATE fires a
	// BEFORE UPDATE whose FROM-less body step reads OLD.a and RAISE(IGNORE)s the
	// row it matches. Oracle: row (1,2,3) keeps c=3, row (4,5,6) becomes c=10.
	flLockstep(t, "nested RAISE(IGNORE) reading OLD", []string{
		`CREATE TABLE tbl(a,b,c)`,
		`CREATE TABLE tbl2(a,b,c)`,
		`INSERT INTO tbl VALUES(1,2,3)`,
		`INSERT INTO tbl VALUES(4,5,6)`,
		`CREATE TRIGGER before_tbl_update BEFORE UPDATE ON tbl BEGIN
		   SELECT CASE WHEN (old.a = 1) THEN RAISE(IGNORE) END; END`,
		`CREATE TRIGGER after_tbl2_insert AFTER INSERT ON tbl2 BEGIN
		   UPDATE tbl SET c = 10; END`,
		`INSERT INTO tbl2 VALUES (1, 2, 3)`,
	}, `SELECT * FROM tbl ORDER BY a`)

	// trigger3.test 7.x: an INSTEAD OF INSERT trigger on a VIEW whose only body
	// step is a FROM-less CASE over NEW.a. Oracle: new.a=1 rolls back, new.a=2
	// is ignored, and the base table stays empty either way.
	flLockstep(t, "INSTEAD OF INSERT reading NEW", []string{
		`CREATE TABLE tbl(a,b,c)`,
		`CREATE VIEW tbl_view AS SELECT * FROM tbl`,
		`CREATE TRIGGER tbl_view_insert INSTEAD OF INSERT ON tbl_view BEGIN
		   SELECT CASE WHEN (new.a = 1) THEN RAISE(ROLLBACK, 'View rollback')
		               WHEN (new.a = 2) THEN RAISE(IGNORE)
		               WHEN (new.a = 3) THEN RAISE(ABORT, 'View abort') END; END`,
		`INSERT INTO tbl_view VALUES(2, 2, 3)`,
	}, `SELECT * FROM tbl`)

	// triggerG.test 400: an INSTEAD OF DELETE trigger whose whole body is
	// "SELECT old.a" -- the value is discarded, but the reference has to resolve
	// or the DELETE fails.
	flLockstep(t, "INSTEAD OF DELETE reading OLD", []string{
		`CREATE VIEW v0(a) AS SELECT 1234`,
		`CREATE TRIGGER t0001 INSTEAD OF DELETE ON v0 BEGIN SELECT old.a; END`,
		`DELETE FROM v0`,
	}, `SELECT a FROM v0`)
}
