// Package libp2p implements replication.Transport over libp2p: request/response
// (ExchangeVV, GetOps) over a dedicated libp2p stream protocol, and push
// (PushOp) over gossipsub. It is an example of a transport, not part of musql,
// and its own module, so nothing in musql pulls in libp2p.
package libp2p

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

// ProtocolID is the base of the libp2p stream protocol used for request/response
// calls (ExchangeVV, GetOps). Each database serves ProtocolID + "/" + its topic,
// so any number of them share a host, each answering only for itself.
const ProtocolID = protocol.ID("/musql/replication/1.0.0")

// Server is the read side of a replication.Store that Transport serves over
// the network on behalf of a local node. *replication.Store satisfies this.
type Server interface {
	// LocalVV returns the local version vector.
	LocalVV() replication.VersionVector

	// OpsSince returns ops from the given site with seq > fromSeq, capped at limit.
	OpsSince(ctx context.Context, site string, fromSeq uint64, limit int) ([]replication.Op, error)

	// SnapshotPage serves one page of part of the snapshot.
	SnapshotPage(ctx context.Context, part int, cursor []byte) (replication.SnapshotPage, error)

	// Acks is every ack the node knows; MergeAcks takes a peer's.
	Acks() replication.Acks
	MergeAcks(replication.Acks)
}

// Transport implements replication.Transport over a libp2p host: ExchangeVV and
// GetOps are request/response calls over ProtocolID; PushOp publishes to a
// gossipsub topic and SubscribePush is fed by a background reader of that
// topic's subscription.
type Transport struct {
	host  host.Host
	srv   Server
	proto protocol.ID
	ownPS bool // the router is this Transport's own (New was given nil)
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	stop  context.CancelFunc

	pushCh chan replication.PeerOp
}

var _ replication.Transport = (*Transport)(nil)

// New creates a Transport bound to host h, joining topicName on the pubsub
// router ps for PushOp/SubscribePush and registering a stream handler on h for
// ExchangeVV/GetOps requests, answered from srv (typically a *replication.Store). The
// background gossip reader runs until ctx is cancelled or Close.
//
// A nil ps makes a gossipsub router for this Transport alone, which lives until
// ctx is cancelled or Close. A host runs one router: a host that already has one
// -- the application's own, or another database's -- is refused, and its router
// is the ps to pass.
func New(ctx context.Context, h host.Host, ps *pubsub.PubSub, topicName string, srv Server) (*Transport, error) {
	ctx, stop := context.WithCancel(ctx)
	t := &Transport{
		host:   h,
		srv:    srv,
		proto:  ProtocolID + "/" + protocol.ID(topicName),
		ownPS:  ps == nil,
		stop:   stop,
		pushCh: make(chan replication.PeerOp, 64),
	}
	fail := func(err error) (*Transport, error) {
		stop()
		return nil, err
	}
	if t.ownPS {
		// A second router on one host takes over the first's gossip streams, and
		// the first's topics go quiet without an error.
		for _, id := range h.Mux().Protocols() {
			if slices.Contains(pubsub.GossipSubDefaultProtocols, id) {
				return fail(fmt.Errorf("examples/libp2p: host already runs a pubsub router (%s); pass it with SyncWith", id))
			}
		}
		var err error
		if ps, err = pubsub.NewGossipSub(ctx, h); err != nil {
			return fail(fmt.Errorf("examples/libp2p: new gossipsub: %w", err))
		}
	}
	topic, err := ps.Join(topicName)
	if err != nil {
		return fail(fmt.Errorf("examples/libp2p: join topic %q: %w", topicName, err))
	}
	if t.sub, err = topic.Subscribe(); err != nil {
		topic.Close()
		return fail(fmt.Errorf("examples/libp2p: subscribe topic %q: %w", topicName, err))
	}
	t.topic = topic

	h.SetStreamHandler(t.proto, t.handleStream)
	go t.readGossip(ctx)

	return t, nil
}

// Close removes this Transport's stream handler and leaves its topic, stopping
// its own router if it made one. Other databases on the host, and the host
// itself, are untouched.
func (t *Transport) Close() error {
	defer t.stop()
	t.host.RemoveStreamHandler(t.proto)
	t.sub.Cancel()
	if !t.ownPS {
		return t.topic.Close()
	}
	// The topic goes with the router. Its stream handlers stay on the host after
	// it stops, and would make the next New on h refuse it.
	//
	// ponytail: removes them by protocol id, so a router the application started
	// on h AFTER this one (which New could not refuse) loses its handlers too.
	for _, id := range pubsub.GossipSubDefaultProtocols {
		t.host.RemoveStreamHandler(id)
	}
	return nil
}

// Peers returns the peers this node currently sees subscribed to the
// gossipsub topic.
func (t *Transport) Peers() []replication.PeerID {
	peers := t.topic.ListPeers()
	out := make([]replication.PeerID, 0, len(peers))
	for _, p := range peers {
		out = append(out, replication.PeerID(p.String()))
	}
	return out
}

// Bounds on the request/response protocol. A peer's message is read only up to
// its MaxSize, so a peer cannot make this node allocate what it claims; a
// GetOps answer stops at maxResponse bytes (at least one op), and at
// maxOpsPerResponse ops whatever limit the caller asked for.
//
// ponytail: one op is sent whole, so a row bigger than maxResponse cannot sync;
// it is logged where it is served. Stream ops in chunks if rows get that big.
const (
	maxRequest        = 4 << 20
	maxResponse       = 64 << 20
	maxOpsPerResponse = 1024
	streamTimeout     = 30 * time.Second
)

// call sends req to peer on a fresh stream and reads its one Response.
func (t *Transport) call(ctx context.Context, peerID replication.PeerID, req *pb.Request) (*pb.Response, error) {
	pid, err := peer.Decode(string(peerID))
	if err != nil {
		return nil, fmt.Errorf("examples/libp2p: decode peer %q: %w", peerID, err)
	}
	s, err := t.host.NewStream(ctx, pid, t.proto)
	if err != nil {
		return nil, fmt.Errorf("examples/libp2p: new stream to %s: %w", peerID, err)
	}
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
		s.Reset()
		return nil, err
	}
	if _, err := protodelim.MarshalTo(s, req); err != nil {
		s.Reset()
		return nil, fmt.Errorf("examples/libp2p: write request to %s: %w", peerID, err)
	}
	var resp pb.Response
	if err := (protodelim.UnmarshalOptions{MaxSize: maxResponse}).UnmarshalFrom(bufio.NewReader(s), &resp); err != nil {
		s.Reset()
		return nil, fmt.Errorf("examples/libp2p: read response from %s: %w", peerID, err)
	}
	return &resp, nil
}

// ExchangeVV exchanges version vectors with peer.
func (t *Transport) ExchangeVV(ctx context.Context, peerID replication.PeerID, vv replication.VersionVector) (replication.VersionVector, error) {
	resp, err := t.call(ctx, peerID, &pb.Request{Msg: &pb.Request_ExchangeVv{ExchangeVv: &pb.ExchangeVVRequest{Vv: vvToPB(vv)}}})
	if err != nil {
		return nil, err
	}
	r := resp.GetExchangeVv()
	if r == nil {
		return nil, fmt.Errorf("examples/libp2p: %s answered ExchangeVV with %T", peerID, resp.GetMsg())
	}
	return vvFromPB(r.GetVv()), nil
}

// GetOps retrieves from peer site's ops after fromSeq, at most limit of them
// (the peer may send fewer).
func (t *Transport) GetOps(ctx context.Context, peerID replication.PeerID, site string, fromSeq uint64, limit int) ([]replication.Op, error) {
	resp, err := t.call(ctx, peerID, &pb.Request{Msg: &pb.Request_GetOps{GetOps: &pb.GetOpsRequest{Site: site, FromSeq: fromSeq, Limit: uint32(limit)}}})
	if err != nil {
		return nil, err
	}
	r := resp.GetGetOps()
	if r == nil {
		return nil, fmt.Errorf("examples/libp2p: %s answered GetOps with %T", peerID, resp.GetMsg())
	}
	if r.GetPruned() {
		return nil, replication.ErrPruned
	}
	return opsFromPB(r.GetOps())
}

// ExchangeAcks trades acks with peer.
func (t *Transport) ExchangeAcks(ctx context.Context, peerID replication.PeerID, acks replication.Acks) (replication.Acks, error) {
	resp, err := t.call(ctx, peerID, &pb.Request{Msg: &pb.Request_ExchangeAcks{ExchangeAcks: &pb.ExchangeAcksRequest{Acks: acksToPB(acks)}}})
	if err != nil {
		return nil, err
	}
	r := resp.GetExchangeAcks()
	if r == nil {
		return nil, fmt.Errorf("examples/libp2p: %s answered ExchangeAcks with %T", peerID, resp.GetMsg())
	}
	return acksFromPB(r.GetAcks()), nil
}

// GetSnapshot fetches one page of part of peer's snapshot.
func (t *Transport) GetSnapshot(ctx context.Context, peerID replication.PeerID, part int, cursor []byte) (replication.SnapshotPage, error) {
	resp, err := t.call(ctx, peerID, &pb.Request{Msg: &pb.Request_GetSnapshot{GetSnapshot: &pb.GetSnapshotRequest{Part: uint32(part), Cursor: cursor}}})
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	r := resp.GetGetSnapshot()
	if r == nil {
		return replication.SnapshotPage{}, fmt.Errorf("examples/libp2p: %s answered GetSnapshot with %T", peerID, resp.GetMsg())
	}
	return snapshotFromPB(r)
}

// PushOp publishes op to the gossipsub topic. The peer argument is ignored:
// gossipsub delivery is topic-wide, not point-to-point.
func (t *Transport) PushOp(ctx context.Context, _ replication.PeerID, op replication.Op) error {
	msg := &pb.PushMessage{Op: opToPB(op)}
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("examples/libp2p: marshal PushMessage: %w", err)
	}
	if err := t.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("examples/libp2p: publish: %w", err)
	}
	return nil
}

// SubscribePush returns a channel fed by the gossipsub topic reader. The
// channel is closed when ctx is cancelled or the underlying subscription
// ends.
func (t *Transport) SubscribePush(ctx context.Context) (<-chan replication.PeerOp, error) {
	out := make(chan replication.PeerOp, 64)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case po, ok := <-t.pushCh:
				if !ok {
					return
				}
				select {
				case out <- po:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// handleStream answers the one Request read from s, then closes the stream.
func (t *Transport) handleStream(s network.Stream) {
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
		s.Reset()
		return
	}
	var req pb.Request
	if err := (protodelim.UnmarshalOptions{MaxSize: maxRequest}).UnmarshalFrom(bufio.NewReader(s), &req); err != nil {
		s.Reset()
		return
	}
	var resp pb.Response
	switch m := req.GetMsg().(type) {
	case *pb.Request_ExchangeVv:
		resp.Msg = &pb.Response_ExchangeVv{ExchangeVv: &pb.ExchangeVVResponse{Vv: vvToPB(t.srv.LocalVV())}}
	case *pb.Request_GetOps:
		limit := int(m.GetOps.GetLimit())
		if limit <= 0 || limit > maxOpsPerResponse {
			limit = maxOpsPerResponse
		}
		ops, err := t.srv.OpsSince(context.Background(), m.GetOps.GetSite(), m.GetOps.GetFromSeq(), limit)
		if errors.Is(err, replication.ErrPruned) {
			resp.Msg = &pb.Response_GetOps{GetOps: &pb.GetOpsResponse{Pruned: true}}
			break
		}
		if err != nil {
			s.Reset()
			return
		}
		out := &pb.GetOpsResponse{}
		size := 0
		for _, op := range opsToPB(ops) {
			n := proto.Size(op) + binary.MaxVarintLen64 + 1 // the field's tag and length
			if size+n > maxResponse-binary.MaxVarintLen64 {
				if len(out.Ops) == 0 {
					log.Printf("examples/libp2p: op %s/%d is %d bytes, over the %d a response carries; it cannot sync", op.GetSite(), op.GetSeq(), n, maxResponse)
					s.Reset()
					return
				}
				break
			}
			size += n
			out.Ops = append(out.Ops, op)
		}
		resp.Msg = &pb.Response_GetOps{GetOps: out}
	case *pb.Request_ExchangeAcks:
		t.srv.MergeAcks(acksFromPB(m.ExchangeAcks.GetAcks()))
		resp.Msg = &pb.Response_ExchangeAcks{ExchangeAcks: &pb.ExchangeAcksResponse{Acks: acksToPB(t.srv.Acks())}}
	case *pb.Request_GetSnapshot:
		page, err := t.srv.SnapshotPage(context.Background(), int(m.GetSnapshot.GetPart()), m.GetSnapshot.GetCursor())
		if err != nil {
			s.Reset()
			return
		}
		resp.Msg = &pb.Response_GetSnapshot{GetSnapshot: snapshotToPB(page)}
	default:
		s.Reset()
		return
	}
	if _, err := protodelim.MarshalTo(s, &resp); err != nil {
		s.Reset()
	}
}

// readGossip runs until ctx is cancelled, feeding decoded PushMessages from
// the gossipsub subscription into pushCh. Messages originating from this
// host are skipped.
func (t *Transport) readGossip(ctx context.Context) {
	defer close(t.pushCh)
	selfID := t.host.ID()
	for {
		msg, err := t.sub.Next(ctx)
		if err != nil {
			return // ctx cancelled or subscription closed
		}
		if msg.ReceivedFrom == selfID {
			continue // skip our own publishes
		}
		var pm pb.PushMessage
		if err := proto.Unmarshal(msg.Data, &pm); err != nil {
			continue
		}
		op, err := opFromPB(pm.GetOp())
		if err != nil {
			log.Printf("examples/libp2p: pushed op from %s: %v", msg.ReceivedFrom, err)
			continue
		}
		po := replication.PeerOp{From: replication.PeerID(msg.ReceivedFrom.String()), Op: op}
		select {
		case t.pushCh <- po:
		case <-ctx.Done():
			return
		}
	}
}

// Sync is the replication.Open option that syncs the database over libp2p: peers of h
// that joined the same topic receive every write, and anti-entropy catches up
// the ones that were away. It starts a gossipsub router of its own on h, which
// db.Close stops; a host that runs one already needs SyncWith.
//
//	db, err := replication.Open(ctx, "app.db", replication.CRDT(), repllibp2p.Sync(host, "my-app"))
func Sync(h host.Host, topic string) replication.Option { return SyncWith(h, nil, topic) }

// SyncWith is Sync over the application's own pubsub router ps, which is how
// one host carries several databases: one router, a topic each. db.Close leaves
// the topic and keeps ps running.
func SyncWith(h host.Host, ps *pubsub.PubSub, topic string) replication.Option {
	return replication.WithTransport(func(ctx context.Context, s *replication.Syncer) (replication.Transport, error) {
		return New(ctx, h, ps, topic, s.Store())
	})
}
