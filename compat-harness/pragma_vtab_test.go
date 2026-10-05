package compat

// This file tests pragma table-valued functions (pragma_<name>(...) forms)
// compared against C SQLite.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// pragmaVtabFixture creates test tables covering various schema features.
var pragmaVtabFixture = []string{
	`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT NOT NULL DEFAULT 'z', c REAL)`,
	`CREATE TABLE t2(x, y, FOREIGN KEY(x) REFERENCES t1(a))`,
	`CREATE TABLE t3(k, v, PRIMARY KEY(k,v))`,
	`CREATE TABLE tc(p TEXT collate nocase, q TEXT, r TEXT COLLATE RtRiM)`,
	`CREATE INDEX i1 ON t1(b)`,
	`CREATE UNIQUE INDEX i3 ON t3(v)`,
	`CREATE INDEX ic1 ON tc(p)`,
	`CREATE INDEX ic2 ON tc(q COLLATE rtrim)`,
	`INSERT INTO t1 VALUES(1,'x',1.5)`,
	`CREATE VIEW v1 AS SELECT a, b, c FROM t1`,
	// The view shapes table_info reports on. tnotype's column has NO declared
	// type, which a view reports as "BLOB" and its own table reports as "".
	`CREATE TABLE tnotype(u, w numeric, x varchar(9), y DoUbLe)`,
	`CREATE VIEW vnamed(p,q) AS SELECT a,b FROM t1`,
	`CREATE VIEW vexpr AS SELECT a+1 AS e1, 'lit' AS e2, count(*) AS e3 FROM t1`,
	`CREATE VIEW vstar AS SELECT * FROM tnotype`,
	`CREATE VIEW vjoin AS SELECT t1.a, tc.p FROM t1, tc`,
	`CREATE VIEW vcast AS SELECT CAST(a AS NUMERIC) AS c1, CAST(a AS TEXT) AS c2, CAST(a AS REAL) AS c3, CAST(a AS BLOB) AS c4 FROM t1`,
	`CREATE VIEW vparen AS SELECT (a) AS pa, +a AS ua FROM t1`,
	`CREATE VIEW vdup AS SELECT a AS d1, a AS d2 FROM t1`,
	`CREATE VIEW vcompound AS SELECT a FROM t1 UNION ALL SELECT b FROM t1`,
}

func pragmaVtabCase(t *testing.T, q string) {
	t.Helper()
	if !differ(t, "pragmavtab/"+q, append(append([]string(nil), pragmaVtabFixture...), q)) {
		t.Errorf("diverged on: %s", q)
	}
}

func TestPragmaVtabMatchesCSQLite(t *testing.T) {
	for _, q := range []string{
		// One per wrapped pragma, in its call form.
		`SELECT * FROM pragma_index_list('t3')`,
		`SELECT * FROM pragma_index_info('i3')`,
		// index_xinfo is wrapped now: its bare form stopped being wrong for a
		// WITHOUT ROWID table's automatic index (engine's
		// withoutRowidAutoPKOwner), which is the condition this file's own
		// header set for wrapping it.
		`SELECT * FROM pragma_index_xinfo('i3')`,
		`SELECT * FROM pragma_index_xinfo('i1')`,
		`SELECT seqno, cid, name, "desc", coll, key FROM pragma_index_xinfo('i3') ORDER BY seqno`,
		`SELECT * FROM pragma_index_xinfo WHERE arg='i3'`,
		`SELECT * FROM pragma_foreign_key_list('t2')`,
		`SELECT * FROM pragma_user_version`,
		`SELECT * FROM pragma_encoding`,
		`SELECT * FROM pragma_integrity_check`,

		// The hidden input columns: echoed when named, and schema is NULL --
		// not 'main' -- when it was not supplied.
		`SELECT arg FROM pragma_index_list('t1')`,
		`SELECT schema FROM pragma_index_list('t1')`,
		`SELECT typeof(arg), typeof(schema) FROM pragma_index_list('t1') LIMIT 1`,
		`SELECT arg, schema FROM pragma_index_list('t1','main')`,

		// The WHERE spelling is equivalent to the call spelling.
		`SELECT * FROM pragma_index_list WHERE arg='t1'`,
		`SELECT * FROM pragma_index_list WHERE arg='t1' AND schema='main'`,
		`SELECT * FROM pragma_index_info WHERE arg='i3'`,

		// NO argument is ZERO ROWS with the right column names, not an error --
		// and so are an explicit NULL and a name that does not exist.
		`SELECT * FROM pragma_index_list`,
		`SELECT count(*) FROM pragma_index_list`,
		`SELECT * FROM pragma_index_list(NULL)`,
		`SELECT * FROM pragma_index_list('nosuchtable')`,
		`SELECT * FROM pragma_index_info('nosuchindex')`,
		`SELECT * FROM pragma_index_list('t2')`,       // exists, but has no indexes
		`SELECT * FROM pragma_foreign_key_list('t1')`, // exists, but has no FKs

		// An unattached schema qualifier is an error on both sides.
		`SELECT * FROM pragma_index_list('t1','nosuchschema')`,
		// An argument to a parameterless pragma is an error on both sides.
		`SELECT * FROM pragma_integrity_check(1)`,
		// An unwrapped/unknown name stays a mutual reject -- the boundary that
		// keeps a default: branch from silently banking every pragma name.
		`SELECT * FROM pragma_nosuchpragma`,

		// The rows behave like any other row source: ORDER BY, LIMIT, DISTINCT,
		// aggregates, GROUP BY, an alias, a projection, a cross join, and a
		// scalar subquery over one.
		`SELECT * FROM pragma_index_list('tc') ORDER BY seq DESC`,
		`SELECT * FROM pragma_index_list('tc') LIMIT 1`,
		`SELECT DISTINCT origin FROM pragma_index_list('t3') ORDER BY 1`,
		`SELECT count(*), max(seq) FROM pragma_index_list('tc')`,
		`SELECT origin, count(*) FROM pragma_index_list('t3') GROUP BY origin ORDER BY origin`,
		`SELECT z.name FROM pragma_index_list('tc') AS z ORDER BY z.seq`,
		`SELECT name FROM pragma_index_list('tc') WHERE seq=0`,
		`SELECT p.name, q.name FROM pragma_index_list('tc') AS p, pragma_index_list('t1') AS q ORDER BY p.seq`,
		`SELECT (SELECT count(*) FROM pragma_index_list('tc'))`,
		`SELECT count(*) FROM (SELECT name FROM pragma_index_list('tc'))`,
		`SELECT * FROM pragma_index_info('sqlite_autoindex_t3_1')`,

		// A VIEW target where the two engines DO agree: C SQLite answers zero
		// rows for index_list/foreign_key_list over a view, and so does this one.
		`SELECT * FROM pragma_index_list('v1')`,
		`SELECT * FROM pragma_foreign_key_list('v1')`,

		// A virtual table inside a SUBQUERY. subqueryScopes could not build a
		// schema-only scope for a vtab FROM item and reported "no such table" --
		// a HARD semantic error for a table that plainly exists, so it propagated
		// rather than declining. It affected EVERY vtab, not just a pragma one
		// (the long-standing generate_series had always failed the same way), and
		// the fix needs no derived-table machinery: a vtab's columns are DECLARED
		// by its module's own Connect. Every case below returns real ROWS rather
		// than agreeing on empty, which is the only way these assert anything.
		`SELECT name FROM pragma_index_list('t1') WHERE name IN (SELECT name FROM pragma_index_list('t1'))`,
		`SELECT count(*) FROM pragma_index_list('t1') WHERE EXISTS(SELECT 1 FROM pragma_index_list('t1'))`,
		`SELECT (SELECT count(*) FROM pragma_index_list('t1'))`,
		`SELECT (SELECT count(*) FROM pragma_index_info('i1'))`,
		`SELECT count(*) FROM pragma_index_list('tc') WHERE name NOT IN (SELECT name FROM pragma_index_list('t1'))`,
		`SELECT count(*) FROM pragma_index_list('t1') WHERE name IN (SELECT name FROM pragma_index_list('t1') UNION SELECT 'zz')`,
		// ... and a genuinely missing table in a subquery FROM must STILL be the
		// hard error it always was, not swept into the new vtab path.
		`SELECT * FROM pragma_index_list('t1') WHERE name IN (SELECT x FROM nosuchtable)`,
	} {
		pragmaVtabCase(t, q)
	}
}

// TestPragmaVtabTempSchema tests temp schema resolution in pragma_* functions.
func TestPragmaVtabTempSchema(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM pragma_index_list('tt')`,
		`SELECT * FROM pragma_index_list('tt','temp')`,
		`SELECT * FROM pragma_index_list('t1','main')`,
		`SELECT * FROM pragma_index_info('ti')`,
	} {
		stmts := append(append([]string(nil), pragmaVtabFixture...), `CREATE TEMP TABLE tt(q INTEGER, r TEXT)`, `CREATE INDEX ti ON tt(r)`, q)
		if !differ(t, "pragmavtabtemp/"+q, stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestPragmaVtabCorrelatedArgument tests pragma_* functions with correlated arguments.
func TestPragmaVtabCorrelatedArgument(t *testing.T) {
	for _, q := range []string{
		`SELECT t.name, p.name FROM sqlite_master AS t JOIN pragma_table_info(t.name) AS p ORDER BY 1,2`,
		`SELECT (SELECT count(*) FROM pragma_table_info(m.name)) FROM sqlite_master AS m ORDER BY 1`,
		// table_list's FULL listing was in the decline list below for the
		// same reason, and it is the sharpest check of the rtree work: it
		// reports every shadow table by name, types each "shadow" rather
		// than "table", and gives each virtual table an ncol that counts its
		// module's hidden columns.
		`SELECT schema, name, type, ncol, wr, strict FROM pragma_table_list ORDER BY schema, name`,
	} {
		stmts := append(append([]string(nil), pragmaVtabFixture...),
			`CREATE VIRTUAL TABLE vrt USING rtree(id, x0, x1)`,
			`CREATE VIRTUAL TABLE vft USING fts4(aa, bb)`, q)
		if !differ(t, "pragmavtabcorr/"+q, stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestPragmaVtabDeclinesStayDeclined verifies certain pragmas remain unimplemented.
func TestPragmaVtabDeclinesStayDeclined(t *testing.T) {
	for _, q := range []string{
		// table_info/table_xinfo ARE wrapped (see TestPragmaTableInfoVtab), and
		// both table_xinfo of a VIRTUAL TABLE and index_xinfo -- which used to
		// sit in this list -- are SERVED now, gated against the oracle in
		// TestVirtualTableInfoMatchesCSQLite and TestPragmaVtabMatchesCSQLite
		// respectively. What stays here is the correlated-argument family below.
		// Not implemented as bare pragmas either, so not wrappable yet.
		`SELECT * FROM pragma_page_count`,
		`SELECT * FROM pragma_freelist_count`,
		`SELECT * FROM pragma_collation_list`,
		// pragma_function_list is served now, row for row in both oracle
		// builds -- see pragma_function_list_test.go, and pragma_database_list
		// likewise -- see pragma_database_list_test.go, which gates its rows
		// against the oracle including the seq numbering and the file column.
	} {
		stmts := append(append([]string(nil), pragmaVtabFixture...),
			`CREATE VIRTUAL TABLE vrt USING rtree(id, x0, x1)`,
			`CREATE VIRTUAL TABLE vft USING fts4(aa, bb)`, q)
		c := run(t, "cgo", stmts)
		m := run(t, "musql", stmts)
		last := len(stmts) - 1
		if c[last]["kind"] == "error" {
			t.Errorf("fixture drift: C SQLite REJECTS %q -- it no longer belongs in this list", q)
			continue
		}
		if m[last]["kind"] != "error" {
			t.Errorf("engine ACCEPTED %q; it must decline until the behavior is built and gated", q)
		}
	}
}

// TestPragmaIntegrityCheckVtabTableArgument tests pragma_integrity_check and
// pragma_quick_check with various arguments.
func TestPragmaIntegrityCheckVtabTableArgument(t *testing.T) {
	setup := []string{`CREATE TABLE t1(a NOT NULL, b UNIQUE)`, `INSERT INTO t1 VALUES(1,2)`, `CREATE TABLE t2(x)`}
	for _, q := range []string{
		`SELECT * FROM pragma_integrity_check`,
		`SELECT * FROM pragma_integrity_check(1)`,
		`SELECT * FROM pragma_integrity_check('t1')`,
		`SELECT * FROM pragma_integrity_check('nosuch')`,
		`SELECT * FROM pragma_integrity_check(NULL)`,
		`SELECT * FROM pragma_quick_check`,
		`SELECT * FROM pragma_quick_check('t1')`,
		`SELECT * FROM pragma_integrity_check('t1','main')`,
		`SELECT * FROM pragma_integrity_check WHERE arg='t2'`,
		`SELECT integrity_check, arg, schema FROM pragma_integrity_check('t1')`,
	} {
		differ(t, q, append(append([]string(nil), setup...), q))
	}
}
