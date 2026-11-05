// Package cluster connects a musql cloud node to its peers over libp2p: the
// node's key, a host that admits only the configured peer IDs, one gossipsub
// router, and a replication option per replicated file (examples/libp2p's
// transport).
package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	p2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	repllibp2p "github.com/samyfodil/musql/examples/libp2p"
	"github.com/samyfodil/musql/replication"
)

// LoadKey reads the node's libp2p key from path, making and saving a new
// Ed25519 one when the file does not exist.
func LoadKey(path string) (crypto.PrivKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(b)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, err
	}
	if b, err = crypto.MarshalPrivateKey(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// PeerID is the ID other nodes know key's node by.
func PeerID(key crypto.PrivKey) (string, error) {
	id, err := peer.IDFromPrivateKey(key)
	return id.String(), err
}

// Peer is another node: its ID and where to dial it.
type Peer struct {
	ID    string
	Addrs []string
}

// Node is this node's libp2p side.
type Node struct {
	host  host.Host
	ps    *pubsub.PubSub
	peers []peer.AddrInfo
	logf  func(string, ...any)
	stop  context.CancelFunc
	wg    sync.WaitGroup
	// closers stop what serves on the host, before it closes.
	closers []func() error
}

// gater admits connections to and from the configured peers only. The
// security handshake proves the remote's peer ID, so InterceptSecured is the
// check that counts; the dial-side checks just avoid dialling strangers.
type gater map[peer.ID]bool

func (g gater) InterceptPeerDial(p peer.ID) bool                 { return g[p] }
func (g gater) InterceptAddrDial(p peer.ID, _ ma.Multiaddr) bool { return g[p] }
func (g gater) InterceptAccept(network.ConnMultiaddrs) bool      { return true }
func (g gater) InterceptSecured(_ network.Direction, p peer.ID, _ network.ConnMultiaddrs) bool {
	return g[p]
}
func (g gater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) { return true, 0 }

// Start starts the host on listen with key, dials peers and keeps redialling
// any it is not connected to. logf reports dial failures.
func Start(key crypto.PrivKey, listen []string, peers []Peer, logf func(string, ...any)) (*Node, error) {
	g := gater{}
	var infos []peer.AddrInfo
	for _, p := range peers {
		id, err := peer.Decode(p.ID)
		if err != nil {
			return nil, fmt.Errorf("cluster: peer_id %q: %w", p.ID, err)
		}
		info := peer.AddrInfo{ID: id}
		for _, a := range p.Addrs {
			m, err := ma.NewMultiaddr(a)
			if err != nil {
				return nil, fmt.Errorf("cluster: peer %s: address %q: %w", p.ID, a, err)
			}
			info.Addrs = append(info.Addrs, m)
		}
		g[id] = true
		infos = append(infos, info)
	}
	h, err := p2p.New(p2p.Identity(key), p2p.ListenAddrStrings(listen...), p2p.ConnectionGater(g))
	if err != nil {
		return nil, fmt.Errorf("cluster: %w", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		stop()
		h.Close()
		return nil, fmt.Errorf("cluster: %w", err)
	}
	n := &Node{host: h, ps: ps, peers: infos, logf: logf, stop: stop}
	n.wg.Go(func() { n.dial(ctx) })
	return n, nil
}

// dial connects to every peer that is not connected, now and every 2s.
func (n *Node) dial(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	failed := map[peer.ID]bool{}
	for {
		for _, p := range n.peers {
			if n.host.Network().Connectedness(p.ID) == network.Connected {
				continue
			}
			n.host.Peerstore().AddAddrs(p.ID, p.Addrs, time.Hour)
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := n.host.Connect(dctx, p)
			cancel()
			if err != nil && !failed[p.ID] && ctx.Err() == nil {
				n.logf("cluster: peer %s: %v", p.ID, err)
			}
			failed[p.ID] = err != nil
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ID is this node's peer ID.
func (n *Node) ID() string { return n.host.ID().String() }

// Addrs are the addresses this node listens on, with its peer ID.
func (n *Node) Addrs() []string {
	var out []string
	for _, a := range n.host.Addrs() {
		out = append(out, a.String())
	}
	return out
}

// Sync is the replication.Open option that replicates a file on topic with
// the peers.
func (n *Node) Sync(topic string) replication.Option {
	return repllibp2p.SyncWith(n.host, n.ps, topic)
}

// Close stops dialling and serving, and closes the host. Close every
// replicated file and election first.
func (n *Node) Close() error {
	n.stop()
	for _, c := range n.closers {
		c()
	}
	n.wg.Wait()
	return n.host.Close()
}
