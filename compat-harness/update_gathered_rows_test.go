package compat

import (
	"fmt"
	"testing"
)

// TestUpdateGatheredRowsMatchesC: an UPDATE's native code gathers each row's
// record and the engine stores a batch of them when native code hands control
// back (engine/vdbe_jit.go, gatherUpdate). Every case here runs over more
// rows than one batch holds, through a bound parameter (which native code
// reads from a register of its own), and is compared row for row, with
// changes(), against C SQLite: values that stop native code partway (a NULL
// or a REAL meeting the column's affinity, an integer overflow), a UNIQUE
// violation after many rows were gathered, which must undo them all, a text
// parameter stored into many rows, triggers that run between gathered rows
// and read what earlier rows stored, a foreign key, and a transaction with a
// savepoint rolled back.
func TestUpdateGatheredRowsMatchesC(t *testing.T) {
	const n = 1500
	cases := []struct {
		name string
		pre  []string
		upd  string
		args []any
		fail bool // both engines must reject it
	}{
		{"plain", nil, `UPDATE t SET v = v + 1 WHERE k = ?`, []any{3}, false},
		{"every row", nil, `UPDATE t SET v = v + ? `, []any{2}, false},
		{"text parameter", nil, `UPDATE t SET w = ? WHERE k <> ?`, []any{"param-text", 4}, false},
		{"nulls and reals", []string{`UPDATE t SET v = NULL WHERE id % 97 = 0`, `UPDATE t SET v = 2.5 WHERE id % 89 = 0`},
			`UPDATE t SET v = v + 1 WHERE k = ?`, []any{3}, false},
		{"overflow", []string{`UPDATE t SET v = 9223372036854775807 WHERE id % 50 = 0`}, `UPDATE t SET v = v + 1 WHERE k = ?`, []any{0}, false},
		{"unique violation late", []string{`CREATE UNIQUE INDEX tu ON t(u)`},
			`UPDATE t SET u = u + 1 WHERE k = ?`, []any{3}, true},
		{"after trigger reads earlier rows", []string{
			`CREATE TABLE log(id INTEGER, s INTEGER)`,
			`CREATE TRIGGER tr AFTER UPDATE ON t WHEN new.id % 100 = 3 BEGIN INSERT INTO log SELECT new.id, sum(v) FROM t; END`},
			`UPDATE t SET v = v + 1 WHERE k = ?`, []any{3}, false},
		{"before trigger rewrites a later row", []string{
			`CREATE TRIGGER tr BEFORE UPDATE ON t WHEN new.id % 200 = 13 BEGIN UPDATE t SET v = -1 WHERE id = new.id + 10; END`},
			`UPDATE t SET v = v + 1 WHERE k = ?`, []any{3}, false},
		{"foreign key", []string{`PRAGMA foreign_keys = ON`, `CREATE TABLE p(id INTEGER PRIMARY KEY)`, `INSERT INTO p SELECT id FROM t`,
			`CREATE TABLE c(id INTEGER PRIMARY KEY, pid REFERENCES p(id))`, `INSERT INTO c SELECT id, id FROM t WHERE id % 7 = 0`},
			`UPDATE c SET pid = pid + ? WHERE id > 0`, []any{0}, false},
	}
	for ci, tc := range cases {
		for _, inTx := range []bool{false, true} {
			name := fmt.Sprintf("%s/tx=%v", tc.name, inTx)
			pureDB, mattnDB, _, _ := openPair(t, fmt.Sprintf("gather%d_%v", ci, inTx))
			pureDB.SetMaxOpenConns(1)
			mattnDB.SetMaxOpenConns(1)
			execBoth(t, pureDB, mattnDB, name, `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w TEXT, u INTEGER)`)
			execBoth(t, pureDB, mattnDB, name, fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d) INSERT INTO t SELECT i, i %% 10, i, 'w' || i, i * 2 FROM c`, n))
			for _, s := range tc.pre {
				execBoth(t, pureDB, mattnDB, name, s)
			}
			if inTx {
				for _, s := range []string{`BEGIN`, `UPDATE t SET w = 'tx' WHERE id % 7 = 0`, `SAVEPOINT s`, `UPDATE t SET v = 0 WHERE id % 5 = 0`, `ROLLBACK TO s`, `RELEASE s`} {
					execBoth(t, pureDB, mattnDB, name, s)
				}
			}
			if tc.fail {
				_, perr := pureDB.Exec(tc.upd, tc.args...)
				_, merr := mattnDB.Exec(tc.upd, tc.args...)
				if (perr == nil) != (merr == nil) {
					t.Fatalf("%s: musql error %v, C error %v", name, perr, merr)
				}
			} else {
				execBoth(t, pureDB, mattnDB, name, tc.upd, tc.args...)
				queryBoth(t, pureDB, mattnDB, name, `SELECT changes()`)
			}
			if inTx {
				execBoth(t, pureDB, mattnDB, name, `COMMIT`)
			}
			queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM t ORDER BY id`)
			if tc.name == "after trigger reads earlier rows" {
				queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM log ORDER BY id`)
			}
			if tc.name == "foreign key" {
				queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM c ORDER BY id`)
			}
		}
	}
}
