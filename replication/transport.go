package replication

import "context"

// PeerID uniquely identifies a peer in the network.
type PeerID string

// PeerOp represents an operation received from a peer.
type PeerOp struct {
	From PeerID
	Op   Op
}

// Transport provides the network interface for CRDT sync operations.
type Transport interface {
	// Peers returns the list of connected peers.
	Peers() []PeerID

	// ExchangeVV exchanges version vectors with a peer. It sends the local
	// version vector and receives the peer's version vector. This is used
	// for cheap anti-entropy discovery.
	ExchangeVV(ctx context.Context, peer PeerID, vv VersionVector) (VersionVector, error)

	// GetOps retrieves operations from a peer for a given site and sequence range.
	// It retrieves up to `limit` operations starting from the operation after
	// `fromSeq` (exclusive).
	GetOps(ctx context.Context, peer PeerID, site string, fromSeq uint64, limit int) ([]Op, error)

	// ExchangeAcks sends this node's known acks to a peer and returns the
	// peer's (prune.go).
	ExchangeAcks(ctx context.Context, peer PeerID, acks Acks) (Acks, error)

	// GetSnapshot fetches one page of part of a peer's snapshot (Store.
	// SnapshotPage), after cursor; nil is the first page.
	GetSnapshot(ctx context.Context, peer PeerID, part int, cursor []byte) (SnapshotPage, error)

	// PushOp sends an operation to a peer.
	PushOp(ctx context.Context, peer PeerID, op Op) error

	// SubscribePush subscribes to pushed operations from peers.
	// The returned channel receives PeerOp values. The channel is closed
	// when the subscription ends or the context is cancelled.
	SubscribePush(ctx context.Context) (<-chan PeerOp, error)
}
