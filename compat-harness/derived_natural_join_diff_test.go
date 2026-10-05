package compat

// Tests for NATURAL and USING joins with derived tables (subqueries, views,
// CTEs). Verifies correctness of column coalescing in unaliased derived tables.

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildDerivedNaturalJoinDB builds the tables this gate needs:
//
//   - dj1/dj2 share exactly one column name ("a"), with a NULL key (never
//     matches), keys matched on only one side (LEFT/RIGHT NULL-extension) and
//     a distinct payload column each, so "SELECT *" column ORDER and NAMES are
//     observable in both join orders.
//   - dm1/dm2 share TWO column names ("a" and "b") -- a multi-column NATURAL
//     common set -- with a row that matches on "a" but NOT on "b", so a
//     condition that silently dropped one of the two common columns shows up
//     as an extra row.
//   - ddis shares NO column name with dj1: NATURAL against it must degenerate
//     to a true cross join (SQLite's own documented zero-common-column rule).
//   - dnc/dtx are the collation pair: dnc.a is declared COLLATE NOCASE, so a
//     NATURAL join against dtx.a exercises whether the derived table carries
//     its source column's collation across the subquery boundary.
//   - dv2/dvdis/dvren are VIEW references (dvren renaming its columns via the
//     CREATE VIEW column list), which desugar through the same derived-table
//     row source but keep a name of their own.
func buildDerivedNaturalJoinDB(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/derived_natural_join.sqlite"
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE dj1(a INTEGER, b TEXT)`,
		`INSERT INTO dj1 VALUES(1,'one')`,
		`INSERT INTO dj1 VALUES(2,'two')`,
		`INSERT INTO dj1 VALUES(3,'three')`, // unmatched in dj2
		`INSERT INTO dj1 VALUES(NULL,'null')`,
		`CREATE TABLE dj2(a INTEGER, c TEXT)`,
		`INSERT INTO dj2 VALUES(1,'x')`,
		`INSERT INTO dj2 VALUES(2,'y')`,
		`INSERT INTO dj2 VALUES(5,'z')`, // unmatched in dj1
		`CREATE TABLE ddis(p INTEGER, q TEXT)`,
		`INSERT INTO ddis VALUES(1,'P1')`,
		`INSERT INTO ddis VALUES(9,'P9')`,
		`CREATE TABLE dm1(a INTEGER, b TEXT, k INTEGER)`,
		`INSERT INTO dm1 VALUES(1,'one',7)`,
		`INSERT INTO dm1 VALUES(2,'two',8)`,
		`CREATE TABLE dm2(a INTEGER, b TEXT, z TEXT)`,
		`INSERT INTO dm2 VALUES(1,'one','M1')`,
		`INSERT INTO dm2 VALUES(2,'nope','M2')`, // matches on a, NOT on b
		`CREATE TABLE dnc(a TEXT COLLATE NOCASE, w TEXT)`,
		`INSERT INTO dnc VALUES('One','N1')`,
		`INSERT INTO dnc VALUES('TWO','N2')`,
		`CREATE TABLE dtx(a TEXT, y TEXT)`,
		`INSERT INTO dtx VALUES('one','X1')`,
		`INSERT INTO dtx VALUES('two','X2')`,
		`CREATE VIEW dv2 AS SELECT a, c FROM dj2`,
		`CREATE VIEW dvdis AS SELECT p, q FROM ddis`,
		`CREATE VIEW dvren(a, cc) AS SELECT a, c FROM dj2`,
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

// derivedNaturalJoinCorpus is the parity gate corpus. Every statement's
// columns, column NAMES and rows must match C SQLite exactly.
var derivedNaturalJoinCorpus = []joinCase{
	// --- derived table on the RIGHT, ALIASED ---
	{"SELECT * FROM dj1 JOIN (SELECT a, c FROM dj2) AS d USING(a)", false},
	{"SELECT * FROM dj1 JOIN (SELECT a, c FROM dj2) d USING(a)", false}, // alias without AS
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d", false},
	{"SELECT * FROM dj1 LEFT JOIN (SELECT a, c FROM dj2) AS d USING(a) ORDER BY a", true},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2) AS d ORDER BY a", true},
	{"SELECT a, b, c FROM dj1 JOIN (SELECT a, c FROM dj2) AS d USING(a)", false},
	{"SELECT d.a FROM dj1 JOIN (SELECT a, c FROM dj2) AS d USING(a)", false},

	// --- derived table on the RIGHT, UNALIASED (ColumnExpr.UsingPinned) ---
	{"SELECT * FROM dj1 JOIN (SELECT a, c FROM dj2) USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2)", false},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2) ORDER BY a", true},
	{"SELECT a, b, c FROM dj1 JOIN (SELECT a, c FROM dj2) USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT * FROM dj2)", false},

	// --- derived table on the LEFT (it becomes the REPRESENTATIVE, so "*"
	// puts the shared column in ITS position, ahead of the base table's) ---
	{"SELECT * FROM (SELECT a, c FROM dj2) AS d JOIN dj1 USING(a)", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) AS d NATURAL JOIN dj1", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) JOIN dj1 USING(a)", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) NATURAL JOIN dj1", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) NATURAL LEFT JOIN dj1 ORDER BY a", true},

	// --- derived on BOTH sides, in all four alias combinations. Both
	// unaliased used to be declined outright ("unaliased on both sides"):
	// two unnamed sides are pinned by INDEX, so they never collapse onto one
	// another the way two identically-NAMED ones do. ---
	{"SELECT * FROM (SELECT a, b FROM dj1) AS d1 NATURAL JOIN (SELECT a, c FROM dj2) AS d2", false},
	{"SELECT * FROM (SELECT a, b FROM dj1) AS d1 NATURAL JOIN (SELECT a, c FROM dj2)", false},
	{"SELECT * FROM (SELECT a, b FROM dj1) NATURAL JOIN (SELECT a, c FROM dj2) AS d2", false},
	{"SELECT * FROM (SELECT a, b FROM dj1) NATURAL JOIN (SELECT a, c FROM dj2)", false},

	// --- how the derived select list NAMES its column decides the common set:
	// an explicit alias participates, an expression-derived name ("a+0") does
	// not, and NATURAL against it degenerates to a cross join. ---
	{"SELECT * FROM dj1 JOIN (SELECT a AS a, c FROM dj2) AS d USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a+0 AS a, c FROM dj2) AS d", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a+0, c FROM dj2) AS d ORDER BY a, 3", true},
	{"SELECT * FROM dj1 JOIN (SELECT dj2.a, c FROM dj2) AS d USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT c AS a, a AS c FROM dj2) AS d", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a AS A, c FROM dj2)", false}, // case-insensitive match, derived spelling reported

	// --- MULTI-column common set (both "a" and "b") ---
	{"SELECT * FROM dm1 NATURAL JOIN (SELECT a, b, z FROM dm2) AS d", false},
	{"SELECT * FROM dm1 NATURAL JOIN (SELECT a, b, z FROM dm2)", false},
	{"SELECT * FROM dm1 JOIN (SELECT a, b, z FROM dm2) AS d USING(a, b)", false},
	{"SELECT * FROM dm1 NATURAL LEFT JOIN (SELECT a, b, z FROM dm2) AS d ORDER BY a", true},

	// --- NATURAL with an EMPTY common-column set: a true cross join, every
	// column kept (SQLite's own documented fallback) ---
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT p, q FROM ddis) AS d ORDER BY a, p", true},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT p, q FROM ddis) ORDER BY a, p", true},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT p, q FROM ddis) AS d ORDER BY a, p", true},
	{"SELECT * FROM dj1 NATURAL JOIN dvdis ORDER BY a, p", true},

	// --- an empty derived table ---
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2 WHERE 0)", false},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2 WHERE 0) ORDER BY a", true},

	// --- the derived column's COLLATION crosses the subquery boundary, and
	// decides which rows the coalesced comparison matches ---
	{"SELECT * FROM dtx NATURAL JOIN (SELECT a, w FROM dnc) AS d", false},
	{"SELECT * FROM dnc NATURAL JOIN (SELECT a, y FROM dtx) AS d", false},
	{"SELECT * FROM dtx JOIN (SELECT a COLLATE NOCASE AS a, w FROM dnc) AS d USING(a)", false},

	// --- VIEW references ---
	{"SELECT * FROM dj1 JOIN dv2 USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN dv2", false},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN dv2 ORDER BY a", true},
	{"SELECT * FROM dv2 NATURAL JOIN dj1", false},
	{"SELECT * FROM dj1 JOIN dv2 AS w USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN dvren", false}, // CREATE VIEW column list renames "c" to "cc"
	{"SELECT dv2.a, dj1.b FROM dj1 NATURAL JOIN dv2", false},

	// --- CTE references (only reachable through QueryArgs, which binds WITH) ---
	{"WITH cte(a,c) AS (SELECT a, c FROM dj2) SELECT * FROM dj1 JOIN cte USING(a)", false},
	{"WITH cte(a,c) AS (SELECT a, c FROM dj2) SELECT * FROM dj1 NATURAL JOIN cte", false},
	{"WITH cte(a,c) AS (SELECT a, c FROM dj2) SELECT * FROM cte NATURAL JOIN dj1", false},
	{"WITH cte(a,c) AS (SELECT a, c FROM dj2) SELECT * FROM dj1 NATURAL LEFT JOIN cte ORDER BY a", true},
	{"WITH cte(a,c) AS (SELECT a, c FROM dj2) SELECT * FROM dj1 JOIN cte AS w USING(a)", false},
	{"WITH RECURSIVE r(a) AS (SELECT 1 UNION ALL SELECT a+1 FROM r WHERE a<3) SELECT * FROM dj1 NATURAL JOIN r ORDER BY a", true},

	// --- LEFT/RIGHT/FULL: which side's value the coalesced column shows on an
	// unmatched row. LEFT keeps the representative's; RIGHT/FULL must fall back
	// to the RIGHT side's own (COALESCE, not a static pick) -- see
	// tableScope.coalesceFallback. ---
	{"SELECT * FROM dj1 NATURAL RIGHT JOIN (SELECT a, c FROM dj2) AS d", false},
	{"SELECT * FROM dj1 RIGHT JOIN (SELECT a, c FROM dj2) AS d USING(a)", false},
	{"SELECT * FROM dj1 NATURAL FULL JOIN (SELECT a, c FROM dj2) AS d", false},
	{"SELECT * FROM dj1 NATURAL RIGHT JOIN (SELECT a, c FROM dj2)", false},
	{"SELECT * FROM dj1 NATURAL RIGHT JOIN dv2", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) NATURAL RIGHT JOIN dj1", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) RIGHT JOIN dj1 USING(a)", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) NATURAL FULL JOIN dj1", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) AS d NATURAL FULL JOIN dj1", false},
	{"SELECT * FROM dj1 RIGHT JOIN (SELECT * FROM dj2, ddis) USING (a) ORDER BY a, p", true},

	// --- chains: coalescing must keep resolving to the leftmost representative
	// across more than one hop, with a derived table anywhere in the chain ---
	{"SELECT * FROM dj1 JOIN dj2 USING(a) JOIN (SELECT a, p FROM ddis, dj2 WHERE p=a) AS d USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d NATURAL JOIN dv2", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d JOIN ddis ON ddis.p = a", false},
	{"SELECT * FROM (SELECT a, c FROM dj2) AS d NATURAL JOIN dj1 NATURAL JOIN (SELECT a, b FROM dj1) AS e", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) NATURAL JOIN (SELECT a, p FROM ddis, dj2 WHERE p=a)", false},
	{"SELECT * FROM dj1 JOIN dj2 USING(a) NATURAL JOIN (SELECT a, p FROM ddis, dj2 WHERE p=a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d, ddis ORDER BY a, p", true},
	{"SELECT * FROM ddis, dj1 NATURAL JOIN (SELECT a, c FROM dj2) ORDER BY p, a", true},
	{"SELECT * FROM (SELECT a, c FROM dj2) NATURAL JOIN dj1 NATURAL JOIN dm1", false},

	// --- a parenthesized join term with no OUTER join in it, and whose own
	// OUTWARD attachment is NATURAL (not a trailing ON/USING -- NATURAL is
	// part of the join OPERATOR itself, parse.y's on_using never sees it) is
	// FLATTENED by the parser (no GroupLen marker), so it is an ordinary
	// chain, not the declined group shape. An outward ON/USING is a
	// DIFFERENT story -- see the two moved entries' new home in
	// TestDerivedNaturalJoinDeclined, below checkFlattenSafe's own fix
	// (sql_parser.go): they are ATOMIC groups now (their own outward
	// attachment carries an explicit ON/USING), so they hit
	// checkDerivedJoinSupported same as any other atomic-group-plus-
	// derived-table-plus-NATURAL/USING shape.
	{"SELECT * FROM (dj1 NATURAL JOIN (SELECT a, c FROM dj2)) NATURAL JOIN ddis ORDER BY a, p", true},

	// --- the shape nested inside another query ---
	{"SELECT * FROM (SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2)) ORDER BY a", true},
	{"SELECT (SELECT count(*) FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2))", false},
	{"SELECT * FROM ddis WHERE p IN (SELECT a FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2))", false},
	{"SELECT a FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) UNION ALL SELECT p FROM ddis ORDER BY 1", true},

	// --- aggregation / DISTINCT / WHERE over the coalesced column ---
	{"SELECT a, count(*) FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d GROUP BY a ORDER BY a", true},
	{"SELECT DISTINCT a FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2) AS d ORDER BY a", true},
	{"SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2) AS d WHERE a > 1 ORDER BY a", true},
	{"SELECT count(*) FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) ORDER BY a DESC", true},

	// --- the coalesced-away side exposing the SAME column name twice. Real
	// SQLite hides only the FIRST occurrence and keeps the rest under the ":N"
	// duplicate-name spelling sqlite3ColumnsFromExprList gives a subquery's
	// column list ("a","b","a:1"). These two were declined here until that
	// renaming was ported (r32mUniqueColumnNames, engine/query.go). ---
	{"SELECT * FROM dj1 JOIN (SELECT a, a FROM dj2) AS d USING(a)", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, a FROM dj2)", false},

	// --- a derived table carrying its own ORDER BY/LIMIT, or a compound body ---
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2 ORDER BY a LIMIT 2) AS d", false},
	{"SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2 UNION ALL SELECT 3, 'q') AS d ORDER BY a", true},

	// --- a derived table inside an ATOMIC parenthesized join GROUP
	// (FromItem.GroupLen -- a paren-grouped join term containing an OUTER
	// join, checkFlattenSafe, sql_parser.go), narrowly re-enabled by
	// checkDerivedJoinSupported's safe carve-out (engine/join.go): the group
	// is the FROM clause's SOLE top-level item (no outer sibling) and none
	// of its own members is itself a NESTED group. See
	// TestDerivedNaturalJoinDeclined for the shapes that stay declined
	// (a sibling outside the group, or a group nested inside a group).
	{"SELECT * FROM (dj1 RIGHT JOIN (SELECT a, c FROM dj2) AS d USING(a))", false},
	{"SELECT * FROM (dj1 RIGHT JOIN (SELECT a, c FROM dj2) USING(a))", false}, // unaliased
	{"SELECT * FROM (dj1 NATURAL RIGHT JOIN (SELECT a, c FROM dj2) AS d)", false},
	{"SELECT * FROM (dj1 FULL JOIN (SELECT a, c FROM dj2) AS d USING(a))", false},
	{"SELECT * FROM (dj1 RIGHT JOIN (SELECT a, p FROM ddis, dj2 WHERE p=a) USING(a))", false}, // multi-table derived body
}

// TestDerivedNaturalJoinParity is the hard gate: every corpus statement must
// produce identical columns, column names and rows through musql and real C
// SQLite.
func TestDerivedNaturalJoinParity(t *testing.T) {
	path := buildDerivedNaturalJoinDB(t)

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
	for _, tc := range derivedNaturalJoinCorpus {
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
	t.Logf("derived-table NATURAL/USING parity gate: %d statements, wrong=%d", total, wrong)
	if wrong != 0 {
		t.Fatalf("derived-table NATURAL/USING parity gate FAILED: wrong=%d (must be 0)", wrong)
	}
}

// TestDerivedNaturalJoinStarCoalescing is the hand-verified "SELECT *" check
// this shape's own doc comment calls out as where the bugs hide: the coalesced
// column must appear EXACTLY ONCE, in the REPRESENTATIVE (leftmost) source's
// position, under its own name -- which for a derived-table representative
// means the derived select list's spelling, in the derived table's slot of the
// output, ahead of the base table's columns. Cross-engine agreement alone
// would not catch a coalescing bug that happened to sit in shared code, so
// each case asserts an explicit column list and row set, first checked against
// C SQLite (so a wrong expectation fails the test rather than blessing
// itself) and then against musql.
func TestDerivedNaturalJoinStarCoalescing(t *testing.T) {
	path := buildDerivedNaturalJoinDB(t)

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
		// Derived table on the RIGHT: "a" is shown once, in dj1's (the
		// representative's) position, then dj1's own remaining column, then
		// the derived table's.
		{
			sql:      "SELECT * FROM dj1 NATURAL JOIN (SELECT a, c FROM dj2) ORDER BY a",
			wantCols: []string{"a", "b", "c"},
			wantRows: [][]string{
				{"I:1", "T:one", "T:x"},
				{"I:2", "T:two", "T:y"},
			},
		},
		// Derived table on the LEFT: it is now the representative, so "a"
		// leads, followed by the derived table's own "c", and only then dj1's
		// "b" -- a different column ORDER for the same underlying join.
		{
			sql:      "SELECT * FROM (SELECT a, c FROM dj2) NATURAL JOIN dj1 ORDER BY a",
			wantCols: []string{"a", "c", "b"},
			wantRows: [][]string{
				{"I:1", "T:x", "T:one"},
				{"I:2", "T:y", "T:two"},
			},
		},
		// NATURAL LEFT JOIN: the coalesced column keeps the LEFT side's value
		// on a NULL-extended row (a=3 stays 3, never NULL), and dj1's own
		// NULL key matches nothing.
		{
			sql:      "SELECT * FROM dj1 NATURAL LEFT JOIN (SELECT a, c FROM dj2) AS d ORDER BY a",
			wantCols: []string{"a", "b", "c"},
			wantRows: [][]string{
				{"N", "T:null", "N"},
				{"I:1", "T:one", "T:x"},
				{"I:2", "T:two", "T:y"},
				{"I:3", "T:three", "N"},
			},
		},
		// NATURAL RIGHT JOIN: the representative (dj1) is the side that gets
		// NULL-extended, so the coalesced column must fall back to the derived
		// table's own value -- a=5 with b NULL, not a NULL.
		{
			sql:      "SELECT * FROM dj1 NATURAL RIGHT JOIN (SELECT a, c FROM dj2) AS d ORDER BY a",
			wantCols: []string{"a", "b", "c"},
			wantRows: [][]string{
				{"I:1", "T:one", "T:x"},
				{"I:2", "T:two", "T:y"},
				{"I:5", "N", "T:z"},
			},
		},
		// Two common columns: both hidden on the derived side, so the output
		// is 4 columns (a, b, k, z), and dm2's a=2/b='nope' row -- which
		// matches on "a" alone -- must NOT survive.
		{
			sql:      "SELECT * FROM dm1 NATURAL JOIN (SELECT a, b, z FROM dm2) ORDER BY a",
			wantCols: []string{"a", "b", "k", "z"},
			wantRows: [][]string{
				{"I:1", "T:one", "I:7", "T:M1"},
			},
		},
		// No common column at all: a cross join, nothing hidden or renamed.
		{
			sql:      "SELECT * FROM dj1 NATURAL JOIN (SELECT p, q FROM ddis) WHERE a=1 ORDER BY p",
			wantCols: []string{"a", "b", "p", "q"},
			wantRows: [][]string{
				{"I:1", "T:one", "I:1", "T:P1"},
				{"I:1", "T:one", "I:9", "T:P9"},
			},
		},
		// The derived select list's own spelling names the output column: an
		// expression with no alias is named by its source text ("a+0"), which
		// therefore shares no name with dj1.a and produces a cross join with
		// FIVE distinct columns, two of them displaying "a".
		{
			sql:      "SELECT * FROM dj1 NATURAL JOIN (SELECT a+0, c FROM dj2) AS d WHERE b='one' ORDER BY 3",
			wantCols: []string{"a", "b", "a+0", "c"},
			wantRows: [][]string{
				{"I:1", "T:one", "I:1", "T:x"},
				{"I:1", "T:one", "I:2", "T:y"},
				{"I:1", "T:one", "I:5", "T:z"},
			},
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
				t.Errorf("coalescing mismatch: %s\n  musql: cols=%v rows=%v\n  want:   cols=%v rows=%v",
					reason, vCols, vRows, tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestDerivedNaturalJoinMutualRejections pins the shapes C SQLite itself
// rejects: this engine must reject them too, never answer them.
func TestDerivedNaturalJoinMutualRejections(t *testing.T) {
	path := buildDerivedNaturalJoinDB(t)

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
		// USING names a column the derived table does not expose.
		"SELECT * FROM dj1 JOIN (SELECT p, q FROM ddis) AS d USING(a)",
		// ... including when the derived table's own NATURAL join already
		// coalesced it away, so it is not in that table's output at all.
		"SELECT * FROM ddis JOIN (SELECT * FROM dj1 NATURAL JOIN dj2) AS x USING(a)",
		// An UNALIASED derived table is not qualifiable: SQLite reports
		// "no such column: d.a" even though a table of some name is there.
		"SELECT d.a FROM dj1 JOIN (SELECT a, c FROM dj2) USING(a)",
		// The subquery's own source table name is likewise not in scope
		// outside it.
		"SELECT dj2.a FROM (SELECT a, c FROM dj2) NATURAL JOIN dj1",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Fatalf("[%s] expected C SQLite to reject this, got nil", sqlText)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected this engine to reject it too, got nil", sqlText)
		}
	}
}

// TestDerivedNaturalJoinDeclined pins the shapes this engine still declines
// CLEANLY (an error, never a wrong answer) where C SQLite answers. Each
// is a deliberate, documented decline, not an accident -- if one of these
// starts working, delete its line here and add it to the parity corpus above.
func TestDerivedNaturalJoinDeclined(t *testing.T) {
	path := buildDerivedNaturalJoinDB(t)

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
		// A derived table alongside an ATOMIC parenthesized join GROUP -- a
		// paren-grouped join term containing an OUTER join, OR one whose own
		// OUTWARD attachment carries an alias or an explicit ON/USING, is what
		// the parser marks with FromItem.GroupLen (checkFlattenSafe,
		// sql_parser.go). checkDerivedJoinSupported (engine/join.go) now has a
		// narrow safe carve-out for a group that is the FROM clause's SOLE
		// top-level item with no nested sub-group of its own (see the parity
		// corpus above) -- these remain declined because they fall OUTSIDE
		// that carve-out: the first two put the derived table OUTSIDE the
		// group entirely (an ordinary sibling attached via NATURAL JOIN, not
		// itself inside the parens), so lifting the decline there is
		// unverified territory; the third and fourth put a base table BEFORE
		// the group at the top level, which is also outside the carve-out.
		"SELECT * FROM (dj1 LEFT JOIN ddis ON dj1.a=ddis.p) NATURAL JOIN (SELECT a, c FROM dj2)",
		"SELECT * FROM (dj1 LEFT JOIN ddis ON dj1.a=ddis.p) NATURAL JOIN (SELECT a, c FROM dj2) AS d",
		"SELECT * FROM dm1 JOIN (dj1 LEFT JOIN (SELECT a, c FROM dj2) USING(a)) USING(a)",
		// A derived table as the group's own CONNECTOR (its first/leading
		// member) is a SEPARATE decline, unrelated to
		// checkDerivedJoinSupported: resolveGroupSource
		// (vdbe_join_codegen.go) declines any derived table, vtab, or CTE
		// reference in that position outright ("derived table as a
		// parenthesized join group's connector"), regardless of USING/
		// NATURAL, because it would need a second, cursor-less identity
		// computation this package's own mined corpus never exercises.
		"SELECT * FROM ((SELECT a, c FROM dj2) RIGHT JOIN dj1 USING(a))",
		// A NESTED group (a group within a group) containing a derived
		// table: resolveGroupSource's memberScopes rebase a nested
		// sub-group's owner-chain positions by its own colBases, but an
		// unaliased derived table's plain UsingPinnedItem index is never
		// part of that rebasing -- verified directly, this returns the right
		// VALUES but the WRONG coalesced column NAME ("c0:1" where the
		// oracle reports "c0:3") once the outer decline is lifted, a silent
		// wrong answer -- so it stays declined.
		"SELECT * FROM (dm1 RIGHT JOIN (dj1 RIGHT JOIN (SELECT a, c FROM dj2) USING(a)) USING(a))",
		// Moved from the parity corpus above: an all-associative (NATURAL
		// JOIN, no outer join) group whose own OUTWARD attachment carries an
		// explicit ON/USING -- checkFlattenSafe's outward-attachment fix
		// makes this atomic now too, same as an internal outer join always
		// did.
		"SELECT * FROM ddis JOIN (dj1 NATURAL JOIN (SELECT a, c FROM dj2) AS d) ON ddis.p = dj1.a",
		"SELECT * FROM dj1 JOIN (dj2 NATURAL JOIN (SELECT a, p FROM ddis, dj2 WHERE p=a) AS d) USING(a)",
	} {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr != nil {
			t.Fatalf("[%s] expected C SQLite to ANSWER this (the point of the decline), got %v", sqlText, cErr)
		}
		if _, _, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] expected a clean decline, got an answer -- verify it against C SQLite and move it to the parity corpus", sqlText)
		}
	}
}
