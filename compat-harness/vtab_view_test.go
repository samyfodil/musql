// This file gates VIEWs reading VIRTUAL TABLEs and schema-qualified references
// to vtabs, ensuring CREATE TABLE parsing recognizes vtab definitions.
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestViewOverVirtualTableMatchesCSQLite(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"view over rtree", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1)`,
			`INSERT INTO r VALUES(1, 0.0, 1.0)`,
			`INSERT INTO r VALUES(2, 5.0, 6.0)`,
			`CREATE VIEW v AS SELECT id, x0 FROM r`,
			`SELECT * FROM v ORDER BY id`,
			`SELECT * FROM v WHERE id=2`,
			`SELECT count(*) FROM v`,
			`SELECT * FROM main.r ORDER BY id`,
		}},
		{"view over fts4", []string{
			`CREATE VIRTUAL TABLE f USING fts4(a)`,
			`INSERT INTO f VALUES('hello world')`,
			`INSERT INTO f VALUES('goodbye')`,
			`CREATE VIEW v AS SELECT docid, a FROM f`,
			`SELECT * FROM v ORDER BY docid`,
			`SELECT * FROM v WHERE a LIKE 'hello%'`,
			`SELECT count(*) FROM v`,
			`SELECT docid FROM main.f ORDER BY docid`,
		}},
		{"view over fts4aux", []string{
			`CREATE VIRTUAL TABLE t1 USING fts4(a)`,
			`INSERT INTO t1 VALUES('one two three')`,
			`INSERT INTO t1 VALUES('two three four')`,
			`CREATE VIRTUAL TABLE terms USING fts4aux(t1)`,
			`CREATE VIEW terms_v AS SELECT term, documents, occurrences FROM terms WHERE col='*'`,
			`SELECT * FROM terms_v ORDER BY term`,
			`SELECT * FROM terms_v WHERE term='two'`,
			`SELECT count(*) FROM terms_v`,
			`SELECT * FROM main.terms WHERE col='*' ORDER BY term`,
		}},
		// json_each rather than generate_series: oracle's build doesn't ship it.
		{"view over an eponymous module still works", []string{
			`CREATE VIEW w AS SELECT key, value FROM json_each('[10,20]')`,
			`SELECT * FROM w`,
			`SELECT count(*) FROM w`,
		}},
		{"a view joining a virtual table to a real one", []string{
			`CREATE TABLE plain(id, label)`,
			`INSERT INTO plain VALUES(1,'one'),(2,'two')`,
			`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1)`,
			`INSERT INTO r VALUES(1, 0.0, 1.0)`,
			`CREATE VIEW v AS SELECT plain.label, r.x0 FROM plain JOIN r ON r.id=plain.id`,
			`SELECT * FROM v`,
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "vtabview/"+tc.name, tc.stmts) })
	}
}

// TestCrossDatabaseFts3Match pins MATCH over an fts table in an ATTACHed
// database with a same-named table in main. Ensures the right file is read.
func TestCrossDatabaseFts3Match(t *testing.T) {
	// ':memory:' rather than a path: each engine then attaches its OWN database,
	// so the identical statement text is a fair comparison (a shared file would
	// leave the second engine's CREATE hitting "already exists").
	setup := []string{
		`CREATE VIRTUAL TABLE t3 USING fts3(content)`,
		`INSERT INTO t3 (rowid, content) VALUES(1, 'hello world')`,
		`CREATE TABLE local(id, tag)`,
		`INSERT INTO local VALUES(2,'x'),(3,'y')`,
		`ATTACH ':memory:' AS two`,
		`CREATE VIRTUAL TABLE two.t3 USING fts3(content)`,
		`INSERT INTO two.t3 (rowid, content) VALUES(2, 'hello there')`,
		`INSERT INTO two.t3 (rowid, content) VALUES(3, 'cruel world')`,
	}
	differ(t, "crossdbmatch/single-source", append(append([]string{}, setup...),
		`SELECT rowid FROM two.t3 WHERE t3 MATCH 'hello'`,
		`SELECT rowid FROM two.t3 WHERE content MATCH 'cruel'`,
		`SELECT rowid FROM t3 WHERE t3 MATCH 'hello'`,
		`SELECT docid, quote(c0content) FROM t3_content ORDER BY docid`,
		`SELECT docid, quote(c0content) FROM two.t3_content ORDER BY docid`,
	))

	// ...and the JOINED form, where the statement runs against main while the
	// index it has to search is two's.
	differ(t, "crossdbmatch/joined", append(append([]string{}, setup...),
		`SELECT local.tag FROM local JOIN two.t3 ON t3.rowid=local.id WHERE t3 MATCH 'hello'`,
		`SELECT count(*) FROM two.t3, local WHERE t3 MATCH 'cruel'`,
		`SELECT rowid FROM two.t3 WHERE t3 MATCH 'hello'`,
	))
}

// TestVirtualTableInfoMatchesCSQLite gates PRAGMA table_info on VIRTUAL
// tables, ensuring vtab columns are reported correctly.
func TestVirtualTableInfoMatchesCSQLite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"fts3 and fts4", []string{
			`CREATE VIRTUAL TABLE f3 USING fts3(a, b)`,
			`CREATE VIRTUAL TABLE f4 USING fts4(x)`,
			`CREATE VIRTUAL TABLE f0 USING fts4`,
			`PRAGMA table_info(f3)`,
			`PRAGMA table_info(f4)`,
			`PRAGMA table_info(f0)`,
		}},
		{"rtree reports its own column types", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1, y0, y1)`,
			`CREATE VIRTUAL TABLE ri USING rtree_i32(id, x0, x1)`,
			`PRAGMA table_info(r)`,
			`PRAGMA table_info(ri)`,
		}},
		{"fts4aux reports its four visible columns", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`PRAGMA table_info(x)`,
		}},
		// A shadow table is an ORDINARY table and must keep answering as one.
		{"an fts shadow table is unaffected", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`PRAGMA table_info(t_content)`,
			`PRAGMA table_info(t_segdir)`,
			`PRAGMA table_xinfo(t_content)`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "vtabinfo/"+tc.name, tc.stmts) })
	}

	// table_xinfo on virtual tables reports columns as the module declares them.
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"table_xinfo fts3/fts4", []string{
			`CREATE VIRTUAL TABLE f3 USING fts3(a, b)`,
			`CREATE VIRTUAL TABLE f4 USING fts4(x)`,
			`CREATE VIRTUAL TABLE fl USING fts4(a, b, languageid=l)`,
			`PRAGMA table_xinfo(f3)`,
			`PRAGMA table_xinfo(f4)`,
			`PRAGMA table_xinfo(fl)`,
		}},
		{"table_xinfo rtree", []string{
			`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1)`,
			`CREATE VIRTUAL TABLE ri USING rtree_i32(id, x0, x1)`,
			`PRAGMA table_xinfo(r)`,
			`PRAGMA table_xinfo(ri)`,
		}},
		{"table_xinfo fts4aux and fts3tokenize", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`CREATE VIRTUAL TABLE k USING fts3tokenize('simple')`,
			`PRAGMA table_xinfo(x)`,
			`PRAGMA table_xinfo(k)`,
		}},
		// TVF spelling is also tested (rows are compared by the corpus).
		{"table_xinfo through the eponymous TVF", []string{
			`CREATE VIRTUAL TABLE f4 USING fts4(x)`,
			`CREATE VIRTUAL TABLE r USING rtree(id, x0, x1)`,
			`SELECT cid, name, type, hidden FROM pragma_table_xinfo('f4') ORDER BY cid`,
			`SELECT cid, name, type, hidden FROM pragma_table_xinfo('r') ORDER BY cid`,
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "vtabxinfo/"+tc.name, tc.stmts) })
	}
}
