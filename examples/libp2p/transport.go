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
	"io"
	"log"
	"slices"
	"sync"
	"time"

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

// ImageProtocolID is the base of the stream protocol an image is sent over
// (replication.Imager), per database as ProtocolID is: a stream of its own,
// since an image is many messages where a call is one.
const ImageProtocolID = protocol.ID("/musql/replication-image/1.0.0")

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

// ImageServer is a Server that sends its database's image to a node that holds
// nothing yet. *replication.Store is one.
type ImageServer interface {
	Image(ctx context.Context, req replication.ImageRequest) (replication.ImageManifest, io.ReadCloser, error)
}

// Transport implements replication.Transport over a libp2p host: ExchangeVV and
// GetOps are request/response calls over ProtocolID; PushOp publishes to a
// gossipsub topic and SubscribePush is fed by a background reader of that
// topic's subscription.
type Transport struct {
	host  host.Host
	srv   Server
	proto protocol.ID
	image protocol.ID
	ownPS bool // the router is this Transport's own (New was given nil)
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	stop  context.CancelFunc
	ctx   context.Context // done once Close is called

	pushCh chan replication.PeerOp

	mu       sync.Mutex
	closed   bool
	idle     map[peer.ID][]*idleStream
	ahead    map[aheadKey]*aheadPage
	fetching sync.WaitGroup
}

var (
	_ replication.Transport = (*Transport)(nil)
	_ replication.Imager    = (*Transport)(nil)
)

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
		image:  ImageProtocolID + "/" + protocol.ID(topicName),
		ownPS:  ps == nil,
		stop:   stop,
		ctx:    ctx,
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
	h.SetStreamHandler(t.image, t.handleImage)
	go t.readGossip(ctx)

	return t, nil
}

// Close removes this Transport's stream handler and leaves its topic, stopping
// its own router if it made one. Other databases on the host, and the host
// itself, are untouched.
func (t *Transport) Close() error {
	defer t.closeIdle()
	defer t.fetching.Wait()
	defer t.stop()
	t.host.RemoveStreamHandler(t.proto)
	t.host.RemoveStreamHandler(t.image)
	t.sub.Cancel()
	if !t.ownPS {
		return t.topic.Close()
	}
	// The topic goes with the router. Its stream handlers stay on the host after
	// it stops, and would make the next New on h refuse it.
	//
	// A deliberate simplification: removes them by protocol id, so a router the application started
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
// A deliberate simplification: one op is sent whole, so a row bigger than maxResponse cannot sync;
// it is logged where it is served. Stream ops in chunks if rows get that big.
const (
	maxRequest        = 4 << 20
	maxResponse       = 64 << 20
	maxOpsPerResponse = 1024
	streamTimeout     = 30 * time.Second
)

// call sends req to peer and reads its one Response, on a stream an earlier
// call left idle when there is one. A peer that answers one request a stream
// closed that since, and the request goes again on a fresh stream; every
// request is safe to send twice.
func (t *Transport) call(ctx context.Context, peerID replication.PeerID, req *pb.Request) (*pb.Response, error) {
	pid, err := peer.Decode(string(peerID))
	if err != nil {
		return nil, fmt.Errorf("examples/libp2p: decode peer %q: %w", peerID, err)
	}
	for {
		is := t.takeIdle(pid)
		reused := is != nil
		if !reused {
			s, err := t.host.NewStream(ctx, pid, t.proto)
			if err != nil {
				return nil, fmt.Errorf("examples/libp2p: new stream to %s: %w", peerID, err)
			}
			is = &idleStream{s: s, r: bufio.NewReader(s)}
		}
		resp, err := is.roundTrip(req)
		if err == nil {
			t.putIdle(pid, is)
			return resp, nil
		}
		is.s.Reset()
		if !reused || ctx.Err() != nil {
			return nil, fmt.Errorf("examples/libp2p: %s: %w", peerID, err)
		}
	}
}

// idleStream is a stream to a peer, which answers requests on it in turn
// until either side closes it (handleStream).
type idleStream struct {
	s     network.Stream
	r     *bufio.Reader
	since time.Time
}

// streamIdle is how long a stream may sit idle and still take a request: well
// under the streamTimeout after which the peer stops waiting for one; maxIdle
// is how many idle streams to a peer are kept.
const (
	streamIdle = streamTimeout / 2
	maxIdle    = 2
)

func (is *idleStream) roundTrip(req *pb.Request) (*pb.Response, error) {
	if err := is.s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
		return nil, err
	}
	if err := writeMsg(is.s, req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	var resp pb.Response
	if err := readOwnedMsg(is.r, &resp, maxResponse); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &resp, nil
}

func (t *Transport) takeIdle(pid peer.ID) *idleStream {
	t.mu.Lock()
	defer t.mu.Unlock()
	for l := t.idle[pid]; len(l) > 0; l = t.idle[pid] {
		is := l[len(l)-1]
		t.idle[pid] = l[:len(l)-1]
		if time.Since(is.since) < streamIdle {
			return is
		}
		is.s.Reset()
	}
	return nil
}

func (t *Transport) putIdle(pid peer.ID, is *idleStream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || len(t.idle[pid]) >= maxIdle {
		is.s.Close()
		return
	}
	if t.idle == nil {
		t.idle = map[peer.ID][]*idleStream{}
	}
	is.since = time.Now()
	t.idle[pid] = append(t.idle[pid], is)
}

// closeIdle closes the idle streams, and every stream a call finishes with
// from now on.
func (t *Transport) closeIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, l := range t.idle {
		for _, is := range l {
			is.s.Close()
		}
	}
	t.idle = nil
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
	return vvFromPB(r.GetVv(), intern{}), nil
}

// GetOps retrieves from peer site's ops after fromSeq, at most limit of them
// (the peer may send fewer). Once it answers a full page, it asks for the next
// one while the caller applies this one, and the next call that starts where
// this page ends takes it.
func (t *Transport) GetOps(ctx context.Context, peerID replication.PeerID, site string, fromSeq uint64, limit int) ([]replication.Op, error) {
	k := aheadKey{peerID, site}
	ops, ok := t.takeAhead(ctx, k, fromSeq, limit)
	var err error
	if !ok {
		ops, err = t.getOps(ctx, peerID, site, fromSeq, limit)
	}
	full := limit
	if full <= 0 || full > maxOpsPerResponse {
		full = maxOpsPerResponse
	}
	if err == nil && len(ops) >= full {
		t.fetchAhead(k, ops[len(ops)-1].Seq, limit)
	}
	return ops, err
}

// aheadKey names the page GetOps fetched ahead for a peer and a site.
type aheadKey struct {
	peer replication.PeerID
	site string
}

type aheadPage struct {
	from  uint64
	limit int
	done  chan struct{}
	ops   []replication.Op
	err   error
}

// takeAhead is the page fetched ahead for k when it starts at from, once it
// has arrived; ok is false when there is none, or its fetch failed.
func (t *Transport) takeAhead(ctx context.Context, k aheadKey, from uint64, limit int) (ops []replication.Op, ok bool) {
	t.mu.Lock()
	p := t.ahead[k]
	delete(t.ahead, k)
	t.mu.Unlock()
	if p == nil || p.from != from || p.limit != limit {
		return nil, false
	}
	select {
	case <-p.done:
		return p.ops, p.err == nil
	case <-ctx.Done():
		return nil, false
	}
}

func (t *Transport) fetchAhead(k aheadKey, from uint64, limit int) {
	p := &aheadPage{from: from, limit: limit, done: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.ahead == nil {
		t.ahead = map[aheadKey]*aheadPage{}
	}
	t.ahead[k] = p
	t.fetching.Go(func() {
		defer close(p.done)
		p.ops, p.err = t.getOps(t.ctx, k.peer, k.site, from, limit)
	})
}

func (t *Transport) getOps(ctx context.Context, peerID replication.PeerID, site string, fromSeq uint64, limit int) ([]replication.Op, error) {
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
	return opsFromPB(r.GetOps(), intern{})
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
	return acksFromPB(r.GetAcks(), intern{}), nil
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

// imageChunk is how many bytes of an image one ImageChunk carries.
const imageChunk = 1 << 20

// GetImage fetches peer's image: the manifest now, the bytes as the returned
// reader is read. Closing the reader ends the stream.
func (t *Transport) GetImage(ctx context.Context, peerID replication.PeerID, req replication.ImageRequest) (replication.ImageManifest, io.ReadCloser, error) {
	pid, err := peer.Decode(string(peerID))
	if err != nil {
		return replication.ImageManifest{}, nil, fmt.Errorf("examples/libp2p: decode peer %q: %w", peerID, err)
	}
	s, err := t.host.NewStream(ctx, pid, t.image)
	if err != nil {
		return replication.ImageManifest{}, nil, fmt.Errorf("examples/libp2p: new image stream to %s: %w", peerID, err)
	}
	fail := func(err error) (replication.ImageManifest, io.ReadCloser, error) {
		s.Reset()
		return replication.ImageManifest{}, nil, err
	}
	if err := s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
		return fail(err)
	}
	if err := writeMsg(s, &pb.GetImageRequest{Layout: req.Layout, FileVersion: req.FileVersion,
		MinFileVersion: req.MinFileVersion, DeltaVersion: req.DeltaVersion}); err != nil {
		return fail(fmt.Errorf("examples/libp2p: write image request to %s: %w", peerID, err))
	}
	r := bufio.NewReader(s)
	var resp pb.GetImageResponse
	if err := readMsg(r, &resp, maxResponse); err != nil {
		return fail(fmt.Errorf("examples/libp2p: read image manifest from %s: %w", peerID, err))
	}
	if why := resp.GetRefused(); why != "" || resp.GetManifest() == nil {
		s.Close()
		return replication.ImageManifest{}, nil, fmt.Errorf("%w: %s: %s", replication.ErrImageRefused, peerID, why)
	}
	return manifestFromPB(resp.GetManifest()), &imageReader{s: s, r: r}, nil
}

// imageReader reads an image's bytes out of the ImageChunks on its stream.
type imageReader struct {
	s    network.Stream
	r    *bufio.Reader
	buf  []byte
	done bool // the last chunk is read
}

func (r *imageReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.s.SetReadDeadline(time.Now().Add(streamTimeout)); err != nil {
			return 0, err
		}
		var c pb.ImageChunk
		if err := readOwnedMsg(r.r, &c, 2*imageChunk); err != nil {
			return 0, fmt.Errorf("examples/libp2p: read image chunk: %w", err)
		}
		r.buf, r.done = c.GetData(), c.GetLast()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *imageReader) Close() error {
	if !r.done || len(r.buf) > 0 {
		return r.s.Reset()
	}
	return r.s.Close()
}

// handleImage answers the GetImageRequest read from s with the image of the
// database t serves, in ImageChunks.
func (t *Transport) handleImage(s network.Stream) {
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
		s.Reset()
		return
	}
	var req pb.GetImageRequest
	if err := readMsg(bufio.NewReader(s), &req, maxRequest); err != nil {
		s.Reset()
		return
	}
	is, ok := t.srv.(ImageServer)
	if !ok {
		writeMsg(s, &pb.GetImageResponse{Refused: "this node serves no image"})
		return
	}
	m, body, err := is.Image(context.Background(), replication.ImageRequest{Layout: req.GetLayout(), FileVersion: req.GetFileVersion(),
		MinFileVersion: req.GetMinFileVersion(), DeltaVersion: req.GetDeltaVersion()})
	if err != nil {
		writeMsg(s, &pb.GetImageResponse{Refused: err.Error()})
		return
	}
	defer body.Close()
	if err := writeMsg(s, &pb.GetImageResponse{Manifest: manifestToPB(m)}); err != nil {
		s.Reset()
		return
	}
	buf := make([]byte, imageChunk)
	for {
		n, err := io.ReadFull(body, buf)
		last := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !last {
			s.Reset()
			return
		}
		if err := s.SetWriteDeadline(time.Now().Add(streamTimeout)); err != nil {
			s.Reset()
			return
		}
		if err := writeMsg(s, &pb.ImageChunk{Data: buf[:n], Last: last}); err != nil {
			s.Reset()
			return
		}
		if last {
			return
		}
	}
}

// PushOp publishes op to the gossipsub topic. The peer argument is ignored:
// gossipsub delivery is topic-wide, not point-to-point.
func (t *Transport) PushOp(ctx context.Context, _ replication.PeerID, op replication.Op) error {
	return t.publish(ctx, &pb.PushMessage{Op: opsToPB([]replication.Op{op})[0]})
}

var _ replication.Broadcaster = (*Transport)(nil)

// maxPushBytes caps the ops of one gossip message, below gossipsub's 1MiB
// default message limit.
const maxPushBytes = 512 << 10

// PushOps publishes a commit's ops to the gossipsub topic, as few messages as
// fit them, once for every peer. A message of one op carries it in op, which
// a peer that predates ops reads too.
func (t *Transport) PushOps(ctx context.Context, ops []replication.Op) error {
	var errs []error
	msg, size := &pb.PushMessage{}, 0
	flush := func() {
		if len(msg.Ops) == 1 {
			msg.Op, msg.Ops = msg.Ops[0], nil
		}
		if err := t.publish(ctx, msg); err != nil {
			errs = append(errs, err)
		}
		msg, size = &pb.PushMessage{}, 0
	}
	for _, op := range opsToPB(ops) {
		n := op.SizeVT()
		if len(msg.Ops) > 0 && size+n > maxPushBytes {
			flush()
		}
		msg.Ops = append(msg.Ops, op)
		size += n
	}
	if len(msg.Ops) > 0 {
		flush()
	}
	return errors.Join(errs...)
}

func (t *Transport) publish(ctx context.Context, msg *pb.PushMessage) error {
	data, err := msg.MarshalVT()
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

// handleStream answers the Requests read from s in turn, until the caller
// closes it or sends none for streamTimeout.
func (t *Transport) handleStream(s network.Stream) {
	defer s.Close()
	r := bufio.NewReader(s)
	for {
		if err := s.SetDeadline(time.Now().Add(streamTimeout)); err != nil {
			s.Reset()
			return
		}
		var req pb.Request
		if err := readMsg(r, &req, maxRequest); err != nil {
			if !errors.Is(err, io.EOF) {
				s.Reset()
			}
			return
		}
		resp := t.answer(&req)
		if resp == nil {
			s.Reset()
			return
		}
		if err := writeMsg(s, resp); err != nil {
			s.Reset()
			return
		}
	}
}

// answer is the Response to req, nil for a request it cannot answer.
func (t *Transport) answer(req *pb.Request) *pb.Response {
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
			return nil
		}
		out := &pb.GetOpsResponse{}
		size := 0
		for _, op := range opsToPB(ops) {
			n := op.SizeVT() + binary.MaxVarintLen64 + 1 // the field's tag and length
			if size+n > maxResponse-binary.MaxVarintLen64 {
				if len(out.Ops) == 0 {
					log.Printf("examples/libp2p: op %s/%d is %d bytes, over the %d a response carries; it cannot sync", op.GetSite(), op.GetSeq(), n, maxResponse)
					return nil
				}
				break
			}
			size += n
			out.Ops = append(out.Ops, op)
		}
		resp.Msg = &pb.Response_GetOps{GetOps: out}
	case *pb.Request_ExchangeAcks:
		t.srv.MergeAcks(acksFromPB(m.ExchangeAcks.GetAcks(), intern{}))
		resp.Msg = &pb.Response_ExchangeAcks{ExchangeAcks: &pb.ExchangeAcksResponse{Acks: acksToPB(t.srv.Acks())}}
	case *pb.Request_GetSnapshot:
		page, err := t.srv.SnapshotPage(context.Background(), int(m.GetSnapshot.GetPart()), m.GetSnapshot.GetCursor())
		if err != nil {
			return nil
		}
		resp.Msg = &pb.Response_GetSnapshot{GetSnapshot: snapshotToPB(page)}
	default:
		return nil
	}
	return &resp
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
		// msg.Data is the message's own, never written again: the ops
		// point into it rather than copy it.
		var pm pb.PushMessage
		if err := pm.UnmarshalVTUnsafe(msg.Data); err != nil {
			continue
		}
		ops := pm.GetOps()
		if pm.GetOp() != nil {
			ops = append([]*pb.Op{pm.GetOp()}, ops...)
		}
		d := newOpDecoder(ops, intern{})
		for _, o := range ops {
			op, err := d.op(o)
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
}

// Sync is the replication.Open option that syncs the database over libp2p: peers of h
// that joined the same topic receive every write, and anti-entropy catches up
// the ones that were away. It starts a gossipsub router of its own on h, which
// db.Close stops; a host that runs one already needs SyncWith.
//
//	db, err := replication.Open(ctx, "app.musq", replication.CRDT(), repllibp2p.Sync(host, "my-app"))
func Sync(h host.Host, topic string) replication.Option { return SyncWith(h, nil, topic) }

// SyncWith is Sync over the application's own pubsub router ps, which is how
// one host carries several databases: one router, a topic each. db.Close leaves
// the topic and keeps ps running.
func SyncWith(h host.Host, ps *pubsub.PubSub, topic string) replication.Option {
	return replication.WithTransport(func(ctx context.Context, s *replication.Syncer) (replication.Transport, error) {
		return New(ctx, h, ps, topic, s.Store())
	})
}
