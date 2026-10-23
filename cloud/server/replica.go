package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/cloud/store"
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/hrana"
)

// replica is a replica node's own part: the primary it follows and a
// follower per database placed at its location, plus one for the state.
type replica struct {
	primary *Peer
	client  cloudv1connect.PeerServiceClient
	proxy   *httputil.ReverseProxy

	mu        sync.Mutex
	followers map[string]*follower // by database ID
	kick      chan struct{}
}

// follower applies one database's records as the primary logs them.
type follower struct {
	h       *store.History
	histDir string
	cancel  context.CancelFunc
	done    chan struct{}

	mu   sync.Mutex
	live chan struct{} // closed once the replica has caught up
}

func newReplica(p *Peer) (*replica, error) {
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
			// serveDB hands Hrana a path below the database; the primary
			// needs the whole one.
			if prefix, ok := pr.In.Context().Value(dbPathKey{}).(string); ok {
				pr.Out.URL.Path = strings.TrimRight(u.Path, "/") + prefix + pr.In.URL.Path
				pr.Out.URL.RawPath = ""
			}
		},
		FlushInterval: -1,
	}
	return &replica{
		primary:   p,
		client:    cloudv1connect.NewPeerServiceClient(http.DefaultClient, p.URL),
		proxy:     proxy,
		followers: map[string]*follower{},
		kick:      make(chan struct{}, 1),
	}, nil
}

type dbPathKey struct{}

// startReplica follows the primary's state until it has caught up, then
// opens it read-only, and starts placing databases.
func (s *Server) startReplica(ctx context.Context) error {
	rp, err := newReplica(s.cfg.primaryPeer())
	if err != nil {
		return err
	}
	s.replica = rp
	h, err := store.OpenReplica(s.statePath(), s.stateHistDir())
	if err != nil {
		return err
	}
	s.stateHist = h
	f := s.startFollower("", h, s.stateHistDir(), s.kickReconcile)
	select {
	case <-f.liveCh():
	case <-ctx.Done():
		s.stopFollowers()
		h.Close()
		return fmt.Errorf("catching up with the primary's state: %w", ctx.Err())
	}
	return nil
}

func (s *Server) statePath() string { return filepath.Join(s.cfg.DataDir, "state.musq") }

func (s *Server) stateHistDir() string { return filepath.Join(s.cfg.DataDir, "history", "_state") }

func (f *follower) liveCh() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

func (f *follower) isLive() bool {
	select {
	case <-f.liveCh():
		return true
	default:
		return false
	}
}

func (s *Server) startFollower(id string, h *store.History, histDir string, applied func()) *follower {
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{h: h, histDir: histDir, cancel: cancel, done: make(chan struct{}), live: make(chan struct{})}
	s.replica.mu.Lock()
	s.replica.followers[id] = f
	s.replica.mu.Unlock()
	go func() {
		defer close(f.done)
		resync := false
		wait := 100 * time.Millisecond
		for ctx.Err() == nil {
			err := s.followOnce(ctx, id, f, applied, &resync)
			if ctx.Err() != nil {
				return
			}
			if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
				s.log.Printf("follow %q: %v", id, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, 2*time.Second)
		}
	}()
	return f
}

// followOnce runs one Follow stream to its end. A record that does not apply
// sets resync: the next stream starts again from the primary's newest base.
func (s *Server) followOnce(ctx context.Context, id string, f *follower, applied func(), resync *bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	after := f.h.Seq()
	if *resync {
		after = 0
	}
	stream, err := s.replica.client.Follow(ctx, peerRequest(s, s.replica.primary, &v1.FollowRequest{DatabaseId: id, AfterSeq: after}))
	if err != nil {
		return err
	}
	defer stream.Close()
	// No message for followTimeout means the primary is gone without a word.
	watchdog := time.AfterFunc(followTimeout, cancel)
	defer watchdog.Stop()
	next := func() (*v1.FollowResponse, error) {
		if !stream.Receive() {
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return nil, io.ErrUnexpectedEOF
		}
		watchdog.Reset(followTimeout)
		return stream.Msg(), nil
	}
	for {
		m, err := next()
		if err != nil {
			return err
		}
		switch x := m.Msg.(type) {
		case *v1.FollowResponse_Live:
			f.mu.Lock()
			select {
			case <-f.live:
			default:
				close(f.live)
			}
			f.mu.Unlock()
		case *v1.FollowResponse_Record:
			rec := x.Record
			var seg, delta io.Reader
			if rec.Base {
				seg = &chunkReader{next: next, n: rec.SegmentSize, delta: false}
				if rec.DeltaSize > 0 {
					delta = &chunkReader{next: next, n: rec.DeltaSize, delta: true}
				}
			}
			if err := f.h.Apply(rec.Data, seg, delta); err != nil {
				var cr *chunkErr
				if !errors.As(err, &cr) && !errors.Is(err, store.ErrClosed) {
					*resync = true
				}
				return err
			}
			*resync = false
			if applied != nil {
				applied()
			}
		default:
			*resync = true
			return errors.New("a base's content arrived without its record")
		}
	}
}

// chunkReader reads a base's file from the chunks that follow its record.
type chunkReader struct {
	next  func() (*v1.FollowResponse, error)
	n     int64
	buf   []byte
	delta bool
}

type chunkErr struct{ err error }

func (e *chunkErr) Error() string { return e.err.Error() }

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.n == 0 && len(r.buf) == 0 {
		return 0, io.EOF
	}
	if len(r.buf) == 0 {
		m, err := r.next()
		if err != nil {
			return 0, &chunkErr{err}
		}
		var b []byte
		switch x := m.Msg.(type) {
		case *v1.FollowResponse_SegmentChunk:
			if r.delta {
				return 0, errors.New("a segment chunk where the delta was due")
			}
			b = x.SegmentChunk
		case *v1.FollowResponse_DeltaChunk:
			if !r.delta {
				return 0, errors.New("a delta chunk where the segment was due")
			}
			b = x.DeltaChunk
		default:
			return 0, errors.New("a base's content was cut short")
		}
		if int64(len(b)) > r.n {
			return 0, errors.New("a base's content is longer than its record says")
		}
		r.buf, r.n = b, r.n-int64(len(b))
	}
	k := copy(p, r.buf)
	r.buf = r.buf[k:]
	return k, nil
}

func (s *Server) kickReconcile() {
	select {
	case s.replica.kick <- struct{}{}:
	default:
	}
}

// runReconcile places databases as the state changes.
func (s *Server) runReconcile() {
	s.reconcile(context.Background())
	for {
		select {
		case <-s.stop:
			return
		case <-s.replica.kick:
			if err := s.provider.ReloadKeys(context.Background()); err != nil {
				s.log.Printf("reload keys: %v", err)
			}
			s.reconcile(context.Background())
		}
	}
}

// reconcile follows every database placed at this node's location and drops
// every other.
func (s *Server) reconcile(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		s.log.Printf("reconcile: %v", err)
		return
	}
	want := map[string]*state.Database{}
	for _, d := range dbs {
		ok, err := s.placed(ctx, d, s.cfg.Node.Location)
		if err != nil {
			s.log.Printf("reconcile %s: %v", d.ID, err)
			continue
		}
		if ok {
			want[d.ID] = d
		}
	}
	s.replica.mu.Lock()
	var gone []string
	for id := range s.replica.followers {
		if _, ok := want[id]; !ok && id != "" {
			gone = append(gone, id)
		}
	}
	var add []*state.Database
	for id, d := range want {
		if _, ok := s.replica.followers[id]; !ok {
			add = append(add, d)
		}
	}
	s.replica.mu.Unlock()
	for _, id := range gone {
		s.unfollow(id)
	}
	for _, d := range add {
		if err := os.MkdirAll(s.dbDir(d), 0o700); err != nil {
			s.log.Printf("place %s: %v", d.ID, err)
			continue
		}
		h, err := store.OpenReplica(s.replicaPath(d), s.histDir(d))
		if err != nil {
			s.log.Printf("place %s: %v", d.ID, err)
			continue
		}
		s.startFollower(d.ID, h, s.histDir(d), nil)
	}
}

// unfollow stops following a database and deletes its copy.
func (s *Server) unfollow(id string) {
	s.replica.mu.Lock()
	f, ok := s.replica.followers[id]
	delete(s.replica.followers, id)
	s.replica.mu.Unlock()
	if !ok {
		return
	}
	f.cancel()
	<-f.done
	s.pool.drop(id)
	f.h.Close()
	removeDBFiles(s.log, f.h.Path())
	if err := os.RemoveAll(f.histDir); err != nil {
		s.log.Printf("remove history of %s: %v", id, err)
	}
}

func (s *Server) stopFollowers() {
	s.replica.mu.Lock()
	all := s.replica.followers
	s.replica.followers = map[string]*follower{}
	s.replica.mu.Unlock()
	for _, f := range all {
		f.cancel()
	}
	for id, f := range all {
		<-f.done
		if id != "" {
			f.h.Close()
		}
	}
}

// replicaPath is a database's copy on a replica: the history it follows
// spans in-place restores, so the name never changes.
func (s *Server) replicaPath(d *state.Database) string {
	return filepath.Join(s.dbDir(d), d.ID+".musq")
}

// replicaFollower is d's follower when it has caught up once.
func (s *Server) replicaFollower(d *state.Database) *follower {
	s.replica.mu.Lock()
	f := s.replica.followers[d.ID]
	s.replica.mu.Unlock()
	if f == nil || f.h.Seq() == 0 {
		return nil
	}
	return f
}

// toPrimary hands a request to the primary as it came.
func (s *Server) toPrimary(w http.ResponseWriter, r *http.Request) {
	s.replica.proxy.ServeHTTP(w, r)
}

// openReplicaEntry serves a database's copy: reads here, the rest at the
// primary.
func (s *Server) openReplicaEntry(d *state.Database) (*entry, error) {
	path := s.replicaPath(d)
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	ro, err := openPinged(path + "?" + dsnMode + "=ro")
	if err != nil {
		return nil, err
	}
	return &entry{db: ro, srv: hrana.NewServer(ro,
		hrana.WithReadOnlyDB(ro),
		hrana.WithAuth(s.hranaAuth(d.OrgID, d.ID)),
		hrana.WithPrimary(http.HandlerFunc(s.toPrimary), readOnlySQL))}, nil
}

// readOnlySQL accepts SQL a replica may run: only SELECT and VALUES
// statements. Anything else, a WITH or a PRAGMA included, goes to the primary.
func readOnlySQL(sql string) bool {
	stmts, err := engine.SplitStatements(sql)
	if err != nil || len(stmts) == 0 {
		return false
	}
	for _, st := range stmts {
		verb, ok := engine.LeadingStatementVerb(st)
		if !ok {
			return false
		}
		switch strings.ToUpper(verb) {
		case "SELECT", "VALUES":
		default:
			return false
		}
	}
	return true
}

// replicaRoutes are what a replica serves itself; the rest goes to the
// primary: the API, sign-in and the OIDC endpoints.
func (s *Server) replicaRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(cloudv1connect.NewPeerServiceHandler(peerService{s}))
	mux.Handle("/db/", http.HandlerFunc(s.serveDB))
	mux.Handle("/console/", s.console())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	mux.Handle("/", http.HandlerFunc(s.toPrimary))
	return s.cors(mux)
}
