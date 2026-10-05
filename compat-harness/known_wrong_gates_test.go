// This file tests fixed wrong answers: recursive CTE dedup collation,
// pragma TVF through views, and recursive CTE dedup performance.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// collateSeedSchema holds tables with case variants and collations.
var collateSeedSchema = []string{
	"CREATE TABLE tb(k TEXT)",                // BINARY (declared nothing)
	"CREATE TABLE tn(k TEXT COLLATE NOCASE)", // NOCASE
	"CREATE TABLE tr(k TEXT COLLATE RTRIM)",  // RTRIM
	"INSERT INTO tb VALUES('a'),('A'),('b'),('B')",
	"INSERT INTO tn VALUES('a'),('A'),('b'),('B')",
	"INSERT INTO tr VALUES('a'),('a  '),('b'),('b ')",
	"CREATE TABLE edge(f TEXT COLLATE NOCASE, t TEXT COLLATE NOCASE)",
	"INSERT INTO edge VALUES('a','B'),('B','c'),('C','a'),('a','d')",
	"CREATE TABLE mixed(p TEXT, q TEXT COLLATE NOCASE)",
	"INSERT INTO mixed VALUES('x','p'),('x','P'),('X','p')",
	"CREATE TABLE ints(v INT)",
	"INSERT INTO ints VALUES(1),(1),(2)",
	"CREATE TABLE nulls(k TEXT COLLATE NOCASE)",
	"INSERT INTO nulls VALUES(NULL),(NULL),('a'),('A')",
	"CREATE TABLE accents(k TEXT COLLATE NOCASE)",
	"INSERT INTO accents VALUES('É'),('é'),('e'),('E')",
}

// TestRecursiveCTEUnionCollation tests recursive CTE dedup respects collation.
func TestRecursiveCTEUnionCollation(t *testing.T) {
	for _, q := range []string{
		// -- must dedup case-insensitively (declared NOCASE seed) --
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT s||'x' FROM c WHERE length(s)<3) SELECT s FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT s FROM c WHERE 0) SELECT hex(s) FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT upper(s) FROM c) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT s FROM c WHERE 0) SELECT count(*) FROM c",
		// ...through a CAST/unary-"+" chain, a derived table and a view, all of
		// which carry the declared collation in C SQLite.
		"WITH RECURSIVE c(s) AS (SELECT +k FROM tn UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT z FROM (SELECT k AS z FROM tn) UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k FROM vn UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		// -- an EXPLICIT COLLATE on a plain column does the same --
		"WITH RECURSIVE c(s) AS (SELECT k COLLATE NOCASE FROM tb UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k COLLATE NOCASE FROM tb UNION SELECT upper(s) FROM c) SELECT s FROM c ORDER BY 1",
		// -- RTRIM, the third built-in, dedups on trailing spaces --
		"WITH RECURSIVE c(s) AS (SELECT k FROM tr UNION SELECT s FROM c WHERE 0) SELECT hex(s) FROM c",
		// -- must NOT dedup: plain BINARY column --
		"WITH RECURSIVE c(s) AS (SELECT k FROM tb UNION SELECT s||'x' FROM c WHERE length(s)<3) SELECT s FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tb UNION SELECT upper(s) FROM c) SELECT s FROM c ORDER BY 1",
		// -- must NOT dedup: an EXPLICIT COLLATE BINARY defeats the declared one --
		"WITH RECURSIVE c(s) AS (SELECT k COLLATE BINARY FROM tn UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		// -- must NOT dedup AT ALL: UNION ALL, for either collation --
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION ALL SELECT s||'x' FROM c WHERE length(s)<3) SELECT s FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tr UNION ALL SELECT s FROM c WHERE 0) SELECT hex(s) FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k COLLATE NOCASE FROM tb UNION ALL SELECT s FROM c WHERE 0) SELECT s FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tb UNION SELECT upper(s) COLLATE NOCASE FROM c) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k||'' FROM tb UNION SELECT upper(s) COLLATE NOCASE FROM c) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k||'' FROM tb UNION SELECT k FROM tn,c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k||'' FROM tb UNION SELECT s FROM c WHERE 0 UNION SELECT k FROM tn,c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE c(u,v) AS (SELECT p,q FROM mixed UNION SELECT u,v FROM c WHERE 0) SELECT u,v FROM c ORDER BY 1,2",
		"WITH RECURSIVE c(v) AS (SELECT v FROM ints UNION SELECT v+1 FROM c WHERE v<4) SELECT v FROM c ORDER BY 1",
		"WITH RECURSIVE c(s) AS (SELECT k FROM nulls UNION SELECT s FROM c WHERE 0) SELECT count(*), count(s) FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM accents UNION SELECT s FROM c WHERE 0) SELECT s FROM c ORDER BY 1",
		"WITH RECURSIVE w(n) AS (SELECT 'A' COLLATE NOCASE UNION SELECT t FROM edge JOIN w ON f=n) SELECT n FROM w",
		"WITH RECURSIVE w(n) AS (SELECT 'A' COLLATE NOCASE UNION SELECT t FROM edge JOIN w ON f=n ORDER BY 1 DESC) SELECT n FROM w",
		"WITH RECURSIVE w(n) AS (SELECT 'A' COLLATE NOCASE UNION SELECT t FROM edge JOIN w ON f=n LIMIT 3) SELECT n FROM w",
		"WITH RECURSIVE w(n) AS (SELECT 'A' COLLATE NOCASE UNION SELECT t FROM edge JOIN w ON f=n LIMIT 2 OFFSET 1) SELECT n FROM w",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT s FROM c WHERE 0) SELECT s FROM c UNION ALL SELECT 'z' ORDER BY 1",
	} {
		stmts := append(append([]string(nil), collateSeedSchema...), "CREATE VIEW vn AS SELECT k FROM tn", q)
		if !differ(t, "recursivecollate", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestRecursiveCTEUnionAtScale tests recursive CTE dedup at scale with various collations.
func TestRecursiveCTEUnionAtScale(t *testing.T) {
	for _, q := range []string{
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x<2000) SELECT count(*), min(x), max(x), sum(x) FROM c",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT (x*7)%2003 FROM c WHERE x<>0) SELECT count(*), sum(x) FROM c",
		"WITH RECURSIVE c(x,y) AS (VALUES(1,'a') UNION SELECT x+1, char(97+(x%26)) FROM c WHERE x<1500) SELECT count(*), count(DISTINCT y) FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tn UNION SELECT s||'a' FROM c WHERE length(s)<9) SELECT count(*) FROM c",
		"WITH RECURSIVE c(s) AS (SELECT k FROM tb UNION SELECT s||'a' FROM c WHERE length(s)<9) SELECT count(*) FROM c",
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x<2000) SELECT count(*), sum(x) FROM c",
	} {
		stmts := append(append([]string(nil), collateSeedSchema...), q)
		if !differ(t, "recursivescale", stmts) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// pragmaTVFSetup creates objects in an ATTACH database for pragma TVF testing.
var pragmaTVFSetup = []string{
	"CREATE TABLE aux.at(a INTEGER PRIMARY KEY, b TEXT, c INT REFERENCES ap(x))",
	"CREATE TABLE aux.ap(x INTEGER PRIMARY KEY)",
	"CREATE INDEX aux.ai ON at(b,c)",
}

// TestPragmaTVFAcrossAttachedThroughDriver tests pragma TVFs through views
// and write paths with ATTACH databases.
func TestPragmaTVFAcrossAttachedThroughDriver(t *testing.T) {
	pureC, mattnC := crossConns(t, "pragmatvf")
	for i, s := range pragmaTVFSetup {
		execBothConn(t, pureC, mattnC, fmt.Sprintf("setup%d", i), s)
	}

	// The direct read spellings, as the baseline of the same rule.
	for _, q := range []string{
		"SELECT * FROM pragma_index_list('at') ORDER BY 1",
		"SELECT * FROM pragma_index_info('ai') ORDER BY 1",
		"SELECT * FROM pragma_table_info('at') ORDER BY 1",
		"SELECT * FROM pragma_table_xinfo('at') ORDER BY 1",
		"SELECT * FROM pragma_foreign_key_list('at') ORDER BY 1,2",
		"PRAGMA index_list('at')",
		"PRAGMA index_info('ai')",
		"PRAGMA table_info('at')",
		"PRAGMA table_xinfo('at')",
		"PRAGMA foreign_key_list('at')",
	} {
		queryBothConn(t, pureC, mattnC, "direct", q)
	}

	// Reached through a VIEW, and through a view over that view: the reading
	// statement's own text contains no "pragma_" at all.
	execBothConn(t, pureC, mattnC, "view", "CREATE VIEW v AS SELECT * FROM pragma_table_info('at')")
	execBothConn(t, pureC, mattnC, "viewview", "CREATE VIEW w AS SELECT * FROM v")
	queryBothConn(t, pureC, mattnC, "view", "SELECT count(*) FROM v")
	queryBothConn(t, pureC, mattnC, "viewview", "SELECT count(*) FROM w")
	queryBothConn(t, pureC, mattnC, "viewrows", "SELECT name FROM v ORDER BY 1")

	// The WRITE path, one destination table per shape so each is independently
	// visible in the read-back.
	writes := []struct{ label, ddl, write, read string }{
		{"insert-select",
			"CREATE TABLE g1(n)",
			"INSERT INTO g1 SELECT name FROM pragma_table_info('at')",
			"SELECT n FROM g1 ORDER BY 1"},
		{"insert-select-index",
			"CREATE TABLE g2(n)",
			"INSERT INTO g2 SELECT name FROM pragma_index_list('at')",
			"SELECT n FROM g2 ORDER BY 1"},
		{"insert-values-subquery",
			"CREATE TABLE g3(n)",
			"INSERT INTO g3 VALUES((SELECT count(*) FROM pragma_table_info('at')))",
			"SELECT n FROM g3"},
		{"insert-select-fk",
			"CREATE TABLE g4(n)",
			"INSERT INTO g4 SELECT count(*) FROM pragma_foreign_key_list('at')",
			"SELECT n FROM g4"},
		{"insert-through-view",
			"CREATE TABLE g5(n)",
			"INSERT INTO g5 SELECT name FROM v",
			"SELECT n FROM g5 ORDER BY 1"},
	}
	for _, w := range writes {
		execBothConn(t, pureC, mattnC, w.label+"/ddl", w.ddl)
		execBothConn(t, pureC, mattnC, w.label+"/write", w.write)
		queryBothConn(t, pureC, mattnC, w.label, w.read)
	}

	execBothConn(t, pureC, mattnC, "upd/ddl", "CREATE TABLE h(n)")
	execBothConn(t, pureC, mattnC, "upd/seed", "INSERT INTO h VALUES(0),(1),(2),(3)")
	execBothConn(t, pureC, mattnC, "upd/write", "UPDATE h SET n=n+(SELECT count(*) FROM pragma_table_info('at')) WHERE n=0")
	queryBothConn(t, pureC, mattnC, "upd", "SELECT n FROM h ORDER BY 1")
	execBothConn(t, pureC, mattnC, "del/write", "DELETE FROM h WHERE n IN (SELECT count(*) FROM pragma_table_info('at'))")
	queryBothConn(t, pureC, mattnC, "del", "SELECT n FROM h ORDER BY 1")

	execBothConn(t, pureC, mattnC, "trig/ddl", "CREATE TABLE fired(n)")
	execBothConn(t, pureC, mattnC, "trig/src", "CREATE TABLE src(m)")
	execBothConn(t, pureC, mattnC, "trig/create",
		"CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO fired SELECT count(*) FROM pragma_table_info('at'); END")
	execBothConn(t, pureC, mattnC, "trig/fire", "INSERT INTO src VALUES(1)")
	queryBothConn(t, pureC, mattnC, "trig", "SELECT n FROM fired")

	execBothConn(t, pureC, mattnC, "txn/ddl", "CREATE TABLE k(n)")
	execBothConn(t, pureC, mattnC, "txn/begin", "BEGIN")
	queryBothConn(t, pureC, mattnC, "txn/read", "SELECT count(*) FROM pragma_table_info('at')")
	queryBothConn(t, pureC, mattnC, "txn/view", "SELECT count(*) FROM v")
	execBothConn(t, pureC, mattnC, "txn/write", "INSERT INTO k SELECT name FROM pragma_table_info('at')")
	execBothConn(t, pureC, mattnC, "txn/commit", "COMMIT")
	queryBothConn(t, pureC, mattnC, "txn", "SELECT n FROM k ORDER BY 1")

	execBothConn(t, pureC, mattnC, "shadow/main", "CREATE TABLE at(zz)")
	queryBothConn(t, pureC, mattnC, "shadow", "SELECT count(*) FROM pragma_index_list('at')")
	queryBothConn(t, pureC, mattnC, "shadow-cols", "SELECT name FROM pragma_table_info('at') ORDER BY 1")
	queryBothConn(t, pureC, mattnC, "shadow-mainqual", "SELECT name FROM main.pragma_table_info('at') ORDER BY 1")
	queryBothConn(t, pureC, mattnC, "shadow-auxqual", "SELECT name FROM aux.pragma_table_info('at') ORDER BY 1")

	execBothConn(t, pureC, mattnC, "plain/ddl", "CREATE TABLE both(v)")
	execBothConn(t, pureC, mattnC, "plain/aux", "CREATE TABLE aux.both(v)")
	execBothConn(t, pureC, mattnC, "plain/mainrow", "INSERT INTO both VALUES('main')")
	execBothConn(t, pureC, mattnC, "plain/auxrow", "INSERT INTO aux.both VALUES('aux')")
	queryBothConn(t, pureC, mattnC, "plain-unqualified", "SELECT v FROM both")
	queryBothConn(t, pureC, mattnC, "plain-mainqual", "SELECT v FROM main.both")
	queryBothConn(t, pureC, mattnC, "plain-auxqual", "SELECT v FROM aux.both")
}
