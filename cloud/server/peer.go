package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/cloud/store"
)

const (
	// nodeHeader names the calling node on a peer call; its secret is the
	// bearer token.
	nodeHeader = "Musql-Node"
	// followIdle is how often an idle Follow stream says it is still live, and
	// followTimeout how long a replica waits for any message before it calls
	// again.
	followIdle    = 10 * time.Second
	followTimeout = 3 * followIdle
	chunkSize     = 1 << 20
)

type peerService struct{ *Server }

// peerAuth checks that a call comes from a configured peer.
func (s *Server) peerAuth(h http.Header) error {
	name := h.Get(nodeHeader)
	for _, p := range s.cfg.Peers {
		if p.Name == name && subtle.ConstantTimeCompare([]byte(bearer(h)), []byte(p.Secret)) == 1 {
			return nil
		}
	}
	return connect.NewError(connect.CodeUnauthenticated, errors.New("not a peer"))
}

// peerRequest is a call to peer p, carrying this node's name and their secret.
func peerRequest[T any](s *Server, p *Peer, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set(nodeHeader, s.cfg.Node.Name)
	req.Header().Set("Authorization", "Bearer "+p.Secret)
	return req
}

// Follow streams a database's history to a replica: what it lacks, then each
// record as it is logged.
func (s peerService) Follow(ctx context.Context, req *connect.Request[v1.FollowRequest], stream *connect.ServerStream[v1.FollowResponse]) error {
	if err := s.peerAuth(req.Header()); err != nil {
		return err
	}
	if s.replica != nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this node is a replica"))
	}
	h := s.stateHist
	if id := req.Msg.DatabaseId; id != "" {
		d, err := s.store.DatabaseByID(ctx, id)
		if err != nil {
			return rpcErr(err)
		}
		if h, err = s.history(d); err != nil {
			return connect.NewError(connect.CodeUnavailable, err)
		}
	}
	after := req.Msg.AfterSeq
	live := false
	idle := time.NewTicker(followIdle)
	defer idle.Stop()
	for {
		out, wait, err := h.Ship(after)
		if errors.Is(err, store.ErrClosed) {
			return connect.NewError(connect.CodeUnavailable, err)
		}
		if err != nil {
			s.log.Printf("follow %s: %v", req.Msg.DatabaseId, err)
			return connect.NewError(connect.CodeInternal, errors.New("internal error"))
		}
		if wait != nil {
			if !live {
				if err := stream.Send(&v1.FollowResponse{Msg: &v1.FollowResponse_Live{Live: true}}); err != nil {
					return err
				}
				live = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-s.stop:
				return connect.NewError(connect.CodeUnavailable, errors.New("the node is stopping"))
			case <-idle.C:
				live = false
			case <-wait:
			}
			continue
		}
		err = func() error {
			defer closeShipments(out)
			for i := range out {
				if err := sendShipment(stream, &out[i]); err != nil {
					return err
				}
				after = out[i].Seq
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
}

func closeShipments(out []store.Shipment) {
	for i := range out {
		out[i].Close()
	}
}

func sendShipment(stream *connect.ServerStream[v1.FollowResponse], sh *store.Shipment) error {
	rec := &v1.Record{Seq: sh.Seq, Data: sh.Record, Base: sh.Base, SegmentSize: sh.SegmentSize, DeltaSize: sh.DeltaSize}
	if err := stream.Send(&v1.FollowResponse{Msg: &v1.FollowResponse_Record{Record: rec}}); err != nil {
		return err
	}
	if !sh.Base {
		return nil
	}
	send := func(r io.Reader, n int64, wrap func([]byte) *v1.FollowResponse) error {
		buf := make([]byte, chunkSize)
		for n > 0 {
			k := min(n, chunkSize)
			if _, err := io.ReadFull(r, buf[:k]); err != nil {
				return err
			}
			if err := stream.Send(wrap(buf[:k])); err != nil {
				return err
			}
			n -= k
		}
		return nil
	}
	if err := send(sh.Segment, sh.SegmentSize, func(b []byte) *v1.FollowResponse {
		return &v1.FollowResponse{Msg: &v1.FollowResponse_SegmentChunk{SegmentChunk: b}}
	}); err != nil {
		return err
	}
	if sh.DeltaSize == 0 {
		return nil
	}
	return send(sh.Delta, sh.DeltaSize, func(b []byte) *v1.FollowResponse {
		return &v1.FollowResponse{Msg: &v1.FollowResponse_DeltaChunk{DeltaChunk: b}}
	})
}

// placed reports whether d is kept at location loc.
func (s *Server) placed(ctx context.Context, d *state.Database, loc string) (bool, error) {
	g, err := s.store.GroupByID(ctx, d.GroupID)
	if err != nil {
		return false, err
	}
	for _, l := range g.Locations {
		if l == loc {
			return true, nil
		}
	}
	return false, nil
}
