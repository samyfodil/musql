// This file tests subqueries inside aggregate queries, including IN subqueries
// and correlated references to the aggregate's anchor row.
//
// Both spellings are exercised. The UNQUALIFIED one ("WHERE k=c") used to
// decline: the substitution that makes a correlated reference visible to a
// separately-compiled subquery body (materializeOuterRefs)
// matches on the FROM-item NAME, so it rewrites qualified references only.
// compileSelectScanRow (engine/vdbe_scan.go) now also hands the live enclosing
// row to compileColumn as its documented last resort, AFTER the subquery's own
// fully-expanded scopes -- which is exactly where C SQLite's lookupName
// walks out to it -- so the two spellings read the same anchor row.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var aggSubSetup = []string{
	`CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT)`,
	`INSERT INTO t1 VALUES(1,2,'x'),(1,9,'y'),(1,4,'z'),(2,5,'p'),(2,7,'q')`,
	`CREATE TABLE t2(k TEXT, v INTEGER)`,
	`INSERT INTO t2 VALUES('x',10),('y',20),('z',30),('p',40),('q',50)`,
	`CREATE TABLE e1(a INTEGER, b INTEGER, c TEXT)`,
}

var aggSubQ = []string{
	// ---- (1) IN (SELECT ...) through the GROUP BY item rewrite ----
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a IN (SELECT 2)`,
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a NOT IN (SELECT 2)`,
	`SELECT a, count(*) FROM t1 GROUP BY a HAVING a IN (SELECT a FROM t1 WHERE b>6)`,
	`SELECT a, count(*), a IN (SELECT 2) FROM t1 GROUP BY a`,
	`SELECT a, count(*), a NOT IN (SELECT 2) FROM t1 GROUP BY a`,
	`SELECT count(*), 1 IN (SELECT 1) FROM t1`,
	`SELECT a, count(*) FROM t1 GROUP BY a ORDER BY a IN (SELECT 1), a`,

	// ---- (2) the anchor row a correlated select-list subquery reads ----
	`SELECT max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	// The UNQUALIFIED spelling of the same reference. It used to be in
	// aggSubDeclined: the subquery body is compiled standalone, and
	// materializeOuterRefs substitutes QUALIFIED outer
	// references only, so "c" resolved to nothing. compileSelectScanRow
	// (engine/vdbe_scan.go) now hands compileColumn the live enclosing row as
	// its last resort, which is where C SQLite's lookupName finds it too,
	// so this reads the SAME anchor row the qualified form above does -- 9, 20.
	`SELECT max(b), (SELECT v FROM t2 WHERE k=c) FROM t1`,
	`SELECT min(b), (SELECT v FROM t2 WHERE k=c) FROM t1`,
	`SELECT count(*), (SELECT v FROM t2 WHERE k=c) FROM t1`,
	`SELECT max(b), (SELECT v FROM t2 WHERE k=c) FROM t1 WHERE 0`,
	`SELECT min(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT count(*), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT sum(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT max(NULL), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT max(b) FILTER (WHERE c='zzz'), (SELECT v FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT max(b), (SELECT v FROM t2 WHERE k=t1.c)+1 FROM t1`,
	`SELECT max(b), EXISTS(SELECT 1 FROM t2 WHERE k=t1.c) FROM t1`,
	`SELECT max(b), (SELECT max(v) FROM t2 WHERE k=t1.c) FROM t1`,
	// No surviving row at all: the anchor is absent and every correlated
	// reference -- through a subquery or bare -- reads NULL, not an error.
	`SELECT max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 WHERE 0`,
	`SELECT count(*), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 WHERE 0`,
	`SELECT max(b), (SELECT v FROM t2 WHERE k=e1.c) FROM e1`,
	`SELECT max(b), c FROM t1 WHERE 0`,
	`SELECT count(*), c FROM t1 WHERE 0`,
	`SELECT max(b), c FROM e1`,

	// ---- GROUP BY: one anchor row per group ----
	`SELECT a, max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 GROUP BY a`,
	`SELECT a, min(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 GROUP BY a`,
	`SELECT a, count(*), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 GROUP BY a`,
	`SELECT a, max(b), (SELECT count(*) FROM t2) FROM t1 GROUP BY a`,
	`SELECT a, (SELECT count(*) FROM t2 WHERE v>t1.a*10) FROM t1 GROUP BY a`,
	`SELECT a, max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 GROUP BY a HAVING count(*)>1`,
	`SELECT a, max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 GROUP BY a ORDER BY 3 DESC`,
	`SELECT DISTINCT a, (SELECT count(*) FROM t2) FROM t1 GROUP BY a`,
	`SELECT a, max(b) FROM t1 GROUP BY a HAVING (SELECT v FROM t2 WHERE k=t1.c)>30`,

	// ---- uncorrelated, and inside an aggregate's own ARGUMENT ----
	`SELECT max(b), (SELECT count(*) FROM t2) FROM t1`,
	`SELECT max((SELECT v FROM t2 WHERE k=t1.c)) FROM t1`,
	`SELECT sum((SELECT v FROM t2 WHERE k=t1.c)) FROM t1`,
	`SELECT group_concat(c, (SELECT k FROM t2 WHERE v=10)) FROM t1`,
	`SELECT count((SELECT v FROM t2 WHERE k=t1.c)) FROM t1`,

	// ---- FROM-less aggregate, and a compound whose arm is one of the above ----
	`SELECT count(*), (SELECT 5)`,
	`SELECT max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 UNION ALL SELECT 1,2`,
	`SELECT max(b), (SELECT v FROM t2 WHERE k=t1.c) FROM t1 UNION SELECT 1,2`,

	// ---- join: the anchor row spans every joined table ----
	`SELECT max(t1.b), (SELECT v FROM t2 AS s WHERE s.k=t1.c) FROM t1, t2 WHERE t2.k=t1.c`,
}

func TestAggregateSelectSubqueryParity(t *testing.T) {
	dir := t.TempDir()
	edb, err := engine.Create(filepath.Join(dir, "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	for _, s := range aggSubSetup {
		if _, _, err := edb.ExecArgs(s, nil); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	for _, q := range aggSubQ {
		ecols, ev, eerr := p.QueryArgs(q, nil)
		ccols, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			// A shape this engine still declines is skipped exactly as the
			// corpus harness does -- an error is a clean decline, never a
			// wrong answer. It must never be a decline C SQLite accepts
			// AND this file claims to cover, so the list above holds only
			// shapes both engines run.
			t.Errorf("[%s] engine declined a shape this gate covers: %v", q, eerr)
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] cgo errored where the engine did not: %v", q, cerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch(ecols, eRows, ccols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}

// aggSubDeclined is the other half of the gate: shapes C SQLite answers
// that this engine must DECLINE (an error) rather than answer, because the
// answer it would produce is provably not SQLite's. Each is a documented,
// root-caused boundary of the select-list-subquery support above -- see
// checkItemSubqueries (engine/sql_group.go). Asserting the decline here is
// what stops one of them from silently becoming a wrong answer later.
var aggSubDeclined = []struct{ sql, why string }{
	// The two tc statements that used to sit here -- "max(c1 COLLATE nocase) IN
	// (SELECT 'aBCd')" and its group_concat spelling -- are gone: the
	// groupAggExpr placeholder CARRIES the call's explicit collation now
	// (sql_group.go), which is what sqlite3ExprCollSeq propagates, so both are
	// SERVED and gated cell-for-cell in compat-harness/r35d_resolution_test.go
	// (TestR35DAggCollate) alongside the "= 'aBCd'" and IN-LIST spellings that
	// never had a decline channel and simply answered 0.
	// The two t34 statements that used to sit here -- subquery.test's own
	// 3.4.1 and 3.4.2, where avg(a.y) inside the subquery is the OUTER group's
	// average -- are gone: they are SERVED now, and gated with their full
	// cell-for-cell output (plus a randomized fuzz over the same rule) in
	// compat-harness/vdbe_agg_hoist_grouped_test.go, which is where that whole
	// sub-shape lives.
}

func TestAggregateSelectSubqueryDeclines(t *testing.T) {
	dir := t.TempDir()
	edb, err := engine.Create(filepath.Join(dir, "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(dir, "c.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer cdb.Close()
	setup := append(append([]string(nil), aggSubSetup...),
		`CREATE TABLE tc(c1)`,
		`INSERT INTO tc VALUES('abcd')`,
		`CREATE TABLE t34(x,y)`,
		`INSERT INTO t34 VALUES(106,4),(107,3),(106,5),(107,5)`,
	)
	for _, s := range setup {
		if _, _, err := edb.ExecArgs(s, nil); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer p.Close()
	for _, tc := range aggSubDeclined {
		if _, _, err := p.QueryArgs(tc.sql, nil); err == nil {
			t.Errorf("[%s] now RUNS -- it used to decline (%s). Either it is genuinely correct now (verify against C SQLite and move it into aggSubQ) or it is a silent wrong answer.", tc.sql, tc.why)
		}
	}
}
