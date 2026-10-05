package replication

import (
	"context"
	"sync"
	"testing"
	"time"
)

// testOpSource is a simple in-memory OpSource for testing.
type testOpSource struct {
	mu       chan struct{} // simple concurrency guard
	vv       VersionVector
	ops      map[string][]Op // ops by site
	received []PeerOp        // received ops
}

func newTestOpSource() *testOpSource {
	return &testOpSource{
		mu:       make(chan struct{}, 1),
		vv:       make(VersionVector),
		ops:      make(map[string][]Op),
		received: make([]PeerOp, 0),
	}
}

func (s *testOpSource) lock() {
	s.mu <- struct{}{}
}

func (s *testOpSource) unlock() {
	<-s.mu
}

func (s *testOpSource) LocalVV() VersionVector {
	s.lock()
	defer s.unlock()
	return s.vv.Clone()
}

func (s *testOpSource) SnapshotPage(int, []byte) (SnapshotPage, error) {
	return SnapshotPage{}, nil
}

func (s *testOpSource) ExchangeAcks(Acks) Acks { return nil }

func (s *testOpSource) OpsSince(site string, fromSeq uint64, limit int) ([]Op, error) {
	s.lock()
	defer s.unlock()

	siteOps, ok := s.ops[site]
	if !ok {
		return []Op{}, nil
	}

	// Find ops after fromSeq.
	var result []Op
	for _, op := range siteOps {
		if op.Seq > fromSeq {
			result = append(result, op)
			if len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (s *testOpSource) ReceiveOp(from PeerID, op Op) {
	s.lock()
	defer s.unlock()

	s.received = append(s.received, PeerOp{From: from, Op: op})

	// Update VV to track receipt.
	if s.vv[op.Site] < op.Seq {
		s.vv[op.Site] = op.Seq
	}
}

// addOp adds an op to the source's storage (for serving via GetOps).
func (s *testOpSource) addOp(op Op) {
	s.lock()
	defer s.unlock()

	s.ops[op.Site] = append(s.ops[op.Site], op)
	if s.vv[op.Site] < op.Seq {
		s.vv[op.Site] = op.Seq
	}
}

// TestMemNetworkBasic tests basic two-node scenario:
// - GetOps from one node reaches another
// - PushOp is received and delivered to SubscribePush channel
// - Partition blocks communication
// - Heal restores communication
func TestMemNetworkBasic(t *testing.T) {
	// Set up network and two nodes.
	net := NewMemNetwork()

	srcA := newTestOpSource()
	srcB := newTestOpSource()

	transportA := net.AddNode("A", srcA)
	transportB := net.AddNode("B", srcB)

	// Add some ops to node B so node A can retrieve them.
	op1 := Op{
		Site:  "siteB",
		Seq:   1,
		HLC:   100,
		Tbl:   "users",
		PK:    []byte("user1"),
		Kind:  OpInsert,
		Cells: []Cell{{Col: "name", Type: TypeText, Val: []byte("Alice")}},
	}
	srcB.addOp(op1)

	op2 := Op{
		Site:  "siteB",
		Seq:   2,
		HLC:   200,
		Tbl:   "users",
		PK:    []byte("user1"),
		Kind:  OpUpdate,
		Cells: []Cell{{Col: "age", Type: TypeInt, Val: []byte{30}}},
	}
	srcB.addOp(op2)

	// Test 1: A can retrieve ops from B via GetOps.
	ops, err := transportA.GetOps(context.Background(), "B", "siteB", 0, 10)
	if err != nil {
		t.Fatalf("GetOps failed: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(ops))
	}
	if ops[0].Seq != 1 || ops[1].Seq != 2 {
		t.Fatalf("unexpected op sequences: %v, %v", ops[0].Seq, ops[1].Seq)
	}

	// Test 2: PushOp from A to B is received by B.
	opA := Op{
		Site:  "siteA",
		Seq:   1,
		HLC:   150,
		Tbl:   "users",
		PK:    []byte("user2"),
		Kind:  OpInsert,
		Cells: []Cell{{Col: "name", Type: TypeText, Val: []byte("Bob")}},
	}

	// Subscribe B to pushes before sending.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	pushCh, err := transportB.SubscribePush(ctx)
	if err != nil {
		t.Fatalf("SubscribePush failed: %v", err)
	}

	// Push from A to B.
	if err := transportA.PushOp(context.Background(), "B", opA); err != nil {
		t.Fatalf("PushOp failed: %v", err)
	}

	// B should have received the op via ReceiveOp.
	if len(srcB.received) != 1 {
		t.Fatalf("expected 1 received op, got %d", len(srcB.received))
	}
	if srcB.received[0].From != "A" || srcB.received[0].Op.Seq != 1 {
		t.Fatalf("unexpected received op: %+v", srcB.received[0])
	}

	// B should receive the op on its SubscribePush channel.
	select {
	case peerOp := <-pushCh:
		if peerOp.From != "A" || peerOp.Op.Seq != 1 {
			t.Fatalf("unexpected push op: %+v", peerOp)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for push op")
	}

	cancel()

	// Test 3: After partition, communication fails.
	net.Partition("A", "B", true)

	// GetOps should error.
	_, err = transportA.GetOps(context.Background(), "B", "siteB", 0, 10)
	if err == nil {
		t.Fatal("expected error after partition, got nil")
	}
	if err != errPartitioned {
		t.Logf("expected errPartitioned, got %v", err)
	}

	// PushOp should error.
	err = transportA.PushOp(context.Background(), "B", opA)
	if err == nil {
		t.Fatal("expected error for PushOp after partition, got nil")
	}
	if err != errPartitioned {
		t.Logf("expected errPartitioned, got %v", err)
	}

	// Test 4: After healing, communication works again.
	net.Partition("A", "B", false)

	ops, err = transportA.GetOps(context.Background(), "B", "siteB", 0, 10)
	if err != nil {
		t.Fatalf("GetOps after heal failed: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops after heal, got %d", len(ops))
	}

	// Test 5: Peers() excludes partitioned nodes.
	net.Partition("A", "B", true)
	peers := transportA.Peers()
	for _, peer := range peers {
		if peer == "B" {
			t.Fatalf("Peers() should not include partitioned peer B")
		}
	}

	net.Partition("A", "B", false)
	peers = transportA.Peers()
	found := false
	for _, peer := range peers {
		if peer == "B" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Peers() should include B after partition healed")
	}

	// Test 6: Duplicate flag causes PushOp to deliver twice.
	net.SetDuplicate(true)
	srcB.received = srcB.received[:0] // clear

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	pushCh2, _ := transportB.SubscribePush(ctx2)

	opA2 := Op{
		Site: "siteA",
		Seq:  2,
		HLC:  250,
		Tbl:  "users",
		PK:   []byte("user3"),
		Kind: OpInsert,
	}
	if err := transportA.PushOp(context.Background(), "B", opA2); err != nil {
		t.Fatalf("PushOp with duplicate failed: %v", err)
	}

	// Should receive 2 copies in the receive list.
	if len(srcB.received) != 2 {
		t.Fatalf("expected 2 received ops with duplicate, got %d", len(srcB.received))
	}

	// Should receive at least 1 on the channel (buffered; might get both).
	count := 0
	for count < 2 {
		select {
		case peerOp := <-pushCh2:
			if peerOp.Op.Seq != 2 {
				t.Fatalf("unexpected op seq: %d", peerOp.Op.Seq)
			}
			count++
		case <-ctx2.Done():
			break
		}
	}

	cancel2()

	t.Log("All tests passed!")
}

// TestMemNetworkNodeLookup covers MemNetwork.Node: it returns a working
// Transport handle for an existing node, and nil for an unknown one.
func TestMemNetworkNodeLookup(t *testing.T) {
	net := NewMemNetwork()
	src := newTestOpSource()
	net.AddNode("A", src)

	tr := net.Node("A")
	if tr == nil {
		t.Fatal("Node(\"A\") = nil, want a Transport")
	}
	if peers := tr.Peers(); len(peers) != 0 {
		t.Fatalf("Peers() on a lone node = %v, want empty", peers)
	}

	if got := net.Node("missing"); got != nil {
		t.Fatalf("Node(\"missing\") = %v, want nil", got)
	}
}

// TestMemNetworkReorderAndDuplicateFlags covers the SetReorder setter and the
// shouldDuplicate getter directly (PushOp itself reads the field, but the
// getter is part of the public-ish surface and otherwise never exercised).
func TestMemNetworkReorderAndDuplicateFlags(t *testing.T) {
	net := NewMemNetwork()

	net.SetReorder(true)
	net.SetReorder(false) // no observable effect (documented no-op); just cover the setter.

	if net.shouldDuplicate() {
		t.Fatal("shouldDuplicate() = true before SetDuplicate(true)")
	}
	net.SetDuplicate(true)
	if !net.shouldDuplicate() {
		t.Fatal("shouldDuplicate() = false after SetDuplicate(true)")
	}
	net.SetDuplicate(false)
	if net.shouldDuplicate() {
		t.Fatal("shouldDuplicate() = true after SetDuplicate(false)")
	}
}

// TestMemNetworkPeerNotFound covers the "peer not found" error path of
// ExchangeVV, GetOps and PushOp, distinct from the "partitioned" error path
// already covered by TestMemNetworkBasic.
func TestMemNetworkPeerNotFound(t *testing.T) {
	net := NewMemNetwork()
	src := newTestOpSource()
	tr := net.AddNode("A", src)

	if _, err := tr.ExchangeVV(context.Background(), "ghost", VersionVector{}); err == nil {
		t.Fatal("ExchangeVV to an unknown peer: got nil error")
	}
	if _, err := tr.GetOps(context.Background(), "ghost", "site", 0, 10); err == nil {
		t.Fatal("GetOps from an unknown peer: got nil error")
	}
	if err := tr.PushOp(context.Background(), "ghost", Op{}); err == nil {
		t.Fatal("PushOp to an unknown peer: got nil error")
	}
}

// TestMemNetworkExchangeVV tests version vector exchange.
func TestMemNetworkExchangeVV(t *testing.T) {
	net := NewMemNetwork()

	srcA := newTestOpSource()
	srcB := newTestOpSource()

	transportA := net.AddNode("A", srcA)
	net.AddNode("B", srcB)

	// Set up version vectors.
	srcB.vv["site1"] = 42
	srcB.vv["site2"] = 100

	// A exchanges VV with B.
	vv, err := transportA.ExchangeVV(context.Background(), "B", VersionVector{"siteA": 1})
	if err != nil {
		t.Fatalf("ExchangeVV failed: %v", err)
	}

	if vv["site1"] != 42 || vv["site2"] != 100 {
		t.Fatalf("unexpected VV: %v", vv)
	}

	// Verify the returned VV is a clone (not a reference).
	vv["site1"] = 999
	if srcB.vv["site1"] != 42 {
		t.Fatal("returned VV is not a clone")
	}

	t.Log("ExchangeVV test passed!")
}

// TestMemNetworkPeersDuringPartition: Peers racing Partition must not deadlock.
// Peers once re-took the read lock it already held, which a waiting Partition
// blocks -- one stress run in ~200 of the crdt tests hung in partition(false).
func TestMemNetworkPeersDuringPartition(t *testing.T) {
	net := NewMemNetwork()
	tr := net.AddNode("a", nil)
	net.AddNode("b", nil)
	net.AddNode("c", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20000 {
					tr.Peers()
				}
			}()
		}
		for i := range 20000 {
			net.Partition("a", "b", i%2 == 0)
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Peers and Partition deadlocked")
	}
}
