package compat

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// TestAppendLoadSegmentsMatchC: a transaction's prepared INSERTs past every
// rowid a table has held go into a segment the commit writes as one delta
// record and publishes to the connection's read views
// (engine/insert_append_delta.go). Every step below -- plain appends,
// appends then a write to them in the same transaction, a rolled-back load,
// savepoints, deletes and updates of appended rows, out-of-order ids, a read
// in the middle of a load, a rowid below a deleted one, and a reopen -- must
// leave the SAME connection answering every query as C SQLite does.
func TestAppendLoadSegmentsMatchC(t *testing.T) {
	// Armed from the first qualifying statement, so every step below takes
	// the path; the last load before the reopen uses the real threshold.
	defer engine.SetAppendArmAfterForTest(engine.SetAppendArmAfterForTest(1))
	pureDB, mattnDB, purePath, mattnPath := openPair(t, "appendload")
	pureDB.SetMaxOpenConns(1)
	mattnDB.SetMaxOpenConns(1)

	execBoth(t, pureDB, mattnDB, "create",
		`CREATE TABLE t(id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, f REAL, payload TEXT, b BLOB)`)
	execBoth(t, pureDB, mattnDB, "index", `CREATE INDEX t_sec ON t(sec)`)

	row := func(id int) []any {
		var f any = float64(id) / 4
		if id%17 == 0 {
			f = nil
		}
		var p any = fmt.Sprintf("p%d", id%100)
		if id%23 == 0 {
			p = id // an INTEGER in the TEXT column: an exception in its segment
		}
		var b any = []byte{byte(id), byte(id >> 8)}
		if id%5 == 0 {
			b = nil
		}
		return []any{id, id % 997, id % 10, id * 3, f, p, b}
	}
	load := func(label string, ids []int, mid func(tx *sql.Tx), end string) {
		t.Helper()
		for _, db := range []*sql.DB{pureDB, mattnDB} {
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			st, err := tx.Prepare(`INSERT INTO t VALUES(?,?,?,?,?,?,?)`)
			if err != nil {
				t.Fatal(err)
			}
			for i, id := range ids {
				if _, err := st.Exec(row(id)...); err != nil {
					t.Fatalf("%s: insert %d: %v", label, id, err)
				}
				if mid != nil && i == len(ids)/2 {
					mid(tx)
				}
			}
			st.Close()
			switch end {
			case "commit":
				err = tx.Commit()
			default:
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatalf("%s: %s: %v", label, end, err)
			}
		}
		battery(t, pureDB, mattnDB, label)
	}
	span := func(lo, hi int) []int {
		var out []int
		for i := lo; i <= hi; i++ {
			out = append(out, i)
		}
		return out
	}

	load("seed", span(1, 2000), nil, "commit")
	execBoth(t, pureDB, mattnDB, "delete tail", `DELETE FROM t WHERE id > 1990`)
	battery(t, pureDB, mattnDB, "after deleting the tail")

	engine.TxBulkRowsForTest()
	// 1991.. is below the deleted 2000: the ordinary path; then past it.
	load("below a deleted rowid", span(1991, 1995), nil, "commit")
	load("append", span(2001, 3000), nil, "commit")
	if n := engine.TxBulkRowsForTest(); n == 0 {
		t.Fatal("no append load armed: the test is not exercising the path")
	}
	load("append then update in the transaction", span(3001, 3500), func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE t SET v = -v WHERE id BETWEEN 3100 AND 3110`); err != nil {
			t.Fatal(err)
		}
	}, "commit")
	load("rolled back", span(3501, 4000), nil, "rollback")
	load("read in the middle", span(4001, 4400), func(tx *sql.Tx) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM t WHERE k = 3`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}, "commit")
	load("savepoints", span(4401, 4600), func(tx *sql.Tx) {
		for _, s := range []string{`SAVEPOINT s`, `INSERT INTO t VALUES(4999,1,1,1,1,'x',NULL)`, `ROLLBACK TO s`, `RELEASE s`} {
			if _, err := tx.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
	}, "commit")
	execBoth(t, pureDB, mattnDB, "update appended", `UPDATE t SET payload = 'u' WHERE id % 50 = 0 AND id > 2000`)
	execBoth(t, pureDB, mattnDB, "delete appended", `DELETE FROM t WHERE id BETWEEN 2500 AND 2600`)
	battery(t, pureDB, mattnDB, "after updating and deleting appended rows")
	load("out of order", []int{5002, 5001, 5003}, nil, "commit")
	load("append after all that", span(6001, 7000), nil, "commit")
	old := engine.SetAppendArmAfterForTest(256)
	load("crossing the arming threshold", span(8001, 8600), nil, "commit")
	engine.SetAppendArmAfterForTest(old)

	// A fresh connection and a reopen read the segment records from the file.
	pureDB.Close()
	mattnDB.Close()
	var err error
	if pureDB, err = sql.Open(driver.DriverName, purePath); err != nil {
		t.Fatal(err)
	}
	if mattnDB, err = sql.Open("sqlite3", mattnPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pureDB.Close(); mattnDB.Close() })
	battery(t, pureDB, mattnDB, "reopened")
	load("append after reopen", span(7001, 7500), nil, "commit")
	execBoth(t, pureDB, mattnDB, "vacuum", `VACUUM`)
	battery(t, pureDB, mattnDB, "vacuumed")
}

func battery(t *testing.T, pureDB, mattnDB *sql.DB, label string) {
	t.Helper()
	for _, q := range []string{
		`SELECT count(*), sum(v), total(f), max(id), min(id) FROM t`,
		`SELECT k, count(*), sum(v), max(f) FROM t GROUP BY k ORDER BY k`,
		`SELECT id, v, payload FROM t WHERE sec = 7 ORDER BY id`,
		`SELECT count(*) FROM t WHERE sec BETWEEN 100 AND 300 AND k = 3`,
		`SELECT * FROM t ORDER BY id DESC LIMIT 7`,
		`SELECT * FROM t WHERE id IN (1990, 1993, 2001, 2500, 3105, 4400, 4500, 5001, 6500, 7499, 8001, 8256, 8257, 8600)`,
		`SELECT count(*) FROM t WHERE payload LIKE 'p1%'`,
		`SELECT typeof(payload), count(*) FROM t GROUP BY 1 ORDER BY 1`,
		`SELECT count(*), sum(length(b)) FROM t WHERE b IS NOT NULL`,
		`SELECT id FROM t WHERE v < 0 ORDER BY id`,
		`SELECT count(*) FROM t WHERE f IS NULL`,
		`PRAGMA integrity_check`,
	} {
		queryBoth(t, pureDB, mattnDB, label, q)
	}
}
