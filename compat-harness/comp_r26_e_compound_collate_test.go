// Tests COMPOUND SELECT collation with correlated references.
// collating sequence -- the one UNION dedup, INTERSECT, EXCEPT and the
// compound's own ORDER BY all compare under -- from the LEFTMOST arm whose
// i'th expression has an opinion, else the next arm rightward, else BINARY;
// and sqlite3ExprCollSeq, which supplies that opinion, descends CAST / unary
// "+" / COLLATE and reads a TK_COLUMN's DECLARED collation whatever scope the
// column resolved against. An enclosing query's scope is not special.
//
// This engine resolved each arm's schema from that arm's OWN FROM clause only,
// so a correlated reference resolved to nothing, the column silently fell back
// to BINARY, and four shapes this engine ALREADY SERVES came out wrong -- no
// error, just a different answer.
//
// Two properties of the fixture are load-bearing, and a fixture without them
// proves nothing:
//
//   - The strings must differ ONLY by case (or only by trailing space). Over
//     'abc'/'def', "(SELECT a EXCEPT SELECT 'zzz')" is 'abc' under BINARY and
//     NOCASE alike; only 'ABC' can tell the two apart. TestCompoundArmOuter
//     CollationValueBlind pins that -- it is the control that would pass on a
//     BROKEN engine, and it is here so nobody mistakes the main gate's fixture
//     for an arbitrary one.
//   - RTRIM is gated beside NOCASE. Without it, a fix that special-cased ASCII
//     case folding would pass.
package compat

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var r26eCollateSetup = []string{
	// the collated column FIRST
	`CREATE TABLE c1(a TEXT COLLATE NOCASE, b TEXT)`,
	`INSERT INTO c1 VALUES('abc','p')`,
	`INSERT INTO c1 VALUES('def','q')`,
	// the collated column SECOND, with a plain one on either side: a rule
	// derived over a table that declares the collated column first cannot see
	// a position mix-up.
	`CREATE TABLE c3(z TEXT, a TEXT COLLATE NOCASE, w TEXT)`,
	`INSERT INTO c3 VALUES('k','abc','m')`,
	`INSERT INTO c3 VALUES('k2','def','m2')`,
	// RTRIM: the difference is trailing blanks, not case
	`CREATE TABLE c2(a TEXT COLLATE RTRIM, b TEXT)`,
	`INSERT INTO c2 VALUES('xy','p')`,
	// a PLAIN column of the SAME NAME, so an arm's own scope can be shown to
	// shadow the enclosing one
	`CREATE TABLE pl(a TEXT, b TEXT)`,
	`INSERT INTO pl VALUES('abc','r')`,
	`INSERT INTO pl VALUES('ABC','s')`,
	// an EMPTY table, for the arm that produces no rows at all
	`CREATE TABLE em(a TEXT COLLATE NOCASE)`,
}

// r26eCollateCorpus is (statement, order-sensitive). Every one must run in
// BOTH engines -- these are shapes this engine serves, not declines -- and
// agree cell for cell.
var r26eCollateCorpus = []subCase{
	// --- the recorded wrong answers ---
	{`SELECT a,(SELECT a EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a FROM c1 WHERE EXISTS (SELECT a INTERSECT SELECT 'ABC') ORDER BY a`, true},
	{`SELECT a,(SELECT a INTERSECT SELECT 'xy  ') FROM c2 ORDER BY a`, true},

	// --- WHICH arm owns the opinion ---
	// leftmost arm is the correlated column: it settles the column.
	{`SELECT a,(SELECT a UNION SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT count(*) FROM (SELECT a UNION SELECT 'ABC')) FROM c1 ORDER BY a`, true},
	// leftmost arm is a LITERAL, which has no opinion, so the search moves
	// right to the correlated column -- the opposite direction, and the case a
	// "just use arm 0" rule gets wrong.
	{`SELECT a,(SELECT 'ABC' EXCEPT SELECT a) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT 'ABC' INTERSECT SELECT a) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT count(*) FROM (SELECT 'ABC' UNION SELECT a)) FROM c1 ORDER BY a`, true},
	// three arms
	{`SELECT a,(SELECT a UNION SELECT 'ABC' UNION SELECT 'Abc') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT count(*) FROM (SELECT a UNION SELECT 'ABC' UNION SELECT 'Abc')) FROM c1 ORDER BY a`, true},

	// --- what sqlite3ExprCollSeq descends, and what it does NOT ---
	{`SELECT a,(SELECT +a EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT CAST(a AS TEXT) EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT a COLLATE BINARY EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT a EXCEPT SELECT 'ABC' COLLATE BINARY) FROM c1 ORDER BY a`, true},
	// "a||''" is a concatenation: a bare column's DECLARED collation does not
	// propagate through one, so this arm has NO opinion and the next arm's
	// (also none, a literal) leaves the column BINARY.
	{`SELECT a,(SELECT a||'' EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},

	// --- how the correlated reference is spelled ---
	{`SELECT a,(SELECT c1.a EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT x.a,(SELECT x.a EXCEPT SELECT 'ABC') FROM c1 AS x ORDER BY x.a`, true},
	{`SELECT x.a,(SELECT a EXCEPT SELECT 'ABC') FROM c1 AS x ORDER BY x.a`, true},

	// --- the collated column is not the table's first ---
	{`SELECT a,(SELECT a EXCEPT SELECT 'ABC') FROM c3 ORDER BY a`, true},
	{`SELECT z,(SELECT z EXCEPT SELECT 'K') FROM c3 ORDER BY z`, true},

	// --- the arm's OWN scope shadows the enclosing one ---
	// pl.a is plain, so these must compare BINARY even though the enclosing
	// c1.a is NOCASE. This is the direction a blanket "look outward" gets
	// wrong.
	{`SELECT a,(SELECT a FROM pl EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT count(*) FROM (SELECT a FROM pl EXCEPT SELECT 'ABC')) FROM c1 ORDER BY a`, true},

	// --- two frames out ---
	{`SELECT a,(SELECT (SELECT a EXCEPT SELECT 'ABC')) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT (SELECT count(*) FROM (SELECT a EXCEPT SELECT 'ABC'))) FROM c1 ORDER BY a`, true},

	// --- column COUNT and ORDER inside the compound ---
	{`SELECT a,(SELECT count(*) FROM (SELECT b,a FROM c1 EXCEPT SELECT 'p','ABC')) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT count(*) FROM (SELECT a,b FROM c1 EXCEPT SELECT 'ABC','p')) FROM c1 ORDER BY a`, true},

	// --- an empty arm on either side ---
	{`SELECT a,(SELECT a EXCEPT SELECT a FROM em) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT a FROM em EXCEPT SELECT 'ABC') FROM c1 ORDER BY a`, true},

	// --- the compound's OWN ORDER BY sorts under the same collation ---
	{`SELECT a,(SELECT a UNION SELECT 'ABC' ORDER BY 1) FROM c1 ORDER BY a`, true},
	{`SELECT a,(SELECT group_concat(q) FROM (SELECT a AS q UNION ALL SELECT 'ABC' ORDER BY 1)) FROM c1 ORDER BY a`, true},

	// --- NOT EXISTS, and IN, the other correlated opcodes ---
	{`SELECT a FROM c1 WHERE NOT EXISTS (SELECT a INTERSECT SELECT 'ABC') ORDER BY a`, true},
	{`SELECT a FROM c1 WHERE 'ABC' IN (SELECT a UNION SELECT 'zz') ORDER BY a`, true},
}

func buildR26eCollateDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/r26e_collate.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range r26eCollateSetup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestCompoundArmOuterCollation(t *testing.T) {
	path := buildR26eCollateDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong := 0
	for _, tc := range r26eCollateCorpus {
		vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			wrong++
			t.Errorf("[%s] fixture is wrong: C SQLite rejected it (%v)", tc.sql, cErr)
			continue
		}
		if vErr != nil {
			wrong++
			t.Errorf("[%s] engine DECLINED a shape it used to serve: %v", tc.sql, vErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] DIVERGES from C SQLite: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
				tc.sql, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("compound-arm outer collation gate: %d statements, wrong=%d", len(r26eCollateCorpus), wrong)
	if wrong != 0 {
		t.Fatalf("compound-arm outer collation gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestCompoundArmOuterCollationValueBlind is the CONTROL: the identical
// statements over values that do not differ only by case agree under BINARY
// and under NOCASE alike, so they pass on a BROKEN engine too. They are here
// to document that the main gate's fixture is not arbitrary -- swap 'ABC' for
// 'zzz' and the whole gate stops testing anything.
func TestCompoundArmOuterCollationValueBlind(t *testing.T) {
	path := buildR26eCollateDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT a,(SELECT a EXCEPT SELECT 'zzz') FROM c1 ORDER BY a`,
		`SELECT a FROM c1 WHERE EXISTS (SELECT a INTERSECT SELECT 'abc') ORDER BY a`,
		`SELECT a,(SELECT a INTERSECT SELECT 'xy') FROM c2 ORDER BY a`,
	} {
		vCols, vVals, vErr := p.QueryArgs(q, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, q, nil)
		if vErr != nil || cErr != nil {
			t.Fatalf("[%s] both engines must answer: engine=%v cgo=%v", q, vErr, cErr)
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] DIVERGES: %s", q, reason)
		}
	}
}

// TestCompoundArmOuterCollationNoLateral keeps the outward walk from becoming
// "resolve anywhere": a compound arm sees an ENCLOSING query's columns, never
// its OWN query's sibling FROM items (SQL has no LATERAL). Both engines must
// reject; only the wording differs.
func TestCompoundArmOuterCollationNoLateral(t *testing.T) {
	path := buildR26eCollateDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, q := range []string{
		`SELECT a FROM c1, (SELECT c1.a EXCEPT SELECT 'ABC')`,
		`SELECT a FROM c1, (SELECT a FROM pl UNION SELECT c1.a)`,
	} {
		_, _, vErr := p.QueryArgs(q, nil)
		_, _, cErr := cgoSelect(t, cdb, q, nil)
		if cErr == nil {
			t.Fatalf("[%s] fixture is wrong: C SQLite ACCEPTED it", q)
		}
		if vErr == nil {
			t.Errorf("[%s] engine ACCEPTED a statement C SQLite rejects (%v)", q, cErr)
			continue
		}
		if !strings.Contains(vErr.Error(), "no such column") && !strings.Contains(vErr.Error(), "no such table") {
			t.Errorf("[%s] rejected, but not as a name-resolution failure: %v", q, vErr)
		}
	}
}
