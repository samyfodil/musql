// Package compat tests UPSERT name resolution when target is named "excluded".
package compat

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestUpsertTargetNamedExcluded(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "upsertexcluded")
	for _, stmt := range []string{
		`CREATE TABLE excluded(a INT, b INT, c INT)`,
		`CREATE UNIQUE INDEX excludedab ON excluded(a,b)`,
		// Unaliased: "excluded.c" reads the STORED row's c, so the three
		// (1,2) tuples leave c at 2. Reading the PROPOSED row's c instead
		// would pin it at 1, since every tuple proposes the same 0.
		`INSERT INTO excluded(a,b,c) VALUES(1,2,0),(1,2,0),(3,4,0),(1,2,0),(5,6,0),(3,4,0)
			ON CONFLICT(b,a) DO UPDATE SET c=excluded.c+1`,
	} {
		if _, err := pureDB.Exec(stmt); err != nil {
			t.Fatalf("pure Exec(%s): %v", stmt, err)
		}
		if _, err := mattnDB.Exec(stmt); err != nil {
			t.Fatalf("mattn Exec(%s): %v", stmt, err)
		}
	}
	queryBoth(t, pureDB, mattnDB, "unaliased", `SELECT a,b,c FROM excluded ORDER BY a`)

	// Shadowing must not affect ordinary tables.
	for _, stmt := range []string{
		`CREATE TABLE ordinary(k INTEGER PRIMARY KEY, v INT)`,
		`INSERT INTO ordinary VALUES(1,5)`,
		`INSERT INTO ordinary VALUES(1,100) ON CONFLICT(k) DO UPDATE SET v=excluded.v+ordinary.v`,
	} {
		if _, err := pureDB.Exec(stmt); err != nil {
			t.Fatalf("pure Exec(%s): %v", stmt, err)
		}
		if _, err := mattnDB.Exec(stmt); err != nil {
			t.Fatalf("mattn Exec(%s): %v", stmt, err)
		}
	}
	queryBoth(t, pureDB, mattnDB, "ordinary", `SELECT k,v FROM ordinary ORDER BY k`)

	// "INSERT INTO <t> AS <alias>" -- upsert3-210's own way of restoring
	// "excluded" as the pseudo-table -- USED to be unparsable here, and this
	// asserted the decline. It parses now (see insert_target_alias_test.go), so
	// the assertion is the STRONGER one the decline was standing in for: the
	// alias names the TARGET, "excluded" goes back to meaning the proposed row,
	// and both engines agree on the result.
	for _, stmt := range []string{
		`INSERT INTO excluded AS base(a,b,c) VALUES(1,2,8)
			ON CONFLICT(b,a) DO UPDATE SET c=excluded.c+1 WHERE base.c<excluded.c`,
		// ...and once more with the two swapped, so a scope lookup that
		// silently resolved BOTH names to the same row could not pass.
		`INSERT INTO excluded AS base(a,b,c) VALUES(1,2,50)
			ON CONFLICT(b,a) DO UPDATE SET c=base.c+excluded.c`,
	} {
		if _, err := pureDB.Exec(stmt); err != nil {
			t.Fatalf("pure Exec(%s): %v", stmt, err)
		}
		if _, err := mattnDB.Exec(stmt); err != nil {
			t.Fatalf("mattn Exec(%s): %v", stmt, err)
		}
	}
	queryBoth(t, pureDB, mattnDB, "aliased", `SELECT a,b,c FROM excluded ORDER BY a`)
}

// TestUpsertExcludedNameStrictTable runs the same rule against a STRICT table.
// It exists because a STRICT table used to decline VDBE compilation outright
// (compileInsertStmt), which took this identical statement down a different
// route through the engine entirely. STRICT's datatype check is an opcode now
// (OpTypeCheck, engine/vdbe_write.go), so this compiles like any other upsert
// -- the case is kept because the rule has to hold for a STRICT table too.
func TestUpsertExcludedNameStrictTable(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "upsertexcludedtw")
	for _, stmt := range []string{
		`CREATE TABLE excluded(a INT, b INT, c INT) STRICT`,
		`CREATE UNIQUE INDEX excludedab ON excluded(a,b)`,
		`INSERT INTO excluded(a,b,c) VALUES(1,2,0),(1,2,0),(3,4,0),(1,2,0),(5,6,0),(3,4,0)
			ON CONFLICT(b,a) DO UPDATE SET c=excluded.c+1`,
	} {
		if _, err := pureDB.Exec(stmt); err != nil {
			t.Fatalf("pure Exec(%s): %v", stmt, err)
		}
		if _, err := mattnDB.Exec(stmt); err != nil {
			t.Fatalf("mattn Exec(%s): %v", stmt, err)
		}
	}
	queryBoth(t, pureDB, mattnDB, "strict-unaliased", `SELECT a,b,c FROM excluded ORDER BY a`)
}
