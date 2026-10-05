package engine

// This gate verifies that statement verbs compile to real opcodes,
// not to a one-instruction program that reroutes execution.

import (
	"strings"
	"testing"
)

// maxRoutedVerbs is how many verbCases still reroute. It may only decrease.
const maxRoutedVerbs = 0

// verbCases tests one statement per verb; they are compiled, not executed.
var verbCases = []writeShapeCase{
	{"PRAGMA", nil, `PRAGMA user_version = 3`},
	{"CREATE VIRTUAL TABLE", nil, `CREATE VIRTUAL TABLE vt USING fts4(x)`},
	{"CREATE TRIGGER", []string{`CREATE TABLE t(a)`, `CREATE TABLE log(x)`},
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
	{"DROP TRIGGER", []string{`CREATE TABLE t(a)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`DROP TRIGGER tr`},
	{"DROP TABLE", []string{`CREATE TABLE t(a)`}, `DROP TABLE t`},
	{"ANALYZE", []string{`CREATE TABLE t(a)`}, `ANALYZE`},
	{"REINDEX", []string{`CREATE TABLE t(a)`, `CREATE INDEX ix ON t(a)`}, `REINDEX`},
	{"VACUUM", []string{`CREATE TABLE t(a)`}, `VACUUM`},
	{"ATTACH", nil, `ATTACH DATABASE ':memory:' AS aux`},
	{"DETACH", nil, `DETACH DATABASE aux`},
	{"BEGIN", nil, `BEGIN`},
	{"BEGIN IMMEDIATE", nil, `BEGIN IMMEDIATE TRANSACTION`},
	{"COMMIT", nil, `COMMIT`},
	{"END", nil, `END TRANSACTION`},
	{"ROLLBACK", nil, `ROLLBACK`},
	{"ROLLBACK TO", nil, `ROLLBACK TO sp`},
	{"SAVEPOINT", nil, `SAVEPOINT sp`},
	{"RELEASE", nil, `RELEASE sp`},
	{"WITH ... DELETE", []string{`CREATE TABLE t(a)`},
		`WITH c(v) AS (VALUES(1)) DELETE FROM t WHERE a IN (SELECT v FROM c)`},
	{"WITH ... UPDATE", []string{`CREATE TABLE t(a)`},
		`WITH c(v) AS (VALUES(1)) UPDATE t SET a=2 WHERE a IN (SELECT v FROM c)`},
	{"WITH ... INSERT", []string{`CREATE TABLE t(a)`},
		`WITH c(v) AS (VALUES(1)) INSERT INTO t SELECT v FROM c`},
	// EXPLAIN is routed to the query side, not compiled.
	{"EXPLAIN", nil, `EXPLAIN SELECT 1`},
}

// TestEveryControlVerbCompilesToRealOpcodes gates that each control/DDL/utility
// verb below produces real opcodes -- logged by name, so the failure says WHICH
// opcode a verb compiles to -- or a clean compile-time error. Nothing is
// EXECUTED here; TestControlVerbErrorTextAndState below covers behaviour.
func TestEveryControlVerbCompilesToRealOpcodes(t *testing.T) {
	var still []string
	for _, tc := range verbCases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("[%s] create: %v", tc.name, err)
		}
		bad := false
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Errorf("[%s] setup %q: %v", tc.name, s, err)
				bad = true
				break
			}
		}
		if bad {
			db.Discard()
			continue
		}
		_, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			// A hard error is a LEGITIMATE outcome, and for EXPLAIN it is the
			// CORRECT one: RULE #1 prescribes an error for a shape the compiler
			// cannot lower. Either way the statement is not routed elsewhere,
			// which is what this gate measures.
			t.Logf("  %-22s -> compile error (a legitimate outcome): %v", tc.name, cerr)
			db.Discard()
			continue
		}
		db.Discard()
	}

	switch {
	case len(still) > maxRoutedVerbs:
		t.Errorf("RULE #1 REGRESSION: %d verbs do not compile to real opcodes, ratchet is %d.\n"+
			"Still routed: %s\n"+
			"A verb the compiler cannot lower must be an ERROR, not a fallback. Do NOT raise the ratchet.",
			len(still), maxRoutedVerbs, strings.Join(still, ", "))
	case len(still) < maxRoutedVerbs:
		t.Logf("PROGRESS: %d verbs are still routed, below the ratchet of %d -- "+
			"lower maxRoutedVerbs in the same commit.", len(still), maxRoutedVerbs)
	}
}

// disasmOps names a compiled program's opcodes, so the log above shows WHICH
// real opcode each verb compiles to rather than only that it compiled.
func disasmOps(prog *Program) []string {
	out := make([]string, 0, len(prog.Insns))
	for i := range prog.Insns {
		out = append(out, prog.Insns[i].Op.String())
	}
	return out
}

// unsupportedText is compileWriteProgram's dispatch-tail error, which used to
// be reported by the write path's keyword dispatch at RUN time and is now
// reported at COMPILE time. There is one text (unsupportedStatementErr,
// insert_write.go); this pins it.
const unsupportedText = `engine: unsupported statement (this write path only supports CREATE TABLE/VIEW/TRIGGER, ` +
	`CREATE/DROP INDEX, DROP TABLE/VIEW/TRIGGER, INSERT, UPDATE, DELETE, ATTACH/DETACH, ` +
	`BEGIN/COMMIT/END/ROLLBACK, and SAVEPOINT/RELEASE/ROLLBACK TO): `

// TestControlVerbErrorTextAndState pins what the differential harness cannot:
// the exact ERROR TEXT each verb produces (differ() compares only whether a
// statement errored -- drivers word errors differently) and the transaction
// state it leaves behind. Every string below was measured on main@7767efb
// BEFORE the verbs were lowered, and must be unchanged after: they got a new
// ROUTE, not new behaviour.
func TestControlVerbErrorTextAndState(t *testing.T) {
	type step struct {
		sql       string
		wantErr   string // "" means it must succeed
		wantInTxn bool
		wantSaveN int
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"commit with nothing open", []step{{`COMMIT`, "engine: cannot commit - no transaction is active", false, 0}}},
		{"end with nothing open", []step{{`END`, "engine: cannot commit - no transaction is active", false, 0}}},
		{"rollback with nothing open", []step{{`ROLLBACK`, "engine: cannot rollback - no transaction is active", false, 0}}},
		{"release of an unknown savepoint", []step{{`RELEASE nosuch`, "engine: no such savepoint: nosuch", false, 0}}},
		{"rollback to an unknown savepoint", []step{{`ROLLBACK TO nosuch`, "engine: no such savepoint: nosuch", false, 0}}},
		{"nested begin", []step{
			{`BEGIN`, "", true, 0},
			{`BEGIN`, "engine: cannot start a transaction within a transaction", true, 0},
		}},
		{"savepoint ladder", []step{
			{`BEGIN`, "", true, 0},
			{`SAVEPOINT sp`, "", true, 1},
			{`ROLLBACK TO sp`, "", true, 1},
			{`RELEASE sp`, "", true, 0},
			{`COMMIT`, "", false, 0},
		}},
		// A savepoint verb whose NAME is missing ENDS at end of input, and every
		// syntax error there is "incomplete input" (parse.y:44-51) -- verified
		// against 3.53.3, which says exactly that for both of these. The other
		// malformed spellings below still fall through to the shared
		// "unsupported statement" text (C reports `near "X": syntax error` for
		// those too; BEGIN/COMMIT's optional-name grammar is not walked yet).
		{"savepoint with no name", []step{{`SAVEPOINT`, "engine: incomplete input", false, 0}}},
		{"release with no name", []step{{`RELEASE`, "engine: incomplete input", false, 0}}},
		{"begin with trailing garbage", []step{{`BEGIN garbage`, unsupportedText + `"BEGIN garbage"`, false, 0}}},
		{"transaction name that is a number", []step{{`BEGIN TRANSACTION 123`, unsupportedText + `"BEGIN TRANSACTION 123"`, false, 0}}},
		{"commit with an unqualified name", []step{{`COMMIT foo`, unsupportedText + `"COMMIT foo"`, false, 0}}},

		{"detach of an unknown alias", []step{{`DETACH nosuch`, "engine: no such database: nosuch", false, 0}}},
		{"detach of main", []step{{`DETACH DATABASE main`, "engine: cannot detach database main", false, 0}}},
		{"analyze of an unknown table", []step{{`ANALYZE nosuchtable`, "engine: no such table: nosuchtable", false, 0}}},
		{"reindex of an unknown object", []step{{`REINDEX nosuchindex`, "engine: unable to identify the object to be reindexed", false, 0}}},
		{"vacuum of an unknown schema", []step{{`VACUUM nosuchschema`, "engine: unknown database nosuchschema", false, 0}}},
		{"create trigger on a missing table", []step{
			{`CREATE TRIGGER tr AFTER INSERT ON nosuchtable BEGIN SELECT 1; END`, "engine: no such table: main.nosuchtable", false, 0},
		}},
		{"drop of an unknown trigger", []step{{`DROP TRIGGER nosuchtrigger`, "engine: no such trigger: nosuchtrigger", false, 0}}},
		{"drop if exists of an unknown trigger", []step{{`DROP TRIGGER IF EXISTS nosuchtrigger`, "", false, 0}}},
		{"create virtual table with an unknown module", []step{
			{`CREATE VIRTUAL TABLE vt USING nosuchmodule(x)`, "engine: no such module: nosuchmodule", false, 0},
		}},
		{"pragma that this engine treats as a no-op", []step{{`PRAGMA nosuchpragmaatall = 3`, "", false, 0}}},

		// The dispatch tail, now a COMPILE-time error carrying the same text
		// that used to be reported at run time.
		{"drop of an unknown object kind", []step{{`DROP FOO x`, unsupportedText + `"DROP FOO x"`, false, 0}}},
		{"bare drop", []step{{`DROP`, unsupportedText + `"DROP"`, false, 0}}},
		// EXPLAIN is NOT in the dispatch tail: it describes a statement and
		// runs nothing, so Exec answers it and discards its rows, exactly as
		// sqlite3_exec("EXPLAIN ...") does (explain.go). It opens no
		// transaction and writes nothing, which is what the two fields after
		// the empty wantErr assert. See TestExplainThroughEngineExec for the
		// rows themselves.
		{"explain is answered, not dispatched", []step{{`EXPLAIN SELECT 1`, "", false, 0}}},
		{"explain query plan likewise", []step{{`EXPLAIN QUERY PLAN SELECT 1`, "", false, 0}}},
		{"a select is not a write", []step{{`SELECT 1`, unsupportedText + `"SELECT 1"`, false, 0}}},
		{"a statement that is only punctuation", []step{{`;`, unsupportedText + `";"`, false, 0}}},
		{"a statement that is only a number", []step{{`123`, unsupportedText + `"123"`, false, 0}}},

		// Whitespace/comments only: a NO-OP, not an error -- C SQLite
		// prepares such input as a NULL statement.
		{"empty statement", []step{{``, "", false, 0}}},
		{"whitespace only", []step{{`   `, "", false, 0}}},
		{"comment only", []step{{`-- just a comment`, "", false, 0}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for i, st := range tc.steps {
				got := db.Exec(st.sql)
				switch {
				case st.wantErr == "" && got != nil:
					t.Fatalf("step %d %q: unexpected error %v", i, st.sql, got)
				case st.wantErr != "" && got == nil:
					t.Fatalf("step %d %q: want error %q, got success", i, st.sql, st.wantErr)
				case st.wantErr != "" && got.Error() != st.wantErr:
					t.Fatalf("step %d %q:\n got %q\nwant %q", i, st.sql, got.Error(), st.wantErr)
				}
				if db.inTransaction() != st.wantInTxn {
					t.Fatalf("step %d %q: inTransaction=%v, want %v", i, st.sql, db.inTransaction(), st.wantInTxn)
				}
				if len(db.savepoints) != st.wantSaveN {
					t.Fatalf("step %d %q: %d savepoints, want %d", i, st.sql, len(db.savepoints), st.wantSaveN)
				}
			}
		})
	}
}

// TestControlVerbGuardsUnchangedByRouting pins ddlKind.guards() -- the split
// that decides which of the two DDL interlocks each OpDdl kind runs.
//
// Before this batch, PRAGMA/VACUUM/ATTACH/DETACH reached their implementations
// through the write path's keyword dispatch (execNonRowStatement, since
// deleted), whose guard switch named only CREATE/DROP/ALTER (both interlocks)
// and ANALYZE/REINDEX (the fts5 one), so those four ran NEITHER. Routing them through OpDdl, whose guard block used to
// be unconditional, would have started declining them -- a behaviour change, in
// the direction of a spurious error. This gate is both halves: the four must
// still be accepted while a direct sqlite_master write is outstanding, and a
// DROP of the very object that edit is keyed on must still be declined.
func TestControlVerbGuardsUnchangedByRouting(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`PRAGMA writable_schema=ON`,
		`UPDATE sqlite_schema SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("setup %q: %v", s, e)
		}
	}
	if !db.writableSchemaEditsActive() {
		t.Fatal("setup left no outstanding direct sqlite_master write -- the gate would pass vacuously")
	}
	// Neither interlock applies to these three.
	for _, s := range []string{
		`PRAGMA user_version = 3`,
		`ATTACH DATABASE ':memory:' AS aux`,
		`DETACH DATABASE aux`,
	} {
		if e := db.Exec(s); e != nil {
			t.Errorf("%q was declined while a direct sqlite_master write is outstanding: %v", s, e)
		}
	}
	// ...while a DROP of the edited object's own row still is declined, which
	// is what says the guard was narrowed rather than removed.
	if e := db.Exec(`DROP TABLE t`); e == nil {
		t.Error("DROP TABLE of the edited object was accepted while its sqlite_master edit is outstanding")
	}
}

// ddlKindGuardWant is the classification every ddlKind must have. It is spelled
// out here, not derived from guards(), so the two have to agree: a kind added
// to the enum without a case in guards() has no entry here either, and the
// walk below names it.
var ddlKindGuardWant = map[ddlKind][2]bool{
	ddlCreateTable:        {true, true},
	ddlCreateIndex:        {true, true},
	ddlCreateView:         {true, true},
	ddlDropIndex:          {true, true},
	ddlDropTable:          {true, true},
	ddlDropView:           {true, true},
	ddlAlterTable:         {true, true},
	ddlCreateTrigger:      {true, true},
	ddlDropTrigger:        {true, true},
	ddlCreateVirtualTable: {true, true},
	ddlAnalyze:            {false, true},
	ddlReindex:            {false, true},
	ddlPragma:             {false, false},
	ddlVacuum:             {false, false},
	ddlAttach:             {false, false},
	ddlDetach:             {false, false},
}

// TestDdlKindGuardsAreExhaustive walks every ddlKind and fails on one that
// guards() does not NAME. Falling off that switch used to hand a kind the
// PRAGMA/VACUUM/ATTACH/DETACH answer -- neither interlock -- by omission, which
// is the wrong default for a DDL verb: the ten CREATE/DROP/ALTER kinds all want
// both. This is the check that makes naming them load-bearing rather than
// decorative.
func TestDdlKindGuardsAreExhaustive(t *testing.T) {
	if ddlKindCount == 0 {
		t.Fatal("ddlKindCount is 0 -- the walk below would be vacuous")
	}
	for k := ddlKind(0); k < ddlKindCount; k++ {
		want, named := ddlKindGuardWant[k]
		if !named {
			t.Errorf("ddlKind %d is not classified: give it a case in guards() "+
				"(vdbe_write.go) and an entry in ddlKindGuardWant. Do NOT let it fall "+
				"off the switch -- a DDL verb that silently runs neither interlock is "+
				"the bug this gate exists for.", int(k))
			continue
		}
		ws, fts := k.guards()
		if [2]bool{ws, fts} != want {
			t.Errorf("ddlKind %d: guards() = (%v,%v), want (%v,%v)", int(k), ws, fts, want[0], want[1])
		}
	}
	if len(ddlKindGuardWant) != int(ddlKindCount) {
		t.Errorf("ddlKindGuardWant has %d entries for %d kinds -- a stale entry outlives its kind",
			len(ddlKindGuardWant), int(ddlKindCount))
	}
}

// TestControlVerbFts5GuardUnchangedByRouting is the second half of the same
// split, and the one with teeth: fts5TxnPendingDDLGuard declines
// UNCONDITIONALLY on the keyword alone (fts5_txn.go), so subjecting the newly
// routed verbs to it would turn a PRAGMA or an ATTACH issued while a
// secure-delete %_config bump is deferred into a spurious error. Measured on
// main: those two are accepted there and CREATE/ANALYZE are declined, and that
// is what must still hold.
func TestControlVerbFts5GuardUnchangedByRouting(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`INSERT INTO ft(body) VALUES('one two'),('three four')`,
		`INSERT INTO ft(ft, rank) VALUES('secure-delete', 1)`,
		`BEGIN`,
		`DELETE FROM ft WHERE rowid=1`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("setup %q: %v", s, e)
		}
	}
	if len(db.fts5TxnPending) == 0 {
		t.Fatal("setup deferred no secure-delete bump -- the gate would pass vacuously")
	}
	for _, s := range []string{
		`PRAGMA user_version = 3`,
		`ATTACH DATABASE ':memory:' AS aux`,
		`DETACH DATABASE aux`,
	} {
		if e := db.Exec(s); e != nil {
			t.Errorf("%q was declined while an fts5 secure-delete bump is deferred: %v", s, e)
		}
	}
	for _, s := range []string{
		`CREATE TABLE unrelated(x)`,
		`ANALYZE`,
	} {
		if e := db.Exec(s); e == nil {
			t.Errorf("%q was accepted while an fts5 secure-delete bump is deferred", s)
		}
	}
}
