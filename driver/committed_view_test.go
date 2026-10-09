package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestCommittedViewFollowsSmallCommits: a connection's committed read view
// is extended by each small commit's own batch (Session.extendCommittedColumns)
// instead of being rebuilt from the file and the whole delta. After every
// write, every read the writer makes must equal the same read on a separate
// connection that rebuilds its view from the file -- including the first
// write, which creates the delta.
func TestCommittedViewFollowsSmallCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.musq")
	open := func() *sql.DB {
		db, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	w := open()
	defer w.Close()
	ex := func(q string) {
		t.Helper()
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v INTEGER, s TEXT)`)
	ex(`CREATE TABLE u(id INTEGER PRIMARY KEY, x)`)
	ex(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 20000) INSERT INTO t SELECT i, i % 10, i * 3, 's' || i FROM c`)
	ex(`INSERT INTO u VALUES (1, 'a'), (2, 'b')`)
	ex(`VACUUM`)
	reads := []string{
		`SELECT count(*), sum(v), max(id), min(id) FROM t`,
		`SELECT count(*) FROM t WHERE v > 30000`,
		`SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k`,
		`SELECT max(id) FROM t`,
		`SELECT id, v, s FROM t WHERE id IN (1, 7, 19999, 20000, 20001, 20002)`,
		`SELECT count(*) FROM t WHERE s LIKE 's1999%'`,
		`SELECT group_concat(x) FROM u`,
		`SELECT id FROM t WHERE id = (SELECT max(id) FROM t)`,
	}
	dump := func(db *sql.DB) string {
		t.Helper()
		out := ""
		for _, q := range reads {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			cols, _ := rows.Columns()
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rows.Next() {
				rows.Scan(ptrs...)
				for _, v := range vals {
					if b, ok := v.([]byte); ok {
						v = string(b)
					}
					out += fmt.Sprint(v, " ")
				}
				out += "\n"
			}
			rows.Close()
			out += "--\n"
		}
		return out
	}
	writes := []string{
		`UPDATE t SET v = v + 1 WHERE id = 7`, // the first commit: creates the delta
		`INSERT INTO t VALUES (20001, 3, 99999, 's20001')`,
		`DELETE FROM t WHERE id = 1`,
		`DELETE FROM t WHERE id = (SELECT max(id) FROM t)`,
		`UPDATE t SET s = 's1999x' WHERE id = 19990`,
		`INSERT INTO u(x) VALUES ('c')`,
		`INSERT INTO t(k, v, s) VALUES (4, 1, 's-auto')`,
		`DELETE FROM t WHERE id = 20000`,
		`UPDATE t SET v = 30001 WHERE id = 2`,
		`DELETE FROM u WHERE id = 1`,
	}
	dump(w) // reads before the first write, so a cached read source exists while there is no delta yet
	for i, q := range writes {
		ex(q)
		got := dump(w)
		r := open()
		want := dump(r)
		r.Close()
		if got != want {
			t.Fatalf("after write %d (%s), the writer reads\n%s\na fresh connection reads\n%s", i, q, got, want)
		}
	}
}
