package replication

import (
	"context"
	"errors"
	"sync"
)

var errPartitioned = errors.New("partition: peer unreachable")

// OpSource provides the op state for a node in the in-memory network.
type OpSource interface {
	// LocalVV returns the version vector for this node.
	LocalVV() VersionVector

	// OpsSince returns up to `limit` ops for the given site, starting after
	// `fromSeq` -- or ErrPruned when those are pruned here.
	OpsSince(site string, fromSeq uint64, limit int) ([]Op, error)

	// SnapshotPage serves one page of part of this node's snapshot.
	SnapshotPage(part int, cursor []byte) (SnapshotPage, error)

	// ExchangeAcks merges a peer's acks and returns this node's.
	ExchangeAcks(acks Acks) Acks

	// ReceiveOp is called when an op is pushed to this node from a peer.
	ReceiveOp(from PeerID, op Op)
}

// MemNetwork is an in-process network simulator for testing.
// It routes ops between nodes and supports fault injection (partitions, duplicates, reorders).
type MemNetwork struct {
	mu    sync.RWMutex
	nodes map[PeerID]*memNode

	// Per-link partition state: key = (a, b) as a, b sorted alphabetically.
	// If partitions[(a,b)] = true, then a and b are partitioned (both directions).
	partitions map[[2]PeerID]bool

	// Global fault injection knobs (simplified; applied deterministically).
	duplicate bool // if true, PushOp delivers the op twice to the target
	reorder   bool // if true, PushOp may reorder (simplified: queue-based)
}

// memNode represents a node in the network.
type memNode struct {
	id        PeerID
	src       OpSource
	mu        sync.RWMutex
	listeners []chan PeerOp // channels for SubscribePush
}

// NewMemNetwork creates a new in-process network simulator.
func NewMemNetwork() *MemNetwork {
	return &MemNetwork{
		nodes:      make(map[PeerID]*memNode),
		partitions: make(map[[2]PeerID]bool),
	}
}

// AddNode registers a node in the network and returns a Transport handle for it.
func (net *MemNetwork) AddNode(id PeerID, src OpSource) Transport {
	net.mu.Lock()
	defer net.mu.Unlock()

	net.nodes[id] = &memNode{
		id:        id,
		src:       src,
		listeners: make([]chan PeerOp, 0),
	}

	return &memTransport{
		net:       net,
		localID:   id,
		localNode: net.nodes[id],
	}
}

// Node returns a Transport handle for the given peer ID (node must already exist).
func (net *MemNetwork) Node(id PeerID) Transport {
	net.mu.RLock()
	node, ok := net.nodes[id]
	net.mu.RUnlock()

	if !ok {
		return nil
	}

	return &memTransport{
		net:       net,
		localID:   id,
		localNode: node,
	}
}

// Partition partitions or heals a link between two nodes.
// If dropped is true, communication between a and b is blocked in both directions.
// If dropped is false, the partition is removed and they can communicate again.
func (net *MemNetwork) Partition(a, b PeerID, dropped bool) {
	net.mu.Lock()
	defer net.mu.Unlock()

	// Normalize order so (a, b) is always stored in the same way.
	if a > b {
		a, b = b, a
	}

	key := [2]PeerID{a, b}
	if dropped {
		net.partitions[key] = true
	} else {
		delete(net.partitions, key)
	}
}

// SetDuplicate enables/disables duplicate delivery for PushOp.
func (net *MemNetwork) SetDuplicate(duplicate bool) {
	net.mu.Lock()
	defer net.mu.Unlock()
	net.duplicate = duplicate
}

// SetReorder enables/disables reordering for PushOp (simplified; currently a no-op).
func (net *MemNetwork) SetReorder(reorder bool) {
	net.mu.Lock()
	defer net.mu.Unlock()
	net.reorder = reorder
}

// isPartitioned checks if two nodes are partitioned.
func (net *MemNetwork) isPartitioned(a, b PeerID) bool {
	net.mu.RLock()
	defer net.mu.RUnlock()
	return net.partitioned(a, b)
}

// partitioned is isPartitioned for a caller already holding net.mu: taking the
// read lock again would deadlock against a Partition waiting for the write lock,
// since a pending writer blocks new readers (sync.RWMutex forbids recursive read
// locking).
func (net *MemNetwork) partitioned(a, b PeerID) bool {
	if a > b {
		a, b = b, a
	}
	return net.partitions[[2]PeerID{a, b}]
}

// shouldDuplicate returns whether to duplicate this push.
func (net *MemNetwork) shouldDuplicate() bool {
	net.mu.RLock()
	defer net.mu.RUnlock()
	return net.duplicate
}

// memTransport implements the Transport interface for a node in the network.
type memTransport struct {
	net       *MemNetwork
	localID   PeerID
	localNode *memNode
}

// Peers returns all other peer IDs in the network that are not partitioned from this node.
func (t *memTransport) Peers() []PeerID {
	t.net.mu.RLock()
	defer t.net.mu.RUnlock()

	var peers []PeerID
	for id := range t.net.nodes {
		if id != t.localID && !t.net.partitioned(t.localID, id) {
			peers = append(peers, id)
		}
	}
	return peers
}

// ExchangeVV exchanges version vectors with a peer.
// It returns the peer's local version vector.
func (t *memTransport) ExchangeVV(ctx context.Context, peer PeerID, vv VersionVector) (VersionVector, error) {
	if t.net.isPartitioned(t.localID, peer) {
		return nil, errPartitioned
	}

	t.net.mu.RLock()
	peerNode, ok := t.net.nodes[peer]
	t.net.mu.RUnlock()

	if !ok {
		return nil, errors.New("peer not found")
	}

	// Get the peer's local version vector.
	return peerNode.src.LocalVV().Clone(), nil
}

// GetOps retrieves operations from a peer for a given site and sequence range.
func (t *memTransport) GetOps(ctx context.Context, peer PeerID, site string, fromSeq uint64, limit int) ([]Op, error) {
	if t.net.isPartitioned(t.localID, peer) {
		return nil, errPartitioned
	}

	t.net.mu.RLock()
	peerNode, ok := t.net.nodes[peer]
	t.net.mu.RUnlock()

	if !ok {
		return nil, errors.New("peer not found")
	}

	// Get ops from the peer's OpSource.
	return peerNode.src.OpsSince(site, fromSeq, limit)
}

// ExchangeAcks trades acks with a peer.
func (t *memTransport) ExchangeAcks(ctx context.Context, peer PeerID, acks Acks) (Acks, error) {
	if t.net.isPartitioned(t.localID, peer) {
		return nil, errPartitioned
	}
	t.net.mu.RLock()
	peerNode, ok := t.net.nodes[peer]
	t.net.mu.RUnlock()
	if !ok {
		return nil, errors.New("peer not found")
	}
	return peerNode.src.ExchangeAcks(acks), nil
}

// GetSnapshot fetches one page of a peer's snapshot.
func (t *memTransport) GetSnapshot(ctx context.Context, peer PeerID, part int, cursor []byte) (SnapshotPage, error) {
	if t.net.isPartitioned(t.localID, peer) {
		return SnapshotPage{}, errPartitioned
	}
	t.net.mu.RLock()
	peerNode, ok := t.net.nodes[peer]
	t.net.mu.RUnlock()
	if !ok {
		return SnapshotPage{}, errors.New("peer not found")
	}
	return peerNode.src.SnapshotPage(part, cursor)
}

// PushOp sends an operation to a peer.
// The op is delivered to the peer's ReceiveOp and fanned to all SubscribePush channels.
func (t *memTransport) PushOp(ctx context.Context, peer PeerID, op Op) error {
	if t.net.isPartitioned(t.localID, peer) {
		return errPartitioned
	}

	t.net.mu.RLock()
	peerNode, ok := t.net.nodes[peer]
	shouldDuplicate := t.net.duplicate
	t.net.mu.RUnlock()

	if !ok {
		return errors.New("peer not found")
	}

	// Deliver the op to the peer's ReceiveOp.
	peerNode.src.ReceiveOp(t.localID, op)

	// Fan the op to all active SubscribePush channels.
	peerOp := PeerOp{From: t.localID, Op: op}
	t.fanToPushListeners(peerNode, peerOp)

	// Optionally duplicate: deliver again to both ReceiveOp and listeners.
	if shouldDuplicate {
		peerNode.src.ReceiveOp(t.localID, op)
		t.fanToPushListeners(peerNode, peerOp)
	}

	return nil
}

// fanToPushListeners sends an op to all active listeners on the peer node.
func (t *memTransport) fanToPushListeners(node *memNode, peerOp PeerOp) {
	node.mu.Lock()
	defer node.mu.Unlock()

	// Try to send to each listener; skip those that don't receive (likely context-cancelled).
	for i := 0; i < len(node.listeners); i++ {
		ch := node.listeners[i]
		select {
		case ch <- peerOp:
		default:
			// Non-blocking; if the channel is full or closed, skip.
		}
	}
}

// SubscribePush subscribes to pushed operations from peers.
// Returns a channel that receives PeerOp values.
// The channel is closed when the context is cancelled.
func (t *memTransport) SubscribePush(ctx context.Context) (<-chan PeerOp, error) {
	ch := make(chan PeerOp, 16) // Buffered to avoid blocking on send.

	t.localNode.mu.Lock()
	t.localNode.listeners = append(t.localNode.listeners, ch)
	t.localNode.mu.Unlock()

	// Launch a goroutine to clean up when the context is cancelled.
	go func() {
		<-ctx.Done()
		t.localNode.mu.Lock()
		// Remove this channel from the listeners list.
		for i, listener := range t.localNode.listeners {
			if listener == ch {
				t.localNode.listeners = append(t.localNode.listeners[:i], t.localNode.listeners[i+1:]...)
				break
			}
		}
		t.localNode.mu.Unlock()
		close(ch)
	}()

	return ch, nil
}
