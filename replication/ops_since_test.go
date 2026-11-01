package replication

import (
	"context"
	"testing"
	"time"
)

// TestOpsSinceReadsWhatTheLogHolds: ops up to a site's contiguous seq come by
// point reads, as many as asked; ops past it by the range scan; and a seq
// missing from the log ends the point reads there. No sync round runs, so
// nothing is pruned.
func TestOpsSinceReadsWhatTheLogHolds(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	quiet := []Option{WithSyncInterval(time.Hour)}
	c.openWith("a", quiet, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
	for _, q := range []string{`INSERT INTO t VALUES(1, 'x')`, `INSERT INTO t VALUES(2, 'y')`, `INSERT INTO t VALUES(3, 'z')`, `INSERT INTO t VALUES(4, 'w')`} {
		c.exec("a", q)
	}
	a := syncerOf(t, c.dbs["a"]).store
	all, err := a.OpsSince(ctx, a.site, 0, 0)
	if err != nil || len(all) < 4 {
		t.Fatalf("a's ops: %d, %v", len(all), err)
	}
	if two, err := a.OpsSince(ctx, a.site, 0, 2); err != nil || len(two) != 2 || two[1].Seq != all[1].Seq {
		t.Fatalf("limit 2: %v, %v", two, err)
	}
	last := all[len(all)-1].Seq
	if none, err := a.OpsSince(ctx, a.site, last, 0); err != nil || len(none) != 0 {
		t.Fatalf("past the last seq: %v, %v", none, err)
	}

	c.openWith("b", quiet, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
	b := syncerOf(t, c.dbs["b"]).store
	for _, k := range []int{0, 2} {
		if err := b.Ingest(ctx, all[k], false); err != nil {
			t.Fatal(err)
		}
	}
	ahead, err := b.OpsSince(ctx, a.site, all[0].Seq, 0)
	if err != nil || len(ahead) != 1 || ahead[0].Seq != all[2].Seq {
		t.Fatalf("an op past the contiguous seq: %v, %v", ahead, err)
	}

	if _, err := a.db.ExecContext(ctx, `DELETE FROM main._repl_oplog WHERE site=? AND seq=?`, a.site, int64(all[2].Seq)); err != nil {
		t.Fatal(err)
	}
	if got, err := a.OpsSince(ctx, a.site, 0, 0); err != nil || len(got) != 2 {
		t.Fatalf("a hole in the log: %d ops, %v", len(got), err)
	}
}
