package compat

// This file gates TCL-substituted labels in do_execsql_test calls. Such labels
// must not be skipped when mining SQL statements.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinerDoExecsqlTestSubstitutedLabel(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		mined []string
	}{{
		// The exact mined statement this fix targets: a bare (unbraced) verb
		// as the SQL argument, with a "$"-substituted label ahead of it.
		name:  "dollar label, bare COMMIT",
		src:   "do_execsql_test $tn.1.1.2 COMMIT\n",
		mined: []string{"COMMIT"},
	}, {
		// The same file's setup statement: a dollar label with a BRACED,
		// multi-statement SQL body.
		name:  "dollar label, braced multi-statement SQL",
		src:   "do_execsql_test $tn.1.0 {\n  CREATE TABLE t1(a, b);\n  INSERT INTO t1 VALUES(1, 2);\n}\n",
		mined: []string{"CREATE TABLE t1(a, b)", "INSERT INTO t1 VALUES(1, 2)"},
	}, {
		// A literal (non-substituted) label must keep working exactly as
		// before -- this fix must not touch the common case.
		name:  "literal label is unaffected",
		src:   "do_execsql_test 1.2 { SELECT 1 }\n",
		mined: []string{"SELECT 1"},
	}, {
		// "-db dbname" is do_execsql_test's OWN different-connection form and
		// must stay unmined -- see findNextBraceGroup's doc comment. It does
		// not start with "$", so tclDoExecsqlTestSkipLabel must never fire
		// for it; confirm the retry doesn't accidentally revive it.
		name:  "-db different-connection form stays unmined",
		src:   "do_execsql_test -db db2 2.1 { SELECT b FROM t3 }\n",
		mined: nil,
	}, {
		// A label that is itself unresolved AND an SQL body that is ALSO
		// dynamic must still be dropped -- skipping the label must not widen
		// this into mining unknowable SQL.
		name:  "dynamic SQL body after a dollar label stays unmined",
		src:   "do_execsql_test $tn.1 $sql\n",
		mined: nil,
	}, {
		// A dollar-labeled call followed by an ordinary one: confirm the
		// retry path doesn't desynchronize later mining.
		name:  "a later literal-labeled call still mines normally",
		src:   "do_execsql_test $tn.1.1.2 COMMIT\ndo_execsql_test 1.3 { SELECT 2 }\n",
		mined: []string{"COMMIT", "SELECT 2"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTCLSQL(c.src)
			if len(got) != len(c.mined) {
				t.Fatalf("mined %d statements %q, want %d %q", len(got), got, len(c.mined), c.mined)
			}
			for i := range got {
				if strings.TrimSpace(got[i]) != c.mined[i] {
					t.Fatalf("statement %d = %q, want %q", i, got[i], c.mined[i])
				}
			}
		})
	}
}

// TestMinerDoExecsqlTestLabelReordersSnapshotSegment pins the end-to-end
// consequence against the real vendored file: before this fix, segment 0 of
// snapshot.test put "PRAGMA journal_mode = WAL" directly after an unclosed
// "BEGIN; SELECT * FROM t1" (the intervening "do_execsql_test $tn.1.1.2
// COMMIT" having gone missing), which is what made runTCLSegment score it as
// an unsupported gap: PRAGMA is excluded from the oracle-agreement probe
// (tclExecProbeSafe), so a real, correctly-declined "cannot change into wal
// mode from within a transaction" (matching vdbe.c:8095-8104's own text) was
// never actually compared against the oracle at all. With the label
// recognized, the segment's premise matches the real script -- journal_mode
// is reached in autocommit, exactly like C SQLite's own replay of this
// file -- and it is mined as an ordinary statement rather than skipped.
func TestMinerDoExecsqlTestLabelReordersSnapshotSegment(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(tclCorpusDir, "snapshot.test"))
	if err != nil {
		t.Skipf("no corpus: %v", err)
	}
	segs := tclSegments(string(b))
	if len(segs) == 0 {
		t.Fatal("expected at least one mined segment")
	}
	seg0 := segs[0]
	idx := -1
	for i, s := range seg0 {
		if s == "PRAGMA journal_mode = WAL" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("segment 0 never mines \"PRAGMA journal_mode = WAL\": %q", seg0)
	}
	// It must be reached in autocommit: walking backward from it, the
	// nearest transaction-control statement must be a COMMIT (or nothing at
	// all), never an unclosed BEGIN.
	for i := idx - 1; i >= 0; i-- {
		switch seg0[i] {
		case "COMMIT", "ROLLBACK", "END":
			return // autocommit -- the fix worked
		case "BEGIN":
			t.Fatalf("PRAGMA journal_mode = WAL (segment index %d) is still reached with an open BEGIN at index %d: %q", idx, i, seg0)
		}
	}
}
