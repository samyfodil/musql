package libp2p

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	p2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

type nopServer struct{}

func (nopServer) LocalVV() replication.VersionVector { return replication.VersionVector{} }
func (nopServer) OpsSince(context.Context, string, uint64, int) ([]replication.Op, error) {
	return nil, nil
}
func (nopServer) SnapshotPage(context.Context, int, []byte) (replication.SnapshotPage, error) {
	return replication.SnapshotPage{}, nil
}
func (nopServer) Acks() replication.Acks     { return nil }
func (nopServer) MergeAcks(replication.Acks) {}

// TestRequestOverMaxSizeIsRefused: a request whose length prefix is over
// maxRequest is refused at once, before the server reads or allocates it.
func TestRequestOverMaxSizeIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cli, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	tr, err := New(ctx, srv, nil, "bounds", nopServer{})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if err := cli.Connect(ctx, peer.AddrInfo{ID: srv.ID(), Addrs: srv.Addrs()}); err != nil {
		t.Fatal(err)
	}
	s, err := cli.NewStream(ctx, srv.ID(), tr.proto)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(5 * time.Second))
	// The server may reset the stream before the write lands; either side
	// seeing the reset, at once and with no answer, is the refusal.
	start := time.Now()
	_, werr := s.Write(append(binary.AppendUvarint(nil, maxRequest+1), make([]byte, 64)...))
	b, rerr := io.ReadAll(s)
	if werr == nil && rerr == nil || len(b) > 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("got %d bytes, write err %v, read err %v after %v; want the stream reset at once", len(b), werr, rerr, time.Since(start))
	}
}

// TestMalformedOpIsRefused: an op no replication node wrote -- a kind or a cell type out
// of range -- is refused rather than read as something else.
func TestMalformedOpIsRefused(t *testing.T) {
	good := &pb.Op{Site: "s", Seq: 1, Kind: uint32(replication.OpSchema), Cells: []*pb.Cell{{Col: "c", Type: uint32(replication.TypeBlob)}}}
	if _, err := opFromPB(good); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]*pb.Op{
		"kind":      {Site: "s", Seq: 1, Kind: uint32(replication.OpSchema) + 1},
		"cell type": {Site: "s", Seq: 1, Cells: []*pb.Cell{{Col: "c", Type: uint32(replication.TypeBlob) + 1}}},
		"no site":   {Seq: 1},
		"seq 0":     {Site: "s"},
	} {
		if _, err := opFromPB(op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
