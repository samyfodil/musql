package compat

import (
	"fmt"
	"testing"
)

// sweepAll runs every statement list through differ and stops reporting after
// a handful of divergences, so one wrong rule does not bury the log. The
// sweeps in this file are cross products over a small alphabet, run against C
// SQLite for the families of wrong answers the Turso corpus turned up.
func sweepAll(t *testing.T, cases [][]string) {
	t.Helper()
	// -short keeps a spread of the larger sweeps (every k-th case, about 400 of
	// them); the full cross products run without it.
	if testing.Short() && len(cases) > 400 {
		k := len(cases) / 400
		thin := cases[:0:0]
		for i := 0; i < len(cases); i += k {
			thin = append(thin, cases[i])
		}
		cases = thin
	}
	bad := 0
	for i, stmts := range cases {
		if !differ(t, fmt.Sprintf("sweep-%d", i), stmts) {
			if bad++; bad >= 12 {
				t.Fatalf("stopping after %d divergences of %d cases run", bad, i+1)
			}
		}
	}
}

// TestFKSelfReferenceSweep covers fkLookupParent's "the row matches itself"
// test (raw values, no affinity, no collation) and the lookup that excludes
// the row being inserted, across key types, collations and deferral.
func TestFKSelfReferenceSweep(t *testing.T) {
	types := []string{"INTEGER", "TEXT", "TEXT COLLATE NOCASE", "NUMERIC", "", "REAL"}
	defer1 := []string{"", " DEFERRABLE INITIALLY DEFERRED"}
	kvals := []string{"1", "'1'", "'A'", "NULL", "1.0"}
	pvals := []string{"1", "'1'", "'a'", "'A'", "NULL", "1.0"}
	var cases [][]string
	for _, kt := range types {
		for _, pt := range types {
			for _, df := range defer1 {
				ddl := fmt.Sprintf("CREATE TABLE t(id INTEGER PRIMARY KEY, k %s UNIQUE, pk %s REFERENCES t(k)%s)", kt, pt, df)
				pre := []string{"PRAGMA foreign_keys=ON", ddl}
				for _, k := range kvals {
					for _, p := range pvals {
						ins := fmt.Sprintf("INSERT INTO t(id,k,pk) VALUES(1,%s,%s)", k, p)
						cases = append(cases,
							append(append([]string(nil), pre...), ins, "SELECT quote(k),quote(pk) FROM t"),
							append(append([]string(nil), pre...), "BEGIN", ins, "COMMIT", "SELECT quote(k),quote(pk) FROM t"),
							append(append([]string(nil), pre...), "INSERT INTO t(id,k,pk) VALUES(9,1,NULL),(8,'A',NULL)", ins, "SELECT id,quote(k),quote(pk) FROM t ORDER BY id"),
						)
					}
				}
			}
		}
	}
	sweepAll(t, cases)
}

// TestFKParentSideSweep crosses parent-key mutations that leave the key as it
// was (C's +1/-1 scans cancel) with every action, a pending deferred
// violation, and OR REPLACE victims that have children.
func TestFKParentSideSweep(t *testing.T) {
	acts := []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}
	defs := []string{"", " DEFERRABLE INITIALLY DEFERRED"}
	targets := []string{"id", "u"}
	ops := []string{
		"UPDATE p SET id=id",
		"UPDATE p SET id=10 WHERE id=10",
		"UPDATE OR REPLACE p SET id=10 WHERE id=10",
		"UPDATE OR REPLACE p SET u='a' WHERE id=10",
		"UPDATE OR REPLACE p SET u='b' WHERE id=10",
		"UPDATE p SET u='z' WHERE id=10",
		"UPDATE p SET u='A' WHERE id=10",
		"UPDATE OR IGNORE p SET u='b' WHERE id=10",
		"INSERT OR REPLACE INTO p VALUES(10,'a')",
		"INSERT OR REPLACE INTO p VALUES(10,'q')",
		"INSERT OR REPLACE INTO p VALUES(30,'b')",
		"REPLACE INTO p VALUES(30,'a')",
		"DELETE FROM p WHERE id=10",
		"INSERT INTO p VALUES(20,'zz')",
		"INSERT INTO p VALUES(50,'new')",
	}
	var cases [][]string
	for _, act := range acts {
		for _, df := range defs {
			for _, tg := range targets {
				for _, ev := range []string{"ON UPDATE", "ON DELETE"} {
					ddl := []string{"PRAGMA foreign_keys=ON",
						"CREATE TABLE p(id INTEGER PRIMARY KEY, u TEXT UNIQUE)",
						fmt.Sprintf("CREATE TABLE c(id INTEGER PRIMARY KEY, pid DEFAULT 20 REFERENCES p(%s) %s %s%s)", tg, ev, act, df),
						"INSERT INTO p VALUES(10,'a'),(20,'b')",
					}
					kid10, kid20, bad := "10", "20", "99"
					if tg == "u" {
						kid10, kid20, bad = "'a'", "'b'", "'missing'"
					}
					seed := fmt.Sprintf("INSERT INTO c VALUES(1,%s),(2,%s)", kid10, kid20)
					for _, op := range ops {
						base := append(append([]string(nil), ddl...), seed)
						cases = append(cases,
							append(append([]string(nil), base...), op, "SELECT group_concat(id||':'||quote(pid)) FROM c", "SELECT group_concat(id||':'||quote(u)) FROM p"),
							append(append([]string(nil), base...), "BEGIN", fmt.Sprintf("INSERT INTO c VALUES(3,%s)", bad), op, "COMMIT", "SELECT group_concat(id||':'||quote(pid)) FROM c"),
							append(append([]string(nil), base...), "BEGIN", fmt.Sprintf("INSERT INTO c VALUES(3,%s)", bad), op, "ROLLBACK", "SELECT group_concat(id||':'||quote(pid)) FROM c"),
						)
					}
				}
			}
		}
	}
	sweepAll(t, cases)
}

// TestFKDropParentSweep crosses DROP TABLE of a parent with every action, the
// default a SET DEFAULT child falls back to, and deferral.
func TestFKDropParentSweep(t *testing.T) {
	acts := []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}
	defs := []string{"", " DEFERRABLE INITIALLY DEFERRED"}
	dflts := []string{"", " DEFAULT NULL", " DEFAULT 999", " DEFAULT 1", " DEFAULT 2"}
	pre := []string{"", "BEGIN"}
	rows := []string{"", "INSERT INTO p VALUES(1)", "INSERT INTO p VALUES(1),(2)"}
	kids := []string{"", "INSERT INTO c VALUES(10,1)", "INSERT INTO c VALUES(10,1),(11,2),(12,NULL)"}
	var cases [][]string
	for _, act := range acts {
		for _, df := range defs {
			for _, d := range dflts {
				for _, b := range pre {
					for _, pr := range rows {
						for _, kd := range kids {
							s := []string{"PRAGMA foreign_keys=ON",
								"CREATE TABLE p(id INTEGER PRIMARY KEY)",
								fmt.Sprintf("CREATE TABLE c(id INTEGER PRIMARY KEY, pid INTEGER%s REFERENCES p(id) ON DELETE %s%s)", d, act, df),
							}
							if b != "" {
								s = append(s, b)
							}
							for _, x := range []string{pr, kd, "DROP TABLE p"} {
								if x != "" {
									s = append(s, x)
								}
							}
							if b != "" {
								s = append(s, "COMMIT")
							}
							s = append(s, "SELECT group_concat(id||':'||quote(pid)) FROM c", "SELECT name FROM sqlite_master ORDER BY name")
							cases = append(cases, s)
						}
					}
				}
			}
		}
	}
	sweepAll(t, cases)
}

// TestFKParentMismatchInsertSweep crosses an INSERT into a parent whose key
// cannot be located (no PRIMARY KEY, no unique index) with the shapes that
// make sqlite3FkCheck look: a multi-row VALUES, INSERT ... SELECT, a trigger,
// a deferred key, OR REPLACE, and the plain single row that C skips.
func TestFKParentMismatchInsertSweep(t *testing.T) {
	ins := []string{
		"INSERT INTO p(v) VALUES('a')",
		"INSERT INTO p(v) VALUES('a'),('b')",
		"INSERT INTO p(v) SELECT 'a'",
		"INSERT INTO p(v) SELECT 'a' UNION ALL SELECT 'b'",
		"INSERT INTO p DEFAULT VALUES",
		"INSERT OR REPLACE INTO p(v) VALUES('a')",
		"REPLACE INTO p(v) VALUES('a')",
		"INSERT INTO p(v) VALUES((SELECT 1))",
		"INSERT INTO p(v) VALUES('a') ON CONFLICT DO NOTHING",
		"WITH x(a) AS (SELECT 1) INSERT INTO p(v) SELECT a FROM x",
		"UPDATE p SET v='z'",
		"DELETE FROM p",
	}
	var cases [][]string
	for _, df := range []string{"", " DEFERRABLE INITIALLY DEFERRED"} {
		for _, ref := range []string{"p", "p(v)"} {
			for _, trig := range []string{"", "CREATE TRIGGER tg AFTER INSERT ON p BEGIN SELECT 1; END", "CREATE TRIGGER tg BEFORE DELETE ON p BEGIN SELECT 1; END"} {
				for _, idx := range []string{"", "CREATE INDEX pi ON p(v)"} {
					for _, op := range ins {
						s := []string{"PRAGMA foreign_keys=ON", "CREATE TABLE p(v TEXT)",
							fmt.Sprintf("CREATE TABLE c(id INTEGER PRIMARY KEY, r REFERENCES %s%s)", ref, df)}
						for _, x := range []string{trig, idx} {
							if x != "" {
								s = append(s, x)
							}
						}
						s = append(s, op, "SELECT count(*) FROM p")
						cases = append(cases, s)
					}
				}
			}
		}
	}
	sweepAll(t, cases)
}
