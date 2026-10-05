package compat

// Tests CAST's target type names against C SQLite. Type names resolve to
// affinities via the same rules as column declarations. The engine must accept
// all spellings that map to the five affinities, including non-canonical ones.

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// TestCastTypeNames crosses every affinity-determining spelling (and several
// that fall through to NUMERIC) with values that make the five affinities
// distinguishable: typeof() names the affinity actually applied, and the value
// itself catches a rule that picks the right affinity for the wrong reason.
func TestCastTypeNames(t *testing.T) {
	types := []string{
		// Rule 1: contains "INT".
		"INT", "INTEGER", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT",
		"UNSIGNED BIG INT", "INT2", "INT8", "POINT", "PRINT",
		// Rule 2: contains CHAR, CLOB or TEXT (and is not caught by rule 1).
		"CHARACTER(20)", "VARCHAR(255)", "VARYING CHARACTER(255)",
		"NCHAR(55)", "NATIVE CHARACTER(70)", "NVARCHAR(100)", "TEXT", "CLOB",
		// Rule 3: contains BLOB, or is a bare quoted empty name.
		"BLOB", "MYBLOBTYPE",
		// Rule 4: contains REAL, FLOA or DOUB.
		"REAL", "DOUBLE", "DOUBLE PRECISION", "FLOAT", "FLOATING",
		// Rule 5: everything else.
		"NUMERIC", "DECIMAL(10,5)", "BOOLEAN", "DATE", "DATETIME",
		"STRING", "VARIANT",
		// A keyword that is still a legal identifier is a legal type name;
		// only the non-identifier ones are refused (see the reject test).
		"KEY", "ABORT", `"NOTHING"`,
		// An EMPTY type name is legal and means NUMERIC, and a string literal
		// is a legal type name too ('ids' is ID|STRING in the grammar).
		"", "'text'", "'int'", "'zz'",
		// Rule ORDER matters: each of these matches two rules and the earlier
		// one must win. "INTCHAR" is INTEGER (not TEXT), "CHARBLOB" is TEXT
		// (not BLOB), "BLOBREAL" is BLOB (not REAL).
		"INTCHAR", "INTBLOB", "INTREAL", "CHARBLOB", "CHARREAL", "BLOBREAL",
		// Case-insensitivity, and the quoted spellings the grammar allows.
		"int", "Integer", "vArChAr(9)", `"unsigned big int"`, "[BIGINT]",
	}
	vals := []string{
		"'A'", "'1.5'", "'42'", "' 42 '", "'42abc'", "''",
		"42", "1.5", "-0.0", "NULL", "x'414243'", "x''",
		"9223372036854775807", "1e300", "'0x10'",
	}
	for _, ty := range types {
		for _, v := range vals {
			q := fmt.Sprintf("SELECT typeof(cast(%s as %s)), hex(cast(%s as %s)), cast(%s as %s)",
				v, ty, v, ty, v, ty)
			if !differ(t, "casttype/"+ty, []string{q}) {
				t.Errorf("diverged on: %s", q)
			}
		}
	}
}

// TestCastTypeNameShapes covers the places a CAST target's spelling has to
// survive something other than a bare SELECT: the result COLUMN NAME (which is
// the verbatim source text, not the canonicalized affinity), a stored column
// default, a CHECK constraint, an index expression, and a CREATE TABLE AS whose
// new column takes both the name and the affinity from the cast.
func TestCastTypeNameShapes(t *testing.T) {
	if !differ(t, "casttype-name", []string{
		"SELECT cast('7' as int), cast('7' AS Big Int), cast(1 as varchar(3))",
	}) {
		t.Error("diverged on cast result column naming")
	}
	// NOTE: the new table's sqlite_master TEXT is deliberately not asserted
	// here. C SQLite gives a CREATE TABLE AS column a DECLARED TYPE derived
	// from the expression's affinity ("CREATE TABLE u(k INT,m TEXT)") and wraps
	// the text once it is wide enough; this engine writes "CREATE TABLE u(k,m)"
	// for every CTAS. That is a separate, PRE-EXISTING rule -- it reproduces
	// identically with the already-accepted spellings cast(... as integer) /
	// cast(... as text), so it is not a consequence of widening the type-name
	// grammar. The VALUES and their typeof() are asserted, which is what this
	// rule owns.
	if !differ(t, "casttype-ddl", []string{
		"CREATE TABLE t(a, b DEFAULT (cast('5' as int)), c CHECK (cast(c as int) < 100))",
		"INSERT INTO t(a,c) VALUES(1, 7)",
		"SELECT a, b, c, typeof(b) FROM t",
		"CREATE TABLE u AS SELECT cast('9' as bigint) AS k, cast(2 as varchar(4)) AS m",
		"SELECT k, m, typeof(k), typeof(m) FROM u",
		"CREATE INDEX ti ON t(cast(a as int))",
		"SELECT a FROM t WHERE cast(a as int) = 1",
	}) {
		t.Error("diverged on cast type names in DDL")
	}
}

// TestCastTypeNameRejected pins the spellings that are a SYNTAX ERROR there, so
// widening the accepted set did not also start accepting a statement real
// SQLite refuses to run.
func TestCastTypeNameRejected(t *testing.T) {
	for _, q := range []string{
		"SELECT cast('1' as 5)",
		"SELECT cast('1' as int(x))",
		"SELECT cast('1' as int(1,2,3))",
		"SELECT cast('1' as int extra 5)",
		// A bare NON-IDENTIFIER keyword is a syntax error as a type name,
		// which is exactly nonIdentifierKeywords (engine/sql_parser.go).
		"SELECT cast('1' as nothing)",
		"SELECT cast('1' as select)",
		"SELECT cast('1' as table)",
		"SELECT cast('1' as index)",
		"SELECT cast('1' as add)",
		"SELECT cast('1' as distinct)",
	} {
		if !differ(t, "casttype-bad", []string{q}) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
