package engine

import "testing"

// TestAlterR30TriggerStoredSQL asserts trigger SQL after ALTER TABLE RENAME.
func TestAlterR30TriggerStoredSQL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		alter string
		want  string
	}{
		{"ON clause spelled as a string", []string{
			`CREATE TABLE t8(a, b, c)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER trig3 AFTER INSERT ON 't8' BEGIN INSERT INTO log VALUES(new.a); END`,
		}, `ALTER TABLE t8 RENAME TO t9`,
			`CREATE TRIGGER trig3 AFTER INSERT ON "t9" BEGIN INSERT INTO log VALUES(new.a); END`},
		{"schema-qualified string ON clause", []string{
			`CREATE TABLE t8(a, b, c)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER trig3 AFTER INSERT ON main.'t8' BEGIN INSERT INTO log VALUES(new.a); END`,
		}, `ALTER TABLE t8 RENAME TO t9`,
			`CREATE TRIGGER trig3 AFTER INSERT ON main."t9" BEGIN INSERT INTO log VALUES(new.a); END`},
		{"a string in a VALUE position is left alone", []string{
			`CREATE TABLE t8(a)`,
			`CREATE TABLE log(x)`,
			`CREATE TRIGGER trig5 AFTER INSERT ON t8 BEGIN INSERT INTO log VALUES('t8'); END`,
		}, `ALTER TABLE t8 RENAME TO t9`,
			`CREATE TRIGGER trig5 AFTER INSERT ON "t9" BEGIN INSERT INTO log VALUES('t8'); END`},
		{"empty IN() operand in the body", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 WHEN new.a NOT NULL BEGIN SELECT true WHERE (SELECT a, b FROM (t1)) IN (); END`,
		}, `ALTER TABLE t1 RENAME TO t1x`,
			`CREATE TRIGGER tr1 AFTER INSERT ON "t1x" WHEN new.a NOT NULL BEGIN SELECT true WHERE (SELECT a, b FROM (t1)) IN (); END`},
		{"UPDATE OF list takes the quoted new column name", []string{
			`CREATE TABLE x5(one, two)`,
			`CREATE TRIGGER x5t AFTER UPDATE OF two ON x5 BEGIN SELECT new.two; END`,
		}, `ALTER TABLE x5 RENAME two TO 'four'`,
			`CREATE TRIGGER x5t AFTER UPDATE OF "four" ON x5 BEGIN SELECT new."four"; END`},
		// Function call with same name as renamed column.
		{"a function spelled like the renamed column is left alone", []string{
			`CREATE TABLE t1(a, b, c)`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 WHEN new.a NOT NULL BEGIN SELECT a () FILTER (WHERE a>0) FROM t1; END`,
		}, `ALTER TABLE t1 RENAME a TO aaa`,
			`CREATE TRIGGER tr1 AFTER INSERT ON t1 WHEN new.aaa NOT NULL BEGIN SELECT a () FILTER (WHERE aaa>0) FROM t1; END`},
		// ... and the same for a table rename, EXCEPT where the "(" belongs to
		// a body step's column list: "INSERT INTO t1(a,b)" is a step target
		// renameTableFunc rewrites by bare name (alter.c:1867).
		{"a body step's column list still follows a table rename", []string{
			`CREATE TABLE t1(a, b)`,
			`CREATE TABLE drv(x)`,
			`CREATE TRIGGER tr2 AFTER INSERT ON drv BEGIN INSERT INTO t1(a,b) VALUES(1,2); END`,
		}, `ALTER TABLE t1 RENAME TO t9`,
			`CREATE TRIGGER tr2 AFTER INSERT ON drv BEGIN INSERT INTO "t9"(a,b) VALUES(1,2); END`},
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
