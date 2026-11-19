package replication

import (
	"context"
	"slices"
	"testing"
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
	c.open("b")
	s := syncerOf(t, a)
	check := func(step string, wantKept bool) {
		t.Helper()
		ops, err := s.store.schemaOps(ctx)
		if err != nil {
			t.Fatal(err)
		}
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

	// A peer's schema op lands after the kept ones: the next local change goes
	// on from them through it.
	c.exec("b", `CREATE TABLE fromb(y)`)
	c.converge("fromb\n", `SELECT name FROM sqlite_master WHERE name = 'fromb'`)
	c.exec("a", `CREATE INDEX t4_x ON t4(x)`)
	check("after a peer's", true)
	c.converge("t4_x\n", `SELECT name FROM sqlite_master WHERE name = 't4_x'`, schemaQ)
}
