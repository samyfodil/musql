package libp2p_test

import (
	"context"
	"testing"

	"github.com/samyfodil/musql/engine"
	replp2p "github.com/samyfodil/musql/examples/libp2p"
	"github.com/samyfodil/musql/replication"
)

// noImages is a Server that is no ImageServer.
type noImages struct{}

func (noImages) LocalVV() replication.VersionVector { return replication.VersionVector{} }
func (noImages) OpsSince(context.Context, string, uint64, int) ([]replication.Op, error) {
	return nil, nil
}
func (noImages) SnapshotPage(context.Context, int, []byte) (replication.SnapshotPage, error) {
	return replication.SnapshotPage{}, nil
}
func (noImages) Acks() replication.Acks     { return nil }
func (noImages) MergeAcks(replication.Acks) {}

// TestGetImageRefusals: an image is refused by a node that serves none and by
// one asked for a layout it does not have, and cannot be asked of a peer ID
// that does not decode or a host that does not speak the protocol.
func TestGetImageRefusals(t *testing.T) {
	ctx := context.Background()
	hA, hB, hC := newHost(t), newHost(t), newHost(t)
	connectHosts(t, hB, hA)
	connectHosts(t, hB, hC)
	ta, err := replp2p.New(ctx, hA, nil, "refuse", noImages{})
	if err != nil {
		t.Fatal(err)
	}
	defer ta.Close()
	tb, err := replp2p.New(ctx, hB, nil, "refuse", noImages{})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	file, minFile, delta := engine.FormatVersions()
	req := replication.ImageRequest{Layout: "2", FileVersion: file, MinFileVersion: minFile, DeltaVersion: delta}
	if _, body, err := tb.GetImage(ctx, replication.PeerID(hA.ID().String()), req); err == nil {
		body.Close()
		t.Error("a node that serves no image sent one")
	}
	if _, _, err := tb.GetImage(ctx, replication.PeerID("not a peer id"), req); err == nil {
		t.Error("an undecodable peer ID was asked")
	}
	if _, _, err := tb.GetImage(ctx, replication.PeerID(hC.ID().String()), req); err == nil {
		t.Error("a host without the protocol answered")
	}

	hD, hE := newHost(t), newHost(t)
	connectHosts(t, hE, hD)
	d := openUsers(t, hD, "d", "refuse-d")
	defer d.Close()
	tb2, err := replp2p.New(ctx, hE, nil, "refuse-d", noImages{})
	if err != nil {
		t.Fatal(err)
	}
	defer tb2.Close()
	other := req
	other.Layout = "no such layout"
	if _, body, err := tb2.GetImage(ctx, replication.PeerID(hD.ID().String()), other); err == nil {
		body.Close()
		t.Error("an image of another layout was sent")
	}

	// A reader closed before the image's end resets the stream.
	_, body, err := tb2.GetImage(ctx, replication.PeerID(hD.ID().String()), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}
