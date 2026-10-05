package compat

// This file gates write-path and query-shape constructs by comparing results
// against C SQLite using differ().

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// inValuesFixture is the test schema for IN (VALUES ...) tests.
var inValuesFixture = []string{
	`CREATE TABLE t1(a,b)`,
	`INSERT INTO t1 VALUES(1,2),(3,4),('abc','def')`,
	`CREATE TABLE tc(a TEXT COLLATE nocase, b)`,
	`INSERT INTO tc VALUES('ABC',2),('def',4)`,
}

// TestInValuesRightHandSide verifies that IN (VALUES ...) works for both
// scalar and row-value comparisons.
func TestInValuesRightHandSide(t *testing.T) {
	for _, q := range []string{
		// Scalar probe: membership over every tuple, and SQL's three-valued
		// NULL rules through the same evalIn path a "(SELECT ...)" takes.
		`SELECT 1 IN (VALUES(1),(2))`,
		`SELECT 5 IN (VALUES(1),(2))`,
		`SELECT 5 IN (VALUES(1),(NULL))`,
		`SELECT 1 IN (VALUES(1),(NULL))`,
		`SELECT 9 NOT IN (VALUES(1),(NULL))`,
		`SELECT 5 NOT IN (VALUES(1),(2))`,
		// Arity is still checked: a 1-column probe against a 2-column VALUES
		// is an error on both sides.
		`SELECT 1 IN (VALUES(1,2))`,
		// Row-value probe, in a select list and in a WHERE.
		`SELECT (a,b) IN (VALUES(1,2)) FROM t1`,
		`SELECT (a,b) NOT IN (VALUES(1,2)) FROM t1`,
		`SELECT * FROM t1 WHERE (a,b) IN (VALUES(1,2))`,
		`SELECT * FROM t1 WHERE (a,b) IN (VALUES(1,2),(3,4)) ORDER BY a`,
		`SELECT rowid FROM t1 WHERE (a,b) IN (VALUES('abc','def'),('ghi','JKL'))`,
		// A compound right-hand side is a select-statement like any other.
		`SELECT 1 IN (VALUES(1),(2) UNION SELECT 3)`,
		`SELECT 3 IN (VALUES(1),(2) UNION SELECT 3)`,
		`SELECT (a,b) IN (VALUES(1,2) UNION ALL VALUES(3,4)) FROM t1 ORDER BY 1`,
		// The COLLATION comes from the left operand, exactly as it does for the
		// "(SELECT ...)" spelling: the NOCASE column matches 'abc' where the
		// same comparison written between two literals does not.
		`SELECT a IN (VALUES('abc')) FROM tc ORDER BY 1`,
		`SELECT 'ABC' IN (VALUES('abc'))`,
		// Correlated to the outer row.
		`SELECT (SELECT 1 WHERE (tc.a,tc.b) IN (VALUES('ABC',2))) FROM tc ORDER BY 1`,
		// And on the write path's WHERE clause.
		`DELETE FROM t1 WHERE (a,b) IN (VALUES(3,4))`,
		`SELECT * FROM t1 ORDER BY 1`,
	} {
		if !differ(t, "invalues/"+q, append(append([]string(nil), inValuesFixture...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// insertValuesSubqueryFixture is the test schema for VALUES with subqueries.
var insertValuesSubqueryFixture = []string{
	`CREATE TABLE t(a)`,
	`INSERT INTO t VALUES(1),(2)`,
	`CREATE TABLE u(x)`,
	`CREATE TABLE q(k INTEGER PRIMARY KEY, v)`,
	`INSERT INTO q VALUES(1,'a')`,
	`CREATE TABLE lg(v)`,
}

// TestInsertValuesSubquery verifies that subqueries in VALUES use a single
// snapshot for all rows.
func TestInsertValuesSubquery(t *testing.T) {
	for _, stmts := range [][]string{
		// One snapshot for the whole multi-row statement.
		{`INSERT INTO t VALUES((SELECT count(*) FROM t)),((SELECT count(*) FROM t))`, `SELECT * FROM t ORDER BY rowid`},
		// The corpus's own idiom, plus the empty-subquery (NULL) and
		// too-many-columns cases.
		{`INSERT INTO u VALUES((SELECT max(a) FROM t)+1)`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((SELECT a FROM t))`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((SELECT a FROM t WHERE a=999))`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((SELECT a,a FROM t))`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((SELECT count(*) FROM nosuchtable))`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((SELECT 1 WHERE EXISTS(SELECT 1 FROM t)))`, `SELECT * FROM u`},
		{`INSERT INTO u VALUES((WITH RECURSIVE c(z) AS (SELECT 1 UNION ALL SELECT z+1 FROM c WHERE z<5) SELECT sum(z) FROM c))`, `SELECT * FROM u`},
		// Through a conflict clause, an upsert, and RETURNING.
		{`INSERT OR REPLACE INTO q VALUES((SELECT min(k) FROM q),'b')`, `SELECT * FROM q ORDER BY k`},
		{`INSERT INTO q VALUES((SELECT 1),'c') ON CONFLICT(k) DO UPDATE SET v='upserted'`, `SELECT * FROM q ORDER BY k`},
		{`INSERT INTO u VALUES((SELECT count(*) FROM t)) RETURNING x`, `SELECT * FROM u`},
		// Inside an open transaction, so the snapshot must include this
		// transaction's own uncommitted rows.
		{`BEGIN`, `INSERT INTO t VALUES(3)`, `INSERT INTO t VALUES((SELECT max(a) FROM t)+1)`, `COMMIT`, `SELECT * FROM t ORDER BY rowid`},
		// From a trigger body -- the shape that used to SEGFAULT (a nil
		// write-path pager reaching the read compiler), and still does not
		// inherit a snapshot from anywhere else because this trigger has no
		// WHEN clause.
		{`CREATE TRIGGER tg AFTER INSERT ON u BEGIN INSERT INTO lg VALUES((SELECT count(*) FROM t)); END`, `INSERT INTO u VALUES(77)`, `SELECT * FROM lg`},
		{`CREATE VIEW vw AS SELECT a FROM t`, `CREATE TRIGGER io INSTEAD OF INSERT ON vw BEGIN INSERT INTO lg VALUES((SELECT count(*) FROM t)); END`, `INSERT INTO vw VALUES(9)`, `SELECT * FROM lg`},
		// Reading an ATTACHed database, a WITHOUT ROWID target, a DEFAULT-
		// bearing target and a generated column.
		{`ATTACH ':memory:' AS aux`, `CREATE TABLE aux.z(p)`, `INSERT INTO aux.z VALUES(42)`, `INSERT INTO u VALUES((SELECT p FROM aux.z))`, `SELECT * FROM u`},
		{`CREATE TABLE w(k PRIMARY KEY, v) WITHOUT ROWID`, `INSERT INTO w VALUES((SELECT count(*) FROM t),'x')`, `SELECT * FROM w`},
		{`CREATE TABLE d(p DEFAULT 9, r)`, `INSERT INTO d(r) VALUES((SELECT count(*) FROM t))`, `SELECT * FROM d`},
		{`CREATE TABLE g(m, n GENERATED ALWAYS AS (m*2))`, `INSERT INTO g(m) VALUES((SELECT count(*) FROM t))`, `SELECT * FROM g`},
	} {
		full := append(append([]string(nil), insertValuesSubqueryFixture...), stmts...)
		if !differ(t, "insertvaluessub/"+stmts[0], full) {
			t.Errorf("diverged on: %v", stmts)
		}
	}
}

// TestInsertValuesSubqueryNondeterministic verifies that nondeterministic
// functions like random() work in VALUES tuples and store correct types.
func TestInsertValuesSubqueryNondeterministic(t *testing.T) {
	for _, tc := range []struct {
		insert, verify string
	}{
		{`INSERT INTO t VALUES((SELECT random()))`, `SELECT typeof(a) FROM t ORDER BY rowid`},
		{`INSERT INTO t VALUES(1+(SELECT random()))`, `SELECT typeof(a) FROM t ORDER BY rowid`},
		{`INSERT INTO t VALUES((SELECT randomblob(4)))`, `SELECT typeof(a), length(a) FROM t ORDER BY rowid`},
	} {
		stmts := append(append([]string(nil), insertValuesSubqueryFixture...), tc.insert)
		flLockstep(t, "insertvaluessub-nondet/"+tc.insert, stmts, tc.verify)
	}

	// The writable VIRTUAL TABLE target that stood in the declined list --
	// "INSERT INTO rt VALUES((SELECT count(*) FROM t),1.0,2.0)" -- is SERVED
	// and is checked in lockstep below instead. It was declined because that
	// path "evaluates its own VALUES tuple on a path that still installs no
	// snapshot"; compileVtabInsertStmt lowers the tuple into a register block,
	// so there is no tuple walk left to need one. Settled against the 3.53.3
	// oracle before it moved.

	// The promoted shape, in lockstep: a table-reading subquery in a writable
	// virtual table's VALUES tuple. Nondeterminism is what the list above is
	// really about, and count(*) has none, so this one is comparable.
	flLockstep(t, "vtab VALUES tuple with a table-reading subquery", []string{
		`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2)`,
		`CREATE VIRTUAL TABLE rt USING rtree(id, x0, x1)`,
		`INSERT INTO rt VALUES((SELECT count(*) FROM t),1.0,2.0)`,
	}, `SELECT id,x0,x1 FROM rt ORDER BY id`)
}

// TestPragmaDeclaredTypeDequoted verifies that PRAGMA table_info reports
// dequoted declared types.
func TestPragmaDeclaredTypeDequoted(t *testing.T) {
	fixture := []string{
		"CREATE TABLE q(a \"weird type\", b [brack et], c `back tick`, d 'sing le'," +
			" e \"has\"\"quote\", f \"INTEGER\", g \"\", h [], i \"a\" \"b\", j \"int\"," +
			" k [integer], l \"double  precision\", m VARCHAR (10), n int   unsigned," +
			" o \"txt\" (5), p 'always', r \"aaaaaaaaaalways\")",
	}
	for _, q := range []string{
		`PRAGMA table_info(q)`,
		`PRAGMA table_xinfo(q)`,
	} {
		if !differ(t, "decltype/"+q, append(append([]string(nil), fixture...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// tableInfoVtabFixture covers what PRAGMA table_info/table_xinfo report on: an
// IPK, a NOT NULL with a DEFAULT, a composite PRIMARY KEY, declared collations,
// no declared type at all, generated columns (which table_info excludes and
// table_xinfo includes), a WITHOUT ROWID table, a STRICT table, six view shapes
// and two virtual tables.
var tableInfoVtabFixture = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT NOT NULL DEFAULT 'z', c REAL)`,
	`CREATE TABLE t3(k, v, PRIMARY KEY(k,v))`,
	`CREATE TABLE tc(p TEXT collate nocase, q TEXT, r TEXT COLLATE RtRiM)`,
	`CREATE TABLE tnotype(u, w numeric, x varchar(9), y DoUbLe)`,
	`CREATE TABLE tg(m, n GENERATED ALWAYS AS (m*2), o AS (m*3) STORED)`,
	`CREATE TABLE hp(a, b, PRIMARY KEY(b,a))`,
	`CREATE TABLE wr(k PRIMARY KEY, v) WITHOUT ROWID`,
	`CREATE TABLE st(a INT, b TEXT NOT NULL) STRICT`,
	`CREATE VIEW v1 AS SELECT a, b, c FROM t1`,
	`CREATE VIEW vnamed(pp,qq) AS SELECT a,b FROM t1`,
	`CREATE VIEW vexpr AS SELECT a+1 AS e1, 'lit' AS e2, count(*) AS e3 FROM t1`,
	`CREATE VIEW vstar AS SELECT * FROM tnotype`,
	`CREATE VIEW vjoin AS SELECT t1.a, tc.p FROM t1, tc`,
	`CREATE VIEW vcast AS SELECT CAST(a AS NUMERIC) AS c1, CAST(a AS TEXT) AS c2 FROM t1`,
	`CREATE VIEW vdup AS SELECT a AS d1, a AS d2 FROM t1`,
	`CREATE VIRTUAL TABLE vrt USING rtree(id, x0, x1)`,
	`CREATE VIRTUAL TABLE vft USING fts4(aa, bb)`,
	`CREATE TEMP TABLE tt(z INTEGER, y TEXT DEFAULT 'q')`,
}

// TestPragmaTableInfoVtab gates pragma_table_info() and pragma_table_xinfo() as
// table-valued functions. Their rows were never row-compared before -- a bare
// PRAGMA runs on the corpus's exec side, where rows are discarded -- so wrapping
// them is what puts every shape below under the oracle for the first time.
func TestPragmaTableInfoVtab(t *testing.T) {
	for _, q := range []string{
		// Every table and view shape in the fixture.
		`SELECT * FROM pragma_table_info('t1')`,
		`SELECT * FROM pragma_table_info('t3')`,
		`SELECT * FROM pragma_table_info('tc')`,
		`SELECT * FROM pragma_table_info('tnotype')`,
		`SELECT * FROM pragma_table_info('tg')`,
		`SELECT * FROM pragma_table_info('hp')`,
		`SELECT * FROM pragma_table_info('wr')`,
		`SELECT * FROM pragma_table_info('st')`,
		`SELECT * FROM pragma_table_info('v1')`,
		`SELECT * FROM pragma_table_info('vnamed')`,
		`SELECT * FROM pragma_table_info('vexpr')`,
		`SELECT * FROM pragma_table_info('vstar')`,
		`SELECT * FROM pragma_table_info('vjoin')`,
		`SELECT * FROM pragma_table_info('vcast')`,
		`SELECT * FROM pragma_table_info('vdup')`,
		// A VIRTUAL TABLE target: table_info reports the module's non-hidden
		// declared columns, which rtree, fts3 and fts4 all already agree on.
		// (table_xinfo, which also reports the hidden ones, does NOT -- see
		// TestPragmaVtabDeclinesStayDeclined.)
		`SELECT * FROM pragma_table_info('vrt')`,
		`SELECT * FROM pragma_table_info('vft')`,
		// The schema catalog itself, and an fts4 shadow table.
		`SELECT * FROM pragma_table_info('sqlite_master')`,
		`SELECT * FROM pragma_table_info('vft_content')`,
		// The temp catalog: unqualified searches temp first, 'temp' scopes to it.
		`SELECT * FROM pragma_table_info('tt')`,
		`SELECT * FROM pragma_table_info('tt','temp')`,
		`SELECT * FROM pragma_table_info('t1','main')`,
		// table_xinfo adds the "hidden" column: 0 for an ordinary column, 2 for
		// a VIRTUAL generated column and 3 for a STORED one.
		`SELECT * FROM pragma_table_xinfo('t1')`,
		`SELECT * FROM pragma_table_xinfo('tg')`,
		`SELECT * FROM pragma_table_xinfo('wr')`,
		`SELECT * FROM pragma_table_xinfo('st')`,
		`SELECT * FROM pragma_table_xinfo('v1')`,
		`SELECT * FROM pragma_table_xinfo('vstar')`,
		`SELECT * FROM pragma_table_xinfo('sqlite_master')`,
		`SELECT * FROM pragma_table_xinfo('tt')`,
		// No argument, an explicit NULL and a name that does not exist are all
		// ZERO ROWS with the right column names, not an error.
		`SELECT * FROM pragma_table_info`,
		`SELECT count(*) FROM pragma_table_info`,
		`SELECT * FROM pragma_table_info(NULL)`,
		`SELECT * FROM pragma_table_info('nosuchtable')`,
		`SELECT * FROM pragma_table_xinfo('nosuchtable')`,
		// The hidden input columns behave as they do for every other wrapper.
		`SELECT arg, schema FROM pragma_table_info('t1','main')`,
		`SELECT typeof(arg), typeof(schema) FROM pragma_table_info('t1') LIMIT 1`,
		`SELECT * FROM pragma_table_info WHERE arg='t1'`,
		`SELECT * FROM pragma_table_info('t1','nosuchschema')`,
		// And the rows compose like any other row source.
		`SELECT name FROM pragma_table_info('t1') ORDER BY cid DESC`,
		`SELECT count(*), max(cid) FROM pragma_table_xinfo('tc')`,
		`SELECT DISTINCT type FROM pragma_table_info('t1') ORDER BY 1`,
		`SELECT (SELECT count(*) FROM pragma_table_info('t1'))`,
		`SELECT p.name, q.name FROM pragma_table_info('tc') AS p, pragma_table_info('t1') AS q ORDER BY p.cid, q.cid`,
		`SELECT count(*) FROM (SELECT name FROM pragma_table_info('tc'))`,
		`SELECT name FROM pragma_table_info('t1') WHERE name IN (SELECT name FROM pragma_table_info('v1'))`,
	} {
		if !differ(t, "tableinfovtab/"+q, append(append([]string(nil), tableInfoVtabFixture...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// parenAliasFixture is shared by the parenthesized-FROM-group alias cases.
var parenAliasFixture = []string{
	`CREATE TABLE t1(a,b)`,
	`CREATE TABLE t2(b,c)`,
	`CREATE TABLE t3(c,d)`,
	`INSERT INTO t1 VALUES(1,2),(3,4)`,
	`INSERT INTO t2 VALUES(2,5),(9,9)`,
	`INSERT INTO t3 VALUES(5,7)`,
}

// TestParenFromGroupAlias pins "( <one table-ref> ) [AS] alias" -- SQLite's
// "LP seltablist RP as" where the parenthesized list holds exactly one element,
// which parse.y implements by moving the alias onto that element rather than by
// building anything nested.
//
// The alias is not cosmetic and the OUTER one wins: verified directly against
// mattn/go-sqlite3, "SELECT x.b FROM (t2 AS y) AS x" answers the rows while
// "SELECT y.b FROM (t2 AS y) AS x" is "no such column: y.b".
func TestParenFromGroupAlias(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM (t2) AS x`,
		`SELECT * FROM (t2) x`,
		`SELECT * FROM ((t2)) AS x`,
		`SELECT x.b FROM (t2) AS x`,
		`SELECT * FROM (t2) AS x WHERE x.b=2`,
		`SELECT count(*) FROM (t2) AS x`,
		`SELECT * FROM (t2) AS x ORDER BY x.b DESC`,
		// The outer alias replaces an inner one.
		`SELECT * FROM (t2 AS y) AS x`,
		`SELECT x.b FROM (t2 AS y) AS x`,
		`SELECT y.b FROM (t2 AS y) AS x`,
		// Joined, where USING collapses the shared column so "SELECT *" has no
		// duplicate name.
		`SELECT * FROM t1 JOIN (t2) AS x USING (b)`,
		`SELECT x.c FROM t1 JOIN (t2) AS x USING (b)`,
		`SELECT * FROM (t1) NATURAL JOIN (t2) AS q`,
		// A table-valued function inside the parens is a one-element group too.
		`SELECT xyz.* FROM (JSON_EACH('{"a":1, "b":2}')) AS xyz`,
		// The alias must not swallow a following keyword.
		`SELECT * FROM (t1) NATURAL JOIN t2`,
		`SELECT * FROM (t1) INDEXED BY nosuchindex`,
		// A derived table's own alias is unaffected.
		`SELECT * FROM (SELECT 1 AS k) AS s`,
		// A qualified read through a two-element aliased group, where the
		// column read is NOT one of the colliding names -- see
		// TestParenFromGroupAliasStillDeclined's own doc comment for why this
		// one is servable even though the group also carries a genuine "b"
		// collision the group's OWN rebuilt-list census (checkOneRebuiltGroup)
		// never has to observe here: nothing in this statement's select list
		// is a "*" of any kind, so wave 13's "no star touches the group"
		// narrowing skips the census outright, and "j1.c" resolves through
		// the separate group-alias pass (resolveInScopes/resolveColumnEx)
		// unaffected by "b"'s ambiguity. Verified directly against
		// mattn/go-sqlite3: answers exactly one row, c=5.
		`SELECT j1.c FROM (t1 INNER JOIN t2 ON t1.b=t2.b) AS j1`,
	} {
		if !differ(t, "parenalias/"+q, append(append([]string(nil), parenAliasFixture...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestParenFromGroupAliasStillDeclined pins what is left of the boundary now
// that a parenthesized group of TWO OR MORE elements DOES take an alias
// (FromItem.GroupAlias; the answering half is
// compat-harness/parser_resolution_tail_test.go).
//
// This test used to assert that the whole multi-element shape was declined,
// on the premise that "SELECT * FROM (t1 INNER JOIN t2 ON t1.b=t2.b) AS j1" is
// an ERROR in C SQLite. That premise was measured and is FALSE as a
// statement about the shape: the error comes from t1.b and t2.b COLLIDING, and
// "SELECT * FROM (t1 CROSS JOIN t3) AS a1" -- same shape, no collision --
// answers a,b,c,d. What survives is the collision itself, which SQLite reports
// as "ambiguous column name: b" for the aliased group while the same group
// UNALIASED simply shows b twice; a qualified read through it is renamed
// ("b:1"), so neither the error nor the naming is reproducible here THROUGH A
// "*" -- but a qualified read of a NON-colliding column ("j1.c" -- see
// TestParenFromGroupAlias) never observes the rebuilt list at all and is
// correctly served since checkOneRebuiltGroup's narrowing ("no star touches
// the group" skips the census outright). What remains
// declined here is only the "*"-observing shape.
func TestParenFromGroupAliasAggregateNowServed(t *testing.T) {
	// The one shape left on the old decline list turned out to be waiting on
	// something else entirely: its own comment said so -- "count(*) declines for
	// an unrelated, still-live reason (aggregate over a parenthesized join group
	// has no VDBE codegen at all yet), not the 'b' collision". That reason is
	// gone (the member-scope offsets were the bug behind it, see
	// TestParenJoinGroupAliasAggregate), and count(*) never reads the colliding
	// name at all, so it agrees. The shape that still declines is the
	// "*"-observing one, pinned by TestParenJoinGroupAliasDeclines.
	for _, q := range []string{
		`SELECT count(*) FROM (t1 INNER JOIN t2 ON t1.b=t2.b) AS j1`,
		`SELECT count(*) FROM (t1 CROSS JOIN t3) AS a1`,
	} {
		stmts := append(append([]string(nil), parenAliasFixture...), q)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		last := len(stmts) - 1
		if c[last]["kind"] == "error" {
			t.Errorf("fixture drift: C SQLite REJECTS %q -- it no longer belongs in this list", q)
			continue
		}
		if fmt.Sprintf("%v", m[last]) != fmt.Sprintf("%v", c[last]) {
			t.Errorf("%q\n  cgo: %v\n  mus: %v", q, c[last], m[last])
		}
	}
}

// engineDirectRows runs stmts on an engine.DB and on a C SQLite connection
// -- the ENGINE-DIRECT pairing TestTCLCorpus uses, not the driver one differ()
// uses -- and returns the last statement's rows from each, as its own error text
// when it errored.
//
// The distinction matters for anything involving both a TEMP object and an
// ATTACHed database: the driver answers a cross-database read by opening a
// fresh reader over each file (crossDBQuery, driver/attach.go), and that
// reader has no temp objects at all, so a rule about the temp catalog cannot be
// observed through it. The engine-direct session holds both at once, which is
// the state the mined corpus replays in.
func engineDirectRows(t *testing.T, stmts []string) (goOut, cgoOut string) {
	t.Helper()
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer godb.Discard()
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	render := func(cols []string, rows [][]string, err error) string {
		if err != nil {
			return "ERROR"
		}
		out := fmt.Sprint(cols)
		sorted := append([][]string(nil), rows...)
		sort.Slice(sorted, func(i, j int) bool { return fmt.Sprint(sorted[i]) < fmt.Sprint(sorted[j]) })
		for _, r := range sorted {
			out += "\n  " + fmt.Sprint(r)
		}
		return out
	}
	for i, stmt := range stmts {
		last := i == len(stmts)-1
		if last {
			gc, gr, gerr, panicked, pv := tclSafeGoQuery(godb, stmt)
			if panicked {
				t.Fatalf("PANIC on %q: %v", stmt, pv)
			}
			cc, cr, cerr := tclRunCGOQuery(cgodb, stmt)
			return render(gc, gr, gerr), render(cc, cr, cerr)
		}
		if gerr, panicked, pv := tclSafeExecArgs(godb, stmt); panicked {
			t.Fatalf("PANIC on %q: %v", stmt, pv)
		} else if gerr != nil {
			t.Fatalf("setup %q failed on the engine: %v", stmt, gerr)
		}
		if _, cerr := cgodb.Exec(stmt); cerr != nil {
			t.Fatalf("setup %q failed on the oracle: %v", stmt, cerr)
		}
	}
	return "", ""
}

// attachedCatalogFixture is the mined shape that made this rule visible
// (fkey2.test / without_rowid3.test): main holds a TRIGGER and two tables, TEMP
// then shadows both of their names, and an ATTACHed database holds two more of
// its own. Every content-dependent rule schemaCatalogQueryGuard applies is then
// TRUE OF MAIN and false of aux.
var attachedCatalogFixture = []string{
	`CREATE TABLE t1(a PRIMARY KEY, b REFERENCES t1)`,
	`CREATE TABLE t2(a PRIMARY KEY, b REFERENCES t1, c REFERENCES t2)`,
	`CREATE TRIGGER tr AFTER DELETE ON t2 BEGIN SELECT 1; END`,
	`CREATE TEMP TABLE t1(a PRIMARY KEY)`,
	`CREATE TEMP TABLE t2(a, b)`,
	`ATTACH ':memory:' AS aux`,
	`CREATE TABLE aux.t1(a PRIMARY KEY)`,
	`CREATE TABLE aux.t2(a, b)`,
}

// TestAttachedCatalogRead pins "SELECT ... FROM aux.sqlite_master": an ATTACHED
// database's catalog is a different file's, so none of the reasons this engine
// cannot answer for its OWN catalog apply to it -- aux has its own triggers, its
// own virtual tables, and necessarily no temp objects at all, since TEMP always
// lands in the temp schema and never in an attachment.
func TestAttachedCatalogRead(t *testing.T) {
	for _, q := range []string{
		`SELECT sql FROM aux.sqlite_master WHERE type = 'table'`,
		`SELECT sql FROM aux.sqlite_master WHERE name='t2'`,
		`SELECT sql FROM aux.sqlite_schema WHERE type = 'table'`,
		`SELECT count(*) FROM aux.sqlite_master`,
		`SELECT name FROM aux.sqlite_master WHERE type='table' ORDER BY name`,
		// ...and, since round 34 retired schemaCatalogQueryGuard's trigger
		// clause, the LOCAL catalogs' own "sql" too -- main's and temp's, with
		// this fixture's trigger live. They were in the declined list below,
		// which is the shape of a decline-assertion outliving its decline; all
		// three agree with the oracle now.
		`SELECT sql FROM sqlite_master WHERE type = 'table'`,
		`SELECT sql FROM temp.sqlite_master WHERE type = 'table'`,
		`SELECT sql FROM sqlite_temp_master WHERE type = 'table'`,
	} {
		got, want := engineDirectRows(t, append(append([]string(nil), attachedCatalogFixture...), q))
		if got != want {
			t.Errorf("diverged on %s\n  cgo:\n%s\n  musql:\n%s", q, want, got)
		}
	}
}

// TestAttachedCatalogRootpageRead pins an attachment's rootpage column, directly
// and through "*". It used to be declined for every database, on the grounds
// that a writer rebuilding its page allocation on flush cannot reproduce it.
// Root page numbering is now reproduced and a diverged history is declined,
// so a decline is still allowed here but an answer must equal C SQLite's.
//
// The three "sql" reads that used to be here moved into TestAttachedCatalogRead
// above when round 34 retired schemaCatalogQueryGuard's trigger clause.
func TestAttachedCatalogRootpageRead(t *testing.T) {
	for _, q := range []string{
		`SELECT rootpage FROM aux.sqlite_master`,
		`SELECT * FROM aux.sqlite_master`,
	} {
		got, want := engineDirectRows(t, append(append([]string(nil), attachedCatalogFixture...), q))
		if want == "ERROR" {
			t.Errorf("fixture drift: C SQLite REJECTS %q -- it no longer belongs in this list", q)
			continue
		}
		if got != "ERROR" && got != want {
			t.Errorf("diverged on %s\n  cgo:\n%s\n  musql:\n%s", q, want, got)
		}
	}
}

// TestNoFromOrderByOrdinalRange pins a FROM-less SELECT's ORDER BY ordinal.
// Such a query yields at most one row, so the sort cannot reorder anything and
// this engine emitted no sort at all -- but C SQLite still RANGE-CHECKS the
// ordinal at prepare time and rejects it: verified directly against
// mattn/go-sqlite3, "SELECT 0 ORDER BY 456" is `1st ORDER BY term out of range
// - should be between 1 and 1`, and so are "ORDER BY 0" and "ORDER BY -1",
// while "SELECT 1,2 ORDER BY 2" is fine.
//
// It was reachable before only through spellings the corpus does not mine much;
// letting an INSERT's VALUES tuple hold a subquery put it on the write path,
// where fuzz.test stores a row C SQLite refuses.
func TestNoFromOrderByOrdinalRange(t *testing.T) {
	for _, q := range []string{
		`SELECT 0 ORDER BY 456`,
		`SELECT 1 ORDER BY 0`,
		`SELECT 1 ORDER BY -1`,
		`SELECT 1,2 ORDER BY 3`,
		// ...and the forms that must keep working.
		`SELECT 0 ORDER BY 1`,
		`SELECT 1,2 ORDER BY 2`,
		`SELECT 1 ORDER BY +1`,
		`SELECT 1 ORDER BY 1 COLLATE nocase`,
		`SELECT 1 ORDER BY 1+0`,
		// Through every wrapper that reaches a FROM-less body.
		`SELECT (SELECT 0 ORDER BY 456)`,
		`SELECT EXISTS(SELECT 1 ORDER BY 5)`,
		`SELECT * FROM (SELECT 0 ORDER BY 456)`,
		`SELECT 1 UNION SELECT 2 ORDER BY 3`,
		`SELECT 1 WHERE 0 ORDER BY 9`,
		// fuzz.test's own repro, which stored a row C SQLite refuses.
		`INSERT INTO t1 VALUES(CASE WHEN NULL THEN NULL ELSE (SELECT 0 ORDER BY 456) END)`,
		`SELECT * FROM t1`,
	} {
		if !differ(t, "nofromorder/"+q, []string{`CREATE TABLE t1(a)`, q}) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestOrderByOrdinalInt32MagnitudeOverflow pins fuzz.test#0's mined repro (a
// heavily-nested statement burying "ORDER BY -2147483649"): an ORDER BY term
// this engine's ordinal-recognition previously accepted as ANY integer
// literal is range-checked against the result-column count only when the
// literal's UN-NEGATED magnitude also fits a signed 32-bit int.
//
// C SQLite's ordinal detector, sqlite3ExprIsInteger (expr.c:2899), only
// ever reads a TK_INTEGER node's value at all when EP_IntValue is set -- and
// sqlite3ExprAlloc sets that flag "if op==TK_INTEGER and pToken points to a
// string that can be translated into a 32-bit integer" (expr.c:924-929,
// tag-20240227-a). A literal whose magnitude overflows int32 therefore never
// reads as an ordinal, even where the NEGATED value itself would fit int32
// (-2147483648 needs the literal token "2147483648", one past int32's
// positive top of 2147483647) -- it falls through to ordinary
// constant-expression handling in resolveOrderGroupBy (resolve.c:1826) and
// resolveCompoundOrderBy (resolve.c:1643) alike, both of which gate the
// out-of-range error on that same sqlite3ExprIsInteger call.
//
// Verified directly against mattn/go-sqlite3 (3.53.3):
//   - "SELECT 1 ORDER BY -2147483649" and "-2147483648" both SUCCEED (sort by
//     a constant, a no-op) -- magnitude overflows int32, not an ordinal.
//   - "SELECT 1 ORDER BY -2147483647" and "SELECT 1 ORDER BY 2147483648"
//     still ERROR / still SUCCEED respectively, pinning the exact int32
//     boundary (magnitude 2147483647 fits, 2147483648 does not).
//   - The identical rule holds for GROUP BY (shares resolveOrderGroupBy).
func TestOrderByOrdinalInt32MagnitudeOverflow(t *testing.T) {
	for _, q := range []string{
		// The exact boundary, both signed and bare.
		`SELECT 1 ORDER BY -2147483649`,
		`SELECT 1 ORDER BY -2147483648`,
		`SELECT 1 ORDER BY -2147483647`,
		`SELECT 1 ORDER BY 2147483647`,
		`SELECT 1 ORDER BY 2147483648`,
		`SELECT 1 ORDER BY +2147483648`,
		`SELECT 1,2 GROUP BY -2147483649`,
		`SELECT 1,2 GROUP BY -2147483648`,
		`SELECT 1,2 GROUP BY -2147483647`,
		// A compound SELECT's ORDER BY goes through the separate
		// resolveCompoundOrderBy path (resolve.c:1607) -- same int32 gate.
		`SELECT 1 UNION SELECT 2 ORDER BY -2147483649`,
		// fuzz.test#0's own mined repro, trimmed to its load-bearing shape:
		// an out-of-int32-range ORDER BY ordinal nested inside a scalar
		// subquery's LIMIT/OFFSET.
		`SELECT ( SELECT ALL -1 ORDER BY -2147483649 LIMIT 1 )`,
	} {
		if !differ(t, "orderby-int32-overflow/"+q, []string{`CREATE TABLE t1(x)`, q}) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
