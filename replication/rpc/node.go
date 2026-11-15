// Package rpc is replication's wire format and a Transport over it. The
// messages are proto/replication/v1's; the functions here convert between
// them and replication's types; and a Node serves a process's replicated
// databases to its peers over ConnectRPC, and syncs each one with theirs.
//
//	n := rpc.NewNode(self, peers, http.DefaultClient)
//	mux.Handle(n.Handler())
//	db, err := replication.Open(ctx, "app.musq", replication.CRDT(), n.Sync("app"))
package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/replication"
	v1 "github.com/samyfodil/musql/replication/rpc/gen/replication/v1"
	"github.com/samyfodil/musql/replication/rpc/gen/replication/v1/replicationv1connect"
)

const (
	// maxOps is the most ops a GetOps answer carries.
	maxOps = 1024
	// imageChunk is the bytes of an image one message carries.
	imageChunk = 1 << 20
	// pushBuffer is how many pushed ops wait for the sync loop.
	pushBuffer = 64
)

// Peers is how a Node finds the others: which nodes there are, and the base
// URL each one serves its Handler under.
type Peers interface {
	Peers() []replication.PeerID
	URL(peer replication.PeerID) (string, bool)
}

// Node serves the databases a process replicates to its peers, and syncs each
// one with theirs: one Handler for all of them, and a Sync option for each.
type Node struct {
	self  replication.PeerID
	peers Peers
	http  connect.HTTPClient
	opts  []connect.ClientOption

	mu      sync.Mutex
	dbs     map[string]*transport
	clients map[string]replicationv1connect.ReplicationServiceClient
}

// NewNode is the node self, reaching its peers through client (nil is
// http.DefaultClient) with opts.
func NewNode(self replication.PeerID, peers Peers, client connect.HTTPClient, opts ...connect.ClientOption) *Node {
	if client == nil {
		client = http.DefaultClient
	}
	return &Node{self: self, peers: peers, http: client, opts: opts,
		dbs: map[string]*transport{}, clients: map[string]replicationv1connect.ReplicationServiceClient{}}
}

// Handler serves n's databases to its peers, under the path it returns.
func (n *Node) Handler(opts ...connect.HandlerOption) (string, http.Handler) {
	return replicationv1connect.NewReplicationServiceHandler(server{n}, opts...)
}

// Sync is replication.Open's option to sync the database over n, under
// database, the name its peers know it by. n serves it under that name until
// the *sql.DB is closed.
func (n *Node) Sync(database string) replication.Option {
	return replication.WithTransport(func(ctx context.Context, s *replication.Syncer) (replication.Transport, error) {
		t := &transport{n: n, db: database, srv: s.Store(), push: make(chan replication.PeerOp, pushBuffer), done: make(chan struct{})}
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := n.dbs[database]; ok {
			return nil, fmt.Errorf("rpc: database %q syncs over this node already", database)
		}
		n.dbs[database] = t
		return t, nil
	})
}

func (n *Node) database(name string) (*transport, error) {
	n.mu.Lock()
	t, ok := n.dbs[name]
	n.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("rpc: no database %q here", name))
	}
	return t, nil
}

func (n *Node) client(peer replication.PeerID) (replicationv1connect.ReplicationServiceClient, error) {
	url, ok := n.peers.URL(peer)
	if !ok {
		return nil, fmt.Errorf("rpc: no URL for peer %q", peer)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	c, ok := n.clients[url]
	if !ok {
		c = replicationv1connect.NewReplicationServiceClient(n.http, url, n.opts...)
		n.clients[url] = c
	}
	return c, nil
}

// transport is one database's replication.Transport over its Node.
type transport struct {
	n    *Node
	db   string
	srv  *replication.Store
	push chan replication.PeerOp
	done chan struct{}
	once sync.Once
}

func (t *transport) Peers() []replication.PeerID {
	var out []replication.PeerID
	for _, p := range t.n.peers.Peers() {
		if p != t.n.self {
			out = append(out, p)
		}
	}
	return out
}

func (t *transport) ExchangeVV(ctx context.Context, peer replication.PeerID, vv replication.VersionVector) (replication.VersionVector, error) {
	c, err := t.n.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.ExchangeVV(ctx, connect.NewRequest(&v1.ExchangeVVRequest{Database: t.db, Vv: VersionVectorToProto(vv)}))
	if err != nil {
		return nil, err
	}
	return VersionVectorFromProto(resp.Msg.GetVv()), nil
}

func (t *transport) GetOps(ctx context.Context, peer replication.PeerID, site string, fromSeq uint64, limit int) ([]replication.Op, error) {
	c, err := t.n.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.GetOps(ctx, connect.NewRequest(&v1.GetOpsRequest{Database: t.db, Site: site, FromSeq: fromSeq, Limit: uint32(min(max(limit, 0), math.MaxUint32))}))
	if err != nil {
		return nil, err
	}
	if resp.Msg.GetPruned() {
		return nil, replication.ErrPruned
	}
	return OpsFromProto(resp.Msg.GetOps())
}

func (t *transport) ExchangeAcks(ctx context.Context, peer replication.PeerID, acks replication.Acks) (replication.Acks, error) {
	c, err := t.n.client(peer)
	if err != nil {
		return nil, err
	}
	resp, err := c.ExchangeAcks(ctx, connect.NewRequest(&v1.ExchangeAcksRequest{Database: t.db, Acks: AcksToProto(acks)}))
	if err != nil {
		return nil, err
	}
	return AcksFromProto(resp.Msg.GetAcks()), nil
}

func (t *transport) GetSnapshot(ctx context.Context, peer replication.PeerID, part int, cursor []byte) (replication.SnapshotPage, error) {
	c, err := t.n.client(peer)
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	resp, err := c.GetSnapshot(ctx, connect.NewRequest(&v1.GetSnapshotRequest{Database: t.db, Part: uint32(part), Cursor: cursor}))
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	return SnapshotPageFromProto(resp.Msg.GetPage())
}

func (t *transport) pushTo(ctx context.Context, peer replication.PeerID, ops []replication.Op) error {
	c, err := t.n.client(peer)
	if err != nil {
		return err
	}
	_, err = c.PushOps(ctx, connect.NewRequest(&v1.PushOpsRequest{Database: t.db, From: string(t.n.self), Ops: OpsToProto(ops)}))
	return err
}

func (t *transport) PushOp(ctx context.Context, peer replication.PeerID, op replication.Op) error {
	return t.pushTo(ctx, peer, []replication.Op{op})
}

// PushOps sends a commit's ops to every peer at once (replication.Broadcaster).
func (t *transport) PushOps(ctx context.Context, ops []replication.Op) error {
	peers := t.Peers()
	errs := make([]error, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Go(func() { errs[i] = t.pushTo(ctx, p, ops) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (t *transport) SubscribePush(ctx context.Context) (<-chan replication.PeerOp, error) {
	out := make(chan replication.PeerOp, pushBuffer)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.done:
				return
			case op := <-t.push:
				select {
				case out <- op:
				case <-ctx.Done():
					return
				case <-t.done:
					return
				}
			}
		}
	}()
	return out, nil
}

func (t *transport) GetImage(ctx context.Context, peer replication.PeerID, req replication.ImageRequest) (replication.ImageManifest, io.ReadCloser, error) {
	c, err := t.n.client(peer)
	if err != nil {
		return replication.ImageManifest{}, nil, err
	}
	stream, err := c.GetImage(ctx, connect.NewRequest(&v1.GetImageRequest{Database: t.db, Request: ImageRequestToProto(req)}))
	if err != nil {
		return replication.ImageManifest{}, nil, err
	}
	fail := func(err error) (replication.ImageManifest, io.ReadCloser, error) {
		stream.Close()
		return replication.ImageManifest{}, nil, err
	}
	if !stream.Receive() {
		err := stream.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fail(fmt.Errorf("rpc: image from %s: %w", peer, err))
	}
	switch m := stream.Msg(); {
	case m.GetRefused() != "":
		return fail(fmt.Errorf("%w: %s: %s", replication.ErrImageRefused, peer, m.GetRefused()))
	case m.GetManifest() == nil:
		return fail(fmt.Errorf("rpc: image from %s: no manifest", peer))
	default:
		return ImageManifestFromProto(m.GetManifest()), &imageReader{stream: stream}, nil
	}
}

// Close stops serving the database (db.Close calls it).
func (t *transport) Close() error {
	t.once.Do(func() {
		t.n.mu.Lock()
		if t.n.dbs[t.db] == t {
			delete(t.n.dbs, t.db)
		}
		t.n.mu.Unlock()
		close(t.done)
	})
	return nil
}

// imageReader reads an image's bytes out of the messages after its manifest.
type imageReader struct {
	stream *connect.ServerStreamForClient[v1.GetImageResponse]
	buf    []byte
}

func (r *imageReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		d, ok := r.stream.Msg().GetMsg().(*v1.GetImageResponse_Data)
		if !ok {
			return 0, errors.New("rpc: an image message that is not its bytes")
		}
		r.buf = d.Data
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *imageReader) Close() error { return r.stream.Close() }

// server answers a Node's peers from its databases' Stores.
type server struct{ n *Node }

func (s server) ExchangeVV(_ context.Context, req *connect.Request[v1.ExchangeVVRequest]) (*connect.Response[v1.ExchangeVVResponse], error) {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ExchangeVVResponse{Vv: VersionVectorToProto(t.srv.LocalVV())}), nil
}

func (s server) GetOps(ctx context.Context, req *connect.Request[v1.GetOpsRequest]) (*connect.Response[v1.GetOpsResponse], error) {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > maxOps {
		limit = maxOps
	}
	ops, err := t.srv.OpsSince(ctx, req.Msg.GetSite(), req.Msg.GetFromSeq(), limit)
	if errors.Is(err, replication.ErrPruned) {
		return connect.NewResponse(&v1.GetOpsResponse{Pruned: true}), nil
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetOpsResponse{Ops: OpsToProto(ops)}), nil
}

func (s server) ExchangeAcks(_ context.Context, req *connect.Request[v1.ExchangeAcksRequest]) (*connect.Response[v1.ExchangeAcksResponse], error) {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return nil, err
	}
	t.srv.MergeAcks(AcksFromProto(req.Msg.GetAcks()))
	return connect.NewResponse(&v1.ExchangeAcksResponse{Acks: AcksToProto(t.srv.Acks())}), nil
}

func (s server) GetSnapshot(ctx context.Context, req *connect.Request[v1.GetSnapshotRequest]) (*connect.Response[v1.GetSnapshotResponse], error) {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return nil, err
	}
	page, err := t.srv.SnapshotPage(ctx, int(req.Msg.GetPart()), req.Msg.GetCursor())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetSnapshotResponse{Page: SnapshotPageToProto(page)}), nil
}

func (s server) PushOps(ctx context.Context, req *connect.Request[v1.PushOpsRequest]) (*connect.Response[v1.PushOpsResponse], error) {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return nil, err
	}
	ops, err := OpsFromProto(req.Msg.GetOps())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	from := replication.PeerID(req.Msg.GetFrom())
	for _, op := range ops {
		select {
		case t.push <- replication.PeerOp{From: from, Op: op}:
		case <-t.done:
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("rpc: database %q is closed", t.db))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return connect.NewResponse(&v1.PushOpsResponse{}), nil
}

func (s server) GetImage(ctx context.Context, req *connect.Request[v1.GetImageRequest], stream *connect.ServerStream[v1.GetImageResponse]) error {
	t, err := s.n.database(req.Msg.GetDatabase())
	if err != nil {
		return err
	}
	m, body, err := t.srv.Image(ctx, ImageRequestFromProto(req.Msg.GetRequest()))
	if errors.Is(err, replication.ErrImageRefused) {
		return stream.Send(&v1.GetImageResponse{Msg: &v1.GetImageResponse_Refused{Refused: err.Error()}})
	}
	if err != nil {
		return err
	}
	defer body.Close()
	if err := stream.Send(&v1.GetImageResponse{Msg: &v1.GetImageResponse_Manifest{Manifest: ImageManifestToProto(m)}}); err != nil {
		return err
	}
	buf := make([]byte, imageChunk)
	for {
		n, err := io.ReadFull(body, buf)
		if n > 0 {
			if serr := stream.Send(&v1.GetImageResponse{Msg: &v1.GetImageResponse_Data{Data: buf[:n]}}); serr != nil {
				return serr
			}
		}
		switch {
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			return nil
		case err != nil:
			return err
		}
	}
}
