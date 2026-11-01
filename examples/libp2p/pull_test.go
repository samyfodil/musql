package libp2p

import (
	"bufio"
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/encoding/protodelim"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

// pageServer serves one site's ops 1..len(ops) and notes where each page it
// was asked for starts.
type pageServer struct {
	nopServer
	ops   []replication.Op
	mu    sync.Mutex
	froms []uint64
}

func (p *pageServer) OpsSince(_ context.Context, _ string, from uint64, limit int) ([]replication.Op, error) {
	p.mu.Lock()
	p.froms = append(p.froms, from)
	p.mu.Unlock()
	from = min(from, uint64(len(p.ops)))
	return p.ops[from:min(from+uint64(limit), uint64(len(p.ops)))], nil
}

func (p *pageServer) asked() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.froms)
}

// pullPair is a transport serving srv and a client host connected to it,
// with a transport of its own.
func pullPair(t *testing.T, srv Server) (server *Transport, serverHost host.Host, client *Transport) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var hs [2]host.Host
	for i := range hs {
		h, err := p2p.New(p2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		hs[i] = h
	}
	var trs [2]*Transport
	for i, s := range []Server{srv, nopServer{}} {
		tr, err := New(ctx, hs[i], nil, "pull", s)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tr.Close() })
		trs[i] = tr
	}
	if err := hs[1].Connect(ctx, peer.AddrInfo{ID: hs[0].ID(), Addrs: hs[0].Addrs()}); err != nil {
		t.Fatal(err)
	}
	return trs[0], hs[0], trs[1]
}

// TestGetOpsTakesThePageFetchedAhead: after a full page, GetOps asks for the
// next one before it is called for it, and the call that starts there takes
// that page; a call that starts elsewhere asks for its own.
func TestGetOpsTakesThePageFetchedAhead(t *testing.T) {
	ctx := context.Background()
	srv := &pageServer{}
	for i := range 2*maxOpsPerResponse + 10 {
		srv.ops = append(srv.ops, replication.Op{Site: "s", Seq: uint64(i + 1), HLC: replication.HLC(i + 1), Tbl: "t",
			PK: replication.EncodePK(int64(i)), Kind: replication.OpInsert, Cells: []replication.Cell{replication.CellFromValue("c", int64(i))}})
	}
	_, serverHost, client := pullPair(t, srv)
	id := replication.PeerID(serverHost.ID().String())
	check := func(from uint64) {
		t.Helper()
		ops, err := client.GetOps(ctx, id, "s", from, maxOpsPerResponse)
		if err != nil {
			t.Fatal(err)
		}
		want := srv.ops[from:min(from+maxOpsPerResponse, uint64(len(srv.ops)))]
		if len(ops) != len(want) || len(ops) > 0 && (ops[0].Seq != want[0].Seq || ops[len(ops)-1].Seq != want[len(want)-1].Seq) {
			t.Fatalf("from %d: %d ops, want %d", from, len(ops), len(want))
		}
	}
	waitAsked := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); len(srv.asked()) < n; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("pages asked for from %v, want %d of them", srv.asked(), n)
			}
		}
	}
	check(0)
	waitAsked(2) // the next page, before a call asks for it
	check(5)     // not where that page starts: asked for, and the next one ahead
	waitAsked(4)
	check(maxOpsPerResponse) // nor is this
	waitAsked(6)
	check(2 * maxOpsPerResponse) // the page fetched ahead: no request
	check(2*maxOpsPerResponse + 10)
	want := []uint64{0, maxOpsPerResponse, 5, maxOpsPerResponse + 5, maxOpsPerResponse, 2 * maxOpsPerResponse, 2*maxOpsPerResponse + 10}
	if got := srv.asked(); !slices.Equal(got, want) {
		t.Fatalf("pages asked for from %v, want %v", got, want)
	}
}

// TestCallsShareAStream: calls to one peer go on one stream in turn, and still
// go through to a peer that answers one request on a stream, as builds before
// this one did.
func TestCallsShareAStream(t *testing.T) {
	ctx := context.Background()
	server, serverHost, client := pullPair(t, nopServer{})
	id := replication.PeerID(serverHost.ID().String())
	var streams atomic.Int32
	serverHost.SetStreamHandler(server.proto, func(s network.Stream) {
		streams.Add(1)
		server.handleStream(s)
	})
	for range 3 {
		if _, err := client.ExchangeVV(ctx, id, replication.VersionVector{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := streams.Load(); n != 1 {
		t.Fatalf("3 calls took %d streams, want 1", n)
	}
	server, serverHost, client = pullPair(t, nopServer{})
	id = replication.PeerID(serverHost.ID().String())
	serverHost.SetStreamHandler(server.proto, func(s network.Stream) {
		defer s.Close()
		var req pb.Request
		if err := (protodelim.UnmarshalOptions{MaxSize: maxRequest}).UnmarshalFrom(bufio.NewReader(s), &req); err != nil {
			s.Reset()
			return
		}
		protodelim.MarshalTo(s, server.answer(&req))
	})
	for range 3 {
		if _, err := client.ExchangeVV(ctx, id, replication.VersionVector{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIdleStreamsAreCappedAndAged: calls that ran at once leave at most
// maxIdle of their streams to reuse, and a stream idle too long is not reused.
func TestIdleStreamsAreCappedAndAged(t *testing.T) {
	ctx := context.Background()
	server, serverHost, client := pullPair(t, nopServer{})
	id := replication.PeerID(serverHost.ID().String())
	const calls = maxIdle + 2
	arrived, release := make(chan struct{}, calls), make(chan struct{})
	var streams atomic.Int32
	serverHost.SetStreamHandler(server.proto, func(s network.Stream) {
		if streams.Add(1) <= calls {
			arrived <- struct{}{}
			<-release
		}
		server.handleStream(s)
	})
	errs := make(chan error, calls)
	for range calls {
		go func() {
			_, err := client.ExchangeVV(ctx, id, replication.VersionVector{})
			errs <- err
		}()
	}
	for range calls {
		<-arrived
	}
	close(release)
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	idle := func() int {
		client.mu.Lock()
		defer client.mu.Unlock()
		return len(client.idle[serverHost.ID()])
	}
	if n := idle(); n != maxIdle {
		t.Fatalf("%d idle streams, want %d", n, maxIdle)
	}
	client.mu.Lock()
	for _, is := range client.idle[serverHost.ID()] {
		is.since = time.Now().Add(-streamIdle)
	}
	client.mu.Unlock()
	if _, err := client.ExchangeVV(ctx, id, replication.VersionVector{}); err != nil {
		t.Fatal(err)
	}
	if n := streams.Load(); n != calls+1 {
		t.Fatalf("%d streams after the idle ones aged, want %d", n, calls+1)
	}
}
