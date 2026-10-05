package compat

// Tests SQLite query flattening observable behavior.
// (1) The PLAN. A FROM-clause subquery is merged into its parent before the
//     planner runs (flattenSubquery, select.c), so the parent's WHERE drives an
//     index on the BASE TABLE and rows arrive in that index's key order. This
//     engine materialized the derived table and scanned it in arrival order,
//     which is a wrong ROW ORDER -- invisible to the corpus, because a plain
//     SELECT has no decline channel. Three cells of the r36 shape space were
//     attributed to from=derived causally (resetting that one axis to a plain
//     table made them agree); the fixture and the cells below are theirs.
//
// (2) The LIMIT. select.c:4695 moves the subquery's LIMIT onto the parent, so
//     the bound applies AFTER the outer ORDER BY. Round 35 ported that for the
//     inline spelling only; the stored-VIEW spelling is here.
//
// Every case asserts THE ORACLE'S ANSWER through differ(), never "this
// declines" -- including the cases that must keep declining, where the oracle
// rejects too and the gate turns red the moment this engine starts answering
// one of them.

import "testing"

// r37cSchema is the r36 shape-space fixture: rowid order, ascending-a order and
// b order are three different permutations, a=1 appears as both INTEGER 1 and
// REAL 1.0 (so which of the two a tie resolves to is observable through
// typeof), and a=2 appears twice with different b.
var r37cSchema = []string{
	"CREATE TABLE t(a, b, c)",
	"INSERT INTO t VALUES(3,'x',10)",
	"INSERT INTO t VALUES(1,'Y',20)",
	"INSERT INTO t VALUES(2,'x',30)",
	"INSERT INTO t VALUES(2,'z',40)",
	"INSERT INTO t VALUES(NULL,'w',50)",
	"INSERT INTO t VALUES(1.0,'X',60)",
}

func r37cCase(t *testing.T, name string, setup []string, tail ...string) {
	t.Helper()
	stmts := append(append([]string(nil), r37cSchema...), setup...)
	differ(t, name, append(stmts, tail...))
}

// TestR37CFlattenTransparentDerived is the from=derived half. Each pair runs the
// same statement over a derived table and over the base table: C SQLite
// answers both identically BECAUSE it flattens, and this engine used to answer
// only the second one that way.
func TestR37CFlattenTransparentDerived(t *testing.T) {
	idx := []string{"CREATE INDEX i1 ON t(a,b,c)"}
	idxAB := []string{"CREATE INDEX i1 ON t(a,b)"}
	twoIdx := []string{"CREATE INDEX i1 ON t(a)", "CREATE INDEX i2 ON t(b)"}

	// schema=idx-cover from=derived where=in read=bare tail=order-a: the two
	// a=1 rows tie, and the index orders them by b ('X' before 'Y').
	r37cCase(t, "derived/in/order", idx,
		"SELECT a, b FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3) ORDER BY a")
	// tail=limit: which two rows survive is decided by the scan order alone.
	r37cCase(t, "derived/in/limit", idx,
		"SELECT a, typeof(a) FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3) LIMIT 2")
	// schema=idx-ab from=derived where=or read=minmax: min() keeps the FIRST
	// of two equal-comparing values, so the OR optimization's index order
	// decides whether it reports INTEGER 1 or REAL 1.0.
	r37cCase(t, "derived/or/minmax", idxAB,
		"SELECT min(a), max(a), typeof(min(a)) FROM (SELECT * FROM t) AS d WHERE a = 1 OR a = 3")
	// schema=idx-ab from=derived where=in read=quote tail=order-nulls-last.
	r37cCase(t, "derived/in/quote", idxAB,
		"SELECT quote(a), quote(b) FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3) ORDER BY a NULLS LAST")
	// An aggregate whose value depends on arrival order.
	//
	// NOTE the readout: "group_concat(b,'-')" ALONE. Adding "count(DISTINCT a)"
	// to the same select list makes BOTH the derived and the plain-table
	// spellings answer x-x-z where 3.53.3 answers x-z-x -- a wrong INDEX CHOICE
	// that has nothing to do with flattening and is not this file's to fix
	// (measured over the same two-index schema; see the r37c report).
	r37cCase(t, "derived/between/groupconcat", twoIdx,
		"SELECT group_concat(b,'-') FROM (SELECT * FROM t) AS d WHERE a BETWEEN 2 AND 4")
	// A join keeps its derived table flattened too.
	r37cCase(t, "derived/join", idx,
		"SELECT d.a, d.b FROM (SELECT * FROM t) AS d JOIN t AS u ON u.a = d.a WHERE d.a IN (1,2) ORDER BY d.a")
	// The unindexed baseline: nothing to flatten INTO, and it must not move.
	r37cCase(t, "derived/noidx", nil,
		"SELECT a, b FROM (SELECT * FROM t) AS d WHERE a IN (1,2,3)")
}

// TestR37CFlattenGuards pins the shapes the rewrite must NOT touch. Each is a
// case where SQLite resolves names BEFORE flattening and this engine flattens
// before resolving, so a rewrite would answer where the oracle rejects.
func TestR37CFlattenGuards(t *testing.T) {
	// A derived table has no rowid pseudo-column: "no such column: rowid" on
	// both engines. Flattening to "FROM t AS d" would make it resolve.
	r37cCase(t, "guard/rowid", nil,
		"SELECT rowid FROM (SELECT * FROM t) AS d")
	r37cCase(t, "guard/rowid-qualified", nil,
		"SELECT d.rowid, a FROM (SELECT * FROM t) AS d")
	r37cCase(t, "guard/rowid-in-where", nil,
		"SELECT a FROM (SELECT * FROM t) AS d WHERE rowid > 1")
	// The base table's own name is not in scope through a derived table.
	r37cCase(t, "guard/base-name", nil,
		"SELECT t.a FROM (SELECT * FROM t) AS d")
	// An UNALIASED derived table is unnameable, so the base name must stay
	// unnameable after the rewrite too.
	r37cCase(t, "guard/unaliased-base-name", nil,
		"SELECT t.a FROM (SELECT * FROM t)")
	// full_column_names=ON + short_column_names=OFF names the SUBQUERY's own
	// columns "t.a", so the outer bare "a" does not resolve -- because
	// sqlite3SelectExpand names them before flattenSubquery runs.
	r37cCase(t, "guard/full-column-names", []string{
		"PRAGMA full_column_names=ON", "PRAGMA short_column_names=OFF",
	}, "SELECT a FROM (SELECT * FROM t) AS d")
	// An OUTER join anywhere in the FROM clause blocks the rewrite, because
	// this engine's planner and SQLite's disagree about outer joins with no
	// derived table in them at all: over the same schema,
	// "SELECT * FROM t AS u LEFT JOIN t AS d ON u.a=99" scans u in rowid order
	// on 3.53.3 and walks the index on t(a,b,c) here -- an ON conjunct of a
	// LEFT JOIN naming only the LEFT table is not a filter on it. Flattening
	// into that plan would trade an agreeing answer for a wrong one; this case
	// is the one that catches it.
	r37cCase(t, "guard/left-join", []string{"CREATE INDEX i1 ON t(a,b,c)"},
		"SELECT * FROM t AS u LEFT JOIN (SELECT * FROM t) AS d ON u.a=99")
	// A body that is not transparent is not this rewrite's business.
	r37cCase(t, "guard/body-where", []string{"CREATE INDEX i1 ON t(a,b,c)"},
		"SELECT a, typeof(a) FROM (SELECT * FROM t WHERE a IS NOT NULL) AS d WHERE a IN (1,2,3) LIMIT 2")
	// A MATERIALIZED CTE is an optimization fence (select.c:7797): SQLite does
	// NOT flatten it, so its rows arrive in the body's own order. This engine
	// agrees today only because it materializes everything -- the gate exists
	// so that stays true when the CTE spelling is flattened.
	r37cCase(t, "guard/materialized-cte", []string{"CREATE INDEX i1 ON t(a,b,c)"},
		"WITH c AS MATERIALIZED (SELECT * FROM t) SELECT a, typeof(a) FROM c WHERE a IN (1,2,3) LIMIT 2")
}

// TestR37CFlattenView is the stored-VIEW spelling of both halves.
// sqlite3SelectExpand turns a view reference into exactly the FROM-clause
// subquery flattenSubquery then consumes, so the same two effects appear.
func TestR37CFlattenView(t *testing.T) {
	// (1) the plan: a view over the whole table is transparent.
	r37cCase(t, "view/in/limit", []string{
		"CREATE INDEX i1 ON t(a,b,c)", "CREATE VIEW v AS SELECT * FROM t",
	}, "SELECT a, typeof(a) FROM v WHERE a IN (1,2,3) LIMIT 2")
	r37cCase(t, "view/in/order", []string{
		"CREATE INDEX i1 ON t(a,b,c)", "CREATE VIEW v AS SELECT * FROM t",
	}, "SELECT a, b FROM v WHERE a IN (1,2,3) ORDER BY a")
	// (2) the LIMIT transfer: the view's LIMIT lands on the reading statement,
	// so the bound applies AFTER its ORDER BY.
	r37cCase(t, "view/limit-transfer", []string{
		"CREATE VIEW v AS SELECT a FROM t LIMIT 2",
	}, "SELECT * FROM v ORDER BY 1 DESC")
	r37cCase(t, "view/limit-transfer-expr", []string{
		"CREATE VIEW v AS SELECT a+0 AS z FROM t LIMIT 3",
	}, "SELECT * FROM v ORDER BY 1 DESC")
	// Blocked exactly as the inline spelling is: an outer WHERE (19), an outer
	// LIMIT (13), an outer DISTINCT (21), a subquery ORDER BY (11).
	r37cCase(t, "view/limit-outer-where", []string{
		"CREATE VIEW v AS SELECT a FROM t LIMIT 2",
	}, "SELECT * FROM v WHERE a IS NOT NULL ORDER BY 1 DESC")
	r37cCase(t, "view/limit-outer-limit", []string{
		"CREATE VIEW v AS SELECT a FROM t LIMIT 2",
	}, "SELECT * FROM v ORDER BY 1 DESC LIMIT 9")
	r37cCase(t, "view/limit-outer-distinct", []string{
		"CREATE VIEW v AS SELECT a FROM t LIMIT 2",
	}, "SELECT DISTINCT a FROM v ORDER BY 1 DESC")
	r37cCase(t, "view/limit-body-order", []string{
		"CREATE VIEW v AS SELECT a FROM t ORDER BY a LIMIT 2",
	}, "SELECT * FROM v ORDER BY 1 DESC")
	// A view has no rowid pseudo-column either.
	r37cCase(t, "view/rowid", []string{"CREATE VIEW v AS SELECT * FROM t"},
		"SELECT rowid FROM v")
	// A view's columns are named AFTER resolution (sqlite3ResultSetOfSelect),
	// which peels likely()/unlikely()/likelihood() off an unaliased item where
	// an inline subquery's naming does not. That is why the expansion leaves
	// such a body alone -- and it is also a naming this engine's VDBE view
	// source did not apply at all, reporting "unlikely(a)" through a SELECT and
	// "a" through PRAGMA table_info for the same view.
	r37cCase(t, "view/unlikely-naming", []string{
		"CREATE VIEW v AS SELECT unlikely(a) FROM t",
	}, "SELECT * FROM v LIMIT 1", "PRAGMA table_info(v)")
	r37cCase(t, "view/collate-naming", []string{
		"CREATE VIEW v AS SELECT b COLLATE nocase FROM t",
	}, "SELECT * FROM v LIMIT 1", "PRAGMA table_info(v)")
	// ... and it reports a rowid alias under its INTEGER PRIMARY KEY column's
	// declared name, which the same view body does NOT get inline.
	r37cCase(t, "view/rowid-alias-naming", []string{
		"CREATE TABLE k(id INTEGER PRIMARY KEY, v)",
		"INSERT INTO k VALUES(1,'a'),(2,'b'),(3,'c')",
		"CREATE VIEW vk AS SELECT oid FROM k",
	}, "SELECT * FROM vk LIMIT 1", "PRAGMA table_info(vk)")
}

// r37cCompound is the fixture for restriction (17): a compound FROM-clause
// subquery whose LIMIT moves onto the parent. t.a and u.b are both declared
// WITHOUT a type, so they share an affinity; w.c is TEXT and x.y is INTEGER, so
// each of those disagrees with t.a and (17h) blocks the transfer.
var r37cCompound = []string{
	"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2),(3),(4),(5)",
	"CREATE TABLE u(b)", "INSERT INTO u VALUES(10),(20)",
	"CREATE TABLE w(c TEXT)", "INSERT INTO w VALUES('p'),('q')",
	"CREATE TABLE x(y INTEGER)", "INSERT INTO x VALUES(7),(8)",
}

func r37cCompoundCase(t *testing.T, name string, tail ...string) {
	t.Helper()
	differ(t, name, append(append([]string(nil), r37cCompound...), tail...))
}

// TestR37CFlattenCompound is restriction (17): a COMPOUND FROM-clause subquery
// carrying the LIMIT. Its eight sub-conditions each get a case, because (17h)
// in particular is not the condition it looks like -- it compares raw
// sqlite3ExprAffinity values, where "not a column at all" is a value of its own
// that this engine's folded affinity cannot express.
func TestR37CFlattenCompound(t *testing.T) {
	// The transfer fires: both arms are untyped columns, so all eight hold.
	r37cCompoundCase(t, "compound/union-all",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/order-by-name",
		"SELECT a FROM (SELECT a FROM t UNION ALL SELECT b AS a FROM u LIMIT 3) ORDER BY a DESC")
	r37cCompoundCase(t, "compound/three-arms",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u UNION ALL SELECT b FROM u LIMIT 4) ORDER BY 1 DESC")
	// Neither arm is a column, so both are sqlite3ExprAffinity 0 -- equal.
	r37cCompoundCase(t, "compound/expr-arms",
		"SELECT * FROM (SELECT a+0 FROM t UNION ALL SELECT b+0 FROM u LIMIT 3) ORDER BY 1 DESC")

	// (17h): an untyped COLUMN is SQLITE_AFF_BLOB and a literal is 0, so these
	// two arms disagree and SQLite does NOT flatten -- the LIMIT stays inside
	// and the answer is the compound's first three rows, sorted.
	r37cCompoundCase(t, "compound/17h-literal",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT 'x' FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17h-text-column",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT c FROM w LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17h-integer-column",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT y FROM x LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17h-column-vs-expr",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b*2 FROM u LIMIT 3) ORDER BY 1 DESC")
	// (17a): only UNION ALL.
	r37cCompoundCase(t, "compound/17a-union",
		"SELECT * FROM (SELECT a FROM t UNION SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17a-except",
		"SELECT * FROM (SELECT a FROM t EXCEPT SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17a-intersect",
		"SELECT * FROM (SELECT a FROM t INTERSECT SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	// (17b): no arm may be aggregate or DISTINCT.
	r37cCompoundCase(t, "compound/17b-aggregate",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT max(b) FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17b-distinct",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT DISTINCT b FROM u LIMIT 3) ORDER BY 1 DESC")
	// (17c): every arm needs a FROM clause.
	r37cCompoundCase(t, "compound/17c-no-from",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT 99 LIMIT 3) ORDER BY 1 DESC")
	// (17d1)/(17d2): the outer query may not be aggregate or DISTINCT.
	r37cCompoundCase(t, "compound/17d-outer-aggregate",
		"SELECT max(a) FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/17d-outer-distinct",
		"SELECT DISTINCT a FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY 1 DESC")
	// (20): the compound subquery may not have its own ORDER BY.
	r37cCompoundCase(t, "compound/20-sub-order",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u ORDER BY 1 LIMIT 3) ORDER BY 1 DESC")
	// (18): every parent ORDER BY term must be a copy of a returned term.
	r37cCompoundCase(t, "compound/18-order-expr",
		"SELECT a FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY a+0 DESC")
	// (13)/(19)/(14): the LIMIT restrictions that gate every spelling.
	r37cCompoundCase(t, "compound/13-outer-limit",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) ORDER BY 1 DESC LIMIT 2")
	r37cCompoundCase(t, "compound/19-outer-where",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3) WHERE a > 0 ORDER BY 1 DESC")
	r37cCompoundCase(t, "compound/14-sub-offset",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3 OFFSET 1) ORDER BY 1 DESC")
	// No outer ORDER BY: the bound takes the same prefix either way.
	r37cCompoundCase(t, "compound/no-outer-order",
		"SELECT * FROM (SELECT a FROM t UNION ALL SELECT b FROM u LIMIT 3)")
}
