package replication

import (
	"fmt"
	"testing"
	"time"
)

// eventually waits until cond holds, failing with what it last said.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ok, got := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", what, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logRowOps is how many row and ack ops a node's log still holds.
func logRowOps(t *testing.T, c *cluster, site string) string {
	return dump(t, c.dbs[site], `SELECT count(*) FROM _repl_oplog WHERE op <> 3`)
}

// TestPruneThenSnapshot: once every node has acked them, the row ops are
// pruned -- the log stops holding every write ever made. A node that joins
// after can no longer get them as ops (ErrPruned), so it takes a snapshot and
// converges anyway; its writes reach the others; and a node whose own ops were
// pruned keeps numbering past them, so its next write still lands.
func TestPruneThenSnapshot(t *testing.T) {
	c := newCluster(t)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, n INT)`)
	c.open("b")
	for i := range 40 {
		c.exec("a", fmt.Sprintf(`INSERT OR REPLACE INTO t VALUES('k%d', %d)`, i%5, i))
	}
	const q = `SELECT id, n FROM t ORDER BY id`
	want := "k0|35\nk1|36\nk2|37\nk3|38\nk4|39\n"
	c.converge(want, q)
	for _, site := range []string{"a", "b"} {
		eventually(t, site+" pruned its row ops", func() (bool, string) {
			got := logRowOps(t, c, site)
			var n int
			fmt.Sscan(got, &n)
			return n < 10, got
		})
	}

	c.open("c")
	c.converge(want, q)

	c.exec("c", `INSERT INTO t VALUES('from-c', 1)`)
	c.exec("a", `UPDATE t SET n = 99 WHERE id = 'k0'`)
	c.converge("from-c|1\nk0|99\nk1|36\nk2|37\nk3|38\nk4|39\n", q)
}

// TestRetire: a node that is gone for good never acks again, so nothing is
// pruned past what it had -- until Retire takes it out of the members, on
// every node.
func TestRetire(t *testing.T) {
	c := newCluster(t)
	a := c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY, n INT)`)
	c.open("b")
	c.open("x")
	c.converge("3\n", `SELECT count(*) FROM _repl_ranges`)
	time.Sleep(300 * time.Millisecond) // x's ack reaches a and b
	for _, peer := range []string{"a", "b"} {
		c.net.Partition("x", PeerID(peer), true)
	}
	for i := range 20 {
		c.exec("a", fmt.Sprintf(`INSERT OR REPLACE INTO t VALUES('k%d', %d)`, i%3, i))
	}
	time.Sleep(500 * time.Millisecond) // many rounds: a and b ack each other, x does not
	var before int
	fmt.Sscan(logRowOps(t, c, "a"), &before)
	if before < 20 {
		t.Fatalf("a pruned ops x never had: %d row ops left", before)
	}
	if err := Retire(t.Context(), a, "x"); err != nil {
		t.Fatal(err)
	}
	if err := Retire(t.Context(), a, "a"); err == nil {
		t.Fatal("a node retired itself")
	}
	for _, site := range []string{"a", "b"} {
		eventually(t, site+" pruned once x was retired", func() (bool, string) {
			got := logRowOps(t, c, site)
			var n int
			fmt.Sscan(got, &n)
			return n < 10, got
		})
	}
}

func schemaOpOf(site string, seq uint64, hlc HLC, so schemaOp) Op {
	return Op{Site: site, Seq: seq, HLC: hlc, Kind: OpSchema, Cells: so.cells()}
}

// TestRetiredNodeKeepsWhatItLacks: a node retired by the others still never
// prunes past its own version vector -- the others' acks say they have ops it
// does not.
func TestRetiredNodeKeepsWhatItLacks(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t, "b")
	for _, op := range []Op{
		insertOp("a", 1, 100, "u1", "x", 1),
		schemaOpOf("a", 2, 200, schemaOp{Act: actRetire, Name: "b"}),
	} {
		if err := s.Ingest(ctx, op, false); err != nil {
			t.Fatal(err)
		}
	}
	far := VersionVector{"a": 4, "schema": 1}
	s.MergeAcks(Acks{"a": {HLC: 300, VV: far}, "schema": {HLC: 300, VV: far}})
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	if f := s.floors["a"]; f > 2 {
		t.Fatalf("floor for a is %d, past the 2 this node holds", f)
	}
	if err := s.Ingest(ctx, insertOp("a", 3, 300, "u3", "y", 3), false); err != nil {
		t.Fatal(err)
	}
	if got := dumpUsers(t, s); got != "u1|x|1\nu3|y|3\n" {
		t.Fatalf("a's op 3 after the prune: %q", got)
	}
}

// TestSchemaOpBelowAFloorStillLands: a snapshot can raise a floor past a schema
// op this node never saw; schema ops are never pruned, so one below a floor
// that is not in the log is applied, where a row op there is dropped.
func TestSchemaOpBelowAFloorStillLands(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t, "floor")
	if err := s.finishSnapshot(ctx, VersionVector{"x": 5}, nil); err != nil {
		t.Fatal(err)
	}
	const ddl = `CREATE TABLE late(id TEXT PRIMARY KEY)`
	if err := s.Ingest(ctx, schemaOpOf("x", 3, 300, schemaOp{Act: actCreate, ID: objectID("table", "late", ddl, 0), Type: "table", Name: "late", SQL: ddl}), false); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, s.db, `SELECT count(*) FROM sqlite_master WHERE name = 'late'`); got != "1\n" {
		t.Fatalf("a schema op below the floor was dropped")
	}
	if err := s.Ingest(ctx, insertOp("x", 4, 400, "u9", "z", 9), false); err != nil {
		t.Fatal(err)
	}
	if got := dumpUsers(t, s); got != "" {
		t.Fatalf("a row op below the floor was applied again: %q", got)
	}
	if same, exists, err := s.holds(ctx, insertOp("x", 4, 400, "u9", "z", 9)); err != nil || !same || !exists {
		t.Fatalf("an op below the floor is applied, not missing: %v %v %v", same, exists, err)
	}
}

// TestClockSeedsTheHLC: after the row ops are pruned, the highest HLC this
// database has seen may be only in the clock; a restart must not hand out an
// older one, which the peers would let lose.
func TestClockSeedsTheHLC(t *testing.T) {
	ctx := t.Context()
	db, err := openApplyDB("file:hlcseed?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := InitSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	future := HLC(uint64(time.Now().Add(time.Hour).UnixMilli()) << 16)
	if _, err := db.Exec(`INSERT INTO _repl_clock(tbl, pk, col, hlc, site, val) VALUES('t', x'01', 'c', ?, 'old', x'01')`, int64(future)); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(ctx, db, "hlcseed")
	if err != nil {
		t.Fatal(err)
	}
	if now := s.Clock().Now(); now <= future {
		t.Fatalf("the clock restarted at %d, below the %d in _repl_clock", now, future)
	}
}

// TestSnapshotPagesHaveAByteBudget: a page stops at snapshotPageBytes, so big
// values cannot make one larger than a transport carries.
func TestSnapshotPagesHaveAByteBudget(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t, "pages")
	big := make([]byte, 1<<20)
	for i := range 40 {
		if err := s.Ingest(ctx, insertOp("w", uint64(i+1), HLC(i+1), fmt.Sprintf("u%02d", i), string(big), int64(i)), false); err != nil {
			t.Fatal(err)
		}
	}
	var cursor []byte
	pages, cells := 0, 0
	for {
		page, err := s.SnapshotPage(ctx, SnapshotClock, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		cells += len(page.Cells)
		size := 0
		for _, c := range page.Cells {
			size += len(c.Val)
		}
		if size > snapshotPageBytes+len(big)+64 { // the budget, plus the cell that crossed it
			t.Fatalf("a page of %d bytes", size)
		}
		if cursor = page.Next; cursor == nil {
			break
		}
	}
	if pages < 3 || cells < 120 {
		t.Fatalf("%d pages, %d cells", pages, cells)
	}
}
