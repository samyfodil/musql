package compat

// Tests PRAGMA automatic_index behavior. The pragma affects query plan
// and row order, which only database/sql can observe.

import (
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r28PragmaShape compares one pragma statement's Query answer against the
// oracle, cell for cell, and requires musql to ANSWER it.
//
// It deliberately does NOT use prDiffer/prDifferQ (pragma_rows_test.go), whose
// whole rule is that a musql decline is tolerated -- correct for a sweep over
// every pragma name, where a decline is an honest gap. It is the wrong rule
// here: this pragma is now claimed to be implemented, and a decline would make
// the assertion vacuous. Declining is what the getter used to do.
func r28PragmaShape(t *testing.T, name string, setup []string, q string) {
	t.Helper()
	dir := t.TempDir()
	got := prQuery(t, "sqlite", filepath.Join(dir, "musql.db"), setup, q)
	want := prQuery(t, "sqlite3", filepath.Join(dir, "cgo.db"), setup, q)
	if got == "ERR" {
		t.Errorf("%s: musql DECLINED a pragma it implements; cgo answered %s", name, want)
		return
	}
	if got != want {
		t.Errorf("%s DIVERGES\n  cgo:    %s\n  musql: %s", name, want, got)
	}
}

// TestR28AutomaticIndexPragmaRowShape pins the pragma's own answer: the getter
// is one row of one INTEGER column named "automatic_index" (pragma.c's
// PragTyp_FLAG arm over SQLITE_AutoIndex, which main.c sets by default, so a
// fresh connection reads 1), and the setter answers no rows at all.
//
// It reads back through a SECOND getter after every setter, which is the half
// that a no-op accept passes and a real implementation must not: "accepted" and
// "silently ignored" look identical until something reports the value.
func TestR28AutomaticIndexPragmaRowShape(t *testing.T) {
	for _, q := range []string{
		"PRAGMA automatic_index",
		"PRAGMA main.automatic_index",
		"PRAGMA temp.automatic_index",
		"PRAGMA automatic_index = 0",
		"PRAGMA automatic_index = 1",
		"PRAGMA automatic_index = off",
		"PRAGMA automatic_index = ON",
		"PRAGMA automatic_index = no",
		"PRAGMA automatic_index = true",
		"PRAGMA automatic_index(0)",
		"PRAGMA automatic_index(1)",
		// The qualifier is IGNORED -- it is a db->flags bit, not per-database --
		// so a qualified setter must move the bare getter too.
		"PRAGMA main.automatic_index = 0",
		"PRAGMA temp.automatic_index = 0",
	} {
		r28PragmaShape(t, q, prSetup, q)
		r28PragmaShape(t, q+" [readback]", append(append([]string{}, prSetup...), q),
			"PRAGMA automatic_index")
	}
}

// TestR28AutomaticIndexNonCanonicalValue used to pin a DECLINE: C SQLite
// parses the value with sqlite3GetBoolean, whose getSafetyLevel maps a leading
// DIGIT through atoi (so "2" is ON) and every unrecognised word to the caller's
// default (so "bogus" and "-1" are OFF), and those two go in OPPOSITE
// directions. That is a description of the algorithm, not a reason to guess:
// it is PORTED now (pragmaGetBoolean), so every spelling is compared against
// the oracle here -- both the setter's own shape and the getter that follows.
func TestR28AutomaticIndexNonCanonicalValue(t *testing.T) {
	var stmts []string
	for _, v := range []string{"2", "bogus", "-1", "''", "NULL", "0x10", "256", "full", "extra"} {
		stmts = append(stmts, "PRAGMA automatic_index="+v, "PRAGMA automatic_index")
	}
	differ(t, "automatic_index value spellings", stmts)
}

// r28AutoIdxOrderCases are the shapes whose OUTPUT ORDER the automatic index
// decides. Each is two unindexed tables joined on an equality, read back through
// a channel that reports the inner loop's visit order (group_concat, a bare
// column in a grouped aggregate, LIMIT without a totally-ordering ORDER BY).
//
// The observer values are a random-looking PERMUTATION rather than ascending
// tags, for the reason joinorder_r27_battery_test.go's r27Table documents: with
// ascending tags the automatic index's key order and the rowid scan order are
// byte-identical, and the divergence hides. The join keys REPEAT, because a key
// matching exactly one row also cannot distinguish two visit orders.
var r28AutoIdxOrderCases = []struct {
	name  string
	setup []string
	query string
}{
	{"gc-flat", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k"},

	{"gc-grouped", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT a.k, count(*), group_concat(b.o) FROM a,b WHERE a.k=b.k GROUP BY a.k ORDER BY 1"},

	{"bare-grouped", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT a.k, b.o FROM a,b WHERE a.k=b.k GROUP BY a.k ORDER BY 1"},

	{"limit", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT a.o, b.o FROM a,b WHERE a.k=b.k LIMIT 4"},

	// The KEY's covering columns, which is where colUsed decides the order: with
	// b.o never named, b's index is keyed (k, rowid) and its matches come out in
	// rowid order even with the index ON -- so this case must NOT move when the
	// pragma changes, and a fix that keyed the index unconditionally would break
	// it. See wherePlanColUsed (engine/where_plan_gate.go).
	{"colused-empty", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT group_concat(a.o) FROM a,b WHERE a.k=b.k"},

	// A single-table restriction alongside the equality: the shape SQLite hoists
	// into the OUTER loop and this package's pre-port rule pushed to the INNER
	// one. It is here because it is exactly where a "just decline the port when
	// the flag is off" implementation diverges (measured 51/600 that way).
	{"filter-plus-eq", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(NULL,'a5'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k AND a.k IS NOT NULL"},

	// Three tables, two equalities: two auto-index levels, and the level the
	// solver picks for each is what the flag removes.
	{"three-table", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, j, o)",
		"CREATE TABLE c(j, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,7,'b9'),(1,8,'b4'),(2,7,'b7'),(1,7,'b1')",
		"INSERT INTO c VALUES(7,'c5'),(8,'c2'),(7,'c1')",
	}, "SELECT group_concat(a.o||b.o||c.o) FROM a,b,c WHERE a.k=b.k AND b.j=c.j"},

	// A LEFT join, whose ON-clause equality can only drive the index of the item
	// the join NULL-extends (constraintCompatibleWithOuterJoin).
	{"left-join", []string{
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(3,'a5'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
	}, "SELECT group_concat(a.o||'-'||coalesce(b.o,'#')) FROM a LEFT JOIN b ON a.k=b.k"},
}

// TestR28AutomaticIndexRowOrder is the BEHAVIOUR gate, and the one that would
// have caught the no-op: for every shape, the answer must match the oracle with
// the pragma ON (the default), with it explicitly ON, and with it OFF. A no-op
// accept passes the first two and fails the third.
//
// It runs through database/sql (the `run` worker), so it also gates the driver's
// carry: the pragma and the query are separate autocommit statements, and the
// driver opens a fresh engine session for each, so a flag that lived only on the
// session that set it would be gone by the time the join is compiled.
func TestR28AutomaticIndexRowOrder(t *testing.T) {
	for _, c := range r28AutoIdxOrderCases {
		for _, set := range []string{"", "PRAGMA automatic_index=ON", "PRAGMA automatic_index=OFF"} {
			var stmts []string
			if set != "" {
				stmts = append(stmts, set)
			}
			stmts = append(stmts, c.setup...)
			stmts = append(stmts, c.query)
			differ(t, fmt.Sprintf("r28-autoidx/%s/%q", c.name, set), stmts)
		}
	}
}

// TestR28AutomaticIndexOffSurvivesStatements pins the CARRY specifically, as its
// own case rather than as a side effect of the one above: the setter is followed
// by DDL, DML, a COMMIT and a ROLLBACK before the join that observes it. Real
// SQLite's flag is a db->flags bit and pragma.c clears no flag at COMMIT or
// ROLLBACK (it masks only SQLITE_ForeignKeys out while autoCommit is 0), so the
// value must still be OFF at the end -- read back through the getter AND through
// the row order, since only the second one fails if the flag is remembered but
// never consulted.
func TestR28AutomaticIndexOffSurvivesStatements(t *testing.T) {
	differ(t, "r28-autoidx-carry", []string{
		"PRAGMA automatic_index=OFF",
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"BEGIN",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7')",
		"COMMIT",
		"BEGIN",
		"INSERT INTO b VALUES(1,'b1'),(2,'b2')",
		"ROLLBACK",
		"INSERT INTO b VALUES(1,'b1'),(2,'b2')",
		"PRAGMA automatic_index",
		"SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k",
		// ...and back ON again on the same connection, which must restore the
		// index -- a one-way flag would pass every case above.
		"PRAGMA automatic_index=ON",
		"PRAGMA automatic_index",
		"SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k",
	})
}

// TestR28AutomaticIndexInsertSelect runs the same observation through a WRITE
// statement's SELECT, whose join is compiled on the write path rather than the
// read one -- a different pager (SnapshotPager rather than the driver's Open),
// and so a second, independent place the flag has to have been carried to.
func TestR28AutomaticIndexInsertSelect(t *testing.T) {
	for _, set := range []string{"PRAGMA automatic_index=ON", "PRAGMA automatic_index=OFF"} {
		differ(t, "r28-autoidx-insert-select/"+set, []string{
			set,
			"CREATE TABLE a(k, o)",
			"CREATE TABLE b(k, o)",
			"CREATE TABLE dst(v)",
			"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
			"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
			"INSERT INTO dst SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k",
			"SELECT v FROM dst",
		})
	}
}

// TestR28AutomaticIndexQualifiedIgnored pins the per-CONNECTION rule against an
// ATTACHed database, where "per-database" and "per-connection" finally differ:
// setting it through the attachment's qualifier must change the flag the MAIN
// database's own join then plans under.
func TestR28AutomaticIndexQualifiedIgnored(t *testing.T) {
	aux := filepath.Join(t.TempDir(), "aux.db")
	differ(t, "r28-autoidx-qualified", []string{
		"ATTACH DATABASE '" + aux + "' AS aux",
		"CREATE TABLE a(k, o)",
		"CREATE TABLE b(k, o)",
		"INSERT INTO a VALUES(1,'a3'),(2,'a1'),(1,'a2')",
		"INSERT INTO b VALUES(1,'b9'),(1,'b4'),(2,'b7'),(1,'b1'),(2,'b2')",
		"PRAGMA aux.automatic_index=OFF",
		"PRAGMA automatic_index",
		"PRAGMA main.automatic_index",
		"PRAGMA aux.automatic_index",
		"SELECT group_concat(a.o||'-'||b.o) FROM a,b WHERE a.k=b.k",
	})
}
