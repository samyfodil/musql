package compat

// Index-order planning with subqueries. Subqueries used to decline index-order
// plans entirely, causing wrong answer (rows in rowid order instead of index order).
// Fixture has a column with 1 (INTEGER) and 1.0 (REAL) with different b values,
// so order differences are observable.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r36dRows is the fixture: a and b columns with observable order differences.
// differently from the table.
const r36dRows = "INSERT INTO %T VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)"

var r36dSchemas = []struct{ name, ddl string }{
	{"noidx", ""},
	{"idx-a", "CREATE INDEX %T_i1 ON %T(a)"},
	{"idx-a-desc", "CREATE INDEX %T_i1 ON %T(a DESC)"},
	{"idx-ab", "CREATE INDEX %T_i1 ON %T(a,b)"},
	{"idx-ba", "CREATE INDEX %T_i1 ON %T(b,a)"},
	{"idx-a-nocase", "CREATE INDEX %T_i1 ON %T(a COLLATE NOCASE)"},
	{"idx-cover", "CREATE INDEX %T_i1 ON %T(a,b,c)"},
	{"two-idx", "CREATE INDEX %T_i1 ON %T(a); CREATE INDEX %T_i2 ON %T(b)"},
}

// r36dReads are select lists carrying a nested SELECT. Each names a different
// way lookupName resolves the names inside it:
//
//   - uncorrelated with no column reference at all (contributes no bit);
//   - uncorrelated, every reference QUALIFIED by the subquery's own alias
//     (contributes no bit either, but only a scope-aware walk can tell);
//   - uncorrelated, reference UNQUALIFIED and shadowed by the subquery's own
//     FROM clause -- the case a qualifier-only rule would get wrong;
//   - CORRELATED, which really does set the enclosing item's bit, and on a
//     column the index does or does not carry;
//   - correlated through a ROWID, which sets fg.rowidUsed and no colUsed bit;
//   - a subquery two deep, one reached through coalesce()/CASE, and two over a
//     DERIVED table -- one projecting a bare column, one a "*" whose expansion
//     is what supplies the shadowing names.
var r36dReads = []struct{ name, sql string }{
	{"const-sub", `a, (SELECT count(*) FROM %T AS s)`},
	{"qualified-sub", `a, (SELECT count(*) FROM %T AS s WHERE s.a = 9)`},
	{"shadowed-bare", `a, (SELECT max(a) FROM %T AS s)`},
	{"corr-sub", `a, (SELECT count(*) FROM %T AS s WHERE s.a = t.a)`},
	{"corr-rowid", `a, (SELECT count(*) FROM %T AS s WHERE s.rowid = t.rowid)`},
	{"corr-other-col", `a, (SELECT count(*) FROM %T AS s WHERE s.b = t.b)`},
	{"corr-uncovered", `a, (SELECT count(*) FROM %T AS s WHERE s.a = t.c)`},
	{"exists-sub", `a, EXISTS (SELECT 1 FROM %T AS s WHERE s.a > t.a)`},
	{"not-exists-sub", `a, NOT EXISTS (SELECT 1 FROM %T AS s WHERE s.c > t.c)`},
	{"in-sub", `a, a IN (SELECT s.a FROM %T AS s WHERE s.b <> t.b)`},
	{"nested-two-deep", `a, (SELECT (SELECT count(*) FROM %T AS u WHERE u.a = t.a) FROM %T AS s LIMIT 1)`},
	{"fromless-sub", `a, (SELECT t.b)`},
	{"case-with-sub", `a, CASE WHEN EXISTS (SELECT 1 FROM %T AS s WHERE s.c = t.c) THEN 1 ELSE 0 END`},
	{"sub-in-func", `a, coalesce((SELECT min(s.c) FROM %T AS s WHERE s.a = t.a), -1)`},
	{"sub-over-derived", `a, (SELECT count(*) FROM (SELECT a FROM %T) AS d)`},
	{"star-sub", `a, (SELECT count(*) FROM (SELECT s.* FROM %T AS s WHERE s.a = t.a))`},
}

var r36dTails = []string{
	"",
	"ORDER BY a",
	"ORDER BY a DESC",
	"LIMIT 3",
	"ORDER BY b LIMIT 3",
	"WHERE a >= 1",
}

type r36dCase struct {
	shape string
	label string // read x schema, the bucket a divergence is reported under
	setup []string
	query string
}

// r36dCases builds the battery: schema x subquery readout x tail, each over its
// own table so a whole batch can share one worker invocation.
func r36dCases() []r36dCase {
	var cases []r36dCase
	n := 0
	for _, sc := range r36dSchemas {
		for _, rd := range r36dReads {
			for _, tail := range r36dTails {
				tbl := fmt.Sprintf("t%d", n)
				n++
				sub := func(s string) string { return strings.ReplaceAll(s, "%T", tbl) }
				setup := []string{sub("CREATE TABLE %T(a, b, c)"), sub(r36dRows)}
				if sc.ddl != "" {
					for _, d := range strings.Split(sub(sc.ddl), ";") {
						if d = strings.TrimSpace(d); d != "" {
							setup = append(setup, d)
						}
					}
				}
				// The readouts name the scanned table as "t", which is what the
				// correlated references qualify with, so the FROM item is
				// aliased rather than renamed.
				q := "SELECT " + sub(rd.sql) + " FROM " + tbl + " AS t"
				if tail != "" {
					q += " " + tail
				}
				cases = append(cases, r36dCase{
					shape: sc.name + "/" + rd.name + "/" + tail,
					label: rd.name + "/" + sc.name,
					setup: setup, query: q,
				})
			}
		}
	}
	return cases
}

// r36dDeclineCeiling is the number of battery cases this engine declined at the
// commit that last moved it. A CEILING, not an expectation: the test fails only
// when declines go UP, so lifting one is never a red test. Lower it whenever a
// change lowers the count.
//
//	768 cases at 6cd2352 (before the colUsed walk), measured by restoring that
//	    tree's engine/ under this battery: agree=423 declined=0 wrong=345.
//	after r36dSubColUsed landed: agree=768 declined=0 wrong=0.
const r36dDeclineCeiling = 0

func TestR36DSubqueryColUsed(t *testing.T) {
	cases := r36dCases()
	var agree, declined, wrong, mutual int
	wrongBy := map[string]int{}

	// One worker process per batch, per engine. r30's own note applies: the
	// batch size is the whole wall-clock story for a battery this wide.
	const batch = 32
	for start := 0; start < len(cases); start += batch {
		end := start + batch
		if end > len(cases) {
			end = len(cases)
		}
		var stmts []string
		qAt := make([]int, 0, batch)
		for _, c := range cases[start:end] {
			stmts = append(stmts, c.setup...)
			qAt = append(qAt, len(stmts))
			stmts = append(stmts, c.query)
		}
		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for k, c := range cases[start:end] {
			cr, mr := cgo[qAt[k]], mush[qAt[k]]
			cb, _ := json.Marshal(cr)
			mb, _ := json.Marshal(mr)
			cErr, mErr := cr["kind"] == "error", mr["kind"] == "error"
			switch {
			case cErr && mErr:
				mutual++
			case cErr != mErr:
				declined++
				t.Logf("R36D declined %s\n  sql: %s\n  cgo: %s\n  mus: %s", c.shape, c.query, cb, mb)
			case string(cb) != string(mb):
				wrong++
				wrongBy[c.label]++
				if wrong <= 12 {
					t.Errorf("R36D WRONG %s\n  sql: %s\n  cgo: %s\n  mus: %s", c.shape, c.query, cb, mb)
				}
			default:
				agree++
			}
		}
	}
	keys := make([]string, 0, len(wrongBy))
	for k := range wrongBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("R36D wrong %-32s %d", k, wrongBy[k])
	}
	t.Logf("R36D SUBQUERY-COLUSED BATTERY: cases=%d agree=%d declined=%d wrong=%d mutualReject=%d",
		len(cases), agree, declined, wrong, mutual)
	if declined > r36dDeclineCeiling {
		t.Errorf("R36D declines rose to %d (ceiling %d): a shape this battery used to "+
			"serve now errors. Find it, then either fix it or -- if the decline is "+
			"deliberate and correct -- raise the ceiling in the same commit.",
			declined, r36dDeclineCeiling)
	}
}

// TestR36DSubqueryColUsedShadowing pins the SHADOWING rule itself, which is what
// makes the walk exact rather than a guess: a name spelled inside a subquery
// belongs to the innermost FROM clause that supplies it, and only a name that
// climbs OUT of every nested clause reaches the enclosing item's colUsed. The
// cases below differ only in which columns the subquery's own FROM supplies, so
// a walk that ignored shadowing would credit the same bits in all of them and
// get some wrong.
func TestR36DSubqueryColUsedShadowing(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"shadowed-by-inner", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(b, z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES('x',1),('z',2)",
			"CREATE INDEX i1 ON t(a,b)",
			// "b" inside the subquery is u's, not t's, so it must NOT set t's
			// bit for b.
			"SELECT a, (SELECT count(*) FROM u WHERE b='x') FROM t",
		}},
		{"climbs-out", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1),(2)",
			"CREATE INDEX i1 ON t(a,b)",
			// "b" is supplied by no nested item, so it climbs out to t and DOES
			// set t's bit -- i1(a,b) stays covering.
			"SELECT a, (SELECT count(*) FROM u WHERE z=1 AND b='x') FROM t",
		}},
		{"climbs-out-uncovered", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1),(2)",
			"CREATE INDEX i1 ON t(a,b)",
			// "c" climbs out too, and t.c is NOT in i1, so the index stops being
			// covering and the plan changes again.
			"SELECT a, (SELECT count(*) FROM u WHERE z=1 AND c=10) FROM t",
		}},
		{"cte-shadows-table", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE s(a, b)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO s VALUES(9,'q')",
			"CREATE INDEX i1 ON t(a,b)",
			// The CTE named "s" shadows the real table "s"; resolving the
			// subquery's FROM through the schema would read the wrong columns,
			// which is why a statement carrying its own WITH declines the walk.
			"WITH s(zz) AS (SELECT 7) SELECT a, (SELECT count(*) FROM s WHERE zz=7) FROM t",
		}},
		{"view-in-subquery", []string{
			"CREATE TABLE t(a, b, c)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"CREATE VIEW v AS SELECT a AS q FROM t",
			"CREATE INDEX i1 ON t(a,b)",
			"SELECT a, (SELECT count(*) FROM v WHERE q>1) FROM t",
		}},
		{"outer-join-in-subquery", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(k, z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1,'p'),(2,'q')",
			"CREATE INDEX i1 ON t(a,b)",
			"SELECT a, (SELECT count(*) FROM u LEFT JOIN t AS s ON s.a=u.k WHERE s.b IS NOT NULL) FROM t",
		}},
		{"using-join-in-subquery", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(a, z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1,'p'),(2,'q')",
			"CREATE INDEX i1 ON t(a,b)",
			"SELECT a, (SELECT count(*) FROM u JOIN t AS s USING (a)) FROM t",
		}},
		{"without-rowid-in-subquery", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE w(k TEXT PRIMARY KEY, z) WITHOUT ROWID",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO w VALUES('p',1),('q',2)",
			"CREATE INDEX i1 ON t(a,b)",
			"SELECT a, (SELECT count(*) FROM w WHERE z=1) FROM t",
		}},
		// The other half of the climb rule -- an inner item that ANSWERS to the
		// qualifier but supplies no such column, so lookupName climbs and the
		// bit lands OUTSIDE -- has no case here because this engine declines
		// that statement outright today ("SELECT a, (SELECT count(*) FROM u AS
		// t WHERE t.c=10) FROM t": the oracle answers, musql errors). The rule
		// is implemented anyway, with the oracle transcript recorded in
		// engine/where_plan_subcolused_r36d.go -- closing that decline without
		// it would expose the wrong answer, which is exactly how this project
		// has found several.
		{"alias-shadows-outer-column", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1),(2)",
			"CREATE INDEX i1 ON t(a,b)",
			// "c" in the subquery's WHERE is its own result-column ALIAS, which
			// resolve.c's NC_UEList block matches after the SrcList and the
			// rowid fallback both miss -- so it never reaches the outer t and
			// i1(a,b) stays covering. Letting it climb would mark t.c and pick
			// the table scan instead.
			"SELECT a, (SELECT z AS c FROM u WHERE c=1 LIMIT 1) FROM t",
		}},
		{"qualifier-matches-rowid", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1),(2)",
			"CREATE INDEX i1 ON t(a,b)",
			// The same shape with a ROWID name, which the aliased u DOES answer
			// (VisibleRowid), so it stops there and sets no bit at all.
			"SELECT a, (SELECT count(*) FROM u AS t WHERE t.rowid=1) FROM t",
		}},
		{"rowid-name-in-subquery", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(oid, z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1,'p'),(2,'q')",
			"CREATE INDEX i1 ON t(a,b)",
			// "oid" is a REAL column of u here, not the rowid fallback.
			"SELECT a, (SELECT count(*) FROM u WHERE oid=1) FROM t",
		}},
		{"agg-anchor-with-sub", []string{
			"CREATE TABLE t(a, b, c)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"CREATE INDEX i1 ON t(a,b)",
			// A BARE column beside an aggregate is the anchor-row shape, whose
			// guard reads the same colUsed.
			"SELECT b, max(a), (SELECT count(*) FROM t AS s WHERE s.a = t.a) FROM t",
			"SELECT b, max(a), (SELECT count(*) FROM t AS s WHERE s.a = t.a) FROM t GROUP BY a",
		}},
		{"join-with-sub", []string{
			"CREATE TABLE t(a, b, c)",
			"CREATE TABLE u(k, z)",
			"INSERT INTO t VALUES(3,'x',10),(1,'Y',20),(2,'x',30),(2,'z',40),(NULL,'w',50),(1.0,'X',60)",
			"INSERT INTO u VALUES(1,'p'),(2,'q'),(3,'r')",
			"CREATE INDEX i1 ON t(a,b)",
			"CREATE INDEX i2 ON u(k)",
			// The MULTI-table arm reads the same colUsed for its automatic-index
			// key and its covering test.
			"SELECT t.a, u.z, (SELECT count(*) FROM t AS s WHERE s.a = t.a) FROM t, u WHERE u.k = t.a",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { differ(t, tc.name, tc.stmts) })
	}
}
