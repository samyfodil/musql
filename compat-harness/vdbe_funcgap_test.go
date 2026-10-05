// Function conformance gaps: octet_length, concat/concat_ws, hex literals, Inf/NaN handling.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// Deterministic differential gate for conformance additions; error text not compared.
var funcGapCases = []string{
	// ---- octet_length(): byte length, not character count ----
	`SELECT octet_length(12345)`,
	`SELECT octet_length(NULL)`,
	`SELECT octet_length(7.5)`,
	`SELECT octet_length(x'30313233')`,
	`SELECT octet_length('hello')`,
	`SELECT octet_length('')`,
	`SELECT octet_length(x'')`,
	`SELECT octet_length(1.0)`,
	`SELECT typeof(octet_length(1))`,
	`SELECT octet_length(-9223372036854775808)`,

	// ---- concat(): NULL -> "", never NULL-propagating like || ----
	`SELECT concat('a','b','c')`,
	`SELECT concat('a',NULL,'c')`,
	`SELECT concat(1,2.5,'c')`,
	`SELECT concat(NULL,NULL)`,
	`SELECT concat('a',x'01')`,
	`SELECT concat(x'ff01')`,
	`SELECT typeof(concat(x'ff01'))`,
	`SELECT concat('solo')`,
	`SELECT typeof(concat(NULL))`,

	// ---- concat_ws(): skips NULL values (no empty placeholder, no doubled
	// separator), NULL separator poisons the whole result to NULL ----
	`SELECT concat_ws(',','a','b','c')`,
	`SELECT concat_ws(',','a',NULL,'c')`,
	`SELECT concat_ws(NULL,'a','b')`,
	`SELECT concat_ws(',',NULL,NULL)`,
	`SELECT concat_ws(',','a')`,
	`SELECT concat_ws(1,2,3)`,
	`SELECT concat_ws(x'2c','a','b')`,
	`SELECT typeof(concat_ws(',','a'))`,

	// ---- arity errors: both engines must reject these identically ----
	`SELECT concat()`,
	`SELECT concat_ws(',')`,
	`SELECT octet_length()`,
	`SELECT octet_length(1,2)`,

	// ---- hexadecimal integer literals ----
	`SELECT 0x0`,
	`SELECT -0x0`,
	`SELECT 0xFF`,
	`SELECT 0X1a`,
	`SELECT -0x1`,
	`SELECT 0x100000001`,
	`SELECT 0x7ffffffffffffffe`,
	`SELECT 0xffffffffffffffff`,     // wraps to -1 -- the sign bit
	`SELECT 0x00000000000000000001`, // leading zeros don't count toward the 64-bit overflow check
	`SELECT typeof(0xFF)`,
	`SELECT 0xFF + 1`,
	`SELECT substr('abcdefg',0x100000001,2)`,
	`SELECT substr('abcdefg',1,0x100000002)`,
	`SELECT quote(substr(x'313233343536373839',0x7ffffffffffffffe,5))`,
	// malformed/overflowing hex literals: both engines must reject
	`SELECT 0x10000000000000000`, // > 64 bits -- "hex literal too big"
	`SELECT 0xZZ`,                // no hex digits after prefix that parse
	`SELECT 0x1g`,                // trailing non-hex identifier char
	`SELECT 0x`,                  // prefix with no digits at all

	// ---- numeric literal overflow to +/-Inf (no longer a parse error) ----
	`SELECT 1e500`,
	`SELECT -1e500`,
	`SELECT typeof(1e500)`,
	`SELECT 4.2e+859`,
	`SELECT -7.8e+904`,
	`SELECT 9e+999`,
	`SELECT -9e+999`,

	// ---- round()/quote() on Inf: must not panic, and quote()'s own
	// distinct "9.0e+999" spelling (not "Inf") ----
	`SELECT round(1e500)`,
	`SELECT round(-1e500)`,
	`SELECT round(1e500, 2)`,
	`SELECT round(1e500, -2)`,
	`SELECT typeof(round(1e500))`,
	`SELECT quote(4.2e+859)`,
	`SELECT quote(-7.8e+904)`,
	`SELECT quote(1e300)`,        // finite REAL -- quote()'s ordinary path, unaffected by the Inf special-case
	`SELECT CAST(1e500 AS TEXT)`, // ordinary REAL->TEXT conversion: plain "Inf", NOT quote()'s "9.0e+999"
	`SELECT ''||1e500`,
}

func TestFuncGapMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("funcgap_%d.sqlite", pageSize))
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

			for _, s := range funcGapCases {
				t.Run(s, func(t *testing.T) {
					compareOneScalar(t, db, sdb, s)
				})
			}
		})
	}
}
