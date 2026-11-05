package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/cluster"
	"github.com/samyfodil/musql/cloud/state"
)

// errNoMaster is a regular database whose group has no master this node can
// reach: being elected, or just elected and catching up.
var errNoMaster = errors.New("this database's group is electing its master; retry shortly")

type forwardedKey struct{}

// serveForwarded serves a request another node forwarded here, as its
// database's master; it is never forwarded again.
func (s *Server) serveForwarded(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), forwardedKey{}, true)))
}

// masterURL is where a request that writes regular database d goes, when
// this node is not the master of d's group: "" serves it here. While the
// group elects one, or the one elected settles, it waits, up to masterWait.
func (s *Server) masterURL(ctx context.Context, d *state.Database) (string, error) {
	e := s.groupMaster(d)
	if e == nil {
		return "", nil
	}
	deadline := time.Now().Add(masterWait)
	for {
		if e.Writer() {
			return "", nil
		}
		id := e.Leader()
		if id != "" && id != s.cl.node.ID() && ctx.Value(forwardedKey{}) == nil {
			return cluster.PeerURL(id) + "/fwd", nil
		}
		if time.Now().After(deadline) {
			return "", connect.NewError(connect.CodeUnavailable, errNoMaster)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// masterWait is how long a request waits for its group's master: longer than
// an election and the settling after it take.
const masterWait = 10 * time.Second

// forward makes req's call on the master with the caller's credential.
func forward[Req, Res any](ctx context.Context, req *connect.Request[Req], call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error)) (*connect.Response[Res], error) {
	out := connect.NewRequest(req.Msg)
	out.Header().Set("Authorization", req.Header().Get("Authorization"))
	return call(ctx, out)
}

// proxy passes a Hrana request, WebSocket included, to the master at base.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, base string) {
	target, err := url.Parse(base)
	if err != nil {
		hranaError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := &httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		Transport: s.cl.client.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Printf("forward to %s: %v", base, err)
			hranaError(w, http.StatusServiceUnavailable, errNoMaster.Error())
		},
	}
	p.ServeHTTP(w, r)
}
