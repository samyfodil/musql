// Tests STRICT table support: parse-time validation of column types and
// runtime enforcement of strict type checking on INSERT and UPDATE.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/driver"
)

// strictPair opens a fresh engine writer over a temp file plus an in-memory
// real-SQLite oracle, for a script driven through execPlainBoth.
func strictPair(t *testing.T, name string) (*engine.Session, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	sdb.SetMaxOpenConns(1)
	t.Cleanup(func() { sdb.Close() })
	return db, sdb, path
}

// dumpQuery runs q and returns every row rendered as strings, for comparing
// what a musql-written file actually holds against the oracle's own copy.
func dumpQuery(t *testing.T, db *sql.DB, q string) [][]string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = normalizeAny(v)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func requireSameDump(t *testing.T, label string, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: row count %d, want %d\n  got:  %v\n  want: %v", label, len(got), len(want), got, want)
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d width %d, want %d", label, i, len(got[i]), len(want[i]))
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("%s: row %d col %d = %q, want %q\n  got:  %v\n  want: %v",
					label, i, j, got[i][j], want[i][j], got, want)
			}
		}
	}
}

// TestStrictTableCreateTimeRules gates the CREATE TABLE half: which declared
// datatypes a STRICT table accepts, and the two distinct rejections
// ("unknown datatype" vs "missing datatype") C SQLite gives, byte for
// byte.
func TestStrictTableCreateTimeRules(t *testing.T) {
	db, sdb, _ := strictPair(t, "strict_create")

	// The six legal spellings, case-insensitively, plus the three quoted
	// forms SQLite dequotes before matching (verified directly: `a "TEXT"`,
	// `a [TEXT]` and `a 'TEXT'` are all a TEXT column).
	for i, ty := range []string{
		"INT", "INTEGER", "REAL", "TEXT", "BLOB", "ANY",
		"int", "integer", "Real", "TeXt", "Blob", "any",
		`"TEXT"`, "[TEXT]", "'TEXT'",
	} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("CREATE TABLE ok%d (a %s) STRICT", i, ty))
	}

	// Everything else is `unknown datatype for <tbl>.<col>: "<type>"` --
	// including SQLite's own ordinary-table type names, a parenthesized
	// width on an OTHERWISE legal name, and a quoted illegal one.
	for i, ty := range []string{
		"VARCHAR(10)", "NUMERIC", "DOUBLE", "FLOAT", "BOOLEAN", "DATE", "DATETIME",
		"BIGINT", "CLOB", "STRING", "INT8", "NVARCHAR(100)", "DECIMAL(10,5)",
		"INT(3)", "TEXT(5)", "ANY(1)", `"VARCHAR"`, "[VARCHAR]", "'VARCHAR'",
	} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("CREATE TABLE bad%d (a %s) STRICT", i, ty))
	}

	// A column with no declared type at all is its own, different error.
	execPlainBoth(t, db, sdb, "CREATE TABLE nt1 (a) STRICT")
	execPlainBoth(t, db, sdb, "CREATE TABLE nt2 (a INT, b) STRICT")
	// ... and stays perfectly legal without the STRICT keyword.
	execPlainBoth(t, db, sdb, "CREATE TABLE nt3 (a, b VARCHAR(10))")

	// The tail itself: the two options are order-independent, comma-
	// separated, and may repeat.
	execPlainBoth(t, db, sdb, "CREATE TABLE ts1 (a INT PRIMARY KEY) STRICT, WITHOUT ROWID")
	execPlainBoth(t, db, sdb, "CREATE TABLE ts2 (a INT PRIMARY KEY) WITHOUT ROWID, STRICT")
	execPlainBoth(t, db, sdb, "CREATE TABLE ts3 (a INT) STRICT, STRICT")
	execPlainBoth(t, db, sdb, "CREATE TABLE ts4 (a INT) strict")
	// STRICT is not a reserved word: it stays usable as a column name, and a
	// table may be both named-after and made STRICT at once.
	execPlainBoth(t, db, sdb, "CREATE TABLE ts5 (a INT, STRICT INT)")
	execPlainBoth(t, db, sdb, "CREATE TABLE ts6 (STRICT INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO ts6 VALUES ('nope')")
}

// TestStrictTablePrimaryKeyIsNotNull gates STRICT's implicit "the PRIMARY
// KEY is NOT NULL", including the ONE exemption: the INTEGER PRIMARY KEY
// rowid alias, where NULL still means "auto-assign".
func TestStrictTablePrimaryKeyIsNotNull(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_pk")

	// Column-level PRIMARY KEY on a non-rowid-alias type: implicitly NOT NULL.
	execPlainBoth(t, db, sdb, "CREATE TABLE pk1 (a INT PRIMARY KEY, b TEXT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO pk1 VALUES (NULL, 'x')")
	// The identical table WITHOUT the STRICT keyword accepts that row --
	// which is what makes the rule STRICT's, not PRIMARY KEY's.
	execPlainBoth(t, db, sdb, "CREATE TABLE pk1n (a INT PRIMARY KEY, b TEXT)")
	execPlainBoth(t, db, sdb, "INSERT INTO pk1n VALUES (NULL, 'x')")

	// The rowid alias is exempt: NULL auto-assigns, exactly as always.
	execPlainBoth(t, db, sdb, "CREATE TABLE pk2 (a INTEGER PRIMARY KEY, b TEXT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO pk2 VALUES (NULL, 'x')")
	execPlainBoth(t, db, sdb, "INSERT INTO pk2 VALUES (NULL, 'y')")

	// A table-level PRIMARY KEY marks EVERY one of its columns.
	execPlainBoth(t, db, sdb, "CREATE TABLE pk3 (a TEXT, b TEXT, PRIMARY KEY(a,b)) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO pk3 VALUES (NULL, 'x')")
	execPlainBoth(t, db, sdb, "INSERT INTO pk3 VALUES ('x', NULL)")
	execPlainBoth(t, db, sdb, "INSERT INTO pk3 VALUES ('x', 'y')")

	// The implicit NOT NULL behaves like any other: an explicit OR IGNORE
	// skips the row instead of failing (unlike a type violation, which is
	// unconditional -- see TestStrictTableRuntimeTypeChecks).
	execPlainBoth(t, db, sdb, "INSERT OR IGNORE INTO pk1 VALUES (NULL, 'z')")
	execPlainBoth(t, db, sdb, "INSERT INTO pk1 VALUES (7, 'ok')")
	execPlainBoth(t, db, sdb, "UPDATE pk1 SET a = NULL WHERE a = 7")

	// An AUTOINCREMENT rowid alias stays a rowid alias under STRICT.
	execPlainBoth(t, db, sdb, "CREATE TABLE pk4 (a INTEGER PRIMARY KEY AUTOINCREMENT, b INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO pk4 VALUES (NULL, 1)")

	// And the constraint is VISIBLE, not just enforced: PRAGMA table_info
	// must report notnull=1 for exactly the columns C SQLite does, or
	// introspection would contradict the INSERTs above. That pragma has its
	// own independent re-parse of the stored CREATE TABLE text
	// (engine/pragma_parse.go's parsePragmaTableDef, deliberately NOT shared
	// with the write path's parser), so it needs its own gate here.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pureDB, err := sql.Open(driver.DriverName, path)
	if err != nil {
		t.Fatalf("sql.Open(driver): %v", err)
	}
	defer pureDB.Close()
	for _, tn := range []string{"pk1", "pk1n", "pk2", "pk3", "pk4"} {
		q := fmt.Sprintf("PRAGMA table_info(%s)", tn)
		requireSameDump(t, q, dumpQuery(t, pureDB, q), dumpQuery(t, sdb, q))
	}
}

// TestStrictTableRuntimeTypeChecks is the boundary gate: for each declared
// datatype, which stored values are accepted (and as WHAT storage class) and
// which are rejected, with the exact error text.
func TestStrictTableRuntimeTypeChecks(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_values")

	execPlainBoth(t, db, sdb, "CREATE TABLE v (i INT, g INTEGER, r REAL, t TEXT, b BLOB, y ANY) STRICT")

	// --- INT: the affinity conversion runs FIRST, and only its RESULT is
	// judged. A whole-string numeric TEXT and an integral REAL therefore
	// PASS (and are stored as integers); a fractional or out-of-range one
	// is reported as a REAL, not as the TEXT it was written as.
	for _, val := range []string{
		"1", "'123'", "'0123'", "' 123 '", "'  12'", "'12  '", "'+12'", "'-12'",
		"1.0", "'1.0'", "1e3", "'1e3'", "2.0e2", "-0.0", "true", "false",
		"9223372036854775807", "-9223372036854775808", "NULL",
		"1.5", "'1.5'", "'.5'", "1.0e300", "9223372036854775808",
		"'99999999999999999999999'", "99999999999999999999999", "'1e400'",
		"x'01'", "'abc'", "''", "' '", "'0x10'", "'12abc'", "'inf'", "'nan'", "'1_0'",
	} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("INSERT INTO v (i) VALUES (%s)", val))
	}
	// INTEGER is a DISTINCT declared type from INT and names itself in the
	// error, even though the two share INTEGER affinity.
	execPlainBoth(t, db, sdb, "INSERT INTO v (g) VALUES (1.5)")
	execPlainBoth(t, db, sdb, "INSERT INTO v (g) VALUES ('7')")

	// --- REAL: an integer is forced into floating point; a numeric TEXT
	// converts; a BLOB and a non-numeric TEXT are rejected.
	for _, val := range []string{
		"1", "1.5", "'1.5'", "'1'", "'5.'", "'.5'", "'1e400'", "9223372036854775807", "NULL",
		"'abc'", "x'01'", "''", "' '", "'inf'", "'nan'",
	} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("INSERT INTO v (r) VALUES (%s)", val))
	}

	// --- TEXT: numbers convert; only a BLOB is rejected.
	for _, val := range []string{"1", "1.5", "'abc'", "''", "NULL", "x'01'"} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("INSERT INTO v (t) VALUES (%s)", val))
	}

	// --- BLOB has no affinity, so NOTHING is ever converted into one: only
	// a blob (or NULL) may be stored.
	for _, val := range []string{"x'01'", "NULL", "1", "1.5", "'abc'", "''"} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("INSERT INTO v (b) VALUES (%s)", val))
	}

	// --- ANY accepts everything, unconverted.
	for _, val := range []string{"1", "1.5", "'abc'", "x'01'", "NULL", "''", "'123'"} {
		execPlainBoth(t, db, sdb, fmt.Sprintf("INSERT INTO v (y) VALUES (%s)", val))
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Every accepted row must have landed in the musql-written FILE with
	// the same storage class and the same value C SQLite gave its own
	// copy -- the affinity-conversion half of the rule, which an
	// error-text-only comparison cannot see.
	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(file): %v", err)
	}
	defer vdb.Close()
	const dump = `SELECT rowid, typeof(i), i, typeof(g), g, typeof(r), r,
		typeof(t), t, typeof(b), typeof(y), y FROM v ORDER BY rowid`
	requireSameDump(t, "stored values", dumpQuery(t, vdb, dump), dumpQuery(t, sdb, dump))

	var ic string
	if err := vdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("integrity_check = %q, want ok", ic)
	}
}

// TestStrictTableAnyHasNoAffinity pins the one CREATE-TABLE-time difference
// that is invisible in error text and visible only in stored data and in
// query results: "ANY" carries NO affinity in a STRICT table but NUMERIC
// affinity in an ordinary one, so the SAME INSERT stores a different
// storage class in each. Getting this wrong is a wrong ANSWER, not a
// missing feature -- hence its own gate, run through the READ path too.
func TestStrictTableAnyHasNoAffinity(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_any")

	execPlainBoth(t, db, sdb, "CREATE TABLE ys (y ANY) STRICT")
	execPlainBoth(t, db, sdb, "CREATE TABLE yn (y ANY)")
	for _, stmt := range []string{
		"INSERT INTO ys VALUES ('123')",
		"INSERT INTO ys VALUES (123)",
		"INSERT INTO ys VALUES (12.5)",
		"INSERT INTO ys VALUES (x'aa')",
		"INSERT INTO ys VALUES (NULL)",
		"INSERT INTO yn VALUES ('123')",
	} {
		execPlainBoth(t, db, sdb, stmt)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(file): %v", err)
	}
	defer vdb.Close()
	for _, q := range []string{
		"SELECT rowid, typeof(y), y FROM ys ORDER BY rowid",
		"SELECT rowid, typeof(y), y FROM yn ORDER BY rowid",
		// A STRICT ANY column defends its storage class in a comparison the
		// way any no-affinity column does: only the INTEGER 123 matches.
		"SELECT count(*) FROM ys WHERE y = 123",
		"SELECT count(*) FROM ys WHERE y = '123'",
		"SELECT typeof(y) FROM ys ORDER BY y",
	} {
		requireSameDump(t, q, dumpQuery(t, vdb, q), dumpQuery(t, sdb, q))
	}

	// The same question through musql's OWN read path (engine.Query, not
	// C SQLite reading musql's file): a STRICT ANY column resolved from
	// the stored CREATE TABLE text must get no affinity there too.
	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer rp.Close()
	for _, tc := range []struct {
		sql  string
		want int64
	}{
		{"SELECT count(*) FROM ys WHERE y = 123", 1},
		{"SELECT count(*) FROM ys WHERE y = '123'", 1},
		{"SELECT count(*) FROM yn WHERE y = 123", 1},
		{"SELECT count(*) FROM yn WHERE y = '123'", 1},
	} {
		_, rows, qerr := rp.Query(tc.sql)
		if qerr != nil {
			t.Fatalf("engine.Query(%s): %v", tc.sql, qerr)
		}
		if len(rows) != 1 || rows[0][0].I != tc.want {
			t.Fatalf("engine.Query(%s) = %v, want %d", tc.sql, rows, tc.want)
		}
	}
}

// TestStrictTableRowidAliasKeepsDatatypeMismatch pins the ONE column a
// STRICT table does NOT type-check the STRICT way: the INTEGER PRIMARY KEY
// rowid alias, which C SQLite still validates through the older rowid
// path and reports as the generic "datatype mismatch" -- byte-identical to
// the same INSERT into the same table WITHOUT the STRICT keyword. A
// "a INT PRIMARY KEY" column, which is NOT the rowid alias, IS type-checked.
func TestStrictTableRowidAliasKeepsDatatypeMismatch(t *testing.T) {
	db, sdb, _ := strictPair(t, "strict_ipk")

	execPlainBoth(t, db, sdb, "CREATE TABLE ip (a INTEGER PRIMARY KEY, b INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO ip VALUES ('abc', 1)")
	execPlainBoth(t, db, sdb, "INSERT INTO ip VALUES (1.5, 1)")
	execPlainBoth(t, db, sdb, "INSERT INTO ip VALUES (x'00', 1)")
	execPlainBoth(t, db, sdb, "INSERT INTO ip VALUES ('5', 1)") // TEXT '5' IS the rowid 5
	execPlainBoth(t, db, sdb, "INSERT INTO ip VALUES (6.0, 1)")

	// The same shapes against a NON-strict table give the identical errors.
	execPlainBoth(t, db, sdb, "CREATE TABLE ipn (a INTEGER PRIMARY KEY, b INT)")
	execPlainBoth(t, db, sdb, "INSERT INTO ipn VALUES ('abc', 1)")
	execPlainBoth(t, db, sdb, "INSERT INTO ipn VALUES (1.5, 1)")

	// "INT PRIMARY KEY" is a plain column, and IS checked the STRICT way.
	execPlainBoth(t, db, sdb, "CREATE TABLE ip2 (a INT PRIMARY KEY, b INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO ip2 VALUES ('abc', 1)")
	execPlainBoth(t, db, sdb, "INSERT INTO ip2 VALUES ('5', 1)")

	// "datatype mismatch" is unsoftenable too: an OR-clause does not turn it
	// into a skipped row (verified directly against C SQLite). Asserted
	// for the STRICT table only -- "INSERT OR IGNORE INTO ipn VALUES('abc',
	// 1)" against the ORDINARY table above is a PRE-EXISTING divergence in
	// this engine (its OpMustBeInt carries the statement's conflict action,
	// so OR IGNORE silently skips the row where C SQLite still fails),
	// unrelated to STRICT and deliberately not addressed here.
	execPlainBoth(t, db, sdb, "INSERT OR IGNORE INTO ip VALUES ('abc', 1)")
	execPlainBoth(t, db, sdb, "INSERT OR REPLACE INTO ip VALUES ('abc', 1)")
}

// TestStrictTableGeneratedColumns pins that a generated column of a STRICT
// table is type-checked like any other -- its COMPUTED value is what must
// fit the declared type -- and that both STORED and VIRTUAL are covered.
func TestStrictTableGeneratedColumns(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_gen")

	execPlainBoth(t, db, sdb, "CREATE TABLE gc (a INT, b TEXT GENERATED ALWAYS AS (a+1) STORED) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO gc (a) VALUES (1)")
	execPlainBoth(t, db, sdb, "CREATE TABLE gc2 (a INT, b BLOB GENERATED ALWAYS AS (a+1) VIRTUAL) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO gc2 (a) VALUES (1)")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(file): %v", err)
	}
	defer vdb.Close()
	const q = "SELECT rowid, typeof(a), a, typeof(b), b FROM gc ORDER BY rowid"
	requireSameDump(t, q, dumpQuery(t, vdb, q), dumpQuery(t, sdb, q))
}

// TestStrictTableInsertSelect gates the INSERT ... SELECT source, whose rows
// reach the same per-row path but never as literal VALUES.
func TestStrictTableInsertSelect(t *testing.T) {
	db, sdb, _ := strictPair(t, "strict_insel")

	for _, stmt := range []string{
		"CREATE TABLE src (x)",
		"INSERT INTO src VALUES ('77')",
		"INSERT INTO src VALUES ('abc')",
		"CREATE TABLE dst (i INT) STRICT",
		"INSERT INTO dst SELECT x FROM src WHERE x = '77'",
		"INSERT INTO dst SELECT x FROM src WHERE x = 'abc'",
		"INSERT OR IGNORE INTO dst SELECT x FROM src WHERE x = 'abc'",
	} {
		execPlainBoth(t, db, sdb, stmt)
	}
}

// TestStrictTableErrorPrecedenceAndConflictClauses pins two rules at once:
// where the datatype check sits relative to NOT NULL, CHECK and UNIQUE, and
// that no conflict clause can soften it.
func TestStrictTableErrorPrecedenceAndConflictClauses(t *testing.T) {
	db, sdb, _ := strictPair(t, "strict_prec")

	// NOT NULL beats a type error on a LATER column; a type error beats a
	// CHECK on an EARLIER column; a CHECK is what is left.
	execPlainBoth(t, db, sdb, "CREATE TABLE pr (a INT NOT NULL CHECK(a<10), b TEXT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO pr VALUES ('zz', 'q')")
	execPlainBoth(t, db, sdb, "INSERT INTO pr VALUES (NULL, x'01')")
	execPlainBoth(t, db, sdb, "INSERT INTO pr VALUES (50, x'01')")
	execPlainBoth(t, db, sdb, "INSERT INTO pr VALUES (50, 'q')")
	execPlainBoth(t, db, sdb, "INSERT INTO pr VALUES (5, 'q')")
	// Within one row, the FIRST offending column in declaration order wins.
	execPlainBoth(t, db, sdb, "CREATE TABLE mb (a INT, b INT, c BLOB) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO mb VALUES ('x','y','z')")
	execPlainBoth(t, db, sdb, "INSERT INTO mb VALUES (1,'y','z')")
	execPlainBoth(t, db, sdb, "INSERT INTO mb VALUES (1,2,'z')")

	// A type error beats a UNIQUE violation on an earlier column.
	execPlainBoth(t, db, sdb, "CREATE TABLE u (a INT UNIQUE, b INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO u VALUES (1,1)")
	execPlainBoth(t, db, sdb, "INSERT INTO u VALUES (1,'x')")

	// No OR-clause softens it -- not IGNORE, not REPLACE, not ROLLBACK --
	// and neither does an upsert's DO UPDATE.
	execPlainBoth(t, db, sdb, "CREATE TABLE c (i INT, k INT PRIMARY KEY, v TEXT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO c VALUES (1, 1, 'a')")
	execPlainBoth(t, db, sdb, "INSERT OR IGNORE INTO c VALUES ('abc', 2, 'b')")
	execPlainBoth(t, db, sdb, "INSERT OR REPLACE INTO c VALUES ('abc', 2, 'b')")
	execPlainBoth(t, db, sdb, "INSERT OR ROLLBACK INTO c VALUES ('abc', 2, 'b')")
	execPlainBoth(t, db, sdb, "REPLACE INTO c VALUES (9, 1, x'00')")
	execPlainBoth(t, db, sdb, "INSERT INTO c VALUES (9,1,'z') ON CONFLICT(k) DO UPDATE SET v=x'00'")
	execPlainBoth(t, db, sdb, "INSERT INTO c VALUES (9,1,'z') ON CONFLICT(k) DO UPDATE SET v='w'")

	// UPDATE enforces identically, again regardless of any OR-clause.
	execPlainBoth(t, db, sdb, "UPDATE c SET i = 'abc'")
	execPlainBoth(t, db, sdb, "UPDATE OR IGNORE c SET i = 'abc'")
	execPlainBoth(t, db, sdb, "UPDATE OR REPLACE c SET i = 'abc'")
	execPlainBoth(t, db, sdb, "UPDATE c SET i = '42'") // numeric TEXT converts, so it passes
	execPlainBoth(t, db, sdb, "UPDATE c SET v = 3")    // number into TEXT converts

	// The contrast that makes "unsoftenable" meaningful: an OR IGNORE really
	// does skip a NOT NULL violation on the SAME table, so the difference is
	// the constraint kind, not the statement.
	execPlainBoth(t, db, sdb, "CREATE TABLE nn (a INT NOT NULL, b INT) STRICT")
	execPlainBoth(t, db, sdb, "INSERT INTO nn VALUES (1, 1)")
	execPlainBoth(t, db, sdb, "UPDATE OR IGNORE nn SET a = NULL")
	execPlainBoth(t, db, sdb, "UPDATE OR IGNORE nn SET a = 'zz'")
	execPlainBoth(t, db, sdb, "INSERT OR IGNORE INTO nn VALUES (NULL, 2)")
	execPlainBoth(t, db, sdb, "INSERT OR IGNORE INTO nn VALUES ('zz', 2)")

	// A trigger body writing into a STRICT table is enforced too.
	execPlainBoth(t, db, sdb, "CREATE TABLE src (x INT)")
	execPlainBoth(t, db, sdb, "CREATE TABLE dst (a INT) STRICT")
	execPlainBoth(t, db, sdb, "CREATE TRIGGER tg AFTER INSERT ON src BEGIN INSERT INTO dst VALUES ('bad'); END")
	execPlainBoth(t, db, sdb, "INSERT INTO src VALUES (1)")
}

// TestStrictTableAlterTable pins what ALTER TABLE does to a STRICT table:
// RENAME COLUMN and DROP COLUMN edit the stored CREATE TABLE text in place
// and must leave both the STRICT tail and the enforcement intact, while ADD
// COLUMN is declined outright -- engine/alter_write.go's addColumn builds
// the new column by hand instead of re-running the CREATE TABLE parser, so
// it would neither reject an illegal datatype (C SQLite: `error in table
// t after add column: unknown datatype for t.z: "VARCHAR(3)"`) nor give an
// ANY column its no-affinity treatment. Declining is honest; silently
// half-applying it would not be.
func TestStrictTableAlterTable(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_alter")

	for _, stmt := range []string{
		"CREATE TABLE t (a INT, b TEXT, c ANY) STRICT",
		"INSERT INTO t VALUES (1, 'x', '123')",
		"ALTER TABLE t RENAME COLUMN b TO bb",
		"ALTER TABLE t DROP COLUMN c",
		"INSERT INTO t VALUES (2, 'y')",
	} {
		execPlainBoth(t, db, sdb, stmt)
	}
	// Still enforced after both in-place schema edits.
	execPlainBoth(t, db, sdb, "INSERT INTO t VALUES ('nope', 'y')")
	execPlainBoth(t, db, sdb, "UPDATE t SET bb = x'00' WHERE a = 1")

	// ADD COLUMN is SERVED now, and it enforces STRICT's own type vocabulary
	// the way C SQLite does -- by RELOADING the altered schema, which is
	// why its message is wrapped ("error in table t after add column: unknown
	// datatype for t.z: \"VARCHAR(3)\""). The column is taken WHOLE from that
	// re-parse rather than built by hand, so "ANY" gets NO affinity here and
	// after the next reopen alike; see addColumn (engine/alter_write.go).
	execPlainBoth(t, db, sdb, "ALTER TABLE t ADD COLUMN z TEXT")
	execPlainBoth(t, db, sdb, "INSERT INTO t VALUES (3, 'w', 'q')")
	execPlainBoth(t, db, sdb, "INSERT INTO t VALUES (4, 'w', 5)")
	execPlainBoth(t, db, sdb, "ALTER TABLE t ADD COLUMN y VARCHAR(3)")
	execPlainBoth(t, db, sdb, "ALTER TABLE t ADD COLUMN y2")
	execPlainBoth(t, db, sdb, "ALTER TABLE t ADD COLUMN y3 ANY")
	execPlainBoth(t, db, sdb, "INSERT INTO t VALUES (5, 'w', 'q', x'00')")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(file): %v", err)
	}
	defer vdb.Close()
	for _, q := range []string{
		"SELECT sql FROM sqlite_schema WHERE name='t'",
		"SELECT rowid, typeof(a), a, typeof(bb), bb FROM t ORDER BY rowid",
	} {
		requireSameDump(t, q, dumpQuery(t, vdb, q), dumpQuery(t, sdb, q))
	}
}

// TestStrictTableSurvivesReopen is the load-bearing gate: STRICT is carried
// ONLY by the table's stored CREATE TABLE text, so a writer that dropped it
// on OpenWrite would silently stop enforcing types on a table whose own SQL
// still says STRICT. It writes a STRICT table (rowid and WITHOUT ROWID),
// closes, reopens with OpenWrite, and requires the identical accept/reject
// behavior -- then hands the file to C SQLite and requires the same
// again.
func TestStrictTableSurvivesReopen(t *testing.T) {
	db, sdb, path := strictPair(t, "strict_reopen")

	for _, stmt := range []string{
		"CREATE TABLE t (i INT, y ANY, b BLOB) STRICT",
		"CREATE TABLE w (k TEXT PRIMARY KEY, n INT) STRICT, WITHOUT ROWID",
		"INSERT INTO t VALUES (1, '123', x'aa')",
		"INSERT INTO w VALUES ('a', 1)",
	} {
		execPlainBoth(t, db, sdb, stmt)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	// Still enforced after the reopen -- the whole point of this test.
	for _, stmt := range []string{
		"INSERT INTO t VALUES (2, 'kept-as-text', x'bb')",
		"INSERT INTO t VALUES ('nope', 1, x'cc')",
		"INSERT INTO t VALUES (3, 1, 'not-a-blob')",
		"UPDATE t SET i = 'nope' WHERE i = 1",
		"INSERT INTO w VALUES (NULL, 2)",
		"INSERT INTO w VALUES ('b', 'nope')",
		"INSERT INTO w VALUES ('b', 2)",
	} {
		execPlainBoth(t, db2, sdb, stmt)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close2: %v", err)
	}

	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(file): %v", err)
	}
	defer vdb.Close()
	// The STRICT keyword itself survived into the rebuilt file's schema, and
	// C SQLite enforces the SAME rules reading musql's copy.
	for _, q := range []string{
		"SELECT name, sql FROM sqlite_schema WHERE type='table' ORDER BY name",
		"SELECT rowid, typeof(i), i, typeof(y), y, typeof(b) FROM t ORDER BY rowid",
		"SELECT k, typeof(n), n FROM w ORDER BY k",
	} {
		requireSameDump(t, q, dumpQuery(t, vdb, q), dumpQuery(t, sdb, q))
	}
	if _, err := vdb.Exec("INSERT INTO t VALUES ('nope', 1, x'cc')"); err == nil {
		t.Fatalf("C SQLite accepted a type violation in musql's rebuilt file -- STRICT was not persisted")
	}

	var ic string
	if err := vdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("integrity_check = %q, want ok", ic)
	}
}
