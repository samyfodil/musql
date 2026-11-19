package replication

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// TestCaptureKeepsSchemaReduction: a local schema change goes on from the
// reductions the last one left (Syncer.capR, capW) rather than replaying every
// schema op again, and those always hold what a fresh replay of the log does:
// after each statement, after a schema change rolled back, after one that
// changes nothing, a TEMP table, and a schema op from a peer.
func TestCaptureKeepsSchemaReduction(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	a := c.open("a")
	s := syncerOf(t, a)
	// The log's schema ops, read through db: the store's own connection is
	// its sync loop's.
	logOps := func(db *sql.DB) []Op {
		t.Helper()
		rows, err := db.Query(schemaOpsQuery)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all [][]any
		for rows.Next() {
			r := make([]any, 4)
			if err := rows.Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
				t.Fatal(err)
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		ops, err := schemaOpsFrom(all)
		if err != nil {
			t.Fatal(err)
		}
		return ops
	}
	check := func(step string, wantKept bool) {
		t.Helper()
		ops := logOps(a)
		r, w := s.takeReplays()
		defer s.keepReplays(r, w)
		if r == nil {
			if wantKept {
				t.Fatalf("%s: no reduction kept", step)
			}
			return
		}
		cat, sc, err := replaySchema(ctx, ops)
		if err != nil {
			t.Fatal(err)
		}
		defer sc.close()
		want, err := sc.snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, k := range []*schemaReplay{r, w} {
			if !slices.Equal(k.keys, schemaKeys(ops)) {
				if wantKept {
					t.Fatalf("%s: kept %d: keys %v, the log's %v", step, i, k.keys, schemaKeys(ops))
				}
				continue
			}
			got, err := k.sc.snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d := sameCatalog(got, want); d != "" {
				t.Fatalf("%s: kept %d differs from a replay: %s", step, i, d)
			}
			if !k.cat.liveEqual(cat) {
				t.Fatalf("%s: kept %d's catalog differs from a replay", step, i)
			}
		}
	}
	steps := []struct {
		sql  []string
		kept bool
	}{
		{[]string{`CREATE TABLE t1(id INTEGER PRIMARY KEY, v TEXT)`}, true},
		{[]string{`CREATE INDEX t1_v ON t1(v)`}, true},
		{[]string{`ALTER TABLE t1 ADD COLUMN w INTEGER DEFAULT 3`}, true},
		{[]string{`CREATE TABLE t2(k TEXT PRIMARY KEY, n INT)`, `CREATE VIEW v1 AS SELECT k FROM t2`}, true},
		{[]string{`ALTER TABLE t2 RENAME COLUMN n TO m`}, true},
		{[]string{`DROP INDEX t1_v`}, true},
		{[]string{`CREATE TABLE IF NOT EXISTS t2(k TEXT PRIMARY KEY, m INT)`}, false},
		{[]string{`CREATE TABLE t3(x)`}, true},
		{[]string{`CREATE TEMP TABLE tt(x)`}, false},
		{[]string{`DROP TABLE t3`}, true},
	}
	for _, st := range steps {
		c.exec("a", st.sql...)
		check(st.sql[0], st.kept)
	}

	// Rolled back: what was kept is spent, and the next change replays.
	tx, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE gone(x)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	check("rollback", false)
	c.exec("a", `CREATE TABLE t4(x)`)
	check("after a rollback", true)

	// A peer's schema ops land after the kept ones: the next local change goes
	// on from them through them. Opened only now, so that none of its ops can
	// land between two of the local changes above, which would leave the kept
	// reductions behind the log and a full replay the right answer.
	b := c.open("b")
	c.exec("b", `CREATE TABLE fromb(y)`)
	siteOps := func(db *sql.DB) int {
		n := 0
		for _, op := range logOps(db) {
			if op.Site == "b" {
				n++
			}
		}
		return n
	}
	// Every one of b's: one arriving after the change below would leave what
	// it keeps behind the log.
	for deadline := time.Now().Add(10 * time.Second); siteOps(a) < siteOps(b); {
		if time.Now().After(deadline) {
			t.Fatalf("a holds %d of b's %d schema ops", siteOps(a), siteOps(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.exec("a", `CREATE INDEX t4_x ON t4(x)`)
	check("after a peer's", true)
	c.converge("t4_x\n", `SELECT name FROM sqlite_master WHERE name = 't4_x'`, schemaQ)
}
