// This file tests PRAGMA trusted_schema=OFF enforcement against C SQLite.
// are unsafe (C SQLite exempts every BUILT-IN), so the entire surface is what
// a virtual table contributes -- MATCH, offsets/snippet/matchinfo/optimize,
// bm25/highlight, and the non-innocuous modules fts4aux/fts3tokenize/fts5vocab/
// pragma_*. Only VIEW bodies and TRIGGER bodies can reach any of it here, and a
// TEMP object is exempt outright.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	_ "github.com/samyfodil/musql/driver"
)

// tsPair is one musql connection and one cgo connection over separate files,
// each pinned to a SINGLE logical connection because trusted_schema is
// per-connection state that must span statements.
type tsPair struct {
	dir  string
	mush *sql.DB
	cgo  *sql.DB
}

func tsOpenPair(t *testing.T) *tsPair {
	t.Helper()
	p := &tsPair{dir: t.TempDir()}
	p.open(t)
	return p
}

func (p *tsPair) open(t *testing.T) {
	t.Helper()
	mush, err := sql.Open("sqlite", filepath.Join(p.dir, "go.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { mush.Close() })
	mush.SetMaxOpenConns(1)
	cgo, err := sql.Open("sqlite3", filepath.Join(p.dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { cgo.Close() })
	cgo.SetMaxOpenConns(1)
	p.mush, p.cgo = mush, cgo
}

// reopen drops both connections and opens fresh ones over the SAME files. It
// exists for one rule: C SQLite's PRAGMA table_info over a view answers from
// the in-memory schema's cached column list once anything on that CONNECTION
// has resolved the view's body, so "has this connection touched the view yet"
// is load-bearing there (see TestTrustedSchemaTableInfoFreshConnection).
func (p *tsPair) reopen(t *testing.T) {
	t.Helper()
	p.mush.Close()
	p.cgo.Close()
	p.open(t)
}

// setup runs statements that MUST succeed on both engines. A gate whose setup
// quietly failed on one side passes vacuously, so this never tolerates an
// error, on either side.
func (p *tsPair) setup(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := p.mush.Exec(s); err != nil {
			t.Fatalf("setup %q: musql: %v", s, err)
		}
		if _, err := p.cgo.Exec(s); err != nil {
			t.Fatalf("setup %q: oracle: %v", s, err)
		}
	}
}

// bothReject requires that BOTH engines refuse s, and that musql's wording
// carries the oracle's own message (which is the whole point of implementing
// the rule rather than declining it: "unsafe use of MATCH()", not a generic
// gap). wantOracle is asserted against the ORACLE, live, so the premise of the
// rule fails loudly if C SQLite ever changes.
func (p *tsPair) bothReject(t *testing.T, s, wantOracle string) {
	t.Helper()
	cerr := tsRun(p.cgo, s)
	if cerr == nil {
		t.Fatalf("%q: the ORACLE accepted it -- this rule's premise no longer holds", s)
	}
	if !strings.Contains(cerr.Error(), wantOracle) {
		t.Fatalf("%q: oracle refused with %q, expected it to contain %q", s, cerr, wantOracle)
	}
	merr := tsRun(p.mush, s)
	if merr == nil {
		t.Fatalf("%q: musql ACCEPTED where the oracle refused with %v", s, cerr)
	}
	if !strings.Contains(merr.Error(), wantOracle) {
		t.Fatalf("%q: musql refused with %q, expected the oracle's own %q", s, merr, wantOracle)
	}
}

// tsRun runs s on whichever of Query/Exec the driver requires for its
// statement kind (it refuses Exec on a row-returning statement) and reports
// only whether it failed. A PRAGMA goes through Query so a getter's rows are
// consumed rather than discarded, which is also where its errors surface.
func tsRun(db *sql.DB, s string) error {
	if tclIsQuery(s) || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "PRAGMA") {
		_, _, err := tclRunCGOQuery(db, s)
		return err
	}
	_, err := db.Exec(s)
	return err
}

// bothAccept requires that both engines run s and return identical cells. This
// is the half that catches OVER-enforcement -- refusing what the oracle allows
// is just as wrong as the reverse.
func (p *tsPair) bothAccept(t *testing.T, s string) {
	t.Helper()
	gCols, gRows, gerr := tclRunCGOQuery(p.mush, s)
	cCols, cRows, cerr := tclRunCGOQuery(p.cgo, s)
	if cerr != nil {
		t.Fatalf("%q: the ORACLE refused it: %v", s, cerr)
	}
	if gerr != nil {
		t.Fatalf("%q: musql refused where the oracle answered: %v", s, gerr)
	}
	if ok, reason := queryResultsMatch(gCols, gRows, cCols, cRows, false); !ok {
		t.Fatalf("%q: %s\n  go:  cols=%v rows=%v\n  cgo: cols=%v rows=%v", s, reason, gCols, gRows, cCols, cRows)
	}
}

// tsBaseSchema is the shared fixture: one ordinary table, one fts4 table, one
// rtree table and the two non-innocuous modules, plus a view per shape the rule
// governs. Every CREATE here succeeds on BOTH engines with the flag ON -- that
// is asserted, not assumed, and it is what makes the OFF half meaningful.
func tsBaseSchema() []string {
	return []string{
		`CREATE TABLE base(a, b, c)`,
		`INSERT INTO base VALUES(1, 'x', '[1,2]')`,
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`INSERT INTO ft VALUES('hello world')`,
		`CREATE VIRTUAL TABLE rt USING rtree(id, x0, x1)`,
		`INSERT INTO rt VALUES(1, 0.0, 1.0)`,
		`CREATE VIRTUAL TABLE ftaux USING fts4aux(ft)`,
		`CREATE VIRTUAL TABLE tok USING fts3tokenize('simple')`,

		// --- views whose bodies are UNSAFE with the flag off ---
		`CREATE VIEW v_match     AS SELECT * FROM ft WHERE x MATCH 'hello'`,
		`CREATE VIEW v_offsets   AS SELECT offsets(ft) AS q FROM ft WHERE x MATCH 'hello'`,
		`CREATE VIEW v_snippet   AS SELECT snippet(ft) AS q FROM ft WHERE x MATCH 'hello'`,
		`CREATE VIEW v_matchinfo AS SELECT quote(matchinfo(ft)) AS q FROM ft WHERE x MATCH 'hello'`,
		`CREATE VIEW v_fts4aux   AS SELECT count(*) AS q FROM ftaux`,
		`CREATE VIEW v_tokenize  AS SELECT count(*) AS q FROM tok WHERE input = 'a b'`,
		`CREATE VIEW v_pragmavtab AS SELECT count(*) AS q FROM pragma_table_info('base')`,
		// ...and the PAREN-LESS eponymous spelling, which C SQLite resolves
		// only after the table lookup fails and refuses just the same.
		`CREATE VIEW v_bareeponym AS SELECT count(*) AS q FROM pragma_table_info`,
		`CREATE VIEW v_bareargs   AS SELECT count(*) AS q FROM pragma_table_info WHERE arg = 'base'`,
		// ...reached only through NESTING, a CTE, a scalar subquery and a
		// never-taken CASE branch: C SQLite checks at name-resolution time,
		// so the CASE arm fails even though it can never be evaluated.
		`CREATE VIEW v_nested AS SELECT count(*) AS q FROM v_match`,
		`CREATE VIEW v_cte    AS WITH z AS (SELECT * FROM ft WHERE x MATCH 'hello') SELECT count(*) AS q FROM z`,
		`CREATE VIEW v_subq   AS SELECT (SELECT count(*) FROM ft WHERE x MATCH 'hello') AS q`,
		`CREATE VIEW v_case   AS SELECT CASE WHEN 0 THEN (SELECT count(*) FROM ft WHERE x MATCH 'a') ELSE 1 END AS q`,
		`CREATE VIEW v_deriv  AS SELECT count(*) AS q FROM (SELECT * FROM ft WHERE x MATCH 'hello')`,

		// --- views that stay INNOCUOUS with the flag off ---
		`CREATE VIEW v_scan   AS SELECT count(*) AS q FROM ft`,
		`CREATE VIEW v_docid  AS SELECT docid AS q FROM ft`,
		`CREATE VIEW v_like   AS SELECT count(*) AS q FROM base WHERE b LIKE 'x%'`,
		`CREATE VIEW v_json   AS SELECT json_extract(c, '$[0]') AS q FROM base`,
		`CREATE VIEW v_jeach  AS SELECT count(*) AS q FROM json_each('[1,2]')`,
		`CREATE VIEW v_jtree  AS SELECT count(*) AS q FROM json_tree('[1,2]')`,
		`CREATE VIEW v_rtree  AS SELECT count(*) AS q FROM rt`,
		`CREATE VIEW v_plain  AS SELECT a + 1 AS q FROM base`,
		// A CTE that happens to share an eponymous module's name SHADOWS it, so
		// this view is innocuous even though "pragma_table_info" appears in its
		// FROM -- an over-fire here would refuse what the oracle allows.
		`CREATE VIEW v_cteshadow AS WITH pragma_table_info(q) AS (SELECT 7) SELECT q FROM pragma_table_info`,

		// --- the TEMP exemption, both object kinds ---
		`CREATE TEMP VIEW tv_match AS SELECT count(*) AS q FROM ft WHERE x MATCH 'hello'`,
		`CREATE TEMP TABLE ttrig(a)`,
		`CREATE TABLE log(a)`,
		`CREATE TEMP TRIGGER tt_pragma AFTER INSERT ON ttrig BEGIN
			INSERT INTO log SELECT count(*) FROM pragma_table_info('base'); END`,

		// --- trigger bodies: the second DDL-origin site ---
		`CREATE TABLE trg_sel(a)`,
		`CREATE TRIGGER tr_sel AFTER INSERT ON trg_sel BEGIN
			INSERT INTO log SELECT count(*) FROM pragma_table_info('base'); END`,
		`CREATE TABLE trg_when(a)`,
		`CREATE TRIGGER tr_when AFTER INSERT ON trg_when
			WHEN (SELECT count(*) FROM pragma_table_info('base')) > 0
			BEGIN INSERT INTO log VALUES(9); END`,
		`CREATE TABLE trg_upd(a)`,
		`CREATE TRIGGER tr_upd AFTER INSERT ON trg_upd BEGIN
			UPDATE log SET a = (SELECT count(*) FROM pragma_table_info('base')); END`,
		`CREATE TABLE trg_ok(a)`,
		`CREATE TRIGGER tr_ok AFTER INSERT ON trg_ok BEGIN INSERT INTO log VALUES(1); END`,
		`CREATE VIEW v_writable AS SELECT * FROM base`,
		`CREATE TRIGGER tr_insteadof INSTEAD OF INSERT ON v_writable BEGIN
			INSERT INTO log SELECT count(*) FROM pragma_table_info('base'); END`,
	}
}

// TestTrustedSchemaOffEnforced is the gate: with the flag off, every unsafe
// shape is refused by BOTH engines in the oracle's own wording, every innocuous
// shape still answers identically, and turning it back on restores all of them.
func TestTrustedSchemaOffEnforced(t *testing.T) {
	p := tsOpenPair(t)
	p.setup(t, tsBaseSchema()...)

	// Every view is readable on BOTH engines while the flag is ON. Asserting
	// this FIRST is what stops the OFF half from passing vacuously: a view
	// musql could not read anyway would "fail correctly" for the wrong reason.
	onlyOn := []string{
		"v_match", "v_offsets", "v_snippet", "v_matchinfo", "v_fts4aux", "v_tokenize",
		"v_pragmavtab", "v_bareeponym", "v_bareargs", "v_nested", "v_cte", "v_subq", "v_case", "v_deriv",
	}
	always := []string{"v_scan", "v_docid", "v_like", "v_json", "v_jeach", "v_jtree", "v_rtree", "v_plain", "v_cteshadow", "tv_match"}
	for _, v := range append(append([]string{}, onlyOn...), always...) {
		p.bothAccept(t, `SELECT * FROM `+v)
	}
	p.bothAccept(t, `PRAGMA trusted_schema`)

	// ---- OFF ----
	p.setup(t, `PRAGMA trusted_schema=OFF`)
	p.bothAccept(t, `PRAGMA trusted_schema`)      // 0 on both, through the driver
	p.bothAccept(t, `PRAGMA main.trusted_schema`) // per-CONNECTION: the qualifier is ignored
	p.bothAccept(t, `PRAGMA temp.trusted_schema`)

	for _, tc := range []struct{ view, want string }{
		{"v_match", "unsafe use of MATCH()"},
		{"v_offsets", "unsafe use of offsets()"},
		{"v_snippet", "unsafe use of snippet()"},
		{"v_matchinfo", "unsafe use of matchinfo()"},
		{"v_fts4aux", `unsafe use of virtual table "ftaux"`},
		{"v_tokenize", `unsafe use of virtual table "tok"`},
		{"v_pragmavtab", `unsafe use of virtual table "pragma_table_info"`},
		{"v_bareeponym", `unsafe use of virtual table "pragma_table_info"`},
		{"v_bareargs", `unsafe use of virtual table "pragma_table_info"`},
		{"v_nested", "unsafe use of MATCH()"},
		{"v_cte", "unsafe use of MATCH()"},
		{"v_subq", "unsafe use of MATCH()"},
		{"v_case", "unsafe use of MATCH()"},
		{"v_deriv", "unsafe use of MATCH()"},
	} {
		p.bothReject(t, `SELECT * FROM `+tc.view, tc.want)
	}
	// table_info over an INNOCUOUS view is untouched. Its unsafe counterpart is
	// connection-history-dependent in C SQLite and is pinned separately, in
	// TestTrustedSchemaTableInfoFreshConnection.
	p.bothAccept(t, `PRAGMA table_info(v_like)`)

	// The innocuous half is untouched -- including a view over LIKE, which is
	// exactly what like.test 18.x runs with this flag off.
	for _, v := range always {
		p.bothAccept(t, `SELECT * FROM `+v)
	}
	// ...and so is a DIRECT use of any of them: the rule is about SCHEMA
	// objects, not about the statement a user types.
	p.bothAccept(t, `SELECT count(*) AS q FROM ft WHERE x MATCH 'hello'`)
	p.bothAccept(t, `SELECT count(*) AS q FROM ftaux`)
	p.bothAccept(t, `SELECT count(*) AS q FROM pragma_table_info('base')`)
	p.bothAccept(t, `SELECT offsets(ft) AS q FROM ft WHERE x MATCH 'hello'`)

	// CREATE VIEW succeeds with the flag off; it is the USE that fails. DROP is
	// fine too -- dropping does not resolve the body.
	p.setup(t, `CREATE VIEW v_late AS SELECT count(*) AS q FROM ft WHERE x MATCH 'hi'`)
	p.bothReject(t, `SELECT * FROM v_late`, "unsafe use of MATCH()")
	p.setup(t, `DROP VIEW v_late`)

	// Trigger bodies, the second site -- and the TEMP trigger that is exempt.
	p.bothReject(t, `INSERT INTO trg_sel VALUES(1)`, `unsafe use of virtual table "pragma_table_info"`)
	p.bothReject(t, `INSERT INTO trg_when VALUES(1)`, `unsafe use of virtual table "pragma_table_info"`)
	p.bothReject(t, `INSERT INTO trg_upd VALUES(1)`, `unsafe use of virtual table "pragma_table_info"`)
	p.bothReject(t, `INSERT INTO v_writable VALUES(1, 'z', NULL)`, `unsafe use of virtual table "pragma_table_info"`)
	p.setup(t, `INSERT INTO trg_ok VALUES(1)`)  // an innocuous trigger still fires
	p.setup(t, `INSERT INTO ttrig VALUES(1)`)   // TEMP trigger: exempt outright
	p.bothAccept(t, `SELECT count(*) FROM log`) // ...and both wrote the same rows

	// ---- back ON ----
	p.setup(t, `PRAGMA trusted_schema=ON`)
	p.bothAccept(t, `PRAGMA trusted_schema`)
	for _, v := range append(append([]string{}, onlyOn...), always...) {
		p.bothAccept(t, `SELECT * FROM `+v)
	}
	p.bothAccept(t, `PRAGMA table_info(v_match)`)
	p.bothAccept(t, `PRAGMA table_info(v_pragmavtab)`)
	p.setup(t, `INSERT INTO trg_sel VALUES(2)`)
	p.bothAccept(t, `SELECT count(*) FROM log`)

	// NOT transactional: a setter inside BEGIN takes effect at once and is
	// still set after a ROLLBACK.
	p.setup(t, `BEGIN`, `PRAGMA trusted_schema=0`)
	p.bothAccept(t, `PRAGMA trusted_schema`)
	p.setup(t, `ROLLBACK`)
	p.bothAccept(t, `PRAGMA trusted_schema`)
	p.bothReject(t, `SELECT * FROM v_match`, "unsafe use of MATCH()")
}

// TestTrustedSchemaTableInfoFreshConnection pins the one shape in this rule
// that C SQLite does not answer the same way twice.
//
// Naming a view's columns means resolving its body, so on a FRESH connection
// "PRAGMA table_info(v)" over an unsafe view is refused exactly like a read of
// it. But SQLite caches a view's resolved column list on the connection's
// in-memory schema (Table.aCol), and once ANYTHING on that connection has
// resolved the body -- a successful read with the flag on, or an earlier
// table_info -- a later table_info answers from the cache and does NOT re-check.
// Measured against mattn/go-sqlite3 3.53.3, over the same file:
//
//	fresh conn, OFF, no prior read   PRAGMA table_info(v) -> unsafe use of MATCH()
//	read with ON, then OFF           PRAGMA table_info(v) -> 1 row
//	                                 SELECT * FROM v      -> unsafe use of MATCH()  (always)
//
// musql has no such cache -- it re-resolves a view's body every time -- so it
// reproduces the FIRST line exactly (pinned here) and, in the cached case,
// raises the same error where the oracle answers from its cache. That is a
// DECLINE, the acceptable failure, and it is the deliberate side to land on:
// the alternative (never checking table_info) would ANSWER where a fresh
// connection's oracle refuses, which is a wrong answer.
func TestTrustedSchemaTableInfoFreshConnection(t *testing.T) {
	p := tsOpenPair(t)
	p.setup(t,
		`CREATE TABLE base(a, b)`,
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`INSERT INTO ft VALUES('hello world')`,
		`CREATE VIEW v_match AS SELECT * FROM ft WHERE x MATCH 'hello'`,
		`CREATE VIEW v_pragmavtab AS SELECT count(*) AS q FROM pragma_table_info('base')`,
		`CREATE VIEW v_plain AS SELECT a + 1 AS q FROM base`,
	)
	// A fresh connection that has never resolved either body.
	p.reopen(t)
	p.setup(t, `PRAGMA trusted_schema=OFF`)
	p.bothReject(t, `PRAGMA table_info(v_match)`, "unsafe use of MATCH()")
	p.bothReject(t, `PRAGMA table_info(v_pragmavtab)`, `unsafe use of virtual table "pragma_table_info"`)
	p.bothAccept(t, `PRAGMA table_info(v_plain)`)
	// ...and a read is refused whatever the connection has already done.
	p.bothReject(t, `SELECT * FROM v_match`, "unsafe use of MATCH()")
}

// TestTrustedSchemaFTS5OffEnforced is the fts5 half, which only exists in the
// `-tags sqlite_fts5` build (the default-build oracle has no fts5 at all, so
// there would be nothing to compare against). bm25/highlight/snippet are
// virtual-table overloads exactly as fts3's are, and fts5vocab is a
// non-innocuous module.
func TestTrustedSchemaFTS5OffEnforced(t *testing.T) {
	if !harnessFTS5 {
		t.Skip("oracle has no fts5 in this build; run with -tags sqlite_fts5")
	}
	p := tsOpenPair(t)
	p.setup(t,
		`CREATE VIRTUAL TABLE f5 USING fts5(x)`,
		`INSERT INTO f5 VALUES('hello world')`,
		`CREATE VIRTUAL TABLE f5v USING fts5vocab(f5, row)`,
		`CREATE VIEW v5_match     AS SELECT count(*) AS q FROM f5 WHERE f5 MATCH 'hello'`,
		`CREATE VIEW v5_colmatch  AS SELECT count(*) AS q FROM f5 WHERE x MATCH 'hello'`,
		`CREATE VIEW v5_bm25      AS SELECT bm25(f5) AS q FROM f5 WHERE f5 MATCH 'hello'`,
		`CREATE VIEW v5_highlight AS SELECT highlight(f5,0,'<','>') AS q FROM f5 WHERE f5 MATCH 'hello'`,
		`CREATE VIEW v5_snippet   AS SELECT snippet(f5,0,'<','>','..',4) AS q FROM f5 WHERE f5 MATCH 'hello'`,
		`CREATE VIEW v5_vocab     AS SELECT count(*) AS q FROM f5v`,
		`CREATE VIEW v5_scan      AS SELECT count(*) AS q FROM f5`,
	)
	for _, v := range []string{"v5_match", "v5_colmatch", "v5_bm25", "v5_highlight", "v5_snippet", "v5_vocab", "v5_scan"} {
		p.bothAccept(t, `SELECT * FROM `+v)
	}
	p.setup(t, `PRAGMA trusted_schema=OFF`)
	for _, tc := range []struct{ view, want string }{
		{"v5_match", "unsafe use of MATCH()"},
		{"v5_colmatch", "unsafe use of MATCH()"},
		{"v5_bm25", "unsafe use of bm25()"},
		{"v5_highlight", "unsafe use of highlight()"},
		{"v5_snippet", "unsafe use of snippet()"},
		{"v5_vocab", `unsafe use of virtual table "f5v"`},
	} {
		p.bothReject(t, `SELECT * FROM `+tc.view, tc.want)
	}
	p.bothAccept(t, `SELECT * FROM v5_scan`) // the fts5 MODULE itself is innocuous
	p.bothAccept(t, `SELECT count(*) AS q FROM f5 WHERE f5 MATCH 'hello'`)
}

// TestTrustedSchemaEngineDirectGetter pins the ENGINE's own two surfaces, which
// the driver test above cannot reach: execPragma's setter (zero rows, and the
// value spellings it declines rather than guesses) and queryPragma's getter
// cell, read back through a SnapshotPager rather than from the Conn's copy.
func TestTrustedSchemaEngineDirectGetter(t *testing.T) {
	runPragmaTailScript(t, []pragmaTailStep{
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`PRAGMA trusted_schema=OFF`),
		pstepRows(`PRAGMA trusted_schema`),
		pstepRows(`PRAGMA main.trusted_schema`),
		pstepRows(`PRAGMA temp.trusted_schema`),
		pstep(`PRAGMA trusted_schema=off`),
		pstep(`PRAGMA trusted_schema=0`),
		pstep(`PRAGMA trusted_schema=no`),
		pstep(`PRAGMA trusted_schema=false`),
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`PRAGMA main.trusted_schema=1`),
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`PRAGMA temp.trusted_schema=0`),
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`BEGIN`),
		pstep(`PRAGMA trusted_schema=1`),
		pstepRows(`PRAGMA trusted_schema`),
		pstep(`COMMIT`),
		pstepRows(`PRAGMA trusted_schema`),
	})

	// A value spelling outside the canonical booleans is DECLINED rather than
	// guessed -- the same line foreign_keys draws. C SQLite's own parse is
	// sqlite3GetBoolean's idiosyncratic one, verified here so the decline is
	// pinned to a live measurement rather than a memory: "=2" is ON while
	// "=-1", "=bogus", "=''" and "=0.0" are all OFF.
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
	for _, tc := range []struct {
		value string
		want  string
	}{{"2", "1"}, {"-1", "0"}, {"bogus", "0"}, {"''", "0"}, {"0.0", "0"}} {
		// SERVED now, with sqlite3GetBoolean's own answer
		// (pragmaGetBoolean): "=2" is ON, everything else here is OFF. The
		// oracle's reading below is the assertion.
		if _, _, err := godb.ExecArgs(`PRAGMA trusted_schema=`+tc.value, nil); err != nil {
			t.Fatalf("PRAGMA trusted_schema=%s: expected it to be served, got %v", tc.value, err)
		}
		if want := tc.want == "1"; godb.TrustedSchema() != want {
			t.Errorf("PRAGMA trusted_schema=%s left trusted_schema=%v, want %v",
				tc.value, godb.TrustedSchema(), want)
		}
		if _, err := cgodb.Exec(`PRAGMA trusted_schema=1`); err != nil {
			t.Fatalf("oracle reset: %v", err)
		}
		if _, err := cgodb.Exec(`PRAGMA trusted_schema=` + tc.value); err != nil {
			t.Fatalf("oracle PRAGMA trusted_schema=%s: %v", tc.value, err)
		}
		var got string
		if err := cgodb.QueryRow(`PRAGMA trusted_schema`).Scan(&got); err != nil {
			t.Fatalf("oracle getter: %v", err)
		}
		if got != tc.want {
			t.Fatalf("oracle PRAGMA trusted_schema=%s reads back %s, expected %s -- this fixture's own premise moved", tc.value, got, tc.want)
		}
	}
}

// tsFuzzPiece is one building block a fuzzed view body can be made of: SQL that
// yields a single scalar, and whether C SQLite calls it unsafe from a schema
// object.
type tsFuzzPiece struct {
	sql    string
	unsafe bool
}

var tsFuzzPieces = []tsFuzzPiece{
	{`(SELECT count(*) FROM ft WHERE x MATCH 'hello')`, true},
	{`(SELECT count(*) FROM ftaux)`, true},
	{`(SELECT count(*) FROM pragma_table_info('base'))`, true},
	{`(SELECT count(*) FROM pragma_table_info WHERE arg='base')`, true},
	{`(WITH pragma_table_info(z) AS (SELECT 7) SELECT z FROM pragma_table_info)`, false},
	{`(SELECT offsets(ft) IS NOT NULL FROM ft WHERE x MATCH 'hello')`, true},
	{`(SELECT count(*) FROM ft)`, false},
	{`(SELECT count(*) FROM json_each('[1,2]'))`, false},
	{`(SELECT count(*) FROM base WHERE b LIKE 'x%')`, false},
	{`(SELECT json_extract(c,'$[0]') FROM base)`, false},
	{`abs(-3)`, false},
	{`length('abc')`, false},
}

// tsWrap nests inner one level deeper in a randomly chosen position. Each
// wrapper puts the expression somewhere a walk could plausibly forget to look
// -- a CASE arm, a window's PARTITION BY, an ORDER BY term, a HAVING clause, a
// derived table, a CTE, an IN list, a compound arm.
func tsWrap(rng *rand.Rand, inner string) string {
	switch rng.Intn(12) {
	case 0:
		return `(SELECT CASE WHEN 0 THEN ` + inner + ` ELSE 1 END)`
	case 1:
		return `(SELECT 1 WHERE ` + inner + ` IS NOT NULL)`
	case 2:
		return `(SELECT count(*) FROM (SELECT ` + inner + ` AS z))`
	case 3:
		return `(WITH w(z) AS (SELECT ` + inner + `) SELECT count(*) FROM w)`
	case 4:
		return `(SELECT 1 FROM base GROUP BY a HAVING ` + inner + ` IS NOT NULL)`
	case 5:
		return `(SELECT a FROM base ORDER BY ` + inner + ` LIMIT 1)`
	case 6:
		return `(SELECT 1 WHERE 1 IN (` + inner + `, 1))`
	case 7:
		return `(SELECT count(*) OVER (PARTITION BY ` + inner + `) FROM base LIMIT 1)`
	case 8:
		return `(SELECT 1 WHERE EXISTS (SELECT ` + inner + `))`
	case 9:
		return `(SELECT ` + inner + ` UNION ALL SELECT 2 LIMIT 1)`
	case 10:
		return `(SELECT 1 FROM base AS o WHERE o.a = coalesce(` + inner + `, o.a))`
	default:
		return `(SELECT cast(` + inner + ` AS INTEGER))`
	}
}

// TestTrustedSchemaFuzzViewBodies is the randomized half. The rule is not
// value-dependent -- it is STRUCTURAL, so what a fuzz has to attack is where in
// a body an unsafe piece can hide. This plants one randomly chosen piece at a
// random nesting depth inside a randomly built view body and requires the two
// engines to reach the SAME accept/reject decision, and the same cells when
// they accept. A walk that forgets one nesting position shows up here as a
// musql answer where the oracle refused.
func TestTrustedSchemaFuzzViewBodies(t *testing.T) {
	p := tsOpenPair(t)
	p.setup(t,
		`CREATE TABLE base(a, b, c)`,
		`INSERT INTO base VALUES(1, 'x', '[1,2]')`,
		`CREATE VIRTUAL TABLE ft USING fts4(x)`,
		`INSERT INTO ft VALUES('hello world')`,
		`CREATE VIRTUAL TABLE ftaux USING fts4aux(ft)`,
	)
	rng := rand.New(rand.NewSource(20260802))
	const iterations = 400
	tried, compared := 0, 0
	for i := 0; i < iterations; i++ {
		piece := tsFuzzPieces[rng.Intn(len(tsFuzzPieces))]
		body := piece.sql
		for d := rng.Intn(3); d >= 0; d-- {
			body = tsWrap(rng, body)
		}
		view := fmt.Sprintf("vf%d", i)
		create := `CREATE VIEW ` + view + ` AS SELECT ` + body + ` AS q`
		// A body neither engine can even CREATE (or that either declines with
		// the flag ON, which is an ordinary capability gap, not this rule) is
		// skipped -- but only after both sides agree it is out of scope, so a
		// silent one-sided failure cannot hide here.
		_, cerr := p.cgo.Exec(create)
		_, merr := p.mush.Exec(create)
		if (cerr != nil) != (merr != nil) {
			t.Fatalf("iter %d: CREATE disagreement for %q\n  go:  %v\n  cgo: %v", i, create, merr, cerr)
		}
		if cerr != nil {
			continue
		}
		tried++
		read := `SELECT * FROM ` + view
		_, _, conErr := tclRunCGOQuery(p.cgo, read)
		_, _, monErr := tclRunCGOQuery(p.mush, read)
		if conErr != nil || monErr != nil {
			continue // a capability gap with the flag ON: out of this rule's scope
		}
		if _, err := p.cgo.Exec(`PRAGMA trusted_schema=OFF`); err != nil {
			t.Fatalf("oracle PRAGMA: %v", err)
		}
		if _, err := p.mush.Exec(`PRAGMA trusted_schema=OFF`); err != nil {
			t.Fatalf("musql PRAGMA: %v", err)
		}
		cCols, cRows, coffErr := tclRunCGOQuery(p.cgo, read)
		gCols, gRows, moffErr := tclRunCGOQuery(p.mush, read)
		if (coffErr != nil) != (moffErr != nil) {
			t.Fatalf("iter %d: trusted_schema=OFF disagreement for %q (piece unsafe=%v)\n  go:  %v\n  cgo: %v",
				i, create, piece.unsafe, moffErr, coffErr)
		}
		if coffErr == nil {
			if piece.unsafe {
				t.Fatalf("iter %d: the ORACLE accepted %q with the flag off, but its piece is recorded unsafe -- the rule's premise moved", i, create)
			}
			if ok, reason := queryResultsMatch(gCols, gRows, cCols, cRows, false); !ok {
				t.Fatalf("iter %d: %q: %s", i, read, reason)
			}
			compared++
		} else if !piece.unsafe {
			t.Fatalf("iter %d: the ORACLE refused %q with the flag off, but its piece is recorded innocuous: %v", i, create, coffErr)
		} else {
			compared++
		}
		if _, err := p.cgo.Exec(`PRAGMA trusted_schema=ON`); err != nil {
			t.Fatalf("oracle PRAGMA: %v", err)
		}
		if _, err := p.mush.Exec(`PRAGMA trusted_schema=ON`); err != nil {
			t.Fatalf("musql PRAGMA: %v", err)
		}
	}
	// A fuzz that compared nothing is a fuzz that proves nothing.
	if compared < iterations/4 {
		t.Fatalf("only %d of %d generated bodies were comparable (%d created) -- this fuzz is not exercising the rule", compared, iterations, tried)
	}
	t.Logf("fuzz: %d created, %d compared under trusted_schema=OFF", tried, compared)
}
