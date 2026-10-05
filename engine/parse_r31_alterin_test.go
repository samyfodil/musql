package engine

import "testing"

// TestParseR31TriggerEmptyInOperand verifies trigger stored SQL is correctly
// updated when renaming columns in empty IN() operands.
func TestParseR31TriggerEmptyInOperand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		alter string
		want  string
	}{
		{"CAST operand is elided", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE CAST(b AS INT) IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE CAST(b AS INT) IN (); END`},
		{"COLLATE blocks EP_HasFunc, so the operand is elided", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE abs(b) COLLATE nocase IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true WHERE abs(b) COLLATE nocase IN (); END`},
		// Surviving operands are rewritten only when in scope
		{"a bare call keeps the operand mapped, so it IS rewritten", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(b) IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(bbb) IN (); END`},
		{"an infix LIKE is a call too", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE (b LIKE 'x') IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE (bbb LIKE 'x') IN (); END`},
		{"COLLATE still elides with a FROM in scope", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(b) COLLATE nocase IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE abs(b) COLLATE nocase IN (); END`},
		{"CAST still elides with a FROM in scope", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE CAST(b AS INT) IN (); END`,
		}, `ALTER TABLE t1 RENAME b TO bbb`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN SELECT true FROM t1 WHERE CAST(b AS INT) IN (); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Exec(tc.alter); err != nil {
				t.Fatalf("%q: %v", tc.alter, err)
			}
			if len(db.triggers) != 1 {
				t.Fatalf("want 1 trigger, got %d", len(db.triggers))
			}
			if got := db.triggers[0].sql; got != tc.want {
				t.Errorf("after %q:\n got  %q\n want %q", tc.alter, got, tc.want)
			}
		})
	}
}
