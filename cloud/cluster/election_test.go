package cluster

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// startNodes starts n nodes on localhost, each the others' peer.
func startNodes(t *testing.T, n int) []*Node {
	t.Helper()
	type ident struct {
		dir, id string
		port    int
	}
	ids := make([]ident, n)
	for i := range ids {
		dir := t.TempDir()
		key, err := LoadKey(filepath.Join(dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := PeerID(key)
		if err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		ids[i] = ident{dir, id, port}
	}
	nodes := make([]*Node, n)
	for i := range nodes {
		var peers []Peer
		for j, p := range ids {
			if j != i {
				peers = append(peers, Peer{ID: p.id, Addrs: []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", p.port)}})
			}
		}
		key, err := LoadKey(filepath.Join(ids[i].dir, "node.key"))
		if err != nil {
			t.Fatal(err)
		}
		nd, err := Start(key, []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", ids[i].port)}, peers, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = nd
		t.Cleanup(func() { nd.Close() })
	}
	return nodes
}

// elect joins n to the election, which the test leaves before n closes.
func elect(t *testing.T, n *Node, dir, name string, members []string) *Election {
	t.Helper()
	e, err := n.Elect(dir, name, members, 50*time.Millisecond, fast)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

var fast = ElectionTiming{Heartbeat: 200 * time.Millisecond, Election: 200 * time.Millisecond, Lease: 100 * time.Millisecond}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !f(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// writers is how many of es report themselves the master.
func writers(es []*Election) (n int, who int) {
	for i, e := range es {
		if e != nil && e.Writer() {
			n, who = n+1, i
		}
	}
	return n, who
}

// TestElection: three nodes elect one master, which every node names, and
// when it leaves the other two elect another.
func TestElection(t *testing.T) {
	nodes := startNodes(t, 3)
	var members []string
	for _, n := range nodes {
		members = append(members, n.ID())
	}
	es := make([]*Election, len(nodes))
	for i, n := range nodes {
		es[i] = elect(t, n, t.TempDir(), "state", members)
	}
	waitFor(t, "a master", func() bool { n, _ := writers(es); return n == 1 })
	_, m := writers(es)
	for i, e := range es {
		if got := e.Leader(); got != nodes[m].ID() {
			t.Fatalf("node %d names %q as the master, not %q", i, got, nodes[m].ID())
		}
	}
	if err := es[m].Close(); err != nil {
		t.Fatal(err)
	}
	es[m] = nil
	waitFor(t, "a new master", func() bool { n, w := writers(es); return n == 1 && w != m })
}

// TestElectionRestart: an election's term, vote and membership survive a
// restart: a node that restarts rejoins without starting the group again.
func TestElectionRestart(t *testing.T) {
	nodes := startNodes(t, 3)
	var members []string
	for _, n := range nodes {
		members = append(members, n.ID())
	}
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	es := make([]*Election, len(nodes))
	for i, n := range nodes {
		es[i] = elect(t, n, dirs[i], "state", members)
	}
	waitFor(t, "a master", func() bool { n, _ := writers(es); return n == 1 })
	for i, e := range es {
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		es[i] = nil
	}
	// Restarted with no members: each must find them in its own state.
	for i, n := range nodes {
		es[i] = elect(t, n, dirs[i], "state", nil)
	}
	waitFor(t, "a master after the restart", func() bool { n, _ := writers(es); return n == 1 })
}

// TestElectionMembers: the master adds a node placed in the group later, and
// removes one no longer placed there.
func TestElectionMembers(t *testing.T) {
	nodes := startNodes(t, 3)
	two := []string{nodes[0].ID(), nodes[1].ID()}
	es := make([]*Election, 3)
	for i := range 2 {
		es[i] = elect(t, nodes[i], t.TempDir(), "g", two)
	}
	waitFor(t, "a master of two", func() bool { n, _ := writers(es); return n == 1 })
	all := append(two, nodes[2].ID())
	es[2] = elect(t, nodes[2], t.TempDir(), "g", all)
	_, m := writers(es)
	if err := es[m].SetMembers(all); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the third node to know the master", func() bool { return es[2].Leader() == nodes[m].ID() })
	// The master leaves the group: it removes itself last, and the others go on.
	var rest []string
	for i, n := range nodes {
		if i != m {
			rest = append(rest, n.ID())
		}
	}
	if err := es[m].SetMembers(rest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a master among the rest", func() bool { n, w := writers(es); return n == 1 && w != m })
}
