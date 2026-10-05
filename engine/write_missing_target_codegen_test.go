package engine

import (
	"errors"
	"testing"
)

// TestWriteMissingTargetIsAnError tests that writes to non-existent tables
// raise an error rather than declining.
func TestWriteMissingTargetIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, stmt string }{
		{"insert-default-values", "INSERT INTO nosuch DEFAULT VALUES"},
		{"insert-values", "INSERT INTO nosuch VALUES(1)"},
		{"insert-select", "INSERT INTO nosuch SELECT a FROM t"},
		{"insert-or-replace", "INSERT OR REPLACE INTO nosuch VALUES(1)"},
		{"delete", "DELETE FROM nosuch"},
		{"delete-where", "DELETE FROM nosuch WHERE a=1"},
		{"update", "UPDATE nosuch SET a=1"},
		{"update-where", "UPDATE nosuch SET a=1 WHERE a=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			if err := db.Exec("CREATE TABLE t(a)"); err != nil {
				t.Fatal(err)
			}
			_, cerr := db.compileWrite(tc.stmt)
			if cerr == nil {
				t.Fatalf("%q compiled to bytecode; it must not compile at all", tc.stmt)
			}
			if errors.Is(cerr, errVDBEUnsupported) {
				t.Errorf("%q declined as unsupported (%v); it must be errVDBESemantic so it PROPAGATES "+
					"rather than falling back to a driver that raises the identical error", tc.stmt, cerr)
			}
			// The statement must still FAIL, with the message it always had.
			if eerr := db.Exec(tc.stmt); eerr == nil {
				t.Errorf("%q succeeded", tc.stmt)
			}
		})
	}
}

// The same lookups must still FIND a target that exists, in either catalog --
// the failure mode a "make it an error" change invites is turning a resolvable
// name into a hard error.
func TestWriteExistingTargetStillCompiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
	}{
		{"main table", []string{"CREATE TABLE t(a)"}, "INSERT INTO t DEFAULT VALUES"},
		{"temp table", []string{"CREATE TEMP TABLE tt(a)"}, "INSERT INTO tt DEFAULT VALUES"},
		{"temp-qualified", []string{"CREATE TEMP TABLE tt(a)"}, "INSERT INTO temp.tt DEFAULT VALUES"},
		{"main-qualified", []string{"CREATE TABLE t(a)"}, "INSERT INTO main.t DEFAULT VALUES"},
		{"view target", []string{"CREATE TABLE t(a)", "CREATE VIEW v AS SELECT a FROM t",
			"CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO t VALUES(new.a); END"}, "INSERT INTO v VALUES(1)"},
		{"vtab target", []string{"CREATE VIRTUAL TABLE f USING fts4(x)"}, "INSERT INTO f VALUES('a')"},
		{"shadowed name resolves to temp", []string{"CREATE TABLE s(a)", "CREATE TEMP TABLE s(a)"}, "INSERT INTO s DEFAULT VALUES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Errorf("%q: %v -- the target exists and must resolve", tc.stmt, cerr)
			}
			if eerr := db.Exec(tc.stmt); eerr != nil {
				t.Errorf("exec %q: %v", tc.stmt, eerr)
			}
		})
	}
}

// A TRIGGER BODY keeps the decline, and this pins WHY rather than merely that.
// The "it would raise the identical error anyway" argument above is false for a
// body statement: the ATTACHed originating/mirrored trigger path runs it
// against a DIFFERENT session's catalog (attachedOriginatingFireInsert,
// attach_write.go), so a name absent from this compile's scope is still
// writable at fire time.
// Turning the body's miss into a hard error broke
// TestAttachedTriggerFiresAgainstOriginatingSession -- which is the discriminating
// evidence, and the reason this exception is not a hedge.
func TestTriggerBodyMissingTargetStillDeclines(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		"CREATE TABLE t(a)",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO nosuch VALUES(new.a); END",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	// The OUTER statement must still compile: its body's missing target is a
	// decline, so the body falls back rather than failing this compile.
	if _, cerr := db.compileWrite("INSERT INTO t VALUES(1)"); cerr != nil {
		if errors.Is(cerr, errVDBESemantic) {
			t.Fatalf("a trigger body's missing target became a SEMANTIC error and failed the "+
				"outer statement at compile time: %v -- a body target can be resolved at fire "+
				"time by the attached-originating path, which this compile cannot see", cerr)
		}
		t.Logf("outer declined (not semantic): %v", cerr)
	}
}
