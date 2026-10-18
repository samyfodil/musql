package compat

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/samyfodil/musql/driver"
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
// savepoint rolled back, and one rolled back whole. Outside a transaction, and with no index or key that
// reads the old row, the new values are written over the cursor's copy of the
// old row (applyGathered's in place); a CHECK that fails after many such rows
// must put every one of them back. Such rows are committed as patches of
// their committed row, so the table is compared again after a fresh open.
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
			`UPDATE t SET u = CASE WHEN id = 1403 THEN 2 ELSE u + 1 END WHERE k = ?`, []any{3}, true}, // id 1's u
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
		{"check fails late", nil, `UPDATE t SET v = v + ? WHERE k = ?`, []any{15, 3}, true}, // only from id 1493 on
		{"not null fails late", nil, `UPDATE t SET v = CASE WHEN id = 1403 THEN NULL ELSE v + ? END WHERE k = ?`, []any{1, 3}, true},
	}
	// The table, where a case needs its own.
	ddls := map[string]string{
		"check fails late":    `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER CHECK (v < 1505), w TEXT, u INTEGER)`,
		"not null fails late": `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER NOT NULL, w TEXT, u INTEGER)`,
	}
	for ci, tc := range cases {
		// tx: 0 autocommit, 1 a transaction committed, 2 one rolled back.
		for tx := range 3 {
			inTx := tx > 0
			name := fmt.Sprintf("%s/tx=%d", tc.name, tx)
			pureDB, mattnDB, purePath, _ := openPair(t, fmt.Sprintf("gather%d_%d", ci, tx))
			pureDB.SetMaxOpenConns(1)
			mattnDB.SetMaxOpenConns(1)
			ddl := ddls[tc.name]
			if ddl == "" {
				ddl = `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w TEXT, u INTEGER)`
			}
			execBoth(t, pureDB, mattnDB, name, ddl)
			execBoth(t, pureDB, mattnDB, name, fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d) INSERT INTO t SELECT i, i %% 10, i, 'w' || i, i * 2 FROM c`, n))
			// Every row into the file's segments: only a committed row the
			// cursor decodes from them is written in place.
			execBoth(t, pureDB, mattnDB, name, `VACUUM`)
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
				if perr == nil || merr == nil {
					t.Fatalf("%s: musql error %v, C error %v; both must fail", name, perr, merr)
				}
			} else {
				execBoth(t, pureDB, mattnDB, name, tc.upd, tc.args...)
				queryBoth(t, pureDB, mattnDB, name, `SELECT changes()`)
			}
			switch tx {
			case 1:
				execBoth(t, pureDB, mattnDB, name, `COMMIT`)
			case 2:
				execBoth(t, pureDB, mattnDB, name, `ROLLBACK`)
			}
			queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM t ORDER BY id`)
			if tc.name == "after trigger reads earlier rows" {
				queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM log ORDER BY id`)
			}
			if tc.name == "foreign key" {
				queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM c ORDER BY id`)
			}
			// What a fresh open reads back: rows updated in place are written
			// as patches of their committed row, which the open replays.
			pureDB.Close()
			reopened, err := sql.Open(driver.DriverName, purePath)
			if err != nil {
				t.Fatal(err)
			}
			queryBoth(t, reopened, mattnDB, name+"/reopened", `SELECT * FROM t ORDER BY id`)
			reopened.Close()
		}
	}
}

// TestUpdateLargeSeekMatchesC: an UPDATE whose equality seek finds thousands
// of rows reads their committed copies on several cores (engine's
// eachOfParallel), mixed with rows the overlay holds and rows already
// deleted, and must still visit, store and commit every one as C does.
func TestUpdateLargeSeekMatchesC(t *testing.T) {
	for _, inTx := range []bool{false, true} {
		name := fmt.Sprintf("large/tx=%v", inTx)
		pureDB, mattnDB, purePath, _ := openPair(t, fmt.Sprintf("large_%v", inTx))
		pureDB.SetMaxOpenConns(1)
		mattnDB.SetMaxOpenConns(1)
		execBoth(t, pureDB, mattnDB, name, `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, w TEXT)`)
		execBoth(t, pureDB, mattnDB, name, `WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 60000) INSERT INTO t SELECT i, i % 10, i, 'w' || i FROM c`)
		execBoth(t, pureDB, mattnDB, name, `VACUUM`)
		if inTx {
			execBoth(t, pureDB, mattnDB, name, `BEGIN`)
		}
		execBoth(t, pureDB, mattnDB, name, `DELETE FROM t WHERE id % 13 = 0`)
		execBoth(t, pureDB, mattnDB, name, `UPDATE t SET w = 'o' || id WHERE id % 11 = 0`)
		execBoth(t, pureDB, mattnDB, name, `UPDATE t SET v = v + ? WHERE k = ?`, 5, 3)
		queryBoth(t, pureDB, mattnDB, name, `SELECT changes()`)
		execBoth(t, pureDB, mattnDB, name, `UPDATE t SET v = v * 2, w = w || 'x' WHERE k = ?`, 3)
		if inTx {
			execBoth(t, pureDB, mattnDB, name, `COMMIT`)
		}
		queryBoth(t, pureDB, mattnDB, name, `SELECT * FROM t ORDER BY id`)
		pureDB.Close()
		reopened, err := sql.Open(driver.DriverName, purePath)
		if err != nil {
			t.Fatal(err)
		}
		queryBoth(t, reopened, mattnDB, name+"/reopened", `SELECT * FROM t ORDER BY id`)
		reopened.Close()
	}
}
