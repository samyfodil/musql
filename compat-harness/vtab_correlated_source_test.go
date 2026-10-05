package compat

import "testing"

// Tests correlated virtual table sources whose constraints reference outer table rows.
// The module must be driven once per outer row to properly evaluate the constraint.
func TestVtabCorrelatedSource(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE tok USING fts3tokenize(simple)`,
		`CREATE TABLE c1(x)`,
		`INSERT INTO c1(x) VALUES('a b c')`,
		`INSERT INTO c1(x) VALUES('d e f')`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, j TEXT, n TEXT)`,
		`INSERT INTO t VALUES(1,'[1,2]','t'),(2,'[3]','t')`,
		`CREATE TABLE e(id INTEGER PRIMARY KEY, j TEXT)`,
		`INSERT INTO e VALUES(1,'[]'),(2,'[7]')`,
		// n holds JSON whose ELEMENTS are themselves JSON, for the chained case.
		`CREATE TABLE n(id INTEGER PRIMARY KEY, j TEXT)`,
		`INSERT INTO n VALUES(1,'["[10,11]","[12]"]'),(2,'["[13]"]')`,
		// mt stays EMPTY: the outer loop then produces no row at all, so the
		// correlated source's ONLY open never executes and its cursor is still
		// nil at the OpClose that ends the program.
		`CREATE TABLE mt(z)`,
		`CREATE TABLE kc(a TEXT COLLATE NOCASE)`,
		`INSERT INTO kc VALUES('A B'),('c d')`,
		`CREATE VIEW vw AS SELECT kc.a AS a, tok.token AS t FROM kc, tok WHERE input = a`,
		`CREATE TABLE fire(z)`,
		`CREATE TABLE dst(a,t)`,
		`CREATE TRIGGER trg AFTER INSERT ON fire BEGIN` +
			` INSERT INTO dst SELECT kc.a, tok.token FROM kc, tok WHERE input = kc.a; END`,
	}
	for _, q := range []string{
		// fts3tok1.test 1.13.2 and its neighbours.
		`SELECT * FROM c1, tok WHERE input = x AND c1.rowid=tok.rowid`,
		`SELECT * FROM c1, tok WHERE input = x`,
		`SELECT c1.rowid, tok.rowid, * FROM c1, tok WHERE input = x`,
		`SELECT x, token FROM c1, tok WHERE input = x ORDER BY x, token`,
		`SELECT count(*) FROM c1, tok WHERE input = x`,
		`SELECT x, count(token) FROM c1, tok WHERE input = x GROUP BY x ORDER BY x`,
		`SELECT x, min(token), max(token) FROM c1, tok WHERE input = x GROUP BY x ORDER BY x`,
		`SELECT * FROM c1, tok WHERE input = x LIMIT 4`,
		`SELECT * FROM (SELECT x, token FROM c1, tok WHERE input = x) ORDER BY x, token`,
		`WITH w AS (SELECT x, token FROM c1, tok WHERE input = x) SELECT * FROM w ORDER BY x, token`,
		`SELECT DISTINCT token FROM c1, tok WHERE input = x ORDER BY token`,
		`SELECT x, token, count(*) OVER (PARTITION BY x) FROM c1, tok WHERE input = x ORDER BY x, token`,
		`SELECT typeof(input), typeof(start) FROM c1, tok WHERE input = x LIMIT 1`,

		// json_each / json_tree / pragma_*: the same mechanism, other modules.
		`SELECT t.id, e2.value FROM t, json_each e2 WHERE e2.json = t.j`,
		`SELECT t.id, e2.rowid, e2.value FROM t, json_each e2 WHERE e2.json = t.j`,
		`SELECT t.id, e2.value FROM t, json_tree e2 WHERE e2.json = t.j`,
		`SELECT t.id, e2.key, e2.type, e2.fullkey, e2.path FROM t, json_each e2 WHERE e2.json = t.j`,
		`SELECT t.id, pt.cid, pt.name, pt.type FROM t, pragma_table_info pt WHERE pt.arg = t.n`,
		`SELECT count(*) FROM t, json_each e2 WHERE e2.json = t.j`,
		`SELECT t.id, e2.value FROM t, json_each e2 WHERE e2.json = t.j ORDER BY e2.value DESC`,
		`SELECT DISTINCT e2.value FROM t, json_each e2 WHERE e2.json = t.j ORDER BY 1`,
		`SELECT t.id, sum(e2.value) FROM t, json_each e2 WHERE e2.json = t.j GROUP BY t.id ORDER BY t.id`,
		`SELECT t.id, e2.value FROM t, json_each e2 WHERE e2.json = t.j LIMIT 2`,
		`SELECT * FROM (SELECT t.id AS i, e2.value AS v FROM t, json_each e2 WHERE e2.json = t.j)`,
		`WITH q AS (SELECT t.id AS i, e2.value AS v FROM t, json_each e2 WHERE e2.json = t.j) SELECT * FROM q`,

		// An outer row for which the module produces NOTHING: the empty array
		// must contribute no combination, which only holds if the module was
		// re-driven for it rather than reusing the previous row's set.
		`SELECT e.id, x.value FROM e, json_each x WHERE x.json = e.j`,
		`SELECT e.id, count(x.value) FROM e, json_each x WHERE x.json = e.j GROUP BY e.id ORDER BY e.id`,

		// An EMPTY outer table -- the open never runs and the cursor is never
		// created; nothing may touch it (AGENTS.md invariant 2).
		`SELECT * FROM mt, tok WHERE input = z`,
		`SELECT count(*) FROM mt, tok WHERE input = z AND mt.rowid=tok.rowid`,

		// A third table, on each side of the correlated one.
		`SELECT t.id, e2.value FROM t, t AS t2, json_each e2 WHERE e2.json = t.j AND t2.id = t.id`,
		`SELECT t.id, e2.value FROM t, json_each e2, t AS t2 WHERE e2.json = t.j AND t2.id = t.id`,
		// Two correlated sources over one outer row...
		`SELECT t.id, a.value, b.value FROM t, json_each a, json_each b WHERE a.json = t.j AND b.json = t.j`,
		// ...and CHAINED, the second correlated to the FIRST -- which only
		// works because each level's open reads cursors the levels outside it
		// have already positioned, and a correlated source is itself one of
		// them by the time the next level opens.
		`SELECT n.id, a.value, b.value FROM n, json_each a, json_each b WHERE a.json = n.j AND b.json = a.value`,
		`SELECT count(*) FROM n, json_each a, json_each b WHERE a.json = n.j AND b.json = a.value`,
		// A correlated source whose value is an EXPRESSION over the outer row,
		// not a bare column -- still one register, coded the same way.
		`SELECT t.id, e2.value FROM t, json_each e2 WHERE e2.json = '['||t.id||']'`,
		`SELECT * FROM c1, tok WHERE input = upper(x)`,
		`SELECT * FROM c1, tok WHERE input = x || 'g h'`,

		// The outer column carries a COLLATION, which the residual
		// "input = x" still applies (nothing was omitted here).
		`SELECT * FROM kc, tok WHERE input = a ORDER BY a, token`,
		// Inside a VIEW, and inside a TRIGGER BODY -- the two places the scan
		// is compiled somewhere other than the statement the user wrote. The
		// trigger case also exercises the channel a table-valued function's
		// argument uses to reach the firing row (derivedSource.vtabTrig),
		// which the correlated open carries unchanged.
		`SELECT * FROM vw ORDER BY a, t`,
		`INSERT INTO fire VALUES(1)`,
		`SELECT * FROM dst ORDER BY a, t`,
	} {
		differ(t, "vtab-correlated/"+q, append(append([]string{}, setup...), q))
	}
}

// TestVtabCorrelatedSourceStillDeclines is the boundary, recorded rather than
// left to be rediscovered: a correlated virtual-table source is answered only
// when THIS ENGINE'S loop order happens to bind the table it names first.
//
// That is a limitation of computeExecOrder (join.go), not of the mechanism.
// Its "min remaining need" placement puts a table that carries a filter of its
// own ahead of one that does not, so adding any single-table conjunct on the
// virtual table -- or simply writing it first in the FROM clause -- moves it to
// the outermost loop, where there is no outer row to read and the constraint is
// unusable again. C SQLite has no such gap: whereLoopAddVirtual asks the
// module about every loop order, and a module that rejects the unusable-input
// plan (json.c:5493, pragma.c:2904) leaves only the orders that work.
//
// Closing it means teaching computeExecOrder that such a source cannot be
// placed before the table it names -- a real change to the join order, which
// would also move the rows of every fts4aux/fts5vocab shape that tolerates an
// unusable constraint and is answered TODAY at the outermost position. That is
// why it is a separate piece of work and not a side effect of this one.
//
// Asserted directly rather than through differ(), which counts "one side
// errors" as a divergence and so cannot express a decline. It fails in BOTH
// directions on purpose -- see TestFts3TokenizeStillDeclines for the same
// reasoning.
func TestVtabCorrelatedSourceStillDeclines(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE tok USING fts3tokenize(simple)`,
		`CREATE TABLE c1(x)`,
		`INSERT INTO c1(x) VALUES('a b c')`,
		`INSERT INTO c1(x) VALUES('d e f')`,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, j TEXT, n TEXT)`,
		`INSERT INTO t VALUES(1,'[1,2]','t'),(2,'[3]','t')`,
		`CREATE TABLE nt(y)`,
		`INSERT INTO nt(y) VALUES(12)`,
	}
	for _, tc := range []struct {
		why string
		q   string
	}{
		{"the virtual table is written FIRST, so this engine binds it outermost",
			`SELECT t.id, v.value FROM json_each v, t WHERE v.json = t.j`},
		{"a filter of its own moves the virtual table to the outermost loop",
			`SELECT t.id, v.value FROM t, json_each v WHERE v.json = t.j AND v.value > 1`},
		{"a filter on any OTHER column of the virtual table does it too",
			`SELECT * FROM c1, tok WHERE input = x AND token = 'b'`},
		{"...including one on a column the module never consults",
			`SELECT * FROM c1, tok WHERE input = x AND tok.position = 1`},
		{"pragma_table_info written first",
			`SELECT t.id, pt.name FROM pragma_table_info pt, t WHERE pt.arg = t.n`},
		// Not a loop-order limit at all: a correlated right-hand side is never
		// a LITERAL, so vtabOmitConjuncts cannot claim it (vtab_omit.go) and
		// "input = x" stays in the residual WHERE. fts3tokenize reports its
		// "input" column as the TEXT RENDERING of the constrained value
		// (fts3_tokenize_vtab.c:388), which for a non-TEXT one can no longer
		// compare equal -- so it declines rather than drop a row, exactly as it
		// does for a non-TEXT literal on a path that missed the rewrite.
		{"a non-TEXT outer value, where the un-omitted conjunct would drop the row",
			`SELECT * FROM nt, tok WHERE input = y`},
	} {
		stmts := append(append([]string{}, setup...), tc.q)
		last := len(stmts) - 1
		cgo := run(t, "cgo", stmts)[last]
		mush := run(t, "musql", stmts)[last]
		rows, _ := cgo["rows"].([]any)
		if cgo["kind"] != "rows" || len(rows) == 0 {
			t.Errorf("[%s] %s: C SQLite no longer ANSWERS it (%v). The decline recorded here was\n"+
				"justified by C answering; re-settle the shape against the oracle.", tc.why, tc.q, cgo)
			continue
		}
		if mush["kind"] != "error" {
			t.Errorf("[%s] %s: musql now ANSWERS it (%v), where it declined before.\n"+
				"That may well be right -- but it needs its own oracle evidence and a case in\n"+
				"TestVtabCorrelatedSource, not a silently changed boundary.", tc.why, tc.q, mush)
		}
	}
}
