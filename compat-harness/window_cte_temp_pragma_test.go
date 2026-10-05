// Differential tests for window subqueries, temp pragma operations, and
// recursive CTEs. Each section tests a specific gate closure.
package compat

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// wssFixture sets up tables for window and CTE tests.
var wssFixture = []string{
	`CREATE TABLE tx(a INTEGER PRIMARY KEY)`,
	`INSERT INTO tx VALUES(1),(2),(3),(4),(5),(6)`,
	`CREATE TABLE map(v INTEGER PRIMARY KEY, t TEXT)`,
	`INSERT INTO map VALUES(1,'odd'),(2,'even'),(3,'odd'),(4,'even'),(5,'odd'),(6,'even')`,
	`CREATE TABLE t7(a,b)`,
	`INSERT INTO t7(rowid,a,b) VALUES(1,1,3),(2,10,4),(3,100,2)`,
	`CREATE TABLE t1(a,b,c)`,
	`INSERT INTO t1 VALUES(1,2,3),(4,5,6),(7,8,9),(2,2,3),(4,1,1)`,
	`CREATE TABLE t2(x,y)`,
	`INSERT INTO t2 VALUES(1,10),(2,20),(3,30)`,
	`CREATE TABLE tc(k TEXT COLLATE NOCASE, n INT)`,
	`INSERT INTO tc VALUES('a',1),('A',2),('b',3),('B',4)`,
	`CREATE TABLE grp(g,v)`,
	`INSERT INTO grp VALUES(1,10),(1,20),(2,30),(2,40),(3,50)`,
	`CREATE VIEW mv AS SELECT v,t FROM map`,
	`CREATE TABLE j1(id,p)`,
	`INSERT INTO j1 VALUES(1,'x'),(2,'y'),(3,'x')`,
	`CREATE TABLE j2(id,q)`,
	`INSERT INTO j2 VALUES(1,10),(2,20),(3,30)`,
	`CREATE TABLE blb(a,b)`,
	`INSERT INTO blb VALUES(1,x'01'),(2,'s'),(3,2.5),(4,NULL)`,
}

// wssPair opens an engine and a C SQLite database with wssFixture.
func wssPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	cdb.SetMaxOpenConns(1)
	for i, s := range wssFixture {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if eerr != nil || cerr != nil {
			t.Fatalf("fixture #%d %s\n  engine: %v\n  cgo:    %v", i, s, eErrOrNil(eerr), cerr)
		}
	}
	return edb, cdb
}

// wssQuery runs a query on both engines and compares results.
func wssQuery(t *testing.T, edb *engine.Session, cdb *sql.DB, q string, orderSensitive bool) {
	t.Helper()
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	ec, ev, eerr := p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(t, cdb, q, nil)
	if eerr != nil || cerr != nil {
		t.Errorf("%s\n  engine: %v\n  cgo:    %v", q, eErrOrNil(eerr), cerr)
		return
	}
	if ok, reason := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, orderSensitive); !ok {
		t.Errorf("%s DIVERGES: %s\n  engine: %v %v\n  cgo:    %v %v", q, reason, ec, engineRowsToStrings(ev), cc, cr)
	}
}

// TestWindowSpecSubqueryMatches checks that subqueries in window PARTITION BY
// and ORDER BY clauses answer correctly.
func TestWindowSpecSubqueryMatches(t *testing.T) {
	edb, cdb := wssPair(t)
	for _, q := range []string{
		`SELECT rowid, sum(a) OVER (PARTITION BY b IN (SELECT rowid FROM t7)) FROM t7`,
		`SELECT rowid, sum(a) OVER w1 FROM t7 WINDOW w1 AS (PARTITION BY b IN (SELECT rowid FROM t7))`,

		`SELECT sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT sum(a) OVER win FROM tx WINDOW win AS (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a)`,
		`WITH map2 AS (SELECT * FROM map) SELECT sum(a) OVER (PARTITION BY (SELECT t FROM map2 WHERE v=a) ORDER BY a) FROM tx`,
		`WITH map2 AS (SELECT * FROM map) SELECT sum(a) OVER win FROM tx WINDOW win AS (PARTITION BY (SELECT t FROM map2 WHERE v=a) ORDER BY a)`,

		`SELECT (SELECT count(a) OVER (ORDER BY (SELECT sum(y) FROM t2)) + total(a) OVER()) FROM t1`,
		`SELECT (SELECT max(a) OVER (ORDER BY (SELECT sum(a) FROM t1)) + min(a) OVER()) FROM t1`,
		// re-runs per enclosing row, so the frame the reference needs is live.
		`SELECT a, (SELECT sum(v) OVER (PARTITION BY (SELECT CASE WHEN v<tx.a THEN 0 ELSE 1 END)) FROM map LIMIT 1) FROM tx`,
		`SELECT a, (SELECT sum(v) OVER (PARTITION BY (SELECT t FROM map m WHERE m.v=tx.a)) FROM map LIMIT 1) FROM tx`,

		// A spec subquery whose OWN body is an aggregate reading a correlated
		// reference to the window query's row ("tx.a"), through a nested EXISTS.
		// This used to sit in TestWindowSpecSubqueryOuterScopeDeclines as a
		// supposed two-level reach; it is one level, and the aggregate's WHERE
		// is where the correlation is read -- see that test's own comment.
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT count(*) FROM map m WHERE EXISTS(SELECT 1 FROM map n WHERE n.v=m.v AND n.v=tx.a))) FROM tx ORDER BY a`,

		// selectA.test -- a spec subquery whose own body carries a join and a
		// compound subquery of its own.
		`SELECT sum(v) OVER (PARTITION BY (SELECT 0 FROM map m JOIN map n WHERE m.v = (SELECT 1 INTERSECT SELECT n.v) AND m.v = 123)) FROM map`,

		// ORDER BY keyed on a subquery, in every direction/NULLS combination:
		// the key is a TEXT value fetched per row, so a dropped correlation
		// would show up as a constant key and a different order.
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a) DESC) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a) DESC NULLS FIRST) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a+3) NULLS LAST, a) FROM tx`,
		`SELECT a, rank() OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY (SELECT -v FROM map WHERE v=a)) FROM tx`,

		// EXISTS / IN / NOT IN as the partition key.
		`SELECT a, row_number() OVER (PARTITION BY EXISTS(SELECT 1 FROM map WHERE v=a AND t='odd') ORDER BY a) FROM tx`,
		`SELECT a, count(*) OVER (PARTITION BY a IN (SELECT v FROM map WHERE t='odd') ORDER BY a) FROM tx`,
		`SELECT a, count(*) OVER (PARTITION BY a NOT IN (SELECT v FROM map WHERE t='odd') ORDER BY a) FROM tx`,

		// An UNCORRELATED spec subquery is a constant key -- one partition. It
		// is the half that COULD legitimately be cached, so it is pinned next to
		// the correlated ones rather than assumed equivalent.
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT count(*) FROM map) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT max(v) FROM map), a) FROM tx`,
		// A subquery returning MORE THAN ONE ROW yields its first row's value in
		// C SQLite, not an error.
		`SELECT a, count(*) OVER (PARTITION BY (SELECT t FROM map) ORDER BY a) FROM tx`,

		// The key expression's own COLLATING SEQUENCE has to survive the
		// subquery: tc.k is TEXT COLLATE NOCASE, so 'a'/'A' are PEERS and the
		// rows come out in NOCASE order (windowKeyCollations, window9.test).
		`SELECT k, n, dense_rank() OVER (ORDER BY (SELECT k), n) FROM tc`,
		`SELECT k, n, group_concat(n) OVER (PARTITION BY (SELECT k) ORDER BY n) FROM tc`,
		`SELECT k, n, dense_rank() OVER (ORDER BY (SELECT k COLLATE BINARY), n) FROM tc`,

		`SELECT a, sum(a) OVER (ORDER BY (SELECT v FROM map WHERE v=a) ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT v FROM map WHERE v=a) RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT v FROM map WHERE v=a) GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT v*1.0 FROM map WHERE v=a) RANGE BETWEEN 1.5 PRECEDING AND CURRENT ROW) FROM tx`,
		`SELECT a, lag(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT a, nth_value(a,2) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT a, ntile(2) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) FILTER (WHERE a>2) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) FILTER (WHERE a<>3) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) FROM tx`,

		`SELECT a, sum(a) OVER (w ORDER BY a) FROM tx WINDOW w AS (PARTITION BY (SELECT t FROM map WHERE v=a))`,
		`SELECT a, sum(a) OVER w2 FROM tx WINDOW w AS (PARTITION BY (SELECT t FROM map WHERE v=a)), w2 AS (w ORDER BY a)`,
		`SELECT a, sum(a) OVER wa, count(*) OVER wa FROM tx WINDOW wa AS (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a)`,

		`SELECT a, sum(a) OVER (ORDER BY (SELECT count(*) FROM tx AS u WHERE u.a<=tx.a)) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT count(*) FROM tx AS u WHERE u.a<tx.a)%2 ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT group_concat(u.a) FROM tx u WHERE u.a<=tx.a) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT CASE WHEN v=a THEN t ELSE NULL END FROM map ORDER BY 1 LIMIT 1) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT max(t) FROM map WHERE abs(v-a)=0) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT count(*) FROM map m WHERE m.v IN (SELECT v FROM map WHERE v=tx.a)) ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v>=a ORDER BY v LIMIT 1), a) FROM tx`,

		`SELECT j1.id, sum(j2.q) OVER (PARTITION BY (SELECT t FROM map WHERE v=j1.id+j2.id) ORDER BY j1.id) FROM j1 JOIN j2 ON j1.id=j2.id`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM mv WHERE v=a) ORDER BY a) FROM tx`,
		`SELECT q.a, sum(q.a) OVER (PARTITION BY (SELECT t FROM map WHERE v=q.a) ORDER BY q.a) FROM tx AS q`,
		`SELECT sum(v) OVER (PARTITION BY (SELECT t FROM map m2 WHERE m2.v=map.v) ORDER BY v) FROM map`,

		`SELECT a, count(*) OVER (PARTITION BY (SELECT b FROM blb x WHERE x.a=blb.a) ORDER BY a) FROM blb`,
		`SELECT a, count(*) OVER (ORDER BY (SELECT b FROM blb x WHERE x.a=blb.a)) FROM blb`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a+100) ORDER BY a) FROM tx`,

		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) AS s FROM tx ORDER BY s, a`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx ORDER BY 2, 1 LIMIT 3`,
		`SELECT DISTINCT sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a)) FROM tx ORDER BY 1`,
		`SELECT * FROM (SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=tx.a)) AS s FROM tx) ORDER BY a`,
		`WITH q AS (SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) AS s FROM tx) SELECT * FROM q ORDER BY a`,
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a)) FROM tx UNION ALL SELECT 99, 99 ORDER BY 1`,
		`SELECT a FROM tx ORDER BY sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a)), a`,
		`SELECT a FROM tx WHERE a = (SELECT max(x) FROM (SELECT a AS x, row_number() OVER (ORDER BY (SELECT t FROM map WHERE v=a)) AS r FROM tx) WHERE r<=2)`,
		`SELECT a FROM tx WHERE EXISTS (SELECT sum(v) OVER (PARTITION BY (SELECT t FROM map m WHERE m.v=map.v)) FROM map WHERE map.v=tx.a) ORDER BY a`,
		// The same subquery text used as BOTH keys, and next to an identical
		// select-list subquery -- sameWindowSpec compares specs structurally.
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY (SELECT t FROM map WHERE v=a), a) FROM tx`,
		`SELECT a, (SELECT t FROM map WHERE v=a) AS lbl, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
	} {
		wssQuery(t, edb, cdb, q, true)
	}
}

// TestWindowSpecSubqueryWritePath checks window subqueries in write paths.
func TestWindowSpecSubqueryWritePath(t *testing.T) {
	edb, cdb := wssPair(t)
	for _, s := range []string{
		`CREATE TABLE dst(a,s)`,
		`INSERT INTO dst SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`,
		`CREATE TABLE dst2 AS SELECT a, sum(a) OVER (ORDER BY (SELECT t FROM map WHERE v=a)) AS s FROM tx`,
		`DELETE FROM dst WHERE a IN (SELECT a FROM (SELECT a, row_number() OVER (ORDER BY (SELECT t FROM map WHERE v=a), a) r FROM tx) WHERE r=1)`,
	} {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("%s accept/reject disagrees\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
		}
	}
	wssQuery(t, edb, cdb, `SELECT * FROM dst ORDER BY a`, true)
	wssQuery(t, edb, cdb, `SELECT * FROM dst2 ORDER BY a`, true)
}

// TestWindowSpecSubqueryOuterScopeDeclines is GONE, and its shapes with it.
//
// TestWindowSpecSubqueryEmitOrderIsNotThisRule checks that row order
// differences are due to window ordering, not subqueries.
func TestWindowSpecSubqueryEmitOrderIsNotThisRule(t *testing.T) {
	edb, cdb := wssPair(t)
	for _, q := range []string{
		`SELECT a, sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a)), count(*) OVER (ORDER BY a) FROM tx`,
		`SELECT a, sum(a) OVER (PARTITION BY a%2), count(*) OVER (ORDER BY a) FROM tx`,
	} {
		wssQuery(t, edb, cdb, q, false)
	}
}

// PRAGMA temp.integrity_check and temp.quick_check tests.
// keeps meaning "still a gap" rather than quietly becoming "both refuse".

// tempPragmaPair opens the two engines with no fixture: the first assertion has
// to hold on a connection whose temp database has never been opened at all.
func tempPragmaPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	cdb.SetMaxOpenConns(1)
	return edb, cdb
}

// tempPragmaCells runs one pragma through the READ side of both engines and
// compares the column names and every cell. Nothing here goes through Exec,
// which is exactly the hole the mined-TCL corpus has.
func tempPragmaCells(t *testing.T, edb *engine.Session, cdb *sql.DB, q string) {
	t.Helper()
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	ec, ev, eerr := p.QueryArgs(q, nil)
	cc, cr, cerr := cgoSelect(t, cdb, q, nil)
	if eerr != nil || cerr != nil {
		t.Errorf("%s\n  engine: %v\n  cgo:    %v", q, eErrOrNil(eerr), cerr)
		return
	}
	if ok, reason := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, true); !ok {
		t.Errorf("%s DIVERGES: %s\n  engine: %v %v\n  cgo:    %v %v", q, reason, ec, engineRowsToStrings(ev), cc, cr)
	}
}

// TestTempIntegrityCheckMatchesOracle checks that PRAGMA integrity_check and
// quick_check work for temp databases.
func TestTempIntegrityCheckMatchesOracle(t *testing.T) {
	edb, cdb := tempPragmaPair(t)
	for _, q := range []string{
		`PRAGMA temp.integrity_check`,
		`PRAGMA temp.quick_check`,
		`PRAGMA temp.integrity_check(1)`,
		`PRAGMA temp.quick_check(1)`,
		`PRAGMA TEMP.integrity_check`,
		`PRAGMA "temp".integrity_check`,
		`PRAGMA main.integrity_check`,
		`PRAGMA integrity_check`,
	} {
		tempPragmaCells(t, edb, cdb, q)
	}
	for _, s := range []string{
		`CREATE TEMP TABLE a(x PRIMARY KEY, y UNIQUE)`,
		`INSERT INTO a VALUES(1,1),(2,2),(3,3)`,
		`CREATE TEMP TRIGGER tr AFTER INSERT ON a BEGIN SELECT 1; END`,
		`CREATE TEMP VIEW vv AS SELECT * FROM a`,
	} {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("%s accept/reject disagrees\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
		}
	}
	for _, q := range []string{
		`PRAGMA temp.integrity_check`,
		`PRAGMA temp.quick_check`,
		`PRAGMA main.integrity_check`,
	} {
		tempPragmaCells(t, edb, cdb, q)
	}
}

// TestTempFileScopedPragmasStillDecline checks that some temp pragmas still decline.
func TestTempFileScopedPragmasStillDecline(t *testing.T) {
	edb, cdb := tempPragmaPair(t)
	for _, s := range []string{`CREATE TEMP TABLE a(x)`, `INSERT INTO a VALUES(1),(2)`} {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, q := range []string{`PRAGMA temp.page_count`, `PRAGMA temp.freelist_count`} {
		p, err := edb.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		_, er, eerr := p.QueryArgs(q, nil)
		p.Close()
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Errorf("%s: engine=%v cgo=%v; both must answer", q, eerr, cerr)
			continue
		}
		if len(er) != 1 || len(cr) != 1 || len(er[0]) != 1 {
			t.Errorf("%s: engine=%v cgo=%v; want one row each", q, er, cr)
			continue
		}
		if got, want := "I:"+strconv.FormatInt(er[0][0].I, 10), cr[0][0]; got != want && q != `PRAGMA temp.page_count` {
			t.Errorf("%s: engine=%s, oracle=%s", q, got, want)
		}
	}
	for _, tc := range []struct {
		q        string
		wantRows int // rows the oracle answers; 0 means "accepted, but empty"
	}{
		{`PRAGMA temp.cache_spill`, 1},
	} {
		p, err := edb.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		_, _, eerr := p.QueryArgs(tc.q, nil)
		p.Close()
		if eerr == nil {
			t.Errorf("%s: engine now ANSWERS this -- compare it against the oracle cell by cell and move it into TestTempIntegrityCheckMatchesOracle", tc.q)
			continue
		}
		_, cr, cerr := cgoSelect(t, cdb, tc.q, nil)
		if cerr != nil {
			t.Errorf("%s: C SQLite now rejects this too (%v) -- the premise of this test is gone", tc.q, cerr)
			continue
		}
		if len(cr) != tc.wantRows {
			t.Errorf("%s: oracle answered %d row(s), expected %d -- the recorded evidence for this decline has drifted", tc.q, len(cr), tc.wantRows)
		}
	}
}

// ---------------------------------------------------------------------------
// PART THREE: a recursive CTE with MORE THAN ONE recursive arm.
//
// detectRecursiveShape (cte.go) used to recognize exactly one self-referencing
// arm -- the LAST one -- and folded everything before it into the initial part,
// so "VALUES(1) UNION <rec1> UNION <rec2>" failed selectReferencesTable on that
// initial part and fell through to the ordinary CTE path, whose circularity
// guard reported "circular reference: closure". That is with5.test's shape: a
// bidirectional graph walk, where one arm follows edges forwards and the other
// backwards.
//
// Nothing about the recursion needed to change. The recursion is already a
// QUEUE that pops ONE pending row and expands it; N arms simply means expanding
// that row with each arm in turn, into the same queue. C SQLite constrains
// the arms enough that one dedup policy always governs the whole recursion --
// see TestMultiArmRecursiveCTEDeclines for the two rejections that pin it.
//
// Every expectation below is mattn/go-sqlite3's.

// marcPair opens the two engines over one graph, the shape with5.test uses.
func marcPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	cdb.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE link(aa,bb)`,
		`INSERT INTO link VALUES(1,2),(2,3),(3,4),(5,6),(6,7),(2,5)`,
		`CREATE TABLE linkA(aa1,aa2)`, `INSERT INTO linkA VALUES(1,2),(2,3)`,
		`CREATE TABLE linkB(bb1,bb2)`, `INSERT INTO linkB VALUES(1,4),(4,5)`,
		`CREATE TABLE linkC(cc1,cc2)`, `INSERT INTO linkC VALUES(2,6)`,
		`CREATE TABLE linkD(dd1,dd2)`, `INSERT INTO linkD VALUES(3,7),(7,8)`,
		`CREATE TABLE org(id,parent,nm)`,
		`INSERT INTO org VALUES(1,NULL,'root'),(2,1,'a'),(3,1,'b'),(4,2,'c'),(5,3,'d')`,
		`CREATE TABLE dst(x)`,
	} {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if eerr != nil || cerr != nil {
			t.Fatalf("fixture %s\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
		}
	}
	return edb, cdb
}

// TestMultiArmRecursiveCTEMatches is the rule: N recursive arms answer exactly
// what C SQLite answers, row for row and in order.
func TestMultiArmRecursiveCTEMatches(t *testing.T) {
	edb, cdb := marcPair(t)
	for _, q := range []string{
		// The nine mined with5.test statements, verbatim in shape. The first
		`WITH RECURSIVE closure(x) AS ( VALUES(1) UNION SELECT aa FROM closure, link WHERE link.bb=closure.x UNION SELECT bb FROM closure, link WHERE link.aa=closure.x ) SELECT x FROM closure ORDER BY x`,
		`WITH RECURSIVE closure(x) AS ( VALUES(1) UNION SELECT aa FROM link, closure WHERE link.bb=closure.x UNION SELECT bb FROM closure, link WHERE link.aa=closure.x ) SELECT x FROM closure ORDER BY x`,
		`WITH RECURSIVE closure(x) AS ( VALUES(1) UNION SELECT bb FROM closure, link WHERE link.aa=closure.x UNION SELECT aa FROM link, closure WHERE link.bb=closure.x ) SELECT x FROM closure ORDER BY x`,
		`WITH RECURSIVE closure(x) AS ( VALUES(1),(200),(300),(400) INTERSECT VALUES(1) UNION SELECT bb FROM closure, link WHERE link.aa=closure.x UNION SELECT aa FROM link, closure WHERE link.bb=closure.x ) SELECT x FROM closure ORDER BY x`,
		`WITH RECURSIVE closure(x) AS ( VALUES(1),(200),(300),(400) UNION ALL VALUES(2) UNION SELECT bb FROM closure, link WHERE link.aa=closure.x UNION SELECT aa FROM link, closure WHERE link.bb=closure.x ) SELECT x FROM closure ORDER BY x`,
		// ...with the CTE's own ORDER BY + LIMIT, which is the recursion's queue
		// discipline and output bound rather than decoration.
		`WITH RECURSIVE closure(x) AS ( SELECT 1 AS x UNION SELECT aa FROM link JOIN closure ON bb=x UNION SELECT bb FROM link JOIN closure on aa=x ORDER BY x LIMIT 4 ) SELECT * FROM closure`,
		`WITH RECURSIVE closure(x) AS ( SELECT 1 AS x UNION ALL SELECT 2 UNION SELECT aa FROM link JOIN closure ON bb=x UNION SELECT bb FROM link JOIN closure on aa=x ORDER BY x LIMIT 4 ) SELECT * FROM closure`,
		// ...and FOUR arms over four different tables, UNION ALL throughout.
		`WITH RECURSIVE closure(x) AS ( VALUES(1) UNION ALL SELECT aa2 FROM linkA JOIN closure ON x=aa1 UNION ALL SELECT bb2 FROM linkB JOIN closure ON x=bb1 UNION ALL SELECT cc2 FROM linkC JOIN closure ON x=cc1 UNION ALL SELECT dd2 FROM linkD JOIN closure ON x=dd1 ) SELECT x FROM closure ORDER BY +x`,
		`WITH RECURSIVE closure(x) AS ( VALUES(1) UNION ALL SELECT aa2 FROM linkA JOIN closure ON x=aa1 UNION ALL SELECT bb2 FROM linkB JOIN closure ON x=bb1 UNION ALL SELECT cc2 FROM linkC JOIN closure ON x=cc1 UNION ALL SELECT dd2 FROM linkD JOIN closure ON x=dd1 ) SELECT x FROM closure`,

		// The EXPANSION ORDER, unmasked: no ORDER BY anywhere, UNION ALL so
		// nothing is deduped away, and two arms that both fire on every row. Its
		// 17 rows come out 1,2,3,4,6,6,9,8,12,12,18,12,18,18,27,16,24 -- pop one
		// row, run BOTH arms, append. Neither a round-by-round nor an
		// arm-by-arm expansion produces that sequence.
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x*2 FROM c WHERE x<10 UNION ALL SELECT x*3 FROM c WHERE x<10) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3 UNION ALL SELECT x+10 FROM c WHERE x<3) SELECT x FROM c`,

		// The SEED dedup follows the recursive arms' own operator: over an
		// initial arm holding (1),(1),(2), UNION answers six rows and UNION ALL
		// answers nine -- the duplicate seed surviving AND being expanded twice.
		`WITH RECURSIVE c(x) AS (VALUES(1),(1),(2) UNION SELECT x+10 FROM c WHERE x<3 UNION SELECT x+100 FROM c WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1),(1),(2) UNION ALL SELECT x+10 FROM c WHERE x<3 UNION ALL SELECT x+100 FROM c WHERE x<3) SELECT x FROM c`,

		// Three arms; an arm that never produces a row; a cyclic graph, where
		// only UNION's dedup makes the walk terminate at all.
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<4 UNION SELECT x+2 FROM c WHERE x<4 UNION SELECT x+3 FROM c WHERE x<4) SELECT x FROM c ORDER BY x`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<4 UNION ALL SELECT x FROM c WHERE 0) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x) SELECT x FROM c ORDER BY x`,

		// Multi-column rows, a DESC queue, an OFFSET, and NULL/mixed-storage-class
		// rows through the dedup.
		`WITH RECURSIVE c(x,d) AS (VALUES(1,0) UNION SELECT link.bb, c.d+1 FROM link,c WHERE link.aa=c.x AND c.d<4 UNION SELECT link.aa, c.d+1 FROM link,c WHERE link.bb=c.x AND c.d<4) SELECT x,min(d) FROM c GROUP BY x ORDER BY x`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x ORDER BY 1 DESC LIMIT 5) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x ORDER BY 1 LIMIT 5 OFFSET 2) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(NULL) UNION SELECT NULL FROM c UNION SELECT 1 FROM c WHERE x IS NULL) SELECT x FROM c ORDER BY x`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT 1.0 FROM c UNION SELECT '1' FROM c) SELECT typeof(x), x FROM c ORDER BY 1,2`,
		`WITH RECURSIVE c(s) AS (SELECT 'a' UNION SELECT upper(s) FROM c WHERE length(s)<2 UNION SELECT s||'z' FROM c WHERE length(s)<2) SELECT s FROM c ORDER BY s`,

		// A tree walk whose second arm re-finds the row it was given, so every
		// popped row produces a duplicate that only the dedup removes.
		`WITH RECURSIVE c(id,nm) AS (SELECT id,nm FROM org WHERE parent IS NULL UNION SELECT o.id,o.nm FROM org o, c WHERE o.parent=c.id UNION SELECT o.id,o.nm FROM org o, c WHERE o.id=c.id) SELECT id,nm FROM c ORDER BY id`,

		// The multi-arm CTE consumed by an aggregate, a join, a nested scalar
		// subquery, twice in one FROM, and alongside a second multi-arm CTE.
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x) SELECT count(*), sum(x), max(x) FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x) SELECT link.aa, link.bb FROM link JOIN c ON link.aa=c.x ORDER BY 1,2`,
		`SELECT (SELECT count(*) FROM (WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x) SELECT x FROM c))`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<3 UNION SELECT x+10 FROM c WHERE x<3) SELECT a.x, b.x FROM c a, c b ORDER BY 1,2`,
		`WITH RECURSIVE c1(x) AS (VALUES(1) UNION SELECT x+1 FROM c1 WHERE x<3 UNION SELECT x+10 FROM c1 WHERE x<3), c2(y) AS (VALUES(100) UNION SELECT y+1 FROM c2 WHERE y<102 UNION SELECT y+5 FROM c2 WHERE y<102) SELECT x,y FROM c1,c2 ORDER BY x,y`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3 UNION ALL SELECT x+1 FROM c WHERE x<3) SELECT DISTINCT x FROM c ORDER BY x`,

		// The SINGLE-arm shapes, unchanged by all of this and re-pinned because
		// detectRecursiveShape now finds its arms a different way.
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<5) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<5) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT x+10 FROM c WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x,y) AS (VALUES(1,'a') UNION ALL SELECT x+1, y||'b' FROM c WHERE x<4) SELECT * FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 ORDER BY 1 DESC LIMIT 5) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<20 LIMIT 4 OFFSET 2) SELECT x FROM c`,
	} {
		wssQuery(t, edb, cdb, q, true)
	}
}

// TestMultiArmRecursiveCTEWritePath checks multi-arm CTEs in write paths.
func TestMultiArmRecursiveCTEWritePath(t *testing.T) {
	edb, cdb := marcPair(t)
	s := `WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT bb FROM link,c WHERE link.aa=c.x UNION SELECT aa FROM link,c WHERE link.bb=c.x) INSERT INTO dst SELECT x FROM c`
	eerr := edb.Exec(s)
	_, cerr := cdb.Exec(s)
	if eerr != nil || cerr != nil {
		t.Fatalf("%s\n  engine: %v\n  cgo:    %v", s, eErrOrNil(eerr), cerr)
	}
	wssQuery(t, edb, cdb, `SELECT x FROM dst ORDER BY x`, true)
}

// TestMultiArmRecursiveCTEDeclines checks that invalid multi-arm CTEs decline.
func TestMultiArmRecursiveCTEDeclines(t *testing.T) {
	edb, cdb := marcPair(t)
	for _, q := range []string{
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<3 UNION ALL SELECT x+1 FROM c WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3 UNION SELECT x+1 FROM c WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1),(1),(2) UNION ALL SELECT x+10 FROM c WHERE x<3 UNION SELECT x+100 FROM c WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<3 UNION ALL VALUES(99)) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c, c c2 WHERE x<3) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) EXCEPT SELECT x FROM c) SELECT x FROM c`,
		`WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT count(*) FROM c WHERE x<3 UNION SELECT x+1 FROM c WHERE x<3) SELECT x FROM c`,
	} {
		p, err := edb.SnapshotPager()
		if err != nil {
			t.Fatalf("SnapshotPager: %v", err)
		}
		_, _, eerr := p.QueryArgs(q, nil)
		p.Close()
		_, _, cerr := cgoSelect(t, cdb, q, nil)
		if eerr == nil {
			t.Errorf("%s: engine now ACCEPTS this (oracle: %v) -- if C SQLite answers it too, verify it and move it into TestMultiArmRecursiveCTEMatches", q, cerr)
			continue
		}
		if cerr == nil {
			t.Errorf("%s: C SQLite ANSWERS this -- it is a coverage gap now, not a mutual rejection; re-derive the rule", q)
		}
	}
}

// TestWindowSpecNestedWindowMatches was TestWindowSpecNestedWindowDeclines: a
// spec subquery that nests ANOTHER window function was refused outright, on the
// belief that it "is not a per-row scalar, because the inner window is computed
// over the inner query's own batch".
//
// window.c says it is a per-row scalar. sqlite3WindowRewrite appends a window's
// own PARTITION BY / ORDER BY expressions to its generated sub-select's
// expression list VERBATIM, and selectWindowRewriteExprCb prunes at any nested
// Select -- so the key is evaluated once per row of the base scan like every
// other scalar, and a window written inside it belongs to that subquery, whose
// own rewrite runs when the subquery runs.
//
// So the cases below are now ANSWERS, compared cell for cell, in every position
// that reaches the rule -- the select list, a compound arm, a WHERE, a derived
// table, a CTE, an IN subquery, an EXISTS, inside CASE, and via a WINDOW clause
// the inner query merely DEFINES. The broader battery (correlated keys, frames,
// two-deep nesting, DISTINCT/LIMIT, and 370 generated queries) lives in
// window_r24_nested_spec_test.go.
//
// Order-insensitively, deliberately: these carry no outer ORDER BY, and with a
// key this engine and SQLite may legitimately order ties differently -- see
// TestWindowSpecNestedWindowMatches checks nested windows in subqueries.
func TestWindowSpecNestedWindowMatches(t *testing.T) {
	edb, cdb := wssPair(t)
	for _, q := range []string{
		`SELECT avg(a) OVER (ORDER BY (SELECT sum(b) OVER () FROM t1 ORDER BY (SELECT total(c) OVER (ORDER BY c) FROM (SELECT 1 AS c) ORDER BY 1))) FROM t1`,
		`SELECT row_number() OVER win FROM t1 WINDOW win AS (ORDER BY (SELECT percent_rank() OVER win2 FROM t2 WINDOW win2 AS (ORDER BY x)))`,
		`SELECT sum(a) OVER (PARTITION BY a IN (SELECT row_number() OVER () FROM t2)) FROM t1`,
		`SELECT sum(a) OVER (PARTITION BY EXISTS(SELECT rank() OVER (ORDER BY x) FROM t2)) FROM t1`,
		`SELECT sum(a) OVER (ORDER BY CASE WHEN a>0 THEN (SELECT sum(y) OVER () FROM t2 LIMIT 1) ELSE 0 END) FROM t1`,
		`SELECT sum(a) OVER (ORDER BY (SELECT x FROM t2 WHERE y=(SELECT max(y) OVER () FROM t2 LIMIT 1) LIMIT 1)) FROM t1`,
		`SELECT sum(a) OVER (ORDER BY (SELECT m FROM (SELECT rank() OVER (ORDER BY x) AS m FROM t2) LIMIT 1)) FROM t1`,
		`SELECT sum(a) OVER (ORDER BY (WITH q AS (SELECT rank() OVER (ORDER BY x) AS m FROM t2) SELECT m FROM q LIMIT 1)) FROM t1`,
		`SELECT sum(a) OVER (ORDER BY (SELECT 1 UNION SELECT row_number() OVER () FROM t2 LIMIT 1)) FROM t1`,
	} {
		wssQuery(t, edb, cdb, q, false)
	}
	wssQuery(t, edb, cdb, `SELECT sum(a) OVER (PARTITION BY (SELECT t FROM map WHERE v=a) ORDER BY a) FROM tx`, true)
	wssQuery(t, edb, cdb, `SELECT rowid, sum(a) OVER (PARTITION BY b IN (SELECT rowid FROM t7)) FROM t7`, true)
}
