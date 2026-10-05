package engine

// Tests engine-side INSERT ... SELECT ... RETURNING compilation to bytecode.

import "testing"

// insertSelectReturningShapes covers statement shapes tested by the harness.
var insertSelectReturningShapes = []conflictShapeCase{
	// The plain promotion: a SELECT source with no OR-clause at all.
	{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(id INTEGER PRIMARY KEY, a, b, dd DEFAULT 'x')`},
		`INSERT INTO d(a,b) SELECT a,b FROM s ORDER BY rowid RETURNING id,a,b,dd`},
	{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a,b)`},
		`INSERT INTO d SELECT a,b FROM s RETURNING *`},
	{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a,b)`},
		`INSERT INTO d SELECT a,b FROM s RETURNING rowid, a, a+1, 'lit'`},
	{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a,b)`},
		`INSERT INTO d SELECT a,b FROM s WHERE b='k' ORDER BY a RETURNING a`},
	{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a,b)`},
		`INSERT INTO d SELECT a,b FROM s UNION ALL SELECT a,b FROM s RETURNING a,b`},

	// ---- THE CROSSING: an OR-clause AND a SELECT source AND RETURNING. ----
	// Neither cluster had this combination; it is the whole point of this file.
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(k,b)`, `CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`},
		`INSERT OR IGNORE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a NOT NULL, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a)`, `CREATE TABLE dst(a, b, CHECK(a<10))`},
		`INSERT OR IGNORE INTO dst SELECT a,'k' FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(k,b)`, `CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`},
		`INSERT OR REPLACE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b,rowid`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a, b NOT NULL DEFAULT 'dd')`},
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR FAIL INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a NOT NULL, b)`},
		`INSERT OR FAIL INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR ABORT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR ROLLBACK INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},

	// A non-integer rowid under an OR-clause, SELECT-sourced. These COMPILE and
	// must ABORT the statement: C SQLite's one-operand
	// "sqlite3VdbeAddOp1(v, OP_MustBeInt, regRowid);" (insert.c:1534) carries no
	// onError, so no OR-clause softens it -- the trap that blocked cluster H2
	// over a VALUES source. Listed HERE because the abort has to come from the
	// COMPILED program, and a differential gate cannot tell which route
	// aborted.
	{[]string{`CREATE TABLE src(k,b)`, `CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`},
		`INSERT OR IGNORE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`},
	{[]string{`CREATE TABLE src(k,b)`, `CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`},
		`INSERT OR FAIL INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`},
	{[]string{`CREATE TABLE src(k,b)`, `CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`},
		`INSERT OR REPLACE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`},

	// A table whose OWN constraint declares an ON CONFLICT default, and the
	// explicit-OR override of one.
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE ON CONFLICT IGNORE, b)`},
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE ON CONFLICT REPLACE, b)`},
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE ON CONFLICT IGNORE, b)`},
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a NOT NULL ON CONFLICT IGNORE, b)`},
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},

	// STRICT x OR-clause x SELECT source.
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a INT, b TEXT) STRICT`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a INT PRIMARY KEY, b TEXT) STRICT`},
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`},

	// Generated columns, including one derived from the auto-assigned INTEGER
	// PRIMARY KEY (rederiveGeneratedFromRowid).
	{[]string{`CREATE TABLE src(v)`, `CREATE TABLE dst(id INTEGER PRIMARY KEY, v, g AS (id*2))`},
		`INSERT INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`},
	{[]string{`CREATE TABLE src(v)`, `CREATE TABLE dst(id INTEGER PRIMARY KEY, v, g AS (id*2) STORED)`},
		`INSERT INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`},
	{[]string{`CREATE TABLE src(v)`, `CREATE TABLE dst(id INTEGER PRIMARY KEY, v UNIQUE, g AS (id*100+v))`},
		`INSERT OR IGNORE INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`},

	// DEFAULTs, a narrower column list, RETURNING * and an expression.
	{[]string{`CREATE TABLE src(a)`, `CREATE TABLE dst(a UNIQUE, d DEFAULT 'dflt', e DEFAULT 7)`},
		`INSERT OR IGNORE INTO dst(a) SELECT a FROM src ORDER BY rowid RETURNING a,d,e`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING *, rowid, a||b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src WHERE b='k' ORDER BY rowid RETURNING a,b`},
	{[]string{`CREATE TABLE src(a,b)`, `CREATE TABLE dst(a UNIQUE, b)`},
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src UNION ALL SELECT a,b FROM src RETURNING a,b`},
}

func TestInsertSelectReturningCompilesToBytecode(t *testing.T) {
	for _, tc := range insertSelectReturningShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: %q should COMPILE, got error: %v.\n"+
				"The differential gate in compat-harness/insert_select_returning_or_test.go cannot see "+
				"this -- which is exactly why this assertion exists.", tc.stmt, err)
		}
	}
}

// insertSelectReturningDeclined: the shapes cluster H4 deliberately leaves
// unlowered. Asserting they still decline is what stops a later "just
// widen the gate" change from compiling them by accident -- each is refused for
// a reason the differential harness cannot observe on its own.
var insertSelectReturningDeclined = []struct {
	why string
	conflictShapeCase
}{
	// "INSERT ... ON CONFLICT ... RETURNING (upsert)" and its SELECT-sourced
	// twin were the first two entries here and are GONE, on both row sources at
	// once. The reason they were declined -- emitUpsertTail REPLACES the row
	// tail that would emit the RETURNING, so compiling them would SILENTLY DROP
	// the clause -- was a statement about this compiler, not about the C, and
	// the tail emits the block itself now: once per ARM, because C codes the
	// RETURNING trigger twice for an upsert (inside sqlite3UpsertDoUpdate's
	// nested sqlite3Update, and in sqlite3Insert's own AFTER block) and reports
	// the UPDATED row from one and the INSERTED row from the other. Settled
	// against the oracle first: compat-harness/upsert_returning_test.go.
	// The SUBQUERY-in-RETURNING half that was left here is served too: each
	// arm now has its own run-once slots, shared across an unrolled VALUES
	// list's tuples (returningSite.onceGroup), and
	// compat-harness/upsert_returning_codegen_test.go compares it with the
	// oracle.
	// "subquery in RETURNING that C evaluates ONCE" was here, as "the OTHER half
	// of that same rule". It COMPILES: emitReturning implements BOTH lifetimes,
	// and the once one is spelled by NOT clearing the block's sub-cache at a loop
	// site -- exactly what expr.c:3889's OP_Once does for C.
	//
	// A MIXED list replaced it and is gone too: the sub-cache discipline was per
	// BLOCK and is per SLOT now (OpSubCacheReset's P4 list), so one block can
	// carry both lifetimes. Its oracle evidence is
	// compat-harness/returning_mixed_lifetime_test.go, INSERT ... SELECT rows
	// included -- "INSERT INTO t SELECT x+30, y FROM s RETURNING a,
	// (SELECT y FROM s WHERE x=1), (SELECT count(*) FROM t WHERE a <= t.a)"
	// freezes the first subquery and re-runs the second, in one statement.
	//
	// The list is EMPTY, which is the point: cluster H4 leaves nothing unlowered
	// any more. The test below still runs, so a shape added back here keeps its
	// guard.
}

// TestInsertSelectSkipAddrIsTheFallThrough pins the claim that this promotion
// changes NOTHING for a non-RETURNING "INSERT ... SELECT".
//
// compileInsertSelectWrite sets plan.skipAddr unconditionally -- for every
// SELECT-sourced INSERT, RETURNING or not -- where before the change it was
// left 0 ("fall through"). That is only safe because the address it is set to
// is exactly the instruction the row would have fallen through TO: with no
// RETURNING emitted after OpInsert, the row's end label and OpInsert+1 are the
// same slot, so an OE_Ignore jump and a fall-through land identically.
//
// Asserting it beats reasoning about it: if a later change ever emits anything
// between OpInsert and the loop's OpNext without also thinking about the ignore
// path, this fails instead of silently making a conflict-IGNOREd row skip work
// it used to do.
func TestInsertSelectSkipAddrIsTheFallThrough(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a UNIQUE,b)`}, `INSERT OR IGNORE INTO d SELECT a,b FROM s`},
		{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a UNIQUE,b)`}, `INSERT OR REPLACE INTO d SELECT a,b FROM s`},
		{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a,b)`}, `INSERT INTO d SELECT a,b FROM s`},
		{[]string{`CREATE TABLE s(a,b)`, `CREATE TABLE d(a UNIQUE ON CONFLICT IGNORE,b)`}, `INSERT INTO d SELECT a,b FROM s`},
	} {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		insAddr, skip := -1, -1
		for i := range prog.Insns {
			if prog.Insns[i].Op == OpInsert {
				insAddr = i
				skip = prog.Insns[i].P4.(*insertPlan).skipAddr
			}
		}
		if insAddr < 0 {
			t.Fatalf("%q: no OpInsert emitted", tc.stmt)
		}
		if skip != insAddr+1 || prog.Insns[skip].Op != OpNext {
			t.Errorf("%q: skipAddr=%d (%v), want OpInsert+1=%d and OpNext -- the OE_Ignore jump "+
				"is no longer the plain fall-through, so this statement's behaviour CHANGED",
				tc.stmt, skip, prog.Insns[skip].Op, insAddr+1)
		}
	}
}

func TestInsertSelectReturningDeclinedShapesStayDeclined(t *testing.T) {
	for _, tc := range insertSelectReturningDeclined {
		if _, err := compileShape(t, tc.conflictShapeCase); err != nil {
			continue // the decline, which is the required outcome
		}
		t.Errorf("%q now COMPILES, but it is declined on purpose (%s).\n"+
			"Compiling it needs its own oracle evidence first -- see the note on this case.",
			tc.stmt, tc.why)
	}
}
