package server

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/ai"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/oidc"
	"github.com/samyfodil/musql/cloud/state"
)

// Server is a running musql cloud node.
type Server struct {
	cfg      *Config
	store    *state.Store
	provider *oidc.Provider
	pool     *pool
	meter    *meter
	hist     histories
	uploads  uploadGate
	handler  http.Handler
	log      *log.Logger
	// cl is set on a cluster node.
	cl *clusterSide
	// initOnly opens the state for Init: written on this node alone, with no
	// election.
	initOnly bool
	genMu    sync.Mutex
	gen      ai.Generator

	stop      chan struct{}
	bg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	// noAdmin is set while a cluster node waits for the admin user to
	// replicate to it.
	noAdmin       atomic.Bool
	noAdminLogged time.Time
}

var errNoAdminYet = errors.New("no admin yet: waiting for the state from a peer, or run musql-cloud init")

// New opens the state in cfg.DataDir, makes sure the configured admin can sign
// in, and builds the handler. Histories are logged by the engine's commit hook,
// which the process must install (engine.WithCommitHook(store.OnCommit));
// without it a history catches up only when synced.
func New(ctx context.Context, cfg *Config, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "musql-cloud: ", log.LstdFlags)
	}
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "databases"), filepath.Join(cfg.DataDir, "uploads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Server{cfg: cfg, log: logger}
	s.removeStaleExports()
	store, err := s.openState(ctx)
	if err != nil {
		return nil, err
	}
	s.store = store
	fail := func(err error) (*Server, error) {
		store.Close()
		if s.cl != nil {
			s.leaveElections()
			s.cl.node.Close()
		}
		return nil, err
	}
	if err := s.requireAdmin(ctx); err != nil {
		if !cfg.Clustered() {
			return fail(err)
		}
		// A new node's state arrives from its peers after it starts.
		s.log.Print(errNoAdminYet)
		s.noAdmin.Store(true)
		s.noAdminLogged = time.Now()
	}
	if err := store.EnsureGroupLocation(ctx, cfg.Node.Location); err != nil {
		return fail(err)
	}
	opts := []oidc.Option{oidc.WithConsoleOrigins(cfg.Console.CORSOrigins), oidc.WithKeyRotation(cfg.OIDC.keyRotation),
		oidc.WithLoginLimits(cfg.OIDC.LoginPerIP, cfg.OIDC.LoginPerEmail, cfg.OIDC.loginWindow)}
	if cfg.Clustered() {
		opts = append(opts, oidc.WithNode(cfg.Node.Name))
	}
	if s.provider, err = oidc.New(ctx, store, cfg.PublicURL, opts...); err != nil {
		return fail(err)
	}
	if s.cl != nil {
		s.cl.starting.Store(false)
	}
	s.meter = newMeter(store, func(id string) int64 {
		d, err := store.DatabaseByID(context.Background(), id)
		if err != nil {
			return 0
		}
		return fileSize(s.dbPath(d))
	})
	s.pool = newPool(s.openEntry)
	s.handler = s.routes()
	s.stop = make(chan struct{})
	s.bg.Go(s.rotateKeys)
	if cfg.snapshotEvery > 0 {
		s.bg.Go(func() { s.runSnapshots(s.stop) })
	}
	s.bg.Go(s.runUsageFlush)
	s.bg.Go(s.runUploadSweep)
	if s.cl != nil {
		s.bg.Go(s.runCluster)
	}
	return s, nil
}

// rotateKeys starts new signing keys on schedule and drops expired ones.
func (s *Server) rotateKeys() {
	s.every(time.Hour, "rotate signing keys", func(ctx context.Context) error {
		return s.provider.RotateIfDue(ctx, time.Now())
	})
}

// runUsageFlush writes counted usage every usageFlush until stop is closed.
func (s *Server) runUsageFlush() {
	s.every(s.cfg.usageFlush, "usage", s.meter.flush)
}

// every runs fn each d until stop is closed.
func (s *Server) every(d time.Duration, what string, fn func(context.Context) error) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := fn(context.Background()); err != nil {
				s.log.Printf("%s: %v", what, err)
			}
		}
	}
}

// Handler serves everything.
func (s *Server) Handler() http.Handler { return s.handler }

// Provider is the OIDC provider.
func (s *Server) Provider() *oidc.Provider { return s.provider }

// Store is the state database.
func (s *Server) Store() *state.Store { return s.store }

// Close closes every open database and the state; once, a second call
// answering as the first did.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *Server) close() error {
	close(s.stop)
	s.bg.Wait()
	// Its counts go to the state while it can still reach the master.
	if err := s.meter.flush(context.Background()); err != nil {
		s.log.Printf("usage: %v", err)
	}
	if s.cl != nil {
		// The others stop hearing from it, and elect, before what it served
		// closes under them.
		s.cl.node.StopServing()
		s.leaveElections()
	}
	s.pool.closeAll()
	if s.cl != nil {
		s.closeCluster()
	}
	s.closeHistories()
	err := s.store.Close()
	if s.cl != nil {
		if cerr := s.cl.node.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

func (s *Server) requireAdmin(ctx context.Context) error {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(users, func(u *state.User) bool { return u.Admin }) {
		return errors.New("no admin: run musql-cloud init --admin-email <email>")
	}
	return nil
}

// Init creates the admin user in the state under cfg.DataDir, or, when the
// email exists, resets its password and makes it an enabled admin. In a
// cluster the row is written locally and replicates when the node runs.
func Init(ctx context.Context, cfg *Config, email, password string) error {
	email = state.NormalizeEmail(email)
	if email == "" {
		return errors.New("admin email is required")
	}
	if password == "" {
		return errors.New("admin password is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	s := &Server{cfg: cfg, log: log.New(os.Stderr, "musql-cloud: ", log.LstdFlags), initOnly: true}
	store, err := s.openState(ctx)
	if err != nil {
		return err
	}
	s.store = store
	defer func() {
		store.Close()
		if s.cl != nil {
			s.cl.node.Close()
		}
	}()
	hash := state.HashPassword(password)
	u, err := store.UserByEmail(ctx, email)
	if errors.Is(err, state.ErrNotFound) {
		_, err = store.CreateUser(ctx, email, "Admin", hash, true)
		return err
	}
	if err != nil {
		return err
	}
	if err := store.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	if !u.Admin || u.Disabled {
		u.Admin, u.Disabled = true, false
		return store.UpdateUser(ctx, u)
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithReadMaxBytes(maxRequestBytes)}
	mux.Handle(cloudv1connect.NewAuthServiceHandler(authService{s}, opts...))
	mux.Handle(cloudv1connect.NewUserServiceHandler(userService{s}, opts...))
	mux.Handle(cloudv1connect.NewOrgServiceHandler(orgService{s}, opts...))
	mux.Handle(cloudv1connect.NewGroupServiceHandler(groupService{s}, opts...))
	mux.Handle(cloudv1connect.NewDatabaseServiceHandler(databaseService{Server: s}, opts...))
	mux.Handle(cloudv1connect.NewAIServiceHandler(aiService{Server: s}, opts...))
	mux.Handle("/db/", http.HandlerFunc(s.serveDB))
	op := s.provider.Handler()
	mux.Handle("/.well-known/openid-configuration", op)
	mux.Handle("/oidc/", op)
	mux.Handle("/login", op)
	mux.Handle("/console/", s.console())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	return s.cors(mux)
}

// oidcCORSPaths are the OIDC endpoints a console on another origin calls.
var oidcCORSPaths = []string{"/oidc/token", "/oidc/userinfo", "/oidc/logout", "/oidc/revoke"}

// cors lets the configured origins call the API, Hrana and the OIDC token
// endpoints from a browser. Credentials travel in the Authorization header,
// never in cookies, so no cookie-bearing request crosses origins.
func (s *Server) cors(next http.Handler) http.Handler {
	origins := s.cfg.Console.CORSOrigins
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		api := strings.HasPrefix(r.URL.Path, "/cloud.v1.") || strings.HasPrefix(r.URL.Path, "/db/") || slices.Contains(oidcCORSPaths, r.URL.Path)
		if origin != "" && api && (slices.Contains(origins, "*") || slices.Contains(origins, origin)) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Connect-Content-Encoding")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Connect-Accept-Encoding, Connect-Content-Encoding, X-User-Agent, X-Grpc-Web, Grpc-Timeout")
				h.Set("Access-Control-Max-Age", "7200")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// console serves the console's build, with index.html for any path that is
// not a file (the console routes on the client).
func (s *Server) console() http.Handler {
	if s.cfg.Console.Dir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the console is not configured (console.dir)", http.StatusNotFound)
		})
	}
	root := os.DirFS(s.cfg.Console.Dir)
	files := http.FileServerFS(root)
	return http.StripPrefix("/console", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" {
			if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, root, "index.html")
	}))
}
