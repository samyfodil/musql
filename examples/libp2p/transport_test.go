package libp2p_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	replp2p "github.com/samyfodil/musql/examples/libp2p"
	"github.com/samyfodil/musql/replication"
)

const usersDDL = `CREATE TABLE users(id TEXT PRIMARY KEY, name TEXT NOT NULL, age INTEGER)`

// newHost starts a libp2p host listening on an ephemeral 127.0.0.1 TCP port.
func newHost(t *testing.T) host.Host {
	t.Helper()
	h, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// connectHosts dials b from a.
func connectHosts(t *testing.T, a, b host.Host) {
	t.Helper()
	ai := peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}
	if err := a.Connect(context.Background(), ai); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

// openUsers makes a fresh database with a users table and opens it with
// replication.Open, syncing over h on topic.
func openUsers(t *testing.T, h host.Host, name, topic string) *sql.DB {
	t.Helper()
	return openUsersWith(t, name, replp2p.Sync(h, topic))
}

// openUsersWith is openUsers with the sync option given.
func openUsersWith(t *testing.T, name string, sync replication.Option) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(usersDDL); err != nil {
		t.Fatal(err)
	}
	db0.Close()
	db, err := replication.Open(context.Background(), path, replication.CRDT(), sync, replication.WithSyncInterval(200*time.Millisecond))
	if err != nil {
		t.Fatalf("replication.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dumpUsers renders the users table as a deterministic string for comparison.
func dumpUsers(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id, name, age FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, name string
		var age sql.NullInt64
		if err := rows.Scan(&id, &name, &age); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ageStr := "NULL"
		if age.Valid {
			ageStr = fmt.Sprintf("%d", age.Int64)
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", id, name, ageStr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return sb.String()
}

// waitFor polls cond every 100ms until it returns true or the timeout elapses,
// failing the test on timeout with msg.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestConvergence writes distinct rows concurrently on two connected,
// gossiping nodes and asserts both converge to the same final state, proving
// push-based propagation over the real libp2p transport.
func TestConvergence(t *testing.T) {
	hA := newHost(t)
	hB := newHost(t)
	connectHosts(t, hA, hB)

	dbA := openUsers(t, hA, "A", "repl-test-convergence")
	dbB := openUsers(t, hB, "B", "repl-test-convergence")

	if _, err := dbA.Exec(`INSERT INTO users(id,name,age) VALUES('1','alice',30)`); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := dbA.Exec(`INSERT INTO users(id,name,age) VALUES('2','bob',25)`); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := dbB.Exec(`INSERT INTO users(id,name,age) VALUES('3','carol',40)`); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	if _, err := dbA.Exec(`UPDATE users SET age=31 WHERE id='1'`); err != nil {
		t.Fatalf("update A: %v", err)
	}

	var dumpA, dumpB string
	waitFor(t, 15*time.Second, "convergence", func() bool {
		dumpA = dumpUsers(t, dbA)
		dumpB = dumpUsers(t, dbB)
		return dumpA != "" && dumpA == dumpB
	})

	want := "1|alice|31\n2|bob|25\n3|carol|40\n"
	if dumpA != want {
		t.Fatalf("converged to unexpected state:\nA:\n%s\nB:\n%s\nwant:\n%s", dumpA, dumpB, want)
	}
}

// TestAntiEntropyCatchUp writes rows on node A while B is not connected to it
// (so no push can possibly have reached it), then connects them and asserts B
// catches up purely via anti-entropy (Node.Sync pulling GetOps over the
// request/response stream), proving pull works independently of push.
func TestAntiEntropyCatchUp(t *testing.T) {
	hA := newHost(t)
	hB := newHost(t)
	// Deliberately not connected yet.

	dbA := openUsers(t, hA, "A", "repl-test-antientropy")
	dbB := openUsers(t, hB, "B", "repl-test-antientropy")

	// Writes happen while A and B are partitioned: no push can reach B.
	if _, err := dbA.Exec(`INSERT INTO users(id,name,age) VALUES('1','alice',30)`); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := dbA.Exec(`INSERT INTO users(id,name,age) VALUES('2','bob',25)`); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := dbA.Exec(`UPDATE users SET age=31 WHERE id='1'`); err != nil {
		t.Fatalf("update A: %v", err)
	}

	// B has definitely not received anything: confirm before connecting.
	if got := dumpUsers(t, dbB); got != "" {
		t.Fatalf("B already has data before connecting: %q", got)
	}

	// Now connect: only anti-entropy (periodic Sync, not push) can carry the
	// already-committed ops across, since PushOp never fired for them on B's side.
	connectHosts(t, hA, hB)

	want := "1|alice|31\n2|bob|25\n"
	waitFor(t, 15*time.Second, "anti-entropy catch-up", func() bool {
		return dumpUsers(t, dbB) == want
	})

	if got := dumpUsers(t, dbA); got != want {
		t.Fatalf("A state = %q, want %q", got, want)
	}
}

// TestOpenSync is the whole user-facing surface over real libp2p hosts: two
// databases opened with replication.Open(ctx, path, CRDT(), Sync(host, topic)) and nothing else
// converge, both directions.
func TestOpenSync(t *testing.T) {
	hA, hB := newHost(t), newHost(t)
	connectHosts(t, hA, hB)
	a, b := openUsers(t, hA, "a", "open-sync-test"), openUsers(t, hB, "b", "open-sync-test")
	if _, err := a.Exec(`INSERT INTO users VALUES('u1','ann',31)`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(`INSERT INTO users VALUES('u2','ben',32)`); err != nil {
		t.Fatal(err)
	}
	const want = "u1|ann|31\nu2|ben|32\n"
	waitFor(t, 20*time.Second, "both nodes to converge", func() bool {
		return dumpUsers(t, a) == want && dumpUsers(t, b) == want
	})
}

// TestTwoDatabasesOneHost: two databases on one host, sharing its router, each
// sync with their own peer and answer only for themselves -- and closing one
// leaves the other serving. Every write lands before the hosts connect, so only
// the request/response protocol (anti-entropy) can carry it.
func TestTwoDatabasesOneHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hA, hB, hC, hD := newHost(t), newHost(t), newHost(t), newHost(t)
	ps, err := pubsub.NewGossipSub(ctx, hA)
	if err != nil {
		t.Fatal(err)
	}
	a1 := openUsersWith(t, "a1", replp2p.SyncWith(hA, ps, "two-dbs-1"))
	a2 := openUsersWith(t, "a2", replp2p.SyncWith(hA, ps, "two-dbs-2"))
	b := openUsers(t, hB, "b", "two-dbs-1")
	c := openUsers(t, hC, "c", "two-dbs-2")
	for db, q := range map[*sql.DB]string{
		a1: `INSERT INTO users VALUES('1','a1',1)`,
		a2: `INSERT INTO users VALUES('2','a2',2)`,
		b:  `INSERT INTO users VALUES('3','b',3)`,
		c:  `INSERT INTO users VALUES('4','c',4)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	connectHosts(t, hB, hA)
	connectHosts(t, hC, hA)
	const want1, want2 = "1|a1|1\n3|b|3\n", "2|a2|2\n4|c|4\n"
	waitFor(t, 20*time.Second, "each pair to converge on its own rows", func() bool {
		return dumpUsers(t, a1) == want1 && dumpUsers(t, b) == want1 &&
			dumpUsers(t, a2) == want2 && dumpUsers(t, c) == want2
	})

	if err := a1.Close(); err != nil {
		t.Fatalf("close a1: %v", err)
	}
	if _, err := a2.Exec(`INSERT INTO users VALUES('5','a2 after',5)`); err != nil {
		t.Fatal(err)
	}
	// d has never been connected, so nothing was pushed to it: it catches up
	// from a2 through hA's handler, which a1's Close must have left in place.
	d := openUsers(t, hD, "d", "two-dbs-2")
	connectHosts(t, hD, hA)
	const want3 = "2|a2|2\n4|c|4\n5|a2 after|5\n"
	waitFor(t, 20*time.Second, "a2 to keep serving after a1 closed", func() bool {
		return dumpUsers(t, d) == want3
	})
}

// TestSyncOneRouterPerHost: Sync starts a router of its own, so a second Sync on
// the same host is refused rather than silently taking over the first one's
// gossip; and once db.Close stops it, the host takes a new one.
func TestSyncOneRouterPerHost(t *testing.T) {
	h := newHost(t)
	a := openUsers(t, h, "a", "one-router")
	path := filepath.Join(t.TempDir(), "b.db")
	if db, err := replication.Open(context.Background(), path, replication.CRDT(), replp2p.Sync(h, "one-router-2")); err == nil {
		db.Close()
		t.Fatal("a second Sync on one host was accepted")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err := replication.Open(context.Background(), path, replication.CRDT(), replp2p.Sync(h, "one-router-2"))
	if err != nil {
		t.Fatalf("Sync after the first database closed: %v", err)
	}
	db.Close()
}

// TestSnapshotOverLibp2p: two nodes write and prune their row ops; a third
// that connects later can only catch up by snapshot, fetched page by page over
// the request/response protocol.
func TestSnapshotOverLibp2p(t *testing.T) {
	hA, hB, hC := newHost(t), newHost(t), newHost(t)
	connectHosts(t, hA, hB)
	a := openUsers(t, hA, "a", "snapshot-test")
	b := openUsers(t, hB, "b", "snapshot-test")
	for i := range 600 { // more than one snapshot page
		if _, err := a.Exec(`INSERT OR REPLACE INTO users VALUES(?, 'n', ?)`, fmt.Sprintf("u%03d", i%300), i); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 30*time.Second, "a and b to converge", func() bool { return dumpUsers(t, a) == dumpUsers(t, b) })
	waitFor(t, 30*time.Second, "a to prune", func() bool {
		var n int
		if err := a.QueryRow(`SELECT count(*) FROM _repl_oplog WHERE op <> 3`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n < 50
	})
	c := openUsers(t, hC, "c", "snapshot-test")
	connectHosts(t, hC, hA)
	want := dumpUsers(t, a)
	waitFor(t, 30*time.Second, "c to catch up by snapshot", func() bool { return dumpUsers(t, c) == want })
}
