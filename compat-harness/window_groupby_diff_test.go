// Differential gate for WINDOW functions combined with GROUP BY or implicit whole-table aggregate.
// Tests window function semantics within aggregate queries.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// windowGroupBySupported: every shape this engine answers, matching C SQLite exactly.
var windowGroupBySupported = []string{
	// Column-free subquery beside a window.
	`SELECT a, (SELECT 1), sum(b) OVER () FROM t GROUP BY a`,
	// Anchor rule and min/max that moves it.
	`SELECT a, sum(b) OVER () FROM t GROUP BY a`,
	// Min/max sites split across HAVING and ORDER BY.
	// SELECT LIST -> ORDER BY -> HAVING, which the derived-table rewrite
	// preserves, so this ANSWERS (verified against the oracle) where it used
	// to be pinned as a decline.
	`SELECT a, sum(b) OVER () FROM t GROUP BY a HAVING max(b)>1 ORDER BY min(c)`,
	`SELECT a, c, sum(b) OVER () FROM t GROUP BY a HAVING max(b)>1 ORDER BY min(c)`,
	`SELECT a, max(b), sum(b) OVER () FROM t GROUP BY a`,
	`SELECT a, min(b), sum(b) OVER () FROM t GROUP BY a`,
	`SELECT a, b, sum(b) OVER () FROM t GROUP BY a`,
	`SELECT a, max(b) OVER () FROM t GROUP BY a`,
	`SELECT a, min(b) OVER () FROM t GROUP BY a`,
	`SELECT a, count(b) OVER () FROM t GROUP BY a`,
	`SELECT a, avg(b) OVER () FROM t GROUP BY a`,
	`SELECT a, total(b) OVER () FROM t GROUP BY a`,
	`SELECT a, group_concat(c) OVER () FROM t GROUP BY a`,
	`SELECT a, sum(b+1) OVER () FROM t GROUP BY a`,
	`SELECT a, sum(a) OVER () FROM t GROUP BY a`,
	// an aggregate of an aggregate: the group's own value, windowed
	`SELECT a, sum(sum(b)) OVER () FROM t GROUP BY a`,
	`SELECT a, sum(sum(b)) OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, sum(sum(b)) OVER (PARTITION BY a) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER () + count(*) FROM t GROUP BY a`,
	`SELECT a, count(*) OVER (), count(*) FROM t GROUP BY a`,
	`SELECT a, abs(sum(b)) + sum(b) OVER () FROM t GROUP BY a`,
	`SELECT a, CASE WHEN sum(b) OVER () > 50 THEN 'big' ELSE 'small' END FROM t GROUP BY a`,
	// Count over groups.
	`SELECT a, count(*) OVER () FROM t GROUP BY a`,
	`SELECT count(*) OVER () FROM t GROUP BY a`,
	// Ranking and positional functions.
	`SELECT a, row_number() OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, rank() OVER (ORDER BY count(*)) FROM t GROUP BY a`,
	`SELECT a, dense_rank() OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, ntile(2) OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, percent_rank() OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, cume_dist() OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, first_value(b) OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, last_value(b) OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, nth_value(b,2) OVER (ORDER BY a) FROM t GROUP BY a`,
	`SELECT a, lag(b) OVER (ORDER BY a) FROM t GROUP BY a`,
	// Window PARTITION BY / ORDER BY with aggregates.
	`SELECT a, sum(b) OVER (PARTITION BY a%2) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY c) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY sum(b)) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (PARTITION BY sum(b)) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY count(*)) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER w FROM t GROUP BY a WINDOW w AS (ORDER BY a)`,
	// Explicit frames.
	`SELECT a, sum(b) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY a RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY a GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW) FROM t GROUP BY a`,
	`SELECT a, sum(b) FILTER (WHERE b>5) OVER () FROM t GROUP BY a`,
	// WHERE / HAVING / GROUP BY expression: all belong to the derived query,
	// so they filter BEFORE the window sees a row
	`SELECT a, sum(b) OVER () FROM t WHERE b>1 GROUP BY a`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a HAVING sum(b)>10`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a HAVING count(*)>1`,
	`SELECT a+0, sum(b) OVER () FROM t GROUP BY a+0`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a, c`,
	`SELECT a||'', sum(b) OVER () FROM t GROUP BY a`,
	// the outer ORDER BY: an ordinal and an output alias name OUTPUT columns;
	// anything else is lifted, so it sorts by the GROUPED value
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY a DESC`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY 2, 1`,
	`SELECT a, sum(b) OVER () AS w FROM t GROUP BY a ORDER BY w, a`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY c`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY sum(b)`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY max(b)`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY b`,
	`SELECT a FROM t GROUP BY a ORDER BY sum(b) OVER (), a`,
	// DISTINCT / LIMIT / OFFSET / "*" all belong to the outer (window) query
	`SELECT DISTINCT a%2, sum(b) OVER () FROM t GROUP BY a`,
	`SELECT sum(b) OVER () FROM t GROUP BY a LIMIT 2`,
	`SELECT sum(b) OVER () FROM t GROUP BY a LIMIT 1 OFFSET 1`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY a LIMIT 2`,
	`SELECT *, sum(b) OVER () FROM t GROUP BY a`,
	`SELECT t.*, sum(b) OVER () FROM t GROUP BY a`,
	// over a join, a view, a derived table, and an empty table
	`SELECT t.a, sum(t.b) OVER () FROM t, t AS u WHERE t.a=u.a GROUP BY t.a`,
	`SELECT t.a, sum(u.b) OVER () FROM t LEFT JOIN t AS u ON u.a=t.a+1 GROUP BY t.a`,
	`SELECT gg, sum(vv) OVER () FROM vw GROUP BY gg`,
	`SELECT x, sum(y) OVER () FROM (SELECT a AS x, b AS y FROM t) GROUP BY x`,
	`SELECT a, sum(b) OVER () FROM t WHERE 0 GROUP BY a`,
	`SELECT a, sum(b) OVER () FROM empty_t GROUP BY a`,
	// NULL group keys, a declared collation reaching the window through the
	// derived column
	`SELECT g, sum(v) OVER () FROM n GROUP BY g`,
	`SELECT g, sum(sum(v)) OVER () FROM n GROUP BY g`,
	`SELECT g, group_concat(label) OVER (ORDER BY label) FROM n GROUP BY g`,
	`SELECT g, row_number() OVER (PARTITION BY label ORDER BY g) FROM n GROUP BY g`,
	// a table whose real column names collide with the generated inner ones
	`SELECT a, sum(_w0) OVER () FROM wcol GROUP BY a`,
	`SELECT _w1, _w0, sum(_w0) OVER (ORDER BY _w1) FROM wcol GROUP BY a`,

	// The SAME rule with NO GROUP BY: a whole-table aggregate is one implicit
	// group, so the window sees exactly ONE row and count(*) OVER () is 1, not
	// the table's row count. All verified directly against C SQLite.
	`SELECT sum(sum(b)) OVER () FROM t`,
	`SELECT max(b), sum(b) OVER () FROM t`,
	`SELECT min(b), sum(b) OVER () FROM t`,
	`SELECT count(*) OVER (), sum(b) FROM t`,
	`SELECT count(b) OVER (), sum(b) FROM t`,
	`SELECT row_number() OVER (), sum(b) FROM t`,
	`SELECT row_number() OVER (ORDER BY sum(b)), count(*) FROM t`,
	`SELECT min(b) OVER (), sum(b) FROM t`,
	`SELECT max(b) OVER (), count(*) FROM t`,
	`SELECT lag(b) OVER (), sum(b) FROM t`,
	`SELECT ntile(2) OVER (), sum(b) FROM t`,
	`SELECT c, sum(b) OVER (), sum(b) FROM t`,
	`SELECT *, sum(b), sum(b) OVER () FROM t`,
	`SELECT sum(b) OVER () + count(*) FROM t`,
	`SELECT sum(b) FILTER (WHERE b>5) OVER (), count(*) FROM t`,
	`SELECT sum(b) OVER w, count(*) FROM t WINDOW w AS (ORDER BY sum(b))`,
	`SELECT t.a, sum(t.b) OVER (), count(*) FROM t, t AS u WHERE t.a=u.a`,
	`SELECT sum(b) OVER (), count(*) FROM t WHERE b>5`,
	// still exactly one row when nothing was scanned
	`SELECT sum(b) OVER (), count(*) FROM t WHERE 0`,
	`SELECT sum(b) OVER (), count(*) FROM empty_t`,
	`SELECT max(b), sum(b) OVER () FROM empty_t`,
	`SELECT sum(b) OVER (), count(*) FROM t ORDER BY 1`,
	`SELECT sum(b) OVER (), count(*) FROM t ORDER BY c`,
	`SELECT sum(b) OVER (), count(*) FROM t ORDER BY sum(b)`,
	`SELECT sum(b) OVER (), count(*) FROM t LIMIT 0`,
	`SELECT sum(b) OVER (), count(*) FROM t LIMIT 1 OFFSET 1`,
	`SELECT DISTINCT sum(b) OVER (), count(*) FROM t`,
	// Aggregate inside window's PARTITION BY / ORDER BY without GROUP BY.
	`SELECT sum(b) OVER (ORDER BY sum(c)) FROM t`,
	`SELECT sum(b) OVER (PARTITION BY sum(c)) FROM t`,
	`SELECT row_number() OVER (PARTITION BY sum(a)) FROM t`,
	`SELECT rank() OVER (ORDER BY sum(a), b) FROM t`,
	`SELECT sum(b) OVER (ORDER BY sum(a) DESC) FROM t`,
	`SELECT sum(b) OVER (ORDER BY sum(c)) FROM t WHERE b>5`,
	`SELECT sum(a) OVER (ORDER BY sum(b)) FROM empty_t`,
}

// windowGroupByErrors: rejected by C SQLite, must also be rejected here.
var windowGroupByErrors = []string{
	`SELECT a, sum(sum(b) OVER ()) FROM t GROUP BY a`,
	`SELECT a, count(*) FROM t GROUP BY a HAVING sum(b) OVER () > 3`,
	`SELECT a, sum(DISTINCT b) OVER () FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a ORDER BY 5`,
	// Window nested inside another window's argument / ORDER BY / FILTER.
	`SELECT a, sum(sum(b) OVER ()) OVER () FROM t GROUP BY a`,
	`SELECT a, sum(b) OVER (ORDER BY sum(b) OVER ()) FROM t GROUP BY a`,
	`SELECT a, sum(b) FILTER (WHERE sum(b) OVER () > 1) OVER () FROM t GROUP BY a`,
	// Name that resolves to nothing or conflicts with generated column names.
	`SELECT a, sum(b) OVER () FROM t GROUP BY _w0`,
	`SELECT a, sum(b) OVER () FROM t WHERE _w1>1 GROUP BY a`,
	`SELECT a, sum(b) OVER () FROM t GROUP BY a HAVING _w0>0`,
}

// windowGroupByDeclined: shapes C SQLite answers, this engine declines to avoid wrong answers.
var windowGroupByDeclined = []string{
	`SELECT (SELECT sum(b) OVER (ORDER BY sum(c)) FROM t) FROM n`,
}

func TestWindowGroupByParity(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b,c)`,
		`INSERT INTO t VALUES(1,50,'z'),(1,10,'a'),(2,1,'m'),(2,20,'b'),(3,30,'k')`,
		`CREATE TABLE empty_t(a,b)`,
		`CREATE TABLE n(g, v, label TEXT COLLATE NOCASE)`,
		`INSERT INTO n VALUES(NULL,3,'Bee'),(NULL,7,'ant'),(1,2,'CAT'),(1,NULL,'dog'),(2,5,'cat'),(3,NULL,NULL)`,
		`CREATE TABLE wcol(_w0, _w1, a)`,
		`INSERT INTO wcol VALUES(7,8,1),(9,10,1),(11,12,2)`,
		`CREATE VIEW vw AS SELECT g AS gg, v AS vv FROM n`,
	}
	edb, eerr := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if eerr != nil {
		t.Fatal(eerr)
	}
	defer edb.Close()
	cdb, cerr := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer cdb.Close()
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, perr := edb.SnapshotPager()
	if perr != nil {
		t.Fatal(perr)
	}

	for _, q := range windowGroupBySupported {
		eCols, eVals, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Errorf("[%s] engine declined a supported shape: %v", q, err)
			continue
		}
		cCols, cRows, err := cgoSelect(t, cdb, q, nil)
		if err != nil {
			t.Errorf("[%s] cgo: %v", q, err)
			continue
		}
		ordered := strings.Contains(strings.ToUpper(q), "ORDER BY")
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, ordered); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v %v\n  cgo:    %v %v",
				q, reason, eCols, engineRowsToStrings(eVals), cCols, cRows)
		}
	}

	for _, q := range windowGroupByErrors {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] engine answered a statement C SQLite rejects", q)
		}
		if _, _, err := cgoSelect(t, cdb, q, nil); err == nil {
			t.Errorf("[%s] cgo no longer rejects this -- the pinned oracle behavior changed", q)
		}
	}

	for _, q := range windowGroupByDeclined {
		if _, _, err := p.QueryArgs(q, nil); err == nil {
			t.Errorf("[%s] this shape is declined on purpose; if it now compiles, verify it against the oracle and move it to windowGroupBySupported", q)
		}
		if _, _, err := cgoSelect(t, cdb, q, nil); err != nil {
			t.Errorf("[%s] cgo now rejects this too -- move it to windowGroupByErrors: %v", q, err)
		}
	}
}
