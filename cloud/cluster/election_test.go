package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
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
	e, err := n.Elect(dir, name, members, 50*time.Millisecond, fast, nil)
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

// recorder is a StateMachine that keeps what it applied, as key=data.
type recorder struct {
	mu      sync.Mutex
	applied []string
}

func (r *recorder) Apply(key string, data []byte) error {
	if key == "refuse" {
		return errors.New("refused")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, key+"="+string(data))
	return nil
}

func (r *recorder) Snapshot() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []byte(strings.Join(r.applied, "\n")), nil
}

func (r *recorder) Restore(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = strings.Split(string(b), "\n")
	return nil
}

func (r *recorder) Stop() {}

func (r *recorder) list() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.applied, ",")
}

// TestElectionLog: what the master proposes is applied on every member in
// one order, and Propose returns what the master's Apply did. A member that
// joins after the master compacted its log starts from a snapshot. A
// follower cannot propose.
func TestElectionLog(t *testing.T) {
	nodes := startNodes(t, 3)
	two := []string{nodes[0].ID(), nodes[1].ID()}
	timing := fast
	timing.Snapshot, timing.Trailing = 4, 1
	rs := []*recorder{{}, {}, {}}
	es := make([]*Election, 3)
	join := func(i int, members []string) {
		e, err := nodes[i].Elect(t.TempDir(), "g", members, 0, timing, rs[i])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close() })
		es[i] = e
	}
	join(0, two)
	join(1, two)
	waitFor(t, "a master", func() bool { n, _ := writers(es); return n == 1 })
	_, m := writers(es)
	var want []string
	for i := range 20 {
		if err := es[m].Propose("db", fmt.Appendf(nil, "%d", i)); err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("db=%d", i))
	}
	if err := es[m].Propose("refuse", nil); err == nil || err.Error() != "refused" {
		t.Fatalf("a refused entry: %v", err)
	}
	if err := es[1-m].Propose("db", []byte("x")); err == nil {
		t.Fatal("a follower proposed")
	}
	if got := rs[m].list(); got != strings.Join(want, ",") {
		t.Fatalf("the master applied %q", got)
	}
	waitFor(t, "the follower to apply every entry", func() bool { return rs[1-m].list() == strings.Join(want, ",") })

	// The log is compacted by now: the third node starts from a snapshot.
	waitFor(t, "a snapshot", func() bool {
		first, err := es[m].st.FirstIndex()
		return err == nil && first > 1
	})
	all := append(two, nodes[2].ID())
	join(2, all)
	if err := es[m].SetMembers(all); err != nil {
		t.Fatal(err)
	}
	if err := es[m].Propose("db", []byte("last")); err != nil {
		t.Fatal(err)
	}
	want = append(want, "db=last")
	waitFor(t, "the third node to hold every entry", func() bool { return rs[2].list() == strings.Join(want, ",") })
}

// TestRaftStoreAdoptsJSON: an election's state from before its log moved to
// a database, raft.json, is moved into it.
func TestRaftStoreAdoptsJSON(t *testing.T) {
	dir := t.TempDir()
	old := struct {
		Stable map[string][]byte
		Logs   []*raft.Log
	}{
		Stable: map[string][]byte{"CurrentTerm": {0, 0, 0, 0, 0, 0, 0, 7}},
		Logs:   []*raft.Log{{Index: 3, Term: 6, Type: raft.LogConfiguration, Data: []byte("c")}, {Index: 4, Term: 7, Type: raft.LogNoop}},
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raft.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := openRaftStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if term, err := st.GetUint64([]byte("CurrentTerm")); err != nil || term != 7 {
		t.Fatalf("term %d, %v", term, err)
	}
	first, err := st.FirstIndex()
	if err != nil {
		t.Fatal(err)
	}
	last, err := st.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	var l raft.Log
	if err := st.GetLog(3, &l); err != nil || first != 3 || last != 4 || l.Term != 6 || l.Type != raft.LogConfiguration || string(l.Data) != "c" {
		t.Fatalf("log %d..%d, entry 3 %+v, %v", first, last, l, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "raft.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raft.json is still there: %v", err)
	}
}
