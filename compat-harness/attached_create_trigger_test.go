package compat

import "testing"

// TestAttachedCreateTrigger tests CREATE TRIGGER with ATTACHed-database qualifiers.
// A non-TEMP trigger cannot reference another database.

func TestAttachedCreateTrigger(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE log(m)`,
		`ATTACH ':memory:' AS aux`,
		`CREATE TABLE aux.t(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE aux.log(m)`,
	}
	cases := [][]string{
		{`CREATE TRIGGER aux.tg AFTER INSERT ON aux.t BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO aux.t VALUES(1,'x')`, `SELECT count(*) FROM aux.log`, `SELECT count(*) FROM main.log`},
		{`CREATE TRIGGER aux.tg AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO main.t VALUES(1,'x')`, `SELECT count(*) FROM main.log`, `SELECT count(*) FROM aux.log`},
		{`CREATE TRIGGER aux.tg2 AFTER INSERT ON aux.t BEGIN SELECT 1; END`,
			`INSERT INTO aux.t VALUES(2,'y')`, `SELECT count(*) FROM aux.t`},
		{`CREATE TRIGGER aux.tg3 BEFORE DELETE ON aux.t BEGIN INSERT INTO log VALUES(old.a); END`,
			`INSERT INTO aux.t VALUES(3,'z')`, `DELETE FROM aux.t`, `SELECT count(*) FROM aux.log`},
		{`CREATE TRIGGER main.tg AFTER INSERT ON main.t BEGIN INSERT INTO log VALUES(new.a); END`,
			`INSERT INTO t VALUES(4,'w')`, `SELECT count(*) FROM main.log`},
		// A non-TEMP trigger may not reference another database.
		{`CREATE TRIGGER aux.tg4 AFTER INSERT ON main.t BEGIN SELECT 1; END`},
		{`CREATE TRIGGER main.tg6 AFTER INSERT ON aux.t BEGIN SELECT 1; END`},
		{`CREATE TRIGGER tg7 AFTER INSERT ON aux.t BEGIN SELECT 1; END`},
		{`CREATE TRIGGER aux.tg5 AFTER INSERT ON aux.t BEGIN INSERT INTO aux.log VALUES(new.a); END`},
		{`CREATE TRIGGER aux.tg AFTER INSERT ON aux.t BEGIN INSERT INTO log VALUES(new.a); END`,
			`DROP TRIGGER aux.tg`, `INSERT INTO aux.t VALUES(6,'v')`, `SELECT count(*) FROM aux.log`},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			differ(t, "auxtrig/"+tc[0], append(append([]string{}, base...), tc...))
		})
	}
}
