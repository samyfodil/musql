package engine

// Tests that REPLACE INTO finds its target the same way INSERT INTO does,
// so foreign key checking is applied correctly to REPLACE statements.

import (
	"strings"
	"testing"
)

func TestFkDMLTargetNameFindsAReplaceTarget(t *testing.T) {
	for _, tc := range []struct {
		sql          string
		wantSchema   string
		wantTable    string
		wantReachDel bool
	}{
		{`INSERT INTO ch VALUES(1)`, "", "ch", false},
		{`REPLACE INTO ch VALUES(1)`, "", "ch", true},
		{`REPLACE INTO main.ch VALUES(1)`, "main", "ch", true},
		{`replace into ch(a) values(1)`, "", "ch", true},
		{`WITH c AS (SELECT 1 AS a) REPLACE INTO ch SELECT a FROM c`, "", "ch", true},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			toks, err := lex(tc.sql)
			if err != nil {
				t.Fatalf("lex: %v", err)
			}
			kw, verbAt := writeDispatchKeywordAt(tc.sql, toks)
			schema, table := fkDMLTargetName(kw, toks, verbAt)
			if schema != tc.wantSchema || table != tc.wantTable {
				t.Errorf("fkDMLTargetName = (%q,%q), want (%q,%q)", schema, table, tc.wantSchema, tc.wantTable)
			}
			gotDel := fkStatementReach(kw, toks, verbAt)&fkReachDelete != 0
			if gotDel != tc.wantReachDel {
				t.Errorf("fkReachDelete = %v, want %v", gotDel, tc.wantReachDel)
			}
		})
	}
}

// TestReplaceStillRaisesAnUnresolvableForeignKey is the end-to-end half: the
// compiled REPLACE has to raise the SAME error the compiled INSERT does. An
// unresolvable foreign key of the TARGET is an error for any DML on it, rows or
// no rows (fkRequireResolvable's first loop, which runs whatever the reach is),
// because C SQLite raises it while GENERATING the code.
func TestReplaceStillRaisesAnUnresolvableForeignKey(t *testing.T) {
	newDB := func(t *testing.T) *Session {
		t.Helper()
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`PRAGMA foreign_keys=ON`,
			`CREATE TABLE ch(a REFERENCES nosuchparent(x))`,
		} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		return db
	}

	db := newDB(t)
	defer db.Discard()
	insErr := db.Exec(`INSERT INTO ch VALUES(1)`)
	if insErr == nil {
		t.Fatal("INSERT into a child whose parent does not exist should error " +
			"(this test's premise); nothing below means anything if it does not")
	}
	if !strings.Contains(insErr.Error(), "nosuchparent") {
		t.Fatalf("INSERT error does not name the missing parent: %v", insErr)
	}

	db2 := newDB(t)
	defer db2.Discard()
	repErr := db2.Exec(`REPLACE INTO ch VALUES(1)`)
	if repErr == nil {
		t.Fatal("REPLACE INTO a child whose parent does not exist SUCCEEDED -- " +
			"fkDMLTargetName lost the target, so Program.FKTargets is empty and " +
			"fkCheckTargets never ran")
	}
	if !strings.Contains(repErr.Error(), "nosuchparent") {
		t.Errorf("REPLACE error does not name the missing parent: %v", repErr)
	}
}
