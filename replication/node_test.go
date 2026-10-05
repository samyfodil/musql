package replication

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

// newNodeOn wires a freshly-opened Store into net under id, returning the
// Node. It follows the two-step construction Node requires when paired with
// MemNetwork: NewNode needs no Transport up front because OpSource only
// touches the Store, so AddNode can run before the Transport is bound back.
func newNodeOn(t *testing.T, net *MemNetwork, id PeerID, site string) *Node {
	t.Helper()
	s := newTestStore(t, site)
	n := NewNode(s, nil)
	tr := net.AddNode(id, n.OpSource())
	n.SetTransport(tr)
	return n
}

// syncAll runs one Sync() round on every node, in order.
func syncAll(t *testing.T, ctx context.Context, nodes []*Node) {
	t.Helper()
	for _, n := range nodes {
		if err := n.Sync(ctx); err != nil {
			t.Fatalf("sync on %s: %v", n.Store().Site(), err)
		}
	}
}

// snapshot concatenates every node's users + clock dump into one comparable
// string, so a whole-fleet round can be checked for "nothing changed" without
// tracking per-node dirty bits.
func snapshot(t *testing.T, nodes []*Node) string {
	t.Helper()
	var out string
	for _, n := range nodes {
		out += dumpUsers(t, n.Store()) + "--\n" + dumpClock(t, n.Store()) + "==\n"
	}
	return out
}

// TestPropertyConvergence drives ~200 random inserts/updates/deletes across 4
// nodes sharing a MemNetwork, with duplicate delivery and a mid-run partition
// injected as faults, then runs anti-entropy rounds until the fleet stops
// changing. All nodes must land on byte-identical user tables and clocks
// regardless of delivery order, duplication, or the partition.
func TestPropertyConvergence(t *testing.T) {
	const (
		numNodes = 4
		numOps   = 200
		numKeys  = 8
	)

	r := rand.New(rand.NewSource(42)) // fixed seed: deterministic and reproducible
	ctx := context.Background()

	net := NewMemNetwork()
	ids := make([]PeerID, numNodes)
	nodes := make([]*Node, numNodes)
	for i := 0; i < numNodes; i++ {
		ids[i] = PeerID(fmt.Sprintf("node%d", i))
		nodes[i] = newNodeOn(t, net, ids[i], fmt.Sprintf("prop%d", i))
	}

	names := []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi"}
	key := func(i int) string { return fmt.Sprintf("k%d", i) }

	// Fault-injection schedule, in terms of op index within the run.
	const (
		dupOn       = 40
		dupOff      = 120
		partitionAt = 70
		healAt      = 150
	)
	partA, partB := ids[0], ids[1]

	for i := 0; i < numOps; i++ {
		switch i {
		case dupOn:
			net.SetDuplicate(true)
		case dupOff:
			net.SetDuplicate(false)
		case partitionAt:
			net.Partition(partA, partB, true)
		case healAt:
			net.Partition(partA, partB, false)
		}

		node := nodes[r.Intn(numNodes)]
		pk := EncodePK(key(r.Intn(numKeys)))
		name := names[r.Intn(len(names))]
		age := int64(r.Intn(90))

		var op Op
		switch r.Intn(3) {
		case 0: // insert: every NOT NULL column gets a value
			op = node.MakeOp(usersID, pk, OpInsert,
				[]Cell{CellFromValue(ucol("name"), name), CellFromValue(ucol("age"), age)})
		case 1: // update: one random column
			var cell Cell
			if r.Intn(2) == 0 {
				cell = CellFromValue(ucol("name"), name)
			} else {
				cell = CellFromValue(ucol("age"), age)
			}
			op = node.MakeOp(usersID, pk, OpUpdate, []Cell{cell})
		default: // delete
			op = node.MakeOp(usersID, pk, OpDelete, nil)
		}

		if err := node.LocalWrite(ctx, op); err != nil {
			t.Fatalf("op %d LocalWrite on %s: %v", i, node.Store().Site(), err)
		}
	}

	// Make sure faults don't outlive the generation phase; only convergence
	// behavior under normal conditions is asserted below.
	net.SetDuplicate(false)
	net.Partition(partA, partB, false)

	// Anti-entropy until the fleet stops changing, or a bounded number of
	// rounds elapses (guards against a hang if convergence is broken).
	const maxRounds = 25
	prev := snapshot(t, nodes)
	converged := false
	for round := 0; round < maxRounds; round++ {
		syncAll(t, ctx, nodes)
		cur := snapshot(t, nodes)
		if cur == prev {
			converged = true
			break
		}
		prev = cur
	}
	if !converged {
		t.Fatalf("did not settle within %d anti-entropy rounds", maxRounds)
	}

	// All nodes must agree, byte for byte.
	wantUsers := dumpUsers(t, nodes[0].Store())
	wantClock := dumpClock(t, nodes[0].Store())
	failed := false
	for i, n := range nodes {
		if got := dumpUsers(t, n.Store()); got != wantUsers {
			t.Errorf("node %d users diverged from node 0", i)
			failed = true
		}
		if got := dumpClock(t, n.Store()); got != wantClock {
			t.Errorf("node %d clock diverged from node 0", i)
			failed = true
		}
	}
	if failed {
		for i, n := range nodes {
			t.Logf("-- node %d (%s) users --\n%s", i, n.Store().Site(), dumpUsers(t, n.Store()))
			t.Logf("-- node %d (%s) clock --\n%s", i, n.Store().Site(), dumpClock(t, n.Store()))
		}
		t.FailNow()
	}
}

// TestSyncCatchUpAfterPartition isolates the anti-entropy path: two nodes
// diverge while partitioned, then a Sync in each direction after healing must
// bring both to the same state.
func TestSyncCatchUpAfterPartition(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()

	nA := newNodeOn(t, net, "A", "syncA")
	nB := newNodeOn(t, net, "B", "syncB")

	net.Partition("A", "B", true)

	opA := nA.MakeOp(usersID, EncodePK("u1"), OpInsert,
		[]Cell{CellFromValue(ucol("name"), "alice"), CellFromValue(ucol("age"), int64(30))})
	if err := nA.LocalWrite(ctx, opA); err != nil {
		t.Fatalf("LocalWrite on A: %v", err)
	}

	opB := nB.MakeOp(usersID, EncodePK("u2"), OpInsert,
		[]Cell{CellFromValue(ucol("name"), "bob"), CellFromValue(ucol("age"), int64(25))})
	if err := nB.LocalWrite(ctx, opB); err != nil {
		t.Fatalf("LocalWrite on B: %v", err)
	}

	// While partitioned, each side only knows about its own write.
	if got := dumpUsers(t, nA.Store()); got != "u1|alice|30\n" {
		t.Fatalf("A before heal = %q", got)
	}
	if got := dumpUsers(t, nB.Store()); got != "u2|bob|25\n" {
		t.Fatalf("B before heal = %q", got)
	}

	net.Partition("A", "B", false)

	if err := nA.Sync(ctx); err != nil {
		t.Fatalf("A sync: %v", err)
	}
	if err := nB.Sync(ctx); err != nil {
		t.Fatalf("B sync: %v", err)
	}

	const want = "u1|alice|30\nu2|bob|25\n"
	gotA, gotB := dumpUsers(t, nA.Store()), dumpUsers(t, nB.Store())
	if gotA != want {
		t.Errorf("A after sync = %q, want %q", gotA, want)
	}
	if gotB != want {
		t.Errorf("B after sync = %q, want %q", gotB, want)
	}
	if gotA != gotB {
		t.Fatalf("A and B diverged after mutual sync:\nA: %q\nB: %q", gotA, gotB)
	}

	if cA, cB := dumpClock(t, nA.Store()), dumpClock(t, nB.Store()); cA != cB {
		t.Fatalf("clocks diverged after mutual sync:\nA: %s\nB: %s", cA, cB)
	}
}

// stubPeer is one peer serving fixed ops.
type stubPeer struct{ ops map[string][]Op }

func (p stubPeer) Peers() []PeerID { return []PeerID{"p"} }
func (p stubPeer) ExchangeVV(context.Context, PeerID, VersionVector) (VersionVector, error) {
	vv := VersionVector{}
	for site, ops := range p.ops {
		vv[site] = uint64(len(ops))
	}
	return vv, nil
}
func (p stubPeer) GetOps(_ context.Context, _ PeerID, site string, from uint64, _ int) ([]Op, error) {
	return p.ops[site][from:], nil
}
func (p stubPeer) PushOp(context.Context, PeerID, Op) error { return nil }
func (p stubPeer) GetSnapshot(context.Context, PeerID, int, []byte) (SnapshotPage, error) {
	return SnapshotPage{}, nil
}
func (p stubPeer) ExchangeAcks(context.Context, PeerID, Acks) (Acks, error) { return nil, nil }
func (p stubPeer) SubscribePush(context.Context) (<-chan PeerOp, error)     { return nil, nil }

// TestSyncSkipsARefusedSite: one site whose ops this node refuses does not stop
// a sync round from taking every other site's.
func TestSyncSkipsARefusedSite(t *testing.T) {
	s := newTestStore(t, "s94558")
	peer := stubPeer{ops: map[string][]Op{"s113311": {insertOp("s113311", 1, 100, "bad", "x", 1)}}}
	for i := range 8 {
		site := fmt.Sprintf("g%d", i)
		peer.ops[site] = []Op{insertOp(site, 1, HLC(200+i), site, "y", 2)}
	}
	if err := NewNode(s, peer).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	vv := s.LocalVV()
	for site := range peer.ops {
		if want := uint64(1); site != "s113311" && vv[site] != want {
			t.Fatalf("site %s: vv %d, want %d (vv %v)", site, vv[site], want, vv)
		}
	}
	if vv["s113311"] != 0 {
		t.Fatalf("the refused site's op was taken: %v", vv)
	}
}
