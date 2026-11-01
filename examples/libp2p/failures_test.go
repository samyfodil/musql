package libp2p

import (
	"context"
	"errors"
	"testing"

	p2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"

	"github.com/samyfodil/musql/replication"
)

// failServer answers every pull with an error.
type failServer struct{ nopServer }

func (failServer) OpsSince(context.Context, string, uint64, int) ([]replication.Op, error) {
	return nil, errors.New("the disk is gone")
}

func (failServer) SnapshotPage(context.Context, int, []byte) (replication.SnapshotPage, error) {
	return replication.SnapshotPage{}, errors.New("the disk is gone")
}

// TestCallsToPeersThatFail: a pull the peer cannot answer, a peer that resets
// every stream, and a peer that is gone each come back as an error.
func TestCallsToPeersThatFail(t *testing.T) {
	ctx := context.Background()
	_, serverHost, client := pullPair(t, failServer{})
	id := replication.PeerID(serverHost.ID().String())
	if _, err := client.GetOps(ctx, id, "s", 0, 10); err == nil {
		t.Error("ops the peer could not read came back")
	}
	if _, err := client.GetSnapshot(ctx, id, replication.SnapshotOps, nil); err == nil {
		t.Error("a snapshot page the peer could not read came back")
	}

	reset := func(s network.Stream) { s.Reset() }
	serverHost.SetStreamHandler(client.proto, reset)
	serverHost.SetStreamHandler(client.image, reset)
	if _, err := client.ExchangeVV(ctx, id, replication.VersionVector{}); err == nil {
		t.Error("a peer that resets its streams answered")
	}
	if _, body, err := client.GetImage(ctx, id, replication.ImageRequest{}); err == nil {
		body.Close()
		t.Error("a peer that resets its streams sent an image")
	}

	gone, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	if _, err := client.ExchangeVV(ctx, replication.PeerID(gone.ID().String()), replication.VersionVector{}); err == nil {
		t.Error("a peer that is gone answered")
	}
}

// TestPullsAfterClose: a full page asks for no page ahead once the transport is
// closed, and a call asking for no limit takes a response's worth.
func TestPullsAfterClose(t *testing.T) {
	ctx := context.Background()
	ops := make([]replication.Op, 2*maxOpsPerResponse)
	for i := range ops {
		ops[i] = replication.Op{Site: "s", Seq: uint64(i + 1), Kind: replication.OpDelete, PK: []byte{1}}
	}
	srv := &pageServer{ops: ops}
	_, serverHost, client := pullPair(t, srv)
	id := replication.PeerID(serverHost.ID().String())
	if got, err := client.GetOps(ctx, id, "s", 0, 0); err != nil || len(got) != maxOpsPerResponse {
		t.Fatalf("no limit: %d ops, %v", len(got), err)
	}
	client.Close()
	if got, err := client.GetOps(ctx, id, "s", maxOpsPerResponse, maxOpsPerResponse); err != nil || len(got) != maxOpsPerResponse {
		t.Fatalf("after close: %d ops, %v", len(got), err)
	}
}

// TestPushes: an op and a commit's ops go out on the topic.
func TestPushes(t *testing.T) {
	ctx := context.Background()
	_, _, client := pullPair(t, nopServer{})
	op := replication.Op{Site: "s", Seq: 1, Kind: replication.OpDelete, PK: []byte{1}}
	if err := client.PushOp(ctx, "", op); err != nil {
		t.Fatal(err)
	}
	if err := client.PushOps(ctx, []replication.Op{op, op}); err != nil {
		t.Fatal(err)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if err := client.PushOps(done, []replication.Op{op}); err == nil {
		t.Error("a push with its context done went out")
	}
}
