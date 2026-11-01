package replication

import (
	"context"
	"testing"
	"time"
)

// TestPushedConflictingOpIsRefused: a pushed op that names a seq its site
// already wrote, with other contents, is refused, and the node keeps what it
// had.
func TestPushedConflictingOpIsRefused(t *testing.T) {
	ctx := context.Background()
	quiet := []Option{WithSyncInterval(time.Hour)}
	c := newCluster(t)
	c.openWith("a", quiet, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
	c.exec("a", `INSERT INTO t VALUES(1, 'one')`)
	st := syncerOf(t, c.dbs["a"]).store
	ops, err := st.OpsSince(ctx, st.site, 0, 0)
	if err != nil || len(ops) == 0 {
		t.Fatalf("a's ops: %v, %v", ops, err)
	}
	other := ops[len(ops)-1]
	other.HLC++
	c.openWith("b", quiet)
	if err := c.net.Node("b").PushOp(ctx, "a", other); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := dump(t, c.dbs["a"], `SELECT id, v FROM t`); got != "1|one\n" {
		t.Fatalf("a holds %q after the conflicting push", got)
	}
}
