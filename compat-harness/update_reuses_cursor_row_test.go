package compat

import (
	"fmt"
	"testing"
)

// TestUpdateReusesCursorRowMatchesC: an UPDATE writes each row from the copy
// its cursor already decoded only while the store provably still holds that
// row (rowStore.rowSeen). Each case changes rows between the cursor's read and
// their write -- a BEFORE trigger rewriting the row itself, an AFTER trigger
// rewriting, deleting or deleting and re-inserting a row the cursor reaches
// later, a REPLACE removing one, rows the transaction already wrote, a
// savepoint rolled back -- over committed rows read from segments, through
// both the equality seek (k = ?) and the full scan (v > ?).
//
// What a mutant shows: dropping every check in rowSeen diverges here 16 times,
// against one engine test and no corpus file. Only its overlay check is
// observable from SQL: a row deleted under the cursor is skipped by the
// loop's own re-seek before it is written, so the gone, base and swap checks
// are what keep rowSeen sound without leaning on that.
func TestUpdateReusesCursorRowMatchesC(t *testing.T) {
	cases := []struct {
		name string
		pre  []string
	}{
		{"plain", nil},
		{"before trigger rewrites the row", []string{
			`CREATE TRIGGER tr BEFORE UPDATE OF v ON t BEGIN UPDATE t SET w = w + 100 WHERE id = new.id; END`}},
		{"after trigger rewrites a later row", []string{
			`CREATE TRIGGER tr AFTER UPDATE OF v ON t BEGIN UPDATE t SET w = -1, v = v - 7 WHERE id = new.id + 10; END`}},
		{"after trigger deletes a later row", []string{
			`CREATE TRIGGER tr AFTER UPDATE OF v ON t BEGIN DELETE FROM t WHERE id = new.id + 10; END`}},
		{"after trigger deletes and re-inserts a later row", []string{
			`CREATE TRIGGER tr AFTER UPDATE OF v ON t BEGIN DELETE FROM t WHERE id = new.id + 10; INSERT INTO t VALUES(new.id + 10, 3, 5, 'again', new.id + 1000); END`}},
		{"after trigger clears the table", []string{
			`CREATE TRIGGER tr AFTER UPDATE OF v ON t WHEN new.id = 13 BEGIN DELETE FROM t; INSERT INTO t VALUES(23, 3, 1, 'back', 0); END`}},
	}
	updates := []string{
		`UPDATE t SET v = v + 1 WHERE k = 3`,
		`UPDATE t SET v = v + 1 WHERE v > 250`,
		`UPDATE OR REPLACE t SET u = u + 10 WHERE k = 3`,
	}
	for ci, tc := range cases {
		for ui, upd := range updates {
			for _, inTx := range []bool{false, true} {
				name := fmt.Sprintf("%s/%d/tx=%v", tc.name, ui, inTx)
				pureDB, mattnDB, _, _ := openPair(t, fmt.Sprintf("reuse%d_%d_%v", ci, ui, inTx))
				pureDB.SetMaxOpenConns(1)
				mattnDB.SetMaxOpenConns(1)
				execBoth(t, pureDB, mattnDB, name, `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w TEXT, u INTEGER UNIQUE)`)
				execBoth(t, pureDB, mattnDB, name, `WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 300) INSERT INTO t SELECT i, i % 10, i, 'w' || i, i FROM c`)
				for _, s := range tc.pre {
					execBoth(t, pureDB, mattnDB, name, s)
				}
				if inTx {
					for _, s := range []string{`BEGIN`, `UPDATE t SET w = 'tx' WHERE id % 7 = 0`, `DELETE FROM t WHERE id % 11 = 0`,
						`SAVEPOINT s`, `UPDATE t SET v = 0 WHERE id % 5 = 0`, `ROLLBACK TO s`, `RELEASE s`} {
						execBoth(t, pureDB, mattnDB, name, s)
					}
				}
				execBoth(t, pureDB, mattnDB, name, upd)
				if inTx {
					execBoth(t, pureDB, mattnDB, name, `COMMIT`)
				}
				queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM t ORDER BY id`)
				pureDB.Close()
				mattnDB.Close()
			}
		}
	}
}
