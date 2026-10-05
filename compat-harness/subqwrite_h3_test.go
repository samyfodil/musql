// This file tests subqueries in INSERT VALUES and UPDATE SET statements.
// Subqueries in INSERT VALUES tuples and reads of the target table in UPDATE SET
// now compile to bytecode and must match C SQLite's answers.
package compat

import "testing"

// --- NOT promoted: UPDATE SET, any subquery over the target table -----------

// TestH3UpdateSetCorrelationBlindSpotsStayServed checks that UPDATE SET declines
// subqueries reading the target table, even when the correlation is hidden inside
// window functions, FILTER clauses, or table-valued functions.
func TestH3UpdateSetCorrelationBlindSpotsStayServed(t *testing.T) {
	differ(t, "H3 update set FILTER hides the correlation", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t AS o SET b=(SELECT count(*) FILTER (WHERE a<o.a) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "H3 update set OVER hides the correlation", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t AS o SET b=(SELECT count(*) OVER (PARTITION BY o.a) FROM t LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "H3 update set WINDOW clause hides the correlation", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t AS o SET b=(SELECT sum(a) OVER w FROM t WINDOW w AS (ORDER BY o.a) LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "H3 update set TVF argument hides the correlation", []string{
		`CREATE TABLE t(a TEXT, b)`,
		`CREATE TABLE u(p,q,r)`,
		`INSERT INTO t VALUES('u',0),('t',0)`,
		`UPDATE t AS o SET b=(SELECT count(*) FROM t, pragma_table_info(o.a))`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

func TestH3UpdateSetSubqueryOverTarget(t *testing.T) {
	// The canonical shape, and the one the ratchet names. Every row must get
	// the PRE-update count (3), not a count that shrinks as rows are written.
	differ(t, "H3 update set count over target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "H3 update set sum over target", []string{
		`CREATE TABLE t(x)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`UPDATE t SET x=(SELECT sum(x) FROM t)`,
		`SELECT x FROM t ORDER BY rowid`,
	})
	// WHERE filters: subquery sees all pre-update rows, not just matched ones.
	differ(t, "H3 update set over target with a filtering WHERE", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0),(4,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0) WHERE a>2`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// WHERE matches nothing: subquery never runs, nothing changes.
	differ(t, "H3 update set over target, WHERE matches nothing", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
		`UPDATE t SET b=(SELECT count(*) FROM t) WHERE a>99`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "H3 update set EXISTS/IN over target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t SET b=(SELECT EXISTS(SELECT 1 FROM t WHERE b IS NULL))`,
		`SELECT a,b FROM t ORDER BY a`,
		`UPDATE t SET b=(a IN (SELECT a FROM t WHERE b=0))`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Target alias: outer references bind to alias, inner "t" is the FROM item.
	differ(t, "H3 update set over target, aliased target", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
		`UPDATE t AS z SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Subquery inside CASE and IN: nested in non-top-level expressions.
	differ(t, "H3 update set over target inside CASE/IN", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t SET b=CASE WHEN a IN (SELECT a FROM t WHERE b>15) THEN 1 ELSE 0 END`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// SET and WHERE subqueries: both share the same snapshot.
	differ(t, "H3 update set over target plus a WHERE subquery", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE o(k)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`INSERT INTO o VALUES(2),(3)`,
		`UPDATE t SET b=(SELECT sum(b) FROM t) WHERE a IN (SELECT k FROM o)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Rowid moves: subquery counts the table in the same statement.
	differ(t, "H3 update set over target while the rowid moves", []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
		`UPDATE t SET k=k+10, b=(SELECT count(*) FROM t)`,
		`SELECT k,b FROM t ORDER BY k`,
		`PRAGMA integrity_check`,
	})
}

// TestH3UpdateSetSubqueryAffinityAndConstraints checks that UPDATE SET subqueries
// respect affinity, NOT NULL, CHECK, UNIQUE constraints, and generated columns.
func TestH3UpdateSetSubqueryAffinityAndConstraints(t *testing.T) {
	differ(t, "H3 update set over target, TEXT into INTEGER", []string{
		`CREATE TABLE t(a TEXT, b INTEGER)`,
		`INSERT INTO t VALUES('1',0),('2',0)`,
		`UPDATE t SET b=(SELECT group_concat(a) FROM t)`,
		`SELECT quote(a),quote(b) FROM t ORDER BY rowid`,
	})
	differ(t, "H3 update set over target, numeric text stays numeric", []string{
		`CREATE TABLE t(a, b INTEGER)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`UPDATE t SET b=(SELECT '007' FROM t LIMIT 1)`,
		`SELECT quote(b) FROM t ORDER BY rowid`,
	})
	// Empty subquery: NULL to NOT NULL column fails; nullable column stores NULL.
	differ(t, "H3 update set over target, empty subquery is NULL", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
		`UPDATE t SET b=(SELECT b FROM t WHERE a=99)`,
		`SELECT a,quote(b) FROM t ORDER BY a`,
	})
	differ(t, "H3 update set over target vs NOT NULL", []string{
		`CREATE TABLE t(a, b NOT NULL)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
		`UPDATE t SET b=(SELECT b FROM t WHERE a=99)`,
		`SELECT a,quote(b) FROM t ORDER BY a`,
	})
	// CHECK over generated column: must see the NEW value and roll back on failure.
	differ(t, "H3 update set over target vs generated column CHECK", []string{
		`CREATE TABLE t(a, b, c AS (b*2), CHECK(c<100))`,
		`INSERT INTO t(a,b) VALUES(1,1),(2,2)`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`UPDATE t SET b=(SELECT 500 FROM t LIMIT 1)`,
		`SELECT a,b,c FROM t ORDER BY a`,
	})
	// UNIQUE index: subquery's value collides for every row; statement aborts.
	differ(t, "H3 update set over target vs UNIQUE", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE UNIQUE INDEX ux ON t(b)`,
		`INSERT INTO t VALUES(1,10),(2,20)`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`SELECT a,b FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	})
	differ(t, "H3 update set over target, WITHOUT ROWID", []string{
		`CREATE TABLE t(k TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',0),('b',0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`SELECT k,b FROM t ORDER BY k`,
		`PRAGMA integrity_check`,
	})
	differ(t, "H3 update set over an unknown table", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,2)`,
		`UPDATE t SET b=(SELECT count(*) FROM nosuchtable)`,
		`SELECT a,b FROM t`,
	})
}

// TestH3UpdateSetSubqueryIsNotCached checks that UPDATE SET subqueries use a fresh
// snapshot on each execution, not a cached one from the first run.
func TestH3UpdateSetSubqueryIsNotCached(t *testing.T) {
	differ(t, "H3 update set subquery is not cached", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "H3 update set subquery inside a transaction", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,0),(2,0)`,
		`BEGIN`,
		`INSERT INTO t VALUES(3,0)`,
		`UPDATE t SET b=(SELECT count(*) FROM t)`,
		`COMMIT`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// --- promoted: a subquery inside an INSERT ... VALUES tuple -----------------

func TestH3InsertValuesSubquery(t *testing.T) {
	differ(t, "H3 insert values subquery over another table", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2)`,
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'x')`,
		`SELECT a,b FROM t`,
		`PRAGMA integrity_check`,
	})
	// The multi-row form is the one that pins OP_Once: both tuples read the
	// PRE-statement count, so both are 1 over a table that already held a row.
	differ(t, "H3 insert values subquery over the target, multi-row", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(9,'seed')`,
		`INSERT INTO t VALUES((SELECT count(*) FROM t),'p'),((SELECT count(*) FROM t),'q')`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	differ(t, "H3 insert values EXISTS and IN", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO t VALUES(EXISTS(SELECT 1 FROM s), 1 IN (SELECT x FROM s))`,
		`INSERT INTO t VALUES(NOT EXISTS(SELECT 1 FROM s), 9 IN (SELECT x FROM s))`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	differ(t, "H3 insert values empty subquery is NULL", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO t VALUES((SELECT x FROM s),'x')`,
		`SELECT quote(a),b FROM t`,
	})
	// A subquery yielding more than one row takes the first (SQLite rewrites
	// the body to LIMIT 1 -- "/* If there is no pre-existing limit add a limit of
	// 1 */ pLimit = sqlite3ExprInt32(pParse->db, 1);", expr.c:3952-3953), and
	// ORDER BY decides which.
	differ(t, "H3 insert values subquery takes the first row", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(3),(1),(2)`,
		`CREATE TABLE log(n)`,
		`INSERT INTO log VALUES((SELECT x FROM s))`,
		`INSERT INTO log VALUES((SELECT x FROM s ORDER BY x))`,
		`INSERT INTO log VALUES((SELECT x FROM s ORDER BY x DESC LIMIT 1))`,
		`SELECT n FROM log ORDER BY rowid`,
	})
	differ(t, "H3 insert values subquery over a join", []string{
		`CREATE TABLE s(x)`,
		`CREATE TABLE u(y)`,
		`INSERT INTO s VALUES(1),(2)`,
		`INSERT INTO u VALUES(2),(3)`,
		`CREATE TABLE log(n)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s JOIN u ON s.x=u.y))`,
		`SELECT n FROM log`,
	})
	// A multi-column subquery is a prepare-time error in C SQLite, and an
	// unresolvable name inside one is too.
	differ(t, "H3 insert values subquery errors", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x,y)`,
		`INSERT INTO t VALUES((SELECT x,y FROM s),'x')`,
		`INSERT INTO t VALUES((SELECT count(*) FROM nosuchtable),'x')`,
		`INSERT INTO t VALUES((SELECT nosuchcol FROM s),'x')`,
		`SELECT count(*) FROM t`,
	})
}

// TestH3InsertValuesSubqueryAffinityAndConstraints puts the subquery's value
// through the INSERT's own per-column machinery: declared affinity, a DEFAULT
// on a column the tuple does not name, NOT NULL, CHECK, and a UNIQUE conflict.
func TestH3InsertValuesSubqueryAffinityAndConstraints(t *testing.T) {
	differ(t, "H3 insert values subquery affinity plus DEFAULT", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES('7')`,
		`CREATE TABLE t(a INTEGER, b TEXT DEFAULT 'dflt', c)`,
		`INSERT INTO t(a,c) VALUES((SELECT x FROM s), (SELECT x||x FROM s))`,
		`SELECT quote(a),quote(b),quote(c) FROM t`,
	})
	differ(t, "H3 insert values subquery vs NOT NULL and CHECK", []string{
		`CREATE TABLE s(x)`,
		`CREATE TABLE t(a NOT NULL CHECK(a<5), b)`,
		`INSERT INTO t VALUES((SELECT x FROM s),'null-case')`,
		`INSERT INTO s VALUES(9)`,
		`INSERT INTO t VALUES((SELECT x FROM s),'check-case')`,
		`SELECT count(*) FROM t`,
	})
	differ(t, "H3 insert values subquery vs UNIQUE", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1)`,
		`CREATE TABLE t(a UNIQUE, b)`,
		`INSERT INTO t VALUES(1,'first')`,
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'dup')`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
	differ(t, "H3 insert values subquery with a conflict clause", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2)`,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT OR REPLACE INTO t VALUES(1,(SELECT count(*) FROM s))`,
		`SELECT a, quote(b) FROM t`,
		`INSERT INTO t VALUES(1,(SELECT count(*) FROM s)||'!') ON CONFLICT(a) DO UPDATE SET b=excluded.b||'?'`,
		`SELECT a, quote(b) FROM t`,
	})
	differ(t, "H3 insert values subquery, WITHOUT ROWID target", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2)`,
		`CREATE TABLE t(k TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO t VALUES('a',(SELECT count(*) FROM s))`,
		`SELECT k,b FROM t`,
		`PRAGMA integrity_check`,
	})
}

// TestH3InsertValuesSubqueryIsNotCached is the mutation target for
// Program.WritePager: the compiled program holds a snapshot, so it must never
// be reused for a later statement of the same text. Without the field the
// self-reading case reads the FIRST run's image forever and answers 0,1,1,1
// (the analyze6.test/count.test trap, named in cachedWriteProgram's own doc
// comment).
func TestH3InsertValuesSubqueryIsNotCached(t *testing.T) {
	differ(t, "H3 insert subquery is not cached", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(0)`,
		`INSERT INTO t VALUES((SELECT count(*) FROM t))`,
		`INSERT INTO t VALUES((SELECT count(*) FROM t))`,
		`INSERT INTO t VALUES((SELECT count(*) FROM t))`,
		`SELECT a FROM t ORDER BY rowid`,
	})
	differ(t, "H3 insert subquery over another table is not cached", []string{
		`CREATE TABLE s(x)`,
		`CREATE TABLE log(n)`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`INSERT INTO s VALUES(2)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`INSERT INTO s VALUES(3)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`SELECT n FROM log ORDER BY rowid`,
	})
	differ(t, "H3 insert subquery inside a transaction", []string{
		`CREATE TABLE s(x)`,
		`CREATE TABLE log(n)`,
		`BEGIN`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`INSERT INTO s VALUES(2)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`COMMIT`,
		`SELECT n FROM log ORDER BY rowid`,
	})
}

// TestH3InsertValuesSubqueryVsTriggers pins the instant the tuple is evaluated
// relative to the triggers the INSERT fires. The subquery is uncorrelated, so
// OP_Once caches it at the first tuple, before any trigger has run -- both rows
// read 0 even though the trigger inserts into the very table being counted, and
// that holds for a BEFORE and an AFTER trigger alike.
func TestH3InsertValuesSubqueryVsTriggers(t *testing.T) {
	differ(t, "H3 insert values subquery vs a BEFORE trigger", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO s VALUES(1); END`,
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'p'),((SELECT count(*) FROM s),'q')`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT count(*) FROM s`,
	})
	differ(t, "H3 insert values subquery vs an AFTER trigger", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO s VALUES(1); END`,
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'p'),((SELECT count(*) FROM s),'q')`,
		`SELECT a,b FROM t ORDER BY rowid`,
		`SELECT count(*) FROM s`,
	})
	// A trigger BODY whose own VALUES tuple holds a subquery: the body runs per
	// firing row and must see the LIVE table, not the firing statement's
	// snapshot. H3 declined that shape outright (programReadsFrozenSnapshot) and
	// this case is what made the decline observable rather than theoretical; batch K
	// PROMOTED it, and the answer here is unchanged -- which is the point of
	// keeping it. See compat-harness/live_read_k_test.go for the promotion's own
	// gate and engine/live_read_codegen_test.go for the half a differ() cannot
	// make.
	differ(t, "H3 trigger body VALUES subquery sees the live table", []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE log(n)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM t)); END`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT n FROM log ORDER BY rowid`,
	})
}

// TestH3InsertValuesSubqueryLeadingWith is the CTE half. A leading WITH in
// front of the VALUES form used to be parsed and DISCARDED, which only became
// observable once a tuple could hold a subquery -- and the discard was a WRONG
// ANSWER, not merely a missing feature, because a CTE unconditionally shadows a
// same-named table: selectExpander tries the WITH stack first ("}else if( (rc =
// resolveFromTermToCte(pParse, pWalker, pFrom))!=0 ){", select.c:6028) and only
// its else branch ("pFrom->pSTab = pTab = sqlite3LocateTableItem(pParse, 0,
// pFrom);", select.c:6036) reaches a real table. The shadowing case is the
// mutation target: without the push it answers the real table's 3 rows.
// TestH3InsertValuesSubqueryBlindSpots applies the SAME audit to the promotion
// that SURVIVED that killed the UPDATE one. The failure mode there was a
// correlated or otherwise unresolvable reference hiding in an AST position the
// compiler's own reasoning never visited, turning into a HARD error where the
// pre-promotion route had answered. So every position C SQLite's walk reaches and
// this engine's helpers historically did not -- FuncExpr.Filter (walker.c:33),
// FuncExpr.Over / SelectStmt.Windows (walker.c:88-90 with walkWindowList at
// walker.c:29-37, plus sqlite3WindowUpdate at resolve.c:1336),
// FromItem.TableFuncArgs (resolve.c:2012-2014) -- is exercised here, in both
// the resolvable and the escaping-to-the-INSERT-target spellings.
//
// It comes out clean, and the reason is structural rather than lucky: a
// top-level VALUES tuple has NO source row, so there is no outer scope for any
// of these to correlate TO. Every "reference the target" spelling below is
// unresolvable on the 3.53.3 oracle as well ("no such column: t.a"), so both
// engines fail the statement and insert nothing; the resolvable ones compile
// and answer. That is why this promotion is safe where the UPDATE one was not,
// and these cases pin it rather than leaving it as an argument.
func TestH3InsertValuesSubqueryBlindSpots(t *testing.T) {
	differ(t, "H3 insert values subquery with FILTER", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE log(v)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO log VALUES((SELECT count(*) FILTER (WHERE x>1) FROM s))`,
		`SELECT v FROM log`,
	})
	differ(t, "H3 insert values subquery with an inline OVER", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE log(v)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO log VALUES((SELECT sum(x) OVER () FROM s LIMIT 1))`,
		`SELECT v FROM log`,
	})
	differ(t, "H3 insert values subquery with a WINDOW clause", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE log(v)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO log VALUES((SELECT sum(x) OVER w FROM s WINDOW w AS (ORDER BY x) LIMIT 1))`,
		`SELECT v FROM log`,
	})
	differ(t, "H3 insert values subquery over a table-valued function", []string{
		`CREATE TABLE u(p,q,r)`, `CREATE TABLE log(v)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM pragma_table_info('u')))`,
		`SELECT v FROM log`,
	})
	// The three escaping spellings: each names the INSERT target from inside the
	// tuple's own subquery, in the plain, FILTER-hidden and TVF-argument
	// positions. None resolves on either engine -- a VALUES tuple has no row.
	differ(t, "H3 insert values subquery names the target directly", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE t(a,b)`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO t VALUES((SELECT count(*) FROM s WHERE s.x=t.a),'x')`,
		`SELECT a,b FROM t`,
	})
	differ(t, "H3 insert values subquery names the target through FILTER", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE t(a,b)`,
		`INSERT INTO s VALUES(1)`,
		`INSERT INTO t VALUES((SELECT count(*) FILTER (WHERE x=t.a) FROM s),'x')`,
		`SELECT a,b FROM t`,
	})
	differ(t, "H3 insert values subquery names the target in a TVF argument", []string{
		`CREATE TABLE u(p,q,r)`, `CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES((SELECT count(*) FROM pragma_table_info(t.a)),'x')`,
		`SELECT a,b FROM t`,
	})
	differ(t, "H3 insert values subquery nested inside another subquery", []string{
		`CREATE TABLE s(x)`, `CREATE TABLE log(v)`,
		`INSERT INTO s VALUES(1),(2),(3)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s WHERE x IN (SELECT x FROM s WHERE x>1)))`,
		`SELECT v FROM log`,
	})
}

func TestH3InsertValuesSubqueryLeadingWith(t *testing.T) {
	differ(t, "H3 insert CTE named by a VALUES subquery", []string{
		`CREATE TABLE log(x)`,
		`WITH cte AS (SELECT 5 AS x) INSERT INTO log VALUES((SELECT x FROM cte))`,
		`SELECT quote(x) FROM log`,
	})
	differ(t, "H3 insert CTE shadows the table", []string{
		`CREATE TABLE t(x)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`WITH t AS (SELECT 99 AS x) INSERT INTO log VALUES((SELECT count(*) FROM t))`,
		`SELECT quote(x) FROM log`,
	})
	differ(t, "H3 insert CTE with several members and a multi-row VALUES", []string{
		`CREATE TABLE log(a,b)`,
		`WITH one AS (SELECT 1 AS v), two AS (SELECT v*2 AS v FROM one)
		   INSERT INTO log VALUES((SELECT v FROM one),(SELECT v FROM two)),
		                         ((SELECT v FROM two),(SELECT v FROM one))`,
		`SELECT a,b FROM log ORDER BY rowid`,
	})
	// The CTE must NOT leak past the statement that declared it.
	differ(t, "H3 insert CTE does not outlive its statement", []string{
		`CREATE TABLE log(x)`,
		`WITH cte AS (SELECT 5 AS x) INSERT INTO log VALUES((SELECT x FROM cte))`,
		`INSERT INTO log VALUES((SELECT x FROM cte))`,
		`SELECT quote(x) FROM log`,
	})
}

// TestH3InsertValuesSubqueryTempCatalog pins that the tuple's subquery resolves
// through the same temp-first scope the rest of the write path uses: an
// unqualified name finds the TEMP table, and "main." reaches past it.
func TestH3InsertValuesSubqueryTempCatalog(t *testing.T) {
	differ(t, "H3 insert values subquery over a shadowed temp table", []string{
		`CREATE TABLE s(x)`,
		`CREATE TEMP TABLE s(x)`,
		`INSERT INTO main.s VALUES(1),(2),(3)`,
		`INSERT INTO temp.s VALUES(1)`,
		`CREATE TABLE log(x)`,
		`INSERT INTO log VALUES((SELECT count(*) FROM s))`,
		`INSERT INTO log VALUES((SELECT count(*) FROM main.s))`,
		`INSERT INTO log VALUES((SELECT count(*) FROM temp.s))`,
		`SELECT x FROM log ORDER BY rowid`,
	})
}
