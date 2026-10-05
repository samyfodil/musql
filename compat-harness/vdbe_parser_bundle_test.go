// This file gates three parser/pragma fixes: string literal aliases, unary
// bitwise-NOT, and session-tuning PRAGMAs (count_changes, encoding,
// reverse_unordered_selects).
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// ---- (1) string alias ----

func TestStringAliasMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testStringAliasScenario(t, pageSize)
		})
	}
}

func testStringAliasScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("stralias_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	cgodb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()

	schema := []string{
		"CREATE TABLE t1(x INTEGER, y TEXT)",
		"INSERT INTO t1 VALUES (5, 'hi'), (NULL, NULL)",
	}
	for _, s := range schema {
		if err := db.Exec(s); err != nil {
			t.Fatalf("engine schema setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo schema setup %q: %v", s, err)
		}
	}

	cases := []string{
		// ---- result-column alias: AS-prefixed and bare ----
		`SELECT x AS 'foo' FROM t1`,
		`SELECT x 'foo' FROM t1`,
		`SELECT x AS '' FROM t1`,       // explicit empty-string alias: still an alias, distinct from none
		`SELECT y AS 'bar', x FROM t1`, // string alias alongside an ordinary column, mixed select list

		// ---- FROM-clause table alias: AS-prefixed and bare ----
		`SELECT * FROM t1 AS 'ta'`,
		`SELECT * FROM t1 'ta'`,
		`SELECT ta.x FROM t1 AS 'ta'`,
		`SELECT x 'foo' FROM t1 AS 'ta'`, // string result-column alias AND string table alias together
		`SELECT * FROM t1 AS '' LIMIT 1`,

		// ---- derived-table alias: AS-prefixed and bare ----
		`SELECT * FROM (SELECT 1 AS c1) AS 'sub'`,
		`SELECT * FROM (SELECT 1 AS c1) 'sub'`,
		`SELECT sub.c1 FROM (SELECT 1 AS c1) AS 'sub'`,

		// ---- JOIN: both sides string-aliased ----
		`SELECT ta.x, tb.x FROM t1 AS 'ta' JOIN t1 AS 'tb' ON ta.x = tb.x`,

		// ---- ordinary identifier alias still works alongside the new string
		// form (regression: aliasTokenText must not have broken the existing
		// tkIdent path) ----
		`SELECT x AS foo FROM t1`,
		`SELECT * FROM t1 AS ta`,
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(db, s)
			if panicked {
				t.Fatalf("engine panicked: %v", panicVal)
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, s)
			if (qerr == nil) != (cerr == nil) {
				t.Fatalf("engine err=%v (cols=%v rows=%v), cgo err=%v (cols=%v rows=%v)", qerr, gotCols, gotRows, cerr, cgoCols, cgoRows)
			}
			if qerr != nil {
				return // both declined identically -- never counted wrong
			}
			ok, reason := queryResultsMatch(gotCols, gotRows, cgoCols, cgoRows, false)
			if !ok {
				t.Errorf("mismatch: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", reason, gotCols, gotRows, cgoCols, cgoRows)
			}
		})
	}
}

// ---- (2) unary bitwise-NOT ("~") ----

// bitNotScalarCases is the FROM-less differential gate for "~", covering
// every operand storage class and coercion rule verified directly against
// mattn/go-sqlite3 (see bitNotValueUnary's doc comment, engine/value_arith.go):
// NULL propagation, INTEGER (including exact Min/MaxInt64 boundaries), REAL
// (truncation toward zero, saturating at the +/-2^63 boundary), and TEXT/BLOB
// (an INTEGER-ONLY leading-prefix parse -- NOT a full float parse -- so
// "5.9e10" reads only "5", never 59000000000).
var bitNotScalarCases = []string{
	`SELECT ~5`,
	`SELECT ~0`,
	`SELECT ~-5`,
	`SELECT ~NULL`,
	`SELECT ~'5'`,
	`SELECT ~'-5'`,
	`SELECT ~'+5'`,
	`SELECT ~'abc'`,    // no numeric prefix at all: coerces to 0
	`SELECT ~'5.9'`,    // integer-only prefix: "5", NOT the full "5.9"
	`SELECT ~'5.9e10'`, // integer-only prefix: "5", NOT 59000000000
	`SELECT ~'  5  '`,  // leading/trailing whitespace
	`SELECT ~'1e300'`,  // integer-only prefix: "1", NOT a saturated huge value
	`SELECT ~3.5`,      // REAL: truncates toward zero (int(3.5)=3, ~3=-4)
	`SELECT ~3.7`,
	`SELECT ~X'0102'`,                 // BLOB: no numeric prefix, coerces to 0
	`SELECT ~9223372036854775807`,     // exact MaxInt64
	`SELECT ~(-9223372036854775808)`,  // exact MinInt64
	`SELECT ~9.223372036854776e18`,    // REAL >= 2^63: saturates to MaxInt64 first
	`SELECT ~(-9.223372036854776e18)`, // REAL == -2^63 exactly: no saturation
	`SELECT ~~5`,                      // double negation
	`SELECT -~5`,                      // mixed unary composition
	`SELECT ~-~5`,
	`SELECT 1 + ~2`, // "~" inside a larger expression
	`SELECT ~2 + 1`,
	`SELECT ~(1+2)`,
	`SELECT typeof(~5)`,
	`SELECT typeof(~NULL)`,
	`SELECT typeof(~'5')`,
	`SELECT typeof(~3.7)`, // always INTEGER, unlike unary "-" which preserves REAL
	`SELECT typeof(~X'0102')`,
}

func TestBitNotMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("bitnot_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()

			for _, s := range bitNotScalarCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
		})
	}
}

// TestBitNotColumnRefs covers "~" applied to an actual COLUMN reference
// (INTEGER, TEXT, and a NULL row), which bitNotScalarCases -- being entirely
// FROM-less -- can't express, plus its own auto-generated result-column name
// ("~x", matching C SQLite's own verbatim-source-text naming), which
// compareOneScalar deliberately doesn't check (it only compares the scalar
// VALUE) -- this uses the column-name-aware queryResultsMatch path instead,
// same as TestStringAliasMatchesCSQLite above.
func TestBitNotColumnRefs(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testBitNotColumnRefsScenario(t, pageSize)
		})
	}
}

func testBitNotColumnRefsScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("bitnotcol_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	cgodb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()

	schema := []string{
		"CREATE TABLE t1(x INTEGER, y TEXT, z REAL)",
		"INSERT INTO t1 VALUES (5, '7', 3.5), (NULL, NULL, NULL), (-5, 'abc', -3.5)",
	}
	for _, s := range schema {
		if err := db.Exec(s); err != nil {
			t.Fatalf("engine schema setup %q: %v", s, err)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo schema setup %q: %v", s, err)
		}
	}

	cases := []string{
		`SELECT ~x FROM t1`,
		`SELECT ~y FROM t1`,
		`SELECT ~z FROM t1`,
		`SELECT x, ~x FROM t1 WHERE ~x = -6`,
		`SELECT x FROM t1 ORDER BY ~x`,
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(db, s)
			if panicked {
				t.Fatalf("engine panicked: %v", panicVal)
			}
			cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, s)
			if (qerr == nil) != (cerr == nil) {
				t.Fatalf("engine err=%v (cols=%v rows=%v), cgo err=%v (cols=%v rows=%v)", qerr, gotCols, gotRows, cerr, cgoCols, cgoRows)
			}
			if qerr != nil {
				return
			}
			orderSensitive := s == `SELECT x FROM t1 ORDER BY ~x`
			ok, reason := queryResultsMatch(gotCols, gotRows, cgoCols, cgoRows, orderSensitive)
			if !ok {
				t.Errorf("mismatch: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", reason, gotCols, gotRows, cgoCols, cgoRows)
			}
		})
	}
}

// ---- (3) count_changes / encoding / reverse_unordered_selects pragmas ----

// TestPragmaBundleNoop covers the three newly-accepted no-op EXEC pragmas'
// setter forms (every value spelling SQLite itself accepts: "=1"/"=0",
// "=on"/"=off", "(1)"), requiring the engine accept EXACTLY when C SQLite
// does (never silently accepting something C SQLite itself rejects, and
// vice versa) -- mirroring compat-harness/tcl_test.go's own exec-path
// pass/fail rule (both sides must error identically, see runTCLSegment's
// tclClassifyExecErr handling), since that TCL-corpus exec path is the one
// this fix is actually gated by (gate 4).
func TestPragmaBundleNoop(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testPragmaBundleNoopScenario(t, pageSize)
		})
	}
}

func testPragmaBundleNoopScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("pragmabundle_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	cgodb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer cgodb.Close()
	cgodb.SetMaxOpenConns(1)

	if err := db.Exec("CREATE TABLE t1(a INTEGER)"); err != nil {
		t.Fatalf("engine schema setup: %v", err)
	}
	if _, err := cgodb.Exec("CREATE TABLE t1(a INTEGER)"); err != nil {
		t.Fatalf("cgo schema setup: %v", err)
	}

	setterStmts := []string{
		"PRAGMA count_changes=1",
		"PRAGMA count_changes=0",
		"PRAGMA count_changes=on",
		"PRAGMA count_changes=off",
		"PRAGMA count_changes(1)",
		"PRAGMA count_changes",
		"PRAGMA encoding='UTF-8'",
		"PRAGMA encoding",
		"PRAGMA reverse_unordered_selects=1",
		"PRAGMA reverse_unordered_selects=0",
		"PRAGMA reverse_unordered_selects=on",
		"PRAGMA reverse_unordered_selects=off",
		"PRAGMA reverse_unordered_selects(1)",
		"PRAGMA reverse_unordered_selects",
	}
	for _, s := range setterStmts {
		t.Run(s, func(t *testing.T) {
			engineErr := db.Exec(s)
			_, cgoErr := cgodb.Exec(s)
			if (engineErr == nil) != (cgoErr == nil) {
				t.Fatalf("%s: engine err=%v, cgo err=%v", s, engineErr, cgoErr)
			}
		})
	}

	// A later, ordinary (non-PRAGMA) statement must still succeed identically
	// on both sides -- proving the no-op accept above genuinely left both
	// engines' subsequent behavior unaffected for anything this package's
	// gates ever compare (see pragma.go's package doc comment on why
	// reverse_unordered_selects's genuine row-order effect is still safe
	// here: an unordered query's row order is never compared strictly).
	if err := db.Exec("INSERT INTO t1 VALUES (1),(2),(3)"); err != nil {
		t.Fatalf("engine post-pragma INSERT: %v", err)
	}
	if _, err := cgodb.Exec("INSERT INTO t1 VALUES (1),(2),(3)"); err != nil {
		t.Fatalf("cgo post-pragma INSERT: %v", err)
	}
	gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(db, "SELECT a FROM t1")
	if panicked {
		t.Fatalf("engine panicked: %v", panicVal)
	}
	if qerr != nil {
		t.Fatalf("engine post-pragma SELECT: %v", qerr)
	}
	cgoCols, cgoRows, cerr := tclRunCGOQuery(cgodb, "SELECT a FROM t1")
	if cerr != nil {
		t.Fatalf("cgo post-pragma SELECT: %v", cerr)
	}
	// Order-INsensitive: reverse_unordered_selects=1 is still active on the
	// cgo connection (a real, live effect this engine's own no-op does not
	// reproduce), so only the row MULTISET is required to match, exactly
	// like every other unordered query this package's gates compare.
	if ok, reason := queryResultsMatch(gotCols, gotRows, cgoCols, cgoRows, false); !ok {
		t.Errorf("post-pragma SELECT mismatch: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v", reason, gotCols, gotRows, cgoCols, cgoRows)
	}
}

// TestPragmaEncodingQuery covers PRAGMA encoding's QUERY (getter) form, which
// (unlike count_changes/reverse_unordered_selects) IS implemented and must
// answer byte-exactly: C SQLite reports "UTF-8" for any database this
// engine could plausibly have created or opened (verified directly against
// mattn/go-sqlite3; this engine's own write path never produces any other
// encoding -- see pragma.go's package doc comment).
func TestPragmaEncodingQuery(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("pragmaenc_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			defer db.Close()
			if err := db.Exec("CREATE TABLE t1(a INTEGER)"); err != nil {
				t.Fatalf("engine schema setup: %v", err)
			}

			sdb, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()
			if _, err := sdb.Exec("CREATE TABLE t1(a INTEGER)"); err != nil {
				t.Fatalf("cgo schema setup: %v", err)
			}

			rp, err := db.SnapshotPager()
			if err != nil {
				t.Fatalf("SnapshotPager: %v", err)
			}
			defer rp.Close()

			assertPragmaEqual(t, rp, sdb, "PRAGMA encoding")
		})
	}
}
