// Tests DEFAULT clause folding: both the computed value and the reported text
// in PRAGMA table_info, across various expression types and operators.
package compat

import (
	"fmt"
	"testing"
)

// defaultFoldDecls are DEFAULT clauses whose value foldDefaultValue decides.
var defaultFoldDecls = []struct {
	name string
	decl string
}{
	{"int-lit", `a INTEGER DEFAULT 5`},
	{"neg-int", `a INTEGER DEFAULT -5`},
	{"neg-str", `a TEXT DEFAULT -'abc'`},
	{"plus-str", `a TEXT DEFAULT +'abc'`},
	{"neg-blob", `a BLOB DEFAULT -x'ff'`},
	{"plus-blob", `a BLOB DEFAULT +x'ff'`},
	{"neg-null", `a DEFAULT -NULL`},
	{"plus-null", `a DEFAULT +NULL`},
	{"minint", `a INTEGER DEFAULT -9223372036854775808`},
	{"hex", `a INTEGER DEFAULT 0x10`},
	{"blob", `a BLOB DEFAULT x'ff00'`},
	{"str", `a TEXT DEFAULT 'hi'`},
	{"id-as-str", `a TEXT DEFAULT hello`},
	{"true", `a DEFAULT TRUE`},
	{"paren-arith", `a INTEGER DEFAULT (1+2)`},
	{"paren-divide", `a REAL DEFAULT (1/2)`},
	{"paren-divzero", `a DEFAULT (1/0)`},
	{"paren-func-abs", `a INTEGER DEFAULT (abs(-3))`},
	{"paren-func-upper", `a TEXT DEFAULT (upper('ab'))`},
	{"paren-concat", `a TEXT DEFAULT ('a' || 'b')`},
	{"paren-cast", `a DEFAULT (CAST('12ab' AS INTEGER))`},
	{"paren-cast-real", `a DEFAULT (CAST(2 AS REAL))`},
	{"paren-hex-blob", `a DEFAULT (hex(x'ff'))`},
	{"paren-typeof", `a DEFAULT (typeof(1))`},
	{"paren-nullif", `a DEFAULT (nullif(1,1))`},
	{"paren-case", `a DEFAULT (CASE WHEN 1 THEN 'y' ELSE 'n' END)`},
	{"paren-in", `a DEFAULT (3 IN (1,2,3))`},
	{"paren-like", `a DEFAULT ('abc' LIKE 'a%')`},
	{"paren-collate", `a TEXT DEFAULT ('AB' COLLATE NOCASE)`},
	{"paren-str-lit", `a TEXT DEFAULT ('hi')`},
	{"paren-neg-lit", `a INTEGER DEFAULT (-7)`},
	{"paren-null", `a DEFAULT (NULL)`},
	{"paren-overflow", `a DEFAULT (9223372036854775807+1)`},
	{"paren-unknown-func", `a DEFAULT (nosuchfunc(1))`},
	{"paren-strftime", `a DEFAULT (strftime('%Y','2020-01-01'))`},
	{"paren-json", `a DEFAULT (json_array(1,2))`},
	{"paren-printf", `a TEXT DEFAULT (printf('%d-%s',3,'x'))`},
	{"paren-round", `a DEFAULT (round(2.345,2))`},
	{"paren-bitand", `a DEFAULT (5 & 3)`},
	{"paren-not", `a DEFAULT (NOT 0)`},
	{"paren-isnull", `a DEFAULT (NULL IS NULL)`},
	{"paren-between", `a DEFAULT (2 BETWEEN 1 AND 3)`},
	{"paren-coalesce", `a DEFAULT (coalesce(NULL,7))`},
	{"paren-quote", `a DEFAULT (quote('a''b'))`},
	{"paren-char", `a DEFAULT (char(65,66))`},
	{"paren-zeroblob", `a DEFAULT (zeroblob(3))`},
	{"paren-replace", `a TEXT DEFAULT (replace('aaa','a','b'))`},
	{"paren-substr", `a TEXT DEFAULT (substr('hello',2,3))`},
	{"paren-iif", `a DEFAULT (iif(1,'y','n'))`},
	{"paren-max2", `a DEFAULT (max(1,2))`},
	{"paren-unicode", `a DEFAULT (unicode('A'))`},
	{"paren-glob", `a DEFAULT ('abc' GLOB 'a*')`},
	{"paren-shift", `a DEFAULT (1 << 10)`},
	{"paren-affinity-txt", `a INTEGER DEFAULT ('5')`},
	{"paren-affinity-num", `a TEXT DEFAULT (5)`},
	{"paren-num-txt-cat", `a NUMERIC DEFAULT ('1' || '2')`},
}

// TestDefaultClauseFoldedValue: the stored value and its storage class for a
// row that omits the column, both for an explicit column list and for INSERT
// ... DEFAULT VALUES. Also reads back the CREATE TABLE text, since the fold
// must survive being replayed out of sqlite_master.
func TestDefaultClauseFoldedValue(t *testing.T) {
	for _, c := range defaultFoldDecls {
		differ(t, "default-fold-"+c.name, []string{
			fmt.Sprintf(`CREATE TABLE d(k INTEGER PRIMARY KEY, %s)`, c.decl),
			`INSERT INTO d(k) VALUES(1)`,
			`INSERT INTO d DEFAULT VALUES`,
			`SELECT k, a, typeof(a) FROM d ORDER BY k`,
			`SELECT sql FROM sqlite_master WHERE name='d'`,
		})
	}
}

// TestDefaultClauseReportedText: PRAGMA table_info/table_xinfo's dflt_value.
// The signed non-number spellings are the ten that were wrong.
func TestDefaultClauseReportedText(t *testing.T) {
	for _, decl := range []string{
		`a TEXT DEFAULT -'abc'`,
		`a TEXT DEFAULT +'abc'`,
		`a BLOB DEFAULT -x'ff'`,
		`a BLOB DEFAULT +x'ff'`,
		`a DEFAULT -NULL`,
		`a DEFAULT +NULL`,
		`a DEFAULT -CURRENT_TIMESTAMP`,
		`a INTEGER DEFAULT -5`,
		`a INTEGER DEFAULT +5`,
		`a REAL DEFAULT -1.5`,
		`a TEXT DEFAULT -'abc' NOT NULL`,
		`a TEXT DEFAULT -'abc' UNIQUE`,
		`a TEXT DEFAULT -'abc' COLLATE NOCASE`,
		`a BLOB DEFAULT -x'ff' NOT NULL`,
		`a INTEGER DEFAULT -5 NOT NULL`,
		`a TEXT DEFAULT ('x')`,
		`a TEXT DEFAULT 'x'`,
	} {
		differ(t, "default-text-"+decl, []string{
			fmt.Sprintf(`CREATE TABLE d(k INTEGER PRIMARY KEY, %s)`, decl),
			`PRAGMA table_info(d)`,
			`PRAGMA table_xinfo(d)`,
			`INSERT INTO d(k) VALUES(1)`,
			`SELECT k, a, typeof(a) FROM d`,
			`INSERT INTO d(k,a) VALUES(2,NULL)`,
			`SELECT count(*) FROM d`,
		})
	}
}
