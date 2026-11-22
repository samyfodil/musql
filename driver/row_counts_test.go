package driver

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// rowCounter runs statements on one connection and reports each one's rows
// read and written (Conn.RowCounts).
type rowCounter struct {
	t    *testing.T
	conn *sql.Conn
}

func newRowCounter(t *testing.T, name string) *rowCounter {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &rowCounter{t: t, conn: conn}
}

func (rc *rowCounter) counts() (read, written int64) {
	rc.conn.Raw(func(dc any) error {
		read, written = dc.(*Conn).RowCounts()
		return nil
	})
	return
}

// run executes q, reading every row it returns, and reports its counts.
func (rc *rowCounter) run(q string) (read, written int64) {
	rc.t.Helper()
	r0, w0 := rc.counts()
	rows, err := rc.conn.QueryContext(context.Background(), q)
	if err != nil {
		rc.t.Fatalf("%s: %v", q, err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rc.t.Fatalf("%s: %v", q, err)
	}
	rows.Close()
	r1, w1 := rc.counts()
	return r1 - r0, w1 - w0
}

// withJIT runs f once with the JIT and once without it.
func withJIT(t *testing.T, f func(t *testing.T, mode string)) {
	for _, mode := range []string{"jit", "nojit"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "nojit" {
				engine.Configure(engine.WithoutJIT())
				defer engine.Configure()
			}
			f(t, mode)
		})
	}
}

// TestRowCountsTursoReference reproduces the counts Turso reports for the same
// statements over a 10,000-row table (musql.dev docs/turso-observed.md).
// Where musql counts less than libSQL the comment says why; it never counts
// more.
func TestRowCountsTursoReference(t *testing.T) {
	withJIT(t, func(t *testing.T, mode string) {
		rc := newRowCounter(t, "ref-"+mode+".musq")
		type step struct {
			q             string
			read, written int64
		}
		for _, s := range []step{
			// libSQL: 1 and 2, the schema row inserted then updated. musql
			// writes its catalog entry once and reads none.
			{"CREATE TABLE big(id INTEGER PRIMARY KEY, k INTEGER, v TEXT)", 0, 1},
			{"INSERT INTO big SELECT value, value % 100, 'v' || value FROM generate_series(1, 10000)", 10000, 10000},
			{"SELECT * FROM big WHERE id = 5000", 1, 0},
			{"SELECT count(*) FROM big WHERE k = 7", 10000, 0},
		} {
			r, w := rc.run(s.q)
			if r != s.read || w != s.written {
				t.Errorf("%s: read %d written %d, want %d %d", s.q, r, w, s.read, s.written)
			}
		}
		r, _ := rc.run("SELECT count(*) FROM big")
		fmt.Println(mode, "count(*)", r)
		r, w := rc.run("CREATE INDEX big_k ON big(k)")
		fmt.Println(mode, "create index", r, w)
		for _, s := range []step{
			// libSQL 101: its index walk also steps onto the first entry past
			// the range, which a musql index never holds.
			{"SELECT count(*) FROM big WHERE k = 7", 100, 0},
			{"SELECT k, count(*) FROM big GROUP BY k LIMIT 3", 301, 0},
			{"UPDATE big SET v = 'x' WHERE k = 7", 100, 100},
		} {
			r, w := rc.run(s.q)
			if r != s.read || w != s.written {
				t.Errorf("%s: read %d written %d, want %d %d", s.q, r, w, s.read, s.written)
			}
		}
	})
}

// TestRowCountsJITParity runs the same statements with and without the JIT,
// which must count the same rows.
func TestRowCountsJITParity(t *testing.T) {
	setup := []string{
		"CREATE TABLE a(id INTEGER PRIMARY KEY, k INTEGER, v TEXT, r REAL)",
		"CREATE TABLE b(id INTEGER PRIMARY KEY, aid INTEGER, w INTEGER)",
		"CREATE INDEX b_aid ON b(aid)",
		"INSERT INTO a SELECT value, value % 50, 'v' || (value % 7), value * 0.5 FROM generate_series(1, 3000)",
		"INSERT INTO b SELECT value, value % 3000 + 1, value % 11 FROM generate_series(1, 6000)",
		"CREATE TABLE c(x TEXT PRIMARY KEY, y INTEGER) WITHOUT ROWID",
		"INSERT INTO c SELECT 'k' || value, value FROM generate_series(1, 500)",
		"CREATE TABLE log(n INTEGER)",
		"CREATE TRIGGER a_del AFTER DELETE ON a BEGIN INSERT INTO log VALUES (old.id); END",
		"CREATE VIEW av AS SELECT k, count(*) n FROM a GROUP BY k",
	}
	queries := []string{
		"SELECT count(*) FROM a",
		"SELECT count(*) FROM a WHERE k = 3",
		"SELECT sum(id) FROM a WHERE k < 10",
		"SELECT sum(r) FROM a WHERE k > 40",
		"SELECT * FROM a WHERE id = 17",
		"SELECT * FROM a WHERE id BETWEEN 10 AND 30",
		"SELECT * FROM a WHERE id IN (1, 5, 9, 4000)",
		"SELECT v FROM a WHERE k = 3 LIMIT 5",
		"SELECT * FROM a ORDER BY r DESC LIMIT 3",
		"SELECT DISTINCT v FROM a",
		"SELECT k, count(*) FROM a GROUP BY k",
		"SELECT v, sum(r) FROM a GROUP BY v ORDER BY 2 LIMIT 2",
		"SELECT count(*) FROM b WHERE aid = 10",
		"SELECT aid, count(*) FROM b GROUP BY aid LIMIT 4",
		"SELECT aid, count(*) FROM b WHERE aid < 4 GROUP BY aid",
		"SELECT aid, sum(w) FROM b WHERE aid > 2990 GROUP BY aid LIMIT 2",
		"SELECT count(*) FROM b WHERE aid BETWEEN 3 AND 5",
		"SELECT sum(r) FROM a WHERE id BETWEEN 100 AND 199",
		"SELECT count(*) FROM a JOIN b ON b.aid = a.id WHERE a.id BETWEEN 1 AND 50",
		"SELECT k, count(*) FROM a WHERE id >= 2900 GROUP BY k",
		"SELECT count(*) FROM a JOIN b ON b.aid = a.id WHERE a.k = 1",
		"SELECT a.id, b.w FROM a JOIN b ON b.aid = a.id WHERE a.id < 20",
		"SELECT sum(b.w) FROM a JOIN b ON b.aid = a.id",
		"SELECT a.k, sum(b.w) FROM a JOIN b ON b.aid = a.id GROUP BY a.k",
		"SELECT * FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.aid = a.id AND b.w = 3) LIMIT 10",
		"SELECT * FROM a WHERE id IN (SELECT aid FROM b WHERE w = 2) LIMIT 10",
		"SELECT id, (SELECT count(*) FROM b WHERE b.aid = a.id) FROM a WHERE id < 50",
		"SELECT * FROM a LEFT JOIN b ON b.aid = a.id AND b.w = 99 WHERE a.id < 10",
		"WITH c AS (SELECT k FROM a WHERE id < 100) SELECT count(*) FROM c",
		"SELECT * FROM (SELECT k, count(*) n FROM a GROUP BY k) WHERE n > 1",
		"SELECT max(id) FROM a",
		"SELECT * FROM a ORDER BY id DESC LIMIT 2",
		"SELECT * FROM b ORDER BY aid LIMIT 5",
		"SELECT a.v, count(*) FROM b JOIN a ON a.id = b.aid WHERE b.w = 4 GROUP BY a.v",
		"SELECT * FROM a JOIN b ON b.aid = a.id WHERE a.k = 2 AND b.w > 5",
		"SELECT count(*) FROM a, b WHERE a.id = b.id AND a.k = 1",
		"SELECT * FROM c WHERE x = 'k10'",
		"SELECT count(*) FROM c WHERE y > 250",
		"SELECT * FROM av WHERE k = 3",
		"SELECT k FROM a WHERE id < 10 UNION SELECT aid FROM b WHERE id < 10",
		"SELECT id, sum(r) OVER (ORDER BY id) FROM a WHERE id < 20",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50) SELECT count(*) FROM n JOIN a ON a.id = n.i",
		"SELECT count(*) FROM json_each('[1,2,3,4]')",
		"SELECT * FROM a WHERE k IN (1, 2) AND id < 200",
		"SELECT * FROM b WHERE aid IN (5, 6, 7)",
		"SELECT min(aid), max(aid) FROM b",
		"SELECT min(aid) FROM b",
		"SELECT max(id) FROM b",
		"SELECT count(*) FROM c WHERE x = 'k7'",
		"SELECT * FROM a WHERE v LIKE 'v3%' LIMIT 4",
		"SELECT count(DISTINCT k) FROM a",
		"INSERT INTO c VALUES ('k1', 0) ON CONFLICT DO NOTHING",
		"REPLACE INTO c VALUES ('k2', -2)",
		"UPDATE b SET aid = aid + 1 WHERE id <= 30",
		"UPDATE b SET w = w + 1 WHERE aid = 100",
		"DELETE FROM a WHERE k = 9",
		"DELETE FROM log",
		"UPDATE a SET v = 'u' WHERE k = 4",
		"UPDATE a SET k = k + 100 WHERE id < 5",
		"DELETE FROM b WHERE w = 10",
		"INSERT INTO b(aid, w) SELECT id, 1 FROM a WHERE id <= 20",
		"INSERT INTO a(id, k, v) VALUES (5000, 1, 'x') ON CONFLICT(id) DO UPDATE SET v = 'y'",
		"INSERT INTO a(id, k, v) VALUES (5000, 1, 'x') ON CONFLICT(id) DO UPDATE SET v = 'z'",
		"DELETE FROM a WHERE id > 2990",
	}
	got := map[string][][2]int64{}
	withJIT(t, func(t *testing.T, mode string) {
		rc := newRowCounter(t, "parity-"+mode+".musq")
		for _, q := range setup {
			rc.run(q)
		}
		for _, q := range queries {
			r, w := rc.run(q)
			got[q] = append(got[q], [2]int64{r, w})
		}
	})
	for _, q := range queries {
		g := got[q]
		if len(g) == 2 && g[0] != g[1] {
			t.Errorf("%s: jit %v, nojit %v", q, g[0], g[1])
		}
		if testing.Verbose() {
			fmt.Printf("%-90s %v\n", q, g)
		}
	}
}
