// This file tests window function projection and aggregate arguments.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// g2Fixture is the test setup for window projection tests.
var g2Fixture = []string{
	`CREATE TABLE w(a, b TEXT, c TEXT COLLATE NOCASE, d)`,
	`INSERT INTO w VALUES(1,'A','apple',10),(2,'B','APPLE',20),(3,'C','pear',30),` +
		`(3,'D','PEAR',30),(5,'E','fig',50),(7,'F','Fig',70),(NULL,'G','date',NULL)`,
	`CREATE TABLE u(k, v)`,
	`INSERT INTO u VALUES(1,100),(2,200),(9,900)`,
	`CREATE VIEW wv AS SELECT a, b, d FROM w`,
	`CREATE TABLE cst(x TEXT)`,
	`INSERT INTO cst VALUES('5x'),('12'),('abc'),(NULL)`,
}

func g2Parity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), g2Fixture...), probes...))
}

// TestG2ProjectionValues drives the compiled projection over every expression
// shape a select list can hold. Each of these lowers (asserted separately by
// engine/window_projection_test.go); what is measured here is that the compiled
// value is byte-identical to 3.53.3's.
func TestG2ProjectionValues(t *testing.T) {
	g2Parity(t, "projection-scalars", []string{
		`SELECT a, sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a+1, -a, a%2, a*1.0/2, sum(d) OVER () FROM w ORDER BY b`,
		`SELECT b||'!', upper(c), length(b), abs(a), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT CASE WHEN a>2 THEN b ELSE c END, sum(d) OVER () FROM w ORDER BY b`,
		`SELECT coalesce(a,-1), ifnull(d,0), nullif(a,3), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a IS NULL, a IS NOT NULL, a BETWEEN 2 AND 5, b LIKE 'A%', sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a IN (1,3,5), b GLOB 'A*', sum(d) OVER () FROM w ORDER BY b`,
		`SELECT c = 'APPLE', c COLLATE BINARY = 'APPLE', sum(d) OVER () FROM w ORDER BY b`,
		`SELECT rowid, a, row_number() OVER (ORDER BY a) FROM w ORDER BY rowid`,
		`SELECT *, count(*) OVER () FROM w ORDER BY b`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM wv ORDER BY b`,
	})

	// A window function's value used INSIDE a larger expression is the case
	// that needs its register read to compose like any other operand
	// (expr.c:5358-5360), not to be a special top-level shape.
	g2Parity(t, "projection-over-window-values", []string{
		`SELECT sum(d) OVER () + 1 FROM w ORDER BY b`,
		`SELECT a, sum(d) OVER (ORDER BY a) * 2 - a FROM w ORDER BY b`,
		`SELECT CASE WHEN row_number() OVER (ORDER BY a) > 3 THEN 'late' ELSE 'early' END FROM w ORDER BY b`,
		`SELECT abs(sum(d) OVER (ORDER BY a) - 50) FROM w ORDER BY b`,
		`SELECT count(*) OVER () || '/' || sum(d) OVER () FROM w ORDER BY b`,
		`SELECT sum(d) OVER (ORDER BY a) IS NULL, lag(a) OVER (ORDER BY a) IS NULL FROM w ORDER BY b`,
		`SELECT CAST(sum(d) OVER () AS TEXT), sum(d) OVER () FROM w ORDER BY b`,
		// The same call written twice is two slots (rewriteWindowCalls matches
		// by call site, not by structure), so both registers must be filled.
		`SELECT sum(d) OVER (ORDER BY a), sum(d) OVER (ORDER BY a) FROM w ORDER BY b`,
		// Two DIFFERENT specs in one projection.
		`SELECT sum(d) OVER (ORDER BY a), sum(d) OVER (PARTITION BY a%2) FROM w ORDER BY b`,
	})

	// CAST beside a second read of the SAME column. The projection's register
	// block is shared between the projected expressions, so an OP_Cast pointed
	// at its operand's own register (rather than at a fresh one, the way
	// sqlite3ExprCode guarantees at expr.c:5904-5924) would rewrite the row.
	g2Parity(t, "projection-cast-aliasing", []string{
		`SELECT CAST(b AS INTEGER), b, sum(d) OVER () FROM w ORDER BY b`,
		`SELECT CAST(x AS INTEGER), x, CAST(x AS REAL), x||'', count(*) OVER () FROM cst ORDER BY x`,
		`SELECT CAST(x AS INTEGER) || '-' || x, count(*) OVER () FROM cst ORDER BY x`,
		`SELECT CAST(a AS TEXT), a+0, sum(d) OVER () FROM w ORDER BY b`,
	})
}

// TestG2ProjectionOrdering drives the ORDER BY half of the projection list: an
// ordinal and an output alias take their value from the projected row, an
// expression term gets its own slot (buildWindowProjList). A wrong index there
// sorts by some other expression's value, which looks plausible and is wrong.
func TestG2ProjectionOrdering(t *testing.T) {
	g2Parity(t, "projection-order", []string{
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY 2`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY 2 DESC`,
		`SELECT a, sum(d) OVER (ORDER BY a) AS s FROM w ORDER BY s`,
		`SELECT a, sum(d) OVER (ORDER BY a) AS s FROM w ORDER BY s DESC, a`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w ORDER BY b||a`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w ORDER BY a%2, b DESC`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w ORDER BY a NULLS FIRST, b`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w ORDER BY a NULLS LAST, b`,
		`SELECT a, c, sum(d) OVER () FROM w ORDER BY c, b`,
		`SELECT a, c, sum(d) OVER () FROM w ORDER BY c COLLATE BINARY, b`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY 1, 2, a+1, b`,
		// A term that is itself a window value, by alias and by expression.
		`SELECT a, row_number() OVER (ORDER BY b DESC) AS r FROM w ORDER BY r`,
		`SELECT a, b FROM w ORDER BY row_number() OVER (ORDER BY b DESC)`,
	})

	g2Parity(t, "projection-distinct-limit", []string{
		`SELECT DISTINCT a%2, sum(d) OVER () FROM w ORDER BY 1`,
		`SELECT DISTINCT b, count(*) OVER () FROM w ORDER BY 1`,
		// NOT here: "SELECT DISTINCT c, ..." over the NOCASE column. That is a
		// PRE-EXISTING divergence of its own and nothing to do with this slice
		// -- windowFinal dedups with keysEqual, which compares BINARY, so
		// 'apple'/'APPLE' survive as two rows where 3.53.3 keeps one. VERIFIED
		// unchanged with the projection lowering disabled. The window path is
		// missing the non-BINARY DISTINCT decline the ordinary scan already has
		// (vdbe_scan.go); fixing it needs collation-aware dedup, not a gate.
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY b LIMIT 3`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY b LIMIT 3 OFFSET 2`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM w ORDER BY b LIMIT 0`,
		`SELECT DISTINCT a, sum(d) OVER (PARTITION BY a) FROM w ORDER BY 1 LIMIT 2`,
	})
}

// TestG2ProjectionDeclineShapes drives the shapes compileWindowProjection once
// refused. Only ONE of them is still a refusal -- the ambiguous name, which is
// now the statement's own compile ERROR, there being nowhere left for it to be
// raised at run time instead (AGENTS.md Rule 1). Everything else here LOWERS
// today, and the point of keeping them running is unchanged: each was measured
// against the oracle before it lowered, so any of them answering differently
// now is a lowering that changed a value.
func TestG2ProjectionDeclineShapes(t *testing.T) {
	// The AMBIGUOUS name is the case that would be a WRONG ANSWER rather than an
	// error. The register-backed resolveRowReg (vdbe_codegen.go) resolves
	// innermost-wins, and compiler.regScopeStrict is what makes a second match
	// raise "ambiguous column name" instead -- resolve.c:785's own answer --
	// rather than a value picked from one arbitrary table where 3.53.3 raises.
	// That error used to be raised at run time and is the COMPILE's error now;
	// both engines must still reject, which is
	// what g2Parity checks. A bare rowid across two rowid tables is the same
	// rule.
	g2Parity(t, "projection-joins-ambiguous", []string{
		`SELECT a, sum(d) OVER () FROM w, w AS w2 ORDER BY 1`,
		`SELECT b, count(*) OVER () FROM w JOIN w AS w2 ON w2.a=w.a ORDER BY 1`,
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w, w AS w2 WHERE w2.a=w.a ORDER BY 1`,
		`SELECT rowid, sum(w.d) OVER () FROM w, u ORDER BY 1`,
	})

	// An UNAMBIGUOUS join now LOWERS (engine/window_projection_test.go asserts
	// the program exists); these measure that its two register scopes address
	// the right slices of the batch row and the right rowid slots.
	g2Parity(t, "projection-joins", []string{
		`SELECT w.a, u.v, sum(w.d) OVER () FROM w, u WHERE u.k=w.a ORDER BY w.b`,
		`SELECT w.a, u.v, sum(w.d) OVER (ORDER BY w.a) FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
		`SELECT w.b, u.v, count(*) OVER () FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
		`SELECT b, v, sum(d) OVER () FROM w JOIN u ON k=a ORDER BY b`,
		// Unqualified reads of BOTH sides, so a scope whose registers were
		// based at the wrong offset reads a neighbour's value.
		`SELECT a, b, c, d, k, v, sum(d) OVER (ORDER BY a) FROM w, u ORDER BY b, k`,
		// Qualified rowids, one per scope: the trailing rowid BLOCK, not just
		// its first slot. Seeding only slot 0 answers w's rowid for u's.
		`SELECT w.rowid, u.rowid, w.b, count(*) OVER () FROM w, u ORDER BY w.b, u.k`,
		`SELECT u.rowid, w.rowid, row_number() OVER (ORDER BY w.b, u.k) FROM w, u ORDER BY 3`,
		// Three scopes, and a NULL-extended LEFT JOIN row through the same
		// register block.
		`SELECT w.b, u.v, x.k, count(*) OVER () FROM w, u, u AS x WHERE x.k=u.k ORDER BY w.b, u.k`,
		`SELECT w.b, u.v, sum(w.d) OVER (PARTITION BY u.v) FROM w LEFT JOIN u ON u.k=w.a ORDER BY w.b`,
	})

	g2Parity(t, "projection-subqueries", []string{
		`SELECT a, (SELECT max(v) FROM u), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a, (SELECT v FROM u WHERE k=w.a), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a, EXISTS(SELECT 1 FROM u WHERE k=w.a), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a, a IN (SELECT k FROM u), sum(d) OVER () FROM w ORDER BY b`,
		`SELECT a, (SELECT count(*) FROM u WHERE k<w.a) + sum(d) OVER () FROM w ORDER BY b`,
	})

	g2Parity(t, "projection-correlated-outer", []string{
		// The window query is a correlated subquery of the outer one, so its
		// projection names a column of an enclosing scope. It has no compiler
		// chain for that reference and does not need one: the reference is
		// BUFFERED as a batch column computed in the scan body, exactly as C
		// buffers it (windowBufCols, window.c:788-817).
		`SELECT k, (SELECT sum(d) OVER () FROM w WHERE w.a=u.k LIMIT 1) FROM u ORDER BY k`,
		`SELECT k, (SELECT u.v + sum(w.d) OVER () FROM w LIMIT 1) FROM u ORDER BY k`,
	})

	g2Parity(t, "projection-cte-and-group", []string{
		`WITH q(x,y) AS (SELECT a, d FROM w) SELECT x, sum(y) OVER (ORDER BY x) FROM q ORDER BY x`,
		`SELECT a, sum(d) OVER (ORDER BY a) FROM (SELECT a, d FROM w WHERE a IS NOT NULL) ORDER BY a`,
		`SELECT a, count(*) OVER (), sum(d) FROM w GROUP BY a ORDER BY a`,
		`SELECT a%2 AS g, sum(sum(d)) OVER (ORDER BY a%2) FROM w GROUP BY a%2 ORDER BY g`,
	})
}

// TestG2ProjectionParams re-executes one prepared statement with several
// bindings. The compiled projection's machine carries the statement's
// parameters for exactly this: an OpVariable read that saw a nil parameter list
// would answer NULL for every binding, which no single-execution test notices.
func TestG2ProjectionParams(t *testing.T) {
	g2ParamParity(t, "projection-params",
		`SELECT ?1, a + ?1, CASE WHEN a > ?1 THEN 'hi' ELSE 'lo' END, sum(d) OVER () FROM w ORDER BY b`,
		[][]any{{0}, {1}, {2}, {3}, {5}, {7}, {nil}})
	g2ParamParity(t, "projection-params-order",
		`SELECT a, b, sum(d) OVER (ORDER BY a) FROM w ORDER BY (a > ?1), b`,
		[][]any{{1}, {3}, {5}})
}

// TestG2WindowAggArgEagerness is the measured half of the aggregate-argument
// lowering, and it is where three WRONG ANSWERS lived.
//
// A lowered argument is computed for every row the scan BUFFERS, which is what
// C does -- the expression sits in the generated sub-select's expression list
// (window.c:1047) and its FILTER beside it (window.c:1049-1051), so both run
// once per buffered row no matter what the frame or the FILTER later decides.
// Before the lowering this engine evaluated them lazily, per frame position,
// and so ANSWERED where 3.53.3 raises "integer overflow".
//
// The json_group_* cases are the other side of the same rule and are why the
// allow-list stops where it does: those are the SQLITE_SUBTYPE aggregates
// (json.c:5699 and :5706), for which SQLite sets bExprArgs and does NOT buffer the
// argument (window.c:1041-1044), re-coding it at step time instead
// (window.c:1733) -- so they must stay lazy, and they do.
func TestG2WindowAggArgEagerness(t *testing.T) {
	const bad = `CASE WHEN a=7 THEN abs(-9223372036854775808) ELSE a END`
	g2Parity(t, "agg-arg-eager", []string{
		// The a=7 row is the last one and this frame only ever looks BACKWARD,
		// so no frame ever contains it: a lazy evaluator never touches its
		// argument.
		`SELECT a, sum(` + bad + `) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM w WHERE a IS NOT NULL ORDER BY a`,
		`SELECT a, group_concat(` + bad + `) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM w WHERE a IS NOT NULL ORDER BY a`,
		`SELECT a, avg(` + bad + `) OVER (ORDER BY a ROWS BETWEEN 3 PRECEDING AND 2 PRECEDING) FROM w WHERE a IS NOT NULL ORDER BY a`,
		// FILTER excludes the row whose argument would fail.
		`SELECT a, sum(` + bad + `) FILTER (WHERE a<>7) OVER (ORDER BY a) FROM w WHERE a IS NOT NULL ORDER BY a`,
		`SELECT a, count(` + bad + `) FILTER (WHERE a<>7) OVER () FROM w WHERE a IS NOT NULL ORDER BY a`,
		// The FILTER itself can fail, and it is buffered too.
		`SELECT a, sum(d) FILTER (WHERE ` + bad + ` > 0) OVER () FROM w WHERE a IS NOT NULL ORDER BY a`,
		// Baseline: the argument IS reached, so both engines must fail.
		`SELECT a, sum(` + bad + `) OVER (ORDER BY a) FROM w WHERE a IS NOT NULL ORDER BY a`,
		// SQLITE_SUBTYPE: not buffered in C, so not lowered here -- both answer.
		`SELECT a, json_group_array(` + bad + `) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM w WHERE a IS NOT NULL ORDER BY a`,
		`SELECT a, json_group_object(b, ` + bad + `) OVER (ORDER BY a ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM w WHERE a IS NOT NULL ORDER BY a`,
	})
}

// TestG2WindowAggArgValues drives the VALUES through the lowered argument,
// separator and FILTER: the aggregate must accumulate exactly what it
// accumulated when it walked the expression itself, over every frame shape and
// every storage class the accumulators branch on.
func TestG2WindowAggArgValues(t *testing.T) {
	g2Parity(t, "agg-arg-values", []string{
		`SELECT b, sum(d*2) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(CASE WHEN a>2 THEN d ELSE 0 END) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, total(d/3.0) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, avg(a*1.0) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(nullif(a,3)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, min(b||'') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, max(CAST(a AS TEXT)) OVER (ORDER BY a) FROM w ORDER BY b`,
		// A NOCASE argument: min/max compare under the column's declared
		// collation, which the lowered read must not lose.
		`SELECT b, min(c) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, max(c) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, min(c COLLATE BINARY) OVER (ORDER BY a) FROM w ORDER BY b`,
	})

	g2Parity(t, "agg-sep-and-filter", []string{
		`SELECT b, group_concat(b) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(b, '-') OVER (ORDER BY a) FROM w ORDER BY b`,
		// The separator comes from the CURRENT row (func.c:2217), so a
		// per-row separator expression must vary with the row.
		`SELECT b, group_concat(b, CASE WHEN a>2 THEN '|' ELSE '-' END) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(b, CAST(a AS TEXT)) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, string_agg(b, '-') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, group_concat(DISTINCT c) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, sum(d) FILTER (WHERE a>2) OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(*) FILTER (WHERE c LIKE 'p%') OVER (ORDER BY a) FROM w ORDER BY b`,
		`SELECT b, count(d) FILTER (WHERE a IS NOT NULL) OVER () FROM w ORDER BY b`,
		`SELECT b, sum(DISTINCT d) OVER (ORDER BY a) FROM w ORDER BY b`,
	})

	g2Parity(t, "agg-arg-frames", []string{
		`SELECT b, sum(d+1) OVER (ORDER BY a ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d+1) OVER (ORDER BY a RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d+1) OVER (ORDER BY a GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM w ORDER BY b`,
		`SELECT b, sum(d+1) OVER (ORDER BY a ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, sum(d+1) OVER (ORDER BY a GROUPS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE GROUP) FROM w ORDER BY b`,
		`SELECT b, sum(d+1) OVER (ORDER BY a GROUPS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING EXCLUDE TIES) FROM w ORDER BY b`,
		`SELECT b, group_concat(b, '-') OVER (PARTITION BY a%2 ORDER BY a ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM w ORDER BY b`,
		`SELECT b, min(a+0) OVER (ORDER BY a ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM w ORDER BY b`,
	})
}

// TestG2CastDoesNotAliasItsRow pins the wrong answer this slice found, which is
// not about windows at all: OP_Cast rewrites its register IN PLACE
// (vdbe.c:2162-2173), and this engine pointed it at whatever register
// compileExpr returned. In a REGISTER-BACKED row scope -- a CHECK constraint, a
// generated column, an index expression, RETURNING, a trigger's NEW/OLD -- that
// register IS the row's column, so a cast destroyed the column for every later
// reader in the same expression.
//
// C never has the problem: TK_CAST codes its operand with sqlite3ExprCode
// (expr.c:5173), which guarantees a fresh target register (expr.c:5899-5924)
// before OP_Cast touches it.
//
// The generated-column case is the worst of the three: the wrong value is
// PERSISTED, and any index over it is built from the wrong value too.
func TestG2CastDoesNotAliasItsRow(t *testing.T) {
	driverParity(t, "cast-generated-column", []string{
		`CREATE TABLE gc(a TEXT, b AS (CAST(a AS INTEGER) || '-' || a))`,
		`INSERT INTO gc(a) VALUES('5x'),('12'),('abc')`,
		`SELECT a, b FROM gc ORDER BY a`,
		`CREATE TABLE gs(a TEXT, b TEXT AS (CAST(a AS INTEGER) || '/' || a) STORED)`,
		`INSERT INTO gs(a) VALUES('5x'),('12'),('abc')`,
		`SELECT a, b FROM gs ORDER BY a`,
		`CREATE INDEX gsi ON gs(b)`,
		`SELECT b FROM gs WHERE b='5/5x'`,
		`PRAGMA integrity_check`,
	})

	driverParity(t, "cast-check-constraint", []string{
		`CREATE TABLE ck(a TEXT, CHECK(CAST(a AS INTEGER)=5 AND a='5x'))`,
		`INSERT INTO ck VALUES('5x')`,
		`SELECT * FROM ck`,
		`INSERT INTO ck VALUES('6x')`,
		`SELECT count(*) FROM ck`,
	})

	driverParity(t, "cast-index-expression", []string{
		`CREATE TABLE ix(a TEXT)`,
		`INSERT INTO ix VALUES('5x'),('12'),('abc')`,
		`CREATE INDEX ixe ON ix(CAST(a AS INTEGER) || '@' || a)`,
		`SELECT a FROM ix WHERE CAST(a AS INTEGER) || '@' || a = '5@5x'`,
		`PRAGMA integrity_check`,
	})

	driverParity(t, "cast-returning-and-trigger", []string{
		`CREATE TABLE rt(a TEXT)`,
		`INSERT INTO rt VALUES('5x') RETURNING CAST(a AS INTEGER), a, CAST(a AS INTEGER) || '-' || a`,
		`CREATE TABLE log(v TEXT)`,
		`CREATE TRIGGER rtt AFTER INSERT ON rt BEGIN
			INSERT INTO log VALUES(CAST(new.a AS INTEGER) || '-' || new.a);
		 END`,
		`INSERT INTO rt VALUES('7y')`,
		`SELECT v FROM log`,
	})
}

// g2ParamParity runs one prepared statement over several argument sets on both
// drivers and compares, which driverParity's SQL-string form cannot express.
func g2ParamParity(t *testing.T, name, query string, argSets [][]any) {
	t.Helper()
	got := make([]string, len(argSets))
	for _, drv := range []string{"sqlite", "sqlite3"} {
		dsn := ":memory:"
		if drv == "sqlite" {
			dsn = filepath.Join(t.TempDir(), "g2p.sqlite")
		}
		db, err := sql.Open(drv, dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, s := range g2Fixture {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("[%s] %s: %v", drv, s, err)
			}
		}
		for i, args := range argSets {
			out := g2QueryArgs(db, query, args...)
			if drv == "sqlite" {
				got[i] = out
				continue
			}
			if got[i] != out {
				t.Errorf("[%s] args=%v %s\n  engine: %s\n  cgo:    %s", name, args, query, got[i], out)
			}
		}
		db.Close()
	}
}

func g2QueryArgs(db *sql.DB, query string, args ...any) string {
	rows, err := db.Query(query, args...)
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
