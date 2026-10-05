package replication

import (
	"context"
	"testing"
)

// TestTempShadowsDoNotCaptureBookkeeping: TEMP is a database of the connection's
// own, and an unqualified name resolves against it BEFORE main (build.c's
// sqlite3LocateTable walks aDb from 1). So a connection that creates a TEMP
// table named like a bookkeeping table must not divert replication into it: a
// captured op written into a temp _repl_oplog is never durable and never
// shipped -- the write would vanish from every peer.
func TestTempShadowsDoNotCaptureBookkeeping(t *testing.T) {
	ctx := context.Background()
	s, db := wrapUsers(t, "shadow")
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		`CREATE TEMP TABLE _repl_oplog(site, seq, hlc, tbl, pk, op, cells)`,
		`CREATE TEMP TABLE _repl_clock(tbl, pk, col, hlc, site, val, UNIQUE(tbl, pk, col))`,
		`CREATE TEMP TABLE _repl_meta(k PRIMARY KEY, v)`,
		`INSERT INTO users(id,name,age) VALUES('u1','alice',30)`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rowOps(ops)); n != 1 {
		t.Fatalf("the insert left %d ops in main's log, want 1 -- captured into a TEMP shadow", n)
	}
	var shadowed int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp._repl_oplog`).Scan(&shadowed); err != nil {
		t.Fatal(err)
	}
	if shadowed != 0 {
		t.Fatalf("%d ops went into the TEMP _repl_oplog", shadowed)
	}
}
