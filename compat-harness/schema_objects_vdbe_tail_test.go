package compat

// This file gates schema-object and VDBE-tail declines:
// derived tables with LIMIT, REINDEX schema qualification, and sqlite_sequence per-database storage.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// compoundArmLimitSchema deliberately TIES on every ordering column a query
// below sorts by, since a tie is the only thing that could make an inner
// "ORDER BY ... LIMIT n" pick different rows in the two engines. num has three
// 5s; b has two 'y's.
var compoundArmLimitSchema = []string{
	"CREATE TABLE t1(a INTEGER, b TEXT, num INTEGER)",
	"CREATE TABLE t2(a INTEGER, b TEXT, num INTEGER)",
	"INSERT INTO t1 VALUES(1,'x',5),(2,'y',5),(3,'z',5),(4,'y',1),(5,'w',9)",
	"INSERT INTO t2 VALUES(2,'y',5),(6,'q',2),(7,'r',9)",
	"CREATE VIEW v1 AS SELECT a, b FROM t1 ORDER BY num DESC LIMIT 3",
}

// TestCompoundArmInnerLimitParity checks derived tables with LIMIT in compounds.
func TestCompoundArmInnerLimitParity(t *testing.T) {
	inners := []string{
		"SELECT a, b FROM t1 ORDER BY num DESC LIMIT 2",
		"SELECT a, b FROM t1 ORDER BY num DESC LIMIT 2 OFFSET 1",
		"SELECT a, b FROM t1 LIMIT 2",
		"SELECT a, b FROM t1 ORDER BY b LIMIT 3",
		"SELECT a, b FROM t1 LIMIT 100", // a LIMIT that truncates nothing
		"SELECT a, b FROM t1 LIMIT 0",
	}
	ops := []string{"UNION", "UNION ALL", "INTERSECT", "EXCEPT"}
	tails := []string{"", " ORDER BY 1", " ORDER BY 1 LIMIT 2"}
	for _, inner := range inners {
		for _, op := range ops {
			for _, tail := range tails {
				for _, q := range []string{
					fmt.Sprintf("SELECT * FROM (%s) %s SELECT a, b FROM t2%s", inner, op, tail),
					fmt.Sprintf("SELECT a, b FROM t2 %s SELECT * FROM (%s)%s", op, inner, tail),
					fmt.Sprintf("WITH cte AS (%s) SELECT * FROM cte %s SELECT a, b FROM t2%s", inner, op, tail),
				} {
					if !differ(t, "cmparmlimit", append(append([]string(nil), compoundArmLimitSchema...), q)) {
						t.Errorf("diverged on: %s", q)
					}
				}
			}
		}
	}
	// A VIEW whose own body carries the LIMIT: the inner LIMIT is not even in
	// this statement's text, which is the case a reader could never call
	// order-bearing. Plus select6.test's nesting (a compound INSIDE a derived
	// table whose own first arm is another derived table with a LIMIT).
	for _, q := range []string{
		"SELECT * FROM v1 UNION SELECT a, b FROM t2",
		"SELECT * FROM v1 UNION ALL SELECT a, b FROM t2 ORDER BY 1",
		"SELECT a, b FROM t2 EXCEPT SELECT * FROM v1",
		"SELECT * FROM (SELECT * FROM (SELECT a, b FROM t1 LIMIT 1) UNION ALL SELECT a, b from t2)",
		"SELECT * FROM (SELECT a FROM t1 LIMIT 1) UNION ALL SELECT 3",
	} {
		if !differ(t, "cmparmlimitview", append(append([]string(nil), compoundArmLimitSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// reindexScopeSchema builds one object of each kind in EACH catalog, with
// deliberately DISJOINT names so nothing here depends on this engine's
// single-namespace approximation of two catalogs.
var reindexScopeSchema = []string{
	"CREATE TABLE m1(a PRIMARY KEY, b, c)",
	"CREATE INDEX mi1 ON m1(c)",
	"CREATE VIEW mv1 AS SELECT a FROM m1",
	"CREATE TEMP TABLE tt1(x PRIMARY KEY, y, z)",
	"CREATE INDEX ti1 ON tt1(z)",
	"CREATE TEMP VIEW tv1 AS SELECT x FROM tt1",
}

// TestReindexQualifiedScopeParity pins REINDEX's name resolution, which is
// pure accept/reject here (this engine rebuilds every index b-tree from the
// live row store at Close, so REINDEX itself is a no-op -- see
// engine/vacuum_write.go). Accepting one C SQLite rejects is scored WRONG by
// the differential harness, so both directions matter.
//
// Verified directly against mattn/go-sqlite3 3.53.3 with exactly this schema:
// "REINDEX temp.tt1"/"temp.ti1"/"temp.tv1" and "REINDEX main.m1"/"main.mi1"/
// "main.mv1" all succeed, while EVERY cross-catalog spelling ("main.tt1",
// "main.ti1", "temp.m1", "temp.mi1") reports "unable to identify the object to
// TestReindexQualifiedScopeParity checks REINDEX name resolution by schema.
func TestReindexQualifiedScopeParity(t *testing.T) {
	cases := []string{
		"REINDEX",
		"REINDEX m1", "REINDEX main.m1", "REINDEX temp.m1",
		"REINDEX mi1", "REINDEX main.mi1", "REINDEX temp.mi1",
		"REINDEX mv1", "REINDEX main.mv1", "REINDEX temp.mv1",
		"REINDEX tt1", "REINDEX temp.tt1", "REINDEX main.tt1",
		"REINDEX ti1", "REINDEX temp.ti1", "REINDEX main.ti1",
		"REINDEX tv1", "REINDEX temp.tv1", "REINDEX main.tv1",
		"REINDEX BINARY", "REINDEX NOCASE", "REINDEX RTRIM",
		"REINDEX main.NOCASE", "REINDEX temp.NOCASE",
		"REINDEX bogus", "REINDEX main.bogus", "REINDEX temp.bogus",
		"REINDEX aux.m1",
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Discard()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1) // TEMP objects are per-connection
	for _, s := range reindexScopeSchema {
		if eerr := edb.Exec(s); eerr != nil {
			t.Fatalf("engine setup %q: %v", s, eerr)
		}
		if _, cerr := cdb.Exec(s); cerr != nil {
			t.Fatalf("cgo setup %q: %v", s, cerr)
		}
	}
	for _, s := range cases {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Errorf("%q: engine=%v cgo=%v", s, eerr, cerr)
		}
	}
}

// TestSqliteSequencePerDatabaseParity checks sqlite_sequence filtering per database.
func TestSqliteSequencePerDatabaseParity(t *testing.T) {
	steps := []string{
		// tnc's column declares NOCASE and holds two rows differing only in
		// case -- the dedup probe the compound queries below need.
		"CREATE TABLE tnc(nc TEXT COLLATE NOCASE)",
		"INSERT INTO tnc VALUES('T1'),('t1'),('zz')",
		"CREATE TABLE t1(x INTEGER PRIMARY KEY AUTOINCREMENT, y)",
		"CREATE TEMP TABLE t3(a INTEGER PRIMARY KEY AUTOINCREMENT, b)",
		"INSERT INTO t1 VALUES(10,1)",
		"INSERT INTO t3 VALUES(20,2)",
		"INSERT INTO t1 VALUES(NULL,3)",
		"INSERT INTO t3 VALUES(NULL,4)",
		"INSERT INTO t1 SELECT * FROM t3",
		"INSERT INTO t3 SELECT x+100, y FROM t1",
		"DROP TABLE t3",
		"CREATE TEMP TABLE t2(p INTEGER PRIMARY KEY AUTOINCREMENT, q)",
		"INSERT INTO t2 SELECT * FROM t1",
		"DROP TABLE t1",
		"DROP TABLE t2",
	}
	queries := []string{
		"SELECT 1, * FROM main.sqlite_sequence",
		"SELECT 2, * FROM temp.sqlite_sequence",
		"SELECT 3, * FROM sqlite_sequence",
		"SELECT 'main', * FROM main.sqlite_sequence UNION ALL SELECT 'temp', * FROM temp.sqlite_sequence ORDER BY 2",
		"SELECT name FROM temp.sqlite_sequence WHERE seq > 0",
		"SELECT count(*) FROM main.sqlite_sequence",
		// A compound whose LEFTMOST arm is the filtered source and whose
		// second arm's column DECLARES a non-BINARY collation. SQLite's rule
		// is that the leftmost arm governs the dedup, so 'T1' and 't1' must
		// stay two rows; an arm this engine cannot resolve is SKIPPED by
		// compoundArmCollation, which would silently hand the dedup the
		// SECOND arm's NOCASE and collapse them. See engine/join.go's
		// sqlite_sequence branch, which exists for exactly this.
		"SELECT name FROM main.sqlite_sequence UNION SELECT nc FROM tnc",
		"SELECT name FROM temp.sqlite_sequence UNION SELECT nc FROM tnc",
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Discard()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1) // the TEMP database is per-connection
	answered := 0
	for si, s := range steps {
		eerr := edb.Exec(s)
		_, cerr := cdb.Exec(s)
		if (eerr == nil) != (cerr == nil) {
			t.Fatalf("step %d %q: engine=%v cgo=%v", si, s, eerr, cerr)
		}
		p, perr := edb.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager after step %d: %v", si, perr)
		}
		for _, q := range queries {
			_, ev, eqerr := p.QueryArgs(q, nil)
			_, cr, cqerr := cgoSelect(t, cdb, q, nil)
			if eqerr != nil {
				continue // declined, exactly as the harness skips one
			}
			if cqerr != nil {
				t.Errorf("after step %d, [%s]: engine answered, cgo failed: %v", si, q, cqerr)
				continue
			}
			answered++
			er := engineRowsToStrings(ev)
			if fmt.Sprint(er) != fmt.Sprint(cr) {
				t.Errorf("after step %d %q, [%s]\n  engine: %v\n  cgo:    %v", si, s, q, er, cr)
			}
		}
		p.Close()
	}
	// A blind gate would pass by declining everything; require that the split
	// actually answered. 15 steps x 8 queries, minus the reads taken before
	// any TEMP AUTOINCREMENT table exists (where temp has no sqlite_sequence
	// on either side), leaves well over half.
	if answered < 70 {
		t.Errorf("only %d queries were answered -- the split is not being exercised", answered)
	}
}
