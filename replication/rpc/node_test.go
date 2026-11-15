package rpc_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/replication"
	"github.com/samyfodil/musql/replication/rpc"
	v1 "github.com/samyfodil/musql/replication/rpc/gen/replication/v1"
	"github.com/samyfodil/musql/replication/rpc/gen/replication/v1/replicationv1connect"
)

const usersDDL = `CREATE TABLE users(id TEXT PRIMARY KEY, name TEXT NOT NULL, age INTEGER)`

// cluster is nodes on httptest servers, each knowing the others' URLs.
type cluster struct {
	mu   sync.Mutex
	urls map[replication.PeerID]string
	ids  []replication.PeerID
}

func (c *cluster) Peers() []replication.PeerID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]replication.PeerID(nil), c.ids...)
}

func (c *cluster) URL(p replication.PeerID) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.urls[p]
	return u, ok
}

// node starts a node named id, serving on an httptest server.
func (c *cluster) node(t *testing.T, id string) *rpc.Node {
	t.Helper()
	n := rpc.NewNode(replication.PeerID(id), c, nil)
	mux := http.NewServeMux()
	mux.Handle(n.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c.mu.Lock()
	if c.urls == nil {
		c.urls = map[replication.PeerID]string{}
	}
	c.urls[replication.PeerID(id)] = srv.URL
	c.ids = append(c.ids, replication.PeerID(id))
	c.mu.Unlock()
	return n
}

// openUsers makes a database with a users table and opens it syncing over n
// as database.
func openUsers(t *testing.T, n *rpc.Node, database string) *sql.DB {
	t.Helper()
	return openUsersEvery(t, n, database, 100*time.Millisecond)
}

// openUsersEvery is openUsers running an anti-entropy round every interval.
func openUsersEvery(t *testing.T, n *rpc.Node, database string, interval time.Duration) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), database+".db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(usersDDL); err != nil {
		t.Fatal(err)
	}
	db0.Close()
	db, err := replication.Open(context.Background(), path, replication.CRDT(), n.Sync(database), replication.WithSyncInterval(interval))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func dumpUsers(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, name, coalesce(age, 'NULL') FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, name, age string
		if err := rows.Scan(&id, &name, &age); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", id, name, age)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", msg)
		}
	}
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// TestConvergence: three nodes writing at once converge, by push and by
// anti-entropy.
func TestConvergence(t *testing.T) {
	var c cluster
	dbs := []*sql.DB{openUsers(t, c.node(t, "a"), "app"), openUsers(t, c.node(t, "b"), "app"), openUsers(t, c.node(t, "c"), "app")}
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Go(func() {
			for j := range 20 {
				exec(t, db, `INSERT INTO users VALUES(?, ?, ?)`, fmt.Sprintf("n%d-%02d", i, j), "x", j)
			}
		})
	}
	wg.Wait()
	has := func(db *sql.DB, id string) func() bool {
		return func() bool { return strings.Contains(dumpUsers(t, db), id+"|") }
	}
	waitFor(t, "b to hold n0-03", has(dbs[1], "n0-03"))
	waitFor(t, "c to hold n1-07", has(dbs[2], "n1-07"))
	exec(t, dbs[1], `UPDATE users SET age = NULL WHERE id = 'n0-03'`)
	exec(t, dbs[2], `DELETE FROM users WHERE id = 'n1-07'`)
	waitFor(t, "the nodes to converge", func() bool {
		a := dumpUsers(t, dbs[0])
		return strings.Count(a, "\n") == 59 && strings.Contains(a, "n0-03|x|NULL") && a == dumpUsers(t, dbs[1]) && a == dumpUsers(t, dbs[2])
	})
}

// TestPush: a commit reaches the peers by push, with no anti-entropy round
// to carry it.
func TestPush(t *testing.T) {
	var c cluster
	a, b := openUsersEvery(t, c.node(t, "a"), "app", time.Hour), openUsersEvery(t, c.node(t, "b"), "app", time.Hour)
	exec(t, a, `INSERT INTO users VALUES('p1', 'a', 1), ('p2', 'a', 2)`)
	waitFor(t, "b to take a's push", func() bool { return dumpUsers(t, b) == "p1|a|1\np2|a|2\n" })
	exec(t, b, `UPDATE users SET age = 9 WHERE id = 'p2'`)
	waitFor(t, "a to take b's push", func() bool { return dumpUsers(t, a) == "p1|a|1\np2|a|9\n" })
}

// TestDatabasesByName: one node serves several databases, each synced only
// with the database of its name on the others.
func TestDatabasesByName(t *testing.T) {
	var c cluster
	na, nb := c.node(t, "a"), c.node(t, "b")
	a1, a2 := openUsers(t, na, "one"), openUsers(t, na, "two")
	b1, b2 := openUsers(t, nb, "one"), openUsers(t, nb, "two")
	exec(t, a1, `INSERT INTO users VALUES('in-one', 'a', 1)`)
	exec(t, b2, `INSERT INTO users VALUES('in-two', 'b', 2)`)
	waitFor(t, "each database to converge", func() bool {
		return dumpUsers(t, b1) == "in-one|a|1\n" && dumpUsers(t, a2) == "in-two|b|2\n"
	})
	if got := dumpUsers(t, a1); got != "in-one|a|1\n" {
		t.Fatalf("one on a holds %q", got)
	}
	if _, err := replication.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), replication.CRDT(), na.Sync("one")); err == nil {
		t.Fatal("a second database synced as one on a")
	}
}

// TestSnapshotCatchUp: a node that joins after the others pruned their logs
// catches up by snapshot.
func TestSnapshotCatchUp(t *testing.T) {
	var c cluster
	a, b := openUsers(t, c.node(t, "a"), "app"), openUsers(t, c.node(t, "b"), "app")
	for i := range 600 { // more than one snapshot page
		exec(t, a, `INSERT OR REPLACE INTO users VALUES(?, 'n', ?)`, fmt.Sprintf("u%03d", i%300), i)
	}
	waitFor(t, "a and b to converge", func() bool { return dumpUsers(t, a) == dumpUsers(t, b) })
	waitFor(t, "a to prune", func() bool {
		var n int
		if err := a.QueryRow(`SELECT count(*) FROM _repl_oplog WHERE op <> 3`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n < 50
	})
	late := openUsers(t, c.node(t, "c"), "app")
	want := dumpUsers(t, a)
	waitFor(t, "c to catch up by snapshot", func() bool { return dumpUsers(t, late) == want })
}

// TestImage: a node that joins with nothing takes a peer's database as an
// image, and then what is written after it.
func TestImage(t *testing.T) {
	var c cluster
	a := openUsers(t, c.node(t, "a"), "app")
	// a's own sync round may commit while the transaction is open, which then
	// cannot commit; it is tried again.
	for try := 1; ; try++ {
		tx, err := a.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3000 { // more ops than an empty node pulls one by one
			if _, err := tx.Exec(`INSERT INTO users VALUES(?, 'n', ?)`, fmt.Sprintf("u%04d", i), i); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err == nil {
			break
		}
		if try == 5 {
			t.Fatal(err)
		}
	}
	b, err := replication.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"), replication.CRDT(),
		c.node(t, "b").Sync("app"), replication.WithSyncInterval(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	waitFor(t, "b to take a's image", func() bool { return replication.ImageInstalled(b) })
	exec(t, a, `UPDATE users SET age = -1 WHERE id = 'u0007'`)
	exec(t, b, `INSERT INTO users VALUES('from-b', 'b', 1)`)
	waitFor(t, "a and b to converge", func() bool {
		s := dumpUsers(t, a)
		return strings.Count(s, "\n") == 3001 && s == dumpUsers(t, b)
	})
}

// TestRefusals: a database the node does not serve is NotFound, and a
// malformed op is refused.
func TestRefusals(t *testing.T) {
	var c cluster
	openUsers(t, c.node(t, "a"), "app")
	url, _ := c.URL("a")
	cl := replicationv1connect.NewReplicationServiceClient(http.DefaultClient, url)
	ctx := context.Background()
	_, err := cl.ExchangeVV(ctx, connect.NewRequest(&v1.ExchangeVVRequest{Database: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an unknown database: %v", err)
	}
	_, err = cl.PushOps(ctx, connect.NewRequest(&v1.PushOpsRequest{Database: "app", From: "x", Ops: []*v1.Op{{Site: "s", Seq: 1, Kind: 99}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a malformed op: %v", err)
	}
	if _, err := cl.ExchangeVV(ctx, connect.NewRequest(&v1.ExchangeVVRequest{Database: "app"})); err != nil {
		t.Fatal(err)
	}
	stream, err := cl.GetImage(ctx, connect.NewRequest(&v1.GetImageRequest{Database: "app", Request: &v1.ImageRequest{Layout: "no such layout"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetRefused() == "" {
		t.Fatalf("an image no node can install: %v, %v", stream.Msg(), stream.Err())
	}
}

// TestConversions: every message converts back to what it was made from.
func TestConversions(t *testing.T) {
	ops := []replication.Op{
		{Site: "s1", Seq: 4, HLC: 77, Tbl: "users", PK: []byte{1, 2}, Kind: replication.OpUpdate,
			Cells: []replication.Cell{{Col: "name", Type: replication.TypeText, Val: []byte("x")}, {Col: "age", Type: replication.TypeNull}}},
		{Site: "s2", Seq: 1, Tbl: "users", PK: []byte{3}, Kind: replication.OpDelete, Cells: []replication.Cell{}},
	}
	got, err := rpc.OpsFromProto(rpc.OpsToProto(ops))
	if err != nil || fmt.Sprint(got) != fmt.Sprint(ops) {
		t.Fatalf("ops: %v, %v", got, err)
	}
	page := replication.SnapshotPage{Ops: ops, Floors: replication.VersionVector{"s1": 3}, Ranges: map[string]uint64{"s1": 1 << 30},
		Cells: []replication.ClockCell{{Tbl: "users", PK: []byte{1}, Col: "name", HLC: 9, Site: "s1", Val: []byte("v")}}, Next: []byte("c")}
	if p, err := rpc.SnapshotPageFromProto(rpc.SnapshotPageToProto(page)); err != nil || fmt.Sprint(p) != fmt.Sprint(page) {
		t.Fatalf("snapshot page: %v, %v", p, err)
	}
	acks := replication.Acks{"s1": {HLC: 5, VV: replication.VersionVector{"s1": 4, "s2": 1}}}
	if a := rpc.AcksFromProto(rpc.AcksToProto(acks)); fmt.Sprint(a) != fmt.Sprint(acks) {
		t.Fatalf("acks: %v", a)
	}
	m := replication.ImageManifest{Site: "s1", FileVersion: 3, DeltaVersion: 2, Layout: "l", Catalog: "c", Size: 100, DeltaSize: 10,
		Checksum: 7, VV: replication.VersionVector{"s1": 4}, Floors: replication.VersionVector{}, Ranges: map[string]uint64{"s1": 1}, MaxHLC: 99}
	if back := rpc.ImageManifestFromProto(rpc.ImageManifestToProto(m)); fmt.Sprint(back) != fmt.Sprint(m) {
		t.Fatalf("manifest: %v", back)
	}
	req := replication.ImageRequest{Layout: "l", FileVersion: 3, MinFileVersion: 1, DeltaVersion: 2}
	if back := rpc.ImageRequestFromProto(rpc.ImageRequestToProto(req)); back != req {
		t.Fatalf("image request: %v", back)
	}
	b, err := rpc.MarshalQuorumState(replication.VersionVector{"s1": 4, "s2": 9})
	if err != nil {
		t.Fatal(err)
	}
	if vv, err := rpc.UnmarshalQuorumState(b); err != nil || fmt.Sprint(vv) != "map[s1:4 s2:9]" {
		t.Fatalf("quorum state: %v, %v", vv, err)
	}
	for _, bad := range []*v1.Op{{Seq: 1}, {Site: "s"}, {Site: "s", Seq: 1, Kind: 99}, {Site: "s", Seq: 1, Cells: []*v1.Cell{{Col: "c", Type: 9}}}} {
		if _, err := rpc.OpsFromProto([]*v1.Op{bad}); err == nil {
			t.Errorf("malformed op %v accepted", bad)
		}
	}
	if _, err := rpc.UnmarshalQuorumState([]byte{0xff}); err == nil {
		t.Error("a malformed quorum state decoded")
	}
}
