package replication

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// Node wires a Store to a Transport: it applies local writes (materializing
// them and broadcasting to peers) and drives anti-entropy sync rounds against
// the peers the Transport knows about.
//
// Phase 1 has no capture (no preupdate hook wired yet), so every Ingest in
// this package uses local=false: the op is always (re)materialized into the
// user table, including for the writer's own local writes.
type Node struct {
	store *Store
	tr    Transport

	seq atomic.Uint64 // next local seq to hand out, monotonic per site
}

// NewNode creates a Node over an already-open Store and Transport. The local
// seq counter picks up from the store's current version vector for its own
// site, so freshly synthesized ops continue the existing oplog rather than
// colliding with it.
func NewNode(s *Store, tr Transport) *Node {
	n := &Node{store: s, tr: tr}
	n.seq.Store(s.LocalVV()[s.Site()])
	return n
}

// Store returns the underlying Store.
func (n *Node) Store() *Store { return n.store }

// Transport returns the underlying Transport.
func (n *Node) Transport() Transport { return n.tr }

// SetTransport (re)binds the Node's Transport. It exists to break the
// construction cycle with MemNetwork: OpSource only needs the Store, so the
// usual sequence is NewNode(store, nil), then net.AddNode(id, node.OpSource())
// to obtain the Transport, then node.SetTransport(tr).
func (n *Node) SetTransport(tr Transport) { n.tr = tr }

// NextSeq hands out the next per-site local seq number, for synthesizing ops.
func (n *Node) NextSeq() uint64 { return n.seq.Add(1) }

// MakeOp synthesizes an Op for a local write: it stamps this node's site,
// the next local seq, and the current HLC time, leaving tbl/pk/kind/cells to
// the caller. This is a convenience for callers (e.g. tests) that generate
// operations directly, without going through a preupdate-hook capture path.
func (n *Node) MakeOp(tbl string, pk []byte, kind OpKind, cells []Cell) Op {
	return Op{
		Site:  n.store.Site(),
		Seq:   n.NextSeq(),
		HLC:   n.store.Clock().Now(),
		Tbl:   tbl,
		PK:    pk,
		Kind:  kind,
		Cells: cells,
	}
}

// LocalWrite ingests op into this node's own store (materializing it — see
// the Phase 1 note on Node), then broadcasts it to every peer the transport
// currently knows about. Per-peer push failures are ignored: a partitioned or
// unreachable peer will pick the op up later via anti-entropy (Sync).
func (n *Node) LocalWrite(ctx context.Context, op Op) error {
	if err := n.store.Ingest(ctx, op, false); err != nil {
		return err
	}
	for _, peer := range n.tr.Peers() {
		_ = n.tr.PushOp(ctx, peer, op)
	}
	return nil
}

// Broadcast pushes already-committed local ops to every current peer,
// best-effort. Peers that miss the push converge later via Sync.
func (n *Node) Broadcast(ops []Op) {
	if n.tr == nil {
		return
	}
	peers := n.tr.Peers()
	for _, op := range ops {
		for _, peer := range peers {
			_ = n.tr.PushOp(context.Background(), peer, op)
		}
	}
}

// opsPerFetch caps how many ops Sync pulls from a peer per GetOps round trip.
const opsPerFetch = 64

// Sync runs one anti-entropy round: it exchanges version vectors with every
// currently reachable peer, then pulls and ingests any ops that peer has and
// this node doesn't, for every site. Unreachable (partitioned) peers are
// skipped; sync with them resumes automatically once the partition heals and
// Sync is called again.
func (n *Node) Sync(ctx context.Context) error {
	for _, peer := range n.tr.Peers() {
		peerVV, err := n.tr.ExchangeVV(ctx, peer, n.store.LocalVV())
		if err != nil {
			continue // partitioned or otherwise unreachable; skip this peer
		}
		for site, peerSeq := range peerVV {
			mySeq := n.store.LocalVV()[site]
			if peerSeq <= mySeq {
				continue
			}
			fromSeq := mySeq
			for fromSeq < peerSeq {
				ops, err := n.tr.GetOps(ctx, peer, site, fromSeq, opsPerFetch)
				if isPruned(err) {
					// The peer pruned what this node lacks: take its state
					// instead, and let the next round continue from there.
					if berr := n.bootstrap(ctx, peer); berr != nil {
						log.Printf("replication: snapshot from %s: %v", peer, berr)
					}
					return nil
				}
				if err != nil {
					break // went unreachable mid-fetch; pick up next Sync
				}
				if len(ops) == 0 {
					break // no-progress guard: peer has nothing more to offer
				}
				failed := false
				for _, op := range ops {
					if err := n.store.Ingest(ctx, op, false); err != nil {
						// This site's later ops wait on this one; the other sites,
						// and the other peers, do not.
						log.Printf("replication: sync %s from %s: %v", site, peer, err)
						failed = true
						break
					}
				}
				if failed {
					break
				}
				newSeq := n.store.LocalVV()[site]
				if newSeq <= fromSeq {
					break // no-progress guard: ingest didn't advance our VV
				}
				fromSeq = newSeq
			}
		}
		// Acks travel with every round, to every peer, whatever it pulled.
		if acks, err := n.tr.ExchangeAcks(ctx, peer, n.store.Acks()); err == nil {
			n.store.MergeAcks(acks)
		}
	}
	return nil
}

// Run drives the node until ctx is cancelled: it applies ops pushed by peers as
// they arrive and runs a full anti-entropy Sync every syncInterval. It works
// with any Transport (in-memory or libp2p), using only the Transport interface.
func (n *Node) Run(ctx context.Context, syncInterval time.Duration) error {
	ch, err := n.tr.SubscribePush(ctx)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case po, ok := <-ch:
			if !ok {
				ch = nil // push stream closed; rely on periodic Sync
				continue
			}
			if err := n.store.Ingest(ctx, po.Op, false); err != nil {
				log.Printf("replication: apply pushed op from %s: %v", po.From, err)
			}
		case <-ticker.C:
			if err := n.Sync(ctx); err != nil {
				log.Printf("replication: sync round: %v", err)
			}
			if err := n.store.prune(ctx); err != nil {
				log.Printf("replication: prune: %v", err)
			}
		}
	}
}

// nodeOpSource adapts a Node to memtransport's OpSource interface, so a Node
// can be registered directly with a MemNetwork via
// net.AddNode(id, node.OpSource()).
type nodeOpSource struct {
	n *Node
}

// OpSource returns an OpSource view of this node for MemNetwork.AddNode.
func (n *Node) OpSource() OpSource { return nodeOpSource{n: n} }

// LocalVV delegates to the underlying Store.
func (a nodeOpSource) LocalVV() VersionVector {
	return a.n.store.LocalVV()
}

// OpsSince delegates to the underlying Store.
func (a nodeOpSource) OpsSince(site string, fromSeq uint64, limit int) ([]Op, error) {
	return a.n.store.OpsSince(context.Background(), site, fromSeq, limit)
}

// ExchangeAcks merges a peer's acks and answers with this node's.
func (a nodeOpSource) ExchangeAcks(acks Acks) Acks {
	a.n.store.MergeAcks(acks)
	return a.n.store.Acks()
}

// SnapshotPage delegates to the underlying Store.
func (a nodeOpSource) SnapshotPage(part int, cursor []byte) (SnapshotPage, error) {
	return a.n.store.SnapshotPage(context.Background(), part, cursor)
}

// ReceiveOp delivers a pushed op into the underlying Store.
func (a nodeOpSource) ReceiveOp(from PeerID, op Op) {
	if err := a.n.store.Ingest(context.Background(), op, false); err != nil {
		log.Printf("replication: ReceiveOp from %s ingest: %v", from, err)
	}
}
