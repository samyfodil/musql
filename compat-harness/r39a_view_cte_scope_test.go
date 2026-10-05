// Tests for CTE scope: a body's WITH clause is in scope for the whole
// expansion, including column list resolution. When a CTE shadows a same-named
// table, the CTE should win. Tests vary the readout and affinity/collation.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

func TestR39aViewCTEShadowReadouts(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(x TEXT COLLATE NOCASE, y INTEGER)`,
		`INSERT INTO t1 VALUES('ABC', 7)`,
		`CREATE TABLE t2(x TEXT COLLATE NOCASE, y INTEGER)`,
		`INSERT INTO t2 VALUES('ABC', 7)`,
		// star body, CTE renaming the columns
		`CREATE VIEW v2 AS SELECT * FROM t1`,
		`CREATE VIEW vs AS WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1`,
		// star body whose CTE keeps the table's own column names, so ONLY the
		// affinity/collation readouts can tell the two apart
		`CREATE VIEW vk AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT * FROM t1`,
		// named body, unaliased column references
		`CREATE VIEW vn AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT x, y FROM t1`,
		// qualified body
		`CREATE VIEW vq AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT t1.x, t1.y FROM t1`,
		// the CTE shadows only one side of a join
		`CREATE VIEW vj AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT t1.x AS a, t2.x AS b FROM t1, t2`,
		// ... and the same join with the two x's left to collide, so the view's
		// own column list has to carry sqlite3ColumnsFromExprList' ":N" suffix
		`CREATE VIEW vd AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT t1.x, t2.x FROM t1, t2`,
		// a body whose CTE shadows another VIEW's name
		`CREATE VIEW vbase AS SELECT 'ZZZ' AS x, 0 AS y`,
		`CREATE VIEW vv AS WITH vbase(x,y) AS (SELECT 'abc', 1) SELECT * FROM vbase`,
		// a view over a view whose body shadows
		`CREATE VIEW vouter AS SELECT * FROM vk`,
		// explicit "(col,...)" rename list over a shadowing body
		`CREATE VIEW vr(a,b) AS WITH t1(x,y) AS (SELECT 'abc', 1) SELECT * FROM t1`,
		// a shadowing body that also carries a LIMIT -- the shape the flattener
		// (flatten_limit_r35c.go) reaches, where inlining the body would resolve
		// its FROM in the PARENT's scope
		`CREATE VIEW vl AS WITH t1(x,y) AS (SELECT 'abc',1 UNION ALL SELECT 'zzz',2) SELECT * FROM t1 LIMIT 2`,
		`CREATE VIEW vl1 AS WITH t1(x,y) AS (SELECT 'abc',1 UNION ALL SELECT 'zzz',2) SELECT * FROM t1 LIMIT 1`,
	}
	probes := []string{
		// ---- names ----
		`SELECT * FROM vs`,
		`SELECT * FROM vk`,
		`SELECT * FROM vn`,
		`SELECT * FROM vq`,
		`SELECT * FROM vj`,
		`SELECT * FROM vv`,
		`SELECT * FROM vouter`,
		`SELECT * FROM vr`,
		// ---- values ----
		`SELECT typeof(x), typeof(y) FROM vk`,
		`SELECT typeof(x), typeof(y) FROM vouter`,
		// COLLATION: t1.x is NOCASE, the CTE's x is BINARY. Reading the column
		// list off the table makes 'ABC' match where it must not.
		`SELECT count(*) FROM vk WHERE x = 'ABC'`,
		`SELECT count(*) FROM vk WHERE x = 'abc'`,
		`SELECT count(*) FROM vn WHERE x = 'ABC'`,
		`SELECT count(*) FROM vq WHERE x = 'ABC'`,
		`SELECT count(*) FROM vj WHERE a = 'ABC'`,
		`SELECT * FROM vd`,
		`SELECT count(*) FROM vd WHERE x = 'ABC'`,
		`SELECT count(*) FROM vd WHERE "x:1" = 'abc'`,
		`SELECT count(*) FROM vouter WHERE x = 'ABC'`,
		// AFFINITY: t1.y is INTEGER-declared, the CTE's y is not, and a
		// comparison applies the column's affinity to the text literal.
		`SELECT count(*) FROM vk WHERE y = '1'`,
		`SELECT count(*) FROM vn WHERE y = '1'`,
		`SELECT count(*) FROM vouter WHERE y = '1'`,
		// ---- the flattener's shapes ----
		`SELECT * FROM vl ORDER BY 1 DESC`,
		`SELECT * FROM vl1 ORDER BY 1 DESC`,
		`SELECT count(*) FROM vl WHERE x = 'ABC'`,
		// ---- the shadow must not LEAK ----
		`SELECT * FROM t1`,
		`SELECT count(*) FROM t1 WHERE x = 'abc'`,
		`SELECT vk.x, t1.x FROM vk, t1`,
		// ... and the QUERYING statement's CTE must not reach inside the view
		// (view2.test): here it would rename v2's columns and change its rows.
		`WITH t1(a,b) AS (SELECT 3,4) SELECT * FROM v2`,
		`WITH t1(x,y) AS (SELECT 'zzz',3) SELECT * FROM vl`,
	}
	r39aRunProbes(t, setup, probes)
}

// TestR39aInlineCTEShadowReadouts is the same rule at the sites a VIEW never
// reaches: an inline derived table, a CTE body nesting a WITH of its own, and
// the IN/EXISTS/scalar subquery bodies. Each has its own column-list derivation
// and each got the scope wrong independently, so one fixture is run through all
// of them.
func TestR39aInlineCTEShadowReadouts(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(x TEXT COLLATE NOCASE, y INTEGER)`,
		`INSERT INTO t1 VALUES('ABC',7)`,
	}
	probes := []string{
		// an inline derived table carrying its own WITH
		`SELECT * FROM (WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1)`,
		`SELECT * FROM (WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1) AS d`,
		`SELECT count(*) FROM (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) WHERE x='ABC'`,
		`SELECT count(*) FROM (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) WHERE y='1'`,
		// ... and as a join partner beside the very table it shadows
		`SELECT count(*) FROM (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) AS d, t1 WHERE d.x='ABC'`,
		// a CTE whose own body nests a shadowing WITH
		`WITH c AS (WITH t1(p,q) AS (SELECT 9,9) SELECT * FROM t1) SELECT * FROM c`,
		`WITH c AS (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) SELECT count(*) FROM c WHERE x='ABC'`,
		`WITH c AS (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) SELECT count(*) FROM c WHERE y='1'`,
		// expression-position subqueries
		`SELECT (WITH t1(x,y) AS (SELECT 'abc',1) SELECT x FROM t1)`,
		`SELECT (SELECT count(*) FROM (WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1) WHERE x='ABC')`,
		`SELECT EXISTS(WITH t1(x,y) AS (SELECT 'abc',1) SELECT * FROM t1 WHERE x='ABC')`,
		// the IN-subquery's RHS collation over a body that does NOT shadow --
		// the shadowing spelling is subqueryScopes' (see this file's header).
		`SELECT 'abc' IN (SELECT x FROM t1)`,
		`SELECT 'abc' IN (WITH cc(x) AS (SELECT 'ABC') SELECT x FROM cc)`,
	}
	r39aRunProbes(t, setup, probes)
}

// r39aRunProbes runs setup then probes on both engines and reports every
// disagreement. A failure here is a WRONG ANSWER or a NEW DECLINE, never a
// missing feature: every probe's oracle answer is recorded by the run itself.
//
// probes must not REPEAT a statement. The answers are keyed by the statement
// text, so a second occurrence overwrites the first and both comparisons then
// read the LAST value -- which, in a list containing a DELETE, silently reports
// the post-delete state for the pre-delete probe. That is not hypothetical: it
// hid the write-path half of this very finding while it was being measured.
func r39aRunProbes(t *testing.T, setup, probes []string) {
	t.Helper()
	seen := make(map[string]bool, len(probes))
	for _, q := range probes {
		if seen[q] {
			t.Fatalf("probe repeated, which makes its result unreadable -- give it a distinct alias: %s", q)
		}
		seen[q] = true
	}
	got := map[string]string{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "r39a.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range setup {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", drv, q, err)
			}
		}
		for _, q := range probes {
			out := r39aQueryString(t, db, q)
			if drv == "sqlite" {
				got[q] = out
				continue
			}
			if got[q] != out {
				t.Errorf("%s\n  engine: %s\n  cgo:    %s", q, got[q], out)
			}
		}
		db.Close()
	}
}

// r39aQueryString renders a query's column names and rows (or its error) as one
// comparable string -- the same shape as queryString (vdbe_view_cte_scope_test.go),
// kept separate so this file's readouts can be widened without touching that gate.
func r39aQueryString(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return "ERR"
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := fmt.Sprintf("cols=%v", cols)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR"
		}
		out += fmt.Sprintf(" %v", vals)
	}
	if rows.Err() != nil {
		return "ERR"
	}
	return out
}
