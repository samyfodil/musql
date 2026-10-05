package compat

// Correlated-reference substitution width testing: affinity, collation, and channel (column reference, aggregate, ordinal)
// variations. Verifies outer values carry column affinity and declared collations through subquery resolution.

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// ovSchema: o6/o2 hold the same row through different declarations and orders; inn/inn2 are inner tables; cc/cd carry collations.
var ovSchema = []string{
	"CREATE TABLE o6(i INTEGER, t TEXT, n, r REAL, m NUMERIC, b BLOB)",
	"INSERT INTO o6 VALUES(1,'1.0',1,1.0,1,'1')",
	"CREATE TABLE o2(t TEXT, i INTEGER)",
	"INSERT INTO o2 VALUES('1.0',1)",
	"CREATE TABLE inn(t TEXT, i INTEGER, n)",
	"INSERT INTO inn VALUES('1.0',1,'1.0')",
	"CREATE TABLE inn2(n, i INTEGER, t TEXT)",
	"INSERT INTO inn2 VALUES('1.0',1,'1.0')",
	"CREATE TABLE ie(t TEXT, i INTEGER)",
	"CREATE TABLE cc(a TEXT COLLATE NOCASE, b TEXT, c TEXT COLLATE RTRIM)",
	"INSERT INTO cc VALUES('abc','ABC','xy ')",
	"CREATE TABLE cd(p TEXT, q TEXT COLLATE NOCASE)",
	"INSERT INTO cd VALUES('ABC','ABC')",
	"CREATE TABLE agg(x INTEGER, y INTEGER)",
	"INSERT INTO agg VALUES(1,5),(2,6)",
	"CREATE TABLE ord(a INTEGER, k INTEGER)",
	"INSERT INTO ord VALUES(1,1)",
	"CREATE TABLE two(x TEXT, y TEXT)",
	"INSERT INTO two VALUES('b','1'),('a','2')",
}

var ovReadCases = []struct{ name, sql string }{
	// AFFINITY: outer column vs literal of different storage class.
	{"aff-int-vs-textlit", "SELECT i, (SELECT o6.i='1.0') FROM o6 GROUP BY i"},
	{"aff-int-vs-textlit-rowmode", "SELECT i, (SELECT o6.i='1.0') FROM o6"},
	{"aff-text-vs-intlit", "SELECT t, (SELECT o6.t=1) FROM o6 GROUP BY t"},
	{"aff-text-vs-intlit-rowmode", "SELECT t, (SELECT o6.t=1) FROM o6"},
	{"aff-real-vs-textlit", "SELECT r, (SELECT o6.r='1.0') FROM o6 GROUP BY r"},
	{"aff-num-vs-textlit", "SELECT m, (SELECT o6.m='1.0') FROM o6 GROUP BY m"},
	{"aff-blob-vs-intlit", "SELECT b, (SELECT o6.b=1) FROM o6 GROUP BY b"},
	{"aff-none-vs-textlit", "SELECT n, (SELECT o6.n='1.0') FROM o6 GROUP BY n"},
	// Reversed operand order: the literal on the LEFT.
	{"aff-textlit-vs-int", "SELECT i, (SELECT '1.0'=o6.i) FROM o6 GROUP BY i"},
	{"aff-intlit-vs-text", "SELECT t, (SELECT 1=o6.t) FROM o6 GROUP BY t"},
	// AFFINITY across tables with reversed declaration order.
	{"aff-order-int", "SELECT i, (SELECT o2.i='1.0') FROM o2 GROUP BY i"},
	{"aff-order-text", "SELECT t, (SELECT o2.t=1) FROM o2 GROUP BY t"},
	// OUTER column vs INNER COLUMN: both sides declare affinity.
	{"aff-outint-vs-inntext", "SELECT i, (SELECT count(*) FROM inn WHERE inn.t=o6.i) FROM o6 GROUP BY i"},
	{"aff-outtext-vs-innint", "SELECT t, (SELECT count(*) FROM inn WHERE inn.i=o6.t) FROM o6 GROUP BY t"},
	{"aff-outnone-vs-inntext", "SELECT n, (SELECT count(*) FROM inn WHERE inn.t=o6.n) FROM o6 GROUP BY n"},
	{"aff-outtext-vs-innnone", "SELECT t, (SELECT count(*) FROM inn WHERE inn.n=o6.t) FROM o6 GROUP BY t"},
	{"aff-outint-vs-inn2text", "SELECT i, (SELECT count(*) FROM inn2 WHERE inn2.t=o6.i) FROM o6 GROUP BY i"},
	{"aff-outnone-vs-inn2none", "SELECT n, (SELECT count(*) FROM inn2 WHERE inn2.n=o6.n) FROM o6 GROUP BY n"},
	// EMPTY inner table: zero rows must stay zero rows.
	{"aff-empty-inner-exists", "SELECT i, (SELECT count(*) FROM ie WHERE ie.t=o6.i) FROM o6 GROUP BY i"},
	{"aff-empty-inner-scalar", "SELECT i, (SELECT ie.i FROM ie WHERE ie.t=o6.i) FROM o6 GROUP BY i"},
	// ROWID: has affinity (INTEGER) but no declared collation.
	{"rowid-vs-textlit", "SELECT i, (SELECT o6.rowid='1.0') FROM o6 GROUP BY i"},
	{"rowid-vs-inntext", "SELECT i, (SELECT count(*) FROM inn WHERE inn.t=o6.rowid) FROM o6 GROUP BY i"},
	// COLLATION: outer column declares NOCASE or RTRIM and must carry it.
	{"coll-nocase-vs-lit", "SELECT a, (SELECT cc.a='ABC') FROM cc GROUP BY a"},
	{"coll-lit-vs-nocase", "SELECT a, (SELECT 'ABC'=cc.a) FROM cc GROUP BY a"},
	{"coll-binary-vs-lit", "SELECT b, (SELECT cc.b='abc') FROM cc GROUP BY b"},
	{"coll-rtrim-vs-lit", "SELECT c, (SELECT cc.c='xy') FROM cc GROUP BY c"},
	// Outer BINARY vs inner NOCASE: left operand's BINARY wins.
	{"coll-outbinary-vs-innnocase", "SELECT b, (SELECT count(*) FROM cd WHERE cc.b=cd.q) FROM cc GROUP BY b"},
	{"coll-innnocase-vs-outbinary", "SELECT b, (SELECT count(*) FROM cd WHERE cd.q=cc.b) FROM cc GROUP BY b"},
	{"coll-outnocase-vs-innbinary", "SELECT a, (SELECT count(*) FROM cd WHERE cc.a=cd.p) FROM cc GROUP BY a"},
	// Explicit collation on the other operand beats the declared one.
	{"coll-explicit-rhs-beats-declared", "SELECT a, (SELECT cc.a='ABC ' COLLATE RTRIM) FROM cc GROUP BY a"},
	{"coll-explicit-lhs-beats-declared", "SELECT a, (SELECT 'ABC ' COLLATE RTRIM = cc.a) FROM cc GROUP BY a"},
	// Explicit collation on the reference survives the substitution.
	{"coll-explicit-on-ref", "SELECT a, (SELECT cc.a COLLATE BINARY = 'ABC') FROM cc GROUP BY a"},
	// Collation reaches ORDER BY/DISTINCT inside the body.
	{"coll-in-body-orderby", "SELECT a, (SELECT cd.p FROM cd WHERE cd.p=cc.a ORDER BY cd.p LIMIT 1) FROM cc GROUP BY a"},
	{"coll-in-body-in", "SELECT a, (SELECT cc.a IN (SELECT q FROM cd)) FROM cc GROUP BY a"},
	// AGGREGATE ASSOCIATION: outer-only vs local-only.
	{"assoc-outer-only", "SELECT x, (SELECT sum(agg.x)) FROM agg GROUP BY x ORDER BY x"},
	{"assoc-outer-only-nogroup", "SELECT (SELECT sum(agg.x)) FROM agg"},
	{"assoc-mixed-local", "SELECT x, (SELECT sum(agg.x+inn.i) FROM inn) FROM agg GROUP BY x ORDER BY x"},
	{"assoc-local-only", "SELECT x, (SELECT sum(inn.i) FROM inn WHERE inn.i=agg.x) FROM agg GROUP BY x ORDER BY x"},
	{"assoc-noargs", "SELECT x, (SELECT count(*) FROM inn WHERE inn.i=agg.x) FROM agg GROUP BY x ORDER BY x"},
	// ORDINAL collision: bare INTEGER in ORDER BY/GROUP BY is a column position reference.
	{"ord-orderby", "SELECT k, (SELECT x FROM two ORDER BY ord.a LIMIT 1) FROM ord GROUP BY k"},
	{"ord-orderby-desc", "SELECT k, (SELECT x FROM two ORDER BY ord.a DESC LIMIT 1) FROM ord GROUP BY k"},
	{"ord-groupby", "SELECT k, (SELECT x FROM two GROUP BY ord.a) FROM ord GROUP BY k"},
	// Not a bare term, so never an ordinal.
	{"ord-not-bare", "SELECT k, (SELECT x FROM two ORDER BY ord.a+0 LIMIT 1) FROM ord GROUP BY k"},
	// A genuine ordinal written in the SQL is untouched.
	{"ord-real-ordinal", "SELECT k, (SELECT x FROM two WHERE two.y>ord.a ORDER BY 1 LIMIT 1) FROM ord GROUP BY k"},
}

func TestR27OuterValueKeepsColumnProperties(t *testing.T) {
	for _, tc := range ovReadCases {
		t.Run(tc.name, func(t *testing.T) {
			differ(t, "r27-outerval-"+tc.name, append(append([]string(nil), ovSchema...), tc.sql))
		})
	}
}

// ovWriteCases: each is run exactly once per engine, scored on side effects.
var ovWriteCases = []struct{ name, stmt, read string }{
	// AFFINITY write side: outer column loses its affinity, silently skips row.
	{"w-aff-exists-int-text", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o6.i)", "SELECT i,t FROM o6"},
	{"w-aff-exists-text-int", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.i=o6.t)", "SELECT i,t FROM o6"},
	{"w-aff-exists-none-text", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o6.n)", "SELECT i,t FROM o6"},
	{"w-aff-exists-real-text", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o6.r)", "SELECT i,t FROM o6"},
	{"w-aff-exists-reversed-cols", "UPDATE o2 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o2.i)", "SELECT i,t FROM o2"},
	{"w-aff-scalar-lit", "UPDATE o6 SET t='HIT' WHERE (SELECT o6.i='1.0')", "SELECT i,t FROM o6"},
	{"w-aff-set-subquery", "UPDATE o6 SET t=(SELECT group_concat(inn.t) FROM inn WHERE inn.t=o6.i)", "SELECT i,t FROM o6"},
	{"w-aff-delete", "DELETE FROM o6 WHERE (SELECT o6.i='1.0')", "SELECT i,t FROM o6"},
	{"w-aff-empty-inner", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM ie WHERE ie.t=o6.i)", "SELECT i,t FROM o6"},
	{"w-aff-rowid", "UPDATE o6 SET t='HIT' WHERE EXISTS (SELECT 1 FROM inn WHERE inn.t=o6.rowid)", "SELECT i,t FROM o6"},
	{"w-aff-in-subquery", "UPDATE o6 SET t='HIT' WHERE (SELECT o6.i IN (SELECT t FROM inn))", "SELECT i,t FROM o6"},
	// COLLATION write side.
	{"w-coll-nocase", "UPDATE cc SET b='HIT' WHERE (SELECT cc.a='ABC')", "SELECT a,b FROM cc"},
	{"w-coll-binary", "UPDATE cc SET b='HIT' WHERE (SELECT cc.b='abc')", "SELECT a,b FROM cc"},
	{"w-coll-rtrim", "UPDATE cc SET b='HIT' WHERE (SELECT cc.c='xy')", "SELECT a,b FROM cc"},
	{"w-coll-explicit-beats-declared", "UPDATE cc SET b='HIT' WHERE (SELECT cc.a='ABC ' COLLATE RTRIM)", "SELECT a,b FROM cc"},
	{"w-coll-explicit-lhs", "UPDATE cc SET b='HIT' WHERE (SELECT 'ABC ' COLLATE RTRIM = cc.a)", "SELECT a,b FROM cc"},
	{"w-coll-cross-table", "UPDATE cc SET b='HIT' WHERE EXISTS (SELECT 1 FROM cd WHERE cd.p=cc.a)", "SELECT a,b FROM cc"},
	{"w-coll-cross-table-rev", "UPDATE cc SET b='HIT' WHERE EXISTS (SELECT 1 FROM cd WHERE cc.a=cd.p)", "SELECT a,b FROM cc"},
	{"w-coll-delete", "DELETE FROM cc WHERE (SELECT cc.a='ABC')", "SELECT a,b FROM cc"},
	// AGGREGATE ASSOCIATION write side: some are "misuse of aggregate" rejections.
	{"w-assoc-update-sum", "UPDATE agg SET y=(SELECT sum(agg.x))", "SELECT x,y FROM agg"},
	{"w-assoc-update-count", "UPDATE agg SET y=(SELECT count(agg.x))", "SELECT x,y FROM agg"},
	{"w-assoc-delete", "DELETE FROM agg WHERE (SELECT sum(agg.x))>0", "SELECT x,y FROM agg"},
	{"w-assoc-where-max", "UPDATE agg SET y=9 WHERE (SELECT max(agg.x))=2", "SELECT x,y FROM agg"},
	{"w-assoc-local-agg", "UPDATE agg SET y=(SELECT sum(inn.i) FROM inn WHERE inn.i=agg.x)", "SELECT x,y FROM agg"},
	{"w-assoc-mixed-agg", "UPDATE agg SET y=(SELECT sum(agg.x+inn.i) FROM inn)", "SELECT x,y FROM agg"},
	{"w-assoc-noarg-agg", "UPDATE agg SET y=(SELECT count(*) FROM inn WHERE inn.i=agg.x)", "SELECT x,y FROM agg"},
	// ORDINAL collision in write: substituted "ORDER BY 1" picks the wrong row.
	{"w-ord-orderby", "UPDATE ord SET k=(SELECT x FROM two ORDER BY ord.a LIMIT 1)", "SELECT a,k FROM ord"},
	{"w-ord-groupby", "UPDATE ord SET k=(SELECT x FROM two GROUP BY ord.a)", "SELECT a,k FROM ord"},
}

// ovPseudoRowCases: trigger NEW/OLD and UPSERT excluded rows through differ().
var ovPseudoRowCases = []struct {
	name  string
	stmts []string
}{
	{"trigger-new-agg", []string{
		"CREATE TABLE src(a INTEGER, b TEXT)",
		"CREATE TABLE tlog(v)",
		"CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO tlog VALUES((SELECT sum(new.a))); END",
		"INSERT INTO src VALUES(3,'x')",
		"SELECT v FROM tlog"}},
	{"trigger-old-agg", []string{
		"CREATE TABLE d(a INTEGER, b INTEGER)",
		"INSERT INTO d VALUES(1,1),(2,2)",
		"CREATE TABLE dlog(v)",
		"CREATE TRIGGER dtr AFTER DELETE ON d BEGIN INSERT INTO dlog VALUES((SELECT count(old.a))); END",
		"DELETE FROM d WHERE a=1",
		"SELECT v FROM dlog"}},
	{"trigger-new-compare", []string{
		"CREATE TABLE s2(a TEXT COLLATE NOCASE)",
		"CREATE TABLE tlog2(v)",
		"CREATE TRIGGER tr2 AFTER INSERT ON s2 BEGIN INSERT INTO tlog2 VALUES((SELECT new.a='ABC')); END",
		"INSERT INTO s2 VALUES('abc')",
		"SELECT v FROM tlog2"}},
	{"upsert-excluded-agg", []string{
		"CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)",
		"INSERT INTO u VALUES(1,10)",
		"INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT sum(excluded.v))",
		"SELECT k,v FROM u"}},
	{"upsert-excluded-plain", []string{
		"CREATE TABLE u2(k INTEGER PRIMARY KEY, v INTEGER)",
		"INSERT INTO u2 VALUES(1,10)",
		"INSERT INTO u2 VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)",
		"SELECT k,v FROM u2"}},
}

func TestR27OuterValuePseudoRows(t *testing.T) {
	for _, tc := range ovPseudoRowCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "r27-pseudorow-"+tc.name, tc.stmts) })
	}
}

// ovRowOuterCases: unqualified outer references reach a second substitution path (compiler.rowOuter).
var ovRowOuterCases = []struct {
	name  string
	stmts []string
}{
	{"unqualified-collation", []string{
		"CREATE TABLE c1(a TEXT COLLATE NOCASE, b TEXT)",
		"INSERT INTO c1 VALUES('abc','x')",
		"UPDATE c1 SET b='HIT' WHERE (SELECT a='ABC')",
		"SELECT a,b FROM c1"}},
	{"unqualified-affinity", []string{
		"CREATE TABLE uu(i INTEGER, mark TEXT)",
		"INSERT INTO uu VALUES(1,'x')",
		"CREATE TABLE tt(t TEXT)",
		"INSERT INTO tt VALUES('1.0')",
		"UPDATE uu SET mark='HIT' WHERE EXISTS (SELECT 1 FROM tt WHERE tt.t=i)",
		"SELECT i,mark FROM uu"}},
	{"unqualified-left-operand-control", []string{
		"CREATE TABLE q1(a TEXT COLLATE NOCASE, b TEXT)",
		"INSERT INTO q1 VALUES('abc','x')",
		"CREATE TABLE q2(z TEXT)",
		"INSERT INTO q2 VALUES('ABC')",
		"UPDATE q1 SET b='HIT' WHERE EXISTS (SELECT 1 FROM q2 WHERE z=a)",
		"SELECT a,b FROM q1"}},
	{"unqualified-right-operand", []string{
		"CREATE TABLE p1(a TEXT COLLATE NOCASE, b TEXT)",
		"INSERT INTO p1 VALUES('abc','x')",
		"CREATE TABLE p2(z TEXT)",
		"INSERT INTO p2 VALUES('ABC')",
		"UPDATE p1 SET b='HIT' WHERE EXISTS (SELECT 1 FROM p2 WHERE a=z)",
		"SELECT a,b FROM p1"}},
	{"unqualified-delete", []string{
		"CREATE TABLE k1(a TEXT COLLATE NOCASE, b TEXT)",
		"INSERT INTO k1 VALUES('abc','x'),('def','y')",
		"DELETE FROM k1 WHERE (SELECT a='ABC')",
		"SELECT a,b FROM k1"}},
	{"unqualified-set", []string{
		"CREATE TABLE m1(i INTEGER, mark TEXT)",
		"INSERT INTO m1 VALUES(1,'x')",
		"CREATE TABLE m2(t TEXT)",
		"INSERT INTO m2 VALUES('1.0')",
		"UPDATE m1 SET mark=(SELECT group_concat(t) FROM m2 WHERE m2.t=i)",
		"SELECT i,mark FROM m1"}},
}

func TestR27OuterValueRowOuterRoute(t *testing.T) {
	for _, tc := range ovRowOuterCases {
		t.Run(tc.name, func(t *testing.T) { differ(t, "r27-rowouter-"+tc.name, tc.stmts) })
	}
}

func TestR27OuterValueWrites(t *testing.T) {
	for _, tc := range ovWriteCases {
		t.Run(tc.name, func(t *testing.T) {
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
			cdb.SetMaxOpenConns(1)
			for _, s := range ovSchema {
				if e := edb.Exec(s); e != nil {
					t.Fatalf("engine setup %s: %v", s, e)
				}
				if _, e := cdb.Exec(s); e != nil {
					t.Fatalf("cgo setup %s: %v", s, e)
				}
			}

			// ONE execution per engine. The oracle's error, if any, is part of
			// the premise: a statement it rejects must leave the rows alone,
			// which is exactly what the read-back below then checks.
			_, cerr := cdb.Exec(tc.stmt)
			eerr := edb.Exec(tc.stmt)

			cRows, qerr := cdb.Query(tc.read)
			if qerr != nil {
				t.Fatalf("cgo read-back %q: %v", tc.read, qerr)
			}
			cCols, _ := cRows.Columns()
			var want [][]string
			for cRows.Next() {
				cells := make([]any, len(cCols))
				ptrs := make([]any, len(cCols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				if e := cRows.Scan(ptrs...); e != nil {
					t.Fatal(e)
				}
				norm := make([]string, len(cCols))
				for i, c := range cells {
					norm[i] = tclNormalizeCGOCell(c)
				}
				want = append(want, norm)
			}
			cRows.Close()

			if eerr != nil {
				// A DECLINE is always acceptable (never wrong), but it must not
				// have left anything behind: the rows are compared either way.
				t.Logf("musql declined %q: %v", tc.stmt, eerr)
			}
			gCols, got, gerr, panicked, pv := tclSafeGoQuery(edb, tc.read)
			if panicked {
				t.Fatalf("engine PANICKED reading back %q: %v", tc.stmt, pv)
			}
			if gerr != nil {
				t.Fatalf("engine read-back failed for %q: %v", tc.stmt, gerr)
			}
			if ok, reason := queryResultsMatch(gCols, got, cCols, want, false); !ok {
				t.Fatalf("SIDE EFFECT DIVERGES for %q (cgo err=%v, musql err=%v): %s\n  engine: %v\n  cgo:    %v",
					tc.stmt, cerr, eerr, reason, got, want)
			}
		})
	}
}
