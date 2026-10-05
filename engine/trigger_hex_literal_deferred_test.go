package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateTriggerAcceptsHexLiteralOverflow tests that CREATE TRIGGER accepts
// hex literals that exceed 64 bits, deferring magnitude checks to code generation time.
func TestCreateTriggerAcceptsHexLiteralOverflow(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if e := db.Exec(`CREATE TABLE t4(x)`); e != nil {
		t.Fatalf("CREATE TABLE: %v", e)
	}
	create := `CREATE TRIGGER tr4 AFTER INSERT ON t4 BEGIN SELECT 0x2147483648e0e0099 AS y WHERE y; END`
	if e := db.Exec(create); e != nil {
		t.Fatalf("CREATE TRIGGER: expected success (C SQLite accepts this -- the literal's magnitude is a codegen-time concern, not a parse-time one), got: %v", e)
	}

	// The firing INSERT must produce an error, not silently store anything or panic.
	ierr := db.Exec(`INSERT INTO t4 VALUES(1)`)
	if ierr == nil {
		t.Fatal("INSERT INTO t4 VALUES(1): expected an error compiling the trigger body, got none")
	}

	// A statement whose compilation itself failed must have stored nothing.
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, qerr := p.QueryArgs(`SELECT count(*) FROM t4`, nil)
	if qerr != nil {
		t.Fatalf("querying t4: %v", qerr)
	}
	if len(rows) != 1 || rows[0][0].I != 0 {
		t.Fatalf("t4 row count = %v, want 0 (the failed INSERT must have no effect)", rows)
	}
}

// TestCreateTriggerFiringHexLiteralOverflowRaises tests that firing a trigger
// with an overflowing hex literal raises the expected error message.
func TestCreateTriggerFiringHexLiteralOverflowRaises(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if e := db.Exec(`CREATE TABLE t4(x)`); e != nil {
		t.Fatalf("CREATE TABLE: %v", e)
	}
	create := `CREATE TRIGGER tr4 AFTER INSERT ON t4 BEGIN SELECT 0x2147483648e0e0099; END`
	if e := db.Exec(create); e != nil {
		t.Fatalf("CREATE TRIGGER: expected success, got: %v", e)
	}
	ierr := db.Exec(`INSERT INTO t4 VALUES(1)`)
	if ierr == nil {
		t.Fatal("INSERT INTO t4 VALUES(1): expected an error, got none")
	}
	const wantSubstr = "hex literal too big: 0x2147483648e0e0099"
	if !strings.Contains(ierr.Error(), wantSubstr) {
		t.Fatalf("INSERT error = %q, want it to contain %q", ierr.Error(), wantSubstr)
	}
}

// TestCreateTriggerHexLiteralOutsideTriggerBodyStillEager confirms that hex
// literal overflow still fails immediately in top-level statements.
func TestCreateTriggerHexLiteralOutsideTriggerBodyStillEager(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, _, qerr := p.QueryArgs(`SELECT 0x2147483648e0e0099`, nil)
	if qerr == nil {
		t.Fatal("top-level SELECT: expected an immediate error, got none")
	}
	const wantSubstr = "hex literal too big: 0x2147483648e0e0099"
	if !strings.Contains(qerr.Error(), wantSubstr) {
		t.Fatalf("top-level SELECT error = %q, want it to contain %q", qerr.Error(), wantSubstr)
	}
}

// TestCreateTriggerHexLiteralInWhenClauseDeferred is the WHEN-clause sibling
// of the bare-SELECT-body case above: p.inTriggerBody is set for a trigger's
// WHEN clause too (sql_parser.go's own doc comment on the field), and
// compileTriggerGuard (vdbe_trigger.go) calls compileExpr directly with no
// shortcut in between -- the same single choke point -- so a poisoned literal
// there must be accepted at CREATE TRIGGER time and raised only once the
// firing statement is compiled.
func TestCreateTriggerHexLiteralInWhenClauseDeferred(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if e := db.Exec(`CREATE TABLE t4(x)`); e != nil {
		t.Fatalf("CREATE TABLE: %v", e)
	}
	create := `CREATE TRIGGER tr4 AFTER INSERT ON t4 WHEN 0x2147483648e0e0099 BEGIN SELECT 1; END`
	if e := db.Exec(create); e != nil {
		t.Fatalf("CREATE TRIGGER with poisoned WHEN clause: expected success, got: %v", e)
	}
	ierr := db.Exec(`INSERT INTO t4 VALUES(1)`)
	if ierr == nil {
		t.Fatal("INSERT INTO t4 VALUES(1): expected an error from the WHEN clause's own hex literal, got none")
	}
	if !strings.Contains(ierr.Error(), "hex literal too big: 0x2147483648e0e0099") {
		t.Fatalf("INSERT error = %q, want it to contain the hex literal wording", ierr.Error())
	}
}

// TestCreateTriggerHexLiteralInInsertBodyDeclinesRatherThanRisksZero is the
// residual-risk regression: a poisoned literal sitting in a trigger's OWN
// INSERT/UPDATE/DELETE body statement routes through the full general write
// compiler (WHERE-plan index selection, conflict resolution, ...), which
// compileExpr's own deferredErr guard (vdbe_codegen.go) does not, by itself,
// prove safe against a value-dependent shortcut reading the literal's
// placeholder Val directly. triggerBodyStmtDeferredLiteralErr (trigger.go) is the
// cheap substitute for that audit: CREATE TRIGGER must still accept the
// construct (matching the oracle), and the firing statement must still
// raise the deferred error cleanly -- never panic, never silently store the
// literal's placeholder 0.
func TestCreateTriggerHexLiteralInInsertBodyDeclinesRatherThanRisksZero(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if e := db.Exec(`CREATE TABLE t4(x)`); e != nil {
		t.Fatalf("CREATE TABLE t4: %v", e)
	}
	if e := db.Exec(`CREATE TABLE t5(y)`); e != nil {
		t.Fatalf("CREATE TABLE t5: %v", e)
	}
	create := `CREATE TRIGGER tr5 AFTER INSERT ON t4 BEGIN INSERT INTO t5 VALUES(0x2147483648e0e0099); END`
	if e := db.Exec(create); e != nil {
		t.Fatalf("CREATE TRIGGER: expected success, got: %v", e)
	}
	ierr := db.Exec(`INSERT INTO t4 VALUES(1)`)
	if ierr == nil {
		t.Fatal("INSERT INTO t4 VALUES(1): expected an error from the trigger body's own poisoned INSERT, got none")
	}
	// TWO wordings are accepted here. The body's DML statement is declined at
	// prepare with "trigger body DML statement with a magnitude-deferred
	// literal" (vdbe_trigger.go), while "hex literal too big: <literal>" is
	// what compileExpr's own deferredErr guard (vdbe_codegen.go) raises if
	// the literal is reached as an expression first. Neither is what this case
	// is about -- what matters is that the statement FAILED and that the
	// poisoned literal never reached a row as 0, which the assertions below
	// check. Pinning the exact string would make this a test of which guard
	// fired first. (The WHEN-clause case above still pins the original wording:
	// its guard is evaluated on a path that produces exactly it.)
	if !strings.Contains(ierr.Error(), "hex literal too big: 0x2147483648e0e0099") &&
		!strings.Contains(ierr.Error(), "magnitude-deferred literal") {
		t.Fatalf("INSERT error = %q, want either the hex-literal wording or the "+
			"compiled route's magnitude-deferred decline", ierr.Error())
	}
	// Neither table may have been touched: t4's own INSERT never completed,
	// and t5 must never have received the literal's placeholder 0.
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"t4", "t5"} {
		_, rows, qerr := p.QueryArgs(`SELECT count(*) FROM `+tbl, nil)
		if qerr != nil {
			t.Fatalf("querying %s: %v", tbl, qerr)
		}
		if len(rows) != 1 || rows[0][0].I != 0 {
			t.Fatalf("%s row count = %v, want 0", tbl, rows)
		}
	}
}
