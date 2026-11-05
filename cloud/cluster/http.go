package cluster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/peer"
)

// httpProtocol carries HTTP between the nodes: a follower's writes to the
// state's master, and requests it forwards to a database's master. Only the
// configured peers can open it (the gater), so it needs no credential of its
// own.
const httpProtocol = "/musql-cloud/http/1"

// peerHost is the host part of a URL that names a peer: "<peer ID>.peer".
const peerHost = ".peer"

// ServeHTTP serves h to the other nodes until the node closes.
func (n *Node) ServeHTTP(h http.Handler) error {
	l, err := gostream.Listen(n.host, httpProtocol)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	n.wg.Go(func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			n.logf("cluster: http: %v", err)
		}
	})
	n.closers = append(n.closers, srv.Close)
	return nil
}

// PeerURL is the base URL of the node with peer ID id, for the client
// HTTPClient returns.
func PeerURL(id string) string { return "http://" + id + peerHost }

// HTTPClient reaches the other nodes' ServeHTTP at PeerURL.
func (n *Node) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			id, err := peer.Decode(strings.TrimSuffix(host, peerHost))
			if err != nil {
				return nil, err
			}
			return gostream.Dial(ctx, n.host, id, httpProtocol)
		},
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     time.Minute,
	}}
}
