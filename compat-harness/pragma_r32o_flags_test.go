// Tests for pragma flags count_changes and reverse_unordered_selects.
// These were previously accepted but ignored, which is a silent wrong answer.
package compat

import (
	"encoding/json"
	"testing"
)

// r32oKnownOpen logs known divergences and fails if they're fixed.
func r32oKnownOpen(t *testing.T, name, why string, stmts []string) {
	t.Helper()
	sub := &testing.T{}
	if differ(sub, name, stmts) {
		t.Errorf("%s no longer diverges (%s) -- delete this case", name, why)
		return
	}
	t.Logf("KNOWN OPEN %s: %s", name, why)
}

// TestR32OCountChangesBattery covers count_changes for DML with various conditions.
func TestR32OCountChangesBattery(t *testing.T) {
	differ(t, "r32o count_changes insert", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`PRAGMA count_changes=1`,
		`PRAGMA count_changes`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`INSERT INTO t1 VALUES(2,'y'),(3,'z')`,
		`INSERT INTO t1 SELECT a+10, b FROM t1`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r32o count_changes update delete", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x'),(2,'y'),(3,'z')`,
		`PRAGMA count_changes=1`,
		`UPDATE t1 SET b='q' WHERE a>1`,
		`UPDATE t1 SET b='q' WHERE a>99`,
		`DELETE FROM t1 WHERE a=2`,
		`DELETE FROM t1 WHERE a>99`,
		`DELETE FROM t1`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r32o count_changes off again", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1)`,
		`PRAGMA count_changes=0`,
		`PRAGMA count_changes`,
		`INSERT INTO t1 VALUES(2)`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r32o count_changes returning", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'x') RETURNING a`,
		`UPDATE t1 SET b='y' RETURNING b`,
		`DELETE FROM t1 RETURNING a`,
		`SELECT count(*) FROM t1`,
	})
	differ(t, "r32o count_changes conflict clauses", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT OR IGNORE INTO t1 VALUES(1,'w')`,
		`INSERT OR REPLACE INTO t1 VALUES(1,'v')`,
		`INSERT OR IGNORE INTO t1 VALUES(2,'m'),(1,'n'),(3,'o')`,
		`SELECT a,b FROM t1 ORDER BY a`,
	})
	// The qualifier is IGNORED: db->flags has one copy per connection and none
	// per database, and pragma.c's PragTyp_FLAG arm never looks at iDb. Every
	// boolean spelling sqlite3GetBoolean takes is exercised too.
	differ(t, "r32o count_changes spellings and qualifier", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA main.count_changes=1`,
		`PRAGMA count_changes`,
		`PRAGMA temp.count_changes=off`,
		`PRAGMA count_changes`,
		`PRAGMA count_changes=yes`,
		`PRAGMA main.count_changes`,
		`PRAGMA count_changes=false`,
		`PRAGMA count_changes`,
		`PRAGMA count_changes=on`,
		`INSERT INTO t1 VALUES(1)`,
	})
	// The upsert, which used to be the one open shape here: insert.c bumps
	// regRowCount only on the INSERT branch (its OE_Update arm falls through to
	// OE_Ignore's goto endOfLoop, insert.c:2362), so a DO UPDATE answers "rows
	// inserted" 0 while sqlite3_changes() is 1. The engine keeps regRowCount
	// itself now (DB.RowsInserted), so this is a plain differ.
	differ(t, "r32o count_changes upsert", []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t1 VALUES(1,'x')`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET b='z'`,
		`INSERT INTO t1 VALUES(2,'m') ON CONFLICT(a) DO UPDATE SET b='n'`,
		`INSERT INTO t1 VALUES(1,'p'),(3,'q'),(2,'r') ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
		`INSERT INTO t1 VALUES(1,'s') ON CONFLICT(a) DO NOTHING`,
		`INSERT INTO t1 VALUES(4,'t') ON CONFLICT(a) DO NOTHING`,
		`INSERT INTO t1 VALUES(1,'u') ON CONFLICT(a) DO UPDATE SET b='v' WHERE 0`,
		`SELECT a,b FROM t1 ORDER BY a`,
	})
	differ(t, "r32o count_changes trigger", []string{
		`CREATE TABLE t1(a)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES(new.a); END`,
		`PRAGMA count_changes=1`,
		`INSERT INTO t1 VALUES(1),(2)`,
		`SELECT count(*) FROM log`,
	})
	// The value is the statement's own count, which for a no-op DML is 0 and
	// which a rolled-back transaction still reports.
	differ(t, "r32o count_changes in a transaction", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA count_changes=1`,
		`BEGIN`,
		`INSERT INTO t1 VALUES(1)`,
		`ROLLBACK`,
		`SELECT count(*) FROM t1`,
	})
}

// TestR32OBusyTimeout pins pragma.c's PragTyp_BUSY_TIMEOUT arm, which is four
// lines and gets five things wrong if you guess: the parse is sqlite3Atoi
// (sqlite3GetInt32 -- digit-stopping, 32-bit overflow to zero), a value <= 0
// lands on 0 rather than being kept or rejected (sqlite3_busy_timeout only
// stores ms>0, and its else branch zeroes it), BOTH forms answer a row, the
// column is named "timeout" rather than after the pragma (mkpragmatab.tcl's
// "COLS: timeout"), and a FRESH connection answers 5000 rather than SQLite's own
// default of 0 -- mattn/go-sqlite3 runs "PRAGMA busy_timeout = 5000" itself on
// every Open (sqlite3.go, :1623), which is also exactly this engine's own
// lock wait (engine.BusyTimeout).
func TestR32OBusyTimeout(t *testing.T) {
	differ(t, "r32o busy_timeout", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA busy_timeout`, // a fresh connection: the driver's own 5000
		`PRAGMA busy_timeout=1234`,
		`PRAGMA busy_timeout`,
		`INSERT INTO t1 VALUES(1)`, // it survives a statement...
		`PRAGMA busy_timeout`,
		`BEGIN`,
		`PRAGMA busy_timeout=99`, // ...and is settable inside a transaction
		`COMMIT`,
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=0`,
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=-5`, // <= 0 zeroes it rather than storing it
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=7`,
		`PRAGMA busy_timeout=-5`,
		`PRAGMA busy_timeout`,
	})
	differ(t, "r32o busy_timeout parse", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA busy_timeout='12abc'`, // sqlite3GetInt32 STOPS at the first non-digit
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=0x10`, // ...and takes the hex form
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout='1.9'`,
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=2147483648`, // does not fit in 32 bits: 0, not a clamp
		`PRAGMA busy_timeout`,
		`PRAGMA busy_timeout=bogus`,
		`PRAGMA busy_timeout`,
	})
	// db->busyTimeout has one copy per connection and none per database.
	differ(t, "r32o busy_timeout qualifier", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA main.busy_timeout=500`,
		`PRAGMA busy_timeout`,
		`PRAGMA temp.busy_timeout=600`,
		`PRAGMA main.busy_timeout`,
		`PRAGMA busy_timeout`,
	})
}

// r32oReverseUnorderedOpen is the tracked set: the READOUTS over which
// "PRAGMA reverse_unordered_selects=1" still makes this engine answer different
// ROWS from the oracle. It is self-verifying in the useful direction -- a shape
// that starts agreeing FAILS, asking to be deleted, and a shape NOT listed that
// diverges fails outright -- so it is a measurement and a gate at once.
//
// Round 32 measured 9 of these 14 open, because the setter was accepted
// (execPragmaSafeNoop) and then really ignored. Round 33 (r33q) carried the flag
// on the connection (pragmaActiveConnFlagNames) and ported where.c:7126's
// "if( pWInfo->pOrderBy==0 && (db->flags & SQLITE_ReverseOrder)!=0 )
// whereReverseScanOrder(pWInfo)" into the SINGLE-TABLE planner
// (wherePlanSingleTableIndexOrder / wherePlanSingleIndexKey), closing 8: the
// subquery and the compound arms closed with the rest because each of their
// branches is itself a one-table scan that the same gate plans.
//
// The JOIN closed with them: whereReverseScanOrder loops over pTabList->nSrc, so
// the multi-table arm flips EVERY level's key (wherePlanMultiTableOrder), which
// emitJoinLoops already applies per level. All 14 readouts now agree; the map is
// kept, empty of open entries, because it is the gate that would catch a
// regression on any of them.
//
// The three shapes deliberately ABSENT from the open set are the answer to
// "which readouts hide this bucket", and they are the reason the corpus never
// scored it: a GROUP BY and a DISTINCT with no index both feed a SORTER, whose
// output order is the key's whatever direction the scan ran in, and an ORDER BY
// is exempt by construction. Round 30's aggregate battery was built entirely out
// of those shapes.
var r32oReverseUnorderedOpen = map[string]bool{
	"join":             false, // whereReverseScanOrder sets revMask for EVERY item
	"bare scan":        false,
	"scan with WHERE":  false,
	"LIMIT":            false, // changes WHICH rows, not only their order
	"LIMIT OFFSET":     false,
	"index scan":       false,
	"subquery":         false,
	"compound":         false,
	"LIMIT 1":          false,
	"turned off again": false,
	"GROUP BY":         false,
	"DISTINCT":         false,
	"ORDER BY":         false,
	"ORDER BY DESC":    false,
}

// TestR32OReverseUnorderedSelectsBattery measures the SCAN DIRECTION flip
// where.c:whereReverseScanOrder makes when pWInfo->pOrderBy==0:
//
//	if( pWInfo->pOrderBy==0 && (db->flags & SQLITE_ReverseOrder)!=0 ){
//	  whereReverseScanOrder(pWInfo);   /* pWInfo->revMask |= MASKBIT(ii) for
//	                                      EVERY FROM item */
//	}
//
// The readout varies deliberately (BRIEF32's rule): only the QUERY's own result
// is compared, so the flag pragmas' own shape divergence -- the setter answers
// an empty result set on the oracle and "ok" here, the getter a row -- cannot
// mask which SELECTS actually come back with different rows.
func TestR32OReverseUnorderedSelectsBattery(t *testing.T) {
	setup := []string{
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c INT)`,
		`INSERT INTO t1 VALUES(1,'a',10),(2,'b',20),(3,'c',30),(4,'d',40),(5,'e',50)`,
		`CREATE INDEX i1 ON t1(c)`,
		`CREATE TABLE t2(x INT, y TEXT)`,
		`INSERT INTO t2 VALUES(1,'p'),(2,'q'),(3,'r')`,
		`PRAGMA reverse_unordered_selects=1`,
	}
	shapes := []struct{ name, query string }{
		{"bare scan", `SELECT a,b FROM t1`},
		{"scan with WHERE", `SELECT a FROM t1 WHERE a>1`},
		{"LIMIT", `SELECT a FROM t1 LIMIT 2`},
		{"LIMIT OFFSET", `SELECT a FROM t1 LIMIT 2 OFFSET 1`},
		{"index scan", `SELECT c FROM t1 WHERE c>15`},
		{"join", `SELECT t1.a, t2.x FROM t1, t2 WHERE t1.a=t2.x`},
		{"GROUP BY", `SELECT b, count(*) FROM t1 GROUP BY b`},
		{"DISTINCT", `SELECT DISTINCT b FROM t1`},
		{"subquery", `SELECT a FROM (SELECT a FROM t1) LIMIT 2`},
		{"compound", `SELECT a FROM t1 UNION ALL SELECT x FROM t2`},
		{"ORDER BY", `SELECT a FROM t1 ORDER BY a`},
		{"ORDER BY DESC", `SELECT a FROM t1 ORDER BY a DESC`},
		{"LIMIT 1", `SELECT a FROM t1 LIMIT 1`},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			r32oQueryRowsAgree(t, s.name, append(append([]string{}, setup...), s.query))
		})
	}
	// ...and the flag really is a flag: turning it back off restores the order.
	t.Run("turned off again", func(t *testing.T) {
		r32oQueryRowsAgree(t, "turned off again", append(append([]string{}, setup...),
			`PRAGMA reverse_unordered_selects=0`, `SELECT a FROM t1 LIMIT 2`))
	})
}

// r32oQueryRowsAgree compares ONLY the last statement's result against the
// oracle's, and reconciles the verdict with r32oReverseUnorderedOpen.
func r32oQueryRowsAgree(t *testing.T, name string, stmts []string) {
	t.Helper()
	oracle := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	ob, _ := json.Marshal(oracle[len(oracle)-1])
	gb, _ := json.Marshal(got[len(got)-1])
	agrees := string(ob) == string(gb)
	open, tracked := r32oReverseUnorderedOpen[name]
	switch {
	case !tracked:
		t.Fatalf("shape %q is not in r32oReverseUnorderedOpen -- add it with its measured verdict", name)
	case open && agrees:
		t.Errorf("%s no longer diverges -- reverse_unordered_selects reached this shape; remove it from r32oReverseUnorderedOpen\n  sql: %s\n  both: %s",
			name, stmts[len(stmts)-1], ob)
	case open:
		t.Logf("KNOWN OPEN reverse_unordered_selects/%s\n  sql:    %s\n  cgo:    %s\n  musql: %s",
			name, stmts[len(stmts)-1], ob, gb)
	case !agrees:
		t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:     %s\n  cgo:     %s\n  musql:  %s",
			name, stmts[len(stmts)-1], ob, gb)
	}
}
