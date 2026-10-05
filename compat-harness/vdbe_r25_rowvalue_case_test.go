package compat

// Row values in CASE expressions: probing multiple times with side effects.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// r25CaseSchema is rowvalue8.test's own schema, plus a collating/affinity pair
// so the desugared comparison is exercised where per-position collation and
// affinity actually decide the answer, and an empty table so a base subquery
// matching NO row is covered (C SQLite reads the absent row as NULLs, which
// never matches a label, so it falls to ELSE).
var r25CaseSchema = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY,b,c,d)`,
	`INSERT INTO t1(a,b,c,d) VALUES(1,1,2,3),(2,2,3,4),(3,1,2,4),(4,2,3,5),(5,3,4,6),(6,4,5,9)`,
	`CREATE TABLE t2(x INTEGER PRIMARY KEY, y)`,
	`INSERT INTO t2(x,y) VALUES(1,6),(2,5),(3,4),(4,3),(5,2),(6,1),(7,99)`,
	`CREATE TABLE hh(a TEXT COLLATE nocase, b INT, c)`,
	`INSERT INTO hh VALUES('ABC', 1, 'x'), ('abc', 2, 'y'), ('def', 1, 'z')`,
	`CREATE TABLE nn(p, q)`,
	`INSERT INTO nn VALUES(1,2),(NULL,2),(1,NULL),(NULL,NULL)`,
}

// r25CaseQ must every one of them AGREE with C SQLite, cell for cell. A
// decline here is a failure, not a pass: this is the shape rowvalue8.test
// covers and it is implemented.
var r25CaseQ = []string{
	// rowvalue8.test 1.1 / 1.2 verbatim: a 2- and a 3-wide row value.
	`SELECT a, CASE (b,c) WHEN (1,2) THEN 'aleph' WHEN (2,3) THEN 'bet' WHEN (3,4) THEN 'gimel' ELSE '-' END, '|' FROM t1 ORDER BY a`,
	`SELECT a, CASE (b,c,d) WHEN (1,2,3) THEN 'aleph' WHEN (2,3,4) THEN 'bet' WHEN (3,4,6) THEN 'gimel' ELSE '-' END, '|' FROM t1 ORDER BY a`,
	// rowvalue8.test 2.1, extended with y=99 so one row's base subquery
	// matches nothing at all.
	`SELECT x, CASE (SELECT b,c FROM t1 WHERE a=y) WHEN (1,2) THEN 'aleph' WHEN (2,3) THEN 'bet' WHEN (3,4) THEN 'gimel' ELSE '-' END, '|' FROM t2 ORDER BY +x`,
	// The label may be the subquery instead of the base.
	`SELECT a, CASE (b,c) WHEN (SELECT 1,2) THEN 'a' ELSE 'z' END FROM t1 ORDER BY a`,
	// NULL elements: the base-form CASE compares with "=", NOT the NULL-safe
	// "IS", so a NULL element never matches -- not even against a NULL label.
	`SELECT p, q, CASE (p,q) WHEN (1,2) THEN 'a' WHEN (NULL,2) THEN 'b' ELSE 'z' END FROM nn ORDER BY rowid`,
	`SELECT CASE (NULL,2) WHEN (NULL,2) THEN 'a' ELSE 'z' END`,
	`SELECT CASE (NULL,2) WHEN (1,2) THEN 'a' END`,
	// No ELSE: an unmatched base yields NULL.
	`SELECT a, CASE (b,c) WHEN (9,9) THEN 'a' END FROM t1 ORDER BY a`,
	// Per-position collation and affinity, which the desugared comparison
	// inherits from the ordinary scalar comparison at each position.
	`SELECT c, CASE (a,b) WHEN ('abc',1) THEN 'hit' ELSE 'miss' END FROM hh ORDER BY c`,
	`SELECT c, CASE (a COLLATE binary,b) WHEN ('abc',1) THEN 'hit' ELSE 'miss' END FROM hh ORDER BY c`,
	// Outside a select list: a WHERE predicate, a GROUP BY query's item, and
	// a nested CASE. The rewrite is in the parser, so every path gets it --
	// this asserts that rather than assuming it.
	`SELECT a FROM t1 WHERE CASE (b,c) WHEN (1,2) THEN 1 ELSE 0 END ORDER BY a`,
	`SELECT b, count(*), CASE (b,min(c)) WHEN (1,2) THEN 'a' ELSE 'z' END FROM t1 GROUP BY b ORDER BY b`,
	`SELECT a, CASE (b,c) WHEN (1,2) THEN CASE (c,d) WHEN (2,3) THEN 'in' ELSE 'out' END ELSE 'z' END FROM t1 ORDER BY a`,
	// A single label writes the base exactly once, so a nondeterministic base
	// is still answerable there -- and this spelling is deterministic enough
	// to compare: abs(random())%1 is always 0.
	`SELECT CASE (abs(random())%1, 1) WHEN (0,1) THEN 'a' ELSE 'z' END`,
}

func TestR25RowValueCaseParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range r25CaseSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r25CaseQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil {
			t.Errorf("[%s] engine declined a row-value CASE it implements: %v", q, eerr)
			continue
		}
		if cerr != nil {
			t.Errorf("[%s] engine accepted what C SQLite rejects: %v", q, cerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		cols := make([]string, len(cc))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		if ok, reason := queryResultsMatch(cols, eRows, cols, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}

// r25DupProbeQ are the shapes whose parse-time rewrite would DUPLICATE a
// nondeterministic probe operand. C SQLite evaluates it once, so each of
// these has an answer this engine cannot reproduce -- it must error, never
// guess. The comment on each line is what C SQLite answers (verified
// against mattn/go-sqlite3 3.53.3).
var r25DupProbeQ = []string{
	// The IN-list rewrite writes the left row once per list element. This was
	// a WRONG ANSWER: the drawn value is 0 or 1 and both are listed, so real
	// SQLite is 1 every time, while a fresh draw per element answered 0 in 2
	// of 8 runs.
	`SELECT (abs(random())%2, 1) IN ((0,1),(1,1))`,     // sqlite: always 1
	`SELECT (abs(random())%2, 1) NOT IN ((0,1),(1,1))`, // sqlite: always 0
	`SELECT (1, randomblob(4)) IN ((1,2),(3,4))`,       // sqlite: always 0
	// The base-form CASE rewrite writes the base once per label.
	`SELECT CASE (abs(random())%2, 1) WHEN (0,1) THEN 'a' WHEN (1,1) THEN 'b' ELSE 'z' END`, // sqlite: never 'z'
	`SELECT CASE (SELECT abs(random())%2, 1) WHEN (0,1) THEN 'a' WHEN (1,1) THEN 'b' ELSE 'z' END`,
}

// r25DupProbeTableQ repeats them against a real table, so the decline is
// exercised on the ordinary table-scan compile path too and not only on the
// FROM-less one, which has its own earlier gate.
var r25DupProbeTableQ = []string{
	`SELECT p FROM nn WHERE (abs(random())%2, p) IN ((0,1),(1,1))`,
	`SELECT CASE (abs(random())%2, p) WHEN (0,1) THEN 'a' WHEN (1,1) THEN 'b' ELSE 'z' END FROM nn`,
}

func TestR25RowValueDuplicatedProbeDeclines(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	for _, s := range r25CaseSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{}, r25DupProbeQ...), r25DupProbeTableQ...) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("[%s] PANIC: %v", q, r)
				}
			}()
			if _, _, err := p.QueryArgs(q, nil); err == nil {
				t.Errorf("[%s] answered a shape whose rewrite duplicates a nondeterministic operand; it must decline, never guess", q)
			}
		}()
	}
}
