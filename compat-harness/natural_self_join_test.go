package compat

// Tests NATURAL/USING joins where both sides share the same table name or alias.
// Row count is the key assertion: a collapsed condition would produce a cross product.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildNaturalSelfJoinDB builds test tables with various self-join scenarios.
func buildNaturalSelfJoinDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/natural_self_join.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE ss1(a INTEGER, b TEXT)`,
		`INSERT INTO ss1 VALUES(1,'one')`,
		`INSERT INTO ss1 VALUES(2,'two')`,
		`INSERT INTO ss1 VALUES(2,'deux')`, // shares a=2 with the row above
		`INSERT INTO ss1 VALUES(NULL,'null')`,
		`CREATE TABLE ss2(a INTEGER, c TEXT)`,
		`INSERT INTO ss2 VALUES(1,'x')`,
		`INSERT INTO ss2 VALUES(2,'y')`,
		`INSERT INTO ss2 VALUES(5,'z')`, // unmatched in ss1
		`CREATE TABLE ssm(a INTEGER, b TEXT, k INTEGER)`,
		`INSERT INTO ssm VALUES(1,'one',7)`,
		`INSERT INTO ssm VALUES(2,'two',8)`,
		`CREATE TABLE ssd(p INTEGER, q TEXT)`,
		`INSERT INTO ssd VALUES(1,'P1')`,
		`INSERT INTO ssd VALUES(9,'P9')`,
		`CREATE TABLE ssk(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO ssk VALUES(1,'a')`,
		`INSERT INTO ssk VALUES(2,'b')`,
		`CREATE TABLE ssc(t TEXT COLLATE NOCASE, u TEXT)`,
		`INSERT INTO ssc VALUES('One','1')`,
		`INSERT INTO ssc VALUES('ONE','2')`,
		// One column only, so a NATURAL self-join coalesces ALL of it (SQLite
		// answers "SELECT *" over it) while the duplicated key still makes the
		// join WIDER than the table -- the row-count discriminator.
		`CREATE TABLE ssdup(a INTEGER)`,
		`INSERT INTO ssdup VALUES(1)`,
		`INSERT INTO ssdup VALUES(2)`,
		`INSERT INTO ssdup VALUES(2)`,
		`INSERT INTO ssdup VALUES(NULL)`,
		// Likewise single-column, and NOCASE: 'One' and 'ONE' are equal keys.
		`CREATE TABLE sscn(t TEXT COLLATE NOCASE)`,
		`INSERT INTO sscn VALUES('One')`,
		`INSERT INTO sscn VALUES('ONE')`,
		`CREATE VIEW ssv AS SELECT a, b FROM ss1`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// naturalSelfJoinCorpus is the parity gate corpus. Every statement's columns,
// column NAMES and rows must match C SQLite exactly.
var naturalSelfJoinCorpus = []joinCase{
	// --- the base shape, in both spellings. Every column is common, so
	// nothing survives un-coalesced and "*" shows the table's own columns
	// exactly once. ---
	// --- a column left EXPOSED by both copies. USING(a) coalesces only a, so
	// b stays visible under both -- which makes a reference to b ambiguous
	// while everything that never names it still answers. These were declined
	// as a whole statement until the ambiguity became per-reference; the
	// spellings that DO name b are in TestNaturalSelfJoinMutualRejections. ---
	{"SELECT a FROM ss1 JOIN ss1 USING(a)", false},
	{"SELECT count(*) FROM ss1 JOIN ss1 USING(a)", false},
	{"SELECT ss1.a FROM ss1 JOIN ss1 USING(a)", false},
	{"SELECT a FROM (ss1 JOIN ss1 USING(a))", false},
	{"SELECT count(*) FROM ss2 JOIN ss1 USING(a) JOIN ss1 USING(a)", false},
	{"SELECT count(*) FROM ss1 NATURAL JOIN ss1, ss1", false},

	{"SELECT * FROM ss1 NATURAL JOIN ss1", false},
	{"SELECT * FROM ss1 JOIN ss1 USING(a,b)", false},
	{"SELECT count(*) FROM ss1 NATURAL JOIN ss1", false},
	{"SELECT a FROM ss1 NATURAL JOIN ss1 ORDER BY a", true},
	{"SELECT b FROM ss1 NATURAL JOIN ss1 ORDER BY b", true},
	{"SELECT ss1.a, ss1.b FROM ss1 NATURAL JOIN ss1 ORDER BY a, b", true},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 WHERE b='two'", false},
	{"SELECT * FROM ssm NATURAL JOIN ssm ORDER BY a", true},
	{"SELECT * FROM ssd NATURAL JOIN ssd ORDER BY p", true},
	{"SELECT count(*) FROM ssd NATURAL JOIN ssd NATURAL JOIN ssd", false},
	{"SELECT * FROM ssdup NATURAL JOIN ssdup ORDER BY a", true},
	{"SELECT count(*) FROM ssdup JOIN ssdup USING(a)", false},
	{"SELECT * FROM sscn NATURAL JOIN sscn ORDER BY t", true},
	{"SELECT * FROM ssk NATURAL JOIN ssk ORDER BY id", true},
	{"SELECT * FROM ssk JOIN ssk USING(id,v) ORDER BY id", true},
	{"SELECT ssk.id FROM ssk NATURAL JOIN ssk ORDER BY id", true},
	// A NOCASE key: 'One' and 'ONE' are equal under it, so the self-join is
	// 4 rows wide, not 2 -- the collation reaches the desugared condition.
	{"SELECT count(*) FROM ssc NATURAL JOIN ssc", false},
	{"SELECT * FROM ssc JOIN ssc USING(t,u) ORDER BY u", true},

	// --- the same table under two different SPELLINGS of one name ---
	{"SELECT * FROM main.ss1 NATURAL JOIN ss1", false},
	{"SELECT * FROM ss1 NATURAL JOIN main.ss1", false},
	{"SELECT * FROM SS1 NATURAL JOIN ss1", false},
	{"SELECT * FROM main.ss1 NATURAL JOIN main.ss1", false},

	// --- the same ALIAS written on both sides ---
	{"SELECT * FROM ss1 AS x NATURAL JOIN ss1 AS x", false},
	{"SELECT * FROM ss1 x NATURAL JOIN ss1 x", false},
	{"SELECT * FROM ss1 AS x JOIN ss1 AS x USING(a,b)", false},
	{"SELECT * FROM (SELECT a,b FROM ss1) AS x NATURAL JOIN (SELECT a,b FROM ss1) AS x", false},
	// An alias that collides with the OTHER side's real table name.
	{"SELECT count(*) FROM ss2 AS ss1 NATURAL JOIN ss1", false},
	{"SELECT count(*) FROM ss1 NATURAL JOIN ss2 AS ss1", false},

	// --- a VIEW referenced twice (no rowid of its own) ---
	{"SELECT * FROM ssv NATURAL JOIN ssv", false},
	{"SELECT * FROM ssv NATURAL JOIN ss1", false},
	{"SELECT * FROM ss1 NATURAL JOIN ssv", false},
	{"SELECT * FROM ssv AS v1 NATURAL JOIN ssv", false},
	{"SELECT count(*) FROM ssv NATURAL JOIN ssv NATURAL JOIN ss1", false},

	// --- OUTER variants. The coalesced column keeps the representative's
	// value on a LEFT-extended row and must fall back to the other side's own
	// on a RIGHT/FULL-extended one; ss1's NULL key matches nothing either way,
	// so it survives once under LEFT/RIGHT and TWICE under FULL. ---
	{"SELECT * FROM ss1 NATURAL LEFT JOIN ss1 ORDER BY a, b", true},
	{"SELECT * FROM ss1 NATURAL RIGHT JOIN ss1 ORDER BY a, b", true},
	{"SELECT * FROM ss1 NATURAL FULL JOIN ss1 ORDER BY a, b", true},
	{"SELECT * FROM ss1 LEFT JOIN ss1 USING(a,b) ORDER BY a, b", true},
	{"SELECT * FROM ss1 RIGHT JOIN ss1 USING(a,b) ORDER BY a, b", true},
	{"SELECT * FROM ss1 FULL JOIN ss1 USING(a,b) ORDER BY a, b", true},
	{"SELECT * FROM ssd NATURAL RIGHT JOIN ssd ORDER BY p", true},
	{"SELECT * FROM ssm NATURAL LEFT JOIN ssm ORDER BY a", true},
	{"SELECT b FROM ss1 NATURAL LEFT JOIN ss1 ORDER BY 1", true},
	{"SELECT ss1.b FROM ss1 NATURAL LEFT JOIN ss1 ORDER BY 1", true},
	{"SELECT * FROM ss1 NATURAL LEFT JOIN ss1 NATURAL LEFT JOIN ss1 ORDER BY a, b", true},

	// --- three-way, and a self-join chained with a third table. The
	// "leftmost representative" rule must keep resolving across the hop: in
	// "ss2 NATURAL JOIN ss1 NATURAL JOIN ss1" the second ss1's "a" belongs to
	// ss2 while its "b" belongs to the FIRST ss1, so the two sides of one
	// desugared condition are pinned to different items. ---
	{"SELECT * FROM ss1 NATURAL JOIN ss1 NATURAL JOIN ss1", false},
	{"SELECT * FROM ss2 NATURAL JOIN ss1 NATURAL JOIN ss1", false},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 NATURAL JOIN ss2", false},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 JOIN ss2 USING(a)", false},
	{"SELECT * FROM ss1 NATURAL JOIN ssv NATURAL JOIN ss1", false},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 JOIN ssd ON ssd.p = a ORDER BY a, p", true},
	{"SELECT * FROM ss1 NATURAL JOIN ss1, ssd ORDER BY a, p", true},
	{"SELECT * FROM ssd, ss1 NATURAL JOIN ss1 ORDER BY p, a", true},
	{"SELECT count(*) FROM ss2 JOIN ss1 USING(a) JOIN ss1 USING(a,b)", false},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 NATURAL JOIN ssd ORDER BY a, p", true},

	// --- a parenthesized term with no OUTER join in it is FLATTENED by the
	// parser (no GroupLen marker), and in LEADING position SQLite answers it
	// too -- see TestNaturalSelfJoinMutualRejections for the non-leading
	// spelling, which SQLite itself rejects. ---
	{"SELECT * FROM (ss1 NATURAL JOIN ss1)", false},
	{"SELECT * FROM (ss1 NATURAL JOIN ss1), ssd ORDER BY a, p", true},
	{"SELECT * FROM (ss1 NATURAL JOIN ss1) JOIN ssd ON ssd.p = a", false},
	{"SELECT * FROM (ss1 NATURAL JOIN ss1) NATURAL JOIN ssd ORDER BY a, p", true},
	{"SELECT * FROM (ssv NATURAL JOIN ssv) JOIN ssd ON ssd.p = a", false},

	// --- the shape nested inside another query ---
	{"SELECT * FROM (SELECT * FROM ss1 NATURAL JOIN ss1) ORDER BY a, b", true},
	{"SELECT (SELECT count(*) FROM ss1 NATURAL JOIN ss1)", false},
	{"SELECT * FROM ssd WHERE p IN (SELECT a FROM ss1 NATURAL JOIN ss1)", false},
	{"SELECT a FROM ss1 NATURAL JOIN ss1 UNION ALL SELECT p FROM ssd ORDER BY 1", true},

	// --- aggregation / DISTINCT / WHERE over the coalesced column ---
	{"SELECT a, count(*) FROM ss1 NATURAL JOIN ss1 GROUP BY a ORDER BY a", true},
	{"SELECT DISTINCT a FROM ss1 NATURAL JOIN ss1 ORDER BY a", true},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 WHERE a > 1 ORDER BY a, b", true},
	{"SELECT * FROM ss1 NATURAL JOIN ss1 ORDER BY a DESC, b", true},

	// --- CTE referenced twice (only reachable through QueryArgs, which binds
	// WITH) ---
	{"WITH cte(a,b) AS (SELECT a,b FROM ss1) SELECT * FROM cte NATURAL JOIN cte", false},
	{"WITH cte(a,b) AS (SELECT a,b FROM ss1) SELECT count(*) FROM cte NATURAL JOIN cte NATURAL JOIN ss1", false},
}

// TestNaturalSelfJoinParity is the hard gate: every corpus statement must
// produce identical columns, column names and rows through musql and real C
// SQLite.
func TestNaturalSelfJoinParity(t *testing.T) {
	path := buildNaturalSelfJoinDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	wrong, total := 0, 0
	for _, tc := range naturalSelfJoinCorpus {
		total++
		vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if vErr != nil || cErr != nil {
			wrong++
			t.Errorf("[%s] unexpected error\n  musql=%v\n  cgo=%v", tc.sql, vErr, cErr)
			continue
		}
		vRows := engineRowsToStrings(vVals)
		if ok, reason := queryResultsMatch(vCols, vRows, cCols, cRows, tc.ordered); !ok {
			wrong++
			t.Errorf("[%s] DIVERGES from C SQLite: %s\n  musql: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
				tc.sql, reason, vCols, vRows, cCols, cRows)
		}
	}
	t.Logf("NATURAL/USING self-join parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("NATURAL/USING self-join parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestNaturalSelfJoinRowCounts is the hand-verified check the shape's own
// failure mode demands. A collapsed condition ("t1.a = t1.a") is a TAUTOLOGY:
// it still returns the right COLUMNS, in the right order, with plausible
// values -- only the row COUNT gives it away, as the full cross product. Each
// case therefore asserts an explicit row set, checked first against real C
// SQLite (so a wrong expectation fails the test rather than blessing itself)
// and only then against musql.
func TestNaturalSelfJoinRowCounts(t *testing.T) {
	path := buildNaturalSelfJoinDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	cases := []struct {
		sql      string
		wantCols []string
		wantRows [][]string
	}{
		// ss1 has FOUR rows; the tautology would return 4x4 = 16. Every column
		// is common, so the NULL-keyed row matches nothing and exactly the
		// three non-NULL rows survive, each matching only itself (b
		// distinguishes the two a=2 rows).
		{
			sql:      "SELECT * FROM ss1 NATURAL JOIN ss1 ORDER BY a, b",
			wantCols: []string{"a", "b"},
			wantRows: [][]string{
				{"I:1", "T:one"},
				{"I:2", "T:deux"},
				{"I:2", "T:two"},
			},
		},
		// The SAME join written with USING over both columns.
		{
			sql:      "SELECT * FROM ss1 JOIN ss1 USING(a,b) ORDER BY a, b",
			wantCols: []string{"a", "b"},
			wantRows: [][]string{
				{"I:1", "T:one"},
				{"I:2", "T:deux"},
				{"I:2", "T:two"},
			},
		},
		// ssdup's duplicated key makes a correct self-join genuinely WIDER
		// than the table -- a=2 appears twice on each side, so 1 + 4 = 5 rows
		// out of 4, the NULL key matching nothing. This is the case that
		// separates a real self-match (5) from the tautology (16) AND from an
		// over-tight match that paired each row only with itself (3).
		{
			sql:      "SELECT * FROM ssdup NATURAL JOIN ssdup ORDER BY a",
			wantCols: []string{"a"},
			wantRows: [][]string{
				{"I:1"}, {"I:2"}, {"I:2"}, {"I:2"}, {"I:2"},
			},
		},
		// NATURAL LEFT JOIN: the NULL-keyed row matches nothing, so it comes
		// back NULL-extended -- but since every column is coalesced, its own
		// values are what show (a NULL, b 'null'), not a NULL-extended blank.
		{
			sql:      "SELECT * FROM ss1 NATURAL LEFT JOIN ss1 ORDER BY a, b",
			wantCols: []string{"a", "b"},
			wantRows: [][]string{
				{"N", "T:null"},
				{"I:1", "T:one"},
				{"I:2", "T:deux"},
				{"I:2", "T:two"},
			},
		},
		// NATURAL RIGHT JOIN: the representative (the LEFT copy) is the side
		// that gets NULL-extended, so the coalesced columns must fall back to
		// the right copy's own values -- same four rows, not a NULL row.
		{
			sql:      "SELECT * FROM ss1 NATURAL RIGHT JOIN ss1 ORDER BY a, b",
			wantCols: []string{"a", "b"},
			wantRows: [][]string{
				{"N", "T:null"},
				{"I:1", "T:one"},
				{"I:2", "T:deux"},
				{"I:2", "T:two"},
			},
		},
		// NATURAL FULL JOIN: the unmatched NULL-keyed row is contributed by
		// BOTH sides, so it appears TWICE -- five rows, not four.
		{
			sql:      "SELECT * FROM ss1 NATURAL FULL JOIN ss1 ORDER BY a, b",
			wantCols: []string{"a", "b"},
			wantRows: [][]string{
				{"N", "T:null"},
				{"N", "T:null"},
				{"I:1", "T:one"},
				{"I:2", "T:deux"},
				{"I:2", "T:two"},
			},
		},
		// A THIRD table as the representative: ss2 owns "a", the first ss1
		// owns "b", so the second ss1's single desugared condition has its two
		// halves pinned to two DIFFERENT items. The old decline never even
		// fired on this shape (the representative's name differed), and it
		// answered 12 rows -- ss2 x ss1 matched on a (3), times ss1 unfiltered
		// (4) -- where three is right.
		{
			sql:      "SELECT * FROM ss2 NATURAL JOIN ss1 NATURAL JOIN ss1 ORDER BY a, b",
			wantCols: []string{"a", "c", "b"},
			wantRows: [][]string{
				{"I:1", "T:x", "T:one"},
				{"I:2", "T:y", "T:deux"},
				{"I:2", "T:y", "T:two"},
			},
		},
		// Multi-column common set: all three of ssm's columns are common, so
		// "*" is three columns and each row matches only itself.
		{
			sql:      "SELECT * FROM ssm NATURAL JOIN ssm ORDER BY a",
			wantCols: []string{"a", "b", "k"},
			wantRows: [][]string{
				{"I:1", "T:one", "I:7"},
				{"I:2", "T:two", "I:8"},
			},
		},
		// A NOCASE key makes 'One' and 'ONE' equal, so the self-join over a
		// 2-row table is 2x2 = 4 rows, not 2: the declared collation reaches
		// the index-pinned condition exactly as it does a qualified one.
		{
			sql:      "SELECT count(*) FROM sscn NATURAL JOIN sscn",
			wantCols: []string{"count(*)"},
			wantRows: [][]string{{"I:4"}},
		},
		// Zero common columns: NATURAL degenerates to a true cross join, and
		// the two same-named items' columns are BOTH kept -- 4 columns, 4
		// rows. (SQLite answers this only because count(*) asks for no column
		// by name; "SELECT *" over it is ambiguous.)
		{
			sql:      "SELECT count(*) FROM ssd NATURAL JOIN ssd",
			wantCols: []string{"count(*)"},
			wantRows: [][]string{{"I:2"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
			if cErr != nil {
				t.Fatalf("cgoSelect: %v", cErr)
			}
			if ok, reason := queryResultsMatch(tc.wantCols, tc.wantRows, cCols, cRows, true); !ok {
				t.Fatalf("hand-computed expectation itself disagrees with C SQLite (fix the test): %s\n  want: cols=%v rows=%v\n  cgo:  cols=%v rows=%v",
					reason, tc.wantCols, tc.wantRows, cCols, cRows)
			}
			vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
			if vErr != nil {
				t.Fatalf("QueryArgs: %v", vErr)
			}
			vRows := engineRowsToStrings(vVals)
			if ok, reason := queryResultsMatch(vCols, vRows, tc.wantCols, tc.wantRows, true); !ok {
				t.Errorf("self-join mismatch: %s\n  musql: cols=%v rows=%v\n  want:   cols=%v rows=%v",
					reason, vCols, vRows, tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestNaturalSelfJoinMutualRejections pins the shapes C SQLite itself
// rejects: this engine must reject them too, never answer them. Every one is a
// consequence of the shared name, and each was verified directly against
// mattn/go-sqlite3 -- these ARE the reason the accepted set above is not
// simply "any self-join".
func TestNaturalSelfJoinMutualRejections(t *testing.T) {
	path := buildNaturalSelfJoinDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		// A column left EXPOSED by both same-named items. SQLite's own "*"
		// expansion writes it as a qualified reference and reports "ambiguous
		// column name: main.ss1.b"; an explicit "ss1.b", or even a bare "b",
		// is the same error. Only the columns the join COALESCED away escape
		// it -- which is exactly why the all-columns-common spellings in the
		// parity corpus above answer.
		"SELECT * FROM ss1 JOIN ss1 USING(a)",
		"SELECT * FROM ss1 LEFT JOIN ss1 USING(a)",
		"SELECT * FROM ss1 RIGHT JOIN ss1 USING(a)",
		"SELECT ss1.b FROM ss1 JOIN ss1 USING(a)",
		"SELECT b FROM ss1 JOIN ss1 USING(a)",
		"SELECT * FROM ssm JOIN ssm USING(a)",
		"SELECT * FROM ssm JOIN ssm USING(a,b)",
		"SELECT * FROM ss1 AS x JOIN ss1 AS x USING(a)",
		"SELECT * FROM ssk JOIN ssk USING(id)",
		"SELECT * FROM ssc JOIN ssc USING(t)",
		"SELECT * FROM ss1 NATURAL JOIN ss1, ss1",

		// The pseudo-rowid is never part of a common-column set, so a shared
		// name always leaves it duplicated: "ambiguous column name:
		// ss1.rowid" even though "ss1.b", coalesced away on the second copy,
		// resolves fine. ssk's "id" IS its rowid and IS coalesced, yet
		// "ssk.rowid" is still ambiguous.
		"SELECT ss1.rowid FROM ss1 NATURAL JOIN ss1",
		"SELECT rowid FROM ss1 NATURAL JOIN ss1",
		"SELECT * FROM ss1 NATURAL JOIN ss1 WHERE rowid = 1",
		"SELECT ssk.rowid FROM ssk NATURAL JOIN ssk",
		// A VIEW has no rowid at all, so the same reference is "no such
		// column" rather than ambiguous -- rejected either way.
		"SELECT ssv.rowid FROM ssv NATURAL JOIN ssv",

		// A same-named pair inside a NON-LEADING parenthesized FROM term:
		// SQLite materializes the term, needs its rows' identity, and reports
		// its internal pseudo-rowid as ambiguous ("ambiguous column name:
		// main.ss1._ROWID_"). The LEADING spelling of the identical join is in
		// the parity corpus above and answers fine.
		"SELECT * FROM ssd, (ss1 NATURAL JOIN ss1)",
		"SELECT * FROM ssd JOIN (ss1 NATURAL JOIN ss1) ON ssd.p = a",
		"SELECT * FROM ssd LEFT JOIN (ss1 NATURAL JOIN ss1) ON ssd.p = a",
		"SELECT a FROM ssd JOIN (ss1 NATURAL JOIN ss1) ON ssd.p = a",
		"SELECT * FROM ssd NATURAL JOIN (ss1 LEFT JOIN ss1 ON 1)",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Fatalf("[%s] expected C SQLite to reject this, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected this engine to reject it too, got nil", sqlText)
		}
	}
}

// TestNaturalSelfJoinDeclined pins the shapes this engine still declines
// CLEANLY (an error, never a wrong answer) where C SQLite answers. Each
// is a deliberate, documented decline -- if one of these starts working,
// delete its line here and add it to the parity corpus above.
func TestNaturalSelfJoinDeclined(t *testing.T) {
	path := buildNaturalSelfJoinDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range []string{
		// A same-named pair alongside an ATOMIC parenthesized join GROUP (a
		// paren-grouped term containing an OUTER join, the only shape the
		// parser marks with FromItem.GroupLen). Each side of the desugared
		// condition is addressed by FROM-item INDEX, and a group is exactly
		// where a scope list's position stops being that index -- see
		// checkSelfJoinGroupSupported (engine/join.go), and
		// checkDerivedJoinSupported for the same decline over an unaliased
		// derived table.
		"SELECT * FROM (ss1 LEFT JOIN ssd ON ss1.a=ssd.p) NATURAL JOIN ss1",
		"SELECT * FROM ss1 NATURAL JOIN (ss1 LEFT JOIN ssd ON ss1.a=ssd.p)",
		"SELECT * FROM (ss1 RIGHT JOIN ssd ON ss1.a=ssd.p) NATURAL JOIN ss1",

		// (A column exposed by both same-named items used to be declined for
		// the WHOLE statement here. It is not any more: the ambiguity is per
		// REFERENCE, exactly as this block's own note predicted -- "carrying
		// the shared name into every resolution site" is what shipped. Those
		// six statements moved into naturalSelfJoinCorpus, where they now
		// agree with C SQLite cell for cell.)

		// Two DIFFERENT tables under one shared alias, where the later one
		// exposes a column the earlier lacks. The join itself is computed
		// correctly (see the count(*) spellings in the parity corpus), but
		// "*" expansion names each surviving column by its scope NAME, which
		// reaches only the first item -- so this engine reports "no such
		// column: c" for a query SQLite answers. A genuine self-join can never
		// hit it (identical column sets), and neither can an ordinary
		// distinct-name FROM clause.
		"SELECT * FROM ss1 AS x NATURAL JOIN ss2 AS x",
		"SELECT * FROM ss1 AS x JOIN ss2 AS x USING(a)",
		"SELECT * FROM ss1 NATURAL JOIN ss2 AS ss1",
		"SELECT * FROM ss2 AS ss1 NATURAL JOIN ss1",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr != nil {
			t.Fatalf("[%s] expected C SQLite to ANSWER this (the point of the decline), got %v", sqlText, cErr)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected a clean decline, got an answer -- verify it against C SQLite and move it to the parity corpus", sqlText)
		}
	}
}
