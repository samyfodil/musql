// Tests two parser/resolution shapes: parenthesized VALUES in expression
// position and aliases on parenthesized join groups. Both test that correct
// cases answer identically to C SQLite and boundary cases stay declined.
package compat

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// prtSchema is the fixture for both tests.
var prtSchema = []string{
	`CREATE TABLE ta(a,b)`,
	`CREATE TABLE tb(c,d)`,
	`CREATE TABLE tc(e,f)`,
	`CREATE TABLE td(b,g)`,
	`CREATE TABLE te(a,z)`,
	`CREATE TABLE tq("values")`,
	`INSERT INTO ta VALUES(1,2),(3,4),(NULL,5)`,
	`INSERT INTO tb VALUES(1,20),(9,90)`,
	`INSERT INTO tc VALUES(1,200),(3,300)`,
	`INSERT INTO td VALUES(2,'x'),(4,'y')`,
	`INSERT INTO te VALUES(1,'p'),(7,'q')`,
	`INSERT INTO tq VALUES(7)`,
	`CREATE TABLE b3(a,b,PRIMARY KEY(a,b))`,
	`CREATE TABLE b4(a)`,
	`CREATE TABLE b5(a,b)`,
	`INSERT INTO b3 VALUES(1,1),(1,2)`,
	`INSERT INTO b4 VALUES(1)`,
	`INSERT INTO b5 VALUES(1,1),(1,2)`,
}

// prtHarness pairs engine and cgo connections with prtSchema.
type prtHarness struct {
	t *testing.T
	p *engine.ReadOnlyPager
	c *sql.DB
}

func newPRTHarness(t *testing.T) *prtHarness {
	t.Helper()
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edb.Close() })
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	for _, s := range prtSchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	return &prtHarness{t: t, p: p, c: cdb}
}

// mustAgree checks that query answers identically on both engines.
func (h *prtHarness) mustAgree(q string) {
	h.t.Helper()
	ec, ev, eerr := h.p.QueryArgs(q, nil)
	if eerr != nil {
		h.t.Errorf("[%s] engine declined a shape this gate requires: %v", q, eerr)
		return
	}
	cc, cr, cerr := cgoSelect(h.t, h.c, q, nil)
	if cerr != nil {
		h.t.Errorf("[%s] C SQLite rejected it -- the gate's own premise is wrong: %v", q, cerr)
		return
	}
	if ok, why := queryResultsMatch(ec, engineRowsToStrings(ev), cc, cr, strings.Contains(strings.ToUpper(q), "ORDER BY")); !ok {
		h.t.Errorf("[%s] DIVERGES: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", q, why, ec, engineRowsToStrings(ev), cc, cr)
	}
}

// mustDecline requires that the engine REJECT q. A decline is never a wrong
// answer, so this is only ever asserted where ANSWERING would be one -- see
// each caller for which rule the shape is on the far side of.
func (h *prtHarness) mustDecline(q string) {
	h.t.Helper()
	if cols, rows, err := h.p.QueryArgs(q, nil); err == nil {
		h.t.Errorf("[%s] engine ANSWERED a shape it must decline: cols=%v rows=%v", q, cols, engineRowsToStrings(rows))
	}
}

// TestParenValuesSubqueryExpr gates rule 1: "(VALUES ...)" in ordinary
// expression position is a SUBQUERY, with the ordinary scalar reading (first
// row, first column) and the ordinary row-value-left-hand-side reading under
// IN -- identical, in every case measured, to spelling it "(SELECT ...)".
func TestParenValuesSubqueryExpr(t *testing.T) {
	h := newPRTHarness(t)

	// rowvalue.test 18.2 and 18.5 -- the two corpus statements this closes.
	// Their "(SELECT b3.a, b3.b)" twins (18.1/18.4) already answered, which is
	// exactly the point: the row-value path was already there, the parser just
	// never handed it this spelling.
	for _, q := range []string{
		`SELECT * FROM b3 WHERE (VALUES(b3.a, b3.b)) IN ( SELECT a, b FROM b5 )`,
		`SELECT * FROM b3 JOIN b4 ON b4.a = b3.a WHERE (VALUES(b3.a, b3.b)) IN ( SELECT a, b FROM b5 )`,
		`SELECT * FROM b3 WHERE (SELECT b3.a, b3.b) IN ( SELECT a, b FROM b5 )`,
		`SELECT (VALUES(1))`,
		`SELECT (VALUES(1),(2))`,
		`SELECT (VALUES(1,2)) = (1,2)`,
		`SELECT (VALUES(1,2)) IN (SELECT a,b FROM b5)`,
		`SELECT EXISTS(VALUES(1))`,
		`SELECT NOT EXISTS(VALUES(1))`,
		`SELECT 1 IN (VALUES(1),(2))`,
		`SELECT (WITH x(y) AS (VALUES(3)) SELECT y FROM x)`,
	} {
		h.mustAgree(q)
	}

	// The QUOTED spellings are the boundary. VALUES is a reserved word in
	// SQLite, so a bare "values" is never a column reference ("SELECT (values)
	// FROM tq" is a syntax error there) -- but all three quoted spellings ARE,
	// and each must still read tq's column rather than be taken for the
	// keyword. Dropping the !quoted test in peekStartsParenSubquery turns each
	// of these from an answer into a parse error, which is what this half
	// catches.
	for _, q := range []string{
		`SELECT ("values") FROM tq`,
		`SELECT ([values]) FROM tq`,
		"SELECT (`values`) FROM tq",
	} {
		h.mustAgree(q)
	}
}

// TestParenJoinGroupAlias gates rule 2's ANSWERING half: an aliased
// parenthesized join group of two or more items, over every internal join kind
// and every reference position, must match C SQLite exactly -- rows, column
// count, and column NAMES.
func TestParenJoinGroupAlias(t *testing.T) {
	h := newPRTHarness(t)

	// The corpus statement this closes (tkt3935.test 3), plus its already-
	// working derived-table twin (tkt3935.test 2) as the control.
	h.mustAgree(`SELECT j1.b FROM ( SELECT * FROM ta INNER JOIN tb ON a=c ) AS j1`)
	h.mustAgree(`SELECT j1.b FROM (ta INNER JOIN tb ON a=c) AS j1`)

	// Every internal join kind, aliased, with the group in the leading
	// position and in a non-leading one, and with the alias read from the
	// select list, the WHERE, the GROUP BY and the ORDER BY. Every one of
	// these is an ALIASED group, so every one is an atomic GroupLen span
	// regardless of its own internal join kind -- checkFlattenSafe
	// (sql_parser.go) forces that off HasGroupAlias alone now, matching
	// parse.y's own seltablist production (parse.y:777-816): the ONLY case
	// it splices a parenthesized group flat ("A = F", no nesting) is
	// "leading AND no alias token AND no trailing ON/USING", so an ALIASED
	// group -- of any internal join kind -- is unconditionally
	// SF_NestedFrom in C SQLite too. (Checked in an earlier revision of
	// this test: JOIN/CROSS JOIN here were wrongly believed to flatten,
	// which happened to be true before that fix and is not C SQLite's
	// rule -- "no such table: a1" over "t4 RIGHT JOIN t5 CROSS JOIN
	// (t2 CROSS JOIN t0) AS a1 ON (a1.c0 < a1.c1)", joinI.test, is what
	// exposed it: the group's own outward ON clause was compiled one
	// physical join level too early, before its own second member was ever
	// bound.)
	//
	// Two things every case here steers around, both PRE-EXISTING at c9df76b
	// and verified unchanged by this rule (the identical UNALIASED spelling
	// declines identically on both trees):
	//
	//   - an UNQUALIFIED name in the group's own ON clause becomes ambiguous
	//     once the group is flattened into a surrounding FROM that also has
	//     that name ("te JOIN (ta JOIN tb ON a=c)" -> "ambiguous column name:
	//     a"), so the joined-onward cases below write "ta.a=tb.c";
	//   - an aggregate or GROUP BY over an atomic GroupLen span -- which
	//     every case here now is -- is declined outright, so those two
	//     select shapes are not exercised through this loop at all (see
	//     TestParenJoinGroupAliasAggregateDeclines).
	for _, op := range []string{"JOIN", "CROSS JOIN", "LEFT JOIN", "RIGHT JOIN", "FULL JOIN"} {
		grp := "ta " + op + " tb ON ta.a=tb.c"
		qs := []string{
			`SELECT * FROM (` + grp + `) AS g`,
			`SELECT g.a, g.b, g.c, g.d FROM (` + grp + `) AS g`,
			`SELECT ta.a, tb.d FROM (` + grp + `) AS g`,
			`SELECT g.a FROM (` + grp + `) AS g WHERE g.d=20`,
			`SELECT g.a FROM (` + grp + `) AS g ORDER BY g.a`,
			`SELECT * FROM te JOIN (` + grp + `) AS g ON g.a=te.a`,
			`SELECT * FROM te LEFT JOIN (` + grp + `) AS g ON g.a=te.a`,
			`SELECT * FROM (` + grp + `) AS g, te WHERE g.a=te.a`,
			`SELECT (SELECT count(*) FROM te WHERE te.a=g.a) FROM (` + grp + `) AS g`,
			// The same group UNALIASED must be untouched by this rule.
			`SELECT * FROM (` + grp + `)`,
		}
		for _, q := range qs {
			h.mustAgree(q)
		}
	}

	// Three members, inner aliases, a nested group, and a derived table inside
	// the group -- the alias reaches every leaf either way, and each leaf keeps
	// its own name alongside it.
	for _, q := range []string{
		`SELECT * FROM (ta JOIN tb ON a=c JOIN tc ON a=e) AS g`,
		`SELECT g.f FROM (ta JOIN tb ON a=c JOIN tc ON a=e) AS g`,
		`SELECT * FROM (ta AS p JOIN tb AS q ON p.a=q.c) AS g`,
		`SELECT g.a, p.a, q.d FROM (ta AS p JOIN tb AS q ON p.a=q.c) AS g`,
		`SELECT * FROM (ta JOIN (tb JOIN tc ON c=e) ON a=c) AS g`,
		`SELECT g.f FROM (ta JOIN (tb JOIN tc ON c=e) ON a=c) AS g`,
		`SELECT * FROM (ta JOIN (SELECT c AS a2, d FROM tb) AS s ON a=a2) AS g`,
		`SELECT g.d, s.d FROM (ta JOIN (SELECT c AS a2, d FROM tb) AS s ON a=a2) AS g`,
		// The alias may repeat a member's own name; the member's columns still
		// answer through it.
		`SELECT * FROM (ta JOIN tb ON a=c) AS ta`,
		`SELECT ta.a FROM (ta JOIN tb ON a=c) AS ta`,
		// Bare "(t1) AS x", the one-item form, keeps moving the alias onto the
		// item itself -- unchanged, and the reason len(inner)==1 stays its own
		// branch.
		`SELECT x.b FROM (ta) AS x`,
	} {
		h.mustAgree(q)
	}
}

// TestParenJoinGroupAliasAggregate was the other side of that boundary and is
// now an AGREEMENT gate. An aggregate or GROUP BY reading a parenthesized join
// group answers in C SQLite -- a group is not otherwise a barrier to either
// -- and this compiler declined it outright rather than "reconcile a group's own
// flattened member scopes against the aggregate compiler's offset-based
// rewriting", which is how the old comment here described the bug it was
// standing in front of: those member scopes carried WITHIN-GROUP offsets, so a
// group written anywhere but first overwrote every earlier source's columns in
// the drain row. resolveJoinSources rebases them (vdbe_join_codegen.go).
//
// Every join kind is exercised, not just the ones that used to (wrongly)
// flatten -- see checkFlattenSafe's own doc comment (sql_parser.go): an ALIASED
// group is unconditionally atomic in C SQLite too, regardless of its internal
// join kind. The LEFT/RIGHT/FULL rows are the interesting ones: they are where
// the group is NULL-extended, and where a misplaced column shows up as a value
// rather than as a missing row.
func TestParenJoinGroupAliasAggregate(t *testing.T) {
	h := newPRTHarness(t)
	for _, op := range []string{"JOIN", "CROSS JOIN", "LEFT JOIN", "RIGHT JOIN", "FULL JOIN"} {
		grp := "ta " + op + " tb ON ta.a=tb.c"
		for _, q := range []string{
			`SELECT count(*) FROM (` + grp + `) AS g`,
			`SELECT max(g.d) FROM (` + grp + `) AS g`,
			`SELECT g.a FROM (` + grp + `) AS g GROUP BY g.a ORDER BY g.a`,
		} {
			h.mustAgree(q)
		}
	}
}

// TestParenJoinGroupAliasDeclines gates rule 2's BOUNDARY. Every shape here
// answers in C SQLite; each is declined because answering it would need a
// column list this engine does not reproduce, and shipping one anyway is the
// wrong answer the never-wrong rule exists to prevent. Verified against
// mattn/go-sqlite3 3.53.3 -- the oracle answers are quoted per case.
func TestParenJoinGroupAliasDeclines(t *testing.T) {
	h := newPRTHarness(t)

	// A column name REPEATED across the group's members is the ONE boundary,
	// and it covers both rewrites at once.
	//
	// Plain repeat, no coalescing: aliased, SQLite reports "ambiguous column
	// name: a" for "SELECT *" and for a bare "a", while answering "SELECT g.a"
	// (silently the first member's) and "count(*)" -- and it names a second
	// qualified read "a:1". Unaliased, the very same group simply shows the
	// name twice. Only a whole-statement decline is exact.
	h.mustDecline(`SELECT * FROM (ta JOIN te ON ta.a=te.a) AS g`)
	// count(*) over that same group ANSWERS in C SQLite -- the comment above
	// says so, and the blanket decline was covering it only because an
	// aggregate over ANY group was declined at the time (the member-scope
	// offset bug, see TestParenJoinGroupAliasAggregate). It reads no repeated
	// name, so nothing about it is ambiguous.
	h.mustAgree(`SELECT count(*) FROM (ta JOIN te ON ta.a=te.a) AS g`)
	// ...but a qualified read through the alias answers exactly as real
	// SQLite does (the first member's column), and so do both members' own
	// columns, the second named "a:1" by the group's rebuilt column list
	// (tableScope.nestedColNames).
	for _, q := range []string{
		`SELECT g.a FROM (ta JOIN te ON ta.a=te.a) AS g`,
		`SELECT g.z FROM (ta JOIN te ON ta.a=te.a) AS g`,
		`SELECT ta.a, te.a FROM (ta JOIN te ON ta.a=te.a) AS g`,
	} {
		h.mustAgree(q)
	}
	h.mustAgree(`SELECT * FROM (ta JOIN te ON ta.a=te.a)`)

	// USING/NATURAL inside the group, where SQLite additionally moves the
	// coalesced column to the FRONT: "(ta JOIN td USING(b)) AS g" is b,a,g
	// where the same group unaliased is a,b,g, and "SELECT ta.b" through it
	// comes back named "b:1". No separate guard is needed for these -- USING
	// and NATURAL only coalesce a name BOTH sides carry, which is a repeat by
	// definition, so the check above already declines every one of them.
	for _, q := range []string{
		`SELECT * FROM (ta JOIN td USING(b)) AS g`,
		`SELECT * FROM (ta NATURAL JOIN td) AS g`,
		`SELECT * FROM (ta NATURAL LEFT JOIN td NATURAL JOIN te) AS qq FULL JOIN tc ON true`,
	} {
		h.mustDecline(q)
	}
	h.mustAgree(`SELECT g.a FROM (ta JOIN td USING(b)) AS g`)
	h.mustAgree(`SELECT ta.b, td.b, g.b FROM (ta JOIN td USING(b)) AS g`)
	// ...and the same groups WITHOUT an alias must keep answering: the decline
	// above is about the alias, not about USING/NATURAL in a group.
	h.mustAgree(`SELECT * FROM (ta JOIN td USING(b))`)
	h.mustAgree(`SELECT * FROM (ta NATURAL JOIN td)`)

	// The converse, and the reason the boundary is the repeat rather than the
	// keyword: a NATURAL join with NO column in common coalesces nothing and
	// reorders nothing, so it ANSWERS -- "(ta NATURAL JOIN tb) AS g" over
	// ta(a,b)/tb(c,d) is the plain a,b,c,d (verified directly). A syntactic
	// "decline any USING/NATURAL group" guard would throw these away.
	for _, q := range []string{
		`SELECT * FROM (ta NATURAL JOIN tb) AS g`,
		`SELECT g.a, g.d FROM (ta NATURAL JOIN tb) AS g`,
		`SELECT * FROM (ta NATURAL LEFT JOIN tb) AS g`,
		`SELECT * FROM (ta NATURAL CROSS JOIN tb) AS g`,
	} {
		h.mustAgree(q)
	}

	// The alias is a QUALIFIER, not a scope: it carries no pseudo-rowid and
	// cannot be starred. Both are what C SQLite does too ("no such column:
	// g.rowid" / "no such table: g"), so these are agreement, not gaps -- gated
	// so that widening the qualifier into a full scope cannot pass unnoticed.
	for _, q := range []string{
		`SELECT g.rowid FROM (ta JOIN tb ON a=c) AS g`,
		`SELECT g.* FROM (ta JOIN tb ON a=c) AS g`,
	} {
		h.mustDecline(q)
		if _, _, cerr := cgoSelect(t, h.c, q, nil); cerr == nil {
			t.Errorf("[%s] C SQLite ACCEPTS this -- it is a gap, not agreement; re-derive the rule", q)
		}
	}

	// A group alias does not reach out through further parentheses when the
	// re-wrapped term is not leading: parse.y:780-808 rebuilds it without the
	// inner alias, so C SQLite answers the star and a member-qualified read
	// but says "no such column: g.c" once g is referenced. This engine answered
	// g.c from the members for a while (a wrong answer); it refuses it now.
	h.mustDecline(`SELECT g.c FROM ta JOIN ((tb JOIN te ON c=te.a) AS g) ON 1`)
	h.mustAgree(`SELECT tb.c, te.a FROM ta JOIN ((tb JOIN te ON c=te.a) AS g) ON 1`)
}
